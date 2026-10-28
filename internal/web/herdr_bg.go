package web

import (
	"bytes"
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ihainan/corral/internal/herdr"
	"github.com/ihainan/corral/internal/sshx"
	"github.com/ihainan/corral/internal/term"
)

// Work a Claude Code agent left running when its turn ended.
//
// herdr says an agent is done when its turn is over, subagents it sent off
// on their own still at work: those come back with a notice that starts
// another turn by itself, and the work is not over until they have. Claude
// Code writes, as each turn ends, how many such subagents are still out
// (system:turn_duration, pendingBackgroundAgentCount; left out when none).
// So when a Claude pane comes to done (or idle) in herdr, the end of its log
// is read once: with subagents out it is "working in the background", not
// done — no "finished" alert, not among those to look at. A subagent coming
// back starts a turn (working), whose end is read again. Commands left
// running in the background (a dev server) hold nothing up: they may run
// for good.
//
// Read: on the change to done or idle; for one already so when the watch
// begins (or another agent session in the pane); and once a minute, for
// one said to have subagents out, to catch a turn missed. A turn's record
// is told from the last one's by its uuid: one not written yet when read
// is read again, a little later, three times at most (then not known, and
// held up for nothing).

// bgRetry: how long before a log is read again for a turn's record not
// written yet (a variable for the tests).
var bgRetry = 1500 * time.Millisecond

// bgRecheck: how often one said to have subagents out is read again (a
// variable for the tests).
var bgRecheck = time.Minute

const (
	bgChecking = "checking" // read for, not known yet
	bgNone     = "none"     // nothing left out
	bgRunning  = "running"  // subagents out (bgN of them)
	bgUnknown  = "unknown"  // could not be told
	bgMoved    = "moved"    // (await only) worked again, or another session, meanwhile

	bgTries    = 3
	bgTail     = 64 << 10  // of each log
	bgFar      = 512 << 10 // of one whose end had no turn record (what came after the turn: request records, a compaction while idle)
	bgBatch    = 12        // logs read in one go
	bgWait     = 5 * time.Second
	maxBgTaken = 256
)

type bgPane struct {
	session string
	round   int64
	state   string
	n       int
	since   int64 // (unix ms) when the turn ended, as this gateway saw it
	tries   int
	far     bool // (its end had no turn record: read further back)
	due     time.Time
	settled chan struct{} // closed once it is no longer checking
}

// bgProber is one herdr watcher's: of its Claude panes, what each left
// running.
type bgProber struct {
	mu    sync.Mutex
	panes map[string]*bgPane
	taken map[string]string // agent session → the uuid of its last turn record taken
	wake  chan struct{}
	poke  func() // (the watcher's last summary goes again)
	read  func(ctx context.Context, sessions []string, tail int) (map[string]bgRecord, error)
	// back: told of a pane whose subagents a look of the prober's own found
	// back, no turn's end told meanwhile (two changes in one snapshot): the
	// "finished" held back goes.
	back func(pane string)
}

// bgRecord is a log's last turn record: its uuid, and the subagents out.
type bgRecord struct {
	uuid string
	n    int
}

func newBgProber(read func(context.Context, []string, int) (map[string]bgRecord, error)) *bgProber {
	return &bgProber{panes: map[string]*bgPane{}, taken: map[string]string{}, wake: make(chan struct{}, 1), read: read}
}

// consider: pane's agent is in status (in round, of agent session session):
// what it left running is read for once it comes to done or idle, and
// forgotten once it works again.
func (p *bgProber) consider(pane, agent, status, session string, round int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	b := p.panes[pane]
	ended := status == herdr.StatusDone || status == herdr.StatusIdle
	if session == "" && ended && b != nil && strings.Contains(strings.ToLower(agent), "claude") {
		// (Its session not told this time — the gateway's reading of it
		// busy just now, say: what was known of the same turn stands.)
		b.round = round
		return
	}
	if !strings.Contains(strings.ToLower(agent), "claude") || !convoUUID.MatchString(session) || !ended {
		if b != nil {
			b.settle()
			delete(p.panes, pane)
		}
		return
	}
	if b != nil && b.session == session {
		b.round = round // (done, then idle: the same turn's end)
		return
	}
	if b != nil {
		b.settle()
	}
	p.panes[pane] = &bgPane{session: session, round: round, state: bgChecking, since: time.Now().UnixMilli(), due: time.Now(), settled: make(chan struct{})}
	select {
	case p.wake <- struct{}{}:
	default:
	}
}

