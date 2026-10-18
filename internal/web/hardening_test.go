package web

// Regression tests for findings from the forwarding and notification review.

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/coder/websocket"
	"golang.org/x/crypto/ssh"

	"github.com/ihainan/corral/internal/auth"
	"github.com/ihainan/corral/internal/config"
	"github.com/ihainan/corral/internal/notify"
	"github.com/ihainan/corral/internal/sshx"
	"github.com/ihainan/corral/internal/testutil"
)

// withPush turns notifications on for the harness server.
func (h *harness) withPush() {
	h.t.Helper()
	n, err := notify.New(context.Background(), h.store, slog.New(slog.NewTextHandler(io.Discard, nil)),
		notify.Options{LoginValid: h.auth.LoginValid})
	if err != nil {
		h.t.Fatal(err)
	}
	h.srv.notifier = n
}

func (h *harness) userID(c *client) int64 {
	h.t.Helper()
	return int64(c.do("GET", "/api/me", nil).json()["id"].(float64))
}

func (c *client) subscribe(endpoint string) resp {
	c.h.t.Helper()
	return c.do("POST", "/api/push/subscriptions", map[string]any{"endpoint": endpoint,
		"keys": map[string]string{"p256dh": "BPk", "auth": "a"}})
}

func (h *harness) endpoints(userID int64) map[string]bool {
	h.t.Helper()
	subs, err := h.store.PushSubscriptions(context.Background(), userID)
	if err != nil {
		h.t.Fatal(err)
	}
	out := map[string]bool{}
	for _, s := range subs {
		out[s.Endpoint] = true
	}
	return out
}

func (h *harness) login(name string) *client {
	h.t.Helper()
	c := &client{h: h}
	mustStatus(h.t, c.do("POST", "/api/login", map[string]string{"username": name, "password": name + "-password-1"}), 200, "login "+name)
	return c
}

const fcm = "https://fcm.googleapis.com/fcm/send/"

func TestPushSubscriptionsEndWithTheirLogin(t *testing.T) {
	h := newHarness(t)
	h.withPush()
	admin := h.admin()
	alice := h.member(admin, "alice")
	aliceID := h.userID(alice)

	// Signing out removes that browser's subscription only.
	phone := h.login("alice")
	mustStatus(t, alice.subscribe(fcm+"laptop"), 200, "subscribe laptop")
	mustStatus(t, phone.subscribe(fcm+"phone"), 200, "subscribe phone")
	mustStatus(t, alice.do("POST", "/api/logout", nil), 200, "logout")
	if got := h.endpoints(aliceID); got[fcm+"laptop"] || !got[fcm+"phone"] {
		t.Fatalf("after signing out the laptop: %v", got)
	}

	// A password change signs other browsers out, and their subscriptions with them.
	tablet := h.login("alice")
	mustStatus(t, tablet.subscribe(fcm+"tablet"), 200, "subscribe tablet")
	mustStatus(t, phone.do("POST", "/api/me/password", map[string]string{"current": "alice-password-1", "next": "alice-password-2"}), 200, "change password")
	if got := h.endpoints(aliceID); got[fcm+"tablet"] || !got[fcm+"phone"] {
		t.Fatalf("after a password change: %v", got)
	}

	// Registering an endpoint again moves it to the new user and login.
	bob := h.member(admin, "bob")
	bobID := h.userID(bob)
	mustStatus(t, bob.subscribe(fcm+"phone"), 200, "bob takes over the device")
	if h.endpoints(aliceID)[fcm+"phone"] || !h.endpoints(bobID)[fcm+"phone"] {
		t.Fatal("endpoint was not rebound to the new user")
	}
	mustStatus(t, bob.do("POST", "/api/logout", nil), 200, "bob logout")
	if len(h.endpoints(bobID)) != 0 {
		t.Fatal("rebound subscription survived the new login's sign-out")
	}

	// Disabling a user and an administrator's password reset end them too.
	mustStatus(t, phone.subscribe(fcm+"phone2"), 200, "subscribe again")
	mustStatus(t, admin.do("POST", fmt.Sprintf("/api/admin/users/%d/password", aliceID), map[string]string{"password": "reset-password-1"}), 200, "reset")
	if len(h.endpoints(aliceID)) != 0 {
		t.Fatal("subscription survived an administrator's password reset")
	}
	carol := h.member(admin, "carol")
	carolID := h.userID(carol)
	mustStatus(t, carol.subscribe(fcm+"carol"), 200, "subscribe carol")
	mustStatus(t, admin.do("PUT", fmt.Sprintf("/api/admin/users/%d", carolID), map[string]any{"disabled": true}), 200, "disable")
	if len(h.endpoints(carolID)) != 0 {
		t.Fatal("subscription survived disabling the user")
	}
}

