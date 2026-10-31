package web

import (
	"context"
	"testing"
	"time"

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
	if all[0].AliveAt <= before.AliveAt || all[0].IdleSince == 0 || all[0].IdleSince > all[0].AliveAt {
		t.Errorf("before %+v after %+v", before, all[0])
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
