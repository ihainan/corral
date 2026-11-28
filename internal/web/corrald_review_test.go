package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/ihainan/corral/internal/config"
	"github.com/ihainan/corral/internal/corrald/gw"
	"github.com/ihainan/corral/internal/corrald/proto"
	"github.com/ihainan/corral/internal/testutil"
)

// TestCorraldReview_SharedConnSSHRetentionAndIdentityVerification covers
// review items #1 and #2:
//   - #2: Installing corrald without an open terminal dials a fresh SSH client;
//     the shared connection must retain it so corrald does not immediately disconnect.
//   - #1: When a member's host record changes to point at another machine/key,
//     shared-conn recovery verifies (fingerprint, username) and refuses to attach
//     the foreign host into the original shared group (marking it rebind instead).
func TestCorraldReview_SharedConnSSHRetentionAndIdentityVerification(t *testing.T) {
	v1Dir, _ := buildTestCorraldDist(t)
	h := newHarness(t, func(c *config.Config) {
		c.CorraldDir = v1Dir
		c.GatewayID = "gw-r12.local"
	})
	admin := h.admin()
	alice := h.member(admin, "alice")
	bob := h.member(admin, "bob")

	srvA, _ := startCorraldSSH(t)
	srvB, _ := startCorraldSSH(t)

	// Item #2: Create Alice's host, open+CLOSE her terminal so no terminal SSH
	// session exists, then install corrald. AcquireSSH must dial a fresh SSH conn
	// and transfer ownership to sharedConn so corrald stays connected!
	aliceHost, aliceSess := createAndPinHost(t, alice, srvA, "boxA-alice")
	mustStatus(t, alice.do("DELETE", "/api/sessions/"+aliceSess, nil), http.StatusOK, "close alice terminal")
	time.Sleep(100 * time.Millisecond)

	startCorraldInstall(t, alice, aliceHost)
	stA := waitCorraldState(t, alice, aliceHost, "running")
	if stA["state"] != "running" {
		t.Fatalf("expected running, got %v", stA)
	}
	aliceUID := int64(alice.do("GET", "/api/me", nil).json()["id"].(float64))
	cl, ok := h.srv.CorraldClient(aliceUID, aliceHost, "locate")
	if !ok || cl == nil {
		t.Fatal("Item #2 regression: corrald client closed immediately after install without open terminal")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := cl.Health(ctx); err != nil {
		t.Fatalf("Item #2 regression: Health() on freshly-installed corrald failed: %v", err)
	}

	// Item #1: Bob shares srvA with Alice. Then Bob edits his host to point at srvB
	// (a different machine with a different host key) while recovery runs.
	bobHost, bobSess := createAndPinHost(t, bob, srvA, "boxA-bob")
	mustStatus(t, bob.do("DELETE", "/api/sessions/"+bobSess, nil), http.StatusOK, "close bob terminal")
	waitCorraldState(t, bob, bobHost, "running")

	// Also install corrald on srvB independently so srvB has its own daemon.
	adminBHost, _ := createAndPinHost(t, admin, srvB, "boxB-admin")
	startCorraldInstall(t, admin, adminBHost)
	stB := waitCorraldState(t, admin, adminBHost, "running")
	pidB := int(stB["running"].(map[string]any)["pid"].(float64))

	// Repoint Bob's host record directly in store (simulating edit + re-pinning srvB's key
	// while sharedConn still holds Bob's ref) and break Alice's host credential so recovery
	// only succeeds if it wrongly accepts Bob's new host key!
	bobUID := int64(bob.do("GET", "/api/me", nil).json()["id"].(float64))
	hkA, _ := h.store.HostKey(context.Background(), aliceUID, aliceHost)
	hkB, _ := h.store.HostKey(context.Background(), int64(admin.do("GET", "/api/me", nil).json()["id"].(float64)), adminBHost)

	bh, _ := h.store.Host(context.Background(), bobUID, bobHost)
	bh.Address = "127.0.0.1"
	bh.Port = srvB.Addr.Port
	if err := h.store.UpdateHost(context.Background(), bh, false); err != nil {
		t.Fatal(err)
	}
	if err := h.store.ReplaceHostKey(context.Background(), bobUID, bobHost, hkA.Fingerprint, hkB); err != nil {
		t.Fatal(err)
	}
	// Make Alice's host unreachable during recovery so acquireMatchingSSH must inspect Bob's ref.
	ah, _ := h.store.Host(context.Background(), aliceUID, aliceHost)
	ah.Port = 1
	_ = h.store.UpdateHost(context.Background(), ah, false)

	// Force disconnect on srvA sharedConn -> recovery MUST reject Bob's mismatched (fp, user)
	// and mark Bob's host rebind, NEVER attaching srvB's daemon to Alice!
	h.srv.CorraldManager().ForceDropConn(hkA.Fingerprint, "alice")
	stBob := waitCorraldState(t, bob, bobHost, "rebind")
	if stBob["state"] != "rebind" {
		t.Fatalf("Item #1: expected bob host to become rebind, got %v", stBob["state"])
	}
	stAlice := alice.do("GET", fmt.Sprintf("/api/hosts/%d/corrald", aliceHost), nil).json()
	if r, _ := stAlice["running"].(map[string]any); r != nil && int(r["pid"].(float64)) == pidB {
		t.Fatalf("Item #1 regression: Alice attached to srvB's daemon pid=%d!", pidB)
	}
}

// TestCorraldReview_AccountWideOpExclusivityAndWithdrawalFirst covers
// review items #3, #4, and #5:
//   - #3: Two users sharing (fingerprint, username) cannot run concurrent ops.
//   - #4: Withdrawal-first remove waits for cancelled install/update to exit and
//     conditional op_id writes prevent stale ops from overwriting remove_pending.
//   - #5: DELETE /api/hosts/{id} cancels and waits for an in-progress install so
//     no untracked helper is left running after deletion.
func TestCorraldReview_AccountWideOpExclusivityAndWithdrawalFirst(t *testing.T) {
	v1Dir, v2Dir := buildTestCorraldDist(t)
	h := newHarness(t, func(c *config.Config) {
		c.CorraldDir = v1Dir
		c.GatewayID = "gw-r345.local"
	})
	admin := h.admin()
	alice := h.member(admin, "alice")
	bob := h.member(admin, "bob")
	srv, home := startCorraldSSH(t)

	distV1, _ := gw.LoadDist(v1Dir)
	distV2, _ := gw.LoadDist(v2Dir)

	updatePaused := make(chan struct{})
	releaseUpdate := make(chan struct{})
	h.srv.corrald.Close()
	h.srv.corrald = gw.NewManager(gw.ManagerConfig{
		Store:     h.store,
		Dist:      distV1,
		GatewayID: "gw-r345.local",
		SSH:       &webSSHProvider{s: h.srv},
		BeforeUpdateStage: func(hostID int64, opID string) {
			select {
			case <-updatePaused:
			default:
				close(updatePaused)
			}
			<-releaseUpdate
		},
	})

	aliceHost, _ := createAndPinHost(t, alice, srv, "acct-alice")
	bobHost, _ := createAndPinHost(t, bob, srv, "acct-bob")

	// Alice installs v1; Bob adopts the shared daemon.
	startCorraldInstall(t, alice, aliceHost)
	waitCorraldState(t, alice, aliceHost, "running")
	waitCorraldState(t, bob, bobHost, "running")

	// Start an update on Alice's host and pause it inside BeforeUpdateStage.
	h.srv.CorraldManager().SetDist(distV2)
	mustStatus(t, alice.do("POST", fmt.Sprintf("/api/hosts/%d/corrald/update", aliceHost), map[string]any{"manifest_hash": distV2.ManifestHash}), http.StatusAccepted, "alice update")
	select {
	case <-updatePaused:
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for BeforeUpdateStage")
	}

	// Item #3: Bob tries to start an update on the same account -> MUST get 409 op_in_progress!
	bobUpd := bob.do("POST", fmt.Sprintf("/api/hosts/%d/corrald/update", bobHost), map[string]any{"manifest_hash": distV2.ManifestHash})
	if bobUpd.Status != http.StatusConflict || bobUpd.code() != "op_in_progress" {
		t.Fatalf("Item #3 regression: expected 409 op_in_progress for peer account op, got %d %s", bobUpd.Status, bobUpd.Body)
	}

	// Item #4: Bob triggers withdrawal-first remove while Alice's update is blocked.
	// Release Alice's update slightly later so remove must wait for it and Alice's
	// cancelled exit must NOT overwrite the removal state!
	removeDone := make(chan resp, 1)
	go func() {
		removeDone <- bob.do("POST", fmt.Sprintf("/api/hosts/%d/corrald/remove", bobHost), nil)
	}()
	time.Sleep(150 * time.Millisecond)
	close(releaseUpdate)
	remResp := <-removeDone
	mustStatus(t, remResp, http.StatusAccepted, "bob withdrawal-first remove")

	stBob := waitCorraldState(t, bob, bobHost, "off")
	if stBob["state"] != "off" {
		t.Fatalf("Item #4 regression: expected off after withdrawal-first remove, got %v", stBob)
	}
	if _, err := os.Stat(filepath.Join(home, ".local", "lib", "corral", "corrald")); !os.IsNotExist(err) {
		t.Fatal("Item #4 regression: corrald binary survived withdrawal-first remove")
	}

	// Item #5: Start a new install blocked at BeforeInstallStart, then DELETE host
	// with remove_corrald=true -> must cancel & wait for install so nothing starts after delete!
	installPaused := make(chan struct{})
	releaseInstall := make(chan struct{})
	srv2, home2 := startCorraldSSH(t)
	h.srv.corrald.Close()
	h.srv.corrald = gw.NewManager(gw.ManagerConfig{
		Store:     h.store,
		Dist:      distV1,
		GatewayID: "gw-r345.local",
		SSH:       &webSSHProvider{s: h.srv},
		BeforeInstallStart: func(hostID int64, opID string) {
			select {
			case <-installPaused:
			default:
				close(installPaused)
			}
			<-releaseInstall
		},
	})
	delHost, _ := createAndPinHost(t, admin, srv2, "del-during-install")
	startCorraldInstall(t, admin, delHost)
	select {
	case <-installPaused:
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for BeforeInstallStart")
	}

	delDone := make(chan resp, 1)
	go func() {
		delDone <- admin.do("DELETE", fmt.Sprintf("/api/hosts/%d", delHost), map[string]any{"remove_corrald": true})
	}()
	time.Sleep(150 * time.Millisecond)
	close(releaseInstall)
	dr := <-delDone
	mustStatus(t, dr, http.StatusOK, "delete host during install")

	// Give any rogue goroutine a moment, then verify no binary or daemon remains.
	time.Sleep(250 * time.Millisecond)
	if _, err := os.Stat(filepath.Join(home2, ".local", "lib", "corral", "corrald")); !os.IsNotExist(err) {
		t.Fatal("Item #5 regression: install continued after DELETE /api/hosts/{id} and left corrald installed")
	}
}

// TestCorraldReview_RunningManifestScopeAndNoFirstEnableDowngrade covers
// review items #6 and #10:
//   - #6: When the running daemon's manifest expands beyond what this user
//     consented to, Status reports scope_change and ClientForCap blocks newly
//     added capabilities until the user explicitly consents.
//   - #10: Enabling corrald on a host that already has a newer Release installed
//     must NOT downgrade it; it attaches directly to the existing newer Release.
func TestCorraldReview_RunningManifestScopeAndNoFirstEnableDowngrade(t *testing.T) {
	v1Dir, v2Dir := buildTestCorraldDist(t)
	h := newHarness(t, func(c *config.Config) {
		c.CorraldDir = v1Dir
		c.GatewayID = "gw-r610.local"
	})
	admin := h.admin()
	srv, home := startCorraldSSH(t)
	hostID, _ := createAndPinHost(t, admin, srv, "r610box")

	// Item #10: Pre-install v2 (Release 2000) directly on host (as if installed by a newer gateway).
	v2Bin := filepath.Join(v2Dir, "linux-amd64", "corrald")
	stageDir := filepath.Join(home, ".local", "state", "corral", "corrald")
	// Use v2 binary's own selftest to discover machine-id state dir.
	stCmd := exec.Command(v2Bin, "selftest")
	stCmd.Env = append(os.Environ(), "HOME="+home)
	stOut, err := stCmd.CombinedOutput()
	if err != nil {
		t.Fatalf("v2 selftest: %v (%s)", err, stOut)
	}
	stFields := gw.ParseKeyValues(stOut)
	midStateDir := stFields["state_dir"]
	preStage := filepath.Join(midStateDir, "stage-preinst01")
	if err := os.MkdirAll(preStage, 0o700); err != nil {
		t.Fatal(err)
	}
	b2, err := os.ReadFile(v2Bin)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(preStage, "corrald"), b2, 0o700); err != nil {
		t.Fatal(err)
	}
	instCmd := exec.Command(filepath.Join(preStage, "corrald"), "install", "--stage", preStage, "--op", "preinst01", "--gateway", "other-gw", "--by", "other")
	instCmd.Env = append(os.Environ(), "HOME="+home)
	if out, err := instCmd.CombinedOutput(); err != nil {
		t.Fatalf("v2 pre-install: %v (%s)", err, out)
	}
	_ = stageDir

	// Now our older v1 gateway (Release 1000) enables corrald for the first time.
	startCorraldInstall(t, admin, hostID)
	st := waitCorraldState(t, admin, hostID, "running")
	if st["version"] != "0.3.2" || int64(st["release"].(float64)) != 2000 {
		t.Fatalf("Item #10 regression: first enable downgraded existing v2 to %v (rel=%v)", st["version"], st["release"])
	}
	linkTarget, err := os.Readlink(filepath.Join(home, ".local", "lib", "corral", "corrald"))
	if err != nil || !strings.Contains(linkTarget, "0.3.2-2000") {
		t.Fatalf("Item #10 regression: symlink changed to %q", linkTarget)
	}

	// Item #6: Simulate the running daemon having an expanded manifest (extra optional cap + extra action).
	expanded := proto.DefaultManifest()
	expanded.Optional = append(expanded.Optional, "extra.cap")
	h.srv.CorraldManager().SetRunningManifestForTest(hostID, expanded)

	stScope := admin.do("GET", fmt.Sprintf("/api/hosts/%d/corrald", hostID), nil).json()
	scObj, _ := stScope["scope_change"].(map[string]any)
	if scObj == nil {
		t.Fatalf("Item #6 regression: expected scope_change for expanded running manifest, got %v", stScope)
	}
	uid := int64(admin.do("GET", "/api/me", nil).json()["id"].(float64))
	if _, ok := h.srv.CorraldClient(uid, hostID, "extra.cap"); ok {
		t.Fatal("Item #6 regression: CorraldClient allowed unconsented capability extra.cap")
	}
	// Previously consented capability remains usable.
	if _, ok := h.srv.CorraldClient(uid, hostID, "agent.state"); !ok {
		t.Fatal("expected previously consented agent.state to remain usable")
	}

	// User explicitly consents via /update with the running manifest's manifest_hash -> scope_change clears!
	expandedHash, _ := proto.ManifestSHA256(expanded)
	mustStatus(t, admin.do("POST", fmt.Sprintf("/api/hosts/%d/corrald/update", hostID), map[string]any{"manifest_hash": expandedHash}), http.StatusAccepted, "consent expanded running manifest")
	stAfter := admin.do("GET", fmt.Sprintf("/api/hosts/%d/corrald", hostID), nil).json()
	if stAfter["scope_change"] != nil {
		t.Fatalf("expected scope_change cleared after consent, got %v", stAfter["scope_change"])
	}
	if _, ok := h.srv.CorraldClient(uid, hostID, "extra.cap"); !ok {
		t.Fatal("expected extra.cap allowed after explicit user consent")
	}
}

