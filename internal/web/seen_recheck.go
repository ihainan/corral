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
	seenRecheckTries = 5                // reads that fail (the host, not the log's binding) before it is left "done"
)

// recheckSeen reads, for each "done" that may be one looked at over again
// (see recheck), when its Codex last wrote its log, and tells rounds.
// Only a log known for sure to be the pane's (codexLogSure); any other
// leaves it "done", as does a read that failed seenRecheckTries times.
func (s *Server) recheckSeen(ctx context.Context, sess *term.Session, rounds *seenRounds, latest func() *herdr.Snapshot) {
	tick := time.NewTicker(seenRecheckEvery)
	defer tick.Stop()
	failed := map[seenCheck]int{} // (by pane's round and mark: see below)

	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		pend, gen := rounds.toCheck()
		if len(pend) == 0 {
			continue
		}
		var snap *herdr.Snapshot
		if latest != nil {
			snap = latest()
		}
		cctx, cancel := context.WithTimeout(ctx, seenRecheckWait)
		logs := map[string]string{} // pane → its log
		unread := map[string]bool{} // panes whose read failed (the host's doing): tried again
		for pane := range pend {
			if hp, ok := snapPane(snap, pane); ok && strings.Contains(strings.ToLower(hp.Agent), "codex") {
				log, err := s.codexLogSure(cctx, sess, hp.ID)
				if err != nil {
					unread[pane] = true
				} else if log != "" {
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
			if err != nil || wrote == nil {
				for _, p := range panes {
					unread[p] = true
				}
			}
		}
		cancel()
		for pane, c := range pend {
			if unread[pane] {
				k := seenCheck{n: c.n, mark: pane + "\x00" + c.mark}
				if failed[k]++; failed[k] < seenRecheckTries {
					continue // (asked again next time)
				}
				delete(failed, k)
			}
			rounds.checked(pane, c, wrote[pane], gen) // (0: not known, "done")
		}
		if len(failed) > 64 {
			clear(failed) // (of rounds long over)
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

// codexLogSure: the log of pane's Codex, where a look begun now tells it
// for sure — surer than for showing it (the conversation page's binding,
// even one not weak, may be of a thread the pane has left since; and
// codexPaneSure takes a title cut short, or one of a name another
// conversation of its folder has): one interactive Codex there, at a
// thread no other pane is at, with its log; that thread named, the pane's
// title read at the look its name whole; no other live conversation of its
// folder of that name, none talked in since it started that no Codex is
// told at (a thread begun or resumed in it since, maybe). "" otherwise.
// (err: the look failed, the host's doing; asked again.)
func (s *Server) codexLogSure(ctx context.Context, sess *term.Session, pane string) (string, error) {
	socket := s.herdrSocks.get(sess.ID)
	if socket == "" {
		return "", nil
	}
	panes, err := s.codexPanes.run(ctx, sess, socket, pane)
	if err != nil {
		return "", err
	}
	return codexRecheckLog(panes, pane), nil
}

// codexRecheckLog: codexLogSure's rule, on what a look found.
func codexRecheckLog(panes map[string]codexPane, pane string) string {
	c, ok := panes[pane]
	if !ok || c.Several || c.Thread == "" || c.Name == "" || !codexLogName.MatchString(c.Path) {
		return ""
	}
	if !c.titleRead || c.SameName != 0 || c.Unexplained != 0 {
		return ""
	}
	if thread, bare := codexTitleParts("codex", c.TitleNow, c.Cwd); bare || thread != c.Name {
		return ""
	}
	for p, o := range panes {
		if p != pane && o.Thread == c.Thread {
			return ""
		}
	}
	return c.Path
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
