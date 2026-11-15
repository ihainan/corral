package web

import (
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// codexIdleInput: Codex at rest, its input empty: nothing says it works
// ("esc to interrupt"), its input is where Codex draws it (codexInputSpan)
// with a footer under it, and it holds its placeholder only ("Ask Codex to
// do anything", cut short in a narrow pane). Any other words there, or none
// (its placeholder is drawn whenever it is empty), are taken for a draft: a
// command is then refused, not typed onto it. A footer is wanted here,
// before anything is typed, as a sign the screen is Codex's at all (a
// shell's prompt may be "› " too): its own hints ("? for shortcuts") are
// always there under an empty input.
func codexIdleInput(screen string) error {
	if strings.Contains(screen, "esc to interrupt") {
		return errBusyAgent
	}
	lines := strings.Split(screen, "\n")
	at, foot, footed, ok := codexInputSpan(lines)
	if !ok || !footed {
		return errScreen
	}
	if !codexEmptyInput(strings.TrimSpace(strings.TrimPrefix(lines[at], "›"))) {
		return errDraft
	}
	for _, l := range lines[at+1 : foot] {
		if strings.TrimSpace(l) != "" {
			return errDraft
		}
	}
	return nil
}

// codexPlaceholderText: what Codex's input shows when empty.
const codexPlaceholderText = "Ask Codex to do anything"

// codexEmptyInput: in is Codex's placeholder, or the start of it as a
// narrow pane cuts it short (enough of it to tell).
func codexEmptyInput(in string) bool {
	return in == codexPlaceholderText || len([]rune(in)) >= 10 && strings.HasPrefix(codexPlaceholderText, in)
}

// codexInputSpan: where Codex's input is on its screen. at: its first line,
// the last one beginning "› " (the person's messages above begin so too:
// the last is the input; its own further lines are indented, never so).
// Under it, down to the screen's end, every line is blank or indented two
// spaces (its lines, its footer); anything else and the bottom is not its
// input (a dialog, a menu whose choice is marked "› 1.", a pager): !ok.
// foot: where its footer begins — the block after the last blank line
// under it, as Codex draws its footer one blank line under its input, a
// block of a few lines at most (footed); where no blank line is under it,
// or the block is longer than any footer, it has none shown (a status line
// of items with no value yet, its hints gone while it holds words: 0.167,
// 11-15), and foot is the screen's end.
// (Its footer is told by where it stands, not by its words: a status line
// is the person's to set, of any of some twenty items, or none — a model,
// a thread's id, its limits, "100% context left" at the right alone.)
func codexInputSpan(lines []string) (at, foot int, footed, ok bool) {
	end := len(lines)
	for end > 0 && strings.TrimSpace(lines[end-1]) == "" {
		end--
	}
	at = -1
	for i := end - 1; i >= 0 && end-i <= codexInputMax+codexFootMax+1; i-- {
		if strings.HasPrefix(lines[i], "› ") || strings.TrimRight(lines[i], " ") == "›" {
			at = i
			break
		}
	}
	if at < 0 || codexMenuChoice.MatchString(lines[at]) && at+1 < len(lines) && codexMenuNext.MatchString(lines[at+1]) {
		return 0, 0, false, false // (a menu: its choice marked, the next under it — a draft "1. …" is not one: review r2a-3)
	}
	blank := -1
	for i := at + 1; i < end; i++ {
		switch {
		case strings.TrimSpace(lines[i]) == "":
			blank = i
		case !strings.HasPrefix(lines[i], "  "):
			return 0, 0, false, false
		}
	}
	if blank < 0 || end-blank-1 > codexFootMax {
		return at, end, false, true
	}
	return at, blank + 1, true, true
}

// codexFootMax: the most lines Codex's footer runs to, wrapped.
const codexFootMax = 6

// codexFooter: a line of Codex's own footer as it is known: its model and
// folder, its hints ("? for shortcuts", "esc again to edit previous
// message"), its notes ("⚠ 1 warning · f2 to view"), each the whole line but
// for what stands at its right. Not what finds its footer (codexInputSpan:
// where it stands): what tells a block under the input after a blank line
// for its footer surely, not the end of a draft that looks like one.
var codexFooter = regexp.MustCompile(`^(?:[A-Za-z][\w./-]*(?: (?:default|minimal|low|medium|high|xhigh|max|ultra))?(?: [A-Z][A-Za-z]+)? · (?:~|/).*|Context \d+% (?:left|used)(?: · .*)?|\d+% context (?:left|used)|(?:← for agents · )?\? for shortcuts|esc again to edit previous message|ctrl \+ c again to quit|⚠ \d+ warnings? · f2 to view)(?:\s{2,}\S.*)?$`)

// codexInputArea: what Codex's input holds (codexInputSpan): area, its
// lines down to its footer; rest, the block taken for its footer where its
// words are not a known footer's (codexFooter) — it may be the end of a
// draft, words after a blank line, with no footer shown under it: a
// caller who knows what should be there tries both. ok: its input told
// apart. Its placeholder, when empty, is what it holds.
func codexInputArea(screen string) (area, rest string, ok bool) {
	lines := strings.Split(strings.TrimRight(screen, "\n"), "\n")
	at, foot, footed, ok := codexInputSpan(lines)
	if !ok {
		return "", "", false
	}
	in := append([]string{strings.TrimPrefix(strings.TrimPrefix(lines[at], "›"), " ")}, lines[at+1:foot]...)
	area = strings.TrimSpace(strings.Join(in, "\n"))
	if footed {
		known := false
		for _, l := range lines[foot:] {
			known = known || codexFooter.MatchString(strings.TrimSpace(l))
		}
		if !known {
			rest = strings.TrimSpace(strings.Join(lines[foot:], "\n"))
		}
	}
	return area, rest, true
}

// codexOneLineFoot: a block under Codex's input, after a blank line, that
// may be its status line of the person's own: one line (Codex cuts it
// short, "…", rather than wrap it). More: words of the input's.
func codexOneLineFoot(rest string) bool {
	return rest != "" && !strings.Contains(rest, "\n")
}

// (A menu's choice, marked: "› 1. Yes, proceed"; its next under it, "  2. …".)
var (
	codexMenuChoice = regexp.MustCompile(`^›\s*\d+\.\s`)
	codexMenuNext   = regexp.MustCompile(`^\s+\d+\.\s`)
)

// codexInputMax: the most lines Codex's input runs to above its footer
// (it grows with what it holds, then scrolls).
const codexInputMax = 40

// codexTypedIn: typedIn for Codex's input — the message there from its
// start (whole: all of it and nothing else). A long paste Codex folds to
// "[Pasted Content N chars]": the message there where N is its length,
// never whole (what it holds cannot be seen).
func codexTypedIn(screen, text string, whole bool) bool {
	area, rest, ok := codexInputArea(screen)
	if !ok {
		return false
	}
	// (Under it, an unknown footer or the end of the message itself: either
	// — a footer of one line only: more is words of the input's, and the
	// message is not there whole without them: review r2a-4.)
	if rest != "" && !codexOneLineFoot(rest) {
		return codexBoxHas(area+"\n"+rest, text, whole)
	}
	return codexBoxHas(area, text, whole) || rest != "" && codexBoxHas(area+"\n"+rest, text, whole)
}

// codexBoxHas: codexTypedIn's reading of what its input holds.
func codexBoxHas(area, text string, whole bool) bool {
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
	area, rest, ok := codexInputArea(screen)
	if !ok {
		return false, false
	}
	area += "\n" + rest // (an unknown footer's words or the end of the message: looked in too)
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

// codexHasDraft: Codex's input holds words (typed in the terminal), whether
// it works or not (a message sent while it works goes in as it works).
// screen: with its colours (convoColorScreenCommand) — its placeholder
// ("Ask Codex to do anything", cut short in a narrow pane) is dim, words
// typed are not.
func codexHasDraft(screen string) bool {
	plain := sgr.ReplaceAllString(screen, "")
	raw := strings.Split(screen, "\n")
	lines := strings.Split(plain, "\n")
	if len(raw) != len(lines) {
		return true
	}
	at, foot, _, ok := codexInputSpan(lines)
	if !ok {
		return false // (its input not told apart: left to the send's own reading)
	}
	for _, l := range lines[at+1 : foot] {
		if strings.TrimSpace(l) != "" {
			return true // (more lines than its placeholder's one)
		}
	}
	return !allDim(raw[at], "› ")
}

// allDim: what line (with its colours) shows after its first prefix is all
// dim (SGR 2) — a placeholder's.
func allDim(line, prefix string) bool {
	dim, seen, past := false, "", false
	for len(line) > 0 {
		if loc := sgr.FindStringIndex(line); loc != nil && loc[0] == 0 {
			for _, p := range strings.Split(strings.TrimSuffix(strings.TrimPrefix(line[:loc[1]], "\x1b["), "m"), ";") {
				switch p {
				case "2":
					dim = true
				case "", "0", "22":
					dim = false
				}
			}
			line = line[loc[1]:]
			continue
		}
		r, n := utf8.DecodeRuneInString(line)
		line = line[n:]
		if !past {
			seen += string(r)
			if strings.HasSuffix(seen, prefix) {
				past = true
			}
			continue
		}
		if !unicode.IsSpace(r) && r != '\r' && !dim {
			return false
		}
	}
	return past
}
