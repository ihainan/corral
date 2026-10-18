package web

// Port forwarding: "<port>-<id>.<forward domain>" serves a port on a target
// host to the browser, through the SSH connection the gateway holds.
//
// Each forwarded site lives on its own subdomain so it cannot read or act as
// the main site (separate origin; the main login cookie is __Host- scoped).
// Access is handed over with a one-time ticket: the main site, where the user
// is signed in, issues it; the subdomain swaps it for its own cookie.

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/ihainan/corral/internal/auth"
	"github.com/ihainan/corral/internal/sshx"
	"github.com/ihainan/corral/internal/store"
)

const (
	fwdCookieInsecure = "corral_fwd"
	fwdAuthPath       = "/__corral/auth"
	ticketTTL         = time.Minute
	fwdTokenTTL       = 12 * time.Hour
	fwdCheckEvery     = 30 * time.Second
	fwdConnIdle       = 10 * time.Minute
)

var fwdLabelRE = regexp.MustCompile(`^([0-9]{1,5})-([a-z2-9]{4})$`)

type fwdGrant struct {
	userID    int64
	loginHash string
	forwardID string
	ret       string // tickets only: where to land on the subdomain, fixed when issued
	expires   time.Time
	checked   time.Time
}

// forwarder holds tickets, subdomain grants and SSH connections opened only
// for forwarding (when the user has no terminal open to that host).
type forwarder struct {
	mu      sync.Mutex
	tickets map[string]*fwdGrant // one-time, keyed by ticket hash
	grants  map[string]*fwdGrant // per subdomain cookie, keyed by token hash
	conns   map[[2]int64]*fwdConn
}

type fwdConn struct {
	client   *ssh.Client
	lastUsed time.Time
}

// fwdCookieName uses the __Host- prefix on HTTPS so a forwarded app cannot
// plant a same-named cookie for all subdomains.
func (s *Server) fwdCookieName() string {
	if s.cfg.SecureCookies() {
		return "__Host-corral_fwd"
	}
	return fwdCookieInsecure
}

func newForwarder() *forwarder {
	return &forwarder{tickets: map[string]*fwdGrant{}, grants: map[string]*fwdGrant{}, conns: map[[2]int64]*fwdConn{}}
}

// forwardLabel extracts port and id when host is a forwarding subdomain.
func (s *Server) forwardLabel(host string) (port int, id string, ok bool) {
	if s.cfg.ForwardDomain == "" {
		return 0, "", false
	}
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	label, found := strings.CutSuffix(host, "."+s.cfg.ForwardDomain)
	if !found {
		return 0, "", false
	}
	m := fwdLabelRE.FindStringSubmatch(label)
	if m == nil {
		return 0, "", false
	}
	port, _ = strconv.Atoi(m[1])
	if port < 1 || port > 65535 {
		return 0, "", false
	}
	return port, m[2], true
}

func (s *Server) forwardOrigin(f *store.Forward) string { return s.forwardOriginFor(f.Port, f.ID) }

// forwardOriginFor is the exact Origin a browser sends for a forward's pages.
func (s *Server) forwardOriginFor(port int, id string) string {
	o := s.cfg.PublicURL.Scheme + "://" + strconv.Itoa(port) + "-" + id + "." + s.cfg.ForwardDomain
	if p := s.cfg.PublicURL.Port(); p != "" {
		o += ":" + p // non-default port (development setups)
	}
	return o
}

// forwardCrossSite reports a request that a page of another origin made,
// including another forwarded port: those are the same site, so the
// SameSite=Lax cookie comes along. Any Origin must be the forward's own;
// WebSocket upgrades and state-changing requests without one are refused when
// the browser says they came from elsewhere.
func forwardCrossSite(r *http.Request, own string) bool {
	if origin := r.Header.Get("Origin"); origin != "" {
		return origin != own
	}
	upgrade := strings.EqualFold(r.Header.Get("Upgrade"), "websocket")
	if !upgrade && (r.Method == http.MethodGet || r.Method == http.MethodHead || r.Method == http.MethodOptions) {
		return false
	}
	switch r.Header.Get("Sec-Fetch-Site") {
	case "", "same-origin", "none":
		return false
	}
	return true
}

