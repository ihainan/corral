package web

// herdr integration: for sessions attached to herdr, the gateway watches the
// herdr server over the same SSH connection and pushes a compact summary of
// workspaces and agents to the browser alongside the terminal stream.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/ihainan/corral/internal/herdr"
	"github.com/ihainan/corral/internal/sshx"
	"github.com/ihainan/corral/internal/store"
	"github.com/ihainan/corral/internal/term"
)

type herdrAgent struct {
	PaneID string `json:"pane_id"`
	Agent  string `json:"agent"`
	Status string `json:"status"`
	Title  string `json:"title"`
	Cwd    string `json:"cwd"`
	// Session is the agent's own id of what runs there (Claude Code's
	// session id), as herdr's integration reports it, when it does.
	Session string `json:"session,omitempty"`
	// Since is when its status last changed, as this gateway saw it (unix
	// milliseconds): when it was first seen, for one that has not changed
	// since the gateway began watching.
	Since int64 `json:"since,omitempty"`
	// Round: which round of its status it is in, as this watcher numbers
	// them (see seenRounds): what the browser names when it marks one
	// "done" seen.
	Round int64 `json:"round,omitempty"`
	// Bg: for a Claude Code agent done or idle, what it left running (see
	// bgProber): "checking", "none", "running" (BgN subagents out, since
	// BgSince, unix ms), "unknown".
	Bg      string `json:"bg,omitempty"`
	BgN     int    `json:"bg_n,omitempty"`
	BgSince int64  `json:"bg_since,omitempty"`
	// BgCmds: for a Claude Code agent done or idle, how many commands it
	// left running in the background, the first of them listed (see
	// cmdProber); none where not known.
	BgCmds    int     `json:"bg_cmds,omitempty"`
	BgCmdList []bgCmd `json:"bg_cmd_list,omitempty"`
}

type herdrWorkspace struct {
	ID      string       `json:"id"`
	Label   string       `json:"label"`
	Number  int          `json:"number"`
	Status  string       `json:"status"`
	Focused bool         `json:"focused"`
	Agents  []herdrAgent `json:"agents"`
}

type herdrSummary struct {
	T          string           `json:"t"`
	Session    string           `json:"session"`
	Version    string           `json:"version"`
	Workspaces []herdrWorkspace `json:"workspaces"`
	Waiting    int              `json:"waiting"` // agents blocked on the user
	// The agent in the focused pane ("claude", "codex"…), if any.
	FocusedAgent string `json:"focused_agent,omitempty"`
	FocusedPane  string `json:"focused_pane,omitempty"` // (what closing a pane closes)
	// Lost: the gateway lost herdr after this summary; it is what was so
	// then, not now, until the next one.
	Lost bool `json:"lost,omitempty"`
	// Keys: the bindings of herdr.KeyNames the host's config.toml sets
	// (read when the watch began), for the shortcut panel's keys; until
	// read, or if it could not be, KeysUnknown (none are pressed then).
	Keys        map[string]string `json:"keys,omitempty"`
	KeysUnknown bool              `json:"keys_unknown,omitempty"`
}

// agentTimes holds when each pane's agent last changed status, as seen by
// one watcher (see herdrAgent.Since).
type agentTimes struct {
	mu sync.Mutex
	at map[string]agentTime
	// primed: the first summary was made. What it found was so since a time
	// not known (the gateway began watching then, say after a restart): no
	// time is given for it until it changes. An agent that shows up later
	// came then.
	primed bool
	// wrote: when each pane's agent last wrote to its log, as the last
	// look over the host found it (unix milliseconds). It is what a pane
	// the gateway never saw change is shown by, so that a restart does not
	// leave every agent without a time; missing counts the panes that
	// needed one, looking that a look is under way, and lookedAt when the
	// last one was.
	wrote    map[string]int64
	missing  int
	looking  bool
	lookedAt time.Time
}

// lookEvery is how often the gateway looks over a host for when each
// agent last wrote to its log (only while some pane has no time of its own).
const lookEvery = 60 * time.Second

// lastWrote is when the pane's agent last wrote to its log, 0 when that is
// not known. Asking says a look is wanted, whether or not there is a time
// already: a log goes on being written, so one found before goes stale.
func (a *agentTimes) lastWrote(pane string) int64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.missing++
	return a.wrote[pane]
}

// due: some pane has no time of its own and no look is under way or was
// made just now. It marks the look as begun.
func (a *agentTimes) due(now time.Time) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.missing == 0 || a.looking || now.Sub(a.lookedAt) < lookEvery {
		return false
	}
	a.looking, a.missing = true, 0
	return true
}

// found takes in what a look over the host found, by pane; nil: the look
// failed, and what was found before stands.
func (a *agentTimes) found(at map[string]int64, now time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.looking, a.lookedAt = false, now
	if at != nil {
		a.wrote = at
	}
}

type agentTime struct {
	status string
	t      time.Time
}

// changed notes a pane's agent going to status to (gone: forgotten). The
// same status told again (by another watcher of the server, or late) moves
// nothing.
func (a *agentTimes) changed(pane, to string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if to == herdr.StatusGone {
		delete(a.at, pane)
	} else if r, ok := a.at[pane]; !ok || r.status != to {
		a.at[pane] = agentTime{to, time.Now()}
	}
}

