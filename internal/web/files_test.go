package web

import (
	"bytes"
	"context"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ihainan/corral/internal/sftplite"
	"github.com/ihainan/corral/internal/sshx"
	"github.com/ihainan/corral/internal/testutil"
	"golang.org/x/crypto/ssh"
)

// fileEnv is a signed-in admin with a session on a host whose SFTP home is root.
type fileEnv struct {
	h     *harness
	c     *client
	sess  string
	root  string
	other string // a folder outside the home, for links that point away
}

func newFileEnv(t *testing.T) *fileEnv {
	h := newHarness(t)
	admin := h.admin()
	root, other := t.TempDir(), t.TempDir()
	srv := h.sshServer(root)
	// Folders are deleted by the host's rm.
	srv.HandleExec(testutil.ShellExec(root, func(cmd string) bool { return strings.Contains(cmd, "rm -rf") }))
	r := admin.do("POST", "/api/sessions", map[string]any{"host_id": h.hostFor(admin, srv.Addr.Port)})
	mustStatus(t, r, 201, "open session")
	return &fileEnv{h: h, c: admin, sess: r.json()["id"].(string), root: root, other: other}
}

func (e *fileEnv) url(op string, q url.Values) string {
	u := "/api/sessions/" + e.sess + "/files"
	if op != "" {
		u += "/" + op
	}
	if q != nil {
		u += "?" + q.Encode()
	}
	return u
}

func (e *fileEnv) raw(method, u string, body io.Reader, ctype string) (*http.Response, []byte) {
	e.h.t.Helper()
	req, _ := http.NewRequest(method, e.h.ts.URL+u, body)
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	req.Header.Set("Origin", e.h.cfg.Origin())
	req.AddCookie(&http.Cookie{Name: e.h.srv.cookieName(), Value: e.c.cookie})
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		e.h.t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res, b
}

func (e *fileEnv) upload(dir, name string, data []byte, overwrite bool) resp {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile("file", "ignored.bin")
	fw.Write(data)
	mw.Close()
	q := url.Values{"dir": {dir}, "name": {name}}
	if overwrite {
		q.Set("overwrite", "1")
	}
	res, b := e.raw("POST", e.url("upload", q), &buf, mw.FormDataContentType())
	return resp{Status: res.StatusCode, Body: b}
}

func entryNames(r resp) map[string]map[string]any {
	out := map[string]map[string]any{}
	list, _ := r.json()["entries"].([]any)
	for _, x := range list {
		m := x.(map[string]any)
		out[m["name"].(string)] = m
	}
	return out
}

func TestFilesListShowsTypesAndLinks(t *testing.T) {
	e := newFileEnv(t)
	os.WriteFile(filepath.Join(e.root, "a.txt"), []byte("hello"), 0o644)
	os.WriteFile(filepath.Join(e.root, ".hidden"), nil, 0o600)
	os.Mkdir(filepath.Join(e.root, "src"), 0o755)
	os.Symlink(e.other, filepath.Join(e.root, "away"))
	os.Symlink(filepath.Join(e.root, "missing"), filepath.Join(e.root, "broken"))

	r := e.c.do("GET", e.url("", nil), nil)
	mustStatus(t, r, 200, "list home")
	if got := r.json()["path"]; got != filepath.Clean(e.root) {
		t.Fatalf("home path %v, want %s", got, e.root)
	}
	n := entryNames(r)
	for name, want := range map[string][2]any{"a.txt": {"file", false}, ".hidden": {"file", false}, "src": {"dir", false}, "away": {"dir", true}, "broken": {"other", true}} {
		m := n[name]
		if m == nil || m["type"] != want[0] || m["link"] != want[1] {
			t.Errorf("%s: %v, want type %v link %v", name, m, want[0], want[1])
		}
	}
	if n["a.txt"]["size"].(float64) != 5 {
		t.Errorf("size %v", n["a.txt"]["size"])
	}

	r = e.c.do("GET", e.url("", url.Values{"path": {e.root + "/src/../src"}}), nil)
	mustStatus(t, r, 200, "list sub folder")
	if r.json()["path"] != filepath.Join(e.root, "src") {
		t.Fatalf("path not cleaned: %v", r.json()["path"])
	}
	mustStatus(t, e.c.do("GET", e.url("", url.Values{"path": {"relative/dir"}}), nil), 400, "relative path")
	if r := e.c.do("GET", e.url("", url.Values{"path": {e.root + "/a.txt"}}), nil); r.Status != 400 || r.code() != "not_a_folder" {
		t.Fatalf("listing a file: %d %s", r.Status, r.Body)
	}
	if r := e.c.do("GET", e.url("", url.Values{"path": {e.root + "/nope"}}), nil); r.Status != 404 {
		t.Fatalf("missing folder: %d %s", r.Status, r.Body)
	}
}

func TestFilesSharedChannelIsReusedAndClosedWithSession(t *testing.T) {
	e := newFileEnv(t)
	for i := 0; i < 3; i++ {
		mustStatus(t, e.c.do("GET", e.url("", nil), nil), 200, "list")
	}
	e.h.srv.files.mu.Lock()
	c := e.h.srv.files.m[e.sess]
	e.h.srv.files.mu.Unlock()
	if c == nil || c.dead || c.users != 0 {
		t.Fatalf("shared channel after listings: %+v", c)
	}
	mustStatus(t, e.c.do("DELETE", "/api/sessions/"+e.sess, nil), 200, "close session")
	<-c.gone
	e.h.srv.files.mu.Lock()
	_, still := e.h.srv.files.m[e.sess]
	e.h.srv.files.mu.Unlock()
	if still {
		t.Fatal("shared channel kept after the session ended")
	}
	if r := e.c.do("GET", e.url("", nil), nil); r.Status != 404 && r.Status != 409 {
		t.Fatalf("listing an ended session: %d", r.Status)
	}
}

