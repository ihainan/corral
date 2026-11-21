package herdr

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/ihainan/corral/internal/sshx"
	"github.com/ihainan/corral/internal/testutil"
)

const streamLocal = "direct-streamlocal@openssh.com"

func dialTest(t *testing.T, srv *testutil.SSHServer) *ssh.Client {
	t.Helper()
	c, err := ssh.Dial("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(srv.Addr.Port)), &ssh.ClientConfig{User: "alice",
		Auth: []ssh.AuthMethod{ssh.Password("alice-pass")}, HostKeyCallback: ssh.InsecureIgnoreHostKey(), Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

// within fails the test if f does not return in time (the old code hung forever).
func within(t *testing.T, d time.Duration, f func() error) error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- f() }()
	select {
	case err := <-done:
		return err
	case <-time.After(d):
		t.Fatal("herdr call did not give up")
		return nil
	}
}

// A herdr that accepts the connection but never answers.
func silentHerdr(t *testing.T) *ssh.Client {
	srv := testutil.StartSSHServer(t, "alice", "alice-pass", t.TempDir())
	srv.HandleChannel(func(nc ssh.NewChannel) bool {
		if nc.ChannelType() != streamLocal {
			return false
		}
		ch, reqs, err := nc.Accept()
		if err == nil {
			go ssh.DiscardRequests(reqs)
			go io.Copy(io.Discard, ch)
		}
		return true
	})
	return dialTest(t, srv)
}

// Discovery end to end against the test SSH server: the session list, the
// "not installed" answer, and a host that sends EOF before the exit status.
func TestDiscoverOverSSH(t *testing.T) {
	srv := testutil.StartSSHServer(t, "alice", "alice-pass", t.TempDir())
	missing := false
	srv.HandleExec(func(cmd string) func(ssh.Channel) {
		if cmd != DiscoverCommand {
			return nil
		}
		return func(ch ssh.Channel) {
			if missing {
				ch.Write([]byte(`{"missing":true}` + "\n"))
			} else {
				ch.Write([]byte(`{"sessions":[{"name":"default","default":true,"running":true,"socket_path":"/run/herdr/default.sock"}]}` + "\n"))
			}
			ch.CloseWrite()
			time.Sleep(100 * time.Millisecond) // the exit status comes after EOF
			ch.SendRequest("exit-status", false, []byte{0, 0, 0, 0})
		}
	})
	c := dialTest(t, srv)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	list, err := Discover(ctx, c)
	if err != nil || len(list) != 1 || !list[0].Default || !list[0].Running || list[0].SocketPath != "/run/herdr/default.sock" {
		t.Fatalf("discover: %+v %v", list, err)
	}
	missing = true
	if _, err := Discover(ctx, c); !errors.Is(err, ErrNotInstalled) {
		t.Fatalf("discover without herdr: %v", err)
	}
}

func TestCallTimesOutOnSilentHerdr(t *testing.T) {
	old := callTimeout
	callTimeout = 200 * time.Millisecond
	defer func() { callTimeout = old }()
	c := silentHerdr(t)
	cl, err := Dial(context.Background(), c, "/run/herdr.sock")
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()
	if err := within(t, 3*time.Second, func() error { _, err := cl.Snapshot(context.Background()); return err }); err == nil {
		t.Fatal("snapshot from a silent herdr succeeded")
	}
}

func TestCallHonoursCancel(t *testing.T) {
	c := silentHerdr(t)
	cl, err := Dial(context.Background(), c, "/run/herdr.sock")
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(100*time.Millisecond, cancel)
	if err := within(t, 3*time.Second, func() error { return cl.FocusPane(ctx, "p1") }); err == nil {
		t.Fatal("cancelled call succeeded")
	}
}