// of says when pane's agent came to status: now, for one not seen before
// or found in another status than noted (it changed while no one watched).
// old: the status is of a snapshot no longer current (its watcher lost
// herdr): it notes nothing, another watcher may know better.
func (a *agentTimes) of(pane, status string, old bool) int64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	r, ok := a.at[pane]
	if old {
		if ok && r.status == status && !r.t.IsZero() {
			return r.t.UnixMilli()
		}
		return 0
	}
	if !ok && !a.primed {
		a.at[pane] = agentTime{status: status} // (since a time not known)
		return 0
	}
	if !ok || r.status != status {
		r = agentTime{status, time.Now()}
		a.at[pane] = r
	}
	if r.t.IsZero() {
		return 0
	}
	return r.t.UnixMilli()
}

// prime says the first summary is made (see agentTimes.primed).
func (a *agentTimes) prime() {
	a.mu.Lock()
	a.primed = true
	a.mu.Unlock()
}

// herdrServers holds the agentTimes of each herdr server (by where the host
// is and the socket), so a second session watching one goes by what the
// first saw.
type herdrServers struct {
	mu sync.Mutex
	m  map[string]*agentTimes
}

func (h *herdrServers) of(host *store.Host, socket string) *agentTimes {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.m == nil {
		h.m = map[string]*agentTimes{}
	}
	k := herdrServerKey(host, socket)
	t := h.m[k]
	if t == nil {
		t = &agentTimes{at: map[string]agentTime{}}
		h.m[k] = t
	}
	return t
}

// herdrServerKey names a herdr server by where it is, not only which host
// record: a record edited to another machine is another server.
func herdrServerKey(host *store.Host, socket string) string {
	jump := int64(0)
	if host.JumpHostID != nil {
		jump = *host.JumpHostID
	}
	return fmt.Sprint(host.ID, "\x00", host.Username, "@", host.Address, ":", host.Port, "\x00", jump, "\x00", socket)
}

// paneSince is when a pane's agent last changed, as the gateway saw it;
// where it never saw it change (it began watching after, as a restart
// does), when that agent last wrote to its log instead, which is when it
// last said anything or was told anything.
func paneSince(p herdr.Pane, times *agentTimes, old bool) int64 {
	t := times.of(p.ID, p.AgentStatus, old)
	if t > 0 || old {
		// (Lost: the status is of a snapshot no longer current. A time of
		// its own is all it may be shown by; a log's would be of what the
		// pane does now, which this status is not.)
		return t
	}
	return times.lastWrote(p.ID)
}

func summarize(session string, s *herdr.Snapshot, times *agentTimes, old bool) herdrSummary {
	out := herdrSummary{T: "herdr", Session: session, Version: s.Version, Workspaces: []herdrWorkspace{}}
	idx := map[string]int{}
	for _, w := range s.Workspaces {
		idx[w.ID] = len(out.Workspaces)
		out.Workspaces = append(out.Workspaces, herdrWorkspace{ID: w.ID, Label: w.Label, Number: w.Number,
			Status: w.AgentStatus, Focused: w.Focused || w.ID == s.FocusedWorkspace, Agents: []herdrAgent{}})
	}
	agents := s.Agents
	if len(agents) == 0 {
		for _, p := range s.Panes {
			if p.Agent != "" {
				agents = append(agents, p)
			}
		}
	}
	for _, p := range agents {
		i, ok := idx[p.WorkspaceID]
		if !ok {
			continue
		}
		out.Workspaces[i].Agents = append(out.Workspaces[i].Agents,
			herdrAgent{PaneID: p.ID, Agent: p.Agent, Status: p.AgentStatus, Title: truncate(p.Title, 120), Cwd: p.Cwd, Session: agentSession(p), Since: paneSince(p, times, old)})
		if p.AgentStatus == herdr.StatusBlocked {
			out.Waiting++
		}
	}
	if !old {
		times.prime()
	}
	out.FocusedPane = s.FocusedPane
	for _, p := range s.Panes {
		if out.FocusedPane == "" && p.Focused {
			out.FocusedPane = p.ID
		}
	}
	for _, list := range [][]herdr.Pane{s.Panes, agents} {
		for _, p := range list {
			if p.Agent != "" && (p.ID == s.FocusedPane || (s.FocusedPane == "" && p.Focused)) {
				out.FocusedAgent = p.Agent
			}
		}
	}
	return out
}

func agentSession(p herdr.Pane) string {
	if p.AgentSession == nil || len(p.AgentSession.Value) > 128 {
		return ""
	}
	return p.AgentSession.Value
}

// herdrSockets remembers which herdr socket each terminal session is
// watching, and the agent sessions its panes run, as herdr last said.
type herdrSockets struct {
	mu     sync.Mutex
	path   map[string]string
	sids   map[string]map[string]string // terminal session → pane → the agent's session id
	rounds map[string]*seenRounds       // terminal session → its watcher's rounds
	bg     map[string]*bgProber         // terminal session → what its Claude panes left running
	// located: terminal session → pane → the Claude Code session reading
	// its conversation found it at, and when that was looked for (kept for
	// the session's life, bounded; "" none found there).
	located map[string]map[string]locatedAt
}

type locatedAt struct {
	sid string
	at  time.Time
}

