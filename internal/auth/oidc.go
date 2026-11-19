package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	jose "github.com/go-jose/go-jose/v4"
	"golang.org/x/oauth2"

	"github.com/ihainan/corral/internal/config"
	"github.com/ihainan/corral/internal/store"
)

// Single sign-on by OpenID Connect: the Authorization Code flow with PKCE
// (S256), a state and a nonce, Corral a confidential client of one provider
// found by discovery from its issuer. An ID token is verified against the
// provider's keys (go-oidc: kept, and fetched again for a key not known):
// its signature by an asymmetric algorithm the provider says it signs with,
// its issuer, that Corral is its audience (and its authorized party where it
// names one, or more than one audience), its expiry and when it was issued,
// and its nonce. A person is the provider's (issuer, subject): never an email
// or a user name. Nothing the provider gave is kept but who it said the
// person is; its tokens are not.

// SignAlgs: what an ID or logout token may be signed with — asymmetric
// algorithms only (not "none", nor HS*, keyed with the client's secret).
var SignAlgs = []string{"RS256", "RS384", "RS512", "PS256", "PS384", "PS512", "ES256", "ES384", "ES512", "EdDSA"}

const (
	flowTTL   = 10 * time.Minute // from the button to back: what a sign-in may take at the provider
	flowCap   = 10000            // flows under way at once
	skew      = time.Minute      // clocks allowed apart
	iatMaxAge = 10 * time.Minute // an ID token issued longer ago is no fresh sign-in
	// a logout token issued longer ago is not taken
	logoutMaxAge = 2 * time.Minute
	jtiTTL       = 5 * time.Minute
	retryAfter   = 10 * time.Second // discovery tried again no sooner after a failure
	httpTimeout  = 10 * time.Second
	// BackchannelEvent is the event a logout token is for.
	BackchannelEvent = "http://schemas.openid.net/event/backchannel-logout"
)

// SSOError is why single sign-on did not get someone in, as a word of a
// fixed list (Reason) the page says it in, and the cause, never shown.
type SSOError struct {
	Reason string
	Err    error
}

func (e *SSOError) Error() string {
	if e.Err != nil {
		return "sso: " + e.Reason + ": " + e.Err.Error()
	}
	return "sso: " + e.Reason
}
func (e *SSOError) Unwrap() error { return e.Err }

func ssoErr(reason string, err error) error { return &SSOError{Reason: reason, Err: err} }

// SSO reasons.
const (
	ReasonUnreachable   = "provider_unreachable" // discovery, its keys or its token endpoint not reached
	ReasonFlow          = "flow_expired"         // no such sign-in under way here (expired, used, another browser)
	ReasonDenied        = "provider_denied"      // the provider sent the person back without signing them in
	ReasonInvalidToken  = "invalid_token"        // what came back was not right
	ReasonNotAllowed    = "not_allowed"          // not in a group allowed in
	ReasonDisabled      = "disabled"             // the account is disabled
	ReasonIdentityTaken = "identity_taken"       // (linking) that identity is another account's
	ReasonLinked        = "already_linked"       // (linking) this account has an identity of the provider already
	ReasonLinkLogin     = "link_signed_out"      // (linking) not signed in as the account that asked any more
	ReasonBusy          = "busy"                 // too many sign-ins under way
)

// OIDC is single sign-on with the configured provider.
type OIDC struct {
	cfg    *config.OIDCConfig
	st     *store.Store
	svc    *Service
	client *http.Client
	now    func() time.Time

	mu       sync.Mutex
	provider *oidc.Provider
	idv      *oidc.IDTokenVerifier // ID tokens
	logoutv  *oidc.IDTokenVerifier // logout tokens: no expiry required
	algs     []string
	endSess  string
	userinfo bool
	tried    time.Time
	flows    map[string]*ssoFlow // by state
	jtis     map[string]time.Time
}

type ssoFlow struct {
	verifier  string // PKCE
	nonce     string
	bound     [32]byte // sha256 of the browser's flow cookie
	origin    string   // the site it began on: it ends there
	next      string
	linkUser  int64  // linking: the account that asked
	linkLogin string // linking: its login (it must still be signed in)
	expires   time.Time
}

