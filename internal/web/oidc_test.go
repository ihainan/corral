package web

import (
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

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
	if n, _ := h.store.CountUsers(t.Context()); n != 0 {
		t.Errorf("accounts made: %d", n)
	}
	if len(auditHas(t, h, "auth.sso.denied")) < len(cases) {
		t.Errorf("not audited")
	}
}

// A key rotated: fetched again (its kid not known), signed in.
func TestSSOKeyRotation(t *testing.T) {
	op := newFakeOP(t)
	h := newHarness(t, withOIDC(op, "", nil))
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
