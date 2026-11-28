package gw

import (
	"context"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/ihainan/corral/internal/corrald/proto"
	"github.com/ihainan/corral/internal/store"
)

const (
	// PerfCompareMinEnabledAge is the minimum time corrald must be enabled
	// before showing measured before/after latency comparisons.
	PerfCompareMinEnabledAge = 24 * time.Hour

	// PerfCompareMinSamples is the minimum successful sample count required on
	// BOTH sides (before SSH and after corrald) for an operation to appear.
	PerfCompareMinSamples int64 = 20

	// PerfCompareBeforeDays limits "before enabling" SSH samples to the 30 days
	// preceding consent_at.
	PerfCompareBeforeDays = 30
)

// PerfCompareItem summarizes measured before-vs-after average latency for one
// operation on a host.
type PerfCompareItem struct {
	Op       string  `json:"op"` // "send", "card"
	AfterMs  float64 `json:"after_ms"`
	BeforeMs float64 `json:"before_ms"`
	AfterN   int64   `json:"after_n"`
	BeforeN  int64   `json:"before_n"`
}

// PerfCompareOps defines the deterministic display order of compared operations.
var PerfCompareOps = []string{"send", "card"}

// ComputePerfComparison computes before-vs-after latency comparisons from
// aggregated before (SSH) and after (corrald) totals.
func ComputePerfComparison(now time.Time, consentAt int64, before, after map[string]store.CorraldPerfTotal) []PerfCompareItem {
	if consentAt <= 0 {
		return nil
	}
	if now.Sub(time.Unix(consentAt, 0)) < PerfCompareMinEnabledAge {
		return nil
	}
	var out []PerfCompareItem
	for _, op := range PerfCompareOps {
		b := before[op]
		a := after[op]
		if b.Count < PerfCompareMinSamples || a.Count < PerfCompareMinSamples {
			continue
		}
		beforeMs := math.Round(float64(b.TotalUs)/float64(b.Count)/100.0) / 10.0
		afterMs := math.Round(float64(a.TotalUs)/float64(a.Count)/100.0) / 10.0
		out = append(out, PerfCompareItem{
			Op:       op,
			AfterMs:  afterMs,
			BeforeMs: beforeMs,
			AfterN:   a.Count,
			BeforeN:  b.Count,
		})
	}
	return out
}

// PerfCompareDateBounds returns the UTC YYYY-MM-DD bounds for before (SSH,
// [beforeMin, consentDay]) and after (corrald, [consentDay, ""]) queries.
func PerfCompareDateBounds(consentAt int64) (beforeMin, consentDay string) {
	t := time.Unix(consentAt, 0).UTC()
	return t.AddDate(0, 0, -PerfCompareBeforeDays).Format("2006-01-02"), t.Format("2006-01-02")
}

// AdminDisabled reports whether the administrator policy currently forbids corrald.
func (m *Manager) AdminDisabled() bool {
	if m == nil {
		return false
	}
	return m.adminDisabled.Load()
}

// SetAdminDisabled updates the administrator policy at runtime.
//
// When disabled becomes true, it takes effect immediately:
//   - All capability routing (ClientForCap, watch) immediately falls back to SSH.
//   - Any in-progress install/update operations are cancelled.
//   - On each currently connected host:
//   - If corrald reports no other gateway connected (health.connected_gateways <= 1),
//     corrald is stopped via daemon.shutdown and the host is marked remove_pending
//     so its files are cleaned up by the existing uninstall flow on next connection.
//   - If another gateway is also connected (health.connected_gateways > 1), only
//     this gateway disconnects — the daemon keeps running and files are untouched.
//
// When disabled becomes false, previously cleaned hosts remain off (users must
// re-enable), while preserved hosts reconnect normally on their next connection.
func (m *Manager) SetAdminDisabled(ctx context.Context, disabled bool) {
	if m == nil {
		return
	}
	m.adminDisabled.Store(disabled)
	if !disabled {
		return
	}

	// Cancel in-flight install/update operations and detach all active connections.
	m.mu.Lock()
	for _, op := range m.opsByHost {
		if (op.kind == OpInstall || op.kind == OpUpdate) && op.cancel != nil {
			op.cancelled = true
			op.cancel()
		}
	}
	for _, op := range m.opsByAcct {
		if (op.kind == OpInstall || op.kind == OpUpdate) && op.cancel != nil {
			op.cancelled = true
			op.cancel()
		}
	}
	conns := make([]*sharedConn, 0, len(m.conns))
	for _, sc := range m.conns {
		conns = append(conns, sc)
	}
	m.conns = make(map[accountKey]*sharedConn)
	m.hostConn = make(map[int64]accountKey)
	m.mu.Unlock()

	if ctx == nil {
		ctx = context.Background()
	}

	for _, sc := range conns {
		m.enforceAdminDisableOnConn(ctx, sc)
	}
}

