package web

import (
	"regexp"
	"strconv"
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
// folder ("gpt-mock default · ~/cxreal", the folder's name spaces and all;
// or "Context 100% left", a status line set to that; one set otherwise is
// not known, its input taken for a draft: commands refused, nothing lost),
// its hints ("? for shortcuts", "esc again to edit previous message"), its
// notes ("⚠ 1 warning · f2 to view"), each the whole line but for what
// stands at its right, two spaces off or more.
var codexFooter = regexp.MustCompile(`^(?:[A-Za-z][\w./-]*(?: (?:default|minimal|low|medium|high|xhigh|max))? · (?:~|/).*|Context \d+% (?:left|used)(?: · .*)?|\? for shortcuts|esc again to edit previous message|ctrl \+ c again to quit|⚠ \d+ warnings? · f2 to view)(?:\s{2,}\S.*)?$`)

// codexInputArea: what Codex's input holds — its last line beginning
// "› " (the person's messages above begin so too: the last is the input),
// and the lines under it down to its footer; ok only where that footer is
// there, close under it (else what is at the bottom is not told: a
// dialog, a menu whose choice is marked "› 1."). Its placeholder, when
// empty, is what it holds.
func codexInputArea(screen string) (area string, ok bool) {
	lines := strings.Split(strings.TrimRight(screen, "\n"), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if !strings.HasPrefix(lines[i], "› ") && lines[i] != "›" {
			continue
		}
		if codexMenuChoice.MatchString(lines[i]) {
			return "", false
		}
		in := []string{strings.TrimPrefix(strings.TrimPrefix(lines[i], "›"), " ")}
		for j := i + 1; j < len(lines) && j-i <= codexInputMax; j++ {
			if codexFooter.MatchString(strings.TrimSpace(lines[j])) {
				return strings.TrimSpace(strings.Join(in, "\n")), true
			}
			in = append(in, lines[j])
		}
		return "", false
	}
	return "", false
}

// (A menu's choice, marked: "› 1. Yes, proceed".)
var codexMenuChoice = regexp.MustCompile(`^›\s*\d+\.\s`)

// codexInputMax: the most lines Codex's input runs to above its footer
// (it grows with what it holds, then scrolls).
const codexInputMax = 40

// codexTypedIn: typedIn for Codex's input — the message there from its
// start (whole: all of it and nothing else). A long paste Codex folds to
// "[Pasted Content N chars]": the message there where N is its length,
// never whole (what it holds cannot be seen).
func codexTypedIn(screen, text string, whole bool) bool {
	area, ok := codexInputArea(screen)
	if !ok {
		return false
	}
	box, want := squeeze(area), squeeze(text)
	if len([]rune(want)) < sentMark {
		return false
	}
	if m := codexPasted.FindStringSubmatch(box); m != nil {
		if whole {
			return false
		}
		n, _ := strconv.Atoi(m[1])
		return n == len([]rune(text)) || n == len(text)
	}
	if whole {
		return box == want
	}
	head := []rune(want)
	return strings.HasPrefix(box, string(head[:min(len(head), typedHead)]))
}

var codexPasted = regexp.MustCompile(`^\[PastedContent(\d+)chars\]$`)

// codexInInput: inInput for Codex's input.
func codexInInput(screen, text string) (waiting, known bool) {
	area, ok := codexInputArea(screen)
	if !ok {
		return false, false
	}
	marks := anchors(text)
	if len(marks) == 0 {
		return false, false
	}
	box := squeeze(area)
	for _, m := range marks {
		if strings.Contains(box, m) {
			return true, true
		}
	}
	if codexPasted.MatchString(box) {
		return false, false // (folded: what it holds is not seen)
	}
	return false, true
}
