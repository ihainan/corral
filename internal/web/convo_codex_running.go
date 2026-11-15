package web

import (
	"strings"

	"github.com/ihainan/corral/internal/convo"
	"github.com/ihainan/corral/internal/term"
)

// codexRunningOnHost: commands a Codex's conversation (st) has as running
// that the host has no process of any more — Codex does not always tell
// of a process's end in its log (a script that polls several at once, its
// output telling none of them by name) — told ended. Only on what the scan
// of the host's processes tells for sure (cmdProber.readSeen): of this very
// conversation (each process found by the CODEX_THREAD_ID Codex gave it),
// whole, on a host whose processes can be read, begun after its log said
// it was at rest (after all its turns set going). Whichever pane runs it,
// named or not: what is told of is the thread's own. See codexRunGone.
func (s *Server) codexRunningOnHost(sess *term.Session, st *convoState) {
	if st.agent != "codex" || st.sub || st.bind == nil || !convoUUID.MatchString(st.bind.Session) {
		return
	}
	cx, ok := st.parser.(*convo.Codex)
	if !ok {
		return
	}
	var p *cmdProber
	switch sess.Persistence {
	case "herdr":
		p = s.herdrSocks.cmdsOf(sess.ID)
	case "tmux":
		p = s.tmuxCmdsOf(sess)
	}
	if p == nil {
		return
	}
	thread := strings.ToLower(st.bind.Session)
	n := 0
	for _, r := range cx.Running() {
		if r.Kind == "cmd" {
			n++
		}
	}
	p.readAt(thread, n > 0 && cx.Status() == "idle") // (looked for while it has some, at rest)
	live, ok := p.readSeen(thread)
	if !ok || n == 0 {
		return
	}
	cx.EndedOnHost(func(r convo.Running) bool { return codexRunGone(r, live, n) })
}

// codexRunGone: running r (one of n commands running) has no process on
// the host, which runs live of this conversation's: none at all; or more
// are running than live, and r's command is none of theirs (named, and
// all of them named: else which ended is not told).
func codexRunGone(r convo.Running, live []bgCmd, n int) bool {
	if len(live) == 0 {
		return true
	}
	what := strings.Join(strings.Fields(r.What), " ")
	if what == "" || n <= len(live) {
		return false
	}
	for _, l := range live {
		t := strings.Join(strings.Fields(l.Title), " ")
		if t == "" || strings.HasPrefix(what, strings.TrimSuffix(t, "…")) || strings.HasPrefix(t, what) {
			return false
		}
	}
	return true
}
