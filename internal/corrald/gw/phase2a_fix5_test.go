package gw

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/ihainan/corral/internal/corrald/proto"
	"github.com/ihainan/corral/internal/store"
)

// Regression test for Fix Round 5 Item #1 [P1]:
// When an old disable worker blocks inside client.Health and the admin
// re-allows corrald (bumping policyGen) + starts a new operation/state, the
// resumed old worker MUST NOT send ShutdownConditional and MUST NOT overwrite
// the host's new DB state or active_op_id.
func TestFix5_Item1_StaleDisableBlockedInHealthNeverShutdownsOrClobbersReallowedHost(t *testing.T) {
	st, uID, hostID := openTestStoreForGW(t)
	ctx := context.Background()
	fp, sshUser, gwID := "SHA256:fp-fix5-item1", "dev", "gw-fix5.local"

	if err := st.UpsertCorraldHost(ctx, &store.CorraldHost{
		HostID:           hostID,
		State:            StateRunning,
		BoundFingerprint: fp,
		BoundUsername:    sshUser,
		BoundGateway:     gwID,
		Version:          "0.3.2",
		Release:          2000,
	}); err != nil {
		t.Fatalf("UpsertCorraldHost: %v", err)
	}

	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()
	defer serverConn.Close()

	healthArrived := make(chan struct{}, 1)
	releaseHealth := make(chan struct{})
	var shutdownCalls atomic.Int32

	go func() {
		for {
			b, err := proto.ReadFrame(serverConn)
			if err != nil {
				return
			}
			var req proto.Request
			if json.Unmarshal(b, &req) != nil {
				return
			}
			switch req.Method {
			case proto.MethodDaemonHealth:
				healthArrived <- struct{}{}
				<-releaseHealth
				res, _ := json.Marshal(proto.HealthResult{PID: 5001, ConnectedGateways: 1})
				rb, _ := json.Marshal(proto.Response{ID: req.ID, Result: res})
				_ = proto.WriteFrame(serverConn, rb)
			case proto.MethodDaemonShutdown:
				shutdownCalls.Add(1)
				res, _ := json.Marshal(proto.ShutdownResult{OK: true})
				rb, _ := json.Marshal(proto.Response{ID: req.ID, Result: res})
				_ = proto.WriteFrame(serverConn, rb)
			}
		}
	}()

	cl := NewClient(clientConn, nil)
	defer cl.Close()

	m := NewManager(ManagerConfig{Store: st, GatewayID: gwID})
	defer m.Close()

	key := accountKey{Fingerprint: fp, Username: sshUser}
	sc := newSharedConn(key)
	sc.client = cl
	sc.hello = &proto.HelloResult{
		Version:  "0.3.2",
		Release:  2000,
		Features: proto.DefaultFeatures(),
	}
	sc.hostRefs[hostID] = &hostRef{HostID: hostID, OwnerID: uID, CorralUser: "tester"}
	m.mu.Lock()
	m.conns[key] = sc
	m.hostConn[hostID] = key
	m.mu.Unlock()

	// 1. Admin disables -> worker enters client.Health and blocks.
	m.SetAdminDisabled(ctx, true)
	select {
	case <-healthArrived:
	case <-time.After(3 * time.Second):
		t.Fatal("disable worker did not reach daemon.health RPC")
	}

	// 2. Admin re-allows while old worker is blocked in Health, and a new
	//    install operation claims the host row with active_op_id="op-reallowed".
	m.SetAdminDisabled(ctx, false)
	if err := st.UpsertCorraldHost(ctx, &store.CorraldHost{
		HostID:           hostID,
		State:            StateInstalling,
		BoundFingerprint: fp,
		BoundUsername:    sshUser,
		BoundGateway:     gwID,
		ActiveOpID:       "op-reallowed",
		Version:          "0.3.2",
		Release:          2000,
	}); err != nil {
		t.Fatalf("UpsertCorraldHost new op: %v", err)
	}

	// 3. Unblock Health -> old disable worker MUST observe stale policyGen,
	//    skip ShutdownConditional, and leave DB state + active_op_id intact.
	close(releaseHealth)
	deadline := time.Now().Add(3 * time.Second)
	for !cl.Closed() {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for stale disable worker to close old client")
		}
		time.Sleep(10 * time.Millisecond)
	}

	if shutdownCalls.Load() != 0 {
		t.Fatalf("Fix5 Item #1 regression: stale disable worker sent %d ShutdownConditional RPC(s) after re-allow!", shutdownCalls.Load())
	}
	ch, err := st.CorraldHostByID(ctx, hostID)
	if err != nil {
		t.Fatalf("CorraldHostByID: %v", err)
	}
	if ch.State != StateInstalling || ch.ActiveOpID != "op-reallowed" {
		t.Fatalf("Fix5 Item #1 regression: stale disable worker clobbered re-allowed host state=%q active_op_id=%q", ch.State, ch.ActiveOpID)
	}
}

