package web

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func cardSample(t *testing.T, path ...string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(append([]string{os.Getenv("HOME"), "tmp/corral-devtools"}, path...)...))
	if err != nil {
		t.Skip("sample not here:", path)
	}
	return string(b)
}

func collie(t *testing.T, name string) string {
	return cardSample(t, "research/src/collie/web/src/fixtures/panes", name)
}

// Claude Code's cards above its input box, read off real screens (ours, and
// collie's of 2.1.302/2.1.304).
func TestCardOf(t *testing.T) {
	c := cardOf(cardSample(t, "ccmock/samples/headsup-cjk.txt"))
	if c == nil || c.State != "offer" || c.Tag != "Heads up" || !c.Actionable || !c.Empty || !strings.HasPrefix(c.Text, "「看过的不再冒出来」这个修复要靠在主机上找到智能体的对话日志才能判断；找不到日志的智能体，比如部分 Codex会话") {
		t.Fatalf("cjk: %+v", c)
	}
	if len(c.Choices) != 3 || c.Choices[0] != (cardChoice{"1", "Learn more"}) || c.Choices[2] != (cardChoice{"0", "Dismiss"}) {
		t.Fatalf("cjk choices: %+v", c.Choices)
	}
	e := cardOf(cardSample(t, "ccmock/samples/headsup-en.txt"))
	if e == nil || e.State != "offer" || e.Text != "The main agent has already built in Ctrl+K for searching agents, and on Windows and Linux that takes over the browser's own Ctrl+K." {
		t.Fatalf("en (while it works): %+v", e)
	}
	if x := cardOf(collie(t, "claude--v2304-heads-up-explained--w135.txt")); x == nil || x.State != "explained" || x.Tag != "Heads up" || !strings.Contains(x.Text, "\n\n") {
		t.Fatalf("explained: %+v", x)
	}
	if x := cardOf(collie(t, "claude--v2304-heads-up-explained--w65.txt")); x == nil || x.State != "explained" || !x.Empty {
		t.Fatalf("explained, narrow, a dim suggestion in the box: %+v", x)
	}
	if d := cardOf(collie(t, "claude--v2304-heads-up-dismissed--w135.txt")); d == nil || d.State != "other" || len(d.Choices) < 3 || d.Choices[0].Key != "1" {
		t.Fatalf("dismissed, its keys on its head: %+v", d)
	}
	if d := cardOf(collie(t, "claude--v2304-heads-up-dismissed--w65.txt")); d == nil || d.State != "other" || len(d.Choices) < 3 {
		t.Fatalf("dismissed, narrow: %+v", d)
	}
	if a := cardOf(collie(t, "claude--v2302-heads-up-above-box--w142.txt")); a == nil || a.State != "offer" {
		t.Fatalf("above the box (2.1.302): %+v", a)
	}

	rule := strings.Repeat("─", 80)
	box := "\n" + rule + "\n❯ \n" + rule + "\n  status\n"
	for name, screen := range map[string]string{
		"quoted in the thread":          "● It said:\n✦ Heads up · something\n  1: Learn more   2: Knew this already   0: Dismiss\n\n● Then I went on.\n" + box,
		"quoted at its end, set in":     "● It said:\n  ✦ Heads up · something\n  1: Learn more   2: Knew this already   0: Dismiss" + box,
		"no keys":                       "✦ Heads up · something\n  more of it" + box,
		"thread under the card":         "✦ Heads up · something\n  1: Learn more   0: Dismiss\n● after it" + box,
		"a menu under the box":          "✦ Heads up · something\n  1: Learn more   0: Dismiss" + box + " 1. Yes\n",
		"the box not at the foot":       "✦ Heads up · something\n  1: Learn more   0: Dismiss" + box + strings.Repeat("x\n", 12),
		"its head off the screen above": strings.Repeat("  more words\n", 70) + "  1: Learn more   0: Dismiss" + box,
	} {
		if got := cardOf(screen); got != nil {
			t.Errorf("%s: %+v", name, got)
		}
	}
	for name, c := range map[string]struct {
		screen     string
		state      string
		actionable bool
		keys       int
	}{
		"being made":                 {"✦ One moment…" + box, "making", false, 0},
		"folded in the terminal":     {"                                        [+]\n✦ Heads up · something" + box, "folded", false, 0},
		"failed, its OK on its head": {"✦ Couldn’t write that explanation · 0: OK" + box, "failed", true, 1},
		"a key's words wrapped":      {"✦ You should know · Needs the log\n\n  Some words.\n\n  1: Understood   2: Chat in main\n  session\n  0: Dismiss" + box, "explained", false, 3},
		"keys on its head wrapped":   {"✦ Dismissed.   1: That was helpful   2: Not\n  relevant\n  0: Dismiss" + box, "other", false, 3},
		"keys over two lines":        {"✦ Heads up · words\n  1: Learn more   2: Knew this already\n  0: Dismiss" + box, "offer", true, 3},
		// (Per review407: cut at the screen's edge with no "…": not to be pressed.)
		"a key cut at the edge": {"✦ You should know · Needs the log\n  1: Understood   2: Chat in main s\n" + strings.Repeat("─", 35) + "\n❯ \n" + strings.Repeat("─", 35) + "\n", "explained", false, 2},
	} {
		got := cardOf(c.screen)
		if got == nil || got.State != c.state || got.Actionable != c.actionable || len(got.Choices) != c.keys {
			t.Errorf("%s: %+v", name, got)
		}
	}
	// The box: empty, a dim suggestion (empty), typed words (not).
	head := "✦ Heads up · words\n  1: Learn more   0: Dismiss\n" + rule + "\n"
	if c := cardOf(head + "❯ \x1b[2mtry this\x1b[0m\n" + rule + "\n"); c == nil || !c.Empty {
		t.Fatalf("a dim suggestion: %+v", c)
	}
	if c := cardOf(head + "❯ my draft\n" + rule + "\n"); c == nil || c.Empty {
		t.Fatalf("a draft: %+v", c)
	}
}

