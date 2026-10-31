package web

import (
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
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
		// shortcuts", the block at the screen's end —: any words are the
		// input's, a draft, blank lines among them or not, words like a
		// footer's among them too — review502.)
		foot := codexFootStart(lines)
		if foot < i {
			return errScreen
		}
		for _, l := range lines[i+1 : foot] {
			if strings.TrimSpace(l) != "" {
				return errDraft
			}
		}
		return nil
	}
	return errScreen
}

// codexFootStart: where Codex's footer begins — as Codex draws it under
// its input, after a blank line: the lines at the screen's end (blank ones
// at the very end aside) with no blank line among them, one of them a
// footer's at least (codexFooter; the others a footer's lines wrapped in a
// narrow pane — review503); -1 where the screen does not end so. (Lines
// like a footer's above that blank line are the input's, a draft's.)
func codexFootStart(lines []string) int {
	end := len(lines)
	for end > 0 && strings.TrimSpace(lines[end-1]) == "" {
		end--
	}
	at, seen := end, false
	for at > 0 && strings.TrimSpace(lines[at-1]) != "" {
		seen = seen || codexFooter.MatchString(strings.TrimSpace(lines[at-1]))
		at--
	}
	if !seen || at == 0 || at == end || end-at > codexFootMax {
		return -1
	}
	return at
}

// codexFootMax: the most lines Codex's footer runs to, wrapped.
const codexFootMax = 6

// codexFooter: a line of Codex's footer under its input: its model and
// folder ("gpt-mock default · ~/cxreal", the folder's name spaces and all;
// or "Context 100% left", a status line set to that; one set otherwise is
// not known, its input taken for a draft: commands refused, nothing lost),
// its hints ("? for shortcuts", "esc again to edit previous message"), its
// notes ("⚠ 1 warning · f2 to view"), each the whole line but for what
// stands at its right, two spaces off or more.
var codexFooter = regexp.MustCompile(`^(?:[A-Za-z][\w./-]*(?: (?:default|minimal|low|medium|high|xhigh|max))? · (?:~|/).*|Context \d+% (?:left|used)(?: · .*)?|\? for shortcuts|esc again to edit previous message|ctrl \+ c again to quit|⚠ \d+ warnings? · f2 to view)(?:\s{2,}\S.*)?$`)

// codexInputArea: what Codex's input holds — its last line beginning
// "› " above its footer (the person's messages above begin so too: the
// last is the input), and the lines under it down to that footer (the
// block at the screen's end: codexFootStart); ok only where that footer is
// there, close under it (else what is at the bottom is not told: a
// dialog, a menu whose choice is marked "› 1."). Its placeholder, when
// empty, is what it holds.
func codexInputArea(screen string) (area string, ok bool) {
	lines := strings.Split(strings.TrimRight(screen, "\n"), "\n")
	foot := codexFootStart(lines)
	if foot < 0 {
		return "", false
	}
	for i := foot - 1; i >= 0 && foot-i <= codexInputMax; i-- {
		if !strings.HasPrefix(lines[i], "› ") && lines[i] != "›" {
			continue
		}
		if codexMenuChoice.MatchString(lines[i]) {
			return "", false
		}
		in := append([]string{strings.TrimPrefix(strings.TrimPrefix(lines[i], "›"), " ")}, lines[i+1:foot]...)
		return strings.TrimSpace(strings.Join(in, "\n")), true
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
	// (Its pictures' paths, each pasted on its own, shown "[Image #N]": as
	// Claude Code's box shows them, wholeOf and typedImage read them.)
	if whole {
		return wholeOf(text).MatchString(box)
	}
	if m := typedImage.FindStringIndex(box); m != nil {
		if p := picturePath(want); p != nil {
			box, want = box[m[1]:], want[p[1]:]
			if want == "" {
				return box == ""
			}
		}
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

// codexPasteParts: a message for Codex cut where a picture's path stands
// alone in it (picturesIn: quoted, or between spaces, as the page puts an
// upload's path in the draft): the parts' lengths in bytes, in turn, each
// path a part of its own — pasted on its own, Codex takes it for a picture
// ("[Image #1]"; in the middle of words, it is words). nil: none in it.
func codexPasteParts(text string) []int {
	var parts []int
	at := 0
	for _, m := range picturesIn.FindAllStringSubmatchIndex(text, -1) {
		if r, _ := utf8.DecodeRuneInString(text[m[3]:]); m[3] < len(text) && !unicode.IsSpace(r) {
			continue // (not standing alone: a word goes on after it)
		}
		if m[2] > at {
			parts = append(parts, m[2]-at)
		}
		parts = append(parts, m[3]-m[2])
		at = m[3]
	}
	if len(parts) == 0 {
		return nil
	}
	if at < len(text) {
		parts = append(parts, len(text)-at)
	}
	return parts
}
