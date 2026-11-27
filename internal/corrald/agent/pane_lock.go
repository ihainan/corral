//go:build linux

package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

const paneFlockPollInterval = 100 * time.Millisecond

// PaneLockFilename builds the exact per-pane flock filename shared with
// Corral's convoGated SSH shell script:
//
//	corral-send-$(printf '%s' "$mode-$p" | tr -c 'A-Za-z0-9-' _)
//
// Every byte outside ASCII [A-Za-z0-9-] is replaced with '_'.
func PaneLockFilename(mode, pane string) string {
	raw := mode + "-" + pane
	var b strings.Builder
	b.Grow(len("corral-send-") + len(raw))
	b.WriteString("corral-send-")
	for i := 0; i < len(raw); i++ {
		c := raw[i]
		if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-' {
			b.WriteByte(c)
		} else {
			b.WriteByte('_')
		}
	}
	return b.String()
}

// openValidatedLockDir opens dir with O_DIRECTORY|O_NOFOLLOW and verifies via
// Fstat that it is a real directory (not a symlink) owned by uid.
func openValidatedLockDir(dir string, uid uint32) (int, bool) {
	if dir == "" {
		return -1, false
	}
	dirFD, err := unix.Open(dir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, false
	}
	var dirSt unix.Stat_t
	if err := unix.Fstat(dirFD, &dirSt); err != nil || dirSt.Mode&unix.S_IFMT != unix.S_IFDIR || dirSt.Uid != uid {
		_ = unix.Close(dirFD)
		return -1, false
	}
	return dirFD, true
}

// selectSharedPaneLockDir chooses the first lock directory matching the shared
// priority rule with convoGated:
//  1. systemRunDir (/run/user/<uid>)
//  2. stateRunDir  (~/.local/state/corral/run)
//
// A directory is accepted only if it exists, is a directory, is not a symlink,
// and is owned by uid.
func selectSharedPaneLockDir(systemRunDir, stateRunDir string, uid uint32) (int, bool) {
	if fd, ok := openValidatedLockDir(systemRunDir, uid); ok {
		return fd, true
	}
	if fd, ok := openValidatedLockDir(stateRunDir, uid); ok {
		return fd, true
	}
	return -1, false
}

// openLockFileInDir opens filename inside the already-validated directory fd
// with O_NOFOLLOW (refusing symlinks) and verifies via Fstat that the target is
// a regular file owned by uid.
func openLockFileInDir(dirFD int, filename string, uid uint32) (int, bool) {
	if filename == "" || strings.Contains(filename, "/") {
		return -1, false
	}
	fd, err := unix.Openat(dirFD, filename, unix.O_CREAT|unix.O_WRONLY|unix.O_APPEND|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return -1, false
	}
	var fileSt unix.Stat_t
	if err := unix.Fstat(fd, &fileSt); err != nil || fileSt.Mode&unix.S_IFMT != unix.S_IFREG || fileSt.Uid != uid {
		_ = unix.Close(fd)
		return -1, false
	}
	return fd, true
}

// openSharedPaneLockFile validates dir and opens filename inside it with
// O_NOFOLLOW + UID verification.
func openSharedPaneLockFile(dir, filename string, uid uint32) (int, bool) {
	dirFD, ok := openValidatedLockDir(dir, uid)
	if !ok {
		return -1, false
	}
	defer unix.Close(dirFD)
	return openLockFileInDir(dirFD, filename, uid)
}

// acquirePaneLock acquires the shared per-pane flock using the exact directory
// priority, filename, and 100ms-poll / 10s-timeout protocol of convoGated:
//  1. /run/user/<uid> (or ps.systemRunDir when injected by tests)
//  2. ~/.local/state/corral/run (ps.stateRunDir from daemon Layout)
//  3. In-process per-pane channel lock fallback when neither directory is usable.
func (ps *PaneSender) acquirePaneLock(ctx context.Context, mode, pane string) (release func(), ok bool) {
	timeout := ps.lockTimeout
	if timeout <= 0 {
		timeout = defaultPaneLockTimeout
	}

	systemDir := ps.systemRunDir
	if systemDir == "" && !ps.lockDirsOverridden {
		systemDir = fmt.Sprintf("/run/user/%d", ps.uid)
	}
	filename := PaneLockFilename(mode, pane)
	if dirFD, hasDir := selectSharedPaneLockDir(systemDir, ps.stateRunDir, ps.uid); hasDir {
		fd, opened := openLockFileInDir(dirFD, filename, ps.uid)
		_ = unix.Close(dirFD)
		if opened {
			deadline := time.Now().Add(timeout)
			for {
				err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB)
				if err == nil {
					var once sync.Once
					return func() {
						once.Do(func() {
							_ = unix.Flock(fd, unix.LOCK_UN)
							_ = unix.Close(fd)
						})
					}, true
				}
				if errors.Is(err, unix.EINTR) {
					continue
				}
				if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
					rem := time.Until(deadline)
					if rem <= 0 {
						_ = unix.Close(fd)
						return nil, false
					}
					step := paneFlockPollInterval
					if rem < step {
						step = rem
					}
					if serr := ps.sleepFn(ctx, step); serr != nil {
						_ = unix.Close(fd)
						return nil, false
					}
					continue
				}
				// Unexpected flock error (e.g. filesystem without flock support):
				// close fd and fall back to the in-process lock below.
				_ = unix.Close(fd)
				break
			}
		}
	}

	return ps.acquireInProcessPaneLock(ctx, mode+"\x00"+pane, timeout)
}

func (ps *PaneSender) acquireInProcessPaneLock(ctx context.Context, key string, timeout time.Duration) (release func(), ok bool) {
	ps.lockMu.Lock()
	slot := ps.paneLocks[key]
	if slot == nil {
		slot = &paneLockSlot{ch: make(chan struct{}, 1)}
		ps.paneLocks[key] = slot
	}
	slot.n++
	ps.lockMu.Unlock()

	leave := func() {
		ps.lockMu.Lock()
		slot.n--
		if slot.n == 0 {
			delete(ps.paneLocks, key)
		}
		ps.lockMu.Unlock()
	}

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case slot.ch <- struct{}{}:
		var once sync.Once
		return func() {
			once.Do(func() {
				<-slot.ch
				leave()
			})
		}, true
	case <-timer.C:
		leave()
		return nil, false
	case <-ctx.Done():
		leave()
		return nil, false
	}
}
