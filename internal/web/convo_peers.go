package web

import (
	"path"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/ihainan/corral/internal/convo"
	"github.com/ihainan/corral/internal/herdr"
	"github.com/ihainan/corral/internal/term"
)

const (
	// peerSlotMaxAge bounds how long a pane/session slot from a prior locate
	// scan may be used for peer matching without being refreshed by a newer
	// scan. Expired slots fall back to "not in Corral" so missed scan paths
	// never misattribute messages.
	peerSlotMaxAge = 10 * time.Minute
)

type peerProcID struct {
	pid   int64
	start int64
}

type livePeerEntry struct {
	info     convo.PeerInfo
	program  string
	cwd      string
	termID   string
	agentSID string
}

type livePaneSlot struct {
	scanAt time.Time
	has    bool
	id     peerProcID
	entry  livePeerEntry
}

type liveSessionPeers struct {
	ownerID    int64
	hostID     int64
	hostConfig string
	fullScanAt time.Time                // latest full-scan start time covering all panes (herdr) or session (tmux)
	panes      map[string]*livePaneSlot // herdr: paneID -> latest picked process; tmux: "" -> latest picked process
}

// peerLocateCache holds ONLY the latest locate scan's mapping of
// currently-selected (pid, proc_start) -> Corral agent per live Corral session.
// Full scans replace all panes of covered herdr sessions (or the scanned tmux
// session); partial/fast scans update ONLY the exact pane they covered without
// touching or inferring any other pane or session.
type peerLocateCache struct {
	mu     sync.Mutex
	bySess map[string]*liveSessionPeers
	nowFn  func() time.Time // test hook; nil uses time.Now
}

func (c *peerLocateCache) now() time.Time {
	if c != nil && c.nowFn != nil {
		return c.nowFn()
	}
	return time.Now()
}

func (c *peerLocateCache) ensureSessLocked(sessID string, ownerID, hostID int64, hostConfig string) *liveSessionPeers {
	if c.bySess == nil {
		c.bySess = map[string]*liveSessionPeers{}
	}
	cur := c.bySess[sessID]
	if cur == nil || cur.ownerID != ownerID || cur.hostID != hostID || cur.hostConfig != hostConfig {
		cur = &liveSessionPeers{
			ownerID:    ownerID,
			hostID:     hostID,
			hostConfig: hostConfig,
			panes:      map[string]*livePaneSlot{},
		}
		c.bySess[sessID] = cur
	}
	return cur
}

func (c *peerLocateCache) replaceHerdrFull(sessID string, ownerID, hostID int64, hostConfig string, scanAt time.Time, picked map[string]livePaneSlot) {
	if sessID == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	cur := c.ensureSessLocked(sessID, ownerID, hostID, hostConfig)
	if scanAt.Before(cur.fullScanAt) {
		return
	}
	cur.fullScanAt = scanAt
	for pane, slot := range cur.panes {
		if scanAt.Before(slot.scanAt) {
			continue
		}
		if next, ok := picked[pane]; ok {
			cp := next
			cp.scanAt = scanAt
			cur.panes[pane] = &cp
		} else {
			delete(cur.panes, pane)
		}
	}
	for pane, next := range picked {
		if _, exists := cur.panes[pane]; exists {
			continue
		}
		cp := next
		cp.scanAt = scanAt
		cur.panes[pane] = &cp
	}
}

func (c *peerLocateCache) replaceHerdrPane(sessID string, ownerID, hostID int64, hostConfig, pane string, scanAt time.Time, picked *livePaneSlot) {
	if sessID == "" || pane == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	cur := c.ensureSessLocked(sessID, ownerID, hostID, hostConfig)
	if scanAt.Before(cur.fullScanAt) {
		return
	}
	if prev := cur.panes[pane]; prev != nil && scanAt.Before(prev.scanAt) {
		return
	}
	if picked != nil && picked.has {
		cp := *picked
		cp.scanAt = scanAt
		cur.panes[pane] = &cp
	} else {
		cur.panes[pane] = &livePaneSlot{scanAt: scanAt, has: false}
	}
}

