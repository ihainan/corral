package convo

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func lines(rows ...string) []byte { return []byte(strings.Join(rows, "\n") + "\n") }

func TestClaudeTurns(t *testing.T) {
	c := NewClaude()
	c.Feed(lines(
		`{"type":"mode","mode":"default"}`,
		`{"type":"user","uuid":"u1","timestamp":"2026-10-23T01:00:00Z","message":{"role":"user","content":"Why is the table broken?"}}`,
		`{"type":"user","uuid":"m1","isMeta":true,"message":{"content":"<system-reminder>hidden</system-reminder>"}}`,
		`{"type":"assistant","uuid":"a1","message":{"content":[{"type":"thinking","thinking":"Let me look."}]}}`,
		`{"type":"assistant","uuid":"a2","message":{"content":[{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"ls -la","description":"List files"}}]}}`,
		`{"type":"assistant","uuid":"a3","isSidechain":true,"message":{"content":[{"type":"text","text":"subagent talk"}]}}`,
		`not json`,
		`{"type":"assistant","uuid":"a4","message":{"content":[{"type":"text","text":"**Found it.**"}]}}`,
	))
	got := c.Since(0)
	if len(got) != 4 {
		t.Fatalf("entries: %+v", got)
	}
	if got[0].Role != RoleUser || got[0].Parts[0].Text != "Why is the table broken?" || got[0].Time == "" {
		t.Fatalf("user turn: %+v", got[0])
	}
	if got[1].Parts[0].Kind != KindThinking {
		t.Fatalf("thinking: %+v", got[1])
	}
	tool := got[2].Parts[0].Tool
	if tool == nil || tool.Name != "Bash" || tool.Title != "List files" || tool.Detail != "ls -la" || tool.Done {
		t.Fatalf("tool call: %+v", got[2].Parts[0])
	}
	if got[3].Parts[0].Text != "**Found it.**" {
		t.Fatalf("answer: %+v", got[3])
	}
	if c.Skipped < 3 { // mode, sidechain, not json
		t.Fatalf("skipped %d", c.Skipped)
	}

	// The result arrives later: only the call's entry changes.
	rev := c.Rev()
	c.Feed(lines(`{"type":"user","uuid":"r1","message":{"content":[{"type":"tool_result","tool_use_id":"t1","content":"a\nb\n\u001b[31mc\u001b[0m"}]}}`))
	ch := c.Since(rev)
	if len(ch) != 1 || ch[0].ID != "a2" {
		t.Fatalf("changed: %+v", ch)
	}
	tl := ch[0].Parts[0].Tool
	if !tl.Done || tl.Result != "a\nb\nc" || tl.ResultLines != 3 || tl.Error {
		t.Fatalf("result: %+v", tl)
	}
	if len(c.Since(c.Rev())) != 0 {
		t.Fatal("nothing changed after the last revision")
	}
}

func TestClaudeEnvelopes(t *testing.T) {
	c := NewClaude()
	c.Feed(lines(
		`{"type":"user","uuid":"1","message":{"content":"<command-name>/model</command-name>\n<command-message>model</command-message>\n<command-args>opus</command-args>"}}`,
		`{"type":"user","uuid":"2","message":{"content":"<local-command-stdout>Set model to opus</local-command-stdout>"}}`,
		`{"type":"user","uuid":"3","message":{"content":"<local-command-caveat>Caveat: ...</local-command-caveat>"}}`,
		`{"type":"user","uuid":"4","message":{"content":"<bash-input>git status</bash-input>"}}`,
		`{"type":"user","uuid":"5","promptSource":"system","message":{"content":"<task-notification>\n<task-id>b1</task-id>\n<status>completed</status>\n<summary>Background command \"tests\" completed</summary>\n</task-notification>"}}`,
		`{"type":"user","uuid":"6","promptSource":"system","message":{"content":"Run the nightly check"}}`,
		`{"type":"user","uuid":"7","message":{"content":"<div> is what I mean"}}`,
		`{"type":"user","uuid":"8","isCompactSummary":true,"message":{"content":"This session is being continued..."}}`,
		`{"type":"user","uuid":"9","message":{"content":[{"type":"text","text":"Look at this"},{"type":"image","source":{"type":"base64","data":"AAAA"}}]}}`,
		`{"type":"attachment","uuid":"10","attachment":{"type":"queued_command","commandMode":"prompt","prompt":"also check the docs"}}`,
		`{"type":"attachment","uuid":"11","attachment":{"type":"queued_command","commandMode":"prompt","isMeta":true,"prompt":"<agent-message>x</agent-message>"}}`,
		`{"type":"system","uuid":"12","subtype":"local_command","content":"<command-name>/clear</command-name>\n<command-args></command-args>"}`,
	))
	want := []struct{ role, text string }{
		{RoleUser, "/model opus"},
		{RoleNote, "Set model to opus"},
		{RoleUser, "! git status"},
		{RoleNote, `Background command "tests" completed`},
		{RoleNote, "Run the nightly check"},
		{RoleUser, "<div> is what I mean"},
		{RoleSummary, "This session is being continued..."},
		{RoleUser, "Look at this"},
		{RoleUser, "also check the docs"},
		{RoleUser, "/clear"},
	}
	got := c.Since(0)
	if len(got) != len(want) {
		for _, e := range got {
			t.Logf("%s %+v", e.Role, e.Parts)
		}
		t.Fatalf("%d entries, want %d", len(got), len(want))
	}
	for i, w := range want {
		if got[i].Role != w.role || got[i].Parts[0].Text != w.text {
			t.Errorf("entry %d: %s %q, want %s %q", i, got[i].Role, got[i].Parts[0].Text, w.role, w.text)
		}
	}
	if img := got[7].Parts; len(img) != 2 || img[1].Kind != KindImage || img[1].Text != "" {
		t.Errorf("image part: %+v", img)
	}
}

func TestClaudeToolInputs(t *testing.T) {
	c := NewClaude()
	c.Feed(lines(
		`{"type":"assistant","uuid":"a","message":{"content":[`+
			`{"type":"tool_use","id":"1","name":"Edit","input":{"file_path":"/w/a.go","old_string":"x := 1","new_string":"x := 2"}},`+
			`{"type":"tool_use","id":"2","name":"Grep","input":{"pattern":"TODO","path":"/w"}},`+
			`{"type":"tool_use","id":"3","name":"Agent","input":{"description":"Find callers","prompt":"Look for callers of f","subagent_type":"Explore"}},`+
			`{"type":"tool_use","id":"4","name":"TodoWrite","input":{"todos":[{"content":"one","status":"completed"},{"content":"two","status":"pending"}]}},`+
			`{"type":"tool_use","id":"5","name":"mcp__x__fetch","input":{"z":1,"url":"https://example.com"}},`+
			`{"type":"tool_use","id":"6","name":"Bash","input":"not an object"}]}}`,
		`{"type":"user","uuid":"r","message":{"content":[{"type":"tool_result","tool_use_id":"1","is_error":true,"content":[{"type":"text","text":"String not found"}]},{"type":"tool_result","tool_use_id":"unknown","content":"x"}]}}`,
	))
	p := c.Since(0)[0].Parts
	if e := p[0].Tool; e.Detail != "/w/a.go" || e.Old != "x := 1" || e.New != "x := 2" || !e.Done || !e.Error || e.Result != "String not found" {
		t.Errorf("edit: %+v", e)
	}
	if g := p[1].Tool; g.Detail != "TODO  /w" {
		t.Errorf("grep: %+v", g)
	}
	if a := p[2].Tool; a.Title != "Find callers" || a.Detail != "Look for callers of f" {
		t.Errorf("agent: %+v", a)
	}
	if td := p[3].Tool; td.Detail != "[x] one\n[ ] two" {
		t.Errorf("todos: %q", td.Detail)
	}
	// (An MCP server's tool says which of its tools it was: the whole name
	// is too long for the line, and the server's part of it says nothing.)
	if m := p[4].Tool; m.Detail != "fetch https://example.com" {
		t.Errorf("mcp: %+v", m)
	}
	if b := p[5].Tool; b.Detail != "" || b.Name != "Bash" {
		t.Errorf("bad input: %+v", b)
	}
}