// A host that never answers the channel open: Dial gives up at the deadline
// (its own 10 s one when the caller has none, as for the watcher). The
// terminal's connection stays up; the unanswered opens keep their places, so
// once sshx.MaxAuxPerConn are stuck further herdr traffic is refused.
func TestDialGivesUpOnSilentHost(t *testing.T) {
	oldDial, oldMax := dialTimeout, sshx.MaxAuxPerConn
	dialTimeout, sshx.MaxAuxPerConn = 200*time.Millisecond, 2
	defer func() { dialTimeout, sshx.MaxAuxPerConn = oldDial, oldMax }()
	srv := testutil.StartSSHServer(t, "alice", "alice-pass", t.TempDir())
	hold := make(chan struct{})
	defer close(hold)
	srv.HandleChannel(func(nc ssh.NewChannel) bool {
		if nc.ChannelType() != streamLocal {
			return false
		}
		go func() { <-hold; nc.Reject(ssh.ConnectionFailed, "gone") }() // never answers the open in time
		return true
	})
	c := dialTest(t, srv)
	short, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	for _, ctx := range []context.Context{short, context.Background()} { // caller's deadline, then Dial's own
		if err := within(t, 3*time.Second, func() error { _, err := Dial(ctx, c, "/run/herdr.sock"); return err }); err == nil {
			t.Fatal("dial succeeded")
		}
	}
	if _, err := Dial(context.Background(), c, "/run/herdr.sock"); !errors.Is(err, sshx.ErrBusy) {
		t.Fatalf("dial with two stuck: %v", err)
	}
	closed := make(chan struct{})
	go func() { c.Wait(); close(closed) }()
	select {
	case <-closed:
		t.Fatal("stuck herdr traffic closed the terminal's connection")
	case <-time.After(300 * time.Millisecond):
	}
}

func TestSnapshotTokensAndReportMetadata(t *testing.T) {
	srv := testutil.StartSSHServer(t, "alice", "alice-pass", t.TempDir())
	var gotEnv map[string]string
	var gotTokens map[string]string
	var gotSource string
	var gotTTL int64

	srv.HandleChannel(func(nc ssh.NewChannel) bool {
		if nc.ChannelType() != streamLocal {
			return false
		}
		ch, reqs, err := nc.Accept()
		if err != nil {
			return true
		}
		go ssh.DiscardRequests(reqs)
		go func() {
			defer ch.Close()
			r := bufio.NewReader(ch)
			line, err := r.ReadBytes('\n')
			if err != nil {
				return
			}
			var req struct {
				ID     string          `json:"id"`
				Method string          `json:"method"`
				Params json.RawMessage `json:"params"`
			}
			if json.Unmarshal(line, &req) != nil {
				return
			}
			switch req.Method {
			case "session.snapshot":
				resp := `{"id":"` + req.ID + `","result":{"snapshot":{"version":"0.8.0","panes":[{"pane_id":"w1:p2","workspace_id":"w1","tab_id":"t1","agent":"claude","name":"worker-1","terminal_id":"term-2","agent_session":{"source":"hook","agent":"claude","kind":"id","value":"sess-2"},"tokens":{"corral_parent_pane":"w1:p1"}}],"agents":[{"pane_id":"w1:p2","workspace_id":"w1","tab_id":"t1","agent":"claude","name":"worker-1","terminal_id":"term-2","agent_session":{"source":"hook","agent":"claude","kind":"id","value":"sess-2"},"tokens":{"corral_parent_pane":"w1:p1"}}]}}}` + "\n"
				_, _ = ch.Write([]byte(resp))
			case "workspace.create":
				var p struct {
					Env map[string]string `json:"env"`
				}
				_ = json.Unmarshal(req.Params, &p)
				gotEnv = p.Env
				resp := `{"id":"` + req.ID + `","result":{"workspace":{"workspace_id":"w2"},"root_pane":{"pane_id":"w2:p1","terminal_id":"term-9"}}}` + "\n"
				_, _ = ch.Write([]byte(resp))
			case "pane.report_metadata":
				var p struct {
					Source string            `json:"source"`
					Tokens map[string]string `json:"tokens"`
					TTLMs  int64             `json:"ttl_ms"`
				}
				_ = json.Unmarshal(req.Params, &p)
				gotSource, gotTokens, gotTTL = p.Source, p.Tokens, p.TTLMs
				resp := `{"id":"` + req.ID + `","result":{"ok":true}}` + "\n"
				_, _ = ch.Write([]byte(resp))
			}
		}()
		return true
	})

	c := dialTest(t, srv)
	ctx := context.Background()

	cl, err := Dial(ctx, c, "/run/herdr.sock")
	if err != nil {
		t.Fatal(err)
	}
	snap, err := cl.Snapshot(ctx)
	cl.Close()
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Agents) != 1 || snap.Agents[0].Name != "worker-1" || snap.Agents[0].Tokens["corral_parent_pane"] != "w1:p1" || snap.Agents[0].AgentSession == nil || snap.Agents[0].AgentSession.Kind != "id" || snap.Agents[0].AgentSession.Value != "sess-2" {
		t.Fatalf("unexpected snapshot agent: %+v", snap.Agents)
	}

	cl2, err := Dial(ctx, c, "/run/herdr.sock")
	if err != nil {
		t.Fatal(err)
	}
	ws, pane, termID, err := cl2.CreateWorkspaceWithEnv(ctx, "/tmp", "child", map[string]string{"CORRAL_PARENT_PANE": "w1:p1"})
	cl2.Close()
	if err != nil || ws != "w2" || pane != "w2:p1" || termID != "term-9" || gotEnv["CORRAL_PARENT_PANE"] != "w1:p1" {
		t.Fatalf("CreateWorkspaceWithEnv: ws=%s pane=%s term=%s env=%v err=%v", ws, pane, termID, gotEnv, err)
	}

	cl3, err := Dial(ctx, c, "/run/herdr.sock")
	if err != nil {
		t.Fatal(err)
	}
	err = cl3.ReportMetadata(ctx, "w2:p1", "corral-lineage", map[string]string{"corral_parent_pane": "w1:p1"}, 86400000)
	cl3.Close()
	if err != nil || gotSource != "corral-lineage" || gotTokens["corral_parent_pane"] != "w1:p1" || gotTTL != 86400000 {
		t.Fatalf("ReportMetadata: source=%s tokens=%v ttl=%d err=%v", gotSource, gotTokens, gotTTL, err)
	}
}

