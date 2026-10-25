package web

import "testing"

// What may be typed into a pane: text, its line ends made "\n"; never a
// control character that would be pressed rather than typed.
func TestSendText(t *testing.T) {
	ok := map[string]string{
		"hi":                          "hi",
		"a\r\nb\rc\nd":                "a\nb\nc\nd",
		"tab\there":                   "tab\there",
		"中文，带标点！":                     "中文，带标点！",
		"  spaced  ":                  "  spaced  ",
		string(make([]byte, 0)) + "x": "x",
	}
	for in, want := range ok {
		if got, good := sendText(in); !good || got != want {
			t.Errorf("sendText(%q) = %q, %v; want %q", in, got, good, want)
		}
	}
	for _, bad := range []string{"", "   \n ", "esc\x1b[201~", "ctrl-b \x02", "del\x7f", "c1\u009b", "nul\x00", "ls ", "\xff\xfe", string(make([]byte, maxSendText+1))} {
		if _, good := sendText(bad); good {
			t.Errorf("sendText(%q) accepted", bad)
		}
	}
}
