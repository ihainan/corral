package web

import (
	"bytes"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ihainan/corral/internal/testutil"
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
	os.WriteFile(filepath.Join(bin, "tmux"), []byte("#!/bin/sh\n[ \"$4\" = \"=work:\" ] && echo '/srv/my project'\n"), 0o755)
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
	e := newFileEnv(t)
	sess, err := e.h.terms.Get(e.h.userID(e.c), e.sess)
	if err != nil {
		t.Fatal(err)
	}
	var held []func()
	for i := 0; i < maxSFTPPerConn; i++ {
		rel, ok := takeSFTPChan(sess.Client())
		if !ok {
			t.Fatal("quota full too early")
		}
		held = append(held, rel)
	}
	if r := e.c.do("GET", e.url("", nil), nil); r.Status != http.StatusTooManyRequests {
		t.Fatalf("listing with no channel left: %d %s", r.Status, r.Body)
	}
	for _, rel := range held {
		rel()
	}
	mustStatus(t, e.c.do("GET", e.url("", nil), nil), 200, "listing once channels ended")
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
