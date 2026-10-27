package web

import "testing"

// The slider's mark against its names, off real screens of Claude Code
// (the pane is wide, so the block is indented; the rule is drawn in ─).
func TestEffortAtReadsTheSlider(t *testing.T) {
	for _, c := range []struct{ want, rule, names string }{
		{"low", "                                           \u2500\u25b2\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500      Ultracode  off", "                                           low     medium     high     xhigh      max      Tab to toggle"},
		{"medium", "                                           \u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u25b2\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500      Ultracode  off", "                                           low     medium     high     xhigh      max      Tab to toggle"},
		{"high", "                                           \u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u25b2\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500      Ultracode  off", "                                           low     medium     high     xhigh      max      Tab to toggle"},
		{"max", "                                           \u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u25b2\u2500      Ultracode  off", "                                           low     medium     high     xhigh      max      Tab to toggle"},
	} {
		if got := effortAt(c.rule + "\n" + c.names); got != c.want {
			t.Errorf("the mark at %s was read as %q", c.want, got)
		}
	}
	// (No slider on the screen: nothing, not a guess.)
	if got := effortAt("nothing here\nnor here"); got != "" {
		t.Errorf("read %q off a screen with no slider", got)
	}
}

// The slider is on the screen, and the model picker is not mistaken for
// it (its line of keys reads "s to use this session only", the slider's
// "s for this session only") — a real screen of each.
func TestEffortShowsTellsTheSliderFromThePicker(t *testing.T) {
	if !effortShows(effortScreen) {
		t.Error("the slider was not seen")
	}
	picker := "   Select model\n     1.  Default (recommended)  Sonnet 5.5\n   ❯ 2.  Opus 5.5 ✔\n\n   Enter to set as default · s to use this session only · Esc to cancel"
	if effortShows(picker) {
		t.Error("the model picker was taken for the slider")
	}
	if !pickerShows(picker) || pickerShows(effortScreen) {
		t.Error("the picker and the slider are not told apart")
	}
}

// What it says over the input once "s" was pressed ("◐ medium · /effort").
func TestEffortTookReadsWhatItSays(t *testing.T) {
	for _, c := range []struct{ screen, want string }{
		{"      ◐ medium · /effort\n────\n❯\n", "medium"},
		// (What earlier switches said stays in the thread: the last is this one's.)
		{"   ◈ max · /effort\n   ○ low · /effort\n────\n❯\n", "low"},
		{"      ◉ xhigh · /effort", "xhigh"},
		{"      ◈ max · /effort", "max"},
		{"nothing of it", ""},
		{"      ○ sideways · /effort", ""}, // (not a level: nothing)
	} {
		if got := effortTook(c.screen); got != c.want {
			t.Errorf("%q read as %q, want %q", c.screen, got, c.want)
		}
	}
}

