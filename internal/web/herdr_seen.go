package web

import (
	"sync"
	"time"

	"github.com/ihainan/corral/internal/herdr"
	"github.com/ihainan/corral/internal/store"
)

// seenKeptFor: how long a look is kept (see seenRounds.kept).
const seenKeptFor = 30 * 24 * time.Hour

// seenRounds is what one watcher of a herdr server knows of the agents
// looked at on the agents page.
//
// herdr's "done" (finished, not looked at since) goes only with the pane
// focused in herdr, which would move the person's herdr elsewhere: read on
// a phone, an agent stayed "done" on every screen. Each status an agent
// comes to here is a round, numbered as they come (two in one millisecond
// are two; another agent session in the pane is another round); the browser
// marks the round of "done" it showed as seen, and while the agent is in
// that round it is told as idle. A word that comes late, for a round that
// is over, marks nothing.
//
// One watcher's own: it is told of a change before the snapshot that has
// it, so what it numbers is never behind itself (shared between watchers,
// one behind another undid what the other had noted). A terminal session
// is one watcher, whichever browsers show it.
//
// What was looked at is kept too (keep: by pane, its agent session and
// when), beyond this watcher's life: a watcher begun anew (the gateway
// started again, the session reconnected) numbers rounds anew, and an
// agent "done" since before it, looked at before, would show "done" again.
// What was kept before this watcher (prior) stands for a pane only until
// this watcher sees it other than "done" (from then on its rounds tell);
// and what went on while no one watched is told by the agent's log: a
// round more writes to it, so one whose log was last written (wrote, by
// the first look over the host after this watcher began, or after herdr
// came back, its clock put to the gateway's) no later than it was looked
// at, of the same agent session, is the "done" looked at. Until that look
// is back it is taken as looked at (a "done" told a few seconds late,
// rather than one looked at told anew); a log not found, or written since:
// "done". (When it was looked at is taken while it is still that round,
// and kept before it is noted: kept, or not noted.)
//
// Where herdr names its statuses (see statusMark), what was kept says which
// one was looked at, and a "done" since a watcher began is told by that
// instead: the same status of the same terminal is the one looked at, a
// later one is not, whether the agent's log is found or not (a Codex that
// keeps none open, say). A terminal begun again (herdr started anew) is
// told by the log, as before.
type seenRounds struct {
	mu     sync.Mutex
	at     map[string]seenRound // pane → the round it is in
	seen   map[string]seenRound // pane → the round of "done" looked at
	n      int64
	poke   func() // (has the watcher's last summary go again)
	prior  map[string]store.AgentSeen
	keep   func(pane string, a store.AgentSeen) error
	looked bool             // (the first look over the host is back)
	wrote  map[string]int64 // (what it found: pane → its log last written, unix ms)
	lost   bool             // (herdr lost: what went on meanwhile is looked for once it is back)
	gen    int64            // (looks begun before herdr was last lost or back count for nothing)

	// A "done" that may be one looked at, over again (see recheck): last,
	// the latest look at each pane (this watcher's, or kept from before
	// it); left, when it last went from "done" to another status (unix
	// ms); pending, the round of "done" its log is to be read for; quiet,
	// the round its log said was the one looked at.
	last    map[string]store.AgentSeen
	left    map[string]int64
	pending map[string]int64
	quiet   map[string]int64
}

// seenAwayMax: how long an agent may be away from "done" and still be
// taken, back, for a "done" looked at over again if its log says so
// (see recheck). Longer, it was at work.
const seenAwayMax = 60 * time.Second

// recheck: herdr counts every change of an agent's state, not its turns'
// ends only. herdr 0.8 seen (11-15): a Codex finished on 11-12, looked at
// on 11-14 and nothing written since, was told anew at 09:13 (from
// "#2535" to "#2746", its screen and log as they were), when another
// client of its app-server daemon came in; looked at, it was "done" in
// For you again. So a "done" of a pane looked at before, back after a
// short while at another status (or first seen by this watcher, kept
// from before it, its mark not that looked at), is read for in its log:
// one not written since that look is the "done" looked at. Till that is
// read, and where its log is not known for sure, it is "done": a turn's
// end hidden is worse than one told twice. (Codex only, the one seen to
// do it: see codexRecheck.)

type seenRound struct {
	status, session string
	n               int64
	at              int64  // (seen: when it was looked at, unix ms)
	mark            string // which status of herdr's, as its last summary said (see statusMark)
}

// changed: pane's agent went to status to (gone: forgotten). session is
// the agent session the change says, "" where it says none (the one noted
// stands).
func (r *seenRounds) changed(pane, to, session string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if to == herdr.StatusGone {
		r.forget(pane)
		return
	}
	if to != herdr.StatusDone {
		delete(r.prior, pane)
	}
	was, ok := r.at[pane]
	if session == "" {
		session = was.session
	}
	if !ok || was.status != to || was.session != session {
		r.next(pane, to, session)
	}
}

