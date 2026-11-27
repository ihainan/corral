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

const (
	maxWatchedHostsPerConn = 512
	eventSubSendBuf        = 64
)

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
	s              *Server
	coalesceWindow time.Duration
	quietThreshold time.Duration

	mu       sync.Mutex
	closed   bool
	subs     map[*eventSub]struct{}
	byUser   map[int64]map[*eventSub]struct{}
	bindings map[convoBindKey]*convoHintBinding
	coalesce map[coalesceKey]*coalesceEntry
}

func newEventHub(s *Server) *eventHub {
	return &eventHub{
		s:              s,
		coalesceWindow: DefaultEventCoalesceWindow,
		quietThreshold: DefaultUnmatchedLogQuietThreshold,
		subs:           make(map[*eventSub]struct{}),
		byUser:         make(map[int64]map[*eventSub]struct{}),
		bindings:       make(map[convoBindKey]*convoHintBinding),
		coalesce:       make(map[coalesceKey]*coalesceEntry),
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

	matchedKeys := make(map[convoBindKey]struct{})
	relocKeys := make(map[convoBindKey]struct{})

	for hid, users := range usersByHost {
		for uid := range users {
			needAgents := false
			hasUnmatchedLog := false
			convoTargets := make(map[coalesceKey]struct{})
			directMatched := make(map[convoBindKey]struct{})

			for _, logPath := range ev.ChangedLogs {
				matched := false
				for _, b := range liveBindings {
					if b.ownerID == uid && b.hostID == hid && b.log != "" && b.log == logPath {
						convoTargets[coalesceKey{
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
					hasUnmatchedLog = true
				}
			}

			// When an unmatched log changes or a new/changed agent PID appears
			// on this host (e.g. a brand-new conversation's first log write or
			// a /clear / /resume log switch), also notify this user's bindings
			// on the same host that either have no log yet or whose bound log
			// has been quiet for at least quietThreshold — without disturbing
			// other actively streaming conversations on the same host.
			if hasUnmatchedLog || len(ev.AddedPIDs) > 0 {
				for _, b := range liveBindings {
					if b.ownerID != uid || b.hostID != hid {
						continue
					}
					bk := convoBindKey{sessionID: b.sessionID, pane: b.pane}
					if _, already := directMatched[bk]; already {
						continue
					}
					if b.log == "" || b.lastLogChange.IsZero() || now.Sub(b.lastLogChange) >= quietThreshold {
						convoTargets[coalesceKey{
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

			for _, pid := range ev.AddedPIDs {
				for _, b := range liveBindings {
					if b.ownerID == uid && b.hostID == hid && b.pid > 0 && b.pid == pid {
						convoTargets[coalesceKey{
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

			for _, pid := range ev.RemovedPIDs {
				for _, b := range liveBindings {
					if b.ownerID == uid && b.hostID == hid && b.pid > 0 && b.pid == pid {
						convoTargets[coalesceKey{
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

			for ck := range convoTargets {
				h.enqueueHint(ck)
			}
			if needAgents {
				h.enqueueHint(coalesceKey{
					kind:   "agents",
					userID: uid,
					hostID: hid,
				})
			}
		}
	}

	if len(matchedKeys) > 0 {
		h.mu.Lock()
		for bk := range matchedKeys {
			if b := h.bindings[bk]; b != nil && b.log != "" {
				b.lastLogChange = now
			}
		}
		h.mu.Unlock()
	}

	// Mark convo states whose pane likely created or switched logs so the
	// browser's immediate push-triggered GET runs locateConvo right away
	// instead of waiting for convoLocateTTL (10s).
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
	var subs []*eventSub
	for sub := range h.byUser[key.userID] {
		if sub.watchesHost(key.hostID) {
			subs = append(subs, sub)
		}
	}
	h.mu.Unlock()

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
