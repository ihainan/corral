package web

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/ihainan/corral/internal/config"
)

// fwdClient talks to a forwarding subdomain of the harness server.
type fwdClient struct {
	h      *harness
	host   string // "<port>-<id>.fwd.test"
	cookie string
}

func (f *fwdClient) get(path string, extra map[string]string) (*http.Response, string) {
	req, _ := http.NewRequest("GET", f.h.ts.URL+path, nil)
	req.Host = f.host
	req.Header.Set("Accept", "text/html")
	for k, v := range extra {
		req.Header.Set(k, v)
	}
	if f.cookie != "" {
		req.AddCookie(&http.Cookie{Name: "corral_fwd", Value: f.cookie}) // appends to any Cookie header
	}
	res, err := noRedirect.Do(req)
	if err != nil {
		f.h.t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	for _, c := range res.Cookies() {
		if c.Name == "corral_fwd" {
			f.cookie = c.Value
		}
	}
	return res, string(b)
}

var noRedirect = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

// open runs the ticket hand-off for a signed-in client and returns a client
// holding the subdomain cookie.
func (h *harness) openForward(c *client, id string) (*fwdClient, int) {
	h.t.Helper()
	req, _ := http.NewRequest("GET", h.ts.URL+"/fwd/"+id+"?return=/hello", nil)
	req.AddCookie(&http.Cookie{Name: h.srv.cookieName(), Value: c.cookie})
	res, err := noRedirect.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusFound {
		return nil, res.StatusCode
	}
	loc, _ := url.Parse(res.Header.Get("Location"))
	f := &fwdClient{h: h, host: loc.Host}
	r2, _ := f.get(loc.RequestURI(), nil)
	if r2.StatusCode != http.StatusFound || f.cookie == "" {
		h.t.Fatalf("ticket exchange: %d", r2.StatusCode)
	}
	return f, http.StatusFound
}

func TestPortForwardingFlow(t *testing.T) {
	// The "dev server" on the host: echoes what it was sent, and upgrades /ws.
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/ws" {
			c, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
			if err != nil {
				return
			}
			_, m, _ := c.Read(r.Context())
			c.Write(r.Context(), websocket.MessageText, append([]byte("echo:"), m...))
			c.Close(websocket.StatusNormalClosure, "")
			return
		}
		if r.URL.Path == "/redirect" {
			http.Redirect(w, r, "http://"+r.Host+"/hello", http.StatusFound)
			return
		}
		fmt.Fprintf(w, "host=%s origin=%s cookie=%s", r.Host, r.Header.Get("Origin"), r.Header.Get("Cookie"))
	}))
	defer app.Close()
	appPort := app.Listener.Addr().(*net.TCPAddr).Port

	h := newHarness(t, func(c *config.Config) { c.ForwardDomain = "fwd.test" })
	admin := h.admin()
	bob := h.member(admin, "bob")
	srv := h.sshServer(t.TempDir())
	hostID := h.hostFor(admin, srv.Addr.Port)
	mustStatus(t, admin.do("POST", "/api/sessions", map[string]any{"host_id": hostID}), 201, "terminal (pins the key)")

	r := admin.do("POST", "/api/forwards", map[string]any{"host_id": hostID, "port": appPort})
	mustStatus(t, r, 200, "create forward")
	id := r.json()["id"].(string)

	f, st := h.openForward(admin, id)
	if st != http.StatusFound {
		t.Fatalf("owner could not open the forward: %d", st)
	}
	if !strings.HasPrefix(f.host, strconv.Itoa(appPort)+"-"+id+".") {
		t.Fatalf("unexpected subdomain %q", f.host)
	}
	res, body := f.get("/hello", map[string]string{"Origin": "http://" + f.host, "Cookie": "app=1; corral_session=stolen"})
	if res.StatusCode != 200 || !strings.Contains(body, "host=localhost:"+strconv.Itoa(appPort)) || !strings.Contains(body, "origin=http://localhost:") {
		t.Fatalf("proxied request: %d %s", res.StatusCode, body)
	}
	if strings.Contains(body, "corral_fwd") || strings.Contains(body, "corral_session") || !strings.Contains(body, "app=1") {
		t.Fatalf("cookies not filtered: %s", body)
	}
	// Redirects to localhost come back to the subdomain.
	res, _ = f.get("/redirect", nil)
	if loc := res.Header.Get("Location"); !strings.HasPrefix(loc, "http://"+f.host) {
		t.Fatalf("redirect not rewritten: %q", loc)
	}
	// WebSocket through the proxy (dev-server hot reload).
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	hdr := http.Header{}
	hdr.Set("Cookie", "corral_fwd="+f.cookie)
	hdr.Set("Host", f.host)
	ws, _, err := websocket.Dial(ctx, strings.Replace(h.ts.URL, "http", "ws", 1)+"/ws", &websocket.DialOptions{HTTPHeader: hdr, Host: f.host})
	if err != nil {
		t.Fatalf("websocket through forward: %v", err)
	}
	ws.Write(ctx, websocket.MessageText, []byte("hi"))
	_, m, err := ws.Read(ctx)
	if err != nil || string(m) != "echo:hi" {
		t.Fatalf("websocket echo: %q %v", m, err)
	}
	ws.CloseNow()

	// Without the cookie: sent to sign in.
	anon := &fwdClient{h: h, host: f.host}
	if res, _ := anon.get("/hello", nil); res.StatusCode != http.StatusFound || !strings.Contains(res.Header.Get("Location"), "/fwd/"+id) {
		t.Fatalf("anonymous request: %d %s", res.StatusCode, res.Header.Get("Location"))
	}
	// Another user cannot open a private forward; can once it is shared.
	if _, st := h.openForward(bob, id); st != http.StatusNotFound {
		t.Fatalf("bob opened a private forward: %d", st)
	}
	mustStatus(t, admin.do("PUT", "/api/forwards/"+id, map[string]any{"sharing": "users"}), 200, "share")
	fb, st := h.openForward(bob, id)
	if st != http.StatusFound {
		t.Fatalf("bob could not open a shared forward: %d", st)
	}
	// A grant is for one forward only: bob's cookie for the shared forward does
	// not open another (private) one.
	app2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "secret") }))
	defer app2.Close()
	p2 := app2.Listener.Addr().(*net.TCPAddr).Port
	r2 := admin.do("POST", "/api/forwards", map[string]any{"host_id": hostID, "port": p2})
	id2 := r2.json()["id"].(string)
	if res, _ := fb.get("/hello", nil); res.StatusCode != 200 { // uses the grant; its periodic check is now fresh
		t.Fatalf("bob on the shared forward: %d", res.StatusCode)
	}
	other := &fwdClient{h: h, host: strconv.Itoa(p2) + "-" + id2 + ".fwd.test", cookie: fb.cookie}
	if res, body := other.get("/hello", nil); res.StatusCode == 200 || strings.Contains(body, "secret") {
		t.Fatal("grant reused on another forward")
	}
	// Disabling bob ends his access through the proxy.
	bobID := int64(bob.do("GET", "/api/me", nil).json()["id"].(float64))
	mustStatus(t, admin.do("PUT", fmt.Sprintf("/api/admin/users/%d", bobID), map[string]any{"disabled": true}), 200, "disable bob")
	if res, _ := fb.get("/hello", nil); res.StatusCode == 200 {
		t.Fatal("disabled user still reaches the forward")
	}
	// Deleting the forward ends the owner's access too.
	mustStatus(t, admin.do("DELETE", "/api/forwards/"+id, nil), 200, "delete forward")
	if res, _ := f.get("/hello", nil); res.StatusCode == 200 {
		t.Fatal("deleted forward still serves")
	}
}
