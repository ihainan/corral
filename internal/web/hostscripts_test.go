package web

import (
	"os/exec"
	"strings"
	"testing"
)

// The scripts run on hosts parse, in the shell they are run with there
// ("sh -c '…'"): a quote too many in one of them breaks every part of it
// on every host, and nothing else would say so before a host did.
func TestHostScriptsParse(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh here")
	}
	cmds := map[string]string{
		"locate herdr":  convoLocateCommand("herdr", "", ""),
		"locate tmux":   convoLocateCommand("tmux", "main", "%1"),
		"screen":        convoScreenCommand("herdr", "w1:p1", "/tmp/herdr.sock"),
		"tails":         convoTailsCommand([]string{"/home/u/.claude/projects/-w/a.jsonl"}),
		"title":         convoTitleCommand("/home/u/.claude/projects/-w/a.jsonl", "11111111-2222-4333-8444-555555555501"),
		"complete":      completeCommand("/home/u/proj"),
		"opencode read": ocQueryCommand("/home/u/.local/share/opencode/opencode.db", "tail", "ses_x"),
	}
	for name, cmd := range cmds {
		// (Read, not run: "sh -n", a script's quoting undone by the shell
		// around it; a command line itself as it is.)
		check := exec.Command(sh, "-n", "-c", cmd)
		if rest, ok := strings.CutPrefix(cmd, "sh -c "); ok {
			check = exec.Command(sh, "-c", "sh -n -c "+rest)
		}
		if out, err := check.CombinedOutput(); err != nil {
			t.Errorf("%s: does not parse: %v %s", name, err, out)
		}
	}
}