// setLocated notes what a pane was found to run (sid, "" none) by a look
// begun at: one begun before the one noted says less, and is not taken.
func (h *herdrSockets) setLocated(id, pane, sid string, at time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.path[id]; !ok {
		return // (not watched: nothing reads it)
	}
	if h.located == nil {
		h.located = map[string]map[string]locatedAt{}
	}
	m := h.located[id]
	if m == nil {
		m = map[string]locatedAt{}
		h.located[id] = m
	}
	was, had := m[pane]
	if had && at.Before(was.at) {
		return
	}
	if !had && len(m) >= 256 {
		return
	}
	m[pane] = locatedAt{sid, at}
}

func (h *herdrSockets) locatedOf(id, pane string) string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.located[id][pane].sid
}

func (h *herdrSockets) setBg(id string, p *bgProber) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.bg == nil {
		h.bg = map[string]*bgProber{}
	}
	h.bg[id] = p
}

func (h *herdrSockets) bgOf(id string) *bgProber {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.bg[id]
}

func (h *herdrSockets) setRounds(id string, r *seenRounds) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.rounds == nil {
		h.rounds = map[string]*seenRounds{}
	}
	h.rounds[id] = r
}

func (h *herdrSockets) roundsOf(id string) *seenRounds {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.rounds[id]
}

func (h *herdrSockets) set(id, path string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if path == "" {
		delete(h.path, id)
		delete(h.sids, id)
		delete(h.rounds, id)
		delete(h.bg, id)
		delete(h.located, id)
	} else {
		h.path[id] = path
	}
}

// setSids keeps the agent session ids of a snapshot's panes.
func (h *herdrSockets) setSids(id string, snap *herdr.Snapshot) {
	m := map[string]string{}
	for _, list := range [][]herdr.Pane{snap.Panes, snap.Agents} {
		for _, p := range list {
			if v := agentSession(p); v != "" {
				m[p.ID] = v
			}
		}
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.path[id]; !ok {
		return // (the watch is over)
	}
	if h.sids == nil {
		h.sids = map[string]map[string]string{}
	}
	h.sids[id] = m
}

// sid is the agent session id herdr gives the pane ("" when it gives none).
func (h *herdrSockets) sid(id, pane string) string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.sids[id][pane]
}

func (h *herdrSockets) get(id string) string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.path[id]
}

