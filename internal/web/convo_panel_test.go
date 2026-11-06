package web

import (
	"os/exec"
	"strings"
	"testing"
)

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
		{[]string{"shift+tab"}, "", false}, // (under a plan, it approves)
		{[]string{"shift+tab"}, d, true},
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

// The gateway's digest of a screen and the host's, the same whatever it holds;
// and of two reads differing only in colours, the same; differing in words, not.
func TestPanelDigest(t *testing.T) {
	for _, s := range []string{
		"\x1b[1;38;5;75mSettings\x1b[0m\n  \x1b[7m❯ Auto-compact\x1b[0m  \x1b[32mtrue\n\n",
		"no newline at its end",
		"",
		"\x1b]8;id=1;https://a.example/\x1b\\link\x1b]8;;\x1b\\\n",
		"中文 ⌕ …\r\nnext\n",
		"with\x00nul\n",
		"\xff\xfe not utf-8\n",
		"a\x1b[38:2::1:2:3mb\n",
		"\x1b[38;5;246m\x1b[49m●\x1b[39m Needs permission\n  ● indented\n⏺ mac\n● ⏺ two\nwords ● here\n",
	} {
		c := exec.Command("sh", "-c", panelDigestPipe)
		c.Stdin = strings.NewReader(s)
		out, err := c.Output()
		if err != nil {
			t.Fatalf("%q: %v", s, err)
		}
		if host, _, _ := strings.Cut(string(out), " "); host != panelDigest([]byte(s)) {
			t.Errorf("%q: host %s, gateway %s", s, host, panelDigest([]byte(s)))
		}
	}
	if panelDigest([]byte("● Needs permission\n")) != panelDigest([]byte("  Needs permission\n")) {
		t.Error("a tool's blinking mark counts")
	}
	if panelDigest([]byte("  a ● b\n")) == panelDigest([]byte("  a   b\n")) {
		t.Error("a mark among words does not count")
	}
	if panelDigest([]byte("\x1b[7m❯\x1b[0m a\n")) != panelDigest([]byte("❯ a\n")) {
		t.Error("colours count")
	}
	if panelDigest([]byte("❯ a\n  b\n")) == panelDigest([]byte("  a\n❯ b\n")) {
		t.Error("the choice does not count")
	}
}

// The digest an answer goes with: the gateway's and the host's the same, on a
// screen as read with no colours, whichever way its tool's row has blinked.
func TestScreenDigestSteady(t *testing.T) {
	for _, s := range []string{"plain\n", "no newline at its end", "with\x00nul\n", "● ⏺ two marks\n", "● Needs permission\n Do you want to proceed?\n ❯ 1. Yes\n", "  ⏺ row\n\twords ● stay\n", "中文\n\n"} {
		c := exec.Command("sh", "-c", steadyPipe+" | sha256sum")
		c.Stdin = strings.NewReader(s)
		out, err := c.Output()
		if err != nil {
			t.Fatalf("%q: %v", s, err)
		}
		_, gw, _ := screenOf([]byte(s))
		if host, _, _ := strings.Cut(string(out), " "); host != gw {
			t.Errorf("%q: host %s, gateway %s", s, host, gw)
		}
	}
	_, a, _ := screenOf([]byte("● Needs permission\n ❯ 1. Yes\n"))
	_, b, _ := screenOf([]byte("  Needs permission\n ❯ 1. Yes\n"))
	if a != b {
		t.Error("the screen's digest changes as its row blinks")
	}
	_, c, _ := screenOf([]byte("  Needs permission\n ❯ 2. No\n"))
	if a == c {
		t.Error("another choice is the same screen")
	}
}

// What names the terminal shown, on the gateway and the host alike; what moves
// by itself apart, what a key acts on not.
func TestLookDigest(t *testing.T) {
	for _, s := range []string{
		"plain\n", "no newline", "", "with\x00nul\n",
		"\x1b[1;38;5;75m✽ Nucleating… (23s)\x1b[0m\n❯ 1. Yes\n  2. No\n",
		"* Nucleating… (23s · ↓ 1.2k tokens)\n◯ main\n● general-purpose  Count the files      14s\n",
		"● ⏺ two\n  ● indented\nwords ● stay\n\ttabbed 12\n",
		"· not a spinner line\n✻ Worked for 3s\n",
		"中文 … 1234\n",
	} {
		c := exec.Command("sh", "-c", lookDigestPipe)
		c.Stdin = strings.NewReader(s)
		out, err := c.Output()
		if err != nil {
			t.Fatalf("%q: %v", s, err)
		}
		if host, _, _ := strings.Cut(string(out), " "); host != lookDigest([]byte(s)) {
			t.Errorf("%q: host %s, gateway %s", s, host, lookDigest([]byte(s)))
		}
	}
	same := [][2]string{
		{"✽ Nucleating… (23s)\nbox\n", "* Pondering… (24s · ↓ 3k tokens)\nbox\n"},
		{"  ● general-purpose  Count      9s\n", "  ● general-purpose  Count     10s\n"},
		{"● Needs permission\n", "  Needs permission\n"},
	}
	for _, c := range same {
		if lookDigest([]byte(c[0])) != lookDigest([]byte(c[1])) {
			t.Errorf("what moves by itself counts: %q, %q", c[0], c[1])
		}
	}
	other := [][2]string{
		{"❯ Auto-compact  true\n  Theme\n", "  Auto-compact  true\n❯ Theme\n"}, // (the cursor)
		{"❯ 1. Yes\n  2. No\n", "  1. Yes\n❯ 2. No\n"},                         // (the choice)
		{"❯ draft\n", "❯ another draft\n"},                                     // (a box's words)
		{"Delete staging?\n❯ 1. Yes\n", "Delete production?\n❯ 1. Yes\n"},      // (what is asked)
	}
	for _, c := range other {
		if lookDigest([]byte(c[0])) == lookDigest([]byte(c[1])) {
			t.Errorf("what a key acts on does not count: %q, %q", c[0], c[1])
		}
	}
}
