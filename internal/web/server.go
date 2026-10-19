// Package web is Corral's HTTP layer: JSON API, terminal WebSocket and the
// embedded single-page app.
package web

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"sync"

	"github.com/ihainan/corral/internal/auth"
	"github.com/ihainan/corral/internal/config"
	"github.com/ihainan/corral/internal/netguard"
	"github.com/ihainan/corral/internal/notify"
	"github.com/ihainan/corral/internal/store"
	"github.com/ihainan/corral/internal/term"
	"github.com/ihainan/corral/internal/vault"
)

// Access levels. Every route declares one; Routes() exposes the table so tests
// can assert nothing was registered without a decision.
type Access int

const (
	Public Access = iota
	SignedIn
	AdminOnly
)

type Route struct {
	Method, Pattern string
	Access          Access
}

type Server struct {
	hostGate keyedGate // per host: opens, edits and forwarding dials take turns
	cfg      *config.Config
	store    *store.Store
	auth     *auth.Service
	vault    *vault.Vault
	guard    *netguard.Guard
	terms    *term.Manager
	log      *slog.Logger
	mux      *http.ServeMux
	routes   []Route
	static   fs.FS

	herdrSocks herdrSockets
	files      *sftpPool        // shared SFTP channels of the file panel
	rec        *recState        // recordings being made, and their pruning
	notifier   *notify.Notifier // nil disables push
	fwd        *forwarder
	code       codeServers
	vis        visibility

	sockMu  sync.Mutex
	socks   map[int64]map[*liveSocket]struct{} // open WebSockets per user, closed on sign-out
	pending map[int64]map[*liveSocket]struct{} // connections being set up, cancelled on sign-out
}

type liveSocket struct {
	loginHash string
	hostID    int64 // for pending connects: deleting the host cancels them
	cancel    context.CancelFunc
}

type Deps struct {
	Config   *config.Config
	Store    *store.Store
	Auth     *auth.Service
	Vault    *vault.Vault
	Guard    *netguard.Guard
	Terms    *term.Manager
	Log      *slog.Logger
	Static   fs.FS // built frontend; nil serves a placeholder
	Notifier *notify.Notifier
}

func New(d Deps) *Server {
	s := &Server{cfg: d.Config, store: d.Store, auth: d.Auth, vault: d.Vault, guard: d.Guard, terms: d.Terms,
		log: d.Log, mux: http.NewServeMux(), static: d.Static, socks: map[int64]map[*liveSocket]struct{}{},
		pending: map[int64]map[*liveSocket]struct{}{}, herdrSocks: herdrSockets{path: map[string]string{}},
		notifier: d.Notifier, vis: visibility{seen: map[string]map[*liveSocket]bool{}}, fwd: newForwarder(), files: newSFTPPool(), code: codeServers{jobs: map[[2]int64]*codeJob{}}}
	s.fwd.wg.Add(1)
	go s.fwd.reap()
	s.terms.OnAlert = s.onTerminalAlert
	s.auth.UseVault(d.Vault) // authenticator secrets are sealed like host credentials
	prev := s.auth.OnUserGone
	s.auth.OnUserGone = func(userID int64) {
		if prev != nil {
			prev(userID)
		}
		// Cancel what is still connecting and the open sockets first: those must
		// stop immediately; closing terminals can take a moment per host.
		s.closeSockets(userID, "")
		s.closeUserForwards(userID)
		s.forgetUserCode(userID)
		if s.notifier != nil {
			s.notifier.Revoke(userID)
		}
		s.terms.CloseUser(userID, "access revoked")
	}
	if s.cfg.DataDir != "" {
		if err := s.startRecordings(); err != nil {
			s.log.Error("cannot prepare session recordings; sessions are not recorded", "err", err)
		}
	}
	s.registerRoutes()
	return s
}

func (s *Server) Routes() []Route { return append([]Route(nil), s.routes...) }

// Handler serves the main site, and forwarded ports on their own subdomains.
func (s *Server) Handler() http.Handler {
	main := s.withHeaders(s.mux)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if port, id, ok := s.forwardLabel(r.Host); ok {
			s.serveForward(w, r, port, id)
			return
		}
		main.ServeHTTP(w, r)
	})
}