// safeReturnPath accepts a path on the same site to redirect to: no scheme or
// host, no "//" or backslash (browsers read both as another host), no control
// characters, also after percent-decoding.
func safeReturnPath(p string) bool {
	if u, err := url.Parse(p); err != nil || u.Scheme != "" || u.Host != "" || u.Opaque != "" || u.User != nil {
		return false
	}
	for range 3 {
		if !strings.HasPrefix(p, "/") || strings.HasPrefix(p, "//") || strings.ContainsFunc(p, func(r rune) bool {
			return r == '\\' || r < 0x20 || r == 0x7f
		}) {
			return false
		}
		dec, err := url.PathUnescape(p)
		if err != nil {
			return false
		}
		if dec == p {
			return true
		}
		p = dec
	}
	return false // still encoded after three rounds: not a path a browser produces
}

// canUseForward applies sharing: the owner always, other active users when shared.
func canUseForward(u *store.User, f *store.Forward) bool {
	return u.ID == f.OwnerID || f.Sharing == store.ShareUsers
}

// GET /fwd/{id}?return=/path on the main site: signed-in users get a ticket
// and are sent to the subdomain; others go to sign-in first.
func (s *Server) handleForwardOpen(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	ret := r.URL.Query().Get("return")
	if !safeReturnPath(ret) {
		ret = "/"
	}
	u, hash, err := s.auth.Authenticate(r.Context(), s.sessionCookie(r))
	if err != nil {
		http.Redirect(w, r, "/login?next="+url.QueryEscape(r.URL.RequestURI()), http.StatusFound)
		return
	}
	f, err := s.store.ForwardByID(r.Context(), id)
	if err != nil || !canUseForward(u, f) {
		http.Error(w, "This forwarded port does not exist or is not shared with you.", http.StatusNotFound)
		return
	}
	tok, tokHash, err := auth.NewToken()
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	s.fwd.mu.Lock()
	s.fwd.sweep()
	s.fwd.tickets[tokHash] = &fwdGrant{userID: u.ID, loginHash: hash, forwardID: f.ID, ret: ret, expires: time.Now().Add(ticketTTL)}
	s.fwd.mu.Unlock()
	q := url.Values{"ticket": {tok}}
	http.Redirect(w, r, s.forwardOrigin(f)+fwdAuthPath+"?"+q.Encode(), http.StatusFound)
}

// sweep drops expired tickets and grants; called with mu held.
func (f *forwarder) sweep() {
	now := time.Now()
	for k, g := range f.tickets {
		if now.After(g.expires) {
			delete(f.tickets, k)
		}
	}
	for k, g := range f.grants {
		if now.After(g.expires) {
			delete(f.grants, k)
		}
	}
}

// serveForward handles every request on a forwarding subdomain.
func (s *Server) serveForward(w http.ResponseWriter, r *http.Request, port int, id string) {
	if forwardCrossSite(r, s.forwardOriginFor(port, id)) {
		http.Error(w, "This request did not come from this forwarded port's own pages.", http.StatusForbidden)
		return
	}
	mainLogin := s.cfg.Origin() + "/fwd/" + id + "?return=" + url.QueryEscape(r.URL.RequestURI())

	if r.URL.Path == fwdAuthPath {
		s.fwd.mu.Lock()
		g := s.fwd.tickets[auth.HashToken(r.URL.Query().Get("ticket"))]
		if g != nil {
			delete(s.fwd.tickets, auth.HashToken(r.URL.Query().Get("ticket"))) // single use
		}
		s.fwd.mu.Unlock()
		if g == nil || time.Now().After(g.expires) || g.forwardID != id {
			http.Error(w, "This link has expired. Open the port again from Corral.", http.StatusForbidden)
			return
		}
		tok, tokHash, err := auth.NewToken()
		if err != nil {
			s.internalError(w, r, err)
			return
		}
		grant := &fwdGrant{userID: g.userID, loginHash: g.loginHash, forwardID: id, expires: time.Now().Add(fwdTokenTTL)}
		s.fwd.mu.Lock()
		s.fwd.grants[tokHash] = grant
		s.fwd.mu.Unlock()
		http.SetCookie(w, &http.Cookie{Name: s.fwdCookieName(), Value: tok, Path: "/", HttpOnly: true, Secure: s.cfg.SecureCookies(),
			SameSite: http.SameSiteLaxMode, MaxAge: int(fwdTokenTTL.Seconds())})
		// Only the path fixed when the ticket was issued; the query here is ignored.
		http.Redirect(w, r, g.ret, http.StatusFound)
		return
	}

	c, err := r.Cookie(s.fwdCookieName())
	if err != nil {
		s.forwardLoginRedirect(w, r, mainLogin)
		return
	}
	tokHash := auth.HashToken(c.Value)
	s.fwd.mu.Lock()
	g := s.fwd.grants[tokHash]
	s.fwd.mu.Unlock()
	if g == nil || g.forwardID != id || time.Now().After(g.expires) {
		s.forwardLoginRedirect(w, r, mainLogin)
		return
	}
	f, err := s.store.ForwardByID(r.Context(), id)
	if err != nil || f.Port != port {
		s.revokeGrant(tokHash)
		http.Error(w, "This forwarded port no longer exists.", http.StatusNotFound)
		return
	}
	// Re-check the sign-in and sharing periodically, not on every asset request.
	if time.Since(g.checked) > fwdCheckEvery {
		u, err := s.auth.CheckLogin(r.Context(), g.loginHash)
		if err != nil || u.ID != g.userID || !canUseForward(u, f) {
			s.revokeGrant(tokHash)
			s.forwardLoginRedirect(w, r, mainLogin)
			return
		}
		s.fwd.mu.Lock()
		g.checked = time.Now()
		s.fwd.mu.Unlock()
	}
	client, err := s.forwardClient(r.Context(), f)
	if err != nil {
		http.Error(w, "Cannot reach the host. Open a terminal to it in Corral, then try again.", http.StatusBadGateway)
		return
	}
	s.proxyTo(w, r, client, f)
}

