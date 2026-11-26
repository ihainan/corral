package web

import (
	"context"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ihainan/corral/internal/convo"
	"github.com/ihainan/corral/internal/herdr"
	"github.com/ihainan/corral/internal/sshx"
	"github.com/ihainan/corral/internal/term"
)

// fastRewindTimings shrinks polling intervals for unit tests and restores them.
func fastRewindTimings(t *testing.T) {
	t.Helper()
	s, w, p, c := claudeRewindStep, claudeRewindWait, claudeRewindPostWait, claudeRewindClearWait
	claudeRewindStep = time.Millisecond
	claudeRewindWait = 35 * time.Millisecond
	claudeRewindPostWait = 45 * time.Millisecond
	claudeRewindClearWait = 30 * time.Millisecond
	t.Cleanup(func() {
		claudeRewindStep, claudeRewindWait, claudeRewindPostWait, claudeRewindClearWait = s, w, p, c
	})
}

// Realistic Claude Code 2.1.330 screen builders.
const (
	ccRule     = "────────────────────────────────────────────────────────────"
	ccOverline = "▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔"
)

func ccNormalScreen(above, input string) string {
	box := "❯"
	if input != "" {
		box = "❯ " + input
	}
	return above + "\n" + ccRule + "\n" + box + "\n" + ccRule + "\n  ? for shortcuts\n"
}

func ccPickerScreen(above string, items []string, selected int) string {
	var b strings.Builder
	b.WriteString(above)
	b.WriteString("\n" + ccOverline + "\n")
	b.WriteString("Rewind\n")
	b.WriteString("Restore the code and/or conversation to the point before…\n\n")
	for i, it := range items {
		prefix := "  "
		if i == selected {
			prefix = "❯ "
		}
		b.WriteString(prefix + it + "\n")
		if it != "(current)" {
			b.WriteString("    No code changes\n")
		}
	}
	b.WriteString("\nEnter to continue · Esc to cancel\n")
	return b.String()
}

func ccConfirmScreen(above string, quoted []string, opts []string, selected int) string {
	var b strings.Builder
	b.WriteString(above)
	b.WriteString("\n" + ccOverline + "\n")
	b.WriteString("Restore to the point before you sent this message:\n\n")
	for _, q := range quoted {
		b.WriteString("│ " + q + "\n")
	}
	b.WriteString("│ (5s ago)\n\n")
	for i, o := range opts {
		prefix := "  "
		if i == selected {
			prefix = "❯ "
		}
		b.WriteString(prefix + strconv.Itoa(i+1) + ". " + o + "\n")
	}
	b.WriteString("\nEnter to continue · Esc to cancel\n")
	return b.String()
}

// fakeClaudeRewindPane simulates Claude Code 2.1.330 TUI transitions and
// identity checks for deterministic testing.
type fakeClaudeRewindPane struct {
	mu            sync.Mutex
	curScreen     string
	curColor      string
	keys          []string
	verifyErr     error
	inFlight      *sync.WaitGroup
	onPress       func(ctx context.Context, p *fakeClaudeRewindPane, keys []string) error
	onCommitEnter func(ctx context.Context, p *fakeClaudeRewindPane, targetText string) error
	onScreen      func(ctx context.Context, p *fakeClaudeRewindPane) (string, error)
	screenReads   int
	pressCounter  int
}

func (f *fakeClaudeRewindPane) verify(ctx context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return f.verifyErr
}

func (f *fakeClaudeRewindPane) press(ctx context.Context, keys ...string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if f.verifyErr != nil {
		return f.verifyErr
	}
	f.pressCounter++
	f.keys = append(f.keys, strings.Join(keys, "+"))
	if f.onPress != nil {
		return f.onPress(ctx, f, keys)
	}
	return nil
}

func (f *fakeClaudeRewindPane) commitEnter(ctx context.Context, targetText string) error {
	f.mu.Lock()
	fn := f.onCommitEnter
	f.mu.Unlock()
	if fn != nil {
		return fn(ctx, f, targetText)
	}
	return f.press(ctx, "enter")
}

func (f *fakeClaudeRewindPane) waitInFlight(ctx context.Context) error {
	return waitGroupCtx(ctx, f.inFlight)
}

func (f *fakeClaudeRewindPane) screen(ctx context.Context) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	f.screenReads++
	if f.onScreen != nil {
		return f.onScreen(ctx, f)
	}
	return f.curScreen, nil
}

func (f *fakeClaudeRewindPane) colorScreen(ctx context.Context) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	return f.curColor, nil
}

func (f *fakeClaudeRewindPane) recordedKeys() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.keys...)
}

// newStandardRewindFake sets up a 3-turn Claude Code session rewinding to msg2.
func newStandardRewindFake(opts []string) (*fakeClaudeRewindPane, claudeRewindPlan) {
	aboveAll := "❯ first prompt\n● First answer\n\n❯ fix login\n● Second answer\n\n❯ add tests\n● Third answer"
	aboveKept := "❯ first prompt\n● First answer"
	items := []string{"first prompt", "fix login", "add tests", "(current)"}
	if opts == nil {
		opts = []string{"Restore conversation", "Summarize from here", "Summarize up to here", "Never mind"}
	}
	state := "normal"
	pickerIdx := len(items) - 1
	optIdx := 0

	fp := &fakeClaudeRewindPane{curScreen: ccNormalScreen(aboveAll, "")}
	fp.onPress = func(_ context.Context, p *fakeClaudeRewindPane, keys []string) error {
		k := strings.Join(keys, "+")
		switch state {
		case "normal":
			if k == "esc+esc" {
				state = "picker"
				pickerIdx = len(items) - 1
				p.curScreen = ccPickerScreen(aboveAll, items, pickerIdx)
			}
		case "picker":
			switch k {
			case "up":
				if pickerIdx > 0 {
					pickerIdx--
				}
				p.curScreen = ccPickerScreen(aboveAll, items, pickerIdx)
			case "down":
				if pickerIdx < len(items)-1 {
					pickerIdx++
				}
				p.curScreen = ccPickerScreen(aboveAll, items, pickerIdx)
			case "enter":
				state = "confirm"
				optIdx = 0
				p.curScreen = ccConfirmScreen(aboveAll, []string{items[pickerIdx], "check edge cases too"}, opts, optIdx)
			case "esc":
				state = "normal"
				p.curScreen = ccNormalScreen(aboveAll, "")
			}
		case "confirm":
			switch k {
			case "down":
				if optIdx < len(opts)-1 {
					optIdx++
				}
				p.curScreen = ccConfirmScreen(aboveAll, []string{items[pickerIdx], "check edge cases too"}, opts, optIdx)
			case "up":
				if optIdx > 0 {
					optIdx--
				}
				p.curScreen = ccConfirmScreen(aboveAll, []string{items[pickerIdx], "check edge cases too"}, opts, optIdx)
			case "enter":
				state = "rewound"
				p.curScreen = ccNormalScreen(aboveKept, "fix login check edge cases too")
			case "esc":
				state = "normal"
				p.curScreen = ccNormalScreen(aboveAll, "")
			}
		case "rewound":
			if k == "ctrl+c" {
				p.curScreen = ccNormalScreen(aboveKept, "")
			}
		}
		return nil
	}
	plan := claudeRewindPlan{
		Steps:             2,
		TargetText:        "fix login\ncheck edge cases too",
		WithdrawnSnippets: []string{"fix login", "Second answer", "add tests", "Third answer"},
	}
	return fp, plan
}

func TestClaudeRewindSuccess(t *testing.T) {
	fastRewindTimings(t)
	fp, plan := newStandardRewindFake(nil)

	res, err := claudeRewind(context.Background(), fp, plan)
	if err != nil {
		t.Fatalf("expected success, got err: %v", err)
	}
	if !res.OK || res.Warning != "" || res.Text != plan.TargetText {
		t.Fatalf("unexpected result: %+v", res)
	}
	wantKeys := []string{"esc+esc", "up", "up", "enter", "enter", "ctrl+c"}
	if got := fp.recordedKeys(); !slices.Equal(got, wantKeys) {
		t.Fatalf("keys = %v, want %v", got, wantKeys)
	}
}

