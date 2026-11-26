package store

import (
	"context"
	"testing"
)

// The users page's "last active": of each account, its sign-ins' latest use; one with none kept, not there.
func TestLastSeenByUser(t *testing.T) {
	ctx := context.Background()
	s, err := OpenMemory()
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	a, _ := s.CreateUser(ctx, "ann", "", "x", RoleAdmin)
	b, _ := s.CreateUser(ctx, "bob", "", "x", RoleMember)
	_, _ = s.CreateUser(ctx, "cid", "", "x", RoleMember)
	for _, h := range []string{"h1", "h2"} {
		if err := s.CreateLoginSession(ctx, h, a.ID, "", ""); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.CreateLoginSession(ctx, "h3", b.ID, "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE login_sessions SET last_seen_at = CASE token_hash WHEN 'h1' THEN 100 WHEN 'h2' THEN 300 ELSE 200 END`); err != nil {
		t.Fatal(err)
	}
	got, err := s.LastSeenByUser(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[a.ID] != 300 || got[b.ID] != 200 {
		t.Fatalf("got %v", got)
	}
}
