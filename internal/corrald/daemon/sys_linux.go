//go:build linux

package daemon

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// ErrLocked is returned when another corrald instance already holds the flock.
var ErrLocked = errors.New("corrald lock is held by another process")

// FileLock wraps an exclusive Linux flock(2) on corrald.lock.
type FileLock struct {
	file *os.File
}

// AcquireLock opens path (0600, refusing symlinks), acquires a non-blocking
// exclusive flock, and verifies that path still refers to the same inode as
// the locked descriptor. If another process unlinked or replaced the lock file
// between open and flock, AcquireLock releases the stale descriptor and retries.
func AcquireLock(path string) (*FileLock, error) {
	const maxInodeRetries = 64
	for attempt := 0; attempt < maxInodeRetries; attempt++ {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0o600)
		if err != nil {
			return nil, err
		}
		finfo, err := f.Stat()
		if err != nil {
			_ = f.Close()
			return nil, err
		}
		if !finfo.Mode().IsRegular() {
			_ = f.Close()
			return nil, fmt.Errorf("lock path %s is not a regular file", path)
		}
		for {
			err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
			if errors.Is(err, syscall.EINTR) {
				continue
			}
			break
		}
		if err != nil {
			_ = f.Close()
			if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
				return nil, ErrLocked
			}
			return nil, fmt.Errorf("flock %s: %w", path, err)
		}
		pinfo, err := os.Lstat(path)
		if err == nil && os.SameFile(finfo, pinfo) {
			return &FileLock{file: f}, nil
		}
		// Path was unlinked or replaced while acquiring flock; drop stale inode and retry.
		_ = f.Close()
		if attempt >= 4 {
			time.Sleep(5 * time.Millisecond)
		}
	}
	return nil, fmt.Errorf("flock %s: path changed repeatedly while locking", path)
}