func TestFilesMkdirRenameDelete(t *testing.T) {
	e := newFileEnv(t)
	mustStatus(t, e.c.do("POST", e.url("mkdir", nil), map[string]any{"dir": e.root, "name": "new 文件夹"}), 201, "mkdir")
	if fi, err := os.Stat(filepath.Join(e.root, "new 文件夹")); err != nil || !fi.IsDir() {
		t.Fatalf("folder not created: %v", err)
	}
	if r := e.c.do("POST", e.url("mkdir", nil), map[string]any{"dir": e.root, "name": "new 文件夹"}); r.Status != 409 {
		t.Fatalf("mkdir existing: %d", r.Status)
	}
	for _, bad := range []string{"", ".", "..", "a/b", "x\x00y", strings.Repeat("n", 256)} {
		if r := e.c.do("POST", e.url("mkdir", nil), map[string]any{"dir": e.root, "name": bad}); r.Status != 400 {
			t.Errorf("mkdir %q: %d", bad, r.Status)
		}
	}

	os.WriteFile(filepath.Join(e.root, "a.txt"), []byte("A"), 0o644)
	os.WriteFile(filepath.Join(e.root, "b.txt"), []byte("B"), 0o644)
	if r := e.c.do("POST", e.url("rename", nil), map[string]any{"path": e.root + "/a.txt", "name": "b.txt"}); r.Status != 409 {
		t.Fatalf("rename onto existing: %d", r.Status)
	}
	if b, _ := os.ReadFile(filepath.Join(e.root, "b.txt")); string(b) != "B" {
		t.Fatal("rename replaced an existing file")
	}
	mustStatus(t, e.c.do("POST", e.url("rename", nil), map[string]any{"path": e.root + "/a.txt", "name": "c.txt"}), 200, "rename")
	if b, _ := os.ReadFile(filepath.Join(e.root, "c.txt")); string(b) != "A" {
		t.Fatal("renamed file missing")
	}
	mustStatus(t, e.c.do("POST", e.url("rename", nil), map[string]any{"path": "/", "name": "x"}), 400, "rename root")

	// A folder with a nested folder, and a link that points outside: the
	// link goes, what it points to stays.
	tree := filepath.Join(e.root, "tree")
	os.MkdirAll(filepath.Join(tree, "deep", "er"), 0o755)
	os.WriteFile(filepath.Join(tree, "deep", "er", "f"), []byte("x"), 0o644)
	os.WriteFile(filepath.Join(e.other, "keep.txt"), []byte("keep"), 0o644)
	os.Symlink(e.other, filepath.Join(tree, "link"))
	mustStatus(t, e.c.do("POST", e.url("delete", nil), map[string]any{"path": tree}), 200, "delete tree")
	if _, err := os.Stat(tree); !os.IsNotExist(err) {
		t.Fatal("tree still there")
	}
	if b, _ := os.ReadFile(filepath.Join(e.other, "keep.txt")); string(b) != "keep" {
		t.Fatal("delete followed a link")
	}
	mustStatus(t, e.c.do("POST", e.url("delete", nil), map[string]any{"path": e.root + "/c.txt"}), 200, "delete file")
	mustStatus(t, e.c.do("POST", e.url("delete", nil), map[string]any{"path": "/"}), 400, "delete root")
	mustStatus(t, e.c.do("POST", e.url("delete", nil), map[string]any{"path": e.root}), 400, "delete home")
	mustStatus(t, e.c.do("POST", e.url("delete", nil), map[string]any{"path": filepath.Dir(e.root)}), 400, "delete a folder holding home")
	// The home folder reached through a link is still the home folder.
	os.Symlink(filepath.Dir(e.root), filepath.Join(e.other, "alias"))
	mustStatus(t, e.c.do("POST", e.url("delete", nil), map[string]any{"path": filepath.Join(e.other, "alias", filepath.Base(e.root))}), 400, "delete home via a link")
	// A link to a folder is deleted as a link.
	os.Symlink(e.other, filepath.Join(e.root, "to-other"))
	mustStatus(t, e.c.do("POST", e.url("delete", nil), map[string]any{"path": e.root + "/to-other"}), 200, "delete link")
	if b, _ := os.ReadFile(filepath.Join(e.other, "keep.txt")); string(b) != "keep" {
		t.Fatal("deleting a link deleted its target")
	}
	if _, err := os.Stat(filepath.Join(e.root, "b.txt")); err != nil {
		t.Fatal("home content gone")
	}

	kinds := map[string]bool{}
	for _, a := range auditActions(t, e.h) {
		kinds[a] = true
	}
	for _, k := range []string{"file.mkdir", "file.rename", "file.delete"} {
		if !kinds[k] {
			t.Errorf("no %s audit entry", k)
		}
	}
}

func auditActions(t *testing.T, h *harness) []string {
	t.Helper()
	list, err := h.store.ListAudit(t.Context(), storeAuditQuery())
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, a := range list {
		out = append(out, a.Action)
	}
	return out
}

