package web

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"golang.org/x/crypto/ssh"

	"github.com/ihainan/corral/internal/testutil"
)

// fakeTmuxRenameHost creates an SSH test host with a stateful fake `tmux` binary
// supporting list-sessions, has-session, rename-session, and new-session.
func fakeTmuxRenameHost(t *testing.T, h *harness, admin *client, initial []string) (float64, string) {
	t.Helper()
	home := t.TempDir()
	stateFile := filepath.Join(home, "tmux_sessions.txt")
	var stateLines []string
	for idx, n := range initial {
		stateLines = append(stateLines, "$"+jsonNumber(float64(idx))+"\t"+n)
	}
	if err := os.WriteFile(stateFile, []byte(strings.Join(stateLines, "\n")+"\n"), 0o600); err != nil {
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
resolve_name() {
  want="${1#=}"
  want="${want%:*}"
  while IFS='	' read -r sid sname; do
    if [ "$sid" = "$want" ] || [ "$sname" = "$want" ]; then
      printf '%s\n' "$sname"
      return 0
    fi
  done < "$STATE"
  return 1
}
case "$cmd" in
  list-sessions)
    while IFS='	' read -r sid n; do
      [ -n "$n" ] && printf '%s|1|1\n' "$n"
    done < "$STATE"
    exit 0
    ;;
  display-message)
    target=""
    fmt=""
    while [ $# -gt 0 ]; do
      case "$1" in
        -p) shift;;
        -t) target="$2"; shift 2;;
        *) fmt="$1"; shift;;
      esac
    done
    want="${target#=}"
    want="${want%:*}"
    if [ "$fmt" = '#{session_id}' ]; then
      while IFS='	' read -r sid sname; do
        if [ "$sid" = "$want" ] || [ "$sname" = "$want" ]; then
          printf '%s\n' "$sid"
          exit 0
        fi
      done < "$STATE"
      exit 1
    fi
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
    resolve_name "$target" >/dev/null && exit 0
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
    old_name=$(resolve_name "$old") || { echo "can't find session: $old" >&2; exit 1; }
    while IFS='	' read -r sid n; do
      [ "$n" = "$new" ] && { echo "duplicate session: $new" >&2; exit 1; }
    done < "$STATE"
    tmp="$STATE.tmp"
    : > "$tmp"
    while IFS='	' read -r sid n; do
      if [ "$n" = "$old_name" ]; then
        printf '%s\t%s\n' "$sid" "$new" >> "$tmp"
      elif [ -n "$n" ]; then
        printf '%s\t%s\n' "$sid" "$n" >> "$tmp"
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

func fakeTmuxRenameServer(t *testing.T, h *harness, initial []string) (int, string) {
	t.Helper()
	home := t.TempDir()
	stateFile := filepath.Join(home, "tmux_sessions.txt")
	var stateLines []string
	for idx, n := range initial {
		stateLines = append(stateLines, "$"+jsonNumber(float64(idx))+"\t"+n)
	}
	if err := os.WriteFile(stateFile, []byte(strings.Join(stateLines, "\n")+"\n"), 0o600); err != nil {
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
resolve_name() {
  want="${1#=}"
  want="${want%:*}"
  while IFS='	' read -r sid sname; do
    if [ "$sid" = "$want" ] || [ "$sname" = "$want" ]; then
      printf '%s\n' "$sname"
      return 0
    fi
  done < "$STATE"
  return 1
}
case "$cmd" in
  list-sessions)
    while IFS='	' read -r sid n; do
      [ -n "$n" ] && printf '%s|1|1\n' "$n"
    done < "$STATE"
    exit 0
    ;;
  display-message)
    target=""
    fmt=""
    while [ $# -gt 0 ]; do
      case "$1" in
        -p) shift;;
        -t) target="$2"; shift 2;;
        *) fmt="$1"; shift;;
      esac
    done
    want="${target#=}"
    want="${want%:*}"
    if [ "$fmt" = '#{session_id}' ]; then
      while IFS='	' read -r sid sname; do
        if [ "$sid" = "$want" ] || [ "$sname" = "$want" ]; then
          printf '%s\n' "$sid"
          exit 0
        fi
      done < "$STATE"
      exit 1
    fi
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
    resolve_name "$target" >/dev/null && exit 0
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
    old_name=$(resolve_name "$old") || { echo "can't find session: $old" >&2; exit 1; }
    while IFS='	' read -r sid n; do
      [ "$n" = "$new" ] && { echo "duplicate session: $new" >&2; exit 1; }
    done < "$STATE"
    tmp="$STATE.tmp"
    : > "$tmp"
    while IFS='	' read -r sid n; do
      if [ "$n" = "$old_name" ]; then
        printf '%s\t%s\n' "$sid" "$new" >> "$tmp"
      elif [ -n "$n" ]; then
        printf '%s\t%s\n' "$sid" "$n" >> "$tmp"
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
	return srv.Addr.Port, stateFile
}