func (s *Server) revokeGrant(tokHash string) {
	s.fwd.mu.Lock()
	delete(s.fwd.grants, tokHash)
	s.fwd.mu.Unlock()
}

// Navigations are sent to sign in; sub-resource requests just fail.
func (s *Server) forwardLoginRedirect(w http.ResponseWriter, r *http.Request, target string) {
	if r.Method == http.MethodGet && strings.Contains(r.Header.Get("Accept"), "text/html") {
		http.Redirect(w, r, target, http.StatusFound)
		return
	}
	http.Error(w, "Sign in to Corral to use this forwarded port.", http.StatusUnauthorized)
}

// forwardClient returns an SSH connection to the forward's host: a running
// terminal session's connection when there is one, otherwise a connection
// opened with the stored credential and closed after a while unused.
func (s *Server) forwardClient(ctx context.Context, f *store.Forward) (*ssh.Client, error) {
	for _, sess := range s.terms.Running(f.OwnerID, f.HostID) {
		return sess.Client(), nil
	}
	key := [2]int64{f.OwnerID, f.HostID}
	s.fwd.mu.Lock()
	if c := s.fwd.conns[key]; c != nil {
		c.lastUsed = time.Now()
		s.fwd.mu.Unlock()
		return c.client, nil
	}
	s.fwd.mu.Unlock()

	h, err := s.store.Host(ctx, f.OwnerID, f.HostID)
	if err != nil {
		return nil, err
	}
	if h.Auth == store.AuthAsk || h.CredentialID == nil {
		return nil, errors.New("no stored credential")
	}
	_, sec, err := s.openCredential(ctx, f.OwnerID, *h.CredentialID)
	if err != nil {
		return nil, err
	}
	k, err := s.store.HostKey(ctx, f.OwnerID, h.ID)
	if err != nil {
		return nil, errors.New("connect to this host once from a terminal first")
	}
	dialed, err := sshx.Dial(ctx, s.guard, sshx.Target{Address: h.Address, Port: h.Port, User: h.Username,
		Password: sec.Password, PrivateKey: []byte(sec.PrivateKey), Passphrase: []byte(sec.Passphrase),
		Pinned: &sshx.Pinned{KeyType: k.KeyType, KeyData: k.KeyData, Fingerprint: k.Fingerprint}})
	if err != nil {
		return nil, err
	}
	s.fwd.mu.Lock()
	if existing := s.fwd.conns[key]; existing != nil { // lost a race; keep the first
		s.fwd.mu.Unlock()
		dialed.Client.Close()
		return existing.client, nil
	}
	s.fwd.conns[key] = &fwdConn{client: dialed.Client, lastUsed: time.Now()}
	s.fwd.mu.Unlock()
	go func() {
		_ = dialed.Client.Wait()
		s.fwd.mu.Lock()
		if c := s.fwd.conns[key]; c != nil && c.client == dialed.Client {
			delete(s.fwd.conns, key)
		}
		s.fwd.mu.Unlock()
	}()
	return dialed.Client, nil
}

