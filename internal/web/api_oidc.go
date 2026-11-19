package web

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/ihainan/corral/internal/auth"
	"github.com/ihainan/corral/internal/store"
)

// Single sign-on (OIDC), the gateway's side: a sign-in begun by the page's
// button (or a linking from Settings), the provider's callback, an account's
// binding undone, and the provider's back-channel logout. See auth/oidc.go
// for the protocol; here, where a browser is sent and what is said to it.

// ssoCookieName: the browser's half of a sign-in under way (its value bound
// to the flow): host-only, as the login's own, and not it.
func (s *Server) ssoCookieName() string {
	if s.cfg.SecureCookies() {
		return "__Host-corral_sso"
	}
	return "corral_sso"
}

func (s *Server) setSSOCookie(w http.ResponseWriter, v string, maxAge int) {
	http.SetCookie(w, &http.Cookie{Name: s.ssoCookieName(), Value: v, Path: "/", HttpOnly: true, Secure: s.cfg.SecureCookies(),
		SameSite: http.SameSiteLaxMode, MaxAge: maxAge})
}

// ssoOrigin: the site a sign-in is begun and ended on — the one the request
// came by (by its Host), where that is one of the main site's ways in
// (CORRAL_PUBLIC_URL, CORRAL_EXTRA_URLS); else CORRAL_PUBLIC_URL's.
func (s *Server) ssoOrigin(r *http.Request) string {
	if o := s.cfg.PublicURL.Scheme + "://" + r.Host; s.cfg.SameSite(o) {
		return o
	}
	return s.cfg.Origin()
}

// safeNext: where to go after signing in — a path of this site only (one
// "/" first, no "//" nor "/\" nor a backslash anywhere, no control
// character): else "/". (An address of another site would make the button a
// way to send people there.)
func safeNext(p string) string {
	if p == "" || p[0] != '/' || strings.HasPrefix(p, "//") || strings.ContainsAny(p, "\\") || len(p) > 2048 {
		return "/"
	}
	for _, r := range p {
		if r < 0x20 || r == 0x7f {
			return "/"
		}
	}
	if u, err := url.Parse(p); err != nil || u.Scheme != "" || u.Host != "" {
		return "/"
	}
	return p
}

// ssoAllowed counts one sign-in step of the address (kind: which), with the
// sign-in's limits in a count of its own.
func (s *Server) ssoAllowed(r *http.Request, kind string) bool {
	return s.auth.AllowAttempt("sso-" + kind + ":" + s.clientIP(r))
}

// GET /api/auth/oidc/start?next=/path: off to the provider to sign in.
func (s *Server) apiOIDCStart(w http.ResponseWriter, r *http.Request) {
	if s.oidc == nil {
		writeError(w, http.StatusNotFound, "not_found", "single sign-on is not set up")
		return
	}
	if !s.ssoAllowed(r, "start") {
		s.ssoBack(w, r, false, "rate_limited")
		return
	}
	u, cookie, err := s.oidc.Begin(r.Context(), s.ssoOrigin(r), safeNext(r.URL.Query().Get("next")), 0, "")
	if err != nil {
		s.ssoFailed(w, r, nil, err, false)
		return
	}
	s.setSSOCookie(w, cookie, int((10 * time.Minute).Seconds()))
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, u, http.StatusFound)
}

// POST /api/me/oidc/link: the provider's address to link the account to an
// identity of it (the browser goes there; it comes back to the callback).
func (s *Server) apiOIDCLink(w http.ResponseWriter, r *http.Request) {
	if s.oidc == nil {
		writeError(w, http.StatusNotFound, "not_found", "single sign-on is not set up")
		return
	}
	if !s.ssoAllowed(r, "start") {
		writeError(w, http.StatusTooManyRequests, "rate_limited", "too many attempts, try again later")
		return
	}
	u, cookie, err := s.oidc.Begin(r.Context(), s.ssoOrigin(r), "/settings", currentUser(r).ID, loginHash(r))
	if err != nil {
		var se *auth.SSOError
		if errors.As(err, &se) {
			writeError(w, http.StatusBadGateway, se.Reason, "single sign-on is not available now")
			return
		}
		s.internalError(w, r, err)
		return
	}
	s.setSSOCookie(w, cookie, int((10 * time.Minute).Seconds()))
	writeJSON(w, http.StatusOK, map[string]any{"url": u})
}

