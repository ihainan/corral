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
	var ids []string
	for _, r := range cx.Running() {
		if r.Kind == "cmd" {
			ids = append(ids, r.ID)
		}
	}
	n := len(ids)
	p.readAt(thread, n > 0 && cx.Status() == "idle", strings.Join(ids, " ")) // (looked for while it has some, at rest)
	live, ok := p.readSeen(thread)
	if !ok || n == 0 {
		return
	}
	all := cx.Running()
	cx.EndedOnHost(func(r convo.Running) bool { return codexRunGone(r, live, n, all) })
}

// codexRunGone: running r (one of n commands running) has no process on
// the host, which runs live of this conversation's: none at all; or more
// are running than live, every one live is one of them by its command (the
// one's words in the other's: "exec sleep 30" runs as "sleep 30", a long
// one cut short — review r2a-5), and r is none of those — else which ended
// is not told.
func codexRunGone(r convo.Running, live []bgCmd, n int, all []convo.Running) bool {
	if len(live) == 0 {
		return true
	}
	if n <= len(live) || codexRunWords(r.What) == "" {
		return false
	}
	for _, l := range live {
		found := false
		for _, o := range all {
			if o.Kind == "cmd" && codexSameCmd(o.What, l.Title) {
				found = true
				break
			}
		}
		if !found {
			return false // (a process not told apart by its command: which ended, not known)
		}
		if codexSameCmd(r.What, l.Title) {
			return false
		}
	}
	return true
}

func codexRunWords(s string) string {
	return strings.TrimSuffix(strings.Join(strings.Fields(s), " "), "…")
}

// codexSameCmd: a command as the log says it and as the host runs it are
// the same, as far as their words tell.
func codexSameCmd(what, title string) bool {
	a, b := codexRunWords(what), codexRunWords(title)
	return a != "" && b != "" && (strings.Contains(a, b) || strings.Contains(b, a))
}
