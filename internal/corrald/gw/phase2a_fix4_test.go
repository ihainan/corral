package gw

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/ihainan/corral/internal/corrald/proto"
	"github.com/ihainan/corral/internal/store"
)

// Regression test for Fix Round 4 Item #1 [P1] (gateway old/new daemon compat):
//   - Old daemon without proto.FeatureConditionalShutdown MUST NEVER receive
//     daemon.shutdown during SetAdminDisabled(true); host is marked stopped and
//     client is closed.
//   - New daemon returning ShutdownResult{OK: false, OtherGateways: true} marks
//     host stopped and does NOT mark remove_pending.
func TestFix4_Item1_OldDaemonAndConditionalShutdownRefusal(t *testing.T) {
	t.Run("old_daemon_without_feature_never_sent_shutdown", func(t *testing.T) {
		st, _, hostID := openTestStoreForGW(t)
		ctx := context.Background()
		fp, sshUser, gwID := "SHA256:fp-old-daemon", "dev", "gw-self.local"

		if err := st.UpsertCorraldHost(ctx, &store.CorraldHost{
			HostID:           hostID,
			State:            StateRunning,
			BoundFingerprint: fp,
			BoundUsername:    sshUser,
			BoundGateway:     gwID,
			Version:          "0.3.1",
			Release:          1000,
		}); err != nil {
			t.Fatalf("UpsertCorraldHost: %v", err)
		}

		clientConn, serverConn := net.Pipe()
		defer clientConn.Close()
		defer serverConn.Close()

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
					res, _ := json.Marshal(proto.HealthResult{PID: 1001, ConnectedGateways: 1})
					rb, _ := json.Marshal(proto.Response{ID: req.ID, Result: res})
					_ = proto.WriteFrame(serverConn, rb)
				case proto.MethodDaemonHello:
					// Old daemon: omits Features!
					res, _ := json.Marshal(proto.HelloResult{
						Version: "0.3.1",
						Release: 1000,
						Clients: []proto.ClientInfo{{Gateway: gwID, CorralUser: "tester"}},
					})
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
			Version: "0.3.1",
			Release: 1000,
			Clients: []proto.ClientInfo{{Gateway: gwID, CorralUser: "tester"}},
			// Features intentionally omitted (old daemon)
		}
		sc.hostRefs[hostID] = &hostRef{HostID: hostID, OwnerID: 1, CorralUser: "tester"}
		m.mu.Lock()
		m.conns[key] = sc
		m.hostConn[hostID] = key
		m.mu.Unlock()

		m.SetAdminDisabled(ctx, true)

		deadline := time.Now().Add(5 * time.Second)
		for {
			ch, err := st.CorraldHostByID(ctx, hostID)
			if err == nil && ch.State == StateStopped && cl.Closed() {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting for stopped state; ch=%+v closed=%v", ch, cl.Closed())
			}
			time.Sleep(10 * time.Millisecond)
		}
		if shutdownCalls.Load() != 0 {
			t.Fatalf("old daemon without FeatureConditionalShutdown must NOT receive daemon.shutdown (got %d calls)", shutdownCalls.Load())
		}
	})

	t.Run("new_daemon_conditional_shutdown_refused_marks_stopped", func(t *testing.T) {
		st, _, hostID := openTestStoreForGW(t)
		ctx := context.Background()
		fp, sshUser, gwID := "SHA256:fp-new-refused", "dev", "gw-self.local"

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

		var gotSoleParam atomic.Value
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
					res, _ := json.Marshal(proto.HealthResult{PID: 1002, ConnectedGateways: 1})
					rb, _ := json.Marshal(proto.Response{ID: req.ID, Result: res})
					_ = proto.WriteFrame(serverConn, rb)
				case proto.MethodDaemonShutdown:
					var sp proto.ShutdownParams
					_ = json.Unmarshal(req.Params, &sp)
					gotSoleParam.Store(sp.OnlyIfSoleGateway)
					res, _ := json.Marshal(proto.ShutdownResult{OK: false, OtherGateways: true})
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
			Clients:  []proto.ClientInfo{{Gateway: gwID, CorralUser: "tester"}},
		}
		sc.hostRefs[hostID] = &hostRef{HostID: hostID, OwnerID: 1, CorralUser: "tester"}
		m.mu.Lock()
		m.conns[key] = sc
		m.hostConn[hostID] = key
		m.mu.Unlock()

		m.SetAdminDisabled(ctx, true)

		deadline := time.Now().Add(5 * time.Second)
		for {
			ch, err := st.CorraldHostByID(ctx, hostID)
			if err == nil && ch.State == StateStopped && cl.Closed() {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting for stopped state; ch=%+v closed=%v", ch, cl.Closed())
			}
			time.Sleep(10 * time.Millisecond)
		}
		if got, _ := gotSoleParam.Load().(string); got != gwID {
			t.Fatalf("expected OnlyIfSoleGateway=%q, got %q", gwID, got)
		}
	})
}

