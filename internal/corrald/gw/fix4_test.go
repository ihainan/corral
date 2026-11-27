package gw

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/ihainan/corral/internal/corrald/proto"
	"github.com/ihainan/corral/internal/store"
	"github.com/ihainan/corral/internal/testutil"
)

// Fix4 #1: When a stale callback enters observing identity A while the host was
// bound to fp-0, and the host is concurrently rebound to identity B (fp-B),
// bound ("fp-B") != observed ("fp-A") is STILL true — only comparing the stored
// binding against the caller's read/expected binding ("fp-0") prevents
// clobbering B to rebind.
func TestRebindCASDoesNotClobberNewBindingBWhenCallbackObservedStaleA(t *testing.T) {
	runPath := func(t *testing.T, source string) {
		t.Helper()
		st, err := store.Open(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		defer st.Close()
		ctx := context.Background()
		u, err := st.CreateUser(ctx, "alice", "Alice", "hash", "admin")
		if err != nil {
			t.Fatal(err)
		}
		h, err := st.CreateHost(ctx, &store.Host{OwnerID: u.ID, Name: "cas-box", Address: "127.0.0.1", Port: 22, Username: "alice", Auth: "password"})
		if err != nil {
			t.Fatal(err)
		}

		m := proto.DefaultManifest()
		mHash, _ := proto.ManifestSHA256(m)
		mJSON, _ := proto.MarshalManifest(m)

		// Initially bound to fp-0.
		if err := st.UpsertCorraldHost(ctx, &store.CorraldHost{
			HostID:           h.ID,
			State:            StateRunning,
			ConsentBy:        &u.ID,
			ConsentAt:        1000,
			BoundFingerprint: "fp-0",
			BoundUsername:    "alice",
			BoundGateway:     "gw-1",
			ManifestJSON:     string(mJSON),
			Version:          "0.3.1",
			Release:          1000,
		}); err != nil {
			t.Fatal(err)
		}
		_ = st.RecordSSHVerified(ctx, h.ID, "fp-B", "alice")

		entered := make(chan struct{})
		releaseHook := make(chan struct{})
		var hookOnce sync.Once

		srv := testutil.StartSSHServer(t, "alice", "pass", t.TempDir())
		cc := &ssh.ClientConfig{
			User:            "alice",
			Auth:            []ssh.AuthMethod{ssh.Password("pass")},
			HostKeyCallback: ssh.InsecureIgnoreHostKey(),
			Timeout:         5 * time.Second,
		}
		sshClient, err := ssh.Dial("tcp", srv.Addr.String(), cc)
		if err != nil {
			t.Fatal(err)
		}
		defer sshClient.Close()

		// Provider initially reports observed identity fp-A (mismatching fp-0).
		var curFP atomic.Value
		curFP.Store("fp-A")

		mgr := NewManager(ManagerConfig{
			Store:     st,
			Dist:      &Dist{Version: "0.3.1", Release: 1000, Manifest: m, ManifestHash: mHash},
			GatewayID: "gw-1",
			SSH: &stubSSHProvider{
				bindings: map[int64][2]string{h.ID: {"alice", "fp-A"}},
				acquire: func(context.Context, int64, int64) (*ssh.Client, func(), string, string, error) {
					return sshClient, func() {}, curFP.Load().(string), "alice", nil
				},
			},
			BeforeRebindMark: func(hostID int64, src string) {
				if hostID == h.ID && src == source {
					hookOnce.Do(func() {
						close(entered)
						<-releaseHook
					})
				}
			},
		})
		defer mgr.Close()

		// Prepare a live sharedConn for the NEW identity B (fp-B).
		keyB := accountKey{Fingerprint: "fp-B", Username: "alice"}
		scB := newSharedConn(keyB)
		c1, c2 := net.Pipe()
		defer c1.Close()
		defer c2.Close()
		scB.client = NewClient(c1, nil)
		defer scB.client.Close()
		scB.hostRefs[h.ID] = &hostRef{HostID: h.ID, OwnerID: u.ID, CorralUser: "alice"}

		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			switch source {
			case "status":
				_, _ = mgr.Status(ctx, u.ID, h.ID)
			case "on_host_connected":
				mgr.OnHostConnected(ctx, u.ID, h.ID, "alice", "fp-A", "alice", sshClient)
			case "acquire_matching_ssh":
				_, _, _ = mgr.acquireMatchingSSH(ctx, accountKey{Fingerprint: "fp-0", Username: "alice"}, []*hostRef{{HostID: h.ID, OwnerID: u.ID, CorralUser: "alice"}})
			}
		}()

		select {
		case <-entered:
		case <-time.After(3 * time.Second):
			t.Fatalf("timed out waiting for %s to enter BeforeRebindMark", source)
		}

		// Concurrently rebind the host to a THIRD identity B (fp-B != fp-0 AND fp-B != fp-A!).
		curFP.Store("fp-B")
		if err := st.UpsertCorraldHost(ctx, &store.CorraldHost{
			HostID:           h.ID,
			State:            StateRunning,
			ConsentBy:        &u.ID,
			ConsentAt:        2000,
			BoundFingerprint: "fp-B",
			BoundUsername:    "alice",
			BoundGateway:     "gw-1",
			ManifestJSON:     string(mJSON),
			Version:          "0.3.1",
			Release:          1000,
		}); err != nil {
			t.Fatal(err)
		}
		mgr.mu.Lock()
		mgr.conns[keyB] = scB
		mgr.hostConn[h.ID] = keyB
		mgr.mu.Unlock()

		close(releaseHook)
		wg.Wait()

		after, err := st.CorraldHostByID(ctx, h.ID)
		if err != nil {
			t.Fatal(err)
		}
		if after.State != StateRunning || after.BoundFingerprint != "fp-B" {
			t.Fatalf("[%s] stale callback observing fp-A clobbered rebound fp-B! got state=%q fp=%q", source, after.State, after.BoundFingerprint)
		}
		mgr.mu.Lock()
		gotKey, attached := mgr.hostConn[h.ID]
		mgr.mu.Unlock()
		if !attached || gotKey != keyB {
			t.Fatalf("[%s] sharedConn for fp-B was detached by stale fp-A callback! attached=%v key=%+v", source, attached, gotKey)
		}
	}

	t.Run("Status", func(t *testing.T) { runPath(t, "status") })
	t.Run("OnHostConnected", func(t *testing.T) { runPath(t, "on_host_connected") })
	t.Run("AcquireMatchingSSH", func(t *testing.T) { runPath(t, "acquire_matching_ssh") })
}

