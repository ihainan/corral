package web

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"testing"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"

	"github.com/ihainan/corral/internal/config"
	"github.com/ihainan/corral/internal/corrald/gw"
	"github.com/ihainan/corral/internal/sftplite"
	"github.com/ihainan/corral/internal/sshx"
	"github.com/ihainan/corral/internal/store"
)

type fakeNetTimeoutErr struct{}

func (fakeNetTimeoutErr) Error() string   { return "i/o timeout" }
func (fakeNetTimeoutErr) Timeout() bool   { return true }
func (fakeNetTimeoutErr) Temporary() bool { return true }

func TestCorraldHintClassifyError(t *testing.T) {
	positives := []struct {
		name string
		err  error
	}{
		{"sshx.ErrBusy", sshx.ErrBusy},
		{"wrapped sshx.ErrBusy", fmt.Errorf("%w: %w", errSFTPUnavailable, sshx.ErrBusy)},
		{"OpenChannelError Prohibited open failed", &ssh.OpenChannelError{Reason: ssh.Prohibited, Message: "open failed"}},
		{"OpenChannelError Prohibited no more sessions", &ssh.OpenChannelError{Reason: ssh.Prohibited, Message: "no more sessions"}},
		{"OpenChannelError ResourceShortage", &ssh.OpenChannelError{Reason: ssh.ResourceShortage, Message: "resource shortage"}},
		{"wrapped OpenChannelError", fmt.Errorf("open session: %w", &ssh.OpenChannelError{Reason: ssh.Prohibited, Message: "open failed"})},
		{"stringified administratively prohibited", errors.New("ssh: rejected: administratively prohibited (open failed)")},
		{"stringified MaxSessions", errors.New("channel open failed: MaxSessions limit reached")},
		{"stringified no more sessions", errors.New("ssh: no more sessions available")},
		{"stringified too many sessions", errors.New("too many sessions on connection")},
		{"stringified resource shortage", errors.New("ssh: rejected: resource shortage")},
		{"stringified channel open failed", errors.New("ssh: channel open failed")},
	}
	for _, tc := range positives {
		reason, ok := ClassifyCorraldSolvableError(tc.err)
		if !ok || reason != HintReasonSSHSessionFull {
			t.Errorf("positive %q: got (%q, %v), want (%q, true)", tc.name, reason, ok, HintReasonSSHSessionFull)
		}
	}

	negatives := []struct {
		name string
		err  error
	}{
		{"nil", nil},
		{"context.DeadlineExceeded", context.DeadlineExceeded},
		{"wrapped context.DeadlineExceeded", fmt.Errorf("locate: %w", context.DeadlineExceeded)},
		{"context.Canceled", context.Canceled},
		{"errFileTimeout", errFileTimeout},
		{"net timeout", fakeNetTimeoutErr{}},
		{"JumpError wrapping Prohibited", &sshx.JumpError{Err: &ssh.OpenChannelError{Reason: ssh.Prohibited, Message: "open failed"}}},
		{"ExitError", &sshx.ExitError{Status: 1}},
		{"ErrOutputTooLarge", sshx.ErrOutputTooLarge},
		{"ErrAuthFailed", sshx.ErrAuthFailed},
		{"HostKeyChangedError", &sshx.HostKeyChangedError{}},
		{"fs.ErrNotExist", fs.ErrNotExist},
		{"fs.ErrPermission", fs.ErrPermission},
		{"sftp.StatusError", &sftp.StatusError{Code: uint32(sftp.ErrSSHFxFailure)}},
		{"sftplite.StatusError", &sftplite.StatusError{Code: 4}},
		{"io.EOF", io.EOF},
		{"OpenChannelError ConnectionFailed", &ssh.OpenChannelError{Reason: ssh.ConnectionFailed, Message: "connect failed"}},
		{"OpenChannelError UnknownChannelType", &ssh.OpenChannelError{Reason: ssh.UnknownChannelType, Message: "unknown"}},
		{"OpenChannelError Prohibited forwarding", &ssh.OpenChannelError{Reason: ssh.Prohibited, Message: "port forwarding disabled"}},
		{"stringified forwarding prohibited", errors.New("ssh: rejected: administratively prohibited (forwarding disabled)")},
		{"plain file open failed", errors.New("open failed: /tmp/foo: no such file or directory")},
		{"refused exec", errors.New("the host refused to run the command")},
		{"ended without exit status", errors.New("the remote command ended without an exit status")},
	}
	for _, tc := range negatives {
		reason, ok := ClassifyCorraldSolvableError(tc.err)
		if ok || reason != "" {
			t.Errorf("negative %q: got (%q, %v), want (\"\", false)", tc.name, reason, ok)
		}
	}
}

