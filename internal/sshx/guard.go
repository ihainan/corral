package sshx

// Channels whose end is really the end.
//
// An SSH channel is not over when the caller gives up on it: cancelling a
// context only stops the caller from waiting, and closing a channel only
// sends a request that the host may never answer (while it still answers
// keepalives). Until the host closes the channel, or the connection ends,
// the library keeps goroutines and state for it. Quotas for channels are
// therefore held until that real end, by exactly one owner, and released
// exactly once.
//
// Side operations next to a terminal (herdr, port listing) share the
// terminal's connection, so a host that leaves them hanging must not cost
// the terminal: they are never cut off by closing the connection. Instead
// at most MaxAuxPerConn may be unfinished per connection; more are refused
// (ErrBusy) until some end. Only a connection dedicated to such traffic (the
// port-forwarding pool) is closed when a channel will not end.

import (
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
)

// StuckGrace is how long a channel on a dedicated connection may take to
// end after it was given up before the connection is closed.
var StuckGrace = 5 * time.Second

// MaxAuxPerConn bounds unfinished side operations per SSH connection.
var MaxAuxPerConn = 4

var ErrBusy = errors.New("too many operations are still running on this SSH connection")

var (
	auxMu  sync.Mutex
	auxOps = map[*ssh.Client]int{}
)

// takeAux reserves one side operation on c; release is safe to call twice.
func takeAux(c *ssh.Client) (release func(), ok bool) {
	auxMu.Lock()
	defer auxMu.Unlock()
	if auxOps[c] >= MaxAuxPerConn {
		return nil, false
	}
	auxOps[c]++
	return oneShot(func() {
		auxMu.Lock()
		defer auxMu.Unlock()
		if auxOps[c]--; auxOps[c] <= 0 {
			delete(auxOps, c)
		}
	}), true
}

// auxInUse is for tests.
func auxInUse(c *ssh.Client) int {
	auxMu.Lock()
	defer auxMu.Unlock()
	return auxOps[c]
}

// oneShot makes a release function that only acts the first time.
func oneShot(f func()) func() {
	var once sync.Once
	return func() {
		if f != nil {
			once.Do(f)
		}
	}
}

// DialOptions describe who pays for a channel and what may be done when it
// will not end.
type DialOptions struct {
	// Aux marks a side operation next to a terminal: it counts against
	// MaxAuxPerConn until the channel really ends.
	Aux bool
	// Release gives back a quota the caller took for this channel. DialVia
	// owns it from the call on, on every path, and calls it once when the
	// channel is really over (or was never opened).
	Release func()
	// Dedicated means no terminal depends on the connection: if the channel
	// will not end within StuckGrace of being given up, the connection is
	// closed.
	Dedicated bool
}

// DialVia opens a forwarded channel ("tcp" or "unix") over c. It returns when
// the channel is open or ctx ends, whichever is first; a channel that opens
// after the caller gave up is closed in the background, and its quota is
// held until it has really ended. The returned connection's reads and writes
// return at once after Close, whatever the host does.
func DialVia(ctx context.Context, c *ssh.Client, network, addr string, opt DialOptions) (net.Conn, error) {
	release := oneShot(opt.Release)
	if opt.Aux {
		aux, ok := takeAux(c)
		if !ok {
			release()
			return nil, ErrBusy
		}
		caller := release
		release = oneShot(func() { aux(); caller() })
	}
	if err := ctx.Err(); err != nil {
		release()
		return nil, err
	}
	grace := StuckGrace
	type result struct {
		conn net.Conn
		err  error
	}
	res := make(chan result, 1)
	go func() {
		conn, err := c.Dial(network, addr)
		res <- result{conn, err}
	}()
	// adopt hands the quota to the channel, or gives it back if none opened.
	adopt := func(r result) *guardedConn {
		if r.err != nil {
			release()
			return nil
		}
		return newGuardedConn(r.conn, c, release, opt.Dedicated, grace)
	}
	select {
	case r := <-res:
		g := adopt(r)
		if r.err != nil {
			return nil, r.err
		}
		if ctx.Err() != nil {
			g.Close()
			return nil, ctx.Err()
		}
		return g, nil
	case <-ctx.Done():
		go func() {
			var stuck <-chan time.Time
			if opt.Dedicated {
				t := time.NewTimer(grace)
				defer t.Stop()
				stuck = t.C
			}
			var r result
			select {
			case r = <-res:
			case <-stuck:
				c.Close() // the open was never answered; the connection is ours alone
				r = <-res
			}
			if g := adopt(r); g != nil {
				g.Close()
			}
		}()
		return nil, ctx.Err()
	}
}

