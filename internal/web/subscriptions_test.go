package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

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

func TestRefreshSubscriptionAccountChangedOnHost(t *testing.T) {
	h := newHarness(t)
	admin := h.admin()
	alice := h.member(admin, "alice")

	aliceUser, err := h.store.UserByName(context.Background(), "alice")
	if err != nil {
		t.Fatal(err)
	}

	now := time.Now()
	resetFuture := now.Add(3 * time.Hour).UTC().Format(time.RFC3339)

	var (
		mu              sync.Mutex
		curClaudeConfig string
		curAuthStatus   string
		usageRefreshHit atomic.Int32
	)
	setHostAccount := func(uuid, email string, weeklyUsed float64, fetchedAt time.Time) {
		mu.Lock()
		defer mu.Unlock()
		curClaudeConfig = fmt.Sprintf(
			`{"oauthAccount":{"accountUuid":%q},"cachedUsageUtilization":{"fetchedAtMs":%d,"accountUuid":%q,"utilization":{"seven_day":{"utilization":%.1f,"resets_at":%q}}}}`,
			uuid, fetchedAt.UnixMilli(), uuid, weeklyUsed, resetFuture,
		)
		curAuthStatus = fmt.Sprintf(
			`{"loggedIn":true,"authMethod":"claude.ai","apiProvider":"firstParty","email":%q,"subscriptionType":"max","rateLimitTier":"default_claude_max_20x"}`,
			email,
		)
	}
	// Host is currently logged in as account B (changed from historical account A).
	setHostAccount("uuid-account-b", "b@example.com", 60, now.Add(-1*time.Minute))

	srv := h.sshServer(t.TempDir())
	srv.HandleExec(func(cmd string) func(ssh.Channel) {
		switch {
		case strings.Contains(cmd, "cachedUsageUtilization"):
			return func(ch ssh.Channel) {
				mu.Lock()
				out := curClaudeConfig
				mu.Unlock()
				ch.Write([]byte(out + "\n"))
				ch.CloseWrite()
				ch.SendRequest("exit-status", false, []byte{0, 0, 0, 0})
			}
		case strings.Contains(cmd, "claude auth status"):
			return func(ch ssh.Channel) {
				mu.Lock()
				out := curAuthStatus
				mu.Unlock()
				ch.Write([]byte(out + "\n"))
				ch.CloseWrite()
				ch.SendRequest("exit-status", false, []byte{0, 0, 0, 0})
			}
		case strings.Contains(cmd, "claude -p /usage"):
			usageRefreshHit.Add(1)
			return func(ch ssh.Channel) {
				ch.CloseWrite()
				ch.SendRequest("exit-status", false, []byte{0, 0, 0, 0})
			}
		}
		return nil
	})

	rHost := alice.do("POST", "/api/hosts", map[string]any{
		"name": "gpu-box", "address": "127.0.0.1", "port": srv.Addr.Port,
		"username": "alice", "auth": "password", "password": "alice-pass", "persistence": "none",
	})
	mustStatus(t, rHost, http.StatusCreated, "create host")
	hostID := int64(rHost.json()["id"].(float64))

	rSess := alice.do("POST", "/api/sessions", map[string]any{"host_id": hostID})
	mustStatus(t, rSess, http.StatusCreated, "open session")

	// Seed historical snapshot for account A on this host.
	h.srv.recordSubscriptionSnapshot(aliceUser.ID, "claude", "uuid-account-a", "a@example.com", hostID, "gpu-box", quotaInfo{
		Known:     true,
		UpdatedAt: now.Add(-30 * time.Minute).Unix(),
		Plan:      "Claude Pro",
		Weekly:    &quotaWindow{Used: 25, ResetsAt: now.Add(24 * time.Hour).Unix()},
	})

	// 1. Refreshing A while host is signed in as B returns 409 account_changed.
	r := alice.do("POST", "/api/subscriptions/refresh", map[string]any{
		"provider":    "claude",
		"account_key": "uuid-account-a",
	})
	mustStatus(t, r, http.StatusConflict, "refresh A when host has B")
	if r.code() != "account_changed" {
		t.Fatalf("expected error=account_changed, got %s", r.Body)
	}
	if usageRefreshHit.Load() != 0 {
		t.Fatalf("must not invoke claude -p /usage when host is signed in as a different account, got %d hits", usageRefreshHit.Load())
	}

	// 2. Account A was NOT overwritten and its cooldown was rolled back (last_refresh_at == 0).
	snA, err := h.store.GetSubscriptionSnapshot(context.Background(), aliceUser.ID, "claude", "uuid-account-a")
	if err != nil {
		t.Fatal(err)
	}
	if snA.LastRefreshAt != 0 {
		t.Fatalf("expected account A cooldown rolled back to 0, got %d", snA.LastRefreshAt)
	}
	var qA quotaInfo
	_ = json.Unmarshal(snA.QuotaJSON, &qA)
	if qA.Weekly == nil || qA.Weekly.Used != 25 {
		t.Fatalf("account A quota should remain 25, got %+v", qA)
	}

	// 3. Account B's reading WAS passively recorded.
	snB, err := h.store.GetSubscriptionSnapshot(context.Background(), aliceUser.ID, "claude", "uuid-account-b")
	if err != nil {
		t.Fatalf("expected account B to be passively persisted: %v", err)
	}
	var qB quotaInfo
	_ = json.Unmarshal(snB.QuotaJSON, &qB)
	if qB.Weekly == nil || qB.Weekly.Used != 60 || snB.AccountLabel != "b@example.com" {
		t.Fatalf("unexpected account B snapshot: sn=%+v q=%+v", snB, qB)
	}

	// 4. Once the host switches back to account A, refreshing A succeeds immediately (cooldown not consumed).
	setHostAccount("uuid-account-a", "a@example.com", 35, now.Add(-1*time.Minute))
	r2 := alice.do("POST", "/api/subscriptions/refresh", map[string]any{
		"provider":    "claude",
		"account_key": "uuid-account-a",
	})
	mustStatus(t, r2, http.StatusOK, "refresh A after host switches back")
	var refResp subscriptionRefreshResponse
	if err := json.Unmarshal(r2.Body, &refResp); err != nil {
		t.Fatal(err)
	}
	if refResp.Item.Quota.Weekly == nil || refResp.Item.Quota.Weekly.Used != 35 {
		t.Fatalf("expected account A updated to 35, got %+v", refResp.Item.Quota)
	}
}

