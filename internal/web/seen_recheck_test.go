package web

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/ihainan/corral/internal/herdr"
	"github.com/ihainan/corral/internal/store"
)

func pend(q *seenRounds) map[string]seenCheck { p, _ := q.toCheck(); return p }

func gen(q *seenRounds) int64 { _, g := q.toCheck(); return g }

// chk: what the log of w1:p1 said for its round round, as of its mark now.
func chk(q *seenRounds, round, wrote, g int64) {
	q.mu.Lock()
	mark := q.at["w1:p1"].mark
	q.mu.Unlock()
	q.checked("w1:p1", seenCheck{n: round, mark: mark}, wrote, g)
}

// A "done" kept from before this watcher, of a later mark of its terminal
// (herdr counts any change of state, not turns' ends alone): "done" till its
// log is read; then the one looked at if it was not written since, "done"
// if it was or is not known.
func TestSeenRecheckKept(t *testing.T) {
	for _, c := range []struct {
		wrote int64
		want  string
	}{
		{900, herdr.StatusIdle},  // (not written since the look at 1000)
		{1000, herdr.StatusIdle}, // (as it was looked at)
		{1001, herdr.StatusDone}, // (written since)
		{0, herdr.StatusDone},    // (its log not known)
	} {
		q := newSeenRounds(map[string]store.AgentSeen{"w1:p1": {At: 1000, Mark: "term_a#5"}}, nil)
		_, g := q.wantsLook()
		q.lookedOver(map[string]int64{"w1:p1": 900}, g)
		s := seenSum(herdr.StatusDone, "")
		s.Workspaces[0].Agents[0].Mark = "term_a#9"
		q.number(&s, false)
		round := s.Workspaces[0].Agents[0].Round
		if got := q.withSeen(s).Workspaces[0].Agents[0].Status; got != herdr.StatusDone {
			t.Fatalf("before its log is read: %s", got)
		}
		if p := pend(q); p["w1:p1"].n != round {
			t.Fatalf("not to be read: %v (round %d)", p, round)
		}
		chk(q, round, c.wrote, gen(q))
		if got := q.withSeen(s).Workspaces[0].Agents[0].Status; got != c.want {
			t.Errorf("wrote %d: %s, want %s", c.wrote, got, c.want)
		}
		if len(pend(q)) != 0 {
			t.Error("still to be read")
		}
	}
}

// Looked at, then away from "done" for a moment and back: "done" till its
// log is read, the one looked at if not written since. Away long, or
// another agent session, or a read for a round over: as before.
func TestSeenRecheckBack(t *testing.T) {
	looked := func() (*seenRounds, store.AgentSeen) {
		var kept store.AgentSeen
		q := newSeenRounds(nil, func(_ string, a store.AgentSeen) error { kept = a; return nil })
		q.changed("w1:p1", herdr.StatusDone, "")
		s := seenSum(herdr.StatusDone, "")
		q.number(&s, false)
		if !marked(q, "w1:p1", s.Workspaces[0].Agents[0].Round, "") {
			t.Fatal("not marked")
		}
		return q, kept
	}
	told := func(q *seenRounds) herdrAgent {
		s := seenSum(herdr.StatusDone, "")
		q.number(&s, false)
		return q.withSeen(s).Workspaces[0].Agents[0]
	}
	for _, away := range []string{herdr.StatusUnknown, herdr.StatusWorking} {
		q, k := looked()
		q.changed("w1:p1", away, "")
		q.changed("w1:p1", herdr.StatusDone, "")
		a := told(q)
		if a.Status != herdr.StatusDone || pend(q)["w1:p1"].n != a.Round {
			t.Fatalf("%s and back, before its log is read: %+v %v", away, a, pend(q))
		}
		chk(q, a.Round, k.At-500, gen(q))
		if got := told(q); got.Status != herdr.StatusIdle || got.Round != a.Round {
			t.Errorf("%s and back, not written since: %+v", away, got)
		}
		// (And again, written since: "done".)
		q.changed("w1:p1", away, "")
		q.changed("w1:p1", herdr.StatusDone, "")
		b := told(q)
		chk(q, b.Round, k.At+1, gen(q))
		if got := told(q); got.Status != herdr.StatusDone {
			t.Errorf("%s and back, written since: %+v", away, got)
		}
	}
	// Away long: at work; not read for.
	q, _ := looked()
	q.changed("w1:p1", herdr.StatusWorking, "")
	q.mu.Lock()
	q.left["w1:p1"] -= seenAwayMax.Milliseconds() + 1
	q.mu.Unlock()
	q.changed("w1:p1", herdr.StatusDone, "")
	if a := told(q); a.Status != herdr.StatusDone || len(pend(q)) != 0 {
		t.Fatalf("away long: %+v %v", a, pend(q))
	}
	// Another agent session: not the one looked at.
	q, k := looked()
	q.changed("w1:p1", herdr.StatusWorking, "")
	q.changed("w1:p1", herdr.StatusDone, "s2")
	if len(pend(q)) != 0 {
		t.Fatalf("another session read for: %v", pend(q))
	}
	// A read for a round over says nothing of the next.
	q, k = looked()
	q.changed("w1:p1", herdr.StatusUnknown, "")
	q.changed("w1:p1", herdr.StatusDone, "")
	a := told(q)
	q.changed("w1:p1", herdr.StatusWorking, "")
	q.changed("w1:p1", herdr.StatusDone, "")
	b := told(q)
	chk(q, a.Round, k.At-500, gen(q))
	if got := told(q); got.Status != herdr.StatusDone || got.Round != b.Round {
		t.Fatalf("a read for the round over took: %+v", got)
	}
	// Gone: forgotten.
	q.changed("w1:p1", herdr.StatusGone, "")
	if len(pend(q)) != 0 || len(q.last) != 0 || len(q.quiet) != 0 {
		t.Fatal("a pane gone is still kept")
	}
}

