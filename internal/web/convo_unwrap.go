package web

import "strings"

// A screen the terminal wrapped again on a resize, before the program drew
// it anew, has a line's words cut in two, the rest at the next row's start:
// a choice of Claude Code's then read as no choice at all, and its prompt
// as none (10-31). What is shown is the screen with such rows joined again;
// what an answer is checked against stays the screen as shown (its digest:
// the host reads it so too).
//
// unwrapScreen: the rows of visible (the screen as shown, row by row) with
// those the terminal wrapped joined, as joined (the same lines joined, by
// the terminal: herdr's recent_unwrapped, which goes back further than the
// screen; tmux's capture-pane -J) has them. From the bottom up, each line
// of joined is the rows it was cut into; the top row may be the end of a
// line begun above the screen, kept as it shows. ok false where they do not
// match (another screen meanwhile, or a terminal that joins otherwise):
// then visible, as it is.
func unwrapScreen(visible, joined string) (string, bool) {
	rows := screenLines(visible)
	lines := screenLines(joined)
	if len(rows) == 0 {
		return visible, false
	}
	out := make([]string, 0, len(rows))
	i, j := len(rows)-1, len(lines)-1
	for i >= 0 {
		if j < 0 {
			return visible, false
		}
		want := bareLine(lines[j])
		got, k := "", 0
		matched := false
		for i-k >= 0 {
			got = bareLine(rows[i-k]) + got
			k++
			if got == want {
				matched = true
				break
			}
			if !strings.HasSuffix(want, got) {
				break
			}
		}
		switch {
		case matched:
			out = append(out, lines[j])
			i -= k
			j--
		case i-k < 0 && strings.HasSuffix(want, got):
			// (The top rows: the end of a line begun above the screen.)
			out = append(out, strings.Join(rows[:i+1], ""))
			i = -1
		default:
			return visible, false
		}
	}
	for l, r := 0, len(out)-1; l < r; l, r = l+1, r-1 {
		out[l], out[r] = out[r], out[l]
	}
	return strings.Join(out, "\n"), true
}

// screenLines: text's lines, without the blank space at the end of each and
// the blank lines at the end of it.
func screenLines(text string) []string {
	ls := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	for k := range ls {
		ls[k] = strings.TrimRight(ls[k], " \t")
	}
	for len(ls) > 0 && ls[len(ls)-1] == "" {
		ls = ls[:len(ls)-1]
	}
	return ls
}

// bareLine: a line as compared, its spaces left out (a row may end, or the next
// begin, with the space it was cut at, shown or not).
func bareLine(s string) string { return strings.ReplaceAll(s, " ", "") }
