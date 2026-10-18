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
	mustStatus(t, admin.do("POST", "/api/sessions", map[string]any{"host_id": id}), 201, "first connect pins")
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
	mustStatus(t, admin.do("POST", "/api/sessions", map[string]any{"host_id": id, "accept_fingerprint": got}), 201, "accepted key")
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
