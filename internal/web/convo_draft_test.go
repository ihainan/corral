package web

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The box's words hashed alike here and on the host (every awk, both
// locales): what Enter for words already in it is checked by.
func TestBoxDigestAwk(t *testing.T) {
	ansi, err := os.ReadFile(filepath.Join("testdata", "view", "restored-after-stop.ansi"))
	if err != nil {
		t.Fatal(err)
	}
	rule := strings.Repeat("─", 40)
	screens := []string{
		sgr.ReplaceAllString(string(ansi), ""),
		"x\n" + rule + "\n❯\u00a0分析现在是否可以部署 \n" + rule + "\n  ⏸ manual mode on\n",
		"x\r\n" + rule + "\r\n❯ first line\r\n  second\u00a0line\r\n" + rule + "\r\n",
		"old\n" + rule + "\n❯ old box\n" + rule + "\nthread\n" + rule + "\n❯ \n" + rule + "\n",
	}
	for _, loc := range []string{"C", "C.UTF-8"} {
		for _, a := range []string{"gawk", "mawk", "busybox"} {
			if _, err := exec.LookPath(a); err != nil {
				continue
			}
			for k, s := range screens {
				args := []string{claudeBoxAwk}
				if a == "busybox" {
					args = append([]string{"awk"}, args...)
				}
				cmd := exec.Command(a, args...)
				cmd.Stdin = strings.NewReader(s)
				cmd.Env = append(os.Environ(), "LC_ALL="+loc)
				out, err := cmd.Output()
				if err != nil {
					t.Fatal(err)
				}
				sum := sha256.Sum256(out)
				if got, want := hex.EncodeToString(sum[:]), boxDigest(s); got != want {
					t.Errorf("screen %d (%s, %s): awk %q", k, a, loc, out)
				}
			}
		}
	}
	if strings.Contains(claudeBoxAwk, "'") {
		t.Fatal("claudeBoxAwk holds a single quote: the script around it breaks")
	}
	for _, c := range []struct {
		lines []string
		full  []bool
		text  string
		want  bool
	}{
		{[]string{"分析现在是否可以部署"}, []bool{false}, "分析现在是否可以部署", true},
		{[]string{"分析", "现在是否可以部署"}, []bool{true, false}, "分析现在是否可以部署", true},                   // (a wrap in a word, the line full)
		{[]string{"the quick brown", "fox"}, []bool{false, false}, "the quick brown fox", true}, // (a wrap at a blank)
		{[]string{"the quick brown", "fox"}, []bool{true, false}, "the quick brown fox", true},
		{[]string{"two  blanks"}, []bool{false}, "two blanks", true},              // (blanks run together, as the page's sameTurn)
		{[]string{"分析 现在是否可以部署"}, []bool{false}, "分析现在是否可以部署", false},             // (a blank more: as the page would not find it)
		{[]string{"first", "second"}, []bool{false, false}, "firstsecond", false}, // (a line break of its own: the page reads a blank — review573)
		{[]string{"first", "second"}, []bool{false, false}, "first second", true},
		{[]string{"Please review internal/web/some_real", "ly_long_directory/conversation.go", "and then check all tests"}, []bool{true, false, false},
			"Please review internal/web/some_really_long_directory/conversation.go and then check all tests", true}, // (both kinds of wrap — review573)
		{[]string{"[Pasted text #1 +3 lines] 请检查"}, []bool{false}, "[Pasted text #1 +3 lines] 请检查", false},
		{[]string{"abc"}, []bool{false}, "abcd", false},
		{nil, nil, "x", false},
	} {
		if got := boxSame(c.lines, c.full, c.text); got != c.want {
			t.Errorf("boxSame(%q, %v, %q) = %v, want %v", c.lines, c.full, c.text, got, c.want)
		}
	}
	// (Which lines reach the edge, off a screen 40 cells wide.)
	r40 := strings.Repeat("─", 40)
	scr := "x\n" + r40 + "\n❯ Please review internal/web/some_real\n  ly_long_directory/conversation.go\n  and then check all tests\n" + r40 + "\n  ⏸ manual mode on\n"
	if ls, full, ok := boxDraftLines(scr); !ok || len(ls) != 3 || !full[0] || full[1] || full[2] || !boxSame(ls, full, "Please review internal/web/some_really_long_directory/conversation.go and then check all tests") {
		t.Errorf("a screen 40 wide: %q %v %v", ls, full, ok)
	}
	if ls, full, _ := boxDraftLines("x\n" + r40 + "\n❯ 分析现在是否可以部署分析现在是否可以部署\n  现在\n" + r40 + "\n"); len(full) != 2 || !full[0] || full[1] {
		t.Errorf("a wide line, its cells counted two each: %q %v", ls, full)
	}
}

// The host's script as sent, every gate: its shell reads it (a name of what
// is put in it, said in a comment, was put in there too — and broke it).
func TestConvoGatedScriptParses(t *testing.T) {
	for _, sh := range [][]string{{"dash", "-n"}, {"busybox", "sh", "-n"}} {
		if _, err := exec.LookPath(sh[0]); err != nil {
			continue
		}
		for _, gate := range []string{"", "free", "enter", "idle", "stop", "box:00", "screen:00", "card:00", "reply:00", "dialog"} {
			cmd := convoGatedCommand(gate, "herdr", "", "p1", "/s", 1, 2, "sid", 0, []string{"enter"}, 0)
			script := strings.TrimPrefix(cmd, "sh -c ")
			// (The quoted script, unquoted by a shell, to a file; then read by the shell under test.)
			f := filepath.Join(t.TempDir(), "s.sh")
			if out, err := exec.Command("sh", "-c", "printf %s "+script[:strings.Index(script, " corral ")]+" > "+f).CombinedOutput(); err != nil {
				t.Fatalf("%s: %v %s", gate, err, out)
			}
			if out, err := exec.Command(sh[0], append(sh[1:], f)...).CombinedOutput(); err != nil {
				t.Errorf("%s, gate %q: %v %s", sh[0], gate, err, out)
			}
		}
	}
}

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
