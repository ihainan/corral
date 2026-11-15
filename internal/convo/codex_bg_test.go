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
	// (Commands told of on their own already: a step of its own, in its turn.)
	c := feedAll(bgItemBefore, bgUser, bgCall, bgOut, bgSaid, bgDone)
	if got := runningIDs(c); got != "cmd:proc:37552@2026-11-15T04:14:12.050Z:sleep 45; echo bg-done" {
		t.Fatalf("still running: %q", got)
	}
	at, tl := entryOf(c, "proc:37552@2026-11-15T04:14:12.050Z")
	if tl == nil || tl.Done || tl.Detail != "sleep 45; echo bg-done" {
		t.Fatalf("its step, running: %+v", tl)
	}
	c.Feed([]byte(bgUser2 + bgEnded))
	if got := runningIDs(c); got != "" {
		t.Errorf("told of as ended: %q", got)
	}
	if !tl.Done || !strings.Contains(tl.Result, "bg-done") {
		t.Errorf("its step ended: %+v", tl)
	}
	n := 0
	for i, e := range c.entries {
		for _, p := range e.Parts {
			if p.Tool != nil && strings.Contains(p.Tool.Detail, "sleep 45") {
				n++
				if i != at {
					t.Errorf("its end told of in another entry (%d, not %d)", i, at)
				}
			}
		}
	}
	if n != 1 {
		t.Errorf("told of %d times", n)
	}
	// (None told of on its own yet: the script's line stands for it, the
	// process in the running list till its end, which is told as any.)
	c = feedAll(bgUser, bgCall, bgOut, bgSaid, bgDone)
	if got := runningIDs(c); got != "cmd:proc:37552@2026-11-15T04:14:12.050Z:sleep 45; echo bg-done" {
		t.Fatalf("no items: still running: %q", got)
	}
	c.Feed([]byte(bgUser2 + bgEnded))
	if got := runningIDs(c); got != "" {
		t.Errorf("no items: told of as ended: %q", got)
	}
	n = 0
	for _, tl := range toolsOf(c) {
		if strings.Contains(tl.Detail, "sleep 45") {
			n++
		}
	}
	if n != 1 {
		t.Errorf("no items: told of %d times", n)
	}
	// (Ended within its call: not running.)
	c = feedAll(bgItemBefore, bgUser, bgCall, bgQuickOut, bgSaid, bgDone)
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