func TestClaudeRewindTargetMismatch(t *testing.T) {
	fastRewindTimings(t)
	fp, plan := newStandardRewindFake(nil)
	plan.Steps = 1 // lands on "add tests", not "fix login"

	_, err := claudeRewind(context.Background(), fp, plan)
	if err == nil || err.code != "target_mismatch" {
		t.Fatalf("got err=%v, want target_mismatch", err)
	}
	keys := fp.recordedKeys()
	if slices.Contains(keys, "enter") {
		t.Fatalf("must not press enter on target mismatch, keys=%v", keys)
	}
	if keys[len(keys)-1] != "esc" {
		t.Fatalf("expected esc cleanup, keys=%v", keys)
	}
}

func TestClaudeRewindConfirmQuoteMismatch(t *testing.T) {
	fastRewindTimings(t)
	fp, plan := newStandardRewindFake(nil)
	orig := fp.onPress
	fp.onPress = func(ctx context.Context, p *fakeClaudeRewindPane, keys []string) error {
		err := orig(ctx, p, keys)
		if strings.Join(keys, "+") == "enter" && strings.Contains(p.curScreen, "Restore to the point") {
			p.curScreen = ccConfirmScreen("above", []string{"completely different message"}, []string{"Restore conversation", "Never mind"}, 0)
		}
		return err
	}

	_, err := claudeRewind(context.Background(), fp, plan)
	if err == nil || err.code != "confirm_mismatch" {
		t.Fatalf("got err=%v, want confirm_mismatch", err)
	}
	keys := fp.recordedKeys()
	// Only 1 enter (from picker to confirm), NEVER the second commit enter!
	enters := 0
	for _, k := range keys {
		if k == "enter" {
			enters++
		}
	}
	if enters != 1 {
		t.Fatalf("expected 1 enter (picker only), got %d, keys=%v", enters, keys)
	}
	if keys[len(keys)-1] != "esc" {
		t.Fatalf("expected esc cleanup, keys=%v", keys)
	}
}

func TestClaudeRewindWithCodeChangesSelectsConversationOnly(t *testing.T) {
	fastRewindTimings(t)
	opts := []string{"Restore code and conversation", "Restore conversation", "Restore code", "Summarize from here", "Never mind"}
	fp, plan := newStandardRewindFake(opts)

	res, err := claudeRewind(context.Background(), fp, plan)
	if err != nil {
		t.Fatalf("expected success, got %v", err)
	}
	if !res.OK || res.Warning != "" {
		t.Fatalf("unexpected result: %+v", res)
	}
	wantKeys := []string{"esc+esc", "up", "up", "enter", "down", "enter", "ctrl+c"}
	if got := fp.recordedKeys(); !slices.Equal(got, wantKeys) {
		t.Fatalf("keys = %v, want %v", got, wantKeys)
	}
}

func TestClaudeRewindRefusesWhenOptionChangedBeforeCommit(t *testing.T) {
	fastRewindTimings(t)
	opts := []string{"Restore code and conversation", "Restore conversation", "Restore code", "Never mind"}
	fp, plan := newStandardRewindFake(opts)
	sawDown := false
	downReads := 0
	origPress := fp.onPress
	fp.onPress = func(ctx context.Context, p *fakeClaudeRewindPane, keys []string) error {
		if strings.Join(keys, "+") == "down" {
			sawDown = true
		}
		return origPress(ctx, p, keys)
	}
	fp.onScreen = func(_ context.Context, p *fakeClaudeRewindPane) (string, error) {
		if sawDown {
			downReads++
			// First read after "down" sees Restore conversation selected;
			// right before final Enter (second read), cursor was moved back to
			// "Restore code and conversation".
			if downReads == 2 {
				p.curScreen = ccConfirmScreen("above", []string{"fix login", "check edge cases too"}, opts, 0)
			}
		}
		return p.curScreen, nil
	}

	_, err := claudeRewind(context.Background(), fp, plan)
	if err == nil || err.code != "confirm_mismatch" {
		t.Fatalf("got err=%v, want confirm_mismatch", err)
	}
	keys := fp.recordedKeys()
	enters := 0
	for _, k := range keys {
		if k == "enter" {
			enters++
		}
	}
	if enters != 1 {
		t.Fatalf("must NEVER press commit enter when selection moved to code restore; enters=%d keys=%v", enters, keys)
	}
	if keys[len(keys)-1] != "esc" {
		t.Fatalf("expected esc cleanup, keys=%v", keys)
	}
}

func TestClaudeRewindInputNotEmpty(t *testing.T) {
	fastRewindTimings(t)
	fp := &fakeClaudeRewindPane{
		curScreen: ccNormalScreen("❯ msg1\n● ans1", "unsent draft in terminal"),
	}
	plan := claudeRewindPlan{Steps: 1, TargetText: "msg1"}

	_, err := claudeRewind(context.Background(), fp, plan)
	if err == nil || err.code != "input_not_empty" {
		t.Fatalf("got err=%v, want input_not_empty", err)
	}
	if len(fp.recordedKeys()) != 0 {
		t.Fatalf("must send zero keys when input is not empty, got %v", fp.recordedKeys())
	}
}

func TestClaudeRewindDuplicateText(t *testing.T) {
	fastRewindTimings(t)
	// Case 1: duplicate detected in conversation plan before pressing any key.
	entries := []convo.Entry{
		{Seq: 1, ID: "u1", Role: convo.RoleUser, Parts: []convo.Part{{Kind: convo.KindText, Text: "continue"}}},
		{Seq: 2, ID: "a1", Role: convo.RoleAssistant, Parts: []convo.Part{{Kind: convo.KindText, Text: "ok 1"}}},
		{Seq: 3, ID: "u2", Role: convo.RoleUser, Parts: []convo.Part{{Kind: convo.KindText, Text: "continue"}}},
	}
	plan, perr := buildClaudeRewindPlan(entries, "u1", "")
	if perr != nil {
		t.Fatalf("buildClaudeRewindPlan error: %v", perr)
	}
	if !plan.Ambiguous {
		t.Fatal("expected Ambiguous=true for duplicate user message first line")
	}
	fp := &fakeClaudeRewindPane{curScreen: ccNormalScreen("❯ continue", "")}
	_, err := claudeRewind(context.Background(), fp, plan)
	if err == nil || err.code != "ambiguous_message" {
		t.Fatalf("got err=%v, want ambiguous_message", err)
	}
	if len(fp.recordedKeys()) != 0 {
		t.Fatalf("expected no keys sent for ambiguous plan, got %v", fp.recordedKeys())
	}

	// Case 2: picker screen itself displays two rows with matching first line.
	fp2 := &fakeClaudeRewindPane{curScreen: ccNormalScreen("❯ continue", "")}
	fp2.onPress = func(_ context.Context, p *fakeClaudeRewindPane, keys []string) error {
		switch strings.Join(keys, "+") {
		case "esc+esc":
			p.curScreen = ccPickerScreen("above", []string{"continue", "continue", "(current)"}, 2)
		case "esc":
			p.curScreen = ccNormalScreen("❯ continue", "")
		}
		return nil
	}
	_, err = claudeRewind(context.Background(), fp2, claudeRewindPlan{Steps: 1, TargetText: "continue"})
	if err == nil || err.code != "ambiguous_message" {
		t.Fatalf("got err=%v, want ambiguous_message", err)
	}
	wantKeys := []string{"esc+esc", "esc"}
	if got := fp2.recordedKeys(); !slices.Equal(got, wantKeys) {
		t.Fatalf("keys = %v, want %v", got, wantKeys)
	}
}

