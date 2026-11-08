package web

import (
	"errors"
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
		// (Its placeholder, "Ask anything… …": not a draft that begins so.)
		if t != "" && !strings.HasPrefix(t, "Ask anything…") {
			return errDraft
		}
	}
	return nil
}

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

// ocInputAwk: ocIdleInput on the host, the pane's screen read (LC_ALL=C):
// "empty" its input there and empty, "draft" words in it, nothing where it is
// not there (see the gate "ofree", convoGated). What stands at the right of
// a line of it, eight blanks or more off, is not its (a sidebar's).
const ocInputAwk = `{ a[NR] = $0 }
END {
  s = 0; e = 0
  for (i = NR; i >= 1 && s < 3; i--) { t = a[i]; sub(/^[ \t]+/, "", t); if (t == "") continue; s++; if (index(t, "\342\225\271\342\226\200") == 1) { e = i; break } }
  if (e < 5) exit
  m = a[e - 1]; sub(/^[ \t]+/, "", m)
  if (index(m, "\342\224\203") != 1 || index(m, " \302\267 ") == 0) exit
  top = e - 2
  while (top > 1) { t = a[top - 1]; sub(/^[ \t]+/, "", t); if (index(t, "\342\224\203") != 1) break; top-- }
  if (e - 2 - top < 2) exit
  for (i = top; i <= e - 2; i++) {
    t = a[i]; sub(/^[ \t]+/, "", t); t = substr(t, 4); sub(/        +.*$/, "", t); sub(/^[ \t]+/, "", t); sub(/[ \t]+$/, "", t)
    if (t != "" && index(t, "Ask anything\342\200\246") != 1) { print "draft"; exit }
  }
  print "empty"
}`
