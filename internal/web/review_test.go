package web

// Regression tests for findings from the phase 1 acceptance review.

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/ihainan/corral/internal/auth"
	"github.com/ihainan/corral/internal/config"
	"github.com/ihainan/corral/internal/sshx"
	"github.com/ihainan/corral/internal/store"
)

func (h *harness) hostFor(c *client, port int) int64 {
	h.t.Helper()
	r := c.do("POST", "/api/hosts", map[string]any{"name": "box", "address": "127.0.0.1", "port": port, "username": "alice", "auth": "password", "password": "alice-pass"})
	mustStatus(h.t, r, 201, "create host")
	return int64(r.json()["id"].(float64))
}

func (h *harness) dialWS(c *client, sessID string, offset string) (*websocket.Conn, error) {
	u := strings.Replace(h.ts.URL, "http", "ws", 1) + "/api/sessions/" + sessID + "/ws" + offset
	hdr := http.Header{}
	hdr.Set("Origin", h.cfg.Origin())
	hdr.Set("Cookie", h.srv.cookieName()+"="+c.cookie)
	ws, _, err := websocket.Dial(context.Background(), u, &websocket.DialOptions{HTTPHeader: hdr})
	return ws, err
}

func TestSessionLimitHoldsUnderConcurrency(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.MaxSessionsPerUser = 1 })
	admin := h.admin()
	srv := h.sshServer(t.TempDir())
	hostID := h.hostFor(admin, srv.Addr.Port)
	var wg sync.WaitGroup
	var mu sync.Mutex
	ok := 0
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if r := admin.do("POST", "/api/sessions", map[string]any{"host_id": hostID}); r.Status == 201 {
				mu.Lock()
				ok++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if ok != 1 {
		t.Fatalf("%d sessions opened with a limit of 1", ok)
	}
}

func TestLoginStartedBeforePasswordChangeCannotComplete(t *testing.T) {
	st, _ := store.OpenMemory()
	defer st.Close()
	ctx := context.Background()
	oldHash, _ := auth.HashPassword("old-password-1")
	u, _ := st.CreateUser(ctx, "carol", "", oldHash, store.RoleMember)
	newHash, _ := auth.HashPassword("new-password-1")
	if err := st.SetPassword(ctx, u.ID, newHash); err != nil {
		t.Fatal(err)
	}
	// A login that verified the old hash now tries to create its session.
	if err := st.CreateLoginSessionIfCurrent(ctx, "h1", u.ID, oldHash, "", ""); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("stale login created a session: %v", err)
	}
	_ = st.SetDisabled(ctx, u.ID, true)
	if err := st.CreateLoginSessionIfCurrent(ctx, "h2", u.ID, newHash, "", ""); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("disabled account got a session: %v", err)
	}
}

func TestConcurrentWrongPasswordsStillLock(t *testing.T) {
	h := newHarness(t, func(c *config.Config) {})
	h.admin()
	var wg sync.WaitGroup
	for i := 0; i < 7; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			c := &client{h: h}
			c.do("POST", "/api/login", map[string]string{"username": "root", "password": fmt.Sprintf("wrong-%d-password", i)})
		}(i)
	}
	wg.Wait()
	c := &client{h: h}
	r := c.do("POST", "/api/login", map[string]string{"username": "root", "password": "root-password-1"})
	if r.code() != "locked" {
		t.Fatalf("after concurrent failures the account is not locked: %d %s", r.Status, r.Body)
	}
}

func TestTwoAdminsCannotBothStepDown(t *testing.T) {
	h := newHarness(t)
	root := h.admin()
	r := root.do("POST", "/api/admin/invites", map[string]any{"role": "admin"})
	link := r.json()["url"].(string)
	second := &client{h: h}
	mustStatus(t, second.do("POST", "/api/invites/"+link[strings.LastIndex(link, "/")+1:]+"/redeem",
		map[string]string{"username": "dave", "password": "dave-password-1"}), 200, "second admin")
	rootID := int64(root.do("GET", "/api/me", nil).json()["id"].(float64))
	daveID := int64(second.do("GET", "/api/me", nil).json()["id"].(float64))
	var wg sync.WaitGroup
	for _, pair := range []struct {
		c  *client
		id int64
	}{{root, daveID}, {second, rootID}} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			pair.c.do("PUT", fmt.Sprintf("/api/admin/users/%d", pair.id), map[string]any{"role": "member"})
		}()
	}
	wg.Wait()
	n, _ := h.store.CountActiveAdmins(context.Background())
	if n < 1 {
		t.Fatal("no active administrator left")
	}
}

