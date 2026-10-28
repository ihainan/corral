package convo

import "testing"

// A subagent's own log: all of its rows are of its side chain, kept by
// the subagent's reader (a session's reader leaves them out). A call that
// sent one off knows it by the id Claude Code's account of it gives.
func TestClaudeSubagent(t *testing.T) {
	rows := lines(
		`{"type":"user","uuid":"s1","isSidechain":true,"agentId":"ab2de014094f7b90b","message":{"role":"user","content":"Count the files"}}`,
		`{"type":"assistant","uuid":"s2","isSidechain":true,"agentId":"ab2de014094f7b90b","message":{"role":"assistant","content":[{"type":"text","text":"There are 3 files."}]}}`,
	)
	main, sub := NewClaude(), NewClaudeSub()
	main.Feed(rows)
	sub.Feed(rows)
	if n := len(main.Since(0)); n != 0 {
		t.Fatalf("a session's reader kept %d side-chain entries", n)
	}
	es := sub.Since(0)
	if len(es) != 2 || es[0].Role != RoleUser || es[1].Parts[0].Text != "There are 3 files." {
		t.Fatalf("the subagent's reader: %+v", es)
	}
	c := NewClaude()
	c.Feed(lines(
		`{"type":"assistant","uuid":"a1","message":{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"Agent","input":{"description":"Count the files","prompt":"Count"}},{"type":"tool_use","id":"t2","name":"Agent","input":{"description":"x","prompt":"y"}}]}}`,
		`{"type":"user","uuid":"r1","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"started"}]},"toolUseResult":{"isAsync":true,"status":"async_launched","agentId":"ab2de014094f7b90b"}}`,
		`{"type":"user","uuid":"r2","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t2","content":"started"}]},"toolUseResult":{"agentId":"../../etc"}}`,
	))
	parts := c.Since(0)[0].Parts
	if parts[0].Tool.AgentID != "ab2de014094f7b90b" || parts[1].Tool.AgentID != "" {
		t.Fatalf("agent ids %q %q", parts[0].Tool.AgentID, parts[1].Tool.AgentID)
	}
}

func TestSubagentID(t *testing.T) {
	for id, ok := range map[string]bool{"ab2de014094f7b90b": true, "a1b2c3": true, "abc": false, "AB2DE014": false, "ab/de014": false, "../x0000": false} {
		if SubagentID(id) != ok {
			t.Errorf("%q", id)
		}
	}
}
