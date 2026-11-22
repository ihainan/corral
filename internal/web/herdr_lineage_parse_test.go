package web

import "testing"

func TestLineageExtractorClaude(t *testing.T) {
	ex := newLineageExtractor("claude", "/run/herdr/s3.sock", "3")

	raw := "" +
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"tu1","name":"Bash","input":{"command":"herdr workspace create --cwd /repo --label worker --json"}}]}}` + "\n" +
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"tu1","is_error":false,"content":"{\"ok\":true,\"result\":{\"type\":\"workspace_created\",\"root_pane\":{\"pane_id\":\"w34:p1\",\"terminal_id\":\"term-34\"}}}"}]}}` + "\n" +
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"tu2","name":"Bash","input":{"command":"herdr agent start worker-a --kind claude --pane w34:p1 --json"}}]}}` + "\n" +
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"tu2","is_error":false,"content":"{\"result\":{\"type\":\"agent_started\",\"agent\":{\"pane_id\":\"w34:p1\",\"terminal_id\":\"term-34\",\"agent_session\":{\"value\":\"sess-34\"}}}}"}]}}` + "\n"

	got := ex.Feed([]byte(raw))
	if len(got) != 2 {
		t.Fatalf("expected 2 discoveries (create + enrich), got %+v", got)
	}
	if got[0].ChildPane != "w34:p1" || got[0].ChildTermID != "term-34" || got[0].Action != "workspace_created" || got[0].EvidenceKey != "claude:tu1" {
		t.Fatalf("unexpected creation discovery: %+v", got[0])
	}
	if got[1].Action != "agent_started" || got[1].ChildPane != "w34:p1" || got[1].ChildSession != "sess-34" {
		t.Fatalf("unexpected enrichment discovery: %+v", got[1])
	}
}

func TestLineageExtractorAgentStartAloneDoesNotConfirm(t *testing.T) {
	ex := newLineageExtractor("claude", "/run/herdr/s3.sock", "3")
	raw := "" +
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"tu1","name":"Bash","input":{"command":"herdr agent start worker-a --kind claude --pane w99:p1 --json"}}]}}` + "\n" +
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"tu1","is_error":false,"content":"{\"result\":{\"type\":\"agent_started\",\"agent\":{\"pane_id\":\"w99:p1\",\"terminal_id\":\"term-99\",\"agent_session\":{\"value\":\"sess-99\"}}}}"}]}}` + "\n"

	if got := ex.Feed([]byte(raw)); len(got) != 0 {
		t.Fatalf("agent start alone must not confirm parent-child edge: %+v", got)
	}
}

func TestLineageExtractorCodexDeduplicatesCommandExecutionAndFunctionCall(t *testing.T) {
	ex := newLineageExtractor("codex", "/run/herdr/s3.sock", "3")
	raw := "" +
		`{"type":"response_item","payload":{"type":"function_call","name":"exec_command","call_id":"call_1","arguments":"{\"cmd\":\"cd /tmp && herdr pane split --direction right --json\"}"}}` + "\n" +
		`{"type":"event_msg","payload":{"type":"item_completed","item":{"type":"CommandExecution","id":"call_1","command":["/bin/bash","-lc","cd /tmp && herdr pane split --direction right --json"],"status":"completed","exit_code":0,"aggregated_output":"{\"type\":\"pane_split\",\"pane\":{\"pane_id\":\"w1:p2\",\"terminal_id\":\"term-2\"}}"}}} ` + "\n" +
		`{"type":"response_item","payload":{"type":"function_call_output","call_id":"call_1","output":"{\"type\":\"pane_split\",\"pane\":{\"pane_id\":\"w1:p2\",\"terminal_id\":\"term-2\"}}"}}` + "\n"

	got := ex.Feed([]byte(raw))
	if len(got) != 1 {
		t.Fatalf("expected exactly 1 deduplicated Codex discovery, got %+v", got)
	}
	if got[0].ChildPane != "w1:p2" || got[0].ChildTermID != "term-2" || got[0].EvidenceKey != "codex:call_1" {
		t.Fatalf("unexpected Codex discovery: %+v", got[0])
	}
}

func TestLineageExtractorAgyFIFO(t *testing.T) {
	ex := newLineageExtractor("agy", "/run/herdr/s3.sock", "3")
	raw := "" +
		`{"step_index":10,"type":"PLANNER_RESPONSE","tool_calls":[{"name":"read_file","args":{"Path":"/tmp/a"}},{"name":"run_command","args":{"CommandLine":"herdr tab create --workspace w1 --json"}}]} ` + "\n" +
		`{"step_index":11,"type":"GENERIC","status":"DONE","content":"file contents"}` + "\n" +
		`{"step_index":12,"type":"GENERIC","status":"DONE","content":"[12:00:01] {\"result\":{\"type\":\"tab_created\",\"root_pane\":{\"pane_id\":\"w1:p3\",\"terminal_id\":\"term-3\"}}}\nCommand exited with code 0"}` + "\n"

	got := ex.Feed([]byte(raw))
	if len(got) != 1 || got[0].ChildPane != "w1:p3" || got[0].ChildTermID != "term-3" || got[0].Action != "tab_created" {
		t.Fatalf("unexpected AGY discovery: %+v", got)
	}
}

