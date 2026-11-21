package convo

import (
	"strings"
	"testing"
)

func TestAgyBasicTurnAndTools(t *testing.T) {
	p := New("agy")
	ag, ok := p.(*Agy)
	if !ok {
		t.Fatalf("New(\"agy\") returned %T, want *Agy", p)
	}
	if ag.Status() != "" {
		t.Fatalf("initial status = %q, want empty", ag.Status())
	}

	lines := strings.Join([]string{
		`{"step_index":0,"source":"USER_EXPLICIT","type":"USER_INPUT","status":"DONE","created_at":"2026-11-20T08:31:12Z","content":"<USER_REQUEST>\nFix the bug\n</USER_REQUEST>\n<ADDITIONAL_METADATA>\nThe current local time is: 2026-11-20T16:31:12+08:00.\n</ADDITIONAL_METADATA>","media":[{"mime_type":"image/jpeg","uri":"/home/u/.gemini/antigravity-cli/brain/abc/.user_uploaded/img0.jpg"}]}`,
		`{"step_index":1,"source":"MODEL","type":"PLANNER_RESPONSE","status":"DONE","created_at":"2026-11-20T08:31:15Z","model":"gemini-4-neon-high","input_tokens":1200,"cache_read_tokens":48800,"output_tokens":320,"thinking":"Let me inspect the file and run tests.","content":"I will check the code first.","tool_calls":[{"name":"read_file","args":{"Path":"/repo/main.go","StartLine":1,"EndLine":50}},{"name":"run_command","args":{"CommandLine":"go test ./...","toolSummary":"Run Go tests","WaitMsBeforeAsync":5000}}]}`,
		`{"step_index":2,"source":"SYSTEM","type":"GENERIC","status":"DONE","created_at":"2026-11-20T08:31:16Z","content":"Created At: 2026-11-20T16:31:15+08:00\nCompleted At: 2026-11-20T16:31:16+08:00\npackage main\nfunc main() {}"}`,
		`{"step_index":3,"source":"SYSTEM","type":"GENERIC","status":"ERROR","created_at":"2026-11-20T08:31:17Z","content":"Created At: 2026-11-20T16:31:16+08:00\nCompleted At: 2026-11-20T16:31:17+08:00\nCommand exited with code 1.\nFAIL"}`,
		`{"step_index":4,"source":"MODEL","type":"PLANNER_RESPONSE","status":"DONE","created_at":"2026-11-20T08:31:20Z","model":"gemini-4-neon-high","input_tokens":1500,"cache_read_tokens":50000,"output_tokens":180,"content":"All done."}`,
	}, "\n") + "\n"

	ag.Feed([]byte(lines))

	if ag.Status() != "idle" {
		t.Fatalf("status = %q, want idle", ag.Status())
	}
	if ag.Title() != "Fix the bug" {
		t.Fatalf("title = %q, want Fix the bug", ag.Title())
	}
	meta := ag.Meta()
	if meta.Model != "gemini-4-neon-high" || meta.Effort != "high" {
		t.Fatalf("meta model/effort = %q/%q", meta.Model, meta.Effort)
	}
	if meta.Window != 1_000_000 || meta.WindowBy != "model" {
		t.Fatalf("meta window = %d (%q), want 1000000 (model)", meta.Window, meta.WindowBy)
	}
	if meta.Context != 51500 {
		t.Fatalf("meta context = %d, want 51500", meta.Context)
	}
	if meta.Answer == nil || meta.Answer.Out != 180 {
		t.Fatalf("meta answer = %+v, want Out=180", meta.Answer)
	}

	entries := ag.Since(0)
	if len(entries) != 3 {
		t.Fatalf("got %d entries, want 3", len(entries))
	}
	// Entry 0: User input + image
	if entries[0].Role != RoleUser || len(entries[0].Parts) != 2 {
		t.Fatalf("entry 0 = %+v", entries[0])
	}
	if entries[0].Parts[0].Text != "Fix the bug" {
		t.Fatalf("user text = %q", entries[0].Parts[0].Text)
	}
	if entries[0].Parts[1].Kind != KindImage || !strings.HasSuffix(entries[0].Parts[1].Path, "/img0.jpg") {
		t.Fatalf("user image = %+v", entries[0].Parts[1])
	}
	// Entry 1: Thinking + Text + Read + Bash
	if entries[1].Role != RoleAssistant || len(entries[1].Parts) != 4 {
		t.Fatalf("entry 1 parts = %d (%+v)", len(entries[1].Parts), entries[1])
	}
	readTool := entries[1].Parts[2].Tool
	bashTool := entries[1].Parts[3].Tool
	if readTool.Name != "Read" || readTool.Detail != "/repo/main.go" || !readTool.Done || readTool.Error {
		t.Fatalf("read tool = %+v", readTool)
	}
	if !strings.HasPrefix(readTool.Result, "package main") {
		t.Fatalf("read result did not strip timestamps: %q", readTool.Result)
	}
	if bashTool.Name != "Bash" || bashTool.Title != "Run Go tests" || bashTool.Detail != "go test ./..." || !bashTool.Done || !bashTool.Error {
		t.Fatalf("bash tool = %+v", bashTool)
	}
	// Entry 2: Final assistant text
	if entries[2].Role != RoleAssistant || len(entries[2].Parts) != 1 || entries[2].Parts[0].Text != "All done." {
		t.Fatalf("entry 2 = %+v", entries[2])
	}
}

