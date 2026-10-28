package web

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ihainan/corral/internal/herdr"
)

const (
	bgSid1 = "11111111-2222-4333-8444-555555555551"
	bgSid2 = "11111111-2222-4333-8444-555555555552"
)

func TestParseBgTails(t *testing.T) {
	out := strings.Join([]string{
		"",
		"@@corral-bg " + bgSid1,
		`id":"cut at the start of the tail"}`,
		`{"type":"system","subtype":"turn_duration","uuid":"t1","pendingBackgroundAgentCount":2}`,
		`{"type":"assistant","uuid":"a9","message":{"content":[{"type":"text","text":"said turn_duration in passing"}]}}`,
		"",
		"@@corral-bg " + bgSid2,
		`{"type":"system","subtype":"turn_duration","uuid":"t2","durationMs":5}`, // (none out: the count left out, as 2.1.312 writes it)
		`{"type":"system","subtype":"turn_duration","uuid":"s1","isSidechain":true,"pendingBackgroundAgentCount":7}`,
		"",
		"@@corral-bg not-a-session",
		`{"type":"system","subtype":"turn_duration","uuid":"x","pendingBackgroundAgentCount":1}`,
	}, "\n")
	got := parseBgTails([]byte(out))
	if r := got[bgSid1]; r.uuid != "t1" || r.n != 2 {
		t.Fatalf("first: %+v", r)
	}
	if r := got[bgSid2]; r.uuid != "t2" || r.n != 0 {
		t.Fatalf("second (count left out, a subagent's own record passed over): %+v", r)
	}
	if len(got) != 2 {
		t.Fatalf("an id not of a session taken: %+v", got)
	}
}

// fakeTails answers the prober's reads as the logs say now.
type fakeTails struct {
	mu    sync.Mutex
	recs  map[string]bgRecord
	reads int
	fail  bool
}

func (f *fakeTails) read(ctx context.Context, sids []string, tail int) (map[string]bgRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads++
	if f.fail {
		return nil, context.DeadlineExceeded
	}
	out := map[string]bgRecord{}
	for _, s := range sids {
		out[s] = f.recs[s]
	}
	return out, nil
}

func (f *fakeTails) set(sid string, r bgRecord) {
	f.mu.Lock()
	f.recs[sid] = r
	f.mu.Unlock()
}

func bgState(p *bgProber, pane string) (string, int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if b := p.panes[pane]; b != nil {
		return b.state, b.n
	}
	return "", 0
}

func TestBgProber(t *testing.T) {
	old := struct{ r time.Duration }{bgRetry}
	defer func() { bgRetry = old.r }()
	bgRetry = 20 * time.Millisecond
	f := &fakeTails{recs: map[string]bgRecord{bgSid1: {uuid: "t1", n: 2}}}
	p := newBgProber(f.read)
	poked := make(chan struct{}, 10)
	p.setPoke(func() { poked <- struct{}{} })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go p.run(ctx)

	// Done with two subagents out (the first look: the log's last record taken).
	p.consider("p1", "claude", herdr.StatusDone, bgSid1, 1)
	if st := p.await(ctx, "p1", 1); st != bgRunning {
		t.Fatalf("first: %s", st)
	}
	<-poked
	// Working again: forgotten; done again with the record not written yet
	// (the last one's still last): read again until it is.
	p.consider("p1", "claude", herdr.StatusWorking, bgSid1, 2)
	if st, _ := bgState(p, "p1"); st != "" {
		t.Fatalf("still kept while working: %s", st)
	}
	p.consider("p1", "claude", herdr.StatusDone, bgSid1, 3)
	time.Sleep(5 * time.Millisecond)
	f.set(bgSid1, bgRecord{uuid: "t2"}) // (none out now)
	if st := p.await(ctx, "p1", 0); st != bgNone {
		t.Fatalf("after the subagents came back: %s", st)
	}
	// Done, then idle (looked at in herdr): the same turn's end, not read again.
	n := f.reads
	p.consider("p1", "claude", herdr.StatusIdle, bgSid1, 4)
	time.Sleep(30 * time.Millisecond)
	if f.reads != n {
		t.Fatalf("read again for the same turn's end")
	}
	// A turn whose record never comes: not known after three reads.
	p.consider("p1", "claude", herdr.StatusWorking, bgSid1, 5)
	p.consider("p1", "claude", herdr.StatusDone, bgSid1, 6)
	if st := p.await(ctx, "p1", 0); st != bgUnknown {
		t.Fatalf("a record never written: %s", st)
	}
	// Not Claude, or no session: nothing read.
	p.consider("p2", "codex", herdr.StatusDone, bgSid2, 7)
	p.consider("p3", "claude", herdr.StatusDone, "", 8)
	if st := p.await(ctx, "p2", 0); st != bgNone {
		t.Fatalf("codex: %s", st)
	}
}