func TestFilesUploadAndDownload(t *testing.T) {
	e := newFileEnv(t)
	mustStatus(t, e.upload(e.root, "报告 1.txt", []byte("first"), false), 201, "upload")
	target := filepath.Join(e.root, "报告 1.txt")
	if b, _ := os.ReadFile(target); string(b) != "first" {
		t.Fatalf("content %q", b)
	}
	os.Chmod(target, 0o640)
	if r := e.upload(e.root, "报告 1.txt", []byte("second"), false); r.Status != 409 || r.code() != "file_exists" {
		t.Fatalf("upload onto existing: %d %s", r.Status, r.Body)
	}
	mustStatus(t, e.upload(e.root, "报告 1.txt", []byte("second"), true), 201, "replace")
	if b, _ := os.ReadFile(target); string(b) != "second" {
		t.Fatalf("content after replace %q", b)
	}
	if fi, _ := os.Stat(target); fi.Mode().Perm() != 0o640 {
		t.Errorf("mode after replace %v, want 0640", fi.Mode().Perm())
	}
	os.Mkdir(filepath.Join(e.root, "dir"), 0o755)
	if r := e.upload(e.root, "dir", []byte("x"), true); r.Status != 409 {
		t.Fatalf("upload onto a folder: %d", r.Status)
	}
	// A link is not replaced (nor written through).
	os.WriteFile(filepath.Join(e.other, "private"), []byte("p"), 0o600)
	os.Symlink(filepath.Join(e.other, "private"), filepath.Join(e.root, "link"))
	if r := e.upload(e.root, "link", []byte("x"), true); r.Status != 409 || r.code() != "not_a_file" {
		t.Fatalf("replace a link: %d %s", r.Status, r.Body)
	}
	if b, _ := os.ReadFile(filepath.Join(e.other, "private")); string(b) != "p" {
		t.Fatal("upload wrote through a link")
	}
	if r := e.upload(e.root, "../escape", []byte("x"), false); r.Status != 400 {
		t.Fatalf("name with a slash: %d", r.Status)
	}
	if r := e.upload(e.root, "big", make([]byte, e.h.cfg.UploadMaxBytes+1), false); r.Status != http.StatusRequestEntityTooLarge {
		t.Fatalf("too large: %d", r.Status)
	}
	matches, _ := filepath.Glob(filepath.Join(e.root, ".corral-upload-*"))
	if len(matches) != 0 {
		t.Fatalf("temporary files left: %v", matches)
	}

	res, body := e.raw("GET", e.url("download", url.Values{"path": {target}}), nil, "")
	if res.StatusCode != 200 || string(body) != "second" {
		t.Fatalf("download: %d %q", res.StatusCode, body)
	}
	if cd := res.Header.Get("Content-Disposition"); !strings.HasPrefix(cd, "attachment") || !strings.Contains(cd, "utf-8''") {
		t.Errorf("Content-Disposition %q", cd)
	}
	if ct := res.Header.Get("Content-Type"); ct != "application/octet-stream" {
		t.Errorf("Content-Type %q", ct)
	}
	if res, _ := e.raw("GET", e.url("download", url.Values{"path": {e.root + "/dir"}}), nil, ""); res.StatusCode != 400 {
		t.Errorf("download a folder: %d", res.StatusCode)
	}
	if res, _ := e.raw("GET", e.url("download", url.Values{"path": {e.root + "/none"}}), nil, ""); res.StatusCode != 404 {
		t.Errorf("download missing: %d", res.StatusCode)
	}
}

func TestFilesBelongToTheSessionOwner(t *testing.T) {
	e := newFileEnv(t)
	other := e.h.member(e.c, "bob")
	os.WriteFile(filepath.Join(e.root, "secret"), []byte("s"), 0o600)
	for _, r := range []resp{
		other.do("GET", e.url("", nil), nil),
		other.do("GET", e.url("download", url.Values{"path": {e.root + "/secret"}}), nil),
		other.do("POST", e.url("delete", nil), map[string]any{"path": e.root + "/secret"}),
		other.do("GET", e.url("cwd", nil), nil),
	} {
		if r.Status != 404 {
			t.Errorf("other user: %d %s", r.Status, r.Body)
		}
	}
	if _, err := os.Stat(filepath.Join(e.root, "secret")); err != nil {
		t.Fatal("file deleted by another user")
	}
	// A plain shell has no folder to report.
	r := e.c.do("GET", e.url("cwd", nil), nil)
	mustStatus(t, r, 200, "cwd")
	if r.json()["path"] != "" {
		t.Fatalf("cwd %v", r.json()["path"])
	}
}

func TestFilesCwdAsksTmuxForTheActivePane(t *testing.T) {
	h := newHarness(t)
	admin := h.admin()
	home := t.TempDir()
	srv := h.sshServer(home)
	bin := filepath.Join(home, ".local", "bin")
	os.MkdirAll(bin, 0o755)
	// A stand-in tmux that answers only for the exact session target.
	os.WriteFile(filepath.Join(bin, "tmux"), []byte("#!/bin/sh\n[ \"$5\" = \"=work:\" ] && [ \"$1\" = -u ] && echo '/srv/my project'\n"), 0o755)
	srv.HandleExec(testutil.ShellExec(home, func(cmd string) bool { return strings.Contains(cmd, "display-message") }))
	r := admin.do("POST", "/api/hosts", map[string]any{"name": "box", "address": "127.0.0.1", "port": srv.Addr.Port,
		"username": "alice", "auth": "password", "password": "alice-pass", "persistence": "tmux", "tmux_session": "work"})
	mustStatus(t, r, 201, "create host")
	r = admin.do("POST", "/api/sessions", map[string]any{"host_id": r.json()["id"]})
	mustStatus(t, r, 201, "open session")
	r = admin.do("GET", "/api/sessions/"+r.json()["id"].(string)+"/files/cwd", nil)
	mustStatus(t, r, 200, "cwd")
	if r.json()["path"] != "/srv/my project" {
		t.Fatalf("cwd %v", r.json()["path"])
	}
}

