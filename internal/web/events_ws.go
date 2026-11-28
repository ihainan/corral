package web

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/ihainan/corral/internal/corrald/proto"
	"github.com/ihainan/corral/internal/term"
)

// DefaultEventCoalesceWindow is the minimum interval between pushed hints for
// the same (user, session, pane) or (user, host) key.
const DefaultEventCoalesceWindow = 150 * time.Millisecond

// DefaultUnmatchedLogQuietThreshold is how long a binding's log must have been
// quiet before an unmatched log change on the same host also triggers a convo
// hint (handling /clear, /resume, or new Codex sessions without waking other
// actively streaming conversations on the same host).
const DefaultUnmatchedLogQuietThreshold = 2 * time.Second

// DefaultFallbackWakeTTL is how long an unmatched log path or unmatched added
// PID is remembered per (user, host) so repeated activity from another agent
// on the same host only wakes no-log or quiet bindings once.
const DefaultFallbackWakeTTL = 10 * time.Minute

const (
	maxWatchedHostsPerConn     = 512
	eventSubSendBuf            = 64
	maxFallbackWakeLogsPerHost = 256
	maxFallbackWakePIDsPerHost = 256
	maxFallbackWakeScopes      = 2048
	maxDeferredWakes           = 2048
)

// userHostKey scopes the fallback-wake memory to one watched pane (user,
// host, session, pane): a log or pid is remembered only for a pane it
// actually woke, so a pane that was busy when a new log first appeared is
// still woken by it once it goes quiet.
type userHostKey struct {
	userID    int64
	hostID    int64
	sessionID string
	pane      string
}

type hostWakeSeen struct {
	logs     map[string]time.Time
	pids     map[int]time.Time
	lastUsed time.Time
}

type convoBindKey struct {
	sessionID string
	pane      string
}

type convoHintBinding struct {
	ownerID       int64
	hostID        int64
	sessionID     string
	pane          string // herdr pane id, or "" for tmux
	bindPane      string // pane reported to browser in {"t":"convo", ...}
	pid           int
	log           string
	lastLogChange time.Time
}

type coalesceKey struct {
	kind      string // "convo" or "agents"
	userID    int64
	hostID    int64
	sessionID string
	pane      string
}

type coalesceEntry struct {
	timer   *time.Timer
	pending bool
}

type deferredWakeEntry struct {
	ownerID  int64
	hostID   int64
	bindPane string
	logs     map[string]struct{}
	pids     map[int]struct{}
	timer    *time.Timer
	gen      uint64
}

type eventSub struct {
	userID  int64
	sendCh  chan []byte
	coverCh chan []byte

	mu           sync.Mutex
	watchedHosts map[int64]struct{}
	lastCover    []int64
	coverSent    bool
}

func newEventSub(userID int64) *eventSub {
	return &eventSub{
		userID:       userID,
		sendCh:       make(chan []byte, eventSubSendBuf),
		coverCh:      make(chan []byte, 1),
		watchedHosts: make(map[int64]struct{}),
	}
}

func (sub *eventSub) trySendHint(payload []byte) bool {
	select {
	case sub.sendCh <- payload:
		return true
	default:
		return false
	}
}

func (sub *eventSub) setCoverPayload(payload []byte) {
	// Replace any older pending cover update so the latest coverage snapshot is
	// never dropped even if the hint channel is full.
	for {
		select {
		case sub.coverCh <- payload:
			return
		default:
			select {
			case <-sub.coverCh:
			default:
			}
		}
	}
}

func (sub *eventSub) watchesHost(hostID int64) bool {
	sub.mu.Lock()
	_, ok := sub.watchedHosts[hostID]
	sub.mu.Unlock()
	return ok
}

func (sub *eventSub) watchedHostList() []int64 {
	sub.mu.Lock()
	ids := make([]int64, 0, len(sub.watchedHosts))
	for id := range sub.watchedHosts {
		ids = append(ids, id)
	}
	sub.mu.Unlock()
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}

type eventHub struct {
	s               *Server
	coalesceWindow  time.Duration
	quietThreshold  time.Duration
	fallbackWakeTTL time.Duration

	mu            sync.Mutex
	closed        bool
	subs          map[*eventSub]struct{}
	byUser        map[int64]map[*eventSub]struct{}
	bindings      map[convoBindKey]*convoHintBinding
	coalesce      map[coalesceKey]*coalesceEntry
	wakeSeen      map[userHostKey]*hostWakeSeen
	deferredWakes map[convoBindKey]*deferredWakeEntry
	onDeliverHint func(key coalesceKey) // test hook, nil in production
}

func newEventHub(s *Server) *eventHub {
	return &eventHub{
		s:               s,
		coalesceWindow:  DefaultEventCoalesceWindow,
		quietThreshold:  DefaultUnmatchedLogQuietThreshold,
		fallbackWakeTTL: DefaultFallbackWakeTTL,
		subs:            make(map[*eventSub]struct{}),
		byUser:          make(map[int64]map[*eventSub]struct{}),
		bindings:        make(map[convoBindKey]*convoHintBinding),
		coalesce:        make(map[coalesceKey]*coalesceEntry),
		wakeSeen:        make(map[userHostKey]*hostWakeSeen),
		deferredWakes:   make(map[convoBindKey]*deferredWakeEntry),
	}
}

