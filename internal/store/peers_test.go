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

	// Upsert two processes with the same PID but different proc_start (PID reuse).
	err = st.UpsertPeerIdentities(ctx, alice.ID, hostA.ID, []PeerIdentity{
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
		},
	})
	if err != nil {
		t.Fatalf("UpsertPeerIdentities: %v", err)
	}

	// Lookup distinguishes by (pid, proc_start) and round-trips agent_session / terminal_id / last_seen.
	got, err := st.LookupPeerIdentities(ctx, alice.ID, hostA.ID, []PeerProcKey{
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
	if r := got[PeerProcKey{PID: 530048, ProcStart: 193322812}]; r.Title != "corral-old" || r.Pane != "w1:p1" || r.AgentSession != "11111111-1111-1111-1111-111111111111" || r.TerminalID != "term-1" || r.LastSeen != now.Unix() {
		t.Fatalf("row1 = %+v", r)
	}
	if r := got[PeerProcKey{PID: 530048, ProcStart: 999999999}]; r.Title != "other-new" || r.Pane != "w2:p1" {
		t.Fatalf("row2 = %+v", r)
	}

	// Ownership isolation: Bob cannot see Alice's peer identities.
	bobGot, err := st.LookupPeerIdentities(ctx, bob.ID, hostA.ID, []PeerProcKey{{PID: 530048, ProcStart: 193322812}})
	if err != nil {
		t.Fatal(err)
	}
	if len(bobGot) != 0 {
		t.Fatalf("bob saw alice's peer identity: %+v", bobGot)
	}
	bobHostGot, err := st.LookupPeerIdentities(ctx, bob.ID, hostB.ID, []PeerProcKey{{PID: 530048, ProcStart: 193322812}})
	if err != nil {
		t.Fatal(err)
	}
	if len(bobHostGot) != 0 {
		t.Fatalf("bob saw peer on hostB: %+v", bobHostGot)
	}

	// Updating with empty title preserves prior title; updating with non-empty title replaces it.
	if err := st.UpsertPeerIdentities(ctx, alice.ID, hostA.ID, []PeerIdentity{
		{PID: 530048, ProcStart: 193322812, AgentKey: "1||w1:p1", Persistence: "herdr", Pane: "w1:p1", HostName: "devbox", Title: ""},
	}); err != nil {
		t.Fatal(err)
	}
	got, _ = st.LookupPeerIdentities(ctx, alice.ID, hostA.ID, []PeerProcKey{{PID: 530048, ProcStart: 193322812}})
	if got[PeerProcKey{PID: 530048, ProcStart: 193322812}].Title != "corral-old" {
		t.Fatalf("empty title overwrote previous title: %+v", got)
	}
	if err := st.UpsertPeerIdentities(ctx, alice.ID, hostA.ID, []PeerIdentity{
		{PID: 530048, ProcStart: 193322812, AgentKey: "1||w1:p1", Persistence: "herdr", Pane: "w1:p1", HostName: "devbox", Title: "corral-renamed"},
	}); err != nil {
		t.Fatal(err)
	}
	got, _ = st.LookupPeerIdentities(ctx, alice.ID, hostA.ID, []PeerProcKey{{PID: 530048, ProcStart: 193322812}})
	if got[PeerProcKey{PID: 530048, ProcStart: 193322812}].Title != "corral-renamed" {
		t.Fatalf("non-empty title did not update: %+v", got)
	}

	// Retention pruning by last_seen (item 6): refreshing last_seen at day 60
	// keeps row 1 alive at day 100 while row 2 (unseen since day 0) is pruned.
	now = now.Add(60 * 24 * time.Hour)
	if err := st.UpsertPeerIdentities(ctx, alice.ID, hostA.ID, []PeerIdentity{
		{PID: 530048, ProcStart: 193322812, AgentKey: "1||w1:p1", Persistence: "herdr", Pane: "w1:p1", HostName: "devbox", Title: "corral-renamed", LastSeen: now.Unix()},
	}); err != nil {
		t.Fatal(err)
	}
	now = now.Add(40 * 24 * time.Hour) // day 100 since start, day 40 since row 1 last_seen
	if err := st.UpsertPeerIdentities(ctx, alice.ID, hostA.ID, []PeerIdentity{
		{PID: 700, ProcStart: 800, AgentKey: "1||w3:p1", Persistence: "herdr", Pane: "w3:p1", HostName: "devbox", Title: "fresh"},
	}); err != nil {
		t.Fatal(err)
	}
	got, _ = st.LookupPeerIdentities(ctx, alice.ID, hostA.ID, []PeerProcKey{
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
