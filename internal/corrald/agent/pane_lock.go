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

// openSharedPaneLockFile validates runtimeDir (must exist, be a directory,
// not a symlink, owned by uid) and opens filename inside it with O_NOFOLLOW
// (rejecting symlinks) and verifying the opened fd is a regular file owned by
// uid. Returns (-1, false) when the runtime directory is unavailable or
// untrusted so callers fall back to the in-process per-pane lock.
func openSharedPaneLockFile(runtimeDir, filename string, uid uint32) (int, bool) {
	if runtimeDir == "" || filename == "" || strings.Contains(filename, "/") {
		return -1, false
	}
	dirFD, err := unix.Open(runtimeDir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, false
	}
	defer unix.Close(dirFD)

	var dirSt unix.Stat_t
	if err := unix.Fstat(dirFD, &dirSt); err != nil {
		return -1, false
	}
	if dirSt.Mode&unix.S_IFMT != unix.S_IFDIR || dirSt.Uid != uid {
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

// acquirePaneLock acquires the shared per-pane flock in /run/user/<uid> (or
// ps.runtimeDir when injected by tests) using the exact filename and 100ms-poll
// / 10s-timeout protocol of convoGated so corrald, SSH fallback scripts, and
// old/new corrald processes during hot-update handoff mutually exclude each
// other. When the runtime directory is unavailable or untrusted, falls back to
// the in-process per-pane channel lock.
func (ps *PaneSender) acquirePaneLock(ctx context.Context, mode, pane string) (release func(), ok bool) {
	timeout := ps.lockTimeout
	if timeout <= 0 {
		timeout = defaultPaneLockTimeout
	}

	runtimeDir := ps.runtimeDir
	if runtimeDir == "" {
		runtimeDir = fmt.Sprintf("/run/user/%d", ps.uid)
	}
	filename := PaneLockFilename(mode, pane)
	if fd, opened := openSharedPaneLockFile(runtimeDir, filename, ps.uid); opened {
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