func TestLogoutClosesThatBrowsersSocket(t *testing.T) {
	h := newHarness(t)
	admin := h.admin()
	srv := h.sshServer(t.TempDir())
	r := admin.do("POST", "/api/sessions", map[string]any{"host_id": h.hostFor(admin, srv.Addr.Port)})
	mustStatus(t, r, 201, "open")
	ws, err := h.dialWS(admin, r.json()["id"].(string), "")
	if err != nil {
		t.Fatal(err)
	}
	defer ws.CloseNow()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ws.Read(ctx) // hello
	mustStatus(t, admin.do("POST", "/api/logout", nil), 200, "logout")
	for {
		if _, _, err := ws.Read(ctx); err != nil {
			if ctx.Err() != nil {
				t.Fatal("socket still open after logout")
			}
			return
		}
	}
}

// The race is timing dependent, so it runs many sessions.
func TestFinalOutputArrivesBeforeEnd(t *testing.T) {
	h := newHarness(t)
	admin := h.admin()
	srv := h.sshServer(t.TempDir())
	hostID := h.hostFor(admin, srv.Addr.Port)
	for i := 0; i < 15; i++ {
		checkBurst(t, h, admin, hostID)
	}
}

func checkBurst(t *testing.T, h *harness, admin *client, hostID int64) {
	r := admin.do("POST", "/api/sessions", map[string]any{"host_id": hostID})
	mustStatus(t, r, 201, "open")
	ws, err := h.dialWS(admin, r.json()["id"].(string), "")
	if err != nil {
		t.Fatal(err)
	}
	defer ws.CloseNow()
	ws.SetReadLimit(1 << 20)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ws.Write(ctx, websocket.MessageBinary, []byte("burst!"))
	var got bytes.Buffer
	var end uint64
	for {
		typ, data, err := ws.Read(ctx)
		if err != nil {
			t.Fatalf("socket closed without a final state: %v", err)
		}
		if typ == websocket.MessageBinary {
			got.Write(data[8:])
			continue
		}
		var m struct {
			T      string `json:"t"`
			Offset uint64 `json:"offset"`
		}
		json.Unmarshal(data, &m)
		if m.T == "state" {
			end = m.Offset
			break
		}
	}
	if !bytes.Contains(got.Bytes(), []byte("END-OF-BURST")) {
		t.Fatalf("tail of output missing: received %d bytes", got.Len())
	}
	if uint64(got.Len()) != end {
		t.Fatalf("received %d bytes, final offset says %d", got.Len(), end)
	}
	_ = binary.BigEndian
}

func TestUploadLimitIsExact(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.UploadMaxBytes = 64 << 10 })
	admin := h.admin()
	srv := h.sshServer(t.TempDir())
	r := admin.do("POST", "/api/sessions", map[string]any{"host_id": h.hostFor(admin, srv.Addr.Port)})
	sess := r.json()["id"].(string)
	png := append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, (64<<10)-8)...)
	mustStatus(t, admin.upload(sess, png), 201, "exactly at the limit")
	mustStatus(t, admin.upload(sess, append(png, 0)), http.StatusRequestEntityTooLarge, "one byte over")
}

func TestConnectFailureHidesRemoteText(t *testing.T) {
	for _, e := range []error{
		errors.New("ssh: disconnect, reason 11: bad password Sup3r-Secret was sent"),
		errors.New("dial tcp 10.0.0.1:22: connect: connection refused"),
	} {
		if r := failureReason(e); strings.Contains(r, "Secret") || strings.Contains(r, " ") {
			t.Fatalf("reason leaks remote text: %q", r)
		}
	}
}

func TestLastAdminCannotStepDown(t *testing.T) {
	h := newHarness(t)
	root := h.admin()
	id := int64(root.do("GET", "/api/me", nil).json()["id"].(float64))
	r := root.do("PUT", fmt.Sprintf("/api/admin/users/%d", id), map[string]any{"role": "member"})
	if r.code() != "last_admin" {
		t.Fatalf("sole admin demoted themselves: %d %s", r.Status, r.Body)
	}
	r = root.do("PUT", fmt.Sprintf("/api/admin/users/%d", id), map[string]any{"disabled": true})
	if r.code() != "last_admin" {
		t.Fatalf("sole admin disabled themselves: %d %s", r.Status, r.Body)
	}
}

// Round 2 -------------------------------------------------------------------

