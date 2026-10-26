package convo

import (
	"encoding/json"
	"strings"
	"testing"
)

func codexLine(t *testing.T, typ string, payload map[string]any) string {
	b, err := json.Marshal(map[string]any{"timestamp": "2026-10-26T01:00:00.000Z", "type": typ, "payload": payload})
	if err != nil {
		t.Fatal(err)
	}
	return string(b) + "\n"
}

func codexSession(t *testing.T) []string {
	patch := "*** Begin Patch\n*** Update File: /p/a.go\n@@\n-old\n+new\n+more\n*** Add File: /p/b.go\n+package p\n*** End Patch"
	lit, _ := json.Marshal(patch)
	return []string{
		codexLine(t, "session_meta", map[string]any{"id": "s1", "git": map[string]any{"branch": "main"}}),
		codexLine(t, "turn_context", map[string]any{"model": "gpt-6.1-sol", "approval_policy": "on-request"}),
		codexLine(t, "event_msg", map[string]any{"type": "task_started"}),
		// (What Codex sends on the person's behalf: not theirs.)
		codexLine(t, "response_item", map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "<environment_context>x</environment_context>"}}}),
		codexLine(t, "event_msg", map[string]any{"type": "item_completed", "item": map[string]any{"type": "UserMessage", "id": "u1", "content": []any{map[string]any{"type": "text", "text": "跑一下测试"}}}}),
		codexLine(t, "response_item", map[string]any{"type": "reasoning", "id": "r1", "summary": []any{map[string]any{"text": "**Testing**"}}}),
		codexLine(t, "response_item", map[string]any{"type": "message", "id": "m1", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": "先跑测试。"}}}),
		codexLine(t, "response_item", map[string]any{"type": "custom_tool_call", "id": "c1", "call_id": "call_1", "name": "exec", "input": `const r = await tools.exec_command({cmd:"go test ./... -run \"A|B\"","workdir":"/p"}); text(r.output);`}),
		codexLine(t, "response_item", map[string]any{"type": "custom_tool_call_output", "call_id": "call_1", "output": []any{map[string]any{"type": "input_text", "text": "Script completed\nWall time 1 seconds\nOutput:\n"}, map[string]any{"type": "input_text", "text": "ok p 0.1s\n"}}}),
		codexLine(t, "response_item", map[string]any{"type": "custom_tool_call", "id": "c2", "call_id": "call_2", "name": "exec", "input": "const patch = " + string(lit) + "; await tools.apply_patch(patch);"}),
		codexLine(t, "event_msg", map[string]any{"type": "token_count", "info": map[string]any{"last_token_usage": map[string]any{"input_tokens": 12345}}}),
	}
}

