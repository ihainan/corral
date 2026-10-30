package web

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/ihainan/corral/internal/sshx"
	"github.com/ihainan/corral/internal/term"
)

// Whether a Codex in a tmux pane waits for an answer (an approval, a
// question): its log does not say (plans/5d2-tmux-codex.md); Codex 0.164
// titles its terminal "[ ! ] Action Required | <folder>", blinking with
// "[ . ] …", while it waits, and only then. The title is read when the
// log says it works or says nothing (a long log's tail read first), at
// most each convoTitleTTL for a pane, and read anew after keys or text
// went to it. A read that fails says it does not wait.

const convoTitleTTL = 2 * time.Second

// codexTitleWaits: the title is one of a Codex waiting for an answer.
func codexTitleWaits(title string) bool {
	return strings.HasPrefix(title, "[ ! ] Action Required") || strings.HasPrefix(title, "[ . ] Action Required")
}

// codexWaits says whether st's Codex waits, its title read if what was
// read of it is older than convoTitleTTL, of another process, or from
// before keys last went to its pane. (Under st.mu.)
func (s *Server) codexWaits(ctx context.Context, sess *term.Session, st *convoState) bool {
	b := st.bind
	who := strings.Join([]string{b.Pane, strconv.FormatInt(b.PID, 10), strconv.FormatInt(b.Start, 10), b.Session}, "\x00")
	pressed, _ := s.cxPressed.Load(s.convoReplyKey(sess, b.Pane))
	at, _ := pressed.(time.Time)
	if st.cxTitleFor == who && time.Since(st.cxTitleAt) < convoTitleTTL && st.cxTitleAt.After(at) {
		return st.cxTitleWait
	}
	began := time.Now()
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	res, err := sshx.Exec(ctx, sess.Client(), codexTitleCommand(b.Pane), 4<<10, sshx.ExecOptions{})
	waits := err == nil && codexTitleWaits(strings.TrimRight(string(res.Out), "\n"))
	st.cxTitleAt, st.cxTitleFor, st.cxTitleWait = began, who, waits
	return waits
}

// codexTitleCommand prints a tmux pane's title (pane: an id of tmux's,
// validated as the bind's).
func codexTitleCommand(pane string) string {
	return "tmux -u display-message -p -t " + sshx.QuoteShell(pane) + " '#{pane_title}'"
}

// codexPressed: keys or text go (or went) to a Codex's pane now: what was
// read of its title before is not what it shows after.
func (s *Server) codexPressed(sess *term.Session, pane string) {
	s.cxPressed.Store(s.convoReplyKey(sess, pane), time.Now())
}