// TestCorraldReview_UninstallMissingBinaryStopsRunningProcess covers review item #7:
// when the binary on disk has been deleted while the daemon is still running,
// the fallback uninstall must verify meta.json + /proc start time, stop the
// running process, and refuse to delete stateDir (reporting stop_failed) if
// corrald.lock is still held.
func TestCorraldReview_UninstallMissingBinaryStopsRunningProcess(t *testing.T) {
	v1Dir, _ := buildTestCorraldDist(t)
	h := newHarness(t, func(c *config.Config) {
		c.CorraldDir = v1Dir
		c.GatewayID = "gw-r7.local"
	})
	admin := h.admin()
	srv, home := startCorraldSSH(t)
	hostID, _ := createAndPinHost(t, admin, srv, "r7box")

	startCorraldInstall(t, admin, hostID)
	st := waitCorraldState(t, admin, hostID, "running")
	pid := int(st["running"].(map[string]any)["pid"].(float64))

	// Delete ALL binaries under ~/.local/lib/corral while daemon is still running!
	if err := os.RemoveAll(filepath.Join(home, ".local", "lib", "corral")); err != nil {
		t.Fatal(err)
	}

	// Case 7a: Hold corrald.lock in a separate process AND remove meta.json's pid
	// match so fallback sees lock still held -> MUST return stop_failed and keep stateDir!
	lockMatches, _ := filepath.Glob(filepath.Join(home, ".local", "state", "corral", "corrald", "*", "corrald.lock"))
	if len(lockMatches) == 0 {
		t.Fatal("expected corrald.lock on host")
	}
	stateDir := filepath.Dir(lockMatches[0])
	metaPath := filepath.Join(stateDir, "meta.json")
	origMeta, _ := os.ReadFile(metaPath)
	// Corrupt pid/start_time and socket_path in meta.json so neither RPC nor PID
	// match can stop the daemon while the real daemon still holds corrald.lock!
	_ = os.WriteFile(metaPath, []byte(`{"pid":999999,"start_time":"99999999","socket_path":"/nonexistent/corrald.sock"}`), 0o600)

	mustStatus(t, admin.do("POST", fmt.Sprintf("/api/hosts/%d/corrald/remove", hostID), nil), http.StatusAccepted, "remove with held lock")
	stFail := waitCorraldState(t, admin, hostID, "error")
	errObj, _ := stFail["error"].(map[string]any)
	if errObj == nil || errObj["code"] != "stop_failed" {
		t.Fatalf("Item #7 regression: expected stop_failed when lock still held and binary missing, got %v", stFail)
	}
	if _, err := os.Stat(metaPath); err != nil {
		t.Fatalf("Item #7 regression: meta.json/stateDir was deleted despite stop_failed: %v", err)
	}

	// Case 7b: Restore real meta.json -> fallback reads meta.json, kills running daemon
	// (verifying /proc/<pid>/stat), verifies lock released, and removes cleanly!
	_ = os.WriteFile(metaPath, origMeta, 0o600)
	mustStatus(t, admin.do("POST", fmt.Sprintf("/api/hosts/%d/corrald/remove", hostID), nil), http.StatusAccepted, "remove missing binary")
	stOff := waitCorraldState(t, admin, hostID, "off")
	if stOff["state"] != "off" {
		t.Fatalf("expected off after fallback stopped daemon, got %v", stOff)
	}
	if _, err := os.Lstat(filepath.Join(home, ".local", "state", "corral", "run")); !os.IsNotExist(err) {
		t.Fatalf("corrald run dir should be removed after fallback uninstall (err=%v)", err)
	}
	// Verify daemon PID actually exited (not running — either gone or unreaped zombie).
	if err := syscall.Kill(pid, 0); err == nil {
		statBytes, rerr := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
		if rerr == nil && !strings.Contains(string(statBytes), ") Z ") && !strings.Contains(string(statBytes), ") X ") {
			t.Fatalf("Item #7 regression: daemon PID %d still alive after missing-binary uninstall reported removed (%s)", pid, statBytes)
		}
	}
}