func TestAgyAskQuestionBackgroundAndCheckpoint(t *testing.T) {
	ag := NewAgy()

	// 1. Ask question pending
	ag.Feed([]byte(`{"step_index":1,"source":"MODEL","type":"PLANNER_RESPONSE","status":"DONE","created_at":"2026-11-20T09:00:00Z","tool_calls":[{"name":"ask_question","args":{"questions":[{"question":"Pick one?","is_multi_select":false,"options":["Alpha","Beta"]}]}}]}` + "\n"))
	if ag.Status() != "busy" {
		t.Fatalf("status with pending ask_question = %q, want busy", ag.Status())
	}
	e := ag.Since(0)[0]
	askTool := e.Parts[0].Tool
	if askTool.Name != "AskUserQuestion" || askTool.Done || len(askTool.Asks) != 1 || len(askTool.Asks[0].Options) != 2 {
		t.Fatalf("pending ask tool = %+v", askTool)
	}

	// 2. Ask question answered
	revBefore := ag.Rev()
	ag.Feed([]byte(`{"step_index":2,"source":"SYSTEM","type":"GENERIC","status":"DONE","created_at":"2026-11-20T09:00:05Z","content":"Created At: 2026-11-20T17:00:00+08:00\nCompleted At: 2026-11-20T17:00:05+08:00\nA1: Beta"}` + "\n"))
	changed := ag.Since(revBefore)
	if len(changed) != 1 || !changed[0].Parts[0].Tool.Done || len(changed[0].Parts[0].Tool.Answers) != 1 || changed[0].Parts[0].Tool.Answers[0] != "Beta" {
		t.Fatalf("answered ask = %+v", changed)
	}

	// 3. Background task start + completion
	ag.Feed([]byte(strings.Join([]string{
		`{"step_index":3,"source":"MODEL","type":"PLANNER_RESPONSE","status":"DONE","created_at":"2026-11-20T09:01:00Z","tool_calls":[{"name":"run_command","args":{"CommandLine":"pnpm run build","toolSummary":"Build web"}}]}`,
		`{"step_index":4,"source":"SYSTEM","type":"GENERIC","status":"RUNNING","created_at":"2026-11-20T09:01:01Z","content":"Created At: 2026-11-20T17:01:00+08:00\nTool is running as a background task with task id: conv-1/task-4\nTask Description: pnpm run build\nTask logs are available at: file:///home/u/logs/task-4.log"}`,
	}, "\n") + "\n"))
	runs := ag.Running()
	if len(runs) != 1 || runs[0].ID != "conv-1/task-4" || runs[0].Out != "/home/u/logs/task-4.log" {
		t.Fatalf("running = %+v", runs)
	}

	ag.Feed([]byte(`{"step_index":5,"source":"SYSTEM","type":"SYSTEM_MESSAGE","status":"DONE","created_at":"2026-11-20T09:01:30Z","content":"The following is a <SYSTEM_MESSAGE>\n\n<SYSTEM_MESSAGE>\n[Message] timestamp=2026-11-20T09:01:30Z sender=conv-1/task-4 priority=MESSAGE_PRIORITY_HIGH content=Task id \"conv-1/task-4\" finished with result:\nCommand exited with code 0.\n</SYSTEM_MESSAGE>"}` + "\n"))
	if len(ag.Running()) != 0 {
		t.Fatalf("running after completion = %+v, want empty", ag.Running())
	}
	all := ag.Since(0)
	lastEv := all[len(all)-1]
	if lastEv.Role != RoleNote || lastEv.Parts[0].Event == nil || lastEv.Parts[0].Event.Kind != EventTask || lastEv.Parts[0].Event.Status != "done" {
		t.Fatalf("task done event = %+v", lastEv)
	}

	// 3b. Background task cancelled via manage_task must be cleared from Running().
	ag.Feed([]byte(strings.Join([]string{
		`{"step_index":6,"source":"MODEL","type":"PLANNER_RESPONSE","status":"DONE","created_at":"2026-11-20T09:02:00Z","tool_calls":[{"name":"run_command","args":{"CommandLine":"gh run watch 123","toolSummary":"Watch CI"}}]}`,
		`{"step_index":7,"source":"SYSTEM","type":"GENERIC","status":"RUNNING","created_at":"2026-11-20T09:02:01Z","content":"Created At: 2026-11-20T17:02:00+08:00\nTool is running as a background task with task id: conv-1/task-7\nTask Description: gh run watch 123"}`,
		`{"step_index":8,"source":"MODEL","type":"PLANNER_RESPONSE","status":"DONE","created_at":"2026-11-20T09:02:10Z","tool_calls":[{"name":"manage_task","args":{"Action":"kill","TaskId":"conv-1/task-7"}}]}`,
		`{"step_index":9,"source":"SYSTEM","type":"GENERIC","status":"DONE","created_at":"2026-11-20T09:02:11Z","content":"Created At: 2026-11-20T17:02:10+08:00\nTask \"conv-1/task-7\" cancelled."}`,
	}, "\n") + "\n"))
	if len(ag.Running()) != 0 {
		t.Fatalf("running after cancel = %+v, want empty", ag.Running())
	}

	// 4. Checkpoint compaction
	ag.Feed([]byte(`{"step_index":6,"source":"SYSTEM","type":"CHECKPOINT","status":"DONE","created_at":"2026-11-20T09:05:00Z","input_tokens":5000,"cache_read_tokens":500000,"content":"# Resuming from a compaction\nSummary body"}` + "\n"))
	all = ag.Since(0)
	if len(all) < 2 || all[len(all)-2].Parts[0].Event.Kind != EventCompact || all[len(all)-1].Role != RoleSummary {
		t.Fatalf("checkpoint entries = %+v", all[len(all)-2:])
	}
}