// NewOIDC is single sign-on with cfg's provider, for svc's accounts. client:
// what the provider is asked by (nil: one with a timeout of its own).
func NewOIDC(cfg *config.OIDCConfig, svc *Service, client *http.Client) *OIDC {
	if client == nil {
		client = &http.Client{Timeout: httpTimeout}
	}
	return &OIDC{cfg: cfg, st: svc.store, svc: svc, client: client, now: time.Now, flows: map[string]*ssoFlow{}, jtis: map[string]time.Time{}}
}

// Button is the sign-in button's words ("": the page's own).
func (o *OIDC) Button() string { return o.cfg.Button }

// Issuer is the provider's issuer.
func (o *OIDC) Issuer() string { return o.cfg.Issuer }

func (o *OIDC) ctx(ctx context.Context) context.Context { return oidc.ClientContext(ctx, o.client) }

// ready: the provider discovered (once, and again after a failure, no
// sooner than retryAfter): a provider down when Corral starts does not stop it.
func (o *OIDC) ready(ctx context.Context) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.provider != nil {
		return nil
	}
	if !o.tried.IsZero() && o.now().Sub(o.tried) < retryAfter {
		return ssoErr(ReasonUnreachable, errors.New("the provider was not reached a moment ago"))
	}
	o.tried = o.now()
	p, err := oidc.NewProvider(o.ctx(ctx), o.cfg.Issuer)
	if err != nil {
		return ssoErr(ReasonUnreachable, err)
	}
	var meta struct {
		Algs       []string `json:"id_token_signing_alg_values_supported"`
		EndSession string   `json:"end_session_endpoint"`
		UserInfo   string   `json:"userinfo_endpoint"`
	}
	if err := p.Claims(&meta); err != nil {
		return ssoErr(ReasonUnreachable, err)
	}
	algs := []string{}
	for _, a := range meta.Algs {
		if slices.Contains(SignAlgs, a) {
			algs = append(algs, a)
		}
	}
	if len(meta.Algs) == 0 {
		algs = []string{"RS256"} // (the default the specification gives)
	}
	if len(algs) == 0 {
		return ssoErr(ReasonUnreachable, fmt.Errorf("the provider signs ID tokens with none of %v (it says %v)", SignAlgs, meta.Algs))
	}
	o.provider, o.algs, o.endSess, o.userinfo = p, algs, meta.EndSession, meta.UserInfo != ""
	// (Expiry with a minute's leeway: its clock and ours may be apart.)
	o.idv = p.Verifier(&oidc.Config{ClientID: o.cfg.ClientID, SupportedSigningAlgs: algs, Now: func() time.Time { return o.now().Add(-skew) }})
	o.logoutv = p.Verifier(&oidc.Config{ClientID: o.cfg.ClientID, SupportedSigningAlgs: algs, SkipExpiryCheck: true})
	return nil
}

func (o *OIDC) oauth(origin string) *oauth2.Config {
	return &oauth2.Config{ClientID: o.cfg.ClientID, ClientSecret: o.cfg.ClientSecret, Endpoint: o.provider.Endpoint(),
		RedirectURL: origin + CallbackPath, Scopes: o.cfg.Scopes}
}

// CallbackPath is where the provider sends a person back to, on the site the sign-in began on.
const CallbackPath = "/api/auth/oidc/callback"

