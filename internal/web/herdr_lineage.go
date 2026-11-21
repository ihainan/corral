package web

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/ihainan/corral/internal/herdr"
	"github.com/ihainan/corral/internal/sftplite"
	"github.com/ihainan/corral/internal/store"
	"github.com/ihainan/corral/internal/term"
)

const (
	lineageKeptFor       = 30 * 24 * time.Hour
	lineagePruneInterval = time.Hour
	lineageProbeCooldown = 10 * time.Second
	lineageProbeTimeout  = 10 * time.Second
	lineageProbeMaxFiles = 6
	lineageProbePerFile  = 384 << 10 // 384 KiB tail per candidate parent log
	lineageProbeMaxBytes = 2 << 20   // 2 MiB total budget per probe run
	lineageMetadataTTLMs = 86400000  // 24h (herdr max ttl_ms)
)

type livePaneInfo struct {
	PaneID     string
	TerminalID string
	Session    string
	Agent      string
	Tokens     map[string]string
}

type knownPaneLog struct {
	agent   string
	session string
	termID  string
	logPath string
}

// lineageTracker maintains the confirmed/rejected parent-child edges for one
// watched herdr server, synchronized with SQLite.
type lineageTracker struct {
	srv          *Server
	sess         *term.Session
	ownerID      int64
	hostID       int64
	server       string
	socket       string
	herdrSession string

	mu          sync.Mutex
	rows        []store.AgentLineage
	live        map[string]livePaneInfo // pane_id -> latest snapshot info
	termToPane  map[string]string       // terminal_id -> pane_id
	knownPanes  map[string]bool         // agent panes already seen in snapshots
	paneLogs    map[string]knownPaneLog // pane_id -> located log path (zero extra SSH)
	lastPruneAt time.Time
	loaded      bool
	poke        func()

	probeMu      sync.Mutex
	probeRunning bool
	lastProbeAt  time.Time
	probedSize   map[string]int64 // logPath -> last scanned file size
	retriedPane  map[string]bool  // child pane_id -> delayed retry already scheduled
}

func (s *Server) newLineageTracker(sess *term.Session, server, socket, herdrSession string) *lineageTracker {
	return &lineageTracker{
		srv:          s,
		sess:         sess,
		ownerID:      sess.OwnerID,
		hostID:       sess.HostID,
		server:       server,
		socket:       socket,
		herdrSession: herdrSession,
		live:         map[string]livePaneInfo{},
		termToPane:   map[string]string{},
		knownPanes:   map[string]bool{},
		paneLogs:     map[string]knownPaneLog{},
		probedSize:   map[string]int64{},
		retriedPane:  map[string]bool{},
	}
}

func (t *lineageTracker) setPoke(fn func()) {
	t.mu.Lock()
	t.poke = fn
	t.mu.Unlock()
}

func (t *lineageTracker) notify() {
	t.mu.Lock()
	fn := t.poke
	t.mu.Unlock()
	if fn != nil {
		fn()
	}
}

func (t *lineageTracker) notePaneLog(pane, agent, session, logPath string) {
	if pane == "" || logPath == "" || (agent != "claude" && agent != "codex" && agent != "agy") {
		return
	}
	var trigger []string
	t.mu.Lock()
	termID := t.live[pane].TerminalID
	prev, had := t.paneLogs[pane]
	t.paneLogs[pane] = knownPaneLog{agent: agent, session: session, termID: termID, logPath: logPath}
	if !had || prev.logPath != logPath {
		for _, p := range t.live {
			if p.Agent != "" && p.PaneID != pane && !t.hasRowForLocked(p.PaneID, p.TerminalID) {
				trigger = append(trigger, p.PaneID)
			}
		}
	}
	t.mu.Unlock()
	if len(trigger) > 0 && t.sess != nil {
		go t.probeCandidateParents(context.Background(), trigger)
	}
}

func (t *lineageTracker) liveOf(pane string) (livePaneInfo, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	info, ok := t.live[pane]
	return info, ok
}

func (t *lineageTracker) reloadLocked(ctx context.Context) error {
	rows, err := t.srv.store.ListAgentLineage(ctx, t.ownerID, t.hostID, t.server)
	if err != nil {
		return err
	}
	t.rows = rows
	t.loaded = true
	return nil
}