// (With r.mu held.)
func (r *seenRounds) next(pane, status, session string) seenRound {
	if r.at == nil {
		r.at = map[string]seenRound{}
	}
	was, had := r.at[pane]
	now := time.Now().UnixMilli()
	if had && was.status == herdr.StatusDone && status != herdr.StatusDone {
		setMs(&r.left, pane, now)
	}
	r.n++
	x := seenRound{status: status, session: session, n: r.n}
	r.at[pane] = x
	if status == herdr.StatusDone {
		// (Back to "done" soon, or first seen with a look kept from before
		// this watcher: its log is read for whether it is the one looked
		// at. See recheck.)
		left, away := r.left[pane]
		_, kept := r.prior[pane]
		if l, ok := r.last[pane]; ok && l.Session == session && (had && away && now-left <= seenAwayMax.Milliseconds() || !had && kept) {
			setMs(&r.pending, pane, x.n)
		}
		delete(r.left, pane)
	}
	return x
}

// forget: pane is gone. (With r.mu held.)
func (r *seenRounds) forget(pane string) {
	delete(r.at, pane)
	delete(r.seen, pane)
	delete(r.prior, pane)
	delete(r.last, pane)
	delete(r.left, pane)
	delete(r.pending, pane)
	delete(r.quiet, pane)
}

func setMs(m *map[string]int64, k string, v int64) {
	if *m == nil {
		*m = map[string]int64{}
	}
	(*m)[k] = v
}

// toCheck: the panes whose log is to be read (see recheck), by the round
// of "done" each is to be read for.
// gen: the reads', for checked. (None while herdr is lost.)
func (r *seenRounds) toCheck() (map[string]int64, int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.pending) == 0 || r.lost {
		return nil, r.gen
	}
	out := make(map[string]int64, len(r.pending))
	for p, n := range r.pending {
		out[p] = n
	}
	return out, r.gen
}

// checked takes in what pane's log said for its round of "done" round:
// wrote, when it was last written (unix ms, the gateway's clock; 0: not
// known). Not written since pane was last looked at: that round is told
// as the one looked at. (Asked for a round no longer the pane's: nothing.
// Read before herdr was last lost or back, gen not the rounds': nothing,
// and it is read again.)
func (r *seenRounds) checked(pane string, round, wrote, gen int64) {
	r.mu.Lock()
	if r.lost || gen != r.gen {
		r.mu.Unlock()
		return
	}
	if r.pending[pane] == round {
		delete(r.pending, pane)
	}
	x, ok := r.at[pane]
	l, looked := r.last[pane]
	if !ok || x.n != round || x.status != herdr.StatusDone || !looked || l.Session != x.session || wrote <= 0 || wrote > l.At {
		r.mu.Unlock()
		return
	}
	setMs(&r.quiet, pane, round)
	poke := r.poke
	r.mu.Unlock()
	if poke != nil {
		poke()
	}
}

// number gives each agent of sum its round: a status or an agent session
// other than the one noted begins another. old: the summary is of a
// snapshot no longer current (herdr lost); it notes nothing, and an agent
// not as noted has no round.
func (r *seenRounds) number(sum *herdrSummary, old bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	here := map[string]bool{}
	for i := range sum.Workspaces {
		for j := range sum.Workspaces[i].Agents {
			a := &sum.Workspaces[i].Agents[j]
			here[a.PaneID] = true
			if !old && a.Status != herdr.StatusDone {
				delete(r.prior, a.PaneID)
			}
			x, ok := r.at[a.PaneID]
			if !ok || x.status != a.Status || x.session != a.Session {
				if old {
					continue
				}
				x = r.next(a.PaneID, a.Status, a.Session)
			}
			if !old && a.Mark != "" && x.mark != a.Mark {
				x.mark = a.Mark // (the latest: a look keeps what it was when looked at)
				r.at[a.PaneID] = x
			}
			a.Round = x.n
		}
	}
	if old {
		return
	}
	if r.lost {
		r.lost = false // (back: a look begun from now)
		r.gen++
	}
	for p := range r.at {
		if !here[p] {
			delete(r.at, p)
			delete(r.seen, p)
			delete(r.left, p)
			delete(r.pending, p)
			delete(r.quiet, p)
		}
	}
}

// markSeen notes pane's agent looked at in its round of "done" round, of
// that agent session, and has the summary go again; false, nothing noted:
// that is not the round it is in.
func (r *seenRounds) markSeen(pane string, round int64, session string) (bool, error) {
	r.mu.Lock()
	x, ok := r.at[pane]
	if !ok || x.status != herdr.StatusDone || x.n != round || x.session != session {
		r.mu.Unlock()
		return false, nil
	}
	k := store.AgentSeen{Session: session, At: time.Now().UnixMilli(), Mark: x.mark}
	keep, gen := r.keep, r.gen
	r.mu.Unlock()
	if keep != nil {
		if err := keep(pane, k); err != nil {
			return false, err
		}
	}
	r.mu.Lock()
	if y, ok := r.at[pane]; !ok || y.n != x.n { // (its mark may have been filled in meanwhile: the same round)
		r.mu.Unlock()
		return false, nil // (another round meanwhile: what was kept is of before it)
	}
	if r.lost || r.gen != gen {
		// (herdr lost, or lost and back, since it was asked: its round may
		// be over unseen; told by the look, as what was kept before.)
		if p, ok := r.prior[pane]; !ok || p.At < k.At {
			if r.prior == nil {
				r.prior = map[string]store.AgentSeen{}
			}
			r.prior[pane] = k
		}
	} else if was, ok := r.seen[pane]; !ok || was.n != x.n || was.at < k.At { // (two at once: the later stands)
		if r.seen == nil {
			r.seen = map[string]seenRound{}
		}
		x.at = k.At
		r.seen[pane] = x
	}
	if l, ok := r.last[pane]; !ok || l.At < k.At { // (of its round still: see recheck)
		if r.last == nil {
			r.last = map[string]store.AgentSeen{}
		}
		r.last[pane] = k
	}
	poke := r.poke
	r.mu.Unlock()
	if poke != nil {
		poke()
	}
	return true, nil
}

