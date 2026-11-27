package gw

import (
	"context"
	"encoding/json"
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
	watchRPCTimeout          = 5 * time.Second
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
		// Evict an arbitrary entry to stay bounded.
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
		subscribed := sc.watchSubscribed && sc.watchClient != nil && sc.watchClient == sc.client && !sc.client.Closed()
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
	covered := sc.watchSubscribed && sc.watchClient == client
	sc.mu.Unlock()
	return covered
}

// WatchSubscribedForHost reports whether hostID's sharedConn currently has an
// active agent.watch subscription (used by tests).
func (m *Manager) WatchSubscribedForHost(hostID int64) bool {
	sc := m.sharedConnForHost(hostID, "", "")
	if sc == nil {
		return false
	}
	sc.mu.Lock()
	defer sc.mu.Unlock()
	return sc.watchSubscribed && sc.client != nil && sc.watchClient == sc.client && !sc.client.Closed()
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
// a host on sc whose stored corrald state is running for that user (used during
// reconnectSharedConn before sc.client is swapped to the new connection).
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

func (sc *sharedConn) cancelUnsubTimerLocked() {
	if sc.watchUnsubCancel != nil {
		close(sc.watchUnsubCancel)
		sc.watchUnsubCancel = nil
	}
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

func (m *Manager) reconcileWatchOnce(sc *sharedConn) {
	select {
	case <-sc.stopCh:
		return
	default:
	}

	sc.mu.Lock()
	curClient := sc.client
	subscribed := sc.watchSubscribed && sc.watchClient == curClient
	prevSig := sc.watchLogsSig
	sc.mu.Unlock()

	if curClient == nil || curClient.Closed() {
		sc.mu.Lock()
		sc.cancelUnsubTimerLocked()
		wasSub := sc.watchSubscribed
		sc.watchSubscribed = false
		sc.watchClient = nil
		sc.watchLogsSig = ""
		sc.mu.Unlock()
		if wasSub {
			m.notifyCoverageForConn(sc)
		}
		return
	}

	want, logs := m.hasEligibleWatchDemand(sc, curClient)
	if want {
		sc.mu.Lock()
		sc.cancelUnsubTimerLocked()
		sc.mu.Unlock()

		sig := watchLogsSignature(logs)
		if subscribed && sig == prevSig {
			return
		}

		wctx, cancel := context.WithTimeout(context.Background(), watchRPCTimeout)
		res, err := curClient.Watch(wctx, logs)
		cancel()

		changed := false
		sc.mu.Lock()
		if sc.client == curClient && !curClient.Closed() {
			if err != nil || !watchResultHealthy(res) {
				if !sc.capDegraded["watch"] || sc.watchSubscribed {
					changed = true
				}
				sc.capDegraded["watch"] = true
				sc.watchSubscribed = false
				sc.watchClient = nil
				sc.watchLogsSig = ""
			} else {
				if !sc.watchSubscribed || sc.watchClient != curClient {
					changed = true
				}
				sc.watchSubscribed = true
				sc.watchClient = curClient
				sc.watchLogsSig = sig
			}
		}
		sc.mu.Unlock()

		if changed {
			m.notifyCoverageForConn(sc)
		}
		return
	}

	// No eligible demand: if currently subscribed on this client, start grace
	// period unsubscribe timer (or leave existing timer running).
	if subscribed {
		m.mu.Lock()
		closed := m.closed
		delay := m.watchUnsubDelay
		if !closed {
			sc.mu.Lock()
			if sc.client == curClient && sc.watchSubscribed && sc.watchClient == curClient && sc.watchUnsubCancel == nil {
				cancelCh := make(chan struct{})
				sc.watchUnsubCancel = cancelCh
				m.wg.Add(1)
				go func(target *Client, stop chan struct{}, wait time.Duration) {
					defer m.wg.Done()
					timer := time.NewTimer(wait)
					defer timer.Stop()
					select {
					case <-timer.C:
						m.finishWatchUnsubscribe(sc, target, stop)
					case <-stop:
					case <-sc.stopCh:
					}
				}(curClient, cancelCh, delay)
			}
			sc.mu.Unlock()
		}
		m.mu.Unlock()
	}
}

func (m *Manager) finishWatchUnsubscribe(sc *sharedConn, target *Client, token chan struct{}) {
	// Clear timer reference first if still ours.
	sc.mu.Lock()
	if sc.watchUnsubCancel != token {
		sc.mu.Unlock()
		return
	}
	sc.watchUnsubCancel = nil
	if sc.client != target || !sc.watchSubscribed || sc.watchClient != target || target.Closed() {
		sc.mu.Unlock()
		return
	}
	sc.mu.Unlock()

	// Re-check demand before unsubscribing (a browser might have arrived right as timer fired).
	if want, _ := m.hasEligibleWatchDemand(sc, target); want {
		m.scheduleWatchReconcile(sc)
		return
	}

	uctx, cancel := context.WithTimeout(context.Background(), watchRPCTimeout)
	_, _ = target.Unwatch(uctx)
	cancel()

	changed := false
	sc.mu.Lock()
	if sc.client == target && sc.watchClient == target && sc.watchSubscribed {
		sc.watchSubscribed = false
		sc.watchClient = nil
		sc.watchLogsSig = ""
		changed = true
	}
	sc.mu.Unlock()

	if changed {
		m.notifyCoverageForConn(sc)
	}
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
