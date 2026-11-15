package web

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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
	q.checked("w1:p1", seenCheck{n: round, mark: mark}, wrote, wrote, g)
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
	lo, hi := parseLogsWrote(string(out), []string{"p0", "p1"})
	if _, ok := lo["p0"]; ok || lo["p1"] != at.UnixMilli() || hi["p1"] != at.UnixMilli() {
		t.Fatalf("%v %v (written %d)", lo, hi, at.UnixMilli())
	}
	// (A Claude Code session's log, by its id, under the account's config;
	// one of an id not there; one not an id at all, never put in the command.)
	conf := t.TempDir()
	sid := "0f9a2c1e-1111-4222-8333-944455556666"
	if err := os.MkdirAll(filepath.Join(conf, "projects", "-home-x-proj"), 0o700); err != nil {
		t.Fatal(err)
	}
	cl := filepath.Join(conf, "projects", "-home-x-proj", sid+".jsonl")
	if err := os.WriteFile(cl, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(cl, at, at); err != nil {
		t.Fatal(err)
	}
	cmd := logsWroteCommand([]string{"claude:" + sid, "claude:0f9a2c1e-1111-4222-8333-000000000000", "claude:x; touch /tmp/no", a})
	if strings.Contains(cmd, "touch") {
		t.Fatalf("not an id, put in: %s", cmd)
	}
	c := exec.Command("sh", "-c", cmd)
	c.Env = append(os.Environ(), "CLAUDE_CONFIG_DIR="+conf)
	if out, err = c.Output(); err != nil {
		t.Fatal(err)
	}
	lo, _ = parseLogsWrote(string(out), []string{"c0", "c1", "c2", "p1"})
	if lo["c0"] != at.UnixMilli() || lo["p1"] != at.UnixMilli() || len(lo) != 2 {
		t.Fatalf("Claude Code's: %v (%q)", lo, out)
	}
	// (A host whose date tells seconds only: that second's first and last
	// millisecond.)
	lo, hi = parseLogsWrote("w=1794706675\nw=\n", []string{"p0", "p1"})
	if lo["p0"] != 1794706675000 || hi["p0"] != 1794706675999 || len(lo) != 1 {
		t.Fatalf("seconds: %v %v", lo, hi)
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
	q.checked("w1:p1", asked["w1:p1"], 900, 900, g)
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
	q.checked("w1:p1", asked["w1:p1"], 900, 900, g) // (asked under the old mark)
	if got := status("term_a#11"); got != herdr.StatusDone || len(pend(q)) != 1 {
		t.Fatalf("a read under the old mark took: %s %v", got, pend(q))
	}
	q.checked("w1:p1", again["w1:p1"], 2000, 2000, g)
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
	q2.checked("w1:p1", a2["w1:p1"], 900, 900, g2)
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

// A look stands, another mark in the same round of "done" (a turn begun
// and ended between two snapshots, or a state flicker), till its log is
// read and says written since: then "done", for one looked at by this
// watcher or kept from before it (told by an older read). Not read, or
// read and not written since: as it was. herdr begun again (another
// terminal): the look kept stands as before.
func TestSeenLookHoldsTillItsLogSays(t *testing.T) {
	sum := func(q *seenRounds, mark string) herdrSummary {
		s := seenSum(herdr.StatusDone, "")
		s.Workspaces[0].Agents[0].Mark = mark
		q.number(&s, false)
		return s
	}
	status := func(q *seenRounds, mark string) string {
		return q.withSeen(sum(q, mark)).Workspaces[0].Agents[0].Status
	}
	for _, c := range []struct {
		name  string
		wrote int64 // (from the look: +, written since)
		read  bool
		want  string
	}{
		{"not read", 0, false, herdr.StatusIdle},
		{"not known", 0, true, herdr.StatusIdle},
		{"not written since", -500, true, herdr.StatusIdle},
		{"written since", 5000, true, herdr.StatusDone},
	} {
		// Looked at by this watcher.
		var k store.AgentSeen
		q := newSeenRounds(nil, func(_ string, a store.AgentSeen) error { k = a; return nil })
		q.changed("w1:p1", herdr.StatusDone, "")
		s := sum(q, "term_a#5")
		if !marked(q, "w1:p1", s.Workspaces[0].Agents[0].Round, "") {
			t.Fatal("not looked at")
		}
		sum(q, "term_a#7")
		if c.read {
			a, g := q.toCheck()
			w := int64(0)
			if c.wrote != 0 {
				w = k.At + c.wrote
			}
			q.checked("w1:p1", a["w1:p1"], w, w, g)
		}
		if got := status(q, "term_a#7"); got != c.want {
			t.Errorf("looked at, %s: %s, want %s", c.name, got, c.want)
		}
		// Kept from before, of another terminal, told by the first look.
		q = newSeenRounds(map[string]store.AgentSeen{"w1:p1": {At: 1000, Mark: "term_a#5"}}, nil)
		_, lg := q.wantsLook()
		q.lookedOver(map[string]int64{"w1:p1": 900}, lg)
		if got := status(q, "term_b#9"); got != herdr.StatusIdle {
			t.Fatalf("another terminal, not written since: %s", got)
		}
		q.mu.Lock()
		q.pending = nil // (the first sight's read: not this one)
		q.mu.Unlock()
		sum(q, "term_b#11")
		if c.read {
			a, g := q.toCheck()
			w := int64(0)
			if c.wrote != 0 {
				w = 1000 + c.wrote
			}
			q.checked("w1:p1", a["w1:p1"], w, w, g)
		}
		if got := status(q, "term_b#11"); got != c.want {
			t.Errorf("kept, %s: %s, want %s", c.name, got, c.want)
		}
	}
	// herdr begun again while watched: the look kept stands, by its log.
	q := newSeenRounds(nil, nil)
	q.changed("w1:p1", herdr.StatusDone, "")
	s := sum(q, "term_a#5")
	if !marked(q, "w1:p1", s.Workspaces[0].Agents[0].Round, "") {
		t.Fatal("not looked at")
	}
	q.lostHerdr()
	_, lg := q.wantsLook()
	sum(q, "term_b#2") // (back)
	_, lg = q.wantsLook()
	q.lookedOver(map[string]int64{"w1:p1": 900}, lg)
	if got := status(q, "term_b#2"); got != herdr.StatusIdle {
		t.Fatalf("herdr begun again, not written since: %s", got)
	}
}

// A read of the log that finds it written since overrules a look kept
// from before, told by an older read; one in the second it was looked at
// (a clock in seconds) neither tells it looked at nor overrules it.
func TestSeenLookRaces(t *testing.T) {
	sum := func(q *seenRounds, mark string) herdrSummary {
		s := seenSum(herdr.StatusDone, "")
		s.Workspaces[0].Agents[0].Mark = mark
		q.number(&s, false)
		return s
	}
	var q *seenRounds
	// Kept from before, of another terminal: the first look (an older
	// read) says not written since; a read for its round says written.
	q = newSeenRounds(map[string]store.AgentSeen{"w1:p1": {At: 1000, Mark: "term_a#5"}}, nil)
	_, lg := q.wantsLook()
	q.lookedOver(map[string]int64{"w1:p1": 900}, lg)
	sum(q, "term_b#9")
	a, g := q.toCheck()
	q.checked("w1:p1", a["w1:p1"], 2000, 2000, g)
	if got := q.withSeen(sum(q, "term_b#9")).Workspaces[0].Agents[0].Status; got != herdr.StatusDone {
		t.Fatalf("written since, by a later read: %s", got)
	}
	for _, c := range []struct {
		lo, hi int64
		want   string
	}{
		{500, 1499, herdr.StatusIdle}, // (as the first look said: not overruled, not told)
		{0, 0, herdr.StatusIdle},
	} {
		q = newSeenRounds(map[string]store.AgentSeen{"w1:p1": {At: 1000, Mark: "term_a#5"}}, nil)
		_, lg = q.wantsLook()
		q.lookedOver(map[string]int64{"w1:p1": 900}, lg)
		sum(q, "term_b#9")
		a, g = q.toCheck()
		q.checked("w1:p1", a["w1:p1"], c.lo, c.hi, g)
		if got := q.withSeen(sum(q, "term_b#9")).Workspaces[0].Agents[0].Status; got != c.want || len(q.quiet) != 0 {
			t.Errorf("read %d..%d: %s, quiet %v", c.lo, c.hi, got, q.quiet)
		}
	}
}

// A host whose clock is told to the second: the log's earliest time is
// moved back by what the host's now may have left out.
func TestHostNowInSeconds(t *testing.T) {
	for out, want := range map[string]bool{
		"w=1\nnow=1794706675000\x00": true,
		"now=1794706675\x00":         true,
		"now=1794706675123\x00":      false,
		"no time":                    false,
	} {
		if got := hostNowInSeconds(out); got != want {
			t.Errorf("%q: %v", out, got)
		}
	}
}

// Looked at while its mark moved on (the look being kept): taken, and its
// log read for.
func TestSeenLookedAsItMoved(t *testing.T) {
	var q *seenRounds
	sum := func(mark string) herdrSummary {
		s := seenSum(herdr.StatusDone, "")
		s.Workspaces[0].Agents[0].Mark = mark
		q.number(&s, false)
		return s
	}
	q = newSeenRounds(nil, func(string, store.AgentSeen) error {
		sum("term_a#7")
		return nil
	})
	q.changed("w1:p1", herdr.StatusDone, "")
	s := sum("term_a#5")
	round := s.Workspaces[0].Agents[0].Round
	if !marked(q, "w1:p1", round, "") {
		t.Fatal("not taken")
	}
	if p := pend(q); p["w1:p1"].n != round || p["w1:p1"].mark != "term_a#7" {
		t.Fatalf("not read for: %v", p)
	}
}

// A "finished" alert: none where the agent's log was last written well
// before it was told done (herdr telling a status again); one where it was
// written as the turn ended, or when that is not known.
func TestDoneQuiet(t *testing.T) {
	at := int64(1794706675000)
	for _, c := range []struct {
		wrote int64
		quiet bool
	}{
		{at - 2*24*3600*1000, true}, // (finished days ago, looked at since: 09:13, 11-15)
		{at - 61_000, true},
		{at - 3_000, false}, // (its turn's end, written as it ended)
		{at + 500, false},
		{0, false}, // (not known)
	} {
		if got := doneQuiet(c.wrote, at); got != c.quiet {
			t.Errorf("written %d ms before: quiet %v", at-c.wrote, got)
		}
	}
}
