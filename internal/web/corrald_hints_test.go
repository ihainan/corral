package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"

	"github.com/ihainan/corral/internal/config"
	"github.com/ihainan/corral/internal/corrald/gw"
	"github.com/ihainan/corral/internal/sftplite"
	"github.com/ihainan/corral/internal/sshx"
	"github.com/ihainan/corral/internal/store"
	"github.com/ihainan/corral/internal/term"
)

type fakeNetTimeoutErr struct{}

func (fakeNetTimeoutErr) Error() string   { return "i/o timeout" }
func (fakeNetTimeoutErr) Timeout() bool   { return true }
func (fakeNetTimeoutErr) Temporary() bool { return true }

// Item 1 regression test: only real SSH session-slot exhaustion (sshx.ErrBusy,
// RFC 4254 ResourceShortage, and explicit MaxSessions / session-limit messages)
// may trigger hints; generic "channel open failed" and policy-based
// ssh.Prohibited / "open failed" rejections must NOT trigger (fails on 89c46aa).
func TestCorraldHintClassifyError(t *testing.T) {
	positives := []struct {
		name string
		err  error
	}{
		{"sshx.ErrBusy", sshx.ErrBusy},
		{"wrapped sshx.ErrBusy", fmt.Errorf("%w: %w", errSFTPUnavailable, sshx.ErrBusy)},
		{"OpenChannelError Prohibited no more sessions", &ssh.OpenChannelError{Reason: ssh.Prohibited, Message: "no more sessions"}},
		{"OpenChannelError Prohibited MaxSessions", &ssh.OpenChannelError{Reason: ssh.Prohibited, Message: "MaxSessions exceeded"}},
		{"OpenChannelError ResourceShortage", &ssh.OpenChannelError{Reason: ssh.ResourceShortage, Message: "resource shortage"}},
		{"wrapped OpenChannelError ResourceShortage", fmt.Errorf("open session: %w", &ssh.OpenChannelError{Reason: ssh.ResourceShortage, Message: "too busy"})},
		{"stringified MaxSessions", errors.New("channel open failed: MaxSessions limit reached")},
		{"stringified no more sessions", errors.New("ssh: no more sessions available")},
		{"stringified too many sessions", errors.New("too many sessions on connection")},
		{"stringified session limit", errors.New("ssh: session limit reached")},
		{"stringified resource shortage", errors.New("ssh: rejected: resource shortage")},
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
		{"JumpError wrapping Prohibited", &sshx.JumpError{Err: &ssh.OpenChannelError{Reason: ssh.Prohibited, Message: "no more sessions"}}},
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
		// Item 1: generic/policy rejections that MUST NOT trigger (failed on 89c46aa).
		{"OpenChannelError Prohibited generic open failed", &ssh.OpenChannelError{Reason: ssh.Prohibited, Message: "open failed"}},
		{"OpenChannelError Prohibited policy", &ssh.OpenChannelError{Reason: ssh.Prohibited, Message: "sessions disabled by policy"}},
		{"OpenChannelError Prohibited empty", &ssh.OpenChannelError{Reason: ssh.Prohibited, Message: ""}},
		{"OpenChannelError Prohibited forwarding", &ssh.OpenChannelError{Reason: ssh.Prohibited, Message: "port forwarding disabled"}},
		{"plain channel open failed", errors.New("ssh: channel open failed")},
		{"generic administratively prohibited open failed", errors.New("ssh: rejected: administratively prohibited (open failed)")},
		{"generic administratively prohibited", errors.New("administratively prohibited by server policy")},
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

	// 4. Enable Dist -> host1 becomes eligible with count=2, op="status".
	h.srv.corrald.SetDist(makeFakeCorraldDist())
	h.srv.recordCorraldHintError(aliceID, host1, HintOpStatus, &ssh.OpenChannelError{Reason: ssh.Prohibited, Message: "no more sessions"})
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

// Item 2 regression test: in-memory failure record is bound to (address, port,
// login user); editing the host to another target invalidates old records, and
// a stale open session cannot attach failures to the new target (fails on 89c46aa).
func TestCorraldHintTargetBindingInvalidatesOnHostEdit(t *testing.T) {
	h := newHarness(t, func(c *config.Config) {
		c.GatewayID = "gw-hint-target.local"
	})
	h.srv.corrald.SetDist(makeFakeCorraldDist())
	admin := h.admin()
	alice := h.member(admin, "alice")
	aliceID := userIDOf(t, alice)

	hostID := createTestHostForHints(t, alice, "orig-box", 2231)
	h.srv.recordCorraldHintError(aliceID, hostID, HintOpStatus, sshx.ErrBusy)

	if got := alice.do("GET", "/api/corrald/hints", nil).json()["hint"]; got == nil {
		t.Fatal("expected hint on original target")
	}

	// 1. Renaming host (same address, port, username) keeps the hint valid.
	mustStatus(t, alice.do("PUT", fmt.Sprintf("/api/hosts/%d", hostID), map[string]any{
		"name":     "renamed-box",
		"address":  "127.0.0.1",
		"port":     2231,
		"username": "devuser",
		"auth":     "ask",
	}), http.StatusOK, "rename host only")
	got := alice.do("GET", "/api/corrald/hints", nil).json()["hint"].(map[string]any)
	if got == nil || got["host_name"] != "renamed-box" {
		t.Fatalf("expected hint to survive rename with updated host_name, got %+v", got)
	}

	// 2. Editing address invalidates the old record.
	mustStatus(t, alice.do("PUT", fmt.Sprintf("/api/hosts/%d", hostID), map[string]any{
		"name":     "renamed-box",
		"address":  "127.0.0.2",
		"port":     2231,
		"username": "devuser",
		"auth":     "ask",
	}), http.StatusOK, "change host address")
	if got := alice.do("GET", "/api/corrald/hints", nil).json()["hint"]; got != nil {
		t.Fatalf("changing address must invalidate old failure record, got %+v", got)
	}

	// 3. Editing port directly in store (even bypassing PUT handler) invalidates the record via Target check.
	h.srv.recordCorraldHintError(aliceID, hostID, HintOpStatus, sshx.ErrBusy)
	if got := alice.do("GET", "/api/corrald/hints", nil).json()["hint"]; got == nil {
		t.Fatal("expected hint on 127.0.0.2:2231")
	}
	curHost, err := h.store.Host(context.Background(), aliceID, hostID)
	if err != nil {
		t.Fatalf("store.Host: %v", err)
	}
	curHost.Port = 2299
	if err := h.store.UpdateHost(context.Background(), curHost, true); err != nil {
		t.Fatalf("UpdateHost port: %v", err)
	}
	if got := alice.do("GET", "/api/corrald/hints", nil).json()["hint"]; got != nil {
		t.Fatalf("changing port must invalidate old failure record, got %+v", got)
	}

	// 4. Editing login username invalidates the old record.
	h.srv.recordCorraldHintError(aliceID, hostID, HintOpStatus, sshx.ErrBusy)
	mustStatus(t, alice.do("PUT", fmt.Sprintf("/api/hosts/%d", hostID), map[string]any{
		"name":     "renamed-box",
		"address":  "127.0.0.2",
		"port":     2299,
		"username": "otheruser",
		"auth":     "ask",
	}), http.StatusOK, "change host username")
	if got := alice.do("GET", "/api/corrald/hints", nil).json()["hint"]; got != nil {
		t.Fatalf("changing username must invalidate old failure record, got %+v", got)
	}

	// 5. A stale running session still bound to the old HostConfig cannot attach
	//    failures to the retargeted host record.
	staleSess := &term.Session{
		OwnerID:    aliceID,
		HostID:     hostID,
		HostConfig: "1|127.0.0.1|2231|devuser|ask|0",
	}
	h.srv.recordCorraldHintSessionError(staleSess, HintOpLog, sshx.ErrBusy)
	if got := alice.do("GET", "/api/corrald/hints", nil).json()["hint"]; got != nil {
		t.Fatalf("stale session error for old target must not show on retargeted host, got %+v", got)
	}
}

// Items 3, 4, 5 regression tests:
//   - Item 3: true->true PATCH is a no-op (does not reset shown/dismissed hosts).
//   - Item 4: stale generation shown/dismiss POSTs return 204 and do not write.
//   - Item 5: false->true atomically writes corraldHintsResetAt in the same
//     settings JSON/transaction without deleting corrald_host_hints rows, and
//     corraldHintsResetAt is rejected if sent by a client.
func TestCorraldHintResetGenerationTrueToTrueStalePostAndAtomicNoDelete(t *testing.T) {
	h := newHarness(t, func(c *config.Config) {
		c.GatewayID = "gw-hint-gen.local"
	})
	h.srv.corrald.SetDist(makeFakeCorraldDist())
	admin := h.admin()
	alice := h.member(admin, "alice")
	aliceID := userIDOf(t, alice)

	host1 := createTestHostForHints(t, alice, "box-one", 2211)
	host2 := createTestHostForHints(t, alice, "box-two", 2212)

	now := time.UnixMilli(1_700_100_000_000)
	h.srv.corraldHints.now = func() time.Time { return now }

	h.srv.recordCorraldHintError(aliceID, host1, HintOpStatus, sshx.ErrBusy)
	now = now.Add(5 * time.Second)
	h.srv.recordCorraldHintError(aliceID, host2, HintOpLog, sshx.ErrBusy)

	// Initial GET returns generation 0 and host2 (most recent).
	r := alice.do("GET", "/api/corrald/hints", nil)
	mustStatus(t, r, http.StatusOK, "initial hints")
	gen0 := int64(r.json()["generation"].(float64))
	if gen0 != 0 {
		t.Fatalf("expected initial generation=0, got %d", gen0)
	}
	if int64(r.json()["hint"].(map[string]any)["host_id"].(float64)) != host2 {
		t.Fatalf("expected host2 first, got %+v", r.json())
	}

	// Claim host2 shown and mark host1 dismissed under gen0.
	rClaim0 := alice.do("POST", fmt.Sprintf("/api/hosts/%d/corrald/hint/shown", host2), map[string]any{"generation": gen0})
	mustStatus(t, rClaim0, http.StatusOK, "claim host2")
	if rClaim0.json()["claimed"] != true {
		t.Fatalf("expected claimed=true for host2 under gen0, got %+v", rClaim0.json())
	}
	mustStatus(t, alice.do("POST", fmt.Sprintf("/api/hosts/%d/corrald/hint/dismiss", host1), map[string]any{"generation": gen0}), http.StatusOK, "dismiss host1")
	// Dismissed host1 is omitted; claimed (not dismissed) host2 stays reported with claimed=true while still eligible.
	if got := alice.do("GET", fmt.Sprintf("/api/corrald/hints?host_id=%d", host1), nil).json()["hint"]; got != nil {
		t.Fatalf("expected nil hint for dismissed host1, got %+v", got)
	}
	gotClaimed0, ok := alice.do("GET", "/api/corrald/hints", nil).json()["hint"].(map[string]any)
	if !ok || int64(gotClaimed0["host_id"].(float64)) != host2 || gotClaimed0["claimed"] != true {
		t.Fatalf("expected claimed=true hint for host2 while still eligible, got %+v", gotClaimed0)
	}

	// Item 3: true -> true (from default true, or repeated true from another writer) MUST BE A NO-OP!
	mustStatus(t, patchUserSettings(t, alice, aliceID, "writer_true_noop_1", 1, map[string]any{"corraldHints": true}), http.StatusOK, "default true->true")
	mustStatus(t, patchUserSettings(t, alice, aliceID, "writer_true_noop_2", 1, map[string]any{"corraldHints": true}), http.StatusOK, "explicit true->true")
	rAfterNoOp := alice.do("GET", "/api/corrald/hints", nil)
	mustStatus(t, rAfterNoOp, http.StatusOK, "hints after true->true")
	if gotNoOp, ok := rAfterNoOp.json()["hint"].(map[string]any); !ok || int64(gotNoOp["host_id"].(float64)) != host2 || gotNoOp["claimed"] != true {
		t.Fatalf("Item 3 regression: true->true PATCH must keep host2 claimed=true (not unclaimed), got %+v", rAfterNoOp.json())
	}
	if gotH1 := alice.do("GET", fmt.Sprintf("/api/corrald/hints?host_id=%d", host1), nil).json()["hint"]; gotH1 != nil {
		t.Fatalf("Item 3 regression: true->true PATCH must not restore dismissed host1, got %+v", gotH1)
	}
	if int64(rAfterNoOp.json()["generation"].(float64)) != 0 {
		t.Fatalf("Item 3 regression: true->true PATCH must not advance generation, got %v", rAfterNoOp.json()["generation"])
	}

	// Item 5: turn OFF (false), then ON (true) -> sets monotonic corraldHintsResetAt
	// in the SAME settings JSON write WITHOUT deleting corrald_host_hints rows.
	writer := "writer_reset_test_01"
	mustStatus(t, patchUserSettings(t, alice, aliceID, writer, 1, map[string]any{"corraldHints": false}), http.StatusOK, "toggle off")
	if got := alice.do("GET", "/api/corrald/hints", nil).json()["hint"]; got != nil {
		t.Fatalf("expected nil hint when off, got %+v", got)
	}

	// Stale superseded seq=1 with true must NOT advance generation or re-enable.
	mustStatus(t, patchUserSettings(t, alice, aliceID, writer, 1, map[string]any{"corraldHints": true}), http.StatusOK, "superseded true")
	if got := alice.do("GET", "/api/corrald/hints", nil).json()["hint"]; got != nil {
		t.Fatalf("superseded patch must not re-enable hints: %+v", got)
	}

	// Real false -> true transition (seq=2).
	mustStatus(t, patchUserSettings(t, alice, aliceID, writer, 2, map[string]any{"corraldHints": true}), http.StatusOK, "toggle back on")

	// DB rows in corrald_host_hints MUST still exist (nothing deleted).
	dbRows, err := h.store.ListCorraldHostHints(context.Background(), aliceID)
	if err != nil {
		t.Fatalf("ListCorraldHostHints: %v", err)
	}
	if len(dbRows) != 2 {
		t.Fatalf("Item 5 regression: reset must NOT delete corrald_host_hints rows; found %d", len(dbRows))
	}

	// user_settings JSON contains corraldHintsResetAt, while GET /api/me/settings hides it.
	rawSettings, err := h.store.UserSettings(context.Background(), aliceID)
	if err != nil {
		t.Fatalf("UserSettings: %v", err)
	}
	var rawObj map[string]json.RawMessage
	if err := json.Unmarshal([]byte(rawSettings), &rawObj); err != nil {
		t.Fatalf("unmarshal raw settings: %v", err)
	}
	if _, ok := rawObj[store.CorraldHintsResetKey]; !ok {
		t.Fatalf("expected %s in stored user_settings JSON: %s", store.CorraldHintsResetKey, rawSettings)
	}
	meSettings := alice.do("GET", "/api/me/settings", nil).json()
	if _, leaked := meSettings[store.CorraldHintsResetKey]; leaked {
		t.Fatalf("GET /api/me/settings must not expose server-only key %s", store.CorraldHintsResetKey)
	}

	// GET /api/corrald/hints returns new generation gen1 > 0 and both hosts are eligible again.
	rGen1 := alice.do("GET", "/api/corrald/hints", nil)
	mustStatus(t, rGen1, http.StatusOK, "hints after reset")
	gen1 := int64(rGen1.json()["generation"].(float64))
	if gen1 <= gen0 {
		t.Fatalf("expected gen1 > gen0 (%d), got %d", gen0, gen1)
	}
	if rGen1.json()["hint"] == nil {
		t.Fatal("expected restored hint after false->true reset")
	}
	if alice.do("GET", fmt.Sprintf("/api/corrald/hints?host_id=%d", host1), nil).json()["hint"] == nil {
		t.Fatal("expected previously dismissed host1 to be restored")
	}

	// Item 4: stale POSTs carrying gen0 (rendered before the reset) MUST NOT
	// write or suppress the restored host: shown returns 200 {claimed:false},
	// dismiss returns 204 No Content.
	staleShown := alice.do("POST", fmt.Sprintf("/api/hosts/%d/corrald/hint/shown", host2), map[string]any{"generation": gen0})
	mustStatus(t, staleShown, http.StatusOK, "stale shown returns 200 claimed:false")
	if staleShown.json()["claimed"] != false {
		t.Fatalf("expected stale shown claimed=false, got %+v", staleShown.json())
	}
	staleDismiss := alice.do("POST", fmt.Sprintf("/api/hosts/%d/corrald/hint/dismiss", host1), map[string]any{"generation": gen0})
	mustStatus(t, staleDismiss, http.StatusNoContent, "stale dismiss returns 204")
	// Both hosts MUST still be eligible and unclaimed!
	if h2 := alice.do("GET", fmt.Sprintf("/api/corrald/hints?host_id=%d", host2), nil).json()["hint"].(map[string]any); h2 == nil || h2["claimed"] != false {
		t.Fatalf("Item 4 regression: stale shown POST suppressed restored host2: %+v", h2)
	}
	if h1 := alice.do("GET", fmt.Sprintf("/api/corrald/hints?host_id=%d", host1), nil).json()["hint"].(map[string]any); h1 == nil || h1["claimed"] != false {
		t.Fatalf("Item 4 regression: stale dismiss POST suppressed restored host1: %+v", h1)
	}

	// Valid POST carrying gen1 succeeds (200 {claimed:true}); GET now reports host2 as claimed=true.
	rClaim1 := alice.do("POST", fmt.Sprintf("/api/hosts/%d/corrald/hint/shown", host2), map[string]any{"generation": gen1})
	mustStatus(t, rClaim1, http.StatusOK, "gen1 claim host2")
	if rClaim1.json()["claimed"] != true {
		t.Fatalf("expected claimed=true under gen1, got %+v", rClaim1.json())
	}
	h2AfterGen1, ok := alice.do("GET", fmt.Sprintf("/api/corrald/hints?host_id=%d", host2), nil).json()["hint"].(map[string]any)
	if !ok || h2AfterGen1["claimed"] != true {
		t.Fatalf("host2 must be marked claimed=true after gen1 shown claim, got %+v", h2AfterGen1)
	}

	// Second false->true reset even at the exact same frozen millisecond advances generation monotonically.
	mustStatus(t, patchUserSettings(t, alice, aliceID, writer, 3, map[string]any{"corraldHints": false}), http.StatusOK, "second off")
	mustStatus(t, patchUserSettings(t, alice, aliceID, writer, 4, map[string]any{"corraldHints": true}), http.StatusOK, "second on same ms")
	gen2 := int64(alice.do("GET", "/api/corrald/hints", nil).json()["generation"].(float64))
	if gen2 <= gen1 {
		t.Fatalf("expected strictly monotonic gen2 > gen1 (%d), got %d", gen1, gen2)
	}
	if h2Gen2, ok := alice.do("GET", fmt.Sprintf("/api/corrald/hints?host_id=%d", host2), nil).json()["hint"].(map[string]any); !ok || h2Gen2["claimed"] != false {
		t.Fatalf("host2 should be unclaimed again after second reset, got %+v", h2Gen2)
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

	// Unknown keys AND server-written corraldHintsResetAt are rejected by whitelist.
	for i, badKey := range []string{store.CorraldHintsResetKey, "corrald_hints", "corraldHintsExtra", "arbitraryKey"} {
		r := patchUserSettings(t, alice, aliceID, writer, int64(20+i), map[string]any{badKey: 12345})
		mustStatus(t, r, http.StatusBadRequest, "rejected key "+badKey)
		if r.code() != "bad_setting" {
			t.Fatalf("expected bad_setting for key %q, got %q", badKey, r.code())
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
	mustStatus(t, alice.do("POST", fmt.Sprintf("/api/hosts/%d/corrald/hint/dismiss", host1), map[string]any{"generation": 0}), http.StatusOK, "dismiss host1")
	mustStatus(t, alice.do("POST", fmt.Sprintf("/api/hosts/%d/corrald/hint/shown", host2), map[string]any{"generation": 0}), http.StatusOK, "shown host2")

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
	mustStatus(t, alice.do("PUT", fmt.Sprintf("/api/hosts/%d", hostID), map[string]any{
		"name":     "busy-box",
		"address":  "127.0.0.1",
		"port":     srv.Addr.Port,
		"username": "alice",
		"auth":     "password",
		"password": "alice-pass",
	}), http.StatusOK, "update host auth")

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

// Phase 2b fix2: two (and many) concurrent claims for the same host &
// generation must return claimed=true to exactly one caller and claimed=false
// to every other caller.
func TestCorraldHintConcurrentClaimsExactlyOneWins(t *testing.T) {
	h := newHarness(t, func(c *config.Config) {
		c.GatewayID = "gw-hint-race.local"
	})
	h.srv.corrald.SetDist(makeFakeCorraldDist())
	admin := h.admin()
	alice := h.member(admin, "alice")
	aliceID := userIDOf(t, alice)

	hostID := createTestHostForHints(t, alice, "race-box", 2241)
	h.srv.recordCorraldHintError(aliceID, hostID, HintOpStatus, sshx.ErrBusy)

	runConcurrentClaims := func(gen int64, n int) {
		t.Helper()
		start := make(chan struct{})
		var wins atomic.Int32
		var fails atomic.Int32
		var wg sync.WaitGroup
		errCh := make(chan string, n)
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				r := alice.do("POST", fmt.Sprintf("/api/hosts/%d/corrald/hint/shown", hostID), map[string]any{"generation": gen})
				if r.Status != http.StatusOK {
					errCh <- fmt.Sprintf("status=%d body=%s", r.Status, string(r.Body))
					return
				}
				claimed, ok := r.json()["claimed"].(bool)
				if !ok {
					errCh <- fmt.Sprintf("missing bool claimed in %+v", r.json())
					return
				}
				if claimed {
					wins.Add(1)
				} else {
					fails.Add(1)
				}
			}()
		}
		close(start)
		wg.Wait()
		close(errCh)
		for msg := range errCh {
			t.Fatalf("concurrent claim failed: %s", msg)
		}
		if wins.Load() != 1 || fails.Load() != int32(n-1) {
			t.Fatalf("gen=%d n=%d: got wins=%d fails=%d, want 1 and %d", gen, n, wins.Load(), fails.Load(), n-1)
		}
	}

	// Two tabs racing under gen 0.
	runConcurrentClaims(0, 2)
	gotAfterWin, ok := alice.do("GET", "/api/corrald/hints", nil).json()["hint"].(map[string]any)
	if !ok || int64(gotAfterWin["host_id"].(float64)) != hostID || gotAfterWin["claimed"] != true {
		t.Fatalf("expected claimed=true hint after winning claim while host stays eligible, got %+v", gotAfterWin)
	}

	// Reset off -> on, then race 6 concurrent tabs under gen 1.
	mustStatus(t, patchUserSettings(t, alice, aliceID, "writer_race_test_01", 1, map[string]any{"corraldHints": false}), http.StatusOK, "off")
	mustStatus(t, patchUserSettings(t, alice, aliceID, "writer_race_test_01", 2, map[string]any{"corraldHints": true}), http.StatusOK, "on")
	gen1 := int64(alice.do("GET", "/api/corrald/hints", nil).json()["generation"].(float64))
	if gen1 <= 0 {
		t.Fatalf("expected gen1 > 0, got %d", gen1)
	}
	runConcurrentClaims(gen1, 6)
}

// Phase 2b fix2: POST /api/hosts/{id}/corrald/hint/shown must refuse with
// 200 {"claimed": false} (and write no shown_at) for every ineligible reason.
func TestCorraldHintClaimRefusedWhenIneligibleForEachReason(t *testing.T) {
	h := newHarness(t, func(c *config.Config) {
		c.GatewayID = "gw-hint-claim-elig.local"
	})
	h.srv.corrald.SetDist(makeFakeCorraldDist())
	admin := h.admin()
	alice := h.member(admin, "alice")
	bob := h.member(admin, "bob")
	aliceID := userIDOf(t, alice)

	hostID := createTestHostForHints(t, alice, "elig-box", 2251)
	hostDismiss := createTestHostForHints(t, alice, "dismiss-box", 2252)
	ctx := context.Background()

	assertClaimed := func(label string, targetHost int64, gen int64, want bool) {
		t.Helper()
		r := alice.do("POST", fmt.Sprintf("/api/hosts/%d/corrald/hint/shown", targetHost), map[string]any{"generation": gen})
		mustStatus(t, r, http.StatusOK, label)
		got, ok := r.json()["claimed"].(bool)
		if !ok || got != want {
			t.Fatalf("%s: got claimed=%v (ok=%v), want %v (body=%s)", label, got, ok, want, string(r.Body))
		}
	}

	// Cross-user access still returns 404.
	mustStatus(t, bob.do("POST", fmt.Sprintf("/api/hosts/%d/corrald/hint/shown", hostID), map[string]any{"generation": 0}), http.StatusNotFound, "bob cross-user claim")
	// Negative generation returns 400.
	mustStatus(t, alice.do("POST", fmt.Sprintf("/api/hosts/%d/corrald/hint/shown", hostID), map[string]any{"generation": -1}), http.StatusBadRequest, "negative gen claim")

	// 1. No failure record yet -> claimed:false, no DB row written.
	assertClaimed("no failure record", hostID, 0, false)
	if _, err := h.store.CorraldHostHint(ctx, aliceID, hostID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("refused claim (no failure) must not write DB row, got %v", err)
	}

	// Record real failure on hostID.
	h.srv.recordCorraldHintError(aliceID, hostID, HintOpStatus, sshx.ErrBusy)

	// 2. Failure record bound to a different target (edited port in store) -> claimed:false.
	curHost, err := h.store.Host(ctx, aliceID, hostID)
	if err != nil {
		t.Fatalf("store.Host: %v", err)
	}
	curHost.Port = 2299
	if err := h.store.UpdateHost(ctx, curHost, true); err != nil {
		t.Fatalf("UpdateHost port: %v", err)
	}
	assertClaimed("mismatched target", hostID, 0, false)
	// Restore failure for updated target.
	h.srv.recordCorraldHintError(aliceID, hostID, HintOpStatus, sshx.ErrBusy)

	// 3. Corrald unavailable -> claimed:false.
	h.srv.corrald.SetDist(nil)
	assertClaimed("corrald unavailable", hostID, 0, false)
	h.srv.corrald.SetDist(makeFakeCorraldDist())

	// 4. Admin disabled -> claimed:false.
	mustStatus(t, admin.do("PUT", "/api/admin/corrald", map[string]any{"allowed": false}), http.StatusOK, "admin off")
	assertClaimed("admin disabled", hostID, 0, false)
	mustStatus(t, admin.do("PUT", "/api/admin/corrald", map[string]any{"allowed": true}), http.StatusOK, "admin on")

	// 5. Master switch off -> claimed:false.
	mustStatus(t, patchUserSettings(t, alice, aliceID, "writer_elig_test_01", 1, map[string]any{"corraldHints": false}), http.StatusOK, "switch off")
	assertClaimed("switch off", hostID, 0, false)
	mustStatus(t, patchUserSettings(t, alice, aliceID, "writer_elig_test_01", 2, map[string]any{"corraldHints": true}), http.StatusOK, "switch back on")
	gen1 := int64(alice.do("GET", "/api/corrald/hints", nil).json()["generation"].(float64))

	// 6. Stale generation -> claimed:false.
	assertClaimed("stale generation", hostID, 0, false)

	// 7. Non-off daemon states (installing, running, rebind, incompatible, etc.) -> claimed:false.
	fp := "SHA256:claim-elig-fp"
	if err := h.store.PinHostKey(ctx, aliceID, hostID, &store.HostKey{
		Fingerprint: fp, KeyType: "ssh-ed25519", KeyData: []byte("pk"),
	}); err != nil {
		t.Fatalf("PinHostKey: %v", err)
	}
	if err := h.store.RecordSSHVerified(ctx, hostID, fp, "devuser"); err != nil {
		t.Fatalf("RecordSSHVerified: %v", err)
	}
	for _, stName := range []string{gw.StateInstalling, gw.StateRunning, gw.StateRebind, gw.StateIncompatible, gw.StatePaused} {
		paused := []int64{}
		dbState := stName
		if stName == gw.StatePaused {
			dbState = gw.StateRunning
			paused = []int64{aliceID}
		}
		if err := h.store.UpsertCorraldHost(ctx, &store.CorraldHost{
			HostID:           hostID,
			State:            dbState,
			BoundFingerprint: fp,
			BoundUsername:    "devuser",
			BoundGateway:     h.cfg.GatewayID,
			PausedUsers:      paused,
		}); err != nil {
			t.Fatalf("UpsertCorraldHost(%s): %v", stName, err)
		}
		assertClaimed("state "+stName, hostID, gen1, false)
	}

	// 8. StateOff with unsupported_os failed install op -> claimed:false.
	if err := h.store.UpsertCorraldHost(ctx, &store.CorraldHost{HostID: hostID, State: gw.StateOff}); err != nil {
		t.Fatalf("reset host off: %v", err)
	}
	if err := h.store.CreateCorraldOp(ctx, &store.CorraldOp{
		ID: "op-claim-incompat", HostID: hostID, Kind: gw.OpInstall, State: gw.OpFailed,
		ErrorCode: "unsupported_os", CreatedBy: aliceID, CreatedAt: time.Now().Unix(), UpdatedAt: time.Now().Unix(),
	}, false); err != nil {
		t.Fatalf("CreateCorraldOp unsupported_os: %v", err)
	}
	assertClaimed("unsupported_os", hostID, gen1, false)

	// None of the above refusals should have created a DB row for hostID!
	if _, err := h.store.CorraldHostHint(ctx, aliceID, hostID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("refused claims must not persist shown_at, got %v", err)
	}

	// 9. Already dismissed -> claimed:false.
	h.srv.recordCorraldHintError(aliceID, hostDismiss, HintOpLog, sshx.ErrBusy)
	mustStatus(t, alice.do("POST", fmt.Sprintf("/api/hosts/%d/corrald/hint/dismiss", hostDismiss), map[string]any{"generation": gen1}), http.StatusOK, "dismiss hostDismiss")
	assertClaimed("already dismissed", hostDismiss, gen1, false)

	// 10. Clean eligible host -> first claim succeeds, second claim returns claimed:false.
	hostClean := createTestHostForHints(t, alice, "clean-box", 2253)
	h.srv.recordCorraldHintError(aliceID, hostClean, HintOpStatus, sshx.ErrBusy)
	assertClaimed("first valid claim", hostClean, gen1, true)
	assertClaimed("already shown", hostClean, gen1, false)
}

type blockingHintSSHProvider struct {
	inner   gw.SSHProvider
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (p *blockingHintSSHProvider) AcquireSSH(ctx context.Context, userID, hostID int64) (*ssh.Client, func(), string, string, error) {
	return p.inner.AcquireSSH(ctx, userID, hostID)
}

func (p *blockingHintSSHProvider) HostBinding(ctx context.Context, userID, hostID int64) (string, string, error) {
	p.once.Do(func() {
		close(p.entered)
		<-p.release
	})
	return p.inner.HostBinding(ctx, userID, hostID)
}

func (p *blockingHintSSHProvider) UserLabel(ctx context.Context, userID int64) string {
	return p.inner.UserLabel(ctx, userID)
}

// Phase 2b fix3 item 2 regression test (fails on 87306f4):
// If the host is retargeted, the failure record is cleared, or admin disables
// corrald while Status() is running, POST /hint/shown must re-check eligibility
// before writing shown_at and return 200 {"claimed": false} without persisting.
func TestCorraldHintClaimRevalidatesAfterStatusLookup(t *testing.T) {
	for _, kind := range []string{"retarget_via_api", "retarget_in_store", "admin_disable"} {
		t.Run(kind, func(t *testing.T) {
			h := newHarness(t, func(c *config.Config) {
				c.GatewayID = "gw-hint-reval-" + kind + ".local"
			})
			admin := h.admin()
			uid := userIDOf(t, admin)
			hid := createTestHostForHints(t, admin, "reval-box", 2291)
			h.srv.recordCorraldHintError(uid, hid, HintOpStatus, sshx.ErrBusy)

			gate := &blockingHintSSHProvider{
				inner:   &webSSHProvider{s: h.srv},
				entered: make(chan struct{}),
				release: make(chan struct{}),
			}
			h.srv.corrald.Close()
			h.srv.corrald = gw.NewManager(gw.ManagerConfig{
				Store:     h.store,
				Dist:      makeFakeCorraldDist(),
				GatewayID: h.cfg.GatewayID,
				SSH:       gate,
			})

			done := make(chan resp, 1)
			go func() {
				done <- admin.do("POST", fmt.Sprintf("/api/hosts/%d/corrald/hint/shown", hid), map[string]any{"generation": 0})
			}()

			select {
			case <-gate.entered:
			case <-time.After(5 * time.Second):
				t.Fatal("claim did not enter Status/HostBinding")
			}

			switch kind {
			case "retarget_via_api":
				mustStatus(t, admin.do("PUT", fmt.Sprintf("/api/hosts/%d", hid), map[string]any{
					"name":     "retargeted-box",
					"address":  "127.0.0.2",
					"port":     2291,
					"username": "devuser",
					"auth":     "ask",
				}), http.StatusOK, "retarget host")
			case "retarget_in_store":
				cur, err := h.store.Host(context.Background(), uid, hid)
				if err != nil {
					t.Fatalf("store.Host: %v", err)
				}
				cur.Port = 2299
				if err := h.store.UpdateHost(context.Background(), cur, true); err != nil {
					t.Fatalf("UpdateHost: %v", err)
				}
			case "admin_disable":
				mustStatus(t, admin.do("PUT", "/api/admin/corrald", map[string]any{"allowed": false}), http.StatusOK, "admin disable")
			}

			close(gate.release)
			r := <-done
			mustStatus(t, r, http.StatusOK, "claim result")
			if got, ok := r.json()["claimed"].(bool); !ok || got {
				t.Fatalf("%s: expected claimed=false, got body=%s", kind, string(r.Body))
			}
			if _, err := h.store.CorraldHostHint(context.Background(), uid, hid); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("%s: refused claim must not persist shown_at, got err=%v", kind, err)
			}
		})
	}
}

// Phase 2b fix4/fix5 regression test (fails on 66e0bdb and 1f4cf63):
// GET /api/corrald/hints must:
//   - Always return a claimed (not dismissed) still-eligible host (claimed:true)
//     BEFORE any unclaimed host (if several are claimed, the most recently claimed).
//   - Only when no such claimed host exists, return the best unclaimed eligible host (claimed:false).
//   - Omit a claimed host as soon as eligibility ends (daemon state, admin disable,
//     switch off, retarget, or dismiss).
func TestCorraldHintClaimedReportedWhileEligibleAndClearedWhenIneligible(t *testing.T) {
	h := newHarness(t, func(c *config.Config) {
		c.GatewayID = "gw-hint-fix5.local"
	})
	h.srv.corrald.SetDist(makeFakeCorraldDist())
	admin := h.admin()
	alice := h.member(admin, "alice")
	aliceID := userIDOf(t, alice)
	ctx := context.Background()

	host1 := createTestHostForHints(t, alice, "claimed-box", 2261)
	host2 := createTestHostForHints(t, alice, "unclaimed-box", 2262)

	now := time.Unix(1_700_200_000, 0)
	h.srv.corraldHints.now = func() time.Time { return now }
	h.srv.recordCorraldHintError(aliceID, host1, HintOpStatus, sshx.ErrBusy)

	// Before claim: GET returns host1 with claimed:false.
	r0 := alice.do("GET", "/api/corrald/hints", nil)
	mustStatus(t, r0, http.StatusOK, "pre-claim GET")
	hint0, ok := r0.json()["hint"].(map[string]any)
	if !ok || int64(hint0["host_id"].(float64)) != host1 || hint0["claimed"] != false {
		t.Fatalf("expected unclaimed host1 before claim, got %+v", r0.json())
	}

	// Claim host1 -> claimed:true.
	rClaim := alice.do("POST", fmt.Sprintf("/api/hosts/%d/corrald/hint/shown", host1), map[string]any{"generation": 0})
	mustStatus(t, rClaim, http.StatusOK, "claim host1")
	if rClaim.json()["claimed"] != true {
		t.Fatalf("expected claimed=true, got %+v", rClaim.json())
	}

	// Several subsequent normal polls keep returning host1 with claimed:true!
	for i := 0; i < 3; i++ {
		rPoll := alice.do("GET", "/api/corrald/hints", nil)
		mustStatus(t, rPoll, http.StatusOK, "normal poll after claim")
		hintPoll, ok := rPoll.json()["hint"].(map[string]any)
		if !ok || int64(hintPoll["host_id"].(float64)) != host1 || hintPoll["claimed"] != true {
			t.Fatalf("poll %d: expected host1 with claimed=true, got %+v", i, rPoll.json())
		}
	}

	// Fix5 (fails on 1f4cf63): even when another eligible unclaimed host2 has a
	// newer failure timestamp, GET MUST keep returning claimed host1 (claimed:true)
	// before any unclaimed host.
	now = now.Add(10 * time.Second)
	h.srv.recordCorraldHintError(aliceID, host2, HintOpLog, sshx.ErrBusy)
	for i := 0; i < 3; i++ {
		rKeepClaimed := alice.do("GET", "/api/corrald/hints", nil)
		mustStatus(t, rKeepClaimed, http.StatusOK, "claimed beats unclaimed")
		hintC, ok := rKeepClaimed.json()["hint"].(map[string]any)
		if !ok || int64(hintC["host_id"].(float64)) != host1 || hintC["claimed"] != true {
			t.Fatalf("fix5 poll %d: expected claimed host1 before unclaimed host2, got %+v", i, rKeepClaimed.json())
		}
	}

	// When host1 becomes temporarily ineligible (installing), GET falls through to
	// unclaimed host2 (claimed:false).
	if err := h.store.UpsertCorraldHost(ctx, &store.CorraldHost{HostID: host1, State: gw.StateInstalling}); err != nil {
		t.Fatalf("UpsertCorraldHost installing: %v", err)
	}
	rFallback := alice.do("GET", "/api/corrald/hints", nil)
	mustStatus(t, rFallback, http.StatusOK, "unclaimed host2 while host1 installing")
	hintU, ok := rFallback.json()["hint"].(map[string]any)
	if !ok || int64(hintU["host_id"].(float64)) != host2 || hintU["claimed"] != false {
		t.Fatalf("expected unclaimed host2 while host1 installing, got %+v", rFallback.json())
	}

	// Claim host2 AFTER host1 (so host2 has a newer shown_at), then make host1 off
	// again with an even newer failure LastAt: among multiple claimed hosts, GET
	// must return the most recently CLAIMED one (host2)!
	time.Sleep(2 * time.Millisecond) // ensure strictly greater unixMilli shown_at
	rClaim2 := alice.do("POST", fmt.Sprintf("/api/hosts/%d/corrald/hint/shown", host2), map[string]any{"generation": 0})
	mustStatus(t, rClaim2, http.StatusOK, "claim host2")
	if rClaim2.json()["claimed"] != true {
		t.Fatalf("expected claimed=true for host2, got %+v", rClaim2.json())
	}
	if err := h.store.UpsertCorraldHost(ctx, &store.CorraldHost{HostID: host1, State: gw.StateOff}); err != nil {
		t.Fatalf("reset host1 off: %v", err)
	}
	now = now.Add(30 * time.Second)
	h.srv.recordCorraldHintError(aliceID, host1, HintOpStatus, sshx.ErrBusy) // host1 has newer LastAt than host2
	rMostRecentClaimed := alice.do("GET", "/api/corrald/hints", nil)
	mustStatus(t, rMostRecentClaimed, http.StatusOK, "most recently claimed host")
	hintMRC, ok := rMostRecentClaimed.json()["hint"].(map[string]any)
	if !ok || int64(hintMRC["host_id"].(float64)) != host2 || hintMRC["claimed"] != true {
		t.Fatalf("fix5: expected most recently claimed host2 (even when host1 has newer LastAt), got %+v", rMostRecentClaimed.json())
	}

	// Dismiss host2 -> GET returns remaining claimed host1.
	mustStatus(t, alice.do("POST", fmt.Sprintf("/api/hosts/%d/corrald/hint/dismiss", host2), map[string]any{"generation": 0}), http.StatusOK, "dismiss host2")
	hintBack, ok := alice.do("GET", "/api/corrald/hints", nil).json()["hint"].(map[string]any)
	if !ok || int64(hintBack["host_id"].(float64)) != host1 || hintBack["claimed"] != true {
		t.Fatalf("expected fallback to claimed host1 after host2 dismissed, got %+v", hintBack)
	}

	// 1. Daemon becomes installing -> GET stops returning claimed host1.
	if err := h.store.UpsertCorraldHost(ctx, &store.CorraldHost{HostID: host1, State: gw.StateInstalling}); err != nil {
		t.Fatalf("UpsertCorraldHost installing: %v", err)
	}
	if got := alice.do("GET", "/api/corrald/hints", nil).json()["hint"]; got != nil {
		t.Fatalf("installing host must omit claimed hint, got %+v", got)
	}
	if err := h.store.UpsertCorraldHost(ctx, &store.CorraldHost{HostID: host1, State: gw.StateOff}); err != nil {
		t.Fatalf("reset off: %v", err)
	}

	// 2. Admin disables corrald -> GET returns null.
	mustStatus(t, admin.do("PUT", "/api/admin/corrald", map[string]any{"allowed": false}), http.StatusOK, "admin disable")
	if got := alice.do("GET", "/api/corrald/hints", nil).json()["hint"]; got != nil {
		t.Fatalf("admin disable must omit claimed hint, got %+v", got)
	}
	mustStatus(t, admin.do("PUT", "/api/admin/corrald", map[string]any{"allowed": true}), http.StatusOK, "admin allow")

	// 3. Master switch off -> GET returns null.
	mustStatus(t, patchUserSettings(t, alice, aliceID, "writer_fix5_test_01", 1, map[string]any{"corraldHints": false}), http.StatusOK, "switch off")
	if got := alice.do("GET", "/api/corrald/hints", nil).json()["hint"]; got != nil {
		t.Fatalf("switch off must omit claimed hint, got %+v", got)
	}
	// Turning switch back on advances generation -> host1 is unclaimed again!
	mustStatus(t, patchUserSettings(t, alice, aliceID, "writer_fix5_test_01", 2, map[string]any{"corraldHints": true}), http.StatusOK, "switch on")
	rReset := alice.do("GET", "/api/corrald/hints", nil)
	gen1 := int64(rReset.json()["generation"].(float64))
	hintReset, ok := rReset.json()["hint"].(map[string]any)
	if !ok || int64(hintReset["host_id"].(float64)) != host1 || hintReset["claimed"] != false {
		t.Fatalf("after reset host1 should be unclaimed again, got %+v", rReset.json())
	}

	// Re-claim under gen1, then verify dismiss and retargeting omit the claimed host.
	mustStatus(t, alice.do("POST", fmt.Sprintf("/api/hosts/%d/corrald/hint/shown", host1), map[string]any{"generation": gen1}), http.StatusOK, "claim gen1")
	if got := alice.do("GET", fmt.Sprintf("/api/corrald/hints?host_id=%d", host1), nil).json()["hint"].(map[string]any); got["claimed"] != true {
		t.Fatalf("expected host1 claimed=true under gen1, got %+v", got)
	}
	mustStatus(t, alice.do("POST", fmt.Sprintf("/api/hosts/%d/corrald/hint/dismiss", host1), map[string]any{"generation": gen1}), http.StatusOK, "dismiss gen1")
	if got := alice.do("GET", fmt.Sprintf("/api/corrald/hints?host_id=%d", host1), nil).json()["hint"]; got != nil {
		t.Fatalf("dismissed host1 must omit hint, got %+v", got)
	}
	// Claim restored host2 under gen1, then retarget it -> GET returns null.
	mustStatus(t, alice.do("POST", fmt.Sprintf("/api/hosts/%d/corrald/hint/shown", host2), map[string]any{"generation": gen1}), http.StatusOK, "claim host2 gen1")
	if got := alice.do("GET", "/api/corrald/hints", nil).json()["hint"].(map[string]any); int64(got["host_id"].(float64)) != host2 || got["claimed"] != true {
		t.Fatalf("expected claimed host2 under gen1, got %+v", got)
	}
	mustStatus(t, alice.do("PUT", fmt.Sprintf("/api/hosts/%d", host2), map[string]any{
		"name":     "retargeted-box",
		"address":  "127.0.0.2",
		"port":     2262,
		"username": "devuser",
		"auth":     "ask",
	}), http.StatusOK, "retarget claimed host2")
	if got := alice.do("GET", "/api/corrald/hints", nil).json()["hint"]; got != nil {
		t.Fatalf("retargeted claimed host2 must omit hint, got %+v", got)
	}
}
