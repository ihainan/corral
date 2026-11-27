//go:build linux

package daemon

import (
	"bufio"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/ihainan/corral/internal/corrald/proto"
)

// HandoffSnapshotSchema is the current handoff state snapshot format version.
const HandoffSnapshotSchema = 1

// HandoffSnapshot is streamed from v1 to v2 over the inherited state pipe
// during a zero-gap hot update (spec §6).
type HandoffSnapshot struct {
	Schema       int                        `json:"schema"`
	Epoch        string                     `json:"epoch"`
	LastSeq      uint64                     `json:"last_seq"`
	Events       []proto.Event              `json:"events"`
	Clients      []proto.ClientInfo         `json:"clients"`
	CompletedOps []CompletedOp              `json:"completed_ops,omitempty"`
	Modules      map[string]json.RawMessage `json:"modules,omitempty"`
	SocketPath   string                     `json:"socket_path"`
	Gateway      string                     `json:"gateway"`
	OldPID       int                        `json:"old_pid"`
	OldVersion   string                     `json:"old_version"`
	OldRelease   int64                      `json:"old_release"`
}

func (s *Server) handleHandoff(ctx context.Context, conn *Conn, raw json.RawMessage) (any, *proto.RPCError) {
	// Actually invoked through dispatchHandoff so we can enqueue the RPC
	// response before broadcasting daemon.reconnect and exiting.
	return nil, &proto.RPCError{Code: proto.ErrInternal, Message: "handoff called outside request context"}
}

