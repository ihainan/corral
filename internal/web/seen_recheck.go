package web

import (
	"context"
	"regexp"
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
// (see recheck), when its agent last wrote its log, and tells rounds.
// Only a log known for sure to be the pane's: a Codex's, codexLogSure; a
// Claude Code's, by the session herdr says it runs. Any other (OpenCode's
// database, all its sessions' in one file, written by any of them) leaves
// it "done", as does a read that failed seenRecheckTries times.
func (s *Server) recheckSeen(ctx context.Context, sess *term.Session, rounds *seenRounds, latest func() *herdr.Snapshot) {
	tick := time.NewTicker(seenRecheckEvery)
	defer tick.Stop()
	type tryKey struct {
		pane string
		c    seenCheck
	}
	failed := map[tryKey]int{} // reads failed, by pane's round and mark

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
			if hp, ok := snapPane(snap, pane); ok {
				log, err := s.paneLogRef(cctx, sess, hp)
				if err != nil {
					unread[pane] = true
				} else if log != "" {
					logs[pane] = log
				}
			}
		}
		var lo, hi map[string]int64
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
				l, h := parseLogsWrote(string(out), panes)
				lo, hi = onGatewayClock(l, string(out), got), onGatewayClock(h, string(out), got)
				if hostNowInSeconds(string(out)) {
					for p := range lo {
						lo[p] -= 999 // (the host's now told to the second: up to 999 ms later than said)
					}
				}
			}
			if err != nil {
				s.recordCorraldHintSessionError(sess, HintOpStatus, err)
			}
			if err != nil || lo == nil || hi == nil {
				for _, p := range panes {
					unread[p] = true
				}
			}
		}
		cancel()
		for k := range failed {
			if pend[k.pane] != k.c {
				delete(failed, k) // (no longer asked for)
			}
		}
		for pane, c := range pend {
			if unread[pane] {
				k := tryKey{pane, c}
				if failed[k]++; failed[k] < seenRecheckTries {
					continue // (asked again next time)
				}
				delete(failed, k)
			}
			rounds.checked(pane, c, lo[pane], hi[pane], gen) // (0: not known, "done")
		}
	}
}

// paneLogRef: the log of the agent in hp known for sure to be its, as
// logsWroteCommand takes it: a Codex's, codexLogSure; a Claude Code's, by
// the session herdr's integration says it runs ("claude:<id>"). "": none
// known for sure (OpenCode's database, all its sessions' in one file); an
// error: the host not read.
func (s *Server) paneLogRef(ctx context.Context, sess *term.Session, hp herdr.Pane) (string, error) {
	switch agent := strings.ToLower(hp.Agent); {
	case strings.Contains(agent, "codex"):
		return s.codexLogSure(ctx, sess, hp.ID)
	case strings.Contains(agent, "claude") && hp.AgentSession != nil && claudeSessionID.MatchString(hp.AgentSession.Value):
		return "claude:" + strings.ToLower(hp.AgentSession.Value), nil
	}
	return "", nil
}

// paneLogWrote: when the agent in hp last wrote its log (paneLogRef), on
// the gateway's clock, unix ms (the latest it may have been); ok false
// where that is not known.
func (s *Server) paneLogWrote(ctx context.Context, sess *term.Session, hp herdr.Pane) (hi int64, ok bool) {
	ref, err := s.paneLogRef(ctx, sess, hp)
	if err != nil || ref == "" {
		return 0, false
	}
	out, err := sshx.Output(ctx, sess.Client(), logsWroteCommand([]string{ref})+"; "+hostNowCommand, 512)
	got := time.Now().UnixMilli()
	if err != nil {
		s.recordCorraldHintSessionError(sess, HintOpStatus, err)
		return 0, false
	}
	_, h := parseLogsWrote(string(out), []string{"p"})
	h = onGatewayClock(h, string(out), got)
	if t, there := h["p"]; there {
		return t, true
	}
	return 0, false
}

// doneQuietFor: a "finished" whose agent's log was last written longer ago
// than this before it is no turn's end — a turn's end is written there as
// it ends (Codex's task_complete, Claude Code's last answer) — but herdr
// telling a status again (its count goes up for any change: a client of
// Codex's daemon come in, 11-15): no alert.
const doneQuietFor = 60 * time.Second

