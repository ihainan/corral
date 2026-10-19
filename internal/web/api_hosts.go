package web

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/ihainan/corral/internal/store"
)

// secret is the plaintext stored inside a credential's ciphertext.
type secret struct {
	Password   string `json:"password,omitempty"`
	PrivateKey string `json:"private_key,omitempty"`
	Passphrase string `json:"passphrase,omitempty"`
}

// credAAD binds a ciphertext to its owner and row.
func credAAD(ownerID, id int64) []byte {
	return []byte("corral-credential:" + strconv.FormatInt(ownerID, 10) + ":" + strconv.FormatInt(id, 10))
}

func (s *Server) saveCredential(ctx context.Context, ownerID int64, name, kind, publicKey string, sec secret) (int64, error) {
	id, err := s.store.ReserveCredential(ctx, ownerID, name, kind, publicKey)
	if err != nil {
		return 0, err
	}
	if err := s.sealInto(ctx, ownerID, id, sec); err != nil {
		// (Removed also when the request was given up meanwhile.)
		_ = s.store.DeleteCredential(context.WithoutCancel(ctx), ownerID, id)
		return 0, err
	}
	return id, nil
}

func (s *Server) sealInto(ctx context.Context, ownerID, id int64, sec secret) error {
	plain, err := json.Marshal(sec)
	if err != nil {
		return err
	}
	sealed, err := s.vault.Seal(plain, credAAD(ownerID, id))
	if err != nil {
		return err
	}
	return s.store.FillCredential(ctx, ownerID, id, sealed.Ciphertext, sealed.WrappedKey, sealed.KeyID)
}

func (s *Server) openCredential(ctx context.Context, ownerID, id int64) (*store.Credential, *secret, error) {
	c, err := s.store.Credential(ctx, ownerID, id)
	if err != nil {
		return nil, nil, err
	}
	plain, err := s.vault.Open(vaultSealed(c), credAAD(ownerID, id))
	if err != nil {
		return nil, nil, err
	}
	var sec secret
	if err := json.Unmarshal(plain, &sec); err != nil {
		return nil, nil, err
	}
	return c, &sec, nil
}

// Hosts --------------------------------------------------------------------

type hostBody struct {
	Name         string `json:"name"`
	Address      string `json:"address"`
	Port         int    `json:"port"`
	Username     string `json:"username"`
	Auth         string `json:"auth"`
	Password     string `json:"password"`      // auth=password: new password (empty keeps the stored one)
	CredentialID *int64 `json:"credential_id"` // auth=key
	JumpHostID   *int64 `json:"jump_host_id"`  // reached through this host of the user's, or null
	Persistence  string `json:"persistence"`
	TmuxSession  string `json:"tmux_session"`
	HerdrSession string `json:"herdr_session"`
	// Edits: the host's version the form was loaded at (see store.Host).
	// Omitted, the edit is saved whatever changed since.
	Version *int64 `json:"version"`
}

var (
	// tmux rewrites "." and ":" in session names, which would break attaching
	// by name, so they are not allowed.
	tmuxNameRE = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	hostnameRE = regexp.MustCompile(`^[A-Za-z0-9_]([A-Za-z0-9._-]{0,251}[A-Za-z0-9_])?$`)
	// The user name travels in the SSH protocol, never through a shell, so any
	// visible characters are fine (e.g. "user@corp", "DOMAIN\\user").
	sshUserRE  = regexp.MustCompile(`^[^\s\x00-\x1f\x7f]{1,255}$`)
	tmuxUnsafe = regexp.MustCompile(`[^A-Za-z0-9_-]`)
)

// defaultTmuxSession is "corral-<user>" with characters tmux would rewrite replaced.
func defaultTmuxSession(username string) string {
	name := "corral-" + tmuxUnsafe.ReplaceAllString(username, "_")
	if len(name) > 64 {
		name = name[:64]
	}
	return name
}

type fieldError struct{ field, msg string }

