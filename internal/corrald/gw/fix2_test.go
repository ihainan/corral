package gw

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/ihainan/corral/internal/corrald/proto"
	"github.com/ihainan/corral/internal/store"
	"github.com/ihainan/corral/internal/testutil"
)

type stubSSHProvider struct {
	bindings map[int64][2]string // hostID -> {sshUser, fp}
	acquire  func(ctx context.Context, ownerID, hostID int64) (*ssh.Client, func(), string, string, error)
}

func (p *stubSSHProvider) AcquireSSH(ctx context.Context, ownerID, hostID int64) (*ssh.Client, func(), string, string, error) {
	if p.acquire != nil {
		return p.acquire(ctx, ownerID, hostID)
	}
	return nil, nil, "", "", ErrNotConnected
}

func (p *stubSSHProvider) HostBinding(_ context.Context, _ int64, hostID int64) (string, string, error) {
	b := p.bindings[hostID]
	return b[0], b[1], nil
}

func (p *stubSSHProvider) UserLabel(_ context.Context, _ int64) string { return "alice" }

// C-1: Scope confirmation in StartUpdate must acquire operation ownership and
// use conditional DB updates so it can never overwrite a concurrent removing state.
func TestScopeConfirmCannotOverwriteConcurrentRemove(t *testing.T) {
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

	baseManifest := proto.DefaultManifest()
	grownManifest := proto.DefaultManifest()
	grownManifest.Reads = append(grownManifest.Reads, "agent-logs:future")
	baseJSON, _ := proto.MarshalManifest(baseManifest)
	grownHash, _ := proto.ManifestSHA256(grownManifest)
	baseHash, _ := proto.ManifestSHA256(baseManifest)

	// Host is ALREADY in removing state with an active remove op (simulating concurrent StartRemove).
	if err := st.CreateCorraldOp(ctx, &store.CorraldOp{
		ID:        "rem-op-1",
		HostID:    h.ID,
		Kind:      OpRemove,
		State:     OpRunning,
		Step:      "mark",
		CreatedBy: u.ID,
	}, false); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertCorraldHost(ctx, &store.CorraldHost{
		HostID:           h.ID,
		State:            StateRemoving,
		BoundFingerprint: "fp-1",
		BoundUsername:    "alice",
		BoundGateway:     "gw-1",
		ManifestJSON:     string(baseJSON),
		Version:          "0.3.1",
		Release:          1000,
		ActiveOpID:       "rem-op-1",
	}); err != nil {
		t.Fatal(err)
	}

	c1, c2 := net.Pipe()
	defer c1.Close()
	defer c2.Close()
	cl := NewClient(c1, nil)
	defer cl.Close()

	mgr := NewManager(ManagerConfig{
		Store:     st,
		Dist:      &Dist{Version: "0.3.1", Release: 1000, Manifest: baseManifest, ManifestHash: baseHash},
		GatewayID: "gw-1",
		SSH:       &stubSSHProvider{bindings: map[int64][2]string{h.ID: {"alice", "fp-1"}}},
	})
	defer mgr.Close()

	key := accountKey{Fingerprint: "fp-1", Username: "alice"}
	sc := newSharedConn(key)
	sc.client = cl
	sc.hello = &proto.HelloResult{Version: "0.3.2", Release: 2000, Manifest: grownManifest}
	mgr.mu.Lock()
	mgr.conns[key] = sc
	mgr.hostConn[h.ID] = key
	mgr.mu.Unlock()

	if _, err := mgr.StartUpdate(ctx, u.ID, h.ID, "alice", grownHash, false); err == nil {
		t.Fatal("expected StartUpdate scope confirmation to be rejected while host is removing")
	}

	after, err := st.CorraldHostByID(ctx, h.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.State != StateRemoving || after.ActiveOpID != "rem-op-1" || after.ManifestJSON != string(baseJSON) {
		t.Fatalf("concurrent remove state was overwritten! got state=%q active_op_id=%q", after.State, after.ActiveOpID)
	}
}

// C-3: Shared metadata (running metrics, users, files, consent) must ONLY be
// returned when corrald_ssh_verified is present — consent_by during an
// unauthenticated install attempt must NOT leak another user's sharedConn info.
func TestCanSeeSharedRequiresSSHVerifiedNotConsentBy(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	victim, err := st.CreateUser(ctx, "alice", "Alice", "hash", "admin")
	if err != nil {
		t.Fatal(err)
	}
	attacker, err := st.CreateUser(ctx, "mallory", "Mallory", "hash", "user")
	if err != nil {
		t.Fatal(err)
	}
	hVictim, err := st.CreateHost(ctx, &store.Host{OwnerID: victim.ID, Name: "v", Address: "127.0.0.1", Port: 22, Username: "ubuntu", Auth: "password"})
	if err != nil {
		t.Fatal(err)
	}
	hAttacker, err := st.CreateHost(ctx, &store.Host{OwnerID: attacker.ID, Name: "m", Address: "127.0.0.1", Port: 22, Username: "ubuntu", Auth: "password"})
	if err != nil {
		t.Fatal(err)
	}

	m := proto.DefaultManifest()
	mHash, _ := proto.ManifestSHA256(m)
	_ = st.RecordSSHVerified(ctx, hVictim.ID, "fp-shared", "ubuntu")
	_ = st.UpsertCorraldHost(ctx, &store.CorraldHost{
		HostID:           hVictim.ID,
		State:            StateRunning,
		ConsentBy:        &victim.ID,
		BoundFingerprint: "fp-shared",
		BoundUsername:    "ubuntu",
		BoundGateway:     "gw-1",
		Version:          "0.3.1",
		Release:          1000,
		Files:            []string{"/home/ubuntu/.local/lib/corral/corrald"},
	})
	// Attacker started install (consent_by = attacker.ID, state = installing)
	// WITHOUT ever passing SSH authentication (no corrald_ssh_verified row).
	_ = st.UpsertCorraldHost(ctx, &store.CorraldHost{
		HostID:           hAttacker.ID,
		State:            StateInstalling,
		ConsentBy:        &attacker.ID,
		BoundFingerprint: "fp-shared",
		BoundUsername:    "ubuntu",
		BoundGateway:     "gw-1",
		Files:            []string{"/home/ubuntu/.local/lib/corral/corrald"},
	})

	c1, c2 := net.Pipe()
	defer c1.Close()
	defer c2.Close()
	cl := NewClient(c1, nil)
	defer cl.Close()

	mgr := NewManager(ManagerConfig{
		Store:     st,
		Dist:      &Dist{Version: "0.3.1", Release: 1000, Manifest: m, ManifestHash: mHash},
		GatewayID: "gw-1",
		SSH: &stubSSHProvider{bindings: map[int64][2]string{
			hVictim.ID:   {"ubuntu", "fp-shared"},
			hAttacker.ID: {"ubuntu", "fp-shared"},
		}},
	})
	defer mgr.Close()

	key := accountKey{Fingerprint: "fp-shared", Username: "ubuntu"}
	sc := newSharedConn(key)
	sc.client = cl
	sc.health = &proto.HealthResult{PID: 4242, UptimeS: 99, RSSKB: 12345}
	sc.hello = &proto.HelloResult{PID: 4242, Clients: []proto.ClientInfo{{Gateway: "gw-1", CorralUser: "alice"}}}
	mgr.mu.Lock()
	mgr.conns[key] = sc
	mgr.hostConn[hVictim.ID] = key
	mgr.mu.Unlock()

	// Victim (SSH verified) sees full shared details.
	vStatus, err := mgr.Status(ctx, victim.ID, hVictim.ID)
	if err != nil {
		t.Fatal(err)
	}
	if vStatus.Running == nil || vStatus.Running.PID != 4242 || vStatus.Users == nil {
		t.Fatalf("verified victim should see shared metrics, got %+v", vStatus)
	}

	// Attacker (NOT SSH verified, even with consent_by == attacker.ID) MUST NOT see shared metrics/users/files.
	aStatus, err := mgr.Status(ctx, attacker.ID, hAttacker.ID)
	if err != nil {
		t.Fatal(err)
	}
	if aStatus.Running != nil || aStatus.Users != nil || aStatus.Consent != nil || len(aStatus.Files) != 0 {
		t.Fatalf("unverified user leaked shared metadata during install: running=%+v users=%+v consent=%+v files=%v",
			aStatus.Running, aStatus.Users, aStatus.Consent, aStatus.Files)
	}
}

// C-2: SelectStartBinary skips failed releases without modifying the shared symlink.
func TestSelectStartBinarySkipsFailedRelease(t *testing.T) {
	probe := &HostProbe{
		LibDir:        "/home/alice/.local/lib/corral",
		CurrentBin:    "/home/alice/.local/lib/corral/corrald",
		HasSymlink:    true,
		CurrentTarget: "corrald-0.3.2-2000/corrald",
		Versions:      []string{"corrald-0.3.1-1000", "corrald-0.3.2-2000"},
	}
	if got, ok := SelectStartBinary(probe, nil, StartSelectOptions{}); !ok || got.Path != "/home/alice/.local/lib/corral/corrald-0.3.2-2000/corrald" || got.Release != 2000 {
		t.Fatalf("SelectStartBinary(no failed) = (%+v, %v), want concrete versioned path for rel=2000 (never shared symlink)", got, ok)
	}
	if got, ok := SelectStartBinary(probe, []int64{2000}, StartSelectOptions{}); !ok || got.Path != "/home/alice/.local/lib/corral/corrald-0.3.1-1000/corrald" || got.Release != 1000 {
		t.Fatalf("SelectStartBinary(2000 failed) = (%+v, %v), want 1000 binary", got, ok)
	}
	// When all installed releases are blacklisted, SelectStartBinary must refuse (ok=false).
	if got, ok := SelectStartBinary(probe, []int64{1000, 2000}, StartSelectOptions{}); ok {
		t.Fatalf("SelectStartBinary(all failed) = (%+v, true), want ok=false", got)
	}
}

// C-6 & C-7: Verify DefaultHandoffTimeout > 60s, reconnect-before-retry when
// receiving handoff_in_progress (accepting already-upgraded target without
// blacklisting), and never blacklisting on timeout while blacklisting on
// explicit handoff_failed.
func TestHandoffBusyReconnectAndFailureBlacklistPolicy(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("linux test")
	}
	if DefaultHandoffTimeout <= 60*time.Second {
		t.Fatalf("DefaultHandoffTimeout = %v, must exceed daemon's 60s lock wait", DefaultHandoffTimeout)
	}

	runCase := func(t *testing.T, mode string) (*store.CorraldHost, *store.CorraldOp) {
		t.Helper()
		home := t.TempDir()
		sockDir, err := os.MkdirTemp("/tmp", "gw-ho-*")
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
							rel := int64(1000)
							ver := "0.3.1"
							if mode == "in_progress_then_v2" && gen >= 2 {
								rel = 2000
								ver = "0.3.2"
							}
							res, _ := json.Marshal(proto.HelloResult{
								Version: ver, Release: rel, Protocol: proto.Protocol,
								Epoch: "ep-" + string(rune('0'+gen)), PID: int(100 + gen),
								Manifest: proto.DefaultManifest(), State: proto.StateRunning,
							})
							_ = proto.WriteJSON(conn, proto.Response{ID: req.ID, Result: res})
						case proto.MethodDaemonResume:
							res, _ := json.Marshal(proto.ResumeResult{Epoch: "ep-" + string(rune('0'+gen))})
							_ = proto.WriteJSON(conn, proto.Response{ID: req.ID, Result: res})
						case proto.MethodDaemonHealth:
							res, _ := json.Marshal(proto.HealthResult{Caps: map[string]string{"locate": "ok"}, PID: int(100 + gen)})
							_ = proto.WriteJSON(conn, proto.Response{ID: req.ID, Result: res})
						case proto.MethodDaemonHandoff:
							switch mode {
							case "in_progress_then_v2":
								_ = proto.WriteJSON(conn, proto.Response{
									ID:    req.ID,
									Error: &proto.RPCError{Code: proto.ErrHandoffInProgress, Message: "another gateway handing off", Retryable: true},
								})
								// Old daemon exits shortly after returning handoff_in_progress!
								return
							case "timeout":
								// Hang past HandoffTimeout without replying.
								time.Sleep(500 * time.Millisecond)
								return
							case "explicit_fail":
								_ = proto.WriteJSON(conn, proto.Response{
									ID:    req.ID,
									Error: &proto.RPCError{Code: proto.ErrHandoffFailed, Message: "v2 crashed before READY"},
								})
							}
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
		h, _ := st.CreateHost(ctx, &store.Host{OwnerID: u.ID, Name: "b", Address: "127.0.0.1", Port: srv.Addr.Port, Username: "alice", Auth: "password"})

		// Create a dummy binary for Dist.
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
			OS: "Linux", Machine: "x86_64", Arch: "amd64", UID: "1000",
			Home: home, HomeWritable: true, FreeKB: 100000, MachineID: "0123456789ab",
			LibDir:     filepath.Join(home, ".local", "lib", "corral"),
			CurrentBin: filepath.Join(home, ".local", "lib", "corral", "corrald"),
			StateDir:   stateDir,
			FailedJSON: filepath.Join(stateDir, "failed.json"),
			SocketPath: sockPath,
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
			HandoffTimeout:    120 * time.Millisecond,
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

		// Attach live connection (gen 1, Release 1000).
		ch, _ := st.CorraldHostByID(ctx, h.ID)
		if err := mgr.attachAndHello(ctx, u.ID, h.ID, "alice", "fp-1", "alice", sshClient, nil, probe, sockPath, ch, ""); err != nil {
			t.Fatalf("attachAndHello: %v", err)
		}

		// Launch update op.
		if _, err := mgr.StartUpdate(ctx, u.ID, h.ID, "alice", mHash, false); err != nil {
			t.Fatalf("StartUpdate: %v", err)
		}
		if err := mgr.WaitOpsIdle(ctx); err != nil {
			t.Fatal(err)
		}
		afterHost, _ := st.CorraldHostByID(ctx, h.ID)
		afterOp, _ := st.LatestCorraldOp(ctx, h.ID)
		return afterHost, afterOp
	}

	t.Run("C6_handoff_in_progress_reconnects_to_new_daemon", func(t *testing.T) {
		h, op := runCase(t, "in_progress_then_v2")
		if op.State != OpDone || h.Release != 2000 {
			t.Fatalf("expected OpDone at release 2000 after reconnect, got op=%s release=%d", op.State, h.Release)
		}
		if len(h.FailedReleases) != 0 {
			t.Fatalf("handoff_in_progress must not blacklist release, got failed=%v", h.FailedReleases)
		}
	})

	t.Run("C7_timeout_does_not_blacklist_release", func(t *testing.T) {
		h, op := runCase(t, "timeout")
		if op.State != OpFailed {
			t.Fatalf("expected OpFailed on timeout, got %s", op.State)
		}
		if len(h.FailedReleases) != 0 {
			t.Fatalf("timeout must NEVER blacklist release, got failed=%v", h.FailedReleases)
		}
	})

	t.Run("C7_explicit_handoff_failed_blacklists_release", func(t *testing.T) {
		h, op := runCase(t, "explicit_fail")
		if op.State != OpFailed {
			t.Fatalf("expected OpFailed on handoff_failed, got %s", op.State)
		}
		if len(h.FailedReleases) != 1 || h.FailedReleases[0] != 2000 {
			t.Fatalf("explicit handoff_failed must blacklist release 2000, got failed=%v", h.FailedReleases)
		}
	})
}

var _ = errors.Is
