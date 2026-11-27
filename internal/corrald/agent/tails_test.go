//go:build linux

package agent

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/ihainan/corral/internal/corrald/proto"
)

func TestPathValidatorRejectsDisallowedPaths(t *testing.T) {
	home := t.TempDir()
	outside := t.TempDir()
	uid := uint32(os.Geteuid())

	claudeProj := filepath.Join(home, ".claude", "projects", "-work")
	codexSess := filepath.Join(home, ".codex", "sessions", "2026", "11", "27")
	agyBrain := filepath.Join(home, ".gemini", "antigravity-cli", "brain", "11111111-1111-4111-8111-111111111111", ".system_generated", "logs")
	customDir := filepath.Join(t.TempDir(), "custom-claude", "projects", "-work")
	for _, d := range []string{claudeProj, codexSess, agyBrain, customDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	validClaude := filepath.Join(claudeProj, "11111111-1111-4111-8111-111111111111.jsonl")
	validCodex := filepath.Join(codexSess, "rollout-2026-11-27T01-00-00-22222222-2222-4222-8222-222222222222.jsonl")
	validAgy := filepath.Join(agyBrain, "transcript_full.jsonl")
	locatedCustom := filepath.Join(customDir, "33333333-3333-4333-8333-333333333333.jsonl")
	outsideSecret := filepath.Join(outside, "secret.jsonl")
	homeSecret := filepath.Join(home, "secret.txt")

	for _, f := range []string{validClaude, validCodex, validAgy, locatedCustom, outsideSecret, homeSecret} {
		if err := os.WriteFile(f, []byte(`{"type":"message","text":"hello"}`+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	// 1. Leaf symlink inside allowed dir pointing outside.
	leafSym := filepath.Join(claudeProj, "evil-leaf.jsonl")
	if err := os.Symlink(outsideSecret, leafSym); err != nil {
		t.Fatal(err)
	}

	// 2. Directory symlink inside allowed dir escaping outside.
	dirSym := filepath.Join(home, ".claude", "projects", "escape-dir")
	if err := os.Symlink(outside, dirSym); err != nil {
		t.Fatal(err)
	}
	viaDirSym := filepath.Join(dirSym, "secret.jsonl")

	// 3. Leaf symlink inside allowed dir pointing to another valid file inside allowed dir.
	internalSym := filepath.Join(claudeProj, "internal-link.jsonl")
	if err := os.Symlink(validClaude, internalSym); err != nil {
		t.Fatal(err)
	}

	// 4. FIFO (named pipe) inside allowed dir (non-regular file).
	fifoPath := filepath.Join(claudeProj, "pipe.jsonl")
	if err := syscall.Mkfifo(fifoPath, 0o600); err != nil {
		t.Fatal(err)
	}

	val := NewPathValidator(home, uid, "/proc")
	val.UpdateDiscovered([]string{locatedCustom}, nil, nil)

	// Allowed cases must succeed.
	for _, tc := range []struct {
		name string
		path string
		cap  string
	}{
		{"claude declared dir", validClaude, proto.ReadAgentLogsClaude},
		{"codex declared dir", validCodex, proto.ReadAgentLogsCodex},
		{"agy declared dir", validAgy, proto.ReadAgentLogsAgy},
		{"located custom log", locatedCustom, proto.ReadAgentLogsClaude},
	} {
		f, gotCap, ok := val.OpenChecked(tc.path)
		if !ok || f == nil {
			t.Errorf("%s (%s): expected allowed, got rejected", tc.name, tc.path)
			continue
		}
		_ = f.Close()
		if gotCap != tc.cap {
			t.Errorf("%s: cap = %q, want %q", tc.name, gotCap, tc.cap)
		}
	}

	// Disallowed cases must ALL be rejected and skipped by Tails & MessageTails.
	rejected := []struct {
		name string
		path string
	}{
		{"leaf symlink escaping dir", leafSym},
		{"dir symlink escaping dir", viaDirSym},
		{"internal leaf symlink", internalSym},
		{"outside regular file same uid", outsideSecret},
		{"home file outside agent dirs", homeSecret},
		{"directory inside allowed dir", claudeProj},
		{"fifo inside allowed dir", fifoPath},
		{"path traversal escaping root", filepath.Join(claudeProj, "..", "..", "..", "secret.txt")},
		{"relative path", "relative/log.jsonl"},
		{"newline in path", validClaude + "\n/etc/passwd"},
		{"nul in path", validClaude + "\x00"},
	}
	if _, err := os.Stat("/etc/hosts"); err == nil && uid != 0 {
		rejected = append(rejected, struct{ name, path string }{"other uid /etc/hosts", "/etc/hosts"})
	}

	var rejectedPaths []string
	for _, tc := range rejected {
		rejectedPaths = append(rejectedPaths, tc.path)
		if f, _, ok := val.OpenChecked(tc.path); ok {
			_ = f.Close()
			t.Errorf("%s (%q): unexpectedly allowed", tc.name, tc.path)
		}
	}

	// Verify Tails and MessageTails skip every rejected path cleanly while reading valid ones.
	mixed := append([]string{validClaude}, rejectedPaths...)
	mixed = append(mixed, locatedCustom)
	tailOut := Tails(val, mixed)
	msgOut := MessageTails(val, mixed)

	for _, p := range rejectedPaths {
		if strings.Contains(string(tailOut), p+"\n") {
			t.Errorf("Tails output leaked rejected path %q", p)
		}
		if strings.Contains(string(msgOut), p+"\n") {
			t.Errorf("MessageTails output leaked rejected path %q", p)
		}
	}
	if !strings.Contains(string(tailOut), validClaude+"\n") || !strings.Contains(string(tailOut), locatedCustom+"\n") {
		t.Fatalf("Tails dropped valid files: %q", tailOut)
	}
	if !strings.Contains(string(msgOut), validClaude+"\n") || !strings.Contains(string(msgOut), locatedCustom+"\n") {
		t.Fatalf("MessageTails dropped valid files: %q", msgOut)
	}

	// Explicit different-UID check on a file inside the declared directory:
	// validator expecting uid+1 must reject files owned by current uid.
	wrongUIDVal := NewPathValidator(home, uid+1, "/proc")
	wrongUIDVal.UpdateDiscovered([]string{validClaude}, nil, nil)
	if f, _, ok := wrongUIDVal.OpenChecked(validClaude); ok {
		_ = f.Close()
		t.Fatal("validator with mismatched UID accepted file owned by different UID")
	}
	if got := Tails(wrongUIDVal, []string{validClaude}); len(got) != 0 {
		t.Fatalf("Tails with mismatched UID returned %d bytes, want 0", len(got))
	}
}

func TestTmuxExecutorAllowlistAndIsolatedServer(t *testing.T) {
	// 1. Validate allowlist enforcement regardless of whether tmux is installed.
	badArgs := [][]string{
		{},
		{"-u"},
		{"-L"},
		{"-L", "sock"},
		{"run-shell", "id"},
		{"-u", "send-keys", "-t", "%1", "ls"},
		{"-u", "new-session", "-d"},
		{"-L", "sock", "-u", "capture-pane", "-p"},
	}
	for _, args := range badArgs {
		if err := validateTmuxArgs(args); err == nil {
			t.Errorf("validateTmuxArgs(%v) unexpectedly succeeded", args)
		}
	}
	goodArgs := [][]string{
		{"-u", "display-message", "-p", "-t", "=main:", "#{pane_id}"},
		{"-L", "test-sock", "-u", "display-message", "-p", "-t", "%0", "#{pane_tty}"},
	}
	for _, args := range goodArgs {
		if err := validateTmuxArgs(args); err != nil {
			t.Errorf("validateTmuxArgs(%v) failed: %v", args, err)
		}
	}

	// Refuse non-tmux binary path.
	nonTmux := &TmuxExecutor{binaryPath: "/bin/sh"}
	if _, err := nonTmux.DisplayMessage(context.Background(), "=main:", "#{pane_id}"); err == nil {
		t.Fatal("TmuxExecutor unexpectedly executed non-tmux binary /bin/sh")
	}

	// 2. Real isolated tmux server test (skip if tmux is not installed).
	tmuxBin, err := exec.LookPath("tmux")
	if err != nil {
		t.Skip("tmux not installed on host")
	}

	tmuxTmp, err := os.MkdirTemp("/tmp", "corrald-tmux-*")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(tmuxTmp, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(tmuxTmp) })
	t.Setenv("TMUX_TMPDIR", tmuxTmp)

	sockName := fmt.Sprintf("corrald-agent-test-%d-%d", os.Getpid(), time.Now().UnixNano())
	startCmd := exec.Command(tmuxBin, "-L", sockName, "-f", "/dev/null", "new-session", "-d", "-s", "corraltest", "sleep", "60")
	if out, err := startCmd.CombinedOutput(); err != nil {
		t.Skipf("unable to start isolated tmux server in sandbox: %v (%s)", err, bytes.TrimSpace(out))
	}
	t.Cleanup(func() {
		_ = exec.Command(tmuxBin, "-L", sockName, "kill-server").Run()
	})

	// Rename pane title so pane_title is deterministic and non-empty.
	paneIDOut, err := exec.Command(tmuxBin, "-L", sockName, "-u", "display-message", "-p", "-t", "=corraltest:", "#{pane_id}").Output()
	if err != nil {
		t.Fatalf("tmux display-message pane_id failed: %v", err)
	}
	paneID := strings.TrimSpace(string(paneIDOut))
	if paneID == "" {
		t.Fatal("empty tmux pane id")
	}
	_ = exec.Command(tmuxBin, "-L", sockName, "select-pane", "-t", paneID, "-T", "corrald-isolated-title").Run()

	// Query directly via reference tmux command.
	wantInfo, err := exec.Command(tmuxBin, "-L", sockName, "-u", "display-message", "-p", "-t", "=corraltest:", "#{pane_id} #{pid} #{pane_tty} #{pane_title}").Output()
	if err != nil {
		t.Fatalf("reference tmux display-message failed: %v", err)
	}
	wantTTY, err := exec.Command(tmuxBin, "-L", sockName, "-u", "display-message", "-p", "-t", paneID, "#{pane_tty}").Output()
	if err != nil {
		t.Fatalf("reference tmux panetty failed: %v", err)
	}

	// Query via Locator + allowlisted TmuxExecutor.
	tx := &TmuxExecutor{
		binaryPath: tmuxBin,
		socketName: sockName,
		timeout:    5 * time.Second,
	}
	loc, err := NewLocator(LocatorOptions{
		Home: t.TempDir(),
		Tmux: tx,
	})
	if err != nil {
		t.Fatal(err)
	}
	out := loc.Locate(context.Background(), proto.AgentLocateParams{
		Mode:     "tmux",
		TmuxName: "corraltest",
		TmuxPane: paneID,
	})
	recs := strings.Split(string(out), "\x00")
	if len(recs) < 3 {
		t.Fatalf("expected at least tmux, panetty, hostnow records, got %q", out)
	}
	wantTmuxRec := "tmux=" + trimTmuxOutput(wantInfo, 800)
	wantPaneTTYRec := "panetty=" + trimTmuxOutput(wantTTY, 200)
	if recs[0] != wantTmuxRec {
		t.Errorf("tmux record:\ngot:  %q\nwant: %q", recs[0], wantTmuxRec)
	}
	if recs[1] != wantPaneTTYRec {
		t.Errorf("panetty record:\ngot:  %q\nwant: %q", recs[1], wantPaneTTYRec)
	}
}
