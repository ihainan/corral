package store

import (
	"context"
	"testing"
)

// The card's "no" and the record that the gateway made the settings file
// (with the connection version it made it under), per owner and host.
func TestAskHookMarks(t *testing.T) {
	ctx := context.Background()
	s, err := OpenMemory()
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.CreateUser(ctx, "root", "", "x", RoleAdmin); err != nil {
		t.Fatal(err)
	}
	u, err := s.CreateUser(ctx, "ann", "", "x", RoleMember)
	if err != nil {
		t.Fatal(err)
	}
	h, err := s.CreateHost(ctx, &Host{OwnerID: u.ID, Name: "h", Address: "10.0.0.1", Port: 22, Username: "a", Auth: AuthPassword})
	if err != nil {
		t.Fatal(err)
	}
	if d, err := s.AskHookDeclined(ctx, u.ID, h.ID); err != nil || d {
		t.Fatal(d, err)
	}
	if v, err := s.AskHookCreated(ctx, u.ID, h.ID); err != nil || v != 0 {
		t.Fatal(v, err)
	}
	if err := s.SetAskHookDeclined(ctx, u.ID, h.ID, true); err != nil {
		t.Fatal(err)
	}
	if err := s.SetAskHookCreated(ctx, u.ID, h.ID, h.ConnVersion); err != nil {
		t.Fatal(err)
	}
	if d, _ := s.AskHookDeclined(ctx, u.ID, h.ID); !d {
		t.Error("no not kept")
	}
	if v, _ := s.AskHookCreated(ctx, u.ID, h.ID); v != h.ConnVersion {
		t.Errorf("made under %d, want %d", v, h.ConnVersion)
	}
	// Another's host: not there.
	if _, err := s.AskHookDeclined(ctx, u.ID+99, h.ID); err != ErrNotFound {
		t.Error(err)
	}
	if err := s.SetAskHookDeclined(ctx, u.ID+99, h.ID, true); err != ErrNotFound {
		t.Error(err)
	}
}