// OnEvent handles decoded herdr event stream envelopes (specifically pane_moved)
// so pane renames/moves migrate persisted edges immediately.
func (t *lineageTracker) OnEvent(ctx context.Context, event string, data json.RawMessage) {
	if event != "pane_moved" || len(data) == 0 {
		return
	}
	var payload struct {
		PreviousPaneID string `json:"previous_pane_id"`
		Pane           *struct {
			PaneID     string `json:"pane_id"`
			TerminalID string `json:"terminal_id"`
		} `json:"pane"`
	}
	if json.Unmarshal(data, &payload) != nil || payload.Pane == nil {
		return
	}
	newPane := payload.Pane.PaneID
	termID := sanitizeTermID(payload.Pane.TerminalID)
	if termID == "" || !herdrPaneID.MatchString(newPane) || newPane == payload.PreviousPaneID {
		return
	}
	mctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	changed, err := t.srv.store.MigrateAgentLineagePanes(mctx, t.ownerID, t.hostID, t.server, map[string]string{termID: newPane}, time.Now().UnixMilli())
	if err != nil {
		if t.srv.log != nil {
			t.srv.log.Warn("agent lineage pane move failed", "err", err)
		}
		return
	}
	if changed {
		t.mu.Lock()
		_ = t.reloadLocked(mctx)
		t.mu.Unlock()
		t.notify()
	}
}

