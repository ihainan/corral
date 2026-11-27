//go:build linux

package agent

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/ihainan/corral/internal/corrald/daemon"
)

// TestPaneLockFilenameMatchesShellTrRule verifies that PaneLockFilename matches
// the exact `corral-send-$(printf '%s' "$mode-$p" | tr -c 'A-Za-z0-9-' _)`
// naming rule used by convoGated across diverse mode/pane combinations.
func TestPaneLockFilenameMatchesShellTrRule(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("sh not found")
	}
	if _, err := exec.LookPath("tr"); err != nil {
		t.Skip("tr not found")
	}
	cases := [][2]string{
		{"tmux", "%0"},
		{"tmux", "%42"},
		{"herdr", "w1:p1"},
		{"herdr", "win_2:pane.3/a-b"},
		{"tmux", "%999"},
		{"herdr", "a b:c$d"},
	}
	for _, tc := range cases {
		mode, pane := tc[0], tc[1]
		cmd := exec.Command(sh, "-c", `printf 'corral-send-%s' "$(printf '%s' "$1-$2" | tr -c 'A-Za-z0-9-' _)"`, "_", mode, pane)
		cmd.Env = []string{"LC_ALL=C", "PATH=/usr/bin:/bin"}
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("shell tr failed for (%q,%q): %v", mode, pane, err)
		}
		got := PaneLockFilename(mode, pane)
		want := string(out)
		if got != want {
			t.Fatalf("PaneLockFilename(%q, %q) = %q, shell tr = %q", mode, pane, got, want)
		}
	}
}

// TestSharedPaneLockSymlinkAndFallback verifies that:
//   - a symlink runtime directory is rejected (never followed)
//   - a symlink lock file inside runtimeDir is rejected via O_NOFOLLOW
//   - a missing runtime directory falls back cleanly to the in-process lock
func TestSharedPaneLockSymlinkAndFallback(t *testing.T) {
	uid := uint32(os.Geteuid())
	base := t.TempDir()

	// 1. Symlink runtimeDir rejected by openSharedPaneLockFile.
	realDir := filepath.Join(base, "real")
	if err := os.MkdirAll(realDir, 0o700); err != nil {
		t.Fatal(err)
	}
	symDir := filepath.Join(base, "sym-dir")
	if err := os.Symlink(realDir, symDir); err != nil {
		t.Fatal(err)
	}
	if fd, ok := openSharedPaneLockFile(symDir, PaneLockFilename("tmux", "%0"), uid); ok {
		_ = unix.Close(fd)
		t.Fatal("openSharedPaneLockFile accepted symlink runtime directory")
	}

	// 2. Symlink lock file inside realDir rejected by O_NOFOLLOW; target untouched.
	target := filepath.Join(base, "victim")
	if err := os.WriteFile(target, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	lockName := PaneLockFilename("tmux", "%1")
	if err := os.Symlink(target, filepath.Join(realDir, lockName)); err != nil {
		t.Fatal(err)
	}
	if fd, ok := openSharedPaneLockFile(realDir, lockName, uid); ok {
		_ = unix.Close(fd)
		t.Fatal("openSharedPaneLockFile followed symlink lock file")
	}

	// 3. Missing runtime directory falls back to in-process lock and still
	// mutually excludes concurrent callers within the same process.
	ps := NewPaneSender(nil, nil, nil, nil)
	ps.SetRuntimeDirForTest(filepath.Join(base, "does-not-exist"))
	ps.SetLockTimeoutForTest(150 * time.Millisecond)

	rel1, ok1 := ps.acquirePaneLock(context.Background(), "tmux", "%2")
	if !ok1 {
		t.Fatal("expected fallback in-process lock to succeed")
	}
	defer rel1()

	if rel2, ok2 := ps.acquirePaneLock(context.Background(), "tmux", "%2"); ok2 {
		rel2()
		t.Fatal("expected second concurrent in-process lock to time out while held")
	}
}

// TestPaneLockSubprocessHelper is invoked as a separate OS process by
// TestSharedPaneLockMutualExclusion to simulate a second corrald process
// (e.g. old vs new daemon during hot-update handoff) contending on the same
// shared flock file.
func TestPaneLockSubprocessHelper(t *testing.T) {
	dir := os.Getenv("CORRAL_TEST_LOCK_DIR")
	stateDir := os.Getenv("CORRAL_TEST_LOCK_STATE_DIR")
	if dir == "" && stateDir == "" {
		return
	}
	mode := os.Getenv("CORRAL_TEST_LOCK_MODE")
	pane := os.Getenv("CORRAL_TEST_LOCK_PANE")
	action := os.Getenv("CORRAL_TEST_LOCK_ACTION")
	readyFile := os.Getenv("CORRAL_TEST_LOCK_READY")
	releaseFile := os.Getenv("CORRAL_TEST_LOCK_RELEASE")

	ps := NewPaneSender(nil, nil, nil, nil)
	if stateDir != "" {
		ps.SetLockDirsForTest(dir, stateDir)
	} else {
		ps.SetRuntimeDirForTest(dir)
	}
	switch action {
	case "hold":
		ps.SetLockTimeoutForTest(2 * time.Second)
		rel, ok := ps.acquirePaneLock(context.Background(), mode, pane)
		if !ok {
			t.Fatalf("subprocess failed to acquire lock")
		}
		if err := os.WriteFile(readyFile, []byte("locked"), 0o600); err != nil {
			rel()
			t.Fatal(err)
		}
		for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
			if _, err := os.Stat(releaseFile); err == nil {
				break
			}
			time.Sleep(15 * time.Millisecond)
		}
		rel()
	case "try_timeout":
		ps.SetLockTimeoutForTest(250 * time.Millisecond)
		rel, ok := ps.acquirePaneLock(context.Background(), mode, pane)
		if ok {
			rel()
			t.Fatalf("subprocess unexpectedly acquired lock held by another corrald process")
		}
	case "wait_acquire":
		ps.SetLockTimeoutForTest(3 * time.Second)
		t0 := time.Now()
		rel, ok := ps.acquirePaneLock(context.Background(), mode, pane)
		if !ok {
			t.Fatalf("subprocess failed to acquire lock after peer released")
		}
		rel()
		if time.Since(t0) < 200*time.Millisecond {
			t.Fatalf("subprocess acquired lock too early (%v), peer lock did not block it", time.Since(t0))
		}
	}
}

