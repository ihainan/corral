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
		if p := q.toCheck(); p["w1:p1"] != round {
			t.Fatalf("not to be read: %v (round %d)", p, round)
		}
		q.checked("w1:p1", round, c.wrote)
		if got := q.withSeen(s).Workspaces[0].Agents[0].Status; got != c.want {
			t.Errorf("wrote %d: %s, want %s", c.wrote, got, c.want)
		}
		if len(q.toCheck()) != 0 {
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
		if a.Status != herdr.StatusDone || q.toCheck()["w1:p1"] != a.Round {
			t.Fatalf("%s and back, before its log is read: %+v %v", away, a, q.toCheck())
		}
		q.checked("w1:p1", a.Round, k.At-500)
		if got := told(q); got.Status != herdr.StatusIdle || got.Round != a.Round {
			t.Errorf("%s and back, not written since: %+v", away, got)
		}
		// (And again, written since: "done".)
		q.changed("w1:p1", away, "")
		q.changed("w1:p1", herdr.StatusDone, "")
		b := told(q)
		q.checked("w1:p1", b.Round, k.At+1)
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
	if a := told(q); a.Status != herdr.StatusDone || len(q.toCheck()) != 0 {
		t.Fatalf("away long: %+v %v", a, q.toCheck())
	}
	// Another agent session: not the one looked at.
	q, k := looked()
	q.changed("w1:p1", herdr.StatusWorking, "")
	q.changed("w1:p1", herdr.StatusDone, "s2")
	if len(q.toCheck()) != 0 {
		t.Fatalf("another session read for: %v", q.toCheck())
	}
	// A read for a round over says nothing of the next.
	q, k = looked()
	q.changed("w1:p1", herdr.StatusUnknown, "")
	q.changed("w1:p1", herdr.StatusDone, "")
	a := told(q)
	q.changed("w1:p1", herdr.StatusWorking, "")
	q.changed("w1:p1", herdr.StatusDone, "")
	b := told(q)
	q.checked("w1:p1", a.Round, k.At-500)
	if got := told(q); got.Status != herdr.StatusDone || got.Round != b.Round {
		t.Fatalf("a read for the round over took: %+v", got)
	}
	// Gone: forgotten.
	q.changed("w1:p1", herdr.StatusGone, "")
	if len(q.toCheck()) != 0 || len(q.last) != 0 || len(q.quiet) != 0 {
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
