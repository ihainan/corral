//go:build linux

package daemon

import (
	"context"
	"crypto/sha256"
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

func TestResolveMachineIDFallbackChain(t *testing.T) {
	dir := t.TempDir()
	etcFile := filepath.Join(dir, "etc-machine-id")
	dbusFile := filepath.Join(dir, "dbus-machine-id")

	// 1. Primary /etc/machine-id present.
	if err := os.WriteFile(etcFile, []byte("AABBCCDDEEFF00112233445566778899\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	id, src, err := ResolveMachineID([]string{etcFile, dbusFile}, func() (string, error) { return "box1", nil })
	if err != nil || id != "aabbccddeeff" || src != etcFile {
		t.Fatalf("primary machine-id: got (%q, %q, %v)", id, src, err)
	}

	// 2. Missing primary -> falls back to dbus machine-id.
	_ = os.Remove(etcFile)
	if err := os.WriteFile(dbusFile, []byte("11223344556677889900aabbccddeeff\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	id, src, err = ResolveMachineID([]string{etcFile, dbusFile}, func() (string, error) { return "box1", nil })
	if err != nil || id != "112233445566" || src != dbusFile {
		t.Fatalf("dbus machine-id: got (%q, %q, %v)", id, src, err)
	}

	// 3. Both missing -> falls back to sha256(hostname)[:12].
	_ = os.Remove(dbusFile)
	id, src, err = ResolveMachineID([]string{etcFile, dbusFile}, func() (string, error) { return "cluster-node-07", nil })
	sum := sha256.Sum256([]byte("cluster-node-07"))
	want := hex.EncodeToString(sum[:6])
	if err != nil || id != want || src != MachineIDSourceHostname {
		t.Fatalf("hostname fallback: got (%q, %q, %v), want (%q, %q)", id, src, err, want, MachineIDSourceHostname)
	}
}

func TestPathResolutionAndTmpFallback(t *testing.T) {
	uid := uint32(12345)
	// Short home stays under ~/.local/state/corral/corrald/<mid>/corrald.sock.
	shortLayout, err := ResolveLayout(PathOptions{
		Home:      "/home/alice",
		UID:       &uid,
		MachineID: "aabbccddeeff",
	})
	if err != nil {
		t.Fatal(err)
	}
	if shortLayout.SocketUsesTmpDir {
		t.Fatalf("short home unexpectedly used /tmp: %s", shortLayout.SocketPath)
	}
	if len(shortLayout.SocketPath) > MaxSocketPath {
		t.Fatalf("short socket path exceeded %d bytes: %s", MaxSocketPath, shortLayout.SocketPath)
	}

	// Long home (> 99 bytes for state socket) falls back to /tmp/corrald-<uid>-<sha256(mid+home)[:8]>/corrald.sock.
	longHome := "/home/" + strings.Repeat("verylongusername", 5)
	tmpBase := t.TempDir()
	longLayout, err := ResolveLayout(PathOptions{
		Home:      longHome,
		UID:       &uid,
		MachineID: "aabbccddeeff",
		TmpBase:   tmpBase,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !longLayout.SocketUsesTmpDir {
		t.Fatalf("long home did not fall back to tmp: %s (len=%d)", longLayout.SocketPath, len(longLayout.SocketPath))
	}
	sum := sha256.Sum256([]byte("aabbccddeeff" + longHome))
	wantPrefix := filepath.Join(tmpBase, "corrald-12345-"+hex.EncodeToString(sum[:4]))
	if longLayout.SocketDir != wantPrefix {
		t.Fatalf("tmp socket dir: got %q, want %q", longLayout.SocketDir, wantPrefix)
	}
	if len(filepath.Join("/tmp", filepath.Base(longLayout.SocketDir), "corrald.sock")) > MaxSocketPath {
		t.Fatalf("fallback socket path too long")
	}
}

func TestEnsureSecureDirRejectsHijackedTmpDir(t *testing.T) {
	base := t.TempDir()
	myUID := uint32(os.Geteuid())

	// 1. Owned by another user (simulated by expecting a different UID).
	otherDir := filepath.Join(base, "owned-by-other")
	if err := os.Mkdir(otherDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := EnsureSecureDir(otherDir, myUID+1); err == nil {
		t.Fatal("expected EnsureSecureDir to reject directory owned by different UID")
	}

	// 2. Symlink at socket directory path.
	target := filepath.Join(base, "real-target")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	sym := filepath.Join(base, "symlink-dir")
	if err := os.Symlink(target, sym); err != nil {
		t.Fatal(err)
	}
	if err := EnsureSecureDir(sym, myUID); err == nil {
		t.Fatal("expected EnsureSecureDir to reject symlink")
	}

	// 3. Regular file at socket directory path.
	reg := filepath.Join(base, "regular-file")
	if err := os.WriteFile(reg, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := EnsureSecureDir(reg, myUID); err == nil {
		t.Fatal("expected EnsureSecureDir to reject regular file")
	}

	// 4. Valid directory owned by us with loose mode is tightened to 0700.
	loose := filepath.Join(base, "loose-dir")
	if err := os.Mkdir(loose, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := EnsureSecureDir(loose, myUID); err != nil {
		t.Fatalf("EnsureSecureDir valid dir: %v", err)
	}
	info, err := os.Lstat(loose)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Fatalf("mode after EnsureSecureDir: got %04o, want 0700", info.Mode().Perm())
	}
}

func TestFileLockExclusive(t *testing.T) {
	lockPath := filepath.Join(t.TempDir(), "corrald.lock")
	l1, err := AcquireLock(lockPath)
	if err != nil {
		t.Fatalf("first lock failed: %v", err)
	}
	if _, err := AcquireLock(lockPath); !errors.Is(err, ErrLocked) {
		t.Fatalf("second concurrent lock: got %v, want ErrLocked", err)
	}
	if err := l1.Release(); err != nil {
		t.Fatalf("release failed: %v", err)
	}
	l2, err := AcquireLock(lockPath)
	if err != nil {
		t.Fatalf("lock after release failed: %v", err)
	}
	_ = l2.Release()
}

func TestParseProcStatHandlesParensAndZombie(t *testing.T) {
	// Normal process with spaces and nested parens in comm.
	line := "1234 (my (weird) proc) S 1 1234 1234 0 -1 4194304 10 0 0 0 1 2 0 0 20 0 1 0 987654321 1000 50\n"
	st, alive, err := parseProcStat(line)
	if err != nil || !alive || st != "987654321" {
		t.Fatalf("parseProcStat normal: got (%q, %v, %v)", st, alive, err)
	}

	// Zombie process must report alive=false.
	zombie := "1234 (corrald) Z 1 1234 1234 0 -1 4194304 10 0 0 0 1 2 0 0 20 0 1 0 987654321 1000 50\n"
	st, alive, err = parseProcStat(zombie)
	if err != nil || alive || st != "987654321" {
		t.Fatalf("parseProcStat zombie: got (%q, %v, %v)", st, alive, err)
	}

	// Verify live self start time matches ProcessMatches.
	selfStart, selfAlive, err := ProcStartTime(os.Getpid())
	if err != nil || !selfAlive || selfStart == "" {
		t.Fatalf("ProcStartTime(self): (%q, %v, %v)", selfStart, selfAlive, err)
	}
	if !ProcessMatches(os.Getpid(), selfStart) {
		t.Fatal("ProcessMatches(self) returned false")
	}
	if ProcessMatches(os.Getpid(), selfStart+"999") {
		t.Fatal("ProcessMatches matched wrong start time")
	}
}

func TestSO_PEERCREDRejectsDifferentUID(t *testing.T) {
	// Unit test on the UID check helper.
	if err := VerifyPeerUID(1000, 1000); err != nil {
		t.Fatalf("matching UID rejected: %v", err)
	}
	if err := VerifyPeerUID(1001, 1000); err == nil {
		t.Fatal("mismatched UID accepted")
	}

	// Live Unix socket test: server expects euid+1, so our real connection
	// (which kernel reports with SO_PEERCRED uid == euid) must be immediately
	// closed by the server without answering any RPC.
	home := t.TempDir()
	wrongUID := uint32(os.Geteuid()) + 1
	layout, err := ResolveLayout(PathOptions{
		Home:      home,
		MachineID: "1234567890ab",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := layout.EnsureStateDir(); err != nil {
		t.Fatal(err)
	}
	if layout.SocketUsesTmpDir {
		t.Cleanup(func() { _ = os.RemoveAll(layout.SocketDir) })
	}

	ready := make(chan struct{})
	srv, err := NewServer(ServerConfig{
		Layout:      layout,
		Build:       proto.BuildInfo{Version: "0.1.0", Release: 100},
		ExpectedUID: &wrongUID,
		OnReady:     func() { close(ready) },
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx) }()
	select {
	case <-ready:
	case err := <-done:
		t.Fatalf("Serve exited before ready: %v", err)
	}

	conn, err := net.DialTimeout("unix", layout.SocketPath, 2*time.Second)
	if err != nil {
		t.Fatalf("dial unix: %v", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
	_ = proto.WriteJSON(conn, proto.Request{ID: 1, Method: proto.MethodDaemonHealth})
	var resp proto.Response
	if err := proto.ReadJSON(conn, &resp); err == nil {
		t.Fatalf("expected peer with mismatched UID to be rejected, but got response %+v", resp)
	}

	cancel()
	<-done
}

func TestRotatingWriterCapsAndKeepsSingleBackup(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "corrald.log")
	w, err := OpenRotatingWriter(logPath, 100)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	// Write 3 chunks of 60 bytes -> should rotate twice, keeping only .log and .log.1.
	chunk := strings.Repeat("x", 59) + "\n"
	for i := 0; i < 3; i++ {
		if _, err := w.Write([]byte(chunk)); err != nil {
			t.Fatal(err)
		}
	}
	_ = w.Close()

	cur, err := os.Stat(logPath)
	if err != nil {
		t.Fatalf("missing corrald.log: %v", err)
	}
	bak, err := os.Stat(logPath + ".1")
	if err != nil {
		t.Fatalf("missing corrald.log.1: %v", err)
	}
	if cur.Size() > 100 || bak.Size() > 100 {
		t.Fatalf("log size exceeded cap: cur=%d bak=%d", cur.Size(), bak.Size())
	}
	if _, err := os.Stat(logPath + ".2"); !os.IsNotExist(err) {
		t.Fatal("unexpected second backup corrald.log.2")
	}
}

func TestOpTableIdempotencyAndConcurrency(t *testing.T) {
	tbl := NewOpTable(10, time.Hour)

	// Empty op_id rejected.
	if _, rpcErr := tbl.Execute("", func() (json.RawMessage, *proto.RPCError) {
		t.Fatal("should not run for empty op_id")
		return nil, nil
	}); rpcErr == nil || rpcErr.Code != proto.ErrInvalidParams {
		t.Fatalf("empty op_id: got %v, want invalid_params", rpcErr)
	}

	// Concurrent duplicate gets retryable in_progress while first executes once.
	started := make(chan struct{})
	release := make(chan struct{})
	var runs atomic.Int32

	var wg sync.WaitGroup
	wg.Add(1)
	var firstRes json.RawMessage
	var firstErr *proto.RPCError
	go func() {
		defer wg.Done()
		firstRes, firstErr = tbl.Execute("op-1", func() (json.RawMessage, *proto.RPCError) {
			runs.Add(1)
			close(started)
			<-release
			return json.RawMessage(`{"sent":true}`), nil
		})
	}()

	<-started
	_, dupErr := tbl.Execute("op-1", func() (json.RawMessage, *proto.RPCError) {
		runs.Add(1)
		return nil, nil
	})
	if dupErr == nil || dupErr.Code != proto.ErrInProgress || !dupErr.Retryable {
		t.Fatalf("concurrent duplicate: got %+v, want retryable in_progress", dupErr)
	}

	close(release)
	wg.Wait()

	if firstErr != nil || string(firstRes) != `{"sent":true}` {
		t.Fatalf("first call result: %s, %v", firstRes, firstErr)
	}

	// Completed duplicate returns cached first result without re-running fn.
	againRes, againErr := tbl.Execute("op-1", func() (json.RawMessage, *proto.RPCError) {
		runs.Add(1)
		return json.RawMessage(`{"sent":false}`), nil
	})
	if againErr != nil || string(againRes) != `{"sent":true}` {
		t.Fatalf("cached replay: got %s, %v", againRes, againErr)
	}
	if runs.Load() != 1 {
		t.Fatalf("fn executed %d times, want 1", runs.Load())
	}

	// Snapshot/Restore preserves completed ops across handoff.
	snap := tbl.Snapshot()
	tbl2 := NewOpTable(10, time.Hour)
	tbl2.Restore(snap)
	restoredRes, restoredErr := tbl2.Execute("op-1", func() (json.RawMessage, *proto.RPCError) {
		t.Fatal("restored op must not re-execute")
		return nil, nil
	})
	if restoredErr != nil || string(restoredRes) != `{"sent":true}` {
		t.Fatalf("restored replay: got %s, %v", restoredRes, restoredErr)
	}
}

func TestEventBusRingBufferAndResumeBoundaries(t *testing.T) {
	bus := NewEventBus(5)
	epoch := bus.Epoch()

	for i := 1; i <= 8; i++ {
		ev, err := bus.Emit("test.ev", map[string]int{"i": i})
		if err != nil {
			t.Fatal(err)
		}
		if ev.Seq != uint64(i) || ev.Epoch != epoch {
			t.Fatalf("event %d: got seq=%d epoch=%q", i, ev.Seq, ev.Epoch)
		}
	}

	// Ring holds seq 4..8. Resuming from lastSeq=3 must succeed and replay 4,5,6,7,8.
	var got []proto.Event
	n, cur, rpcErr := bus.Resume(epoch, 3, nil, func(replayed []proto.Event, currentSeq uint64) {
		got = replayed
	})
	if rpcErr != nil || n != 5 || cur != 8 || len(got) != 5 || got[0].Seq != 4 || got[4].Seq != 8 {
		t.Fatalf("resume(3): n=%d cur=%d err=%v got=%v", n, cur, rpcErr, got)
	}

	// Resuming from lastSeq=2 must fail with resync_required (seq 3 was evicted).
	if _, _, rpcErr := bus.Resume(epoch, 2, nil, nil); rpcErr == nil || rpcErr.Code != proto.ErrResyncRequired {
		t.Fatalf("resume(2): got %v, want resync_required", rpcErr)
	}

	// Resuming with wrong epoch must fail with resync_required.
	if _, _, rpcErr := bus.Resume("wrong-epoch", 8, nil, nil); rpcErr == nil || rpcErr.Code != proto.ErrResyncRequired {
		t.Fatalf("resume wrong epoch: got %v, want resync_required", rpcErr)
	}

	// Resuming from current seq=8 replays 0 events.
	n, cur, rpcErr = bus.Resume(epoch, 8, nil, nil)
	if rpcErr != nil || n != 0 || cur != 8 {
		t.Fatalf("resume(8): n=%d cur=%d err=%v", n, cur, rpcErr)
	}
}

func TestValidateHandoffBinaryAndPruneOldVersions(t *testing.T) {
	home := t.TempDir()
	layout, err := ResolveLayout(PathOptions{
		Home:      home,
		MachineID: "abcdef123456",
		TmpBase:   t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(layout.LibDir, 0o755); err != nil {
		t.Fatal(err)
	}

	verDir, _ := layout.VersionDir("0.2.0", 200)
	if err := os.MkdirAll(verDir, 0o700); err != nil {
		t.Fatal(err)
	}
	binPath := filepath.Join(verDir, "corrald")
	content := []byte("#!/bin/sh\n")
	if err := os.WriteFile(binPath, content, 0o700); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(content)
	digest := hex.EncodeToString(sum[:])

	// Valid binary succeeds.
	if _, err := ValidateHandoffBinary(layout, binPath, digest, layout.UID); err != nil {
		t.Fatalf("valid binary rejected: %v", err)
	}

	// Wrong sha256 rejected.
	if _, err := ValidateHandoffBinary(layout, binPath, strings.Repeat("0", 64), layout.UID); err == nil {
		t.Fatal("wrong sha256 accepted")
	}

	// Wrong UID rejected.
	if _, err := ValidateHandoffBinary(layout, binPath, digest, layout.UID+1); err == nil {
		t.Fatal("wrong UID accepted")
	}

	// Wrong permissions (0755) rejected.
	if err := os.Chmod(binPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := ValidateHandoffBinary(layout, binPath, digest, layout.UID); err == nil {
		t.Fatal("0755 binary accepted")
	}
	_ = os.Chmod(binPath, 0o700)

	// Outside LibDir rejected.
	outside := filepath.Join(t.TempDir(), "corrald")
	_ = os.WriteFile(outside, content, 0o700)
	if _, err := ValidateHandoffBinary(layout, outside, digest, layout.UID); err == nil {
		t.Fatal("binary outside LibDir accepted")
	}

	// Symlink rejected.
	sym := filepath.Join(verDir, "link")
	_ = os.Symlink(binPath, sym)
	if _, err := ValidateHandoffBinary(layout, sym, digest, layout.UID); err == nil {
		t.Fatal("symlink binary accepted")
	}

	// PruneOldVersions keeps current (300) + previous (200), removes older (100, 50), preserves code-server.
	for _, d := range []string{"corrald-0.0.5-50", "corrald-0.1.0-100", "corrald-0.3.0-300", "code-server-4.90.0"} {
		if err := os.MkdirAll(filepath.Join(layout.LibDir, d), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	removed, err := PruneOldVersions(layout, "0.3.0", 300)
	if err != nil {
		t.Fatalf("PruneOldVersions: %v", err)
	}
	if len(removed) != 2 {
		t.Fatalf("expected 2 pruned versions, got %v", removed)
	}
	for _, mustExist := range []string{"corrald-0.3.0-300", "corrald-0.2.0-200", "code-server-4.90.0"} {
		if _, err := os.Stat(filepath.Join(layout.LibDir, mustExist)); err != nil {
			t.Fatalf("expected %s to be preserved: %v", mustExist, err)
		}
	}
	for _, mustBeGone := range []string{"corrald-0.1.0-100", "corrald-0.0.5-50"} {
		if _, err := os.Stat(filepath.Join(layout.LibDir, mustBeGone)); !os.IsNotExist(err) {
			t.Fatalf("expected %s to be pruned", mustBeGone)
		}
	}
}

func TestStopDaemonRefusesMismatchedStartTime(t *testing.T) {
	layout, err := ResolveLayout(PathOptions{
		Home:      t.TempDir(),
		MachineID: "fedcba987654",
		TmpBase:   t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := layout.EnsureStateDir(); err != nil {
		t.Fatal(err)
	}
	// Point meta.json at our own test PID with a deliberately wrong start time.
	// StopDaemon must NOT signal our process; it should treat the PID as reused.
	if err := WriteMetaRecord(layout.MetaJSON, &MetaRecord{
		PID:       os.Getpid(),
		StartTime: "0",
	}); err != nil {
		t.Fatal(err)
	}
	stopped, pid, err := StopDaemon(layout)
	if err != nil || stopped || pid != 0 {
		t.Fatalf("StopDaemon mismatched start time: stopped=%v pid=%d err=%v", stopped, pid, err)
	}
}
