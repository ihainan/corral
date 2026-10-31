package web

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/ihainan/corral/internal/sshx"
	"github.com/ihainan/corral/internal/term"
)

// Looking for a message that was typed into a pane and has not turned up
// in the agent's log.
//
// A send types the message and presses Enter, and the page then waits for
// the log to say the agent took it in. When it never does, the page could
// only say so: whether the words were still sitting in the input box, or
// had been taken and the log was merely behind, nobody could tell
// afterwards. This reads the pane's screen at that moment, which answers
// it, and leaves a line in the audit saying which it was — the screen
// itself is shown to the person and kept nowhere.

// sentLook is how much of the pane's screen is kept to show the person
// (its end: the input is at the bottom).
const sentLook = 2000

// POST /api/sessions/{id}/conversation/sent-missing (the same body as a
// send, with the message that has not turned up)
//
// {"screen":…,"waiting":true,"known":true} — the words are in the pane's
// input: they were typed and not taken in. waiting false: the input no
// longer holds them, so they were taken and the log is behind. known
// false: the screen does not say either way, and nothing is claimed.
func (s *Server) apiConversationSentMissing(w http.ResponseWriter, r *http.Request) {
	s.convoPress(w, r, "sent_missing")
}

// sentMissing reads the pane's screen and says whether the message is
// still waiting in its input. text is what was sent.
func (s *Server) sentMissing(w http.ResponseWriter, ctx context.Context, sess *term.Session, pane, socket, agent, text string, outcome, reason *string, release func()) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	res, err := sshx.Exec(ctx, sess.Client(), convoScreenCommand(sess.Persistence, pane, socket), 256<<10, sshx.ExecOptions{Release: release})
	if err != nil {
		*outcome, *reason = "unknown", "no_screen"
		writeError(w, http.StatusBadGateway, "no_screen", "the pane's screen could not be read")
		return
	}
	screen := string(res.Out)
	waiting, known := inInput(screen, text)
	if agent == "codex" {
		waiting, known = codexInInput(screen, text)
	}
	*outcome = "looked"
	switch {
	case !known:
		*reason = "unsure"
	case waiting:
		*reason = "waiting"
	default:
		*reason = "taken"
	}
	// (Its end, where the input is: a screen longer than that is cut at
	// the front, not the back.)
	writeJSON(w, http.StatusOK, map[string]any{"screen": lastOf(screen, sentLook), "waiting": waiting, "known": known})
}