func (b *bgPane) settle() {
	select {
	case <-b.settled:
	default:
		close(b.settled)
	}
}

// await: what pane left running at the end of the turn of round (0: of
// whatever round it is in), once known (at the timeout: checking);
// bgMoved where it is another round's now, or none is read for it.
func (p *bgProber) await(ctx context.Context, pane string, round int64) string {
	p.mu.Lock()
	b := p.panes[pane]
	p.mu.Unlock()
	if b == nil {
		return bgNone
	}
	select {
	case <-b.settled:
	case <-ctx.Done():
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.panes[pane] != b || (round != 0 && b.round != round) {
		return bgMoved
	}
	return b.state
}

func (p *bgProber) setBack(f func(pane string)) {
	p.mu.Lock()
	p.back = f
	p.mu.Unlock()
}

func (p *bgProber) setPoke(f func()) {
	p.mu.Lock()
	p.poke = f
	p.mu.Unlock()
}

// decorate tells sum's agents what they left running.
func (p *bgProber) decorate(sum *herdrSummary) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i := range sum.Workspaces {
		for j := range sum.Workspaces[i].Agents {
			a := &sum.Workspaces[i].Agents[j]
			if b := p.panes[a.PaneID]; b != nil && (a.Session == "" || b.session == a.Session) {
				a.Bg, a.BgN, a.BgSince = b.state, b.n, b.since
			}
		}
	}
}

// run reads what is due, until ctx ends.
func (p *bgProber) run(ctx context.Context) {
	timer := time.NewTimer(time.Hour)
	defer timer.Stop()
	for {
		p.mu.Lock()
		next := time.Time{}
		for _, b := range p.panes {
			if b.state == bgChecking || b.state == bgRunning {
				if next.IsZero() || b.due.Before(next) {
					next = b.due
				}
			}
		}
		p.mu.Unlock()
		wait := time.Hour
		if !next.IsZero() {
			wait = max(0, time.Until(next))
		}
		timer.Reset(wait)
		select {
		case <-ctx.Done():
			return
		case <-p.wake:
		case <-timer.C:
		}
		p.once(ctx)
	}
}

// once reads the logs of the panes due (a batch), and takes what they say.
func (p *bgProber) once(ctx context.Context) {
	now := time.Now()
	p.mu.Lock()
	due := map[string]*bgPane{}
	var sessions []string
	seen := map[string]bool{}
	// (Those read near the end first; those to read further back, apart.)
	far := true
	for _, b := range p.panes {
		if (b.state == bgChecking || b.state == bgRunning) && !b.due.After(now) && !b.far {
			far = false
		}
	}
	for pane, b := range p.panes {
		if (b.state == bgChecking || b.state == bgRunning) && !b.due.After(now) && b.far == far {
			if !seen[b.session] && len(sessions) < bgBatch {
				seen[b.session] = true
				sessions = append(sessions, b.session)
			}
			if seen[b.session] {
				due[pane] = b
			}
		}
	}
	p.mu.Unlock()
	if len(sessions) == 0 {
		return
	}
	tail := bgTail
	if far {
		tail = bgFar
	}
	rctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	got, err := p.read(rctx, sessions, tail)
	cancel()
	changed := false
	var backs []string
	p.mu.Lock()
	now = time.Now()
	for pane, b := range due {
		if p.panes[pane] != b {
			continue // (worked again, or another session, meanwhile: what was read is of before)
		}
		r, ok := got[b.session]
		was, have := p.taken[b.session]
		switch {
		case err != nil || !ok || r.uuid == "":
			if err == nil && ok {
				b.far = true // (none near the end: further back next)
			}
			if b.state == bgRunning {
				b.due = now.Add(bgRecheck) // (as it was, until read)
				continue
			}
			b.tries++
			if b.tries >= bgTries {
				b.state = bgUnknown
				b.settle()
				changed = true
			} else {
				b.due = now.Add(bgRetry)
			}
		case b.state == bgChecking && have && r.uuid == was:
			b.tries++ // (the last turn's: this one's not written yet)
			if b.tries >= bgTries {
				b.state = bgUnknown
				b.settle()
				changed = true
			} else {
				b.due = now.Add(bgRetry)
			}
		case b.state == bgRunning && have && r.uuid == was:
			b.due = now.Add(bgRecheck)
		default:
			p.taken[b.session] = r.uuid
			state := bgNone
			if r.n > 0 {
				state = bgRunning
			}
			if b.state == bgRunning && state == bgNone {
				backs = append(backs, pane)
			}
			changed = changed || state != b.state || r.n != b.n
			b.state, b.n = state, r.n
			b.due = now.Add(bgRecheck)
			b.settle()
		}
	}
	// (Kept across turns, the pane at work meanwhile: what tells this turn's
	// record from the last. Bounded: one of those no pane is of goes.)
	if len(p.taken) > maxBgTaken {
		live := map[string]bool{}
		for _, b := range p.panes {
			live[b.session] = true
		}
		for s := range p.taken {
			if !live[s] && len(p.taken) > maxBgTaken {
				delete(p.taken, s)
			}
		}
	}
	poke, back := p.poke, p.back
	p.mu.Unlock()
	if changed && poke != nil {
		poke()
	}
	if back != nil {
		for _, pane := range backs {
			back(pane)
		}
	}
}

