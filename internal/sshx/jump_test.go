package sshx

import (
	"context"
	"errors"
	"net/netip"
	"testing"
	"time"

	"github.com/ihainan/corral/internal/netguard"
	"github.com/ihainan/corral/internal/testutil"
)

func loopbackGuard() *netguard.Guard {
	return netguard.New([]netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}, nil)
}

// A host reached through a jump host: the second hop starts on the jump
// host, both host keys are checked, and the two connections end together.
func TestDialThroughJumpHost(t *testing.T) {
	jumpSrv := testutil.StartSSHServer(t, "alice", "alice-pass", t.TempDir())
	target := testutil.StartSSHServer(t, "bob", "bob-pass", t.TempDir())
	// The target is named "localhost", which the jump host resolves on its
	// side: the gateway's guard only ever sees the jump host's address.
	guard := loopbackGuard()
	jump := &Target{Address: "127.0.0.1", Port: jumpSrv.Addr.Port, User: "alice", Password: "alice-pass"}
	ctx := context.Background()

	r, err := Dial(ctx, guard, Target{Address: "localhost", Port: target.Addr.Port, User: "bob", Password: "bob-pass", Jump: jump})
	if err != nil {
		t.Fatal(err)
	}
	if out, err := Output(ctx, r.Client, "echo through", 1024); err != nil || len(out) == 0 {
		t.Fatalf("command through the jump host: %q %v", out, err)
	}
	if r.SeenKey == nil || r.SeenKey.Fingerprint == "" {
		t.Fatal("target key not reported")
	}

	// The jump host going away ends the connection through it.
	jumpSrv.Close()
	done := make(chan struct{})
	go func() { r.Client.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("connection through a closed jump host still open")
	}
}

func TestJumpFailures(t *testing.T) {
	jumpSrv := testutil.StartSSHServer(t, "alice", "alice-pass", t.TempDir())
	target := testutil.StartSSHServer(t, "bob", "bob-pass", t.TempDir())
	guard := loopbackGuard()
	ctx := context.Background()
	jump := func() *Target {
		return &Target{Address: "127.0.0.1", Port: jumpSrv.Addr.Port, User: "alice", Password: "alice-pass"}
	}

	// The jump host refuses the credential.
	bad := jump()
	bad.Password = "wrong"
	_, err := Dial(ctx, guard, Target{Address: "127.0.0.1", Port: target.Addr.Port, User: "bob", Password: "bob-pass", Jump: bad})
	var je *JumpError
	if !errors.As(err, &je) || !errors.Is(err, ErrAuthFailed) {
		t.Fatalf("jump auth failure: %v", err)
	}
	// The jump host's key is not the one pinned.
	pinned := jump()
	pinned.Pinned = &Pinned{KeyType: "ssh-ed25519", KeyData: []byte("other"), Fingerprint: "SHA256:other"}
	_, err = Dial(ctx, guard, Target{Address: "127.0.0.1", Port: target.Addr.Port, User: "bob", Password: "bob-pass", Jump: pinned})
	var hk *HostKeyChangedError
	if !errors.As(err, &je) || !errors.As(err, &hk) {
		t.Fatalf("jump key mismatch: %v", err)
	}
	// Nothing listens there on the jump host's side.
	_, err = Dial(ctx, guard, Target{Address: "127.0.0.1", Port: 1, User: "bob", Password: "bob-pass", Jump: jump()})
	if !errors.As(err, &je) {
		t.Fatalf("unreachable target: %v", err)
	}
	// Addresses that are never a host are refused before anything is dialled.
	for _, a := range []string{"169.254.169.254", "0.0.0.0", "ff02::1", "fe80::1"} {
		_, err = Dial(ctx, guard, Target{Address: a, Port: 22, User: "bob", Password: "bob-pass", Jump: jump()})
		if !errors.Is(err, netguard.ErrForbidden) {
			t.Errorf("%s through a jump host: %v", a, err)
		}
	}
	// One level only.
	twice := jump()
	twice.Jump = jump()
	_, err = Dial(ctx, guard, Target{Address: "127.0.0.1", Port: target.Addr.Port, User: "bob", Password: "bob-pass", Jump: twice})
	if !errors.As(err, &je) {
		t.Fatalf("jump through a jump: %v", err)
	}
	// The target's own key check still applies.
	_, err = Dial(ctx, guard, Target{Address: "127.0.0.1", Port: target.Addr.Port, User: "bob", Password: "bob-pass", Jump: jump(),
		Pinned: &Pinned{KeyType: "ssh-ed25519", KeyData: []byte("x"), Fingerprint: "SHA256:x"}})
	if !errors.As(err, &hk) || errors.As(err, &je) {
		t.Fatalf("target key mismatch: %v", err)
	}
}
