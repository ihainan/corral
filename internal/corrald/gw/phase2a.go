package gw

import (
	"context"
	"math"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/ihainan/corral/internal/corrald/proto"
	"github.com/ihainan/corral/internal/store"
)

const (
	// adminDisableConcurrency bounds concurrent per-host admin disable/reconcile
	// workers launched in the background so SetAdminDisabled returns immediately
	// without overwhelming the gateway or remote hosts.
	adminDisableConcurrency = 4

	// pendingCleanupRetryCooldown rate-limits repeated background cleanup retries
	// for the same host on successive operations over an existing SSH connection.
	pendingCleanupRetryCooldown = 5 * time.Second
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
// [beforeMin, beforeMax], strictly before consentDay) and after (corrald,
// [afterMin, ""], strictly after consentDay). The enable day itself is excluded
// from both sides so post-enable SSH fallback traffic on consentDay never
// contaminates the "before" baseline and same-day corrald traffic is not mixed.
func PerfCompareDateBounds(consentAt int64) (beforeMin, beforeMax, afterMin string) {
	t := time.Unix(consentAt, 0).UTC()
	return t.AddDate(0, 0, -PerfCompareBeforeDays).Format("2006-01-02"),
		t.AddDate(0, 0, -1).Format("2006-01-02"),
		t.AddDate(0, 0, 1).Format("2006-01-02")
}

// AdminDisabled reports whether the administrator policy currently forbids corrald.
func (m *Manager) AdminDisabled() bool {
	if m == nil {
		return false
	}
	return m.adminDisabled.Load()
}

// clientsHaveOtherGateway reports whether any registered client belongs to a
// gateway other than gatewayID (or if gatewayID itself is unknown).
func clientsHaveOtherGateway(clients []proto.ClientInfo, gatewayID string) bool {
	if gatewayID == "" {
		return true
	}
	for _, c := range clients {
		if c.Gateway != gatewayID {
			return true
		}
	}
	return false
}

// SetAdminDisabled updates the administrator policy at runtime and returns
// immediately; per-host shutdown/cleanup runs asynchronously with bounded
// concurrency (adminDisableConcurrency = 4). Same-value updates are idempotent
// so repeated "disable" requests never invalidate in-flight cleanup workers.
func (m *Manager) SetAdminDisabled(ctx context.Context, disabled bool) {
	if m == nil {
		return
	}
	if !disabled {
		m.policyMu.Lock()
		if !m.adminDisabled.Load() {
			m.policyMu.Unlock()
			return
		}
		gen := m.policyGen.Add(1)
		m.adminDisabled.Store(false)
		m.mu.Lock()
		closed := m.closed
		launch := !closed && m.store != nil
		if launch {
			m.wg.Add(1)
		}
		m.mu.Unlock()
		m.policyMu.Unlock()

		if launch {
			go func() {
				defer m.wg.Done()
				m.reconcileRemovePendingOnAllow(gen)
			}()
		}
		return
	}

	// Acquire policyMu.Lock first so idempotency check, policyGen increment,
	// adminDisabled flip, and draining m.conns are atomic with respect to
	// in-flight attach/reconnect/rollback writes and duplicate SetAdminDisabled.
	m.policyMu.Lock()
	if m.adminDisabled.Load() {
		m.policyMu.Unlock()
		return
	}
	gen := m.policyGen.Add(1)
	m.adminDisabled.Store(true)

	m.mu.Lock()
	closed := m.closed
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
	var detachedHostIDs []int64
	for _, sc := range m.conns {
		conns = append(conns, sc)
	}
	for hid := range m.hostConn {
		detachedHostIDs = append(detachedHostIDs, hid)
	}
	m.conns = make(map[accountKey]*sharedConn)
	m.hostConn = make(map[int64]accountKey)
	if !closed && len(conns) > 0 {
		m.wg.Add(1)
	}
	m.mu.Unlock()
	m.policyMu.Unlock()

	if len(detachedHostIDs) > 0 {
		m.notifyCoverageChanged(detachedHostIDs)
	}
	if closed || len(conns) == 0 {
		return
	}

	go func() {
		defer m.wg.Done()
		bgCtx := m.bgCtx
		if bgCtx == nil {
			bgCtx = context.Background()
		}
		sem := make(chan struct{}, adminDisableConcurrency)
		var wg sync.WaitGroup
		for _, sc := range conns {
			sem <- struct{}{}
			wg.Add(1)
			go func(conn *sharedConn) {
				defer wg.Done()
				defer func() { <-sem }()
				m.enforceAdminDisableOnConn(bgCtx, conn, gen)
			}(sc)
		}
		wg.Wait()
	}()
}

func (m *Manager) isAdminDisableGen(gen uint64) bool {
	if gen == 0 {
		return m.AdminDisabled()
	}
	return m.AdminDisabled() && m.policyGen.Load() == gen
}

func (m *Manager) enforceAdminDisableOnConn(ctx context.Context, sc *sharedConn, gen uint64) {
	sc.mu.Lock()
	client := sc.client
	hello := sc.hello
	sshClient := sc.sshClient
	refs := make([]*hostRef, 0, len(sc.hostRefs))
	hostIDs := make([]int64, 0, len(sc.hostRefs))
	for hid, r := range sc.hostRefs {
		hostIDs = append(hostIDs, hid)
		if r != nil {
			cp := *r
			refs = append(refs, &cp)
		}
	}
	key := sc.key
	sc.mu.Unlock()

	defer func() {
		m.closeSharedConn(sc, false)
		if len(hostIDs) > 0 {
			m.notifyCoverageChanged(hostIDs)
		}
	}()

	if m.cfg.BeforeAdminDisableCheck != nil && len(hostIDs) > 0 {
		m.cfg.BeforeAdminDisableCheck(hostIDs[0])
	}

	if !m.isAdminDisableGen(gen) {
		return
	}

	exclusive := m.enforceAdminDisableOnClient(ctx, client, hello, refs, hostIDs, key, gen, "")
	if !exclusive {
		return
	}
	if !m.isAdminDisableGen(gen) {
		return
	}

	// Close the daemon RPC stream before uninstalling so the daemon exits cleanly.
	if client != nil {
		_ = client.Close()
	}
	if !m.isAdminDisableGen(gen) {
		return
	}

	// Immediately borrow the existing SSH connection (sc.sshClient or pooled SSH
	// from m.ssh) and run host-side atomic sole-gateway uninstall under lock.
	m.tryImmediateAdminUninstall(ctx, key, refs, hostIDs, sshClient, gen, "")
}

// enforceAdminDisableOnClient atomically requests conditional shutdown on the
// daemon (ShutdownParams.OnlyIfSoleGateway) ONLY after confirming the daemon
// advertises proto.FeatureConditionalShutdown in HelloResult.Features.
//
//   - Before every RPC and before every DB write (under policyMu.RLock), re-verifies
//     m.isAdminDisableGen(gen) so an obsolete disable worker superseded by a
//     subsequent "re-allow" can never shut down a new connection or overwrite DB state.
//   - Old daemons without FeatureConditionalShutdown: never sent unconditional
//     Shutdown; marked stopped (files preserved).
//   - Live Health/Hello failure: never falls back to cached health or unconditional
//     shutdown; leaves daemon running and marks stopped.
//   - Conditional Shutdown transport/timeout error (response lost): treated as
//     outcome unknown — marks remove_pending + markPendingCleanup so host-side
//     atomic retry resolves whether to clean up or preserve.
//   - Confirmed other gateway (Health.ConnectedGateways > 1 or ShutdownResult.OtherGateways):
//     marked stopped (files preserved).
//   - Confirmed sole gateway (atomic ShutdownConditional OK): daemon enters
//     shuttingDown (rejecting new Hello) and host is marked remove_pending.
func (m *Manager) enforceAdminDisableOnClient(ctx context.Context, client *Client, hello *proto.HelloResult, refs []*hostRef, hostIDs []int64, key accountKey, gen uint64, heldOpID string) bool {
	if ctx == nil {
		ctx = context.Background()
	}
	writeIfActive := func(fn func()) bool {
		if m.store == nil {
			return true
		}
		m.policyMu.RLock()
		defer m.policyMu.RUnlock()
		if !m.isAdminDisableGen(gen) {
			return false
		}
		fn()
		return true
	}

	markCheckFailed := func() {
		if heldOpID != "" {
			// Item #3: When called from an in-flight install/update op (holding
			// heldOpID), never leave the host row in 'installing' with active_op_id
			// dangling (where outer defer would reset it to 'off') or drop retry:
			// record remove_pending and arm hasPendingCleanup.
			if writeIfActive(func() {
				_ = m.store.MarkCorraldHostsRemovePendingIfOp(context.Background(), hostIDs, key.Fingerprint, key.Username, heldOpID)
			}) {
				m.markPendingCleanup()
			}
			return
		}
		writeIfActive(func() {
			_ = m.store.MarkCorraldHostsStoppedIfOp(context.Background(), hostIDs, key.Fingerprint, key.Username, heldOpID)
		})
	}

	if !m.isAdminDisableGen(gen) {
		return false
	}
	if client == nil || client.Closed() || m.gatewayID == "" {
		markCheckFailed()
		return false
	}

	// Verify live daemon health first (never fall back to cached sc.health).
	hctx, hcancel := context.WithTimeout(ctx, 3*time.Second)
	h, herr := client.Health(hctx)
	hcancel()
	if !m.isAdminDisableGen(gen) {
		return false
	}
	if herr != nil || h == nil {
		markCheckFailed()
		return false
	}
	if h.ConnectedGateways > 1 {
		if writeIfActive(func() {
			_ = m.store.MarkCorraldHostsPreservedOtherGatewayIfOp(context.Background(), hostIDs, key.Fingerprint, key.Username, heldOpID)
		}) {
			m.refreshPendingCleanupFlag()
		}
		return false
	}

	// Verify daemon supports conditional shutdown via HelloResult.Features.
	// If hello was not cached yet, fetch it once without downgrading to
	// unconditional shutdown on older daemons.
	if hello == nil {
		if !m.isAdminDisableGen(gen) {
			return false
		}
		var ident proto.ClientIdentity
		if len(refs) > 0 {
			ident = proto.ClientIdentity{
				CorralUser: refs[0].CorralUser,
				HostID:     strconv.FormatInt(refs[0].HostID, 10),
			}
		}
		helloCtx, helloCancel := context.WithTimeout(ctx, 3*time.Second)
		freshHello, err := client.Hello(helloCtx, proto.HelloParams{
			Gateway:        m.gatewayID,
			GatewayVersion: m.gatewayVersion,
			ProtoMin:       m.protoMin,
			ProtoMax:       m.protoMax,
			Client:         ident,
		})
		helloCancel()
		if !m.isAdminDisableGen(gen) {
			return false
		}
		if err != nil || freshHello == nil {
			markCheckFailed()
			return false
		}
		hello = freshHello
	}
	if !hello.HasFeature(proto.FeatureConditionalShutdown) {
		// Older daemon cannot perform atomic sole-gateway shutdown: only
		// disconnect, mark stopped, and preserve files.
		if writeIfActive(func() {
			_ = m.store.MarkCorraldHostsPreservedOtherGatewayIfOp(context.Background(), hostIDs, key.Fingerprint, key.Username, heldOpID)
		}) {
			m.refreshPendingCleanupFlag()
		}
		return false
	}

	// Re-check policy generation right before sending conditional shutdown so an
	// obsolete worker superseded by a re-allow never sends ShutdownConditional.
	if !m.isAdminDisableGen(gen) {
		return false
	}

	// Atomically check exclusivity AND enter shuttingDown inside the daemon
	// under clientGateMu.
	sctx, scancel := context.WithTimeout(ctx, 3*time.Second)
	sres, serr := client.ShutdownConditional(sctx, "admin_disabled", m.gatewayID)
	scancel()
	if !m.isAdminDisableGen(gen) {
		return false
	}
	if serr != nil || sres == nil {
		// Response lost (EOF/disconnect/timeout): the daemon may have accepted
		// shutdown and exited before flushing the reply. Treat as unknown and
		// record remove_pending so host-side atomic sole-gateway retry finishes
		// cleanup (or preserves if another gateway is actually present).
		if writeIfActive(func() {
			_ = m.store.MarkCorraldHostsRemovePendingIfOp(context.Background(), hostIDs, key.Fingerprint, key.Username, heldOpID)
		}) {
			m.markPendingCleanup()
		}
		return false
	}
	if sres.OtherGateways || !sres.OK {
		if writeIfActive(func() {
			_ = m.store.MarkCorraldHostsPreservedOtherGatewayIfOp(context.Background(), hostIDs, key.Fingerprint, key.Username, heldOpID)
		}) {
			m.refreshPendingCleanupFlag()
		}
		return false
	}

	if !writeIfActive(func() {
		_ = m.store.MarkCorraldHostsRemovePendingIfOp(context.Background(), hostIDs, key.Fingerprint, key.Username, heldOpID)
	}) {
		return false
	}
	m.markPendingCleanup()
	return true
}

// acquireCleanupOpLock registers an exclusive per-account OpRemove operation
// lock so background cleanup never races with install, update, remove, or
// another cleanup on any host record sharing the same (fingerprint, sshUser).
func (m *Manager) acquireCleanupOpLock(hostID int64, acct accountKey) (*runningOp, bool) {
	opID := newHexID()
	rop := &runningOp{
		opID:   opID,
		hostID: hostID,
		acct:   acct,
		kind:   OpRemove,
		step:   "remove",
		done:   make(chan struct{}),
	}
	if err := m.registerRunningOp(rop); err != nil {
		return nil, false
	}
	return rop, true
}

func (m *Manager) releaseCleanupOpLock(rop *runningOp) {
	if rop == nil {
		return
	}
	close(rop.done)
	m.clearRunningOp(rop)
}

// tryImmediateAdminUninstall borrows an existing SSH connection (preferring
// scSSH, falling back to m.acquireMatchingSSH which reuses open terminal pool
// sessions first) and runs host-side atomic sole-gateway uninstall under the
// per-account operation lock. When heldOpID is non-empty, the caller already
// holds the account's runningOp lock and this function reuses it.
func (m *Manager) tryImmediateAdminUninstall(
	ctx context.Context,
	key accountKey,
	refs []*hostRef,
	hostIDs []int64,
	scSSH *ssh.Client,
	gen uint64,
	heldOpID string,
) {
	if ctx == nil {
		ctx = context.Background()
	}
	if !m.isAdminDisableGen(gen) {
		return
	}
	primaryHostID := int64(0)
	if len(hostIDs) > 0 {
		primaryHostID = hostIDs[0]
	}
	opID := heldOpID
	if heldOpID == "" {
		rop, ok := m.acquireCleanupOpLock(primaryHostID, key)
		if !ok {
			return
		}
		defer m.releaseCleanupOpLock(rop)
		opID = rop.opID
	}

	uctx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()

	var (
		activeSSH *ssh.Client
		probe     *HostProbe
		release   func()
	)
	defer func() {
		if release != nil {
			release()
		}
	}()

	if scSSH != nil {
		if p, err := m.probeHost(uctx, scSSH); err == nil && p != nil {
			activeSSH = scSSH
			probe = p
		}
	}
	if activeSSH == nil && m.ssh != nil && len(refs) > 0 {
		if c, rel, err := m.acquireMatchingSSH(uctx, key, refs); err == nil && c != nil {
			release = rel
			if p, perr := m.probeHost(uctx, c); perr == nil && p != nil {
				activeSSH = c
				probe = p
			}
		}
	}
	if activeSSH == nil || probe == nil {
		// Transient SSH/probe failure: keep remove_pending for retry.
		return
	}
	if !m.isAdminDisableGen(gen) {
		return
	}

	// Re-check DB state/binding right before running host-side uninstall.
	if m.store != nil && primaryHostID > 0 {
		fresh, err := m.store.CorraldHostByID(uctx, primaryHostID)
		if err != nil || fresh.State == StateOff || !CheckBinding(toSnapshot(fresh), key.Fingerprint, key.Username, m.gatewayID) {
			return
		}
		if fresh.ActiveOpID != "" && fresh.ActiveOpID != heldOpID {
			return
		}
	}

	out := RunUninstallSoleGatewayWithRetry(uctx, activeSSH, m.Dist(), probe, opID, m.gatewayID, m.busyRetry, nil)
	m.applySoleGatewayUninstallOutcome(key.Fingerprint, key.Username, hostIDs, true, out, gen, heldOpID)
}

// applySoleGatewayUninstallOutcome updates DB state after host-side atomic
// sole-gateway uninstall, strictly distinguishing confirmed other_gateways /
// unsupported (marked stopped, files preserved) from transient failures
// (which preserve remove_pending for retry). When disableGen > 0, checks
// m.isAdminDisableGen(disableGen) under policyMu.RLock before writing DB.
func (m *Manager) applySoleGatewayUninstallOutcome(fp, sshUser string, hostIDs []int64, wasPending bool, out UninstallOutcome, disableGen uint64, heldOpID string) {
	if m.store == nil {
		return
	}
	if disableGen > 0 {
		m.policyMu.RLock()
		if !m.isAdminDisableGen(disableGen) {
			m.policyMu.RUnlock()
			return
		}
		defer m.policyMu.RUnlock()
	}
	switch out.Result {
	case "removed":
		m.resetHostsOff(fp, sshUser, hostIDs, heldOpID)
	case "other_gateways", "unsupported":
		_ = m.store.MarkCorraldHostsPreservedOtherGatewayIfOp(context.Background(), hostIDs, fp, sshUser, heldOpID)
		m.refreshPendingCleanupFlag()
	default:
		// Transient/incomplete failure (pending, partial, stop_failed, busy):
		// keep remove_pending so subsequent connections or host uses retry.
		if wasPending {
			m.markPendingCleanup()
		} else {
			_ = m.store.MarkCorraldHostsRemovePendingIfOp(context.Background(), hostIDs, fp, sshUser, heldOpID)
			m.markPendingCleanup()
		}
	}
}

func (m *Manager) resetHostsOff(fp, sshUser string, hostIDs []int64, allowedOpID string) {
	if m.store == nil {
		return
	}
	ctx := context.Background()
	if fp != "" && sshUser != "" {
		_ = m.store.CorraldResetByBindingIfIdle(ctx, fp, sshUser, allowedOpID)
	}
	for _, hid := range hostIDs {
		_ = m.store.ResetCorraldHostOffIfIdle(ctx, hid, allowedOpID)
	}
	m.refreshPendingCleanupFlag()
}

// markPendingCleanup atomically advances the pending-flag generation and marks
// hasPendingCleanup = true so a concurrent stale refresh cannot clear it.
func (m *Manager) markPendingCleanup() {
	if m == nil {
		return
	}
	m.pendingFlagMu.Lock()
	m.pendingFlagGen++
	m.hasPendingCleanup.Store(true)
	m.pendingFlagMu.Unlock()
}

// refreshPendingCleanupFlag queries SQLite and publishes the result ONLY if no
// newer markPendingCleanup() occurred while the query was in flight.
func (m *Manager) refreshPendingCleanupFlag() {
	if m == nil || m.store == nil {
		return
	}
	m.pendingFlagMu.Lock()
	gen := m.pendingFlagGen
	m.pendingFlagMu.Unlock()

	has, err := m.store.HasCorraldHostsInStates(context.Background(), StateRemovePending, StateRemoving)
	if err != nil {
		return
	}
	m.pendingFlagMu.Lock()
	if m.pendingFlagGen == gen {
		m.hasPendingCleanup.Store(has)
	}
	m.pendingFlagMu.Unlock()
}

// MaybeRetryPendingCleanup triggers a rate-limited background cleanup retry for
// hostID's account when any host is still in remove_pending/removing, reusing
// sshClient (or borrowing an open pooled SSH connection). Deduplication is
// scoped by (fingerprint, sshUser) account and acquires the per-account op lock.
func (m *Manager) MaybeRetryPendingCleanup(userID, hostID int64, corralUser string, sshClient *ssh.Client) {
	if m == nil || m.store == nil || hostID <= 0 || !m.hasPendingCleanup.Load() {
		return
	}

	// Resolve account key for account-level dedup (fallback to hostID if unknown).
	var acct accountKey
	if m.ssh != nil && userID > 0 {
		if u, fp, err := m.ssh.HostBinding(context.Background(), userID, hostID); err == nil && fp != "" && u != "" {
			acct = accountKey{Fingerprint: fp, Username: u}
		}
	}

	now := time.Now()
	m.pendingRetryMu.Lock()
	if acct.valid() {
		if m.pendingRetryAcctActive[acct] || (!m.pendingRetryAcctAt[acct].IsZero() && now.Sub(m.pendingRetryAcctAt[acct]) < pendingCleanupRetryCooldown) {
			m.pendingRetryMu.Unlock()
			return
		}
		m.pendingRetryAcctActive[acct] = true
		m.pendingRetryAcctAt[acct] = now
	} else {
		if m.pendingRetryActive[hostID] || (!m.pendingRetryAt[hostID].IsZero() && now.Sub(m.pendingRetryAt[hostID]) < pendingCleanupRetryCooldown) {
			m.pendingRetryMu.Unlock()
			return
		}
		m.pendingRetryActive[hostID] = true
		m.pendingRetryAt[hostID] = now
	}
	m.pendingRetryMu.Unlock()

	clearDedup := func() {
		m.pendingRetryMu.Lock()
		if acct.valid() {
			delete(m.pendingRetryAcctActive, acct)
		} else {
			delete(m.pendingRetryActive, hostID)
		}
		m.pendingRetryMu.Unlock()
	}

	m.policyMu.RLock()
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		m.policyMu.RUnlock()
		clearDedup()
		return
	}
	m.wg.Add(1)
	m.mu.Unlock()
	m.policyMu.RUnlock()

	go func() {
		defer m.wg.Done()
		defer clearDedup()

		baseCtx := m.bgCtx
		if baseCtx == nil {
			baseCtx = context.Background()
		}
		ctx, cancel := context.WithTimeout(baseCtx, 25*time.Second)
		defer cancel()

		ch, err := m.store.CorraldHostByID(ctx, hostID)
		if err != nil {
			return
		}
		if ch.State != StateRemovePending && ch.State != StateRemoving {
			m.refreshPendingCleanupFlag()
			return
		}

		var fp, sshUser string
		if m.ssh != nil && userID > 0 {
			sshUser, fp, _ = m.ssh.HostBinding(ctx, userID, hostID)
		}
		if fp == "" {
			fp = ch.BoundFingerprint
		}
		if sshUser == "" {
			sshUser = ch.BoundUsername
		}
		if !CheckBinding(toSnapshot(ch), fp, sshUser, m.gatewayID) {
			return
		}
		key := accountKey{Fingerprint: fp, Username: sshUser}
		rop, ok := m.acquireCleanupOpLock(hostID, key)
		if !ok {
			return
		}
		defer m.releaseCleanupOpLock(rop)

		if m.cfg.BeforePendingCleanupSSH != nil {
			m.cfg.BeforePendingCleanupSSH(hostID)
		}

		activeSSH := sshClient
		var release func()
		if activeSSH == nil && m.ssh != nil && userID > 0 {
			c, rel, gotFP, gotUser, aerr := m.ssh.AcquireSSH(ctx, userID, hostID)
			if aerr != nil || c == nil {
				return
			}
			if (gotFP != "" && fp != "" && gotFP != fp) || (gotUser != "" && sshUser != "" && gotUser != sshUser) {
				if rel != nil {
					rel()
				}
				return
			}
			activeSSH = c
			release = rel
		}
		if release != nil {
			defer release()
		}
		if activeSSH == nil {
			return
		}
		if corralUser == "" && m.ssh != nil && userID > 0 {
			corralUser = m.ssh.UserLabel(ctx, userID)
		}
		m.runSafeCleanupLocked(ctx, userID, hostID, corralUser, fp, sshUser, activeSSH, rop.opID, true, 0, "")
	}()
}