func (s *Server) registerRoutes() {
	// Public
	s.handle("GET /api/bootstrap", Public, s.apiBootstrap)
	s.handle("POST /api/setup", Public, s.apiSetup)
	s.handle("POST /api/login", Public, s.apiLogin)
	s.handle("POST /api/login/code", Public, s.apiLoginMFA)
	s.handle("GET /api/invites/{token}", Public, s.apiInviteInfo)
	s.handle("POST /api/invites/{token}/redeem", Public, s.apiInviteRedeem)

	// Signed in
	s.handle("POST /api/logout", SignedIn, s.apiLogout)
	s.handle("GET /api/me", SignedIn, s.apiMe)
	s.handle("PUT /api/me", SignedIn, s.apiUpdateMe)
	s.handle("POST /api/me/password", SignedIn, s.apiChangePassword)
	s.handle("GET /api/me/settings", SignedIn, s.apiGetSettings)
	s.handle("PATCH /api/me/settings", SignedIn, s.apiPatchSettings)
	s.handle("GET /api/me/totp", SignedIn, s.apiTOTPStatus)
	s.handle("POST /api/me/totp/begin", SignedIn, s.apiTOTPBegin)
	s.handle("POST /api/me/totp/enable", SignedIn, s.apiTOTPEnable)
	s.handle("POST /api/me/totp/disable", SignedIn, s.apiTOTPDisable)
	s.handle("POST /api/me/totp/recovery", SignedIn, s.apiTOTPRecovery)

	s.handle("GET /api/hosts", SignedIn, s.apiListHosts)
	s.handle("POST /api/hosts", SignedIn, s.apiCreateHost)
	s.handle("GET /api/hosts/{id}", SignedIn, s.apiGetHost)
	s.handle("PUT /api/hosts/{id}", SignedIn, s.apiUpdateHost)
	s.handle("DELETE /api/hosts/{id}", SignedIn, s.apiDeleteHost)
	s.handle("GET /api/hosts/{id}/hostkey", SignedIn, s.apiGetHostKey)

	s.handle("GET /api/credentials", SignedIn, s.apiListCredentials)
	s.handle("POST /api/credentials", SignedIn, s.apiCreateCredential)
	s.handle("PUT /api/credentials/{id}", SignedIn, s.apiRenameCredential)
	s.handle("DELETE /api/credentials/{id}", SignedIn, s.apiDeleteCredential)

	s.handle("GET /api/sessions", SignedIn, s.apiListSessions)
	s.handle("POST /api/sessions", SignedIn, s.apiOpenSession)
	s.handle("DELETE /api/sessions/{id}", SignedIn, s.apiCloseSession)
	s.handle("GET /api/sessions/{id}/ws", SignedIn, s.apiSessionSocket)
	s.handle("POST /api/sessions/{id}/upload", SignedIn, s.apiUpload)
	s.handle("POST /api/sessions/{id}/herdr", SignedIn, s.apiHerdrAction)
	s.handle("GET /api/sessions/{id}/shares", SignedIn, s.apiListShares)
	s.handle("POST /api/sessions/{id}/shares", SignedIn, s.apiAddShare)
	s.handle("DELETE /api/sessions/{id}/shares/{uid}", SignedIn, s.apiRemoveShare)
	s.handle("GET /api/shared", SignedIn, s.apiListShared)
	s.handle("GET /api/shared/{id}", SignedIn, s.apiGetShared)
	s.handle("GET /api/shared/{id}/ws", SignedIn, s.apiWatchSocket)
	s.handle("GET /api/sessions/{id}/files", SignedIn, s.apiFilesList)
	s.handle("GET /api/sessions/{id}/files/cwd", SignedIn, s.apiFilesCwd)
	s.handle("GET /api/sessions/{id}/files/download", SignedIn, s.apiFilesDownload)
	s.handle("POST /api/sessions/{id}/files/upload", SignedIn, s.apiFilesUpload)
	s.handle("POST /api/sessions/{id}/files/mkdir", SignedIn, s.apiFilesMkdir)
	s.handle("POST /api/sessions/{id}/files/rename", SignedIn, s.apiFilesRename)
	s.handle("POST /api/sessions/{id}/files/delete", SignedIn, s.apiFilesDelete)

	s.handle("GET /api/recordings", SignedIn, s.apiListRecordings)
	s.handle("GET /api/recordings/{id}", SignedIn, s.apiGetRecording)
	s.handle("GET /api/recordings/{id}/cast", SignedIn, s.apiRecordingCast)
	s.handle("DELETE /api/recordings/{id}", AdminOnly, s.apiDeleteRecording)

	s.handle("GET /api/forwards", SignedIn, s.apiListForwards)
	s.handle("POST /api/forwards", SignedIn, s.apiCreateForward)
	s.handle("PUT /api/forwards/{id}", SignedIn, s.apiUpdateForward)
	s.handle("DELETE /api/forwards/{id}", SignedIn, s.apiDeleteForward)
	s.handle("GET /api/hosts/{id}/ports", SignedIn, s.apiHostPorts)
	s.handle("GET /fwd/{id}", Public, s.handleForwardOpen)

	s.handle("GET /api/hosts/{id}/code", SignedIn, s.apiCodeStatus)
	s.handle("POST /api/hosts/{id}/code", SignedIn, s.apiCodeStart)
	s.handle("POST /api/hosts/{id}/code/stop", SignedIn, s.apiCodeStop)

	s.handle("GET /api/push", SignedIn, s.apiPushInfo)
	s.handle("POST /api/push/subscriptions", SignedIn, s.apiPushSubscribe)
	s.handle("DELETE /api/push/subscriptions", SignedIn, s.apiPushUnsubscribe)
	s.handle("POST /api/push/test", SignedIn, s.apiPushTest)

	// Admin
	s.handle("GET /api/admin/users", AdminOnly, s.apiAdminListUsers)
	s.handle("PUT /api/admin/users/{id}", AdminOnly, s.apiAdminUpdateUser)
	s.handle("POST /api/admin/users/{id}/password", AdminOnly, s.apiAdminResetPassword)
	s.handle("POST /api/admin/users/{id}/totp/reset", AdminOnly, s.apiAdminResetTOTP)
	s.handle("POST /api/admin/invites", AdminOnly, s.apiAdminCreateInvite)
	s.handle("GET /api/admin/audit", AdminOnly, s.apiAdminAudit)

	s.mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok\n")) })
	s.mux.Handle("/api/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, "not_found", "no such endpoint")
	}))
	s.mux.Handle("/", s.staticHandler())
}

