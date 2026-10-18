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
	var b struct {
		HostID   int64  `json:"host_id"`
		Cols     int    `json:"cols"`
		Rows     int    `json:"rows"`
		Password string `json:"password"` // for hosts set to ask every time
		// After a host key change: the fingerprint the user saw and accepted.
		// The connection only goes ahead if the host presents exactly this key.
		AcceptFingerprint string `json:"accept_fingerprint"`
	}
	if err := readJSON(r, &b); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	h, err := s.store.Host(r.Context(), u.ID, b.HostID)
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "no such host")
		return
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
		_, sec, err := s.openCredential(r.Context(), u.ID, *h.CredentialID)
		if err != nil {
			s.internalError(w, r, err)
			return
		}
		t.Password, t.PrivateKey, t.Passphrase = sec.Password, []byte(sec.PrivateKey), []byte(sec.Passphrase)
	}
	// Only a host with no key on record is trusted on first use; a failed lookup
	// is an error, not "first use".
	k, err := s.store.HostKey(r.Context(), u.ID, h.ID)
	switch {
	case err == nil:
		t.Pinned = &sshx.Pinned{KeyType: k.KeyType, KeyData: k.KeyData, Fingerprint: k.Fingerprint}
	case !errors.Is(err, store.ErrNotFound):
		s.internalError(w, r, err)
		return
	}
	t.Accept = b.AcceptFingerprint

	res, err := s.terms.Reserve(u.ID)
	if err != nil {
		writeError(w, http.StatusTooManyRequests, "too_many_sessions", "close some sessions first")
		return
	}
	// Revoking access (disable, sign-out) cancels this while it is still connecting.
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	untrack := s.trackPending(u.ID, &liveSocket{loginHash: loginHash(r), cancel: cancel})
	defer untrack()
	dialed, err := sshx.Dial(ctx, s.guard, t)
	if err != nil {
		res.Release()
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
		default:
			reason := failureReason(err)
			s.audit(r, "session.connect_failed", hostTarget(h), map[string]any{"reason": reason})
			writeErrorExtra(w, http.StatusBadGateway, "connect_failed", "cannot connect to the host", map[string]any{"reason": reason})
		}
		return
	}
	if err := s.recordHostKey(r, h, t.Pinned, dialed); err != nil {
		res.Release()
		dialed.Client.Close()
		var hk *sshx.HostKeyChangedError
		if errors.As(err, &hk) {
			writeErrorExtra(w, http.StatusConflict, "host_key_changed", "the host key has changed",
				map[string]any{"expected": hk.Expected, "got": hk.Got})
			return
		}
		s.internalError(w, r, err)
		return
	}
	// Last check before anything runs on the host.
	if _, err := s.auth.CheckLogin(ctx, loginHash(r)); err != nil || ctx.Err() != nil {
		res.Release()
		dialed.Client.Close()
		writeError(w, http.StatusUnauthorized, "unauthorized", "please sign in")
		return
	}
	sess, err := s.terms.Start(ctx, res, u.ID, h.ID, h.Name, dialed.Client, term.StartOptions{
		Cols: b.Cols, Rows: b.Rows, Persistence: h.Persistence, TmuxSession: h.TmuxSession,
	})
	if err != nil {
		writeError(w, http.StatusBadGateway, "start_failed", "connected, but could not start a shell")
		return
	}
	// Access may have been revoked while we were connecting. The session is
	// registered first, then checked, so a concurrent revocation either sees it
	// (and closes it) or this check sees the revocation.
	if _, err := s.auth.CheckLogin(r.Context(), loginHash(r)); err != nil {
		sess.Close("access revoked")
		writeError(w, http.StatusUnauthorized, "unauthorized", "please sign in")
		return
	}
	_ = s.store.TouchHostConnected(r.Context(), u.ID, h.ID)
	s.audit(r, "session.open", hostTarget(h), map[string]any{"session": sess.ID})
	writeJSON(w, http.StatusCreated, sess.Info())
}

