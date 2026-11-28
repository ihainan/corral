package web

import (
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
	peerCacheMaxPerHost = 512
	peerNegCacheTTL     = 60 * time.Second
	peerLastSeenRefresh = time.Hour
)

type peerHostScope struct {
	ownerID int64
	hostID  int64
}

type peerProcID struct {
	pid   int64
	start int64
}

type observedPeer struct {
	info      store.PeerIdentity
	sessionID string    // live Corral term.Session ID when observed (empty when loaded only from DB)
	seenAt    time.Time // zero when loaded from DB and not yet confirmed alive in this gateway run
	exited    bool      // true once a full scan covering this scope confirmed exit
	gen       uint64    // monotonic observation generation for concurrent write ordering
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

// peerRegistry caches observed (owner, host, pid, proc_start) -> Corral agent
// mappings captured passively from Corral's existing locate scans and persists
// them to SQLite so historical cross-session messages remain stable across
// reloads, gateway restarts, and sender exits.
type peerRegistry struct {
	// persistMu serializes SQLite upserts so concurrent writers never commit an
	// older observation over a newer one. Lock order: persistMu -> mu.
	persistMu sync.Mutex

	mu          sync.Mutex
	nextGen     uint64
	known       map[peerHostScope]map[peerProcID]*observedPeer
	persisted   map[peerHostScope]map[peerProcID]persistedPeer
	latestByKey map[peerHostScope]map[string]peerProcID // agentKey -> latest observed live (pid, start)
	titles      map[string]string                       // sessID+"\x00"+pane -> latest convo.Title()
	negCache    map[peerHostScope]map[peerProcID]time.Time

	// Test hooks (nil in production).
	nowFn     func() time.Time
	persistFn func(ctx context.Context, ownerID, hostID int64, rows []store.PeerIdentity) error
	lookupFn  func(ctx context.Context, ownerID, hostID int64, keys []store.PeerProcKey) (map[store.PeerProcKey]store.PeerIdentity, error)
}

func (r *peerRegistry) now() time.Time {
	if r.nowFn != nil {
		return r.nowFn()
	}
	return time.Now()
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

func samePeerProgram(a, b string) bool {
	na, nb := normalizePeerProgram(a), normalizePeerProgram(b)
	return na == "" || nb == "" || na == nb
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

func (r *peerRegistry) noteConvoTitle(sessID, pane, title string) {
	t := sanitizePeerLabel(title, 160)
	if sessID == "" || t == "" {
		return
	}
	r.mu.Lock()
	if r.titles == nil {
		r.titles = map[string]string{}
	}
	if len(r.titles) >= 2048 {
		r.titles = map[string]string{}
	}
	r.titles[sessID+"\x00"+pane] = t
	r.mu.Unlock()
}

func (r *peerRegistry) convoTitleOf(sessID, pane string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.titles == nil {
		return ""
	}
	return r.titles[sessID+"\x00"+pane]
}

// record updates in-memory observed peer state and returns the process IDs that
// need to be persisted to SQLite (either new/changed metadata or throttled
// last_seen refresh). It deliberately does NOT mark rows as persisted before
// SQLite commits.
func (r *peerRegistry) record(scope peerHostScope, items []observedPeer, scan peerScanScope, now time.Time) []peerProcID {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.known == nil {
		r.known = map[peerHostScope]map[peerProcID]*observedPeer{}
	}
	if r.persisted == nil {
		r.persisted = map[peerHostScope]map[peerProcID]persistedPeer{}
	}
	if r.latestByKey == nil {
		r.latestByKey = map[peerHostScope]map[string]peerProcID{}
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
		lm = map[string]peerProcID{}
		r.latestByKey[scope] = lm
	}
	nm := r.negCache[scope]

	nowSec := now.Unix()
	seenItems := make(map[peerProcID]bool, len(items))
	needPersist := make(map[peerProcID]struct{})

	for _, item := range items {
		if item.info.PID <= 0 || item.info.ProcStart <= 0 {
			continue
		}
		id := peerProcID{pid: item.info.PID, start: item.info.ProcStart}
		seenItems[id] = true
		if nm != nil {
			delete(nm, id)
		}
		if prev := km[id]; prev != nil {
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
		item.info.LastSeen = nowSec
		if item.info.UpdatedAt <= 0 {
			item.info.UpdatedAt = nowSec
		}
		r.nextGen++
		item.gen = r.nextGen
		item.seenAt = now
		item.exited = false
		cp := item
		km[id] = &cp
		if item.info.AgentKey != "" {
			lm[item.info.AgentKey] = id
		}
		if prevP, ok := pm[id]; !ok || !prevP.info.SameSnapshot(item.info) || nowSec-prevP.lastSeen >= int64(peerLastSeenRefresh/time.Second) {
			needPersist[id] = struct{}{}
		}
	}

	// Keep previously known tmux peers alive when their (pid, start) is still
	// present in a host-wide /proc scan (even when the scan ran from a herdr
	// session and had no tmuxActive metadata).
	if len(scan.aliveInProcScan) > 0 {
		for id, obs := range km {
			if seenItems[id] || obs.info.Persistence != "tmux" {
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
			if obs.info.AgentKey != "" && lm[obs.info.AgentKey] != (peerProcID{}) && lm[obs.info.AgentKey] != id {
				continue
			}
			obs.seenAt = now
			obs.exited = false
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
			switch obs.info.Persistence {
			case "herdr":
				if !scan.aliveInProcScan[id] || scan.coveredHerdrMux[normalizeHerdrMux(obs.info.MuxSession)] {
					obs.exited = true
				}
			case "tmux":
				if !scan.aliveInProcScan[id] || scan.coveredTmuxKeys[obs.info.AgentKey] {
					obs.exited = true
				}
			default:
				if !scan.aliveInProcScan[id] {
					obs.exited = true
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
			km[id] = &observedPeer{info: row}
		}
		if _, exists := pm[id]; !exists {
			seen := row.LastSeen
			if seen <= 0 {
				seen = row.UpdatedAt
			}
			pm[id] = persistedPeer{info: row, lastSeen: seen}
		}
		if nm := r.negCache[scope]; nm != nil {
			delete(nm, id)
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
			r.negCache = map[peerHostScope]map[peerProcID]time.Time{}
		}
		nm := r.negCache[scope]
		if nm == nil || len(nm) >= peerCacheMaxPerHost {
			nm = map[peerProcID]time.Time{}
			r.negCache[scope] = nm
		}
		nm[id] = now
	}
}

// persistPeerIdentities writes pending peer observations to SQLite under
// persistMu so concurrent callers never overwrite a newer observation with an
// older one, and marks rows persisted ONLY after SQLite commits cleanly.
func (s *Server) persistPeerIdentities(ownerID, hostID int64, ids []peerProcID) {
	if s == nil || len(ids) == 0 || (s.store == nil && s.peers.persistFn == nil) {
		return
	}
	scope := peerHostScope{ownerID: ownerID, hostID: hostID}

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
		err = s.peers.persistFn(ctx, ownerID, hostID, rows)
	} else {
		err = s.store.UpsertPeerIdentities(ctx, ownerID, hostID, rows)
	}
	if err != nil {
		if s.log != nil {
			s.log.Warn("peer identities: persist failed", "owner", ownerID, "host", hostID, "count", len(rows), "err", err)
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
// process selected by pickConvoProc for each Corral-managed pane is mapped to
// that pane; child/background processes sharing HERDR_PANE_ID are ignored.
func (s *Server) recordLocatedProcs(sess *term.Session, l convoLocated, full bool) {
	if s == nil || sess == nil || sess.OwnerID <= 0 || sess.HostID <= 0 || l.noProc {
		return
	}
	scope := peerHostScope{ownerID: sess.OwnerID, hostID: sess.HostID}
	var running []*term.Session
	if s.terms != nil {
		running = s.terms.Running(sess.OwnerID, sess.HostID)
	}
	if len(running) == 0 {
		running = []*term.Session{sess}
	}

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
		info := rs.Info()
		hp, wsLabel, _ := snapIndexes[rs.ID].lookup(pk.pane)
		convoTitle := s.peers.convoTitleOf(rs.ID, pk.pane)
		title := peerTitleFromHerdr(wsLabel, hp.Label, convoTitle, hp.Title, hp.Name, p.cwd, p.agent)
		if wsLabel == "~" {
			wsLabel = ""
		}
		items = append(items, observedPeer{
			sessionID: rs.ID,
			info: store.PeerIdentity{
				OwnerID:      sess.OwnerID,
				HostID:       sess.HostID,
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
				Program:      sanitizePeerLabel(p.agent, 64),
				AgentSession: sanitizePeerLabel(p.sid, 128),
				TerminalID:   sanitizePeerLabel(hp.TerminalID, 128),
			},
		})
	}

	coveredTmuxKeys := map[string]bool{}
	if sess.Persistence == "tmux" && l.tmuxActive != "" && l.tmuxPID != "" {
		info := sess.Info()
		tkey := tmuxAgentKey(sess.HostID, info.MuxSession)
		coveredTmuxKeys[tkey] = true
		if p := pickConvoProc(l.procs, "tmux", l.tmuxActive, "", l.tmuxPID, l.tmuxTTY, ""); p != nil && p.pid > 0 && p.start > 0 {
			convoTitle := s.peers.convoTitleOf(sess.ID, p.tmuxPane)
			title := peerTitleFromTmux(convoTitle, l.tmuxTitle, info.DisplayName, info.MuxSession, p.agent)
			items = append(items, observedPeer{
				sessionID: sess.ID,
				info: store.PeerIdentity{
					OwnerID:      sess.OwnerID,
					HostID:       sess.HostID,
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

	needPersist := s.peers.record(scope, items, peerScanScope{
		full:            full,
		aliveInProcScan: aliveInProcScan,
		coveredHerdrMux: coveredHerdrMux,
		coveredTmuxKeys: coveredTmuxKeys,
	}, s.peers.now())
	if len(needPersist) > 0 {
		s.persistPeerIdentities(sess.OwnerID, sess.HostID, needPersist)
	}
}

// recordBoundProc records the active conversation binding for sess (handles
// both herdr and tmux sessions, and refreshes the remembered Corral title).
func (s *Server) recordBoundProc(sess *term.Session, b *convoBind, convoTitle string) {
	if s == nil || sess == nil || b == nil || b.PID <= 0 || b.Start <= 0 || sess.OwnerID <= 0 || sess.HostID <= 0 {
		return
	}
	if convoTitle != "" {
		s.peers.noteConvoTitle(sess.ID, b.Pane, convoTitle)
	}
	scope := peerHostScope{ownerID: sess.OwnerID, hostID: sess.HostID}
	info := sess.Info()

	var rec store.PeerIdentity
	switch sess.Persistence {
	case "herdr":
		if b.Pane == "" {
			return
		}
		snap, _ := s.herdrSocks.snapshotOf(sess.ID)
		hp, wsLabel, _ := indexHerdrSnapshot(snap).lookup(b.Pane)
		if convoTitle == "" {
			convoTitle = s.peers.convoTitleOf(sess.ID, b.Pane)
		}
		title := peerTitleFromHerdr(wsLabel, hp.Label, convoTitle, hp.Title, hp.Name, hp.Cwd, b.Agent)
		if wsLabel == "~" {
			wsLabel = ""
		}
		termID := b.TerminalID
		if termID == "" {
			termID = hp.TerminalID
		}
		rec = store.PeerIdentity{
			OwnerID:      sess.OwnerID,
			HostID:       sess.HostID,
			PID:          b.PID,
			ProcStart:    b.Start,
			AgentKey:     herdrAgentKey(sess.HostID, info.MuxSession, b.Pane),
			Persistence:  "herdr",
			MuxSession:   normalizeHerdrMux(info.MuxSession),
			DisplayName:  sanitizePeerLabel(info.DisplayName, 120),
			Pane:         b.Pane,
			HostName:     sanitizePeerLabel(info.HostName, 120),
			Workspace:    sanitizePeerLabel(wsLabel, 120),
			Title:        title,
			Program:      sanitizePeerLabel(b.Agent, 64),
			AgentSession: sanitizePeerLabel(b.Session, 128),
			TerminalID:   sanitizePeerLabel(termID, 128),
		}
	case "tmux":
		if convoTitle == "" {
			convoTitle = s.peers.convoTitleOf(sess.ID, b.Pane)
		}
		title := peerTitleFromTmux(convoTitle, b.Title, info.DisplayName, info.MuxSession, b.Agent)
		rec = store.PeerIdentity{
			OwnerID:      sess.OwnerID,
			HostID:       sess.HostID,
			PID:          b.PID,
			ProcStart:    b.Start,
			AgentKey:     tmuxAgentKey(sess.HostID, info.MuxSession),
			Persistence:  "tmux",
			MuxSession:   sanitizePeerLabel(info.MuxSession, 120),
			DisplayName:  sanitizePeerLabel(info.DisplayName, 120),
			Pane:         b.Pane,
			HostName:     sanitizePeerLabel(info.HostName, 120),
			Title:        title,
			Program:      sanitizePeerLabel(b.Agent, 64),
			AgentSession: sanitizePeerLabel(b.Session, 128),
		}
	default:
		return
	}

	needPersist := s.peers.record(scope, []observedPeer{{info: rec, sessionID: sess.ID}}, peerScanScope{}, s.peers.now())
	if len(needPersist) > 0 {
		s.persistPeerIdentities(sess.OwnerID, sess.HostID, needPersist)
	}
}

// peerMatchesHerdrPane verifies that the live herdr pane snapshot still holds
// the same agent identity as the recorded peer (terminal ID, session ID, and
// program). Any mismatch treats the recorded peer as exited rather than
// misattributing old messages to a replacement agent in the same pane.
func peerMatchesHerdrPane(info store.PeerIdentity, hp herdr.Pane, locatedSID string) bool {
	if hp.Agent == "" || hp.AgentStatus == herdr.StatusGone {
		return false
	}
	if !samePeerProgram(info.Program, hp.Agent) {
		return false
	}
	if info.TerminalID != "" && hp.TerminalID != "" && hp.TerminalID != info.TerminalID {
		return false
	}
	snapSID := agentSession(hp)
	if info.AgentSession != "" {
		if snapSID != "" && snapSID != info.AgentSession {
			return false
		}
		if locatedSID != "" && locatedSID != info.AgentSession {
			return false
		}
	}
	return true
}

// resolveConvoPeers builds the peer lookup table for all verified peer
// references present in st.parser (and queued items), combining in-memory
// observations, SQLite history (with negative caching), and strict live
// Corral session/pane identity verification. Never triggers remote calls.
func (s *Server) resolveConvoPeers(ctx context.Context, sess *term.Session, st *convoState) map[string]convo.PeerInfo {
	if s == nil || sess == nil || st == nil || st.parser == nil || sess.OwnerID <= 0 || sess.HostID <= 0 {
		return nil
	}
	refs := convo.PeerRefs(st.parser)
	if len(refs) == 0 {
		return nil
	}
	scope := peerHostScope{ownerID: sess.OwnerID, hostID: sess.HostID}
	now := s.peers.now()

	// Step 1: check in-memory cache and negative cache before querying SQLite.
	var dbKeys []store.PeerProcKey
	s.peers.mu.Lock()
	km := s.peers.known[scope]
	nm := s.peers.negCache[scope]
	for _, ref := range refs {
		id := peerProcID{pid: ref.PID, start: ref.Start}
		if km != nil && km[id] != nil {
			continue
		}
		if nm != nil {
			if missAt, ok := nm[id]; ok && now.Sub(missAt) < peerNegCacheTTL {
				continue
			}
		}
		dbKeys = append(dbKeys, store.PeerProcKey{PID: ref.PID, ProcStart: ref.Start})
	}
	s.peers.mu.Unlock()

	if len(dbKeys) > 0 && (s.store != nil || s.peers.lookupFn != nil) {
		var dbRows map[store.PeerProcKey]store.PeerIdentity
		var err error
		if s.peers.lookupFn != nil {
			dbRows, err = s.peers.lookupFn(ctx, sess.OwnerID, sess.HostID, dbKeys)
		} else {
			dbRows, err = s.store.LookupPeerIdentities(ctx, sess.OwnerID, sess.HostID, dbKeys)
		}
		if err == nil {
			s.peers.recordLoadedFromDB(scope, dbRows, dbKeys, now)
		}
	}

	// Step 2: gather currently running Corral sessions and index each snapshot
	// once (O(N)) to verify whether each matched peer is still alive and holds
	// the exact same pane identity.
	var running []*term.Session
	if s.terms != nil {
		running = s.terms.Running(sess.OwnerID, sess.HostID)
	}
	if len(running) == 0 {
		running = []*term.Session{sess}
	}
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
		if !obs.seenAt.IsZero() && !obs.exited {
			// Alive only if this exact (pid, start) is still the latest
			// occupant for its agentKey AND matches the live session/pane
			// identity (terminal ID, session ID, program).
			replaced := obs.info.AgentKey != "" && lm != nil && lm[obs.info.AgentKey] != (peerProcID{}) && lm[obs.info.AgentKey] != id
			if !replaced {
				switch obs.info.Persistence {
				case "herdr":
					if rs := liveHerdr[normalizeHerdrMux(obs.info.MuxSession)]; rs != nil {
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
							if hp.TerminalID != "" {
								pi.TerminalID = sanitizePeerLabel(hp.TerminalID, 128)
							}
							if t := peerTitleFromHerdr(wsLabel, hp.Label, s.peers.titles[rs.ID+"\x00"+obs.info.Pane], hp.Title, hp.Name, hp.Cwd, pi.Program); t != "" {
								pi.Title = t
							}
						}
					}
				case "tmux":
					if rs := liveTmux[obs.info.MuxSession]; rs != nil {
						info := rs.Info()
						pi.Alive = true
						pi.SessionID = rs.ID
						pi.HostName = sanitizePeerLabel(info.HostName, 120)
						pi.DisplayName = sanitizePeerLabel(info.DisplayName, 120)
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
