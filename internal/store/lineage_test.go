package store

import (
	"context"
	"errors"
	"testing"
)

func TestAgentLineageLifecycle(t *testing.T) {
	st, err := OpenMemory()
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()

	// Insert initial tool_log edge with empty child_term_id.
	changed, err := st.UpsertAgentLineage(ctx, AgentLineage{
		OwnerID:     1,
		HostID:      10,
		Server:      "srv-a",
		ChildPane:   "w1:p2",
		ParentPane:  "w1:p1",
		Source:      LineageToolLog,
		State:       LineageConfirmed,
		EvidenceKey: "toolu_1",
		CreatedAt:   1000,
		UpdatedAt:   1000,
	})
	if err != nil || !changed {
		t.Fatalf("initial upsert: changed=%v err=%v", changed, err)
	}

	// Duplicate snapshot with identical data should be a no-op (changed=false).
	changed, err = st.UpsertAgentLineage(ctx, AgentLineage{
		OwnerID:     1,
		HostID:      10,
		Server:      "srv-a",
		ChildPane:   "w1:p2",
		ParentPane:  "w1:p1",
		Source:      LineageToolLog,
		State:       LineageConfirmed,
		EvidenceKey: "toolu_1",
		CreatedAt:   2000,
		UpdatedAt:   2000,
	})
	if err != nil || changed {
		t.Fatalf("idempotent upsert: changed=%v err=%v", changed, err)
	}

	// Upgrade empty child_term_id -> concrete term_id and higher-priority metadata source.
	changed, err = st.UpsertAgentLineage(ctx, AgentLineage{
		OwnerID:       1,
		HostID:        10,
		Server:        "srv-a",
		ChildPane:     "w1:p2",
		ChildTermID:   "term-2",
		ChildSession:  "sess-2",
		ParentPane:    "w1:p1",
		ParentTermID:  "term-1",
		ParentSession: "sess-1",
		Source:        LineageMetadata,
		State:         LineageConfirmed,
		UpdatedAt:     3000,
	})
	if err != nil || !changed {
		t.Fatalf("upgrade upsert: changed=%v err=%v", changed, err)
	}

	rows, err := st.ListAgentLineage(ctx, 1, 10, "srv-a")
	if err != nil || len(rows) != 1 {
		t.Fatalf("list after upgrade: len=%d err=%v rows=%+v", len(rows), err, rows)
	}
	if rows[0].ChildTermID != "term-2" || rows[0].Source != LineageMetadata || rows[0].EvidenceKey != "toolu_1" {
		t.Fatalf("unexpected upgraded row: %+v", rows[0])
	}

	// Another parent with equal/lower priority cannot hijack child or reuse evidence_key.
	changed, err = st.UpsertAgentLineage(ctx, AgentLineage{
		OwnerID:      1,
		HostID:       10,
		Server:       "srv-a",
		ChildPane:    "w1:p2",
		ChildTermID:  "term-2",
		ParentPane:   "w1:p9",
		ParentTermID: "term-9",
		Source:       LineageMetadata,
		State:        LineageConfirmed,
		UpdatedAt:    3500,
	})
	if err != nil || changed {
		t.Fatalf("equal-priority hijack should be ignored: changed=%v err=%v", changed, err)
	}

	// Forked session with same evidence_key cannot create a competing edge.
	changed, err = st.UpsertAgentLineage(ctx, AgentLineage{
		OwnerID:     1,
		HostID:      10,
		Server:      "srv-a",
		ChildPane:   "w1:p3",
		ParentPane:  "w1:p9",
		Source:      LineageToolLog,
		State:       LineageConfirmed,
		EvidenceKey: "toolu_1",
		UpdatedAt:   3600,
	})
	if err != nil || changed {
		t.Fatalf("duplicate evidence_key hijack should be ignored: changed=%v err=%v", changed, err)
	}

	// Owner isolation: owner 2 sees nothing.
	other, err := st.ListAgentLineage(ctx, 2, 10, "srv-a")
	if err != nil || len(other) != 0 {
		t.Fatalf("owner isolation violated: %+v %v", other, err)
	}

	// Cycle detection: p1 -> p2 would form p1 -> p2 -> p1.
	_, err = st.UpsertAgentLineage(ctx, AgentLineage{
		OwnerID:      1,
		HostID:       10,
		Server:       "srv-a",
		ChildPane:    "w1:p1",
		ChildTermID:  "term-1",
		ParentPane:   "w1:p2",
		ParentTermID: "term-2",
		Source:       LineageManual,
		State:        LineageConfirmed,
		UpdatedAt:    4000,
	})
	if !errors.Is(err, ErrLineageCycle) {
		t.Fatalf("expected ErrLineageCycle, got %v", err)
	}

	// Migrate both parent and child panes when terminals move.
	moved, err := st.MigrateAgentLineagePanes(ctx, 1, 10, "srv-a", map[string]string{
		"term-1": "w9:p1",
		"term-2": "w9:p2",
	}, 5000)
	if err != nil || !moved {
		t.Fatalf("migrate panes: moved=%v err=%v", moved, err)
	}
	rows, err = st.ListAgentLineage(ctx, 1, 10, "srv-a")
	if err != nil || len(rows) != 1 || rows[0].ChildPane != "w9:p2" || rows[0].ParentPane != "w9:p1" {
		t.Fatalf("unexpected rows after migrate: %+v %v", rows, err)
	}

	// Reject (manual unlink) suppresses auto re-creation even after another move.
	if err := st.RejectAgentLineage(ctx, 1, 10, "srv-a", "w9:p2", "term-2", "sess-2", 6000); err != nil {
		t.Fatal(err)
	}
	// Migrate rejected row too!
	moved, err = st.MigrateAgentLineagePanes(ctx, 1, 10, "srv-a", map[string]string{
		"term-2": "w10:p2",
	}, 6500)
	if err != nil || !moved {
		t.Fatalf("migrate rejected pane: moved=%v err=%v", moved, err)
	}
	changed, err = st.UpsertAgentLineage(ctx, AgentLineage{
		OwnerID:     1,
		HostID:      10,
		Server:      "srv-a",
		ChildPane:   "w10:p2",
		ChildTermID: "term-2",
		ParentPane:  "w9:p1",
		Source:      LineageMetadata,
		State:       LineageConfirmed,
		UpdatedAt:   7000,
	})
	if err != nil || changed {
		t.Fatalf("metadata should not resurrect rejected edge: changed=%v err=%v", changed, err)
	}

	// Active-aware pruning keeps live term-2 even when older than cutoff.
	if err := st.PruneRetiredAgentLineage(ctx, 1, 10, "srv-a", 999999, map[string]bool{"term-2": true}); err != nil {
		t.Fatal(err)
	}
	rows, _ = st.ListAgentLineage(ctx, 1, 10, "srv-a")
	if len(rows) != 1 {
		t.Fatalf("live rejected row should not be pruned: %+v", rows)
	}
	// Once term-2 is no longer alive, pruning removes it.
	if err := st.PruneRetiredAgentLineage(ctx, 1, 10, "srv-a", 999999, map[string]bool{}); err != nil {
		t.Fatal(err)
	}
	rows, _ = st.ListAgentLineage(ctx, 1, 10, "srv-a")
	if len(rows) != 0 {
		t.Fatalf("retired row should be pruned: %+v", rows)
	}
}

