package web

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/coder/websocket"
	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"

	"github.com/ihainan/corral/internal/herdr"
	"github.com/ihainan/corral/internal/netguard"
	"github.com/ihainan/corral/internal/sftplite"
	"github.com/ihainan/corral/internal/sshx"
	"github.com/ihainan/corral/internal/store"
	"github.com/ihainan/corral/internal/term"
	"github.com/ihainan/corral/internal/vault"
)

func vaultSealed(c *store.Credential) vault.Sealed {
	return vault.Sealed{Ciphertext: c.Ciphertext, WrappedKey: c.WrappedKey, KeyID: c.KeyID}
}

func parseInt(s string) (int64, error) { return strconv.ParseInt(s, 10, 64) }

// failureReason turns a connection error into a fixed category. Remote error
// text is never returned or stored: a hostile server controls it and could
// echo back the password it was sent.
func failureReason(err error) string {
	var ne net.Error
	switch {
	case errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout()):
		return "timeout"
	case strings.Contains(err.Error(), "connection refused"):
		return "refused"
	case strings.Contains(err.Error(), "no such host") || strings.Contains(err.Error(), "cannot resolve"):
		return "dns"
	case strings.Contains(err.Error(), "network is unreachable") || strings.Contains(err.Error(), "no route to host"):
		return "unreachable"
	case strings.Contains(err.Error(), "cannot use private key"):
		return "bad_key"
	case strings.Contains(err.Error(), "ssh: handshake failed"):
		return "handshake"
	default:
		return "other"
	}
}

func (s *Server) apiListSessions(w http.ResponseWriter, r *http.Request) {
	writeJSONCompressed(w, r, http.StatusOK, s.terms.List(currentUser(r).ID))
}

func (s *Server) apiOpenSession(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	var b openBody
	if err := readJSON(r, &b); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	// Waiting for the turn and everything after it end with the login at
	// the latest, and revoking access (sign-out, disable, password change,
	// host deletion) cancels them.
	expiry, err := s.auth.LoginExpiry(r.Context(), loginHash(r))
	if err != nil {
		writeError(w, http.StatusUnauthorized, "unauthorized", "please sign in")
		return
	}
	ctx, cancel := context.WithDeadline(r.Context(), expiry)
	defer cancel()
	untrack := s.trackPending(u.ID, &liveSocket{loginHash: loginHash(r), hostID: b.HostID, cancel: cancel, opens: true})
	defer untrack()
	// Opens, edits and forwarding dials of one host take turns, so two taps
	// cannot both find nothing running and start two, and an open reads the
	// host, its credential and its pinned key as they are together. The
	// response is written after the turn is over.
	resp, err := s.underHostGate(ctx, u.ID, b.HostID, func(w http.ResponseWriter) { s.openSession(ctx, w, r, &b) })
	// Cancelled by the gateway stopping: not signed out (a 401 signs the
	// page out); to be tried again once it is back.
	if s.terms.Closing() && r.Context().Err() == nil && (err != nil && !errors.Is(err, errGateBusy) || err == nil && resp.status == http.StatusUnauthorized) {
		writeError(w, http.StatusServiceUnavailable, "restarting", "the gateway is restarting; try again in a moment")
		return
	}
	switch {
	case errors.Is(err, errGateBusy):
		writeError(w, http.StatusTooManyRequests, "busy", "this host is already being opened; try again in a moment")
	case err != nil && r.Context().Err() == nil:
		writeError(w, http.StatusUnauthorized, "unauthorized", "please sign in") // signed out or expired while waiting
	case err == nil:
		resp.writeTo(w)
	}
}

type openBody struct {
	HostID   int64  `json:"host_id"`
	Cols     int    `json:"cols"`
	Rows     int    `json:"rows"`
	Password string `json:"password"` // for hosts set to ask every time
	// After a host key change: the fingerprint the user saw and accepted.
	// The connection only goes ahead if the host presents exactly this key.
	AcceptFingerprint string `json:"accept_fingerprint"`
	// What to open; absent, the host is asked what it has and the answer
	// lists it to choose from (with Pending, the connection kept for that).
	Open    *openChoice `json:"open"`
	Pending string      `json:"pending"`
}

