package web

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/ihainan/corral/internal/herdr"
	"github.com/ihainan/corral/internal/term"
)

// Chimes: how many times each pane's agent finished, and came to need the
// person, as the gateway judges it (the "finished" alert's own judgement:
// subagents it sent off back, the round the same), counted for the page's
// sound. Counts, not changes: a turn begun and ended between two readings
// of the list is a count gone up all the same. They go with each summary;
// a watcher's own, so a page takes the first it sees (and one gone down:
// the gateway began again) as where they stand, and sounds only for more.

type chimeN struct {
	done, needs int64
}

type chimeCounts struct {
	mu      sync.Mutex
	n       map[string]chimeN // pane → its counts
	counted map[string]int64  // pane → the last round counted as finished
	// told: pane → the agent session whose finish was counted, not at work
	// since: another word of a finish of it (the same work looked at, its
	// subagents back) is not another — review588-4; another session's in
	// the pane is (review590-1).
	told map[string]string
	poke func() // (has the watcher's last summary go again)
}

func newChimeCounts() *chimeCounts {
	return &chimeCounts{n: map[string]chimeN{}, counted: map[string]int64{}, told: map[string]string{}}
}

// addRound counts a finish of pane's agent (of agent session sid, "" not
// known) in round (the watcher's numbering: see seenRounds), once a round
// and not one before the last counted; 0, none known: counted.
func (c *chimeCounts) addRound(pane, sid string, round int64) {
	c.mu.Lock()
	if t, ok := c.told[pane]; (ok && t == sid) || (round != 0 && round <= c.counted[pane]) {
		c.mu.Unlock()
		return
	}
	if round != 0 {
		c.counted[pane] = round
	}
	c.told[pane] = sid
	c.mu.Unlock()
	c.add(pane, false)
}

// working: pane's agent is at work again: its next finish is news.
func (c *chimeCounts) working(pane string) {
	c.mu.Lock()
	delete(c.told, pane)
	c.mu.Unlock()
}

func (c *chimeCounts) setPoke(f func()) {
	c.mu.Lock()
	c.poke = f
	c.mu.Unlock()
}

func (c *chimeCounts) add(pane string, needs bool) {
	c.mu.Lock()
	x := c.n[pane]
	if needs {
		x.needs++
	} else {
		x.done++
	}
	c.n[pane] = x
	poke := c.poke
	c.mu.Unlock()
	if poke != nil {
		poke()
	}
}

// decorate puts the counts on the summary's agents (and forgets the panes
// no longer in it).
func (c *chimeCounts) decorate(sum *herdrSummary) {
	c.mu.Lock()
	defer c.mu.Unlock()
	here := map[string]bool{}
	for i := range sum.Workspaces {
		for j := range sum.Workspaces[i].Agents {
			a := &sum.Workspaces[i].Agents[j]
			here[a.PaneID] = true
			x := c.n[a.PaneID]
			a.ChimeDone, a.ChimeNeeds = x.done, x.needs
		}
	}
	if sum.Lost {
		return // (an old summary: what it lacks may be there still)
	}
	for p := range c.n {
		if !here[p] {
			delete(c.n, p)
			delete(c.counted, p)
			delete(c.told, p)
		}
	}
}

func (h *herdrSockets) setChimes(id string, c *chimeCounts) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.chimes == nil {
		h.chimes = map[string]*chimeCounts{}
	}
	h.chimes[id] = c
}

func (h *herdrSockets) chimesOf(id string) *chimeCounts {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.chimes[id]
}

// chimeOn counts a change of an agent's status: to "needs you" at once;
// to "done" (or idle) from work once what it left running is known, as
// the "finished" alert does (onHerdrTransition) — whoever looks at it.
func (s *Server) chimeOn(sess *term.Session, tr herdr.Transition) {
	c := s.herdrSocks.chimesOf(sess.ID)
	if c == nil {
		return
	}
	switch {
	case tr.To == herdr.StatusWorking:
		c.working(tr.Pane.ID)
	case tr.To == herdr.StatusBlocked && tr.From != herdr.StatusBlocked:
		c.add(tr.Pane.ID, true)
	// (Idle, not done: it finished in sight in herdr — the agents page shows
	// it there, say. Finished all the same. From waiting for the person:
	// answered, it worked on too briefly to be seen at it — review590-2.)
	case (tr.To == herdr.StatusDone || tr.To == herdr.StatusIdle) && (tr.From == herdr.StatusWorking || tr.From == herdr.StatusBlocked):
		var round int64
		sid := agentSession(tr.Pane)
		if rs := s.herdrSocks.roundsOf(sess.ID); rs != nil {
			if x, ok := rs.current(tr.Pane.ID); ok {
				round = x.n
				if sid == "" {
					sid = x.session
				}
			}
		}
		go func() {
			// (Looked at meanwhile — done to idle, the agents page showing it
			// — another round at rest: what was left running is asked of that
			// one. At work again: not a finish to tell (the next one tells of
			// itself). Subagents out: their coming back tells it (bg.setBack).
			// A round counted once, whichever looks at it — review585-5.)
			for try := 0; ; try++ {
				st := s.bgAwait(sess.ID, tr.Pane.ID, round)
				if st == bgRunning {
					return
				}
				rs := s.herdrSocks.roundsOf(sess.ID)
				if rs == nil || round == 0 {
					break
				}
				x, ok := rs.current(tr.Pane.ID)
				if !ok || x.status == herdr.StatusWorking || x.status == herdr.StatusBlocked {
					return
				}
				if (st != bgMoved && x.n == round) || try == 2 {
					round = x.n
					break
				}
				round = x.n
			}
			c.addRound(tr.Pane.ID, sid, round)
		}()
	}
}

// chimeOnDone: the person asked for a sound when an agent finishes (the
// "chime" setting; not set, it is on).
func (s *Server) chimeOnDone(user int64) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	raw, err := s.store.UserSettings(ctx, user)
	if err != nil {
		return false
	}
	var p struct {
		Chime string `json:"chime"`
	}
	return json.Unmarshal([]byte(raw), &p) == nil && p.Chime != "off"
}
