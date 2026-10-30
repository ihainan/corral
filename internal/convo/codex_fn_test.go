package convo

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"testing"
)

// Rows of a Codex in its function call mode (plans/5a-codex-parse.md), as
// its log has them: a call, what came back, and what it did told of when
// done, of the same id.
func fnCall(t *testing.T, id, name string, args any) string {
	b, _ := json.Marshal(args)
	return codexLine(t, "response_item", map[string]any{"type": "function_call", "name": name, "call_id": id, "arguments": string(b)})
}

func fnCallNS(t *testing.T, id, ns, name string, args any) string {
	b, _ := json.Marshal(args)
	return codexLine(t, "response_item", map[string]any{"type": "function_call", "namespace": ns, "name": name, "call_id": id, "arguments": string(b)})
}

func fnOut(t *testing.T, id string, out any) string {
	return codexLine(t, "response_item", map[string]any{"type": "function_call_output", "call_id": id, "output": out})
}

func fnDone(t *testing.T, item map[string]any) string {
	return codexLine(t, "event_msg", map[string]any{"type": "item_completed", "item": item})
}

func fnUser(t *testing.T, id, text string) string {
	return fnDone(t, map[string]any{"type": "UserMessage", "id": id, "content": []any{map[string]any{"type": "text", "text": text}}})
}

func feedAll(rows ...string) *Codex {
	c := NewCodex()
	c.Feed([]byte(strings.Join(rows, "")))
	return c
}

func toolsOf(c *Codex) []*Tool {
	var out []*Tool
	for _, e := range c.Since(0) {
		for _, p := range e.Parts {
			if p.Tool != nil {
				out = append(out, p.Tool)
			}
		}
	}
	return out
}

// A command of a call: one line, its command, what it printed (as told
// when it was done), whether it failed — not the call's JSON and the
// command again.
func TestCodexFnCommandOnce(t *testing.T) {
	c := feedAll(
		fnUser(t, "u1", "看看状态"),
		codexLine(t, "event_msg", map[string]any{"type": "task_started"}),
		fnCall(t, "call_1", "exec_command", map[string]any{"cmd": "git status --short", "workdir": "/w", "yield_time_ms": 1000}),
		fnCall(t, "call_2", "exec_command", map[string]any{"cmd": "false", "justification": "看看会不会失败"}),
		fnOut(t, "call_1", "Chunk ID: ab12\nWall time: 0.01 seconds\nProcess exited with code 0\nOriginal token count: 3\nOutput:\n M a.go\n"),
		fnOut(t, "call_2", "Chunk ID: ab13\nWall time: 0.01 seconds\nProcess exited with code 1\nOutput:\n"),
		fnDone(t, map[string]any{"type": "CommandExecution", "id": "call_1", "command": []string{"/bin/zsh", "-lc", "git status --short"}, "parsed_cmd": []any{map[string]any{"cmd": "git status --short"}}, "status": "completed", "aggregated_output": " M a.go\n", "exit_code": 0}),
	)
	ts := toolsOf(c)
	if len(ts) != 2 || ts[0].Name != "Bash" || ts[0].Detail != "git status --short" || ts[0].Result != " M a.go\n" || !ts[0].Done || ts[0].Error {
		t.Fatalf("first: %+v", ts)
	}
	if ts[1].Title != "看看会不会失败" || !ts[1].Error || !strings.HasPrefix(ts[1].Result, "Exit code 1") {
		t.Errorf("second: %+v", ts[1])
	}
}

// A patch applied by a command: its files as edits; the change told of
// when done fills in a file moved and one the patch did not name.
func TestCodexFnPatch(t *testing.T) {
	patch := "apply_patch <<'PATCH'\n*** Begin Patch\n*** Update File: auth/login.py\n@@\n-a\n+b\n+c\n*** Add File: auth/new.py\n+x\n*** End Patch\nPATCH"
	c := feedAll(
		fnCall(t, "call_p", "exec_command", map[string]any{"cmd": patch}),
		fnOut(t, "call_p", "Wall time: 0.0 seconds\nOutput:\nSuccess.\n"),
		fnDone(t, map[string]any{"type": "FileChange", "id": "call_p", "status": "completed", "changes": map[string]any{
			"/w/auth/login.py": map[string]any{"type": "update", "unified_diff": "@@ -1 +1,2 @@\n-a\n+b\n+c\n", "move_path": "/w/auth/signin.py"},
			"/w/auth/new.py":   map[string]any{"type": "add", "content": "x\n"},
			"/w/auth/extra.py": map[string]any{"type": "update", "unified_diff": "@@ -3 +3 @@\n-q\n+r\n"},
		}}),
	)
	ts := toolsOf(c)
	names := []string{}
	for _, x := range ts {
		names = append(names, x.Name+":"+x.Detail+":"+x.Title)
	}
	if len(ts) != 3 || ts[0].Name != "Edit" || ts[0].Add != 2 || ts[0].Del != 1 || !strings.Contains(ts[0].Title, "signin.py") || ts[1].Name != "Write" || ts[2].Detail != "/w/auth/extra.py" || ts[2].Add != 1 {
		t.Fatalf("files: %v", names)
	}
	for _, x := range ts {
		if !x.Done || x.Error {
			t.Errorf("not done, or failed: %+v", x)
		}
	}
}

