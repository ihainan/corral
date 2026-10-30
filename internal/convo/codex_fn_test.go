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

// review446: what review 446 found, each as it would happen.
func TestCodexFnReview446(t *testing.T) {
	// 1. A search's words only from the search's own arguments, not what a
	// script types to a command.
	script := "await tools.write_stdin({session_id:1, chars:'{q:\"PRIVATE_SENTINEL\"}'});\nawait tools.web__run({search_query:[{q:\"public\"}]});"
	c := feedAll(codexLine(t, "response_item", map[string]any{"type": "custom_tool_call", "name": "exec", "call_id": "s1", "input": script}))
	if b, _ := json.Marshal(c.Since(0)); strings.Contains(string(b), "PRIVATE") {
		t.Errorf("1: %s", b)
	}
	// 2. What comes back of a call read in a part before its telling: on the
	// telling's line, the page told.
	later := feedAll(fnDone(t, map[string]any{"type": "ImageView", "id": "v1", "path": "/w/a.png"}))
	later.Prepend(feedAll(fnCall(t, "v1", "view_image", map[string]any{"path": "/w/a.png"})))
	rev := later.Rev()
	later.Feed([]byte(fnOut(t, "v1", []any{map[string]any{"type": "input_image", "image_url": "data:,"}})))
	if ts := toolsOf(later); len(ts) != 1 || ts[0].Images != 1 || len(later.Since(rev)) == 0 {
		t.Errorf("2: %+v, since %d", ts, len(later.Since(rev)))
	}
	// 3, 4. A call of two files, its telling naming one, parted: both kept,
	// the line changed told.
	patch := "apply_patch <<'P'\n*** Begin Patch\n*** Update File: a.py\n@@\n-a\n+b\n*** Update File: b.py\n@@\n-c\n+d\n*** End Patch\nP"
	tell := fnDone(t, map[string]any{"type": "FileChange", "id": "p1", "status": "completed", "changes": map[string]any{"/w/0.py": map[string]any{"type": "update", "unified_diff": "@@ -1 +1 @@\n-x\n+y\n"}, "/w/a.py": map[string]any{"type": "update", "unified_diff": "@@ -1 +1 @@\n-a\n+b\n"}}})
	later = feedAll(tell)
	rev = later.Rev()
	later.Prepend(feedAll(fnCall(t, "p1", "exec_command", map[string]any{"cmd": patch})))
	var paths []string
	for _, x := range toolsOf(later) {
		paths = append(paths, x.Detail)
	}
	sort.Strings(paths)
	if strings.Join(paths, ",") != "/w/0.py,a.py,b.py" || len(later.Since(rev)) == 0 {
		t.Errorf("3, 4: %v, since %d", paths, len(later.Since(rev)))
	}
	// 5. The same change told in both parts: its files once.
	later = feedAll(tell)
	later.Prepend(feedAll(tell))
	if ts := toolsOf(later); len(ts) != 2 {
		t.Errorf("5: %d lines", len(ts))
	}
	// 6. A failed MCP call stays failed; what came back is its words.
	c = feedAll(
		fnCallNS(t, "m1", "mcp__x__", "do", map[string]any{}),
		fnDone(t, map[string]any{"type": "McpToolCall", "id": "m1", "server": "x", "tool": "do", "status": "failed", "result": map[string]any{"content": []any{map[string]any{"type": "text", "text": "no"}}, "isError": true}}),
		fnOut(t, "m1", `{"content":[{"type":"text","text":"bad thing"}],"isError":true}`),
	)
	if ts := toolsOf(c); len(ts) != 1 || !ts[0].Error || strings.Contains(ts[0].Result, "isError") {
		t.Errorf("6: %+v", ts)
	}
	// 10, 11. A patch of many files: those kept, the rest counted; a file
	// moved, said.
	var b strings.Builder
	b.WriteString("apply_patch <<'P'\n*** Begin Patch\n*** Update File: m.py\n*** Move to: n.py\n@@\n-a\n+b\n")
	for i := 0; i < 100; i++ {
		fmt.Fprintf(&b, "*** Add File: f%d.py\n+x\n", i)
	}
	b.WriteString("*** End Patch\nP")
	ts := toolsOf(feedAll(fnCall(t, "big", "exec_command", map[string]any{"cmd": b.String()})))
	if len(ts) != maxFiles || ts[len(ts)-1].MoreFiles != 101-maxFiles || ts[0].Title != "m.py → n.py" {
		t.Errorf("10, 11: %d lines, more %d, %q", len(ts), ts[len(ts)-1].MoreFiles, ts[0].Title)
	}
	// 12. Many questions: the latest answered by the next message.
	var rows []string
	for i := 0; i < maxAsked+1; i++ {
		rows = append(rows, fnCall(t, fmt.Sprintf("q%d", i), "request_user_input_async", map[string]any{"questions": []any{map[string]any{"title": "q"}}}), fnOut(t, fmt.Sprintf("q%d", i), `{"accepted":true}`))
	}
	ts = toolsOf(feedAll(append(rows, fnUser(t, "u", "答"))...))
	if !ts[len(ts)-1].Done {
		t.Error("12: the latest question not answered")
	}
}

