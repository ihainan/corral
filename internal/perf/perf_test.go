package perf

import (
	"sync"
	"testing"
	"time"
)

func TestPercentile(t *testing.T) {
	if got := Percentile(nil, 0.5); got != 0 {
		t.Fatalf("empty slice: got %v, want 0", got)
	}
	single := []time.Duration{42 * time.Millisecond}
	for _, q := range []float64{0, 0.5, 0.95, 0.99, 1} {
		if got := Percentile(single, q); got != 42*time.Millisecond {
			t.Fatalf("single q=%v: got %v, want 42ms", q, got)
		}
	}

	// 1ms .. 100ms
	s100 := make([]time.Duration, 100)
	for i := range s100 {
		s100[i] = time.Duration(i+1) * time.Millisecond
	}
	cases := []struct {
		q    float64
		want time.Duration
	}{
		{0.0, 1 * time.Millisecond},
		{0.50, 50 * time.Millisecond},
		{0.95, 95 * time.Millisecond},
		{0.99, 99 * time.Millisecond},
		{1.0, 100 * time.Millisecond},
	}
	for _, tc := range cases {
		if got := Percentile(s100, tc.q); got != tc.want {
			t.Errorf("100 samples q=%v: got %v, want %v", tc.q, got, tc.want)
		}
	}

	// 20 samples: 1ms .. 20ms -> p50=10ms, p95=19ms, p99=20ms
	s20 := s100[:20]
	if got := Percentile(s20, 0.50); got != 10*time.Millisecond {
		t.Errorf("20 samples p50: got %v, want 10ms", got)
	}
	if got := Percentile(s20, 0.95); got != 19*time.Millisecond {
		t.Errorf("20 samples p95: got %v, want 19ms", got)
	}
	if got := Percentile(s20, 0.99); got != 20*time.Millisecond {
		t.Errorf("20 samples p99: got %v, want 20ms", got)
	}
}

func TestProcBucket(t *testing.T) {
	cases := []struct {
		n    int
		want string
	}{
		{-1, "0"},
		{0, "0"},
		{1, "1-5"},
		{5, "1-5"},
		{6, "6-15"},
		{15, "6-15"},
		{16, "16-30"},
		{30, "16-30"},
		{31, "31-60"},
		{60, "31-60"},
		{61, "61+"},
		{200, "61+"},
	}
	for _, tc := range cases {
		if got := ProcBucket(tc.n); got != tc.want {
			t.Errorf("ProcBucket(%d) = %q, want %q", tc.n, got, tc.want)
		}
	}
}

func TestTrackerCapacityLimit(t *testing.T) {
	now := time.Date(2026, 11, 27, 10, 0, 0, 0, time.UTC)
	tr := New(Options{
		MaxSamples: 100,
		Window:     time.Hour,
		Now:        func() time.Time { return now },
	})

	// Push 50 old slow error samples, then 100 newer fast ok samples (1ms..100ms).
	// Because cap is 100, all 50 old samples must be overwritten.
	for i := 0; i < 50; i++ {
		tr.Observe("locate.full", 9*time.Second, "error", 1, "61+")
	}
	for i := 1; i <= 100; i++ {
		tr.Observe("locate.full", time.Duration(i)*time.Millisecond, "ok", 2, "1-5")
	}

	snap := tr.Snapshot()
	op, ok := snap.Ops["locate.full"]
	if !ok {
		t.Fatal("missing locate.full op")
	}
	if op.Count != 100 {
		t.Fatalf("count = %d, want 100", op.Count)
	}
	if op.Outcomes["error"] != 0 || op.Outcomes["ok"] != 100 {
		t.Fatalf("outcomes = %v, want only ok=100", op.Outcomes)
	}
	if op.P50Ms == nil || *op.P50Ms != 50 {
		t.Fatalf("p50 = %v, want 50", op.P50Ms)
	}
	if op.P95Ms == nil || *op.P95Ms != 95 {
		t.Fatalf("p95 = %v, want 95", op.P95Ms)
	}
	if op.P99Ms == nil || *op.P99Ms != 99 {
		t.Fatalf("p99 = %v, want 99", op.P99Ms)
	}
	if op.MaxMs == nil || *op.MaxMs != 100 {
		t.Fatalf("max = %v, want 100", op.MaxMs)
	}
	if _, leaked := op.ByHost["1"]; leaked {
		t.Fatalf("evicted host 1 still present in ByHost: %v", op.ByHost)
	}
	if _, leaked := op.ByBucket["61+"]; leaked {
		t.Fatalf("evicted bucket 61+ still present in ByBucket: %v", op.ByBucket)
	}
}