func TestFilesRefusedWhileChannelsHaveNotEnded(t *testing.T) {
	oldWait := sftpSlotWait
	sftpSlotWait = 120 * time.Millisecond
	t.Cleanup(func() { sftpSlotWait = oldWait })

	e := newFileEnv(t)
	sess, err := e.h.terms.Get(e.h.userID(e.c), e.sess)
	if err != nil {
		t.Fatal(err)
	}
	var held []func()
	for i := 0; i < sshx.MaxSFTPPerConn; i++ {
		rel, ok := takeSFTPChan(sess.Client())
		if !ok {
			t.Fatal("quota full too early")
		}
		held = append(held, rel)
	}
	if r := e.c.do("GET", e.url("", nil), nil); r.Status != http.StatusTooManyRequests || r.code() != "ssh_channels_busy" {
		t.Fatalf("listing with no channel left: %d %s", r.Status, r.Body)
	}
	for _, rel := range held {
		rel()
	}
	mustStatus(t, e.c.do("GET", e.url("", nil), nil), 200, "listing once channels ended")
}

// Even with zero SFTP channels open, filling the unified SSH session ledger
// via other classes (Terminal + Pane + Aux = 9/9) blocks SFTP with
// ssh_channels_busy after queueing, and releasing one slot during the queue
// window lets the file listing succeed.
func TestFilesRespectsUnifiedSessionLedgerAndQueues(t *testing.T) {
	oldWait := sftpSlotWait
	sftpSlotWait = 250 * time.Millisecond
	t.Cleanup(func() { sftpSlotWait = oldWait })

	e := newFileEnv(t)
	sess, err := e.h.terms.Get(e.h.userID(e.c), e.sess)
	if err != nil {
		t.Fatal(err)
	}
	c := sess.Client()

	// Terminal already holds 1/9; fill 4 Pane + 4 interactive Aux = 9/9.
	var held []func()
	t.Cleanup(func() {
		for _, rel := range held {
			rel()
		}
	})
	for range sshx.MaxPanePerConn {
		rel, err := sshx.AcquireSession(context.Background(), c, sshx.SessionPane, true, 0)
		if err != nil {
			t.Fatalf("fill pane: %v", err)
		}
		held = append(held, rel)
	}
	for range sshx.MaxAuxPerConn {
		rel, err := sshx.AcquireSession(context.Background(), c, sshx.SessionAux, true, 0)
		if err != nil {
			t.Fatalf("fill aux: %v", err)
		}
		held = append(held, rel)
	}

	// Still full across the queue window -> 429 ssh_channels_busy.
	r := e.c.do("GET", e.url("", nil), nil)
	if r.Status != http.StatusTooManyRequests || r.code() != "ssh_channels_busy" {
		t.Fatalf("expected 429 ssh_channels_busy when unified ledger full, got %d %s", r.Status, r.Body)
	}

	// Release one slot midway through the queue window -> listing wakes and succeeds.
	go func() {
		time.Sleep(60 * time.Millisecond)
		held[0]()
	}()
	mustStatus(t, e.c.do("GET", e.url("", nil), nil), 200, "listing after slot freed during queue")
}

// The host-side check before rm -rf, run by a real sh.
func TestRemoveTreeCommandProtectsHome(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	base := t.TempDir()
	parent := filepath.Join(base, "users\n") // a folder name ending in a line break
	home := filepath.Join(parent, "alice")
	os.MkdirAll(filepath.Join(home, "work", "deep"), 0o755)
	os.WriteFile(filepath.Join(home, "work", "deep", "f"), []byte("x"), 0o644)
	os.Symlink(parent, filepath.Join(base, "alias"))
	run := func(p string) int {
		cmd := exec.Command("sh", "-c", removeTreeCommand(p))
		cmd.Env = []string{"HOME=" + home, "PATH=" + os.Getenv("PATH")}
		err := cmd.Run()
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return ee.ExitCode()
		}
		if err != nil {
			t.Fatal(err)
		}
		return 0
	}
	for _, p := range []string{home, parent, base, filepath.Join(base, "alias", "alice")} {
		if code := run(p); code != 4 {
			t.Errorf("%q: exit %d, want 4", p, code)
		}
	}
	if _, err := os.Stat(filepath.Join(home, "work", "deep", "f")); err != nil {
		t.Fatal("home content gone")
	}
	if code := run(filepath.Join(base, "alias", "alice", "missing")); code != 3 {
		t.Errorf("missing: exit %d, want 3", code)
	}
	if code := run(filepath.Join(base, "alias", "alice", "work")); code != 0 {
		t.Fatalf("delete work: exit %d", code)
	}
	if _, err := os.Stat(filepath.Join(home, "work")); !os.IsNotExist(err) {
		t.Fatal("work not deleted")
	}
}

// A huge folder is not read whole: the listing stops at the limit.
func TestFilesListStopsAtTheLimit(t *testing.T) {
	e := newFileEnv(t)
	big := filepath.Join(e.root, "big")
	os.Mkdir(big, 0o755)
	for i := 0; i < maxListEntries+1000; i++ {
		f, err := os.Create(filepath.Join(big, "f"+strconv.Itoa(i)))
		if err != nil {
			t.Fatal(err)
		}
		f.Close()
	}
	r := e.c.do("GET", e.url("", url.Values{"path": {big}}), nil)
	mustStatus(t, r, 200, "list big folder")
	if n := len(r.json()["entries"].([]any)); n != maxListEntries || r.json()["truncated"] != true {
		t.Fatalf("%d entries, truncated %v", n, r.json()["truncated"])
	}
	small := e.c.do("GET", e.url("", url.Values{"path": {e.root}}), nil)
	if small.json()["truncated"] != false {
		t.Fatal("a small folder says it is truncated")
	}
}

