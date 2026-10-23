package web

import (
	"testing"
	"time"
)

func TestAgentTimes(t *testing.T) {
	a := &agentTimes{at: map[string]time.Time{}}
	first := a.of("w1:p1") // (first seen: now, and kept)
	time.Sleep(5 * time.Millisecond)
	if got := a.of("w1:p1"); got != first {
		t.Fatalf("first-seen time moved: %d then %d", first, got)
	}
	a.changed("w1:p1", false)
	if got := a.of("w1:p1"); got <= first {
		t.Fatalf("a change did not move it: %d then %d", first, got)
	}
	a.changed("w1:p1", true)
	if _, ok := a.at["w1:p1"]; ok {
		t.Fatal("a pane gone is still kept")
	}
}