// Regression test for Fix Round 5 Item #2 [P1] (store conditional reset):
// CorraldResetByBindingIfIdle must reset idle rows for the account to 'off'
// while NEVER clobbering a peer row actively installing with a new active_op_id.
func TestFix5_Item2_ConditionalResetNeverClobbersActiveInstall(t *testing.T) {
	st, uID, hostA := openTestStoreForGW(t)
	ctx := context.Background()
	fp, sshUser, gwID := "SHA256:fp-fix5-item2", "dev", "gw-fix5.local"

	hB, err := st.CreateHost(ctx, &store.Host{
		OwnerID:     uID,
		Name:        "box2",
		Address:     "127.0.0.1",
		Port:        22,
		Username:    sshUser,
		Auth:        "agent",
		Persistence: "tmux",
	})
	if err != nil {
		t.Fatalf("CreateHost B: %v", err)
	}

	if err := st.UpsertCorraldHost(ctx, &store.CorraldHost{
		HostID:           hostA,
		State:            StateRunning,
		BoundFingerprint: fp,
		BoundUsername:    sshUser,
		BoundGateway:     gwID,
		Version:          "0.3.2",
		Release:          2000,
	}); err != nil {
		t.Fatalf("UpsertCorraldHost A: %v", err)
	}
	if err := st.UpsertCorraldHost(ctx, &store.CorraldHost{
		HostID:           hB.ID,
		State:            StateInstalling,
		BoundFingerprint: fp,
		BoundUsername:    sshUser,
		BoundGateway:     gwID,
		ActiveOpID:       "op-concurrent-install",
	}); err != nil {
		t.Fatalf("UpsertCorraldHost B: %v", err)
	}

	if err := st.CorraldResetByBindingIfIdle(ctx, fp, sshUser, "op-old-cleanup"); err != nil {
		t.Fatalf("CorraldResetByBindingIfIdle: %v", err)
	}

	gotA, _ := st.CorraldHostByID(ctx, hostA)
	gotB, _ := st.CorraldHostByID(ctx, hB.ID)
	if gotA.State != StateOff {
		t.Fatalf("expected idle hostA reset to off, got %q", gotA.State)
	}
	if gotB.State != StateInstalling || gotB.ActiveOpID != "op-concurrent-install" {
		t.Fatalf("Fix5 Item #2 regression: CorraldResetByBindingIfIdle clobbered installing hostB: state=%q op=%q", gotB.State, gotB.ActiveOpID)
	}
}