// TestSharedPaneLockMutualExclusion verifies real cross-process flock mutual
// exclusion between:
//  1. SSH convoGated shell script holding flock -> corrald waits / times out (exit 7)
//  2. corrald holding flock -> SSH convoGated shell script waits / fails flock -n
//  3. Two separate corrald processes (simulating hot update) contending on the same pane
func TestSharedPaneLockMutualExclusion(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("sh not found")
	}
	if _, err := exec.LookPath("flock"); err != nil {
		t.Skip("flock not found")
	}

	runtimeDir := t.TempDir()
	if err := os.Chmod(runtimeDir, 0o700); err != nil {
		t.Fatal(err)
	}

	t.Run("shell_holds_lock_corrald_waits_and_times_out", func(t *testing.T) {
		mode, pane := "tmux", "%0"
		readyFile := filepath.Join(runtimeDir, "sh-ready")
		releaseFile := filepath.Join(runtimeDir, "sh-release")

		// Exact lock path formula from convoGated.
		script := `
l=$XDG_RUNTIME_DIR/corral-send-$(printf '%s' "$1-$2" | tr -c 'A-Za-z0-9-' _)
exec 9>>"$l"
flock -n 9 || exit 1
printf ok > "$3"
while [ ! -f "$4" ]; do sleep 0.02; done
`
		cmd := exec.Command(sh, "-c", script, "_", mode, pane, readyFile, releaseFile)
		cmd.Env = []string{"XDG_RUNTIME_DIR=" + runtimeDir, "LC_ALL=C", "PATH=/usr/bin:/bin"}
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			_ = os.WriteFile(releaseFile, []byte("go"), 0o600)
			_ = cmd.Wait()
		})
		for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); {
			if _, err := os.Stat(readyFile); err == nil {
				break
			}
			time.Sleep(15 * time.Millisecond)
		}
		if _, err := os.Stat(readyFile); err != nil {
			t.Fatal("shell script did not acquire lock in time")
		}

		// 1a. While shell holds lock, corrald with short timeout must fail (exit 7).
		psShort := NewPaneSender(nil, nil, nil, nil)
		psShort.SetRuntimeDirForTest(runtimeDir)
		psShort.SetLockTimeoutForTest(250 * time.Millisecond)
		if rel, ok := psShort.acquirePaneLock(context.Background(), mode, pane); ok {
			rel()
			t.Fatal("corrald acquired lock while shell script was still holding flock")
		}

		// 1b. corrald waits while shell holds lock, then succeeds once shell releases.
		psWait := NewPaneSender(nil, nil, nil, nil)
		psWait.SetRuntimeDirForTest(runtimeDir)
		psWait.SetLockTimeoutForTest(3 * time.Second)
		go func() {
			time.Sleep(250 * time.Millisecond)
			_ = os.WriteFile(releaseFile, []byte("go"), 0o600)
		}()
		t0 := time.Now()
		rel, ok := psWait.acquirePaneLock(context.Background(), mode, pane)
		elapsed := time.Since(t0)
		if !ok {
			t.Fatal("corrald did not acquire lock after shell released")
		}
		rel()
		if elapsed < 200*time.Millisecond {
			t.Fatalf("corrald acquired lock after only %v, expected wait >= 200ms while shell held it", elapsed)
		}
	})

	t.Run("corrald_holds_lock_shell_waits", func(t *testing.T) {
		mode, pane := "herdr", "w1:p2"
		ps := NewPaneSender(nil, nil, nil, nil)
		ps.SetRuntimeDirForTest(runtimeDir)
		ps.SetLockTimeoutForTest(2 * time.Second)

		rel, ok := ps.acquirePaneLock(context.Background(), mode, pane)
		if !ok {
			t.Fatal("corrald failed to acquire lock")
		}

		// While corrald holds lock, shell non-blocking flock must fail immediately.
		probeScript := `
l=$XDG_RUNTIME_DIR/corral-send-$(printf '%s' "$1-$2" | tr -c 'A-Za-z0-9-' _)
exec 9>>"$l"
flock -n 9
`
		probe := exec.Command(sh, "-c", probeScript, "_", mode, pane)
		probe.Env = []string{"XDG_RUNTIME_DIR=" + runtimeDir, "LC_ALL=C", "PATH=/usr/bin:/bin"}
		if err := probe.Run(); err == nil {
			rel()
			t.Fatal("shell flock -n unexpectedly succeeded while corrald held lock")
		}

		// Shell polling loop (matching convoGated) waits until corrald releases.
		waitScript := `
l=$XDG_RUNTIME_DIR/corral-send-$(printf '%s' "$1-$2" | tr -c 'A-Za-z0-9-' _)
exec 9>>"$l"
n=0
until flock -n 9; do
  [ "$n" -ge 40 ] && exit 7
  sleep 0.05
  n=$((n + 1))
done
`
		go func() {
			time.Sleep(250 * time.Millisecond)
			rel()
		}()
		t0 := time.Now()
		wcmd := exec.Command(sh, "-c", waitScript, "_", mode, pane)
		wcmd.Env = []string{"XDG_RUNTIME_DIR=" + runtimeDir, "LC_ALL=C", "PATH=/usr/bin:/bin"}
		if out, err := wcmd.CombinedOutput(); err != nil {
			t.Fatalf("shell wait loop failed after corrald released: %v (%s)", err, strings.TrimSpace(string(out)))
		}
		if time.Since(t0) < 200*time.Millisecond {
			t.Fatalf("shell acquired lock too early (%v), corrald lock did not block it", time.Since(t0))
		}
	})

	t.Run("two_corrald_processes_mutually_exclude", func(t *testing.T) {
		mode, pane := "tmux", "%7"
		readyFile := filepath.Join(runtimeDir, "procA-ready")
		releaseFile := filepath.Join(runtimeDir, "procA-release")

		// Process A simulates old corrald holding the pane lock.
		procA := exec.Command(os.Args[0], "-test.run=^TestPaneLockSubprocessHelper$")
		procA.Env = append(os.Environ(),
			"CORRAL_TEST_LOCK_DIR="+runtimeDir,
			"CORRAL_TEST_LOCK_MODE="+mode,
			"CORRAL_TEST_LOCK_PANE="+pane,
			"CORRAL_TEST_LOCK_ACTION=hold",
			"CORRAL_TEST_LOCK_READY="+readyFile,
			"CORRAL_TEST_LOCK_RELEASE="+releaseFile,
		)
		if err := procA.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			_ = os.WriteFile(releaseFile, []byte("go"), 0o600)
			_ = procA.Wait()
		})
		for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
			if _, err := os.Stat(readyFile); err == nil {
				break
			}
			time.Sleep(15 * time.Millisecond)
		}
		if _, err := os.Stat(readyFile); err != nil {
			t.Fatal("process A did not acquire lock in time")
		}

		// Process B1 simulates new corrald attempting to acquire with short timeout -> must fail.
		procB1 := exec.Command(os.Args[0], "-test.run=^TestPaneLockSubprocessHelper$")
		procB1.Env = append(os.Environ(),
			"CORRAL_TEST_LOCK_DIR="+runtimeDir,
			"CORRAL_TEST_LOCK_MODE="+mode,
			"CORRAL_TEST_LOCK_PANE="+pane,
			"CORRAL_TEST_LOCK_ACTION=try_timeout",
		)
		if out, err := procB1.CombinedOutput(); err != nil {
			t.Fatalf("process B1 failed: %v\n%s", err, out)
		}

		// Process B2 waits for Process A to release, then acquires.
		go func() {
			time.Sleep(250 * time.Millisecond)
			_ = os.WriteFile(releaseFile, []byte("go"), 0o600)
		}()
		procB2 := exec.Command(os.Args[0], "-test.run=^TestPaneLockSubprocessHelper$")
		procB2.Env = append(os.Environ(),
			"CORRAL_TEST_LOCK_DIR="+runtimeDir,
			"CORRAL_TEST_LOCK_MODE="+mode,
			"CORRAL_TEST_LOCK_PANE="+pane,
			"CORRAL_TEST_LOCK_ACTION=wait_acquire",
		)
		if out, err := procB2.CombinedOutput(); err != nil {
			t.Fatalf("process B2 failed: %v\n%s", err, out)
		}
	})
}

