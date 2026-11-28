//go:build linux

package daemon

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ihainan/corral/internal/corrald/proto"
)

func randomHexMid(t *testing.T) string {
	t.Helper()
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatalf("rand.Read: %v", err)
	}
	return hex.EncodeToString(b[:])
}

func startTestServerForSoleGW(t *testing.T, home, mid string) (*Server, *Layout, context.CancelFunc, <-chan error) {
	t.Helper()
	uid := uint32(os.Getuid())
	layout, err := ResolveLayout(PathOptions{Home: home, UID: &uid, MachineID: mid})
	if err != nil {
		t.Fatalf("ResolveLayout: %v", err)
	}
	if err := layout.EnsureStateDir(); err != nil {
		t.Fatalf("EnsureStateDir: %v", err)
	}
	if layout.SocketUsesTmpDir {
		t.Cleanup(func() { _ = os.RemoveAll(layout.SocketDir) })
	}
	srv, err := NewServer(ServerConfig{
		Layout:              layout,
		Build:               proto.BuildInfo{Version: "0.3.2", Release: 2000, Commit: "test"},
		IdleTimeout:         30 * time.Second,
		SocketWatchInterval: 30 * time.Second,
		OldVersionGCDelay:   30 * time.Second,
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	doneCh := make(chan error, 1)
	go func() {
		doneCh <- srv.Serve(ctx)
	}()

	deadline := time.Now().Add(5 * time.Second)
	for {
		var h proto.HealthResult
		if err := callLocalRPC(layout.SocketPath, proto.MethodDaemonHealth, nil, &h, 300*time.Millisecond); err == nil {
			break
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatalf("daemon server failed to become ready at %s", layout.SocketPath)
		}
		time.Sleep(15 * time.Millisecond)
	}
	t.Cleanup(func() {
		cancel()
		select {
		case <-doneCh:
		case <-time.After(3 * time.Second):
		}
	})
	return srv, layout, cancel, doneCh
}

func dialUnixRPC(t *testing.T, sockPath string) *net.UnixConn {
	t.Helper()
	c, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: sockPath, Net: "unix"})
	if err != nil {
		t.Fatalf("dial %s: %v", sockPath, err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func rpcCallConn(c net.Conn, id uint64, method string, params any, dst any) (*proto.RPCError, error) {
	_ = c.SetDeadline(time.Now().Add(3 * time.Second))
	var raw json.RawMessage
	if params != nil {
		b, err := json.Marshal(params)
		if err != nil {
			return nil, err
		}
		raw = b
	}
	if err := proto.WriteJSON(c, proto.Request{ID: id, Method: method, Params: raw}); err != nil {
		return nil, err
	}
	for {
		var env proto.Envelope
		if err := proto.ReadJSON(c, &env); err != nil {
			return nil, err
		}
		if env.IsEvent() || env.ID != id {
			continue
		}
		if env.Error != nil {
			return env.Error, nil
		}
		if dst != nil && len(env.Result) > 0 {
			if err := json.Unmarshal(env.Result, dst); err != nil {
				return nil, err
			}
		}
		return nil, nil
	}
}

// Regression test for Fix Round 4 Item #1 [P1] (daemon RPC side):
//   - HelloResult advertises proto.FeatureConditionalShutdown.
//   - Conditional shutdown succeeds ONLY when no other gateway is connected or
//     registered in clients.json, and atomically enters shuttingDown so any
//     subsequent Hello is rejected with proto.ErrShuttingDown.
//   - Concurrent Hello (gw-b) vs ShutdownConditional (gw-a) is strictly
//     mutually exclusive under -race.
func TestDaemonConditionalShutdownSoleGateway(t *testing.T) {
	t.Run("sole_gateway_succeeds_and_rejects_new_hello", func(t *testing.T) {
		_, layout, _, doneCh := startTestServerForSoleGW(t, t.TempDir(), randomHexMid(t))

		cA := dialUnixRPC(t, layout.SocketPath)
		var hA proto.HelloResult
		rpcErr, err := rpcCallConn(cA, 1, proto.MethodDaemonHello, proto.HelloParams{
			Gateway:  "gw-a",
			ProtoMin: proto.ProtoMin,
			ProtoMax: proto.ProtoMax,
			Client:   proto.ClientIdentity{CorralUser: "alice", HostID: "1"},
		}, &hA)
		if err != nil || rpcErr != nil {
			t.Fatalf("Hello A failed: err=%v rpcErr=%v", err, rpcErr)
		}
		if !hA.HasFeature(proto.FeatureConditionalShutdown) {
			t.Fatalf("expected HelloResult.Features to include %q, got %v", proto.FeatureConditionalShutdown, hA.Features)
		}

		var sRes proto.ShutdownResult
		rpcErr, err = rpcCallConn(cA, 2, proto.MethodDaemonShutdown, proto.ShutdownParams{
			Reason:            "admin_disabled",
			OnlyIfSoleGateway: "gw-a",
		}, &sRes)
		if err != nil || rpcErr != nil {
			t.Fatalf("ShutdownConditional A failed: err=%v rpcErr=%v", err, rpcErr)
		}
		if !sRes.OK || sRes.OtherGateways {
			t.Fatalf("expected OK=true OtherGateways=false, got %+v", sRes)
		}

		// Immediately after conditional shutdown is accepted, a new Hello from gw-b
		// MUST be rejected with ErrShuttingDown (or fail because socket closed).
		if cB, dialErr := net.DialUnix("unix", nil, &net.UnixAddr{Name: layout.SocketPath, Net: "unix"}); dialErr == nil {
			defer cB.Close()
			var hB proto.HelloResult
			rpcErrB, errB := rpcCallConn(cB, 1, proto.MethodDaemonHello, proto.HelloParams{
				Gateway:  "gw-b",
				ProtoMin: proto.ProtoMin,
				ProtoMax: proto.ProtoMax,
				Client:   proto.ClientIdentity{CorralUser: "bob", HostID: "2"},
			}, &hB)
			if errB == nil && (rpcErrB == nil || rpcErrB.Code != proto.ErrShuttingDown) {
				t.Fatalf("expected ErrShuttingDown for post-shutdown Hello, got rpcErr=%v hB=%+v", rpcErrB, hB)
			}
		}

		select {
		case err := <-doneCh:
			if err != nil {
				t.Fatalf("server exit error: %v", err)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("timed out waiting for daemon to exit after sole-gateway shutdown")
		}
	})

	t.Run("refuses_when_other_gateway_connected", func(t *testing.T) {
		_, layout, _, doneCh := startTestServerForSoleGW(t, t.TempDir(), randomHexMid(t))

		cA := dialUnixRPC(t, layout.SocketPath)
		cB := dialUnixRPC(t, layout.SocketPath)
		var hA, hB proto.HelloResult
		if rpcErr, err := rpcCallConn(cA, 1, proto.MethodDaemonHello, proto.HelloParams{
			Gateway: "gw-a", ProtoMin: 1, ProtoMax: 1, Client: proto.ClientIdentity{CorralUser: "alice", HostID: "1"},
		}, &hA); err != nil || rpcErr != nil {
			t.Fatalf("Hello A: %v / %v", err, rpcErr)
		}
		if rpcErr, err := rpcCallConn(cB, 1, proto.MethodDaemonHello, proto.HelloParams{
			Gateway: "gw-b", ProtoMin: 1, ProtoMax: 1, Client: proto.ClientIdentity{CorralUser: "bob", HostID: "2"},
		}, &hB); err != nil || rpcErr != nil {
			t.Fatalf("Hello B: %v / %v", err, rpcErr)
		}

		var sRes proto.ShutdownResult
		if rpcErr, err := rpcCallConn(cA, 2, proto.MethodDaemonShutdown, proto.ShutdownParams{
			Reason:            "admin_disabled",
			OnlyIfSoleGateway: "gw-a",
		}, &sRes); err != nil || rpcErr != nil {
			t.Fatalf("ShutdownConditional: %v / %v", err, rpcErr)
		}
		if sRes.OK || !sRes.OtherGateways {
			t.Fatalf("expected refusal OK=false OtherGateways=true, got %+v", sRes)
		}

		// Server must remain running and serving gw-b.
		var health proto.HealthResult
		if rpcErr, err := rpcCallConn(cB, 2, proto.MethodDaemonHealth, nil, &health); err != nil || rpcErr != nil {
			t.Fatalf("daemon died after refused conditional shutdown: %v / %v", err, rpcErr)
		}
		select {
		case <-doneCh:
			t.Fatal("server unexpectedly exited after refused conditional shutdown")
		case <-time.After(60 * time.Millisecond):
		}
	})

	t.Run("refuses_when_other_gateway_only_in_clients_registry", func(t *testing.T) {
		_, layout, _, doneCh := startTestServerForSoleGW(t, t.TempDir(), randomHexMid(t))

		cA := dialUnixRPC(t, layout.SocketPath)
		var hA proto.HelloResult
		if rpcErr, err := rpcCallConn(cA, 1, proto.MethodDaemonHello, proto.HelloParams{
			Gateway: "gw-a", ProtoMin: 1, ProtoMax: 1, Client: proto.ClientIdentity{CorralUser: "alice", HostID: "1"},
		}, &hA); err != nil || rpcErr != nil {
			t.Fatalf("Hello A: %v / %v", err, rpcErr)
		}

		// gw-b registers and immediately disconnects (so ConnectedGateways == 1,
		// but clients.json still records gw-b).
		cB, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: layout.SocketPath, Net: "unix"})
		if err != nil {
			t.Fatalf("dial B: %v", err)
		}
		var hB proto.HelloResult
		if rpcErr, err := rpcCallConn(cB, 1, proto.MethodDaemonHello, proto.HelloParams{
			Gateway: "gw-b", ProtoMin: 1, ProtoMax: 1, Client: proto.ClientIdentity{CorralUser: "bob", HostID: "2"},
		}, &hB); err != nil || rpcErr != nil {
			t.Fatalf("Hello B: %v / %v", err, rpcErr)
		}
		_ = cB.Close()
		time.Sleep(40 * time.Millisecond)

		var sRes proto.ShutdownResult
		if rpcErr, err := rpcCallConn(cA, 2, proto.MethodDaemonShutdown, proto.ShutdownParams{
			Reason:            "admin_disabled",
			OnlyIfSoleGateway: "gw-a",
		}, &sRes); err != nil || rpcErr != nil {
			t.Fatalf("ShutdownConditional: %v / %v", err, rpcErr)
		}
		if sRes.OK || !sRes.OtherGateways {
			t.Fatalf("expected refusal when other gateway is in clients.json, got %+v", sRes)
		}
		select {
		case <-doneCh:
			t.Fatal("server unexpectedly exited when other gateway was registered in clients.json")
		case <-time.After(60 * time.Millisecond):
		}
	})

	t.Run("concurrent_hello_vs_conditional_shutdown_mutual_exclusion", func(t *testing.T) {
		for iter := 0; iter < 10; iter++ {
			_, layout, cancel, _ := startTestServerForSoleGW(t, t.TempDir(), randomHexMid(t))
			cA := dialUnixRPC(t, layout.SocketPath)
			var hA proto.HelloResult
			if rpcErr, err := rpcCallConn(cA, 1, proto.MethodDaemonHello, proto.HelloParams{
				Gateway: "gw-a", ProtoMin: 1, ProtoMax: 1, Client: proto.ClientIdentity{CorralUser: "alice", HostID: "1"},
			}, &hA); err != nil || rpcErr != nil {
				t.Fatalf("iter %d Hello A: %v / %v", iter, err, rpcErr)
			}

			cB := dialUnixRPC(t, layout.SocketPath)
			start := make(chan struct{})
			var shutdownOK atomic.Bool
			var helloBSucceeded atomic.Bool
			var wg sync.WaitGroup
			wg.Add(2)
			go func() {
				defer wg.Done()
				<-start
				var sRes proto.ShutdownResult
				if rpcErr, err := rpcCallConn(cA, 2, proto.MethodDaemonShutdown, proto.ShutdownParams{
					Reason:            "race",
					OnlyIfSoleGateway: "gw-a",
				}, &sRes); err == nil && rpcErr == nil && sRes.OK {
					shutdownOK.Store(true)
				}
			}()
			go func() {
				defer wg.Done()
				<-start
				var hB proto.HelloResult
				if rpcErr, err := rpcCallConn(cB, 1, proto.MethodDaemonHello, proto.HelloParams{
					Gateway: "gw-b", ProtoMin: 1, ProtoMax: 1, Client: proto.ClientIdentity{CorralUser: "bob", HostID: "2"},
				}, &hB); err == nil && rpcErr == nil {
					helloBSucceeded.Store(true)
				}
			}()
			close(start)
			wg.Wait()

			if shutdownOK.Load() && helloBSucceeded.Load() {
				t.Fatalf("iter %d VIOLATION: both gw-a conditional shutdown AND gw-b Hello succeeded simultaneously!", iter)
			}
			cancel()
		}
	})

	// Regression test for Fix Round 5 Item #1 [P1] (daemon side):
	// A second active connection from the SAME gateway (e.g. established after
	// admin re-allowed corrald) MUST prevent an obsolete connection's in-flight
	// ShutdownConditional from killing the daemon.
	t.Run("refuses_when_second_connection_from_same_gateway_is_active", func(t *testing.T) {
		_, layout, _, doneCh := startTestServerForSoleGW(t, t.TempDir(), randomHexMid(t))

		cOld := dialUnixRPC(t, layout.SocketPath)
		cNew := dialUnixRPC(t, layout.SocketPath)
		var h1, h2 proto.HelloResult
		if rpcErr, err := rpcCallConn(cOld, 1, proto.MethodDaemonHello, proto.HelloParams{
			Gateway: "gw-a", ProtoMin: 1, ProtoMax: 1, Client: proto.ClientIdentity{CorralUser: "alice", HostID: "1"},
		}, &h1); err != nil || rpcErr != nil {
			t.Fatalf("Hello old: %v / %v", err, rpcErr)
		}
		if rpcErr, err := rpcCallConn(cNew, 1, proto.MethodDaemonHello, proto.HelloParams{
			Gateway: "gw-a", ProtoMin: 1, ProtoMax: 1, Client: proto.ClientIdentity{CorralUser: "alice", HostID: "1"},
		}, &h2); err != nil || rpcErr != nil {
			t.Fatalf("Hello new: %v / %v", err, rpcErr)
		}

		var sRes proto.ShutdownResult
		if rpcErr, err := rpcCallConn(cOld, 2, proto.MethodDaemonShutdown, proto.ShutdownParams{
			Reason:            "stale_admin_disabled",
			OnlyIfSoleGateway: "gw-a",
		}, &sRes); err != nil || rpcErr != nil {
			t.Fatalf("ShutdownConditional: %v / %v", err, rpcErr)
		}
		if sRes.OK || !sRes.OtherGateways {
			t.Fatalf("Fix5 Item #1 regression: expected refusal when second same-gateway conn is active, got %+v", sRes)
		}

		// Daemon must stay alive and keep serving cNew.
		var health proto.HealthResult
		if rpcErr, err := rpcCallConn(cNew, 2, proto.MethodDaemonHealth, nil, &health); err != nil || rpcErr != nil {
			t.Fatalf("daemon died despite active second same-gateway connection: %v / %v", err, rpcErr)
		}
		select {
		case <-doneCh:
			t.Fatal("server unexpectedly exited while second same-gateway connection was active")
		case <-time.After(60 * time.Millisecond):
		}
	})
}

// Regression test for Fix Round 4 Item #1 [P1] (host CLI side):
// `corrald uninstall --only-if-sole-gateway <gw>` verifies exclusivity atomically
// under .corrald-shared.lock -> op.lock -> corrald.lock before removing
// install.json or writing any tombstone.
func TestCLIUninstallOnlyIfSoleGateway(t *testing.T) {
	bins := getTestBinaries(t)

	installAndServe := func(t *testing.T, home, mid string) (string, *Layout) {
		t.Helper()
		env := []string{"CORRALD_TEST_MACHINE_ID=" + mid}
		stage := stageBinary(t, home, mid, "23071e2c5a7701c8", bins.v1)
		if out, code := runCLI(t, filepath.Join(stage, "corrald"), home, env,
			"install", "--stage", stage, "--op", "23071e2c5a7701c8", "--gateway", "gw-a", "--by", "alice"); code != 0 {
			t.Fatalf("install failed: %s", out)
		}
		installedBin := filepath.Join(home, ".local", "lib", "corral", "corrald")
		if out, code := runCLI(t, installedBin, home, env, "serve", "--gateway", "gw-a"); code != 0 {
			t.Fatalf("serve failed: %s", out)
		}
		uid := uint32(os.Getuid())
		layout, err := ResolveLayout(PathOptions{Home: home, UID: &uid, MachineID: mid})
		if err != nil {
			t.Fatalf("ResolveLayout: %v", err)
		}
		t.Cleanup(func() {
			_, _, _ = StopDaemon(layout)
		})
		return installedBin, layout
	}

	t.Run("sole_gateway_stops_and_removes_all_files", func(t *testing.T) {
		home := t.TempDir()
		mid := randomHexMid(t)
		env := []string{"CORRALD_TEST_MACHINE_ID=" + mid}
		installedBin, layout := installAndServe(t, home, mid)

		// Register gw-a via Hello and disconnect (simulating gateway closing conn before uninstall).
		var hA proto.HelloResult
		if err := callLocalRPC(layout.SocketPath, proto.MethodDaemonHello, proto.HelloParams{
			Gateway: "gw-a", ProtoMin: 1, ProtoMax: 1, Client: proto.ClientIdentity{CorralUser: "alice", HostID: "1"},
		}, &hA, 2*time.Second); err != nil {
			t.Fatalf("Hello gw-a: %v", err)
		}

		out, code := runCLI(t, installedBin, home, env, "uninstall", "--op", "bfb7f4053adf35df", "--only-if-sole-gateway", "gw-a")
		if code != 0 || parseKV(out)["result"] != "removed" {
			t.Fatalf("expected sole-gateway uninstall removed, got code=%d\n%s", code, out)
		}
		if _, err := os.Lstat(installedBin); !os.IsNotExist(err) {
			t.Fatalf("expected binary removed, stat err=%v", err)
		}
	})

	t.Run("other_gateway_registered_preserves_daemon_and_files", func(t *testing.T) {
		home := t.TempDir()
		mid := randomHexMid(t)
		env := []string{"CORRALD_TEST_MACHINE_ID=" + mid}
		installedBin, layout := installAndServe(t, home, mid)

		var h proto.HelloResult
		if err := callLocalRPC(layout.SocketPath, proto.MethodDaemonHello, proto.HelloParams{
			Gateway: "gw-a", ProtoMin: 1, ProtoMax: 1, Client: proto.ClientIdentity{CorralUser: "alice", HostID: "1"},
		}, &h, 2*time.Second); err != nil {
			t.Fatalf("Hello gw-a: %v", err)
		}
		if err := callLocalRPC(layout.SocketPath, proto.MethodDaemonHello, proto.HelloParams{
			Gateway: "gw-b", ProtoMin: 1, ProtoMax: 1, Client: proto.ClientIdentity{CorralUser: "bob", HostID: "2"},
		}, &h, 2*time.Second); err != nil {
			t.Fatalf("Hello gw-b: %v", err)
		}

		out, code := runCLI(t, installedBin, home, env, "uninstall", "--op", "36fc46328de5f8ff", "--only-if-sole-gateway", "gw-a")
		if code != 0 || parseKV(out)["result"] != "other_gateways" {
			t.Fatalf("expected result=other_gateways, got code=%d\n%s", code, out)
		}
		if _, err := os.Stat(installedBin); err != nil {
			t.Fatalf("binary must be preserved: %v", err)
		}
		if _, err := os.Stat(layout.InstallJSON); err != nil {
			t.Fatalf("install.json must be preserved: %v", err)
		}
		if layout.TombstoneExists() {
			t.Fatal("tombstone must NOT be written when other_gateways refuses uninstall")
		}
		// Daemon must still be running.
		var health proto.HealthResult
		if err := callLocalRPC(layout.SocketPath, proto.MethodDaemonHealth, nil, &health, 2*time.Second); err != nil {
			t.Fatalf("daemon must remain running: %v", err)
		}
	})

	t.Run("corrupt_clients_json_fails_closed_without_tombstone", func(t *testing.T) {
		home := t.TempDir()
		mid := randomHexMid(t)
		env := []string{"CORRALD_TEST_MACHINE_ID=" + mid}
		installedBin, layout := installAndServe(t, home, mid)

		if _, _, err := StopDaemon(layout); err != nil {
			t.Fatalf("StopDaemon: %v", err)
		}
		if err := os.WriteFile(layout.ClientsJSON, []byte("{corrupt"), 0o600); err != nil {
			t.Fatalf("write corrupt clients.json: %v", err)
		}

		out, code := runCLI(t, installedBin, home, env, "uninstall", "--op", "cc4eac9dd8eba3ca", "--only-if-sole-gateway", "gw-a")
		if code == 0 || parseKV(out)["error"] != "clients_unreadable" {
			t.Fatalf("expected error=clients_unreadable, got code=%d\n%s", code, out)
		}
		if _, err := os.Stat(installedBin); err != nil {
			t.Fatalf("binary must be preserved: %v", err)
		}
		if _, err := os.Stat(layout.InstallJSON); err != nil {
			t.Fatalf("install.json must be preserved: %v", err)
		}
		if layout.TombstoneExists() {
			t.Fatal("tombstone must NOT be written when clients.json is unreadable")
		}
	})

	t.Run("old_daemon_without_conditional_shutdown_feature_fails_closed", func(t *testing.T) {
		home := t.TempDir()
		mid := randomHexMid(t)
		env := []string{"CORRALD_TEST_MACHINE_ID=" + mid}
		stage := stageBinary(t, home, mid, "2872e3188f45e568", bins.v1)
		if out, code := runCLI(t, filepath.Join(stage, "corrald"), home, env,
			"install", "--stage", stage, "--op", "2872e3188f45e568", "--gateway", "gw-a", "--by", "alice"); code != 0 {
			t.Fatalf("install failed: %s", out)
		}
		installedBin := filepath.Join(home, ".local", "lib", "corral", "corrald")
		uid := uint32(os.Getuid())
		layout, err := ResolveLayout(PathOptions{Home: home, UID: &uid, MachineID: mid})
		if err != nil {
			t.Fatalf("ResolveLayout: %v", err)
		}
		// Hold corrald.lock to simulate a running OLD daemon, and serve a mock Unix
		// socket that returns HelloResult with NO Features field.
		lifeLock, err := AcquireLock(layout.LockPath)
		if err != nil {
			t.Fatalf("AcquireLock: %v", err)
		}
		defer lifeLock.Release()

		if err := os.MkdirAll(filepath.Dir(layout.SocketPath), 0o700); err != nil {
			t.Fatalf("mkdir socket dir: %v", err)
		}
		if layout.SocketUsesTmpDir {
			t.Cleanup(func() { _ = os.RemoveAll(layout.SocketDir) })
		}
		_ = os.Remove(layout.SocketPath)
		ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: layout.SocketPath, Net: "unix"})
		if err != nil {
			t.Fatalf("ListenUnix: %v", err)
		}
		defer ln.Close()

		var shutdownCalled atomic.Int32
		go func() {
			for {
				conn, err := ln.Accept()
				if err != nil {
					return
				}
				go func(c net.Conn) {
					defer c.Close()
					for {
						var req proto.Request
						if err := proto.ReadJSON(c, &req); err != nil {
							return
						}
						switch req.Method {
						case proto.MethodDaemonHello:
							// Old daemon: omits Features!
							res, _ := json.Marshal(proto.HelloResult{
								Version:  "0.3.1",
								Release:  1000,
								Protocol: 1,
								PID:      os.Getpid(),
							})
							_ = proto.WriteJSON(c, proto.Response{ID: req.ID, Result: res})
						case proto.MethodDaemonShutdown:
							shutdownCalled.Add(1)
							res, _ := json.Marshal(proto.ShutdownResult{OK: true})
							_ = proto.WriteJSON(c, proto.Response{ID: req.ID, Result: res})
						}
					}
				}(conn)
			}
		}()

		out, code := runCLI(t, installedBin, home, env, "uninstall", "--op", "7a14e93405c8f6bf", "--only-if-sole-gateway", "gw-a")
		if code == 0 || parseKV(out)["error"] != "unsupported_conditional_shutdown" {
			t.Fatalf("expected error=unsupported_conditional_shutdown, got code=%d\n%s", code, out)
		}
		if shutdownCalled.Load() != 0 {
			t.Fatalf("old daemon without FeatureConditionalShutdown must NEVER receive daemon.shutdown (got %d calls)", shutdownCalled.Load())
		}
		if _, err := os.Stat(installedBin); err != nil {
			t.Fatalf("binary must be preserved for old daemon: %v", err)
		}
		if _, err := os.Stat(layout.InstallJSON); err != nil {
			t.Fatalf("install.json must be preserved for old daemon: %v", err)
		}
		if layout.TombstoneExists() {
			t.Fatal("tombstone must NOT be written for old daemon")
		}
	})

	t.Run("serializes_under_shared_lock_with_install_and_handoff", func(t *testing.T) {
		home := t.TempDir()
		mid := randomHexMid(t)
		env := []string{"CORRALD_TEST_MACHINE_ID=" + mid}
		installedBin, layout := installAndServe(t, home, mid)

		var hA proto.HelloResult
		if err := callLocalRPC(layout.SocketPath, proto.MethodDaemonHello, proto.HelloParams{
			Gateway: "gw-a", ProtoMin: 1, ProtoMax: 1, Client: proto.ClientIdentity{CorralUser: "alice", HostID: "1"},
		}, &hA, 2*time.Second); err != nil {
			t.Fatalf("Hello gw-a: %v", err)
		}

		// Hold .corrald-shared.lock to simulate a concurrent install/handoff.
		sharedLk, err := AcquireLock(layout.SharedLockPath)
		if err != nil {
			t.Fatalf("AcquireLock(SharedLockPath): %v", err)
		}

		done := make(chan struct {
			out  string
			code int
		}, 1)
		go func() {
			out, code := runCLI(t, installedBin, home, env, "uninstall", "--op", "b788b491de925f29", "--only-if-sole-gateway", "gw-a")
			done <- struct {
				out  string
				code int
			}{out, code}
		}()

		select {
		case res := <-done:
			_ = sharedLk.Release()
			t.Fatalf("uninstall did NOT block on .corrald-shared.lock! res=%+v", res)
		case <-time.After(200 * time.Millisecond):
		}

		// While SharedLock is held, another gateway registers before we release SharedLock!
		// Once SharedLock is released, uninstall MUST observe gw-b and refuse!
		var hB proto.HelloResult
		if err := callLocalRPC(layout.SocketPath, proto.MethodDaemonHello, proto.HelloParams{
			Gateway: "gw-b", ProtoMin: 1, ProtoMax: 1, Client: proto.ClientIdentity{CorralUser: "bob", HostID: "2"},
		}, &hB, 2*time.Second); err != nil {
			_ = sharedLk.Release()
			t.Fatalf("Hello gw-b while shared lock held: %v", err)
		}
		if err := sharedLk.Release(); err != nil && !errors.Is(err, os.ErrClosed) {
			t.Fatalf("release shared lock: %v", err)
		}

		select {
		case res := <-done:
			if res.code != 0 || parseKV(res.out)["result"] != "other_gateways" {
				t.Fatalf("expected uninstall to observe gw-b after acquiring SharedLock and return other_gateways, got code=%d\n%s", res.code, res.out)
			}
			if !strings.Contains(res.out, "result=other_gateways") {
				t.Fatalf("unexpected output: %s", res.out)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("timed out waiting for blocked uninstall after SharedLock release")
		}
	})

	// Regression tests for Fix Round 5 Item #4 [P2] (CLI reentrant uninstall):
	t.Run("reentrant_tombstone_write_failure_restores_install_json_and_recovers", func(t *testing.T) {
		home := t.TempDir()
		mid := randomHexMid(t)
		env := []string{"CORRALD_TEST_MACHINE_ID=" + mid}
		installedBin, layout := installAndServe(t, home, mid)

		var hA proto.HelloResult
		if err := callLocalRPC(layout.SocketPath, proto.MethodDaemonHello, proto.HelloParams{
			Gateway: "gw-a", ProtoMin: 1, ProtoMax: 1, Client: proto.ClientIdentity{CorralUser: "alice", HostID: "1"},
		}, &hA, 2*time.Second); err != nil {
			t.Fatalf("Hello gw-a: %v", err)
		}

		// Block tombstone creation with a non-empty directory at removing.
		if err := os.MkdirAll(filepath.Join(layout.TombstonePath, "blocker"), 0o700); err != nil {
			t.Fatal(err)
		}
		out, code := runCLI(t, installedBin, home, env, "uninstall", "--op", "064d8c0e95ec4e41", "--only-if-sole-gateway", "gw-a")
		if code == 0 || parseKV(out)["error"] != "tombstone_failed" {
			t.Fatalf("expected error=tombstone_failed, got code=%d\n%s", code, out)
		}
		// install.json MUST have been restored when tombstone write failed!
		if _, err := os.Stat(layout.InstallJSON); err != nil {
			t.Fatalf("Fix5 Item #4 regression: install.json lost after tombstone write failure: %v", err)
		}

		// Clear blocker and retry -> must succeed cleanly to result=removed!
		if err := os.RemoveAll(layout.TombstonePath); err != nil {
			t.Fatal(err)
		}
		out, code = runCLI(t, installedBin, home, env, "uninstall", "--op", "e3dbf04c0d4cd308", "--only-if-sole-gateway", "gw-a")
		if code != 0 || parseKV(out)["result"] != "removed" {
			t.Fatalf("expected retry after tombstone_failed to succeed with result=removed, got code=%d\n%s", code, out)
		}
		if _, err := os.Lstat(installedBin); !os.IsNotExist(err) {
			t.Fatalf("expected binary removed on retry, stat err=%v", err)
		}
	})

	t.Run("reentrant_partial_lib_delete_after_install_json_removed_recovers", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("chmod 0500 does not block root")
		}
		home := t.TempDir()
		mid := randomHexMid(t)
		env := []string{"CORRALD_TEST_MACHINE_ID=" + mid}
		installedBin, layout := installAndServe(t, home, mid)

		var hA proto.HelloResult
		if err := callLocalRPC(layout.SocketPath, proto.MethodDaemonHello, proto.HelloParams{
			Gateway: "gw-a", ProtoMin: 1, ProtoMax: 1, Client: proto.ClientIdentity{CorralUser: "alice", HostID: "1"},
		}, &hA, 2*time.Second); err != nil {
			t.Fatalf("Hello gw-a: %v", err)
		}

		// Lock the version directory so removing binaries fails transiently AFTER
		// install.json has been removed and tombstone written.
		entries, err := os.ReadDir(layout.LibDir)
		if err != nil {
			t.Fatal(err)
		}
		var verDir string
		for _, e := range entries {
			if e.IsDir() && strings.HasPrefix(e.Name(), "corrald-") {
				verDir = filepath.Join(layout.LibDir, e.Name())
				break
			}
		}
		if verDir == "" {
			t.Fatalf("could not find version dir in %s", layout.LibDir)
		}
		if err := os.Chmod(verDir, 0o500); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(verDir, 0o700) })

		out, code := runCLI(t, installedBin, home, env, "uninstall", "--op", "1c09c6e6e83a50c5", "--only-if-sole-gateway", "gw-a")
		if code == 0 || parseKV(out)["result"] != "partial" {
			t.Fatalf("expected result=partial while verDir is read-only, got code=%d\n%s", code, out)
		}
		// cmdUninstall MUST preserve the manifest-bearing removing tombstone on
		// transient partial failure so retry can recover automatically!
		if !layout.TombstoneExists() {
			t.Fatal("Fix5 Item #4 regression: removing tombstone was deleted despite partial LibDir failure")
		}

		// Fault clears -> retry using versioned binary MUST recover automatically
		// via the preserved tombstone manifest and return result=removed!
		if err := os.Chmod(verDir, 0o700); err != nil {
			t.Fatal(err)
		}
		retryBin := filepath.Join(verDir, "corrald")
		out, code = runCLI(t, retryBin, home, env, "uninstall", "--op", "3609269a820a4791", "--only-if-sole-gateway", "gw-a")
		if code != 0 || parseKV(out)["result"] != "removed" {
			t.Fatalf("Fix5 Item #4 regression: retry from tombstone+missing install.json failed: code=%d\n%s", code, out)
		}
		if _, err := os.Lstat(verDir); !os.IsNotExist(err) {
			t.Fatalf("expected version dir removed on retry, stat err=%v", err)
		}
		if _, err := os.Lstat(layout.StateDir); !os.IsNotExist(err) {
			t.Fatalf("expected state dir removed on retry, stat err=%v", err)
		}
	})

	// Regression tests for Fix Round 6 F1 [P1]:
	// When CLI sole-gateway uninstall runs while the daemon is already inside its
	// 15ms pre-exit window (answering daemon.hello with ErrShuttingDown, or closing
	// its socket right before releasing corrald.lock), CLI MUST wait for corrald.lock
	// and let the authoritative clients.json re-check under SharedLock -> OpLock ->
	// lifeLock decide removed vs other_gateways (never failing transiently with
	// daemon_unreachable, and never deleting files needed by another gateway).
	t.Run("f1_shutting_down_window_sole_gateway_waits_and_removes", func(t *testing.T) {
		home := t.TempDir()
		mid := randomHexMid(t)
		env := []string{"CORRALD_TEST_MACHINE_ID=" + mid}
		stage := stageBinary(t, home, mid, "df9380c60d9d0227", bins.v1)
		if out, code := runCLI(t, filepath.Join(stage, "corrald"), home, env,
			"install", "--stage", stage, "--op", "df9380c60d9d0227", "--gateway", "gw-a", "--by", "alice"); code != 0 {
			t.Fatalf("install: %s", out)
		}
		installedBin := filepath.Join(home, ".local", "lib", "corral", "corrald")
		uid := uint32(os.Getuid())
		layout, err := ResolveLayout(PathOptions{Home: home, UID: &uid, MachineID: mid})
		if err != nil {
			t.Fatalf("ResolveLayout: %v", err)
		}
		if err := os.WriteFile(layout.ClientsJSON, []byte(`[{"gateway":"gw-a","corral_user":"alice"}]`), 0o600); err != nil {
			t.Fatal(err)
		}
		lifeLock, err := AcquireLock(layout.LockPath)
		if err != nil {
			t.Fatalf("AcquireLock: %v", err)
		}
		if err := os.MkdirAll(filepath.Dir(layout.SocketPath), 0o700); err != nil {
			_ = lifeLock.Release()
			t.Fatal(err)
		}
		if layout.SocketUsesTmpDir {
			t.Cleanup(func() { _ = os.RemoveAll(layout.SocketDir) })
		}
		_ = os.Remove(layout.SocketPath)
		ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: layout.SocketPath, Net: "unix"})
		if err != nil {
			_ = lifeLock.Release()
			t.Fatalf("ListenUnix: %v", err)
		}
		defer ln.Close()

		// Fake daemon inside its shutting-down window: rejects daemon.hello with
		// ErrShuttingDown, then exits (closes socket + releases corrald.lock) 80ms later.
		go func() {
			conn, aerr := ln.Accept()
			if aerr != nil {
				return
			}
			var req proto.Request
			if rerr := proto.ReadJSON(conn, &req); rerr == nil {
				_ = proto.WriteJSON(conn, proto.Response{
					ID:    req.ID,
					Error: &proto.RPCError{Code: proto.ErrShuttingDown, Message: "daemon is shutting down"},
				})
			}
			_ = conn.Close()
			time.Sleep(80 * time.Millisecond)
			_ = ln.Close()
			_ = os.Remove(layout.SocketPath)
			_ = lifeLock.Release()
		}()

		out, code := runCLI(t, installedBin, home, env, "uninstall", "--op", "bea7b67d56c0cbb1", "--only-if-sole-gateway", "gw-a")
		if code != 0 || parseKV(out)["result"] != "removed" {
			t.Fatalf("F1 regression: expected uninstall during ErrShuttingDown window to wait and return result=removed, got code=%d\n%s", code, out)
		}
		if _, err := os.Lstat(installedBin); !os.IsNotExist(err) {
			t.Fatalf("expected binary removed, stat err=%v", err)
		}
		if layout.SocketUsesTmpDir {
			if _, err := os.Lstat(layout.SocketDir); !os.IsNotExist(err) {
				t.Fatalf("F2 regression: expected tmp socket dir %s removed after uninstall, stat err=%v", layout.SocketDir, err)
			}
		}
	})

	t.Run("f1_shutting_down_window_with_other_gateway_in_clients_json_preserves_files", func(t *testing.T) {
		home := t.TempDir()
		mid := randomHexMid(t)
		env := []string{"CORRALD_TEST_MACHINE_ID=" + mid}
		stage := stageBinary(t, home, mid, "b183d3b1002342da", bins.v1)
		if out, code := runCLI(t, filepath.Join(stage, "corrald"), home, env,
			"install", "--stage", stage, "--op", "b183d3b1002342da", "--gateway", "gw-a", "--by", "alice"); code != 0 {
			t.Fatalf("install: %s", out)
		}
		installedBin := filepath.Join(home, ".local", "lib", "corral", "corrald")
		uid := uint32(os.Getuid())
		layout, err := ResolveLayout(PathOptions{Home: home, UID: &uid, MachineID: mid})
		if err != nil {
			t.Fatalf("ResolveLayout: %v", err)
		}
		// Another gateway gw-b is registered in clients.json! Even though the daemon
		// is shutting down (e.g. from an unconditional stop or idle exit), CLI MUST
		// re-check clients.json under SharedLock -> OpLock -> lifeLock and preserve files.
		if err := os.WriteFile(layout.ClientsJSON, []byte(`[{"gateway":"gw-a","corral_user":"alice"},{"gateway":"gw-b","corral_user":"bob"}]`), 0o600); err != nil {
			t.Fatal(err)
		}
		lifeLock, err := AcquireLock(layout.LockPath)
		if err != nil {
			t.Fatalf("AcquireLock: %v", err)
		}
		if err := os.MkdirAll(filepath.Dir(layout.SocketPath), 0o700); err != nil {
			_ = lifeLock.Release()
			t.Fatal(err)
		}
		if layout.SocketUsesTmpDir {
			t.Cleanup(func() { _ = os.RemoveAll(layout.SocketDir) })
		}
		_ = os.Remove(layout.SocketPath)
		ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: layout.SocketPath, Net: "unix"})
		if err != nil {
			_ = lifeLock.Release()
			t.Fatalf("ListenUnix: %v", err)
		}
		defer ln.Close()

		go func() {
			conn, aerr := ln.Accept()
			if aerr != nil {
				return
			}
			var req proto.Request
			if rerr := proto.ReadJSON(conn, &req); rerr == nil {
				_ = proto.WriteJSON(conn, proto.Response{
					ID:    req.ID,
					Error: &proto.RPCError{Code: proto.ErrShuttingDown, Message: "daemon is shutting down"},
				})
			}
			_ = conn.Close()
			time.Sleep(80 * time.Millisecond)
			_ = ln.Close()
			_ = os.Remove(layout.SocketPath)
			_ = lifeLock.Release()
		}()

		out, code := runCLI(t, installedBin, home, env, "uninstall", "--op", "68fb07dc9a7c17b3", "--only-if-sole-gateway", "gw-a")
		if code != 0 || parseKV(out)["result"] != "other_gateways" {
			t.Fatalf("F1 safety regression: expected result=other_gateways when gw-b is in clients.json, got code=%d\n%s", code, out)
		}
		if _, err := os.Stat(installedBin); err != nil {
			t.Fatalf("binary must be preserved for gw-b: %v", err)
		}
		if _, err := os.Stat(layout.InstallJSON); err != nil {
			t.Fatalf("install.json must be preserved for gw-b: %v", err)
		}
		if layout.TombstoneExists() {
			t.Fatal("tombstone must NOT be created when gw-b is in clients.json")
		}
	})

	t.Run("f1_socket_closed_before_lock_release_waits_and_removes", func(t *testing.T) {
		home := t.TempDir()
		mid := randomHexMid(t)
		env := []string{"CORRALD_TEST_MACHINE_ID=" + mid}
		stage := stageBinary(t, home, mid, "cb4b7082efb493b6", bins.v1)
		if out, code := runCLI(t, filepath.Join(stage, "corrald"), home, env,
			"install", "--stage", stage, "--op", "cb4b7082efb493b6", "--gateway", "gw-a", "--by", "alice"); code != 0 {
			t.Fatalf("install: %s", out)
		}
		installedBin := filepath.Join(home, ".local", "lib", "corral", "corrald")
		uid := uint32(os.Getuid())
		layout, err := ResolveLayout(PathOptions{Home: home, UID: &uid, MachineID: mid})
		if err != nil {
			t.Fatalf("ResolveLayout: %v", err)
		}
		if layout.SocketUsesTmpDir {
			t.Cleanup(func() { _ = os.RemoveAll(layout.SocketDir) })
		}
		if err := os.WriteFile(layout.ClientsJSON, []byte(`[{"gateway":"gw-a","corral_user":"alice"}]`), 0o600); err != nil {
			t.Fatal(err)
		}
		// Hold corrald.lock with no listening socket (daemon already closed socket
		// during exit), then release corrald.lock after 100ms.
		lifeLock, err := AcquireLock(layout.LockPath)
		if err != nil {
			t.Fatalf("AcquireLock: %v", err)
		}
		go func() {
			time.Sleep(100 * time.Millisecond)
			_ = lifeLock.Release()
		}()

		out, code := runCLI(t, installedBin, home, env, "uninstall", "--op", "3da57b93fe94fd6b", "--only-if-sole-gateway", "gw-a")
		if code != 0 || parseKV(out)["result"] != "removed" {
			t.Fatalf("expected uninstall to succeed after daemon released lock shortly after socket close, got code=%d\n%s", code, out)
		}
	})

	t.Run("f1_unreachable_live_daemon_holding_lock_still_fails_closed", func(t *testing.T) {
		home := t.TempDir()
		mid := randomHexMid(t)
		env := []string{"CORRALD_TEST_MACHINE_ID=" + mid}
		stage := stageBinary(t, home, mid, "ca9370a00e1003f2", bins.v1)
		if out, code := runCLI(t, filepath.Join(stage, "corrald"), home, env,
			"install", "--stage", stage, "--op", "ca9370a00e1003f2", "--gateway", "gw-a", "--by", "alice"); code != 0 {
			t.Fatalf("install: %s", out)
		}
		installedBin := filepath.Join(home, ".local", "lib", "corral", "corrald")
		uid := uint32(os.Getuid())
		layout, err := ResolveLayout(PathOptions{Home: home, UID: &uid, MachineID: mid})
		if err != nil {
			t.Fatalf("ResolveLayout: %v", err)
		}
		if layout.SocketUsesTmpDir {
			t.Cleanup(func() { _ = os.RemoveAll(layout.SocketDir) })
		}
		// Hold corrald.lock for the entire duration with no socket -> must fail closed!
		lifeLock, err := AcquireLock(layout.LockPath)
		if err != nil {
			t.Fatalf("AcquireLock: %v", err)
		}
		defer lifeLock.Release()

		out, code := runCLI(t, installedBin, home, env, "uninstall", "--op", "93a498853c552977", "--only-if-sole-gateway", "gw-a")
		if code == 0 || parseKV(out)["error"] != "daemon_unreachable" {
			t.Fatalf("expected error=daemon_unreachable when lock remains held with unreachable socket, got code=%d\n%s", code, out)
		}
		if _, err := os.Stat(installedBin); err != nil {
			t.Fatalf("binary must be preserved when daemon is unreachable: %v", err)
		}
	})

	// Regression test for Fix Round 7 Item #4 [P2]:
	// Tombstone must record the UNINSTALLING gateway (gw-b), NOT the original
	// installer (gw-a), so after clients.json is deleted during a partial
	// failure, gw-b's retry succeeds instead of returning other_gateways forever.
	t.Run("fix7_item4_tombstone_records_uninstalling_gateway_not_installer", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("chmod 0500 does not block root")
		}
		home := t.TempDir()
		mid := randomHexMid(t)
		env := []string{"CORRALD_TEST_MACHINE_ID=" + mid}
		// Originally installed by gw-a.
		installedBin, layout := installAndServe(t, home, mid)

		// Now ONLY gw-b is registered in clients.json (gw-a left).
		if _, _, err := StopDaemon(layout); err != nil {
			t.Fatalf("StopDaemon: %v", err)
		}
		if err := os.WriteFile(layout.ClientsJSON, []byte(`[{"gateway":"gw-b","corral_user":"bob"}]`), 0o600); err != nil {
			t.Fatal(err)
		}

		// Force a partial failure on gw-b's first uninstall by locking version dir.
		entries, err := os.ReadDir(layout.LibDir)
		if err != nil {
			t.Fatal(err)
		}
		var verDir string
		for _, e := range entries {
			if e.IsDir() && strings.HasPrefix(e.Name(), "corrald-") {
				verDir = filepath.Join(layout.LibDir, e.Name())
				break
			}
		}
		if verDir == "" {
			t.Fatalf("version dir not found in %s", layout.LibDir)
		}
		if err := os.Chmod(verDir, 0o500); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(verDir, 0o700) })

		out, code := runCLI(t, installedBin, home, env, "uninstall", "--op", "d28cd63aad030770", "--only-if-sole-gateway", "gw-b")
		if code == 0 || parseKV(out)["result"] != "partial" {
			t.Fatalf("expected partial on locked verDir, got code=%d\n%s", code, out)
		}
		if !layout.TombstoneExists() {
			t.Fatal("expected removing tombstone preserved on partial")
		}
		// Clear fault -> gw-b retries after clients.json is gone!
		if err := os.Chmod(verDir, 0o700); err != nil {
			t.Fatal(err)
		}
		retryBin := filepath.Join(verDir, "corrald")
		out, code = runCLI(t, retryBin, home, env, "uninstall", "--op", "8c1442d6c6401c68", "--only-if-sole-gateway", "gw-b")
		if code != 0 || parseKV(out)["result"] != "removed" {
			t.Fatalf("Fix7 Item #4 regression: gw-b retry failed (tombstone recorded wrong gateway?): code=%d\n%s", code, out)
		}
	})

	// Regression test for Fix Round 7 Item #6 [P2]:
	// A truncated, unreadable, or gateway-less removing tombstone when
	// install.json is missing MUST fail closed with error=tombstone_unreadable
	// and preserve all files.
	t.Run("fix7_item6_truncated_or_incomplete_tombstone_fails_closed", func(t *testing.T) {
		for _, tc := range []struct {
			name    string
			content string
		}{
			{"truncated_json", `{"created_at":"2026-11-28`},
			{"plain_timestamp", "2026-11-28T00:00:00Z\n"},
			{"missing_gateway", `{"created_at":"2026-11-28T00:00:00Z"}` + "\n"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				home := t.TempDir()
				mid := randomHexMid(t)
				env := []string{"CORRALD_TEST_MACHINE_ID=" + mid}
				installedBin, layout := installAndServe(t, home, mid)
				if _, _, err := StopDaemon(layout); err != nil {
					t.Fatalf("StopDaemon: %v", err)
				}
				_ = os.Remove(layout.InstallJSON)
				_ = os.Remove(layout.ClientsJSON)
				if err := os.WriteFile(layout.TombstonePath, []byte(tc.content), 0o600); err != nil {
					t.Fatal(err)
				}

				out, code := runCLI(t, installedBin, home, env, "uninstall", "--op", "5aea1c8f2580a376", "--only-if-sole-gateway", "gw-a")
				if code == 0 || parseKV(out)["error"] != "tombstone_unreadable" {
					t.Fatalf("Fix7 Item #6 regression (%s): expected error=tombstone_unreadable, got code=%d\n%s", tc.name, code, out)
				}
				if _, err := os.Stat(installedBin); err != nil {
					t.Fatalf("binary must be preserved when tombstone is %s: %v", tc.name, err)
				}
			})
		}
	})

	// Regression test for Fix Round 8 Item #3 [P1]:
	// cmdUninstall MUST check run/ (and every other leftover) BEFORE removing
	// the removing tombstone, and MUST keep the tombstone whenever run/ cannot
	// be removed so a subsequent retry can still recover cleanly.
	t.Run("fix8_item3_run_dir_leftover_keeps_tombstone_for_retry", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("chmod 0500 does not block root")
		}
		home := t.TempDir()
		mid := randomHexMid(t)
		env := []string{"CORRALD_TEST_MACHINE_ID=" + mid}
		installedBin, layout := installAndServe(t, home, mid)

		var h proto.HelloResult
		if err := callLocalRPC(layout.SocketPath, proto.MethodDaemonHello, proto.HelloParams{
			Gateway: "gw-a", ProtoMin: 1, ProtoMax: 1, Client: proto.ClientIdentity{CorralUser: "alice", HostID: "1"},
		}, &h, 2*time.Second); err != nil {
			t.Fatalf("Hello gw-a: %v", err)
		}

		// Create run/ with a leftover file and make run/ read-only so deletion fails.
		if err := os.MkdirAll(layout.RunDir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(layout.RunDir, "corral-send-tmux-_0"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(layout.RunDir, 0o500); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(layout.RunDir, 0o700) })

		out, code := runCLI(t, installedBin, home, env, "uninstall", "--op", "19d44765e3d45c48", "--only-if-sole-gateway", "gw-a")
		if code == 0 || parseKV(out)["result"] != "partial" {
			t.Fatalf("expected partial when run/ cannot be removed, got code=%d\n%s", code, out)
		}
		if !strings.Contains(out, layout.RunDir) {
			t.Fatalf("expected left output to mention %s, got:\n%s", layout.RunDir, out)
		}
		if !layout.TombstoneExists() {
			t.Fatalf("Fix8 Item #3 regression: removing tombstone was deleted even though run/ remained!\n%s", out)
		}

		// Clear fault: reinstall binary into LibDir via --for-uninstall-recovery
		// and re-run normal locked uninstall -> cleanly removes run/, LibDir, StateDir, tombstone.
		if err := os.Chmod(layout.RunDir, 0o700); err != nil {
			t.Fatal(err)
		}
		stage := stageBinary(t, home, mid, "92f35c4079754c3c", bins.v1)
		if recOut, recCode := runCLI(t, filepath.Join(stage, "corrald"), home, env,
			"install", "--for-uninstall-recovery", "--stage", stage, "--op", "92f35c4079754c3c", "--gateway", "gw-a"); recCode != 0 || parseKV(recOut)["result"] != "ok" {
			t.Fatalf("install --for-uninstall-recovery failed: code=%d\n%s", recCode, recOut)
		}
		out2, code2 := runCLI(t, installedBin, home, env, "uninstall", "--op", "92f35c4079754c3c", "--only-if-sole-gateway", "gw-a")
		if code2 != 0 || parseKV(out2)["result"] != "removed" {
			t.Fatalf("retry uninstall after clearing run/ fault failed: code=%d\n%s", code2, out2)
		}
		if _, err := os.Lstat(layout.RunDir); !os.IsNotExist(err) {
			t.Fatalf("expected run/ removed, stat err=%v", err)
		}
		if layout.TombstoneExists() {
			t.Fatal("expected removing tombstone removed on clean retry")
		}
		if _, err := os.Lstat(layout.LibDir); !os.IsNotExist(err) {
			t.Fatalf("expected LibDir removed on clean retry, stat err=%v", err)
		}
	})

	// Regression test for Fix Round 8 Item #3 [P1] (post-check ordering):
	// When initial deletions succeed (len(left)==0) and a leftover is only
	// discovered during post-check (e.g. an unlisted corrald-* dir in LibDir
	// or recreated run/), the removing tombstone MUST NOT have been deleted yet.
	t.Run("fix8_item3_postcheck_leftover_keeps_tombstone", func(t *testing.T) {
		home2 := t.TempDir()
		mid2 := randomHexMid(t)
		env2 := []string{"CORRALD_TEST_MACHINE_ID=" + mid2}
		installedBin2, layout2 := installAndServe(t, home2, mid2)
		var h2 proto.HelloResult
		if err := callLocalRPC(layout2.SocketPath, proto.MethodDaemonHello, proto.HelloParams{
			Gateway: "gw-a", ProtoMin: 1, ProtoMax: 1, Client: proto.ClientIdentity{CorralUser: "alice", HostID: "1"},
		}, &h2, 2*time.Second); err != nil {
			t.Fatalf("Hello gw-a (case 2): %v", err)
		}
		unlistedDir := filepath.Join(layout2.LibDir, "corrald-9.9.9-99")
		if err := os.MkdirAll(unlistedDir, 0o700); err != nil {
			t.Fatal(err)
		}
		outP, codeP := runCLI(t, installedBin2, home2, env2, "uninstall", "--op", "58eaa18dcaab3d15", "--only-if-sole-gateway", "gw-a")
		if codeP == 0 || parseKV(outP)["result"] != "partial" {
			t.Fatalf("expected partial when post-check finds unlisted corrald dir, got code=%d\n%s", codeP, outP)
		}
		if !layout2.TombstoneExists() {
			t.Fatalf("Fix8 Item #3 regression: removing tombstone was deleted BEFORE post-check found %s!\n%s", unlistedDir, outP)
		}
	})

	// Regression test for Fix Round 8 Items #1+#4+#5:
	// `corrald install --for-uninstall-recovery` creates LibDir + holds
	// .corrald-shared.lock -> op.lock, validates the removing tombstone and
	// gateway, places the binary without writing install.json or removing the
	// tombstone or starting the daemon, and cleans up stage-<op>.
	t.Run("fix8_item1_4_install_for_uninstall_recovery_creates_libdir_and_locks", func(t *testing.T) {
		home := t.TempDir()
		mid := randomHexMid(t)
		env := []string{"CORRALD_TEST_MACHINE_ID=" + mid}
		uid := uint32(os.Getuid())
		layout, err := ResolveLayout(PathOptions{Home: home, UID: &uid, MachineID: mid})
		if err != nil {
			t.Fatalf("ResolveLayout: %v", err)
		}
		if err := os.MkdirAll(layout.StateDir, 0o700); err != nil {
			t.Fatal(err)
		}
		// LibDir intentionally does NOT exist yet!
		if err := os.WriteFile(layout.TombstonePath, []byte(`{"created_at":"2026-11-28T00:00:00Z","gateway":"gw-a"}`+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}

		// Wrong gateway must refuse with other_gateways and not place binary.
		stageWrong := stageBinary(t, home, mid, "80f14e0fce17fe62", bins.v1)
		outW, codeW := runCLI(t, filepath.Join(stageWrong, "corrald"), home, env,
			"install", "--for-uninstall-recovery", "--stage", stageWrong, "--op", "80f14e0fce17fe62", "--gateway", "gw-b")
		if codeW != 0 || parseKV(outW)["result"] != "other_gateways" {
			t.Fatalf("expected other_gateways for foreign tombstone, got code=%d\n%s", codeW, outW)
		}
		if _, err := os.Lstat(layout.CurrentSymlink); !os.IsNotExist(err) {
			t.Fatalf("binary must not be placed for foreign gateway, stat err=%v", err)
		}

		// Matching gateway places binary into LibDir without writing install.json or removing tombstone.
		stage := stageBinary(t, home, mid, "d914f0a10759a094", bins.v1)
		out, code := runCLI(t, filepath.Join(stage, "corrald"), home, env,
			"install", "--for-uninstall-recovery", "--stage", stage, "--op", "d914f0a10759a094", "--gateway", "gw-a")
		if code != 0 || parseKV(out)["result"] != "ok" {
			t.Fatalf("install --for-uninstall-recovery failed: code=%d\n%s", code, out)
		}
		if _, err := os.Stat(layout.CurrentSymlink); err != nil {
			t.Fatalf("expected CurrentSymlink placed in LibDir: %v", err)
		}
		if _, err := os.Stat(layout.SharedLockPath); err != nil {
			t.Fatalf("expected .corrald-shared.lock created in LibDir: %v", err)
		}
		if _, err := os.Lstat(layout.InstallJSON); !os.IsNotExist(err) {
			t.Fatalf("install.json must NOT be written during uninstall recovery, err=%v", err)
		}
		if !layout.TombstoneExists() {
			t.Fatal("removing tombstone must remain intact after recovery binary placement")
		}
		if _, err := os.Lstat(stage); !os.IsNotExist(err) {
			t.Fatalf("stage dir must be removed, stat err=%v", err)
		}
		// Now normal locked uninstall cleans up everything including LibDir and tombstone.
		uOut, uCode := runCLI(t, layout.CurrentSymlink, home, env, "uninstall", "--op", "d914f0a10759a094", "--only-if-sole-gateway", "gw-a")
		if uCode != 0 || parseKV(uOut)["result"] != "removed" {
			t.Fatalf("locked uninstall after recovery install failed: code=%d\n%s", uCode, uOut)
		}
		if _, err := os.Lstat(layout.LibDir); !os.IsNotExist(err) {
			t.Fatalf("expected LibDir removed, stat err=%v", err)
		}
		if _, err := os.Lstat(layout.StateDir); !os.IsNotExist(err) {
			t.Fatalf("expected StateDir removed, stat err=%v", err)
		}
	})

	// Regression test for Fix Round 9 Item #2 [P2]:
	// The removing tombstone MUST be removed LAST, after LibDir, lock files,
	// StateDir, and StateRoot removals succeed; if removing LibDir or StateDir
	// fails in Part B, the tombstone MUST remain on disk for retry.
	t.Run("fix9_item2_tombstone_removed_last_after_dir_removals", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("chmod 0500 does not block root")
		}
		// Case A: ~/.local/lib is read-only -> all files inside LibDir/StateDir
		// delete cleanly in Part A, then os.Remove(LibDir) fails in Part B.
		t.Run("libdir_rmdir_failure_keeps_tombstone", func(t *testing.T) {
			home := t.TempDir()
			mid := randomHexMid(t)
			env := []string{"CORRALD_TEST_MACHINE_ID=" + mid}
			installedBin, layout := installAndServe(t, home, mid)

			var h proto.HelloResult
			if err := callLocalRPC(layout.SocketPath, proto.MethodDaemonHello, proto.HelloParams{
				Gateway: "gw-a", ProtoMin: 1, ProtoMax: 1, Client: proto.ClientIdentity{CorralUser: "alice", HostID: "1"},
			}, &h, 2*time.Second); err != nil {
				t.Fatalf("Hello gw-a: %v", err)
			}

			libParent := filepath.Dir(layout.LibDir)
			if err := os.Chmod(libParent, 0o500); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(libParent, 0o700) })

			out, code := runCLI(t, installedBin, home, env, "uninstall", "--op", "5c19d6cf9492066b", "--only-if-sole-gateway", "gw-a")
			if code == 0 || parseKV(out)["result"] != "partial" {
				t.Fatalf("expected result=partial when LibDir rmdir fails, got code=%d\n%s", code, out)
			}
			if !layout.TombstoneExists() {
				t.Fatalf("Fix9 Item #2 regression: removing tombstone was deleted BEFORE LibDir removal failed!\n%s", out)
			}
		})

		// Case B: StateRoot is read-only -> files delete cleanly, then
		// os.Remove(StateDir) fails in Part B -> tombstone must be restored.
		t.Run("statedir_rmdir_failure_keeps_tombstone", func(t *testing.T) {
			home := t.TempDir()
			mid := randomHexMid(t)
			env := []string{"CORRALD_TEST_MACHINE_ID=" + mid}
			installedBin, layout := installAndServe(t, home, mid)

			var h proto.HelloResult
			if err := callLocalRPC(layout.SocketPath, proto.MethodDaemonHello, proto.HelloParams{
				Gateway: "gw-a", ProtoMin: 1, ProtoMax: 1, Client: proto.ClientIdentity{CorralUser: "alice", HostID: "1"},
			}, &h, 2*time.Second); err != nil {
				t.Fatalf("Hello gw-a: %v", err)
			}

			if err := os.Chmod(layout.StateRoot, 0o500); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(layout.StateRoot, 0o700) })

			out, code := runCLI(t, installedBin, home, env, "uninstall", "--op", "71fd8ba5c9019417", "--only-if-sole-gateway", "gw-a")
			if code == 0 || parseKV(out)["result"] != "partial" {
				t.Fatalf("expected result=partial when StateDir rmdir fails, got code=%d\n%s", code, out)
			}
			if !layout.TombstoneExists() {
				t.Fatalf("Fix9 Item #2 regression: removing tombstone was not kept when StateDir rmdir failed!\n%s", out)
			}
		})
	})

	// Regression test for Fix Round 9 Item #3 [P2]:
	// `corrald install --for-uninstall-recovery` MUST use shouldUpdateSharedSymlink
	// so recovering with an older gateway binary never downgrades a newer shared symlink.
	t.Run("fix9_item3_recovery_install_never_downgrades_newer_symlink", func(t *testing.T) {
		home := t.TempDir()
		mid := randomHexMid(t)
		env := []string{"CORRALD_TEST_MACHINE_ID=" + mid}
		uid := uint32(os.Getuid())
		layout, err := ResolveLayout(PathOptions{Home: home, UID: &uid, MachineID: mid})
		if err != nil {
			t.Fatalf("ResolveLayout: %v", err)
		}

		// Install v2 (higher release) first so LibDir/corrald points to v2.
		stageV2 := stageBinary(t, home, mid, "974d3ed642c06613", bins.v2)
		if out, code := runCLI(t, filepath.Join(stageV2, "corrald"), home, env,
			"install", "--stage", stageV2, "--op", "974d3ed642c06613", "--gateway", "gw-a", "--by", "alice"); code != 0 {
			t.Fatalf("install v2 failed: %s", out)
		}
		v2Target, err := os.Readlink(layout.CurrentSymlink)
		if err != nil {
			t.Fatalf("Readlink v2: %v", err)
		}

		// Simulate removing tombstone on this machine while shared LibDir still holds v2.
		_ = os.Remove(layout.InstallJSON)
		if err := os.WriteFile(layout.TombstonePath, []byte(`{"created_at":"2026-11-28T00:00:00Z","gateway":"gw-a"}`+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}

		// Run recovery install with older v1 binary.
		stageV1 := stageBinary(t, home, mid, "498578ff6730b962", bins.v1)
		out, code := runCLI(t, filepath.Join(stageV1, "corrald"), home, env,
			"install", "--for-uninstall-recovery", "--stage", stageV1, "--op", "498578ff6730b962", "--gateway", "gw-a")
		if code != 0 || parseKV(out)["result"] != "ok" {
			t.Fatalf("recovery install v1 failed: code=%d\n%s", code, out)
		}
		gotTarget, err := os.Readlink(layout.CurrentSymlink)
		if err != nil {
			t.Fatalf("Readlink after recovery: %v", err)
		}
		if gotTarget != v2Target {
			t.Fatalf("Fix9 Item #3 regression: recovery install downgraded shared symlink from %q to %q!", v2Target, gotTarget)
		}
	})

	// Regression test for Fix Round 10 Item #1 [P1]:
	// Uninstall MUST NEVER remove directories by "corrald-" prefix (e.g.
	// LibDir/corrald-backups/corrald) and MUST NEVER RemoveAll a directory
	// holding files corrald did not create.
	t.Run("fix10_item1_corrald_backups_and_user_files_preserved", func(t *testing.T) {
		t.Run("binary_under_corrald_backups_preserves_backup_dir_and_user_file", func(t *testing.T) {
			home := t.TempDir()
			mid := randomHexMid(t)
			env := []string{"CORRALD_TEST_MACHINE_ID=" + mid}
			_, layout := installAndServe(t, home, mid)

			var h proto.HelloResult
			if err := callLocalRPC(layout.SocketPath, proto.MethodDaemonHello, proto.HelloParams{
				Gateway: "gw-a", ProtoMin: 1, ProtoMax: 1, Client: proto.ClientIdentity{CorralUser: "alice", HostID: "1"},
			}, &h, 2*time.Second); err != nil {
				t.Fatalf("Hello gw-a: %v", err)
			}

			// Place a copy of corrald AND a user file under LibDir/corrald-backups/.
			backupDir := filepath.Join(layout.LibDir, "corrald-backups")
			if err := os.MkdirAll(backupDir, 0o700); err != nil {
				t.Fatal(err)
			}
			binBytes, err := os.ReadFile(bins.v1)
			if err != nil {
				t.Fatal(err)
			}
			backupBin := filepath.Join(backupDir, "corrald")
			if err := os.WriteFile(backupBin, binBytes, 0o700); err != nil {
				t.Fatal(err)
			}
			userFile := filepath.Join(backupDir, "important-user-data.txt")
			if err := os.WriteFile(userFile, []byte("do not delete\n"), 0o600); err != nil {
				t.Fatal(err)
			}

			// Execute uninstall directly from LibDir/corrald-backups/corrald!
			out, code := runCLI(t, backupBin, home, env, "uninstall", "--op", "1ecdccb759ba69fd", "--only-if-sole-gateway", "gw-a")
			if code != 0 || parseKV(out)["result"] != "removed" {
				t.Fatalf("expected clean uninstall of real corrald while preserving corrald-backups, got code=%d\n%s", code, out)
			}
			gotUser, err := os.ReadFile(userFile)
			if err != nil || string(gotUser) != "do not delete\n" {
				t.Fatalf("Fix10 Item #1 regression: user file under corrald-backups/ was deleted! err=%v content=%q", err, string(gotUser))
			}
			if _, err := os.Stat(backupDir); err != nil {
				t.Fatalf("Fix10 Item #1 regression: corrald-backups/ dir was deleted: %v", err)
			}
			if _, err := os.Stat(backupBin); err != nil {
				t.Fatalf("Fix10 Item #1 regression: corrald-backups/corrald was deleted: %v", err)
			}
		})

		t.Run("extra_user_file_inside_version_dir_survives_and_reports_partial", func(t *testing.T) {
			home := t.TempDir()
			mid := randomHexMid(t)
			env := []string{"CORRALD_TEST_MACHINE_ID=" + mid}
			installedBin, layout := installAndServe(t, home, mid)

			var h proto.HelloResult
			if err := callLocalRPC(layout.SocketPath, proto.MethodDaemonHello, proto.HelloParams{
				Gateway: "gw-a", ProtoMin: 1, ProtoMax: 1, Client: proto.ClientIdentity{CorralUser: "alice", HostID: "1"},
			}, &h, 2*time.Second); err != nil {
				t.Fatalf("Hello gw-a: %v", err)
			}

			resolvedBin, err := filepath.EvalSymlinks(installedBin)
			if err != nil {
				t.Fatal(err)
			}
			verDir := filepath.Dir(resolvedBin)
			userNote := filepath.Join(verDir, "user-note.txt")
			if err := os.WriteFile(userNote, []byte("keep me\n"), 0o600); err != nil {
				t.Fatal(err)
			}

			out, code := runCLI(t, installedBin, home, env, "uninstall", "--op", "adf15e2473380112", "--only-if-sole-gateway", "gw-a")
			if code == 0 || parseKV(out)["result"] != "partial" {
				t.Fatalf("Fix10 Item #1 regression: expected partial when version dir has unknown user file, got code=%d\n%s", code, out)
			}
			got, err := os.ReadFile(userNote)
			if err != nil || string(got) != "keep me\n" {
				t.Fatalf("Fix10 Item #1 regression: user file inside version dir was deleted! err=%v content=%q", err, string(got))
			}
		})
	})

	// Regression test for Fix Round 10 Item #2 [P2]:
	// SharedLockPath MUST remain on disk until EVERY state lock, tombstone,
	// StateDir, and StateRoot removal has succeeded; if StateDir removal fails,
	// BOTH TombstonePath AND SharedLockPath MUST remain on disk.
	t.Run("fix10_item2_shared_lock_preserved_until_all_state_removed", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("chmod 0500 does not block root")
		}
		home := t.TempDir()
		mid := randomHexMid(t)
		env := []string{"CORRALD_TEST_MACHINE_ID=" + mid}
		installedBin, layout := installAndServe(t, home, mid)

		var h proto.HelloResult
		if err := callLocalRPC(layout.SocketPath, proto.MethodDaemonHello, proto.HelloParams{
			Gateway: "gw-a", ProtoMin: 1, ProtoMax: 1, Client: proto.ClientIdentity{CorralUser: "alice", HostID: "1"},
		}, &h, 2*time.Second); err != nil {
			t.Fatalf("Hello gw-a: %v", err)
		}

		if err := os.Chmod(layout.StateRoot, 0o500); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(layout.StateRoot, 0o700) })

		out, code := runCLI(t, installedBin, home, env, "uninstall", "--op", "65c7e8ceb0b2e848", "--only-if-sole-gateway", "gw-a")
		if code == 0 || parseKV(out)["result"] != "partial" {
			t.Fatalf("expected partial when StateRoot is read-only, got code=%d\n%s", code, out)
		}
		if !layout.TombstoneExists() {
			t.Fatalf("expected tombstone kept when StateDir removal fails:\n%s", out)
		}
		if _, err := os.Stat(layout.SharedLockPath); err != nil {
			t.Fatalf("Fix10 Item #2 regression: .corrald-shared.lock was unlinked before StateDir removal succeeded: %v\n%s", err, out)
		}
	})

	// Regression tests for Fix Round 11 Items #1-#4 [P1]:
	// Single RemoveOwned implementation MUST never follow symlinks, never
	// traverse symlinked ops/stage-* directory roots, and never delete unowned
	// files like .notes.tmp.backup, ops/notes.json, or stage-backups/notes.txt.
	t.Run("fix11_items1_4_owned_removal_protects_symlinks_and_unowned_files", func(t *testing.T) {
		t.Run("ops_symlink_and_stage_symlink_never_traversed", func(t *testing.T) {
			home := t.TempDir()
			mid := randomHexMid(t)
			env := []string{"CORRALD_TEST_MACHINE_ID=" + mid}
			installedBin, layout := installAndServe(t, home, mid)

			var h proto.HelloResult
			if err := callLocalRPC(layout.SocketPath, proto.MethodDaemonHello, proto.HelloParams{
				Gateway: "gw-a", ProtoMin: 1, ProtoMax: 1, Client: proto.ClientIdentity{CorralUser: "alice", HostID: "1"},
			}, &h, 2*time.Second); err != nil {
				t.Fatalf("Hello gw-a: %v", err)
			}

			// Replace StateDir/ops with a symlink to a user directory containing a user file.
			userOpsDir := t.TempDir()
			userOpsFile := filepath.Join(userOpsDir, "0123456789abcdef.json")
			if err := os.WriteFile(userOpsFile, []byte("user-secret\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			_ = os.RemoveAll(layout.OpsDir)
			if err := os.Symlink(userOpsDir, layout.OpsDir); err != nil {
				t.Fatal(err)
			}

			// Create a symlinked stage-<16hex> pointing to a user directory containing a user file.
			userStageDir := t.TempDir()
			userStageFile := filepath.Join(userStageDir, "corrald")
			if err := os.WriteFile(userStageFile, []byte("user-binary\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			symStage := filepath.Join(layout.StateDir, "stage-deadbeef12345678")
			if err := os.Symlink(userStageDir, symStage); err != nil {
				t.Fatal(err)
			}

			out, code := runCLI(t, installedBin, home, env, "uninstall", "--op", "a897793bea2eae80", "--only-if-sole-gateway", "gw-a")
			if code == 0 || parseKV(out)["result"] != "partial" {
				t.Fatalf("Fix11 regression: expected partial when ops and stage-* are symlinks, got code=%d\n%s", code, out)
			}
			if got, err := os.ReadFile(userOpsFile); err != nil || string(got) != "user-secret\n" {
				t.Fatalf("Fix11 regression: symlinked ops target file was deleted! err=%v content=%q", err, string(got))
			}
			if got, err := os.ReadFile(userStageFile); err != nil || string(got) != "user-binary\n" {
				t.Fatalf("Fix11 regression: symlinked stage-* target file was deleted! err=%v content=%q", err, string(got))
			}
			if !strings.Contains(out, layout.OpsDir) || !strings.Contains(out, symStage) {
				t.Fatalf("Fix11 regression: expected symlinked dirs reported as leftovers, got:\n%s", out)
			}
		})

		t.Run("unowned_temp_and_ops_notes_and_stage_backups_preserved", func(t *testing.T) {
			home := t.TempDir()
			mid := randomHexMid(t)
			env := []string{"CORRALD_TEST_MACHINE_ID=" + mid}
			installedBin, layout := installAndServe(t, home, mid)

			var h proto.HelloResult
			if err := callLocalRPC(layout.SocketPath, proto.MethodDaemonHello, proto.HelloParams{
				Gateway: "gw-a", ProtoMin: 1, ProtoMax: 1, Client: proto.ClientIdentity{CorralUser: "alice", HostID: "1"},
			}, &h, 2*time.Second); err != nil {
				t.Fatalf("Hello gw-a: %v", err)
			}

			notesTmp := filepath.Join(layout.StateDir, ".notes.tmp.backup")
			opsNotes := filepath.Join(layout.OpsDir, "notes.json")
			stageBkpDir := filepath.Join(layout.StateDir, "stage-backups")
			stageBkpNotes := filepath.Join(stageBkpDir, "notes.txt")
			if err := os.MkdirAll(stageBkpDir, 0o700); err != nil {
				t.Fatal(err)
			}
			for _, f := range []string{notesTmp, opsNotes, stageBkpNotes} {
				if err := os.WriteFile(f, []byte("keep-user-content\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}

			out, code := runCLI(t, installedBin, home, env, "uninstall", "--op", "a085a16653dd746c", "--only-if-sole-gateway", "gw-a")
			if code == 0 || parseKV(out)["result"] != "partial" {
				t.Fatalf("Fix11 regression: expected partial when unowned files exist, got code=%d\n%s", code, out)
			}
			for _, f := range []string{notesTmp, opsNotes, stageBkpNotes} {
				if got, err := os.ReadFile(f); err != nil || string(got) != "keep-user-content\n" {
					t.Fatalf("Fix11 regression: unowned file %s was deleted! err=%v content=%q", f, err, string(got))
				}
			}
			if !strings.Contains(out, notesTmp) || !strings.Contains(out, opsNotes) || !strings.Contains(out, stageBkpDir) {
				t.Fatalf("Fix11 regression: expected unowned files reported in left output:\n%s", out)
			}
		})
	})

	// Regression test for Fix Round 11 Item #7 [P2]:
	// Two leftover tombstones from two machines in a shared HOME MUST NOT block
	// each other forever; each machine removes its own state dir (and shared
	// files go when no machine has install.json) so retries from both reach removed.
	t.Run("fix11_item7_two_tombstones_shared_home_no_deadlock", func(t *testing.T) {
		home := t.TempDir()
		midA := randomHexMid(t)
		midB := randomHexMid(t)
		envA := []string{"CORRALD_TEST_MACHINE_ID=" + midA}
		envB := []string{"CORRALD_TEST_MACHINE_ID=" + midB}
		uid := uint32(os.Getuid())
		layoutA, err := ResolveLayout(PathOptions{Home: home, UID: &uid, MachineID: midA})
		if err != nil {
			t.Fatal(err)
		}
		layoutB, err := ResolveLayout(PathOptions{Home: home, UID: &uid, MachineID: midB})
		if err != nil {
			t.Fatal(err)
		}

		// Install on both machines sharing HOME, then stop/delete install.json
		// and leave removing tombstones on BOTH machines.
		stageA := stageBinary(t, home, midA, "0123456789abcdef", bins.v1)
		if out, code := runCLI(t, filepath.Join(stageA, "corrald"), home, envA,
			"install", "--stage", stageA, "--op", "0123456789abcdef", "--gateway", "gw-a", "--by", "alice"); code != 0 {
			t.Fatalf("install A: %s", out)
		}
		stageB := stageBinary(t, home, midB, "fedcba9876543210", bins.v1)
		if out, code := runCLI(t, filepath.Join(stageB, "corrald"), home, envB,
			"install", "--stage", stageB, "--op", "fedcba9876543210", "--gateway", "gw-a", "--by", "alice"); code != 0 {
			t.Fatalf("install B: %s", out)
		}
		_ = os.Remove(layoutA.InstallJSON)
		_ = os.Remove(layoutB.InstallJSON)
		tomb := []byte(`{"created_at":"2026-11-28T00:00:00Z","gateway":"gw-a"}` + "\n")
		if err := os.WriteFile(layoutA.TombstonePath, tomb, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(layoutB.TombstonePath, tomb, 0o600); err != nil {
			t.Fatal(err)
		}

		// Machine A retries uninstall -> MUST succeed (not blocked by midB tombstone)!
		outA, codeA := runCLI(t, layoutA.CurrentSymlink, home, envA, "uninstall", "--op", "1111111111111111", "--only-if-sole-gateway", "gw-a")
		if codeA != 0 || parseKV(outA)["result"] != "removed" {
			t.Fatalf("Fix11 Item #7 regression: machine A blocked by machine B tombstone! code=%d\n%s", codeA, outA)
		}
		if _, err := os.Lstat(layoutA.StateDir); !os.IsNotExist(err) {
			t.Fatalf("expected midA state dir removed, stat err=%v", err)
		}
		if _, err := os.Lstat(layoutA.LibDir); !os.IsNotExist(err) {
			t.Fatalf("expected shared LibDir removed when no machine has install.json, stat err=%v", err)
		}
		if !layoutB.TombstoneExists() {
			t.Fatal("expected midB tombstone preserved for machine B's own retry")
		}

		// Machine B retries via binaryless recovery -> MUST also reach removed and clean StateRoot!
		recStageB := stageBinary(t, home, midB, "2222222222222222", bins.v1)
		if out, code := runCLI(t, filepath.Join(recStageB, "corrald"), home, envB,
			"install", "--for-uninstall-recovery", "--stage", recStageB, "--op", "2222222222222222", "--gateway", "gw-a"); code != 0 {
			t.Fatalf("recovery install B: %s", out)
		}
		outB, codeB := runCLI(t, layoutB.CurrentSymlink, home, envB, "uninstall", "--op", "2222222222222222", "--only-if-sole-gateway", "gw-a")
		if codeB != 0 || parseKV(outB)["result"] != "removed" {
			t.Fatalf("Fix11 Item #7 regression: machine B retry failed! code=%d\n%s", codeB, outB)
		}
		for _, p := range []string{layoutB.StateDir, layoutB.StateRoot, layoutB.LibDir} {
			if _, err := os.Lstat(p); !os.IsNotExist(err) {
				t.Fatalf("expected %s removed after both machines finished, stat err=%v", p, err)
			}
		}
	})

	t.Run("fix12_item1_run_dir_preserves_user_files_and_symlinks", func(t *testing.T) {
		home := t.TempDir()
		mid := randomHexMid(t)
		env := []string{"CORRALD_TEST_MACHINE_ID=" + mid}
		installedBin, layout := installAndServe(t, home, mid)

		if err := os.MkdirAll(layout.RunDir, 0o700); err != nil {
			t.Fatal(err)
		}
		ownedLock := filepath.Join(layout.RunDir, "corral-send-tmux-_0")
		if err := os.WriteFile(ownedLock, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		userNotes := filepath.Join(layout.RunDir, "notes.txt")
		if err := os.WriteFile(userNotes, []byte("keep-my-run-notes\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		extTarget := filepath.Join(home, "outside.txt")
		if err := os.WriteFile(extTarget, []byte("keep-outside\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		userLink := filepath.Join(layout.RunDir, "user-link")
		if err := os.Symlink(extTarget, userLink); err != nil {
			t.Fatal(err)
		}

		out, code := runCLI(t, installedBin, home, env, "uninstall", "--op", "1201000000000001", "--only-if-sole-gateway", "gw-a")
		if code == 0 || parseKV(out)["result"] != "partial" {
			t.Fatalf("Fix12 Item #1 regression: expected partial when run/ contains user files/symlinks, got code=%d\n%s", code, out)
		}
		if _, err := os.Lstat(ownedLock); !os.IsNotExist(err) {
			t.Fatalf("expected owned pane lock %s removed, stat err=%v", ownedLock, err)
		}
		if got, err := os.ReadFile(userNotes); err != nil || string(got) != "keep-my-run-notes\n" {
			t.Fatalf("Fix12 Item #1 regression: run/notes.txt deleted! err=%v content=%q", err, string(got))
		}
		if info, err := os.Lstat(userLink); err != nil || info.Mode()&os.ModeSymlink == 0 {
			t.Fatalf("Fix12 Item #1 regression: run/user-link deleted! err=%v", err)
		}
		if got, err := os.ReadFile(extTarget); err != nil || string(got) != "keep-outside\n" {
			t.Fatalf("Fix12 Item #1 regression: symlink target %s disturbed! err=%v", extTarget, err)
		}
		if !strings.Contains(out, "left="+userNotes+"\t") {
			t.Fatalf("expected %s in left=:\n%s", userNotes, out)
		}
	})

	t.Run("fix12_item5_socket_only_at_layout_path_and_only_if_socket", func(t *testing.T) {
		home := t.TempDir()
		mid := randomHexMid(t)
		env := []string{"CORRALD_TEST_MACHINE_ID=" + mid}
		installedBin, layout := installAndServe(t, home, mid)

		// Stop cleanly, then tamper meta.json.socket_path to point at a user file.
		if out, code := runCLI(t, installedBin, home, env, "stop"); code != 0 {
			t.Fatalf("stop: %s", out)
		}
		userNotes := filepath.Join(home, "notes.txt")
		if err := os.WriteFile(userNotes, []byte("precious\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		metaBytes, err := json.Marshal(MetaRecord{
			PID: 999999999, StartTime: "0", SocketPath: userNotes, Version: "0.1.0", Release: 100,
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(layout.MetaJSON, metaBytes, 0o600); err != nil {
			t.Fatal(err)
		}

		// StopDaemon cleanup must NEVER unlink meta.json.socket_path!
		if _, _, err := StopDaemon(layout); err != nil {
			t.Fatalf("StopDaemon: %v", err)
		}
		if got, err := os.ReadFile(userNotes); err != nil || string(got) != "precious\n" {
			t.Fatalf("Fix12 Item #5 regression: StopDaemon deleted meta.json.socket_path %s! err=%v", userNotes, err)
		}

		// Re-write tampered meta.json AND place a regular non-socket file at layout.SocketPath.
		if err := os.WriteFile(layout.MetaJSON, metaBytes, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Dir(layout.SocketPath), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(layout.SocketPath, []byte("not-a-socket\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			_ = os.Remove(layout.SocketPath)
			if layout.SocketUsesTmpDir {
				_ = os.Remove(layout.SocketDir)
			}
		})

		out, code := runCLI(t, installedBin, home, env, "uninstall", "--op", "1205000000000001", "--only-if-sole-gateway", "gw-a")
		if code == 0 || parseKV(out)["result"] != "partial" {
			t.Fatalf("Fix12 Item #5 regression: expected partial when regular file sits at layout.SocketPath, got code=%d\n%s", code, out)
		}
		if got, err := os.ReadFile(userNotes); err != nil || string(got) != "precious\n" {
			t.Fatalf("Fix12 Item #5 regression: uninstall deleted meta.json.socket_path %s! err=%v", userNotes, err)
		}
		if got, err := os.ReadFile(layout.SocketPath); err != nil || string(got) != "not-a-socket\n" {
			t.Fatalf("Fix12 Item #5 regression: uninstall deleted non-socket regular file at %s! err=%v", layout.SocketPath, err)
		}
	})

	t.Run("fix12_item6_clean_stage_takes_locks_and_respects_active_op", func(t *testing.T) {
		home := t.TempDir()
		mid := randomHexMid(t)
		env := []string{"CORRALD_TEST_MACHINE_ID=" + mid}
		installedBin, layout := installAndServe(t, home, mid)

		const stageOp = "1206000000000001"
		stageDir := filepath.Join(layout.StateDir, "stage-"+stageOp)
		stageBin := filepath.Join(stageDir, "corrald")
		if err := os.MkdirAll(stageDir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(stageBin, []byte("in-progress-binary"), 0o600); err != nil {
			t.Fatal(err)
		}

		// Hold OpLock to simulate an active install op in progress.
		opLk, err := AcquireLock(layout.OpLockPath)
		if err != nil {
			t.Fatalf("AcquireLock(OpLock): %v", err)
		}
		out, code := runCLI(t, installedBin, home, env, "clean-stage", "--stage", stageDir, "--op", stageOp)
		_ = opLk.Release()
		if code == 0 || parseKV(out)["error"] != proto.ErrBusy {
			t.Fatalf("Fix12 Item #6 regression: clean-stage ignored held OpLock! code=%d\n%s", code, out)
		}
		if _, err := os.Stat(stageBin); err != nil {
			t.Fatalf("Fix12 Item #6 regression: clean-stage deleted in-progress stage while OpLock held: %v", err)
		}

		// Hold SharedLock to verify SharedLock -> OpLock lock chain.
		shLk, err := AcquireLock(layout.SharedLockPath)
		if err != nil {
			t.Fatalf("AcquireLock(SharedLock): %v", err)
		}
		out, code = runCLI(t, installedBin, home, env, "clean-stage", "--stage", stageDir, "--op", stageOp)
		_ = shLk.Release()
		if code == 0 || parseKV(out)["error"] != proto.ErrBusy {
			t.Fatalf("Fix12 Item #6 regression: clean-stage ignored held SharedLock! code=%d\n%s", code, out)
		}
		if _, err := os.Stat(stageBin); err != nil {
			t.Fatalf("Fix12 Item #6 regression: clean-stage deleted stage while SharedLock held: %v", err)
		}

		// Once unlocked, clean-stage succeeds cleanly.
		out, code = runCLI(t, installedBin, home, env, "clean-stage", "--stage", stageDir, "--op", stageOp)
		if code != 0 || parseKV(out)["result"] != "ok" {
			t.Fatalf("expected clean-stage ok after locks released, got code=%d\n%s", code, out)
		}
		if _, err := os.Lstat(stageDir); !os.IsNotExist(err) {
			t.Fatalf("expected stage dir %s removed, stat err=%v", stageDir, err)
		}
	})

	t.Run("fix12_item7_stage_and_manifest_cannot_widen_owned_names", func(t *testing.T) {
		home := t.TempDir()
		mid := randomHexMid(t)
		env := []string{"CORRALD_TEST_MACHINE_ID=" + mid}
		installedBin, layout := installAndServe(t, home, mid)

		// 1. clean-stage --op backups --stage <StateDir>/stage-backups must be rejected!
		bkpStage := filepath.Join(layout.StateDir, "stage-backups")
		bkpFile := filepath.Join(bkpStage, "corrald")
		if err := os.MkdirAll(bkpStage, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(bkpFile, []byte("user-corrald-backup\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		out, code := runCLI(t, installedBin, home, env, "clean-stage", "--stage", bkpStage, "--op", "backups")
		if code == 0 {
			t.Fatalf("Fix12 Item #7 regression: clean-stage accepted non-16hex stage-backups!\n%s", out)
		}
		if got, err := os.ReadFile(bkpFile); err != nil || string(got) != "user-corrald-backup\n" {
			t.Fatalf("Fix12 Item #7 regression: clean-stage deleted stage-backups/corrald! err=%v content=%q", err, string(got))
		}
		// Direct RemoveOwned(OwnedRemoveStageDir) must also reject stage-backups.
		if left := RemoveOwned(layout, OwnedRemoveStageDir, bkpStage, nil); len(left) == 0 {
			t.Fatalf("Fix12 Item #7 regression: RemoveOwned accepted non-16hex stage-backups!")
		}
		if got, err := os.ReadFile(bkpFile); err != nil || string(got) != "user-corrald-backup\n" {
			t.Fatalf("Fix12 Item #7 regression: RemoveOwned deleted stage-backups/corrald! err=%v", err)
		}
		_ = os.RemoveAll(bkpStage)

		// 2. Putting ops/notes.json in install.json's Files manifest MUST NOT allow deleting it!
		opsNotes := filepath.Join(layout.OpsDir, "notes.json")
		if err := os.WriteFile(opsNotes, []byte(`{"user":"keep"}`), 0o600); err != nil {
			t.Fatal(err)
		}
		instRec, err := ReadInstallRecord(layout.InstallJSON)
		if err != nil {
			t.Fatalf("ReadInstallRecord: %v", err)
		}
		instRec.Files = append(instRec.Files, opsNotes)
		if err := WriteInstallRecord(layout.InstallJSON, instRec); err != nil {
			t.Fatalf("WriteInstallRecord: %v", err)
		}
		out, code = runCLI(t, installedBin, home, env, "uninstall", "--op", "1207000000000001", "--only-if-sole-gateway", "gw-a")
		if code == 0 || parseKV(out)["result"] != "partial" {
			t.Fatalf("Fix12 Item #7 regression: expected partial preserving manifest-listed ops/notes.json, got code=%d\n%s", code, out)
		}
		if got, err := os.ReadFile(opsNotes); err != nil || string(got) != `{"user":"keep"}` {
			t.Fatalf("Fix12 Item #7 regression: manifest widened ops rule and deleted ops/notes.json! err=%v content=%q", err, string(got))
		}
	})

	t.Run("fix12_item8_selftest_and_rebind_sockets_cleaned_by_uninstall", func(t *testing.T) {
		// Use short /tmp dir so Unix socket sun_path stays well under 108 bytes.
		home, err := os.MkdirTemp("/tmp", "c12s-")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.RemoveAll(home) })
		mid := randomHexMid(t)
		env := []string{"CORRALD_TEST_MACHINE_ID=" + mid}
		installedBin, layout := installAndServe(t, home, mid)

		selftestSock := filepath.Join(layout.StateDir, ".selftest-12345.sock")
		rebindSock := filepath.Join(layout.StateDir, ".rebind-12345-a1b2c3.sock")
		for _, sp := range []string{selftestSock, rebindSock} {
			ln, lerr := net.ListenUnix("unix", &net.UnixAddr{Name: sp, Net: "unix"})
			if lerr != nil {
				t.Fatalf("ListenUnix(%s): %v", sp, lerr)
			}
			ln.SetUnlinkOnClose(false)
			_ = ln.Close()
		}

		out, code := runCLI(t, installedBin, home, env, "uninstall", "--op", "1208000000000001", "--only-if-sole-gateway", "gw-a")
		if code != 0 || parseKV(out)["result"] != "removed" {
			t.Fatalf("Fix12 Item #8 regression: leftover .selftest/.rebind sockets blocked uninstall! code=%d\n%s", code, out)
		}
		for _, p := range []string{selftestSock, rebindSock, layout.StateDir, layout.LibDir} {
			if _, err := os.Lstat(p); !os.IsNotExist(err) {
				t.Fatalf("expected %s removed, stat err=%v", p, err)
			}
		}
	})

	t.Run("fix12_item9_libdir_temp_files_enumerated_and_enotempty_reported", func(t *testing.T) {
		// Subcase A: interrupted atomic update leaves .corrald.tmp.* and .README.txt.tmp.*
		// directly in LibDir -> uninstall MUST enumerate and delete them AND remove LibDir.
		homeA := t.TempDir()
		midA := randomHexMid(t)
		envA := []string{"CORRALD_TEST_MACHINE_ID=" + midA}
		installedBinA, layoutA := installAndServe(t, homeA, midA)

		tmpBin := filepath.Join(layoutA.LibDir, ".corrald.tmp.12345.deadbeef")
		tmpReadme := filepath.Join(layoutA.LibDir, ".README.txt.tmp.12345.cafebabe")
		if err := os.WriteFile(tmpBin, []byte("tmp-bin"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(tmpReadme, []byte("tmp-readme"), 0o600); err != nil {
			t.Fatal(err)
		}
		outA, codeA := runCLI(t, installedBinA, homeA, envA, "uninstall", "--op", "1209000000000001", "--only-if-sole-gateway", "gw-a")
		if codeA != 0 || parseKV(outA)["result"] != "removed" {
			t.Fatalf("expected clean uninstall removing LibDir temp files, got code=%d\n%s", codeA, outA)
		}
		for _, p := range []string{tmpBin, tmpReadme, layoutA.LibDir} {
			if _, err := os.Lstat(p); !os.IsNotExist(err) {
				t.Fatalf("Fix12 Item #9 regression: %s left behind after uninstall reported removed! stat err=%v", p, err)
			}
		}

		// Subcase B: an unexpected regular file in LibDir causes LibDir rmdir ENOTEMPTY ->
		// MUST NOT silently report result=removed; must report result=partial with left=.
		homeB := t.TempDir()
		midB := randomHexMid(t)
		envB := []string{"CORRALD_TEST_MACHINE_ID=" + midB}
		installedBinB, layoutB := installAndServe(t, homeB, midB)
		extraLibFile := filepath.Join(layoutB.LibDir, ".notes.tmp.backup")
		if err := os.WriteFile(extraLibFile, []byte("keep-lib-file\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		outB, codeB := runCLI(t, installedBinB, homeB, envB, "uninstall", "--op", "1209000000000002", "--only-if-sole-gateway", "gw-a")
		if codeB == 0 || parseKV(outB)["result"] != "partial" {
			t.Fatalf("Fix12 Item #9 regression: LibDir ENOTEMPTY with leftover file reported removed! code=%d\n%s", codeB, outB)
		}
		if got, err := os.ReadFile(extraLibFile); err != nil || string(got) != "keep-lib-file\n" {
			t.Fatalf("expected unowned file %s preserved: err=%v content=%q", extraLibFile, err, string(got))
		}
		if !strings.Contains(outB, "left="+extraLibFile+"\t") {
			t.Fatalf("expected %s in left=:\n%s", extraLibFile, outB)
		}
	})

	t.Run("fix13_item4_reject_non_canonical_op_before_any_write", func(t *testing.T) {
		home := t.TempDir()
		mid := randomHexMid(t)
		env := []string{"CORRALD_TEST_MACHINE_ID=" + mid}
		uid := uint32(os.Getuid())
		layout, err := ResolveLayout(PathOptions{Home: home, UID: &uid, MachineID: mid})
		if err != nil {
			t.Fatalf("ResolveLayout: %v", err)
		}

		// 1. `install --op badop` MUST fail with exit=2 invalid_op BEFORE creating
		//    LibDir, install.json, or ops/badop.json.
		badStage := filepath.Join(layout.StateDir, "stage-badop")
		if err := os.MkdirAll(badStage, 0o700); err != nil {
			t.Fatal(err)
		}
		binBytes, err := os.ReadFile(bins.v1)
		if err != nil {
			t.Fatal(err)
		}
		badStageBin := filepath.Join(badStage, "corrald")
		if err := os.WriteFile(badStageBin, binBytes, 0o700); err != nil {
			t.Fatal(err)
		}
		out, code := runCLI(t, badStageBin, home, env, "install", "--stage", badStage, "--op", "badop", "--gateway", "gw-a", "--by", "alice")
		if code != 2 || parseKV(out)["error"] != "invalid_op" {
			t.Fatalf("Fix13 Item #4 regression: install did not reject non-canonical --op before writes! code=%d\n%s", code, out)
		}
		for _, p := range []string{layout.LibDir, layout.InstallJSON, filepath.Join(layout.OpsDir, "badop.json")} {
			if _, err := os.Lstat(p); !os.IsNotExist(err) {
				t.Fatalf("Fix13 Item #4 regression: install wrote %s despite invalid --op! stat err=%v", p, err)
			}
		}

		// 2. `uninstall --op badop` on an installed host MUST fail with exit=2 invalid_op
		//    before touching install.json, tombstone, or binary.
		home2 := t.TempDir()
		mid2 := randomHexMid(t)
		env2 := []string{"CORRALD_TEST_MACHINE_ID=" + mid2}
		installedBin2, layout2 := installAndServe(t, home2, mid2)
		out2, code2 := runCLI(t, installedBin2, home2, env2, "uninstall", "--op", "badop", "--only-if-sole-gateway", "gw-a")
		if code2 != 2 || parseKV(out2)["error"] != "invalid_op" {
			t.Fatalf("Fix13 Item #4 regression: uninstall accepted non-canonical --op! code=%d\n%s", code2, out2)
		}
		if _, err := os.Stat(layout2.InstallJSON); err != nil {
			t.Fatalf("Fix13 Item #4 regression: uninstall touched install.json despite invalid --op: %v", err)
		}
		if _, err := os.Lstat(layout2.TombstonePath); !os.IsNotExist(err) {
			t.Fatalf("Fix13 Item #4 regression: uninstall wrote tombstone despite invalid --op: %v", err)
		}
	})
}

// Regression test for Fix Round 13 Item #5 [P2]:
// Server exit cleanup (server.go:468) MUST remove layout.SocketPath ONLY via
// removeOwnedSocket (verifying it is a Unix socket), never unlinking a regular
// file that replaced the socket path.
func TestServerExitCleanupNeverDeletesNonSocketAtSocketPath(t *testing.T) {
	home, err := os.MkdirTemp("/tmp", "c13s-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	mid := randomHexMid(t)
	uid := uint32(os.Getuid())
	layout, err := ResolveLayout(PathOptions{Home: home, UID: &uid, MachineID: mid})
	if err != nil {
		t.Fatalf("ResolveLayout: %v", err)
	}
	if err := layout.EnsureStateDir(); err != nil {
		t.Fatal(err)
	}
	lifeLock, err := AcquireLock(layout.LockPath)
	if err != nil {
		t.Fatalf("AcquireLock: %v", err)
	}
	defer lifeLock.Release()

	ready := make(chan struct{})
	srv, err := NewServer(ServerConfig{
		Layout:      layout,
		Lock:        lifeLock,
		Build:       proto.BuildInfo{Version: "0.1.0", Release: 100},
		IdleTimeout: time.Minute,
		OnReady:     func() { close(ready) },
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- srv.Serve(ctx)
	}()
	select {
	case <-ready:
	case err := <-done:
		t.Fatalf("Server.Run exited before ready: %v", err)
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("timed out waiting for server ready")
	}

	// Replace the socket path with a regular non-socket file before daemon exits.
	if err := os.Remove(layout.SocketPath); err != nil {
		cancel()
		t.Fatalf("remove socket: %v", err)
	}
	if err := os.WriteFile(layout.SocketPath, []byte("user-file-not-a-socket\n"), 0o600); err != nil {
		cancel()
		t.Fatalf("write non-socket file: %v", err)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Server.Run error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for server exit")
	}

	got, err := os.ReadFile(layout.SocketPath)
	if err != nil || string(got) != "user-file-not-a-socket\n" {
		t.Fatalf("Fix13 Item #5 regression: server exit cleanup deleted non-socket regular file at %s! err=%v content=%q", layout.SocketPath, err, string(got))
	}
}

// Regression test for Fix Round 6 F2 [P3]:
// Verify selftest and clean daemon stop remove their /tmp/corrald-<uid>-<hash>
// fallback socket directory so tests and long-HOME hosts do not leak empty dirs.
func TestCleanStopAndSelftestRemoveTmpSocketDir(t *testing.T) {
	bins := getTestBinaries(t)
	// Ensure a long HOME path so layout.SocketUsesTmpDir is guaranteed true.
	home := filepath.Join(t.TempDir(), "nested-long-home-dir-for-tmp-socket-fallback-test-0123456789abcdef")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	mid := randomHexMid(t)
	env := []string{"CORRALD_TEST_MACHINE_ID=" + mid}
	uid := uint32(os.Getuid())
	layout, err := ResolveLayout(PathOptions{Home: home, UID: &uid, MachineID: mid})
	if err != nil {
		t.Fatalf("ResolveLayout: %v", err)
	}
	if !layout.SocketUsesTmpDir {
		t.Fatalf("expected SocketUsesTmpDir=true for long home %q", home)
	}
	t.Cleanup(func() {
		_, _, _ = StopDaemon(layout)
		_ = os.RemoveAll(layout.SocketDir)
	})

	// 1. selftest must clean up its own tmp socket dir before exiting when unlocked.
	if out, code := runCLI(t, bins.v1, home, env, "selftest"); code != 0 {
		t.Fatalf("selftest failed: %s", out)
	}
	if _, err := os.Lstat(layout.SocketDir); !os.IsNotExist(err) {
		t.Fatalf("F2 regression: selftest leaked tmp socket dir %s (lstat err=%v)", layout.SocketDir, err)
	}

	// 1b. Fix7 Item #7 [P2]: when corrald.lock is ALREADY held (e.g. a daemon
	// passed EnsureSocketDir and is about to bind corrald.sock), selftest MUST
	// NOT remove the tmp socket dir!
	if err := layout.EnsureSocketDir(); err != nil {
		t.Fatalf("EnsureSocketDir: %v", err)
	}
	heldLock, err := AcquireLock(layout.LockPath)
	if err != nil {
		t.Fatalf("AcquireLock: %v", err)
	}
	if out, code := runCLI(t, bins.v1, home, env, "selftest"); code != 0 {
		_ = heldLock.Release()
		t.Fatalf("selftest while lock held failed: %s", out)
	}
	if _, err := os.Stat(layout.SocketDir); err != nil {
		_ = heldLock.Release()
		t.Fatalf("Fix7 Item #7 regression: selftest removed tmp socket dir %s while corrald.lock was held: %v", layout.SocketDir, err)
	}
	_ = heldLock.Release()

	// 2. serve creates the tmp socket dir; clean stop removes it.
	stage := stageBinary(t, home, mid, "2cb05f3775aecce4", bins.v1)
	if out, code := runCLI(t, filepath.Join(stage, "corrald"), home, env,
		"install", "--stage", stage, "--op", "2cb05f3775aecce4", "--gateway", "gw-f2", "--by", "alice"); code != 0 {
		t.Fatalf("install failed: %s", out)
	}
	installedBin := filepath.Join(home, ".local", "lib", "corral", "corrald")
	if out, code := runCLI(t, installedBin, home, env, "serve", "--gateway", "gw-f2"); code != 0 {
		t.Fatalf("serve failed: %s", out)
	}
	if _, err := os.Stat(layout.SocketDir); err != nil {
		t.Fatalf("expected tmp socket dir %s to exist while serving: %v", layout.SocketDir, err)
	}
	if out, code := runCLI(t, installedBin, home, env, "stop"); code != 0 {
		t.Fatalf("stop failed: %s", out)
	}
	if _, err := os.Lstat(layout.SocketDir); !os.IsNotExist(err) {
		t.Fatalf("F2 regression: clean daemon stop leaked tmp socket dir %s (lstat err=%v)", layout.SocketDir, err)
	}
}
