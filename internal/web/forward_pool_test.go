package web

// Second-round regressions for forwarding: pool races, channel accounting,
// streams that outlive their login.

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"golang.org/x/crypto/ssh"

	"github.com/ihainan/corral/internal/config"
	"github.com/ihainan/corral/internal/herdr"
	"github.com/ihainan/corral/internal/testutil"
)

func sshClient(t *testing.T, srv *testutil.SSHServer) *ssh.Client {
	t.Helper()
	c, err := ssh.Dial("tcp", srv.Addr.String(), &ssh.ClientConfig{User: "alice", Auth: []ssh.AuthMethod{ssh.Password("alice-pass")},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(), Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func closedSoon(c *ssh.Client) bool {
	done := make(chan struct{})
	go func() { c.Wait(); close(done) }()
	select {
	case <-done:
		return true
	case <-time.After(2 * time.Second):
		return false
	}
}

// Taking a pooled connection and claiming a dial are one step: once a
// connection is pooled nobody dials again, and a finished dial never replaces
// a pooled connection (which would leave it open, uncounted and unclosable).
func TestForwardPoolNeverOrphansAConnection(t *testing.T) {
	srv := testutil.StartSSHServer(t, "alice", "alice-pass", t.TempDir())
	f := newForwarder()
	defer func() {
		done := make(chan struct{})
		go func() { f.shutdown(); close(done) }()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("shutdown hangs on a connection that left the pool")
		}
	}()
	key := [2]int64{1, 2}
	noop := func() {}

	_, d, lead, err := f.acquire(key, noop)
	if err != nil || !lead {
		t.Fatalf("first request should dial: %v %v", lead, err)
	}
	if _, d2, lead2, _ := f.acquire(key, noop); lead2 || d2 != d {
		t.Fatal("a second request dialled while the first dial was under way")
	}
	first := sshClient(t, srv)
	fc, err := f.finishDial(d, first, nil)
	if err != nil || fc.client != first {
		t.Fatalf("dial not pooled: %v", err)
	}
	got, d3, lead3, err := f.acquire(key, noop)
	if err != nil || got != fc || d3 != nil || lead3 {
		t.Fatal("a request after the dial did not reuse the pooled connection")
	}
	f.release(got)

	// Even a dial finishing next to a pooled connection keeps the pooled one.
	f.mu.Lock()
	late := &fwdDial{ownerID: key[0], hostID: key[1], cancel: noop, done: make(chan struct{})}
	f.mu.Unlock()
	second := sshClient(t, srv)
	if fc2, err := f.finishDial(late, second, nil); err != nil || fc2 != fc {
		t.Fatal("pooled connection was replaced")
	}
	if !closedSoon(second) {
		t.Fatal("redundant connection left open")
	}
	f.mu.Lock()
	pooled := f.conns[key]
	f.mu.Unlock()
	if pooled != fc {
		t.Fatal("pool lost its connection")
	}
}

// The per-connection channel limit belongs to the SSH connection: dropping
// and recreating the transport around a terminal's connection (the reaper
// does) must not reset it while channels are still open.
func TestChannelLimitSurvivesANewTransport(t *testing.T) {
	setVar(t, &maxFwdChannelsPerConn, 1)
	quiet, err := net.Listen("tcp", "127.0.0.1:0") // keeps the forwarded channel open
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
	c := sshClient(t, srv)
	f := newForwarder()
	defer f.shutdown()
	ctx := context.Background()
	fc, _ := f.borrow(c)
	conn, err := fc.transport.DialContext(ctx, "tcp", quiet.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	f.release(fc)
	f.mu.Lock()
	delete(f.borrowed, c) // what the reaper does with an unused wrapper
	f.mu.Unlock()
	fc2, _ := f.borrow(c)
	if _, err := fc2.transport.DialContext(ctx, "tcp", quiet.Addr().String()); err == nil {
		t.Fatal("a new transport reset the connection's channel limit")
	}
}

// Channels to forwarded ports count until they are really finished; over
// the cap, new ones are refused.
func TestForwardChannelsAreCapped(t *testing.T) {
	setVar(t, &maxFwdChannels, 1)
	e := newForwardEnv(t, echoApp)
	ws := e.openWS(e.owner) // holds the only channel
	f, _ := e.h.openForward(e.owner, e.id)
	if res, _ := f.get("/x", nil); res.StatusCode == 200 {
		t.Fatal("a second channel was opened over the cap")
	}
	ws.Close(websocket.StatusNormalClosure, "")
	eventually(t, "channel was not given back", func() bool { res, _ := f.get("/x", nil); return res.StatusCode == 200 })
	e.h.srv.fwd.mu.Lock()
	n := e.h.srv.fwd.channels
	e.h.srv.fwd.mu.Unlock()
	if n < 0 || n > 1 {
		t.Fatalf("%d channels counted", n)
	}
}

// Port listing counts against the same per-user limit as proxied requests.
func TestPortListingCountsAsAStream(t *testing.T) {
	setVar(t, &maxFwdStreamsPerUser, 1)
	e := newForwardEnv(t, echoApp)
	e.openWS(e.owner)
	r := e.owner.do("GET", fmt.Sprintf("/api/hosts/%d/ports", e.hostID), nil)
	if r.Status != http.StatusServiceUnavailable || r.code() != "busy" {
		t.Fatalf("port listing over the limit: %d %s", r.Status, r.Body)
	}
}

// floodApp streams without end, over a plain response and over a WebSocket.
var floodApp = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
	chunk := bytes.Repeat([]byte("x"), 64<<10)
	if r.URL.Path == "/ws" {
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		defer c.CloseNow()
		for c.Write(r.Context(), websocket.MessageBinary, chunk) == nil {
		}
		return
	}
	for {
		if _, err := w.Write(chunk); err != nil {
			return
		}
		http.NewResponseController(w).Flush()
	}
})

func (e *fwdEnv) streams() int {
	e.h.srv.fwd.mu.Lock()
	defer e.h.srv.fwd.mu.Unlock()
	return len(e.h.srv.fwd.streams)
}

// A browser that stopped reading leaves the proxy blocked writing to it.
// Ending the request (here: signing out) must still end the handler, for a
// plain streamed response and for a WebSocket.
func TestRevocationFreesHandlersBlockedOnASlowBrowser(t *testing.T) {
	for _, path := range []string{"/stream", "/ws"} {
		t.Run(path, func(t *testing.T) {
			e := newForwardEnv(t, floodApp)
			f, _ := e.h.openForward(e.owner, e.id)
			conn, err := net.Dial("tcp", e.h.ts.Listener.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			req := "GET " + path + " HTTP/1.1\r\nHost: " + f.host + "\r\nCookie: corral_fwd=" + f.cookie + "\r\n"
			if path == "/ws" {
				req += "Connection: Upgrade\r\nUpgrade: websocket\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n"
			}
			conn.Write([]byte(req + "\r\n"))
			conn.(*net.TCPConn).SetReadBuffer(4 << 10)
			if _, err := conn.Read(make([]byte, 512)); err != nil { // the response starts, then we stop reading
				t.Fatal(err)
			}
			eventually(t, "stream never started", func() bool { return e.streams() == 1 })
			time.Sleep(time.Second) // every buffer on the way fills up
			mustStatus(t, e.owner.do("POST", "/api/logout", nil), 200, "logout")
			eventually(t, "handler stayed blocked writing to a browser that stopped reading", func() bool { return e.streams() == 0 })
		})
	}
}

// A herdr session end to end on the test SSH server: the terminal starts,
// discovery over the same connection finds the running herdr session (the
// host sends EOF before the exit status), and the gateway starts watching
// its socket.
func TestHerdrSessionIsDiscovered(t *testing.T) {
	h := newHarness(t)
	admin := h.admin()
	srv := h.sshServer(t.TempDir())
	srv.HandleExec(func(cmd string) func(ssh.Channel) {
		if cmd != herdr.DiscoverCommand {
			return nil // the terminal itself: the default echoing shell
		}
		return func(ch ssh.Channel) {
			ch.Write([]byte(`{"sessions":[{"name":"default","default":true,"running":true,"socket_path":"/run/herdr.sock"}]}`))
			ch.CloseWrite()
			time.Sleep(50 * time.Millisecond)
			ch.SendRequest("exit-status", false, []byte{0, 0, 0, 0})
		}
	})
	r := admin.do("POST", "/api/hosts", map[string]any{"name": "box", "address": "127.0.0.1", "port": srv.Addr.Port,
		"username": "alice", "auth": "password", "password": "alice-pass", "persistence": "herdr"})
	mustStatus(t, r, 201, "create host")
	r = admin.do("POST", "/api/sessions", map[string]any{"host_id": int64(r.json()["id"].(float64))})
	mustStatus(t, r, 201, "open herdr session")
	id := r.json()["id"].(string)
	eventually(t, "herdr session was never discovered", func() bool { return h.srv.herdrSocks.get(id) == "/run/herdr.sock" })
}

// holdApp accepts a WebSocket and keeps it open without a word.
var holdApp = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		return
	}
	defer c.CloseNow()
	c.Read(r.Context())
})