// watchHerdr runs for the life of a herdr-attached session.
func (s *Server) watchHerdr(sess *term.Session, h *store.Host) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		<-sess.Finished()
		cancel()
	}()
	defer s.herdrSocks.set(sess.ID, "")

	// herdr starts its server when the session attaches; give it a moment.
	var socket string
	for attempt := 0; attempt < 10 && ctx.Err() == nil; attempt++ {
		dctx, dcancel := context.WithTimeout(ctx, 10*time.Second)
		list, err := herdr.Discover(dctx, sess.Client())
		dcancel()
		if errors.Is(err, herdr.ErrNotInstalled) {
			return
		}
		for _, si := range list {
			if si.Running && ((sess.HerdrSession == "" && si.Default) || si.Name == sess.HerdrSession) {
				socket = si.SocketPath
			}
		}
		if socket != "" {
			break
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Duration(attempt+1) * time.Second):
		}
	}
	if socket == "" {
		return
	}
	s.herdrSocks.set(sess.ID, socket)
	name := sess.HerdrSession
	if name == "" {
		name = "default"
	}
	times := s.herdrTimes.of(h, socket)
	// What was looked at before this watcher (read before its first
	// summary: the gateway's own database), and kept as more is (before
	// the look is said noted, so that a watcher begun next reads it).
	server := herdrServerKey(h, socket)
	kctx, kcancel := context.WithTimeout(ctx, 3*time.Second)
	prior, err := s.store.AgentsSeen(kctx, sess.OwnerID, sess.HostID, server, time.Now().Add(-seenKeptFor).UnixMilli())
	kcancel()
	if err != nil {
		s.log.Warn("agents seen not read", "err", err)
	}
	rounds := newSeenRounds(prior, func(pane string, a store.AgentSeen) error {
		kctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		err := s.store.SetAgentSeen(kctx, sess.OwnerID, sess.HostID, server, pane, a)
		if err != nil {
			s.log.Warn("agent seen not kept", "err", err)
		}
		return err
	})
	s.herdrSocks.setRounds(sess.ID, rounds)
	bg := newBgProber(func(ctx context.Context, sids []string, tail int) (map[string]bgRecord, error) {
		out, err := sshx.Output(ctx, sess.Client(), bgTailCommand(sids, tail), len(sids)*(tail+256))
		if err != nil {
			return nil, err
		}
		return parseBgTails(out), nil
	})
	s.herdrSocks.setBg(sess.ID, bg)
	go bg.run(ctx)
	cmds := newCmdProber(func(ctx context.Context) (map[string][]bgCmd, bool, error) {
		out, err := sshx.Output(ctx, sess.Client(), cmdScanCommand(), cmdScanMax)
		if err != nil {
			return nil, false, err
		}
		got, whole := parseCmdScan(string(out))
		return got, whole, nil
	})
	go cmds.run(ctx)
	// When each agent last wrote to its log: what the panes the gateway
	// never saw change are shown by (see paneSince). Looked for beside the
	// watch, only while a pane needs it, and at most once a minute: a slow
	// host holds up no summary.
	// (Also once at the start where what was looked at before is told by
	// it: see seenRounds.)
	go func() {
		tick := time.NewTicker(5 * time.Second)
		defer tick.Stop()
		first := make(chan time.Time, 1)
		if want, _ := rounds.wantsLook(); want {
			first <- time.Now()
		}
		for {
			var now time.Time
			select {
			case <-ctx.Done():
				return
			case now = <-first:
			case now = <-tick.C:
			}
			// (One for what was looked at before: not the one the watchers of
			// the server share, which it leaves as it was.)
			due := times.due(now)
			want, gen := rounds.wantsLook()
			if !due && !want {
				continue
			}
			lctx, cancel := context.WithTimeout(ctx, 20*time.Second)
			out, err := sshx.Output(lctx, sess.Client(), convoLocateTimesCommand(sess.Persistence, sess.TmuxSession)+"; "+hostNowCommand, convoLocateMax)
			got := time.Now().UnixMilli()
			cancel()
			var at map[string]int64
			if err == nil {
				at = map[string]int64{}
				found := parseConvoLocate(string(out))
				for _, p := range found.procs {
					// (Of this herdr server: another on the host names
					// its panes as this one does.)
					if p.herdrSocket != socket || !herdrPane.MatchString(p.herdrPane) {
						continue
					}
					if w := procWrote(p, found.cands); w > at[p.herdrPane] {
						at[p.herdrPane] = w
					}
				}
			}
			if due {
				times.found(at, time.Now())
			}
			if want {
				rounds.lookedOver(onGatewayClock(at, string(out), got), gen)
			}
		}
	}()
	// The bindings are read beside the watch, not before it: a slow host
	// holds up no summary. The last summary sent goes again with them.
	var (
		mu       sync.Mutex
		last     *herdrSummary
		keys     map[string]string
		keysRead bool
	)
	send := func(sum herdrSummary) { // (with mu held: none goes out of order)
		sum.Keys, sum.KeysUnknown = keys, !keysRead
		last = &sum
		told := rounds.withSeen(sum)
		bg.decorate(&told)
		cmds.decorate(&told)
		if b, err := json.Marshal(told); err == nil {
			sess.PublishMeta(b)
		}
	}
	publish := func(sum herdrSummary) {
		mu.Lock()
		defer mu.Unlock()
		send(sum)
	}
	go func() {
		for attempt := 0; attempt < 3 && ctx.Err() == nil; attempt++ {
			kctx, kcancel := context.WithTimeout(ctx, 10*time.Second)
			k, err := herdr.ReadKeys(kctx, sess.Client())
			kcancel()
			if err == nil {
				mu.Lock()
				keys, keysRead = k, true
				if last != nil {
					send(*last)
				}
				mu.Unlock()
				return
			}
			select {
			case <-ctx.Done():
			case <-time.After(time.Duration(attempt+1) * 5 * time.Second):
			}
		}
	}()
	w := herdr.NewWatcher(sess.Client(), socket,
		func(snap *herdr.Snapshot) {
			s.herdrSocks.setSids(sess.ID, snap)
			sum := summarize(name, snap, times, false)
			rounds.number(&sum, false)
			for _, w := range sum.Workspaces {
				for _, a := range w.Agents {
					sid := s.bgSession(sess, a.PaneID, a.Session)
					bg.consider(a.PaneID, a.Agent, a.Status, sid, a.Round)
					cmds.consider(a.PaneID, a.Agent, a.Status, sid)
				}
			}
			publish(sum)
		},
		// (A change is told before the snapshot that has it.)
		func(tr herdr.Transition) {
			times.changed(tr.Pane.ID, tr.To)
			rounds.changed(tr.Pane.ID, tr.To, agentSession(tr.Pane))
			r, _ := rounds.current(tr.Pane.ID)
			sid := s.bgSession(sess, tr.Pane.ID, r.session)
			bg.consider(tr.Pane.ID, tr.Pane.Agent, tr.To, sid, r.n)
			cmds.consider(tr.Pane.ID, tr.Pane.Agent, tr.To, sid)
			s.onAgentTransition(sess, h, tr)
		},
	)
	// An agent looked at: the last summary goes again, with it no longer
	// "done"; or what one left running told.
	again := func() {
		mu.Lock()
		defer mu.Unlock()
		if last != nil && ctx.Err() == nil {
			send(*last)
		}
	}
	rounds.setPoke(again)
	bg.setPoke(again)
	cmds.setPoke(again)
	// Subagents found back by the prober's own look, the turn's end that
	// would have told it not seen (two changes in one snapshot): the
	// "finished" held back goes, as for a turn ending now, if it is done
	// still (see onHerdrTransition).
	bg.setBack(func(pane string) {
		snap := w.Latest()
		if snap == nil {
			return
		}
		label := map[string]string{}
		for _, ws := range snap.Workspaces {
			label[ws.ID] = ws.Label
		}
		for _, list := range [][]herdr.Pane{snap.Agents, snap.Panes} {
			for _, p := range list {
				if p.ID == pane && p.AgentStatus == herdr.StatusDone {
					s.onAgentTransition(sess, h, herdr.Transition{Pane: p, Workspace: label[p.WorkspaceID], From: herdr.StatusWorking, To: herdr.StatusDone})
					return
				}
			}
		}
	})
	// The browser holds the last summary as current until told otherwise;
	// it keeps it (a page opened meanwhile gets it too), marked as old.
	w.OnLost = func() {
		rounds.lostHerdr()
		snap := w.Latest()
		if snap == nil {
			return
		}
		sum := summarize(name, snap, times, true)
		rounds.number(&sum, true)
		sum.Lost = true
		publish(sum)
	}
	w.Run(ctx)
	s.endSessionAlerts(sess) // (its agents wait no more)
}

