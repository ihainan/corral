package web

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestAgyIdleInputAndBoxPipeParity(t *testing.T) {
	rule := strings.Repeat("─", 80)

	idleEmpty := strings.Join([]string{
		"● Done.",
		rule,
		">",
		rule,
		"? for shortcuts                                      auto · Gemini 4 Neon · high",
		"",
	}, "\n")

	idlePlaceholder := strings.Join([]string{
		"● Done.",
		rule,
		"> Auto mode: edits & safe commands auto-approved (shift+tab to cycle)",
		rule,
		"? for shortcuts                                      auto · Gemini 4 Neon · high",
	}, "\n")

	idleAcceptEdits := strings.Join([]string{
		"● Done.",
		rule,
		"> Accept-edits mode: file edits auto-appr...",
		rule,
		"? for shortcuts",
		"accept-edits · Gemini 4 Neon · high",
	}, "\n")

	idleWrappedPlaceholder := strings.Join([]string{
		"● Done.",
		rule,
		"> Auto mode: edits & safe commands auto-approved",
		"  (shift+tab to cycle)",
		rule,
		"? for shortcuts",
		"auto · Gemini 4 Neon · high",
		"Update available",
		"Press ? for help",
	}, "\n")

	withDraft := strings.Join([]string{
		"● Done.",
		rule,
		"> fix the failing test",
		rule,
		"? for shortcuts                                      auto · Gemini 4 Neon · high",
	}, "\n")

	permPrompt := strings.Join([]string{
		"Command",
		rule,
		"",
		"Requesting permission for:",
		"   echo PROBE_PERM > /tmp/out.txt",
		"",
		"Run this command?",
		"> 1. Yes, run command",
		"  2. No, cancel",
		"",
		"  ↑/↓ Navigate · tab Amend · ctrl+g edit/expand command",
		"esc to cancel                                accept-edits · Gemini 4 Neon · high",
	}, "\n")

	trustPrompt := strings.Join([]string{
		"Do you trust the contents of this project?",
		"",
		"> Yes, I trust this folder",
		"  No, exit",
		"",
		"  ↑/↓ Navigate · enter Confirm",
	}, "\n")

	if err := agyIdleInput(idleEmpty); err != nil {
		t.Fatalf("idleEmpty err = %v, want nil", err)
	}
	if err := agyIdleInput(idlePlaceholder); err != nil {
		t.Fatalf("idlePlaceholder err = %v, want nil", err)
	}
	if err := agyIdleInput(idleAcceptEdits); err != nil {
		t.Fatalf("idleAcceptEdits err = %v, want nil", err)
	}
	if err := agyIdleInput(idleWrappedPlaceholder); err != nil {
		t.Fatalf("idleWrappedPlaceholder err = %v, want nil", err)
	}
	if err := agyIdleInput(withDraft); !errors.Is(err, errDraft) {
		t.Fatalf("withDraft err = %v, want errDraft", err)
	}
	if err := agyIdleInput(permPrompt); !errors.Is(err, errAgyAsks) {
		t.Fatalf("permPrompt err = %v, want errAgyAsks", err)
	}
	if err := agyIdleInput(trustPrompt); !errors.Is(err, errAgyAsks) {
		t.Fatalf("trustPrompt err = %v, want errAgyAsks", err)
	}

	// Verify Go agyBoxDigest and shell agyBoxPipe produce identical SHA-256 digests.
	for name, scr := range map[string]string{
		"idleEmpty":              idleEmpty,
		"idlePlaceholder":        idlePlaceholder,
		"idleAcceptEdits":        idleAcceptEdits,
		"idleWrappedPlaceholder": idleWrappedPlaceholder,
		"withDraft":              withDraft,
	} {
		want, ok := agyBoxDigest([]byte(scr))
		if !ok {
			t.Fatalf("%s: agyBoxDigest ok=false", name)
		}
		cmd := exec.Command("sh", "-c", agyBoxPipe)
		cmd.Stdin = strings.NewReader(scr)
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("%s: agyBoxPipe failed: %v", name, err)
		}
		sum := sha256.Sum256(out)
		got := hex.EncodeToString(sum[:])
		if got != want {
			t.Fatalf("%s: pipe digest %s != go digest %s\npipe out:\n%q", name, got, want, string(out))
		}
	}

	// Verify agyBoxPipe outputs empty when a permission or trust prompt is shown.
	for name, scr := range map[string]string{
		"permPrompt":  permPrompt,
		"trustPrompt": trustPrompt,
	} {
		if _, ok := agyBoxDigest([]byte(scr)); ok {
			t.Fatalf("%s: agyBoxDigest should be false", name)
		}
		cmd := exec.Command("sh", "-c", agyBoxPipe)
		cmd.Stdin = strings.NewReader(scr)
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("%s: agyBoxPipe failed: %v", name, err)
		}
		if len(out) != 0 {
			t.Fatalf("%s: agyBoxPipe returned %q, want empty", name, string(out))
		}
	}
}