// recordHostKey pins the key seen on first use, or swaps in the key the user
// accepted. If another request recorded a key meanwhile, the stored key must be
// exactly the one we saw; otherwise the connection is refused.
func (s *Server) recordHostKey(r *http.Request, h *store.Host, pinned *sshx.Pinned, d *sshx.Result) error {
	ctx := r.Context()
	seen := toHostKey(d.SeenKey)
	switch {
	case pinned == nil:
		if err := s.store.PinHostKey(ctx, h.OwnerID, h.ID, seen); err != nil && !errors.Is(err, store.ErrNotFound) {
			return err
		}
	case d.Replaced:
		if err := s.store.ReplaceHostKey(ctx, h.OwnerID, h.ID, pinned.Fingerprint, seen); err != nil && !errors.Is(err, store.ErrNotFound) {
			return err
		}
	default:
		return nil // matched the pinned key
	}
	k, err := s.store.HostKey(ctx, h.OwnerID, h.ID)
	if err != nil {
		return err
	}
	if !bytes.Equal(k.KeyData, seen.KeyData) {
		return &sshx.HostKeyChangedError{Expected: k.Fingerprint, Got: seen.Fingerprint}
	}
	if d.Replaced {
		s.audit(r, "hostkey.accept", hostTarget(h), map[string]any{"fingerprint": seen.Fingerprint})
	}
	return nil
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
//           {"t":"state","state":"exited|lost|closed","code":N,"reason":"…","offset":N}
//           (sent after every output byte up to offset)
//           {"t":"pong","ts":N}
// Browser → server
//   binary: keyboard input
//   text:   {"t":"resize","cols":N,"rows":N} | {"t":"ping","ts":N}

const (
	maxFrame         = 64 << 10
	loginRecheck     = time.Minute
	maxSocketsPerUser = 32
)

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
	untrack, ok := s.trackSocket(u.ID, &liveSocket{loginHash: hash, cancel: cancel})
	if !ok {
		writeError(w, http.StatusTooManyRequests, "too_many_windows", "too many open terminal windows")
		return
	}
	defer untrack()
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
				T    string `json:"t"`
				Cols int    `json:"cols"`
				Rows int    `json:"rows"`
				TS   int64  `json:"ts"`
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

// openSFTP starts the sftp subsystem on its own channel so every exit path can
// close that channel (pkg/sftp's NewClient leaks it when the subsystem is
// refused). Setup has a time limit, stderr is drained so it cannot stall the
// channel, and closing shuts the channel first so blocked reads return.
func openSFTP(c *ssh.Client) (*sftp.Client, func(), error) {
	sess, err := c.NewSession()
	if err != nil {
		return nil, nil, err
	}
	sess.Stderr = io.Discard
	pw, err := sess.StdinPipe()
	if err != nil {
		sess.Close()
		return nil, nil, err
	}
	pr, err := sess.StdoutPipe()
	if err != nil {
		sess.Close()
		return nil, nil, err
	}
	type result struct {
		client *sftp.Client
		err    error
	}
	done := make(chan result, 1)
	go func() {
		if err := sess.RequestSubsystem("sftp"); err != nil {
			done <- result{err: err}
			return
		}
		client, err := sftp.NewClientPipe(pr, pw)
		done <- result{client, err}
	}()
	var res result
	select {
	case res = <-done:
	case <-time.After(15 * time.Second):
		sess.Close() // unblocks the setup goroutine
		res = <-done
		if res.client != nil {
			res.client.Close()
		}
		return nil, nil, errors.New("file transfer did not start in time")
	}
	if res.err != nil {
		sess.Close()
		return nil, nil, res.err
	}
	var once sync.Once
	return res.client, func() {
		once.Do(func() {
			sess.Close()
			go res.client.Close() // returns once its reader sees the closed channel
		})
	}, nil
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
	stop := context.AfterFunc(r.Context(), closeSFTP)
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
