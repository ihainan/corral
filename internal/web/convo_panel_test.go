package web

import "testing"

func TestPanelKeys(t *testing.T) {
	d := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	for _, c := range []struct {
		keys   []string
		digest string
		ok     bool
	}{
		{[]string{"up"}, "", true},
		{[]string{"esc"}, "", true},
		{[]string{"a"}, "", true},
		{[]string{"shift+tab"}, "", true},
		{[]string{"backspace"}, "", true},
		{[]string{"enter"}, d, true},
		{[]string{"space"}, d, true},
		{[]string{"3"}, d, true},
		// (What settles something: never without the screen it goes on.)
		{[]string{"enter"}, "", false},
		{[]string{"space"}, "nope", false},
		{[]string{"3"}, "", false},
		// (One at a time, of those only: no chord, nothing a shell or tmux reads.)
		{[]string{"up", "enter"}, d, false},
		{nil, "", false},
		{[]string{"ctrl+c"}, "", false},
		{[]string{"C-c"}, "", false},
		{[]string{";"}, "", false},
		{[]string{"-"}, "", false},
		{[]string{"ab"}, "", false},
		{[]string{"\n"}, "", false},
		{[]string{"中"}, "", false},
	} {
		if got := panelOK(c.keys, c.digest); got != c.ok {
			t.Errorf("%q %q: %v", c.keys, c.digest, got)
		}
	}
}

func TestPanelScreenOf(t *testing.T) {
	d := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	text, digest := panelScreenOf([]byte("\x1b[1mModel\x1b[0m\n  1. Opus\n\n\n\x01D " + d + "  -\n"))
	if text != "\x1b[1mModel\x1b[0m\n  1. Opus" || digest != d {
		t.Errorf("%q %q", text, digest)
	}
	// (No sha256sum there: no digest, so nothing that settles something goes.)
	if _, digest := panelScreenOf([]byte("x\n\x01D ")); digest != "" {
		t.Errorf("no digest: %q", digest)
	}
	// (The last one is ours, whatever the screen shows.)
	if text, digest := panelScreenOf([]byte("a\x01D zzz\nb\n\x01D " + d + "  -\n")); digest != d || text != "a\x01D zzz\nb" {
		t.Errorf("%q %q", text, digest)
	}
}
