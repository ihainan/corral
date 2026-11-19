package web

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ihainan/corral/internal/auth"
	"github.com/ihainan/corral/internal/config"
	"github.com/ihainan/corral/internal/store"
)

// ssoError: the reason a callback sent the browser back with ("": none, signed in).
func ssoError(t *testing.T, res *http.Response) string {
	t.Helper()
	loc, _ := url.Parse(res.Header.Get("Location"))
	return loc.Query().Get("sso_error")
}

func meOf(t *testing.T, b *browser) map[string]any {
	t.Helper()
	r := b.api("GET", "/api/me", nil)
	if r.Status != 200 {
		return nil
	}
	return r.json()
}

func auditHas(t *testing.T, h *harness, action string) []*store.AuditEntry {
	t.Helper()
	rows, _ := h.store.ListAudit(t.Context(), store.AuditQuery{Limit: 500})
	var out []*store.AuditEntry
	for _, e := range rows {
		if e.Action == action {
			out = append(out, e)
		}
	}
	return out
}

func TestSSOSignInMakesTheAccount(t *testing.T) {
	op := newFakeOP(t)
	h := newHarness(t, withOIDC(op, "", nil))
	h.admin()
	b := newBrowser(t, h)
	res := b.signIn("/agents")
	if res.StatusCode != 302 || res.Header.Get("Location") != "/agents" || ssoError(t, res) != "" {
		t.Fatalf("callback: %d %s", res.StatusCode, res.Header.Get("Location"))
	}
	me := meOf(t, b)
	if me == nil || me["username"] != "alice" || me["has_password"] != false || me["role"] != "member" {
		t.Fatalf("me: %v", me)
	}
	ids, _ := me["sso"].([]any)
	if len(ids) != 1 || ids[0].(map[string]any)["subject"] != "sub-alice" {
		t.Errorf("sso: %v", me["sso"])
	}
	// Again: the same account, not another.
	b2 := newBrowser(t, h)
	b2.signIn("/")
	if me2 := meOf(t, b2); me2 == nil || me2["id"] != me["id"] {
		t.Errorf("second sign-in: %v", me2)
	}
	if len(auditHas(t, h, "auth.sso.login")) != 2 {
		t.Errorf("audit")
	}
	for _, e := range auditHas(t, h, "auth.sso.login") {
		if strings.Contains(string(e.Detail), "at-sub") || strings.Contains(string(e.Detail), "eyJ") {
			t.Errorf("a token in the audit: %s", e.Detail)
		}
	}
	// (Its TOTP not asked, its password none: the empty one is no way in.)
	c := &client{h: h}
	if r := c.do("POST", "/api/login", map[string]any{"username": "alice", "password": ""}); r.Status != 401 {
		t.Errorf("empty password: %d %s", r.Status, r.Body)
	}
}

// Where to go after: this site's paths only.
func TestSSONextIsThisSitesOnly(t *testing.T) {
	op := newFakeOP(t)
	h := newHarness(t, withOIDC(op, "", nil))
	h.admin()
	for _, n := range []string{"//evil.example", "/\\evil.example", "https://evil.example", "%2F%2Fevil", "/ok\\x", "javascript:alert(1)"} {
		b := newBrowser(t, h)
		res := b.signIn(n)
		if got := res.Header.Get("Location"); got != "/" {
			t.Errorf("next %q: went to %q", n, got)
		}
	}
	b := newBrowser(t, h)
	if got := b.signIn("/settings?tab=x").Header.Get("Location"); got != "/settings?tab=x" {
		t.Errorf("a path of ours: %q", got)
	}
	// ("%2F%2Fevil" as the browser sends it — decoded once: "//evil".)
	if safeNext("//evil") != "/" || safeNext("/%2F%2Fevil") != "/%2F%2Fevil" {
		t.Errorf("safeNext")
	}
}

// Tokens that are not right: refused, no account, no login.
func TestSSOBadTokens(t *testing.T) {
	op := newFakeOP(t)
	h := newHarness(t, withOIDC(op, "", nil))
	h.admin()
	cases := map[string]func(){
		"wrong aud":          func() { op.tamper = func(c map[string]any) { c["aud"] = "someone-else" } },
		"wrong iss":          func() { op.tamper = func(c map[string]any) { c["iss"] = "https://evil.example" } },
		"wrong nonce":        func() { op.tamper = func(c map[string]any) { c["nonce"] = "not-it" } },
		"no nonce":           func() { op.tamper = func(c map[string]any) { delete(c, "nonce") } },
		"expired":            func() { op.tamper = func(c map[string]any) { c["exp"] = time.Now().Add(-5 * time.Minute).Unix() } },
		"issued in future":   func() { op.tamper = func(c map[string]any) { c["iat"] = time.Now().Add(10 * time.Minute).Unix() } },
		"issued long ago":    func() { op.tamper = func(c map[string]any) { c["iat"] = time.Now().Add(-time.Hour).Unix() } },
		"multi aud, no azp":  func() { op.tamper = func(c map[string]any) { c["aud"] = []string{"corral", "other"} } },
		"azp another client": func() { op.tamper = func(c map[string]any) { c["azp"] = "other" } },
		"no subject":         func() { op.tamper = func(c map[string]any) { c["sub"] = "" } },
		"alg none":           func() { op.alg = "none" },
		"HS256 with secret":  func() { op.alg = "HS256" },
		"unknown kid":        func() { op.kid = "nope" },
		"PKCE mismatch":      func() { op.breakPKCE = true },
	}
	for name, set := range cases {
		op.mu.Lock()
		set()
		op.mu.Unlock()
		b := newBrowser(t, h)
		res := b.signIn("/")
		if got := ssoError(t, res); got != "invalid_token" || b.sessionCookie() != "" {
			t.Errorf("%s: %q %s", name, got, res.Header.Get("Location"))
		}
	}
	if n, _ := h.store.CountUsers(t.Context()); n != 1 {
		t.Errorf("accounts made: %d", n-1)
	}
	if len(auditHas(t, h, "auth.sso.denied")) < len(cases) {
		t.Errorf("not audited")
	}
}

// A key rotated: fetched again (its kid not known), signed in.
func TestSSOKeyRotation(t *testing.T) {
	op := newFakeOP(t)
	h := newHarness(t, withOIDC(op, "", nil))
	h.admin()
	newBrowser(t, h).signIn("/")
	before := op.jwksHits
	op.rotate()
	b := newBrowser(t, h)
	if res := b.signIn("/"); ssoError(t, res) != "" {
		t.Fatalf("after rotation: %s", res.Header.Get("Location"))
	}
	if op.jwksHits <= before {
		t.Errorf("keys not fetched again")
	}
}