// reapForwardConns closes idle forwarding-only connections.
func (s *Server) reapForwardConns(stop <-chan struct{}) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
		}
		s.fwd.mu.Lock()
		s.fwd.sweep()
		for k, c := range s.fwd.conns {
			if time.Since(c.lastUsed) > fwdConnIdle {
				c.client.Close()
				delete(s.fwd.conns, k)
			}
		}
		s.fwd.mu.Unlock()
	}
}

// closeUserForwards ends forwarding access for a user whose access was revoked.
func (s *Server) closeUserForwards(userID int64) {
	s.fwd.mu.Lock()
	defer s.fwd.mu.Unlock()
	for k, g := range s.fwd.grants {
		if g.userID == userID {
			delete(s.fwd.grants, k)
		}
	}
	for k, g := range s.fwd.tickets {
		if g.userID == userID {
			delete(s.fwd.tickets, k)
		}
	}
	for k, c := range s.fwd.conns {
		if k[0] == userID {
			c.client.Close()
			delete(s.fwd.conns, k)
		}
	}
}

// proxyTo relays the request to 127.0.0.1:<port> on the host. Requests look
// like local ones (Host and Origin are rewritten to localhost:<port>) so dev
// servers such as Vite and Next.js accept them without configuration.
func (s *Server) proxyTo(w http.ResponseWriter, r *http.Request, client *ssh.Client, f *store.Forward) {
	target := "127.0.0.1:" + strconv.Itoa(f.Port)
	local := "localhost:" + strconv.Itoa(f.Port)
	public := s.forwardOrigin(f)
	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme = "http"
			pr.Out.URL.Host = target
			pr.Out.Host = local
			h := pr.Out.Header
			for _, k := range []string{"X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto", "X-Real-Ip", "Forwarded"} {
				h.Del(k)
			}
			if o := h.Get("Origin"); o != "" { // already checked to be this forward's own
				h.Set("Origin", "http://"+local)
			}
			if ref := h.Get("Referer"); strings.HasPrefix(ref, public) {
				h.Set("Referer", "http://"+local+strings.TrimPrefix(ref, public))
			}
			stripCookie(h, s.fwdCookieName())
		},
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return client.DialContext(ctx, "tcp", target)
			},
			MaxIdleConnsPerHost:   4,
			IdleConnTimeout:       30 * time.Second,
			ResponseHeaderTimeout: 2 * time.Minute,
		},
		FlushInterval: -1, // stream server-sent events and long responses as they arrive
		ModifyResponse: func(res *http.Response) error {
			if loc := res.Header.Get("Location"); loc != "" {
				for _, from := range []string{"http://" + local, "http://127.0.0.1:" + strconv.Itoa(f.Port), "http://0.0.0.0:" + strconv.Itoa(f.Port)} {
					if strings.HasPrefix(loc, from) {
						res.Header.Set("Location", public+strings.TrimPrefix(loc, from))
					}
				}
			}
			// Cookies set by the forwarded app stay on its own subdomain.
			if cs := res.Header.Values("Set-Cookie"); len(cs) > 0 {
				res.Header.Del("Set-Cookie")
				for _, c := range cs {
					res.Header.Add("Set-Cookie", dropCookieDomain(c))
				}
			}
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			http.Error(w, "Nothing is answering on port "+strconv.Itoa(f.Port)+" of the host.", http.StatusBadGateway)
		},
	}
	rp.ServeHTTP(w, r)
}

func stripCookie(h http.Header, name string) {
	cookies := h.Values("Cookie")
	if len(cookies) == 0 {
		return
	}
	var keep []string
	for _, line := range cookies {
		for _, part := range strings.Split(line, ";") {
			p := strings.TrimSpace(part)
			if p == "" || strings.HasPrefix(p, name+"=") || strings.HasPrefix(p, "__Host-corral") || strings.HasPrefix(p, "corral_") {
				continue
			}
			keep = append(keep, p)
		}
	}
	h.Del("Cookie")
	if len(keep) > 0 {
		h.Set("Cookie", strings.Join(keep, "; "))
	}
}

func dropCookieDomain(c string) string {
	parts := strings.Split(c, ";")
	out := parts[:1]
	for _, p := range parts[1:] {
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(p)), "domain=") {
			continue
		}
		out = append(out, p)
	}
	return strings.Join(out, ";")
}

