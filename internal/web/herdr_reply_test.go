package web

import (
	"bufio"
	"encoding/json"
	"fmt"
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
	tabs   []any // the snapshot's tabs
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
			result = map[string]any{"snapshot": map[string]any{"version": "0.9.5", "focused_workspace_id": "w1", "workspaces": []any{}, "panes": []any{}, "tabs": f.tabs}}
		case "pane.read":
			result = map[string]any{"read": map[string]any{"text": f.screen}}
		case "pane.zoom", "tab.create", "pane.split", "pane.focus_direction", "tab.focus":
			req.Params["method"] = req.Method
			f.sent = append(f.sent, req.Params)
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

// noHerdrConfig answers the read of herdr's config.toml on a host that has none.
func noHerdrConfig(ch ssh.Channel) {
	ch.CloseWrite()
	ch.SendRequest("exit-status", false, []byte{0, 0, 0, 0})
}

// herdrReplyEnv opens a herdr session of admin's on a host whose herdr is
// hd, and returns the base path of pane w1:p1's endpoints and the session.
func herdrReplyEnv(t *testing.T, h *harness, admin *client, hd *fakeHerdr) (base, id string) {
	t.Helper()
	srv := h.sshServer(t.TempDir())
	srv.HandleExec(func(cmd string) func(ssh.Channel) {
		if cmd == herdr.KeysCommand {
			return noHerdrConfig // (none: herdr's default keys)
		}
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

	sess, err := h.srv.terms.Get(1, id) // (the admin, the first account)
	if err != nil {
		t.Fatal(err)
	}
	key := h.srv.convoReplyKey(sess, "w1:p1")         // (the pane's, whichever connection)
	held, err := h.srv.replies.lock(t.Context(), key) // another answer under way
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
		e := h.srv.replies.m[key]
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

// zoom_pane has a pane fill its tab; unzoom ends that for the focused pane.
func TestHerdrZoom(t *testing.T) {
	h := newHarness(t)
	admin := h.admin()
	hd := newFakeHerdr(t)
	_, id := herdrReplyEnv(t, h, admin, hd)
	path := "/api/sessions/" + id + "/herdr"
	mustStatus(t, admin.do("POST", path, map[string]any{"action": "zoom_pane", "target": "w1:p2"}), 200, "zoom")
	mustStatus(t, admin.do("POST", path, map[string]any{"action": "unzoom"}), 200, "unzoom")
	mustStatus(t, admin.do("POST", path, map[string]any{"action": "zoom_pane"}), 400, "zoom without a pane")
	mustStatus(t, admin.do("POST", path, map[string]any{"action": "unzoom", "target": "w1:p2"}), 400, "unzoom with a pane")
	s := hd.typed()
	if len(s) != 2 || s[0]["pane_id"] != "w1:p2" || s[0]["mode"] != "on" || s[1]["mode"] != "off" || s[1]["pane_id"] != nil {
		t.Fatalf("zoom requests: %v", s)
	}
}

// The shortcut panel's actions act on the focused pane or tab.
func TestHerdrPanelActions(t *testing.T) {
	h := newHarness(t)
	admin := h.admin()
	hd := newFakeHerdr(t)
	hd.tabs = []any{
		map[string]any{"tab_id": "w1:t2", "workspace_id": "w1", "number": 2, "focused": true},
		map[string]any{"tab_id": "w1:t1", "workspace_id": "w1", "number": 1},
		map[string]any{"tab_id": "w1:t3", "workspace_id": "w1", "number": 3},
		map[string]any{"tab_id": "w2:t1", "workspace_id": "w2", "number": 1},
	}
	_, id := herdrReplyEnv(t, h, admin, hd)
	path := "/api/sessions/" + id + "/herdr"
	for _, a := range []string{"zoom_toggle", "new_tab", "split_right", "split_down", "focus_left", "focus_down", "next_tab", "prev_tab"} {
		mustStatus(t, admin.do("POST", path, map[string]any{"action": a}), 200, a)
	}
	mustStatus(t, admin.do("POST", path, map[string]any{"action": "split_right", "target": "w1:p2"}), 400, "split with a target")
	mustStatus(t, admin.do("POST", path, map[string]any{"action": "split_left"}), 400, "unknown action")
	got := []string{}
	for _, p := range hd.typed() {
		got = append(got, fmt.Sprintf("%v mode=%v dir=%v focus=%v tab=%v", p["method"], p["mode"], p["direction"], p["focus"], p["tab_id"]))
	}
	want := []string{
		"pane.zoom mode=toggle dir=<nil> focus=<nil> tab=<nil>",
		"tab.create mode=<nil> dir=<nil> focus=true tab=<nil>",
		"pane.split mode=<nil> dir=right focus=true tab=<nil>",
		"pane.split mode=<nil> dir=down focus=true tab=<nil>",
		"pane.focus_direction mode=<nil> dir=left focus=<nil> tab=<nil>",
		"pane.focus_direction mode=<nil> dir=down focus=<nil> tab=<nil>",
		"tab.focus mode=<nil> dir=<nil> focus=<nil> tab=w1:t3", // next after 2 (of 1, 2, 3; w2's aside)
		"tab.focus mode=<nil> dir=<nil> focus=<nil> tab=w1:t1",
	}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("requests:\n got %q\nwant %q", got, want)
	}
}