// openSession runs with the host's turn held; ctx ends with the login.
func (s *Server) openSession(ctx context.Context, w http.ResponseWriter, r *http.Request, b *openBody) {
	u := currentUser(r)
	switch ok, err := s.auth.LoginValid(ctx, loginHash(r)); {
	case ctx.Err() != nil:
		if r.Context().Err() == nil {
			writeError(w, http.StatusUnauthorized, "unauthorized", "please sign in")
		}
		return
	case err != nil:
		s.internalError(w, r, err)
		return
	case !ok:
		writeError(w, http.StatusUnauthorized, "unauthorized", "please sign in")
		return
	}
	h, err := s.store.Host(ctx, u.ID, b.HostID)
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "no such host")
		return
	}
	cfg := hostConfig(h)
	var choice *openChoice
	if b.Open != nil {
		if !b.Open.valid() {
			writeError(w, http.StatusBadRequest, "bad_request", "unknown choice of what to open")
			return
		}
		choice = b.Open
	}

	// Every gateway terminal attached to one tmux or herdr session would
	// only mirror the first (and fight it over the terminal size). Opening
	// one that a terminal of this host shows returns that terminal, as long
	// as it was made with the host's current settings. (For "ask" hosts this
	// re-attaches without a password, like opening the terminal from the
	// list: no new connection is made and nothing is sent to the host.)
	if choice != nil && (choice.Kind == openTmux || choice.Kind == openHerdr) {
		for _, rs := range s.terms.Running(u.ID, h.ID) {
			if rs.HostConfig == cfg && sessionKind(rs.Persistence) == choice.Kind && sameMux(choice.Kind, rs.Info().MuxSession, choice.Name) {
				// Marked as reused: a client that gave up on this open must
				// not close it, as other devices may be using it.
				writeJSON(w, http.StatusOK, struct {
					term.Info
					Reused bool `json:"reused"`
				}{rs.Info(), true})
				return
			}
		}
	}

	if !s.opening.acquire(u.ID) {
		writeError(w, http.StatusTooManyRequests, "busy", "too many hosts are being opened at once; try again in a moment")
		return
	}
	defer s.opening.release(u.ID)
	// The connection made when the host was asked what it has, if still
	// kept for this pick; otherwise a new one.
	client := s.picks.take(b.Pending, u.ID, h.ID, cfg, loginHash(r))
	if client == nil {
		var ok bool
		if client, cfg, ok = s.dialForOpen(ctx, w, r, h, b, cfg); !ok {
			return
		}
	}
	if choice == nil {
		// What the host has. Nothing to choose from: a plain shell.
		hc := probeHost(ctx, client)
		if ctx.Err() != nil {
			client.Close()
			writeError(w, http.StatusUnauthorized, "unauthorized", "please sign in")
			return
		}
		if !hc.any() {
			choice = &openChoice{Kind: openShell}
		} else {
			running := []runningChoice{}
			for _, rs := range s.terms.Running(u.ID, h.ID) {
				in := rs.Info()
				running = append(running, runningChoice{ID: in.ID, Kind: sessionKind(in.Persistence), Name: in.MuxSession, Title: in.Title, CreatedAt: in.CreatedAt})
			}
			token := s.picks.put(u.ID, h.ID, cfg, loginHash(r), client)
			// Signed out meanwhile (the drop on sign-out may have run before
			// the put): nothing is kept for it.
			if _, err := s.auth.CheckLogin(ctx, loginHash(r)); err != nil {
				if c := s.picks.take(token, u.ID, h.ID, cfg, loginHash(r)); c != nil {
					c.Close()
				}
				writeError(w, http.StatusUnauthorized, "unauthorized", "please sign in")
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{"choose": hc, "running": running, "pending": token})
			return
		}
	}
	opts := term.StartOptions{Cols: b.Cols, Rows: b.Rows, Persistence: store.PersistNone, HostConfig: cfg}
	switch choice.Kind {
	case openTmux:
		opts.Persistence, opts.TmuxSession = store.PersistTmux, choice.Name
	case openTmuxNew:
		// Named after all the sessions there are: a name taken would attach
		// to that session instead of starting a new one.
		lctx, lcancel := context.WithTimeout(ctx, probeTimeout)
		_, taken, err := tmuxSessions(lctx, client)
		lcancel()
		if err != nil {
			client.Close()
			writeError(w, http.StatusBadGateway, "tmux_list_failed", "could not list the tmux sessions on the host; try again")
			return
		}
		// (Started as new: one of that name made meanwhile is not attached.)
		opts.Persistence, opts.TmuxSession, opts.TmuxNew = store.PersistTmux, newTmuxName(u.Username, taken), true
	case openHerdr:
		opts.Persistence, opts.HerdrSession = store.PersistHerdr, choice.Name
	case openHerdrNew:
		// Named after all its sessions, as for tmux. Without herdr on the
		// host, the command falls back to a plain shell (as tmux's does).
		lctx, lcancel := context.WithTimeout(ctx, probeTimeout)
		taken, err := herdr.Discover(lctx, client)
		lcancel()
		if err != nil && !errors.Is(err, herdr.ErrNotInstalled) {
			client.Close()
			writeError(w, http.StatusBadGateway, "herdr_list_failed", "could not list the herdr sessions on the host; try again")
			return
		}
		// Also the names of this host's terminals here: one just started may
		// not be in herdr's list yet.
		for _, rs := range s.terms.Running(u.ID, h.ID) {
			if rs.Persistence == store.PersistHerdr {
				taken = append(taken, herdr.SessionInfo{Name: rs.Info().MuxSession})
			}
		}
		opts.Persistence, opts.HerdrSession = store.PersistHerdr, newHerdrName(u.Username, taken)
	}
	// A terminal slot is taken only to start one (choosing needs none).
	res, err := s.terms.Reserve(u.ID)
	if err != nil {
		client.Close()
		writeError(w, http.StatusTooManyRequests, "too_many_sessions", "close some sessions first")
		return
	}
	defer res.Release() // no-op once Start took it
	if opts.Persistence != store.PersistNone {
		// Kept, to be opened again after a restart (see tabs.go).
		opts.Register = s.registerOpened(u.ID, h.ID, cfg, loginHash(r))
	}
	// Last check before anything runs on the host.
	if _, err := s.auth.CheckLogin(ctx, loginHash(r)); err != nil || ctx.Err() != nil {
		client.Close()
		writeError(w, http.StatusUnauthorized, "unauthorized", "please sign in")
		return
	}
	sess, err := s.terms.Start(ctx, res, u.ID, h.ID, h.Name, client, opts)
	if err != nil {
		if errors.Is(err, term.ErrClosing) {
			writeError(w, http.StatusServiceUnavailable, "restarting", "the gateway is restarting; try again in a moment")
			return
		}
		if ctx.Err() != nil || errors.Is(err, store.ErrNoLogin) {
			writeError(w, http.StatusUnauthorized, "unauthorized", "please sign in")
			return
		}
		writeError(w, http.StatusBadGateway, "start_failed", "connected, but could not start a shell")
		return
	}
	// Access or the host may have gone while we were starting. The session is
	// registered first, then checked, so a concurrent revocation or deletion
	// either sees it (and closes it) or this check sees the change. The
	// gateway stopping (which cancels ctx) is not a revocation: the session
	// is left to the stop, to end as stopped (and be kept).
	if _, err := s.auth.CheckLogin(ctx, loginHash(r)); err != nil {
		if s.terms.Closing() {
			writeError(w, http.StatusServiceUnavailable, "restarting", "the gateway is restarting; try again in a moment")
			return
		}
		sess.Close("access revoked")
		writeError(w, http.StatusUnauthorized, "unauthorized", "please sign in")
		return
	}
	if _, err := s.store.Host(ctx, u.ID, h.ID); err != nil {
		if s.terms.Closing() {
			writeError(w, http.StatusServiceUnavailable, "restarting", "the gateway is restarting; try again in a moment")
			return
		}
		sess.Close("host deleted")
		writeError(w, http.StatusNotFound, "not_found", "no such host")
		return
	}
	if sess.Persistence == store.PersistHerdr {
		go s.watchHerdr(sess, h)
	}
	_ = s.store.TouchHostConnected(r.Context(), u.ID, h.ID)
	s.audit(r, "session.open", hostTarget(h), map[string]any{"session": sess.ID, "open": choice.Kind})
	writeJSON(w, http.StatusCreated, sess.Info())
}

