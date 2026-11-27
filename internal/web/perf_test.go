package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ihainan/corral/internal/perf"
	"github.com/ihainan/corral/internal/sshx"
)

func TestAdminPerfAuth(t *testing.T) {
	h := newHarness(t)
	admin := h.admin()
	member := h.member(admin, "alice")
	anon := &client{h: h}

	// Unauthenticated -> 401.
	mustStatus(t, anon.do("GET", "/api/admin/perf", nil), http.StatusUnauthorized, "anon GET /api/admin/perf")
	// Non-admin member -> 403.
	mustStatus(t, member.do("GET", "/api/admin/perf", nil), http.StatusForbidden, "member GET /api/admin/perf")

	// Seed a couple of samples and verify admin receives valid JSON summary.
	h.srv.perf.Observe("locate.full", 125*time.Millisecond, "ok", 42, perf.ProcBucket(12))
	h.srv.perf.Observe("gated.send", 80*time.Millisecond, "enter_not_taken", 42, "")
	h.srv.perf.Count("ssh.busy", "exec", 42)

	r := admin.do("GET", "/api/admin/perf", nil)
	mustStatus(t, r, http.StatusOK, "admin GET /api/admin/perf")

	var snap perf.Snapshot
	if err := json.Unmarshal(r.Body, &snap); err != nil {
		t.Fatalf("unmarshal /api/admin/perf: %v (body=%s)", err, r.Body)
	}
	if snap.WindowSeconds != int64(perf.DefaultWindow/time.Second) {
		t.Fatalf("window_seconds = %d, want %d", snap.WindowSeconds, int64(perf.DefaultWindow/time.Second))
	}
	if snap.MaxSamplesPerOp != perf.DefaultMaxSamples {
		t.Fatalf("max_samples_per_op = %d, want %d", snap.MaxSamplesPerOp, perf.DefaultMaxSamples)
	}
	loc, ok := snap.Ops["locate.full"]
	if !ok || loc.Count != 1 || loc.Outcomes["ok"] != 1 || loc.ByBucket["6-15"].Count != 1 || loc.ByHost["42"].Count != 1 {
		t.Fatalf("unexpected locate.full summary: %+v", loc)
	}
	gs, ok := snap.Ops["gated.send"]
	if !ok || gs.Count != 1 || gs.Outcomes["enter_not_taken"] != 1 {
		t.Fatalf("unexpected gated.send summary: %+v", gs)
	}
	busy, ok := snap.Ops["ssh.busy"]
	if !ok || busy.Count != 1 || busy.Outcomes["exec"] != 1 || busy.P50Ms != nil {
		t.Fatalf("unexpected ssh.busy summary: %+v", busy)
	}
}

func TestAdminPerfNoSensitiveFields(t *testing.T) {
	h := newHarness(t)
	admin := h.admin()

	const hostName = "prod-secret-box"
	const hostAddr = "10.23.45.67"
	r := admin.do("POST", "/api/hosts", map[string]any{
		"name": hostName, "address": hostAddr, "username": "alice", "auth": "ask",
	})
	mustStatus(t, r, http.StatusCreated, "create host")
	hostID := int64(r.json()["id"].(float64))

	h.srv.perf.Observe("convo.read", 15*time.Millisecond, "ok", hostID, "")
	res := admin.do("GET", "/api/admin/perf", nil)
	mustStatus(t, res, http.StatusOK, "admin GET /api/admin/perf")
	body := string(res.Body)
	for _, forbidden := range []string{hostName, hostAddr, "alice"} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("/api/admin/perf leaked %q in %s", forbidden, body)
		}
	}
}