// bgSession: the agent session of a pane, as herdr says (its integration
// does, in some versions), else as this gateway found it reading the
// pane's conversation, through this terminal session or another of the
// owner's to the same herdr server (the same host and socket).
func (s *Server) bgSession(sess *term.Session, pane, said string) string {
	if said != "" {
		return said
	}
	if sid := s.convos.sidOf(sess.ID + "\x00" + pane); sid != "" {
		return sid
	}
	socket := s.herdrSocks.get(sess.ID)
	for _, o := range s.terms.Running(sess.OwnerID, sess.HostID) {
		if o.ID != sess.ID && socket != "" && s.herdrSocks.get(o.ID) == socket {
			if sid := s.convos.sidOf(o.ID + "\x00" + pane); sid != "" {
				return sid
			}
		}
	}
	return ""
}

// bgTailCommand prints, for each Claude Code session id given (validated),
// a line "@@corral-bg <id>" and the end of its log. Fixed script; the ids
// are its arguments.
func bgTailCommand(sessions []string, tail int) string {
	script := `n=$1; shift
for s in "$@"; do
  printf '\n@@corral-bg %s\n' "$s"
  for f in "$HOME"/.claude/projects/*/"$s".jsonl; do [ -f "$f" ] && tail -c "$n" "$f"; break; done
done`
	cmd := "sh -c " + sshx.QuoteShell(script) + " corral " + strconv.Itoa(tail)
	for _, s := range sessions {
		cmd += " " + sshx.QuoteShell(s)
	}
	return cmd
}

// parseBgTails reads what bgTailCommand printed: each log's last turn
// record (none where the end read has none).
func parseBgTails(out []byte) map[string]bgRecord {
	got := map[string]bgRecord{}
	for _, part := range bytes.Split(out, []byte("\n@@corral-bg "))[1:] {
		nl := bytes.IndexByte(part, '\n')
		if nl < 0 {
			continue
		}
		sid := string(part[:nl])
		if !convoUUID.MatchString(sid) {
			continue
		}
		lines := bytes.Split(part[nl+1:], []byte("\n"))
		rec := bgRecord{}
		for i := len(lines) - 1; i >= 0; i-- {
			l := lines[i]
			if !bytes.Contains(l, []byte(`"turn_duration"`)) {
				continue
			}
			var r struct {
				Type    string `json:"type"`
				Subtype string `json:"subtype"`
				UUID    string `json:"uuid"`
				Pending *int   `json:"pendingBackgroundAgentCount"`
				Side    bool   `json:"isSidechain"`
			}
			if json.Unmarshal(l, &r) != nil || r.Type != "system" || r.Subtype != "turn_duration" || r.Side || r.UUID == "" {
				continue // (cut by the tail's start, or not the record)
			}
			rec.uuid = clipStr(r.UUID, 128)
			if r.Pending != nil && *r.Pending > 0 {
				rec.n = min(*r.Pending, 999)
			}
			break
		}
		got[sid] = rec
	}
	return got
}

func clipStr(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
