package web

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ihainan/corral/internal/herdr"
	"github.com/ihainan/corral/internal/store"
)

func marked(r *seenRounds, pane string, round int64, session string) bool {
	ok, _ := r.markSeen(pane, round, session)
	return ok
}

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
	if marked(r, "w1:p1", a.Round+1, "s1") || marked(r, "w1:p1", a.Round, "s2") || marked(r, "w1:p1", a.Round, "") || poked != 0 {
		t.Fatal("another round, or another session's, marked")
	}
	if !marked(r, "w1:p1", a.Round, "s1") || poked != 1 {
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
	if marked(r, "w1:p1", a.Round, "s1") {
		t.Fatal("the word for the round over, late, marked")
	}
	// Another agent session in the pane, done as well: another round.
	c := told(herdr.StatusDone, "s2")
	if c.Round == b.Round || c.Status != herdr.StatusDone {
		t.Fatalf("another session: %+v", c)
	}
	// One with no session told (another agent, which says none): marked as that.
	d := told(herdr.StatusDone, "")
	if d.Round == c.Round || !marked(r, "w1:p1", d.Round, "") || told(herdr.StatusDone, "").Status != herdr.StatusIdle {
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
	var keepErr error
	r := newSeenRounds(map[string]store.AgentSeen{"w1:p1": {Session: "s1", At: 1000}}, func(pane string, a store.AgentSeen) error {
		keptPane, keptAs = pane, a
		return keepErr
	})
	poked := 0
	r.setPoke(func() { poked++ })
	told := func(status, session string) herdrAgent {
		s := seenSum(status, session)
		r.number(&s, false)
		return r.withSeen(s).Workspaces[0].Agents[0]
	}
	want, gen := r.wantsLook()
	if !want {
		t.Fatal("no look wanted")
	}
	if got := told(herdr.StatusDone, "s1"); got.Status != herdr.StatusIdle {
		t.Fatalf("before the look: %s", got.Status)
	}
	if got := told(herdr.StatusDone, "s2"); got.Status != herdr.StatusDone {
		t.Fatalf("another session: %s", got.Status)
	}
	r.lookedOver(map[string]int64{"w1:p1": 900}, gen)
	if want, _ := r.wantsLook(); want || poked != 1 {
		t.Fatal("the look not taken in")
	}
	r.lookedOver(map[string]int64{"w1:p1": 5000}, gen) // (only the first)
	if got := told(herdr.StatusDone, "s1"); got.Status != herdr.StatusIdle {
		t.Fatalf("its log not written since: %s", got.Status)
	}
	// Written since (a round while no one watched), or not found: done.
	for _, wrote := range []map[string]int64{{"w1:p1": 1100}, {}, nil} {
		q := newSeenRounds(map[string]store.AgentSeen{"w1:p1": {Session: "s1", At: 1000}}, nil)
		_, g := q.wantsLook()
		q.lookedOver(wrote, g)
		s := seenSum(herdr.StatusDone, "s1")
		q.number(&s, false)
		if got := q.withSeen(s).Workspaces[0].Agents[0].Status; got != herdr.StatusDone {
			t.Fatalf("wrote %v: %s", wrote, got)
		}
	}
	// herdr lost, then back: looked for again; a look begun before it came
	// back counts for nothing; one after, as the first.
	r.lostHerdr()
	o := seenSum(herdr.StatusWorking, "s1")
	r.number(&o, true) // (a lost herdr's summary changes nothing of it)
	if want, _ := r.wantsLook(); want {
		t.Fatal("a look wanted while lost")
	}
	_, stale := r.wantsLook()
	if got := told(herdr.StatusDone, "s1"); got.Status != herdr.StatusIdle {
		t.Fatalf("back, before the look: %s", got.Status)
	}
	r.lookedOver(map[string]int64{"w1:p1": 900}, stale)
	want, gen = r.wantsLook()
	if !want {
		t.Fatal("a look from before it came back taken in")
	}
	r.lookedOver(map[string]int64{"w1:p1": 1200}, gen) // (a round while it was lost)
	if got := told(herdr.StatusDone, "s1"); got.Status != herdr.StatusDone {
		t.Fatalf("a round while lost: %s", got.Status)
	}
	// Seen working by this watcher, then done: a round of its own, done.
	r.changed("w1:p1", herdr.StatusWorking, "")
	r.changed("w1:p1", herdr.StatusDone, "")
	a := told(herdr.StatusDone, "s1")
	if a.Status != herdr.StatusDone {
		t.Fatalf("done again while watched: %s", a.Status)
	}
	// Not kept: not noted.
	keepErr = errors.New("disk full")
	if ok, err := r.markSeen("w1:p1", a.Round, "s1"); ok || err == nil || told(herdr.StatusDone, "s1").Status != herdr.StatusDone {
		t.Fatal("noted though not kept")
	}
	// Looked at: kept (when, taken in its round), told by its round.
	keepErr = nil
	before := time.Now().UnixMilli()
	if !marked(r, "w1:p1", a.Round, "s1") || keptPane != "w1:p1" || keptAs.Session != "s1" || keptAs.At < before {
		t.Fatalf("not kept: %q %+v", keptPane, keptAs)
	}
	if got := told(herdr.StatusDone, "s1"); got.Status != herdr.StatusIdle {
		t.Fatalf("looked at: %s", got.Status)
	}
	// Looked at in this watcher, herdr lost and back: looked for again, as
	// what was kept before; a round while it was lost (its log written
	// since): done; none: idle.
	for _, round := range []bool{true, false} {
		q := newSeenRounds(nil, nil)
		qs := seenSum(herdr.StatusDone, "s1")
		q.number(&qs, false)
		at := time.Now().UnixMilli()
		if !marked(q, "w1:p1", qs.Workspaces[0].Agents[0].Round, "s1") {
			t.Fatal("not marked")
		}
		q.lostHerdr()
		qs = seenSum(herdr.StatusDone, "s1")
		q.number(&qs, false) // (back, done as before)
		want, g := q.wantsLook()
		if !want {
			t.Fatal("no look wanted for what this watcher saw looked at")
		}
		wrote := at - 5000
		if round {
			wrote = at + 5000
		}
		q.lookedOver(map[string]int64{"w1:p1": wrote}, g)
		qs = seenSum(herdr.StatusDone, "s1")
		q.number(&qs, false)
		if got, w := q.withSeen(qs).Workspaces[0].Agents[0].Status, map[bool]string{true: herdr.StatusDone, false: herdr.StatusIdle}[round]; got != w {
			t.Fatalf("a round while lost %v: %s", round, got)
		}
	}
	// Looked at while herdr is lost: told by the look once it is back, not by
	// its round (which may be over unseen).
	{
		q := newSeenRounds(nil, nil)
		qs := seenSum(herdr.StatusDone, "s1")
		q.number(&qs, false)
		round := qs.Workspaces[0].Agents[0].Round
		q.lostHerdr()
		at := time.Now().UnixMilli()
		if !marked(q, "w1:p1", round, "s1") || len(q.seen) != 0 || q.prior["w1:p1"].At < at {
			t.Fatalf("marked while lost: seen %v prior %v", q.seen, q.prior)
		}
		qs = seenSum(herdr.StatusDone, "s1")
		q.number(&qs, false)
		_, g := q.wantsLook()
		q.lookedOver(map[string]int64{"w1:p1": at + 5000}, g)
		qs = seenSum(herdr.StatusDone, "s1")
		q.number(&qs, false)
		if got := q.withSeen(qs).Workspaces[0].Agents[0].Status; got != herdr.StatusDone {
			t.Fatalf("a round while lost, marked meanwhile: %s", got)
		}
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

// The host's times put on the gateway's clock by the host's time printed last.
func TestOnGatewayClock(t *testing.T) {
	out := "pid=1\nlog=x\x00now=1793234937500\x00"
	if got := onGatewayClock(map[string]int64{"w1:p1": 1793234937000}, out, 1793234937000); got["w1:p1"] != 1793234936500 {
		t.Fatalf("ahead: %v", got)
	}
	if got := onGatewayClock(map[string]int64{"w1:p1": 1793234937000}, "now=1793234938\x00", 1793234937000); got["w1:p1"] != 1793234936000 {
		t.Fatalf("in seconds: %v", got)
	}
	if got := onGatewayClock(map[string]int64{"w1:p1": 10000}, "pid=1\x00", 10000); got != nil {
		t.Fatalf("no host time: %v", got)
	}
	if got := onGatewayClock(nil, out, 10000); got != nil {
		t.Fatalf("a failed look: %v", got)
	}
}
