//go:build linux

package web

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/ihainan/corral/internal/auth"
	"github.com/ihainan/corral/internal/config"
	"github.com/ihainan/corral/internal/corrald/agent"
	"github.com/ihainan/corral/internal/corrald/daemon"
	"github.com/ihainan/corral/internal/corrald/gw"
	"github.com/ihainan/corral/internal/corrald/proto"
	"github.com/ihainan/corral/internal/perf"
	"github.com/ihainan/corral/internal/sshx"
	"github.com/ihainan/corral/internal/store"
	"github.com/ihainan/corral/internal/term"
	"github.com/ihainan/corral/internal/testutil"
)

// TestNoDirectConvoGatedBypass statically guarantees that no production file
// under internal/web constructs or executes convoGated* scripts outside the
// unified runGated entrypoint (corrald_gated.go) and the script builder
// helpers in conversation.go.
func TestNoDirectConvoGatedBypass(t *testing.T) {
	forbidden := map[string]bool{
		"convoGated":                 true,
		"convoSendCommand":           true,
		"convoGatedCommand":          true,
		"convoGatedPasteCommand":     true,
		"convoGatedQueueCommand":     true,
		"convoGatedImmediateCommand": true,
		"convoGatedFillCommand":      true,
		"convoGatedPicCommand":       true,
	}
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	checked := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		checked++
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			// Allow definitions of the convoGated* builders inside conversation.go,
			// and the single unified fallback inside corrald_gated.go:runGated.
			if name == "conversation.go" && forbidden[fn.Name.Name] {
				continue
			}
			if name == "corrald_gated.go" && fn.Name.Name == "runGated" {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				id, ok := call.Fun.(*ast.Ident)
				if ok && forbidden[id.Name] {
					pos := fset.Position(call.Pos())
					t.Errorf("%s:%d (%s) calls %s directly; must route through Server.runGated", name, pos.Line, fn.Name.Name, id.Name)
				}
				return true
			})
		}
	}
	if checked < 10 {
		t.Fatalf("expected to scan >=10 production files in internal/web, scanned %d", checked)
	}
}

type fakeGatedEnv struct {
	srv       *Server
	mgr       *gw.Manager
	sess      *term.Session
	sshSrv    *testutil.SSHServer
	sshClient *ssh.Client
	sshCount  *atomic.Int32
	userID    int64
	hostID    int64
	sockPath  string
}

type testGatedSSHProvider struct {
	client *ssh.Client
	fp     string
	user   string
}

func (p *testGatedSSHProvider) AcquireSSH(_ context.Context, _, _ int64) (*ssh.Client, func(), string, string, error) {
	return p.client, func() {}, p.fp, p.user, nil
}

func (p *testGatedSSHProvider) HostBinding(_ context.Context, _, _ int64) (string, string, error) {
	return p.user, p.fp, nil
}

func (p *testGatedSSHProvider) UserLabel(_ context.Context, _ int64) string {
	return p.user
}