func (e *fieldError) Error() string { return e.field + ": " + e.msg }

// validateHost checks a host's form; hostID is the host being edited (0 when
// creating).
func (s *Server) validateHost(ctx context.Context, owner *store.User, hostID int64, b *hostBody) error {
	b.Name = strings.TrimSpace(b.Name)
	b.Address = strings.TrimSpace(b.Address)
	b.Username = strings.TrimSpace(b.Username)
	if b.Name == "" || len([]rune(b.Name)) > 64 {
		return &fieldError{"name", "required, at most 64 characters"}
	}
	addr := strings.TrimSuffix(strings.TrimPrefix(b.Address, "["), "]")
	if !hostnameRE.MatchString(addr) && !strings.Contains(addr, ":") {
		return &fieldError{"address", "must be a host name or IP address"}
	}
	b.Address = addr
	if b.Port == 0 {
		b.Port = 22
	}
	if b.Port < 1 || b.Port > 65535 {
		return &fieldError{"port", "must be 1–65535"}
	}
	if !sshUserRE.MatchString(b.Username) {
		return &fieldError{"username", "enter the user name, without spaces"}
	}
	switch b.Auth {
	case store.AuthPassword, store.AuthAsk:
	case store.AuthKey:
		if b.CredentialID == nil {
			return &fieldError{"credential_id", "choose a key"}
		}
		c, err := s.store.Credential(ctx, owner.ID, *b.CredentialID)
		if err != nil || c.Kind != store.CredPrivateKey {
			return &fieldError{"credential_id", "choose one of your keys"}
		}
	default:
		return &fieldError{"auth", "must be password, key or ask"}
	}
	if b.Persistence == "" {
		b.Persistence = store.PersistTmux
	}
	if b.Persistence != store.PersistNone && b.Persistence != store.PersistTmux && b.Persistence != store.PersistHerdr {
		return &fieldError{"persistence", "must be none, tmux or herdr"}
	}
	b.HerdrSession = strings.TrimSpace(b.HerdrSession)
	if b.HerdrSession != "" && !tmuxNameRE.MatchString(b.HerdrSession) {
		return &fieldError{"herdr_session", "letters, digits, dash and underscore only"}
	}
	b.TmuxSession = strings.TrimSpace(b.TmuxSession)
	if b.TmuxSession == "" {
		b.TmuxSession = defaultTmuxSession(owner.Username)
	}
	if !tmuxNameRE.MatchString(b.TmuxSession) {
		return &fieldError{"tmux_session", "letters, digits, dash and underscore only"}
	}
	// Hosts reached through this one need it reachable directly, with a
	// saved credential.
	var through []string
	if hostID != 0 {
		var err error
		if through, err = s.store.HostsThrough(ctx, owner.ID, hostID); err != nil {
			return err
		}
	}
	if len(through) > 0 && b.Auth == store.AuthAsk {
		return &fieldError{"auth", "other hosts are reached through this one: it needs a saved password or key"}
	}
	if b.JumpHostID != nil {
		if *b.JumpHostID == hostID {
			return &fieldError{"jump_host_id", "a host cannot be reached through itself"}
		}
		j, err := s.store.Host(ctx, owner.ID, *b.JumpHostID)
		if err != nil {
			return &fieldError{"jump_host_id", "choose one of your hosts"}
		}
		switch {
		case j.JumpHostID != nil:
			return &fieldError{"jump_host_id", "that host is itself reached through another one"}
		case j.Auth == store.AuthAsk || j.CredentialID == nil:
			return &fieldError{"jump_host_id", "that host needs a saved password or key to be a jump host"}
		case len(through) > 0:
			return &fieldError{"jump_host_id", "other hosts are reached through this one, so it cannot use a jump host itself"}
		}
	}
	return nil
}

func writeFieldError(w http.ResponseWriter, err error) bool {
	var fe *fieldError
	if errors.As(err, &fe) {
		writeErrorExtra(w, http.StatusUnprocessableEntity, "invalid_field", fe.msg, map[string]string{"field": fe.field})
		return true
	}
	return false
}