func (s *Server) onAgentTransition(sess *term.Session, h *store.Host, tr herdr.Transition) {
	s.onHerdrTransition(sess, h, tr)
}

// herdrConn connects to the herdr server of one of the user's running
// sessions, or writes the error and returns nil. The caller closes it.
// herdr answers one request per connection (subscriptions aside).
func (s *Server) herdrConn(w http.ResponseWriter, r *http.Request, ctx context.Context) (*herdr.Client, *term.Session) {
	sess, socket := s.herdrSession(w, r)
	if sess == nil {
		return nil, nil
	}
	return herdrDial(w, ctx, sess, socket), sess
}

// herdrSession is one of the user's running sessions with herdr, and its
// socket; or nil, with the error written.
func (s *Server) herdrSession(w http.ResponseWriter, r *http.Request) (*term.Session, string) {
	u := currentUser(r)
	sess, err := s.terms.Get(u.ID, r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "no such session")
		return nil, ""
	}
	socket := s.herdrSocks.get(sess.ID)
	if socket == "" || sess.State() != term.StateRunning {
		writeError(w, http.StatusConflict, "herdr_unavailable", "herdr is not available in this session")
		return nil, ""
	}
	return sess, socket
}

// herdrDial connects to a session's herdr, or writes the error and returns nil.
func herdrDial(w http.ResponseWriter, ctx context.Context, sess *term.Session, socket string) *herdr.Client {
	// herdr traffic is a side operation on the terminal's connection: while
	// too many earlier ones are unfinished, new ones are refused.
	c, err := herdr.Dial(ctx, sess.Client(), socket)
	if errors.Is(err, sshx.ErrBusy) {
		writeError(w, http.StatusTooManyRequests, "herdr_busy", "herdr is still answering earlier actions")
		return nil
	}
	if err != nil {
		writeError(w, http.StatusBadGateway, "herdr_unavailable", "cannot reach herdr")
		return nil
	}
	return c
}

// writeHerdrError reports an error of a herdr request.
func writeHerdrError(w http.ResponseWriter, err error) {
	var apiErr *herdr.APIError
	if errors.As(err, &apiErr) {
		writeError(w, http.StatusUnprocessableEntity, "herdr_error", apiErr.Message)
		return
	}
	writeError(w, http.StatusBadGateway, "herdr_unavailable", "cannot reach herdr")
}

// POST /api/sessions/{id}/herdr {"action":"focus_workspace"|"focus_pane"|"zoom_pane","target":"…"}
// zoom_pane focuses a pane and has it fill its tab. Without a target, on the
// focused pane or tab (the shortcut panel's): unzoom ends a zoom,
// zoom_toggle starts or ends one, new_tab, split_right, split_down,
// focus_left/right/up/down (the pane next to it), next_tab, prev_tab,
// new_workspace. close_pane closes the pane given (the one the browser was
// told is focused, which the user confirmed), not whichever is focused by then.
var herdrUntargeted = map[string]bool{"unzoom": true, "zoom_toggle": true, "new_tab": true, "split_right": true, "split_down": true,
	"focus_left": true, "focus_right": true, "focus_up": true, "focus_down": true, "next_tab": true, "prev_tab": true, "new_workspace": true}