func TestRenameIsolationAcrossHostConfigs(t *testing.T) {
	h := newHarness(t)
	admin := h.admin()
	me := admin.do("GET", "/api/me", nil)
	mustStatus(t, me, 200, "me")
	uid := int64(me.json()["id"].(float64))

	// --- 1. tmux: sessions opened under different HostConfig on the same host ID ---
	port1, _ := fakeTmuxRenameServer(t, h, []string{"work", "taken-on-old"})
	port2, _ := fakeTmuxRenameServer(t, h, []string{"work"})

	r := admin.do("POST", "/api/hosts", map[string]any{
		"name": "box", "address": "127.0.0.1", "port": port1,
		"username": "alice", "auth": "password", "password": "alice-pass",
	})
	mustStatus(t, r, 201, "create tmux host")
	hostID := r.json()["id"].(float64)

	r = admin.do("POST", "/api/sessions", map[string]any{"host_id": hostID, "open": map[string]any{"kind": "tmux", "name": "work"}})
	mustStatus(t, r, 201, "open old work")
	oldWorkSID := r.json()["id"].(string)

	r = admin.do("POST", "/api/sessions", map[string]any{"host_id": hostID, "open": map[string]any{"kind": "tmux", "name": "taken-on-old"}})
	mustStatus(t, r, 201, "open old taken")
	oldTakenSID := r.json()["id"].(string)

	// Re-point the same host record to port2 (different machine -> different HostConfig).
	r = admin.do("PUT", "/api/hosts/"+jsonNumber(hostID), map[string]any{
		"name": "box", "address": "127.0.0.1", "port": port2,
		"username": "alice", "auth": "password", "password": "alice-pass",
	})
	mustStatus(t, r, 200, "update host to port2")

	r = admin.do("POST", "/api/sessions", map[string]any{"host_id": hostID, "open": map[string]any{"kind": "tmux", "name": "work"}})
	mustStatus(t, r, 201, "open new work")
	newWorkSID := r.json()["id"].(string)
	if newWorkSID == oldWorkSID {
		t.Fatalf("expected distinct session under new HostConfig")
	}

	// Renaming newWorkSID to "taken-on-old" MUST succeed (oldTakenSID has a different HostConfig),
	// and MUST NOT rename oldWorkSID (which also had name "work" on the old HostConfig).
	r = admin.do("PUT", "/api/sessions/"+newWorkSID+"/name", map[string]any{"name": "taken-on-old"})
	mustStatus(t, r, 200, "rename new work to name taken on old HostConfig")
	if r.json()["mux_session"] != "taken-on-old" {
		t.Fatalf("expected new session renamed, got %s", r.Body)
	}

	oldSess, err := h.srv.terms.Get(uid, oldWorkSID)
	if err != nil {
		t.Fatal(err)
	}
	if oldSess.CurrentTmuxSession() != "work" {
		t.Fatalf("old HostConfig session was wrongly renamed to %q", oldSess.CurrentTmuxSession())
	}
	oldTab, err := h.store.OpenTabByID(context.Background(), uid, oldWorkSID)
	if err != nil || oldTab.Name != "work" {
		t.Fatalf("old HostConfig open_tab wrongly updated: %+v err=%v", oldTab, err)
	}
	oldTaken, err := h.srv.terms.Get(uid, oldTakenSID)
	if err != nil || oldTaken.CurrentTmuxSession() != "taken-on-old" {
		t.Fatalf("old taken session changed: %+v err=%v", oldTaken, err)
	}
	newTab, err := h.store.OpenTabByID(context.Background(), uid, newWorkSID)
	if err != nil || newTab.Name != "taken-on-old" {
		t.Fatalf("new HostConfig open_tab not updated: %+v err=%v", newTab, err)
	}

	// --- 2. herdr: display names and sync are also isolated by HostConfig ---
	herdrJSON := `{"sessions":[{"name":"default","default":true,"running":true},{"name":"dev","default":false,"running":true}]}`
	srvH1, herdrHostID := pickerHost(t, h, admin, false, "password", herdrJSON)
	_ = srvH1
	// Build a second herdr SSH server on another port.
	srvH2, _ := pickerHost(t, h, admin, false, "password", herdrJSON)

	r = admin.do("POST", "/api/sessions", map[string]any{"host_id": herdrHostID, "open": map[string]any{"kind": "herdr", "name": ""}})
	mustStatus(t, r, 201, "open herdr default on cfg1")
	hOldSID := r.json()["id"].(string)

	r = admin.do("PUT", "/api/sessions/"+hOldSID+"/name", map[string]any{"name": "旧机器服务"})
	mustStatus(t, r, 200, "rename herdr on cfg1")

	// Update host to point to srvH2 (new HostConfig).
	r = admin.do("PUT", "/api/hosts/"+jsonNumber(herdrHostID), map[string]any{
		"name": "h-box", "address": "127.0.0.1", "port": srvH2.Addr.Port,
		"username": "alice", "auth": "password", "password": "alice-pass",
	})
	mustStatus(t, r, 200, "update herdr host to srvH2")

	// Probing the updated host must NOT attach cfg1's display name to cfg2's choose.herdr_sessions.
	r = admin.do("POST", "/api/sessions", map[string]any{"host_id": herdrHostID, "open": nil})
	mustStatus(t, r, 200, "probe herdr on cfg2")
	var probeDoc struct {
		Choose struct {
			HerdrSessions []struct {
				Name        string `json:"name"`
				DisplayName string `json:"display_name"`
			} `json:"herdr_sessions"`
		} `json:"choose"`
	}
	if err := json.Unmarshal(r.Body, &probeDoc); err != nil {
		t.Fatal(err)
	}
	for _, hs := range probeDoc.Choose.HerdrSessions {
		if hs.DisplayName != "" {
			t.Fatalf("cfg1 display_name leaked into cfg2 choose.herdr_sessions: %+v", probeDoc.Choose.HerdrSessions)
		}
	}

	// Opening default herdr on cfg2 starts with empty display_name and can reuse "旧机器服务".
	r = admin.do("POST", "/api/sessions", map[string]any{"host_id": herdrHostID, "open": map[string]any{"kind": "herdr", "name": ""}})
	mustStatus(t, r, 201, "open herdr default on cfg2")
	hNewSID := r.json()["id"].(string)
	if dn, _ := r.json()["display_name"].(string); dn != "" {
		t.Fatalf("expected empty display_name on new HostConfig, got %q", dn)
	}
	r = admin.do("PUT", "/api/sessions/"+hNewSID+"/name", map[string]any{"name": "旧机器服务"})
	mustStatus(t, r, 200, "reuse name on new HostConfig")
	r = admin.do("PUT", "/api/sessions/"+hNewSID+"/name", map[string]any{"name": "新机器服务"})
	mustStatus(t, r, 200, "rename cfg2 herdr")

	// cfg1 session display name must remain untouched.
	hOldSess, err := h.srv.terms.Get(uid, hOldSID)
	if err != nil {
		t.Fatal(err)
	}
	if hOldSess.CurrentDisplayName() != "旧机器服务" {
		t.Fatalf("cfg1 herdr session display name was clobbered: %q", hOldSess.CurrentDisplayName())
	}
}

