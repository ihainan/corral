package web

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/ihainan/corral/internal/netguard"
	"github.com/ihainan/corral/internal/sshx"
	"github.com/ihainan/corral/internal/store"
)

// A host's connection, checked from its form before it is saved (POST
// /api/hosts/test), and its public key put on it with a password typed once
// (POST /api/hosts/install-key). Both go the way a connection to open a
// terminal goes — the network guard, the jump host, the host's key, signing
// in — and say where it failed, in words of a fixed list: nothing a host
// said is passed on. Nothing is stored: not the password, nor the key seen
// (a host's key is pinned when it is first connected to, as before). Each is
// audited without its secrets, and counted per user like sign-in attempts:
// it can be used to look about the network.

// hostCheckBody is the form as it stands, and the host it edits (0: a new one).
type hostCheckBody struct {
	hostBody
	ID int64 `json:"id"`
}

// hostCheck says how far a connection got. Stage, where it stopped: address
// (the name or the guard), jump, connect (the port), ssh (no SSH server
// there), hostkey, auth; reason, why there (a word of a fixed list); field,
// the form's field that belongs to it. Fingerprint: the key the host showed
// (new hosts: to be compared with the host's own, before it is pinned on
// first connect). LoginTested: signing in was tried (not for a host whose
// password is asked each time); Open: such a host let it in with nothing.
type hostCheck struct {
	OK          bool   `json:"ok"`
	Stage       string `json:"stage,omitempty"`
	Reason      string `json:"reason,omitempty"`
	Field       string `json:"field,omitempty"`
	Fingerprint string `json:"fingerprint,omitempty"`
	Expected    string `json:"expected,omitempty"`
	LoginTested bool   `json:"login_tested"`
	Open        bool   `json:"open,omitempty"`
	Installed   string `json:"installed,omitempty"` // (install-key: "added" or "present")
}

// hostCheckTarget is what reaching the form's host takes, as dialForOpen
// builds it: its credential (or the password typed; editing, the one
// stored where none is typed), its jump host, and the decision on its key —
// the one on record where the host is edited and its address, port and jump
// host are as they were; else none (shown, not pinned). A problem with the
// form is written as save would write it, and ok is false.
func (s *Server) hostCheckTarget(ctx context.Context, w http.ResponseWriter, r *http.Request, b *hostCheckBody) (t sshx.Target, _ *store.Host, ok bool) {
	u := currentUser(r)
	var h *store.Host
	if b.ID != 0 {
		var err error
		if h, err = s.store.Host(ctx, u.ID, b.ID); err != nil {
			writeError(w, http.StatusNotFound, "not_found", "no such host")
			return t, nil, false
		}
		if b.Persistence == "" {
			b.Persistence, b.TmuxSession, b.HerdrSession = h.Persistence, h.TmuxSession, h.HerdrSession
		}
	}
	if err := s.validateHost(ctx, u, b.ID, &b.hostBody); err != nil {
		if !writeFieldError(w, err) {
			s.internalError(w, r, err)
		}
		return t, nil, false
	}
	t = sshx.Target{Address: b.Address, Port: b.Port, User: b.Username}
	switch b.Auth {
	case store.AuthAsk:
		t.ProbeOnly = true
	case store.AuthKey:
		_, sec, err := s.openCredential(ctx, u.ID, *b.CredentialID)
		if err != nil {
			s.internalError(w, r, err)
			return t, nil, false
		}
		t.PrivateKey, t.Passphrase = []byte(sec.PrivateKey), []byte(sec.Passphrase)
	case store.AuthPassword:
		switch {
		case b.Password != "":
			t.Password = b.Password
		// (The one stored: only to where it was stored for — another address, port or jump
		// host is another machine, which a password it was not given for is not sent to.)
		case h != nil && h.Auth == store.AuthPassword && h.CredentialID != nil && h.Address == b.Address && h.Port == b.Port && sameID(h.JumpHostID, b.JumpHostID):
			_, sec, err := s.openCredential(ctx, u.ID, *h.CredentialID)
			if err != nil {
				s.internalError(w, r, err)
				return t, nil, false
			}
			t.Password = sec.Password
		case h != nil && h.Auth == store.AuthPassword && h.CredentialID != nil:
			writeErrorExtra(w, http.StatusUnprocessableEntity, "invalid_field", "the address changed: enter the password to check it", map[string]string{"field": "password"})
			return t, nil, false
		default:
			writeErrorExtra(w, http.StatusUnprocessableEntity, "invalid_field", "enter the password", map[string]string{"field": "password"})
			return t, nil, false
		}
	}
	// (A host of its own to ask the store about: what the form says, the user's.)
	form := &store.Host{OwnerID: u.ID, Name: b.Name, Address: b.Address, Port: b.Port, Username: b.Username, Auth: b.Auth, JumpHostID: b.JumpHostID}
	if h != nil {
		form.ID = h.ID
	}
	var pinned *store.HostKey
	if h != nil && h.Address == b.Address && h.Port == b.Port && sameID(h.JumpHostID, b.JumpHostID) {
		if k, err := s.store.HostKey(ctx, u.ID, h.ID); err == nil {
			pinned = k
		}
	}
	// (Decided in the handshake, before any credential is sent, as a connection's is.)
	t.Verify = func(seen *sshx.Pinned) error {
		if pinned != nil && !bytes.Equal(pinned.KeyData, seen.KeyData) {
			return &sshx.HostKeyChangedError{Expected: pinned.Fingerprint, Got: seen.Fingerprint}
		}
		return nil
	}
	return t, form, true
}

