package web

import (
	"container/list"
	"context"
	"fmt"
	"path"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/ihainan/corral/internal/convo"
	"github.com/ihainan/corral/internal/herdr"
	"github.com/ihainan/corral/internal/store"
	"github.com/ihainan/corral/internal/term"
)

const (
	peerCacheMaxPerHost    = 512
	peerNegCacheMaxPerHost = 2048
	peerNegCacheTTL        = 60 * time.Second
	peerLastSeenRefresh    = time.Hour
)

type peerHostScope struct {
	ownerID    int64
	hostID     int64
	hostConfig string
}

type peerProcID struct {
	pid   int64
	start int64
}

type peerTitleKey struct {
	scope peerHostScope
	id    peerProcID
}

type observedPeer struct {
	info          store.PeerIdentity
	sessionID     string    // live Corral term.Session ID when observed (empty when loaded only from DB)
	tmuxServerPID string    // live tmux server PID when observed on tmux
	seenAt        time.Time // zero when loaded from DB and not yet confirmed alive in this gateway run
	observedAt    time.Time // scan/snapshot timestamp of the latest live observation
	stateAt       time.Time // latest scan/snapshot timestamp that set alive or exited state
	exited        bool      // true once a scan covering this scope confirmed exit or replacement
	gen           uint64    // monotonic observation generation for concurrent write ordering
}

type latestPeerOccupant struct {
	id            peerProcID
	sessionID     string
	tmuxServerPID string
	observedAt    time.Time
}

type persistedPeer struct {
	info     store.PeerIdentity
	gen      uint64
	lastSeen int64 // unix seconds last committed to SQLite
}

type peerScanScope struct {
	full            bool
	aliveInProcScan map[peerProcID]bool
	coveredHerdrMux map[string]bool
	coveredTmuxKeys map[string]bool
}

type peerNegEntry struct {
	id     peerProcID
	missAt time.Time
}

// peerNegLRU is a bounded per-scope LRU negative cache for peer (pid, start)
// lookups that missed SQLite. When full, only the least-recently-used entry is
// evicted rather than clearing the whole table.
type peerNegLRU struct {
	ll    *list.List
	items map[peerProcID]*list.Element
	cap   int
}

func newPeerNegLRU(capacity int) *peerNegLRU {
	if capacity <= 0 {
		capacity = peerNegCacheMaxPerHost
	}
	return &peerNegLRU{
		ll:    list.New(),
		items: make(map[peerProcID]*list.Element),
		cap:   capacity,
	}
}

func (c *peerNegLRU) hasFresh(id peerProcID, now time.Time, ttl time.Duration) bool {
	if c == nil {
		return false
	}
	el, ok := c.items[id]
	if !ok {
		return false
	}
	ent := el.Value.(*peerNegEntry)
	if now.Sub(ent.missAt) >= ttl {
		c.ll.Remove(el)
		delete(c.items, id)
		return false
	}
	c.ll.MoveToFront(el)
	return true
}

func (c *peerNegLRU) put(id peerProcID, now time.Time) {
	if c == nil {
		return
	}
	if el, ok := c.items[id]; ok {
		el.Value.(*peerNegEntry).missAt = now
		c.ll.MoveToFront(el)
		return
	}
	for c.ll.Len() >= c.cap {
		back := c.ll.Back()
		if back == nil {
			break
		}
		c.ll.Remove(back)
		delete(c.items, back.Value.(*peerNegEntry).id)
	}
	el := c.ll.PushFront(&peerNegEntry{id: id, missAt: now})
	c.items[id] = el
}

func (c *peerNegLRU) delete(id peerProcID) {
	if c == nil {
		return
	}
	if el, ok := c.items[id]; ok {
		c.ll.Remove(el)
		delete(c.items, id)
	}
}

// peerRegistry caches observed (owner, host, host_config, pid, proc_start) ->
// Corral agent mappings captured passively from Corral's existing locate scans
// and persists them to SQLite so historical cross-session messages remain
// stable across reloads, gateway restarts, and sender exits.
type peerRegistry struct {
	// persistMu serializes SQLite upserts so concurrent writers never commit an
	// older observation over a newer one. Lock order: persistMu -> mu.
	persistMu sync.Mutex

	mu          sync.Mutex
	nextGen     uint64
	known       map[peerHostScope]map[peerProcID]*observedPeer
	persisted   map[peerHostScope]map[peerProcID]persistedPeer
	latestByKey map[peerHostScope]map[string]latestPeerOccupant // agentKey -> latest picked live occupant
	titles      map[peerTitleKey]string                         // (scope, pid, start) -> verified convo.Title()
	negCache    map[peerHostScope]*peerNegLRU

	// Test hooks (nil/zero in production).
	nowFn       func() time.Time
	negCacheCap int
	persistFn   func(ctx context.Context, ownerID, hostID int64, hostConfig string, rows []store.PeerIdentity) error
	lookupFn    func(ctx context.Context, ownerID, hostID int64, hostConfig string, keys []store.PeerProcKey) (map[store.PeerProcKey]store.PeerIdentity, error)
}

func (r *peerRegistry) now() time.Time {
	if r.nowFn != nil {
		return r.nowFn()
	}
	return time.Now()
}

func peerHostConfig(sess *term.Session) string {
	if sess == nil {
		return ""
	}
	if sess.HostConfig != "" {
		return sess.HostConfig
	}
	if c := sess.Client(); c != nil {
		addr := ""
		if ra := c.RemoteAddr(); ra != nil {
			addr = ra.String()
		}
		if user := c.User(); user != "" || addr != "" {
			return "ssh:" + user + "@" + addr
		}
	}
	return ""
}

