package web

import (
	"strings"
	"testing"
)

// An Antigravity's "finished" alert: its last words, not its command line (11-26).
func TestAgyLastSaid(t *testing.T) {
	user := `{"step_index":0,"source":"USER_EXPLICIT","type":"USER_INPUT","status":"DONE","created_at":"2026-11-20T08:31:12Z","content":"<USER_REQUEST>\nFix the bug\n</USER_REQUEST>"}`
	mid := `{"step_index":1,"source":"MODEL","type":"PLANNER_RESPONSE","status":"DONE","created_at":"2026-11-20T08:31:15Z","content":"I will check the code first.","tool_calls":[{"name":"run_command","args":{"CommandLine":"go test ./...","toolSummary":"Run Go tests"}}]}`
	end := `{"step_index":3,"source":"MODEL","type":"PLANNER_RESPONSE","status":"DONE","created_at":"2026-11-20T08:31:20Z","content":"All done:   the bug is fixed."}`
	cut := `ep_index":9,"type":"GENERIC"}` // (the tail's first line, cut)
	if got := agyLastSaid([]byte(strings.Join([]string{cut, user, mid, end}, "\n") + "\n")); got != "All done: the bug is fixed." {
		t.Errorf("last words: %q", got)
	}
	if got := agyLastSaid([]byte(strings.Join([]string{cut, mid, user}, "\n") + "\n")); got != "" {
		t.Errorf("nothing said since the person's message: %q", got)
	}
	for title, says := range map[string]bool{
		"agy --model gemini-4-neon-high --mode auto": false,
		"/usr/local/bin/agy":                         false,
		"修复下载按钮没反应":                                  true,
		"修复 --help 输出格式":                             true,
		"claude --resume abc":                        false,
		"Fix the login - redirect loop":              true,
	} {
		if titleSays(title, "agy") != says {
			t.Errorf("titleSays(%q) = %v", title, !says)
		}
	}
}

// Its tail read only where written lately, quoted; a description taken up to the next call's id only.
func TestAgyTailCommand(t *testing.T) {
	c := agyTailCommand("/home/u/.gemini/antigravity-cli/brain/x y/.system_generated/logs/transcript_full.jsonl", 1024)
	if !strings.Contains(c, "find '/home/u/.gemini/antigravity-cli/brain/x y/") || !strings.Contains(c, "-mmin -5") || !strings.HasSuffix(c, "tail -c 1024 -- '/home/u/.gemini/antigravity-cli/brain/x y/.system_generated/logs/transcript_full.jsonl'") {
		t.Fatal(c)
	}
}
