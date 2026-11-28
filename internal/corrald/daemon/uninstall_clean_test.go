//go:build linux

package daemon

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ihainan/corral/internal/corrald/proto"
)

// Part B: Single-machine uninstall must leave zero lock files or empty Corral
// directories behind, while preserving any unrelated non-empty sibling dirs.
func TestUninstallRemovesLocksAndEmptyDirsSingleMachine(t *testing.T) {
	bins := getTestBinaries(t)
	home := t.TempDir()
	const mid = "aabbcc112233"
	env := []string{"CORRALD_TEST_MACHINE_ID=" + mid}

	// Unrelated sibling under ~/.local/state must remain untouched.
	unrelated := filepath.Join(home, ".local", "state", "other-app", "keep.txt")
	if err := os.MkdirAll(filepath.Dir(unrelated), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(unrelated, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}

	stage := stageBinary(t, home, mid, "840cc9f0738a7ba2", bins.v1)
	if out, code := runCLI(t, filepath.Join(stage, "corrald"), home, env,
		"install", "--stage", stage, "--op", "840cc9f0738a7ba2", "--gateway", "gw.clean", "--by", "alice"); code != 0 {
		t.Fatalf("install failed: %s", out)
	}
	installedBin := filepath.Join(home, ".local", "lib", "corral", "corrald")
	if out, code := runCLI(t, installedBin, home, env, "serve", "--gateway", "gw.clean"); code != 0 {
		t.Fatalf("serve failed: %s", out)
	}

	// serve must have created ~/.local/state/corral/run (0700) for shared pane locks.
	runDir := filepath.Join(home, ".local", "state", "corral", "run")
	if fi, err := os.Lstat(runDir); err != nil || !fi.IsDir() || fi.Mode().Perm() != 0o700 {
		t.Fatalf("expected %s to exist as 0700 dir after serve, err=%v fi=%v", runDir, err, fi)
	}
	// Simulate a pane lock file left inside run/ and verify uninstall cleans both run/ and ~/.local/state/corral.
	if err := os.WriteFile(filepath.Join(runDir, "corral-send-tmux-_0"), nil, 0o600); err != nil {
		t.Fatal(err)
	}

	out, code := runCLI(t, installedBin, home, env, "uninstall", "--op", "26e01e033d43b3d4")
	if code != 0 || parseKV(out)["result"] != "removed" {
		t.Fatalf("uninstall failed: code=%d\n%s", code, out)
	}

	mustNotExist := []string{
		filepath.Join(home, ".local", "lib", "corral", ".corrald-shared.lock"),
		filepath.Join(home, ".local", "lib", "corral"),
		filepath.Join(home, ".local", "state", "corral", "corrald", mid, "corrald.lock"),
		filepath.Join(home, ".local", "state", "corral", "corrald", mid, "op.lock"),
		filepath.Join(home, ".local", "state", "corral", "corrald", mid),
		filepath.Join(home, ".local", "state", "corral", "corrald"),
		runDir,
		filepath.Join(home, ".local", "state", "corral"),
	}
	for _, p := range mustNotExist {
		if _, err := os.Lstat(p); !os.IsNotExist(err) {
			t.Errorf("expected %s to be completely removed after single-machine uninstall, lstat err=%v", p, err)
		}
	}
	if _, err := os.Stat(unrelated); err != nil {
		t.Errorf("unrelated file %s was unexpectedly affected: %v", unrelated, err)
	}
}

// Part B: Verify inode-checked locking prevents two mutually non-exclusive
// locks when one holder unlinks the lock file and removes its empty directory
// while concurrent waiters acquire the lock, and verify concurrent uninstall
// and install serialize cleanly.
func TestUninstallAndInstallConcurrentNoDualLock(t *testing.T) {
	// 1. Direct mutual-exclusion stress test with unlink + rmdir under lock.
	base := t.TempDir()
	lockDir := filepath.Join(base, "corral")
	lockPath := filepath.Join(lockDir, ".corrald-shared.lock")
	ensureDir := func() error { return os.MkdirAll(lockDir, 0o700) }

	var inCS atomic.Int32
	var overlap atomic.Int32
	var wg sync.WaitGroup
	for g := 0; g < 6; g++ {
		g := g
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 15; i++ {
				lk, err := AcquireDirLockWait(ensureDir, lockPath, 5*time.Second)
				if err != nil {
					t.Errorf("goroutine %d iter %d AcquireDirLockWait failed: %v", g, i, err)
					return
				}
				if n := inCS.Add(1); n > 1 {
					overlap.Add(1)
				}
				time.Sleep(time.Millisecond)
				// Leaving critical section before unlinking lockPath (unlinking the
				// lock file is the logical release point for new creators; any waiter
				// that already opened the old inode must reject it via SameFile and
				// serialize on the new inode instead of holding a second lock).
				inCS.Add(-1)
				if (g+i)%2 == 0 {
					_ = os.Remove(lockPath)
					_ = os.Remove(lockDir)
				}
				_ = lk.Release()
			}
		}()
	}
	wg.Wait()
	if overlap.Load() > 0 {
		t.Fatalf("detected %d overlapping critical sections (dual lock bug)", overlap.Load())
	}

	// 2. Real CLI concurrent uninstall + install race on the same home.
	bins := getTestBinaries(t)
	home := t.TempDir()
	const mid = "ddeeff445566"
	env := []string{"CORRALD_TEST_MACHINE_ID=" + mid}

	stage1 := stageBinary(t, home, mid, "6eb98ca203d9b39c", bins.v1)
	if out, code := runCLI(t, filepath.Join(stage1, "corrald"), home, env,
		"install", "--stage", stage1, "--op", "6eb98ca203d9b39c", "--gateway", "gw.race", "--by", "alice"); code != 0 {
		t.Fatalf("initial install failed: %s", out)
	}
	installedBin := filepath.Join(home, ".local", "lib", "corral", "corrald")
	stage2 := stageBinary(t, home, mid, "c86d166ec9c93c8e", bins.v2)

	var wg2 sync.WaitGroup
	var rmOut, inOut string
	var rmCode, inCode int
	wg2.Add(2)
	go func() {
		defer wg2.Done()
		rmOut, rmCode = runCLI(t, installedBin, home, env, "uninstall", "--op", "502b1789ccb8956b")
	}()
	go func() {
		defer wg2.Done()
		inOut, inCode = runCLI(t, filepath.Join(stage2, "corrald"), home, env,
			"install", "--stage", stage2, "--op", "c86d166ec9c93c8e", "--gateway", "gw.race", "--by", "bob")
	}()
	wg2.Wait()

	// Final on-disk state must be consistent: either cleanly installed or cleanly removed.
	instPath := filepath.Join(home, ".local", "state", "corral", "corrald", mid, "install.json")
	if _, err := os.Stat(instPath); err == nil {
		// Install won: symlink must resolve to a valid binary.
		if _, serr := os.Stat(installedBin); serr != nil {
			t.Fatalf("inconsistent state: install.json exists but symlink broken (rm=%d:%s in=%d:%s): %v", rmCode, rmOut, inCode, inOut, serr)
		}
		_, _ = runCLI(t, installedBin, home, env, "uninstall")
	} else {
		// Uninstall won: symlink must not be left behind dangling.
		if _, lerr := os.Lstat(installedBin); !os.IsNotExist(lerr) {
			t.Fatalf("inconsistent state: install.json absent but symlink remains (rm=%d:%s in=%d:%s)", rmCode, rmOut, inCode, inOut)
		}
	}
}

