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
	"github.com/ihainan/corral/internal/sshx"
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
// concurrency (adminDisableConcurrency = 4).
//
// When disabled becomes true, it takes effect immediately:
//   - All capability routing (ClientForCap, watch) immediately falls back to SSH.
//   - Any in-progress install/update operations are cancelled and active
//     connections are detached immediately.
//   - For each previously connected host, a background worker verifies
//     exclusivity (live Health + Hello):
//   - If confirmed sole gateway, it stops corrald via daemon.shutdown and
//     immediately borrows the existing SSH connection to uninstall files
//     (transitioning directly to off). Only if immediate cleanup fails does the
//     host remain in remove_pending for retry on next connection or next host use.
//   - If another gateway is connected/registered OR live checks fail, only this
//     gateway disconnects — the daemon keeps running and files are preserved
//     (host marked stopped).
//
// When disabled becomes false, previously cleaned hosts remain off (never
// auto-reinstalled); any hosts still in remove_pending are reconciled in the
// background (reset to off if files are already gone or exclusive cleanup
// finishes, or marked stopped if another gateway is using them).
func (m *Manager) SetAdminDisabled(ctx context.Context, disabled bool) {
	if m == nil {
		return
	}
	gen := m.policyGen.Add(1)
	if !disabled {
		m.policyMu.Lock()
		m.adminDisabled.Store(false)
		m.policyMu.Unlock()

		m.mu.Lock()
		if !m.closed && m.store != nil {
			m.wg.Add(1)
			m.mu.Unlock()
			go func() {
				defer m.wg.Done()
				m.reconcileRemovePendingOnAllow(gen)
			}()
		} else {
			m.mu.Unlock()
		}
		return
	}

	// Mark disabled first so new starts/attaches bail immediately, then take
	// policyMu.Lock to wait for any in-flight publish+DB-running section to
	// finish before snapshotting and draining active connections.
	m.adminDisabled.Store(true)
	m.policyMu.Lock()
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

	exclusive := m.enforceAdminDisableOnClient(ctx, client, hello, refs, hostIDs, key)
	if !exclusive {
		return
	}

	// Close the daemon RPC stream before uninstalling so the daemon can exit cleanly.
	if client != nil {
		_ = client.Close()
	}
	if !m.isAdminDisableGen(gen) {
		return
	}

	// Immediately borrow the existing SSH connection (sc.sshClient or pooled SSH
	// from m.ssh) and uninstall files so the host transitions straight to off
	// instead of remaining stuck in remove_pending on pooled connections.
	m.tryImmediateAdminUninstall(ctx, key, refs, hostIDs, sshClient, gen)
}

// enforceAdminDisableOnClient applies the shared-daemon safety rule to a live
// client: it ONLY calls daemon.shutdown and marks remove_pending when a fresh
// Health RPC succeeds with connected_gateways <= 1 and no other gateway is
// registered (verified against both hello and live ClientRegistry). If Health
// fails or cannot confirm exclusivity, it NEVER falls back to cached health —
// it leaves the daemon running and marks hosts stopped.
// Returns true iff exclusivity was confirmed and daemon.shutdown was sent.
func (m *Manager) enforceAdminDisableOnClient(ctx context.Context, client *Client, hello *proto.HelloResult, refs []*hostRef, hostIDs []int64, key accountKey) bool {
	if ctx == nil {
		ctx = context.Background()
	}
	otherGateways := true
	if client != nil && !client.Closed() && m.gatewayID != "" {
		hctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		h, err := client.Health(hctx)
		cancel()
		if err == nil && h != nil {
			otherGateways = h.ConnectedGateways > 1
			if !otherGateways && hello != nil && clientsHaveOtherGateway(hello.Clients, m.gatewayID) {
				otherGateways = true
			}
			// Also refresh the live registered client list via daemon.hello so
			// another gateway that registered after our initial Hello (even if
			// currently disconnected) is never missed.
			if !otherGateways {
				var ident proto.ClientIdentity
				if len(refs) > 0 {
					ident = proto.ClientIdentity{
						CorralUser: refs[0].CorralUser,
						HostID:     strconv.FormatInt(refs[0].HostID, 10),
					}
				}
				helloCtx, helloCancel := context.WithTimeout(ctx, 3*time.Second)
				freshHello, herr := client.Hello(helloCtx, proto.HelloParams{
					Gateway:        m.gatewayID,
					GatewayVersion: m.gatewayVersion,
					ProtoMin:       m.protoMin,
					ProtoMax:       m.protoMax,
					Client:         ident,
				})
				helloCancel()
				if herr != nil || freshHello == nil || clientsHaveOtherGateway(freshHello.Clients, m.gatewayID) {
					otherGateways = true
				}
			}
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
			m.hasPendingCleanup.Store(true)
		}
		return true
	}
	if m.store != nil {
		_ = m.store.MarkCorraldHostsStopped(context.Background(), hostIDs, key.Fingerprint, key.Username)
	}
	return false
}

