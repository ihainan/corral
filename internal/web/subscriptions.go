package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/ihainan/corral/internal/convo"
	"github.com/ihainan/corral/internal/sshx"
	"github.com/ihainan/corral/internal/store"
	"github.com/ihainan/corral/internal/term"
)

// GET  /api/subscriptions
// POST /api/subscriptions/refresh
//
// The Subscriptions overview displays quota snapshots already collected across
// the user's hosts and persisted in SQLite. Opening the page makes zero SSH or
// provider calls. Manual refresh is strictly opt-in (user click only) and
// rate-limited per (user, provider, account) to at most once every 5 minutes
// in SQLite.

const (
	subscriptionRefreshCooldown = 5 * time.Minute
	subscriptionManualTimeout   = 35 * time.Second
)

var errSubscriptionAccountChanged = errors.New("subscription account changed on host")

type subscriptionItem struct {
	Provider      string                   `json:"provider"`
	AccountKey    string                   `json:"account_key"`
	AccountLabel  string                   `json:"account_label,omitempty"`
	Plan          string                   `json:"plan,omitempty"`
	HostID        int64                    `json:"host_id"`
	HostName      string                   `json:"host_name"`
	Hosts         []store.SubscriptionHost `json:"hosts"`
	Quota         quotaInfo                `json:"quota"`
	UpdatedAt     int64                    `json:"updated_at"`
	SeenAt        int64                    `json:"seen_at"`
	CanRefresh    bool                     `json:"can_refresh"`
	RefreshReason string                   `json:"refresh_reason,omitempty"`
	NextRefreshAt int64                    `json:"next_refresh_at,omitempty"`
}

type subscriptionsListResponse struct {
	Items           []subscriptionItem `json:"items"`
	ServerTime      int64              `json:"server_time"`
	CooldownSeconds int64              `json:"cooldown_seconds"`
}

type subscriptionRefreshRequest struct {
	Provider   string `json:"provider"`
	AccountKey string `json:"account_key"`
}

type subscriptionRefreshResponse struct {
	Item            subscriptionItem `json:"item"`
	ServerTime      int64            `json:"server_time"`
	CooldownSeconds int64            `json:"cooldown_seconds"`
}

// codexQuotaRecorded deduplicates in-memory writes of identical Codex rollout
// quota snapshots so repeated conversation polls do not hammer SQLite.
type codexQuotaRecorded struct {
	mu   sync.Mutex
	last map[string]string
}

func (c *codexQuotaRecorded) changed(key, sig string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.last == nil {
		c.last = map[string]string{}
	}
	if c.last[key] == sig {
		return false
	}
	if len(c.last) > 256 {
		clear(c.last)
	}
	c.last[key] = sig
	return true
}

func shortAccountLabel(accountKey string) string {
	k := strings.TrimSpace(accountKey)
	if k == "" || strings.HasPrefix(k, "host:") || strings.HasPrefix(k, "ws:") {
		return ""
	}
	if len(k) > 12 {
		return k[:8] + "…"
	}
	return k
}

// recordSubscriptionSnapshot persists a non-secret quota snapshot in SQLite so
// the Subscriptions overview can render it later with zero provider calls.
func (s *Server) recordSubscriptionSnapshot(ownerID int64, provider, accountKey, accountLabel string, hostID int64, hostName string, q quotaInfo) {
	if s.store == nil || ownerID <= 0 || provider == "" || accountKey == "" || !q.Known {
		return
	}
	quotaBytes, err := json.Marshal(q)
	if err != nil {
		return
	}
	now := time.Now().Unix()
	updatedAt := q.UpdatedAt
	if updatedAt <= 0 {
		updatedAt = now
	}
	label := strings.TrimSpace(accountLabel)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = s.store.UpsertSubscriptionSnapshot(ctx, store.SubscriptionSnapshot{
		OwnerID:      ownerID,
		Provider:     provider,
		AccountKey:   accountKey,
		AccountLabel: label,
		Plan:         cleanPlanName(q.Plan),
		HostID:       hostID,
		HostName:     hostName,
		QuotaJSON:    quotaBytes,
		UpdatedAt:    updatedAt,
		SeenAt:       now,
	})
}

