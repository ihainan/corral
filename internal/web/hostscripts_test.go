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
		"locate herdr":              convoLocateCommand("herdr", "", ""),
		"locate pids":               convoLocatePidsCommand(50, []int{100, 200}),
		"locate tmux":               convoLocateCommand("tmux", "main", "%1"),
		"rename tmux":               tmuxRenameCommand("main", "work-2", "$0"),
		"rename tmux unbound":       tmuxRenameCommand("main", "work-2", ""),
		"tmux pane env":             tmuxPaneEnvironCommand("main", "$0", "%1"),
		"tmux pane env unbound":     tmuxPaneEnvironCommand("main", "", "%1"),
		"tmux pane inspect":         tmuxPaneInspectCommand("main", "$0", "%1"),
		"tmux pane inspect unbound": tmuxPaneInspectCommand("main", "", "%1"),
		"fresh status":              freshStatusCommand(123),
		"line alive":                lineAliveCommand(123, 456),
		"bg tail":                   bgTailCommand([]string{"11111111-2222-4333-8444-555555555501"}, 4096),
		"cmd scan":                  cmdScanCommand("w:11111111-2222-4333-8444-555555555501"),
		"fork find claude":          forkFindCommand("claude", "11111111-2222-4333-8444-555555555501"),
		"logs wrote claude":         logsWroteCommand([]string{"claude:11111111-2222-4333-8444-555555555501"}) + "; " + hostNowCommand,
		"gated herdr":               convoGatedCommand("free", "herdr", "", "w1:p1", "/tmp/herdr.sock", 100, 200, "11111111-2222-4333-8444-555555555501", 0, []string{"enter"}, 0),
		"rewind check herdr":        claudeRewindCheckSh("herdr", "w1:p1", "/tmp/herdr.sock", "", 100, 200, "11111111-2222-4333-8444-555555555501"),
		"side question":             sideQuestionCommand(100, 200, "11111111-2222-4333-8444-555555555501", "claude-opus-5-5", "f00d"),
		"tmux claude fork":          agentTmuxCmd("fork-1", "/tmp/work", "claude", "/tmp/my cfg/.claude-alt", agentStart{resume: "11111111-2222-4333-8444-555555555501"}),
		"screen":                    convoScreenCommand("herdr", "w1:p1", "/tmp/herdr.sock"),
		"screen joined":             convoScreenJoinedCommand("tmux", "%1", ""),
		"tails":                     convoTailsCommand([]string{"/home/u/.claude/projects/-w/a.jsonl"}),
		"title":                     convoTitleCommand("/home/u/.claude/projects/-w/a.jsonl", "11111111-2222-4333-8444-555555555501", 4096),
		"complete":                  completeCommand("/home/u/proj"),
		"opencode read":             ocQueryCommand("/home/u/.local/share/opencode/opencode.db", "tail", "ses_x"),
		"devin read":                dvQueryCommand("/home/u/.local/share/devin/cli/sessions.db", "tail", "x-y", "200"),
		"codex models cache":        codexModelsCacheCommand(123),
	}
	shells := []string{sh}
	if dash, err := exec.LookPath("dash"); err == nil && dash != sh {
		shells = append(shells, dash)
	}
	for name, cmd := range cmds {
		for _, bin := range shells {
			// (Read, not run: "sh -n", a script's quoting undone by the shell
			// around it; a command line itself as it is.)
			check := exec.Command(bin, "-n", "-c", cmd)
			if rest, ok := strings.CutPrefix(cmd, "sh -c "); ok {
				check = exec.Command(bin, "-c", bin+" -n -c "+rest)
			}
			if out, err := check.CombinedOutput(); err != nil {
				t.Errorf("%s (%s): does not parse: %v %s", name, bin, err, out)
			}
		}
	}
}
