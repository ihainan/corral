package web

// Host edits against opens and connections made under older settings.

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"syscall"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/ihainan/corral/internal/auth"
	"github.com/ihainan/corral/internal/testutil"
)

// holdAuth makes the test SSH server hold the next password check until
// the returned release is called; entered is closed when it is reached.
func holdAuth(t *testing.T, srv interface{ BeforeAuth(func()) }) (entered chan struct{}, release func()) {
	entered = make(chan struct{})
	gate := make(chan struct{})
	var once, reached sync.Once
	srv.BeforeAuth(func() {
		first := false
		reached.Do(func() { first = true; close(entered) })
		if first {
			<-gate
		}
	})
	release = func() { once.Do(func() { close(gate) }) }
	t.Cleanup(release)
	return entered, release
}

func (c *client) doAsync(method, path string, body any) chan resp {
	ch := make(chan resp, 1)
	go func() { ch <- c.do(method, path, body) }()
	return ch
}

func notYet(t *testing.T, ch chan resp, what string) {
	t.Helper()
	select {
	case r := <-ch:
		t.Fatalf("%s did not wait: %d %s", what, r.Status, r.Body)
	case <-time.After(300 * time.Millisecond):
	}
}

func awaitResp(t *testing.T, ch chan resp, what string) resp {
	t.Helper()
	select {
	case r := <-ch:
		return r
	case <-time.After(5 * time.Second):
		t.Fatalf("%s never finished", what)
		return resp{}
	}
}

// An edit waits for an open in progress: the open never pairs the old
// address with the new password (or re-pins a machine under new settings).
func TestHostEditWaitsForAnOpenInProgress(t *testing.T) {
	h := newHarness(t)
	admin := h.admin()
	srv := h.sshServer(t.TempDir())
	id := h.hostFor(admin, srv.Addr.Port)
	entered, release := holdAuth(t, srv)
	open := admin.doAsync("POST", "/api/sessions", map[string]any{"host_id": id})
	<-entered // the open has read the host and its password, and is authenticating
	edit := admin.doAsync("PUT", fmt.Sprintf("/api/hosts/%d", id), map[string]any{"name": "box", "address": "127.0.0.1",
		"port": srv.Addr.Port, "username": "alice", "auth": "password", "password": "a-new-password"})
	notYet(t, edit, "host edit during an open")
	release()
	mustStatus(t, awaitResp(t, open, "open"), 201, "open with the settings it read")
	mustStatus(t, awaitResp(t, edit, "edit"), 200, "edit after the open")
}

// Opens waiting for their turn end with the login.
func TestWaitingOpenEndsWithSignOut(t *testing.T) {
	h := newHarness(t)
	admin := h.admin()
	srv := h.sshServer(t.TempDir())
	id := h.hostFor(admin, srv.Addr.Port)
	entered, release := holdAuth(t, srv)
	first := admin.doAsync("POST", "/api/sessions", map[string]any{"host_id": id})
	<-entered
	other := h.login("root") // another browser of the same user
	waiting := other.doAsync("POST", "/api/sessions", map[string]any{"host_id": id})
	notYet(t, waiting, "second open")
	mustStatus(t, other.do("POST", "/api/logout", nil), 200, "sign out the waiting browser")
	if r := awaitResp(t, waiting, "waiting open after sign-out"); r.Status != http.StatusUnauthorized {
		t.Fatalf("waiting open after sign-out: %d %s", r.Status, r.Body)
	}
	release()
	mustStatus(t, awaitResp(t, first, "first open"), 201, "first open")
}