func TestCodexTurn(t *testing.T) {
	c := NewCodex()
	c.Feed([]byte(strings.Join(codexSession(t), "")))
	if c.Status() != "busy" {
		t.Errorf("status %q", c.Status())
	}
	c.Feed([]byte(codexLine(t, "response_item", map[string]any{"type": "custom_tool_call_output", "call_id": "call_2", "output": []any{map[string]any{"type": "input_text", "text": "Script completed\nWall time 0 seconds\nOutput:\nDone"}}}) +
		codexLine(t, "response_item", map[string]any{"type": "message", "id": "m2", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": "好了。"}}}) +
		codexLine(t, "event_msg", map[string]any{"type": "task_complete"})))
	es := c.Since(0)
	var roles, kinds []string
	for _, e := range es {
		roles = append(roles, e.Role)
		for _, p := range e.Parts {
			k := p.Kind
			if p.Tool != nil {
				k += ":" + p.Tool.Name
			}
			kinds = append(kinds, k)
		}
	}
	if strings.Join(roles, ",") != "user,assistant,assistant,assistant,assistant,assistant" {
		t.Fatalf("roles %v", roles)
	}
	if strings.Join(kinds, ",") != "text,thinking,text,tool:Bash,tool:Edit,tool:Write,text" {
		t.Fatalf("kinds %v", kinds)
	}
	bash := es[3].Parts[0].Tool
	if bash.Detail != `go test ./... -run "A|B"` || !bash.Done || bash.Result != "ok p 0.1s\n" || bash.Error {
		t.Errorf("bash %+v", bash)
	}
	edit, write := es[4].Parts[0].Tool, es[4].Parts[1].Tool
	if edit.Detail != "/p/a.go" || edit.Add != 2 || edit.Del != 1 || len(edit.Patch) != 1 || !edit.Done || write.Detail != "/p/b.go" || write.Add != 1 {
		t.Errorf("patch %+v / %+v", edit, write)
	}
	m := c.Meta()
	if m.Model != "gpt-6.1-sol" || m.Mode != "on-request" || m.Branch != "main" || m.Context != 12345 {
		t.Errorf("meta %+v", m)
	}
	if c.Title() != "跑一下测试" || c.Status() != "idle" {
		t.Errorf("title %q status %q", c.Title(), c.Status())
	}
}

// A call read before (Prepend), its output in the part read first: filled in.
func TestCodexPrepend(t *testing.T) {
	lines := codexSession(t)
	newer := NewCodex()
	newer.Feed([]byte(codexLine(t, "response_item", map[string]any{"type": "custom_tool_call_output", "call_id": "call_2", "output": []any{map[string]any{"type": "input_text", "text": "Done"}}}) +
		codexLine(t, "event_msg", map[string]any{"type": "turn_aborted", "reason": "interrupted"})))
	older := NewCodex()
	older.Feed([]byte(strings.Join(lines, "")))
	newer.Prepend(older)
	es := newer.Since(0)
	if len(es) != 6 || es[0].Role != "user" || es[len(es)-1].Role != RoleNote {
		t.Fatalf("entries %+v", es)
	}
	if !es[4].Parts[0].Tool.Done {
		t.Errorf("the patch's output not filled in: %+v", es[4].Parts[0].Tool)
	}
	if newer.Title() != "跑一下测试" || newer.Meta().Model != "gpt-6.1-sol" || newer.Status() != "idle" {
		t.Errorf("title %q meta %+v status %q", newer.Title(), newer.Meta(), newer.Status())
	}
	for i := 1; i < len(es); i++ {
		if es[i].Seq <= es[i-1].Seq {
			t.Fatalf("order: %d then %d", es[i-1].Seq, es[i].Seq)
		}
	}
}

// A compaction: its summary shows, how full the context is is not known.
func TestCodexCompacted(t *testing.T) {
	c := NewCodex()
	c.Feed([]byte(codexLine(t, "event_msg", map[string]any{"type": "token_count", "info": map[string]any{"last_token_usage": map[string]any{"input_tokens": 200000}}}) +
		codexLine(t, "compacted", map[string]any{"message": "Summary of the work so far."})))
	es := c.Since(0)
	if len(es) != 1 || es[0].Role != RoleSummary || c.Meta().Context != 0 {
		t.Fatalf("entries %+v meta %+v", es, c.Meta())
	}
}

// review 236: a template literal's patch, apply_patch on its own, two
// commands in one script (one tool, the output of both), a failed script.
func TestCodexTools(t *testing.T) {
	tmpl := "const p = `*** Begin Patch\n*** Update File: /p/t.go\n@@\n-a\n+b\n*** End Patch`; await tools.apply_patch(p);"
	if ts := codexTools("exec", tmpl); len(ts) != 1 || ts[0].Name != "Edit" || ts[0].Detail != "/p/t.go" || ts[0].Add != 1 || ts[0].Del != 1 {
		t.Errorf("template: %+v", ts)
	}
	raw := "*** Begin Patch\n*** Add File: /p/n.go\n+package p\n*** End Patch"
	if ts := codexTools("apply_patch", raw); len(ts) != 1 || ts[0].Name != "Write" || ts[0].Detail != "/p/n.go" {
		t.Errorf("apply_patch: %+v", ts)
	}
	two := `await tools.exec_command({cmd:"make a"}); await tools.exec_command({cmd:"make b"});`
	if ts := codexTools("exec", two); len(ts) != 1 || ts[0].Detail != "make a\nmake b" {
		t.Errorf("two commands: %+v", ts)
	}
	c := NewCodex()
	c.Feed([]byte(codexLine(t, "response_item", map[string]any{"type": "custom_tool_call", "call_id": "k", "name": "exec", "input": two}) +
		codexLine(t, "response_item", map[string]any{"type": "custom_tool_call_output", "call_id": "k", "output": []any{map[string]any{"type": "input_text", "text": "Script failed\nWall time 0 seconds\nOutput:\nReferenceError: x"}}})))
	if tool := c.Since(0)[0].Parts[0].Tool; !tool.Done || !tool.Error || tool.Result != "ReferenceError: x" {
		t.Errorf("failed: %+v", tool)
	}
}

// Three parts read newest first: a call in the oldest, its output in the middle one.
func TestCodexPrependTwice(t *testing.T) {
	call := codexLine(t, "response_item", map[string]any{"type": "custom_tool_call", "call_id": "z", "name": "exec", "input": `await tools.exec_command({cmd:"ls"})`})
	out := codexLine(t, "response_item", map[string]any{"type": "custom_tool_call_output", "call_id": "z", "output": []any{map[string]any{"type": "input_text", "text": "a b"}}})
	last := codexLine(t, "response_item", map[string]any{"type": "message", "id": "m", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": "done"}}})
	c, mid, old := NewCodex(), NewCodex(), NewCodex()
	c.Feed([]byte(last))
	mid.Feed([]byte(out))
	c.Prepend(mid)
	old.Feed([]byte(call))
	c.Prepend(old)
	if tool := c.Since(0)[0].Parts[0].Tool; !tool.Done || tool.Result != "a b" {
		t.Errorf("not filled in: %+v", tool)
	}
}