// review446 8: a plan of what is left out (the conversation longer than
// is kept) is no list.
func TestCodexPlanTrimmed(t *testing.T) {
	rows := []string{fnCall(t, "p", "update_plan", map[string]any{"plan": []any{map[string]any{"step": "x", "status": "pending"}}}), fnOut(t, "p", "Plan updated")}
	for i := 0; i < maxEntries; i++ {
		rows = append(rows, fnUser(t, fmt.Sprintf("u%d", i), "m"))
	}
	if ts := feedAll(rows...).Tasks(); ts != nil {
		t.Errorf("tasks of what was left out: %+v", ts)
	}
}

// review446 9: a call of the code mode hidden in the part before: what came
// back of it in this part is let be.
func TestCodexHiddenAcrossParts(t *testing.T) {
	before := feedAll(
		fnDone(t, map[string]any{"type": "CommandExecution", "id": "exec-1", "command": []string{"ls"}, "status": "completed", "aggregated_output": "", "exit_code": 0}),
		codexLine(t, "response_item", map[string]any{"type": "custom_tool_call", "name": "exec", "call_id": "s9", "input": "await tools.exec_command({cmd:\"ls\"});"}),
	)
	later := feedAll(fnOut(t, "s9", "Script completed\nWall time 0 seconds\nOutput:\n"))
	later.Prepend(before)
	if _, kept := later.orphans["s9"]; kept {
		t.Error("what came back of a hidden call is kept")
	}
}

