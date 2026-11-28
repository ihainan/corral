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
	"github.com/ihainan/corral/internal/corrald/proto"
	"github.com/ihainan/corral/internal/herdr"
	"github.com/ihainan/corral/internal/store"
	"github.com/ihainan/corral/internal/term"
)

const (
	peerCacheMaxPerHost  = 512
	peerMissScanCooldown = 30 * time.Second
	peerFullScanFresh    = 10 * time.Second
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
	exited    bool      // true once a full scan confirmed this process is no longer running
}

// peerRegistry caches observed (owner, host, pid, proc_start) -> Corral agent
// mappings captured passively from Corral's existing locate scans and persists
// them to SQLite so historical cross-session messages remain stable across
// reloads, gateway restarts, and sender exits.
type peerRegistry struct {
	mu          sync.Mutex
	known       map[peerHostScope]map[peerProcID]*observedPeer
	persisted   map[peerHostScope]map[peerProcID]store.PeerIdentity
	latestByKey map[peerHostScope]map[string]peerProcID // agentKey -> latest observed live (pid, start)
	titles      map[string]string                       // sessID+"\x00"+pane -> latest convo.Title()
	fullScanAt  map[peerHostScope]time.Time
	scanRunning map[peerHostScope]bool
	lastBgScan  map[peerHostScope]time.Time
	missChecked map[peerHostScope]map[peerProcID]time.Time
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

// recordLocked records observed live processes for scope and returns rows that
// need to be persisted to SQLite (new or changed snapshots).
func (r *peerRegistry) record(scope peerHostScope, items []observedPeer, full bool, now time.Time) []store.PeerIdentity {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.known == nil {
		r.known = map[peerHostScope]map[peerProcID]*observedPeer{}
		r.persisted = map[peerHostScope]map[peerProcID]store.PeerIdentity{}
		r.latestByKey = map[peerHostScope]map[string]peerProcID{}
		r.fullScanAt = map[peerHostScope]time.Time{}
	}
	km := r.known[scope]
	if km == nil {
		km = map[peerProcID]*observedPeer{}
		r.known[scope] = km
	}
	pm := r.persisted[scope]
	if pm == nil {
		pm = map[peerProcID]store.PeerIdentity{}
		r.persisted[scope] = pm
	}
	lm := r.latestByKey[scope]
	if lm == nil {
		lm = map[string]peerProcID{}
		r.latestByKey[scope] = lm
	}

	seenInFull := map[peerProcID]bool{}
	var dirty []store.PeerIdentity

	for _, item := range items {
		if item.info.PID <= 0 || item.info.ProcStart <= 0 {
			continue
		}
		id := peerProcID{pid: item.info.PID, start: item.info.ProcStart}
		if full {
			seenInFull[id] = true
		}
		// Preserve prior non-empty title if the new observation lacked one.
		if item.info.Title == "" {
			if prev := km[id]; prev != nil && prev.info.Title != "" {
				item.info.Title = prev.info.Title
			}
		}
		item.seenAt = now
		item.exited = false
		cp := item
		km[id] = &cp
		if item.info.AgentKey != "" {
			lm[item.info.AgentKey] = id
		}
		if prev, ok := pm[id]; !ok || !prev.SameSnapshot(item.info) {
			pm[id] = item.info
			dirty = append(dirty, item.info)
		}
	}

	if full {
		r.fullScanAt[scope] = now
		for id, obs := range km {
			if !seenInFull[id] {
				obs.exited = true
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
	return dirty
}

func (r *peerRegistry) recordLoadedFromDB(scope peerHostScope, rows map[store.PeerProcKey]store.PeerIdentity) {
	if len(rows) == 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.known == nil {
		r.known = map[peerHostScope]map[peerProcID]*observedPeer{}
		r.persisted = map[peerHostScope]map[peerProcID]store.PeerIdentity{}
	}
	km := r.known[scope]
	if km == nil {
		km = map[peerProcID]*observedPeer{}
		r.known[scope] = km
	}
	pm := r.persisted[scope]
	if pm == nil {
		pm = map[peerProcID]store.PeerIdentity{}
		r.persisted[scope] = pm
	}
	for k, row := range rows {
		id := peerProcID{pid: k.PID, start: k.ProcStart}
		if _, exists := km[id]; !exists {
			km[id] = &observedPeer{info: row}
		}
		if _, exists := pm[id]; !exists {
			pm[id] = row
		}
	}
}

func (s *Server) persistPeerIdentities(ownerID, hostID int64, dirty []store.PeerIdentity) {
	if s == nil || s.store == nil || len(dirty) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = s.store.UpsertPeerIdentities(ctx, ownerID, hostID, dirty)
}

func herdrPaneMetaFromSnapshot(snap *herdr.Snapshot, paneID string) (pane herdr.Pane, wsLabel string, found bool) {
	if snap == nil || paneID == "" {
		return herdr.Pane{}, "", false
	}
	wsLabels := make(map[string]string, len(snap.Workspaces))
	for _, w := range snap.Workspaces {
		wsLabels[w.ID] = w.Label
	}
	for _, list := range [][]herdr.Pane{snap.Agents, snap.Panes} {
		for _, p := range list {
			if p.ID == paneID {
				return p, wsLabels[p.WorkspaceID], true
			}
		}
	}
	return herdr.Pane{}, "", false
}

// recordLocatedProcs updates the server's peer identity registry from a
// locate scan's results (zero extra remote calls). full is true when procs came
// from a host-wide scan rather than a pane-scoped fast locate.
func (s *Server) recordLocatedProcs(sess *term.Session, procs []convoProc, full bool) {
	if s == nil || sess == nil || sess.OwnerID <= 0 || sess.HostID <= 0 {
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
	snapsBySess := map[string]*herdr.Snapshot{}
	for _, rs := range running {
		if rs.Persistence == "herdr" {
			if sock := s.herdrSocks.get(rs.ID); sock != "" {
				herdrBySock[sock] = rs
			}
			if snap, ok := s.herdrSocks.snapshotOf(rs.ID); ok {
				snapsBySess[rs.ID] = snap
			}
		}
	}

	var items []observedPeer
	for _, p := range procs {
		if p.pid <= 0 || p.start <= 0 {
			continue
		}
		if p.herdrPane != "" && p.herdrSocket != "" {
			rs := herdrBySock[p.herdrSocket]
			if rs == nil {
				continue // not in a Corral-managed herdr session
			}
			info := rs.Info()
			hp, wsLabel, _ := herdrPaneMetaFromSnapshot(snapsBySess[rs.ID], p.herdrPane)
			convoTitle := s.peers.convoTitleOf(rs.ID, p.herdrPane)
			title := peerTitleFromHerdr(wsLabel, hp.Label, convoTitle, hp.Title, hp.Name, p.cwd, p.agent)
			if wsLabel == "~" {
				wsLabel = ""
			}
			items = append(items, observedPeer{
				sessionID: rs.ID,
				info: store.PeerIdentity{
					OwnerID:     sess.OwnerID,
					HostID:      sess.HostID,
					PID:         p.pid,
					ProcStart:   p.start,
					AgentKey:    herdrAgentKey(rs.HostID, info.MuxSession, p.herdrPane),
					Persistence: "herdr",
					MuxSession:  normalizeHerdrMux(info.MuxSession),
					DisplayName: sanitizePeerLabel(info.DisplayName, 120),
					Pane:        p.herdrPane,
					HostName:    sanitizePeerLabel(info.HostName, 120),
					Workspace:   sanitizePeerLabel(wsLabel, 120),
					Title:       title,
					Program:     sanitizePeerLabel(p.agent, 64),
				},
			})
		}
	}

	dirty := s.peers.record(scope, items, full, time.Now())
	if len(dirty) > 0 {
		s.persistPeerIdentities(sess.OwnerID, sess.HostID, dirty)
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
		hp, wsLabel, _ := herdrPaneMetaFromSnapshot(snap, b.Pane)
		if convoTitle == "" {
			convoTitle = s.peers.convoTitleOf(sess.ID, b.Pane)
		}
		title := peerTitleFromHerdr(wsLabel, hp.Label, convoTitle, hp.Title, hp.Name, hp.Cwd, b.Agent)
		if wsLabel == "~" {
			wsLabel = ""
		}
		rec = store.PeerIdentity{
			OwnerID:     sess.OwnerID,
			HostID:      sess.HostID,
			PID:         b.PID,
			ProcStart:   b.Start,
			AgentKey:    herdrAgentKey(sess.HostID, info.MuxSession, b.Pane),
			Persistence: "herdr",
			MuxSession:  normalizeHerdrMux(info.MuxSession),
			DisplayName: sanitizePeerLabel(info.DisplayName, 120),
			Pane:        b.Pane,
			HostName:    sanitizePeerLabel(info.HostName, 120),
			Workspace:   sanitizePeerLabel(wsLabel, 120),
			Title:       title,
			Program:     sanitizePeerLabel(b.Agent, 64),
		}
	case "tmux":
		if convoTitle == "" {
			convoTitle = s.peers.convoTitleOf(sess.ID, b.Pane)
		}
		title := peerTitleFromTmux(convoTitle, b.Title, info.DisplayName, info.MuxSession, b.Agent)
		rec = store.PeerIdentity{
			OwnerID:     sess.OwnerID,
			HostID:      sess.HostID,
			PID:         b.PID,
			ProcStart:   b.Start,
			AgentKey:    tmuxAgentKey(sess.HostID, info.MuxSession),
			Persistence: "tmux",
			MuxSession:  sanitizePeerLabel(info.MuxSession, 120),
			DisplayName: sanitizePeerLabel(info.DisplayName, 120),
			Pane:        b.Pane,
			HostName:    sanitizePeerLabel(info.HostName, 120),
			Title:       title,
			Program:     sanitizePeerLabel(b.Agent, 64),
		}
	default:
		return
	}

	dirty := s.peers.record(scope, []observedPeer{{info: rec, sessionID: sess.ID}}, false, time.Now())
	if len(dirty) > 0 {
		s.persistPeerIdentities(sess.OwnerID, sess.HostID, dirty)
	}
}

// shouldTriggerBgScan decides whether an unresolved (or post-restart unconfirmed)
// peer reference warrants a single background host scan via the existing
// runConvoScan path. Bounded by per-host cooldown and per-ref check memory.
func (r *peerRegistry) shouldTriggerBgScan(scope peerHostScope, refs []peerProcID, now time.Time) bool {
	if len(refs) == 0 {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.scanRunning[scope] {
		return false
	}
	if !r.fullScanAt[scope].IsZero() && now.Sub(r.fullScanAt[scope]) < peerFullScanFresh {
		return false
	}
	if !r.lastBgScan[scope].IsZero() && now.Sub(r.lastBgScan[scope]) < peerMissScanCooldown {
		return false
	}
	if r.missChecked == nil {
		r.missChecked = map[peerHostScope]map[peerProcID]time.Time{}
	}
	cm := r.missChecked[scope]
	if cm == nil {
		cm = map[peerProcID]time.Time{}
		r.missChecked[scope] = cm
	}
	need := false
	for _, ref := range refs {
		if checkedAt, ok := cm[ref]; !ok || checkedAt.IsZero() {
			need = true
			break
		}
	}
	if !need {
		return false
	}
	if len(cm) > peerCacheMaxPerHost {
		cm = map[peerProcID]time.Time{}
		r.missChecked[scope] = cm
	}
	for _, ref := range refs {
		cm[ref] = now
	}
	if r.scanRunning == nil {
		r.scanRunning = map[peerHostScope]bool{}
		r.lastBgScan = map[peerHostScope]time.Time{}
	}
	r.scanRunning[scope] = true
	r.lastBgScan[scope] = now
	return true
}

func (r *peerRegistry) finishBgScan(scope peerHostScope) {
	r.mu.Lock()
	delete(r.scanRunning, scope)
	r.mu.Unlock()
}

// resolveConvoPeers builds the peer lookup table for all verified peer
// references present in st.parser (and queued items), combining in-memory
// observations, SQLite history, and live Corral session/pane liveness.
func (s *Server) resolveConvoPeers(ctx context.Context, sess *term.Session, st *convoState) map[string]convo.PeerInfo {
	if s == nil || sess == nil || st == nil || st.parser == nil || sess.OwnerID <= 0 || sess.HostID <= 0 {
		return nil
	}
	refs := convo.PeerRefs(st.parser)
	if len(refs) == 0 {
		return nil
	}
	scope := peerHostScope{ownerID: sess.OwnerID, hostID: sess.HostID}

	// Step 1: check in-memory cache and collect missing keys for SQLite lookup.
	var dbKeys []store.PeerProcKey
	s.peers.mu.Lock()
	km := s.peers.known[scope]
	for _, ref := range refs {
		id := peerProcID{pid: ref.PID, start: ref.Start}
		if km == nil || km[id] == nil {
			dbKeys = append(dbKeys, store.PeerProcKey{PID: ref.PID, ProcStart: ref.Start})
		}
	}
	s.peers.mu.Unlock()

	if len(dbKeys) > 0 && s.store != nil {
		if dbRows, err := s.store.LookupPeerIdentities(ctx, sess.OwnerID, sess.HostID, dbKeys); err == nil && len(dbRows) > 0 {
			s.peers.recordLoadedFromDB(scope, dbRows)
		}
	}

	// Step 2: gather currently running Corral sessions on this host to verify
	// whether each matched peer is still alive and reachable right now.
	var running []*term.Session
	if s.terms != nil {
		running = s.terms.Running(sess.OwnerID, sess.HostID)
	}
	if len(running) == 0 {
		running = []*term.Session{sess}
	}
	liveHerdr := map[string]*term.Session{} // normalized mux -> session
	liveTmux := map[string]*term.Session{}  // mux -> session
	snaps := map[string]*herdr.Snapshot{}
	for _, rs := range running {
		info := rs.Info()
		switch rs.Persistence {
		case "herdr":
			liveHerdr[normalizeHerdrMux(info.MuxSession)] = rs
			if snap, ok := s.herdrSocks.snapshotOf(rs.ID); ok {
				snaps[rs.ID] = snap
			}
		case "tmux":
			liveTmux[info.MuxSession] = rs
		}
	}

	out := map[string]convo.PeerInfo{}
	var needScan []peerProcID

	s.peers.mu.Lock()
	km = s.peers.known[scope]
	lm := s.peers.latestByKey[scope]
	for _, ref := range refs {
		id := peerProcID{pid: ref.PID, start: ref.Start}
		obs := km[id]
		if obs == nil {
			needScan = append(needScan, id)
			continue
		}
		pi := convo.PeerInfo{
			Key:         obs.info.AgentKey,
			HostID:      obs.info.HostID,
			HostName:    obs.info.HostName,
			Persistence: obs.info.Persistence,
			MuxSession:  obs.info.MuxSession,
			DisplayName: obs.info.DisplayName,
			Pane:        obs.info.Pane,
			Workspace:   obs.info.Workspace,
			Title:       obs.info.Title,
			Program:     obs.info.Program,
		}
		if obs.seenAt.IsZero() {
			// Loaded from DB after restart; confirm liveness in the background once.
			needScan = append(needScan, id)
		} else if !obs.exited {
			// Still alive only if this exact (pid, start) has not been replaced
			// in its pane/key and the Corral session/pane still exists.
			replaced := obs.info.AgentKey != "" && lm != nil && lm[obs.info.AgentKey] != (peerProcID{}) && lm[obs.info.AgentKey] != id
			if !replaced {
				switch obs.info.Persistence {
				case "herdr":
					if rs := liveHerdr[normalizeHerdrMux(obs.info.MuxSession)]; rs != nil {
						snap := snaps[rs.ID]
						hp, wsLabel, paneExists := herdrPaneMetaFromSnapshot(snap, obs.info.Pane)
						if snap == nil || (paneExists && hp.Agent != "") {
							info := rs.Info()
							pi.Alive = true
							pi.SessionID = rs.ID
							pi.HostName = sanitizePeerLabel(info.HostName, 120)
							pi.DisplayName = sanitizePeerLabel(info.DisplayName, 120)
							if paneExists {
								if wsLabel != "" && wsLabel != "~" {
									pi.Workspace = sanitizePeerLabel(wsLabel, 120)
								}
								if t := peerTitleFromHerdr(wsLabel, hp.Label, s.peers.titles[rs.ID+"\x00"+obs.info.Pane], hp.Title, hp.Name, hp.Cwd, pi.Program); t != "" {
									pi.Title = t
								}
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

	// If any peer was not yet observed on this host (e.g. a newly started pane
	// while locateScan used single-pane fast locate) or was loaded from DB
	// after a gateway restart, reuse the existing host locate scan once in the
	// background (non-blocking, zero new SSH commands).
	if len(needScan) > 0 && sess.Client() != nil && s.peers.shouldTriggerBgScan(scope, needScan, time.Now()) {
		go s.refreshHostPeersBg(sess, scope)
	}

	if len(out) == 0 {
		return nil
	}
	return out
}

func (s *Server) refreshHostPeersBg(sess *term.Session, scope peerHostScope) {
	defer s.peers.finishBgScan(scope)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	tmuxName := sess.CurrentTmuxSession()
	params := proto.AgentLocateParams{Mode: sess.Persistence, TmuxName: tmuxName}
	cmd := convoLocateCommand(sess.Persistence, tmuxName, "")
	out, _, _, _, err := s.runConvoScan(ctx, sess, params, cmd, time.Time{})
	if err != nil {
		return
	}
	l := parseConvoLocate(string(out))
	if !l.noProc {
		s.recordLocatedProcs(sess, l.procs, true)
	}
}
