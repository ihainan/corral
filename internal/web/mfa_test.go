package web

import (
	"bytes"
	"encoding/base32"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ihainan/corral/internal/auth"
	"github.com/ihainan/corral/internal/store"
)

// enableTOTP turns two-step sign-in on for c and returns the secret and recovery codes.
func enableTOTP(t *testing.T, c *client, password string) ([]byte, []string) {
	t.Helper()
	mustStatus(t, c.do("POST", "/api/me/totp/begin", map[string]string{"password": "wrong-password-1"}), 422, "begin, wrong password")
	r := c.do("POST", "/api/me/totp/begin", map[string]string{"password": password})
	mustStatus(t, r, 200, "begin")
	secretText := r.json()["secret"].(string)
	if !strings.HasPrefix(r.json()["uri"].(string), "otpauth://totp/Corral:") {
		t.Fatalf("uri %v", r.json()["uri"])
	}
	secret, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(secretText)
	if err != nil {
		t.Fatal(err)
	}
	if r := c.do("POST", "/api/me/totp/enable", map[string]string{"code": "000000"}); r.Status != 422 || r.code() != "bad_code" {
		t.Fatalf("enable with a wrong code: %d %s", r.Status, r.Body)
	}
	r = c.do("POST", "/api/me/totp/enable", map[string]string{"code": auth.TOTPCode(secret, time.Now())})
	mustStatus(t, r, 200, "enable")
	var codes []string
	for _, x := range r.json()["recovery_codes"].([]any) {
		codes = append(codes, x.(string))
	}
	if len(codes) != 10 {
		t.Fatalf("recovery codes %v", codes)
	}
	return secret, codes
}

func loginStep(t *testing.T, h *harness, name, password string) (*client, string) {
	t.Helper()
	c := &client{h: h}
	r := c.do("POST", "/api/login", map[string]string{"username": name, "password": password})
	mustStatus(t, r, 200, "password step")
	if r.json()["code_required"] != true || c.cookie != "" {
		t.Fatalf("password step should ask for a code and not sign in: %s cookie=%q", r.Body, c.cookie)
	}
	return c, r.json()["token"].(string)
}

func TestTwoStepSignIn(t *testing.T) {
	h := newHarness(t)
	admin := h.admin()
	bob := h.member(admin, "bob")
	secret, codes := enableTOTP(t, bob, "bob-password-1")
	if bob.do("GET", "/api/me", nil).json()["totp"] != true {
		t.Fatal("me does not say two-step is on")
	}
	// The secret is stored sealed.
	st, err := h.store.UserTOTP(t.Context(), h.userID(bob))
	if err != nil || bytes.Contains(st.Ciphertext, secret) {
		t.Fatalf("stored secret: %v", err)
	}

	// Wrong code, then the right one (the next step: the current one was used to enable).
	c, tok := loginStep(t, h, "bob", "bob-password-1")
	if r := c.do("POST", "/api/login/code", map[string]string{"token": tok, "code": "123456"}); r.Status != 422 || r.code() != "bad_code" {
		t.Fatalf("wrong code: %d %s", r.Status, r.Body)
	}
	code := auth.TOTPCode(secret, time.Now().Add(30*time.Second))
	mustStatus(t, c.do("POST", "/api/login/code", map[string]string{"token": tok, "code": code}), 200, "right code")
	if c.cookie == "" {
		t.Fatal("not signed in after the code")
	}
	mustStatus(t, c.do("GET", "/api/me", nil), 200, "signed in")
	// The same code again: refused, also for a new sign-in.
	c2, tok2 := loginStep(t, h, "bob", "bob-password-1")
	if r := c2.do("POST", "/api/login/code", map[string]string{"token": tok, "code": code}); r.Status != 401 {
		t.Fatalf("used challenge: %d", r.Status)
	}
	if r := c2.do("POST", "/api/login/code", map[string]string{"token": tok2, "code": code}); r.Status != 422 {
		t.Fatalf("reused code: %d %s", r.Status, r.Body)
	}
	// A recovery code works once, typed loosely.
	loose := strings.ToUpper(strings.ReplaceAll(codes[0], "-", " "))
	mustStatus(t, c2.do("POST", "/api/login/code", map[string]string{"token": tok2, "code": loose}), 200, "recovery code")
	c3, tok3 := loginStep(t, h, "bob", "bob-password-1")
	if r := c3.do("POST", "/api/login/code", map[string]string{"token": tok3, "code": codes[0]}); r.Status != 422 {
		t.Fatalf("recovery code reused: %d", r.Status)
	}
	if n := bob.do("GET", "/api/me/totp", nil).json()["recovery_left"]; n != float64(9) {
		t.Fatalf("recovery left %v", n)
	}

	// Disabling takes the password and a code.
	mustStatus(t, bob.do("POST", "/api/me/totp/disable", map[string]string{"password": "bob-password-1", "code": "000000"}), 422, "disable, wrong code")
	mustStatus(t, bob.do("POST", "/api/me/totp/disable", map[string]string{"password": "nope-nope-1", "code": codes[1]}), 422, "disable, wrong password")
	mustStatus(t, bob.do("POST", "/api/me/totp/disable", map[string]string{"password": "bob-password-1", "code": codes[1]}), 200, "disable")
	r := (&client{h: h}).do("POST", "/api/login", map[string]string{"username": "bob", "password": "bob-password-1"})
	if r.Status != 200 || r.json()["code_required"] == true {
		t.Fatalf("after disable: %s", r.Body)
	}
}