// OnSnapshot reconciles live terminal_id -> pane_id mappings, upgrades pending
// empty-term edges, ingests explicit metadata tokens, prunes retired old rows,
// and triggers a bounded on-demand probe when new unlinked agent panes appear.
func (t *lineageTracker) OnSnapshot(ctx context.Context, snap *herdr.Snapshot) {
	if snap == nil {
		return
	}
	now := time.Now()
	nowMs := now.UnixMilli()

	live := map[string]livePaneInfo{}
	termToPane := map[string]string{}
	aliveTerms := map[string]bool{}

	mergePane := func(p herdr.Pane) {
		if p.ID == "" {
			return
		}
		cur := live[p.ID]
		cur.PaneID = p.ID
		if p.TerminalID != "" {
			cur.TerminalID = sanitizeTermID(p.TerminalID)
		}
		if s := agentSession(p); s != "" {
			cur.Session = s
		}
		if p.Agent != "" {
			cur.Agent = p.Agent
		}
		if len(p.Tokens) > 0 {
			if cur.Tokens == nil {
				cur.Tokens = map[string]string{}
			}
			for k, v := range p.Tokens {
				cur.Tokens[k] = v
			}
		}
		live[p.ID] = cur
		if cur.TerminalID != "" {
			termToPane[cur.TerminalID] = cur.PaneID
			aliveTerms[cur.TerminalID] = true
		}
	}
	for _, p := range snap.Panes {
		mergePane(p)
	}
	for _, p := range snap.Agents {
		mergePane(p)
	}

	sctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	// 1. Snapshot-driven migration of moved terminals (covers both confirmed and rejected rows).
	changed := false
	if moved, err := t.srv.store.MigrateAgentLineagePanes(sctx, t.ownerID, t.hostID, t.server, termToPane, nowMs); err != nil {
		if t.srv.log != nil {
			t.srv.log.Warn("agent lineage migrate failed", "err", err)
		}
	} else if moved {
		changed = true
	}

	// 2. Active-aware TTL pruning (never prunes rows whose terminal is still alive),
	//    and always reload authoritative rows from SQLite so multi-watcher sessions
	//    immediately observe links/unlinks performed by another tab/watcher.
	t.mu.Lock()
	shouldPrune := t.lastPruneAt.IsZero() || now.Sub(t.lastPruneAt) >= lineagePruneInterval
	t.mu.Unlock()
	if shouldPrune {
		cutoff := now.Add(-lineageKeptFor).UnixMilli()
		if err := t.srv.store.PruneRetiredAgentLineage(sctx, t.ownerID, t.hostID, t.server, cutoff, aliveTerms); err == nil {
			t.mu.Lock()
			t.lastPruneAt = now
			t.mu.Unlock()
		}
	}

	t.mu.Lock()
	if err := t.reloadLocked(sctx); err != nil && t.srv.log != nil {
		t.srv.log.Warn("agent lineage load failed", "err", err)
	}
	snapshotRows := append([]store.AgentLineage(nil), t.rows...)
	t.mu.Unlock()

	// 3. Upgrade confirmed rows missing child_term_id / parent_term_id / session from live snapshot.
	// Uses UpgradeAgentLineageIdentity so concurrent manual unlinks can never be resurrected.
	for _, r := range snapshotRows {
		if r.State != store.LineageConfirmed {
			continue
		}
		childLive, hasChild := live[r.ChildPane]
		if !hasChild {
			continue
		}
		if r.ChildTermID != "" && childLive.TerminalID != "" && r.ChildTermID != childLive.TerminalID {
			continue
		}
		if r.ChildSession != "" && childLive.Session != "" && r.ChildSession != childLive.Session {
			continue
		}
		needUpgrade := (r.ChildTermID == "" && childLive.TerminalID != "") ||
			(r.ChildSession == "" && childLive.Session != "")
		parentTerm := r.ParentTermID
		parentSess := r.ParentSession
		if pLive, hasParent := live[r.ParentPane]; hasParent {
			parentCompatible := (r.ParentTermID == "" || pLive.TerminalID == "" || r.ParentTermID == pLive.TerminalID) &&
				(r.ParentSession == "" || pLive.Session == "" || r.ParentSession == pLive.Session)
			if parentCompatible {
				if parentTerm == "" && pLive.TerminalID != "" {
					parentTerm = pLive.TerminalID
					needUpgrade = true
				}
				if parentSess == "" && pLive.Session != "" {
					parentSess = pLive.Session
					needUpgrade = true
				}
			}
		}
		if !needUpgrade {
			continue
		}
		up := r
		if up.ChildTermID == "" {
			up.ChildTermID = childLive.TerminalID
		}
		if up.ChildSession == "" {
			up.ChildSession = childLive.Session
		}
		up.ParentTermID = parentTerm
		up.ParentSession = parentSess
		up.UpdatedAt = nowMs
		if c, err := t.srv.store.UpgradeAgentLineageIdentity(sctx, up); err == nil && c {
			changed = true
		}
	}

	// 4. Signal 1: Ingest explicit metadata tokens from live panes (zero extra SSH).
	for _, p := range live {
		if len(p.Tokens) == 0 {
			continue
		}
		parentPane := p.Tokens["corral_parent_pane"]
		parentTerm := sanitizeTermID(p.Tokens["corral_parent_term"])
		parentSess := clipLineageSession(p.Tokens["corral_parent_session"])
		childSessTok := clipLineageSession(p.Tokens["corral_child_session"])
		spawnOp := clipLineageSession(p.Tokens["corral_spawn_op"])

		if parentPane == "" && parentTerm == "" {
			continue
		}
		// Resolve moved parent by terminal_id if available.
		if parentTerm != "" {
			if resolved, ok := termToPane[parentTerm]; ok {
				parentPane = resolved
			}
		}
		if !herdrPaneID.MatchString(parentPane) || parentPane == p.PaneID {
			continue
		}
		// Symmetric occupant session check: stale token from a previous occupant must not re-bind.
		if childSessTok != "" && p.Session != "" && childSessTok != p.Session {
			continue
		}
		if pLive, hasParent := live[parentPane]; hasParent {
			if parentTerm != "" && pLive.TerminalID != "" && parentTerm != pLive.TerminalID {
				continue
			}
			if parentSess != "" && pLive.Session != "" && parentSess != pLive.Session {
				continue
			}
			if parentTerm == "" {
				parentTerm = pLive.TerminalID
			}
			if parentSess == "" {
				parentSess = pLive.Session
			}
		}
		childSess := p.Session
		if childSess == "" {
			childSess = childSessTok
		}
		edge := store.AgentLineage{
			OwnerID:       t.ownerID,
			HostID:        t.hostID,
			Server:        t.server,
			ChildPane:     p.PaneID,
			ChildTermID:   p.TerminalID,
			ChildSession:  childSess,
			ParentPane:    parentPane,
			ParentTermID:  parentTerm,
			ParentSession: parentSess,
			Source:        store.LineageMetadata,
			State:         store.LineageConfirmed,
			EvidenceKey:   spawnOp,
			CreatedAt:     nowMs,
			UpdatedAt:     nowMs,
		}
		if c, err := t.srv.store.UpsertAgentLineage(sctx, edge); err == nil && c {
			changed = true
		}
	}

	// 5. Update in-memory state and detect newly-appeared unlinked agent panes for bounded probe.
	var newUnlinked []string
	t.mu.Lock()
	if changed {
		_ = t.reloadLocked(sctx)
	}
	t.live = live
	t.termToPane = termToPane
	for _, p := range live {
		if p.Agent == "" {
			continue
		}
		if !t.knownPanes[p.PaneID] {
			t.knownPanes[p.PaneID] = true
			if !t.hasRowForLocked(p.PaneID, p.TerminalID) {
				newUnlinked = append(newUnlinked, p.PaneID)
			}
		}
	}
	// Forget closed panes (and evict stale paneLogs when a pane disappears or its terminal changes).
	for pane := range t.knownPanes {
		if p, ok := live[pane]; !ok || p.Agent == "" {
			delete(t.knownPanes, pane)
		}
	}
	for pane, kl := range t.paneLogs {
		p, ok := live[pane]
		if !ok || p.Agent == "" || (kl.termID != "" && p.TerminalID != "" && kl.termID != p.TerminalID) || (kl.session != "" && p.Session != "" && kl.session != p.Session) {
			delete(t.paneLogs, pane)
		}
	}
	t.mu.Unlock()

	if len(newUnlinked) > 0 && t.sess != nil {
		go t.probeCandidateParents(ctx, newUnlinked)
	}
}

