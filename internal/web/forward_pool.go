package web

// Forwarding state: tickets, subdomain grants, the SSH connections used for
// forwarding, connections being dialled, and every proxied request in flight.
//
// Revoking access (sign-out, password change, disabling a user, unsharing or
// deleting a forward, deleting a host) goes through revoke, which ends what is
// open as well as what comes next. Every proxied request registers itself
// before it re-checks its access in the database, and every dial registers
// itself before it reads the host: a revocation either commits before those
// reads (and they see it) or reaches the registration (and cancels it).

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/ihainan/corral/internal/sshx"
)

// Limits; variables so tests can lower them.
var (
	maxFwdConnsPerUser    = 8    // forwarding-only SSH connections (incl. dials) per host owner
	maxFwdConns           = 256  // and in total
	maxFwdDials           = 32   // connections being set up at once, in total
	maxFwdStreamsPerUser  = 64   // proxied requests and WebSockets in flight per user
	maxFwdStreams         = 1024 // and in total
	maxFwdChannels        = 512  // channels to forwarded ports (open, idle, opening or closing), in total
	maxFwdChannelsPerConn = 64   // and per SSH connection (a terminal's included)
	maxFwdTicketsPerUser  = 16   // unredeemed tickets per user; the oldest give way
	maxFwdGrantsPerUser   = 64   // subdomain cookies per user; the oldest give way

	fwdConnIdle       = 10 * time.Minute // forwarding-only connections without users are closed after this
	fwdReapEvery      = time.Minute
	fwdConnectTimeout = 10 * time.Second // opening a channel to a forwarded port
)

var (
	errFwdBusy    = errors.New("too many forwarded connections")
	errFwdRevoked = errors.New("forwarding access was revoked")
)

// fwdUse identifies who uses which forward; tickets, grants and requests in
// flight all carry one so a revocation can find them.
type fwdUse struct {
	userID          int64
	loginHash       string
	forwardID       string
	ownerID, hostID int64
}

type fwdGrant struct {
	fwdUse
	ret     string // tickets only: where to land on the subdomain, fixed when issued
	created time.Time
	expires time.Time
}

type forwarder struct {
	mu        sync.Mutex
	tickets   map[string]*fwdGrant     // one-time, keyed by ticket hash
	grants    map[string]*fwdGrant     // per subdomain cookie, keyed by token hash
	conns     map[[2]int64]*fwdConn    // opened for forwarding, by owner and host
	borrowed  map[*ssh.Client]*fwdConn // terminal sessions' connections in use for forwarding
	dials     map[[2]int64]*fwdDial    // connections being opened; requests for the same host wait for them
	streams   map[*fwdStream]struct{}
	perUser   map[int64]int       // streams per user
	channels  int                 // channels to forwarded ports, counted until each is really finished
	perClient map[*ssh.Client]int // the same per SSH connection; outlives transports dropped by the reaper
	closed    bool
	stop      chan struct{}
	wg        sync.WaitGroup
	closeOnce sync.Once
}

// fwdConn is an SSH connection with the HTTP transport that proxies over it.
type fwdConn struct {
	client    *ssh.Client
	transport *http.Transport
	owned     bool      // opened for forwarding (closed when idle), not a terminal's
	version   int64     // pooled: the host's connection version it was made with
	codeSock  string    // the owner's code-server socket on the host (codeUpstream), under forwarder.mu
	users     int       // requests and port listings using it now
	idleSince time.Time // when users last dropped to zero
}

type fwdDial struct {
	ownerID, hostID int64
	cancel          context.CancelFunc
	revoked         bool
	done            chan struct{} // closed when the dial is over
	err             error         // its failure, set before done is closed
}

type fwdStream struct {
	fwdUse
	cancel context.CancelFunc
}

func newForwarder() *forwarder {
	return &forwarder{tickets: map[string]*fwdGrant{}, grants: map[string]*fwdGrant{}, conns: map[[2]int64]*fwdConn{},
		borrowed: map[*ssh.Client]*fwdConn{}, dials: map[[2]int64]*fwdDial{}, streams: map[*fwdStream]struct{}{},
		perUser: map[int64]int{}, perClient: map[*ssh.Client]int{}, stop: make(chan struct{})}
}