// dialForOpen connects to the host to open a terminal: its credential (or
// the password typed), its jump host, the trust decision on its key (a
// changed key only if the user accepted exactly the one presented). On a
// failure the response is written and ok is false. cfg is returned updated
// when an accepted key changed the host's connection version.
func (s *Server) dialForOpen(ctx context.Context, w http.ResponseWriter, r *http.Request, h *store.Host, b *openBody, cfg string) (client *ssh.Client, _ string, ok bool) {
	u := currentUser(r)
	var err error
	t := sshx.Target{Address: h.Address, Port: h.Port, User: h.Username}
	switch h.Auth {
	case store.AuthAsk:
		if b.Password == "" {
			writeError(w, http.StatusConflict, "password_required", "enter the password for this host")
			return nil, cfg, false
		}
		t.Password = b.Password
	case store.AuthPassword, store.AuthKey:
		if h.CredentialID == nil {
			writeError(w, http.StatusConflict, "credential_missing", "this host has no saved credential; edit the host")
			return nil, cfg, false
		}
		_, sec, err := s.openCredential(ctx, u.ID, *h.CredentialID)
		if err != nil {
			s.internalError(w, r, err)
			return nil, cfg, false
		}
		t.Password, t.PrivateKey, t.Passphrase = sec.Password, []byte(sec.PrivateKey), []byte(sec.Passphrase)
	}
	var jumpVersion int64
	if t.Jump, jumpVersion, err = s.jumpTarget(ctx, h); err != nil {
		if ctx.Err() != nil {
			writeError(w, http.StatusUnauthorized, "unauthorized", "please sign in")
			return nil, cfg, false
		}
		writeJumpError(w, err)
		return nil, cfg, false
	}
	// Registered (by the caller) first, then checked: a revocation in between
	// is caught either way.
	if _, err := s.auth.CheckLogin(ctx, loginHash(r)); err != nil {
		writeError(w, http.StatusUnauthorized, "unauthorized", "please sign in")
		return nil, cfg, false
	}

	// The trust decision runs inside the handshake, before credentials are sent,
	// against the key on record at that moment.
	accepted := false
	t.Verify = func(seen *sshx.Pinned) error {
		ok, wasReplaced, err := s.trustHostKey(ctx, h, seen, b.AcceptFingerprint)
		if err != nil {
			return err
		}
		accepted = wasReplaced
		if !ok {
			return errHostKeyRejected
		}
		return nil
	}
	dialed, err := sshx.Dial(ctx, s.guard, t)
	if err != nil {
		var hk *sshx.HostKeyChangedError
		switch {
		case jumpProblem(err) != "":
			// (Before the host key case: the jump host's key is not this host's.)
			if ctx.Err() != nil {
				writeError(w, http.StatusUnauthorized, "unauthorized", "please sign in")
				break
			}
			s.audit(r, "session.connect_failed", hostTarget(h), map[string]any{"reason": "jump host: " + jumpReason(err)})
			writeJumpError(w, err)
		case errors.As(err, &hk):
			s.audit(r, "hostkey.mismatch", hostTarget(h), map[string]any{"expected": hk.Expected, "got": hk.Got})
			writeErrorExtra(w, http.StatusConflict, "host_key_changed", "the host key has changed",
				map[string]any{"expected": hk.Expected, "got": hk.Got})
		case errors.Is(err, netguard.ErrForbidden):
			s.audit(r, "session.forbidden", hostTarget(h), nil)
			writeError(w, http.StatusForbidden, "target_forbidden", "this address is not allowed by the administrator")
		case errors.Is(err, sshx.ErrAuthFailed):
			s.audit(r, "session.auth_failed", hostTarget(h), nil)
			writeError(w, http.StatusUnprocessableEntity, "auth_failed", "the host rejected the user name or credential")
		case ctx.Err() != nil:
			writeError(w, http.StatusUnauthorized, "unauthorized", "please sign in")
		default:
			reason := failureReason(err)
			s.audit(r, "session.connect_failed", hostTarget(h), map[string]any{"reason": reason})
			writeErrorExtra(w, http.StatusBadGateway, "connect_failed", "cannot connect to the host", map[string]any{"reason": reason})
		}
		return nil, cfg, false
	}
	if accepted {
		s.audit(r, "hostkey.accept", hostTarget(h), map[string]any{"fingerprint": dialed.SeenKey.Fingerprint})
		// Another machine key: nothing connected under the old one is reused.
		_ = s.store.BumpConnVersion(ctx, u.ID, h.ID)
		s.closeHostForwards(u.ID, h.ID)
		s.forgetHostCode(u.ID, h.ID)
		s.forgetThrough(ctx, u.ID, h.ID)
		if nh, err := s.store.Host(ctx, u.ID, h.ID); err == nil {
			cfg = hostConfig(nh)
		}
		// The new version labels this connection, so it must be one made
		// with what that version is: not if the jump host changed meanwhile.
		if h.JumpHostID != nil {
			if j, err := s.store.Host(ctx, u.ID, *h.JumpHostID); err != nil || j.ConnVersion != jumpVersion {
				dialed.Client.Close()
				writeError(w, http.StatusConflict, "host_changed", "the jump host was changed meanwhile; open the host again")
				return nil, cfg, false
			}
		}
	}
	return dialed.Client, cfg, true
}

