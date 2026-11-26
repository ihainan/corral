package web

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/ihainan/corral/internal/herdr"
	"github.com/ihainan/corral/internal/store"
)

// claudeCall is a Claude Code log's Bash call and its result, at the given
// times (unix ms), as rows of its JSONL.
func claudeCall(id, cmd string, at int64, result string, resultAt int64, isError bool) []byte {
	ts := func(ms int64) string { return time.UnixMilli(ms).UTC().Format(time.RFC3339Nano) }
	use, _ := json.Marshal(map[string]any{
		"type": "assistant", "timestamp": ts(at),
		"message": map[string]any{"content": []any{map[string]any{"type": "tool_use", "id": id, "name": "Bash", "input": map[string]any{"command": cmd}}}},
	})
	res, _ := json.Marshal(map[string]any{
		"type": "user", "timestamp": ts(resultAt),
		"message": map[string]any{"content": []any{map[string]any{"type": "tool_result", "tool_use_id": id, "is_error": isError, "content": result}}},
	})
	return append(append(use, '\n'), append(res, '\n')...)
}

// The shapes the parents of 11-25 made their panes in (ZGCAI-Blog, ZGCAI
// Analysis), none of which the strict reading proves: each is a window naming
// the panes made, and only those.
func TestLineageWindowsFromRealCommands(t *testing.T) {
	const t0 = int64(1795600000000)
	cases := []struct {
		name, cmd, out string
		isError        bool
		want           []string
	}{
		{
			name: "loop, its answer kept in a variable and reworked",
			cmd:  `for x in a b c; do out=$(herdr tab create --workspace $HERDR_WORKSPACE_ID --cwd ~/w/P1.4i-$x --label blog-p14i-$x --no-focus 2>&1); pane=$(echo "$out" | python3 -c 'import json,sys; print(json.load(sys.stdin)["result"]["root_pane"]["pane_id"])'); echo "$x pane=$pane"; done`,
			out:  "a pane=w2R:pA\nb pane=w2R:pB\nc pane=w2R:pC",
			want: []string{"w2R:pA", "w2R:pB", "w2R:pC"},
		},
		{
			name: "loop with its answer filtered by grep",
			cmd:  `for x in b d; do git worktree add -q -b wp/p1.4f-$x ~/w/P1.4f-$x 51b7e57 && herdr tab create --workspace $HERDR_WORKSPACE_ID --cwd $HOME/w/P1.4f-$x --label blog-p14f-$x --no-focus 2>&1 | grep -o '"pane_id":"[^"]*"'; done`,
			out:  `"pane_id":"w2R:p5"` + "\n" + `"pane_id":"w2R:p6"`,
			want: []string{"w2R:p5", "w2R:p6"},
		},
		{
			name: "beside a heredoc",
			cmd:  "python3 - <<'EOF'\nprint(1)\nEOF\nherdr tab create --workspace \"$HERDR_WORKSPACE_ID\" --cwd /w/P1.4g --label blog-p14g --no-focus",
			out:  `1` + "\n" + `{"id":"cli:tab:create","result":{"root_pane":{"pane_id":"w2R:p8","terminal_id":"term_8"}}}`,
			want: []string{"w2R:p8"},
		},
		{
			name: "a split, then an agent started in the pane split from",
			cmd:  `herdr pane split w2Y:p2 --direction right --cwd /w/sdk 2>&1 | grep -o '"pane_id":"[^"]*"' | head -1; herdr agent start mx-core --kind agy --pane w2Y:p2 -- --mode auto 2>&1 | tail -2`,
			out:  `"pane_id":"w2Y:p3"` + "\n" + `{"id":"cli:agent:start","result":{"agent":{"name":"mx-core","pane_id":"w2Y:p2"}}}`,
			want: []string{"w2Y:p3"},
		},
		{
			// (A pane another parent made meanwhile would be in the listing,
			// and born in the window: no window at all.)
			name: "made and listed",
			cmd:  `herdr tab create --workspace w2R --label x --no-focus | tail -1; herdr tab list --workspace w2R | grep -oE '"pane_id":"[^"]*"'`,
			out:  `{"result":{"root_pane":{"pane_id":"w2R:p4"}}}` + "\n" + `"pane_id":"w2R:p1"` + "\n" + `"pane_id":"w2R:p4"`,
		},
		{name: "made and looked up", cmd: `p=$(herdr tab create --label x | jq -r .result.root_pane.pane_id); herdr pane get $p`, out: `"pane_id":"w1:p9"`},
		{name: "a listing, the making in a comment", cmd: "sleep 1; herdr tab list # herdr tab create", out: `"pane_id":"w1:p9"`},
		{name: "the making in a comment only", cmd: "cat panes.txt # then herdr tab create", out: `w1:p9`},
		{name: "a # in a quoted label", cmd: `herdr tab create --label 'a #b' --no-focus | grep -o 'w1:p[0-9]*'`, out: `w1:p9`, want: []string{"w1:p9"}},
		{name: "help only", cmd: `herdr tab create --help; cat panes.txt`, out: `w1:p9`},
		{name: "help, short", cmd: `herdr pane split -h`, out: `w1:p9`},
		{name: "its own session named", cmd: `herdr --session default tab create --label x`, out: `"pane_id":"w1:p9"`, want: []string{"w1:p9"}},
		{name: "another herdr session", cmd: `herdr --session 2 tab create --label x`, out: `"pane_id":"w1:p9"`},
		{name: "another socket", cmd: `HERDR_SOCKET_PATH=/other.sock herdr tab create --label x`, out: `"pane_id":"w1:p9"`},
		{name: "another host", cmd: `herdr --remote box tab create --label x`, out: `"pane_id":"w1:p9"`},
		{name: "over ssh", cmd: `ssh box 'herdr tab create --label x'`, out: `"pane_id":"w1:p9"`},
		{name: "failed", cmd: `herdr tab create --label x`, out: `"pane_id":"w1:p9"`, isError: true},
		{name: "no pane made: a listing only", cmd: `herdr tab list --workspace w2R`, out: `"pane_id":"w2R:p1"`},
		{name: "the word in an echo only", cmd: `echo "use herdr pane split next"`, out: `w1:p3`, want: []string{"w1:p3"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ex := newLineageExtractor("claude", "/home/u/.config/herdr/herdr.sock", "default")
			ex.Feed(claudeCall("toolu_1", c.cmd, t0, c.out, t0+4000, c.isError))
			ws := ex.TakeWindows()
			var got []string
			for _, w := range ws {
				if w.From != t0 || w.To != t0+4000 || w.Key != "claude:toolu_1" {
					t.Fatalf("window: %+v", w)
				}
				got = append(got, w.Panes...)
			}
			if strings.Join(got, ",") != strings.Join(c.want, ",") {
				t.Fatalf("panes %v, want %v", got, c.want)
			}
		})
	}
}

