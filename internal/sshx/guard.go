package sshx

import (
	"context"
	"net"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
)

// StuckGrace is how long an SSH operation may outlive its deadline (or the
// close of its channel) before the whole connection is closed to end it.
//
// A host that answers keepalives but never confirms a channel open, an exec,
// or a channel close would otherwise hold the goroutines, and the limits they
// count against, for good: cancelling a context only stops the caller from
// waiting, and closing a channel only sends a request. Closing the connection
// fails every read and write blocked on it at once.
var StuckGrace = 5 * time.Second

// watch closes c if the operation is still running StuckGrace after ctx ends;
// soft, if set, runs first (in the background, as it may block too). Call
// the returned func once the operation is over.
func watch(ctx context.Context, c *ssh.Client, soft func()) (finished func()) {
	done := make(chan struct{})
	grace := StuckGrace
	stop := context.AfterFunc(ctx, func() {
		if soft != nil {
			go soft()
		}
		t := time.NewTimer(grace)
		defer t.Stop()
		select {
		case <-done:
		case <-t.C:
			c.Close()
		}
	})
	var once sync.Once
	return func() {
		once.Do(func() {
			close(done)
			stop()
		})
	}
}

// DialVia opens a forwarded channel ("tcp" or "unix") over c. It returns only
// once the open is really over: if ctx ends first, by StuckGrace later at the
// latest (closing c). ctx should carry a deadline. The connection's Close
// works the same way for reads and writes still blocked on it; done, if set,
// runs once the channel is finished with for good.
func DialVia(ctx context.Context, c *ssh.Client, network, addr string, done func()) (net.Conn, error) {
	finished := watch(ctx, c, nil)
	conn, err := c.Dial(network, addr)
	finished()
	if err != nil {
		return nil, err
	}
	g := &guardedConn{Conn: conn, client: c, done: done}
	if ctx.Err() != nil {
		g.Close()
		return nil, ctx.Err()
	}
	return g, nil
}

// guardedConn is a channel whose Close cannot leave I/O blocked forever.
type guardedConn struct {
	net.Conn
	client *ssh.Client
	done   func()

	mu     sync.Mutex
	busy   int // reads, writes and the close itself in progress
	closed bool
	ended  bool
	timer  *time.Timer
}

func (g *guardedConn) enter() {
	g.mu.Lock()
	g.busy++
	g.mu.Unlock()
}

func (g *guardedConn) leave() {
	g.mu.Lock()
	g.busy--
	end := g.closed && g.busy == 0 && !g.ended
	if end {
		g.ended = true
		if g.timer != nil {
			g.timer.Stop()
		}
	}
	g.mu.Unlock()
	if end && g.done != nil {
		g.done()
	}
}

func (g *guardedConn) Read(p []byte) (int, error) {
	g.enter()
	defer g.leave()
	return g.Conn.Read(p)
}

func (g *guardedConn) Write(p []byte) (int, error) {
	g.enter()
	defer g.leave()
	return g.Conn.Write(p)
}

// Close returns at once. The channel close runs in the background; if it, or
// any read or write, is still blocked after StuckGrace, the SSH connection is
// closed.
func (g *guardedConn) Close() error {
	g.mu.Lock()
	if g.closed {
		g.mu.Unlock()
		return nil
	}
	g.closed = true
	g.busy++ // the close itself
	g.timer = time.AfterFunc(StuckGrace, func() {
		g.mu.Lock()
		stuck := !g.ended
		g.mu.Unlock()
		if stuck {
			g.client.Close()
		}
	})
	g.mu.Unlock()
	go func() {
		g.Conn.Close()
		g.leave()
	}()
	return nil
}
