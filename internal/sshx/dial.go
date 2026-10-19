// Package sshx opens SSH connections to target hosts on behalf of a user.
package sshx

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/ihainan/corral/internal/netguard"
)

// HostKeyChangedError means the target presented a different key than the one
// pinned on first use. The connection is refused until the owner accepts it.
type HostKeyChangedError struct {
	Expected string
	Got      string
}

func (e *HostKeyChangedError) Error() string {
	return fmt.Sprintf("host key changed: expected %s, got %s", e.Expected, e.Got)
}

// Pinned is the key recorded for a host, or nil on first use.
type Pinned struct {
	KeyType     string
	KeyData     []byte
	Fingerprint string
}

type Target struct {
	Address string
	Port    int
	User    string

	Password   string // used for password and keyboard-interactive auth
	PrivateKey []byte // PEM / OpenSSH private key
	Passphrase []byte

	Pinned *Pinned
	// Accept is a fingerprint the user explicitly accepted after a key change.
	// A different key than the pinned one is allowed only if it matches exactly.
	Accept string
	// Verify, when set, makes the trust decision during the handshake, before
	// any credential is sent; it replaces the Pinned/Accept comparison.
	Verify func(seen *Pinned) error

	// Jump, when set, is the host this one is reached through: the gateway
	// connects to it, and from there to Address:Port. The network guard
	// applies to the jump host's address; the second hop starts on the jump
	// host and is only refused addresses that are never a host
	// (netguard.CheckLiteral).
	Jump *Target
}

// JumpError is a failure to connect to the jump host, or through it. Its
// text is fixed by kind: what the jump host said (it may echo what it was
// sent) is never part of it; Err keeps the cause for errors.Is and As.
type JumpError struct{ Err error }

func (e *JumpError) Error() string {
	var hk *HostKeyChangedError
	var oc *ssh.OpenChannelError
	switch {
	case errors.As(e.Err, &hk):
		return "jump host: its host key changed"
	case errors.Is(e.Err, ErrAuthFailed):
		return "jump host: it rejected the user name or credential"
	case errors.Is(e.Err, netguard.ErrForbidden):
		return "jump host: its address is not allowed"
	case errors.As(e.Err, &oc):
		return "jump host: it could not connect to the host"
	}
	return "jump host: cannot connect through it"
}

func (e *JumpError) Unwrap() error { return e.Err }

// Result carries the key we saw, so the caller can pin it after success.
type Result struct {
	Client   *ssh.Client
	SeenKey  *Pinned
	Replaced bool // the host presented the accepted new key instead of the pinned one
}

var ErrAuthFailed = errors.New("authentication failed")

// errHandshakeThroughJump: the handshake with a host reached through a jump
// host failed (what the other side said is not kept).
var errHandshakeThroughJump = errors.New("ssh: handshake failed with the host reached through the jump host")

const dialTimeout = 15 * time.Second

// RekeyThreshold overrides how many bytes pass before SSH re-negotiates keys
// (0 keeps the library default). Tests lower it to exercise key re-exchange.
var RekeyThreshold uint64