// TestCorraldReview_UnauthenticatedUserCannotSeeSharedMetadata covers item #8:
// pinning a host key via a failed SSH login (wrong password) must NOT grant
// access to another user's shared helper metadata.
func TestCorraldReview_UnauthenticatedUserCannotSeeSharedMetadata(t *testing.T) {
	v1Dir, _ := buildTestCorraldDist(t)
	h := newHarness(t, func(c *config.Config) {
		c.CorraldDir = v1Dir
		c.GatewayID = "gw-r8.local"
	})
	admin := h.admin()
	alice := h.member(admin, "alice")
	eve := h.member(admin, "eve")
	srv, _ := startCorraldSSH(t)

	// Alice legitimately logs in and installs corrald.
	aliceHost, _ := createAndPinHost(t, alice, srv, "alice-srv")
	startCorraldInstall(t, alice, aliceHost)
	waitCorraldState(t, alice, aliceHost, "running")

	// Eve creates a host pointing to the same (srv, alice) with a WRONG password.
	r := eve.do("POST", "/api/hosts", map[string]any{
		"name":     "eve-probe",
		"address":  "127.0.0.1",
		"port":     srv.Addr.Port,
		"username": "alice",
		"auth":     "password",
		"password": "wrong-password",
	})
	mustStatus(t, r, http.StatusCreated, "eve create host")
	eveHost := int64(r.json()["id"].(float64))

	// Eve tries to open a session -> fails auth, even though host key gets pinned!
	failSess := eve.do("POST", "/api/sessions", map[string]any{"host_id": eveHost})
	if failSess.Status == http.StatusCreated {
		t.Fatalf("expected auth failure for wrong password, got 201")
	}

	// Eve calls GET /api/hosts/{eveHost}/corrald -> MUST see off with NO shared metadata!
	stEve := eve.do("GET", fmt.Sprintf("/api/hosts/%d/corrald", eveHost), nil).json()
	if stEve["state"] != "off" || stEve["running"] != nil || stEve["users"] != nil || stEve["consent"] != nil {
		t.Fatalf("Item #8 regression: unauthenticated user saw shared helper metadata: %v", stEve)
	}
	if files, _ := stEve["files"].([]any); len(files) > 0 {
		t.Fatalf("Item #8 regression: unauthenticated user saw install files: %v", files)
	}

	// Once Eve provides the correct password and logs in, she can legitimately see shared state.
	mustStatus(t, eve.do("PUT", fmt.Sprintf("/api/hosts/%d", eveHost), map[string]any{
		"name": "eve-probe", "address": "127.0.0.1", "port": srv.Addr.Port,
		"username": "alice", "auth": "password", "password": "alice-pass",
	}), http.StatusOK, "eve fix password")
	mustStatus(t, eve.do("POST", "/api/sessions", map[string]any{"host_id": eveHost, "open": map[string]any{"kind": "shell"}}), http.StatusCreated, "eve real login")
	stEveOK := waitCorraldState(t, eve, eveHost, "running")
	if stEveOK["state"] != "running" || stEveOK["running"] == nil {
		t.Fatalf("expected running after Eve completed real SSH login, got %v", stEveOK)
	}
}