func (s *Server) dispatchHandoff(ctx context.Context, conn *Conn, req proto.Request) {
	started := time.Now()
	var p proto.HandoffParams
	if err := json.Unmarshal(req.Params, &p); err != nil {
		conn.enqueueResponse(req.ID, nil, &proto.RPCError{Code: proto.ErrInvalidParams, Message: err.Error()})
		return
	}

	// 1. Validate binary path, ownership, mode 0700, and sha256 BEFORE entering draining.
	resolvedPath, err := ValidateHandoffBinary(s.layout, p.Path, p.SHA256, s.expectedUID)
	if err != nil {
		s.log.Warn("handoff binary validation failed", "path", p.Path, "err", err)
		conn.enqueueResponse(req.ID, nil, &proto.RPCError{
			Code:    proto.ErrInvalidParams,
			Message: err.Error(),
		})
		return
	}

	// 2. Enter draining atomically (reject concurrent handoffs).
	s.stateMu.Lock()
	if s.handoffActive || s.handedOff {
		s.stateMu.Unlock()
		conn.enqueueResponse(req.ID, nil, &proto.RPCError{
			Code:      proto.ErrHandoffInProgress,
			Message:   "handoff already in progress",
			Retryable: true,
		})
		return
	}
	s.handoffActive = true
	s.state = proto.StateDraining
	s.stateMu.Unlock()

	rollback := func(reason string) {
		s.resumeAccept()
		s.stateMu.Lock()
		s.handoffActive = false
		s.state = proto.StateRunning
		s.stateMu.Unlock()
		if p.Release > 0 {
			if recErr := RecordFailedRelease(s.layout.FailedJSON, p.Release, reason); recErr != nil {
				s.log.Warn("record failed release error", "release", p.Release, "err", recErr)
			}
		}
		s.log.Error("handoff failed; resumed serving on current process", "release", p.Release, "reason", reason)
		conn.enqueueResponse(req.ID, nil, &proto.RPCError{
			Code:    proto.ErrHandoffFailed,
			Message: reason,
		})
	}

	// Wait up to DrainTimeout (5 s) for in-flight mutating RPCs to finish.
	s.waitForInflightWrites(ctx, s.cfg.DrainTimeout)

	// 3. Pause accept loop while keeping the listening socket FD open in the kernel backlog.
	if err := s.pauseAccept(); err != nil {
		rollback("pause accept: " + err.Error())
		return
	}

	listenFile, err := s.ln.File()
	if err != nil {
		rollback("dup listen fd: " + err.Error())
		return
	}

	stateR, stateW, err := os.Pipe()
	if err != nil {
		_ = listenFile.Close()
		rollback("create state pipe: " + err.Error())
		return
	}
	readyR, readyW, err := os.Pipe()
	if err != nil {
		_ = listenFile.Close()
		_ = stateR.Close()
		_ = stateW.Close()
		rollback("create ready pipe: " + err.Error())
		return
	}
	devNull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		_ = listenFile.Close()
		_ = stateR.Close()
		_ = stateW.Close()
		_ = readyR.Close()
		_ = readyW.Close()
		rollback("open /dev/null: " + err.Error())
		return
	}
	logOut, err := os.OpenFile(s.layout.LogPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		logOut = devNull
	}

	// Snapshot current daemon state right before spawning v2.
	epoch, lastSeq, events := s.bus.Snapshot()
	snap := HandoffSnapshot{
		Schema:       HandoffSnapshotSchema,
		Epoch:        epoch,
		LastSeq:      lastSeq,
		Events:       events,
		Clients:      s.clients.List(),
		CompletedOps: s.ops.Snapshot(),
		Modules:      s.snapshotModuleStates(),
		SocketPath:   s.layout.SocketPath,
		Gateway:      s.cfg.Gateway,
		OldPID:       os.Getpid(),
		OldVersion:   s.build.Version,
		OldRelease:   s.build.Release,
	}
	snapBytes, err := json.Marshal(snap)
	if err != nil {
		_ = listenFile.Close()
		_ = stateR.Close()
		_ = stateW.Close()
		_ = readyR.Close()
		_ = readyW.Close()
		_ = devNull.Close()
		if logOut != devNull {
			_ = logOut.Close()
		}
		rollback("marshal snapshot: " + err.Error())
		return
	}

	// Prepare lock file for inheritance (if server does not hold one, e.g. in an
	// embedded test, pass /dev/null and omit --lock-fd).
	files := []*os.File{
		devNull,    // fd 0: stdin
		logOut,     // fd 1: stdout
		logOut,     // fd 2: stderr
		listenFile, // fd 3: listening Unix socket
		stateR,     // fd 4: state snapshot read end
		readyW,     // fd 5: readiness signal write end
	}
	argv := []string{
		resolvedPath,
		"serve",
		"--gateway", s.cfg.Gateway,
		"--foreground",
		"--handoff",
		"--listen-fd=3",
		"--state-fd=4",
		"--ready-fd=5",
	}
	if s.lock != nil && s.lock.File() != nil {
		files = append(files, s.lock.File()) // fd 6: flock file descriptor
		argv = append(argv, "--lock-fd=6")
	}

	proc, err := os.StartProcess(resolvedPath, argv, &os.ProcAttr{
		Files: files,
		Sys:   &syscall.SysProcAttr{Setsid: true},
	})

	// Close parent-side duplicates of child FDs immediately.
	_ = listenFile.Close()
	_ = stateR.Close()
	_ = readyW.Close()
	_ = devNull.Close()
	if logOut != devNull {
		_ = logOut.Close()
	}

	if err != nil {
		_ = stateW.Close()
		_ = readyR.Close()
		rollback("spawn new corrald: " + err.Error())
		return
	}

	// Stream snapshot to child asynchronously so large snapshots never block on
	// kernel pipe capacity.
	go func() {
		_, _ = stateW.Write(snapBytes)
		_ = stateW.Close()
	}()

	// 4. Wait up to ReadyTimeout (5 s) for READY line from v2.
	newPID, newRelease, readyErr := waitForReadyLine(readyR, s.cfg.ReadyTimeout)
	_ = readyR.Close()
	if readyErr != nil {
		_ = proc.Kill()
		_, _ = proc.Wait()
		rollback("new process did not become ready: " + readyErr.Error())
		return
	}

	// 5. Handoff succeeded! Mark handed off, close our copy of listener without
	// unlinking the socket, reply to caller, push daemon.reconnect, and exit.
	elapsedMS := time.Since(started).Milliseconds()
	s.stateMu.Lock()
	s.handedOff = true
	remainingWrites := s.inflightWrites
	s.stateMu.Unlock()

	s.closeListenerForExit()

	if newPID == 0 {
		newPID = proc.Pid
	}
	if newRelease == 0 {
		newRelease = p.Release
	}

	// Enqueue RPC response first, then broadcast daemon.reconnect to all clients.
	conn.enqueueResponse(req.ID, proto.HandoffResult{
		PID:        newPID,
		Release:    newRelease,
		DurationMS: elapsedMS,
	}, nil)

	reconnectData, _ := json.Marshal(proto.ReconnectEventData{
		Reason:     "hot_update",
		Seq:        lastSeq,
		NewPID:     newPID,
		NewRelease: newRelease,
	})
	s.broadcastControlEvent(proto.Event{
		Event: proto.EventDaemonReconnect,
		Epoch: epoch,
		Seq:   lastSeq,
		Data:  reconnectData,
	})

	s.log.Info("handoff completed; retiring old process",
		"new_pid", newPID,
		"new_release", newRelease,
		"duration_ms", elapsedMS,
		"remaining_writes", remainingWrites,
	)

	// Allow clients to read response + reconnect frame, and give any write that
	// exceeded the 5 s drain window up to 15 s total before exiting.
	go func() {
		if remainingWrites > 0 {
			s.waitForInflightWrites(context.Background(), s.cfg.PostHandoffWriteCap)
		}
		// Give outbound queues a brief window to flush response + reconnect frames,
		// bounded by PostHandoffExit (3 s).
		flushCap := s.cfg.PostHandoffExit
		if flushCap > 200*time.Millisecond {
			// Wait up to 200 ms for active conns to flush, or until all conns close,
			// capped at PostHandoffExit.
			deadline := time.Now().Add(flushCap)
			minWait := time.Now().Add(80 * time.Millisecond)
			for time.Now().Before(deadline) {
				if s.connectedCount() == 0 && time.Now().After(minWait) {
					break
				}
				if time.Now().After(minWait) && s.allConnsFlushed() {
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
		}
		s.RequestShutdown()
	}()
}

func (s *Server) allConnsFlushed() bool {
	s.connMu.Lock()
	defer s.connMu.Unlock()
	for c := range s.conns {
		c.mu.Lock()
		n := len(c.outCh)
		c.mu.Unlock()
		if n > 0 {
			return false
		}
	}
	return true
}

func (s *Server) waitForInflightWrites(ctx context.Context, timeout time.Duration) {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	for {
		s.stateMu.Lock()
		n := s.inflightWrites
		s.stateMu.Unlock()
		if n == 0 {
			return
		}
		select {
		case <-s.writeDoneCh:
		case <-deadline.C:
			return
		case <-ctx.Done():
			return
		}
	}
}

func (s *Server) pauseAccept() error {
	s.acceptMu.Lock()
	defer s.acceptMu.Unlock()
	if s.acceptStopped || s.ln == nil {
		return errors.New("listener already closed")
	}
	s.acceptPaused = true
	_ = s.ln.SetDeadline(time.Now())
	for s.acceptInCall && !s.acceptStopped {
		s.acceptCond.Wait()
	}
	if s.acceptStopped {
		return errors.New("accept loop stopped")
	}
	return nil
}

func (s *Server) resumeAccept() {
	s.acceptMu.Lock()
	defer s.acceptMu.Unlock()
	if s.acceptStopped || s.ln == nil {
		return
	}
	// UnixListener.File() clears O_NONBLOCK on the shared open file description.
	// Restore non-blocking mode so Go's netpoller deadlines and Close() wake
	// AcceptUnix() promptly after a rolled-back handoff.
	if raw, err := s.ln.SyscallConn(); err == nil {
		_ = raw.Control(func(fd uintptr) {
			_ = syscall.SetNonblock(int(fd), true)
		})
	}
	_ = s.ln.SetDeadline(time.Time{})
	s.acceptPaused = false
	s.acceptCond.Broadcast()
}

// ValidateHandoffBinary verifies that candidatePath resides inside
// ~/.local/lib/corral/ (after resolving symlinks on parent directories), is a
// regular file (not itself a symlink), is owned by expectedUID, has mode 0700,
// and matches expectedSHA256.
func ValidateHandoffBinary(layout *Layout, candidatePath, expectedSHA256 string, expectedUID uint32) (string, error) {
	if candidatePath == "" || !filepath.IsAbs(candidatePath) {
		return "", errors.New("handoff path must be absolute")
	}
	if strings.ContainsAny(candidatePath, "\x00\n") {
		return "", errors.New("handoff path contains invalid characters")
	}
	expectedSHA256 = strings.ToLower(strings.TrimSpace(expectedSHA256))
	if len(expectedSHA256) != 64 {
		return "", errors.New("invalid sha256 digest length")
	}
	if _, err := hex.DecodeString(expectedSHA256); err != nil {
		return "", errors.New("invalid sha256 hex digest")
	}

	// Ensure candidate is not a symlink itself.
	info, err := os.Lstat(candidatePath)
	if err != nil {
		return "", fmt.Errorf("lstat binary: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("handoff binary must not be a symlink")
	}
	if !info.Mode().IsRegular() {
		return "", errors.New("handoff binary must be a regular file")
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return "", errors.New("stat metadata unavailable")
	}
	if st.Uid != expectedUID {
		return "", fmt.Errorf("handoff binary owned by uid %d, expected %d", st.Uid, expectedUID)
	}
	if info.Mode().Perm() != 0o700 {
		return "", fmt.Errorf("handoff binary mode is %04o, want 0700", info.Mode().Perm())
	}

	// Resolve real paths of LibDir and candidatePath's parent to prevent traversal.
	realLib, err := filepath.EvalSymlinks(layout.LibDir)
	if err != nil {
		return "", fmt.Errorf("resolve lib dir: %w", err)
	}
	realBin, err := filepath.EvalSymlinks(candidatePath)
	if err != nil {
		return "", fmt.Errorf("resolve binary path: %w", err)
	}
	rel, err := filepath.Rel(realLib, realBin)
	if err != nil || rel == "." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || rel == ".." {
		return "", fmt.Errorf("handoff binary %s is outside %s", realBin, realLib)
	}

	gotHex, err := FileSHA256(realBin)
	if err != nil {
		return "", fmt.Errorf("hash binary: %w", err)
	}
	if subtle.ConstantTimeCompare([]byte(gotHex), []byte(expectedSHA256)) != 1 {
		return "", fmt.Errorf("sha256 mismatch: got %s, want %s", gotHex, expectedSHA256)
	}
	return realBin, nil
}

// FileSHA256 computes the lowercase hex SHA-256 digest of a file.
func FileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func waitForReadyLine(r *os.File, timeout time.Duration) (pid int, release int64, err error) {
	type result struct {
		line string
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		sc := bufio.NewScanner(r)
		if sc.Scan() {
			ch <- result{line: sc.Text()}
			return
		}
		if err := sc.Err(); err != nil {
			ch <- result{err: err}
			return
		}
		ch <- result{err: io.EOF}
	}()

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case res := <-ch:
		if res.err != nil {
			return 0, 0, res.err
		}
		fields := strings.Fields(res.line)
		if len(fields) == 0 || fields[0] != "READY" {
			return 0, 0, fmt.Errorf("unexpected ready signal %q", res.line)
		}
		if len(fields) >= 2 {
			pid, _ = strconv.Atoi(fields[1])
		}
		if len(fields) >= 4 {
			release, _ = strconv.ParseInt(fields[3], 10, 64)
		}
		return pid, release, nil
	case <-timer.C:
		_ = r.Close()
		return 0, 0, errors.New("timed out waiting for READY")
	}
}

// PruneOldVersions retains at most two corrald-* version directories under
// ~/.local/lib/corral/ (the current running version and the newest previous
// version) and removes older corrald-* directories. Non-corrald directories
// (such as code-server-*) are never touched.
func PruneOldVersions(layout *Layout, currentVersion string, currentRelease int64) ([]string, error) {
	entries, err := os.ReadDir(layout.LibDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	currentDirName := fmt.Sprintf("corrald-%s-%d", currentVersion, currentRelease)
	type verEntry struct {
		name    string
		path    string
		release int64
		modTime time.Time
	}
	var others []verEntry
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), "corrald-") {
			continue
		}
		if e.Name() == currentDirName {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		rel := parseVersionDirRelease(e.Name())
		others = append(others, verEntry{
			name:    e.Name(),
			path:    filepath.Join(layout.LibDir, e.Name()),
			release: rel,
			modTime: info.ModTime(),
		})
	}
	if len(others) <= 1 {
		return nil, nil
	}
	// Sort descending by release timestamp, breaking ties by modTime descending.
	sort.Slice(others, func(i, j int) bool {
		if others[i].release != others[j].release {
			return others[i].release > others[j].release
		}
		return others[i].modTime.After(others[j].modTime)
	})
	// Keep others[0] (the single previous version); delete others[1:].
	var removed []string
	for _, o := range others[1:] {
		if err := os.RemoveAll(o.path); err == nil {
			removed = append(removed, o.path)
		}
	}
	return removed, nil
}

func parseVersionDirRelease(name string) int64 {
	idx := strings.LastIndexByte(name, '-')
	if idx < 0 || idx+1 >= len(name) {
		return 0
	}
	n, err := strconv.ParseInt(name[idx+1:], 10, 64)
	if err != nil {
		return 0
	}
	return n
}