func TestClaudeBounds(t *testing.T) {
	c := NewClaude()
	long := strings.Repeat("字", maxText) // 3 bytes each
	c.Feed(lines(`{"type":"user","uuid":"u","message":{"content":"` + long + `"}}`))
	p := c.Since(0)[0].Parts[0]
	if !p.Truncated || len(p.Text) > maxText || !strings.HasPrefix(long, p.Text) {
		t.Fatalf("long text: %d bytes, truncated %v", len(p.Text), p.Truncated)
	}
	// Old entries are dropped past the limit, and their pending calls too.
	c = NewClaude()
	var b strings.Builder
	b.WriteString(`{"type":"assistant","uuid":"first","message":{"content":[{"type":"tool_use","id":"old","name":"Bash","input":{}}]}}` + "\n")
	for i := 0; i < maxEntries+10; i++ {
		b.WriteString(`{"type":"user","uuid":"x","message":{"content":"hi"}}` + "\n")
	}
	c.Feed([]byte(b.String()))
	if n := len(c.Since(0)); n != maxEntries {
		t.Fatalf("kept %d", n)
	}
	if _, ok := c.tools["old"]; ok {
		t.Fatal("dropped call still pending")
	}
	if c.First() != 12 {
		t.Fatalf("first seq %d", c.First())
	}
	// Control characters and bad UTF-8 do not reach the view.
	c = NewClaude()
	c.Feed(lines(`{"type":"user","uuid":"u","message":{"content":"a\u0007b\u202ec\r\nd"}}`, "{\"type\":\"user\",\"uuid\":\"v\",\"message\":{\"content\":\"\xff\xfeok\"}}"))
	got := c.Since(0)
	if got[0].Parts[0].Text != "abc\nd" {
		t.Errorf("controls: %q", got[0].Parts[0].Text)
	}
	if len(got) > 1 && strings.ContainsRune(got[1].Parts[0].Text, 0xff) {
		t.Errorf("bad utf-8 kept")
	}
}

// TestClaudeRealLogs reads real logs when CONVO_REAL_LOGS names a glob of
// them (not part of the normal run: such logs are private).
func TestClaudeRealLogs(t *testing.T) {
	pattern := os.Getenv("CONVO_REAL_LOGS")
	if pattern == "" {
		t.Skip("CONVO_REAL_LOGS not set")
	}
	files, _ := filepath.Glob(pattern)
	var entries, tools, done, skipped int
	roles := map[string]int{}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		c := NewClaude()
		// In pieces, as the gateway reads it.
		for len(b) > 0 {
			n := min(len(b), 1<<20)
			if i := bytes.LastIndexByte(b[:n], '\n'); i >= 0 {
				n = i + 1
			} else if j := bytes.IndexByte(b, '\n'); j >= 0 {
				n = j + 1
			} else {
				n = len(b)
			}
			c.Feed(b[:n])
			b = b[n:]
		}
		for _, e := range c.Since(0) {
			entries++
			roles[e.Role]++
			for _, p := range e.Parts {
				if p.Tool != nil {
					tools++
					if p.Tool.Done {
						done++
					}
				}
			}
		}
		skipped += c.Skipped
	}
	t.Logf("%d files: %d entries %v, %d tool calls (%d with results), %d lines skipped", len(files), entries, roles, tools, done, skipped)
}

func TestClaudeTrimKeepsReusedToolID(t *testing.T) {
	c := NewClaude()
	var b strings.Builder
	call := `{"type":"assistant","uuid":"a","message":{"content":[{"type":"tool_use","id":"same","name":"Bash","input":{}}]}}` + "\n"
	b.WriteString(call)
	for i := 0; i < maxEntries-1; i++ {
		b.WriteString(`{"type":"user","uuid":"x","message":{"content":"hi"}}` + "\n")
	}
	b.WriteString(call) // the id again: the first call is dropped as old after this
	c.Feed([]byte(b.String()))
	c.Feed(lines(`{"type":"user","uuid":"r","message":{"content":[{"type":"tool_result","tool_use_id":"same","content":"done"}]}}`))
	got := c.Since(0)
	last := got[len(got)-1].Parts[0].Tool
	if last == nil || !last.Done || last.Result != "done" {
		t.Fatalf("the kept call lost its result: %+v", last)
	}
}

func TestClaudeTitle(t *testing.T) {
	c := NewClaude()
	c.Feed(lines(
		`{"type":"ai-title","aiTitle":"Fix the login","sessionId":"s"}`,
		`{"type":"ai-title","aiTitle":"Rate-limit the login","sessionId":"s"}`,
	))
	if c.Title() != "Rate-limit the login" || c.Skipped != 0 {
		t.Fatalf("title %q, skipped %d", c.Title(), c.Skipped)
	}
	c.Feed(lines(`{"type":"custom-title","customTitle":"login limits"}`, `{"type":"ai-title","aiTitle":"Later"}`))
	if c.Title() != "login limits" {
		t.Fatalf("the person's title: %q", c.Title())
	}
}

func TestClaudeEditLines(t *testing.T) {
	c := NewClaude()
	c.Feed(lines(
		// From the patch Claude Code reports.
		`{"type":"assistant","uuid":"a1","message":{"content":[{"type":"tool_use","id":"e1","name":"Edit","input":{"file_path":"/w/a.go","old_string":"x\ny","new_string":"x\nz\nw"}}]}}`,
		`{"type":"user","uuid":"r1","toolUseResult":{"structuredPatch":[{"lines":[" x","-y","+z","+w"," q"]},{"lines":["-k"]}]},"message":{"content":[{"type":"tool_result","tool_use_id":"e1","content":"ok"}]}}`,
		// A file created: all its lines.
		`{"type":"assistant","uuid":"a2","message":{"content":[{"type":"tool_use","id":"w1","name":"Write","input":{"file_path":"/w/b.go","content":"1\n2\n3\n"}}]}}`,
		`{"type":"user","uuid":"r2","toolUseResult":{"type":"create","content":"1\n2\n3\n","structuredPatch":[]},"message":{"content":[{"type":"tool_result","tool_use_id":"w1","content":"ok"}]}}`,
		// No account of it: from the input, without the lines both share.
		`{"type":"assistant","uuid":"a3","message":{"content":[{"type":"tool_use","id":"e2","name":"Edit","input":{"file_path":"/w/c.go","old_string":"a\nb\nc","new_string":"a\nB\nB2\nc"}}]}}`,
		`{"type":"user","uuid":"r3","message":{"content":[{"type":"tool_result","tool_use_id":"e2","content":"ok"}]}}`,
		// A last newline is no line of its own; an empty patch is no change.
		`{"type":"assistant","uuid":"a4","message":{"content":[{"type":"tool_use","id":"e3","name":"Edit","input":{"file_path":"/w/d.go","old_string":"a\n","new_string":""}}]}}`,
		`{"type":"user","uuid":"r4","message":{"content":[{"type":"tool_result","tool_use_id":"e3","content":"ok"}]}}`,
		`{"type":"assistant","uuid":"a5","message":{"content":[{"type":"tool_use","id":"w2","name":"Write","input":{"file_path":"/w/e.go","content":"a\n"}}]}}`,
		`{"type":"user","uuid":"r5","toolUseResult":{"type":"update","content":"a\n","structuredPatch":[]},"message":{"content":[{"type":"tool_result","tool_use_id":"w2","content":"ok"}]}}`,
	))
	got := c.Since(0)
	want := [][2]int{{2, 2}, {3, 0}, {2, 1}, {0, 1}, {0, 0}}
	for i, w := range want {
		tl := got[i].Parts[0].Tool
		if tl.Add != w[0] || tl.Del != w[1] {
			t.Errorf("%s: +%d -%d, want +%d -%d", tl.Detail, tl.Add, tl.Del, w[0], w[1])
		}
	}
}

func TestClaudeTasks(t *testing.T) {
	c := NewClaude()
	c.Feed(lines(
		`{"type":"assistant","uuid":"a1","message":{"content":[{"type":"tool_use","id":"c1","name":"TaskCreate","input":{"subject":"Read the config"}}]}}`,
		`{"type":"user","uuid":"r1","toolUseResult":{"task":{"id":"1","subject":"Read the config"}},"message":{"content":[{"type":"tool_result","tool_use_id":"c1","content":"Task #1 created successfully: Read the config"}]}}`,
		`{"type":"assistant","uuid":"a2","message":{"content":[{"type":"tool_use","id":"c2","name":"TaskCreate","input":{"subject":"Run the tests"}}]}}`,
		`{"type":"user","uuid":"r2","message":{"content":[{"type":"tool_result","tool_use_id":"c2","content":"Task #2 created successfully: Run the tests"}]}}`,
		`{"type":"assistant","uuid":"a3","message":{"content":[{"type":"tool_use","id":"u1","name":"TaskUpdate","input":{"taskId":"1","status":"completed"}}]}}`,
		`{"type":"user","uuid":"r3","message":{"content":[{"type":"tool_result","tool_use_id":"u1","content":"Updated task #1 status"}]}}`,
		// A call that failed changes nothing.
		`{"type":"assistant","uuid":"a4","message":{"content":[{"type":"tool_use","id":"u2","name":"TaskUpdate","input":{"taskId":"2","status":"completed"}}]}}`,
		`{"type":"user","uuid":"r4","message":{"content":[{"type":"tool_result","tool_use_id":"u2","is_error":true,"content":"no"}]}}`,
		`{"type":"assistant","uuid":"a5","message":{"content":[{"type":"tool_use","id":"u3","name":"TaskUpdate","input":{"taskId":"2","status":"in_progress"}}]}}`,
		`{"type":"user","uuid":"r5","message":{"content":[{"type":"tool_result","tool_use_id":"u3","content":"ok"}]}}`,
	))
	got := c.Tasks()
	if len(got) != 2 || got[0] != (Task{"Read the config", "completed"}) || got[1] != (Task{"Run the tests", "in_progress"}) {
		t.Fatalf("tasks: %+v", got)
	}
	c.Feed(lines(
		`{"type":"assistant","uuid":"a6","message":{"content":[{"type":"tool_use","id":"t1","name":"TodoWrite","input":{"todos":[{"content":"One","status":"completed"},{"content":"Two","status":"weird"}]}}]}}`,
		`{"type":"user","uuid":"r6","message":{"content":[{"type":"tool_result","tool_use_id":"t1","content":"ok"}]}}`,
	))
	if got := c.Tasks(); len(got) != 2 || got[0] != (Task{"One", "completed"}) || got[1] != (Task{"Two", "pending"}) {
		t.Fatalf("todo list: %+v", got)
	}
}