func (h *eventHub) Close() {
	if h == nil {
		return
	}
	h.mu.Lock()
	h.closed = true
	for k, e := range h.coalesce {
		if e.timer != nil {
			e.timer.Stop()
		}
		delete(h.coalesce, k)
	}
	for k, dw := range h.deferredWakes {
		if dw.timer != nil {
			dw.timer.Stop()
		}
		dw.gen++
		delete(h.deferredWakes, k)
	}
	clear(h.wakeSeen)
	h.mu.Unlock()
}

func (h *eventHub) SetCoalesceWindow(d time.Duration) {
	if h == nil || d <= 0 {
		return
	}
	h.mu.Lock()
	h.coalesceWindow = d
	h.mu.Unlock()
}

func (h *eventHub) SetUnmatchedLogQuietThreshold(d time.Duration) {
	if h == nil || d <= 0 {
		return
	}
	h.mu.Lock()
	h.quietThreshold = d
	h.mu.Unlock()
}

func (h *eventHub) SetFallbackWakeTTL(d time.Duration) {
	if h == nil || d <= 0 {
		return
	}
	h.mu.Lock()
	h.fallbackWakeTTL = d
	h.mu.Unlock()
}

func (h *eventHub) SetOnDeliverHintForTest(fn func(key coalesceKey)) {
	if h == nil {
		return
	}
	h.mu.Lock()
	h.onDeliverHint = fn
	h.mu.Unlock()
}

func (h *eventHub) wakeScopeLocked(uid, hid int64, sessionID, pane string, now time.Time, ttl time.Duration) *hostWakeSeen {
	k := userHostKey{userID: uid, hostID: hid, sessionID: sessionID, pane: pane}
	if sc := h.wakeSeen[k]; sc != nil {
		sc.lastUsed = now
		return sc
	}
	if len(h.wakeSeen) >= maxFallbackWakeScopes {
		for sk, sc := range h.wakeSeen {
			if now.Sub(sc.lastUsed) >= ttl {
				delete(h.wakeSeen, sk)
			}
		}
	}
	if len(h.wakeSeen) >= maxFallbackWakeScopes {
		var oldestKey userHostKey
		var oldestTime time.Time
		first := true
		for sk, sc := range h.wakeSeen {
			if first || sc.lastUsed.Before(oldestTime) {
				oldestKey = sk
				oldestTime = sc.lastUsed
				first = false
			}
		}
		if !first {
			delete(h.wakeSeen, oldestKey)
		}
	}
	sc := &hostWakeSeen{
		logs:     make(map[string]time.Time),
		pids:     make(map[int]time.Time),
		lastUsed: now,
	}
	h.wakeSeen[k] = sc
	return sc
}

// markFreshUnmatchedWakes records unmatched log paths and PIDs for (uid, hid)
// and reports whether at least one of them has not yet triggered a fallback
// wake within fallbackWakeTTL.
func (h *eventHub) markFreshUnmatchedWakes(uid, hid int64, sessionID, pane string, logs []string, pids []int, now time.Time) bool {
	if len(logs) == 0 && len(pids) == 0 {
		return false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return false
	}
	return h.markFreshUnmatchedWakesLocked(uid, hid, sessionID, pane, logs, pids, now)
}

func (h *eventHub) markFreshUnmatchedWakesLocked(uid, hid int64, sessionID, pane string, logs []string, pids []int, now time.Time) bool {
	if len(logs) == 0 && len(pids) == 0 {
		return false
	}
	ttl := h.fallbackWakeTTL
	if ttl <= 0 {
		ttl = DefaultFallbackWakeTTL
	}
	sc := h.wakeScopeLocked(uid, hid, sessionID, pane, now, ttl)
	fresh := false

	for _, logPath := range logs {
		if logPath == "" {
			continue
		}
		if seenAt, ok := sc.logs[logPath]; ok && now.Sub(seenAt) < ttl {
			continue
		}
		if len(sc.logs) >= maxFallbackWakeLogsPerHost {
			for p, t := range sc.logs {
				if now.Sub(t) >= ttl {
					delete(sc.logs, p)
				}
			}
		}
		if len(sc.logs) >= maxFallbackWakeLogsPerHost {
			var oldestPath string
			var oldestAt time.Time
			first := true
			for p, t := range sc.logs {
				if first || t.Before(oldestAt) {
					oldestPath = p
					oldestAt = t
					first = false
				}
			}
			if !first {
				delete(sc.logs, oldestPath)
			}
		}
		sc.logs[logPath] = now
		fresh = true
	}

	for _, pid := range pids {
		if pid <= 0 {
			continue
		}
		if seenAt, ok := sc.pids[pid]; ok && now.Sub(seenAt) < ttl {
			continue
		}
		if len(sc.pids) >= maxFallbackWakePIDsPerHost {
			for p, t := range sc.pids {
				if now.Sub(t) >= ttl {
					delete(sc.pids, p)
				}
			}
		}
		if len(sc.pids) >= maxFallbackWakePIDsPerHost {
			var oldestPID int
			var oldestAt time.Time
			first := true
			for p, t := range sc.pids {
				if first || t.Before(oldestAt) {
					oldestPID = p
					oldestAt = t
					first = false
				}
			}
			if !first {
				delete(sc.pids, oldestPID)
			}
		}
		sc.pids[pid] = now
		fresh = true
	}

	return fresh
}

