package web

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func cardSample(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(os.Getenv("HOME"), "tmp/corral-devtools/ccmock/samples", name))
	if err != nil {
		t.Skip("sample not here:", name)
	}
	return string(b)
}

// Claude Code's cards above its input box, read off real screens.
func TestCardOf(t *testing.T) {
	c := cardOf(cardSample(t, "headsup-cjk.txt"))
	if c == nil || c.Tag != "Heads up" || !strings.HasPrefix(c.Text, "「看过的不再冒出来」这个修复要靠在主机上找到智能体的对话日志才能判断；找不到日志的智能体，比如部分 Codex会话") {
		t.Fatalf("cjk: %+v", c)
	}
	if len(c.Choices) != 3 || c.Choices[0] != (cardChoice{"1", "Learn more"}) || c.Choices[2] != (cardChoice{"0", "Dismiss"}) {
		t.Fatalf("cjk choices: %+v", c.Choices)
	}
	e := cardOf(cardSample(t, "headsup-en.txt"))
	if e == nil || e.Text != "The main agent has already built in Ctrl+K for searching agents, and on Windows and Linux that takes over the browser's own Ctrl+K." {
		t.Fatalf("en: %+v", e)
	}
	rule := strings.Repeat("─", 40)
	box := "\n" + rule + "\n❯ \n" + rule + "\n  status\n"
	for name, screen := range map[string]string{
		"being made":            "✦ One moment…" + box,
		"quoted in the thread":  "● It said:\n✦ Heads up · something\n  1: Learn more   2: Knew this already   0: Dismiss\n\n● Then I went on.\n" + box,
		"no keys":               "✦ Heads up · something\n  more of it" + box,
		"thread under the card": "✦ Heads up · something\n  1: Learn more   0: Dismiss\n● after it" + box,
	} {
		if got := cardOf(screen); got != nil {
			t.Errorf("%s: %+v", name, got)
		}
	}
	// Opened out (1: Learn more): its explanation, other keys.
	x := cardOf("                                        [-]\n✦ You should know · Needs the log\n\n  Without the log the done mark\n  comes back after an update.\n  1: Understood   2: Chat in main session   0: Dismiss" + box)
	if x == nil || x.Tag != "You should know" || x.Text != "Needs the log\n\nWithout the log the done mark comes back after an update." || len(x.Choices) != 3 || x.Choices[1].Label != "Chat in main session" {
		t.Fatalf("opened: %+v", x)
	}
}