// The command reads when each log was written, in turn; one not there
// keeps its place, unread.
func TestLogsWrote(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "it's a.jsonl")
	if err := os.WriteFile(a, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	at := time.Now().Add(-time.Hour).Truncate(time.Second)
	if err := os.Chtimes(a, at, at); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("sh", "-c", logsWroteCommand([]string{filepath.Join(dir, "none.jsonl"), a})).Output()
	if err != nil {
		t.Fatal(err)
	}
	got := parseLogsWrote(string(out), []string{"p0", "p1"})
	if _, ok := got["p0"]; ok || got["p1"] != at.UnixMilli()+1000 {
		t.Fatalf("%v (written %d)", got, at.UnixMilli())
	}
}

// herdr lost: what a log said is of before; read again once it is back
// (a turn ended meanwhile, in the same round), and a read begun before it
// was lost, or before it was back, tells nothing.
func TestSeenRecheckLost(t *testing.T) {
	q := newSeenRounds(map[string]store.AgentSeen{"w1:p1": {At: 1000, Mark: "term_a#5"}}, nil)
	s := seenSum(herdr.StatusDone, "")
	s.Workspaces[0].Agents[0].Mark = "term_a#9"
	q.number(&s, false)
	round := s.Workspaces[0].Agents[0].Round
	chk(q, round, 900, gen(q))
	status := func() string {
		s := seenSum(herdr.StatusDone, "")
		s.Workspaces[0].Agents[0].Mark = "term_a#9"
		q.number(&s, false)
		return q.withSeen(s).Workspaces[0].Agents[0].Status
	}
	if got := status(); got != herdr.StatusIdle {
		t.Fatalf("not written since: %s", got)
	}
	before := gen(q)
	q.lostHerdr()
	if len(pend(q)) != 0 {
		t.Fatal("read for while lost")
	}
	chk(q, round, 900, before) // (begun before it was lost)
	if _, ok := q.quiet["w1:p1"]; ok {
		t.Fatal("lost, what the log said before still stands")
	}
	if got := status(); got != herdr.StatusDone { // (back)
		t.Fatalf("back, before it is read again: %s", got)
	}
	p, g := q.toCheck()
	if p["w1:p1"].n != round || g == before {
		t.Fatalf("back, not to be read again: %v %d", p, g)
	}
	chk(q, round, 900, before) // (a read from before: nothing)
	if got := status(); got != herdr.StatusDone || len(pend(q)) != 1 {
		t.Fatalf("a read from before took: %s %v", got, pend(q))
	}
	chk(q, round, 2000, g) // (a turn ended while it was lost)
	if got := status(); got != herdr.StatusDone {
		t.Fatalf("written while lost: %s", got)
	}
}