func TestAgyFdsSh(t *testing.T) {
	tmp := t.TempDir()
	base := filepath.Join(tmp, ".gemini", "antigravity-cli")
	sid := "6490c7d4-0626-4ed8-a158-da789586d5b1"
	otherSID := "11111111-2222-3333-4444-555555555555"

	if err := os.MkdirAll(filepath.Join(base, "presence"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(base, "conversations"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(base, "crashes"), 0o755); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(base, "brain", sid, ".system_generated", "logs", "transcript_full.jsonl")
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(logPath, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	proc := filepath.Join(tmp, "proc", "123")
	fdDir := filepath.Join(proc, "fd")
	if err := os.MkdirAll(fdDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Crash log and older conversation DB must be ignored in favor of presence/<sid>.lock.
	_ = os.Symlink(filepath.Join(base, "crashes", "crash_123_"+otherSID+".log"), filepath.Join(fdDir, "3"))
	_ = os.Symlink(filepath.Join(base, "conversations", otherSID+".db"), filepath.Join(fdDir, "4"))
	_ = os.Symlink(filepath.Join(base, "presence", sid+".lock"), filepath.Join(fdDir, "5"))

	cmd := exec.Command("sh", "-c", agyFdsSh+"\nagy_fds \"$1\"; printf '%s\\n%s' \"$agysid\" \"$agylog\"", "sh", proc)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("agy_fds failed: %v", err)
	}
	parts := strings.Split(string(out), "\n")
	if len(parts) != 2 || parts[0] != sid || parts[1] != logPath {
		t.Fatalf("agy_fds = %q, want %s and %s", string(out), sid, logPath)
	}
}

func TestAgyLocateLogAndArgs(t *testing.T) {
	sid := "6490c7d4-0626-4ed8-a158-da789586d5b1"
	logPath := "/home/u/.gemini/antigravity-cli/brain/" + sid + "/.system_generated/logs/transcript_full.jsonl"

	if !convoLogOf("agy", logPath, sid) {
		t.Fatalf("convoLogOf rejected valid agy transcript_full.jsonl")
	}
	if convoLogOf("agy", "/home/u/.gemini/antigravity-cli/brain/"+sid+"/.system_generated/logs/transcript.jsonl", sid) {
		t.Fatalf("convoLogOf accepted compact transcript.jsonl instead of transcript_full.jsonl")
	}
	if convoLogOf("agy", logPath, convoNoSession) {
		t.Fatalf("convoLogOf accepted zero session id")
	}

	raw := strings.Join([]string{
		"pid=4242",
		"start=100",
		"fg=1",
		"procstart=100",
		"sid=" + sid,
		"log=" + logPath,
		"agent=agy",
		"HERDR_PANE_ID=w1:p1",
		"HERDR_SOCKET_PATH=/tmp/herdr.sock",
	}, "\n") + "\x00"
	loc := parseConvoLocate(raw)
	if len(loc.procs) != 1 || loc.procs[0].agent != "agy" || loc.procs[0].sid != sid || loc.procs[0].log != logPath {
		t.Fatalf("parseConvoLocate = %+v", loc.procs)
	}

	// Fresh agy (zero session id, empty log) must also be accepted.
	rawFresh := strings.Join([]string{
		"pid=4243",
		"start=101",
		"fg=1",
		"procstart=101",
		"sid=" + convoNoSession,
		"log=",
		"agent=agy",
		"HERDR_PANE_ID=w1:p2",
		"HERDR_SOCKET_PATH=/tmp/herdr.sock",
	}, "\n") + "\x00"
	locFresh := parseConvoLocate(rawFresh)
	if len(locFresh.procs) != 1 || locFresh.procs[0].agent != "agy" || locFresh.procs[0].sid != convoNoSession || locFresh.procs[0].log != "" {
		t.Fatalf("parseConvoLocate fresh = %+v", locFresh.procs)
	}

	args := agentArgs("agy", agentStart{resume: sid})
	if len(args) != 2 || args[0] != "--conversation" || args[1] != sid {
		t.Fatalf("agentArgs agy resume = %v", args)
	}

	parsed := parseAgySessions([]byte(`[{"id":"` + sid + `","title":"My Task","first":"Hello","ms":1700000000000},{"id":"not-a-uuid","title":"Bad","ms":1}]`))
	if len(parsed) != 1 || parsed[0].ID != sid || parsed[0].Title != "My Task" || parsed[0].Modified != 1700000000000 {
		t.Fatalf("parseAgySessions = %+v", parsed)
	}
}

func TestAgyNonInterSh(t *testing.T) {
	cases := []struct {
		args []string
		want bool
	}{
		{[]string{"agy"}, false},
		{[]string{"agy", "--mode", "auto", "--model", "gemini-4-neon-high"}, false},
		{[]string{"agy", "--conversation", "6490c7d4-0626-4ed8-a158-da789586d5b1"}, false},
		{[]string{"agy", "-i", "help me"}, false},
		{[]string{"agy", "--model", "models"}, false}, // "models" is the value of --model, not subcommand
		{[]string{"agy", "-p", "hi"}, true},
		{[]string{"agy", "--print", "hi"}, true},
		{[]string{"agy", "--prompt=hi"}, true},
		{[]string{"agy", "models"}, true},
		{[]string{"agy", "mcp", "list"}, true},
		{[]string{"agy", "acp"}, true},
	}
	for _, tc := range cases {
		f := filepath.Join(t.TempDir(), "cmdline")
		if err := os.WriteFile(f, []byte(strings.Join(tc.args, "\x00")+"\x00"), 0o644); err != nil {
			t.Fatal(err)
		}
		script := strings.Replace(agyFdsSh, `"/proc/$1/cmdline"`, `"$1"`, 1) + "\nagy_noninter \"$1\""
		cmd := exec.Command("sh", "-c", script, "sh", f)
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("%v: %v", tc.args, err)
		}
		got := strings.TrimSpace(string(out)) != ""
		if got != tc.want {
			t.Fatalf("agy_noninter(%v) = %v, want %v", tc.args, got, tc.want)
		}
	}
}
