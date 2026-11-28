package store

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

func TestCorraldHostHintsLifecycleGenerationAndStaleWrites(t *testing.T) {
	st, err := OpenMemory()
	if err != nil {
		t.Fatalf("OpenMemory: %v", err)
	}
	defer st.Close()

	now := time.UnixMilli(1_700_000_000_000)
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
	if _, err := st.MarkCorraldHintShown(ctx, uidB, hidA1, 0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-user MarkCorraldHintShown: got %v, want ErrNotFound", err)
	}
	if _, err := st.MarkCorraldHintDismissed(ctx, uidB, hidA1, 0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-user MarkCorraldHintDismissed: got %v, want ErrNotFound", err)
	}

	// Negative or future generation rejected.
	if _, err := st.MarkCorraldHintShown(ctx, uidA, hidA1, -1); !errors.Is(err, ErrInvalidHintGeneration) {
		t.Fatalf("negative gen: got %v, want ErrInvalidHintGeneration", err)
	}
	if _, err := st.MarkCorraldHintShown(ctx, uidA, hidA1, 999); !errors.Is(err, ErrInvalidHintGeneration) {
		t.Fatalf("future gen: got %v, want ErrInvalidHintGeneration", err)
	}

	// Mark shown on host A1 under generation 0.
	applied, err := st.MarkCorraldHintShown(ctx, uidA, hidA1, 0)
	if err != nil || !applied {
		t.Fatalf("MarkCorraldHintShown: (%v, %v)", applied, err)
	}
	got, err := st.CorraldHostHint(ctx, uidA, hidA1)
	if err != nil {
		t.Fatalf("CorraldHostHint after shown: %v", err)
	}
	if got.ShownAt != 1_700_000_000_000 || got.DismissedAt != 0 {
		t.Fatalf("unexpected row after shown: %+v", got)
	}
	if !CorraldHintRowActive(got, 0) {
		t.Fatal("row should be active under resetAt=0")
	}

	// Second MarkCorraldHintShown in same generation preserves original shown_at.
	now = now.Add(60 * time.Second)
	applied, err = st.MarkCorraldHintShown(ctx, uidA, hidA1, 0)
	if err != nil || !applied {
		t.Fatalf("second MarkCorraldHintShown: (%v, %v)", applied, err)
	}
	got, err = st.CorraldHostHint(ctx, uidA, hidA1)
	if err != nil {
		t.Fatalf("CorraldHostHint after second shown: %v", err)
	}
	if got.ShownAt != 1_700_000_000_000 || got.DismissedAt != 0 {
		t.Fatalf("second shown should preserve timestamp: %+v", got)
	}

	// Dismiss host A1 later: preserves original shown_at and sets dismissed_at.
	applied, err = st.MarkCorraldHintDismissed(ctx, uidA, hidA1, 0)
	if err != nil || !applied {
		t.Fatalf("MarkCorraldHintDismissed a1: (%v, %v)", applied, err)
	}
	got, err = st.CorraldHostHint(ctx, uidA, hidA1)
	if err != nil {
		t.Fatalf("CorraldHostHint after dismiss: %v", err)
	}
	if got.ShownAt != 1_700_000_000_000 || got.DismissedAt != 1_700_000_060_000 {
		t.Fatalf("unexpected row after dismiss: %+v", got)
	}

	// Dismiss A2 and mark Bob's host shown.
	if _, err := st.MarkCorraldHintDismissed(ctx, uidA, hA2.ID, 0); err != nil {
		t.Fatalf("MarkCorraldHintDismissed a2: %v", err)
	}
	if _, err := st.MarkCorraldHintShown(ctx, uidB, hidB, 0); err != nil {
		t.Fatalf("MarkCorraldHintShown bob: %v", err)
	}

	// Advance alice's reset generation atomically via MergeUserSettingsWithHintMax
	// WITHOUT deleting any corrald_host_hints rows.
	var gen1 int64
	if err := st.MergeUserSettingsWithHintMax(ctx, uidA, func(old string, maxHintTS int64) (string, error) {
		if maxHintTS != 1_700_000_060_000 {
			t.Fatalf("expected maxHintTS=1700000060000, got %d", maxHintTS)
		}
		gen1 = maxHintTS + 1
		b, _ := json.Marshal(map[string]any{
			"corraldHints":       true,
			CorraldHintsResetKey: gen1,
		})
		return string(b), nil
	}); err != nil {
		t.Fatalf("MergeUserSettingsWithHintMax: %v", err)
	}

	// Rows are NOT deleted, yet are no longer active under gen1.
	listA, err := st.ListCorraldHostHints(ctx, uidA)
	if err != nil {
		t.Fatalf("ListCorraldHostHints alice: %v", err)
	}
	if len(listA) != 2 {
		t.Fatalf("expected 2 preserved rows for alice (nothing deleted), got %d", len(listA))
	}
	rA1 := listA[hidA1]
	if CorraldHintRowActive(&rA1, gen1) {
		t.Fatalf("pre-reset row must not be active under gen1=%d: %+v", gen1, rA1)
	}

	// Stale generation (0 < gen1) POSTs are ignored with applied=false and no write.
	applied, err = st.MarkCorraldHintShown(ctx, uidA, hidA1, 0)
	if err != nil || applied {
		t.Fatalf("stale MarkCorraldHintShown: got (%v, %v), want (false, nil)", applied, err)
	}
	applied, err = st.MarkCorraldHintDismissed(ctx, uidA, hidA1, 0)
	if err != nil || applied {
		t.Fatalf("stale MarkCorraldHintDismissed: got (%v, %v), want (false, nil)", applied, err)
	}
	afterStale, _ := st.CorraldHostHint(ctx, uidA, hidA1)
	if afterStale.ShownAt != 1_700_000_000_000 || afterStale.DismissedAt != 1_700_000_060_000 {
		t.Fatalf("stale write mutated row: %+v", afterStale)
	}

	// Current generation (gen1) updates shown_at to >= gen1 so it becomes active again.
	applied, err = st.MarkCorraldHintShown(ctx, uidA, hidA1, gen1)
	if err != nil || !applied {
		t.Fatalf("current gen MarkCorraldHintShown: (%v, %v)", applied, err)
	}
	afterNew, _ := st.CorraldHostHint(ctx, uidA, hidA1)
	if !CorraldHintRowActive(afterNew, gen1) || afterNew.ShownAt < gen1 {
		t.Fatalf("row after gen1 shown should be active with shown_at>=%d: %+v", gen1, afterNew)
	}

	// Bob's generation and row remain unaffected.
	listB, err := st.ListCorraldHostHints(ctx, uidB)
	if err != nil {
		t.Fatalf("ListCorraldHostHints bob: %v", err)
	}
	rB := listB[hidB]
	if !CorraldHintRowActive(&rB, 0) {
		t.Fatalf("bob row must remain active: %+v", rB)
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
		if _, err := st.MarkCorraldHintDismissed(ctx, pair[0], pair[1], 0); err != nil {
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