func (s *Server) apiHerdrAction(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Action string `json:"action"`
		Target string `json:"target"`
	}
	if err := readJSON(r, &b); err != nil || len(b.Target) > 64 || (b.Target == "") != herdrUntargeted[b.Action] {
		writeError(w, http.StatusBadRequest, "bad_request", "action and target are required")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	sess, socket := s.herdrSession(w, r)
	if sess == nil {
		return
	}
	c := herdrDial(w, ctx, sess, socket)
	if c == nil {
		return
	}
	defer c.Close()
	var err error
	switch b.Action {
	case "focus_workspace":
		err = c.FocusWorkspace(ctx, b.Target)
	case "focus_pane":
		err = c.FocusPane(ctx, b.Target)
	case "zoom_pane":
		err = c.ZoomPane(ctx, b.Target, "on")
	case "unzoom":
		err = c.ZoomPane(ctx, "", "off")
	case "zoom_toggle":
		err = c.ToggleZoom(ctx)
	case "new_tab":
		err = c.NewTab(ctx, "")
	case "split_right", "split_down":
		err = c.Split(ctx, strings.TrimPrefix(b.Action, "split_"))
	case "close_pane":
		if !herdrPaneID.MatchString(b.Target) {
			writeError(w, http.StatusBadRequest, "bad_request", "bad pane")
			return
		}
		err = c.ClosePane(ctx, b.Target)
	case "new_workspace":
		// (One request per connection: the snapshot for the focused
		// workspace, whose folder the new one follows, then a new one.)
		var snap *herdr.Snapshot
		if snap, err = c.Snapshot(ctx); err == nil {
			c2 := herdrDial(w, ctx, sess, socket)
			if c2 == nil {
				return
			}
			defer c2.Close()
			err = c2.NewWorkspace(ctx, snap.FocusedWorkspace)
		}
	case "focus_left", "focus_right", "focus_up", "focus_down":
		err = c.FocusDirection(ctx, strings.TrimPrefix(b.Action, "focus_"))
	case "next_tab", "prev_tab":
		// (herdr answers one request per connection: the snapshot, then a new one.)
		var tab string
		if tab, err = stepTab(ctx, c, b.Action == "next_tab"); err == nil && tab != "" {
			c2 := herdrDial(w, ctx, sess, socket)
			if c2 == nil {
				return
			}
			defer c2.Close()
			err = c2.FocusTab(ctx, tab)
		}
	default:
		writeError(w, http.StatusBadRequest, "bad_request", "unknown action")
		return
	}
	if err != nil {
		writeHerdrError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// stepTab is the next (or previous) tab of the focused workspace, by number,
// wrapping around; "" when there is no other.
func stepTab(ctx context.Context, c *herdr.Client, next bool) (string, error) {
	snap, err := c.Snapshot(ctx)
	if err != nil {
		return "", err
	}
	ws := snap.FocusedWorkspace
	var tabs []herdr.Tab
	cur := -1
	for _, t := range snap.Tabs {
		if t.WorkspaceID == ws {
			tabs = append(tabs, t)
		}
	}
	sort.Slice(tabs, func(i, j int) bool { return tabs[i].Number < tabs[j].Number })
	for i, t := range tabs {
		if t.Focused {
			cur = i
		}
	}
	if len(tabs) < 2 || cur < 0 {
		return "", nil
	}
	step := 1
	if !next {
		step = len(tabs) - 1
	}
	return tabs[(cur+step)%len(tabs)].ID, nil
}

// Answering an agent from the browser: the user sees a pane's screen, picks
// an answer, and it is typed into that pane only if the screen is still the
// one the user saw (the prompt has not moved on or changed meanwhile).

var (
	herdrPaneID = regexp.MustCompile(`^[A-Za-z0-9:_.-]{1,64}$`)
	// replyKeys are the keys an answer may press: choices, confirming,
	// cancelling, moving in a list.
	replyKeys = map[string]bool{"enter": true, "esc": true, "tab": true, "space": true,
		"up": true, "down": true, "left": true, "right": true}
)

const (
	maxReplyText   = 2000 // characters
	maxScreenBytes = 64 << 10
)

func init() {
	for c := '0'; c <= '9'; c++ {
		replyKeys[string(c)] = true
	}
}

// paneScreen reads a pane's visible text, without trailing blank space, and
// the digest that names it (of all of it); the text returned is at most
// maxScreenBytes (its end).
func paneScreen(ctx context.Context, c *herdr.Client, pane string) (text, digest string, err error) {
	text, digest, _, err = paneScreenCut(ctx, c, pane)
	return text, digest, err
}

// paneScreenCut is paneScreen, and whether its start was cut (longer than
// maxScreenBytes): then what is asked may not all be in it.
func paneScreenCut(ctx context.Context, c *herdr.Client, pane string) (text, digest string, cut bool, err error) {
	text, err = c.ReadPane(ctx, pane, 0)
	if err != nil {
		return "", "", false, err
	}
	text = strings.TrimRight(strings.ReplaceAll(text, "\r\n", "\n"), " \t\n")
	sum := sha256.Sum256([]byte(text))
	if len(text) > maxScreenBytes {
		at := len(text) - maxScreenBytes
		for at < len(text) && !utf8.RuneStart(text[at]) {
			at++
		}
		text, cut = text[at:], true
	}
	return text, hex.EncodeToString(sum[:]), cut, nil
}

// answeredFor is how long an answered screen stays answered: the same
// screen once more within it is taken for a repeated answer (a double tap,
// or a program that has not redrawn yet), not a new one.
const answeredFor = 10 * time.Second

// paneReplies serializes the answers to each pane and remembers the screen
// last answered there.
type paneReplies struct {
	mu sync.Mutex
	m  map[string]*paneReply
}

type paneReply struct {
	turn   chan struct{} // holds a token while an answer is under way
	users  int           // holding or waiting for the turn
	digest string
	at     time.Time
}

// lock takes a pane's turn, waiting for an answer under way, or gives up
// with ctx.
func (p *paneReplies) lock(ctx context.Context, key string) (*paneReply, error) {
	p.mu.Lock()
	if p.m == nil {
		p.m = map[string]*paneReply{}
	}
	for k, e := range p.m { // forget panes nobody answers any more
		if e.users == 0 && time.Since(e.at) > answeredFor {
			delete(p.m, k)
		}
	}
	e := p.m[key]
	if e == nil {
		e = &paneReply{turn: make(chan struct{}, 1)}
		p.m[key] = e
	}
	e.users++
	p.mu.Unlock()
	select {
	case e.turn <- struct{}{}:
		return e, nil
	case <-ctx.Done():
		p.mu.Lock()
		e.users--
		p.mu.Unlock()
		return nil, ctx.Err()
	}
}

func (p *paneReplies) unlock(e *paneReply) {
	<-e.turn
	p.mu.Lock()
	e.users--
	p.mu.Unlock()
}

// GET /api/sessions/{id}/herdr/panes/{pane}/screen
func (s *Server) apiHerdrScreen(w http.ResponseWriter, r *http.Request) {
	pane := r.PathValue("pane")
	if !herdrPaneID.MatchString(pane) {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid pane")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	c, _ := s.herdrConn(w, r, ctx)
	if c == nil {
		return
	}
	defer c.Close()
	text, digest, cut, err := paneScreenCut(ctx, c, pane)
	if err != nil {
		writeHerdrError(w, err)
		return
	}
	// Shown with the lines the terminal wrapped joined (see unwrapScreen);
	// its digest still that of the screen as shown, as an answer checks it.
	// (Asked last on this request's own connection: an older herdr refuses
	// that source, and closes it.)
	if !cut {
		if j, err := c.ReadPaneSource(ctx, pane, "recent_unwrapped", 0); err == nil {
			if u, ok := unwrapScreen(text, j); ok {
				text = u
			}
		}
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{"text": text, "digest": digest, "cut": cut})
}

// POST /api/sessions/{id}/herdr/panes/{pane}/reply
// {"digest": "<of the screen the user saw>", "keys": ["1"]} or {"digest": …, "text": "…"}
// The text is typed and Enter pressed. A screen that changed since, or that
// was answered moments ago, is answered with 409 and the screen as it is;
// nothing is typed then.
//
// herdr cannot compare and type in one step: what happens on the host
// between the check and the typing (another terminal answering in that
// moment) is not seen. The gap is kept short: answers to a pane go one at a
// time, and the connection that types is open before the screen is read.
// A pane's turn is taken before connecting, so answers waiting for it hold
// no connections.
// POST /api/sessions/{id}/herdr/panes/{pane}/seen {"round":…,"session":…}
// — the pane's agent, "done" in that round of that agent session (as the
// summary said), was looked at on the agents page: no longer "done" on any
// screen of Corral's (herdr's own badge is untouched: see seenRounds).
// {"seen":false}: that round is over, and nothing changed.
func (s *Server) apiHerdrSeen(w http.ResponseWriter, r *http.Request) {
	pane := r.PathValue("pane")
	var b struct {
		Round   int64  `json:"round"`
		Session string `json:"session"`
	}
	if err := readJSON(r, &b); err != nil || !herdrPaneID.MatchString(pane) || b.Round <= 0 || len(b.Session) > 128 {
		writeError(w, http.StatusBadRequest, "bad_request", "pane and round are required")
		return
	}
	sess, _ := s.herdrSession(w, r)
	if sess == nil {
		return
	}
	rs := s.herdrSocks.roundsOf(sess.ID)
	if rs == nil {
		writeError(w, http.StatusConflict, "herdr_unavailable", "herdr is not available in this session")
		return
	}
	ok, err := rs.markSeen(pane, b.Round, b.Session)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "not_kept", "it could not be noted")
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"seen": ok})
}