func TestLineageExtractorRejectsUnsafeAndOutOfScopeCommands(t *testing.T) {
	ex := newLineageExtractor("claude", "/run/herdr/s3.sock", "3")

	rejected := []string{
		`herdr pane split --help`,
		`echo 'herdr pane split --json'`,
		`# herdr pane split --json`,
		`herdr pane split --remote prod --json`,
		`ssh box herdr pane split --json`,
		`docker exec c1 herdr pane split --json`,
		`HERDR_SOCKET_PATH=/run/herdr/other.sock herdr pane split --json`,
		`herdr --socket /run/herdr/other.sock pane split --json`,
		`herdr --session other pane split --json`,
		`false || herdr pane split --json`,
		`herdr pane split --json | sh`,
		"cat <<'EOF'\nherdr pane split --json\nEOF",
		`echo '{"result":{"type":"pane_info","pane":{"pane_id":"w1:p2"}}}'`,
	}
	for _, cmd := range rejected {
		if intent := ex.classifyHerdrCommand(cmd); intent.valid {
			t.Errorf("command should be rejected, got valid %+v: %q", intent, cmd)
		}
	}

	accepted := []string{
		`herdr pane split --direction right --json`,
		`cd /repo && HERDR_SOCKET_PATH=/run/herdr/s3.sock herdr workspace create --cwd /repo --json 2>&1`,
		`herdr --session 3 tab create --json`,
		`/home/user/.local/bin/herdr pane split --json`,
		// Real-world Claude/Codex idioms: output pipe truncation & variable capture scripts.
		`herdr workspace create --cwd /repo --label "Protocol Bridge" --no-focus 2>&1 | head -c 800`,
		`mkdir -p /tmp/x; W=$(herdr workspace create --label "Dev" --cwd /repo 2>&1); echo "$W" | head -c 400`,
	}
	for _, cmd := range accepted {
		if intent := ex.classifyHerdrCommand(cmd); !intent.valid {
			t.Errorf("command should be accepted: %q", cmd)
		}
	}

	// Failed tool result or mismatched result.type must be ignored.
	badResults := "" +
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"e1","name":"Bash","input":{"command":"herdr pane split --json"}}]}}` + "\n" +
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"e1","is_error":true,"content":"{\"type\":\"pane_split\",\"pane\":{\"pane_id\":\"w1:p2\"}}"}]}}` + "\n" +
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"e2","name":"Bash","input":{"command":"herdr pane split --json"}}]}}` + "\n" +
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"e2","is_error":false,"content":"{\"type\":\"workspace_created\",\"root_pane\":{\"pane_id\":\"w1:p2\"}}"}]}}` + "\n"
	if got := ex.Feed([]byte(badResults)); len(got) != 0 {
		t.Fatalf("failed/mismatched results should produce no discoveries, got %+v", got)
	}
}

func TestLineageExtractorRealHerdrV080PaneSplit(t *testing.T) {
	ex := newLineageExtractor("claude", "/run/herdr/s3.sock", "3")
	// Exact JSON output produced by real Herdr v0.8.0 `herdr pane split`: result.type is "pane_info".
	raw := "" +
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"tu_split","name":"Bash","input":{"command":"herdr pane split --pane w1:p1 --direction right"}}]}}` + "\n" +
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"tu_split","is_error":false,"content":"{\"id\":\"cli:pane:split\",\"result\":{\"pane\":{\"agent_status\":\"unknown\",\"cwd\":\"/tmp\",\"focused\":false,\"pane_id\":\"w1:p2\",\"tab_id\":\"w1:t1\",\"terminal_id\":\"term_660cbf6c862b42\",\"workspace_id\":\"w1\"},\"type\":\"pane_info\"}}"}]}}` + "\n"
	got := ex.Feed([]byte(raw))
	if len(got) != 1 || got[0].ChildPane != "w1:p2" || got[0].ChildTermID != "term_660cbf6c862b42" || got[0].Action != "pane_split" {
		t.Fatalf("real Herdr v0.8.0 pane.split result was missed: %+v", got)
	}
}

