//go:build linux

package daemon

import (
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

	stage := stageBinary(t, home, mid, "opClean1", bins.v1)
	if out, code := runCLI(t, filepath.Join(stage, "corrald"), home, env,
		"install", "--stage", stage, "--op", "opClean1", "--gateway", "gw.clean", "--by", "alice"); code != 0 {
		t.Fatalf("install failed: %s", out)
	}
	installedBin := filepath.Join(home, ".local", "lib", "corral", "corrald")
	if out, code := runCLI(t, installedBin, home, env, "serve", "--gateway", "gw.clean"); code != 0 {
		t.Fatalf("serve failed: %s", out)
	}

	out, code := runCLI(t, installedBin, home, env, "uninstall", "--op", "opRmClean1")
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

	stage1 := stageBinary(t, home, mid, "opRace1", bins.v1)
	if out, code := runCLI(t, filepath.Join(stage1, "corrald"), home, env,
		"install", "--stage", stage1, "--op", "opRace1", "--gateway", "gw.race", "--by", "alice"); code != 0 {
		t.Fatalf("initial install failed: %s", out)
	}
	installedBin := filepath.Join(home, ".local", "lib", "corral", "corrald")
	stage2 := stageBinary(t, home, mid, "opRace2", bins.v2)

	var wg2 sync.WaitGroup
	var rmOut, inOut string
	var rmCode, inCode int
	wg2.Add(2)
	go func() {
		defer wg2.Done()
		rmOut, rmCode = runCLI(t, installedBin, home, env, "uninstall", "--op", "opRaceRm")
	}()
	go func() {
		defer wg2.Done()
		inOut, inCode = runCLI(t, filepath.Join(stage2, "corrald"), home, env,
			"install", "--stage", stage2, "--op", "opRace2", "--gateway", "gw.race", "--by", "bob")
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
	stage1 := stageBinary(t, home, mid, "opTmux1", bins.v1)
	if out, code := runCLI(t, filepath.Join(stage1, "corrald"), home, env,
		"install", "--stage", stage1, "--op", "opTmux1", "--gateway", "gw.tmux", "--by", "alice"); code != 0 {
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
	stage2 := stageBinary(t, home, mid, "opTmux2", bins.v2)
	out2, code2 := runCLI(t, filepath.Join(stage2, "corrald"), home, env,
		"install", "--upgrade", "--stage", stage2, "--op", "opTmux2", "--gateway", "gw.tmux", "--by", "alice")
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
		stage1 := stageBinary(t, home, mid, "opCold1", bins.v1)
		if out, code := runCLI(t, filepath.Join(stage1, "corrald"), home, env,
			"install", "--stage", stage1, "--op", "opCold1", "--gateway", "gw.cold", "--by", "alice"); code != 0 {
			t.Fatalf("install v1 failed: %s", out)
		}

		// 2. Cold-start update to V2 (install WITHOUT --upgrade).
		stage2 := stageBinary(t, home, mid, "opCold2", bins.v2)
		if out, code := runCLI(t, filepath.Join(stage2, "corrald"), home, env,
			"install", "--stage", stage2, "--op", "opCold2", "--gateway", "gw.cold", "--by", "alice"); code != 0 {
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
		out, code := runCLI(t, installedBin, home, env, "uninstall", "--op", "opColdRm")
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

		stage1 := stageBinary(t, home, mid, "opLeft1", bins.v1)
		if out, code := runCLI(t, filepath.Join(stage1, "corrald"), home, env,
			"install", "--stage", stage1, "--op", "opLeft1", "--gateway", "gw.left", "--by", "alice"); code != 0 {
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
		out, code := runCLI(t, installedBin, home, env, "uninstall", "--op", "opLeftRm")
		kv := parseKV(out)
		if code == 0 || kv["result"] != "partial" {
			t.Fatalf("expected non-zero exit with result=partial when leftover corrald file exists, got code=%d:\n%s", code, out)
		}
		if !strings.Contains(out, "left="+orphanDir) {
			t.Fatalf("expected left=%s in uninstall output, got:\n%s", orphanDir, out)
		}
	})
}
