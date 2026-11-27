package web

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"golang.org/x/crypto/ssh"

	"github.com/ihainan/corral/internal/herdr"
	"github.com/ihainan/corral/internal/sshx"
)

func TestHerdrTerminalAttachCommand(t *testing.T) {
	cmd := herdrTerminalAttachCommand("/home/u/.config/herdr/herdr.sock", "term_660ca5e7e9eef1")
	if !strings.Contains(cmd, "exec herdr terminal attach \"$2\" --takeover") {
		t.Fatalf("unexpected command: %s", cmd)
	}
	if !strings.Contains(cmd, "unset HERDR_ENV HERDR_PANE_ID HERDR_TAB_ID HERDR_WORKSPACE_ID") {
		t.Fatalf("missing HERDR_* unset: %s", cmd)
	}
	if !strings.Contains(cmd, "'/home/u/.config/herdr/herdr.sock'") || !strings.Contains(cmd, "'term_660ca5e7e9eef1'") {
		t.Fatalf("args not shell-quoted: %s", cmd)
	}
}

func TestHerdrTermIDValidation(t *testing.T) {
	for _, good := range []string{"term_660ca5e7e9eef1", "t-1:2.3", "abc123"} {
		if !herdrTermID.MatchString(good) {
			t.Errorf("expected valid terminal id: %q", good)
		}
	}
	for _, bad := range []string{"", "term;id", "term id", "../x", "$(id)", strings.Repeat("a", 101)} {
		if herdrTermID.MatchString(bad) {
			t.Errorf("expected invalid terminal id: %q", bad)
		}
	}
}

func TestClampPaneDim(t *testing.T) {
	if got := clampPaneDim("", 80, 20, 400); got != 80 {
		t.Fatalf("empty: got %d", got)
	}
	if got := clampPaneDim("abc", 80, 20, 400); got != 80 {
		t.Fatalf("non-int: got %d", got)
	}
	if got := clampPaneDim("5", 80, 20, 400); got != 20 {
		t.Fatalf("below min: got %d", got)
	}
	if got := clampPaneDim("999", 80, 20, 400); got != 400 {
		t.Fatalf("above max: got %d", got)
	}
	if got := clampPaneDim("120", 80, 20, 400); got != 120 {
		t.Fatalf("in range: got %d", got)
	}
}

func TestPaneAttachSlots(t *testing.T) {
	var slots paneAttachSlots
	c1 := &ssh.Client{}
	c2 := &ssh.Client{}
	ctx := context.Background()

	rel1, err := slots.acquire(ctx, c1, "term_1", 0)
	if err != nil {
		t.Fatalf("first acquire should succeed: %v", err)
	}
	// Duplicate (client, termID) must be rejected so two panels never fight.
	if _, err := slots.acquire(ctx, c1, "term_1", 0); !errors.Is(err, errPaneDuplicate) {
		t.Fatalf("duplicate acquire for same terminal: got %v, want errPaneDuplicate", err)
	}
	// Different client with same termID is allowed.
	relOther, err := slots.acquire(ctx, c2, "term_1", 0)
	if err != nil {
		t.Fatalf("acquire on distinct client should succeed: %v", err)
	}
	relOther()

	// Fill up to sshx.MaxPanePerConn on c1.
	rels := []func(){rel1}
	for i := 2; i <= sshx.MaxPanePerConn; i++ {
		rel, err := slots.acquire(ctx, c1, "term_"+string(rune('0'+i)), 0)
		if err != nil {
			t.Fatalf("acquire #%d should succeed: %v", i, err)
		}
		rels = append(rels, rel)
	}
	if _, err := slots.acquire(ctx, c1, "term_overflow", 0); !errors.Is(err, sshx.ErrBusy) {
		t.Fatalf("acquire above per-client cap: got %v, want sshx.ErrBusy", err)
	}
	if got := sshx.SessionClassInUse(c1, sshx.SessionPane); got != sshx.MaxPanePerConn {
		t.Fatalf("unified ledger pane count = %d, want %d", got, sshx.MaxPanePerConn)
	}
	// Releasing frees both the terminal key and unified ledger quota (idempotent).
	rels[0]()
	rels[0]()
	relAgain, err := slots.acquire(ctx, c1, "term_1", 0)
	if err != nil {
		t.Fatalf("re-acquire after release should succeed: %v", err)
	}
	relAgain()
	for _, r := range rels[1:] {
		r()
	}
	if got := sshx.SessionInUse(c1); got != 0 {
		t.Fatalf("unified ledger leaked slots: %d", got)
	}
}

