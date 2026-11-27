// Package perf keeps bounded, in-process latency and outcome statistics for
// key gateway operations so online baselines can be inspected without external
// metrics infrastructure. It stores only operation names, durations, fixed
// outcome/bucket labels, and numeric host IDs — never user text, paths,
// command lines, or host addresses.
package perf

import (
	"math"
	"slices"
	"strconv"
	"sync"
	"time"
)

const (
	DefaultMaxSamples = 2000
	DefaultWindow     = 24 * time.Hour
	DefaultMaxOps     = 256
)

// Options configures a Tracker. Zero values use package defaults.
type Options struct {
	MaxSamples int
	Window     time.Duration
	MaxOps     int
	Now        func() time.Time
}

// Tracker stores bounded ring-buffer samples per operation name.
// A nil *Tracker is safe to call and acts as a no-op.
type Tracker struct {
	maxSamples int
	window     time.Duration
	maxOps     int
	now        func() time.Time

	mu  sync.Mutex
	ops map[string]*ring
}

type sample struct {
	atNs    int64
	durNs   int64
	timed   bool
	hostID  int64
	outcome string
	bucket  string
}

type ring struct {
	buf  []sample
	next int
	full bool
}

func (r *ring) push(s sample, capN int) {
	if len(r.buf) < capN {
		r.buf = append(r.buf, s)
		if len(r.buf) == capN {
			r.full = true
			r.next = 0
		}
		return
	}
	r.buf[r.next] = s
	r.next++
	if r.next >= capN {
		r.next = 0
	}
	r.full = true
}

// Snapshot is the JSON-serializable summary returned by Tracker.Snapshot.
type Snapshot struct {
	GeneratedAt     time.Time            `json:"generated_at"`
	WindowSeconds   int64                `json:"window_seconds"`
	MaxSamplesPerOp int                  `json:"max_samples_per_op"`
	Ops             map[string]OpSummary `json:"ops"`
}

// StatSummary holds count, outcome counts, and latency quantiles (when timed
// samples exist).
type StatSummary struct {
	Count    int            `json:"count"`
	Outcomes map[string]int `json:"outcomes"`
	P50Ms    *float64       `json:"p50_ms,omitempty"`
	P95Ms    *float64       `json:"p95_ms,omitempty"`
	P99Ms    *float64       `json:"p99_ms,omitempty"`
	MaxMs    *float64       `json:"max_ms,omitempty"`
}

// OpSummary summarizes one operation overall and optionally by process-count
// bucket and numeric host ID.
type OpSummary struct {
	StatSummary
	ByBucket map[string]StatSummary `json:"by_bucket,omitempty"`
	ByHost   map[string]StatSummary `json:"by_host,omitempty"`
}

// New creates a Tracker with the given options (zero fields use defaults).
func New(opt Options) *Tracker {
	maxSamples := opt.MaxSamples
	if maxSamples <= 0 {
		maxSamples = DefaultMaxSamples
	}
	window := opt.Window
	if window <= 0 {
		window = DefaultWindow
	}
	maxOps := opt.MaxOps
	if maxOps <= 0 {
		maxOps = DefaultMaxOps
	}
	nowFn := opt.Now
	if nowFn == nil {
		nowFn = time.Now
	}
	return &Tracker{
		maxSamples: maxSamples,
		window:     window,
		maxOps:     maxOps,
		now:        nowFn,
		ops:        make(map[string]*ring),
	}
}

// Observe records a timed operation sample.
func (t *Tracker) Observe(op string, d time.Duration, outcome string, hostID int64, bucket string) {
	if t == nil || op == "" {
		return
	}
	if d < 0 {
		d = 0
	}
	t.record(op, sample{
		atNs:    t.now().UnixNano(),
		durNs:   d.Nanoseconds(),
		timed:   true,
		hostID:  hostID,
		outcome: normOutcome(outcome),
		bucket:  bucket,
	})
}

// Count records a count-only event (no duration sample).
func (t *Tracker) Count(op string, outcome string, hostID int64) {
	if t == nil || op == "" {
		return
	}
	t.record(op, sample{
		atNs:    t.now().UnixNano(),
		timed:   false,
		hostID:  hostID,
		outcome: normOutcome(outcome),
	})
}

func (t *Tracker) record(op string, s sample) {
	t.mu.Lock()
	defer t.mu.Unlock()
	r := t.ops[op]
	if r == nil {
		if len(t.ops) >= t.maxOps {
			return
		}
		r = &ring{}
		t.ops[op] = r
	}
	r.push(s, t.maxSamples)
}

