package web

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"

	"github.com/ihainan/corral/internal/corrald/gw"
	"github.com/ihainan/corral/internal/sftplite"
	"github.com/ihainan/corral/internal/sshx"
	"github.com/ihainan/corral/internal/store"
)

// CorraldHintReason identifies a real, observable failure category that
// enabling the host helper daemon (corrald) directly resolves.
type CorraldHintReason string

const (
	// HintReasonSSHSessionFull indicates an SSH status or log read failed
	// because SSH session/channel slots on the connection were exhausted
	// (either the gateway's per-connection session quota or the host's
	// OpenSSH MaxSessions / channel-open refusal).
	HintReasonSSHSessionFull CorraldHintReason = "ssh_session_full"
)

// CorraldHintOp identifies which read path(s) hit the failure on a host.
type CorraldHintOp string

const (
	HintOpStatus CorraldHintOp = "status"
	HintOpLog    CorraldHintOp = "log"
	HintOpBoth   CorraldHintOp = "both"
)

// ClassifyCorraldSolvableError determines whether err is a real, reliable
// failure that enabling corrald can solve (currently: SSH session/channel
// quota exhaustion on status or log reads).
//
// Timeouts, cancellations, remote command non-zero exits, file/SFTP errors,
// authentication/host-key/jump-host errors, and generic network drops return
// ("", false) so the gateway never guesses from timeouts or unrelated errors.
func ClassifyCorraldSolvableError(err error) (CorraldHintReason, bool) {
	if err == nil {
		return "", false
	}
	// Never guess from timeouts or cancellations.
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) || errors.Is(err, errFileTimeout) {
		return "", false
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return "", false
	}
	// Jump host dial errors are direct-tcpip forwarding failures, not target
	// host session exhaustion.
	var jumpErr *sshx.JumpError
	if errors.As(err, &jumpErr) {
		return "", false
	}
	// Command ran on an opened channel, or auth/file-level errors: not session cap.
	var exitErr *sshx.ExitError
	if errors.As(err, &exitErr) {
		return "", false
	}
	var hkErr *sshx.HostKeyChangedError
	if errors.As(err, &hkErr) {
		return "", false
	}
	if errors.Is(err, sshx.ErrOutputTooLarge) || errors.Is(err, sshx.ErrAuthFailed) ||
		errors.Is(err, fs.ErrNotExist) || errors.Is(err, fs.ErrPermission) {
		return "", false
	}
	var sftpErr *sftp.StatusError
	if errors.As(err, &sftpErr) {
		return "", false
	}
	var liteErr *sftplite.StatusError
	if errors.As(err, &liteErr) {
		return "", false
	}

	// 1. Local SSH session/channel ledger full.
	if errors.Is(err, sshx.ErrBusy) {
		return HintReasonSSHSessionFull, true
	}

	// 2. Remote SSH server rejected opening a session channel (e.g. OpenSSH MaxSessions).
	var openErr *ssh.OpenChannelError
	if errors.As(err, &openErr) {
		switch openErr.Reason {
		case ssh.ResourceShortage:
			return HintReasonSSHSessionFull, true
		case ssh.Prohibited:
			if strings.Contains(strings.ToLower(openErr.Message), "forwarding") {
				return "", false
			}
			return HintReasonSSHSessionFull, true
		default:
			return "", false
		}
	}

	// 3. Stringified SSH channel-open rejection messages (without matching
	//    unrelated file "open failed" errors or port-forwarding rejections).
	msg := strings.ToLower(err.Error())
	if strings.Contains(msg, "forwarding") {
		return "", false
	}
	switch {
	case strings.Contains(msg, "maxsessions"),
		strings.Contains(msg, "no more sessions"),
		strings.Contains(msg, "too many sessions"),
		strings.Contains(msg, "administratively prohibited"),
		strings.Contains(msg, "resource shortage"),
		strings.Contains(msg, "channel open failed"),
		strings.Contains(msg, "ssh: rejected:") && strings.Contains(msg, "open failed"):
		return HintReasonSSHSessionFull, true
	}

	return "", false
}

const maxCorraldHintEntries = 4096

type corraldHintKey struct {
	userID int64
	hostID int64
}

type corraldHostProblem struct {
	Reason CorraldHintReason
	Op     CorraldHintOp
	Count  int
	LastAt time.Time
}

type corraldHostProblemEntry struct {
	HostID int64
	Prob   corraldHostProblem
}

// corraldHintTracker holds in-memory counts of recent corrald-solvable host
// failures per (user, host). Intentionally ephemeral: never writes to SQLite on
// failure and resets cleanly on gateway restart.
type corraldHintTracker struct {
	mu      sync.Mutex
	entries map[corraldHintKey]corraldHostProblem
	now     func() time.Time
}

func (t *corraldHintTracker) clock() time.Time {
	if t != nil && t.now != nil {
		return t.now()
	}
	return time.Now()
}