// Regression test for Fix Round 5 Item #4 [P2] (shell fallback reentrancy):
// When binaries in LibDir were already deleted by an earlier uninstall attempt
// and only StateDir/removing (+ locks / empty dirs) remain, UninstallCommandSoleGateway
// MUST recover to result=removed instead of returning error=cannot_verify_exclusivity
// forever, while STILL failing closed when no tombstone exists, another gateway
// owns the tombstone, or corrald.lock is held.
func TestFix5_Item4_ShellSoleGatewayUninstallRecoversFromLeftoverTombstoneWithoutBinary(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("requires linux")
	}

	runShell := func(t *testing.T, script string) ([]byte, error) {
		t.Helper()
		cmd := exec.Command("sh", "-c", script)
		return cmd.CombinedOutput()
	}

	t.Run("recovers_matching_tombstone_without_binary", func(t *testing.T) {
		base := t.TempDir()
		libDir := filepath.Join(base, "lib", "corral")
		stateDir := filepath.Join(base, "state", "corral", "corrald", "aabbcc112233")
		if err := os.MkdirAll(libDir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(stateDir, 0o700); err != nil {
			t.Fatal(err)
		}
		// Simulate binaries already removed, leaving tombstone + lock files + sibling tool.
		if err := os.WriteFile(filepath.Join(stateDir, "removing"), []byte(`{"created_at":"2026-11-28T00:00:00Z","gateway":"gw-a.local"}`+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(stateDir, "op.lock"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(stateDir, "corrald.lock"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(libDir, ".corrald-shared.lock"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		// Unrelated sibling tool in LibDir MUST be preserved!
		sibling := filepath.Join(libDir, "code-server-keep")
		if err := os.WriteFile(sibling, []byte("keep"), 0o600); err != nil {
			t.Fatal(err)
		}

		script := UninstallCommandSoleGateway(libDir, stateDir, filepath.Join(stateDir, "corrald.sock"), "op-retry1", "gw-a.local")
		out, err := runShell(t, script)
		kv := ParseKeyValues(out)
		if err != nil || kv["result"] != "removed" {
			t.Fatalf("expected reentrant recovery result=removed, got err=%v\n%s", err, out)
		}
		if _, statErr := os.Lstat(stateDir); !os.IsNotExist(statErr) {
			t.Fatalf("expected stateDir removed, stat err=%v", statErr)
		}
		if _, statErr := os.Lstat(filepath.Join(libDir, ".corrald-shared.lock")); !os.IsNotExist(statErr) {
			t.Fatalf("expected .corrald-shared.lock removed, stat err=%v", statErr)
		}
		if _, statErr := os.Stat(sibling); statErr != nil {
			t.Fatalf("unrelated sibling tool in LibDir was deleted: %v", statErr)
		}
	})

	t.Run("refuses_when_tombstone_owned_by_other_gateway", func(t *testing.T) {
		base := t.TempDir()
		libDir := filepath.Join(base, "lib", "corral")
		stateDir := filepath.Join(base, "state", "corral", "corrald", "aabbcc112234")
		_ = os.MkdirAll(libDir, 0o700)
		_ = os.MkdirAll(stateDir, 0o700)
		_ = os.WriteFile(filepath.Join(stateDir, "removing"), []byte(`{"created_at":"2026-11-28T00:00:00Z","gateway":"gw-other.local"}`+"\n"), 0o600)

		script := UninstallCommandSoleGateway(libDir, stateDir, filepath.Join(stateDir, "corrald.sock"), "op-retry2", "gw-a.local")
		out, err := runShell(t, script)
		if err != nil || ParseKeyValues(out)["result"] != "other_gateways" {
			t.Fatalf("expected result=other_gateways for another gateway's tombstone, got err=%v\n%s", err, out)
		}
		if _, statErr := os.Stat(filepath.Join(stateDir, "removing")); statErr != nil {
			t.Fatalf("other gateway's tombstone must be preserved: %v", statErr)
		}
	})

	t.Run("fails_closed_without_tombstone", func(t *testing.T) {
		base := t.TempDir()
		libDir := filepath.Join(base, "lib", "corral")
		stateDir := filepath.Join(base, "state", "corral", "corrald", "aabbcc112235")
		_ = os.MkdirAll(libDir, 0o700)
		_ = os.MkdirAll(stateDir, 0o700)
		_ = os.WriteFile(filepath.Join(stateDir, "install.json"), []byte(`{"gateway":"gw-a.local"}`), 0o600)

		script := UninstallCommandSoleGateway(libDir, stateDir, filepath.Join(stateDir, "corrald.sock"), "op-retry3", "gw-a.local")
		out, err := runShell(t, script)
		if err == nil || ParseKeyValues(out)["error"] != "cannot_verify_exclusivity" {
			t.Fatalf("expected fail-closed cannot_verify_exclusivity without tombstone, got err=%v\n%s", err, out)
		}
		if _, statErr := os.Stat(filepath.Join(stateDir, "install.json")); statErr != nil {
			t.Fatalf("install.json must be preserved without tombstone: %v", statErr)
		}
	})

	t.Run("fails_closed_when_corrald_lock_held", func(t *testing.T) {
		if _, err := exec.LookPath("flock"); err != nil {
			t.Skip("flock binary not installed")
		}
		base := t.TempDir()
		libDir := filepath.Join(base, "lib", "corral")
		stateDir := filepath.Join(base, "state", "corral", "corrald", "aabbcc112236")
		_ = os.MkdirAll(libDir, 0o700)
		_ = os.MkdirAll(stateDir, 0o700)
		_ = os.WriteFile(filepath.Join(stateDir, "removing"), []byte(`{"created_at":"2026-11-28T00:00:00Z","gateway":"gw-a.local"}`+"\n"), 0o600)
		lockFile, err := os.OpenFile(filepath.Join(stateDir, "corrald.lock"), os.O_CREATE|os.O_RDWR, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		defer lockFile.Close()
		if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
			t.Fatalf("flock: %v", err)
		}
		defer syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN)

		script := UninstallCommandSoleGateway(libDir, stateDir, filepath.Join(stateDir, "corrald.sock"), "op-retry4", "gw-a.local")
		out, runErr := runShell(t, script)
		if runErr == nil || !strings.Contains(string(out), "error=stop_failed") {
			t.Fatalf("expected error=stop_failed when corrald.lock is held, got err=%v\n%s", runErr, out)
		}
	})
}

// Regression test for Fix Round 5 Item #5 [P2]:
// When ShutdownConditional fails because the response is lost (daemon closes
// the stream / EOF / timeout before reply arrives), the gateway MUST treat the
// outcome as unknown, mark the host remove_pending (NOT stopped), and set
// hasPendingCleanup=true so subsequent retry completes cleanup.
func TestFix5_Item5_ShutdownConditionalResponseLostMarksRemovePending(t *testing.T) {
	st, uID, hostID := openTestStoreForGW(t)
	ctx := context.Background()
	fp, sshUser, gwID := "SHA256:fp-fix5-item5", "dev", "gw-fix5.local"

	if err := st.UpsertCorraldHost(ctx, &store.CorraldHost{
		HostID:           hostID,
		State:            StateRunning,
		BoundFingerprint: fp,
		BoundUsername:    sshUser,
		BoundGateway:     gwID,
		Version:          "0.3.2",
		Release:          2000,
	}); err != nil {
		t.Fatalf("UpsertCorraldHost: %v", err)
	}

	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()
	defer serverConn.Close()

	go func() {
		for {
			b, err := proto.ReadFrame(serverConn)
			if err != nil {
				return
			}
			var req proto.Request
			if json.Unmarshal(b, &req) != nil {
				return
			}
			switch req.Method {
			case proto.MethodDaemonHealth:
				res, _ := json.Marshal(proto.HealthResult{PID: 5005, ConnectedGateways: 1})
				rb, _ := json.Marshal(proto.Response{ID: req.ID, Result: res})
				_ = proto.WriteFrame(serverConn, rb)
			case proto.MethodDaemonShutdown:
				// Simulate daemon accepting shutdown and exiting / dropping stream
				// BEFORE the RPC response reaches the gateway!
				_ = serverConn.Close()
				return
			}
		}
	}()

	cl := NewClient(clientConn, nil)
	defer cl.Close()

	m := NewManager(ManagerConfig{Store: st, GatewayID: gwID})
	defer m.Close()

	key := accountKey{Fingerprint: fp, Username: sshUser}
	sc := newSharedConn(key)
	sc.client = cl
	sc.hello = &proto.HelloResult{
		Version:  "0.3.2",
		Release:  2000,
		Features: proto.DefaultFeatures(),
	}
	sc.hostRefs[hostID] = &hostRef{HostID: hostID, OwnerID: uID, CorralUser: "tester"}
	m.mu.Lock()
	m.conns[key] = sc
	m.hostConn[hostID] = key
	m.mu.Unlock()

	m.SetAdminDisabled(ctx, true)

	deadline := time.Now().Add(5 * time.Second)
	for {
		ch, err := st.CorraldHostByID(ctx, hostID)
		if err == nil && ch.State == StateRemovePending && m.hasPendingCleanup.Load() {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("Fix5 Item #5 regression: expected StateRemovePending + hasPendingCleanup=true on lost ShutdownConditional response; got ch=%+v hasPending=%v", ch, m.hasPendingCleanup.Load())
		}
		time.Sleep(10 * time.Millisecond)
	}
}