// Gone back to work while being read for: what was waited for is told as
// moved, and the read's answer is of before (dropped).
func TestBgProberMoved(t *testing.T) {
	f := &fakeTails{recs: map[string]bgRecord{}, fail: true}
	p := newBgProber(f.read)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	p.consider("p1", "claude", herdr.StatusDone, bgSid1, 1)
	done := make(chan string, 1)
	go func() { st := p.await(ctx, "p1", 0); done <- st }()
	time.Sleep(10 * time.Millisecond)
	p.consider("p1", "claude", herdr.StatusWorking, bgSid1, 2)
	if st := <-done; st != bgMoved {
		t.Fatalf("moved: %s", st)
	}
}

// A log whose end has no turn record (request records, a compaction while
// idle came after it) is read further back.
func TestBgProberFar(t *testing.T) {
	var tails []int
	var mu sync.Mutex
	p := newBgProber(func(ctx context.Context, sids []string, tail int) (map[string]bgRecord, error) {
		mu.Lock()
		tails = append(tails, tail)
		mu.Unlock()
		if tail < bgFar {
			return map[string]bgRecord{sids[0]: {}}, nil
		}
		return map[string]bgRecord{sids[0]: {uuid: "t9", n: 1}}, nil
	})
	old := bgRetry
	bgRetry = 10 * time.Millisecond
	defer func() { bgRetry = old }()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	go p.run(ctx)
	p.consider("p1", "claude", herdr.StatusDone, bgSid1, 1)
	if st := p.await(ctx, "p1", 0); st != bgRunning {
		t.Fatalf("read further back: %s (tails %v)", st, tails)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(tails) != 2 || tails[0] != bgTail || tails[1] != bgFar {
		t.Fatalf("tails read: %v", tails)
	}
}

// Subagents out, then back with no turn's end told (two changes in one
// snapshot): the minute's look finds them back and tells it.
func TestBgProberBack(t *testing.T) {
	oldR, oldC := bgRetry, bgRecheck
	bgRetry, bgRecheck = 10*time.Millisecond, 30*time.Millisecond
	defer func() { bgRetry, bgRecheck = oldR, oldC }()
	f := &fakeTails{recs: map[string]bgRecord{bgSid1: {uuid: "t1", n: 1}}}
	p := newBgProber(f.read)
	back := make(chan string, 1)
	p.setBack(func(pane string) { back <- pane })
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	go p.run(ctx)
	p.consider("p1", "claude", herdr.StatusDone, bgSid1, 1)
	if st := p.await(ctx, "p1", 1); st != bgRunning {
		t.Fatalf("out: %s", st)
	}
	f.set(bgSid1, bgRecord{uuid: "t2"})
	select {
	case pane := <-back:
		if pane != "p1" {
			t.Fatalf("back: %s", pane)
		}
	case <-ctx.Done():
		t.Fatal("not told back")
	}
	// Its session not told a while (the gateway's reading busy): kept.
	p.consider("p1", "claude", herdr.StatusDone, "", 2)
	if st, _ := bgState(p, "p1"); st != bgNone {
		t.Fatalf("lost without its session: %q", st)
	}
}