func (s *Server) apiListHosts(w http.ResponseWriter, r *http.Request) {
	hosts, err := s.store.ListHosts(r.Context(), currentUser(r).ID)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, hosts)
}

func (s *Server) apiGetHost(w http.ResponseWriter, r *http.Request) {
	h, ok := s.ownedHost(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, h)
}

func (s *Server) ownedHost(w http.ResponseWriter, r *http.Request) (*store.Host, bool) {
	id, ok := pathID(r)
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "no such host")
		return nil, false
	}
	h, err := s.store.Host(r.Context(), currentUser(r).ID, id)
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "no such host")
		return nil, false
	}
	return h, true
}

func (s *Server) apiCreateHost(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	var b hostBody
	if err := readJSON(r, &b); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	if err := s.validateHost(r.Context(), u, 0, &b); err != nil {
		writeFieldError(w, err)
		return
	}
	h := &store.Host{OwnerID: u.ID, Name: b.Name, Address: b.Address, Port: b.Port, Username: b.Username,
		Auth: b.Auth, Persistence: b.Persistence, TmuxSession: b.TmuxSession, HerdrSession: b.HerdrSession, JumpHostID: b.JumpHostID}
	switch b.Auth {
	case store.AuthKey:
		h.CredentialID = b.CredentialID
	case store.AuthPassword:
		if b.Password == "" {
			writeErrorExtra(w, http.StatusUnprocessableEntity, "invalid_field", "enter the password", map[string]string{"field": "password"})
			return
		}
		id, err := s.saveCredential(r.Context(), u.ID, "password for "+b.Name, store.CredPassword, "", secret{Password: b.Password})
		if err != nil {
			s.internalError(w, r, err)
			return
		}
		h.CredentialID = &id
	}
	created, err := s.store.CreateHost(r.Context(), h)
	if err != nil && created == nil && b.Auth == store.AuthPassword && h.CredentialID != nil {
		// Not saved: the password made for it goes too.
		_ = s.store.DeleteCredential(context.WithoutCancel(r.Context()), u.ID, *h.CredentialID)
	}
	if errors.Is(err, store.ErrJumpConflict) {
		writeError(w, http.StatusConflict, "jump_conflict", "the jump host cannot be used; it was changed meanwhile")
		return
	}
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	s.audit(r, "host.create", hostTarget(created), nil)
	writeJSON(w, http.StatusCreated, created)
}

// hostTurn runs f with the host's turn held (see underHostGate): edits do not
// interleave with opens, code-server starts or forwarding dials of the host.
func (s *Server) hostTurn(w http.ResponseWriter, r *http.Request, f func(w http.ResponseWriter)) {
	id, ok := pathID(r)
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "no such host")
		return
	}
	resp, err := s.underHostGate(r.Context(), currentUser(r).ID, id, f)
	switch {
	case errors.Is(err, errGateBusy):
		writeError(w, http.StatusTooManyRequests, "busy", "this host is busy; try again in a moment")
	case err == nil:
		resp.writeTo(w)
	}
}

func (s *Server) apiUpdateHost(w http.ResponseWriter, r *http.Request) {
	var b hostBody
	if err := readJSON(r, &b); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	s.hostTurn(w, r, func(w http.ResponseWriter) { s.updateHost(w, r, &b) })
}