const effortScreen = "  \u251c\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u253c\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u253c\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u253c\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2524\n  \u2502 s-tab     \u2502 Vim / Emacs \u7684\u5199\u6cd5                                 \u2502 Shift+Tab \u2502 ^[[Z                     \u2502\n  \u2514\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2534\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2534\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2534\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2518\n\n  \u5982\u679c\u4f60\u5728\u67e5 Shift+Tab \u4e3a\u4ec0\u4e48\u4e0d\u8d77\u4f5c\u7528\uff08\u6bd4\u5982\u5728 Claude Code \u91cc\u5207\u4e0d\u4e86\u6743\u9650\u6a21\u5f0f\uff09\uff0c\u5148\u5728\u666e\u901a\u7ec8\u7aef\u91cc\u8dd1 cat -v\uff0c\u6309 Shift+Tab \u518d\u6309\u56de\u8f66\uff1a\n\n  - \u663e\u793a ^[[Z\uff1a\u7ec8\u7aef\u6ca1\u95ee\u9898\uff0c\u95ee\u9898\u5728\u7a0b\u5e8f\u90a3\u8fb9\u7684\u5feb\u6377\u952e\u8bbe\u7f6e\u3002\n  - \u663e\u793a ^I\uff0c\u6216\u8005\u8ddf\u6309 Tab \u5b8c\u5168\u4e00\u6837\uff1a\u7ec8\u7aef\uff08\u6216 tmux\u3001SSH \u5ba2\u6237\u7aef\uff09\u628a Shift \u5403\u6389\u4e86\uff0c\u6ca1\u6709\u53d1\u51fa\u53cd\u5411 Tab\uff0c\u8981\u53bb\u6539\u7ec8\u7aef\u6216 tmux \u7684\u8bbe\u7f6e\u3002\n  - \u4ec0\u4e48\u90fd\u4e0d\u663e\u793a\uff1a\u8fd9\u4e2a\u7ec4\u5408\u952e\u5728\u66f4\u5916\u9762\u4e00\u5c42\u5c31\u88ab\u622a\u8d70\u4e86\uff0c\u6bd4\u5982\u7a97\u53e3\u7ba1\u7406\u5668\u6216\u7ec8\u7aef\u81ea\u5df1\u7684\u5feb\u6377\u952e\u3002\n\n  \u544a\u8bc9\u6211\u4f60\u5b9e\u9645\u770b\u5230\u7684\u662f\u54ea\u79cd\uff0c\u4ee5\u53ca\u7528\u7684\u662f\u4ec0\u4e48\u7ec8\u7aef\u3001\u6709\u6ca1\u6709\u5957 tmux\uff0c\u6211\u5e2e\u4f60\u5b9a\u4f4d\u3002\n\n\u273b Worked for 20s \u00b7 done 2:52 PM\n\n\u276f /model\n  \u23bf  Kept model as Opus 5.5\n\n\u276f /model\n  \u23bf  Kept model as Opus 5.5\n\n\u276f /model\n  \u23bf  Set model to Opus 5.5 for this session only with xhigh effort\n\n\u276f /effort\n  \u23bf  Cancelled\n\n\u276f /effort\n  \u23bf  Set effort level to medium (this session only): Balanced approach with standard implementation and testing\n\u2594\u2594\u2594\u2594\u2594\u2594\u2594\u2594\u2594\u2594\u2594\u2594\u2594\u2594\u2594\u2594\u2594\u2594\u2594\u2594\u2594\u2594\u2594\u2594\u2594\u2594\u2594\u2594\u2594\u2594\u2594\u2594\u2594\u2594\u2594\u2594\u2594\u2594\u2594\u2594\u2594\u2594\u2594\u2594\u2594\u2594\u2594\u2594\u2594\u2594\u2594\u2594\u2594\u2594\u2594\u2594\u2594\u2594\u2594\u2594\u2594\u2594\u2594\u2594\u2594\u2594\u2594\u2594\u2594\u2594\u2594\u2594\u2594\u2594\u2594\u2594\u2594\u2594\u2594\u2594\u2594\u2594\u2594\u2594\u2594\u2594\u2594\u2594\u2594\u2594\u2594\u2594\u2594\u2594\u2594\u2594\u2594\u2594\u2594\u2594\u2594\u2594\u2594\u2594\u2594\u2594\u2594\u2594\u2594\u2594\u2594\u2594\u2594\u2594\u2594\u2594\u2594\u2594\u2594\u2594\u2594\u2594\u2594\u2594\u2594\u2594\u2594\u2594\u2594\u2594\u2594\u2594\u2594\u2594\u2594\u2594\u2594\u2594\u2594\u2594\u2594\u2594\u2594\u2594\u2594\u2594\u2594\n   Effort\n\n                                           Faster                             Smarter\n                                           \u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u25b2\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500      Ultracode  off\n                                           low     medium     high     xhigh      max      Tab to toggle\n\n                                                  ~1.5x the estimated cost of medium (the default)\n\n\n   \u2190/\u2192 to adjust \u00b7 Enter to confirm \u00b7 s for this session only \u00b7 Esc to cancel\n"

// A pane too narrow for the slider wraps both its lines, so the tail of
// the rule lies between the mark and the names (a real screen of 74
// columns): the mark is still read off the line it is really on.
func TestEffortAtReadsAWrappedSlider(t *testing.T) {
	if got := effortAt(wrappedSlider); got != "high" {
		t.Errorf("the mark read as %q, want high", got)
	}
	// (The tail of a wrapped line begins at the left: its columns say
	// nothing, and a mark there is no word on where the slider stands.)
	tail := " \u25b2 off\n                 low     medium     high     xhigh      max      Tab to tog"
	if got := effortAt(tail); got != "" {
		t.Errorf("a mark on a wrapped tail was read as %q", got)
	}
}

const wrappedSlider = "                 \u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u25b2\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500      Ultracode\n off\n                 low     medium     high     xhigh      max      Tab to tog"