// Fix4 #2: Startup recovery and reconciliation MUST clear orphaned active_op_id
// pointing to done/failed/cancelled/missing ops (while preserving live pending/
// running ops), and finishOpCancelled MUST finalize op + host in one transaction.
func TestStaleActiveOpIDRecoveredOnStartupAndReconcileAndFinishOpCancelledAtomic(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	u, _ := st.CreateUser(ctx, "alice", "Alice", "hash", "admin")
	hCancelled, _ := st.CreateHost(ctx, &store.Host{OwnerID: u.ID, Name: "h1", Address: "127.0.0.1", Port: 22, Username: "alice", Auth: "password"})
	hMissing, _ := st.CreateHost(ctx, &store.Host{OwnerID: u.ID, Name: "h2", Address: "127.0.0.2", Port: 22, Username: "alice", Auth: "password"})
	hLive, _ := st.CreateHost(ctx, &store.Host{OwnerID: u.ID, Name: "h3", Address: "127.0.0.3", Port: 22, Username: "alice", Auth: "password"})
	hReconcile, _ := st.CreateHost(ctx, &store.Host{OwnerID: u.ID, Name: "h4", Address: "127.0.0.4", Port: 22, Username: "alice", Auth: "password"})

	// Simulate pre-fix crash: op is already cancelled, but corrald_hosts still holds active_op_id.
	_ = st.CreateCorraldOp(ctx, &store.CorraldOp{ID: "op-cancelled-1", HostID: hCancelled.ID, Kind: OpInstall, State: OpCancelled, Step: "upload", CreatedBy: u.ID}, false)
	_ = st.UpsertCorraldHost(ctx, &store.CorraldHost{HostID: hCancelled.ID, State: StateInstalling, ActiveOpID: "op-cancelled-1"})

	// Orphaned active_op_id referencing a nonexistent op row.
	_ = st.UpsertCorraldHost(ctx, &store.CorraldHost{HostID: hMissing.ID, State: StateRunning, ActiveOpID: "op-nonexistent"})

	// Genuine live running op MUST NOT be cleared by startup cleanup.
	_ = st.CreateCorraldOp(ctx, &store.CorraldOp{ID: "op-live-1", HostID: hLive.ID, Kind: OpUpdate, State: OpRunning, Step: "upload", CreatedBy: u.ID}, false)
	_ = st.UpsertCorraldHost(ctx, &store.CorraldHost{HostID: hLive.ID, State: StateUpdating, ActiveOpID: "op-live-1"})

	// 1. Direct reconcile check BEFORE NewManager clears stale ops: UpdateCorraldHostForReconcile
	//    with expectedOpID="" MUST still succeed when active_op_id points to a cancelled op.
	_ = st.CreateCorraldOp(ctx, &store.CorraldOp{ID: "op-cancelled-4", HostID: hReconcile.ID, Kind: OpInstall, State: OpCancelled, Step: "upload", CreatedBy: u.ID}, false)
	_ = st.UpsertCorraldHost(ctx, &store.CorraldHost{HostID: hReconcile.ID, State: StateInstalling, ActiveOpID: "op-cancelled-4"})
	recHost, _ := st.CorraldHostByID(ctx, hReconcile.ID)
	recHost.State = StateOff
	if ok, err := st.UpdateCorraldHostForReconcile(ctx, recHost, nil, ""); err != nil || !ok {
		t.Fatalf("UpdateCorraldHostForReconcile failed to clear cancelled active_op_id: ok=%v err=%v", ok, err)
	}
	got4, _ := st.CorraldHostByID(ctx, hReconcile.ID)
	if got4.ActiveOpID != "" || got4.State != StateOff {
		t.Fatalf("expected reconciled host4 off with empty active_op_id, got state=%q op=%q", got4.State, got4.ActiveOpID)
	}

	// 2. Startup recovery via NewManager clears orphaned active_op_id on h1 and h2,
	//    while leaving live h3 untouched.
	m := proto.DefaultManifest()
	mHash, _ := proto.ManifestSHA256(m)
	mgr := NewManager(ManagerConfig{
		Store:     st,
		Dist:      &Dist{Version: "0.3.1", Release: 1000, Manifest: m, ManifestHash: mHash},
		GatewayID: "gw-1",
		SSH:       &stubSSHProvider{},
	})
	defer mgr.Close()

	got1, _ := st.CorraldHostByID(ctx, hCancelled.ID)
	if got1.ActiveOpID != "" {
		t.Fatalf("startup recovery failed to clear cancelled active_op_id on h1: got %q", got1.ActiveOpID)
	}
	got2, _ := st.CorraldHostByID(ctx, hMissing.ID)
	if got2.ActiveOpID != "" {
		t.Fatalf("startup recovery failed to clear nonexistent active_op_id on h2: got %q", got2.ActiveOpID)
	}
	if claimed, err := st.ClaimCorraldHostOp(ctx, hMissing.ID, "op-new-2", StateUpdating, ""); err != nil || !claimed {
		t.Fatalf("expected ClaimCorraldHostOp to succeed after recovery, got claimed=%v err=%v", claimed, err)
	}
	got3, _ := st.CorraldHostByID(ctx, hLive.ID)
	if got3.ActiveOpID != "op-live-1" {
		t.Fatalf("startup recovery erroneously cleared live running op on h3: got %q", got3.ActiveOpID)
	}

	// 3. finishOpCancelled atomically cancels op and clears host active_op_id.
	opCancel := &store.CorraldOp{ID: "op-fin-cancel", HostID: hCancelled.ID, Kind: OpInstall, State: OpRunning, Step: "upload", CreatedBy: u.ID}
	_ = st.CreateCorraldOp(ctx, opCancel, false)
	_ = st.UpsertCorraldHost(ctx, &store.CorraldHost{HostID: hCancelled.ID, State: StateInstalling, ActiveOpID: opCancel.ID})
	chCancel, _ := st.CorraldHostByID(ctx, hCancelled.ID)
	mgr.finishOpCancelled(opCancel, chCancel)
	afterH, _ := st.CorraldHostByID(ctx, hCancelled.ID)
	afterOp, _ := st.LatestCorraldOp(ctx, hCancelled.ID)
	if afterH.State != StateOff || afterH.ActiveOpID != "" || afterOp.State != OpCancelled {
		t.Fatalf("finishOpCancelled mismatch: host=(%q,%q) op=%q", afterH.State, afterH.ActiveOpID, afterOp.State)
	}
}

