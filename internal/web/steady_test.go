package web

import (
	"bytes"
	"os/exec"
	"strings"
	"testing"
)

// The countdown under a permission prompt is no change of its screen: two
// reads a second apart name the same screen, here and on the host alike
// (steady and steadyAwk, to the byte, by every awk found here); anything
// else changed still does.
func TestSteadyScreen(t *testing.T) {
	prompt := func(left, tail string) string {
		return "● Rebuilding the upstream workspace packages\n" +
			"────────────────────────────────────────\n" +
			" Bash command\n" +
			"   Rebuild the upstream workspace packages\n" +
			"   ╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌\n" +
			"   │ cd ~/shared/Projects && npm run build\n" +
			"   ╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌\n" +
			"   │ Dangerous rm operation not ruled out\n" +
			"   ⚠ Claude Code will automatically block this request in " + left + ", to avoid holding up an\n" +
			tail +
			"\n" +
			" Do you want to proceed?\n" +
			" ❯ 1. Yes\n" +
			"   2. No\n" +
			"\n" +
			" Esc to cancel · Tab to amend"
	}
	a := prompt("1:09", "   unattended session\n")
	b := prompt("0:54", "   unattended session\n")
	c := prompt("10:00", "   unattended\n   session\n") // (a wider figure: wrapped elsewhere)
	_, da, _ := screenOf([]byte(a))
	_, db, _ := screenOf([]byte(b))
	_, dc, _ := screenOf([]byte(c))
	if da != db || da != dc {
		t.Errorf("the countdown changed the digest: %s %s %s", da, db, dc)
	}
	if strings.Contains(string(steady([]byte(a))), "unattended") || !strings.Contains(string(steady([]byte(a))), "Do you want to proceed?") {
		t.Errorf("steady: %q", steady([]byte(a)))
	}
	// (Anything else changed: another screen.)
	_, dd, _ := screenOf([]byte(strings.Replace(a, "npm run build", "npm run test", 1)))
	_, de, _ := screenOf([]byte(strings.Replace(a, "❯ 1. Yes\n   2. No", "  1. Yes\n ❯ 2. No", 1)))
	if dd == da || de == da {
		t.Errorf("a change of the command or the cursor went unseen")
	}
	// (No countdown: as read, line by line.)
	plain := "a\n\nb  \n"
	if got := string(steady([]byte(plain))); got != plain {
		t.Errorf("plain: %q", got)
	}
	// The host's awk, the same bytes out.
	var awks []string
	for _, a := range []string{"awk", "mawk", "gawk", "original-awk"} {
		if p, err := exec.LookPath(a); err == nil {
			awks = append(awks, p)
		}
	}
	if p, err := exec.LookPath("busybox"); err == nil {
		awks = append(awks, p+" awk")
	}
	if len(awks) == 0 {
		t.Skip("no awk here")
	}
	for _, aw := range awks {
		for _, in := range []string{a, b, c, a + "\n", plain, "no newline at the end", "", "x\n   ⚠ Claude Code will automatically block this request in 0:05, to avoid\n   holding\n"} {
			args := append(strings.Fields(aw)[1:], steadyAwk)
			cmd := exec.Command(strings.Fields(aw)[0], args...)
			cmd.Stdin = strings.NewReader(in)
			out, err := cmd.Output()
			if err != nil {
				t.Fatalf("%s: %v", aw, err)
			}
			if !bytes.Equal(out, steady([]byte(in))) {
				t.Errorf("%s on %.40q: awk %q, steady %q", aw, in, out, steady([]byte(in)))
			}
		}
	}
}