// An uploaded file's path gets "@" in front when Gemini CLI is the program
// in front: told by the pane title in tmux, by the session's title otherwise.
func TestUploadPrefixForGemini(t *testing.T) {
	png := append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 64)...)
	for _, tc := range []struct{ title, args, want string }{
		{"◇  Ready (proj)", "node /usr/local/bin/gemini", "@"},
		{"Gemini CLI (proj)", "gemini", "@"},
		{"✦  Working… (proj)", "npm exec @google/gemini-cli", "@"},
		{"✳ Claude Code", "claude", ""},
		{"bash", "-bash", ""},
		{"Gemini CLI (proj)", "-bash", ""}, // the title Gemini left behind, the shell in front
		{"◇  Ready (proj)", "node", ""},    // ... or another Node program
		{"◇  Ready (proj)", "vim notes.txt", ""},
	} {
		h := newHarness(t)
		admin := h.admin()
		home := t.TempDir()
		srv := h.sshServer(home)
		bin := filepath.Join(home, ".local", "bin")
		os.MkdirAll(bin, 0o755)
		os.WriteFile(filepath.Join(bin, "tmux"), []byte("#!/bin/sh\n[ \"$5\" = \"=work:\" ] && [ \"$1\" = -u ] && [ \"$6\" = '#{pane_pid} #{pane_title}' ] && printf '%s\\n' '4141 "+tc.title+"'\n"), 0o755)
		// The shell is 4141; the program in front of it is 4242.
		os.WriteFile(filepath.Join(bin, "ps"), []byte("#!/bin/sh\ncase \"$*\" in '-o tpgid= -p 4141') echo '  4242';; '-ww -o args= -p 4242') echo '"+tc.args+"';; *) exit 1;; esac\n"), 0o755)
		srv.HandleExec(testutil.ShellExec(home, func(cmd string) bool { return strings.Contains(cmd, "display-message") }))
		r := admin.do("POST", "/api/hosts", map[string]any{"name": "box", "address": "127.0.0.1", "port": srv.Addr.Port,
			"username": "alice", "auth": "password", "password": "alice-pass", "persistence": "tmux", "tmux_session": "work"})
		r = admin.do("POST", "/api/sessions", map[string]any{"host_id": r.json()["id"]})
		up := admin.upload(r.json()["id"].(string), png)
		mustStatus(t, up, 201, "upload")
		if got, _ := up.json()["prefix"].(string); got != tc.want {
			t.Errorf("title %q, program %q: prefix %q, want %q", tc.title, tc.args, got, tc.want)
		}
	}
	// A plain shell cannot tell what runs in front: no prefix, whatever the title.
	h := newHarness(t)
	admin := h.admin()
	srv := h.sshServer(t.TempDir())
	hr := admin.do("POST", "/api/hosts", map[string]any{"name": "plain", "address": "127.0.0.1", "port": srv.Addr.Port,
		"username": "alice", "auth": "password", "password": "alice-pass", "persistence": "none"})
	id := admin.do("POST", "/api/sessions", map[string]any{"host_id": hr.json()["id"]}).json()["id"].(string)
	sess, _ := h.terms.Get(h.userID(admin), id)
	sess.Write(t.Context(), []byte("\x1b]2;Gemini CLI (x)\x07")) // the test host echoes it back as output
	for i := 0; i < 50 && sess.Info().Title == ""; i++ {
		time.Sleep(20 * time.Millisecond)
	}
	if got, _ := admin.upload(id, png).json()["prefix"].(string); got != "" {
		t.Fatalf("plain shell with a Gemini title: prefix %q (title %q)", got, sess.Info().Title)
	}
}

func TestGeminiCommandLine(t *testing.T) {
	for args, want := range map[string]bool{
		"gemini":                     true,
		"/usr/local/bin/gemini -m x": true,
		"node /usr/local/bin/gemini": true,
		"node --max-old-space-size=8192 /usr/lib/node_modules/@google/gemini-cli/dist/index.js": true,
		"npm exec @google/gemini-cli":                                    true,
		"npx -y @google/gemini-cli@latest":                               true,
		"bun x @google/gemini-cli":                                       true,
		"pnpm dlx @google/gemini-cli@0.64.0":                             true,
		"node --require ./preload.cjs /usr/local/bin/gemini":             true,
		"node --conditions gemini server.js":                             false,
		"bun run /usr/lib/node_modules/@google/gemini-cli/dist/index.js": true,
		"deno run -a npm:@google/gemini-cli":                             true,
		"deno run -A -r npm:@google/gemini-cli":                          true,
		"node --diagnostic-dir /tmp /usr/local/bin/gemini":               true,
		"bun --smol run /usr/local/bin/gemini":                           true,
		"bun add @google/gemini-cli":                                     false,
		"node gemini":                                                    false,
		"npm exec gemini":                                                true,
		"npx gemini":                                                     true,
		"bun x gemini":                                                   true,
		"deno run -A chat.ts --provider gemini":                          false,
		"bun run dev gemini":                                             false,
		"npm exec gemini-helper":                                         false,
		"npm install -g @google/gemini-cli":                              false,
		"npx @google/gemini-cli-extension":                               false,
		"npm exec prettier @google/gemini-cli":                           false,
		"node":                                                           false,
		"-bash":                                                          false,
		"vim gemini.js":                                                  false,
		"vim /work/gemini-cli/README.md":                                 false,
		"node server.js gemini":                                          false,
		"npm run build":                                                  false,
		"claude --resume":                                                false,
		"node /home/a/proj/scripts/build.js":                             false,
	} {
		if got := geminiCommandLine(args); got != want {
			t.Errorf("%q: %v, want %v", args, got, want)
		}
	}
}

