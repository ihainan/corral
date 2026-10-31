package web

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/ihainan/corral/internal/store"
)

func (h *harness) tabs() []*store.OpenTab {
	h.t.Helper()
	all, err := h.store.OpenTabs(context.Background(), 100)
	if err != nil {
		h.t.Fatal(err)
	}
	return all
}

// waitTabs waits for the records to come to n (they change as sessions end,
// on the session's goroutine).
func (h *harness) waitTabs(n int, what string) []*store.OpenTab {
	h.t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		all := h.tabs()
		if len(all) == n {
			return all
		}
		if time.Now().After(deadline) {
			h.t.Fatalf("%s: %d records, want %d: %+v", what, len(all), n, all)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// A tmux terminal opened is kept (a shell is not); closed, it is not.
func TestOpenTabKeptWhileOpen(t *testing.T) {
	h := newHarness(t)
	admin := h.admin()
	_, id := pickerHost(t, h, admin, true, "password")
	mustStatus(t, admin.do("POST", "/api/sessions", map[string]any{"host_id": id, "open": map[string]any{"kind": "shell"}}), 201, "shell")
	r := admin.do("POST", "/api/sessions", map[string]any{"host_id": id, "open": map[string]any{"kind": "tmux", "name": "work"}})
	mustStatus(t, r, 201, "tmux")
	sid := r.json()["id"].(string)
	all := h.tabs()
	if len(all) != 1 || all[0].SessionID != sid || all[0].Kind != store.PersistTmux || all[0].Name != "work" || all[0].HostID != int64(id) {
		t.Fatalf("records %+v", all)
	}
	// Opened again from another tab: the same terminal, the same record.
	mustStatus(t, admin.do("POST", "/api/sessions", map[string]any{"host_id": id, "open": map[string]any{"kind": "tmux", "name": "work"}}), 200, "reused")
	if all := h.tabs(); len(all) != 1 || all[0].SessionID != sid {
		t.Fatalf("after reuse %+v", all)
	}
	mustStatus(t, admin.do("DELETE", "/api/sessions/"+sid, nil), 200, "close")
	h.waitTabs(0, "closed")
}

// The gateway stopping: the terminal ends as stopped and its record stays,
// with when it was last running and since when nobody looked; opening
// after is refused as restarting, and nothing is kept for it.
func TestOpenTabKeptOverStop(t *testing.T) {
	h := newHarness(t)
	admin := h.admin()
	_, id := pickerHost(t, h, admin, true, "password")
	r := admin.do("POST", "/api/sessions", map[string]any{"host_id": id, "open": map[string]any{"kind": "tmux", "name": "work"}})
	mustStatus(t, r, 201, "tmux")
	before := h.tabs()[0]
	sess, err := h.terms.Get(before.OwnerID, before.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	idle := sess.Snapshot().IdleSince // (nobody has looked since it was opened)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	time.Sleep(1100 * time.Millisecond) // (times are in seconds)
	h.srv.BeginShutdown(ctx)
	r = admin.do("POST", "/api/sessions", map[string]any{"host_id": id, "open": map[string]any{"kind": "tmux", "name": "other"}})
	if r.Status != 503 || r.code() != "restarting" {
		t.Fatalf("open while stopping: %d %s", r.Status, r.Body)
	}
	h.terms.Shutdown()
	if !h.terms.WaitEnds(ctx) {
		t.Fatal("ends not recorded")
	}
	all := h.tabs()
	if len(all) != 1 || all[0].SessionID != before.SessionID {
		t.Fatalf("records after the stop %+v", all)
	}
	// As of the stop, and since when nobody looked: kept, not the stop's time.
	if all[0].AliveAt <= before.AliveAt || idle.IsZero() || all[0].IdleSince != idle.Unix() {
		t.Errorf("before %+v (idle since %v) after %+v", before, idle, all[0])
	}
}

// Signing out of the last login takes the records; the terminal stays
// (as it does for sessions shared or open elsewhere).
func TestOpenTabRevoked(t *testing.T) {
	h := newHarness(t)
	admin := h.admin()
	_, id := pickerHost(t, h, admin, true, "password")
	mustStatus(t, admin.do("POST", "/api/sessions", map[string]any{"host_id": id, "open": map[string]any{"kind": "tmux", "name": "work"}}), 201, "tmux")
	mustStatus(t, admin.do("POST", "/api/logout", nil), 200, "sign out")
	if all := h.tabs(); len(all) != 0 {
		t.Fatalf("kept after the last sign-out: %+v", all)
	}
}

// The heartbeat records running terminals as running.
func TestOpenTabHeartbeat(t *testing.T) {
	h := newHarness(t)
	admin := h.admin()
	_, id := pickerHost(t, h, admin, true, "password")
	mustStatus(t, admin.do("POST", "/api/sessions", map[string]any{"host_id": id, "open": map[string]any{"kind": "tmux", "name": "work"}}), 201, "tmux")
	before := h.tabs()[0]
	time.Sleep(1100 * time.Millisecond)
	h.srv.keepRunningTabs(context.Background())
	if after := h.tabs()[0]; after.AliveAt <= before.AliveAt {
		t.Errorf("not recorded as running: before %d after %d", before.AliveAt, after.AliveAt)
	}
}

// restored: the session.restore outcomes, by the name of what was kept.
func (h *harness) restored() map[string]string {
	h.t.Helper()
	list, err := h.store.ListAudit(context.Background(), storeAuditQuery())
	if err != nil {
		h.t.Fatal(err)
	}
	out := map[string]string{}
	for _, a := range list {
		if a.Action != "session.restore" {
			continue
		}
		var d struct{ Name, Outcome string }
		_ = json.Unmarshal(a.Detail, &d)
		out[d.Name] = d.Outcome
	}
	return out
}

// kept: the records as a start reads them, before anyone connects.
func (h *harness) kept() []*store.OpenTab {
	h.t.Helper()
	all, err := h.store.OpenTabs(context.Background(), RestoreMax)
	if err != nil {
		h.t.Fatal(err)
	}
	return all
}

func (h *harness) restoreNow(kept []*store.OpenTab) {
	h.t.Helper()
	h.srv.StartRestore(kept)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if !h.srv.WaitRestores(ctx) {
		h.t.Fatal("restores did not end")
	}
}

// After a restart: what still runs at the host is opened again, attached
// only, its record now the new terminal's, with its size and unattended
// time; what is no longer there is not, nor what is too old; one opened
// again from a browser first is left to it.
func TestRestoreAfterRestart(t *testing.T) {
	h := newHarness(t)
	admin := h.admin()
	srv, id := pickerHost(t, h, admin, true, "password") // (its tmux lists work and other)
	for _, name := range []string{"work", "other", "nothere"} {
		mustStatus(t, admin.do("POST", "/api/sessions", map[string]any{"host_id": id, "cols": 132, "rows": 40, "open": map[string]any{"kind": "tmux", "name": name}}), 201, name)
	}
	h.restart()
	kept := h.kept()
	byName := map[string]*store.OpenTab{}
	for _, tb := range kept {
		byName[tb.Name] = tb
	}
	// other: opened again from a browser before its turn.
	mustStatus(t, admin.do("POST", "/api/sessions", map[string]any{"host_id": id, "open": map[string]any{"kind": "tmux", "name": "other"}}), 201, "other again")
	execsBefore := len(srv.Commands())
	h.restoreNow(kept)

	got := h.restored()
	if got["work"] != "restored" || got["nothere"] != "skipped:not_running" || got["other"] != "" {
		t.Fatalf("outcomes %v", got)
	}
	list := h.terms.List(byName["work"].OwnerID)
	if len(list) != 2 {
		t.Fatalf("running %+v", list)
	}
	var otherID string
	var work string
	for _, in := range list {
		switch in.MuxSession {
		case "work":
			work = in.ID
		case "other":
			otherID = in.ID
		}
	}
	all := h.tabs()
	names := map[string]string{}
	for _, tb := range all {
		names[tb.Name] = tb.SessionID
	}
	if len(all) != 2 || names["work"] != work || work == byName["work"].SessionID || names["other"] != otherID || names["nothere"] != "" {
		t.Fatalf("records %+v (work running as %s)", all, work)
	}
	sess, _ := h.terms.Get(byName["work"].OwnerID, work)
	if snap := sess.Snapshot(); snap.Cols != 132 || snap.Rows != 40 || snap.IdleSince.Unix() != byName["work"].IdleSince {
		t.Errorf("size or unattended time not kept: %+v, recorded %+v", snap, byName["work"])
	}
	// Attached only: no command that makes a session or a shell.
	for _, c := range srv.Commands()[execsBefore:] {
		if strings.Contains(c, "new-session") || strings.Contains(c, "SHELL") {
			t.Errorf("ran %s", c)
		}
	}
}

// Too old, or its user signed out everywhere: not opened again.
func TestRestoreSkipsStaleAndSignedOut(t *testing.T) {
	h := newHarness(t)
	admin := h.admin()
	_, id := pickerHost(t, h, admin, true, "password")
	mustStatus(t, admin.do("POST", "/api/sessions", map[string]any{"host_id": id, "open": map[string]any{"kind": "tmux", "name": "work"}}), 201, "work")
	h.restart()
	tb := h.tabs()[0]
	old := time.Now().Add(-2 * time.Hour).Unix()
	if err := h.store.KeepTabs(context.Background(), []store.TabState{{SessionID: tb.SessionID, OwnerID: tb.OwnerID, AliveAt: old, IdleSince: old, Cols: 80, Rows: 24}}); err != nil {
		t.Fatal(err)
	}
	h.restoreNow(h.kept())
	if got := h.restored()["work"]; got != "skipped:stale" || len(h.tabs()) != 0 || len(h.terms.List(tb.OwnerID)) != 0 {
		t.Fatalf("stale: %q, records %d", got, len(h.tabs()))
	}

	mustStatus(t, admin.do("POST", "/api/sessions", map[string]any{"host_id": id, "open": map[string]any{"kind": "tmux", "name": "other"}}), 201, "other")
	// (The logins outlived as the new gateway counts them: none valid.)
	time.Sleep(1100 * time.Millisecond)
	h.cfg.LoginIdleTimeout = time.Millisecond
	h.restart()
	h.restoreNow(h.kept())
	if got := h.restored()["other"]; got != "skipped:no_login" || len(h.tabs()) != 0 || len(h.terms.List(tb.OwnerID)) != 0 {
		t.Fatalf("signed out: %q, records %d", got, len(h.tabs()))
	}
}

// herdr: the default session opened again as herdr's default (not one
// named "default"), a named one by its name; one stopped since, not.
func TestRestoreHerdr(t *testing.T) {
	h := newHarness(t)
	admin := h.admin()
	list := `{"sessions":[{"name":"default","default":true,"running":true},{"name":"work","default":false,"running":true},{"name":"gone","default":false,"running":false}]}`
	srv, id := pickerHost(t, h, admin, false, "password", list)
	for _, name := range []string{"", "work", "gone"} {
		mustStatus(t, admin.do("POST", "/api/sessions", map[string]any{"host_id": id, "open": map[string]any{"kind": "herdr", "name": name}}), 201, "herdr "+name)
	}
	if all := h.tabs(); len(all) != 3 {
		t.Fatalf("records %+v", all)
	}
	h.restart()
	before := len(srv.Commands())
	h.restoreNow(h.kept())
	got := h.restored()
	if got["default"] != "restored" || got["work"] != "restored" || got["gone"] != "skipped:not_running" {
		t.Fatalf("outcomes %v", got)
	}
	var attach []string
	for _, c := range srv.Commands()[before:] {
		if strings.Contains(c, "exit 127") {
			attach = append(attach, c)
		}
	}
	if len(attach) != 2 || !slices.ContainsFunc(attach, func(c string) bool { return strings.HasSuffix(c, " corral ''") }) ||
		!slices.ContainsFunc(attach, func(c string) bool { return strings.HasSuffix(c, " corral 'work'") }) {
		t.Errorf("attached with %q", attach)
	}
	if all := h.tabs(); len(all) != 2 {
		t.Errorf("records after %+v", all)
	}
}

// Over the user's terminals: the rest kept, for later (Reserve counts
// what is pending: it may be only for now).
func TestRestoreQuotaKeeps(t *testing.T) {
	h := newHarness(t)
	admin := h.admin()
	_, id := pickerHost(t, h, admin, true, "password")
	for _, name := range []string{"work", "other"} {
		mustStatus(t, admin.do("POST", "/api/sessions", map[string]any{"host_id": id, "open": map[string]any{"kind": "tmux", "name": name}}), 201, name)
	}
	h.cfg.MaxSessionsPerUser = 1
	h.restart()
	h.restoreNow(h.kept())
	got := h.restored()
	if !(got["work"] == "restored" && got["other"] == "cancelled:quota" || got["other"] == "restored" && got["work"] == "cancelled:quota") {
		t.Fatalf("outcomes %v", got)
	}
	if all := h.tabs(); len(all) != 2 {
		t.Errorf("records %+v", all)
	}
}

// The gateway stopping while an open asks the host (here, the names of its
// tmux sessions for a new one): restarting, not the host failing.
func TestOpenCancelledByStop(t *testing.T) {
	h := newHarness(t)
	admin := h.admin()
	srv := h.sshServer(t.TempDir())
	asked, block := make(chan struct{}), make(chan struct{})
	defer close(block)
	srv.HandleExec(func(cmd string) func(ch ssh.Channel) {
		if !strings.Contains(cmd, "list-sessions") {
			return nil
		}
		return func(ch ssh.Channel) {
			close(asked)
			<-block
		}
	})
	r := admin.do("POST", "/api/hosts", map[string]any{"name": "box", "address": "127.0.0.1", "port": srv.Addr.Port, "username": "alice", "auth": "password", "password": "alice-pass"})
	mustStatus(t, r, 201, "host")
	id := r.json()["id"].(float64)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go func() {
		<-asked
		h.srv.BeginShutdown(ctx)
	}()
	r = admin.do("POST", "/api/sessions", map[string]any{"host_id": id, "open": map[string]any{"kind": "tmux_new"}})
	if r.Status != 503 || r.code() != "restarting" {
		t.Fatalf("open cancelled by the stop: %d %s", r.Status, r.Body)
	}
	if len(h.tabs()) != 0 {
		t.Errorf("kept %+v", h.tabs())
	}
}