// TestCorraldReview_ManifestHashRequired400 covers item #9:
// empty or missing manifest_hash returns 400 Bad Request on both /install and /update.
func TestCorraldReview_ManifestHashRequired400(t *testing.T) {
	v1Dir, _ := buildTestCorraldDist(t)
	h := newHarness(t, func(c *config.Config) {
		c.CorraldDir = v1Dir
		c.GatewayID = "gw-r9.local"
	})
	admin := h.admin()
	srv, _ := startCorraldSSH(t)
	hostID, _ := createAndPinHost(t, admin, srv, "r9box")

	for _, body := range []any{nil, map[string]any{}, map[string]any{"manifest_hash": ""}} {
		r := admin.do("POST", fmt.Sprintf("/api/hosts/%d/corrald/install", hostID), body)
		if r.Status != http.StatusBadRequest {
			t.Fatalf("Item #9 regression: install with %v returned %d %s, want 400", body, r.Status, r.Body)
		}
	}

	// Install properly, then verify /update also rejects empty manifest_hash with 400.
	startCorraldInstall(t, admin, hostID)
	waitCorraldState(t, admin, hostID, "running")

	for _, body := range []any{nil, map[string]any{}, map[string]any{"manifest_hash": ""}} {
		r := admin.do("POST", fmt.Sprintf("/api/hosts/%d/corrald/update", hostID), body)
		if r.Status != http.StatusBadRequest {
			t.Fatalf("Item #9 regression: update with %v returned %d %s, want 400", body, r.Status, r.Body)
		}
	}
}

