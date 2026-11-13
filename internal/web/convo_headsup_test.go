package web

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestHeadsUpWhere(t *testing.T) {
	sid := "ad72bcbb-16d0-4474-ab6b-74adbe11da0b"
	cfg, got, ok := headsUpWhere("/home/u/.claude/projects/-home-u-x/" + sid + ".jsonl")
	if !ok || cfg != "/home/u/.claude" || got != sid {
		t.Fatalf("a log: %q %q %v", cfg, got, ok)
	}
	for _, bad := range []string{
		"",
		"relative/projects/p/" + sid + ".jsonl",
		"/home/u/.claude/projects/p/not-a-session.jsonl",
		"/home/u/.claude/elsewhere/p/" + sid + ".jsonl",
		"/home/u/.claude/projects/p/" + sid + "/subagents/agent-a1.jsonl",
		"/home/u/.claude/projects/p/../p/" + sid + ".jsonl",
		"/home/u/.codex/sessions/2026/11/13/rollout-x.jsonl",
	} {
		if _, _, ok := headsUpWhere(bad); ok {
			t.Errorf("taken: %q", bad)
		}
	}
}

// The host's part, run here: a herdr that prints a screen, the plugin's
// store in a config folder. Shown only while the screen draws the note.
func TestHeadsUpCommand(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("no python3")
	}
	dir := t.TempDir()
	bin, cfg := filepath.Join(dir, "bin"), filepath.Join(dir, "cfg")
	for _, d := range []string{bin, filepath.Join(cfg, "plugins", "store")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	screen := filepath.Join(dir, "screen")
	if err := os.WriteFile(filepath.Join(bin, "herdr"), []byte("#!/bin/sh\ncat \""+screen+"\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	sid, other := "ad72bcbb-16d0-4474-ab6b-74adbe11da0b", "11111111-2222-3333-4444-555555555555"
	store := `{"sessions":[
	 {"id":"` + other + `","tag":"Heads up","view":{"kind":"offer","line":"别的会话的提醒。","explanation":"x","shownAt":"1"}},
	 {"id":"` + sid + `","askedTurn":"t","tag":"Heads up","view":{"kind":"offer","line":"长输出另存的文件实际放在容器的临时目录，不在报告里说的工作目录。.","explanation":"**长输出文件存在容器临时目录**\n- 代价一","shownAt":1794548710893}}
	],"seen":[]}`
	if err := os.WriteFile(filepath.Join(cfg, "plugins", "store", "cc-plugin-you-should-know_builtin-19776595df2c.json"), []byte(store), 0o600); err != nil {
		t.Fatal(err)
	}
	run := func(scr, session string) *headsUpNote {
		t.Helper()
		if err := os.WriteFile(screen, []byte(scr), 0o644); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command("sh", "-c", headsUpCommand("w2R:p1", "/nowhere.sock", cfg, session))
		cmd.Env = append(os.Environ(), "HOME="+dir, "PATH="+bin+":"+os.Getenv("PATH"))
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("ran: %v", err)
		}
		if len(strings.TrimSpace(string(out))) == 0 {
			return nil
		}
		n := parseHeadsUp(out)
		if n == nil {
			t.Fatalf("not read: %q", out)
		}
		return n
	}
	// (As drawn: the line wrapped, the keys below it.)
	shown := "                [-]\n✦ Heads up · 长输出另存的文件实际放在容器的临时目录，不在报告里说的工作目\n  录。.\n  1: Learn more   2: Knew this already   0: Dismiss\n───\n❯\n"
	n := run(shown, sid)
	if n == nil || n.Kind != "offer" || n.Tag != "Heads up" || !strings.HasPrefix(n.Line, "长输出") || !strings.Contains(n.More, "代价一") || n.ShownAt != 1794548710893 {
		t.Fatalf("shown: %+v", n)
	}
	if n := run("● done\n───\n❯\n", sid); n != nil {
		t.Errorf("gone from the screen, kept in the store: %+v", n)
	}
	if n := run("✦ Heads up · 另一条提醒，和存的不是一条\n❯\n", sid); n != nil {
		t.Errorf("a note of another line on the screen: %+v", n)
	}
	if n := run(shown, "99999999-2222-3333-4444-555555555555"); n != nil {
		t.Errorf("a session the store does not know: %+v", n)
	}
	if n := run(shown, other); n != nil {
		t.Errorf("another session's note (its line not on the screen): %+v", n)
	}
}

func TestParseHeadsUp(t *testing.T) {
	for _, bad := range []string{"", "{}", "not json", `{"kind":"offer"}`, `{"kind":"other","line":"x"}`} {
		if n := parseHeadsUp([]byte(bad)); n != nil {
			t.Errorf("%q read as %+v", bad, n)
		}
	}
	long := strings.Repeat("长", headsUpLineMax+10)
	n := parseHeadsUp([]byte(`{"tag":"Heads up","kind":"explained","line":"` + long + `","more":"answer"}`))
	if n == nil || n.Kind != "explained" || len([]rune(n.Line)) != headsUpLineMax+1 || n.More != "answer" {
		t.Fatalf("explained, long: %+v", n)
	}
}