// hostNowCommand prints the host's time (unix ms; seconds where its date
// has no %N, or prints nothing for it) as a record of its own, "now=…", after what came before it.
const hostNowCommand = `sh -c 'n=$(date +%s%3N 2>/dev/null); case $n in ""|*[!0-9]*) n=$(date +%s)000;; esac; printf "now=%s\0" "$n"'`

// onGatewayClock puts times of the host's clock (unix ms, by pane) on the
// gateway's: by the host's time printed last in out (hostNowCommand), got
// when out came. nil, or no host time: nil (nothing known).
func onGatewayClock(at map[string]int64, out string, got int64) map[string]int64 {
	if at == nil {
		return nil
	}
	i := strings.LastIndex(out, "now=")
	if i < 0 {
		return nil
	}
	v, _, _ := strings.Cut(out[i+len("now="):], "\x00")
	now, err := strconv.ParseInt(v, 10, 64)
	if err != nil || now <= 0 {
		return nil
	}
	if now < 1e11 { // (seconds: a date with no %N leaves it out, digits all the same)
		now *= 1000
	}
	off := now - got // (the host ahead by; a little less than it is, by the way back)
	m := make(map[string]int64, len(at))
	for p, t := range at {
		m[p] = t - off
	}
	return m
}

func (s *Server) apiHerdrReply(w http.ResponseWriter, r *http.Request) {
	pane := r.PathValue("pane")
	var b struct {
		Digest string   `json:"digest"`
		Keys   []string `json:"keys"`
		Text   string   `json:"text"`
	}
	if err := readJSON(r, &b); err != nil || !herdrPaneID.MatchString(pane) || len(b.Digest) != 64 {
		writeError(w, http.StatusBadRequest, "bad_request", "pane, digest and an answer are required")
		return
	}
	switch {
	case b.Text != "" && len(b.Keys) == 0:
		if utf8.RuneCountInString(b.Text) > maxReplyText || !utf8.ValidString(b.Text) || strings.ContainsFunc(b.Text, unicode.IsControl) {
			writeError(w, http.StatusBadRequest, "bad_request", "the text is too long or has control characters")
			return
		}
	case b.Text == "" && len(b.Keys) >= 1 && len(b.Keys) <= 4:
		for _, k := range b.Keys {
			if !replyKeys[k] {
				writeError(w, http.StatusBadRequest, "bad_request", "key not allowed: "+k)
				return
			}
		}
	default:
		writeError(w, http.StatusBadRequest, "bad_request", "send either keys or text")
		return
	}
	sess, socket := s.herdrSession(w, r)
	if sess == nil {
		return
	}
	s.replyToPane(w, r, sess, socket, pane, b.Digest, b.Keys, b.Text, nil)
}

