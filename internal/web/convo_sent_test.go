package web

import (
	"strings"
	"testing"
)

// A message that was typed into a pane and has not turned up in the log is
// looked for in the pane's input box, and nowhere else.
func TestInInputLooksOnlyAtTheInput(t *testing.T) {
	const msg = "把 /home/alice/proj/internal/web/conversation.go 里的超时改成 30 秒"
	box := func(holds string) string {
		return "● 在。\n\n─────────────────────────\n❯ " + holds + "\n─────────────────────────\n  ⏵⏵ auto mode on\n"
	}
	// Still in the input, wrapped as the pane wraps it: it was not taken in.
	wrapped := box("把 /home/alice/proj/internal/web/conver\nsation.go 里的超时改成 30 秒")
	if w, known := inInput(wrapped, msg); !known || !w {
		t.Errorf("a message wrapped in the input: waiting %v, known %v", w, known)
	}
	// Taken in: the pane writes it into the thread above and the input is
	// empty. Finding it up there would say the opposite of the truth.
	above := "● 在。\n\n> " + msg + "\n\n● 好的，改完了。\n─────────────────────────\n❯\n─────────────────────────\n  ⏵⏵ auto mode on\n"
	if w, known := inInput(above, msg); !known || w {
		t.Errorf("a message taken in and echoed above: waiting %v, known %v", w, known)
	}
	// A message too long for the box: it shows the end of it (a line or
	// two), and the end is what is looked for as well as the longest run.
	long := strings.Repeat("改一下这个很长的说明文字。", 20) + "最后这一句话是它的结尾了"
	tail := []rune(long)
	if w, known := inInput(box("…"+string(tail[len(tail)-40:])), long); !known || !w {
		t.Errorf("an input showing the end of a long message: waiting %v, known %v", w, known)
	}
	// No input box on the screen: nothing is claimed either way.
	if _, known := inInput("just some words\nand more", msg); known {
		t.Error("something was claimed of a screen with no input box")
	}
	// Too little of its own to tell apart: nothing is claimed.
	if _, known := inInput(box("ok"), "ok"); known {
		t.Error("two letters were taken for the message")
	}
}

// The screen is kept from its end, where the input is: a long one cut at
// the front still shows the box that answers the question.
func TestSentScreenKeepsItsEnd(t *testing.T) {
	s := strings.Repeat("x", 100) + "\n❯ the input\n"
	got := lastOf(s, 20)
	if !strings.HasSuffix(got, "❯ the input\n") || !strings.HasPrefix(got, "…") {
		t.Errorf("kept %q", got)
	}
	if lastOf("short", 20) != "short" {
		t.Error("a short screen was cut")
	}
}

// What is looked for is cut on whole characters: half of one would never
// be found, and a message of nothing but Chinese is the common case.
func TestAnchorsAreWholeCharacters(t *testing.T) {
	msg := "a" + strings.Repeat("中", 41) // (its 120th byte falls inside a character)
	marks := anchors(msg)
	if len(marks) == 0 {
		t.Fatal("no anchor for a long Chinese message")
	}
	for _, m := range marks {
		if strings.ContainsRune(m, '�') {
			t.Errorf("an anchor was cut through a character: %q", m)
		}
		if !strings.Contains(squeeze(msg), m) {
			t.Errorf("anchor %q is not part of the message", m)
		}
	}
}

