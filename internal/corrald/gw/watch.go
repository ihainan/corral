package gw

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/ihainan/corral/internal/corrald/proto"
)

// DefaultWatchUnsubscribeDelay is how long the gateway waits after the last
// browser stops watching any host on a sharedConn before sending agent.unwatch.
const DefaultWatchUnsubscribeDelay = 30 * time.Second

const (
	maxExtraWatchLogsPerHost = 128
	maxExtraWatchLogsPerConn = 512
	defaultWatchRPCTimeout   = 5 * time.Second
)

type watchRegistration struct {
	userID int64
	hosts  map[int64]struct{}
}

// SetWatchCallbacks registers callbacks invoked when corrald pushes an
// agent.changed hint or when push coverage changes for one or more hosts.
func (m *Manager) SetWatchCallbacks(
	onChanged func(hostIDs []int64, ev proto.AgentChangedEventData),
	onCoverage func(hostIDs []int64),
) {
	if m == nil {
		return
	}
	m.watchCbMu.Lock()
	m.onAgentChanged = onChanged
	m.onCoverageChanged = onCoverage
	m.watchCbMu.Unlock()
}

// SetWatcher registers or updates a browser watcher's watched host set and
// reconciles corrald agent.watch subscriptions for all affected connections.
func (m *Manager) SetWatcher(token any, userID int64, hostIDs []int64) {
	if m == nil || token == nil {
		return
	}
	affected := make(map[*sharedConn]struct{})
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	if prev := m.watchers[token]; prev != nil {
		for hid := range prev.hosts {
			if key, ok := m.hostConn[hid]; ok {
				if sc := m.conns[key]; sc != nil {
					affected[sc] = struct{}{}
				}
			}
		}
	}
	if len(hostIDs) == 0 {
		delete(m.watchers, token)
	} else {
		hset := make(map[int64]struct{}, len(hostIDs))
		for _, hid := range hostIDs {
			if hid > 0 {
				hset[hid] = struct{}{}
				if key, ok := m.hostConn[hid]; ok {
					if sc := m.conns[key]; sc != nil {
						affected[sc] = struct{}{}
					}
				}
			}
		}
		if len(hset) == 0 {
			delete(m.watchers, token)
		} else {
			m.watchers[token] = &watchRegistration{userID: userID, hosts: hset}
		}
	}
	m.mu.Unlock()

	for sc := range affected {
		m.scheduleWatchReconcile(sc)
	}
}

// RemoveWatcher removes a browser watcher and triggers the grace-period
// unsubscribe timer on any sharedConn that no longer has active watchers.
func (m *Manager) RemoveWatcher(token any) {
	m.SetWatcher(token, 0, nil)
}