func makeFakeCorraldDist() *gw.Dist {
	return &gw.Dist{
		Version:      "0.3.1",
		Release:      1000,
		ManifestHash: "abc123",
		Binaries: map[string]*gw.ArchBinary{
			"amd64": {Arch: "amd64", Path: "/nonexistent/corrald", SHA256: "00", Size: 10 << 20},
		},
	}
}

func createTestHostForHints(t *testing.T, c *client, name string, port int) int64 {
	t.Helper()
	r := c.do("POST", "/api/hosts", map[string]any{
		"name":     name,
		"address":  "127.0.0.1",
		"port":     port,
		"username": "devuser",
		"auth":     "ask",
	})
	mustStatus(t, r, http.StatusCreated, "create host "+name)
	return int64(r.json()["id"].(float64))
}

func patchUserSettings(t *testing.T, c *client, userID int64, writer string, seq int64, set map[string]any) resp {
	t.Helper()
	return c.do("PATCH", "/api/me/settings", map[string]any{
		"user":   userID,
		"writer": writer,
		"seq":    seq,
		"set":    set,
	})
}

func TestCorraldHintEligibilityAllBranches(t *testing.T) {
	h := newHarness(t, func(c *config.Config) {
		c.GatewayID = "gw-hint-elig.local"
	})
	admin := h.admin()
	alice := h.member(admin, "alice")
	bob := h.member(admin, "bob")

	aliceID := userIDOf(t, alice)
	host1 := createTestHostForHints(t, alice, "alpha-box", 2201)
	host2 := createTestHostForHints(t, alice, "beta-box", 2202)

	now := time.Unix(1_700_000_000, 0)
	h.srv.corraldHints.now = func() time.Time { return now }

	// 1. No problem recorded -> hint is null even when corrald is available.
	h.srv.corrald.SetDist(makeFakeCorraldDist())
	r := alice.do("GET", "/api/corrald/hints", nil)
	mustStatus(t, r, http.StatusOK, "hints with no problems")
	if r.json()["hint"] != nil {
		t.Fatalf("expected nil hint with no problem, got %v", r.json()["hint"])
	}

	// 2. Non-solvable error (timeout) is ignored.
	if h.srv.recordCorraldHintError(aliceID, host1, HintOpStatus, context.DeadlineExceeded) {
		t.Fatal("timeout must not be recorded as corrald-solvable")
	}
	if got := alice.do("GET", "/api/corrald/hints", nil).json()["hint"]; got != nil {
		t.Fatalf("expected nil hint after timeout, got %v", got)
	}

	// 3. Corrald unavailable (no Dist) -> hint is null even with recorded problem.
	h.srv.corrald.SetDist(nil)
	if !h.srv.recordCorraldHintError(aliceID, host1, HintOpStatus, sshx.ErrBusy) {
		t.Fatal("expected ErrBusy to be recorded")
	}
	if got := alice.do("GET", "/api/corrald/hints", nil).json()["hint"]; got != nil {
		t.Fatalf("expected nil hint when corrald dist unavailable, got %v", got)
	}

	// Recording failures must NOT write anything to corrald_host_hints in DB.
	dbRows, err := h.store.ListCorraldHostHints(context.Background(), aliceID)
	if err != nil {
		t.Fatalf("ListCorraldHostHints: %v", err)
	}
	if len(dbRows) != 0 {
		t.Fatalf("recording failures must stay in memory only; found %d DB rows", len(dbRows))
	}

	// 4. Enable Dist -> host1 becomes eligible with count=1, op="status".
	h.srv.corrald.SetDist(makeFakeCorraldDist())
	h.srv.recordCorraldHintError(aliceID, host1, HintOpStatus, &ssh.OpenChannelError{Reason: ssh.Prohibited, Message: "open failed"})
	r = alice.do("GET", "/api/corrald/hints", nil)
	mustStatus(t, r, http.StatusOK, "eligible hint")
	hint := r.json()["hint"].(map[string]any)
	if int64(hint["host_id"].(float64)) != host1 ||
		hint["host_name"] != "alpha-box" ||
		hint["reason"] != string(HintReasonSSHSessionFull) ||
		hint["op"] != string(HintOpStatus) ||
		int(hint["count"].(float64)) != 2 {
		t.Fatalf("unexpected hint payload: %+v", hint)
	}

	// Adding a log-read failure on host1 upgrades op to "both" and count to 3.
	now = now.Add(5 * time.Second)
	h.srv.recordCorraldHintError(aliceID, host1, HintOpLog, sshx.ErrBusy)
	hint = alice.do("GET", "/api/corrald/hints", nil).json()["hint"].(map[string]any)
	if hint["op"] != string(HintOpBoth) || int(hint["count"].(float64)) != 3 {
		t.Fatalf("expected op=both count=3, got %+v", hint)
	}

	// 5. Multiple hosts with problems -> only ONE hint returned, the most recent one.
	now = now.Add(10 * time.Second)
	h.srv.recordCorraldHintError(aliceID, host2, HintOpLog, sshx.ErrBusy)
	hint = alice.do("GET", "/api/corrald/hints", nil).json()["hint"].(map[string]any)
	if int64(hint["host_id"].(float64)) != host2 || hint["op"] != string(HintOpLog) {
		t.Fatalf("expected most recent host2 (beta-box), got %+v", hint)
	}

	// Explicit ?host_id= filter works for owner and returns 404 for other user.
	r1 := alice.do("GET", fmt.Sprintf("/api/corrald/hints?host_id=%d", host1), nil)
	mustStatus(t, r1, http.StatusOK, "alice host1 hint")
	if int64(r1.json()["hint"].(map[string]any)["host_id"].(float64)) != host1 {
		t.Fatalf("expected host1 for explicit filter, got %+v", r1.json())
	}
	mustStatus(t, bob.do("GET", fmt.Sprintf("/api/corrald/hints?host_id=%d", host1), nil), http.StatusNotFound, "bob querying alice host")
	if got := bob.do("GET", "/api/corrald/hints", nil).json()["hint"]; got != nil {
		t.Fatalf("bob must not see alice's hints: %v", got)
	}

	// 6. Admin disabled -> returns null; re-allowing restores it.
	mustStatus(t, admin.do("PUT", "/api/admin/corrald", map[string]any{"allowed": false}), http.StatusOK, "admin disable")
	if got := alice.do("GET", "/api/corrald/hints", nil).json()["hint"]; got != nil {
		t.Fatalf("expected nil hint when admin disabled, got %v", got)
	}
	mustStatus(t, admin.do("PUT", "/api/admin/corrald", map[string]any{"allowed": true}), http.StatusOK, "admin allow")
	if got := alice.do("GET", "/api/corrald/hints", nil).json()["hint"]; got == nil {
		t.Fatal("expected hint to reappear when admin re-allowed")
	}

	// 7. Every non-off corrald_hosts state suppresses the hint on that host.
	ctx := context.Background()
	fp := "SHA256:hint-test-fp"
	if err := h.store.PinHostKey(ctx, aliceID, host2, &store.HostKey{
		Fingerprint: fp, KeyType: "ssh-ed25519", KeyData: []byte("pk"),
	}); err != nil {
		t.Fatalf("PinHostKey: %v", err)
	}
	if err := h.store.RecordSSHVerified(ctx, host2, fp, "devuser"); err != nil {
		t.Fatalf("RecordSSHVerified: %v", err)
	}

	nonEligibleStates := []string{
		gw.StateInstalling,
		gw.StateRunning,
		gw.StateStopped,
		gw.StateUpdating,
		gw.StatePaused,
		gw.StateLost,
		gw.StateError,
		gw.StateRemoving,
		gw.StateRemovePending,
		gw.StateRebind,
		gw.StateIncompatible,
	}
	for _, stName := range nonEligibleStates {
		paused := []int64{}
		dbState := stName
		if stName == gw.StatePaused {
			dbState = gw.StateRunning
			paused = []int64{aliceID}
		}
		if err := h.store.UpsertCorraldHost(ctx, &store.CorraldHost{
			HostID:           host2,
			State:            dbState,
			BoundFingerprint: fp,
			BoundUsername:    "devuser",
			BoundGateway:     h.cfg.GatewayID,
			PausedUsers:      paused,
		}); err != nil {
			t.Fatalf("UpsertCorraldHost(%s): %v", stName, err)
		}
		r := alice.do("GET", fmt.Sprintf("/api/corrald/hints?host_id=%d", host2), nil)
		mustStatus(t, r, http.StatusOK, "state "+stName)
		if r.json()["hint"] != nil {
			t.Errorf("state %q must suppress hint, got %+v", stName, r.json()["hint"])
		}
	}

	// Restoring state to off makes host2 eligible again, UNLESS an unsupported_os/arch install failed.
	if err := h.store.UpsertCorraldHost(ctx, &store.CorraldHost{HostID: host2, State: gw.StateOff}); err != nil {
		t.Fatalf("reset host2 off: %v", err)
	}
	if got := alice.do("GET", fmt.Sprintf("/api/corrald/hints?host_id=%d", host2), nil).json()["hint"]; got == nil {
		t.Fatal("expected host2 eligible again once off")
	}

	// Platform-incompatible install failure suppresses hint even in StateOff.
	if err := h.store.CreateCorraldOp(ctx, &store.CorraldOp{
		ID: "op-incompat-1", HostID: host2, Kind: gw.OpInstall, State: gw.OpFailed,
		ErrorCode: "unsupported_os", CreatedBy: aliceID, CreatedAt: now.Unix(), UpdatedAt: now.Unix(),
	}, false); err != nil {
		t.Fatalf("CreateCorraldOp unsupported_os: %v", err)
	}
	if got := alice.do("GET", fmt.Sprintf("/api/corrald/hints?host_id=%d", host2), nil).json()["hint"]; got != nil {
		t.Fatalf("unsupported_os must suppress hint, got %+v", got)
	}
}

