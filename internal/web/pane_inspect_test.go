package web

import (
	"encoding/json"
	"testing"
)

func TestWorktreeBranchOK(t *testing.T) {
	for _, good := range []string{"", "feat/my-task", "fix_123", "v1.2.3", "user/branch-01"} {
		if !worktreeBranchOK(good) {
			t.Errorf("worktreeBranchOK(%q) = false, want true", good)
		}
	}
	for _, bad := range []string{"-flag", "../escape", "a..b", "a//b", "a/", "foo.lock", "bad space", "semi;colon"} {
		if worktreeBranchOK(bad) {
			t.Errorf("worktreeBranchOK(%q) = true, want false", bad)
		}
	}
}

func TestParseHerdrExplain(t *testing.T) {
	raw := json.RawMessage(`{
		"agent": "claude",
		"full_lifecycle_hook_authority": true,
		"screen_detection_skipped": false,
		"evaluated_rules": [
			{"id": "osc_title_working", "matched": true, "state": "working", "region": "osc_title"},
			{"id": "idle_prompt", "matched": false, "state": "idle", "region": "bottom"},
			{"id": "live_turn_working", "matched": true, "state": "working", "region": "bottom_non_empty_lines(12)"}
		]
	}`)
	got := parseHerdrExplain(raw)
	if got == nil || got.Agent != "claude" || got.Authority != "hook" || len(got.MatchedRules) != 2 {
		t.Fatalf("unexpected explain: %+v", got)
	}
	if got.MatchedRules[0].ID != "osc_title_working" || got.MatchedRules[1].ID != "live_turn_working" {
		t.Fatalf("unexpected rules: %+v", got.MatchedRules)
	}
}

func TestParseTmuxPaneInspect(t *testing.T) {
	raw := []byte("pane\t%3|1234|/dev/pts/4|go|1\nproc\t1234 zsh -zsh\nproc\t5678 go go test ./...\n")
	got := parseTmuxPaneInspect(raw)
	if got.Mode != "tmux" || got.Pane != "%3" || got.ShellPID != 1234 || !got.Zoomed {
		t.Fatalf("unexpected tmux inspect header: %+v", got)
	}
	if len(got.Processes) != 2 || got.Processes[1].PID != 5678 || got.Processes[1].Cmdline != "go test ./..." {
		t.Fatalf("unexpected tmux inspect procs: %+v", got.Processes)
	}
}
