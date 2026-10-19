package web

import (
	"bufio"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/ihainan/corral/internal/herdr"
)

// fakeHerdr is a herdr server on a Unix socket: it answers snapshots and
// subscriptions (for the gateway's watcher), reads a pane's screen as set
// by the test, and records what is typed into panes. Like herdr, it answers
// one request per connection, a subscription aside.
type fakeHerdr struct {
	path   string
	mu     sync.Mutex
	screen string
	sent   []map[string]any // pane.send_keys / pane.send_input params, with "method"
	// onSend: "" answers, "refuse" answers with an error, "drop" types and
	// closes the connection without answering.
	onSend string
}

func newFakeHerdr(t *testing.T) *fakeHerdr {
	t.Helper()
	dir, err := os.MkdirTemp("", "hd") // short: Unix socket paths are limited
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	f := &fakeHerdr{path: filepath.Join(dir, "h.sock")}
	l, err := net.Listen("unix", f.path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go f.serve(c)
		}
	}()
	return f
}

func (f *fakeHerdr) serve(c net.Conn) {
	defer c.Close()
	sc := bufio.NewScanner(c)
	for sc.Scan() {
		var req struct {
			ID     string         `json:"id"`
			Method string         `json:"method"`
			Params map[string]any `json:"params"`
		}
		if json.Unmarshal(sc.Bytes(), &req) != nil {
			return
		}
		var result any = map[string]any{}
		f.mu.Lock()
		switch req.Method {
		case "session.snapshot":
			result = map[string]any{"snapshot": map[string]any{"version": "0.9.5", "workspaces": []any{}, "panes": []any{}}}
		case "pane.read":
			result = map[string]any{"read": map[string]any{"text": f.screen}}
		case "pane.send_keys", "pane.send_input":
			switch f.onSend {
			case "refuse":
				f.mu.Unlock()
				b, _ := json.Marshal(map[string]any{"id": req.ID, "error": map[string]any{"code": "pane_not_found", "message": "no such pane"}})
				c.Write(append(b, '\n'))
				return
			case "drop":
				req.Params["method"] = req.Method
				f.sent = append(f.sent, req.Params)
				f.mu.Unlock()
				return
			}
			req.Params["method"] = req.Method
			f.sent = append(f.sent, req.Params)
		}
		f.mu.Unlock()
		b, _ := json.Marshal(map[string]any{"id": req.ID, "result": result})
		c.Write(append(b, '\n'))
		if req.Method != "events.subscribe" {
			return
		}
	}
}

func (f *fakeHerdr) setScreen(s string) {
	f.mu.Lock()
	f.screen = s
	f.mu.Unlock()
}

func (f *fakeHerdr) typed() []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]map[string]any(nil), f.sent...)
}