// Changing only the saved password is a new connection version: a running
// session is not handed out for it, and terminals and pooled connections made
// before are not used for forwarding.
func TestPasswordChangeRetiresOldConnections(t *testing.T) {
	e := newForwardEnv(t, echoApp)
	// A forwarding connection of its own, and a terminal running.
	f, _ := e.h.openForward(e.owner, e.id)
	if res, _ := f.get("/x", nil); res.StatusCode != 200 {
		t.Fatal(res.StatusCode)
	}
	a := e.owner.do("POST", "/api/sessions", map[string]any{"host_id": e.hostID})
	mustStatus(t, a, 201, "terminal")
	edit := func(password string) {
		mustStatus(t, e.owner.do("PUT", fmt.Sprintf("/api/hosts/%d", e.hostID), map[string]any{"name": "box", "address": "127.0.0.1",
			"port": e.srv.Addr.Port, "username": "alice", "auth": "password", "password": password}), 200, "edit password")
	}
	edit("alice-pass") // the same password typed again still counts as a change
	b := e.owner.do("POST", "/api/sessions", map[string]any{"host_id": e.hostID})
	mustStatus(t, b, 201, "open after a password edit")
	if a.json()["id"] == b.json()["id"] {
		t.Fatal("a session made before the password edit was reused")
	}
	// Now a wrong password: forwarding must dial with it (and fail), not ride
	// on the terminals or the pooled connection made before.
	e.srv.HandleExec(func(cmd string) func(ssh.Channel) {
		if cmd != listenCommand {
			return nil
		}
		return func(ch ssh.Channel) {
			ch.Write([]byte("LISTEN 0 4096 127.0.0.1:8080 0.0.0.0:*\n"))
			ch.SendRequest("exit-status", false, []byte{0, 0, 0, 0})
		}
	})
	edit("wrong-password")
	f, _ = e.h.openForward(e.owner, e.id) // the edit ended the old grant too
	if res, _ := f.get("/x", nil); res.StatusCode == 200 {
		t.Fatal("forwarding used a connection made with the old password")
	}
	r := e.owner.do("GET", fmt.Sprintf("/api/hosts/%d/ports", e.hostID), nil)
	if r.Status == 200 {
		t.Fatalf("port listing used a terminal made with the old password: %s", r.Body)
	}
}

// A pooled connection of an older version is retired, not handed out.
func TestPoolRetiresOlderVersions(t *testing.T) {
	srv := testutil.StartSSHServer(t, "alice", "alice-pass", t.TempDir())
	f := newForwarder()
	defer f.shutdown()
	key := [2]int64{1, 2}
	noop := func() {}
	_, d, _, _ := f.acquire(key, 1, noop)
	old := sshClient(t, srv)
	if _, err := f.finishDial(d, old, 1, nil); err != nil {
		t.Fatal(err)
	}
	fc, d2, lead, err := f.acquire(key, 2, noop)
	if err != nil || fc != nil || !lead || d2 == nil {
		t.Fatal("an older pooled connection was handed out for a newer version")
	}
	if !closedSoon(old) {
		t.Fatal("retired connection left open")
	}
	f.finishDial(d2, sshClient(t, srv), 2, nil)
}

// Errors checking the login are not reported as signed out.
func TestOpenReportsLoginCheckErrors(t *testing.T) {
	h := newHarness(t)
	admin := h.admin()
	u, _ := h.store.UserByName(context.Background(), "root")
	h.store.Close() // the check cannot be made
	ctx := context.WithValue(context.WithValue(context.Background(), userKey, u), loginKey, auth.HashToken(admin.cookie))
	rec := httptest.NewRecorder()
	h.srv.openSession(ctx, rec, httptest.NewRequest("POST", "/api/sessions", nil).WithContext(ctx), &openBody{HostID: 1})
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("login check failure: %d %s", rec.Code, rec.Body)
	}
}

// The turn is over before the response is written.
func TestHostGateIsFreeBeforeTheResponse(t *testing.T) {
	h := newHarness(t)
	resp, err := h.srv.underHostGate(context.Background(), 1, 2, func(w http.ResponseWriter) {
		writeError(w, http.StatusConflict, "x", "y")
	})
	if err != nil || resp.status != http.StatusConflict || h.srv.hostGate.size() != 0 {
		t.Fatalf("%v %+v gate %d", err, resp, h.srv.hostGate.size())
	}
	rec := httptest.NewRecorder()
	resp.writeTo(rec)
	if rec.Code != http.StatusConflict || rec.Header().Get("Content-Type") == "" {
		t.Fatalf("buffered response: %d %v", rec.Code, rec.Header())
	}
}

// Deleting a host stops VS Code there (best effort, after the delete).
func TestDeletingAHostStopsItsCodeServer(t *testing.T) {
	e := newCodeEnv(t, false)
	if st, _ := e.start(e.owner); st["state"] != codeReady {
		t.Fatalf("start: %v", st)
	}
	pid := e.pid()
	if pid <= 0 {
		t.Fatal("no pid recorded")
	}
	mustStatus(t, e.owner.do("DELETE", fmt.Sprintf("/api/hosts/%d", e.hostID), nil), 200, "delete host")
	eventually(t, "code-server kept running on a deleted host", func() bool { return syscall.Kill(pid, 0) != nil })
}
