package web

import (
	"context"
	"errors"
	"fmt"
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
	"github.com/ihainan/corral/internal/term"
)

// CorraldHintReason identifies a real, observable failure category that
// enabling the host helper daemon (corrald) directly resolves.
type CorraldHintReason string

const (
	// HintReasonSSHSessionFull indicates an SSH status or log read failed
	// because SSH session/channel slots on the connection were exhausted
	// (either the gateway's per-connection session quota or the host's
	// OpenSSH MaxSessions / resource-shortage refusal).
	HintReasonSSHSessionFull CorraldHintReason = "ssh_session_full"
)

// CorraldHintOp identifies which read path(s) hit the failure on a host.
type CorraldHintOp string

const (
	HintOpStatus CorraldHintOp = "status"
	HintOpLog    CorraldHintOp = "log"
	HintOpBoth   CorraldHintOp = "both"
)

func isExplicitSessionLimitMessage(s string) bool {
	msg := strings.ToLower(s)
	if strings.Contains(msg, "forwarding") {
		return false
	}
	return strings.Contains(msg, "maxsessions") ||
		strings.Contains(msg, "max sessions") ||
		strings.Contains(msg, "no more sessions") ||
		strings.Contains(msg, "too many sessions") ||
		strings.Contains(msg, "session limit")
}

// ClassifyCorraldSolvableError determines whether err is a real, reliable
// failure that enabling corrald can solve: exclusively SSH session-slot
// exhaustion (sshx.ErrBusy, RFC 4254 ResourceShortage, or an explicit
// MaxSessions / session-limit refusal).
//
// Generic "channel open failed", policy-based ssh.Prohibited / "open failed",
// timeouts, cancellations, remote command non-zero exits, file/SFTP errors,
// authentication/host-key/jump-host errors, and generic network drops return
// ("", false).
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

	// 2. Remote SSH server rejected opening a session channel due to session
	//    slot exhaustion (ResourceShortage, or Prohibited with an explicit
	//    MaxSessions / no-more-sessions message — never policy rejections).
	var openErr *ssh.OpenChannelError
	if errors.As(err, &openErr) {
		switch openErr.Reason {
		case ssh.ResourceShortage:
			return HintReasonSSHSessionFull, true
		case ssh.Prohibited:
			if isExplicitSessionLimitMessage(openErr.Message) {
				return HintReasonSSHSessionFull, true
			}
			return "", false
		default:
			return "", false
		}
	}

	// 3. Stringified explicit session-limit or resource-shortage messages
	//    (never generic "channel open failed" or "administratively prohibited").
	msg := strings.ToLower(err.Error())
	if strings.Contains(msg, "forwarding") {
		return "", false
	}
	if isExplicitSessionLimitMessage(msg) || strings.Contains(msg, "resource shortage") {
		return HintReasonSSHSessionFull, true
	}

	return "", false
}

// corraldHintTarget returns the canonical target tuple (address, port, login
// user) used to bind in-memory failure records to a host's current target.
func corraldHintTarget(h *store.Host) string {
	if h == nil {
		return ""
	}
	return fmt.Sprintf("%s\x00%d\x00%s", strings.TrimSpace(h.Address), h.Port, h.Username)
}

// corraldHintTargetFromSession extracts (address, port, login user) from a
// running session's hostConfig fingerprint ("connVer|addr|port|user|auth|cred").
func corraldHintTargetFromSession(sess *term.Session) string {
	if sess == nil || sess.HostConfig == "" {
		return ""
	}
	parts := strings.Split(sess.HostConfig, "|")
	if len(parts) < 4 {
		return ""
	}
	addr := strings.TrimSpace(parts[1])
	port := strings.TrimSpace(parts[2])
	user := parts[3]
	if addr == "" || port == "" {
		return ""
	}
	return addr + "\x00" + port + "\x00" + user
}

const maxCorraldHintEntries = 4096

type corraldHintKey struct {
	userID int64
	hostID int64
}