// newFwdConn wraps an SSH connection with a transport that proxies over it,
// to 127.0.0.1 on the host only. Every channel it opens counts against
// maxFwdChannels and maxFwdChannelsPerConn from the start of the open until
// the channel has really ended (sshx.DialVia owns the release), so requests
// that gave up cannot leave uncounted channels behind. Only on connections
// opened for forwarding may a channel that will not end close the connection.
func (f *forwarder) newFwdConn(c *ssh.Client, owned bool) *fwdConn {
	fc := &fwdConn{client: c, owned: owned}
	fc.transport = &http.Transport{
		DialContext: func(ctx context.Context, _, addr string) (net.Conn, error) {
			network := "tcp"
			if addr == codeUpstream {
				network, addr = "unix", f.codeSocket(fc)
				if addr == "" {
					return nil, errors.New("code-server is not running")
				}
			} else if h, _, err := net.SplitHostPort(addr); err != nil || h != "127.0.0.1" {
				return nil, fmt.Errorf("forwarding only reaches 127.0.0.1, not %q", addr)
			}
			release, ok := f.takeChannel(c)
			if !ok {
				return nil, errFwdBusy
			}
			ctx, cancel := context.WithTimeout(ctx, fwdConnectTimeout)
			defer cancel()
			return sshx.DialVia(ctx, c, network, addr, sshx.DialOptions{Release: release, Dedicated: owned})
		},
		MaxIdleConns:          16,
		MaxIdleConnsPerHost:   4,
		IdleConnTimeout:       30 * time.Second,
		ResponseHeaderTimeout: 2 * time.Minute,
	}
	return fc
}

// codeUpstream is the request address that stands for the connection's
// code-server socket.
const codeUpstream = "code-server.corral:80"

func (f *forwarder) codeSocket(fc *fwdConn) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return fc.codeSock
}

// setCodeSocket points codeUpstream at a socket; keep-alive channels to an
// earlier one are dropped.
func (f *forwarder) setCodeSocket(fc *fwdConn, path string) {
	f.mu.Lock()
	changed := fc.codeSock != "" && fc.codeSock != path
	fc.codeSock = path
	f.mu.Unlock()
	if changed {
		fc.transport.CloseIdleConnections()
	}
}

// takeChannel reserves a channel on c; the release is the reservation's only
// owner and acts once. The count belongs to the SSH connection, not to the
// transport wrapping it, which the reaper may drop and recreate while
// channels it opened are still ending.
func (f *forwarder) takeChannel(c *ssh.Client) (release func(), ok bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed || f.channels >= maxFwdChannels || f.perClient[c] >= maxFwdChannelsPerConn {
		return nil, false
	}
	f.channels++
	f.perClient[c]++
	var once sync.Once
	return func() {
		once.Do(func() {
			f.mu.Lock()
			f.channels--
			if f.perClient[c]--; f.perClient[c] <= 0 {
				delete(f.perClient, c)
			}
			f.mu.Unlock()
		})
	}, true
}

// Tickets and grants -------------------------------------------------------

// sweep drops expired tickets and grants; called with mu held.
func (f *forwarder) sweep() {
	now := time.Now()
	for k, g := range f.tickets {
		if now.After(g.expires) {
			delete(f.tickets, k)
		}
	}
	for k, g := range f.grants {
		if now.After(g.expires) {
			delete(f.grants, k)
		}
	}
}

// keepAtMost makes room for one more entry of userID in m by dropping that
// user's oldest; called with mu held.
func keepAtMost(m map[string]*fwdGrant, userID int64, limit int) {
	limit = max(limit, 1)
	for {
		n, oldestKey := 0, ""
		var oldest time.Time
		for k, g := range m {
			if g.userID != userID {
				continue
			}
			n++
			if oldestKey == "" || g.created.Before(oldest) {
				oldestKey, oldest = k, g.created
			}
		}
		if n < limit {
			return
		}
		delete(m, oldestKey)
	}
}

func (f *forwarder) addTicket(hash string, g *fwdGrant) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sweep()
	g.created = time.Now()
	keepAtMost(f.tickets, g.userID, maxFwdTicketsPerUser)
	f.tickets[hash] = g
}

// takeTicket removes and returns a ticket (single use).
func (f *forwarder) takeTicket(hash string) *fwdGrant {
	f.mu.Lock()
	defer f.mu.Unlock()
	g := f.tickets[hash]
	delete(f.tickets, hash)
	return g
}

func (f *forwarder) addGrant(hash string, g *fwdGrant) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sweep()
	g.created = time.Now()
	keepAtMost(f.grants, g.userID, maxFwdGrantsPerUser)
	f.grants[hash] = g
}

// grant returns a copy of a live grant's details.
func (f *forwarder) grant(hash string) (fwdUse, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	g := f.grants[hash]
	if g == nil || time.Now().After(g.expires) {
		return fwdUse{}, false
	}
	return g.fwdUse, true
}

func (f *forwarder) dropGrant(hash string) {
	f.mu.Lock()
	delete(f.grants, hash)
	f.mu.Unlock()
}

