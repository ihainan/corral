package web

import (
	"testing"
	"time"

	"github.com/ihainan/corral/internal/herdr"
)

// A pane the gateway never saw change is shown by when its agent last
// wrote to its log; one it did see change keeps that time.
func TestPaneSinceFallsBackToTheLog(t *testing.T) {
	times := &agentTimes{at: map[string]agentTime{}}
	p := herdr.Pane{ID: "%1", AgentStatus: herdr.StatusIdle}
	// The first summary after the gateway began watching: what it finds
	// has been so since a time it does not know.
	if got := paneSince(p, times, false); got != 0 {
		t.Fatalf("with nothing known: %d, want 0", got)
	}
	times.prime()
	if !times.due(time.Now()) {
		t.Fatal("a pane with no time of its own does not ask for a look")
	}
	times.found(map[string]int64{"%1": 1700000000000}, time.Now())
	if got := paneSince(p, times, false); got != 1700000000000 {
		t.Errorf("after the look: %d, want the log's time", got)
	}
	// A log goes on being written: having a time once is no reason to
	// stop looking.
	if !times.due(time.Now().Add(2 * lookEvery)) {
		t.Error("a pane shown by its log stopped asking for a look")
	}
	times.found(map[string]int64{"%1": 1700000000000}, time.Now())
	// Lost herdr: the status is of a snapshot no longer current, and a
	// log's time is of what the pane does now, not of that status.
	if got := paneSince(p, times, true); got != 0 {
		t.Errorf("with herdr lost: %d, want no time at all", got)
	}
	// Seen to change: that time, not the log's.
	times.changed("%1", herdr.StatusWorking)
	p.AgentStatus = herdr.StatusWorking
	if got := paneSince(p, times, false); got == 1700000000000 || got == 0 {
		t.Errorf("after a change seen: %d, want when it changed", got)
	}
}

// A look is made at most once a minute, and only while a pane needs one.
func TestAgentTimesLookRate(t *testing.T) {
	times := &agentTimes{at: map[string]agentTime{}}
	now := time.Now()
	if times.due(now) {
		t.Error("a look with no pane needing one")
	}
	times.lastWrote("%1")
	if !times.due(now) {
		t.Fatal("no look for a pane that needs one")
	}
	if times.due(now) {
		t.Error("a second look while one is under way")
	}
	times.found(nil, now)
	times.lastWrote("%1")
	if times.due(now.Add(30*time.Second - time.Millisecond)) {
		t.Error("a look again within the minute")
	}
	if !times.due(now.Add(2 * lookEvery)) {
		t.Error("no look after the minute")
	}
}
