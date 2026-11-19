package web

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"

	"github.com/ihainan/corral/internal/config"
)

// fakeOP is an OpenID provider for tests: discovery, an authorize endpoint
// that signs in whoever is set as next (no page), a token endpoint that
// checks the client, the code (once) and PKCE, keys it can rotate, and ways
// to make what it issues wrong.
type fakeOP struct {
	t   *testing.T
	srv *httptest.Server

	mu        sync.Mutex
	keys      []fakeKey
	sign      int // the key signing
	codes     map[string]*fakeCode
	who       fakeWho
	tamper    func(claims map[string]any) // changes the next ID token's claims
	alg       string                      // "": RS256; "none", "HS256": made so
	kid       string                      // a kid other than the signing key's
	jwksHits  int
	clientID  string
	secret    string
	breakPKCE bool // the next code's challenge is not the one the gateway sent
	// groups only by userinfo (not in the ID token); userinfo failing.
	groupsByUserinfo bool
	userinfoFails    bool
}

type fakeKey struct {
	kid  string
	priv *rsa.PrivateKey
}

type fakeCode struct {
	challenge, nonce, redirect string
	who                        fakeWho
	used                       bool
}

// fakeWho: who the provider signs in next.
type fakeWho struct {
	Sub, Username, Email, Name string
	Groups                     []string
	Sid                        string
}

func newFakeOP(t *testing.T) *fakeOP {
	op := &fakeOP{t: t, codes: map[string]*fakeCode{}, clientID: "corral", secret: "s3cret-client-for-tests-32-bytes!!",
		who: fakeWho{Sub: "sub-alice", Username: "alice", Email: "alice@example.com", Name: "Alice A", Sid: "sid-1"}}
	op.rotate()
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", op.discovery)
	mux.HandleFunc("/authorize", op.authorize)
	mux.HandleFunc("/token", op.token)
	mux.HandleFunc("/jwks", op.jwks)
	mux.HandleFunc("/userinfo", op.userinfo)
	op.srv = httptest.NewServer(mux)
	t.Cleanup(op.srv.Close)
	return op
}

func (op *fakeOP) issuer() string { return op.srv.URL }

// rotate adds a new signing key (the old ones still listed).
func (op *fakeOP) rotate() {
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		op.t.Fatal(err)
	}
	op.mu.Lock()
	op.keys = append(op.keys, fakeKey{kid: fmt.Sprintf("k%d", len(op.keys)+1), priv: k})
	op.sign = len(op.keys) - 1
	op.mu.Unlock()
}

// config is the gateway's OIDC configuration for this provider.
func (op *fakeOP) config() *config.OIDCConfig {
	return &config.OIDCConfig{Issuer: op.issuer(), ClientID: op.clientID, ClientSecret: op.secret, Scopes: []string{"openid", "profile", "email"}, GroupsClaim: "groups"}
}

func (op *fakeOP) discovery(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{
		"issuer": op.issuer(), "authorization_endpoint": op.issuer() + "/authorize", "token_endpoint": op.issuer() + "/token",
		"jwks_uri": op.issuer() + "/jwks", "userinfo_endpoint": op.issuer() + "/userinfo", "end_session_endpoint": op.issuer() + "/logout",
		"id_token_signing_alg_values_supported": []string{"RS256"}, "response_types_supported": []string{"code"},
		"subject_types_supported": []string{"public"},
	})
}

func (op *fakeOP) authorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if q.Get("client_id") != op.clientID || q.Get("response_type") != "code" || q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") == "" || q.Get("nonce") == "" {
		http.Error(w, "bad authorize request", 400)
		return
	}
	code := opRandHex()
	op.mu.Lock()
	ch := q.Get("code_challenge")
	if op.breakPKCE {
		ch, op.breakPKCE = "not-"+ch, false
	}
	op.codes[code] = &fakeCode{challenge: ch, nonce: q.Get("nonce"), redirect: q.Get("redirect_uri"), who: op.who}
	op.mu.Unlock()
	u, _ := url.Parse(q.Get("redirect_uri"))
	v := u.Query()
	v.Set("code", code)
	v.Set("state", q.Get("state"))
	u.RawQuery = v.Encode()
	http.Redirect(w, r, u.String(), http.StatusFound)
}

