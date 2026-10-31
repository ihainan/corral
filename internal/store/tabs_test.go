package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

type tabEnv struct {
	t      *testing.T
	ctx    context.Context
	s      *Store
	user   *User
	host   *Host
	rule   LoginRule
	nowSec int64
}

func newTabEnv(t *testing.T) *tabEnv {
	t.Helper()
	ctx := context.Background()
	s, err := OpenMemory()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	// (An administrator besides: disabling ann leaves one.)
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
	now := time.Now().Unix()
	return &tabEnv{t: t, ctx: ctx, s: s, user: u, host: h, rule: LoginRule{SeenSince: now - 3600, CreatedSince: now - 86400}, nowSec: now}
}

func (e *tabEnv) tab(id, name string) *OpenTab {
	return &OpenTab{SessionID: id, OwnerID: e.user.ID, HostID: e.host.ID, Kind: "tmux", Name: name, HostConfig: "cfg1", Cols: 80, Rows: 24, AliveAt: e.nowSec}
}

func (e *tabEnv) ids() []string {
	e.t.Helper()
	all, err := e.s.OpenTabs(e.ctx, 100)
	if err != nil {
		e.t.Fatal(err)
	}
	var out []string
	for _, t := range all {
		out = append(out, t.SessionID)
	}
	return out
}

func (e *tabEnv) login(hash string) {
	e.t.Helper()
	if err := e.s.CreateLoginSession(e.ctx, hash, e.user.ID, "", ""); err != nil {
		e.t.Fatal(err)
	}
}

// Registered only with the login that opened it valid; taking the place of
// the record of the same session left from before.
func TestRegisterTab(t *testing.T) {
	e := newTabEnv(t)
	if err := e.s.RegisterTab(e.ctx, e.tab("s1", "main"), "nologin", e.rule); !errors.Is(err, ErrNoLogin) {
		t.Fatalf("no login: %v", err)
	}
	e.login("L1")
	if err := e.s.RegisterTab(e.ctx, e.tab("s1", "main"), "L1", e.rule); err != nil {
		t.Fatal(err)
	}
	// (The same session opened again: the old record goes, the new one stays.)
	if err := e.s.RegisterTab(e.ctx, e.tab("s2", "main"), "L1", e.rule); err != nil {
		t.Fatal(err)
	}
	if ids := e.ids(); len(ids) != 1 || ids[0] != "s2" {
		t.Fatalf("taken over: %v", ids)
	}
	// A login too old for the rule, or of a user told to choose a password: none.
	stale := LoginRule{SeenSince: e.nowSec + 10, CreatedSince: e.rule.CreatedSince}
	if err := e.s.RegisterTab(e.ctx, e.tab("s3", "other"), "L1", stale); !errors.Is(err, ErrNoLogin) {
		t.Errorf("stale login: %v", err)
	}
	if _, err := e.s.db.Exec(`UPDATE users SET must_change_password = 1 WHERE id = ?`, e.user.ID); err != nil {
		t.Fatal(err)
	}
	if err := e.s.RegisterTab(e.ctx, e.tab("s3", "other"), "L1", e.rule); !errors.Is(err, ErrNoLogin) {
		t.Errorf("must change: %v", err)
	}
}

