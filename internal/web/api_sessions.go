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
	"net/http"
	"path"
	"strconv"
	"os"
	"time"

	"github.com/coder/websocket"
	"github.com/pkg/sftp"

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
	if k, err := s.store.HostKey(r.Context(), u.ID, h.ID); err == nil {
		t.Pinned = &sshx.Pinned{KeyType: k.KeyType, KeyData: k.KeyData, Fingerprint: k.Fingerprint}
	}

	res, err := sshx.Dial(r.Context(), s.guard, t)
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
		default:
			s.audit(r, "session.connect_failed", hostTarget(h), map[string]any{"error": err.Error()})
			writeErrorExtra(w, http.StatusBadGateway, "connect_failed", "cannot connect to the host", map[string]any{"detail": err.Error()})
		}
		return
	}
	if t.Pinned == nil && res.SeenKey != nil {
		_ = s.store.PinHostKey(r.Context(), u.ID, h.ID, &store.HostKey{KeyType: res.SeenKey.KeyType, KeyData: res.SeenKey.KeyData, Fingerprint: res.SeenKey.Fingerprint})
	}
	sess, err := s.terms.Start(u.ID, h.ID, h.Name, res.Client, term.StartOptions{
		Cols: b.Cols, Rows: b.Rows, Persistence: h.Persistence, TmuxSession: h.TmuxSession,
	})
	if err != nil {
		if errors.Is(err, term.ErrTooMany) {
			writeError(w, http.StatusTooManyRequests, "too_many_sessions", "close some sessions first")
			return
		}
		writeErrorExtra(w, http.StatusBadGateway, "start_failed", "connected, but could not start a shell", map[string]any{"detail": err.Error()})
		return
	}
	_ = s.store.TouchHostConnected(r.Context(), u.ID, h.ID)
	s.audit(r, "session.open", hostTarget(h), map[string]any{"session": sess.ID})
	writeJSON(w, http.StatusCreated, sess.Info())
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
//           {"t":"state","state":"exited|lost|closed","code":N,"reason":"…"}
//           {"t":"pong","ts":N}
// Browser → server
//   binary: keyboard input
//   text:   {"t":"resize","cols":N,"rows":N} | {"t":"ping","ts":N}

const maxFrame = 64 << 10

func (s *Server) apiSessionSocket(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
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
	// Origin was already checked against the configured site in guardRequest.
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true, CompressionMode: websocket.CompressionDisabled})
	if err != nil {
		return
	}
	c.SetReadLimit(1 << 20)
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	untrack := s.trackSocket(u.ID, &liveSocket{loginHash: loginHash(r), cancel: cancel})
	defer untrack()

	sub, backlog, start, reset, ev := sess.Attach(offset, have)
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

	if err := writeText(map[string]any{"t": "hello", "offset": start, "reset": reset, "state": sess.State()}); err != nil {
		return
	}
	if err := writeData(start, backlog); err != nil {
		return
	}
	if ev != nil {
		_ = writeText(map[string]any{"t": "state", "state": ev.State, "code": ev.ExitCode, "reason": ev.Reason})
		c.Close(websocket.StatusNormalClosure, "session ended")
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
				if err := sess.Write(data); err != nil {
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

	for {
		select {
		case <-ctx.Done():
			c.Close(websocket.StatusGoingAway, "closing")
			return
		case ch := <-sub.C:
			if err := writeData(ch.Offset, ch.Data); err != nil {
				return
			}
		case e := <-sub.Events:
			_ = writeText(map[string]any{"t": "state", "state": e.State, "code": e.ExitCode, "reason": e.Reason})
			c.Close(websocket.StatusNormalClosure, "session ended")
			return
		case <-sub.Done():
			// Drain anything already queued before giving up on this viewer.
		drain:
			for {
				select {
				case ch := <-sub.C:
					if err := writeData(ch.Offset, ch.Data); err != nil {
						return
					}
				case e := <-sub.Events:
					_ = writeText(map[string]any{"t": "state", "state": e.State, "code": e.ExitCode, "reason": e.Reason})
					c.Close(websocket.StatusNormalClosure, "session ended")
					return
				default:
					break drain
				}
			}
			c.Close(websocket.StatusTryAgainLater, "reconnect")
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

	client, err := sftp.NewClient(sess.Client())
	if err != nil {
		writeErrorExtra(w, http.StatusBadGateway, "sftp_unavailable", "the host does not allow file transfer", map[string]any{"detail": err.Error()})
		return
	}
	defer client.Close()
	home, err := client.Getwd()
	if err != nil {
		writeErrorExtra(w, http.StatusBadGateway, "sftp_failed", "cannot find the home directory", map[string]any{"detail": err.Error()})
		return
	}
	dir := path.Join(home, ".cache", "corral", "uploads")
	if err := client.MkdirAll(dir); err != nil {
		writeErrorExtra(w, http.StatusBadGateway, "sftp_failed", "cannot create the upload directory", map[string]any{"detail": err.Error()})
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
		writeErrorExtra(w, http.StatusBadGateway, "sftp_failed", "cannot create the file", map[string]any{"detail": err.Error()})
		return
	}
	_ = f.Chmod(0o600)
	written, err := io.Copy(f, io.MultiReader(bytes.NewReader(head), part))
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		_ = client.Remove(full)
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeError(w, http.StatusRequestEntityTooLarge, "too_large", fmt.Sprintf("files are limited to %d MB", s.cfg.UploadMaxBytes>>20))
			return
		}
		writeErrorExtra(w, http.StatusBadGateway, "sftp_failed", "upload failed", map[string]any{"detail": err.Error()})
		return
	}
	s.audit(r, "file.upload", sess.HostName, map[string]any{"session": sess.ID, "path": full, "bytes": written, "type": ctype})
	writeJSON(w, http.StatusCreated, map[string]any{"path": full, "bytes": written, "type": ctype})
}