// The flow: bound to its browser, taken once, its state its own.
func TestSSOFlowBound(t *testing.T) {
	op := newFakeOP(t)
	h := newHarness(t, withOIDC(op, "", nil))
	h.admin()
	// Another browser finishing it (no flow cookie): refused.
	a := newBrowser(t, h)
	res := a.get("/api/auth/oidc/start")
	authURL := res.Header.Get("Location")
	other := newBrowser(t, h)
	if got := ssoError(t, other.follow(authURL)); got != "flow_expired" {
		t.Errorf("another browser: %q", got)
	}
	// A state not its own: refused.
	b := newBrowser(t, h)
	res = b.get("/api/auth/oidc/start")
	u, _ := url.Parse(res.Header.Get("Location"))
	q := u.Query()
	q.Set("state", "forged")
	u.RawQuery = q.Encode()
	if got := ssoError(t, b.follow(u.String())); got != "flow_expired" {
		t.Errorf("forged state: %q", got)
	}
	// The callback again (replayed): refused.
	c := newBrowser(t, h)
	res = c.get("/api/auth/oidc/start")
	cb := c.get(res.Header.Get("Location")).Header.Get("Location")
	if got := ssoError(t, c.get(cb)); got != "" {
		t.Fatalf("first: %q", got)
	}
	d := newBrowser(t, h)
	d.c.Jar = c.c.Jar
	if got := ssoError(t, d.get(cb)); got != "flow_expired" {
		t.Errorf("replayed: %q", got)
	}
	// The provider sending back an error: said, its words not passed on.
	e := newBrowser(t, h)
	res = e.get("/api/auth/oidc/start")
	u, _ = url.Parse(res.Header.Get("Location"))
	if got := ssoError(t, e.get("/api/auth/oidc/callback?error=access_denied&error_description=%3Cscript%3E&state="+u.Query().Get("state"))); got != "provider_denied" {
		t.Errorf("denied: %q", got)
	}
}

// Groups: who gets in, who is an administrator (never the last one made none).
func TestSSOGroups(t *testing.T) {
	op := newFakeOP(t)
	h := newHarness(t, withOIDC(op, "", func(c *config.OIDCConfig) {
		c.Allowed = []string{"corral-users", "corral-admins"}
		c.Admins = []string{"corral-admins"}
	}))
	root := h.admin()
	// Not in a group allowed in: refused, no account.
	if got := ssoError(t, newBrowser(t, h).signIn("/")); got != "not_allowed" {
		t.Errorf("no groups: %q", got)
	}
	if n, _ := h.store.CountUsers(t.Context()); n != 1 {
		t.Errorf("an account made for one not allowed in: %d", n)
	}
	// An administrators' group: an administrator; out of it at the next sign-in: no more.
	op.who.Groups = []string{"corral-admins"}
	b := newBrowser(t, h)
	b.signIn("/")
	if me := meOf(t, b); me["role"] != "admin" {
		t.Fatalf("admin group: %v", me)
	}
	op.who.Groups = []string{"corral-users"}
	b = newBrowser(t, h)
	b.signIn("/")
	if me := meOf(t, b); me["role"] != "member" {
		t.Errorf("out of the admin group: %v", me)
	}
	// The last administrator: kept one, whatever the groups say.
	op.who.Groups = []string{"corral-admins"}
	b = newBrowser(t, h)
	b.signIn("/")
	rootMe := root.do("GET", "/api/me", nil).json()
	if err := h.store.SetDisabled(t.Context(), int64(rootMe["id"].(float64)), true); err != nil {
		t.Fatal(err)
	}
	op.who.Groups = []string{"corral-users"}
	b = newBrowser(t, h)
	b.signIn("/")
	if me := meOf(t, b); me["role"] != "admin" {
		t.Errorf("the last administrator demoted: %v", me)
	}
	found := false
	for _, e := range auditHas(t, h, "auth.sso.login") {
		found = found || strings.Contains(string(e.Detail), "role_kept")
	}
	if !found {
		t.Errorf("the kept role not audited")
	}
}

// A user name taken: another, never the existing account; no linking by email.
func TestSSONameTakenNoEmailLink(t *testing.T) {
	op := newFakeOP(t)
	h := newHarness(t, withOIDC(op, "", nil))
	root := h.admin()
	mustStatus(t, root.do("POST", "/api/admin/users", map[string]any{"username": "alice", "password": "alice-local-pw-1", "role": "member", "must_change": false}), 201, "local alice")
	b := newBrowser(t, h)
	b.signIn("/")
	me := meOf(t, b)
	if me["username"] != "alice-2" {
		t.Errorf("name: %v", me["username"])
	}
	local, _ := h.store.UserByName(t.Context(), "alice")
	if ids, _ := h.store.IdentitiesOf(t.Context(), local.ID); len(ids) != 0 || !local.HasPassword() {
		t.Errorf("the local account was taken: %v", ids)
	}
}

