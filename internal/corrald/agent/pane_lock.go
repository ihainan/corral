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

const (
	paneFlockPollInterval    = 100 * time.Millisecond
	paneFlockMaxInodeRetries = 64
	paneFlockInodeRetryDelay = 5 * time.Millisecond
)

type lockDevIno struct {
	dev uint64
	ino uint64
}

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

// openValidatedLockDirStat opens dir with O_DIRECTORY|O_NOFOLLOW and verifies
// via Fstat that it is a real directory (not a symlink) owned by uid.
func openValidatedLockDirStat(dir string, uid uint32) (int, unix.Stat_t, bool) {
	var zero unix.Stat_t
	if dir == "" {
		return -1, zero, false
	}
	dirFD, err := unix.Open(dir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, zero, false
	}
	var dirSt unix.Stat_t
	if err := unix.Fstat(dirFD, &dirSt); err != nil || dirSt.Mode&unix.S_IFMT != unix.S_IFDIR || dirSt.Uid != uid {
		_ = unix.Close(dirFD)
		return -1, zero, false
	}
	return dirFD, dirSt, true
}

// openValidatedLockDir opens dir with O_DIRECTORY|O_NOFOLLOW and verifies via
// Fstat that it is a real directory (not a symlink) owned by uid.
func openValidatedLockDir(dir string, uid uint32) (int, bool) {
	fd, _, ok := openValidatedLockDirStat(dir, uid)
	return fd, ok
}

// openLockFileInDirStat opens filename inside the already-validated directory
// fd with O_NOFOLLOW (refusing symlinks) and verifies via Fstat that the target
// is a regular file owned by uid.
func openLockFileInDirStat(dirFD int, filename string, uid uint32) (int, unix.Stat_t, bool) {
	var zero unix.Stat_t
	if filename == "" || strings.Contains(filename, "/") {
		return -1, zero, false
	}
	fd, err := unix.Openat(dirFD, filename, unix.O_CREAT|unix.O_WRONLY|unix.O_APPEND|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return -1, zero, false
	}
	var fileSt unix.Stat_t
	if err := unix.Fstat(fd, &fileSt); err != nil || fileSt.Mode&unix.S_IFMT != unix.S_IFREG || fileSt.Uid != uid {
		_ = unix.Close(fd)
		return -1, zero, false
	}
	return fd, fileSt, true
}