var errHostKeyRejected = errors.New("host key rejected")

// trustHostKey decides, during the handshake, whether the presented key is
// trusted. With no key on record it is pinned (trust on first use). A different
// key is accepted only when it is exactly the fingerprint the user confirmed,
// and then replaces the record atomically. Concurrent requests resolve through
// the conditional writes: whatever ends up stored must equal what we saw.
func (s *Server) trustHostKey(ctx context.Context, h *store.Host, seen *sshx.Pinned, accept string) (ok, replaced bool, err error) {
	want := toHostKey(seen)
	for attempt := 0; attempt < 3; attempt++ {
		cur, err := s.store.HostKey(ctx, h.OwnerID, h.ID)
		switch {
		case errors.Is(err, store.ErrNotFound):
			if err := s.store.PinHostKey(ctx, h.OwnerID, h.ID, want); err != nil && !errors.Is(err, store.ErrNotFound) {
				return false, false, err
			}
			continue // re-read: we or a concurrent request pinned something
		case err != nil:
			return false, false, err
		}
		if bytes.Equal(cur.KeyData, want.KeyData) {
			return true, replaced, nil
		}
		if accept == "" || accept != want.Fingerprint {
			return false, false, &sshx.HostKeyChangedError{Expected: cur.Fingerprint, Got: want.Fingerprint}
		}
		if err := s.store.ReplaceHostKey(ctx, h.OwnerID, h.ID, cur.Fingerprint, want); err != nil && !errors.Is(err, store.ErrNotFound) {
			return false, false, err
		}
		replaced = true
	}
	return false, false, errors.New("host key record kept changing")
}

func toHostKey(p *sshx.Pinned) *store.HostKey {
	return &store.HostKey{KeyType: p.KeyType, KeyData: p.KeyData, Fingerprint: p.Fingerprint}
}

func (s *Server) apiCloseSession(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	id := r.PathValue("id")
	if err := s.terms.Remove(u.ID, id); err != nil {
		writeError(w, http.StatusNotFound, "not_found", "no such session")
		return
	}
	s.audit(r, "session.close", id, nil)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// Screen previews -----------------------------------------------------------
//
// The session list shows each running session's screen in small. The gateway
// keeps no screen of its own, so the owner's windows send what they show
// (text only), now and then and when they are left; the latest is kept in
// memory with the session.

const (
	previewMaxLines = 100
	previewMaxCols  = 500
	previewMaxBytes = 64 << 10
)

// PUT /api/sessions/{id}/preview {"cols":N,"lines":["…",…],"win":"…","seq":N}
// win names the sending window (page) and seq numbers its previews, so an
// older one arriving late is dropped; both are optional.
func (s *Server) apiPutPreview(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Cols  int      `json:"cols"`
		Lines []string `json:"lines"`
		Win   string   `json:"win"`
		Seq   int64    `json:"seq"`
	}
	if err := readJSON(r, &b); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	lines, ok := cleanPreview(b.Lines)
	if !ok || b.Cols < 1 || b.Cols > previewMaxCols || len(b.Win) > 64 {
		writeError(w, http.StatusBadRequest, "bad_request", "a preview is up to 100 lines of text")
		return
	}
	sess, err := s.terms.Get(currentUser(r).ID, r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "no such session")
		return
	}
	if sess.State() != term.StateRunning {
		writeError(w, http.StatusConflict, "ended", "the session has ended")
		return
	}
	sess.SetPreview(term.Preview{Cols: b.Cols, Lines: lines}, b.Win, b.Seq) // (an older one is dropped quietly)
	w.WriteHeader(http.StatusNoContent)
}

// GET /api/sessions/{id}/preview
func (s *Server) apiGetPreview(w http.ResponseWriter, r *http.Request) {
	sess, err := s.terms.Get(currentUser(r).ID, r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "no such session")
		return
	}
	p := sess.Preview()
	if p == nil {
		writeError(w, http.StatusNotFound, "no_preview", "no preview yet")
		return
	}
	writeJSON(w, http.StatusOK, p)
}

// cleanPreview checks the size of a preview and turns control characters
// (and invalid UTF-8) into spaces, so it is plain text.
func cleanPreview(in []string) ([]string, bool) {
	if len(in) > previewMaxLines {
		return nil, false
	}
	out := make([]string, len(in))
	total := 0
	for i, l := range in {
		total += len(l)
		if total > previewMaxBytes || utf8.RuneCountInString(l) > 2*previewMaxCols {
			return nil, false
		}
		out[i] = strings.Map(func(c rune) rune {
			if c < 0x20 || c == 0x7f || (c >= 0x80 && c < 0xa0) || c == utf8.RuneError {
				return ' '
			}
			return c
		}, l)
	}
	return out, true
}

// Terminal socket ----------------------------------------------------------
//
// Server → browser
//   binary: 8-byte big-endian offset, then raw terminal output
//   text:   {"t":"hello","offset":N,"reset":bool,"modes":"…","version":"…","instance":"…"}  sent first
//           (modes, on some resets: terminal output to write after clearing
//           and before the backlog; it restores modes set by output the
//           backlog no longer holds)
//           {"t":"live"}  after the backlog: output from here on is new
//           {"t":"state","state":"exited|lost|closed","code":N,"reason":"…","offset":N}
//           (sent after every output byte up to offset)
//           {"t":"pong","ts":N}
//           {"t":"error","code":"too_many_viewers"}  then close 4429: the
//           session already has as many viewers as it allows
// Browser → server
//   binary: keyboard input
// (the socket URL may carry ?viewer=ID, the same for all connections of one
// browser window, so a reconnecting window keeps being the one in charge)
//   text:   {"t":"resize","cols":N,"rows":N,"seq":N,"take":bool} | {"t":"ping","ts":N}
//           (seq numbers this socket's resize requests, from 1; take: the user
//           acted here, so the size is taken over; without it the size only
//           applies while this socket set it, or nobody did)
//   (server text also includes {"t":"size","cols":N,"rows":N,"own":bool,"ack":N}:
//   the terminal's current size, whether this socket set it, and the last of
//   its resize requests applied; sent on attach, whenever the size or the
//   socket that set it changes, and in answer to each resize request)

