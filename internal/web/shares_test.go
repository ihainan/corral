package web

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func (h *harness) dialWatch(c *client, sessID string) (*websocket.Conn, error) {
	u := strings.Replace(h.ts.URL, "http", "ws", 1) + "/api/shared/" + sessID + "/ws"
	hdr := http.Header{}
	hdr.Set("Origin", h.cfg.Origin())
	hdr.Set("Cookie", h.srv.cookieName()+"="+c.cookie)
	ws, _, err := websocket.Dial(context.Background(), u, &websocket.DialOptions{HTTPHeader: hdr})
	return ws, err
}

// readOutput collects terminal output from a socket until it holds want or
// the time is up; it returns what came and how the socket closed (if it did).
func readOutput(ws *websocket.Conn, want string, limit time.Duration) (string, websocket.StatusCode) {
	ctx, cancel := context.WithTimeout(context.Background(), limit)
	defer cancel()
	var out bytes.Buffer
	for {
		typ, data, err := ws.Read(ctx)
		if err != nil {
			return out.String(), websocket.CloseStatus(err)
		}
		if typ == websocket.MessageBinary && len(data) >= 8 {
			out.Write(data[8:])
			if want != "" && strings.Contains(out.String(), want) {
				return out.String(), -1
			}
		}
	}
}

func TestWatchingIsReadOnly(t *testing.T) {
	h := newHarness(t)
	admin := h.admin()
	bob := h.member(admin, "bob")
	carol := h.member(admin, "carol")
	dan := h.member(admin, "dan")
	srv := h.sshServer(t.TempDir())
	r := bob.do("POST", "/api/sessions", map[string]any{"host_id": h.hostFor(bob, srv.Addr.Port)})
	mustStatus(t, r, 201, "open")
	id := r.json()["id"].(string)

	// Not shared yet: nothing to see.
	mustStatus(t, carol.do("GET", "/api/shared/"+id, nil), 404, "before sharing")
	if _, err := h.dialWatch(carol, id); err == nil {
		t.Fatal("watch socket before sharing")
	}
	// Grants: not to oneself, not to unknown or disabled accounts.
	mustStatus(t, bob.do("POST", "/api/sessions/"+id+"/shares", map[string]string{"username": "bob"}), 400, "self")
	mustStatus(t, bob.do("POST", "/api/sessions/"+id+"/shares", map[string]string{"username": "nobody"}), 404, "unknown")
	mustStatus(t, admin.do("PUT", "/api/admin/users/"+strconv.FormatInt(h.userID(dan), 10), map[string]any{"disabled": true}), 200, "disable dan")
	mustStatus(t, bob.do("POST", "/api/sessions/"+id+"/shares", map[string]string{"username": "dan"}), 404, "disabled")
	mustStatus(t, carol.do("POST", "/api/sessions/"+id+"/shares", map[string]string{"username": "carol"}), 404, "not the owner")
	mustStatus(t, bob.do("POST", "/api/sessions/"+id+"/shares", map[string]string{"username": "carol"}), 200, "share with carol")

	list := carol.do("GET", "/api/shared", nil).jsonArray()
	if len(list) != 1 || list[0]["id"] != id || list[0]["owner_name"] != "bob" {
		t.Fatalf("carol's shared list: %v", list)
	}
	// carol watches: sees output, her input and size do not reach the host.
	cw, err := h.dialWatch(carol, id)
	if err != nil {
		t.Fatal(err)
	}
	defer cw.CloseNow()
	cw.Write(t.Context(), websocket.MessageBinary, []byte("from-carol\n"))
	b, _ := json.Marshal(map[string]any{"t": "resize", "cols": 33, "rows": 11, "take": true, "seq": 1})
	cw.Write(t.Context(), websocket.MessageText, b)
	bw, err := h.dialWS(bob, id, "")
	if err != nil {
		t.Fatal(err)
	}
	defer bw.CloseNow()
	bw.Write(t.Context(), websocket.MessageBinary, []byte("from-bob\n"))
	got, _ := readOutput(cw, "from-bob", 3*time.Second)
	if !strings.Contains(got, "from-bob") {
		t.Fatalf("watcher did not see the owner's output: %q", got)
	}
	if strings.Contains(got, "from-carol") {
		t.Fatal("the watcher's input reached the host")
	}
	for _, s := range bob.do("GET", "/api/sessions", nil).jsonArray() {
		if s["id"] == id && (s["cols"] == float64(33) || s["watching"] != float64(1) || s["viewers"] != float64(1)) {
			t.Fatalf("session after watching: %v", s)
		}
	}
	// The owner's other features stay the owner's.
	mustStatus(t, carol.do("GET", "/api/sessions/"+id+"/files", nil), 404, "files")
	mustStatus(t, carol.do("GET", "/api/sessions/"+id+"/shares", nil), 404, "shares list")

	shares := bob.do("GET", "/api/sessions/"+id+"/shares", nil).jsonArray()
	if len(shares) != 1 || shares[0]["username"] != "carol" || shares[0]["watching"] != true {
		t.Fatalf("shares: %v", shares)
	}
	// Taking the leave back closes carol's window, and keeps her out.
	mustStatus(t, bob.do("DELETE", "/api/sessions/"+id+"/shares/"+strconv.FormatInt(h.userID(carol), 10), nil), 200, "unshare")
	if _, code := readOutput(cw, "", 3*time.Second); code != statusRevoked {
		t.Fatalf("watcher socket closed with %d, want %d", code, statusRevoked)
	}
	mustStatus(t, carol.do("GET", "/api/shared/"+id, nil), 404, "after unsharing")
	found := false
	for _, a := range auditActions(t, h) {
		found = found || a == "session.share"
	}
	if !found {
		t.Fatal("sharing not audited")
	}
}