type ctxKey int

const (
	userKey ctxKey = iota
	loginKey
)

func currentUser(r *http.Request) *store.User {
	u, _ := r.Context().Value(userKey).(*store.User)
	return u
}
func loginHash(r *http.Request) string { h, _ := r.Context().Value(loginKey).(string); return h }

func (s *Server) handle(pattern string, access Access, h http.HandlerFunc) {
	method, path, _ := strings.Cut(pattern, " ")
	s.routes = append(s.routes, Route{Method: method, Pattern: path, Access: access})
	s.mux.Handle(pattern, s.guardRequest(access, h))
}

// guardRequest applies the cross-site check and the declared access level.
func (s *Server) guardRequest(access Access, h http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.sameOrigin(r) {
			writeError(w, http.StatusForbidden, "bad_origin", "request did not come from this site")
			return
		}
		if access != Public || r.Method != http.MethodGet {
			r.Body = http.MaxBytesReader(w, r.Body, s.bodyLimit(r))
		}
		u, hash, err := s.auth.Authenticate(r.Context(), s.sessionCookie(r))
		if err == nil {
			ctx := context.WithValue(r.Context(), userKey, u)
			ctx = context.WithValue(ctx, loginKey, hash)
			r = r.WithContext(ctx)
		}
		switch access {
		case SignedIn:
			if err != nil {
				writeError(w, http.StatusUnauthorized, "unauthorized", "please sign in")
				return
			}
		case AdminOnly:
			if err != nil {
				writeError(w, http.StatusUnauthorized, "unauthorized", "please sign in")
				return
			}
			if !u.IsAdmin() {
				writeError(w, http.StatusForbidden, "forbidden", "administrators only")
				return
			}
		}
		h(w, r)
	})
}

