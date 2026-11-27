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
	inFlight      *inFlightCounter
	onPress          func(ctx context.Context, p *fakeClaudeRewindPane, keys []string) error
	onPickerEnter    func(ctx context.Context, p *fakeClaudeRewindPane, targetText string) error
	onCommitEnter    func(ctx context.Context, p *fakeClaudeRewindPane, targetText string) (claudeRewindCommit, error)
	onCommitEnterOpt func(ctx context.Context, p *fakeClaudeRewindPane, targetText string, restoreCode bool, expectedFiles []string) (claudeRewindCommit, error)
	onScreen         func(ctx context.Context, p *fakeClaudeRewindPane) (string, error)
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

func (f *fakeClaudeRewindPane) pickerEnter(ctx context.Context, targetText string) error {
	f.mu.Lock()
	fn := f.onPickerEnter
	f.mu.Unlock()
	if fn != nil {
		return fn(ctx, f, targetText)
	}
	return f.press(ctx, "enter")
}

func (f *fakeClaudeRewindPane) commitEnter(ctx context.Context, targetText string, restoreCode bool, expectedFiles []string) (claudeRewindCommit, error) {
	f.mu.Lock()
	fnOpt := f.onCommitEnterOpt
	fn := f.onCommitEnter
	f.mu.Unlock()
	if fnOpt != nil {
		return fnOpt(ctx, f, targetText, restoreCode, expectedFiles)
	}
	if fn != nil {
		return fn(ctx, f, targetText)
	}
	return claudeRewindCommit{started: true}, f.press(ctx, "enter")
}

