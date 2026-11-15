package convo

import (
	"strings"
	"testing"
)

// Rows as a real Codex 0.167.2 wrote them in code mode (cxaudit.py #bg,
// 11-15): a script whose exec_command yielded, the process left running
// past its turn; its end told of in a later turn, by its process.
const (
	bgItemBefore = `{"timestamp":"2026-11-15T04:12:28.006Z","type":"event_msg","payload":{"type":"item_completed","turn_id":"t1","item":{"type":"CommandExecution","id":"exec-1","process_id":"100","command":["/bin/bash","-lc","ls -la"],"parsed_cmd":[{"type":"list_files","cmd":"ls -la"}],"source":"unified_exec_startup","status":"completed","aggregated_output":"x\n","exit_code":0}}}` + "\n"
	bgUser       = `{"timestamp":"2026-11-15T04:14:11.347Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"start background #bg"}]}}` + "\n"
	bgCall       = `{"timestamp":"2026-11-15T04:14:11.378Z","type":"response_item","payload":{"type":"custom_tool_call","status":"completed","call_id":"call_7_0","name":"exec","input":"text(await tools.exec_command({cmd:\"sleep 45; echo bg-done\",yield_time_ms:500}))"}}` + "\n"
	bgOut        = `{"timestamp":"2026-11-15T04:14:12.050Z","type":"response_item","payload":{"type":"custom_tool_call_output","call_id":"call_7_0","output":[{"type":"input_text","text":"Script completed\nWall time 0.7 seconds\nOutput:\n"},{"type":"input_text","text":"{\"chunk_id\":\"659348\",\"wall_time_seconds\":0.501150577,\"session_id\":37552,\"original_token_count\":0,\"output\":\"\"}"}]}}` + "\n"
	bgSaid       = `{"timestamp":"2026-11-15T04:14:12.076Z","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Started it in the background."}]}}` + "\n"
	bgDone       = `{"timestamp":"2026-11-15T04:14:12.087Z","type":"event_msg","payload":{"type":"task_complete","turn_id":"t2"}}` + "\n"
	bgUser2      = `{"timestamp":"2026-11-15T04:14:49.531Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"nap #sleep"}]}}` + "\n"
	bgEnded      = `{"timestamp":"2026-11-15T04:14:56.402Z","type":"event_msg","payload":{"type":"item_completed","turn_id":"t3","item":{"type":"CommandExecution","id":"exec-9add","process_id":"37552","command":["/bin/bash","-lc","sleep 45; echo bg-done"],"parsed_cmd":[{"type":"unknown","cmd":"sleep 45; echo bg-done"}],"source":"unified_exec_startup","status":"completed","aggregated_output":"bg-done\n","exit_code":0},"started_at_ms":1794716051547,"completed_at_ms":1794716096402}}` + "\n"
	bgQuickOut   = `{"timestamp":"2026-11-15T04:14:12.050Z","type":"response_item","payload":{"type":"custom_tool_call_output","call_id":"call_7_0","output":[{"type":"input_text","text":"Script completed\nWall time 0.1 seconds\nOutput:\n"},{"type":"input_text","text":"{\"chunk_id\":\"deb7ba\",\"wall_time_seconds\":0.02,\"exit_code\":0,\"session_id\":37552,\"original_token_count\":5,\"output\":\"done\\n\"}"}]}}` + "\n"
)

// entryOf: the entry whose parts hold the tool of id, and its index.
func entryOf(c *Codex, id string) (int, *Tool) {
	for i, e := range c.entries {
		for _, p := range e.Parts {
			if p.Tool != nil && p.Tool.ID == id {
				return i, p.Tool
			}
		}
	}
	return -1, nil
}

func TestCodexScriptLeftRunning(t *testing.T) {
	for _, before := range []string{bgItemBefore, ""} { // (commands told of on their own already; and not yet)
		c := feedAll(before, bgUser, bgCall, bgOut, bgSaid, bgDone)
		if got := runningIDs(c); got != "cmd:proc:37552:sleep 45; echo bg-done" {
			t.Fatalf("before %t: still running: %q", before != "", got)
		}
		at, tl := entryOf(c, "proc:37552")
		if before == "" {
			at, tl = entryOf(c, "call_7_0")
		}
		if tl == nil || tl.Done || tl.Detail != "sleep 45; echo bg-done" {
			t.Fatalf("before %t: its step, running: %+v", before != "", tl)
		}
		// (The turn after it, and its end: in its own step, in its own turn.)
		c.Feed([]byte(bgUser2 + bgEnded))
		if got := runningIDs(c); got != "" {
			t.Errorf("before %t: told of as ended: %q", before != "", got)
		}
		if !tl.Done || !strings.Contains(tl.Result, "bg-done") {
			t.Errorf("before %t: its step ended: %+v", before != "", tl)
		}
		n := 0
		for i, e := range c.entries {
			for _, p := range e.Parts {
				if p.Tool != nil && strings.Contains(p.Tool.Detail, "sleep 45") {
					n++
					if i != at {
						t.Errorf("before %t: its end told of in another entry (%d, not %d)", before != "", i, at)
					}
				}
			}
		}
		if n != 1 {
			t.Errorf("before %t: told of %d times", before != "", n)
		}
	}
	// (Ended within its call: not running.)
	c := feedAll(bgItemBefore, bgUser, bgCall, bgQuickOut, bgSaid, bgDone)
	if got := runningIDs(c); got != "" {
		t.Errorf("ended within its call: %q", got)
	}
}