var viewerIDRE = regexp.MustCompile(`^[A-Za-z0-9_-]{8,64}$`)

const (
	maxFrame          = 64 << 10
	maxSocketsPerUser = 32
	// statusTooManyViewers closes a socket the session has no room for
	// (application range 4000-4999; 429 as in Too Many Requests).
	statusTooManyViewers websocket.StatusCode = 4429
)

// loginRecheck is how often long-lived connections (terminal sockets,
// forwarded WebSockets and streams) re-check the login they run under.
var loginRecheck = time.Minute

func (s *Server) apiSessionSocket(w http.ResponseWriter, r *http.Request) {
	sess, err := s.terms.Get(currentUser(r).ID, r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "no such session")
		return
	}
	s.serveTerminal(w, r, sess, 0)
}

// apiWatchSocket is the read-only socket of a user the owner let watch.
func (s *Server) apiWatchSocket(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	sess, err := s.terms.GetShared(u.ID, r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "no such session")
		return
	}
	s.serveTerminal(w, r, sess, u.ID)
}

// statusRevoked closes a watcher's socket once the owner took the leave back.
const statusRevoked websocket.StatusCode = 4403

// serveTerminal runs a terminal socket: for the owner, or (watcher != 0) for
// a user watching, whose input, sizes and visibility are all ignored.
func (s *Server) serveTerminal(w http.ResponseWriter, r *http.Request, sess *term.Session, watcher int64) {
	u := currentUser(r)
	hash := loginHash(r)
	var offset uint64
	have := false
	if v := r.URL.Query().Get("offset"); v != "" {
		if n, err := strconv.ParseUint(v, 10, 64); err == nil {
			offset, have = n, true
		}
	}
	expiry, err := s.auth.LoginExpiry(r.Context(), hash)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "unauthorized", "please sign in")
		return
	}
	// Everything on this socket, including the backlog, ends when the login does.
	ctx, cancel := context.WithDeadline(context.WithoutCancel(r.Context()), expiry)
	defer cancel()
	me := &liveSocket{loginHash: hash, cancel: cancel}
	untrack, ok := s.trackSocket(u.ID, me)
	if !ok {
		writeError(w, http.StatusTooManyRequests, "too_many_windows", "too many open terminal windows")
		return
	}
	defer untrack()
	// Until the page says otherwise, an open socket counts as someone watching
	// (for notifications: the owner's windows only).
	if watcher == 0 {
		s.vis.set(sess.ID, me, true)
		defer s.vis.drop(sess.ID, me)
	}
	// Registered before this check, so a sign-out racing with us is caught either way.
	if _, err := s.auth.CheckLogin(ctx, hash); err != nil {
		writeError(w, http.StatusUnauthorized, "unauthorized", "please sign in")
		return
	}

	// Origin was already checked against the configured site in guardRequest.
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true, CompressionMode: websocket.CompressionDisabled})
	if err != nil {
		return
	}
	c.SetReadLimit(1 << 20)

	// The browser window, so its reconnections count as the same viewer
	// (the one in charge of the terminal size stays in charge).
	viewer := r.URL.Query().Get("viewer")
	if !viewerIDRE.MatchString(viewer) {
		viewer = ""
	}
	var sub *term.Sub
	var backlog []byte
	var start uint64
	var reset bool
	var ev *term.Event
	if watcher != 0 {
		sub, backlog, start, reset, ev, err = sess.AttachWatcher(watcher, offset, have)
		if errors.Is(err, term.ErrNotFound) {
			c.Close(statusRevoked, "revoked")
			return
		}
	} else {
		sub, backlog, start, reset, ev, err = sess.AttachAs(viewer, offset, have)
	}
	if err != nil {
		// Said in a frame and a close code of its own, so the browser stops
		// reconnecting instead of retrying a socket that is refused anyway.
		wctx, wcancel := context.WithTimeout(ctx, 10*time.Second)
		_ = c.Write(wctx, websocket.MessageText, []byte(`{"t":"error","code":"too_many_viewers"}`))
		wcancel()
		c.Close(statusTooManyViewers, "too_many_viewers")
		return
	}
	defer sess.Detach(sub)
	if watcher != 0 {
		// The leave taken back ends the socket at once: what is still being
		// sent (a replay, queued output) is cut off, not delivered first.
		go func() {
			select {
			case <-sub.Revoked():
				// Said first: cancelling would drop the connection before
				// the close frame goes out.
				c.Close(statusRevoked, "revoked")
				cancel()
			case <-ctx.Done():
			}
		}()
		if sub.FirstWatch() {
			owner := ""
			if o, err := s.store.UserByID(r.Context(), sess.OwnerID); err == nil {
				owner = o.Username
			}
			s.audit(r, "session.watch", sess.HostName, map[string]any{"session": sess.ID, "owner": owner})
		}
	}
	revoked := func() bool {
		select {
		case <-sub.Revoked():
			return true
		default:
			return false
		}
	}

	writeText := func(v any) error {
		b, _ := json.Marshal(v)
		wctx, wcancel := context.WithTimeout(ctx, 10*time.Second)
		defer wcancel()
		return c.Write(wctx, websocket.MessageText, b)
	}
	writeData := func(off uint64, data []byte) error {
		for len(data) > 0 {
			n := min(len(data), maxFrame)
			frame := make([]byte, 8+n)
			binary.BigEndian.PutUint64(frame, off)
			copy(frame[8:], data[:n])
			wctx, wcancel := context.WithTimeout(ctx, 10*time.Second)
			err := c.Write(wctx, websocket.MessageBinary, frame)
			wcancel()
			if err != nil {
				return err
			}
			off += uint64(n)
			data = data[n:]
		}
		return nil
	}
	sendState := func(e term.Event) {
		_ = writeText(map[string]any{"t": "state", "state": e.State, "code": e.ExitCode, "reason": e.Reason, "offset": e.Offset})
		c.Close(websocket.StatusNormalClosure, "session ended")
	}
	// drain forwards output already queued for this viewer, keeping order.
	drain := func() error {
		for {
			select {
			case ch := <-sub.C:
				if err := writeData(ch.Offset, ch.Data); err != nil {
					return err
				}
			default:
				return nil
			}
		}
	}

	hello := map[string]any{"t": "hello", "offset": start, "reset": reset, "state": sess.State(), "version": Version, "instance": instance}
	if len(sub.Prelude) > 0 {
		hello["modes"] = string(sub.Prelude)
	}
	if err := writeText(hello); err != nil {
		return
	}
	if err := writeData(start, backlog); err != nil {
		return
	}
	// Replayed output may contain old terminal queries; the browser ignores its
	// automatic answers until this marker.
	if err := writeText(map[string]any{"t": "live"}); err != nil {
		return
	}
	if ev != nil {
		sendState(*ev)
		return
	}
	// Only the owner's windows repaint the host (it briefly changes the size).
	if reset && start > 0 && watcher == 0 {
		go sess.Redraw()
	}

	pongs := make(chan int64, 4)
	go func() {
		defer cancel()
		for {
			typ, data, err := c.Read(ctx)
			if err != nil {
				return
			}
			if typ == websocket.MessageBinary {
				if watcher != 0 {
					continue // read-only
				}
				if err := sess.Write(ctx, data); err != nil {
					if errors.Is(err, term.ErrInputBusy) {
						c.Close(websocket.StatusTryAgainLater, "the host is not reading input")
					}
					return
				}
				continue
			}
			var m struct {
				T       string `json:"t"`
				Cols    int    `json:"cols"`
				Rows    int    `json:"rows"`
				Seq     int64  `json:"seq"`
				Take    bool   `json:"take"`
				TS      int64  `json:"ts"`
				Visible bool   `json:"v"`
			}
			if json.Unmarshal(data, &m) != nil {
				continue
			}
			if watcher != 0 && m.T != "ping" {
				continue // a watcher only follows
			}
			switch m.T {
			case "resize":
				_ = sess.ResizeFrom(sub, m.Seq, m.Take, m.Cols, m.Rows)
			case "ping":
				select {
				case pongs <- m.TS:
				default:
				}
			case "visible":
				s.vis.set(sess.ID, me, m.Visible)
			}
		}
	}()

	recheck := time.NewTicker(loginRecheck)
	defer recheck.Stop()
	for {
		select {
		case <-ctx.Done():
			if revoked() {
				c.Close(statusRevoked, "revoked")
				return
			}
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				c.Close(websocket.StatusPolicyViolation, "sign-in expired")
			} else {
				c.Close(websocket.StatusGoingAway, "closing")
			}
			return
		case <-recheck.C:
			if _, err := s.auth.CheckLogin(ctx, hash); err != nil {
				c.Close(websocket.StatusPolicyViolation, "signed out")
				return
			}
		case ch := <-sub.C:
			if err := writeData(ch.Offset, ch.Data); err != nil {
				return
			}
		case e := <-sub.Events:
			if drain() == nil {
				sendState(e)
			}
			return
		case <-sub.Done():
			if revoked() {
				c.Close(statusRevoked, "revoked")
				return
			}
			if drain() != nil {
				return
			}
			select {
			case e := <-sub.Events:
				sendState(e)
			default:
				if watcher != 0 && !sess.Granted(watcher) {
					c.Close(statusRevoked, "revoked")
				} else {
					c.Close(websocket.StatusTryAgainLater, "reconnect")
				}
			}
			return
		case ts := <-pongs:
			if err := writeText(map[string]any{"t": "pong", "ts": ts}); err != nil {
				return
			}
		case m := <-sub.Meta:
			if watcher != 0 {
				continue // side information (herdr) is the owner's
			}
			wctx, wcancel := context.WithTimeout(ctx, 10*time.Second)
			err := c.Write(wctx, websocket.MessageText, m)
			wcancel()
			if err != nil {
				return
			}
		case sz := <-sub.Size:
			if err := writeText(map[string]any{"t": "size", "cols": sz.Cols, "rows": sz.Rows, "own": sz.Own, "ack": sz.Ack}); err != nil {
				return
			}
		}
	}
}