// Requests in flight -------------------------------------------------------

// startStream registers a proxied request; cancel ends it on revocation.
func (f *forwarder) startStream(use fwdUse, cancel context.CancelFunc) (*fwdStream, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed || len(f.streams) >= maxFwdStreams || f.perUser[use.userID] >= maxFwdStreamsPerUser {
		return nil, false
	}
	st := &fwdStream{fwdUse: use, cancel: cancel}
	f.streams[st] = struct{}{}
	f.perUser[use.userID]++
	return st, true
}

// setForward records which owner and host a request reaches. It must happen
// before access is re-checked, so revoking the owner or host finds it.
func (f *forwarder) setForward(st *fwdStream, ownerID, hostID int64) {
	f.mu.Lock()
	st.ownerID, st.hostID = ownerID, hostID
	f.mu.Unlock()
}

func (f *forwarder) endStream(st *fwdStream) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.streams, st)
	if f.perUser[st.userID]--; f.perUser[st.userID] <= 0 {
		delete(f.perUser, st.userID)
	}
}

// Connections --------------------------------------------------------------

// borrow takes a reference on a terminal session's connection.
func (f *forwarder) borrow(c *ssh.Client) (*fwdConn, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return nil, errFwdRevoked
	}
	fc := f.borrowed[c]
	if fc == nil {
		fc = f.newFwdConn(c, false)
		f.borrowed[c] = fc
	}
	fc.users++
	return fc, nil
}

// acquire takes a reference on the open forwarding connection to key, or
// else claims the dial for it (lead) or returns the dial already under way to
// wait for. One step under the lock, so two requests cannot both find the
// pool empty and both dial.
// A pooled connection made with an older connection version is retired.
func (f *forwarder) acquire(key [2]int64, version int64, cancel context.CancelFunc) (fc *fwdConn, d *fwdDial, lead bool, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return nil, nil, false, errFwdRevoked
	}
	if fc := f.conns[key]; fc != nil {
		if fc.version >= version {
			fc.users++
			return fc, nil, false, nil
		}
		go f.detachConn(key, fc)() // closing may wait on the network: not under mu
	}
	if d := f.dials[key]; d != nil {
		return nil, d, false, nil
	}
	mine := 0
	for k := range f.conns {
		if k[0] == key[0] {
			mine++
		}
	}
	for k := range f.dials {
		if k[0] == key[0] {
			mine++
		}
	}
	if len(f.dials) >= maxFwdDials || len(f.conns)+len(f.dials) >= maxFwdConns || mine >= maxFwdConnsPerUser {
		return nil, nil, false, errFwdBusy
	}
	d = &fwdDial{ownerID: key[0], hostID: key[1], cancel: cancel, done: make(chan struct{})}
	f.dials[key] = d
	return nil, d, true, nil
}

// finishDial ends a dial: on success (c set, err nil) the connection is
// pooled, unless the dial was revoked meanwhile, and returned with a
// reference held. A connection already in the pool is never replaced.
func (f *forwarder) finishDial(d *fwdDial, c *ssh.Client, version int64, err error) (*fwdConn, error) {
	f.mu.Lock()
	key := [2]int64{d.ownerID, d.hostID}
	delete(f.dials, key)
	defer close(d.done)
	if err == nil && (d.revoked || f.closed) {
		err = errFwdRevoked
	}
	if err != nil {
		d.err = err
		f.mu.Unlock()
		if c != nil {
			c.Close()
		}
		return nil, err
	}
	if existing := f.conns[key]; existing != nil { // cannot happen with acquire; never orphan a connection
		existing.users++
		f.mu.Unlock()
		c.Close()
		return existing, nil
	}
	fc := f.newFwdConn(c, true)
	fc.users, fc.version = 1, version
	f.conns[key] = fc
	f.wg.Add(1)
	f.mu.Unlock()
	go func() {
		defer f.wg.Done()
		_ = c.Wait()
		f.mu.Lock()
		if f.conns[key] == fc {
			delete(f.conns, key)
		}
		f.mu.Unlock()
		fc.transport.CloseIdleConnections()
	}()
	return fc, nil
}

func (f *forwarder) release(fc *fwdConn) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if fc.users--; fc.users == 0 {
		fc.idleSince = time.Now()
	}
}

// detachConn takes a pooled connection out of the pool; called with mu held.
// The returned func closes it, and runs after mu is released: closing can
// wait on the network, and must not hold up every other forward.
func (f *forwarder) detachConn(key [2]int64, fc *fwdConn) func() {
	delete(f.conns, key)
	return func() {
		fc.client.Close()
		fc.transport.CloseIdleConnections()
	}
}