// Watchers have room of their own; the owner can always get in.
func TestWatchersDoNotCrowdOutTheOwner(t *testing.T) {
	h := newHarness(t)
	admin := h.admin()
	bob := h.member(admin, "bob")
	carol := h.member(admin, "carol")
	srv := h.sshServer(t.TempDir())
	id := bob.do("POST", "/api/sessions", map[string]any{"host_id": h.hostFor(bob, srv.Addr.Port)}).json()["id"].(string)
	mustStatus(t, bob.do("POST", "/api/sessions/"+id+"/shares", map[string]string{"username": "carol"}), 200, "share")
	var open []*websocket.Conn
	defer func() {
		for _, c := range open {
			c.CloseNow()
		}
	}()
	// Four watcher windows fit; more are refused like a full session. (A
	// read that times out closes the socket, so only the extra ones are read.)
	for i := 0; i < 6; i++ {
		c, err := h.dialWatch(carol, id)
		if err != nil {
			t.Fatal(err)
		}
		open = append(open, c)
		time.Sleep(100 * time.Millisecond)
	}
	for i, c := range open[4:] {
		if _, code := readOutput(c, "", 2*time.Second); code != statusTooManyViewers {
			t.Fatalf("watcher window %d: close %d, want %d", i+5, code, statusTooManyViewers)
		}
	}
	n := 0
	for _, s := range bob.do("GET", "/api/sessions", nil).jsonArray() {
		if s["id"] == id {
			n = int(s["watching"].(float64))
		}
	}
	if n != 4 {
		t.Fatalf("%d watcher windows attached, want 4", n)
	}
	for i := 0; i < 3; i++ {
		c, err := h.dialWS(bob, id, "")
		if err != nil {
			t.Fatal(err)
		}
		open = append(open, c)
		if out, code := readOutput(c, "ready", 2*time.Second); code == statusTooManyViewers {
			t.Fatalf("owner refused while watchers are on: %q", out)
		}
	}
}

func (r resp) jsonArray() []map[string]any {
	var out []map[string]any
	_ = json.Unmarshal(r.Body, &out)
	return out
}

// A watcher's socket gets neither the owner's side information nor a
// repaint of the host, and a revoke cuts it off.
func TestWatcherGetsNoSideInfo(t *testing.T) {
	h := newHarness(t)
	admin := h.admin()
	bob := h.member(admin, "bob")
	carol := h.member(admin, "carol")
	srv := h.sshServer(t.TempDir())
	id := bob.do("POST", "/api/sessions", map[string]any{"host_id": h.hostFor(bob, srv.Addr.Port)}).json()["id"].(string)
	sess, _ := h.terms.Get(h.userID(bob), id)
	sess.PublishMeta([]byte(`{"t":"herdr","secret":"owner-only"}`))
	mustStatus(t, bob.do("POST", "/api/sessions/"+id+"/shares", map[string]string{"username": "carol"}), 200, "share")
	cw, err := h.dialWatch(carol, id)
	if err != nil {
		t.Fatal(err)
	}
	defer cw.CloseNow()
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	for {
		typ, data, err := cw.Read(ctx)
		if err != nil {
			break
		}
		if typ == websocket.MessageText && strings.Contains(string(data), "owner-only") {
			t.Fatal("the watcher got the owner's side information")
		}
	}
	found := false
	for _, a := range auditActions(t, h) {
		found = found || a == "session.watch"
	}
	if !found {
		t.Fatal("watching is not audited")
	}
}

// Each leave is audited once when first used: again after being taken back
// and given again, not on every reconnect.
func TestWatchAuditedPerLeave(t *testing.T) {
	h := newHarness(t)
	admin := h.admin()
	bob := h.member(admin, "bob")
	carol := h.member(admin, "carol")
	srv := h.sshServer(t.TempDir())
	id := bob.do("POST", "/api/sessions", map[string]any{"host_id": h.hostFor(bob, srv.Addr.Port)}).json()["id"].(string)
	cid := strconv.FormatInt(h.userID(carol), 10)
	watchOnce := func() {
		c, err := h.dialWatch(carol, id)
		if err != nil {
			t.Fatal(err)
		}
		readOutput(c, "ready", time.Second)
		c.CloseNow()
	}
	count := func() int {
		n := 0
		for _, a := range auditActions(t, h) {
			if a == "session.watch" {
				n++
			}
		}
		return n
	}
	mustStatus(t, bob.do("POST", "/api/sessions/"+id+"/shares", map[string]string{"username": "carol"}), 200, "share")
	watchOnce()
	watchOnce()
	if n := count(); n != 1 {
		t.Fatalf("%d watch entries after two connections under one leave", n)
	}
	mustStatus(t, bob.do("DELETE", "/api/sessions/"+id+"/shares/"+cid, nil), 200, "unshare")
	mustStatus(t, bob.do("POST", "/api/sessions/"+id+"/shares", map[string]string{"username": "carol"}), 200, "share again")
	watchOnce()
	if n := count(); n != 2 {
		t.Fatalf("%d watch entries after a new leave", n)
	}
}
