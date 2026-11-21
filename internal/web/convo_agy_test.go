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
	"time"
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

	// Live reproduction: background task strip between two extra rules below the input box.
	idleWithBgTaskStrip := strings.Join([]string{
		"⣾  Running command...",
		rule,
		">",
		rule,
		"  ● [21:48:39] cd /repo && gh run watch 44747496286 --exit-status ... running",
		rule,
		"esc to cancel                               auto · Gemini 4 Neon · high · 1 task(s) · /tasks",
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
	if err := agyIdleInput(idleWithBgTaskStrip); err != nil {
		t.Fatalf("idleWithBgTaskStrip err = %v, want nil", err)
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
		"idleWithBgTaskStrip":    idleWithBgTaskStrip,
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

func TestAgyColorDraftModeAndSentHelpers(t *testing.T) {
	rule := strings.Repeat("─", 80)

	// ANSI grey dynamic suggestion must NOT be treated as a user draft.
	ansiSuggestion := strings.Join([]string{
		"\x1b[90m" + rule + "\x1b[0m",
		"\x1b[94m>\x1b[39m \x1b[90mTry asking to run the test suite\x1b[39m",
		"\x1b[90m" + rule + "\x1b[0m",
		"? for shortcuts                                      auto · Gemini 4 Neon · high",
	}, "\n")
	if err := agyIdleInputColor(ansiSuggestion); err != nil {
		t.Fatalf("ansiSuggestion err = %v, want nil", err)
	}

	ansi256Suggestion := strings.Join([]string{
		"\x1b[38;5;8m" + rule + "\x1b[0m",
		"\x1b[38;5;12m>\x1b[0m \x1b[38;5;8mCustom dynamic hint\x1b[0m",
		"\x1b[38;5;8m" + rule + "\x1b[0m",
		"? for shortcuts                                      plan · Gemini 4 Neon · high",
	}, "\n")
	if err := agyIdleInputColor(ansi256Suggestion); err != nil {
		t.Fatalf("ansi256Suggestion err = %v, want nil", err)
	}

	// Normal-colour user draft MUST still be refused as errDraft.
	ansiDraft := strings.Join([]string{
		"\x1b[90m" + rule + "\x1b[0m",
		"\x1b[94m>\x1b[39m fix the failing build",
		"\x1b[90m" + rule + "\x1b[0m",
		"? for shortcuts                                      auto · Gemini 4 Neon · high",
	}, "\n")
	if err := agyIdleInputColor(ansiDraft); !errors.Is(err, errDraft) {
		t.Fatalf("ansiDraft err = %v, want errDraft", err)
	}

	// Mode parsing across all 4 agy modes + background task strip + prompt.
	if got := agyModeOnScreen(ansiSuggestion); got != "auto" {
		t.Fatalf("mode auto = %q", got)
	}
	if got := agyModeOnScreen(ansi256Suggestion); got != "plan" {
		t.Fatalf("mode plan = %q", got)
	}
	acceptScr := strings.Join([]string{rule, ">", rule, "? for shortcuts   accept-edits · Gemini 4 Neon · high"}, "\n")
	if got := agyModeOnScreen(acceptScr); got != "acceptEdits" {
		t.Fatalf("mode acceptEdits = %q", got)
	}
	defaultScr := strings.Join([]string{rule, ">", rule, "? for shortcuts   Gemini 4 Neon · high"}, "\n")
	if got := agyModeOnScreen(defaultScr); got != "default" {
		t.Fatalf("mode default = %q", got)
	}
	if got := agyModeOnScreen("Run this command?\n> 1. Yes, run command\n  2. No, cancel"); got != "" {
		t.Fatalf("mode during prompt = %q, want empty", got)
	}

	// Sent-missing / sent-enter input checks.
	if waiting, known := agyInInput(ansiDraft, "fix the failing build"); !waiting || !known {
		t.Fatalf("agyInInput waiting=%v known=%v, want true true", waiting, known)
	}
	if !agyTypedWhole(ansiDraft, "fix the failing build") {
		t.Fatalf("agyTypedWhole should be true")
	}
	if waiting, known := agyInInput(defaultScr, "fix the failing build"); waiting || !known {
		t.Fatalf("agyInInput empty box waiting=%v known=%v, want false true", waiting, known)
	}

	// Turns outline filter.
	if !turnLine("agy", []byte(`{"step_index":0,"type":"USER_INPUT","content":"hi"}`)) {
		t.Fatalf("turnLine should match agy USER_INPUT")
	}
	if turnLine("agy", []byte(`{"step_index":1,"type":"PLANNER_RESPONSE","content":"hi"}`)) {
		t.Fatalf("turnLine should skip agy PLANNER_RESPONSE")
	}
}

func TestAgyScreenAsksAndWaitCache(t *testing.T) {
	rule := strings.Repeat("─", 80)

	// Permission prompt with Reason:, tall wrapped command, ANSI colon SGR, and
	// 35 trailing blank lines must be detected as asking (not errScreen).
	perm := strings.Join([]string{
		"\x1b[38:2:200:200:200mReason: The command restarts docker compose in a shared directory.\x1b[0m",
		"Requesting permission for:",
		"  mkdir -p /home/ihainan/tmp/backup && sqlite3 corral.db && docker compose up -d",
		"Run this command?",
		"> 1. Yes, run command",
		"  2. Yes, and always allow",
		"  3. No, cancel",
		"  ↑/↓ Navigate · tab Amend · ctrl+g edit/expand command · ctrl+r Review",
		"esc to cancel",
	}, "\n") + strings.Repeat("\n", 35)
	if !agyScreenAsks(perm) {
		t.Fatalf("agyScreenAsks(perm) = false, want true")
	}
	if err := agyIdleInputColor(perm); !errors.Is(err, errAgyAsks) {
		t.Fatalf("agyIdleInputColor(perm) = %v, want errAgyAsks", err)
	}

	// Footer-only confirmation cue (e.g. long command pushed top header out of 60 lines).
	footOnly := strings.Join([]string{
		"  some very long command tail",
		"> 1. Yes, allow",
		"  2. No, cancel",
		"esc to cancel",
	}, "\n")
	if !agyScreenAsks(footOnly) {
		t.Fatalf("agyScreenAsks(footOnly) = false, want true")
	}

	// Idle input box must NOT report asking even if older scrollback mentions permission text.
	idleAfterPerm := strings.Join([]string{
		"Requesting permission for:",
		"Run this command?",
		"● Done.",
		rule,
		">",
		rule,
	}, "\n")
	if agyScreenAsks(idleAfterPerm) {
		t.Fatalf("agyScreenAsks(idleAfterPerm) = true, want false")
	}

	// Wait cache: fresh within convoTitleTTL, invalidated by pressed >= agyWaitAt,
	// backed off on failure for convoTitleBackoff.
	now := time.Now()
	st := &convoState{
		agyWaitAt:  now.Add(-time.Second),
		agyWaitFor: "w28:p1\x001\x002\x00sid",
		agyWait:    true,
	}
	if w, ok := st.agyWaitKept(st.agyWaitFor, now.Add(-2*time.Second), now); !ok || !w {
		t.Fatalf("fresh cache = (%v, %v), want (true, true)", w, ok)
	}
	if _, ok := st.agyWaitKept(st.agyWaitFor, now.Add(-500*time.Millisecond), now); ok {
		t.Fatalf("cache should invalidate when pressed after agyWaitAt")
	}
	if _, ok := st.agyWaitKept(st.agyWaitFor, time.Time{}, now.Add(convoTitleTTL)); ok {
		t.Fatalf("cache should expire after convoTitleTTL")
	}
	st.agyWaitFail = true
	st.agyWaitFailAt = now.Add(-2 * time.Second)
	if w, ok := st.agyWaitKept(st.agyWaitFor, time.Time{}, now); !ok || !w {
		t.Fatalf("failed read should honor backoff: (%v, %v)", w, ok)
	}

	// isAgyHerdrPane fallback via noteAgyPane when herdrSocks has no hp.Agent.
	s := &Server{}
	if s.isAgyHerdrPane("s1", "w28:p1") {
		t.Fatalf("expected false before noteAgyPane")
	}
	s.noteAgyPane("s1", "w28:p1", true)
	if !s.isAgyHerdrPane("s1", "w28:p1") {
		t.Fatalf("expected true after noteAgyPane(true)")
	}
	s.noteAgyPane("s1", "w28:p1", false)
	if s.isAgyHerdrPane("s1", "w28:p1") {
		t.Fatalf("expected false after noteAgyPane(false)")
	}
}