// AcquireDirLockWait ensures the lock's parent directory via ensureDir and
// acquires an exclusive inode-verified flock on path, retrying on ErrLocked or
// ENOENT (e.g. when a concurrent uninstall removes the lock file and empty
// parent directory right as this operation starts).
func AcquireDirLockWait(ensureDir func() error, path string, timeout time.Duration) (*FileLock, error) {
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	deadline := time.Now().Add(timeout)
	for {
		if ensureDir != nil {
			if err := ensureDir(); err != nil {
				return nil, err
			}
		}
		lock, err := AcquireLock(path)
		if err == nil {
			return lock, nil
		}
		if !errors.Is(err, ErrLocked) && !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		if time.Now().After(deadline) {
			if errors.Is(err, os.ErrNotExist) {
				return nil, fmt.Errorf("timed out waiting for lock %s: %w", path, ErrLocked)
			}
			return nil, fmt.Errorf("timed out waiting for lock %s: %w", path, ErrLocked)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// AcquireLockWait acquires an exclusive flock on path, retrying up to timeout
// if currently held. Used to serialize short lifecycle transitions (install,
// serve startup, uninstall, shared NFS cleanup).
func AcquireLockWait(path string, timeout time.Duration) (*FileLock, error) {
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	deadline := time.Now().Add(timeout)
	for {
		lock, err := AcquireLock(path)
		if err == nil {
			return lock, nil
		}
		if !errors.Is(err, ErrLocked) {
			return nil, err
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("timed out waiting for lock %s: %w", path, ErrLocked)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// AdoptLock wraps an inherited file descriptor that already holds the flock
// (passed across hot-update handoff via ExtraFiles / ProcAttr.Files).
func AdoptLock(fd uintptr) (*FileLock, error) {
	f := os.NewFile(fd, "corrald.lock")
	if f == nil {
		return nil, errors.New("invalid lock fd")
	}
	return &FileLock{file: f}, nil
}

// File returns the underlying *os.File for passing across handoff.
func (l *FileLock) File() *os.File {
	if l == nil {
		return nil
	}
	return l.file
}

// Release releases the lock by closing the file descriptor.
func (l *FileLock) Release() error {
	if l == nil || l.file == nil {
		return nil
	}
	f := l.file
	l.file = nil
	return f.Close()
}

// CheckPeerCred verifies via SO_PEERCRED that the remote peer of a Unix domain
// socket connection runs as expectedUID.
func CheckPeerCred(conn *net.UnixConn, expectedUID uint32) (uid uint32, pid int, err error) {
	raw, err := conn.SyscallConn()
	if err != nil {
		return 0, 0, fmt.Errorf("syscall conn: %w", err)
	}
	var cred *syscall.Ucred
	var sockErr error
	if err := raw.Control(func(fd uintptr) {
		cred, sockErr = syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
	}); err != nil {
		return 0, 0, fmt.Errorf("raw control: %w", err)
	}
	if sockErr != nil {
		return 0, 0, fmt.Errorf("SO_PEERCRED: %w", sockErr)
	}
	if err := VerifyPeerUID(cred.Uid, expectedUID); err != nil {
		return cred.Uid, int(cred.Pid), err
	}
	return cred.Uid, int(cred.Pid), nil
}

// VerifyPeerUID checks that peerUID matches expectedUID.
func VerifyPeerUID(peerUID, expectedUID uint32) error {
	if peerUID != expectedUID {
		return fmt.Errorf("peer uid %d does not match expected uid %d", peerUID, expectedUID)
	}
	return nil
}

// ProcStartTime reads /proc/<pid>/stat and returns field 22 (starttime in
// clock ticks since boot) and whether the process is alive (not a zombie).
func ProcStartTime(pid int) (startTime string, alive bool, err error) {
	if pid <= 0 {
		return "", false, fmt.Errorf("invalid pid %d", pid)
	}
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return "", false, err
	}
	return parseProcStat(string(data))
}

func parseProcStat(raw string) (string, bool, error) {
	// Field 2 (comm) is wrapped in parentheses and may itself contain spaces or
	// parentheses, so locate the final ')'.
	idx := strings.LastIndexByte(raw, ')')
	if idx < 0 || idx+2 >= len(raw) {
		return "", false, errors.New("malformed /proc/pid/stat")
	}
	fields := strings.Fields(raw[idx+2:])
	// fields[0] is field 3 (state); field 22 (starttime) is fields[19].
	if len(fields) < 20 {
		return "", false, errors.New("truncated /proc/pid/stat")
	}
	state := fields[0]
	startTime := fields[19]
	return startTime, state != "Z" && state != "X", nil
}

// ProcessMatches reports whether pid is currently alive (non-zombie) and has
// the exact /proc/<pid>/stat field-22 start time expected.
func ProcessMatches(pid int, expectedStartTime string) bool {
	if pid <= 0 || expectedStartTime == "" {
		return false
	}
	st, alive, err := ProcStartTime(pid)
	return err == nil && alive && st == expectedStartTime
}

// ProcRSSKB reads VmRSS in KiB from /proc/<pid>/status (0 for current process).
func ProcRSSKB(pid int) int64 {
	p := "/proc/self/status"
	if pid > 0 {
		p = fmt.Sprintf("/proc/%d/status", pid)
	}
	f, err := os.Open(p)
	if err != nil {
		return 0
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "VmRSS:") {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				if kb, err := strconv.ParseInt(fields[1], 10, 64); err == nil {
					return kb
				}
			}
			break
		}
	}
	return 0
}

// FileInode returns the (dev, ino) pair for path via os.Stat.
func FileInode(path string) (uint64, uint64, error) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, 0, err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, errors.New("stat metadata unavailable")
	}
	return uint64(st.Dev), st.Ino, nil
}

// RandomHex returns n random bytes formatted as 2*n lowercase hex characters.
func RandomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(fmt.Sprintf("crypto/rand failed: %v", err))
	}
	return hex.EncodeToString(b)
}

