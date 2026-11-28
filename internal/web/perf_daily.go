package web

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/ihainan/corral/internal/corrald/gw"
	"github.com/ihainan/corral/internal/store"
)

const (
	corraldPerfRetentionDays = 90
	corraldPerfFlushInterval = 5 * time.Second
	corraldPerfPruneInterval = time.Hour
)

type perfDailyKey struct {
	hostID int64
	day    string
	op     string
	via    string
}

// corraldPerfDaily buffers successful per-host/day/op/via latency totals in
// memory and flushes them asynchronously to SQLite so sending/answering never
// waits on database I/O.
type corraldPerfDaily struct {
	store         *store.Store
	log           *slog.Logger
	now           func() time.Time
	flushInterval time.Duration
	writeBatch    func(ctx context.Context, deltas []store.CorraldPerfDailyDelta) error

	flushMu   sync.Mutex
	mu        sync.Mutex
	pending   map[perfDailyKey]store.CorraldPerfTotal
	lastPrune time.Time
	closed    bool

	stopCh chan struct{}
	doneCh chan struct{}
}

func newCorraldPerfDaily(st *store.Store, log *slog.Logger, flushInterval time.Duration, nowFn func() time.Time) *corraldPerfDaily {
	if flushInterval <= 0 {
		flushInterval = corraldPerfFlushInterval
	}
	if nowFn == nil {
		nowFn = time.Now
	}
	p := &corraldPerfDaily{
		store:         st,
		log:           log,
		now:           nowFn,
		flushInterval: flushInterval,
		pending:       make(map[perfDailyKey]store.CorraldPerfTotal),
		stopCh:        make(chan struct{}),
		doneCh:        make(chan struct{}),
	}
	go p.loop()
	return p
}

// canonicalPerfOp maps gated operation names to persisted daily comparison
// buckets. Returns "" for operations not tracked in the before/after display.
func canonicalPerfOp(what string) string {
	switch what {
	case "send", "reply":
		return "send"
	case "card":
		return "card"
	default:
		return ""
	}
}

// Observe buffers one successful operation sample without blocking on I/O.
func (p *corraldPerfDaily) Observe(hostID int64, what, via string, d time.Duration) {
	if p == nil || hostID <= 0 {
		return
	}
	op := canonicalPerfOp(what)
	if op == "" {
		return
	}
	if via != "ssh" && via != "corrald" {
		return
	}
	if d < 0 {
		d = 0
	}
	day := p.now().UTC().Format("2006-01-02")
	k := perfDailyKey{hostID: hostID, day: day, op: op, via: via}

	p.mu.Lock()
	if !p.closed {
		cur := p.pending[k]
		cur.Count++
		cur.TotalUs += d.Microseconds()
		p.pending[k] = cur
	}
	p.mu.Unlock()
}

func (p *corraldPerfDaily) loop() {
	defer close(p.doneCh)
	tk := time.NewTicker(p.flushInterval)
	defer tk.Stop()
	for {
		select {
		case <-tk.C:
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			_ = p.Flush(ctx)
			cancel()
		case <-p.stopCh:
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			_ = p.Flush(ctx)
			cancel()
			return
		}
	}
}

// Flush writes all buffered daily deltas to SQLite and prunes rows older than
// 90 days (throttled to once per hour). Concurrent flushes (including the final
// flush on Close) are serialized by flushMu so a failed periodic flush always
// re-queues its batch before Close runs its final retry.
func (p *corraldPerfDaily) Flush(ctx context.Context) error {
	if p == nil || (p.store == nil && p.writeBatch == nil) {
		return nil
	}
	p.flushMu.Lock()
	defer p.flushMu.Unlock()

	p.mu.Lock()
	var batch []store.CorraldPerfDailyDelta
	if len(p.pending) > 0 {
		batch = make([]store.CorraldPerfDailyDelta, 0, len(p.pending))
		for k, v := range p.pending {
			batch = append(batch, store.CorraldPerfDailyDelta{
				HostID:  k.hostID,
				Day:     k.day,
				Op:      k.op,
				Via:     k.via,
				Count:   v.Count,
				TotalUs: v.TotalUs,
			})
		}
		p.pending = make(map[perfDailyKey]store.CorraldPerfTotal)
	}
	now := p.now()
	needPrune := p.store != nil && (p.lastPrune.IsZero() || now.Sub(p.lastPrune) >= corraldPerfPruneInterval)
	if needPrune {
		p.lastPrune = now
	}
	writeFn := p.writeBatch
	if writeFn == nil {
		writeFn = p.store.AddCorraldPerfDailyBatch
	}
	p.mu.Unlock()

	var firstErr error
	if len(batch) > 0 {
		if err := writeFn(ctx, batch); err != nil {
			firstErr = err
			// Always re-queue failed deltas (even if Close() was called
			// concurrently) so the final flush on shutdown can persist them.
			p.mu.Lock()
			for _, d := range batch {
				k := perfDailyKey{hostID: d.HostID, day: d.Day, op: d.Op, via: d.Via}
				cur := p.pending[k]
				cur.Count += d.Count
				cur.TotalUs += d.TotalUs
				p.pending[k] = cur
			}
			p.mu.Unlock()
			if p.log != nil {
				p.log.Warn("flush corrald daily perf failed", "err", err)
			}
		}
	}
	if needPrune {
		cutoff := now.UTC().AddDate(0, 0, -corraldPerfRetentionDays).Format("2006-01-02")
		if _, err := p.store.PruneCorraldPerfDaily(ctx, cutoff); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			if p.log != nil {
				p.log.Warn("prune corrald daily perf failed", "err", err)
			}
		}
	}
	return firstErr
}

// Compare computes the before/after latency comparison for hostID, merging
// persisted SQLite rollups with any not-yet-flushed in-memory deltas.
func (p *corraldPerfDaily) Compare(ctx context.Context, hostID, consentAt int64) []gw.PerfCompareItem {
	if p == nil || p.store == nil || hostID <= 0 || consentAt <= 0 {
		return nil
	}
	now := p.now()
	if now.Sub(time.Unix(consentAt, 0)) < gw.PerfCompareMinEnabledAge {
		return nil
	}
	beforeMin, beforeMax, afterMin := gw.PerfCompareDateBounds(consentAt)
	before, err := p.store.CorraldPerfDailyTotals(ctx, hostID, "ssh", beforeMin, beforeMax)
	if err != nil {
		return nil
	}
	after, err := p.store.CorraldPerfDailyTotals(ctx, hostID, "corrald", afterMin, "")
	if err != nil {
		return nil
	}

	// Merge any unflushed deltas in memory so comparisons reflect recent samples.
	p.mu.Lock()
	for k, v := range p.pending {
		if k.hostID != hostID {
			continue
		}
		switch k.via {
		case "ssh":
			if k.day >= beforeMin && k.day <= beforeMax {
				cur := before[k.op]
				cur.Count += v.Count
				cur.TotalUs += v.TotalUs
				before[k.op] = cur
			}
		case "corrald":
			if k.day >= afterMin {
				cur := after[k.op]
				cur.Count += v.Count
				cur.TotalUs += v.TotalUs
				after[k.op] = cur
			}
		}
	}
	p.mu.Unlock()

	return gw.ComputePerfComparison(now, consentAt, before, after)
}

// Close stops the background flusher and best-effort flushes remaining samples.
func (p *corraldPerfDaily) Close() {
	if p == nil {
		return
	}
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	close(p.stopCh)
	p.mu.Unlock()
	<-p.doneCh
}
