package web

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/ihainan/corral/internal/herdr"
	"github.com/ihainan/corral/internal/sshx"
	"github.com/ihainan/corral/internal/term"
)

const (
	seenRecheckEvery = 3 * time.Second
	seenRecheckWait  = 10 * time.Second // (all of it: finding the logs, reading when they were written)
)

// recheckSeen reads, for each "done" that may be one looked at over again
// (see recheck), when its Codex last wrote its log, and tells rounds.
// Only a log known for sure to be the pane's (codexLogSure); any other, or
// a read that fails, leaves it "done".
func (s *Server) recheckSeen(ctx context.Context, sess *term.Session, rounds *seenRounds, latest func() *herdr.Snapshot) {
	tick := time.NewTicker(seenRecheckEvery)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		pend := rounds.toCheck()
		if len(pend) == 0 {
			continue
		}
		var snap *herdr.Snapshot
		if latest != nil {
			snap = latest()
		}
		cctx, cancel := context.WithTimeout(ctx, seenRecheckWait)
		logs := map[string]string{} // pane → its log
		for pane := range pend {
			if hp, ok := snapPane(snap, pane); ok && strings.Contains(strings.ToLower(hp.Agent), "codex") {
				if log := s.codexLogSure(cctx, sess, hp); log != "" {
					logs[pane] = log
				}
			}
		}
		wrote := map[string]int64{}
		if len(logs) > 0 {
			panes := make([]string, 0, len(logs))
			files := make([]string, 0, len(logs))
			for p, l := range logs {
				panes = append(panes, p)
				files = append(files, l)
			}
			out, err := sshx.Output(cctx, sess.Client(), logsWroteCommand(files)+"; "+hostNowCommand, 64*len(files)+256)
			got := time.Now().UnixMilli()
			if err == nil {
				wrote = onGatewayClock(parseLogsWrote(string(out), panes), string(out), got)
			}
		}
		cancel()
		for pane, round := range pend {
			rounds.checked(pane, round, wrote[pane]) // (0: not known, "done")
		}
	}
}

// snapPane: pane as snap has it, its agent's entry first (its agent named).
func snapPane(snap *herdr.Snapshot, pane string) (herdr.Pane, bool) {
	if snap == nil {
		return herdr.Pane{}, false
	}
	var found herdr.Pane
	ok := false
	for _, list := range [][]herdr.Pane{snap.Agents, snap.Panes} {
		for _, p := range list {
			if p.ID != pane {
				continue
			}
			if !ok {
				found, ok = p, true
			} else if found.Title == "" {
				found.Title = p.Title
			}
		}
	}
	return found, ok
}

// codexLogSure: the log of pane's Codex, where it is known for sure: the
// conversation page's binding where it is not weak (see convoState.weak),
// else the short look's where codexPaneSure takes it. "" otherwise.
func (s *Server) codexLogSure(ctx context.Context, sess *term.Session, pane herdr.Pane) string {
	if st := s.convos.peek(sess.ID + "\x00" + pane.ID); st != nil {
		st.mu.Lock()
		log, agent, reason, sub, weak := st.log, st.agent, st.reason, st.sub, st.weak
		st.mu.Unlock()
		if agent == "codex" && reason == "" && !sub && !weak && codexLogName.MatchString(log) {
			return log
		}
	}
	socket := s.herdrSocks.get(sess.ID)
	if socket == "" {
		return ""
	}
	panes, err := s.codexPanes.of(ctx, sess, socket)
	if err != nil {
		return ""
	}
	if c, ok := codexPaneSure(panes, pane.ID, pane); ok && codexLogName.MatchString(c.Path) {
		return c.Path
	}
	return ""
}

// logsWroteCommand prints, for each of files in turn, a line "w=" and when
// it was last written (unix seconds; empty: not read).
func logsWroteCommand(files []string) string {
	q := make([]string, len(files))
	for i, f := range files {
		q[i] = sshx.QuoteShell(f)
	}
	return `for f in ` + strings.Join(q, " ") + `; do printf 'w=%s\n' "$(stat -c %Y -- "$f" 2>/dev/null)"; done`
}

// parseLogsWrote reads logsWroteCommand's lines, for panes in the order of
// its files: unix ms, on the host's clock, rounded up (a log written in
// the second a look was taken is not taken for one written before it).
// One not read is left out.
func parseLogsWrote(out string, panes []string) map[string]int64 {
	at := map[string]int64{}
	i := 0
	for _, line := range strings.Split(out, "\n") {
		v, ok := strings.CutPrefix(strings.TrimRight(line, "\r"), "w=")
		if !ok {
			continue
		}
		if i >= len(panes) {
			break
		}
		if sec, err := strconv.ParseInt(v, 10, 64); err == nil && sec > 0 {
			at[panes[i]] = (sec + 1) * 1000
		}
		i++
	}
	return at
}
