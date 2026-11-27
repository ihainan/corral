//go:build linux

package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
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

	otherDir := filepath.Join(base, "owned-by-other")
	if err := os.Mkdir(otherDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := EnsureSecureDir(otherDir, myUID+1); err == nil {
		t.Fatal("expected EnsureSecureDir to reject directory owned by different UID")
	}

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

	reg := filepath.Join(base, "regular-file")
	if err := os.WriteFile(reg, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := EnsureSecureDir(reg, myUID); err == nil {
		t.Fatal("expected EnsureSecureDir to reject regular file")
	}

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
	line := "1234 (my (weird) proc) S 1 1234 1234 0 -1 4194304 10 0 0 0 1 2 0 0 20 0 1 0 987654321 1000 50\n"
	st, alive, err := parseProcStat(line)
	if err != nil || !alive || st != "987654321" {
		t.Fatalf("parseProcStat normal: got (%q, %v, %v)", st, alive, err)
	}

	zombie := "1234 (corrald) Z 1 1234 1234 0 -1 4194304 10 0 0 0 1 2 0 0 20 0 1 0 987654321 1000 50\n"
	st, alive, err = parseProcStat(zombie)
	if err != nil || alive || st != "987654321" {
		t.Fatalf("parseProcStat zombie: got (%q, %v, %v)", st, alive, err)
	}

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
	if err := VerifyPeerUID(1000, 1000); err != nil {
		t.Fatalf("matching UID rejected: %v", err)
	}
	if err := VerifyPeerUID(1001, 1000); err == nil {
		t.Fatal("mismatched UID accepted")
	}

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

func TestRotatingWriterCapsAndRejectsSymlink(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "corrald.log")
	w, err := OpenRotatingWriter(logPath, 100)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

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

	// Item 10: Symlink at corrald.log pointing outside MUST be refused without modifying target.
	outsideDir := t.TempDir()
	outsideFile := filepath.Join(outsideDir, "victim.txt")
	if err := os.WriteFile(outsideFile, []byte("untouched"), 0o600); err != nil {
		t.Fatal(err)
	}
	symLog := filepath.Join(t.TempDir(), "corrald.log")
	if err := os.Symlink(outsideFile, symLog); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenRotatingWriter(symLog, 100); err == nil {
		t.Fatal("OpenRotatingWriter followed symlink")
	}
	got, _ := os.ReadFile(outsideFile)
	if string(got) != "untouched" {
		t.Fatalf("symlink target was modified: %q", got)
	}
}

func TestOpTableIdempotencyInFlightHandoffAndExpiry(t *testing.T) {
	now := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
	tbl := NewOpTable(3, 30*time.Minute)
	tbl.nowFn = func() time.Time { return now }

	// Empty or malformed op_id rejected with invalid_params.
	for _, bad := range []string{"", "not-timestamped", "abc-123456"} {
		if _, rpcErr := tbl.Execute(bad, func() (json.RawMessage, *proto.RPCError) {
			t.Fatalf("should not run for invalid op_id %q", bad)
			return nil, nil
		}); rpcErr == nil || rpcErr.Code != proto.ErrInvalidParams {
			t.Fatalf("invalid op_id %q: got %v, want invalid_params", bad, rpcErr)
		}
	}

	// Item 16: op_id older than dedup window rejected with op_expired.
	oldID := proto.NewOpID(now.Add(-31 * time.Minute))
	if _, rpcErr := tbl.Execute(oldID, func() (json.RawMessage, *proto.RPCError) {
		t.Fatal("expired op_id must not execute")
		return nil, nil
	}); rpcErr == nil || rpcErr.Code != proto.ErrOpExpired {
		t.Fatalf("expired op_id: got %v, want op_expired", rpcErr)
	}

	// Concurrent duplicate gets retryable in_progress; snapshot while in-flight
	// hands off state=unknown so v2 refuses to re-execute (Item 1).
	op1 := proto.NewOpID(now)
	started := make(chan struct{})
	release := make(chan struct{})
	var runs atomic.Int32

	var wg sync.WaitGroup
	wg.Add(1)
	var firstRes json.RawMessage
	var firstErr *proto.RPCError
	go func() {
		defer wg.Done()
		firstRes, firstErr = tbl.Execute(op1, func() (json.RawMessage, *proto.RPCError) {
			runs.Add(1)
			close(started)
			<-release
			return json.RawMessage(`{"sent":true}`), nil
		})
	}()

	<-started
	_, dupErr := tbl.Execute(op1, func() (json.RawMessage, *proto.RPCError) {
		runs.Add(1)
		return nil, nil
	})
	if dupErr == nil || dupErr.Code != proto.ErrInProgress || !dupErr.Retryable {
		t.Fatalf("concurrent duplicate: got %+v, want retryable in_progress", dupErr)
	}

	// Take handoff snapshot WHILE op1 is still in progress!
	inFlightSnap := tbl.Snapshot()
	tblV2 := NewOpTable(3, 30*time.Minute)
	tblV2.nowFn = func() time.Time { return now }
	tblV2.Restore(inFlightSnap)

	_, v2Err := tblV2.Execute(op1, func() (json.RawMessage, *proto.RPCError) {
		t.Fatal("v2 must NEVER re-execute an op_id that was in-flight during handoff")
		return nil, nil
	})
	if v2Err == nil || v2Err.Code != proto.ErrOpUnknown || v2Err.Retryable {
		t.Fatalf("v2 retry of in-flight op_id: got %+v, want non-retryable op_unknown", v2Err)
	}

	close(release)
	wg.Wait()

	if firstErr != nil || string(firstRes) != `{"sent":true}` {
		t.Fatalf("first call result: %s, %v", firstRes, firstErr)
	}
	if runs.Load() != 1 {
		t.Fatalf("fn executed %d times, want 1", runs.Load())
	}

	// Completed duplicate returns cached result.
	againRes, againErr := tbl.Execute(op1, func() (json.RawMessage, *proto.RPCError) {
		t.Fatal("completed op must not re-execute")
		return nil, nil
	})
	if againErr != nil || string(againRes) != `{"sent":true}` {
		t.Fatalf("cached replay: got %s, %v", againRes, againErr)
	}

	// Item 16: Capacity eviction advances watermark so evicted op_id is rejected
	// with op_expired instead of re-executing!
	for i := 1; i <= 4; i++ {
		id := proto.NewOpID(now.Add(time.Duration(i) * time.Second))
		if _, err := tbl.Execute(id, func() (json.RawMessage, *proto.RPCError) {
			return json.RawMessage(`{"ok":true}`), nil
		}); err != nil {
			t.Fatalf("op %d: %v", i, err)
		}
	}
	// Capacity is 3, so op1 (at now) was evicted; retrying op1 MUST return op_expired!
	if _, rpcErr := tbl.Execute(op1, func() (json.RawMessage, *proto.RPCError) {
		t.Fatal("evicted op_id must NOT re-execute")
		return nil, nil
	}); rpcErr == nil || rpcErr.Code != proto.ErrOpExpired {
		t.Fatalf("evicted op_id retry: got %+v, want op_expired", rpcErr)
	}

	// Eviction watermark survives Snapshot -> Restore!
	snapAfterEvict := tbl.Snapshot()
	tblV3 := NewOpTable(3, 30*time.Minute)
	tblV3.nowFn = func() time.Time { return now.Add(5 * time.Second) }
	tblV3.Restore(snapAfterEvict)
	if _, rpcErr := tblV3.Execute(op1, func() (json.RawMessage, *proto.RPCError) {
		t.Fatal("evicted op_id must NOT re-execute after handoff")
		return nil, nil
	}); rpcErr == nil || rpcErr.Code != proto.ErrOpExpired {
		t.Fatalf("evicted op_id after handoff: got %+v, want op_expired", rpcErr)
	}
}

func TestEventBusRingBufferAndFreeze(t *testing.T) {
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

	// Ring holds seq 4..8.
	var got []proto.Event
	n, cur, rpcErr := bus.Resume(epoch, 3, nil, func(replayed []proto.Event, currentSeq uint64) {
		got = replayed
	})
	if rpcErr != nil || n != 5 || cur != 8 || len(got) != 5 || got[0].Seq != 4 || got[4].Seq != 8 {
		t.Fatalf("resume(3): n=%d cur=%d err=%v got=%v", n, cur, rpcErr, got)
	}
	if _, _, rpcErr := bus.Resume(epoch, 2, nil, nil); rpcErr == nil || rpcErr.Code != proto.ErrResyncRequired {
		t.Fatalf("resume(2): got %v, want resync_required", rpcErr)
	}
	if _, _, rpcErr := bus.Resume("wrong-epoch", 8, nil, nil); rpcErr == nil || rpcErr.Code != proto.ErrResyncRequired {
		t.Fatalf("resume wrong epoch: got %v, want resync_required", rpcErr)
	}

	// Item 2: FreezeAndSnapshot prevents further Emit under same epoch until Unfreeze.
	snapEpoch, snapSeq, snapEvents := bus.FreezeAndSnapshot()
	if snapEpoch != epoch || snapSeq != 8 || len(snapEvents) != 5 {
		t.Fatalf("FreezeAndSnapshot: epoch=%q seq=%d len=%d", snapEpoch, snapSeq, len(snapEvents))
	}
	if bus.CurrentSeq() != 8 {
		t.Fatalf("seq advanced while frozen: got %d, want 8", bus.CurrentSeq())
	}

	// Unfreeze (rollback path) without frozen drops keeps epoch and allows emitting.
	if rotated := bus.Unfreeze(); rotated {
		t.Fatal("Unfreeze should not rotate epoch when no emit was dropped while frozen")
	}
	ev9, err := bus.Emit("test.ev", map[string]int{"i": 9})
	if err != nil || ev9.Seq != 9 || ev9.Epoch != epoch {
		t.Fatalf("Emit after clean Unfreeze: %+v, %v", ev9, err)
	}

	// Item 3: Freeze, attempt Emit (dropped), then Unfreeze -> MUST rotate epoch!
	bus.Freeze()
	if _, err := bus.Emit("test.ev", map[string]int{"i": 10}); !errors.Is(err, ErrBusFrozen) {
		t.Fatalf("Emit while frozen: got %v, want ErrBusFrozen", err)
	}
	_, _, _, dropped := bus.FrozenSnapshot()
	if !dropped {
		t.Fatal("FrozenSnapshot did not report dropped=true after frozen Emit")
	}
	if rotated := bus.Unfreeze(); !rotated {
		t.Fatal("Unfreeze must rotate epoch after frozen Emit was dropped")
	}
	if bus.Epoch() == epoch || bus.CurrentSeq() != 0 {
		t.Fatalf("expected rotated epoch and seq=0, got epoch=%q (old %q) seq=%d", bus.Epoch(), epoch, bus.CurrentSeq())
	}
	if _, _, rpcErr := bus.Resume(epoch, 9, nil, nil); rpcErr == nil || rpcErr.Code != proto.ErrResyncRequired {
		t.Fatalf("resume with pre-rotation epoch: got %v, want resync_required", rpcErr)
	}
}

func TestBuildBoundedSnapshotTrimsOldestEvents(t *testing.T) {
	// Item 3: 17 events of ~1 MiB each must be cleanly trimmed to fit within
	// MaxHandoffSnapshotBytes (12 MiB) while preserving newest events, LastSeq,
	// Clients, and OpTable entries.
	bigData, _ := json.Marshal(map[string]string{"pad": strings.Repeat("A", 1<<20)})
	events := make([]proto.Event, 17)
	for i := range events {
		events[i] = proto.Event{
			Event: "test.big",
			Epoch: "ep1",
			Seq:   uint64(i + 1),
			Data:  bigData,
		}
	}
	snap := HandoffSnapshot{
		Schema:  HandoffSnapshotSchema,
		Epoch:   "ep1",
		LastSeq: 17,
		Events:  events,
		Clients: []proto.ClientInfo{{Gateway: "gw", CorralUser: "alice", LastSeen: "2026-11-27T00:00:00Z"}},
		Ops: OpTableSnapshot{
			Entries: []OpSnapshotEntry{{OpID: proto.NewOpID(time.Now()), State: OpStateDone, CompletedAt: time.Now().UTC()}},
		},
	}

	b, err := BuildBoundedSnapshot(snap, MaxHandoffSnapshotBytes)
	if err != nil {
		t.Fatalf("BuildBoundedSnapshot: %v", err)
	}
	if len(b) > MaxHandoffSnapshotBytes {
		t.Fatalf("snapshot bytes %d exceeded cap %d", len(b), MaxHandoffSnapshotBytes)
	}
	var parsed HandoffSnapshot
	if err := json.Unmarshal(b, &parsed); err != nil {
		t.Fatalf("trimmed snapshot invalid JSON: %v", err)
	}
	if parsed.LastSeq != 17 || len(parsed.Clients) != 1 || len(parsed.Ops.Entries) != 1 {
		t.Fatalf("trimmed snapshot lost metadata: lastSeq=%d clients=%d ops=%d", parsed.LastSeq, len(parsed.Clients), len(parsed.Ops.Entries))
	}
	if len(parsed.Events) == 0 || len(parsed.Events) >= 17 {
		t.Fatalf("expected trimmed event subset, got %d events", len(parsed.Events))
	}
	// Newest event must be retained at the end.
	if parsed.Events[len(parsed.Events)-1].Seq != 17 {
		t.Fatalf("last retained event seq=%d, want 17", parsed.Events[len(parsed.Events)-1].Seq)
	}

	// Impossible cap (smaller than base metadata) must return error rather than truncating.
	if _, err := BuildBoundedSnapshot(snap, 32); err == nil {
		t.Fatal("expected error when snapshot base exceeds maxBytes")
	}
}

func TestOpenValidatedHandoffBinaryChecksParentAndPinsFD(t *testing.T) {
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
	content := []byte("original-binary-payload")
	if err := os.WriteFile(binPath, content, 0o700); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(content)
	digest := hex.EncodeToString(sum[:])

	// Valid binary succeeds and pins open FD.
	f, resolved, dirRel, err := OpenValidatedHandoffBinary(layout, binPath, digest, layout.UID)
	if err != nil || resolved != binPath || dirRel != 200 {
		t.Fatalf("valid binary rejected: f=%v resolved=%q rel=%d err=%v", f, resolved, dirRel, err)
	}
	// Item 11: Even if the path is replaced on disk after validation, the pinned
	// FD still points to the original validated content!
	if err := os.Remove(binPath); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(binPath, []byte("swapped-evil-payload"), 0o700); err != nil {
		t.Fatal(err)
	}
	pinnedBytes, err := io.ReadAll(f)
	_ = f.Close()
	if err != nil || string(pinnedBytes) != string(content) {
		t.Fatalf("pinned FD did not retain original content: got %q, want %q", pinnedBytes, content)
	}
	// Restore original file for remaining checks.
	_ = os.WriteFile(binPath, content, 0o700)

	// Item 11: Group/other writable parent dir (0770) MUST be rejected.
	if err := os.Chmod(verDir, 0o770); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := OpenValidatedHandoffBinary(layout, binPath, digest, layout.UID); err == nil {
		t.Fatal("group-writable parent dir 0770 accepted")
	}
	_ = os.Chmod(verDir, 0o700)

	// Non-version parent dir (corrald-backups) MUST be rejected.
	badDir := filepath.Join(layout.LibDir, "corrald-backups")
	_ = os.MkdirAll(badDir, 0o700)
	badBin := filepath.Join(badDir, "corrald")
	_ = os.WriteFile(badBin, content, 0o700)
	if _, _, _, err := OpenValidatedHandoffBinary(layout, badBin, digest, layout.UID); err == nil {
		t.Fatal("non-version parent dir corrald-backups accepted")
	}

	// Wrong sha256, wrong UID, wrong binary mode (0755), outside LibDir, symlink all rejected.
	if _, err := ValidateHandoffBinary(layout, binPath, strings.Repeat("0", 64), layout.UID); err == nil {
		t.Fatal("wrong sha256 accepted")
	}
	if _, err := ValidateHandoffBinary(layout, binPath, digest, layout.UID+1); err == nil {
		t.Fatal("wrong UID accepted")
	}
	_ = os.Chmod(binPath, 0o755)
	if _, err := ValidateHandoffBinary(layout, binPath, digest, layout.UID); err == nil {
		t.Fatal("0755 binary accepted")
	}
	_ = os.Chmod(binPath, 0o700)
	outside := filepath.Join(t.TempDir(), "corrald")
	_ = os.WriteFile(outside, content, 0o700)
	if _, err := ValidateHandoffBinary(layout, outside, digest, layout.UID); err == nil {
		t.Fatal("binary outside LibDir accepted")
	}
	sym := filepath.Join(verDir, "link")
	_ = os.Symlink(binPath, sym)
	if _, err := ValidateHandoffBinary(layout, sym, digest, layout.UID); err == nil {
		t.Fatal("symlink binary accepted")
	}
}

func TestPruneOldVersionsKeepsHealthyPrevDropsFailedCandidateAndPreservesUnlisted(t *testing.T) {
	// Items 8 & 18:
	// Current=300, healthy prev=200, older=100 & 50, failed candidate=400 (in failed.json),
	// unlisted user dir corrald-backups, unlisted corrald-0.0.1-1, and code-server-4.90.0.
	// Expected: keep 300, 200, corrald-backups, corrald-0.0.1-1, code-server-4.90.0;
	// remove 400 (failed), 100, and 50!
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
	if err := layout.EnsureStateDir(); err != nil {
		t.Fatal(err)
	}

	managed := []string{
		"corrald-0.0.5-50",
		"corrald-0.1.0-100",
		"corrald-0.2.0-200",
		"corrald-0.3.0-300",
		"corrald-0.4.0-400",
	}
	unlisted := []string{
		"corrald-backups",
		"corrald-0.0.1-1",
		"code-server-4.90.0",
	}
	var manifestFiles []string
	for _, d := range managed {
		p := filepath.Join(layout.LibDir, d)
		if err := os.MkdirAll(p, 0o700); err != nil {
			t.Fatal(err)
		}
		manifestFiles = append(manifestFiles, p)
	}
	for _, d := range unlisted {
		if err := os.MkdirAll(filepath.Join(layout.LibDir, d), 0o700); err != nil {
			t.Fatal(err)
		}
	}

	if err := WriteInstallRecord(layout.InstallJSON, &InstallRecord{
		Version: "0.3.0", Release: 300, Files: manifestFiles,
	}); err != nil {
		t.Fatal(err)
	}
	if err := RecordFailedRelease(layout.FailedJSON, 400, "handoff failed"); err != nil {
		t.Fatal(err)
	}

	removed, err := PruneOldVersions(layout, "0.3.0", 300)
	if err != nil {
		t.Fatalf("PruneOldVersions: %v", err)
	}
	if len(removed) != 3 {
		t.Fatalf("expected 3 pruned versions (400, 100, 50), got %v", removed)
	}
	for _, mustExist := range []string{
		"corrald-0.3.0-300",
		"corrald-0.2.0-200",
		"corrald-backups",
		"corrald-0.0.1-1",
		"code-server-4.90.0",
	} {
		if _, err := os.Stat(filepath.Join(layout.LibDir, mustExist)); err != nil {
			t.Fatalf("expected %s to be preserved: %v", mustExist, err)
		}
	}
	for _, mustBeGone := range []string{"corrald-0.4.0-400", "corrald-0.1.0-100", "corrald-0.0.5-50"} {
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

func TestStopDaemonFailsWhenLockHeldWithoutMeta(t *testing.T) {
	// Item 7: If meta.json is missing/corrupt while a process still holds
	// corrald.lock, StopDaemon must fail instead of misreporting stopped and
	// deleting the socket.
	layout, err := ResolveLayout(PathOptions{
		Home:      t.TempDir(),
		MachineID: "123412341234",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := layout.EnsureStateDir(); err != nil {
		t.Fatal(err)
	}
	if err := layout.EnsureSocketDir(); err != nil {
		t.Fatal(err)
	}
	if layout.SocketUsesTmpDir {
		t.Cleanup(func() { _ = os.RemoveAll(layout.SocketDir) })
	}

	// Create a dummy socket file and hold corrald.lock without writing meta.json.
	if err := os.WriteFile(layout.SocketPath, []byte("sock"), 0o600); err != nil {
		t.Fatal(err)
	}
	lock, err := AcquireLock(layout.LockPath)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Release()

	stopped, _, err := StopDaemon(layout)
	if err == nil || stopped {
		t.Fatalf("expected StopDaemon to fail while lock held without meta, got stopped=%v err=%v", stopped, err)
	}
	if _, statErr := os.Lstat(layout.SocketPath); statErr != nil {
		t.Fatalf("socket was deleted even though StopDaemon failed: %v", statErr)
	}
}

func TestStopDaemonStopsDrainingPredecessor(t *testing.T) {
	// Item 6: When meta.json records a PrevPID that is still alive (draining old
	// daemon holding the inherited flock after v2 exited), StopDaemon must stop
	// PrevPID and confirm corrald.lock is released.
	layout, err := ResolveLayout(PathOptions{
		Home:      t.TempDir(),
		MachineID: "567856785678",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := layout.EnsureStateDir(); err != nil {
		t.Fatal(err)
	}

	// Spawn a lightweight sleeper child that inherits the held corrald.lock FD.
	lock, err := AcquireLock(layout.LockPath)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sleep", "30")
	cmd.ExtraFiles = []*os.File{lock.File()}
	if err := cmd.Start(); err != nil {
		_ = lock.Release()
		t.Skipf("sleep unavailable: %v", err)
	}
	// Close parent copy so ONLY the child holds corrald.lock.
	_ = lock.Release()
	waitDone := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(waitDone)
	}()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		<-waitDone
	})

	childStart, alive, err := ProcStartTime(cmd.Process.Pid)
	if err != nil || !alive {
		t.Fatalf("child proc start: %q %v %v", childStart, alive, err)
	}

	// meta.json points PID to a dead process (0) and PrevPID to our draining child.
	if err := WriteMetaRecord(layout.MetaJSON, &MetaRecord{
		PID:           99999999,
		StartTime:     "1",
		PrevPID:       cmd.Process.Pid,
		PrevStartTime: childStart,
	}); err != nil {
		t.Fatal(err)
	}

	stopped, _, err := StopDaemon(layout)
	if err != nil || !stopped {
		t.Fatalf("StopDaemon failed to stop draining predecessor: stopped=%v err=%v", stopped, err)
	}
	if ProcessMatches(cmd.Process.Pid, childStart) {
		t.Fatal("draining predecessor process still running after StopDaemon")
	}
}

func TestIsLastMachineConservativeOnErrors(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("chmod 0000 does not block root")
	}
	stateRoot := t.TempDir()
	otherDir := filepath.Join(stateRoot, "bbbbbbbbbbbb")
	if err := os.MkdirAll(otherDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(otherDir, "install.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Item 15: Unreadable sibling directory -> MUST return (false, err), NOT true!
	if err := os.Chmod(otherDir, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(otherDir, 0o700) })

	isLast, err := isLastMachineInStateRoot(stateRoot, "aaaaaaaaaaaa")
	if err == nil || isLast {
		t.Fatalf("unreadable sibling dir: got isLast=%v err=%v, want (false, non-nil err)", isLast, err)
	}

	// Unreadable stateRoot itself -> MUST return (false, err), NOT true!
	_ = os.Chmod(otherDir, 0o700)
	if err := os.Chmod(stateRoot, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(stateRoot, 0o700) })

	isLast, err = isLastMachineInStateRoot(stateRoot, "aaaaaaaaaaaa")
	if err == nil || isLast {
		t.Fatalf("unreadable stateRoot: got isLast=%v err=%v, want (false, non-nil err)", isLast, err)
	}
}

func TestHelloRejectsOversizedFieldsAndCapsClientRegistry(t *testing.T) {
	// Item 19: Field lengths and client list size must be strictly bounded so
	// hello responses can never exceed MaxFrameSize.
	home := t.TempDir()
	layout, err := ResolveLayout(PathOptions{
		Home:      home,
		MachineID: "998877665544",
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

	// Seed a corrupt oversized clients.json on disk and verify OpenClientRegistry sanitizes it.
	dirty := []proto.ClientInfo{{
		Gateway:    strings.Repeat("g", 1024),
		CorralUser: strings.Repeat("u", 512),
		HostID:     "h1",
		LastSeen:   "2026-11-27T00:00:00Z",
	}}
	b, _ := json.Marshal(dirty)
	if err := os.WriteFile(layout.ClientsJSON, b, 0o600); err != nil {
		t.Fatal(err)
	}

	ready := make(chan struct{})
	srv, err := NewServer(ServerConfig{
		Layout:  layout,
		Build:   proto.BuildInfo{Version: "0.1.0", Release: 100},
		OnReady: func() { close(ready) },
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
		t.Fatalf("Serve failed: %v", err)
	}
	defer func() { cancel(); <-done }()

	c := dialRPC(t, layout.SocketPath)
	defer c.close()

	// Oversized gateway (3 MiB) must be rejected with invalid_params.
	resp, _ := c.call(t, proto.MethodDaemonHello, proto.HelloParams{
		Gateway:        strings.Repeat("x", 3<<20),
		GatewayVersion: "1.0",
		ProtoMin:       1,
		ProtoMax:       1,
		Client:         proto.ClientIdentity{CorralUser: "alice", HostID: "h1"},
	})
	if resp.Error == nil || resp.Error.Code != proto.ErrInvalidParams {
		t.Fatalf("oversized hello gateway: got %+v, want invalid_params", resp)
	}

	// Registering 80 distinct users must cap stored list at MaxRegisteredClients (64).
	for i := 0; i < 80; i++ {
		r, _ := c.call(t, proto.MethodDaemonHello, proto.HelloParams{
			Gateway:        "gw.cap",
			GatewayVersion: "1.0",
			ProtoMin:       1,
			ProtoMax:       1,
			Client:         proto.ClientIdentity{CorralUser: fmt.Sprintf("user-%03d", i), HostID: "h1"},
		})
		if r.Error != nil {
			t.Fatalf("hello %d failed: %v", i, r.Error)
		}
		var hr proto.HelloResult
		if err := json.Unmarshal(r.Result, &hr); err != nil {
			t.Fatal(err)
		}
		if len(hr.Clients) > proto.MaxRegisteredClients {
			t.Fatalf("clients list grew past cap %d: got %d", proto.MaxRegisteredClients, len(hr.Clients))
		}
	}
}

func TestSocketWatchdogRebindsUnlinkedSocket(t *testing.T) {
	// Item 20: Removing corrald.sock while server is running must cause the
	// watchdog to rebind corrald.sock so new clients connect cleanly.
	home := t.TempDir()
	layout, err := ResolveLayout(PathOptions{
		Home:      home,
		MachineID: "a1b2c3d4e5f6",
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
		Layout:              layout,
		Build:               proto.BuildInfo{Version: "0.1.0", Release: 100},
		SocketWatchInterval: 40 * time.Millisecond,
		OnReady:             func() { close(ready) },
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
		t.Fatalf("Serve failed: %v", err)
	}
	defer func() { cancel(); <-done }()

	// Unlink the live socket (simulating systemd-tmpfiles cleanup).
	if err := os.Remove(layout.SocketPath); err != nil {
		t.Fatal(err)
	}

	// Wait for watchdog to rebind and accept a fresh RPC connection.
	deadline := time.Now().Add(2 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		var health proto.HealthResult
		if err := callLocalRPC(layout.SocketPath, proto.MethodDaemonHealth, nil, &health, 300*time.Millisecond); err == nil {
			if health.Caps["socket"] == "ok" {
				return
			}
			lastErr = fmt.Errorf("socket cap=%q", health.Caps["socket"])
		} else {
			lastErr = err
		}
		time.Sleep(30 * time.Millisecond)
	}
	t.Fatalf("socket watchdog did not rebind unlinked socket: %v", lastErr)
}

func TestHandoffRejectsNonNewerRelease(t *testing.T) {
	// Item 21: daemon.handoff must reject equal or older candidate releases
	// with error code not_newer.
	home := t.TempDir()
	layout, err := ResolveLayout(PathOptions{
		Home:      home,
		MachineID: "102030405060",
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
		Layout:  layout,
		Build:   proto.BuildInfo{Version: "0.2.0", Release: 200},
		OnReady: func() { close(ready) },
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
		t.Fatalf("Serve failed: %v", err)
	}
	defer func() { cancel(); <-done }()

	c := dialRPC(t, layout.SocketPath)
	defer c.close()

	for _, rel := range []int64{200, 100, 0} {
		resp, _ := c.call(t, proto.MethodDaemonHandoff, proto.HandoffParams{
			Path:    filepath.Join(layout.LibDir, fmt.Sprintf("corrald-0.1.0-%d", rel), "corrald"),
			SHA256:  strings.Repeat("a", 64),
			Release: rel,
		})
		if resp.Error == nil || resp.Error.Code != proto.ErrNotNewer {
			t.Fatalf("handoff release=%d on current=200: got %+v, want not_newer", rel, resp)
		}
	}
}

func TestFinalizeHandoffCutoverFailClosedAndRestoresState(t *testing.T) {
	// Items 4 & 17: If persistent cutover fails midway (e.g. unwritable
	// StateDir preventing meta.json write), finalizeHandoffCutover must restore
	// the previous symlink and install.json and return an error.
	if os.Geteuid() == 0 {
		t.Skip("chmod 0500 does not block root")
	}
	home := t.TempDir()
	layout, err := ResolveLayout(PathOptions{
		Home:      home,
		MachineID: "abcdef012345",
		TmpBase:   t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := layout.EnsureLibDir(); err != nil {
		t.Fatal(err)
	}
	if err := layout.EnsureStateDir(); err != nil {
		t.Fatal(err)
	}

	v1Dir, _ := layout.VersionDir("0.1.0", 100)
	v2Dir, _ := layout.VersionDir("0.2.0", 200)
	_ = os.MkdirAll(v1Dir, 0o700)
	_ = os.MkdirAll(v2Dir, 0o700)
	v1Bin := filepath.Join(v1Dir, "corrald")
	v2Bin := filepath.Join(v2Dir, "corrald")
	_ = os.WriteFile(v1Bin, []byte("v1"), 0o700)
	_ = os.WriteFile(v2Bin, []byte("v2"), 0o700)

	origTarget := "corrald-0.1.0-100/corrald"
	if err := AtomicSymlink(origTarget, layout.CurrentSymlink); err != nil {
		t.Fatal(err)
	}
	if err := WriteInstallRecord(layout.InstallJSON, &InstallRecord{
		Version: "0.1.0", Release: 100, Files: []string{v1Dir, v1Bin},
	}); err != nil {
		t.Fatal(err)
	}
	if err := WriteMetaRecord(layout.MetaJSON, &MetaRecord{
		PID: 111, StartTime: "10", Version: "0.1.0", Release: 100,
	}); err != nil {
		t.Fatal(err)
	}

	// Make StateDir read-only so updating install.json / meta.json fails AFTER symlink switched.
	if err := os.Chmod(layout.StateDir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(layout.StateDir, 0o700) })

	err = finalizeHandoffCutover(layout, proto.BuildInfo{Version: "0.2.0", Release: 200}, v2Bin, &MetaRecord{
		PID: 222, StartTime: "20", Version: "0.2.0", Release: 200,
	})
	if err == nil {
		t.Fatal("expected finalizeHandoffCutover to fail when StateDir is read-only")
	}

	// Symlink MUST be rolled back to v1!
	gotLink, rerr := os.Readlink(layout.CurrentSymlink)
	if rerr != nil || gotLink != origTarget {
		t.Fatalf("symlink not restored after failed cutover: got %q (%v), want %q", gotLink, rerr, origTarget)
	}
}

func TestServeDoesNotAcceptOrExecuteBeforePreReady(t *testing.T) {
	// Round-2 Item 1: New process must NOT accept or execute any request before
	// PreReady finishes; if PreReady fails, zero requests may have run.
	home := t.TempDir()
	layout, err := ResolveLayout(PathOptions{Home: home, MachineID: "111100001111"})
	if err != nil {
		t.Fatal(err)
	}
	if err := layout.EnsureStateDir(); err != nil {
		t.Fatal(err)
	}
	if layout.SocketUsesTmpDir {
		t.Cleanup(func() { _ = os.RemoveAll(layout.SocketDir) })
	}

	// Pre-bind listener (as inherited during handoff).
	if err := layout.EnsureSocketDir(); err != nil {
		t.Fatal(err)
	}
	_ = os.Remove(layout.SocketPath)
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: layout.SocketPath, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	ln.SetUnlinkOnClose(false)

	preReadyEntered := make(chan struct{})
	preReadyRelease := make(chan struct{})
	var writesExecuted atomic.Int32

	srv, err := NewServer(ServerConfig{
		Layout:   layout,
		Build:    proto.BuildInfo{Version: "0.2.0", Release: 200},
		Listener: ln,
		PreReady: func(_ *MetaRecord) error {
			close(preReadyEntered)
			<-preReadyRelease
			return errors.New("cutover failed")
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	srv.Register("test.idem", MethodOptions{Mutating: true, Idempotent: true}, func(_ context.Context, _ *Conn, _ json.RawMessage) (any, *proto.RPCError) {
		writesExecuted.Add(1)
		return map[string]bool{"ok": true}, nil
	})

	serveDone := make(chan error, 1)
	go func() { serveDone <- srv.Serve(context.Background()) }()

	<-preReadyEntered
	// Dial and send a write request while PreReady is blocked!
	conn, err := net.DialTimeout("unix", layout.SocketPath, time.Second)
	if err != nil {
		t.Fatalf("dial queued listener: %v", err)
	}
	defer conn.Close()
	raw, _ := json.Marshal(map[string]string{"op_id": proto.NewOpID(time.Now())})
	_ = proto.WriteJSON(conn, proto.Request{ID: 1, Method: "test.idem", Params: raw})

	// Give scheduler time; handler MUST NOT run while PreReady is incomplete!
	time.Sleep(80 * time.Millisecond)
	if writesExecuted.Load() != 0 {
		t.Fatalf("write executed BEFORE PreReady completed! count=%d", writesExecuted.Load())
	}

	close(preReadyRelease)
	if err := <-serveDone; err == nil {
		t.Fatal("expected Serve to fail when PreReady returned error")
	}
	if writesExecuted.Load() != 0 {
		t.Fatalf("write executed despite failed PreReady! count=%d", writesExecuted.Load())
	}
}

func TestAtomicIdempotentAdmissionBeforeHandlerRuns(t *testing.T) {
	// Round-2 Item 2: An admitted idempotent write MUST already be present in
	// OpTable.Snapshot() BEFORE the handler finishes (even if handler stalls
	// immediately upon entry), and duplicates get retryable in_progress.
	home := t.TempDir()
	layout, err := ResolveLayout(PathOptions{Home: home, MachineID: "222200002222"})
	if err != nil {
		t.Fatal(err)
	}
	if err := layout.EnsureStateDir(); err != nil {
		t.Fatal(err)
	}
	if layout.SocketUsesTmpDir {
		t.Cleanup(func() { _ = os.RemoveAll(layout.SocketDir) })
	}

	entered := make(chan struct{})
	release := make(chan struct{})
	ready := make(chan struct{})
	srv, err := NewServer(ServerConfig{
		Layout:  layout,
		Build:   proto.BuildInfo{Version: "0.1.0", Release: 100},
		OnReady: func() { close(ready) },
	})
	if err != nil {
		t.Fatal(err)
	}
	srv.Register("test.atomic_write", MethodOptions{Mutating: true, Idempotent: true}, func(_ context.Context, _ *Conn, _ json.RawMessage) (any, *proto.RPCError) {
		close(entered)
		<-release
		return map[string]bool{"done": true}, nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx) }()
	<-ready
	defer func() { cancel(); <-done }()

	opID := proto.NewOpID(time.Now())
	c1 := dialRPC(t, layout.SocketPath)
	defer c1.close()
	resCh := make(chan proto.Response, 1)
	go func() {
		r, _ := c1.call(t, "test.atomic_write", map[string]string{"op_id": opID})
		resCh <- r
	}()

	<-entered
	// Snapshot immediately while handler is blocked: opID MUST be in snapshot as unknown!
	snap := srv.Ops().Snapshot()
	found := false
	for _, e := range snap.Entries {
		if e.OpID == opID && e.State == OpStateUnknown {
			found = true
		}
	}
	if !found {
		t.Fatalf("admitted op_id %q missing from OpTable.Snapshot() while handler stalled: %+v", opID, snap)
	}

	// Duplicate while running gets retryable in_progress.
	c2 := dialRPC(t, layout.SocketPath)
	defer c2.close()
	dup, _ := c2.call(t, "test.atomic_write", map[string]string{"op_id": opID})
	if dup.Error == nil || dup.Error.Code != proto.ErrInProgress || !dup.Error.Retryable {
		t.Fatalf("duplicate while running: got %+v, want retryable in_progress", dup)
	}

	close(release)
	r1 := <-resCh
	if r1.Error != nil {
		t.Fatalf("first call failed: %v", r1.Error)
	}
}

func TestForceNewEpochInHandoffSnapshot(t *testing.T) {
	// Round-2 Item 3: When ForceNewEpoch is true in HandoffSnapshot, v2 MUST
	// rotate to a fresh epoch while still restoring OpTable and Clients.
	home := t.TempDir()
	layout, err := ResolveLayout(PathOptions{Home: home, MachineID: "333300003333"})
	if err != nil {
		t.Fatal(err)
	}
	if err := layout.EnsureStateDir(); err != nil {
		t.Fatal(err)
	}
	if layout.SocketUsesTmpDir {
		t.Cleanup(func() { _ = os.RemoveAll(layout.SocketDir) })
	}

	opID := proto.NewOpID(time.Now())
	ready := make(chan struct{})
	srv, err := NewServer(ServerConfig{
		Layout: layout,
		Build:  proto.BuildInfo{Version: "0.2.0", Release: 200},
		InitialSnapshot: &HandoffSnapshot{
			Schema:        HandoffSnapshotSchema,
			Epoch:         "old-epoch-1111",
			LastSeq:       42,
			ForceNewEpoch: true,
			Clients:       []proto.ClientInfo{{Gateway: "gw", CorralUser: "alice", LastSeen: "2026-11-27T00:00:00Z"}},
			Ops: OpTableSnapshot{
				Entries: []OpSnapshotEntry{{OpID: opID, State: OpStateUnknown, CompletedAt: time.Now().UTC()}},
			},
		},
		OnReady: func() { close(ready) },
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx) }()
	<-ready
	defer func() { cancel(); <-done }()

	if srv.Bus().Epoch() == "old-epoch-1111" {
		t.Fatal("v2 did not rotate epoch when ForceNewEpoch=true")
	}
	if srv.Bus().CurrentSeq() != 0 {
		t.Fatalf("expected seq=0 on new epoch, got %d", srv.Bus().CurrentSeq())
	}
	// OpTable must still have been restored!
	if _, rpcErr := srv.Ops().Execute(opID, func() (json.RawMessage, *proto.RPCError) {
		t.Fatal("restored unknown op_id must not re-execute")
		return nil, nil
	}); rpcErr == nil || rpcErr.Code != proto.ErrOpUnknown {
		t.Fatalf("expected op_unknown, got %v", rpcErr)
	}
}

func TestRebindDoesNotInterleaveOrUnpauseHandoff(t *testing.T) {
	// Round-2 Item 4: Socket rebind shares listenerOpMu with handoff and must
	// never unpause a handoff-paused accept loop or swap listener during handoff.
	home := t.TempDir()
	layout, err := ResolveLayout(PathOptions{Home: home, MachineID: "444400004444"})
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
		Layout:  layout,
		Build:   proto.BuildInfo{Version: "0.1.0", Release: 100},
		OnReady: func() { close(ready) },
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx) }()
	<-ready
	defer func() { cancel(); <-done }()

	// Simulate handoff holding listenerOpMu with handoffActive=true & acceptPaused=true.
	srv.stateMu.Lock()
	srv.handoffActive = true
	srv.stateMu.Unlock()
	srv.listenerOpMu.Lock()
	if err := srv.pauseAccept(); err != nil {
		srv.listenerOpMu.Unlock()
		t.Fatal(err)
	}
	_ = os.Remove(layout.SocketPath)

	rebindDone := make(chan error, 1)
	go func() { rebindDone <- srv.rebindSocket() }()

	// rebindSocket MUST block on listenerOpMu and NOT unpause acceptPaused!
	select {
	case err := <-rebindDone:
		t.Fatalf("rebindSocket returned while handoff held listenerOpMu: %v", err)
	case <-time.After(80 * time.Millisecond):
	}
	srv.acceptMu.Lock()
	paused := srv.acceptPaused
	srv.acceptMu.Unlock()
	if !paused {
		t.Fatal("rebindSocket unpaused accept loop during handoff")
	}

	// Simulate handoff completion (handedOff=true) and release listenerOpMu.
	srv.stateMu.Lock()
	srv.handedOff = true
	srv.stateMu.Unlock()
	srv.listenerOpMu.Unlock()

	if err := <-rebindDone; err != nil {
		t.Fatalf("rebindSocket after handoff: %v", err)
	}
	// Socket must NOT have been recreated after handoff completed.
	if _, err := os.Lstat(layout.SocketPath); !os.IsNotExist(err) {
		t.Fatal("rebindSocket recreated socket after handoff already completed")
	}
}

func TestAcquireLockMutualExclusionWithUnlink(t *testing.T) {
	// Round-2 Item 6: Standard inode-verified AcquireLock guarantees mutual
	// exclusion even when the lock holder unlinks the lock file before releasing.
	lockPath := filepath.Join(t.TempDir(), "shared.lock")
	var active atomic.Int32
	var violations atomic.Int32
	var wg sync.WaitGroup

	for g := 0; g < 6; g++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for i := 0; i < 15; i++ {
				lk, err := AcquireLockWait(lockPath, 5*time.Second)
				if err != nil {
					t.Errorf("goroutine %d iter %d: %v", id, i, err)
					return
				}
				if cur := active.Add(1); cur != 1 {
					violations.Add(1)
				}
				time.Sleep(2 * time.Millisecond)
				if i%3 == 0 {
					// Unlink while holding lock (last-machine uninstall pattern).
					_ = os.Remove(lockPath)
				}
				active.Add(-1)
				_ = lk.Release()
			}
		}(g)
	}
	wg.Wait()
	if violations.Load() > 0 {
		t.Fatalf("detected %d lock mutual-exclusion violations across unlink", violations.Load())
	}
}

func TestPruneOldVersionsSkipsWhenAnyOtherMachineDirExists(t *testing.T) {
	// Round-2 Item 7: Simplified GC MUST NOT delete ANY version dir whenever
	// any other machine directory exists under StateRoot.
	home := t.TempDir()
	layout, err := ResolveLayout(PathOptions{Home: home, MachineID: "aaaaaaaaaaaa", TmpBase: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	_ = layout.EnsureLibDir()
	_ = layout.EnsureStateDir()

	var files []string
	for _, d := range []string{"corrald-0.1.0-100", "corrald-0.2.0-200", "corrald-0.3.0-300"} {
		p := filepath.Join(layout.LibDir, d)
		_ = os.MkdirAll(p, 0o700)
		files = append(files, p)
	}
	_ = WriteInstallRecord(layout.InstallJSON, &InstallRecord{Version: "0.3.0", Release: 300, Files: files})

	// Create another machine directory under StateRoot (even without install.json).
	otherDir := filepath.Join(layout.StateRoot, "bbbbbbbbbbbb")
	if err := os.MkdirAll(otherDir, 0o700); err != nil {
		t.Fatal(err)
	}

	removed, err := PruneOldVersions(layout, "0.3.0", 300)
	if err != nil || len(removed) != 0 {
		t.Fatalf("GC deleted versions while another machine dir existed: removed=%v err=%v", removed, err)
	}
	if _, err := os.Stat(filepath.Join(layout.LibDir, "corrald-0.1.0-100")); err != nil {
		t.Fatal("corrald-0.1.0-100 was deleted while another machine existed")
	}

	// Once the other machine directory is gone, GC deletes 0.1.0-100 and keeps 0.2.0-200 + 0.3.0-300.
	_ = os.RemoveAll(otherDir)
	removed, err = PruneOldVersions(layout, "0.3.0", 300)
	if err != nil || len(removed) != 1 {
		t.Fatalf("GC when sole machine: removed=%v err=%v, want 1", removed, err)
	}
}

func TestFinalizeHandoffCutoverFailsWhenTargetMissing(t *testing.T) {
	// Round-2 Item 7: If the target binary was deleted on disk before cutover,
	// finalizeHandoffCutover MUST fail and MUST NOT write a dangling symlink.
	home := t.TempDir()
	layout, err := ResolveLayout(PathOptions{Home: home, MachineID: "777700007777", TmpBase: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	_ = layout.EnsureLibDir()
	_ = layout.EnsureStateDir()

	missingBin := filepath.Join(layout.LibDir, "corrald-0.2.0-200", "corrald")
	err = finalizeHandoffCutover(layout, proto.BuildInfo{Version: "0.2.0", Release: 200}, missingBin, &MetaRecord{PID: 999, Release: 200})
	if err == nil {
		t.Fatal("expected finalizeHandoffCutover to fail for missing binary")
	}
	if _, statErr := os.Lstat(layout.CurrentSymlink); !os.IsNotExist(statErr) {
		t.Fatal("finalizeHandoffCutover created a dangling symlink for missing binary")
	}
}

func TestCompareAndRestorePreservesExternalUpdateDeletesNewLinkAndReportsHealth(t *testing.T) {
	// Round-2 Items 8 & 9:
	// 1. Compare-and-restore must NOT overwrite a newer symlink written by another host
	//    and must preserve concurrent additions to install.json.Files.
	// 2. When symlink did not exist previously, compare-and-restore must DELETE the
	//    newly created link if it still points to the failed candidate.
	// 3. When restore fails, error is returned (and Server surfaces it via daemon.health).
	home := t.TempDir()
	layout, err := ResolveLayout(PathOptions{Home: home, MachineID: "888800008888"})
	if err != nil {
		t.Fatal(err)
	}
	_ = layout.EnsureLibDir()
	_ = layout.EnsureStateDir()
	if layout.SocketUsesTmpDir {
		t.Cleanup(func() { _ = os.RemoveAll(layout.SocketDir) })
	}

	// Case 1: Machine B concurrently upgraded symlink to 0.3.0-300 while A rolled back 0.2.0-200.
	bk := cutoverBackup{
		hasSymlink:  true,
		symlink:     "corrald-0.1.0-100/corrald",
		hasInstall:  true,
		origVersion: "0.1.0",
		origRelease: 100,
	}
	_ = AtomicSymlink("corrald-0.3.0-300/corrald", layout.CurrentSymlink)
	_ = WriteInstallRecord(layout.InstallJSON, &InstallRecord{
		Version: "0.2.0",
		Release: 200,
		Files:   []string{"/a", "/concurrently-added"},
	})
	if err := compareAndRestoreCutoverLocked(layout, bk, &MetaRecord{PID: 10, Release: 100}, "corrald-0.2.0-200/corrald", 20, 200, false); err != nil {
		t.Fatalf("compareAndRestore Case 1: %v", err)
	}
	// Symlink MUST still be 0.3.0-300 (NOT clobbered back to 0.1.0-100)!
	if link, _ := os.Readlink(layout.CurrentSymlink); link != "corrald-0.3.0-300/corrald" {
		t.Fatalf("external newer symlink was overwritten: got %q", link)
	}
	// install.json reverted Release to 100 while preserving "/concurrently-added" in Files!
	rec, _ := ReadInstallRecord(layout.InstallJSON)
	if rec.Release != 100 || !strings.Contains(strings.Join(rec.Files, ","), "/concurrently-added") {
		t.Fatalf("install.json rollback lost concurrent Files or failed revert: %+v", rec)
	}

	// Case 2: Symlink did NOT exist before (hasSymlink=false), candidate created it -> MUST be deleted!
	bkNoLink := cutoverBackup{hasSymlink: false}
	_ = AtomicSymlink("corrald-0.2.0-200/corrald", layout.CurrentSymlink)
	if err := compareAndRestoreCutoverLocked(layout, bkNoLink, nil, "corrald-0.2.0-200/corrald", 20, 200, false); err != nil {
		t.Fatalf("compareAndRestore Case 2: %v", err)
	}
	if _, err := os.Lstat(layout.CurrentSymlink); !os.IsNotExist(err) {
		t.Fatal("newly created symlink was not removed when hasSymlink=false")
	}

	// Case 3: Health reports rollback_failed when rollbackDegraded is set.
	ready := make(chan struct{})
	srv, err := NewServer(ServerConfig{
		Layout:  layout,
		Build:   proto.BuildInfo{Version: "0.1.0", Release: 100},
		OnReady: func() { close(ready) },
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx) }()
	<-ready
	defer func() { cancel(); <-done }()

	srv.stateMu.Lock()
	srv.rollbackDegraded = "restore symlink: permission denied"
	srv.stateMu.Unlock()

	var health proto.HealthResult
	if err := callLocalRPC(layout.SocketPath, proto.MethodDaemonHealth, nil, &health, 2*time.Second); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(health.Caps["state"], "rollback_failed:") {
		t.Fatalf("expected daemon.health caps[state] rollback_failed, got %+v", health.Caps)
	}
}

func TestHardExitInvokesCallbackWithoutReleasingLock(t *testing.T) {
	// Round-2 Item 10: On hard exit deadline, OnHardExit (os.Exit(0)) must be
	// invoked WITHOUT pre-releasing s.lock (the OS releases the lock atomically
	// when the process exits).
	home := t.TempDir()
	layout, err := ResolveLayout(PathOptions{Home: home, MachineID: "999900009999"})
	if err != nil {
		t.Fatal(err)
	}
	_ = layout.EnsureStateDir()
	if layout.SocketUsesTmpDir {
		t.Cleanup(func() { _ = os.RemoveAll(layout.SocketDir) })
	}

	lock, err := AcquireLock(layout.LockPath)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Release()

	var lockStillHeld atomic.Bool
	var hardExitCalled atomic.Bool
	blockWrite := make(chan struct{})
	defer close(blockWrite)
	entered := make(chan struct{})
	ready := make(chan struct{})

	srv, err := NewServer(ServerConfig{
		Layout:  layout,
		Build:   proto.BuildInfo{Version: "0.1.0", Release: 100},
		Lock:    lock,
		OnReady: func() { close(ready) },
		OnHardExit: func() {
			hardExitCalled.Store(true)
			if _, err := AcquireLock(layout.LockPath); errors.Is(err, ErrLocked) {
				lockStillHeld.Store(true)
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	srv.Register("test.stuck", MethodOptions{Mutating: true}, func(_ context.Context, _ *Conn, _ json.RawMessage) (any, *proto.RPCError) {
		close(entered)
		<-blockWrite
		return nil, nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx) }()
	<-ready

	c := dialRPC(t, layout.SocketPath)
	defer c.close()
	go func() {
		id := c.nextID.Add(1)
		_ = proto.WriteJSON(c.conn, proto.Request{ID: id, Method: "test.stuck"})
	}()
	<-entered

	// Simulate post-handoff hard exit deadline expiring while test.stuck is blocked.
	srv.stateMu.Lock()
	srv.handedOff = true
	srv.hardExitDeadline = time.Now().Add(60 * time.Millisecond)
	srv.stateMu.Unlock()
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve returned error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Serve did not return at hard exit deadline")
	}

	if !hardExitCalled.Load() {
		t.Fatal("OnHardExit callback was not invoked")
	}
	if !lockStillHeld.Load() {
		t.Fatal("s.lock was released BEFORE OnHardExit (os.Exit) was invoked")
	}
}
