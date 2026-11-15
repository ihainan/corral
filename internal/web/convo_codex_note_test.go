package web

import (
	"fmt"
	"strings"
	"testing"
)

// Screens as a real Codex 0.167.2 drew them (cxaudit.py, 11-15).
func TestCodexWorkNote(t *testing.T) {
	const tail = "\n\n› Ask Codex to do anything\n\n  GPT-6.1-Sol high · ~/cxreal/proj · Working · Context 100% left · 01a1dd6f\n  ← for agents · ? for shortcuts\n"
	for name, c := range map[string]struct {
		screen  string
		compact bool
		retry   *codexRetry
	}{
		"retrying": {"› rate limited slowly #r429\n\n• Reconnecting... 2/5 (7s • esc to interrupt)\n  └ Exceeded retry limit, last status: 429 Too Many Requests" + tail, false,
			&codexRetry{N: 2, Of: 5, Why: "Exceeded retry limit, last status: 429 Too Many Requests"}},
		"retrying, blinking, with terminals": {"◦ Reconnecting... 1/5 (3s • esc to interrupt) · 1 background terminal running · /ps to view · /stop to close" + tail, false,
			&codexRetry{N: 1, Of: 5}},
		"compacting": {"• Compacting context (4s • esc to interrupt)\n  └ Making room to continue." + tail, true, nil},
		"working":    {"• Working (6s • esc to interrupt) · 1 background terminal running · /ps to view · /stop to close" + tail, false, nil},
		// (Its words higher up, then at work on something else: not what it does now.)
		"said before": {"• Compacting context (4s • esc to interrupt)\n• Context compacted · 8s\n\n• Working (2s • esc to interrupt)" + tail, false, nil},
		"done":        {"• Context compacted · 8s\n  Worked for 8s • 04:22" + tail, false, nil},
		"quoted":      {"• the docs say \"• Compacting context (4s • esc to interrupt)\" when it compacts" + tail, false, nil},
		// (Pasted into its box while it works: below its status line.)
		"in the box": {"• Working (6s • esc to interrupt)\n\n› look at this\n  • Reconnecting... 2/5 (7s • esc to interrupt)\n\n  GPT-6.1-Sol high · Working\n", false, nil},
		"no box":     {"• Reconnecting... 2/5 (7s • esc to interrupt)\n", false, nil},
	} {
		compact, retry := codexWorkNote(c.screen)
		if compact != c.compact || (retry == nil) != (c.retry == nil) || (retry != nil && *retry != *c.retry) {
			t.Errorf("%s: %v %+v", name, compact, retry)
		}
	}
}

// Its approvals reviewer at work (auto_review), a real screen (11-15).
func TestCodexWorkNoteReview(t *testing.T) {
	screen := "• Reviewing approval request (5s • esc to interrupt)\n  └ /usr/bin/zsh -lc \"printf 'hello\\n' > /home/u/outside.txt\"\n\n› Ask Codex to do anything\n\n  GPT-6.1-Sol high Fast · ~/p · Working\n"
	if _, _, review := codexWorkNoteAll(screen); !strings.Contains(review, "outside.txt") {
		t.Fatalf("review: %q", review)
	}
}

// "Implement this plan?" as a real Codex 0.167.2 draws it (plan mode, 11-15),
// its marked choice any of the three; not where its words are the agent's.
func TestCodexPlanPrompt(t *testing.T) {
	asked := "• Proposed Plan\n  # Fix the tests\n  1. Read the failing test.\n• I looked at it.\n  Worked for <1s • 11:27 AM\n\n  Implement this plan?\n%s\n\n  enter select · esc back\n"
	for _, menu := range []string{
		"› 1. Yes, implement this plan\n  2. Yes, clear context and implement\n  3. No, stay in Plan mode",
		"  1. Yes, implement this plan\n  2. Yes, clear context and implement\n› 3. No, stay in Plan mode",
	} {
		if !codexPlanPrompt(fmt.Sprintf(asked, menu)) {
			t.Errorf("not told: %q", menu[:20])
		}
	}
	for _, screen := range []string{
		"• Implement this plan?\n\n› Ask Codex to do anything\n\n  GPT-6.1-Sol medium · ~/p\n",
		"  Would you like to run the following command?\n\n› 1. Yes, proceed (y)\n  2. No (esc)\n\n  Press enter to confirm or esc to cancel\n",
		"  Implement this plan?\n› 1. Yes, implement this plan\n  2. No\n\n  GPT-6.1-Sol medium · ~/p\n", // (no menu keys: a draft)
	} {
		if codexPlanPrompt(screen) {
			t.Errorf("told: %q", screen[:30])
		}
	}
}