func runAll(fs []func()) {
	for _, fn := range fs {
		fn()
	}
}

// reap closes forwarding connections nobody has used for a while. A
// connection carrying a WebSocket or a long response is in use, however long
// it has been since the last new request.
func (f *forwarder) reap() {
	defer f.wg.Done()
	t := time.NewTicker(fwdReapEvery)
	defer t.Stop()
	for {
		select {
		case <-f.stop:
			return
		case <-t.C:
		}
		var later []func()
		f.mu.Lock()
		f.sweep()
		for k, fc := range f.conns {
			if fc.users == 0 && time.Since(fc.idleSince) > fwdConnIdle {
				later = append(later, f.detachConn(k, fc))
			}
		}
		for c, fc := range f.borrowed {
			if fc.users == 0 && time.Since(fc.idleSince) > fwdReapEvery {
				delete(f.borrowed, c) // the terminal owns the connection; only our transport goes
				later = append(later, fc.transport.CloseIdleConnections)
			}
		}
		f.mu.Unlock()
		runAll(later)
	}
}

// Revocation and shutdown --------------------------------------------------

// revoke ends forwarding access: tickets, grants and requests in flight whose
// use matches, and, when conn is set, the forwarding connections (and dials)
// to matching owner/host pairs.
func (f *forwarder) revoke(use func(fwdUse) bool, conn func(ownerID, hostID int64) bool) {
	var later []func()
	f.mu.Lock()
	for k, g := range f.tickets {
		if use(g.fwdUse) {
			delete(f.tickets, k)
		}
	}
	for k, g := range f.grants {
		if use(g.fwdUse) {
			delete(f.grants, k)
		}
	}
	for st := range f.streams {
		if use(st.fwdUse) {
			st.cancel()
		}
	}
	if conn != nil {
		for _, d := range f.dials {
			if conn(d.ownerID, d.hostID) {
				d.revoked = true
				d.cancel()
			}
		}
		for k, fc := range f.conns {
			if conn(k[0], k[1]) {
				later = append(later, f.detachConn(k, fc))
			}
		}
	}
	f.mu.Unlock()
	runAll(later)
}

// shutdown ends every request, dial and connection, stops the reaper and
// waits for the background goroutines.
func (f *forwarder) shutdown() {
	f.closeOnce.Do(func() {
		var later []func()
		f.mu.Lock()
		f.closed = true
		close(f.stop)
		for st := range f.streams {
			st.cancel()
		}
		for _, d := range f.dials {
			d.revoked = true
			d.cancel()
		}
		for k, fc := range f.conns {
			later = append(later, f.detachConn(k, fc))
		}
		for c, fc := range f.borrowed {
			delete(f.borrowed, c)
			later = append(later, fc.transport.CloseIdleConnections)
		}
		f.mu.Unlock()
		runAll(later)
	})
	f.wg.Wait()
}

// Server-side revocation events ----------------------------------------------

// endForwardLogin ends what one browser login has open through forwards.
func (s *Server) endForwardLogin(userID int64, loginHash string) {
	s.fwd.revoke(func(u fwdUse) bool { return u.userID == userID && u.loginHash == loginHash }, nil)
}

// endForwardOtherLogins keeps only the login keep (after a password change).
func (s *Server) endForwardOtherLogins(userID int64, keep string) {
	s.fwd.revoke(func(u fwdUse) bool { return u.userID == userID && u.loginHash != keep }, nil)
}

// closeUserForwards ends a user's forwarding: what they use, and everything
// on forwards they own (shared ones included), with the connections behind it.
func (s *Server) closeUserForwards(userID int64) {
	s.fwd.revoke(func(u fwdUse) bool { return u.userID == userID || u.ownerID == userID },
		func(ownerID, _ int64) bool { return ownerID == userID })
}

// closeHostForwards ends forwarding to a deleted host. Its forwards are
// removed by the database (cascade).
func (s *Server) closeHostForwards(userID, hostID int64) {
	s.fwd.revoke(func(u fwdUse) bool { return u.ownerID == userID && u.hostID == hostID },
		func(ownerID, h int64) bool { return ownerID == userID && h == hostID })
}

// revokeForwardGrants ends access to one forward, except for keepUser (0: none).
func (s *Server) revokeForwardGrants(id string, keepUser int64) {
	s.fwd.revoke(func(u fwdUse) bool { return u.forwardID == id && u.userID != keepUser }, nil)
}

// Close stops background work and closes the forwarding connections. Call it
// once the HTTP server has stopped taking requests.
func (s *Server) Close() {
	s.stopRecordings()
	s.files.shutdown()
	s.code.shutdown()
	s.fwd.shutdown()
	s.picks.shutdown()
}