func TestCorraldHintShowOnceDismissAndMasterToggle(t *testing.T) {
	h := newHarness(t)
	h.srv.corrald.SetDist(makeFakeCorraldDist())
	admin := h.admin()
	alice := h.member(admin, "alice")
	aliceID := userIDOf(t, alice)

	host1 := createTestHostForHints(t, alice, "box-one", 2211)
	host2 := createTestHostForHints(t, alice, "box-two", 2212)

	now := time.Unix(1_700_100_000, 0)
	h.srv.corraldHints.now = func() time.Time { return now }

	// Record problem on host1, then later on host2.
	h.srv.recordCorraldHintError(aliceID, host1, HintOpStatus, sshx.ErrBusy)
	now = now.Add(5 * time.Second)
	h.srv.recordCorraldHintError(aliceID, host2, HintOpLog, sshx.ErrBusy)

	// Host2 is most recent.
	got := alice.do("GET", "/api/corrald/hints", nil).json()["hint"].(map[string]any)
	if int64(got["host_id"].(float64)) != host2 {
		t.Fatalf("expected host2 first, got %+v", got)
	}

	// Report host2 shown (simulating user seeing banner and closing with × without clicking dismiss).
	mustStatus(t, alice.do("POST", fmt.Sprintf("/api/hosts/%d/corrald/hint/shown", host2), nil), http.StatusOK, "mark host2 shown")
	row2, err := h.store.CorraldHostHint(context.Background(), aliceID, host2)
	if err != nil || row2.ShownAt == 0 || row2.DismissedAt != 0 {
		t.Fatalf("expected shown_at>0, dismissed_at=0 for host2: (%+v, %v)", row2, err)
	}

	// Even if another failure hits host2, host2 is shown once and does NOT reappear; host1 now surfaces.
	now = now.Add(10 * time.Second)
	h.srv.recordCorraldHintError(aliceID, host2, HintOpStatus, sshx.ErrBusy)
	got = alice.do("GET", "/api/corrald/hints", nil).json()["hint"].(map[string]any)
	if int64(got["host_id"].(float64)) != host1 {
		t.Fatalf("expected host1 after host2 was shown, got %+v", got)
	}

	// Dismiss host1 ("不再提示") -> both shown_at and dismissed_at set; no more hints.
	mustStatus(t, alice.do("POST", fmt.Sprintf("/api/hosts/%d/corrald/hint/dismiss", host1), nil), http.StatusOK, "dismiss host1")
	row1, err := h.store.CorraldHostHint(context.Background(), aliceID, host1)
	if err != nil || row1.ShownAt == 0 || row1.DismissedAt == 0 {
		t.Fatalf("expected shown_at>0 and dismissed_at>0 for host1: (%+v, %v)", row1, err)
	}
	if got := alice.do("GET", "/api/corrald/hints", nil).json()["hint"]; got != nil {
		t.Fatalf("expected nil hint after host1 dismissed & host2 shown, got %+v", got)
	}

	// Master toggle OFF suppresses even a brand-new host3 with a fresh failure.
	host3 := createTestHostForHints(t, alice, "box-three", 2213)
	now = now.Add(5 * time.Second)
	h.srv.recordCorraldHintError(aliceID, host3, HintOpStatus, sshx.ErrBusy)

	writer := "writer_hint_test_01"
	mustStatus(t, patchUserSettings(t, alice, aliceID, writer, 1, map[string]any{"corraldHints": false}), http.StatusOK, "master toggle off")
	if got := alice.do("GET", "/api/corrald/hints", nil).json()["hint"]; got != nil {
		t.Fatalf("expected nil hint when master toggle is off, got %+v", got)
	}

	// Stale superseded request (seq=1 again with true) must NOT re-enable or reset.
	stale := patchUserSettings(t, alice, aliceID, writer, 1, map[string]any{"corraldHints": true})
	mustStatus(t, stale, http.StatusOK, "stale patch")
	if got := alice.do("GET", "/api/corrald/hints", nil).json()["hint"]; got != nil {
		t.Fatalf("stale superseded patch must not re-enable hints: %+v", got)
	}

	// Master toggle ON (seq=2) re-enables AND restores previously dismissed & shown hosts!
	mustStatus(t, patchUserSettings(t, alice, aliceID, writer, 2, map[string]any{"corraldHints": true}), http.StatusOK, "master toggle on")
	allRows, err := h.store.ListCorraldHostHints(context.Background(), aliceID)
	if err != nil {
		t.Fatalf("ListCorraldHostHints after re-enable: %v", err)
	}
	if len(allRows) != 0 {
		t.Fatalf("expected all host hint rows cleared on re-enable, got %d", len(allRows))
	}
	// Host1 (previously dismissed) is eligible again!
	r1 := alice.do("GET", fmt.Sprintf("/api/corrald/hints?host_id=%d", host1), nil)
	mustStatus(t, r1, http.StatusOK, "host1 restored")
	if r1.json()["hint"] == nil {
		t.Fatal("expected previously dismissed host1 to be restored after turning master toggle on")
	}
	// Host2 (previously shown) is also eligible again!
	r2 := alice.do("GET", fmt.Sprintf("/api/corrald/hints?host_id=%d", host2), nil)
	mustStatus(t, r2, http.StatusOK, "host2 restored")
	if r2.json()["hint"] == nil {
		t.Fatal("expected previously shown host2 to be restored after turning master toggle on")
	}
}

