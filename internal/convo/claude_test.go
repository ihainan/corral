package convo

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