func TestAgyPrependPairsOrphans(t *testing.T) {
	older := NewAgy()
	older.Feed([]byte(strings.Join([]string{
		`{"step_index":0,"source":"USER_EXPLICIT","type":"USER_INPUT","status":"DONE","created_at":"2026-11-20T08:00:00Z","content":"<USER_REQUEST>\nEarlier prompt\n</USER_REQUEST>"}`,
		`{"step_index":1,"source":"MODEL","type":"PLANNER_RESPONSE","status":"DONE","created_at":"2026-11-20T08:00:02Z","model":"gemini-4-neon-medium","tool_calls":[{"name":"edit_file","args":{"Path":"/repo/a.go","Target":"old","Replacement":"new1\nnew2"}}]}`,
	}, "\n") + "\n"))

	newer := NewAgy()
	newer.Feed([]byte(strings.Join([]string{
		`{"step_index":2,"source":"SYSTEM","type":"GENERIC","status":"DONE","created_at":"2026-11-20T08:00:03Z","content":"Created At: 2026-11-20T16:00:02+08:00\nCompleted At: 2026-11-20T16:00:03+08:00\nEdited /repo/a.go"}`,
		`{"step_index":3,"source":"MODEL","type":"PLANNER_RESPONSE","status":"DONE","created_at":"2026-11-20T08:00:05Z","content":"Finished edit."}`,
	}, "\n") + "\n"))

	newer.Prepend(older)

	entries := newer.Since(0)
	if len(entries) != 3 {
		t.Fatalf("got %d entries after Prepend, want 3", len(entries))
	}
	if !(entries[0].Seq < entries[1].Seq && entries[1].Seq < entries[2].Seq) {
		t.Fatalf("seqs not strictly increasing: %d, %d, %d", entries[0].Seq, entries[1].Seq, entries[2].Seq)
	}
	editTool := entries[1].Parts[0].Tool
	if editTool.Name != "Edit" || !editTool.Done || editTool.Add != 2 || editTool.Del != 1 || editTool.Result != "Edited /repo/a.go" {
		t.Fatalf("prepended edit tool = %+v", editTool)
	}
	if newer.Title() != "Earlier prompt" {
		t.Fatalf("title after Prepend = %q, want Earlier prompt", newer.Title())
	}
	if newer.Meta().Model != "gemini-4-neon-medium" || newer.Meta().Effort != "medium" {
		t.Fatalf("meta after Prepend = %+v", newer.Meta())
	}
}