// review447: what review 447 found, each as it would happen.
func TestCodexFnReview447(t *testing.T) {
	// 1. A call written in a string is no call.
	script := "await tools.write_stdin({session_id:1, chars:'tools.web__run({search_query:[{q:\"PRIVATE\"}]})'});\nawait tools.web__run({search_query:[{q:\"public\"}]});"
	c := feedAll(codexLine(t, "response_item", map[string]any{"type": "custom_tool_call", "name": "exec", "call_id": "s1", "input": script}))
	if b, _ := json.Marshal(c.Since(0)); strings.Contains(string(b), "PRIVATE") {
		t.Errorf("1: %s", b)
	}
	// 2. Parted: the call's picture and failure kept on the telling's line;
	// a file the telling did not name shares its failing.
	later := feedAll(fnDone(t, map[string]any{"type": "ImageView", "id": "v1", "path": "/w/a.png"}))
	later.Prepend(feedAll(fnCall(t, "v1", "view_image", map[string]any{"path": "/w/a.png"}), fnOut(t, "v1", []any{map[string]any{"type": "input_image", "image_url": "data:,"}})))
	if ts := toolsOf(later); len(ts) != 1 || ts[0].Images != 1 {
		t.Errorf("2: %+v", ts)
	}
	patch := "apply_patch <<'P'\n*** Begin Patch\n*** Update File: a.py\n@@\n-a\n+b\n*** Update File: b.py\n@@\n-c\n+d\n*** End Patch\nP"
	later = feedAll(fnDone(t, map[string]any{"type": "FileChange", "id": "p1", "status": "failed", "changes": map[string]any{"/w/a.py": map[string]any{"type": "update", "unified_diff": "@@ -1 +1 @@\n-a\n+b\n"}}}))
	later.Prepend(feedAll(fnCall(t, "p1", "exec_command", map[string]any{"cmd": patch})))
	for _, x := range toolsOf(later) {
		if !x.Error {
			t.Errorf("2: a file of a failed change not failed: %+v", x)
		}
	}
	// 3. A plan of a part put before that is left out (no room): no list.
	var rows []string
	for i := 0; i < maxEntries; i++ {
		rows = append(rows, fnUser(t, fmt.Sprintf("u%d", i), "m"))
	}
	later = feedAll(rows...)
	later.Prepend(feedAll(fnCall(t, "p", "update_plan", map[string]any{"plan": []any{map[string]any{"step": "x", "status": "pending"}}}), fnOut(t, "p", "ok")))
	if ts := later.Tasks(); ts != nil {
		t.Errorf("3: %+v", ts)
	}
	// 4. A script hidden as the parts are put together: what comes back of
	// it after is let be.
	later = feedAll(fnDone(t, map[string]any{"type": "CommandExecution", "id": "exec-1", "command": []string{"ls"}, "status": "completed", "aggregated_output": "", "exit_code": 0}))
	later.Prepend(feedAll(codexLine(t, "response_item", map[string]any{"type": "custom_tool_call", "name": "exec", "call_id": "s9", "input": "await tools.exec_command({cmd:\"ls\"});"})))
	later.Feed([]byte(fnOut(t, "s9", "Script completed\nWall time 0 seconds\nOutput:\n")))
	if _, kept := later.orphans["s9"]; kept {
		t.Error("4: what came back of a call hidden as the parts were put together is kept")
	}
	// 5. A script of many patches: no more lines than an entry holds, each
	// of them in it.
	var b strings.Builder
	for p := 0; p < 4; p++ {
		b.WriteString("await tools.apply_patch(\"*** Begin Patch\\n")
		for i := 0; i < 20; i++ {
			fmt.Fprintf(&b, "*** Add File: p%d_%d.py\\n+x\\n", p, i)
		}
		b.WriteString("*** End Patch\");\n")
	}
	c = feedAll(codexLine(t, "response_item", map[string]any{"type": "custom_tool_call", "name": "exec", "call_id": "s2", "input": b.String()}))
	ts := toolsOf(c)
	if len(ts) > maxParts || len(c.tools["s2"]) != len(ts) {
		t.Errorf("5: %d lines, %d of the call", len(ts), len(c.tools["s2"]))
	}
	for _, x := range c.tools["s2"] {
		if x.at == nil {
			t.Error("5: a line of the call in no entry")
			break
		}
	}
	// 7. An MCP tool's picture counted.
	if _, _, images := codexResult(json.RawMessage(`"{\"content\":[{\"type\":\"image\",\"data\":\"AA==\"}],\"isError\":false}"`)); images != 1 {
		t.Errorf("7: %d pictures", images)
	}
	// 8. The oldest question let go of is over.
	rows = nil
	for i := 0; i < maxAsked+1; i++ {
		rows = append(rows, fnCall(t, fmt.Sprintf("q%d", i), "request_user_input_async", map[string]any{"questions": []any{map[string]any{"title": "q"}}}), fnOut(t, fmt.Sprintf("q%d", i), `{"accepted":true}`))
	}
	if ts := toolsOf(feedAll(append(rows, fnUser(t, "u", "答"))...)); !ts[0].Done {
		t.Error("8: the oldest question left waiting")
	}
}