func random32() string {
	b := make([]byte, 32)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// Begin starts a sign-in (linkUser 0) or a linking of linkUser's account
// (signed in by linkLogin) on origin, to go to next after: the provider's
// URL to send the browser to, and the value of the flow cookie the browser
// must bring back (the sign-in is bound to it: one begun in another browser
// is not finished in this one).
func (o *OIDC) Begin(ctx context.Context, origin, next string, linkUser int64, linkLogin string) (authURL, cookie string, err error) {
	if err := o.ready(ctx); err != nil {
		return "", "", err
	}
	state, nonce, cookie := random32(), random32(), random32()
	verifier := oauth2.GenerateVerifier()
	now := o.now()
	o.mu.Lock()
	if len(o.flows) >= flowCap {
		for k, f := range o.flows {
			if now.After(f.expires) {
				delete(o.flows, k)
			}
		}
	}
	if len(o.flows) >= flowCap {
		o.mu.Unlock()
		return "", "", ssoErr(ReasonBusy, nil)
	}
	o.flows[state] = &ssoFlow{verifier: verifier, nonce: nonce, bound: sha256.Sum256([]byte(cookie)), origin: origin, next: next,
		linkUser: linkUser, linkLogin: linkLogin, expires: now.Add(flowTTL)}
	o.mu.Unlock()
	return o.oauth(origin).AuthCodeURL(state, oauth2.S256ChallengeOption(verifier), oidc.Nonce(nonce)), cookie, nil
}

// Claimed is who the provider said came back: its issuer and subject, its
// session (sid), the person's groups (by the configured claim) and what it
// said of them to show; and the sign-in's own: where to go after, and the
// account being linked (0: a sign-in).
type Claimed struct {
	Issuer, Subject, Sid  string
	Groups                []string
	Username, Email, Name string
	Next                  string
	LinkUser              int64
	LinkLogin             string
}

// Finish ends a sign-in the provider sent back to origin: state and code as
// it gave them, cookie the browser's flow cookie. The flow is taken once,
// whatever becomes of it: a callback replayed finds none.
func (o *OIDC) Finish(ctx context.Context, origin, state, code, cookie string) (*Claimed, error) {
	c, err := o.finish(ctx, origin, state, code, cookie)
	return c, err
}

// finish is Finish; on a failure after the flow was taken, what of it the
// caller needs to say so (which page it began from) comes back with the error.
func (o *OIDC) finish(ctx context.Context, origin, state, code, cookie string) (*Claimed, error) {
	o.mu.Lock()
	f := o.flows[state]
	delete(o.flows, state)
	o.mu.Unlock()
	if f == nil || state == "" {
		return nil, ssoErr(ReasonFlow, nil)
	}
	base := &Claimed{Next: f.next, LinkUser: f.linkUser, LinkLogin: f.linkLogin}
	if o.now().After(f.expires) || f.origin != origin {
		return base, ssoErr(ReasonFlow, nil)
	}
	got := sha256.Sum256([]byte(cookie))
	if cookie == "" || subtle.ConstantTimeCompare(got[:], f.bound[:]) != 1 {
		return base, ssoErr(ReasonFlow, errors.New("not the browser it began in"))
	}
	if code == "" {
		return base, ssoErr(ReasonInvalidToken, errors.New("no code"))
	}
	if err := o.ready(ctx); err != nil {
		return base, err
	}
	tok, err := o.oauth(origin).Exchange(o.ctx(ctx), code, oauth2.VerifierOption(f.verifier))
	if err != nil {
		var re *oauth2.RetrieveError
		if errors.As(err, &re) {
			return base, ssoErr(ReasonInvalidToken, fmt.Errorf("token endpoint: %s", re.ErrorCode))
		}
		return base, ssoErr(ReasonUnreachable, err)
	}
	raw, _ := tok.Extra("id_token").(string)
	if raw == "" {
		return base, ssoErr(ReasonInvalidToken, errors.New("no id_token"))
	}
	idt, err := o.idv.Verify(o.ctx(ctx), raw)
	if err != nil {
		if strings.Contains(err.Error(), "fetching keys") || strings.Contains(err.Error(), "failed to verify id token signature") && strings.Contains(err.Error(), "get keys") {
			return base, ssoErr(ReasonUnreachable, err)
		}
		return base, ssoErr(ReasonInvalidToken, err)
	}
	var claims map[string]any
	if err := idt.Claims(&claims); err != nil {
		return base, ssoErr(ReasonInvalidToken, err)
	}
	if err := o.checkID(idt, claims, f.nonce); err != nil {
		return base, ssoErr(ReasonInvalidToken, err)
	}
	c := &Claimed{Issuer: idt.Issuer, Subject: idt.Subject, Sid: str(claims["sid"]), Username: str(claims["preferred_username"]),
		Email: str(claims["email"]), Name: str(claims["name"]), Next: f.next, LinkUser: f.linkUser, LinkLogin: f.linkLogin}
	groups, found := claimAt(claims, o.cfg.GroupsClaim)
	if !found && (len(o.cfg.Allowed) > 0 || len(o.cfg.Admins) > 0) && o.userinfo {
		// (Not in the ID token: the provider asked, by the access token this once.)
		if ui, err := o.provider.UserInfo(o.ctx(ctx), oauth2.StaticTokenSource(tok)); err == nil && ui.Subject == idt.Subject {
			var uc map[string]any
			if ui.Claims(&uc) == nil {
				groups, _ = claimAt(uc, o.cfg.GroupsClaim)
			}
		}
	}
	c.Groups = groups
	return c, nil
}

// checkID: what go-oidc leaves of an ID token's checks — its subject, its
// authorized party, when it was issued, and its nonce.
func (o *OIDC) checkID(idt *oidc.IDToken, claims map[string]any, nonce string) error {
	if idt.Subject == "" || len(idt.Subject) > 255 {
		return errors.New("no subject")
	}
	azp, hasAzp := claims["azp"]
	if (len(idt.Audience) > 1 || hasAzp) && str(azp) != o.cfg.ClientID {
		return errors.New("authorized party is not this client")
	}
	now := o.now()
	if idt.IssuedAt.IsZero() || idt.IssuedAt.After(now.Add(skew)) || idt.IssuedAt.Before(now.Add(-iatMaxAge)) {
		return errors.New("issued at an unlikely time")
	}
	if subtle.ConstantTimeCompare([]byte(idt.Nonce), []byte(nonce)) != 1 || nonce == "" {
		return errors.New("nonce does not match")
	}
	return nil
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

// claimAt: the groups at a claim's name, or a dotted path to one
// (realm_access.roles): a list of strings, or one string.
func claimAt(claims map[string]any, path string) ([]string, bool) {
	var v any = claims
	for _, part := range strings.Split(path, ".") {
		m, ok := v.(map[string]any)
		if !ok {
			return nil, false
		}
		if v, ok = m[part]; !ok {
			return nil, false
		}
	}
	switch t := v.(type) {
	case string:
		return []string{t}, true
	case []any:
		out := make([]string, 0, len(t))
		for _, x := range t {
			if s, ok := x.(string); ok {
				out = append(out, s)
			}
		}
		return out, true
	}
	return nil, true
}

func inAny(groups, of []string) bool {
	for _, g := range groups {
		if slices.Contains(of, g) {
			return true
		}
	}
	return false
}

// SignedIn is what SignIn made of a sign-in: the account, whether it was
// made now, and its role as it was before (to say a change).
type SignedIn struct {
	User     *store.User
	Created  bool
	RoleWas  string
	RoleKept bool // the provider's groups would have made it no administrator, but it is the last one
}

// SignIn lets c's person in: its account (made now, the first time — no
// existing one taken, whoever's its name), refused where they are not in a
// group allowed in or the account is disabled; its role by the
// administrators' groups where they are configured (never the last
// administrator made none).
func (o *OIDC) SignIn(ctx context.Context, c *Claimed) (*SignedIn, error) {
	if len(o.cfg.Allowed) > 0 && !inAny(c.Groups, o.cfg.Allowed) {
		return nil, ssoErr(ReasonNotAllowed, nil)
	}
	id := &store.OIDCIdentity{Issuer: c.Issuer, Subject: c.Subject, UsernameHint: limit(c.Username, 128), Email: limit(c.Email, 254), Name: limit(c.Name, 128)}
	want := ""
	if len(o.cfg.Admins) > 0 {
		want = store.RoleMember
		if inAny(c.Groups, o.cfg.Admins) {
			want = store.RoleAdmin
		}
	}
	out := &SignedIn{}
	b, err := o.st.IdentityBySubject(ctx, c.Issuer, c.Subject)
	switch {
	case errors.Is(err, store.ErrNotFound):
		role := want
		if role == "" {
			role = store.RoleMember
		}
		display := c.Name
		if display == "" {
			display = c.Username
		}
		u, err := o.st.CreateSSOUser(ctx, id, SuggestUsername(c.Username, c.Email, c.Subject), limit(display, 64), role)
		if errors.Is(err, store.ErrIdentityTaken) {
			return o.SignIn(ctx, c) // (made meanwhile, by another sign-in of the same: that one)
		}
		if err != nil {
			return nil, err
		}
		out.User, out.Created, out.RoleWas = u, true, role
		return out, nil
	case err != nil:
		return nil, err
	}
	u, err := o.st.UserByID(ctx, b.UserID)
	if err != nil {
		return nil, err
	}
	if u.Disabled {
		return nil, ssoErr(ReasonDisabled, nil)
	}
	_ = o.st.TouchIdentity(ctx, id)
	out.User, out.RoleWas = u, u.Role
	if want != "" && u.Role != want {
		switch err := o.st.UpdateUserGuarded(ctx, u.ID, &want, nil); {
		case errors.Is(err, store.ErrLastAdmin):
			out.RoleKept = true
		case err != nil:
			return nil, err
		default:
			if out.User, err = o.st.UserByID(ctx, u.ID); err != nil {
				return nil, err
			}
		}
	}
	return out, nil
}

func limit(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}

// SuggestUsername: a user name for an account single sign-on makes — the
// provider's preferred_username, else the email's part before its @, made
// to fit Corral's rules; else "sso-" and the first of a hash of the
// subject. (Only a suggestion: an account with it already is never taken —
// CreateSSOUser adds -2, -3 …)
func SuggestUsername(preferred, email, subject string) string {
	for _, c := range []string{preferred, strings.SplitN(email, "@", 2)[0]} {
		var b strings.Builder
		for _, r := range c {
			switch {
			case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
				b.WriteRune(r)
			case r == ' ' || r == '@' || r == '+':
				b.WriteRune('-')
			}
		}
		s := strings.TrimLeft(b.String(), "._-")
		if len(s) > 26 {
			s = s[:26]
		}
		if CheckUsername(s) == nil {
			return s
		}
	}
	h := sha256.Sum256([]byte(subject))
	return "sso-" + hex.EncodeToString(h[:4])
}

// StartSession makes the login of an account single sign-on let in, noted
// as made by it (for the provider to end): only if the account is not
// disabled, and still has the password it had (where it has one) — not one
// disabled or changed meanwhile.
func (o *OIDC) StartSession(ctx context.Context, u *store.User, c *Claimed, ip, ua string) (string, string, error) {
	tok, hash, err := NewToken()
	if err != nil {
		return "", "", err
	}
	if len(ua) > 300 {
		ua = ua[:300]
	}
	if err := o.st.CreateLoginSessionIfCurrent(ctx, hash, u.ID, u.PasswordHash(), ip, ua, false); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return "", "", ssoErr(ReasonDisabled, err)
		}
		return "", "", err
	}
	if err := o.st.RecordOIDCLogin(ctx, hash, c.Issuer, c.Subject, limit(c.Sid, 255)); err != nil {
		_ = o.st.DeleteLoginSession(context.WithoutCancel(ctx), hash)
		return "", "", err
	}
	return tok, hash, nil
}