// reconcileRemovePendingOnAllow runs in the background (concurrency cap 4)
// after the administrator switches the policy back to "allowed": each
// remove_pending/removing host acquires the per-account op lock and re-reads
// DB state before touching SSH so a newly re-enabled installation is never
// disrupted. Never auto-reinstalls.
func (m *Manager) reconcileRemovePendingOnAllow(gen uint64) {
	if m == nil || m.store == nil || m.ssh == nil {
		return
	}
	baseCtx := m.bgCtx
	if baseCtx == nil {
		baseCtx = context.Background()
	}
	items, err := m.store.ListCorraldHostsByStates(baseCtx, StateRemovePending, StateRemoving)
	if err != nil || len(items) == 0 {
		m.refreshPendingCleanupFlag()
		return
	}

	// Deduplicate by account so multiple host records for the same account do
	// not contend needlessly.
	seenAcct := make(map[accountKey]struct{})
	var unique []store.CorraldHostWithOwner
	for _, it := range items {
		k := accountKey{Fingerprint: it.Host.BoundFingerprint, Username: it.Host.BoundUsername}
		if k.valid() {
			if _, dup := seenAcct[k]; dup {
				continue
			}
			seenAcct[k] = struct{}{}
		}
		unique = append(unique, it)
	}

	sem := make(chan struct{}, adminDisableConcurrency)
	var wg sync.WaitGroup
	for _, item := range unique {
		if m.AdminDisabled() || m.policyGen.Load() != gen {
			break
		}
		sem <- struct{}{}
		wg.Add(1)
		go func(it store.CorraldHostWithOwner) {
			defer wg.Done()
			defer func() { <-sem }()
			if m.AdminDisabled() || m.policyGen.Load() != gen {
				return
			}
			ctx, cancel := context.WithTimeout(baseCtx, 25*time.Second)
			defer cancel()

			sshUser, fp, _ := m.ssh.HostBinding(ctx, it.OwnerID, it.HostID)
			if fp == "" {
				fp = it.Host.BoundFingerprint
			}
			if sshUser == "" {
				sshUser = it.Host.BoundUsername
			}
			key := accountKey{Fingerprint: fp, Username: sshUser}
			rop, ok := m.acquireCleanupOpLock(it.HostID, key)
			if !ok {
				return
			}
			defer m.releaseCleanupOpLock(rop)

			if m.cfg.BeforePendingCleanupSSH != nil {
				m.cfg.BeforePendingCleanupSSH(it.HostID)
			}

			// Re-verify fresh DB state BEFORE dialing SSH.
			fresh, ferr := m.store.CorraldHostByID(ctx, it.HostID)
			if ferr != nil || (fresh.State != StateRemovePending && fresh.State != StateRemoving) {
				return
			}
			if !CheckBinding(toSnapshot(fresh), fp, sshUser, m.gatewayID) {
				return
			}

			c, rel, gotFP, gotUser, aerr := m.ssh.AcquireSSH(ctx, it.OwnerID, it.HostID)
			if aerr != nil || c == nil {
				return
			}
			defer func() {
				if rel != nil {
					rel()
				}
			}()
			if (gotFP != "" && fp != "" && gotFP != fp) || (gotUser != "" && sshUser != "" && gotUser != sshUser) {
				return
			}
			if m.AdminDisabled() || m.policyGen.Load() != gen {
				return
			}
			m.runSafeCleanupLocked(ctx, it.OwnerID, it.HostID, it.CorralUser, fp, sshUser, c, rop.opID, true, gen, "")
		}(item)
	}
	wg.Wait()
	m.refreshPendingCleanupFlag()
}

