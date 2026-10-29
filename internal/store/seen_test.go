package store

import (
	"context"
	"testing"
)

// What was looked at: by owner, host and server; a look noted out of order
// does not undo a later one; the old are let go.
func TestAgentsSeen(t *testing.T) {
	ctx := context.Background()
	s, err := OpenMemory()
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(s.SetAgentSeen(ctx, 1, 2, "a", "w1:p1", AgentSeen{"s1", 2000}))
	must(s.SetAgentSeen(ctx, 1, 2, "a", "w1:p1", AgentSeen{"s0", 1500})) // (late, older)
	must(s.SetAgentSeen(ctx, 1, 2, "a", "w1:p2", AgentSeen{"s2", 100}))
	must(s.SetAgentSeen(ctx, 1, 2, "b", "w1:p1", AgentSeen{"x", 2000}))
	must(s.SetAgentSeen(ctx, 9, 2, "a", "w1:p1", AgentSeen{"y", 2000}))
	got, err := s.AgentsSeen(ctx, 1, 2, "a", 1000)
	must(err)
	if len(got) != 1 || got["w1:p1"] != (AgentSeen{"s1", 2000}) {
		t.Fatalf("got %+v", got)
	}
	must(s.SetAgentSeen(ctx, 1, 2, "a", "w1:p1", AgentSeen{"s3", 3000}))
	if got, _ := s.AgentsSeen(ctx, 1, 2, "a", 0); got["w1:p1"] != (AgentSeen{"s3", 3000}) || len(got) != 1 {
		t.Fatalf("later: %+v", got)
	}
	if got, _ := s.AgentsSeen(ctx, 1, 2, "b", 0); got["w1:p1"].Session != "x" {
		t.Fatalf("another server: %+v", got)
	}
}
