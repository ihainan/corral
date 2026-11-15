package web

import "testing"

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
