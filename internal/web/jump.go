package web

// Jump hosts: a host may be reached through another host of the same owner.
// The jump host is connected to with its own saved credential and the key
// pinned for it (connected to directly once before); one level only.

import (
	"context"
	"errors"
	"net/http"

	"github.com/ihainan/corral/internal/netguard"
	"github.com/ihainan/corral/internal/sshx"
	"github.com/ihainan/corral/internal/store"
)

var (
	errJumpGone         = errors.New("the jump host no longer exists")
	errJumpChained      = errors.New("the jump host is itself reached through another host")
	errJumpNoCredential = errors.New("the jump host has no saved password or key")
	errJumpNotTrusted   = errors.New("connect to the jump host directly once first, to check its host key")
)

// storedTarget is what reaching h takes with what is stored: its saved
// credential, the key pinned for it, and its jump host if it has one.
func (s *Server) storedTarget(ctx context.Context, h *store.Host) (*sshx.Target, error) {
	if h.Auth == store.AuthAsk || h.CredentialID == nil {
		return nil, errors.New("no stored credential")
	}
	_, sec, err := s.openCredential(ctx, h.OwnerID, *h.CredentialID)
	if err != nil {
		return nil, err
	}
	k, err := s.store.HostKey(ctx, h.OwnerID, h.ID)
	if err != nil {
		return nil, errors.New("connect to this host once from a terminal first")
	}
	t := &sshx.Target{Address: h.Address, Port: h.Port, User: h.Username, Password: sec.Password, PrivateKey: []byte(sec.PrivateKey),
		Passphrase: []byte(sec.Passphrase), Pinned: &sshx.Pinned{KeyType: k.KeyType, KeyData: k.KeyData, Fingerprint: k.Fingerprint}}
	if t.Jump, _, err = s.jumpTarget(ctx, h); err != nil {
		return nil, err
	}
	return t, nil
}

// jumpTarget is what reaching h's jump host takes, or nil without one,
// with the jump host's connection version at that moment. It is read with
// the jump host's turn held, so that an edit of it (credential, address and
// key are saved one after another) is never seen half done.
func (s *Server) jumpTarget(ctx context.Context, h *store.Host) (*sshx.Target, int64, error) {
	if h.JumpHostID == nil {
		return nil, 0, nil
	}
	release, err := s.hostGate.acquire(ctx, [2]int64{h.OwnerID, *h.JumpHostID})
	if err != nil {
		return nil, 0, err
	}
	defer release()
	j, err := s.store.Host(ctx, h.OwnerID, *h.JumpHostID)
	if err != nil {
		return nil, 0, errJumpGone
	}
	if j.JumpHostID != nil {
		return nil, 0, errJumpChained
	}
	if j.Auth == store.AuthAsk || j.CredentialID == nil {
		return nil, 0, errJumpNoCredential
	}
	_, sec, err := s.openCredential(ctx, j.OwnerID, *j.CredentialID)
	if err != nil {
		return nil, 0, err
	}
	k, err := s.store.HostKey(ctx, j.OwnerID, j.ID)
	if err != nil {
		return nil, 0, errJumpNotTrusted
	}
	return &sshx.Target{Address: j.Address, Port: j.Port, User: j.Username, Password: sec.Password, PrivateKey: []byte(sec.PrivateKey),
		Passphrase: []byte(sec.Passphrase), Pinned: &sshx.Pinned{KeyType: k.KeyType, KeyData: k.KeyData, Fingerprint: k.Fingerprint}}, j.ConnVersion, nil
}

// jumpProblem is the user-facing code for a failure to reach a host through
// its jump host, or "".
func jumpProblem(err error) string {
	switch {
	case errors.Is(err, errJumpGone), errors.Is(err, errJumpChained):
		return "jump_invalid"
	case errors.Is(err, errJumpNoCredential):
		return "jump_no_credential"
	case errors.Is(err, errJumpNotTrusted):
		return "jump_not_trusted"
	}
	var je *sshx.JumpError
	if errors.As(err, &je) {
		return "jump_failed"
	}
	return ""
}

// writeJumpError answers a request that failed on the way through a jump host.
func writeJumpError(w http.ResponseWriter, err error) {
	code := jumpProblem(err)
	if code == "" {
		code = "jump_failed"
	}
	var hk *sshx.HostKeyChangedError
	switch {
	case errors.As(err, &hk):
		code = "jump_key_changed" // connect to it directly to check and accept the new key
	case errors.Is(err, sshx.ErrAuthFailed):
		code = "jump_auth_failed"
	case errors.Is(err, netguard.ErrForbidden):
		code = "target_forbidden"
	}
	// (Fixed text: what the hosts said is not passed on, only the reason
	// as failureReason puts it.)
	writeErrorExtra(w, http.StatusBadGateway, code, "cannot connect through the jump host", map[string]any{"reason": jumpReason(err)})
}

// jumpReason is failureReason of the cause behind a jump host failure (a
// word from a fixed list: nothing the hosts said).
func jumpReason(err error) string {
	var je *sshx.JumpError
	if errors.As(err, &je) {
		err = je.Err
	}
	return failureReason(err)
}

// forgetThrough closes the forwards and VS Code connections of the hosts
// reached through a host whose connection changed.
func (s *Server) forgetThrough(ctx context.Context, ownerID, hostID int64) {
	hosts, err := s.store.ListHosts(ctx, ownerID)
	if err != nil {
		return
	}
	for _, d := range hosts {
		if d.JumpHostID != nil && *d.JumpHostID == hostID {
			s.closeHostForwards(ownerID, d.ID)
			s.forgetHostCode(ownerID, d.ID)
		}
	}
}