func (s *Server) bodyLimit(r *http.Request) int64 {
	if strings.HasSuffix(r.URL.Path, "/upload") {
		return s.cfg.UploadMaxBytes + 1<<20
	}
	return 1 << 20
}

// sameOrigin rejects state-changing requests (and WebSocket upgrades) that a
// browser sent on behalf of another site. Reads are allowed without an Origin.
func (s *Server) sameOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	isUpgrade := strings.EqualFold(r.Header.Get("Upgrade"), "websocket")
	if (r.Method == http.MethodGet || r.Method == http.MethodHead) && !isUpgrade {
		return origin == "" || origin == s.cfg.Origin()
	}
	return origin == s.cfg.Origin()
}

func (s *Server) cookieName() string {
	if s.cfg.SecureCookies() {
		// __Host- forbids a Domain attribute, so forwarded-port subdomains can
		// neither read nor overwrite the main site's login.
		return "__Host-corral_session"
	}
	return "corral_session"
}

func (s *Server) sessionCookie(r *http.Request) string {
	c, err := r.Cookie(s.cookieName())
	if err != nil {
		return ""
	}
	return c.Value
}

func (s *Server) setSessionCookie(w http.ResponseWriter, token string) {
	http.SetCookie(w, &http.Cookie{
		Name: s.cookieName(), Value: token, Path: "/", HttpOnly: true, Secure: s.cfg.SecureCookies(),
		SameSite: http.SameSiteLaxMode, MaxAge: int(s.cfg.LoginAbsoluteLimit.Seconds()),
	})
}

func (s *Server) clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{Name: s.cookieName(), Value: "", Path: "/", HttpOnly: true,
		Secure: s.cfg.SecureCookies(), SameSite: http.SameSiteLaxMode, MaxAge: -1})
}

// clientIP returns the caller's address. X-Forwarded-For is honoured only when
// the direct peer is a configured trusted proxy, and then we take the rightmost
// address that is not itself a trusted proxy (the one our proxy actually saw).
func (s *Server) clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	peer, err := netip.ParseAddr(host)
	if err != nil || !s.trusted(peer) {
		return host
	}
	parts := strings.Split(r.Header.Get("X-Forwarded-For"), ",")
	for i := len(parts) - 1; i >= 0; i-- {
		a, err := netip.ParseAddr(strings.TrimSpace(parts[i]))
		if err != nil {
			break
		}
		if !s.trusted(a) {
			return a.String()
		}
	}
	return host
}

func (s *Server) trusted(a netip.Addr) bool {
	a = a.Unmap()
	for _, p := range s.cfg.TrustedProxies {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

func (s *Server) withHeaders(next http.Handler) http.Handler {
	csp := "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data: blob:; " +
		"font-src 'self' data:; connect-src 'self' " + wsOrigin(s.cfg.Origin()) + "; worker-src 'self'; manifest-src 'self'; " +
		"frame-ancestors 'none'; base-uri 'none'; form-action 'self'; object-src 'none'"
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", csp)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "same-origin")
		h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		if s.cfg.SecureCookies() {
			h.Set("Strict-Transport-Security", "max-age=31536000")
		}
		next.ServeHTTP(w, r)
	})
}

func wsOrigin(origin string) string {
	if strings.HasPrefix(origin, "https://") {
		return "wss://" + strings.TrimPrefix(origin, "https://")
	}
	return "ws://" + strings.TrimPrefix(origin, "http://")
}

// sockets ------------------------------------------------------------------

func (s *Server) trackSocket(userID int64, ls *liveSocket) (func(), bool) {
	s.sockMu.Lock()
	if len(s.socks[userID]) >= maxSocketsPerUser {
		s.sockMu.Unlock()
		return nil, false
	}
	if s.socks[userID] == nil {
		s.socks[userID] = map[*liveSocket]struct{}{}
	}
	s.socks[userID][ls] = struct{}{}
	s.sockMu.Unlock()
	return func() {
		s.sockMu.Lock()
		delete(s.socks[userID], ls)
		if len(s.socks[userID]) == 0 {
			delete(s.socks, userID)
		}
		s.sockMu.Unlock()
	}, true
}

