package web

import (
	"regexp"
	"strings"
)

// codexIdleInput: Codex at rest, its input empty: nothing says it works
// ("esc to interrupt"), and its input ("› " at the left, the last such,
// with the lines under it that go on from it, up to a blank one) holds its
// placeholder only. Any other words there are taken for a draft (Codex's
// placeholder varies, dimmed, which the text alone does not tell apart: a
// command is then refused, not typed onto it).
func codexIdleInput(screen string) error {
	if strings.Contains(screen, "esc to interrupt") {
		return errBusyAgent
	}
	lines := strings.Split(screen, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if !strings.HasPrefix(lines[i], "› ") {
			continue
		}
		if in := strings.TrimSpace(strings.TrimPrefix(lines[i], "› ")); in != "" && in != "Ask Codex to do anything" {
			return errDraft
		}
		// (Under it, down to its footer — its model and folder, "? for
		// shortcuts" —: any words are the input's, a draft, blank lines among
		// them or not.)
		for _, l := range lines[i+1:] {
			t := strings.TrimSpace(l)
			if t == "" {
				continue
			}
			if codexFooter.MatchString(t) {
				break
			}
			return errDraft
		}
		return nil
	}
	return errScreen
}

// codexFooter: a line of Codex's footer under its input: its model and
// folder ("gpt-mock default · ~/cxreal"), its hints ("? for shortcuts",
// "esc again to edit previous message"), its notes ("⚠ 1 warning").
var codexFooter = regexp.MustCompile(`^\S.* · (?:~|/)\S*(?: · .*)?$|^\? for shortcuts\b|^esc again to edit previous message$|^ctrl \+ c again to quit$|^⚠ \d+ warnings? · f2 to view$|\s{2,}⚠ \d+ warnings? · f2 to view$`)
