package sshx

import (
	"context"
	"errors"
	"io"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/ihainan/corral/internal/testutil"
)

func setGrace(t *testing.T, d time.Duration) {
	old := StuckGrace
	StuckGrace = d
	t.Cleanup(func() { StuckGrace = old })
}

func setMaxAux(t *testing.T, n int) {
	old := MaxAuxPerConn
	MaxAuxPerConn = n
	t.Cleanup(func() { MaxAuxPerConn = old })
}

func isClosed(c *ssh.Client, within time.Duration) bool {
	closed := make(chan struct{})
	go func() { c.Wait(); close(closed) }()
	select {
	case <-closed:
		return true
	case <-time.After(within):
		return false
	}
}

func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for deadline := time.Now().Add(3 * time.Second); !ok(); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal(what)
		}
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

// A host that never answers the session open. Output returns at its
// deadline, but the connection (which may carry a terminal) stays up, and the
// operation keeps its place until the host lets go: past MaxAuxPerConn,
// further side operations are refused.
func TestStuckSideOperationsAreCappedNotCutOff(t *testing.T) {
	setMaxAux(t, 2)
	srv := testutil.StartSSHServer(t, "alice", "alice-pass", t.TempDir())
	hold := make(chan struct{})
	srv.HandleChannel(func(nc ssh.NewChannel) bool {
		if nc.ChannelType() != "session" {
			return false
		}
		go func() { <-hold; nc.Reject(ssh.ConnectionFailed, "gone") }()
		return true
	})
	c := dialTest(t, srv)
	for range 2 {
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		returnsWithin(t, 3*time.Second, func() {
			if _, err := Output(ctx, c, "ss", 1<<20); !errors.Is(err, context.DeadlineExceeded) {
				t.Errorf("stuck command: %v", err)
			}
		})
		cancel()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if _, err := Output(ctx, c, "ss", 1<<20); !errors.Is(err, ErrBusy) {
		t.Fatalf("third side operation with two stuck: %v", err)
	}
	if _, err := DialVia(ctx, c, "unix", "/run/x.sock", DialOptions{Aux: true}); !errors.Is(err, ErrBusy) {
		t.Fatalf("socket dial with two stuck: %v", err)
	}
	if isClosed(c, 300*time.Millisecond) {
		t.Fatal("a stuck side operation closed the connection")
	}
	if _, _, err := c.SendRequest("keepalive@openssh.com", true, nil); err != nil {
		t.Fatalf("connection no longer usable: %v", err)
	}
	close(hold) // the host finally answers: the places come back
	eventually(t, "side operations kept their places after ending", func() bool { return auxInUse(c) == 0 })
}

// A cancelled context is refused before any channel is opened.
func TestNoChannelIsOpenedForACancelledContext(t *testing.T) {
	srv := testutil.StartSSHServer(t, "alice", "alice-pass", t.TempDir())
	var opens atomic.Int32
	srv.HandleChannel(func(nc ssh.NewChannel) bool { opens.Add(1); return false })
	c := dialTest(t, srv)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var released atomic.Int32
	if _, err := DialVia(ctx, c, "tcp", "127.0.0.1:1", DialOptions{Release: func() { released.Add(1) }}); err == nil {
		t.Fatal("dial with a cancelled context succeeded")
	}
	if _, err := Output(ctx, c, "ss", 1<<20); err == nil {
		t.Fatal("command with a cancelled context ran")
	}
	time.Sleep(50 * time.Millisecond)
	if opens.Load() != 0 || released.Load() != 1 || auxInUse(c) != 0 {
		t.Fatalf("%d channels opened, quota released %d times", opens.Load(), released.Load())
	}
}

// A channel that opens only after the caller gave up is closed in the
// background, and its quota is released exactly once, when it has ended.
func TestChannelOpenedAfterCancelReleasesOnce(t *testing.T) {
	srv := testutil.StartSSHServer(t, "alice", "alice-pass", t.TempDir())
	answer := make(chan struct{})
	srv.HandleChannel(func(nc ssh.NewChannel) bool {
		if nc.ChannelType() != "direct-tcpip" {
			return false
		}
		go func() {
			<-answer
			ch, reqs, err := nc.Accept()
			if err != nil {
				return
			}
			go ssh.DiscardRequests(reqs)
			io.Copy(io.Discard, ch) // until our close arrives
			ch.Close()
		}()
		return true
	})
	c := dialTest(t, srv)
	var quota atomic.Int32
	quota.Store(1) // taken by the caller, handed to DialVia
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := DialVia(ctx, c, "tcp", "127.0.0.1:1", DialOptions{Release: func() { quota.Add(-1) }}); err == nil {
		t.Fatal("dial past its deadline succeeded")
	}
	if quota.Load() != 1 {
		t.Fatal("quota released while the open was still pending")
	}
	close(answer) // now it opens
	eventually(t, "quota of the late channel never released", func() bool { return quota.Load() <= 0 })
	time.Sleep(200 * time.Millisecond)
	if q := quota.Load(); q != 0 {
		t.Fatalf("quota released %d times", 1-q)
	}
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
			go f.relay(in, out)
			go f.relay(out, in)
		}
	}()
	return f
}