// The host takes a card's region as the gateway does (the same screen, the
// same digest), with the awk there is.
func TestCardRegionAwk(t *testing.T) {
	if _, err := exec.LookPath("awk"); err != nil {
		t.Skip("no awk")
	}
	for _, screen := range []string{
		cardSample(t, "ccmock/samples/headsup-cjk.txt"),
		// (As much under the box as may be: found on both.)
		"x\n✦ Heads up · words\n  1: Learn more   0: Dismiss\n" + strings.Repeat("─", 30) + "\n❯ \n" + strings.Repeat("─", 30) + "\n" + strings.Repeat("  status\n", 10),
		collie(t, "claude--v2304-heads-up-explained--w65.txt"),
		"x\n\x1b[38;2;177;185;249m✦ \x1b[0mHeads up · words  \n  1: Learn more   0: Dismiss\n" + strings.Repeat("─", 30) + "\n❯ \x1b[2mtry\x1b[0m   \n" + strings.Repeat("─", 30) + "\n  status\n",
	} {
		c := cardOf(screen)
		if c == nil {
			t.Fatal("no card")
		}
		cmd := exec.Command("awk", cardRegionAwk)
		cmd.Stdin = strings.NewReader(screen)
		out, err := cmd.Output()
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(out)
		if got := hex.EncodeToString(sum[:]); got != c.digest {
			t.Fatalf("awk %s, Go %s:\n%q", got[:12], c.digest[:12], out)
		}
	} // Where Go finds no card the host finds none either (review407: a menu
	// under the box, its key let through on the host).
	card := "x\n✦ Heads up · words\n  1: Learn more   0: Dismiss\n" + strings.Repeat("─", 30) + "\n❯ \n" + strings.Repeat("─", 30) + "\n"
	for name, screen := range map[string]string{
		"a menu under the box":      card + "  1. Menu choice\n",
		"another box under it":      card + "❯ more\n",
		"too much under the box":    card + strings.Repeat("  status\n", 11),
		"a line of the thread over": "x\n✦ Heads up · words\nThe thread\n  1: Learn more   0: Dismiss\n" + strings.Repeat("─", 30) + "\n❯ \n" + strings.Repeat("─", 30) + "\n",
	} {
		if cardOf(screen) != nil {
			t.Fatalf("%s: Go found a card", name)
		}
		cmd := exec.Command("awk", cardRegionAwk)
		cmd.Stdin = strings.NewReader(screen)
		out, err := cmd.Output()
		if err != nil || len(out) != 0 {
			t.Fatalf("%s: awk gave %q (%v)", name, out, err)
		}
	}
}