func quotaInfoFromConvo(q *convo.Quota) quotaInfo {
	if q == nil {
		return quotaInfo{}
	}
	out := quotaInfo{
		UpdatedAt: q.UpdatedAt,
		Plan:      cleanPlanName(q.Plan),
	}
	if q.FiveHour != nil {
		out.FiveHour = &quotaWindow{Used: q.FiveHour.Used, ResetsAt: q.FiveHour.ResetsAt}
	}
	if q.Weekly != nil {
		out.Weekly = &quotaWindow{Used: q.Weekly.Used, ResetsAt: q.Weekly.ResetsAt}
	}
	out.Known = out.FiveHour != nil || out.Weekly != nil
	return out
}

// recordCodexQuotaSnapshot passively persists Codex quota observed from a
// rollout log when its values or timestamp change.
func (s *Server) recordCodexQuotaSnapshot(sess *term.Session, q *convo.Quota) {
	if sess == nil || q == nil {
		return
	}
	qi := quotaInfoFromConvo(q)
	if !qi.Known {
		return
	}
	key := fmt.Sprintf("%d\x00%d", sess.OwnerID, sess.HostID)
	sig := fmt.Sprintf("%d|%s|%v|%v", qi.UpdatedAt, qi.Plan, qi.FiveHour, qi.Weekly)
	if !s.codexQuotaSeen.changed(key, sig) {
		return
	}
	s.recordSubscriptionSnapshot(sess.OwnerID, "codex", fmt.Sprintf("host:%d", sess.HostID), "", sess.HostID, sess.HostName, qi)
}

func (s *Server) findRunningSessionForHosts(ownerID, primaryHostID int64, hosts []store.SubscriptionHost) *term.Session {
	if s.terms == nil {
		return nil
	}
	tryHost := func(hid int64) *term.Session {
		if hid <= 0 {
			return nil
		}
		for _, sess := range s.terms.Running(ownerID, hid) {
			if sess.State() == term.StateRunning {
				return sess
			}
		}
		return nil
	}
	if sess := tryHost(primaryHostID); sess != nil {
		return sess
	}
	for _, h := range hosts {
		if h.HostID == primaryHostID {
			continue
		}
		if sess := tryHost(h.HostID); sess != nil {
			return sess
		}
	}
	return nil
}

func (s *Server) subscriptionItemOf(sn *store.SubscriptionSnapshot, ownerID, now int64) subscriptionItem {
	var q quotaInfo
	_ = json.Unmarshal(sn.QuotaJSON, &q)
	if q.Plan == "" {
		q.Plan = sn.Plan
	}
	if q.UpdatedAt == 0 {
		q.UpdatedAt = sn.UpdatedAt
	}
	label := sn.AccountLabel
	if label == "" {
		if sn.Provider == "opencode" {
			label = strings.TrimSpace(sn.HostName)
		} else {
			label = shortAccountLabel(sn.AccountKey)
		}
	}
	item := subscriptionItem{
		Provider:     sn.Provider,
		AccountKey:   sn.AccountKey,
		AccountLabel: label,
		Plan:         sn.Plan,
		HostID:       sn.HostID,
		HostName:     sn.HostName,
		Hosts:        sn.Hosts,
		Quota:        q,
		UpdatedAt:    sn.UpdatedAt,
		SeenAt:       sn.SeenAt,
	}
	cooldownSec := int64(subscriptionRefreshCooldown / time.Second)
	effectiveLastRefresh := sn.LastRefreshAt
	if sn.Provider == "opencode" && s.store != nil {
		targetHostID := sn.HostID
		if sess := s.findRunningSessionForHosts(ownerID, sn.HostID, sn.Hosts); sess != nil {
			targetHostID = sess.HostID
		}
		if targetHostID > 0 {
			rctx, rcancel := context.WithTimeout(context.Background(), 2*time.Second)
			if hostLast, err := s.store.GetSubscriptionHostRefreshAt(rctx, ownerID, targetHostID, sn.Provider); err == nil && hostLast > effectiveLastRefresh {
				effectiveLastRefresh = hostLast
			}
			rcancel()
		}
	}
	if effectiveLastRefresh > 0 && effectiveLastRefresh+cooldownSec > now {
		item.NextRefreshAt = effectiveLastRefresh + cooldownSec
	}
	switch sn.Provider {
	case "codex", "devin":
		item.CanRefresh = false
		item.RefreshReason = "passive_only"
	case "claude", "agy", "opencode":
		if s.findRunningSessionForHosts(ownerID, sn.HostID, sn.Hosts) != nil {
			item.CanRefresh = true
		} else {
			item.CanRefresh = false
			item.RefreshReason = "host_offline"
		}
	default:
		item.CanRefresh = false
		item.RefreshReason = "passive_only"
	}
	return item
}