func TestTrackerExpiration(t *testing.T) {
	now := time.Date(2026, 11, 27, 10, 0, 0, 0, time.UTC)
	tr := New(Options{
		MaxSamples: 200,
		Window:     10 * time.Minute,
		Now:        func() time.Time { return now },
	})

	// Record 10 slow samples at t=0.
	for i := 0; i < 10; i++ {
		tr.Observe("gated.send", 5*time.Second, "screen_changed", 7, "")
	}
	tr.Observe("old.only", 100*time.Millisecond, "ok", 7, "")

	// Advance 9 minutes: still within 10m window.
	now = now.Add(9 * time.Minute)
	tr.Observe("gated.send", 20*time.Millisecond, "ok", 8, "")
	snap := tr.Snapshot()
	if snap.Ops["gated.send"].Count != 11 {
		t.Fatalf("at 9m: count = %d, want 11", snap.Ops["gated.send"].Count)
	}

	// Advance 2 more minutes (total 11m): t=0 samples expire, t=9m sample remains.
	now = now.Add(2 * time.Minute)
	snap = tr.Snapshot()
	if _, still := snap.Ops["old.only"]; still {
		t.Fatalf("old.only should be omitted once all its samples expire")
	}
	gs, ok := snap.Ops["gated.send"]
	if !ok {
		t.Fatal("gated.send missing")
	}
	if gs.Count != 1 {
		t.Fatalf("after expiry: count = %d, want 1", gs.Count)
	}
	if gs.Outcomes["screen_changed"] != 0 || gs.Outcomes["ok"] != 1 {
		t.Fatalf("after expiry: outcomes = %v, want ok=1", gs.Outcomes)
	}
	if gs.P50Ms == nil || *gs.P50Ms != 20 || *gs.P99Ms != 20 {
		t.Fatalf("after expiry: p50=%v p99=%v, want 20", gs.P50Ms, gs.P99Ms)
	}
	if _, hasHost7 := gs.ByHost["7"]; hasHost7 {
		t.Fatalf("expired host 7 still present: %v", gs.ByHost)
	}
	if gs.ByHost["8"].Count != 1 {
		t.Fatalf("host 8 count = %d, want 1", gs.ByHost["8"].Count)
	}
}

func TestTrackerMaxOpsCap(t *testing.T) {
	tr := New(Options{MaxOps: 3, MaxSamples: 10})
	tr.Count("op1", "ok", 1)
	tr.Count("op2", "ok", 1)
	tr.Count("op3", "ok", 1)
	tr.Count("op4", "ok", 1) // dropped because MaxOps=3

	snap := tr.Snapshot()
	if len(snap.Ops) != 3 {
		t.Fatalf("len(ops) = %d, want 3", len(snap.Ops))
	}
	if _, ok := snap.Ops["op4"]; ok {
		t.Fatalf("op4 should not be tracked past MaxOps=3")
	}
}

func TestTrackerCountOnlyAndDimensions(t *testing.T) {
	tr := New(Options{})
	tr.Count("ssh.busy", "exec", 11)
	tr.Count("ssh.busy", "dial_watch", 11)
	tr.Count("ssh.busy", "exec", 12)

	tr.Observe("locate.full", 123450*time.Microsecond, "ok", 11, ProcBucket(20))
	tr.Observe("locate.full", 250*time.Millisecond, "busy", 12, ProcBucket(40))

	snap := tr.Snapshot()
	busy := snap.Ops["ssh.busy"]
	if busy.Count != 3 || busy.Outcomes["exec"] != 2 || busy.Outcomes["dial_watch"] != 1 {
		t.Fatalf("unexpected ssh.busy summary: %+v", busy)
	}
	if busy.P50Ms != nil || busy.P95Ms != nil || busy.P99Ms != nil || busy.MaxMs != nil {
		t.Fatalf("count-only op should omit latency fields: %+v", busy)
	}
	if busy.ByHost["11"].Count != 2 || busy.ByHost["12"].Count != 1 {
		t.Fatalf("unexpected ssh.busy by_host: %+v", busy.ByHost)
	}

	loc := snap.Ops["locate.full"]
	if loc.ByBucket["16-30"].Count != 1 || loc.ByBucket["31-60"].Count != 1 {
		t.Fatalf("unexpected locate.full by_bucket: %+v", loc.ByBucket)
	}
	if *loc.ByBucket["16-30"].P50Ms != 123.45 {
		t.Fatalf("bucket 16-30 p50 = %v, want 123.45", *loc.ByBucket["16-30"].P50Ms)
	}
}

func TestTrackerNilSafeAndConcurrent(t *testing.T) {
	var nilTracker *Tracker
	nilTracker.Observe("x", time.Millisecond, "ok", 1, "1-5")
	nilTracker.Count("y", "ok", 1)
	if s := nilTracker.Snapshot(); len(s.Ops) != 0 {
		t.Fatalf("nil snapshot ops = %v, want empty", s.Ops)
	}

	tr := New(Options{MaxSamples: 50})
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(id int64) {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				tr.Observe("convo.read", time.Duration(i+1)*time.Millisecond, "ok", id+1, "")
				tr.Count("ssh.busy", "exec", id+1)
				if i%20 == 0 {
					_ = tr.Snapshot()
				}
			}
		}(int64(g))
	}
	wg.Wait()
	snap := tr.Snapshot()
	if snap.Ops["convo.read"].Count != 50 {
		t.Fatalf("concurrent cap: count = %d, want 50", snap.Ops["convo.read"].Count)
	}
	if snap.Ops["ssh.busy"].Count != 50 {
		t.Fatalf("concurrent busy cap: count = %d, want 50", snap.Ops["ssh.busy"].Count)
	}
}
