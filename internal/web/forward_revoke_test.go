package web

// Revocation, limits and lifecycle of forwarded connections.

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// setVar changes a package variable for one test. The restore runs after the
// harness has shut down, so background goroutines never see it change.
func setVar[T any](t *testing.T, v *T, val T) {
	t.Helper()
	old := *v
	*v = val
	t.Cleanup(func() { *v = old })
}

// openWS opens a forwarded WebSocket for c and checks it echoes.
func (e *fwdEnv) openWS(c *client) *websocket.Conn {
	e.h.t.Helper()
	f, st := e.h.openForward(c, e.id)
	if st != http.StatusFound {
		e.h.t.Fatalf("open forward: %d", st)
	}
	ws, err := e.dialWS(f, e.origin())
	if err != nil {
		e.h.t.Fatalf("websocket: %v", err)
	}
	e.h.t.Cleanup(func() { ws.CloseNow() })
	if !echoes(ws) {
		e.h.t.Fatal("new websocket does not echo")
	}
	return ws
}

func echoes(ws *websocket.Conn) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := ws.Write(ctx, websocket.MessageText, []byte("ping")); err != nil {
		return false
	}
	_, m, err := ws.Read(ctx)
	return err == nil && string(m) == "ping"
}

func mustClose(t *testing.T, ws *websocket.Conn, what string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	for {
		if _, _, err := ws.Read(ctx); err != nil {
			if ctx.Err() != nil {
				t.Fatalf("%s: forwarded WebSocket stayed open", what)
			}
			return
		}
	}
}

func (e *fwdEnv) share() {
	mustStatus(e.h.t, e.owner.do("PUT", "/api/forwards/"+e.id, map[string]any{"sharing": "users"}), 200, "share")
}

func (e *fwdEnv) pooled() int {
	e.h.srv.fwd.mu.Lock()
	defer e.h.srv.fwd.mu.Unlock()
	return len(e.h.srv.fwd.conns)
}