func mergeCorraldHintOp(prev, next CorraldHintOp) CorraldHintOp {
	if next == "" {
		next = HintOpStatus
	}
	if prev == "" || prev == next {
		return next
	}
	return HintOpBoth
}

func (t *corraldHintTracker) record(userID, hostID int64, op CorraldHintOp, err error) bool {
	if t == nil || userID <= 0 || hostID <= 0 {
		return false
	}
	reason, ok := ClassifyCorraldSolvableError(err)
	if !ok {
		return false
	}
	now := t.clock()
	key := corraldHintKey{userID: userID, hostID: hostID}

	t.mu.Lock()
	defer t.mu.Unlock()
	if t.entries == nil {
		t.entries = make(map[corraldHintKey]corraldHostProblem)
	}
	cur, exists := t.entries[key]
	if !exists && len(t.entries) >= maxCorraldHintEntries {
		var oldestKey corraldHintKey
		var oldestAt time.Time
		first := true
		for k, v := range t.entries {
			if first || v.LastAt.Before(oldestAt) {
				oldestKey, oldestAt, first = k, v.LastAt, false
			}
		}
		if !first {
			delete(t.entries, oldestKey)
		}
	}
	cur.Reason = reason
	cur.Op = mergeCorraldHintOp(cur.Op, op)
	cur.Count++
	cur.LastAt = now
	t.entries[key] = cur
	return true
}