// A pane named (a conversation's, "%3"): that pane's folder, where it is of
// this session; another session's: none, never the active pane's instead.
func TestFilesCwdOfAPane(t *testing.T) {
	h := newHarness(t)
	admin := h.admin()
	home := t.TempDir()
	srv := h.sshServer(home)
	bin := filepath.Join(home, ".local", "bin")
	os.MkdirAll(bin, 0o755)
	// (%3 of work, in a window linked into "other" too, its own session that; %9 of other only.)
	os.WriteFile(filepath.Join(bin, "tmux"), []byte(`#!/bin/sh
[ "$2" = list-panes ] && { [ "$5" = "=work" ] && printf '%%1\n%%3\n'; [ "$5" = "=other" ] && printf '%%3\n%%9\n'; exit 0; }
case "$5|$6" in
  "=work:|#{pane_current_path}") echo /srv/active ;;
  "%3|#{session_name}") echo other ;;
  "%3|#{pane_current_path}") echo /srv/pane3 ;;
  "%9|#{session_name}") echo other ;;
  "%9|#{pane_current_path}") echo /srv/elsewhere ;;
esac
`), 0o755)
	srv.HandleExec(testutil.ShellExec(home, func(cmd string) bool { return strings.Contains(cmd, "display-message") }))
	r := admin.do("POST", "/api/hosts", map[string]any{"name": "box", "address": "127.0.0.1", "port": srv.Addr.Port,
		"username": "alice", "auth": "password", "password": "alice-pass", "persistence": "tmux", "tmux_session": "work"})
	mustStatus(t, r, 201, "create host")
	r = admin.do("POST", "/api/sessions", map[string]any{"host_id": r.json()["id"]})
	mustStatus(t, r, 201, "open session")
	url := "/api/sessions/" + r.json()["id"].(string) + "/files/cwd"
	for q, want := range map[string]string{"": "/srv/active", "?pane=%253": "/srv/pane3", "?pane=%259": ""} {
		r := admin.do("GET", url+q, nil)
		mustStatus(t, r, 200, "cwd "+q)
		if r.json()["path"] != want {
			t.Errorf("%q: %v, want %q", q, r.json()["path"], want)
		}
	}
}

// Deleting a folder is a direct user-clicked action, so it must be able to draw
// from InteractiveReserve even when background Aux calls are refused (e.g. 7/9
// slots already held: 1 Terminal + 4 Pane + 2 SFTP).
func TestFilesDeleteFolderUsesInteractiveReserve(t *testing.T) {
	e := newFileEnv(t)
	dir := filepath.Join(e.root, "dropme")
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sub", "f.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Open the file panel's shared SFTP channel first so it is already counted
	// (1 Terminal + 1 shared SFTP = 2/9).
	mustStatus(t, e.c.do("GET", e.url("", nil), nil), 200, "warm shared sftp")

	sess, err := e.h.terms.Get(e.h.userID(e.c), e.sess)
	if err != nil {
		t.Fatal(err)
	}
	// Hold 4 Pane + 1 more SFTP -> 1 Terminal + 4 Pane + 2 SFTP = 7/9 used,
	// leaving only the 2 InteractiveReserve slots free.
	var held []func()
	t.Cleanup(func() {
		for _, r := range held {
			r()
		}
	})
	for range sshx.MaxPanePerConn {
		rel, err := sshx.AcquireSession(context.Background(), sess.Client(), sshx.SessionPane, true, 0)
		if err != nil {
			t.Fatalf("fill pane: %v", err)
		}
		held = append(held, rel)
	}
	relSFTP, err := sshx.AcquireSession(context.Background(), sess.Client(), sshx.SessionSFTP, true, 0)
	if err != nil {
		t.Fatalf("fill sftp: %v", err)
	}
	held = append(held, relSFTP)

	// Sanity check: a background Aux call must be refused at 7/9 (7+1+2 > 9).
	if _, err := sshx.AcquireSession(context.Background(), sess.Client(), sshx.SessionAux, false, 0); !errors.Is(err, sshx.ErrBusy) {
		t.Fatalf("background Aux at 7/9: got %v, want ErrBusy", err)
	}

	// Deleting a folder MUST succeed into InteractiveReserve.
	mustStatus(t, e.c.do("POST", e.url("delete", nil), map[string]any{"path": dir}), 200, "delete folder into interactive reserve")
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("folder still exists after delete: %v", err)
	}
}

// Background shared-SFTP opens (e.g. conversation log polling and lineage
// probes) must use non-interactive priority and return ErrBusy immediately
// when only InteractiveReserve remains, whereas a user-triggered file panel
// open draws from InteractiveReserve and succeeds.
func TestSharedSFTPBackgroundVsInteractivePriority(t *testing.T) {
	e := newFileEnv(t)
	sess, err := e.h.terms.Get(e.h.userID(e.c), e.sess)
	if err != nil {
		t.Fatal(err)
	}

	// Hold 4 Pane + 2 SFTP on top of 1 Terminal = 7/9 used; only the 2
	// InteractiveReserve slots remain (and shared SFTP is NOT open yet).
	var held []func()
	t.Cleanup(func() {
		for _, r := range held {
			r()
		}
	})
	for range sshx.MaxPanePerConn {
		rel, err := sshx.AcquireSession(context.Background(), sess.Client(), sshx.SessionPane, true, 0)
		if err != nil {
			t.Fatalf("fill pane: %v", err)
		}
		held = append(held, rel)
	}
	for range 2 {
		rel, err := sshx.AcquireSession(context.Background(), sess.Client(), sshx.SessionSFTP, true, 0)
		if err != nil {
			t.Fatalf("fill sftp: %v", err)
		}
		held = append(held, rel)
	}

	// Background shared SFTP open MUST fail immediately with sshx.ErrBusy
	// without consuming InteractiveReserve.
	start := time.Now()
	bgErr := e.h.srv.withSFTPBackground(context.Background(), sess, fileOpTimeout, func(c *sftplite.Client) error {
		_, err := c.RealPath(".")
		return err
	})
	if !errors.Is(bgErr, sshx.ErrBusy) {
		t.Fatalf("background SFTP open at 7/9: got %v, want sshx.ErrBusy", bgErr)
	}
	if d := time.Since(start); d > 150*time.Millisecond {
		t.Fatalf("background SFTP open did not fail immediately: waited %v", d)
	}
	if got := sshx.SessionInUse(sess.Client()); got != 7 {
		t.Fatalf("background SFTP open altered ledger count: got %d, want 7", got)
	}

	// User opening the file panel MUST succeed into InteractiveReserve (7 -> 8).
	mustStatus(t, e.c.do("GET", e.url("", nil), nil), 200, "interactive file panel open into reserve")
	if got := sshx.SessionInUse(sess.Client()); got != 8 {
		t.Fatalf("after interactive SFTP open: ledger count = %d, want 8", got)
	}

	// Once the shared SFTP channel is already open, background readers reuse it
	// without opening a second SSH channel.
	if err := e.h.srv.withSFTPBackground(context.Background(), sess, fileOpTimeout, func(c *sftplite.Client) error {
		_, err := c.RealPath(".")
		return err
	}); err != nil {
		t.Fatalf("background reuse of already-open shared SFTP failed: %v", err)
	}
	if got := sshx.SessionInUse(sess.Client()); got != 8 {
		t.Fatalf("background reuse opened extra channel: ledger count = %d, want 8", got)
	}
}

