package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestCorraldHostHintsLifecycleAndReset(t *testing.T) {
	st, err := OpenMemory()
	if err != nil {
		t.Fatalf("OpenMemory: %v", err)
	}
	defer st.Close()

	now := time.Unix(1_700_000_000, 0)
	st.now = func() time.Time { return now }
	ctx := context.Background()

	uidA, hidA1 := seedTestUserAndHost(t, st, "alice", "box-a1")
	hA2, err := st.CreateHost(ctx, &Host{OwnerID: uidA, Name: "box-a2", Address: "127.0.0.1", Port: 2222, Username: "dev", Auth: "ask"})
	if err != nil {
		t.Fatalf("CreateHost a2: %v", err)
	}
	uidB, hidB := seedTestUserAndHost(t, st, "bob", "box-b1")

	// Missing initially.
	if _, err := st.CorraldHostHint(ctx, uidA, hidA1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("initial CorraldHostHint: got %v, want ErrNotFound", err)
	}

	// Cross-user mark is refused with ErrNotFound.
	if err := st.MarkCorraldHintShown(ctx, uidB, hidA1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-user MarkCorraldHintShown: got %v, want ErrNotFound", err)
	}
	if err := st.MarkCorraldHintDismissed(ctx, uidB, hidA1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-user MarkCorraldHintDismissed: got %v, want ErrNotFound", err)
	}

	// Mark shown on host A1.
	if err := st.MarkCorraldHintShown(ctx, uidA, hidA1); err != nil {
		t.Fatalf("MarkCorraldHintShown: %v", err)
	}
	got, err := st.CorraldHostHint(ctx, uidA, hidA1)
	if err != nil {
		t.Fatalf("CorraldHostHint after shown: %v", err)
	}
	if got.ShownAt != 1_700_000_000 || got.DismissedAt != 0 {
		t.Fatalf("unexpected row after shown: %+v", got)
	}

	// Second MarkCorraldHintShown preserves original shown_at.
	now = now.Add(60 * time.Second)
	if err := st.MarkCorraldHintShown(ctx, uidA, hidA1); err != nil {
		t.Fatalf("second MarkCorraldHintShown: %v", err)
	}
	got, err = st.CorraldHostHint(ctx, uidA, hidA1)
	if err != nil {
		t.Fatalf("CorraldHostHint after second shown: %v", err)
	}
	if got.ShownAt != 1_700_000_000 || got.DismissedAt != 0 {
		t.Fatalf("second shown should preserve timestamp: %+v", got)
	}

	// Dismiss host A1 later: preserves original shown_at and sets dismissed_at.
	if err := st.MarkCorraldHintDismissed(ctx, uidA, hidA1); err != nil {
		t.Fatalf("MarkCorraldHintDismissed a1: %v", err)
	}
	got, err = st.CorraldHostHint(ctx, uidA, hidA1)
	if err != nil {
		t.Fatalf("CorraldHostHint after dismiss: %v", err)
	}
	if got.ShownAt != 1_700_000_000 || got.DismissedAt != 1_700_000_060 {
		t.Fatalf("unexpected row after dismiss: %+v", got)
	}

	// Dismiss directly on host A2 (without prior shown) sets both timestamps.
	if err := st.MarkCorraldHintDismissed(ctx, uidA, hA2.ID); err != nil {
		t.Fatalf("MarkCorraldHintDismissed a2: %v", err)
	}
	// Mark Bob's host shown to verify user isolation on reset.
	if err := st.MarkCorraldHintShown(ctx, uidB, hidB); err != nil {
		t.Fatalf("MarkCorraldHintShown bob: %v", err)
	}

	listA, err := st.ListCorraldHostHints(ctx, uidA)
	if err != nil {
		t.Fatalf("ListCorraldHostHints alice: %v", err)
	}
	if len(listA) != 2 {
		t.Fatalf("expected 2 hint rows for alice, got %d", len(listA))
	}

	// Resetting alice's hints clears all of alice's rows but leaves bob's intact.
	if err := st.ResetCorraldHintsForUser(ctx, uidA); err != nil {
		t.Fatalf("ResetCorraldHintsForUser alice: %v", err)
	}
	listA, err = st.ListCorraldHostHints(ctx, uidA)
	if err != nil {
		t.Fatalf("ListCorraldHostHints alice after reset: %v", err)
	}
	if len(listA) != 0 {
		t.Fatalf("expected 0 hint rows for alice after reset, got %d", len(listA))
	}
	listB, err := st.ListCorraldHostHints(ctx, uidB)
	if err != nil {
		t.Fatalf("ListCorraldHostHints bob after alice reset: %v", err)
	}
	if len(listB) != 1 || listB[hidB].ShownAt == 0 {
		t.Fatalf("bob hint row must remain untouched: %+v", listB)
	}
}

func TestCorraldHostHintsCascadeDeleteHostAndUser(t *testing.T) {
	st, err := OpenMemory()
	if err != nil {
		t.Fatalf("OpenMemory: %v", err)
	}
	defer st.Close()
	ctx := context.Background()

	uidA, hidA1 := seedTestUserAndHost(t, st, "alice", "box-a1")
	hA2, err := st.CreateHost(ctx, &Host{OwnerID: uidA, Name: "box-a2", Address: "127.0.0.1", Port: 2222, Username: "dev", Auth: "ask"})
	if err != nil {
		t.Fatalf("CreateHost a2: %v", err)
	}
	uidB, hidB := seedTestUserAndHost(t, st, "bob", "box-b1")

	for _, pair := range [][2]int64{{uidA, hidA1}, {uidA, hA2.ID}, {uidB, hidB}} {
		if err := st.MarkCorraldHintDismissed(ctx, pair[0], pair[1]); err != nil {
			t.Fatalf("MarkCorraldHintDismissed(%v): %v", pair, err)
		}
	}

	// Deleting host A1 must remove its hint row and keep A2 + B.
	if err := st.DeleteHost(ctx, uidA, hidA1); err != nil {
		t.Fatalf("DeleteHost a1: %v", err)
	}
	if _, err := st.CorraldHostHint(ctx, uidA, hidA1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("hint for deleted host a1 still present: %v", err)
	}
	if _, err := st.CorraldHostHint(ctx, uidA, hA2.ID); err != nil {
		t.Fatalf("hint for surviving host a2 lost: %v", err)
	}

	// Deleting user B directly cascades to corrald_host_hints (and hosts).
	if _, err := st.db.ExecContext(ctx, `DELETE FROM users WHERE id = ?`, uidB); err != nil {
		t.Fatalf("DELETE user bob: %v", err)
	}
	var countB int
	if err := st.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM corrald_host_hints WHERE user_id = ?`, uidB).Scan(&countB); err != nil {
		t.Fatalf("count bob hints: %v", err)
	}
	if countB != 0 {
		t.Fatalf("expected 0 hint rows after deleting user bob, got %d", countB)
	}

	// Alice's surviving host hint is still intact.
	var countA int
	if err := st.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM corrald_host_hints WHERE user_id = ?`, uidA).Scan(&countA); err != nil {
		t.Fatalf("count alice hints: %v", err)
	}
	if countA != 1 {
		t.Fatalf("expected 1 surviving hint row for alice, got %d", countA)
	}
}