// tryImmediateAdminUninstall borrows an existing SSH connection (preferring
// scSSH, falling back to m.acquireMatchingSSH which reuses open terminal pool
// sessions first) to remove corrald files immediately after Shutdown.
// On failure, hosts stay in remove_pending for retry on next connection or use.
func (m *Manager) tryImmediateAdminUninstall(
	ctx context.Context,
	key accountKey,
	refs []*hostRef,
	hostIDs []int64,
	scSSH *ssh.Client,
	gen uint64,
) {
	if ctx == nil {
		ctx = context.Background()
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
		return
	}

	files := ReadHostStateFiles(uctx, activeSSH, probe, "")
	hasOnHost := probe.HasSymlink || probe.HasInstall || probe.HasTombstone ||
		len(probe.Versions) > 0 || len(probe.Stages) > 0 ||
		files.Install != nil || files.Meta != nil
	if !hasOnHost {
		m.resetHostsOff(key.Fingerprint, key.Username, hostIDs)
		return
	}

	// Re-verify clients.json on disk before removing files so another gateway
	// that registered in the narrow window after Shutdown is never disrupted.
	if !files.ClientsLoaded || clientsHaveOtherGateway(files.Clients, m.gatewayID) {
		if m.store != nil {
			_ = m.store.MarkCorraldHostsStopped(context.Background(), hostIDs, key.Fingerprint, key.Username)
		}
		m.refreshPendingCleanupFlag()
		return
	}
	if !m.isAdminDisableGen(gen) {
		return
	}

	out := RunUninstallWithRetry(uctx, activeSSH, probe, newHexID(), m.busyRetry, nil)
	if out.Result == "removed" {
		m.resetHostsOff(key.Fingerprint, key.Username, hostIDs)
	}
}

func (m *Manager) resetHostsOff(fp, sshUser string, hostIDs []int64) {
	if m.store == nil {
		return
	}
	ctx := context.Background()
	if fp != "" && sshUser != "" {
		_ = m.store.CorraldResetByBinding(ctx, fp, sshUser)
	}
	for _, hid := range hostIDs {
		_ = m.store.ResetCorraldHostOff(ctx, hid)
	}
	m.refreshPendingCleanupFlag()
}

func (m *Manager) refreshPendingCleanupFlag() {
	if m == nil || m.store == nil {
		return
	}
	has, err := m.store.HasCorraldHostsInStates(context.Background(), StateRemovePending, StateRemoving)
	if err == nil {
		m.hasPendingCleanup.Store(has)
	}
}

