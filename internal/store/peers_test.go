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
			PID:         530048,
			ProcStart:   193322812,
			AgentKey:    "1||w1:p1",
			Persistence: "herdr",
			MuxSession:  "",
			Pane:        "w1:p1",
			HostName:    "devbox",
			Workspace:   "corral",
			Title:       "corral-old",
			Program:     "claude",
		},
		{
			PID:         530048,
			ProcStart:   999999999,
			AgentKey:    "1||w2:p1",
			Persistence: "herdr",
			MuxSession:  "",
			Pane:        "w2:p1",
			HostName:    "devbox",
			Workspace:   "other",
			Title:       "other-new",
			Program:     "claude",
		},
	})
	if err != nil {
		t.Fatalf("UpsertPeerIdentities: %v", err)
	}

	// Lookup distinguishes by (pid, proc_start).
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
	if r := got[PeerProcKey{PID: 530048, ProcStart: 193322812}]; r.Title != "corral-old" || r.Pane != "w1:p1" {
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

	// Retention pruning: advancing > 90 days prunes old rows on next upsert.
	now = now.Add(91 * 24 * time.Hour)
	if err := st.UpsertPeerIdentities(ctx, alice.ID, hostA.ID, []PeerIdentity{
		{PID: 700, ProcStart: 800, AgentKey: "1||w3:p1", Persistence: "herdr", Pane: "w3:p1", HostName: "devbox", Title: "fresh"},
	}); err != nil {
		t.Fatal(err)
	}
	got, _ = st.LookupPeerIdentities(ctx, alice.ID, hostA.ID, []PeerProcKey{
		{PID: 530048, ProcStart: 193322812},
		{PID: 700, ProcStart: 800},
	})
	if len(got) != 1 || got[PeerProcKey{PID: 700, ProcStart: 800}].Title != "fresh" {
		t.Fatalf("retention pruning failed: %+v", got)
	}
}