// trackPending registers an in-progress connection so revoking access cancels it.
func (s *Server) trackPending(userID int64, ls *liveSocket) func() {
	s.sockMu.Lock()
	if s.pending[userID] == nil {
		s.pending[userID] = map[*liveSocket]struct{}{}
	}
	s.pending[userID][ls] = struct{}{}
	s.sockMu.Unlock()
	return func() {
		s.sockMu.Lock()
		delete(s.pending[userID], ls)
		if len(s.pending[userID]) == 0 {
			delete(s.pending, userID)
		}
		s.sockMu.Unlock()
	}
}

// cancelPendingForHost stops connects to a host that is being deleted.
func (s *Server) cancelPendingForHost(userID, hostID int64) {
	s.sockMu.Lock()
	defer s.sockMu.Unlock()
	for ls := range s.pending[userID] {
		if ls.hostID == hostID {
			ls.cancel()
		}
	}
}

// closeOtherLogins closes sockets and pending connects of every login of the
// user except keep (after a password change).
func (s *Server) closeOtherLogins(userID int64, keep string) {
	s.sockMu.Lock()
	defer s.sockMu.Unlock()
	for _, set := range []map[*liveSocket]struct{}{s.socks[userID], s.pending[userID]} {
		for ls := range set {
			if ls.loginHash != keep {
				ls.cancel()
			}
		}
	}
	s.endForwardOtherLogins(userID, keep)
	if s.notifier != nil {
		s.notifier.RevokeOtherLogins(userID, keep)
	}
}

// closeSockets closes a user's open terminal sockets and cancels connections
// being set up; with loginHash set, only those of that browser login.
func (s *Server) closeSockets(userID int64, loginHash string) {
	s.sockMu.Lock()
	defer s.sockMu.Unlock()
	for _, set := range []map[*liveSocket]struct{}{s.socks[userID], s.pending[userID]} {
		for ls := range set {
			if loginHash == "" || ls.loginHash == loginHash {
				ls.cancel()
			}
		}
	}
	if loginHash != "" { // all of the user's: closeUserForwards and notifier.Revoke
		s.endForwardLogin(userID, loginHash)
		if s.notifier != nil {
			s.notifier.RevokeLogin(userID, loginHash)
		}
	}
}

// helpers ------------------------------------------------------------------

type apiError struct {
	Error   string `json:"error"`
	Message string `json:"message"`
	Extra   any    `json:"extra,omitempty"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, apiError{Error: code, Message: msg})
}

func writeErrorExtra(w http.ResponseWriter, status int, code, msg string, extra any) {
	writeJSON(w, status, apiError{Error: code, Message: msg, Extra: extra})
}

func readJSON(r *http.Request, v any) error {
	ct := r.Header.Get("Content-Type")
	if !strings.HasPrefix(ct, "application/json") {
		return errors.New("expected application/json")
	}
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("unexpected data after the JSON body")
	}
	return nil
}

func pathID(r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	return id, err == nil && id > 0
}

func (s *Server) audit(r *http.Request, action, target string, detail map[string]any) {
	var uid *int64
	name := ""
	if u := currentUser(r); u != nil {
		uid, name = &u.ID, u.Username
	}
	if err := s.store.Audit(r.Context(), uid, name, action, target, s.clientIP(r), detail); err != nil {
		s.log.Error("audit write failed", "action", action, "err", err)
	}
}

func (s *Server) auditAs(r *http.Request, u *store.User, action, target string, detail map[string]any) {
	var uid *int64
	name := ""
	if u != nil {
		uid, name = &u.ID, u.Username
	}
	if err := s.store.Audit(r.Context(), uid, name, action, target, s.clientIP(r), detail); err != nil {
		s.log.Error("audit write failed", "action", action, "err", err)
	}
}

func (s *Server) internalError(w http.ResponseWriter, r *http.Request, err error) {
	s.log.Error("request failed", "method", r.Method, "path", r.URL.Path, "err", err)
	writeError(w, http.StatusInternalServerError, "internal", "something went wrong on the server")
}