func TestCorraldHintSettingsWhitelist(t *testing.T) {
	h := newHarness(t)
	alice := h.member(h.admin(), "alice")
	aliceID := userIDOf(t, alice)
	writer := "writer_wl_test_01"

	// Valid boolean values accepted and round-tripped via GET /api/me/settings.
	mustStatus(t, patchUserSettings(t, alice, aliceID, writer, 1, map[string]any{"corraldHints": false}), http.StatusOK, "corraldHints false")
	got := alice.do("GET", "/api/me/settings", nil).json()
	if v, ok := got["corraldHints"].(bool); !ok || v {
		t.Fatalf("expected corraldHints=false in GET /api/me/settings, got %+v", got)
	}
	mustStatus(t, patchUserSettings(t, alice, aliceID, writer, 2, map[string]any{"corraldHints": true}), http.StatusOK, "corraldHints true")
	got = alice.do("GET", "/api/me/settings", nil).json()
	if v, ok := got["corraldHints"].(bool); !ok || !v {
		t.Fatalf("expected corraldHints=true in GET /api/me/settings, got %+v", got)
	}

	// Invalid types for corraldHints rejected with 400 bad_setting.
	for i, badVal := range []any{"true", 1, nil, map[string]any{"on": true}} {
		r := patchUserSettings(t, alice, aliceID, writer, int64(10+i), map[string]any{"corraldHints": badVal})
		mustStatus(t, r, http.StatusBadRequest, fmt.Sprintf("bad corraldHints value %#v", badVal))
		if r.code() != "bad_setting" {
			t.Fatalf("expected bad_setting for %#v, got %q", badVal, r.code())
		}
	}

	// Unknown keys rejected by whitelist with 400 bad_setting.
	for i, badKey := range []string{"corrald_hints", "corraldHintsExtra", "arbitraryKey"} {
		r := patchUserSettings(t, alice, aliceID, writer, int64(20+i), map[string]any{badKey: true})
		mustStatus(t, r, http.StatusBadRequest, "unknown key "+badKey)
		if r.code() != "bad_setting" {
			t.Fatalf("expected bad_setting for unknown key %q, got %q", badKey, r.code())
		}
	}
}