// Replaced only while the old record is there, its user has a login and the
// host is as wanted.
func TestReplaceTab(t *testing.T) {
	e := newTabEnv(t)
	e.login("L1")
	if err := e.s.RegisterTab(e.ctx, e.tab("old", "main"), "L1", e.rule); err != nil {
		t.Fatal(err)
	}
	ok := func(*Host) bool { return true }
	if err := e.s.ReplaceTab(e.ctx, "old", e.tab("new", "main"), e.rule, func(*Host) bool { return false }); !errors.Is(err, ErrHostChanged) {
		t.Errorf("host changed: %v", err)
	}
	if ids := e.ids(); len(ids) != 1 || ids[0] != "old" {
		t.Fatalf("a refused replace kept the old: %v", ids)
	}
	if err := e.s.ReplaceTab(e.ctx, "old", e.tab("new", "main"), e.rule, ok); err != nil {
		t.Fatal(err)
	}
	if ids := e.ids(); len(ids) != 1 || ids[0] != "new" {
		t.Fatalf("replaced: %v", ids)
	}
	if err := e.s.ReplaceTab(e.ctx, "old", e.tab("again", "main"), e.rule, ok); !errors.Is(err, ErrNotFound) {
		t.Errorf("old gone: %v", err)
	}
	// No login left: refused, the old record kept.
	if _, err := e.s.db.Exec(`DELETE FROM login_sessions`); err != nil {
		t.Fatal(err)
	}
	if err := e.s.ReplaceTab(e.ctx, "new", e.tab("n2", "main"), e.rule, ok); !errors.Is(err, ErrNoLogin) {
		t.Errorf("no login: %v", err)
	}
	if ids := e.ids(); len(ids) != 1 || ids[0] != "new" {
		t.Fatalf("a refused replace kept the old: %v", ids)
	}
}

// Signing out keeps the records while another valid login is left; the last
// one takes them; ending access, resetting the password, disabling the user
// take them in the same transaction; deleting the host or the user too.
func TestTabsRevoked(t *testing.T) {
	e := newTabEnv(t)
	e.login("L1")
	e.login("L2")
	reg := func(id string) {
		t.Helper()
		if err := e.s.RegisterTab(e.ctx, e.tab(id, id), "L1", e.rule); err != nil {
			t.Fatal(err)
		}
	}
	reg("a")
	if err := e.s.LogoutTx(e.ctx, "L1", e.rule); err != nil {
		t.Fatal(err)
	}
	if len(e.ids()) != 1 {
		t.Fatal("signing out of one device took them")
	}
	if err := e.s.LogoutTx(e.ctx, "L2", e.rule); err != nil {
		t.Fatal(err)
	}
	if len(e.ids()) != 0 {
		t.Fatal("the last login's sign-out kept them")
	}
	for _, revoke := range []func() error{
		func() error { return e.s.EndAccessTx(e.ctx, e.user.ID) },
		func() error { return e.s.ResetPasswordTx(e.ctx, e.user.ID, "y", false) },
		func() error {
			d := true
			return e.s.UpdateUserGuarded(e.ctx, e.user.ID, nil, &d)
		},
		func() error { return e.s.DeleteHost(e.ctx, e.user.ID, e.host.ID) },
	} {
		e.login("L1")
		if _, err := e.s.db.Exec(`UPDATE users SET disabled = 0, must_change_password = 0, password_changed_at = 0 WHERE id = ?`, e.user.ID); err != nil {
			t.Fatal(err)
		}
		if e.host == nil {
			break
		}
		reg("b")
		if err := revoke(); err != nil {
			t.Fatal(err)
		}
		if ids := e.ids(); len(ids) != 0 {
			t.Errorf("kept after revocation: %v", ids)
		}
	}
}

// Kept as running: only the ids given, a hundred to a transaction.
func TestKeepTabs(t *testing.T) {
	e := newTabEnv(t)
	e.login("L1")
	for i := 0; i < 3; i++ {
		id := string(rune('a' + i))
		if err := e.s.RegisterTab(e.ctx, e.tab(id, id), "L1", e.rule); err != nil {
			t.Fatal(err)
		}
	}
	states := []TabState{{SessionID: "a", OwnerID: e.user.ID, AliveAt: 999, IdleSince: 5, Cols: 100, Rows: 40}}
	for i := 0; i < 150; i++ { // (not there: nothing)
		states = append(states, TabState{SessionID: "x", OwnerID: e.user.ID, AliveAt: 1})
	}
	if err := e.s.KeepTabs(e.ctx, states); err != nil {
		t.Fatal(err)
	}
	a, _ := e.s.OpenTabByID(e.ctx, e.user.ID, "a")
	b, _ := e.s.OpenTabByID(e.ctx, e.user.ID, "b")
	if a.AliveAt != 999 || a.IdleSince != 5 || a.Cols != 100 || b.AliveAt != e.nowSec {
		t.Errorf("a %+v b %+v", a, b)
	}
}