func (t *lineageTracker) hasRowForLocked(pane, termID string) bool {
	for _, r := range t.rows {
		if r.ChildPane == pane && (r.ChildTermID == "" || termID == "" || r.ChildTermID == termID) {
			return true
		}
		if termID != "" && r.ChildTermID == termID {
			return true
		}
	}
	return false
}

// RecordCorralSpawn persists a confirmed corral_spawn edge from POST /api/sessions/{id}/agents.
func (t *lineageTracker) RecordCorralSpawn(ctx context.Context, childPane, childTerm, parentPane, parentTerm, parentSess, evidenceKey string) {
	if !herdrPaneID.MatchString(childPane) || !herdrPaneID.MatchString(parentPane) || childPane == parentPane {
		return
	}
	nowMs := time.Now().UnixMilli()
	sctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	changed, err := t.srv.store.UpsertAgentLineage(sctx, store.AgentLineage{
		OwnerID:       t.ownerID,
		HostID:        t.hostID,
		Server:        t.server,
		ChildPane:     childPane,
		ChildTermID:   sanitizeTermID(childTerm),
		ParentPane:    parentPane,
		ParentTermID:  sanitizeTermID(parentTerm),
		ParentSession: clipLineageSession(parentSess),
		Source:        store.LineageCorralSpawn,
		State:         store.LineageConfirmed,
		EvidenceKey:   evidenceKey,
		CreatedAt:     nowMs,
		UpdatedAt:     nowMs,
	})
	if err != nil {
		if t.srv.log != nil {
			t.srv.log.Warn("agent lineage corral_spawn persist failed", "err", err)
		}
		return
	}
	if changed {
		t.mu.Lock()
		_ = t.reloadLocked(sctx)
		t.mu.Unlock()
		t.notify()
		t.srv.herdrSocks.reloadLineageForServer(sctx, t)
	}
}