func (s *Server) updateHost(w http.ResponseWriter, r *http.Request, b *hostBody) {
	u := currentUser(r)
	h, ok := s.ownedHost(w, r)
	if !ok {
		return
	}
	// Saved since the form was loaded (on another device or tab): saving it
	// would silently undo that edit. Checked before anything is stored.
	if b.Version != nil && *b.Version != h.Version {
		writeHostChanged(w)
		return
	}
	// What terminals run in is chosen when opening; the old per-host
	// settings are kept as they are when an edit leaves them out.
	if b.Persistence == "" {
		b.Persistence, b.TmuxSession, b.HerdrSession = h.Persistence, h.TmuxSession, h.HerdrSession
	}
	if err := s.validateHost(r.Context(), u, h.ID, b); err != nil {
		writeFieldError(w, err)
		return
	}
	endpointChanged := h.Address != b.Address || h.Port != b.Port || !sameID(h.JumpHostID, b.JumpHostID)
	// Anything that changes what a connection to the host is: connections
	// made before are then neither reused nor borrowed nor kept in the pool.
	connChanged := endpointChanged || h.Username != b.Username || h.Auth != b.Auth ||
		(b.Auth == store.AuthKey && !sameID(h.CredentialID, b.CredentialID)) || (b.Auth == store.AuthPassword && b.Password != "")
	oldCred := h.CredentialID
	oldAuth := h.Auth
	h.Name, h.Address, h.Port, h.Username, h.Persistence, h.TmuxSession = b.Name, b.Address, b.Port, b.Username, b.Persistence, b.TmuxSession
	h.HerdrSession = b.HerdrSession
	h.Auth = b.Auth
	h.JumpHostID = b.JumpHostID

	var dropCred *int64 // a host-private password credential that is no longer used
	// A new password goes into a credential of its own, which the host is
	// switched to in the same save: refused, the host keeps its old one and
	// the new one is removed.
	var madeCred *int64
	switch b.Auth {
	case store.AuthKey:
		h.CredentialID = b.CredentialID
		if oldAuth == store.AuthPassword {
			dropCred = oldCred
		}
	case store.AuthAsk:
		h.CredentialID = nil
		if oldAuth == store.AuthPassword {
			dropCred = oldCred
		}
	case store.AuthPassword:
		keep := oldAuth == store.AuthPassword && oldCred != nil
		if b.Password == "" && !keep {
			writeErrorExtra(w, http.StatusUnprocessableEntity, "invalid_field", "enter the password", map[string]string{"field": "password"})
			return
		}
		if b.Password != "" {
			id, err := s.saveCredential(r.Context(), u.ID, "password for "+b.Name, store.CredPassword, "", secret{Password: b.Password})
			if err != nil {
				s.internalError(w, r, err)
				return
			}
			h.CredentialID = &id
			madeCred = &id
			if keep {
				dropCred = oldCred
			}
		}
	}
	err := s.store.UpdateHost(r.Context(), h, connChanged)
	// What follows the save is done whatever becomes of the request.
	bg, cancelBg := context.WithTimeout(context.WithoutCancel(r.Context()), 10*time.Second)
	defer cancelBg()
	if err != nil {
		if madeCred != nil {
			_ = s.store.DeleteCredential(bg, u.ID, *madeCred)
		}
		switch {
		case errors.Is(err, store.ErrNotFound):
			writeHostChanged(w) // changed or deleted since it was read
		case errors.Is(err, store.ErrJumpConflict):
			writeError(w, http.StatusConflict, "jump_conflict", "the jump hosts were changed meanwhile; check and save again")
		default:
			s.internalError(w, r, err)
		}
		return
	}
	if dropCred != nil {
		_ = s.store.DeleteCredential(bg, u.ID, *dropCred)
	}
	if endpointChanged {
		_ = s.store.ForgetHostKey(bg, u.ID, h.ID) // a different machine is verified afresh
	}
	if connChanged {
		// Nothing open through the old settings carries on, here or on the
		// hosts reached through this one.
		s.closeHostForwards(u.ID, h.ID)
		s.forgetHostCode(u.ID, h.ID)
		s.forgetThrough(bg, u.ID, h.ID)
		s.picks.dropHost(u.ID, h.ID)
	}
	s.audit(r, "host.update", hostTarget(h), map[string]any{"endpoint_changed": endpointChanged, "connection_changed": connChanged})
	updated, _ := s.store.Host(bg, u.ID, h.ID)
	writeJSON(w, http.StatusOK, updated)
}

