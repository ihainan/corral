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

// TestUninstallCommandIgnoresCorraldBackupsBinary verifies Fix10 Item #1 on the
// shell candidate search: a binary placed under LibDir/corrald-backups/corrald
// MUST NOT be picked by UninstallCommand or treated as a corrald version dir.
func TestUninstallCommandIgnoresCorraldBackupsBinary(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("sh not found")
	}

	home := t.TempDir()
	libDir := filepath.Join(home, ".local", "lib", "corral")
	backupDir := filepath.Join(libDir, "corrald-backups")
	stateDir := filepath.Join(home, ".local", "state", "corral", "corrald", "aa11bb22cc44")
	for _, d := range []string{backupDir, stateDir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	// Fake executable inside corrald-backups/ that prints a marker if executed!
	marker := filepath.Join(backupDir, "ran.txt")
	fakeBin := filepath.Join(backupDir, "corrald")
	if err := os.WriteFile(fakeBin, []byte("#!/bin/sh\necho RAN > "+marker+"\necho result=removed\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, "install.json"), []byte(`{"gateway":"gw-test.local"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(sh, "-c", UninstallCommand(libDir, stateDir, "", "opBkpShell"))
	cmd.Env = []string{"HOME=" + home, "PATH=/usr/bin:/bin", "LC_ALL=C"}
	out, runErr := cmd.CombinedOutput()
	if runErr == nil || ParseKeyValues(out)["error"] != "binary_missing" {
		t.Fatalf("Fix10 Item #1 regression: shell candidate search picked corrald-backups/corrald! err=%v out=%s", runErr, string(out))
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatalf("Fix10 Item #1 regression: corrald-backups/corrald was executed!")
	}
}

// TestUninstallCommandRejectsOverflowReleaseCandidate verifies Fix11 Items #1-#4:
// a directory whose release overflows int64 / ParseVersionDirName (e.g.
// corrald-backup-20261128151244000000/corrald) MUST NOT be selected by the
// shell candidate search or ParseProbe.
func TestUninstallCommandRejectsOverflowReleaseCandidate(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("sh not found")
	}

	home := t.TempDir()
	libDir := filepath.Join(home, ".local", "lib", "corral")
	ovfDir := filepath.Join(libDir, "corrald-backup-20261128151244000000")
	stateDir := filepath.Join(home, ".local", "state", "corral", "corrald", "aa11bb22cc55")
	for _, d := range []string{ovfDir, stateDir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	marker := filepath.Join(ovfDir, "ran.txt")
	fakeBin := filepath.Join(ovfDir, "corrald")
	if err := os.WriteFile(fakeBin, []byte("#!/bin/sh\necho RAN > "+marker+"\necho result=removed\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, "install.json"), []byte(`{"gateway":"gw-test.local"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, cmdStr := range []string{
		UninstallCommand(libDir, stateDir, "", "0123456789abcdef"),
		UninstallCommandSoleGateway(libDir, stateDir, "", "0123456789abcdef", "gw-test.local"),
	} {
		cmd := exec.Command(sh, "-c", cmdStr)
		cmd.Env = []string{"HOME=" + home, "PATH=/usr/bin:/bin", "LC_ALL=C"}
		out, runErr := cmd.CombinedOutput()
		if runErr == nil || ParseKeyValues(out)["error"] != "binary_missing" {
			t.Fatalf("Fix11 regression: overflow candidate corrald-backup-20261128151244000000/corrald was picked! err=%v out=%s", runErr, string(out))
		}
		if _, err := os.Stat(marker); err == nil {
			t.Fatalf("Fix11 regression: corrald-backup-20261128151244000000/corrald was executed!")
		}
	}

	// ParseProbe must also reject the overflow release directory.
	probeOut := []byte("os=Linux\nmachine=x86_64\nuid=1000\nhome=" + home + "\nhome_writable=1\nmid_raw=aa11bb22cc55\nversion_dir=corrald-backup-20261128151244000000\nresult=ok\n")
	probe, err := ParseProbe(probeOut)
	if err != nil {
		t.Fatalf("ParseProbe: %v", err)
	}
	if len(probe.Versions) != 0 {
		t.Fatalf("Fix11 regression: ParseProbe accepted overflow version_dir: %v", probe.Versions)
	}
}

// TestCleanStageCommandNeverRunsShellRm verifies Fix12 Items #2 & #6:
// CleanStageCommand MUST NEVER fall back to shell `rm`; when no verified
// binary is executable on the host it returns error=no_verified_binary and
// leaves the stage directory intact for the next verified run.
func TestCleanStageCommandNeverRunsShellRm(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("sh not found")
	}

	home := t.TempDir()
	stageDir := filepath.Join(home, ".local", "state", "corral", "corrald", "aa11bb22cc66", "stage-1206000000000001")
	if err := os.MkdirAll(stageDir, 0o700); err != nil {
		t.Fatal(err)
	}
	stagedFile := filepath.Join(stageDir, "corrald")
	if err := os.WriteFile(stagedFile, []byte("unverified-bytes"), 0o600); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(sh, "-c", CleanStageCommand(filepath.Join(home, ".local", "lib", "corral", "corrald"), "", stageDir, "1206000000000001"))
	cmd.Env = []string{"HOME=" + home, "PATH=/usr/bin:/bin", "LC_ALL=C"}
	out, runErr := cmd.CombinedOutput()
	if runErr == nil || ParseKeyValues(out)["error"] != "no_verified_binary" {
		t.Fatalf("Fix12 Item #6 regression: expected error=no_verified_binary without shell rm, got err=%v out=%s", runErr, string(out))
	}
	if _, err := os.Stat(stagedFile); err != nil {
		t.Fatalf("Fix12 Item #6 regression: shell rm fallback deleted staged file %s: %v", stagedFile, err)
	}
}

// TestProbeCommandDetectsLibDirTempFiles verifies Fix12 Item #9:
// ProbeCommand + ParseProbe MUST detect corrald's own top-level temp files in
// LibDir (.corrald.tmp.<pid>.<8hex> and .README.txt.tmp.<pid>.<8hex>) while
// ignoring unrelated temp filenames like .notes.tmp.backup.
func TestProbeCommandDetectsLibDirTempFiles(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("sh not found")
	}

	for _, tc := range []struct {
		name     string
		filename string
		wantLib  bool
	}{
		{"corrald_tmp", ".corrald.tmp.12345.deadbeef", true},
		{"readme_tmp", ".README.txt.tmp.12345.cafebabe", true},
		{"unowned_tmp", ".notes.tmp.backup", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			libDir := filepath.Join(home, ".local", "lib", "corral")
			if err := os.MkdirAll(libDir, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(libDir, tc.filename), []byte("x"), 0o600); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(sh, "-c", ProbeCommand())
			cmd.Env = []string{"HOME=" + home, "PATH=/usr/bin:/bin", "LC_ALL=C"}
			out, runErr := cmd.CombinedOutput()
			if runErr != nil {
				t.Fatalf("ProbeCommand failed: %v\n%s", runErr, string(out))
			}
			probe, err := ParseProbe(out)
			if err != nil {
				t.Fatalf("ParseProbe: %v\nout=%s", err, string(out))
			}
			if probe.HasLibFiles != tc.wantLib {
				t.Fatalf("Fix12 Item #9 regression: filename=%q HasLibFiles=%v, want %v (out=%s)", tc.filename, probe.HasLibFiles, tc.wantLib, string(out))
			}
		})
	}
}

