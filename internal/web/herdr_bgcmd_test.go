package web

import (
	"context"
	"errors"
	"os/exec"
	"sync/atomic"
	"testing"
	"time"
)

const cmdSess = "8251e81c-b1f1-4d4a-8f3f-bb630ba7bcf1"

// What the scan printed: each session's commands, one per task (its
// earliest process), in the order begun, their titles as Claude Code's
// shell runs them; what is not a task's output, or a session's, left out.
func TestParseCmdScan(t *testing.T) {
	dir := "/tmp/claude-1000/-home-u-proj/" + cmdSess + "/tasks/"
	out := "clock\t1700000000\t100\n" +
		"cmd\t11\t5000\t" + dir + "b2.output\tcd ~/w && sh suite.sh > log 2>&1' < /dev/null && pwd -P >| /tmp/claude-1-cwd\n" +
		"cmd\t10\t4000\t" + dir + "b1.output (deleted)\tnpm run dev -- --port '\"'\"'3000'\"'\"'' < /dev/null && pwd -P >| /tmp/x\n" +
		"cmd\t12\t4001\t" + dir + "b1.output\tsleep 9' < /dev/null\n" + // (the task's shell's child)
		"cmd\t13\t4500\t" + dir + "b3.output\tcat > f <<'EOF' a long one cut short\n" +
		"cmd\t14\t4600\t/tmp/claude-1000/x/not-a-session/tasks/b4.output\techo\n" +
		"cmd\t15\t4700\t/home/u/log.txt\techo\n" +
		"cmd\t16\tx\t" + dir + "b5.output\techo\n"
	got, whole := parseCmdScan(out)
	c := got[cmdSess]
	if !whole {
		t.Error("not whole")
	}
	if _, whole := parseCmdScan(out + "more\n"); whole {
		t.Error("cut short, yet whole")
	}
	if len(got) != 1 || len(c) != 3 {
		t.Fatalf("got %+v", got)
	}
	if c[0].Task != "b1" || c[0].Title != "npm run dev -- --port '3000'" || c[0].Since != 1_700_000_000_000+40_000 {
		t.Errorf("first: %+v", c[0])
	}
	if c[1].Task != "b3" || c[1].Title != "cat > f <<'EOF' a long one cut short" || c[2].Task != "b2" || c[2].Title != "cd ~/w && sh suite.sh > log 2>&1" {
		t.Errorf("then: %+v", c[1:])
	}
}

// Told only for a Claude Code agent done or idle, of the session it is at,
// while what was found is recent; a look that fails tells nothing.
func TestCmdProberTells(t *testing.T) {
	var fail atomic.Bool
	scans := make(chan struct{}, 8)
	p := newCmdProber(func(context.Context) (map[string][]bgCmd, bool, error) {
		defer func() { scans <- struct{}{} }()
		if fail.Load() {
			return nil, false, errors.New("no")
		}
		return map[string][]bgCmd{cmdSess: {{Task: "b1", Title: "a"}, {Task: "b2"}, {Task: "b3"}, {Task: "b4"}}}, true, nil
	})
	p.every = 50 * time.Millisecond
	poked := make(chan struct{}, 8)
	p.setPoke(func() { poked <- struct{}{} })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go p.run(ctx)
	sum := func() herdrAgent {
		s := herdrSummary{Workspaces: []herdrWorkspace{{Agents: []herdrAgent{{PaneID: "w1:p1", Agent: "claude", Session: cmdSess}}}}}
		p.decorate(&s)
		return s.Workspaces[0].Agents[0]
	}
	// Working: not looked at, nothing told.
	p.consider("w1:p1", "claude", "working", cmdSess)
	select {
	case <-scans:
		t.Fatal("looked at while working")
	case <-time.After(150 * time.Millisecond):
	}
	p.consider("w1:p1", "claude", "done", cmdSess)
	<-scans
	<-poked
	if a := sum(); a.BgCmds != 4 || len(a.BgCmdList) != maxCmdsShown || a.BgCmdList[0].Title != "a" {
		t.Fatalf("told %d %+v", a.BgCmds, a.BgCmdList)
	}
	// Another session in the pane (as herdr tells it): not its.
	s := herdrSummary{Workspaces: []herdrWorkspace{{Agents: []herdrAgent{{PaneID: "w1:p1", Agent: "claude", Session: "11111111-2222-4333-8444-555555555555"}}}}}
	p.decorate(&s)
	if s.Workspaces[0].Agents[0].BgCmds != 0 {
		t.Error("told of another session's")
	}
	// A look fails: nothing told.
	fail.Store(true)
	<-scans
	<-poked
	if a := sum(); a.BgCmds != 0 {
		t.Errorf("told after a look failed: %d", a.BgCmds)
	}
	// It works again: not told (no longer done or idle).
	p.consider("w1:p1", "claude", "working", cmdSess)
	fail.Store(false)
	if a := sum(); a.BgCmds != 0 {
		t.Errorf("told while working: %d", a.BgCmds)
	}
}

// The command, from the shell's first eval (the one Claude Code's wrapper
// runs it with), though it holds an eval of its own.
func TestCmdTitleFromTheFirstEval(t *testing.T) {
	line := `source snap 2>/dev/null || true && eval 'echo setup && eval '"'"'sleep 30'"'"'' < /dev/null && pwd -P >| /tmp/x`
	out, err := exec.Command("sh", "-c", `printf '%s\n' "$1" | awk -v "m=&& eval '" '`+cmdEvalAwk+`'`, "t", line).Output()
	if err != nil {
		t.Fatal(err)
	}
	if got := cmdTitle(string(out)); got != "echo setup && eval 'sleep 30'" {
		t.Errorf("title %q", got)
	}
}

// What a look begun before the pane came to done or idle found is not
// told of it (it may be what it ran in front then).
func TestCmdProberNotOfBefore(t *testing.T) {
	p := newCmdProber(nil)
	p.at, p.cmds = time.Now().Add(-time.Second), map[string][]bgCmd{cmdSess: {{Task: "b1"}}}
	p.consider("w1:p1", "claude", "done", cmdSess)
	s := herdrSummary{Workspaces: []herdrWorkspace{{Agents: []herdrAgent{{PaneID: "w1:p1", Agent: "claude", Session: cmdSess}}}}}
	p.decorate(&s)
	if s.Workspaces[0].Agents[0].BgCmds != 0 {
		t.Error("told what a look begun before it was done found")
	}
}

// The same found again, now of a pane come to done since the last look
// began: told (the summary goes again).
func TestCmdProberTellsOncePastItsTurn(t *testing.T) {
	same := map[string][]bgCmd{cmdSess: {{Task: "b1"}}}
	p := newCmdProber(func(context.Context) (map[string][]bgCmd, bool, error) { return same, true, nil })
	p.soonBy = 30 * time.Millisecond
	p.at, p.cmds = time.Now(), same
	poked := make(chan struct{}, 4)
	p.setPoke(func() { poked <- struct{}{} })
	time.Sleep(5 * time.Millisecond)
	p.consider("w1:p1", "claude", "done", cmdSess)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go p.run(ctx)
	select {
	case <-poked:
	case <-time.After(2 * time.Second):
		t.Fatal("not told")
	}
}
