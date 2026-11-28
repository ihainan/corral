package store

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

func TestSubscriptionSnapshotsDedupAndOrdering(t *testing.T) {
	s, err := OpenMemory()
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	now := time.Date(2026, 11, 28, 12, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }

	ctx := context.Background()
	u1, err := s.CreateUser(ctx, "alice", "Alice", "hash", RoleMember)
	if err != nil {
		t.Fatal(err)
	}
	u2, err := s.CreateUser(ctx, "bob", "Bob", "hash", RoleMember)
	if err != nil {
		t.Fatal(err)
	}

	// Host 1 reports Claude account acc-1 at T-10m.
	if err := s.UpsertSubscriptionSnapshot(ctx, SubscriptionSnapshot{
		OwnerID:      u1.ID,
		Provider:     "claude",
		AccountKey:   "acc-1",
		AccountLabel: "alice@example.com",
		Plan:         "Claude Max 20x",
		HostID:       10,
		HostName:     "dev-box-a",
		QuotaJSON:    json.RawMessage(`{"known":true,"five_hour":{"used":25,"resets_at":1800000000}}`),
		UpdatedAt:    now.Add(-10 * time.Minute).Unix(),
		SeenAt:       now.Unix(),
	}); err != nil {
		t.Fatal(err)
	}

	// Host 2 reports the SAME Claude account acc-1 with a NEWER reading at T-2m -> deduplicates and updates quota + merges hosts.
	if err := s.UpsertSubscriptionSnapshot(ctx, SubscriptionSnapshot{
		OwnerID:    u1.ID,
		Provider:   "claude",
		AccountKey: "acc-1",
		HostID:     20,
		HostName:   "dev-box-b",
		QuotaJSON:  json.RawMessage(`{"known":true,"five_hour":{"used":42,"resets_at":1800000000}}`),
		UpdatedAt:  now.Add(-2 * time.Minute).Unix(),
		SeenAt:     now.Add(1 * time.Second).Unix(),
	}); err != nil {
		t.Fatal(err)
	}

	// Host 3 reports an OLDER cached reading for acc-1 (T-1h) -> must NOT overwrite newer quota, but DOES merge host into hosts list.
	if err := s.UpsertSubscriptionSnapshot(ctx, SubscriptionSnapshot{
		OwnerID:    u1.ID,
		Provider:   "claude",
		AccountKey: "acc-1",
		HostID:     30,
		HostName:   "dev-box-c",
		QuotaJSON:  json.RawMessage(`{"known":true,"five_hour":{"used":5,"resets_at":1800000000}}`),
		UpdatedAt:  now.Add(-1 * time.Hour).Unix(),
		SeenAt:     now.Add(2 * time.Second).Unix(),
	}); err != nil {
		t.Fatal(err)
	}

	list, err := s.ListSubscriptionSnapshots(ctx, u1.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("expected 1 deduplicated snapshot, got %d", len(list))
	}
	got := list[0]
	if got.HostID != 20 || got.HostName != "dev-box-b" {
		t.Errorf("expected latest host dev-box-b (20), got %s (%d)", got.HostName, got.HostID)
	}
	if got.AccountLabel != "alice@example.com" || got.Plan != "Claude Max 20x" {
		t.Errorf("expected preserved label and plan, got label=%q plan=%q", got.AccountLabel, got.Plan)
	}
	if len(got.Hosts) != 3 {
		t.Fatalf("expected 3 merged hosts, got %+v", got.Hosts)
	}
	if string(got.QuotaJSON) != `{"known":true,"five_hour":{"used":42,"resets_at":1800000000}}` {
		t.Errorf("older snapshot overwrote newer quota: %s", got.QuotaJSON)
	}

	// Cross-user isolation: bob sees nothing and cannot get alice's snapshot.
	bobList, err := s.ListSubscriptionSnapshots(ctx, u2.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(bobList) != 0 {
		t.Fatalf("expected 0 snapshots for bob, got %d", len(bobList))
	}
	if _, err := s.GetSubscriptionSnapshot(ctx, u2.ID, "claude", "acc-1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound for cross-user get, got %v", err)
	}
}

func TestSubscriptionRefreshCooldown(t *testing.T) {
	s, err := OpenMemory()
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	now := time.Date(2026, 11, 28, 12, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }

	ctx := context.Background()
	u, err := s.CreateUser(ctx, "alice", "Alice", "hash", RoleMember)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertSubscriptionSnapshot(ctx, SubscriptionSnapshot{
		OwnerID:    u.ID,
		Provider:   "claude",
		AccountKey: "acc-1",
		HostID:     1,
		HostName:   "box",
		QuotaJSON:  json.RawMessage(`{"known":true}`),
		UpdatedAt:  now.Unix(),
		SeenAt:     now.Unix(),
	}); err != nil {
		t.Fatal(err)
	}

	const cooldown = 300
	allowed, retry, nextAt, err := s.TryClaimSubscriptionRefresh(ctx, u.ID, "claude", "acc-1", cooldown)
	if err != nil || !allowed || retry != 0 || nextAt != now.Unix()+cooldown {
		t.Fatalf("first claim: allowed=%v retry=%d nextAt=%d err=%v", allowed, retry, nextAt, err)
	}

	// 2 minutes later: still blocked with 180s remaining.
	now = now.Add(2 * time.Minute)
	allowed, retry, nextAt, err = s.TryClaimSubscriptionRefresh(ctx, u.ID, "claude", "acc-1", cooldown)
	if err != nil || allowed || retry != 180 {
		t.Fatalf("second claim at +2m: allowed=%v retry=%d nextAt=%d err=%v", allowed, retry, nextAt, err)
	}

	// 4m59s after first claim: still blocked with 1s remaining.
	now = now.Add(2*time.Minute + 59*time.Second)
	allowed, retry, _, err = s.TryClaimSubscriptionRefresh(ctx, u.ID, "claude", "acc-1", cooldown)
	if err != nil || allowed || retry != 1 {
		t.Fatalf("third claim at +4m59s: allowed=%v retry=%d err=%v", allowed, retry, err)
	}

	// Exactly 5 minutes after first claim: allowed again.
	now = now.Add(1 * time.Second)
	allowed, retry, nextAt, err = s.TryClaimSubscriptionRefresh(ctx, u.ID, "claude", "acc-1", cooldown)
	if err != nil || !allowed || retry != 0 || nextAt != now.Unix()+cooldown {
		t.Fatalf("fourth claim at +5m: allowed=%v retry=%d nextAt=%d err=%v", allowed, retry, nextAt, err)
	}
}