// Only a log a look tells for sure is read: the thread named, the title
// its whole name, no other of its name or unexplained in its folder, no
// other pane at it, its title read at the look.
func TestCodexRecheckLog(t *testing.T) {
	const log = "/h/.codex/sessions/2026/11/12/rollout-2026-11-12T14-44-47-01a1ce80-757b-7792-8f1b-63c9d996ff88.jsonl"
	good := codexPane{Cwd: "/h/AI", Thread: "01a1ce80-757b-7792-8f1b-63c9d996ff88", Path: log, Name: "Check the subscription", Src: "argv", TitleNow: "Check the subscription | AI", titleRead: true}
	if got := codexRecheckLog(map[string]codexPane{"w1:p1": good}, "w1:p1"); got != log {
		t.Fatalf("sure one: %q", got)
	}
	for name, mod := range map[string]func(c *codexPane){
		"no name":     func(c *codexPane) { c.Name = ""; c.TitleNow = "AI" },
		"cut short":   func(c *codexPane) { c.TitleNow = "Check the sub… | AI" },
		"another":     func(c *codexPane) { c.TitleNow = "Something else | AI" },
		"same name":   func(c *codexPane) { c.SameName = 1 },
		"unexplained": func(c *codexPane) { c.Unexplained = 1 },
		"not read":    func(c *codexPane) { c.titleRead = false },
		"several":     func(c *codexPane) { c.Several = true },
		"no log":      func(c *codexPane) { c.Path = "" },
	} {
		c := good
		mod(&c)
		if got := codexRecheckLog(map[string]codexPane{"w1:p1": c}, "w1:p1"); got != "" {
			t.Errorf("%s: %q", name, got)
		}
	}
	if got := codexRecheckLog(map[string]codexPane{"w1:p1": good, "w2:p1": good}, "w1:p1"); got != "" {
		t.Errorf("two panes at it: %q", got)
	}
}

// Told the one looked at by its log, then another mark in the same round
// (a turn begun and ended between two snapshots): read again, "done" till
// then and if written since. A read asked for under the old mark tells
// nothing. A new round drops what the log said of the last.
func TestSeenRecheckMark(t *testing.T) {
	q := newSeenRounds(map[string]store.AgentSeen{"w1:p1": {At: 1000, Mark: "term_a#5"}}, nil)
	sum := func(mark string) herdrSummary {
		s := seenSum(herdr.StatusDone, "")
		s.Workspaces[0].Agents[0].Mark = mark
		q.number(&s, false)
		return s
	}
	status := func(mark string) string { return q.withSeen(sum(mark)).Workspaces[0].Agents[0].Status }
	s := sum("term_a#9")
	round := s.Workspaces[0].Agents[0].Round
	asked, g := q.toCheck()
	q.checked("w1:p1", asked["w1:p1"], 900, g)
	if got := status("term_a#9"); got != herdr.StatusIdle {
		t.Fatalf("not written since: %s", got)
	}
	if got := status("term_a#11"); got != herdr.StatusDone {
		t.Fatalf("another mark, as before: %s", got)
	}
	again, g := q.toCheck()
	if again["w1:p1"].n != round || again["w1:p1"].mark != "term_a#11" {
		t.Fatalf("not read again: %v", again)
	}
	q.checked("w1:p1", asked["w1:p1"], 900, g) // (asked under the old mark)
	if got := status("term_a#11"); got != herdr.StatusDone || len(pend(q)) != 1 {
		t.Fatalf("a read under the old mark took: %s %v", got, pend(q))
	}
	q.checked("w1:p1", again["w1:p1"], 2000, g)
	if got := status("term_a#11"); got != herdr.StatusDone {
		t.Fatalf("written since: %s", got)
	}
	// A new round: what the log said of the last is dropped, and herdr
	// lost does not bring it back over the new round's read.
	q2 := newSeenRounds(map[string]store.AgentSeen{"w1:p1": {At: 1000, Mark: "term_a#5"}}, nil)
	s2 := seenSum(herdr.StatusDone, "")
	s2.Workspaces[0].Agents[0].Mark = "term_a#9"
	q2.number(&s2, false)
	a2, g2 := q2.toCheck()
	q2.checked("w1:p1", a2["w1:p1"], 900, g2)
	q2.changed("w1:p1", herdr.StatusUnknown, "")
	q2.changed("w1:p1", herdr.StatusDone, "")
	if len(q2.quiet) != 0 {
		t.Fatal("what the log said of a round over kept")
	}
	cur, _ := q2.current("w1:p1")
	q2.lostHerdr()
	if q2.pending["w1:p1"] != cur.n {
		t.Fatalf("lost: the new round's read put back to %d (round %d)", q2.pending["w1:p1"], cur.n)
	}
}