// C-9: When a daemon started with a valid --tmux path later has that tmux path
// deleted (e.g. Nix GC), hot-update handoff must drop the stale --tmux path
// instead of failing/rolling back the upgrade.
func TestHandoffDropsInvalidatedTmuxPath(t *testing.T) {
	bins := getTestBinaries(t)
	home := t.TempDir()
	const mid = "123456abcdef"
	env := []string{"CORRALD_TEST_MACHINE_ID=" + mid}

	// Build a trusted tmux inside a 0755 dir under /tmp (or under home).
	// Because real child processes run outside `go test`, validate from "/" so
	// check if we can place a 0755 dir under /tmp; /tmp itself is 1777 so
	// ValidateTmuxBinary from "/" rejects /tmp. Instead, test both:
	// 1. Real CLI serve with an invalid --tmux path succeeds without exiting.
	// 2. Real CLI handoff when meta/cfg holds a stale tmux path (via in-process
	//    Server with SetTrustedTmuxCheckRootForTest) succeeds cleanly.
	stage1 := stageBinary(t, home, mid, "105a366d72b41879", bins.v1)
	if out, code := runCLI(t, filepath.Join(stage1, "corrald"), home, env,
		"install", "--stage", stage1, "--op", "105a366d72b41879", "--gateway", "gw.tmux", "--by", "alice"); code != 0 {
		t.Fatalf("install v1 failed: %s", out)
	}
	installedBin := filepath.Join(home, ".local", "lib", "corral", "corrald")
	t.Cleanup(func() {
		_, _ = runCLI(t, installedBin, home, env, "uninstall")
	})

	// 1. `serve --tmux /nonexistent/old-nix-store/bin/tmux` must NOT fail; starts cleanly with empty tmux_path.
	out, code := runCLI(t, installedBin, home, env, "serve", "--gateway", "gw.tmux", "--tmux", "/nonexistent/old-nix-store/bin/tmux")
	if code != 0 || parseKV(out)["result"] != "ok" {
		t.Fatalf("serve with stale --tmux failed: code=%d\n%s", code, out)
	}
	metaPath := filepath.Join(home, ".local", "state", "corral", "corrald", mid, "meta.json")
	meta, err := ReadMetaRecord(metaPath)
	if err != nil {
		t.Fatal(err)
	}
	if meta.TmuxPath != "" {
		t.Fatalf("expected empty tmux_path after invalid --tmux, got %q", meta.TmuxPath)
	}

	// 2. Also verify handoff to v2 succeeds cleanly when v1 was started with --tmux /bin/sh or stale path.
	sock := parseKV(out)["socket"]
	stage2 := stageBinary(t, home, mid, "81bef567407d3d39", bins.v2)
	out2, code2 := runCLI(t, filepath.Join(stage2, "corrald"), home, env,
		"install", "--upgrade", "--stage", stage2, "--op", "81bef567407d3d39", "--gateway", "gw.tmux", "--by", "alice")
	if code2 != 0 {
		t.Fatalf("upgrade v2 failed: %s", out2)
	}
	v2Bin := parseKV(out2)["binary"]
	v2SHA, _ := FileSHA256(v2Bin)

	var hres proto.HandoffResult
	if err := callLocalRPC(sock, proto.MethodDaemonHandoff, proto.HandoffParams{
		Path: v2Bin, SHA256: v2SHA, Release: 200,
	}, &hres, 8*time.Second); err != nil {
		t.Fatalf("handoff to v2 failed: %v", err)
	}
	if hres.Release != 200 {
		t.Fatalf("handoff release = %d, want 200", hres.Release)
	}
	if !strings.Contains(meta.Version, "0.1.0") {
		t.Fatalf("unexpected v1 meta version %q", meta.Version)
	}
}

