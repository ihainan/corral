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
	if m := p[4].Tool; m.Detail != "https://example.com" {
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
