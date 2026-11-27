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

const (
	// MaxHandoffSnapshotBytes bounds serialized handoff snapshots sent over the
	// state pipe so valid large event histories are trimmed cleanly rather than
	// truncated mid-JSON at the receiver.
	MaxHandoffSnapshotBytes = 12 << 20

	// MaxHandoffReadBytes is the hard ceiling accepted by the replacement
	// process when reading the state pipe.
	MaxHandoffReadBytes = 16 << 20
)

// HandoffSnapshot is streamed from v1 to v2 over the inherited state pipe
// during a zero-gap hot update (spec §6).
type HandoffSnapshot struct {
	Schema       int                        `json:"schema"`
	Epoch        string                     `json:"epoch"`
	LastSeq      uint64                     `json:"last_seq"`
	Events       []proto.Event              `json:"events"`
	Clients      []proto.ClientInfo         `json:"clients"`
	Ops          OpTableSnapshot            `json:"ops"`
	Modules      map[string]json.RawMessage `json:"modules,omitempty"`
	SocketPath   string                     `json:"socket_path"`
	Gateway      string                     `json:"gateway"`
	OldPID       int                        `json:"old_pid"`
	OldStartTime string                     `json:"old_start_time,omitempty"`
	OldVersion   string                     `json:"old_version"`
	OldRelease   int64                      `json:"old_release"`
}

// BuildBoundedSnapshot marshals snap within maxBytes. If the serialized
// snapshot exceeds maxBytes, oldest events in snap.Events are dropped first
// (preserving LastSeq so clients whose cursor was dropped receive
// resync_required, while keeping OpTable, Clients, and Modules intact).
func BuildBoundedSnapshot(snap HandoffSnapshot, maxBytes int) ([]byte, error) {
	if maxBytes <= 0 {
		maxBytes = MaxHandoffSnapshotBytes
	}
	b, err := json.Marshal(snap)
	if err != nil {
		return nil, err
	}
	if len(b) <= maxBytes {
		return b, nil
	}

	// Binary search the maximum suffix of snap.Events that fits in maxBytes.
	events := snap.Events
	lo, hi := 0, len(events)
	var best []byte
	for lo <= hi {
		mid := lo + (hi-lo)/2
		candidate := snap
		if mid == 0 {
			candidate.Events = nil
		} else {
			candidate.Events = events[len(events)-mid:]
		}
		encoded, err := json.Marshal(candidate)
		if err != nil {
			return nil, err
		}
		if len(encoded) <= maxBytes {
			best = encoded
			lo = mid + 1
		} else {
			hi = mid - 1
		}
	}
	if best == nil {
		return nil, fmt.Errorf("handoff snapshot exceeds %d bytes even without event history", maxBytes)
	}
	return best, nil
}

type cutoverBackup struct {
	hasSymlink bool
	symlink    string
	hasInstall bool
	install    []byte
	hasReadme  bool
	readme     []byte
}

func captureCutoverBackup(layout *Layout) cutoverBackup {
	var bk cutoverBackup
	if target, err := os.Readlink(layout.CurrentSymlink); err == nil {
		bk.hasSymlink = true
		bk.symlink = target
	}
	if b, err := os.ReadFile(layout.InstallJSON); err == nil {
		bk.hasInstall = true
		bk.install = b
	}
	if b, err := os.ReadFile(layout.ReadmePath); err == nil {
		bk.hasReadme = true
		bk.readme = b
	}
	return bk
}

func restoreCutoverBackup(layout *Layout, bk cutoverBackup, meta *MetaRecord) {
	if bk.hasSymlink {
		_ = AtomicSymlink(bk.symlink, layout.CurrentSymlink)
	}
	if bk.hasInstall {
		_ = WriteFileAtomic(layout.InstallJSON, bk.install, 0o600)
	}
	if bk.hasReadme {
		_ = WriteFileAtomic(layout.ReadmePath, bk.readme, 0o644)
	}
	if meta != nil {
		_ = WriteMetaRecord(layout.MetaJSON, meta)
	}
}