func (e *fwdEnv) channels() int {
	e.h.srv.fwd.mu.Lock()
	defer e.h.srv.fwd.mu.Unlock()
	return e.h.srv.fwd.channels
}

// The SSH connection under an upgraded forward drops while the browser keeps
// its side open and silent: the proxy still waits on the browser, but the
// channel is gone, so its quota must come back without waiting for that.
func TestChannelQuotaReturnsWhenSSHDropsUnderAnOpenWebSocket(t *testing.T) {
	e := newForwardEnv(t, holdApp)
	f, _ := e.h.openForward(e.owner, e.id)
	conn, err := net.Dial("tcp", e.h.ts.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close() // only at the end: the browser's side stays open
	conn.Write([]byte("GET /ws HTTP/1.1\r\nHost: " + f.host + "\r\nCookie: corral_fwd=" + f.cookie +
		"\r\nConnection: Upgrade\r\nUpgrade: websocket\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n\r\n"))
	if _, err := conn.Read(make([]byte, 512)); err != nil { // 101 Switching Protocols
		t.Fatal(err)
	}
	eventually(t, "forwarded WebSocket never opened", func() bool { return e.channels() == 1 })
	e.srv.Close() // the host side goes away
	eventually(t, "channel quota kept after the SSH connection ended", func() bool { return e.channels() == 0 })
}

// stop may be called more than once, also concurrently, cancelled or not.
func TestAfterFuncWaitStopIsIdempotent(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stop := afterFuncWait(ctx, func() {})
	returnsWithin := func(what string, f func()) {
		done := make(chan struct{})
		go func() { f(); close(done) }()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal(what)
		}
	}
	returnsWithin("second stop hung", func() { stop(); stop() })
	returnsWithin("concurrent stops hung", func() {
		var wg sync.WaitGroup
		for range 8 {
			wg.Add(1)
			go func() { defer wg.Done(); stop() }()
		}
		wg.Wait()
	})
	// Cancelled: every stop returns, and only after the callback finished.
	ctx2, cancel2 := context.WithCancel(context.Background())
	var finished atomic.Bool
	started := make(chan struct{})
	stop2 := afterFuncWait(ctx2, func() { close(started); time.Sleep(50 * time.Millisecond); finished.Store(true) })
	cancel2()
	<-started
	returnsWithin("stops after a cancel hung", func() {
		var wg sync.WaitGroup
		for range 4 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				stop2()
				if !finished.Load() {
					t.Error("a stop returned before the callback finished")
				}
			}()
		}
		wg.Wait()
	})
}