func peerScopeOf(sess *term.Session) peerHostScope {
	return peerHostScope{
		ownerID:    sess.OwnerID,
		hostID:     sess.HostID,
		hostConfig: peerHostConfig(sess),
	}
}

func (s *Server) runningInPeerScope(sess *term.Session, scope peerHostScope) []*term.Session {
	var filtered []*term.Session
	if s.terms != nil {
		for _, rs := range s.terms.Running(scope.ownerID, scope.hostID) {
			if peerHostConfig(rs) == scope.hostConfig {
				filtered = append(filtered, rs)
			}
		}
	}
	if len(filtered) == 0 && sess != nil {
		return []*term.Session{sess}
	}
	return filtered
}

func peerMapKey(pid, start int64) string {
	return strconv.FormatInt(pid, 10) + ":" + strconv.FormatInt(start, 10)
}

func sanitizePeerLabel(s string, maxRunes int) string {
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || (r >= 0x202a && r <= 0x202e) || (r >= 0x2066 && r <= 0x2069) {
			return -1
		}
		return r
	}, strings.TrimSpace(strings.ToValidUTF8(s, "")))
	if maxRunes > 0 {
		rs := []rune(s)
		if len(rs) > maxRunes {
			s = string(rs[:maxRunes])
		}
	}
	return s
}

var (
	peerLaunchCmdRe = regexp.MustCompile(`^(?:claude|codex|opencode|gemini|agy|devin)(?:\s+-\S+(?:\s+[^-\s]\S*)?)*$`)
	peerHomeDirRe   = regexp.MustCompile(`^(?:/home/[^/]+|/Users/[^/]+|/root)$`)
)

func cleanPeerPaneTitle(s string) string {
	s = sanitizePeerLabel(s, 160)
	s = strings.TrimLeftFunc(s, func(r rune) bool {
		if unicode.IsSpace(r) {
			return true
		}
		if r >= 0x2800 && r <= 0x28ff { // Braille spinner frames
			return true
		}
		switch r {
		case '·', '✢', '✳', '✶', '✻', '✽', '*', '◐', '◑', '◒', '◓', '○', '●', '◌':
			return true
		}
		return false
	})
	s = strings.TrimSpace(s)
	if peerLaunchCmdRe.MatchString(s) {
		return ""
	}
	return s
}

func peerDirName(cwd string) string {
	p := strings.TrimRight(strings.TrimSpace(cwd), "/")
	if p == "" || peerHomeDirRe.MatchString(p) {
		return ""
	}
	return sanitizePeerLabel(path.Base(p), 120)
}

func normalizeHerdrMux(mux string) string {
	if mux == "" || mux == "default" {
		return ""
	}
	return mux
}

func herdrAgentKey(hostID int64, muxSession, pane string) string {
	return fmt.Sprintf("%d|%s|%s", hostID, normalizeHerdrMux(muxSession), pane)
}

func tmuxAgentKey(hostID int64, muxSession string) string {
	return fmt.Sprintf("%d|tmux|%s", hostID, muxSession)
}

func normalizePeerProgram(agent string) string {
	a := strings.ToLower(strings.TrimSpace(agent))
	switch {
	case strings.Contains(a, "claude"):
		return "claude"
	case strings.Contains(a, "codex"):
		return "codex"
	case strings.Contains(a, "opencode"):
		return "opencode"
	case strings.HasPrefix(a, "agy") || strings.Contains(a, "antigravity"):
		return "agy"
	case strings.HasPrefix(a, "devin"):
		return "devin"
	default:
		return a
	}
}

// samePeerProgram is fail-closed: both sides must identify a known program and
// match after normalization.
func samePeerProgram(a, b string) bool {
	na, nb := normalizePeerProgram(a), normalizePeerProgram(b)
	return na != "" && nb != "" && na == nb
}

func peerTitleFromHerdr(wsLabel, paneLabel, convoTitle, paneTitle, agentName, cwd, agent string) string {
	if t := sanitizePeerLabel(paneLabel, 120); t != "" {
		return t
	}
	if ws := sanitizePeerLabel(wsLabel, 120); ws != "" && ws != "~" {
		return ws
	}
	if t := sanitizePeerLabel(convoTitle, 160); t != "" {
		return t
	}
	if t := cleanPeerPaneTitle(paneTitle); t != "" {
		return t
	}
	if t := sanitizePeerLabel(agentName, 120); t != "" {
		return t
	}
	if t := peerDirName(cwd); t != "" {
		return t
	}
	return sanitizePeerLabel(agent, 64)
}

func peerTitleFromTmux(convoTitle, paneTitle, displayName, muxSession, agent string) string {
	if t := sanitizePeerLabel(convoTitle, 160); t != "" {
		return t
	}
	if t := cleanPeerPaneTitle(paneTitle); t != "" {
		return t
	}
	if t := sanitizePeerLabel(displayName, 120); t != "" {
		return t
	}
	if t := sanitizePeerLabel(muxSession, 120); t != "" {
		return t
	}
	return sanitizePeerLabel(agent, 64)
}

// herdrSnapIndex indexes a herdr.Snapshot once so pane & workspace lookups are
// O(1) instead of rescanning every workspace and pane per process.
type herdrSnapIndex struct {
	wsLabels map[string]string
	panes    map[string]herdr.Pane
}