// openLockFileInDir opens filename inside the already-validated directory fd
// with O_NOFOLLOW (refusing symlinks) and verifies via Fstat that the target is
// a regular file owned by uid.
func openLockFileInDir(dirFD int, filename string, uid uint32) (int, bool) {
	fd, _, ok := openLockFileInDirStat(dirFD, filename, uid)
	return fd, ok
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

// acquireOneDirPaneLock attempts to open and exclusively flock filename inside
// dir (validating non-symlink, directory/file ownership, and post-flock inode
// identity so a lock file unlinked by a concurrent uninstall is never treated
// as a live mutual-exclusion lock).
//
// Returns:
//   - acquired=true: caller owns fd and must unlock+close it.
//   - timedOut=true: lock contention exceeded deadline or ctx was cancelled.
//   - acquired=false, timedOut=false: dir is absent/invalid/duplicate or flock
//     is unsupported on this filesystem; caller should move to the next dir.
func (ps *PaneSender) acquireOneDirPaneLock(
	ctx context.Context,
	dir, filename string,
	deadline time.Time,
	heldDirs map[lockDevIno]struct{},
) (fd int, dirID lockDevIno, acquired bool, timedOut bool) {
	for attempt := 0; attempt < paneFlockMaxInodeRetries; attempt++ {
		dirFD, dirSt, ok := openValidatedLockDirStat(dir, ps.uid)
		if !ok {
			return -1, lockDevIno{}, false, false
		}
		id := lockDevIno{dev: uint64(dirSt.Dev), ino: dirSt.Ino}
		if _, dup := heldDirs[id]; dup {
			_ = unix.Close(dirFD)
			return -1, lockDevIno{}, false, false
		}
		lockFD, fileSt, opened := openLockFileInDirStat(dirFD, filename, ps.uid)
		_ = unix.Close(dirFD)
		if !opened {
			return -1, lockDevIno{}, false, false
		}

		locked := false
		for {
			err := unix.Flock(lockFD, unix.LOCK_EX|unix.LOCK_NB)
			if err == nil {
				locked = true
				break
			}
			if errors.Is(err, unix.EINTR) {
				continue
			}
			if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
				rem := time.Until(deadline)
				if rem <= 0 {
					_ = unix.Close(lockFD)
					return -1, lockDevIno{}, false, true
				}
				step := paneFlockPollInterval
				if rem < step {
					step = rem
				}
				if serr := ps.sleepFn(ctx, step); serr != nil {
					_ = unix.Close(lockFD)
					return -1, lockDevIno{}, false, true
				}
				continue
			}
			// Unexpected flock error (e.g. filesystem without flock support).
			_ = unix.Close(lockFD)
			return -1, lockDevIno{}, false, false
		}
		if !locked {
			_ = unix.Close(lockFD)
			return -1, lockDevIno{}, false, false
		}

		// Verify post-flock that both dir and dir/filename still refer to the
		// exact same inodes we locked (rejecting unlinked/replaced inodes from
		// a concurrent uninstall).
		curDirFD, curDirSt, dirOK := openValidatedLockDirStat(dir, ps.uid)
		same := false
		if dirOK {
			if curDirSt.Dev == dirSt.Dev && curDirSt.Ino == dirSt.Ino {
				var curFileSt unix.Stat_t
				if err := unix.Fstatat(curDirFD, filename, &curFileSt, unix.AT_SYMLINK_NOFOLLOW); err == nil {
					if curFileSt.Dev == fileSt.Dev && curFileSt.Ino == fileSt.Ino &&
						curFileSt.Mode&unix.S_IFMT == unix.S_IFREG && curFileSt.Uid == ps.uid {
						same = true
					}
				}
			}
			_ = unix.Close(curDirFD)
		}
		if same {
			return lockFD, id, true, false
		}

		// Stale inode (file or dir unlinked/replaced while waiting): drop and retry.
		_ = unix.Flock(lockFD, unix.LOCK_UN)
		_ = unix.Close(lockFD)
		if attempt >= 4 {
			rem := time.Until(deadline)
			if rem <= 0 {
				return -1, lockDevIno{}, false, true
			}
			step := paneFlockInodeRetryDelay
			if rem < step {
				step = rem
			}
			if serr := ps.sleepFn(ctx, step); serr != nil {
				return -1, lockDevIno{}, false, true
			}
		}
	}
	return -1, lockDevIno{}, false, true
}

// acquirePaneLock acquires the shared per-pane flock across ALL existing valid
// candidate directories in fixed order, matching convoGated:
//  1. /run/user/<uid> (or ps.systemRunDir when injected by tests)
//  2. ~/.local/state/corral/run (ps.stateRunDir from daemon Layout)
//
// Locking every existing candidate directory in fixed order guarantees mutual
// exclusion even if /run/user/<uid> appears or disappears mid-send or
// ~/.local/state/corral/run is concurrently uninstalled. Falls back to an
// in-process per-pane channel lock only when neither directory is usable.
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
	deadline := time.Now().Add(timeout)

	var heldFDs []int
	heldDirs := make(map[lockDevIno]struct{}, 2)
	releaseAll := func() {
		for i := len(heldFDs) - 1; i >= 0; i-- {
			_ = unix.Flock(heldFDs[i], unix.LOCK_UN)
			_ = unix.Close(heldFDs[i])
		}
		heldFDs = nil
	}

	for _, dir := range []string{systemDir, ps.stateRunDir} {
		if dir == "" {
			continue
		}
		fd, dirID, acquired, timedOut := ps.acquireOneDirPaneLock(ctx, dir, filename, deadline, heldDirs)
		if timedOut {
			releaseAll()
			return nil, false
		}
		if acquired {
			heldFDs = append(heldFDs, fd)
			heldDirs[dirID] = struct{}{}
		}
	}

	// If neither directory yielded a lock (e.g. stateRunDir was just removed by
	// uninstall right as systemRunDir appeared), re-check systemDir once before
	// falling back to the in-process lock.
	if len(heldFDs) == 0 && systemDir != "" {
		fd, dirID, acquired, timedOut := ps.acquireOneDirPaneLock(ctx, systemDir, filename, deadline, heldDirs)
		if timedOut {
			releaseAll()
			return nil, false
		}
		if acquired {
			heldFDs = append(heldFDs, fd)
			heldDirs[dirID] = struct{}{}
		}
	}

	if len(heldFDs) > 0 {
		var once sync.Once
		return func() {
			once.Do(releaseAll)
		}, true
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