// Review cxparity1: an end told before the script's output; a start in the
// part before, its end in this one (either read first); a script of two
// commands, each its own process.
func TestCodexProcsReview(t *testing.T) {
	steps := func(c *Codex, word string) (n, done int) {
		for _, tl := range toolsOf(c) {
			if strings.Contains(tl.Detail, word) {
				n++
				if tl.Done {
					done++
				}
			}
		}
		return
	}
	// 1. Its end before what the script gave back: not set going again.
	c := feedAll(bgItemBefore, bgUser, bgCall, bgEnded, bgOut)
	if got := runningIDs(c); got != "" {
		t.Errorf("1: ended first, still running: %q", got)
	}
	if n, done := steps(c, "sleep 45"); n != 1 || done != 1 {
		t.Errorf("1: %d steps, %d done", n, done)
	}
	// 2. Its start in the part before.
	older := feedAll(bgItemBefore, bgUser, bgCall, bgOut, bgSaid, bgDone)
	for _, order := range []string{"prepend first", "end first"} {
		var c *Codex
		if order == "prepend first" {
			c = feedAll(bgUser2)
			c.Prepend(feedAll(bgItemBefore, bgUser, bgCall, bgOut, bgSaid, bgDone))
			c.Feed([]byte(bgEnded))
		} else {
			c = feedAll(bgUser2, bgEnded)
			c.Prepend(feedAll(bgItemBefore, bgUser, bgCall, bgOut, bgSaid, bgDone))
		}
		if got := runningIDs(c); got != "" {
			t.Errorf("2 %s: still running: %q", order, got)
		}
		at, tl := entryOf(c, "proc:37552@2026-11-15T04:14:12.050Z")
		if tl == nil || !tl.Done || !strings.Contains(tl.Result, "bg-done") {
			t.Errorf("2 %s: its step: %+v", order, tl)
		}
		if n, _ := steps(c, "sleep 45"); n != 1 {
			t.Errorf("2 %s: told of %d times", order, n)
		}
		_ = at
	}
	_ = older
	// 3. Two commands, two processes: each its own, by its end.
	two := `{"timestamp":"2026-11-15T04:14:11.378Z","type":"response_item","payload":{"type":"custom_tool_call","status":"completed","call_id":"call_8","name":"exec","input":"text(await tools.exec_command({cmd:\"sleep 40\",yield_time_ms:500}));\ntext(await tools.exec_command({cmd:\"sleep 50\",yield_time_ms:500}))"}}` + "\n"
	twoOut := `{"timestamp":"2026-11-15T04:14:12.050Z","type":"response_item","payload":{"type":"custom_tool_call_output","call_id":"call_8","output":[{"type":"input_text","text":"{\"chunk_id\":\"a\",\"session_id\":41,\"output\":\"\"}{\"chunk_id\":\"b\",\"session_id\":42,\"output\":\"\"}"}]}}` + "\n"
	c = feedAll(bgItemBefore, bgUser, two, twoOut)
	var ids, whats []string
	for _, r := range c.Running() {
		ids, whats = append(ids, r.ID), append(whats, r.What)
	}
	if len(ids) != 2 || ids[0] == ids[1] || whats[0] != "" || whats[1] != "" {
		t.Errorf("3: %q %q (not named by a guess)", ids, whats)
	}
}

// Review cxparity2: a poll of it told running in this part, then its end,
// its start in the part before; its output in this part, its call in the
// part before.
func TestCodexProcsAcrossParts(t *testing.T) {
	poll := `{"timestamp":"2026-11-15T04:14:30.000Z","type":"response_item","payload":{"type":"custom_tool_call","status":"completed","call_id":"call_p","name":"exec","input":"text(await tools.write_stdin({session_id:37552, chars:\"\"}))"}}` + "\n"
	pollOut := `{"timestamp":"2026-11-15T04:14:31.000Z","type":"response_item","payload":{"type":"custom_tool_call_output","call_id":"call_p","output":[{"type":"input_text","text":"{\"chunk_id\":\"c\",\"session_id\":37552,\"output\":\"\"}"}]}}` + "\n"
	count := func(c *Codex) (n, done int) {
		for _, tl := range toolsOf(c) {
			if strings.Contains(tl.Detail, "sleep 45") {
				n++
				if tl.Done {
					done++
				}
			}
		}
		return
	}
	c := feedAll(bgItemBefore, bgUser2, poll, pollOut, bgEnded)
	c.Prepend(feedAll(bgItemBefore, bgUser, bgCall, bgOut, bgSaid, bgDone))
	if got := runningIDs(c); got != "" {
		t.Errorf("poll then end, start before: running %q", got)
	}
	if n, done := count(c); n != 1 || done != 1 {
		t.Errorf("poll then end, start before: %d steps, %d done", n, done)
	}
	// (Its output first in this part: its call read with the part before.)
	c = feedAll(bgOut, bgSaid, bgDone)
	c.Prepend(feedAll(bgItemBefore, bgUser, bgCall))
	if got := runningIDs(c); got != "cmd:proc:37552@2026-11-15T04:14:12.050Z:sleep 45; echo bg-done" {
		t.Errorf("output before its call was read: running %q", got)
	}
}