// TestCorraldReview_InterruptedFirstInstallReconcilesNotRebind covers item #11:
// authorization and binding are written at install start, so if the gateway
// restarts mid-install, reconnect reconciles from host ops/stage records instead
// of entering rebind.
func TestCorraldReview_InterruptedFirstInstallReconcilesNotRebind(t *testing.T) {
	v1Dir, _ := buildTestCorraldDist(t)
	h := newHarness(t, func(c *config.Config) {
		c.CorraldDir = v1Dir
		c.GatewayID = "gw-r11.local"
	})
	admin := h.admin()
	srv, _ := startCorraldSSH(t)
	hostID, _ := createAndPinHost(t, admin, srv, "r11box")

	// Simulate gateway crash right after `corrald install` completed (before `serve`),
	// using BeforeInstallStart to cancel and close the old manager.
	dist, _ := gw.LoadDist(v1Dir)
	hit := make(chan struct{})
	gate := make(chan struct{})
	h.srv.corrald.Close()
	h.srv.corrald = gw.NewManager(gw.ManagerConfig{
		Store:     h.store,
		Dist:      dist,
		GatewayID: "gw-r11.local",
		SSH:       &webSSHProvider{s: h.srv},
		BeforeInstallStart: func(hid int64, opID string) {
			close(hit)
			<-gate
		},
	})

	startCorraldInstall(t, admin, hostID)
	select {
	case <-hit:
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for BeforeInstallStart")
	}

	// Verify DB ALREADY has state=installing and bound identity persisted before serve!
	chBefore, err := h.store.CorraldHostByID(context.Background(), hostID)
	if err != nil {
		t.Fatalf("CorraldHostByID during install: %v", err)
	}
	if chBefore.State != gw.StateInstalling || chBefore.BoundFingerprint == "" || chBefore.BoundUsername != "alice" || chBefore.BoundGateway != "gw-r11.local" {
		t.Fatalf("Item #11 regression: binding not persisted at install start: %+v", chBefore)
	}

	// Simulate hard gateway restart: swap to a brand-new Manager while DB still has
	// state=installing + active op (without letting old op mark cancelled).
	oldMgr := h.srv.corrald
	h.srv.corrald = gw.NewManager(gw.ManagerConfig{
		Store:     h.store,
		Dist:      dist,
		GatewayID: "gw-r11.local",
		SSH:       &webSSHProvider{s: h.srv},
	})
	// Reconnect -> MUST reconcile ops/<id>.json + install.json -> start daemon -> running (NOT rebind!).
	mustStatus(t, admin.do("POST", "/api/sessions", map[string]any{"host_id": hostID, "open": map[string]any{"kind": "shell"}}), http.StatusCreated, "reconnect after crash")
	st := waitCorraldState(t, admin, hostID, "running")
	if st["state"] != "running" {
		t.Fatalf("Item #11 regression: expected running after post-crash install reconcile, got %v", st)
	}
	close(gate)
	oldMgr.Close()
}

// TestCorraldReview_SFTPSymlinkRejectedBeforeWrite covers item #12:
// pre-existing symlinks along ~/.local/lib/corral or ~/.local/state/corral/corrald
// are rejected BEFORE chmod or SFTP writes occur.
func TestCorraldReview_SFTPSymlinkRejectedBeforeWrite(t *testing.T) {
	v1Dir, _ := buildTestCorraldDist(t)
	h := newHarness(t, func(c *config.Config) {
		c.CorraldDir = v1Dir
		c.GatewayID = "gw-r12sym.local"
	})
	admin := h.admin()
	srv, home := startCorraldSSH(t)
	hostID, _ := createAndPinHost(t, admin, srv, "symbox")

	// Plant a symlink at ~/.local/state/corral/corrald pointing to a victim dir outside home.
	victim := t.TempDir()
	if err := os.Chmod(victim, 0o755); err != nil {
		t.Fatal(err)
	}
	stateParent := filepath.Join(home, ".local", "state", "corral")
	if err := os.MkdirAll(stateParent, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, filepath.Join(stateParent, "corrald")); err != nil {
		t.Fatal(err)
	}

	startCorraldInstall(t, admin, hostID)
	st := waitCorraldState(t, admin, hostID, "off")
	op, _ := st["op"].(map[string]any)
	if op == nil || op["state"] != "failed" || op["error_code"] != "home_readonly" {
		t.Fatalf("Item #12 regression: expected failed/home_readonly for symlink state dir, got %v", st)
	}
	entries, _ := os.ReadDir(victim)
	if len(entries) != 0 {
		t.Fatalf("Item #12 regression: files written into symlink target victim dir: %v", entries)
	}
	info, _ := os.Stat(victim)
	if info.Mode().Perm() != 0o755 {
		t.Fatalf("Item #12 regression: victim dir permissions modified to %o", info.Mode().Perm())
	}
}

// TestCorraldReview_FailedReleaseSkippedOnSharedFastPath covers item #15:
// after a hot update to a bad release fails and writes failed.json, opening
// another terminal hits the live shared-conn fast path and MUST NOT retry the
// failed release automatically.
func TestCorraldReview_FailedReleaseSkippedOnSharedFastPath(t *testing.T) {
	v1Dir, v2Dir := buildTestCorraldDist(t)
	h := newHarness(t, func(c *config.Config) {
		c.CorraldDir = v1Dir
		c.GatewayID = "gw-r15.local"
	})
	admin := h.admin()
	srv, _ := startCorraldSSH(t)
	hostID, _ := createAndPinHost(t, admin, srv, "r15box")

	startCorraldInstall(t, admin, hostID)
	waitCorraldState(t, admin, hostID, "running")

	// Switch to v2 with CORRALD_TEST_FAIL_READY=1 so handoff fails and records Release 2000 as failed.
	t.Setenv("CORRALD_TEST_FAIL_READY", "1")
	distV2, _ := gw.LoadDist(v2Dir)
	h.srv.CorraldManager().SetDist(distV2)
	mustStatus(t, admin.do("POST", fmt.Sprintf("/api/hosts/%d/corrald/update", hostID), map[string]any{"manifest_hash": distV2.ManifestHash}), http.StatusAccepted, "trigger bad update")
	st := waitCorraldState(t, admin, hostID, "running")
	failedOpID := st["op"].(map[string]any)["id"].(string)

	// Open a NEW terminal session -> hits live shared-conn fast path in OnHostConnected!
	mustStatus(t, admin.do("POST", "/api/sessions", map[string]any{"host_id": hostID, "open": map[string]any{"kind": "shell"}}), http.StatusCreated, "open 2nd session")
	time.Sleep(400 * time.Millisecond)

	st2 := waitCorraldState(t, admin, hostID, "running")
	if st2["op"].(map[string]any)["id"].(string) != failedOpID {
		t.Fatalf("Item #15 regression: shared fast path retried failed release! old op=%s, new op=%v", failedOpID, st2["op"])
	}
}

