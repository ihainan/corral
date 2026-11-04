package web

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ihainan/corral/internal/askhook"
	"github.com/ihainan/corral/internal/sshx"
	"github.com/ihainan/corral/internal/store"
	"github.com/ihainan/corral/internal/testutil"
)

// The scripts run here by sh, with a home of the test's own.
func runAskHookScript(t *testing.T, home, script string, stdin []byte, args ...string) (string, int) {
	t.Helper()
	cmd := exec.Command("sh", append([]string{"-c", script, "sh"}, args...)...)
	cmd.Env = append(os.Environ(), "HOME="+home)
	cmd.Stdin = bytes.NewReader(stdin)
	out, err := cmd.Output()
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return string(out), ee.ExitCode()
	}
	if err != nil {
		t.Fatal(err)
	}
	return string(out), 0
}

func shaOf(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func readAt(t *testing.T, home string) *askHookFileRead {
	t.Helper()
	out, code := runAskHookScript(t, home, askHookRead, nil)
	if code != 0 {
		t.Fatalf("read: %d", code)
	}
	f, err := parseAskHookRead([]byte(out))
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func TestAskHookWriteNew(t *testing.T) {
	home := t.TempDir()
	if info := askHookInfoOf(readAt(t, home)); info.State != askhook.Off || info.Cannot != "" || info.Seen != "none" {
		t.Fatalf("no file: %+v", info)
	}
	next, _, _ := askhook.Enable(nil)
	if out, code := runAskHookScript(t, home, askHookWrite, next, "none", "write", shaOf(next)); code != 0 || strings.TrimSpace(out) != "done" {
		t.Fatalf("write: %d %q", code, out)
	}
	f := filepath.Join(home, ".claude", "settings.json")
	b, _ := os.ReadFile(f)
	st, _ := os.Stat(f)
	dir, _ := os.Stat(filepath.Dir(f))
	if !bytes.Equal(b, next) || st.Mode().Perm() != 0o600 || dir.Mode().Perm() != 0o700 {
		t.Errorf("%s %v %v", b, st.Mode(), dir.Mode())
	}
	if _, err := os.Stat(f + ".corral-bak"); !os.IsNotExist(err) {
		t.Error("a backup of nothing")
	}
	info := askHookInfoOf(readAt(t, home))
	if info.State != askhook.On || info.Seen != shaOf(next) {
		t.Errorf("%+v", info)
	}
	// Off again: nothing else in it, so the file goes (a backup of it kept).
	off, empty, _, _ := askhook.Disable(b)
	if !empty {
		t.Fatal("not empty")
	}
	_ = off
	if out, code := runAskHookScript(t, home, askHookWrite, nil, info.Seen, "remove"); code != 0 || strings.TrimSpace(out) != "done" {
		t.Fatalf("remove: %d %q", code, out)
	}
	if _, err := os.Stat(f); !os.IsNotExist(err) {
		t.Error("still there")
	}
	if bak, _ := os.ReadFile(f + ".corral-bak"); !bytes.Equal(bak, next) {
		t.Error("backup")
	}
	if left, _ := filepath.Glob(filepath.Join(home, ".claude", ".settings.json.corral*")); len(left) > 0 {
		t.Errorf("temporaries left: %v", left)
	}
}

func TestAskHookWriteKeepsModeAndBacksUp(t *testing.T) {
	home := t.TempDir()
	d := filepath.Join(home, ".claude")
	os.MkdirAll(d, 0o755)
	f := filepath.Join(d, "settings.json")
	old := []byte(`{"model":"opus"}` + "\n")
	os.WriteFile(f, old, 0o644)
	os.Chmod(f, 0o644)
	next, _, _ := askhook.Enable(old)
	if _, code := runAskHookScript(t, home, askHookWrite, next, shaOf(old), "write", shaOf(next)); code != 0 {
		t.Fatal(code)
	}
	st, _ := os.Stat(f)
	b, _ := os.ReadFile(f)
	bak, _ := os.ReadFile(f + ".corral-bak")
	if st.Mode().Perm() != 0o644 || !bytes.Equal(b, next) || !bytes.Equal(bak, old) {
		t.Errorf("%v %s %s", st.Mode(), b, bak)
	}
}

// Changed since read (or appeared): nothing written, nothing backed up.
func TestAskHookWriteRefusesAChangedFile(t *testing.T) {
	home := t.TempDir()
	d := filepath.Join(home, ".claude")
	os.MkdirAll(d, 0o700)
	f := filepath.Join(d, "settings.json")
	now := []byte(`{"model":"sonnet"}`)
	os.WriteFile(f, now, 0o600)
	next, _, _ := askhook.Enable([]byte(`{"model":"opus"}`))
	if _, code := runAskHookScript(t, home, askHookWrite, next, shaOf([]byte(`{"model":"opus"}`)), "write", shaOf(next)); code != 3 {
		t.Fatalf("exit %d", code)
	}
	if _, code := runAskHookScript(t, home, askHookWrite, next, "none", "write", shaOf(next)); code != 3 {
		t.Fatalf("appeared: exit %d", code)
	}
	if b, _ := os.ReadFile(f); !bytes.Equal(b, now) {
		t.Error("written")
	}
	if _, err := os.Stat(f + ".corral-bak"); !os.IsNotExist(err) {
		t.Error("backed up")
	}
	if left, _ := filepath.Glob(filepath.Join(d, ".settings.json.corral*")); len(left) > 0 {
		t.Errorf("temporaries left: %v", left)
	}
}

// A backup name that is a link to another file: the link replaced, its target untouched.
func TestAskHookBackupLinkNotFollowed(t *testing.T) {
	home := t.TempDir()
	d := filepath.Join(home, ".claude")
	os.MkdirAll(d, 0o700)
	f := filepath.Join(d, "settings.json")
	old := []byte(`{}`)
	os.WriteFile(f, old, 0o600)
	victim := filepath.Join(home, "victim")
	os.WriteFile(victim, []byte("keep"), 0o600)
	os.Symlink(victim, f+".corral-bak")
	next, _, _ := askhook.Enable(old)
	if _, code := runAskHookScript(t, home, askHookWrite, next, shaOf(old), "write", shaOf(next)); code != 0 {
		t.Fatal(code)
	}
	if v, _ := os.ReadFile(victim); string(v) != "keep" {
		t.Errorf("victim: %q", v)
	}
	if st, _ := os.Lstat(f + ".corral-bak"); st.Mode()&os.ModeSymlink != 0 {
		t.Error("still a link")
	}
}

// The settings file a link, or hard-linked: told so on reading, never written.
func TestAskHookLinksRefused(t *testing.T) {
	for _, kind := range []string{"link", "hard_link"} {
		home := t.TempDir()
		d := filepath.Join(home, ".claude")
		os.MkdirAll(d, 0o700)
		f := filepath.Join(d, "settings.json")
		real := filepath.Join(home, "dotfiles.json")
		os.WriteFile(real, []byte(`{}`), 0o600)
		if kind == "link" {
			os.Symlink(real, f)
		} else {
			os.Link(real, f)
		}
		info := askHookInfoOf(readAt(t, home))
		if info.Cannot != kind {
			t.Errorf("%s: %+v", kind, info)
		}
		next, _, _ := askhook.Enable([]byte(`{}`))
		if _, code := runAskHookScript(t, home, askHookWrite, next, shaOf([]byte(`{}`)), "write", shaOf(next)); code != 4 {
			t.Errorf("%s: exit %d", kind, code)
		}
		if b, _ := os.ReadFile(real); string(b) != `{}` {
			t.Errorf("%s: written through: %s", kind, b)
		}
	}
}

func TestAskHookInfoOfOddFiles(t *testing.T) {
	cases := []struct {
		read   string
		cannot string
		state  askhook.State
	}{
		{"kind=file\nlinks=1\nsum=1\nsettings=" + b64(`{"a": // x`) + "\n", "invalid", ""},
		{"kind=file\nlinks=1\nsettings=" + b64(`{}`) + "\n", "no_sum", askhook.Off},
		{"kind=other\nsum=1\n", "not_file", ""},
		{"kind=none\nsum=1\nmanaged=" + b64(`{"allowManagedHooksOnly":true}`) + "\n", "", askhook.Blocked},
	}
	for _, c := range cases {
		f, err := parseAskHookRead([]byte(c.read))
		if err != nil {
			t.Fatal(err)
		}
		info := askHookInfoOf(f)
		if info.Cannot != c.cannot || info.State != c.state {
			t.Errorf("%q: %+v", c.read, info)
		}
	}
	if _, err := parseAskHookRead([]byte("garbage\n")); err == nil {
		t.Error("garbage read")
	}
}

func b64(s string) string {
	out, _ := exec.Command("sh", "-c", "printf %s \"$1\" | base64 | tr -d '\\n'", "sh", s).Output()
	return string(out)
}

// The endpoints, over a terminal open to a test host whose commands run in a
// home of the test's own.
func TestAskHookEndpoints(t *testing.T) {
	h := newHarness(t)
	admin := h.admin()
	home := t.TempDir()
	srv := h.sshServer(home)
	srv.HandleExec(testutil.ShellExec(home, func(cmd string) bool { return strings.Contains(cmd, ".claude") }))
	hostID := h.hostFor(admin, srv.Addr.Port)
	u := fmt.Sprintf("/api/hosts/%d/ask-hook", hostID)

	r := admin.do("GET", u, nil)
	mustStatus(t, r, 409, "no terminal yet")
	if r.json()["error"] != "no_connection" {
		t.Errorf("%v", r.json())
	}
	mustStatus(t, admin.do("POST", "/api/sessions", map[string]any{"host_id": hostID}), 201, "open session")

	r = admin.do("GET", u, nil)
	mustStatus(t, r, 200, "read")
	if j := r.json(); j["state"] != "off" || j["seen"] != "none" || j["declined"] != false || j["file"] != "~/.claude/settings.json" {
		t.Fatalf("%v", j)
	}
	// Someone else's host: not there.
	other := h.member(admin, "bob")
	mustStatus(t, other.do("GET", u, nil), 404, "another's host")
	mustStatus(t, other.do("POST", u, map[string]any{"on": true, "seen": "none"}), 404, "another's host")

	// No.
	mustStatus(t, admin.do("POST", u, map[string]any{"declined": true}), 200, "declined")
	if j := admin.do("GET", u, nil).json(); j["declined"] != true {
		t.Errorf("%v", j)
	}
	// On, with the file as seen: written; the no taken back.
	r = admin.do("POST", u, map[string]any{"on": true, "seen": "none"})
	mustStatus(t, r, 200, "on")
	j := r.json()
	if j["state"] != "on" || j["declined"] != false {
		t.Fatalf("%v", j)
	}
	f := filepath.Join(home, ".claude", "settings.json")
	b, _ := os.ReadFile(f)
	if st, _, _ := askhook.Read(b, nil); st != askhook.On {
		t.Fatalf("%s", b)
	}
	// Seen as it no longer is: refused, the file as it is told.
	r = admin.do("POST", u, map[string]any{"on": false, "seen": "none"})
	mustStatus(t, r, 409, "stale")
	if r.json()["error"] != "changed" {
		t.Errorf("%v", r.json())
	}
	// Off: the file was ours alone, so it goes.
	mustStatus(t, admin.do("POST", u, map[string]any{"on": false, "seen": j["seen"]}), 200, "off")
	if _, err := os.Stat(f); !os.IsNotExist(err) {
		t.Error("left")
	}
	// A file of the person's that said nothing ({}, with its newline or not, or empty): kept once off,
	// not removed (review534-6, review535-1).
	for _, body := range []string{"{}", "{}\n", ""} {
		os.WriteFile(f, []byte(body), 0o600)
		r = admin.do("GET", u+"?fresh=1", nil)
		r = admin.do("POST", u, map[string]any{"on": true, "seen": r.json()["seen"]})
		mustStatus(t, r, 200, "on over "+body)
		mustStatus(t, admin.do("POST", u, map[string]any{"on": false, "seen": r.json()["seen"]}), 200, "off over "+body)
		if b, err := os.ReadFile(f); err != nil || strings.TrimSpace(string(b)) != "{}" {
			t.Errorf("%q: %q %v", body, b, err)
		}
	}
	// Hooks turned off there, or the person's own doing it: not put in (review534-7).
	for _, body := range []string{`{"disableAllHooks":true}`, `{"hooks":{"PreToolUse":[{"matcher":"*","hooks":[{"type":"command","command":"x"}]}]}}`} {
		os.WriteFile(f, []byte(body), 0o600)
		r = admin.do("GET", u+"?fresh=1", nil)
		mustStatus(t, admin.do("POST", u, map[string]any{"on": true, "seen": r.json()["seen"]}), 422, body)
		if b, _ := os.ReadFile(f); string(b) != body {
			t.Errorf("written: %s", b)
		}
	}
	// A file the gateway does not change: refused with what to add by hand.
	os.WriteFile(f, []byte(`{"a": // x`), 0o600)
	r = admin.do("GET", u+"?fresh=1", nil)
	mustStatus(t, r, 200, "odd file")
	if j := r.json(); j["cannot"] != "invalid" || !strings.Contains(fmt.Sprint(j["snippet"]), "AskUserQuestion") {
		t.Errorf("%v", j)
	}
	r = admin.do("POST", u, map[string]any{"on": true, "seen": r.json()["seen"]})
	mustStatus(t, r, 422, "odd file")
	if b, _ := os.ReadFile(f); string(b) != `{"a": // x` {
		t.Errorf("written: %s", b)
	}
	// Each change recorded.
	entries, err := h.srv.store.ListAudit(context.Background(), store.AuditQuery{Limit: 500})
	n := 0
	for _, e := range entries {
		if e.Action == "host.ask_hook" {
			n++
		}
	}
	if err != nil || n != 8 { // (on, off; on and off over three empty files)
		t.Errorf("audit: %d %v", n, err)
	}
}

// The bell hook beside the question hook, in the same file, each on its
// own: the file the gateway made from none goes once the last of them is
// off; a bell has no "no" to say; an unknown hook is refused.
func TestBellHookEndpoints(t *testing.T) {
	h := newHarness(t)
	admin := h.admin()
	home := t.TempDir()
	srv := h.sshServer(home)
	srv.HandleExec(testutil.ShellExec(home, func(cmd string) bool { return strings.Contains(cmd, ".claude") }))
	hostID := h.hostFor(admin, srv.Addr.Port)
	u := fmt.Sprintf("/api/hosts/%d/ask-hook", hostID)
	mustStatus(t, admin.do("POST", "/api/sessions", map[string]any{"host_id": hostID}), 201, "open session")
	r := admin.do("GET", u, nil)
	mustStatus(t, r, 200, "read")
	bell := func(j map[string]any) any {
		b, _ := j["bell"].(map[string]any)
		return b["state"]
	}
	if j := r.json(); bell(j) != "off" || !strings.Contains(fmt.Sprint(j["bell"]), "terminalSequence") {
		t.Fatalf("%v", j)
	}
	mustStatus(t, admin.do("POST", u, map[string]any{"declined": true, "hook": "bell"}), 400, "a bell's no")
	mustStatus(t, admin.do("POST", u, map[string]any{"on": true, "seen": "none", "hook": "other"}), 400, "unknown hook")
	r = admin.do("POST", u, map[string]any{"on": true, "seen": "none"})
	mustStatus(t, r, 200, "ask on")
	r = admin.do("POST", u, map[string]any{"on": true, "seen": r.json()["seen"], "hook": "bell"})
	mustStatus(t, r, 200, "bell on")
	j := r.json()
	if j["state"] != "on" || bell(j) != "on" {
		t.Fatalf("%v", j)
	}
	f := filepath.Join(home, ".claude", "settings.json")
	r = admin.do("POST", u, map[string]any{"on": false, "seen": j["seen"]})
	mustStatus(t, r, 200, "ask off")
	if j := r.json(); j["state"] != "off" || bell(j) != "on" {
		t.Fatalf("%v", j)
	}
	if b, _ := os.ReadFile(f); !bytes.Equal(b, askhook.Bell.Created()) {
		t.Fatalf("the bell's left: %s", b)
	}
	mustStatus(t, admin.do("POST", u, map[string]any{"on": false, "seen": r.json()["seen"], "hook": "bell"}), 200, "bell off")
	if _, err := os.Stat(f); !os.IsNotExist(err) {
		t.Error("the file the gateway made, left")
	}
}

// The agents the gateway starts: Claude Code with the hook given inline,
// beside a resume; Codex and OpenCode as before.
func TestAgentStartCmd(t *testing.T) {
	inline := sshx.QuoteShell(askhook.Inline())
	if c := agentStartCmd("/s", "w1:p1", "claude-1", "claude", agentStart{}); !strings.Contains(c, " -- '--settings' "+inline+" 2>&1") {
		t.Errorf("%s", c)
	}
	if c := agentStartCmd("/s", "w1:p1", "claude-1", "claude", agentStart{resume: "0b1c2d3e-0000-4000-8000-000000000000"}); !strings.Contains(c, " -- '--settings' "+inline+" '--resume' '0b1c2d3e-0000-4000-8000-000000000000' 2>&1") {
		t.Errorf("%s", c)
	}
	for _, k := range []string{"codex", "opencode"} {
		if c := agentStartCmd("/s", "w1:p1", k+"-1", k, agentStart{}); strings.Contains(c, "--settings") || strings.Contains(c, " -- ") {
			t.Errorf("%s", c)
		}
	}
}

// review534: the new file come short (its sum not the one sent): nothing
// written; another change under way on the host (its lock): refused, one
// left over a minute taken as gone; a backup name that is a folder: refused,
// a link to one: the link replaced, the folder untouched.
func TestAskHookWriteGuards(t *testing.T) {
	home := t.TempDir()
	d := filepath.Join(home, ".claude")
	os.MkdirAll(d, 0o700)
	f := filepath.Join(d, "settings.json")
	old := []byte(`{"a":1}`)
	os.WriteFile(f, old, 0o600)
	next, _, _ := askhook.Enable(old)
	if _, code := runAskHookScript(t, home, askHookWrite, next[:len(next)/2], shaOf(old), "write", shaOf(next)); code != 5 {
		t.Errorf("short: exit %d", code)
	}
	if b, _ := os.ReadFile(f); !bytes.Equal(b, old) {
		t.Errorf("short: written %s", b)
	}
	lk := filepath.Join(d, ".settings.json.corral-lock")
	os.Mkdir(lk, 0o700)
	if _, code := runAskHookScript(t, home, askHookWrite, next, shaOf(old), "write", shaOf(next)); code != 6 {
		t.Errorf("locked: exit %d", code)
	}
	past := time.Now().Add(-2 * time.Minute)
	// Old, but its taker still running: not taken over (review535-2).
	os.WriteFile(filepath.Join(lk, "pid"), []byte(fmt.Sprint(os.Getpid())), 0o600)
	os.Chtimes(lk, past, past)
	if _, code := runAskHookScript(t, home, askHookWrite, next, shaOf(old), "write", shaOf(next)); code != 6 {
		t.Errorf("an old lock of a live taker: exit %d", code)
	}
	// Old, its taker gone: taken over.
	gone := exec.Command("true")
	gone.Run()
	os.WriteFile(filepath.Join(lk, "pid"), []byte(fmt.Sprint(gone.Process.Pid)), 0o600)
	os.Chtimes(lk, past, past)
	if _, code := runAskHookScript(t, home, askHookWrite, next, shaOf(old), "write", shaOf(next)); code != 0 {
		t.Errorf("a stale lock: exit %d", code)
	}
	if _, err := os.Stat(lk); !os.IsNotExist(err) {
		t.Error("the lock left behind")
	}
	// Another taking a stale lock over meanwhile (its reclaim lock there): this one gives up
	// (review539-2); that reclaim lock left over a minute: cleared, the one finding it giving up.
	cur0, _ := os.ReadFile(f)
	again, _, _, _ := askhook.Disable(cur0)
	os.Mkdir(lk, 0o700)
	os.WriteFile(filepath.Join(lk, "pid"), []byte(fmt.Sprint(gone.Process.Pid)), 0o600)
	os.Chtimes(lk, past, past)
	rc := lk + ".reclaim"
	os.Mkdir(rc, 0o700)
	if _, code := runAskHookScript(t, home, askHookWrite, again, shaOf(cur0), "write", shaOf(again)); code != 6 {
		t.Errorf("another taking it over: exit %d", code)
	}
	os.Chtimes(rc, past, past)
	if _, code := runAskHookScript(t, home, askHookWrite, again, shaOf(cur0), "write", shaOf(again)); code != 6 {
		t.Errorf("a reclaim lock left: exit %d", code)
	}
	if _, err := os.Stat(rc); !os.IsNotExist(err) {
		t.Error("the reclaim lock left")
	}
	if _, code := runAskHookScript(t, home, askHookWrite, again, shaOf(cur0), "write", shaOf(again)); code != 0 {
		t.Errorf("then: exit %d", code)
	}
	for _, p := range []string{lk, rc} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s left", p)
		}
	}
	old = again
	// The backup's name a folder: refused, nothing written.
	cur, _ := os.ReadFile(f)
	b := f + ".corral-bak"
	os.Remove(b)
	os.Mkdir(b, 0o700)
	off, _, _, _ := askhook.Disable(cur)
	if _, code := runAskHookScript(t, home, askHookWrite, off, shaOf(cur), "write", shaOf(off)); code != 4 {
		t.Errorf("backup a folder: exit %d", code)
	}
	if now, _ := os.ReadFile(f); !bytes.Equal(now, cur) {
		t.Error("written past a folder")
	}
	// A link to a folder: the link replaced by the backup, the folder as it was.
	os.Remove(b)
	away := filepath.Join(home, "away")
	os.Mkdir(away, 0o700)
	os.Symlink(away, b)
	if _, code := runAskHookScript(t, home, askHookWrite, off, shaOf(cur), "write", shaOf(off)); code != 0 {
		t.Errorf("backup a link to a folder: exit %d", code)
	}
	if st, err := os.Lstat(b); err != nil || !st.Mode().IsRegular() {
		t.Errorf("backup: %v %v", st, err)
	}
	if left, _ := os.ReadDir(away); len(left) != 0 {
		t.Errorf("written into the folder: %v", left)
	}
}