// herdrReplyEnv opens a herdr session of admin's on a host whose herdr is
// hd, and returns the base path of pane w1:p1's endpoints and the session.
func herdrReplyEnv(t *testing.T, h *harness, admin *client, hd *fakeHerdr) (base, id string) {
	t.Helper()
	srv := h.sshServer(t.TempDir())
	srv.HandleExec(func(cmd string) func(ssh.Channel) {
		if cmd != herdr.DiscoverCommand {
			return nil
		}
		return func(ch ssh.Channel) {
			b, _ := json.Marshal(map[string]any{"sessions": []any{map[string]any{"name": "default", "default": true, "running": true, "socket_path": hd.path}}})
			ch.Write(b)
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
	id = r.json()["id"].(string)
	eventually(t, "herdr session was never discovered", func() bool { return h.srv.herdrSocks.get(id) == hd.path })
	return "/api/sessions/" + id + "/herdr/panes/w1:p1/", id
}

func TestHerdrReply(t *testing.T) {
	h := newHarness(t)
	admin := h.admin()
	hd := newFakeHerdr(t)
	hd.setScreen("Do you want to proceed?\r\n❯ 1. Yes\r\n  2. No\r\n\r\n   \r\n")
	base, id := herdrReplyEnv(t, h, admin, hd)

	// The screen, without trailing blank space, and its digest.
	r := admin.do("GET", base+"screen", nil)
	mustStatus(t, r, 200, "screen")
	if got := r.json()["text"]; got != "Do you want to proceed?\n❯ 1. Yes\n  2. No" {
		t.Fatalf("screen text %q", got)
	}
	digest, _ := r.json()["digest"].(string)
	if len(digest) != 64 {
		t.Fatalf("digest %q", digest)
	}

	// Invalid answers are refused before anything is read or typed.
	for name, body := range map[string]map[string]any{
		"key not allowed": {"digest": digest, "keys": []string{"q"}},
		"too many keys":   {"digest": digest, "keys": []string{"1", "1", "1", "1", "1"}},
		"keys and text":   {"digest": digest, "keys": []string{"1"}, "text": "x"},
		"neither":         {"digest": digest},
		"control in text": {"digest": digest, "text": "a\x1b[Ab"},
		"newline in text": {"digest": digest, "text": "a\nb"},
		"text too long":   {"digest": digest, "text": strings.Repeat("字", maxReplyText+1)},
		"no digest":       {"keys": []string{"1"}},
	} {
		mustStatus(t, admin.do("POST", base+"reply", body), 400, name)
	}
	mustStatus(t, admin.do("GET", "/api/sessions/"+id+"/herdr/panes/w1:p1%2F..%2Fx/screen", nil), 400, "bad pane id")

	// The screen the user saw: the key is pressed in that pane.
	mustStatus(t, admin.do("POST", base+"reply", map[string]any{"digest": digest, "keys": []string{"2"}}), 200, "reply with a key")
	if s := hd.typed(); len(s) != 1 || s[0]["method"] != "pane.send_keys" || s[0]["pane_id"] != "w1:p1" || len(s[0]["keys"].([]any)) != 1 || s[0]["keys"].([]any)[0] != "2" {
		t.Fatalf("typed: %v", s)
	}

	// The same screen again at once (a double tap, a program that has not
	// redrawn yet): not typed twice.
	r = admin.do("POST", base+"reply", map[string]any{"digest": digest, "keys": []string{"2"}})
	mustStatus(t, r, 409, "same screen answered twice")
	if r.code() != "already_answered" || len(hd.typed()) != 1 {
		t.Fatalf("repeated answer: %s, typed %d", r.Body, len(hd.typed()))
	}

	// A screen larger than what is shown: a change in its hidden start still
	// changes the digest.
	big := strings.Repeat("x", maxScreenBytes)
	hd.setScreen("question A\n" + big)
	bigDigest, _ := admin.do("GET", base+"screen", nil).json()["digest"].(string)
	hd.setScreen("question B\n" + big)
	mustStatus(t, admin.do("POST", base+"reply", map[string]any{"digest": bigDigest, "keys": []string{"1"}}), 409, "change outside the shown text")
	if len(hd.typed()) != 1 {
		t.Fatal("typed into a screen whose start changed")
	}

	// The screen moved on: nothing is typed, the new screen comes back.
	hd.setScreen("Working…")
	r = admin.do("POST", base+"reply", map[string]any{"digest": digest, "keys": []string{"1"}})
	mustStatus(t, r, 409, "stale screen")
	extra, _ := r.json()["extra"].(map[string]any)
	if r.code() != "screen_changed" || extra["text"] != "Working…" || extra["digest"] == digest {
		t.Fatalf("stale answer: %s", r.Body)
	}
	if len(hd.typed()) != 1 {
		t.Fatal("typed into a screen that changed")
	}

	// Text is typed and Enter pressed, in one request.
	newDigest := extra["digest"].(string)
	mustStatus(t, admin.do("POST", base+"reply", map[string]any{"digest": newDigest, "text": "use the staging bucket"}), 200, "reply with text")
	if s := hd.typed(); len(s) != 2 || s[1]["method"] != "pane.send_input" || s[1]["text"] != "use the staging bucket" || s[1]["keys"].([]any)[0] != "enter" {
		t.Fatalf("typed: %v", s)
	}

	// Someone else's session does not exist for them.
	bob := h.member(admin, "bob")
	mustStatus(t, bob.do("GET", base+"screen", nil), 404, "other user's screen")
	mustStatus(t, bob.do("POST", base+"reply", map[string]any{"digest": newDigest, "keys": []string{"1"}}), 404, "other user's reply")
	if len(hd.typed()) != 2 {
		t.Fatal("another user typed into the pane")
	}

	// Audited, without what was typed.
	list, err := h.store.ListAudit(t.Context(), storeAuditQuery())
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, a := range list {
		if a.Action != "herdr.reply" {
			continue
		}
		n++
		if strings.Contains(string(a.Detail), "staging") {
			t.Fatalf("typed text in the audit log: %s", a.Detail)
		}
	}
	if n != 2 {
		t.Fatalf("%d replies audited, want 2", n)
	}
}

// Two answers to one screen at once: one is typed. herdr refusing, or its
// acknowledgement getting lost, is audited as such.
func TestHerdrReplyRacesAndOutcomes(t *testing.T) {
	h := newHarness(t)
	admin := h.admin()
	hd := newFakeHerdr(t)
	hd.setScreen("❯ 1. Yes\n  2. No")
	base, _ := herdrReplyEnv(t, h, admin, hd)
	digest, _ := admin.do("GET", base+"screen", nil).json()["digest"].(string)

	var wg sync.WaitGroup
	statuses := make(chan int, 4)
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			statuses <- admin.do("POST", base+"reply", map[string]any{"digest": digest, "keys": []string{"1"}}).Status
		}()
	}
	wg.Wait()
	close(statuses)
	ok := 0
	// (Refused as already answered, or as too busy: a connection of the
	// one before may not be given back yet. Either way, typed once.)
	for st := range statuses {
		if st == 200 {
			ok++
		} else if st != 409 && st != 429 {
			t.Fatalf("concurrent answer: status %d", st)
		}
	}
	if ok != 1 || len(hd.typed()) != 1 {
		t.Fatalf("%d answers accepted, %d typed; want 1", ok, len(hd.typed()))
	}

	outcome := func() string {
		list, err := h.store.ListAudit(t.Context(), storeAuditQuery())
		if err != nil {
			t.Fatal(err)
		}
		for _, a := range list { // newest first
			if a.Action == "herdr.reply" {
				var d map[string]any
				json.Unmarshal(a.Detail, &d)
				o, _ := d["outcome"].(string)
				return o
			}
		}
		return ""
	}
	setSend := func(mode string) {
		hd.mu.Lock()
		hd.onSend = mode
		hd.mu.Unlock()
	}
	if o := outcome(); o != "typed" {
		t.Fatalf("outcome %q, want typed", o)
	}
	hd.setScreen("❯ 1. Yes\n  2. No\n  3. Later")
	digest, _ = admin.do("GET", base+"screen", nil).json()["digest"].(string)
	setSend("refuse")
	mustStatus(t, admin.do("POST", base+"reply", map[string]any{"digest": digest, "keys": []string{"3"}}), 422, "refused")
	if o := outcome(); o != "refused" {
		t.Fatalf("outcome %q, want refused", o)
	}
	setSend("drop")
	mustStatus(t, admin.do("POST", base+"reply", map[string]any{"digest": digest, "keys": []string{"3"}}), 502, "acknowledgement lost")
	if o := outcome(); o != "unknown" {
		t.Fatalf("outcome %q, want unknown", o)
	}
	// It may have been typed: the same screen is not answered again at once.
	setSend("")
	mustStatus(t, admin.do("POST", base+"reply", map[string]any{"digest": digest, "keys": []string{"3"}}), 409, "after a lost acknowledgement")
}

// An answer waiting for its turn when its login ends is not typed.
func TestHerdrReplyEndsWithTheLogin(t *testing.T) {
	h := newHarness(t)
	admin := h.admin()
	hd := newFakeHerdr(t)
	hd.setScreen("❯ 1. Yes\n  2. No")
	base, id := herdrReplyEnv(t, h, admin, hd)
	digest, _ := admin.do("GET", base+"screen", nil).json()["digest"].(string)

	held, err := h.srv.replies.lock(t.Context(), id+"\x00w1:p1") // another answer under way
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan resp, 1)
	go func() {
		done <- admin.do("POST", base+"reply", map[string]any{"digest": digest, "keys": []string{"1"}})
	}()
	eventually(t, "answer never started waiting", func() bool {
		h.srv.replies.mu.Lock()
		defer h.srv.replies.mu.Unlock()
		e := h.srv.replies.m[id+"\x00w1:p1"]
		return e != nil && e.users == 2
	})
	mustStatus(t, admin.do("POST", "/api/logout", nil), 200, "sign out")
	select {
	case r := <-done:
		if r.Status == 200 {
			t.Fatal("answer typed after signing out")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("waiting answer not cancelled by signing out")
	}
	h.srv.replies.unlock(held)
	if len(hd.typed()) != 0 {
		t.Fatal("typed after signing out")
	}
}
