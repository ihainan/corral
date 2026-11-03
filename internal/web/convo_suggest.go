package web

import (
	"context"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/ihainan/corral/internal/sshx"
	"github.com/ihainan/corral/internal/term"
)

// Claude Code's suggestion of the next message, for the conversation view.
//
// Once a turn ends Claude Code suggests what to send next: greyed text in its
// input box (dim), taken with Tab. The terminal's composer reads it off the
// screen it draws (see web/src/terminal/suggestion.ts); a conversation has no
// screen of its own, so this reads the pane's, with its colours, and takes the
// same text: what follows the prompt's mark in the box in front, all of it
// dim. What the person typed there (in the default colour) is no
// suggestion, nor is the box's own example ('Try "…"'). Its log says only
// that a message was one taken ("suggestion_accepted"), not what is offered.

// POST /api/sessions/{id}/conversation/suggestion (the same body as a send,
// with no text) → {"suggestion": "…"}, "" where there is none.
func (s *Server) apiConversationSuggestion(w http.ResponseWriter, r *http.Request) {
	s.convoPress(w, r, "suggest")
}

func (s *Server) suggestion(w http.ResponseWriter, ctx context.Context, sess *term.Session, pane, socket string, outcome, reason *string, release func()) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	res, err := sshx.Exec(ctx, sess.Client(), convoColorScreenCommand(sess.Persistence, pane, socket), 512<<10, sshx.ExecOptions{Release: release})
	if err != nil {
		*outcome, *reason = "unknown", "no_screen"
		writeError(w, http.StatusBadGateway, "no_screen", "the pane's screen could not be read")
		return
	}
	*outcome = "looked"
	// (Only of the conversation's own box: with a subagent's conversation
	// shown, or the background sessions' view, its greyed text is that
	// one's placeholder ("Message @…"), and a card above it that one's —
	// agent-view.md.)
	out := map[string]any{"suggestion": ""}
	if claudeOwner(sgr.ReplaceAllString(string(res.Out), ""), false) == "main" {
		out["suggestion"] = suggestionOf(string(res.Out))
		// (Claude Code's card above its box, if one is there: read off the same screen.)
		if c := cardOf(string(res.Out)); c != nil {
			out["card"] = c
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// convoColorScreenCommand prints a pane's screen as convoScreenCommand does,
// with its colours (SGR sequences) kept. Fixed script; the mode, the
// validated pane id and the socket from discovery are its arguments.
func convoColorScreenCommand(mode, pane, socket string) string {
	script := `PATH="$HOME/.local/bin:$HOME/bin:$HOME/.cargo/bin:/opt/homebrew/bin:/usr/local/bin:$PATH"
if [ "$1" = tmux ]; then tmux -u capture-pane -p -e -t "$2" 2>/dev/null 9>&-
else HERDR_SOCKET_PATH=$3 herdr pane read "$2" --source visible --format ansi 2>/dev/null 9>&-; fi`
	return "sh -c " + sshx.QuoteShell(script) + " corral " + sshx.QuoteShell(mode) + " " + sshx.QuoteShell(pane) + " " + sshx.QuoteShell(socket)
}

// suggestionOf: the greyed text in Claude Code's input box in front, as the
// screen (with its colours) shows it; "" where there is none. The box: a line
// beginning with the prompt's mark between two rules, at the screen's foot,
// nothing chosen among under it (as stillTyped). Its text, the lines in the
// box after the mark, counts only where every character of it is dim.
func suggestionOf(screen string) string {
	lines := strings.Split(strings.TrimRight(screen, "\r\n"), "\n")
	plain := make([]string, len(lines))
	for i, l := range lines {
		plain[i] = strings.TrimRight(sgr.ReplaceAllString(l, ""), "\r")
	}
	for i := len(plain) - 1; i >= 1; i-- {
		if !strings.HasPrefix(strings.TrimSpace(plain[i]), "❯") || !pickerRule.MatchString(plain[i-1]) {
			continue
		}
		end := -1
		for j := i + 1; j < len(plain); j++ {
			if pickerRule.MatchString(plain[j]) {
				end = j
				break
			}
		}
		if end < 0 || !boxFootClear(plain[end+1:], typedFoot) {
			return ""
		}
		// (What the lines are drawn in carries on from line to line, and from
		// before the mark: a line break does not reset it.)
		var out []string
		var st sgrState
		for j := 0; j < i; j++ {
			st = st.through(lines[j])
		}
		for j := i; j < end; j++ {
			l := strings.TrimRight(lines[j], "\r")
			if j == i {
				// (After the mark: what comes before it only sets how it is drawn.)
				k := strings.Index(l, "\u276f")
				st = st.through(l[:k])
				l = l[k+len("\u276f"):]
			}
			text, ok, next := st.greyed(l)
			if !ok {
				return ""
			}
			st = next
			if text != "" {
				out = append(out, text)
			}
		}
		s := strings.Join(out, " ")
		if strings.HasPrefix(s, `Try "`) {
			return ""
		}
		if r := []rune(s); len(r) > maxSuggestion {
			return ""
		}
		return s
	}
	return ""
}

// sgrState is what text is drawn in, as far as a suggestion goes: dim.
// (Only dim: Claude Code draws its suggestion so; a grey of a theme may be
// what the person typed.)
type sgrState struct{ dim bool }

// through: the state after a stretch of the screen, its text aside.
// boxDraft: the words left in Claude Code's input box in front, as the
// screen (with its colours) shows them — typed in the terminal, or a message
// a stop interrupted, which Claude Code puts back there — their text where
// any of it is drawn as typed (not greyed), its lines as one; "" where the
// box is empty or holds greyed words only (a suggestion, its example, a
// placeholder). known false where no box in front is seen.
func boxDraft(screen string) (draft string, known bool) {
	ls, _, known := boxDraftLines(screen)
	return strings.Join(ls, " "), known
}

// boxDraftLines: boxDraft's words, a line of the box each (none where it
// holds none, or greyed ones only), and of each whether it reaches the
// box's right edge (within a few cells of its rule's width): a line that
// does may have been broken in a word by the wrap, one that does not was
// broken at a blank or by a line break of the words' own.
func boxDraftLines(screen string) (words []string, full []bool, known bool) {
	lines := strings.Split(strings.TrimRight(screen, "\r\n"), "\n")
	plain := make([]string, len(lines))
	for i, l := range lines {
		plain[i] = strings.TrimRight(sgr.ReplaceAllString(l, ""), "\r")
	}
	for i := len(plain) - 1; i >= 1; i-- {
		if !strings.HasPrefix(strings.TrimSpace(plain[i]), "❯") || !pickerRule.MatchString(plain[i-1]) {
			continue
		}
		end := -1
		for j := i + 1; j < len(plain); j++ {
			if pickerRule.MatchString(plain[j]) {
				end = j
				break
			}
		}
		if end < 0 || !boxFootClear(plain[end+1:], typedFoot) {
			return nil, nil, false
		}
		edge := dispWidth(strings.TrimRight(plain[i-1], " \t\u00a0")) - 3
		var st sgrState
		for j := 0; j < i; j++ {
			st = st.through(lines[j])
		}
		greyed := true
		for j := i; j < end; j++ {
			l, p := strings.TrimRight(lines[j], "\r"), plain[j]
			if j == i {
				k := strings.Index(l, "❯")
				st = st.through(l[:k])
				l = l[k+len("❯"):]
				p = p[strings.Index(p, "❯")+len("❯"):]
			}
			if _, ok, next := st.greyed(l); ok {
				st = next
			} else {
				greyed = false
				st = st.through(l)
			}
			if t := strings.Trim(p, " \t\u00a0"); t != "" {
				words = append(words, t)
				full = append(full, dispWidth(strings.TrimRight(plain[j], " \t\u00a0")) >= edge)
			}
		}
		if greyed || len(words) == 0 {
			return nil, nil, true
		}
		return words, full, true
	}
	return nil, nil, false
}

// dispWidth: the cells a line takes on a terminal: the wide characters
// (CJK, full-width forms, most emoji) two, any other one.
func dispWidth(s string) int {
	n := 0
	for _, r := range s {
		switch {
		case r >= 0x1100 && r <= 0x115F, r >= 0x2E80 && r <= 0xA4CF && r != 0x303F, r >= 0xAC00 && r <= 0xD7A3,
			r >= 0xF900 && r <= 0xFAFF, r >= 0xFE30 && r <= 0xFE4F, r >= 0xFF00 && r <= 0xFF60, r >= 0xFFE0 && r <= 0xFFE6,
			r >= 0x1F300 && r <= 0x1F64F, r >= 0x1F900 && r <= 0x1F9FF, r >= 0x20000 && r <= 0x3FFFD:
			n += 2
		default:
			n++
		}
	}
	return n
}

func (st sgrState) through(s string) sgrState {
	for _, m := range sgr.FindAllString(s, -1) {
		st = st.apply(m[2 : len(m)-1])
	}
	return st
}

// greyed: a line's text where every character of it (spaces aside) is dim,
// and the state after it; ok false where any is not.
func (st sgrState) greyed(l string) (string, bool, sgrState) {
	var b strings.Builder
	for len(l) > 0 {
		m := sgr.FindStringIndex(l)
		if m != nil && m[0] == 0 {
			st = st.apply(l[2 : m[1]-1])
			l = l[m[1]:]
			continue
		}
		next := len(l)
		if m != nil {
			next = m[0]
		}
		for _, r := range l[:next] {
			if r == ' ' || r == '\u00a0' {
				b.WriteRune(' ')
				continue
			}
			if r < 0x20 {
				continue // (a stray control, a carriage return)
			}
			if !st.dim {
				return "", false, st
			}
			b.WriteRune(r)
		}
		l = l[next:]
	}
	return strings.TrimSpace(b.String()), true, st
}

// apply: the state as an SGR sequence's parameters leave it (the extended
// colours' own parameters skipped, not taken for attributes).
func (st sgrState) apply(params string) sgrState {
	ps := strings.Split(params, ";")
	if params == "" {
		ps = []string{"0"}
	}
	for i := 0; i < len(ps); i++ {
		n, _ := strconv.Atoi(ps[i])
		switch {
		case n == 0:
			st.dim = false
		case n == 2:
			st.dim = true
		case n == 22:
			st.dim = false
		case (n == 38 || n == 48 || n == 58) && i+1 < len(ps) && ps[i+1] == "5":
			i += 2
		case (n == 38 || n == 48 || n == 58) && i+1 < len(ps) && ps[i+1] == "2":
			i += 4
		}
	}
	return st
}

// (An SGR sequence; how long a suggestion may be: Claude Code's are a line.)
var sgr = regexp.MustCompile(`\x1b\[[0-9;]*m`)

const maxSuggestion = 500
