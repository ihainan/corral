package web

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/ihainan/corral/internal/herdr"
	"github.com/ihainan/corral/internal/store"
)

func seenSum(status, session string) herdrSummary {
	return herdrSummary{Workspaces: []herdrWorkspace{
		{ID: "w1", Agents: []herdrAgent{{PaneID: "w1:p1", Status: status, Session: session}}},
		{ID: "w2", Agents: []herdrAgent{}},
	}}
}

// An agent looked at on the agents page is no longer "done" in what the
// watcher tells, for the round it was looked at in.
func TestSeenRounds(t *testing.T) {
	r := &seenRounds{}
	poked := 0
	r.setPoke(func() { poked++ })
	told := func(status, session string) herdrAgent {
		s := seenSum(status, session)
		r.number(&s, false)
		return r.withSeen(s).Workspaces[0].Agents[0]
	}
	r.changed("w1:p1", herdr.StatusDone, "s1")
	a := told(herdr.StatusDone, "s1")
	if a.Status != herdr.StatusDone || a.Round == 0 {
		t.Fatalf("not looked at: %+v", a)
	}
	if r.markSeen("w1:p1", a.Round+1, "s1") || r.markSeen("w1:p1", a.Round, "s2") || r.markSeen("w1:p1", a.Round, "") || poked != 0 {
		t.Fatal("another round, or another session's, marked")
	}
	if !r.markSeen("w1:p1", a.Round, "s1") || poked != 1 {
		t.Fatal("its round not marked, or the summary not sent again")
	}
	if got := told(herdr.StatusDone, "s1"); got.Status != herdr.StatusIdle || got.Round != a.Round {
		t.Fatalf("looked at: %+v", got)
	}
	// What is kept is not changed: only what goes out.
	s := seenSum(herdr.StatusDone, "s1")
	r.number(&s, false)
	if s.Workspaces[0].Agents[0].Status != herdr.StatusDone {
		t.Fatal("the summary kept was changed")
	}
	// No agent: [], not null.
	if b, _ := json.Marshal(r.withSeen(s)); !strings.Contains(string(b), `"agents":[]`) {
		t.Fatalf("an empty workspace: %s", b)
	}
	// Working, then done again, in no time at all: a round of its own.
	r.changed("w1:p1", herdr.StatusWorking, "")
	r.changed("w1:p1", herdr.StatusDone, "")
	b := told(herdr.StatusDone, "s1")
	if b.Status != herdr.StatusDone || b.Round == a.Round {
		t.Fatalf("done again: %+v (first round %d)", b, a.Round)
	}
	if r.markSeen("w1:p1", a.Round, "s1") {
		t.Fatal("the word for the round over, late, marked")
	}
	// Another agent session in the pane, done as well: another round.
	c := told(herdr.StatusDone, "s2")
	if c.Round == b.Round || c.Status != herdr.StatusDone {
		t.Fatalf("another session: %+v", c)
	}
	// One with no session told (another agent, which says none): marked as that.
	d := told(herdr.StatusDone, "")
	if d.Round == c.Round || !r.markSeen("w1:p1", d.Round, "") || told(herdr.StatusDone, "").Status != herdr.StatusIdle {
		t.Fatalf("no session: %+v", d)
	}
	// Lost herdr: as noted, nothing new.
	o := seenSum(herdr.StatusWorking, "")
	r.number(&o, true)
	if o.Workspaces[0].Agents[0].Round != 0 || r.at["w1:p1"].status != herdr.StatusDone {
		t.Fatal("an old summary noted a round")
	}
	// Gone from the summaries: forgotten.
	e := herdrSummary{Workspaces: []herdrWorkspace{{ID: "w1", Agents: []herdrAgent{}}}}
	r.number(&e, false)
	if len(r.at) != 0 || len(r.seen) != 0 {
		t.Fatal("a pane gone is still kept")
	}
}

// A contest seen again while the end of the last was being read: that end
// is not this one's.
func TestOcContests(t *testing.T) {
	var c ocContests
	c.note("h\x00/p", true)
	_, open, seen := c.note("h\x00/p", false)
	if !open {
		t.Fatal("its end not to be read")
	}
	c.note("h\x00/p", true) // (contested again meanwhile)
	c.ended("h\x00/p", 100, seen)
	if e, o, _ := c.note("h\x00/p", false); e != 0 || !o {
		t.Fatalf("closed by an old end: %d %v", e, o)
	}
	_, _, seen = c.note("h\x00/p", false)
	c.ended("h\x00/p", 200, seen)
	if e, o, _ := c.note("h\x00/p", false); e != 200 || o {
		t.Fatalf("its own end: %d %v", e, o)
	}
}