// GET /api/subscriptions — returns all cached subscription snapshots owned by
// the signed-in user. Strictly read-only against SQLite; makes zero SSH or
// provider calls.
func (s *Server) apiListSubscriptions(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	snaps, err := s.store.ListSubscriptionSnapshots(r.Context(), u.ID)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	now := time.Now().Unix()
	items := make([]subscriptionItem, 0, len(snaps))
	for _, sn := range snaps {
		items = append(items, s.subscriptionItemOf(sn, u.ID, now))
	}
	writeJSON(w, http.StatusOK, subscriptionsListResponse{
		Items:           items,
		ServerTime:      now,
		CooldownSeconds: int64(subscriptionRefreshCooldown / time.Second),
	})
}

// POST /api/subscriptions/refresh — manually refreshes a single account on
// user click, strictly rate-limited in SQLite to at least 5 minutes per
// (user, provider, account_key).
func (s *Server) apiRefreshSubscription(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	var req subscriptionRefreshRequest
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid request body")
		return
	}
	req.Provider = strings.TrimSpace(req.Provider)
	req.AccountKey = strings.TrimSpace(req.AccountKey)
	if req.AccountKey == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "account_key is required")
		return
	}
	switch req.Provider {
	case "claude", "agy", "opencode":
	case "codex":
		writeError(w, http.StatusBadRequest, "refresh_unsupported", "Codex quota is collected passively from session logs")
		return
	case "devin":
		writeError(w, http.StatusBadRequest, "refresh_unsupported", "Devin quota is collected passively from local cache")
		return
	default:
		writeError(w, http.StatusBadRequest, "bad_request", "unsupported provider")
		return
	}

	sn, err := s.store.GetSubscriptionSnapshot(r.Context(), u.ID, req.Provider, req.AccountKey)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "subscription not found")
		return
	}
	if err != nil {
		s.internalError(w, r, err)
		return
	}

	var sess *term.Session
	if s.subscriptionRefreshFn == nil {
		sess = s.findRunningSessionForHosts(u.ID, sn.HostID, sn.Hosts)
		if sess == nil {
			writeError(w, http.StatusConflict, "host_offline", "no active session on this account's host")
			return
		}
	}

	cooldownSec := int64(subscriptionRefreshCooldown / time.Second)
	prevRefreshAt := sn.LastRefreshAt
	allowed, retryAfter, nextAt, err := s.store.TryClaimSubscriptionRefresh(r.Context(), u.ID, req.Provider, req.AccountKey, cooldownSec)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "subscription not found")
		return
	}
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	if !allowed {
		writeErrorExtra(w, http.StatusTooManyRequests, "rate_limited", "please wait before refreshing this account again", map[string]any{
			"retry_after":     retryAfter,
			"next_refresh_at": nextAt,
		})
		return
	}
	claimedAt := nextAt - cooldownSec

	targetHostID := sn.HostID
	targetHost := sn.HostName
	if sess != nil {
		targetHostID = sess.HostID
		targetHost = sess.HostName
	}
	// For OpenCode, also check and claim a durable (user, host, provider) cooldown
	// BEFORE any remote call (covering placeholders and workspace switches); once
	// claimed it is never released on rejection or account_changed.
	if req.Provider == "opencode" && targetHostID > 0 {
		hostAllowed, hostRetry, hostNextAt, herr := s.store.TryClaimSubscriptionHostRefresh(r.Context(), u.ID, targetHostID, req.Provider, cooldownSec)
		if herr != nil {
			rctx, rcancel := context.WithTimeout(context.Background(), 3*time.Second)
			_ = s.store.ResetSubscriptionRefresh(rctx, u.ID, req.Provider, req.AccountKey, claimedAt, prevRefreshAt)
			rcancel()
			s.internalError(w, r, herr)
			return
		}
		if !hostAllowed {
			// No remote call was executed; restore the per-account claim and reject via host cooldown.
			rctx, rcancel := context.WithTimeout(context.Background(), 3*time.Second)
			_ = s.store.ResetSubscriptionRefresh(rctx, u.ID, req.Provider, req.AccountKey, claimedAt, prevRefreshAt)
			rcancel()
			writeErrorExtra(w, http.StatusTooManyRequests, "rate_limited", "please wait before refreshing this host again", map[string]any{
				"retry_after":     hostRetry,
				"next_refresh_at": hostNextAt,
			})
			return
		}
	}
	s.audit(r, "subscription.refresh", targetHost, map[string]any{
		"provider": req.Provider,
		"account":  req.AccountKey,
	})

	ctx, cancel := context.WithTimeout(r.Context(), subscriptionManualTimeout)
	defer cancel()

	resolvedKey := req.AccountKey
	var refreshErr error
	if s.subscriptionRefreshFn != nil {
		var q quotaInfo
		var label string
		q, label, refreshErr = s.subscriptionRefreshFn(ctx, u.ID, req.Provider, req.AccountKey)
		if refreshErr == nil && q.Known {
			s.recordSubscriptionSnapshot(u.ID, req.Provider, req.AccountKey, label, sn.HostID, sn.HostName, q)
		}
	} else {
		resolvedKey, refreshErr = s.refreshSubscriptionOnSession(ctx, sess, req.Provider, req.AccountKey)
	}
	if refreshErr != nil {
		if errors.Is(refreshErr, errSubscriptionAccountChanged) {
			// Never release OpenCode cooldowns on account_changed; Claude keeps its existing rollback.
			if req.Provider != "opencode" {
				rctx, rcancel := context.WithTimeout(context.Background(), 3*time.Second)
				_ = s.store.ResetSubscriptionRefresh(rctx, u.ID, req.Provider, req.AccountKey, claimedAt, prevRefreshAt)
				rcancel()
			}
			writeError(w, http.StatusConflict, "account_changed", "another account is currently signed in on this host")
			return
		}
		if errors.Is(refreshErr, errOpenCodeNotRunning) {
			writeError(w, http.StatusConflict, "opencode_not_running", "OpenCode is not running on this host")
			return
		}
		if errors.Is(refreshErr, errOpenCodeUnsupported) {
			writeError(w, http.StatusConflict, "opencode_unsupported", "OpenCode refresh is not supported on this host")
			return
		}
		if errors.Is(refreshErr, context.Canceled) {
			return
		}
		if errors.Is(refreshErr, sshx.ErrBusy) {
			writeSSHChannelsBusy(w)
			return
		}
		writeError(w, http.StatusBadGateway, "refresh_failed", "could not refresh subscription quota")
		return
	}

	if resolvedKey == "" {
		resolvedKey = req.AccountKey
	}
	updated, err := s.store.GetSubscriptionSnapshot(r.Context(), u.ID, req.Provider, resolvedKey)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	now := time.Now().Unix()
	writeJSON(w, http.StatusOK, subscriptionRefreshResponse{
		Item:            s.subscriptionItemOf(updated, u.ID, now),
		ServerTime:      now,
		CooldownSeconds: cooldownSec,
	})
}