// Review cxparity3.
func TestCodexProcsReview3(t *testing.T) {
	count := func(c *Codex) (n, done int) {
		for _, tl := range toolsOf(c) {
			if strings.Contains(tl.Detail, "sleep 45") {
				n++
				if tl.Done {
					done++
				}
			}
		}
		return
	}
	// 1. Started before commands were told of on their own, then one is:
	// running still (its script's line gone with the rest), its end shown
	// once, as any command's.
	c := feedAll(bgUser, bgCall, bgOut, bgSaid, bgDone, bgUser2, bgItemBefore)
	if got := runningIDs(c); got != "cmd:proc:37552@2026-11-15T04:14:12.050Z:sleep 45; echo bg-done" {
		t.Errorf("1: running %q", got)
	}
	c.Feed([]byte(bgEnded))
	if n, done := count(c); n != 1 || done != 1 || runningIDs(c) != "" {
		t.Errorf("1, ended: %d steps, %d done, running %q", n, done, runningIDs(c))
	}
	// 2. Its call hidden in the part before, its output in this one.
	c = feedAll(bgOut, bgSaid, bgDone)
	c.Prepend(feedAll(bgItemBefore, bgUser, bgCall))
	if got := runningIDs(c); got != "cmd:proc:37552@2026-11-15T04:14:12.050Z:sleep 45; echo bg-done" {
		t.Errorf("2: running %q", got)
	}
	if n, done := count(c); n != 1 || done != 0 {
		t.Errorf("2: %d steps, %d done", n, done)
	}
	// 3. Its end told running then done in this part; its start before.
	inProg := strings.Replace(strings.Replace(bgEnded, `"status":"completed"`, `"status":"in_progress"`, 1), `"exit_code":0`, `"exit_code":null`, 1)
	c = feedAll(bgUser2, inProg, bgEnded)
	c.Prepend(feedAll(bgItemBefore, bgUser, bgCall, bgOut, bgSaid, bgDone))
	if n, done := count(c); n != 1 || done != 1 || runningIDs(c) != "" {
		t.Errorf("3: %d steps, %d done, running %q", n, done, runningIDs(c))
	}
}

// Real use (11-15 replay): a process looked at again by a script that
// yields, what it gave back coming with a wait on its cell; ended, it
// comes back with no session — no CommandExecution of it.
func TestCodexProcPolledEnded(t *testing.T) {
	poll := `{"timestamp":"2026-11-15T04:15:00.000Z","type":"response_item","payload":{"type":"custom_tool_call","status":"completed","call_id":"call_q","name":"exec","input":"// @exec: {\"yield_time_ms\": 1000}\ntext(await tools.write_stdin({session_id:37552,chars:\"\",\"yield_time_ms\":40000}));"}}` + "\n"
	pollOut := `{"timestamp":"2026-11-15T04:15:01.000Z","type":"response_item","payload":{"type":"custom_tool_call_output","call_id":"call_q","output":[{"type":"input_text","text":"Script running with cell ID 7\nWall time 1.0 seconds\nOutput:\n"}]}}` + "\n"
	wait := `{"timestamp":"2026-11-15T04:15:02.000Z","type":"response_item","payload":{"type":"function_call","name":"wait","call_id":"call_w","arguments":"{\"cell_id\":\"7\",\"yield_time_ms\":40000}"}}` + "\n"
	waitRunning := `{"timestamp":"2026-11-15T04:15:42.000Z","type":"response_item","payload":{"type":"function_call_output","call_id":"call_w","output":[{"type":"input_text","text":"Script completed\nWall time 40 seconds\nOutput:\n"},{"type":"input_text","text":"{\"chunk_id\":\"a\",\"wall_time_seconds\":40.0,\"session_id\":37552,\"output\":\"\"}"}]}}` + "\n"
	waitEnded := strings.Replace(waitRunning, `\"session_id\":37552,`, `\"exit_code\":0,`, 1)
	c := feedAll(bgItemBefore, bgUser, bgCall, bgOut, bgSaid, bgDone, bgUser2, poll, pollOut, wait, waitRunning)
	if got := runningIDs(c); got != "cmd:proc:37552@2026-11-15T04:14:12.050Z:sleep 45; echo bg-done" {
		t.Fatalf("still running when the poll says so: %q", got)
	}
	t.Logf("ended row: %s", waitEnded)
	c = feedAll(bgItemBefore, bgUser, bgCall, bgOut, bgSaid, bgDone, bgUser2, poll, pollOut, wait, waitEnded)
	if got := runningIDs(c); got != "" {
		t.Fatalf("ended, its poll saying so: %q", got)
	}
	if _, tl := entryOf(c, "proc:37552@2026-11-15T04:14:12.050Z"); tl == nil || !tl.Done {
		t.Errorf("its step ended: %+v", tl)
	}
}

