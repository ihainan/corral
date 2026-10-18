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
	"net"
	"net/http"
	"os"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"

	"github.com/ihainan/corral/internal/netguard"
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
	writeJSON(w, http.StatusOK, s.terms.List(currentUser(r).ID))
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
	untrack := s.trackPending(u.ID, &liveSocket{loginHash: loginHash(r), hostID: b.HostID, cancel: cancel})
	defer untrack()
	// Opens, edits and forwarding dials of one host take turns, so two taps
	// cannot both find nothing running and start two, and an open reads the
	// host, its credential and its pinned key as they are together. The
	// response is written after the turn is over.
	resp, err := s.underHostGate(ctx, u.ID, b.HostID, func(w http.ResponseWriter) { s.openSession(ctx, w, r, &b) })
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

	// With tmux or herdr every gateway session attaches the same remote
	// session, so a second one would only mirror the first (and fight it over
	// the terminal size). Opening such a host again returns the running one,
	// as long as it was made with the host's current settings; after an edit
	// a new session is started. (For "ask" hosts this re-attaches without a
	// password, like opening the session from the list: no new connection is
	// made and nothing is sent to the host.)
	if h.Persistence != store.PersistNone {
		for _, rs := range s.terms.Running(u.ID, h.ID) {
			if rs.HostConfig == cfg {
				writeJSON(w, http.StatusOK, rs.Info())
				return
			}
		}
	}

	t := sshx.Target{Address: h.Address, Port: h.Port, User: h.Username}
	switch h.Auth {
	case store.AuthAsk:
		if b.Password == "" {
			writeError(w, http.StatusConflict, "password_required", "enter the password for this host")
			return
		}
		t.Password = b.Password
	case store.AuthPassword, store.AuthKey:
		if h.CredentialID == nil {
			writeError(w, http.StatusConflict, "credential_missing", "this host has no saved credential; edit the host")
			return
		}
		_, sec, err := s.openCredential(ctx, u.ID, *h.CredentialID)
		if err != nil {
			s.internalError(w, r, err)
			return
		}
		t.Password, t.PrivateKey, t.Passphrase = sec.Password, []byte(sec.PrivateKey), []byte(sec.Passphrase)
	}
	res, err := s.terms.Reserve(u.ID)
	if err != nil {
		writeError(w, http.StatusTooManyRequests, "too_many_sessions", "close some sessions first")
		return
	}
	defer res.Release() // no-op once Start took it
	// Registered (by the caller) first, then checked: a revocation in between
	// is caught either way.
	if _, err := s.auth.CheckLogin(ctx, loginHash(r)); err != nil {
		writeError(w, http.StatusUnauthorized, "unauthorized", "please sign in")
		return
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
		return
	}
	if accepted {
		s.audit(r, "hostkey.accept", hostTarget(h), map[string]any{"fingerprint": dialed.SeenKey.Fingerprint})
		// Another machine key: nothing connected under the old one is reused.
		_ = s.store.BumpConnVersion(ctx, u.ID, h.ID)
		s.closeHostForwards(u.ID, h.ID)
		s.forgetHostCode(u.ID, h.ID)
		if nh, err := s.store.Host(ctx, u.ID, h.ID); err == nil {
			cfg = hostConfig(nh)
		}
	}
	// Last check before anything runs on the host.
	if _, err := s.auth.CheckLogin(ctx, loginHash(r)); err != nil || ctx.Err() != nil {
		dialed.Client.Close()
		writeError(w, http.StatusUnauthorized, "unauthorized", "please sign in")
		return
	}
	sess, err := s.terms.Start(ctx, res, u.ID, h.ID, h.Name, dialed.Client, term.StartOptions{
		Cols: b.Cols, Rows: b.Rows, Persistence: h.Persistence, TmuxSession: h.TmuxSession, HerdrSession: h.HerdrSession,
		HostConfig: cfg,
	})
	if err != nil {
		if ctx.Err() != nil {
			writeError(w, http.StatusUnauthorized, "unauthorized", "please sign in")
			return
		}
		writeError(w, http.StatusBadGateway, "start_failed", "connected, but could not start a shell")
		return
	}
	// Access or the host may have gone while we were starting. The session is
	// registered first, then checked, so a concurrent revocation or deletion
	// either sees it (and closes it) or this check sees the change.
	if _, err := s.auth.CheckLogin(ctx, loginHash(r)); err != nil {
		sess.Close("access revoked")
		writeError(w, http.StatusUnauthorized, "unauthorized", "please sign in")
		return
	}
	if _, err := s.store.Host(ctx, u.ID, h.ID); err != nil {
		sess.Close("host deleted")
		writeError(w, http.StatusNotFound, "not_found", "no such host")
		return
	}
	if h.Persistence == store.PersistHerdr {
		go s.watchHerdr(sess, h)
	}
	_ = s.store.TouchHostConnected(r.Context(), u.ID, h.ID)
	s.audit(r, "session.open", hostTarget(h), map[string]any{"session": sess.ID})
	writeJSON(w, http.StatusCreated, sess.Info())
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