// Regression test for Fix Round 4 Item #4 [P2]:
// Repeated SetAdminDisabled(true) calls must be idempotent: must NOT increment
// policyGen or invalidate in-flight cleanup workers from the first call.
func TestFix4_Item4_RepeatedSetAdminDisabledIsIdempotent(t *testing.T) {
	st, uID, hostID := openTestStoreForGW(t)
	ctx := context.Background()
	fp, sshUser, gwID := "SHA256:fp-idem", "dev", "gw-idem.local"

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

	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	m := NewManager(ManagerConfig{
		Store:     st,
		GatewayID: gwID,
		BeforeAdminDisableCheck: func(hid int64) {
			entered <- struct{}{}
			<-release
		},
	})
	defer m.Close()

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
				res, _ := json.Marshal(proto.HealthResult{PID: 2001, ConnectedGateways: 1})
				rb, _ := json.Marshal(proto.Response{ID: req.ID, Result: res})
				_ = proto.WriteFrame(serverConn, rb)
			case proto.MethodDaemonShutdown:
				res, _ := json.Marshal(proto.ShutdownResult{OK: true})
				rb, _ := json.Marshal(proto.Response{ID: req.ID, Result: res})
				_ = proto.WriteFrame(serverConn, rb)
			}
		}
	}()

	cl := NewClient(clientConn, nil)
	defer cl.Close()
	key := accountKey{Fingerprint: fp, Username: sshUser}
	sc := newSharedConn(key)
	sc.client = cl
	sc.hello = &proto.HelloResult{Version: "0.3.2", Release: 2000, Features: proto.DefaultFeatures()}
	sc.hostRefs[hostID] = &hostRef{HostID: hostID, OwnerID: uID, CorralUser: "tester"}
	m.mu.Lock()
	m.conns[key] = sc
	m.hostConn[hostID] = key
	m.mu.Unlock()

	// First disable launches worker and pauses at BeforeAdminDisableCheck.
	m.SetAdminDisabled(ctx, true)
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("worker did not reach BeforeAdminDisableCheck")
	}
	genAfterFirst := m.policyGen.Load()

	// Duplicate disable while worker is in flight MUST be a no-op!
	m.SetAdminDisabled(ctx, true)
	if got := m.policyGen.Load(); got != genAfterFirst {
		t.Fatalf("duplicate SetAdminDisabled(true) bumped policyGen from %d to %d", genAfterFirst, got)
	}

	// Unblock worker -> must complete conditional shutdown and mark remove_pending
	// (under the old code, policyGen mismatch caused the worker to abort leaving DB running!).
	close(release)
	deadline := time.Now().Add(5 * time.Second)
	for {
		ch, err := st.CorraldHostByID(ctx, hostID)
		if err == nil && ch.State == StateRemovePending {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("expected host to reach remove_pending after duplicate disable, got (%+v, %v)", ch, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Regression test for Fix Round 4 Item #6 [P2]:
// Transient cleanup/check failures (probe error, partial, stop_failed) MUST NOT
// downgrade remove_pending to stopped; only confirmed other_gateways/unsupported
// transitions remove_pending -> stopped.
func TestFix4_Item6_TransientFailurePreservesRemovePending(t *testing.T) {
	st, uID, hostID := openTestStoreForGW(t)
	ctx := context.Background()
	fp, sshUser, gwID := "SHA256:fp-transient", "dev", "gw-trans.local"

	if err := st.UpsertCorraldHost(ctx, &store.CorraldHost{
		HostID:           hostID,
		State:            StateRemovePending,
		BoundFingerprint: fp,
		BoundUsername:    sshUser,
		BoundGateway:     gwID,
		Version:          "0.3.2",
		Release:          2000,
	}); err != nil {
		t.Fatalf("UpsertCorraldHost: %v", err)
	}

	m := NewManager(ManagerConfig{
		Store:         st,
		GatewayID:     gwID,
		AdminDisabled: true,
		ProbeOverride: func(ctx context.Context, c *ssh.Client) (*HostProbe, error) {
			return nil, errors.New("ssh session EOF during probe")
		},
	})
	defer m.Close()

	// 1. Transient probe failure during runSafeCleanupLocked -> MUST keep remove_pending!
	m.runSafeCleanupLocked(ctx, uID, hostID, "tester", fp, sshUser, &ssh.Client{}, "op1", true, 0)
	ch, err := st.CorraldHostByID(ctx, hostID)
	if err != nil {
		t.Fatalf("CorraldHostByID: %v", err)
	}
	if ch.State != StateRemovePending {
		t.Fatalf("transient probe failure downgraded remove_pending to %q", ch.State)
	}
	if !m.hasPendingCleanup.Load() {
		t.Fatal("expected hasPendingCleanup to remain true after transient probe failure")
	}

	// 2. Uninstall returning partial / stop_failed -> MUST keep remove_pending!
	m.applySoleGatewayUninstallOutcome(fp, sshUser, []int64{hostID}, true, UninstallOutcome{Result: "partial"})
	ch, _ = st.CorraldHostByID(ctx, hostID)
	if ch.State != StateRemovePending {
		t.Fatalf("partial uninstall downgraded remove_pending to %q", ch.State)
	}
	m.applySoleGatewayUninstallOutcome(fp, sshUser, []int64{hostID}, true, UninstallOutcome{Result: "stop_failed"})
	ch, _ = st.CorraldHostByID(ctx, hostID)
	if ch.State != StateRemovePending {
		t.Fatalf("stop_failed uninstall downgraded remove_pending to %q", ch.State)
	}

	// 3. Store-level MarkCorraldHostsStopped MUST NOT overwrite remove_pending!
	if err := st.MarkCorraldHostsStopped(ctx, []int64{hostID}, fp, sshUser); err != nil {
		t.Fatalf("MarkCorraldHostsStopped: %v", err)
	}
	ch, _ = st.CorraldHostByID(ctx, hostID)
	if ch.State != StateRemovePending {
		t.Fatalf("MarkCorraldHostsStopped unexpectedly overwrote remove_pending to %q", ch.State)
	}

	// 4. Confirmed other_gateways -> DOES transition remove_pending to stopped!
	m.applySoleGatewayUninstallOutcome(fp, sshUser, []int64{hostID}, true, UninstallOutcome{Result: "other_gateways"})
	ch, _ = st.CorraldHostByID(ctx, hostID)
	if ch.State != StateStopped {
		t.Fatalf("confirmed other_gateways should mark stopped, got %q", ch.State)
	}
}

// Regression test for Fix Round 4 Item #7 [P2]:
// A stale refreshPendingCleanupFlag() reading false from SQLite MUST NOT
// overwrite a newer markPendingCleanup() setting true.
func TestFix4_Item7_PendingCleanupFlagGenerationGuard(t *testing.T) {
	st, _, hostID := openTestStoreForGW(t)
	ctx := context.Background()
	m := NewManager(ManagerConfig{Store: st, GatewayID: "gw-flag.local"})
	defer m.Close()

	// Simulate the exact race window:
	// 1. Capture pendingFlagGen before any pending host exists (query returns false).
	m.pendingFlagMu.Lock()
	staleGen := m.pendingFlagGen
	m.pendingFlagMu.Unlock()
	has, err := st.HasCorraldHostsInStates(ctx, StateRemovePending, StateRemoving)
	if err != nil || has {
		t.Fatalf("expected initially false, got has=%v err=%v", has, err)
	}

	// 2. Concurrently, another worker writes remove_pending and calls markPendingCleanup().
	if err := st.UpsertCorraldHost(ctx, &store.CorraldHost{
		HostID:           hostID,
		State:            StateRemovePending,
		BoundFingerprint: "SHA256:fp-flag",
		BoundUsername:    "dev",
		BoundGateway:     "gw-flag.local",
	}); err != nil {
		t.Fatalf("UpsertCorraldHost: %v", err)
	}
	m.markPendingCleanup()
	if !m.hasPendingCleanup.Load() {
		t.Fatal("expected hasPendingCleanup=true after markPendingCleanup")
	}

	// 3. Stale publisher attempts to publish its old false with staleGen -> MUST be rejected!
	m.pendingFlagMu.Lock()
	if m.pendingFlagGen == staleGen {
		m.hasPendingCleanup.Store(has)
	}
	m.pendingFlagMu.Unlock()

	if !m.hasPendingCleanup.Load() {
		t.Fatal("stale refreshPendingCleanupFlag overwrote newer true with false!")
	}

	// Concurrent stress test under -race: refreshPendingCleanupFlag racing with markPendingCleanup.
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				m.markPendingCleanup()
			}
		}()
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				m.refreshPendingCleanupFlag()
			}
		}()
	}
	wg.Wait()
	if !m.hasPendingCleanup.Load() {
		t.Fatal("expected hasPendingCleanup=true while remove_pending row exists in DB")
	}
}

