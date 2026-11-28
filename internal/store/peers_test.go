package store

import (
	"context"
	"testing"
	"time"
)

func TestPeerIdentitiesLifecycleAndOwnership(t *testing.T) {
	ctx := context.Background()
	st, err := OpenMemory()
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	now := time.Unix(1_700_000_000, 0)
	st.now = func() time.Time { return now }

	alice, err := st.CreateUser(ctx, "alice", "", "pw", RoleAdmin)
	if err != nil {
		t.Fatal(err)
	}
	bob, err := st.CreateUser(ctx, "bob", "", "pw", RoleMember)
	if err != nil {
		t.Fatal(err)
	}
	hostA, err := st.CreateHost(ctx, &Host{OwnerID: alice.ID, Name: "devbox", Address: "10.0.0.1", Port: 22, Username: "alice", Auth: AuthPassword})
	if err != nil {
		t.Fatal(err)
	}
	hostB, err := st.CreateHost(ctx, &Host{OwnerID: bob.ID, Name: "bobbox", Address: "10.0.0.2", Port: 22, Username: "bob", Auth: AuthPassword})
	if err != nil {
		t.Fatal(err)
	}

	cfgA1 := "1|10.0.0.1|22|alice|password|0"
	cfgA2 := "2|10.0.0.9|2222|alice|password|0"

	// Upsert two processes with the same PID but different proc_start (PID reuse).
	err = st.UpsertPeerIdentities(ctx, alice.ID, hostA.ID, cfgA1, []PeerIdentity{
		{
			PID:          530048,
			ProcStart:    193322812,
			AgentKey:     "1||w1:p1",
			Persistence:  "herdr",
			MuxSession:   "",
			Pane:         "w1:p1",
			HostName:     "devbox",
			Workspace:    "corral",
			Title:        "corral-old",
			Program:      "claude",
			AgentSession: "11111111-1111-1111-1111-111111111111",
			TerminalID:   "term-1",
			ObservedAt:   now.UnixMilli(),
		},
		{
			PID:          530048,
			ProcStart:    999999999,
			AgentKey:     "1||w2:p1",
			Persistence:  "herdr",
			MuxSession:   "",
			Pane:         "w2:p1",
			HostName:     "devbox",
			Workspace:    "other",
			Title:        "other-new",
			Program:      "claude",
			AgentSession: "22222222-2222-2222-2222-222222222222",
			TerminalID:   "term-2",
			ObservedAt:   now.UnixMilli(),
		},
	})
	if err != nil {
		t.Fatalf("UpsertPeerIdentities: %v", err)
	}

	// Lookup distinguishes by (pid, proc_start) and round-trips all identity fields.
	got, err := st.LookupPeerIdentities(ctx, alice.ID, hostA.ID, cfgA1, []PeerProcKey{
		{PID: 530048, ProcStart: 193322812},
		{PID: 530048, ProcStart: 999999999},
		{PID: 530048, ProcStart: 12345},
	})
	if err != nil {
		t.Fatalf("LookupPeerIdentities: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d rows, want 2: %+v", len(got), got)
	}
	if r := got[PeerProcKey{PID: 530048, ProcStart: 193322812}]; r.Title != "corral-old" || r.Pane != "w1:p1" || r.HostConfig != cfgA1 || r.AgentSession != "11111111-1111-1111-1111-111111111111" || r.TerminalID != "term-1" || r.ObservedAt != now.UnixMilli() || r.LastSeen != now.Unix() {
		t.Fatalf("row1 = %+v", r)
	}
	if r := got[PeerProcKey{PID: 530048, ProcStart: 999999999}]; r.Title != "other-new" || r.Pane != "w2:p1" {
		t.Fatalf("row2 = %+v", r)
	}

	// Host-config isolation (fix2 #8): retargeting hostA to another machine/config
	// must NOT read rows from cfgA1.
	retargetGot, err := st.LookupPeerIdentities(ctx, alice.ID, hostA.ID, cfgA2, []PeerProcKey{{PID: 530048, ProcStart: 193322812}})
	if err != nil {
		t.Fatal(err)
	}
	if len(retargetGot) != 0 {
		t.Fatalf("cfgA2 saw cfgA1's peer identity: %+v", retargetGot)
	}

	// Ownership isolation: Bob cannot see Alice's peer identities.
	bobGot, err := st.LookupPeerIdentities(ctx, bob.ID, hostA.ID, cfgA1, []PeerProcKey{{PID: 530048, ProcStart: 193322812}})
	if err != nil {
		t.Fatal(err)
	}
	if len(bobGot) != 0 {
		t.Fatalf("bob saw alice's peer identity: %+v", bobGot)
	}
	bobHostGot, err := st.LookupPeerIdentities(ctx, bob.ID, hostB.ID, cfgA1, []PeerProcKey{{PID: 530048, ProcStart: 193322812}})
	if err != nil {
		t.Fatal(err)
	}
	if len(bobHostGot) != 0 {
		t.Fatalf("bob saw peer on hostB: %+v", bobHostGot)
	}

	// ObservedAt ordering (fix2 #7): a late-arriving older observation must NOT
	// overwrite a newer observation in SQLite.
	newerObsAt := now.UnixMilli() + 5000
	olderObsAt := now.UnixMilli() - 5000
	if err := st.UpsertPeerIdentities(ctx, alice.ID, hostA.ID, cfgA1, []PeerIdentity{
		{PID: 530048, ProcStart: 193322812, AgentKey: "1||w1:p1", Persistence: "herdr", Pane: "w1:p1", HostName: "devbox", Title: "corral-renamed", ObservedAt: newerObsAt},
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertPeerIdentities(ctx, alice.ID, hostA.ID, cfgA1, []PeerIdentity{
		{PID: 530048, ProcStart: 193322812, AgentKey: "1||w1:p1", Persistence: "herdr", Pane: "w1:p1", HostName: "devbox", Title: "stale-older-scan", ObservedAt: olderObsAt},
	}); err != nil {
		t.Fatal(err)
	}
	got, _ = st.LookupPeerIdentities(ctx, alice.ID, hostA.ID, cfgA1, []PeerProcKey{{PID: 530048, ProcStart: 193322812}})
	if r := got[PeerProcKey{PID: 530048, ProcStart: 193322812}]; r.Title != "corral-renamed" || r.ObservedAt != newerObsAt {
		t.Fatalf("older observed_at overwrote newer row: %+v", r)
	}

	// Updating with empty title at >= observed_at preserves prior title.
	if err := st.UpsertPeerIdentities(ctx, alice.ID, hostA.ID, cfgA1, []PeerIdentity{
		{PID: 530048, ProcStart: 193322812, AgentKey: "1||w1:p1", Persistence: "herdr", Pane: "w1:p1", HostName: "devbox", Title: "", ObservedAt: newerObsAt + 1000},
	}); err != nil {
		t.Fatal(err)
	}
	got, _ = st.LookupPeerIdentities(ctx, alice.ID, hostA.ID, cfgA1, []PeerProcKey{{PID: 530048, ProcStart: 193322812}})
	if got[PeerProcKey{PID: 530048, ProcStart: 193322812}].Title != "corral-renamed" {
		t.Fatalf("empty title overwrote previous title: %+v", got)
	}

	// Retention pruning by last_seen: refreshing last_seen at day 60 keeps row 1
	// alive at day 100 while row 2 (unseen since day 0) is pruned.
	now = now.Add(60 * 24 * time.Hour)
	if err := st.UpsertPeerIdentities(ctx, alice.ID, hostA.ID, cfgA1, []PeerIdentity{
		{PID: 530048, ProcStart: 193322812, AgentKey: "1||w1:p1", Persistence: "herdr", Pane: "w1:p1", HostName: "devbox", Title: "corral-renamed", ObservedAt: now.UnixMilli(), LastSeen: now.Unix()},
	}); err != nil {
		t.Fatal(err)
	}
	now = now.Add(40 * 24 * time.Hour) // day 100 since start, day 40 since row 1 last_seen
	if err := st.UpsertPeerIdentities(ctx, alice.ID, hostA.ID, cfgA1, []PeerIdentity{
		{PID: 700, ProcStart: 800, AgentKey: "1||w3:p1", Persistence: "herdr", Pane: "w3:p1", HostName: "devbox", Title: "fresh", ObservedAt: now.UnixMilli()},
	}); err != nil {
		t.Fatal(err)
	}
	got, _ = st.LookupPeerIdentities(ctx, alice.ID, hostA.ID, cfgA1, []PeerProcKey{
		{PID: 530048, ProcStart: 193322812},
		{PID: 530048, ProcStart: 999999999},
		{PID: 700, ProcStart: 800},
	})
	if len(got) != 2 {
		t.Fatalf("retention pruning by last_seen failed: got %d rows: %+v", len(got), got)
	}
	if got[PeerProcKey{PID: 530048, ProcStart: 193322812}].Title != "corral-renamed" {
		t.Fatalf("refreshed row1 was wrongly pruned: %+v", got)
	}
	if _, still := got[PeerProcKey{PID: 530048, ProcStart: 999999999}]; still {
		t.Fatalf("stale row2 should have been pruned: %+v", got)
	}
}