// What is typed to a command still running is never shown: keys by name,
// else how many characters. Nowhere in what the view is given.
func TestCodexFnStdinPrivate(t *testing.T) {
	c := feedAll(
		fnCall(t, "call_a", "write_stdin", map[string]any{"session_id": 63529, "chars": "sk-SENTINEL-do-not-show\n", "yield_time_ms": 1000}),
		fnOut(t, "call_a", "Wall time: 0.1 seconds\nOutput:\nok\n"),
		fnCall(t, "call_b", "write_stdin", map[string]any{"session_id": 63529, "chars": "\u0003"}),
		fnCall(t, "call_c", "write_stdin", map[string]any{"session_id": "63529", "chars": ""}),
		fnCall(t, "call_d", "write_stdin", `not json: sk-SENTINEL-do-not-show`),
	)
	ts := toolsOf(c)
	if len(ts) != 4 || ts[0].Stdin == nil || ts[0].Stdin.Chars != 24 || ts[0].Stdin.Session != "63529" || ts[0].Result != "ok\n" {
		t.Fatalf("typed: %+v", ts)
	}
	if k := ts[1].Stdin.Keys; len(k) != 1 || k[0] != "Ctrl+C" {
		t.Errorf("keys: %v", k)
	}
	if s := ts[2].Stdin; len(s.Keys) != 0 || s.Chars != 0 || s.Session != "63529" {
		t.Errorf("a look: %+v", s)
	}
	b, _ := json.Marshal(c.Since(0))
	if strings.Contains(string(b), "SENTINEL") {
		t.Errorf("what was typed is shown: %s", b)
	}
}

// Its other tools, as the ones the view knows; one it does not know, by
// its name and arguments.
func TestCodexFnTools(t *testing.T) {
	c := feedAll(
		fnCall(t, "c1", "view_image", map[string]any{"path": "/w/shot.png"}),
		fnCallNS(t, "c2", "mcp__codex_apps__search_service", "_web_run", map[string]any{"system2_search_query": []any{map[string]any{"q": "codex rollout"}}, "response_length": "short"}),
		fnCallNS(t, "c3", "mcp__codex_apps__search_service", "_web_run", map[string]any{"open": []any{map[string]any{"ref_id": "https://example.com/a"}}}),
		fnCallNS(t, "c4", "mcp__stitch__", "list_projects", map[string]any{"filter": "x"}),
		fnCallNS(t, "c5", "collaboration", "spawn_agent", map[string]any{"task_name": "检查配置", "message": "只读检查 configs"}),
		fnCall(t, "c6", "brand_new_tool", map[string]any{"b": 2, "a": 1}),
	)
	ts := toolsOf(c)
	want := []string{"Read:/w/shot.png", "WebSearch:codex rollout", "WebFetch:https://example.com/a", "mcp__stitch__list_projects:{\"filter\":\"x\"}", "Agent:只读检查 configs", "brand_new_tool:{\"a\":1,\"b\":2}"}
	var got []string
	for _, x := range ts {
		got = append(got, x.Name+":"+x.Detail)
	}
	if strings.Join(got, "|") != strings.Join(want, "|") || ts[4].Title != "检查配置" {
		t.Errorf("tools\n%v\nwant\n%v", got, want)
	}
}