// guardedConn decouples callers from the channel: two pumps move the data,
// so Close can make reads and writes return at once, while the pumps keep
// going until the channel has really ended (the host closed it or sent EOF,
// or the connection ended). Only then is the quota released.
type guardedConn struct {
	ch        net.Conn
	client    *ssh.Client
	dedicated bool
	grace     time.Duration

	rd     *io.PipeReader // callers read here; the read pump fills it
	rdFeed *io.PipeWriter
	wr     *io.PipeWriter // callers write here; the write pump drains it
	wrSrc  *io.PipeReader

	closeOnce sync.Once
	ended     chan struct{}
}

func newGuardedConn(ch net.Conn, client *ssh.Client, release func(), dedicated bool, grace time.Duration) *guardedConn {
	g := &guardedConn{ch: ch, client: client, dedicated: dedicated, grace: grace, ended: make(chan struct{})}
	g.rd, g.rdFeed = io.Pipe()
	g.wrSrc, g.wr = io.Pipe()
	var pumps sync.WaitGroup
	pumps.Add(2)
	go func() { // read pump: until EOF or the connection ends, also after Close
		defer pumps.Done()
		buf := make([]byte, 32<<10)
		for {
			n, err := ch.Read(buf)
			if n > 0 {
				_, _ = g.rdFeed.Write(buf[:n]) // after Close this fails at once: the data is discarded
			}
			if err != nil {
				g.rdFeed.CloseWithError(err)
				return
			}
		}
	}()
	go func() { // write pump: until Close or a failed write
		defer pumps.Done()
		buf := make([]byte, 32<<10)
		for {
			n, err := g.wrSrc.Read(buf)
			if n > 0 {
				if _, werr := ch.Write(buf[:n]); werr != nil {
					g.wrSrc.CloseWithError(werr)
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()
	go func() {
		pumps.Wait()
		release()
		close(g.ended)
	}()
	return g
}

func (g *guardedConn) Read(p []byte) (int, error)  { return g.rd.Read(p) }
func (g *guardedConn) Write(p []byte) (int, error) { return g.wr.Write(p) }

// Close returns at once; the channel's end is awaited in the background.
func (g *guardedConn) Close() error {
	g.closeOnce.Do(func() {
		g.rd.CloseWithError(net.ErrClosed)
		g.wr.CloseWithError(net.ErrClosed)
		go g.ch.Close() // sends the close; may wait on a stalled connection
		if g.dedicated {
			t := time.AfterFunc(g.grace, func() {
				select {
				case <-g.ended:
				default:
					g.client.Close()
				}
			})
			go func() { <-g.ended; t.Stop() }()
		}
	})
	return nil
}

func (g *guardedConn) LocalAddr() net.Addr                { return g.ch.LocalAddr() }
func (g *guardedConn) RemoteAddr() net.Addr               { return g.ch.RemoteAddr() }
func (g *guardedConn) SetDeadline(t time.Time) error      { return g.ch.SetDeadline(t) }
func (g *guardedConn) SetReadDeadline(t time.Time) error  { return g.ch.SetReadDeadline(t) }
func (g *guardedConn) SetWriteDeadline(t time.Time) error { return g.ch.SetWriteDeadline(t) }
