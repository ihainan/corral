package web

import "testing"

func TestParseAgentSessions(t *testing.T) {
	raw := `[
		{"id":"6490c7d4-0626-4ed8-a158-da789586d5b1","kind":"agy","dir":"/home/u/proj","ms":1795000000000,"title":"Agy task","first":"preview"},
		{"id":"ses_e79a6b922ffefvqldnQZeW0t7K","kind":"opencode","dir":"/home/u/oc","ms":1794000000000,"title":"OpenCode task","first":""},
		{"id":"not-a-uuid","kind":"claude","dir":"/home/u/bad","ms":1794000000000,"title":"skip"},
		{"id":"6490c7d4-0626-4ed8-a158-da789586d5b2","kind":"claude","dir":"relative/path","ms":1794000000000,"title":"skip bad dir"}
	]`
	got := parseAgentSessions([]byte(raw))
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2: %+v", len(got), got)
	}
	if got[0].Kind != "agy" || got[0].Dir != "/home/u/proj" || got[0].Title != "Agy task" {
		t.Fatalf("got[0] = %+v", got[0])
	}
	if got[1].Kind != "opencode" || got[1].ID != "ses_e79a6b922ffefvqldnQZeW0t7K" || got[1].Dir != "/home/u/oc" {
		t.Fatalf("got[1] = %+v", got[1])
	}
}

func TestAgentArgsOpenCodeResume(t *testing.T) {
	args := agentArgs("opencode", agentStart{resume: "ses_e79a6b922ffefvqldnQZeW0t7K"})
	if len(args) != 2 || args[0] != "--session" || args[1] != "ses_e79a6b922ffefvqldnQZeW0t7K" {
		t.Fatalf("opencode resume args = %v", args)
	}
}