func TestClaudeMeta(t *testing.T) {
	c := NewClaude()
	if c.Meta() != (Meta{}) {
		t.Fatalf("empty: %+v", c.Meta())
	}
	c.Feed(lines(
		`{"type":"permission-mode","permissionMode":"auto"}`,
		`{"type":"user","uuid":"u1","gitBranch":"main","permissionMode":"default","message":{"content":"go"}}`,
		`{"type":"assistant","uuid":"a1","gitBranch":"main","message":{"model":"claude-opus-5-5","usage":{"input_tokens":2,"cache_creation_input_tokens":100,"cache_read_input_tokens":1000,"output_tokens":5},"content":[{"type":"text","text":"ok"}]}}`,
		`{"type":"assistant","uuid":"a2","isSidechain":true,"message":{"model":"claude-haiku-5-5","usage":{"input_tokens":9},"content":[{"type":"text","text":"sub"}]}}`,
		`{"type":"assistant","uuid":"a3","message":{"model":"<synthetic>","usage":{"input_tokens":0},"content":[{"type":"text","text":"API Error"}]}}`,
	))
	m := c.Meta()
	if m.Model != "claude-opus-5-5" || m.Mode != "default" || m.Branch != "main" || m.Context != 1102 {
		t.Fatalf("meta: %+v", m)
	}
	c.Feed(lines(`{"type":"permission-mode","permissionMode":"plan"}`, `{"type":"system","subtype":"turn_duration","gitBranch":"feat/x"}`))
	if m := c.Meta(); m.Mode != "plan" || m.Branch != "feat/x" {
		t.Fatalf("later rows: %+v", m)
	}
	c.Feed(lines(`{"type":"user","uuid":"u2","gitBranch":"","message":{"content":"elsewhere"}}`, `{"type":"system","subtype":"turn_duration"}`))
	if m := c.Meta(); m.Branch != "" {
		t.Fatalf("no branch any more: %+v", m)
	}
}

func TestClaudeQueue(t *testing.T) {
	c := NewClaude()
	c.Feed(lines(
		`{"type":"user","uuid":"u1","timestamp":"2026-10-24T09:00:00Z","message":{"content":"go"}}`,
		`{"type":"queue-operation","operation":"enqueue","timestamp":"2026-10-24T09:00:05Z","content":"also the README"}`,
		`{"type":"queue-operation","operation":"enqueue","timestamp":"2026-10-24T09:00:06Z","content":"<task-notification>\n<task-id>b1</task-id>\n<status>completed</status>\n</task-notification>"}`,
		`{"type":"queue-operation","operation":"enqueue","timestamp":"2026-10-24T09:00:07Z","content":"and commit"}`,
		// (A subagent's report handed back to the session waits in the queue too: the agent's own, not the person's.)
		`{"type":"queue-operation","operation":"enqueue","timestamp":"2026-10-24T09:00:08Z","content":"<agent-message from=\"a1\">\n[Subagent hand-back] The text below is the final report of a subagent...\n</agent-message>"}`,
	))
	if q := c.Queued(); len(q) != 2 || q[0].Text != "also the README" || q[1].Text != "and commit" || q[0].Time == "" {
		t.Fatalf("queued: %+v", q)
	}
	// Taken in during the turn: no longer queued, and marked so.
	c.Feed(lines(
		`{"type":"queue-operation","operation":"remove","content":"also the README"}`,
		`{"type":"attachment","uuid":"q1","attachment":{"type":"queued_command","commandMode":"prompt","prompt":"also the README"}}`,
	))
	if q := c.Queued(); len(q) != 1 || q[0].Text != "and commit" {
		t.Fatalf("after remove: %+v", q)
	}
	all := c.Since(0)
	if last := all[len(all)-1]; last.Role != RoleUser || !last.Mid || all[0].Mid {
		t.Fatalf("mid: %+v", all)
	}
	// The turn ends: what waits is dequeued (which, the log does not say) and
	// arrives as the next turn: then it waits no more.
	c.Feed(lines(`{"type":"queue-operation","operation":"dequeue"}`))
	if q := c.Queued(); len(q) != 1 {
		t.Fatalf("a dequeue alone says nothing: %+v", q)
	}
	c.Feed(lines(`{"type":"user","uuid":"u3","message":{"content":"and  commit"}}`))
	if q := c.Queued(); len(q) != 0 {
		t.Fatalf("arrived: %+v", q)
	}
	// Put back in the draft (Esc): the latest first.
	c.Feed(lines(
		`{"type":"queue-operation","operation":"enqueue","content":"one"}`,
		`{"type":"queue-operation","operation":"enqueue","content":"two"}`,
		`{"type":"queue-operation","operation":"popOne"}`,
	))
	if q := c.Queued(); len(q) != 1 || q[0].Text != "one" {
		t.Fatalf("popOne: %+v", q)
	}
	c.Feed(lines(`{"type":"queue-operation","operation":"popAll"}`))
	if q := c.Queued(); len(q) != 0 {
		t.Fatalf("popAll: %+v", q)
	}
	// The same text twice: one arrives, one still waits.
	c.Feed(lines(
		`{"type":"queue-operation","operation":"enqueue","content":"again"}`,
		`{"type":"queue-operation","operation":"enqueue","content":"again"}`,
		`{"type":"user","uuid":"u4","message":{"content":"again"}}`,
	))
	if q := c.Queued(); len(q) != 1 {
		t.Fatalf("twice: %+v", q)
	}
	c.ClearQueue()
	// More than are kept: the latest stay.
	for i := 0; i < maxQueue+3; i++ {
		c.Feed(lines(fmt.Sprintf(`{"type":"queue-operation","operation":"enqueue","content":"m%d"}`, i)))
	}
	if q := c.Queued(); len(q) != maxQueue || q[0].Text != "m3" {
		t.Fatalf("bounded: %d %+v", len(q), q[0])
	}
	// Claude Code's word that it was stopped is a note.
	c.Feed(lines(`{"type":"user","uuid":"x","message":{"content":[{"type":"text","text":"[Request interrupted by user]"}]}}`))
	all = c.Since(0)
	if last := all[len(all)-1]; last.Role != RoleNote || last.Parts[0].Text != "[Request interrupted by user]" {
		t.Fatalf("interrupted: %+v", last)
	}
}

func TestClaudePatch(t *testing.T) {
	c := NewClaude()
	long := strings.Repeat("x", 2000)
	c.Feed(lines(
		`{"type":"assistant","uuid":"a1","message":{"content":[{"type":"tool_use","id":"e1","name":"Edit","input":{"file_path":"/p/a.py","old_string":"300","new_string":"600"}}]}}`,
		`{"type":"user","uuid":"r1","message":{"content":[{"type":"tool_result","tool_use_id":"e1","content":"The file /p/a.py has been updated successfully."}]},"toolUseResult":{"structuredPatch":[{"oldStart":7,"oldLines":3,"newStart":7,"newLines":3,"lines":[" keep","-WINDOW = 300","+WINDOW = 600","\\ No newline at end of file"]}]}}`,
		`{"type":"assistant","uuid":"a2","message":{"content":[{"type":"tool_use","id":"w1","name":"Write","input":{"file_path":"/p/new.py","content":"a\nb\n"}}]}}`,
		`{"type":"user","uuid":"r2","message":{"content":[{"type":"tool_result","tool_use_id":"w1","content":"File created"}]},"toolUseResult":{"type":"create","content":"a\nb\n","structuredPatch":[]}}`,
		`{"type":"assistant","uuid":"a3","message":{"content":[{"type":"tool_use","id":"e2","name":"Edit","input":{"file_path":"/p/b.py","old_string":"a","new_string":"b"}}]}}`,
		`{"type":"user","uuid":"r3","message":{"content":[{"type":"tool_result","tool_use_id":"e2","content":"ok"}]},"toolUseResult":{"structuredPatch":[{"oldStart":1,"newStart":1,"lines":[`+strings.Repeat(`"+`+long+`",`, 20)+`"+end"]}]}}`,
	))
	got := c.Since(0)
	e := got[0].Parts[0].Tool
	if len(e.Patch) != 1 || e.Patch[0].OldStart != 7 || len(e.Patch[0].Lines) != 3 || e.Patch[0].Lines[2] != "+WINDOW = 600" || e.Old != "" || e.New != "" || e.Add != 1 || e.Del != 1 {
		t.Fatalf("edit: %+v", e)
	}
	w := got[1].Parts[0].Tool
	if w.Patch != nil || w.New != "a\nb\n" || w.Add != 2 {
		t.Fatalf("created: %+v", w)
	}
	b := got[2].Parts[0].Tool
	size := 0
	for _, l := range b.Patch[0].Lines {
		size += len(l)
		if len(l) > maxPatchLine {
			t.Fatalf("a line kept whole: %d", len(l))
		}
	}
	if !b.PatchCut || size > maxPatch {
		t.Fatalf("bounded: cut %v size %d", b.PatchCut, size)
	}
}

