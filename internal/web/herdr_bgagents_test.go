package web

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// What the script tells of a session's subagents (its "sub" lines): of a Claude Code that marks
// them finished, the latest sent first, what each was sent for; one of an older Claude Code, of
// a bad id or session: not. (Those finished, sent in front, or long untouched the script leaves
// out: TestCmdScanSubagents.)
func TestParseBgSubScan(t *testing.T) {
	const other = "11111111-2222-4333-8444-555555555555"
	out := "clock\t1700000000\t100\n" +
		"sub\t" + cmdSess + "\taaaaaaaa11\t1700000100\t2.1.331\t{\"description\":\"Web research\"}\n" +
		"sub\t" + cmdSess + "\tbbbbbbbb22\t1700000200\t2.1.302\t{\"description\":\"核对 402 错误码\\u202e\"}\n" +
		"sub\t" + cmdSess + "\tcccccccc33\t1700000050\t2.1.331\t{\"description\":\"cut sho\n" +
		"sub\t" + other + "\teeeeeeee55\t1700000500\t2.1.300\t{\"description\":\"old\"}\n" +
		"sub\t" + other + "\tBAD/ID\t1700000500\t2.1.331\t{\"description\":\"bad\"}\n" +
		"sub\tnot-a-session\tffffffff66\t1700000500\t2.1.331\t{\"description\":\"bad\"}\n" +
		"sub\t" + cmdSess + "\taaaaaaaa11\t1700000100\t2.1.331\t{\"description\":\"Web research\"}\n"
	got := parseBgSubScan(out)
	l := got[cmdSess]
	if len(got) != 1 || len(l) != 3 || l[0].ID != "bbbbbbbb22" || l[0].What != "核对 402 错误码" || l[0].Since != 1700000200000 || l[1].ID != "aaaaaaaa11" || l[1].What != "Web research" || l[2].ID != "cccccccc33" || l[2].What != "" {
		t.Fatalf("got %+v", got)
	}
}

// The script itself, on a home of its own: of the sessions given, the subagents sent off in the
// background, not finished, whose log was written to in the last six hours — not one finished,
// sent in front, long untouched, nor of a session not given; what each was sent for, quotes and all.
func TestCmdScanSubagents(t *testing.T) {
	const other = "11111111-2222-4333-8444-555555555555"
	home := t.TempDir()
	sub := func(session, id, meta string, old bool) {
		d := filepath.Join(home, ".claude", "projects", "-home-u-proj", session, "subagents")
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		j := filepath.Join(d, "agent-"+id+".jsonl")
		if err := os.WriteFile(j, []byte(`{"agentId":"`+id+`","version":"2.1.331"}`+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, "agent-"+id+".meta.json"), []byte(meta), 0o644); err != nil {
			t.Fatal(err)
		}
		if old {
			at := time.Now().Add(-7 * time.Hour)
			_ = os.Chtimes(j, at, at)
		}
	}
	sub(cmdSess, "a1live00001", `{"agentType":"general-purpose","description":"查 \"缓存\" 字段","requestShape":"background"}`, false)
	sub(cmdSess, "a2done00002", `{"description":"over","requestShape":"background","finished":true}`, false)
	sub(cmdSess, "a3front0003", `{"description":"in front","requestShape":"foreground"}`, false)
	sub(cmdSess, "a4old000004", `{"description":"long gone","requestShape":"background"}`, true)
	sub(other, "a5other0005", `{"description":"not given","requestShape":"background"}`, false)
	cmd := exec.Command("sh", "-c", cmdScanCommand("s:"+cmdSess, "x:"+other, "s:not-a-uuid"))
	cmd.Env = append(os.Environ(), "HOME="+home)
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	got := parseBgSubScan(string(out))
	if l := got[cmdSess]; len(got) != 1 || len(l) != 1 || l[0].ID != "a1live00001" || l[0].What != `查 "缓存" 字段` || l[0].Since == 0 {
		t.Fatalf("got %+v\n%s", got, out)
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
	p := newCmdSubProber(func(_ context.Context, watched []string) (map[string][]bgCmd, map[string][]bgAgent, map[string]map[string]bool, bool, error) {
		asked <- watched
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
	if w := <-asked; len(w) != 1 || w[0] != "w:"+cmdSess {
		t.Fatalf("asked of %v", w)
	}
	<-asked // (one begun after it came to work)
	a := sum("working", "", 0)
	if a.BgCmds != 2 || a.BgCmdList[0].Task != "bback" || a.BgCmdList[1].Task != "monitor-9" || len(a.BgAgentList) != 1 {
		t.Fatalf("at work: %+v", a)
	}
	p.consider("w1:p1", "claude", "done", cmdSess)
	for w := <-asked; len(w) != 1 || w[0] != "s:"+cmdSess; w = <-asked {
	}
	<-asked
	if a := sum("done", bgNone, 0); a.BgCmds != 3 || len(a.BgAgentList) != 0 {
		t.Fatalf("done, none out: %+v", a)
	}
	if a := sum("done", bgRunning, 1); len(a.BgAgentList) != 1 {
		t.Fatalf("done, one out: %+v", a)
	}
}

// nudge wakes cmdProber within soonBy even while an agent stays working,
// instead of waiting a full every (1 minute) interval.
func TestCmdProberNudgeWhileWorking(t *testing.T) {
	scans := make(chan struct{}, 8)
	p := newCmdSubProber(func(_ context.Context, _ []string) (map[string][]bgCmd, map[string][]bgAgent, map[string]map[string]bool, bool, error) {
		scans <- struct{}{}
		return nil, map[string][]bgAgent{cmdSess: {{ID: "aaaaaaaa11", What: "Survey cluster"}}}, nil, true, nil
	})
	p.every = 5 * time.Second
	p.soonBy = 20 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go p.run(ctx)

	p.consider("w1:p1", "claude", "working", cmdSess)
	select {
	case <-scans:
	case <-time.After(time.Second):
		t.Fatal("initial scan did not run")
	}
	// Agent stays working and launches a background subagent: nudge triggers
	// a scan well before p.every (5s).
	p.nudge()
	select {
	case <-scans:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("nudge did not trigger early scan while working")
	}
}

// Transitioning between working and done/idle for the same session keeps
// already-discovered background subagents visible without a 5s gap.
func TestCmdProberSubsAcrossTransition(t *testing.T) {
	p := newCmdSubProber(nil)
	p.at = time.Now()
	p.subs = map[string][]bgAgent{cmdSess: {{ID: "aaaaaaaa11", What: "Survey cluster"}}}
	p.consider("w1:p1", "claude", "working", cmdSess)
	time.Sleep(5 * time.Millisecond)
	p.consider("w1:p1", "claude", "done", cmdSess)
	s := herdrSummary{Workspaces: []herdrWorkspace{{Agents: []herdrAgent{{PaneID: "w1:p1", Agent: "claude", Status: "done", Session: cmdSess, Bg: bgRunning, BgN: 1}}}}}
	p.decorate(&s)
	if l := s.Workspaces[0].Agents[0].BgAgentList; len(l) != 1 || l[0].ID != "aaaaaaaa11" {
		t.Fatalf("subagent lost across working->done transition: %+v", l)
	}
}