// Fix4 #3: SelectStartBinary MUST ALWAYS return the concrete immutable binary
// path inside corrald-<ver>-<rel>/corrald, NEVER the mutable shared symlink
// (`probe.CurrentBin`), even when the symlink target is not blacklisted.
func TestSelectStartBinaryNeverReturnsMutableSharedSymlink(t *testing.T) {
	probe := &HostProbe{
		LibDir:        "/home/alice/.local/lib/corral",
		CurrentBin:    "/home/alice/.local/lib/corral/corrald",
		HasSymlink:    true,
		CurrentTarget: "corrald-0.3.2-2000/corrald",
		Versions:      []string{"corrald-0.3.1-1000", "corrald-0.3.2-2000"},
	}

	cand, ok := SelectStartBinary(probe, nil, StartSelectOptions{})
	if !ok {
		t.Fatal("expected SelectStartBinary ok=true")
	}
	want := "/home/alice/.local/lib/corral/corrald-0.3.2-2000/corrald"
	if cand.Path != want {
		t.Fatalf("SelectStartBinary returned %q, want concrete versioned path %q (must NEVER return mutable symlink %q)", cand.Path, want, probe.CurrentBin)
	}
	if cand.Path == probe.CurrentBin {
		t.Fatalf("SelectStartBinary returned mutable shared symlink %q", cand.Path)
	}

	// Even with an unversioned/dangling symlink and no version dirs, must NOT fall back to probe.CurrentBin.
	dangling := &HostProbe{
		LibDir:        "/home/alice/.local/lib/corral",
		CurrentBin:    "/home/alice/.local/lib/corral/corrald",
		HasSymlink:    true,
		CurrentTarget: "",
		Versions:      nil,
	}
	if got, ok := SelectStartBinary(dangling, nil, StartSelectOptions{}); ok {
		t.Fatalf("expected ok=false when no versioned binary exists, got %+v", got)
	}
}