func (op *fakeOP) token(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	id, sec, ok := r.BasicAuth()
	if !ok {
		id, sec = r.PostForm.Get("client_id"), r.PostForm.Get("client_secret")
	}
	if id != op.clientID || sec != op.secret {
		writeJSON(w, 401, map[string]any{"error": "invalid_client"})
		return
	}
	op.mu.Lock()
	c := op.codes[r.PostForm.Get("code")]
	if c == nil || c.used || c.redirect != r.PostForm.Get("redirect_uri") {
		op.mu.Unlock()
		writeJSON(w, 400, map[string]any{"error": "invalid_grant"})
		return
	}
	c.used = true
	op.mu.Unlock()
	sum := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
	if base64.RawURLEncoding.EncodeToString(sum[:]) != c.challenge {
		writeJSON(w, 400, map[string]any{"error": "invalid_grant", "error_description": "PKCE"})
		return
	}
	now := time.Now()
	claims := map[string]any{"iss": op.issuer(), "sub": c.who.Sub, "aud": op.clientID, "exp": now.Add(5 * time.Minute).Unix(), "iat": now.Unix(),
		"nonce": c.nonce, "preferred_username": c.who.Username, "email": c.who.Email, "name": c.who.Name, "sid": c.who.Sid}
	op.mu.Lock()
	byUI := op.groupsByUserinfo
	op.mu.Unlock()
	if c.who.Groups != nil && !byUI {
		claims["groups"] = c.who.Groups
	}
	op.mu.Lock()
	tamper := op.tamper
	op.tamper = nil
	op.mu.Unlock()
	if tamper != nil {
		tamper(claims)
	}
	writeJSON(w, 200, map[string]any{"access_token": "at-" + c.who.Sub, "token_type": "Bearer", "expires_in": 300, "id_token": op.signJWT(claims, "")})
}

// signJWT signs claims as the provider does (alg and kid as set), typ as given.
func (op *fakeOP) signJWT(claims map[string]any, typ string) string {
	op.mu.Lock()
	alg, kid, k := op.alg, op.kid, op.keys[op.sign]
	op.alg, op.kid = "", ""
	op.mu.Unlock()
	payload, _ := json.Marshal(claims)
	switch alg {
	case "none":
		h, _ := json.Marshal(map[string]any{"alg": "none", "typ": "JWT"})
		return opB64(h) + "." + opB64(payload) + "."
	case "HS256":
		s, _ := jose.NewSigner(jose.SigningKey{Algorithm: jose.HS256, Key: []byte(op.secret)}, nil)
		o, _ := s.Sign(payload)
		out, _ := o.CompactSerialize()
		return out
	}
	if kid == "" {
		kid = k.kid
	}
	opts := (&jose.SignerOptions{}).WithHeader("kid", kid)
	if typ != "" {
		opts = opts.WithType(jose.ContentType(typ))
	}
	s, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: k.priv}, opts)
	if err != nil {
		op.t.Fatal(err)
	}
	o, _ := s.Sign(payload)
	out, _ := o.CompactSerialize()
	return out
}

func (op *fakeOP) jwks(w http.ResponseWriter, r *http.Request) {
	op.mu.Lock()
	op.jwksHits++
	var set jose.JSONWebKeySet
	for _, k := range op.keys {
		set.Keys = append(set.Keys, jose.JSONWebKey{Key: &k.priv.PublicKey, KeyID: k.kid, Algorithm: "RS256", Use: "sig"})
	}
	op.mu.Unlock()
	writeJSON(w, 200, set)
}

func (op *fakeOP) userinfo(w http.ResponseWriter, r *http.Request) {
	op.mu.Lock()
	who, fails := op.who, op.userinfoFails
	op.mu.Unlock()
	if fails {
		http.Error(w, "down", http.StatusServiceUnavailable)
		return
	}
	writeJSON(w, 200, map[string]any{"sub": who.Sub, "groups": who.Groups})
}

