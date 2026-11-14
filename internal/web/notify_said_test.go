package web

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// What Codex said last, from its log's end: the turn's end with words
// first; else its last message; nothing since the person's last message,
// none. The first line, cut by the tail's start, passed over.
func TestCodexLastSaid(t *testing.T) {
	ended := strings.Join([]string{
		`ut by the tail"}}`,
		`{"type":"event_msg","payload":{"type":"user_message","message":"审一下"}}`,
		`{"type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"先看看。"}]}}`,
		`{"type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"总体上\n\n可以验收。"}]}}`,
		`{"type":"event_msg","payload":{"type":"token_count"}}`,
		`{"type":"event_msg","payload":{"type":"task_complete","last_agent_message":"总体上\n\n可以   验收。"}}`,
		``,
	}, "\n")
	if s := codexLastSaid([]byte(ended)); s != "总体上 可以 验收。" {
		t.Errorf("ended: %q", s)
	}
	// (No turn's end with words, as one stopped: its last message.)
	stopped := strings.Join([]string{
		`{"type":"event_msg","payload":{"type":"user_message","message":"审一下"}}`,
		`{"type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"做了一半"}]}}`,
		`{"type":"event_msg","payload":{"type":"turn_aborted"}}`,
	}, "\n")
	if s := codexLastSaid([]byte(stopped)); s != "做了一半" {
		t.Errorf("stopped: %q", s)
	}
	// (Nothing said since the person's message: not the turn before's.)
	silent := strings.Join([]string{
		`{"type":"event_msg","payload":{"type":"task_complete","last_agent_message":"the turn before"}}`,
		`{"type":"event_msg","payload":{"type":"user_message","message":"再来"}}`,
		`{"type":"response_item","payload":{"type":"function_call","name":"shell"}}`,
		`{"type":"event_msg","payload":{"type":"task_complete","last_agent_message":null}}`,
	}, "\n")
	if s := codexLastSaid([]byte(silent)); s != "" {
		t.Errorf("silent: %q", s)
	}
	long := `{"type":"event_msg","payload":{"type":"task_complete","last_agent_message":"` + strings.Repeat("长", 900) + `"}}`
	if n := len([]rune(codexLastSaid([]byte(long)))); n != maxSaid {
		t.Errorf("long: %d", n)
	}
}

// The command prints the end of the log asked for, its path one argument
// whatever it holds.
func TestCodexTailCommand(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "it's here")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(dir, "rollout-2026-11-14T12-50-51-01a1d864-d7b5-79c2-b461-21279a92ada9.jsonl")
	if err := os.WriteFile(log, []byte(strings.Repeat("x", 100)+"END"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !codexLogName.MatchString(log) {
		t.Fatal("not taken for a Codex log")
	}
	out, err := exec.Command("sh", "-c", codexTailCommand(log, 10)).Output()
	if err != nil || string(out) != "xxxxxxxEND" {
		t.Errorf("%q %v", out, err)
	}
}
