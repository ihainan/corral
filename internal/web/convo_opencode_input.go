package web

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"regexp"
	"strings"
)

// OpenCode 2's input, as its screen shows it: at the screen's foot a box,
// its last line its bottom edge ("╹▀▀▀…"), above that the line of its
// agent and model ("┃  Build · DeepSeek V4.1 Flash OpenCode Go"), above
// that a blank line of the box's, what is typed, and a blank line again —
// each line of it begun "┃", the screen above it parted from it by a line
// that is not. A question or a permission it asks for takes the box's place
// (its choices, "enter confirm", "esc dismiss"): where that edge is not
// there, nothing is typed (an Enter would answer it).

// errOcAsks: OpenCode asks something (a question, a permission): its input
// is not there.
var errOcAsks = errors.New("it asks something: its input is not there")

// ocAsking: words of what OpenCode 2 draws in its input's place while it
// asks (its permission prompt, its questions).
var ocAsking = []string{"Permission required", "Allow once", "enter confirm", "esc dismiss", "enter submit"}

// ocIdleInput: OpenCode 2's input there, at the screen's foot, and empty
// (its placeholder at most, "Ask anything…"). errOcAsks where it asks
// something; errDraft where words are in it; errScreen where its input is
// not told (another view of its, a screen not as known).
func ocIdleInput(screen string) error {
	lines := strings.Split(strings.TrimRight(screen, "\n"), "\n")
	edge := ocEdge(lines)
	if edge < 0 {
		foot := strings.Join(lines[max(0, len(lines)-30):], "\n")
		for _, w := range ocAsking {
			if strings.Contains(foot, w) {
				return errOcAsks
			}
		}
		return errScreen
	}
	width := len([]rune(strings.TrimRight(lines[edge], " ")))
	in := func(i int) (string, bool) {
		l := []rune(lines[i])
		if len(l) > width {
			l = l[:width] // (what stands at its right, past the box: not its)
		}
		t := strings.TrimLeft(string(l), " ")
		if !strings.HasPrefix(t, "┃") {
			return "", false
		}
		return strings.TrimSpace(strings.TrimPrefix(t, "┃")), true
	}
	if edge < 4 {
		return errScreen
	}
	if model, ok := in(edge - 1); !ok || !strings.Contains(model, " · ") {
		return errScreen
	}
	// (Up from the line over the model's to the first that is not the box's:
	// the blank line under what is typed, what is typed, the blank over it.)
	top := edge - 2
	for top > 0 {
		if _, ok := in(top - 1); !ok {
			break
		}
		top--
	}
	if edge-2-top < 2 {
		return errScreen
	}
	for i := top; i <= edge-2; i++ {
		t, _ := in(i)
		if t != "" && !ocPlaceholder.MatchString(t) {
			return errDraft
		}
	}
	return nil
}

// ocPlaceholder: what OpenCode 2's input shows empty, an example of a
// question in quotes ("Ask anything… "What is the tech stack of this
// project?"", cut off in a narrow pane): not a draft that begins with those
// words (but one that begins with them and a quote: not told apart, the
// words alone read — such a draft is not one anyone types).
var ocPlaceholder = regexp.MustCompile(`^Ask anything…(?: "[^"]*"?)?$`)

// ocEdge: the line of OpenCode 2's input's bottom edge, among the last
// three that are not blank (its footer under it: the folder, the hints);
// -1 where it is not.
func ocEdge(lines []string) int {
	seen := 0
	for i := len(lines) - 1; i >= 0 && seen < 3; i-- {
		t := strings.TrimSpace(lines[i])
		if t == "" {
			continue
		}
		seen++
		if strings.HasPrefix(t, "╹▀") {
			return i
		}
	}
	return -1
}

// ocBox: OpenCode 2's input as its screen shows it — from the first of the
// lines begun "┃" above its edge down to that edge (ocEdge) —, each line
// without the blanks at its end; the screen without NULs and CRs. ok false
// where it is not there. Its digest (ocBoxDigest) is what the host checks
// again before typing: the input seen empty here, unchanged there since —
// no judging of its words on the host (see the gate "ofree", convoGated).
func ocBox(raw []byte) (box []string, ok bool) {
	s := strings.NewReplacer("\x00", "", "\r", "").Replace(string(raw))
	lines := strings.Split(s, "\n")
	for i := range lines {
		lines[i] = strings.TrimRight(lines[i], " \t")
	}
	e, seen := -1, 0
	for i := len(lines) - 1; i >= 0 && seen < 3; i-- {
		t := strings.TrimLeft(lines[i], " \t")
		if t == "" {
			continue
		}
		seen++
		if strings.HasPrefix(t, "╹▀") {
			e = i
			break
		}
	}
	if e < 0 {
		return nil, false
	}
	top := e
	for top > 0 && strings.HasPrefix(strings.TrimLeft(lines[top-1], " \t"), "┃") {
		top--
	}
	if top == e {
		return nil, false
	}
	return lines[top : e+1], true
}

func ocBoxDigest(raw []byte) (string, bool) {
	box, ok := ocBox(raw)
	if !ok {
		return "", false
	}
	sum := sha256.Sum256([]byte(strings.Join(box, "\n") + "\n"))
	return hex.EncodeToString(sum[:]), true
}

// ocBoxPipe: ocBox on the host, of the screen that comes in: its lines (none
// where it is not there). Its sha256 is ocBoxDigest's.
const ocBoxPipe = `tr -d '\000\r' | LC_ALL=C sed 's/[ 	]*$//' | LC_ALL=C awk '{ a[NR] = $0 }
END {
  s = 0; e = 0
  for (i = NR; i >= 1 && s < 3; i--) { t = a[i]; sub(/^[ 	]+/, "", t); if (t == "") continue; s++; if (index(t, "\342\225\271\342\226\200") == 1) { e = i; break } }
  if (!e) exit
  top = e
  while (top > 1) { t = a[top - 1]; sub(/^[ 	]+/, "", t); if (index(t, "\342\224\203") != 1) break; top-- }
  if (top == e) exit
  for (i = top; i <= e; i++) print a[i]
}'`