func newFakeGatedEnv(t *testing.T, onRPC func(conn net.Conn, req proto.Request)) *fakeGatedEnv {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()
	u, err := st.CreateUser(ctx, "alice", "Alice", "hash", "admin")
	if err != nil {
		t.Fatal(err)
	}
	h, err := st.CreateHost(ctx, &store.Host{
		OwnerID: u.ID, Name: "box", Address: "127.0.0.1", Port: 22, Username: "alice", Auth: "password",
	})
	if err != nil {
		t.Fatal(err)
	}
	manifestBytes, err := proto.MarshalManifest(proto.DefaultManifest())
	if err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertCorraldHost(ctx, &store.CorraldHost{
		HostID:           h.ID,
		State:            gw.StateRunning,
		BoundFingerprint: "fp-gated",
		BoundUsername:    "alice",
		BoundGateway:     "gw-gated",
		ManifestJSON:     string(manifestBytes),
		Version:          "0.4.0",
		Release:          2000,
	}); err != nil {
		t.Fatal(err)
	}
	_ = st.RecordSSHVerified(ctx, h.ID, "fp-gated", "alice")

	// Start fake corrald Unix socket server.
	sockDir, err := os.MkdirTemp("/tmp", "corrald-gw-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(sockDir) })
	sockPath := filepath.Join(sockDir, "d.sock")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				defer conn.Close()
				for {
					var req proto.Request
					if err := proto.ReadJSON(conn, &req); err != nil {
						return
					}
					switch req.Method {
					case proto.MethodDaemonHello:
						resBytes, _ := json.Marshal(proto.HelloResult{
							Version:  "0.4.0",
							Release:  2000,
							Protocol: proto.Protocol,
							Epoch:    "ep-1",
							PID:      os.Getpid(),
							Manifest: proto.DefaultManifest(),
							State:    proto.StateRunning,
						})
						_ = proto.WriteJSON(conn, proto.Response{ID: req.ID, Result: resBytes})
					case proto.MethodDaemonHealth:
						resBytes, _ := json.Marshal(proto.HealthResult{
							Caps: map[string]string{
								"locate": "ok", "tails": "ok", "watch": "ok",
								"pane.send": "ok", "pane.answer": "ok",
							},
						})
						_ = proto.WriteJSON(conn, proto.Response{ID: req.ID, Result: resBytes})
					case proto.MethodDaemonResume:
						resBytes, _ := json.Marshal(proto.ResumeResult{Epoch: "ep-1"})
						_ = proto.WriteJSON(conn, proto.Response{ID: req.ID, Result: resBytes})
					case proto.MethodAgentWatch:
						_ = proto.WriteJSON(conn, proto.Response{ID: req.ID, Result: json.RawMessage(`{}`)})
					default:
						onRPC(conn, req)
					}
				}
			}(c)
		}
	}()

	// Start counting SSH server that forwards direct-streamlocal to sockPath
	// and counts every SSH exec call.
	sshCount := &atomic.Int32{}
	sshSrv := testutil.StartSSHServer(t, "alice", "pass", t.TempDir())
	sshSrv.HandleExec(func(cmd string) func(ssh.Channel) {
		return func(ch ssh.Channel) {
			defer ch.Close()
			if strings.Contains(cmd, "corral-send-") {
				_, _ = io.Copy(io.Discard, ch)
				sshCount.Add(1)
				_, _ = ch.Write([]byte("ssh-fallback-ok\n"))
			}
			_, _ = ch.SendRequest("exit-status", false, ssh.Marshal(&struct{ Status uint32 }{0}))
		}
	})
	sshSrv.HandleChannel(func(nc ssh.NewChannel) bool {
		if nc.ChannelType() != "direct-streamlocal@openssh.com" {
			return false
		}
		var payload struct {
			SocketPath string
			Reserved0  string
			Reserved1  uint32
		}
		if err := ssh.Unmarshal(nc.ExtraData(), &payload); err != nil {
			_ = nc.Reject(ssh.ConnectionFailed, "bad payload")
			return true
		}
		uconn, err := net.Dial("unix", payload.SocketPath)
		if err != nil {
			_ = nc.Reject(ssh.ConnectionFailed, err.Error())
			return true
		}
		ch, reqs, err := nc.Accept()
		if err != nil {
			_ = uconn.Close()
			return true
		}
		go ssh.DiscardRequests(reqs)
		go func() {
			defer ch.Close()
			defer uconn.Close()
			buf := make([]byte, 32<<10)
			for {
				n, rerr := ch.Read(buf)
				if n > 0 {
					if _, werr := uconn.Write(buf[:n]); werr != nil {
						return
					}
				}
				if rerr != nil {
					return
				}
			}
		}()
		go func() {
			defer ch.Close()
			defer uconn.Close()
			buf := make([]byte, 32<<10)
			for {
				n, rerr := uconn.Read(buf)
				if n > 0 {
					if _, werr := ch.Write(buf[:n]); werr != nil {
						return
					}
				}
				if rerr != nil {
					return
				}
			}
		}()
		return true
	})

	sshClient, err := ssh.Dial("tcp", sshSrv.Addr.String(), &ssh.ClientConfig{
		User:            "alice",
		Auth:            []ssh.AuthMethod{ssh.Password("pass")},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         5 * time.Second,
	})
	if err != nil {
		t.Fatalf("dial ssh: %v", err)
	}
	t.Cleanup(func() { _ = sshClient.Close() })

	dist := &gw.Dist{
		Version:      "0.4.0",
		Release:      2000,
		Manifest:     proto.DefaultManifest(),
		ManifestHash: "mh",
	}
	mgr := gw.NewManager(gw.ManagerConfig{
		Store:               st,
		SSH:                 &testGatedSSHProvider{client: sshClient, fp: "fp-gated", user: "alice"},
		Dist:                dist,
		GatewayID:           "gw-gated",
		GatewayVersion:      "0.4.0",
		CapFailureThreshold: 3,
		ProbeOverride: func(ctx context.Context, c *ssh.Client) (*gw.HostProbe, error) {
			return &gw.HostProbe{
				OS:           "Linux",
				Arch:         runtime.GOARCH,
				MachineID:    "aabbccddeeff",
				Home:         sockDir,
				UID:          strconv.Itoa(os.Geteuid()),
				HomeWritable: true,
				LibDir:       sockDir,
				StateDir:     sockDir,
				SocketPath:   sockPath,
				HasSymlink:   true,
				HasInstall:   true,
			}, nil
		},
	})
	t.Cleanup(func() { mgr.Close() })

	// Attach live corrald client through Manager.OnHostConnected.
	mgr.OnHostConnected(ctx, u.ID, h.ID, "alice", "fp-gated", "alice", sshClient)
	if _, ok := mgr.ClientForCap(u.ID, h.ID, "pane.send"); !ok {
		t.Fatal("expected corrald client to be connected for pane.send")
	}

	tm := term.NewManager(time.Hour, 10)
	t.Cleanup(func() { tm.Shutdown() })
	res, err := tm.Reserve(u.ID)
	if err != nil {
		t.Fatalf("reserve term slot: %v", err)
	}
	sctx, scancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer scancel()
	sess, err := tm.Start(sctx, res, u.ID, h.ID, "box", sshClient, term.StartOptions{
		Cols: 80,
		Rows: 24,
	})
	if err != nil {
		t.Fatalf("start test term session: %v", err)
	}
	s := &Server{
		store:   st,
		terms:   tm,
		corrald: mgr,
		perf:    perf.New(perf.Options{}),
	}

	return &fakeGatedEnv{
		srv:       s,
		mgr:       mgr,
		sess:      sess,
		sshSrv:    sshSrv,
		sshClient: sshClient,
		sshCount:  sshCount,
		userID:    u.ID,
		hostID:    h.ID,
		sockPath:  sockPath,
	}
}

func newTestTermSession(t *testing.T, ownerID, hostID int64, mode string, c *ssh.Client) *term.Session {
	t.Helper()
	tm := term.NewManager(time.Hour, 10)
	t.Cleanup(func() { tm.Shutdown() })
	res, err := tm.Reserve(ownerID)
	if err != nil {
		t.Fatalf("reserve term slot: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	sess, err := tm.Start(ctx, res, ownerID, hostID, "box", c, term.StartOptions{
		Cols: 80,
		Rows: 24,
	})
	if err != nil {
		t.Fatalf("start test term session: %v", err)
	}
	return sess
}