func Dial(ctx context.Context, guard *netguard.Guard, t Target) (*Result, error) {
	ctx, cancel := context.WithTimeout(ctx, dialTimeout)
	defer cancel()

	var auths []ssh.AuthMethod
	if len(t.PrivateKey) > 0 {
		var signer ssh.Signer
		var err error
		if len(t.Passphrase) > 0 {
			signer, err = ssh.ParsePrivateKeyWithPassphrase(t.PrivateKey, t.Passphrase)
		} else {
			signer, err = ssh.ParsePrivateKey(t.PrivateKey)
		}
		if err != nil {
			return nil, fmt.Errorf("cannot use private key: %w", err)
		}
		auths = append(auths, ssh.PublicKeys(signer))
	}
	if t.Password != "" {
		pw := t.Password
		auths = append(auths,
			ssh.Password(pw),
			// Many servers only offer keyboard-interactive; answer password-like prompts.
			ssh.KeyboardInteractive(func(_, _ string, questions []string, echos []bool) ([]string, error) {
				answers := make([]string, len(questions))
				for i := range questions {
					if !echos[i] {
						answers[i] = pw
					}
				}
				return answers, nil
			}))
	}
	if len(auths) == 0 {
		return nil, errors.New("no credentials available for this host")
	}

	seen := &Pinned{}
	replaced := false
	var verified []byte
	cfg := &ssh.ClientConfig{
		User:    t.User,
		Auth:    auths,
		Timeout: dialTimeout,
		HostKeyCallback: func(_ string, _ net.Addr, key ssh.PublicKey) error {
			// The callback also runs on every later key exchange of this
			// connection. Those must present the key verified at connect time;
			// they are not re-decided (the deciding request is long gone).
			if verified != nil {
				if !bytes.Equal(verified, key.Marshal()) {
					return &HostKeyChangedError{Expected: seen.Fingerprint, Got: ssh.FingerprintSHA256(key)}
				}
				return nil
			}
			seen.KeyType = key.Type()
			seen.KeyData = key.Marshal()
			seen.Fingerprint = ssh.FingerprintSHA256(key)
			if t.Verify != nil {
				if err := t.Verify(seen); err != nil {
					return err
				}
				verified = seen.KeyData
				return nil
			}
			if t.Pinned != nil && !bytes.Equal(t.Pinned.KeyData, seen.KeyData) {
				if t.Accept != "" && t.Accept == seen.Fingerprint {
					replaced = true
					verified = seen.KeyData
					return nil
				}
				return &HostKeyChangedError{Expected: t.Pinned.Fingerprint, Got: seen.Fingerprint}
			}
			verified = seen.KeyData
			return nil
		},
		// Prefer modern algorithms but keep RSA for older servers.
		HostKeyAlgorithms: nil,
	}
	cfg.RekeyThreshold = RekeyThreshold

	var conn net.Conn
	var jump *ssh.Client
	if t.Jump != nil {
		if t.Jump.Jump != nil {
			return nil, &JumpError{Err: errors.New("a jump host cannot itself be reached through another")}
		}
		if err := netguard.CheckLiteral(t.Address); err != nil {
			return nil, err
		}
		jr, err := Dial(ctx, guard, *t.Jump)
		if err != nil {
			return nil, &JumpError{Err: err}
		}
		jump = jr.Client
		ch, err := jump.DialContext(ctx, "tcp", net.JoinHostPort(t.Address, strconv.Itoa(t.Port)))
		if err != nil {
			jump.Close()
			return nil, &JumpError{Err: fmt.Errorf("cannot reach %s from the jump host: %w", t.Address, err)}
		}
		conn = &jumpConn{Conn: ch, jump: jump}
	} else {
		var err error
		conn, err = guard.DialTCP(ctx, t.Address, t.Port)
		if err != nil {
			return nil, err
		}
	}
	closeAll := func() { conn.Close() } // (through a jump host: both, the jump connection first)
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}
	// Cancelling (sign-out, disable) during the handshake closes the socket at once.
	stop := context.AfterFunc(ctx, closeAll)
	c, chans, reqs, err := ssh.NewClientConn(conn, net.JoinHostPort(t.Address, strconv.Itoa(t.Port)), cfg)
	if !stop() && err == nil {
		c.Close()
		closeAll()
		return nil, ctx.Err()
	}
	if err != nil {
		closeAll()
		var hk *HostKeyChangedError
		if errors.As(err, &hk) {
			return nil, hk
		}
		if isAuthError(err) {
			return nil, ErrAuthFailed
		}
		if jump != nil {
			// Through a jump host, the other side of this handshake may be the
			// jump host itself, which could put what it was sent into its
			// messages: none of them is passed on.
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, errHandshakeThroughJump
		}
		return nil, err
	}
	_ = conn.SetDeadline(time.Time{})
	client := ssh.NewClient(c, chans, reqs)
	go keepalive(client)
	if jump != nil {
		// The connection through the jump host lives as long as both: the
		// jump connection ending ends it, and it ending (closed here, or by
		// the host) closes the jump one.
		go func() {
			client.Wait()
			jump.Close()
		}()
		go func() {
			jump.Wait()
			client.Close()
		}()
	}
	return &Result{Client: client, SeenKey: seen, Replaced: replaced}, nil
}

func isAuthError(err error) bool {
	return err != nil && bytes.Contains([]byte(err.Error()), []byte("unable to authenticate"))
}

// keepalive probes the server and closes the client when it stops answering, so
// a dead network path surfaces as a closed session instead of a silent hang.
func keepalive(c *ssh.Client) {
	closed := make(chan struct{})
	go func() {
		_ = c.Wait()
		close(closed)
	}()
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	misses := 0
	for {
		select {
		case <-closed:
			return
		case <-t.C:
		}
		errc := make(chan error, 1)
		go func() {
			_, _, err := c.SendRequest("keepalive@openssh.com", true, nil)
			errc <- err
		}()
		select {
		case <-closed:
			return
		case err := <-errc:
			if err != nil {
				misses++
			} else {
				misses = 0
			}
		case <-time.After(15 * time.Second):
			misses++
		}
		if misses >= 3 {
			c.Close()
			return
		}
	}
}

// QuoteShell single-quotes s for a POSIX shell. Remote commands are built only
// from constants and values passed through this function.
func QuoteShell(s string) string {
	var b bytes.Buffer
	b.WriteByte('\'')
	for _, r := range []byte(s) {
		if r == '\'' {
			b.WriteString(`'\''`)
		} else {
			b.WriteByte(r)
		}
	}
	b.WriteByte('\'')
	return b.String()
}

// jumpConn is the connection to a host through a jump host: closing it
// closes the jump connection first, so that a jump host that does not
// answer cannot hold the close (or a cancelled handshake) up.
type jumpConn struct {
	net.Conn
	jump *ssh.Client
}

func (c *jumpConn) Close() error {
	c.jump.Close()
	return c.Conn.Close()
}