// handleAdminDisabledOnConnect enforces safe host-side sole-gateway cleanup
// when an SSH connection arrives while corrald is disabled (or for a host
// remaining in remove_pending/removing). It acquires the per-account op lock
// and delegates to runSafeCleanupLocked.
func (m *Manager) handleAdminDisabledOnConnect(
	ctx context.Context,
	userID, hostID int64,
	corralUser, fp, sshUser string,
	sshClient *ssh.Client,
	ch *store.CorraldHost,
) {
	if m == nil || sshClient == nil || ch == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	key := accountKey{Fingerprint: fp, Username: sshUser}
	rop, ok := m.acquireCleanupOpLock(hostID, key)
	if !ok {
		return
	}
	defer m.releaseCleanupOpLock(rop)

	requirePending := !m.AdminDisabled()
	m.runSafeCleanupLocked(ctx, userID, hostID, corralUser, fp, sshUser, sshClient, rop.opID, requirePending, 0, "")
}

// handleAdminDisabledWhileHoldingOp enforces safe host-side sole-gateway
// cleanup when admin-disable fires mid-operation (e.g. inside runInstallWithSSH
// or attachAndHello while the caller ALREADY holds the account's runningOp lock).
// Reusing heldOpID avoids deadlocking/skipping on acquireCleanupOpLock and
// guarantees the host is either cleaned to off, preserved as stopped, or
// reliably marked remove_pending + hasPendingCleanup for retry.
func (m *Manager) handleAdminDisabledWhileHoldingOp(
	ctx context.Context,
	userID, hostID int64,
	corralUser, fp, sshUser string,
	sshClient *ssh.Client,
	ch *store.CorraldHost,
	heldOpID string,
) {
	if m == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if sshClient == nil || ch == nil {
		if m.store != nil {
			_ = m.store.MarkCorraldHostsRemovePendingIfOp(context.Background(), []int64{hostID}, fp, sshUser, heldOpID)
			m.markPendingCleanup()
		}
		return
	}
	opID := heldOpID
	if opID == "" {
		opID = newHexID()
	}
	m.runSafeCleanupLocked(ctx, userID, hostID, corralUser, fp, sshUser, sshClient, opID, false, 0, heldOpID)
}