// When the unified SSH session ledger is full, GET /api/sessions/{id}/pane-ws
// briefly queues and then returns a structured ssh_channels_busy error over WS
// (code 4429) without leaking the per-pane key.
func TestPaneSocketBusySendsChannelsBusyError(t *testing.T) {
	oldWait := paneSlotWait
	paneSlotWait = 120 * time.Millisecond
	t.Cleanup(func() { paneSlotWait = oldWait })

	h := newHarness(t)
	admin := h.admin()
	srv := h.sshServer(t.TempDir())
	r := admin.do("POST", "/api/hosts", map[string]any{
		"name": "box", "address": "127.0.0.1", "port": srv.Addr.Port,
		"username": "alice", "auth": "password", "password": "alice-pass",
	})
	mustStatus(t, r, 201, "create host")
	r = admin.do("POST", "/api/sessions", map[string]any{"host_id": r.json()["id"]})
	mustStatus(t, r, 201, "open session")
	sessID := r.json()["id"].(string)
	sess, err := h.terms.Get(h.userID(admin), sessID)
	if err != nil {
		t.Fatal(err)
	}
	// Register a synthetic herdr socket and pane with a valid terminal_id.
	h.srv.herdrSocks.set(sessID, "/run/herdr.sock")
	h.srv.herdrSocks.setServer(sessID, "test", h.userID(admin), func() (*herdr.Snapshot, time.Time) {
		return &herdr.Snapshot{
			Panes: []herdr.Pane{{ID: "w1:p1", TerminalID: "term_test_1"}},
		}, time.Now()
	}, nil)

	// Fill all remaining slots in the unified ledger (Terminal is already 1/9):
	// take 4 SFTP + 4 Aux (interactive) = 9/9 so 0 slots remain for SessionPane.
	var held []func()
	t.Cleanup(func() {
		for _, rel := range held {
			rel()
		}
	})
	for range sshx.MaxSFTPPerConn {
		rel, err := sshx.AcquireSession(context.Background(), sess.Client(), sshx.SessionSFTP, true, 0)
		if err != nil {
			t.Fatalf("fill sftp: %v", err)
		}
		held = append(held, rel)
	}
	for range sshx.MaxAuxPerConn {
		rel, err := sshx.AcquireSession(context.Background(), sess.Client(), sshx.SessionAux, true, 0)
		if err != nil {
			t.Fatalf("fill aux: %v", err)
		}
		held = append(held, rel)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	wsURL := strings.Replace(h.ts.URL, "http", "ws", 1) + "/api/sessions/" + sessID + "/pane-ws?pane=w1:p1"
	hdr := http.Header{}
	hdr.Set("Origin", h.cfg.Origin())
	hdr.Set("Cookie", h.srv.cookieName()+"="+admin.cookie)

	ws, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{HTTPHeader: hdr})
	if err != nil {
		t.Fatalf("ws dial: %v", err)
	}
	defer ws.CloseNow()

	typ, data, err := ws.Read(ctx)
	if err != nil || typ != websocket.MessageText {
		t.Fatalf("read error msg: typ=%v err=%v", typ, err)
	}
	var msg struct {
		T    string `json:"t"`
		Code string `json:"code"`
	}
	if err := json.Unmarshal(data, &msg); err != nil || msg.T != "error" || msg.Code != "ssh_channels_busy" {
		t.Fatalf("got message %s, want error/ssh_channels_busy", data)
	}
	_, _, err = ws.Read(ctx)
	var ce websocket.CloseError
	if !errors.As(err, &ce) || ce.Code != statusTooManyViewers || ce.Reason != "ssh_channels_busy" {
		t.Fatalf("close err = %v, want 4429 ssh_channels_busy", err)
	}

	// The pane key must have been released on busy failure (not stuck as duplicate).
	relKey, ok := h.srv.paneAttaches.claimKey(sess.Client(), "term_test_1")
	if !ok {
		t.Fatal("pane key leaked after ssh_channels_busy refusal")
	}
	relKey()
}

