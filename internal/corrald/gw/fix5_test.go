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

// Fix5 #1: MarkCorraldRebindIfMismatch MUST verify inside the same UPDATE SQL
// that the observed (fingerprint, username) still equals the host's current
// pinned identity in hosts + host_keys. Even if a stale callback carrying
// identity A reads corrald_hosts AFTER new binding B was written (so
// expected=B, observed=A), SQL rejects the stale callback and leaves B intact.
func TestRebindSQLRejectsStaleObservedIdentityEvenWhenReadAfterNewBinding(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()

	const sshUser = "test-ssh-u1"
	const gwID = "gw-fix5-rebind"
	u, err := st.CreateUser(ctx, "user-f5-1", "User One", "hash", "admin")
	if err != nil {
		t.Fatal(err)
	}
	h, err := st.CreateHost(ctx, &store.Host{
		OwnerID: u.ID, Name: "box-f5-1", Address: "127.0.0.1", Port: 22, Username: sshUser, Auth: "password",
	})
	if err != nil {
		t.Fatal(err)
	}

	// Host is currently pinned to fp-B in host_keys and bound to fp-B in corrald_hosts.
	if err := st.PinHostKey(ctx, u.ID, h.ID, &store.HostKey{
		KeyType: "ssh-ed25519", KeyData: []byte("k-B"), Fingerprint: "fp-B",
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertCorraldHost(ctx, &store.CorraldHost{
		HostID:           h.ID,
		State:            StateRunning,
		ConsentBy:        &u.ID,
		ConsentAt:        2000,
		BoundFingerprint: "fp-B",
		BoundUsername:    sshUser,
		BoundGateway:     gwID,
		Version:          "0.3.1",
		Release:          1000,
	}); err != nil {
		t.Fatal(err)
	}

	// 1. Stale callback read snapshot AFTER B was written (expected=fp-B, observed=fp-A).
	//    In fix4, CAS alone (`bound == expected && bound != observed`) erroneously matched!
	//    Now SQL verifies observed (fp-A, sshUser) == current (host_keys.fingerprint, hosts.username),
	//    which is FALSE (fp-B != fp-A), so 0 rows are updated.
	changed, err := st.MarkCorraldRebindIfMismatch(ctx, h.ID, "fp-B", sshUser, gwID, "fp-A", sshUser, gwID, false)
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Fatal("stale observed fp-A must NOT mark fp-B host as rebind even when expected==fp-B")
	}
	got, _ := st.CorraldHostByID(ctx, h.ID)
	if got.State != StateRunning || got.BoundFingerprint != "fp-B" {
		t.Fatalf("host state clobbered by stale callback: state=%q fp=%q", got.State, got.BoundFingerprint)
	}

	// 2. Stale callback with outdated username (hosts.username is still sshUser, observed="old-user").
	changed, err = st.MarkCorraldRebindIfMismatch(ctx, h.ID, "fp-B", sshUser, gwID, "fp-B", "old-user", gwID, true)
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Fatal("stale observed username must NOT mark host as rebind")
	}

	// 3. Genuine key rotation in host_keys to fp-C: now observed=fp-C matches host_keys,
	//    expected=fp-B matches bound_fingerprint, and fp-B != fp-C -> MUST mark rebind!
	if err := st.ReplaceHostKey(ctx, u.ID, h.ID, "fp-B", &store.HostKey{
		KeyType: "ssh-ed25519", KeyData: []byte("k-C"), Fingerprint: "fp-C",
	}); err != nil {
		t.Fatal(err)
	}
	changed, err = st.MarkCorraldRebindIfMismatch(ctx, h.ID, "fp-B", sshUser, gwID, "fp-C", sshUser, gwID, false)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("expected genuine host key rotation fp-B -> fp-C to mark rebind")
	}
	got, _ = st.CorraldHostByID(ctx, h.ID)
	if got.State != StateRebind {
		t.Fatalf("expected StateRebind after genuine rotation, got %q", got.State)
	}
}