func TestLineageExtractorCodexCodeModeCreation(t *testing.T) {
	ex := newLineageExtractor("codex", "/run/herdr.sock", "default")
	raw := "" +
		`{"type":"response_item","payload":{"type":"custom_tool_call","call_id":"call_cm1","name":"exec","input":"const r = await tools.exec_command({cmd:\"herdr workspace create --cwd /repo\"}); text(r.output);"}}` + "\n" +
		`{"type":"response_item","payload":{"type":"custom_tool_call_output","call_id":"call_cm1","output":[{"type":"input_text","text":"Script completed\nWall time 0 seconds\nOutput:\n{\"result\":{\"type\":\"workspace_created\",\"root_pane\":{\"pane_id\":\"w2:p1\",\"terminal_id\":\"term-2\"}}}"}]}}` + "\n"
	got := ex.Feed([]byte(raw))
	if len(got) != 1 || got[0].ChildPane != "w2:p1" || got[0].ChildTermID != "term-2" {
		t.Fatalf("Codex code-mode workspace creation was missed: %+v", got)
	}

	// Wrapped text(r) output + two child spawns in a single code-mode script.
	rawMulti := "" +
		`{"type":"response_item","payload":{"type":"custom_tool_call","call_id":"call_cm2","name":"exec","input":"const a = await tools.exec_command({cmd:\"herdr pane split --direction right\"}); const b = await tools.exec_command({cmd:\"herdr pane split --direction down\"}); text(a); text(b);"}}` + "\n" +
		`{"type":"response_item","payload":{"type":"custom_tool_call_output","call_id":"call_cm2","output":[{"type":"input_text","text":"Script completed\nWall time 0 seconds\nOutput:\n{\"chunk_id\":\"c1\",\"exit_code\":0,\"output\":\"{\\\"result\\\":{\\\"type\\\":\\\"pane_info\\\",\\\"pane\\\":{\\\"pane_id\\\":\\\"w1:p2\\\",\\\"terminal_id\\\":\\\"term-2\\\"}}}\"}\n{\"chunk_id\":\"c2\",\"exit_code\":0,\"output\":\"{\\\"result\\\":{\\\"type\\\":\\\"pane_info\\\",\\\"pane\\\":{\\\"pane_id\\\":\\\"w1:p3\\\",\\\"terminal_id\\\":\\\"term-3\\\"}}}\"}"}]}}` + "\n"
	got2 := ex.Feed([]byte(rawMulti))
	if len(got2) != 2 || got2[0].ChildPane != "w1:p2" || got2[1].ChildPane != "w1:p3" || got2[0].EvidenceKey == got2[1].EvidenceKey {
		t.Fatalf("expected 2 distinct wrapped code-mode discoveries, got %+v", got2)
	}

	// Wrapped result with non-zero exit_code must be rejected.
	rawFail := "" +
		`{"type":"response_item","payload":{"type":"custom_tool_call","call_id":"call_cm3","name":"exec","input":"text(await tools.exec_command({cmd:\"herdr pane split --direction right\"}));"}}` + "\n" +
		`{"type":"response_item","payload":{"type":"custom_tool_call_output","call_id":"call_cm3","output":[{"type":"input_text","text":"Script completed\nOutput:\n{\"chunk_id\":\"c3\",\"exit_code\":1,\"output\":\"{\\\"result\\\":{\\\"type\\\":\\\"pane_info\\\",\\\"pane\\\":{\\\"pane_id\\\":\\\"w1:p9\\\",\\\"terminal_id\\\":\\\"term-9\\\"}}}\"}"}]}}` + "\n"
	if got3 := ex.Feed([]byte(rawFail)); len(got3) != 0 {
		t.Fatalf("non-zero exit_code wrapper must be rejected, got %+v", got3)
	}
}

