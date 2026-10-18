package sshx

import (
	"context"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/ihainan/corral/internal/testutil"
)

func lowerGrace(t *testing.T) {
	old := StuckGrace
	StuckGrace = 150 * time.Millisecond
	t.Cleanup(func() { StuckGrace = old })
}

// waitClosed fails unless c's connection is closed soon.
func waitClosed(t *testing.T, c *ssh.Client) {
	t.Helper()
	closed := make(chan struct{})
	go func() { c.Wait(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(3 * time.Second):
		t.Fatal("SSH connection with a stuck operation was left open")
	}
}

// returnsWithin fails the test if f does not return in time.
func returnsWithin(t *testing.T, d time.Duration, f func()) {
	t.Helper()
	done := make(chan struct{})
	go func() { f(); close(done) }()
	select {
	case <-done:
	case <-time.After(d):
		t.Fatal("operation did not return")
	}
}

// A host that never answers the session open: Output gives up at the
// deadline and the connection is closed, so its goroutine ends too.
func TestOutputEndsStuckChannelOpen(t *testing.T) {
	lowerGrace(t)
	srv := testutil.StartSSHServer(t, "alice", "alice-pass", t.TempDir())
	hold := make(chan struct{})
	defer close(hold)
	srv.HandleChannel(func(nc ssh.NewChannel) bool {
		if nc.ChannelType() != "session" {
			return false
		}
		go func() { <-hold; nc.Reject(ssh.ConnectionFailed, "gone") }()
		return true
	})
	c := dialTest(t, srv)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	returnsWithin(t, 3*time.Second, func() {
		if _, err := Output(ctx, c, "ss", 1<<20); err == nil {
			t.Error("stuck command succeeded")
		}
	})
	waitClosed(t, c)
}

// freezer relays a TCP connection and can stop relaying in both directions,
// like a host that stops processing anything on the connection.
type freezer struct {
	ln     net.Listener
	frozen atomic.Bool
}

func newFreezer(t *testing.T, target string) *freezer {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &freezer{ln: ln}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			in, err := ln.Accept()
			if err != nil {
				return
			}
			out, err := net.Dial("tcp", target)
			if err != nil {
				in.Close()
				return
			}
			t.Cleanup(func() { in.Close(); out.Close() })
			go f.relay(in, out)
			go f.relay(out, in)
		}
	}()
	return f
}

func (f *freezer) relay(dst, src net.Conn) {
	buf := make([]byte, 32<<10)
	for {
		n, err := src.Read(buf)
		for f.frozen.Load() {
			time.Sleep(10 * time.Millisecond)
		}
		if n > 0 {
			if _, err := dst.Write(buf[:n]); err != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

// Closing a forwarded channel whose close the host never answers: the read
// blocked on it would wait forever; after the grace period the SSH connection
// is closed, the read returns and the channel counts as finished.
func TestStuckChannelCloseClosesTheConnection(t *testing.T) {
	lowerGrace(t)
	quiet, err := net.Listen("tcp", "127.0.0.1:0") // accepts, never says anything
	if err != nil {
		t.Fatal(err)
	}
	defer quiet.Close()
	go func() {
		for {
			c, err := quiet.Accept()
			if err != nil {
				return
			}
			defer c.Close()
		}
	}()
	srv := testutil.StartSSHServer(t, "alice", "alice-pass", t.TempDir())
	fr := newFreezer(t, srv.Addr.String())
	c, err := ssh.Dial("tcp", fr.ln.Addr().String(), &ssh.ClientConfig{User: "alice",
		Auth: []ssh.AuthMethod{ssh.Password("alice-pass")}, HostKeyCallback: ssh.InsecureIgnoreHostKey(), Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	var ended sync.WaitGroup
	ended.Add(1)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := DialVia(ctx, c, "tcp", quiet.Addr().String(), ended.Done)
	if err != nil {
		t.Fatal(err)
	}
	read := make(chan struct{})
	go func() { io.Copy(io.Discard, conn); close(read) }()
	time.Sleep(50 * time.Millisecond)
	fr.frozen.Store(true)
	conn.Close()
	select {
	case <-read:
	case <-time.After(3 * time.Second):
		t.Fatal("read on a closed channel stayed blocked")
	}
	returnsWithin(t, 3*time.Second, ended.Wait)
	waitClosed(t, c)
	fr.frozen.Store(false)
}