// WriteFileAtomic writes data to a temporary file in the same directory as
// path, fsyncs it, chmods it to perm, and atomically renames it into place.
// It refuses to write if the parent directory or an existing target is a symlink.
func WriteFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if dinfo, err := os.Lstat(dir); err != nil {
		return err
	} else if dinfo.Mode()&os.ModeSymlink != 0 || !dinfo.IsDir() {
		return fmt.Errorf("parent directory %s is not a real directory", dir)
	}
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("refusing to write over symlink %s", path)
		}
	}
	tmp := filepath.Join(dir, fmt.Sprintf(".%s.tmp.%d.%s", filepath.Base(path), os.Getpid(), RandomHex(4)))
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY|syscall.O_NOFOLLOW, perm)
	if err != nil {
		return err
	}
	cleanup := func() {
		_ = f.Close()
		_ = os.Remove(tmp)
	}
	if _, err := f.Write(data); err != nil {
		cleanup()
		return err
	}
	if err := f.Sync(); err != nil {
		cleanup()
		return err
	}
	if err := f.Chmod(perm); err != nil {
		cleanup()
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// AtomicSymlink replaces linkPath with a symlink pointing to target by
// creating a sibling temporary symlink and renaming it over linkPath.
func AtomicSymlink(target, linkPath string) error {
	dir := filepath.Dir(linkPath)
	tmp := filepath.Join(dir, fmt.Sprintf(".%s.tmp.%d.%s", filepath.Base(linkPath), os.Getpid(), RandomHex(4)))
	if err := os.Symlink(target, tmp); err != nil {
		return err
	}
	if err := os.Rename(tmp, linkPath); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// SetProcessName sets this process's name (/proc/self/comm, what ps, top and
// pgrep show) to name. A hot update starts the new release through
// /proc/self/fd/N, which would otherwise leave the process named "N" and
// hide it from anyone looking for corrald on the host.
func SetProcessName(name string) {
	b := make([]byte, 16)
	copy(b[:15], name)
	_, _, _ = syscall.RawSyscall(syscall.SYS_PRCTL, syscall.PR_SET_NAME, uintptr(unsafe.Pointer(&b[0])), 0)
}

func openValidatedDirLinux(dir string, uid uint32) (int, unix.Stat_t, bool) {
	var zero unix.Stat_t
	if dir == "" {
		return -1, zero, false
	}
	fd, err := unix.Open(dir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, zero, false
	}
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil || st.Mode&unix.S_IFMT != unix.S_IFDIR || st.Uid != uid {
		_ = unix.Close(fd)
		return -1, zero, false
	}
	return fd, st, true
}

func flockExistingNonBlocking(dirFD int, name string, uid uint32, deadline time.Time) (fd int, st unix.Stat_t, exists bool, err error) {
	fd, err = unix.Openat(dirFD, name, unix.O_WRONLY|unix.O_APPEND|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		if errors.Is(err, unix.ENOENT) || errors.Is(err, unix.ELOOP) || errors.Is(err, unix.EISDIR) {
			return -1, unix.Stat_t{}, false, nil
		}
		return -1, unix.Stat_t{}, false, err
	}
	if err := unix.Fstat(fd, &st); err != nil || st.Mode&unix.S_IFMT != unix.S_IFREG || st.Uid != uid {
		_ = unix.Close(fd)
		return -1, unix.Stat_t{}, false, nil
	}
	for {
		err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			return fd, st, true, nil
		}
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
			rem := time.Until(deadline)
			if rem <= 0 {
				_ = unix.Close(fd)
				return -1, unix.Stat_t{}, true, ErrLocked
			}
			step := 25 * time.Millisecond
			if rem < step {
				step = rem
			}
			time.Sleep(step)
			continue
		}
		_ = unix.Close(fd)
		return -1, unix.Stat_t{}, true, err
	}
}

// RemoveRunDirLocked safely removes ~/.local/state/corral/run only after
// acquiring exclusive flocks on every existing corrald-owned pane lock file
// inside it (in the same fixed order as senders: systemRunDir first when
// present, then runDir, sorted by filename), delegating all unlinking and
// rmdir to RemoveOwned while holding the locks.
//
// Any unowned file, symlink, or subdirectory inside run/ is left untouched and
// reported as a leftover. If any owned pane lock is currently held by an active
// sender (or new owned lock files are concurrently created), runDir is left
// intact and ErrLocked is returned so uninstall reports `busy` and retries cleanly.
func RemoveRunDirLocked(runDir, systemRunDir string, uid uint32, timeout time.Duration) error {
	_, err := RemoveRunDirLockedOwned(&Layout{RunDir: runDir, UID: uid}, systemRunDir, timeout)
	return err
}