func TestTwoStepLimitsAndReset(t *testing.T) {
	h := newHarness(t)
	admin := h.admin()
	bob := h.member(admin, "bob")
	secret, _ := enableTOTP(t, bob, "bob-password-1")

	// One challenge allows a few tries only.
	c, tok := loginStep(t, h, "bob", "bob-password-1")
	for i := 0; i < 5; i++ {
		c.do("POST", "/api/login/code", map[string]string{"token": tok, "code": "111111"})
	}
	if r := c.do("POST", "/api/login/code", map[string]string{"token": tok, "code": auth.TOTPCode(secret, time.Now().Add(30*time.Second))}); r.Status == 200 {
		t.Fatal("challenge still usable after five wrong codes")
	}
	// Wrong codes count as failed sign-ins: the account now waits, as after
	// wrong passwords.
	if r := (&client{h: h}).do("POST", "/api/login", map[string]string{"username": "bob", "password": "bob-password-1"}); r.Status != http.StatusTooManyRequests || r.code() != "locked" {
		t.Fatalf("after five wrong codes: %d %s", r.Status, r.Body)
	}
	h.store.ResetLoginFailures(t.Context(), h.userID(bob)) // also ends the wait

	// A password change in between ends the challenge.
	h2 := newHarness(t)
	a2 := h2.admin()
	carol := h2.member(a2, "carol")
	sec2, _ := enableTOTP(t, carol, "carol-password-1")
	c, tok = loginStep(t, h2, "carol", "carol-password-1")
	mustStatus(t, a2.do("POST", "/api/admin/users/"+strconv.FormatInt(h2.userID(carol), 10)+"/password", map[string]string{"password": "carol-password-2"}), 200, "admin reset password")
	if r := c.do("POST", "/api/login/code", map[string]string{"token": tok, "code": auth.TOTPCode(sec2, time.Now().Add(30*time.Second))}); r.Status != 401 {
		t.Fatalf("challenge after a password reset: %d %s", r.Status, r.Body)
	}

	// The admin reset turns it off; members cannot.
	mustStatus(t, bob.do("POST", "/api/admin/users/"+strconv.FormatInt(h.userID(bob), 10)+"/totp/reset", nil), 403, "member reset")
	mustStatus(t, admin.do("POST", "/api/admin/users/"+strconv.FormatInt(h.userID(bob), 10)+"/totp/reset", nil), 200, "admin reset")
	r := (&client{h: h}).do("POST", "/api/login", map[string]string{"username": "bob", "password": "bob-password-1"})
	if r.Status != 200 || r.json()["code_required"] == true || r.json()["user"] == nil {
		t.Fatalf("after the admin reset: %d %s", r.Status, r.Body)
	}
	found := false
	for _, a := range auditActions(t, h) {
		found = found || a == "admin.totp_reset"
	}
	if !found {
		t.Fatal("no audit entry for the reset")
	}
}