// RecordToolLog validates occupant identity and persists a tool_log discovery
// emitted by lineageExtractor.
func (t *lineageTracker) RecordToolLog(ctx context.Context, parentPane, expectedParentTerm, expectedParentSession string, d lineageDiscovery) {
	if !herdrPaneID.MatchString(parentPane) || !herdrPaneID.MatchString(d.ChildPane) || parentPane == d.ChildPane {
		return
	}

	// Re-verify live parent occupant identity before committing.
	t.mu.Lock()
	pLive, hasParent := t.live[parentPane]
	cLive, hasChild := t.live[d.ChildPane]
	if d.ChildTermID != "" {
		if movedPane, ok := t.termToPane[d.ChildTermID]; ok {
			d.ChildPane = movedPane
			cLive, hasChild = t.live[movedPane]
		}
	}
	t.mu.Unlock()

	if hasParent {
		if expectedParentTerm != "" && pLive.TerminalID != "" && expectedParentTerm != pLive.TerminalID {
			return
		}
		if expectedParentSession != "" && pLive.Session != "" && expectedParentSession != pLive.Session {
			return
		}
	}

	parentTerm := expectedParentTerm
	parentSess := expectedParentSession
	if hasParent {
		if parentTerm == "" {
			parentTerm = pLive.TerminalID
		}
		if parentSess == "" {
			parentSess = pLive.Session
		}
	}

	childTerm := d.ChildTermID
	childSess := d.ChildSession
	if hasChild {
		if childTerm != "" && cLive.TerminalID != "" && childTerm != cLive.TerminalID {
			// A newer terminal now occupies that pane_id; keep original childTerm
			// (do not bind to the newer occupant).
		} else {
			if childTerm == "" {
				childTerm = cLive.TerminalID
			}
			if childSess == "" {
				childSess = cLive.Session
			}
		}
	}

	nowMs := time.Now().UnixMilli()
	sctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	// agent_started enrichment only updates an already-confirmed edge for this parent.
	if d.Action == "agent_started" {
		if childSess == "" {
			return
		}
		t.mu.Lock()
		var match *store.AgentLineage
		for i := range t.rows {
			r := &t.rows[i]
			if r.State == store.LineageConfirmed && r.ParentPane == parentPane &&
				((childTerm != "" && r.ChildTermID == childTerm) || r.ChildPane == d.ChildPane) {
				cp := *r
				match = &cp
				break
			}
		}
		t.mu.Unlock()
		if match == nil || match.ChildSession != "" {
			return
		}
		match.ChildSession = childSess
		match.UpdatedAt = nowMs
		// Use UpgradeAgentLineageIdentity (never UpsertAgentLineage) so a cached
		// manual row can never resurrect a concurrent manual unlink.
		if changed, err := t.srv.store.UpgradeAgentLineageIdentity(sctx, *match); err == nil && changed {
			t.mu.Lock()
			_ = t.reloadLocked(sctx)
			t.mu.Unlock()
			t.notify()
			t.srv.herdrSocks.reloadLineageForServer(sctx, t)
		}
		return
	}

	changed, err := t.srv.store.UpsertAgentLineage(sctx, store.AgentLineage{
		OwnerID:       t.ownerID,
		HostID:        t.hostID,
		Server:        t.server,
		ChildPane:     d.ChildPane,
		ChildTermID:   childTerm,
		ChildSession:  clipLineageSession(childSess),
		ParentPane:    parentPane,
		ParentTermID:  sanitizeTermID(parentTerm),
		ParentSession: clipLineageSession(parentSess),
		Source:        store.LineageToolLog,
		State:         store.LineageConfirmed,
		EvidenceKey:   d.EvidenceKey,
		CreatedAt:     nowMs,
		UpdatedAt:     nowMs,
	})
	if err != nil {
		return
	}
	if changed {
		t.mu.Lock()
		_ = t.reloadLocked(sctx)
		t.mu.Unlock()
		t.notify()
		t.srv.herdrSocks.reloadLineageForServer(sctx, t)
	}
}