// codexTmuxNamedNow (user-triggered) must use interactive priority so it can
// draw from InteractiveReserve when background codexTmuxNamed is refused, and
// must propagate sshx.ErrBusy when full so convoPress returns 429
// ssh_channels_busy instead of misreporting 409 agent_changed.
func TestCodexTmuxNamedNowInteractiveAndBusy(t *testing.T) {
	h := newHarness(t)
	admin := h.admin()
	root := t.TempDir()
	srv := h.sshServer(root)

	const (
		threadID = "019a0000-0000-7000-8000-000000000001"
		logPath  = "/tmp/rollout-2026-11-27-" + threadID + ".jsonl"
	)
	codexPanesRecord := strings.Join([]string{
		"%3", "1234", "56789", "1700000000000", "/tmp/proj",
		threadID, logPath, "my-thread", "title", "first",
		"1", "0", "db", "0", "", "0", "my-thread | proj",
	}, "\x1f")

	r := admin.do("POST", "/api/sessions", map[string]any{"host_id": h.hostFor(admin, srv.Addr.Port)})
	mustStatus(t, r, 201, "open session")
	sessID := r.json()["id"].(string)
	sess, err := h.terms.Get(h.userID(admin), sessID)
	if err != nil {
		t.Fatal(err)
	}
	sess.Persistence = "tmux"
	sess.TmuxSession = "s1"

	var (
		mu       sync.Mutex
		onLocate func()
	)
	srv.HandleExec(func(cmd string) func(ch ssh.Channel) {
		isLocate := strings.Contains(cmd, "panetty=") || (strings.Contains(cmd, "/proc/") && !strings.Contains(cmd, "python3"))
		isCodexPanes := strings.Contains(cmd, "python3") && strings.Contains(cmd, "codex")
		if !isLocate && !isCodexPanes {
			return nil
		}
		return func(ch ssh.Channel) {
			defer ch.Close()
			if isLocate {
				mu.Lock()
				fn := onLocate
				mu.Unlock()
				if fn != nil {
					fn()
				}
				// Minimal parseConvoLocate output for a daemon-backed tmux Codex in %3.
				rec := strings.Join([]string{
					"tmux=%3 999 /dev/pts/3 my-thread | proj",
					"panetty=/dev/pts/3",
					"pid=1234\nstart=56789\nprocstart=56789\nsid=" + convoNoSession + "\nfg=1\nagent=codex\nTMUX=/tmp/tmux-1000/default,999,0\nTMUX_PANE=%3\ntty=/dev/pts/3",
				}, "\x00")
				_, _ = io.WriteString(ch, rec)
			} else {
				_, _ = io.WriteString(ch, codexPanesRecord)
			}
			_, _ = ch.SendRequest("exit-status", false, ssh.Marshal(&struct{ Status uint32 }{0}))
		}
	})

	// Hold 4 Pane + 2 SFTP on top of 1 Terminal = 7/9 used; only the 2
	// InteractiveReserve slots remain.
	var held []func()
	releaseAll := func() {
		for _, rel := range held {
			rel()
		}
		held = nil
	}
	t.Cleanup(releaseAll)
	for range sshx.MaxPanePerConn {
		rel, err := sshx.AcquireSession(context.Background(), sess.Client(), sshx.SessionPane, true, 0)
		if err != nil {
			t.Fatalf("fill pane: %v", err)
		}
		held = append(held, rel)
	}
	for range 2 {
		rel, err := sshx.AcquireSession(context.Background(), sess.Client(), sshx.SessionSFTP, true, 0)
		if err != nil {
			t.Fatalf("fill sftp: %v", err)
		}
		held = append(held, rel)
	}

	proc := &convoProc{pid: 1234, start: 56789, agent: "codex", tmuxPane: "%3", fg: true}
	located := convoLocated{procs: []convoProc{*proc}}

	// 1) Background codexTmuxNamed MUST be refused immediately at 7/9.
	if _, ok := h.srv.codexTmuxNamed(context.Background(), sess, proc, located); ok {
		t.Fatal("background codexTmuxNamed succeeded at 7/9, want refusal")
	}

	// 2) Interactive codexTmuxNamedNow MUST succeed into InteractiveReserve.
	front := *proc
	ok, err := h.srv.codexTmuxNamedNow(context.Background(), sess, &front, located, true)
	if err != nil || !ok {
		t.Fatalf("interactive codexTmuxNamedNow at 7/9: ok=%v err=%v, want true/nil", ok, err)
	}
	if front.sid != threadID || front.log != logPath {
		t.Fatalf("codexTmuxNamedNow populated sid=%q log=%q, want %q / %q", front.sid, front.log, threadID, logPath)
	}

	// 3) At 9/9 full cap, interactive codexTmuxNamedNow MUST return sshx.ErrBusy.
	extra := make([]func(), 0, 2)
	for range 2 {
		rel, err := sshx.AcquireSession(context.Background(), sess.Client(), sshx.SessionSFTP, true, 0)
		if err != nil {
			t.Fatalf("fill last 2 sftp: %v", err)
		}
		extra = append(extra, rel)
	}
	frontBusy := *proc
	ok, err = h.srv.codexTmuxNamedNow(context.Background(), sess, &frontBusy, located, true)
	if ok || !errors.Is(err, sshx.ErrBusy) {
		t.Fatalf("interactive codexTmuxNamedNow at 9/9: ok=%v err=%v, want false/sshx.ErrBusy", ok, err)
	}
	// Release 1 of the 2 extra slots so convoLocateCommand can run at 8/9 -> 9/9,
	// then grab the remaining slot right as locate returns so codexTmuxNamedNow
	// inside convoPress hits 9/9 sshx.ErrBusy.
	extra[1]()
	var grabRel func()
	mu.Lock()
	onLocate = func() {
		// Queue a waiter for the final slot BEFORE locate exits so it takes the
		// 9th slot deterministically the instant locate releases its Aux slot,
		// ensuring codexTmuxNamedNow inside convoPress hits 9/9 sshx.ErrBusy.
		started := make(chan struct{})
		go func() {
			close(started)
			if rel, aerr := sshx.AcquireSession(context.Background(), sess.Client(), sshx.SessionSFTP, true, 2*time.Second); aerr == nil {
				mu.Lock()
				grabRel = rel
				mu.Unlock()
			}
		}()
		<-started
		time.Sleep(25 * time.Millisecond)
	}
	mu.Unlock()
	t.Cleanup(func() {
		extra[0]()
		mu.Lock()
		if grabRel != nil {
			grabRel()
		}
		mu.Unlock()
	})

	// Make sure the slot grab happens cleanly: fill 1 slot right before calling send
	// is tricky because locate needs 1 slot first. Wait briefly inside locate?
	// Even cleaner: acquire the 9th slot via SessionSFTP while locate holds Aux?
	// No, 8+1(Aux)=9 so SFTP waits and immediately takes the 9th slot the instant
	// locate releases Aux, before codexTmuxNamedNow acquires Aux!
	resp := admin.do("POST", "/api/sessions/"+sessID+"/conversation/send", map[string]any{
		"pane":    "%3",
		"pid":     1234,
		"start":   56789,
		"session": threadID,
		"text":    "hello",
	})
	if resp.Status != http.StatusTooManyRequests || resp.code() != "ssh_channels_busy" {
		t.Fatalf("convoPress when codexTmuxNamedNow hits ErrBusy: status=%d code=%q body=%s, want 429 ssh_channels_busy", resp.Status, resp.code(), resp.Body)
	}
}