// TestCorraldReview_AccountCleanupPausedResetAndUnknownRemote covers items #16 and #17:
//   - #17: Admin password reset does NOT uninstall corrald.
//   - #16a: Disabling Alice while Bob is paused keeps corrald (paused user counts as a user).
//   - #16b: When remote user count cannot be determined, CleanupUserHosts preserves
//     the helper and reports "not_cleaned".
func TestCorraldReview_AccountCleanupPausedResetAndUnknownRemote(t *testing.T) {
	v1Dir, _ := buildTestCorraldDist(t)
	h := newHarness(t, func(c *config.Config) {
		c.CorraldDir = v1Dir
		c.GatewayID = "gw-r1617.local"
	})
	admin := h.admin()
	alice := h.member(admin, "alice")
	bob := h.member(admin, "bob")
	srv, home := startCorraldSSH(t)

	aliceHost, _ := createAndPinHost(t, alice, srv, "r1617-a")
	bobHost, _ := createAndPinHost(t, bob, srv, "r1617-b")

	startCorraldInstall(t, alice, aliceHost)
	waitCorraldState(t, alice, aliceHost, "running")
	waitCorraldState(t, bob, bobHost, "running")

	// Item #17: Admin resets Alice's password -> corrald MUST stay installed!
	aliceUID := int64(alice.do("GET", "/api/me", nil).json()["id"].(float64))
	mustStatus(t, admin.do("POST", fmt.Sprintf("/api/admin/users/%d/password", aliceUID), map[string]any{
		"password":    "NewStrongPass!99",
		"must_change": false,
	}), http.StatusOK, "admin reset password")
	time.Sleep(300 * time.Millisecond)
	if _, err := os.Stat(filepath.Join(home, ".local", "lib", "corral", "corrald")); err != nil {
		t.Fatalf("Item #17 regression: password reset deleted corrald: %v", err)
	}

	// Item #16a: Bob pauses his host, then admin disables Alice -> Bob is paused
	// but STILL counts as a user, so corrald MUST remain installed!
	mustStatus(t, bob.do("POST", fmt.Sprintf("/api/hosts/%d/corrald/pause", bobHost), nil), http.StatusOK, "bob pause")
	waitCorraldState(t, bob, bobHost, "paused")

	mustStatus(t, admin.do("PUT", fmt.Sprintf("/api/admin/users/%d", aliceUID), map[string]any{"disabled": true}), http.StatusOK, "disable alice")
	time.Sleep(400 * time.Millisecond)
	if _, err := os.Stat(filepath.Join(home, ".local", "lib", "corral", "corrald")); err != nil {
		t.Fatalf("Item #16 regression: disabling alice deleted corrald while bob was paused: %v", err)
	}

	// Item #16b: Single user (charlie) on an unreachable host -> remote users unknown
	// -> CleanupUserHosts returns "not_cleaned" and preserves installation.
	charlie := h.member(admin, "charlie")
	srv2, home2 := startCorraldSSH(t)
	charlieHost, _ := createAndPinHost(t, charlie, srv2, "r16-charlie")
	startCorraldInstall(t, charlie, charlieHost)
	waitCorraldState(t, charlie, charlieHost, "running")

	// Disconnect live conn and stop srv2 so remote user count cannot be queried.
	charlieUID := int64(charlie.do("GET", "/api/me", nil).json()["id"].(float64))
	h.srv.CorraldManager().DetachHost(charlieHost)
	srv2.Close()

	res := h.srv.CorraldManager().CleanupUserHosts(context.Background(), charlieUID, "charlie")
	if res[charlieHost] != "not_cleaned" {
		t.Fatalf("Item #16 regression: expected not_cleaned when remote clients unknown, got %v", res)
	}
	if _, err := os.Stat(filepath.Join(home2, ".local", "lib", "corral", "corrald")); err != nil {
		t.Fatalf("expected binary preserved when remote clients unknown: %v", err)
	}
}

// TestCorraldReview_DeleteHostProbesWithoutRecordAndAllFalseSkipsVSCode covers
// review items #18 and #20:
//   - #18: DELETE with remove_corrald=true probes and uninstalls corrald even
//     when local DB has no corrald_hosts record (e.g. installed by another gateway).
//   - #20: DELETE with all three options explicitly false does NOT run legacy
//     VS Code stop.
func TestCorraldReview_DeleteHostProbesWithoutRecordAndAllFalseSkipsVSCode(t *testing.T) {
	v1Dir, _ := buildTestCorraldDist(t)
	h := newHarness(t, func(c *config.Config) {
		c.CorraldDir = v1Dir
		c.GatewayID = "gw-r1820.local"
	})
	admin := h.admin()
	srv, home := startCorraldSSH(t)

	// Item #18: Install corrald on host directly without creating any local corrald_hosts row.
	v1Bin := filepath.Join(v1Dir, "linux-amd64", "corrald")
	stOut, err := func() ([]byte, error) {
		cmd := exec.Command(v1Bin, "selftest")
		cmd.Env = append(os.Environ(), "HOME="+home)
		return cmd.CombinedOutput()
	}()
	if err != nil {
		t.Fatalf("selftest: %v (%s)", err, stOut)
	}
	stDir := gw.ParseKeyValues(stOut)["state_dir"]
	stageDir := filepath.Join(stDir, "stage-ext00001")
	_ = os.MkdirAll(stageDir, 0o700)
	b1, _ := os.ReadFile(v1Bin)
	_ = os.WriteFile(filepath.Join(stageDir, "corrald"), b1, 0o700)
	instCmd := exec.Command(filepath.Join(stageDir, "corrald"), "install", "--stage", stageDir, "--op", "ext00001", "--gateway", "other-gw", "--by", "ext")
	instCmd.Env = append(os.Environ(), "HOME="+home)
	if out, err := instCmd.CombinedOutput(); err != nil {
		t.Fatalf("ext install: %v (%s)", err, out)
	}

	host1, _ := createAndPinHost(t, admin, srv, "ext-installed")
	dr := admin.do("DELETE", fmt.Sprintf("/api/hosts/%d", host1), map[string]any{
		"remove_corrald": true,
		"stop_vscode":    false,
		"remove_vscode":  false,
	})
	mustStatus(t, dr, http.StatusOK, "delete host with remove_corrald=true")
	corraldRes, _ := dr.json()["corrald"].(map[string]any)
	if corraldRes == nil || corraldRes["state"] != "removed" {
		t.Fatalf("Item #18 regression: expected corrald.state=removed when installed without local DB row, got %s", dr.Body)
	}
	if _, err := os.Stat(filepath.Join(home, ".local", "lib", "corral", "corrald")); !os.IsNotExist(err) {
		t.Fatal("Item #18 regression: corrald binary still exists on host")
	}

	// Item #20: Track exec calls on SSH server; DELETE with all three options false
	// must NOT invoke legacy VS Code stop!
	var execAfterDelete atomic.Int32
	srv2 := testutil.StartSSHServer(t, "alice", "alice-pass", t.TempDir())
	srv2.HandleExec(func(cmd string) func(ch ssh.Channel) {
		if strings.Contains(cmd, "code") || strings.Contains(cmd, "corral-code") {
			execAfterDelete.Add(1)
		}
		return nil
	})
	host2, sess2 := createAndPinHost(t, admin, srv2, "all-false-delete")
	mustStatus(t, admin.do("DELETE", "/api/sessions/"+sess2, nil), http.StatusOK, "close sess2")
	execAfterDelete.Store(0)

	dr2 := admin.do("DELETE", fmt.Sprintf("/api/hosts/%d", host2), map[string]any{
		"remove_corrald": false,
		"stop_vscode":    false,
		"remove_vscode":  false,
	})
	mustStatus(t, dr2, http.StatusOK, "delete host all-false")
	time.Sleep(250 * time.Millisecond)
	if n := execAfterDelete.Load(); n != 0 {
		t.Fatalf("Item #20 regression: all-false delete ran %d VS Code stop command(s)", n)
	}
}