// review448: what review 448 found, each as it would happen.
func TestCodexFnReview448(t *testing.T) {
	// 1. A call written in a string, after a comment with a quote in it, or
	// in a template's text: no call (neither a search nor a command).
	for _, script := range []string{
		"/* ' */ await tools.write_stdin({session_id:1, chars:'tools.web__run({search_query:[{q:\"PRIVATE\"}]}) tools.exec_command({cmd:\"PRIVATE\"})'});\nawait tools.web__run({search_query:[{q:\"public\"}]});",
		"const s = `x ${`tools.exec_command({cmd:\"PRIVATE\"})`} y`;\nawait tools.write_stdin({session_id:1, chars:s});\nawait tools.web__run({search_query:[{q:\"public\"}]});",
	} {
		c := feedAll(codexLine(t, "response_item", map[string]any{"type": "custom_tool_call", "name": "exec", "call_id": "s1", "input": script}))
		if b, _ := json.Marshal(c.Since(0)); strings.Contains(string(b), "PRIVATE") {
			t.Errorf("1: %s", b)
		}
	}
	// (A call in code, with a template's ${…} in its argument, is read.)
	c := feedAll(codexLine(t, "response_item", map[string]any{"type": "custom_tool_call", "name": "exec", "call_id": "s2", "input": "await tools.exec_command({cmd:\"go test ./...\"}); const x = `a ${f(\"b\")} c`;"}))
	if ts := toolsOf(c); len(ts) != 1 || ts[0].Detail != "go test ./..." {
		t.Errorf("1, a call in code: %+v", ts)
	}
	// 2. Told of on their own in the part before: this part's script lines
	// go too (as reading it whole).
	item := fnDone(t, map[string]any{"type": "CommandExecution", "id": "exec-1", "command": []string{"ls"}, "status": "completed", "aggregated_output": "", "exit_code": 0})
	script := codexLine(t, "response_item", map[string]any{"type": "custom_tool_call", "name": "exec", "call_id": "s3", "input": "await tools.exec_command({cmd:\"pwd\"});"})
	item2 := fnDone(t, map[string]any{"type": "CommandExecution", "id": "exec-2", "command": []string{"pwd"}, "status": "completed", "aggregated_output": "/w", "exit_code": 0})
	whole := shownOf(feedAll(item, script, item2))
	later := feedAll(script)
	later.Prepend(feedAll(item))
	later.Feed([]byte(item2))
	if got := shownOf(later); got != whole {
		t.Errorf("2:\n%s\nwhole:\n%s", got, whole)
	}
	// 3. A script of many patches that asks last: its question kept.
	var b strings.Builder
	for p := 0; p < 4; p++ {
		b.WriteString("await tools.apply_patch(\"*** Begin Patch\\n")
		for i := 0; i < 20; i++ {
			fmt.Fprintf(&b, "*** Add File: p%d_%d.py\\n+x\\n", p, i)
		}
		b.WriteString("*** End Patch\");\n")
	}
	b.WriteString("await tools.request_user_input({questions:[]});")
	c = feedAll(codexLine(t, "response_item", map[string]any{"type": "custom_tool_call", "name": "exec", "call_id": "s4", "input": b.String()}))
	ts := toolsOf(c)
	if len(ts) > maxParts || ts[len(ts)-1].Name != "request_user_input" || len(c.asked) != 1 {
		t.Errorf("3: %d lines, last %s, asked %d", len(ts), ts[len(ts)-1].Name, len(c.asked))
	}
}