func indexHerdrSnapshot(snap *herdr.Snapshot) *herdrSnapIndex {
	if snap == nil {
		return nil
	}
	wsLabels := make(map[string]string, len(snap.Workspaces))
	for _, w := range snap.Workspaces {
		wsLabels[w.ID] = w.Label
	}
	panes := make(map[string]herdr.Pane, len(snap.Agents)+len(snap.Panes))
	// Panes first, then Agents overwrite so Agents take precedence (matching paneOf).
	for _, p := range snap.Panes {
		if p.ID != "" {
			panes[p.ID] = p
		}
	}
	for _, p := range snap.Agents {
		if p.ID != "" {
			panes[p.ID] = p
		}
	}
	return &herdrSnapIndex{wsLabels: wsLabels, panes: panes}
}

func (idx *herdrSnapIndex) lookup(paneID string) (herdr.Pane, string, bool) {
	if idx == nil || paneID == "" {
		return herdr.Pane{}, "", false
	}
	p, ok := idx.panes[paneID]
	if !ok {
		return herdr.Pane{}, "", false
	}
	return p, idx.wsLabels[p.WorkspaceID], true
}

func (r *peerRegistry) noteConvoTitleLocked(scope peerHostScope, id peerProcID, title string) {
	t := sanitizePeerLabel(title, 160)
	if id.pid <= 0 || id.start <= 0 || t == "" {
		return
	}
	if r.titles == nil {
		r.titles = map[peerTitleKey]string{}
	}
	if len(r.titles) >= 2048 {
		r.titles = map[peerTitleKey]string{}
	}
	r.titles[peerTitleKey{scope: scope, id: id}] = t
}

func (r *peerRegistry) convoTitleOf(scope peerHostScope, id peerProcID) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.titles == nil {
		return ""
	}
	return r.titles[peerTitleKey{scope: scope, id: id}]
}