// freshUnmatchedLocked returns the subset of logs and pids that have NOT yet
// woken (uid, hid, sessionID, pane) within fallbackWakeTTL, without mutating
// wakeSeen.
func (h *eventHub) freshUnmatchedLocked(uid, hid int64, sessionID, pane string, logs []string, pids []int, now time.Time) ([]string, []int) {
	ttl := h.fallbackWakeTTL
	if ttl <= 0 {
		ttl = DefaultFallbackWakeTTL
	}
	sc := h.wakeSeen[userHostKey{userID: uid, hostID: hid, sessionID: sessionID, pane: pane}]
	var outLogs []string
	for _, logPath := range logs {
		if logPath == "" {
			continue
		}
		if sc != nil {
			if seenAt, ok := sc.logs[logPath]; ok && now.Sub(seenAt) < ttl {
				continue
			}
		}
		outLogs = append(outLogs, logPath)
	}
	var outPIDs []int
	for _, pid := range pids {
		if pid <= 0 {
			continue
		}
		if sc != nil {
			if seenAt, ok := sc.pids[pid]; ok && now.Sub(seenAt) < ttl {
				continue
			}
		}
		outPIDs = append(outPIDs, pid)
	}
	return outLogs, outPIDs
}

func (h *eventHub) cancelDeferredWakeLocked(bk convoBindKey) {
	if dw := h.deferredWakes[bk]; dw != nil {
		if dw.timer != nil {
			dw.timer.Stop()
		}
		dw.gen++
		delete(h.deferredWakes, bk)
	}
}

func (h *eventHub) userWatchesHostLocked(userID, hostID int64) bool {
	for sub := range h.byUser[userID] {
		if sub.watchesHost(hostID) {
			return true
		}
	}
	return false
}

func (h *eventHub) cancelDeferredWakesForUnwatchedLocked(userID int64) {
	for bk, dw := range h.deferredWakes {
		if dw.ownerID != userID {
			continue
		}
		if !h.userWatchesHostLocked(userID, dw.hostID) {
			if dw.timer != nil {
				dw.timer.Stop()
			}
			dw.gen++
			delete(h.deferredWakes, bk)
		}
	}
}

func (h *eventHub) pruneUnwatchedDeferredWakes(userID int64) {
	if h == nil {
		return
	}
	h.mu.Lock()
	h.cancelDeferredWakesForUnwatchedLocked(userID)
	h.mu.Unlock()
}

func (h *eventHub) DeferredWakeCountForTest() int {
	if h == nil {
		return 0
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.deferredWakes)
}

func (h *eventHub) rescheduleDeferredWakeLocked(bk convoBindKey, dw *deferredWakeEntry, fireAt, now time.Time) {
	if dw.timer != nil {
		dw.timer.Stop()
	}
	dw.gen++
	gen := dw.gen
	delay := fireAt.Sub(now)
	if delay < 0 {
		delay = 0
	}
	dw.timer = time.AfterFunc(delay, func() {
		h.onDeferredWake(bk, dw, gen)
	})
}

func (h *eventHub) scheduleDeferredWakeLocked(b *convoHintBinding, logs []string, pids []int, lastActivity, now time.Time, quietThreshold time.Duration) {
	if h.closed || b == nil {
		return
	}
	freshLogs, freshPIDs := h.freshUnmatchedLocked(b.ownerID, b.hostID, b.sessionID, b.pane, logs, pids, now)
	if len(freshLogs) == 0 && len(freshPIDs) == 0 {
		return
	}
	bk := convoBindKey{sessionID: b.sessionID, pane: b.pane}
	dw := h.deferredWakes[bk]
	if dw == nil {
		if len(h.deferredWakes) >= maxDeferredWakes {
			return
		}
		dw = &deferredWakeEntry{
			ownerID:  b.ownerID,
			hostID:   b.hostID,
			bindPane: b.bindPane,
			logs:     make(map[string]struct{}),
			pids:     make(map[int]struct{}),
		}
		h.deferredWakes[bk] = dw
	} else {
		dw.ownerID = b.ownerID
		dw.hostID = b.hostID
		dw.bindPane = b.bindPane
	}
	for _, p := range freshLogs {
		if p != "" && p != b.log && len(dw.logs) < maxFallbackWakeLogsPerHost {
			dw.logs[p] = struct{}{}
		}
	}
	for _, pid := range freshPIDs {
		if pid > 0 && pid != b.pid && len(dw.pids) < maxFallbackWakePIDsPerHost {
			dw.pids[pid] = struct{}{}
		}
	}
	if len(dw.logs) == 0 && len(dw.pids) == 0 {
		h.cancelDeferredWakeLocked(bk)
		return
	}
	fireAt := lastActivity.Add(quietThreshold)
	if b.log != "" && !b.lastLogChange.IsZero() {
		if bFire := b.lastLogChange.Add(quietThreshold); bFire.After(fireAt) {
			fireAt = bFire
		}
	}
	h.rescheduleDeferredWakeLocked(bk, dw, fireAt, now)
}