// review449: what review 449 found, each as it would happen.
func TestCodexFnReview449(t *testing.T) {
	leaks := func(script string) string {
		c := feedAll(codexLine(t, "response_item", map[string]any{"type": "custom_tool_call", "name": "exec", "call_id": "s", "input": script}))
		b, _ := json.Marshal(c.Since(0))
		return string(b)
	}
	for i, script := range []string{
		// 1. A regular expression with "//" in it, one with a quote; a class with ")".
		"const r = /https?:\\/\\//; await tools.write_stdin({session_id:1, chars:\"PRIVATE\"});",
		"const r = /'/; await tools.write_stdin({session_id:1, chars:'tools.exec_command({cmd:\"PRIVATE\"})'});",
		// 2. A string continued over CRLF; a comment ended by CR.
		"await tools.write_stdin({session_id:1, chars:'a\\\r\ntools.exec_command({cmd:\"PRIVATE\"})'});",
		// 3. A patch typed to a command, or written in a comment.
		"await tools.write_stdin({session_id:1, chars:\"*** Begin Patch\\n*** Add File: PRIVATE\\n+PRIVATE\\n*** End Patch\"});",
		"/* \"*** Begin Patch\\n*** Add File: PRIVATE\\n+x\\n*** End Patch\" */ await tools.exec_command({cmd:\"ls\"});",
		// 8. Templates opened without end: bounded, nothing taken from them.
		strings.Repeat("`${", 2000) + "tools.exec_command({cmd:\"PRIVATE\"})",
	} {
		if b := leaks(script); strings.Contains(b, "PRIVATE") {
			t.Errorf("%d: %s", i, b)
		}
	}
	// (A regular expression's ")" does not end the search's arguments.)
	if b := leaks("await tools.web__run({search_query:[{q:\"one\"}], m: /[)]/, x:[{q:\"two\"}]});"); !strings.Contains(b, "one") || !strings.Contains(b, "two") {
		t.Errorf("1, a class: %s", b)
	}
	// 4. Keys in quotes: the command and the words still read.
	if b := leaks("await tools.exec_command({\"cmd\":\"go test ./...\"}); await tools.web__run({\"search_query\":[{\"q\":\"public\"}]});"); !strings.Contains(b, "go test ./...") || !strings.Contains(b, "public") {
		t.Errorf("4: %s", b)
	}
	// 5. This part's script lines gone as the part before is put before:
	// told as of a revision after the page's.
	later := feedAll(codexLine(t, "response_item", map[string]any{"type": "custom_tool_call", "name": "exec", "call_id": "s3", "input": "await tools.exec_command({cmd:\"pwd\"});"}))
	rev := later.Rev()
	later.Prepend(feedAll(fnDone(t, map[string]any{"type": "CommandExecution", "id": "exec-1", "command": []string{"ls"}, "status": "completed", "aggregated_output": "", "exit_code": 0})))
	told := false
	for _, e := range later.Since(rev) {
		if e.ID == later.entries[len(later.entries)-1].ID {
			told = true
		}
	}
	if !told {
		t.Error("5: the page not told its script line went")
	}
	// 6. A question after many calls: still a question.
	c := feedAll(codexLine(t, "response_item", map[string]any{"type": "custom_tool_call", "name": "exec", "call_id": "s4", "input": strings.Repeat("await tools.exec_command({cmd:\"ls\"});\n", 40) + "await tools.request_user_input({questions:[]});"}))
	if len(c.asked) != 1 {
		t.Errorf("6: asked %d", len(c.asked))
	}
}

// review449 7: a notice with the person's words around it is theirs, whole.
func TestTaskNoticeWithWords(t *testing.T) {
	for _, s := range []string{
		"<task-notification><summary>Agent \"a\" finished</summary></task-notification> Please explain this.",
		"<task-notification><summary>Agent \"a\" finished</summary></task-notification> and <task-notification><summary>Agent \"b\" finished</summary></task-notification>",
	} {
		c := NewClaude()
		b, _ := json.Marshal(map[string]any{"type": "user", "uuid": "u1", "message": map[string]any{"content": s}})
		c.Feed(append(b, '\n'))
		es := c.Since(0)
		if len(es) != 1 || es[0].Role != RoleUser || es[0].Parts[0].Text != s {
			t.Errorf("%q: %+v", s, es)
		}
	}
}