func TestWatcherLegacySubscribeFallbackAndOnEvent(t *testing.T) {
	srv := testutil.StartSSHServer(t, "alice", "alice-pass", t.TempDir())
	var subCalls atomic.Int32
	eventSent := make(chan struct{})

	srv.HandleChannel(func(nc ssh.NewChannel) bool {
		if nc.ChannelType() != streamLocal {
			return false
		}
		ch, reqs, err := nc.Accept()
		if err != nil {
			return true
		}
		go ssh.DiscardRequests(reqs)
		go func() {
			defer ch.Close()
			r := bufio.NewReader(ch)
			line, err := r.ReadBytes('\n')
			if err != nil {
				return
			}
			var req struct {
				ID     string          `json:"id"`
				Method string          `json:"method"`
				Params json.RawMessage `json:"params"`
			}
			if json.Unmarshal(line, &req) != nil {
				return
			}
			switch req.Method {
			case "session.snapshot":
				_, _ = ch.Write([]byte(`{"id":"` + req.ID + `","result":{"snapshot":{"version":"0.7.0","panes":[{"pane_id":"w1:p1","workspace_id":"w1","tab_id":"t1"}]}}}` + "\n"))
			case "events.subscribe":
				n := subCalls.Add(1)
				if n == 1 && strings.Contains(string(req.Params), "pane.moved") {
					// Simulate older herdr rejecting unknown event type and closing connection.
					_, _ = ch.Write([]byte(`{"id":"` + req.ID + `","error":{"code":"invalid_params","message":"unknown subscription type pane.moved"}}` + "\n"))
					return
				}
				_, _ = ch.Write([]byte(`{"id":"` + req.ID + `","result":{"ok":true}}` + "\n"))
				_, _ = ch.Write([]byte(`{"event":"pane_moved","data":{"type":"pane_moved","previous_pane_id":"w1:p1","pane":{"pane_id":"w2:p1","terminal_id":"term-1"}}}` + "\n"))
				close(eventSent)
				_, _ = io.Copy(io.Discard, ch)
			}
		}()
		return true
	})

	c := dialTest(t, srv)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	gotEvent := make(chan string, 1)
	w := NewWatcher(c, "/run/herdr.sock", nil, nil)
	w.OnEvent = func(ev string, _ json.RawMessage) {
		select {
		case gotEvent <- ev:
		default:
		}
	}
	go w.Run(ctx)

	select {
	case ev := <-gotEvent:
		if ev != "pane_moved" {
			t.Fatalf("expected pane_moved, got %q", ev)
		}
	case <-ctx.Done():
		t.Fatal("timed out waiting for fallback subscribe and event")
	}
	if subCalls.Load() < 2 {
		t.Fatalf("expected re-Dial fallback subscribe call, got %d", subCalls.Load())
	}
}