func TestCodeModeGoing(t *testing.T) {
	for raw, want := range map[string]string{
		`"{\"chunk_id\":\"a\",\"session_id\":12,\"output\":\"\"}"`:                          "12",
		`"{\"chunk_id\":\"a\",\"session_id\":\"13\",\"exit_code\":null}"`:                   "13",
		`"{\"chunk_id\":\"a\",\"session_id\":12,\"exit_code\":0}"`:                          "",
		`"x {\"chunk_id\":\"a\",\"session_id\":1} y {\"chunk_id\":\"b\",\"session_id\":2}"`: "1,2",
		`"{\"chunk_id\":\"a\",\"session_id\":\"x1\"}"`:                                      "",
		`"{\"chunk_id\": broken"`:                                                           "",
	} {
		if got := strings.Join(codeModeGoing([]byte(raw)), ","); got != want {
			t.Errorf("%s: %q, want %q", raw, got, want)
		}
	}
}

// clock.sleep: a wait, as long as it said.
func TestCodexSleep(t *testing.T) {
	for ms, want := range map[float64]string{12000: "12s", 400: "1s", 90000: "1m 30s", 120000: "2m", 3_900_000: "1h 5m", 7_200_000: "2h", 0: ""} {
		if got := sleepFor(ms); got != want {
			t.Errorf("%v: %q, want %q", ms, got, want)
		}
	}
	ts, _ := codexFn("clock", "sleep", `{"duration_ms":30000}`)
	if len(ts) != 1 || ts[0].Name != "Sleep" || ts[0].Detail != "30s" {
		t.Errorf("%+v", ts)
	}
}

// Commands told of on their own (once done): a script's commands are shown
// while its call is out, running, then that line goes for what tells of
// them — not shown twice, not lost while it runs.
func TestCodexScriptShownWhileOut(t *testing.T) {
	long := `{"timestamp":"2026-11-15T04:13:45.100Z","type":"response_item","payload":{"type":"custom_tool_call","status":"completed","call_id":"call_9","name":"exec","input":"text(await tools.exec_command({cmd:\"for i in 1 2 3; do sleep 2; done\",yield_time_ms:30000}))"}}` + "\n"
	longItem := `{"timestamp":"2026-11-15T04:14:05.000Z","type":"event_msg","payload":{"type":"item_completed","item":{"type":"CommandExecution","id":"exec-9","process_id":"200","command":["/bin/bash","-lc","for i in 1 2 3; do sleep 2; done"],"parsed_cmd":[{"type":"unknown","cmd":"for i in 1 2 3; do sleep 2; done"}],"status":"completed","aggregated_output":"","exit_code":0}}}` + "\n"
	longOut := `{"timestamp":"2026-11-15T04:14:05.020Z","type":"response_item","payload":{"type":"custom_tool_call_output","call_id":"call_9","output":[{"type":"input_text","text":"Script completed\nWall time 20 seconds\nOutput:\n"},{"type":"input_text","text":"{\"chunk_id\":\"a\",\"exit_code\":0,\"session_id\":200,\"output\":\"\"}"}]}}` + "\n"
	c := feedAll(bgItemBefore, bgUser, long)
	n, running := 0, 0
	for _, tl := range toolsOf(c) {
		if strings.Contains(tl.Detail, "for i in") {
			n++
			if !tl.Done {
				running++
			}
		}
	}
	if n != 1 || running != 1 {
		t.Fatalf("while out: %d lines, %d running", n, running)
	}
	for _, rows := range [][]string{{longItem, longOut}, {longOut, longItem}, {longItem}} {
		c := feedAll(append([]string{bgItemBefore, bgUser, long}, rows...)...)
		n, done := 0, 0
		for _, tl := range toolsOf(c) {
			if strings.Contains(tl.Detail, "for i in") {
				n++
				if tl.Done {
					done++
				}
			}
		}
		if n != 1 || done != 1 {
			t.Errorf("%d rows after: %d lines, %d done", len(rows), n, done)
		}
	}
	// (The turn over with its call never answered: not left running.)
	c = feedAll(bgItemBefore, bgUser, long, bgDone)
	for _, tl := range toolsOf(c) {
		if strings.Contains(tl.Detail, "for i in") && !tl.Done {
			t.Errorf("left running past its turn: %+v", tl)
		}
	}
}