// Review cxreal1: a poll whose result is not printed, another command's
// is; a poll of several: not taken for the end of what was polled.
func TestCodexProcPolledNotEnded(t *testing.T) {
	poll := `{"timestamp":"2026-11-15T04:15:00.000Z","type":"response_item","payload":{"type":"custom_tool_call","status":"completed","call_id":"call_q","name":"exec","input":"await tools.write_stdin({session_id:37552,chars:\"\"}); text(await tools.exec_command({cmd:\"ls\"}));"}}` + "\n"
	pollOut := `{"timestamp":"2026-11-15T04:15:01.000Z","type":"response_item","payload":{"type":"custom_tool_call_output","call_id":"call_q","output":[{"type":"input_text","text":"Script completed\nOutput:\n"},{"type":"input_text","text":"{\"chunk_id\":\"b\",\"exit_code\":0,\"output\":\"x\"}"}]}}` + "\n"
	c := feedAll(bgItemBefore, bgUser, bgCall, bgOut, bgSaid, bgDone, bgUser2, poll, pollOut)
	if got := runningIDs(c); got != "cmd:proc:37552@2026-11-15T04:14:12.050Z:sleep 45; echo bg-done" {
		t.Errorf("another command's result taken for its end: %q", got)
	}
	two := strings.Replace(poll, `await tools.write_stdin({session_id:37552,chars:\"\"}); text(await tools.exec_command({cmd:\"ls\"}));`, `text(await tools.write_stdin({session_id:37552,chars:\"\"})); text(await tools.write_stdin({session_id:99,chars:\"\"}));`, 1)
	c = feedAll(bgItemBefore, bgUser, bgCall, bgOut, bgSaid, bgDone, bgUser2, two, pollOut)
	if got := runningIDs(c); got == "" {
		t.Errorf("a poll of two: one's exit taken for its end")
	}
}

// The conversation's first command one left running (nothing told of on its
// own yet: the script's line stands for it): its end, told on its own,
// makes it a step in its turn, where that line was — not a step of the
// turn going on when it ended (A7, 11-15).
func TestCodexFirstCommandLeftRunning(t *testing.T) {
	c := feedAll(bgUser, bgCall, bgOut, bgSaid, bgDone)
	if got := runningIDs(c); got != "cmd:proc:37552@2026-11-15T04:14:12.050Z:sleep 45; echo bg-done" {
		t.Fatalf("still running: %q", got)
	}
	callAt := -1
	for i, e := range c.entries {
		for _, p := range e.Parts {
			if p.Tool != nil && p.Tool.ID == "call_7_0" {
				callAt = i
			}
		}
	}
	if callAt < 0 {
		t.Fatal("the script's line not shown")
	}
	c.Feed([]byte(bgUser2 + bgEnded))
	if got := runningIDs(c); got != "" {
		t.Errorf("told of as ended: %q", got)
	}
	at, tl := entryOf(c, "proc:37552@2026-11-15T04:14:12.050Z")
	if tl == nil || !tl.Done || !strings.Contains(tl.Result, "bg-done") || tl.Detail != "sleep 45; echo bg-done" {
		t.Fatalf("its step: %+v", tl)
	}
	if at != callAt {
		t.Errorf("its step in entry %d, its call's line was in %d", at, callAt)
	}
	n := 0
	for _, x := range toolsOf(c) {
		if strings.Contains(x.Detail, "sleep 45") {
			n++
		}
	}
	if n != 1 {
		t.Errorf("%d steps of it", n)
	}
}