func TestRunGatedPostSendDisconnectReturns409UnknownResultNoSSH(t *testing.T) {
	var rpcSeen atomic.Int32
	env := newFakeGatedEnv(t, func(conn net.Conn, req proto.Request) {
		if req.Method == proto.MethodPaneSend {
			rpcSeen.Add(1)
			// Request arrived on corrald; immediately drop connection without replying!
			_ = conn.Close()
		}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var released atomic.Int32
	_, err := env.srv.runGated(ctx, env.sess, gatedReq{
		Gate:    "free",
		Mode:    "herdr",
		Pane:    "w1:p1",
		Socket:  "/tmp/h.sock",
		PID:     100,
		Start:   200,
		SID:     "11111111-1111-4111-8111-111111111111",
		Text:    "hello",
		Release: func() { released.Add(1) },
		What:    "send",
	})
	if !errors.Is(err, gw.ErrUnknownResult) {
		t.Fatalf("post-send disconnect err = %v, want gw.ErrUnknownResult", err)
	}
	if rpcSeen.Load() != 1 {
		t.Fatalf("expected 1 corrald pane.send RPC, got %d", rpcSeen.Load())
	}
	if env.sshCount.Load() != 0 {
		t.Fatalf("SSH fallback MUST NOT run after post-send disconnect (sshCount=%d)", env.sshCount.Load())
	}
	if released.Load() != 1 {
		t.Fatalf("Release callback called %d times, want 1", released.Load())
	}

	// Verify HTTP mapping returns 409 corrald_unknown_result (matching frontend contract).
	rec := httptest.NewRecorder()
	writeCorraldUnknownResult(rec)
	if rec.Code != http.StatusConflict {
		t.Fatalf("HTTP status = %d, want 409", rec.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["error"] != "corrald_unknown_result" {
		t.Fatalf("HTTP error code = %v, want corrald_unknown_result", body["error"])
	}

	// Verify writeSwitchError also maps ErrUnknownResult to 409 corrald_unknown_result.
	rec2 := httptest.NewRecorder()
	var outcome, reason string
	writeSwitchError(rec2, err, false, &outcome, &reason)
	if rec2.Code != http.StatusConflict || reason != "corrald_unknown_result" {
		t.Fatalf("writeSwitchError = (%d, %q), want (409, corrald_unknown_result)", rec2.Code, reason)
	}

	// Verify perf recorded corrald bucket with outcome corrald_unknown_result.
	snap := env.srv.perf.Snapshot()
	op, ok := snap.Ops["gated.send"]
	if !ok || op.ByBucket["corrald"].Outcomes["corrald_unknown_result"] != 1 {
		t.Fatalf("unexpected perf snapshot for gated.send: %+v", op)
	}
}

func TestRunGatedPreSendFailureFallsBackToSSHOnce(t *testing.T) {
	env := newFakeGatedEnv(t, func(conn net.Conn, req proto.Request) {
		t.Errorf("unexpected corrald RPC %s", req.Method)
	})

	// Close the underlying client BEFORE runGated runs -> pre-send failure -> SSH fallback.
	cl, ok := env.mgr.ClientForCap(env.userID, env.hostID, "pane.send")
	if !ok {
		t.Fatal("expected active client")
	}
	_ = cl.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var released atomic.Int32
	out, err := env.srv.runGated(ctx, env.sess, gatedReq{
		Gate:    "free",
		Mode:    "herdr",
		Pane:    "w1:p1",
		Socket:  "/tmp/h.sock",
		PID:     100,
		Start:   200,
		SID:     "11111111-1111-4111-8111-111111111111",
		Text:    "hello",
		Release: func() { released.Add(1) },
		What:    "send",
	})
	if err != nil {
		t.Fatalf("pre-send fallback failed: %v", err)
	}
	if string(out) != "ssh-fallback-ok\n" {
		t.Fatalf("out = %q, want ssh-fallback-ok\\n", string(out))
	}
	if env.sshCount.Load() != 1 {
		t.Fatalf("sshCount = %d, want 1", env.sshCount.Load())
	}
	for deadline := time.Now().Add(time.Second); time.Now().Before(deadline) && released.Load() == 0; {
		time.Sleep(5 * time.Millisecond)
	}
	if released.Load() != 1 {
		t.Fatalf("Release callback called %d times, want 1", released.Load())
	}

	snap := env.srv.perf.Snapshot()
	if snap.Ops["gated.send"].ByBucket["ssh"].Outcomes["ok"] != 1 {
		t.Fatalf("expected perf bucket ssh ok=1, got %+v", snap.Ops["gated.send"])
	}
}

func TestRunGatedHandoffInProgressRetriesSameOpIDOnce(t *testing.T) {
	oldBase := gatedHandoffRetryBase
	gatedHandoffRetryBase = 15 * time.Millisecond
	t.Cleanup(func() { gatedHandoffRetryBase = oldBase })

	var mu sync.Mutex
	var seenOpIDs []string
	var execCount atomic.Int32

	env := newFakeGatedEnv(t, func(conn net.Conn, req proto.Request) {
		if req.Method != proto.MethodPaneSend {
			return
		}
		var p proto.PaneSendParams
		_ = json.Unmarshal(req.Params, &p)
		mu.Lock()
		seenOpIDs = append(seenOpIDs, p.OpID)
		attempt := len(seenOpIDs)
		mu.Unlock()

		if attempt <= 2 {
			_ = proto.WriteJSON(conn, proto.Response{
				ID: req.ID,
				Error: &proto.RPCError{
					Code:      proto.ErrHandoffInProgress,
					Message:   "draining",
					Retryable: true,
				},
			})
			return
		}
		execCount.Add(1)
		resBytes, _ := json.Marshal(proto.PaneSendResult{Exit: 0, Out: []byte("corrald-ok\n")})
		_ = proto.WriteJSON(conn, proto.Response{ID: req.ID, Result: resBytes})
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := env.srv.runGated(ctx, env.sess, gatedReq{
		Gate:   "free",
		Mode:   "herdr",
		Pane:   "w1:p1",
		Socket: "/tmp/h.sock",
		PID:    100,
		Start:  200,
		SID:    "11111111-1111-4111-8111-111111111111",
		Text:   "hello",
		What:   "send",
	})
	if err != nil {
		t.Fatalf("runGated handoff retry err: %v", err)
	}
	if string(out) != "corrald-ok\n" {
		t.Fatalf("out = %q, want corrald-ok\\n", string(out))
	}
	if execCount.Load() != 1 {
		t.Fatalf("execCount = %d, want 1", execCount.Load())
	}
	if env.sshCount.Load() != 0 {
		t.Fatalf("sshCount = %d, want 0", env.sshCount.Load())
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seenOpIDs) != 3 {
		t.Fatalf("seenOpIDs = %v, want 3 attempts", seenOpIDs)
	}
	if seenOpIDs[0] == "" || seenOpIDs[0] != seenOpIDs[1] || seenOpIDs[1] != seenOpIDs[2] {
		t.Fatalf("op_id changed across handoff retries: %v", seenOpIDs)
	}
}

func TestRunGatedCapDegradedFallsBackToSSH(t *testing.T) {
	var rpcCount atomic.Int32
	env := newFakeGatedEnv(t, func(conn net.Conn, req proto.Request) {
		if req.Method == proto.MethodPaneSend {
			rpcCount.Add(1)
			// Simulate older corrald that does not implement pane.send -> immediate degrade + SSH fallback.
			_ = proto.WriteJSON(conn, proto.Response{
				ID: req.ID,
				Error: &proto.RPCError{
					Code:    proto.ErrMethodNotFound,
					Message: "unknown method pane.send",
				},
			})
		}
	})

	ctx := context.Background()
	req := gatedReq{
		Gate:   "free",
		Mode:   "herdr",
		Pane:   "w1:p1",
		Socket: "/tmp/h.sock",
		PID:    100,
		Start:  200,
		SID:    "11111111-1111-4111-8111-111111111111",
		Text:   "hello",
		What:   "send",
	}

	// First call hits method_not_found, degrades pane.send immediately, and falls back to SSH.
	out1, err := env.srv.runGated(ctx, env.sess, req)
	if err != nil || string(out1) != "ssh-fallback-ok\n" {
		t.Fatalf("first call = (%q, %v)", string(out1), err)
	}
	if rpcCount.Load() != 1 || env.sshCount.Load() != 1 {
		t.Fatalf("after 1st call: rpc=%d ssh=%d, want 1, 1", rpcCount.Load(), env.sshCount.Load())
	}

	// Second call sees pane.send degraded and goes straight to SSH without calling corrald.
	out2, err := env.srv.runGated(ctx, env.sess, req)
	if err != nil || string(out2) != "ssh-fallback-ok\n" {
		t.Fatalf("second call = (%q, %v)", string(out2), err)
	}
	if rpcCount.Load() != 1 || env.sshCount.Load() != 2 {
		t.Fatalf("after 2nd call: rpc=%d ssh=%d, want 1, 2", rpcCount.Load(), env.sshCount.Load())
	}

	// Non-zero gate exit code (e.g. exit 9) from corrald maps to *sshx.ExitError{Status: 9}
	// and does NOT degrade capability or fall back to SSH.
	env2 := newFakeGatedEnv(t, func(conn net.Conn, req proto.Request) {
		if req.Method == proto.MethodPaneSend {
			resBytes, _ := json.Marshal(proto.PaneSendResult{Exit: 9})
			_ = proto.WriteJSON(conn, proto.Response{ID: req.ID, Result: resBytes})
		}
	})
	_, err9 := env2.srv.runGated(ctx, env2.sess, req)
	var ee *sshx.ExitError
	if !errors.As(err9, &ee) || ee.Status != 9 {
		t.Fatalf("exit 9 err = %v, want *sshx.ExitError{Status: 9}", err9)
	}
	if env2.sshCount.Load() != 0 {
		t.Fatalf("exit 9 must not trigger SSH fallback (sshCount=%d)", env2.sshCount.Load())
	}
	if _, ok := env2.mgr.ClientForCap(env2.userID, env2.hostID, "pane.send"); !ok {
		t.Fatal("gate exit 9 must not degrade pane.send capability")
	}
}

// TestRunGatedErrorFallbackWhitelist verifies Fix 1's strict whitelist rule:
//   - ONLY proven pre-write failures (no client, degraded cap, client closed
//     before write -> gw.ErrNotSent) and whitelisted pre-execution rejection RPC
//     codes (invalid_params, method_not_found, not_supported, pane_not_located)
//     may fall back to SSH (sshCount == 1).
//   - handoff_in_progress retries with the SAME op_id (never falls back to SSH).
//   - EVERY other error (decode error, internal_error, daemon panic, unknown
//     RPC code, timeout after send, disconnect after send, op_unknown,
//     op_expired) returns gw.ErrUnknownResult (HTTP 409) with sshCount == 0.
func TestRunGatedErrorFallbackWhitelist(t *testing.T) {
	baseReq := gatedReq{
		Gate:   "free",
		Mode:   "herdr",
		Pane:   "w1:p1",
		Socket: "/tmp/h.sock",
		PID:    100,
		Start:  200,
		SID:    "11111111-1111-4111-8111-111111111111",
		Text:   "hello",
		What:   "send",
	}

	// 1. Errors that MUST return gw.ErrUnknownResult and NEVER fall back to SSH (sshCount == 0).
	noFallbackCases := []struct {
		name  string
		onRPC func(conn net.Conn, req proto.Request)
		tmo   time.Duration
	}{
		{
			name: "decode_error_malformed_result",
			onRPC: func(conn net.Conn, req proto.Request) {
				if req.Method == proto.MethodPaneSend {
					// Exit expects int; sending string triggers json.Unmarshal decode failure AFTER daemon ran.
					_ = proto.WriteJSON(conn, proto.Response{ID: req.ID, Result: json.RawMessage(`{"exit":"bad-type"}`)})
				}
			},
		},
		{
			name: "internal_error",
			onRPC: func(conn net.Conn, req proto.Request) {
				if req.Method == proto.MethodPaneSend {
					_ = proto.WriteJSON(conn, proto.Response{
						ID:    req.ID,
						Error: &proto.RPCError{Code: proto.ErrInternal, Message: "boom"},
					})
				}
			},
		},
		{
			name: "unknown_rpc_error_code",
			onRPC: func(conn net.Conn, req proto.Request) {
				if req.Method == proto.MethodPaneSend {
					_ = proto.WriteJSON(conn, proto.Response{
						ID:    req.ID,
						Error: &proto.RPCError{Code: "unrecognized_daemon_error", Message: "mystery"},
					})
				}
			},
		},
		{
			name: "timeout_after_send",
			tmo:  180 * time.Millisecond,
			onRPC: func(conn net.Conn, req proto.Request) {
				// Accept request, never reply before ctx expires.
			},
		},
		{
			name: "disconnect_after_send",
			onRPC: func(conn net.Conn, req proto.Request) {
				if req.Method == proto.MethodPaneSend {
					_ = conn.Close()
				}
			},
		},
		{
			name: "op_unknown",
			onRPC: func(conn net.Conn, req proto.Request) {
				if req.Method == proto.MethodPaneSend {
					_ = proto.WriteJSON(conn, proto.Response{
						ID:    req.ID,
						Error: &proto.RPCError{Code: proto.ErrOpUnknown, Message: "unknown after handoff"},
					})
				}
			},
		},
		{
			name: "op_expired",
			onRPC: func(conn net.Conn, req proto.Request) {
				if req.Method == proto.MethodPaneSend {
					_ = proto.WriteJSON(conn, proto.Response{
						ID:    req.ID,
						Error: &proto.RPCError{Code: proto.ErrOpExpired, Message: "expired"},
					})
				}
			},
		},
	}

	for _, tc := range noFallbackCases {
		t.Run("no_fallback/"+tc.name, func(t *testing.T) {
			env := newFakeGatedEnv(t, tc.onRPC)
			tmo := 3 * time.Second
			if tc.tmo > 0 {
				tmo = tc.tmo
			}
			ctx, cancel := context.WithTimeout(context.Background(), tmo)
			defer cancel()
			_, err := env.srv.runGated(ctx, env.sess, baseReq)
			if !errors.Is(err, gw.ErrUnknownResult) {
				t.Fatalf("%s: err = %v, want gw.ErrUnknownResult", tc.name, err)
			}
			if got := env.sshCount.Load(); got != 0 {
				t.Fatalf("%s: sshCount = %d, want 0 (must never fall back to SSH)", tc.name, got)
			}
		})
	}

	// 1b. Real daemon handler panic -> daemon.Server converts to internal_error ->
	// runGated MUST return gw.ErrUnknownResult with sshCount == 0.
	t.Run("no_fallback/real_daemon_panic", func(t *testing.T) {
		home := t.TempDir()
		sockDir, err := os.MkdirTemp("/tmp", "corrald-panic-")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.RemoveAll(sockDir) })
		layout, err := daemon.ResolveLayout(daemon.PathOptions{
			Home:      home,
			MachineID: "aabbccddeeff",
			TmpBase:   sockDir,
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := layout.EnsureStateDir(); err != nil {
			t.Fatal(err)
		}
		dSrv, err := daemon.NewServer(daemon.ServerConfig{
			Layout:      layout,
			Build:       proto.BuildInfo{Version: "0.4.0", Release: 2000},
			Gateway:     "gw-panic",
			IdleTimeout: 5 * time.Minute,
		})
		if err != nil {
			t.Fatal(err)
		}
		uid := uint32(os.Geteuid())
		if _, err := agent.Register(dSrv, agent.ServiceOptions{
			LocatorOpts: agent.LocatorOptions{Home: home, UID: &uid},
		}); err != nil {
			t.Fatal(err)
		}
		// Override pane.send with a handler that panics mid-execution.
		dSrv.Register(proto.MethodPaneSend, daemon.MethodOptions{Mutating: true, Idempotent: true}, func(context.Context, *daemon.Conn, json.RawMessage) (any, *proto.RPCError) {
			panic("simulated post-send panic")
		})
		dCtx, dCancel := context.WithCancel(context.Background())
		dDone := make(chan error, 1)
		go func() { dDone <- dSrv.Serve(dCtx) }()
		t.Cleanup(func() { dCancel(); <-dDone })
		for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
			if c, derr := net.Dial("unix", layout.SocketPath); derr == nil {
				_ = c.Close()
				break
			}
			time.Sleep(15 * time.Millisecond)
		}

		sshCount := &atomic.Int32{}
		sshSrv := testutil.StartSSHServer(t, "alice", "pass", home)
		sshSrv.HandleExec(func(cmd string) func(ssh.Channel) {
			return func(ch ssh.Channel) {
				defer ch.Close()
				if strings.Contains(cmd, "corral-send-") {
					_, _ = io.Copy(io.Discard, ch)
					sshCount.Add(1)
				}
				_, _ = ch.SendRequest("exit-status", false, ssh.Marshal(&struct{ Status uint32 }{0}))
			}
		})
		sshSrv.HandleChannel(func(nc ssh.NewChannel) bool {
			if nc.ChannelType() != "direct-streamlocal@openssh.com" {
				return false
			}
			var payload struct {
				SocketPath string
				Reserved0  string
				Reserved1  uint32
			}
			if err := ssh.Unmarshal(nc.ExtraData(), &payload); err != nil {
				_ = nc.Reject(ssh.ConnectionFailed, "bad payload")
				return true
			}
			uconn, err := net.Dial("unix", payload.SocketPath)
			if err != nil {
				_ = nc.Reject(ssh.ConnectionFailed, err.Error())
				return true
			}
			ch, reqs, err := nc.Accept()
			if err != nil {
				_ = uconn.Close()
				return true
			}
			go ssh.DiscardRequests(reqs)
			go func() { defer ch.Close(); defer uconn.Close(); _, _ = io.Copy(uconn, ch) }()
			go func() { defer ch.Close(); defer uconn.Close(); _, _ = io.Copy(ch, uconn) }()
			return true
		})
		sshClient, err := ssh.Dial("tcp", sshSrv.Addr.String(), &ssh.ClientConfig{
			User: "alice", Auth: []ssh.AuthMethod{ssh.Password("pass")},
			HostKeyCallback: ssh.InsecureIgnoreHostKey(), Timeout: 5 * time.Second,
		})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = sshClient.Close() })

		st, err := store.Open(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = st.Close() })
		ctx := context.Background()
		u, _ := st.CreateUser(ctx, "alice", "Alice", "hash", "admin")
		h, _ := st.CreateHost(ctx, &store.Host{OwnerID: u.ID, Name: "box", Address: "127.0.0.1", Port: 22, Username: "alice", Auth: "password"})
		mb, _ := proto.MarshalManifest(proto.DefaultManifest())
		_ = st.UpsertCorraldHost(ctx, &store.CorraldHost{
			HostID: h.ID, State: gw.StateRunning, BoundFingerprint: "fp-p",
			BoundUsername: "alice", BoundGateway: "gw-panic", ManifestJSON: string(mb),
			Version: "0.4.0", Release: 2000,
		})
		_ = st.RecordSSHVerified(ctx, h.ID, "fp-p", "alice")
		mgr := gw.NewManager(gw.ManagerConfig{
			Store: st, SSH: &testGatedSSHProvider{client: sshClient, fp: "fp-p", user: "alice"},
			Dist:      &gw.Dist{Version: "0.4.0", Release: 2000, Manifest: proto.DefaultManifest(), ManifestHash: "mh"},
			GatewayID: "gw-panic", GatewayVersion: "0.4.0",
			ProbeOverride: func(ctx context.Context, c *ssh.Client) (*gw.HostProbe, error) {
				return &gw.HostProbe{
					OS: "Linux", Arch: runtime.GOARCH, MachineID: "aabbccddeeff", Home: home, UID: strconv.Itoa(int(uid)),
					HomeWritable: true, LibDir: sockDir, StateDir: sockDir, SocketPath: layout.SocketPath,
					HasSymlink: true, HasInstall: true,
				}, nil
			},
		})
		t.Cleanup(func() { mgr.Close() })
		mgr.OnHostConnected(ctx, u.ID, h.ID, "alice", "fp-p", "alice", sshClient)

		s := &Server{store: st, corrald: mgr, perf: perf.New(perf.Options{})}
		sess := newTestTermSession(t, u.ID, h.ID, "herdr", sshClient)

		rctx, rcancel := context.WithTimeout(ctx, 3*time.Second)
		defer rcancel()
		_, err = s.runGated(rctx, sess, baseReq)
		if !errors.Is(err, gw.ErrUnknownResult) {
			t.Fatalf("daemon panic err = %v, want gw.ErrUnknownResult", err)
		}
		if sshCount.Load() != 0 {
			t.Fatalf("daemon panic must not fall back to SSH (sshCount=%d)", sshCount.Load())
		}
	})

	// 1c. handoff_in_progress until deadline -> retries same op_id, never falls back to SSH.
	t.Run("no_fallback/handoff_in_progress_until_deadline", func(t *testing.T) {
		origBase := gatedHandoffRetryBase
		gatedHandoffRetryBase = 25 * time.Millisecond
		t.Cleanup(func() { gatedHandoffRetryBase = origBase })

		var opIDs sync.Map
		var calls atomic.Int32
		env := newFakeGatedEnv(t, func(conn net.Conn, req proto.Request) {
			if req.Method == proto.MethodPaneSend {
				var p proto.PaneSendParams
				_ = json.Unmarshal(req.Params, &p)
				opIDs.Store(p.OpID, true)
				calls.Add(1)
				_ = proto.WriteJSON(conn, proto.Response{
					ID:    req.ID,
					Error: &proto.RPCError{Code: proto.ErrHandoffInProgress, Message: "draining", Retryable: true},
				})
			}
		})
		ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
		defer cancel()
		_, err := env.srv.runGated(ctx, env.sess, baseReq)
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("handoff timeout err = %v, want context.DeadlineExceeded", err)
		}
		if env.sshCount.Load() != 0 {
			t.Fatalf("handoff_in_progress must not fall back to SSH (sshCount=%d)", env.sshCount.Load())
		}
		if calls.Load() < 2 {
			t.Fatalf("expected >= 2 retries during handoff, got %d", calls.Load())
		}
		distinct := 0
		opIDs.Range(func(_, _ any) bool { distinct++; return true })
		if distinct != 1 {
			t.Fatalf("handoff retries used %d distinct op_ids, want 1", distinct)
		}
	})

	// 2. Whitelisted pre-execution rejection codes -> MUST fall back to SSH (sshCount == 1).
	fallbackCodes := []string{
		proto.ErrInvalidParams,
		proto.ErrMethodNotFound,
		proto.ErrNotSupported,
		proto.ErrPaneNotLocated,
	}
	for _, code := range fallbackCodes {
		t.Run("fallback/"+code, func(t *testing.T) {
			env := newFakeGatedEnv(t, func(conn net.Conn, req proto.Request) {
				if req.Method == proto.MethodPaneSend {
					_ = proto.WriteJSON(conn, proto.Response{
						ID:    req.ID,
						Error: &proto.RPCError{Code: code, Message: "rejected before execution"},
					})
				}
			})
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			out, err := env.srv.runGated(ctx, env.sess, baseReq)
			if err != nil || string(out) != "ssh-fallback-ok\n" {
				t.Fatalf("%s: runGated = (%q, %v), want ssh fallback ok", code, string(out), err)
			}
			if got := env.sshCount.Load(); got != 1 {
				t.Fatalf("%s: sshCount = %d, want 1", code, got)
			}
		})
	}

	// 2b. No corrald manager / client -> straight to SSH (sshCount == 1).
	t.Run("fallback/no_client", func(t *testing.T) {
		env := newFakeGatedEnv(t, func(conn net.Conn, req proto.Request) {
			t.Errorf("unexpected corrald RPC %s when corrald is nil", req.Method)
		})
		env.srv.corrald = nil
		out, err := env.srv.runGated(context.Background(), env.sess, baseReq)
		if err != nil || string(out) != "ssh-fallback-ok\n" {
			t.Fatalf("no_client: runGated = (%q, %v)", string(out), err)
		}
		if got := env.sshCount.Load(); got != 1 {
			t.Fatalf("no_client: sshCount = %d, want 1", got)
		}
	})
}

// TestConvoPressReplyUnknownResultRecordsDedupDigest verifies Fix 3:
// when convoPress("reply") receives gw.ErrUnknownResult from corrald, it still
// records answer.digest/at (matching the SSH unknown path) so a second reply
// for the same digest is rejected with 409 already_answered without sending
// again via corrald or SSH.
func TestConvoPressReplyUnknownResultRecordsDedupDigest(t *testing.T) {
	var paneSendCalls atomic.Int32
	sid := "11111111-1111-4111-8111-111111111111"
	digest := strings.Repeat("a", 64)

	env := newFakeGatedEnv(t, func(conn net.Conn, req proto.Request) {
		switch req.Method {
		case proto.MethodAgentLocate:
			// Locate response showing Claude Code running in tmux pane %0 (pid=100, start=200, fg=1).
			out := "tmux=%0 99 /dev/pts/1\x00" +
				"panetty=/dev/pts/1\x00" +
				"pid=100\nstart=200\nprocstart=200\nfg=1\nsid=" + sid + "\nlog=/tmp/fake.jsonl\nagent=claude\nTMUX=/tmp/tmux,99,0\nTMUX_PANE=%0\ntty=/dev/pts/1\x00"
			resBytes, _ := json.Marshal(proto.AgentLocateResult{Out: []byte(out)})
			_ = proto.WriteJSON(conn, proto.Response{ID: req.ID, Result: resBytes})
		case proto.MethodPaneSend:
			paneSendCalls.Add(1)
			// Simulate post-send disconnect -> ErrUnknownResult.
			_ = conn.Close()
		}
	})

	// Reuse env.sess (already registered in env.srv.terms) as a tmux session.
	sess := env.sess
	sess.Persistence = "tmux"
	sess.TmuxSession = "s1"
	env.srv.cfg = &config.Config{}
	env.srv.pending = map[int64]map[*liveSocket]struct{}{}
	env.srv.herdrSocks = herdrSockets{path: map[string]string{}}
	env.srv.auth = auth.NewService(env.srv.store, time.Hour, 24*time.Hour)
	_, loginHash, err := auth.NewToken()
	if err != nil {
		t.Fatal(err)
	}
	if err := env.srv.store.CreateLoginSession(context.Background(), loginHash, env.userID, "", ""); err != nil {
		t.Fatal(err)
	}

	user := &store.User{ID: env.userID, Username: "alice", Role: "admin"}
	doReply := func() *httptest.ResponseRecorder {
		body, _ := json.Marshal(map[string]any{
			"pane":    "%0",
			"pid":     100,
			"start":   200,
			"session": sid,
			"digest":  digest,
			"keys":    []string{"enter"},
		})
		r := httptest.NewRequest(http.MethodPost, "/api/sessions/"+sess.ID+"/conversation/reply", bytes.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		r.SetPathValue("id", sess.ID)
		ctx := context.WithValue(context.WithValue(r.Context(), userKey, user), loginKey, loginHash)
		r = r.WithContext(ctx)
		rec := httptest.NewRecorder()
		env.srv.convoPress(rec, r, "reply")
		return rec
	}

	// 1st reply: post-send disconnect -> 409 corrald_unknown_result, 0 SSH fallback.
	rec1 := doReply()
	if rec1.Code != http.StatusConflict {
		t.Fatalf("1st reply status = %d body=%s, want 409", rec1.Code, rec1.Body.String())
	}
	var b1 map[string]any
	_ = json.Unmarshal(rec1.Body.Bytes(), &b1)
	if b1["error"] != "corrald_unknown_result" {
		t.Fatalf("1st reply error = %v, want corrald_unknown_result", b1["error"])
	}
	if paneSendCalls.Load() != 1 || env.sshCount.Load() != 0 {
		t.Fatalf("after 1st reply: paneSend=%d ssh=%d, want 1, 0", paneSendCalls.Load(), env.sshCount.Load())
	}

	// Wait briefly for the async unlockAnswer goroutine to release the answer turn.
	time.Sleep(50 * time.Millisecond)

	// 2nd reply with SAME digest: MUST be blocked by 409 already_answered!
	rec2 := doReply()
	if rec2.Code != http.StatusConflict {
		t.Fatalf("2nd reply status = %d body=%s, want 409", rec2.Code, rec2.Body.String())
	}
	var b2 map[string]any
	_ = json.Unmarshal(rec2.Body.Bytes(), &b2)
	if b2["error"] != "already_answered" {
		t.Fatalf("2nd reply error = %v, want already_answered (old code skipped recording answer.digest)", b2["error"])
	}
	if paneSendCalls.Load() != 1 || env.sshCount.Load() != 0 {
		t.Fatalf("after 2nd reply: paneSend=%d ssh=%d, want still 1, 0", paneSendCalls.Load(), env.sshCount.Load())
	}
}

// TestFlowsPropagateErrUnknownResultWithoutFollowupKeys verifies Fix 4 across
// modelFlow, codexModelFlow, effortFlow, modelLineFlow, and convoModeTo:
// gw.ErrUnknownResult MUST propagate verbatim to HTTP 409 corrald_unknown_result
// without ever replacing it with errScreen/errUnsure or pressing follow-up
// Escape / Shift+Tab keys.
func TestFlowsPropagateErrUnknownResultWithoutFollowupKeys(t *testing.T) {
	claudeIdle := strings.Join([]string{
		"────────────────────────────────────────────────────────",
		"❯ ",
		"────────────────────────────────────────────────────────",
		"  ? for shortcuts",
	}, "\n")
	claudePicker := strings.Join([]string{
		"Select model",
		"Switch between Claude models.",
		"",
		" ❯ 1. Sonnet 4.5 (current)  Best for everyday tasks",
		"   2. Opus 4.5              Most capable",
		"",
		"Enter to confirm · s to use this session only · Esc to exit",
	}, "\n")
	claudeEffortSlider := strings.Join([]string{
		"Effort",
		"────────▲────────",
		"low  medium  high  xhigh  max",
		"←/→ to adjust · s for this session only · Esc to exit",
	}, "\n")
	codexIdle := cxScreen(t, "modelset")
	codexPickerScr := strings.Join([]string{
		"Select Model and Effort",
		" › 1. gpt-5-codex  Fast coding model",
		"   2. gpt-5-pro    Deeper reasoning",
		"enter select · esc back",
	}, "\n")

	t.Run("modelFlow_list_close_esc_unknown_no_leave", func(t *testing.T) {
		var leaveCalls int
		stepCalls := 0
		step := func(keys []string, text string, read int) (string, error) {
			stepCalls++
			switch stepCalls {
			case 1:
				return claudeIdle, nil // initial look
			case 2:
				return claudePicker, nil // /model opens picker
			case 3:
				// Closing Escape on target == "" returns ErrUnknownResult!
				return "", gw.ErrUnknownResult
			default:
				t.Fatalf("unexpected extra step call #%d: keys=%v text=%q", stepCalls, keys, text)
				return "", nil
			}
		}
		leave := func(keys []string, text string, read int) (string, error) {
			leaveCalls++
			return claudeIdle, nil
		}

		_, err := modelFlow(step, leave, "", "")
		if !errors.Is(err, gw.ErrUnknownResult) {
			t.Fatalf("modelFlow list close err = %v, want gw.ErrUnknownResult", err)
		}
		if leaveCalls != 0 {
			t.Fatalf("leave() called %d times after ErrUnknownResult, want 0", leaveCalls)
		}
		rec := httptest.NewRecorder()
		outcome, reason := "", ""
		writeSwitchError(rec, err, true, &outcome, &reason)
		if rec.Code != http.StatusConflict || reason != "corrald_unknown_result" {
			t.Fatalf("writeSwitchError = (status=%d, reason=%q), want (409, corrald_unknown_result)", rec.Code, reason)
		}
	})

	t.Run("modelFlow_leave_returns_unknown_propagates_and_stops", func(t *testing.T) {
		var leaveCalls int
		stepCalls := 0
		step := func(keys []string, text string, read int) (string, error) {
			stepCalls++
			if stepCalls == 1 {
				return claudeIdle, nil
			}
			return claudePicker, nil
		}
		leave := func(keys []string, text string, read int) (string, error) {
			leaveCalls++
			return "", gw.ErrUnknownResult
		}
		_, err := modelFlow(step, leave, "NonExistentModel", "")
		if !errors.Is(err, gw.ErrUnknownResult) {
			t.Fatalf("modelFlow leave err = %v, want gw.ErrUnknownResult", err)
		}
		if leaveCalls != 1 {
			t.Fatalf("leave() called %d times, want 1 (must stop on ErrUnknownResult)", leaveCalls)
		}
	})

	t.Run("codexModelFlow_undo_returns_unknown_propagates_and_stops", func(t *testing.T) {
		origTries, origGap := codexSettleTries, codexSettleGap
		codexSettleTries, codexSettleGap = 1, time.Millisecond
		t.Cleanup(func() { codexSettleTries, codexSettleGap = origTries, origGap })

		lookN := 0
		do := codexIO{
			look: func() (string, string, error) {
				lookN++
				if lookN == 1 {
					return codexIdle, "d1", nil
				}
				return codexPickerScr, "d2", nil
			},
			press: func(keys []string, text, digest string) error {
				return nil
			},
		}
		var undoPressCalls int
		undo := codexIO{
			look: func() (string, string, error) { return codexPickerScr, "d2", nil },
			press: func(keys []string, text, digest string) error {
				undoPressCalls++
				return gw.ErrUnknownResult
			},
		}
		_, err := codexModelFlow(do, undo, "MissingModel", "high")
		if !errors.Is(err, gw.ErrUnknownResult) {
			t.Fatalf("codexModelFlow undo err = %v, want gw.ErrUnknownResult", err)
		}
		if undoPressCalls != 1 {
			t.Fatalf("undo.press called %d times, want 1", undoPressCalls)
		}
	})

	t.Run("effortFlow_leave_returns_unknown_propagates_and_stops", func(t *testing.T) {
		var leaveCalls int
		stepCalls := 0
		step := func(keys []string, text string, read int) (string, error) {
			stepCalls++
			switch stepCalls {
			case 1:
				return claudeIdle, nil
			case 2:
				return claudeEffortSlider, nil
			case 3:
				// Return wrong screen (slider gone) to trigger out(errNoEffort) -> leave().
				return "no slider here", nil
			default:
				return "", nil
			}
		}
		leave := func(keys []string, text string, read int) (string, error) {
			leaveCalls++
			return "", gw.ErrUnknownResult
		}
		_, err := effortFlow(step, leave, "high")
		if !errors.Is(err, gw.ErrUnknownResult) {
			t.Fatalf("effortFlow leave err = %v, want gw.ErrUnknownResult", err)
		}
		if leaveCalls != 1 {
			t.Fatalf("effortFlow leave() called %d times, want 1", leaveCalls)
		}
	})

	t.Run("modelLineFlow_leave_pressOn_unknown_propagates_409", func(t *testing.T) {
		askScr := strings.Join([]string{
			"▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔",
			"   Switch model?",
			"   Your next response will be slower and use more tokens",
			"",
			"   ❯ 1. Yes, switch to Opus 4.5",
			"     2. No, go back",
		}, "\n")
		var pressKeys []string
		ioMock := lineIO{
			look:  func() (string, error) { return claudeIdle, nil },
			typed: func(text string) error { return nil },
			dialog: func() (string, string, error) {
				return askScr, "dlg-digest", nil
			},
			pressOn: func(digest, key string) error {
				pressKeys = append(pressKeys, key)
				if key == "yes" {
					// Simulate screen_changed on yes, triggering leave() -> pressOn(digest, "esc").
					return &sshx.ExitError{Status: 10}
				}
				return gw.ErrUnknownResult
			},
			logFrom: func(off int64) ([]byte, error) { return nil, nil },
			now:     time.Now,
			sleep:   func(time.Duration) error { return errors.New("stop wait") },
		}
		var off int64
		_, sent, err := modelLineFlow(ioMock, &lineLog{sid: "s1", arg: "opus"}, &off, "Opus 4.5")
		if sent != lineMaybe {
			t.Fatalf("sent = %v, want lineMaybe", sent)
		}
		if !errors.Is(err, gw.ErrUnknownResult) {
			t.Fatalf("modelLineFlow leave unknown err = %v, want gw.ErrUnknownResult (old code swallowed into errUnsure)", err)
		}
		if len(pressKeys) != 2 || pressKeys[0] != "yes" || pressKeys[1] != "esc" {
			t.Fatalf("pressKeys = %v, want [yes esc]", pressKeys)
		}
	})

	t.Run("convoModeTo_second_press_unknown_stops_and_returns_409", func(t *testing.T) {
		var rpcCount atomic.Int32
		env := newFakeGatedEnv(t, func(conn net.Conn, req proto.Request) {
			if req.Method == proto.MethodPaneSend {
				n := rpcCount.Add(1)
				if n == 1 {
					// 1st Shift+Tab succeeds and lands on acceptEdits (not target "plan").
					scr := "────────────────────────────────────\n❯ \n────────────────────────────────────\n  ⏵⏵ accept edits on (shift+tab to cycle)\n"
					resBytes, _ := json.Marshal(proto.PaneSendResult{Exit: 0, Out: []byte(scr)})
					_ = proto.WriteJSON(conn, proto.Response{ID: req.ID, Result: resBytes})
					return
				}
				// 2nd Shift+Tab disconnects post-send -> ErrUnknownResult.
				_ = conn.Close()
			}
		})
		// Override SSH exec to return initial default-mode screen for convoScreenCommand.
		env.sshSrv.HandleExec(func(cmd string) func(ssh.Channel) {
			return func(ch ssh.Channel) {
				defer ch.Close()
				if strings.Contains(cmd, "corral-send-") {
					_, _ = io.Copy(io.Discard, ch)
					env.sshCount.Add(1)
				} else {
					_, _ = ch.Write([]byte(claudeIdle + "\n"))
				}
				_, _ = ch.SendRequest("exit-status", false, ssh.Marshal(&struct{ Status uint32 }{0}))
			}
		})

		rec := httptest.NewRecorder()
		outcome, reason := "refused", ""
		done := make(chan struct{})
		env.srv.convoModeTo(rec, context.Background(), env.sess, "claude", "w1:p1", "/tmp/h.sock", 100, 200,
			"11111111-1111-4111-8111-111111111111", []string{"shift+tab"}, "plan", &outcome, &reason, func() { close(done) })
		<-done
		if rec.Code != http.StatusConflict || reason != "corrald_unknown_result" {
			t.Fatalf("convoModeTo 2nd-press unknown = (status=%d, reason=%q, body=%s), want (409, corrald_unknown_result)", rec.Code, reason, rec.Body.String())
		}
		if rpcCount.Load() != 2 || env.sshCount.Load() != 0 {
			t.Fatalf("convoModeTo stopped counts: rpc=%d ssh=%d, want 2, 0", rpcCount.Load(), env.sshCount.Load())
		}
	})
}