// Fix4 #4: When cold-start update fails AND rollback cannot start or connect
// to a live daemon (either no eligible rollback binary or rollback serve fails),
// host MUST transition to StateError (so Retry is allowed), NEVER StateRunning.
func TestColdStartRollbackFailureEntersStateErrorAndAllowsRetry(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("linux test")
	}

	runCase := func(t *testing.T, mode string) {
		t.Helper()
		home := t.TempDir()
		libDir := filepath.Join(home, ".local", "lib", "corral")
		stateDir := filepath.Join(home, ".local", "state", "corral", "corrald", "0123456789ab")
		_ = os.MkdirAll(stateDir, 0o700)
		_ = os.MkdirAll(filepath.Join(libDir, "corrald-0.3.1-1000"), 0o700)
		_ = os.MkdirAll(filepath.Join(libDir, "corrald-0.3.2-2000"), 0o700)

		srv := testutil.StartSSHServer(t, "alice", "pass", home)
		realExec := testutil.ShellExec(home, func(string) bool { return true })
		srv.HandleExec(func(cmd string) func(ch ssh.Channel) {
			if strings.Contains(cmd, " selftest") || strings.Contains(cmd, " install ") {
				return func(ch ssh.Channel) {
					defer ch.Close()
					_, _ = ch.Write([]byte("result=ok\nbinary=" + filepath.Join(libDir, "corrald-0.3.2-2000", "corrald") + "\n"))
					_, _ = ch.SendRequest("exit-status", false, ssh.Marshal(&struct{ Status uint32 }{0}))
				}
			}
			if strings.Contains(cmd, " serve ") {
				return func(ch ssh.Channel) {
					defer ch.Close()
					_, _ = ch.Write([]byte("error=start_failed\ndetail=daemon failed to start\n"))
					_, _ = ch.SendRequest("exit-status", false, ssh.Marshal(&struct{ Status uint32 }{1}))
				}
			}
			return realExec(cmd)
		})

		cc := &ssh.ClientConfig{
			User:            "alice",
			Auth:            []ssh.AuthMethod{ssh.Password("pass")},
			HostKeyCallback: ssh.InsecureIgnoreHostKey(),
			Timeout:         5 * time.Second,
		}
		sshClient, err := ssh.Dial("tcp", srv.Addr.String(), cc)
		if err != nil {
			t.Fatal(err)
		}
		defer sshClient.Close()

		st, err := store.Open(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		defer st.Close()
		ctx := context.Background()
		u, _ := st.CreateUser(ctx, "alice", "Alice", "hash", "admin")
		h, _ := st.CreateHost(ctx, &store.Host{OwnerID: u.ID, Name: "cs-err", Address: "127.0.0.1", Port: srv.Addr.Port, Username: "alice", Auth: "password"})

		fakeBin := filepath.Join(t.TempDir(), "corrald")
		_ = os.WriteFile(fakeBin, []byte("fake-bin-v2"), 0o755)
		m := proto.DefaultManifest()
		mHash, _ := proto.ManifestSHA256(m)
		mJSON, _ := proto.MarshalManifest(m)
		dist := &Dist{
			Version:      "0.3.2",
			Release:      2000,
			Manifest:     m,
			ManifestHash: mHash,
			Binaries:     map[string]*ArchBinary{"amd64": {Arch: "amd64", Path: fakeBin, SHA256: "deadbeef"}},
		}

		versions := []string{"corrald-0.3.1-1000", "corrald-0.3.2-2000"}
		if mode == "no_rollback_binary" {
			versions = []string{"corrald-0.3.2-2000"}
		}

		_ = st.UpsertCorraldHost(ctx, &store.CorraldHost{
			HostID: h.ID, State: StateStopped, BoundFingerprint: "fp-1", BoundUsername: "alice",
			BoundGateway: "gw-1", ManifestJSON: string(mJSON), Version: "0.3.1", Release: 1000,
		})

		mgr := NewManager(ManagerConfig{
			Store:             st,
			Dist:              dist,
			GatewayID:         "gw-1",
			BusyRetryInterval: 20 * time.Millisecond,
			BusyMaxRetries:    1,
			VerifySHA256Hook:  func(string) string { return "deadbeef" },
			ProbeOverride: func(context.Context, *ssh.Client) (*HostProbe, error) {
				return &HostProbe{
					OS: "Linux", Machine: "x86_64", Arch: "amd64", UID: strconv.Itoa(os.Getuid()),
					Home: home, HomeWritable: true, FreeKB: 100000, MachineID: "0123456789ab",
					HasSymlink:    true,
					CurrentTarget: "corrald-0.3.2-2000/corrald",
					Versions:      versions,
					LibDir:        libDir,
					CurrentBin:    filepath.Join(libDir, "corrald"),
					StateDir:      stateDir,
					FailedJSON:    filepath.Join(stateDir, "failed.json"),
					SocketPath:    filepath.Join(stateDir, "corrald.sock"),
				}, nil
			},
			SSH: &stubSSHProvider{
				bindings: map[int64][2]string{h.ID: {"alice", "fp-1"}},
				acquire: func(context.Context, int64, int64) (*ssh.Client, func(), string, string, error) {
					return sshClient, func() {}, "fp-1", "alice", nil
				},
			},
		})
		defer mgr.Close()

		if _, err := mgr.StartUpdate(ctx, u.ID, h.ID, "alice", mHash, false); err != nil {
			t.Fatalf("StartUpdate: %v", err)
		}
		if err := mgr.WaitOpsIdle(ctx); err != nil {
			t.Fatal(err)
		}

		after, err := st.CorraldHostByID(ctx, h.ID)
		if err != nil {
			t.Fatal(err)
		}
		if after.State != StateError {
			t.Fatalf("[%s] expected StateError when cold-start rollback failed to connect, got %q", mode, after.State)
		}
		if after.ActiveOpID != "" {
			t.Fatalf("[%s] expected active_op_id cleared, got %q", mode, after.ActiveOpID)
		}
		if after.LastError == "" {
			t.Fatalf("[%s] expected LastError populated with failure reason", mode)
		}
		if tr := Transition(toSnapshot(after), Event{Kind: EvRetry}); !tr.Allowed {
			t.Fatalf("[%s] expected Retry to be allowed from resulting state %q", mode, after.State)
		}
	}

	t.Run("rollback_serve_fails", func(t *testing.T) { runCase(t, "rollback_serve_fails") })
	t.Run("no_rollback_binary", func(t *testing.T) { runCase(t, "no_rollback_binary") })
}