// answerSettle is how long a pane's turn is held after an answer that may
// still reach it (see replyToPane).
const answerSettle = 3 * time.Second

// replyToPane types an answer (keys, or text and Enter) into a herdr pane if
// its screen is still the one named by digest (and, with still, while it
// says so: checked right before typing), and writes the outcome.
func (s *Server) replyToPane(w http.ResponseWriter, r *http.Request, sess *term.Session, socket, pane, digest string, keys []string, text string, still func() bool) {
	// Bound to the login: signing out (or being signed out) cancels an
	// answer still waiting or connecting, and it is checked once more right
	// before typing.
	expiry, err := s.auth.LoginExpiry(r.Context(), loginHash(r))
	if err != nil {
		writeError(w, http.StatusUnauthorized, "unauthorized", "please sign in")
		return
	}
	ctx, cancel := context.WithDeadline(r.Context(), expiry)
	defer cancel()
	ctx, cancelStep := context.WithTimeout(ctx, 10*time.Second)
	defer cancelStep()
	untrack := s.trackPending(currentUser(r).ID, &liveSocket{loginHash: loginHash(r), hostID: sess.HostID, cancel: cancel})
	defer untrack()
	lock, err := s.replies.lock(ctx, s.convoReplyKey(sess, pane)) // (the pane's, whichever connection: as the conversation's answer)
	if err != nil {
		writeError(w, http.StatusTooManyRequests, "herdr_busy", "an earlier answer to this pane is still under way")
		return
	}
	locked := true
	release := func() {
		if locked {
			locked = false
			s.replies.unlock(lock)
		}
	}
	defer release()
	// (One thing at a time to a pane: a message from the agents page, its
	// keys, or an answer. Two at once would interleave: a message's text, an
	// answer's Enter.)
	turn, ok := convoSending.take(ctx, convoTurnKey(sess.ID, pane))
	if !ok {
		writeError(w, http.StatusTooManyRequests, "herdr_busy", "an earlier answer to this pane is still under way")
		return
	}
	defer func() { turn() }() // (once only: given back below, as soon as it may be)
	reader := herdrDial(w, ctx, sess, socket)
	if reader == nil {
		return
	}
	typer := herdrDial(w, ctx, sess, socket)
	if typer == nil {
		reader.Close()
		return
	}
	defer typer.Close()
	shown, shownDigest, shownCut, err := paneScreenCut(ctx, reader, pane)
	reader.Close()
	if err != nil {
		writeHerdrError(w, err)
		return
	}
	screen := map[string]any{"text": shown, "digest": shownDigest, "cut": shownCut}
	if shownDigest != digest {
		writeErrorExtra(w, http.StatusConflict, "screen_changed", "the screen changed; look again before answering", screen)
		return
	}
	if lock.digest == digest && time.Since(lock.at) < answeredFor {
		writeErrorExtra(w, http.StatusConflict, "already_answered", "this screen was just answered; look again before answering", screen)
		return
	}
	if _, err := s.auth.CheckLogin(ctx, loginHash(r)); err != nil {
		writeError(w, http.StatusUnauthorized, "unauthorized", "please sign in")
		return
	}
	if still != nil && !still() {
		writeError(w, http.StatusGone, "alert_gone", "the agent no longer waits as the alert said")
		return
	}
	if text != "" {
		err = typer.SendInput(ctx, pane, text, []string{"enter"})
	} else {
		err = typer.SendKeys(ctx, pane, keys)
	}
	// Whether it was typed: herdr said so, herdr refused, or the answer was
	// lost on the way (then it may have been typed). Every attempt is
	// audited, also when the browser has gone.
	var apiErr *herdr.APIError
	outcome := "typed"
	switch {
	case errors.As(err, &apiErr):
		outcome = "refused"
	case err != nil:
		outcome = "unknown"
	}
	if outcome != "refused" {
		lock.digest, lock.at = digest, time.Now()
	}
	typer.Close()
	if err == nil {
		turn()
	} else {
		// (Not known to have been typed: closing does not wait for herdr to
		// be done with what reached it. A while, before the pane's next.)
		hold := turn
		turn = func() {}
		time.AfterFunc(answerSettle, hold)
	}
	// (The pane's next answer need not wait for the audit; not known to have
	// been typed, it waits as the pane's turn does, from any connection.)
	if err != nil && locked {
		locked = false // (the deferred release then does nothing: this one gives it back)
		time.AfterFunc(answerSettle, func() { s.replies.unlock(lock) })
	} else {
		release()
	}
	detail := map[string]any{"session": sess.ID, "pane": pane, "outcome": outcome}
	if text != "" {
		detail["text_length"] = utf8.RuneCountInString(text) // what was typed may be private
	} else {
		detail["keys"] = strings.Join(keys, " ")
	}
	actx, acancel := context.WithTimeout(context.WithoutCancel(r.Context()), 5*time.Second)
	defer acancel()
	s.audit(r.WithContext(actx), "herdr.reply", sess.HostName, detail)
	if err != nil {
		writeHerdrError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