func jsonNumber(f float64) string {
	b, _ := json.Marshal(int64(f))
	return string(b)
}

func TestConcurrentHerdrRenamePreventsDuplicate(t *testing.T) {
	h := newHarness(t)
	admin := h.admin()
	_, hostID := pickerHost(t, h, admin, false, "password",
		`{"sessions":[{"name":"default","default":true,"running":true},{"name":"dev","default":false,"running":true}]}`)

	r := admin.do("POST", "/api/sessions", map[string]any{"host_id": hostID, "open": map[string]any{"kind": "herdr", "name": ""}})
	mustStatus(t, r, 201, "open default herdr")
	sid1 := r.json()["id"].(string)

	r = admin.do("POST", "/api/sessions", map[string]any{"host_id": hostID, "open": map[string]any{"kind": "herdr", "name": "dev"}})
	mustStatus(t, r, 201, "open dev herdr")
	sid2 := r.json()["id"].(string)

	me := admin.do("GET", "/api/me", nil)
	mustStatus(t, me, 200, "me")
	uid := int64(me.json()["id"].(float64))
	s1, err := h.srv.terms.Get(uid, sid1)
	if err != nil {
		t.Fatal(err)
	}

	for round := 0; round < 10; round++ {
		target := "并发争抢-" + jsonNumber(float64(round))
		start := make(chan struct{})
		results := make([]resp, 2)
		var wg sync.WaitGroup
		wg.Add(2)
		for idx, sid := range []string{sid1, sid2} {
			go func(i int, id string) {
				defer wg.Done()
				<-start
				results[i] = admin.do("PUT", "/api/sessions/"+id+"/name", map[string]any{"name": target})
			}(idx, sid)
		}
		close(start)
		wg.Wait()

		okCount, conflictCount := 0, 0
		for _, res := range results {
			switch res.Status {
			case 200:
				okCount++
			case 409:
				if res.code() != "session_name_taken" {
					t.Fatalf("round %d: unexpected 409 code %q: %s", round, res.code(), res.Body)
				}
				conflictCount++
			default:
				t.Fatalf("round %d: unexpected status %d: %s", round, res.Status, res.Body)
			}
		}
		if okCount != 1 || conflictCount != 1 {
			t.Fatalf("round %d: expected 1 ok and 1 conflict, got ok=%d conflict=%d", round, okCount, conflictCount)
		}

		aliases, err := h.store.ListSessionDisplayNames(context.Background(), uid, int64(hostID), s1.HostConfig, "herdr")
		if err != nil {
			t.Fatal(err)
		}
		holders := 0
		for _, v := range aliases {
			if v == target {
				holders++
			}
		}
		if holders != 1 {
			t.Fatalf("round %d: expected exactly 1 holder of %q in session_names, got %+v", round, target, aliases)
		}
	}
}