// current: the round pane is in.
func (r *seenRounds) current(pane string) (seenRound, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	x, ok := r.at[pane]
	return x, ok
}

// newSeenRounds: prior, what was kept of what was looked at before this
// watcher; keep, how to keep more (as it is looked at, before it is said
// noted).
func newSeenRounds(prior map[string]store.AgentSeen, keep func(pane string, a store.AgentSeen) error) *seenRounds {
	last := make(map[string]store.AgentSeen, len(prior))
	for p, k := range prior {
		last[p] = k
	}
	return &seenRounds{prior: prior, keep: keep, last: last}
}

// wantsLook: a look over the host is wanted for what was kept (see
// seenRounds): there is some, the first look (since herdr came back) is
// not back, and herdr is not lost. gen: the look's, for lookedOver.
func (r *seenRounds) wantsLook() (bool, int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.prior) > 0 && !r.looked && !r.lost, r.gen
}

// lostHerdr: herdr is lost; what went on meanwhile is looked for again once
// it is back (see number). What this watcher saw looked at goes with what
// was kept before: a round while it was lost would come back as the same.
func (r *seenRounds) lostHerdr() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for p, x := range r.seen {
		if k, ok := r.prior[p]; !ok || k.At < x.at {
			if r.prior == nil {
				r.prior = map[string]store.AgentSeen{}
			}
			r.prior[p] = store.AgentSeen{Session: x.session, At: x.at, Mark: x.mark}
		}
		delete(r.seen, p)
	}
	// (What a log said is of before: a round told by it is read for again
	// once herdr is back, a turn ended meanwhile in the same round.)
	for p, q := range r.quiet {
		setMs(&r.pending, p, q)
	}
	r.quiet = nil
	r.lost, r.looked, r.wrote = true, false, nil
	r.gen++
}

// lookedOver takes in the first look over the host (nil: it failed, no
// log known) begun as gen, and has the summary go again.
func (r *seenRounds) lookedOver(wrote map[string]int64, gen int64) {
	r.mu.Lock()
	if r.looked || r.lost || gen != r.gen {
		r.mu.Unlock()
		return
	}
	r.looked, r.wrote = true, wrote
	poke := r.poke
	r.mu.Unlock()
	if poke != nil {
		poke()
	}
}

// lookedBefore: a, "done" now, is the "done" k was kept of — told by
// herdr's marks where both are of one terminal, else by its log. (With
// r.mu held.)
func (r *seenRounds) lookedBefore(k store.AgentSeen, a herdrAgent) bool {
	if sameTerminal(k.Mark, a.Mark) {
		return k.Mark == a.Mark
	}
	return !r.looked || r.wrote[a.PaneID] > 0 && r.wrote[a.PaneID] <= k.At
}

func (r *seenRounds) setPoke(f func()) {
	r.mu.Lock()
	r.poke = f
	r.mu.Unlock()
}

// withSeen is sum as it is told: an agent "done" in a round looked at,
// idle. (At the moment it goes out, so that no summary made before a look,
// and sent after it, says otherwise.)
func (r *seenRounds) withSeen(sum herdrSummary) herdrSummary {
	r.mu.Lock()
	defer r.mu.Unlock()
	ws := make([]herdrWorkspace, len(sum.Workspaces))
	for i, w := range sum.Workspaces {
		w.Agents = append(make([]herdrAgent, 0, len(w.Agents)), w.Agents...) // (none: [], not null)
		for j, a := range w.Agents {
			if a.Status != herdr.StatusDone {
				continue
			}
			if x, ok := r.seen[a.PaneID]; ok && x.n == a.Round && x.session == a.Session {
				w.Agents[j].Status = herdr.StatusIdle
			} else if k, ok := r.prior[a.PaneID]; ok && k.Session == a.Session && r.lookedBefore(k, a) {
				w.Agents[j].Status = herdr.StatusIdle // (looked at before this watcher: see seenRounds)
			} else if q, ok := r.quiet[a.PaneID]; ok && q == a.Round && a.Round != 0 {
				w.Agents[j].Status = herdr.StatusIdle // (its log not written since it was looked at: see recheck)
			}
		}
		ws[i] = w
	}
	sum.Workspaces = ws
	return sum
}
