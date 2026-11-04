package web

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/ihainan/corral/internal/herdr"
	"github.com/ihainan/corral/internal/term"
)

// The counts the page's sound goes by: to "needs you" at once; to "done"
// from work only once finished (no subagents out); on the summary's agents,
// the panes gone forgotten (not while the summary is an old one).
func TestChimeCounts(t *testing.T) {
	s := &Server{}
	c := newChimeCounts()
	var pokes atomic.Int32
	c.setPoke(func() { pokes.Add(1) })
	s.herdrSocks.setChimes("s1", c)
	sess := &term.Session{ID: "s1"}
	p := herdr.Pane{ID: "w1:p1"}
	s.chimeOn(sess, herdr.Transition{Pane: p, From: herdr.StatusWorking, To: herdr.StatusBlocked})
	s.chimeOn(sess, herdr.Transition{Pane: p, From: herdr.StatusBlocked, To: herdr.StatusBlocked})
	s.chimeOn(sess, herdr.Transition{Pane: p, From: herdr.StatusIdle, To: herdr.StatusDone}) // (not from work: not a finish)
	s.chimeOn(sess, herdr.Transition{Pane: p, From: herdr.StatusBlocked, To: herdr.StatusDone}) // (answered, done at once: a finish)
	s.chimeOn(sess, herdr.Transition{Pane: p, From: herdr.StatusWorking, To: herdr.StatusDone})
	var got chimeN
	for i := 0; i < 100 && got.done == 0; i++ {
		time.Sleep(10 * time.Millisecond)
		c.mu.Lock()
		got = c.n["w1:p1"]
		c.mu.Unlock()
	}
	if got.needs != 1 || got.done != 1 || pokes.Load() != 2 { // (two finishes of one spell of work, unseen at it between: one)
		t.Fatalf("counts %+v, pokes %d", got, pokes.Load())
	}
	sum := herdrSummary{Workspaces: []herdrWorkspace{{Agents: []herdrAgent{{PaneID: "w1:p1"}, {PaneID: "w1:p2"}}}}}
	c.n["gone"] = chimeN{done: 3}
	old := sum
	old.Lost = true
	c.decorate(&old)
	if _, ok := c.n["gone"]; !ok {
		t.Fatal("an old summary forgot a pane")
	}
	c.decorate(&sum)
	if a := sum.Workspaces[0].Agents; a[0].ChimeDone != 1 || a[0].ChimeNeeds != 1 || a[1].ChimeDone != 0 {
		t.Fatalf("decorated %+v", a)
	}
	if _, ok := c.n["gone"]; ok {
		t.Fatal("a pane gone not forgotten")
	}
	// (No watcher: nothing counted, nothing fails.)
	s.chimeOn(&term.Session{ID: "s2"}, herdr.Transition{Pane: p, From: herdr.StatusWorking, To: herdr.StatusBlocked})
}

// A round counted once (two looks at one finish), none before the last.
func TestChimeRoundOnce(t *testing.T) {
	c := newChimeCounts()
	c.addRound("p", "s1", 5)
	c.addRound("p", "s1", 5)
	c.addRound("p", "s1", 7) // (the same work, looked at: another round, not another finish)
	if c.n["p"].done != 1 {
		t.Fatalf("%+v", c.n["p"])
	}
	c.addRound("p", "s2", 8) // (another session in the pane, its work not seen: its finish is one)
	c.working("p")
	c.addRound("p", "s2", 4) // (a round before the last counted)
	c.addRound("p", "s2", 9)
	c.working("p")
	c.addRound("p", "", 0)
	if c.n["p"].done != 4 {
		t.Fatalf("%+v", c.n["p"])
	}
}