func (s *Server) refreshSubscriptionOnSession(ctx context.Context, sess *term.Session, provider, expectedAccountKey string) (string, error) {
	now := time.Now()
	switch provider {
	case "agy":
		q, err := s.fetchAgyQuota(ctx, sess)
		if err != nil {
			return "", err
		}
		cacheKey := fmt.Sprintf("%d\x00%d", sess.OwnerID, sess.HostID)
		s.agyQuota.putCached(cacheKey, q, now)
		return expectedAccountKey, nil

	case "opencode":
		return s.fetchOpenCodeQuota(ctx, sess, expectedAccountKey)

	case "claude":
		readClaude := func() (quotaInfo, string, bool, error) {
			res, err := sshx.Exec(ctx, sess.Client(), quotaReadCommand(), quotaReadMax, sshx.ExecOptions{})
			if err != nil {
				return quotaInfo{}, "", false, err
			}
			q, signedIn, ok := quotaOfConfig(res.Out, time.Now())
			if !ok {
				return quotaInfo{}, "", false, errors.New("could not read claude config")
			}
			return q, quotaAccountOfConfig(res.Out), signedIn, nil
		}
		accountMatches := func(accUUID string, signedIn bool) bool {
			if strings.HasPrefix(expectedAccountKey, "host:") {
				return accUUID == ""
			}
			return signedIn && accUUID != "" && accUUID == expectedAccountKey
		}
		recordOtherAccount := func(q quotaInfo, accUUID string, signedIn bool) {
			if !q.Known || accUUID == "" || accUUID == expectedAccountKey {
				return
			}
			var email string
			if signedIn {
				planKey := fmt.Sprintf("%d\x00%d\x00%s", sess.OwnerID, sess.HostID, accUUID)
				q.Plan, email = s.claudePlan.getInfo(ctx, planKey, func(fctx context.Context) (string, string, error) {
					return s.fetchClaudeAuthInfo(fctx, sess)
				})
			}
			s.recordSubscriptionSnapshot(sess.OwnerID, "claude", accUUID, email, sess.HostID, sess.HostName, q)
		}

		q, accUUID, signedIn, err := readClaude()
		if err != nil {
			return "", err
		}
		if !accountMatches(accUUID, signedIn) {
			recordOtherAccount(q, accUUID, signedIn)
			return "", errSubscriptionAccountChanged
		}
		// Respect Claude Code's 15-minute minimum cadence both in gateway memory
		// and on-host (~/.cache/corral/usage-asked) before invoking `claude -p /usage`.
		old := !q.Known || time.Now().Unix()-q.UpdatedAt >= int64(quotaEvery/time.Second)
		if signedIn && old && s.quotaRuns.due(fmt.Sprintf("%d\x00%d", sess.OwnerID, sess.HostID), now) {
			_, _ = sshx.Exec(ctx, sess.Client(), quotaRefreshCommand(), 4<<10, sshx.ExecOptions{})
			if q2, acc2, signedIn2, err2 := readClaude(); err2 == nil {
				q, accUUID, signedIn = q2, acc2, signedIn2
			}
		}
		if !accountMatches(accUUID, signedIn) {
			recordOtherAccount(q, accUUID, signedIn)
			return "", errSubscriptionAccountChanged
		}
		var email string
		if signedIn {
			planKey := fmt.Sprintf("%d\x00%d\x00%s", sess.OwnerID, sess.HostID, accUUID)
			q.Plan, email = s.claudePlan.getInfo(ctx, planKey, func(fctx context.Context) (string, string, error) {
				return s.fetchClaudeAuthInfo(fctx, sess)
			})
		}
		accountKey := accUUID
		if accountKey == "" {
			accountKey = expectedAccountKey
		}
		if q.Known && accountKey != "" {
			s.recordSubscriptionSnapshot(sess.OwnerID, "claude", accountKey, email, sess.HostID, sess.HostName, q)
		}
		return expectedAccountKey, nil
	default:
		return "", errors.New("unsupported provider")
	}
}