func TestClaudeRewindIgnoresQuotedConfirmInChat(t *testing.T) {
	// Conversation transcript above the normal prompt box quotes an entire
	// Rewind confirm screen including the ▔ overline divider.
	quotedChat := strings.Join([]string{
		"❯ Look at this screen:",
		"● Sure, here is what Claude Code shows:",
		ccOverline,
		"Restore to the point before you sent this message:",
		"│ fix login",
		"❯ 1. Restore conversation",
		"  2. Never mind",
		"Enter to continue · Esc to cancel",
	}, "\n")
	normal := ccNormalScreen(quotedChat, "")
	if _, ok := parseRewindConfirm(normal); ok {
		t.Fatal("parseRewindConfirm matched quoted confirm text above normal input box")
	}
	if _, ok := parseRewindPicker(normal); ok {
		t.Fatal("parseRewindPicker matched quoted text above normal input box")
	}

	// Also when a real picker is open BELOW quoted confirm text in transcript:
	// only the region below the LAST ▔ divider is parsed.
	withRealPicker := ccPickerScreen(quotedChat, []string{"fix login", "(current)"}, 1)
	if _, ok := parseRewindConfirm(withRealPicker); ok {
		t.Fatal("parseRewindConfirm matched old quoted confirm above real picker")
	}
	pk, ok := parseRewindPicker(withRealPicker)
	if !ok || !pk.onCurrent {
		t.Fatalf("expected real picker parsed below last overline, got pk=%+v ok=%v", pk, ok)
	}
}

func TestClaudeRewindMidFlowIdentityChanges(t *testing.T) {
	fastRewindTimings(t)
	cases := []struct {
		name string
		code string
	}{
		{"session UUID changed mid-flow", "session_changed"},
		{"terminal ID changed mid-flow", "terminal_changed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fp, plan := newStandardRewindFake(nil)
			orig := fp.onPress
			fp.onPress = func(ctx context.Context, p *fakeClaudeRewindPane, keys []string) error {
				if err := orig(ctx, p, keys); err != nil {
					return err
				}
				// Right after first "up", identity changes!
				if strings.Join(keys, "+") == "up" {
					p.verifyErr = rwErr(http.StatusConflict, tc.code, tc.name)
				}
				return nil
			}
			_, err := claudeRewind(context.Background(), fp, plan)
			if err == nil || err.code != tc.code {
				t.Fatalf("got err=%v, want %s", err, tc.code)
			}
			// Must stop immediately and NEVER press Esc onto the changed session/terminal.
			wantKeys := []string{"esc+esc", "up"}
			if got := fp.recordedKeys(); !slices.Equal(got, wantKeys) {
				t.Fatalf("keys = %v, want %v (must not send keys after identity change)", got, wantKeys)
			}
		})
	}
}

func TestClaudeRewindCancelCleansUpOnlyWhenPanelOpen(t *testing.T) {
	fastRewindTimings(t)

	t.Run("panel still open sends Esc with independent context", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		fp, plan := newStandardRewindFake(nil)
		orig := fp.onPress
		fp.onPress = func(pctx context.Context, p *fakeClaudeRewindPane, keys []string) error {
			err := orig(pctx, p, keys)
			if strings.Join(keys, "+") == "up" {
				cancel() // request cancelled while picker is open
			}
			return err
		}
		_, err := claudeRewind(ctx, fp, plan)
		if err == nil {
			t.Fatal("expected cancellation error")
		}
		keys := fp.recordedKeys()
		if len(keys) == 0 || keys[len(keys)-1] != "esc" {
			t.Fatalf("expected esc cleanup after cancel while panel open, keys=%v", keys)
		}
	})

	t.Run("panel already closed does not send Esc", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		fp, plan := newStandardRewindFake(nil)
		orig := fp.onPress
		fp.onPress = func(pctx context.Context, p *fakeClaudeRewindPane, keys []string) error {
			err := orig(pctx, p, keys)
			if strings.Join(keys, "+") == "up" {
				// Simulate user already closed picker in terminal before cancel.
				p.curScreen = ccNormalScreen("❯ first prompt", "")
				cancel()
			}
			return err
		}
		_, err := claudeRewind(ctx, fp, plan)
		if err == nil {
			t.Fatal("expected cancellation error")
		}
		keys := fp.recordedKeys()
		if keys[len(keys)-1] == "esc" {
			t.Fatalf("must NOT press esc when panel is already closed, keys=%v", keys)
		}
	})
}

func TestClaudeRewindCommitErrorButScreenRewound(t *testing.T) {
	fastRewindTimings(t)
	fp, plan := newStandardRewindFake(nil)
	enters := 0
	orig := fp.onPress
	fp.onPress = func(ctx context.Context, p *fakeClaudeRewindPane, keys []string) error {
		if strings.Join(keys, "+") == "enter" {
			enters++
			if enters == 2 {
				// Enter actually took effect on host, then transport dropped.
				_ = orig(ctx, p, keys)
				return errors.New("ssh transport lost")
			}
		}
		return orig(ctx, p, keys)
	}

	res, err := claudeRewind(context.Background(), fp, plan)
	if err != nil {
		t.Fatalf("expected success when screen confirms rewind despite Enter err, got %v", err)
	}
	if !res.OK || res.Warning != "" {
		t.Fatalf("unexpected result: %+v", res)
	}
	for _, k := range fp.recordedKeys() {
		if k == "esc" {
			t.Fatalf("must NEVER press Esc after commit Enter, keys=%v", fp.recordedKeys())
		}
	}
}

func TestClaudeRewindCommitUncertain(t *testing.T) {
	fastRewindTimings(t)
	fp, plan := newStandardRewindFake(nil)
	enters := 0
	orig := fp.onPress
	fp.onPress = func(ctx context.Context, p *fakeClaudeRewindPane, keys []string) error {
		if strings.Join(keys, "+") == "enter" {
			enters++
			if enters == 2 {
				// Commit Enter leaves screen stuck on confirm (cannot confirm rewind).
				return errors.New("ssh timeout")
			}
		}
		return orig(ctx, p, keys)
	}

	res, err := claudeRewind(context.Background(), fp, plan)
	if err == nil || err.code != "rewind_uncertain" {
		t.Fatalf("got res=%+v err=%v, want rewind_uncertain", res, err)
	}
	for _, k := range fp.recordedKeys() {
		if k == "esc" {
			t.Fatalf("must NEVER press Esc after commit Enter, keys=%v", fp.recordedKeys())
		}
	}
}

func TestClaudeRewindRefillNotCleared(t *testing.T) {
	fastRewindTimings(t)
	fp, plan := newStandardRewindFake(nil)
	orig := fp.onPress
	fp.onPress = func(ctx context.Context, p *fakeClaudeRewindPane, keys []string) error {
		if strings.Join(keys, "+") == "ctrl+c" {
			return nil // ctrl+c fails to clear input box
		}
		return orig(ctx, p, keys)
	}

	res, err := claudeRewind(context.Background(), fp, plan)
	if err != nil {
		t.Fatalf("expected OK with warning, got err=%v", err)
	}
	if !res.OK || res.Warning != "input_not_cleared" {
		t.Fatalf("got %+v, want OK with warning=input_not_cleared", res)
	}
}

