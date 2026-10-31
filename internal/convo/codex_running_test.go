package convo

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// Rows as a real Codex 0.164 wrote them (cxprobe4.py, cxsub.py): a command
// still running when its call came back, then told of as ended after the
// turn; a subagent sent off, then told of as done.
const (
	runCall   = `{"timestamp":"2026-10-31T06:27:57.600Z","type":"response_item","payload":{"type":"function_call","name":"exec_command","arguments":"{\"cmd\": \"sleep 25; echo fertig\", \"workdir\": \"/home/alice/cxreal\", \"yield_time_ms\": 1000}","call_id":"call_4"}}` + "\n"
	runOut    = `{"timestamp":"2026-10-31T06:27:58.780Z","type":"response_item","payload":{"type":"function_call_output","call_id":"call_4","output":"Chunk ID: fca447\nWall time: 1.0014 seconds\nProcess running with session ID 8873\nOriginal token count: 0\nOutput:\n"}}` + "\n"
	runEnded  = `{"timestamp":"2026-10-31T06:28:23.000Z","type":"event_msg","payload":{"type":"item_completed","item":{"type":"CommandExecution","id":"call_4","process_id":"8873","command":["/bin/bash","-lc","sleep 25; echo fertig"],"parsed_cmd":[{"type":"unknown","cmd":"sleep 25; echo fertig"}],"source":"unified_exec_startup","status":"completed","aggregated_output":"fertig\n","exit_code":0}}}` + "\n"
	quickCall = `{"timestamp":"2026-10-31T06:27:59.000Z","type":"response_item","payload":{"type":"function_call","name":"exec_command","arguments":"{\"cmd\": \"ls\"}","call_id":"call_9"}}` + "\n"
	quickOut  = `{"timestamp":"2026-10-31T06:27:59.100Z","type":"response_item","payload":{"type":"function_call_output","call_id":"call_9","output":"Chunk ID: 72149c\nWall time: 0.0000 seconds\nProcess exited with code 0\nOriginal token count: 13\nOutput:\ncxmock.log\n"}}` + "\n"
	subCall   = `{"timestamp":"2026-10-31T06:18:03.100Z","type":"response_item","payload":{"type":"function_call","name":"spawn_agent","namespace":"multi_agent_v1","arguments":"{\"message\": \"#sub 数一数文件\", \"task_name\": \"counter\"}","call_id":"call_5"}}` + "\n"
	subOut    = `{"timestamp":"2026-10-31T06:18:03.200Z","type":"response_item","payload":{"type":"function_call_output","call_id":"call_5","output":"{\"agent_id\":\"01a1909b-aa1e-76f1-ad4d-0bf3d35d3293\",\"nickname\":\"Euclid\"}"}}` + "\n"
	subDone   = `{"timestamp":"2026-10-31T06:18:04.000Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"<subagent_notification>\n{\"agent_path\":\"01a1909b-aa1e-76f1-ad4d-0bf3d35d3293\",\"status\":{\"completed\":\"这里有几个文件。\"}}\n</subagent_notification>"}]}}` + "\n"
	subStill  = `{"timestamp":"2026-10-31T06:18:03.500Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"<subagent_notification>\n{\"agent_path\":\"01a1909b-aa1e-76f1-ad4d-0bf3d35d3293\",\"status\":\"running\"}\n</subagent_notification>"}]}}` + "\n"
	subStill2 = `{"timestamp":"2026-10-31T06:18:03.600Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"<subagent_notification>\n{\"agent_path\":\"01a1909b-aa1e-76f1-ad4d-0bf3d35d3293\",\"status\":{\"running\":null}}\n</subagent_notification>"}]}}` + "\n"
)

func runningIDs(c *Codex) string {
	var ids []string
	for _, r := range c.Running() {
		ids = append(ids, r.Kind+":"+r.ID+":"+r.What)
	}
	return strings.Join(ids, " ")
}

func TestCodexRunningCommand(t *testing.T) {
	c := feedAll(runCall, runOut, quickCall, quickOut)
	if got := runningIDs(c); got != "cmd:call_4:sleep 25; echo fertig" {
		t.Fatalf("still running: %q", got)
	}
	c.Feed([]byte(runEnded))
	if got := runningIDs(c); got != "" {
		t.Errorf("told of as ended: %q", got)
	}
	var res string
	for _, tl := range toolsOf(c) {
		if tl.ID == "call_4" {
			res = tl.Result
		}
	}
	if !strings.Contains(res, "fertig") {
		t.Errorf("its card: what it printed, %q", res)
	}
}

func TestCodexRunningSubagent(t *testing.T) {
	c := feedAll(subCall, subOut)
	if got := runningIDs(c); got != "agent:01a1909b-aa1e-76f1-ad4d-0bf3d35d3293:Euclid: counter" {
		t.Fatalf("sent off: %q", got)
	}
	c.Feed([]byte(subStill + subStill2))
	if got := runningIDs(c); got == "" {
		t.Errorf("told of as still running: gone")
	}
	c.Feed([]byte(subDone))
	if got := runningIDs(c); got != "" {
		t.Errorf("done: %q", got)
	}
	// (The notification is no message of the person's.)
	for _, e := range c.Since(0) {
		if e.Role == RoleUser {
			t.Errorf("a person's message: %+v", e.Parts)
		}
	}
}