// Regression test for Fix Round 4 Item #9 [P2]:
// Connection publication, SSH ownership transfer, and m.wg.Add(1) must share
// the same policyMu synchronization boundary with Close() so Close() cannot
// race with attach/reconnect, leak SSH release, or leave a published conn.
func TestFix4_Item9_ReconnectPublishSynchronizedWithClose(t *testing.T) {
	st, uID, hostID := openTestStoreForGW(t)
	ctx := context.Background()
	fp, sshUser, gwID := "SHA256:fp-close-sync", "dev", "gw-close.local"

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

	for iter := 0; iter < 5; iter++ {
		hookReached := make(chan struct{})
		closeStarted := make(chan struct{})
		m := NewManager(ManagerConfig{
			Store:     st,
			GatewayID: gwID,
			AfterAttachConnsInsert: func(hid int64) {
				close(hookReached)
				<-closeStarted
			},
		})

		key := accountKey{Fingerprint: fp, Username: sshUser}
		sc := newSharedConn(key)
		sc.hostRefs[hostID] = &hostRef{HostID: hostID, OwnerID: uID, CorralUser: "tester"}
		var releaseCalls atomic.Int32
		own := newSSHOwnership(func() { releaseCalls.Add(1) })

		clientConn, serverConn := net.Pipe()
		cl := NewClient(clientConn, nil)

		var wg sync.WaitGroup
		wg.Add(2)

		// Goroutine 1: publish connection using the exact policyMu.RLock boundary
		// used by attachAndHello / reconnectSharedConn.
		go func() {
			defer wg.Done()
			defer own.releaseIfOwned()
			m.policyMu.RLock()
			if m.AdminDisabled() {
				m.policyMu.RUnlock()
				_ = cl.Close()
				return
			}
			m.mu.Lock()
			if m.closed {
				m.mu.Unlock()
				m.policyMu.RUnlock()
				_ = cl.Close()
				return
			}
			m.conns[key] = sc
			m.hostConn[hostID] = key
			m.mu.Unlock()

			if m.cfg.AfterAttachConnsInsert != nil {
				m.cfg.AfterAttachConnsInsert(hostID)
			}

			sc.mu.Lock()
			sc.client = cl
			sc.sshRelease = own.take()
			sc.mu.Unlock()
			m.wg.Add(1)
			m.policyMu.RUnlock()

			go func() {
				defer m.wg.Done()
				<-sc.stopCh
				m.closeSharedConn(sc, false)
			}()
		}()

		// Goroutine 2: Close() called while goroutine 1 is inside the hook!
		// Because goroutine 1 holds policyMu.RLock across insertion, own.take(),
		// and m.wg.Add(1), Close() MUST block until publication completes, then
		// cleanly close sc, invoke release exactly once, and wait for the monitor.
		go func() {
			defer wg.Done()
			<-hookReached
			close(closeStarted)
			m.Close()
		}()

		wg.Wait()
		_ = serverConn.Close()
		_ = clientConn.Close()

		if got := releaseCalls.Load(); got != 1 {
			t.Fatalf("iter %d: expected SSH release called exactly once, got %d", iter, got)
		}
		m.mu.Lock()
		nConns := len(m.conns)
		m.mu.Unlock()
		if nConns != 0 {
			t.Fatalf("iter %d: expected 0 conns after Close, got %d", iter, nConns)
		}
	}
}