func TestClaudeRewindTmuxPanePIDCheck(t *testing.T) {
	const sid = "11111111-2222-3333-4444-555555555555"
	// Script must query #{pane_pid}, never tmux server #{pid}.
	paneSh := claudeRewindTmuxPaneSh("%7")
	if !strings.Contains(paneSh, "#{pane_pid}") || strings.Contains(paneSh, "#{pid}") {
		t.Fatalf("tmux pane query must use #{pane_pid}, got: %s", paneSh)
	}
	checkSh := claudeRewindCheckSh("tmux", "%7", "", "999", 123, 456, sid)
	if !strings.Contains(checkSh, "#{pane_pid}") || strings.Contains(checkSh, "#{pid}") {
		t.Fatalf("tmux check script must use #{pane_pid}, got: %s", checkSh)
	}

	// Execute claudeRewindCheckSh with a stubbed `tmux` binary in PATH to prove
	// pane_pid mismatch exits 34 (terminal_changed) and sessionId mismatch exits 33.
	if _, err := os.Stat("/proc/self/stat"); err != nil {
		t.Skip("no /proc")
	}
	bin := t.TempDir()
	home := t.TempDir()
	pid := os.Getpid()
	statBytes, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		t.Fatal(err)
	}
	rest := string(statBytes)
	rest = rest[strings.LastIndex(rest, ") ")+2:]
	fields := strings.Fields(rest)
	start, _ := strconv.ParseInt(fields[19], 10, 64)
	tty, _ := os.Readlink("/proc/" + strconv.Itoa(pid) + "/fd/0")

	writeTmuxStub := func(panePID, dead string) {
		script := "#!/bin/sh\n" +
			"for a in \"$@\"; do last=$a; done\n" +
			"case \"$last\" in\n" +
			"  *pane_pid*) echo '" + panePID + " " + dead + "' ;;\n" +
			"  *pane_tty*) echo '" + tty + "' ;;\n" +
			"esac\n"
		if err := os.WriteFile(filepath.Join(bin, "tmux"), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	sessDir := filepath.Join(home, ".claude", "sessions")
	if err := os.MkdirAll(sessDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sessDir, strconv.Itoa(pid)+".json"),
		[]byte(`{"sessionId":"`+sid+`","procStart":`+strconv.FormatInt(start, 10)+`}`), 0o644); err != nil {
		t.Fatal(err)
	}

	runCheck := func(expectPanePID, expectSID string) int {
		sh := claudeRewindCheckSh("tmux", "%7", "", expectPanePID, int64(pid), start, expectSID)
		// Prepend bin ahead of PATH set by script so stub tmux wins.
		cmd := exec.Command("sh", "-c", `PATH=`+bin+`:"$PATH"; `+strings.Replace(sh, `PATH="`, `PATH="`+bin+`:`, 1))
		cmd.Env = append(os.Environ(), "HOME="+home)
		err := cmd.Run()
		if err == nil {
			return 0
		}
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return ee.ExitCode()
		}
		t.Fatalf("unexpected run err: %v", err)
		return -1
	}

	writeTmuxStub("4242", "0")
	if code := runCheck("9999", sid); code != 34 {
		t.Fatalf("pane_pid mismatch exited %d, want 34 (terminal_changed)", code)
	}
	writeTmuxStub("4242", "1")
	if code := runCheck("4242", sid); code != 34 {
		t.Fatalf("dead pane exited %d, want 34 (terminal_changed)", code)
	}
	if re := mapRewindExitErr(&sshxExitStub{34}); !isIdentityErr(re) || asRewindErr(re).code != "terminal_changed" {
		t.Fatalf("exit 34 mapped to %v, want terminal_changed", re)
	}
	if re := mapRewindExitErr(&sshxExitStub{33}); !isIdentityErr(re) || asRewindErr(re).code != "session_changed" {
		t.Fatalf("exit 33 mapped to %v, want session_changed", re)
	}
	if re := mapRewindExitErr(&sshxExitStub{31}); !isIdentityErr(re) || asRewindErr(re).code != "process_changed" {
		t.Fatalf("exit 31 mapped to %v, want process_changed", re)
	}
}

type sshxExitStub struct{ status int }

func (s *sshxExitStub) Error() string { return "exit" }
func (s *sshxExitStub) As(target any) bool {
	if p, ok := target.(**sshx.ExitError); ok {
		*p = &sshx.ExitError{Status: s.status}
		return true
	}
	return false
}

func TestClaudeRewindCrossConnectionMutex(t *testing.T) {
	srv := &Server{herdrSocks: herdrSockets{path: map[string]string{}}}
	s1 := &term.Session{ID: "conn-1", OwnerID: 7, HostID: 3, Persistence: "herdr"}
	s2 := &term.Session{ID: "conn-2", OwnerID: 7, HostID: 3, Persistence: "herdr"}
	srv.herdrSocks.set(s1.ID, "/tmp/herdr.sock")
	srv.herdrSocks.set(s2.ID, "/tmp/herdr.sock")

	k1 := srv.convoReplyKey(s1, "w1:p1")
	k2 := srv.convoReplyKey(s2, "w1:p1")
	if k1 != k2 {
		t.Fatalf("different Corral connections to same herdr pane got different lock keys: %q vs %q", k1, k2)
	}

	lock1, err := srv.replies.lock(context.Background(), k1)
	if err != nil {
		t.Fatal(err)
	}
	// Second connection trying to lock the same physical pane must block until released.
	shortCtx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, err := srv.replies.lock(shortCtx, k2); err == nil {
		t.Fatal("second connection acquired lock while first still held it")
	}

	acquired := make(chan *paneReply, 1)
	go func() {
		l, err := srv.replies.lock(context.Background(), k2)
		if err == nil {
			acquired <- l
		}
	}()
	time.Sleep(15 * time.Millisecond)
	select {
	case <-acquired:
		t.Fatal("second connection acquired lock before first unlocked")
	default:
	}

	srv.replies.unlock(lock1)
	select {
	case lock2 := <-acquired:
		srv.replies.unlock(lock2)
	case <-time.After(time.Second):
		t.Fatal("second connection did not acquire lock after first unlocked")
	}
}

func TestHerdrPaneSnapshotImmutableAndRaceFree(t *testing.T) {
	const claudeUUID = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	snap := &herdr.Snapshot{
		Panes: []herdr.Pane{
			{
				ID:           "w1:p1",
				Agent:        "claude",
				AgentSession: &herdr.AgentSessionInfo{Source: "herdr:claude", Agent: "claude", Value: claudeUUID},
				Tokens:       map[string]string{"role": "main"},
			},
			{
				ID:           "w1:p2",
				Agent:        "claude",
				AgentSession: &herdr.AgentSessionInfo{Source: "herdr:claude", Agent: "claude", Value: "cuddly-dime"},
			},
			{
				ID:    "w1:p3",
				Agent: "claude",
			},
		},
		Agents: []herdr.Pane{
			{
				ID:          "w1:p1",
				Agent:       "claude",
				AgentStatus: herdr.StatusIdle,
				TerminalID:  "term-1",
				// Agents entry has no AgentSession, matching real herdr behavior.
			},
		},
	}

	// Concurrent readers mutate their returned copies (including Tokens map
	// and AgentSession struct). Under -race, sharing any map/pointer fails.
	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 100 {
				p, ok := herdrPaneSnapshot(snap, "w1:p1")
				if !ok {
					t.Error("pane w1:p1 not found")
					return
				}
				if p.TerminalID != "term-1" || p.AgentStatus != herdr.StatusIdle {
					t.Errorf("missing Agents fields: %+v", p)
					return
				}
				if got := herdrValidatedUUID(p); got != claudeUUID {
					t.Errorf("missing Panes AgentSession: got %q, want %q", got, claudeUUID)
					return
				}
				// Mutate returned copy aggressively.
				p.Tokens["mutated"] = "yes"
				p.AgentSession.Value = "mutated"
			}
		}()
	}
	wg.Wait()

	if snap.Panes[0].AgentSession.Value != claudeUUID {
		t.Fatalf("snapshot AgentSession was mutated: %q", snap.Panes[0].AgentSession.Value)
	}
	if _, leaked := snap.Panes[0].Tokens["mutated"]; leaked {
		t.Fatal("snapshot Tokens map was mutated")
	}

	// Devin hook pollution ("cuddly-dime") must be ignored; missing AgentSession allowed.
	p2, ok := herdrPaneSnapshot(snap, "w1:p2")
	if !ok || herdrValidatedUUID(p2) != "" {
		t.Fatalf("non-UUID session should be ignored, got %q", herdrValidatedUUID(p2))
	}
	p3, ok := herdrPaneSnapshot(snap, "w1:p3")
	if !ok || herdrValidatedUUID(p3) != "" {
		t.Fatalf("missing AgentSession should yield empty UUID, got %q", herdrValidatedUUID(p3))
	}
}

