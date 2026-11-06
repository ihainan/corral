package web

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

var oldTime = time.Now().Add(-3 * time.Hour)

// subScanWrite: a subagent's log, with what its first row says and how it ends.
func subScanWrite(t *testing.T, dir, id string, depth int, what string, rows ...string) {
	t.Helper()
	m, _ := json.Marshal(map[string]any{"description": what, "spawnDepth": depth})
	if err := os.WriteFile(filepath.Join(dir, "agent-"+id+".meta.json"), m, 0o600); err != nil {
		t.Fatal(err)
	}
	var b []byte
	for _, r := range rows {
		b = append(append(b, r...), '\n')
	}
	if err := os.WriteFile(filepath.Join(dir, "agent-"+id+".jsonl"), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

const (
	subFirst  = `{"parentUuid":null,"type":"user","message":{"role":"user","content":"去查"},"uuid":"u0","timestamp":"2026-11-06T01:23:59.373Z"}`
	subAtWork = `{"message":{"model":"m","role":"assistant","content":[{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"go test ./...","description":"跑测试"}}],"stop_reason":"tool_use","usage":{"input_tokens":4,"cache_read_input_tokens":1000,"cache_creation_input_tokens":20,"output_tokens":6}},"type":"assistant","uuid":"a1","timestamp":"2026-11-06T01:24:00.000Z"}`
	subHandBk = `{"message":{"model":"m","role":"assistant","content":[{"type":"tool_use","id":"t2","name":"SubagentHandback","input":{"result":"done"}}],"stop_reason":"tool_use"},"type":"assistant","uuid":"a2","timestamp":"2026-11-06T01:25:00.000Z"}`
	subResult = `{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t2","content":"ok"}]},"uuid":"u2","timestamp":"2026-11-06T01:25:01.000Z"}`
	subAnswer = `{"message":{"model":"m","role":"assistant","content":[{"type":"text","text":"好了"}],"stop_reason":"end_turn"},"type":"assistant","uuid":"a3","timestamp":"2026-11-06T01:26:00.000Z"}`
)

// Run on a shell as the host's: those at work are told, with (detail) their
// call and what they carry; those ended, by handback or a last answer, are
// not; nor one untouched for long, nor a link, nor a name not a log's.
func TestSubScan(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, "subagents")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	subScanWrite(t, dir, "aaaaaaaa1", 1, "直接的", subFirst, subAtWork)
	subScanWrite(t, dir, "aaaaaaaa2", 2, "派生的", subFirst, subAtWork)
	subScanWrite(t, dir, "aaaaaaaa3", 2, "交回了", subFirst, subAtWork, subHandBk, subResult)
	subScanWrite(t, dir, "aaaaaaaa4", 1, "答完了", subFirst, subAtWork, subAnswer)
	subScanWrite(t, dir, "aaaaaaaa5", 2, "早没动静", subFirst, subAtWork)
	if err := os.Chtimes(filepath.Join(dir, "agent-aaaaaaaa5.jsonl"), oldTime, oldTime); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(dir, "agent-aaaaaaaa1.jsonl"), filepath.Join(dir, "agent-aaaaaaaa6.jsonl")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "agent-bad name.jsonl"), []byte(subFirst+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run := func(detail bool) map[string]subScanAgent {
		t.Helper()
		out, err := exec.Command("sh", "-c", subScanCommand(home, detail)).Output()
		if err != nil {
			t.Fatal(err)
		}
		got := map[string]subScanAgent{}
		for _, a := range parseSubScan(out) {
			got[a.ID] = a
		}
		return got
	}
	brief := run(false)
	if len(brief) != 2 || brief["aaaaaaaa1"].Depth != 1 || brief["aaaaaaaa2"].Depth != 2 {
		t.Fatalf("those at work: %+v", brief)
	}
	if a := brief["aaaaaaaa2"]; a.What != "派生的" || a.Time != "2026-11-06T01:23:59.373Z" || a.Tool != nil || a.Tokens != 0 {
		t.Errorf("brief: %+v", a)
	}
	full := run(true)
	a := full["aaaaaaaa1"]
	if a.Tool == nil || a.Tool.Name != "Bash" || a.Tool.Title != "跑测试" || a.Tool.Done || a.Tokens != 1030 {
		t.Errorf("detail: %+v %+v", a, a.Tool)
	}

	// (A link in the session's folder's place: nothing.)
	link := filepath.Join(t.TempDir(), "s")
	if err := os.Symlink(home, link); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("sh", "-c", subScanCommand(link, false)).Output()
	if err != nil || len(parseSubScan(out)) != 0 {
		t.Errorf("through a link: %v %q", err, out)
	}
	// (No such folder: nothing, not an error.)
	if out, err := exec.Command("sh", "-c", subScanCommand(filepath.Join(home, "none"), true)).Output(); err != nil || len(parseSubScan(out)) != 0 {
		t.Errorf("no folder: %v %q", err, out)
	}
}