// Fix5 #2: SelectStartBinary MUST ignore any symlink target pointing outside
// LibDir or referencing a version directory absent from probe.Versions, fall
// back to remaining healthy installed versions (or return ok=false if none),
// and NEVER blacklist any release because of an invalid/missing symlink target.
func TestSelectStartBinaryIgnoresOutsideOrMissingSymlinkTargetWithoutBlacklisting(t *testing.T) {
	libDir := filepath.Join(t.TempDir(), ".local", "lib", "corral")
	curBin := filepath.Join(libDir, "corrald")

	t.Run("relative_traversal_outside_libdir_ignored", func(t *testing.T) {
		p := &HostProbe{
			LibDir:        libDir,
			CurrentBin:    curBin,
			HasSymlink:    true,
			CurrentTarget: "../../outside/corrald-0.9.9-9000/corrald",
			Versions:      []string{"corrald-0.3.1-1000"},
		}
		cand, ok := SelectStartBinary(p, nil, StartSelectOptions{})
		if !ok || cand.Release != 1000 || cand.Path != filepath.Join(libDir, "corrald-0.3.1-1000", "corrald") {
			t.Fatalf("expected fallback to healthy 1000 in LibDir, got (%+v, %v)", cand, ok)
		}
	})

	t.Run("absolute_path_outside_libdir_ignored", func(t *testing.T) {
		p := &HostProbe{
			LibDir:        libDir,
			CurrentBin:    curBin,
			HasSymlink:    true,
			CurrentTarget: "/evil/corrald-0.9.9-9000/corrald",
			Versions:      []string{"corrald-0.3.1-1000"},
		}
		cand, ok := SelectStartBinary(p, nil, StartSelectOptions{})
		if !ok || cand.Release != 1000 || cand.Path != filepath.Join(libDir, "corrald-0.3.1-1000", "corrald") {
			t.Fatalf("expected fallback to healthy 1000 in LibDir, got (%+v, %v)", cand, ok)
		}
	})

	t.Run("missing_target_dir_ignored_falls_back_to_healthy", func(t *testing.T) {
		p := &HostProbe{
			LibDir:        libDir,
			CurrentBin:    curBin,
			HasSymlink:    true,
			CurrentTarget: "corrald-0.3.2-2000/corrald", // NOT present in Versions!
			Versions:      []string{"corrald-0.3.1-1000"},
		}
		cand, ok := SelectStartBinary(p, nil, StartSelectOptions{})
		if !ok || cand.Release != 1000 || cand.Path != filepath.Join(libDir, "corrald-0.3.1-1000", "corrald") {
			t.Fatalf("expected missing symlink target 2000 to be ignored and pick 1000, got (%+v, %v)", cand, ok)
		}
	})

	t.Run("missing_target_and_no_healthy_versions_returns_false", func(t *testing.T) {
		p := &HostProbe{
			LibDir:        libDir,
			CurrentBin:    curBin,
			HasSymlink:    true,
			CurrentTarget: "corrald-0.3.2-2000/corrald",
			Versions:      nil,
		}
		if cand, ok := SelectStartBinary(p, nil, StartSelectOptions{}); ok {
			t.Fatalf("expected ok=false when target dir missing and no healthy versions exist, got %+v", cand)
		}
	})

	t.Run("invalid_symlink_does_not_blacklist_any_release", func(t *testing.T) {
		st, err := store.Open(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		defer st.Close()
		ctx := context.Background()
		u, _ := st.CreateUser(ctx, "user-f5-2", "User Two", "hash", "admin")
		h, _ := st.CreateHost(ctx, &store.Host{OwnerID: u.ID, Name: "box-f5-2", Address: "127.0.0.1", Port: 22, Username: "ssh-u2", Auth: "password"})
		_ = st.UpsertCorraldHost(ctx, &store.CorraldHost{
			HostID: h.ID, State: StateStopped, BoundFingerprint: "fp-1", BoundUsername: "ssh-u2", BoundGateway: "gw-1",
		})
		mgr := NewManager(ManagerConfig{Store: st, GatewayID: "gw-1", SSH: &stubSSHProvider{}})
		defer mgr.Close()

		badProbe := &HostProbe{
			LibDir:        libDir,
			CurrentBin:    curBin,
			HasSymlink:    true,
			CurrentTarget: "/outside/corrald-0.9.9-9000/corrald",
			Versions:      nil,
		}
		ch, _ := st.CorraldHostByID(ctx, h.ID)
		_, _, _, selErr := mgr.resolveStartBinary(ctx, nil, badProbe, h.ID, "fp-1", "ssh-u2", ch, nil, StartSelectOptions{})
		if selErr == nil {
			t.Fatal("expected error when no healthy binary exists")
		}
		after, _ := st.CorraldHostByID(ctx, h.ID)
		if len(after.FailedReleases) != 0 {
			t.Fatalf("invalid/missing symlink target must NEVER blacklist any release, got %v", after.FailedReleases)
		}
	})
}

// Fix5 #3: When cold-start upgrade launches a daemon whose hello returns
// proto_mismatch:
//   - Subtest A: rollback MUST stop the running incompatible daemon first,
//     then start the compatible rollback binary and reconnect -> StateRunning.
//   - Subtest B: when rollback also fails after attachAndHello already cleared
//     active_op_id on proto_mismatch, finishUpdateFailError (active_op_id IN
//     (”, op.ID)) MUST still write StateError; and EvRetry accepts StateIncompatible.
func TestColdStartRollbackStopsIncompatibleDaemonAndHandlesClearedActiveOpID(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("linux test")
	}

	// Verify state machine directly accepts EvRetry from StateIncompatible.
	if tr := Transition(StateSnapshot{State: StateIncompatible}, Event{Kind: EvRetry}); !tr.Allowed {
		t.Fatalf("expected EvRetry to be allowed from StateIncompatible, got %+v", tr)
	}

	runCase := func(t *testing.T, rollbackServeSucceeds bool) {
		t.Helper()
		home := t.TempDir()
		uidStr := strconv.Itoa(os.Getuid())
		mid := "f5mid3a1b2c3"
		libDir := filepath.Join(home, ".local", "lib", "corral")
		stateDir := filepath.Join(home, ".local", "state", "corral", "corrald", mid)
		_ = os.MkdirAll(stateDir, 0o700)
		_ = os.MkdirAll(filepath.Join(libDir, "corrald-0.3.1-1000"), 0o700)
		_ = os.MkdirAll(filepath.Join(libDir, "corrald-0.3.2-2000"), 0o700)

		sockDir, err := os.MkdirTemp("", "gw-f5-3-*")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.RemoveAll(sockDir) })
		// If sockDir is long, fall back to short /tmp dir just for the Unix socket.
		sockPath := filepath.Join(sockDir, "c.sock")
		if len(sockPath) > MaxSocketPath {
			shortDir, serr := os.MkdirTemp("/tmp", "gw-f5-3-*")
			if serr != nil {
				t.Fatal(serr)
			}
			t.Cleanup(func() { _ = os.RemoveAll(shortDir) })
			sockPath = filepath.Join(shortDir, "c.sock")
		}

		ln, err := net.Listen("unix", sockPath)
		if err != nil {
			t.Fatal(err)
		}
		defer ln.Close()

		// Mode switches from incompatible R2 (proto 99) -> stopped -> compatible R1
		// ONLY after StopCommand is executed!
		var daemonMode atomic.Int32 // 0 = incompatible R2 running, 1 = stopped, 2 = compatible R1 running
		go func() {
			for {
				c, err := ln.Accept()
				if err != nil {
					return
				}
				go func(conn net.Conn) {
					defer conn.Close()
					for {
						var req proto.Request
						if err := proto.ReadJSON(conn, &req); err != nil {
							return
						}
						mode := daemonMode.Load()
						switch req.Method {
						case proto.MethodDaemonHello:
							if mode == 0 {
								_ = proto.WriteJSON(conn, proto.Response{
									ID:    req.ID,
									Error: &proto.RPCError{Code: proto.ErrProtoMismatch, Message: "incompatible protocol 99"},
								})
								return
							}
							if mode == 2 {
								res, _ := json.Marshal(proto.HelloResult{
									Version: "0.3.1", Release: 1000, Protocol: proto.Protocol,
									Epoch: "ep-r1", PID: 101, Manifest: proto.DefaultManifest(), State: proto.StateRunning,
								})
								_ = proto.WriteJSON(conn, proto.Response{ID: req.ID, Result: res})
							}
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

		var mu sync.Mutex
		var events []string
		sshUser := "ssh-f5-3"
		srv := testutil.StartSSHServer(t, sshUser, "pass", home)
		realExec := testutil.ShellExec(home, func(string) bool { return true })
		srv.HandleExec(func(cmd string) func(ch ssh.Channel) {
			if strings.Contains(cmd, " selftest") || strings.Contains(cmd, " install ") {
				return func(ch ssh.Channel) {
					defer ch.Close()
					_, _ = ch.Write([]byte("result=ok\nbinary=" + filepath.Join(libDir, "corrald-0.3.2-2000", "corrald") + "\n"))
					_, _ = ch.SendRequest("exit-status", false, ssh.Marshal(&struct{ Status uint32 }{0}))
				}
			}
			if strings.Contains(cmd, " stop") {
				mu.Lock()
				events = append(events, "stop")
				mu.Unlock()
				daemonMode.Store(1)
				return func(ch ssh.Channel) {
					defer ch.Close()
					_, _ = ch.Write([]byte("result=ok\nstopped=true\n"))
					_, _ = ch.SendRequest("exit-status", false, ssh.Marshal(&struct{ Status uint32 }{0}))
				}
			}
			if strings.Contains(cmd, " serve ") {
				if strings.Contains(cmd, "corrald-0.3.2-2000") {
					mu.Lock()
					events = append(events, "serve-r2")
					mu.Unlock()
					daemonMode.Store(0)
					return func(ch ssh.Channel) {
						defer ch.Close()
						_, _ = ch.Write([]byte("result=ok\npid=202\nsocket=" + sockPath + "\n"))
						_, _ = ch.SendRequest("exit-status", false, ssh.Marshal(&struct{ Status uint32 }{0}))
					}
				}
				if strings.Contains(cmd, "corrald-0.3.1-1000") {
					mu.Lock()
					events = append(events, "serve-r1")
					mu.Unlock()
					if !rollbackServeSucceeds {
						return func(ch ssh.Channel) {
							defer ch.Close()
							_, _ = ch.Write([]byte("error=start_failed\ndetail=r1 rollback failed\n"))
							_, _ = ch.SendRequest("exit-status", false, ssh.Marshal(&struct{ Status uint32 }{1}))
						}
					}
					// Only allow R1 to take over socket if incompatible R2 was stopped first!
					if daemonMode.Load() == 1 {
						daemonMode.Store(2)
					}
					return func(ch ssh.Channel) {
						defer ch.Close()
						_, _ = ch.Write([]byte("result=ok\npid=101\nsocket=" + sockPath + "\n"))
						_, _ = ch.SendRequest("exit-status", false, ssh.Marshal(&struct{ Status uint32 }{0}))
					}
				}
			}
			return realExec(cmd)
		})

		cc := &ssh.ClientConfig{
			User:            sshUser,
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
		u, _ := st.CreateUser(ctx, "user-f5-3", "User Three", "hash", "admin")
		h, _ := st.CreateHost(ctx, &store.Host{OwnerID: u.ID, Name: "box-f5-3", Address: "127.0.0.1", Port: srv.Addr.Port, Username: sshUser, Auth: "password"})
		_ = st.PinHostKey(ctx, u.ID, h.ID, &store.HostKey{KeyType: "ssh-ed25519", KeyData: []byte("k1"), Fingerprint: "fp-1"})

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

		_ = st.UpsertCorraldHost(ctx, &store.CorraldHost{
			HostID: h.ID, State: StateStopped, BoundFingerprint: "fp-1", BoundUsername: sshUser,
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
					OS: "Linux", Machine: "x86_64", Arch: "amd64", UID: uidStr,
					Home: home, HomeWritable: true, FreeKB: 100000, MachineID: mid,
					HasSymlink:    true,
					CurrentTarget: "corrald-0.3.2-2000/corrald",
					Versions:      []string{"corrald-0.3.1-1000", "corrald-0.3.2-2000"},
					LibDir:        libDir,
					CurrentBin:    filepath.Join(libDir, "corrald"),
					StateDir:      stateDir,
					FailedJSON:    filepath.Join(stateDir, "failed.json"),
					SocketPath:    sockPath,
				}, nil
			},
			SSH: &stubSSHProvider{
				bindings: map[int64][2]string{h.ID: {sshUser, "fp-1"}},
				acquire: func(context.Context, int64, int64) (*ssh.Client, func(), string, string, error) {
					return sshClient, func() {}, "fp-1", sshUser, nil
				},
			},
		})
		defer mgr.Close()

		if _, err := mgr.StartUpdate(ctx, u.ID, h.ID, "user-f5-3", mHash, false); err != nil {
			t.Fatalf("StartUpdate: %v", err)
		}
		if err := mgr.WaitOpsIdle(ctx); err != nil {
			t.Fatal(err)
		}

		mu.Lock()
		gotEvents := append([]string(nil), events...)
		mu.Unlock()
		if len(gotEvents) < 3 || gotEvents[0] != "serve-r2" || gotEvents[1] != "stop" || gotEvents[2] != "serve-r1" {
			t.Fatalf("expected sequence [serve-r2 stop serve-r1], got %v", gotEvents)
		}

		after, err := st.CorraldHostByID(ctx, h.ID)
		if err != nil {
			t.Fatal(err)
		}
		if rollbackServeSucceeds {
			if after.State != StateRunning || after.Release != 1000 {
				t.Fatalf("expected rollback to StateRunning@1000 after stopping incompatible R2, got state=%q rel=%d", after.State, after.Release)
			}
		} else {
			if after.State != StateError {
				t.Fatalf("expected StateError after rollback failed following proto_mismatch (even with active_op_id cleared), got %q", after.State)
			}
			if after.ActiveOpID != "" {
				t.Fatalf("expected empty active_op_id, got %q", after.ActiveOpID)
			}
		}
	}

	t.Run("stops_incompatible_daemon_and_rolls_back_to_v1", func(t *testing.T) { runCase(t, true) })
	t.Run("rollback_fail_after_proto_mismatch_writes_state_error", func(t *testing.T) { runCase(t, false) })
}

// Fix5 #4: Rollback target is NOT pre-stored at update completion; instead,
// SelectStartBinary(Rollback: true, BelowRelease: actualRel) dynamically selects
// the highest installed, non-failed release strictly below the actual crashed
// release on the host at rollback time.
func TestDynamicCrashRollbackSelectsHighestNonFailedBelowActualRelease(t *testing.T) {
	libDir := filepath.Join(t.TempDir(), ".local", "lib", "corral")
	curBin := filepath.Join(libDir, "corrald")

	t.Run("selector_below_release_skips_higher_and_failed", func(t *testing.T) {
		p := &HostProbe{
			LibDir:        libDir,
			CurrentBin:    curBin,
			HasSymlink:    true,
			CurrentTarget: "corrald-0.3.4-4000/corrald",
			Versions: []string{
				"corrald-0.3.1-1000",
				"corrald-0.3.2-2000",
				"corrald-0.3.3-3000",
				"corrald-0.3.4-4000",
			},
		}
		// Rolling back from 3000 MUST pick 2000 (strictly < 3000, NOT 4000!).
		cand, ok := SelectStartBinary(p, []int64{3000}, StartSelectOptions{Rollback: true, BelowRelease: 3000})
		if !ok || cand.Release != 2000 {
			t.Fatalf("expected 2000 when rolling back below 3000, got (%+v, %v)", cand, ok)
		}
		// If 2000 is also failed, MUST pick 1000!
		cand, ok = SelectStartBinary(p, []int64{2000, 3000}, StartSelectOptions{Rollback: true, BelowRelease: 3000})
		if !ok || cand.Release != 1000 {
			t.Fatalf("expected 1000 when 2000 & 3000 failed below 3000, got (%+v, %v)", cand, ok)
		}
		// No version below 1000 -> ok=false.
		if cand, ok = SelectStartBinary(p, []int64{1000}, StartSelectOptions{Rollback: true, BelowRelease: 1000}); ok {
			t.Fatalf("expected ok=false when no version < 1000 exists, got %+v", cand)
		}
	})

	t.Run("attemptPostUpdateRollback_computes_target_dynamically_with_empty_prev", func(t *testing.T) {
		if runtime.GOOS != "linux" {
			t.Skip("linux test")
		}
		home := t.TempDir()
		uidStr := strconv.Itoa(os.Getuid())
		mid := "f5mid4d5e6f7"
		hLibDir := filepath.Join(home, ".local", "lib", "corral")
		stateDir := filepath.Join(home, ".local", "state", "corral", "corrald", mid)
		_ = os.MkdirAll(stateDir, 0o700)
		for _, v := range []string{"corrald-0.3.1-1000", "corrald-0.3.2-2000", "corrald-0.3.3-3000", "corrald-0.3.4-4000"} {
			_ = os.MkdirAll(filepath.Join(hLibDir, v), 0o700)
		}
		// R3 (3000) is already in failed.json on host!
		failedBytes, _ := json.Marshal(HostFailedRecord{
			Releases: []HostFailedEntry{{Release: 3000, Reason: "earlier_failure"}},
		})
		_ = os.WriteFile(filepath.Join(stateDir, "failed.json"), failedBytes, 0o600)

		shortDir, err := os.MkdirTemp("/tmp", "gw-f5-4-*")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.RemoveAll(shortDir) })
		sockPath := filepath.Join(shortDir, "c.sock")

		ln, err := net.Listen("unix", sockPath)
		if err != nil {
			t.Fatal(err)
		}
		defer ln.Close()

		go func() {
			for {
				c, err := ln.Accept()
				if err != nil {
					return
				}
				go func(conn net.Conn) {
					defer conn.Close()
					for {
						var req proto.Request
						if err := proto.ReadJSON(conn, &req); err != nil {
							return
						}
						switch req.Method {
						case proto.MethodDaemonHello:
							res, _ := json.Marshal(proto.HelloResult{
								Version: "0.3.2", Release: 2000, Protocol: proto.Protocol,
								Epoch: "ep-r2", PID: 202, Manifest: proto.DefaultManifest(), State: proto.StateRunning,
							})
							_ = proto.WriteJSON(conn, proto.Response{ID: req.ID, Result: res})
						case proto.MethodDaemonResume:
							res, _ := json.Marshal(proto.ResumeResult{Epoch: "ep-r2"})
							_ = proto.WriteJSON(conn, proto.Response{ID: req.ID, Result: res})
						case proto.MethodDaemonHealth:
							res, _ := json.Marshal(proto.HealthResult{PID: 202, Caps: map[string]string{"locate": "ok"}})
							_ = proto.WriteJSON(conn, proto.Response{ID: req.ID, Result: res})
						}
					}
				}(c)
			}
		}()

		var mu sync.Mutex
		var servedCmds []string
		sshUser := "ssh-f5-4"
		srv := testutil.StartSSHServer(t, sshUser, "pass", home)
		srv.HandleExec(func(cmd string) func(ch ssh.Channel) {
			if strings.Contains(cmd, " stop") {
				return func(ch ssh.Channel) {
					defer ch.Close()
					_, _ = ch.Write([]byte("result=ok\nstopped=true\n"))
					_, _ = ch.SendRequest("exit-status", false, ssh.Marshal(&struct{ Status uint32 }{0}))
				}
			}
			if strings.Contains(cmd, " serve ") {
				mu.Lock()
				servedCmds = append(servedCmds, cmd)
				mu.Unlock()
				return func(ch ssh.Channel) {
					defer ch.Close()
					_, _ = ch.Write([]byte("result=ok\npid=202\nsocket=" + sockPath + "\n"))
					_, _ = ch.SendRequest("exit-status", false, ssh.Marshal(&struct{ Status uint32 }{0}))
				}
			}
			return testutil.ShellExec(home, func(string) bool { return true })(cmd)
		})

		cc := &ssh.ClientConfig{
			User:            sshUser,
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
		u, _ := st.CreateUser(ctx, "user-f5-4", "User Four", "hash", "admin")
		h, _ := st.CreateHost(ctx, &store.Host{OwnerID: u.ID, Name: "box-f5-4", Address: "127.0.0.1", Port: srv.Addr.Port, Username: sshUser, Auth: "password"})
		_ = st.PinHostKey(ctx, u.ID, h.ID, &store.HostKey{KeyType: "ssh-ed25519", KeyData: []byte("k4"), Fingerprint: "fp-4"})
		_ = st.UpsertCorraldHost(ctx, &store.CorraldHost{
			HostID: h.ID, State: StateLost, BoundFingerprint: "fp-4", BoundUsername: sshUser,
			BoundGateway: "gw-1", Version: "0.3.4", Release: 4000,
			UpdateTargetVer: "0.3.4", UpdateTargetRel: 4000, UpdateReadyAt: time.Now().Unix(),
			// No pre-stored UpdatedFrom* fields!
		})

		m := proto.DefaultManifest()
		mHash, _ := proto.ManifestSHA256(m)
		mgr := NewManager(ManagerConfig{
			Store:     st,
			Dist:      &Dist{Version: "0.3.1", Release: 1000, Manifest: m, ManifestHash: mHash}, // Gateway carries R1, NOT R2!
			GatewayID: "gw-1",
			ProbeOverride: func(context.Context, *ssh.Client) (*HostProbe, error) {
				return &HostProbe{
					OS: "Linux", Machine: "x86_64", Arch: "amd64", UID: uidStr,
					Home: home, HomeWritable: true, FreeKB: 100000, MachineID: mid,
					HasSymlink:    true,
					CurrentTarget: "corrald-0.3.4-4000/corrald",
					Versions:      []string{"corrald-0.3.1-1000", "corrald-0.3.2-2000", "corrald-0.3.3-3000", "corrald-0.3.4-4000"},
					LibDir:        hLibDir,
					CurrentBin:    filepath.Join(hLibDir, "corrald"),
					StateDir:      stateDir,
					FailedJSON:    filepath.Join(stateDir, "failed.json"),
					SocketPath:    sockPath,
				}, nil
			},
			SSH: &stubSSHProvider{
				bindings: map[int64][2]string{h.ID: {sshUser, "fp-4"}},
				acquire: func(context.Context, int64, int64) (*ssh.Client, func(), string, string, error) {
					return sshClient, func() {}, "fp-4", sshUser, nil
				},
			},
		})
		defer mgr.Close()

		key := accountKey{Fingerprint: "fp-4", Username: sshUser}
		sc := newSharedConn(key)
		sc.libDir = hLibDir
		sc.stateDir = stateDir
		sc.homeDir = home
		sc.uid = int64(os.Getuid())
		sc.machineID = mid
		sc.socketPath = sockPath
		refs := []*hostRef{{HostID: h.ID, OwnerID: u.ID, CorralUser: "user-f5-4"}}
		sc.hostRefs[h.ID] = refs[0]
		mgr.mu.Lock()
		mgr.conns[key] = sc
		mgr.hostConn[h.ID] = key
		mgr.mu.Unlock()

		// Trigger rollback for actual crashed release 4000 with empty prevVer/prevRel/prevRelTarget.
		if ok := mgr.attemptPostUpdateRollback(sc, refs, "0.3.4", 4000, "", 0, "", home, int64(os.Getuid()), hLibDir, stateDir, sockPath, mid); !ok {
			t.Fatal("attemptPostUpdateRollback failed")
		}

		mu.Lock()
		cmds := append([]string(nil), servedCmds...)
		mu.Unlock()
		if len(cmds) != 1 || !strings.Contains(cmds[0], filepath.Join(hLibDir, "corrald-0.3.2-2000", "corrald")) {
			t.Fatalf("expected rollback to dynamically select R2 (highest non-failed < 4000, skipping failed 3000), got serve cmds: %v", cmds)
		}
		after, _ := st.CorraldHostByID(ctx, h.ID)
		if after.State != StateRunning || after.Release != 2000 || after.Version != "0.3.2" {
			t.Fatalf("expected host running at rolled-back 0.3.2/2000, got state=%q %s/%d", after.State, after.Version, after.Release)
		}
	})
}