// record updates in-memory observed peer state from a scan taken at observedAt
// and returns the process IDs that need to be persisted to SQLite. Stale
// late-arriving scans (observedAt older than already-recorded state) are
// ignored and never overwrite newer metadata or resurrect exited processes.
// It deliberately does NOT mark rows as persisted before SQLite commits.
func (r *peerRegistry) record(scope peerHostScope, items []observedPeer, scan peerScanScope, observedAt time.Time) []peerProcID {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.known == nil {
		r.known = map[peerHostScope]map[peerProcID]*observedPeer{}
	}
	if r.persisted == nil {
		r.persisted = map[peerHostScope]map[peerProcID]persistedPeer{}
	}
	if r.latestByKey == nil {
		r.latestByKey = map[peerHostScope]map[string]latestPeerOccupant{}
	}
	km := r.known[scope]
	if km == nil {
		km = map[peerProcID]*observedPeer{}
		r.known[scope] = km
	}
	pm := r.persisted[scope]
	if pm == nil {
		pm = map[peerProcID]persistedPeer{}
		r.persisted[scope] = pm
	}
	lm := r.latestByKey[scope]
	if lm == nil {
		lm = map[string]latestPeerOccupant{}
		r.latestByKey[scope] = lm
	}
	nm := r.negCache[scope]

	nowSec := observedAt.Unix()
	obsMs := observedAt.UnixMilli()
	seenItems := make(map[peerProcID]bool, len(items))
	pickedByKey := make(map[string]peerProcID, len(items))
	needPersist := make(map[peerProcID]struct{})

	for _, item := range items {
		if item.info.PID <= 0 || item.info.ProcStart <= 0 {
			continue
		}
		id := peerProcID{pid: item.info.PID, start: item.info.ProcStart}
		if prev := km[id]; prev != nil && observedAt.Before(prev.stateAt) {
			// Late-arriving older scan: never overwrite newer observation or
			// resurrect a process already confirmed exited by a newer scan.
			continue
		}
		seenItems[id] = true
		if nm != nil {
			nm.delete(id)
		}
		if prev := km[id]; prev != nil && prev.info.AgentKey == item.info.AgentKey {
			if item.info.Title == "" && prev.info.Title != "" {
				item.info.Title = prev.info.Title
			}
			if item.info.AgentSession == "" && prev.info.AgentSession != "" {
				item.info.AgentSession = prev.info.AgentSession
			}
			if item.info.TerminalID == "" && prev.info.TerminalID != "" {
				item.info.TerminalID = prev.info.TerminalID
			}
			if item.info.Program == "" && prev.info.Program != "" {
				item.info.Program = prev.info.Program
			}
		}
		item.info.HostConfig = scope.hostConfig
		item.info.ObservedAt = obsMs
		item.info.LastSeen = nowSec
		if item.info.UpdatedAt <= 0 {
			item.info.UpdatedAt = nowSec
		}
		r.nextGen++
		item.gen = r.nextGen
		item.seenAt = observedAt
		item.observedAt = observedAt
		item.stateAt = observedAt
		item.exited = false
		cp := item
		km[id] = &cp
		if item.info.AgentKey != "" {
			pickedByKey[item.info.AgentKey] = id
			if prevOcc, ok := lm[item.info.AgentKey]; !ok || !observedAt.Before(prevOcc.observedAt) {
				lm[item.info.AgentKey] = latestPeerOccupant{
					id:            id,
					sessionID:     item.sessionID,
					tmuxServerPID: item.tmuxServerPID,
					observedAt:    observedAt,
				}
			}
		}
		if prevP, ok := pm[id]; !ok || !prevP.info.SameSnapshot(item.info) || nowSec-prevP.lastSeen >= int64(peerLastSeenRefresh/time.Second) {
			needPersist[id] = struct{}{}
		}
	}

	// If a tmux session was explicitly scanned and no process won pickConvoProc
	// for its active pane, clear its current occupant and mark prior occupants
	// exited (unless a newer scan already updated them).
	for tkey := range scan.coveredTmuxKeys {
		if _, picked := pickedByKey[tkey]; picked {
			continue
		}
		if prevOcc, ok := lm[tkey]; ok && !observedAt.Before(prevOcc.observedAt) {
			delete(lm, tkey)
		}
		for id, obs := range km {
			if seenItems[id] || obs.info.AgentKey != tkey {
				continue
			}
			if observedAt.Before(obs.stateAt) {
				continue
			}
			obs.exited = true
			obs.stateAt = observedAt
		}
	}

	// Keep previously known tmux peers alive when their (pid, start) is still
	// present in a host-wide /proc scan (even when the scan ran from a herdr
	// session and had no tmuxActive metadata), provided they are still the
	// current verified occupant and this scan is not older than their state.
	if len(scan.aliveInProcScan) > 0 {
		for id, obs := range km {
			if seenItems[id] || obs.info.Persistence != "tmux" || obs.exited || obs.seenAt.IsZero() {
				continue
			}
			if observedAt.Before(obs.stateAt) {
				continue
			}
			if !scan.aliveInProcScan[id] {
				continue
			}
			if scan.coveredTmuxKeys[obs.info.AgentKey] {
				// This tmux session was explicitly scanned and another process
				// (or no process) won pickConvoProc for its active pane.
				continue
			}
			occ, ok := lm[obs.info.AgentKey]
			if !ok || occ.id != id || occ.sessionID != obs.sessionID {
				continue
			}
			obs.seenAt = observedAt
			obs.observedAt = observedAt
			obs.stateAt = observedAt
			obs.exited = false
			obs.info.ObservedAt = obsMs
			obs.info.LastSeen = nowSec
			if prevP, ok := pm[id]; !ok || nowSec-prevP.lastSeen >= int64(peerLastSeenRefresh/time.Second) {
				r.nextGen++
				obs.gen = r.nextGen
				needPersist[id] = struct{}{}
			}
		}
	}

	if scan.full {
		for id, obs := range km {
			if seenItems[id] {
				continue
			}
			if observedAt.Before(obs.stateAt) {
				// Never let a late-arriving older full scan mark a newer
				// observation as exited.
				continue
			}
			markExited := false
			switch obs.info.Persistence {
			case "herdr":
				if !scan.aliveInProcScan[id] || scan.coveredHerdrMux[normalizeHerdrMux(obs.info.MuxSession)] {
					markExited = true
				}
			case "tmux":
				if !scan.aliveInProcScan[id] || scan.coveredTmuxKeys[obs.info.AgentKey] {
					markExited = true
				}
			default:
				if !scan.aliveInProcScan[id] {
					markExited = true
				}
			}
			if markExited {
				obs.exited = true
				obs.stateAt = observedAt
				if occ, ok := lm[obs.info.AgentKey]; ok && occ.id == id && !observedAt.Before(occ.observedAt) {
					delete(lm, obs.info.AgentKey)
				}
			}
		}
	}

	// Bound per-host in-memory cache size.
	if len(km) > peerCacheMaxPerHost {
		for id, obs := range km {
			if len(km) <= peerCacheMaxPerHost {
				break
			}
			if obs.exited || obs.seenAt.IsZero() {
				delete(km, id)
				delete(pm, id)
			}
		}
	}

	if len(needPersist) == 0 {
		return nil
	}
	out := make([]peerProcID, 0, len(needPersist))
	for id := range needPersist {
		out = append(out, id)
	}
	return out
}

func (r *peerRegistry) recordLoadedFromDB(scope peerHostScope, rows map[store.PeerProcKey]store.PeerIdentity, queried []store.PeerProcKey, now time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.known == nil {
		r.known = map[peerHostScope]map[peerProcID]*observedPeer{}
	}
	if r.persisted == nil {
		r.persisted = map[peerHostScope]map[peerProcID]persistedPeer{}
	}
	km := r.known[scope]
	if km == nil {
		km = map[peerProcID]*observedPeer{}
		r.known[scope] = km
	}
	pm := r.persisted[scope]
	if pm == nil {
		pm = map[peerProcID]persistedPeer{}
		r.persisted[scope] = pm
	}
	for k, row := range rows {
		id := peerProcID{pid: k.PID, start: k.ProcStart}
		if _, exists := km[id]; !exists {
			var obsAt time.Time
			if row.ObservedAt > 0 {
				obsAt = time.UnixMilli(row.ObservedAt)
			}
			km[id] = &observedPeer{info: row, observedAt: obsAt, stateAt: obsAt}
		}
		if _, exists := pm[id]; !exists {
			seen := row.LastSeen
			if seen <= 0 {
				seen = row.UpdatedAt
			}
			pm[id] = persistedPeer{info: row, lastSeen: seen}
		}
		if nm := r.negCache[scope]; nm != nil {
			nm.delete(id)
		}
	}
	// Record negative cache entries for keys queried from SQLite that did not exist.
	for _, k := range queried {
		if _, found := rows[k]; found {
			continue
		}
		id := peerProcID{pid: k.PID, start: k.ProcStart}
		if km[id] != nil {
			continue
		}
		if r.negCache == nil {
			r.negCache = map[peerHostScope]*peerNegLRU{}
		}
		nm := r.negCache[scope]
		if nm == nil {
			nm = newPeerNegLRU(r.negCacheCap)
			r.negCache[scope] = nm
		}
		nm.put(id, now)
	}
}