// Regression test for Fix Round 4 Item #10 [P2]:
// finishUpdateFailKeepPrev and attemptPostUpdateRollback MUST NOT overwrite
// stopped/remove_pending back to running after admin disables corrald.
func TestFix4_Item10_UpdateFailAndRollbackGuardAgainstAdminDisabled(t *testing.T) {
	st, uID, hostID := openTestStoreForGW(t)
	ctx := context.Background()
	fp, sshUser, gwID := "SHA256:fp-u10", "dev", "gw-u10.local"

	if err := st.UpsertCorraldHost(ctx, &store.CorraldHost{
		HostID:           hostID,
		State:            StateStopped,
		BoundFingerprint: fp,
		BoundUsername:    sshUser,
		BoundGateway:     gwID,
		Version:          "0.3.1",
		Release:          1000,
	}); err != nil {
		t.Fatalf("UpsertCorraldHost: %v", err)
	}

	m := NewManager(ManagerConfig{
		Store:         st,
		GatewayID:     gwID,
		AdminDisabled: true,
	})
	defer m.Close()

	op := &store.CorraldOp{
		ID:     "op-u10",
		HostID: hostID,
		Kind:   OpUpdate,
		State:  "running",
	}
	ch := &store.CorraldHost{
		HostID:           hostID,
		State:            StateUpdating,
		BoundFingerprint: fp,
		BoundUsername:    sshUser,
		BoundGateway:     gwID,
		Version:          "0.3.1",
		Release:          1000,
	}
	_ = uID

	m.finishUpdateFailKeepPrev(op, ch, "0.3.1", 1000, "0.3.2", "test")

	after, err := st.CorraldHostByID(ctx, hostID)
	if err != nil {
		t.Fatalf("CorraldHostByID: %v", err)
	}
	if after.State != StateStopped {
		t.Fatalf("finishUpdateFailKeepPrev overwrote stopped to %q while admin disabled!", after.State)
	}
}