// AddHostWatchLog records a located agent log path for hostID so that active
// agent.watch subscriptions include it even before a daemon-side locate scan.
func (m *Manager) AddHostWatchLog(hostID int64, logPath string) {
	if m == nil || hostID <= 0 || logPath == "" {
		return
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	set := m.hostExtraLogs[hostID]
	if set == nil {
		set = make(map[string]struct{})
		m.hostExtraLogs[hostID] = set
	}
	if _, exists := set[logPath]; exists {
		m.mu.Unlock()
		return
	}
	if len(set) >= maxExtraWatchLogsPerHost {
		for k := range set {
			delete(set, k)
			break
		}
	}
	set[logPath] = struct{}{}
	var sc *sharedConn
	if key, ok := m.hostConn[hostID]; ok {
		sc = m.conns[key]
	}
	m.mu.Unlock()

	if sc != nil {
		sc.mu.Lock()
		subscribed := sc.watchRemoteActive && sc.watchHealthy && sc.watchClient != nil && sc.watchClient == sc.client && !sc.client.Closed()
		sc.mu.Unlock()
		if subscribed {
			m.scheduleWatchReconcile(sc)
		}
	}
}

// IsWatchCovered reports whether hostID is currently covered by an active,
// healthy corrald agent.watch subscription for userID.
func (m *Manager) IsWatchCovered(userID, hostID int64) bool {
	client, sc, ok := m.clientAndConnForCap(userID, hostID, "watch")
	if !ok || client == nil || sc == nil {
		return false
	}
	sc.mu.Lock()
	covered := sc.watchRemoteActive && sc.watchHealthy && sc.watchClient == client
	sc.mu.Unlock()
	return covered
}

// WatchSubscribedForHost reports whether hostID's sharedConn currently has an
// active remote agent.watch subscription (including degraded poll mode; used by tests).
func (m *Manager) WatchSubscribedForHost(hostID int64) bool {
	sc := m.sharedConnForHost(hostID, "", "")
	if sc == nil {
		return false
	}
	sc.mu.Lock()
	defer sc.mu.Unlock()
	return sc.watchRemoteActive && sc.client != nil && sc.watchClient == sc.client && !sc.client.Closed()
}

func (sc *sharedConn) cancelUnsubTimerLocked() {
	if sc.watchUnsubCancel != nil {
		close(sc.watchUnsubCancel)
		sc.watchUnsubCancel = nil
	}
}

func (sc *sharedConn) clearWatchStateLocked() {
	sc.cancelUnsubTimerLocked()
	sc.watchUnsubDue = false
	sc.watchRemoteActive = false
	sc.watchHealthy = false
	sc.watchClient = nil
	sc.watchLogsSig = ""
}

// isWatchEffectivelyCoveredLocked reports whether sc currently has a live,
// healthy, non-degraded watch subscription (caller holds sc.mu).
func (sc *sharedConn) isWatchEffectivelyCoveredLocked() bool {
	return sc.client != nil &&
		!sc.client.Closed() &&
		sc.watchRemoteActive &&
		sc.watchHealthy &&
		sc.watchClient == sc.client &&
		!sc.capDegraded["watch"] &&
		isWatchHealthOK(sc.health)
}

func mergeUniqueHostIDs(lists ...[]int64) []int64 {
	total := 0
	for _, l := range lists {
		total += len(l)
	}
	if total == 0 {
		return nil
	}
	seen := make(map[int64]struct{}, total)
	out := make([]int64, 0, total)
	for _, l := range lists {
		for _, id := range l {
			if id <= 0 {
				continue
			}
			if _, ok := seen[id]; !ok {
				seen[id] = struct{}{}
				out = append(out, id)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// mutateWatchState is the unified exit for ALL mutations that affect watch
// subscription validity, watch health/degradation, client attachment/teardown,
// or host detachment on sc. It atomically applies mutate under sc.mu, tracks
// previously covered host IDs so detachment of the last host never loses its
// IDs, and guarantees OnCoverageChanged is emitted whenever coverage changes.
func (m *Manager) mutateWatchState(sc *sharedConn, extraHosts []int64, mutate func()) {
	if m == nil || sc == nil {
		return
	}
	beforeConnHosts := m.connHostIDs(sc)

	var (
		wasDegraded bool
		nowDegraded bool
		prevCovered []int64
		nextCovered []int64
		afterRefs   []int64
	)

	sc.mu.Lock()
	wasDegraded = sc.capDegraded["watch"]
	for hid := range sc.watchCoveredHosts {
		prevCovered = append(prevCovered, hid)
	}
	if mutate != nil {
		mutate()
	}
	nowCovered := sc.isWatchEffectivelyCoveredLocked()
	nowDegraded = sc.capDegraded["watch"]
	for hid := range sc.hostRefs {
		afterRefs = append(afterRefs, hid)
	}
	if nowCovered {
		if sc.watchCoveredHosts == nil {
			sc.watchCoveredHosts = make(map[int64]struct{})
		}
		clear(sc.watchCoveredHosts)
		for _, hid := range afterRefs {
			sc.watchCoveredHosts[hid] = struct{}{}
		}
		nextCovered = append(nextCovered, afterRefs...)
	} else {
		clear(sc.watchCoveredHosts)
	}
	sc.mu.Unlock()

	sort.Slice(prevCovered, func(i, j int) bool { return prevCovered[i] < prevCovered[j] })
	sort.Slice(nextCovered, func(i, j int) bool { return nextCovered[i] < nextCovered[j] })
	sort.Slice(afterRefs, func(i, j int) bool { return afterRefs[i] < afterRefs[j] })
	affected := mergeUniqueHostIDs(beforeConnHosts, prevCovered, nextCovered, afterRefs, extraHosts)
	if len(affected) == 0 {
		return
	}
	if !slices.Equal(prevCovered, nextCovered) || wasDegraded != nowDegraded || len(extraHosts) > 0 {
		m.notifyCoverageChanged(affected)
	}
}

func (m *Manager) emitAgentChanged(hostIDs []int64, ev proto.AgentChangedEventData) {
	if len(hostIDs) == 0 {
		return
	}
	m.watchCbMu.RLock()
	fn := m.onAgentChanged
	m.watchCbMu.RUnlock()
	if fn != nil {
		fn(hostIDs, ev)
	}
}

func (m *Manager) notifyCoverageChanged(hostIDs []int64) {
	if len(hostIDs) == 0 {
		return
	}
	m.watchCbMu.RLock()
	fn := m.onCoverageChanged
	m.watchCbMu.RUnlock()
	if fn != nil {
		fn(hostIDs)
	}
}

func (m *Manager) notifyCoverageForConn(sc *sharedConn) {
	if sc == nil {
		return
	}
	ids := m.connHostIDs(sc)
	if len(ids) > 0 {
		m.notifyCoverageChanged(ids)
	}
}

func (m *Manager) connHostIDs(sc *sharedConn) []int64 {
	m.mu.Lock()
	idSet := make(map[int64]struct{})
	for hid, k := range m.hostConn {
		if k == sc.key {
			idSet[hid] = struct{}{}
		}
	}
	sc.mu.Lock()
	for hid := range sc.hostRefs {
		idSet[hid] = struct{}{}
	}
	for hid := range sc.watchCoveredHosts {
		idSet[hid] = struct{}{}
	}
	sc.mu.Unlock()
	m.mu.Unlock()
	if len(idSet) == 0 {
		return nil
	}
	ids := make([]int64, 0, len(idSet))
	for hid := range idSet {
		ids = append(ids, hid)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}

type watchedPair struct {
	userID int64
	hostID int64
}

// connWatchDemandSnapshot returns the unique (userID, hostID) pairs watching
// any host attached to sc, plus the sorted deduplicated extra log paths.
func (m *Manager) connWatchDemandSnapshot(sc *sharedConn) ([]watchedPair, []string) {
	m.mu.Lock()
	hostSet := make(map[int64]struct{})
	for hid, k := range m.hostConn {
		if k == sc.key {
			hostSet[hid] = struct{}{}
		}
	}
	sc.mu.Lock()
	for hid := range sc.hostRefs {
		hostSet[hid] = struct{}{}
	}
	sc.mu.Unlock()

	pairSet := make(map[watchedPair]struct{})
	for _, reg := range m.watchers {
		for hid := range reg.hosts {
			if _, onConn := hostSet[hid]; onConn {
				pairSet[watchedPair{userID: reg.userID, hostID: hid}] = struct{}{}
			}
		}
	}
	logSet := make(map[string]struct{})
	for hid := range hostSet {
		for p := range m.hostExtraLogs[hid] {
			logSet[p] = struct{}{}
		}
	}
	m.mu.Unlock()

	pairs := make([]watchedPair, 0, len(pairSet))
	for p := range pairSet {
		pairs = append(pairs, p)
	}
	logs := make([]string, 0, len(logSet))
	for p := range logSet {
		logs = append(logs, p)
	}
	sort.Strings(logs)
	if len(logs) > maxExtraWatchLogsPerConn {
		logs = logs[:maxExtraWatchLogsPerConn]
	}
	return pairs, logs
}

func (m *Manager) hasEligibleWatchDemand(sc *sharedConn, curClient *Client) (bool, []string) {
	pairs, logs := m.connWatchDemandSnapshot(sc)
	if len(pairs) == 0 {
		return false, nil
	}
	for _, p := range pairs {
		cl, gotSC, ok := m.clientAndConnForCap(p.userID, p.hostID, "watch")
		if ok && gotSC == sc && cl == curClient {
			return true, logs
		}
	}
	return false, nil
}

// hasAnyRunningWatchDemand checks whether any registered browser watcher wants
// a host on sc whose stored corrald state is running for that user (ignoring
// local watch degradation/health, so poll-degraded subscriptions stay alive
// while browsers watch and unsubscribe cleanly after the last browser leaves).
func (m *Manager) hasAnyRunningWatchDemand(ctx context.Context, sc *sharedConn, manifest *proto.Manifest) (bool, []string) {
	if m == nil || m.adminDisabled || !m.Available() {
		return false, nil
	}
	pairs, logs := m.connWatchDemandSnapshot(sc)
	if len(pairs) == 0 {
		return false, nil
	}
	for _, p := range pairs {
		ch, err := m.store.CorraldHostByID(ctx, p.hostID)
		if err != nil {
			continue
		}
		if EffectiveUserState(toSnapshot(ch), p.userID) != StateRunning {
			continue
		}
		if manifest != nil {
			if consented, hasConsented := parseManifestJSON(ch.ManifestJSON); hasConsented {
				if !CapAllowedByScope(consented, *manifest, "watch") {
					continue
				}
			}
		}
		return true, logs
	}
	return false, nil
}

func watchLogsSignature(logs []string) string {
	if len(logs) == 0 {
		return ""
	}
	cp := slices.Clone(logs)
	sort.Strings(cp)
	return strings.Join(cp, "\x00")
}

func watchResultHealthy(res *proto.AgentWatchResult) bool {
	if res == nil || !res.Watching {
		return false
	}
	if res.Mode == "poll" {
		return false
	}
	return true
}

func (m *Manager) scheduleWatchReconcile(sc *sharedConn) {
	if m == nil || sc == nil {
		return
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	sc.mu.Lock()
	if sc.watchReconciling {
		sc.watchReconcileAgain = true
		sc.mu.Unlock()
		m.mu.Unlock()
		return
	}
	sc.watchReconciling = true
	sc.watchReconcileAgain = false
	m.wg.Add(1)
	sc.mu.Unlock()
	m.mu.Unlock()

	go func() {
		defer m.wg.Done()
		for {
			m.reconcileWatchOnce(sc)
			sc.mu.Lock()
			if !sc.watchReconcileAgain {
				sc.watchReconciling = false
				sc.mu.Unlock()
				return
			}
			sc.watchReconcileAgain = false
			sc.mu.Unlock()
		}
	}()
}

// reconcileWatchOnce strictly serializes Watch and Unwatch RPCs for sc so a
// grace-period Unwatch can never race with a newly arriving browser watcher.
func (m *Manager) reconcileWatchOnce(sc *sharedConn) {
	select {
	case <-sc.stopCh:
		return
	default:
	}

	sc.mu.Lock()
	curClient := sc.client
	remoteActive := sc.watchRemoteActive && sc.watchClient == curClient
	healthy := sc.watchHealthy && remoteActive
	prevSig := sc.watchLogsSig
	unsubDue := sc.watchUnsubDue
	var curManifest *proto.Manifest
	if sc.hello != nil {
		cp := sc.hello.Manifest
		curManifest = &cp
	}
	sc.mu.Unlock()

	if curClient == nil || curClient.Closed() {
		m.mutateWatchState(sc, nil, func() {
			if sc.client == nil || sc.client == curClient || sc.client.Closed() {
				sc.clearWatchStateLocked()
			}
		})
		return
	}

	wantEligible, logs := m.hasEligibleWatchDemand(sc, curClient)
	if wantEligible {
		sc.mu.Lock()
		sc.cancelUnsubTimerLocked()
		sc.watchUnsubDue = false
		sc.mu.Unlock()

		sig := watchLogsSignature(logs)
		if healthy && sig == prevSig {
			return
		}

		wctx, cancel := context.WithTimeout(context.Background(), m.watchRPCTimeout)
		res, err := curClient.Watch(wctx, logs)
		cancel()

		m.mutateWatchState(sc, nil, func() {
			if sc.client != curClient || curClient.Closed() {
				if sc.watchClient == curClient && curClient.Closed() {
					sc.clearWatchStateLocked()
				}
				return
			}
			if err != nil || res == nil || !res.Watching {
				sc.capDegraded["watch"] = true
				alreadyRemote := sc.watchRemoteActive && sc.watchClient == curClient
				var rpcErr *proto.RPCError
				explicitRPCReject := err != nil && errors.As(err, &rpcErr)
				if (alreadyRemote && err != nil) || (!alreadyRemote && err != nil && !explicitRPCReject) {
					// The connection is still open and either a prior remote
					// subscription was active or the Watch RPC ended ambiguously
					// (e.g. timeout while corrald may still process it): drop
					// healthy coverage immediately, but keep watchRemoteActive
					// so the last browser leaving still issues agent.unwatch.
					sc.cancelUnsubTimerLocked()
					sc.watchUnsubDue = false
					sc.watchRemoteActive = true
					sc.watchHealthy = false
					sc.watchClient = curClient
				} else {
					sc.clearWatchStateLocked()
				}
				return
			}
			sc.cancelUnsubTimerLocked()
			sc.watchUnsubDue = false
			sc.watchRemoteActive = true
			sc.watchClient = curClient
			sc.watchLogsSig = sig
			if watchResultHealthy(res) {
				sc.watchHealthy = true
			} else {
				// Mode="poll": remote subscription IS active (must be unwatched
				// after last browser leaves), but host is not in healthy cover.
				sc.watchHealthy = false
				sc.capDegraded["watch"] = true
			}
		})
		return
	}

	if !remoteActive {
		return
	}

	// Remote subscription is active (healthy or poll-degraded). Check whether
	// any browser still watches a running host on this sharedConn.
	rctx, rcancel := context.WithTimeout(context.Background(), 2*time.Second)
	wantRaw, _ := m.hasAnyRunningWatchDemand(rctx, sc, curManifest)
	rcancel()
	if wantRaw {
		sc.mu.Lock()
		sc.cancelUnsubTimerLocked()
		sc.watchUnsubDue = false
		sc.mu.Unlock()
		return
	}

	// No browsers watching any running host on sc. If the grace delay has
	// elapsed, send Unwatch inside this serialized reconcile loop.
	if unsubDue {
		sc.mu.Lock()
		sc.watchUnsubDue = false
		stillMine := sc.client == curClient && sc.watchClient == curClient && sc.watchRemoteActive && !curClient.Closed()
		sc.mu.Unlock()
		if !stillMine {
			return
		}

		uctx, cancel := context.WithTimeout(context.Background(), m.watchRPCTimeout)
		_, _ = curClient.Unwatch(uctx)
		cancel()

		m.mutateWatchState(sc, nil, func() {
			if sc.client == curClient && sc.watchClient == curClient {
				sc.clearWatchStateLocked()
			}
		})
		// Re-check demand after Unwatch returns: if a browser arrived while
		// Unwatch was in flight, scheduleWatchReconcile already set
		// watchReconcileAgain = true, and checking here guarantees zero window.
		rctx2, rcancel2 := context.WithTimeout(context.Background(), 2*time.Second)
		wantAfter, _ := m.hasAnyRunningWatchDemand(rctx2, sc, curManifest)
		rcancel2()
		if wantAfter {
			sc.mu.Lock()
			sc.watchReconcileAgain = true
			sc.mu.Unlock()
		}
		return
	}

	// Start grace-period unsubscribe timer if one is not already running.
	m.mu.Lock()
	closed := m.closed
	delay := m.watchUnsubDelay
	if !closed {
		sc.mu.Lock()
		if sc.client == curClient && sc.watchRemoteActive && sc.watchClient == curClient && sc.watchUnsubCancel == nil {
			cancelCh := make(chan struct{})
			sc.watchUnsubCancel = cancelCh
			m.wg.Add(1)
			go func(target *Client, stop chan struct{}, wait time.Duration) {
				defer m.wg.Done()
				timer := time.NewTimer(wait)
				defer timer.Stop()
				select {
				case <-timer.C:
					sc.mu.Lock()
					if sc.watchUnsubCancel == stop && sc.client == target && sc.watchClient == target && sc.watchRemoteActive {
						sc.watchUnsubCancel = nil
						sc.watchUnsubDue = true
						sc.mu.Unlock()
						m.scheduleWatchReconcile(sc)
						return
					}
					sc.mu.Unlock()
				case <-stop:
				case <-sc.stopCh:
				}
			}(curClient, cancelCh, delay)
		}
		sc.mu.Unlock()
	}
	m.mu.Unlock()
}

func (m *Manager) handleAgentChangedEvent(sc *sharedConn, ev proto.Event) {
	if len(ev.Data) == 0 {
		return
	}
	var data proto.AgentChangedEventData
	if err := json.Unmarshal(ev.Data, &data); err != nil {
		return
	}
	if len(data.AddedPIDs) == 0 && len(data.RemovedPIDs) == 0 && len(data.ChangedLogs) == 0 {
		return
	}
	ids := m.connHostIDs(sc)
	if len(ids) == 0 {
		return
	}
	m.emitAgentChanged(ids, data)
}

func isWatchHealthOK(health *proto.HealthResult) bool {
	if health == nil || health.Caps == nil {
		return true
	}
	st, exists := health.Caps["watch"]
	return !exists || st == "ok"
}