func TestClaudeQueueArrivals(t *testing.T) {
	c := NewClaude()
	// Two alike: one removed and taken in mid-turn; the other still waits.
	c.Feed(lines(
		`{"type":"queue-operation","operation":"enqueue","content":"same"}`,
		`{"type":"queue-operation","operation":"enqueue","content":"same"}`,
		`{"type":"queue-operation","operation":"remove","content":"same"}`,
		`{"type":"attachment","uuid":"q1","attachment":{"type":"queued_command","commandMode":"prompt","prompt":"same"}}`,
	))
	if q := c.Queued(); len(q) != 1 {
		t.Fatalf("the other still waits: %+v", q)
	}
	// A message with a picture arrives as a list.
	c.Feed(lines(
		`{"type":"queue-operation","operation":"enqueue","content":"look"}`,
		`{"type":"user","uuid":"u1","message":{"content":[{"type":"text","text":"look"},{"type":"image","source":{"data":"AA"}}]}}`,
	))
	if q := c.Queued(); len(q) != 1 || q[0].Text != "same" {
		t.Fatalf("the list arrival: %+v", q)
	}
	// A file written over with what it held: nothing changed, nothing to show.
	c.Feed(lines(
		`{"type":"assistant","uuid":"a1","message":{"content":[{"type":"tool_use","id":"w1","name":"Write","input":{"file_path":"/p/x","content":"a\nb\n"}}]}}`,
		`{"type":"user","uuid":"r1","message":{"content":[{"type":"tool_result","tool_use_id":"w1","content":"ok"}]},"toolUseResult":{"type":"update","content":"a\nb\n","structuredPatch":[]}}`,
	))
	all := c.Since(0)
	w := all[len(all)-1].Parts[0].Tool
	if !w.Same || w.New != "" || w.Patch != nil || w.Add != 0 {
		t.Fatalf("unchanged write: %+v", w)
	}
}

func TestClaudeQueueSince(t *testing.T) {
	c := NewClaude()
	c.Feed(lines(`{"type":"queue-operation","operation":"enqueue","timestamp":"2026-10-24T09:09:58.500Z","content":"X"}`))
	started := time.Date(2026, 10, 24, 9, 10, 0, 0, time.UTC).UnixMilli()
	c.Feed(lines(`{"type":"queue-operation","operation":"enqueue","timestamp":"2026-10-24T09:09:58.600Z","content":"Z"}`, `{"type":"queue-operation","operation":"remove","timestamp":"2026-10-24T09:09:58.700Z","content":"Z"}`))
	c.QueueSince(started)
	if q := c.Queued(); len(q) != 0 {
		t.Fatalf("the process before's: %+v", q)
	}
	// The new process queues Z: its arrival is of it, not of the old one's removal.
	c.Feed(lines(`{"type":"queue-operation","operation":"enqueue","timestamp":"2026-10-24T09:10:01Z","content":"Z"}`, `{"type":"user","uuid":"uz","message":{"content":"Z"}}`))
	if q := c.Queued(); len(q) != 0 {
		t.Fatalf("Z arrived: %+v", q)
	}
	// The new process queues the same text: removed and taken in, it waits no more.
	c.Feed(lines(
		`{"type":"queue-operation","operation":"enqueue","timestamp":"2026-10-24T09:09:59Z","content":"late row of the old one"}`,
		`{"type":"queue-operation","operation":"enqueue","timestamp":"2026-10-24T09:10:05Z","content":"X"}`,
		`{"type":"queue-operation","operation":"remove","timestamp":"2026-10-24T09:10:06Z","content":"X"}`,
		`{"type":"attachment","uuid":"q1","attachment":{"type":"queued_command","commandMode":"prompt","prompt":"X"}}`,
	))
	if q := c.Queued(); len(q) != 0 {
		t.Fatalf("after: %+v", q)
	}
	// A removal whose arrival came while nothing waited leaves nothing behind.
	c.Feed(lines(
		`{"type":"queue-operation","operation":"enqueue","timestamp":"2026-10-24T09:11:00Z","content":"Y"}`,
		`{"type":"queue-operation","operation":"remove","timestamp":"2026-10-24T09:11:01Z","content":"Y"}`,
		`{"type":"user","uuid":"u1","message":{"content":[{"type":"text","text":"Y"}]}}`,
		`{"type":"queue-operation","operation":"enqueue","timestamp":"2026-10-24T09:12:00Z","content":"Y"}`,
		`{"type":"user","uuid":"u2","message":{"content":"Y"}}`,
	))
	if q := c.Queued(); len(q) != 0 {
		t.Fatalf("second Y arrived: %+v", q)
	}
}

func TestClaudeQueueSinceKeepsTheNewProcessRemovals(t *testing.T) {
	c := NewClaude()
	c.Feed(lines(
		`{"type":"queue-operation","operation":"enqueue","timestamp":"2026-10-24T09:10:01Z","content":"X"}`,
		`{"type":"queue-operation","operation":"enqueue","timestamp":"2026-10-24T09:10:02Z","content":"X"}`,
		`{"type":"queue-operation","operation":"remove","timestamp":"2026-10-24T09:10:03Z","content":"X"}`,
	))
	c.QueueSince(time.Date(2026, 10, 24, 9, 10, 0, 0, time.UTC).UnixMilli()) // (found only now)
	c.Feed(lines(`{"type":"attachment","uuid":"q1","attachment":{"type":"queued_command","commandMode":"prompt","prompt":"X"}}`))
	if q := c.Queued(); len(q) != 1 {
		t.Fatalf("the second X still waits: %+v", q)
	}
}

func TestClaudeQueueSinceToTheMillisecond(t *testing.T) {
	c := NewClaude()
	c.Feed(lines(`{"type":"queue-operation","operation":"enqueue","timestamp":"2026-10-24T09:10:00.050Z","content":"X"}`, `{"type":"queue-operation","operation":"remove","timestamp":"2026-10-24T09:10:00.100Z","content":"X"}`))
	c.QueueSince(time.Date(2026, 10, 24, 9, 10, 0, 800e6, time.UTC).UnixMilli())
	c.Feed(lines(`{"type":"queue-operation","operation":"enqueue","timestamp":"2026-10-24T09:10:01Z","content":"X"}`, `{"type":"user","uuid":"u","message":{"content":"X"}}`))
	if q := c.Queued(); len(q) != 0 {
		t.Fatalf("the old removal was dropped, the new X arrived: %+v", q)
	}
}

// A message Claude Code took as a paste, a picture's path in it: shown as it
// was typed (the wrapper and the "[Image #N]" taken off), the picture a part.
func TestUserPasteWithImage(t *testing.T) {
	c := NewClaude()
	c.Feed(lines(`{"type":"user","uuid":"u1","promptSource":"typed","message":{"content":[{"type":"text","text":"[Image #6]\n\n<pasted_content id=\"4cde\">\n1. one\n2. two\n</pasted_content id=\"4cde\">\n"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AA=="}}]}}`))
	es := c.Since(0)
	if len(es) != 1 || len(es[0].Parts) != 2 || es[0].Parts[0].Text != "1. one\n2. two" || es[0].Parts[1].Kind != KindImage {
		t.Fatalf("got %+v", es)
	}
	// (Ids that differ: not a wrapper of Claude Code's, left as it is.)
	if s := unpasted(`<pasted_content id="a">x</pasted_content id="b">`); s != `<pasted_content id="a">x</pasted_content id="b">` {
		t.Fatalf("unpasted: %q", s)
	}
}

