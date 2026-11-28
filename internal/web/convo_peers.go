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

type peerProcID struct {
	pid   int64
	start int64
}

type livePeerEntry struct {
	info     convo.PeerInfo
	program  string
	termID   string
	agentSID string
}

type liveLocatePeers struct {
	ownerID    int64
	hostID     int64
	hostConfig string
	scanAt     time.Time
	byProc     map[peerProcID]livePeerEntry
}

// peerLocateCache holds ONLY the latest full locate scan's mapping of
// currently-selected (pid, proc_start) -> Corral agent per live Corral session.
// Every full locate replaces the session's map wholesale (no persistence, no
// cross-scan merging, no negative cache, no exited history).
type peerLocateCache struct {
	mu     sync.Mutex
	bySess map[string]*liveLocatePeers
}

func (c *peerLocateCache) replace(sessID string, next *liveLocatePeers) {
	if sessID == "" || next == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.bySess == nil {
		c.bySess = map[string]*liveLocatePeers{}
	}
	if cur := c.bySess[sessID]; cur != nil && next.scanAt.Before(cur.scanAt) {
		return
	}
	c.bySess[sessID] = next
}

func (c *peerLocateCache) pruneTmuxAbsent(sessID string, scanAt time.Time, aliveInProc map[peerProcID]bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	cur := c.bySess[sessID]
	if cur == nil || scanAt.Before(cur.scanAt) {
		return
	}
	for id := range cur.byProc {
		if !aliveInProc[id] {
			delete(cur.byProc, id)
		}
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

func peerTitleFromHerdr(wsLabel, paneLabel, paneTitle, agentName, cwd, agent string) string {
	if t := sanitizePeerLabel(paneLabel, 120); t != "" {
		return t
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

// recordLocatedProcs replaces each running session's current (pid, start) ->
// Corral agent map from a full locate scan's results (zero extra remote calls).
// Only the process selected by pickConvoProc for each pane or tmux active pane
// is recorded; older scans and non-full scans are ignored.
func (s *Server) recordLocatedProcs(sess *term.Session, l convoLocated, full bool, observedAt time.Time) {
	if !full || s == nil || sess == nil || sess.OwnerID <= 0 || sess.HostID <= 0 || l.noProc {
		return
	}
	if observedAt.IsZero() {
		observedAt = time.Now()
	}
	hc := peerHostConfig(sess)
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

	aliveInProc := make(map[peerProcID]bool, len(l.procs))
	panesBySock := map[string]map[string]struct{}{}
	for _, p := range l.procs {
		if p.pid <= 0 || p.start <= 0 {
			continue
		}
		aliveInProc[peerProcID{pid: p.pid, start: p.start}] = true
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
		byProc := map[peerProcID]livePeerEntry{}
		info := rs.Info()
		snapIdx := snapIndexes[rs.ID]
		for pane := range panesBySock[sock] {
			p := pickConvoProc(l.procs, "herdr", pane, sock, "", "", s.herdrSocks.sid(rs.ID, pane))
			if p == nil || p.pid <= 0 || p.start <= 0 {
				continue
			}
			hp, wsLabel, paneExists := snapIdx.lookup(pane)
			if !paneExists || hp.AgentStatus == herdr.StatusGone {
				continue
			}
			if hp.Agent != "" && !samePeerProgram(p.agent, hp.Agent) {
				continue
			}
			if p.sid != "" && agentSession(hp) != "" && agentSession(hp) != p.sid {
				continue
			}
			title := peerTitleFromHerdr(wsLabel, hp.Label, hp.Title, hp.Name, p.cwd, p.agent)
			if title == "" {
				continue
			}
			if wsLabel == "~" {
				wsLabel = ""
			}
			byProc[peerProcID{pid: p.pid, start: p.start}] = livePeerEntry{
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
				termID:   hp.TerminalID,
				agentSID: p.sid,
			}
		}
		s.peers.replace(rs.ID, &liveLocatePeers{
			ownerID:    rs.OwnerID,
			hostID:     rs.HostID,
			hostConfig: hc,
			scanAt:     observedAt,
			byProc:     byProc,
		})
	}

	if sess.Persistence == "tmux" {
		byProc := map[peerProcID]livePeerEntry{}
		if l.tmuxActive != "" && l.tmuxPID != "" {
			if p := pickConvoProc(l.procs, "tmux", l.tmuxActive, "", l.tmuxPID, l.tmuxTTY, ""); p != nil && p.pid > 0 && p.start > 0 {
				info := sess.Info()
				title := peerTitleFromTmux(l.tmuxTitle, info.DisplayName, info.MuxSession, p.agent)
				if title != "" {
					byProc[peerProcID{pid: p.pid, start: p.start}] = livePeerEntry{
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
					}
				}
			}
		}
		s.peers.replace(sess.ID, &liveLocatePeers{
			ownerID:    sess.OwnerID,
			hostID:     sess.HostID,
			hostConfig: hc,
			scanAt:     observedAt,
			byProc:     byProc,
		})
	}

	// Prune any other tmux session's process on this host if it is no longer in
	// /proc (including empty full scans).
	for _, rs := range running {
		if rs.Persistence == "tmux" && rs.ID != sess.ID {
			s.peers.pruneTmuxAbsent(rs.ID, observedAt, aliveInProc)
		}
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
	for _, rs := range running {
		if rs.Persistence == "herdr" {
			if snap, ok := s.herdrSocks.snapshotOf(rs.ID); ok {
				snapIndexes[rs.ID] = indexHerdrSnapshot(snap)
			}
		}
	}

	out := map[string]convo.PeerInfo{}
	s.peers.mu.Lock()
	for _, ref := range refs {
		id := peerProcID{pid: ref.PID, start: ref.Start}
		for _, rs := range running {
			cur := s.peers.bySess[rs.ID]
			if cur == nil || cur.ownerID != sess.OwnerID || cur.hostID != sess.HostID || cur.hostConfig != hc {
				continue
			}
			entry, ok := cur.byProc[id]
			if !ok {
				continue
			}
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
				if locatedSID := s.herdrSocks.locatedOf(rs.ID, pi.Pane); entry.agentSID != "" && locatedSID != "" && locatedSID != entry.agentSID {
					continue
				}
				if wsLabel != "" && wsLabel != "~" {
					pi.Workspace = sanitizePeerLabel(wsLabel, 120)
				}
			}
			info := rs.Info()
			pi.HostName = sanitizePeerLabel(info.HostName, 120)
			pi.DisplayName = sanitizePeerLabel(info.DisplayName, 120)
			out[peerMapKey(ref.PID, ref.Start)] = pi
			break
		}
	}
	s.peers.mu.Unlock()

	if len(out) == 0 {
		return nil
	}
	return out
}
