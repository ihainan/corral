package gw

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"slices"
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

// Part A: Deterministically reproduce the read-old-row -> concurrent-reinstall
// -> write-rebind interleaving across Status(), onHostConnectedOwned(), and
// acquireMatchingSSH(). Under the old whole-row UpsertCorraldHost / unconditional
// MarkCorraldRebind implementation, every subtest failed by clobbering the
// freshly installed `running` row back to `rebind`.
func TestRebindConditionalSQLDoesNotOverwriteConcurrentReinstall(t *testing.T) {
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
		h, err := st.CreateHost(ctx, &store.Host{OwnerID: u.ID, Name: "box", Address: "127.0.0.1", Port: 22, Username: "alice", Auth: "password"})
		if err != nil {
			t.Fatal(err)
		}

		m := proto.DefaultManifest()
		mHash, _ := proto.ManifestSHA256(m)
		mJSON, _ := proto.MarshalManifest(m)

		// Initial state before concurrent reinstall: bound to fp-old.
		initialState := StateRunning
		if source == "on_host_connected" {
			// Exact build-machine race from TestCorraldIdentityRebindAndHostKeyRotation:
			// row was already marked rebind with old bound_fingerprint when async
			// OnHostConnected read it, then StartInstall completed before the write!
			initialState = StateRebind
		}
		oldConsentAt := int64(1000)
		if err := st.UpsertCorraldHost(ctx, &store.CorraldHost{
			HostID:           h.ID,
			State:            initialState,
			ConsentBy:        &u.ID,
			ConsentAt:        oldConsentAt,
			BoundFingerprint: "fp-old",
			BoundUsername:    "alice",
			BoundGateway:     "gw-1",
			ManifestJSON:     string(mJSON),
			Version:          "0.3.1",
			Release:          1000,
		}); err != nil {
			t.Fatal(err)
		}
		_ = st.RecordSSHVerified(ctx, h.ID, "fp-new", "alice")

		entered := make(chan struct{})
		releaseHook := make(chan struct{})
		var hookOnce sync.Once

		// Set up a dummy SSH client for OnHostConnected / acquireMatchingSSH.
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

		mgr := NewManager(ManagerConfig{
			Store:     st,
			Dist:      &Dist{Version: "0.3.1", Release: 1000, Manifest: m, ManifestHash: mHash},
			GatewayID: "gw-1",
			SSH: &stubSSHProvider{
				bindings: map[int64][2]string{h.ID: {"alice", "fp-new"}},
				acquire: func(context.Context, int64, int64) (*ssh.Client, func(), string, string, error) {
					return sshClient, func() {}, "fp-new", "alice", nil
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

		// Attach a live sharedConn for the NEW key so we can also assert the stale
		// rebind path does NOT detach the freshly established connection.
		newKey := accountKey{Fingerprint: "fp-new", Username: "alice"}
		newSC := newSharedConn(newKey)
		c1, c2 := net.Pipe()
		defer c1.Close()
		defer c2.Close()
		newSC.client = NewClient(c1, nil)
		defer newSC.client.Close()
		newSC.hostRefs[h.ID] = &hostRef{HostID: h.ID, OwnerID: u.ID, CorralUser: "alice"}

		var wg sync.WaitGroup
		wg.Add(1)
		var statusOut *StatusResponse
		go func() {
			defer wg.Done()
			switch source {
			case "status":
				statusOut, _ = mgr.Status(ctx, u.ID, h.ID)
			case "on_host_connected":
				mgr.OnHostConnected(ctx, u.ID, h.ID, "alice", "fp-new", "alice", sshClient)
			case "acquire_matching_ssh":
				_, _, _ = mgr.acquireMatchingSSH(ctx, accountKey{Fingerprint: "fp-old", Username: "alice"}, []*hostRef{{HostID: h.ID, OwnerID: u.ID, CorralUser: "alice"}})
			}
		}()

		select {
		case <-entered:
		case <-time.After(3 * time.Second):
			t.Fatalf("timed out waiting for %s to hit BeforeRebindMark hook", source)
		}

		// While the stale caller is paused right before marking rebind, a full
		// re-enable/reinstall completes on fp-new with a fresh consent timestamp
		// and attaches newSC.
		newConsentAt := int64(2000)
		if err := st.UpsertCorraldHost(ctx, &store.CorraldHost{
			HostID:           h.ID,
			State:            StateRunning,
			ConsentBy:        &u.ID,
			ConsentAt:        newConsentAt,
			BoundFingerprint: "fp-new",
			BoundUsername:    "alice",
			BoundGateway:     "gw-1",
			ManifestJSON:     string(mJSON),
			Version:          "0.3.1",
			Release:          1000,
			ActiveOpID:       "",
		}); err != nil {
			t.Fatal(err)
		}
		mgr.mu.Lock()
		mgr.conns[newKey] = newSC
		mgr.hostConn[h.ID] = newKey
		mgr.mu.Unlock()

		// Release the paused goroutine so its conditional rebind SQL executes now.
		close(releaseHook)
		wg.Wait()

		after, err := st.CorraldHostByID(ctx, h.ID)
		if err != nil {
			t.Fatal(err)
		}
		if after.State != StateRunning {
			t.Fatalf("[%s] concurrent reinstall was clobbered: state=%q, want %q", source, after.State, StateRunning)
		}
		if after.BoundFingerprint != "fp-new" || after.ConsentAt != newConsentAt {
			t.Fatalf("[%s] binding/consent clobbered: fp=%q consent_at=%d", source, after.BoundFingerprint, after.ConsentAt)
		}
		mgr.mu.Lock()
		gotKey, attached := mgr.hostConn[h.ID]
		mgr.mu.Unlock()
		if !attached || gotKey != newKey {
			t.Fatalf("[%s] new sharedConn was erroneously detached: attached=%v key=%+v", source, attached, gotKey)
		}
		if source == "status" && statusOut != nil && statusOut.State != StateRunning {
			t.Fatalf("[status] expected refreshed state=running after skipped rebind, got %q", statusOut.State)
		}
	}

	t.Run("Status", func(t *testing.T) { runPath(t, "status") })
	t.Run("OnHostConnected", func(t *testing.T) { runPath(t, "on_host_connected") })
	t.Run("AcquireMatchingSSH", func(t *testing.T) { runPath(t, "acquire_matching_ssh") })
}

// B-1: After a rollback on NFS (where the shared symlink still points to the
// blacklisted release R2), tryRestartSharedConn MUST route through
// SelectStartBinary and launch the non-failed R1 binary, NEVER the symlink.
func TestAutoRestartUsesSelectStartBinarySkipsBlacklisted(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("linux test")
	}
	home := t.TempDir()
	sockDir, err := os.MkdirTemp("/tmp", "gw-b1-*")
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

	// Daemon only starts listening AFTER serve is called on the non-failed R1 binary.
	var serveStarted atomic.Bool
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				defer conn.Close()
				if !serveStarted.Load() {
					return
				}
				for {
					var req proto.Request
					if err := proto.ReadJSON(conn, &req); err != nil {
						return
					}
					switch req.Method {
					case proto.MethodDaemonHello:
						res, _ := json.Marshal(proto.HelloResult{
							Version: "0.3.1", Release: 1000, Protocol: proto.Protocol,
							Epoch: "ep-r1", PID: 101, Manifest: proto.DefaultManifest(), State: proto.StateRunning,
						})
						_ = proto.WriteJSON(conn, proto.Response{ID: req.ID, Result: res})
					case proto.MethodDaemonResume:
						res, _ := json.Marshal(proto.ResumeResult{Epoch: "ep-r1"})
						_ = proto.WriteJSON(conn, proto.Response{ID: req.ID, Result: res})
					case proto.MethodDaemonHealth:
						res, _ := json.Marshal(proto.HealthResult{PID: 101, Caps: map[string]string{"locate": "ok"}})
						_ = proto.WriteJSON(conn, proto.Response{ID: req.ID, Result: res})
					}
				}
			}(c)
		}
	}()

	stateDir := filepath.Join(home, ".local", "state", "corral", "corrald", "0123456789ab")
	libDir := filepath.Join(home, ".local", "lib", "corral")
	_ = os.MkdirAll(stateDir, 0o700)
	_ = os.MkdirAll(filepath.Join(libDir, "corrald-0.3.1-1000"), 0o700)
	_ = os.MkdirAll(filepath.Join(libDir, "corrald-0.3.2-2000"), 0o700)
	// Write host failed.json blacklisting release 2000.
	failedBytes, _ := json.Marshal(HostFailedRecord{
		Releases: []HostFailedEntry{{Release: 2000, Reason: "crash_loop_after_update"}},
	})
	_ = os.WriteFile(filepath.Join(stateDir, "failed.json"), failedBytes, 0o600)

	var mu sync.Mutex
	var serveCmds []string
	srv := testutil.StartSSHServer(t, "alice", "pass", home)
	srv.HandleExec(func(cmd string) func(ch ssh.Channel) {
		if strings.Contains(cmd, " serve ") {
			mu.Lock()
			serveCmds = append(serveCmds, cmd)
			mu.Unlock()
			serveStarted.Store(true)
			return func(ch ssh.Channel) {
				defer ch.Close()
				_, _ = ch.Write([]byte("result=ok\npid=101\nsocket=" + sockPath + "\n"))
				_, _ = ch.SendRequest("exit-status", false, ssh.Marshal(&struct{ Status uint32 }{0}))
			}
		}
		return testutil.ShellExec(home, func(string) bool { return true })(cmd)
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
	h, _ := st.CreateHost(ctx, &store.Host{OwnerID: u.ID, Name: "b1", Address: "127.0.0.1", Port: srv.Addr.Port, Username: "alice", Auth: "password"})
	_ = st.UpsertCorraldHost(ctx, &store.CorraldHost{
		HostID: h.ID, State: StateLost, BoundFingerprint: "fp-1", BoundUsername: "alice",
		BoundGateway: "gw-1", Version: "0.3.1", Release: 1000, FailedReleases: []int64{2000},
	})

	probe := &HostProbe{
		OS: "Linux", Machine: "x86_64", Arch: "amd64", UID: strconv.Itoa(os.Getuid()),
		Home: home, HomeWritable: true, FreeKB: 100000, MachineID: "0123456789ab",
		HasSymlink:    true,
		CurrentTarget: "corrald-0.3.2-2000/corrald",
		Versions:      []string{"corrald-0.3.1-1000", "corrald-0.3.2-2000"},
		LibDir:        libDir,
		CurrentBin:    filepath.Join(libDir, "corrald"),
		StateDir:      stateDir,
		FailedJSON:    filepath.Join(stateDir, "failed.json"),
		SocketPath:    sockPath,
	}

	m := proto.DefaultManifest()
	mHash, _ := proto.ManifestSHA256(m)
	mgr := NewManager(ManagerConfig{
		Store:         st,
		Dist:          &Dist{Version: "0.3.2", Release: 2000, Manifest: m, ManifestHash: mHash},
		GatewayID:     "gw-1",
		ProbeOverride: func(context.Context, *ssh.Client) (*HostProbe, error) { return probe, nil },
		SSH: &stubSSHProvider{
			bindings: map[int64][2]string{h.ID: {"alice", "fp-1"}},
			acquire: func(context.Context, int64, int64) (*ssh.Client, func(), string, string, error) {
				return sshClient, func() {}, "fp-1", "alice", nil
			},
		},
	})
	defer mgr.Close()

	key := accountKey{Fingerprint: "fp-1", Username: "alice"}
	sc := newSharedConn(key)
	sc.libDir = libDir
	sc.stateDir = stateDir
	sc.machineID = "0123456789ab"
	sc.socketPath = sockPath
	refs := []*hostRef{{HostID: h.ID, OwnerID: u.ID, CorralUser: "alice"}}
	sc.hostRefs[h.ID] = refs[0]
	mgr.mu.Lock()
	mgr.conns[key] = sc
	mgr.hostConn[h.ID] = key
	mgr.mu.Unlock()

	if ok := mgr.tryRestartSharedConn(sc, refs); !ok {
		t.Fatal("tryRestartSharedConn failed; expected restart on non-failed R1")
	}

	mu.Lock()
	cmds := append([]string(nil), serveCmds...)
	mu.Unlock()
	if len(cmds) != 1 {
		t.Fatalf("expected 1 serve command, got %d: %v", len(cmds), cmds)
	}
	wantBin := filepath.Join(libDir, "corrald-0.3.1-1000", "corrald")
	if !strings.Contains(cmds[0], wantBin) {
		t.Fatalf("tryRestartSharedConn did NOT launch non-failed R1 (%q); got cmd:\n%s", wantBin, cmds[0])
	}
	if strings.Contains(cmds[0], "corrald-0.3.2-2000") {
		t.Fatalf("tryRestartSharedConn must NEVER launch blacklisted R2; got cmd:\n%s", cmds[0])
	}
}

// B-3: When scope confirmation fails, is cancelled, or panics AFTER
// ClaimCorraldHostOp succeeds, active_op_id MUST be conditionally released so
// the host is never permanently wedged.
func TestScopeConfirmFailureCancelPanicReleasesActiveOpID(t *testing.T) {
	setup := func(t *testing.T, hook func(ctxCancel context.CancelFunc)) (*store.Store, *Manager, int64, int64, string, context.Context, context.CancelFunc) {
		t.Helper()
		st, err := store.Open(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = st.Close() })
		bg := context.Background()
		u, err := st.CreateUser(bg, "alice", "Alice", "hash", "admin")
		if err != nil {
			t.Fatal(err)
		}
		h, err := st.CreateHost(bg, &store.Host{OwnerID: u.ID, Name: "box", Address: "127.0.0.1", Port: 22, Username: "alice", Auth: "password"})
		if err != nil {
			t.Fatal(err)
		}

		baseManifest := proto.DefaultManifest()
		grownManifest := proto.DefaultManifest()
		grownManifest.Reads = append(grownManifest.Reads, "agent-logs:future")
		baseJSON, _ := proto.MarshalManifest(baseManifest)
		grownHash, _ := proto.ManifestSHA256(grownManifest)
		baseHash, _ := proto.ManifestSHA256(baseManifest)

		if err := st.UpsertCorraldHost(bg, &store.CorraldHost{
			HostID:           h.ID,
			State:            StateRunning,
			BoundFingerprint: "fp-1",
			BoundUsername:    "alice",
			BoundGateway:     "gw-1",
			ManifestJSON:     string(baseJSON),
			Version:          "0.3.1",
			Release:          1000,
		}); err != nil {
			t.Fatal(err)
		}

		callCtx, cancel := context.WithCancel(bg)
		mgr := NewManager(ManagerConfig{
			Store:     st,
			Dist:      &Dist{Version: "0.3.1", Release: 1000, Manifest: baseManifest, ManifestHash: baseHash},
			GatewayID: "gw-1",
			SSH:       &stubSSHProvider{bindings: map[int64][2]string{h.ID: {"alice", "fp-1"}}},
			AfterScopeClaim: func(hostID int64, opID string) {
				if hook != nil {
					hook(cancel)
				}
			},
		})
		t.Cleanup(mgr.Close)

		c1, c2 := net.Pipe()
		t.Cleanup(func() { _ = c1.Close(); _ = c2.Close() })
		cl := NewClient(c1, nil)
		t.Cleanup(func() { _ = cl.Close() })

		key := accountKey{Fingerprint: "fp-1", Username: "alice"}
		sc := newSharedConn(key)
		sc.client = cl
		sc.hello = &proto.HelloResult{Version: "0.3.2", Release: 2000, Manifest: grownManifest}
		mgr.mu.Lock()
		mgr.conns[key] = sc
		mgr.hostConn[h.ID] = key
		mgr.mu.Unlock()

		return st, mgr, u.ID, h.ID, grownHash, callCtx, cancel
	}

	t.Run("ctx_cancelled_after_claim_releases_op", func(t *testing.T) {
		st, mgr, uid, hid, grownHash, callCtx, cancel := setup(t, func(c context.CancelFunc) {
			c() // cancel caller context right after ClaimCorraldHostOp succeeds!
		})
		defer cancel()

		if _, err := mgr.StartUpdate(callCtx, uid, hid, "alice", grownHash, false); err == nil {
			t.Fatal("expected StartUpdate to fail after context cancellation")
		}
		after, err := st.CorraldHostByID(context.Background(), hid)
		if err != nil {
			t.Fatal(err)
		}
		if after.ActiveOpID != "" {
			t.Fatalf("host wedged with active_op_id=%q after cancelled scope confirm", after.ActiveOpID)
		}
		if op, err := st.ActiveCorraldOp(context.Background(), hid); err == nil {
			t.Fatalf("expected no active op remaining, got %+v", op)
		}
		// Subsequent ClaimCorraldHostOp MUST succeed now that active_op_id was released.
		mgr.cfg.AfterScopeClaim = nil
		if _, err := mgr.StartUpdate(context.Background(), uid, hid, "alice", grownHash, false); err != nil {
			t.Fatalf("subsequent StartUpdate failed on previously cancelled host: %v", err)
		}
	})

	t.Run("panic_after_claim_releases_op", func(t *testing.T) {
		st, mgr, uid, hid, grownHash, callCtx, cancel := setup(t, func(context.CancelFunc) {
			panic("injected panic after claim")
		})
		defer cancel()

		panicked := false
		func() {
			defer func() {
				if r := recover(); r != nil {
					panicked = true
				}
			}()
			_, _ = mgr.StartUpdate(callCtx, uid, hid, "alice", grownHash, false)
		}()
		if !panicked {
			t.Fatal("expected injected panic")
		}
		after, err := st.CorraldHostByID(context.Background(), hid)
		if err != nil {
			t.Fatal(err)
		}
		if after.ActiveOpID != "" {
			t.Fatalf("host wedged with active_op_id=%q after panic during scope confirm", after.ActiveOpID)
		}
	})
}