// review534-4: a reading begun before a change is not kept after it.
func TestAskHookCacheGenerations(t *testing.T) {
	var a askHooks
	key := [2]int64{1, 2}
	g := a.generation(key)
	a.forget(key) // (a change, meanwhile)
	a.keep(key, 7, g, askHookInfo{State: askhook.Off})
	if _, ok := a.cached(key, 7); ok {
		t.Error("an old reading kept")
	}
	a.keep(key, 7, a.generation(key), askHookInfo{State: askhook.On})
	if i, ok := a.cached(key, 7); !ok || i.State != askhook.On {
		t.Error("a new reading not kept")
	}
	if _, ok := a.cached(key, 8); ok {
		t.Error("kept over a connection change")
	}
}

// review585-4: the file the gateway made, replaced since by one of the
// person's ({}): the bell put in and taken out of theirs, it stays.
func TestHookRecordNotForAnothersFile(t *testing.T) {
	if !onlyOurs(askhook.Ask.Created()) || !onlyOurs(askhook.Bell.Created()) {
		t.Fatal("the gateway's own")
	}
	both, _, _ := askhook.Bell.Enable(askhook.Ask.Created())
	if !onlyOurs(both) {
		t.Fatal("both of ours")
	}
	for _, f := range []string{"{}\n", "{}", `{"model":"x"}`, `{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"x"}]}]}}`} {
		if onlyOurs([]byte(f)) {
			t.Fatalf("%s taken as ours", f)
		}
	}
	h := newHarness(t)
	admin := h.admin()
	home := t.TempDir()
	srv := h.sshServer(home)
	srv.HandleExec(testutil.ShellExec(home, func(cmd string) bool { return strings.Contains(cmd, ".claude") }))
	hostID := h.hostFor(admin, srv.Addr.Port)
	u := fmt.Sprintf("/api/hosts/%d/ask-hook", hostID)
	mustStatus(t, admin.do("POST", "/api/sessions", map[string]any{"host_id": hostID}), 201, "open session")
	admin.do("GET", u, nil)
	mustStatus(t, admin.do("POST", u, map[string]any{"on": true, "seen": "none"}), 200, "made")
	f := filepath.Join(home, ".claude", "settings.json")
	os.WriteFile(f, []byte("{}\n"), 0o600) // (the person's own, in its place)
	r := admin.do("GET", u+"?fresh=1", nil)
	r = admin.do("POST", u, map[string]any{"on": true, "seen": r.json()["seen"], "hook": "bell"})
	mustStatus(t, r, 200, "bell on")
	mustStatus(t, admin.do("POST", u, map[string]any{"on": false, "seen": r.json()["seen"], "hook": "bell"}), 200, "bell off")
	if b, err := os.ReadFile(f); err != nil || strings.TrimSpace(string(b)) != "{}" {
		t.Fatalf("the person's file: %q %v", b, err)
	}
}