// Fix4 #5: When daemon.handoff succeeds (alreadyAtTarget == false) and the
// subsequent verify reconnect discovers another gateway already upgraded the
// host to R3 (3000 > dist R2 2000), the crash-rollback window MUST record
// target=R3 AND rollback target=R2 (dist), NOT pre-op R1!
func TestPostHandoffVerifyReconnectHigherReleaseSetsDistAsRollbackTarget(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("linux test")
	}
	home := t.TempDir()
	sockDir, err := os.MkdirTemp("/tmp", "gw-f4-5-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(sockDir) })
	sockPath := filepath.Join(sockDir, "corrald.sock")

	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	var connSeq atomic.Int32
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			seq := connSeq.Add(1)
			go func(conn net.Conn, gen int32) {
				defer conn.Close()
				for {
					var req proto.Request
					if err := proto.ReadJSON(conn, &req); err != nil {
						return
					}
					switch req.Method {
					case proto.MethodDaemonHello:
						ver, rel := "0.3.1", int64(1000)
						if gen >= 2 {
							// Post-handoff verify reconnect sees another gateway upgraded to R3!
							ver, rel = "0.3.3", 3000
						}
						res, _ := json.Marshal(proto.HelloResult{
							Version: ver, Release: rel, Protocol: proto.Protocol,
							Epoch: "ep-" + string(rune('0'+gen)), PID: int(300 + gen),
							Manifest: proto.DefaultManifest(), State: proto.StateRunning,
						})
						_ = proto.WriteJSON(conn, proto.Response{ID: req.ID, Result: res})
					case proto.MethodDaemonResume:
						res, _ := json.Marshal(proto.ResumeResult{Epoch: "ep-" + string(rune('0'+gen))})
						_ = proto.WriteJSON(conn, proto.Response{ID: req.ID, Result: res})
					case proto.MethodDaemonHealth:
						res, _ := json.Marshal(proto.HealthResult{Caps: map[string]string{"locate": "ok"}, PID: int(300 + gen)})
						_ = proto.WriteJSON(conn, proto.Response{ID: req.ID, Result: res})
					case proto.MethodDaemonHandoff:
						// Handoff SUCCEEDS (alreadyAtTarget remains false)!
						res, _ := json.Marshal(proto.HandoffResult{PID: 302, Release: 2000})
						_ = proto.WriteJSON(conn, proto.Response{ID: req.ID, Result: res})
					}
				}
			}(c, seq)
		}
	}()

	srv := testutil.StartSSHServer(t, "alice", "pass", home)
	realExec := testutil.ShellExec(home, func(string) bool { return true })
	srv.HandleExec(func(cmd string) func(ch ssh.Channel) {
		if strings.Contains(cmd, " selftest") || strings.Contains(cmd, " install ") {
			return func(ch ssh.Channel) {
				defer ch.Close()
				_, _ = ch.Write([]byte("result=ok\nbinary=/tmp/fake-v2\n"))
				_, _ = ch.SendRequest("exit-status", false, ssh.Marshal(&struct{ Status uint32 }{0}))
			}
		}
		return realExec(cmd)
	})
	cc := &ssh.ClientConfig{
		User:            "alice",
		Auth:            []ssh.AuthMethod{ssh.Password("pass")},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         5 * time.Second,
	}
	sshClient, err := ssh.Dial("tcp", srv.Addr.String(), cc)
	if err != nil {
		t.Fatal(err)
	}
	defer sshClient.Close()

	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	u, _ := st.CreateUser(ctx, "alice", "Alice", "hash", "admin")
	h, _ := st.CreateHost(ctx, &store.Host{OwnerID: u.ID, Name: "f4-5", Address: "127.0.0.1", Port: srv.Addr.Port, Username: "alice", Auth: "password"})

	fakeBin := filepath.Join(t.TempDir(), "corrald")
	_ = os.WriteFile(fakeBin, []byte("fake-bin-v2"), 0o755)
	m := proto.DefaultManifest()
	mHash, _ := proto.ManifestSHA256(m)
	mJSON, _ := proto.MarshalManifest(m)
	dist := &Dist{
		Version:      "0.3.2",
		Release:      2000,
		Manifest:     m,
		ManifestHash: mHash,
		Binaries:     map[string]*ArchBinary{"amd64": {Arch: "amd64", Path: fakeBin, SHA256: "deadbeef"}},
	}

	stateDir := filepath.Join(home, ".local", "state", "corral", "corrald", "0123456789ab")
	_ = os.MkdirAll(stateDir, 0o700)
	_ = os.MkdirAll(filepath.Join(home, ".local", "lib", "corral"), 0o755)
	probe := &HostProbe{
		OS: "Linux", Machine: "x86_64", Arch: "amd64", UID: strconv.Itoa(os.Getuid()),
		Home: home, HomeWritable: true, FreeKB: 100000, MachineID: "0123456789ab",
		HasSymlink:    true,
		CurrentTarget: "corrald-0.3.1-1000/corrald",
		Versions:      []string{"corrald-0.3.1-1000", "corrald-0.3.2-2000", "corrald-0.3.3-3000"},
		LibDir:        filepath.Join(home, ".local", "lib", "corral"),
		CurrentBin:    filepath.Join(home, ".local", "lib", "corral", "corrald"),
		StateDir:      stateDir,
		FailedJSON:    filepath.Join(stateDir, "failed.json"),
		SocketPath:    sockPath,
	}

	_ = st.UpsertCorraldHost(ctx, &store.CorraldHost{
		HostID: h.ID, State: StateRunning, BoundFingerprint: "fp-1", BoundUsername: "alice",
		BoundGateway: "gw-1", ManifestJSON: string(mJSON), Version: "0.3.1", Release: 1000,
	})

	mgr := NewManager(ManagerConfig{
		Store:             st,
		Dist:              dist,
		GatewayID:         "gw-1",
		BusyRetryInterval: 25 * time.Millisecond,
		BusyMaxRetries:    3,
		HandoffTimeout:    2 * time.Second,
		VerifySHA256Hook:  func(string) string { return "deadbeef" },
		ProbeOverride:     func(context.Context, *ssh.Client) (*HostProbe, error) { return probe, nil },
		SSH: &stubSSHProvider{
			bindings: map[int64][2]string{h.ID: {"alice", "fp-1"}},
			acquire: func(context.Context, int64, int64) (*ssh.Client, func(), string, string, error) {
				return sshClient, func() {}, "fp-1", "alice", nil
			},
		},
	})
	defer mgr.Close()

	ch, _ := st.CorraldHostByID(ctx, h.ID)
	if err := mgr.attachAndHello(ctx, u.ID, h.ID, "alice", "fp-1", "alice", sshClient, nil, probe, sockPath, ch, ""); err != nil {
		t.Fatalf("attachAndHello: %v", err)
	}

	if _, err := mgr.StartUpdate(ctx, u.ID, h.ID, "alice", mHash, false); err != nil {
		t.Fatalf("StartUpdate: %v", err)
	}
	if err := mgr.WaitOpsIdle(ctx); err != nil {
		t.Fatal(err)
	}

	after, err := st.CorraldHostByID(ctx, h.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Version != "0.3.3" || after.Release != 3000 {
		t.Fatalf("expected running 0.3.3/3000, got %s/%d", after.Version, after.Release)
	}
	if after.UpdateTargetVer != "0.3.3" || after.UpdateTargetRel != 3000 {
		t.Fatalf("expected crash window target 0.3.3/3000, got %s/%d", after.UpdateTargetVer, after.UpdateTargetRel)
	}
	if after.UpdatedFromVer != "0.3.2" || after.UpdatedFromRel != 2000 || after.UpdatedFromTarget != "corrald-0.3.2-2000/corrald" {
		t.Fatalf("expected rollback target to be dist R2 (0.3.2/2000), NOT pre-op R1; got from=%s/%d target=%q",
			after.UpdatedFromVer, after.UpdatedFromRel, after.UpdatedFromTarget)
	}

	sc := mgr.getSharedConn(accountKey{Fingerprint: "fp-1", Username: "alice"})
	if sc == nil {
		t.Fatal("expected sharedConn")
	}
	sc.mu.Lock()
	gotPrevVer, gotPrevRel, gotPrevTarget := sc.prevVersion, sc.prevRelease, sc.prevRelTarget
	sc.mu.Unlock()
	if gotPrevVer != "0.3.2" || gotPrevRel != 2000 || gotPrevTarget != "corrald-0.3.2-2000/corrald" {
		t.Fatalf("sharedConn prev = (%s/%d, %q), want (0.3.2/2000, corrald-0.3.2-2000/corrald)", gotPrevVer, gotPrevRel, gotPrevTarget)
	}
}