func TestTwoStepRaces(t *testing.T) {
	h := newHarness(t)
	admin := h.admin()
	bob := h.member(admin, "bob")
	stale, _ := h.store.UserByName(t.Context(), "bob") // read before two-step is on
	secret, codes := enableTOTP(t, bob, "bob-password-1")
	ctx := t.Context()

	// A password login that read the account before two-step was turned on
	// cannot create a login without the code.
	if err := h.store.CreateLoginSessionIfCurrent(ctx, "x1", stale.ID, stale.PasswordHash(), "", "", true); err == nil {
		t.Fatal("password-only login created while two-step is on")
	}
	// A second set-up cannot replace the active secret.
	if err := h.store.EnableTOTP(ctx, stale.ID, store.TOTPSecret{Ciphertext: []byte("x"), WrappedKey: []byte("y")}, nil, stale.PasswordHash(), auth.HashToken(bob.cookie)); err == nil {
		t.Fatal("set-up replaced an active secret")
	}

	// One challenge, two recovery codes at once: one sign-in at most.
	c, tok := loginStep(t, h, "bob", "bob-password-1")
	var wg sync.WaitGroup
	results := make(chan int, 2)
	for _, code := range codes[:2] {
		wg.Add(1)
		go func(code string) {
			defer wg.Done()
			cc := &client{h: h}
			results <- cc.do("POST", "/api/login/code", map[string]string{"token": tok, "code": code}).Status
		}(code)
	}
	wg.Wait()
	close(results)
	ok := 0
	for st := range results {
		if st == 200 {
			ok++
		}
	}
	if ok > 1 {
		t.Fatalf("one challenge signed in %d times", ok)
	}
	_ = c

	// Many wrong codes at once stay within the account's limit. (The
	// challenges come first: a wrong code still being checked would count
	// towards the account's backoff and lock out a later password step.)
	h.store.ResetLoginFailures(ctx, h.userID(bob)) // the account backoff is tested elsewhere
	var tokens []string
	for i := 0; i < 4; i++ {
		_, tk := loginStep(t, h, "bob", "bob-password-1")
		tokens = append(tokens, tk)
	}
	var bad atomic.Int32
	var wg2 sync.WaitGroup
	for _, tk := range tokens {
		for j := 0; j < 5; j++ {
			wg2.Add(1)
			go func() {
				defer wg2.Done()
				r := (&client{h: h}).do("POST", "/api/login/code", map[string]string{"token": tk, "code": "000000"})
				if r.code() == "bad_code" {
					bad.Add(1)
				}
			}()
		}
	}
	wg2.Wait()
	if n := bad.Load(); n > 10 {
		t.Fatalf("%d wrong codes were checked; the limit is 10", n)
	}

	// After an admin password reset, a change confirmed with the old
	// password does not apply.
	h2 := newHarness(t)
	a2 := h2.admin()
	carol := h2.member(a2, "carol")
	_, codes2 := enableTOTP(t, carol, "carol-password-1")
	before, _ := h2.store.UserByName(ctx, "carol")
	mustStatus(t, a2.do("POST", "/api/admin/users/"+strconv.FormatInt(before.ID, 10)+"/password", map[string]string{"password": "carol-password-2"}), 200, "reset")
	if err := h2.auth.DisableTOTP(ctx, before, "carol-password-1", codes2[0], auth.HashToken(carol.cookie)); !errors.Is(err, auth.ErrChanged) {
		t.Fatalf("disable after a password reset: %v", err)
	}
	_ = secret

	// A set-up begun before a password change does not finish after it.
	dan := h2.member(a2, "dan")
	r := dan.do("POST", "/api/me/totp/begin", map[string]string{"password": "dan-password-1"})
	mustStatus(t, r, 200, "begin")
	sec, _ := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(r.json()["secret"].(string))
	mustStatus(t, dan.do("POST", "/api/me/password", map[string]string{"current": "dan-password-1", "next": "dan-password-2"}), 200, "change password")
	if r := dan.do("POST", "/api/me/totp/enable", map[string]string{"code": auth.TOTPCode(sec, time.Now())}); r.code() != "account_changed" {
		t.Fatalf("enable after a password change: %d %s", r.Status, r.Body)
	}
}