func TestLineageExtractorRealWorldCompoundScripts(t *testing.T) {
	ex := newLineageExtractor("claude", "/run/herdr/s3.sock", "3")

	// 1. Real Claude idiom: `herdr workspace create ... 2>&1 | head -c 800`
	rawPipe := "" +
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"tu_pipe","name":"Bash","input":{"command":"herdr workspace create --cwd /repo --label \"Protocol Bridge\" --no-focus 2>&1 | head -c 800"}}]}}` + "\n" +
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"tu_pipe","is_error":false,"content":"{\"id\":\"cli:workspace:create\",\"result\":{\"root_pane\":{\"pane_id\":\"w34:p1\",\"terminal_id\":\"term-34\"},\"type\":\"workspace_created\"}}"}]}}` + "\n"
	gotPipe := ex.Feed([]byte(rawPipe))
	if len(gotPipe) != 1 || gotPipe[0].ChildPane != "w34:p1" || gotPipe[0].ChildTermID != "term-34" {
		t.Fatalf("pipe-truncated workspace create missed: %+v", gotPipe)
	}

	// 2. Real Claude idiom: multi-line script with VAR=$(herdr pane split ...) spawning 2 children
	rawMulti := "" +
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"tu_multi","name":"Bash","input":{"command":"P1=$(herdr pane split --current --direction right --no-focus 2>&1)\necho \"$P1\"\nP2=$(herdr pane split --current --direction down --no-focus 2>&1)\necho \"$P2\""}}]}}` + "\n" +
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"tu_multi","is_error":false,"content":"{\"id\":\"cli:pane:split\",\"result\":{\"pane\":{\"pane_id\":\"w1:p2\",\"terminal_id\":\"term-2\"},\"type\":\"pane_info\"}}\n{\"id\":\"cli:pane:split\",\"result\":{\"pane\":{\"pane_id\":\"w1:p3\",\"terminal_id\":\"term-3\"},\"type\":\"pane_info\"}}"}]}}` + "\n"
	gotMulti := ex.Feed([]byte(rawMulti))
	if len(gotMulti) != 2 || gotMulti[0].ChildPane != "w1:p2" || gotMulti[1].ChildPane != "w1:p3" {
		t.Fatalf("multi-line VAR=$(herdr pane split) missed: %+v", gotMulti)
	}

	// 3. Real Claude idiom (#26): capture pane split into $out, grep/cut pane_id, echo "pane $pid",
	// and filter `herdr agent start` through `grep -oE` so no full JSON object appears in output.
	rawFiltered := "" +
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"tu_filt","name":"Bash","input":{"command":"out=$(herdr pane split w33:p4 --direction down --cwd /repo 2>&1); pid=$(echo \"$out\" | grep -oE '\"pane_id\":\"[^\"]+\"' | head -1 | cut -d'\"' -f4); echo \"pane $pid\"; sleep 1; herdr agent start ag-hub --kind agy --pane \"$pid\" --timeout 60000 2>&1 | grep -oE '\"(type|agent_status|name)\":\"[^\"]+\"' | tr '\\n' ' '"}}]}}` + "\n" +
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"tu_filt","is_error":false,"content":"pane w33:p5\n\"agent_status\":\"idle\" \"name\":\"ag-hub\" \"type\":\"agent_started\""}]}}` + "\n"
	gotFiltered := ex.Feed([]byte(rawFiltered))
	if len(gotFiltered) != 1 || gotFiltered[0].ChildPane != "w33:p5" || gotFiltered[0].Action != "pane_split" {
		t.Fatalf("filtered-output pane split (#26) missed: %+v", gotFiltered)
	}

	// 4. Truncated JSON via `head -c 160` (incomplete closing braces, retains pane_id & terminal_id).
	rawTrunc := "" +
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"tu_trunc","name":"Bash","input":{"command":"herdr workspace create --cwd /repo 2>&1 | head -c 160"}}]}}` + "\n" +
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"tu_trunc","is_error":false,"content":"{\"id\":\"cli:workspace:create\",\"result\":{\"root_pane\":{\"pane_id\":\"w35:p1\",\"terminal_id\":\"term_trunc35\",\"cwd\":\"/repo\"}"}]}}` + "\n"
	gotTrunc := ex.Feed([]byte(rawTrunc))
	if len(gotTrunc) != 1 || gotTrunc[0].ChildPane != "w35:p1" || gotTrunc[0].ChildTermID != "term_trunc35" {
		t.Fatalf("truncated JSON workspace create missed: %+v", gotTrunc)
	}

	// 5. Safety: target pane echoed back, spoofed pane in command text, Herdr error,
	// list command in same script, or ambiguous multi-pane output must all be rejected.
	rawBad := "" +
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"tu_bad1","name":"Bash","input":{"command":"herdr pane split w33:p4; echo w99:p9"}}]}}` + "\n" +
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"tu_bad1","is_error":false,"content":"w33:p4\nw99:p9"}]}}` + "\n" +
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"tu_bad2","name":"Bash","input":{"command":"herdr pane split w33:p4 2>&1 | head -c 160"}}]}}` + "\n" +
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"tu_bad2","is_error":false,"content":"{\"error\":{\"code\":\"not_found\",\"message\":\"w33:p5 missing\"}}"}]}}` + "\n" +
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"tu_bad3","name":"Bash","input":{"command":"herdr pane split w33:p4; herdr agent list"}}]}}` + "\n" +
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"tu_bad3","is_error":false,"content":"pane w33:p6"}]}}` + "\n" +
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"tu_bad4","name":"Bash","input":{"command":"herdr pane split w33:p4"}}]}}` + "\n" +
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"tu_bad4","is_error":false,"content":"w33:p7 w33:p8"}]}}` + "\n"
	if gotBad := ex.Feed([]byte(rawBad)); len(gotBad) != 0 {
		t.Fatalf("unsafe filtered fallback cases must be rejected, got %+v", gotBad)
	}
}