// Upload -------------------------------------------------------------------
//
// Files (typically images for coding agents) are written over SFTP to a private
// directory in the remote user's home. The remote name is generated here; the
// browser-supplied file name is never used in a path or a command.

var uploadExt = map[string]string{
	"image/png": ".png", "image/jpeg": ".jpg", "image/gif": ".gif", "image/webp": ".webp",
	"application/pdf": ".pdf", "text/plain; charset=utf-8": ".txt",
}

var errTooLarge = errors.New("file too large")

// maxSFTPPerConn bounds the SFTP channels of one SSH connection that have
// not really ended (see sshx/guard.go): the file panel's shared channel,
// transfers, and channels given up on a host that has not let go of them.
var maxSFTPPerConn = 4

var sftpChans = struct {
	sync.Mutex
	m map[*ssh.Client]int
}{m: map[*ssh.Client]int{}}

// takeSFTPChan reserves one SFTP channel on c; release is safe to call twice.
func takeSFTPChan(c *ssh.Client) (release func(), ok bool) {
	sftpChans.Lock()
	defer sftpChans.Unlock()
	if sftpChans.m[c] >= maxSFTPPerConn {
		return nil, false
	}
	sftpChans.m[c]++
	var once sync.Once
	return func() {
		once.Do(func() {
			sftpChans.Lock()
			defer sftpChans.Unlock()
			if sftpChans.m[c]--; sftpChans.m[c] <= 0 {
				delete(sftpChans.m, c)
			}
		})
	}, true
}