func (h *eventHub) onDeferredWake(bk convoBindKey, entry *deferredWakeEntry, gen uint64) {
	now := time.Now()
	h.mu.Lock()
	if h.closed || h.deferredWakes[bk] != entry || entry.gen != gen {
		h.mu.Unlock()
		return
	}
	b := h.bindings[bk]
	if b == nil {
		delete(h.deferredWakes, bk)
		h.mu.Unlock()
		return
	}
	qt := h.quietThreshold
	if qt <= 0 {
		qt = DefaultUnmatchedLogQuietThreshold
	}
	// If the binding received more log writes before becoming quiet, postpone
	// until lastLogChange + quietThreshold.
	if b.log != "" && !b.lastLogChange.IsZero() && now.Sub(b.lastLogChange) < qt {
		h.rescheduleDeferredWakeLocked(bk, entry, b.lastLogChange.Add(qt), now)
		h.mu.Unlock()
		return
	}
	if !h.userWatchesHostLocked(b.ownerID, b.hostID) {
		delete(h.deferredWakes, bk)
		h.mu.Unlock()
		return
	}

	ownerID := b.ownerID
	hostID := b.hostID
	sessionID := b.sessionID
	pane := b.pane
	bindPane := b.bindPane
	logs := make([]string, 0, len(entry.logs))
	for p := range entry.logs {
		if p != "" && p != b.log {
			logs = append(logs, p)
		}
	}
	pids := make([]int, 0, len(entry.pids))
	for pid := range entry.pids {
		if pid > 0 && pid != b.pid {
			pids = append(pids, pid)
		}
	}
	delete(h.deferredWakes, bk)
	h.mu.Unlock()

	if len(logs) == 0 && len(pids) == 0 {
		return
	}

	// Verify session is still running outside h.mu (matching HandleAgentChanged).
	if h.s != nil && h.s.terms != nil {
		sess, err := h.s.terms.Get(ownerID, sessionID)
		if err != nil || sess == nil || sess.State() != term.StateRunning {
			h.mu.Lock()
			if cur := h.bindings[bk]; cur != nil && cur.ownerID == ownerID && cur.hostID == hostID {
				delete(h.bindings, bk)
			}
			h.mu.Unlock()
			return
		}
	}

	if !h.markFreshUnmatchedWakes(ownerID, hostID, sessionID, pane, logs, pids, now) {
		return
	}

	// Write relocAfter BEFORE emitting the convo hint so any immediate
	// push-triggered GET is guaranteed to observe relocAfter.
	if h.s != nil {
		ts := now.UnixNano()
		if st := h.s.convos.peek(sessionID + "\x00" + pane); st != nil {
			for {
				was := st.relocAfter.Load()
				if was >= ts || st.relocAfter.CompareAndSwap(was, ts) {
					break
				}
			}
		}
	}

	h.enqueueHint(coalesceKey{
		kind:      "convo",
		userID:    ownerID,
		hostID:    hostID,
		sessionID: sessionID,
		pane:      bindPane,
	})
}

func (h *eventHub) forgetWakePIDs(uid, hid int64, pids []int) {
	if len(pids) == 0 {
		return
	}
	h.mu.Lock()
	for k, sc := range h.wakeSeen {
		if k.userID != uid || k.hostID != hid {
			continue
		}
		for _, pid := range pids {
			delete(sc.pids, pid)
		}
	}
	for bk, dw := range h.deferredWakes {
		if dw.ownerID != uid || dw.hostID != hid {
			continue
		}
		for _, pid := range pids {
			delete(dw.pids, pid)
		}
		if len(dw.logs) == 0 && len(dw.pids) == 0 {
			h.cancelDeferredWakeLocked(bk)
		}
	}
	h.mu.Unlock()
}

func (h *eventHub) addSub(sub *eventSub) {
	h.mu.Lock()
	h.subs[sub] = struct{}{}
	if h.byUser[sub.userID] == nil {
		h.byUser[sub.userID] = make(map[*eventSub]struct{})
	}
	h.byUser[sub.userID][sub] = struct{}{}
	h.mu.Unlock()
}

func (h *eventHub) removeSub(sub *eventSub) {
	h.mu.Lock()
	delete(h.subs, sub)
	if us := h.byUser[sub.userID]; us != nil {
		delete(us, sub)
		if len(us) == 0 {
			delete(h.byUser, sub.userID)
		}
	}
	h.cancelDeferredWakesForUnwatchedLocked(sub.userID)
	h.mu.Unlock()
	if h.s != nil && h.s.corrald != nil {
		h.s.corrald.RemoveWatcher(sub)
	}
}

// RecordBinding updates the gateway's fast lock-free-from-IO mapping from
// (session, pane) -> (owner, host, pid, logPath) used to map corrald's
// agent.changed hints without contending on convoState.mu.
func (h *eventHub) RecordBinding(ownerID, hostID int64, sessionID, pane, bindPane string, pid int, logPath string) {
	h.RecordBindingWithActivity(ownerID, hostID, sessionID, pane, bindPane, pid, logPath, time.Time{})
}

// RecordBindingWithActivity is RecordBinding with an optional last log-growth
// timestamp (from convoState.grewAt) so quiet bindings can be distinguished
// from actively streaming conversations when an unmatched log appears.
func (h *eventHub) RecordBindingWithActivity(ownerID, hostID int64, sessionID, pane, bindPane string, pid int, logPath string, logActivity time.Time) {
	if h == nil || sessionID == "" || hostID <= 0 {
		return
	}
	k := convoBindKey{sessionID: sessionID, pane: pane}
	h.mu.Lock()
	if pid <= 0 && logPath == "" {
		delete(h.bindings, k)
		h.cancelDeferredWakeLocked(k)
	} else {
		if bindPane == "" {
			bindPane = pane
		}
		var lastChange time.Time
		if logPath != "" {
			if prev := h.bindings[k]; prev != nil && prev.log == logPath {
				lastChange = prev.lastLogChange
			}
			if !logActivity.IsZero() && logActivity.After(lastChange) {
				lastChange = logActivity
			}
			if lastChange.IsZero() {
				lastChange = time.Now()
			}
		}
		h.bindings[k] = &convoHintBinding{
			ownerID:       ownerID,
			hostID:        hostID,
			sessionID:     sessionID,
			pane:          pane,
			bindPane:      bindPane,
			pid:           pid,
			log:           logPath,
			lastLogChange: lastChange,
		}
		if dw := h.deferredWakes[k]; dw != nil {
			if logPath != "" {
				delete(dw.logs, logPath)
			}
			if pid > 0 {
				delete(dw.pids, pid)
			}
			if len(dw.logs) == 0 && len(dw.pids) == 0 {
				h.cancelDeferredWakeLocked(k)
			} else {
				dw.ownerID = ownerID
				dw.hostID = hostID
				dw.bindPane = bindPane
				if logPath != "" && !lastChange.IsZero() {
					qt := h.quietThreshold
					if qt <= 0 {
						qt = DefaultUnmatchedLogQuietThreshold
					}
					h.rescheduleDeferredWakeLocked(k, dw, lastChange.Add(qt), time.Now())
				}
			}
		}
	}
	h.mu.Unlock()

	if logPath != "" && h.s != nil && h.s.corrald != nil {
		h.s.corrald.AddHostWatchLog(hostID, logPath)
	}
}