func (t *corraldHintTracker) get(userID, hostID int64) (corraldHostProblem, bool) {
	if t == nil || userID <= 0 || hostID <= 0 {
		return corraldHostProblem{}, false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	p, ok := t.entries[corraldHintKey{userID: userID, hostID: hostID}]
	return p, ok
}

func (t *corraldHintTracker) forUser(userID int64) []corraldHostProblemEntry {
	if t == nil || userID <= 0 {
		return nil
	}
	t.mu.Lock()
	var out []corraldHostProblemEntry
	for k, v := range t.entries {
		if k.userID == userID && v.Count > 0 {
			out = append(out, corraldHostProblemEntry{HostID: k.hostID, Prob: v})
		}
	}
	t.mu.Unlock()
	sort.Slice(out, func(i, j int) bool {
		if !out[i].Prob.LastAt.Equal(out[j].Prob.LastAt) {
			return out[i].Prob.LastAt.After(out[j].Prob.LastAt)
		}
		return out[i].HostID < out[j].HostID
	})
	return out
}

func (t *corraldHintTracker) forgetHost(userID, hostID int64) {
	if t == nil || userID <= 0 || hostID <= 0 {
		return
	}
	t.mu.Lock()
	delete(t.entries, corraldHintKey{userID: userID, hostID: hostID})
	t.mu.Unlock()
}

func (t *corraldHintTracker) forgetUser(userID int64) {
	if t == nil || userID <= 0 {
		return
	}
	t.mu.Lock()
	for k := range t.entries {
		if k.userID == userID {
			delete(t.entries, k)
		}
	}
	t.mu.Unlock()
}

// recordCorraldHintError records a corrald-solvable SSH failure in memory for
// (userID, hostID). Safe to call on nil Server or zero IDs.
func (s *Server) recordCorraldHintError(userID, hostID int64, op CorraldHintOp, err error) bool {
	if s == nil {
		return false
	}
	return s.corraldHints.record(userID, hostID, op, err)
}

// CorraldHintItem is the gateway's decision payload for the frontend contextual
// hint banner above the agent list.
type CorraldHintItem struct {
	HostID   int64             `json:"host_id"`
	HostName string            `json:"host_name"`
	Reason   CorraldHintReason `json:"reason"`
	Op       CorraldHintOp     `json:"op"`
	Count    int               `json:"count"`
	LastAt   int64             `json:"last_at"`
}

// corraldHintsEnabled reads the user's account-level master switch from
// user_settings ("corraldHints", default true when absent).
func (s *Server) corraldHintsEnabled(ctx context.Context, userID int64) bool {
	if s == nil || s.store == nil || userID <= 0 {
		return false
	}
	raw, err := s.store.UserSettings(ctx, userID)
	if err != nil {
		return true
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal([]byte(raw), &obj) != nil || obj == nil {
		return true
	}
	val, ok := obj["corraldHints"]
	if !ok {
		return true
	}
	var enabled bool
	if json.Unmarshal(val, &enabled) != nil {
		return true
	}
	return enabled
}

// corraldHostStatusEligible reports whether st represents an unenabled host
// where the user can currently enable corrald.
func corraldHostStatusEligible(st *gw.StatusResponse) bool {
	if st == nil || st.AdminDisabled {
		return false
	}
	if st.State != gw.StateOff {
		return false
	}
	if st.Op != nil && (st.Op.State == gw.OpPending || st.Op.State == gw.OpRunning) {
		return false
	}
	if st.Error != nil && (st.Error.Code == "unsupported_os" || st.Error.Code == "unsupported_arch") {
		return false
	}
	if st.Op != nil && st.Op.Kind == gw.OpInstall && st.Op.State == gw.OpFailed &&
		(st.Op.ErrorCode == "unsupported_os" || st.Op.ErrorCode == "unsupported_arch") {
		return false
	}
	return true
}

// evaluateCorraldHostHint is the single centralized rule determining whether a
// contextual corrald hint should be shown for (userID, hostID):
//  1. A real corrald-solvable problem was observed on this host.
//  2. The account-level master switch ("corraldHints") is enabled.
//  3. Corrald is available on this gateway and not disabled by the admin.
//  4. This host has not already been shown or dismissed for this user.
//  5. The host exists, belongs to userID, and is currently unenabled & enableable
//     (StateOff, no active op, not admin-disabled, not rebind/incompatible).
func (s *Server) evaluateCorraldHostHint(
	ctx context.Context,
	userID, hostID int64,
	prob corraldHostProblem,
	hintRow *store.CorraldHostHint,
	masterEnabled bool,
) (*CorraldHintItem, bool) {
	if prob.Count <= 0 || prob.Reason == "" {
		return nil, false
	}
	if !masterEnabled {
		return nil, false
	}
	if s == nil || s.corrald == nil || !s.corrald.Available() || s.corraldAdminDisabled() {
		return nil, false
	}
	if hintRow != nil && (hintRow.ShownAt > 0 || hintRow.DismissedAt > 0) {
		return nil, false
	}
	h, err := s.store.Host(ctx, userID, hostID)
	if err != nil || h == nil {
		return nil, false
	}
	st, err := s.corrald.Status(ctx, userID, hostID)
	if err != nil || !corraldHostStatusEligible(st) {
		return nil, false
	}
	op := prob.Op
	if op == "" {
		op = HintOpStatus
	}
	return &CorraldHintItem{
		HostID:   h.ID,
		HostName: h.Name,
		Reason:   prob.Reason,
		Op:       op,
		Count:    prob.Count,
		LastAt:   prob.LastAt.Unix(),
	}, true
}

// selectCorraldHint returns the single most recently affected eligible host
// hint for userID (or for filterHostID when > 0).
func (s *Server) selectCorraldHint(ctx context.Context, userID, filterHostID int64) (*CorraldHintItem, error) {
	if filterHostID > 0 {
		if _, err := s.store.Host(ctx, userID, filterHostID); err != nil {
			return nil, err
		}
	}
	if s.corrald == nil || !s.corrald.Available() || s.corraldAdminDisabled() {
		return nil, nil
	}
	masterEnabled := s.corraldHintsEnabled(ctx, userID)
	if !masterEnabled {
		return nil, nil
	}

	if filterHostID > 0 {
		prob, ok := s.corraldHints.get(userID, filterHostID)
		if !ok {
			return nil, nil
		}
		row, err := s.store.CorraldHostHint(ctx, userID, filterHostID)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return nil, err
		}
		if errors.Is(err, store.ErrNotFound) {
			row = nil
		}
		if item, eligible := s.evaluateCorraldHostHint(ctx, userID, filterHostID, prob, row, masterEnabled); eligible {
			return item, nil
		}
		return nil, nil
	}

	probs := s.corraldHints.forUser(userID)
	if len(probs) == 0 {
		return nil, nil
	}
	rows, err := s.store.ListCorraldHostHints(ctx, userID)
	if err != nil {
		return nil, err
	}
	for _, entry := range probs {
		var row *store.CorraldHostHint
		if existing, ok := rows[entry.HostID]; ok {
			r := existing
			row = &r
		}
		if item, eligible := s.evaluateCorraldHostHint(ctx, userID, entry.HostID, entry.Prob, row, masterEnabled); eligible {
			return item, nil
		}
	}
	return nil, nil
}

// GET /api/corrald/hints[?host_id=<id>]
func (s *Server) apiGetCorraldHints(w http.ResponseWriter, r *http.Request) {
	var filterHostID int64
	if raw := strings.TrimSpace(r.URL.Query().Get("host_id")); raw != "" {
		id, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || id <= 0 {
			writeError(w, http.StatusBadRequest, "bad_request", "invalid host_id")
			return
		}
		filterHostID = id
	}
	item, err := s.selectCorraldHint(r.Context(), currentUser(r).ID, filterHostID)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "no such host")
		return
	}
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{"hint": item})
}

// POST /api/hosts/{id}/corrald/hint/shown
func (s *Server) apiMarkCorraldHintShown(w http.ResponseWriter, r *http.Request) {
	h, ok := s.ownedHost(w, r)
	if !ok {
		return
	}
	if err := s.store.MarkCorraldHintShown(r.Context(), currentUser(r).ID, h.ID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not_found", "no such host")
			return
		}
		s.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// POST /api/hosts/{id}/corrald/hint/dismiss
func (s *Server) apiDismissCorraldHint(w http.ResponseWriter, r *http.Request) {
	h, ok := s.ownedHost(w, r)
	if !ok {
		return
	}
	if err := s.store.MarkCorraldHintDismissed(r.Context(), currentUser(r).ID, h.ID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not_found", "no such host")
			return
		}
		s.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