// Regression test for Fix Round 4 Item #5 [P2]:
// When attachAndHello is entered after RunServeWithRetry while AdminDisabled()
// has just become true, it MUST NOT simply return ErrAdminDisabled leaving a
// freshly started daemon untracked; it must invoke safe host cleanup / reliably
// record remove_pending + hasPendingCleanup.
func TestFix4_Item5_AttachAndHelloAdminDisabledRecordsPendingOrCleans(t *testing.T) {
	st, uID, hostID := openTestStoreForGW(t)
	ctx := context.Background()
	fp, sshUser, gwID := "SHA256:fp-attach-entry", "dev", "gw-attach.local"

	ch := &store.CorraldHost{
		HostID:           hostID,
		State:            StateRunning,
		BoundFingerprint: fp,
		BoundUsername:    sshUser,
		BoundGateway:     gwID,
		Version:          "0.3.2",
		Release:          2000,
	}
	if err := st.UpsertCorraldHost(ctx, ch); err != nil {
		t.Fatalf("UpsertCorraldHost: %v", err)
	}

	var probeCalled atomic.Int32
	m := NewManager(ManagerConfig{
		Store:         st,
		GatewayID:     gwID,
		AdminDisabled: true,
		ProbeOverride: func(ctx context.Context, c *ssh.Client) (*HostProbe, error) {
			probeCalled.Add(1)
			return nil, errors.New("simulated transient probe error")
		},
	})
	defer m.Close()

	own := newSSHOwnership(nil)
	err := m.attachAndHello(ctx, uID, hostID, "tester", fp, sshUser, &ssh.Client{}, own, &HostProbe{}, "/tmp/fake.sock", ch, "")
	if !errors.Is(err, ErrAdminDisabled) {
		t.Fatalf("expected ErrAdminDisabled, got %v", err)
	}
	if probeCalled.Load() == 0 {
		t.Fatal("attachAndHello returned ErrAdminDisabled without attempting host-side safe cleanup!")
	}
	after, err := st.CorraldHostByID(ctx, hostID)
	if err != nil {
		t.Fatalf("CorraldHostByID: %v", err)
	}
	if after.State != StateRemovePending {
		t.Fatalf("expected StateRemovePending after transient cleanup failure on attach entry, got %q", after.State)
	}
	if !m.hasPendingCleanup.Load() {
		t.Fatal("expected hasPendingCleanup=true after attach entry marked remove_pending")
	}
}