// GET /api/auth/oidc/callback: back from the provider — signed in (its
// login made as a password's is), or linked; else back to the page it
// began from, with why.
func (s *Server) apiOIDCCallback(w http.ResponseWriter, r *http.Request) {
	if s.oidc == nil {
		writeError(w, http.StatusNotFound, "not_found", "single sign-on is not set up")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	cookie := ""
	if c, err := r.Cookie(s.ssoCookieName()); err == nil {
		cookie = c.Value
	}
	s.setSSOCookie(w, "", -1) // (one use: gone whatever becomes of it)
	if !s.ssoAllowed(r, "callback") {
		s.ssoBack(w, r, false, "rate_limited")
		return
	}
	q := r.URL.Query()
	if e := q.Get("error"); e != "" {
		// (The provider's own word on why — access_denied, login_required — is not passed on.)
		c, _ := s.oidc.Finish(r.Context(), s.ssoOrigin(r), q.Get("state"), "", cookie)
		s.ssoFailed(w, r, c, &auth.SSOError{Reason: auth.ReasonDenied}, c != nil && c.LinkUser != 0)
		return
	}
	c, err := s.oidc.Finish(r.Context(), s.ssoOrigin(r), q.Get("state"), q.Get("code"), cookie)
	link := c != nil && c.LinkUser != 0
	if err != nil {
		s.ssoFailed(w, r, c, err, link)
		return
	}
	if link {
		s.ssoLink(w, r, c)
		return
	}
	in, err := s.oidc.SignIn(r.Context(), c)
	if err != nil {
		s.ssoFailed(w, r, c, err, false)
		return
	}
	tok, _, err := s.oidc.StartSession(r.Context(), in.User, c, s.clientIP(r), r.UserAgent())
	if err != nil {
		s.ssoFailed(w, r, c, err, false)
		return
	}
	d := map[string]any{"issuer": c.Issuer, "created": in.Created}
	if in.User.Role != in.RoleWas {
		d["role"], d["role_was"] = in.User.Role, in.RoleWas
	}
	if in.RoleKept {
		d["role_kept"] = "last administrator" // (the groups would have made it none)
	}
	s.auditAs(r, in.User, "auth.sso.login", in.User.Username, d)
	s.setSessionCookie(w, tok)
	http.Redirect(w, r, safeNext(c.Next), http.StatusFound)
}

// ssoLink binds the identity c to the account that asked — only while the
// login it asked from is still the one signed in here.
func (s *Server) ssoLink(w http.ResponseWriter, r *http.Request, c *auth.Claimed) {
	u, hash, err := s.auth.Authenticate(r.Context(), s.sessionCookie(r))
	if err != nil || u.ID != c.LinkUser || hash != c.LinkLogin {
		s.ssoFailed(w, r, c, &auth.SSOError{Reason: auth.ReasonLinkLogin}, true)
		return
	}
	if err := s.oidc.Link(r.Context(), c); err != nil {
		s.ssoFailed(w, r, c, err, true)
		return
	}
	s.auditAs(r, u, "auth.sso.link", u.Username, map[string]any{"issuer": c.Issuer})
	http.Redirect(w, r, "/settings?sso=linked", http.StatusFound)
}

// ssoFailed says why single sign-on did not get someone in (audited, with
// the reason and nothing the provider sent), back on the page it began from.
func (s *Server) ssoFailed(w http.ResponseWriter, r *http.Request, c *auth.Claimed, err error, link bool) {
	reason := "error"
	var se *auth.SSOError
	if errors.As(err, &se) {
		reason = se.Reason
	} else if r.Context().Err() == nil {
		s.log.Error("single sign-on failed", "err", err)
	}
	d := map[string]any{"reason": reason}
	target := ""
	if c != nil {
		if c.Issuer != "" {
			d["issuer"] = c.Issuer
		}
		target = truncate(c.Username, 64)
	}
	if link {
		d["link"] = true
	}
	s.auditAs(r, nil, "auth.sso.denied", target, d)
	s.ssoBack(w, r, link, reason)
}

func (s *Server) ssoBack(w http.ResponseWriter, r *http.Request, link bool, reason string) {
	page := "/login"
	if link {
		page = "/settings"
	}
	http.Redirect(w, r, page+"?sso_error="+url.QueryEscape(reason), http.StatusFound)
}

// DELETE /api/me/oidc: the account's binding undone — only where it has a
// password left to sign in with.
func (s *Server) apiOIDCUnlink(w http.ResponseWriter, r *http.Request) {
	if s.oidc == nil {
		writeError(w, http.StatusNotFound, "not_found", "single sign-on is not set up")
		return
	}
	u := currentUser(r)
	s.unlink(w, r, u, "self")
}

// DELETE /api/admin/users/{id}/oidc: an administrator undoes an account's
// binding — not of one with no password (it would have no way in: disable
// it, or give it a password first).
func (s *Server) apiAdminOIDCUnlink(w http.ResponseWriter, r *http.Request) {
	if s.oidc == nil {
		writeError(w, http.StatusNotFound, "not_found", "single sign-on is not set up")
		return
	}
	id, ok := pathID(r)
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "no such user")
		return
	}
	u, err := s.store.UserByID(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "no such user")
		return
	}
	s.unlink(w, r, u, "admin")
}