// Review 228's cases: each as the log would have it.
func TestReview228Parser(t *testing.T) {
	// A slash command's envelope that begins with its message.
	c := NewClaude()
	c.Feed(lines(`{"type":"user","uuid":"u1","message":{"content":"<command-message>review</command-message>\n<command-name>/review</command-name>\n<command-args>HEAD</command-args>"}}`))
	if es := c.Since(0); len(es) != 1 || es[0].Parts[0].Text != "/review HEAD" {
		t.Errorf("command-message first: %+v", es)
	}
	// A last line without its newline does not keep a batch from being trimmed.
	c = NewClaude()
	c.Feed([]byte(strings.Repeat(`{"type":"user","message":{"content":"x"}}`+"\n", maxEntries+1) + "{"))
	if n := len(c.Since(0)); n != maxEntries || !c.Trimmed() {
		t.Errorf("trim with a last half line: %d kept, trimmed %v", n, c.Trimmed())
	}
	// A summary written as blocks is a summary, not the person's.
	c = NewClaude()
	c.Feed(lines(`{"type":"user","uuid":"s","isCompactSummary":true,"message":{"content":[{"type":"text","text":"Session summary"}]}}`))
	if es := c.Since(0); len(es) != 1 || es[0].Role != RoleSummary {
		t.Errorf("summary as blocks: %+v", es)
	}
	// A queued message with a picture arrives as typed: its queue entry goes.
	c = NewClaude()
	c.Feed(lines(
		`{"type":"queue-operation","operation":"enqueue","timestamp":"2026-10-25T09:00:00Z","content":"look"}`,
		`{"type":"attachment","uuid":"q","attachment":{"type":"queued_command","commandMode":"prompt","prompt":[{"type":"text","text":"[Image #1] look"},{"type":"image"}]}}`,
	))
	if es := c.Since(0); len(es) != 1 || es[0].Parts[0].Text != "look" || len(c.Queued()) != 0 {
		t.Errorf("queued with a picture: %+v, queue %+v", es, c.Queued())
	}
	// A long queued message arrives: the queue's shortened copy goes too.
	long := strings.Repeat("x", maxText+1)
	c = NewClaude()
	c.Feed(lines(
		`{"type":"queue-operation","operation":"enqueue","timestamp":"2026-10-25T09:00:00Z","content":"`+long+`"}`,
		`{"type":"user","uuid":"u","message":{"content":"`+long+`"}}`,
	))
	if len(c.Queued()) != 0 {
		t.Errorf("a long one stays queued")
	}
	// A call id longer than kept: its result still finds it.
	id := strings.Repeat("i", maxID+5)
	c = NewClaude()
	c.Feed(lines(
		`{"type":"assistant","uuid":"a","message":{"content":[{"type":"tool_use","id":"`+id+`","name":"Bash","input":{"command":"ls"}}]}}`,
		`{"type":"user","uuid":"r","message":{"content":[{"type":"tool_result","tool_use_id":"`+id+`","content":"out"}]}}`,
	))
	if es := c.Since(0); len(es) == 0 || es[0].Parts[0].Tool == nil || !es[0].Parts[0].Tool.Done {
		t.Errorf("long id: %+v", es)
	}
}

// A long message removed from the queue, queued again, then the first
// arrives: the removal is what it matches, the second stays queued.
func TestLongArrivalMatchesRemoval(t *testing.T) {
	long := strings.Repeat("x", maxText+1)
	c := NewClaude()
	c.Feed(lines(
		`{"type":"queue-operation","operation":"enqueue","timestamp":"2026-10-25T09:00:00Z","content":"`+long+`"}`,
		`{"type":"queue-operation","operation":"remove","timestamp":"2026-10-25T09:00:01Z","content":"`+long+`"}`,
		`{"type":"queue-operation","operation":"enqueue","timestamp":"2026-10-25T09:00:02Z","content":"`+long+`"}`,
		`{"type":"attachment","uuid":"q","attachment":{"type":"queued_command","commandMode":"prompt","prompt":"`+long+`"}}`,
	))
	if n := len(c.Queued()); n != 1 {
		t.Fatalf("queued %d, want 1", n)
	}
}

// A shell command that changed files: each with its patch, counted as the
// turn's changes (Claude Code reports them in toolUseResult.bashEditDiff).
func TestClaudeBashEditDiff(t *testing.T) {
	c := NewClaude()
	c.Feed(lines(
		`{"type":"assistant","uuid":"a1","message":{"content":[{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"sed -i s/a/b/ x.py y.py","description":"fix it"}}]}}`,
		`{"type":"user","uuid":"r1","message":{"content":[{"type":"tool_result","tool_use_id":"t1","content":""}]},"toolUseResult":{"stdout":"","stderr":"","bashEditDiff":{"files":[`+
			`{"filePath":"/home/u/p/x.py","hunks":[{"oldStart":1,"oldLines":2,"newStart":1,"newLines":2,"lines":[" import os","-a = 1","+a = 2"]}]},`+
			`{"filePath":"/home/u/p/y.py","hunks":[{"oldStart":9,"oldLines":1,"newStart":9,"newLines":2,"lines":["-b = 1","+b = 2","+c = 3"]}]}],`+
			`"changedFiles":["/home/u/p/x.py","/home/u/p/y.py"],"moreFiles":3}}}`,
	))
	all := c.Since(0)
	var tool *Tool
	for _, e := range all {
		for _, p := range e.Parts {
			if p.Tool != nil {
				tool = p.Tool
			}
		}
	}
	if tool == nil || len(tool.Files) != 2 {
		t.Fatalf("files: %+v", tool)
	}
	if tool.Files[0].Path != "/home/u/p/x.py" || tool.Files[0].Add != 1 || tool.Files[0].Del != 1 || len(tool.Files[0].Patch) != 1 {
		t.Errorf("the first file: %+v", tool.Files[0])
	}
	if tool.Files[1].Add != 2 || tool.Files[1].Del != 1 {
		t.Errorf("the second file: %+v", tool.Files[1])
	}
	if tool.Add != 3 || tool.Del != 2 || tool.MoreFiles != 3 {
		t.Errorf("the call: +%d -%d, %d more", tool.Add, tool.Del, tool.MoreFiles)
	}
}

// A call that came back with a picture (a screenshot read, a PDF's pages)
// or with tools a search found: not nothing, though it is not words.
func TestClaudeResultNotWords(t *testing.T) {
	c := NewClaude()
	c.Feed(lines(
		`{"type":"assistant","uuid":"a1","message":{"content":[{"type":"tool_use","id":"t1","name":"Read","input":{"file_path":"/p/shot.png"}}]}}`,
		`{"type":"user","uuid":"r1","message":{"content":[{"type":"tool_result","tool_use_id":"t1","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAAA"}}]}]},"toolUseResult":{"type":"image"}}`,
		`{"type":"assistant","uuid":"a2","message":{"content":[{"type":"tool_use","id":"t2","name":"ToolSearch","input":{"query":"slack"}}]}}`,
		`{"type":"user","uuid":"r2","message":{"content":[{"type":"tool_result","tool_use_id":"t2","content":[{"type":"tool_reference","name":"a"},{"type":"tool_reference","name":"b"}]}]}}`,
	))
	var got []*Tool
	for _, e := range c.Since(0) {
		for _, p := range e.Parts {
			if p.Tool != nil {
				got = append(got, p.Tool)
			}
		}
	}
	if len(got) != 2 {
		t.Fatalf("tools: %d", len(got))
	}
	if got[0].Images != 1 || got[0].Refs != 0 || !got[0].Done {
		t.Errorf("the picture: %+v", got[0])
	}
	if got[1].Refs != 2 || got[1].Images != 0 {
		t.Errorf("the tools found: %+v", got[1])
	}
}

// A tool of the model's own (the advisor) answers in a row of its own: the
// call it answers is done, with what it said.
func TestClaudeModelToolResult(t *testing.T) {
	c := NewClaude()
	c.Feed(lines(
		`{"type":"assistant","uuid":"a1","message":{"content":[{"type":"server_tool_use","id":"t1","name":"advisor","input":{}}]}}`,
		`{"type":"assistant","uuid":"a2","message":{"content":[{"type":"advisor_tool_result","tool_use_id":"t1","content":"先把测试跑一遍。"}]}}`,
	))
	var tool *Tool
	for _, e := range c.Since(0) {
		for _, p := range e.Parts {
			if p.Tool != nil {
				tool = p.Tool
			}
		}
	}
	if tool == nil || !tool.Done || tool.Result != "先把测试跑一遍。" {
		t.Fatalf("the advisor's answer: %+v", tool)
	}
}

// A subagent sent off to work on its own: its report, where Claude Code
// keeps it with the call, is what the call came back with (not the word
// that it was sent off).
func TestClaudeSubagentHandback(t *testing.T) {
	c := NewClaude()
	c.Feed(lines(
		`{"type":"assistant","uuid":"a1","message":{"content":[{"type":"tool_use","id":"t1","name":"Agent","input":{"description":"查一下","prompt":"去查"}}]}}`,
		`{"type":"user","uuid":"r1","message":{"content":[{"type":"tool_result","tool_use_id":"t1","content":"Async agent launched successfully."}]},"toolUseResult":{"isAsync":true,"status":"completed","handbackReport":"查完了：三个来源都对得上。"}}`,
	))
	var tool *Tool
	for _, e := range c.Since(0) {
		for _, p := range e.Parts {
			if p.Tool != nil {
				tool = p.Tool
			}
		}
	}
	if tool == nil || tool.Result != "查完了：三个来源都对得上。" {
		t.Fatalf("the report: %+v", tool)
	}
}

