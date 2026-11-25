package web

import (
	"context"
	"testing"
	"time"
)

// A subagent sent off in the background, not finished, of a Claude Code that
// marks them finished: told, the latest first; one finished, one sent in
// front, one of an older Claude Code, one of a bad id or session: not.
func TestParseBgSubScan(t *testing.T) {
	const other = "11111111-2222-4333-8444-555555555555"
	out := "clock\t1700000000\t100\n" +
		"sub\t" + cmdSess + "\taaaaaaaa11\t1700000100\t2.1.331\t{\"description\":\"Web research\",\"requestShape\":\"background\"}\n" +
		"sub\t" + cmdSess + "\tbbbbbbbb22\t1700000200\t2.1.302\t{\"description\":\"核对 402 错误码\\u202e\",\"requestShape\":\"background\"}\n" +
		"sub\t" + cmdSess + "\tcccccccc33\t1700000300\t2.1.331\t{\"description\":\"over\",\"requestShape\":\"background\",\"finished\":true}\n" +
		"sub\t" + cmdSess + "\tdddddddd44\t1700000400\t2.1.331\t{\"description\":\"in front\",\"requestShape\":\"foreground\"}\n" +
		"sub\t" + other + "\teeeeeeee55\t1700000500\t2.1.300\t{\"description\":\"old\",\"requestShape\":\"background\"}\n" +
		"sub\t" + other + "\tBAD/ID\t1700000500\t2.1.331\t{\"description\":\"bad\",\"requestShape\":\"background\"}\n" +
		"sub\tnot-a-session\tffffffff66\t1700000500\t2.1.331\t{\"description\":\"bad\",\"requestShape\":\"background\"}\n" +
		"sub\t" + cmdSess + "\taaaaaaaa11\t1700000100\t2.1.331\t{\"description\":\"Web research\",\"requestShape\":\"background\"}\n"
	got := parseBgSubScan(out)
	l := got[cmdSess]
	if len(got) != 1 || len(l) != 2 || l[0].ID != "bbbbbbbb22" || l[0].What != "核对 402 错误码" || l[0].Since != 1700000200000 || l[1].ID != "aaaaaaaa11" || l[1].What != "Web research" {
		t.Fatalf("got %+v", got)
	}
}

func TestVersionAtLeast(t *testing.T) {
	for v, want := range map[string]bool{"2.1.302": true, "2.1.331": true, "2.2.0": true, "3.0.0": true, "2.1.301": false, "2.0.999": false, "": false, "2.1": false, "2.1.x": false} {
		if versionAtLeast(v, subMinVersion) != want {
			t.Errorf("%q: want %v", v, want)
		}
	}
}

// As the turn record says: none out, none listed; n out, the latest n; at work, all.
func TestSubsShown(t *testing.T) {
	found := []bgAgent{{ID: "c"}, {ID: "b"}, {ID: "a"}}
	if l := subsShown(found, "done", bgNone, 0); l != nil {
		t.Errorf("none out: %+v", l)
	}
	if l := subsShown(found, "idle", bgRunning, 2); len(l) != 2 || l[0].ID != "c" {
		t.Errorf("two out: %+v", l)
	}
	if l := subsShown(found, "working", bgNone, 0); len(l) != 3 {
		t.Errorf("at work: %+v", l)
	}
	if l := subsShown(found, "done", bgChecking, 0); len(l) != 3 {
		t.Errorf("checking: %+v", l)
	}
}

func TestParseBgIDs(t *testing.T) {
	got := parseBgIDs("bgid\t" + cmdSess + "\tb5kvj0szm\nbgid\t" + cmdSess + "\tbad id\nbgid\tnope\tb1\nclock\t1\t2\n")
	if len(got) != 1 || len(got[cmdSess]) != 1 || !got[cmdSess]["b5kvj0szm"] {
		t.Fatalf("got %+v", got)
	}
}

// At work: only the commands its log says it set going in the background (and a
// Monitor's) are told, with its subagents; the working sessions are what the
// look is given. Done: all its commands, as before.
func TestCmdProberAtWork(t *testing.T) {
	asked := make(chan []string, 8)
	p := newCmdSubProber(func(_ context.Context, working []string) (map[string][]bgCmd, map[string][]bgAgent, map[string]map[string]bool, bool, error) {
		asked <- working
		return map[string][]bgCmd{cmdSess: {{Task: "bfront"}, {Task: "bback"}, {Task: "monitor-9", quiet: true}}},
			map[string][]bgAgent{cmdSess: {{ID: "aaaaaaaa11", What: "research"}}},
			map[string]map[string]bool{cmdSess: {"bback": true}}, true, nil
	})
	p.every = 50 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go p.run(ctx)
	sum := func(status, bg string, n int) herdrAgent {
		s := herdrSummary{Workspaces: []herdrWorkspace{{Agents: []herdrAgent{{PaneID: "w1:p1", Agent: "claude", Status: status, Session: cmdSess, Bg: bg, BgN: n}}}}}
		p.decorate(&s)
		return s.Workspaces[0].Agents[0]
	}
	p.consider("w1:p1", "claude", "working", cmdSess)
	if w := <-asked; len(w) != 1 || w[0] != cmdSess {
		t.Fatalf("asked of %v", w)
	}
	<-asked // (one begun after it came to work)
	a := sum("working", "", 0)
	if a.BgCmds != 2 || a.BgCmdList[0].Task != "bback" || a.BgCmdList[1].Task != "monitor-9" || len(a.BgAgentList) != 1 {
		t.Fatalf("at work: %+v", a)
	}
	p.consider("w1:p1", "claude", "done", cmdSess)
	for w := <-asked; len(w) != 0; w = <-asked {
	}
	<-asked
	if a := sum("done", bgNone, 0); a.BgCmds != 3 || len(a.BgAgentList) != 0 {
		t.Fatalf("done, none out: %+v", a)
	}
	if a := sum("done", bgRunning, 1); len(a.BgAgentList) != 1 {
		t.Fatalf("done, one out: %+v", a)
	}
}