// checkDial dials t (its jump host, for form, first), closes what it got at
// once — nothing is kept, pooled or used — and says how far it got.
func (s *Server) checkDial(ctx context.Context, form *store.Host, t sshx.Target) (hostCheck, error) {
	authField := "password"
	if form.Auth == store.AuthKey {
		authField = "credential_id"
	}
	jt, _, err := s.jumpTarget(ctx, form)
	if err != nil {
		if ctx.Err() != nil {
			return hostCheck{}, ctx.Err()
		}
		return hostCheck{Stage: "jump", Reason: jumpProblem(err), Field: "jump_host_id"}, nil
	}
	t.Jump = jt
	res, err := sshx.Dial(ctx, s.guard, t)
	if res != nil && res.Client != nil {
		res.Client.Close() // (signed in: closed, no session nor channel opened on it)
	}
	seen := ""
	if res != nil && res.SeenKey != nil {
		seen = res.SeenKey.Fingerprint
	}
	switch {
	case err == nil:
		// (Probing a host whose password is asked each time, and let in with none: said.)
		return hostCheck{OK: true, Fingerprint: seen, LoginTested: !t.ProbeOnly, Open: t.ProbeOnly}, nil
	case errors.Is(err, sshx.ErrProbed):
		return hostCheck{OK: true, Fingerprint: seen}, nil
	}
	c, err := s.checkFailure(ctx, err)
	if c.Stage == "auth" && c.Reason != "bad_key" {
		c.Field, c.Fingerprint = authField, seen
	}
	return c, err
}

// hostCheckAllowed counts one check of the user's (with sign-in's limits,
// in a count of its own); written and false where there have been too many.
func (s *Server) hostCheckAllowed(w http.ResponseWriter, r *http.Request) bool {
	if !s.auth.AllowAttempt("host-check:" + strconv.FormatInt(currentUser(r).ID, 10)) {
		writeError(w, http.StatusTooManyRequests, "rate_limited", "too many connection checks; try again in a few minutes")
		return false
	}
	return true
}

// POST /api/hosts/test: the form's host connected to as a terminal would be,
// and closed at once (hostCheck).
func (s *Server) apiTestHost(w http.ResponseWriter, r *http.Request) {
	if !s.hostCheckAllowed(w, r) {
		return
	}
	var b hostCheckBody
	if err := readJSON(r, &b); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "the form could not be read")
		return
	}
	ctx := r.Context()
	t, form, ok := s.hostCheckTarget(ctx, w, r, &b)
	if !ok {
		return
	}
	c, err := s.checkDial(ctx, form, t)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "unauthorized", "please sign in")
		return
	}
	s.audit(r, "host.test", hostTarget(form), checkDetail(b.ID, c))
	writeJSON(w, http.StatusOK, c)
}

func checkDetail(id int64, c hostCheck) map[string]any {
	d := map[string]any{"ok": c.OK, "login_tested": c.LoginTested}
	if id != 0 {
		d["host"] = id
	}
	if c.Stage != "" {
		d["stage"], d["reason"] = c.Stage, c.Reason
	}
	if c.Open {
		d["open"] = true
	}
	if c.Installed != "" {
		d["installed"] = c.Installed
	}
	return d
}

// installKeyBody: the form (its key the one to put on the host) and the
// password to sign in with this once (never stored, nor logged).
type installKeyBody struct {
	hostCheckBody
	InstallPassword string `json:"install_password"`
}

// keyComment: what of a key's comment goes onto the host with it — letters,
// digits and a few marks (it is written by echo, which some shells read
// backslashes in): the rest dropped.
var keyComment = regexp.MustCompile(`[^A-Za-z0-9@._:+=-]`)

