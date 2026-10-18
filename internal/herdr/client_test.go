package herdr

import (
	"context"
	"errors"
	"io"
	"net"
	"strconv"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/ihainan/corral/internal/sshx"
	"github.com/ihainan/corral/internal/testutil"
)

const streamLocal = "direct-streamlocal@openssh.com"

func dialTest(t *testing.T, srv *testutil.SSHServer) *ssh.Client {
	t.Helper()
	c, err := ssh.Dial("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(srv.Addr.Port)), &ssh.ClientConfig{User: "alice",
		Auth: []ssh.AuthMethod{ssh.Password("alice-pass")}, HostKeyCallback: ssh.InsecureIgnoreHostKey(), Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

// within fails the test if f does not return in time (the old code hung forever).
func within(t *testing.T, d time.Duration, f func() error) error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- f() }()
	select {
	case err := <-done:
		return err
	case <-time.After(d):
		t.Fatal("herdr call did not give up")
		return nil
	}
}

// A herdr that accepts the connection but never answers.
func silentHerdr(t *testing.T) *ssh.Client {
	srv := testutil.StartSSHServer(t, "alice", "alice-pass", t.TempDir())
	srv.HandleChannel(func(nc ssh.NewChannel) bool {
		if nc.ChannelType() != streamLocal {
			return false
		}
		ch, reqs, err := nc.Accept()
		if err == nil {
			go ssh.DiscardRequests(reqs)
			go io.Copy(io.Discard, ch)
		}
		return true
	})
	return dialTest(t, srv)
}

func TestCallTimesOutOnSilentHerdr(t *testing.T) {
	old := callTimeout
	callTimeout = 200 * time.Millisecond
	defer func() { callTimeout = old }()
	c := silentHerdr(t)
	cl, err := Dial(context.Background(), c, "/run/herdr.sock")
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()
	if err := within(t, 3*time.Second, func() error { _, err := cl.Snapshot(context.Background()); return err }); err == nil {
		t.Fatal("snapshot from a silent herdr succeeded")
	}
}

func TestCallHonoursCancel(t *testing.T) {
	c := silentHerdr(t)
	cl, err := Dial(context.Background(), c, "/run/herdr.sock")
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(100*time.Millisecond, cancel)
	if err := within(t, 3*time.Second, func() error { return cl.FocusPane(ctx, "p1") }); err == nil {
		t.Fatal("cancelled call succeeded")
	}
}

// A host that never answers the channel open: Dial gives up at the deadline
// (its own 10 s one when the caller has none, as for the watcher). The
// terminal's connection stays up; the unanswered opens keep their places, so
// once sshx.MaxAuxPerConn are stuck further herdr traffic is refused.
func TestDialGivesUpOnSilentHost(t *testing.T) {
	oldDial, oldMax := dialTimeout, sshx.MaxAuxPerConn
	dialTimeout, sshx.MaxAuxPerConn = 200*time.Millisecond, 2
	defer func() { dialTimeout, sshx.MaxAuxPerConn = oldDial, oldMax }()
	srv := testutil.StartSSHServer(t, "alice", "alice-pass", t.TempDir())
	hold := make(chan struct{})
	defer close(hold)
	srv.HandleChannel(func(nc ssh.NewChannel) bool {
		if nc.ChannelType() != streamLocal {
			return false
		}
		go func() { <-hold; nc.Reject(ssh.ConnectionFailed, "gone") }() // never answers the open in time
		return true
	})
	c := dialTest(t, srv)
	short, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	for _, ctx := range []context.Context{short, context.Background()} { // caller's deadline, then Dial's own
		if err := within(t, 3*time.Second, func() error { _, err := Dial(ctx, c, "/run/herdr.sock"); return err }); err == nil {
			t.Fatal("dial succeeded")
		}
	}
	if _, err := Dial(context.Background(), c, "/run/herdr.sock"); !errors.Is(err, sshx.ErrBusy) {
		t.Fatalf("dial with two stuck: %v", err)
	}
	closed := make(chan struct{})
	go func() { c.Wait(); close(closed) }()
	select {
	case <-closed:
		t.Fatal("stuck herdr traffic closed the terminal's connection")
	case <-time.After(300 * time.Millisecond):
	}
}
