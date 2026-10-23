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
	// Since is when its status last changed, as this gateway saw it (unix
	// milliseconds): when it was first seen, for one that has not changed
	// since the gateway began watching.
	Since int64 `json:"since,omitempty"`
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
}

// agentTimes holds when each pane's agent last changed status, as seen by
// one watcher (see herdrAgent.Since).
type agentTimes struct {
	mu sync.Mutex
	at map[string]agentTime
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
func (a *agentTimes) of(pane, status string) int64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	r, ok := a.at[pane]
	if !ok || r.status != status {
		r = agentTime{status, time.Now()}
		a.at[pane] = r
	}
	return r.t.UnixMilli()
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
	// (Where it is, not only which record: a record edited to another machine is another server.)
	k := fmt.Sprint(host.ID, "\x00", host.Username, "@", host.Address, ":", host.Port, "\x00", socket)
	t := h.m[k]
	if t == nil {
		t = &agentTimes{at: map[string]agentTime{}}
		h.m[k] = t
	}
	return t
}

func summarize(session string, s *herdr.Snapshot, times *agentTimes) herdrSummary {
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
			herdrAgent{PaneID: p.ID, Agent: p.Agent, Status: p.AgentStatus, Title: truncate(p.Title, 120), Cwd: p.Cwd, Since: times.of(p.ID, p.AgentStatus)})
		if p.AgentStatus == herdr.StatusBlocked {
			out.Waiting++
		}
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

// herdrSockets remembers which herdr socket each terminal session is watching.
type herdrSockets struct {
	mu   sync.Mutex
	path map[string]string
}

func (h *herdrSockets) set(id, path string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if path == "" {
		delete(h.path, id)
	} else {
		h.path[id] = path
	}
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
	w := herdr.NewWatcher(sess.Client(), socket,
		func(snap *herdr.Snapshot) {
			if b, err := json.Marshal(summarize(name, snap, times)); err == nil {
				sess.PublishMeta(b)
			}
		},
		// (A change is told before the snapshot that has it.)
		func(tr herdr.Transition) {
			times.changed(tr.Pane.ID, tr.To)
			s.onAgentTransition(sess, h, tr)
		},
	)
	// The browser holds the last summary as current until told otherwise;
	// it keeps it (a page opened meanwhile gets it too), marked as old.
	w.OnLost = func() {
		snap := w.Latest()
		if snap == nil {
			return
		}
		sum := summarize(name, snap, times)
		sum.Lost = true
		if b, err := json.Marshal(sum); err == nil {
			sess.PublishMeta(b)
		}
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
	maxScreenBytes = 16 << 10
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
	text, err = c.ReadPane(ctx, pane, 0)
	if err != nil {
		return "", "", err
	}
	text = strings.TrimRight(strings.ReplaceAll(text, "\r\n", "\n"), " \t\n")
	sum := sha256.Sum256([]byte(text))
	if len(text) > maxScreenBytes {
		cut := len(text) - maxScreenBytes
		for cut < len(text) && !utf8.RuneStart(text[cut]) {
			cut++
		}
		text = text[cut:]
	}
	return text, hex.EncodeToString(sum[:]), nil
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
	text, digest, err := paneScreen(ctx, c, pane)
	if err != nil {
		writeHerdrError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{"text": text, "digest": digest})
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
	lock, err := s.replies.lock(ctx, sess.ID+"\x00"+pane)
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
	shown, shownDigest, err := paneScreen(ctx, reader, pane)
	reader.Close()
	if err != nil {
		writeHerdrError(w, err)
		return
	}
	screen := map[string]any{"text": shown, "digest": shownDigest}
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
	release() // the pane's next answer need not wait for the audit
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
