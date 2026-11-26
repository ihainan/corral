package web

import (
	"context"
	"errors"
	"maps"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ihainan/corral/internal/convo"
	"github.com/ihainan/corral/internal/herdr"
	"github.com/ihainan/corral/internal/sshx"
	"github.com/ihainan/corral/internal/term"
)

// Claude Code in-place conversation rewind: HTTP endpoint, immutable herdr
// snapshot reading, cross-connection physical-pane locking, pending rewind
// overlay store (in-memory; lost on gateway restart), and herdr/tmux pane
// drivers that verify full identity before every keypress.

const (
	rewindStoreMax   = 256
	rewindMaxSteps   = 200
	rewindConfirmOpt = "Restore conversation"
)

var (
	claudeRewindStep      = 200 * time.Millisecond
	claudeRewindWait      = 4 * time.Second
	claudeRewindPostWait  = 5 * time.Second
	claudeRewindClearWait = 2500 * time.Millisecond
)

type rewindError struct {
	status int
	code   string
	msg    string
}

func (e *rewindError) Error() string { return e.code + ": " + e.msg }
func rwErr(status int, code, msg string) *rewindError {
	return &rewindError{status: status, code: code, msg: msg}
}

func isIdentityErr(err error) bool {
	var re *rewindError
	if !errors.As(err, &re) {
		return false
	}
	return re.code == "terminal_changed" || re.code == "process_changed" || re.code == "session_changed" || re.code == "not_in_front"
}

func asRewindErr(err error) *rewindError {
	if err == nil {
		return nil
	}
	var re *rewindError
	if errors.As(err, &re) {
		return re
	}
	if errors.Is(err, sshx.ErrBusy) {
		return rwErr(http.StatusTooManyRequests, "busy", "the host is still answering earlier requests")
	}
	return rwErr(http.StatusBadGateway, "host_error", err.Error())
}

// ---------------------------------------------------------------------------
// herdr snapshot reading (immutable copy merging Panes + Agents)
// ---------------------------------------------------------------------------

// herdrPaneSnapshot merges Panes (AgentSession, Tokens, Label) and Agents
// (AgentStatus, TerminalID, StateSeq) for pane into a fresh copy without
// mutating snap or sharing maps/pointers.
func herdrPaneSnapshot(snap *herdr.Snapshot, pane string) (herdr.Pane, bool) {
	if snap == nil || pane == "" {
		return herdr.Pane{}, false
	}
	var out herdr.Pane
	var found bool
	var tokens map[string]string
	merge := func(src herdr.Pane, prefer bool) {
		if !found {
			out.ID, found = src.ID, true
		}
		pick := func(dst *string, v string) {
			if v != "" && (prefer || *dst == "") {
				*dst = v
			}
		}
		pick(&out.TabID, src.TabID)
		pick(&out.WorkspaceID, src.WorkspaceID)
		pick(&out.AgentStatus, src.AgentStatus)
		pick(&out.Agent, src.Agent)
		pick(&out.Name, src.Name)
		pick(&out.Cwd, src.Cwd)
		pick(&out.Label, src.Label)
		pick(&out.Title, src.Title)
		pick(&out.TerminalID, src.TerminalID)
		out.Focused = out.Focused || src.Focused
		out.StateSeq = max(out.StateSeq, src.StateSeq)
		if src.AgentSession != nil && (prefer || out.AgentSession == nil) {
			cp := *src.AgentSession
			out.AgentSession = &cp
		}
		if len(src.Tokens) > 0 {
			if tokens == nil {
				tokens = make(map[string]string, len(src.Tokens))
			}
			for k, v := range src.Tokens {
				if _, exists := tokens[k]; prefer || !exists {
					tokens[k] = v
				}
			}
		}
	}
	for i := range snap.Panes {
		if snap.Panes[i].ID == pane {
			merge(snap.Panes[i], false)
			break
		}
	}
	for i := range snap.Agents {
		if snap.Agents[i].ID == pane {
			merge(snap.Agents[i], true)
			break
		}
	}
	if !found {
		return herdr.Pane{}, false
	}
	out.Tokens = tokens
	return out, true
}