// openSFTP starts the sftp subsystem on its own channel with hard bounds that
// do not depend on the host cooperating:
//   - opening the channel and the subsystem handshake share one time limit;
//     on timeout we return at once and a background goroutine cleans up;
//   - the sftp client talks through local pipes, so closing them unblocks it
//     immediately even if the host never answers;
//   - stderr is read continuously so it cannot fill the channel window;
//   - the channel counts against maxSFTPPerConn until it has really ended
//     (its request stream closes), not merely until we gave up on it, so a
//     host that never lets go cannot make us pile up channels.
func openSFTP(c *ssh.Client) (*sftp.Client, func(), error) {
	return openSFTPWith(c, func(r io.Reader, w io.WriteCloser, closeAll func()) (*sftp.Client, func(), error) {
		client, err := sftp.NewClientPipe(r, w)
		if err != nil {
			return nil, nil, err
		}
		return client, func() { closeAll(); go client.Close() }, nil
	})
}

// openSFTPLite is openSFTP for the file panel's quick operations.
func openSFTPLite(c *ssh.Client) (*sftplite.Client, func(), error) {
	return openSFTPWith(c, func(r io.Reader, w io.WriteCloser, closeAll func()) (*sftplite.Client, func(), error) {
		client, err := sftplite.New(r, w, closeAll)
		if err != nil {
			return nil, nil, err
		}
		return client, func() { client.Close() }, nil
	})
}

// openSFTPWith does the work of openSFTP; mk starts the client on the local
// pipes (r: from the host, w: to it), closeAll ends them and the channel.
func openSFTPWith[T any](c *ssh.Client, mk func(r io.Reader, w io.WriteCloser, closeAll func()) (T, func(), error)) (T, func(), error) {
	var zero T
	releaseChan, ok := takeSFTPChan(c)
	if !ok {
		return zero, nil, sshx.ErrBusy
	}
	type result struct {
		client T
		close  func()
		err    error
	}
	done := make(chan result, 1)
	// If we give up waiting, whatever the setup goroutine has acquired so far is
	// released right away (and anything acquired later as soon as it is).
	var mu sync.Mutex
	abandoned := false
	var release func()
	hold := func(f func()) bool {
		mu.Lock()
		defer mu.Unlock()
		if abandoned {
			f()
			return false
		}
		release = f
		return true
	}
	go func() {
		ch, reqs, err := c.OpenChannel("session", nil)
		if err != nil {
			releaseChan()
			done <- result{err: err}
			return
		}
		go func() {
			for r := range reqs {
				if r.WantReply {
					_ = r.Reply(false, nil)
				}
			}
			releaseChan()
		}()
		if !hold(func() { go ch.Close() }) {
			return
		}
		go io.Copy(io.Discard, ch.Stderr())
		// Local pipes between the sftp client and the channel.
		outR, outW := io.Pipe()
		inR, inW := io.Pipe()
		var once sync.Once
		// Local pipes close at once; the channel close needs the transport and
		// may wait on a stalled host, so it runs in the background.
		closeAll := func() {
			once.Do(func() {
				outR.CloseWithError(io.ErrClosedPipe)
				inW.CloseWithError(io.ErrClosedPipe)
				go ch.Close()
			})
		}
		if !hold(closeAll) {
			return
		}
		ok, err := ch.SendRequest("subsystem", true, ssh.Marshal(&struct{ Name string }{"sftp"}))
		if err == nil && !ok {
			err = errors.New("the host refused the sftp subsystem")
		}
		if err != nil {
			closeAll()
			done <- result{err: err}
			return
		}
		go func() { _, err := io.Copy(outW, ch); outW.CloseWithError(err) }()
		go func() { _, err := io.Copy(ch, inR); inR.CloseWithError(err) }()
		client, closer, err := mk(outR, inW, closeAll)
		if err != nil {
			closeAll()
			done <- result{err: err}
			return
		}
		if !hold(closer) {
			return
		}
		done <- result{client: client, close: closer}
	}()
	t := time.NewTimer(15 * time.Second)
	defer t.Stop()
	select {
	case r := <-done:
		if r.err != nil {
			return zero, nil, r.err
		}
		return r.client, r.close, nil
	case <-t.C:
		mu.Lock()
		abandoned = true
		rel := release
		mu.Unlock()
		if rel != nil {
			rel()
		}
		return zero, nil, errors.New("file transfer did not start in time")
	}
}

