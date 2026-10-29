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
}

type seenRound struct {
	status, session string
	n               int64
}

// changed: pane's agent went to status to (gone: forgotten). session is
// the agent session the change says, "" where it says none (the one noted
// stands).
func (r *seenRounds) changed(pane, to, session string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if to == herdr.StatusGone {
		delete(r.at, pane)
		delete(r.seen, pane)
		delete(r.prior, pane)
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
	r.n++
	x := seenRound{status, session, r.n}
	r.at[pane] = x
	return x
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
	k := store.AgentSeen{Session: session, At: time.Now().UnixMilli()}
	keep := r.keep
	r.mu.Unlock()
	if keep != nil {
		if err := keep(pane, k); err != nil {
			return false, err
		}
	}
	r.mu.Lock()
	if y, ok := r.at[pane]; !ok || y != x {
		r.mu.Unlock()
		return false, nil // (another round meanwhile: what was kept is of before it)
	}
	if r.seen == nil {
		r.seen = map[string]seenRound{}
	}
	r.seen[pane] = x
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
	return &seenRounds{prior: prior, keep: keep}
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
// it is back (see number).
func (r *seenRounds) lostHerdr() {
	r.mu.Lock()
	defer r.mu.Unlock()
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
			} else if k, ok := r.prior[a.PaneID]; ok && k.Session == a.Session && (!r.looked || r.wrote[a.PaneID] > 0 && r.wrote[a.PaneID] <= k.At) {
				w.Agents[j].Status = herdr.StatusIdle // (looked at before this watcher: see seenRounds)
			}
		}
		ws[i] = w
	}
	sum.Workspaces = ws
	return sum
}