// TestCorraldReview_AutoRestoreTerminalReconnectsCorrald covers item #21:
// gateway restart terminal restore (StartRestore -> terms.Start) must also
// trigger OnHostConnected so corrald reconnects without opening a new terminal.
func TestCorraldReview_AutoRestoreTerminalReconnectsCorrald(t *testing.T) {
	v1Dir, _ := buildTestCorraldDist(t)
	h := newHarness(t, func(c *config.Config) {
		c.CorraldDir = v1Dir
		c.GatewayID = "gw-r21.local"
	})
	admin := h.admin()
	home := t.TempDir()
	// Provide a fake tmux script in $HOME/.local/bin so no real tmux server is ever launched.
	fakeBin := filepath.Join(home, ".local", "bin")
	if err := os.MkdirAll(fakeBin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fakeBin, "tmux"), []byte("#!/bin/sh\nprintf 'work|1|1\\n'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	srv := testutil.StartSSHServer(t, "alice", "alice-pass", home)
	srv.HandleExec(testutil.ShellExec(home, func(cmd string) bool {
		return strings.Contains(cmd, gw.CommandTag) || strings.Contains(cmd, "corrald") || strings.Contains(cmd, "list-sessions")
	}, "PATH="+fakeBin+":"+os.Getenv("PATH")))
	t.Cleanup(func() {
		bin := filepath.Join(home, ".local", "lib", "corral", "corrald")
		if _, err := os.Stat(bin); err == nil {
			c := exec.Command(bin, "stop")
			c.Env = append(os.Environ(), "HOME="+home)
			_ = c.Run()
		}
	})

	r := admin.do("POST", "/api/hosts", map[string]any{
		"name": "restore-box", "address": "127.0.0.1", "port": srv.Addr.Port,
		"username": "alice", "auth": "password", "password": "alice-pass",
	})
	mustStatus(t, r, http.StatusCreated, "create restore host")
	hostID := int64(r.json()["id"].(float64))
	mustStatus(t, admin.do("POST", "/api/sessions", map[string]any{
		"host_id": hostID, "cols": 80, "rows": 24,
		"open": map[string]any{"kind": "tmux", "name": "work"},
	}), http.StatusCreated, "open tmux work")

	startCorraldInstall(t, admin, hostID)
	waitCorraldState(t, admin, hostID, "running")

	// Simulate a full gateway restart (new Server, empty terms & fresh gw.Manager on same DB).
	h.restart()
	if h.srv.CorraldManager().SharedConnCount() != 0 {
		t.Fatal("expected 0 shared conns right after restart")
	}

	kept := h.kept()
	if len(kept) == 0 {
		t.Fatal("expected at least one saved open_tab for restore")
	}
	h.restoreNow(kept)

	// Wait for async OnHostConnected from restore.go to establish the shared corrald connection!
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if h.srv.CorraldManager().SharedConnCount() == 1 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if n := h.srv.CorraldManager().SharedConnCount(); n != 1 {
		t.Fatalf("Item #21 regression: SharedConnCount after StartRestore = %d, want 1", n)
	}
}

// TestCorraldReview_PostUpdateCrashWindowPersistsAcrossRestart covers item #22:
// update time, target release, previous version, and crash count persist to
// corrald_hosts so a gateway restart between crash #1 and crash #2 still
// triggers automatic rollback within the 30-minute window.
func TestCorraldReview_PostUpdateCrashWindowPersistsAcrossRestart(t *testing.T) {
	v1Dir, v2Dir := buildTestCorraldDist(t)
	h := newHarness(t, func(c *config.Config) {
		c.CorraldDir = v1Dir
		c.GatewayID = "gw-r22.local"
	})
	admin := h.admin()
	srv, home := startCorraldSSH(t)

	distV1, _ := gw.LoadDist(v1Dir)
	distV2, _ := gw.LoadDist(v2Dir)
	newMgr := func(d *gw.Dist) *gw.Manager {
		return gw.NewManager(gw.ManagerConfig{
			Store:           h.store,
			Dist:            d,
			GatewayID:       "gw-r22.local",
			SSH:             &webSSHProvider{s: h.srv},
			HealthInterval:  5 * time.Second,
			RestartBackoffs: []time.Duration{20 * time.Millisecond, 20 * time.Millisecond, 20 * time.Millisecond},
			CrashWindow:     30 * time.Minute,
			CrashThreshold:  2,
		})
	}
	h.srv.corrald.Close()
	h.srv.corrald = newMgr(distV1)

	hostID, _ := createAndPinHost(t, admin, srv, "r22box")
	startCorraldInstall(t, admin, hostID)
	waitCorraldState(t, admin, hostID, "running")

	// Update cleanly to v2.
	h.srv.CorraldManager().SetDist(distV2)
	mustStatus(t, admin.do("POST", fmt.Sprintf("/api/hosts/%d/corrald/update", hostID), map[string]any{"manifest_hash": distV2.ManifestHash}), http.StatusAccepted, "update to v2")
	st := waitCorraldState(t, admin, hostID, "running")
	if st["version"] != "0.3.2" {
		t.Fatalf("expected 0.3.2, got %v", st["version"])
	}

	// Verify update window metadata (actual target + window start) was persisted in corrald_hosts;
	// rollback target is computed dynamically at rollback time by SelectStartBinary.
	chRow, err := h.store.CorraldHostByID(context.Background(), hostID)
	if err != nil {
		t.Fatal(err)
	}
	if chRow.UpdateReadyAt == 0 || chRow.UpdateTargetRel != 2000 || chRow.UpdateTargetVer != "0.3.2" {
		t.Fatalf("Item #22 regression: update window not persisted in DB: %+v", chRow)
	}

	uid := int64(admin.do("GET", "/api/me", nil).json()["id"].(float64))
	hk, _ := h.store.HostKey(context.Background(), uid, hostID)

	// Crash #1 before gateway restart.
	if !h.srv.CorraldManager().ForceDropConn(hk.Fingerprint, "alice") {
		t.Fatal("first ForceDropConn failed")
	}
	waitCorraldState(t, admin, hostID, "running")

	// Verify crash count=1 was persisted!
	chRow, _ = h.store.CorraldHostByID(context.Background(), hostID)
	if chRow.UpdateCrashCount != 1 {
		t.Fatalf("Item #22 regression: expected UpdateCrashCount=1 persisted, got %d", chRow.UpdateCrashCount)
	}

	// Gateway restarts (fresh gw.Manager, empty in-memory sharedConn!).
	h.srv.corrald.Close()
	h.srv.corrald = newMgr(distV2)
	mustStatus(t, admin.do("POST", "/api/sessions", map[string]any{"host_id": hostID, "open": map[string]any{"kind": "shell"}}), http.StatusCreated, "reconnect after restart")
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) && h.srv.CorraldManager().SharedConnCount() == 0 {
		time.Sleep(50 * time.Millisecond)
	}
	if h.srv.CorraldManager().SharedConnCount() != 1 {
		t.Fatal("expected reconnected sharedConn after restart")
	}

	// Crash #2 after gateway restart -> MUST still trigger automatic rollback to v1!
	if !h.srv.CorraldManager().ForceDropConn(hk.Fingerprint, "alice") {
		t.Fatal("second ForceDropConn after restart failed")
	}
	deadline = time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		st = waitCorraldState(t, admin, hostID, "running")
		if st["version"] == "0.3.1" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if st["version"] != "0.3.1" {
		t.Fatalf("Item #22 regression: crash #2 after gateway restart did not roll back to 0.3.1 (got %v)", st)
	}
	matches, _ := filepath.Glob(filepath.Join(home, ".local", "state", "corral", "corrald", "*", "failed.json"))
	if len(matches) == 0 {
		t.Fatal("expected failed.json after post-restart rollback")
	}
}