// enterAgain: a message just typed into Claude Code (screen: its pane a
// moment after the Enter) still in its input box, Enter let go of: Enter
// again, at most twice, the screen read anew after each. Not where the
// screen does not say (nothing is pressed on a guess), and not once the
// box no longer holds it: taken in, or queued while the agent works. Left
// there, the words stayed in the box, and the next message typed went in
// after them, the two then sent as one. (ended: the send's turn, held until
// these have ended on the host too.)
// later: a context for the look a moment on, tied to the person's login
// (its end, a sign-out or revocation calls it off), nil where it is over.
func (s *Server) enterAgain(ctx context.Context, sess *term.Session, pane, socket, agent string, pid, start int64, sid, text, screen string, ended *sync.WaitGroup, later func(d time.Duration) (context.Context, func())) {
	still, whole := stillTyped, typedWhole
	if agent == "codex" { // (its input drawn its own way: codexInputArea)
		still = func(screen, text string) bool { return codexTypedIn(screen, text, false) }
		whole = func(screen, text string) bool { return codexTypedIn(screen, text, true) }
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	enter := []string{"enter"}
	if sess.Persistence == "tmux" {
		enter = []string{"Enter"}
	}
	press := func(ctx context.Context) bool {
		slog.Info("conversation: message still in the input, Enter again", "session", sess.ID, "pane", pane)
		ended.Add(1)
		// (Not while it waits: the Enter would be a question's.)
		res, err := sshx.Exec(ctx, sess.Client(), convoGatedCommand("free", sess.Persistence, sess.TmuxSession, pane, socket, pid, start, sid, 0, enter, sendLines), sendOut,
			sshx.ExecOptions{Stdin: strings.NewReader(""), Release: ended.Done})
		if err != nil {
			return false
		}
		screen = string(res.Out)
		return true
	}
	for range 2 {
		if !still(screen, text) || !press(ctx) {
			break
		}
	}
	// A moment on, looked at once more: a message typed as the agent was
	// ending its turn (its hooks running, a card of its own being drawn)
	// was not yet in its box when looked at right after the Enter, and the
	// Enter was lost; seen there now, Enter once more. (Beside the send's
	// answer, not before it; the send's turn held until it is done.)
	// (Only the message whole: by then the box could hold another typed in
	// the terminal that begins as it does.)
	lctx, done := later(enterLater + 10*time.Second)
	if lctx == nil {
		return // (signed out meanwhile, or the login over)
	}
	ended.Add(1)
	go func() {
		defer ended.Done()
		defer done()
		select {
		case <-lctx.Done():
			return
		case <-time.After(enterLater):
		}
		ended.Add(1)
		res, err := sshx.Exec(lctx, sess.Client(), convoScreenCommand(sess.Persistence, pane, socket), 256<<10, sshx.ExecOptions{Release: ended.Done})
		if err == nil && whole(string(res.Out), text) {
			press(lctx)
		}
	}()
}

// enterLater: how long after a send its input box is looked at once more.
const enterLater = 2500 * time.Millisecond

// sentEnter: Enter pressed for a message the person was told is still in
// the input box — only where the screen, read now, shows it there (the box
// in front, holding it from its start: see stillTyped); pressed tells.
func (s *Server) sentEnter(w http.ResponseWriter, ctx context.Context, sess *term.Session, pane, socket, agent string, pid, start int64, sid, text string, outcome, reason *string, release func()) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	var held sync.WaitGroup
	held.Add(1)
	defer func() {
		go func() {
			held.Wait()
			release()
		}()
	}()
	res, err := sshx.Exec(ctx, sess.Client(), convoScreenCommand(sess.Persistence, pane, socket), 256<<10, sshx.ExecOptions{Release: held.Done})
	if err != nil {
		*outcome, *reason = "unknown", "no_screen"
		writeError(w, http.StatusBadGateway, "no_screen", "the pane's screen could not be read")
		return
	}
	whole := typedWhole
	if agent == "codex" {
		whole = func(screen, text string) bool { return codexTypedIn(screen, text, true) }
	}
	if !whole(string(res.Out), text) {
		*outcome, *reason = "refused", "not_in_input"
		writeJSON(w, http.StatusOK, map[string]any{"pressed": false})
		return
	}
	enter := []string{"enter"}
	if sess.Persistence == "tmux" {
		enter = []string{"Enter"}
	}
	held.Add(1)
	_, err = sshx.Exec(ctx, sess.Client(), convoGatedCommand("free", sess.Persistence, sess.TmuxSession, pane, socket, pid, start, sid, 0, enter, sendLines), sendOut,
		sshx.ExecOptions{Stdin: strings.NewReader(""), Release: held.Done})
	var exitErr *sshx.ExitError
	switch {
	case err == nil:
	case errors.Is(err, sshx.ErrBusy), errors.As(err, &exitErr) && exitErr.Status == 7:
		*outcome, *reason = "refused", "busy" // (nothing pressed)
		writeError(w, http.StatusConflict, "busy", "the pane is busy with another request: try again")
		return
	case errors.As(err, &exitErr) && exitErr.Status == 4:
		*outcome, *reason = "refused", "send_failed" // (nothing pressed)
		writeError(w, http.StatusBadGateway, "send_failed", "nothing could be sent to the pane")
		return
	case errors.As(err, &exitErr) && exitErr.Status == 3:
		*outcome, *reason = "refused", "agent_changed"
		writeError(w, http.StatusConflict, "agent_changed", "that pane no longer runs this agent")
		return
	case errors.As(err, &exitErr) && exitErr.Status == 9:
		*outcome, *reason = "refused", "agent_waiting"
		writeError(w, http.StatusConflict, "agent_waiting", "it waits for an answer, or has a dialog open: answer it first")
		return
	default:
		*outcome, *reason = "unknown", "no_answer"
		writeError(w, http.StatusBadGateway, "no_answer", "the host did not answer")
		return
	}
	*outcome = "pressed"
	writeJSON(w, http.StatusOK, map[string]any{"pressed": true})
}

