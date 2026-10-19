package web

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ihainan/corral/internal/config"
	"github.com/ihainan/corral/internal/testutil"
)

// pickerHost: a host whose tmux (a stand-in under ~/.local/bin, nothing else
// on PATH) has the sessions "work" (2 windows, attached) and "other".
func pickerHost(t *testing.T, h *harness, admin *client, withTmux bool, auth string) (*testutil.SSHServer, float64) {
	t.Helper()
	home := t.TempDir()
	srv := h.sshServer(home)
	if withTmux {
		bin := filepath.Join(home, ".local", "bin")
		os.MkdirAll(bin, 0o755)
		os.WriteFile(filepath.Join(bin, "tmux"), []byte("#!/bin/sh\nprintf 'work|2|1\\nother|1|0\\nbad.name|1|0\\n'\n"), 0o755)
	}
	// Only sh on PATH: whatever tmux or herdr this machine has is not seen.
	sys := filepath.Join(t.TempDir(), "sys")
	os.MkdirAll(sys, 0o755)
	if sh, err := exec.LookPath("sh"); err == nil {
		os.Symlink(sh, filepath.Join(sys, "sh"))
	}
	srv.HandleExec(testutil.ShellExec(home, func(cmd string) bool {
		return strings.Contains(cmd, "list-sessions") || strings.Contains(cmd, "herdr")
	}, "PATH="+sys))
	body := map[string]any{"name": "box", "address": "127.0.0.1", "port": srv.Addr.Port, "username": "alice", "auth": auth}
	if auth == "password" {
		body["password"] = "alice-pass"
	}
	r := admin.do("POST", "/api/hosts", body)
	mustStatus(t, r, 201, "host")
	return srv, r.json()["id"].(float64)
}

func TestPickerListsAndOpensOverTheSameConnection(t *testing.T) {
	h := newHarness(t)
	admin := h.admin()
	srv, id := pickerHost(t, h, admin, true, "password")

	r := admin.do("POST", "/api/sessions", map[string]any{"host_id": id, "open": nil})
	mustStatus(t, r, 200, "asked")
	choose, _ := r.json()["choose"].(map[string]any)
	list, _ := choose["tmux_sessions"].([]any)
	if choose["tmux"] != true || choose["herdr"] != false || len(list) != 2 {
		t.Fatalf("choices: %s", r.Body)
	}
	if first := list[0].(map[string]any); first["name"] != "work" || first["windows"] != float64(2) || first["attached"] != true {
		t.Fatalf("first tmux session: %v", first)
	}
	pending, _ := r.json()["pending"].(string)
	attempts := srv.Attempts()

	// The pick uses the connection kept for it: no new sign-in on the host.
	r = admin.do("POST", "/api/sessions", map[string]any{"host_id": id, "pending": pending, "open": map[string]any{"kind": "tmux", "name": "work"}})
	mustStatus(t, r, 201, "open work")
	if srv.Attempts() != attempts {
		t.Fatal("connected again for the pick")
	}
	if r.json()["persistence"] != "tmux" || r.json()["mux_session"] != "work" {
		t.Fatalf("opened: %s", r.Body)
	}
	first := r.json()["id"]

	// The same tmux session again: the terminal showing it.
	r = admin.do("POST", "/api/sessions", map[string]any{"host_id": id, "open": map[string]any{"kind": "tmux", "name": "work"}})
	mustStatus(t, r, 200, "work again")
	if r.json()["id"] != first || r.json()["reused"] != true {
		t.Fatalf("not reused: %s", r.Body)
	}
	// The running terminal is listed with the choices.
	r = admin.do("POST", "/api/sessions", map[string]any{"host_id": id, "open": nil})
	running, _ := r.json()["running"].([]any)
	if len(running) != 1 || running[0].(map[string]any)["name"] != "work" {
		t.Fatalf("running: %s", r.Body)
	}

	// A new tmux session gets a name not taken.
	r = admin.do("POST", "/api/sessions", map[string]any{"host_id": id, "open": map[string]any{"kind": "tmux_new"}})
	mustStatus(t, r, 201, "new tmux session")
	if n, _ := r.json()["mux_session"].(string); !strings.HasPrefix(n, "corral-") {
		t.Fatalf("new session name: %s", r.Body)
	}
	// A plain shell.
	r = admin.do("POST", "/api/sessions", map[string]any{"host_id": id, "open": map[string]any{"kind": "shell"}})
	mustStatus(t, r, 201, "shell")
	if r.json()["persistence"] != "none" {
		t.Fatalf("shell: %s", r.Body)
	}
	// What cannot be opened.
	for _, o := range []map[string]any{{"kind": "tmux", "name": "bad.name"}, {"kind": "tmux"}, {"kind": "sh"}, {"kind": "shell", "name": "x"}} {
		mustStatus(t, admin.do("POST", "/api/sessions", map[string]any{"host_id": id, "open": o}), 400, "bad choice")
	}
}