// Link binds c's identity to the account that asked (its login still
// signed in, checked by the caller).
func (o *OIDC) Link(ctx context.Context, c *Claimed) error {
	id := &store.OIDCIdentity{Issuer: c.Issuer, Subject: c.Subject, UsernameHint: limit(c.Username, 128), Email: limit(c.Email, 254), Name: limit(c.Name, 128)}
	switch err := o.st.LinkIdentity(ctx, c.LinkUser, id); {
	case errors.Is(err, store.ErrIdentityTaken):
		return ssoErr(ReasonIdentityTaken, nil)
	case errors.Is(err, store.ErrAlreadyLinked):
		return ssoErr(ReasonLinked, nil)
	default:
		return err
	}
}

// EndSessionURL: where a browser signing out of a login single sign-on
// made is sent to sign out at the provider too (CORRAL_OIDC_END_SESSION,
// and the provider has an end_session_endpoint), back to after at origin;
// "" for none.
func (o *OIDC) EndSessionURL(ctx context.Context, origin string) string {
	if !o.cfg.EndSession || o.ready(ctx) != nil || o.endSess == "" {
		return ""
	}
	sep := "?"
	if strings.Contains(o.endSess, "?") {
		sep = "&"
	}
	return o.endSess + sep + "client_id=" + url.QueryEscape(o.cfg.ClientID) + "&post_logout_redirect_uri=" + url.QueryEscape(origin+"/login")
}