// MaybeRetryPendingCleanup triggers a rate-limited background cleanup retry for
// hostID when any host is still in remove_pending/removing, reusing sshClient
// (or borrowing an open pooled SSH connection) so cleanup does not wait for a
// brand-new SSH connection.
func (m *Manager) MaybeRetryPendingCleanup(userID, hostID int64, corralUser string, sshClient *ssh.Client) {
	if m == nil || m.store == nil || hostID <= 0 || !m.hasPendingCleanup.Load() {
		return
	}
	now := time.Now()
	m.pendingRetryMu.Lock()
	if m.pendingRetryActive[hostID] || (!m.pendingRetryAt[hostID].IsZero() && now.Sub(m.pendingRetryAt[hostID]) < pendingCleanupRetryCooldown) {
		m.pendingRetryMu.Unlock()
		return
	}
	m.pendingRetryActive[hostID] = true
	m.pendingRetryAt[hostID] = now
	m.pendingRetryMu.Unlock()

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		m.pendingRetryMu.Lock()
		delete(m.pendingRetryActive, hostID)
		m.pendingRetryMu.Unlock()
		return
	}
	m.wg.Add(1)
	m.mu.Unlock()

	go func() {
		defer m.wg.Done()
		defer func() {
			m.pendingRetryMu.Lock()
			delete(m.pendingRetryActive, hostID)
			m.pendingRetryMu.Unlock()
		}()

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
		acct := accountKey{Fingerprint: fp, Username: sshUser}
		if m.hasRunningOp(hostID, acct) {
			return
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
		m.handleAdminDisabledOnConnect(ctx, userID, hostID, corralUser, fp, sshUser, activeSSH, ch)
	}()
}

// reconcileRemovePendingOnAllow runs in the background (concurrency cap 4)
// after the administrator switches the policy back to "allowed": any host still
// in remove_pending/removing is checked over SSH — if files are already removed
// (or exclusive cleanup finishes), state resets to off; if another gateway is
// using corrald, state becomes stopped (files preserved). Never auto-reinstalls.
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

	sem := make(chan struct{}, adminDisableConcurrency)
	var wg sync.WaitGroup
	for _, item := range items {
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
			if !CheckBinding(toSnapshot(it.Host), fp, sshUser, m.gatewayID) {
				return
			}
			acct := accountKey{Fingerprint: fp, Username: sshUser}
			if m.hasRunningOp(it.HostID, acct) {
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
			m.handleAdminDisabledOnConnect(ctx, it.OwnerID, it.HostID, it.CorralUser, fp, sshUser, c, it.Host)
		}(item)
	}
	wg.Wait()
	m.refreshPendingCleanupFlag()
}

// handleAdminDisabledOnConnect enforces the administrator disable policy when
// an SSH connection arrives for a host while corrald is disabled. It handles
// both previously marked remove_pending/removing hosts and hosts that were
// disconnected (running/stopped/lost/etc.) when the policy was switched.
//
// Safety invariant: before stopping corrald or uninstalling files, it re-checks
// exclusivity over this SSH connection (live daemon Health+Hello if running,
// otherwise clients.json after confirming the daemon is stopped). Whenever
// another gateway is present or exclusivity cannot be confirmed, it preserves
// the daemon and files and marks the host stopped.
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

	// Ensure this gateway has no lingering shared connection for this host.
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

	probe, err := m.probeHost(ctx, sshClient)
	if err != nil || probe == nil {
		// Cannot reach/probe host to confirm exclusivity -> preserve files.
		if m.store != nil {
			_ = m.store.MarkCorraldHostsStopped(context.Background(), []int64{hostID}, fp, sshUser)
		}
		return
	}

	files := ReadHostStateFiles(ctx, sshClient, probe, "")
	hasOnHost := probe.HasSymlink || probe.HasInstall || probe.HasTombstone ||
		len(probe.Versions) > 0 || len(probe.Stages) > 0 ||
		files.Install != nil || files.Meta != nil
	if !hasOnHost {
		if m.store != nil {
			_ = m.store.CorraldResetByBinding(context.Background(), fp, sshUser)
			if ch.State != StateOff {
				ch.State = StateOff
				ch.Version = ""
				ch.Release = 0
				ch.Files = []string{}
				ch.ActiveOpID = ""
				ch.LastError = ""
				activeOp, _ := m.store.ActiveCorraldOp(ctx, hostID)
				if activeOp != nil {
					activeOp.State = OpDone
					activeOp.Detail = encodeOpDetail(opDetailPayload{Result: "removed"})
				}
				_, _ = m.store.UpdateCorraldHostForReconcile(ctx, ch, activeOp, ch.ActiveOpID)
			}
		}
		return
	}

	exclusive := m.confirmExclusiveAndStopOnHost(ctx, hostID, corralUser, sshClient, probe, files)
	if !exclusive {
		// Another gateway is registered/connected or exclusivity could not be
		// confirmed: preserve daemon & files, mark stopped, and disconnect.
		if m.store != nil {
			_ = m.store.MarkCorraldHostsStopped(context.Background(), []int64{hostID}, fp, sshUser)
			m.refreshPendingCleanupFlag()
		}
		return
	}

	// Confirmed this gateway is the sole user and daemon is stopped: mark
	// remove_pending and run the existing reconcileHost uninstall flow.
	if m.store != nil {
		_ = m.store.MarkCorraldHostsRemovePending(context.Background(), []int64{hostID}, fp, sshUser)
		m.hasPendingCleanup.Store(true)
	}
	ch.State = StateRemovePending
	activeOp, _ := m.store.ActiveCorraldOp(ctx, hostID)
	m.reconcileHost(ctx, userID, hostID, corralUser, fp, sshUser, sshClient, ch, activeOp)
	m.refreshPendingCleanupFlag()
}

