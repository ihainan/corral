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
// What was kept before this watcher (prior) is told by time: one whose
// "done" began (its since) no later than it was looked at, of the same
// agent session, is that one. (Only what was kept before: a round this
// watcher saw begin is told by its round, as above.)
type seenRounds struct {
	mu    sync.Mutex
	at    map[string]seenRound // pane → the round it is in
	seen  map[string]seenRound // pane → the round of "done" looked at
	n     int64
	poke  func() // (has the watcher's last summary go again)
	prior map[string]store.AgentSeen
	keep  func(pane string, a store.AgentSeen)
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
		return
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
func (r *seenRounds) markSeen(pane string, round int64, session string) bool {
	r.mu.Lock()
	x, ok := r.at[pane]
	if !ok || x.status != herdr.StatusDone || x.n != round || x.session != session {
		r.mu.Unlock()
		return false
	}
	if r.seen == nil {
		r.seen = map[string]seenRound{}
	}
	r.seen[pane] = x
	poke, keep := r.poke, r.keep
	r.mu.Unlock()
	if keep != nil {
		keep(pane, store.AgentSeen{Session: session, At: time.Now().UnixMilli()})
	}
	if poke != nil {
		poke()
	}
	return true
}

// current: the round pane is in.
func (r *seenRounds) current(pane string) (seenRound, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	x, ok := r.at[pane]
	return x, ok
}

// setKept gives what was kept of what was looked at before this watcher,
// and how to keep more. (A look meanwhile, before it was read, is kept by
// what comes after it only: it is in seen, as this watcher's own.)
func (r *seenRounds) setKept(prior map[string]store.AgentSeen, keep func(pane string, a store.AgentSeen)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.prior, r.keep = prior, keep
	if r.poke != nil {
		go r.poke()
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
			} else if k, ok := r.prior[a.PaneID]; ok && k.Session == a.Session && a.Since <= k.At {
				// (Looked at before this watcher began, its "done" begun before
				// that: the same. When it began not yet known (0: a restart, its
				// log not yet looked at), taken as the same: a "done" told a
				// little late, rather than one looked at told anew.)
				w.Agents[j].Status = herdr.StatusIdle
			}
		}
		ws[i] = w
	}
	sum.Workspaces = ws
	return sum
}