// decorate populates ParentPane, ParentTermID, ParentSession, LineageSource,
// and TerminalID on each live herdrAgent in sum, enforcing symmetric occupant
// identity binding for both child and parent.
func (t *lineageTracker) decorate(sum *herdrSummary) {
	if t == nil || sum == nil {
		return
	}
	t.mu.Lock()
	rows := append([]store.AgentLineage(nil), t.rows...)
	live := make(map[string]livePaneInfo, len(t.live))
	for k, v := range t.live {
		live[k] = v
	}
	t.mu.Unlock()

	// Build candidate map by child pane after symmetric identity checks.
	type edge struct {
		parentPane    string
		parentTermID  string
		parentSession string
		source        string
	}
	byChild := map[string]edge{}

	for _, r := range rows {
		if r.State != store.LineageConfirmed {
			continue
		}
		childLive, hasChild := live[r.ChildPane]
		if !hasChild {
			continue
		}
		// Child identity match.
		if r.ChildTermID != "" && childLive.TerminalID != "" && r.ChildTermID != childLive.TerminalID {
			continue
		}
		if r.ChildSession != "" && childLive.Session != "" && r.ChildSession != childLive.Session {
			continue
		}
		// Symmetric parent identity match (when parent pane is live).
		if parentLive, hasParent := live[r.ParentPane]; hasParent {
			if r.ParentTermID != "" && parentLive.TerminalID != "" && r.ParentTermID != parentLive.TerminalID {
				continue
			}
			if r.ParentSession != "" && parentLive.Session != "" && r.ParentSession != parentLive.Session {
				continue
			}
		}
		byChild[r.ChildPane] = edge{
			parentPane:    r.ParentPane,
			parentTermID:  r.ParentTermID,
			parentSession: r.ParentSession,
			source:        r.Source,
		}
	}

	// Belt-and-suspenders cycle check on the live projection.
	safeEdge := func(pane string) (edge, bool) {
		e, ok := byChild[pane]
		if !ok {
			return edge{}, false
		}
		cur := e.parentPane
		for depth := 0; depth < 16; depth++ {
			if cur == pane {
				return edge{}, false
			}
			next, has := byChild[cur]
			if !has {
				return e, true
			}
			cur = next.parentPane
		}
		return edge{}, false
	}

	for wi := range sum.Workspaces {
		for ai := range sum.Workspaces[wi].Agents {
			a := &sum.Workspaces[wi].Agents[ai]
			if info, ok := live[a.PaneID]; ok && info.TerminalID != "" {
				a.TerminalID = info.TerminalID
			}
			if e, ok := safeEdge(a.PaneID); ok {
				a.ParentPane = e.parentPane
				a.ParentTermID = e.parentTermID
				a.ParentSession = e.parentSession
				a.LineageSource = e.source
			}
		}
	}
}

// probeCandidateParents performs a strictly bounded SFTP tail scan (zero extra
// SSH exec calls) over known live parent logs when a newly-appeared agent pane
// has no lineage yet.
func (t *lineageTracker) probeCandidateParents(ctx context.Context, newPanes []string) {
	t.probeMu.Lock()
	if t.probeRunning || time.Since(t.lastProbeAt) < lineageProbeCooldown {
		t.probeMu.Unlock()
		return
	}
	t.probeRunning = true
	t.lastProbeAt = time.Now()
	t.probeMu.Unlock()

	defer func() {
		t.probeMu.Lock()
		t.probeRunning = false
		t.probeMu.Unlock()
	}()

	type candidate struct {
		pane    string
		termID  string
		session string
		agent   string
		logPath string
	}

	newSet := map[string]bool{}
	for _, p := range newPanes {
		newSet[p] = true
	}

	t.mu.Lock()
	var cands []candidate
	for pane, kl := range t.paneLogs {
		if newSet[pane] {
			continue
		}
		info, ok := t.live[pane]
		if !ok || info.Agent == "" {
			continue
		}
		if kl.termID != "" && info.TerminalID != "" && kl.termID != info.TerminalID {
			continue
		}
		if kl.session != "" && info.Session != "" && kl.session != info.Session {
			continue
		}
		sess := info.Session
		if sess == "" {
			sess = kl.session
		}
		cands = append(cands, candidate{
			pane:    pane,
			termID:  info.TerminalID,
			session: sess,
			agent:   kl.agent,
			logPath: kl.logPath,
		})
		if len(cands) >= lineageProbeMaxFiles {
			break
		}
	}
	t.mu.Unlock()

	if len(cands) == 0 {
		return
	}

	pctx, cancel := context.WithTimeout(ctx, lineageProbeTimeout)
	defer cancel()

	var totalBytes int64
	_ = t.srv.withSFTP(pctx, t.sess, lineageProbeTimeout, func(c *sftplite.Client) error {
		for _, cand := range cands {
			if pctx.Err() != nil || totalBytes >= lineageProbeMaxBytes {
				break
			}
			fi, err := c.Stat(cand.logPath)
			if err != nil || !fi.Mode().IsRegular() || fi.Size() <= 0 {
				continue
			}
			t.probeMu.Lock()
			prevSize, seen := t.probedSize[cand.logPath]
			t.probeMu.Unlock()
			if seen && prevSize == fi.Size() {
				continue
			}

			readLen := min(fi.Size(), int64(lineageProbePerFile))
			if totalBytes+readLen > lineageProbeMaxBytes {
				readLen = lineageProbeMaxBytes - totalBytes
			}
			if readLen <= 0 {
				break
			}
			off := fi.Size() - readLen
			data, err := c.ReadRange(cand.logPath, off, int(readLen))
			if err != nil {
				continue
			}
			totalBytes += int64(len(data))
			t.probeMu.Lock()
			t.probedSize[cand.logPath] = fi.Size()
			t.probeMu.Unlock()

			// Skip partial first line when reading from mid-file.
			if off > 0 {
				if idx := bytes.IndexByte(data, '\n'); idx >= 0 {
					data = data[idx+1:]
				} else {
					continue
				}
			}

			ex := newLineageExtractor(cand.agent, t.socket, t.herdrSession)
			for _, d := range ex.Feed(data) {
				t.RecordToolLog(pctx, cand.pane, cand.termID, cand.session, d)
			}
		}
		return nil
	})

	// Schedule at most 1 delayed retry (3s later) for any newly-appeared pane
	// still unlinked (e.g. parent's tool_result flushed slightly after pane creation).
	var retry []string
	t.mu.Lock()
	for _, p := range newPanes {
		info, ok := t.live[p]
		if !ok || t.hasRowForLocked(p, info.TerminalID) {
			continue
		}
		if !t.retriedPane[p] {
			t.retriedPane[p] = true
			retry = append(retry, p)
		}
	}
	t.mu.Unlock()

	if len(retry) > 0 {
		time.AfterFunc(3*time.Second, func() {
			if ctx.Err() != nil {
				return
			}
			t.probeMu.Lock()
			// Allow this single delayed retry to bypass cooldown once.
			t.lastProbeAt = time.Time{}
			t.probeMu.Unlock()
			t.probeCandidateParents(ctx, retry)
		})
	}
}

