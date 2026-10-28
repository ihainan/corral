package web

import (
	"strings"
	"testing"
)

// Claude Code's suggestion, as herdr and tmux give its screen with colours:
// greyed (dim) after the prompt's mark in the box in front; what was typed
// there, its example, an empty box or a dialog in front are none.
func TestSuggestionOf(t *testing.T) {
	rule := "\x1b[0m\x1b[38;2;136;136;136m" + strings.Repeat("─", 40) + "\x1b[0m"
	foot := "  \x1b[38;5;245mOpus 5.5·H │ Ctx 12K\x1b[0m\n  \x1b[38;2;153;153;153m⏵⏵ auto mode on\x1b[0m"
	box := func(line string) string { return "● 好了。\n\n" + rule + "\n" + line + "\n" + rule + "\n" + foot }
	for _, c := range []struct {
		name, screen, want string
	}{
		{"dim, as Claude Code draws it", box("❯ \x1b[0m\x1b[2m提交并推送\x1b[0m"), "提交并推送"},
		{"dim set before the mark", box("\x1b[2m❯ run the tests\x1b[0m"), "run the tests"},
		{"dim carried onto the next line", "● 好了。\n\n" + rule + "\n❯ \x1b[2mrun the tests and\n  fix what fails\x1b[0m\n" + rule + "\n" + foot, "run the tests and fix what fails"},
		{"typed, in a theme's grey (256)", box("❯ \x1b[38;5;244mrun the tests\x1b[0m"), ""},
		{"typed, in a theme's grey (RGB)", box("❯ \x1b[38;2;128;128;128mrun the tests\x1b[0m"), ""},
		{"dim, its colour grey too", box("❯ \x1b[2;38;2;128;128;128mrun the tests\x1b[0m"), "run the tests"},
		{"38;5;2 is a colour, not dim", box("❯ \x1b[38;5;2mrun the tests\x1b[0m"), ""},
		{"typed: the default colour", box("❯ 我希望你检查一下"), ""},
		{"typed, then greyed", box("❯ abc\x1b[2mdef\x1b[0m"), ""},
		{"its example", box("❯ \x1b[2mTry \"edit <filepath> to...\"\x1b[0m"), ""},
		{"an empty box (its mark grey)", box("\x1b[0m\x1b[38;2;153;153;153m❯ \x1b[0m"), ""},
		{"a colour not grey", box("❯ \x1b[38;2;200;80;80mred words\x1b[0m"), ""},
		{"a dialog in front, a box above", box("❯ \x1b[2m提交并推送\x1b[0m") + "\n\n Do you want to proceed?\n ❯ 1. Yes\n   2. No\n", ""},
		{"no box", "$ ls\nREADME.md", ""},
	} {
		if got := suggestionOf(c.screen); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
	// (Bold or reset in between keeps what is greyed so.)
	if got := suggestionOf(box("❯ \x1b[2mfix \x1b[1;2mthe\x1b[22;2m tests\x1b[0m")); got != "fix the tests" {
		t.Errorf("greyed across sequences: %q", got)
	}
}