func TestAgentLineageDepthLimitCycleRejection(t *testing.T) {
	st, err := OpenMemory()
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()

	// Build a valid chain of length 15: p0 <- p1 <- ... <- p15.
	for i := 1; i <= 15; i++ {
		_, err := st.UpsertAgentLineage(ctx, AgentLineage{
			OwnerID:    1,
			HostID:     1,
			Server:     "s",
			ChildPane:  paneNum(i),
			ParentPane: paneNum(i - 1),
			Source:     LineageManual,
			State:      LineageConfirmed,
			UpdatedAt:  int64(i * 100),
		})
		if err != nil {
			t.Fatalf("step %d failed: %v", i, err)
		}
	}
	// Adding depth 16 (p16 -> p15) exceeds max depth 16 and must be rejected.
	_, err = st.UpsertAgentLineage(ctx, AgentLineage{
		OwnerID:    1,
		HostID:     1,
		Server:     "s",
		ChildPane:  paneNum(16),
		ParentPane: paneNum(15),
		Source:     LineageManual,
		State:      LineageConfirmed,
		UpdatedAt:  1700,
	})
	if !errors.Is(err, ErrLineageCycle) {
		t.Fatalf("expected depth-limit ErrLineageCycle, got %v", err)
	}
}

func paneNum(n int) string {
	return "w1:p" + string(rune('a'+n))
}