// review450: a script that types to a running command is read for nothing
// (however what it types is come by); a call it only names is no call; a
// fake call in a comment does not crowd out a real one; "x++ / 2" divides.
func TestCodexFnReview450(t *testing.T) {
	shown := func(script string) (string, *Codex) {
		c := feedAll(codexLine(t, "response_item", map[string]any{"type": "custom_tool_call", "name": "exec", "call_id": "s", "input": script}))
		b, _ := json.Marshal(c.Since(0))
		return string(b), c
	}
	for i, script := range []string{
		"const input = \"*** Begin Patch\\n*** Add File: PRIVATE\\n+PRIVATE\\n*** End Patch\";\nawait tools.write_stdin({session_id:1, chars:input});",
		"await tools.write_stdin ({session_id:1, chars:\"*** Begin Patch\\n*** Add File: PRIVATE\\n+PRIVATE\\n*** End Patch\"});",
		"await tools[\"write_stdin\"]({session_id:1, chars:'tools.exec_command({cmd:\"PRIVATE\"})'});",
		"await tools.write_stdin({session_id:1, chars:(() => tools.PRIVATE()).toString()});",
		"await tools.write_stdin({session_id:1, chars:'a\u2028) \"*** Begin Patch\\n*** Add File: PRIVATE\\n+PRIVATE\\n*** End Patch\"'});",
	} {
		if b, _ := shown(script); strings.Contains(b, "PRIVATE") {
			t.Errorf("%d: %s", i, b)
		}
	}
	if _, c := shown("const ask = tools.request_user_input;\ntext(\"no call\");"); len(c.asked) != 0 {
		t.Error("a question only named taken for one asked")
	}
	if b, _ := shown(strings.Repeat("// tools.exec_command({cmd:\"fake\"})\n", 20) + "await tools.exec_command({cmd:\"pwd\"});"); !strings.Contains(b, `"detail":"pwd"`) {
		t.Errorf("a real command crowded out: %s", b)
	}
	if b, _ := shown("let n=4; const x=n++ / 2;\nawait tools.exec_command({cmd:\"pwd\"});"); !strings.Contains(b, `"detail":"pwd"`) {
		t.Errorf("a division taken for a regular expression: %s", b)
	}
}

// review451: a question called optionally, or with a space of JavaScript's
// before its "("; reading not cut short by however many fakes before.
func TestCodexFnReview451(t *testing.T) {
	for _, script := range []string{
		"await tools.request_user_input_async?.({questions:[{title:\"Continue?\"}]});",
		"await tools.request_user_input ({questions:[]});",
		"await tools.request_user_input ({questions:[]});",
	} {
		c := feedAll(codexLine(t, "response_item", map[string]any{"type": "custom_tool_call", "name": "exec", "call_id": "s", "input": script}))
		if len(c.asked) != 1 {
			t.Errorf("%q: not taken for a question", script)
		}
	}
	for _, script := range []string{"const f = tools.request_user_input;", "await tools.request_user_input_other({});", "await mytools.request_user_input({});"} {
		c := feedAll(codexLine(t, "response_item", map[string]any{"type": "custom_tool_call", "name": "exec", "call_id": "s", "input": script}))
		if len(c.asked) != 0 {
			t.Errorf("%q: taken for a question", script)
		}
	}
	c := feedAll(codexLine(t, "response_item", map[string]any{"type": "custom_tool_call", "name": "exec", "call_id": "s", "input": strings.Repeat("// tools.exec_command({cmd:\"fake\"})\n", 300) + "await tools.exec_command({cmd:\"pwd\"});"}))
	if b, _ := json.Marshal(c.Since(0)); !strings.Contains(string(b), `"detail":"pwd"`) {
		t.Errorf("crowded out: %s", b)
	}
}