// What was looked at, kept beyond the watcher: a watcher begun anew (the
// gateway started again) tells an agent "done" looked at before as idle,
// of the same agent session, its log not written since; until the first
// look over the host is back, as idle too. Seen other than "done" by this
// watcher, it goes by its rounds.
func TestSeenKept(t *testing.T) {
	var keptPane string
	var keptAs store.AgentSeen
	r := newSeenRounds(map[string]store.AgentSeen{"w1:p1": {Session: "s1", At: 1000}}, func(pane string, a store.AgentSeen) { keptPane, keptAs = pane, a })
	poked := 0
	r.setPoke(func() { poked++ })
	told := func(status, session string) herdrAgent {
		s := seenSum(status, session)
		r.number(&s, false)
		return r.withSeen(s).Workspaces[0].Agents[0]
	}
	if !r.wantsLook() {
		t.Fatal("no look wanted")
	}
	if got := told(herdr.StatusDone, "s1"); got.Status != herdr.StatusIdle {
		t.Fatalf("before the look: %s", got.Status)
	}
	if got := told(herdr.StatusDone, "s2"); got.Status != herdr.StatusDone {
		t.Fatalf("another session: %s", got.Status)
	}
	r.lookedOver(map[string]int64{"w1:p1": 900})
	if r.wantsLook() || poked != 1 {
		t.Fatal("the look not taken in")
	}
	r.lookedOver(map[string]int64{"w1:p1": 5000}) // (only the first)
	if got := told(herdr.StatusDone, "s1"); got.Status != herdr.StatusIdle {
		t.Fatalf("its log not written since: %s", got.Status)
	}
	// Written since (a round while no one watched), or not found: done.
	for _, wrote := range []map[string]int64{{"w1:p1": 1100}, {}, nil} {
		q := newSeenRounds(map[string]store.AgentSeen{"w1:p1": {Session: "s1", At: 1000}}, nil)
		q.lookedOver(wrote)
		s := seenSum(herdr.StatusDone, "s1")
		q.number(&s, false)
		if got := q.withSeen(s).Workspaces[0].Agents[0].Status; got != herdr.StatusDone {
			t.Fatalf("wrote %v: %s", wrote, got)
		}
	}
	// A lost herdr's summary changes nothing of it.
	o := seenSum(herdr.StatusWorking, "s1")
	r.number(&o, true)
	if got := told(herdr.StatusDone, "s1"); got.Status != herdr.StatusIdle {
		t.Fatalf("after a lost summary: %s", got.Status)
	}
	// Seen working by this watcher, then done: a round of its own, done.
	r.changed("w1:p1", herdr.StatusWorking, "")
	r.changed("w1:p1", herdr.StatusDone, "")
	a := told(herdr.StatusDone, "s1")
	if a.Status != herdr.StatusDone {
		t.Fatalf("done again while watched: %s", a.Status)
	}
	// Looked at: kept (said noted after), told by its round.
	before := time.Now().UnixMilli()
	if !r.markSeen("w1:p1", a.Round, "s1") || keptPane != "w1:p1" || keptAs.Session != "s1" || keptAs.At < before {
		t.Fatalf("not kept: %q %+v", keptPane, keptAs)
	}
	if got := told(herdr.StatusDone, "s1"); got.Status != herdr.StatusIdle {
		t.Fatalf("looked at: %s", got.Status)
	}
	// Seen working in a summary (no word of it): the kept one no longer stands.
	q := newSeenRounds(map[string]store.AgentSeen{"w1:p1": {Session: "s1", At: 1000}}, nil)
	for _, st := range []string{herdr.StatusWorking, herdr.StatusDone} {
		s := seenSum(st, "s1")
		q.number(&s, false)
		if st == herdr.StatusDone {
			if got := q.withSeen(s).Workspaces[0].Agents[0].Status; got != herdr.StatusDone {
				t.Fatalf("seen working, then done: %s", got)
			}
		}
	}
}