func clipLineageSession(s string) string {
	if len(s) > 128 {
		return ""
	}
	return s
}

// feedConvoLineage piggybacks on raw JSONL bytes already read by conversation.go
// (zero extra SSH) and forwards confirmed child-creation discoveries to the
// session's lineageTracker.
func (s *Server) feedConvoLineage(sess *term.Session, st *convoState, raw []byte) {
	if sess == nil || sess.Persistence != "herdr" || st == nil || st.sub || st.pane == "" || len(raw) == 0 {
		return
	}
	if st.agent != "claude" && st.agent != "codex" && st.agent != "agy" {
		return
	}
	lt := s.herdrSocks.lineageOf(sess.ID)
	if lt == nil {
		return
	}
	if st.lineage == nil || st.lineageAgent != st.agent || st.lineageSocket != lt.socket {
		st.lineage = newLineageExtractor(st.agent, lt.socket, lt.herdrSession)
		st.lineageAgent = st.agent
		st.lineageSocket = lt.socket
	}
	discoveries := st.lineage.Feed(raw)
	if len(discoveries) == 0 {
		return
	}
	parentPane := st.pane
	parentSess := st.sid
	parentTerm := ""
	if info, ok := lt.liveOf(parentPane); ok {
		parentTerm = info.TerminalID
		if parentSess == "" {
			parentSess = info.Session
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	for _, d := range discoveries {
		lt.RecordToolLog(ctx, parentPane, parentTerm, parentSess, d)
	}
}

// POST /api/sessions/{id}/herdr/lineage
// {"action":"link"|"unlink","child_pane":"w1:p2","child_term_id":"...","child_session":"...","parent_pane":"w1:p1","parent_term_id":"...","parent_session":"..."}
func (s *Server) apiHerdrLineage(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Action        string `json:"action"`
		ChildPane     string `json:"child_pane"`
		ChildTermID   string `json:"child_term_id"`
		ChildSession  string `json:"child_session"`
		ParentPane    string `json:"parent_pane"`
		ParentTermID  string `json:"parent_term_id"`
		ParentSession string `json:"parent_session"`
	}
	if err := readJSON(r, &b); err != nil || !herdrPaneID.MatchString(b.ChildPane) ||
		(b.Action != "link" && b.Action != "unlink") ||
		(b.ChildTermID != "" && !herdrTermIDRe.MatchString(b.ChildTermID)) ||
		len(b.ChildSession) > 128 ||
		(b.ParentTermID != "" && !herdrTermIDRe.MatchString(b.ParentTermID)) ||
		len(b.ParentSession) > 128 {
		writeError(w, http.StatusBadRequest, "bad_request", "valid action and child_pane are required")
		return
	}
	if b.Action == "link" && (!herdrPaneID.MatchString(b.ParentPane) || b.ParentPane == b.ChildPane) {
		writeError(w, http.StatusBadRequest, "bad_request", "valid parent_pane different from child_pane is required")
		return
	}

	sess, _ := s.herdrSession(w, r)
	if sess == nil {
		return
	}
	lt := s.herdrSocks.lineageOf(sess.ID)
	if lt == nil {
		writeError(w, http.StatusConflict, "herdr_unavailable", "herdr is not available in this session")
		return
	}

	// Validate live occupant identity so stale UI actions cannot mutate a replaced pane.
	childLive, hasChild := lt.liveOf(b.ChildPane)
	if !hasChild {
		writeError(w, http.StatusConflict, "pane_gone", "the target pane is no longer present")
		return
	}
	if b.ChildTermID != "" && childLive.TerminalID != "" && b.ChildTermID != childLive.TerminalID {
		writeError(w, http.StatusConflict, "occupant_changed", "the target pane's terminal changed; refresh and try again")
		return
	}
	if b.ChildSession != "" && childLive.Session != "" && b.ChildSession != childLive.Session {
		writeError(w, http.StatusConflict, "occupant_changed", "the target pane's agent session changed; refresh and try again")
		return
	}

	childTerm := childLive.TerminalID
	if childTerm == "" {
		childTerm = b.ChildTermID
	}
	childSess := childLive.Session
	if childSess == "" {
		childSess = b.ChildSession
	}

	nowMs := time.Now().UnixMilli()
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	switch b.Action {
	case "link":
		parentLive, hasParent := lt.liveOf(b.ParentPane)
		if !hasParent {
			writeError(w, http.StatusConflict, "pane_gone", "the parent pane is no longer present")
			return
		}
		if b.ParentTermID != "" && parentLive.TerminalID != "" && b.ParentTermID != parentLive.TerminalID {
			writeError(w, http.StatusConflict, "occupant_changed", "the parent pane's terminal changed; refresh and try again")
			return
		}
		if b.ParentSession != "" && parentLive.Session != "" && b.ParentSession != parentLive.Session {
			writeError(w, http.StatusConflict, "occupant_changed", "the parent pane's agent session changed; refresh and try again")
			return
		}
		parentTerm := parentLive.TerminalID
		if parentTerm == "" {
			parentTerm = b.ParentTermID
		}
		parentSess := parentLive.Session
		if parentSess == "" {
			parentSess = b.ParentSession
		}
		_, err := s.store.UpsertAgentLineage(ctx, store.AgentLineage{
			OwnerID:       lt.ownerID,
			HostID:        lt.hostID,
			Server:        lt.server,
			ChildPane:     b.ChildPane,
			ChildTermID:   childTerm,
			ChildSession:  childSess,
			ParentPane:    b.ParentPane,
			ParentTermID:  parentTerm,
			ParentSession: parentSess,
			Source:        store.LineageManual,
			State:         store.LineageConfirmed,
			CreatedAt:     nowMs,
			UpdatedAt:     nowMs,
		})
		if errors.Is(err, store.ErrLineageCycle) {
			writeError(w, http.StatusConflict, "lineage_cycle", "linking these panes would create a cycle")
			return
		}
		if err != nil {
			s.internalError(w, r, err)
			return
		}
		lt.mu.Lock()
		_ = lt.reloadLocked(ctx)
		lt.mu.Unlock()
		lt.notify()
		s.herdrSocks.reloadLineageForServer(ctx, lt)
		s.audit(r, "herdr.lineage.link", sess.HostName, map[string]any{"session": sess.ID, "child_pane": b.ChildPane, "parent_pane": b.ParentPane})
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})

	case "unlink":
		if err := s.store.RejectAgentLineage(ctx, lt.ownerID, lt.hostID, lt.server, b.ChildPane, childTerm, childSess, nowMs); err != nil {
			s.internalError(w, r, err)
			return
		}
		lt.mu.Lock()
		_ = lt.reloadLocked(ctx)
		lt.mu.Unlock()
		lt.notify()
		s.herdrSocks.reloadLineageForServer(ctx, lt)
		s.audit(r, "herdr.lineage.unlink", sess.HostName, map[string]any{"session": sess.ID, "child_pane": b.ChildPane})
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	}
}