func TestSubscriptionsDevinOpenCodeAndClaudeScopedFlows(t *testing.T) {
	h := newHarness(t)
	admin := h.admin()
	alice := h.member(admin, "alice")

	aliceUser, err := h.store.UserByName(context.Background(), "alice")
	if err != nil {
		t.Fatal(err)
	}

	now := time.Now()
	var (
		mu              sync.Mutex
		curWSID         = "wrk_alpha"
		curWSName       = "SECRET-WORKSPACE-NAME"
		curWeeklyPct    = 36.0
		curExitStatus   = byte(0)
		opencodeCalls   atomic.Int32
	)
	srv := h.sshServer(t.TempDir())
	srv.HandleExec(func(cmd string) func(ssh.Channel) {
		if strings.Contains(cmd, "experimental.usage.limits") {
			opencodeCalls.Add(1)
			return func(ch ssh.Channel) {
				mu.Lock()
				wsID, wsName, weeklyPct, exitCode := curWSID, curWSName, curWeeklyPct, curExitStatus
				mu.Unlock()
				if exitCode != 0 {
					ch.CloseWrite()
					ch.SendRequest("exit-status", false, []byte{0, 0, 0, exitCode})
					return
				}
				wsJSON := fmt.Sprintf(`{"id":%q,"name":%q}`, wsID, wsName)
				if wsID == "" {
					wsJSON = fmt.Sprintf(`{"name":%q}`, wsName)
				}
				out := fmt.Sprintf(
					`{"location":{"directory":"/home/alice"},"data":{"workspace":%s,"go":{"rolling":{"percent":10,"limited":false,"resetsAt":%d},"weekly":{"percent":%.1f,"limited":false,"resetsAt":%d},"monthly":{"percent":38,"limited":false,"resetsAt":%d}},"budgets":[]}}`,
					wsJSON,
					now.Add(2*time.Hour).UnixMilli(),
					weeklyPct,
					now.Add(24*time.Hour).UnixMilli(),
					now.Add(15*24*time.Hour).UnixMilli(),
				)
				ch.Write([]byte(out + "\n"))
				ch.CloseWrite()
				ch.SendRequest("exit-status", false, []byte{0, 0, 0, 0})
			}
		}
		return nil
	})

	rHost := alice.do("POST", "/api/hosts", map[string]any{
		"name": "oc-box", "address": "127.0.0.1", "port": srv.Addr.Port,
		"username": "alice", "auth": "password", "password": "alice-pass", "persistence": "none",
	})
	mustStatus(t, rHost, http.StatusCreated, "create host")
	hostID := int64(rHost.json()["id"].(float64))

	rSess := alice.do("POST", "/api/sessions", map[string]any{"host_id": hostID})
	mustStatus(t, rSess, http.StatusCreated, "open session")

	// 1. Seed a Devin passive snapshot, an unfetched OpenCode host placeholder, and a Claude snapshot with weekly_scoped.
	cap50 := 50.0
	h.srv.recordSubscriptionSnapshot(aliceUser.ID, "claude", "uuid-scoped-1", "alice@example.com", hostID, "oc-box", quotaInfo{
		Known:     true,
		UpdatedAt: now.Unix(),
		Plan:      "Claude Max 20x",
		Weekly:    &quotaWindow{Used: 84, ResetsAt: now.Add(24 * time.Hour).Unix()},
		WeeklyScoped: []quotaScopedLimit{
			{Name: "Fable", Used: 0, ResetsAt: now.Add(24 * time.Hour).Unix(), IsActive: false, AllowancePctOfWeekly: &cap50},
		},
	})
	h.srv.recordSubscriptionSnapshot(aliceUser.ID, "devin", "user-devin-1", "alice@example.com", hostID, "oc-box", quotaInfo{
		Known:     true,
		UpdatedAt: now.Unix(),
		Plan:      "Devin Max",
		Weekly:    &quotaWindow{Used: 100, ResetsAt: now.Add(18 * time.Hour).Unix()},
	})
	ocPlaceholder := fmt.Sprintf("host:%d", hostID)
	if err := h.store.EnsureSubscriptionHost(context.Background(), aliceUser.ID, "opencode", ocPlaceholder, hostID, "oc-box"); err != nil {
		t.Fatal(err)
	}

	// 2. Opening /api/subscriptions makes ZERO SSH/CLI calls and returns all three items properly classified.
	rList := alice.do("GET", "/api/subscriptions", nil)
	mustStatus(t, rList, http.StatusOK, "list subscriptions")
	if opencodeCalls.Load() != 0 {
		t.Fatalf("GET /api/subscriptions must not invoke opencode CLI, got %d calls", opencodeCalls.Load())
	}
	var listResp subscriptionsListResponse
	if err := json.Unmarshal(rList.Body, &listResp); err != nil {
		t.Fatal(err)
	}
	if len(listResp.Items) != 3 {
		t.Fatalf("expected 3 items, got %d", len(listResp.Items))
	}
	byProvider := map[string]subscriptionItem{}
	for _, it := range listResp.Items {
		byProvider[it.Provider] = it
	}
	if cl := byProvider["claude"]; len(cl.Quota.WeeklyScoped) != 1 || cl.Quota.WeeklyScoped[0].Name != "Fable" || cl.Quota.WeeklyScoped[0].AllowancePctOfWeekly == nil || *cl.Quota.WeeklyScoped[0].AllowancePctOfWeekly != 50 {
		t.Fatalf("claude weekly_scoped lost in round-trip: %+v", cl.Quota)
	}
	if dv := byProvider["devin"]; dv.CanRefresh || dv.RefreshReason != "passive_only" || dv.Plan != "Devin Max" {
		t.Fatalf("devin should be passive_only: %+v", dv)
	}
	if oc := byProvider["opencode"]; !oc.CanRefresh || oc.UpdatedAt != 0 || oc.Quota.Known {
		t.Fatalf("opencode placeholder should be refreshable and unfetched: %+v", oc)
	}

	// 3. Manual refresh on Devin is rejected cleanly (passive_only).
	rDevinRef := alice.do("POST", "/api/subscriptions/refresh", map[string]any{
		"provider":    "devin",
		"account_key": "user-devin-1",
	})
	mustStatus(t, rDevinRef, http.StatusBadRequest, "devin refresh unsupported")

	// 4. Fix 2 [P1] & Fix2 Item 5 [P1]: When OpenCode service is not running (exit 75),
	//    Refresh returns 409 opencode_not_running AND keeps the host-level + card cooldown claimed
	//    so repeated attempts cannot bypass rate limits. Fails on 0f4911c.
	mu.Lock()
	curExitStatus = opencodeNotRunningExitCode
	mu.Unlock()
	callsBeforeNotRun := opencodeCalls.Load()
	rNotRun := alice.do("POST", "/api/subscriptions/refresh", map[string]any{
		"provider":    "opencode",
		"account_key": ocPlaceholder,
	})
	mustStatus(t, rNotRun, http.StatusConflict, "opencode not running")
	if rNotRun.code() != "opencode_not_running" {
		t.Fatalf("expected error=opencode_not_running, got %s", rNotRun.Body)
	}
	if opencodeCalls.Load() != callsBeforeNotRun+1 {
		t.Fatalf("expected 1 call, got %d", opencodeCalls.Load()-callsBeforeNotRun)
	}
	// Immediate retry on placeholder MUST be rate-limited BEFORE any SSH call.
	rNotRunRetry := alice.do("POST", "/api/subscriptions/refresh", map[string]any{
		"provider":    "opencode",
		"account_key": ocPlaceholder,
	})
	mustStatus(t, rNotRunRetry, http.StatusTooManyRequests, "rejected placeholder refresh must keep cooldown")
	if opencodeCalls.Load() != callsBeforeNotRun+1 {
		t.Fatalf("rate-limited placeholder retry must not run CLI again")
	}

	// Open a second host (host2) to test exit 76 (missing timeout -> opencode_unsupported).
	rHost2 := alice.do("POST", "/api/hosts", map[string]any{
		"name": "oc-box-2", "address": "127.0.0.1", "port": srv.Addr.Port,
		"username": "alice", "auth": "password", "password": "alice-pass", "persistence": "none",
	})
	mustStatus(t, rHost2, http.StatusCreated, "create host2")
	host2ID := int64(rHost2.json()["id"].(float64))
	rSess2 := alice.do("POST", "/api/sessions", map[string]any{"host_id": host2ID})
	mustStatus(t, rSess2, http.StatusCreated, "open session2")
	ocPlaceholder2 := fmt.Sprintf("host:%d", host2ID)
	if err := h.store.EnsureSubscriptionHost(context.Background(), aliceUser.ID, "opencode", ocPlaceholder2, host2ID, "oc-box-2"); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	curExitStatus = opencodeUnsupportedExitCode
	mu.Unlock()
	rUnsup := alice.do("POST", "/api/subscriptions/refresh", map[string]any{
		"provider":    "opencode",
		"account_key": ocPlaceholder2,
	})
	mustStatus(t, rUnsup, http.StatusConflict, "opencode unsupported when host lacks timeout")
	if rUnsup.code() != "opencode_unsupported" {
		t.Fatalf("expected error=opencode_unsupported, got %s", rUnsup.Body)
	}

	// Open a third host (host3) for missing workspace.id check.
	rHost3 := alice.do("POST", "/api/hosts", map[string]any{
		"name": "oc-box-3", "address": "127.0.0.1", "port": srv.Addr.Port,
		"username": "alice", "auth": "password", "password": "alice-pass", "persistence": "none",
	})
	mustStatus(t, rHost3, http.StatusCreated, "create host3")
	host3ID := int64(rHost3.json()["id"].(float64))
	rSess3 := alice.do("POST", "/api/sessions", map[string]any{"host_id": host3ID})
	mustStatus(t, rSess3, http.StatusCreated, "open session3")
	ocPlaceholder3 := fmt.Sprintf("host:%d", host3ID)
	if err := h.store.EnsureSubscriptionHost(context.Background(), aliceUser.ID, "opencode", ocPlaceholder3, host3ID, "oc-box-3"); err != nil {
		t.Fatal(err)
	}

	// 5. Fix 3 [P1]: When OpenCode returns output without workspace.id, nothing is stored.
	mu.Lock()
	curExitStatus = 0
	curWSID = ""
	mu.Unlock()
	rNoWS := alice.do("POST", "/api/subscriptions/refresh", map[string]any{
		"provider":    "opencode",
		"account_key": ocPlaceholder3,
	})
	mustStatus(t, rNoWS, http.StatusBadGateway, "opencode missing workspace id fails")
	snapsAfterNoWS, _ := h.store.ListSubscriptionSnapshots(context.Background(), aliceUser.ID)
	for _, sn := range snapsAfterNoWS {
		if sn.Provider == "opencode" && strings.HasPrefix(sn.AccountKey, "ws:") {
			t.Fatalf("must not store opencode snapshot without workspace.id, found %+v", sn)
		}
	}

	// 6. Fix 3 & Fix 5 [P1]: Valid Refresh on host4 promotes host placeholder to ws:<sha256[:16]>,
	//    drops workspace.name, labels card with host name ("oc-box-4"), and enforces per-workspace + host cooldown.
	rHost4 := alice.do("POST", "/api/hosts", map[string]any{
		"name": "oc-box-4", "address": "127.0.0.1", "port": srv.Addr.Port,
		"username": "alice", "auth": "password", "password": "alice-pass", "persistence": "none",
	})
	mustStatus(t, rHost4, http.StatusCreated, "create host4")
	host4ID := int64(rHost4.json()["id"].(float64))
	rSess4 := alice.do("POST", "/api/sessions", map[string]any{"host_id": host4ID})
	mustStatus(t, rSess4, http.StatusCreated, "open session4")
	ocPlaceholder4 := fmt.Sprintf("host:%d", host4ID)
	if err := h.store.EnsureSubscriptionHost(context.Background(), aliceUser.ID, "opencode", ocPlaceholder4, host4ID, "oc-box-4"); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	curWSID = "wrk_alpha"
	curWSName = "SECRET-WORKSPACE-NAME"
	curWeeklyPct = 36
	mu.Unlock()

	rOC := alice.do("POST", "/api/subscriptions/refresh", map[string]any{
		"provider":    "opencode",
		"account_key": ocPlaceholder4,
	})
	mustStatus(t, rOC, http.StatusOK, "opencode manual refresh")
	if strings.Contains(string(rOC.Body), "SECRET-WORKSPACE-NAME") || strings.Contains(string(rOC.Body), "wrk_alpha") {
		t.Fatalf("workspace name/raw id must not appear in response: %s", rOC.Body)
	}
	var refResp subscriptionRefreshResponse
	if err := json.Unmarshal(rOC.Body, &refResp); err != nil {
		t.Fatal(err)
	}
	wantAlphaKey := opencodeWorkspaceKey("wrk_alpha")
	if refResp.Item.AccountKey != wantAlphaKey || refResp.Item.AccountLabel != "oc-box-4" || refResp.Item.Plan != "OpenCode Go" {
		t.Fatalf("unexpected promoted opencode item: %+v (want key=%q label=oc-box-4)", refResp.Item, wantAlphaKey)
	}
	if refResp.Item.Quota.FiveHour == nil || refResp.Item.Quota.FiveHour.Used != 10 ||
		refResp.Item.Quota.Weekly == nil || refResp.Item.Quota.Weekly.Used != 36 ||
		refResp.Item.Quota.Monthly == nil || refResp.Item.Quota.Monthly.Used != 38 {
		t.Fatalf("unexpected opencode windows: %+v", refResp.Item.Quota)
	}
	// Placeholder must be removed after promotion.
	if _, err := h.store.GetSubscriptionSnapshot(context.Background(), aliceUser.ID, "opencode", ocPlaceholder4); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("expected host placeholder to be deleted after promotion, got err=%v", err)
	}

	// Immediate second refresh on ws:<alpha> hits 5-min cooldown without running CLI.
	callsBefore := opencodeCalls.Load()
	rOC2 := alice.do("POST", "/api/subscriptions/refresh", map[string]any{
		"provider":    "opencode",
		"account_key": wantAlphaKey,
	})
	mustStatus(t, rOC2, http.StatusTooManyRequests, "opencode per-workspace cooldown enforced")
	if opencodeCalls.Load() != callsBefore {
		t.Fatalf("rate-limited opencode refresh must not run CLI again")
	}

	// 7. Fix2 Item 5 [P1]: Host5 has ws:<gamma> with no active cooldown, switched to ws:<delta>.
	//    First refresh on ws:<gamma> returns 409 account_changed, preserves ws:<gamma>, saves ws:<delta>,
	//    and MUST NOT release either the per-workspace or host-level cooldown: immediate retry on
	//    ws:<gamma>, ws:<delta>, or another card on host5 is blocked with 429 BEFORE any SSH call.
	//    Fails on 0f4911c.
	rHost5 := alice.do("POST", "/api/hosts", map[string]any{
		"name": "oc-box-5", "address": "127.0.0.1", "port": srv.Addr.Port,
		"username": "alice", "auth": "password", "password": "alice-pass", "persistence": "none",
	})
	mustStatus(t, rHost5, http.StatusCreated, "create host5")
	host5ID := int64(rHost5.json()["id"].(float64))
	rSess5 := alice.do("POST", "/api/sessions", map[string]any{"host_id": host5ID})
	mustStatus(t, rSess5, http.StatusCreated, "open session5")

	wantGammaKey := opencodeWorkspaceKey("wrk_gamma")
	h.srv.recordSubscriptionSnapshot(aliceUser.ID, "opencode", wantGammaKey, "oc-box-5", host5ID, "oc-box-5", quotaInfo{
		Known:     true,
		UpdatedAt: now.Add(-10 * time.Minute).Unix(),
		Plan:      "OpenCode Go",
		Weekly:    &quotaWindow{Used: 22, ResetsAt: now.Add(24 * time.Hour).Unix()},
	})
	mu.Lock()
	curWSID = "wrk_delta"
	curWSName = "OTHER-SECRET-WS"
	curWeeklyPct = 77
	mu.Unlock()

	callsBeforeSwitch := opencodeCalls.Load()
	rSwitch := alice.do("POST", "/api/subscriptions/refresh", map[string]any{
		"provider":    "opencode",
		"account_key": wantGammaKey,
	})
	mustStatus(t, rSwitch, http.StatusConflict, "workspace switch returns account_changed")
	if rSwitch.code() != "account_changed" {
		t.Fatalf("expected error=account_changed, got %s", rSwitch.Body)
	}
	if opencodeCalls.Load() != callsBeforeSwitch+1 {
		t.Fatalf("expected exactly 1 CLI call on switch, got %d", opencodeCalls.Load()-callsBeforeSwitch)
	}
	// Workspace gamma must remain untouched at 22% AND its cooldown must NOT have been rolled back.
	snGammaAfter, err := h.store.GetSubscriptionSnapshot(context.Background(), aliceUser.ID, "opencode", wantGammaKey)
	if err != nil {
		t.Fatal(err)
	}
	if snGammaAfter.LastRefreshAt == 0 {
		t.Fatalf("opencode workspace cooldown must NOT be released on account_changed")
	}
	var qGamma quotaInfo
	_ = json.Unmarshal(snGammaAfter.QuotaJSON, &qGamma)
	if qGamma.Weekly == nil || qGamma.Weekly.Used != 22 {
		t.Fatalf("workspace gamma was overwritten! got %+v", qGamma)
	}
	// Workspace delta must be saved under its own key ws:<delta> with host label.
	wantDeltaKey := opencodeWorkspaceKey("wrk_delta")
	snDelta, err := h.store.GetSubscriptionSnapshot(context.Background(), aliceUser.ID, "opencode", wantDeltaKey)
	if err != nil {
		t.Fatalf("expected workspace delta saved under %q: %v", wantDeltaKey, err)
	}
	if snDelta.AccountLabel != "oc-box-5" || snDelta.LastRefreshAt == 0 {
		t.Fatalf("unexpected workspace delta metadata: %+v", snDelta)
	}
	var qDelta quotaInfo
	_ = json.Unmarshal(snDelta.QuotaJSON, &qDelta)
	if qDelta.Weekly == nil || qDelta.Weekly.Used != 77 {
		t.Fatalf("unexpected workspace delta quota: %+v", qDelta)
	}

	// Immediate repeated refresh on gamma MUST return 429 rate_limited without running CLI.
	rSwitchRetry := alice.do("POST", "/api/subscriptions/refresh", map[string]any{
		"provider":    "opencode",
		"account_key": wantGammaKey,
	})
	mustStatus(t, rSwitchRetry, http.StatusTooManyRequests, "repeated refresh on gamma after account_changed must be rate limited")
	if opencodeCalls.Load() != callsBeforeSwitch+1 {
		t.Fatalf("repeated refresh after account_changed bypassed cooldown! calls=%d", opencodeCalls.Load()-callsBeforeSwitch)
	}

	// Even if a fresh un-cooled workspace card (ws:<epsilon>) points at host5, host-level cooldown
	// MUST block it with 429 BEFORE any remote call.
	wantEpsilonKey := opencodeWorkspaceKey("wrk_epsilon")
	h.srv.recordSubscriptionSnapshot(aliceUser.ID, "opencode", wantEpsilonKey, "oc-box-5", host5ID, "oc-box-5", quotaInfo{
		Known:     true,
		UpdatedAt: now.Add(-10 * time.Minute).Unix(),
		Plan:      "OpenCode Go",
		Weekly:    &quotaWindow{Used: 5, ResetsAt: now.Add(24 * time.Hour).Unix()},
	})
	rHostBlocked := alice.do("POST", "/api/subscriptions/refresh", map[string]any{
		"provider":    "opencode",
		"account_key": wantEpsilonKey,
	})
	mustStatus(t, rHostBlocked, http.StatusTooManyRequests, "host-level cooldown must block another workspace card on same host")
	if opencodeCalls.Load() != callsBeforeSwitch+1 {
		t.Fatalf("host-level cooldown did not prevent SSH call! calls=%d", opencodeCalls.Load()-callsBeforeSwitch)
	}
}
