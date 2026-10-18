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
}

// Result carries the key we saw, so the caller can pin it after success.
type Result struct {
	Client   *ssh.Client
	SeenKey  *Pinned
	Replaced bool // the host presented the accepted new key instead of the pinned one
}

var ErrAuthFailed = errors.New("authentication failed")

const dialTimeout = 15 * time.Second

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
	cfg := &ssh.ClientConfig{
		User:    t.User,
		Auth:    auths,
		Timeout: dialTimeout,
		HostKeyCallback: func(_ string, _ net.Addr, key ssh.PublicKey) error {
			seen.KeyType = key.Type()
			seen.KeyData = key.Marshal()
			seen.Fingerprint = ssh.FingerprintSHA256(key)
			if t.Verify != nil {
				return t.Verify(seen)
			}
			if t.Pinned != nil && !bytes.Equal(t.Pinned.KeyData, seen.KeyData) {
				if t.Accept != "" && t.Accept == seen.Fingerprint {
					replaced = true
					return nil
				}
				return &HostKeyChangedError{Expected: t.Pinned.Fingerprint, Got: seen.Fingerprint}
			}
			return nil
		},
		// Prefer modern algorithms but keep RSA for older servers.
		HostKeyAlgorithms: nil,
	}

	conn, err := guard.DialTCP(ctx, t.Address, t.Port)
	if err != nil {
		return nil, err
	}
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}
	// Cancelling (sign-out, disable) during the handshake closes the socket at once.
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	c, chans, reqs, err := ssh.NewClientConn(conn, net.JoinHostPort(t.Address, strconv.Itoa(t.Port)), cfg)
	if !stop() && err == nil {
		c.Close()
		return nil, ctx.Err()
	}
	if err != nil {
		conn.Close()
		var hk *HostKeyChangedError
		if errors.As(err, &hk) {
			return nil, hk
		}
		if isAuthError(err) {
			return nil, ErrAuthFailed
		}
		return nil, err
	}
	_ = conn.SetDeadline(time.Time{})
	client := ssh.NewClient(c, chans, reqs)
	go keepalive(client)
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
