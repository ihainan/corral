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
)

// ErrLocked is returned when another corrald instance already holds the flock.
var ErrLocked = errors.New("corrald lock is held by another process")

// FileLock wraps an exclusive Linux flock(2) on corrald.lock.
type FileLock struct {
	file *os.File
}

// AcquireLock opens path (0600, refusing symlinks) and acquires a non-blocking
// exclusive flock.
func AcquireLock(path string) (*FileLock, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	if !info.Mode().IsRegular() {
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
	return &FileLock{file: f}, nil
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