func windowTracker(h *harness) *lineageTracker {
	return &lineageTracker{
		srv: h.srv, ownerID: 1, hostID: 10, server: "win",
		socket: "/home/u/.config/herdr/herdr.sock", herdrSession: "default",
		live: map[string]livePaneInfo{}, termToPane: map[string]string{},
		knownPanes: map[string]bool{}, paneLogs: map[string]knownPaneLog{},
		probedSize: map[string]int64{},
	}
}

func windowSnap(panes ...herdr.Pane) *herdr.Snapshot {
	return &herdr.Snapshot{Version: "0.8.0", Workspaces: []herdr.Workspace{{ID: "w1", Label: "main", Number: 1}}, Agents: panes}
}

// A pane its parent's call names counts only if it came into being while the
// call ran: not one there before (a listing names it too), not one born
// before or after, not one the person unlinked; on the host's clock, however
// far it is off the gateway's.
func TestLineageRecordWindow(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	lt := windowTracker(h)
	parent := herdr.Pane{ID: "w1:p1", WorkspaceID: "w1", Agent: "claude", TerminalID: "term-1", AgentSession: &herdr.AgentSessionInfo{Value: "ps"}}
	old := herdr.Pane{ID: "w1:p2", WorkspaceID: "w1", Agent: "agy", TerminalID: "term-2"}
	lt.OnSnapshot(ctx, windowSnap(parent, old))

	a := herdr.Pane{ID: "w1:p3", WorkspaceID: "w1", TerminalID: "term-3"}
	b := herdr.Pane{ID: "w1:p4", WorkspaceID: "w1", Agent: "agy", TerminalID: "term-4"}
	refused := herdr.Pane{ID: "w1:p5", WorkspaceID: "w1", Agent: "agy", TerminalID: "term-5"}
	if err := h.srv.store.RejectAgentLineage(ctx, 1, 10, "win", "w1:p5", "term-5", "", time.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	made := time.Now().UnixMilli()
	lt.OnSnapshot(ctx, windowSnap(parent, old, a, b, refused))

	// The host's clock 10 minutes ahead, as measured.
	const ahead = int64(10 * 60 * 1000)
	lt.noteClock(ahead)
	// Long before: none of them.
	lt.RecordWindow(ctx, "w1:p1", "term-1", "ps", lineageWindow{Panes: []string{"w1:p3"}, From: made + ahead - 120000, To: made + ahead - 60000, Key: "claude:early"})
	if lt.hasRowForLocked("w1:p3", "term-3") {
		t.Fatal("a pane born after the call ended was linked")
	}
	// The call that made them: the two new ones, not the old, not the refused.
	lt.RecordWindow(ctx, "w1:p1", "term-1", "ps", lineageWindow{Panes: []string{"w1:p2", "w1:p3", "w1:p4", "w1:p5", "w1:p1"}, From: made + ahead - 1500, To: made + ahead + 800, Key: "claude:make"})

	sum := summarize("default", windowSnap(parent, old, a, b, refused), &agentTimes{at: map[string]agentTime{}}, false)
	lt.decorate(&sum)
	parents := map[string]string{}
	for _, ag := range sum.Workspaces[0].Agents {
		parents[ag.PaneID] = ag.ParentPane
	}
	if parents["w1:p4"] != "w1:p1" {
		t.Fatalf("the pane the call made not linked: %v", parents)
	}
	if parents["w1:p2"] != "" || parents["w1:p5"] != "" || parents["w1:p1"] != "" {
		t.Fatalf("linked one the call did not make, or one refused: %v", parents)
	}
	rows, err := h.srv.store.ListAgentLineage(ctx, 1, 10, "win")
	if err != nil {
		t.Fatal(err)
	}
	keys := map[string]string{}
	for _, r := range rows {
		if r.State == store.LineageConfirmed {
			keys[r.ChildPane] = r.EvidenceKey
		}
	}
	// (Both panes of the one call: each its own evidence key.)
	if keys["w1:p3"] != "claude:make:w1:p3" || keys["w1:p4"] != "claude:make:w1:p4" || len(keys) != 2 {
		t.Fatalf("confirmed rows: %v", keys)
	}
}

// Not measured, the host's clock is given 30 s either way; past it, no link.
func TestLineageRecordWindowClockUnknown(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	lt := windowTracker(h)
	parent := herdr.Pane{ID: "w1:p1", WorkspaceID: "w1", Agent: "claude", TerminalID: "term-1"}
	lt.OnSnapshot(ctx, windowSnap(parent))
	a := herdr.Pane{ID: "w1:p2", WorkspaceID: "w1", Agent: "agy", TerminalID: "term-2"}
	b := herdr.Pane{ID: "w1:p3", WorkspaceID: "w1", Agent: "agy", TerminalID: "term-3"}
	made := time.Now().UnixMilli()
	lt.OnSnapshot(ctx, windowSnap(parent, a, b))
	lt.RecordWindow(ctx, "w1:p1", "term-1", "", lineageWindow{Panes: []string{"w1:p2"}, From: made - 20000, To: made - 15000, Key: "claude:x"})
	lt.RecordWindow(ctx, "w1:p1", "term-1", "", lineageWindow{Panes: []string{"w1:p3"}, From: made - 90000, To: made - 60000, Key: "claude:y"})
	if !lt.hasRowForLocked("w1:p2", "term-2") {
		t.Fatal("within 30 s of an unmeasured clock: not linked")
	}
	if lt.hasRowForLocked("w1:p3", "term-3") {
		t.Fatal("a minute off an unmeasured clock: linked")
	}
}

// The panes of the first snapshot have no known birth: never linked so.
func TestLineageRecordWindowFirstSnapshot(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	lt := windowTracker(h)
	now := time.Now().UnixMilli()
	lt.OnSnapshot(ctx, windowSnap(
		herdr.Pane{ID: "w1:p1", WorkspaceID: "w1", Agent: "claude", TerminalID: "term-1"},
		herdr.Pane{ID: "w1:p2", WorkspaceID: "w1", Agent: "agy", TerminalID: "term-2"},
	))
	lt.noteClock(0)
	lt.RecordWindow(ctx, "w1:p1", "term-1", "", lineageWindow{Panes: []string{"w1:p2"}, From: now - 1000, To: now + 1000, Key: "claude:z"})
	if lt.hasRowForLocked("w1:p2", "term-2") {
		t.Fatal("a pane there at the first snapshot was linked")
	}
}

// End to end, as the probe and the chat page feed it: the log of a call
// making two panes in a loop, its answer filtered.
func TestLineageWindowEndToEnd(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	lt := windowTracker(h)
	parent := herdr.Pane{ID: "w2R:p1", WorkspaceID: "w2R", Agent: "claude", TerminalID: "term-1"}
	lt.OnSnapshot(ctx, windowSnap(parent))
	began := time.Now().UnixMilli()
	five := herdr.Pane{ID: "w2R:p5", WorkspaceID: "w2R", Agent: "agy", TerminalID: "term-5"}
	six := herdr.Pane{ID: "w2R:p6", WorkspaceID: "w2R", Agent: "agy", TerminalID: "term-6"}
	lt.OnSnapshot(ctx, windowSnap(parent, five, six))
	lt.noteClock(0)
	ex := newLineageExtractor("claude", lt.socket, lt.herdrSession)
	strict := ex.Feed(claudeCall("toolu_9",
		`for x in b d; do herdr tab create --workspace $HERDR_WORKSPACE_ID --label blog-p14f-$x --no-focus 2>&1 | grep -o '"pane_id":"[^"]*"'; done`,
		began-500, `"pane_id":"w2R:p5"`+"\n"+`"pane_id":"w2R:p6"`, began+900, false))
	for _, d := range strict {
		lt.RecordToolLog(ctx, "w2R:p1", "term-1", "", d)
	}
	for _, w := range ex.TakeWindows() {
		lt.RecordWindow(ctx, "w2R:p1", "term-1", "", w)
	}
	if !lt.hasRowForLocked("w2R:p5", "term-5") || !lt.hasRowForLocked("w2R:p6", "term-6") {
		t.Fatalf("panes of a loop not linked: %+v", lt.rows)
	}
}

// herdr numbers panes past 9 in capitals (w2R:pA): both patterns that read a
// pane id from text take them — the lineage's, and the Antigravity title
// script's (11-26: blog-p14i-a..c, panes pA..pC, had neither parent nor title).
func TestPaneIDPatternsTakeLetters(t *testing.T) {
	line := agyTitlesScript[strings.Index(agyTitlesScript, "pane_re = re.compile(r'")+len("pane_re = re.compile(r'"):]
	scriptRe := regexp.MustCompile(line[:strings.Index(line, "')")])
	for _, id := range []string{"w1:p3", "w2R:pA", "w2R:pC", "w11:p10", "wD:p1"} {
		if !scriptRe.MatchString(id) {
			t.Errorf("title script: %s not a pane", id)
		}
		if m := herdrPaneTokenRe.FindStringSubmatch(`"pane_id":"` + id + `"`); m == nil || m[1] != id {
			t.Errorf("lineage: %s read as %v", id, m)
		}
	}
	for _, s := range []string{"workspace:pane", "w1:panes", "w2R:"} {
		if scriptRe.MatchString(s) || herdrPaneTokenRe.MatchString(s) {
			t.Errorf("%q taken for a pane", s)
		}
	}
}

// codexRow is a row of a Codex rollout log at ms (unix).
func codexRow(ms int64, payload map[string]any) []byte {
	b, _ := json.Marshal(map[string]any{"timestamp": time.UnixMilli(ms).UTC().Format(time.RFC3339Nano), "type": "response_item", "payload": payload})
	return append(b, '\n')
}

// A Codex call left running ("Process running with session ID N") goes on in
// write_stdin calls: its window stays open and takes their output, closing
// when the process ends — not on the first answer, which names no pane yet.
func TestLineageWindowCodexStillRunning(t *testing.T) {
	const t0 = int64(1795600000000)
	args, _ := json.Marshal(map[string]any{"cmd": `for x in a b; do herdr tab create --label m-$x --no-focus | grep -o '"pane_id":"[^"]*"'; sleep 20; done`})
	poll, _ := json.Marshal(map[string]any{"session_id": 63529, "chars": "", "yield_time_ms": 30000})
	other, _ := json.Marshal(map[string]any{"session_id": 777, "chars": ""})
	var log []byte
	log = append(log, codexRow(t0, map[string]any{"type": "function_call", "name": "exec_command", "call_id": "call_A", "arguments": string(args)})...)
	log = append(log, codexRow(t0+1000, map[string]any{"type": "function_call_output", "call_id": "call_A", "output": "Chunk ID: a1\nWall time: 1.0 seconds\nProcess running with session ID 63529\nOutput:\n\"pane_id\":\"w1:p5\"\n"})...)
	log = append(log, codexRow(t0+2000, map[string]any{"type": "function_call", "name": "write_stdin", "call_id": "call_B", "arguments": string(other)})...)
	log = append(log, codexRow(t0+2100, map[string]any{"type": "function_call_output", "call_id": "call_B", "output": "Process exited with code 0\nOutput:\nw1:p8\n"})...)
	log = append(log, codexRow(t0+30000, map[string]any{"type": "function_call", "name": "write_stdin", "call_id": "call_C", "arguments": string(poll)})...)
	log = append(log, codexRow(t0+31000, map[string]any{"type": "function_call_output", "call_id": "call_C", "output": "Chunk ID: a2\nProcess running with session ID 63529\nOutput:\n"})...)
	ex := newLineageExtractor("codex", "/s.sock", "default")
	ex.Feed(log)
	if ws := ex.TakeWindows(); len(ws) != 0 {
		t.Fatalf("window closed while the process runs: %+v", ws)
	}
	var end []byte
	end = append(end, codexRow(t0+40000, map[string]any{"type": "function_call", "name": "write_stdin", "call_id": "call_D", "arguments": string(poll)})...)
	end = append(end, codexRow(t0+42000, map[string]any{"type": "function_call_output", "call_id": "call_D", "output": "Chunk ID: a3\nProcess exited with code 0\nOutput:\n\"pane_id\":\"w1:p6\"\n"})...)
	ex.Feed(end)
	ws := ex.TakeWindows()
	if len(ws) != 1 || ws[0].From != t0 || ws[0].To != t0+42000 || ws[0].Key != "codex:call_A" || strings.Join(ws[0].Panes, ",") != "w1:p5,w1:p6" {
		t.Fatalf("windows: %+v", ws)
	}

	// Failed in the end: no window.
	ex2 := newLineageExtractor("codex", "/s.sock", "default")
	ex2.Feed(log)
	ex2.Feed([]byte(strings.ReplaceAll(string(end), "code 0", "code 1")))
	if ws := ex2.TakeWindows(); len(ws) != 0 {
		t.Fatalf("a failed run made a window: %+v", ws)
	}
}

// A pane herdr first tells without its terminal, then with it (an agent seen
// in it): the same pane, not come into being — a listing naming it in any
// window later does not link it.
func TestLineageBornTerminalLearnedLater(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	lt := windowTracker(h)
	parent := herdr.Pane{ID: "w1:p1", WorkspaceID: "w1", Agent: "claude", TerminalID: "term-1"}
	shell := herdr.Pane{ID: "w1:p3", WorkspaceID: "w1"}
	lt.OnSnapshot(ctx, windowSnap(parent, shell))
	lt.OnSnapshot(ctx, windowSnap(parent, herdr.Pane{ID: "w1:p3", WorkspaceID: "w1", Agent: "agy", TerminalID: "term-3"}))
	lt.noteClock(0)
	now := time.Now().UnixMilli()
	lt.RecordWindow(ctx, "w1:p1", "term-1", "", lineageWindow{Panes: []string{"w1:p3"}, From: now - 1000, To: now + 1000, Key: "claude:k"})
	if lt.hasRowForLocked("w1:p3", "term-3") {
		t.Fatal("a pane there all along was linked once its terminal was told")
	}
	// Born while watched, its terminal told later: its birth kept.
	lt.OnSnapshot(ctx, windowSnap(parent, herdr.Pane{ID: "w1:p3", WorkspaceID: "w1", Agent: "agy", TerminalID: "term-3"}, herdr.Pane{ID: "w1:p4", WorkspaceID: "w1"}))
	at, ok := lt.born["pane:w1:p4"]
	lt.OnSnapshot(ctx, windowSnap(parent, herdr.Pane{ID: "w1:p3", WorkspaceID: "w1", Agent: "agy", TerminalID: "term-3"}, herdr.Pane{ID: "w1:p4", WorkspaceID: "w1", Agent: "agy", TerminalID: "term-4"}))
	if !ok || lt.born["term-4"] != at {
		t.Fatalf("birth not carried over: %v %v", at, lt.born)
	}
}

// The host's clock measured (or found moved) after the logs were read: the
// sizes that would skip them are forgotten, so they are read again; a
// measure within the slack forgets nothing.
func TestLineageClockMeasuredRereads(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	lt := windowTracker(h)
	parent := herdr.Pane{ID: "w1:p1", WorkspaceID: "w1", Agent: "claude", TerminalID: "term-1"}
	lt.OnSnapshot(ctx, windowSnap(parent))
	lt.OnSnapshot(ctx, windowSnap(parent, herdr.Pane{ID: "w1:p2", WorkspaceID: "w1", Agent: "agy", TerminalID: "term-2"}))
	lt.probedSize["/log/a.jsonl"] = 100
	lt.noteClock(10 * 60 * 1000)
	if len(lt.probedSize) != 0 {
		t.Fatal("first measure: logs not to be read again")
	}
	lt.probedSize["/log/a.jsonl"] = 100
	lt.noteClock(10*60*1000 + 500)
	if len(lt.probedSize) != 1 {
		t.Fatal("a measure within the slack: logs read again")
	}
	lt.noteClock(0)
	if len(lt.probedSize) != 0 {
		t.Fatal("clock moved: logs not to be read again")
	}
}