// update_plan's list is the task list; a later one replaces it, a failed
// one does not, and one taken back (a backtrack) leaves the one before.
func TestCodexPlan(t *testing.T) {
	plan := func(id string, steps ...string) string {
		var p []any
		for i, s := range steps {
			st := "pending"
			if i == 0 {
				st = "in_progress"
			}
			p = append(p, map[string]any{"step": s, "status": st})
		}
		return fnCall(t, id, "update_plan", map[string]any{"explanation": "why", "plan": p})
	}
	turn := func(rows ...string) []string {
		return append([]string{codexLine(t, "event_msg", map[string]any{"type": "task_started"})}, rows...)
	}
	rows := turn(fnUser(t, "u1", "做吧"), plan("p1", "读代码", "改代码"), fnOut(t, "p1", "Plan updated"))
	c := feedAll(rows...)
	if ts := c.Tasks(); len(ts) != 2 || ts[0].Subject != "读代码" || ts[0].Status != "in_progress" {
		t.Fatalf("tasks %+v", ts)
	}
	rows = append(rows, turn(fnUser(t, "u2", "再改"), plan("p2", "只改一处"), fnOut(t, "p2", "Plan updated"), plan("p3", "坏的"), fnOut(t, "p3", `{"output":"invalid plan","metadata":{"exit_code":1}}`))...)
	c = feedAll(rows...)
	if ts := c.Tasks(); len(ts) != 1 || ts[0].Subject != "只改一处" {
		t.Fatalf("after a later one and a failed one: %+v", ts)
	}
	rows = append(rows, codexLine(t, "event_msg", map[string]any{"type": "thread_rolled_back", "num_turns": 1}))
	c = feedAll(rows...)
	if ts := c.Tasks(); len(ts) != 2 || ts[0].Subject != "读代码" {
		t.Errorf("after its turn was taken back: %+v", ts)
	}
}

// A question put to the person: its words and choices; taken
// ({"accepted": true}) is not answered: their next message is.
func TestCodexAskAsync(t *testing.T) {
	q := map[string]any{"questions": []any{map[string]any{"title": "你的技能主要属于哪种交互？", "options": []any{"网页", "外部 API", "两者都需要"}}}}
	rows := []string{fnUser(t, "u1", "帮我看看"), fnCall(t, "q1", "request_user_input_async", q), fnOut(t, "q1", `{"accepted":true}`)}
	c := feedAll(rows...)
	ts := toolsOf(c)
	if len(ts) != 1 || ts[0].Done || len(ts[0].Asks) != 1 || len(ts[0].Asks[0].Options) != 3 || ts[0].Title != "你的技能主要属于哪种交互？" {
		t.Fatalf("asked: %+v", ts)
	}
	c = feedAll(append(rows, fnUser(t, "u2", "两者都需要"))...)
	if ts := toolsOf(c); !ts[0].Done {
		t.Error("not answered by their next message")
	}
}

// An older Codex's account: the person's message (event user_message),
// an error, a review's conclusion, a search, a compaction.
func TestCodexOlderEvents(t *testing.T) {
	c := feedAll(
		codexLine(t, "event_msg", map[string]any{"type": "user_message", "message": "旧版里发的消息"}),
		codexLine(t, "event_msg", map[string]any{"type": "error", "message": "Selected model is at capacity."}),
		codexLine(t, "event_msg", map[string]any{"type": "web_search_end", "call_id": "ws_1", "query": "codex docs"}),
		codexLine(t, "event_msg", map[string]any{"type": "exited_review_mode", "review_output": map[string]any{"overall_explanation": "No findings."}}),
		codexLine(t, "event_msg", map[string]any{"type": "context_compacted"}),
	)
	es := c.Since(0)
	if len(es) != 5 || es[0].Role != RoleUser || c.Title() != "旧版里发的消息" {
		t.Fatalf("entries %+v", es)
	}
	if tr := es[1].Parts[0].Trouble; tr == nil || tr.Kind != "api" || !strings.Contains(tr.Text, "capacity") {
		t.Errorf("error: %+v", es[1].Parts[0])
	}
	if es[2].Parts[0].Tool == nil || es[2].Parts[0].Tool.Name != "WebSearch" || es[3].Parts[0].Text != "No findings." || es[4].Parts[0].Event == nil || es[4].Parts[0].Event.Kind != EventCompact {
		t.Errorf("the rest: %+v %+v %+v", es[2].Parts[0], es[3].Parts[0], es[4].Parts[0])
	}
}

// What came back, its wrappings off: {output, metadata.exit_code}, a
// command's head with its exit code, content blocks (a picture counted).
func TestCodexResult(t *testing.T) {
	for _, c := range []struct {
		raw    string
		text   string
		failed bool
		images int
	}{
		{`"{\"output\":\"Success.\\n\",\"metadata\":{\"exit_code\":0,\"duration_seconds\":0.1}}"`, "Success.\n", false, 0},
		{`"{\"output\":\"boom\",\"metadata\":{\"exit_code\":2}}"`, "Exit code 2\nboom", true, 0},
		{`"Chunk ID: x\nWall time: 0 seconds\nProcess exited with code 3\nOutput:\nno\n"`, "Exit code 3\nno\n", true, 0},
		{`[{"type":"input_text","text":"看了"},{"type":"input_image","image_url":"data:,"}]`, "看了", false, 1},
		{`"{\"goal\":{\"objective\":\"x\"}}"`, `{"goal":{"objective":"x"}}`, false, 0},
	} {
		text, failed, images := codexResult(json.RawMessage(c.raw))
		if text != c.text || failed != c.failed || images != c.images {
			t.Errorf("%s: %q %v %d", c.raw, text, failed, images)
		}
	}
}