// Linking from Settings: signed in, the identity bound; then signing in by it.
func TestSSOLinkAndUnlink(t *testing.T) {
	op := newFakeOP(t)
	h := newHarness(t, withOIDC(op, "", nil))
	root := h.admin()
	mustStatus(t, root.do("POST", "/api/admin/users", map[string]any{"username": "carol", "password": "carol-local-pw-1", "role": "member", "must_change": false}), 201, "carol")
	op.who = fakeWho{Sub: "sub-carol", Username: "carol.sso", Email: "carol@example.com", Sid: "sid-c"}
	b := newBrowser(t, h)
	mustStatus(t, b.api("POST", "/api/login", map[string]any{"username": "carol", "password": "carol-local-pw-1"}), 200, "carol signs in")
	r := b.api("POST", "/api/me/oidc/link", nil)
	mustStatus(t, r, 200, "link")
	res := b.follow(r.json()["url"].(string))
	if res.Header.Get("Location") != "/settings?sso=linked" {
		t.Fatalf("linked: %s", res.Header.Get("Location"))
	}
	carol, _ := h.store.UserByName(t.Context(), "carol")
	if ids, _ := h.store.IdentitiesOf(t.Context(), carol.ID); len(ids) != 1 || ids[0].Subject != "sub-carol" {
		t.Fatalf("identity: %v", ids)
	}
	// Signing in by it: carol, her password kept.
	s := newBrowser(t, h)
	s.signIn("/")
	if me := meOf(t, s); me["username"] != "carol" || me["has_password"] != true {
		t.Errorf("signed in as: %v", me)
	}
	// Unlinked by herself (a password left): gone, its single sign-on logins ended.
	mustStatus(t, b.api("DELETE", "/api/me/oidc", nil), 200, "unlink")
	if meOf(t, s) != nil {
		t.Errorf("its single sign-on login not ended")
	}
	if len(auditHas(t, h, "auth.sso.link")) != 1 || len(auditHas(t, h, "auth.sso.unlink")) != 1 {
		t.Errorf("audit")
	}
	// An identity that is another account's: refused.
	op.who = fakeWho{Sub: "sub-dave", Username: "dave"}
	newBrowser(t, h).signIn("/") // (dave's account, by it)
	r = b.api("POST", "/api/me/oidc/link", nil)
	if got := ssoError(t, b.follow(r.json()["url"].(string))); got != "identity_taken" {
		t.Errorf("another's identity: %q", got)
	}
	// Signed out meanwhile: not linked.
	op.who = fakeWho{Sub: "sub-carol-2", Username: "c2"}
	r = b.api("POST", "/api/me/oidc/link", nil)
	b.api("POST", "/api/logout", nil)
	if got := ssoError(t, b.follow(r.json()["url"].(string))); got != "link_signed_out" {
		t.Errorf("signed out meanwhile: %q", got)
	}
}

// An account single sign-on is its only way into: not unlinked (by itself or
// an administrator) till it has a password; its password not changed (it has none).
func TestSSOOnlyAccount(t *testing.T) {
	op := newFakeOP(t)
	h := newHarness(t, withOIDC(op, "", nil))
	root := h.admin()
	b := newBrowser(t, h)
	b.signIn("/")
	me := meOf(t, b)
	id := strconvI(me["id"])
	if r := b.api("DELETE", "/api/me/oidc", nil); r.Status != 409 || r.code() != "sso_only" {
		t.Errorf("self unlink: %d %s", r.Status, r.Body)
	}
	if r := root.do("DELETE", "/api/admin/users/"+id+"/oidc", nil); r.Status != 409 || r.code() != "sso_only" {
		t.Errorf("admin unlink: %d %s", r.Status, r.Body)
	}
	if r := b.api("POST", "/api/me/password", map[string]any{"current": "", "next": "a-new-password-1"}); r.Status != 409 || r.code() != "no_password" {
		t.Errorf("change password: %d %s", r.Status, r.Body)
	}
	// Given a password by an administrator: an ordinary linked account, unlinked then.
	mustStatus(t, root.do("POST", "/api/admin/users/"+id+"/password", map[string]any{"password": "given-password-1"}), 200, "reset")
	mustStatus(t, root.do("DELETE", "/api/admin/users/"+id+"/oidc", nil), 200, "admin unlink")
	// The admin list says it.
	r := root.do("GET", "/api/admin/users", nil)
	if !strings.Contains(string(r.Body), `"has_password":true`) {
		t.Errorf("admin list: %s", r.Body)
	}
}

// A disabled account stays out, single sign-on or not.
func TestSSODisabled(t *testing.T) {
	op := newFakeOP(t)
	h := newHarness(t, withOIDC(op, "", nil))
	h.admin()
	b := newBrowser(t, h)
	b.signIn("/")
	id := int64(meOf(t, b)["id"].(float64))
	if err := h.store.SetDisabled(t.Context(), id, true); err != nil {
		t.Fatal(err)
	}
	c := newBrowser(t, h)
	if got := ssoError(t, c.signIn("/")); got != "disabled" || c.sessionCookie() != "" {
		t.Errorf("disabled: %q", got)
	}
}

// CORRAL_LOCAL_LOGIN: admins only, or none — said only once the password was right.
func TestLocalLoginModes(t *testing.T) {
	op := newFakeOP(t)
	h := newHarness(t, withOIDC(op, config.LocalLoginAdmins, nil))
	root := h.admin()
	mustStatus(t, root.do("POST", "/api/admin/users", map[string]any{"username": "erin", "password": "erin-local-pw-1", "role": "member", "must_change": false}), 201, "erin")
	c := &client{h: h}
	if r := c.do("POST", "/api/login", map[string]any{"username": "erin", "password": "wrong-password"}); r.Status != 401 || r.code() != "invalid_login" {
		t.Errorf("member, wrong password: %d %s", r.Status, r.Body)
	}
	if r := c.do("POST", "/api/login", map[string]any{"username": "erin", "password": "erin-local-pw-1"}); r.Status != 403 || r.code() != "local_login_off" {
		t.Errorf("member, right password: %d %s", r.Status, r.Body)
	}
	if r := c.do("POST", "/api/login", map[string]any{"username": "nobody", "password": "whatever-pw"}); r.code() != "invalid_login" {
		t.Errorf("no such account: %s", r.Body)
	}
	if r := (&client{h: h}).do("POST", "/api/login", map[string]any{"username": "root", "password": "root-password-1"}); r.Status != 200 {
		t.Errorf("admin: %d %s", r.Status, r.Body)
	}
	h.auth.SetLocalLogin(config.LocalLoginOff)
	if r := (&client{h: h}).do("POST", "/api/login", map[string]any{"username": "root", "password": "root-password-1"}); r.code() != "local_login_off" {
		t.Errorf("off, admin: %s", r.Body)
	}
	if r := (&client{h: h}).do("POST", "/api/login", map[string]any{"username": "root", "password": "nope-nope"}); r.code() != "invalid_login" {
		t.Errorf("off, wrong: %s", r.Body)
	}
	// The page is told.
	r := (&client{h: h}).do("GET", "/api/bootstrap", nil)
	if j := r.json(); j["local_login"] != "admins" || j["oidc"] == nil {
		t.Errorf("bootstrap: %s", r.Body)
	}
}