// What was set going before the process it is read for started went with
// the process before: not shown. (Kept: a start told wrong a while, then
// right again, loses nothing — review503.)
func TestCodexRunningSince(t *testing.T) {
	c := feedAll(runCall, runOut)
	at, _ := time.Parse(time.RFC3339Nano, "2026-10-31T06:30:00Z")
	c.QueueSince(at.UnixMilli())
	if got := runningIDs(c); got != "" {
		t.Errorf("of the process before: %q", got)
	}
	at, _ = time.Parse(time.RFC3339Nano, "2026-10-31T06:00:00Z")
	c.QueueSince(at.UnixMilli())
	if got := runningIDs(c); got == "" {
		t.Errorf("of this process, its start told right again: gone")
	}
}

// Read in parts: set going in the part before, ended in this one (or
// not); ended in the part before.
func TestCodexRunningAcrossPrepend(t *testing.T) {
	before, after := feedAll(runCall, runOut, subCall, subOut), feedAll(runEnded)
	after.Prepend(before)
	if got := runningIDs(after); got != "agent:01a1909b-aa1e-76f1-ad4d-0bf3d35d3293:Euclid: counter" {
		t.Errorf("command ended since, subagent going: %q", got)
	}
	// (Its end read first, its start after: not set going again.)
	c := feedAll(runEnded)
	c.Feed([]byte(runCall + runOut))
	if got := runningIDs(c); got != "" {
		t.Errorf("ended before its start was read: %q", got)
	}
}

// review502: the subagent's state a word, not an object.
func TestCodexRunningStateWord(t *testing.T) {
	for _, st := range []string{"shutdown", "not_found", "errored"} {
		c := feedAll(subCall, subOut)
		c.Feed([]byte(strings.Replace(subDone, `{\"completed\":\"这里有几个文件。\"}`, `\"`+st+`\"`, 1)))
		if got := runningIDs(c); got != "" {
			t.Errorf("%s: %q", st, got)
		}
	}
}

// review502: its call in the part before, what came back of it in this
// one: going all the same, once the parts are put together.
func TestCodexRunningCallBefore(t *testing.T) {
	before, after := feedAll(runCall), feedAll(runOut)
	after.Prepend(before)
	if got := runningIDs(after); got != "cmd:call_4:sleep 25; echo fertig" {
		t.Errorf("its call before: %q", got)
	}
}

// review502: the ends of many, then one told of again: the rest not
// forgotten (a start read later of one of them not set going again).
func TestCodexRunningEndsKept(t *testing.T) {
	c := NewCodex()
	for i := 0; i < maxRunning-1; i++ {
		c.endRun(fmt.Sprintf("call_x%d", i))
	}
	c.endRun("call_4")
	c.endRun("call_x0") // (one told of again: one place, not all of them gone)
	c.Feed([]byte(runCall + runOut))
	if got := runningIDs(c); got != "" {
		t.Errorf("ended before its start was read: %q", got)
	}
}

// review502: what the process before left, not shown, takes no place from
// what this one sets going.
func TestCodexRunningOldTakesNoPlace(t *testing.T) {
	var rows []string
	for i := 0; i < maxRunning; i++ {
		rows = append(rows, strings.ReplaceAll(strings.ReplaceAll(runCall+runOut, "call_4", fmt.Sprintf("call_o%d", i)), "8873", fmt.Sprint(9000+i)))
	}
	c := feedAll(rows...)
	at, _ := time.Parse(time.RFC3339Nano, "2026-10-31T06:28:00Z")
	c.QueueSince(at.UnixMilli())
	c.Feed([]byte(strings.ReplaceAll(strings.ReplaceAll(runCall+runOut, "06:27:5", "06:29:5"), "call_4", "call_new")))
	if got := runningIDs(c); got != "cmd:call_new:sleep 25; echo fertig" {
		t.Errorf("this process's: %q", got)
	}
}

// review503: a part before read in, full of what the process before left:
// it takes no place from what this one sets going.
func TestCodexRunningOldPartTakesNoPlace(t *testing.T) {
	var rows []string
	for i := 0; i < maxRunning; i++ {
		rows = append(rows, strings.ReplaceAll(strings.ReplaceAll(runCall+runOut, "call_4", fmt.Sprintf("call_o%d", i)), "8873", fmt.Sprint(9000+i)))
	}
	before := feedAll(rows...)
	after := feedAll(strings.ReplaceAll(strings.ReplaceAll(runCall+runOut, "06:27:5", "06:29:5"), "call_4", "call_new"))
	at, _ := time.Parse(time.RFC3339Nano, "2026-10-31T06:28:00Z")
	after.QueueSince(at.UnixMilli())
	after.Prepend(before)
	after.Feed([]byte(strings.ReplaceAll(strings.ReplaceAll(runCall+runOut, "06:27:5", "06:29:5"), "call_4", "call_newer")))
	if got := runningIDs(after); got != "cmd:call_new:sleep 25; echo fertig cmd:call_newer:sleep 25; echo fertig" {
		t.Errorf("this process's: %q", got)
	}
}