// confirmExclusiveAndStopOnHost verifies over sshClient that no gateway other
// than m.gatewayID uses the host's corrald installation. If the daemon is
// running and exclusive, it stops it via daemon.shutdown before returning true.
// On any ambiguity (unreachable daemon, unreadable/corrupt clients.json, status
// failure, empty gatewayID), it returns false.
func (m *Manager) confirmExclusiveAndStopOnHost(
	ctx context.Context,
	hostID int64,
	corralUser string,
	sshClient *ssh.Client,
	probe *HostProbe,
	files HostStateFiles,
) bool {
	if m.gatewayID == "" || sshClient == nil || probe == nil {
		return false
	}

	sock := probe.SocketPath
	if files.Meta != nil && files.Meta.SocketPath != "" {
		sock = files.Meta.SocketPath
	}

	// 1. If a socket path is known, try connecting directly to check live state.
	if sock != "" {
		dctx, dcancel := context.WithTimeout(ctx, 5*time.Second)
		tmpClient, derr := Dial(dctx, sshClient, sock, nil)
		dcancel()
		if derr == nil {
			defer tmpClient.Close()

			hctx, hcancel := context.WithTimeout(ctx, 3*time.Second)
			hello, herr := tmpClient.Hello(hctx, proto.HelloParams{
				Gateway:        m.gatewayID,
				GatewayVersion: m.gatewayVersion,
				ProtoMin:       m.protoMin,
				ProtoMax:       m.protoMax,
				Client: proto.ClientIdentity{
					CorralUser: corralUser,
					HostID:     strconv.FormatInt(hostID, 10),
				},
			})
			hcancel()
			if herr != nil || hello == nil {
				return false
			}

			healthCtx, healthCancel := context.WithTimeout(ctx, 3*time.Second)
			health, healthErr := tmpClient.Health(healthCtx)
			healthCancel()
			if healthErr != nil || health == nil {
				return false
			}

			if health.ConnectedGateways > 1 || clientsHaveOtherGateway(hello.Clients, m.gatewayID) {
				return false
			}

			// Confirmed sole gateway on live daemon: stop it cleanly.
			sctx, scancel := context.WithTimeout(ctx, 3*time.Second)
			_, serr := tmpClient.Shutdown(sctx, "admin_disabled")
			scancel()
			if serr != nil {
				return false
			}
			return true
		}
	}

	// 2. Could not connect to a live daemon; confirm it is actually stopped
	// before trusting clients.json on disk.
	if probe.HasSymlink {
		sctx, scancel := context.WithTimeout(ctx, 5*time.Second)
		sres, serr := runSSH(sctx, sshClient, StatusCommand(probe.CurrentBin), 8192, sshx.ExecOptions{})
		scancel()
		if serr != nil && !sres.Ended {
			return false
		}
		sf := ParseKeyValues(sres.Out)
		if sf["state"] != proto.StateStopped && sf["state"] != "not_installed" {
			return false
		}
	}

	// 3. Daemon is not running: require a readable clients.json (or confirmed
	// absence) with no other gateway registered.
	if !files.ClientsLoaded {
		return false
	}
	if clientsHaveOtherGateway(files.Clients, m.gatewayID) {
		return false
	}
	return true
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