func TestClaudeRewindOverlayLifecycle(t *testing.T) {
	srv := &Server{herdrSocks: herdrSockets{path: map[string]string{}}}
	s1 := &term.Session{ID: "conn-1", OwnerID: 1, HostID: 1, Persistence: "herdr"}
	s2 := &term.Session{ID: "conn-2", OwnerID: 1, HostID: 1, Persistence: "herdr"}
	srv.herdrSocks.set(s1.ID, "/tmp/h.sock")
	srv.herdrSocks.set(s2.ID, "/tmp/h.sock")

	const sid = "11111111-1111-1111-1111-111111111111"
	const pane = "w1:p1"
	const pid, start = int64(500), int64(9000)

	// Log already contains an earlier rewind (u2old -> u2) plus u3 after u2.
	t0 := time.Date(2026, 11, 26, 10, 0, 0, 0, time.UTC)
	ts := func(d time.Duration) string { return t0.Add(d).Format(time.RFC3339Nano) }
	baseLog := strings.Join([]string{
		`{"type":"user","uuid":"u1","parentUuid":null,"timestamp":"` + ts(1*time.Minute) + `","message":{"role":"user","content":"first"}}`,
		`{"type":"assistant","uuid":"a1","parentUuid":"u1","timestamp":"` + ts(2*time.Minute) + `","message":{"content":[{"type":"text","text":"ans1"}]}}`,
		`{"type":"user","uuid":"u2old","parentUuid":"a1","timestamp":"` + ts(3*time.Minute) + `","message":{"role":"user","content":"old try"}}`,
		`{"type":"assistant","uuid":"a2old","parentUuid":"u2old","timestamp":"` + ts(4*time.Minute) + `","message":{"content":[{"type":"text","text":"old ans"}]}}`,
		`{"type":"user","uuid":"u2","parentUuid":"a1","timestamp":"` + ts(5*time.Minute) + `","message":{"role":"user","content":"second"}}`,
		`{"type":"assistant","uuid":"a2","parentUuid":"u2","timestamp":"` + ts(6*time.Minute) + `","message":{"content":[{"type":"text","text":"ans2"}]}}`,
		`{"type":"user","uuid":"u3","parentUuid":"a2","timestamp":"` + ts(7*time.Minute) + `","message":{"role":"user","content":"third"}}`,
		`{"type":"assistant","uuid":"a3","parentUuid":"u3","timestamp":"` + ts(8*time.Minute) + `","message":{"content":[{"type":"text","text":"ans3"}]}}`,
	}, "\n") + "\n"
	srv.rewinds.now = func() time.Time { return t0.Add(10 * time.Minute) }

	makeState := func(p, stTime int64, raw string) *convoState {
		cl := convo.NewClaude()
		cl.Feed([]byte(raw))
		return &convoState{
			gen:    convoGens.Add(1),
			agent:  "claude",
			sid:    sid,
			parser: cl,
			bind:   &convoBind{Pane: pane, PID: p, Start: stTime, Session: sid, Agent: "claude"},
		}
	}

	goneSet := func(es []convo.Entry) map[string]bool {
		m := map[string]bool{}
		for _, e := range es {
			if e.Gone {
				m[e.ID] = true
			}
		}
		return m
	}

	// Record rewind to before u2 using knownUsers from current log.
	st1 := makeState(pid, start, baseLog)
	known := st1.parser.(*convo.Claude).UserIDs()
	key := rewindKey{paneKey: srv.convoReplyKey(s1, pane), sid: sid, pid: pid, start: start}
	srv.rewinds.set(key, "u2", known)

	// 1. Immediate view on conn-1 sees u2, a2, u3, a3 as Gone (plus u2old/a2old).
	genBefore := st1.gen
	target := srv.syncClaudeRewind(s1, st1)
	if target != "u2" || st1.gen == genBefore {
		t.Fatalf("expected active rewind u2 and gen bump, got target=%q genChanged=%v", target, st1.gen != genBefore)
	}
	es1 := overlayRewindEntries(st1, target, st1.parser.Since(0))
	g1 := goneSet(es1)
	if g1["u1"] || g1["a1"] || !g1["u2"] || !g1["a2"] || !g1["u3"] || !g1["a3"] {
		t.Fatalf("conn-1 gone set wrong: %v", g1)
	}

	// 2. State eviction + rebuild from the exact same log MUST NOT delete the
	// rewind record (even though baseLog has earlier rewind u2 -> a1 and later u3).
	stRebuilt := makeState(pid, start, baseLog)
	if target := srv.syncClaudeRewind(s1, stRebuilt); target != "u2" {
		t.Fatalf("state rebuild mistakenly dropped rewind record: target=%q", target)
	}
	if srv.rewinds.get(key) == nil {
		t.Fatal("rewind record deleted on state rebuild")
	}

	// 3. Another Corral connection (conn-2) viewing the same pane + process sees Gone too,
	// even after loading earlier history (not in knownUsers) with timestamps before rewind.
	st2 := makeState(pid, start, baseLog)
	earlier := convo.NewClaude()
	earlier.Feed([]byte(strings.Join([]string{
		`{"type":"user","uuid":"u0","parentUuid":null,"timestamp":"` + ts(-5*time.Minute) + `","message":{"role":"user","content":"zero"}}`,
		`{"type":"assistant","uuid":"a0","parentUuid":"u0","timestamp":"` + ts(-4*time.Minute) + `","message":{"content":[{"type":"text","text":"ans0"}]}}`,
		`{"type":"user","uuid":"u0b","parentUuid":"a0","timestamp":"` + ts(-3*time.Minute) + `","message":{"role":"user","content":"zero retry"}}`,
	}, "\n") + "\n"))
	st2.parser.Prepend(earlier)
	target2 := srv.syncClaudeRewind(s2, st2)
	if target2 != "u2" {
		t.Fatalf("conn-2 did not keep rewind record after prepending earlier history: target=%q", target2)
	}
	if srv.rewinds.get(key) == nil {
		t.Fatal("rewind record deleted when conn-2 loaded earlier history")
	}
	g2 := goneSet(overlayRewindEntries(st2, target2, st2.parser.Since(0)))
	if !g2["u2"] || !g2["u3"] || g2["u1"] {
		t.Fatalf("conn-2 gone set wrong: %v", g2)
	}

	// 4. Process restart (new pid or new start) MUST NOT apply the old rewind record.
	stRestart := makeState(pid+1, start+100, baseLog)
	if targetR := srv.syncClaudeRewind(s1, stRestart); targetR != "" {
		t.Fatalf("restarted process should not inherit rewind record, got %q", targetR)
	}
	gR := goneSet(overlayRewindEntries(stRestart, "", stRestart.parser.Since(0)))
	if gR["u2"] || gR["u3"] {
		t.Fatalf("restarted process wrongly marked u2/u3 gone: %v", gR)
	}

	// 5. New user message hanging on a1 (before u2) with timestamp after rewind settles the rewind.
	newMsg := `{"type":"user","uuid":"u4","parentUuid":"a1","timestamp":"` + ts(11*time.Minute) + `","message":{"role":"user","content":"new direction"}}` + "\n"
	st1.parser.Feed([]byte(newMsg))
	genBeforeSettle := st1.gen
	if targetAfter := srv.syncClaudeRewind(s1, st1); targetAfter != "" {
		t.Fatalf("expected rewind record cleared after new user message, got %q", targetAfter)
	}
	if st1.gen == genBeforeSettle {
		t.Fatal("expected gen bump when rewind record cleared")
	}
	if srv.rewinds.get(key) != nil {
		t.Fatal("rewind record still present in store after log took over")
	}
	gAfter := goneSet(st1.parser.Since(0))
	if gAfter["u1"] || gAfter["a1"] || !gAfter["u2"] || !gAfter["u3"] || gAfter["u4"] {
		t.Fatalf("post-settle native gone set wrong: %v", gAfter)
	}
}