// logoutToken: a back-channel logout token, its claims changed by f.
func (op *fakeOP) logoutToken(sub, sid string, f func(map[string]any)) string {
	now := time.Now()
	c := map[string]any{"iss": op.issuer(), "aud": op.clientID, "iat": now.Unix(), "exp": now.Add(2 * time.Minute).Unix(), "jti": opRandHex(),
		"events": map[string]any{"http://schemas.openid.net/event/backchannel-logout": map[string]any{}}}
	if sub != "" {
		c["sub"] = sub
	}
	if sid != "" {
		c["sid"] = sid
	}
	if f != nil {
		f(c)
	}
	return op.signJWT(c, "logout+jwt")
}

func opB64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func opRandHex() string {
	b := make([]byte, 12)
	rand.Read(b)
	return fmt.Sprintf("%x", b)
}

// browser is a browser going through the gateway and the provider: its
// cookies kept, redirects seen one by one.
type browser struct {
	t    *testing.T
	h    *harness
	c    *http.Client
	last *http.Response
	base string // the site it goes to ("": the gateway's PublicURL)
}

func newBrowser(t *testing.T, h *harness) *browser {
	jar, _ := cookiejar.New(nil)
	return &browser{t: t, h: h, c: &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}

// get fetches u (absolute, or a path of the gateway's), not following redirects.
func (b *browser) get(u string) *http.Response {
	b.t.Helper()
	if strings.HasPrefix(u, "/") {
		u = b.site() + u
	}
	res, err := b.c.Get(u)
	if err != nil {
		b.t.Fatal(err)
	}
	io.Copy(io.Discard, res.Body)
	res.Body.Close()
	b.last = res
	return res
}

// signIn goes from the button to back: the start, the provider, the
// callback. It returns the gateway's answer to the callback.
func (b *browser) signIn(next string) *http.Response {
	b.t.Helper()
	res := b.get("/api/auth/oidc/start?next=" + url.QueryEscape(next))
	if res.StatusCode != 302 {
		b.t.Fatalf("start: %d", res.StatusCode)
	}
	return b.follow(res.Header.Get("Location"))
}

// follow goes on from the provider's URL to the gateway's callback.
func (b *browser) follow(authURL string) *http.Response {
	b.t.Helper()
	res := b.get(authURL)
	if res.StatusCode != 302 {
		b.t.Fatalf("authorize: %d", res.StatusCode)
	}
	return b.get(res.Header.Get("Location"))
}

func (b *browser) site() string {
	if b.base != "" {
		return b.base
	}
	return b.h.ts.URL
}

// sessionCookie: the gateway's login cookie this browser has ("": none).
func (b *browser) sessionCookie() string {
	u, _ := url.Parse(b.site())
	for _, c := range b.c.Jar.Cookies(u) {
		if c.Name == b.h.srv.cookieName() {
			return c.Value
		}
	}
	return ""
}

// api calls the gateway as this browser's page would (its cookies, its site's Origin).
func (b *browser) api(method, path string, body any) resp {
	b.t.Helper()
	var rd io.Reader
	if body != nil {
		j, _ := json.Marshal(body)
		rd = strings.NewReader(string(j))
	}
	req, _ := http.NewRequest(method, b.site()+path, rd)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Origin", b.site())
	res, err := b.c.Do(req)
	if err != nil {
		b.t.Fatal(err)
	}
	defer res.Body.Close()
	out, _ := io.ReadAll(res.Body)
	return resp{Status: res.StatusCode, Body: out}
}

// withOIDC is a harness option: single sign-on with op, local login mode as given ("" keeps all).
func withOIDC(op *fakeOP, local string, set func(*config.OIDCConfig)) harnessOpt {
	return func(c *config.Config) {
		c.OIDC = op.config()
		if set != nil {
			set(c.OIDC)
		}
		if local != "" {
			c.LocalLogin = local
		} else {
			c.LocalLogin = config.LocalLoginAll
		}
	}
}
