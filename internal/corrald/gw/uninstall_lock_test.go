//go:build linux

package gw

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestUninstallCommandBinaryMissingNeverDeletesUnlocked verifies Fix9 Item #1:
// when the corrald binary is missing on the host while state/tombstone/run/
// remain, UninstallCommand MUST NOT truncate the tombstone or execute unlocked
// shell deletion; it must return error=binary_missing so RunUninstallWithRetry
// reinstalls corrald into LibDir and runs the locked Go uninstall path.
func TestUninstallCommandBinaryMissingNeverDeletesUnlocked(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("sh not found")
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

	tombPath := filepath.Join(stateDir, "removing")
	tombContent := `{"created_at":"2026-11-28T00:00:00Z","gateway":"gw-test.local"}` + "\n"
	if err := os.WriteFile(tombPath, []byte(tombContent), 0o600); err != nil {
		t.Fatal(err)
	}
	lockPath := filepath.Join(runDir, "corral-send-tmux-_0")
	if err := os.WriteFile(lockPath, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}

	cmdStr := UninstallCommand(libDir, stateDir, "", "opBinMissing")
	cmd := exec.Command(sh, "-c", cmdStr)
	cmd.Env = []string{"HOME=" + home, "PATH=/usr/bin:/bin", "LC_ALL=C"}
	out, runErr := cmd.CombinedOutput()
	kv := ParseKeyValues(out)
	if runErr == nil || kv["error"] != "binary_missing" {
		t.Fatalf("Fix9 Item #1 regression: expected non-zero with error=binary_missing, got err=%v kv=%v out=%s", runErr, kv, string(out))
	}
	gotTomb, err := os.ReadFile(tombPath)
	if err != nil || string(gotTomb) != tombContent {
		t.Fatalf("Fix9 Item #1 regression: removing tombstone was truncated or removed: content=%q err=%v", string(gotTomb), err)
	}
	if _, err := os.Stat(lockPath); err != nil {
		t.Fatalf("Fix9 Item #1 regression: run/ file deleted by unlocked shell: %v", err)
	}

	// When nothing remains at all, UninstallCommand cleanly reports result=removed.
	_ = os.RemoveAll(stateDir)
	_ = os.RemoveAll(libDir)
	_ = os.RemoveAll(runDir)
	cmdClean := exec.Command(sh, "-c", UninstallCommand(libDir, stateDir, "", "opAlreadyClean"))
	cmdClean.Env = []string{"HOME=" + home, "PATH=/usr/bin:/bin", "LC_ALL=C"}
	outClean, errClean := cmdClean.CombinedOutput()
	if errClean != nil || ParseKeyValues(outClean)["result"] != "removed" {
		t.Fatalf("expected result=removed when nothing remains, err=%v out=%s", errClean, string(outClean))
	}
}