type corraldHostProblem struct {
	Target string
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
// failures per (user, host), bound to the host's (address, port, login user).
// Intentionally ephemeral: never writes to SQLite on failure and resets cleanly
// on gateway restart.
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

func (t *corraldHintTracker) record(userID, hostID int64, target string, op CorraldHintOp, err error) bool {
	if t == nil || userID <= 0 || hostID <= 0 || target == "" {
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
	if exists && cur.Target != target {
		cur = corraldHostProblem{}
	}
	cur.Target = target
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

func (s *Server) hostHintTarget(userID, hostID int64) string {
	if s == nil || s.store == nil || userID <= 0 || hostID <= 0 {
		return ""
	}
	h, err := s.store.Host(context.Background(), userID, hostID)
	if err != nil || h == nil {
		return ""
	}
	return corraldHintTarget(h)
}

// recordCorraldHintError records a corrald-solvable SSH failure in memory for
// (userID, hostID) bound to the host's current target. Safe to call on nil Server.
func (s *Server) recordCorraldHintError(userID, hostID int64, op CorraldHintOp, err error) bool {
	if s == nil || userID <= 0 || hostID <= 0 {
		return false
	}
	if _, ok := ClassifyCorraldSolvableError(err); !ok {
		return false
	}
	target := s.hostHintTarget(userID, hostID)
	if target == "" {
		return false
	}
	return s.corraldHints.record(userID, hostID, target, op, err)
}

// recordCorraldHintSessionError records a corrald-solvable SSH failure observed
// on sess, binding the record to sess's connected (address, port, login user)
// so a stale session cannot attach failures to a retargeted host record.
func (s *Server) recordCorraldHintSessionError(sess *term.Session, op CorraldHintOp, err error) bool {
	if s == nil || sess == nil || sess.OwnerID <= 0 || sess.HostID <= 0 {
		return false
	}
	if _, ok := ClassifyCorraldSolvableError(err); !ok {
		return false
	}
	target := corraldHintTargetFromSession(sess)
	if target == "" {
		target = s.hostHintTarget(sess.OwnerID, sess.HostID)
	}
	if target == "" {
		return false
	}
	return s.corraldHints.record(sess.OwnerID, sess.HostID, target, op, err)
}

// CorraldHintItem is the gateway's decision payload for the frontend contextual
// hint banner above the agent list.
type CorraldHintItem struct {
	HostID     int64             `json:"host_id"`
	HostName   string            `json:"host_name"`
	Reason     CorraldHintReason `json:"reason"`
	Op         CorraldHintOp     `json:"op"`
	Count      int               `json:"count"`
	LastAt     int64             `json:"last_at"`
	Generation int64             `json:"generation"`
}

// corraldHintsSettings reads the user's account-level master switch
// ("corraldHints", default true) and server-written reset generation
// ("corraldHintsResetAt", ms, default 0) from user_settings.
func (s *Server) corraldHintsSettings(ctx context.Context, userID int64) (enabled bool, resetAt int64) {
	if s == nil || s.store == nil || userID <= 0 {
		return false, 0
	}
	raw, err := s.store.UserSettings(ctx, userID)
	if err != nil {
		return true, 0
	}
	return store.ParseCorraldHintsSettings(raw)
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

// corraldHostProblemAndStateEligible verifies the non-DB eligibility rules
// shared by GET /api/corrald/hints and POST /api/hosts/{id}/corrald/hint/shown:
// corrald is available & not admin-disabled, a real problem is recorded for
// h's current target (address, port, login user), and h's daemon state is
// currently off and enableable.
func (s *Server) corraldHostProblemAndStateEligible(ctx context.Context, userID int64, h *store.Host, prob corraldHostProblem) bool {
	if s == nil || h == nil || prob.Count <= 0 || prob.Reason == "" || prob.Target == "" {
		return false
	}
	if s.corrald == nil || !s.corrald.Available() || s.corraldAdminDisabled() {
		return false
	}
	if prob.Target != corraldHintTarget(h) {
		return false
	}
	st, err := s.corrald.Status(ctx, userID, h.ID)
	if err != nil || !corraldHostStatusEligible(st) {
		return false
	}
	return true
}

// evaluateCorraldHostHint is the single centralized rule determining whether a
// contextual corrald hint should be shown for (userID, hostID):
//  1. A real corrald-solvable problem was observed on this host's current target.
//  2. The account-level master switch ("corraldHints") is enabled.
//  3. Corrald is available on this gateway and not disabled by the admin.
//  4. This host has not already been shown or dismissed at or after resetAt.
//  5. The host exists, belongs to userID, its target still matches prob.Target,
//     and it is currently unenabled & enableable.
func (s *Server) evaluateCorraldHostHint(
	ctx context.Context,
	userID, hostID int64,
	prob corraldHostProblem,
	hintRow *store.CorraldHostHint,
	masterEnabled bool,
	resetAt int64,
) (*CorraldHintItem, bool) {
	if !masterEnabled || store.CorraldHintRowActive(hintRow, resetAt) {
		return nil, false
	}
	if s == nil || s.corrald == nil || !s.corrald.Available() || s.corraldAdminDisabled() {
		return nil, false
	}
	h, err := s.store.Host(ctx, userID, hostID)
	if err != nil || h == nil {
		return nil, false
	}
	if !s.corraldHostProblemAndStateEligible(ctx, userID, h, prob) {
		return nil, false
	}
	op := prob.Op
	if op == "" {
		op = HintOpStatus
	}
	return &CorraldHintItem{
		HostID:     h.ID,
		HostName:   h.Name,
		Reason:     prob.Reason,
		Op:         op,
		Count:      prob.Count,
		LastAt:     prob.LastAt.Unix(),
		Generation: resetAt,
	}, true
}

// selectCorraldHint returns the single most recently affected eligible host
// hint for userID (or for filterHostID when > 0) alongside the user's current
// reset generation.
func (s *Server) selectCorraldHint(ctx context.Context, userID, filterHostID int64) (*CorraldHintItem, int64, error) {
	if filterHostID > 0 {
		if _, err := s.store.Host(ctx, userID, filterHostID); err != nil {
			return nil, 0, err
		}
	}
	masterEnabled, resetAt := s.corraldHintsSettings(ctx, userID)
	if s.corrald == nil || !s.corrald.Available() || s.corraldAdminDisabled() || !masterEnabled {
		return nil, resetAt, nil
	}

	if filterHostID > 0 {
		prob, ok := s.corraldHints.get(userID, filterHostID)
		if !ok {
			return nil, resetAt, nil
		}
		row, err := s.store.CorraldHostHint(ctx, userID, filterHostID)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return nil, resetAt, err
		}
		if errors.Is(err, store.ErrNotFound) {
			row = nil
		}
		if item, eligible := s.evaluateCorraldHostHint(ctx, userID, filterHostID, prob, row, masterEnabled, resetAt); eligible {
			return item, resetAt, nil
		}
		return nil, resetAt, nil
	}

	probs := s.corraldHints.forUser(userID)
	if len(probs) == 0 {
		return nil, resetAt, nil
	}
	rows, err := s.store.ListCorraldHostHints(ctx, userID)
	if err != nil {
		return nil, resetAt, err
	}
	for _, entry := range probs {
		var row *store.CorraldHostHint
		if existing, ok := rows[entry.HostID]; ok {
			r := existing
			row = &r
		}
		if item, eligible := s.evaluateCorraldHostHint(ctx, userID, entry.HostID, entry.Prob, row, masterEnabled, resetAt); eligible {
			return item, resetAt, nil
		}
	}
	return nil, resetAt, nil
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
	item, gen, err := s.selectCorraldHint(r.Context(), currentUser(r).ID, filterHostID)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "no such host")
		return
	}
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{
		"hint":       item,
		"generation": gen,
	})
}