func TestPaneFlowBackpressure(t *testing.T) {
	f := newPaneFlow()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if !f.waitRoom(ctx, paneOutWindow) {
		t.Fatal("initial window should allow write")
	}
	done := make(chan bool, 1)
	go func() {
		done <- f.waitRoom(ctx, 1024)
	}()
	f.ack(1024)
	if ok := <-done; !ok {
		t.Fatal("ack should unblock writer")
	}
}

// paneFreezer relays TCP traffic between Corral and the test SSH server, and
// can freeze client->server bytes (up) so the server never receives or
// auto-replies SSH_MSG_CHANNEL_CLOSE.
type paneFreezer struct {
	ln net.Listener
	up atomic.Bool
}

func newPaneFreezer(t *testing.T, target string) *paneFreezer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &paneFreezer{ln: ln}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			in, err := ln.Accept()
			if err != nil {
				return
			}
			out, err := net.Dial("tcp", target)
			if err != nil {
				in.Close()
				return
			}
			go f.relay(out, in, &f.up)
			go f.relay(in, out, nil)
		}
	}()
	return f
}

func (f *paneFreezer) relay(dst, src net.Conn, gate *atomic.Bool) {
	defer dst.Close()
	buf := make([]byte, 32<<10)
	for {
		n, err := src.Read(buf)
		for gate != nil && gate.Load() {
			time.Sleep(10 * time.Millisecond)
		}
		if n > 0 {
			if _, werr := dst.Write(buf[:n]); werr != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

// When RequestPty fails, exec/Start fails, or setup times out AND the peer
// never replies SSH_MSG_CHANNEL_CLOSE, the SessionPane ledger slot and per-pane
// key MUST NOT be released early. They remain held until the peer replies
// CHANNEL_CLOSE or the owning term.Session closes the underlying *ssh.Client.
func TestPaneSocketSetupFailureHoldsSlotUntilChannelEnds(t *testing.T) {
	cases := []struct {
		name           string
		mode           string // "pty_fail", "exec_fail", "setup_timeout"
		closeViaClient bool   // true: release by closing owning session; false: unfreeze relay
	}{
		{name: "pty_fail_unfreeze", mode: "pty_fail", closeViaClient: false},
		{name: "exec_fail_unfreeze", mode: "exec_fail", closeViaClient: false},
		{name: "setup_timeout_client_close", mode: "setup_timeout", closeViaClient: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.mode == "setup_timeout" {
				oldTimeout := paneSetupTimeout
				paneSetupTimeout = 150 * time.Millisecond
				t.Cleanup(func() { paneSetupTimeout = oldTimeout })
			}

			h := newHarness(t)
			admin := h.admin()
			srv := h.sshServer(t.TempDir())
			fr := newPaneFreezer(t, srv.Addr.String())

			// First session channel is the main terminal (handled normally);
			// subsequent session channels are pane-ws channels where we inject
			// the failure and freeze client->server so CHANNEL_CLOSE is never
			// answered by golang.org/x/crypto/ssh's server mux.
			var sessCount atomic.Int32
			srv.HandleChannel(func(nc ssh.NewChannel) bool {
				if nc.ChannelType() != "session" {
					return false
				}
				if sessCount.Add(1) == 1 {
					return false // main terminal uses default handler
				}
				ch, reqs, err := nc.Accept()
				if err != nil {
					return true
				}
				go func() {
					for r := range reqs {
						switch r.Type {
						case "pty-req":
							if tc.mode == "pty_fail" {
								fr.up.Store(true)
								_ = r.Reply(false, nil)
								go io.Copy(io.Discard, ch)
								continue
							}
							_ = r.Reply(true, nil)
						case "exec":
							if tc.mode == "exec_fail" {
								fr.up.Store(true)
								_ = r.Reply(false, nil)
								go io.Copy(io.Discard, ch)
								continue
							}
							if tc.mode == "setup_timeout" {
								// Freeze client->server and never reply to exec,
								// forcing paneSetupTimeout to fire while the
								// channel stays open on the peer.
								fr.up.Store(true)
								go io.Copy(io.Discard, ch)
								continue
							}
							_ = r.Reply(true, nil)
						default:
							if r.WantReply {
								_ = r.Reply(false, nil)
							}
						}
					}
				}()
				return true
			})

			relayPort := fr.ln.Addr().(*net.TCPAddr).Port
			r := admin.do("POST", "/api/hosts", map[string]any{
				"name": "box", "address": "127.0.0.1", "port": relayPort,
				"username": "alice", "auth": "password", "password": "alice-pass",
			})
			mustStatus(t, r, 201, "create host")
			r = admin.do("POST", "/api/sessions", map[string]any{"host_id": r.json()["id"]})
			mustStatus(t, r, 201, "open session")
			sessID := r.json()["id"].(string)
			sess, err := h.terms.Get(h.userID(admin), sessID)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				fr.up.Store(false)
				sess.Close("test cleanup")
			})

			h.srv.herdrSocks.set(sessID, "/run/herdr.sock")
			h.srv.herdrSocks.setServer(sessID, "test", h.userID(admin), func() (*herdr.Snapshot, time.Time) {
				return &herdr.Snapshot{
					Panes: []herdr.Pane{{ID: "w1:p1", TerminalID: "term_fail_1"}},
				}, time.Now()
			}, nil)

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			wsURL := strings.Replace(h.ts.URL, "http", "ws", 1) + "/api/sessions/" + sessID + "/pane-ws?pane=w1:p1"
			hdr := http.Header{}
			hdr.Set("Origin", h.cfg.Origin())
			hdr.Set("Cookie", h.srv.cookieName()+"="+admin.cookie)

			ws, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{HTTPHeader: hdr})
			if err != nil {
				t.Fatalf("ws dial: %v", err)
			}
			defer ws.CloseNow()

			// The WebSocket handler must exit promptly with {"t":"exit","code":1}
			// even though the remote channel has not answered CHANNEL_CLOSE.
			typ, data, err := ws.Read(ctx)
			if err != nil || typ != websocket.MessageText {
				t.Fatalf("read exit msg: typ=%v err=%v", typ, err)
			}
			var msg struct {
				T    string `json:"t"`
				Code int    `json:"code"`
			}
			if err := json.Unmarshal(data, &msg); err != nil || msg.T != "exit" || msg.Code != 1 {
				t.Fatalf("got message %s, want exit code 1", data)
			}

			// Wait well past any loopback RTT to verify no premature release occurred.
			time.Sleep(150 * time.Millisecond)

			if got := sshx.SessionClassInUse(sess.Client(), sshx.SessionPane); got != 1 {
				t.Fatalf("SessionPane slot released early while peer never answered CHANNEL_CLOSE: got %d, want 1", got)
			}
			if _, ok := h.srv.paneAttaches.claimKey(sess.Client(), "term_fail_1"); ok {
				t.Fatal("pane key released early while peer never answered CHANNEL_CLOSE")
			}

			// Now release either by letting CHANNEL_CLOSE reach the server or by
			// closing the owning term.Session (which closes *ssh.Client).
			if tc.closeViaClient {
				sess.Close("reclaim stuck channel")
			} else {
				fr.up.Store(false)
			}

			deadline := time.Now().Add(3 * time.Second)
			for sshx.SessionClassInUse(sess.Client(), sshx.SessionPane) != 0 {
				if time.Now().After(deadline) {
					t.Fatalf("SessionPane slot never released after %s: still %d", tc.name, sshx.SessionClassInUse(sess.Client(), sshx.SessionPane))
				}
				time.Sleep(10 * time.Millisecond)
			}
			relKey, ok := h.srv.paneAttaches.claimKey(sess.Client(), "term_fail_1")
			if !ok {
				t.Fatal("pane key still held after channel ended")
			}
			relKey()
		})
	}
}