// persistPeerIdentities writes pending peer observations to SQLite under
// persistMu so concurrent callers never overwrite a newer observation with an
// older one, and marks rows persisted ONLY after SQLite commits cleanly.
func (s *Server) persistPeerIdentities(scope peerHostScope, ids []peerProcID) {
	if s == nil || len(ids) == 0 || (s.store == nil && s.peers.persistFn == nil) {
		return
	}

	s.peers.persistMu.Lock()
	defer s.peers.persistMu.Unlock()

	type writeItem struct {
		id  peerProcID
		row store.PeerIdentity
		gen uint64
	}
	var batch []writeItem

	s.peers.mu.Lock()
	km := s.peers.known[scope]
	pm := s.peers.persisted[scope]
	for _, id := range ids {
		obs := km[id]
		if obs == nil || obs.info.PID <= 0 || obs.info.ProcStart <= 0 {
			continue
		}
		if prevP, ok := pm[id]; ok {
			if obs.gen <= prevP.gen {
				continue
			}
			if prevP.info.SameSnapshot(obs.info) && obs.info.LastSeen-prevP.lastSeen < int64(peerLastSeenRefresh/time.Second) {
				continue
			}
		}
		batch = append(batch, writeItem{id: id, row: obs.info, gen: obs.gen})
	}
	s.peers.mu.Unlock()

	if len(batch) == 0 {
		return
	}
	rows := make([]store.PeerIdentity, len(batch))
	for i, w := range batch {
		rows[i] = w.row
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var err error
	if s.peers.persistFn != nil {
		err = s.peers.persistFn(ctx, scope.ownerID, scope.hostID, scope.hostConfig, rows)
	} else {
		err = s.store.UpsertPeerIdentities(ctx, scope.ownerID, scope.hostID, scope.hostConfig, rows)
	}
	if err != nil {
		if s.log != nil {
			s.log.Warn("peer identities: persist failed", "owner", scope.ownerID, "host", scope.hostID, "count", len(rows), "err", err)
		}
		return
	}

	s.peers.mu.Lock()
	if s.peers.persisted == nil {
		s.peers.persisted = map[peerHostScope]map[peerProcID]persistedPeer{}
	}
	pm = s.peers.persisted[scope]
	if pm == nil {
		pm = map[peerProcID]persistedPeer{}
		s.peers.persisted[scope] = pm
	}
	for _, w := range batch {
		if prev, ok := pm[w.id]; !ok || w.gen >= prev.gen {
			pm[w.id] = persistedPeer{info: w.row, gen: w.gen, lastSeen: w.row.LastSeen}
		}
	}
	s.peers.mu.Unlock()
}

// recordLocatedProcs updates the server's peer identity registry from an
// existing locate scan's results (zero extra remote calls). Only the primary
// process selected by pickConvoProc for each Corral-managed pane/tmux session
// whose full positive identity evidence is present is mapped; child/background
// processes sharing HERDR_PANE_ID and panes with incomplete snapshots are
// ignored.
func (s *Server) recordLocatedProcs(sess *term.Session, l convoLocated, full bool, observedAt time.Time) {
	if s == nil || sess == nil || sess.OwnerID <= 0 || sess.HostID <= 0 || l.noProc {
		return
	}
	if observedAt.IsZero() {
		observedAt = s.peers.now()
	}
	scope := peerScopeOf(sess)
	running := s.runningInPeerScope(sess, scope)

	herdrBySock := map[string]*term.Session{}
	snapIndexes := map[string]*herdrSnapIndex{}
	coveredHerdrMux := map[string]bool{}
	for _, rs := range running {
		if rs.Persistence == "herdr" {
			if sock := s.herdrSocks.get(rs.ID); sock != "" {
				herdrBySock[sock] = rs
				coveredHerdrMux[normalizeHerdrMux(rs.Info().MuxSession)] = true
			}
			if snap, ok := s.herdrSocks.snapshotOf(rs.ID); ok {
				snapIndexes[rs.ID] = indexHerdrSnapshot(snap)
			}
		}
	}

	aliveInProcScan := make(map[peerProcID]bool, len(l.procs))
	type herdrPaneKey struct {
		socket string
		pane   string
	}
	seenPanes := map[herdrPaneKey]struct{}{}
	for _, p := range l.procs {
		if p.pid <= 0 || p.start <= 0 {
			continue
		}
		aliveInProcScan[peerProcID{pid: p.pid, start: p.start}] = true
		if p.herdrPane != "" && p.herdrSocket != "" && herdrBySock[p.herdrSocket] != nil {
			seenPanes[herdrPaneKey{socket: p.herdrSocket, pane: p.herdrPane}] = struct{}{}
		}
	}

	var items []observedPeer
	for pk := range seenPanes {
		rs := herdrBySock[pk.socket]
		if rs == nil {
			continue
		}
		// Only the primary process chosen by pickConvoProc owns this pane.
		// Background/child Claude processes inheriting HERDR_PANE_ID are never
		// mapped onto their parent's pane.
		p := pickConvoProc(l.procs, "herdr", pk.pane, pk.socket, "", "", s.herdrSocks.sid(rs.ID, pk.pane))
		if p == nil || p.pid <= 0 || p.start <= 0 {
			continue
		}
		hp, wsLabel, paneExists := snapIndexes[rs.ID].lookup(pk.pane)
		if !paneExists {
			continue
		}
		candidate := store.PeerIdentity{
			Program:      sanitizePeerLabel(p.agent, 64),
			AgentSession: sanitizePeerLabel(p.sid, 128),
			TerminalID:   sanitizePeerLabel(hp.TerminalID, 128),
		}
		// Fail-closed: only record a herdr peer identity when the current pane
		// snapshot has complete, matching positive evidence (program, terminal
		// ID, and agent session).
		if !peerMatchesHerdrPane(candidate, hp, s.herdrSocks.locatedOf(rs.ID, pk.pane)) {
			continue
		}
		info := rs.Info()
		convoTitle := s.peers.convoTitleOf(scope, peerProcID{pid: p.pid, start: p.start})
		title := peerTitleFromHerdr(wsLabel, hp.Label, convoTitle, hp.Title, hp.Name, p.cwd, p.agent)
		if wsLabel == "~" {
			wsLabel = ""
		}
		items = append(items, observedPeer{
			sessionID: rs.ID,
			info: store.PeerIdentity{
				OwnerID:      sess.OwnerID,
				HostID:       sess.HostID,
				HostConfig:   scope.hostConfig,
				PID:          p.pid,
				ProcStart:    p.start,
				AgentKey:     herdrAgentKey(rs.HostID, info.MuxSession, pk.pane),
				Persistence:  "herdr",
				MuxSession:   normalizeHerdrMux(info.MuxSession),
				DisplayName:  sanitizePeerLabel(info.DisplayName, 120),
				Pane:         pk.pane,
				HostName:     sanitizePeerLabel(info.HostName, 120),
				Workspace:    sanitizePeerLabel(wsLabel, 120),
				Title:        title,
				Program:      candidate.Program,
				AgentSession: candidate.AgentSession,
				TerminalID:   candidate.TerminalID,
			},
		})
	}

	coveredTmuxKeys := map[string]bool{}
	if sess.Persistence == "tmux" && (full || l.tmuxActive != "") {
		info := sess.Info()
		tkey := tmuxAgentKey(sess.HostID, info.MuxSession)
		coveredTmuxKeys[tkey] = true
		if l.tmuxActive != "" && l.tmuxPID != "" {
			if p := pickConvoProc(l.procs, "tmux", l.tmuxActive, "", l.tmuxPID, l.tmuxTTY, ""); p != nil && p.pid > 0 && p.start > 0 && normalizePeerProgram(p.agent) != "" {
				convoTitle := s.peers.convoTitleOf(scope, peerProcID{pid: p.pid, start: p.start})
				title := peerTitleFromTmux(convoTitle, l.tmuxTitle, info.DisplayName, info.MuxSession, p.agent)
				items = append(items, observedPeer{
					sessionID:     sess.ID,
					tmuxServerPID: l.tmuxPID,
					info: store.PeerIdentity{
						OwnerID:      sess.OwnerID,
						HostID:       sess.HostID,
						HostConfig:   scope.hostConfig,
						PID:          p.pid,
						ProcStart:    p.start,
						AgentKey:     tkey,
						Persistence:  "tmux",
						MuxSession:   sanitizePeerLabel(info.MuxSession, 120),
						DisplayName:  sanitizePeerLabel(info.DisplayName, 120),
						Pane:         p.tmuxPane,
						HostName:     sanitizePeerLabel(info.HostName, 120),
						Title:        title,
						Program:      sanitizePeerLabel(p.agent, 64),
						AgentSession: sanitizePeerLabel(p.sid, 128),
					},
				})
			}
		}
	}

	needPersist := s.peers.record(scope, items, peerScanScope{
		full:            full,
		aliveInProcScan: aliveInProcScan,
		coveredHerdrMux: coveredHerdrMux,
		coveredTmuxKeys: coveredTmuxKeys,
	}, observedAt)
	if len(needPersist) > 0 {
		s.persistPeerIdentities(scope, needPersist)
	}
}

// recordBoundProc updates the remembered title/session metadata for an ALREADY
// verified live peer process (one currently selected by pickConvoProc in
// latestByKey for sess's pane/tmux session). Fail-closed: if the binding does
// not match the pane's current (pid, start) occupant or live herdr pane
// identity, it writes nothing and merges nothing.
func (s *Server) recordBoundProc(sess *term.Session, b *convoBind, convoTitle string, observedAt time.Time) {
	if s == nil || sess == nil || b == nil || b.PID <= 0 || b.Start <= 0 || sess.OwnerID <= 0 || sess.HostID <= 0 {
		return
	}
	if normalizePeerProgram(b.Agent) == "" {
		return
	}
	if observedAt.IsZero() {
		observedAt = s.peers.now()
	}
	scope := peerScopeOf(sess)
	id := peerProcID{pid: b.PID, start: b.Start}
	info := sess.Info()

	var agentKey, title, wsLabel, termID, agentSID string
	switch sess.Persistence {
	case "herdr":
		if b.Pane == "" {
			return
		}
		snap, ok := s.herdrSocks.snapshotOf(sess.ID)
		if !ok {
			return
		}
		hp, ws, paneExists := indexHerdrSnapshot(snap).lookup(b.Pane)
		if !paneExists {
			return
		}
		candidate := store.PeerIdentity{
			Program:      sanitizePeerLabel(b.Agent, 64),
			AgentSession: sanitizePeerLabel(b.Session, 128),
			TerminalID:   sanitizePeerLabel(b.TerminalID, 128),
		}
		if !peerMatchesHerdrPane(candidate, hp, s.herdrSocks.locatedOf(sess.ID, b.Pane)) {
			return
		}
		agentKey = herdrAgentKey(sess.HostID, info.MuxSession, b.Pane)
		wsLabel = ws
		termID = candidate.TerminalID
		agentSID = candidate.AgentSession
		cleanConvoTitle := sanitizePeerLabel(convoTitle, 160)
		if cleanConvoTitle == "" {
			cleanConvoTitle = s.peers.convoTitleOf(scope, id)
		}
		title = peerTitleFromHerdr(wsLabel, hp.Label, cleanConvoTitle, hp.Title, hp.Name, hp.Cwd, b.Agent)
		if wsLabel == "~" {
			wsLabel = ""
		}
	case "tmux":
		agentKey = tmuxAgentKey(sess.HostID, info.MuxSession)
		agentSID = sanitizePeerLabel(b.Session, 128)
		cleanConvoTitle := sanitizePeerLabel(convoTitle, 160)
		if cleanConvoTitle == "" {
			cleanConvoTitle = s.peers.convoTitleOf(scope, id)
		}
		title = peerTitleFromTmux(cleanConvoTitle, b.Title, info.DisplayName, info.MuxSession, b.Agent)
	default:
		return
	}

	var shouldPersist bool
	s.peers.mu.Lock()
	km := s.peers.known[scope]
	lm := s.peers.latestByKey[scope]
	pm := s.peers.persisted[scope]
	occ, hasOcc := lm[agentKey]
	obs := km[id]
	if hasOcc && occ.id == id && occ.sessionID == sess.ID && obs != nil && !obs.exited && !obs.seenAt.IsZero() {
		// Verified: b matches the exact current (pid, start) picked by
		// pickConvoProc for this pane/tmux session.
		if convoTitle != "" {
			s.peers.noteConvoTitleLocked(scope, id, convoTitle)
		}
		updated := obs.info
		updated.HostConfig = scope.hostConfig
		updated.HostName = sanitizePeerLabel(info.HostName, 120)
		updated.DisplayName = sanitizePeerLabel(info.DisplayName, 120)
		if wsLabel != "" {
			updated.Workspace = sanitizePeerLabel(wsLabel, 120)
		}
		if title != "" {
			updated.Title = title
		}
		if agentSID != "" {
			updated.AgentSession = agentSID
		}
		if termID != "" {
			updated.TerminalID = termID
		}
		// Never fabricate a newer locate scan timestamp than observedAt; only
		// advance observedAt/stateAt/LastSeen when observedAt is strictly newer.
		if observedAt.After(obs.observedAt) {
			nowSec := observedAt.Unix()
			updated.ObservedAt = observedAt.UnixMilli()
			updated.LastSeen = nowSec
			if updated.UpdatedAt < nowSec {
				updated.UpdatedAt = nowSec
			}
			obs.seenAt = observedAt
			obs.observedAt = observedAt
			if observedAt.After(obs.stateAt) {
				obs.stateAt = observedAt
			}
		} else if nowSec := s.peers.now().Unix(); updated.UpdatedAt < nowSec {
			updated.UpdatedAt = nowSec
		}
		changed := !obs.info.SameSnapshot(updated)
		obs.info = updated
		if prevP, ok := pm[id]; !ok || changed || !prevP.info.SameSnapshot(updated) || updated.LastSeen-prevP.lastSeen >= int64(peerLastSeenRefresh/time.Second) {
			s.peers.nextGen++
			obs.gen = s.peers.nextGen
			shouldPersist = true
		}
	}
	s.peers.mu.Unlock()

	if shouldPersist {
		s.persistPeerIdentities(scope, []peerProcID{id})
	}
}

// peerMatchesHerdrPane is strictly fail-closed: it returns true ONLY when the
// live herdr pane snapshot carries complete, non-empty, matching positive
// evidence for program, terminal ID, and agent session (and agrees with any
// locatedSID). Missing or mismatched fields always return false.
func peerMatchesHerdrPane(info store.PeerIdentity, hp herdr.Pane, locatedSID string) bool {
	if hp.Agent == "" || hp.AgentStatus == herdr.StatusGone {
		return false
	}
	if !samePeerProgram(info.Program, hp.Agent) {
		return false
	}
	if info.TerminalID == "" || hp.TerminalID == "" || hp.TerminalID != info.TerminalID {
		return false
	}
	snapSID := agentSession(hp)
	if info.AgentSession == "" || snapSID == "" || snapSID != info.AgentSession {
		return false
	}
	if locatedSID != "" && locatedSID != info.AgentSession {
		return false
	}
	return true
}

// resolveConvoPeers builds the peer lookup table for all verified peer
// references present in st.parser (and queued items), combining in-memory
// observations, SQLite history (with LRU negative caching), and strict
// fail-closed live Corral session/pane identity verification. Never triggers
// remote calls.
func (s *Server) resolveConvoPeers(ctx context.Context, sess *term.Session, st *convoState) map[string]convo.PeerInfo {
	if s == nil || sess == nil || st == nil || st.parser == nil || sess.OwnerID <= 0 || sess.HostID <= 0 {
		return nil
	}
	refs := convo.PeerRefs(st.parser)
	if len(refs) == 0 {
		return nil
	}
	scope := peerScopeOf(sess)
	now := s.peers.now()

	// Step 1: check in-memory cache and LRU negative cache before querying SQLite.
	var dbKeys []store.PeerProcKey
	s.peers.mu.Lock()
	km := s.peers.known[scope]
	nm := s.peers.negCache[scope]
	for _, ref := range refs {
		id := peerProcID{pid: ref.PID, start: ref.Start}
		if km != nil && km[id] != nil {
			continue
		}
		if nm != nil && nm.hasFresh(id, now, peerNegCacheTTL) {
			continue
		}
		dbKeys = append(dbKeys, store.PeerProcKey{PID: ref.PID, ProcStart: ref.Start})
	}
	s.peers.mu.Unlock()

	if len(dbKeys) > 0 && (s.store != nil || s.peers.lookupFn != nil) {
		var dbRows map[store.PeerProcKey]store.PeerIdentity
		var err error
		if s.peers.lookupFn != nil {
			dbRows, err = s.peers.lookupFn(ctx, scope.ownerID, scope.hostID, scope.hostConfig, dbKeys)
		} else {
			dbRows, err = s.store.LookupPeerIdentities(ctx, scope.ownerID, scope.hostID, scope.hostConfig, dbKeys)
		}
		if err == nil {
			s.peers.recordLoadedFromDB(scope, dbRows, dbKeys, now)
		}
	}

	// Step 2: gather currently running Corral sessions in the SAME host config
	// scope and index each herdr snapshot once (O(N)) to verify whether each
	// matched peer is still alive and holds the exact same pane/process
	// identity.
	running := s.runningInPeerScope(sess, scope)
	liveHerdr := map[string]*term.Session{} // normalized mux -> session
	liveTmux := map[string]*term.Session{}  // mux -> session
	snapIndexes := map[string]*herdrSnapIndex{}
	for _, rs := range running {
		info := rs.Info()
		switch rs.Persistence {
		case "herdr":
			liveHerdr[normalizeHerdrMux(info.MuxSession)] = rs
			if snap, ok := s.herdrSocks.snapshotOf(rs.ID); ok {
				snapIndexes[rs.ID] = indexHerdrSnapshot(snap)
			}
		case "tmux":
			liveTmux[info.MuxSession] = rs
		}
	}

	out := map[string]convo.PeerInfo{}

	s.peers.mu.Lock()
	km = s.peers.known[scope]
	lm := s.peers.latestByKey[scope]
	for _, ref := range refs {
		id := peerProcID{pid: ref.PID, start: ref.Start}
		obs := km[id]
		if obs == nil {
			continue
		}
		pi := convo.PeerInfo{
			Key:          obs.info.AgentKey,
			HostID:       obs.info.HostID,
			HostName:     obs.info.HostName,
			Persistence:  obs.info.Persistence,
			MuxSession:   obs.info.MuxSession,
			DisplayName:  obs.info.DisplayName,
			Pane:         obs.info.Pane,
			Workspace:    obs.info.Workspace,
			Title:        obs.info.Title,
			Program:      obs.info.Program,
			AgentSession: obs.info.AgentSession,
			TerminalID:   obs.info.TerminalID,
		}
		// Fail-closed liveness: positive evidence is required on every check.
		// This exact (pid, start) must still be the current pickConvoProc
		// occupant for its agentKey on the exact same live Corral session.
		if !obs.seenAt.IsZero() && !obs.exited && obs.info.AgentKey != "" && obs.sessionID != "" && lm != nil {
			occ, ok := lm[obs.info.AgentKey]
			if ok && occ.id == id && occ.sessionID == obs.sessionID {
				switch obs.info.Persistence {
				case "herdr":
					if rs := liveHerdr[normalizeHerdrMux(obs.info.MuxSession)]; rs != nil && rs.ID == obs.sessionID {
						snapIdx := snapIndexes[rs.ID]
						hp, wsLabel, paneExists := snapIdx.lookup(obs.info.Pane)
						locatedSID := s.herdrSocks.locatedOf(rs.ID, obs.info.Pane)
						if paneExists && peerMatchesHerdrPane(obs.info, hp, locatedSID) {
							info := rs.Info()
							pi.Alive = true
							pi.SessionID = rs.ID
							pi.HostName = sanitizePeerLabel(info.HostName, 120)
							pi.DisplayName = sanitizePeerLabel(info.DisplayName, 120)
							if wsLabel != "" && wsLabel != "~" {
								pi.Workspace = sanitizePeerLabel(wsLabel, 120)
							}
							pi.TerminalID = sanitizePeerLabel(hp.TerminalID, 128)
							if t := peerTitleFromHerdr(wsLabel, hp.Label, s.peers.titles[peerTitleKey{scope: scope, id: id}], hp.Title, hp.Name, hp.Cwd, pi.Program); t != "" {
								pi.Title = t
							}
						}
					}
				case "tmux":
					if rs := liveTmux[obs.info.MuxSession]; rs != nil && rs.ID == obs.sessionID && normalizePeerProgram(obs.info.Program) != "" {
						if obs.tmuxServerPID == "" || occ.tmuxServerPID == "" || obs.tmuxServerPID == occ.tmuxServerPID {
							info := rs.Info()
							pi.Alive = true
							pi.SessionID = rs.ID
							pi.HostName = sanitizePeerLabel(info.HostName, 120)
							pi.DisplayName = sanitizePeerLabel(info.DisplayName, 120)
						}
					}
				}
			}
		}
		out[peerMapKey(ref.PID, ref.Start)] = pi
	}
	s.peers.mu.Unlock()

	if len(out) == 0 {
		return nil
	}
	return out
}
