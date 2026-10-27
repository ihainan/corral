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
