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
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/ihainan/corral/internal/corrald/daemon"
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
	val.UpdateDiscovered(true, []string{locatedCustom}, nil, nil)

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
		{"unclean path with dotdot", claudeProj + "/../-work/11111111-1111-4111-8111-111111111111.jsonl"},
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

	// Mismatched UID check on a file inside declared dir.
	wrongUIDVal := NewPathValidator(home, uid+1, "/proc")
	wrongUIDVal.UpdateDiscovered(true, []string{validClaude}, nil, nil)
	if f, _, ok := wrongUIDVal.OpenChecked(validClaude); ok {
		_ = f.Close()
		t.Fatal("validator with mismatched UID accepted file owned by different UID")
	}
	if got := Tails(wrongUIDVal, []string{validClaude}); len(got) != 0 {
		t.Fatalf("Tails with mismatched UID returned %d bytes, want 0", len(got))
	}
}

// Review #1: locate discovering a symlinked session log must NEVER add the
// symlink target (e.g. ~/.ssh/id_ed25519) to the authorized log set.
func TestLocateSymlinkDoesNotPoisonAuthorization(t *testing.T) {
	home := t.TempDir()
	uid := uint32(os.Geteuid())

	sshDir := filepath.Join(home, ".ssh")
	projDir := filepath.Join(home, ".claude", "projects", "-p")
	for _, d := range []string{sshDir, projDir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	privKey := filepath.Join(sshDir, "id_ed25519")
	if err := os.WriteFile(privKey, []byte("PRIVATE-KEY-DO-NOT-READ\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	symLog := filepath.Join(projDir, "11111111-1111-4111-8111-111111111111.jsonl")
	if err := os.Symlink(privKey, symLog); err != nil {
		t.Fatal(err)
	}

	val := NewPathValidator(home, uid, "/proc")
	val.UpdateDiscovered(true, []string{symLog}, nil, nil)

	// Requesting the symlink target directly must be rejected!
	if f, _, ok := val.OpenChecked(privKey); ok {
		_ = f.Close()
		t.Fatalf("symlink target %s was poisoned into authorization set", privKey)
	}
	// Requesting the symlink path itself must also be rejected (O_NOFOLLOW)!
	if f, _, ok := val.OpenChecked(symLog); ok {
		_ = f.Close()
		t.Fatalf("symlink %s was followed by OpenChecked", symLog)
	}
	if got := Tails(val, []string{privKey, symLog}); len(got) != 0 {
		t.Fatalf("Tails leaked symlinked private key: %q", got)
	}
}

// Review #2: openat(O_NOFOLLOW|O_DIRECTORY) prevents parent symlink swap races
// and rejects symlinks at any parent component.
func TestOpenNoFollowRejectsParentSymlinkSwapRace(t *testing.T) {
	home := t.TempDir()
	outside := t.TempDir()
	uid := uint32(os.Geteuid())

	projectsDir := filepath.Join(home, ".claude", "projects")
	if err := os.MkdirAll(projectsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	outsideFile := filepath.Join(outside, "secret.jsonl")
	if err := os.WriteFile(outsideFile, []byte("OUTSIDE_SECRET\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Prepare a real dir and a symlink in a sibling staging spot for atomic-ish swaps.
	realDir := filepath.Join(projectsDir, "real-sub")
	if err := os.MkdirAll(realDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(realDir, "secret.jsonl"), []byte("INSIDE_OK\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	swapTarget := filepath.Join(projectsDir, "swap")
	val := NewPathValidator(home, uid, "/proc")

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			_ = os.RemoveAll(swapTarget)
			if i%2 == 0 {
				_ = os.Symlink(outside, swapTarget)
			} else {
				_ = os.Mkdir(swapTarget, 0o755)
				_ = os.WriteFile(filepath.Join(swapTarget, "secret.jsonl"), []byte("INSIDE_OK\n"), 0o600)
			}
		}
	}()

	candidate := filepath.Join(swapTarget, "secret.jsonl")
	for i := 0; i < 400; i++ {
		out := Tails(val, []string{candidate})
		if bytes.Contains(out, []byte("OUTSIDE_SECRET")) {
			close(stop)
			wg.Wait()
			t.Fatalf("parent symlink swap race leaked outside file content: %q", out)
		}
	}
	close(stop)
	wg.Wait()

	// Also verify symlink at ~/.claude itself is rejected.
	home2 := t.TempDir()
	realClaude := filepath.Join(outside, "real-claude")
	_ = os.MkdirAll(filepath.Join(realClaude, "projects", "-p"), 0o755)
	_ = os.WriteFile(filepath.Join(realClaude, "projects", "-p", "a.jsonl"), []byte("x\n"), 0o600)
	_ = os.Symlink(realClaude, filepath.Join(home2, ".claude"))
	val2 := NewPathValidator(home2, uid, "/proc")
	if f, _, ok := val2.OpenChecked(filepath.Join(home2, ".claude", "projects", "-p", "a.jsonl")); ok {
		_ = f.Close()
		t.Fatal("symlinked parent .claude was not rejected by openat O_NOFOLLOW")
	}
}

// Review #4: full locate UpdateDiscovered(replace=true) evicts previous locate
// entries and custom config directories so old paths cannot be read later.
func TestUpdateDiscoveredReplaceEvictsStaleLogs(t *testing.T) {
	home := t.TempDir()
	uid := uint32(os.Geteuid())

	cfgA := filepath.Join(t.TempDir(), "claude-a")
	cfgB := filepath.Join(t.TempDir(), "claude-b")
	logA := filepath.Join(cfgA, "projects", "-a", "11111111-1111-4111-8111-111111111111.jsonl")
	logB := filepath.Join(cfgB, "projects", "-b", "22222222-2222-4222-8222-222222222222.jsonl")
	for _, p := range []string{logA, logB} {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("ok\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	val := NewPathValidator(home, uid, "/proc")
	val.UpdateDiscovered(true, []string{logA}, []string{cfgA}, nil)
	if _, ok := val.ValidatePath(logA); !ok {
		t.Fatal("logA should be authorized after first full locate")
	}

	// Next full locate no longer sees logA or cfgA.
	val.UpdateDiscovered(true, []string{logB}, []string{cfgB}, nil)
	if _, ok := val.ValidatePath(logA); ok {
		t.Fatal("stale logA remained authorized after replacement full locate")
	}
	if _, ok := val.ValidatePath(logB); !ok {
		t.Fatal("logB should be authorized after replacement full locate")
	}
}

// Review #13: readTailFromFile bounds read bytes strictly to maxBytes even if
// the file grows concurrently after Stat().
func TestReadTailFromFileBoundedByWindow(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "growing.jsonl")
	if err := os.WriteFile(p, bytes.Repeat([]byte("a"), 2000), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	// Append 1 MiB to the same file on disk in parallel while reading with a
	// 1024-byte window; readTailFromFile must NEVER return > 1024 bytes.
	writer, err := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()

	chunk := bytes.Repeat([]byte("b"), 65536)
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 64; i++ {
			select {
			case <-stop:
				return
			default:
			}
			_, _ = writer.Write(chunk)
		}
	}()

	for i := 0; i < 50; i++ {
		got, err := readTailFromFile(f, 1024)
		if err != nil {
			close(stop)
			wg.Wait()
			t.Fatal(err)
		}
		if len(got) > 1024 {
			close(stop)
			wg.Wait()
			t.Fatalf("readTailFromFile returned %d bytes, exceeding window 1024", len(got))
		}
	}
	close(stop)
	wg.Wait()
}

// Review #10 & #15: strict allowlist of full tmux args + --tmux binary validation
// + real isolated tmux server query.
func TestTmuxExecutorAllowlistAndIsolatedServer(t *testing.T) {
	// 1. Validate full argument allowlist enforcement (Review #10).
	badArgs := [][]string{
		{},
		{"-u"},
		{"-L"},
		{"-L", "sock"},
		{"run-shell", "id"},
		{"-u", "send-keys", "-t", "%1", "ls"},
		{"-u", "new-session", "-d"},
		{"-L", "sock", "-u", "capture-pane", "-p"},
		// Arbitrary format string with #(command) shell execution attempt:
		{"-u", "display-message", "-p", "-t", "=main:", "#(id)"},
		// Command injection / extra trailing commands:
		{"-u", "display-message", "-p", "-t", "%0;", "kill-server"},
		{"-u", "display-message", "-p", "-t", "=main:", tmuxFormatSessionPane, ";", "kill-server"},
		// Wrong target/format pairings:
		{"-u", "display-message", "-p", "-t", "=main:", tmuxFormatPaneTTY},
		{"-u", "display-message", "-p", "-t", "%0", tmuxFormatSessionPane},
		// Bad session/pane characters:
		{"-u", "display-message", "-p", "-t", "=a;b:", tmuxFormatSessionPane},
		{"-u", "display-message", "-p", "-t", "=a.b:", tmuxFormatSessionPane},
		{"-u", "display-message", "-p", "-t", "%abc", tmuxFormatPaneTTY},
		{"-L", "bad;sock", "-u", "display-message", "-p", "-t", "%0", tmuxFormatPaneTTY},
	}
	for _, args := range badArgs {
		if err := validateTmuxArgs(args); err == nil {
			t.Errorf("validateTmuxArgs(%v) unexpectedly succeeded", args)
		}
	}
	goodArgs := [][]string{
		{"-u", "display-message", "-p", "-t", "=main_1-a:", tmuxFormatSessionPane},
		{"-L", "test-sock.1", "-u", "display-message", "-p", "-t", "%0", tmuxFormatPaneTTY},
	}
	for _, args := range goodArgs {
		if err := validateTmuxArgs(args); err != nil {
			t.Errorf("validateTmuxArgs(%v) failed: %v", args, err)
		}
	}

	// Public methods reject invalid session/pane names before exec.
	ex := NewTmuxExecutor(t.TempDir())
	if _, err := ex.QuerySessionPane(context.Background(), "bad;name"); err == nil {
		t.Fatal("QuerySessionPane accepted invalid session name")
	}
	if _, err := ex.QueryPaneTTY(context.Background(), "%0;kill-server"); err == nil {
		t.Fatal("QueryPaneTTY accepted invalid pane id")
	}

	// Refuse non-tmux binary path.
	nonTmux := NewTmuxExecutorWithPath(t.TempDir(), "/bin/sh", uint32(os.Geteuid()))
	if _, err := nonTmux.QuerySessionPane(context.Background(), "main"); err == nil {
		t.Fatal("TmuxExecutor unexpectedly executed non-tmux binary /bin/sh")
	}

	// 2. Real isolated tmux server test via custom-dir symlink (simulating Nix profile, Review #15).
	tmuxBin, err := exec.LookPath("tmux")
	if err != nil {
		t.Skip("tmux not installed on host")
	}

	// Put symlink named "tmux" inside a safe custom directory (0755) not in fixed paths.
	customBinDir := filepath.Join(t.TempDir(), "nix-profile", "bin")
	if err := os.MkdirAll(customBinDir, 0o755); err != nil {
		t.Fatal(err)
	}
	customTmux := filepath.Join(customBinDir, "tmux")
	if err := os.Symlink(tmuxBin, customTmux); err != nil {
		t.Fatal(err)
	}
	if _, err := daemon.ValidateTmuxBinary(customTmux, uint32(os.Geteuid())); err != nil {
		t.Fatalf("ValidateTmuxBinary rejected valid custom-dir tmux symlink: %v", err)
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

	paneIDOut, err := exec.Command(tmuxBin, "-L", sockName, "-u", "display-message", "-p", "-t", "=corraltest:", "#{pane_id}").Output()
	if err != nil {
		t.Fatalf("tmux display-message pane_id failed: %v", err)
	}
	paneID := strings.TrimSpace(string(paneIDOut))
	if paneID == "" {
		t.Fatal("empty tmux pane id")
	}
	_ = exec.Command(tmuxBin, "-L", sockName, "select-pane", "-t", paneID, "-T", "corrald-isolated-title").Run()

	wantInfo, err := exec.Command(tmuxBin, "-L", sockName, "-u", "display-message", "-p", "-t", "=corraltest:", "#{pane_id} #{pid} #{pane_tty} #{pane_title}").Output()
	if err != nil {
		t.Fatalf("reference tmux display-message failed: %v", err)
	}
	wantTTY, err := exec.Command(tmuxBin, "-L", sockName, "-u", "display-message", "-p", "-t", paneID, "#{pane_tty}").Output()
	if err != nil {
		t.Fatalf("reference tmux panetty failed: %v", err)
	}

	tx := NewTmuxExecutorWithPath(t.TempDir(), customTmux, uint32(os.Geteuid()))
	tx.socketName = sockName
	emptyStr := ""
	loc, err := NewLocator(LocatorOptions{
		Home:            t.TempDir(),
		Tmux:            tx,
		ListPIDs:        func() ([]string, error) { return nil, nil },
		ClaudeConfigDir: &emptyStr,
		XDGDataHome:     &emptyStr,
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
