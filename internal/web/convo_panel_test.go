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
		"plain\n", "no newline", "123", "A\n123", "", "with\x00nul\n",
		"\x1b[1;38;5;75m✽ Nucleating… (23s)\x1b[0m\n❯ 1. Yes\n  2. No\n",
		"* Nucleating… (23s · ↓ 1.2k tokens)\n◯ main\n● general-purpose  Count the files      14s\n",
		"❯ ● general-purpose  Count the files      1m 5s\n  ◯ other x 2h\n",
		"● ⏺ two\n  ● indented\nwords ● stay\n\ttabbed 12\n",
		"· not a spinner line\n✻ Worked for 3s\n",
		"中文 … 1234\n",
		"✽ Nucleating… (3s)",
		"● Bash(kill -TERM 1234)\n",
		"● 5s\n",
		"● task\n  5s\n",
		"  ◯ main\n❯ ● general-purpose  Count the files  1m 5s\n\n\n",
		"  ● timeout 5s\n Esc to cancel\n",
		"  ● timeout 5s\n",
		"中文\n◯ main\n● 子代理 x 2h 3m 4s  \n",
		// (OpenCode 2's, as its screens are.)
		"  ┃  ⠼ touch needs-ok\n  ┃\n  ┃  △ Permission required\n  ┃   Allow once   Always allow   Reject\n",
		"\t┃\t⠋ x\n",
		"   ⬝⬝■■■■■■ esc interrupt                 1.0K (1%)\n",
		"■ not a bar\n",
		"  ┃  ⠼\n",
		"⠼ not after a bar of its\n",
	} {
		// (By every awk there is here: a host's may be any.)
		for _, awk := range []string{"awk", "mawk", "gawk", "busybox awk", "original-awk"} {
			if _, err := exec.LookPath(strings.Fields(awk)[0]); err != nil {
				continue
			}
			c := exec.Command("sh", "-c", strings.ReplaceAll(lookDigestPipe, "LC_ALL=C awk", "LC_ALL=C "+awk))
			c.Stdin = strings.NewReader(s)
			out, err := c.Output()
			if err != nil {
				t.Fatalf("%s, %q: %v", awk, s, err)
			}
			if host, _, _ := strings.Cut(string(out), " "); host != lookDigest([]byte(s)) {
				t.Errorf("%s, %q: host %s, gateway %s", awk, s, host, lookDigest([]byte(s)))
			}
		}
	}
	same := [][2]string{
		{"✽ Nucleating… (23s)\nbox\n", "* Pondering… (24s · ↓ 3k tokens)\nbox\n"},
		{"  ◯ main\n  ● general-purpose  Count      9s\n", "  ◯ main\n  ● general-purpose  Count     10s\n"},
		{"❯ ● general-purpose  Count 22s\n  ◯ main\n", "❯ ● general-purpose  Count 23s\n  ◯ main\n"},
		{"◯ main\n● task 59s\n\n", "◯ main\n● task 1m 0s\n\n"},
		{"● Needs permission\n", "  Needs permission\n"},
		{"  ┃  ⠼ touch needs-ok\n  ┃   Allow once\n", "  ┃  ⠙ touch needs-ok\n  ┃   Allow once\n"},
		{"   ⬝⬝■■■■■■ esc interrupt   1.0K\n", "   ■■⬝⬝■■■■ esc interrupt   1.0K\n"},
	}
	for _, c := range same {
		if lookDigest([]byte(c[0])) != lookDigest([]byte(c[1])) {
			t.Errorf("what moves by itself counts: %q, %q", c[0], c[1])
		}
	}
	other := [][2]string{
		{"❯ Auto-compact  true\n  Theme\n", "  Auto-compact  true\n❯ Theme\n"},
		{"❯ 1. Yes\n  2. No\n", "  1. Yes\n❯ 2. No\n"},
		{"❯ draft\n", "❯ another draft\n"},
		{"Delete staging?\n❯ 1. Yes\n", "Delete production?\n❯ 1. Yes\n"},
		{"kill -TERM 1234\n❯ 1. Yes\n", "kill -TERM 5678\n❯ 1. Yes\n"},
		{"echo a b\n", "echo ab\n"},
		{"❯ 1. release/123\n  2. release/124\n", "❯ 1. release/124\n  2. release/123\n"},
		{"● Bash(sleep 5s)\n", "● Bash(sleep 9s)  x\n"},
		// (Words of a prompt that look like an agent's row.)
		{"  ● timeout 5s\n ❯ 1. Yes\n Esc to cancel\n", "  ● timeout 9s\n ❯ 1. Yes\n Esc to cancel\n"},
		{"  ● timeout 5s\n", "  ● timeout 9s\n"},
		{"◯ main\n● task 5s\nmore\n", "◯ main\n● task 9s\nmore\n"},
		{"● task\n  5s\n", "● task\n  9s\n"},
		{"  ┃  ⠼ touch a\n", "  ┃  ⠼ touch b\n"},
		{"  ┃   Allow once   Always allow\n", "  ┃   Always allow   Allow once\n"},
		{"⠼ x\n", "⠙ x\n"},
	}
	for _, c := range other {
		if lookDigest([]byte(c[0])) == lookDigest([]byte(c[1])) {
			t.Errorf("what a key acts on does not count: %q, %q", c[0], c[1])
		}
	}
}