// runSafeCleanupLocked executes the host-side atomic sole-gateway uninstall
// flow while the caller holds the per-account runningOp lock.
//
// Before any destructive action (including detaching a shared connection), it
// re-reads the host row from SQLite and verifies:
//   - binding still matches (fp, sshUser, m.gatewayID),
//   - if requirePending is true, DB state is STILL remove_pending/removing
//     (so a concurrent explicit reinstall is never touched!),
//   - if allowGen > 0, policyGen has not moved and admin has not re-disabled.
func (m *Manager) runSafeCleanupLocked(
	ctx context.Context,
	userID, hostID int64,
	corralUser, fp, sshUser string,
	sshClient *ssh.Client,
	opID string,
	requirePending bool,
	allowGen uint64,
	heldOpID string,
) {
	if m == nil || sshClient == nil || m.store == nil {
		return
	}
	fresh, err := m.store.CorraldHostByID(ctx, hostID)
	if err != nil {
		return
	}
	if fresh.State == StateOff {
		m.refreshPendingCleanupFlag()
		return
	}
	if requirePending && fresh.State != StateRemovePending && fresh.State != StateRemoving {
		return
	}
	if !requirePending && !m.AdminDisabled() {
		return
	}
	if allowGen > 0 && (m.AdminDisabled() || m.policyGen.Load() != allowGen) {
		return
	}
	if !CheckBinding(toSnapshot(fresh), fp, sshUser, m.gatewayID) {
		return
	}
	if fresh.ActiveOpID != "" && fresh.ActiveOpID != heldOpID {
		return
	}

	wasPending := fresh.State == StateRemovePending || fresh.State == StateRemoving

	// Detach any lingering gateway connection for this host before uninstalling
	// (only AFTER confirming state is still eligible for cleanup!).
	if sc := m.sharedConnForHost(hostID, fp, sshUser); sc != nil {
		m.mu.Lock()
		if m.conns[sc.key] == sc {
			delete(m.conns, sc.key)
		}
		delete(m.hostConn, hostID)
		m.mu.Unlock()
		m.closeSharedConn(sc, false)
	} else {
		m.detachHost(hostID)
	}

	probe, perr := m.probeHost(ctx, sshClient)
	if perr != nil || probe == nil {
		// Transient probe failure: keep remove_pending if already pending;
		// otherwise mark remove_pending so future connections/uses retry safely.
		if !wasPending {
			_ = m.store.MarkCorraldHostsRemovePendingIfOp(context.Background(), []int64{hostID}, fp, sshUser, heldOpID)
		}
		m.markPendingCleanup()
		return
	}

	files := ReadHostStateFiles(ctx, sshClient, probe, "")
	hasOnHost := probe.HasSymlink || probe.HasInstall || probe.HasTombstone || probe.HasRunDir ||
		len(probe.Versions) > 0 || len(probe.Stages) > 0 ||
		files.Install != nil || files.Meta != nil
	if !hasOnHost {
		m.resetHostsOff(fp, sshUser, []int64{hostID}, heldOpID)
		return
	}

	// Host-side atomic sole-gateway uninstall under .corrald-shared.lock ->
	// op.lock -> corrald.lock. Refuses without modifying files if another
	// gateway is connected/registered or if the host binary/daemon is too old.
	out := RunUninstallSoleGatewayWithRetry(ctx, sshClient, m.Dist(), probe, opID, m.gatewayID, m.busyRetry, nil)
	if out.Result == "removed" {
		m.recordAccountRemoval(accountKey{Fingerprint: fp, Username: sshUser})
	}
	m.applySoleGatewayUninstallOutcome(fp, sshUser, []int64{hostID}, wasPending, out, 0, heldOpID)
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
	beforeMin, beforeMax, afterMin := PerfCompareDateBounds(consentAt)
	before, err := m.store.CorraldPerfDailyTotals(ctx, hostID, "ssh", beforeMin, beforeMax)
	if err != nil {
		return nil
	}
	after, err := m.store.CorraldPerfDailyTotals(ctx, hostID, "corrald", afterMin, "")
	if err != nil {
		return nil
	}
	return ComputePerfComparison(now, consentAt, before, after)
}