func TestPickerSkippedWithoutTmuxOrHerdr(t *testing.T) {
	h := newHarness(t)
	admin := h.admin()
	_, id := pickerHost(t, h, admin, false, "password")
	r := admin.do("POST", "/api/sessions", map[string]any{"host_id": id, "open": nil})
	mustStatus(t, r, 201, "plain shell at once")
	if r.json()["persistence"] != "none" {
		t.Fatalf("opened: %s", r.Body)
	}
}

// A host that asks for its password: asked once, for listing; the pick needs
// none. A token of someone else's, or an expired one, does not help.
func TestPickerAsksThePasswordOnce(t *testing.T) {
	h := newHarness(t)
	admin := h.admin()
	srv, id := pickerHost(t, h, admin, true, "ask")
	mustStatus(t, admin.do("POST", "/api/sessions", map[string]any{"host_id": id, "open": nil}), 409, "password needed to list")
	r := admin.do("POST", "/api/sessions", map[string]any{"host_id": id, "open": nil, "password": "alice-pass"})
	mustStatus(t, r, 200, "asked")
	pending, _ := r.json()["pending"].(string)

	bob := h.member(admin, "bob")
	r = bob.do("POST", "/api/sessions", map[string]any{"host_id": id, "pending": pending, "open": map[string]any{"kind": "shell"}})
	mustStatus(t, r, 404, "someone else's host and token")

	attempts := srv.Attempts()
	mustStatus(t, admin.do("POST", "/api/sessions", map[string]any{"host_id": id, "pending": pending, "open": map[string]any{"kind": "shell"}}), 201, "pick")
	if srv.Attempts() != attempts {
		t.Fatal("connected again for the pick")
	}
	// The token is used up: the password is asked again.
	r = admin.do("POST", "/api/sessions", map[string]any{"host_id": id, "pending": pending, "open": map[string]any{"kind": "shell"}})
	if r.Status != 409 || r.code() != "password_required" {
		t.Fatalf("used token: %d %s", r.Status, r.Body)
	}
}

// At the limit of terminals, the host can still be asked (to go back to a
// running terminal); a new one is refused only when started. Signing out
// closes the connection kept for a pick.
func TestPickerAtTheLimitAndSigningOut(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.MaxSessionsPerUser = 1 })
	admin := h.admin()
	_, id := pickerHost(t, h, admin, true, "password")
	mustStatus(t, admin.do("POST", "/api/sessions", map[string]any{"host_id": id, "open": map[string]any{"kind": "tmux", "name": "work"}}), 201, "first terminal")

	r := admin.do("POST", "/api/sessions", map[string]any{"host_id": id, "open": nil})
	mustStatus(t, r, 200, "asked at the limit")
	pending, _ := r.json()["pending"].(string)
	r = admin.do("POST", "/api/sessions", map[string]any{"host_id": id, "pending": pending, "open": map[string]any{"kind": "tmux", "name": "work"}})
	mustStatus(t, r, 200, "back to the running terminal")
	mustStatus(t, admin.do("POST", "/api/sessions", map[string]any{"host_id": id, "open": map[string]any{"kind": "shell"}}), 429, "a second terminal")

	mustStatus(t, admin.do("POST", "/api/sessions", map[string]any{"host_id": id, "open": nil}), 200, "asked again")
	mustStatus(t, admin.do("POST", "/api/logout", nil), 200, "sign out")
	h.srv.picks.mu.Lock()
	kept := len(h.srv.picks.m)
	h.srv.picks.mu.Unlock()
	if kept != 0 {
		t.Fatalf("%d connections kept after signing out", kept)
	}
}

func TestSameMux(t *testing.T) {
	for _, c := range []struct {
		kind, a, b string
		want       bool
	}{
		{"herdr", "", "default", true}, {"herdr", "default", "", true}, {"herdr", "", "", true},
		{"herdr", "other", "", false}, {"tmux", "", "default", false}, {"tmux", "work", "work", true},
	} {
		if got := sameMux(c.kind, c.a, c.b); got != c.want {
			t.Errorf("sameMux(%q, %q, %q) = %v", c.kind, c.a, c.b, got)
		}
	}
}