// Work set going and left to itself (a command in the background, a
// subagent on its own) is there until the agent is told it has ended; what
// it says of itself meanwhile shows with it.
func TestClaudeRunning(t *testing.T) {
	c := NewClaude()
	c.Feed(lines(
		`{"type":"assistant","uuid":"a1","message":{"content":[{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"go test ./...","description":"跑测试"}}]}}`,
		`{"type":"user","uuid":"r1","timestamp":"2026-10-26T10:00:00Z","message":{"content":[{"type":"tool_result","tool_use_id":"t1","content":"running in background"}]},"toolUseResult":{"stdout":"","backgroundTaskId":"b1"}}`,
		`{"type":"assistant","uuid":"a2","message":{"content":[{"type":"tool_use","id":"t2","name":"Agent","input":{"description":"查资料","prompt":"去查"}}]}}`,
		`{"type":"user","uuid":"r2","timestamp":"2026-10-26T10:00:05Z","message":{"content":[{"type":"tool_result","tool_use_id":"t2","content":"Async agent launched"}]},"toolUseResult":{"isAsync":true,"agentId":"ag1","status":"async_launched"}}`,
	))
	if r := c.Running(); len(r) != 2 || r[0].ID != "b1" || r[0].What != "跑测试" || r[0].Kind != "cmd" || r[1].Kind != "agent" {
		t.Fatalf("running: %+v", c.Running())
	}
	// It says something of itself: still at it, with that said.
	c.Feed(lines(`{"type":"attachment","uuid":"n1","attachment":{"type":"queued_command","commandMode":"task-notification","prompt":"<task-notification>\n<task-id>b1</task-id>\n<event>3 passed</event>\n</task-notification>"}}`))
	if r := c.Running(); len(r) != 2 || r[0].Note != "3 passed" {
		t.Errorf("what it said: %+v", c.Running())
	}
	// It has ended: no longer there.
	c.Feed(lines(`{"type":"attachment","uuid":"n2","attachment":{"type":"queued_command","commandMode":"task-notification","prompt":"<task-notification>\n<task-id>b1</task-id>\n<status>completed</status>\n<summary>Background command \"tests\" completed</summary>\n</task-notification>"}}`))
	if r := c.Running(); len(r) != 1 || r[0].ID != "ag1" {
		t.Errorf("after it ended: %+v", c.Running())
	}
}

// A command that changed more than there is room to keep: the lines it
// changed are still counted whole, and the files left without a patch say
// there was more of it.
func TestClaudeBashEditDiffBudget(t *testing.T) {
	big := func(n int) string {
		out := make([]string, 0, n)
		for i := range n {
			out = append(out, fmt.Sprintf(`"+%d %s"`, i, strings.Repeat("x", 300)))
		}
		return strings.Join(out, ",")
	}
	c := NewClaude()
	c.Feed(lines(
		`{"type":"assistant","uuid":"a1","message":{"content":[{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"sed -i …","description":"改"}}]}}`,
		`{"type":"user","uuid":"r1","message":{"content":[{"type":"tool_result","tool_use_id":"t1","content":""}]},"toolUseResult":{"bashEditDiff":{"files":[`+
			`{"filePath":"/p/a","hunks":[{"oldStart":1,"newStart":1,"lines":[`+big(60)+`]}]},`+
			`{"filePath":"/p/b","hunks":[{"oldStart":1,"newStart":1,"lines":[`+big(2)+`]}]}],"moreFiles":0}}}`,
	))
	var tool *Tool
	for _, e := range c.Since(0) {
		for _, p := range e.Parts {
			if p.Tool != nil {
				tool = p.Tool
			}
		}
	}
	if tool == nil || len(tool.Files) != 2 {
		t.Fatalf("files: %+v", tool)
	}
	if tool.Files[0].Add != 60 || tool.Files[1].Add != 2 || tool.Add != 62 {
		t.Errorf("lines counted: %d, %d, %d", tool.Files[0].Add, tool.Files[1].Add, tool.Add)
	}
	if len(tool.Files[1].Patch) != 0 || !tool.Files[1].PatchCut {
		t.Errorf("the file left without a patch: %+v", tool.Files[1])
	}
}

// Told it had ended before what set it going was read (the notice first,
// or the log read from its middle): it is not taken for going.
func TestClaudeRunningOutOfOrder(t *testing.T) {
	c := NewClaude()
	c.Feed(lines(
		`{"type":"user","uuid":"n1","message":{"content":"<task-notification>\n<task-id>b7</task-id>\n<status>completed</status>\n<summary>done</summary>\n</task-notification>"}}`,
		`{"type":"assistant","uuid":"a1","message":{"content":[{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"sleep 9","description":"等着"}}]}}`,
		`{"type":"user","uuid":"r1","message":{"content":[{"type":"tool_result","tool_use_id":"t1","content":"running in background"}]},"toolUseResult":{"backgroundTaskId":"b7"}}`,
	))
	if r := c.Running(); len(r) != 0 {
		t.Errorf("taken for going after it had ended: %+v", r)
	}
	// Read on: what the part before set going is kept when the newer part
	// was never told it ended.
	old := NewClaude()
	old.Feed(lines(
		`{"type":"assistant","uuid":"o1","message":{"content":[{"type":"tool_use","id":"o-t","name":"Bash","input":{"command":"make","description":"编译"}}]}}`,
		`{"type":"user","uuid":"o2","message":{"content":[{"type":"tool_result","tool_use_id":"o-t","content":"running in background"}]},"toolUseResult":{"backgroundTaskId":"b8"}}`,
	))
	c.Prepend(old)
	if r := c.Running(); len(r) != 1 || r[0].ID != "b8" || r[0].What != "编译" {
		t.Errorf("what the part before set going: %+v", c.Running())
	}
}

// Another session's agent writing to this one, and a notice from the
// harness: what waits says who it is from and reads as it was written, not
// as the envelope the harness put round it.
func TestClaudeQueuedNotTheirs(t *testing.T) {
	c := NewClaude()
	c.Feed(lines(
		`{"type":"user","uuid":"u1","timestamp":"2026-10-26T15:00:00Z","message":{"content":"go"}}`,
		`{"type":"queue-operation","operation":"enqueue","timestamp":"2026-10-26T15:00:05Z","content":"<cross-session-message from=\"uds:/tmp/cc-socks/18530.sock\" hop-chain=\"9d934e8f\" from-name=\"ai-a8\" from-mode=\"prompting\">\n席位空出来了，你可以开测。"}`,
		`{"type":"queue-operation","operation":"enqueue","timestamp":"2026-10-26T15:00:06Z","content":"[Cross-session idle notice] \"ai-a8\" is idle now."}`,
		`{"type":"queue-operation","operation":"enqueue","timestamp":"2026-10-26T15:00:07Z","content":"顺便把日志级别调低"}`,
	))
	q := c.Queued()
	if len(q) != 3 {
		t.Fatalf("queued: %+v", q)
	}
	if q[0].From != "ai-a8" || !q[0].Note || q[0].Text != "席位空出来了，你可以开测。" {
		t.Errorf("from another session: %+v", q[0])
	}
	if q[1].From != "" || !q[1].Note || !strings.HasPrefix(q[1].Text, "[Cross-session idle notice]") {
		t.Errorf("a notice: %+v", q[1])
	}
	if q[2].Note || q[2].Text != "顺便把日志级别调低" {
		t.Errorf("the person's own: %+v", q[2])
	}
	// Taken in, it is a note in the thread, with its words and no envelope.
	c.Feed(lines(`{"type":"user","uuid":"u2","message":{"content":"<cross-session-message from=\"uds:/x.sock\" from-name=\"ai-a8\">\n席位空出来了，你可以开测。"}}`))
	all := c.Since(0)
	last := all[len(all)-1]
	if last.Role != RoleNote || last.Parts[0].Text != "席位空出来了，你可以开测。" {
		t.Errorf("taken in: %s %+v", last.Role, last.Parts)
	}
}