// Back-channel logout: by session, by subject (single sign-on's logins only); bad tokens refused.
func TestSSOBackchannel(t *testing.T) {
	op := newFakeOP(t)
	h := newHarness(t, withOIDC(op, "", nil))
	root := h.admin()
	mustStatus(t, root.do("POST", "/api/admin/users", map[string]any{"username": "frank", "password": "frank-local-pw-1", "role": "member", "must_change": false}), 201, "frank")
	// frank, linked, signed in three ways: by password, and by single sign-on in two sessions.
	op.who = fakeWho{Sub: "sub-frank", Username: "frank", Sid: "sid-a"}
	pw := &client{h: h}
	mustStatus(t, pw.do("POST", "/api/login", map[string]any{"username": "frank", "password": "frank-local-pw-1"}), 200, "password")
	frank, _ := h.store.UserByName(t.Context(), "frank")
	if err := h.store.LinkIdentity(t.Context(), frank.ID, &store.OIDCIdentity{Issuer: op.issuer(), Subject: "sub-frank"}); err != nil {
		t.Fatal(err)
	}
	a := newBrowser(t, h)
	a.signIn("/")
	op.who.Sid = "sid-b"
	b := newBrowser(t, h)
	b.signIn("/")
	post := func(tok string) int {
		res, err := http.PostForm(h.ts.URL+"/api/auth/oidc/backchannel", url.Values{"logout_token": {tok}})
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		return res.StatusCode
	}
	// Bad ones: nothing ended.
	for name, tok := range map[string]string{
		"nonce":       op.logoutToken("sub-frank", "", func(c map[string]any) { c["nonce"] = "x" }),
		"no event":    op.logoutToken("sub-frank", "", func(c map[string]any) { delete(c, "events") }),
		"wrong aud":   op.logoutToken("sub-frank", "", func(c map[string]any) { c["aud"] = "other" }),
		"old":         op.logoutToken("sub-frank", "", func(c map[string]any) { c["iat"] = time.Now().Add(-time.Hour).Unix() }),
		"neither":     op.logoutToken("", "", nil),
		"no jti":      op.logoutToken("sub-frank", "", func(c map[string]any) { delete(c, "jti") }),
		"bad sig":     op.logoutToken("sub-frank", "", nil) + "x",
		"an id token": op.signJWT(map[string]any{"iss": op.issuer(), "aud": "corral", "sub": "sub-frank", "iat": time.Now().Unix(), "exp": time.Now().Add(time.Minute).Unix(), "nonce": "n"}, ""),
	} {
		if got := post(tok); got != 400 {
			t.Errorf("%s: %d", name, got)
		}
	}
	if meOf(t, a) == nil || meOf(t, b) == nil {
		t.Fatalf("ended by a bad token")
	}
	// By session: that one only.
	tok := op.logoutToken("", "sid-a", nil)
	if got := post(tok); got != 200 {
		t.Fatalf("by sid: %d", got)
	}
	if meOf(t, a) != nil || meOf(t, b) == nil {
		t.Errorf("by sid: a %v, b %v", meOf(t, a) != nil, meOf(t, b) != nil)
	}
	// The same token again: refused.
	if got := post(tok); got != 400 {
		t.Errorf("replayed: %d", got)
	}
	// By subject: its single sign-on logins, not the password's.
	if got := post(op.logoutToken("sub-frank", "", nil)); got != 200 {
		t.Fatalf("by sub: %d", got)
	}
	if meOf(t, b) != nil {
		t.Errorf("by sub: single sign-on login left")
	}
	if r := pw.do("GET", "/api/me", nil); r.Status != 200 {
		t.Errorf("the password login ended: %d", r.Status)
	}
}

// Signing out a login single sign-on made: on to the provider (when set); a password's not.
func TestSSOEndSession(t *testing.T) {
	op := newFakeOP(t)
	h := newHarness(t, withOIDC(op, "", func(c *config.OIDCConfig) { c.EndSession = true }))
	root := h.admin()
	b := newBrowser(t, h)
	b.signIn("/")
	r := b.api("POST", "/api/logout", nil)
	red, _ := r.json()["redirect"].(string)
	if !strings.HasPrefix(red, op.issuer()+"/logout?client_id=corral&post_logout_redirect_uri=") {
		t.Errorf("sso logout: %s", r.Body)
	}
	if r := root.do("POST", "/api/logout", nil); strings.Contains(string(r.Body), "redirect") {
		t.Errorf("password logout: %s", r.Body)
	}
}

// Another way in (CORRAL_EXTRA_URLS): the sign-in begins and ends there.
func TestSSOExtraOrigin(t *testing.T) {
	op := newFakeOP(t)
	var extra string
	h := newHarness(t, withOIDC(op, "", nil), func(c *config.Config) {
		extra = "http://localhost:" + c.PublicURL.Port()
		c.ExtraOrigins = []string{extra}
	})
	h.admin()
	b := newBrowser(t, h)
	b.base = extra
	res := b.get("/api/auth/oidc/start")
	u, _ := url.Parse(res.Header.Get("Location"))
	if ru := u.Query().Get("redirect_uri"); ru != extra+"/api/auth/oidc/callback" {
		t.Fatalf("redirect_uri: %s", ru)
	}
	if got := ssoError(t, b.follow(res.Header.Get("Location"))); got != "" {
		t.Errorf("by the other way in: %q", got)
	}
	// (A Host of no way in: the public URL's.)
	req, _ := http.NewRequest("GET", h.ts.URL+"/api/auth/oidc/start", nil)
	req.Host = "evil.example"
	res2, _ := (&http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}).Do(req)
	u, _ = url.Parse(res2.Header.Get("Location"))
	if ru := u.Query().Get("redirect_uri"); ru != h.cfg.Origin()+"/api/auth/oidc/callback" {
		t.Errorf("a Host of no way in: %s", ru)
	}
}

// The callback and back-channel counted per address.
func TestSSORateLimited(t *testing.T) {
	op := newFakeOP(t)
	h := newHarness(t, withOIDC(op, "", nil))
	h.admin()
	b := newBrowser(t, h)
	limited := false
	for i := 0; i < 40 && !limited; i++ {
		limited = ssoError(t, b.get("/api/auth/oidc/callback?state=x&code=y")) == "rate_limited"
	}
	if !limited {
		t.Errorf("callback never limited")
	}
	limited = false
	for i := 0; i < 40 && !limited; i++ {
		res, _ := http.PostForm(h.ts.URL+"/api/auth/oidc/backchannel", url.Values{"logout_token": {"x"}})
		res.Body.Close()
		limited = res.StatusCode == 429
	}
	if !limited {
		t.Errorf("back-channel never limited")
	}
}

