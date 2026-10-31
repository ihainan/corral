package web

import "strings"

// A screen the terminal wrapped again on a resize, before the program drew
// it anew, has a line's words cut in two, the rest at the next row's start:
// a choice of Claude Code's then read as no choice at all, and its prompt
// as none (10-31). What is shown is the screen with such rows joined again;
// what an answer is checked against stays the screen as shown (its digest:
// the host reads it so too).

// unwrapMax: how much of the joined read is gone by (its end): the screen
// is at its end, and herdr's goes back further.
const unwrapMax = 256 << 10

// unwrapScreen: the rows of visible (the screen as shown, row by row) with
// those the terminal wrapped joined, as joined (the same lines joined, by
// the terminal: herdr's recent-unwrapped, which goes back further than the
// screen; tmux's capture-pane -J) has them. From the bottom up, each line of
// joined is the rows it was cut into, letter for letter but the spaces a
// row's end lost (shown rows end with none); the top rows may be the end of
// a line begun above the screen, kept as they show. Nothing is shown that
// the screen does not: ok false (and visible, as it is) where any line does
// not match (another screen meanwhile, or a terminal that joins otherwise).
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
		k := rowsOf(lines[j], rows[:i+1])
		switch {
		case k > 0:
			out = append(out, lines[j])
			i -= k
			j--
		case endsLine(lines[j], rows[:i+1]):
			// (The top rows: the end of a line begun above the screen, as shown.)
			for r := i; r >= 0; r-- {
				out = append(out, rows[r])
			}
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

// rowsOf: how many of rows' last rows line was cut into (0: none fit).
func rowsOf(line string, rows []string) int {
	for k := 1; k <= len(rows) && k <= 400; k++ {
		if cutInto(line, rows[len(rows)-k:]) {
			return k
		}
	}
	return 0
}

// cutInto: line is rows one after another, letter for letter; between two,
// the spaces the first one's end lost. A blank row is a blank line only.
func cutInto(line string, rows []string) bool {
	if len(rows) > 1 {
		for _, r := range rows {
			if r == "" {
				return false
			}
		}
	}
	pos := 0
	for n, r := range rows {
		if n > 0 {
			for pos < len(line) && line[pos] == ' ' && !strings.HasPrefix(line[pos:], r) {
				pos++
			}
		}
		if !strings.HasPrefix(line[pos:], r) {
			return false
		}
		pos += len(r)
	}
	return pos == len(line)
}

// endsLine: rows, one after another as cutInto has them, are line's end
// (none of them blank).
func endsLine(line string, rows []string) bool {
	pos := len(line)
	for n := len(rows) - 1; n >= 0; n-- {
		r := rows[n]
		if r == "" || !strings.HasSuffix(line[:pos], r) {
			return false
		}
		pos -= len(r)
		if n > 0 {
			// (The spaces the row before lost at its end.)
			for pos > 0 && line[pos-1] == ' ' && !strings.HasSuffix(line[:pos], rows[n-1]) {
				pos--
			}
		}
	}
	return true
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