// Interactive endpoints must wait ~2s for a slot and map sshx.ErrBusy to
// 429 ssh_channels_busy rather than 502 host_error / inspect_failed.
func TestInteractiveEndpointsReturn429SSHChannelsBusyWhenFull(t *testing.T) {
	e := newFileEnv(t)
	sess, err := e.h.terms.Get(e.h.userID(e.c), e.sess)
	if err != nil {
		t.Fatal(err)
	}
	sess.Persistence = "tmux"
	sess.TmuxSession = "s1"

	// Fill all remaining 8 slots (4 Pane + 4 SFTP on top of 1 Terminal = 9/9).
	var held []func()
	t.Cleanup(func() {
		for _, rel := range held {
			rel()
		}
	})
	for range sshx.MaxPanePerConn {
		rel, err := sshx.AcquireSession(context.Background(), sess.Client(), sshx.SessionPane, true, 0)
		if err != nil {
			t.Fatalf("fill pane: %v", err)
		}
		held = append(held, rel)
	}
	for range sshx.MaxSFTPPerConn {
		rel, err := sshx.AcquireSession(context.Background(), sess.Client(), sshx.SessionSFTP, true, 0)
		if err != nil {
			t.Fatalf("fill sftp: %v", err)
		}
		held = append(held, rel)
	}

	endpoints := []struct {
		name string
		path string
	}{
		{"codex-sessions", "/api/sessions/" + e.sess + "/codex-sessions?dir=/tmp"},
		{"agy-sessions", "/api/sessions/" + e.sess + "/agy-sessions?dir=/tmp"},
		{"agent-sessions", "/api/sessions/" + e.sess + "/agent-sessions?dir=/tmp"},
		{"pane-inspect", "/api/sessions/" + e.sess + "/pane-inspect?pane=%253"},
		{"complete", "/api/sessions/" + e.sess + "/complete?cwd=/tmp"},
	}

	// Run all 5 concurrently so the ~2s interactive AwaitSlot runs in parallel.
	var wg sync.WaitGroup
	for _, ep := range endpoints {
		wg.Add(1)
		go func(name, path string) {
			defer wg.Done()
			start := time.Now()
			r := e.c.do("GET", path, nil)
			elapsed := time.Since(start)
			if r.Status != http.StatusTooManyRequests || r.code() != "ssh_channels_busy" {
				t.Errorf("%s: status=%d code=%q body=%s, want 429 ssh_channels_busy", name, r.Status, r.code(), r.Body)
			}
			if elapsed < 1500*time.Millisecond || elapsed > 4*time.Second {
				t.Errorf("%s: elapsed=%v, want ~2s interactive slot wait", name, elapsed)
			}
		}(ep.name, ep.path)
	}
	wg.Wait()
}