// A tool nothing here knows says what it acted on: the field that tells
// most of it, not the first one by name (which used to put "engine" or
// "colorScheme" on the line).
func TestClaudeUnknownToolObject(t *testing.T) {
	c := NewClaude()
	c.Feed(lines(
		`{"type":"assistant","uuid":"a","message":{"content":[` +
			`{"type":"tool_use","id":"1","name":"Monitor","input":{"command":"tail -f log","description":"wait for the build","timeout":600}},` +
			`{"type":"tool_use","id":"2","name":"TaskStop","input":{"task_id":"b12"}},` +
			`{"type":"tool_use","id":"3","name":"Artifact","input":{"url":"https://claude.ai/artifact/x","action":"read"}},` +
			`{"type":"tool_use","id":"4","name":"Artifact","input":{"file_path":"page.html","icon":"chart","description":"a chart"}},` +
			`{"type":"tool_use","id":"5","name":"SendMessage","input":{"to":"review-agent","message":"look at line 20","summary":"a review"}},` +
			`{"type":"tool_use","id":"6","name":"mcp__plugin_playwright_playwright__browser_click","input":{"element":"Send button","target":"page"}},` +
			`{"type":"tool_use","id":"7","name":"mcp__claude_ai_Claude_Docs__update","input":{"engine":"markdown"}},` +
			`{"type":"tool_use","id":"8","name":"mcp__plugin_playwright_playwright__browser_resize","input":{"width":390,"height":844}},` +
			`{"type":"tool_use","id":"9","name":"advisor","input":{}},` +
			`{"type":"tool_use","id":"10","name":"mcp__notion__notion-fetch","input":{"id":"abc"}}]}}`,
	))
	p := c.Since(0)[0].Parts
	if len(p) != 10 {
		t.Fatalf("parts: %d", len(p))
	}
	want := []struct{ title, detail string }{
		{"wait for the build", "tail -f log"},
		{"", "b12"},
		{"", "read https://claude.ai/artifact/x"},
		{"", "publish page.html"},
		{"review-agent", "a review"},
		{"", "click Send button"},
		{"", "update markdown"},
		{"", "resize"},
		{"", ""},
		{"", "notion-fetch abc"},
	}
	for i, w := range want {
		got := p[i].Tool
		if got.Title != w.title || got.Detail != w.detail {
			t.Errorf("%s: title %q detail %q, want %q %q", got.Name, got.Title, got.Detail, w.title, w.detail)
		}
	}
}

func TestMcpToolName(t *testing.T) {
	for _, x := range []struct{ name, server, tool string }{
		{"mcp__plugin_playwright_playwright__browser_click", "plugin_playwright_playwright", "browser_click"},
		{"mcp__notion__notion-fetch", "notion", "notion-fetch"},
		{"mcp__claude_ai_Claude_Docs__update", "claude_ai_Claude_Docs", "update"},
		{"mcp__x", "", "x"},
	} {
		s, tl, ok := mcpTool(x.name)
		if !ok || s != x.server || tl != x.tool {
			t.Errorf("%s: %q %q %v", x.name, s, tl, ok)
		}
	}
	if _, _, ok := mcpTool("Bash"); ok {
		t.Error("Bash read as an MCP tool")
	}
}

// A file Claude Code says changed shows only where the agent did not write
// it itself, and the files the person referred to only where they said so.
// The notice comes while the turn goes on: it stays where it was read, with
// the work that came after it below.
func TestClaudeNotedFiles(t *testing.T) {
	c := NewClaude()
	c.Feed(lines(
		`{"type":"user","uuid":"u1","timestamp":"2026-10-27T01:00:00Z","message":{"role":"user","content":"look at @web/src/app.tsx and @web/src/prefs.ts"}}`,
		`{"type":"attachment","uuid":"f1","attachment":{"type":"file","filename":"/w/web/src/app.tsx","displayPath":"web/src/app.tsx","content":"…"}}`,
		`{"type":"attachment","uuid":"f2","attachment":{"type":"file","filename":"/w/web/src/prefs.ts","displayPath":"web/src/prefs.ts","content":"…"}}`,
		`{"type":"attachment","uuid":"f3","attachment":{"type":"file","filename":"/w/web/src/i18n.ts","displayPath":"web/src/i18n.ts","content":"…"}}`,
		`{"type":"assistant","uuid":"a1","timestamp":"2026-10-27T01:00:05Z","message":{"content":[{"type":"tool_use","id":"1","name":"Edit","input":{"file_path":"/w/web/src/app.tsx","old_string":"a","new_string":"b"}}]}}`,
		`{"type":"user","uuid":"r1","message":{"content":[{"type":"tool_result","tool_use_id":"1","content":"ok"}]}}`,
		`{"type":"attachment","uuid":"e1","attachment":{"type":"edited_text_file","filename":"/w/web/src/app.tsx","snippet":""}}`,
		`{"type":"attachment","uuid":"e2","attachment":{"type":"edited_text_file","filename":"/w/notes.md","snippet":""}}`,
		`{"type":"attachment","uuid":"e3","attachment":{"type":"edited_text_file","filename":"/w/other.md","snippet":""}}`,
		`{"type":"assistant","uuid":"a2","timestamp":"2026-10-27T01:00:09Z","message":{"content":[{"type":"tool_use","id":"2","name":"Read","input":{"file_path":"/w/notes.md"}}]}}`,
	))
	es := c.Since(0)
	var got []string
	for _, e := range es {
		if len(e.Parts) == 1 && e.Parts[0].Kind == KindNoted {
			n := e.Parts[0].Noted
			got = append(got, e.Role+" "+n.Kind+" "+strings.Join(n.Paths, ","))
			continue
		}
		got = append(got, e.Role)
	}
	want := []string{
		"user",
		"note brought web/src/app.tsx,web/src/prefs.ts", // (i18n.ts was not asked for: a compaction's doing)
		"assistant",
		"note changed /w/notes.md,/w/other.md", // (app.tsx is the agent's own writing)
		"assistant",
	}
	if len(got) != len(want) {
		t.Fatalf("entries: %q", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("entry %d: %q, want %q", i, got[i], want[i])
		}
	}
}

// A file a shell command wrote is the agent's own doing too.
func TestClaudeNotedAfterShellEdit(t *testing.T) {
	c := NewClaude()
	c.Feed(lines(
		`{"type":"assistant","uuid":"a1","message":{"content":[{"type":"tool_use","id":"1","name":"Bash","input":{"command":"sed -i s/a/b/ x.go","description":"patch x"}}]}}`,
		`{"type":"user","uuid":"r1","toolUseResult":{"stdout":"","bashEditDiff":{"files":[{"filePath":"/w/x.go","hunks":[{"oldStart":1,"oldLines":1,"newStart":1,"newLines":1,"lines":["-a","+b"]}]}],"changedFiles":["/w/x.go"]}},"message":{"content":[{"type":"tool_result","tool_use_id":"1","content":"ok"}]}}`,
		`{"type":"attachment","uuid":"e1","attachment":{"type":"edited_text_file","filename":"/w/x.go","snippet":""}}`,
		`{"type":"attachment","uuid":"e2","attachment":{"type":"edited_text_file","filename":"/w/y.go","snippet":""}}`,
	))
	var notes []*Noted
	for _, e := range c.Since(0) {
		for _, p := range e.Parts {
			if p.Noted != nil {
				notes = append(notes, p.Noted)
			}
		}
	}
	if len(notes) != 1 {
		t.Fatalf("notes: %d", len(notes))
	}
	if len(notes[0].Paths) != 1 || notes[0].Paths[0] != "/w/y.go" {
		t.Errorf("noted %+v: the file the command wrote is not news", notes[0])
	}
}