// closeHostForwards drops forwarding connections to a deleted host. Its
// forwards are removed by the database (cascade); their grants then fail the
// per-request lookup.
func (s *Server) closeHostForwards(userID, hostID int64) {
	s.fwd.mu.Lock()
	defer s.fwd.mu.Unlock()
	if c := s.fwd.conns[[2]int64{userID, hostID}]; c != nil {
		c.client.Close()
		delete(s.fwd.conns, [2]int64{userID, hostID})
	}
}

func (s *Server) revokeForwardGrants(id string) {
	s.fwd.mu.Lock()
	defer s.fwd.mu.Unlock()
	for k, g := range s.fwd.grants {
		if g.forwardID == id {
			delete(s.fwd.grants, k)
		}
	}
}

// Management API ----------------------------------------------------------

type forwardView struct {
	*store.Forward
	URL string `json:"url"`
}

func (s *Server) forwardView(f *store.Forward) forwardView {
	return forwardView{Forward: f, URL: s.cfg.Origin() + "/fwd/" + f.ID}
}

func (s *Server) forwardingOff(w http.ResponseWriter) bool {
	if s.cfg.ForwardDomain == "" {
		writeError(w, http.StatusNotFound, "forwarding_disabled", "port forwarding is not set up on this server")
		return true
	}
	return false
}

func (s *Server) apiListForwards(w http.ResponseWriter, r *http.Request) {
	list, err := s.store.ListForwards(r.Context(), currentUser(r).ID)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	out := make([]forwardView, 0, len(list))
	for _, f := range list {
		out = append(out, s.forwardView(f))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) apiCreateForward(w http.ResponseWriter, r *http.Request) {
	if s.forwardingOff(w) {
		return
	}
	var b struct {
		HostID int64 `json:"host_id"`
		Port   int   `json:"port"`
	}
	if err := readJSON(r, &b); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	if b.Port < 1 || b.Port > 65535 {
		writeErrorExtra(w, http.StatusUnprocessableEntity, "invalid_field", "must be 1–65535", map[string]string{"field": "port"})
		return
	}
	u := currentUser(r)
	h, err := s.store.Host(r.Context(), u.ID, b.HostID)
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "no such host")
		return
	}
	f, err := s.store.EnsureForward(r.Context(), u.ID, h.ID, b.Port)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	s.audit(r, "forward.create", hostTarget(h), map[string]any{"port": b.Port, "forward": f.ID})
	writeJSON(w, http.StatusOK, s.forwardView(f))
}

func (s *Server) apiUpdateForward(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Sharing string `json:"sharing"`
	}
	if err := readJSON(r, &b); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	if b.Sharing != store.SharePrivate && b.Sharing != store.ShareUsers {
		writeError(w, http.StatusBadRequest, "bad_request", "sharing must be private or users")
		return
	}
	id := r.PathValue("id")
	if err := s.store.SetForwardSharing(r.Context(), currentUser(r).ID, id, b.Sharing); err != nil {
		writeError(w, http.StatusNotFound, "not_found", "no such forward")
		return
	}
	if b.Sharing == store.SharePrivate {
		s.revokeForwardGrants(id) // others' access ends now; the owner signs in again transparently
	}
	s.audit(r, "forward.share", id, map[string]any{"sharing": b.Sharing})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) apiDeleteForward(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.store.DeleteForward(r.Context(), currentUser(r).ID, id); err != nil {
		writeError(w, http.StatusNotFound, "not_found", "no such forward")
		return
	}
	s.revokeForwardGrants(id)
	s.audit(r, "forward.delete", id, nil)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// GET /api/hosts/{id}/ports lists ports listening on the host.
func (s *Server) apiHostPorts(w http.ResponseWriter, r *http.Request) {
	h, ok := s.ownedHost(w, r)
	if !ok {
		return
	}
	client, err := s.forwardClient(r.Context(), &store.Forward{OwnerID: h.OwnerID, HostID: h.ID})
	if err != nil {
		writeError(w, http.StatusConflict, "no_connection", "open a terminal to this host first")
		return
	}
	ports, err := listeningPorts(r.Context(), client)
	if err != nil {
		writeError(w, http.StatusBadGateway, "ports_failed", "could not list ports on the host")
		return
	}
	writeJSON(w, http.StatusOK, ports)
}
