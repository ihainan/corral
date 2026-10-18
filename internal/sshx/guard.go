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
	"fmt"
	"io"
	"net"
	"strconv"
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
	return openGuarded(ctx, c, network, addr, opt, func() (ssh.Channel, <-chan *ssh.Request, error) {
		return openForwarded(c, network, addr)
	})
}

// openGuarded opens a channel with open and wraps it (see DialVia). An open
// that fails after the channel exists hands back the channel too, so it is
// closed and accounted for like any other.
func openGuarded(ctx context.Context, c *ssh.Client, network, addr string, opt DialOptions,
	open func() (ssh.Channel, <-chan *ssh.Request, error)) (net.Conn, error) {
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
		ch   ssh.Channel
		reqs <-chan *ssh.Request
		err  error
	}
	res := make(chan result, 1)
	go func() {
		ch, reqs, err := open()
		res <- result{ch, reqs, err}
	}()
	// adopt hands the quota to the channel, or gives it back if none opened.
	adopt := func(r result) *guardedConn {
		if r.ch == nil {
			release()
			return nil
		}
		g := newGuardedConn(r.ch, r.reqs, network, addr, c, release, opt.Dedicated, grace)
		if r.err != nil {
			g.Close()
			return nil
		}
		return g
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

// openForwarded opens a "direct-tcpip" or "direct-streamlocal" channel as
// ssh.Client.Dial does, but keeps the channel's request stream: it is closed
// when the channel has really ended, which is what quotas wait for.
func openForwarded(c *ssh.Client, network, addr string) (ssh.Channel, <-chan *ssh.Request, error) {
	switch network {
	case "tcp":
		host, p, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, nil, err
		}
		port, err := strconv.ParseUint(p, 10, 16)
		if err != nil {
			return nil, nil, err
		}
		return c.OpenChannel("direct-tcpip", ssh.Marshal(&struct {
			Host       string
			Port       uint32
			OriginAddr string
			OriginPort uint32
		}{host, uint32(port), "0.0.0.0", 0}))
	case "unix":
		return c.OpenChannel("direct-streamlocal@openssh.com", ssh.Marshal(&struct {
			SocketPath string
			Reserved0  string
			Reserved1  uint32
		}{addr, "", 0}))
	}
	return nil, nil, fmt.Errorf("sshx: cannot forward network %q", network)
}

// guardedConn decouples callers from the channel: pumps move the data, so
// Close can make reads and writes return at once. The quota is released only
// once the channel has really ended: its request stream is closed (the host
// closed the channel, or the connection ended; data EOF alone is not the
// end) and every goroutine working on it has returned.
type guardedConn struct {
	ch        ssh.Channel
	client    *ssh.Client
	network   string
	addr      string
	dedicated bool
	grace     time.Duration
	release   func()

	rd     *io.PipeReader // callers read here; the read pump fills it
	rdFeed *io.PipeWriter
	wr     *io.PipeWriter // callers write here; the write pump drains it
	wrSrc  *io.PipeReader

	mu       sync.Mutex
	running  int // goroutines working on the channel
	released bool

	closeOnce sync.Once
	ended     chan struct{}
}

func newGuardedConn(ch ssh.Channel, reqs <-chan *ssh.Request, network, addr string, client *ssh.Client, release func(),
	dedicated bool, grace time.Duration) *guardedConn {
	g := &guardedConn{ch: ch, client: client, network: network, addr: addr, dedicated: dedicated, grace: grace,
		release: release, running: 4, ended: make(chan struct{})}
	g.rd, g.rdFeed = io.Pipe()
	g.wrSrc, g.wr = io.Pipe()
	go func() { // extended data (stderr), discarded; it ends with the channel
		defer g.done()
		io.Copy(io.Discard, ch.Stderr())
	}()
	go func() { // the channel's requests: the stream closes when the channel has ended
		defer g.done()
		for r := range reqs {
			if r.WantReply {
				r.Reply(false, nil)
			}
		}
		// The channel is gone (closed by the host, or the connection ended):
		// nothing more can be written, so the write pump stops now instead
		// of waiting for a caller that may never close (a proxy holding a
		// half-closed WebSocket, say).
		g.wrSrc.CloseWithError(net.ErrClosed)
	}()
	go func() { // read pump: until EOF or the channel ends, also after Close
		defer g.done()
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
		defer g.done()
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
	return g
}

func (g *guardedConn) start() {
	g.mu.Lock()
	g.running++
	g.mu.Unlock()
}

// done ends one goroutine; the last one releases the quota, once.
func (g *guardedConn) done() {
	g.mu.Lock()
	g.running--
	end := g.running == 0 && !g.released
	if end {
		g.released = true
	}
	g.mu.Unlock()
	if end {
		g.release()
		close(g.ended)
	}
}

func (g *guardedConn) Read(p []byte) (int, error)  { return g.rd.Read(p) }
func (g *guardedConn) Write(p []byte) (int, error) { return g.wr.Write(p) }

// Close returns at once; the channel's end is awaited in the background.
func (g *guardedConn) Close() error {
	g.closeOnce.Do(func() {
		g.rd.CloseWithError(net.ErrClosed)
		g.wr.CloseWithError(net.ErrClosed)
		g.start()
		go func() { // sends the close; may wait on a stalled connection
			defer g.done()
			g.ch.Close()
		}()
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

type chanAddr struct{ network, addr string }

func (a chanAddr) Network() string { return a.network }
func (a chanAddr) String() string  { return a.addr }

var errNoDeadline = errors.New("sshx: channels do not support deadlines; use Close")

func (g *guardedConn) LocalAddr() net.Addr              { return chanAddr{g.network, "ssh"} }
func (g *guardedConn) RemoteAddr() net.Addr             { return chanAddr{g.network, g.addr} }
func (g *guardedConn) SetDeadline(time.Time) error      { return errNoDeadline }
func (g *guardedConn) SetReadDeadline(time.Time) error  { return errNoDeadline }
func (g *guardedConn) SetWriteDeadline(time.Time) error { return errNoDeadline }