// authorizedLine is the key as a line of authorized_keys: its type, its
// data and its comment (keyComment's), from the credential's public key.
func authorizedLine(publicKey string) (line, typ, data string, ok bool) {
	k, comment, _, _, err := ssh.ParseAuthorizedKey([]byte(strings.TrimSpace(publicKey)))
	if err != nil {
		return "", "", "", false
	}
	full := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(k)))
	typ, data, _ = strings.Cut(full, " ")
	line = full
	if c := keyComment.ReplaceAllString(comment, ""); c != "" {
		line += " " + c
	}
	return line, typ, data, true
}

// installKeyScript puts the key line read from its input into the user's
// ~/.ssh/authorized_keys: the folder made (700) and the file (600) where
// they are not there; the line added only where that key (its type and
// data, as an entry: after any options, not in a comment) is not already
// in it, at the end, nothing else of it touched. Its words on a line of
// their own (a login file's output without a line break before it is not
// run into them). A
// link, or what is not a folder or a file there, or a file it may not write
// to, is said, not replaced. It prints "<nonce> OK added", "<nonce> OK
// present" or "<nonce> E <why>". Fixed POSIX script, run by sh whatever the
// user's shell is (fish too): it has no single quote nor backslash in it,
// so that it is the same however the login shell reads a quoted word. Its
// arguments: a hex nonce, and authorizedKeysScan; the key comes on its
// input, never on the command line.
const installKeyScript = `N=$1
A=$2
say() { echo; echo "$N $*"; }
umask 077
[ -n "$HOME" ] && [ -d "$HOME" ] || { say E nohome; exit 0; }
d=$HOME/.ssh
f=$d/authorized_keys
IFS= read -r k || [ -n "$k" ] || { say E nokey; exit 0; }
set -f
set -- $k
set +f
t=$1
b=$2
[ -n "$t" ] && [ -n "$b" ] || { say E nokey; exit 0; }
[ -L "$d" ] && { say E dirlink; exit 0; }
if [ ! -e "$d" ]; then mkdir -m 700 "$d" 2>/dev/null || { say E mkdir; exit 0; }; fi
[ -d "$d" ] || { say E notdir; exit 0; }
[ -L "$f" ] && { say E link; exit 0; }
if [ -e "$f" ] && [ ! -f "$f" ]; then say E notfile; exit 0; fi
if [ -f "$f" ]; then
  awk -v t="$t" -v b="$b" "$A" "$f" 2>/dev/null
  case $? in 0) say OK present; exit 0;; 1) ;; *) say E scan; exit 0;; esac
fi

new=
[ -e "$f" ] || new=1
if [ -z "$new" ] && [ ! -w "$f" ]; then say E notwritable; exit 0; fi
[ -w "$d" ] || [ -z "$new" ] || { say E notwritable; exit 0; }
if [ -s "$f" ] && [ -n "$(tail -c 1 "$f")" ]; then echo >> "$f" || { say E write; exit 0; }; fi
echo "$k" >> "$f" 2>/dev/null || { say E write; exit 0; }
[ -z "$new" ] || chmod 600 "$f" 2>/dev/null
say OK added
`

// authorizedKeysScan is the awk program installKeyScript asks whether the key
// (t, b: its type and data) is an entry of authorized_keys already: exit 0
// it is, 1 it is not. A line is read as sshd reads it — a comment or a blank
// line is none; options, if any, are name or name="value" separated by
// commas (a backslash-escaped quote inside a value no end), up to a space or
// tab — a line whose options are not so is no entry, as sshd refuses it;
// then the key's type and data (an RSA key's rsa-sha2-256 and rsa-sha2-512,
// which sshd takes for it, too); a CR at its end dropped (CRLF files). Only
// a line with the key's data in it, and of no more than 64 KiB, is read
// further (BusyBox's awk takes a long line a character at a time in time
// that grows with its square): one longer is taken for none — the worst
// that does is the key added again, which sshd minds not. Like the
// script, no single quote nor backslash in it (the characters made with
// sprintf), so it is passed as it is, quoted, under any login shell.
const authorizedKeysScan = `BEGIN { tab = sprintf("%c", 9); cr = sprintf("%c", 13); bs = sprintf("%c", 92); dq = sprintf("%c", 34); ws = "[ " tab "]+"; found = 0 }
{
  l = $0
  if (substr(l, length(l), 1) == cr) l = substr(l, 1, length(l) - 1)
  if (index(l, b) == 0 || length(l) > 65536) next
  sub("^" ws, "", l)
  if (l == "" || substr(l, 1, 1) == "#") next
  if (l !~ ("^(ssh-|ecdsa-|sk-|rsa-sha2-)[^ " tab "]*[ " tab "]")) {
    L = length(l)
    i = 1
    ok = 1
    while (1) {
      s0 = i
      while (i <= L && substr(l, i, 1) ~ /[A-Za-z0-9_-]/) i++
      if (i == s0) { ok = 0; break }
      c = substr(l, i, 1)
      if (c == "=") {
        i++
        if (substr(l, i, 1) != dq) { ok = 0; break }
        i++
        while (i <= L) {
          c = substr(l, i, 1)
          if (c == bs) { i += 2; continue }
          if (c == dq) break
          i++
        }
        if (i > L) { ok = 0; break }
        i++
        c = substr(l, i, 1)
      }
      if (c == ",") { i++; continue }
      if (c == " " || c == tab) break
      ok = 0
      break
    }
    if (!ok) next
    l = substr(l, i)
    sub("^" ws, "", l)
  }
  n = split(l, f, ws)
  if (n < 2 || f[2] != b) next
  if (f[1] == t || (t == "ssh-rsa" && (f[1] == "rsa-sha2-256" || f[1] == "rsa-sha2-512"))) { found = 1; exit }
}
END { exit (found ? 0 : 1) }
`