// ForgetSession removes all cached conversation hint bindings for sessionID.
func (h *eventHub) ForgetSession(sessionID string) {
	if h == nil || sessionID == "" {
		return
	}
	h.mu.Lock()
	for k := range h.bindings {
		if k.sessionID == sessionID {
			delete(h.bindings, k)
		}
	}
	for k := range h.deferredWakes {
		if k.sessionID == sessionID {
			h.cancelDeferredWakeLocked(k)
		}
	}
	h.mu.Unlock()
}

func (h *eventHub) computeCover(sub *eventSub) []int64 {
	watched := sub.watchedHostList()
	if len(watched) == 0 || h.s == nil || h.s.corrald == nil {
		return []int64{}
	}
	covered := make([]int64, 0, len(watched))
	for _, hid := range watched {
		if h.s.corrald.IsWatchCovered(sub.userID, hid) {
			covered = append(covered, hid)
		}
	}
	return covered
}

func (h *eventHub) sendCoverIfChanged(sub *eventSub, force bool) {
	covered := h.computeCover(sub)
	sub.mu.Lock()
	if !force && sub.coverSent && slices.Equal(sub.lastCover, covered) {
		sub.mu.Unlock()
		return
	}
	sub.lastCover = slices.Clone(covered)
	sub.coverSent = true
	sub.mu.Unlock()

	b, err := json.Marshal(struct {
		T     string  `json:"t"`
		Hosts []int64 `json:"hosts"`
	}{
		T:     "cover",
		Hosts: covered,
	})
	if err == nil {
		sub.setCoverPayload(b)
	}
}

// HandleCoverageChanged recomputes and pushes {"t":"cover","hosts":[...]} to
// every browser subscriber watching any host in hostIDs whose coverage changed.
func (h *eventHub) HandleCoverageChanged(hostIDs []int64) {
	if h == nil || len(hostIDs) == 0 {
		return
	}
	hset := make(map[int64]struct{}, len(hostIDs))
	for _, id := range hostIDs {
		hset[id] = struct{}{}
	}
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return
	}
	var targets []*eventSub
	for sub := range h.subs {
		sub.mu.Lock()
		match := false
		for id := range sub.watchedHosts {
			if _, ok := hset[id]; ok {
				match = true
				break
			}
		}
		sub.mu.Unlock()
		if match {
			targets = append(targets, sub)
		}
	}
	h.mu.Unlock()

	for _, sub := range targets {
		h.sendCoverIfChanged(sub, false)
	}
}