func writeHostChanged(w http.ResponseWriter) {
	writeError(w, http.StatusConflict, "host_changed", "this host was changed elsewhere; open it again to edit it")
}

func sameID(a, b *int64) bool { return (a == nil && b == nil) || (a != nil && b != nil && *a == *b) }

func (s *Server) apiDeleteHost(w http.ResponseWriter, r *http.Request) {
	s.hostTurn(w, r, func(w http.ResponseWriter) { s.deleteHost(w, r) })
}

func (s *Server) deleteHost(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	h, ok := s.ownedHost(w, r)
	if !ok {
		return
	}
	// Hosts reached through it keep it.
	if names, err := s.store.HostsThrough(r.Context(), u.ID, h.ID); err == nil && len(names) > 0 {
		writeErrorExtra(w, http.StatusConflict, "jump_in_use", "other hosts are reached through this host", map[string]any{"hosts": names})
		return
	}
	// VS Code left running on the host is stopped (best effort, after the
	// delete, with what is needed to reach the host read now).
	stopCode := s.codeStopperFor(r.Context(), h)
	if err := s.store.DeleteHost(r.Context(), u.ID, h.ID); errors.Is(err, store.ErrInUse) {
		names, _ := s.store.HostsThrough(r.Context(), u.ID, h.ID)
		writeErrorExtra(w, http.StatusConflict, "jump_in_use", "other hosts are reached through this host", map[string]any{"hosts": names})
		return
	} else if err != nil {
		s.internalError(w, r, err)
		return
	}
	// Deleted first, then closed: a connect finishing now fails its host re-check.
	s.cancelPendingForHost(u.ID, h.ID)
	s.picks.dropHost(u.ID, h.ID)
	s.closeHostForwards(u.ID, h.ID)
	s.forgetHostCode(u.ID, h.ID)
	s.terms.CloseHost(u.ID, h.ID, "host deleted")
	if h.Auth == store.AuthPassword && h.CredentialID != nil {
		_ = s.store.DeleteCredential(r.Context(), u.ID, *h.CredentialID)
	}
	s.audit(r, "host.delete", hostTarget(h), nil)
	if stopCode != nil {
		stopCode()
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) apiGetHostKey(w http.ResponseWriter, r *http.Request) {
	h, ok := s.ownedHost(w, r)
	if !ok {
		return
	}
	k, err := s.store.HostKey(r.Context(), currentUser(r).ID, h.ID)
	if errors.Is(err, store.ErrNotFound) {
		writeJSON(w, http.StatusOK, map[string]any{"pinned": false})
		return
	}
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"pinned": true, "key_type": k.KeyType, "fingerprint": k.Fingerprint, "first_seen": k.FirstSeen})
}

func hostTarget(h *store.Host) string {
	return fmt.Sprintf("%s (%s@%s:%d)", h.Name, h.Username, h.Address, h.Port)
}

// Credentials --------------------------------------------------------------

type credentialView struct {
	ID        int64  `json:"id"`
	Name      string `json:"name"`
	Kind      string `json:"kind"`
	PublicKey string `json:"public_key"`
	CreatedAt int64  `json:"created_at"`
}