func (c *peerLocateCache) replaceTmux(sessID string, ownerID, hostID int64, hostConfig string, scanAt time.Time, picked *livePaneSlot) {
	if sessID == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	cur := c.ensureSessLocked(sessID, ownerID, hostID, hostConfig)
	if scanAt.Before(cur.fullScanAt) {
		return
	}
	cur.fullScanAt = scanAt
	cur.panes = map[string]*livePaneSlot{}
	if picked != nil && picked.has {
		cp := *picked
		cp.scanAt = scanAt
		cur.panes[""] = &cp
	}
}

func (c *peerLocateCache) dropClosed(ownerID, hostID int64, hostConfig string, aliveSess map[string]bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for sid, cur := range c.bySess {
		if cur.ownerID == ownerID && cur.hostID == hostID && cur.hostConfig == hostConfig && !aliveSess[sid] {
			delete(c.bySess, sid)
		}
	}
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

func (s *Server) runningInPeerScope(sess *term.Session, ownerID, hostID int64, hostConfig string) []*term.Session {
	var filtered []*term.Session
	if s.terms != nil {
		for _, rs := range s.terms.Running(ownerID, hostID) {
			if peerHostConfig(rs) == hostConfig {
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
	return na != "" && nb != "" && na == nb
}

// peerTitleFromHerdr mirrors the sidebar's sidebarHerdrTitle rule in
// web/src/terminal/peerSender.ts (used by AgentsChat.tsx's labelOf):
//  1. A custom pane label always wins.
//  2. A same-workspace lineage child without a custom label shows its own name
//     (pane title, agent name, program, or dir) instead of its parent's workspace.
//  3. A plain workspace agent keeps the workspace name (except "~").
//  4. Otherwise falls back to pane title, agent name, dir, or program.
func peerTitleFromHerdr(wsLabel, paneLabel, paneTitle, agentName, cwd, agent string, sameWsParent bool) string {
	if t := sanitizePeerLabel(paneLabel, 120); t != "" {
		return t
	}
	if sameWsParent {
		if t := cleanPeerPaneTitle(paneTitle); t != "" {
			return t
		}
		if t := sanitizePeerLabel(agentName, 120); t != "" {
			return t
		}
		if t := sanitizePeerLabel(agent, 64); t != "" {
			return t
		}
		return peerDirName(cwd)
	}
	if ws := sanitizePeerLabel(wsLabel, 120); ws != "" && ws != "~" {
		return ws
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

func peerTitleFromTmux(paneTitle, displayName, muxSession, agent string) string {
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

// mergeHerdrPane overlays non-empty agent snapshot fields onto the pane entry
// so custom pane names (Label) or metadata present only in Snapshot.Panes are
// preserved when merged with Snapshot.Agents.
func mergeHerdrPane(base, over herdr.Pane) herdr.Pane {
	out := base
	if over.ID != "" {
		out.ID = over.ID
	}
	if over.TabID != "" {
		out.TabID = over.TabID
	}
	if over.WorkspaceID != "" {
		out.WorkspaceID = over.WorkspaceID
	}
	if over.AgentStatus != "" {
		out.AgentStatus = over.AgentStatus
	}
	if over.Agent != "" {
		out.Agent = over.Agent
	}
	if over.Name != "" {
		out.Name = over.Name
	}
	if over.Cwd != "" {
		out.Cwd = over.Cwd
	}
	if over.Focused {
		out.Focused = true
	}
	if over.Label != "" {
		out.Label = over.Label
	}
	if over.Title != "" {
		out.Title = over.Title
	}
	if over.AgentSession != nil {
		out.AgentSession = over.AgentSession
	}
	if len(over.Tokens) > 0 {
		out.Tokens = over.Tokens
	}
	if over.TerminalID != "" {
		out.TerminalID = over.TerminalID
	}
	if over.StateSeq != 0 {
		out.StateSeq = over.StateSeq
	}
	return out
}

type herdrSnapIndex struct {
	wsLabels   map[string]string
	panes      map[string]herdr.Pane
	agentPanes map[string]bool
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
	agentPanes := make(map[string]bool, len(snap.Agents))
	for _, p := range snap.Panes {
		if p.ID != "" {
			panes[p.ID] = p
			if p.Agent != "" && p.AgentStatus != herdr.StatusGone {
				agentPanes[p.ID] = true
			}
		}
	}
	for _, a := range snap.Agents {
		if a.ID == "" {
			continue
		}
		if prev, ok := panes[a.ID]; ok {
			panes[a.ID] = mergeHerdrPane(prev, a)
		} else {
			panes[a.ID] = a
		}
		if a.AgentStatus == herdr.StatusGone {
			delete(agentPanes, a.ID)
		} else {
			agentPanes[a.ID] = true
		}
	}
	return &herdrSnapIndex{wsLabels: wsLabels, panes: panes, agentPanes: agentPanes}
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

func (s *Server) buildHerdrPaneSlot(rs *term.Session, sock, pane string, procs []convoProc, snapIdx *herdrSnapIndex, sameWsParents map[string]bool) *livePaneSlot {
	p := pickConvoProc(procs, "herdr", pane, sock, "", "", s.herdrSocks.sid(rs.ID, pane))
	if p == nil || p.pid <= 0 || p.start <= 0 {
		return nil
	}
	hp, wsLabel, paneExists := snapIdx.lookup(pane)
	if !paneExists || hp.AgentStatus == herdr.StatusGone {
		return nil
	}
	if hp.Agent != "" && !samePeerProgram(p.agent, hp.Agent) {
		return nil
	}
	if p.sid != "" && agentSession(hp) != "" && agentSession(hp) != p.sid {
		return nil
	}
	title := peerTitleFromHerdr(wsLabel, hp.Label, hp.Title, hp.Name, p.cwd, p.agent, sameWsParents[pane])
	if title == "" {
		return nil
	}
	if wsLabel == "~" {
		wsLabel = ""
	}
	info := rs.Info()
	return &livePaneSlot{
		has: true,
		id:  peerProcID{pid: p.pid, start: p.start},
		entry: livePeerEntry{
			info: convo.PeerInfo{
				SessionID:   rs.ID,
				Pane:        pane,
				Title:       title,
				HostName:    sanitizePeerLabel(info.HostName, 120),
				Persistence: "herdr",
				MuxSession:  normalizeHerdrMux(info.MuxSession),
				DisplayName: sanitizePeerLabel(info.DisplayName, 120),
				Workspace:   sanitizePeerLabel(wsLabel, 120),
			},
			program:  p.agent,
			cwd:      p.cwd,
			termID:   hp.TerminalID,
			agentSID: p.sid,
		},
	}
}

// recordLocatedProcs updates the server's in-memory live peer map from an
// existing locate scan's results (zero extra remote calls).
//   - When full == true: replaces all panes of each covered herdr session on
//     this host connection (and, if sess is tmux, replaces sess's tmux slot).
//   - When full == false: updates ONLY coveredPane on sess itself (e.g. a
//     single-pane fast locate), leaving all other panes and sessions untouched.
//
// A tmux session's slot is ONLY updated by scans of that tmux session itself.
func (s *Server) recordLocatedProcs(sess *term.Session, l convoLocated, full bool, coveredPane string, observedAt time.Time) {
	if s == nil || sess == nil || sess.OwnerID <= 0 || sess.HostID <= 0 || l.noProc {
		return
	}
	if observedAt.IsZero() {
		observedAt = s.peers.now()
	}
	hc := peerHostConfig(sess)

	// Non-full scan (e.g. fast single-pane PID locate or OpenCode-only scan):
	// update ONLY the exact pane covered on sess; never touch other panes.
	if !full {
		if sess.Persistence != "herdr" || coveredPane == "" {
			return
		}
		sock := s.herdrSocks.get(sess.ID)
		if sock == "" {
			return
		}
		snap, ok := s.herdrSocks.snapshotOf(sess.ID)
		if !ok {
			return
		}
		snapIdx := indexHerdrSnapshot(snap)
		sameWs := s.herdrSocks.lineageOf(sess.ID).sameWorkspaceParents(snapIdx)
		slot := s.buildHerdrPaneSlot(sess, sock, coveredPane, l.procs, snapIdx, sameWs)
		s.peers.replaceHerdrPane(sess.ID, sess.OwnerID, sess.HostID, hc, coveredPane, observedAt, slot)
		return
	}

	running := s.runningInPeerScope(sess, sess.OwnerID, sess.HostID, hc)

	herdrBySock := map[string]*term.Session{}
	snapIndexes := map[string]*herdrSnapIndex{}
	aliveSess := make(map[string]bool, len(running))
	for _, rs := range running {
		aliveSess[rs.ID] = true
		if rs.Persistence == "herdr" {
			if sock := s.herdrSocks.get(rs.ID); sock != "" {
				herdrBySock[sock] = rs
			}
			if snap, ok := s.herdrSocks.snapshotOf(rs.ID); ok {
				snapIndexes[rs.ID] = indexHerdrSnapshot(snap)
			}
		}
	}

	panesBySock := map[string]map[string]struct{}{}
	for _, p := range l.procs {
		if p.pid <= 0 || p.start <= 0 {
			continue
		}
		if p.herdrPane != "" && p.herdrSocket != "" && herdrBySock[p.herdrSocket] != nil {
			pm := panesBySock[p.herdrSocket]
			if pm == nil {
				pm = map[string]struct{}{}
				panesBySock[p.herdrSocket] = pm
			}
			pm[p.herdrPane] = struct{}{}
		}
	}

	for sock, rs := range herdrBySock {
		picked := map[string]livePaneSlot{}
		snapIdx := snapIndexes[rs.ID]
		sameWs := s.herdrSocks.lineageOf(rs.ID).sameWorkspaceParents(snapIdx)
		for pane := range panesBySock[sock] {
			if slot := s.buildHerdrPaneSlot(rs, sock, pane, l.procs, snapIdx, sameWs); slot != nil && slot.has {
				picked[pane] = *slot
			}
		}
		s.peers.replaceHerdrFull(rs.ID, rs.OwnerID, rs.HostID, hc, observedAt, picked)
	}

	// Tmux mapping is replaced strictly by scans of that tmux session itself.
	if sess.Persistence == "tmux" {
		var slot *livePaneSlot
		if l.tmuxActive != "" && l.tmuxPID != "" {
			if p := pickConvoProc(l.procs, "tmux", l.tmuxActive, "", l.tmuxPID, l.tmuxTTY, ""); p != nil && p.pid > 0 && p.start > 0 {
				info := sess.Info()
				title := peerTitleFromTmux(l.tmuxTitle, info.DisplayName, info.MuxSession, p.agent)
				if title != "" {
					slot = &livePaneSlot{
						has: true,
						id:  peerProcID{pid: p.pid, start: p.start},
						entry: livePeerEntry{
							info: convo.PeerInfo{
								SessionID:   sess.ID,
								Title:       title,
								HostName:    sanitizePeerLabel(info.HostName, 120),
								Persistence: "tmux",
								MuxSession:  sanitizePeerLabel(info.MuxSession, 120),
								DisplayName: sanitizePeerLabel(info.DisplayName, 120),
							},
							program:  p.agent,
							agentSID: p.sid,
						},
					}
				}
			}
		}
		s.peers.replaceTmux(sess.ID, sess.OwnerID, sess.HostID, hc, observedAt, slot)
	}

	if s.terms != nil {
		s.peers.dropClosed(sess.OwnerID, sess.HostID, hc, aliveSess)
	}
}

// resolveConvoPeers matches each message's own verified (pid, proc_start)
// against the latest locate scan's currently-selected Corral agent processes
// on the same host connection. Everything else is omitted (shown as "not in
// Corral" on the frontend). Never triggers remote calls or database queries.
func (s *Server) resolveConvoPeers(sess *term.Session, st *convoState) map[string]convo.PeerInfo {
	if s == nil || sess == nil || st == nil || st.parser == nil || sess.OwnerID <= 0 || sess.HostID <= 0 {
		return nil
	}
	refs := convo.PeerRefs(st.parser)
	if len(refs) == 0 {
		return nil
	}
	hc := peerHostConfig(sess)
	running := s.runningInPeerScope(sess, sess.OwnerID, sess.HostID, hc)

	snapIndexes := map[string]*herdrSnapIndex{}
	sameWsBySess := map[string]map[string]bool{}
	for _, rs := range running {
		if rs.Persistence == "herdr" {
			if snap, ok := s.herdrSocks.snapshotOf(rs.ID); ok {
				idx := indexHerdrSnapshot(snap)
				snapIndexes[rs.ID] = idx
				sameWsBySess[rs.ID] = s.herdrSocks.lineageOf(rs.ID).sameWorkspaceParents(idx)
			}
		}
	}

	now := s.peers.now()
	out := map[string]convo.PeerInfo{}
	s.peers.mu.Lock()
	for _, ref := range refs {
		id := peerProcID{pid: ref.PID, start: ref.Start}
		matched := false
		for _, rs := range running {
			cur := s.peers.bySess[rs.ID]
			if cur == nil || cur.ownerID != sess.OwnerID || cur.hostID != sess.HostID || cur.hostConfig != hc {
				continue
			}
			for _, slot := range cur.panes {
				if slot == nil || !slot.has || slot.id != id {
					continue
				}
				if slot.scanAt.IsZero() || now.Sub(slot.scanAt) > peerSlotMaxAge {
					continue
				}
				entry := slot.entry
				pi := entry.info
				if rs.Persistence == "herdr" {
					hp, wsLabel, paneExists := snapIndexes[rs.ID].lookup(pi.Pane)
					if !paneExists || hp.AgentStatus == herdr.StatusGone {
						continue
					}
					if hp.Agent != "" && !samePeerProgram(entry.program, hp.Agent) {
						continue
					}
					if entry.termID != "" && hp.TerminalID != entry.termID {
						continue
					}
					if entry.agentSID != "" && agentSession(hp) != "" && agentSession(hp) != entry.agentSID {
						continue
					}
					if locatedSID, locatedAt := s.herdrSocks.locatedAtOf(rs.ID, pi.Pane); entry.agentSID != "" && locatedSID != "" && !locatedAt.Before(slot.scanAt) && locatedSID != entry.agentSID {
						continue
					}
					if t := peerTitleFromHerdr(wsLabel, hp.Label, hp.Title, hp.Name, entry.cwd, entry.program, sameWsBySess[rs.ID][pi.Pane]); t != "" {
						pi.Title = t
					}
					if wsLabel != "" && wsLabel != "~" {
						pi.Workspace = sanitizePeerLabel(wsLabel, 120)
					} else {
						pi.Workspace = ""
					}
				}
				info := rs.Info()
				pi.HostName = sanitizePeerLabel(info.HostName, 120)
				pi.DisplayName = sanitizePeerLabel(info.DisplayName, 120)
				out[peerMapKey(ref.PID, ref.Start)] = pi
				matched = true
				break
			}
			if matched {
				break
			}
		}
	}
	s.peers.mu.Unlock()

	if len(out) == 0 {
		return nil
	}
	return out
}