func TestAgySubagentIDAndSetTitle(t *testing.T) {
	ag := NewAgy()
	ag.SetTitle("Long session title")
	if ag.Title() != "Long session title" {
		t.Fatalf("SetTitle = %q", ag.Title())
	}
	subUUID := "11111111-2222-3333-4444-555555555555"
	ag.Feed([]byte(strings.Join([]string{
		`{"step_index":1,"source":"MODEL","type":"PLANNER_RESPONSE","status":"DONE","created_at":"2026-11-20T09:00:00Z","tool_calls":[{"name":"invoke_subagent","args":{"TypeName":"research","Role":"Code Researcher","Prompt":"Survey repo"}}]}`,
		`{"step_index":2,"source":"SYSTEM","type":"GENERIC","status":"DONE","created_at":"2026-11-20T09:00:01Z","content":"Created At: 2026-11-20T17:00:00+08:00\nSubagent launched with conversationId: ` + subUUID + `"}`,
	}, "\n") + "\n"))
	entries := ag.Since(0)
	if len(entries) != 1 || entries[0].Parts[0].Tool.Name != "Agent" || entries[0].Parts[0].Tool.AgentID != subUUID {
		t.Fatalf("subagent tool = %+v", entries[0].Parts[0].Tool)
	}
}

// pbEncode helpers for synthetic Antigravity step_type=17 payloads in tests.
func testPBVarint(v uint64) []byte {
	var out []byte
	for {
		b := byte(v & 0x7f)
		v >>= 7
		if v != 0 {
			out = append(out, b|0x80)
		} else {
			out = append(out, b)
			return out
		}
	}
}

func testPBFieldVarint(field uint64, v uint64) []byte {
	return append(testPBVarint(field<<3|0), testPBVarint(v)...)
}

func testPBFieldBytes(field uint64, b []byte) []byte {
	out := append(testPBVarint(field<<3|2), testPBVarint(uint64(len(b)))...)
	return append(out, b...)
}