// A look holds for the state it was taken in: another mark in the same
// round of "done" (a turn begun and ended between two snapshots) is
// "done", for one looked at by this watcher or kept from before it (its
// verdict of the log of then), till its log says otherwise.
func TestSeenLookHoldsForItsMark(t *testing.T) {
	sum := func(q *seenRounds, mark string) herdrSummary {
		s := seenSum(herdr.StatusDone, "")
		s.Workspaces[0].Agents[0].Mark = mark
		q.number(&s, false)
		return s
	}
	status := func(q *seenRounds, mark string) string {
		return q.withSeen(sum(q, mark)).Workspaces[0].Agents[0].Status
	}
	// Looked at by this watcher.
	q := newSeenRounds(nil, nil)
	q.changed("w1:p1", herdr.StatusDone, "")
	s := sum(q, "term_a#5")
	if !marked(q, "w1:p1", s.Workspaces[0].Agents[0].Round, "") || status(q, "term_a#5") != herdr.StatusIdle {
		t.Fatal("not looked at")
	}
	if got := status(q, "term_a#7"); got != herdr.StatusDone {
		t.Fatalf("another mark, looked at before it: %s", got)
	}
	a, g := q.toCheck()
	q.checked("w1:p1", a["w1:p1"], time.Now().UnixMilli()+5000, g) // (written since)
	if got := status(q, "term_a#7"); got != herdr.StatusDone {
		t.Fatalf("written since: %s", got)
	}
	// Kept from before, of another terminal, told by the first look's log.
	q = newSeenRounds(map[string]store.AgentSeen{"w1:p1": {At: 1000, Mark: "term_a#5"}}, nil)
	_, lg := q.wantsLook()
	q.lookedOver(map[string]int64{"w1:p1": 900}, lg)
	if got := status(q, "term_b#9"); got != herdr.StatusIdle {
		t.Fatalf("another terminal, not written since: %s", got)
	}
	if got := status(q, "term_b#11"); got != herdr.StatusDone {
		t.Fatalf("then another mark: %s", got)
	}
}

// A look whose state changed while it was kept is of before; and a read
// of the log that finds it written since overrules a look kept from
// before, told by an older read.
func TestSeenLookRaces(t *testing.T) {
	sum := func(q *seenRounds, mark string) herdrSummary {
		s := seenSum(herdr.StatusDone, "")
		s.Workspaces[0].Agents[0].Mark = mark
		q.number(&s, false)
		return s
	}
	var q *seenRounds
	q = newSeenRounds(nil, func(string, store.AgentSeen) error {
		sum(q, "term_a#7") // (a turn begun and ended while the look is kept)
		return nil
	})
	q.changed("w1:p1", herdr.StatusDone, "")
	s := sum(q, "term_a#5")
	if marked(q, "w1:p1", s.Workspaces[0].Agents[0].Round, "") {
		t.Fatal("a look of before taken")
	}
	if got := q.withSeen(sum(q, "term_a#7")).Workspaces[0].Agents[0].Status; got != herdr.StatusDone {
		t.Fatalf("after: %s", got)
	}
	// Kept from before, of another terminal: the first look (an older
	// read) says not written since; a read for its round says written.
	q = newSeenRounds(map[string]store.AgentSeen{"w1:p1": {At: 1000, Mark: "term_a#5"}}, nil)
	_, lg := q.wantsLook()
	q.lookedOver(map[string]int64{"w1:p1": 900}, lg)
	sum(q, "term_b#9")
	a, g := q.toCheck()
	q.checked("w1:p1", a["w1:p1"], 2000, g)
	if got := q.withSeen(sum(q, "term_b#9")).Workspaces[0].Agents[0].Status; got != herdr.StatusDone {
		t.Fatalf("written since, by a later read: %s", got)
	}
}
