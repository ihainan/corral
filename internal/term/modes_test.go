package term

import (
	"bytes"
	"strings"
	"testing"
)

func preludeOf(chunks ...string) string {
	var m modes
	for _, c := range chunks {
		m.feed([]byte(c))
	}
	return string(m.prelude())
}

// What herdr writes as it starts (taken from a recording), and nothing else.
const herdrStart = "\x1b[?1006l\x1b[?1016l\x1b[?1015l\x1b[?1005l\x1b[?1003l\x1b[?1002l\x1b[?1000l\x1b[?1049h" +
	"\x1b[?1000h\x1b[?1002h\x1b[?1003h\x1b[?1015h\x1b[?1006h\x1b[?2004h\x1b[?1004h\x1b[?2031h\x1b[>7u\x1b[?7l" +
	"\x1b[?2026h\x1b[?25l\x1b[Hscreen\x1b[?25h\x1b[?2026l"

func TestModesPrelude(t *testing.T) {
	want := "\x1b[?7l\x1b[?1004h\x1b[?2004h\x1b[?1049h\x1b[?1003h\x1b[?1006h"
	if got := preludeOf(herdrStart); got != want {
		t.Fatalf("herdr's modes: got %q, want %q", got, want)
	}
	// Split anywhere, even inside a sequence.
	var m modes
	for i := 0; i < len(herdrStart); i++ {
		m.feed([]byte{herdrStart[i]})
	}
	if got := string(m.prelude()); got != want {
		t.Fatalf("fed byte by byte: got %q", got)
	}
	for _, c := range []struct{ name, in, want string }{
		{"fresh terminal", "plain text\r\n\x1b[31mred\x1b[0m", ""},
		{"turned off again", "\x1b[?1000h\x1b[?2004h\x1b[?1000l\x1b[?2004l", ""},
		{"several in one", "\x1b[?1;1002;1006h", "\x1b[?1h\x1b[?1002h\x1b[?1006h"},
		{"any reset ends mouse reporting", "\x1b[?1003h\x1b[?9l", ""},
		{"47 is the alternate screen", "\x1b[?47h", "\x1b[?1049h"},
		{"hidden cursor, insert mode, keypad", "\x1b[?25l\x1b[4h\x1b=", "\x1b[?25l\x1b[4h\x1b="},
		{"keypad by mode 66, then numeric", "\x1b[?66h\x1b>", ""},
		{"RIS", herdrStart + "\x1bc", ""},
		{"DECSTR keeps mouse and screen", herdrStart + "\x1b[!p", "\x1b[?1049h\x1b[?1003h\x1b[?1006h"},
		{"not SM/RM", "\x1b[?1000$p\x1b[>1000h\x1b[?1000 h", ""},
		{"inside OSC", "\x1b]2;[?1000h\x07\x1bP+q[?1000h\x1b\\", ""},
		{"cancelled", "\x1b[?1000\x18h", ""},
		{"only 32 parameters count", "\x1b[?" + strings.Repeat("1;", 200) + "1000h", "\x1b[?1h"},
		{"the 33rd is dropped", "\x1b[?" + strings.Repeat("0;", 32) + "1003h", ""},
		{"long digits", "\x1b[?" + strings.Repeat("0", 252) + "1003h", "\x1b[?1003h"},
		{"C1 CSI inside a CSI", "\x1b[?1000\xc2\x9b?1003h", "\x1b[?1003h"},
		{"prefix not first", "\x1b[1?1003h", ""},
		{"many in one", "\x1b[?1;7;25;1000;1002;1003;1004;1006;2004h", "\x1b[?1h\x1b[?1004h\x1b[?2004h\x1b[?1003h\x1b[?1006h"},
		{"DECSTR with a parameter", "\x1b[?2004h\x1b[0!p", ""},
		{"C1 CSI in UTF-8", "\xc2\x9b?1003h", "\x1b[?1003h"},
		{"C1 OSC, ended by C1 ST", "\xc2\x9d2;[?1000h title\xc2\x9c\x1b[?2004h", "\x1b[?2004h"},
		{"not a C1 control", "\xc2\xa9[?1003h", ""},
		{"C0 inside ESC", "\x1b\n[?1003h", "\x1b[?1003h"},
		{"subparameters", "\x1b[?1003:1h", "\x1b[?1003h"},
		{"cursor style", "\x1b[5 q", "\x1b[5 q"},
		{"cursor style back to the default", "\x1b[5 q\x1b[0 q", ""},
		{"cursor style reset by DECSTR", "\x1b[3 q\x1b[!p", ""},
		{"not a cursor style", "\x1b[5q\x1b[?5 q", ""},
		{"cursor blinking off (vim)", "\x1b[?12h\x1b[?12l", "\x1b[?12l"},
		{"cursor blinking kept by RIS and DECSTR", "\x1b[?12l\x1bc\x1b[!p", "\x1b[?12l"},
		{"subparameters off", "\x1b[?1003h\x1b[?1003:1l", ""},
	} {
		if got := preludeOf(c.in); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

func TestRingReplayRestoresModes(t *testing.T) {
	r := newRing(512)
	r.write([]byte(herdrStart))
	if _, _, p := r.all(); p != nil {
		t.Fatalf("nothing trimmed yet: no prelude, got %q", p)
	}
	for i := 0; i < 60; i++ {
		r.write([]byte("some output line\r\n"))
	}
	b, from, p := r.all()
	if from == 0 || bytes.Contains(b, []byte("\x1b[?1003h")) {
		t.Fatalf("herdr's start should be trimmed (from %d)", from)
	}
	if !bytes.Contains(p, []byte("\x1b[?1003h")) || !bytes.Contains(p, []byte("\x1b[?1049h")) {
		t.Fatalf("prelude should restore mouse reporting and the screen, got %q", p)
	}
	// A mode change in the part skipped to reach a line start counts too.
	r = newRing(64)
	r.write([]byte(strings.Repeat("x", 90) + "\x1b[?2004h tail\nnext\n"))
	b, _, p = r.all()
	if bytes.Contains(b, []byte("2004")) || string(p) != "\x1b[?2004h" {
		t.Fatalf("skipped part: replay %q, prelude %q", b, p)
	}
	// A line break inside a sequence is not where the replay starts.
	r = newRing(32)
	r.write([]byte(strings.Repeat("x", 60) + "\x1b\n[?1003h\ntext\n"))
	b, _, p = r.all()
	if string(b) != "text\n" || string(p) != "\x1b[?1003h" {
		t.Fatalf("break inside a sequence: replay %q, prelude %q", b, p)
	}
}
