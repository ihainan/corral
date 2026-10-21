package web

import (
	"strconv"
	"testing"
)

// An administrator makes an account with a first password; the user signs
// in with it and can do nothing but choose their own (not the same one),
// then everything works and the flag is gone.
func TestAdminCreatesUserWhoMustChangePassword(t *testing.T) {
	h := newHarness(t)
	admin := h.admin()

	// Checked as an invite's redemption would be; administrators only.
	mustStatus(t, admin.do("POST", "/api/admin/users", map[string]any{"username": "x", "password": "long-enough-1", "role": "member"}), 422, "short name")
	mustStatus(t, admin.do("POST", "/api/admin/users", map[string]any{"username": "dave", "password": "short", "role": "member"}), 422, "weak password")
	mustStatus(t, admin.do("POST", "/api/admin/users", map[string]any{"username": "dave", "password": "long-enough-1", "role": "owner"}), 400, "bad role")
	r := admin.do("POST", "/api/admin/users", map[string]any{"username": "dave", "display_name": "Dave", "password": "first-pass-123", "role": "member"})
	mustStatus(t, r, 201, "create")
	if r.json()["must_change_password"] != true {
		t.Fatalf("created without the flag: %s", r.Body)
	}
	mustStatus(t, admin.do("POST", "/api/admin/users", map[string]any{"username": "dave", "password": "first-pass-123", "role": "member"}), 409, "taken")

	dave := &client{h: h}
	mustStatus(t, dave.do("POST", "/api/login", map[string]string{"username": "dave", "password": "first-pass-123"}), 200, "login")
	me := dave.do("GET", "/api/me", nil)
	mustStatus(t, me, 200, "me")
	if me.json()["must_change_password"] != true {
		t.Fatalf("me does not say so: %s", me.Body)
	}
	// Nothing else, not even the admin routes for an admin made this way.
	for _, p := range []string{"/api/hosts", "/api/sessions", "/api/me/settings", "/api/credentials"} {
		r := dave.do("GET", p, nil)
		if r.Status != 403 || r.code() != "password_change_required" {
			t.Fatalf("%s before the change: %d %s", p, r.Status, r.Body)
		}
	}
	mustStatus(t, admin.do("GET", "/api/hosts", nil), 200, "others are not affected")
	// The bootstrap (public) still says who is signed in, with the flag.
	if b := dave.do("GET", "/api/bootstrap", nil); b.Status != 200 {
		t.Fatalf("bootstrap: %d", b.Status)
	}

	r = dave.do("POST", "/api/me/password", map[string]string{"current": "first-pass-123", "next": "first-pass-123"})
	if r.Status != 422 || r.code() != "same_password" {
		t.Fatalf("same password: %d %s", r.Status, r.Body)
	}
	mustStatus(t, dave.do("POST", "/api/me/password", map[string]string{"current": "first-pass-123", "next": "my-own-pass-456"}), 200, "change")
	mustStatus(t, dave.do("GET", "/api/hosts", nil), 200, "after the change")
	if dave.do("GET", "/api/me", nil).json()["must_change_password"] == true {
		t.Fatal("flag still set after the change")
	}

	// Without must_change the account works at once.
	mustStatus(t, admin.do("POST", "/api/admin/users", map[string]any{"username": "erin", "password": "erins-pass-123", "role": "member", "must_change": false}), 201, "create as is")
	erin := &client{h: h}
	mustStatus(t, erin.do("POST", "/api/login", map[string]string{"username": "erin", "password": "erins-pass-123"}), 200, "login erin")
	mustStatus(t, erin.do("GET", "/api/hosts", nil), 200, "erin works")
	mustStatus(t, erin.do("POST", "/api/admin/users", map[string]any{"username": "fred", "password": "freds-pass-123", "role": "member"}), 403, "members cannot create")
}

// An administrator's reset makes the user choose their own password next
// (unless told otherwise).
func TestAdminResetMakesUserChangePassword(t *testing.T) {
	h := newHarness(t)
	admin := h.admin()
	bob := h.member(admin, "bob")
	id := int64(bob.do("GET", "/api/me", nil).json()["id"].(float64))

	mustStatus(t, admin.do("POST", "/api/admin/users/"+strconv.FormatInt(id, 10)+"/password", map[string]any{"password": "reset-pass-123"}), 200, "reset")
	bob = &client{h: h}
	mustStatus(t, bob.do("POST", "/api/login", map[string]string{"username": "bob", "password": "reset-pass-123"}), 200, "login")
	if r := bob.do("GET", "/api/sessions", nil); r.Status != 403 || r.code() != "password_change_required" {
		t.Fatalf("after a reset: %d %s", r.Status, r.Body)
	}
	mustStatus(t, bob.do("POST", "/api/me/password", map[string]string{"current": "reset-pass-123", "next": "bobs-own-pass-1"}), 200, "change")
	mustStatus(t, bob.do("GET", "/api/sessions", nil), 200, "after the change")

	mustStatus(t, admin.do("POST", "/api/admin/users/"+strconv.FormatInt(id, 10)+"/password", map[string]any{"password": "reset-pass-456", "must_change": false}), 200, "reset as is")
	bob = &client{h: h}
	mustStatus(t, bob.do("POST", "/api/login", map[string]string{"username": "bob", "password": "reset-pass-456"}), 200, "login again")
	mustStatus(t, bob.do("GET", "/api/sessions", nil), 200, "no change asked")
}