// HandleAgentChanged maps a corrald agent.changed event onto per-user convo
// and agents hints, applying per-pane/per-host coalescing and recording perf.
func (h *eventHub) HandleAgentChanged(hostIDs []int64, ev proto.AgentChangedEventData) {
	if h == nil || len(hostIDs) == 0 {
		return
	}
	if h.s != nil && h.s.perf != nil {
		for _, hid := range hostIDs {
			h.s.perf.Count("push.corrald_hint", "agent.changed", hid)
		}
	}

	hostSet := make(map[int64]struct{}, len(hostIDs))
	for _, hid := range hostIDs {
		hostSet[hid] = struct{}{}
	}

	// Snapshot active watchers per host and active bindings for these hosts.
	usersByHost := make(map[int64]map[int64]struct{}, len(hostIDs))
	var bindings []*convoHintBinding
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return
	}
	for sub := range h.subs {
		sub.mu.Lock()
		for hid := range sub.watchedHosts {
			if _, ok := hostSet[hid]; ok {
				if usersByHost[hid] == nil {
					usersByHost[hid] = make(map[int64]struct{})
				}
				usersByHost[hid][sub.userID] = struct{}{}
			}
		}
		sub.mu.Unlock()
	}
	for _, b := range h.bindings {
		if _, ok := hostSet[b.hostID]; ok {
			cp := *b
			bindings = append(bindings, &cp)
		}
	}
	h.mu.Unlock()

	if len(usersByHost) == 0 {
		return
	}

	// Filter bindings to still-running sessions owned by their recorded owner.
	var liveBindings []*convoHintBinding
	var staleKeys []convoBindKey
	for _, b := range bindings {
		if h.s != nil && h.s.terms != nil {
			sess, err := h.s.terms.Get(b.ownerID, b.sessionID)
			if err != nil || sess == nil || sess.State() != term.StateRunning {
				staleKeys = append(staleKeys, convoBindKey{sessionID: b.sessionID, pane: b.pane})
				continue
			}
		}
		liveBindings = append(liveBindings, b)
	}
	if len(staleKeys) > 0 {
		h.mu.Lock()
		for _, k := range staleKeys {
			delete(h.bindings, k)
			h.cancelDeferredWakeLocked(k)
		}
		h.mu.Unlock()
	}

	// Map event items independently per (hostID, userID) to preserve strict
	// cross-user isolation even when two users share a host account.
	now := time.Now()
	h.mu.Lock()
	quietThreshold := h.quietThreshold
	h.mu.Unlock()
	if quietThreshold <= 0 {
		quietThreshold = DefaultUnmatchedLogQuietThreshold
	}

	type deferredWakeReq struct {
		b            *convoHintBinding
		logs         []string
		pids         []int
		lastActivity time.Time
	}

	matchedKeys := make(map[convoBindKey]struct{})
	relocKeys := make(map[convoBindKey]struct{})
	pendingHints := make(map[coalesceKey]struct{})
	var deferredReqs []deferredWakeReq

	for hid, users := range usersByHost {
		for uid := range users {
			needAgents := false
			var unmatchedLogs []string
			var unmatchedAddedPIDs []int
			directMatched := make(map[convoBindKey]struct{})

			for _, logPath := range ev.ChangedLogs {
				matched := false
				for _, b := range liveBindings {
					if b.ownerID == uid && b.hostID == hid && b.log != "" && b.log == logPath {
						pendingHints[coalesceKey{
							kind:      "convo",
							userID:    uid,
							hostID:    hid,
							sessionID: b.sessionID,
							pane:      b.bindPane,
						}] = struct{}{}
						bk := convoBindKey{sessionID: b.sessionID, pane: b.pane}
						directMatched[bk] = struct{}{}
						matchedKeys[bk] = struct{}{}
						matched = true
					}
				}
				if !matched {
					needAgents = true
					if logPath != "" {
						unmatchedLogs = append(unmatchedLogs, logPath)
					}
				}
			}

			for _, pid := range ev.AddedPIDs {
				matched := false
				for _, b := range liveBindings {
					if b.ownerID == uid && b.hostID == hid && b.pid > 0 && b.pid == pid {
						pendingHints[coalesceKey{
							kind:      "convo",
							userID:    uid,
							hostID:    hid,
							sessionID: b.sessionID,
							pane:      b.bindPane,
						}] = struct{}{}
						bk := convoBindKey{sessionID: b.sessionID, pane: b.pane}
						directMatched[bk] = struct{}{}
						relocKeys[bk] = struct{}{}
						matched = true
					}
				}
				if !matched && pid > 0 {
					unmatchedAddedPIDs = append(unmatchedAddedPIDs, pid)
				}
				needAgents = true
			}

			// An unmatched log path or unmatched added PID wakes each of this
			// user's no-log or quiet (>= quietThreshold) bindings on this host
			// once within fallbackWakeTTL. Bindings currently busy (own log
			// written within quietThreshold, or written in this same event) are
			// deferred until they go quiet so short replies on a newly-created
			// log are not missed until the 10s fallback poll.
			if len(unmatchedLogs) > 0 || len(unmatchedAddedPIDs) > 0 {
				for _, b := range liveBindings {
					if b.ownerID != uid || b.hostID != hid {
						continue
					}
					bk := convoBindKey{sessionID: b.sessionID, pane: b.pane}
					if _, already := directMatched[bk]; already {
						if _, relocating := relocKeys[bk]; !relocating {
							deferredReqs = append(deferredReqs, deferredWakeReq{
								b:            b,
								logs:         unmatchedLogs,
								pids:         unmatchedAddedPIDs,
								lastActivity: now,
							})
						}
						continue
					}
					if b.log != "" && !b.lastLogChange.IsZero() && now.Sub(b.lastLogChange) < quietThreshold {
						deferredReqs = append(deferredReqs, deferredWakeReq{
							b:            b,
							logs:         unmatchedLogs,
							pids:         unmatchedAddedPIDs,
							lastActivity: b.lastLogChange,
						})
						continue
					}
					if h.markFreshUnmatchedWakes(uid, hid, b.sessionID, b.pane, unmatchedLogs, unmatchedAddedPIDs, now) {
						pendingHints[coalesceKey{
							kind:      "convo",
							userID:    uid,
							hostID:    hid,
							sessionID: b.sessionID,
							pane:      b.bindPane,
						}] = struct{}{}
						relocKeys[bk] = struct{}{}
					}
				}
			}

			if len(ev.RemovedPIDs) > 0 {
				h.forgetWakePIDs(uid, hid, ev.RemovedPIDs)
			}
			for _, pid := range ev.RemovedPIDs {
				for _, b := range liveBindings {
					if b.ownerID == uid && b.hostID == hid && b.pid > 0 && b.pid == pid {
						pendingHints[coalesceKey{
							kind:      "convo",
							userID:    uid,
							hostID:    hid,
							sessionID: b.sessionID,
							pane:      b.bindPane,
						}] = struct{}{}
						relocKeys[convoBindKey{sessionID: b.sessionID, pane: b.pane}] = struct{}{}
					}
				}
				needAgents = true
			}

			if needAgents {
				pendingHints[coalesceKey{
					kind:   "agents",
					userID: uid,
					hostID: hid,
				}] = struct{}{}
			}
		}
	}

	if len(matchedKeys) > 0 || len(relocKeys) > 0 || len(deferredReqs) > 0 {
		h.mu.Lock()
		for bk := range matchedKeys {
			if b := h.bindings[bk]; b != nil && b.log != "" {
				b.lastLogChange = now
				if dw := h.deferredWakes[bk]; dw != nil {
					h.rescheduleDeferredWakeLocked(bk, dw, now.Add(quietThreshold), now)
				}
			}
		}
		for bk := range relocKeys {
			if dw := h.deferredWakes[bk]; dw != nil {
				logs := make([]string, 0, len(dw.logs))
				for p := range dw.logs {
					logs = append(logs, p)
				}
				pids := make([]int, 0, len(dw.pids))
				for pid := range dw.pids {
					pids = append(pids, pid)
				}
				h.markFreshUnmatchedWakesLocked(dw.ownerID, dw.hostID, bk.sessionID, bk.pane, logs, pids, now)
				h.cancelDeferredWakeLocked(bk)
			}
		}
		for _, req := range deferredReqs {
			bk := convoBindKey{sessionID: req.b.sessionID, pane: req.b.pane}
			if _, relocating := relocKeys[bk]; relocating {
				continue
			}
			cur := h.bindings[bk]
			if cur == nil || cur.ownerID != req.b.ownerID || cur.hostID != req.b.hostID {
				continue
			}
			if !h.userWatchesHostLocked(cur.ownerID, cur.hostID) {
				continue
			}
			h.scheduleDeferredWakeLocked(cur, req.logs, req.pids, req.lastActivity, now, quietThreshold)
		}
		h.mu.Unlock()
	}

	// Mark convo states BEFORE emitting any convo hints so a browser's
	// immediate push-triggered GET is guaranteed to observe relocAfter and run
	// locateConvo right away instead of racing with hint delivery.
	if len(relocKeys) > 0 && h.s != nil {
		ts := now.UnixNano()
		for bk := range relocKeys {
			if st := h.s.convos.peek(bk.sessionID + "\x00" + bk.pane); st != nil {
				for {
					was := st.relocAfter.Load()
					if was >= ts || st.relocAfter.CompareAndSwap(was, ts) {
						break
					}
				}
			}
		}
	}

	for ck := range pendingHints {
		h.enqueueHint(ck)
	}
}