// installKeyCommand: installKeyScript run by sh, with the nonce and authorizedKeysScan.
func installKeyCommand(nonce string) string {
	return "sh -c " + sshx.QuoteShell(installKeyScript) + " corral " + sshx.QuoteShell(nonce) + " " + sshx.QuoteShell(authorizedKeysScan)
}

// POST /api/hosts/install-key: the form's key put on the host — signed in
// with the password typed (used once), installKeyScript run, the key line on
// its input — and then signed in with the key, to see it take.
func (s *Server) apiInstallHostKey(w http.ResponseWriter, r *http.Request) {
	if !s.hostCheckAllowed(w, r) {
		return
	}
	var b installKeyBody
	if err := readJSON(r, &b); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "the form could not be read")
		return
	}
	if b.Auth != store.AuthKey {
		writeErrorExtra(w, http.StatusUnprocessableEntity, "invalid_field", "choose a key", map[string]string{"field": "auth"})
		return
	}
	if b.InstallPassword == "" {
		writeErrorExtra(w, http.StatusUnprocessableEntity, "invalid_field", "enter the password", map[string]string{"field": "install_password"})
		return
	}
	ctx := r.Context()
	t, form, ok := s.hostCheckTarget(ctx, w, r, &b.hostCheckBody)
	if !ok {
		return
	}
	cred, err := s.store.Credential(ctx, currentUser(r).ID, *b.CredentialID)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	line, _, _, ok := authorizedLine(cred.PublicKey)
	if !ok {
		writeErrorExtra(w, http.StatusUnprocessableEntity, "invalid_field", "this key has no public key to put on the host", map[string]string{"field": "credential_id"})
		return
	}
	c, err := s.installKey(ctx, form, t, b.InstallPassword, line)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "unauthorized", "please sign in")
		return
	}
	s.audit(r, "host.install_key", hostTarget(form), checkDetail(b.ID, c))
	writeJSON(w, http.StatusOK, c)
}