func (f *freezer) relay(dst, src net.Conn) {
	defer dst.Close()
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

// stuckChannel opens a channel through a relay, starts a reader on it, then
// freezes the relay (the host stops answering, its close included) and
// closes the channel.
func stuckChannel(t *testing.T, dedicated bool) (c *ssh.Client, fr *freezer, quota *atomic.Int32, readDone chan struct{}) {
	quiet, err := net.Listen("tcp", "127.0.0.1:0") // accepts, never says anything
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { quiet.Close() })
	go func() {
		for {
			c, err := quiet.Accept()
			if err != nil {
				return
			}
			t.Cleanup(func() { c.Close() })
		}
	}()
	srv := testutil.StartSSHServer(t, "alice", "alice-pass", t.TempDir())
	fr = newFreezer(t, srv.Addr.String())
	c, err = ssh.Dial("tcp", fr.ln.Addr().String(), &ssh.ClientConfig{User: "alice",
		Auth: []ssh.AuthMethod{ssh.Password("alice-pass")}, HostKeyCallback: ssh.InsecureIgnoreHostKey(), Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	quota = &atomic.Int32{}
	quota.Store(1)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := DialVia(ctx, c, "tcp", quiet.Addr().String(), DialOptions{Release: func() { quota.Add(-1) }, Dedicated: dedicated})
	if err != nil {
		t.Fatal(err)
	}
	readDone = make(chan struct{})
	go func() { io.Copy(io.Discard, conn); close(readDone) }()
	time.Sleep(50 * time.Millisecond)
	fr.frozen.Store(true)
	conn.Close()
	return c, fr, quota, readDone
}

// On a connection dedicated to forwarding, a channel whose close is never
// answered ends the connection after the grace period.
func TestStuckChannelOnDedicatedConnectionClosesIt(t *testing.T) {
	setGrace(t, 150*time.Millisecond)
	c, _, quota, readDone := stuckChannel(t, true)
	returnsWithin(t, time.Second, func() { <-readDone })
	if !isClosed(c, 3*time.Second) {
		t.Fatal("dedicated connection with a stuck channel left open")
	}
	eventually(t, "quota not released after the connection ended", func() bool { return quota.Load() == 0 })
}

// On a connection a terminal may depend on, the reader is released at once
// but the connection stays up; the quota is held until the host answers.
func TestStuckChannelOnSharedConnectionKeepsIt(t *testing.T) {
	setGrace(t, 150*time.Millisecond)
	c, fr, quota, readDone := stuckChannel(t, false)
	returnsWithin(t, time.Second, func() { <-readDone })
	if isClosed(c, 500*time.Millisecond) {
		t.Fatal("a stuck side channel closed a shared connection")
	}
	if quota.Load() != 1 {
		t.Fatal("quota released before the channel ended")
	}
	fr.frozen.Store(false) // the host catches up and answers the close
	eventually(t, "quota not released once the channel ended", func() bool { return quota.Load() == 0 })
	if quota.Load() != 0 {
		t.Fatal("quota released twice")
	}
}