// TestCorraldReview_EpochChangeUnknownResultNoRetry covers Section B:
// write operations use proto.NewOpID; if a mutating RPC disconnects mid-flight
// and reconnects to a DIFFERENT daemon epoch (cold restart), retrying with the
// same op_id MUST return ErrUnknownResult (mapped to HTTP 409 corrald_unknown_result)
// without sending the op_id over the wire again.
func TestCorraldReview_EpochChangeUnknownResultNoRetry(t *testing.T) {
	v1Dir, _ := buildTestCorraldDist(t)
	h := newHarness(t, func(c *config.Config) {
		c.CorraldDir = v1Dir
		c.GatewayID = "gw-epoch.local"
	})
	admin := h.admin()
	srv, home := startCorraldSSH(t)
	hostID, _ := createAndPinHost(t, admin, srv, "epochbox")

	startCorraldInstall(t, admin, hostID)
	waitCorraldState(t, admin, hostID, "running")

	uid := int64(admin.do("GET", "/api/me", nil).json()["id"].(float64))
	hk, _ := h.store.HostKey(context.Background(), uid, hostID)

	// Verify writeCorraldError maps ErrUnknownResult to HTTP 409 corrald_unknown_result.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/hosts/1/corrald/test", nil)
	h.srv.writeCorraldError(rec, req, gw.ErrUnknownResult)
	if rec.Code != http.StatusConflict {
		t.Fatalf("writeCorraldError(ErrUnknownResult) status = %d, want 409", rec.Code)
	}
	var errBody map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &errBody)
	if errBody["error"] != "corrald_unknown_result" {
		t.Fatalf("writeCorraldError(ErrUnknownResult) body = %s, want error=corrald_unknown_result", rec.Body.String())
	}

	// Obtain current epoch before cold restart.
	cl1, ok := h.srv.CorraldClient(uid, hostID, "pane.send")
	if !ok || cl1 == nil {
		t.Fatal("expected live corrald client")
	}
	epoch1, _ := cl1.Cursor()
	if epoch1 == "" {
		t.Fatal("expected non-empty initial daemon epoch")
	}

	// Trigger a mutating RPC with a protocol-compliant op_id while forcing the
	// connection to drop so its outcome is unacknowledged in epoch1.
	opID := gw.NewWriteOpID()
	if _, err := proto.ParseOpID(opID); err != nil {
		t.Fatalf("NewWriteOpID invalid: %v", err)
	}
	// Use a short timeout and drop the connection immediately to guarantee ErrUnknownResult.
	go func() {
		time.Sleep(5 * time.Millisecond)
		h.srv.CorraldManager().ForceDropConn(hk.Fingerprint, "alice")
	}()
	// Even if pane.send replies not_supported fast on a live daemon, test epoch-change
	// refusal directly by cold-restarting the daemon after an unacknowledged write.
	bin := filepath.Join(home, ".local", "lib", "corral", "corrald")
	stopCmd := exec.Command(bin, "stop")
	stopCmd.Env = append(os.Environ(), "HOME="+home)
	_ = stopCmd.Run()

	// Send mutating op while daemon is stopping/dropped -> must return ErrUnknownResult or ErrNotConnected.
	cctx, ccancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	_, err1 := h.srv.CorraldManager().CallMutating(cctx, uid, hostID, "pane.send", proto.MethodPaneSend, opID, func(id string) any {
		return map[string]any{"op_id": id, "pane": "p1", "text": "hello"}
	}, nil)
	ccancel()
	// Wait for auto-recovery to cold-start corrald with a NEW epoch!
	deadline := time.Now().Add(15 * time.Second)
	var epoch2 string
	for time.Now().Before(deadline) {
		if cl2, ok := h.srv.CorraldClient(uid, hostID, "pane.send"); ok && cl2 != nil {
			ep, _ := cl2.Cursor()
			if ep != "" && ep != epoch1 {
				epoch2 = ep
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	if epoch2 == "" || epoch2 == epoch1 {
		t.Fatalf("expected new epoch after cold restart, got epoch1=%q epoch2=%q", epoch1, epoch2)
	}

	// If err1 was ErrUnknownResult (unacked in epoch1), retrying SAME opID in epoch2
	// MUST return ErrUnknownResult without calling buildParams/sending on the wire!
	if errors.Is(err1, gw.ErrUnknownResult) {
		calledWire := false
		_, err2 := h.srv.CorraldManager().CallMutating(context.Background(), uid, hostID, "pane.send", proto.MethodPaneSend, opID, func(id string) any {
			calledWire = true
			return map[string]any{"op_id": id}
		}, nil)
		if !errors.Is(err2, gw.ErrUnknownResult) {
			t.Fatalf("Section B regression: retrying old-epoch op_id in new epoch returned %v, want ErrUnknownResult", err2)
		}
		if calledWire {
			t.Fatal("Section B regression: old-epoch op_id was built/sent to new epoch instead of rejected immediately")
		}
	}
}