func (s *Server) apiListCredentials(w http.ResponseWriter, r *http.Request) {
	creds, err := s.store.ListCredentials(r.Context(), currentUser(r).ID)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	// Host-bound passwords are managed through the host form; list keys only.
	out := []credentialView{}
	for _, c := range creds {
		if c.Kind == store.CredPrivateKey {
			out = append(out, credentialView{ID: c.ID, Name: c.Name, Kind: c.Kind, PublicKey: c.PublicKey, CreatedAt: c.CreatedAt})
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) apiCreateCredential(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	var b struct {
		Name       string `json:"name"`
		Mode       string `json:"mode"` // "generate" or "import"
		PrivateKey string `json:"private_key"`
		Passphrase string `json:"passphrase"`
	}
	if err := readJSON(r, &b); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	b.Name = strings.TrimSpace(b.Name)
	if b.Name == "" || len([]rune(b.Name)) > 64 {
		writeErrorExtra(w, http.StatusUnprocessableEntity, "invalid_field", "required, at most 64 characters", map[string]string{"field": "name"})
		return
	}
	var sec secret
	var pub ssh.PublicKey
	switch b.Mode {
	case "generate":
		pk, sk, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			s.internalError(w, r, err)
			return
		}
		block, err := ssh.MarshalPrivateKey(sk, "corral:"+u.Username)
		if err != nil {
			s.internalError(w, r, err)
			return
		}
		sec.PrivateKey = string(pem.EncodeToMemory(block))
		pub, _ = ssh.NewPublicKey(pk)
	case "import":
		var signer ssh.Signer
		var err error
		if b.Passphrase != "" {
			signer, err = ssh.ParsePrivateKeyWithPassphrase([]byte(b.PrivateKey), []byte(b.Passphrase))
		} else {
			signer, err = ssh.ParsePrivateKey([]byte(b.PrivateKey))
		}
		if err != nil {
			var missing *ssh.PassphraseMissingError
			if errors.As(err, &missing) {
				writeErrorExtra(w, http.StatusUnprocessableEntity, "invalid_field", "this key needs its passphrase", map[string]string{"field": "passphrase"})
				return
			}
			writeErrorExtra(w, http.StatusUnprocessableEntity, "invalid_field", "not a valid private key", map[string]string{"field": "private_key"})
			return
		}
		sec.PrivateKey, sec.Passphrase = b.PrivateKey, b.Passphrase
		pub = signer.PublicKey()
	default:
		writeError(w, http.StatusBadRequest, "bad_request", "mode must be generate or import")
		return
	}
	pubLine := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(pub))) + " corral:" + u.Username
	id, err := s.saveCredential(r.Context(), u.ID, b.Name, store.CredPrivateKey, pubLine, sec)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	s.audit(r, "credential.create", b.Name, map[string]any{"mode": b.Mode, "fingerprint": ssh.FingerprintSHA256(pub)})
	writeJSON(w, http.StatusCreated, credentialView{ID: id, Name: b.Name, Kind: store.CredPrivateKey, PublicKey: pubLine})
}

func (s *Server) apiRenameCredential(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "no such key")
		return
	}
	var b struct {
		Name string `json:"name"`
	}
	if err := readJSON(r, &b); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	name := strings.TrimSpace(b.Name)
	if name == "" || len([]rune(name)) > 64 {
		writeErrorExtra(w, http.StatusUnprocessableEntity, "invalid_field", "required, at most 64 characters", map[string]string{"field": "name"})
		return
	}
	if err := s.store.RenameCredential(r.Context(), currentUser(r).ID, id, name); err != nil {
		writeError(w, http.StatusNotFound, "not_found", "no such key")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) apiDeleteCredential(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	id, ok := pathID(r)
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "no such key")
		return
	}
	c, err := s.store.Credential(r.Context(), u.ID, id)
	if err != nil || c.Kind != store.CredPrivateKey {
		writeError(w, http.StatusNotFound, "not_found", "no such key")
		return
	}
	hosts, err := s.store.ListHosts(r.Context(), u.ID)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	var users []string
	for _, h := range hosts {
		if h.CredentialID != nil && *h.CredentialID == id {
			users = append(users, h.Name)
		}
	}
	if len(users) > 0 {
		writeErrorExtra(w, http.StatusConflict, "in_use", "this key is used by hosts", map[string]any{"hosts": users})
		return
	}
	if err := s.store.DeleteCredential(r.Context(), u.ID, id); err != nil {
		writeError(w, http.StatusNotFound, "not_found", "no such key")
		return
	}
	s.audit(r, "credential.delete", c.Name, nil)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