// What the review of 2026-10-27 found: a file named by the start of a
// longer name is not that file; a message sent while the agent works names
// files too; an edit that failed wrote nothing; a command's own list of the
// files it changed counts, beyond those whose patch is kept; and what Since
// gives does not change under its reader.
func TestClaudeNotedEdges(t *testing.T) {
	c := NewClaude()
	c.Feed(lines(
		`{"type":"user","uuid":"u1","message":{"role":"user","content":"compare @web/a.ts.bak with the old one"}}`,
		`{"type":"attachment","uuid":"f1","attachment":{"type":"file","filename":"/w/web/a.ts","displayPath":"web/a.ts","content":"…"}}`,
		`{"type":"attachment","uuid":"f2","attachment":{"type":"file","filename":"/w/web/a.ts.bak","displayPath":"web/a.ts.bak","content":"…"}}`,
	))
	var paths []string
	for _, e := range c.Since(0) {
		for _, p := range e.Parts {
			if p.Noted != nil {
				paths = append(paths, p.Noted.Paths...)
			}
		}
	}
	if len(paths) != 1 || paths[0] != "web/a.ts.bak" {
		t.Errorf("brought %q: only the file named is brought in", paths)
	}

	// A message sent while the agent works names files as any other does.
	c2 := NewClaude()
	c2.Feed(lines(
		`{"type":"assistant","uuid":"a1","message":{"content":[{"type":"text","text":"working"}]}}`,
		`{"type":"attachment","uuid":"q1","attachment":{"type":"queued_command","commandMode":"prompt","prompt":"also read @notes.md"}}`,
		`{"type":"attachment","uuid":"f1","attachment":{"type":"file","filename":"/w/notes.md","displayPath":"notes.md","content":"…"}}`,
	))
	found := false
	for _, e := range c2.Since(0) {
		for _, p := range e.Parts {
			if p.Noted != nil && p.Noted.Kind == "brought" && len(p.Noted.Paths) == 1 && p.Noted.Paths[0] == "notes.md" {
				found = true
			}
		}
	}
	if !found {
		t.Error("a file named in a message sent mid-turn is not brought in")
	}

	// An edit that failed wrote nothing; a command's changedFiles count.
	c3 := NewClaude()
	c3.Feed(lines(
		`{"type":"assistant","uuid":"a1","message":{"content":[{"type":"tool_use","id":"1","name":"Edit","input":{"file_path":"/w/x.go","old_string":"a","new_string":"b"}},{"type":"tool_use","id":"2","name":"Bash","input":{"command":"./fix.sh","description":"fix"}}]}}`,
		`{"type":"user","uuid":"r1","message":{"content":[{"type":"tool_result","tool_use_id":"1","is_error":true,"content":"String not found"}]}}`,
		`{"type":"user","uuid":"r2","toolUseResult":{"stdout":"","bashEditDiff":{"files":[],"changedFiles":["/w/y.go"],"moreFiles":0}},"message":{"content":[{"type":"tool_result","tool_use_id":"2","content":"ok"}]}}`,
		`{"type":"attachment","uuid":"e1","attachment":{"type":"edited_text_file","filename":"/w/x.go","snippet":""}}`,
		`{"type":"attachment","uuid":"e2","attachment":{"type":"edited_text_file","filename":"/w/y.go","snippet":""}}`,
	))
	var changed []string
	for _, e := range c3.Since(0) {
		for _, p := range e.Parts {
			if p.Noted != nil {
				changed = append(changed, p.Noted.Paths...)
			}
		}
	}
	if len(changed) != 1 || changed[0] != "/w/x.go" {
		t.Errorf("changed %q: the edit that failed wrote nothing, the command wrote y.go", changed)
	}

	// What Since gave stays as it was given.
	c4 := NewClaude()
	c4.Feed(lines(`{"type":"attachment","uuid":"e1","attachment":{"type":"edited_text_file","filename":"/w/a.md","snippet":""}}`))
	first := c4.Since(0)
	c4.Feed(lines(`{"type":"attachment","uuid":"e2","attachment":{"type":"edited_text_file","filename":"/w/b.md","snippet":""}}`))
	if len(first) != 1 || first[0].Parts[0].Noted == nil || len(first[0].Parts[0].Noted.Paths) != 1 {
		t.Errorf("what Since gave changed under its reader: %+v", first[0].Parts[0].Noted)
	}
}

// A reference ends where the sentence goes on, in Chinese as in English;
// a command that wrote files and then failed wrote them all the same.
func TestClaudeNotedSentenceAndFailedCommand(t *testing.T) {
	c := NewClaude()
	c.Feed(lines(
		`{"type":"user","uuid":"u1","message":{"role":"user","content":"请读 @web/a.ts，然后说说 @b.md。"}}`,
		`{"type":"attachment","uuid":"f1","attachment":{"type":"file","filename":"/w/web/a.ts","displayPath":"web/a.ts","content":"…"}}`,
		`{"type":"attachment","uuid":"f2","attachment":{"type":"file","filename":"/w/b.md","displayPath":"b.md","content":"…"}}`,
	))
	var brought []string
	for _, e := range c.Since(0) {
		for _, p := range e.Parts {
			if p.Noted != nil {
				brought = append(brought, p.Noted.Paths...)
			}
		}
	}
	if len(brought) != 2 || brought[0] != "web/a.ts" || brought[1] != "b.md" {
		t.Errorf("brought %q: a name ends where the sentence goes on", brought)
	}

	c2 := NewClaude()
	c2.Feed(lines(
		`{"type":"assistant","uuid":"a1","message":{"content":[{"type":"tool_use","id":"1","name":"Bash","input":{"command":"./build.sh","description":"build"}}]}}`,
		`{"type":"user","uuid":"r1","toolUseResult":{"stdout":"","bashEditDiff":{"files":[],"changedFiles":["/w/gen.go"]}},"message":{"content":[{"type":"tool_result","tool_use_id":"1","is_error":true,"content":"exit 1"}]}}`,
		`{"type":"attachment","uuid":"e1","attachment":{"type":"edited_text_file","filename":"/w/gen.go","snippet":""}}`,
	))
	for _, e := range c2.Since(0) {
		for _, p := range e.Parts {
			if p.Noted != nil {
				t.Errorf("noted %+v: the command wrote it before it failed", p.Noted)
			}
		}
	}
}

// Work the agent stopped itself, or that Claude Code says of its own
// accord has ended, is no longer said to be going; and one notice may be
// of several at once (a session that ended with work still going).
func TestClaudeRunningEnds(t *testing.T) {
	going := func(rows ...string) []Running {
		c := NewClaude()
		c.Feed(lines(rows...))
		return c.Running()
	}
	start := func(id, call, what string) []string {
		return []string{
			`{"type":"assistant","uuid":"a` + id + `","message":{"content":[{"type":"tool_use","id":"` + call + `","name":"Bash","input":{"command":"./run.sh","description":"` + what + `"}}]}}`,
			`{"type":"user","uuid":"r` + id + `","toolUseResult":{"backgroundTaskId":"` + id + `","stdout":""},"message":{"content":[{"type":"tool_result","tool_use_id":"` + call + `","content":"Command running in background with ID: ` + id + `"}]}}`,
		}
	}
	if r := going(start("b1", "t1", "serve")...); len(r) != 1 || r[0].ID != "b1" {
		t.Fatalf("set going: %+v", r)
	}
	// Stopped by the agent: nothing is sent to say it ended.
	stop := append(start("b1", "t1", "serve"),
		`{"type":"assistant","uuid":"a2","message":{"content":[{"type":"tool_use","id":"t2","name":"TaskStop","input":{"task_id":"b1"}}]}}`,
		`{"type":"user","uuid":"r2","message":{"content":[{"type":"tool_result","tool_use_id":"t2","content":"stopped"}]}}`)
	if r := going(stop...); len(r) != 0 {
		t.Errorf("after it was stopped: %+v", r)
	}
	// Claude Code says of its own accord that it ended.
	done := append(start("b2", "t1", "serve"),
		`{"type":"attachment","uuid":"s1","attachment":{"type":"task_status","taskId":"b2","status":"completed","description":"serve"}}`)
	if r := going(done...); len(r) != 0 {
		t.Errorf("after it was said to be done: %+v", r)
	}
	// Still going, with the latest it has said of itself.
	on := append(start("b3", "t1", "serve"),
		`{"type":"attachment","uuid":"s1","attachment":{"type":"task_status","taskId":"b3","status":"running","deltaSummary":"4 of 9 done"}}`)
	if r := going(on...); len(r) != 1 || r[0].Note != "4 of 9 done" {
		t.Errorf("while it goes on: %+v", r)
	}
	// One notice of three at once, with a marker among them.
	var many []string
	many = append(many, start("b4", "t1", "one")...)
	many = append(many, start("b5", "t2", "two")...)
	many = append(many, start("b6", "t3", "three")...)
	if r := going(many...); len(r) != 3 {
		t.Fatalf("three going: %+v", r)
	}
	many = append(many, `{"type":"user","uuid":"n1","message":{"role":"user","content":"<task-notification>\n<task-id>b4</task-id>\n<task-id>b5</task-id>\n<task-id>__orphan_summary__:shell</task-id>\n<status>stopped</status>\n<summary>2 background shell command tasks didn't finish before the previous session ended.</summary>\n</task-notification>"}`+`}`)
	r := going(many...)
	if len(r) != 1 || r[0].ID != "b6" {
		t.Errorf("after a notice of several: %+v", r)
	}
}

// How hard the model was asked to think, as the log says it on each answer
// (Claude Code's /effort, its status line's "·XH"): the latest stands, and
// a row Claude Code made up itself says nothing.
func TestClaudeEffort(t *testing.T) {
	c := NewClaude()
	c.Feed(lines(
		`{"type":"assistant","uuid":"a1","effort":"high","message":{"model":"claude-opus-5-5","content":[{"type":"text","text":"one"}]}}`,
		`{"type":"assistant","uuid":"a2","effort":"xhigh","message":{"model":"claude-opus-5-5","content":[{"type":"text","text":"two"}]}}`,
		`{"type":"assistant","uuid":"a3","effort":"max","message":{"model":"<synthetic>","content":[{"type":"text","text":"API Error"}]}}`,
	))
	if m := c.Meta(); m.Effort != "xhigh" {
		t.Errorf("effort %q, want xhigh", m.Effort)
	}
	// (Said nowhere: nothing, not a guess.)
	plain := NewClaude()
	plain.Feed(lines(`{"type":"assistant","uuid":"a1","message":{"model":"claude-opus-5-5","content":[{"type":"text","text":"one"}]}}`))
	if m := plain.Meta(); m.Effort != "" {
		t.Errorf("effort %q, want none", m.Effort)
	}
}