func (h *herdrSockets) paneDetail(id, pane string) (herdr.Pane, bool) {
	snap, ok := h.snapshotOf(id)
	if !ok {
		return herdr.Pane{}, false
	}
	return herdrPaneSnapshot(snap, pane)
}

// herdrValidatedUUID returns a Claude Code UUID reported by herdr, ignoring
// non-UUID hook values (e.g. Devin CLI's "cuddly-dime") and missing values.
func herdrValidatedUUID(p herdr.Pane) string {
	if v := agentSession(p); v != "" && convoUUID.MatchString(v) {
		return v
	}
	return ""
}

// ---------------------------------------------------------------------------
// Pending rewind record store (in-memory; lost on gateway restart)
// ---------------------------------------------------------------------------

type rewindKey struct {
	paneKey string
	sid     string
	pid     int64
	start   int64
}

type rewindMark struct {
	targetID   string
	knownUsers map[string]bool
	at         time.Time
}

type rewindStore struct {
	mu sync.Mutex
	m  map[rewindKey]*rewindMark
}

func (r *rewindStore) set(key rewindKey, targetID string, knownUsers map[string]bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.m == nil {
		r.m = make(map[rewindKey]*rewindMark)
	}
	if _, exists := r.m[key]; !exists && len(r.m) >= rewindStoreMax {
		var oldKey rewindKey
		var oldAt time.Time
		for k, v := range r.m {
			if oldAt.IsZero() || v.at.Before(oldAt) {
				oldKey, oldAt = k, v.at
			}
		}
		delete(r.m, oldKey)
	}
	r.m[key] = &rewindMark{targetID: targetID, knownUsers: maps.Clone(knownUsers), at: time.Now()}
}

func (r *rewindStore) get(key rewindKey) *rewindMark {
	r.mu.Lock()
	defer r.mu.Unlock()
	m := r.m[key]
	if m == nil {
		return nil
	}
	return &rewindMark{targetID: m.targetID, knownUsers: maps.Clone(m.knownUsers), at: m.at}
}

func (r *rewindStore) delete(key rewindKey) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.m, key)
}

func (s *Server) syncClaudeRewind(sess *term.Session, st *convoState) string {
	if st.agent != "claude" || st.sub || st.bind == nil {
		if st.rewindTarget != "" {
			st.rewindTarget = ""
			st.gen = convoGens.Add(1)
		}
		return ""
	}
	cl, ok := st.parser.(*convo.Claude)
	if !ok {
		return ""
	}
	key := rewindKey{paneKey: s.convoReplyKey(sess, st.bind.Pane), sid: st.bind.Session, pid: st.bind.PID, start: st.bind.Start}
	mark := s.rewinds.get(key)
	if mark != nil && cl.RewindSettled(mark.targetID, mark.knownUsers) {
		s.rewinds.delete(key)
		mark = nil
	}
	want := ""
	if mark != nil {
		want = mark.targetID
	}
	if want != st.rewindTarget {
		st.rewindTarget = want
		st.gen = convoGens.Add(1)
	}
	return want
}