// Every check of a password goes by verifyPassword: CheckPassword is called
// nowhere else (but for an unknown account's dummy check), nor is a stored
// hash compared by hand.
func TestPasswordChecksGoOneWay(t *testing.T) {
	root := filepath.Join("..")
	call := regexp.MustCompile(`CheckPassword\(`)
	byHand := regexp.MustCompile(`PasswordHash\(\)\s*(==|!=)|ConstantTimeCompare\([^)]*PasswordHash`)
	filepath.Walk(root, func(p string, fi os.FileInfo, err error) error {
		if err != nil || fi.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		b, _ := os.ReadFile(p)
		lines := strings.Split(string(b), "\n")
		fn := ""
		for i, l := range lines {
			if strings.HasPrefix(l, "func ") {
				fn = l
			}
			if call.MatchString(l) && !strings.HasPrefix(strings.TrimSpace(l), "//") && !strings.HasPrefix(l, "func CheckPassword(") {
				ok := strings.Contains(fn, "func verifyPassword(") || strings.Contains(l, "dummyHash")
				if !ok {
					t.Errorf("%s:%d: a password checked outside verifyPassword: %s", p, i+1, strings.TrimSpace(l))
				}
			}
			if byHand.MatchString(l) && !strings.Contains(p, "mfa.go") {
				t.Errorf("%s:%d: a stored password hash compared by hand: %s", p, i+1, strings.TrimSpace(l))
			}
		}
		return nil
	})
}

func strconvI(v any) string { return strconv.FormatInt(int64(v.(float64)), 10) }

// Review 2 (Codex) — each as it reproduced, now refused.

// Audiences besides Corral: not trusted (an ID token, and a logout token).
func TestSSOOnlyOurAudience(t *testing.T) {
	op := newFakeOP(t)
	h := newHarness(t, withOIDC(op, "", nil))
	h.admin()
	op.tamper = func(c map[string]any) { c["aud"] = []string{"corral", "untrusted"}; c["azp"] = "corral" }
	if got := ssoError(t, newBrowser(t, h).signIn("/")); got != "invalid_token" {
		t.Errorf("ID token for others too: %q", got)
	}
	b := newBrowser(t, h)
	b.signIn("/")
	tok := op.logoutToken("sub-alice", "", func(c map[string]any) { c["aud"] = []string{"corral", "untrusted"} })
	res, _ := http.PostForm(h.ts.URL+"/api/auth/oidc/backchannel", url.Values{"logout_token": {tok}})
	res.Body.Close()
	if res.StatusCode != 400 || meOf(t, b) == nil {
		t.Errorf("logout token for others too: %d", res.StatusCode)
	}
}

func postLogout(t *testing.T, h *harness, tok string) int {
	t.Helper()
	res, err := http.PostForm(h.ts.URL+"/api/auth/oidc/backchannel", url.Values{"logout_token": {tok}})
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	return res.StatusCode
}

// A nonce in a logout token, null too: refused. A fractional exp: taken.
func TestSSOLogoutTokenEdges(t *testing.T) {
	op := newFakeOP(t)
	h := newHarness(t, withOIDC(op, "", nil))
	h.admin()
	b := newBrowser(t, h)
	b.signIn("/")
	if got := postLogout(t, h, op.logoutToken("sub-alice", "", func(c map[string]any) { c["nonce"] = nil })); got != 400 || meOf(t, b) == nil {
		t.Errorf("nonce null: %d", got)
	}
	if got := postLogout(t, h, op.logoutToken("sub-alice", "", func(c map[string]any) { c["exp"] = float64(time.Now().Add(time.Minute).Unix()) + 0.5 })); got != 200 || meOf(t, b) != nil {
		t.Errorf("fractional exp: %d", got)
	}
}

// A long sid is kept whole (or refused), never cut to collide with another.
func TestSSOLongSid(t *testing.T) {
	op := newFakeOP(t)
	h := newHarness(t, withOIDC(op, "", nil))
	h.admin()
	prefix := strings.Repeat("x", 255)
	op.who.Sid = prefix + "y"
	a := newBrowser(t, h)
	a.signIn("/")
	op.who.Sid = prefix
	b := newBrowser(t, h)
	b.signIn("/")
	if postLogout(t, h, op.logoutToken("", prefix, nil)) != 200 || meOf(t, b) != nil || meOf(t, a) == nil {
		t.Errorf("the prefix's logout: a %v, b %v", meOf(t, a) != nil, meOf(t, b) != nil)
	}
	if postLogout(t, h, op.logoutToken("", prefix+"y", nil)) != 200 || meOf(t, a) != nil {
		t.Errorf("the long sid's logout did not end it")
	}
	op.who.Sid = strings.Repeat("z", 2000)
	if got := ssoError(t, newBrowser(t, h).signIn("/")); got != "invalid_token" {
		t.Errorf("a sid too long to keep: %q", got)
	}
}

