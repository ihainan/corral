package store

import (
	"context"
	"testing"
)

func seedTestUserAndHost(t *testing.T, st *Store, username, hostName string) (int64, int64) {
	t.Helper()
	ctx := context.Background()
	u, err := st.CreateUser(ctx, username, username, "hash", RoleMember)
	if err != nil {
		t.Fatalf("CreateUser(%s): %v", username, err)
	}
	h := &Host{
		OwnerID:  u.ID,
		Name:     hostName,
		Address:  "127.0.0.1",
		Port:     22,
		Username: "dev",
		Auth:     "ask",
	}
	created, err := st.CreateHost(ctx, h)
	if err != nil {
		t.Fatalf("CreateHost(%s): %v", hostName, err)
	}
	return u.ID, created.ID
}

func TestCorraldPerfDailyBatchPruneAndHostDelete(t *testing.T) {
	st, err := OpenMemory()
	if err != nil {
		t.Fatalf("OpenMemory: %v", err)
	}
	defer st.Close()
	ctx := context.Background()

	uid, hid := seedTestUserAndHost(t, st, "alice", "box-1")

	// Batch 1 + unknown host (must be skipped safely without failing the batch).
	err = st.AddCorraldPerfDailyBatch(ctx, []CorraldPerfDailyDelta{
		{HostID: hid, Day: "2026-07-01", Op: "send", Via: "ssh", Count: 5, TotalUs: 5_000_000},
		{HostID: hid, Day: "2026-10-15", Op: "send", Via: "ssh", Count: 12, TotalUs: 24_000_000},
		{HostID: hid, Day: "2026-10-16", Op: "card", Via: "ssh", Count: 10, TotalUs: 18_000_000},
		{HostID: hid, Day: "2026-11-20", Op: "send", Via: "corrald", Count: 25, TotalUs: 7_500_000},
		{HostID: 999999, Day: "2026-11-20", Op: "send", Via: "ssh", Count: 1, TotalUs: 1000},
	})
	if err != nil {
		t.Fatalf("AddCorraldPerfDailyBatch #1: %v", err)
	}

	// Batch 2 adds onto existing (host, day, op, via).
	err = st.AddCorraldPerfDailyBatch(ctx, []CorraldPerfDailyDelta{
		{HostID: hid, Day: "2026-10-15", Op: "send", Via: "ssh", Count: 8, TotalUs: 14_000_000},
	})
	if err != nil {
		t.Fatalf("AddCorraldPerfDailyBatch #2: %v", err)
	}

	// Prune strictly older than 2026-08-30 (90-day cutoff).
	pruned, err := st.PruneCorraldPerfDaily(ctx, "2026-08-30")
	if err != nil {
		t.Fatalf("PruneCorraldPerfDaily: %v", err)
	}
	if pruned != 1 {
		t.Fatalf("expected 1 pruned row, got %d", pruned)
	}

	sshTotals, err := st.CorraldPerfDailyTotals(ctx, hid, "ssh", "2026-10-01", "2026-11-19")
	if err != nil {
		t.Fatalf("CorraldPerfDailyTotals ssh: %v", err)
	}
	if got := sshTotals["send"]; got.Count != 20 || got.TotalUs != 38_000_000 {
		t.Fatalf("unexpected ssh send totals: %+v", got)
	}
	if got := sshTotals["card"]; got.Count != 10 || got.TotalUs != 18_000_000 {
		t.Fatalf("unexpected ssh card totals: %+v", got)
	}

	// Deleting the host must cascade-delete its daily perf rows.
	if err := st.DeleteHost(ctx, uid, hid); err != nil {
		t.Fatalf("DeleteHost: %v", err)
	}
	rows, err := st.CorraldPerfDailyRows(ctx, hid)
	if err != nil {
		t.Fatalf("CorraldPerfDailyRows after delete: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("expected 0 rows after host delete, got %d", len(rows))
	}
}

func TestCorraldUpdateNoticesAndChangelogSeen(t *testing.T) {
	st, err := OpenMemory()
	if err != nil {
		t.Fatalf("OpenMemory: %v", err)
	}
	defer st.Close()
	ctx := context.Background()

	uidA, hid1 := seedTestUserAndHost(t, st, "alice", "alpha")
	h2 := &Host{OwnerID: uidA, Name: "beta", Address: "127.0.0.1", Port: 2222, Username: "dev", Auth: "ask"}
	created2, err := st.CreateHost(ctx, h2)
	if err != nil {
		t.Fatalf("CreateHost beta: %v", err)
	}
	uidB, hidB := seedTestUserAndHost(t, st, "bob", "bob-box")

	err = st.RecordCorraldUpdateNotices(ctx, []CorraldUpdateNotice{
		{UserID: uidA, HostID: hid1, OldVersion: "0.3.1", NewVersion: "0.3.2", NewRelease: 2000, Success: true},
		{UserID: uidA, HostID: created2.ID, OldVersion: "0.3.1", NewVersion: "0.3.2", NewRelease: 2000, Success: false},
		{UserID: uidB, HostID: hidB, OldVersion: "0.3.1", NewVersion: "0.3.2", NewRelease: 2000, Success: true},
	})
	if err != nil {
		t.Fatalf("RecordCorraldUpdateNotices: %v", err)
	}

	listA, err := st.ListUnseenCorraldUpdateNotices(ctx, uidA)
	if err != nil {
		t.Fatalf("ListUnseen A: %v", err)
	}
	if len(listA) != 2 {
		t.Fatalf("expected 2 notices for A, got %d", len(listA))
	}
	if listA[0].HostName != "alpha" || !listA[0].Success {
		t.Fatalf("unexpected first notice: %+v", listA[0])
	}
	if listA[1].HostName != "beta" || listA[1].Success {
		t.Fatalf("unexpected second notice: %+v", listA[1])
	}

	// Bob attempting to mark Alice's notices seen must not affect Alice.
	if err := st.MarkCorraldUpdateNoticesSeen(ctx, uidB, []int64{listA[0].ID, listA[1].ID}); err != nil {
		t.Fatalf("MarkSeen cross-user: %v", err)
	}
	listA2, _ := st.ListUnseenCorraldUpdateNotices(ctx, uidA)
	if len(listA2) != 2 {
		t.Fatalf("cross-user mark affected Alice: got %d", len(listA2))
	}

	// Alice marks her notices seen.
	if err := st.MarkCorraldUpdateNoticesSeen(ctx, uidA, []int64{listA[0].ID, listA[1].ID}); err != nil {
		t.Fatalf("MarkSeen A: %v", err)
	}
	listA3, _ := st.ListUnseenCorraldUpdateNotices(ctx, uidA)
	if len(listA3) != 0 {
		t.Fatalf("expected 0 unseen notices for A, got %d", len(listA3))
	}

	// Deleting Bob's host cascades its unseen notice.
	if err := st.DeleteHost(ctx, uidB, hidB); err != nil {
		t.Fatalf("DeleteHost B: %v", err)
	}
	listB, _ := st.ListUnseenCorraldUpdateNotices(ctx, uidB)
	if len(listB) != 0 {
		t.Fatalf("expected 0 notices for B after host delete, got %d", len(listB))
	}

	// Changelog seen cursor round-trip.
	seen0, err := st.GetCorraldChangelogSeen(ctx, uidA)
	if err != nil || seen0 != nil {
		t.Fatalf("expected nil initial changelog seen, got (%+v, %v)", seen0, err)
	}
	if err := st.SetCorraldChangelogSeen(ctx, uidA, "0.3.2", 2000); err != nil {
		t.Fatalf("SetCorraldChangelogSeen: %v", err)
	}
	seen1, err := st.GetCorraldChangelogSeen(ctx, uidA)
	if err != nil || seen1 == nil || seen1.LastVersion != "0.3.2" || seen1.LastRelease != 2000 {
		t.Fatalf("unexpected changelog seen: (%+v, %v)", seen1, err)
	}
}

func TestSetAppSettingUpsert(t *testing.T) {
	st, err := OpenMemory()
	if err != nil {
		t.Fatalf("OpenMemory: %v", err)
	}
	defer st.Close()
	ctx := context.Background()

	if err := st.SetAppSetting(ctx, "corrald_policy", "disabled"); err != nil {
		t.Fatalf("SetAppSetting disabled: %v", err)
	}
	v, ok, err := st.AppSetting(ctx, "corrald_policy")
	if err != nil || !ok || v != "disabled" {
		t.Fatalf("got (%q, %v, %v), want disabled", v, ok, err)
	}
	if err := st.SetAppSetting(ctx, "corrald_policy", "allowed"); err != nil {
		t.Fatalf("SetAppSetting allowed: %v", err)
	}
	v, ok, err = st.AppSetting(ctx, "corrald_policy")
	if err != nil || !ok || v != "allowed" {
		t.Fatalf("got (%q, %v, %v), want allowed", v, ok, err)
	}
}
