package web

import (
	"testing"
	"time"

	"github.com/ihainan/corral/internal/convo"
)

func TestCodexRunGone(t *testing.T) {
	run := func(what string) convo.Running { return convo.Running{ID: "proc:1@x", Kind: "cmd", What: what} }
	srv := []bgCmd{{Task: "codex-11", Title: "python3 -m http.server 18799 --bind 127.0.0.1"}}
	for _, c := range []struct {
		name string
		r    convo.Running
		live []bgCmd
		n    int
		gone bool
	}{
		{"none of its left", run("sleep 45"), nil, 1, true},
		{"none left, unnamed", run(""), nil, 2, true},
		{"its own still there", run("python3 -m http.server 18799 --bind 127.0.0.1"), srv, 1, false},
		{"as many running as there: not told which", run("sleep 45"), srv, 1, false},
		{"one more running, this one not there", run("sleep 45"), srv, 2, true},
		{"one more, this one there", run("python3 -m http.server 18799 --bind 127.0.0.1"), srv, 2, false},
		{"one more, this one unnamed", run(""), srv, 2, false},
		{"one more, one there unnamed", run("sleep 45"), []bgCmd{{Task: "codex-12"}}, 2, false},
		{"its title cut short there", run("for i in $(seq 1 100); do echo tick $i; sleep 1; done"), []bgCmd{{Task: "codex-13", Title: "for i in $(seq 1 100); do echo…"}}, 2, false},
	} {
		if got := codexRunGone(c.r, c.live, c.n); got != c.gone {
			t.Errorf("%s: gone %v", c.name, got)
		}
	}
}

// What a look found is told for sure of a conversation a page reads only
// where it was at rest when the look began, the look recent; told of no
// longer once it is at work, or no page has read it a while.
func TestCmdProberReadSeen(t *testing.T) {
	const th = "01a1dd95-c55c-7270-ad56-1d53266a6fd0"
	p := newCmdProber(nil)
	if _, ok := p.readSeen(th); ok {
		t.Fatal("not read")
	}
	p.readAt(th, true)
	p.at, p.cmds = time.Now().Add(-time.Second), map[string][]bgCmd{}
	if _, ok := p.readSeen(th); ok {
		t.Error("a look begun before it came to rest")
	}
	p.at = time.Now()
	if live, ok := p.readSeen(th); !ok || len(live) != 0 {
		t.Errorf("none left: %v %v", live, ok)
	}
	p.readAt(th, true) // (read again, at rest still: as of when it came to rest)
	if _, ok := p.readSeen(th); !ok {
		t.Error("read again")
	}
	if _, ok := p.readSeen("01a1dd95-0000-7270-ad56-1d53266a6fd0"); ok {
		t.Error("another conversation's")
	}
	p.at = time.Now().Add(-p.kept - time.Second)
	if _, ok := p.readSeen(th); ok {
		t.Error("an old look")
	}
	p.at = time.Now()
	p.readAt(th, false)
	if _, ok := p.readSeen(th); ok {
		t.Error("at work: what it runs now is its turn's")
	}
	p.readAt("not an id", true)
	if len(p.read) != 0 {
		t.Error("not an id, looked for")
	}
}

// A host whose processes cannot be read (no /proc: no boot time told):
// not whole, which is not "none running".
func TestParseCmdScanNoProc(t *testing.T) {
	if _, whole := parseCmdScan("clock\t\t100\n"); whole {
		t.Error("no /proc: whole")
	}
	if _, whole := parseCmdScan("clock\t1794700000\t100\n"); !whole {
		t.Error("none running, /proc read: not whole")
	}
}

// A pane's Codex's thread for the commands it left running: told by its
// process (its log open, `codex resume <id>`), named or not; else (the
// daemon's: the one conversation of its folder begun since it started)
// only where named, its title that name, nothing else unexplained there.
func TestCodexThreadOf(t *testing.T) {
	const th = "01a1dd95-c55c-7270-ad56-1d53266a6fd0"
	look := func(got map[string]codexPane) *codexPaneLooks {
		d := make(chan struct{})
		close(d)
		return &codexPaneLooks{by: map[string]*codexPaneLook{"s": {at: time.Now(), done: d, got: got}}}
	}
	for _, c := range []struct {
		name  string
		pane  codexPane
		title string
		other *codexPane
		want  bool
	}{
		{"its log open, unnamed", codexPane{Thread: th, Cwd: "/w/proj", Src: "fd"}, "proj", nil, true},
		{"resumed at it, unnamed", codexPane{Thread: th, Cwd: "/w/proj", Src: "argv"}, "proj", nil, true},
		{"resumed at it, another talked in since (/new, /resume)", codexPane{Thread: th, Cwd: "/w/proj", Src: "argv", Unexplained: 1}, "proj", nil, false},
		{"the daemon's, unnamed", codexPane{Thread: th, Cwd: "/w/proj", Src: "db"}, "proj", nil, false},
		{"the daemon's, named, its title", codexPane{Thread: th, Cwd: "/w/proj", Src: "db", Name: "fix tests"}, "fix tests | proj", nil, true},
		{"the daemon's, named, another title", codexPane{Thread: th, Cwd: "/w/proj", Src: "db", Name: "fix tests"}, "proj", nil, false},
		{"the daemon's, named, one unexplained", codexPane{Thread: th, Cwd: "/w/proj", Src: "db", Name: "fix tests", Unexplained: 1}, "fix tests | proj", nil, false},
		{"its log open, another pane at it too", codexPane{Thread: th, Cwd: "/w/proj", Src: "fd"}, "proj", &codexPane{Thread: th, Cwd: "/w/proj", Src: "db"}, false},
		{"its log open, two Codexes in the pane", codexPane{Thread: th, Cwd: "/w/proj", Src: "fd", Several: true}, "proj", nil, false},
	} {
		got := map[string]codexPane{"w1:p1": c.pane}
		if c.other != nil {
			got["w1:p2"] = *c.other
		}
		thread, _ := look(got).threadOf("s", "w1:p1", c.title)
		if (thread == th) != c.want {
			t.Errorf("%s: %q", c.name, thread)
		}
	}
}