// Snapshot computes summary statistics over all non-expired samples.
func (t *Tracker) Snapshot() Snapshot {
	if t == nil {
		return Snapshot{
			GeneratedAt:     time.Now().UTC(),
			WindowSeconds:   int64(DefaultWindow / time.Second),
			MaxSamplesPerOp: DefaultMaxSamples,
			Ops:             map[string]OpSummary{},
		}
	}
	now := t.now()
	cutoffNs := now.Add(-t.window).UnixNano()

	// Copy each op's non-expired samples under lock, then aggregate unlocked.
	t.mu.Lock()
	copied := make(map[string][]sample, len(t.ops))
	for op, r := range t.ops {
		var valid []sample
		for _, s := range r.buf {
			if s.atNs >= cutoffNs {
				valid = append(valid, s)
			}
		}
		if len(valid) > 0 {
			copied[op] = valid
		}
	}
	windowSec := int64(t.window / time.Second)
	maxSamples := t.maxSamples
	t.mu.Unlock()

	ops := make(map[string]OpSummary, len(copied))
	for op, samples := range copied {
		ops[op] = summarizeOp(samples)
	}
	return Snapshot{
		GeneratedAt:     now.UTC(),
		WindowSeconds:   windowSec,
		MaxSamplesPerOp: maxSamples,
		Ops:             ops,
	}
}

func summarizeOp(samples []sample) OpSummary {
	out := OpSummary{
		StatSummary: summarizeSamples(samples),
	}
	var buckets map[string][]sample
	var hosts map[string][]sample
	for _, s := range samples {
		if s.bucket != "" {
			if buckets == nil {
				buckets = make(map[string][]sample)
			}
			buckets[s.bucket] = append(buckets[s.bucket], s)
		}
		if s.hostID > 0 {
			if hosts == nil {
				hosts = make(map[string][]sample)
			}
			hk := strconv.FormatInt(s.hostID, 10)
			hosts[hk] = append(hosts[hk], s)
		}
	}
	if len(buckets) > 0 {
		out.ByBucket = make(map[string]StatSummary, len(buckets))
		for k, bs := range buckets {
			out.ByBucket[k] = summarizeSamples(bs)
		}
	}
	if len(hosts) > 0 {
		out.ByHost = make(map[string]StatSummary, len(hosts))
		for k, hs := range hosts {
			out.ByHost[k] = summarizeSamples(hs)
		}
	}
	return out
}

func summarizeSamples(samples []sample) StatSummary {
	outcomes := make(map[string]int)
	durs := make([]time.Duration, 0, len(samples))
	for _, s := range samples {
		outcomes[s.outcome]++
		if s.timed {
			durs = append(durs, time.Duration(s.durNs))
		}
	}
	sum := StatSummary{
		Count:    len(samples),
		Outcomes: outcomes,
	}
	if len(durs) > 0 {
		slices.Sort(durs)
		p50 := RoundMs(Percentile(durs, 0.50))
		p95 := RoundMs(Percentile(durs, 0.95))
		p99 := RoundMs(Percentile(durs, 0.99))
		mx := RoundMs(durs[len(durs)-1])
		sum.P50Ms = &p50
		sum.P95Ms = &p95
		sum.P99Ms = &p99
		sum.MaxMs = &mx
	}
	return sum
}

// Percentile returns the nearest-rank quantile q (in (0, 1]) of an already
// sorted ascending duration slice. Returns 0 when sorted is empty.
func Percentile(sorted []time.Duration, q float64) time.Duration {
	n := len(sorted)
	if n == 0 {
		return 0
	}
	if q <= 0 {
		return sorted[0]
	}
	if q >= 1 {
		return sorted[n-1]
	}
	idx := int(math.Ceil(q*float64(n))) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= n {
		idx = n - 1
	}
	return sorted[idx]
}

// RoundMs converts a duration to milliseconds rounded to 2 decimal places.
func RoundMs(d time.Duration) float64 {
	return math.Round(float64(d)/float64(time.Millisecond)*100) / 100
}

// ProcBucket groups a host's located agent process count into coarse buckets.
func ProcBucket(n int) string {
	switch {
	case n <= 0:
		return "0"
	case n <= 5:
		return "1-5"
	case n <= 15:
		return "6-15"
	case n <= 30:
		return "16-30"
	case n <= 60:
		return "31-60"
	default:
		return "61+"
	}
}

func normOutcome(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}
