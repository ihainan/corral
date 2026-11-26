package sshx

import (
	"context"
	"errors"
	"io"
	"net"
	"sync"
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

func setMaxWatch(t *testing.T, n int) {
	old := MaxWatchPerConn
	MaxWatchPerConn = n
	t.Cleanup(func() { MaxWatchPerConn = old })
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
	frozen atomic.Bool // both directions
	up     atomic.Bool // client to host only: the host never hears from us
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
			go f.relay(out, in, &f.up)
			go f.relay(in, out, nil)
		}
	}()
	return f
}

func (f *freezer) relay(dst, src net.Conn, dir *atomic.Bool) {
	defer dst.Close()
	buf := make([]byte, 32<<10)
	for {
		n, err := src.Read(buf)
		for f.frozen.Load() || (dir != nil && dir.Load()) {
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

func dialVia(t *testing.T, fr *freezer) *ssh.Client {
	c, err := ssh.Dial("tcp", fr.ln.Addr().String(), &ssh.ClientConfig{User: "alice",
		Auth: []ssh.AuthMethod{ssh.Password("alice-pass")}, HostKeyCallback: ssh.InsecureIgnoreHostKey(), Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

// A host that sends EOF but never closes the channel: the channel is not
// over, so its quota is held until the close really happens.
func TestEOFIsNotTheEndOfAChannel(t *testing.T) {
	srv := testutil.StartSSHServer(t, "alice", "alice-pass", t.TempDir())
	srv.HandleChannel(func(nc ssh.NewChannel) bool {
		if nc.ChannelType() != "direct-tcpip" {
			return false
		}
		ch, reqs, err := nc.Accept()
		if err == nil {
			go ssh.DiscardRequests(reqs)
			ch.CloseWrite() // EOF, and nothing else
			go io.Copy(io.Discard, ch)
		}
		return true
	})
	fr := newFreezer(t, srv.Addr.String())
	c := dialVia(t, fr)
	var quota atomic.Int32
	quota.Store(1)
	conn, err := DialVia(context.Background(), c, "tcp", "127.0.0.1:1", DialOptions{Release: func() { quota.Add(-1) }})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(io.Discard, conn); err != nil { // reads EOF
		t.Fatal(err)
	}
	fr.up.Store(true) // our close will not reach the host
	conn.Close()
	time.Sleep(300 * time.Millisecond)
	if quota.Load() != 1 {
		t.Fatal("quota released on EOF, before the channel was closed")
	}
	fr.up.Store(false)
	eventually(t, "quota not released once the channel closed", func() bool { return quota.Load() == 0 })
}

// A refused exec leaves the session channel open until the host closes it:
// the side operation keeps its place until then.
func TestRefusedExecHoldsItsPlaceUntilTheChannelEnds(t *testing.T) {
	srv := testutil.StartSSHServer(t, "alice", "alice-pass", t.TempDir())
	var fr *freezer
	srv.HandleChannel(func(nc ssh.NewChannel) bool {
		if nc.ChannelType() != "session" {
			return false
		}
		ch, reqs, err := nc.Accept()
		if err != nil {
			return true
		}
		go func() {
			for r := range reqs {
				if r.Type == "exec" {
					fr.up.Store(true) // from now on the host hears nothing from us
					r.Reply(false, nil)
					ch.CloseWrite()
				} else if r.WantReply {
					r.Reply(false, nil)
				}
			}
		}()
		return true
	})
	fr = newFreezer(t, srv.Addr.String())
	c := dialVia(t, fr)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	returnsWithin(t, 3*time.Second, func() {
		if _, err := Output(ctx, c, "ss", 1<<20); err == nil {
			t.Error("refused command succeeded")
		}
	})
	if auxInUse(c) != 1 {
		t.Fatal("refused command gave up its place while the channel was still open")
	}
	fr.up.Store(false)
	eventually(t, "place not given back once the channel closed", func() bool { return auxInUse(c) == 0 })
}

// Aux and Watch quotas are counted separately: filling Aux does not block Watch,
// filling Watch (2 unfinished) rejects a 3rd Watch without affecting Aux, and
// Aux+Watch together are rejected as mutually exclusive.
func TestWatchAndAuxQuotasAreIndependent(t *testing.T) {
	if MaxWatchPerConn != 2 {
		t.Fatalf("MaxWatchPerConn = %d, want 2", MaxWatchPerConn)
	}
	setMaxAux(t, 2)
	setMaxWatch(t, 2)

	srv := testutil.StartSSHServer(t, "alice", "alice-pass", t.TempDir())
	srv.HandleChannel(func(nc ssh.NewChannel) bool {
		if nc.ChannelType() != "direct-streamlocal@openssh.com" {
			return false
		}
		ch, reqs, err := nc.Accept()
		if err != nil {
			return true
		}
		go ssh.DiscardRequests(reqs)
		go func() {
			defer ch.Close()
			io.Copy(io.Discard, ch)
		}()
		return true
	})

	c := dialTest(t, srv)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Fill all Aux slots.
	aux1, err := DialVia(ctx, c, "unix", "/run/aux1.sock", DialOptions{Aux: true})
	if err != nil {
		t.Fatalf("aux1: %v", err)
	}
	aux2, err := DialVia(ctx, c, "unix", "/run/aux2.sock", DialOptions{Aux: true})
	if err != nil {
		t.Fatalf("aux2: %v", err)
	}
	if _, err := DialVia(ctx, c, "unix", "/run/aux3.sock", DialOptions{Aux: true}); !errors.Is(err, ErrBusy) {
		t.Fatalf("3rd aux with Aux full: got %v, want ErrBusy", err)
	}

	// Even with Aux completely full, both Watch slots are available.
	w1, err := DialVia(ctx, c, "unix", "/run/watch1.sock", DialOptions{Watch: true})
	if err != nil {
		t.Fatalf("watch1 while Aux full: %v", err)
	}
	w2, err := DialVia(ctx, c, "unix", "/run/watch2.sock", DialOptions{Watch: true})
	if err != nil {
		t.Fatalf("watch2 while Aux full: %v", err)
	}
	if auxInUse(c) != 2 || watchInUse(c) != 2 {
		t.Fatalf("inUse aux=%d watch=%d, want 2, 2", auxInUse(c), watchInUse(c))
	}
	if _, err := DialVia(ctx, c, "unix", "/run/watch3.sock", DialOptions{Watch: true}); !errors.Is(err, ErrBusy) {
		t.Fatalf("3rd watch with Watch full: got %v, want ErrBusy", err)
	}

	// Release Aux channels while Watch stays full: Watch remains capped at 2, Aux is unaffected.
	aux1.Close()
	aux2.Close()
	eventually(t, "aux slots not released", func() bool { return auxInUse(c) == 0 })

	if _, err := DialVia(ctx, c, "unix", "/run/watch3.sock", DialOptions{Watch: true}); !errors.Is(err, ErrBusy) {
		t.Fatalf("3rd watch still full: got %v, want ErrBusy", err)
	}
	aux3, err := DialVia(ctx, c, "unix", "/run/aux3.sock", DialOptions{Aux: true})
	if err != nil {
		t.Fatalf("aux3 while Watch full: %v", err)
	}
	aux4, err := DialVia(ctx, c, "unix", "/run/aux4.sock", DialOptions{Aux: true})
	if err != nil {
		t.Fatalf("aux4 while Watch full: %v", err)
	}
	aux3.Close()
	aux4.Close()
	w1.Close()
	w2.Close()
	eventually(t, "quotas not returned to zero", func() bool { return auxInUse(c) == 0 && watchInUse(c) == 0 })

	// Aux and Watch are mutually exclusive; Release is still called once.
	var released atomic.Int32
	if _, err := DialVia(ctx, c, "unix", "/run/both.sock", DialOptions{
		Aux:     true,
		Watch:   true,
		Release: func() { released.Add(1) },
	}); err == nil {
		t.Fatal("DialOptions{Aux: true, Watch: true} succeeded")
	}
	if released.Load() != 1 || auxInUse(c) != 0 || watchInUse(c) != 0 {
		t.Fatalf("after invalid options: released=%d aux=%d watch=%d", released.Load(), auxInUse(c), watchInUse(c))
	}
}

// A Watch channel holds its slot until the SSH channel really ends (not on EOF
// or local Close alone), matching Aux accounting.
func TestWatchChannelHoldsQuotaUntilRealEnd(t *testing.T) {
	srv := testutil.StartSSHServer(t, "alice", "alice-pass", t.TempDir())
	srv.HandleChannel(func(nc ssh.NewChannel) bool {
		if nc.ChannelType() != "direct-streamlocal@openssh.com" {
			return false
		}
		ch, reqs, err := nc.Accept()
		if err == nil {
			go ssh.DiscardRequests(reqs)
			ch.CloseWrite() // EOF without closing the channel
			go io.Copy(io.Discard, ch)
		}
		return true
	})
	fr := newFreezer(t, srv.Addr.String())
	c := dialVia(t, fr)
	conn, err := DialVia(context.Background(), c, "unix", "/run/watch.sock", DialOptions{Watch: true})
	if err != nil {
		t.Fatal(err)
	}
	if watchInUse(c) != 1 || auxInUse(c) != 0 {
		t.Fatalf("inUse watch=%d aux=%d, want 1, 0", watchInUse(c), auxInUse(c))
	}
	if _, err := io.Copy(io.Discard, conn); err != nil {
		t.Fatal(err)
	}
	fr.up.Store(true) // prevent our close from reaching the host
	conn.Close()
	time.Sleep(250 * time.Millisecond)
	if watchInUse(c) != 1 {
		t.Fatalf("watch slot released before channel really ended: %d", watchInUse(c))
	}
	fr.up.Store(false)
	eventually(t, "watch slot not released after channel ended", func() bool { return watchInUse(c) == 0 })
}

// Concurrent acquire/release across Aux and Watch stays within bounds and
// leaves zero slots held under -race.
func TestWatchAndAuxConcurrentAcquireRelease(t *testing.T) {
	c := &ssh.Client{}
	var wg sync.WaitGroup
	var errCount atomic.Int32

	runPool := func(take func(*ssh.Client) (func(), bool), inUse func(*ssh.Client) int, limit int) {
		defer wg.Done()
		for range 200 {
			rel, ok := take(c)
			if !ok {
				continue
			}
			if n := inUse(c); n < 1 || n > limit {
				errCount.Add(1)
			}
			rel()
			rel() // oneShot must ignore duplicate release
		}
	}

	for range 8 {
		wg.Add(2)
		go runPool(takeAux, auxInUse, MaxAuxPerConn)
		go runPool(takeWatch, watchInUse, MaxWatchPerConn)
	}
	wg.Wait()

	if errCount.Load() != 0 {
		t.Fatalf("quota invariant violated %d times", errCount.Load())
	}
	if auxInUse(c) != 0 || watchInUse(c) != 0 {
		t.Fatalf("slots leaked: aux=%d watch=%d", auxInUse(c), watchInUse(c))
	}
}
