package web

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ihainan/corral/internal/testutil"
)

// fakeTmuxRenameHost creates an SSH test host with a stateful fake `tmux` binary
// supporting list-sessions, has-session, rename-session, and new-session.
func fakeTmuxRenameHost(t *testing.T, h *harness, admin *client, initial []string) (float64, string) {
	t.Helper()
	home := t.TempDir()
	stateFile := filepath.Join(home, "tmux_sessions.txt")
	if err := os.WriteFile(stateFile, []byte(strings.Join(initial, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(home, ".local", "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	script := `#!/bin/sh
STATE="` + stateFile + `"
if [ "$1" = "-u" ]; then shift; fi
cmd="$1"; shift
case "$cmd" in
  list-sessions)
    while IFS= read -r n; do
      [ -n "$n" ] && printf '%s|1|1\n' "$n"
    done < "$STATE"
    exit 0
    ;;
  has-session)
    target=""
    while [ $# -gt 0 ]; do
      case "$1" in
        -t) target="$2"; shift 2;;
        *) shift;;
      esac
    done
    target="${target#=}"
    while IFS= read -r n; do
      [ "$n" = "$target" ] && exit 0
    done < "$STATE"
    echo "can't find session: $target" >&2
    exit 1
    ;;
  rename-session)
    old=""
    while [ $# -gt 0 ]; do
      case "$1" in
        -t) old="$2"; shift 2;;
        --) shift; break;;
        *) break;;
      esac
    done
    new="$1"
    old="${old#=}"
    found=0
    while IFS= read -r n; do
      [ "$n" = "$new" ] && { echo "duplicate session: $new" >&2; exit 1; }
      [ "$n" = "$old" ] && found=1
    done < "$STATE"
    [ "$found" -eq 0 ] && { echo "can't find session: $old" >&2; exit 1; }
    tmp="$STATE.tmp"
    : > "$tmp"
    while IFS= read -r n; do
      if [ "$n" = "$old" ]; then
        printf '%s\n' "$new" >> "$tmp"
      elif [ -n "$n" ]; then
        printf '%s\n' "$n" >> "$tmp"
      fi
    done < "$STATE"
    mv "$tmp" "$STATE"
    exit 0
    ;;
  new-session|attach-session)
    read x
    exit 0
    ;;
