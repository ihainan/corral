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
// Snapshots with any other schema version are rejected so incompatible upgrades
// fail closed.
const HandoffSnapshotSchema = 2

const (
	// MaxHandoffSnapshotBytes bounds serialized handoff snapshots sent over the
	// state pipe.
	MaxHandoffSnapshotBytes = 12 << 20

	// MaxHandoffReadBytes is the hard ceiling accepted by the replacement
	// process when reading the state pipe.
	MaxHandoffReadBytes = 16 << 20
)

// HandoffSnapshot is streamed from v1 to v2 over the inherited state pipe
// during a zero-gap hot update (spec §6). Hot updates always start a fresh
// epoch on v2 (clients full-resync on reconnect), while Clients, OpTable, and
// Modules are carried across the upgrade boundary.
type HandoffSnapshot struct {
	Schema       int                        `json:"schema"`
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

// BuildBoundedSnapshot marshals snap and enforces maxBytes.
func BuildBoundedSnapshot(snap HandoffSnapshot, maxBytes int) ([]byte, error) {
	if maxBytes <= 0 {
		maxBytes = MaxHandoffSnapshotBytes
	}
	b, err := json.Marshal(snap)
	if err != nil {
		return nil, err
	}
	if len(b) > maxBytes {
		return nil, fmt.Errorf("handoff snapshot (%d bytes) exceeds limit (%d bytes)", len(b), maxBytes)
	}
	return b, nil
}

type cutoverBackup struct {
	hasSymlink bool
	symlink    string
	hasInstall bool
	install    []byte
	hasReadme  bool
	readme     []byte
}

// captureCutoverBackup captures all persistent files touched during handoff
// cutover. Must be called under SharedLock -> OpLock. Any read error other
// than os.ErrNotExist is returned so handoff aborts cleanly instead of
// proceeding with an incomplete backup.
func captureCutoverBackup(layout *Layout) (cutoverBackup, error) {
	var bk cutoverBackup

	if target, err := os.Readlink(layout.CurrentSymlink); err == nil {
		bk.hasSymlink = true
		bk.symlink = target
	} else if !errors.Is(err, os.ErrNotExist) {
		return bk, fmt.Errorf("readlink %s: %w", layout.CurrentSymlink, err)
	}

	if b, err := os.ReadFile(layout.InstallJSON); err == nil {
		bk.hasInstall = true
		bk.install = b
	} else if !errors.Is(err, os.ErrNotExist) {
		return bk, fmt.Errorf("read %s: %w", layout.InstallJSON, err)
	}

	if b, err := os.ReadFile(layout.ReadmePath); err == nil {
		bk.hasReadme = true
		bk.readme = b
	} else if !errors.Is(err, os.ErrNotExist) {
		return bk, fmt.Errorf("read %s: %w", layout.ReadmePath, err)
	}

	return bk, nil
}

// restoreCutoverBackup unconditionally restores the backup captured before
// handoff. Because SharedLock -> OpLock are held continuously from
// captureCutoverBackup through restoreCutoverBackup, no other host or process
// can modify these files in between. Any restore error is returned so callers
// surface rollback_failed.
func restoreCutoverBackup(layout *Layout, bk cutoverBackup, v1Meta *MetaRecord) error {
	var errs []string

	if bk.hasSymlink {
		if err := AtomicSymlink(bk.symlink, layout.CurrentSymlink); err != nil {
			errs = append(errs, fmt.Sprintf("restore symlink: %v", err))
		}
	} else {
		if err := os.Remove(layout.CurrentSymlink); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, fmt.Sprintf("remove symlink: %v", err))
		}
	}

	if bk.hasInstall {
		if err := WriteFileAtomic(layout.InstallJSON, bk.install, 0o600); err != nil {
			errs = append(errs, fmt.Sprintf("restore install.json: %v", err))
		}
	} else {
		if err := os.Remove(layout.InstallJSON); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, fmt.Sprintf("remove install.json: %v", err))
		}
	}

	if bk.hasReadme {
		if err := WriteFileAtomic(layout.ReadmePath, bk.readme, 0o644); err != nil {
			errs = append(errs, fmt.Sprintf("restore README.txt: %v", err))
		}
	} else {
		if err := os.Remove(layout.ReadmePath); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, fmt.Sprintf("remove README.txt: %v", err))
		}
	}

	if v1Meta != nil {
		if err := WriteMetaRecord(layout.MetaJSON, v1Meta); err != nil {
			errs = append(errs, fmt.Sprintf("restore meta.json: %v", err))
		}
	}

	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "; "))
	}
	return nil
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

	if !proto.ShouldUpgrade(s.build.Release, p.Release) {
		conn.enqueueResponse(req.ID, nil, &proto.RPCError{
			Code:    proto.ErrNotNewer,
			Message: fmt.Sprintf("candidate release %d is not newer than current release %d", p.Release, s.build.Release),
		})
		return
	}

	// Rule 1 & Round-4 #3: Serialize handoff with install, uninstall, GC, and
	// other hosts using SharedLock -> OpLock held across the entire handoff
	// transaction. Any stat or lock error fails closed; locking is never skipped.
	if _, statErr := os.Lstat(s.layout.LibDir); statErr != nil {
		conn.enqueueResponse(req.ID, nil, &proto.RPCError{
			Code:    proto.ErrHandoffFailed,
			Message: fmt.Sprintf("stat lib dir %s: %v", s.layout.LibDir, statErr),
		})
		return
	}
	sharedLock, err := AcquireLockWait(s.layout.SharedLockPath, s.cfg.LockTimeout)
	if err != nil {
		code := proto.ErrHandoffFailed
		retryable := false
		if errors.Is(err, ErrLocked) {
			code = proto.ErrBusy
			retryable = true
		}
		conn.enqueueResponse(req.ID, nil, &proto.RPCError{
			Code:      code,
			Message:   err.Error(),
			Retryable: retryable,
		})
		return
	}
	var opLock *FileLock
	releaseLifecycleLocks := func() {
		if opLock != nil {
			_ = opLock.Release()
			opLock = nil
		}
		if sharedLock != nil {
			_ = sharedLock.Release()
			sharedLock = nil
		}
	}
	defer releaseLifecycleLocks()

	if _, statErr := os.Lstat(s.layout.StateDir); statErr != nil {
		conn.enqueueResponse(req.ID, nil, &proto.RPCError{
			Code:    proto.ErrHandoffFailed,
			Message: fmt.Sprintf("stat state dir %s: %v", s.layout.StateDir, statErr),
		})
		return
	}
	opLock, err = AcquireLockWait(s.layout.OpLockPath, s.cfg.LockTimeout)
	if err != nil {
		code := proto.ErrHandoffFailed
		retryable := false
		if errors.Is(err, ErrLocked) {
			code = proto.ErrBusy
			retryable = true
		}
		conn.enqueueResponse(req.ID, nil, &proto.RPCError{
			Code:      code,
			Message:   err.Error(),
			Retryable: retryable,
		})
		return
	}

	if s.layout.TombstoneExists() {
		conn.enqueueResponse(req.ID, nil, &proto.RPCError{
			Code:    proto.ErrRemoving,
			Message: "uninstall tombstone present",
		})
		return
	}

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

	// Capture backup under SharedLock -> OpLock BEFORE entering draining.
	backup, err := captureCutoverBackup(s.layout)
	if err != nil {
		conn.enqueueResponse(req.ID, nil, &proto.RPCError{
			Code:    proto.ErrHandoffFailed,
			Message: "capture handoff backup: " + err.Error(),
		})
		return
	}

	// Enter draining atomically.
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

	listenerLocked := false
	releaseListenerLock := func() {
		if listenerLocked {
			listenerLocked = false
			s.listenerOpMu.Unlock()
		}
	}

	rollback := func(reason string) {
		// Rule 2: Rotate epoch on rollback so all clients resync cleanly.
		s.bus.RotateEpoch()
		v1Meta := s.currentMetaRecord()

		// Rule 1 & Rule 5: Unconditionally restore backup under held
		// SharedLock -> OpLock; any restore error marks rollback_failed.
		restoreErr := restoreCutoverBackup(s.layout, backup, v1Meta)

		s.resumeAccept()
		s.stateMu.Lock()
		s.handoffActive = false
		s.state = proto.StateRunning
		if restoreErr != nil {
			s.rollbackDegraded = restoreErr.Error()
		} else {
			s.rollbackDegraded = ""
		}
		s.stateMu.Unlock()
		releaseListenerLock()
		releaseLifecycleLocks()

		errCode := proto.ErrHandoffFailed
		fullReason := reason
		if restoreErr != nil {
			errCode = proto.ErrRollbackFailed
			fullReason = fmt.Sprintf("%s (rollback_failed: %v)", reason, restoreErr)
		}

		// Rule 2 & Round-4 #5: Enqueue error reply and disconnect all existing
		// clients BEFORE writing failed.json or logging so blocked disk/log I/O
		// can never keep stale clients attached after epoch rotation.
		conn.enqueueResponse(req.ID, nil, &proto.RPCError{
			Code:    errCode,
			Message: fullReason,
		})
		s.closeAllConnsExcept(conn)
		go func(c *Conn) {
			time.Sleep(30 * time.Millisecond)
			c.Close()
		}(conn)

		if p.Release > 0 {
			if recErr := RecordFailedRelease(s.layout.FailedJSON, p.Release, fullReason); recErr != nil {
				s.log.Warn("record failed release error", "release", p.Release, "err", recErr)
			}
		}
		s.log.Error("handoff failed; rotated epoch and closed clients", "release", p.Release, "reason", fullReason)
	}

	// Wait up to DrainTimeout (5 s) for in-flight mutating RPCs to finish.
	s.waitForInflightWrites(ctx, s.cfg.DrainTimeout)

	// Acquire listenerOpMu to exclude concurrent socket rebinding across the
	// entire pause -> dup -> spawn -> READY/rollback sequence.
	s.listenerOpMu.Lock()
	listenerLocked = true

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

	snap := HandoffSnapshot{
		Schema:       HandoffSnapshotSchema,
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
		lockFD := len(files)
		files = append(files, s.lock.File())
		argv = append(argv, fmt.Sprintf("--lock-fd=%d", lockFD))
	}
	// Round-4 #2: Pass SharedLock and OpLock FDs to child so the kernel flock
	// remains held across cutover even if parent v1 is SIGKILL'd before READY.
	if sharedLock != nil && sharedLock.File() != nil {
		sharedFD := len(files)
		files = append(files, sharedLock.File())
		argv = append(argv, fmt.Sprintf("--shared-lock-fd=%d", sharedFD))
	}
	if opLock != nil && opLock.File() != nil {
		opFD := len(files)
		files = append(files, opLock.File())
		argv = append(argv, fmt.Sprintf("--op-lock-fd=%d", opFD))
	}

	proc, err := os.StartProcess("/proc/self/fd/6", argv, &os.ProcAttr{
		Files: files,
		Sys:   &syscall.SysProcAttr{Setsid: true},
	})

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

	go func() {
		_, _ = stateW.Write(snapBytes)
		_ = stateW.Close()
	}()

	newPID, newRelease, readyErr := waitForReadyLine(readyR, s.cfg.ReadyTimeout)
	_ = readyR.Close()
	if readyErr != nil {
		_ = proc.Kill()
		_, _ = proc.Wait()
		rollback("new process did not become ready: " + readyErr.Error())
		return
	}

	// Rule 3 & Round-4 #4: The instant READY arrives — BEFORE any logging, lock
	// release, or other I/O — arm the hard-exit timer. It is never cancelled so
	// even post-Serve cleanup (e.g. closing an NFS log file) remains bounded by
	// the 3s / 15s hard cap until the process exits.
	s.stateMu.Lock()
	s.handedOff = true
	remainingWrites := s.inflightWrites
	hardCap := s.cfg.PostHandoffExit
	if remainingWrites > 0 {
		hardCap = s.cfg.PostHandoffWriteCap
	}
	hardExitFn := s.cfg.OnHardExit
	if hardExitFn == nil {
		hardExitFn = func() { os.Exit(0) }
	}
	time.AfterFunc(hardCap, hardExitFn)
	s.stateMu.Unlock()

	s.closeListenerForExit()
	releaseListenerLock()
	releaseLifecycleLocks()

	elapsedMS := time.Since(started).Milliseconds()
	if newPID == 0 {
		newPID = proc.Pid
	}
	if newRelease == 0 {
		newRelease = p.Release
	}

	conn.enqueueResponse(req.ID, proto.HandoffResult{
		PID:        newPID,
		Release:    newRelease,
		DurationMS: elapsedMS,
	}, nil)

	curEpoch := s.bus.Epoch()
	curSeq := s.bus.CurrentSeq()
	reconnectData, _ := json.Marshal(proto.ReconnectEventData{
		Reason:     "hot_update",
		Seq:        curSeq,
		NewPID:     newPID,
		NewRelease: newRelease,
	})
	s.broadcastControlEvent(proto.Event{
		Event: proto.EventDaemonReconnect,
		Epoch: curEpoch,
		Seq:   curSeq,
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
		hardDeadline := time.Now().Add(hardCap)
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
// the open FD itself.
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

// PruneOldVersions removes old managed corrald version directories only when
// this machine is the sole machine directory under StateRoot and no lifecycle
// operation holds SharedLock / OpLock.
func PruneOldVersions(layout *Layout, currentVersion string, currentRelease int64) ([]string, error) {
	sharedLock, err := AcquireLock(layout.SharedLockPath)
	if err != nil {
		if errors.Is(err, ErrLocked) || errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	defer sharedLock.Release()

	opLock, err := AcquireLock(layout.OpLockPath)
	if err != nil {
		if errors.Is(err, ErrLocked) || errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	defer opLock.Release()

	// Never prune if another live process (e.g. a newly handed-off replacement)
	// already owns meta.json.
	if meta, err := ReadMetaRecord(layout.MetaJSON); err == nil && meta.PID > 0 && meta.PID != os.Getpid() && ProcessMatches(meta.PID, meta.StartTime) {
		return nil, nil
	}

	// Conservative NFS rule: if ANY other machine directory exists under
	// StateRoot (or StateRoot cannot be read), do not delete any version dir.
	stateEntries, err := os.ReadDir(layout.StateRoot)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	for _, se := range stateEntries {
		if se.IsDir() && se.Name() != layout.MachineID {
			return nil, nil
		}
	}

	installRec, err := ReadInstallRecord(layout.InstallJSON)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	if installRec.Release > 0 && installRec.Release != currentRelease {
		return nil, nil
	}

	linkedDirName := ""
	if target, err := os.Readlink(layout.CurrentSymlink); err == nil {
		linkedDirName = filepath.Base(filepath.Dir(filepath.Clean(target)))
	}

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
		if !e.IsDir() || name == currentDirName || (linkedDirName != "" && name == linkedDirName) {
			continue
		}
		if _, isManaged := managed[name]; !isManaged {
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
	}

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