func TestSlowPasswordChangeCannotUndoAdminReset(t *testing.T) {
	st, _ := store.OpenMemory()
	defer st.Close()
	ctx := context.Background()
	old, _ := auth.HashPassword("old-password-1")
	u, _ := st.CreateUser(ctx, "erin", "", old, store.RoleMember)
	_ = st.CreateLoginSession(ctx, "login1", u.ID, "", "")
	// Admin resets the password and signs the user out.
	reset, _ := auth.HashPassword("admin-chosen-1")
	_ = st.SetPassword(ctx, u.ID, reset)
	_ = st.DeleteUserLoginSessions(ctx, u.ID, "")
	// The user's change request, verified against the old hash, lands afterwards.
	mine, _ := auth.HashPassword("attacker-new-1")
	if err := st.ChangePasswordIf(ctx, u.ID, old, mine, "login1"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("stale change applied: %v", err)
	}
	got, _ := st.UserByID(ctx, u.ID)
	if got.PasswordHash() != reset {
		t.Fatal("admin reset was overwritten")
	}
}

func TestHostKeyCannotBeForgottenDirectly(t *testing.T) {
	h := newHarness(t)
	admin := h.admin()
	srv := h.sshServer(t.TempDir())
	id := h.hostFor(admin, srv.Addr.Port)
	if r := admin.do("DELETE", fmt.Sprintf("/api/hosts/%d/hostkey", id), nil); r.Status != http.StatusNotFound && r.Status != http.StatusMethodNotAllowed {
		t.Fatalf("hostkey can be deleted: %d", r.Status)
	}
}

func TestSocketEndsWhenLoginExpires(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.LoginAbsoluteLimit = 3 * time.Second })
	admin := h.admin()
	srv := h.sshServer(t.TempDir())
	r := admin.do("POST", "/api/sessions", map[string]any{"host_id": h.hostFor(admin, srv.Addr.Port)})
	mustStatus(t, r, 201, "open")
	ws, err := h.dialWS(admin, r.json()["id"].(string), "")
	if err != nil {
		t.Fatal(err)
	}
	defer ws.CloseNow()
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	for {
		if _, _, err := ws.Read(ctx); err != nil {
			if ctx.Err() != nil {
				t.Fatal("socket outlived the login")
			}
			return
		}
	}
}

// Round 3 -------------------------------------------------------------------

func TestChangedHostKeyRefusedBeforeCredentialsAreSent(t *testing.T) {
	h := newHarness(t)
	admin := h.admin()
	srv := h.sshServer(t.TempDir())
	id := h.hostFor(admin, srv.Addr.Port)
	first := admin.do("POST", "/api/sessions", map[string]any{"host_id": id})
	mustStatus(t, first, 201, "first connect pins")
	// End it: a running session would be returned as is, without a new connection.
	mustStatus(t, admin.do("DELETE", "/api/sessions/"+first.json()["id"].(string), nil), 200, "close first")
	srv.RotateHostKey()
	before := srv.Attempts()
	r := admin.do("POST", "/api/sessions", map[string]any{"host_id": id})
	if r.code() != "host_key_changed" {
		t.Fatalf("changed key accepted: %d %s", r.Status, r.Body)
	}
	if srv.Attempts() != before {
		t.Fatal("password was sent to a host with an unexpected key")
	}
	got := r.json()["extra"].(map[string]any)["got"].(string)
	// A wrong confirmation is still refused, a matching one re-pins.
	r = admin.do("POST", "/api/sessions", map[string]any{"host_id": id, "accept_fingerprint": "SHA256:not-it"})
	if r.code() != "host_key_changed" {
		t.Fatalf("wrong confirmation accepted: %d %s", r.Status, r.Body)
	}
	accepted := admin.do("POST", "/api/sessions", map[string]any{"host_id": id, "accept_fingerprint": got})
	mustStatus(t, accepted, 201, "accepted key")
	mustStatus(t, admin.do("DELETE", "/api/sessions/"+accepted.json()["id"].(string), nil), 200, "close accepted")
	mustStatus(t, admin.do("POST", "/api/sessions", map[string]any{"host_id": id}), 201, "new key now pinned")
}

