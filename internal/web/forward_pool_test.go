package web

// Second-round regressions for forwarding: pool races, channel accounting,
// streams that outlive their login.

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/coder/websocket"
	"golang.org/x/crypto/ssh"

	"github.com/ihainan/corral/internal/config"
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