func TestHistoricalTermMustNotCauseLiveCycle(t *testing.T) {
	st, err := OpenMemory()
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()

	_, err = st.UpsertAgentLineage(ctx, AgentLineage{
		OwnerID: 1, HostID: 1, Server: "s",
		ChildPane: "w1:p2", ChildTermID: "old-2",
		ParentPane: "w1:p1", ParentTermID: "old-1",
		Source: LineageToolLog,
	})
	if err != nil {
		t.Fatal(err)
	}
	// New server incarnation reuses pane IDs, but every terminal identity changed.
	_, err = st.UpsertAgentLineage(ctx, AgentLineage{
		OwnerID: 1, HostID: 1, Server: "s",
		ChildPane: "w1:p1", ChildTermID: "new-1",
		ParentPane: "w1:p2", ParentTermID: "new-2",
		Source: LineageManual,
	})
	if err != nil {
		t.Fatalf("retired terminals must not block an acyclic link between new occupants: %v", err)
	}
}

func TestMetadataCannotRebindConfirmedIdentity(t *testing.T) {
	st, err := OpenMemory()
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()

	_, err = st.UpsertAgentLineage(ctx, AgentLineage{
		OwnerID: 1, HostID: 1, Server: "s",
		ChildPane: "w1:p2", ChildTermID: "term-2", ChildSession: "cs1",
		ParentPane: "w1:p1", ParentTermID: "term-1", ParentSession: "ps1",
		Source: LineageManual,
	})
	if err != nil {
		t.Fatal(err)
	}

	changed, err := st.UpsertAgentLineage(ctx, AgentLineage{
		OwnerID: 1, HostID: 1, Server: "s",
		ChildPane: "w1:p2", ChildTermID: "term-2", ChildSession: "cs2",
		ParentPane: "w1:p1", ParentTermID: "term-1", ParentSession: "ps2",
		Source: LineageMetadata,
	})
	if err != nil || changed {
		t.Fatalf("metadata must not overwrite bound session identity: changed=%v err=%v", changed, err)
	}

	rows, _ := st.ListAgentLineage(ctx, 1, 1, "s")
	if len(rows) != 1 || rows[0].ChildSession != "cs1" || rows[0].ParentSession != "ps1" {
		t.Fatalf("bound identity was mutated: %+v", rows)
	}

	// Higher-priority automatic metadata with a DIFFERENT parent AND DIFFERENT child session
	// must also NOT overwrite an already-bound child session.
	_, err = st.UpsertAgentLineage(ctx, AgentLineage{
		OwnerID: 1, HostID: 1, Server: "s2",
		ChildPane: "w1:p2", ChildTermID: "term-2", ChildSession: "S1",
		ParentPane: "w1:p1", ParentTermID: "term-1",
		Source: LineageToolLog,
	})
	if err != nil {
		t.Fatal(err)
	}
	changed, err = st.UpsertAgentLineage(ctx, AgentLineage{
		OwnerID: 1, HostID: 1, Server: "s2",
		ChildPane: "w1:p2", ChildTermID: "term-2", ChildSession: "S2",
		ParentPane: "w1:p9", ParentTermID: "term-9",
		Source: LineageMetadata,
	})
	if err != nil || changed {
		t.Fatalf("metadata with different parent must not rebind child session S1->S2: changed=%v err=%v", changed, err)
	}
	rows2, _ := st.ListAgentLineage(ctx, 1, 1, "s2")
	if len(rows2) != 1 || rows2[0].ChildSession != "S1" || rows2[0].ParentPane != "w1:p1" {
		t.Fatalf("expected original S1/w1:p1 to remain intact, got %+v", rows2)
	}
}