// The name of a conversation of Codex's: its first message's first line,
// short, no secret in it.
func TestCodexTitle(t *testing.T) {
	for in, want := range map[string]string{
		"测试一下这个 OpenAI 官方 API Key 是否可用：sk-proj-EXAMPLEonlyNOTaREALkey0000000000000000000000000000000000": "测试一下这个 OpenAI 官方 API Key 是否可用：sk-proj…",
		"\n\n  修好登录\n然后跑测试":                                     "修好登录",
		"token ghp_abcdefghijklmnopqrstuvwxyz0123456789 please": "token ghp_abc… please",
		"一二三四五六七八九十一二三四五六七八九十一二三四五六七八九十一二三四五六七八九十多出来的": "一二三四五六七八九十一二三四五六七八九十一二三四五六七八九十一二三四五六七八九十…",
		// (review454: keys of AWS's for a while and of Aliyun's; a long name, a path: kept.)
		"check ASIA1234567890ABCDEF now":                "check ASIA123… now",
		"key LTAI5tAbCdEfGhIjKlMnOpQrSt here":           "key LTAI5tA… here",
		"fix request_user_input_async_handler now":      "fix request_user_input_async_handler now",
		"open /home/ihainan/shared/Projects/AI":         "open /home/ihainan/shared/Projects/AI",
		"token Zx9QwErTyUiOpAsDfGhJkLzXcVbNm1234567 ok": "token Zx9QwEr… ok",
		// (review455: an AWS secret, slashes in it; a capitalised word after ASIA; a long name with a digit; a path.)
		"aws wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY": "aws wJalrXU…",
		"see ASIARegionMapping":                        "see ASIARegionMapping",
		"hex 0123456789ABCDEF0123456789ABCDEF":         "hex 0123456…",
		"fix request_user_input_async_handler2":        "fix request_user_input_async_handler2",
		"ls /tmp/build20261030_abcdefghijklmnopq":      "ls /tmp/build20261030_abcdefghijklmnopq",
	} {
		if got := codexTitle(in); got != want {
			t.Errorf("%q: %q, want %q", in, got, want)
		}
	}
}

// review452: what review 452 found, each as it would happen.
func TestCodexFnReview452(t *testing.T) {
	// 1. A question without choices: its choices an empty list, not null.
	c := feedAll(fnCall(t, "q", "request_user_input_async", map[string]any{"questions": []any{map[string]any{"title": "还有吗？"}}}))
	if b, _ := json.Marshal(c.Since(0)); !strings.Contains(string(b), `"options":[]`) {
		t.Errorf("1: %s", b)
	}
	// 2. A patch whose line is past what is kept: no hunk without lines.
	ts := toolsOf(feedAll(fnCall(t, "p", "exec_command", map[string]any{"cmd": "apply_patch <<'P'\n*** Begin Patch\n*** Update File: a.txt\n@@\n-" + strings.Repeat("x", maxPatch+10) + "\n@@\n+y\n*** End Patch\nP"})))
	for _, h := range ts[0].Patch {
		if len(h.Lines) == 0 {
			t.Errorf("2: a hunk without lines: %+v", ts[0].Patch)
		}
	}
	// 3. A question answered by its result: done.
	ts = toolsOf(feedAll(fnCall(t, "q", "request_user_input", map[string]any{"questions": []any{map[string]any{"title": "?"}}}), fnOut(t, "q", `{"answers":{"0":"yes"}}`)))
	if !ts[0].Done {
		t.Error("3: answered, not done")
	}
	// 4. A script that only looks at a picture, told of on its own: one line.
	ts = toolsOf(feedAll(
		codexLine(t, "response_item", map[string]any{"type": "custom_tool_call", "name": "exec", "call_id": "s", "input": "await tools.view_image({path:\"/w/a.png\"});"}),
		fnDone(t, map[string]any{"type": "ImageView", "id": "exec-9", "path": "/w/a.png"}),
	))
	if len(ts) != 1 || ts[0].Detail != "/w/a.png" {
		t.Errorf("4: %+v", ts)
	}
	// 5. A plan with no list: the one there stands.
	c = feedAll(fnCall(t, "p1", "update_plan", map[string]any{"plan": []any{map[string]any{"step": "x", "status": "pending"}}}), fnOut(t, "p1", "Plan updated"), fnCall(t, "p2", "update_plan", map[string]any{"plan": "bad"}), fnOut(t, "p2", "Error parsing function call: plan"))
	if ts := c.Tasks(); len(ts) != 1 {
		t.Errorf("5: %+v", ts)
	}
}

