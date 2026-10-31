package web

import "strings"

// codexIdleInput: Codex at rest, its input empty: nothing says it works
// ("esc to interrupt"), and its input's line ("› " at the left, the last
// such) holds its placeholder only.
func codexIdleInput(screen string) error {
	if strings.Contains(screen, "esc to interrupt") {
		return errBusyAgent
	}
	lines := strings.Split(screen, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if !strings.HasPrefix(lines[i], "› ") {
			continue
		}
		if in := strings.TrimSpace(strings.TrimPrefix(lines[i], "› ")); in != "" && in != "Ask Codex to do anything" && !strings.HasPrefix(in, "Implement {feature}") {
			return errDraft
		}
		return nil
	}
	return errScreen
}
