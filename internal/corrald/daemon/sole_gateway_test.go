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
		stage := stageBinary(t, home, mid, "opSole1", bins.v1)
		if out, code := runCLI(t, filepath.Join(stage, "corrald"), home, env,
			"install", "--stage", stage, "--op", "opSole1", "--gateway", "gw-a", "--by", "alice"); code != 0 {
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

		out, code := runCLI(t, installedBin, home, env, "uninstall", "--op", "opRm1", "--only-if-sole-gateway", "gw-a")
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

		out, code := runCLI(t, installedBin, home, env, "uninstall", "--op", "opRm2", "--only-if-sole-gateway", "gw-a")
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

		out, code := runCLI(t, installedBin, home, env, "uninstall", "--op", "opRm3", "--only-if-sole-gateway", "gw-a")
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
		stage := stageBinary(t, home, mid, "opOld1", bins.v1)
		if out, code := runCLI(t, filepath.Join(stage, "corrald"), home, env,
			"install", "--stage", stage, "--op", "opOld1", "--gateway", "gw-a", "--by", "alice"); code != 0 {
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

		out, code := runCLI(t, installedBin, home, env, "uninstall", "--op", "opRmOld", "--only-if-sole-gateway", "gw-a")
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
			out, code := runCLI(t, installedBin, home, env, "uninstall", "--op", "opRmLock", "--only-if-sole-gateway", "gw-a")
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
		out, code := runCLI(t, installedBin, home, env, "uninstall", "--op", "opTombFail", "--only-if-sole-gateway", "gw-a")
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
		out, code = runCLI(t, installedBin, home, env, "uninstall", "--op", "opTombRetry", "--only-if-sole-gateway", "gw-a")
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

		out, code := runCLI(t, installedBin, home, env, "uninstall", "--op", "opPart1", "--only-if-sole-gateway", "gw-a")
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
		out, code = runCLI(t, retryBin, home, env, "uninstall", "--op", "opPartRetry", "--only-if-sole-gateway", "gw-a")
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
		stage := stageBinary(t, home, mid, "opF1a", bins.v1)
		if out, code := runCLI(t, filepath.Join(stage, "corrald"), home, env,
			"install", "--stage", stage, "--op", "opF1a", "--gateway", "gw-a", "--by", "alice"); code != 0 {
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

		out, code := runCLI(t, installedBin, home, env, "uninstall", "--op", "opRmF1a", "--only-if-sole-gateway", "gw-a")
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
		stage := stageBinary(t, home, mid, "opF1b", bins.v1)
		if out, code := runCLI(t, filepath.Join(stage, "corrald"), home, env,
			"install", "--stage", stage, "--op", "opF1b", "--gateway", "gw-a", "--by", "alice"); code != 0 {
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

		out, code := runCLI(t, installedBin, home, env, "uninstall", "--op", "opRmF1b", "--only-if-sole-gateway", "gw-a")
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
		stage := stageBinary(t, home, mid, "opF1c", bins.v1)
		if out, code := runCLI(t, filepath.Join(stage, "corrald"), home, env,
			"install", "--stage", stage, "--op", "opF1c", "--gateway", "gw-a", "--by", "alice"); code != 0 {
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

		out, code := runCLI(t, installedBin, home, env, "uninstall", "--op", "opRmF1c", "--only-if-sole-gateway", "gw-a")
		if code != 0 || parseKV(out)["result"] != "removed" {
			t.Fatalf("expected uninstall to succeed after daemon released lock shortly after socket close, got code=%d\n%s", code, out)
		}
	})

	t.Run("f1_unreachable_live_daemon_holding_lock_still_fails_closed", func(t *testing.T) {
		home := t.TempDir()
		mid := randomHexMid(t)
		env := []string{"CORRALD_TEST_MACHINE_ID=" + mid}
		stage := stageBinary(t, home, mid, "opF1d", bins.v1)
		if out, code := runCLI(t, filepath.Join(stage, "corrald"), home, env,
			"install", "--stage", stage, "--op", "opF1d", "--gateway", "gw-a", "--by", "alice"); code != 0 {
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

		out, code := runCLI(t, installedBin, home, env, "uninstall", "--op", "opRmF1d", "--only-if-sole-gateway", "gw-a")
		if code == 0 || parseKV(out)["error"] != "daemon_unreachable" {
			t.Fatalf("expected error=daemon_unreachable when lock remains held with unreachable socket, got code=%d\n%s", code, out)
		}
		if _, err := os.Stat(installedBin); err != nil {
			t.Fatalf("binary must be preserved when daemon is unreachable: %v", err)
		}
	})
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

	// 1. selftest must clean up its own tmp socket dir before exiting.
	if out, code := runCLI(t, bins.v1, home, env, "selftest"); code != 0 {
		t.Fatalf("selftest failed: %s", out)
	}
	if _, err := os.Lstat(layout.SocketDir); !os.IsNotExist(err) {
		t.Fatalf("F2 regression: selftest leaked tmp socket dir %s (lstat err=%v)", layout.SocketDir, err)
	}

	// 2. serve creates the tmp socket dir; clean stop removes it.
	stage := stageBinary(t, home, mid, "opF2", bins.v1)
	if out, code := runCLI(t, filepath.Join(stage, "corrald"), home, env,
		"install", "--stage", stage, "--op", "opF2", "--gateway", "gw-f2", "--by", "alice"); code != 0 {
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