func TestAgyDecodeAndMergeDBErrors(t *testing.T) {
	// Build a synthetic protobuf payload matching real step_type=17 idx=1597:
	// f1=17, f4=3, f5.f1.f1=1795232837, f24.f3.{f1,f2,f6,f7,f10,f10}.
	ts := testPBFieldBytes(1, testPBFieldVarint(1, 1795232837))
	meta := testPBFieldBytes(5, ts)
	errInner := append([]byte(nil), testPBFieldBytes(1, []byte("Individual quota reached. Please upgrade your subscription to increase your limits. Resets in 1h37m22s."))...)
	errInner = append(errInner, testPBFieldBytes(2, []byte("RESOURCE_EXHAUSTED (code 429): Individual quota reached."))...)
	errInner = append(errInner, testPBFieldBytes(6, []byte("bdd1dc2a-995b-4663-9447-acc165022e79-1601"))...)
	errInner = append(errInner, testPBFieldVarint(7, 429)...)
	errInner = append(errInner, testPBFieldBytes(10, []byte(`{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"QUOTA_EXHAUSTED"}`))...)
	errInner = append(errInner, testPBFieldBytes(10, []byte(`{"@type":"type.googleapis.com/google.rpc.RetryInfo","retryDelay":"5842.65s"}`))...)
	payload := append(testPBFieldVarint(1, 17), testPBFieldVarint(4, 3)...)
	payload = append(payload, meta...)
	payload = append(payload, testPBFieldBytes(24, testPBFieldBytes(3, errInner))...)

	step, ok := DecodeAgyErrorStep(1601, payload)
	if !ok {
		t.Fatalf("DecodeAgyErrorStep failed")
	}
	if step.CreatedAt != 1795232837 || step.HTTPStatus != 429 || step.Reason != "QUOTA_EXHAUSTED" || step.ErrorID != "bdd1dc2a-995b-4663-9447-acc165022e79-1601" {
		t.Fatalf("unexpected decoded step: %+v", step)
	}
	if step.RetryDelaySec < 5842 || step.RetryDelaySec > 5843 {
		t.Fatalf("RetryDelaySec = %v", step.RetryDelaySec)
	}

	// Compact variant (like idx=575): no f7 and no f10, status in f2 text and Resets in f1.
	compactInner := append([]byte(nil), testPBFieldBytes(1, []byte("Individual quota reached. Resets in 1h16m52s."))...)
	compactInner = append(compactInner, testPBFieldBytes(2, []byte("RESOURCE_EXHAUSTED (code 429): Individual quota reached."))...)
	compactPayload := append(testPBFieldBytes(5, testPBFieldBytes(1, testPBFieldVarint(1, 1795176862))), testPBFieldBytes(24, testPBFieldBytes(3, compactInner))...)
	cStep, ok := DecodeAgyErrorStep(575, compactPayload)
	if !ok {
		t.Fatalf("DecodeAgyErrorStep compact failed")
	}
	trCompact := agyClassifyTrouble(cStep.Message, cStep.Detail, cStep.Reason, cStep.ErrorID, cStep.HTTPStatus, cStep.CreatedAt, cStep.RetryDelaySec)
	if trCompact == nil || trCompact.Kind != TroubleLimit || trCompact.Status != 429 || trCompact.ResetsAt != 1795176862+4612 {
		t.Fatalf("compact trouble = %+v", trCompact)
	}

	// Safety refusal & generic 503 API errors.
	if tr := agyClassifyTrouble("Blocked by safety filter", "", "SAFETY", "", 400, 0, 0); tr == nil || tr.Kind != TroubleRefused {
		t.Fatalf("safety trouble = %+v", tr)
	}
	if tr := agyClassifyTrouble("Service temporarily unavailable", "", "UNAVAILABLE", "err-503", 503, 0, 0); tr == nil || tr.Kind != TroubleAPI || tr.Status != 503 || !strings.Contains(tr.Text, "Error ID: err-503") {
		t.Fatalf("api 503 trouble = %+v", tr)
	}

	// MergeDBErrors: chronological splice + tail append + status flip busy->idle + dedup + window guard.
	ag := NewAgy()
	ag.Feed([]byte(strings.Join([]string{
		`{"step_index":1596,"source":"USER","type":"USER_INPUT","status":"DONE","created_at":"2026-11-21T03:00:00Z","content":"<USER_REQUEST>first</USER_REQUEST>"}`,
		`{"step_index":1598,"source":"USER","type":"USER_INPUT","status":"DONE","created_at":"2026-11-21T03:05:00Z","content":"<USER_REQUEST>second</USER_REQUEST>"}`,
		`{"step_index":1600,"source":"USER","type":"USER_INPUT","status":"DONE","created_at":"2026-11-21T03:20:00Z","content":"<USER_REQUEST>continue</USER_REQUEST>"}`,
	}, "\n") + "\n"))
	if ag.Status() != "busy" {
		t.Fatalf("expected busy after unanswered USER_INPUT, got %q", ag.Status())
	}

	step1597 := AgyErrorStep{Idx: 1597, CreatedAt: 1795231392, Message: "Individual quota reached. Resets in 2h1m28s.", HTTPStatus: 429, Reason: "QUOTA_EXHAUSTED", RetryDelaySec: 7288}
	added, spliced := ag.MergeDBErrors([]AgyErrorStep{cStep, step1597, step})
	if !added || !spliced {
		t.Fatalf("expected added=true spliced=true, got %v %v", added, spliced)
	}
	if ag.Status() != "idle" {
		t.Fatalf("expected idle after tail error step-1601, got %q", ag.Status())
	}
	es := ag.Since(0)
	// cStep (idx 575) must be skipped because it's older than MinStepIndex (1596).
	wantIDs := []string{"step-1596", "step-1597", "step-1598", "step-1600", "step-1601"}
	if len(es) != len(wantIDs) {
		t.Fatalf("len(es)=%d want %d: %+v", len(es), len(wantIDs), es)
	}
	for i, id := range wantIDs {
		if es[i].ID != id || es[i].Seq != i+1 {
			t.Fatalf("es[%d] = ID:%q Seq:%d, want ID:%q Seq:%d", i, es[i].ID, es[i].Seq, id, i+1)
		}
	}
	if tr := es[4].Parts[0].Trouble; tr == nil || tr.Kind != TroubleLimit || tr.ResetsAt != 1795232837+5843 {
		t.Fatalf("tail trouble = %+v", tr)
	}

	// Second call with same errors is a no-op.
	if added2, spliced2 := ag.MergeDBErrors([]AgyErrorStep{step1597, step}); added2 || spliced2 {
		t.Fatalf("expected dedup no-op, got %v %v", added2, spliced2)
	}
}