func TestPushSubscriptionsPerUserAreCapped(t *testing.T) {
	old := maxPushSubscriptionsPerUser
	maxPushSubscriptionsPerUser = 3
	defer func() { maxPushSubscriptionsPerUser = old }()
	h := newHarness(t)
	h.withPush()
	admin := h.admin()
	for i := 0; i < 6; i++ {
		mustStatus(t, admin.subscribe(fmt.Sprintf("%sdev%d", fcm, i)), 200, "subscribe")
	}
	got := h.endpoints(h.userID(admin))
	if len(got) != 3 || !got[fcm+"dev5"] || got[fcm+"dev0"] {
		t.Fatalf("kept %v, want the 3 newest", got)
	}
	r := admin.do("GET", "/api/push", nil)
	if devs, _ := r.json()["devices"].([]any); len(devs) != 3 {
		t.Fatalf("device list: %s", r.Body)
	}
	if r.Status != http.StatusOK {
		t.Fatal(r.Status)
	}
}

// The cap drops the least recently registered devices, never the one just
// registered, even when it is an old row taken over from someone else.
func TestPushSubscriptionCapKeepsTheDeviceJustRegistered(t *testing.T) {
	setVar(t, &maxPushSubscriptionsPerUser, 3)
	h := newHarness(t)
	h.withPush()
	admin := h.admin()
	bob := h.member(admin, "bob")
	mustStatus(t, bob.subscribe(fcm+"shared"), 200, "bob's device") // the oldest row
	for i := 1; i <= 3; i++ {
		mustStatus(t, admin.subscribe(fmt.Sprintf("%sa%d", fcm, i)), 200, "subscribe")
	}
	mustStatus(t, admin.subscribe(fcm+"shared"), 200, "device moves to the admin")
	got := h.endpoints(h.userID(admin))
	if len(got) != 3 || !got[fcm+"shared"] || got[fcm+"a1"] {
		t.Fatalf("kept %v, want the moved device and the two newest", got)
	}
	// Registering again counts as recent: a2 survives, a3 is now the oldest.
	mustStatus(t, admin.subscribe(fcm+"a2"), 200, "re-register")
	mustStatus(t, admin.subscribe(fcm+"a4"), 200, "new device")
	got = h.endpoints(h.userID(admin))
	if len(got) != 3 || !got[fcm+"a2"] || !got[fcm+"a4"] || !got[fcm+"shared"] {
		t.Fatalf("kept %v, want a2, a4 and shared", got)
	}
}

func TestPortListingOutputIsBounded(t *testing.T) {
	old := sshx.MaxOutput
	sshx.MaxOutput = 64 << 10
	defer func() { sshx.MaxOutput = old }()
	h := newHarness(t)
	admin := h.admin()
	srv := h.sshServer(t.TempDir())
	srv.HandleExec(func(cmd string) func(ssh.Channel) {
		if cmd != listenCommand {
			return nil
		}
		return func(ch ssh.Channel) {
			line := []byte(strings.Repeat("LISTEN 0 4096 127.0.0.1:8080 0.0.0.0:*\n", 100))
			for {
				if _, err := ch.Write(line); err != nil {
					return
				}
			}
		}
	})
	hostID := h.hostFor(admin, srv.Addr.Port)
	mustStatus(t, admin.do("POST", "/api/sessions", map[string]any{"host_id": hostID}), 201, "terminal")
	r := admin.do("GET", fmt.Sprintf("/api/hosts/%d/ports", hostID), nil)
	if r.Status != http.StatusBadGateway || r.code() != "ports_failed" {
		t.Fatalf("endless port list: %d %s", r.Status, r.Body)
	}
}

// fwdEnv is a harness with forwarding on, a host and a forward to app's port.
type fwdEnv struct {
	h      *harness
	admin  *client
	owner  *client // "alice", a member: owns the host and the forward
	srv    *testutil.SSHServer
	hostID int64
	id     string
	port   int
}

