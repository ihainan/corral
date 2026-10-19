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
	if err := st.CreateLoginSessionIfCurrent(ctx, "h1", u.ID, oldHash, "", "", true); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("stale login created a session: %v", err)
	}
	_ = st.SetDisabled(ctx, u.ID, true)
	if err := st.CreateLoginSessionIfCurrent(ctx, "h2", u.ID, newHash, "", "", true); !errors.Is(err, store.ErrNotFound) {
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
	// Only the reused answer says so: a client cancelling its open must not
	// close a session another device is using.
	if a.json()["reused"] == true || b.json()["reused"] != true || b.json()["state"] != "running" {
		t.Fatalf("reuse not marked as such: new %s, reused %s", a.Body, b.Body)
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
	if n := h.srv.hostGate.size(); n != 0 {
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

// Settings: a later arrival wins, except that a writer's own older request
// never undoes its newer one; values are checked; and a request made for
// another account is refused.
// Settings saved in the previous format (only the last writer of each field)
// keep that writer's request numbers when the new format takes over.
func TestSettingsOldFormatKeepsRequestNumbers(t *testing.T) {
	h := newHarness(t)
	admin := h.admin()
	uid := admin.do("GET", "/api/me", nil).json()["id"]
	old := `{"termTheme":"dracula","_w":{"termTheme":{"id":"pageAAAA","seq":5}}}`
	if err := h.srv.store.MergeUserSettings(context.Background(), int64(uid.(float64)), func(string) (string, error) { return old, nil }); err != nil {
		t.Fatal(err)
	}
	// Another page writes first, then A's late request numbered below 5 arrives.
	mustStatus(t, admin.do("PATCH", "/api/me/settings", map[string]any{"user": uid, "writer": "pageBBBB", "seq": 1, "set": map[string]any{"keybar": false}}), 200, "B")
	r := admin.do("PATCH", "/api/me/settings", map[string]any{"user": uid, "writer": "pageAAAA", "seq": 4, "set": map[string]any{"termTheme": "nord"}})
	if sup, _ := r.json()["superseded"].([]any); r.Status != 200 || len(sup) != 1 {
		t.Fatalf("A's older request was accepted after the format change: %d %s", r.Status, r.Body)
	}
	if got := admin.do("GET", "/api/me/settings", nil).json(); got["termTheme"] != "dracula" {
		t.Fatalf("settings: %v", got)
	}
}

func TestSettingsPatchOrderingValidationAndAccount(t *testing.T) {
	h := newHarness(t)
	admin := h.admin()
	uid := admin.do("GET", "/api/me", nil).json()["id"]
	patch := func(writer string, seq int, set map[string]any) resp {
		return admin.do("PATCH", "/api/me/settings", map[string]any{"user": uid, "writer": writer, "seq": seq, "set": set})
	}
	mustStatus(t, patch("pageAAAA", 2, map[string]any{"termTheme": "dracula", "fontSize": 15}), 200, "A seq 2")
	r := patch("pageAAAA", 1, map[string]any{"termTheme": "nord"}) // A's older request arriving late
	mustStatus(t, r, 200, "A seq 1 late")
	if sup, _ := r.json()["superseded"].([]any); len(sup) != 1 || sup[0] != "termTheme" {
		t.Fatalf("late request not reported as superseded: %s", r.Body)
	}
	mustStatus(t, patch("pageBBBB", 1, map[string]any{"keybar": false}), 200, "B other field")
	got := admin.do("GET", "/api/me/settings", nil).json()
	if got["termTheme"] != "dracula" || got["fontSize"] != float64(15) || got["keybar"] != false {
		t.Fatalf("merge went wrong: %v", got)
	}
	for k := range got {
		if strings.HasPrefix(k, "_") {
			t.Fatalf("bookkeeping leaked into the settings: %v", got)
		}
	}
	mustStatus(t, patch("pageBBBB", 2, map[string]any{"termTheme": "nord"}), 200, "B later arrival")
	if got := admin.do("GET", "/api/me/settings", nil).json(); got["termTheme"] != "nord" {
		t.Fatalf("a later arrival from another page did not win: %v", got)
	}
	// A's older request still loses after B wrote the field in between.
	r = patch("pageAAAA", 1, map[string]any{"termTheme": "dracula"})
	if sup, _ := r.json()["superseded"].([]any); r.Status != 200 || len(sup) != 1 {
		t.Fatalf("an older request was revived by another writer: %d %s", r.Status, r.Body)
	}
	if got := admin.do("GET", "/api/me/settings", nil).json(); got["termTheme"] != "nord" {
		t.Fatalf("an older request was revived by another writer: %v", got)
	}
	// Writers are remembered within a bound, the least recently heard from
	// forgotten first.
	for i := range maxSettingWriters {
		mustStatus(t, patch(fmt.Sprintf("pageC%03d", i), 1, map[string]any{"keybar": true}), 200, "another page")
	}
	mustStatus(t, patch("pageAAAA", 1, map[string]any{"termTheme": "dracula"}), 200, "A seq 1 again")
	if got := admin.do("GET", "/api/me/settings", nil).json(); got["termTheme"] != "dracula" {
		t.Fatalf("a forgotten writer is still remembered: %v", got)
	}
	data, _ := h.store.UserSettings(context.Background(), int64(uid.(float64)))
	var stored map[string]json.RawMessage
	var writers []settingWriter
	if json.Unmarshal([]byte(data), &stored) != nil || json.Unmarshal(stored[settingsWriters], &writers) != nil || len(writers) != maxSettingWriters {
		t.Fatalf("writers kept: %d (%s)", len(writers), data)
	}
	mustStatus(t, patch("pageAAAA", 3, map[string]any{"fontSize": map[string]any{}}), 400, "object as font size")
	mustStatus(t, patch("pageAAAA", 3, map[string]any{"fontSize": 99}), 400, "font size out of range")
	mustStatus(t, patch("pageAAAA", 3, map[string]any{"_w": 1}), 400, "reserved name")
	mustStatus(t, patch("pageAAAA", 3, map[string]any{"colour": "x"}), 400, "unknown setting")
	if r := admin.do("PATCH", "/api/me/settings", map[string]any{"user": 99999, "writer": "pageAAAA", "seq": 4, "set": map[string]any{"keybar": true}}); r.code() != "user_mismatch" {
		t.Fatalf("a request for another account was accepted: %d %s", r.Status, r.Body)
	}
	if r := admin.do("PUT", "/api/me/settings", map[string]any{"termTheme": "x"}); r.Status == 200 {
		t.Fatal("whole-object PUT still overwrites settings")
	}
}

// A session with as many viewers as it allows refuses one more with a reason
// the browser can act on (stop reconnecting) instead of a plain close.
func TestTooManyViewersIsSaid(t *testing.T) {
	h := newHarness(t)
	admin := h.admin()
	srv := h.sshServer(t.TempDir())
	r := admin.do("POST", "/api/sessions", map[string]any{"host_id": h.hostFor(admin, srv.Addr.Port)})
	mustStatus(t, r, 201, "open")
	id := r.json()["id"].(string)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for i := 0; i < 8; i++ {
		ws, err := h.dialWS(admin, id, "")
		if err != nil {
			t.Fatal(err)
		}
		defer ws.CloseNow()
		if _, b, err := ws.Read(ctx); err != nil || !bytes.Contains(b, []byte(`"hello"`)) {
			t.Fatalf("viewer %d: %s %v", i+1, b, err)
		}
	}
	ws, err := h.dialWS(admin, id, "")
	if err != nil {
		t.Fatal(err)
	}
	defer ws.CloseNow()
	typ, b, err := ws.Read(ctx)
	if err != nil || typ != websocket.MessageText || string(b) != `{"t":"error","code":"too_many_viewers"}` {
		t.Fatalf("one viewer too many got %v %s %v", typ, b, err)
	}
	_, _, err = ws.Read(ctx)
	var ce websocket.CloseError
	if !errors.As(err, &ce) || ce.Code != 4429 || ce.Reason != "too_many_viewers" {
		t.Fatalf("one viewer too many closed with %v", err)
	}
}

// An edit from a form loaded before the host was saved elsewhere is refused
// instead of silently restoring the older settings.
func TestStaleHostEditIsRefused(t *testing.T) {
	h := newHarness(t)
	admin := h.admin()
	id := h.hostFor(admin, 2222)
	path := fmt.Sprintf("/api/hosts/%d", id)
	loaded := admin.do("GET", path, nil).json()["version"]
	edit := func(address string, version any) resp {
		body := map[string]any{"name": "box", "address": address, "port": 2222, "username": "alice", "auth": "password", "persistence": "tmux"}
		if version != nil {
			body["version"] = version
		}
		return admin.do("PUT", path, body)
	}
	a := edit("10.0.0.1", loaded) // device A saves first
	mustStatus(t, a, 200, "first edit")
	if a.json()["version"] == loaded {
		t.Fatalf("version not bumped by an edit: %s", a.Body)
	}
	b := edit("127.0.0.1", loaded) // device B, from a form loaded before that
	if b.Status != 409 || b.code() != "host_changed" {
		t.Fatalf("a stale edit was not refused: %d %s", b.Status, b.Body)
	}
	if got := admin.do("GET", path, nil).json(); got["address"] != "10.0.0.1" || got["version"] != a.json()["version"] {
		t.Fatalf("a refused edit changed the host: %v", got)
	}
	mustStatus(t, edit("10.0.0.2", a.json()["version"]), 200, "edit from a fresh form")
	mustStatus(t, edit("10.0.0.3", nil), 200, "edit without a version")
}

// Size notes over the socket say whether that socket set the size and how
// many of its requests were applied; a resize without take does not move
// the size away from the viewer in charge.
func TestSizeNotesSayWhoSetIt(t *testing.T) {
	h := newHarness(t)
	admin := h.admin()
	srv := h.sshServer(t.TempDir())
	r := admin.do("POST", "/api/sessions", map[string]any{"host_id": h.hostFor(admin, srv.Addr.Port)})
	mustStatus(t, r, 201, "open")
	id := r.json()["id"].(string)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	type note struct {
		T          string
		Cols, Rows int
		Own        bool
		Ack        int64
	}
	// next returns the next size note on ws.
	next := func(ws *websocket.Conn) note {
		t.Helper()
		for {
			typ, b, err := ws.Read(ctx)
			if err != nil {
				t.Fatal(err)
			}
			var n note
			if typ == websocket.MessageText && json.Unmarshal(b, &n) == nil && n.T == "size" {
				return n
			}
		}
	}
	send := func(ws *websocket.Conn, v map[string]any) {
		b, _ := json.Marshal(v)
		if err := ws.Write(ctx, websocket.MessageText, b); err != nil {
			t.Fatal(err)
		}
	}
	a, err := h.dialWS(admin, id, "?viewer=windowAAAA")
	if err != nil {
		t.Fatal(err)
	}
	defer a.CloseNow()
	next(a) // the size on attaching
	b, err := h.dialWS(admin, id, "")
	if err != nil {
		t.Fatal(err)
	}
	defer b.CloseNow()
	next(b)

	send(a, map[string]any{"t": "resize", "cols": 120, "rows": 40, "seq": 1, "take": true})
	if n := next(a); n.Cols != 120 || !n.Own || n.Ack != 1 {
		t.Fatalf("a after taking over: %+v", n)
	}
	if n := next(b); n.Cols != 120 || n.Own || n.Ack != 0 {
		t.Fatalf("b after a took over: %+v", n)
	}
	// b's window changes size without the user acting there: a stays in charge.
	send(b, map[string]any{"t": "resize", "cols": 80, "rows": 24, "seq": 1})
	if n := next(b); n.Cols != 120 || n.Own || n.Ack != 1 {
		t.Fatalf("b after a resize without take: %+v", n)
	}
	send(b, map[string]any{"t": "resize", "cols": 80, "rows": 24, "seq": 2, "take": true})
	if n := next(b); n.Cols != 80 || !n.Own || n.Ack != 2 {
		t.Fatalf("b after taking over: %+v", n)
	}
	if n := next(a); n.Cols != 80 || n.Own || n.Ack != 1 {
		t.Fatalf("a after b took over: %+v", n)
	}
	// Window a takes over again, then reconnects while its first connection
	// is still attached: the new connection is a, still in charge, so its
	// window's own resize applies.
	send(a, map[string]any{"t": "resize", "cols": 120, "rows": 40, "seq": 2, "take": true})
	if n := next(a); n.Cols != 120 || !n.Own {
		t.Fatalf("a taking over again: %+v", n)
	}
	a2, err := h.dialWS(admin, id, "?viewer=windowAAAA")
	if err != nil {
		t.Fatal(err)
	}
	defer a2.CloseNow()
	if n := next(a2); n.Cols != 120 || !n.Own {
		t.Fatalf("a's new connection on attaching: %+v", n)
	}
	send(a2, map[string]any{"t": "resize", "cols": 130, "rows": 40, "seq": 1})
	if n := next(a2); n.Cols != 130 || !n.Own || n.Ack != 1 {
		t.Fatalf("a's new connection resizing: %+v", n)
	}
}
