package web

import "strings"

// Reading words off a pane that may have broken them.
//
// A pane too narrow for a line breaks it where it must, so a phrase the
// gateway looks for ("s to use this session only") can lie on two lines,
// and a line matched whole is then never found at all. Everything here
// reads the screen in a way that survives that: the words are looked for
// with the spaces and the line ends taken out.

// flatScreen is s with every space and line end taken out.
func flatScreen(s string) string {
	return strings.Map(func(r rune) rune {
		if r == ' ' || r == '\n' || r == '\r' || r == '\t' || r == 0xa0 {
			return -1
		}
		return r
	}, s)
}

// screenSays: the screen holds phrase, whole or broken over lines.
func screenSays(screen, phrase string) bool {
	return strings.Contains(flatScreen(screen), flatScreen(phrase))
}

// wrapSpan is how many lines of a pane one phrase may be broken over.
const wrapSpan = 4

// wrapAt is the line phrase begins on, looked for from line "from" down,
// found as well where the pane broke it over the lines after it. -1: the
// screen does not hold it.
func wrapAt(lines []string, phrase string, from int) int {
	want := flatScreen(phrase)
	if want == "" {
		return -1
	}
	if from < 0 {
		from = 0
	}
	for i := from; i < len(lines); i++ {
		head := flatScreen(lines[i])
		if head == "" {
			continue
		}
		acc := head
		for j := i; j < len(lines) && j < i+wrapSpan; j++ {
			if j > i {
				acc += flatScreen(lines[j])
			}
			k := strings.Index(acc, want)
			if k < 0 {
				continue
			}
			if k < len(head) {
				return i // (it begins on this line)
			}
			break // (it begins on a line below: found from there)
		}
	}
	return -1
}

// indentOf: how many columns line is indented by, and what stands there.
func indentOf(line string) (int, string) {
	t := strings.TrimLeft(line, " \t")
	return len([]rune(line)) - len([]rune(t)), strings.TrimRight(t, " \r")
}

// firstWord, lastWord: the word at either end of s ("" of none).
func firstWord(s string) string {
	if f := strings.Fields(s); len(f) > 0 {
		return f[0]
	}
	return ""
}

func lastWord(s string) string {
	if f := strings.Fields(s); len(f) > 0 {
		return f[len(f)-1]
	}
	return ""
}