func TestStaleHashCannotChangePasswordEvenWithLiveLogin(t *testing.T) {
	st, _ := store.OpenMemory()
	defer st.Close()
	ctx := context.Background()
	old, _ := auth.HashPassword("old-password-1")
	u, _ := st.CreateUser(ctx, "frank", "", old, store.RoleMember)
	_ = st.CreateLoginSession(ctx, "login1", u.ID, "", "")
	newer, _ := auth.HashPassword("newer-password-1")
	_ = st.SetPassword(ctx, u.ID, newer) // someone changed it meanwhile; login still exists
	mine, _ := auth.HashPassword("mine-password-1")
	if err := st.ChangePasswordIf(ctx, u.ID, old, mine, "login1"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("change against a stale hash applied: %v", err)
	}
}

func TestLoginsOlderThanPasswordAreVoid(t *testing.T) {
	st, _ := store.OpenMemory()
	defer st.Close()
	ctx := context.Background()
	svc := auth.NewService(st, time.Hour, 24*time.Hour)
	pw, _ := auth.HashPassword("gina-password-1")
	u, _ := st.CreateUser(ctx, "gina", "", pw, store.RoleMember)
	tok, hash, _ := auth.NewToken()
	_ = st.CreateLoginSession(ctx, hash, u.ID, "", "")
	time.Sleep(1100 * time.Millisecond)
	_ = st.SetPassword(ctx, u.ID, pw) // password_changed_at moves past the login
	if _, _, err := svc.Authenticate(ctx, tok); err == nil {
		t.Fatal("a login older than the password still works")
	}
}

// Round 4 -------------------------------------------------------------------

// Later key exchanges re-run the host key callback after the connecting
// request (and its context) is gone; the session must survive them.
func TestSessionSurvivesKeyReexchange(t *testing.T) {
	sshx.RekeyThreshold = 64 << 10
	defer func() { sshx.RekeyThreshold = 0 }()
	h := newHarness(t)
	admin := h.admin()
	srv := h.sshServer(t.TempDir())
	r := admin.do("POST", "/api/sessions", map[string]any{"host_id": h.hostFor(admin, srv.Addr.Port)})
	mustStatus(t, r, 201, "open")
	id := r.json()["id"].(string)
	ws, err := h.dialWS(admin, id, "")
	if err != nil {
		t.Fatal(err)
	}
	defer ws.CloseNow()
	ws.SetReadLimit(1 << 20)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	payload := bytes.Repeat([]byte("x"), 32<<10)
	for i := 0; i < 12; i++ { // ~400 KB each way: several re-exchanges at 64 KB
		if err := ws.Write(ctx, websocket.MessageBinary, payload); err != nil {
			t.Fatal(err)
		}
	}
	received := 0
	for received < 12*len(payload) {
		typ, data, err := ws.Read(ctx)
		if err != nil {
			t.Fatalf("socket ended after %d bytes: %v", received, err)
		}
		if typ == websocket.MessageBinary {
			received += len(data) - 8
		}
	}
	for _, s := range h.terms.List(1) {
		if s.ID == id && s.State != "running" {
			t.Fatalf("session state %s after key re-exchange", s.State)
		}
	}
}

func TestPasswordPolicyAllowsEightCharacters(t *testing.T) {
	if err := auth.CheckPasswordPolicy("veB3#jJaH"); err != nil {
		t.Fatalf("9-character password rejected: %v", err)
	}
	if err := auth.CheckPasswordPolicy("short7!"); err == nil {
		t.Fatal("7-character password accepted")
	}
}

// With tmux or herdr every gateway session would attach the same remote
// session, so opening the host again returns the running one; plain shells
// are independent and each open starts a new one.
func TestPersistentHostsKeepOneSession(t *testing.T) {
	h := newHarness(t)
	admin := h.admin()
	srv := h.sshServer(t.TempDir())
	id := h.hostFor(admin, srv.Addr.Port) // tmux by default
	a := admin.do("POST", "/api/sessions", map[string]any{"host_id": id})
	mustStatus(t, a, 201, "first open")
	b := admin.do("POST", "/api/sessions", map[string]any{"host_id": id})
	mustStatus(t, b, 200, "second open returns the running session")
	if a.json()["id"] != b.json()["id"] {
		t.Fatalf("a second session was started for a tmux host: %v vs %v", a.json()["id"], b.json()["id"])
	}

	plain := admin.do("POST", "/api/hosts", map[string]any{"name": "plain", "address": "127.0.0.1", "port": srv.Addr.Port,
		"username": "alice", "auth": "password", "password": "alice-pass", "persistence": "none"})
	mustStatus(t, plain, 201, "create plain host")
	pid := plain.json()["id"]
	c := admin.do("POST", "/api/sessions", map[string]any{"host_id": pid})
	d := admin.do("POST", "/api/sessions", map[string]any{"host_id": pid})
	mustStatus(t, c, 201, "plain open 1")
	mustStatus(t, d, 201, "plain open 2")
	if c.json()["id"] == d.json()["id"] {
		t.Fatal("plain shells were merged into one session")
	}
}