// RemoveRunDirLockedOwned is the Layout-aware entry point returning per-file
// OwnedLeftover entries for any unowned files preserved in run/.
func RemoveRunDirLockedOwned(layout *Layout, systemRunDir string, timeout time.Duration) ([]OwnedLeftover, error) {
	if layout == nil || layout.RunDir == "" {
		return nil, nil
	}
	runDir := layout.RunDir
	uid := layout.UID
	if timeout < 0 {
		timeout = 0
	}
	deadline := time.Now().Add(timeout)

	const maxPasses = 8
	for pass := 0; pass < maxPasses; pass++ {
		rinfo, rerr := os.Lstat(runDir)
		if rerr != nil {
			if errors.Is(rerr, os.ErrNotExist) {
				return nil, nil
			}
			return nil, rerr
		}
		if rinfo.Mode()&os.ModeSymlink != 0 {
			left := []OwnedLeftover{{Path: filepath.Clean(runDir), Reason: "symlinked run dir not traversed"}}
			return left, formatOwnedLeftoverErr(left)
		}
		rst, ok := rinfo.Sys().(*syscall.Stat_t)
		if !rinfo.IsDir() || !ok || rst.Uid != uid {
			left := []OwnedLeftover{{Path: filepath.Clean(runDir), Reason: "run dir is not a user-owned directory"}}
			return left, formatOwnedLeftoverErr(left)
		}

		runDirFD, runDirSt, ok := openValidatedDirLinux(runDir, uid)
		if !ok {
			if _, err := os.Lstat(runDir); errors.Is(err, os.ErrNotExist) {
				return nil, nil
			}
			left := []OwnedLeftover{{Path: filepath.Clean(runDir), Reason: "run dir could not be opened safely"}}
			return left, formatOwnedLeftoverErr(left)
		}

		entries, err := os.ReadDir(runDir)
		if err != nil {
			_ = unix.Close(runDirFD)
			if errors.Is(err, os.ErrNotExist) {
				return nil, nil
			}
			return nil, err
		}

		sysDirFD, sysDirSt, sysOK := openValidatedDirLinux(systemRunDir, uid)
		if sysOK && sysDirSt.Dev == runDirSt.Dev && sysDirSt.Ino == runDirSt.Ino {
			_ = unix.Close(sysDirFD)
			sysDirFD = -1
			sysOK = false
		}

		var heldFDs []int
		seenOwned := make(map[string]struct{}, len(entries))
		lockedPaths := make(map[string]struct{}, len(entries))
		hasUnowned := false
		releaseAll := func() {
			for i := len(heldFDs) - 1; i >= 0; i-- {
				_ = unix.Flock(heldFDs[i], unix.LOCK_UN)
				_ = unix.Close(heldFDs[i])
			}
			heldFDs = nil
			if sysDirFD >= 0 {
				_ = unix.Close(sysDirFD)
				sysDirFD = -1
			}
			if runDirFD >= 0 {
				_ = unix.Close(runDirFD)
				runDirFD = -1
			}
		}

		retryPass := false
		for _, e := range entries {
			name := e.Name()
			if name == "" || name == "." || name == ".." {
				continue
			}
			// Fix12 Item #1: Only lock and remove corrald's exact owned pane
			// lock filenames; leave any other file, symlink, or directory untouched.
			if !isOwnedRunFileName(name) {
				hasUnowned = true
				continue
			}
			typ := e.Type()
			if typ.IsDir() {
				hasUnowned = true
				continue
			}
			if typ&os.ModeSymlink != 0 {
				// Owned-name symlink: senders ignore symlinks (O_NOFOLLOW);
				// RemoveOwned unlinks the link without traversing it.
				seenOwned[name] = struct{}{}
				lockedPaths[filepath.Clean(filepath.Join(runDir, name))] = struct{}{}
				continue
			}
			seenOwned[name] = struct{}{}

			// Acquire locks in the exact same order as senders:
			// 1. systemRunDir/<name> (only if already present; never created)
			// 2. runDir/<name>
			if sysOK {
				sfd, _, sexists, serr := flockExistingNonBlocking(sysDirFD, name, uid, deadline)
				if serr != nil {
					releaseAll()
					return nil, serr
				}
				if sexists && sfd >= 0 {
					heldFDs = append(heldFDs, sfd)
				}
			}

			rfd, fst, rexists, rerr := flockExistingNonBlocking(runDirFD, name, uid, deadline)
			if rerr != nil {
				releaseAll()
				return nil, rerr
			}
			if !rexists || rfd < 0 {
				continue
			}
			// Verify the locked fd still matches the directory entry.
			var curSt unix.Stat_t
			if ferr := unix.Fstatat(runDirFD, name, &curSt, unix.AT_SYMLINK_NOFOLLOW); ferr != nil ||
				curSt.Dev != fst.Dev || curSt.Ino != fst.Ino {
				_ = unix.Flock(rfd, unix.LOCK_UN)
				_ = unix.Close(rfd)
				releaseAll()
				retryPass = true
				break
			}
			heldFDs = append(heldFDs, rfd)
			lockedPaths[filepath.Clean(filepath.Join(runDir, name))] = struct{}{}
		}
		if retryPass {
			continue
		}

		// Fix13 Item #2: While still holding every exclusive flock, delegate
		// unlinking ONLY of the lockedPaths set (never a newly-created file not
		// locked in the first pass) and rmdir of runDir to RemoveOwned.
		left := RemoveOwned(layout, OwnedRemoveRunDir, runDir, lockedPaths)
		releaseAll()

		if len(left) == 0 {
			return nil, nil
		}
		// If a concurrent sender created a NEW owned lock file between ReadDir
		// and rmdir (with no unowned entries present), retry pass to detect
		// whether it is currently held (ErrLocked) or idle. Files that were
		// already present in ReadDir and failed to unlink (e.g. EACCES) are
		// genuine leftovers and must not be misclassified as ErrLocked.
		if !hasUnowned {
			newOwnedCreated := false
			for _, l := range left {
				base := filepath.Base(l.Path)
				if _, existed := seenOwned[base]; existed {
					continue
				}
				if filepath.Dir(l.Path) == filepath.Clean(runDir) && isOwnedRunFileName(base) {
					if info, serr := os.Lstat(l.Path); serr == nil && info.Mode().IsRegular() {
						newOwnedCreated = true
						break
					}
				}
			}
			if newOwnedCreated {
				if time.Now().After(deadline) {
					return nil, ErrLocked
				}
				continue
			}
		}
		return left, formatOwnedLeftoverErr(left)
	}
	return nil, ErrLocked
}