// Logout is a back-channel logout token the provider sent: the logins it
// ends (their accounts), once it is verified — its signature by the
// provider's keys, its issuer, that Corral is its audience, when it was
// issued (and its expiry where it has one), its event, no nonce, a subject
// or a session, a jti not seen before, and a type of a logout token where it
// says one.
func (o *OIDC) Logout(ctx context.Context, raw string) (map[string]int64, error) {
	if err := o.ready(ctx); err != nil {
		return nil, err
	}
	if typ, err := jwtType(raw); err != nil || (typ != "" && !strings.EqualFold(typ, "logout+jwt") && !strings.EqualFold(typ, "JWT")) {
		return nil, errors.New("not a logout token")
	}
	t, err := o.logoutv.Verify(o.ctx(ctx), raw)
	if err != nil {
		return nil, err
	}
	var claims struct {
		Sid    string                     `json:"sid"`
		Jti    string                     `json:"jti"`
		Exp    *json.Number               `json:"exp"`
		Nonce  *string                    `json:"nonce"`
		Events map[string]json.RawMessage `json:"events"`
	}
	if err := t.Claims(&claims); err != nil {
		return nil, err
	}
	now := o.now()
	switch {
	case t.IssuedAt.IsZero() || t.IssuedAt.After(now.Add(skew)) || t.IssuedAt.Before(now.Add(-logoutMaxAge-skew)):
		return nil, errors.New("issued at an unlikely time")
	case claims.Nonce != nil:
		return nil, errors.New("a logout token has no nonce")
	case claims.Events == nil || len(claims.Events[BackchannelEvent]) == 0 || claims.Events[BackchannelEvent][0] != '{':
		return nil, errors.New("not a back-channel logout event")
	case t.Subject == "" && claims.Sid == "":
		return nil, errors.New("neither a subject nor a session")
	case claims.Jti == "":
		return nil, errors.New("no jti")
	}
	if claims.Exp != nil {
		if e, err := claims.Exp.Int64(); err != nil || time.Unix(e, 0).Before(now.Add(-skew)) {
			return nil, errors.New("expired")
		}
	}
	o.mu.Lock()
	for k, at := range o.jtis {
		if now.Sub(at) > jtiTTL {
			delete(o.jtis, k)
		}
	}
	key := t.Issuer + "\x00" + claims.Jti
	_, seen := o.jtis[key]
	if !seen {
		o.jtis[key] = now
	}
	o.mu.Unlock()
	if seen {
		return nil, errors.New("this logout token was used already")
	}
	return o.st.EndOIDCLogins(ctx, t.Issuer, t.Subject, claims.Sid)
}

// jwtType: the "typ" of a JWS's protected header ("" where it has none).
func jwtType(raw string) (string, error) {
	sig, err := jose.ParseSigned(raw, []jose.SignatureAlgorithm{"RS256", "RS384", "RS512", "PS256", "PS384", "PS512", "ES256", "ES384", "ES512", "EdDSA"})
	if err != nil || len(sig.Signatures) != 1 {
		return "", errors.New("not a signed token")
	}
	typ, _ := sig.Signatures[0].Protected.ExtraHeaders[jose.HeaderKey("typ")].(string)
	return typ, nil
}