// review454: a question put by the _async call waits for the person's
// next message whatever its result says (a script's, empty); one put by the
// other is answered by its result, whatever it says ("accepted" among it).
func TestCodexFnReview454(t *testing.T) {
	ts := toolsOf(feedAll(
		codexLine(t, "response_item", map[string]any{"type": "custom_tool_call", "name": "exec", "call_id": "s", "input": `await tools.request_user_input_async({questions:[{title:"继续吗？"}]});`}),
		codexLine(t, "response_item", map[string]any{"type": "custom_tool_call_output", "call_id": "s", "output": []any{map[string]any{"type": "input_text", "text": "Script completed\nWall time 0 seconds\nOutput:\n"}}}),
	))
	if len(ts) != 1 || ts[0].Name != "request_user_input" || ts[0].Done {
		t.Errorf("1: %+v", ts)
	}
	ts = toolsOf(feedAll(fnCall(t, "q", "request_user_input", map[string]any{"questions": []any{map[string]any{"title": "?"}}}), fnOut(t, "q", `{"answers":{"q":{"answers":["accepted"]}}}`)))
	if !ts[0].Done {
		t.Error("2: answered \"accepted\", not done")
	}
}

// review455: a question a script puts that waits stays open when a thing
// of the same id as the script is told of as done (on its line, or another
// line added to it).
func TestCodexFnReview455(t *testing.T) {
	ts := toolsOf(feedAll(
		codexLine(t, "response_item", map[string]any{"type": "custom_tool_call", "name": "exec", "call_id": "s", "input": `await tools.view_image({path:"/w/a.png"}); await tools.request_user_input_async({questions:[{title:"继续吗？"}]});`}),
		fnDone(t, map[string]any{"type": "ImageView", "id": "s", "path": "/w/a.png"}),
		codexLine(t, "response_item", map[string]any{"type": "custom_tool_call_output", "call_id": "s", "output": []any{map[string]any{"type": "input_text", "text": "Script completed\nWall time 0 seconds\nOutput:\n"}}}),
	))
	for _, x := range ts {
		if x.Name == "request_user_input" && x.Done {
			t.Errorf("answered by a picture looked at: %+v", ts)
		}
	}
}

// review456: a file a script changed, told of as done with the script's id,
// leaves the question it put that waits open.
func TestCodexFnReview456(t *testing.T) {
	ts := toolsOf(feedAll(
		codexLine(t, "response_item", map[string]any{"type": "custom_tool_call", "name": "exec", "call_id": "s", "input": "await tools.apply_patch(\"*** Begin Patch\\n*** Add File: /w/a.txt\\n+x\\n*** End Patch\"); await tools.request_user_input_async({questions:[{title:\"继续吗？\"}]});"}),
		fnDone(t, map[string]any{"type": "FileChange", "id": "s", "status": "completed", "changes": map[string]any{"/w/a.txt": map[string]any{"type": "add", "content": "x\n"}}}),
	))
	asked := false
	for _, x := range ts {
		if x.Name == "request_user_input" {
			asked = true
			if x.Done {
				t.Errorf("answered by a file changed: %+v", x)
			}
		}
	}
	if !asked {
		t.Errorf("no question: %+v", ts)
	}
}
