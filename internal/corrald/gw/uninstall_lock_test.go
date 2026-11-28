//go:build linux

package gw

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/ihainan/corral/internal/corrald/daemon"
	"github.com/ihainan/corral/internal/corrald/proto"
)

// TestFallbackUninstallBusyWhenRunDirPaneLockHeld verifies bug 1(a) on the
// shell fallback path of UninstallCommand (commands.go): when the corrald
// binary is already absent and an active SSH send holds a flock inside
// ~/.local/state/corral/run, fallback uninstall MUST return error=busy and
// leave run/ (and the exact lock file inode) untouched instead of rm -rf'ing it.
// Once the lock is released, retrying fallback uninstall removes run/ cleanly.
func TestFallbackUninstallBusyWhenRunDirPaneLockHeld(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("sh not found")
	}
	if _, err := exec.LookPath("flock"); err != nil {
		t.Skip("flock not found")
	}

	home := t.TempDir()
	libDir := filepath.Join(home, ".local", "lib", "corral")
	stateRoot := filepath.Join(home, ".local", "state", "corral", "corrald")
	stateDir := filepath.Join(stateRoot, "aa11bb22cc33")
	runDir := filepath.Join(home, ".local", "state", "corral", "run")
	for _, d := range []string{libDir, stateDir, runDir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}

	// Point uninstallSystemRunBase to a nonexistent dir so fallback uninstall
	// does not depend on host /run/user/<uid>.
	missingSysBase := filepath.Join(t.TempDir(), "no-run-user")
	origBase := uninstallSystemRunBase
	uninstallSystemRunBase = missingSysBase
	t.Cleanup(func() { uninstallSystemRunBase = origBase })

	lockPath := filepath.Join(runDir, "corral-send-tmux-_0")
	held, err := daemon.AcquireLock(lockPath)
	if err != nil {
		t.Fatalf("AcquireLock: %v", err)
	}
	t.Cleanup(func() { _ = held.Release() })
	devBefore, inoBefore, err := daemon.FileInode(lockPath)
	if err != nil {
		t.Fatalf("FileInode: %v", err)
	}

	// 1. While SSH send holds lock in run/, fallback uninstall MUST fail with error=busy
	//    and leave run/ + lock file inode intact.
	cmdStr := UninstallCommand(libDir, stateDir, "", "opFbBusy1")
	cmd := exec.Command(sh, "-c", cmdStr)
	cmd.Env = []string{"HOME=" + home, "PATH=/usr/bin:/bin", "LC_ALL=C"}
	out, runErr := cmd.CombinedOutput()
	kv := ParseKeyValues(out)
	if runErr == nil || kv["error"] != proto.ErrBusy {
		t.Fatalf("fallback uninstall with held run/ lock: err=%v kv=%v out=%s; want non-zero with error=busy", runErr, kv, string(out))
	}
	devAfter, inoAfter, err := daemon.FileInode(lockPath)
	if err != nil || devAfter != devBefore || inoAfter != inoBefore {
		t.Fatalf("fallback uninstall disturbed lock file while held: before=(%d,%d) after=(%d,%d) err=%v",
			devBefore, inoBefore, devAfter, inoAfter, err)
	}

	// 2. Also verify systemRunDir lock (when present) blocks fallback uninstall
	//    without deleting either systemRunDir/<file> or run/.
	sysBase := t.TempDir()
	uidStr := strconv.FormatUint(uint64(os.Geteuid()), 10)
	sysDir := filepath.Join(sysBase, uidStr)
	if err := os.MkdirAll(sysDir, 0o700); err != nil {
		t.Fatal(err)
	}
	uninstallSystemRunBase = sysBase
	_ = held.Release()

	sysLockPath := filepath.Join(sysDir, "corral-send-tmux-_0")
	sysHeld, err := daemon.AcquireLock(sysLockPath)
	if err != nil {
		t.Fatalf("AcquireLock(sys): %v", err)
	}
	t.Cleanup(func() { _ = sysHeld.Release() })

	cmdSys := exec.Command(sh, "-c", UninstallCommand(libDir, stateDir, "", "opFbBusySys"))
	cmdSys.Env = []string{"HOME=" + home, "PATH=/usr/bin:/bin", "LC_ALL=C"}
	outSys, errSys := cmdSys.CombinedOutput()
	kvSys := ParseKeyValues(outSys)
	if errSys == nil || kvSys["error"] != proto.ErrBusy {
		t.Fatalf("fallback uninstall with held sysDir lock: err=%v kv=%v out=%s; want error=busy", errSys, kvSys, string(outSys))
	}
	if _, err := os.Stat(lockPath); err != nil {
		t.Fatalf("run/ lock removed while sysDir lock was held: %v", err)
	}

	// 3. Release sysDir lock -> fallback uninstall succeeds with result=removed,
	//    deletes run/, and leaves sysDir lock file untouched.
	_ = sysHeld.Release()
	cmdOK := exec.Command(sh, "-c", UninstallCommand(libDir, stateDir, "", "opFbOK"))
	cmdOK.Env = []string{"HOME=" + home, "PATH=/usr/bin:/bin", "LC_ALL=C"}
	outOK, errOK := cmdOK.CombinedOutput()
	kvOK := ParseKeyValues(outOK)
	if errOK != nil || kvOK["result"] != "removed" {
		t.Fatalf("fallback uninstall after lock release failed: err=%v kv=%v out=%s", errOK, kvOK, string(outOK))
	}
	if _, err := os.Lstat(runDir); !os.IsNotExist(err) {
		t.Fatalf("expected runDir removed, lstat err=%v", err)
	}
	if _, err := os.Stat(sysLockPath); err != nil {
		t.Fatalf("fallback uninstall unexpectedly removed sysDir lock file: %v", err)
	}
}
