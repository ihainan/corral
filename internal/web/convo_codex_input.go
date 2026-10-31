package web

import "strings"

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
		for _, l := range lines[i+1:] {
			if strings.TrimSpace(l) == "" {
				break
			}
			if strings.HasPrefix(l, "  ") {
				return errDraft // (a line of the input under its first)
			}
			break
		}
		return nil
	}
	return errScreen
}