// NotifyAgentsChanged emits a coalesced {"t":"agents","host":hostID} hint to
// ownerID's browsers watching hostID when hostID is currently covered by
// corrald push (leaving behavior completely unchanged when corrald is off).
func (h *eventHub) NotifyAgentsChanged(ownerID, hostID int64) {
	if h == nil || ownerID <= 0 || hostID <= 0 {
		return
	}
	if h.s == nil || h.s.corrald == nil || !h.s.corrald.IsWatchCovered(ownerID, hostID) {
		return
	}
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return
	}
	hasWatcher := false
	for sub := range h.byUser[ownerID] {
		if sub.watchesHost(hostID) {
			hasWatcher = true
			break
		}
	}
	h.mu.Unlock()
	if !hasWatcher {
		return
	}
	h.enqueueHint(coalesceKey{
		kind:   "agents",
		userID: ownerID,
		hostID: hostID,
	})
}

func (h *eventHub) enqueueHint(key coalesceKey) {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return
	}
	if entry, ok := h.coalesce[key]; ok {
		if entry.pending {
			h.mu.Unlock()
			if h.s != nil && h.s.perf != nil {
				h.s.perf.Count("push.coalesced", key.kind, key.hostID)
			}
			return
		}
		entry.pending = true
		h.mu.Unlock()
		return
	}
	window := h.coalesceWindow
	if window <= 0 {
		window = DefaultEventCoalesceWindow
	}
	entry := &coalesceEntry{}
	entry.timer = time.AfterFunc(window, func() {
		h.onCoalesceExpire(key, entry)
	})
	h.coalesce[key] = entry
	h.mu.Unlock()

	h.deliverHint(key)
}

func (h *eventHub) onCoalesceExpire(key coalesceKey, entry *coalesceEntry) {
	h.mu.Lock()
	if h.closed || h.coalesce[key] != entry {
		h.mu.Unlock()
		return
	}
	if !entry.pending {
		delete(h.coalesce, key)
		h.mu.Unlock()
		return
	}
	entry.pending = false
	window := h.coalesceWindow
	if window <= 0 {
		window = DefaultEventCoalesceWindow
	}
	entry.timer = time.AfterFunc(window, func() {
		h.onCoalesceExpire(key, entry)
	})
	h.mu.Unlock()

	h.deliverHint(key)
}

func (h *eventHub) deliverHint(key coalesceKey) {
	var payload []byte
	var err error
	switch key.kind {
	case "convo":
		payload, err = json.Marshal(struct {
			T       string `json:"t"`
			Session string `json:"session"`
			Pane    string `json:"pane"`
		}{
			T:       "convo",
			Session: key.sessionID,
			Pane:    key.pane,
		})
	case "agents":
		payload, err = json.Marshal(struct {
			T    string `json:"t"`
			Host int64  `json:"host"`
		}{
			T:    "agents",
			Host: key.hostID,
		})
	default:
		return
	}
	if err != nil {
		return
	}

	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return
	}
	onDeliver := h.onDeliverHint
	var subs []*eventSub
	for sub := range h.byUser[key.userID] {
		if sub.watchesHost(key.hostID) {
			subs = append(subs, sub)
		}
	}
	h.mu.Unlock()

	if onDeliver != nil {
		onDeliver(key)
	}

	sent := 0
	for _, sub := range subs {
		if sub.trySendHint(payload) {
			sent++
		}
	}
	if sent > 0 && h.s != nil && h.s.perf != nil {
		for i := 0; i < sent; i++ {
			h.s.perf.Count("push.sent", key.kind, key.hostID)
		}
	}
}