// A logout that failed to end what it named may be sent again.
func TestSSOLogoutRetry(t *testing.T) {
	op := newFakeOP(t)
	h := newHarness(t, withOIDC(op, "", nil))
	h.admin()
	b := newBrowser(t, h)
	b.signIn("/")
	tok := op.logoutToken("sub-alice", "", nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := h.srv.oidc.Logout(ctx, tok); err == nil {
		t.Fatal("expected a failure")
	}
	if _, err := h.srv.oidc.Logout(t.Context(), tok); err != nil || meOf(t, b) != nil {
		t.Errorf("retry: %v, still signed in %v", err, meOf(t, b) != nil)
	}
}

// Unlinked between its sign-in and its login being made: no login.
func TestSSOUnlinkedMidSignIn(t *testing.T) {
	op := newFakeOP(t)
	h := newHarness(t, withOIDC(op, "", nil))
	h.admin()
	root, _ := h.store.UserByName(t.Context(), "root")
	if err := h.store.LinkIdentity(t.Context(), root.ID, &store.OIDCIdentity{Issuer: op.issuer(), Subject: "sub-root"}); err != nil {
		t.Fatal(err)
	}
	c := &auth.Claimed{Issuer: op.issuer(), Subject: "sub-root", Sid: "sid-root", IssuedAt: time.Now()}
	in, err := h.srv.oidc.SignIn(t.Context(), c)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.store.UnlinkIdentity(t.Context(), root.ID, op.issuer(), nil); err != nil {
		t.Fatal(err)
	}
	if _, _, err := h.srv.oidc.StartSession(t.Context(), in.User, c, "127.0.0.1", "t"); err == nil {
		t.Errorf("a login made after the unlink")
	}
}

// Ended by the provider before its login was made (between the two): the login is refused.
func TestSSOLogoutBeforeLogin(t *testing.T) {
	op := newFakeOP(t)
	h := newHarness(t, withOIDC(op, "", nil))
	h.admin()
	newBrowser(t, h).signIn("/") // (alice's account)
	u, _ := h.store.UserByName(t.Context(), "alice")
	c := &auth.Claimed{Issuer: op.issuer(), Subject: "sub-alice", Sid: "sid-late", IssuedAt: time.Now().Add(-time.Second)}
	if _, err := h.srv.oidc.Logout(t.Context(), op.logoutToken("", "sid-late", nil)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := h.srv.oidc.StartSession(t.Context(), u, c, "127.0.0.1", "t"); err == nil {
		t.Errorf("a login of a session the provider ended")
	}
	c2 := &auth.Claimed{Issuer: op.issuer(), Subject: "sub-alice", Sid: "sid-other", IssuedAt: time.Now().Add(-time.Second)}
	if _, err := h.srv.oidc.Logout(t.Context(), op.logoutToken("sub-alice", "", nil)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := h.srv.oidc.StartSession(t.Context(), u, c2, "127.0.0.1", "t"); err == nil {
		t.Errorf("a login from before the subject's logout")
	}
}

// Many good logouts from the provider: none held back.
func TestSSOBackchannelNotLimitedWhenGood(t *testing.T) {
	op := newFakeOP(t)
	h := newHarness(t, withOIDC(op, "", nil))
	h.admin()
	b := newBrowser(t, h)
	b.signIn("/")
	for i := 0; i < 40; i++ {
		if got := postLogout(t, h, op.logoutToken("", "other-"+strconv.Itoa(i), nil)); got != 200 {
			t.Fatalf("%d: %d", i, got)
		}
	}
	if got := postLogout(t, h, op.logoutToken("sub-alice", "", nil)); got != 200 || meOf(t, b) != nil {
		t.Errorf("after many: %d", got)
	}
}

// Wrong passwords: an account with none backs off as one with a password does.
func TestSSOOnlyLocksLikeAny(t *testing.T) {
	op := newFakeOP(t)
	h := newHarness(t, withOIDC(op, "", nil))
	root := h.admin()
	mustStatus(t, root.do("POST", "/api/admin/users", map[string]any{"username": "local", "password": "valid-password-1", "role": "member", "must_change": false}), 201, "local")
	newBrowser(t, h).signIn("/") // (alice: no password)
	got := map[string]string{}
	for _, name := range []string{"local", "alice"} {
		var r resp
		for i := 0; i < 6; i++ {
			r = (&client{h: h}).do("POST", "/api/login", map[string]any{"username": name, "password": "wrong-password"})
		}
		got[name] = r.code()
	}
	if got["local"] != got["alice"] {
		t.Errorf("told apart: %v", got)
	}
}

// Before the first administrator is made: no account by single sign-on (setup still works).
func TestSSONotBeforeSetup(t *testing.T) {
	op := newFakeOP(t)
	h := newHarness(t, withOIDC(op, "", nil))
	if got := ssoError(t, newBrowser(t, h).signIn("/")); got != "setup_needed" {
		t.Errorf("before setup: %q", got)
	}
	h.admin()
}

// Made an ordinary user between its password and its code (admins only): no login.
func TestLocalLoginRecheckedAtCode(t *testing.T) {
	op := newFakeOP(t)
	h := newHarness(t, withOIDC(op, config.LocalLoginAdmins, nil))
	root := h.admin()
	mustStatus(t, root.do("POST", "/api/admin/users", map[string]any{"username": "second", "password": "second-password-1", "role": "admin", "must_change": false}), 201, "second")
	c := &client{h: h}
	mustStatus(t, c.do("POST", "/api/login", map[string]string{"username": "second", "password": "second-password-1"}), 200, "second")
	secret, _ := enableTOTP(t, c, "second-password-1")
	attempt, challenge := loginStep(t, h, "second", "second-password-1")
	u, _ := h.store.UserByName(t.Context(), "second")
	role := store.RoleMember
	if err := h.store.UpdateUserGuarded(t.Context(), u.ID, &role, nil); err != nil {
		t.Fatal(err)
	}
	r := attempt.do("POST", "/api/login/code", map[string]string{"token": challenge, "code": auth.TOTPCode(secret, time.Now().Add(30*time.Second))})
	if r.Status == 200 || attempt.cookie != "" {
		t.Errorf("signed in after demotion: %d %s", r.Status, r.Body)
	}
}

// Unlinking where its password may not be used (off; admins for a member): refused.
func TestSSOUnlinkKeepsAWayIn(t *testing.T) {
	for _, mode := range []string{config.LocalLoginOff, config.LocalLoginAdmins} {
		t.Run(mode, func(t *testing.T) {
			op := newFakeOP(t)
			h := newHarness(t, withOIDC(op, mode, nil))
			h.admin()
			hash, _ := auth.HashPassword("a-local-password-1")
			role := store.RoleMember
			if mode == config.LocalLoginOff {
				role = store.RoleAdmin
			}
			u, err := h.store.CreateUser(t.Context(), "linked", "Linked", hash, role)
			if err != nil {
				t.Fatal(err)
			}
			if err := h.store.LinkIdentity(t.Context(), u.ID, &store.OIDCIdentity{Issuer: op.issuer(), Subject: op.who.Sub}); err != nil {
				t.Fatal(err)
			}
			b := newBrowser(t, h)
			b.signIn("/")
			if r := b.api("DELETE", "/api/me/oidc", nil); r.Status != 409 || r.code() != "no_way_in" {
				t.Errorf("unlink: %d %s", r.Status, r.Body)
			}
		})
	}
}

// The configured issuer is the identity's, whatever spelling of it the token has.
func TestSSOIdentityByConfiguredIssuer(t *testing.T) {
	op := newFakeOP(t)
	h := newHarness(t, withOIDC(op, "", nil))
	h.admin()
	b := newBrowser(t, h)
	b.signIn("/")
	ids, _ := h.store.AllIdentities(t.Context())
	if len(ids) != 1 || ids[0].Issuer != op.issuer() {
		t.Errorf("issuer kept: %v", ids)
	}
}

// A binding to a provider used before: not shown as this one's.
func TestSSOOldIssuerNotShown(t *testing.T) {
	op := newFakeOP(t)
	h := newHarness(t, withOIDC(op, "", nil))
	root := h.admin()
	u, _ := h.store.UserByName(t.Context(), "root")
	if err := h.store.LinkIdentity(t.Context(), u.ID, &store.OIDCIdentity{Issuer: "https://old.example", Subject: "old-root"}); err != nil {
		t.Fatal(err)
	}
	if j := root.do("GET", "/api/me", nil).json(); j["sso"] != nil {
		t.Errorf("an old provider's binding shown: %v", j["sso"])
	}
	if r := root.do("GET", "/api/admin/users", nil); strings.Contains(string(r.Body), "old.example") {
		t.Errorf("admin list: %s", r.Body)
	}
}

// Review 3 (Codex).

// A subject's logouts out of order: the latest kept, not undone by an earlier one come later.
func TestSSOLogoutsOutOfOrder(t *testing.T) {
	op := newFakeOP(t)
	h := newHarness(t, withOIDC(op, "", nil))
	h.admin()
	newBrowser(t, h).signIn("/")
	u, _ := h.store.UserByName(t.Context(), "alice")
	t2 := time.Now()
	if _, err := h.srv.oidc.Logout(t.Context(), op.logoutToken("sub-alice", "", func(c map[string]any) { c["iat"] = t2.Unix() })); err != nil {
		t.Fatal(err)
	}
	if _, err := h.srv.oidc.Logout(t.Context(), op.logoutToken("sub-alice", "", func(c map[string]any) { c["iat"] = t2.Add(-30 * time.Second).Unix() })); err != nil {
		t.Fatal(err)
	}
	c := &auth.Claimed{Issuer: op.issuer(), Subject: "sub-alice", Sid: "s", IssuedAt: t2.Add(-10 * time.Second)}
	if _, _, err := h.srv.oidc.StartSession(t.Context(), u, c, "127.0.0.1", "t"); err == nil {
		t.Errorf("a login from before the latest logout")
	}
}

// A sign-in whose ID token is older than a logout's mark is kept: not finished.
func TestSSOStaleSignIn(t *testing.T) {
	op := newFakeOP(t)
	h := newHarness(t, withOIDC(op, "", nil))
	h.admin()
	newBrowser(t, h).signIn("/")
	u, _ := h.store.UserByName(t.Context(), "alice")
	c := &auth.Claimed{Issuer: op.issuer(), Subject: "sub-alice", Sid: "s", IssuedAt: time.Now().Add(-11 * time.Minute)}
	if _, _, err := h.srv.oidc.StartSession(t.Context(), u, c, "127.0.0.1", "t"); err == nil {
		t.Errorf("a stale sign-in finished")
	}
}

// admins only: made an ordinary user after its password (and code) were checked — no login.
func TestLocalLoginRoleAtLoginTime(t *testing.T) {
	op := newFakeOP(t)
	h := newHarness(t, withOIDC(op, config.LocalLoginAdmins, nil))
	h.admin()
	hash, _ := auth.HashPassword("a-local-password-1")
	u, err := h.store.CreateUser(t.Context(), "adm2", "", hash, store.RoleAdmin)
	if err != nil {
		t.Fatal(err)
	}
	role := store.RoleMember
	if err := h.store.UpdateUserGuarded(t.Context(), u.ID, &role, nil); err != nil {
		t.Fatal(err)
	}
	// (As the session is made: its role then, not the one read before.)
	if err := h.store.CreateLoginSessionIfCurrent(t.Context(), "zz", u.ID, hash, "", "", false, true); err == nil {
		t.Errorf("a password login of an ordinary user made")
	}
}

// Unlinking decides by the role in its own transaction.
func TestSSOUnlinkRoleNow(t *testing.T) {
	op := newFakeOP(t)
	h := newHarness(t, withOIDC(op, config.LocalLoginAdmins, nil))
	h.admin()
	hash, _ := auth.HashPassword("a-local-password-1")
	u, _ := h.store.CreateUser(t.Context(), "adm3", "", hash, store.RoleAdmin)
	if err := h.store.LinkIdentity(t.Context(), u.ID, &store.OIDCIdentity{Issuer: op.issuer(), Subject: "sub-adm3"}); err != nil {
		t.Fatal(err)
	}
	role := store.RoleMember
	if err := h.store.UpdateUserGuarded(t.Context(), u.ID, &role, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := h.store.UnlinkIdentity(t.Context(), u.ID, op.issuer(), h.auth.RoleMayUsePassword); !errors.Is(err, store.ErrNoWayIn) {
		t.Errorf("unlink of a member under admins: %v", err)
	}
}

// Review part A (ai-04's Codex pass).

// Linking from a login signed out (or ended) by the time it is written: nothing linked.
func TestSSOLinkNeedsTheLogin(t *testing.T) {
	op := newFakeOP(t)
	h := newHarness(t, withOIDC(op, "", nil))
	h.admin()
	root, _ := h.store.UserByName(t.Context(), "root")
	id := &store.OIDCIdentity{Issuer: op.issuer(), Subject: "sub-x"}
	if err := h.store.LinkIdentityForLogin(t.Context(), root.ID, "no-such-login", id); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("a login gone: %v", err)
	}
	if ids, _ := h.store.IdentitiesOf(t.Context(), root.ID); len(ids) != 0 {
		t.Errorf("linked: %v", ids)
	}
}

// The groups' role is given only once the login stands: a sign-in refused changes none.
func TestSSORoleOnlyWithTheLogin(t *testing.T) {
	op := newFakeOP(t)
	h := newHarness(t, withOIDC(op, "", func(c *config.OIDCConfig) { c.Admins = []string{"corral-admins"} }))
	h.admin()
	hash, _ := auth.HashPassword("a-local-password-1")
	u, _ := h.store.CreateUser(t.Context(), "mem", "", hash, store.RoleMember)
	if err := h.store.LinkIdentity(t.Context(), u.ID, &store.OIDCIdentity{Issuer: op.issuer(), Subject: "sub-mem"}); err != nil {
		t.Fatal(err)
	}
	c := &auth.Claimed{Issuer: op.issuer(), Subject: "sub-mem", Groups: []string{"corral-admins"}, IssuedAt: time.Now()}
	in, err := h.srv.oidc.SignIn(t.Context(), c)
	if err != nil {
		t.Fatal(err)
	}
	if now, _ := h.store.UserByID(t.Context(), u.ID); now.Role != store.RoleMember {
		t.Errorf("promoted before its login: %s", now.Role)
	}
	if _, err := h.store.UnlinkIdentity(t.Context(), u.ID, op.issuer(), nil); err != nil {
		t.Fatal(err)
	}
	if _, _, err := h.srv.oidc.StartSession(t.Context(), in.User, c, "127.0.0.1", "t"); err == nil {
		t.Fatal("a login after the unlink")
	}
	if now, _ := h.store.UserByID(t.Context(), u.ID); now.Role != store.RoleMember {
		t.Errorf("promoted by a refused sign-in: %s", now.Role)
	}
	// (By a sign-in that stands: promoted.)
	op.who = fakeWho{Sub: "sub-new", Username: "newadm", Groups: []string{"corral-admins"}}
	b := newBrowser(t, h)
	b.signIn("/")
	if me := meOf(t, b); me["role"] != "admin" {
		t.Errorf("a new administrator by its groups: %v", me)
	}
}

// Groups from userinfo that could not be read: no sign-in, no role changed.
func TestSSOUserinfoDown(t *testing.T) {
	op := newFakeOP(t)
	h := newHarness(t, withOIDC(op, "", func(c *config.OIDCConfig) { c.Admins = []string{"corral-admins"} }))
	h.admin()
	op.groupsByUserinfo = true
	op.who = fakeWho{Sub: "sub-adm", Username: "adm", Groups: []string{"corral-admins"}}
	b := newBrowser(t, h)
	b.signIn("/")
	if me := meOf(t, b); me["role"] != "admin" {
		t.Fatalf("by userinfo's groups: %v", me)
	}
	op.userinfoFails = true
	if got := ssoError(t, newBrowser(t, h).signIn("/")); got != "provider_unreachable" {
		t.Errorf("userinfo down: %q", got)
	}
	if u, _ := h.store.UserByName(t.Context(), "adm"); u.Role != store.RoleAdmin {
		t.Errorf("demoted by a fault: %s", u.Role)
	}
	// (Read, and none: a real "no groups" — demoted.)
	op.userinfoFails = false
	op.who.Groups = []string{}
	newBrowser(t, h).signIn("/")
	if u, _ := h.store.UserByName(t.Context(), "adm"); u.Role != store.RoleMember {
		t.Errorf("no groups read: %s", u.Role)
	}
}

// A logout token not valid yet (nbf ahead): refused; one valid since: taken.
func TestSSOLogoutNotBefore(t *testing.T) {
	op := newFakeOP(t)
	h := newHarness(t, withOIDC(op, "", nil))
	h.admin()
	b := newBrowser(t, h)
	b.signIn("/")
	if got := postLogout(t, h, op.logoutToken("sub-alice", "", func(c map[string]any) { c["nbf"] = time.Now().Add(10 * time.Minute).Unix() })); got != 400 || meOf(t, b) == nil {
		t.Errorf("nbf ahead: %d", got)
	}
	if got := postLogout(t, h, op.logoutToken("sub-alice", "", func(c map[string]any) { c["nbf"] = time.Now().Add(-time.Minute).Unix() })); got != 200 || meOf(t, b) != nil {
		t.Errorf("nbf past: %d", got)
	}
}

// Two-step setup for an account with no password: said so, not a wrong password.
func TestSSOOnlyTOTP(t *testing.T) {
	op := newFakeOP(t)
	h := newHarness(t, withOIDC(op, "", nil))
	h.admin()
	b := newBrowser(t, h)
	b.signIn("/")
	if r := b.api("POST", "/api/me/totp/begin", map[string]any{"password": "anything"}); r.Status != 409 || r.code() != "no_password" {
		t.Errorf("totp begin: %d %s", r.Status, r.Body)
	}
}

// A login single sign-on made but could not finish is ended as a sign-out
// is: with the user's last login, their sessions' records go too.
func TestSSOUnfinishedLoginEndsAsLogout(t *testing.T) {
	op := newFakeOP(t)
	h := newHarness(t, withOIDC(op, "", nil))
	h.admin()
	ctx := t.Context()
	u, _ := h.store.CreateUser(ctx, "mem", "", "", store.RoleMember)
	host, err := h.store.CreateHost(ctx, &store.Host{OwnerID: u.ID, Name: "h", Address: "10.0.0.1", Port: 22, Username: "a", Auth: store.AuthPassword})
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range []string{"other", "unfinished"} {
		if err := h.store.CreateLoginSession(ctx, l, u.ID, "127.0.0.1", "t"); err != nil {
			t.Fatal(err)
		}
	}
	tab := &store.OpenTab{SessionID: "s1", OwnerID: u.ID, HostID: host.ID, Kind: "tmux", Name: "main", HostConfig: "c", Cols: 80, Rows: 24, AliveAt: time.Now().Unix()}
	if err := h.store.RegisterTab(ctx, tab, "other", h.srv.auth.LoginRule(), func(*store.Host) bool { return true }); err != nil {
		t.Fatal(err)
	}
	tabs := func() int {
		all, err := h.store.OpenTabs(ctx, 100)
		if err != nil {
			t.Fatal(err)
		}
		return len(all)
	}
	r := httptest.NewRequest("GET", "/api/auth/oidc/callback", nil)
	if !h.srv.endSSOLogin(r, "unfinished", &auth.Claimed{Issuer: op.issuer()}) {
		t.Fatal("not ended")
	}
	if _, err := h.store.LoginSessionByHash(ctx, "unfinished"); err == nil {
		t.Error("the unfinished login stands")
	}
	if tabs() != 1 {
		t.Error("records gone with another login left")
	}
	// (The other browser signed out meanwhile: this one was the last.)
	if err := h.store.CreateLoginSession(ctx, "unfinished2", u.ID, "127.0.0.1", "t"); err != nil {
		t.Fatal(err)
	}
	if err := h.store.DeleteLoginSession(ctx, "other"); err != nil {
		t.Fatal(err)
	}
	if !h.srv.endSSOLogin(r, "unfinished2", &auth.Claimed{Issuer: op.issuer()}) {
		t.Fatal("not ended")
	}
	if n := tabs(); n != 0 {
		t.Errorf("records left after the last login: %d", n)
	}
}
