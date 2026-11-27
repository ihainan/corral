//go:build linux

package web

import (
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

	s := &Server{
		store:   st,
		corrald: mgr,
		perf:    perf.New(perf.Options{}),
	}
	sess := &term.Session{
		ID:          "sess-1",
		OwnerID:     u.ID,
		HostID:      h.ID,
		Persistence: "herdr",
	}
	// Inject sshClient into term.Session via helper or test wrapper:
	// Set the unexported client field using a real term.Session? Let's check!
	// Wait: sess.Client() returns the SSH client; we can create a real term.Session
	// or check how term.Session is constructed in web tests!
	sess = newTestTermSession(t, u.ID, h.ID, "herdr", sshClient)

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