// Enter is pressed again on a message only where the box in front holds it
// from its start: not on a box of the thread above while a permission
// dialog is in front, not on the box's placeholder, not on a box holding
// only some of it.
func TestStillTyped(t *testing.T) {
	rule := strings.Repeat("─", 40)
	msg := "提交所有的代码并 push。下一个问题，右下角的 context 为什么是 200K /home/u/shot.png"
	foot := "  Opus 5.5·H │ Ctx 0\n  ⏵⏵ auto mode on (shift+tab to cycle)"
	box := func(in string) string { return rule + "\n❯ " + in + "\n" + rule + "\n" + foot }
	for _, c := range []struct {
		name   string
		screen string
		want   bool
	}{
		{"still in the box", "● 在。\n\n" + box("提交所有的代码并 push。下一个问题，右下角的\n  context 为什么是 200K [Image #1]"), true},
		{"taken in: the box empty", "> " + msg + "\n\n" + box(""), false},
		{"the placeholder only", box(`Try "edit <filepath> to..."`), false},
		{"queued while it works: the box empty", "✢ Working…\n  ⎿ queued: " + msg + "\n" + box(""), false},
		{"a dialog in front, a box in the thread above", "● output:\n" + box(msg) + "\n\n" + strings.Repeat("x\n", 3) +
			" Bash command\n   rm -rf build\n Do you want to proceed?\n ❯ 1. Yes\n   2. No\n\n Esc to cancel\n", false},
		{"only its end in the box", box("context 为什么是 200K /home/u/shot.png"), false},
		{"no box at all", "$ ls\nREADME.md\n", false},
		{"a short dialog under an old box", box(msg)[:len(box(msg))-len(foot)] + "\n Do you want to proceed?\n ❯ 1. Yes\n   2. No\n Esc to cancel", false},
		{"a dialog's numbered choice under it", box(msg) + "\n 1. Yes", false},
	} {
		if got := stillTyped(c.screen, msg); got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
	// What Claude Code shows of a paste at its start: a long one folded, a
	// picture's path as [Image #N].
	long := "第一行很长的说明文字\n第二行\n第三行\n第四行"
	if !stillTyped(box("[Pasted text #1 +3 lines]"), long) {
		t.Error("a long paste folded, still in the box: not pressed for")
	}
	if stillTyped(box("[Pasted text #1 +9 lines]"), long) {
		t.Error("another paste's fold taken for it")
	}
	pic := "/home/u/shot.png 这张图里右下角为什么是 200K"
	if !stillTyped(box("[Image #1] 这张图里右下角为什么是 200K"), pic) {
		t.Error("a picture at its start, still in the box: not pressed for")
	}
	if !stillTyped(box("[Image #1] 请分析这张图"), "'/home/user name/shot.png' 请分析这张图") {
		t.Error("a quoted path to a picture at its start: not pressed for")
	}
	if !stillTyped(box(`[Image #1] "请分析这张图"`), `/home/u/shot.png "请分析这张图"`) {
		t.Error("words in quotes after a path: their quote taken for the path's")
	}
	if stillTyped(box("[Image #1] 请分析这张图"), `'/home/u/shot.png" 请分析这张图`) {
		t.Error("quotes of two kinds taken for a pair")
	}
	// Whole (for Enter pressed later, or from the page): all of it and
	// nothing more, a picture anywhere in it as Claude Code shows it.
	stuck := "额外插一句，这个按钮非常丑。\n\n/home/u/.cache/up/20261029.png\n\n调研一下其他软件的按钮样式。"
	for _, c := range []struct {
		name string
		box  string
		want bool
	}{
		{"all of it, its picture as shown", "额外插一句，这个按钮非常丑。\n  [Image #1]\n  调研一下其他软件的按钮样式。", true},
		{"another message that begins as it does", "额外插一句，这个按钮非常丑。\n  [Image #1]\n  调研一下别的东西。", false},
		{"it and more after", "额外插一句，这个按钮非常丑。\n  [Image #1]\n  调研一下其他软件的按钮样式。再加一句", false},
		{"its picture still as a path", "额外插一句，这个按钮非常丑。\n  /home/u/.cache/up/20261029.png\n  调研一下其他软件的按钮样式。", false},
	} {
		if got := typedWhole(box(c.box), stuck); got != c.want {
			t.Errorf("whole, %s: %v, want %v", c.name, got, c.want)
		}
	}
	if typedWhole(box("[Pasted text #1 +3 lines]"), long) {
		t.Error("a folded paste taken as whole")
	}
	if !typedWhole(box(msg[:len(msg)-len("/home/u/shot.png")]+"[Image #1]"), msg) || typedWhole(box("提交所有的代码并 push。"), msg) {
		t.Error("whole: a picture at its end, or a part of it")
	}
	if stillTyped(box("[Image #1] 别的话"), pic) {
		t.Error("another message after a picture taken for it")
	}
	// (Too short a message to tell its start from anything: never pressed for.)
	if stillTyped(box("ok"), "ok") {
		t.Error("a short message pressed for")
	}
}