// The code mode's script: what it calls by name (commands read out of it);
// once its things are told of on their own, those are what shows.
func TestCodexScriptMixed(t *testing.T) {
	script := "const r = await tools.exec_command({cmd:\"go test ./...\"});\nawait tools.view_image({path: p});\nconst w = await tools.web__run({search_query:[{q:\"glm reasoning_effort\"}]});"
	call := codexLine(t, "response_item", map[string]any{"type": "custom_tool_call", "name": "exec", "call_id": "call_s", "input": script})
	c := feedAll(call)
	var got []string
	for _, x := range toolsOf(c) {
		got = append(got, x.Name+":"+x.Detail)
	}
	if strings.Join(got, "|") != "Bash:go test ./...|Read:|WebSearch:glm reasoning_effort" {
		t.Fatalf("script: %v", got)
	}
	c = feedAll(call, fnDone(t, map[string]any{"type": "CommandExecution", "id": "exec-1", "command": []string{"go", "test", "./..."}, "status": "completed", "aggregated_output": "ok", "exit_code": 0}))
	got = nil
	for _, x := range toolsOf(c) {
		got = append(got, x.Name+":"+x.Detail)
	}
	if strings.Join(got, "|") != "Bash:go test ./..." {
		t.Errorf("told of on their own: %v", got)
	}
}

// Read whole, a line at a time, or in two parts (the earlier put before):
// the same lines, each with the same command, files, result and state.
func TestCodexFnSplit(t *testing.T) {
	patch := "apply_patch <<'P'\n*** Begin Patch\n*** Update File: a.py\n@@\n-a\n+b\n*** End Patch\nP"
	rows := []string{
		fnUser(t, "u1", "开始"),
		codexLine(t, "event_msg", map[string]any{"type": "task_started"}),
		fnCall(t, "c1", "exec_command", map[string]any{"cmd": "ls"}),
		fnOut(t, "c1", "Wall time: 0 seconds\nOutput:\na\n"),
		fnDone(t, map[string]any{"type": "CommandExecution", "id": "c1", "command": []string{"ls"}, "status": "completed", "aggregated_output": "a\n", "exit_code": 0}),
		fnCall(t, "c2", "exec_command", map[string]any{"cmd": patch}),
		fnOut(t, "c2", "Wall time: 0 seconds\nOutput:\nSuccess.\n"),
		fnDone(t, map[string]any{"type": "FileChange", "id": "c2", "status": "completed", "changes": map[string]any{"/w/a.py": map[string]any{"type": "update", "unified_diff": "@@ -1 +1 @@\n-a\n+b\n"}}}),
		fnCall(t, "c3", "write_stdin", map[string]any{"session_id": 7, "chars": "\u0003"}),
		fnOut(t, "c3", "Wall time: 0 seconds\nOutput:\n^C\n"),
		fnCall(t, "c4", "update_plan", map[string]any{"plan": []any{map[string]any{"step": "一步", "status": "completed"}}}),
		fnOut(t, "c4", "Plan updated"),
		codexLine(t, "event_msg", map[string]any{"type": "task_complete"}),
	}
	whole := shownOf(feedAll(rows...))
	byLine := NewCodex()
	for _, r := range rows {
		byLine.Feed([]byte(r))
	}
	if got := shownOf(byLine); got != whole {
		t.Errorf("a line at a time:\n%s\nwhole:\n%s", got, whole)
	}
	for k := 1; k < len(rows); k++ {
		later := feedAll(rows[k:]...)
		later.Prepend(feedAll(rows[:k]...))
		if got := shownOf(later); got != whole {
			t.Errorf("parted at %d:\n%s\nwhole:\n%s", k, got, whole)
		}
		if ts := later.Tasks(); len(ts) != 1 {
			t.Errorf("parted at %d: tasks %+v", k, ts)
		}
	}
}

// shownOf: what the view shows of a conversation, line by line, in no
// order (where it was parted, a line may stand where its end was told).
func shownOf(c *Codex) string {
	var out []string
	for _, e := range c.Since(0) {
		if e.Gone {
			continue
		}
		for _, p := range e.Parts {
			if p.Tool == nil {
				out = append(out, e.Role+" "+p.Kind+" "+p.Text)
				continue
			}
			x := p.Tool
			out = append(out, fmt.Sprintf("%s %s %q %q done=%v err=%v res=%q +%d-%d stdin=%v", e.Role, x.Name, x.Title, x.Detail, x.Done, x.Error, x.Result, x.Add, x.Del, x.Stdin != nil))
		}
	}
	sort.Strings(out)
	return strings.Join(out, "\n")
}