// POST /api/sessions/{id}/conversation/sent-enter (the same body as a
// send, with the message still in the input box): Enter, where it is.
func (s *Server) apiConversationSentEnter(w http.ResponseWriter, r *http.Request) {
	s.convoPress(w, r, "sent_enter")
}

// stillTyped: what Enter would now be pressed on is the message itself —
// stricter than inInput, which says what became of a message, as Enter is
// to be pressed on it: the input box is the one in front (at the foot of
// the screen, only the status lines under it: not one of the thread above,
// as when a permission dialog has taken the box's place) and holds the
// message from its start (not a part of it, nor the box's placeholder).
func stillTyped(screen, text string) bool { return typedIn(screen, text, false) }

// typedWhole: stillTyped, the box holding all of the message and nothing
// else — so that Enter, pressed for an earlier message, does not send
// another that begins as it does (a paste folded to "[Pasted text #1 …]"
// cannot be told whole: not taken).
func typedWhole(screen, text string) bool { return typedIn(screen, text, true) }

func typedIn(screen, text string, whole bool) bool {
	lines := strings.Split(strings.TrimRight(screen, "\n"), "\n")
	for i := len(lines) - 1; i >= 1; i-- {
		if !strings.HasPrefix(strings.TrimSpace(lines[i]), "\u276f") || !pickerRule.MatchString(lines[i-1]) {
			continue
		}
		end := -1
		for j := i + 1; j < len(lines); j++ {
			if pickerRule.MatchString(lines[j]) {
				end = j
				break
			}
		}
		if end < 0 || len(lines)-end-1 > typedFoot {
			return false
		}
		// (Under it, status lines only: a choice there, a dialog's, is in front.)
		for _, l := range lines[end+1:] {
			if strings.Contains(l, "\u276f") || typedChoice.MatchString(l) {
				return false
			}
		}
		box := strings.TrimPrefix(squeeze(strings.Join(lines[i:end], "\n")), "\u276f")
		want := squeeze(text)
		if len([]rune(want)) < sentMark {
			return false
		}
		// What Claude Code shows of a paste at its start: a long one folded to
		// "[Pasted text #1 +40 lines]" (as many lines as the message has); a
		// path to a picture to "[Image #1]", the rest after it.
		if m := typedPasted.FindStringSubmatch(box); m != nil {
			if whole {
				return false
			}
			// (Its newlines or its lines, as Claude Code counts them.)
			n, _ := strconv.Atoi(m[1])
			nl := strings.Count(strings.TrimRight(text, "\n"), "\n")
			return nl > 0 && (n == nl || n == nl+1)
		}
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
	return false
}

// (A dialog's choice, "1. Yes"; Claude Code's placeholders for a long paste
// and a picture at the box's start, spaces squeezed out; a path to a picture
// at the message's start.)
var (
	typedChoice = regexp.MustCompile(`^\s*\d+\.\s`)
	typedPasted = regexp.MustCompile(`^\[Pastedtext#\d+\+(\d+)lines\]$`)
	typedImage  = regexp.MustCompile(`^\[Image#\d+\]`)
	typedPath   = []*regexp.Regexp{
		regexp.MustCompile(`(?i)^'[^']*?\.(?:png|jpe?g|gif|webp)'`),
		regexp.MustCompile(`(?i)^"[^"]*?\.(?:png|jpe?g|gif|webp)"`),
		regexp.MustCompile(`(?i)^[^'"\s]*?\.(?:png|jpe?g|gif|webp)`),
	}
)

// wholeOf: the message as Claude Code's box shows all of it, spaces
// squeezed out: each path to a picture in it (quoted, or standing alone
// between spaces) as "[Image #N]" or as it is, the rest as it is.
func wholeOf(text string) *regexp.Regexp {
	var b strings.Builder
	b.WriteString("^")
	at := 0
	for _, m := range picturesIn.FindAllStringSubmatchIndex(text, -1) {
		// (Standing alone: a space, or the end, after it too. The space is
		// not taken by the match, so the next path may begin after it.)
		if r, _ := utf8.DecodeRuneInString(text[m[3]:]); m[3] < len(text) && !unicode.IsSpace(r) {
			continue
		}
		b.WriteString(regexp.QuoteMeta(squeeze(text[at:m[2]])))
		// (As Claude Code shows a picture it found, or as typed where it did not.)
		b.WriteString(`(?:\[Image#\d+\]|` + regexp.QuoteMeta(squeeze(text[m[2]:m[3]])) + `)`)
		at = m[3]
	}
	b.WriteString(regexp.QuoteMeta(squeeze(text[at:])))
	b.WriteString("$")
	return regexp.MustCompile(b.String())
}

// (A path to a picture anywhere in a message: quoted, or alone between spaces.)
var picturesIn = regexp.MustCompile(`(?i)(?:^|[\s\x{00A0}])('[^']*?\.(?:png|jpe?g|gif|webp)'|"[^"]*?\.(?:png|jpe?g|gif|webp)"|[^'"\s\x{00A0}]+\.(?:png|jpe?g|gif|webp))`)

// picturePath: where a path to a picture at a message's start ends (its
// quotes, a pair of the one kind, with it; one not quoted has none in it).
func picturePath(s string) []int {
	for _, re := range typedPath {
		if m := re.FindStringIndex(s); m != nil {
			return m
		}
	}
	return nil
}

// typedFoot: the lines under Claude Code's input box (its status lines);
// typedHead: how much of a message's start its box must show.
const (
	typedFoot = 10
	typedHead = 24
)

// inInput: the message is in the pane's input box, as the pane shows it.
//
// Only the input box, not the whole screen: a message that was taken in
// is written into the thread above, and finding it there would say the
// opposite of the truth. known false where nothing can be told — the
// screen shows no input box, or the message has too little of its own in
// it to tell from anything else.
func inInput(screen, text string) (waiting, known bool) {
	area := squeeze(inputArea(screen))
	if area == "" {
		return false, false
	}
	marks := anchors(text)
	if len(marks) == 0 {
		return false, false
	}
	for _, m := range marks {
		if strings.Contains(area, m) {
			return true, true
		}
	}
	return false, true
}

// inputArea: what the pane's input box holds — the lines between the rule
// above the line beginning with ❯ and the rule under it (Claude Code draws
// it so; the last such box on the screen is the one being typed into). ""
// where the screen shows none.
func inputArea(screen string) string {
	lines := strings.Split(screen, "\n")
	for i := len(lines) - 1; i >= 1; i-- {
		if !strings.HasPrefix(strings.TrimSpace(lines[i]), "\u276f") || !pickerRule.MatchString(lines[i-1]) {
			continue
		}
		end := len(lines)
		for j := i + 1; j < len(lines); j++ {
			if pickerRule.MatchString(lines[j]) {
				end = j
				break
			}
		}
		return strings.Join(lines[i:end], "\n")
	}
	return ""
}

// anchors: the pieces of the message looked for in the input, each long
// enough to tell the message from anything else that might be there. Its
// longest unbroken run (a path, a command) is what a wrapped line keeps
// whole; its end is what an input scrolled to the bottom shows.
func anchors(text string) []string {
	var out []string
	add := func(s string) {
		if r := []rune(squeeze(s)); len(r) >= sentMark {
			out = append(out, string(r))
		}
	}
	add(longestRun(text))
	if r := []rune(squeeze(text)); len(r) > sentMark {
		add(string(r[max(0, len(r)-sentTail):]))
	}
	return out
}

// sentMark: how much of the message must be found to call it the message;
// sentTail: how much of its end is looked for (both in characters).
const (
	sentMark = 6
	sentTail = 24
)

// longestRun: the longest stretch of the message without a space or a line
// break (a word, a path, a command), cut on a character where it is long.
func longestRun(text string) string {
	best := ""
	for _, f := range strings.FieldsFunc(text, func(r rune) bool { return r == ' ' || r == '\n' || r == '\r' || r == '\t' }) {
		if len(f) > len(best) {
			best = f
		}
	}
	if r := []rune(best); len(r) > 120 {
		best = string(r[:120])
	}
	return best
}

// lastOf: the end of s, at most n characters of it.
func lastOf(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return "…" + string(r[len(r)-n:])
}

func squeeze(s string) string {
	return strings.Map(func(r rune) rune {
		if r == ' ' || r == '\n' || r == '\r' || r == '\t' || r == 0xa0 {
			return -1
		}
		return r
	}, s)
}