func overlayRewindEntries(st *convoState, targetID string, entries []convo.Entry) []convo.Entry {
	if targetID == "" || len(entries) == 0 {
		return entries
	}
	cl, ok := st.parser.(*convo.Claude)
	if !ok {
		return entries
	}
	targetSeq, found := cl.SeqOf(targetID)
	if !found && !st.older {
		return entries
	}
	out := append([]convo.Entry(nil), entries...)
	for i := range out {
		if !found || out[i].Seq >= targetSeq {
			out[i].Gone = true
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Remote shell scripts & real pane implementations (herdr + tmux)
// ---------------------------------------------------------------------------

// claudeRewindCheckSh verifies full identity on host right before key delivery.
// Exit codes: 31=process_changed, 32=not_in_front, 33=session_changed, 34=terminal_changed.
func claudeRewindCheckSh(mode, pane, socket, tmuxPanePID string, pid, start int64, sid string) string {
	q := sshx.QuoteShell
	var b strings.Builder
	b.WriteString(`PATH="$HOME/.local/bin:$HOME/bin:$HOME/.cargo/bin:/opt/homebrew/bin:/usr/local/bin:$PATH"` + "\n")
	b.WriteString("pid=" + strconv.FormatInt(pid, 10) + " start=" + strconv.FormatInt(start, 10) + " sid=" + q(sid) + " pane=" + q(pane) + "\n")
	if mode == "tmux" {
		b.WriteString("tpp=" + q(tmuxPanePID) + "\n")
		b.WriteString(`[ "$(tmux -u display-message -p -t "$pane" '#{pane_pid} #{pane_dead}' 2>/dev/null 9>&-)" = "$tpp 0" ] || exit 34` + "\n")
		b.WriteString(`tty=$(tmux -u display-message -p -t "$pane" '#{pane_tty}' 2>/dev/null 9>&-)` + "\n")
		b.WriteString(`[ -n "$tty" ] && [ "$(readlink "/proc/$pid/fd/0" 2>/dev/null)" = "$tty" ] || exit 31` + "\n")
	} else {
		b.WriteString("sock=" + q(socket) + "\n")
		b.WriteString(`tr '\0\n' '\n\001' <"/proc/$pid/environ" 2>/dev/null | grep -qxF "HERDR_PANE_ID=$pane" || exit 31` + "\n")
		b.WriteString(`tr '\0\n' '\n\001' <"/proc/$pid/environ" 2>/dev/null | grep -qxF "HERDR_SOCKET_PATH=$sock" || exit 31` + "\n")
	}
	b.WriteString(`stat=$(tr '\n' ' ' <"/proc/$pid/stat" 2>/dev/null) || exit 31` + "\n")
	b.WriteString(`rest=${stat##*) }` + "\n")
	b.WriteString(`[ "$(printf '%s' "$rest" | cut -d' ' -f20)" = "$start" ] || exit 31` + "\n")
	b.WriteString(`pgrp=$(printf '%s' "$rest" | cut -d' ' -f3); tpgid=$(printf '%s' "$rest" | cut -d' ' -f6)` + "\n")
	b.WriteString(`[ -n "$pgrp" ] && [ "$pgrp" = "$tpgid" ] || exit 32` + "\n")
	b.WriteString(`c=$(head -c 65536 "$HOME/.claude/sessions/$pid.json" 2>/dev/null | tr -d '\n')` + "\n")
	b.WriteString(`ps=$(printf '%s' "$c" | grep -o '"procStart" *: *"*[0-9]*"*' | head -n 1 | tr -dc '0-9')` + "\n")
	b.WriteString(`[ "$ps" = "$start" ] || exit 31` + "\n")
	b.WriteString(`csid=$(printf '%s' "$c" | grep -o '"sessionId" *: *"[0-9a-fA-F-]*"' | head -n 1 | sed 's/.*"\([0-9a-fA-F-]*\)"$/\1/')` + "\n")
	b.WriteString(`[ "$csid" = "$sid" ] || exit 33` + "\n")
	return b.String()
}

var claudeRewindAllowedKeys = map[string]string{
	"esc": "Escape", "up": "Up", "down": "Down", "enter": "Enter", "ctrl+c": "C-c",
}

func claudeRewindPressSh(mode, pane, socket, tmuxPanePID string, pid, start int64, sid string, keys []string) (string, error) {
	if len(keys) == 0 {
		return "", errors.New("no keys")
	}
	q := sshx.QuoteShell
	var b strings.Builder
	b.WriteString(claudeRewindCheckSh(mode, pane, socket, tmuxPanePID, pid, start, sid))
	for i, k := range keys {
		tk, ok := claudeRewindAllowedKeys[k]
		if !ok {
			return "", errors.New("invalid rewind key: " + k)
		}
		if i > 0 && k == "esc" && keys[i-1] == "esc" {
			b.WriteString("sleep 0.05\n")
		}
		if mode == "tmux" {
			b.WriteString(`tmux send-keys -t "$pane" ` + q(tk) + ` 9>&- || exit 6` + "\n")
		} else {
			b.WriteString(`HERDR_SOCKET_PATH="$sock" herdr pane send-keys "$pane" ` + q(k) + ` >/dev/null 2>&1 9>&- || exit 6` + "\n")
		}
	}
	b.WriteString("echo ok\n")
	return b.String(), nil
}

func claudeRewindTmuxPaneSh(pane string) string {
	return `tmux -u display-message -p -t ` + sshx.QuoteShell(pane) + ` '#{pane_pid} #{pane_dead} #{pane_tty}' 2>/dev/null 9>&-`
}

func mapRewindExitErr(err error) error {
	var exitErr *sshx.ExitError
	if errors.As(err, &exitErr) {
		switch exitErr.Status {
		case 31:
			return rwErr(http.StatusConflict, "process_changed", "the Claude Code process in this pane changed")
		case 32:
			return rwErr(http.StatusConflict, "not_in_front", "Claude Code is not in front in its pane")
		case 33:
			return rwErr(http.StatusConflict, "session_changed", "the Claude Code session in this pane changed")
		case 34:
			return rwErr(http.StatusConflict, "terminal_changed", "the terminal in this pane changed")
		}
	}
	return err
}

func claudeRewindExec(ctx context.Context, sess *term.Session, wg *sync.WaitGroup, cmd string, limit int, await time.Duration) ([]byte, error) {
	wg.Add(1)
	res, err := sshx.Exec(ctx, sess.Client(), cmd, limit, sshx.ExecOptions{AwaitSlot: await, Release: wg.Done})
	return res.Out, err
}

type herdrClaudeRewindPane struct {
	sess                     *term.Session
	wg                       *sync.WaitGroup
	socket, pane, terminalID string
	pid, start               int64
	sid                      string
}

func (p *herdrClaudeRewindPane) checkHerdrSnapshot(ctx context.Context) error {
	c, err := herdr.DialWait(ctx, p.sess.Client(), p.socket, 5*time.Second)
	if err != nil {
		return err
	}
	defer c.Close()
	snap, err := c.Snapshot(ctx)
	if err != nil {
		return err
	}
	info, ok := herdrPaneSnapshot(snap, p.pane)
	if !ok || (p.terminalID != "" && info.TerminalID != p.terminalID) {
		return rwErr(http.StatusConflict, "terminal_changed", "the terminal in this pane changed")
	}
	if hsid := herdrValidatedUUID(info); hsid != "" && !strings.EqualFold(hsid, p.sid) {
		return rwErr(http.StatusConflict, "session_changed", "herdr reports a different session in this pane")
	}
	return nil
}

func (p *herdrClaudeRewindPane) verify(ctx context.Context) error {
	if err := p.checkHerdrSnapshot(ctx); err != nil {
		return err
	}
	_, err := claudeRewindExec(ctx, p.sess, p.wg, "sh -c "+sshx.QuoteShell(claudeRewindCheckSh("herdr", p.pane, p.socket, "", p.pid, p.start, p.sid)), 128, 3*time.Second)
	return mapRewindExitErr(err)
}

func (p *herdrClaudeRewindPane) press(ctx context.Context, keys ...string) error {
	if err := p.checkHerdrSnapshot(ctx); err != nil {
		return err
	}
	sh, err := claudeRewindPressSh("herdr", p.pane, p.socket, "", p.pid, p.start, p.sid, keys)
	if err != nil {
		return err
	}
	out, err := claudeRewindExec(ctx, p.sess, p.wg, "sh -c "+sshx.QuoteShell(sh), 128, 0)
	if err != nil {
		return mapRewindExitErr(err)
	}
	if strings.TrimSpace(string(out)) != "ok" {
		return errors.New("key delivery unconfirmed")
	}
	return nil
}

func (p *herdrClaudeRewindPane) screen(ctx context.Context) (string, error) {
	c, err := herdr.DialWait(ctx, p.sess.Client(), p.socket, 5*time.Second)
	if err != nil {
		return "", err
	}
	defer c.Close()
	return c.ReadPaneSource(ctx, p.pane, "visible", 0)
}

func (p *herdrClaudeRewindPane) colorScreen(ctx context.Context) (string, error) {
	c, err := herdr.DialWait(ctx, p.sess.Client(), p.socket, 5*time.Second)
	if err != nil {
		return "", err
	}
	defer c.Close()
	return c.ReadPaneANSI(ctx, p.pane, "visible", 0)
}

type tmuxClaudeRewindPane struct {
	sess          *term.Session
	wg            *sync.WaitGroup
	pane, panePID string
	pid, start    int64
	sid           string
}

func (p *tmuxClaudeRewindPane) verify(ctx context.Context) error {
	_, err := claudeRewindExec(ctx, p.sess, p.wg, "sh -c "+sshx.QuoteShell(claudeRewindCheckSh("tmux", p.pane, "", p.panePID, p.pid, p.start, p.sid)), 128, 3*time.Second)
	return mapRewindExitErr(err)
}

func (p *tmuxClaudeRewindPane) press(ctx context.Context, keys ...string) error {
	sh, err := claudeRewindPressSh("tmux", p.pane, "", p.panePID, p.pid, p.start, p.sid, keys)
	if err != nil {
		return err
	}
	out, err := claudeRewindExec(ctx, p.sess, p.wg, "sh -c "+sshx.QuoteShell(sh), 128, 0)
	if err != nil {
		return mapRewindExitErr(err)
	}
	if strings.TrimSpace(string(out)) != "ok" {
		return errors.New("key delivery unconfirmed")
	}
	return nil
}

func (p *tmuxClaudeRewindPane) screen(ctx context.Context) (string, error) {
	sh := claudeRewindCheckSh("tmux", p.pane, "", p.panePID, p.pid, p.start, p.sid) + `tmux -u capture-pane -p -t "$pane" 2>/dev/null 9>&-`
	out, err := claudeRewindExec(ctx, p.sess, p.wg, "sh -c "+sshx.QuoteShell(sh), 256<<10, 5*time.Second)
	return string(out), mapRewindExitErr(err)
}

func (p *tmuxClaudeRewindPane) colorScreen(ctx context.Context) (string, error) {
	sh := claudeRewindCheckSh("tmux", p.pane, "", p.panePID, p.pid, p.start, p.sid) + `tmux -u capture-pane -p -e -t "$pane" 2>/dev/null 9>&-`
	out, err := claudeRewindExec(ctx, p.sess, p.wg, "sh -c "+sshx.QuoteShell(sh), 512<<10, 5*time.Second)
	return string(out), mapRewindExitErr(err)
}

// ---------------------------------------------------------------------------
// HTTP handler: POST /api/sessions/{id}/conversation/rewind
// ---------------------------------------------------------------------------

func (s *Server) apiConversationRewind(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.runningSession(w, r)
	if !ok {
		return
	}
	var b struct {
		Pane       string `json:"pane"`
		PID        int64  `json:"pid"`
		Start      int64  `json:"start"`
		Session    string `json:"session"`
		TerminalID string `json:"terminal_id"`
		MessageID  string `json:"message_id"`
	}
	if err := readJSON(r, &b); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid request body")
		return
	}
	if sess.Persistence != "herdr" && sess.Persistence != "tmux" {
		writeError(w, http.StatusConflict, "unsupported", "only in tmux and herdr sessions")
		return
	}
	if b.PID <= 0 || b.Start <= 0 || !convoUUID.MatchString(b.Session) || !convoUUID.MatchString(b.MessageID) ||
		(sess.Persistence == "herdr" && !herdrPane.MatchString(b.Pane)) ||
		(sess.Persistence == "tmux" && !tmuxPane.MatchString(b.Pane)) {
		writeError(w, http.StatusBadRequest, "bad_request", "valid pane, process, session and message_id are required")
		return
	}
	outcome, reason := "refused", ""
	defer func() {
		if reason == "terminal_changed" || reason == "process_changed" || reason == "session_changed" {
			s.convoRelocNow(sess, b.Pane)
		}
		actx, acancel := context.WithTimeout(context.WithoutCancel(r.Context()), 5*time.Second)
		defer acancel()
		detail := map[string]any{"session": sess.ID, "pane": b.Pane, "pid": b.PID, "start": b.Start, "claude_session": b.Session, "message_id": b.MessageID, "outcome": outcome}
		if reason != "" {
			detail["reason"] = reason
		}
		s.audit(r.WithContext(actx), "convo.rewind", sess.HostName, detail)
	}()

	expiry, err := s.auth.LoginExpiry(r.Context(), loginHash(r))
	if err != nil {
		reason = "signed_out"
		writeError(w, http.StatusUnauthorized, "unauthorized", "please sign in")
		return
	}
	ctx, cancel := context.WithDeadline(r.Context(), expiry)
	defer cancel()
	ctx, cancelStep := context.WithTimeout(ctx, 45*time.Second)
	defer cancelStep()
	untrack := s.trackPending(currentUser(r).ID, &liveSocket{loginHash: loginHash(r), hostID: sess.HostID, cancel: cancel})
	defer untrack()

	paneKey := s.convoReplyKey(sess, b.Pane)
	lock, err := s.replies.lock(ctx, paneKey)
	if err != nil {
		reason = "busy"
		writeError(w, http.StatusTooManyRequests, "busy", "another operation on this pane is still under way")
		return
	}
	turn, ok := convoSending.take(ctx, convoTurnKey(sess.ID, b.Pane))
	if !ok {
		s.replies.unlock(lock)
		reason = "busy"
		writeError(w, http.StatusTooManyRequests, "busy", "another operation on this pane is still under way")
		return
	}
	var running sync.WaitGroup
	defer func() {
		go func() {
			running.Wait()
			turn()
			s.replies.unlock(lock)
		}()
	}()
	fail := func(re *rewindError) {
		reason = re.code
		writeError(w, re.status, re.code, re.msg)
	}

	// Verify physical pane (herdr live snapshot or tmux pane_pid).
	socket := s.herdrSocks.get(sess.ID)
	var liveTermID, liveHerdrSID, liveHerdrStatus, tmuxPanePID string
	if sess.Persistence == "herdr" {
		if socket == "" {
			fail(rwErr(http.StatusConflict, "terminal_changed", "herdr socket is not available"))
			return
		}
		hc, derr := herdr.DialWait(ctx, sess.Client(), socket, 5*time.Second)
		if derr != nil {
			fail(asRewindErr(derr))
			return
		}
		snap, serr := hc.Snapshot(ctx)
		hc.Close()
		if serr != nil {
			fail(asRewindErr(serr))
			return
		}
		info, found := herdrPaneSnapshot(snap, b.Pane)
		if !found || (b.TerminalID != "" && info.TerminalID != b.TerminalID) {
			fail(rwErr(http.StatusConflict, "terminal_changed", "the terminal in this pane changed"))
			return
		}
		liveTermID, liveHerdrSID, liveHerdrStatus = info.TerminalID, herdrValidatedUUID(info), info.AgentStatus
	} else {
		out, terr := claudeRewindExec(ctx, sess, &running, claudeRewindTmuxPaneSh(b.Pane), 512, 3*time.Second)
		if terr != nil {
			fail(asRewindErr(terr))
			return
		}
		f := strings.Fields(string(out))
		if len(f) < 2 || !isDigits(f[0]) || f[1] != "0" {
			fail(rwErr(http.StatusConflict, "terminal_changed", "tmux pane no longer exists or is dead"))
			return
		}
		tmuxPanePID = f[0]
	}

	// Locate real Claude Code process via /proc scan and verify identity & idle state.
	tmuxTarget := ""
	if sess.Persistence == "tmux" {
		tmuxTarget = b.Pane
	}
	locOut, lerr := claudeRewindExec(ctx, sess, &running, convoLocateCommand(sess.Persistence, sess.TmuxSession, tmuxTarget), convoLocateMax, 3*time.Second)
	if lerr != nil {
		fail(asRewindErr(lerr))
		return
	}
	located := parseConvoLocate(string(locOut))
	front := pickConvoProc(located.procs, sess.Persistence, b.Pane, socket, located.tmuxPID, located.paneTTY, liveHerdrSID)
	switch {
	case front == nil || front.pid != b.PID || front.start != b.Start:
		fail(rwErr(http.StatusConflict, "process_changed", "the Claude Code process in this pane changed"))
		return
	case front.agent != "claude":
		fail(rwErr(http.StatusConflict, "unsupported", "rewind is only supported for Claude Code"))
		return
	case front.sid != b.Session || (liveHerdrSID != "" && !strings.EqualFold(liveHerdrSID, b.Session)):
		fail(rwErr(http.StatusConflict, "session_changed", "the Claude Code session in this pane changed"))
		return
	case !front.fg:
		fail(rwErr(http.StatusConflict, "not_in_front", "Claude Code is not in front in its pane"))
		return
	case liveHerdrStatus == herdr.StatusWorking || liveHerdrStatus == herdr.StatusBlocked:
		fail(rwErr(http.StatusConflict, "agent_busy", "the agent is busy"))
		return
	}
	if status := s.convoFreshStatus(ctx, sess, &convoBind{PID: front.pid, Start: front.start, Session: front.sid}); status != "" && status != "idle" {
		fail(rwErr(http.StatusConflict, "agent_busy", "the agent is busy or waiting for input"))
		return
	}

	// Refresh log state and build rewind plan.
	stateKey := sess.ID + "\x00"
	if sess.Persistence == "herdr" {
		stateKey += b.Pane
	}
	st := s.convos.get(stateKey)
	defer st.done()
	st.mu.Lock()
	if st.sid != front.sid || st.log != front.log || st.agent != "claude" {
		st.bind = &convoBind{PID: front.pid, Start: front.start, Session: front.sid, Agent: "claude", Pane: b.Pane, TerminalID: liveTermID}
		st.adopt(front.sid, front.log, "claude", "")
	}
	_, rerr := s.readConvo(sess, st)
	if rerr != nil {
		st.mu.Unlock()
		fail(asRewindErr(rerr))
		return
	}
	cl, ok := st.parser.(*convo.Claude)
	if !ok {
		st.mu.Unlock()
		fail(rwErr(http.StatusConflict, "unsupported", "rewind is only supported for Claude Code"))
		return
	}
	allEntries := overlayRewindEntries(st, s.syncClaudeRewind(sess, st), cl.Since(0))
	knownUsers := cl.UserIDs()
	st.mu.Unlock()

	plan, perr := buildClaudeRewindPlan(allEntries, b.MessageID)
	if perr != nil {
		fail(perr)
		return
	}

	var pane claudeRewindPane
	if sess.Persistence == "herdr" {
		pane = &herdrClaudeRewindPane{sess: sess, wg: &running, socket: socket, pane: b.Pane, terminalID: liveTermID, pid: front.pid, start: front.start, sid: front.sid}
	} else {
		pane = &tmuxClaudeRewindPane{sess: sess, wg: &running, pane: b.Pane, panePID: tmuxPanePID, pid: front.pid, start: front.start, sid: front.sid}
	}
	res, rwerr := claudeRewind(ctx, pane, plan)
	if rwerr != nil {
		if rwerr.code == "rewind_uncertain" {
			outcome = "unknown"
		}
		fail(rwerr)
		return
	}
	s.rewinds.set(rewindKey{paneKey: paneKey, sid: front.sid, pid: front.pid, start: front.start}, b.MessageID, knownUsers)
	outcome = "rewound"
	if res.Warning != "" {
		reason = res.Warning
	}
	writeJSON(w, http.StatusOK, res)
}
