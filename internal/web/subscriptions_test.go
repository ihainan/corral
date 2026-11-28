package web

import (
	"context"
	"encoding/json"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ihainan/corral/internal/store"
)

func TestClaudeAuthInfoOfStatus(t *testing.T) {
	raw := []byte(`{"loggedIn":true,"authMethod":"claude.ai","apiProvider":"firstParty","email":"dev@example.com","subscriptionType":"max","rateLimitTier":"default_claude_max_20x"}`)
	plan, email := claudeAuthInfoOfStatus(raw)
	if plan != "Claude Max 20x" || email != "dev@example.com" {
		t.Fatalf("got plan=%q email=%q", plan, email)
	}
	// API key sign-in must yield empty plan and email.
	plan, email = claudeAuthInfoOfStatus([]byte(`{"loggedIn":true,"authMethod":"api_key","email":"dev@example.com","subscriptionType":"max"}`))
	if plan != "" || email != "" {
		t.Fatalf("api_key should be ignored, got plan=%q email=%q", plan, email)
	}
}

func TestSubscriptionsOverviewReadOnlyDedupAndCooldown(t *testing.T) {
	h := newHarness(t)
	admin := h.admin()
	alice := h.member(admin, "alice")
	bob := h.member(admin, "bob")

	aliceUser, err := h.store.UserByName(context.Background(), "alice")
	if err != nil {
		t.Fatal(err)
	}

	var refreshCalls atomic.Int32
	h.srv.subscriptionRefreshFn = func(ctx context.Context, ownerID int64, provider, accountKey string) (quotaInfo, string, error) {
		refreshCalls.Add(1)
		return quotaInfo{
			Known:     true,
			UpdatedAt: time.Now().Unix(),
			Plan:      "Claude Max 20x",
			FiveHour:  &quotaWindow{Used: 55, ResetsAt: time.Now().Add(2 * time.Hour).Unix()},
			Weekly:    &quotaWindow{Used: 77, ResetsAt: time.Now().Add(37 * time.Hour).Unix()},
		}, "alice@example.com", nil
	}

	now := time.Now().Unix()
	// Record the same Claude account from two different hosts: must deduplicate to 1 item.
	h.srv.recordSubscriptionSnapshot(aliceUser.ID, "claude", "uuid-claude-1", "alice@example.com", 101, "gpu-1", quotaInfo{
		Known:     true,
		UpdatedAt: now - 600,
		Plan:      "Claude Max 20x",
		FiveHour:  &quotaWindow{Used: 20, ResetsAt: now + 3600},
		Weekly:    &quotaWindow{Used: 40, ResetsAt: now + 86400},
	})
	h.srv.recordSubscriptionSnapshot(aliceUser.ID, "claude", "uuid-claude-1", "", 102, "gpu-2", quotaInfo{
		Known:     true,
		UpdatedAt: now - 120,
		Plan:      "Claude Max 20x",
		FiveHour:  &quotaWindow{Used: 33, ResetsAt: now + 3600},
		Weekly:    &quotaWindow{Used: 48, ResetsAt: now + 86400},
	})
	// Record a Codex snapshot and an Agy snapshot.
	h.srv.recordSubscriptionSnapshot(aliceUser.ID, "codex", "host:101", "", 101, "gpu-1", quotaInfo{
		Known:     true,
		UpdatedAt: now - 3600,
		Plan:      "ChatGPT Pro",
		FiveHour:  &quotaWindow{Used: 15, ResetsAt: now + 7200},
		Weekly:    &quotaWindow{Used: 62, ResetsAt: now + 172800},
	})
	h.srv.recordSubscriptionSnapshot(aliceUser.ID, "agy", "host:102", "", 102, "gpu-2", quotaInfo{
		Known:     true,
		UpdatedAt: now - 2*86400, // stale (>1 day)
		Plan:      "Google AI Ultra",
		Groups: []quotaGroup{
			{Name: "Gemini Models", FiveHour: &quotaWindow{Used: 10, ResetsAt: now - 100}},
		},
	})

	// 1. GET /api/subscriptions returns cached snapshots only and triggers zero refresh calls.
	r := alice.do("GET", "/api/subscriptions", nil)
	mustStatus(t, r, http.StatusOK, "list subscriptions")
	if refreshCalls.Load() != 0 {
		t.Fatalf("opening subscriptions page must not call provider refresh, got %d calls", refreshCalls.Load())
	}
	var listResp subscriptionsListResponse
	if err := json.Unmarshal(r.Body, &listResp); err != nil {
		t.Fatal(err)
	}
	if len(listResp.Items) != 3 {
		t.Fatalf("expected 3 deduplicated items (claude, codex, agy), got %d: %+v", len(listResp.Items), listResp.Items)
	}
	if listResp.CooldownSeconds != 300 {
		t.Fatalf("expected cooldown_seconds=300, got %d", listResp.CooldownSeconds)
	}
	var claudeItem, codexItem *subscriptionItem
	for i := range listResp.Items {
		switch listResp.Items[i].Provider {
		case "claude":
			claudeItem = &listResp.Items[i]
		case "codex":
			codexItem = &listResp.Items[i]
		}
	}
	if claudeItem == nil || len(claudeItem.Hosts) != 2 || claudeItem.HostName != "gpu-2" || claudeItem.AccountLabel != "alice@example.com" {
		t.Fatalf("unexpected deduplicated claude item: %+v", claudeItem)
	}
	if codexItem == nil || codexItem.CanRefresh || codexItem.RefreshReason != "passive_only" {
		t.Fatalf("codex should be passive_only: %+v", codexItem)
	}

	// 2. Cross-user isolation: bob sees empty list and gets 404 refreshing alice's account.
	rb := bob.do("GET", "/api/subscriptions", nil)
	mustStatus(t, rb, http.StatusOK, "bob list subscriptions")
	var bobList subscriptionsListResponse
	if err := json.Unmarshal(rb.Body, &bobList); err != nil {
		t.Fatal(err)
	}
	if len(bobList.Items) != 0 {
		t.Fatalf("bob should see 0 subscriptions, got %d", len(bobList.Items))
	}
	rbRef := bob.do("POST", "/api/subscriptions/refresh", map[string]any{
		"provider":    "claude",
		"account_key": "uuid-claude-1",
	})
	mustStatus(t, rbRef, http.StatusNotFound, "bob cannot refresh alice's subscription")
	if refreshCalls.Load() != 0 {
		t.Fatalf("unauthorized refresh must not run, got %d calls", refreshCalls.Load())
	}

	// 3. Codex manual refresh is rejected cleanly.
	rCodex := alice.do("POST", "/api/subscriptions/refresh", map[string]any{
		"provider":    "codex",
		"account_key": "host:101",
	})
	mustStatus(t, rCodex, http.StatusBadRequest, "codex refresh unsupported")

	// 4. First manual refresh for claude succeeds, updates snapshot, and writes audit entry.
	r1 := alice.do("POST", "/api/subscriptions/refresh", map[string]any{
		"provider":    "claude",
		"account_key": "uuid-claude-1",
	})
	mustStatus(t, r1, http.StatusOK, "first manual refresh")
	if refreshCalls.Load() != 1 {
		t.Fatalf("expected 1 refresh call, got %d", refreshCalls.Load())
	}
	var refResp subscriptionRefreshResponse
	if err := json.Unmarshal(r1.Body, &refResp); err != nil {
		t.Fatal(err)
	}
	if refResp.Item.Quota.Weekly == nil || refResp.Item.Quota.Weekly.Used != 77 {
		t.Fatalf("expected updated weekly quota 77, got %+v", refResp.Item.Quota)
	}
	if refResp.Item.NextRefreshAt <= time.Now().Unix() {
		t.Fatalf("expected future next_refresh_at, got %d", refResp.Item.NextRefreshAt)
	}

	// Verify audit entry recorded.
	entries, err := h.store.ListAudit(context.Background(), store.AuditQuery{Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	foundAudit := false
	for _, e := range entries {
		if e.Action == "subscription.refresh" && e.Username == "alice" {
			foundAudit = true
			break
		}
	}
	if !foundAudit {
		t.Fatalf("expected subscription.refresh audit entry")
	}

	// 5. Immediate second manual refresh for the same account is rejected with 429 rate_limited.
	r2 := alice.do("POST", "/api/subscriptions/refresh", map[string]any{
		"provider":    "claude",
		"account_key": "uuid-claude-1",
	})
	mustStatus(t, r2, http.StatusTooManyRequests, "second refresh within 5 min blocked")
	if refreshCalls.Load() != 1 {
		t.Fatalf("rate-limited request must not invoke refresh, got %d calls", refreshCalls.Load())
	}
	j2 := r2.json()
	if j2["error"] != "rate_limited" {
		t.Fatalf("expected error=rate_limited, got %+v", j2)
	}
	extra, _ := j2["extra"].(map[string]any)
	if retryAfter, _ := extra["retry_after"].(float64); retryAfter <= 0 || retryAfter > 300 {
		t.Fatalf("unexpected retry_after: %+v", extra)
	}

	// 6. Refreshing a different account (agy) is independent and succeeds immediately.
	r3 := alice.do("POST", "/api/subscriptions/refresh", map[string]any{
		"provider":    "agy",
		"account_key": "host:102",
	})
	mustStatus(t, r3, http.StatusOK, "different account refresh allowed")
	if refreshCalls.Load() != 2 {
		t.Fatalf("expected 2 total refresh calls, got %d", refreshCalls.Load())
	}

	// 7. Without test hook and with no active session on host, refresh returns 409 host_offline.
	h.srv.subscriptionRefreshFn = nil
	h.srv.recordSubscriptionSnapshot(aliceUser.ID, "claude", "uuid-offline", "", 999, "offline-host", quotaInfo{
		Known:     true,
		UpdatedAt: now - 300,
		FiveHour:  &quotaWindow{Used: 10, ResetsAt: now + 3600},
	})
	rOff := alice.do("POST", "/api/subscriptions/refresh", map[string]any{
		"provider":    "claude",
		"account_key": "uuid-offline",
	})
	mustStatus(t, rOff, http.StatusConflict, "offline host refresh rejected")
}