func TestRenameTmuxConcurrentFirstBindConflict(t *testing.T) {
	h := newHarness(t)
	admin := h.admin()

	home := t.TempDir()
	srv := h.sshServer(home)

	resolveStarted := make(chan struct{})
	resolveRelease := make(chan struct{})
	var renameMu sync.Mutex
	renameCalls := 0

	srv.HandleExec(func(cmd string) func(ssh.Channel) {
		if strings.Contains(cmd, "new-session") || strings.Contains(cmd, "attach-session") {
			return func(ch ssh.Channel) {
				buf := make([]byte, 16)
				_, _ = ch.Read(buf)
				ch.SendRequest("exit-status", false, []byte{0, 0, 0, 0})
			}
		}
		if strings.Contains(cmd, "rename-session") {
			return func(ch ssh.Channel) {
				renameMu.Lock()
				renameCalls++
				renameMu.Unlock()
				_, _ = ch.Write([]byte("$2\n"))
				ch.CloseWrite()
				ch.SendRequest("exit-status", false, []byte{0, 0, 0, 0})
			}
		}
		if strings.Contains(cmd, "#{session_id}") {
			return func(ch ssh.Channel) {
				close(resolveStarted)
				<-resolveRelease
				// Simulate read-only Step 1 resolving replacement session $2.
				_, _ = ch.Write([]byte("$2\n"))
				ch.CloseWrite()
				ch.SendRequest("exit-status", false, []byte{0, 0, 0, 0})
			}
		}
		return nil
	})

	rHost := admin.do("POST", "/api/hosts", map[string]any{
		"name": "rename-race-box", "address": "127.0.0.1", "port": srv.Addr.Port,
		"username": "alice", "auth": "password", "password": "alice-pass",
	})
	mustStatus(t, rHost, http.StatusCreated, "create host")
	hostID := rHost.json()["id"].(float64)

	rSess := admin.do("POST", "/api/sessions", map[string]any{
		"host_id": hostID,
		"open":    map[string]any{"kind": "tmux", "name": "work"},
	})
	mustStatus(t, rSess, http.StatusCreated, "open tmux work")
	sid := rSess.json()["id"].(string)

	me := admin.do("GET", "/api/me", nil)
	mustStatus(t, me, http.StatusOK, "me")
	uid := int64(me.json()["id"].(float64))
	sess, err := h.srv.terms.Get(uid, sid)
	if err != nil {
		t.Fatal(err)
	}
	if got := sess.CurrentTmuxSessionID(); got != "" {
		t.Fatalf("expected unbound session before rename, got %q", got)
	}

	// Start rename while boundID is empty; while read-only Step 1 is in flight,
	// bind session to $0; then let Step 1 report $2. Step 2 (rename-session) must NEVER run.
	done := make(chan resp, 1)
	go func() {
		done <- admin.do("PUT", "/api/sessions/"+sid+"/name", map[string]any{"name": "backend-new"})
	}()
	<-resolveStarted
	if !sess.SetTmuxSessionID("$0") {
		t.Fatal("expected concurrent bind to $0")
	}
	close(resolveRelease)

	rRename := <-done
	mustStatus(t, rRename, http.StatusConflict, "rename conflict with concurrent bind")
	if rRename.code() != "session_mismatch" {
		t.Fatalf("expected session_mismatch, got %s", rRename.Body)
	}
	renameMu.Lock()
	gotRenameCalls := renameCalls
	renameMu.Unlock()
	if gotRenameCalls != 0 {
		t.Fatalf("remote rename-session was executed %d times despite Step 1 bind conflict, want 0", gotRenameCalls)
	}
	if got := sess.CurrentTmuxSession(); got != "work" {
		t.Fatalf("local session name changed after conflict: got %q, want work", got)
	}
	if got := sess.CurrentTmuxSessionID(); got != "$0" {
		t.Fatalf("bound session id changed after conflict: got %q, want $0", got)
	}
	tab, err := h.store.OpenTabByID(context.Background(), uid, sid)
	if err != nil || tab.Name != "work" {
		t.Fatalf("open_tab wrongly updated after conflict: %+v err=%v", tab, err)
	}
}