// doneQuiet: doneQuietFor's rule — the log last written at wrote (gateway
// clock, unix ms), the agent told done at at.
func doneQuiet(wrote, at int64) bool {
	return wrote > 0 && wrote < at-doneQuietFor.Milliseconds()
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
// it was last written: unix ms where the host's date tells them (GNU
// date -r), else unix seconds (GNU stat, or BSD's and macOS's: no %N in
// their date, no -c in their stat); empty: not read. A file
// "claude:<session id>" is that Claude Code session's log when strictly one
// exists across physical-deduplicated config dirs (empty / unknown when absent,
// ambiguous, or config dir enumeration was incomplete).
func logsWroteCommand(files []string) string {
	var b strings.Builder
	needClaude := false
	for _, f := range files {
		if sid, ok := strings.CutPrefix(f, "claude:"); ok && claudeSessionID.MatchString(sid) {
			needClaude = true
			break
		}
	}
	if needClaude {
		b.WriteString(claudeConfigDirsSh + "claude_cfg_dirs\n")
	}
	for _, f := range files {
		if sid, ok := strings.CutPrefix(f, "claude:"); ok {
			if !claudeSessionID.MatchString(sid) {
				b.WriteString(`f=; `)
			} else {
				sid = strings.ToLower(sid)
				b.WriteString(`f=; _ln=0; if [ "$cldirs_incomplete" = 0 ]; then while IFS= read -r _cd; do [ -n "$_cd" ] || continue; for _cf in "$_cd"/projects/*/` + sid + `.jsonl; do [ -f "$_cf" ] || continue; _ln=$((_ln + 1)); if [ "$_ln" -eq 1 ]; then f=$_cf; else f=; break; fi; done; [ "$_ln" -le 1 ] || break; done <<CORRAL_CLDIRS` + "\n" + `$cldirs` + "\n" + `CORRAL_CLDIRS` + "\n" + `fi; `)
			}
		} else {
			b.WriteString(`f=` + sshx.QuoteShell(f) + `; `)
		}
		b.WriteString(`t=; if [ -n "$f" ]; then t=$(date -r "$f" +%s%3N 2>/dev/null); case $t in ""|*[!0-9]*) t=$(stat -c %Y -- "$f" 2>/dev/null || stat -f %m -- "$f" 2>/dev/null);; esac; fi; printf 'w=%s\n' "$t"; `)
	}
	return strings.TrimSuffix(b.String(), "; ")
}

// claudeSessionID: a Claude Code session's id as herdr tells it.
var claudeSessionID = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// parseLogsWrote reads logsWroteCommand's lines, for panes in the order of
// its files: when each was last written, unix ms on the host's clock, no
// earlier than lo and no later than hi (a time in seconds: that second's
// first and last millisecond). One not read is left out.
func parseLogsWrote(out string, panes []string) (lo, hi map[string]int64) {
	lo, hi = map[string]int64{}, map[string]int64{}
	i := 0
	for _, line := range strings.Split(out, "\n") {
		v, ok := strings.CutPrefix(strings.TrimRight(line, "\r"), "w=")
		if !ok {
			continue
		}
		if i >= len(panes) {
			break
		}
		if t, err := strconv.ParseInt(v, 10, 64); err == nil && t > 0 {
			if t < 1e11 { // (seconds)
				lo[panes[i]], hi[panes[i]] = t*1000, t*1000+999
			} else {
				lo[panes[i]], hi[panes[i]] = t, t
			}
		}
		i++
	}
	return lo, hi
}

// hostNowInSeconds: the host's time hostNowCommand printed last in out is
// to the second only (a date with no %N, its milliseconds "000").
func hostNowInSeconds(out string) bool {
	i := strings.LastIndex(out, "now=")
	if i < 0 {
		return false
	}
	v, _, _ := strings.Cut(out[i+len("now="):], "\x00")
	n, err := strconv.ParseInt(v, 10, 64)
	return err == nil && (n < 1e11 || n%1000 == 0)
}
