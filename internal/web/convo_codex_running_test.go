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

// What a look found is told for sure only of the pane's own session, begun
// after it came to done or idle, recent and whole.
func TestCmdProberSeen(t *testing.T) {
	const th = "01a1dd95-c55c-7270-ad56-1d53266a6fd0"
	p := newCmdProber(nil)
	if _, ok := p.seen("w1:p1", th); ok {
		t.Fatal("no look yet")
	}
	p.consider("w1:p1", "codex", "idle", th)
	p.at, p.cmds = time.Now().Add(-time.Second), map[string][]bgCmd{}
	if _, ok := p.seen("w1:p1", th); ok {
		t.Error("a look begun before it came to idle")
	}
	p.at = time.Now()
	if live, ok := p.seen("w1:p1", th); !ok || len(live) != 0 {
		t.Errorf("none left: %v %v", live, ok)
	}
	if _, ok := p.seen("w1:p1", "01a1dd95-0000-7270-ad56-1d53266a6fd0"); ok {
		t.Error("another conversation's")
	}
	if _, ok := p.seen("w1:p2", th); ok {
		t.Error("another pane's")
	}
	p.at = time.Now().Add(-p.kept - time.Second)
	if _, ok := p.seen("w1:p1", th); ok {
		t.Error("an old look")
	}
	p.consider("w1:p1", "codex", "working", th)
	p.at = time.Now()
	if _, ok := p.seen("w1:p1", th); ok {
		t.Error("at work: what it runs now is its turn's")
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