// TestUninstallCommandDoesNotReportRemovedWhenLibDirHasTempFiles verifies Fix13 Item #6:
// The uninstall shell's early `result=removed` branch MUST NOT fire while
// LibDir still holds anything (corrald temp files or unowned leftovers); it
// must fall through to the Go recovery/uninstall path.
func TestUninstallCommandDoesNotReportRemovedWhenLibDirHasTempFiles(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("sh not found")
	}

	for _, filename := range []string{
		".corrald.tmp.12345.deadbeef",
		".README.txt.tmp.12345.cafebabe",
		"leftover.dat",
	} {
		t.Run(filename, func(t *testing.T) {
			home := t.TempDir()
			libDir := filepath.Join(home, ".local", "lib", "corral")
			stateDir := filepath.Join(home, ".local", "state", "corral", "corrald", "aa11bb22cc77")
			if err := os.MkdirAll(libDir, 0o700); err != nil {
				t.Fatal(err)
			}
			// StateDir and run/ do NOT exist; only LibDir/<filename> remains.
			target := filepath.Join(libDir, filename)
			if err := os.WriteFile(target, []byte("leftover"), 0o600); err != nil {
				t.Fatal(err)
			}

			// 1. UninstallCommand (manual remove) -> must fall through to error=binary_missing (NOT result=removed).
			cmd1 := exec.Command(sh, "-c", UninstallCommand(libDir, stateDir, "", "1306000000000001"))
			cmd1.Env = []string{"HOME=" + home, "PATH=/usr/bin:/bin", "LC_ALL=C"}
			out1, err1 := cmd1.CombinedOutput()
			if err1 == nil || ParseKeyValues(out1)["result"] == "removed" || ParseKeyValues(out1)["error"] != "binary_missing" {
				t.Fatalf("Fix13 Item #6 regression: UninstallCommand fired early removed with LibDir/%s present! err=%v out=%s", filename, err1, string(out1))
			}

			// 2. UninstallCommandSoleGateway -> must fail closed (NOT result=removed).
			cmd2 := exec.Command(sh, "-c", UninstallCommandSoleGateway(libDir, stateDir, "", "1306000000000002", "gw-test.local"))
			cmd2.Env = []string{"HOME=" + home, "PATH=/usr/bin:/bin", "LC_ALL=C"}
			out2, err2 := cmd2.CombinedOutput()
			if err2 == nil || ParseKeyValues(out2)["result"] == "removed" {
				t.Fatalf("Fix13 Item #6 regression: UninstallCommandSoleGateway fired early removed with LibDir/%s present! err=%v out=%s", filename, err2, string(out2))
			}

			if _, err := os.Lstat(target); err != nil {
				t.Fatalf("shell unexpectedly removed %s: %v", target, err)
			}
		})
	}
}
