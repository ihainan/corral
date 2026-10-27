package web

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/ihainan/corral/internal/herdr"
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