// The write cut-off for a cancelled request must be over by the time the
// handler returns: on HTTP/1.1 the connection serves the next request
// right after, which a late "deadline in the past" would break.
func TestCutoffNeverOutlivesTheHandler(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var finished atomic.Bool
	started := make(chan struct{})
	stop := afterFuncWait(ctx, func() {
		close(started)
		time.Sleep(100 * time.Millisecond)
		finished.Store(true)
	})
	cancel()
	<-started // the callback is running as the handler returns
	stop()
	if !finished.Load() {
		t.Fatal("stop returned while the cut-off was still running")
	}
}

// A forwarded WebSocket ends when its login does, also when nothing
// announces it: on the periodic re-check, and at the login's absolute expiry.
func TestForwardedStreamsEndWithTheLogin(t *testing.T) {
	t.Run("re-check", func(t *testing.T) {
		setVar(t, &loginRecheck, 50*time.Millisecond)
		e := newForwardEnv(t, echoApp)
		ws := e.openWS(e.owner)
		if err := e.h.store.DeleteUserLoginSessions(context.Background(), e.h.userID(e.owner), ""); err != nil {
			t.Fatal(err)
		}
		mustClose(t, ws, "after the login ended")
	})
	t.Run("absolute expiry", func(t *testing.T) {
		e := newForwardEnv(t, echoApp, func(c *config.Config) { c.LoginAbsoluteLimit = 4 * time.Second })
		ws := e.openWS(e.owner)
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		for {
			if _, _, err := ws.Read(ctx); err != nil {
				if ctx.Err() != nil {
					t.Fatal("forwarded WebSocket outlived the login")
				}
				return
			}
		}
	})
}
