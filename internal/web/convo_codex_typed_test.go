package web

import (
	"strings"
	"testing"
)

// A real Codex 0.164's screen (cxprobe4.py), its input holding what is
// given; the person's message before ("› go") in the conversation above.
func codexScreen(input string, footer ...string) string {
	if footer == nil {
		footer = []string{"  gpt-mock default · ~/cxreal", "  ? for shortcuts                                                            ⚠ 1 warning · f2 to view"}
	}
	return "\n  >_ OpenAI Codex (v0.164.0)\n     ~/cxreal\n\n\n› go\n\n\n• 后台跑一个慢命令。\n\n• 它还在跑，我先回来。\n\n  Worked for 1s • 06:27\n\n\n\n\n\n" +
		input + "\n\n" + strings.Join(footer, "\n") + "\n"
}

func TestCodexTypedIn(t *testing.T) {
	const msg = "请帮我检查一下 internal/web 下的测试"
	for _, c := range []struct {
		name         string
		screen, text string
		still, whole bool
	}{
		{"empty: its placeholder", codexScreen("› Ask Codex to do anything"), msg, false, false},
		{"the message whole", codexScreen("› " + msg), msg, true, true},
		{"wrapped on two lines", codexScreen("› 请帮我检查一下\n  internal/web 下的测试"), msg, true, true},
		{"it and more typed after", codexScreen("› " + msg + " 还有别的"), msg, true, false},
		{"another message", codexScreen("› 别的什么消息内容"), msg, false, false},
		{"the same words above, the input empty", codexScreen("› Ask Codex to do anything"), "go go go go go go", false, false},
		{"a paste folded, its length", codexScreen("› [Pasted Content 1200 chars]"), strings.Repeat("x", 1200), true, false},
		{"a paste folded, another length", codexScreen("› [Pasted Content 900 chars]"), strings.Repeat("x", 1200), false, false},
		// (A status line of the person's own, of words not known: where it stands tells it — 11-15.)
		{"an unknown footer under it", codexScreen("› "+msg, "  something else"), msg, true, true},
		{"no footer at all", "• done\n\n› " + msg + "\n\n\n", msg, true, true},
		{"the agent's words under it", "› " + msg + "\n\n• answered\n", msg, false, false},
		// (review r2a-4: no footer shown, two lines after a blank one: words of the input's.)
		{"more of a draft under it", "• done\n\n› " + msg + "\n\n  extra draft\n  more of it\n", msg, true, false},
		// (review r2a-3: a draft beginning "1. ": not a menu.)
		{"a draft like a menu's choice", codexScreen("› 1. " + msg), "1. " + msg, true, true},
		{"a menu's choice", "• done\n\n  Would you like to run the following command?\n\n› 1. Yes, proceed (y)\n  2. No (esc)\n\n  Press enter to confirm or esc to cancel\n", "1. Yes, proceed (y)", false, false},
		// (review r2b-2: a draft of a numbered list, two lines: not a menu — no keys of one under it.)
		{"a draft like a menu, two lines", codexScreen("› 1. " + msg + "\n  2. 再跑一次"), "1. " + msg + "\n2. 再跑一次", true, true},
		// (review r2b-3: under the message after a blank line, a known footer's words and more: words of the input's.)
		// (review r2c-2: the message ends with words like a known footer's, the input holding only its start.)
		{"its start, a footer's words its end", codexScreen("› 请解释这行快捷键提示", "  ? for shortcuts"), "请解释这行快捷键提示\n? for shortcuts", false, false},
		{"a footer's words and more under it", "• done\n\n› " + msg + "\n\n  ? for shortcuts\n  这是额外草稿\n", msg, true, false},
		{"too short to tell", codexScreen("› go"), "go", false, false},
		// (review502: the draft goes on with words like a footer's, then more: not the message whole.)
		{"its words, a footer's, more", codexScreen("› " + msg + "\n  ? for shortcuts\n  再加一句"), msg, true, false},
		// (review503: the draft ends with words like a footer's; its footer wrapped in a narrow pane.)
		{"its words, then a footer's", codexScreen("› " + msg + "\n  ? for shortcuts"), msg, true, false},
		{"its footer wrapped", codexScreen("› "+msg, "  gpt-mock default · ~/cxreal", "  ? for", "  shortcuts"), msg, true, true},
	} {
		if got := codexTypedIn(c.screen, c.text, false); got != c.still {
			t.Errorf("%s: still typed %v", c.name, got)
		}
		if got := codexTypedIn(c.screen, c.text, true); got != c.whole {
			t.Errorf("%s: whole %v", c.name, got)
		}
	}
}

func TestCodexInInput(t *testing.T) {
	const msg = "请帮我检查一下 internal/web 下的测试"
	for _, c := range []struct {
		name           string
		screen         string
		waiting, known bool
	}{
		{"still there", codexScreen("› " + msg), true, true},
		{"taken: its input empty", codexScreen("› Ask Codex to do anything"), false, true},
		{"folded: not seen", codexScreen("› [Pasted Content 1200 chars]"), false, false},
		{"an unknown footer", codexScreen("› "+msg, "  nothing of its footer"), true, true},
		{"no input told apart", "› " + msg + "\n\n• answered\n", false, false},
	} {
		if w, k := codexInInput(c.screen, msg); w != c.waiting || k != c.known {
			t.Errorf("%s: waiting %v known %v", c.name, w, k)
		}
	}
	// (Claude Code's reading of the same screen: no box of its own there.)
	if _, k := inInput(codexScreen("› "+msg), msg); k {
		t.Error("Claude Code's box found on Codex's screen")
	}
}