func TestCorraldHintDeleteHostAndUserCleanup(t *testing.T) {
	h := newHarness(t)
	h.srv.corrald.SetDist(makeFakeCorraldDist())
	admin := h.admin()
	alice := h.member(admin, "alice")
	aliceID := userIDOf(t, alice)

	host1 := createTestHostForHints(t, alice, "del-box-1", 2221)
	host2 := createTestHostForHints(t, alice, "del-box-2", 2222)

	h.srv.recordCorraldHintError(aliceID, host1, HintOpStatus, sshx.ErrBusy)
	h.srv.recordCorraldHintError(aliceID, host2, HintOpLog, sshx.ErrBusy)
	mustStatus(t, alice.do("POST", fmt.Sprintf("/api/hosts/%d/corrald/hint/dismiss", host1), nil), http.StatusOK, "dismiss host1")
	mustStatus(t, alice.do("POST", fmt.Sprintf("/api/hosts/%d/corrald/hint/shown", host2), nil), http.StatusOK, "shown host2")

	// Delete host1 via API -> clears both DB row and in-memory problem entry.
	mustStatus(t, alice.do("DELETE", fmt.Sprintf("/api/hosts/%d", host1), map[string]any{
		"remove_corrald": false,
		"stop_vscode":    false,
		"remove_vscode":  false,
	}), http.StatusOK, "delete host1")

	if _, ok := h.srv.corraldHints.get(aliceID, host1); ok {
		t.Fatal("in-memory hint problem for deleted host1 was not cleaned up")
	}
	if _, err := h.store.CorraldHostHint(context.Background(), aliceID, host1); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("DB hint row for deleted host1 still present: %v", err)
	}
	if _, ok := h.srv.corraldHints.get(aliceID, host2); !ok {
		t.Fatal("in-memory hint problem for surviving host2 should remain")
	}

	// Revoking/removing user via OnUserGone cleans up in-memory tracker.
	h.auth.OnUserGone(aliceID)
	if len(h.srv.corraldHints.forUser(aliceID)) != 0 {
		t.Fatal("in-memory hint problems for gone user were not cleaned up")
	}
}