// Terminal socket ----------------------------------------------------------
//
// Server → browser
//   binary: 8-byte big-endian offset, then raw terminal output
//   text:   {"t":"hello","offset":N,"reset":bool}  sent first
//           {"t":"live"}  after the backlog: output from here on is new
//           {"t":"state","state":"exited|lost|closed","code":N,"reason":"…","offset":N}
//           (sent after every output byte up to offset)
//           {"t":"pong","ts":N}
// Browser → server
//   binary: keyboard input
//   text:   {"t":"resize","cols":N,"rows":N} | {"t":"ping","ts":N}
//   (server text also includes {"t":"size","cols":N,"rows":N}: the terminal's
//   current size, sent on attach and whenever any viewer changes it)

const (
	maxFrame          = 64 << 10
	maxSocketsPerUser = 32
)

// loginRecheck is how often long-lived connections (terminal sockets,
// forwarded WebSockets and streams) re-check the login they run under.
var loginRecheck = time.Minute

func (s *Server) apiSessionSocket(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	hash := loginHash(r)
	sess, err := s.terms.Get(u.ID, r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "no such session")
		return
	}
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
	// Until the page says otherwise, an open socket counts as someone watching.
	s.vis.set(sess.ID, me, true)
	defer s.vis.drop(sess.ID, me)
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

	sub, backlog, start, reset, ev, err := sess.Attach(offset, have)
	if err != nil {
		c.Close(websocket.StatusPolicyViolation, "too many windows for this session")
		return
	}
	defer sess.Detach(sub)

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

	if err := writeText(map[string]any{"t": "hello", "offset": start, "reset": reset, "state": sess.State()}); err != nil {
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
	if reset && start > 0 {
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
				TS      int64  `json:"ts"`
				Visible bool   `json:"v"`
			}
			if json.Unmarshal(data, &m) != nil {
				continue
			}
			switch m.T {
			case "resize":
				_ = sess.Resize(m.Cols, m.Rows)
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
			if drain() != nil {
				return
			}
			select {
			case e := <-sub.Events:
				sendState(e)
			default:
				c.Close(websocket.StatusTryAgainLater, "reconnect")
			}
			return
		case ts := <-pongs:
			if err := writeText(map[string]any{"t": "pong", "ts": ts}); err != nil {
				return
			}
		case m := <-sub.Meta:
			wctx, wcancel := context.WithTimeout(ctx, 10*time.Second)
			err := c.Write(wctx, websocket.MessageText, m)
			wcancel()
			if err != nil {
				return
			}
		case sz := <-sub.Size:
			if err := writeText(map[string]any{"t": "size", "cols": sz[0], "rows": sz[1]}); err != nil {
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

// openSFTP starts the sftp subsystem on its own channel with hard bounds that
// do not depend on the host cooperating:
//   - opening the channel and the subsystem handshake share one time limit;
//     on timeout we return at once and a background goroutine cleans up;
//   - the sftp client talks through local pipes, so closing them unblocks it
//     immediately even if the host never answers;
//   - stderr is read continuously so it cannot fill the channel window.
func openSFTP(c *ssh.Client) (*sftp.Client, func(), error) {
	type result struct {
		client *sftp.Client
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
		sess, err := c.NewSession()
		if err != nil {
			done <- result{err: err}
			return
		}
		if !hold(func() { go sess.Close() }) {
			return
		}
		fail := func(err error) { sess.Close(); done <- result{err: err} }
		chIn, err := sess.StdinPipe()
		if err != nil {
			fail(err)
			return
		}
		chOut, err := sess.StdoutPipe()
		if err != nil {
			fail(err)
			return
		}
		chErr, err := sess.StderrPipe()
		if err != nil {
			fail(err)
			return
		}
		go io.Copy(io.Discard, chErr)
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
				go sess.Close()
			})
		}
		if !hold(closeAll) {
			return
		}
		if err := sess.RequestSubsystem("sftp"); err != nil {
			closeAll()
			done <- result{err: err}
			return
		}
		go func() { _, err := io.Copy(outW, chOut); outW.CloseWithError(err) }()
		go func() { _, err := io.Copy(chIn, inR); inR.CloseWithError(err) }()
		client, err := sftp.NewClientPipe(outR, inW)
		if err != nil {
			closeAll()
			done <- result{err: err}
			return
		}
		closer := func() { closeAll(); go client.Close() }
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
			return nil, nil, r.err
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
		return nil, nil, errors.New("file transfer did not start in time")
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
	releaseSlot, ok := sess.UploadSlot()
	if !ok {
		writeError(w, http.StatusTooManyRequests, "busy", "another upload is in progress")
		return
	}
	defer releaseSlot()
	// Bound to the login like a terminal socket: sign-out, password change,
	// disable or the login's expiry stop the transfer.
	expiry, err := s.auth.LoginExpiry(r.Context(), loginHash(r))
	if err != nil {
		writeError(w, http.StatusUnauthorized, "unauthorized", "please sign in")
		return
	}
	ctx, cancel := context.WithDeadline(r.Context(), expiry)
	defer cancel()
	untrack := s.trackPending(u.ID, &liveSocket{loginHash: loginHash(r), hostID: sess.HostID, cancel: cancel})
	defer untrack()
	if _, err := s.auth.CheckLogin(ctx, loginHash(r)); err != nil {
		writeError(w, http.StatusUnauthorized, "unauthorized", "please sign in")
		return
	}
	// Reading the body ends with the login at the latest; cancelling also
	// interrupts a read that is blocked on a client that stopped sending.
	rc := http.NewResponseController(w)
	_ = rc.SetReadDeadline(expiry)
	stopBody := context.AfterFunc(ctx, func() {
		_ = rc.SetReadDeadline(time.Now())
		r.Body.Close()
	})
	defer stopBody()
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
			part = p
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

	client, closeSFTP, err := openSFTP(sess.Client())
	if err != nil {
		writeError(w, http.StatusBadGateway, "sftp_unavailable", "the host does not allow file transfer")
		return
	}
	defer closeSFTP()
	// A cancelled request (browser gone) stops the transfer; so does a host
	// that stops answering for too long.
	stop := context.AfterFunc(ctx, closeSFTP)
	defer stop()
	watchdog := time.AfterFunc(30*time.Minute, closeSFTP)
	defer watchdog.Stop()

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
		_ = client.Remove(full)
		var tooBig *http.MaxBytesError
		if errors.Is(err, errTooLarge) || errors.As(err, &tooBig) {
			writeError(w, http.StatusRequestEntityTooLarge, "too_large", fmt.Sprintf("files are limited to %d MB", limit>>20))
			return
		}
		writeError(w, http.StatusBadGateway, "sftp_failed", "upload failed")
		return
	}
	s.audit(r, "file.upload", sess.HostName, map[string]any{"session": sess.ID, "path": full, "bytes": written, "type": ctype})
	writeJSON(w, http.StatusCreated, map[string]any{"path": full, "bytes": written, "type": ctype})
}

// hostConfig fingerprints the settings a connection is made with.
func hostConfig(h *store.Host) string {
	cred := int64(0)
	if h.CredentialID != nil {
		cred = *h.CredentialID
	}
	return fmt.Sprintf("%d|%s|%d|%s|%s|%d|%s|%s|%s", h.ConnVersion, h.Address, h.Port, h.Username, h.Auth, cred, h.Persistence,
		h.TmuxSession, h.HerdrSession)
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