// GET /api/events/ws
//
// Per-tab browser push channel delivering hint-only notifications:
//   - {"t":"convo","session":"<id>","pane":"<pane>"}
//   - {"t":"agents","host":<id>}
//   - {"t":"cover","hosts":[<id>,...]}
//
// Browser -> gateway:
//   - {"t":"watch","hosts":[<id>,...]}
//   - {"t":"ping"} -> {"t":"pong"}
func (s *Server) apiEventsSocket(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	hash := loginHash(r)
	expiry, err := s.auth.LoginExpiry(r.Context(), hash)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "unauthorized", "please sign in")
		return
	}
	ctx, cancel := context.WithDeadline(context.WithoutCancel(r.Context()), expiry)
	defer cancel()

	me := &liveSocket{loginHash: hash, cancel: cancel}
	untrack, ok := s.trackSocket(u.ID, me)
	if !ok {
		writeError(w, http.StatusTooManyRequests, "too_many_windows", "too many open terminal windows")
		return
	}
	defer untrack()

	if _, err := s.auth.CheckLogin(ctx, hash); err != nil {
		writeError(w, http.StatusUnauthorized, "unauthorized", "please sign in")
		return
	}

	// Origin was already verified against the configured site in guardRequest.
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		InsecureSkipVerify: true,
		CompressionMode:    websocket.CompressionDisabled,
	})
	if err != nil {
		return
	}
	c.SetReadLimit(64 << 10)
	defer c.Close(websocket.StatusNormalClosure, "")

	sub := newEventSub(u.ID)
	s.events.addSub(sub)
	defer s.events.removeSub(sub)

	// Always send initial coverage snapshot upon connect.
	s.events.sendCoverIfChanged(sub, true)

	// Periodic login recheck matching serveTerminal / apiPaneSocket.
	go func() {
		tick := time.NewTicker(loginRecheck)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				if _, err := s.auth.CheckLogin(ctx, hash); err != nil {
					cancel()
					return
				}
			}
		}
	}()

	var writeMu sync.Mutex
	writeRaw := func(payload []byte) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		wctx, wcancel := context.WithTimeout(ctx, 10*time.Second)
		defer wcancel()
		return c.Write(wctx, websocket.MessageText, payload)
	}

	// Writer goroutine draining cover updates (prioritized) and hints.
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		for {
			// Prefer delivering latest cover snapshot before queued hints.
			select {
			case p := <-sub.coverCh:
				if err := writeRaw(p); err != nil {
					cancel()
					return
				}
				continue
			default:
			}
			select {
			case <-ctx.Done():
				return
			case p := <-sub.coverCh:
				if err := writeRaw(p); err != nil {
					cancel()
					return
				}
			case p := <-sub.sendCh:
				if err := writeRaw(p); err != nil {
					cancel()
					return
				}
			}
		}
	}()

	// Read loop for {"t":"watch","hosts":[...]} and {"t":"ping"}.
	for {
		typ, data, rerr := c.Read(ctx)
		if rerr != nil {
			cancel()
			<-writerDone
			return
		}
		if typ != websocket.MessageText {
			continue
		}
		var msg struct {
			T     string  `json:"t"`
			Hosts []int64 `json:"hosts"`
		}
		if json.Unmarshal(data, &msg) != nil {
			continue
		}
		switch msg.T {
		case "ping":
			if err := writeRaw([]byte(`{"t":"pong"}`)); err != nil {
				cancel()
				<-writerDone
				return
			}
		case "watch":
			owned := s.filterOwnedHosts(ctx, u.ID, msg.Hosts)
			sub.mu.Lock()
			next := make(map[int64]struct{}, len(owned))
			for _, id := range owned {
				next[id] = struct{}{}
			}
			sub.watchedHosts = next
			sub.mu.Unlock()
			s.events.pruneUnwatchedDeferredWakes(u.ID)
			if s.corrald != nil {
				s.corrald.SetWatcher(sub, u.ID, owned)
			}
			s.events.sendCoverIfChanged(sub, false)
		}
	}
}

func (s *Server) filterOwnedHosts(ctx context.Context, userID int64, raw []int64) []int64 {
	if len(raw) == 0 {
		return nil
	}
	if len(raw) > maxWatchedHostsPerConn {
		raw = raw[:maxWatchedHostsPerConn]
	}
	hosts, err := s.store.ListHosts(ctx, userID)
	if err != nil || len(hosts) == 0 {
		return nil
	}
	ownedSet := make(map[int64]struct{}, len(hosts))
	for _, h := range hosts {
		ownedSet[h.ID] = struct{}{}
	}
	seen := make(map[int64]struct{}, len(raw))
	out := make([]int64, 0, len(raw))
	for _, id := range raw {
		if id <= 0 {
			continue
		}
		if _, ok := ownedSet[id]; !ok {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func (s *Server) recordConvoHintBinding(sess *term.Session, pane string, st *convoState) {
	if s == nil || s.events == nil || sess == nil || st == nil {
		return
	}
	keyPane := ""
	if sess.Persistence == "herdr" {
		keyPane = pane
	}
	bindPane := keyPane
	pid := 0
	if st.bind != nil {
		pid = int(st.bind.PID)
		if bindPane == "" {
			bindPane = st.bind.Pane
		}
	}
	s.events.RecordBindingWithActivity(sess.OwnerID, sess.HostID, sess.ID, keyPane, bindPane, pid, st.log, st.grewAt)
}

func convoReadTrigger(raw string) string {
	switch raw {
	case "push":
		return "push"
	case "poll":
		return "poll"
	default:
		return "other"
	}
}