func (f *fakeClaudeRewindPane) waitInFlight(ctx context.Context) error {
	return f.inFlight.Wait(ctx)
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
	srv.rewinds.set(key, "u2", known, t0.Add(10*time.Minute))

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
			if ok, _ := parseClaudeRewindCommitOut([]byte(out)); code != 0 || !ok || keys == "" {
				t.Fatalf("exact regex metachar target failed: code=%d out=%q keys=%q", code, out, keys)
			}

			// 5) Valid "Restore conversation" + matching first line -> exits 0, sends Enter, reports remote timestamp.
			out, code, keys = run(ccConfirmScreen("above", []string{"fix login", "second line"}, opts, 1), "fix login\nsecond line")
			if ok, remoteAt := parseClaudeRewindCommitOut([]byte(out)); code != 0 || !ok || remoteAt.IsZero() || keys == "" {
				t.Fatalf("valid commit failed: code=%d out=%q remoteAt=%v keys=%q", code, out, remoteAt, keys)
			}
			// 6) Wrapped / ellipsis-truncated long first line -> exits 0, sends Enter.
			longFirst := "refactor authentication middleware to validate signed cookies properly"
			out, code, keys = run(ccConfirmScreen("above", []string{"refactor authentication middleware…"}, opts, 1), longFirst)
			if ok, _ := parseClaudeRewindCommitOut([]byte(out)); code != 0 || !ok || keys == "" {
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
	fp.onCommitEnter = func(_ context.Context, _ *fakeClaudeRewindPane, _ string) (claudeRewindCommit, error) {
		commitCalls++
		// Simulate remote atomic check exiting 35 without sending Enter.
		return evalCommitExec(sshx.ExecResult{Ended: true, Started: true}, &sshxExitStub{35})
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
	var inflight inFlightCounter
	aboveAll := "❯ first prompt\n● First answer\n\n❯ fix login\n● Second answer"
	items := []string{"first prompt", "fix login", "(current)"}
	fp := &fakeClaudeRewindPane{
		curScreen: ccNormalScreen(aboveAll, ""),
		inFlight:  &inflight,
	}
	fp.onPress = func(pctx context.Context, p *fakeClaudeRewindPane, keys []string) error {
		k := strings.Join(keys, "+")
		if k == "esc+esc" {
			// Simulate sshx.Exec returning immediately on cancel while the remote
			// command finishes 15ms later and opens the rewind picker.
			inflight.Add()
			go func() {
				defer inflight.Done()
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
	st.set(rewindKey{paneKey: "exp-0", sid: "s", pid: 1, start: 1}, "u1", nil, cur)
	st.set(rewindKey{paneKey: "exp-1", sid: "s", pid: 2, start: 1}, "u1", nil, cur)
	cur = now.Add(-24 * time.Hour)
	for i := 2; i < rewindStoreMax; i++ {
		st.set(rewindKey{paneKey: "fresh-" + strconv.Itoa(i), sid: "s", pid: int64(i), start: 1}, "u1", nil, cur)
	}
	cur = now
	st.set(rewindKey{paneKey: "new-entry", sid: "s", pid: 999, start: 1}, "u2", nil, cur)

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
		st2.set(rewindKey{paneKey: "k-" + strconv.Itoa(i), sid: "s", pid: int64(i), start: 1}, "u1", nil, cur)
	}
	cur = now
	st2.set(rewindKey{paneKey: "overflow", sid: "s", pid: 999, start: 1}, "u2", nil, cur)
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

// ---------------------------------------------------------------------------
// Round 3 regression tests (Items 1-7)
// ---------------------------------------------------------------------------

// setupFakeCommitEnv creates a fake host + tmux/herdr stubs for running
// claudeRewindCommitSh end-to-end against real sh + awk.
func setupFakeCommitEnv(t *testing.T, mode string) (run func(screen, target string, doubleQuote bool) (string, int, string), fullCmdLen func(target string) int) {
	t.Helper()
	const (
		sid   = "11111111-2222-3333-4444-555555555555"
		pid   = int64(4242)
		start = int64(987654)
		sock  = "/tmp/test-herdr.sock"
	)
	pane := "%7"
	if mode == "herdr" {
		pane = "w1:p1"
	}
	binDir, homeDir, fakeProc, _ := setupFakeRewindHost(t, pid, start, sid, pane, sock)
	scrFile := filepath.Join(t.TempDir(), "screen.txt")
	keyLog := filepath.Join(t.TempDir(), "keys.log")

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

	fullCmdLen = func(target string) int {
		sh, err := claudeRewindCommitSh(mode, pane, sock, "9001", pid, start, sid, target)
		if err != nil {
			t.Fatal(err)
		}
		return len("sh -c " + sshx.QuoteShell(sh))
	}

	run = func(screen, target string, doubleQuote bool) (string, int, string) {
		_ = os.Remove(keyLog)
		if err := os.WriteFile(scrFile, []byte(screen), 0o644); err != nil {
			t.Fatal(err)
		}
		sh, err := claudeRewindCommitSh(mode, pane, sock, "9001", pid, start, sid, target)
		if err != nil {
			t.Fatal(err)
		}
		patched := strings.Replace(sh, `PATH="`, `PATH="`+binDir+`:`, 1)
		patched = strings.ReplaceAll(patched, `"/proc/`, `"`+fakeProc+`/`)
		cmdStr := patched
		if doubleQuote {
			cmdStr = "sh -c " + sshx.QuoteShell(patched)
		}
		cmd := exec.Command("sh", "-c", cmdStr)
		cmd.Env = append(os.Environ(), "HOME="+homeDir)
		out, err := cmd.CombinedOutput()
		code := 0
		if err != nil {
			var ee *exec.ExitError
			if errors.As(err, &ee) {
				code = ee.ExitCode()
			} else {
				t.Fatalf("unexpected exec err: %v (out=%q)", err, string(out))
			}
		}
		logged, _ := os.ReadFile(keyLog)
		return string(out), code, string(logged)
	}
	return run, fullCmdLen
}

// Round 3 Item 1: Failure before remote commit script starts (e.g. herdr
// snapshot cancel/error, ErrBusy, exec refused, or remote exit 31-35/126-127)
// MUST be treated as Phase-1 failure and run cleanup, NOT rewind_uncertain.
func TestClaudeRewindCommitNotStartedCleansUp(t *testing.T) {
	fastRewindTimings(t)

	t.Run("herdr snapshot canceled before remote script starts closes confirm panel", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		fp, plan := newStandardRewindFake(nil)
		fp.onCommitEnter = func(_ context.Context, _ *fakeClaudeRewindPane, _ string) (claudeRewindCommit, error) {
			cancel()
			return claudeRewindCommit{started: false}, context.Canceled
		}

		_, err := claudeRewind(ctx, fp, plan)
		if err == nil || err.code == "rewind_uncertain" {
			t.Fatalf("got err=%v, must NOT be rewind_uncertain when remote script never started", err)
		}
		keys := fp.recordedKeys()
		if len(keys) == 0 || keys[len(keys)-1] != "esc" {
			t.Fatalf("expected esc cleanup to close confirm panel, keys=%v", keys)
		}
		fp.mu.Lock()
		finalScr := fp.curScreen
		fp.mu.Unlock()
		if _, open := parseRewindConfirm(finalScr); open {
			t.Fatal("confirm panel left open in terminal")
		}
	})

	t.Run("herdr snapshot error before remote script starts closes confirm panel", func(t *testing.T) {
		fp, plan := newStandardRewindFake(nil)
		fp.onCommitEnter = func(_ context.Context, _ *fakeClaudeRewindPane, _ string) (claudeRewindCommit, error) {
			return claudeRewindCommit{started: false}, errors.New("herdr dial failed")
		}

		_, err := claudeRewind(context.Background(), fp, plan)
		if err == nil || err.code != "host_error" {
			t.Fatalf("got err=%v, want host_error (not rewind_uncertain)", err)
		}
		keys := fp.recordedKeys()
		if len(keys) == 0 || keys[len(keys)-1] != "esc" {
			t.Fatalf("expected esc cleanup, keys=%v", keys)
		}
	})

	t.Run("script failed to start (E2BIG / exit 126 / ErrBusy) treated as not started", func(t *testing.T) {
		for _, tc := range []struct {
			name string
			res  sshx.ExecResult
			err  error
			code string
		}{
			{"ErrBusy before dispatch", sshx.ExecResult{Started: false}, sshx.ErrBusy, "ssh_channels_busy"},
			{"refused exec before start", sshx.ExecResult{Started: false}, errors.New("refused"), "host_error"},
			{"sh exit 126 (E2BIG)", sshx.ExecResult{Ended: true, Started: true}, &sshxExitStub{126}, "host_error"},
			{"sh exit 127 (not found)", sshx.ExecResult{Ended: true, Started: true}, &sshxExitStub{127}, "host_error"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				fp, plan := newStandardRewindFake(nil)
				fp.onCommitEnter = func(_ context.Context, _ *fakeClaudeRewindPane, _ string) (claudeRewindCommit, error) {
					return evalCommitExec(tc.res, tc.err)
				}
				_, err := claudeRewind(context.Background(), fp, plan)
				if err == nil || err.code != tc.code {
					t.Fatalf("got err=%v, want %s", err, tc.code)
				}
				keys := fp.recordedKeys()
				if len(keys) == 0 || keys[len(keys)-1] != "esc" {
					t.Fatalf("expected esc cleanup when script did not start, keys=%v", keys)
				}
			})
		}
	})

	t.Run("remote identity exit 31-34 is Phase-1 without Esc; started+lost is rewind_uncertain", func(t *testing.T) {
		for _, code := range []int{31, 32, 33, 34} {
			c, err := evalCommitExec(sshx.ExecResult{Ended: true, Started: true}, &sshxExitStub{code})
			if c.started || !isIdentityErr(err) {
				t.Fatalf("exit %d: got started=%v err=%v, want started=false identity err", code, c.started, err)
			}
		}
		// Started + missing "ok" output -> committed / rewind_uncertain, NEVER Esc.
		fp, plan := newStandardRewindFake(nil)
		fp.onCommitEnter = func(_ context.Context, _ *fakeClaudeRewindPane, _ string) (claudeRewindCommit, error) {
			return evalCommitExec(sshx.ExecResult{Ended: true, Started: true, Out: []byte("")}, nil)
		}
		_, err := claudeRewind(context.Background(), fp, plan)
		if err == nil || err.code != "rewind_uncertain" {
			t.Fatalf("missing ok output: got err=%v, want rewind_uncertain", err)
		}
		if slices.Contains(fp.recordedKeys(), "esc") {
			t.Fatalf("must NOT press Esc after script started with uncertain outcome, keys=%v", fp.recordedKeys())
		}
	})
}

// Round 3 Item 2: Remote commit command length is strictly bounded even with
// tens of thousands of single quotes, and shell metacharacters are never
// executed as code.
func TestClaudeRewindCommitScriptLengthLimitAndInjectionSafety(t *testing.T) {
	opts := []string{"Restore code and conversation", "Restore conversation", "Never mind"}
	run, fullCmdLen := setupFakeCommitEnv(t, "tmux")

	// 20,000 single quotes on the first line (double-quoting without a cap
	// would inflate to ~200 KiB and hit Linux MAX_ARG_STRLEN / E2BIG).
	hugeQuotes := strings.Repeat("'", 20000) + "\nsecond line ignored"
	cmdLen := fullCmdLen(hugeQuotes)
	if cmdLen >= 64<<10 {
		t.Fatalf("double-quoted commit command too long: %d bytes (want < 64 KiB)", cmdLen)
	}
	// Confirm panel showing ellipsis-truncated prefix of the huge first line matches.
	out, code, keys := run(ccConfirmScreen("above", []string{strings.Repeat("'", 60) + "…"}, opts, 1), hugeQuotes, true)
	if ok, _ := parseClaudeRewindCommitOut([]byte(out)); code != 0 || !ok || keys == "" {
		t.Fatalf("huge quotes ellipsis match failed: code=%d out=%q keys=%q", code, out, keys)
	}
	// Confirm panel showing wrapped >= 2000 quotes also matches prefix when truncated.
	wrappedQuotes := make([]string, 0, 25)
	for range 25 {
		wrappedQuotes = append(wrappedQuotes, strings.Repeat("'", 100))
	}
	out, code, keys = run(ccConfirmScreen("above", wrappedQuotes, opts, 1), hugeQuotes, true)
	if ok, _ := parseClaudeRewindCommitOut([]byte(out)); code != 0 || !ok || keys == "" {
		t.Fatalf("huge quotes wrapped prefix match failed: code=%d out=%q keys=%q", code, out, keys)
	}
	// Mismatched confirm screen must still exit 35 without sending Enter.
	_, code, keys = run(ccConfirmScreen("above", []string{"different message"}, opts, 1), hugeQuotes, true)
	if code != 35 || keys != "" {
		t.Fatalf("huge quotes mismatch: code=%d keys=%q, want 35 and empty", code, keys)
	}

	// Shell metacharacters (single quotes, backslashes, $(), backticks, $HOME, newlines)
	// MUST NEVER be executed as shell commands.
	pwnDir := t.TempDir()
	pwn1 := filepath.Join(pwnDir, "pwn1")
	pwn2 := filepath.Join(pwnDir, "pwn2")
	pwn3 := filepath.Join(pwnDir, "pwn3")
	evilFirst := `fix ''' $(touch ` + pwn1 + `) ` + "`touch " + pwn2 + "`" + ` $HOME \n \`
	evilTarget := evilFirst + "\n$(touch " + pwn3 + ")"
	out, code, keys = run(ccConfirmScreen("above", []string{evilFirst}, opts, 1), evilTarget, true)
	if ok, _ := parseClaudeRewindCommitOut([]byte(out)); code != 0 || !ok || keys == "" {
		t.Fatalf("metachar target match failed: code=%d out=%q keys=%q", code, out, keys)
	}
	for _, p := range []string{pwn1, pwn2, pwn3} {
		if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("shell injection executed: %s exists!", p)
		}
	}
}

// Round 3 Item 3: When an in-flight remote command outlives waitInFlight's
// timeout during cancellation, releaseClaudeRewind waits for it to finish and
// closes the late-opened panel BEFORE releasing the pane lock.
func TestClaudeRewindDelayedInFlightCleanupBeforeUnlock(t *testing.T) {
	fastRewindTimings(t)
	origCleanup := claudeRewindCleanupWait
	claudeRewindCleanupWait = 15 * time.Millisecond
	t.Cleanup(func() { claudeRewindCleanupWait = origCleanup })

	ctx, cancel := context.WithCancel(context.Background())
	var running inFlightCounter
	aboveAll := "❯ first prompt\n● First answer\n\n❯ fix login\n● Second answer"
	items := []string{"first prompt", "fix login", "(current)"}

	cmdDone := make(chan struct{})
	fp := &fakeClaudeRewindPane{
		curScreen: ccNormalScreen(aboveAll, ""),
		inFlight:  &running,
	}
	fp.onPress = func(_ context.Context, p *fakeClaudeRewindPane, keys []string) error {
		k := strings.Join(keys, "+")
		if k == "esc+esc" {
			// Remote command finishes 60ms later (> 15ms claudeRewindCleanupWait)
			// and opens the rewind picker long after inline waitInFlight timed out.
			running.Add()
			go func() {
				defer running.Done()
				time.Sleep(60 * time.Millisecond)
				p.mu.Lock()
				p.curScreen = ccPickerScreen(aboveAll, items, len(items)-1)
				p.mu.Unlock()
				close(cmdDone)
			}()
			cancel()
			return context.Canceled
		}
		if k == "esc" {
			p.curScreen = ccNormalScreen(aboveAll, "")
		}
		return nil
	}

	res, err := claudeRewind(ctx, fp, claudeRewindPlan{Steps: 1, TargetText: "fix login"})
	if err == nil {
		t.Fatal("expected cancellation error")
	}
	if !res.needsCleanup {
		t.Fatal("expected res.needsCleanup=true when inline waitInFlight timed out")
	}
	// Inline cleanup timed out before the 60ms remote command finished.
	select {
	case <-cmdDone:
		t.Fatal("expected claudeRewind to return after waitInFlight timeout before delayed command finished")
	default:
	}

	unlocked := make(chan struct{})
	go releaseClaudeRewind(&running, fp, res.needsCleanup, func() {
		close(unlocked)
	})

	// Lock MUST remain held while the delayed command is still running.
	select {
	case <-unlocked:
		t.Fatal("pane lock released before delayed remote command finished and cleanup ran")
	case <-cmdDone:
	}

	// After the delayed command finishes and opens the picker, releaseClaudeRewind
	// must close the picker with Esc and then unlock.
	select {
	case <-unlocked:
	case <-time.After(2 * time.Second):
		t.Fatal("pane lock was not released after delayed cleanup")
	}
	keys := fp.recordedKeys()
	if len(keys) < 2 || keys[len(keys)-1] != "esc" {
		t.Fatalf("expected deferred cleanup to press esc after delayed command opened picker, keys=%v", keys)
	}
	fp.mu.Lock()
	finalScr := fp.curScreen
	fp.mu.Unlock()
	if _, open := parseRewindPicker(finalScr); open {
		t.Fatal("rewind picker was still open after pane lock was released")
	}

	// Verify committed / rewind_uncertain NEVER triggers deferred Esc cleanup.
	fp2, plan2 := newStandardRewindFake(nil)
	fp2.onCommitEnter = func(_ context.Context, _ *fakeClaudeRewindPane, _ string) (claudeRewindCommit, error) {
		return claudeRewindCommit{started: true}, errors.New("ssh timeout")
	}
	res2, err2 := claudeRewind(context.Background(), fp2, plan2)
	if err2 == nil || err2.code != "rewind_uncertain" {
		t.Fatalf("got err=%v, want rewind_uncertain", err2)
	}
	if res2.needsCleanup {
		t.Fatal("committed/rewind_uncertain must have needsCleanup=false")
	}
	var running2 inFlightCounter
	releaseClaudeRewind(&running2, fp2, res2.needsCleanup, func() {})
	if slices.Contains(fp2.recordedKeys(), "esc") {
		t.Fatalf("deferred release must NEVER press Esc after commit, keys=%v", fp2.recordedKeys())
	}
}

// Round 3 Item 4: Go and remote awk handle Unicode whitespace (NBSP U+00A0,
// fullwidth space U+3000, narrow NBSP U+202F, tabs) identically.
func TestClaudeRewindUnicodeWhitespaceParity(t *testing.T) {
	opts := []string{"Restore code and conversation", "Restore conversation", "Never mind"}
	run, _ := setupFakeCommitEnv(t, "tmux")

	target := "fix\u00a0login\u3000with\u202fspaces\tand  tabs"
	screenQuote := "fix\u00a0login\u3000with\u202fspaces   and\ttabs"

	if !matchPickerSelection(screenQuote, target) {
		t.Fatal("Go matchPickerSelection failed with Unicode whitespace")
	}
	if !matchConfirmQuote([]string{screenQuote}, target) {
		t.Fatal("Go matchConfirmQuote failed with Unicode whitespace")
	}
	// Remote awk must accept NBSP / U+3000 / U+202F on screen against Go-normalized TARGET_FIRST.
	out, code, keys := run(ccConfirmScreen("above", []string{screenQuote}, opts, 1), target, true)
	if ok, _ := parseClaudeRewindCommitOut([]byte(out)); code != 0 || !ok || keys == "" {
		t.Fatalf("remote awk rejected Unicode whitespace match: code=%d out=%q keys=%q", code, out, keys)
	}
	// Also test plain spaces on screen when original target had NBSP/U+3000.
	out, code, keys = run(ccConfirmScreen("above", []string{"fix login with spaces and tabs"}, opts, 1), target, true)
	if ok, _ := parseClaudeRewindCommitOut([]byte(out)); code != 0 || !ok || keys == "" {
		t.Fatalf("remote awk rejected ASCII-normalized screen vs Unicode target: code=%d out=%q keys=%q", code, out, keys)
	}
	// Different text must still be rejected.
	_, code, keys = run(ccConfirmScreen("above", []string{"fix\u00a0logout\u3000now"}, opts, 1), target, true)
	if code != 35 || keys != "" {
		t.Fatalf("mismatched Unicode text should exit 35, got code=%d keys=%q", code, keys)
	}
}

// Round 3 Item 5: Mixed line wraps (some wrap breaks mid-word without a space,
// some at spaces) match on both Go and remote awk, while keeping the strict
// "Restore conversation" selection requirement unchanged.
func TestClaudeRewindMixedWrapConfirmMatch(t *testing.T) {
	opts := []string{"Restore code and conversation", "Restore conversation", "Never mind"}
	run, _ := setupFakeCommitEnv(t, "tmux")

	// Single-line prompt where wrap 1 splits "superlongword" (no space) and wrap 2 splits before "gamma" (space).
	target := "alpha beta superlongwordthathastowrap gamma delta"
	mixedWrapped := []string{
		"alpha beta superlong",
		"wordthathastowrap",
		"gamma delta",
	}
	if !matchConfirmQuote(mixedWrapped, target) {
		t.Fatal("Go matchConfirmQuote failed on mixed-wrap single line")
	}
	out, code, keys := run(ccConfirmScreen("above", mixedWrapped, opts, 1), target, true)
	if ok, _ := parseClaudeRewindCommitOut([]byte(out)); code != 0 || !ok || keys == "" {
		t.Fatalf("remote awk failed on mixed-wrap single line: code=%d out=%q keys=%q", code, out, keys)
	}

	// Multi-line prompt whose first line has a mixed wrap across 2 lines, followed by line 2.
	multiTarget := "alpha beta superlongwordthathastowrap gamma\nsecond line here"
	multiWrapped := []string{
		"alpha beta superlong",
		"wordthathastowrap gamma",
		"second line here",
	}
	if !matchConfirmQuote(multiWrapped, multiTarget) {
		t.Fatal("Go matchConfirmQuote failed on mixed-wrap multiline")
	}
	out, code, keys = run(ccConfirmScreen("above", multiWrapped, opts, 1), multiTarget, true)
	if ok, _ := parseClaudeRewindCommitOut([]byte(out)); code != 0 || !ok || keys == "" {
		t.Fatalf("remote awk failed on mixed-wrap multiline: code=%d out=%q keys=%q", code, out, keys)
	}

	// Mixed-wrap + ellipsis truncation on Go and remote awk.
	mixedEllipsis := []string{"alpha beta superlong", "wordthathasto…"}
	if !matchConfirmQuote(mixedEllipsis, target) {
		t.Fatal("Go matchConfirmQuote failed on mixed-wrap ellipsis")
	}
	out, code, keys = run(ccConfirmScreen("above", mixedEllipsis, opts, 1), target, true)
	if ok, _ := parseClaudeRewindCommitOut([]byte(out)); code != 0 || !ok || keys == "" {
		t.Fatalf("remote awk failed on mixed-wrap ellipsis: code=%d out=%q keys=%q", code, out, keys)
	}

	// Option requirement MUST remain strict: "Restore code and conversation" selected -> exit 35, no Enter!
	_, code, keys = run(ccConfirmScreen("above", mixedWrapped, opts, 0), target, true)
	if code != 35 || keys != "" {
		t.Fatalf("wrong option selected with mixed wrap must exit 35, got code=%d keys=%q", code, keys)
	}

	// Different message sharing a prefix ("fix login bug" vs "fix login") MUST NOT match.
	_, code, keys = run(ccConfirmScreen("above", []string{"fix login bug"}, opts, 1), "fix login", true)
	if code != 35 || keys != "" {
		t.Fatalf("prefix spoof must exit 35, got code=%d keys=%q", code, keys)
	}
}

// Round 3 Item 6: Rewind settling uses the remote host's clock reported by the
// commit script (or gateway - 10m fallback when unavailable), handling both
// remote-slower (-5m) and remote-faster (+5m) clock skew.
func TestClaudeRewindRemoteClockSkew(t *testing.T) {
	const (
		sid        = "11111111-1111-1111-1111-111111111111"
		pane       = "w1:p1"
		pid, start = int64(500), int64(9000)
	)
	gwNow := time.Date(2026, 11, 26, 10, 10, 0, 0, time.UTC)

	makeServer := func() (*Server, *term.Session, rewindKey) {
		srv := &Server{herdrSocks: herdrSockets{path: map[string]string{}}}
		srv.rewinds.now = func() time.Time { return gwNow }
		s := &term.Session{ID: "conn-1", OwnerID: 1, HostID: 1, Persistence: "herdr"}
		srv.herdrSocks.set(s.ID, "/tmp/h.sock")
		return srv, s, rewindKey{paneKey: srv.convoReplyKey(s, pane), sid: sid, pid: pid, start: start}
	}
	makeState := func(raw string) *convoState {
		cl := convo.NewClaude()
		cl.Feed([]byte(raw))
		return &convoState{
			gen:    convoGens.Add(1),
			agent:  "claude",
			sid:    sid,
			parser: cl,
			bind:   &convoBind{Pane: pane, PID: pid, Start: start, Session: sid, Agent: "claude"},
		}
	}
	row := func(typ, id, parent, content string, at time.Time) string {
		ts := at.UTC().Format(time.RFC3339Nano)
		if typ == "user" {
			return `{"type":"user","uuid":"` + id + `","parentUuid":` + parent + `,"timestamp":"` + ts + `","message":{"role":"user","content":"` + content + `"}}` + "\n"
		}
		return `{"type":"assistant","uuid":"` + id + `","parentUuid":` + parent + `,"timestamp":"` + ts + `","message":{"content":[{"type":"text","text":"` + content + `"}]}}` + "\n"
	}

	t.Run("remote 5 minutes slower than gateway settles new message", func(t *testing.T) {
		remoteNow := gwNow.Add(-5 * time.Minute) // 10:05:00 vs gateway 10:10:00
		srv, sess, key := makeServer()
		base := row("user", "u1", "null", "first", remoteNow.Add(-4*time.Minute)) +
			row("assistant", "a1", `"u1"`, "ans1", remoteNow.Add(-3*time.Minute)) +
			row("user", "u2", `"a1"`, "second", remoteNow.Add(-2*time.Minute)) +
			row("assistant", "a2", `"u2"`, "ans2", remoteNow.Add(-1*time.Minute))
		st := makeState(base)
		known := st.parser.(*convo.Claude).UserIDs()

		srv.rewinds.set(key, "u2", known, remoteNow)
		if got := srv.syncClaudeRewind(sess, st); got != "u2" {
			t.Fatalf("expected active mark u2 before new message, got %q", got)
		}

		// New message written 10s later on remote host (10:05:10 — still 4m50s behind gateway clock!).
		st.parser.Feed([]byte(row("user", "u3", `"a1"`, "new after rewind", remoteNow.Add(10*time.Second))))
		if got := srv.syncClaudeRewind(sess, st); got != "" {
			t.Fatalf("new message on slower remote clock did not settle rewind: target=%q", got)
		}
		if srv.rewinds.get(key) != nil {
			t.Fatal("settled mark still present in store")
		}
	})

	t.Run("remote 5 minutes slower fallback when remoteAt unavailable", func(t *testing.T) {
		remoteNow := gwNow.Add(-5 * time.Minute)
		srv, sess, key := makeServer()
		base := row("user", "u1", "null", "first", remoteNow.Add(-4*time.Minute)) +
			row("assistant", "a1", `"u1"`, "ans1", remoteNow.Add(-3*time.Minute)) +
			row("user", "u2", `"a1"`, "second", remoteNow.Add(-2*time.Minute))
		st := makeState(base)
		known := st.parser.(*convo.Claude).UserIDs()

		// Zero remoteAt falls back to gatewayNow - 10m (10:00:00).
		srv.rewinds.set(key, "u2", known, time.Time{})
		st.parser.Feed([]byte(row("user", "u3", `"a1"`, "new msg", remoteNow.Add(10*time.Second))))
		if got := srv.syncClaudeRewind(sess, st); got != "" {
			t.Fatalf("fallback clock slack did not settle new message on slower remote: target=%q", got)
		}
	})

	t.Run("remote 5 minutes faster than gateway ignores prepended history and settles new msg", func(t *testing.T) {
		remoteNow := gwNow.Add(5 * time.Minute) // 10:15:00 vs gateway 10:10:00
		srv, sess, key := makeServer()
		tail := row("user", "u2", `"a1"`, "second", remoteNow.Add(-2*time.Minute)) +
			row("assistant", "a2", `"u2"`, "ans2", remoteNow.Add(-1*time.Minute))
		st := makeState(tail)
		known := st.parser.(*convo.Claude).UserIDs()
		srv.rewinds.set(key, "u2", known, remoteNow)

		// Another connection prepends earlier history (including an older rewind at 10:12:00,
		// which is 2 minutes AFTER gateway clock 10:10:00, but 3 minutes BEFORE remote clock 10:15:00!).
		earlier := convo.NewClaude()
		earlier.Feed([]byte(
			row("user", "u0", "null", "zero", remoteNow.Add(-6*time.Minute)) +
				row("assistant", "a0", `"u0"`, "ans0", remoteNow.Add(-5*time.Minute)) +
				row("user", "u1old", `"a0"`, "old try", remoteNow.Add(-4*time.Minute)) +
				row("user", "u1", `"a0"`, "early retry", remoteNow.Add(-3*time.Minute)) +
				row("assistant", "a1", `"u1"`, "ans1", remoteNow.Add(-2*time.Minute-30*time.Second)),
		))
		st2 := makeState(tail)
		st2.parser.Prepend(earlier)
		if got := srv.syncClaudeRewind(sess, st2); got != "u2" {
			t.Fatalf("prepended history on faster remote prematurely settled rewind: target=%q", got)
		}

		// New message at 10:15:10 settles cleanly.
		st2.parser.Feed([]byte(row("user", "u3", `"a1"`, "new msg", remoteNow.Add(10*time.Second))))
		if got := srv.syncClaudeRewind(sess, st2); got != "" {
			t.Fatalf("new message on faster remote did not settle rewind: target=%q", got)
		}
	})
}

// Round 3 Item 7: Retiring an old rewind mark M1 must compare generation/identity
// so it NEVER deletes a newer mark M2 written to the same key by another connection.
func TestRewindStoreCompareAndDeleteConcurrent(t *testing.T) {
	const (
		sid        = "11111111-1111-1111-1111-111111111111"
		pane       = "w1:p1"
		pid, start = int64(500), int64(9000)
	)
	t0 := time.Date(2026, 11, 26, 10, 0, 0, 0, time.UTC)

	// 1. Deterministic interleaving: A reads M1 -> B retires M1 and writes M2 -> A tries to delete M1.
	st := &rewindStore{now: func() time.Time { return t0 }}
	key := rewindKey{paneKey: "p1", sid: sid, pid: pid, start: start}
	st.set(key, "u2", map[string]bool{"u1": true, "u2": true}, t0)
	m1 := st.get(key)
	if m1 == nil || m1.gen == 0 {
		t.Fatalf("expected non-zero gen for M1, got %+v", m1)
	}
	if !st.deleteIf(key, m1.gen) {
		t.Fatal("B failed to retire M1 with matching gen")
	}
	st.set(key, "u1", map[string]bool{"u1": true}, t0.Add(time.Minute))
	m2 := st.get(key)
	if m2 == nil || m2.gen == m1.gen {
		t.Fatalf("expected distinct gen for M2, m1=%+v m2=%+v", m1, m2)
	}
	if st.deleteIf(key, m1.gen) {
		t.Fatal("A's stale deleteIf(M1.gen) wrongly deleted M2!")
	}
	if cur := st.get(key); cur == nil || cur.targetID != "u1" || cur.gen != m2.gen {
		t.Fatalf("M2 was lost or corrupted: cur=%+v want=%+v", cur, m2)
	}

	// 2. Concurrent race test with syncClaudeRewind across multiple connections.
	srv := &Server{herdrSocks: herdrSockets{path: map[string]string{}}}
	srv.rewinds.now = func() time.Time { return t0 }
	sessA := &term.Session{ID: "conn-A", OwnerID: 1, HostID: 1, Persistence: "herdr"}
	sessB := &term.Session{ID: "conn-B", OwnerID: 1, HostID: 1, Persistence: "herdr"}
	srv.herdrSocks.set(sessA.ID, "/tmp/h.sock")
	srv.herdrSocks.set(sessB.ID, "/tmp/h.sock")
	rkey := rewindKey{paneKey: srv.convoReplyKey(sessA, pane), sid: sid, pid: pid, start: start}

	ts := func(d time.Duration) string { return t0.Add(d).Format(time.RFC3339Nano) }
	logWithM1Settled := strings.Join([]string{
		`{"type":"user","uuid":"u1","parentUuid":null,"timestamp":"` + ts(-4*time.Minute) + `","message":{"role":"user","content":"first"}}`,
		`{"type":"assistant","uuid":"a1","parentUuid":"u1","timestamp":"` + ts(-3*time.Minute) + `","message":{"content":[{"type":"text","text":"ans1"}]}}`,
		`{"type":"user","uuid":"u2","parentUuid":"a1","timestamp":"` + ts(-2*time.Minute) + `","message":{"role":"user","content":"second"}}`,
		`{"type":"assistant","uuid":"a2","parentUuid":"u2","timestamp":"` + ts(-1*time.Minute) + `","message":{"content":[{"type":"text","text":"ans2"}]}}`,
		`{"type":"user","uuid":"u3","parentUuid":"a1","timestamp":"` + ts(10*time.Second) + `","message":{"role":"user","content":"after m1"}}`,
	}, "\n") + "\n"

	for iter := range 100 {
		knownM1 := map[string]bool{"u1": true, "u2": true}
		knownM2 := map[string]bool{"u1": true, "u2": true, "u3": true}
		srv.rewinds.set(rkey, "u2", knownM1, t0)

		var wg sync.WaitGroup
		startCh := make(chan struct{})
		// 4 readers concurrently trying to retire M1 via syncClaudeRewind.
		for range 4 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				cl := convo.NewClaude()
				cl.Feed([]byte(logWithM1Settled))
				stA := &convoState{
					gen:    convoGens.Add(1),
					agent:  "claude",
					sid:    sid,
					parser: cl,
					bind:   &convoBind{Pane: pane, PID: pid, Start: start, Session: sid, Agent: "claude"},
				}
				<-startCh
				srv.syncClaudeRewind(sessA, stA)
			}()
		}
		// Writer B retires M1 and installs M2 (target "u3", remoteAt t0+30s — unsettled by logWithM1Settled).
		wg.Add(1)
		go func() {
			defer wg.Done()
			cl := convo.NewClaude()
			cl.Feed([]byte(logWithM1Settled))
			stB := &convoState{
				gen:    convoGens.Add(1),
				agent:  "claude",
				sid:    sid,
				parser: cl,
				bind:   &convoBind{Pane: pane, PID: pid, Start: start, Session: sid, Agent: "claude"},
			}
			<-startCh
			srv.syncClaudeRewind(sessB, stB)
			srv.rewinds.set(rkey, "u3", knownM2, t0.Add(30*time.Second))
		}()
		close(startCh)
		wg.Wait()

		// M2 MUST ALWAYS survive — no reader retiring M1 may ever delete M2!
		got := srv.rewinds.get(rkey)
		if got == nil || got.targetID != "u3" {
			t.Fatalf("iter %d: M2 was wrongly deleted by concurrent M1 retirement! got=%+v", iter, got)
		}
	}
}

// ---------------------------------------------------------------------------
// Round 4 regression tests (Items 1-3)
// ---------------------------------------------------------------------------

// setupFakePickerEnv creates a fake host + tmux/herdr stubs for running
// claudeRewindPickerEnterSh end-to-end against real sh + awk.
func setupFakePickerEnv(t *testing.T, mode string) (run func(screen, target string, doubleQuote bool) (string, int, string), fullCmdLen func(target string) int) {
	t.Helper()
	const (
		sid   = "11111111-2222-3333-4444-555555555555"
		pid   = int64(4242)
		start = int64(987654)
		sock  = "/tmp/test-herdr.sock"
	)
	pane := "%7"
	if mode == "herdr" {
		pane = "w1:p1"
	}
	binDir, homeDir, fakeProc, _ := setupFakeRewindHost(t, pid, start, sid, pane, sock)
	scrFile := filepath.Join(t.TempDir(), "screen.txt")
	keyLog := filepath.Join(t.TempDir(), "keys.log")

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

	fullCmdLen = func(target string) int {
		sh, err := claudeRewindPickerEnterSh(mode, pane, sock, "9001", pid, start, sid, target)
		if err != nil {
			t.Fatal(err)
		}
		return len("sh -c " + sshx.QuoteShell(sh))
	}

	run = func(screen, target string, doubleQuote bool) (string, int, string) {
		_ = os.Remove(keyLog)
		if err := os.WriteFile(scrFile, []byte(screen), 0o644); err != nil {
			t.Fatal(err)
		}
		sh, err := claudeRewindPickerEnterSh(mode, pane, sock, "9001", pid, start, sid, target)
		if err != nil {
			t.Fatal(err)
		}
		patched := strings.Replace(sh, `PATH="`, `PATH="`+binDir+`:`, 1)
		patched = strings.ReplaceAll(patched, `"/proc/`, `"`+fakeProc+`/`)
		cmdStr := patched
		if doubleQuote {
			cmdStr = "sh -c " + sshx.QuoteShell(patched)
		}
		cmd := exec.Command("sh", "-c", cmdStr)
		cmd.Env = append(os.Environ(), "HOME="+homeDir)
		out, err := cmd.CombinedOutput()
		code := 0
		if err != nil {
			var ee *exec.ExitError
			if errors.As(err, &ee) {
				code = ee.ExitCode()
			} else {
				t.Fatalf("unexpected exec err: %v (out=%q)", err, string(out))
			}
		}
		logged, _ := os.ReadFile(keyLog)
		return string(out), code, string(logged)
	}
	return run, fullCmdLen
}

// Round 4 Item 1: Atomic remote picker-Enter script checks identity + live
// screen below the last ▔ rule, requiring a Rewind picker (NOT a confirm page)
// whose sole ❯ row matches target's first line before sending Enter.
func TestClaudeRewindPickerEnterScriptAtomicCheck(t *testing.T) {
	opts := []string{"Restore code and conversation", "Restore conversation", "Restore code", "Never mind"}

	for _, mode := range []string{"tmux", "herdr"} {
		t.Run(mode, func(t *testing.T) {
			run, fullCmdLen := setupFakePickerEnv(t, mode)

			// 1) User already pressed Enter in terminal -> confirm screen with
			// "Restore code and conversation" selected. MUST exit 36, NEVER send Enter!
			_, code, keys := run(ccConfirmScreen("above", []string{"fix login"}, opts, 0), "fix login\nsecond line", true)
			if code != 36 || keys != "" {
				t.Fatalf("confirm screen with code restore selected: code=%d keys=%q, want 36 and empty", code, keys)
			}

			// 2) Confirm screen even with "Restore conversation" selected -> MUST exit 36, no Enter.
			_, code, keys = run(ccConfirmScreen("above", []string{"fix login"}, opts, 1), "fix login", true)
			if code != 36 || keys != "" {
				t.Fatalf("confirm screen (conversation only): code=%d keys=%q, want 36 and empty", code, keys)
			}

			// 3) Picker open on wrong message or (current) -> MUST exit 36, no Enter.
			_, code, keys = run(ccPickerScreen("above", []string{"first", "fix login", "(current)"}, 0), "fix login", true)
			if code != 36 || keys != "" {
				t.Fatalf("wrong picker selection: code=%d keys=%q, want 36 and empty", code, keys)
			}
			_, code, keys = run(ccPickerScreen("above", []string{"first", "fix login", "(current)"}, 2), "(current)", true)
			if code != 36 || keys != "" {
				t.Fatalf("(current) selected: code=%d keys=%q, want 36 and empty", code, keys)
			}

			// 4) Quoted picker in transcript above normal prompt box -> MUST exit 36, no Enter.
			quotedAbove := ccNormalScreen(ccPickerScreen("chat", []string{"fix login", "(current)"}, 0), "")
			_, code, keys = run(quotedAbove, "fix login", true)
			if code != 36 || keys != "" {
				t.Fatalf("quoted picker above prompt box: code=%d keys=%q, want 36 and empty", code, keys)
			}

			// 5) Valid picker selection -> exits 0, prints ok, sends Enter.
			out, code, keys := run(ccPickerScreen("above", []string{"first", "fix login", "(current)"}, 1), "fix login\nsecond line", true)
			if code != 0 || strings.TrimSpace(out) != "ok" || keys == "" {
				t.Fatalf("valid picker enter failed: code=%d out=%q keys=%q", code, out, keys)
			}

			// 6) Unicode whitespace and ellipsis rules match Go's matchPickerSelection.
			unicodeTarget := "fix\u00a0login\u3000with\u202fspaces\tand  tabs"
			out, code, keys = run(ccPickerScreen("above", []string{"fix login with spaces and tabs", "(current)"}, 0), unicodeTarget, true)
			if code != 0 || strings.TrimSpace(out) != "ok" || keys == "" {
				t.Fatalf("unicode whitespace match failed: code=%d out=%q keys=%q", code, out, keys)
			}
			longFirst := "refactor authentication middleware to validate signed cookies properly"
			out, code, keys = run(ccPickerScreen("above", []string{"refactor authentication middleware…", "(current)"}, 0), longFirst, true)
			if code != 0 || strings.TrimSpace(out) != "ok" || keys == "" {
				t.Fatalf("ellipsis match failed: code=%d out=%q keys=%q", code, out, keys)
			}
			out, code, keys = run(ccPickerScreen("above", []string{"refactor authentication middleware ...", "(current)"}, 0), longFirst, true)
			if code != 0 || strings.TrimSpace(out) != "ok" || keys == "" {
				t.Fatalf("ascii ellipsis match failed: code=%d out=%q keys=%q", code, out, keys)
			}
			// Too-short ellipsis prefix (< 4 chars) must be rejected.
			_, code, keys = run(ccPickerScreen("above", []string{"fix...", "(current)"}, 0), "fix login", true)
			if code != 36 || keys != "" {
				t.Fatalf("short ellipsis prefix should exit 36, got code=%d keys=%q", code, keys)
			}
			// Prefix without ellipsis must be rejected.
			_, code, keys = run(ccPickerScreen("above", []string{"fix login", "(current)"}, 0), "fix login bug", true)
			if code != 36 || keys != "" {
				t.Fatalf("non-ellipsis prefix should exit 36, got code=%d keys=%q", code, keys)
			}

			// 7) Regex metacharacters and shell injection safety + huge quote length cap.
			metaTarget := `fix foo.*bar [a-z]+ (test) $1 \n`
			_, code, keys = run(ccPickerScreen("above", []string{`fix fooXbar a (test) $1 \n`, "(current)"}, 0), metaTarget, true)
			if code != 36 || keys != "" {
				t.Fatalf("regex metachar spoof matched: code=%d keys=%q", code, keys)
			}
			out, code, keys = run(ccPickerScreen("above", []string{metaTarget, "(current)"}, 0), metaTarget, true)
			if code != 0 || strings.TrimSpace(out) != "ok" || keys == "" {
				t.Fatalf("exact regex metachar failed: code=%d out=%q keys=%q", code, out, keys)
			}

			hugeQuotes := strings.Repeat("'", 20000) + "\nsecond"
			if cmdLen := fullCmdLen(hugeQuotes); cmdLen >= 64<<10 {
				t.Fatalf("double-quoted picker enter command too long: %d bytes", cmdLen)
			}
			out, code, keys = run(ccPickerScreen("above", []string{strings.Repeat("'", 60) + "…", "(current)"}, 0), hugeQuotes, true)
			if code != 0 || strings.TrimSpace(out) != "ok" || keys == "" {
				t.Fatalf("huge quotes ellipsis match failed: code=%d out=%q keys=%q", code, out, keys)
			}

			pwnFile := filepath.Join(t.TempDir(), "pwn")
			evil := `fix ''' $(touch ` + pwnFile + `) $HOME`
			out, code, keys = run(ccPickerScreen("above", []string{evil, "(current)"}, 0), evil, true)
			if code != 0 || strings.TrimSpace(out) != "ok" || keys == "" {
				t.Fatalf("shell metachar match failed: code=%d out=%q keys=%q", code, out, keys)
			}
			if _, err := os.Stat(pwnFile); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("shell injection executed: %s exists", pwnFile)
			}

			// 8) Special message first lines in picker (confirm header, "Rewind", footer).
			for _, special := range []string{
				"Restore to the point before you sent this message:",
				"Rewind",
				rewindPanelFooter,
			} {
				scr := ccPickerScreen("above", []string{"first", special, "(current)"}, 1)
				out, code, keys = run(scr, special, true)
				if code != 0 || strings.TrimSpace(out) != "ok" || keys == "" {
					t.Fatalf("special message %q in picker failed: code=%d out=%q keys=%q", special, code, out, keys)
				}
			}
		})
	}
	if re := mapRewindExitErr(&sshxExitStub{36}); asRewindErr(re).code != "target_mismatch" || isIdentityErr(re) {
		t.Fatalf("exit 36 mapped to %v, want non-identity target_mismatch", re)
	}
}

// Round 4 Item 1 (flow): If someone presses Enter in the terminal before
// pickerEnter arrives, putting Claude Code on the confirm page with
// "Restore code and conversation" selected, pickerEnter refuses (exit 36)
// without sending Enter and Phase-1 cleanup closes the confirm page with Esc.
func TestClaudeRewindPickerEnterRefusesConfirmAndCleansUp(t *testing.T) {
	fastRewindTimings(t)
	opts := []string{"Restore code and conversation", "Restore conversation", "Restore code", "Never mind"}
	fp, plan := newStandardRewindFake(opts)
	pickerEnterCalled := 0
	fp.onPickerEnter = func(_ context.Context, p *fakeClaudeRewindPane, _ string) error {
		pickerEnterCalled++
		// Simulate user pressing Enter in terminal right before our Enter arrives:
		// confirm screen is now open with "Restore code and conversation" selected!
		p.mu.Lock()
		p.curScreen = ccConfirmScreen("above", []string{"fix login", "check edge cases too"}, opts, 0)
		p.mu.Unlock()
		return mapRewindExitErr(&sshxExitStub{36})
	}
	// Make sure Esc on confirm closes back to normal.
	origPress := fp.onPress
	fp.onPress = func(ctx context.Context, p *fakeClaudeRewindPane, keys []string) error {
		if strings.Join(keys, "+") == "esc" && isRewindPanelOpen(p.curScreen) {
			p.curScreen = ccNormalScreen("❯ first prompt\n● First answer", "")
			return nil
		}
		return origPress(ctx, p, keys)
	}

	_, err := claudeRewind(context.Background(), fp, plan)
	if err == nil || err.code != "target_mismatch" {
		t.Fatalf("got err=%v, want target_mismatch", err)
	}
	if pickerEnterCalled != 1 {
		t.Fatalf("pickerEnterCalled = %d, want 1", pickerEnterCalled)
	}
	keys := fp.recordedKeys()
	if slices.Contains(keys, "enter") {
		t.Fatalf("must NEVER send Enter when confirm screen already appeared, keys=%v", keys)
	}
	if len(keys) == 0 || keys[len(keys)-1] != "esc" {
		t.Fatalf("expected Phase-1 cleanup to press Esc and close confirm screen, keys=%v", keys)
	}
	fp.mu.Lock()
	finalScr := fp.curScreen
	fp.mu.Unlock()
	if isRewindPanelOpen(finalScr) {
		t.Fatal("confirm panel left open after pickerEnter refusal")
	}
}

// Round 4 Item 2: inFlightCounter supports Wait timeout followed by repeated
// Add/Done cycles under -race without panicking, and wakes Wait promptly when
// count drops to 0.
func TestInFlightCounterTimeoutAndReuseRace(t *testing.T) {
	// 1. Nil counter and idle counter return immediately.
	var nilCounter *inFlightCounter
	if err := nilCounter.Wait(context.Background()); err != nil {
		t.Fatalf("nil Wait err = %v", err)
	}
	var c inFlightCounter
	cancelledCtx, cancelNow := context.WithCancel(context.Background())
	cancelNow()
	if err := c.Wait(cancelledCtx); err != nil {
		t.Fatalf("idle counter Wait should return nil immediately, got %v", err)
	}

	// 2. Repeated rounds: Wait times out while count > 0, then more Add/Done
	// calls happen concurrently while earlier ops are still running (the exact
	// pattern that panicked sync.WaitGroup with "reused before previous Wait has returned").
	for round := range 40 {
		c.Add()
		shortCtx, cancel := context.WithTimeout(context.Background(), 2*time.Millisecond)
		err := c.Wait(shortCtx)
		cancel()
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("round %d: expected DeadlineExceeded, got %v", round, err)
		}

		// Immediately add/done concurrently while the first op is still in-flight!
		var workers sync.WaitGroup
		for range 4 {
			workers.Add(1)
			go func() {
				defer workers.Done()
				for range 10 {
					c.Add()
					time.Sleep(time.Microsecond * 50)
					c.Done()
				}
			}()
		}
		// Another timed-out waiter racing with Add/Done.
		wctx, wcancel := context.WithTimeout(context.Background(), time.Millisecond)
		_ = c.Wait(wctx)
		wcancel()

		workers.Wait()
		c.Done()

		// Now count is 0: Wait must return immediately.
		finalCtx, fcancel := context.WithTimeout(context.Background(), time.Second)
		if err := c.Wait(finalCtx); err != nil {
			fcancel()
			t.Fatalf("round %d: Wait after draining failed: %v", round, err)
		}
		fcancel()
	}

	// 3. Wait wakes promptly when count drops to 0.
	c.Add()
	c.Add()
	waitDone := make(chan error, 1)
	start := time.Now()
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		waitDone <- c.Wait(ctx)
	}()
	time.Sleep(15 * time.Millisecond)
	c.Done()
	select {
	case err := <-waitDone:
		t.Fatalf("Wait returned early while 1 op still in-flight: err=%v", err)
	case <-time.After(10 * time.Millisecond):
	}
	c.Done()
	select {
	case err := <-waitDone:
		if err != nil {
			t.Fatalf("Wait returned error after count reached 0: %v", err)
		}
		if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
			t.Fatalf("Wait took too long to wake after count reached 0: %v", elapsed)
		}
	case <-time.After(time.Second):
		t.Fatal("Wait did not wake after count reached 0")
	}
}

// Round 4 Item 3: Messages whose first line equals the confirm header,
// "Rewind", or the footer hint ("Enter to continue · Esc to cancel") can be
// rewound normally AND always trigger Esc cleanup on failure.
func TestClaudeRewindSpecialMessageFirstLines(t *testing.T) {
	fastRewindTimings(t)
	specials := []struct {
		name string
		text string
	}{
		{"confirm_header", "Restore to the point before you sent this message:"},
		{"rewind_title", "Rewind"},
		{"footer_hint", rewindPanelFooter},
	}

	makeFake := func(msg string) (*fakeClaudeRewindPane, claudeRewindPlan) {
		aboveAll := "❯ first prompt\n● First answer\n\n❯ " + msg + "\n● Second answer\n\n❯ add tests\n● Third answer"
		aboveKept := "❯ first prompt\n● First answer"
		items := []string{"first prompt", msg, "add tests", "(current)"}
		opts := []string{"Restore code and conversation", "Restore conversation", "Never mind"}
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
				case "enter":
					state = "confirm"
					optIdx = 0
					p.curScreen = ccConfirmScreen(aboveAll, []string{items[pickerIdx]}, opts, optIdx)
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
					p.curScreen = ccConfirmScreen(aboveAll, []string{items[pickerIdx]}, opts, optIdx)
				case "enter":
					state = "rewound"
					p.curScreen = ccNormalScreen(aboveKept, msg)
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
			TargetText:        msg,
			WithdrawnSnippets: []string{"Second answer", "add tests", "Third answer"},
		}
		return fp, plan
	}

	for _, tc := range specials {
		t.Run(tc.name+"/normal_rewind_completes", func(t *testing.T) {
			fp, plan := makeFake(tc.text)
			res, err := claudeRewind(context.Background(), fp, plan)
			if err != nil {
				t.Fatalf("normal rewind for %q failed: %v", tc.text, err)
			}
			if !res.OK || res.Warning != "" || res.Text != tc.text {
				t.Fatalf("unexpected result for %q: %+v", tc.text, res)
			}
			wantKeys := []string{"esc+esc", "up", "up", "enter", "down", "enter", "ctrl+c"}
			if got := fp.recordedKeys(); !slices.Equal(got, wantKeys) {
				t.Fatalf("keys = %v, want %v", got, wantKeys)
			}
		})

		t.Run(tc.name+"/failure_cleanup_presses_esc", func(t *testing.T) {
			fp, plan := makeFake(tc.text)
			plan.Steps = 1 // lands on "add tests", mismatch while special message is visible in picker
			_, err := claudeRewind(context.Background(), fp, plan)
			if err == nil || err.code != "target_mismatch" {
				t.Fatalf("got err=%v, want target_mismatch", err)
			}
			keys := fp.recordedKeys()
			if len(keys) == 0 || keys[len(keys)-1] != "esc" {
				t.Fatalf("cleanup must press esc when special message %q is in picker, keys=%v", tc.text, keys)
			}
			fp.mu.Lock()
			finalScr := fp.curScreen
			fp.mu.Unlock()
			if isRewindPanelOpen(finalScr) {
				t.Fatalf("panel remained open after cleanup for %q", tc.text)
			}
		})
	}
}

// Round 4 Item 3 (cleanup resilience): closeRewindPanel detects open panels
// without requiring full picker/confirm parsing, ignores quoted panels above a
// normal prompt box, and returns explicit error code "panel_not_closed" when
// two Esc presses fail to close the panel.
func TestClaudeRewindClosePanelFallbackAndStillOpenError(t *testing.T) {
	fastRewindTimings(t)

	// 1. Minimal/unparseable panel fragments below ▔ still trigger Esc.
	fragments := []struct {
		name   string
		screen string
	}{
		{"title_only", "chat\n" + ccOverline + "\nRewind\nsome unexpected content\n"},
		{"footer_only", "chat\n" + ccOverline + "\nsome unexpected content\n" + rewindPanelFooter + "\n"},
		{"confirm_opt_only", "chat\n" + ccOverline + "\n  1. Restore conversation\n  2. Never mind\n"},
	}
	for _, tc := range fragments {
		t.Run(tc.name, func(t *testing.T) {
			if _, ok := parseRewindPicker(tc.screen); ok {
				t.Fatalf("expected fragment %q not to parse as full picker", tc.name)
			}
			if _, ok := parseRewindConfirm(tc.screen); ok {
				t.Fatalf("expected fragment %q not to parse as full confirm", tc.name)
			}
			if !isRewindPanelOpen(tc.screen) {
				t.Fatalf("expected isRewindPanelOpen=true for %q", tc.name)
			}
			fp := &fakeClaudeRewindPane{curScreen: tc.screen}
			fp.onPress = func(_ context.Context, p *fakeClaudeRewindPane, keys []string) error {
				if strings.Join(keys, "+") == "esc" {
					p.curScreen = ccNormalScreen("chat", "")
				}
				return nil
			}
			if st := closeRewindPanel(context.Background(), fp, false); st != rewindCloseDone {
				t.Fatalf("closeRewindPanel = %v, want rewindCloseDone", st)
			}
			if got := fp.recordedKeys(); !slices.Equal(got, []string{"esc"}) {
				t.Fatalf("keys = %v, want [esc]", got)
			}
		})
	}

	// 2. Quoted panel text above a normal prompt box is NOT treated as open.
	quotedNormal := ccNormalScreen("chat\n"+ccOverline+"\nRewind\n1. Restore conversation\n"+rewindPanelFooter, "")
	if isRewindPanelOpen(quotedNormal) {
		t.Fatal("quoted panel above normal prompt box must NOT be treated as open")
	}

	// 3. When panel stays open after 2 Esc presses, claudeRewind returns explicit
	// error code "panel_not_closed" telling the user to press Esc in the terminal.
	t.Run("stuck_panel_after_two_escs_returns_panel_not_closed", func(t *testing.T) {
		fp, plan := newStandardRewindFake(nil)
		plan.Steps = 1 // triggers target_mismatch -> cleanup
		orig := fp.onPress
		fp.onPress = func(ctx context.Context, p *fakeClaudeRewindPane, keys []string) error {
			if strings.Join(keys, "+") == "esc" {
				// Simulate panel ignoring Esc and remaining open.
				return nil
			}
			return orig(ctx, p, keys)
		}
		res, err := claudeRewind(context.Background(), fp, plan)
		if err == nil || err.code != "panel_not_closed" {
			t.Fatalf("got res=%+v err=%v, want panel_not_closed", res, err)
		}
		if !strings.Contains(err.msg, "Esc") {
			t.Fatalf("error message should tell user to press Esc, got %q", err.msg)
		}
		escCount := 0
		for _, k := range fp.recordedKeys() {
			if k == "esc" {
				escCount++
			}
		}
		if escCount != 2 {
			t.Fatalf("expected exactly 2 Esc attempts before returning panel_not_closed, got %d (keys=%v)", escCount, fp.recordedKeys())
		}
	})
}

// ---------------------------------------------------------------------------
// Restore code + conversation tests (modeled on real Claude Code 2.1.330 screens)
// ---------------------------------------------------------------------------

// Real Claude Code 2.1.330 screen fixtures adapted from captured screens
// (single-file and multi-file code restore confirmation panels, dynamic
// option descriptions, and post-rewind screen).
const (
	realCCAboveThreeTurns = `● Write(a.txt)
  ⎿  Wrote 1 line to a.txt
      1 v1

● Reply two.

✻ Churned for 0s · done 2:28 PM

❯ third message

● Write(a.txt)
  ⎿  Added 2 lines, removed 1 line
      1 -v1
      1 +v2
      2 +more

● Reply three.

✻ Worked for 0s · done 2:28 PM`

	realCCAboveTwoFiles = `✻ Churned for 0s · done 2:30 PM

❯ third message

● Write(a.txt)
  ⎿  Added 2 lines, removed 1 line
      1 -v1
      1 +v2
      2 +more

● Write(sub/b.txt)
  ⎿  Wrote 3 lines to sub/b.txt
      1 b1
      2 b2
      3 b3

● Reply three.

✻ Cooked for 0s · done 2:30 PM`

	realCCScreenPostRewind = ` ▐▛███▛█   Claude Code v2.1.330
▝▜██████▀  Opus 5.5 · API Usage Billing
 ▝▝   ▝▝   ~/rwc1153452

❯ first message

● Reply one.

✻ Crunched for 0s · done 2:28 PM

❯ second message

● Write(a.txt)
  ⎿  Wrote 1 line to a.txt
      1 v1

● Reply two.

✻ Churned for 0s · done 2:28 PM

──────────────────────────────────────────────────────────────────────────────────────────────────────────────
❯ third message
──────────────────────────────────────────────────────────────────────────────────────────────────────────────
  ⏵⏵ auto mode on (shift+tab to cycle)
`

	realCCScreenPostClear = ` ▐▛███▛█   Claude Code v2.1.330
▝▜██████▀  Opus 5.5 · API Usage Billing
 ▝▝   ▝▝   ~/rwc1153452

❯ first message

● Reply one.

✻ Crunched for 0s · done 2:28 PM

❯ second message

● Write(a.txt)
  ⎿  Wrote 1 line to a.txt
      1 v1

● Reply two.

✻ Churned for 0s · done 2:28 PM

──────────────────────────────────────────────────────────────────────────────────────────────────────────────
❯
──────────────────────────────────────────────────────────────────────────────────────────────────────────────
  ⏵⏵ auto mode on (shift+tab to cycle)
`
)

func realCCPickerScreen(above string, thirdSummary string, selected int) string {
	type row struct{ msg, summary string }
	rows := []row{
		{"first message", "No code changes"},
		{"second message", "a.txt +1"},
		{"third message", thirdSummary},
		{"(current)", ""},
	}
	var b strings.Builder
	b.WriteString(above)
	b.WriteString("\n" + ccOverline + "\n")
	b.WriteString("  Rewind\n\n")
	b.WriteString("  Restore the code and/or conversation to the point before…\n\n")
	for i, r := range rows {
		prefix := "    "
		if i == selected {
			prefix = "  ❯ "
		}
		b.WriteString(prefix + r.msg + "\n")
		if r.summary != "" {
			b.WriteString("    " + r.summary + "\n")
		}
		b.WriteString("\n")
	}
	b.WriteString("  Enter to continue · Esc to cancel\n")
	return b.String()
}

func realCCConfirmScreen(above, quoted, codeDesc string, hasCodeOpts bool, selected int) string {
	opts := []string{
		"Restore conversation",
		"Summarize from here",
		"Summarize up to here",
		"Never mind",
	}
	if hasCodeOpts {
		opts = []string{
			"Restore code and conversation",
			"Restore conversation",
			"Restore code",
			"Summarize from here",
			"Summarize up to here",
			"Never mind",
		}
	}
	var convLine, codeLine string
	selLabel := ""
	if selected >= 0 && selected < len(opts) {
		selLabel = opts[selected]
	}
	switch selLabel {
	case "Restore code and conversation":
		convLine = "The conversation goes back to before this message."
		codeLine = codeDesc
	case "Restore conversation":
		convLine = "The conversation goes back to before this message."
		codeLine = "The code will be unchanged."
	case "Restore code":
		convLine = "The conversation will be unchanged."
		codeLine = codeDesc
	case "Summarize from here":
		convLine = "Messages after this point will be summarized."
	case "Summarize up to here":
		convLine = "Earlier messages will be summarized. This message and later ones stay as they are."
	default:
		convLine = "The conversation will be unchanged."
		codeLine = "The code will be unchanged."
	}

	var b strings.Builder
	b.WriteString(above)
	b.WriteString("\n" + ccOverline + "\n")
	b.WriteString("  Rewind\n\n")
	b.WriteString("  Restore to the point before you sent this message:\n\n")
	b.WriteString("  │ " + quoted + "\n")
	b.WriteString("  │ (5s ago)\n\n")
	if convLine != "" {
		b.WriteString("  " + convLine + "\n")
	}
	if codeLine != "" {
		b.WriteString("  " + codeLine + "\n")
	}
	b.WriteString("\n")
	for i, o := range opts {
		prefix := "    "
		if i == selected {
			prefix = "  ❯ "
		}
		b.WriteString(prefix + strconv.Itoa(i+1) + ". " + o + "\n")
	}
	b.WriteString("\n  ⚠ Restored files go back whole, losing later changes by anyone. Bash changes to other files are not\n  undone.\n")
	return b.String()
}

func newRealCCCodeRewindFake(above, thirdSummary, codeDesc string, hasCodeOpts bool) (*fakeClaudeRewindPane, claudeRewindPlan) {
	state := "normal"
	pickerIdx := 3 // (current)
	optIdx := 0
	optCount := 4
	if hasCodeOpts {
		optCount = 6
	}
	normalScreen := ccNormalScreen(above, "")
	fp := &fakeClaudeRewindPane{curScreen: normalScreen}
	fp.onPress = func(_ context.Context, p *fakeClaudeRewindPane, keys []string) error {
		k := strings.Join(keys, "+")
		switch state {
		case "normal":
			if k == "esc+esc" {
				state = "picker"
				pickerIdx = 3
				p.curScreen = realCCPickerScreen(above, thirdSummary, pickerIdx)
			}
		case "picker":
			switch k {
			case "up":
				if pickerIdx > 0 {
					pickerIdx--
				}
				p.curScreen = realCCPickerScreen(above, thirdSummary, pickerIdx)
			case "down":
				if pickerIdx < 3 {
					pickerIdx++
				}
				p.curScreen = realCCPickerScreen(above, thirdSummary, pickerIdx)
			case "enter":
				state = "confirm"
				optIdx = 0
				p.curScreen = realCCConfirmScreen(above, "third message", codeDesc, hasCodeOpts, optIdx)
			case "esc":
				state = "normal"
				p.curScreen = normalScreen
			}
		case "confirm":
			switch k {
			case "down":
				if optIdx < optCount-1 {
					optIdx++
				}
				p.curScreen = realCCConfirmScreen(above, "third message", codeDesc, hasCodeOpts, optIdx)
			case "up":
				if optIdx > 0 {
					optIdx--
				}
				p.curScreen = realCCConfirmScreen(above, "third message", codeDesc, hasCodeOpts, optIdx)
			case "enter":
				state = "rewound"
				p.curScreen = realCCScreenPostRewind
			case "esc":
				state = "normal"
				p.curScreen = normalScreen
			}
		case "rewound":
			if k == "ctrl+c" {
				p.curScreen = realCCScreenPostClear
			}
		}
		return nil
	}
	plan := claudeRewindPlan{
		Steps:             1,
		TargetText:        "third message",
		WithdrawnSnippets: []string{"third message", "Reply three."},
	}
	return fp, plan
}

func TestClaudeRewindRestoreCodeSuccess(t *testing.T) {
	fastRewindTimings(t)
	fp, plan := newRealCCCodeRewindFake(
		realCCAboveThreeTurns,
		"a.txt +2 -1",
		"The code will be restored +1 -2 in a.txt.",
		true,
	)
	plan.RestoreCode = true
	plan.ExpectedFiles = []string{"a.txt"}

	res, err := claudeRewind(context.Background(), fp, plan)
	if err != nil {
		t.Fatalf("expected success, got err: %v", err)
	}
	if !res.OK || !res.RestoredCode || res.Warning != "" || res.Text != "third message" {
		t.Fatalf("unexpected result: %+v", res)
	}
	// Cursor is already on default option 1 ("Restore code and conversation"):
	// must NOT press down/up before commit Enter.
	wantKeys := []string{"esc+esc", "up", "enter", "enter", "ctrl+c"}
	if got := fp.recordedKeys(); !slices.Equal(got, wantKeys) {
		t.Fatalf("keys = %v, want %v", got, wantKeys)
	}
}

func TestClaudeRewindConversationOnlyMovesToSecondOptionOnRealScreen(t *testing.T) {
	fastRewindTimings(t)
	fp, plan := newRealCCCodeRewindFake(
		realCCAboveThreeTurns,
		"a.txt +2 -1",
		"The code will be restored +1 -2 in a.txt.",
		true,
	)
	plan.RestoreCode = false

	res, err := claudeRewind(context.Background(), fp, plan)
	if err != nil {
		t.Fatalf("expected success, got err: %v", err)
	}
	if !res.OK || res.RestoredCode || res.Warning != "" {
		t.Fatalf("unexpected result: %+v", res)
	}
	// Must move down from option 1 to option 2 ("Restore conversation").
	wantKeys := []string{"esc+esc", "up", "enter", "down", "enter", "ctrl+c"}
	if got := fp.recordedKeys(); !slices.Equal(got, wantKeys) {
		t.Fatalf("keys = %v, want %v", got, wantKeys)
	}
}

func TestClaudeRewindRestoreCodeNoCodeChangesOption(t *testing.T) {
	fastRewindTimings(t)
	fp, plan := newRealCCCodeRewindFake(
		realCCAboveThreeTurns,
		"No code changes",
		"The code will be unchanged.",
		false, // 4-option confirm screen without code restore
	)
	plan.RestoreCode = true
	plan.ExpectedFiles = []string{"a.txt"}

	_, err := claudeRewind(context.Background(), fp, plan)
	if err == nil || err.code != "no_code_changes" {
		t.Fatalf("got err=%v, want no_code_changes", err)
	}
	keys := fp.recordedKeys()
	enters := 0
	for _, k := range keys {
		if k == "enter" {
			enters++
		}
	}
	if enters != 1 {
		t.Fatalf("must not press commit enter when no code option exists; enters=%d keys=%v", enters, keys)
	}
	if keys[len(keys)-1] != "esc" {
		t.Fatalf("expected esc cleanup, keys=%v", keys)
	}
}

func TestClaudeRewindRestoreCodeFileMismatch(t *testing.T) {
	fastRewindTimings(t)
	cases := []struct {
		name     string
		codeDesc string
		expected []string
	}{
		{"different_file", "The code will be restored +1 -2 in a.txt.", []string{"b.txt"}},
		{"extra_file_on_screen", "The code will be restored +1 -5 in a.txt and b.txt.", []string{"a.txt"}},
		{"missing_file_on_screen", "The code will be restored +1 -2 in a.txt.", []string{"a.txt", "b.txt"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fp, plan := newRealCCCodeRewindFake(realCCAboveThreeTurns, "a.txt +2 -1", tc.codeDesc, true)
			plan.RestoreCode = true
			plan.ExpectedFiles = tc.expected

			_, err := claudeRewind(context.Background(), fp, plan)
			if err == nil || err.code != "code_mismatch" {
				t.Fatalf("got err=%v, want code_mismatch", err)
			}
			keys := fp.recordedKeys()
			enters := 0
			for _, k := range keys {
				if k == "enter" {
					enters++
				}
			}
			if enters != 1 {
				t.Fatalf("must not press commit enter on file mismatch; enters=%d keys=%v", enters, keys)
			}
			if keys[len(keys)-1] != "esc" {
				t.Fatalf("expected esc cleanup, keys=%v", keys)
			}
		})
	}
}

func TestClaudeRewindRestoreCodeTwoFiles(t *testing.T) {
	fastRewindTimings(t)
	fp, plan := newRealCCCodeRewindFake(
		realCCAboveTwoFiles,
		"2 files changed +5 -1",
		"The code will be restored +1 -5 in a.txt and b.txt.",
		true,
	)
	plan.RestoreCode = true
	// Reverse order and subdir path: must match by basename set {"a.txt", "b.txt"}.
	plan.ExpectedFiles = []string{"sub/b.txt", "a.txt"}

	res, err := claudeRewind(context.Background(), fp, plan)
	if err != nil {
		t.Fatalf("expected success, got err: %v", err)
	}
	if !res.OK || !res.RestoredCode {
		t.Fatalf("unexpected result: %+v", res)
	}
	wantKeys := []string{"esc+esc", "up", "enter", "enter", "ctrl+c"}
	if got := fp.recordedKeys(); !slices.Equal(got, wantKeys) {
		t.Fatalf("keys = %v, want %v", got, wantKeys)
	}
}

func TestClaudeRewindRestoreCodeOptionChangedBeforeCommit(t *testing.T) {
	fastRewindTimings(t)
	for _, tc := range []struct {
		name     string
		movedIdx int // 1 = Restore conversation, 2 = Restore code
	}{
		{"changed_to_conversation_only", 1},
		{"changed_to_restore_code_only", 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fp, plan := newRealCCCodeRewindFake(
				realCCAboveThreeTurns,
				"a.txt +2 -1",
				"The code will be restored +1 -2 in a.txt.",
				true,
			)
			plan.RestoreCode = true
			plan.ExpectedFiles = []string{"a.txt"}

			sawConfirm := false
			confirmReads := 0
			fp.onScreen = func(_ context.Context, p *fakeClaudeRewindPane) (string, error) {
				if _, ok := parseRewindConfirm(p.curScreen); ok {
					sawConfirm = true
				}
				if sawConfirm {
					confirmReads++
					// Second confirm read is preCommitScr: someone moved the cursor!
					if confirmReads == 2 {
						p.curScreen = realCCConfirmScreen(
							realCCAboveThreeTurns,
							"third message",
							"The code will be restored +1 -2 in a.txt.",
							true,
							tc.movedIdx,
						)
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
				t.Fatalf("must not press commit enter when option changed; enters=%d keys=%v", enters, keys)
			}
			if keys[len(keys)-1] != "esc" {
				t.Fatalf("expected esc cleanup, keys=%v", keys)
			}
		})
	}
}

func TestClaudeRewindRestoreCodeUnrecognizedDescLine(t *testing.T) {
	fastRewindTimings(t)
	badDescs := []struct {
		name string
		desc string
	}{
		{"n_files_summary", "The code will be restored +1 -5 in 3 files."},
		{"and_more_summary", "The code will be restored +1 -5 in a.txt, b.txt and 2 other files."},
		{"ellipsis_unicode", "The code will be restored +1 -5 in a.txt, b.txt…"},
		{"ellipsis_ascii", "The code will be restored +1 -5 in a.txt, b.txt..."},
		{"unchanged_line", "The code will be unchanged."},
		{"missing_in_clause", "The code will be restored +1 -2."},
		{"empty_desc", ""},
	}
	for _, tc := range badDescs {
		t.Run(tc.name, func(t *testing.T) {
			fp, plan := newRealCCCodeRewindFake(realCCAboveThreeTurns, "a.txt +2 -1", tc.desc, true)
			plan.RestoreCode = true
			plan.ExpectedFiles = []string{"a.txt"}

			_, err := claudeRewind(context.Background(), fp, plan)
			if err == nil || err.code != "code_mismatch" {
				t.Fatalf("got err=%v, want code_mismatch", err)
			}
			keys := fp.recordedKeys()
			enters := 0
			for _, k := range keys {
				if k == "enter" {
					enters++
				}
			}
			if enters != 1 {
				t.Fatalf("must not press commit enter on unrecognized desc line %q; enters=%d keys=%v", tc.desc, enters, keys)
			}
			if keys[len(keys)-1] != "esc" {
				t.Fatalf("expected esc cleanup, keys=%v", keys)
			}
		})
	}
}

func TestClaudeRewindRestoreCodeRemoteCodeMismatchCleansUp(t *testing.T) {
	fastRewindTimings(t)
	fp, plan := newRealCCCodeRewindFake(
		realCCAboveThreeTurns,
		"a.txt +2 -1",
		"The code will be restored +1 -2 in a.txt.",
		true,
	)
	plan.RestoreCode = true
	plan.ExpectedFiles = []string{"a.txt"}
	fp.onCommitEnterOpt = func(_ context.Context, _ *fakeClaudeRewindPane, _ string, restoreCode bool, expectedFiles []string) (claudeRewindCommit, error) {
		if !restoreCode || !slices.Equal(expectedFiles, []string{"a.txt"}) {
			t.Fatalf("unexpected commitEnter args: restoreCode=%v expectedFiles=%v", restoreCode, expectedFiles)
		}
		// Simulate remote host-side awk exiting 37 without sending Enter.
		return evalCommitExec(sshx.ExecResult{Ended: true, Started: true}, &sshxExitStub{37})
	}

	_, err := claudeRewind(context.Background(), fp, plan)
	if err == nil || err.code != "code_mismatch" {
		t.Fatalf("got err=%v, want code_mismatch", err)
	}
	keys := fp.recordedKeys()
	enters := 0
	for _, k := range keys {
		if k == "enter" {
			enters++
		}
	}
	if enters != 1 {
		t.Fatalf("must not press commit enter when remote script exited 37; enters=%d keys=%v", enters, keys)
	}
	if keys[len(keys)-1] != "esc" {
		t.Fatalf("expected esc cleanup after remote exit 37, keys=%v", keys)
	}
}

func TestClaudeRewindCommitScriptCodeRestoreAtomicCheck(t *testing.T) {
	const (
		sid   = "11111111-2222-3333-4444-555555555555"
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

			run := func(screen, target string, restoreCode bool, files []string) (string, int, string) {
				_ = os.Remove(keyLog)
				if err := os.WriteFile(scrFile, []byte(screen), 0o644); err != nil {
					t.Fatal(err)
				}
				sh, err := claudeRewindCommitOptSh(mode, pane, sock, "9001", pid, start, sid, target, restoreCode, files)
				if err != nil {
					t.Fatal(err)
				}
				out, code := runRewindSh(t, sh, binDir, homeDir, fakeProc)
				logged, _ := os.ReadFile(keyLog)
				return out, code, string(logged)
			}

			// 1) Single file real screen -> exit 0, sends Enter.
			scrOne := realCCConfirmScreen(realCCAboveThreeTurns, "third message", "The code will be restored +1 -2 in a.txt.", true, 0)
			out, code, keys := run(scrOne, "third message", true, []string{"sub/a.txt"})
			if ok, remoteAt := parseClaudeRewindCommitOut([]byte(out)); code != 0 || !ok || remoteAt.IsZero() || keys == "" {
				t.Fatalf("single file code restore failed: code=%d out=%q keys=%q", code, out, keys)
			}

			// 2) Two files real screen ("a.txt and b.txt") -> exit 0, sends Enter regardless of order.
			scrTwo := realCCConfirmScreen(realCCAboveTwoFiles, "third message", "The code will be restored +1 -5 in a.txt and b.txt.", true, 0)
			out, code, keys = run(scrTwo, "third message", true, []string{"sub/b.txt", "a.txt"})
			if ok, _ := parseClaudeRewindCommitOut([]byte(out)); code != 0 || !ok || keys == "" {
				t.Fatalf("two files code restore failed: code=%d out=%q keys=%q", code, out, keys)
			}
			// Two distinct paths sharing the same basename ("src/b.txt", "test/b.txt") -> "b.txt and b.txt." passes!
			scrTwoSame := realCCConfirmScreen(realCCAboveTwoFiles, "third message", "The code will be restored -2 in b.txt and b.txt.", true, 0)
			out, code, keys = run(scrTwoSame, "third message", true, []string{"src/b.txt", "test/b.txt"})
			if ok, _ := parseClaudeRewindCommitOut([]byte(out)); code != 0 || !ok || keys == "" {
				t.Fatalf("two duplicate-basename paths failed: code=%d out=%q keys=%q", code, out, keys)
			}
			// Two different basenames must NOT match "a.txt and a.txt.".
			scrTwoDupMismatch := realCCConfirmScreen(realCCAboveTwoFiles, "third message", "The code will be restored -2 in a.txt and a.txt.", true, 0)
			_, code, keys = run(scrTwoDupMismatch, "third message", true, []string{"a.txt", "sub/b.txt"})
			if code != 37 || keys != "" {
				t.Fatalf("two different basenames matched duplicate screen: code=%d keys=%q, want 37 and empty", code, keys)
			}

			// 3) Real 4-file (.brief/screens3/screen-3.txt, with duplicate basename src/b.txt & test/b.txt)
			//    and 10-file (.brief/screens3/screen-4.txt) "in <name> and N other files." screens -> exit 0.
			fourPaths := []string{"a.txt", "src/b.txt", "src/c.txt", "test/b.txt"}
			scrFour := realCCConfirmScreen(realCCAboveTwoFiles, "third message", "The code will be restored -4 in a.txt and 3 other files.", true, 0)
			out, code, keys = run(scrFour, "third message", true, fourPaths)
			if ok, _ := parseClaudeRewindCommitOut([]byte(out)); code != 0 || !ok || keys == "" {
				t.Fatalf("4-file code restore failed: code=%d out=%q keys=%q", code, out, keys)
			}
			// Wrapped 10-file screen -> exit 0.
			tenPaths := []string{"d1.txt", "d2.txt", "d3.txt", "d4.txt", "d5.txt", "d6.txt", "a.txt", "src/b.txt", "src/c.txt", "test/b.txt"}
			for _, desc := range []string{
				"The code will be restored -10 in d1.txt and 9 other files.",
				"The code will be restored -10 in d1.txt and\n  9 other files.",
			} {
				scrTen := realCCConfirmScreen(realCCAboveTwoFiles, "first message", desc, true, 0)
				out, code, keys = run(scrTen, "first message", true, tenPaths)
				if ok, _ := parseClaudeRewindCommitOut([]byte(out)); code != 0 || !ok || keys == "" {
					t.Fatalf("10-file (%q) failed: code=%d out=%q keys=%q", desc, code, out, keys)
				}
			}
			// 3+ files: wrong count, unknown first name, or non-"and N other files" comma shapes -> MUST exit 37, no Enter.
			for _, badMany := range []struct {
				desc  string
				files []string
			}{
				{"The code will be restored -4 in a.txt and 2 other files.", fourPaths},
				{"The code will be restored -4 in a.txt and 4 other files.", fourPaths},
				{"The code will be restored -4 in z.txt and 3 other files.", fourPaths},
				{"The code will be restored +3 -1 in a.txt, b.txt and c.txt.", []string{"a.txt", "b.txt", "c.txt"}},
				{"The code will be restored +3 -1 in a.txt, b.txt, and c.txt.", []string{"a.txt", "b.txt", "c.txt"}},
				{"The code will be restored -4 in 4 files.", fourPaths},
				{"The code will be restored -2 in a.txt and 1 other file.", []string{"a.txt", "b.txt"}},
				{"The code will be restored -2 in a.txt and 1 other files.", []string{"a.txt", "b.txt"}},
				{"The code will be restored -4 in a.txt and 03 other files.", fourPaths},
			} {
				scrBadMany := realCCConfirmScreen(realCCAboveTwoFiles, "third message", badMany.desc, true, 0)
				_, code, keys = run(scrBadMany, "third message", true, badMany.files)
				if code != 37 || keys != "" {
					t.Fatalf("bad many desc %q for %v: code=%d keys=%q, want 37 and empty", badMany.desc, badMany.files, code, keys)
				}
			}

			// 4) Selection moved to option 3 ("Restore code" only, which STILL has the
			//    matching "The code will be restored +1 -2 in a.txt." line!) -> MUST exit 35, no Enter.
			scrOpt3 := realCCConfirmScreen(realCCAboveThreeTurns, "third message", "The code will be restored +1 -2 in a.txt.", true, 2)
			_, code, keys = run(scrOpt3, "third message", true, []string{"a.txt"})
			if code != 35 || keys != "" {
				t.Fatalf("option 3 selected: code=%d keys=%q, want 35 and empty", code, keys)
			}

			// 5) Selection moved to option 2 ("Restore conversation") when code restore requested -> exit 35, no Enter.
			scrOpt2 := realCCConfirmScreen(realCCAboveThreeTurns, "third message", "The code will be restored +1 -2 in a.txt.", true, 1)
			_, code, keys = run(scrOpt2, "third message", true, []string{"a.txt"})
			if code != 35 || keys != "" {
				t.Fatalf("option 2 selected: code=%d keys=%q, want 35 and empty", code, keys)
			}

			// 6) Conversation-only request on option 1 -> exit 35, no Enter; on option 2 -> exit 0, sends Enter.
			_, code, keys = run(scrOne, "third message", false, nil)
			if code != 35 || keys != "" {
				t.Fatalf("conv-only on option 1: code=%d keys=%q, want 35 and empty", code, keys)
			}
			out, code, keys = run(scrOpt2, "third message", false, nil)
			if ok, _ := parseClaudeRewindCommitOut([]byte(out)); code != 0 || !ok || keys == "" {
				t.Fatalf("conv-only on option 2 failed: code=%d out=%q keys=%q", code, out, keys)
			}

			// 7) File mismatch -> MUST exit 37, no Enter.
			_, code, keys = run(scrTwo, "third message", true, []string{"a.txt"})
			if code != 37 || keys != "" {
				t.Fatalf("file mismatch (extra on screen): code=%d keys=%q, want 37 and empty", code, keys)
			}
			_, code, keys = run(scrOne, "third message", true, []string{"a.txt", "b.txt"})
			if code != 37 || keys != "" {
				t.Fatalf("file mismatch (missing on screen): code=%d keys=%q, want 37 and empty", code, keys)
			}

			// 8) Unrecognizable code description lines -> MUST exit 37, no Enter.
			for _, desc := range []string{
				"The code will be restored +1 -5 in 3 files.",
				"The code will be restored +1 -5 in a.txt, b.txt and 2 more.",
				"The code will be restored +1 -5 in a.txt…",
				"The code will be unchanged.",
				"",
			} {
				scrBad := realCCConfirmScreen(realCCAboveThreeTurns, "third message", desc, true, 0)
				_, code, keys = run(scrBad, "third message", true, []string{"a.txt"})
				if code != 37 || keys != "" {
					t.Fatalf("unrecognized desc %q: code=%d keys=%q, want 37 and empty", desc, code, keys)
				}
			}

			// 9) Quoted user message trying to spoof "The code will be restored ..." -> MUST exit 37, no Enter.
			scrSpoof := realCCConfirmScreen(realCCAboveThreeTurns, "The code will be restored +1 -2 in a.txt.", "The code will be unchanged.", true, 0)
			_, code, keys = run(scrSpoof, "The code will be restored +1 -2 in a.txt.", true, []string{"a.txt"})
			if code != 37 || keys != "" {
				t.Fatalf("quoted spoof matched: code=%d keys=%q, want 37 and empty", code, keys)
			}

			// 10) Regex/shell metacharacters and " and " inside filenames -> exact match, no injection.
			evilFile := `a+b[1]'$HOME.txt`
			scrEvil := realCCConfirmScreen(realCCAboveThreeTurns, "third message", "The code will be restored +1 -2 in "+evilFile+".", true, 0)
			out, code, keys = run(scrEvil, "third message", true, []string{evilFile})
			if ok, _ := parseClaudeRewindCommitOut([]byte(out)); code != 0 || !ok || keys == "" {
				t.Fatalf("metachar filename failed: code=%d out=%q keys=%q", code, out, keys)
			}
			// Single filename containing " and ".
			scrAndOne := realCCConfirmScreen(realCCAboveThreeTurns, "third message", "The code will be restored +1 -2 in fish and chips.txt.", true, 0)
			out, code, keys = run(scrAndOne, "third message", true, []string{"/repo/fish and chips.txt"})
			if ok, _ := parseClaudeRewindCommitOut([]byte(out)); code != 0 || !ok || keys == "" {
				t.Fatalf("single filename with ' and ' failed: code=%d out=%q keys=%q", code, out, keys)
			}
			// Two files where one contains " and " (both orders accepted).
			for _, desc := range []string{
				"The code will be restored +1 -5 in fish and chips.txt and a.txt.",
				"The code will be restored +1 -5 in a.txt and fish and chips.txt.",
			} {
				scrAndTwo := realCCConfirmScreen(realCCAboveTwoFiles, "third message", desc, true, 0)
				out, code, keys = run(scrAndTwo, "third message", true, []string{"/repo/fish and chips.txt", "/repo/a.txt"})
				if ok, _ := parseClaudeRewindCommitOut([]byte(out)); code != 0 || !ok || keys == "" {
					t.Fatalf("two files with ' and ' (%q) failed: code=%d out=%q keys=%q", desc, code, out, keys)
				}
			}
			// Three files where the shown filename contains " and ".
			scrAndThree := realCCConfirmScreen(realCCAboveTwoFiles, "third message", "The code will be restored -3 in fish and chips.txt and 2 other files.", true, 0)
			out, code, keys = run(scrAndThree, "third message", true, []string{"/repo/fish and chips.txt", "/repo/a.txt", "/repo/b.txt"})
			if ok, _ := parseClaudeRewindCommitOut([]byte(out)); code != 0 || !ok || keys == "" {
				t.Fatalf("three files with ' and ' failed: code=%d out=%q keys=%q", code, out, keys)
			}

			// 11) Direct awk guard: OPT_WANT outside the 2 allowlisted labels MUST exit 35.
			for _, badOpt := range []string{"", "Restore code", "Summarize from here", "Never mind"} {
				cmd := exec.Command("awk", claudeRewindCommitAwk)
				cmd.Env = append(os.Environ(), "LC_ALL=C", "TARGET_FIRST=third message", "TARGET_TRUNC=0", "OPT_WANT="+badOpt, "EXPECT_FILES=a.txt")
				cmd.Stdin = strings.NewReader(scrOpt3)
				err := cmd.Run()
				var ee *exec.ExitError
				if !errors.As(err, &ee) || ee.ExitCode() != 35 {
					t.Fatalf("OPT_WANT=%q: got err=%v, want exit 35", badOpt, err)
				}
			}
		})
	}
	if re := mapRewindExitErr(&sshxExitStub{37}); asRewindErr(re).code != "code_mismatch" || isIdentityErr(re) {
		t.Fatalf("exit 37 mapped to %v, want non-identity code_mismatch", re)
	}
}

func TestClaudeRewindRestoreCodeManyFilesRealScreens(t *testing.T) {
	fastRewindTimings(t)
	fourPaths := []string{"a.txt", "src/b.txt", "src/c.txt", "test/b.txt"}
	tenPaths := []string{"d1.txt", "d2.txt", "d3.txt", "d4.txt", "d5.txt", "d6.txt", "a.txt", "src/b.txt", "src/c.txt", "test/b.txt"}

	t.Run("four_files_with_duplicate_basename", func(t *testing.T) {
		fp, plan := newRealCCCodeRewindFake(
			realCCAboveTwoFiles,
			"4 files changed +4",
			"The code will be restored -4 in a.txt and 3 other files.",
			true,
		)
		plan.RestoreCode = true
		plan.ExpectedFiles = fourPaths

		res, err := claudeRewind(context.Background(), fp, plan)
		if err != nil {
			t.Fatalf("expected success, got err: %v", err)
		}
		if !res.OK || !res.RestoredCode {
			t.Fatalf("unexpected result: %+v", res)
		}
		wantKeys := []string{"esc+esc", "up", "enter", "enter", "ctrl+c"}
		if got := fp.recordedKeys(); !slices.Equal(got, wantKeys) {
			t.Fatalf("keys = %v, want %v", got, wantKeys)
		}
	})

	t.Run("ten_files_across_turns", func(t *testing.T) {
		fp, plan := newRealCCCodeRewindFake(
			realCCAboveTwoFiles,
			"4 files changed +4",
			"The code will be restored -10 in d1.txt and 9 other files.",
			true,
		)
		plan.RestoreCode = true
		plan.ExpectedFiles = tenPaths

		res, err := claudeRewind(context.Background(), fp, plan)
		if err != nil {
			t.Fatalf("expected success, got err: %v", err)
		}
		if !res.OK || !res.RestoredCode {
			t.Fatalf("unexpected result: %+v", res)
		}
	})

	t.Run("two_paths_same_basename", func(t *testing.T) {
		fp, plan := newRealCCCodeRewindFake(
			realCCAboveTwoFiles,
			"2 files changed +2",
			"The code will be restored -2 in b.txt and b.txt.",
			true,
		)
		plan.RestoreCode = true
		plan.ExpectedFiles = []string{"src/b.txt", "test/b.txt"}

		res, err := claudeRewind(context.Background(), fp, plan)
		if err != nil {
			t.Fatalf("expected success, got err: %v", err)
		}
		if !res.OK || !res.RestoredCode {
			t.Fatalf("unexpected result: %+v", res)
		}
	})

	t.Run("wrong_other_files_count_refuses_enter", func(t *testing.T) {
		fp, plan := newRealCCCodeRewindFake(
			realCCAboveTwoFiles,
			"4 files changed +4",
			"The code will be restored -4 in a.txt and 2 other files.",
			true,
		)
		plan.RestoreCode = true
		plan.ExpectedFiles = fourPaths

		_, err := claudeRewind(context.Background(), fp, plan)
		if err == nil || err.code != "code_mismatch" {
			t.Fatalf("got err=%v, want code_mismatch", err)
		}
		keys := fp.recordedKeys()
		if keys[len(keys)-1] != "esc" {
			t.Fatalf("expected esc cleanup, keys=%v", keys)
		}
	})

	t.Run("first_name_not_in_expected_refuses_enter", func(t *testing.T) {
		fp, plan := newRealCCCodeRewindFake(
			realCCAboveTwoFiles,
			"4 files changed +4",
			"The code will be restored -4 in other.txt and 3 other files.",
			true,
		)
		plan.RestoreCode = true
		plan.ExpectedFiles = fourPaths

		_, err := claudeRewind(context.Background(), fp, plan)
		if err == nil || err.code != "code_mismatch" {
			t.Fatalf("got err=%v, want code_mismatch", err)
		}
		keys := fp.recordedKeys()
		if keys[len(keys)-1] != "esc" {
			t.Fatalf("expected esc cleanup, keys=%v", keys)
		}
	})
}

func TestClaudeRewindNormalizeFilesAndParseParity(t *testing.T) {
	// Distinct paths sharing a basename (a.txt and dir/a.txt) MUST both be counted!
	bases, cleaned, err := normalizeRewindFiles([]string{"sub/b.txt", "a.txt", "dir/a.txt", "sub/b.txt"})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if !slices.Equal(bases, []string{"a.txt", "a.txt", "b.txt"}) {
		t.Fatalf("bases = %v, want [a.txt a.txt b.txt]", bases)
	}
	if !slices.Equal(cleaned, []string{"sub/b.txt", "a.txt", "dir/a.txt"}) {
		t.Fatalf("cleaned = %v", cleaned)
	}
	for _, bad := range [][]string{
		nil,
		{},
		{""},
		{"   "},
		{"a.txt\nb.txt"},
		{"a.txt\x00"},
		{"/"},
	} {
		if _, _, err := normalizeRewindFiles(bad); err == nil {
			t.Fatalf("expected error for bad files %q", bad)
		}
	}

	fourPaths := []string{"a.txt", "src/b.txt", "src/c.txt", "test/b.txt"}
	tenPaths := []string{"d1.txt", "d2.txt", "d3.txt", "d4.txt", "d5.txt", "d6.txt", "a.txt", "src/b.txt", "src/c.txt", "test/b.txt"}

	goodCases := []struct {
		line  string
		paths []string
	}{
		{"The code will be restored +1 -2 in a.txt.", []string{"a.txt"}},
		{"The code will be restored +1 in a.txt.", []string{"sub/a.txt"}},
		{"The code will be restored -2 in a.txt.", []string{"a.txt"}},
		{"The code will be restored in a.txt.", []string{"a.txt"}},
		{"The code will be restored +1,200 -5 in a.txt.", []string{"a.txt"}},
		{"The code will be restored +1 -2 in fish and chips.txt.", []string{"/repo/fish and chips.txt"}},
		{"The code will be restored +1 -5 in a.txt and b.txt.", []string{"sub/b.txt", "a.txt"}},
		{"The code will be restored +1 -5 in fish and chips.txt and a.txt.", []string{"/repo/a.txt", "/repo/fish and chips.txt"}},
		{"The code will be restored +1 -5 in a.txt and fish and chips.txt.", []string{"/repo/fish and chips.txt", "/repo/a.txt"}},
		{"The code will be restored +1 -5 in fish and chips.txt and salt and pepper.txt.", []string{"fish and chips.txt", "salt and pepper.txt"}},
		{"The code will be restored -2 in b.txt and b.txt.", []string{"src/b.txt", "test/b.txt"}},
		{"The code will be restored -4 in a.txt and 3 other files.", fourPaths},
		{"The code will be restored -4 in b.txt and 3 other files.", fourPaths},
		{"The code will be restored -3 in fish and chips.txt and 2 other files.", []string{"fish and chips.txt", "a.txt", "b.txt"}},
		{"The code will be restored -10 in d1.txt and 9 other files.", tenPaths},
	}
	for _, tc := range goodCases {
		if !matchRewindCodeLine(tc.line, tc.paths) {
			t.Fatalf("matchRewindCodeLine(%q, %v) = false, want true", tc.line, tc.paths)
		}
	}

	mismatchCases := []struct {
		line  string
		paths []string
	}{
		{"The code will be unchanged.", []string{"a.txt"}},
		{"The code will be restored +1 -2.", []string{"a.txt"}},
		{"The code will be restored +1 -2 in .", []string{"a.txt"}},
		{"The code will be restored +1 -2 in a.txt…", []string{"a.txt"}},
		{"The code will be restored +1 -2 in a.txt...", []string{"a.txt"}},
		{"The code will be restored +1 -2 in fish and chips.txt", []string{"fish and chips.txt"}}, // missing trailing period
		{"The code will be restored +1 -5 in a.txt and a.txt.", []string{"a.txt", "b.txt"}},
		{"The code will be restored -2 in b.txt.", []string{"src/b.txt", "test/b.txt"}},
		{"The code will be restored -2 in a.txt and 1 other file.", []string{"a.txt", "b.txt"}},
		{"The code will be restored -2 in a.txt and 1 other files.", []string{"a.txt", "b.txt"}},
		{"The code will be restored +3 -1 in a.txt, b.txt and c.txt.", []string{"a.txt", "b.txt", "c.txt"}},
		{"The code will be restored +3 -1 in a.txt, b.txt, and c.txt.", []string{"a.txt", "b.txt", "c.txt"}},
		{"The code will be restored -4 in 4 files.", fourPaths},
		{"The code will be restored -4 in a.txt and 2 other files.", fourPaths},
		{"The code will be restored -4 in z.txt and 3 other files.", fourPaths},
		{"The code will be restored -4 in a.txt and 03 other files.", fourPaths},
		{"The code will be restored -10 in d1.txt and 8 other files.", tenPaths},
		{"The code will be restored +1 -2 in a.txt and b.txt and c.txt.", []string{"a.txt", "b.txt", "c.txt"}},
	}
	for _, tc := range mismatchCases {
		if matchRewindCodeLine(tc.line, tc.paths) {
			t.Fatalf("matchRewindCodeLine(%q, %v) = true, want false", tc.line, tc.paths)
		}
	}
}

func TestClaudeRewindVerifyCodePathsAgainstLog(t *testing.T) {
	mkUser := func(seq int, id, text string) convo.Entry {
		return convo.Entry{
			Seq:   seq,
			ID:    id,
			Role:  convo.RoleUser,
			Parts: []convo.Part{{Kind: convo.KindText, Text: text}},
		}
	}
	mkTool := func(seq int, name, detail string, isErr, gone bool) convo.Entry {
		return convo.Entry{
			Seq:  seq,
			ID:   "a-" + strconv.Itoa(seq),
			Role: convo.RoleAssistant,
			Gone: gone,
			Parts: []convo.Part{{
				Kind: convo.KindTool,
				Tool: &convo.Tool{Name: name, Detail: detail, Error: isErr},
			}},
		}
	}

	// 1. Same basename in different directories: /shown/a.txt vs /other/a.txt -> code_mismatch.
	entriesOtherDir := []convo.Entry{
		mkUser(1, "u1", "first"),
		mkTool(2, "Write", "/other/a.txt", false, false),
	}
	if _, err := verifyRewindCodePaths(entriesOtherDir, "u1", []string{"/shown/a.txt"}); err == nil || err.code != "code_mismatch" {
		t.Fatalf("same basename different dir: got err=%v, want code_mismatch", err)
	}

	// 2. 3+ files where screen would show "in a.txt and 2 other files." for both,
	//    but actual full paths contain secret.txt & config.json -> code_mismatch.
	entriesThreeActual := []convo.Entry{
		mkUser(1, "u1", "first"),
		mkTool(2, "Write", "/repo/a.txt", false, false),
		mkTool(3, "Edit", "/repo/secret.txt", false, false),
		mkTool(4, "MultiEdit", "/repo/config.json", false, false),
	}
	if _, err := verifyRewindCodePaths(entriesThreeActual, "u1", []string{"/repo/a.txt", "/repo/b.txt", "/repo/c.txt"}); err == nil || err.code != "code_mismatch" {
		t.Fatalf("3+ files swapped hidden files: got err=%v, want code_mismatch", err)
	}

	// 3. Different order, identical full-path set (including duplicate basenames in different dirs,
	//    repeated edits, ignored Bash/Read/failed/gone/before-target edits) -> passes.
	entriesMixed := []convo.Entry{
		mkUser(1, "u0", "earlier"),
		mkTool(2, "Write", "/repo/before.txt", false, false),
		mkUser(3, "u1", "target"),
		mkTool(4, "Write", "/repo/a.txt", false, false),
		mkTool(5, "Edit", "/repo/src/b.txt", false, false),
		mkTool(6, "MultiEdit", "/repo/test/b.txt", false, false),
		mkTool(7, "NotebookEdit", "/repo/nb/demo.ipynb", false, false),
		mkTool(8, "Edit", "/repo/a.txt", false, false),           // duplicate path
		mkTool(9, "Write", "/repo/failed.txt", true, false),      // errored edit -> ignored
		mkTool(10, "Write", "/repo/gone.txt", false, true),       // gone entry -> ignored
		mkTool(11, "Bash", "echo hi > /repo/bash.txt", false, false),
		mkTool(12, "Read", "/repo/read.txt", false, false),
	}
	got, err := verifyRewindCodePaths(entriesMixed, "u1", []string{
		"/repo/nb/demo.ipynb",
		"/repo/test/b.txt",
		"/repo/a.txt",
		"/repo/src/b.txt",
	})
	if err != nil {
		t.Fatalf("same full-path set in different order failed: %v", err)
	}
	wantLog := []string{"/repo/a.txt", "/repo/src/b.txt", "/repo/test/b.txt", "/repo/nb/demo.ipynb"}
	if !slices.Equal(got, wantLog) {
		t.Fatalf("verified paths = %v, want %v", got, wantLog)
	}

	// 4. Empty requested list OR no edit-tool files in log -> no_code_changes.
	if _, err := verifyRewindCodePaths(entriesMixed, "u1", nil); err == nil || err.code != "no_code_changes" {
		t.Fatalf("empty requested files: got err=%v, want no_code_changes", err)
	}
	entriesNoEdits := []convo.Entry{
		mkUser(1, "u1", "target"),
		mkTool(2, "Bash", "echo hi > /repo/bash.txt", false, false),
		mkTool(3, "Write", "/repo/failed.txt", true, false),
	}
	if _, err := verifyRewindCodePaths(entriesNoEdits, "u1", []string{"/repo/bash.txt"}); err == nil || err.code != "no_code_changes" {
		t.Fatalf("no edit tools in log: got err=%v, want no_code_changes", err)
	}
}

func TestClaudeRewindRestoreCodeFilenameWithAndFlow(t *testing.T) {
	fastRewindTimings(t)

	t.Run("single_file_with_and", func(t *testing.T) {
		fp, plan := newRealCCCodeRewindFake(
			realCCAboveThreeTurns,
			"fish and chips.txt +2 -1",
			"The code will be restored +1 -2 in fish and chips.txt.",
			true,
		)
		plan.RestoreCode = true
		plan.ExpectedFiles = []string{"/repo/fish and chips.txt"}

		res, err := claudeRewind(context.Background(), fp, plan)
		if err != nil {
			t.Fatalf("expected success, got err: %v", err)
		}
		if !res.OK || !res.RestoredCode {
			t.Fatalf("unexpected result: %+v", res)
		}
	})

	t.Run("two_files_one_with_and", func(t *testing.T) {
		fp, plan := newRealCCCodeRewindFake(
			realCCAboveTwoFiles,
			"2 files changed +5 -1",
			"The code will be restored +1 -5 in a.txt and fish and chips.txt.",
			true,
		)
		plan.RestoreCode = true
		plan.ExpectedFiles = []string{"/repo/fish and chips.txt", "/repo/a.txt"}

		res, err := claudeRewind(context.Background(), fp, plan)
		if err != nil {
			t.Fatalf("expected success, got err: %v", err)
		}
		if !res.OK || !res.RestoredCode {
			t.Fatalf("unexpected result: %+v", res)
		}
	})
}
