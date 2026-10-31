package web

import (
	"bytes"
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/ihainan/corral/internal/convo"
	"github.com/ihainan/corral/internal/sftplite"
	"github.com/ihainan/corral/internal/sshx"
	"github.com/ihainan/corral/internal/term"
)

// Whether a Codex in a tmux pane waits for an answer (an approval, a
// question): its log does not say (plans/5d2-tmux-codex.md); Codex 0.164
// titles its terminal "[ ! ] Action Required | <folder>", blinking with
// "[ . ] …", while it waits, and only then. The title is read when the
// log says it works or says nothing (a long log's tail read first), at
// most each convoTitleTTL for a pane, and read anew after keys or text
// went to it. A read that fails says it does not wait here (no card), and
// is not tried again for convoTitleBackoff (unless keys went to the pane since); the host's gate, reading it
// itself, lets no message or answer go when it cannot.

const (
	convoTitleTTL     = 2 * time.Second
	convoTitleBackoff = 10 * time.Second // (after a read that failed: not each poll a wait of its timeout)
)

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
	if waits, ok := st.codexTitleKept(who, at, time.Now()); ok {
		return waits
	}
	began := time.Now()
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	res, err := sshx.Exec(ctx, sess.Client(), codexTitleCommand(b.Pane), 4<<10, sshx.ExecOptions{})
	waits := err == nil && codexTitleWaits(strings.TrimRight(string(res.Out), "\n"))
	st.cxTitleAt, st.cxTitleFor, st.cxTitleWait, st.cxTitleFail, st.cxTitleFailAt = began, who, waits, err != nil, time.Now()
	return waits
}

// codexTitleCommand prints a tmux pane's title (pane: an id of tmux's,
// validated as the bind's).
func codexTitleCommand(pane string) string {
	return `PATH="$HOME/.local/bin:$HOME/bin:$HOME/.cargo/bin:/opt/homebrew/bin:/usr/local/bin:$PATH" tmux -u display-message -p -t ` + sshx.QuoteShell(pane) + " '#{pane_title}'"
}

// codexPressed: keys or text go (or went) to a Codex's pane now: what was
// read of its title before is not what it shows after.
func (s *Server) codexPressed(sess *term.Session, pane string) {
	s.cxPressed.Store(s.convoReplyKey(sess, pane), time.Now())
}

// codexTitleKept: what was read of the title of who's pane, if it may be
// used again at now: read since keys last went to it (pressed; by when the
// read began, so keys sent while it was under way count), within its time
// (a failed read's backoff from when it failed).
func (st *convoState) codexTitleKept(who string, pressed, now time.Time) (waits, ok bool) {
	fresh := now.Sub(st.cxTitleAt) < convoTitleTTL
	if st.cxTitleFail {
		fresh = now.Sub(st.cxTitleFailAt) < convoTitleBackoff
	}
	if st.cxTitleFor != who || !fresh || !st.cxTitleAt.After(pressed) {
		return false, false
	}
	return st.cxTitleWait, true
}

// codexBegunBy: the Codex log at log (one a process of the user's has
// open, as locating found it) has first, and no other, as the first
// message of the person's in its head (its first codexHeadMax bytes).
func (s *Server) codexBegunBy(ctx context.Context, sess *term.Session, log, first string) bool {
	var head []byte
	err := s.withSFTP(ctx, sess, 5*time.Second, func(c *sftplite.Client) error {
		var err error
		head, err = c.ReadRange(log, 0, codexHeadMax)
		return err
	})
	return err == nil && codexFirstIs(head, first)
}

// codexFirstIs: head (a Codex log's start) has first as the first message
// of the person's in it.
func codexFirstIs(head []byte, first string) bool {
	i := bytes.LastIndexByte(head, '\n')
	if i < 0 {
		return false
	}
	head = head[:i+1] // (whole lines only)
	cx := convo.NewCodex()
	cx.Feed(head)
	for _, e := range cx.Since(0) {
		if e.Role != convo.RoleUser {
			continue
		}
		var text strings.Builder
		for _, p := range e.Parts {
			text.WriteString(p.Text)
		}
		return strings.TrimSpace(text.String()) == strings.TrimSpace(first)
	}
	return false
}

const codexHeadMax = 256 << 10