esac
exit 0
`
	if err := os.WriteFile(filepath.Join(bin, "tmux"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	sys := filepath.Join(t.TempDir(), "sys")
	if err := os.MkdirAll(sys, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, tool := range []string{"sh", "mv"} {
		if p, err := exec.LookPath(tool); err == nil {
			_ = os.Symlink(p, filepath.Join(sys, tool))
		}
	}
	srv := h.sshServer(home)
	srv.HandleExec(testutil.ShellExec(home, func(cmd string) bool {
		return strings.Contains(cmd, "tmux") || strings.Contains(cmd, "herdr")
	}, "PATH="+sys))

	r := admin.do("POST", "/api/hosts", map[string]any{
		"name": "devbox", "address": "127.0.0.1", "port": srv.Addr.Port,
		"username": "alice", "auth": "password", "password": "alice-pass",
	})
	mustStatus(t, r, 201, "create host")
	return r.json()["id"].(float64), stateFile
}

func TestRenameTmuxSession(t *testing.T) {
	h := newHarness(t)
	admin := h.admin()
	bob := h.member(admin, "bob")
	hostID, stateFile := fakeTmuxRenameHost(t, h, admin, []string{"work", "other"})

	// Open the existing tmux session "work".
	r := admin.do("POST", "/api/sessions", map[string]any{
		"host_id": hostID,
		"open":    map[string]any{"kind": "tmux", "name": "work"},
	})
	mustStatus(t, r, 201, "open work")
	sid := r.json()["id"].(string)

	// Another user cannot rename admin's session.
	mustStatus(t, bob.do("PUT", "/api/sessions/"+sid+"/name", map[string]any{"name": "hijack"}), 404, "other user forbidden")

	// Unauthenticated request is rejected.
	anon := &client{h: h}
	mustStatus(t, anon.do("PUT", "/api/sessions/"+sid+"/name", map[string]any{"name": "anon"}), 401, "unauthenticated")

	// Invalid tmux names ('.', ':', spaces, empty, >64) rejected with 400.
	for _, bad := range []string{"", "a.b", "a:b", "two words", strings.Repeat("x", 65)} {
		r = admin.do("PUT", "/api/sessions/"+sid+"/name", map[string]any{"name": bad})
		if r.Status != 400 || r.code() != "invalid_session_name" {
			t.Fatalf("bad name %q: %d %s", bad, r.Status, r.Body)
		}
	}

	// Duplicate with existing remote tmux session "other" rejected with 409.
	r = admin.do("PUT", "/api/sessions/"+sid+"/name", map[string]any{"name": "other"})
	if r.Status != 409 || r.code() != "session_name_taken" {
		t.Fatalf("duplicate rename: %d %s", r.Status, r.Body)
	}

	// Valid rename succeeds, updates remote tmux, live session info, open_tabs, and audit log.
	r = admin.do("PUT", "/api/sessions/"+sid+"/name", map[string]any{"name": "backend-1"})
	mustStatus(t, r, 200, "rename tmux")
	if r.json()["mux_session"] != "backend-1" {
		t.Fatalf("renamed response: %s", r.Body)
	}

	// Remote state file reflects the actual tmux rename.
	rawState, err := os.ReadFile(stateFile)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(rawState), "backend-1\n") || strings.Contains(string(rawState), "work\n") {
		t.Fatalf("remote tmux state not updated: %q", string(rawState))
	}

	// GET /api/sessions returns the updated name.
	r = admin.do("GET", "/api/sessions", nil)
	mustStatus(t, r, 200, "list sessions")
	var listed []map[string]any
	if err := json.Unmarshal(r.Body, &listed); err != nil {
		t.Fatalf("decode sessions: %v %s", err, r.Body)
	}
	if len(listed) != 1 || listed[0]["mux_session"] != "backend-1" {
		t.Fatalf("listed sessions: %s", r.Body)
	}

	// open_tabs in DB updated so gateway restart restores the new tmux session name.
	me := admin.do("GET", "/api/me", nil)
	mustStatus(t, me, 200, "me")
	uid := int64(me.json()["id"].(float64))
	tab, err := h.store.OpenTabByID(context.Background(), uid, sid)
	if err != nil || tab.Name != "backend-1" {
		t.Fatalf("open_tab after rename: %+v err=%v", tab, err)
	}

	// Re-opening "backend-1" reuses the existing renamed session.
	r = admin.do("POST", "/api/sessions", map[string]any{
		"host_id": hostID,
		"open":    map[string]any{"kind": "tmux", "name": "backend-1"},
	})
	mustStatus(t, r, 200, "reopen renamed")
	if r.json()["id"] != sid || r.json()["reused"] != true {
		t.Fatalf("expected reuse of renamed session: %s", r.Body)
	}

	// Audit log contains session.rename entry.
	r = admin.do("GET", "/api/admin/audit?action=session.rename", nil)
	mustStatus(t, r, 200, "audit")
	if !strings.Contains(string(r.Body), `"session.rename"`) || !strings.Contains(string(r.Body), `"backend-1"`) || !strings.Contains(string(r.Body), `"remote":true`) {
		t.Fatalf("expected session.rename audit entry: %s", r.Body)
	}
}

func TestRenameHerdrSessionPersistsAcrossReconnect(t *testing.T) {
	h := newHarness(t)
	admin := h.admin()
	_, hostID := pickerHost(t, h, admin, false, "password",
		`{"sessions":[{"name":"default","default":true,"running":true},{"name":"dev","default":false,"running":true},{"name":"ops","default":false,"running":false}]}`)

	// Open default and dev herdr sessions.
	r := admin.do("POST", "/api/sessions", map[string]any{"host_id": hostID, "open": map[string]any{"kind": "herdr", "name": ""}})
	mustStatus(t, r, 201, "open default herdr")
	sidDefault := r.json()["id"].(string)

	r = admin.do("POST", "/api/sessions", map[string]any{"host_id": hostID, "open": map[string]any{"kind": "herdr", "name": "dev"}})
	mustStatus(t, r, 201, "open dev herdr")
	sidDev := r.json()["id"].(string)

	// Rename default herdr session display name (herdr has no remote rename RPC).
	r = admin.do("PUT", "/api/sessions/"+sidDefault+"/name", map[string]any{"name": "核心服务"})
	mustStatus(t, r, 200, "rename herdr")
	if r.json()["display_name"] != "核心服务" || r.json()["mux_session"] != "" {
		t.Fatalf("renamed herdr: %s", r.Body)
	}

	// Renaming dev to the same display name is rejected.
	r = admin.do("PUT", "/api/sessions/"+sidDev+"/name", map[string]any{"name": "核心服务"})
	if r.Status != 409 || r.code() != "session_name_taken" {
		t.Fatalf("duplicate herdr display name: %d %s", r.Status, r.Body)
	}

	// Renaming dev to collide with unopened remote herdr session "ops" is also rejected.
	r = admin.do("PUT", "/api/sessions/"+sidDev+"/name", map[string]any{"name": "ops"})
	if r.Status != 409 || r.code() != "session_name_taken" {
		t.Fatalf("collision with remote herdr session: %d %s", r.Status, r.Body)
	}

	// Close the default session, probe the host, and reconnect: display_name persists!
	mustStatus(t, admin.do("DELETE", "/api/sessions/"+sidDefault, nil), 200, "close default")
	r = admin.do("POST", "/api/sessions", map[string]any{"host_id": hostID, "open": nil})
	mustStatus(t, r, 200, "probe after rename")
	if !strings.Contains(string(r.Body), `"display_name":"核心服务"`) {
		t.Fatalf("probe should include persisted display_name: %s", r.Body)
	}

	r = admin.do("POST", "/api/sessions", map[string]any{"host_id": hostID, "open": map[string]any{"kind": "herdr", "name": ""}})
	mustStatus(t, r, 201, "reconnect default herdr")
	if r.json()["display_name"] != "核心服务" {
		t.Fatalf("reconnected herdr lost display_name: %s", r.Body)
	}

	// Audit log contains session.rename with remote:false.
	r = admin.do("GET", "/api/admin/audit?action=session.rename", nil)
	mustStatus(t, r, 200, "audit")
	if !strings.Contains(string(r.Body), `"remote":false`) || !strings.Contains(string(r.Body), `"核心服务"`) {
		t.Fatalf("expected herdr session.rename audit entry: %s", r.Body)
	}
}

func TestRenamePlainShellSession(t *testing.T) {
	h := newHarness(t)
	admin := h.admin()
	_, hostID := pickerHost(t, h, admin, false, "password")

	r := admin.do("POST", "/api/sessions", map[string]any{"host_id": hostID, "open": map[string]any{"kind": "shell"}})
	mustStatus(t, r, 201, "open shell")
	sid := r.json()["id"].(string)

	r = admin.do("PUT", "/api/sessions/"+sid+"/name", map[string]any{"name": "临时排查"})
	mustStatus(t, r, 200, "rename shell")
	if r.json()["display_name"] != "临时排查" {
		t.Fatalf("renamed shell: %s", r.Body)
	}
}