func (s *Server) handleHandoff(_ context.Context, _ *Conn, _ json.RawMessage) (any, *proto.RPCError) {
	return nil, &proto.RPCError{Code: proto.ErrInternal, Message: "handoff called outside request context"}
}

func (s *Server) dispatchHandoff(ctx context.Context, conn *Conn, req proto.Request) {
	started := time.Now()
	var p proto.HandoffParams
	if err := json.Unmarshal(req.Params, &p); err != nil {
		conn.enqueueResponse(req.ID, nil, &proto.RPCError{Code: proto.ErrInvalidParams, Message: err.Error()})
		return
	}

	// Enforce monotonic upgrades (spec §2: never downgrade or re-handoff to same release).
	if !proto.ShouldUpgrade(s.build.Release, p.Release) {
		conn.enqueueResponse(req.ID, nil, &proto.RPCError{
			Code:    proto.ErrNotNewer,
			Message: fmt.Sprintf("candidate release %d is not newer than current release %d", p.Release, s.build.Release),
		})
		return
	}

	// Withdrawal-first: refuse handoff if uninstall tombstone is present.
	if s.layout.TombstoneExists() {
		conn.enqueueResponse(req.ID, nil, &proto.RPCError{
			Code:    proto.ErrRemoving,
			Message: "uninstall tombstone present",
		})
		return
	}

	// 1. Validate binary path, parent dir ownership/mode, file FD ownership/mode
	// 0700, directory release match, and SHA-256 on the open FD BEFORE draining.
	// Holding binFile open pins the validated inode until execve("/proc/self/fd/6").
	binFile, resolvedPath, dirRelease, err := OpenValidatedHandoffBinary(s.layout, p.Path, p.SHA256, s.expectedUID)
	if err != nil {
		s.log.Warn("handoff binary validation failed", "path", p.Path, "err", err)
		conn.enqueueResponse(req.ID, nil, &proto.RPCError{
			Code:    proto.ErrInvalidParams,
			Message: err.Error(),
		})
		return
	}
	defer binFile.Close()

	if dirRelease != p.Release {
		conn.enqueueResponse(req.ID, nil, &proto.RPCError{
			Code:    proto.ErrInvalidParams,
			Message: fmt.Sprintf("handoff release %d does not match version directory release %d", p.Release, dirRelease),
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

	backup := captureCutoverBackup(s.layout)
	v1Meta := s.currentMetaRecord()

	rollback := func(reason string) {
		restoreCutoverBackup(s.layout, backup, v1Meta)
		s.bus.Unfreeze()
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

	// Re-check tombstone after draining.
	if s.layout.TombstoneExists() {
		rollback("uninstall tombstone appeared during drain")
		return
	}

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
	logOut, err := OpenLogFileNoFollow(s.layout.LogPath)
	if err != nil {
		logOut = devNull
	}

	// Freeze the EventBus atomically with capturing the snapshot so v1 can never
	// allocate sequence numbers under the same epoch concurrently with v2.
	epoch, lastSeq, events := s.bus.FreezeAndSnapshot()
	snap := HandoffSnapshot{
		Schema:       HandoffSnapshotSchema,
		Epoch:        epoch,
		LastSeq:      lastSeq,
		Events:       events,
		Clients:      s.clients.List(),
		Ops:          s.ops.Snapshot(),
		Modules:      s.snapshotModuleStates(),
		SocketPath:   s.layout.SocketPath,
		Gateway:      s.cfg.Gateway,
		OldPID:       os.Getpid(),
		OldStartTime: s.procStart,
		OldVersion:   s.build.Version,
		OldRelease:   s.build.Release,
	}
	snapBytes, err := BuildBoundedSnapshot(snap, MaxHandoffSnapshotBytes)
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
		rollback("build bounded snapshot: " + err.Error())
		return
	}

	// Execute the pinned, validated file descriptor at FD 6 (/proc/self/fd/6)
	// so no TOCTOU replacement can occur between validation and execve.
	files := []*os.File{
		devNull,    // fd 0: stdin
		logOut,     // fd 1: stdout
		logOut,     // fd 2: stderr
		listenFile, // fd 3: listening Unix socket
		stateR,     // fd 4: state snapshot read end
		readyW,     // fd 5: readiness signal write end
		binFile,    // fd 6: pinned validated executable FD
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
		"--bin-fd=6",
		"--binary-path", resolvedPath,
	}
	if s.lock != nil && s.lock.File() != nil {
		files = append(files, s.lock.File()) // fd 7: flock file descriptor
		argv = append(argv, "--lock-fd=7")
	}

	proc, err := os.StartProcess("/proc/self/fd/6", argv, &os.ProcAttr{
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

	// 5. Handoff succeeded! Mark handed off, establish hard exit deadline, close
	// our copy of listener without unlinking the socket, reply to caller, push
	// daemon.reconnect, and exit within the hard cap.
	elapsedMS := time.Since(started).Milliseconds()
	s.stateMu.Lock()
	s.handedOff = true
	remainingWrites := s.inflightWrites
	hardCap := s.cfg.PostHandoffExit
	if remainingWrites > 0 {
		hardCap = s.cfg.PostHandoffWriteCap
	}
	hardDeadline := time.Now().Add(hardCap)
	s.hardExitDeadline = hardDeadline
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
		"hard_cap", hardCap,
	)

	go func() {
		if remainingWrites > 0 {
			if rem := time.Until(hardDeadline) - 100*time.Millisecond; rem > 0 {
				s.waitForInflightWrites(context.Background(), rem)
			}
		}
		flushLimit := time.Now().Add(s.cfg.PostHandoffExit)
		if flushLimit.After(hardDeadline) {
			flushLimit = hardDeadline
		}
		minWait := time.Now().Add(80 * time.Millisecond)
		if minWait.After(flushLimit) {
			minWait = flushLimit
		}
		for time.Now().Before(flushLimit) {
			if time.Now().After(minWait) && (s.connectedCount() == 0 || s.allConnsFlushed()) {
				break
			}
			time.Sleep(10 * time.Millisecond)
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

// OpenValidatedHandoffBinary opens candidatePath without following symlinks
// (O_NOFOLLOW), validates LibDir and version parent directory ownership and
// permissions, verifies the strict "corrald-<Version>-<Release>/corrald"
// layout, validates the open FD's type/owner/0700 mode, and checks SHA-256 on
// the open FD itself. The caller owns the returned *os.File and should execute
// it via /proc/self/fd/<N> to prevent TOCTOU swaps.
func OpenValidatedHandoffBinary(layout *Layout, candidatePath, expectedSHA256 string, expectedUID uint32) (*os.File, string, int64, error) {
	if candidatePath == "" || !filepath.IsAbs(candidatePath) {
		return nil, "", 0, errors.New("handoff path must be absolute")
	}
	if strings.ContainsAny(candidatePath, "\x00\n") {
		return nil, "", 0, errors.New("handoff path contains invalid characters")
	}
	expectedSHA256 = strings.ToLower(strings.TrimSpace(expectedSHA256))
	if len(expectedSHA256) != 64 {
		return nil, "", 0, errors.New("invalid sha256 digest length")
	}
	if _, err := hex.DecodeString(expectedSHA256); err != nil {
		return nil, "", 0, errors.New("invalid sha256 hex digest")
	}

	// Verify LibDir is a real directory owned by expectedUID and not group/other writable.
	if err := CheckNonWritableDir(layout.LibDir, expectedUID); err != nil {
		return nil, "", 0, fmt.Errorf("lib dir check failed: %w", err)
	}

	cleanCand := filepath.Clean(candidatePath)
	parentDir := filepath.Dir(cleanCand)
	if filepath.Dir(parentDir) != filepath.Clean(layout.LibDir) {
		return nil, "", 0, fmt.Errorf("handoff binary %s is not directly inside a version dir under %s", cleanCand, layout.LibDir)
	}
	if filepath.Base(cleanCand) != "corrald" {
		return nil, "", 0, fmt.Errorf("handoff binary basename must be corrald, got %q", filepath.Base(cleanCand))
	}
	_, dirRelease, ok := ParseVersionDirName(filepath.Base(parentDir))
	if !ok {
		return nil, "", 0, fmt.Errorf("handoff parent %q is not a valid corrald-<version>-<release> directory", filepath.Base(parentDir))
	}

	// Verify version parent directory is a real directory (not symlink), owned by
	// expectedUID, and strictly 0700 (no group/other write or access).
	pinfo, err := os.Lstat(parentDir)
	if err != nil {
		return nil, "", 0, fmt.Errorf("lstat parent dir: %w", err)
	}
	if pinfo.Mode()&os.ModeSymlink != 0 || !pinfo.IsDir() {
		return nil, "", 0, errors.New("handoff parent directory must be a real directory")
	}
	pst, ok := pinfo.Sys().(*syscall.Stat_t)
	if !ok || pst.Uid != expectedUID {
		return nil, "", 0, fmt.Errorf("handoff parent directory owned by uid %d, expected %d", pst.Uid, expectedUID)
	}
	if pinfo.Mode().Perm() != 0o700 {
		return nil, "", 0, fmt.Errorf("handoff parent directory mode is %04o, want 0700", pinfo.Mode().Perm())
	}

	// Open the candidate with O_NOFOLLOW so a symlink cannot be substituted.
	f, err := os.OpenFile(cleanCand, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, "", 0, fmt.Errorf("open handoff binary: %w", err)
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, "", 0, fmt.Errorf("fstat binary: %w", err)
	}
	if !info.Mode().IsRegular() {
		_ = f.Close()
		return nil, "", 0, errors.New("handoff binary must be a regular file")
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		_ = f.Close()
		return nil, "", 0, errors.New("stat metadata unavailable")
	}
	if st.Uid != expectedUID {
		_ = f.Close()
		return nil, "", 0, fmt.Errorf("handoff binary owned by uid %d, expected %d", st.Uid, expectedUID)
	}
	if info.Mode().Perm() != 0o700 {
		_ = f.Close()
		return nil, "", 0, fmt.Errorf("handoff binary mode is %04o, want 0700", info.Mode().Perm())
	}

	// Hash the already-open file descriptor directly.
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		_ = f.Close()
		return nil, "", 0, fmt.Errorf("hash binary fd: %w", err)
	}
	gotHex := hex.EncodeToString(h.Sum(nil))
	if subtle.ConstantTimeCompare([]byte(gotHex), []byte(expectedSHA256)) != 1 {
		_ = f.Close()
		return nil, "", 0, fmt.Errorf("sha256 mismatch: got %s, want %s", gotHex, expectedSHA256)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		_ = f.Close()
		return nil, "", 0, fmt.Errorf("seek binary fd: %w", err)
	}
	return f, cleanCand, dirRelease, nil
}

// ValidateHandoffBinary validates candidatePath and closes the pinned file
// descriptor. Callers executing the binary should use OpenValidatedHandoffBinary.
func ValidateHandoffBinary(layout *Layout, candidatePath, expectedSHA256 string, expectedUID uint32) (string, error) {
	f, resolved, _, err := OpenValidatedHandoffBinary(layout, candidatePath, expectedSHA256, expectedUID)
	if err != nil {
		return "", err
	}
	_ = f.Close()
	return resolved, nil
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

// PruneOldVersions retains the current running version and the single newest
// healthy previous version (release < currentRelease and not in failed.json),
// while removing older managed versions and recorded failed candidates.
//
// Only version directories matching the strict "corrald-<Version>-<Release>"
// format AND recorded in install.json are eligible for deletion; arbitrary
// directories (such as corrald-backups or code-server-*) and versions actively
// referenced by other machines in a shared NFS home are never touched.
func PruneOldVersions(layout *Layout, currentVersion string, currentRelease int64) ([]string, error) {
	// Skip non-blockingly if an install/uninstall currently holds the shared lock.
	sharedLock, err := AcquireLock(layout.SharedLockPath)
	if err != nil {
		if errors.Is(err, ErrLocked) {
			return nil, nil
		}
		// If LibDir doesn't exist yet, nothing to prune.
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	defer sharedLock.Release()

	installRec, err := ReadInstallRecord(layout.InstallJSON)
	if err != nil {
		// Without an authoritative install manifest, never guess which dirs to remove.
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}

	// Build the set of version directories explicitly recorded as managed by corrald.
	managed := make(map[string]struct{})
	for _, f := range installRec.Files {
		clean := filepath.Clean(f)
		if filepath.Dir(clean) == filepath.Clean(layout.LibDir) {
			if _, _, ok := ParseVersionDirName(filepath.Base(clean)); ok {
				managed[filepath.Base(clean)] = struct{}{}
			}
		}
	}
	if len(managed) == 0 {
		return nil, nil
	}

	// Collect version dirs currently referenced by other machines sharing $HOME.
	inUseByOthers, err := activeVersionDirsOfOtherMachines(layout)
	if err != nil {
		// Conservative on NFS / I/O errors: do not delete shared versions.
		return nil, err
	}

	// Build failed release set from failed.json.
	failedSet := make(map[int64]bool)
	if frec, err := ReadFailedRecord(layout.FailedJSON); err == nil {
		for _, fe := range frec.Releases {
			failedSet[fe.Release] = true
		}
	}

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
	var healthyPrev []verEntry
	var failedDirs []verEntry

	for _, e := range entries {
		name := e.Name()
		if !e.IsDir() || name == currentDirName {
			continue
		}
		if _, isManaged := managed[name]; !isManaged {
			continue
		}
		if inUseByOthers[name] {
			continue
		}
		_, rel, ok := ParseVersionDirName(name)
		if !ok {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		ve := verEntry{
			name:    name,
			path:    filepath.Join(layout.LibDir, name),
			release: rel,
			modTime: info.ModTime(),
		}
		if failedSet[rel] {
			failedDirs = append(failedDirs, ve)
			continue
		}
		if rel < currentRelease {
			healthyPrev = append(healthyPrev, ve)
		}
		// Note: non-failed directories with rel >= currentRelease are left alone
		// (e.g. a freshly staged upgrade awaiting handoff).
	}

	// Sort healthy previous versions descending by release so healthyPrev[0] is
	// the newest valid rollback version.
	sort.Slice(healthyPrev, func(i, j int) bool {
		if healthyPrev[i].release != healthyPrev[j].release {
			return healthyPrev[i].release > healthyPrev[j].release
		}
		return healthyPrev[i].modTime.After(healthyPrev[j].modTime)
	})

	var toRemove []verEntry
	toRemove = append(toRemove, failedDirs...)
	if len(healthyPrev) > 1 {
		toRemove = append(toRemove, healthyPrev[1:]...)
	}
	if len(toRemove) == 0 {
		return nil, nil
	}

	removedSet := make(map[string]struct{}, len(toRemove))
	var removed []string
	for _, o := range toRemove {
		if err := os.RemoveAll(o.path); err == nil {
			removed = append(removed, o.path)
			removedSet[o.path] = struct{}{}
		}
	}

	// Update install.json to drop removed paths.
	if len(removedSet) > 0 {
		kept := make([]string, 0, len(installRec.Files))
		for _, f := range installRec.Files {
			clean := filepath.Clean(f)
			if _, drop := removedSet[clean]; drop {
				continue
			}
			if _, drop := removedSet[filepath.Dir(clean)]; drop {
				continue
			}
			kept = append(kept, f)
		}
		installRec.Files = kept
		_ = WriteInstallRecord(layout.InstallJSON, installRec)
	}
	return removed, nil
}

func activeVersionDirsOfOtherMachines(layout *Layout) (map[string]bool, error) {
	inUse := make(map[string]bool)
	entries, err := os.ReadDir(layout.StateRoot)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return inUse, nil
		}
		return nil, err
	}
	for _, e := range entries {
		if !e.IsDir() || e.Name() == layout.MachineID {
			continue
		}
		dir := filepath.Join(layout.StateRoot, e.Name())
		if rec, err := ReadInstallRecord(filepath.Join(dir, "install.json")); err == nil {
			inUse[fmt.Sprintf("corrald-%s-%d", rec.Version, rec.Release)] = true
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		if meta, err := ReadMetaRecord(filepath.Join(dir, "meta.json")); err == nil {
			inUse[fmt.Sprintf("corrald-%s-%d", meta.Version, meta.Release)] = true
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
	}
	return inUse, nil
}