func (m *Manager) enforceAdminDisableOnConn(ctx context.Context, sc *sharedConn) {
	sc.mu.Lock()
	client := sc.client
	cachedHealth := sc.health
	hostIDs := make([]int64, 0, len(sc.hostRefs))
	for hid := range sc.hostRefs {
		hostIDs = append(hostIDs, hid)
	}
	key := sc.key
	sc.mu.Unlock()

	otherGateways := false
	checked := false
	if client != nil && !client.Closed() {
		hctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		h, err := client.Health(hctx)
		cancel()
		if err == nil && h != nil {
			checked = true
			otherGateways = h.ConnectedGateways > 1
		}
	}
	if !checked {
		if cachedHealth != nil {
			checked = true
			otherGateways = cachedHealth.ConnectedGateways > 1
		} else {
			// Cannot verify whether another gateway is connected; stay safe and
			// only disconnect without stopping or marking for removal.
			otherGateways = true
		}
	}

	if !otherGateways {
		if client != nil && !client.Closed() {
			sctx, cancel := context.WithTimeout(ctx, 3*time.Second)
			_, _ = client.Shutdown(sctx, "admin_disabled")
			cancel()
		}
		if m.store != nil {
			_ = m.store.MarkCorraldHostsRemovePending(context.Background(), hostIDs, key.Fingerprint, key.Username)
		}
	} else {
		if m.store != nil {
			_ = m.store.MarkCorraldHostsStopped(context.Background(), hostIDs, key.Fingerprint, key.Username)
		}
	}

	m.closeSharedConn(sc, false)
	if len(hostIDs) > 0 {
		m.notifyCoverageChanged(hostIDs)
	}
}

// Changelog returns the filtered release notes for userID, showing the current
// version and any versions released after the user's last viewed changelog
// version (omitting versions with no notes).
func (m *Manager) Changelog(ctx context.Context, userID int64, currentVersion string) (curVer string, curRel int64, entries []proto.ChangelogEntry, err error) {
	dist := m.Dist()
	if dist == nil {
		return "", 0, nil, ErrUnavailable
	}
	curVer = strings.TrimSpace(currentVersion)
	if curVer == "" {
		curVer = dist.Version
	}
	curRel = dist.Release

	all := dist.Changelog
	if len(all) == 0 {
		all = proto.Changelog()
	}

	lastSeen := ""
	if m.store != nil && userID > 0 {
		seen, serr := m.store.GetCorraldChangelogSeen(ctx, userID)
		if serr != nil {
			return "", 0, nil, serr
		}
		if seen != nil {
			lastSeen = seen.LastVersion
		}
	}
	return curVer, curRel, proto.FilterChangelog(all, curVer, lastSeen), nil
}

// recordUpdateNotices creates pending update notices for every enabled user of
// the host/account when an update succeeds or fails and rolls back.
func (m *Manager) recordUpdateNotices(hostID int64, fp, sshUser, oldVer, newVer string, newRel int64, success bool) {
	if m == nil || m.store == nil || hostID <= 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	type pair struct {
		userID int64
		hostID int64
	}
	seen := make(map[pair]struct{})
	var notices []store.CorraldUpdateNotice
	now := time.Now().Unix()

	add := func(uid, hid int64) {
		if uid <= 0 || hid <= 0 {
			return
		}
		p := pair{userID: uid, hostID: hid}
		if _, dup := seen[p]; dup {
			return
		}
		seen[p] = struct{}{}
		notices = append(notices, store.CorraldUpdateNotice{
			UserID:     uid,
			HostID:     hid,
			OldVersion: oldVer,
			NewVersion: newVer,
			NewRelease: newRel,
			Success:    success,
			CreatedAt:  now,
		})
	}

	if fp != "" && sshUser != "" {
		if peers, err := m.store.CorraldPeersByBinding(ctx, fp, sshUser); err == nil {
			for _, p := range peers {
				if p.State == "" || p.State == StateOff || p.State == StateRemoving || p.State == StateRemovePending || p.State == StateRebind {
					continue
				}
				if slices.Contains(p.PausedUsers, p.OwnerID) {
					continue
				}
				add(p.OwnerID, p.HostID)
			}
		}
	}

	// Ensure the primary host's owner receives a notice even if binding lookup
	// was empty (e.g. host had not yet synced peer rows).
	if ownerID, err := m.store.HostOwnerID(ctx, hostID); err == nil && ownerID > 0 {
		if ch, cerr := m.store.CorraldHostByID(ctx, hostID); cerr == nil {
			if ch.State != StateOff && ch.State != StateRemoving && ch.State != StateRemovePending && ch.State != StateRebind && !slices.Contains(ch.PausedUsers, ownerID) {
				add(ownerID, hostID)
			}
		}
	}

	if len(notices) > 0 {
		_ = m.store.RecordCorraldUpdateNotices(ctx, notices)
	}
}

func (m *Manager) computeStatusPerfComparison(ctx context.Context, hostID, consentAt int64) []PerfCompareItem {
	if hostID <= 0 || consentAt <= 0 {
		return nil
	}
	if m.cfg.PerfCompare != nil {
		return m.cfg.PerfCompare(ctx, hostID, consentAt)
	}
	if m.store == nil {
		return nil
	}
	now := time.Now()
	if now.Sub(time.Unix(consentAt, 0)) < PerfCompareMinEnabledAge {
		return nil
	}
	beforeMin, consentDay := PerfCompareDateBounds(consentAt)
	before, err := m.store.CorraldPerfDailyTotals(ctx, hostID, "ssh", beforeMin, consentDay)
	if err != nil {
		return nil
	}
	after, err := m.store.CorraldPerfDailyTotals(ctx, hostID, "corrald", consentDay, "")
	if err != nil {
		return nil
	}
	return ComputePerfComparison(now, consentAt, before, after)
}