func TestGatedOutcomeMapping(t *testing.T) {
	cases := []struct {
		name string
		err  error
		what string
		want string
	}{
		{"ok", nil, "send", "ok"},
		{"channel_busy", sshx.ErrBusy, "send", "channel_busy"},
		{"exit3", &sshx.ExitError{Status: 3}, "send", "agent_changed"},
		{"exit4", &sshx.ExitError{Status: 4}, "send", "send_failed"},
		{"exit5", &sshx.ExitError{Status: 5}, "send", "typed_not_sent"},
		{"exit6", &sshx.ExitError{Status: 6}, "send", "type_failed"},
		{"exit7", &sshx.ExitError{Status: 7}, "send", "pane_busy"},
		{"exit8", &sshx.ExitError{Status: 8}, "send", "screen_unread"},
		{"exit9_reply", &sshx.ExitError{Status: 9}, "reply", "not_waiting"},
		{"exit9_dialog", &sshx.ExitError{Status: 9}, "close_dialog", "no_dialog"},
		{"exit9_panel", &sshx.ExitError{Status: 9}, "panel", "no_dialog"},
		{"exit9_send", &sshx.ExitError{Status: 9}, "send", "agent_waiting"},
		{"exit10", &sshx.ExitError{Status: 10}, "reply", "screen_changed"},
		{"exit11", &sshx.ExitError{Status: 11}, "reply", "cannot_verify"},
		{"exit12", &sshx.ExitError{Status: 12}, "command", "agent_busy"},
		{"exit13", &sshx.ExitError{Status: exitOwnerSubagent}, "send", "subagent_view"},
		{"exit14", &sshx.ExitError{Status: exitOwnerOther}, "send", "other_view"},
		{"exit15", &sshx.ExitError{Status: exitOwnerNone}, "send", "no_box"},
		{"exit16", &sshx.ExitError{Status: exitOwnerTyped}, "send", "owner_changed_typed"},
		{"exit17", &sshx.ExitError{Status: 17}, "send", "draft"},
		{"exit18", &sshx.ExitError{Status: 18}, "send", "enter_not_taken"},
		{"exit19", &sshx.ExitError{Status: exitOwnerUnread}, "send", "no_screen"},
		{"exit99", &sshx.ExitError{Status: 99}, "send", "exit_other"},
		{"timeout", context.DeadlineExceeded, "send", "timeout"},
		{"canceled", context.Canceled, "send", "canceled"},
		{"other", errors.New("boom"), "send", "error"},
	}
	for _, tc := range cases {
		if got := gatedOutcome(tc.err, tc.what); got != tc.want {
			t.Errorf("%s: gatedOutcome(%v, %q) = %q, want %q", tc.name, tc.err, tc.what, got, tc.want)
		}
	}
}

func TestLocateAndPressPerfOutcome(t *testing.T) {
	if got := locateOutcome(nil); got != "ok" {
		t.Errorf("locateOutcome(nil) = %q, want ok", got)
	}
	if got := locateOutcome(sshx.ErrBusy); got != "busy" {
		t.Errorf("locateOutcome(ErrBusy) = %q, want busy", got)
	}
	if got := locateOutcome(context.DeadlineExceeded); got != "timeout" {
		t.Errorf("locateOutcome(DeadlineExceeded) = %q, want timeout", got)
	}
	if got := locateOutcome(context.Canceled); got != "canceled" {
		t.Errorf("locateOutcome(Canceled) = %q, want canceled", got)
	}
	if got := locateOutcome(errors.New("eof")); got != "error" {
		t.Errorf("locateOutcome(err) = %q, want error", got)
	}

	s := &Server{perf: perf.New(perf.Options{})}
	start := time.Now().Add(-50 * time.Millisecond)
	s.perfRecordPress("models", start, "typed", "", false, 3)
	s.perfRecordPress("models", start, "kept", "list_kept", true, 3)
	s.perfRecordPress("models", start, "kept", "list_kept", false, 3)
	s.perfRecordPress("model", start, "typed", "", false, 3)
	s.perfRecordPress("effort", start, "refused", "busy", false, 3)
	s.perfRecordPress("mode", start, "looked", "unsure", false, 3)

	snap := s.perf.Snapshot()
	if snap.Ops["convo.models_first"].Outcomes["ok"] != 1 {
		t.Fatalf("models_first = %+v", snap.Ops["convo.models_first"])
	}
	rep := snap.Ops["convo.models_repeat"]
	if rep.Count != 2 || rep.Outcomes["fast_kept"] != 1 || rep.Outcomes["scan_kept"] != 1 {
		t.Fatalf("models_repeat = %+v", rep)
	}
	if snap.Ops["convo.model"].Outcomes["ok"] != 1 {
		t.Fatalf("convo.model = %+v", snap.Ops["convo.model"])
	}
	if snap.Ops["convo.effort"].Outcomes["busy"] != 1 {
		t.Fatalf("convo.effort = %+v", snap.Ops["convo.effort"])
	}
	if snap.Ops["convo.mode"].Outcomes["unsure"] != 1 {
		t.Fatalf("convo.mode = %+v", snap.Ops["convo.mode"])
	}
}