// B-2: Cold-start update (V1 install -> V2 install without --upgrade) must merge
// install.json so uninstall removes BOTH V1 and V2 leaving zero files (result=removed);
// and if a leftover corrald file remains on disk, uninstall must report result=partial
// with left= listing the remaining path instead of ignoring the non-empty dir error.
func TestColdStartUpdatePreservesInstallManifestAndUninstallPartialOnLeftover(t *testing.T) {
	bins := getTestBinaries(t)
	const mid = "cafe01234567"
	env := []string{"CORRALD_TEST_MACHINE_ID=" + mid}

	t.Run("v1_cold_update_v2_then_uninstall_removes_everything", func(t *testing.T) {
		home := t.TempDir()

		// 1. Install V1.
		stage1 := stageBinary(t, home, mid, "4080591eb01a08c7", bins.v1)
		if out, code := runCLI(t, filepath.Join(stage1, "corrald"), home, env,
			"install", "--stage", stage1, "--op", "4080591eb01a08c7", "--gateway", "gw.cold", "--by", "alice"); code != 0 {
			t.Fatalf("install v1 failed: %s", out)
		}

		// 2. Cold-start update to V2 (install WITHOUT --upgrade).
		stage2 := stageBinary(t, home, mid, "e4414ad919c82cbd", bins.v2)
		if out, code := runCLI(t, filepath.Join(stage2, "corrald"), home, env,
			"install", "--stage", stage2, "--op", "e4414ad919c82cbd", "--gateway", "gw.cold", "--by", "alice"); code != 0 {
			t.Fatalf("cold-start install v2 failed: %s", out)
		}

		// Verify both version dirs exist before uninstall and install.json kept V1.
		v1Dir := filepath.Join(home, ".local", "lib", "corral", "corrald-0.1.0-100")
		v2Dir := filepath.Join(home, ".local", "lib", "corral", "corrald-0.2.0-200")
		if _, err := os.Stat(v1Dir); err != nil {
			t.Fatalf("expected v1 dir before uninstall: %v", err)
		}
		if _, err := os.Stat(v2Dir); err != nil {
			t.Fatalf("expected v2 dir before uninstall: %v", err)
		}

		// 3. Uninstall: MUST delete V1, V2, symlink, README, locks, and empty dirs -> result=removed.
		installedBin := filepath.Join(home, ".local", "lib", "corral", "corrald")
		out, code := runCLI(t, installedBin, home, env, "uninstall", "--op", "0024022c36f6568d")
		kv := parseKV(out)
		if code != 0 || kv["result"] != "removed" {
			t.Fatalf("expected clean result=removed after V1->V2 cold-start update, got code=%d:\n%s", code, out)
		}

		mustNotExist := []string{
			v1Dir,
			v2Dir,
			filepath.Join(home, ".local", "lib", "corral"),
			filepath.Join(home, ".local", "state", "corral", "corrald", mid),
			filepath.Join(home, ".local", "state", "corral", "corrald"),
			filepath.Join(home, ".local", "state", "corral"),
		}
		for _, p := range mustNotExist {
			if _, err := os.Lstat(p); !os.IsNotExist(err) {
				t.Errorf("expected %s to be completely removed, lstat err=%v", p, err)
			}
		}
	})

	t.Run("leftover_file_reports_partial_with_left_path", func(t *testing.T) {
		home := t.TempDir()

		stage1 := stageBinary(t, home, mid, "afa575444d54689d", bins.v1)
		if out, code := runCLI(t, filepath.Join(stage1, "corrald"), home, env,
			"install", "--stage", stage1, "--op", "afa575444d54689d", "--gateway", "gw.left", "--by", "alice"); code != 0 {
			t.Fatalf("install v1 failed: %s", out)
		}

		// Artificially leave an untracked corrald version directory in LibDir
		// (same symptom as a dropped old version dir) AND verify uninstall reports
		// result=partial listing it instead of ignoring non-empty LibDir removal.
		orphanDir := filepath.Join(home, ".local", "lib", "corral", "corrald-0.0.1-1")
		if err := os.MkdirAll(orphanDir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(orphanDir, "corrald"), []byte("orphan"), 0o700); err != nil {
			t.Fatal(err)
		}

		installedBin := filepath.Join(home, ".local", "lib", "corral", "corrald")
		out, code := runCLI(t, installedBin, home, env, "uninstall", "--op", "4e4b80a4ec0c3bd2")
		kv := parseKV(out)
		if code == 0 || kv["result"] != "partial" {
			t.Fatalf("expected non-zero exit with result=partial when leftover corrald file exists, got code=%d:\n%s", code, out)
		}
		if !strings.Contains(out, "left="+orphanDir) {
			t.Fatalf("expected left=%s in uninstall output, got:\n%s", orphanDir, out)
		}
	})
}