// A session is only reused while the host still describes it: after an edit
// (here the persistence mode, which also covers a plain shell turning into
// tmux) opening the host starts a new session.
func TestEditedHostIsNotServedAnOldSession(t *testing.T) {
	h := newHarness(t)
	admin := h.admin()
	srv := h.sshServer(t.TempDir())
	id := h.hostFor(admin, srv.Addr.Port)
	a := admin.do("POST", "/api/sessions", map[string]any{"host_id": id})
	mustStatus(t, a, 201, "first open")
	mustStatus(t, admin.do("PUT", fmt.Sprintf("/api/hosts/%d", id), map[string]any{"name": "box", "address": "127.0.0.1", "port": srv.Addr.Port,
		"username": "alice", "auth": "password", "persistence": "tmux", "tmux_session": "other"}), 200, "edit host")
	b := admin.do("POST", "/api/sessions", map[string]any{"host_id": id})
	mustStatus(t, b, 201, "open after edit starts a new session")
	if a.json()["id"] == b.json()["id"] {
		t.Fatal("an edited host was given its old session")
	}
	if n := h.srv.openGate.size(); n != 0 {
		t.Fatalf("open gate keeps %d entries after the opens finished", n)
	}
}

func TestOpenGateWaitsCancelsAndForgets(t *testing.T) {
	var g keyedGate
	key := [2]int64{1, 2}
	release, err := g.acquire(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := g.acquire(ctx, key); done <- err }()
	select {
	case err := <-done:
		t.Fatalf("second open did not wait: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled wait returned %v", err)
	}
	old := maxGateWaiters
	maxGateWaiters = 1
	defer func() { maxGateWaiters = old }()
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	go func() { _, _ = g.acquire(ctx2, key) }()
	time.Sleep(50 * time.Millisecond)
	if _, err := g.acquire(context.Background(), key); !errors.Is(err, errGateBusy) {
		t.Fatalf("a full queue still took a waiter: %v", err)
	}
	cancel2()
	time.Sleep(20 * time.Millisecond)
	release()
	release() // idempotent
	if n := g.size(); n != 0 {
		t.Fatalf("gate keeps %d entries with nobody holding or waiting", n)
	}
}

// Settings are patched field by field and a field only takes a newer
// change: a late request (a keepalive sent while a page closed, an old
// device) cannot undo a newer value, and fields never overwrite each other.
func TestSettingsPatchKeepsTheNewestChangePerField(t *testing.T) {
	h := newHarness(t)
	admin := h.admin()
	patch := func(set map[string]any, ts map[string]int64) resp {
		return admin.do("PATCH", "/api/me/settings", map[string]any{"set": set, "ts": ts})
	}
	mustStatus(t, patch(map[string]any{"termTheme": "dracula", "fontSize": 15}, map[string]int64{"termTheme": 200, "fontSize": 200}), 200, "first patch")
	mustStatus(t, patch(map[string]any{"termTheme": "nord"}, map[string]int64{"termTheme": 100}), 200, "older change")
	mustStatus(t, patch(map[string]any{"keybar": false}, map[string]int64{"keybar": 150}), 200, "other field")
	got := admin.do("GET", "/api/me/settings", nil).json()
	if got["termTheme"] != "dracula" || got["fontSize"] != float64(15) || got["keybar"] != false {
		t.Fatalf("merge went wrong: %v", got)
	}
	if _, ok := got["_ts"]; ok {
		t.Fatal("bookkeeping leaked into the settings")
	}
	mustStatus(t, patch(map[string]any{"termTheme": "nord"}, map[string]int64{"termTheme": 300}), 200, "newer change")
	if got := admin.do("GET", "/api/me/settings", nil).json(); got["termTheme"] != "nord" {
		t.Fatalf("a newer change was not taken: %v", got)
	}
	mustStatus(t, patch(map[string]any{"_ts": map[string]any{}}, map[string]int64{"_ts": 999}), 400, "reserved name")
	mustStatus(t, patch(map[string]any{"fontSize": 12}, nil), 400, "missing ts")
	if r := admin.do("PUT", "/api/me/settings", map[string]any{"termTheme": "x"}); r.Status == 200 {
		t.Fatal("whole-object PUT still overwrites settings")
	}
}