func newForwardEnv(t *testing.T, app http.Handler, opts ...harnessOpt) *fwdEnv {
	t.Helper()
	ts := httptest.NewServer(app)
	t.Cleanup(ts.Close)
	e := &fwdEnv{port: ts.Listener.Addr().(*net.TCPAddr).Port}
	e.h = newHarness(t, append([]harnessOpt{func(c *config.Config) { c.ForwardDomain = "fwd.test" }}, opts...)...)
	e.admin = e.h.admin()
	e.owner = e.h.member(e.admin, "alice")
	e.srv = e.h.sshServer(t.TempDir())
	e.hostID = e.h.hostFor(e.owner, e.srv.Addr.Port)
	// A terminal once, to pin the host key; then closed so forwarding dials its own connection.
	r := e.owner.do("POST", "/api/sessions", map[string]any{"host_id": e.hostID})
	mustStatus(t, r, 201, "terminal (pins the key)")
	mustStatus(t, e.owner.do("DELETE", "/api/sessions/"+r.json()["id"].(string), nil), 200, "close terminal")
	r = e.owner.do("POST", "/api/forwards", map[string]any{"host_id": e.hostID, "port": e.port})
	mustStatus(t, r, 200, "create forward")
	e.id = r.json()["id"].(string)
	return e
}

func (e *fwdEnv) origin() string { return e.h.srv.forwardOriginFor(e.port, e.id) }

// dialWS opens a WebSocket on the forward with the given Origin ("" for none).
func (e *fwdEnv) dialWS(f *fwdClient, origin string) (*websocket.Conn, error) {
	hdr := http.Header{}
	hdr.Set("Cookie", "corral_fwd="+f.cookie)
	if origin != "" {
		hdr.Set("Origin", origin)
	}
	ws, _, err := websocket.Dial(context.Background(), strings.Replace(e.h.ts.URL, "http", "ws", 1)+"/ws",
		&websocket.DialOptions{HTTPHeader: hdr, Host: f.host})
	return ws, err
}

// echoApp reports the Origin it saw, and keeps WebSockets open, echoing.
var echoApp = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/ws" {
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		defer c.CloseNow()
		for {
			_, m, err := c.Read(r.Context())
			if err != nil {
				return
			}
			c.Write(r.Context(), websocket.MessageText, m)
		}
	}
	fmt.Fprintf(w, "origin=%s", r.Header.Get("Origin"))
})

func TestForwardRefusesRequestsFromOtherOrigins(t *testing.T) {
	e := newForwardEnv(t, echoApp)
	f, _ := e.h.openForward(e.owner, e.id)
	own := e.origin()
	sibling := e.h.srv.forwardOriginFor(e.port+1, "zzzz") // another forwarded port: same site
	cases := []struct {
		method string
		hdr    map[string]string
		want   int
	}{
		{"GET", nil, 200},
		{"GET", map[string]string{"Origin": own}, 200},
		{"GET", map[string]string{"Origin": sibling}, 403},
		{"POST", map[string]string{"Origin": own}, 200},
		{"POST", map[string]string{"Origin": sibling}, 403},
		{"POST", map[string]string{"Origin": "null"}, 403},
		{"POST", map[string]string{"Origin": e.h.cfg.Origin()}, 403}, // even the main site
		{"POST", nil, 200}, // no Origin and no browser hints: not a browser
		{"POST", map[string]string{"Sec-Fetch-Site": "same-origin"}, 200},
		{"POST", map[string]string{"Sec-Fetch-Site": "same-site"}, 403},
		{"DELETE", map[string]string{"Sec-Fetch-Site": "cross-site"}, 403},
		{"OPTIONS", map[string]string{"Origin": sibling}, 403},
	}
	for _, c := range cases {
		if got := f.send(c.method, "/x", c.hdr); got != c.want {
			t.Errorf("%s with %v: %d, want %d", c.method, c.hdr, got, c.want)
		}
	}
	// The app sees the localhost form only for the forward's own pages.
	if _, body := f.get("/x", map[string]string{"Origin": own}); !strings.Contains(body, "origin=http://localhost:"+strconv.Itoa(e.port)) {
		t.Fatalf("origin not rewritten: %s", body)
	}
	if ws, err := e.dialWS(f, sibling); err == nil {
		ws.CloseNow()
		t.Fatal("WebSocket from another forwarded port was accepted")
	}
	ws, err := e.dialWS(f, own)
	if err != nil {
		t.Fatalf("WebSocket from the forward's own page: %v", err)
	}
	ws.CloseNow()
}