func (s *Server) apiUpload(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	sess, err := s.terms.Get(u.ID, r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "no such session")
		return
	}
	if sess.State() != term.StateRunning {
		writeError(w, http.StatusConflict, "session_ended", "this session has ended")
		return
	}
	// Bound to the login like a terminal socket (sign-out, password change,
	// disable or the login's expiry stop it), as the file panel's transfers
	// are; so is a client or host that makes no progress for transferStall.
	t, ok := s.beginTransfer(w, r, sess)
	if !ok {
		return
	}
	defer t.end()
	ctx := t.ctx
	mr, err := r.MultipartReader()
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "expected a multipart upload")
		return
	}
	var part io.Reader
	for {
		p, err := mr.NextPart()
		if err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "no file in upload")
			return
		}
		if p.FormName() == "file" {
			part = &progressReader{r: p, t: t}
			break
		}
	}
	head := make([]byte, 512)
	n, _ := io.ReadFull(part, head)
	head = head[:n]
	if n == 0 {
		writeError(w, http.StatusBadRequest, "bad_request", "the file is empty")
		return
	}
	ctype := http.DetectContentType(head)
	ext, ok := uploadExt[ctype]
	if !ok {
		ext = ".bin"
	}

	client, err := s.openTransferSFTP(t, sess)
	if errors.Is(err, sshx.ErrBusy) {
		writeError(w, http.StatusTooManyRequests, "busy", "earlier file transfers have not ended yet")
		return
	}
	if err != nil {
		writeError(w, http.StatusBadGateway, "sftp_unavailable", "the host does not allow file transfer")
		return
	}

	home, err := client.Getwd()
	if err != nil {
		writeError(w, http.StatusBadGateway, "sftp_failed", "cannot find the home directory")
		return
	}
	dir := path.Join(home, ".cache", "corral", "uploads")
	if err := client.MkdirAll(dir); err != nil {
		writeError(w, http.StatusBadGateway, "sftp_failed", "cannot create the upload directory")
		return
	}
	_ = client.Chmod(path.Join(home, ".cache", "corral"), 0o700)
	_ = client.Chmod(dir, 0o700)

	rnd := make([]byte, 4)
	_, _ = rand.Read(rnd)
	name := time.Now().Format("20060102-150405") + "-" + hex.EncodeToString(rnd) + ext
	full := path.Join(dir, name)
	f, err := client.OpenFile(full, os.O_WRONLY|os.O_CREATE|os.O_EXCL)
	if err != nil {
		// (Made, maybe, the answer lost on the way: by its random name, ours.)
		if !errors.Is(err, fs.ErrExist) {
			s.removeFileLater(client, sess, full)
		}
		writeError(w, http.StatusBadGateway, "sftp_failed", "cannot create the file")
		return
	}
	_ = f.Chmod(0o600)
	limit := s.cfg.UploadMaxBytes
	src := io.LimitReader(io.MultiReader(bytes.NewReader(head), part), limit+1)
	written, err := io.Copy(f, src)
	if err == nil && written > limit {
		err = errTooLarge
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		s.removeFileLater(client, sess, full)
		var tooBig *http.MaxBytesError
		if errors.Is(err, errTooLarge) || errors.As(err, &tooBig) {
			writeError(w, http.StatusRequestEntityTooLarge, "too_large", fmt.Sprintf("files are limited to %d MB", limit>>20))
			return
		}
		writeError(w, http.StatusBadGateway, "sftp_failed", "upload failed")
		return
	}
	s.audit(r, "file.upload", sess.HostName, map[string]any{"session": sess.ID, "path": full, "bytes": written, "type": ctype})
	// "prefix": what to type before the path (Gemini CLI takes @path).
	writeJSON(w, http.StatusCreated, map[string]any{"path": full, "bytes": written, "type": ctype, "prefix": s.pathPrefix(ctx, sess)})
}

// removeFileLater deletes a file an upload left half written: with the
// upload's own channel while it works, otherwise (the upload was stopped,
// which closes it) with the session's shared one.
func (s *Server) removeFileLater(c *sftp.Client, sess *term.Session, full string) {
	if err := c.Remove(full); err == nil || errors.Is(err, fs.ErrNotExist) {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), fileOpTimeout)
		defer cancel()
		_ = s.withSFTP(ctx, sess, fileOpTimeout, func(c *sftplite.Client) error {
			if err := c.Remove(full); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return err
			}
			return nil
		})
	}()
}

// hostConfig fingerprints the settings a connection is made with.
func hostConfig(h *store.Host) string {
	cred := int64(0)
	if h.CredentialID != nil {
		cred = *h.CredentialID
	}
	// (What a terminal runs in is chosen when it is opened, not a setting.)
	return fmt.Sprintf("%d|%s|%d|%s|%s|%d", h.ConnVersion, h.Address, h.Port, h.Username, h.Auth, cred)
}

// underHostGate runs f with the host's turn held, collecting its response;
// the caller writes it once the turn is over, so a slow client never holds
// the turn.
func (s *Server) underHostGate(ctx context.Context, owner, host int64, f func(w http.ResponseWriter)) (*bufferedResponse, error) {
	release, err := s.hostGate.acquire(ctx, [2]int64{owner, host})
	if err != nil {
		return nil, err
	}
	defer release()
	resp := &bufferedResponse{header: http.Header{}}
	f(resp)
	return resp, nil
}

// bufferedResponse holds a small response until it can be written.
type bufferedResponse struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (b *bufferedResponse) Header() http.Header { return b.header }

func (b *bufferedResponse) WriteHeader(code int) {
	if b.status == 0 {
		b.status = code
	}
}

func (b *bufferedResponse) Write(p []byte) (int, error) {
	b.WriteHeader(http.StatusOK)
	return b.body.Write(p)
}

func (b *bufferedResponse) writeTo(w http.ResponseWriter) {
	for k, v := range b.header {
		w.Header()[k] = v
	}
	b.WriteHeader(http.StatusOK)
	w.WriteHeader(b.status)
	w.Write(b.body.Bytes())
}

var errGateBusy = errors.New("too many waiting")

// maxGateWaiters bounds how many opens of one host may queue up.
var maxGateWaiters = 4

// keyedGate lets one holder per key through at a time. Waiting can be
// cancelled, and entries disappear when nobody holds or waits for them.
type keyedGate struct {
	mu    sync.Mutex
	gates map[[2]int64]*gate
}

type gate struct {
	turn chan struct{}
	refs int // holder and waiters
}

func (g *keyedGate) acquire(ctx context.Context, key [2]int64) (func(), error) {
	g.mu.Lock()
	if g.gates == nil {
		g.gates = map[[2]int64]*gate{}
	}
	gt := g.gates[key]
	if gt == nil {
		gt = &gate{turn: make(chan struct{}, 1)}
		g.gates[key] = gt
	}
	if gt.refs > maxGateWaiters {
		g.mu.Unlock()
		return nil, errGateBusy
	}
	gt.refs++
	g.mu.Unlock()
	select {
	case gt.turn <- struct{}{}:
	case <-ctx.Done():
		g.drop(key, gt)
		return nil, ctx.Err()
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			<-gt.turn
			g.drop(key, gt)
		})
	}, nil
}

func (g *keyedGate) drop(key [2]int64, gt *gate) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if gt.refs--; gt.refs == 0 {
		delete(g.gates, key)
	}
}

func (g *keyedGate) size() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.gates)
}