// installKey signs in to t's host with password (not its key), runs
// installKeyScript with line on its input, closes that connection, and signs
// in with the key (checkDial) to see it take. Stage "install": the script
// said why it did not (reason its word); "verify": put there, but signing in
// with it did not work (reason as checkDial's).
func (s *Server) installKey(ctx context.Context, form *store.Host, keyed sshx.Target, password, line string) (hostCheck, error) {
	pw := keyed
	pw.PrivateKey, pw.Passphrase, pw.ProbeOnly = nil, nil, false
	pw.Password = password
	jt, _, err := s.jumpTarget(ctx, form)
	if err != nil {
		if ctx.Err() != nil {
			return hostCheck{}, ctx.Err()
		}
		return hostCheck{Stage: "jump", Reason: jumpProblem(err), Field: "jump_host_id"}, nil
	}
	pw.Jump = jt
	res, err := sshx.Dial(ctx, s.guard, pw)
	if err != nil {
		c, cerr := s.checkFailure(ctx, err)
		if c.Stage == "auth" {
			c.Field = "install_password"
		}
		return c, cerr
	}
	defer res.Client.Close()
	nonce := installNonce()
	ectx, cancel := context.WithTimeout(ctx, 20*time.Second)
	out, err := sshx.Exec(ectx, res.Client, installKeyCommand(nonce), 64<<10, sshx.ExecOptions{Stdin: strings.NewReader(line + "\n")})
	cancel()
	res.Client.Close()
	if err != nil && len(out.Out) == 0 {
		if ctx.Err() != nil {
			return hostCheck{}, ctx.Err()
		}
		return hostCheck{Stage: "install", Reason: "failed"}, nil
	}
	said := installSaid(string(out.Out), nonce)
	installed := ""
	switch {
	case said == "OK added":
		installed = "added"
	case said == "OK present":
		installed = "present"
	case strings.HasPrefix(said, "E "):
		why := strings.TrimPrefix(said, "E ")
		if !installReasons[why] {
			why = "failed"
		}
		return hostCheck{Stage: "install", Reason: why}, nil
	default:
		return hostCheck{Stage: "install", Reason: "failed"}, nil
	}
	// (Signed in with the key to the very machine the password went to: its key
	// as that connection saw it, whatever is on record.)
	first := res.SeenKey
	prev := keyed.Verify
	keyed.Verify = func(seen *sshx.Pinned) error {
		if prev != nil {
			if err := prev(seen); err != nil {
				return err
			}
		}
		if first == nil || !bytes.Equal(first.KeyData, seen.KeyData) {
			exp := ""
			if first != nil {
				exp = first.Fingerprint
			}
			return &sshx.HostKeyChangedError{Expected: exp, Got: seen.Fingerprint}
		}
		return nil
	}
	c, err := s.checkDial(ctx, form, keyed)
	if err != nil {
		return c, err
	}
	c.Installed = installed
	if !c.OK {
		c.Stage = "verify:" + c.Stage
	}
	return c, nil
}

// installSaid: what installKeyScript said, from its output (a line of its
// own, its nonce first; the last of them), "" for none.
func installSaid(out, nonce string) string {
	said := ""
	for _, l := range strings.Split(out, "\n") {
		if rest, ok := strings.CutPrefix(strings.TrimRight(l, "\r"), nonce+" "); ok {
			said = rest
		}
	}
	return said
}

// installReasons: the words installKeyScript says why it did not in.
var installReasons = map[string]bool{"scan": true, "nohome": true, "nokey": true, "dirlink": true, "mkdir": true, "notdir": true, "link": true, "notfile": true, "notwritable": true, "write": true}

// checkFailure is checkDial's account of a dial's failure (its jump host
// and the rest, as there).
func (s *Server) checkFailure(ctx context.Context, err error) (hostCheck, error) {
	var hk *sshx.HostKeyChangedError
	var je *sshx.JumpError
	switch {
	case ctx.Err() != nil && !errors.Is(err, context.DeadlineExceeded):
		return hostCheck{}, ctx.Err()
	case errors.As(err, &je):
		c := hostCheck{Stage: "jump", Field: "jump_host_id", Reason: jumpReason(err)}
		switch {
		case errors.As(je.Err, &hk):
			c.Reason = "key_changed"
		case errors.Is(je.Err, sshx.ErrAuthFailed):
			c.Reason = "auth_failed"
		case errors.Is(je.Err, netguard.ErrForbidden):
			c.Reason = "forbidden"
		}
		return c, nil
	case errors.As(err, &hk):
		return hostCheck{Stage: "hostkey", Reason: "changed", Field: "address", Fingerprint: hk.Got, Expected: hk.Expected}, nil
	case errors.Is(err, netguard.ErrForbidden):
		return hostCheck{Stage: "address", Reason: "forbidden", Field: "address"}, nil
	case errors.Is(err, sshx.ErrAuthNotOffered):
		return hostCheck{Stage: "auth", Reason: "not_offered", LoginTested: true}, nil
	case errors.Is(err, sshx.ErrAuthFailed):
		return hostCheck{Stage: "auth", Reason: "refused", LoginTested: true}, nil
	}
	switch why := failureReason(err); why {
	case "dns":
		return hostCheck{Stage: "address", Reason: "dns", Field: "address"}, nil
	case "bad_key":
		return hostCheck{Stage: "auth", Reason: "bad_key", Field: "credential_id"}, nil
	case "handshake":
		return hostCheck{Stage: "ssh", Reason: "handshake", Field: "port"}, nil
	case "refused":
		return hostCheck{Stage: "connect", Reason: "refused", Field: "port"}, nil
	default:
		return hostCheck{Stage: "connect", Reason: why, Field: "address"}, nil
	}
}

// installNonce: a marker of the gateway's own for one run of installKeyScript.
func installNonce() string {
	b := make([]byte, 12)
	rand.Read(b)
	return hex.EncodeToString(b)
}
