package web

// Port forwarding: "<port>-<id>.<forward domain>" serves a port on a target
// host to the browser, through the SSH connection the gateway holds.
//
// Each forwarded site lives on its own subdomain so it cannot read or act as
// the main site (separate origin; the main login cookie is __Host- scoped).
// Access is handed over with a one-time ticket: the main site, where the user
// is signed in, issues it; the subdomain swaps it for its own cookie.

import (
	"bufio"
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
)

var (
	fwdLabelRE  = regexp.MustCompile(`^([0-9]{1,5})-([a-z2-9]{4})$`)
	codeLabelRE = regexp.MustCompile(`^code-([a-z2-9]{4})$`) // VS Code; port 0
)

// fwdCookieName uses the __Host- prefix on HTTPS so a forwarded app cannot
// plant a same-named cookie for all subdomains.
func (s *Server) fwdCookieName() string {
	if s.cfg.SecureCookies() {
		return "__Host-corral_fwd"
	}
	return fwdCookieInsecure
}

// forwardLabel extracts port and id when host is a forwarding subdomain;
// port is 0 for a code forward ("code-<id>").
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
	if m := codeLabelRE.FindStringSubmatch(label); m != nil {
		return 0, m[1], true
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

// forwardOriginFor is the exact Origin a browser sends for a forward's pages
// (port 0: a code forward).
func (s *Server) forwardOriginFor(port int, id string) string {
	label := strconv.Itoa(port) + "-" + id
	if port == 0 {
		label = "code-" + id
	}
	o := s.cfg.PublicURL.Scheme + "://" + label + "." + s.cfg.ForwardDomain
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
	if err != nil || !canUseForward(u, f) || !s.ownerActive(r.Context(), f.OwnerID) {
		http.Error(w, "This forwarded port does not exist or is not shared with you.", http.StatusNotFound)
		return
	}
	tok, tokHash, err := auth.NewToken()
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	s.fwd.addTicket(tokHash, &fwdGrant{fwdUse: fwdUse{userID: u.ID, loginHash: hash, forwardID: f.ID, ownerID: f.OwnerID, hostID: f.HostID},
		ret: ret, expires: time.Now().Add(ticketTTL)})
	q := url.Values{"ticket": {tok}}
	http.Redirect(w, r, s.forwardOrigin(f)+fwdAuthPath+"?"+q.Encode(), http.StatusFound)
}

// serveForward handles every request on a forwarding subdomain.
func (s *Server) serveForward(w http.ResponseWriter, r *http.Request, port int, id string) {
	if forwardCrossSite(r, s.forwardOriginFor(port, id)) {
		http.Error(w, "This request did not come from this forwarded port's own pages.", http.StatusForbidden)
		return
	}
	mainLogin := s.cfg.Origin() + "/fwd/" + id + "?return=" + url.QueryEscape(r.URL.RequestURI())

	if r.URL.Path == fwdAuthPath {
		g := s.fwd.takeTicket(auth.HashToken(r.URL.Query().Get("ticket")))
		if g == nil || time.Now().After(g.expires) || g.forwardID != id {
			http.Error(w, "This link has expired. Open the port again from Corral.", http.StatusForbidden)
			return
		}
		tok, tokHash, err := auth.NewToken()
		if err != nil {
			s.internalError(w, r, err)
			return
		}
		s.fwd.addGrant(tokHash, &fwdGrant{fwdUse: g.fwdUse, expires: time.Now().Add(fwdTokenTTL)})
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
	g, ok := s.fwd.grant(tokHash)
	if !ok || g.forwardID != id {
		s.forwardLoginRedirect(w, r, mainLogin)
		return
	}
	// Registered before access is re-checked, so a revocation from now on
	// cancels this request (and its WebSocket, if it becomes one).
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	st, ok := s.fwd.startStream(g, cancel)
	if !ok {
		http.Error(w, "Too many open connections through forwarded ports. Close some tabs and try again.", http.StatusServiceUnavailable)
		return
	}
	defer s.fwd.endStream(st)
	f, err := s.store.ForwardByID(ctx, id)
	if err != nil || f.Port != port || (f.Kind == store.KindCode) != (port == 0) {
		s.fwd.dropGrant(tokHash)
		http.Error(w, "This forwarded port no longer exists.", http.StatusNotFound)
		return
	}
	code := f.Kind == store.KindCode
	s.fwd.setForward(st, f.OwnerID, f.HostID)
	// Sign-in, sharing and the owner's account are checked on every request:
	// ending any of them takes effect at once.
	if !s.forwardAllowed(ctx, g, f) {
		s.fwd.dropGrant(tokHash)
		s.forwardLoginRedirect(w, r, mainLogin)
		return
	}
	// A WebSocket or a long response lives no longer than the login, and
	// re-checks access as often as a terminal socket does: logins also end
	// by expiring, which nothing announces.
	expiry, err := s.auth.LoginExpiry(ctx, g.loginHash)
	if err != nil {
		s.forwardLoginRedirect(w, r, mainLogin)
		return
	}
	ctx, cancelAtExpiry := context.WithDeadline(ctx, expiry)
	defer cancelAtExpiry()
	defer recheckEvery(ctx, loginRecheck, func() bool { return s.forwardAllowed(ctx, g, f) }, cancel)()
	var sock string
	if code {
		if sock = s.code.socket(f.OwnerID, f.HostID); sock == "" {
			http.Error(w, "VS Code is not running on this host. Open it again from Corral.", http.StatusBadGateway)
			return
		}
	}
	fc, err := s.forwardConn(ctx, f)
	if errors.Is(err, errFwdBusy) {
		http.Error(w, "Too many forwarded connections are open right now. Try again in a moment.", http.StatusServiceUnavailable)
		return
	}
	if err != nil {
		http.Error(w, "Cannot reach the host. Open a terminal to it in Corral, then try again.", http.StatusBadGateway)
		return
	}
	defer s.fwd.release(fc)
	if code {
		s.fwd.setCodeSocket(fc, sock)
	}
	// Ending the request (revocation, expiry) must also free a handler stuck
	// writing to a browser that stopped reading, or it would keep its quotas.
	cw := &cutoffWriter{ResponseWriter: w}
	defer afterFuncWait(ctx, cw.cut)() // the cut never outlives the handler (and its connection's next request)
	s.proxyTo(cw, r.WithContext(ctx), fc, f)
}

// afterFuncWait is context.AfterFunc whose stop also waits for a call of f
// that already started, so f never runs after the caller returned.
func afterFuncWait(ctx context.Context, f func()) (stop func()) {
	done := make(chan struct{})
	stopAfter := context.AfterFunc(ctx, func() {
		defer close(done)
		f()
	})
	var once sync.Once
	return func() { // safe to call more than once, also concurrently
		once.Do(func() {
			if !stopAfter() {
				<-done
			}
		})
	}
}

// cutoffWriter lets a cancelled request stop writing to the browser: a
// response write is given a deadline in the past, a hijacked (upgraded)
// connection is closed.
type cutoffWriter struct {
	http.ResponseWriter
	mu       sync.Mutex
	cutOff   bool
	hijacked net.Conn
}

func (c *cutoffWriter) Unwrap() http.ResponseWriter { return c.ResponseWriter }

func (c *cutoffWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	conn, brw, err := http.NewResponseController(c.ResponseWriter).Hijack()
	if err != nil {
		return nil, nil, err
	}
	c.mu.Lock()
	c.hijacked = conn
	late := c.cutOff
	c.mu.Unlock()
	if late {
		conn.Close()
	}
	return conn, brw, nil
}

func (c *cutoffWriter) cut() {
	c.mu.Lock()
	c.cutOff = true
	conn := c.hijacked
	c.mu.Unlock()
	if conn != nil {
		conn.Close()
		return
	}
	_ = http.NewResponseController(c.ResponseWriter).SetWriteDeadline(time.Now())
}

// recheckEvery runs check every d while ctx lasts and calls fail (once) when
// it reports false. It uses a timer rather than a goroutine, as most proxied
// requests are short. The returned func stops it.
func recheckEvery(ctx context.Context, d time.Duration, check func() bool, fail func()) (stop func()) {
	var mu sync.Mutex
	stopped := false
	var t *time.Timer
	mu.Lock() // t is set before the first run reads it
	defer mu.Unlock()
	t = time.AfterFunc(d, func() {
		if ctx.Err() != nil {
			return
		}
		if !check() {
			fail()
			return
		}
		mu.Lock()
		if !stopped {
			t.Reset(d)
		}
		mu.Unlock()
	})
	return func() {
		mu.Lock()
		stopped = true
		t.Stop()
		mu.Unlock()
	}
}

// forwardAllowed re-checks a grant: the login is live and is still the
// user's, the forward is theirs or shared, and its owner is not disabled.
func (s *Server) forwardAllowed(ctx context.Context, g fwdUse, f *store.Forward) bool {
	u, err := s.auth.CheckLogin(ctx, g.loginHash)
	if err != nil || u.ID != g.userID || !canUseForward(u, f) {
		return false
	}
	return f.OwnerID == u.ID || s.ownerActive(ctx, f.OwnerID)
}

// ownerActive: a disabled user's forwards, shared ones included, stop working.
func (s *Server) ownerActive(ctx context.Context, ownerID int64) bool {
	o, err := s.store.UserByID(ctx, ownerID)
	return err == nil && !o.Disabled
}

// Navigations are sent to sign in; sub-resource requests just fail.
func (s *Server) forwardLoginRedirect(w http.ResponseWriter, r *http.Request, target string) {
	if r.Method == http.MethodGet && strings.Contains(r.Header.Get("Accept"), "text/html") {
		http.Redirect(w, r, target, http.StatusFound)
		return
	}
	http.Error(w, "Sign in to Corral to use this forwarded port.", http.StatusUnauthorized)
}

// forwardConn returns an SSH connection to the forward's host, with a
// reference held until release: a running terminal session's connection when
// there is one, otherwise a connection opened with the stored credential and
// closed once unused for a while.
//
// VS Code (code forwards and setting up code-server) always gets a
// connection of its own: it is long-lived and heavy, and a terminal must not
// depend on it.
func (s *Server) forwardConn(ctx context.Context, f *store.Forward) (*fwdConn, error) {
	for _, sess := range s.terms.Running(f.OwnerID, f.HostID) {
		if f.Kind == store.KindCode {
			break
		}
		return s.fwd.borrow(sess.Client())
	}
	key := [2]int64{f.OwnerID, f.HostID}
	for range 3 {
		// The dial is shared by every request waiting for it, so it does not
		// end with the request that started it; revocation cancels it.
		dctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
		// Registered before the host and owner are read: revoking either from
		// now on cancels this dial and keeps its connection out of the pool.
		fc, d, lead, err := s.fwd.acquire(key, cancel)
		switch {
		case err != nil:
			cancel()
			return nil, err
		case fc != nil:
			cancel()
			return fc, nil
		case lead:
			client, err := s.dialForward(dctx, f.OwnerID, f.HostID)
			cancel()
			return s.fwd.finishDial(d, client, err)
		}
		cancel()
		select {
		case <-d.done:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		if d.err != nil {
			return nil, d.err
		}
	}
	return nil, errFwdBusy // the connection kept going away as soon as it was opened
}

func (s *Server) dialForward(ctx context.Context, ownerID, hostID int64) (*ssh.Client, error) {
	if !s.ownerActive(ctx, ownerID) {
		return nil, errFwdRevoked
	}
	h, err := s.store.Host(ctx, ownerID, hostID)
	if err != nil {
		return nil, err
	}
	if h.Auth == store.AuthAsk || h.CredentialID == nil {
		return nil, errors.New("no stored credential")
	}
	_, sec, err := s.openCredential(ctx, ownerID, *h.CredentialID)
	if err != nil {
		return nil, err
	}
	k, err := s.store.HostKey(ctx, ownerID, h.ID)
	if err != nil {
		return nil, errors.New("connect to this host once from a terminal first")
	}
	dialed, err := sshx.Dial(ctx, s.guard, sshx.Target{Address: h.Address, Port: h.Port, User: h.Username,
		Password: sec.Password, PrivateKey: []byte(sec.PrivateKey), Passphrase: []byte(sec.Passphrase),
		Pinned: &sshx.Pinned{KeyType: k.KeyType, KeyData: k.KeyData, Fingerprint: k.Fingerprint}})
	if err != nil {
		return nil, err
	}
	return dialed.Client, nil
}

// proxyTo relays the request to 127.0.0.1:<port> on the host. Requests look
// like local ones (Host and Origin are rewritten to localhost:<port>) so dev
// servers such as Vite and Next.js accept them without configuration.
func (s *Server) proxyTo(w http.ResponseWriter, r *http.Request, fc *fwdConn, f *store.Forward) {
	target := "127.0.0.1:" + strconv.Itoa(f.Port)
	local := "localhost:" + strconv.Itoa(f.Port)
	unavailable := "Nothing is answering on port " + strconv.Itoa(f.Port) + " of the host."
	if f.Kind == store.KindCode {
		// code-server on its socket; it accepts a WebSocket when Origin and
		// Host agree, so both say localhost.
		target, local = codeUpstream, "localhost"
		unavailable = "VS Code is not answering on the host. Open it again from Corral."
	}
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
		Transport:     fc.transport, // one per SSH connection, so idle keep-alives are reused and bounded
		FlushInterval: -1,           // stream server-sent events and long responses as they arrive
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
			http.Error(w, unavailable, http.StatusBadGateway)
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
	if f, err := s.store.ForwardByID(r.Context(), id); err == nil && f.OwnerID == currentUser(r).ID && f.Kind == store.KindCode {
		writeError(w, http.StatusConflict, "code_private", "VS Code is only ever available to its owner")
		return
	}
	if err := s.store.SetForwardSharing(r.Context(), currentUser(r).ID, id, b.Sharing); err != nil {
		writeError(w, http.StatusNotFound, "not_found", "no such forward")
		return
	}
	if b.Sharing == store.SharePrivate {
		s.revokeForwardGrants(id, currentUser(r).ID) // others' access ends now, open connections included
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
	s.revokeForwardGrants(id, 0)
	s.audit(r, "forward.delete", id, nil)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// GET /api/hosts/{id}/ports lists ports listening on the host.
func (s *Server) apiHostPorts(w http.ResponseWriter, r *http.Request) {
	h, ok := s.ownedHost(w, r)
	if !ok {
		return
	}
	// Counted like a proxied request, and ended by the same revocations.
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	st, ok := s.fwd.startStream(fwdUse{userID: h.OwnerID, loginHash: loginHash(r), ownerID: h.OwnerID, hostID: h.ID}, cancel)
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "busy", "too many forwarding connections are open; try again in a moment")
		return
	}
	defer s.fwd.endStream(st)
	fc, err := s.forwardConn(ctx, &store.Forward{OwnerID: h.OwnerID, HostID: h.ID})
	if errors.Is(err, errFwdBusy) {
		writeError(w, http.StatusServiceUnavailable, "busy", "too many forwarding connections are open; try again in a moment")
		return
	}
	if err != nil {
		writeError(w, http.StatusConflict, "no_connection", "open a terminal to this host first")
		return
	}
	defer s.fwd.release(fc)
	ports, err := listeningPorts(ctx, fc.client)
	if errors.Is(err, sshx.ErrBusy) {
		writeError(w, http.StatusServiceUnavailable, "busy", "earlier port listings on this host are still running; try again in a moment")
		return
	}
	if err != nil {
		writeError(w, http.StatusBadGateway, "ports_failed", "could not list ports on the host")
		return
	}
	writeJSON(w, http.StatusOK, ports)
}
