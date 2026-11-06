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
	if panelDigest([]byte("\x1b[7m❯\x1b[0m a\n")) != panelDigest([]byte("❯ a\n")) {
		t.Error("colours count")
	}
	if panelDigest([]byte("❯ a\n  b\n")) == panelDigest([]byte("  a\n❯ b\n")) {
		t.Error("the choice does not count")
	}
}