func TestUpgradeIdentityCannotResurrectUnlink(t *testing.T) {
	st, err := OpenMemory()
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()

	_, err = st.UpsertAgentLineage(ctx, AgentLineage{
		OwnerID: 1, HostID: 1, Server: "s",
		ChildPane: "w1:p2", ChildTermID: "term-2",
		ParentPane: "w1:p1", ParentTermID: "term-1",
		Source: LineageManual,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.RejectAgentLineage(ctx, 1, 1, "s", "w1:p2", "term-2", "", 2000); err != nil {
		t.Fatal(err)
	}
	changed, err := st.UpgradeAgentLineageIdentity(ctx, AgentLineage{
		OwnerID: 1, HostID: 1, Server: "s",
		ChildPane: "w1:p2", ChildTermID: "term-2", ChildSession: "cs1",
		ParentPane: "w1:p1", ParentTermID: "term-1", ParentSession: "ps1",
		Source: LineageManual,
	})
	if err != nil || changed {
		t.Fatalf("identity upgrade must not resurrect rejected row: changed=%v err=%v", changed, err)
	}
	rows, _ := st.ListAgentLineage(ctx, 1, 1, "s")
	if len(rows) != 1 || rows[0].State != LineageRejected {
		t.Fatalf("expected row to stay rejected, got %+v", rows)
	}
}

// A manual relink under the same parent after the child's agent session changed binds the new session (review of
// 11dd0f0: it kept the old one and reported success, the edge hidden as not this occupant's).
func TestManualRelinkSameParentTakesNewSession(t *testing.T) {
	st, err := OpenMemory()
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	base := AgentLineage{OwnerID: 1, HostID: 1, Server: "s", ChildPane: "w1:p2", ChildTermID: "term-2", ParentPane: "w1:p1", ParentTermID: "term-1", Source: LineageManual}
	first := base
	first.ChildSession, first.ParentSession = "cs1", "ps1"
	if _, err := st.UpsertAgentLineage(ctx, first); err != nil {
		t.Fatal(err)
	}
	again := base
	again.ChildSession, again.ParentSession = "cs2", "ps2"
	if changed, err := st.UpsertAgentLineage(ctx, again); err != nil || !changed {
		t.Fatalf("manual relink must rebind: changed=%v err=%v", changed, err)
	}
	rows, _ := st.ListAgentLineage(ctx, 1, 1, "s")
	if len(rows) != 1 || rows[0].ChildSession != "cs2" || rows[0].ParentSession != "ps2" {
		t.Fatalf("expected cs2/ps2, got %+v", rows)
	}
	// An empty field in a manual link still keeps the bound one.
	blank := base
	if _, err := st.UpsertAgentLineage(ctx, blank); err != nil {
		t.Fatal(err)
	}
	rows, _ = st.ListAgentLineage(ctx, 1, 1, "s")
	if len(rows) != 1 || rows[0].ChildSession != "cs2" || rows[0].ParentSession != "ps2" {
		t.Fatalf("blank manual fields must keep bound ones, got %+v", rows)
	}
	// An automatic source under the same parent still cannot rebind it.
	auto := base
	auto.ChildSession, auto.Source = "cs3", LineageToolLog
	if changed, _ := st.UpsertAgentLineage(ctx, auto); changed {
		t.Fatal("automatic source must not rebind a bound child session")
	}
}
