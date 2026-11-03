package web

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Words left in Claude Code's box, as a send sees them first: a message a
// stop interrupted, put back there by Claude Code (the real screen, 2.1.312
// fullscreen), or typed in the terminal; greyed ones are none.
func TestBoxDraft(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("testdata", "view", "restored-after-stop.ansi"))
	if err != nil {
		t.Fatal(err)
	}
	if d, ok := boxDraft(string(b)); !ok || d != "分析现在是否可以部署" {
		t.Errorf("put back by a stop: %q %v", d, ok)
	}
	rule := "\x1b[38;5;244m" + strings.Repeat("─", 40) + "\x1b[39m"
	foot := "\x1b[39m  \x1b[38;5;246m⏸ manual mode on\x1b[39m"
	box := func(line string) string { return "● 好了。\n" + rule + "\n" + line + "\n" + rule + "\n" + foot }
	for _, c := range []struct {
		name, screen, want string
		known              bool
	}{
		{"empty", box("\x1b[39m❯ \x1b[7m \x1b[0m"), "", true},
		{"a suggestion (dim)", box("❯ \x1b[0m\x1b[2m提交并推送\x1b[0m"), "", true},
		{"its example", box("❯ \x1b[2mTry \"edit <filepath> to...\"\x1b[0m"), "", true},
		{"a subagent's placeholder (dim, as drawn)", box("❯\u00a0\x1b[0m\x1b[2mMessage @general-purpose…\x1b[0m"), "", true},
		{"a suggestion, as drawn", box("❯\u00a0\x1b[0m\x1b[2m部署\x1b[0m"), "", true},
		{"a grey colour is typed words, not greyed", box("❯ \x1b[38;5;246m灰色的字\x1b[39m"), "灰色的字", true},
		{"typed", box("❯ 我自己打的字\x1b[7m \x1b[0m"), "我自己打的字", true},
		{"typed over two lines", "● 好了。\n" + rule + "\n❯ first line\n  second line\n" + rule + "\n" + foot, "first line second line", true},
		{"typed, then greyed", box("❯ abc\x1b[2mdef\x1b[0m"), "abcdef", true},
		{"a dialog in front", box("❯ words") + "\n Do you want to proceed?\n ❯ 1. Yes\n   2. No", "", false},
		{"no box", "$ ls\nREADME.md", "", false},
	} {
		if d, ok := boxDraft(c.screen); d != c.want || ok != c.known {
			t.Errorf("%s: %q %v, want %q %v", c.name, d, ok, c.want, c.known)
		}
	}
}