func (s *Server) unlink(w http.ResponseWriter, r *http.Request, u *store.User, by string) {
	// (Its password not one it may sign in with — CORRAL_LOCAL_LOGIN, by its role as it is
	// in the unlink's own transaction — single sign-on is its way in still.)
	ended, err := s.store.UnlinkIdentity(r.Context(), u.ID, s.oidc.Issuer(), s.auth.RoleMayUsePassword)
	switch {
	case errors.Is(err, store.ErrNoWayIn):
		writeError(w, http.StatusConflict, "no_way_in", "this account may not sign in with its password, so single sign-on is its only way in")
		return
	case errors.Is(err, store.ErrNoPassword):
		writeError(w, http.StatusConflict, "sso_only", "this account can only sign in with single sign-on")
		return
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_linked", "this account is not linked")
		return
	case err != nil:
		s.internalError(w, r, err)
		return
	}
	for _, h := range ended {
		s.closeSockets(u.ID, h)
	}
	s.audit(r, "auth.sso.unlink", u.Username, map[string]any{"by": by, "issuer": s.oidc.Issuer(), "logins_ended": len(ended)})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// POST /api/auth/oidc/backchannel: the provider ends logins it made (OIDC
// Back-Channel Logout: a logout token, form-encoded). Called by the
// provider's server, not a browser: no site check, no login.
func (s *Server) apiOIDCBackchannel(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if s.oidc == nil {
		writeError(w, http.StatusNotFound, "not_found", "single sign-on is not set up")
		return
	}
	// (Counted are the ones refused only: a provider signing many people out
	// at once is not held back, and what it ends is ended.)
	key := "sso-backchannel:" + s.clientIP(r)
	if s.auth.Exhausted(key) {
		writeError(w, http.StatusTooManyRequests, "rate_limited", "too many requests")
		return
	}
	if err := r.ParseForm(); err != nil {
		s.auth.AllowAttempt(key)
		writeError(w, http.StatusBadRequest, "invalid_request", "the logout token could not be read")
		return
	}
	ended, err := s.oidc.Logout(r.Context(), r.PostForm.Get("logout_token"))
	if err != nil {
		s.auth.AllowAttempt(key)
		s.audit(r, "auth.sso.denied", "", map[string]any{"reason": "bad_logout_token"})
		writeError(w, http.StatusBadRequest, "invalid_request", "the logout token was not accepted")
		return
	}
	for h, uid := range ended {
		s.closeSockets(uid, h)
	}
	s.audit(r, "auth.sso.logout", "", map[string]any{"issuer": s.oidc.Issuer(), "logins_ended": len(ended)})
	w.WriteHeader(http.StatusOK)
}

// ssoLogoutURL: where a browser signing out of login hash is sent next, to
// sign out at the provider too ("" for none: not a login single sign-on
// made, or not configured).
func (s *Server) ssoLogoutURL(ctx context.Context, r *http.Request, hash string) string {
	if s.oidc == nil || hash == "" {
		return ""
	}
	if _, _, _, ok, err := s.store.OIDCLoginOf(ctx, hash); err != nil || !ok {
		return ""
	}
	return s.oidc.EndSessionURL(ctx, s.ssoOrigin(r))
}