func TestCorraldHintEndToEndOnBusyConversationRead(t *testing.T) {
	h := newHarness(t, func(c *config.Config) {
		c.GatewayID = "gw-hint-e2e.local"
	})
	h.srv.corrald.SetDist(makeFakeCorraldDist())
	admin := h.admin()
	alice := h.member(admin, "alice")
	aliceID := userIDOf(t, alice)

	srv := h.sshServer(t.TempDir())
	hostID := createTestHostForHints(t, alice, "busy-box", srv.Addr.Port)
	// Update host to password auth so we can open a real test SSH session.
	mustStatus(t, alice.do("PUT", fmt.Sprintf("/api/hosts/%d", hostID), map[string]any{
		"name":     "busy-box",
		"address":  "127.0.0.1",
		"port":     srv.Addr.Port,
		"username": "alice",
		"auth":     "password",
		"password": "alice-pass",
	}), http.StatusOK, "update host auth")

	// First open gives host key prompt; accept it to open the session.
	r := alice.do("POST", "/api/sessions", map[string]any{"host_id": hostID})
	if r.Status == http.StatusConflict {
		fp := r.json()["fingerprint"].(string)
		r = alice.do("POST", "/api/sessions", map[string]any{"host_id": hostID, "accept_fingerprint": fp})
	}
	mustStatus(t, r, http.StatusCreated, "open session")
	sessID := r.json()["id"].(string)
	sess, err := h.terms.Get(aliceID, sessID)
	if err != nil {
		t.Fatalf("terms.Get: %v", err)
	}

	// Exhaust all Aux session slots on sess.Client() so convoTailsOutput hits real sshx.ErrBusy.
	var releases []func()
	defer func() {
		for _, rel := range releases {
			rel()
		}
	}()
	for i := 0; i < sshx.MaxAuxPerConn+2; i++ {
		rel, aerr := sshx.AcquireSession(context.Background(), sess.Client(), sshx.SessionAux, true, 0)
		if aerr != nil {
			break
		}
		releases = append(releases, rel)
	}

	_, cerr := h.srv.convoTailsOutput(context.Background(), sess, []string{"/tmp/nonexistent.jsonl"}, 4096)
	if !errors.Is(cerr, sshx.ErrBusy) {
		t.Fatalf("expected sshx.ErrBusy from convoTailsOutput with full Aux slots, got %v", cerr)
	}

	// Gateway hint API must now report this real failure for busy-box.
	hr := alice.do("GET", "/api/corrald/hints", nil)
	mustStatus(t, hr, http.StatusOK, "GET /api/corrald/hints")
	hint, ok := hr.json()["hint"].(map[string]any)
	if !ok || hint == nil {
		t.Fatalf("expected non-nil hint after real sshx.ErrBusy, got %+v", hr.json())
	}
	if int64(hint["host_id"].(float64)) != hostID || hint["host_name"] != "busy-box" || hint["reason"] != string(HintReasonSSHSessionFull) || hint["op"] != string(HintOpLog) {
		t.Fatalf("unexpected e2e hint payload: %+v", hint)
	}
}