// B-4: On NFS cold-start update where another machine already raised the shared
// symlink to R3 (3000) while this gateway carries R2 (2000), if starting R3
// fails, ONLY R3 (3000) may be blacklisted — NEVER R2 (2000) — and rollback
// must launch the non-failed fallback via SelectStartBinary.
func TestNFSColdStartFailureBlacklistsActualStartedReleaseNotDist(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("linux test")
	}
	home := t.TempDir()
	libDir := filepath.Join(home, ".local", "lib", "corral")
	stateDir := filepath.Join(home, ".local", "state", "corral", "corrald", "0123456789ab")
	_ = os.MkdirAll(stateDir, 0o700)
	_ = os.MkdirAll(filepath.Join(libDir, "corrald-0.3.1-1000"), 0o700)
	_ = os.MkdirAll(filepath.Join(libDir, "corrald-0.3.2-2000"), 0o700)
	_ = os.MkdirAll(filepath.Join(libDir, "corrald-0.3.3-3000"), 0o700)

	var mu sync.Mutex
	var serveBins []string
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
			mu.Lock()
			serveBins = append(serveBins, cmd)
			mu.Unlock()
			// Starting the shared symlink (pointing to R3=3000) fails!
			// Fallback to R1=1000 succeeds.
			if strings.Contains(cmd, "corrald-0.3.1-1000") {
				return func(ch ssh.Channel) {
					defer ch.Close()
					_, _ = ch.Write([]byte("result=ok\npid=111\nsocket=" + filepath.Join(stateDir, "corrald.sock") + "\n"))
					_, _ = ch.SendRequest("exit-status", false, ssh.Marshal(&struct{ Status uint32 }{0}))
				}
			}
			return func(ch ssh.Channel) {
				defer ch.Close()
				_, _ = ch.Write([]byte("error=start_failed\ndetail=r3 crashed on startup\n"))
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
	h, _ := st.CreateHost(ctx, &store.Host{OwnerID: u.ID, Name: "nfs-b4", Address: "127.0.0.1", Port: srv.Addr.Port, Username: "alice", Auth: "password"})

	fakeBin := filepath.Join(t.TempDir(), "corrald")
	_ = os.WriteFile(fakeBin, []byte("fake-bin-v2"), 0o755)
	m := proto.DefaultManifest()
	mHash, _ := proto.ManifestSHA256(m)
	mJSON, _ := proto.MarshalManifest(m)
	dist := &Dist{
		Version:      "0.3.2",
		Release:      2000, // Gateway target is R2!
		Manifest:     m,
		ManifestHash: mHash,
		Binaries:     map[string]*ArchBinary{"amd64": {Arch: "amd64", Path: fakeBin, SHA256: "deadbeef"}},
	}

	// Host was previously on R1 (stopped), while another NFS peer already raised
	// the shared symlink to R3 (3000).
	_ = st.UpsertCorraldHost(ctx, &store.CorraldHost{
		HostID: h.ID, State: StateStopped, BoundFingerprint: "fp-1", BoundUsername: "alice",
		BoundGateway: "gw-1", ManifestJSON: string(mJSON), Version: "0.3.1", Release: 1000,
	})

	var probeCalls atomic.Int32
	mgr := NewManager(ManagerConfig{
		Store:             st,
		Dist:              dist,
		GatewayID:         "gw-1",
		BusyRetryInterval: 20 * time.Millisecond,
		BusyMaxRetries:    1,
		VerifySHA256Hook:  func(string) string { return "deadbeef" },
		ProbeOverride: func(context.Context, *ssh.Client) (*HostProbe, error) {
			n := probeCalls.Add(1)
			versions := []string{"corrald-0.3.1-1000", "corrald-0.3.3-3000"}
			if n >= 2 {
				versions = []string{"corrald-0.3.1-1000", "corrald-0.3.2-2000", "corrald-0.3.3-3000"}
			}
			return &HostProbe{
				OS: "Linux", Machine: "x86_64", Arch: "amd64", UID: strconv.Itoa(os.Getuid()),
				Home: home, HomeWritable: true, FreeKB: 100000, MachineID: "0123456789ab",
				HasSymlink:    true,
				CurrentTarget: "corrald-0.3.3-3000/corrald", // Shared symlink stays at R3!
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

	// No live sharedConn -> triggers cold-start upgrade path.
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
	if !slices.Contains(after.FailedReleases, 3000) {
		t.Fatalf("expected actual failed release 3000 in DB FailedReleases, got %v", after.FailedReleases)
	}
	if slices.Contains(after.FailedReleases, 2000) {
		t.Fatalf("dist.Release 2000 was falsely blacklisted when R3 failed! got %v", after.FailedReleases)
	}

	// Verify host failed.json ALSO recorded 3000 and NOT 2000.
	raw, err := os.ReadFile(filepath.Join(stateDir, "failed.json"))
	if err != nil {
		t.Fatalf("read failed.json: %v", err)
	}
	var hostFailed HostFailedRecord
	if err := json.Unmarshal(raw, &hostFailed); err != nil {
		t.Fatalf("unmarshal failed.json: %v", err)
	}
	rels := hostFailed.ReleaseList()
	if !slices.Contains(rels, 3000) || slices.Contains(rels, 2000) {
		t.Fatalf("host failed.json releases = %v, want [3000] without 2000", rels)
	}

	// Verify fallback launched a non-failed binary (NOT R3).
	mu.Lock()
	cmds := append([]string(nil), serveBins...)
	mu.Unlock()
	if len(cmds) < 2 {
		t.Fatalf("expected primary serve + rollback serve, got %d cmds: %v", len(cmds), cmds)
	}
	if strings.Contains(cmds[1], "corrald-0.3.3-3000") || strings.Contains(cmds[1], filepath.Join(libDir, "corrald")+"'") {
		t.Fatalf("rollback serve should not retry R3 symlink, got:\n%s", cmds[1])
	}
}

// B-5: When a concurrent hot-update returns handoff_in_progress and reconnect
// finds another gateway already upgraded the daemon to a strictly higher R3
// (3000 > dist.Release 2000), the crash-rollback window MUST record the actual
// running R3 (NOT R2) and MUST NOT record this gateway's stale pre-op R1
// rollback info.
func TestConcurrentHandoffHigherReleaseRecordsActualCrashWindow(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("linux test")
	}
	home := t.TempDir()
	sockDir, err := os.MkdirTemp("/tmp", "gw-b5-*")
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
							// Concurrent gateway upgraded host to strictly higher R3!
							ver, rel = "0.3.3", 3000
						}
						res, _ := json.Marshal(proto.HelloResult{
							Version: ver, Release: rel, Protocol: proto.Protocol,
							Epoch: "ep-" + string(rune('0'+gen)), PID: int(200 + gen),
							Manifest: proto.DefaultManifest(), State: proto.StateRunning,
						})
						_ = proto.WriteJSON(conn, proto.Response{ID: req.ID, Result: res})
					case proto.MethodDaemonResume:
						res, _ := json.Marshal(proto.ResumeResult{Epoch: "ep-" + string(rune('0'+gen))})
						_ = proto.WriteJSON(conn, proto.Response{ID: req.ID, Result: res})
					case proto.MethodDaemonHealth:
						res, _ := json.Marshal(proto.HealthResult{Caps: map[string]string{"locate": "ok"}, PID: int(200 + gen)})
						_ = proto.WriteJSON(conn, proto.Response{ID: req.ID, Result: res})
					case proto.MethodDaemonHandoff:
						_ = proto.WriteJSON(conn, proto.Response{
							ID:    req.ID,
							Error: &proto.RPCError{Code: proto.ErrHandoffInProgress, Message: "other gateway handing off to R3", Retryable: true},
						})
						return
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
	h, _ := st.CreateHost(ctx, &store.Host{OwnerID: u.ID, Name: "b5", Address: "127.0.0.1", Port: srv.Addr.Port, Username: "alice", Auth: "password"})

	fakeBin := filepath.Join(t.TempDir(), "corrald")
	_ = os.WriteFile(fakeBin, []byte("fake-bin-v2"), 0o755)
	m := proto.DefaultManifest()
	mHash, _ := proto.ManifestSHA256(m)
	mJSON, _ := proto.MarshalManifest(m)
	dist := &Dist{
		Version:      "0.3.2",
		Release:      2000, // Gateway target is R2
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
		t.Fatalf("expected running version 0.3.3/3000, got %s/%d", after.Version, after.Release)
	}
	if after.UpdateTargetRel != 3000 || after.UpdateTargetVer != "0.3.3" {
		t.Fatalf("crash window target = %s/%d, want actual running 0.3.3/3000 (NOT dist 0.3.2/2000)", after.UpdateTargetVer, after.UpdateTargetRel)
	}
	if after.UpdatedFromRel != 2000 || after.UpdatedFromVer != "0.3.2" || after.UpdatedFromTarget != "corrald-0.3.2-2000/corrald" {
		t.Fatalf("expected rollback target to be dist R2 (0.3.2/2000) when accepting higher R3, got from=%s/%d target=%q",
			after.UpdatedFromVer, after.UpdatedFromRel, after.UpdatedFromTarget)
	}

	sc := mgr.getSharedConn(accountKey{Fingerprint: "fp-1", Username: "alice"})
	if sc == nil {
		t.Fatal("expected sharedConn")
	}
	sc.mu.Lock()
	gotMemTargetRel := sc.updateTargetRel
	gotMemPrevRel := sc.prevRelease
	gotMemPrevTarget := sc.prevRelTarget
	sc.mu.Unlock()
	if gotMemTargetRel != 3000 || gotMemPrevRel != 2000 || gotMemPrevTarget != "corrald-0.3.2-2000/corrald" {
		t.Fatalf("sharedConn crash window = target:%d prev:%d prevTarget:%q, want target:3000 prev:2000 prevTarget:\"corrald-0.3.2-2000/corrald\"",
			gotMemTargetRel, gotMemPrevRel, gotMemPrevTarget)
	}
}