func TestSafeReturnPath(t *testing.T) {
	for _, ok := range []string{"/", "/hello", "/a/b?x=1&y=%2F", "/search?q=caf%C3%A9", "/a%20b"} {
		if !safeReturnPath(ok) {
			t.Errorf("%q refused", ok)
		}
	}
	for _, bad := range []string{"", "hello", "//evil.example", "/\\evil.example", "/%5Cevil.example", "/%5cevil.example",
		"/%2F/evil.example", "%2F%2Fevil.example", "/%252F%252Fevil.example", "https://evil.example/", "/a\r\nSet-Cookie: x",
		"/%0d%0aSet-Cookie:x", "/\x00", "/%zz", "/%25252525252F"} {
		if safeReturnPath(bad) {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestForwardTicketReturnIsFixedWhenIssued(t *testing.T) {
	e := newForwardEnv(t, echoApp)
	issue := func(ret string) *url.URL {
		req, _ := http.NewRequest("GET", e.h.ts.URL+"/fwd/"+e.id+"?return="+url.QueryEscape(ret), nil)
		req.AddCookie(&http.Cookie{Name: e.h.srv.cookieName(), Value: e.owner.cookie})
		res, err := noRedirect.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		loc, _ := url.Parse(res.Header.Get("Location"))
		if loc.Query().Has("return") {
			t.Fatalf("return path travels outside the ticket: %s", loc)
		}
		return loc
	}
	redeem := func(loc *url.URL, extra string) string {
		f := &fwdClient{h: e.h, host: loc.Host}
		res, _ := f.get(loc.RequestURI()+extra, nil)
		return res.Header.Get("Location")
	}
	if got := redeem(issue("/app/page?x=1"), ""); got != "/app/page?x=1" {
		t.Fatalf("return path: %q", got)
	}
	if got := redeem(issue("/%5Cevil.example"), ""); got != "/" {
		t.Fatalf("unsafe return path kept: %q", got)
	}
	// A return given at redemption is ignored.
	if got := redeem(issue("/ok"), "&return=/%5Cevil.example"); got != "/ok" {
		t.Fatalf("redemption honoured a supplied return: %q", got)
	}
}

// Sign-out deletes the login even if the browser gave up on the request, and
// says so when it could not.
func TestLogoutDeletesTheLoginOrFails(t *testing.T) {
	h := newHarness(t)
	admin := h.admin()
	other := h.login("root")
	u, err := h.store.UserByName(context.Background(), "root")
	if err != nil {
		t.Fatal(err)
	}
	logout := func(c *client, ctx context.Context) *httptest.ResponseRecorder {
		ctx = context.WithValue(context.WithValue(ctx, userKey, u), loginKey, auth.HashToken(c.cookie))
		rec := httptest.NewRecorder()
		h.srv.apiLogout(rec, httptest.NewRequest("POST", "/api/logout", nil).WithContext(ctx))
		return rec
	}
	gone, cancel := context.WithCancel(context.Background())
	cancel() // the browser already went away
	if rec := logout(other, gone); rec.Code != 200 {
		t.Fatalf("sign-out of a cancelled request: %d %s", rec.Code, rec.Body)
	}
	mustStatus(t, other.do("GET", "/api/me", nil), http.StatusUnauthorized, "login after sign-out")

	h.store.Close() // the database refuses the delete
	rec := logout(admin, context.Background())
	if rec.Code != http.StatusInternalServerError || !strings.Contains(rec.Body.String(), "logout_failed") {
		t.Fatalf("failed sign-out reported: %d %s", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Header().Get("Set-Cookie"), "Max-Age=0") {
		t.Fatal("cookie cleared although the login was not deleted")
	}
}

func TestHerdrActionsPerSessionAreLimited(t *testing.T) {
	h := herdrSockets{path: map[string]string{}, busy: map[string]int{}}
	var releases []func()
	for i := 0; i < maxHerdrOpsPerSession; i++ {
		rel, ok := h.acquire("s1")
		if !ok {
			t.Fatalf("slot %d refused", i)
		}
		releases = append(releases, rel)
	}
	if _, ok := h.acquire("s1"); ok {
		t.Fatal("more herdr actions in flight than allowed")
	}
	if rel, ok := h.acquire("s2"); !ok {
		t.Fatal("another session was limited too")
	} else {
		rel()
	}
	releases[0]()
	if _, ok := h.acquire("s1"); !ok {
		t.Fatal("slot not released")
	}
}