// TestSharedPaneLockStateRunDirFallbackAndPriority verifies the 2-tier lock
// directory selection in corrald:
//  1. systemRunDir (/run/user/<uid>) preferred when valid
//  2. stateRunDir (~/.local/state/corral/run, created by Layout.EnsureRunDir)
//     used when systemRunDir is unavailable, including mutual exclusion across
//     two separate corrald processes (simulating hot update)
//  3. symlink or wrong-owner stateRunDir rejected and skipped (falls back to
//     in-process lock without touching target)
func TestSharedPaneLockStateRunDirFallbackAndPriority(t *testing.T) {
	uid := uint32(os.Geteuid())

	t.Run("fallback_to_state_run_dir_and_two_process_mutex", func(t *testing.T) {
		home := t.TempDir()
		layout, err := daemon.ResolveLayout(daemon.PathOptions{
			Home:      home,
			UID:       &uid,
			MachineID: "112233445566",
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := layout.EnsureRunDir(); err != nil {
			t.Fatalf("EnsureRunDir: %v", err)
		}
		info, err := os.Lstat(layout.RunDir)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0o700 {
			t.Fatalf("EnsureRunDir created unexpected entry: info=%v err=%v", info, err)
		}

		missingSys := filepath.Join(home, "no-run-user")
		mode, pane := "tmux", "%9"
		readyFile := filepath.Join(home, "pA-ready")
		releaseFile := filepath.Join(home, "pA-release")

		// Two independent corrald OS processes with missing /run/user/<uid>
		// must mutually exclude via ~/.local/state/corral/run!
		procA := exec.Command(os.Args[0], "-test.run=^TestPaneLockSubprocessHelper$")
		procA.Env = append(os.Environ(),
			"CORRAL_TEST_LOCK_DIR="+missingSys,
			"CORRAL_TEST_LOCK_STATE_DIR="+layout.RunDir,
			"CORRAL_TEST_LOCK_MODE="+mode,
			"CORRAL_TEST_LOCK_PANE="+pane,
			"CORRAL_TEST_LOCK_ACTION=hold",
			"CORRAL_TEST_LOCK_READY="+readyFile,
			"CORRAL_TEST_LOCK_RELEASE="+releaseFile,
		)
		if err := procA.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			_ = os.WriteFile(releaseFile, []byte("go"), 0o600)
			_ = procA.Wait()
		})
		for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
			if _, err := os.Stat(readyFile); err == nil {
				break
			}
			time.Sleep(15 * time.Millisecond)
		}
		if _, err := os.Stat(readyFile); err != nil {
			t.Fatal("procA did not acquire stateRunDir lock in time")
		}

		// Lock file must exist inside layout.RunDir.
		lockPath := filepath.Join(layout.RunDir, PaneLockFilename(mode, pane))
		if _, err := os.Stat(lockPath); err != nil {
			t.Fatalf("expected lock file in stateRunDir %s: %v", lockPath, err)
		}

		// Second corrald process must time out while procA holds it.
		procB1 := exec.Command(os.Args[0], "-test.run=^TestPaneLockSubprocessHelper$")
		procB1.Env = append(os.Environ(),
			"CORRAL_TEST_LOCK_DIR="+missingSys,
			"CORRAL_TEST_LOCK_STATE_DIR="+layout.RunDir,
			"CORRAL_TEST_LOCK_MODE="+mode,
			"CORRAL_TEST_LOCK_PANE="+pane,
			"CORRAL_TEST_LOCK_ACTION=try_timeout",
		)
		if out, err := procB1.CombinedOutput(); err != nil {
			t.Fatalf("procB1 failed: %v\n%s", err, out)
		}

		// Third corrald process waits for procA to release and acquires cleanly.
		go func() {
			time.Sleep(250 * time.Millisecond)
			_ = os.WriteFile(releaseFile, []byte("go"), 0o600)
		}()
		procB2 := exec.Command(os.Args[0], "-test.run=^TestPaneLockSubprocessHelper$")
		procB2.Env = append(os.Environ(),
			"CORRAL_TEST_LOCK_DIR="+missingSys,
			"CORRAL_TEST_LOCK_STATE_DIR="+layout.RunDir,
			"CORRAL_TEST_LOCK_MODE="+mode,
			"CORRAL_TEST_LOCK_PANE="+pane,
			"CORRAL_TEST_LOCK_ACTION=wait_acquire",
		)
		if out, err := procB2.CombinedOutput(); err != nil {
			t.Fatalf("procB2 failed: %v\n%s", err, out)
		}
	})

	t.Run("system_run_dir_preferred_over_state_run_dir", func(t *testing.T) {
		base := t.TempDir()
		sysDir := filepath.Join(base, "sys-run")
		stateDir := filepath.Join(base, "state-run")
		for _, d := range []string{sysDir, stateDir} {
			if err := os.MkdirAll(d, 0o700); err != nil {
				t.Fatal(err)
			}
		}
		ps := NewPaneSender(nil, nil, nil, nil)
		ps.SetLockDirsForTest(sysDir, stateDir)
		rel, ok := ps.acquirePaneLock(context.Background(), "herdr", "w1:p1")
		if !ok {
			t.Fatal("acquirePaneLock failed")
		}
		rel()
		if _, err := os.Stat(filepath.Join(sysDir, PaneLockFilename("herdr", "w1:p1"))); err != nil {
			t.Fatalf("expected lock file in systemRunDir: %v", err)
		}
		if entries, _ := os.ReadDir(stateDir); len(entries) != 0 {
			t.Fatalf("stateRunDir should remain empty when systemRunDir is usable, got %d entries", len(entries))
		}
	})

	t.Run("symlink_or_wrong_owner_state_run_dir_ignored", func(t *testing.T) {
		home := t.TempDir()
		victim := t.TempDir()
		stateParent := filepath.Join(home, ".local", "state", "corral")
		if err := os.MkdirAll(stateParent, 0o700); err != nil {
			t.Fatal(err)
		}
		symRun := filepath.Join(stateParent, "run")
		if err := os.Symlink(victim, symRun); err != nil {
			t.Fatal(err)
		}
		layout, err := daemon.ResolveLayout(daemon.PathOptions{Home: home, UID: &uid, MachineID: "112233445566"})
		if err != nil {
			t.Fatal(err)
		}
		if err := layout.EnsureRunDir(); err == nil {
			t.Fatal("EnsureRunDir accepted symlink run dir")
		}
		ps := NewPaneSender(nil, nil, nil, nil)
		ps.SetLockDirsForTest(filepath.Join(home, "missing-sys"), symRun)
		rel, ok := ps.acquirePaneLock(context.Background(), "tmux", "%3")
		if !ok {
			t.Fatal("expected fallback in-process lock to succeed")
		}
		rel()
		if entries, _ := os.ReadDir(victim); len(entries) != 0 {
			t.Fatalf("corrald wrote through symlink stateRunDir into victim: %v", entries)
		}

		// Wrong owner check (simulated via uid+1 without needing root).
		realRun := filepath.Join(home, "real-run")
		if err := os.MkdirAll(realRun, 0o700); err != nil {
			t.Fatal(err)
		}
		psWrong := NewPaneSender(nil, nil, nil, nil)
		psWrong.SetLockUIDForTest(uid + 1)
		psWrong.SetLockDirsForTest(filepath.Join(home, "missing-sys"), realRun)
		rel2, ok2 := psWrong.acquirePaneLock(context.Background(), "tmux", "%4")
		if !ok2 {
			t.Fatal("expected fallback in-process lock to succeed on uid mismatch")
		}
		rel2()
		if entries, _ := os.ReadDir(realRun); len(entries) != 0 {
			t.Fatalf("corrald used wrong-owner stateRunDir: %v", entries)
		}
	})
}