func readHintGenerationBody(r *http.Request) (int64, error) {
	if r.Body == nil || r.ContentLength == 0 {
		return 0, nil
	}
	var b struct {
		Generation int64 `json:"generation"`
	}
	if err := readJSON(r, &b); err != nil {
		return 0, err
	}
	if b.Generation < 0 {
		return 0, store.ErrInvalidHintGeneration
	}
	return b.Generation, nil
}

// POST /api/hosts/{id}/corrald/hint/shown
// Atomic claim-before-display: records shown_at ONLY if this host is still
// eligible for the user and generation (failure record present & bound to
// current target, corrald available & not admin-disabled, host state off &
// enableable, master switch on, current generation, not already shown or
// dismissed). Returns 200 {"claimed": true|false}.
func (s *Server) apiMarkCorraldHintShown(w http.ResponseWriter, r *http.Request) {
	h, ok := s.ownedHost(w, r)
	if !ok {
		return
	}
	gen, err := readHintGenerationBody(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	userID := currentUser(r).ID
	w.Header().Set("Cache-Control", "no-store")
	prob, hasProb := s.corraldHints.get(userID, h.ID)
	if !hasProb || !s.corraldHostProblemAndStateEligible(r.Context(), userID, h, prob) {
		writeJSON(w, http.StatusOK, map[string]any{"claimed": false})
		return
	}
	// Re-read mutable eligibility after the Status() lookup so concurrent
	// retargeting, failure clearing, or admin disable refuses the claim before
	// writing shown_at.
	curHost, err := s.store.Host(r.Context(), userID, h.ID)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "no such host")
		return
	}
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	prob, hasProb = s.corraldHints.get(userID, h.ID)
	if s.corrald == nil || !s.corrald.Available() || s.corraldAdminDisabled() ||
		!hasProb || prob.Count <= 0 || prob.Reason == "" || prob.Target != corraldHintTarget(curHost) {
		writeJSON(w, http.StatusOK, map[string]any{"claimed": false})
		return
	}
	claimed, err := s.store.ClaimCorraldHintShown(r.Context(), userID, h.ID, gen)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "no such host")
		return
	}
	if errors.Is(err, store.ErrInvalidHintGeneration) {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"claimed": claimed})
}

// POST /api/hosts/{id}/corrald/hint/dismiss
func (s *Server) apiDismissCorraldHint(w http.ResponseWriter, r *http.Request) {
	h, ok := s.ownedHost(w, r)
	if !ok {
		return
	}
	gen, err := readHintGenerationBody(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	applied, err := s.store.MarkCorraldHintDismissed(r.Context(), currentUser(r).ID, h.ID, gen)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "no such host")
		return
	}
	if errors.Is(err, store.ErrInvalidHintGeneration) {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	if !applied {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