// setupFakeRewindHost creates a fake /proc tree, HOME/.claude/sessions/<pid>.json,
// and bin directory for executing real claudeRewind* shell scripts in unit tests.
func setupFakeRewindHost(t *testing.T, pid, start int64, sid, pane, sock string) (binDir, homeDir, fakeProc, sessFile string) {
	t.Helper()
	binDir = t.TempDir()
	homeDir = t.TempDir()
	fakeProc = t.TempDir()
	pdir := filepath.Join(fakeProc, strconv.FormatInt(pid, 10))
	if err := os.MkdirAll(filepath.Join(pdir, "fd"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/dev/pts/9", filepath.Join(pdir, "fd", "0")); err != nil {
		t.Fatal(err)
	}
	envBytes := "HERDR_PANE_ID=" + pane + "\x00HERDR_SOCKET_PATH=" + sock + "\x00"
	if err := os.WriteFile(filepath.Join(pdir, "environ"), []byte(envBytes), 0o644); err != nil {
		t.Fatal(err)
	}
	// /proc/$pid/stat: after ") ", f3=pgrp(pid), f6=tpgid(pid), f20=start.
	statLine := strconv.FormatInt(pid, 10) + " (claude) S 1 " + strconv.FormatInt(pid, 10) + " " + strconv.FormatInt(pid, 10) +
		" 34816 " + strconv.FormatInt(pid, 10) + " 0 0 0 0 0 0 0 0 0 0 0 0 0 " + strconv.FormatInt(start, 10) + " 0\n"
	if err := os.WriteFile(filepath.Join(pdir, "stat"), []byte(statLine), 0o644); err != nil {
		t.Fatal(err)
	}
	sessDir := filepath.Join(homeDir, ".claude", "sessions")
	if err := os.MkdirAll(sessDir, 0o755); err != nil {
		t.Fatal(err)
	}
	sessFile = filepath.Join(sessDir, strconv.FormatInt(pid, 10)+".json")
	if err := os.WriteFile(sessFile, []byte(`{"sessionId":"`+sid+`","procStart":`+strconv.FormatInt(start, 10)+`}`), 0o644); err != nil {
		t.Fatal(err)
	}
	return binDir, homeDir, fakeProc, sessFile
}

func runRewindSh(t *testing.T, sh, binDir, homeDir, fakeProc string) (string, int) {
	t.Helper()
	patched := strings.Replace(sh, `PATH="`, `PATH="`+binDir+`:`, 1)
	patched = strings.ReplaceAll(patched, `"/proc/`, `"`+fakeProc+`/`)
	cmd := exec.Command("sh", "-c", patched)
	cmd.Env = append(os.Environ(), "HOME="+homeDir)
	out, err := cmd.CombinedOutput()
	if err == nil {
		return string(out), 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return string(out), ee.ExitCode()
	}
	t.Fatalf("unexpected exec err: %v (out=%q)", err, string(out))
	return "", -1
}

// Item 1: Atomic remote commit-Enter script checks identity + live captured screen
// in the same shell invocation before issuing Enter.
func TestClaudeRewindCommitScriptAtomicCheck(t *testing.T) {
	const (
		sid  = "11111111-2222-3333-4444-555555555555"
		pid  = int64(4242)
		start = int64(987654)
		sock = "/tmp/test-herdr.sock"
	)
	opts := []string{"Restore code and conversation", "Restore conversation", "Restore code", "Never mind"}

	for _, mode := range []string{"tmux", "herdr"} {
		t.Run(mode, func(t *testing.T) {
			pane := "%7"
			if mode == "herdr" {
				pane = "w1:p1"
			}
			binDir, homeDir, fakeProc, _ := setupFakeRewindHost(t, pid, start, sid, pane, sock)
			scrFile := filepath.Join(t.TempDir(), "screen.txt")
			keyLog := filepath.Join(t.TempDir(), "keys.log")

			if mode == "tmux" {
				stub := "#!/bin/sh\n" +
					"case \"$1\" in\n" +
					"  -u) shift ;;\n" +
					"esac\n" +
					"case \"$1\" in\n" +
					"  display-message)\n" +
					"    for a in \"$@\"; do last=$a; done\n" +
					"    case \"$last\" in\n" +
					"      *pane_pid*) echo '9001 0' ;;\n" +
					"      *pane_tty*) echo '/dev/pts/9' ;;\n" +
					"    esac ;;\n" +
					"  capture-pane) cat " + sshx.QuoteShell(scrFile) + " ;;\n" +
					"  send-keys) echo \"$*\" >> " + sshx.QuoteShell(keyLog) + " ;;\n" +
					"esac\n"
				if err := os.WriteFile(filepath.Join(binDir, "tmux"), []byte(stub), 0o755); err != nil {
					t.Fatal(err)
				}
			} else {
				stub := "#!/bin/sh\n" +
					"if [ \"$1\" = pane ] && [ \"$2\" = read ]; then\n" +
					"  cat " + sshx.QuoteShell(scrFile) + "\n" +
					"elif [ \"$1\" = pane ] && [ \"$2\" = send-keys ]; then\n" +
					"  echo \"$*\" >> " + sshx.QuoteShell(keyLog) + "\n" +
					"fi\n"
				if err := os.WriteFile(filepath.Join(binDir, "herdr"), []byte(stub), 0o755); err != nil {
					t.Fatal(err)
				}
			}

			run := func(screen, target string) (string, int, string) {
				_ = os.Remove(keyLog)
				if err := os.WriteFile(scrFile, []byte(screen), 0o644); err != nil {
					t.Fatal(err)
				}
				sh, err := claudeRewindCommitSh(mode, pane, sock, "9001", pid, start, sid, target)
				if err != nil {
					t.Fatal(err)
				}
				out, code := runRewindSh(t, sh, binDir, homeDir, fakeProc)
				logged, _ := os.ReadFile(keyLog)
				return out, code, string(logged)
			}

			// 1) Option changed to "Restore code and conversation" -> MUST exit 35 and NEVER send Enter.
			_, code, keys := run(ccConfirmScreen("above", []string{"fix login", "second line"}, opts, 0), "fix login\nsecond line")
			if code != 35 || keys != "" {
				t.Fatalf("code restore selected: code=%d keys=%q, want code=35 keys=\"\"", code, keys)
			}

			// 2) Wrong quoted message even with "Restore conversation" selected -> MUST exit 35, no Enter.
			_, code, keys = run(ccConfirmScreen("above", []string{"other prompt"}, opts, 1), "fix login\nsecond line")
			if code != 35 || keys != "" {
				t.Fatalf("quote mismatch: code=%d keys=%q, want code=35 keys=\"\"", code, keys)
			}

			// 3) Quoted confirm screen above normal prompt box -> MUST exit 35, no Enter.
			fakeAbove := ccNormalScreen(ccConfirmScreen("chat", []string{"fix login"}, opts, 1), "")
			_, code, keys = run(fakeAbove, "fix login")
			if code != 35 || keys != "" {
				t.Fatalf("quoted confirm above prompt box: code=%d keys=%q, want code=35 keys=\"\"", code, keys)
			}

			// 4) Regex metacharacters in target are compared as fixed strings, not regex.
			metaTarget := `fix foo.*bar [a-z]+ (test) $1 \n`
			_, code, keys = run(ccConfirmScreen("above", []string{`fix fooXbar a (test) $1 \n`}, opts, 1), metaTarget)
			if code != 35 || keys != "" {
				t.Fatalf("regex metachar spoof matched: code=%d keys=%q, want code=35 keys=\"\"", code, keys)
			}
			out, code, keys := run(ccConfirmScreen("above", []string{metaTarget}, opts, 1), metaTarget)
			if code != 0 || strings.TrimSpace(out) != "ok" || keys == "" {
				t.Fatalf("exact regex metachar target failed: code=%d out=%q keys=%q", code, out, keys)
			}

			// 5) Valid "Restore conversation" + matching first line -> exits 0, sends Enter.
			out, code, keys = run(ccConfirmScreen("above", []string{"fix login", "second line"}, opts, 1), "fix login\nsecond line")
			if code != 0 || strings.TrimSpace(out) != "ok" || keys == "" {
				t.Fatalf("valid commit failed: code=%d out=%q keys=%q", code, out, keys)
			}
			// 6) Wrapped / ellipsis-truncated long first line -> exits 0, sends Enter.
			longFirst := "refactor authentication middleware to validate signed cookies properly"
			out, code, keys = run(ccConfirmScreen("above", []string{"refactor authentication middleware…"}, opts, 1), longFirst)
			if code != 0 || strings.TrimSpace(out) != "ok" || keys == "" {
				t.Fatalf("ellipsis commit failed: code=%d out=%q keys=%q", code, out, keys)
			}
		})
	}
	if re := mapRewindExitErr(&sshxExitStub{35}); asRewindErr(re).code != "confirm_mismatch" || isIdentityErr(re) {
		t.Fatalf("exit 35 mapped to %v, want non-identity confirm_mismatch", re)
	}
}

func TestClaudeRewindCommitRemoteMismatchRefusesEnter(t *testing.T) {
	fastRewindTimings(t)
	opts := []string{"Restore code and conversation", "Restore conversation", "Restore code", "Never mind"}
	fp, plan := newStandardRewindFake(opts)
	commitCalls := 0
	fp.onCommitEnter = func(_ context.Context, _ *fakeClaudeRewindPane, _ string) error {
		commitCalls++
		// Simulate remote atomic check exiting 35 without sending Enter.
		return mapRewindExitErr(&sshxExitStub{35})
	}

	_, err := claudeRewind(context.Background(), fp, plan)
	if err == nil || err.code != "confirm_mismatch" {
		t.Fatalf("got err=%v, want confirm_mismatch", err)
	}
	if commitCalls != 1 {
		t.Fatalf("commitEnter calls = %d, want 1", commitCalls)
	}
	keys := fp.recordedKeys()
	enters := 0
	for _, k := range keys {
		if k == "enter" {
			enters++
		}
	}
	if enters != 1 {
		t.Fatalf("expected only picker enter (1), got %d, keys=%v", enters, keys)
	}
	if keys[len(keys)-1] != "esc" {
		t.Fatalf("expected esc cleanup after remote mismatch refused commit, keys=%v", keys)
	}
}

// Item 2: Identity is re-checked between consecutive keys (especially Esc, Esc)
// inside the same remote shell script.
func TestClaudeRewindPressChecksIdentityBetweenEscs(t *testing.T) {
	const (
		sid   = "11111111-2222-3333-4444-555555555555"
		sid2  = "99999999-2222-3333-4444-555555555555"
		pid   = int64(4242)
		start = int64(987654)
		sock  = "/tmp/test-herdr.sock"
	)

	for _, mode := range []string{"tmux", "herdr"} {
		t.Run(mode, func(t *testing.T) {
			pane := "%7"
			if mode == "herdr" {
				pane = "w1:p1"
			}
			binDir, homeDir, fakeProc, sessFile := setupFakeRewindHost(t, pid, start, sid, pane, sock)
			keyLog := filepath.Join(t.TempDir(), "keys.log")
			// After the first key is sent, mutate sessionId so the second key's check MUST fail (exit 33).
			mutate := `printf '{"sessionId":"` + sid2 + `","procStart":` + strconv.FormatInt(start, 10) + `}' > ` + sshx.QuoteShell(sessFile)

			if mode == "tmux" {
				stub := "#!/bin/sh\n" +
					"case \"$1\" in -u) shift ;; esac\n" +
					"case \"$1\" in\n" +
					"  display-message)\n" +
					"    for a in \"$@\"; do last=$a; done\n" +
					"    case \"$last\" in\n" +
					"      *pane_pid*) echo '9001 0' ;;\n" +
					"      *pane_tty*) echo '/dev/pts/9' ;;\n" +
					"    esac ;;\n" +
					"  send-keys)\n" +
					"    echo \"$*\" >> " + sshx.QuoteShell(keyLog) + "\n" +
					"    " + mutate + " ;;\n" +
					"esac\n"
				if err := os.WriteFile(filepath.Join(binDir, "tmux"), []byte(stub), 0o755); err != nil {
					t.Fatal(err)
				}
			} else {
				stub := "#!/bin/sh\n" +
					"if [ \"$1\" = pane ] && [ \"$2\" = send-keys ]; then\n" +
					"  echo \"$*\" >> " + sshx.QuoteShell(keyLog) + "\n" +
					"  " + mutate + "\n" +
					"fi\n"
				if err := os.WriteFile(filepath.Join(binDir, "herdr"), []byte(stub), 0o755); err != nil {
					t.Fatal(err)
				}
			}

			sh, err := claudeRewindPressSh(mode, pane, sock, "9001", pid, start, sid, []string{"esc", "esc"})
			if err != nil {
				t.Fatal(err)
			}
			_, code := runRewindSh(t, sh, binDir, homeDir, fakeProc)
			if code != 33 {
				t.Fatalf("expected exit 33 (session_changed) before 2nd esc, got %d", code)
			}
			logged, _ := os.ReadFile(keyLog)
			sent := strings.Split(strings.TrimSpace(string(logged)), "\n")
			if len(sent) != 1 || sent[0] == "" {
				t.Fatalf("expected exactly 1 key sent before identity check failed, got %q", string(logged))
			}
		})
	}
}

// Item 3: Cleanup waits for in-flight remote commands to complete before reading
// screen and deciding whether to press Esc.
func TestClaudeRewindCancelWaitsInFlightBeforeCleanup(t *testing.T) {
	fastRewindTimings(t)
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	aboveAll := "❯ first prompt\n● First answer\n\n❯ fix login\n● Second answer"
	items := []string{"first prompt", "fix login", "(current)"}
	fp := &fakeClaudeRewindPane{
		curScreen: ccNormalScreen(aboveAll, ""),
		inFlight:  &wg,
	}
	fp.onPress = func(pctx context.Context, p *fakeClaudeRewindPane, keys []string) error {
		k := strings.Join(keys, "+")
		if k == "esc+esc" {
			// Simulate sshx.Exec returning immediately on cancel while the remote
			// command finishes 15ms later and opens the rewind picker.
			wg.Add(1)
			go func() {
				defer wg.Done()
				time.Sleep(15 * time.Millisecond)
				p.mu.Lock()
				p.curScreen = ccPickerScreen(aboveAll, items, len(items)-1)
				p.mu.Unlock()
			}()
			cancel()
			return context.Canceled
		}
		if k == "esc" {
			p.curScreen = ccNormalScreen(aboveAll, "")
		}
		return nil
	}

	_, err := claudeRewind(ctx, fp, claudeRewindPlan{Steps: 1, TargetText: "fix login"})
	if err == nil {
		t.Fatal("expected cancellation error")
	}
	keys := fp.recordedKeys()
	if len(keys) < 2 || keys[len(keys)-1] != "esc" {
		t.Fatalf("cleanup did not wait for late esc+esc and close picker; keys=%v", keys)
	}
	fp.mu.Lock()
	finalScr := fp.curScreen
	fp.mu.Unlock()
	if _, open := parseRewindPicker(finalScr); open {
		t.Fatal("rewind picker was left open after cancellation cleanup")
	}
}

// Item 4: Footer detection matches ONLY the exact trimmed footer line
// "Enter to continue · Esc to cancel", never message text containing substrings.
func TestClaudeRewindFooterExactLineMatch(t *testing.T) {
	msgWithHints := "Fix dialog: Enter to continue or Esc to cancel"
	pickerScr := ccPickerScreen("above", []string{"first", msgWithHints, "(current)"}, 1)
	pk, ok := parseRewindPicker(pickerScr)
	if !ok {
		t.Fatal("parseRewindPicker failed when a user message contained footer substrings")
	}
	if pk.selected != msgWithHints {
		t.Fatalf("picker selected = %q, want %q", pk.selected, msgWithHints)
	}

	opts := []string{"Restore code and conversation", "Restore conversation", "Never mind"}
	confirmScr := ccConfirmScreen("above", []string{msgWithHints, "Enter to continue · Esc to cancel"}, opts, 1)
	cf, ok := parseRewindConfirm(confirmScr)
	if !ok {
		t.Fatal("parseRewindConfirm failed when quoted message contained footer text")
	}
	sel, selOK := cf.selectedOption()
	if !selOK || sel.label != "Restore conversation" {
		t.Fatalf("confirm selected = %+v ok=%v, want Restore conversation", sel, selOK)
	}
	if !matchConfirmQuote(cf.quoted, msgWithHints+"\nEnter to continue · Esc to cancel") {
		t.Fatalf("quoted lines = %v did not match full message", cf.quoted)
	}
}

// Item 6: rewindStore evicts expired (>7d) marks first at capacity; only falls back
// to oldest mark when all 256 marks are still within 7 days.
func TestRewindStoreTTLEvictionAndFallback(t *testing.T) {
	now := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	cur := now
	st := &rewindStore{now: func() time.Time { return cur }}

	// Case 1: 2 expired (>7d) + 254 fresh (<7d) = 256. Inserting 1 new entry
	// must evict BOTH expired entries and keep all 254 fresh entries (len=255).
	cur = now.Add(-8 * 24 * time.Hour)
	st.set(rewindKey{paneKey: "exp-0", sid: "s", pid: 1, start: 1}, "u1", nil)
	st.set(rewindKey{paneKey: "exp-1", sid: "s", pid: 2, start: 1}, "u1", nil)
	cur = now.Add(-24 * time.Hour)
	for i := 2; i < rewindStoreMax; i++ {
		st.set(rewindKey{paneKey: "fresh-" + strconv.Itoa(i), sid: "s", pid: int64(i), start: 1}, "u1", nil)
	}
	cur = now
	st.set(rewindKey{paneKey: "new-entry", sid: "s", pid: 999, start: 1}, "u2", nil)

	if st.get(rewindKey{paneKey: "exp-0", sid: "s", pid: 1, start: 1}) != nil {
		t.Fatal("expired entry exp-0 was not evicted")
	}
	if st.get(rewindKey{paneKey: "exp-1", sid: "s", pid: 2, start: 1}) != nil {
		t.Fatal("expired entry exp-1 was not evicted")
	}
	if len(st.m) != rewindStoreMax-1 {
		t.Fatalf("store len = %d, want %d (both expired evicted, all fresh kept)", len(st.m), rewindStoreMax-1)
	}
	if st.get(rewindKey{paneKey: "fresh-2", sid: "s", pid: 2, start: 1}) == nil {
		t.Fatal("fresh entry fresh-2 was wrongly evicted")
	}

	// Case 2: All 256 entries are within 7 days -> fallback evicts oldest entry.
	st2 := &rewindStore{now: func() time.Time { return cur }}
	for i := 0; i < rewindStoreMax; i++ {
		cur = now.Add(-6*24*time.Hour + time.Duration(i)*time.Minute)
		st2.set(rewindKey{paneKey: "k-" + strconv.Itoa(i), sid: "s", pid: int64(i), start: 1}, "u1", nil)
	}
	cur = now
	st2.set(rewindKey{paneKey: "overflow", sid: "s", pid: 999, start: 1}, "u2", nil)
	if len(st2.m) != rewindStoreMax {
		t.Fatalf("fallback store len = %d, want %d", len(st2.m), rewindStoreMax)
	}
	if st2.get(rewindKey{paneKey: "k-0", sid: "s", pid: 0, start: 1}) != nil {
		t.Fatal("oldest entry k-0 was not evicted on fallback")
	}
	if st2.get(rewindKey{paneKey: "k-1", sid: "s", pid: 1, start: 1}) == nil {
		t.Fatal("second-oldest entry k-1 was wrongly evicted")
	}
}

// Item 7: Long messages (>64 KiB, <=100 KiB) restore full text from the session log;
// >100 KiB or unreadable full text is refused instead of refilling truncated text.
func TestClaudeRewindFullTextAndTooLong(t *testing.T) {
	const id = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	// 80 KiB message: exceeds 64 KiB display parser clip, fits within 100 KiB maxSendText.
	text80K := "long prompt first line\n" + strings.Repeat("0123456789abcdef", 5<<10) + "\nEND_MARKER"
	row80K := `{"type":"user","uuid":"` + id + `","parentUuid":null,"message":{"role":"user","content":` + strconv.Quote(text80K) + `}}` + "\n"

	cl := convo.NewClaude()
	cl.Feed([]byte(row80K))
	entries := cl.Since(0)
	if len(entries) != 1 || !entries[0].Parts[0].Truncated {
		t.Fatalf("expected display entry truncated at 64 KiB, got %+v", entries)
	}

	// Without fullText, buildClaudeRewindPlan MUST refuse rather than returning truncated text.
	if _, err := buildClaudeRewindPlan(entries, id, ""); err == nil || err.code != "message_text_unavailable" {
		t.Fatalf("expected message_text_unavailable for truncated entry without fullText, got %v", err)
	}

	// Reading row via claudeRewindRowCommand + claudeRewindTextFromRows yields complete 80 KiB text.
	logFile := filepath.Join(t.TempDir(), "session.jsonl")
	if err := os.WriteFile(logFile, []byte(row80K), 0o644); err != nil {
		t.Fatal(err)
	}
	grepOut, gerr := exec.Command("sh", "-c", claudeRewindRowCommand(logFile, id)).Output()
	if gerr != nil {
		t.Fatalf("claudeRewindRowCommand failed: %v", gerr)
	}
	full, rerr := claudeRewindTextFromRows(grepOut, id)
	if rerr != nil {
		t.Fatalf("claudeRewindTextFromRows failed: %v", rerr)
	}
	if full != text80K {
		t.Fatalf("full text len=%d want %d", len(full), len(text80K))
	}
	plan, perr := buildClaudeRewindPlan(entries, id, full)
	if perr != nil {
		t.Fatalf("buildClaudeRewindPlan failed: %v", perr)
	}
	if plan.TargetText != text80K {
		t.Fatalf("plan.TargetText len=%d want %d (untruncated)", len(plan.TargetText), len(text80K))
	}

	// 110 KiB message (> 100 KiB maxSendText) MUST be rejected with message_too_long.
	text110K := "huge prompt\n" + strings.Repeat("x", maxSendText+4096)
	row110K := `{"type":"user","uuid":"` + id + `","parentUuid":null,"message":{"role":"user","content":` + strconv.Quote(text110K) + `}}` + "\n"
	if _, err := claudeRewindTextFromRows([]byte(row110K), id); err == nil || err.code != "message_too_long" {
		t.Fatalf("expected message_too_long for >100 KiB message, got %v", err)
	}

	// Unmatched/missing row MUST be rejected with message_text_unavailable.
	if _, err := claudeRewindTextFromRows(nil, id); err == nil || err.code != "message_text_unavailable" {
		t.Fatalf("expected message_text_unavailable for missing row, got %v", err)
	}
}
