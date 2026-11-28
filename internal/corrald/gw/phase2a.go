package gw

import (
	"context"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/ihainan/corral/internal/corrald/proto"
	"github.com/ihainan/corral/internal/sshx"
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

// SetAdminDisabled updates the administrator policy at runtime.
//
// When disabled becomes true, it takes effect immediately:
//   - All capability routing (ClientForCap, watch) immediately falls back to SSH.
//   - Any in-progress install/update operations are cancelled.
//   - On each currently connected host:
//   - If corrald confirms via a live Health RPC that no other gateway is
//     connected (connected_gateways <= 1) and no other gateway is registered,
//     corrald is stopped via daemon.shutdown and the host is marked remove_pending
//     so its files can be re-verified and cleaned up on next SSH connection.
//   - If another gateway is connected/registered OR the live Health query fails,
//     only this gateway disconnects — the daemon keeps running and files are
//     preserved (host marked stopped).
//
// When disabled becomes false, previously cleaned hosts remain off (users must
// re-enable), while preserved hosts reconnect normally on their next connection.
func (m *Manager) SetAdminDisabled(ctx context.Context, disabled bool) {
	if m == nil {
		return
	}
	if !disabled {
		m.policyMu.Lock()
		m.adminDisabled.Store(false)
		m.policyMu.Unlock()
		return
	}

	// Mark disabled first so new starts/attaches bail immediately, then take
	// policyMu.Lock to wait for any in-flight publish+DB-running section to
	// finish before snapshotting and draining active connections.
	m.adminDisabled.Store(true)
	m.policyMu.Lock()
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
	m.policyMu.Unlock()

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
	hello := sc.hello
	hostIDs := make([]int64, 0, len(sc.hostRefs))
	for hid := range sc.hostRefs {
		hostIDs = append(hostIDs, hid)
	}
	key := sc.key
	sc.mu.Unlock()

	m.enforceAdminDisableOnClient(ctx, client, hello, hostIDs, key)

	m.closeSharedConn(sc, false)
	if len(hostIDs) > 0 {
		m.notifyCoverageChanged(hostIDs)
	}
}

// enforceAdminDisableOnClient applies the shared-daemon safety rule to a live
// client: it ONLY calls daemon.shutdown and marks remove_pending when a fresh
// Health RPC succeeds with connected_gateways <= 1 and no other gateway is
// registered. If Health fails or cannot confirm exclusivity, it NEVER falls
// back to cached health — it leaves the daemon running and marks hosts stopped.
func (m *Manager) enforceAdminDisableOnClient(ctx context.Context, client *Client, hello *proto.HelloResult, hostIDs []int64, key accountKey) {
	if ctx == nil {
		ctx = context.Background()
	}
	otherGateways := true
	if client != nil && !client.Closed() {
		hctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		h, err := client.Health(hctx)
		cancel()
		if err == nil && h != nil {
			otherGateways = h.ConnectedGateways > 1
			if !otherGateways && hello != nil && clientsHaveOtherGateway(hello.Clients, m.gatewayID) {
				otherGateways = true
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
		}
	} else {
		if m.store != nil {
			_ = m.store.MarkCorraldHostsStopped(context.Background(), hostIDs, key.Fingerprint, key.Username)
		}
	}
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
		}
		return
	}

	// Confirmed this gateway is the sole user and daemon is stopped: mark
	// remove_pending and run the existing reconcileHost uninstall flow.
	if m.store != nil {
		_ = m.store.MarkCorraldHostsRemovePending(context.Background(), []int64{hostID}, fp, sshUser)
	}
	ch.State = StateRemovePending
	activeOp, _ := m.store.ActiveCorraldOp(ctx, hostID)
	m.reconcileHost(ctx, userID, hostID, corralUser, fp, sshUser, sshClient, ch, activeOp)
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