func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for deadline := time.Now().Add(3 * time.Second); !ok(); {
		if time.Now().After(deadline) {
			t.Fatal(what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestRevokingAccessClosesOpenForwards(t *testing.T) {
	t.Run("sign-out", func(t *testing.T) {
		e := newForwardEnv(t, echoApp)
		other := e.h.login("alice")
		ws, keep := e.openWS(e.owner), e.openWS(other)
		mustStatus(t, e.owner.do("POST", "/api/logout", nil), 200, "logout")
		mustClose(t, ws, "after sign-out")
		if !echoes(keep) {
			t.Fatal("another browser's forward was closed by this sign-out")
		}
	})
	t.Run("password change", func(t *testing.T) {
		e := newForwardEnv(t, echoApp)
		other := e.h.login("alice")
		ws, keep := e.openWS(other), e.openWS(e.owner)
		mustStatus(t, e.owner.do("POST", "/api/me/password", map[string]string{"current": "alice-password-1", "next": "alice-password-2"}), 200, "change")
		mustClose(t, ws, "other browser after a password change")
		if !echoes(keep) {
			t.Fatal("the browser that changed the password lost its forward")
		}
	})
	t.Run("unsharing", func(t *testing.T) {
		e := newForwardEnv(t, echoApp)
		bob := e.h.member(e.admin, "bob")
		e.share()
		ws, keep := e.openWS(bob), e.openWS(e.owner)
		mustStatus(t, e.owner.do("PUT", "/api/forwards/"+e.id, map[string]any{"sharing": "private"}), 200, "unshare")
		mustClose(t, ws, "other user after unsharing")
		if !echoes(keep) {
			t.Fatal("the owner lost their forward when unsharing")
		}
	})
	t.Run("deleting the forward", func(t *testing.T) {
		e := newForwardEnv(t, echoApp)
		ws := e.openWS(e.owner)
		mustStatus(t, e.owner.do("DELETE", "/api/forwards/"+e.id, nil), 200, "delete")
		mustClose(t, ws, "after deleting the forward")
	})
	t.Run("deleting the host", func(t *testing.T) {
		e := newForwardEnv(t, echoApp)
		ws := e.openWS(e.owner)
		mustStatus(t, e.owner.do("DELETE", fmt.Sprintf("/api/hosts/%d", e.hostID), nil), 200, "delete host")
		mustClose(t, ws, "after deleting the host")
		if e.pooled() != 0 {
			t.Fatal("connection to a deleted host still pooled")
		}
	})
	t.Run("disabling the user", func(t *testing.T) {
		e := newForwardEnv(t, echoApp)
		bob := e.h.member(e.admin, "bob")
		e.share()
		ws := e.openWS(bob)
		mustStatus(t, e.admin.do("PUT", fmt.Sprintf("/api/admin/users/%d", e.h.userID(bob)), map[string]any{"disabled": true}), 200, "disable")
		mustClose(t, ws, "disabled user")
	})
	// Disabling a user revokes everything they own, shared forwards included.
	t.Run("disabling the owner", func(t *testing.T) {
		e := newForwardEnv(t, echoApp)
		bob := e.h.member(e.admin, "bob")
		e.share()
		ws := e.openWS(bob)
		fb, _ := e.h.openForward(bob, e.id)
		mustStatus(t, e.admin.do("PUT", fmt.Sprintf("/api/admin/users/%d", e.h.userID(e.owner)), map[string]any{"disabled": true}), 200, "disable owner")
		mustClose(t, ws, "shared forward of a disabled owner")
		if e.pooled() != 0 {
			t.Fatal("disabled owner's connection still pooled")
		}
		if res, _ := fb.get("/x", nil); res.StatusCode == 200 {
			t.Fatal("shared forward of a disabled owner still serves")
		}
		if _, st := e.h.openForward(bob, e.id); st != http.StatusNotFound {
			t.Fatalf("new ticket for a disabled owner's forward: %d", st)
		}
	})
}

// The owner's account and the login are re-checked on every request, even
// when nothing announced the change.
func TestForwardChecksAccessOnEveryRequest(t *testing.T) {
	e := newForwardEnv(t, echoApp)
	bob := e.h.member(e.admin, "bob")
	e.share()
	fb, _ := e.h.openForward(bob, e.id)
	if res, _ := fb.get("/x", nil); res.StatusCode != 200 {
		t.Fatal(res.StatusCode)
	}
	if err := e.h.store.SetDisabled(context.Background(), e.h.userID(e.owner), true); err != nil {
		t.Fatal(err)
	}
	if res, _ := fb.get("/x", nil); res.StatusCode == 200 {
		t.Fatal("served right after the owner was disabled")
	}

	e2 := newForwardEnv(t, echoApp)
	f, _ := e2.h.openForward(e2.owner, e2.id)
	if res, _ := f.get("/x", nil); res.StatusCode != 200 {
		t.Fatal(res.StatusCode)
	}
	// The login ends without an event (as when it expires).
	if err := e2.h.store.DeleteUserLoginSessions(context.Background(), e2.h.userID(e2.owner), ""); err != nil {
		t.Fatal(err)
	}
	if res, _ := f.get("/x", nil); res.StatusCode == 200 {
		t.Fatal("served right after the login ended")
	}
}

// A connection still being dialled when access is revoked never joins the
// pool and does not serve the request.
func TestRevocationReachesDialsInFlight(t *testing.T) {
	for _, revoke := range []string{"disable owner", "delete host"} {
		t.Run(revoke, func(t *testing.T) {
			e := newForwardEnv(t, echoApp)
			f, _ := e.h.openForward(e.owner, e.id)
			entered := make(chan struct{}, 1)
			release := make(chan struct{})
			var once sync.Once
			free := func() { once.Do(func() { close(release) }) }
			t.Cleanup(free)
			e.srv.BeforeAuth(func() {
				select {
				case entered <- struct{}{}:
				default:
				}
				<-release
			})
			got := make(chan int, 1)
			go func() {
				req, _ := http.NewRequest("GET", e.h.ts.URL+"/x", nil)
				req.Host = f.host
				req.AddCookie(&http.Cookie{Name: "corral_fwd", Value: f.cookie})
				res, err := noRedirect.Do(req)
				if err != nil {
					got <- 0
					return
				}
				res.Body.Close()
				got <- res.StatusCode
			}()
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("dial never reached the host")
			}
			if revoke == "disable owner" {
				mustStatus(t, e.admin.do("PUT", fmt.Sprintf("/api/admin/users/%d", e.h.userID(e.owner)), map[string]any{"disabled": true}), 200, "disable")
			} else {
				mustStatus(t, e.owner.do("DELETE", fmt.Sprintf("/api/hosts/%d", e.hostID), nil), 200, "delete host")
			}
			free()
			select {
			case st := <-got:
				if st == 200 {
					t.Fatal("request served through a connection dialled across the revocation")
				}
			case <-time.After(5 * time.Second):
				t.Fatal("request did not finish")
			}
			time.Sleep(100 * time.Millisecond)
			if e.pooled() != 0 {
				t.Fatal("connection dialled across the revocation was pooled")
			}
		})
	}
}

// A WebSocket is a user of its connection however long it stays quiet; only
// connections nobody uses are closed as idle.
func TestIdleReaperSparesConnectionsInUse(t *testing.T) {
	setVar(t, &fwdConnIdle, 50*time.Millisecond)
	setVar(t, &fwdReapEvery, 20*time.Millisecond)
	e := newForwardEnv(t, echoApp)
	ws := e.openWS(e.owner)
	time.Sleep(300 * time.Millisecond)
	if !echoes(ws) || e.pooled() != 1 {
		t.Fatal("connection carrying a WebSocket was reaped as idle")
	}
	ws.Close(websocket.StatusNormalClosure, "")
	eventually(t, "unused connection was never reaped", func() bool { return e.pooled() == 0 })
}

func TestServerCloseEndsForwarding(t *testing.T) {
	e := newForwardEnv(t, echoApp)
	ws := e.openWS(e.owner)
	f, _ := e.h.openForward(e.owner, e.id)
	done := make(chan struct{})
	go func() { e.h.srv.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not return (background goroutines still running)")
	}
	mustClose(t, ws, "after Close")
	if e.pooled() != 0 {
		t.Fatal("pooled connection survived Close")
	}
	if res, _ := f.get("/x", nil); res.StatusCode == 200 {
		t.Fatal("forward still served after Close")
	}
	e.h.srv.Close() // twice is fine
}

func TestForwardLimits(t *testing.T) {
	t.Run("requests per user", func(t *testing.T) {
		setVar(t, &maxFwdStreamsPerUser, 1)
		e := newForwardEnv(t, echoApp)
		ws := e.openWS(e.owner)
		f, _ := e.h.openForward(e.owner, e.id)
		if res, _ := f.get("/x", nil); res.StatusCode != http.StatusServiceUnavailable {
			t.Fatalf("second request in flight: %d", res.StatusCode)
		}
		ws.Close(websocket.StatusNormalClosure, "")
		eventually(t, "slot not freed", func() bool { res, _ := f.get("/x", nil); return res.StatusCode == 200 })
	})
	t.Run("connections per owner", func(t *testing.T) {
		setVar(t, &maxFwdConnsPerUser, 1)
		e := newForwardEnv(t, echoApp)
		host2 := e.h.hostFor(e.owner, e.srv.Addr.Port)
		r := e.owner.do("POST", "/api/sessions", map[string]any{"host_id": host2})
		mustStatus(t, r, 201, "pin second host")
		mustStatus(t, e.owner.do("DELETE", "/api/sessions/"+r.json()["id"].(string), nil), 200, "close")
		r = e.owner.do("POST", "/api/forwards", map[string]any{"host_id": host2, "port": e.port})
		mustStatus(t, r, 200, "second forward")
		f1, _ := e.h.openForward(e.owner, e.id)
		f2, _ := e.h.openForward(e.owner, r.json()["id"].(string))
		if res, _ := f1.get("/x", nil); res.StatusCode != 200 {
			t.Fatal(res.StatusCode)
		}
		if res, _ := f2.get("/x", nil); res.StatusCode != http.StatusServiceUnavailable {
			t.Fatalf("second SSH connection over the limit: %d", res.StatusCode)
		}
	})
	t.Run("tickets and grants per user", func(t *testing.T) {
		setVar(t, &maxFwdTicketsPerUser, 2)
		setVar(t, &maxFwdGrantsPerUser, 2)
		e := newForwardEnv(t, echoApp)
		var tickets []*url.URL
		for range 3 {
			req, _ := http.NewRequest("GET", e.h.ts.URL+"/fwd/"+e.id, nil)
			req.AddCookie(&http.Cookie{Name: e.h.srv.cookieName(), Value: e.owner.cookie})
			res, err := noRedirect.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			res.Body.Close()
			loc, _ := url.Parse(res.Header.Get("Location"))
			tickets = append(tickets, loc)
		}
		redeem := func(loc *url.URL) int {
			res, _ := (&fwdClient{h: e.h, host: loc.Host}).get(loc.RequestURI(), nil)
			return res.StatusCode
		}
		if redeem(tickets[0]) != http.StatusForbidden || redeem(tickets[2]) != http.StatusFound {
			t.Fatal("oldest ticket was not dropped at the limit")
		}
		var grants []*fwdClient
		for range 3 {
			f, _ := e.h.openForward(e.owner, e.id)
			grants = append(grants, f)
		}
		if res, _ := grants[0].get("/x", nil); res.StatusCode == 200 {
			t.Fatal("oldest grant was not dropped at the limit")
		}
		if res, _ := grants[2].get("/x", nil); res.StatusCode != 200 {
			t.Fatal(res.StatusCode)
		}
	})
	// One HTTP transport per SSH connection: keep-alive connections are reused
	// across requests instead of piling up one idle channel per request.
	t.Run("transport reuse", func(t *testing.T) {
		var conns atomic.Int32
		app := &http.Server{Handler: echoApp, ConnState: func(_ net.Conn, s http.ConnState) {
			if s == http.StateNew {
				conns.Add(1)
			}
		}}
		e := newForwardEnv(t, echoApp)
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		go app.Serve(ln)
		t.Cleanup(func() { app.Close() })
		port := ln.Addr().(*net.TCPAddr).Port
		r := e.owner.do("POST", "/api/forwards", map[string]any{"host_id": e.hostID, "port": port})
		mustStatus(t, r, 200, "forward")
		f, _ := e.h.openForward(e.owner, r.json()["id"].(string))
		for range 5 {
			if res, _ := f.get("/x", nil); res.StatusCode != 200 {
				t.Fatal(res.StatusCode)
			}
		}
		if n := conns.Load(); n != 1 {
			t.Fatalf("%d connections to the app for 5 sequential requests, want 1", n)
		}
	})
}

// Many requests at once on one grant (a page loading its assets).
func TestConcurrentForwardRequests(t *testing.T) {
	e := newForwardEnv(t, echoApp)
	f, _ := e.h.openForward(e.owner, e.id)
	var wg sync.WaitGroup
	var bad atomic.Int32
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req, _ := http.NewRequest("GET", e.h.ts.URL+"/x", nil)
			req.Host = f.host
			req.AddCookie(&http.Cookie{Name: "corral_fwd", Value: f.cookie})
			res, err := http.DefaultClient.Do(req)
			if err != nil || res.StatusCode != 200 {
				bad.Add(1)
			}
			if err == nil {
				res.Body.Close()
			}
		}()
	}
	wg.Wait()
	if bad.Load() != 0 {
		t.Fatalf("%d of 16 concurrent requests failed", bad.Load())
	}
}
