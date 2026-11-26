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
		"npm run dev -- --port 3000":                 false,
		"修复下载按钮没反应":                                  true,
		"Fix the login - redirect loop":              true,
	} {
		if titleSays(title, "agy") != says {
			t.Errorf("titleSays(%q) = %v", title, !says)
		}
	}
}
