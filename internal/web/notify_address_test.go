package web

import "testing"

// An alert about an agent opens the agents page at it (its pane's ":" kept
// as it is read back, see AgentsChat's readPick).
func TestAgentAddress(t *testing.T) {
	if got, want := agentAddress("0123abcd", "w27K:p1"), "/agents?s=0123abcd&pane=w27K%3Ap1"; got != want {
		t.Fatalf("%s, want %s", got, want)
	}
}