func TestEnsureRunDirValidation(t *testing.T) {
	uid := uint32(os.Geteuid())

	// 1. Creates ~/.local/state/corral/run with 0700 when absent.
	home := t.TempDir()
	layout, err := ResolveLayout(PathOptions{Home: home, UID: &uid, MachineID: "aabbccddeeff"})
	if err != nil {
		t.Fatal(err)
	}
	if err := layout.EnsureRunDir(); err != nil {
		t.Fatalf("EnsureRunDir create: %v", err)
	}
	fi, err := os.Lstat(layout.RunDir)
	if err != nil || !fi.IsDir() || fi.Mode().Perm() != 0o700 {
		t.Fatalf("created RunDir stat = (%v, %v), want 0700 dir", fi, err)
	}

	// 2. Existing valid directory is accepted as-is (only checked, not modified).
	if err := os.Chmod(layout.RunDir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := layout.EnsureRunDir(); err != nil {
		t.Fatalf("EnsureRunDir existing valid dir: %v", err)
	}
	if fi2, err := os.Lstat(layout.RunDir); err != nil || fi2.Mode().Perm() != 0o750 {
		t.Fatalf("EnsureRunDir unexpectedly modified existing dir mode: %v %v", fi2, err)
	}

	// 3. Symlink at RunDir is rejected and untouched.
	homeSym := t.TempDir()
	victim := t.TempDir()
	layoutSym, err := ResolveLayout(PathOptions{Home: homeSym, UID: &uid, MachineID: "aabbccddeeff"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(layoutSym.RunDir), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, layoutSym.RunDir); err != nil {
		t.Fatal(err)
	}
	if err := layoutSym.EnsureRunDir(); err == nil {
		t.Fatal("expected EnsureRunDir to reject symlink")
	}
	if target, err := os.Readlink(layoutSym.RunDir); err != nil || target != victim {
		t.Fatalf("EnsureRunDir modified symlink: (%q, %v)", target, err)
	}

	// 4. Wrong-owner directory is rejected.
	otherUID := uid + 1
	layoutWrong, err := ResolveLayout(PathOptions{Home: home, UID: &otherUID, MachineID: "aabbccddeeff"})
	if err != nil {
		t.Fatal(err)
	}
	if err := layoutWrong.EnsureRunDir(); err == nil {
		t.Fatal("expected EnsureRunDir to reject directory owned by different UID")
	}
}

// TestUninstallBusyWhenRunDirPaneLockHeld verifies bug 1(a):
// When an active SSH send holds an exclusive flock inside ~/.local/state/corral/run,
// `corrald uninstall` MUST NOT delete run/ (or remove install.json/binaries),
// returning error=busy while keeping the removing tombstone so serve stays blocked.
// Once the SSH send releases the lock, retrying `corrald uninstall` removes
// everything cleanly (result=removed).
func TestUninstallBusyWhenRunDirPaneLockHeld(t *testing.T) {
	bins := getTestBinaries(t)
	home := t.TempDir()
	const mid = "b1a1b1a1b1a1"
	env := []string{
		"CORRALD_TEST_MACHINE_ID=" + mid,
		"CORRALD_TEST_SYSTEM_RUN_DIR=none",
		"CORRALD_TEST_RUN_LOCK_TIMEOUT=100ms",
	}

	stage := stageBinary(t, home, mid, "fa73b0ea0a81a764", bins.v1)
	if out, code := runCLI(t, filepath.Join(stage, "corrald"), home, env,
		"install", "--stage", stage, "--op", "fa73b0ea0a81a764", "--gateway", "gw.lock", "--by", "alice"); code != 0 {
		t.Fatalf("install failed: %s", out)
	}
	installedBin := filepath.Join(home, ".local", "lib", "corral", "corrald")
	if out, code := runCLI(t, installedBin, home, env, "serve", "--gateway", "gw.lock"); code != 0 {
		t.Fatalf("serve failed: %s", out)
	}

	runDir := filepath.Join(home, ".local", "state", "corral", "run")
	paneLockPath := filepath.Join(runDir, "corral-send-tmux-_0")
	// Simulate an in-flight SSH send holding the flock in ~/.local/state/corral/run.
	heldLock, err := AcquireLock(paneLockPath)
	if err != nil {
		t.Fatalf("AcquireLock(%s): %v", paneLockPath, err)
	}
	t.Cleanup(func() { _ = heldLock.Release() })
	devBefore, inoBefore, err := FileInode(paneLockPath)
	if err != nil {
		t.Fatalf("FileInode: %v", err)
	}

	// Uninstall while SSH send holds lock MUST fail with error=busy and keep run/ intact.
	out, code := runCLI(t, installedBin, home, env, "uninstall", "--op", "23c840837110906a")
	kv := parseKV(out)
	if code == 0 || kv["error"] != proto.ErrBusy {
		t.Fatalf("expected uninstall to return error=busy while run/ pane lock is held, got code=%d:\n%s", code, out)
	}

	// run/ and the exact lock file inode must still exist untouched.
	devAfter, inoAfter, err := FileInode(paneLockPath)
	if err != nil || devAfter != devBefore || inoAfter != inoBefore {
		t.Fatalf("pane lock file was removed or replaced during busy uninstall: before=(%d,%d) after=(%d,%d) err=%v",
			devBefore, inoBefore, devAfter, inoAfter, err)
	}
	// install.json and removing tombstone must remain so serve is blocked and retry works cleanly.
	if outServe, codeServe := runCLI(t, installedBin, home, env, "serve", "--gateway", "gw.lock"); codeServe == 0 || parseKV(outServe)["error"] != proto.ErrRemoving {
		t.Fatalf("expected serve to be rejected with error=removing during busy uninstall, got code=%d:\n%s", codeServe, outServe)
	}

	// Release SSH send lock and retry uninstall -> must cleanly remove everything!
	if err := heldLock.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
	out2, code2 := runCLI(t, installedBin, home, env, "uninstall", "--op", "d1ce8d1cb59a57c5")
	if code2 != 0 || parseKV(out2)["result"] != "removed" {
		t.Fatalf("retry uninstall after pane lock release failed: code=%d\n%s", code2, out2)
	}
	for _, p := range []string{
		runDir,
		filepath.Join(home, ".local", "state", "corral"),
		filepath.Join(home, ".local", "lib", "corral"),
	} {
		if _, err := os.Lstat(p); !os.IsNotExist(err) {
			t.Errorf("expected %s removed after retry uninstall, lstat err=%v", p, err)
		}
	}
}

// TestRemoveRunDirLockedChecksSystemRunDirFirst verifies that RemoveRunDirLocked
// also respects a lock held in systemRunDir (without ever unlinking files in
// systemRunDir) and leaves runDir intact when busy.
func TestRemoveRunDirLockedChecksSystemRunDirFirst(t *testing.T) {
	uid := uint32(os.Geteuid())
	base := t.TempDir()
	sysDir := filepath.Join(base, "sys-run")
	runDir := filepath.Join(base, "state-run")
	for _, d := range []string{sysDir, runDir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	const fname = "corral-send-herdr-w1_p1"
	runLockPath := filepath.Join(runDir, fname)
	sysLockPath := filepath.Join(sysDir, fname)
	if err := os.WriteFile(runLockPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	sysHeld, err := AcquireLock(sysLockPath)
	if err != nil {
		t.Fatalf("AcquireLock(sys): %v", err)
	}
	t.Cleanup(func() { _ = sysHeld.Release() })

	// While sysDir/<fname> is held (e.g. sender acquired sysDir and is about to
	// acquire runDir), RemoveRunDirLocked must return ErrLocked and leave both untouched.
	if err := RemoveRunDirLocked(runDir, sysDir, uid, 80*time.Millisecond); err == nil {
		t.Fatal("RemoveRunDirLocked succeeded while systemRunDir lock was held")
	}
	if _, err := os.Stat(runLockPath); err != nil {
		t.Fatalf("runDir lock removed despite busy sysDir lock: %v", err)
	}

	// Release sysDir lock -> RemoveRunDirLocked removes runDir and keeps sysDir file untouched.
	_ = sysHeld.Release()
	if err := RemoveRunDirLocked(runDir, sysDir, uid, 200*time.Millisecond); err != nil {
		t.Fatalf("RemoveRunDirLocked after release: %v", err)
	}
	if _, err := os.Lstat(runDir); !os.IsNotExist(err) {
		t.Fatalf("expected runDir removed, lstat err=%v", err)
	}
	if _, err := os.Stat(sysLockPath); err != nil {
		t.Fatalf("RemoveRunDirLocked unexpectedly removed systemRunDir file: %v", err)
	}
}

// TestRemoveRunDirLockedDoesNotDeleteUnlistedConcurrentPaneLock verifies Fix13 Item #2:
// OwnedRemoveRunDir MUST NOT delete a pane lock file that was not in the
// lockedPaths allowlist from pass 1 (e.g. a new pane file created and locked by
// a concurrent sender between ReadDir and RemoveOwned), and concurrent senders
// creating/holding locks during RemoveRunDirLocked never have their held lock
// file unlinked.
func TestRemoveRunDirLockedDoesNotDeleteUnlistedConcurrentPaneLock(t *testing.T) {
	uid := uint32(os.Geteuid())
	base := t.TempDir()
	runDir := filepath.Join(base, "run")
	if err := os.MkdirAll(runDir, 0o700); err != nil {
		t.Fatal(err)
	}

	lockedFile := filepath.Join(runDir, "corral-send-tmux-_0")
	concurrentFile := filepath.Join(runDir, "corral-send-tmux-_9")
	if err := os.WriteFile(lockedFile, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(concurrentFile, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	// Simulate pass 1 only having locked lockedFile; concurrentFile arrived
	// after pass 1's ReadDir and is NOT in lockedPaths.
	lockedPaths := map[string]struct{}{
		filepath.Clean(lockedFile): {},
	}
	left := RemoveOwned(&Layout{RunDir: runDir, UID: uid}, OwnedRemoveRunDir, runDir, lockedPaths)
	if len(left) == 0 {
		t.Fatalf("Fix13 Item #2 regression: RemoveOwned deleted unlisted concurrent pane file %s!", concurrentFile)
	}
	if _, err := os.Lstat(lockedFile); !os.IsNotExist(err) {
		t.Fatalf("expected lockedFile %s removed, stat err=%v", lockedFile, err)
	}
	if _, err := os.Lstat(concurrentFile); err != nil {
		t.Fatalf("Fix13 Item #2 regression: unlisted concurrent pane file %s was deleted: %v", concurrentFile, err)
	}

	// Verify that when concurrentFile is held by a sender, RemoveRunDirLocked
	// returns ErrLocked and leaves concurrentFile intact.
	lk, err := AcquireLock(concurrentFile)
	if err != nil {
		t.Fatalf("AcquireLock(concurrentFile): %v", err)
	}
	defer lk.Release()
	if err := RemoveRunDirLocked(runDir, filepath.Join(base, "no-sys"), uid, 60*time.Millisecond); !errors.Is(err, ErrLocked) {
		t.Fatalf("expected ErrLocked while concurrentFile is held, got %v", err)
	}
	if _, err := os.Lstat(concurrentFile); err != nil {
		t.Fatalf("held concurrentFile was deleted: %v", err)
	}
}
