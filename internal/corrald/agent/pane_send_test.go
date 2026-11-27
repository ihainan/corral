//go:build linux

package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ihainan/corral/internal/corrald/daemon"
	"github.com/ihainan/corral/internal/corrald/gw"
	"github.com/ihainan/corral/internal/corrald/proto"
)

func TestValidateTmuxArgsStrictAllowlist(t *testing.T) {
	good := [][]string{
		{"-u", "display-message", "-p", "-t", "=main:", tmuxFormatSessionPane},
		{"-u", "display-message", "-p", "-t", "%12", tmuxFormatPaneTTY},
		{"-u", "display-message", "-p", "-t", "%12", tmuxFormatPaneTitle},
		{"-u", "display-message", "-p", "-t", "%12", tmuxFormatSessionName},
		{"-L", "sock_1", "-u", "capture-pane", "-p", "-t", "%0"},
		{"-L", "sock_1", "-u", "capture-pane", "-p", "-e", "-t", "%0"},
		{"send-keys", "-t", "%0", "Enter"},
		{"send-keys", "-t", "%0", "BTab"},
		{"send-keys", "-t", "%0", "M-m"},
		{"send-keys", "-t", "%0", "1"},
		{"send-keys", "-t", "%0", "y"},
		{"load-buffer", "-b", "corral-send-123-1", "-"},
		{"paste-buffer", "-p", "-r", "-d", "-b", "corral-send-123-1", "-t", "%0"},
		{"delete-buffer", "-b", "corral-send-123-1"},
	}
	for _, args := range good {
		if err := validateTmuxArgs(args); err != nil {
			t.Errorf("validateTmuxArgs(%v) rejected valid command: %v", args, err)
		}
	}

	bad := [][]string{
		{},
		{"-u", "display-message", "-p", "-t", "%0", "#(id)"},
		{"-u", "capture-pane", "-p", "-S", "-100", "-t", "%0"},
		{"send-keys", "-t", "%0", "-l", "rm -rf /"},
		{"send-keys", "-t", "%0", "rm -rf /"},
		{"send-keys", "-t", "badpane", "Enter"},
		{"load-buffer", "-b", "corral-send-123-1", "/etc/passwd"},
		{"load-buffer", "-b", "../evil", "-"},
		{"paste-buffer", "-p", "-r", "-d", "-b", "evil", "-t", "%0"},
		{"run-shell", "id"},
		{"-L", "../bad", "send-keys", "-t", "%0", "Enter"},
	}
	for _, args := range bad {
		if err := validateTmuxArgs(args); err == nil {
			t.Errorf("validateTmuxArgs(%v) accepted forbidden command", args)
		}
	}
}

func startFakeHerdrSocketServer(t *testing.T, onReq func(method string, params map[string]any) (any, *struct{ Code, Message string })) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "corral-hd-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "h.sock")
	ln, err := net.Listen("unix", sock)
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
				sc := bufio.NewScanner(conn)
				if !sc.Scan() {
					return
				}
				var req struct {
					ID     string         `json:"id"`
					Method string         `json:"method"`
					Params map[string]any `json:"params"`
				}
				if json.Unmarshal(sc.Bytes(), &req) != nil {
					return
				}
				res, apiErr := onReq(req.Method, req.Params)
				resp := map[string]any{"id": req.ID}
				if apiErr != nil {
					resp["error"] = map[string]any{"code": apiErr.Code, "message": apiErr.Message}
				} else {
					if res == nil {
						res = map[string]any{}
					}
					resp["result"] = res
				}
				b, _ := json.Marshal(resp)
				_, _ = conn.Write(append(b, '\n'))
			}(c)
		}
	}()
	return sock
}

func TestHerdrExecutorSocketSecurity(t *testing.T) {
	uid := uint32(os.Geteuid())
	rd := newCapReader("/proc")
	val := NewPathValidator(t.TempDir(), uid, "/proc")

	sock := startFakeHerdrSocketServer(t, func(method string, params map[string]any) (any, *struct{ Code, Message string }) {
		if method == "pane.read" {
			return map[string]any{"read": map[string]any{"text": "screen-ok\n"}}, nil
		}
		return map[string]any{"ok": true}, nil
	})
	exec := NewHerdrExecutor(uid, rd, val)
	ctx := context.Background()

	// 1. Unlocated socket path MUST be rejected even though it is a real owned Unix socket.
	if _, err := exec.ReadPane(ctx, sock, "w1:p1", "visible", 0, false); err == nil {
		t.Fatal("expected unlocated herdr socket to be rejected")
	}

	// 2. Once recorded in located sockets, ReadPane succeeds.
	val.UpdateHerdrSockets(LocateScopeFull, []string{sock})
	txt, err := exec.ReadPane(ctx, sock, "w1:p1", "visible", 0, false)
	if err != nil || txt != "screen-ok\n" {
		t.Fatalf("located socket ReadPane = (%q, %v), want (screen-ok\\n, nil)", txt, err)
	}

	// 3. Symlink pointing to a real socket MUST be rejected even if registered.
	symPath := filepath.Join(filepath.Dir(sock), "sym.sock")
	if err := os.Symlink(sock, symPath); err != nil {
		t.Fatal(err)
	}
	val.UpdateHerdrSockets("tree:1", []string{symPath})
	if _, err := exec.ReadPane(ctx, symPath, "w1:p1", "visible", 0, false); err == nil {
		t.Fatal("expected symlink herdr socket to be rejected")
	}

	// 4. Regular file MUST be rejected even if registered.
	regPath := filepath.Join(filepath.Dir(sock), "not-a-sock")
	if err := os.WriteFile(regPath, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	val.UpdateHerdrSockets("tree:2", []string{regPath})
	if _, err := exec.ReadPane(ctx, regPath, "w1:p1", "visible", 0, false); err == nil {
		t.Fatal("expected regular file herdr socket to be rejected")
	}

	// 5. UID mismatch MUST be rejected.
	wrongUIDExec := NewHerdrExecutor(uid+1, rd, val)
	if _, err := wrongUIDExec.ReadPane(ctx, sock, "w1:p1", "visible", 0, false); err == nil {
		t.Fatal("expected uid mismatch to be rejected")
	}
}

func startPaneSendTestDaemon(t *testing.T, extraCfg func(*daemon.ServerConfig)) (*daemon.Server, *Service, string, func()) {
	t.Helper()
	home := t.TempDir()
	// Short socket path inside /tmp to avoid 99-byte unix socket limit.
	sockDir, err := os.MkdirTemp("", "corrald-ps-")
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

	cfg := daemon.ServerConfig{
		Layout:      layout,
		Build:       proto.BuildInfo{Version: "test", Release: 100},
		Gateway:     "gw.test",
		IdleTimeout: 5 * time.Minute,
	}
	if extraCfg != nil {
		extraCfg(&cfg)
	}
	srv, err := daemon.NewServer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	svc, err := Register(srv, ServiceOptions{
		LocatorOpts: LocatorOptions{Home: home},
	})
	if err != nil {
		t.Fatal(err)
	}

	ready := make(chan struct{})
	cfgReady := srv
	_ = cfgReady
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		errCh <- srv.Serve(ctx)
	}()

	// Wait until socket accepts connections.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if c, derr := net.Dial("unix", layout.SocketPath); derr == nil {
			_ = c.Close()
			close(ready)
			break
		}
		time.Sleep(15 * time.Millisecond)
	}
	select {
	case <-ready:
	default:
		cancel()
		t.Fatalf("daemon did not become ready")
	}

	var stopOnce sync.Once
	stop := func() {
		stopOnce.Do(func() {
			cancel()
			select {
			case <-errCh:
			case <-time.After(5 * time.Second):
				t.Errorf("daemon did not exit cleanly")
			}
		})
	}
	return srv, svc, layout.SocketPath, stop
}

func dialLocalDaemon(t *testing.T, sockPath string) *gw.Client {
	t.Helper()
	conn, err := net.Dial("unix", sockPath)
	if err != nil {
		t.Fatalf("dial daemon %s: %v", sockPath, err)
	}
	cl := gw.NewClient(conn, nil)
	t.Cleanup(func() { _ = cl.Close() })
	return cl
}

func TestPaneSendOpIDDedupConcurrencyAndEpochRules(t *testing.T) {
	srv, svc, sockPath, stop := startPaneSendTestDaemon(t, nil)
	defer stop()

	// Replace pane.send handler wrapper with a controllable slow mock that still
	// exercises Server's real MethodOptions{Mutating: true, Idempotent: true}
	// and OpTable!
	var execCount atomic.Int32
	started := make(chan struct{}, 4)
	release := make(chan struct{})

	srv.Register(proto.MethodPaneSend, daemon.MethodOptions{Mutating: true, Idempotent: true}, func(ctx context.Context, _ *daemon.Conn, raw json.RawMessage) (any, *proto.RPCError) {
		var p proto.PaneSendParams
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, &proto.RPCError{Code: proto.ErrInvalidParams, Message: err.Error()}
		}
		n := execCount.Add(1)
		select {
		case started <- struct{}{}:
		default:
		}
		<-release
		return proto.PaneSendResult{Exit: 0, Out: []byte("run-" + p.Pane + "-" + string('0'+byte(n)))}, nil
	})
	_ = svc

	cl := dialLocalDaemon(t, sockPath)
	hello1, err := cl.Hello(context.Background(), proto.HelloParams{
		Gateway: "gw.test", GatewayVersion: "1", ProtoMin: 1, ProtoMax: 1,
		Client: proto.ClientIdentity{CorralUser: "alice", HostID: "1"},
	})
	if err != nil {
		t.Fatalf("hello: %v", err)
	}

	opID := proto.NewOpID(time.Now())
	req := proto.PaneSendParams{
		OpID: opID, Mode: "herdr", Pane: "w1:p1", LenSpec: "2", Text: []byte("hi"),
	}

	// 1. Start first request; while it's in flight, send duplicate -> in_progress.
	var wg sync.WaitGroup
	var firstRes *proto.PaneSendResult
	var firstErr error
	wg.Add(1)
	go func() {
		defer wg.Done()
		firstRes, firstErr = cl.PaneSend(context.Background(), req)
	}()

	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("first pane.send did not start")
	}

	_, dupErr := cl.PaneSend(context.Background(), req)
	var rpcErr *proto.RPCError
	if !errors.As(dupErr, &rpcErr) || rpcErr.Code != proto.ErrInProgress || !rpcErr.Retryable {
		t.Fatalf("concurrent duplicate op_id error = %v, want retryable in_progress", dupErr)
	}

	// Let first complete.
	close(release)
	wg.Wait()
	if firstErr != nil || firstRes == nil || firstRes.Exit != 0 || string(firstRes.Out) != "run-w1:p1-1" {
		t.Fatalf("first result = (%+v, %v)", firstRes, firstErr)
	}

	// 2. Repeat same op_id in same epoch -> cached first result, handler NOT re-executed.
	secondRes, secondErr := cl.PaneSend(context.Background(), req)
	if secondErr != nil || secondRes == nil || string(secondRes.Out) != "run-w1:p1-1" {
		t.Fatalf("cached duplicate result = (%+v, %v)", secondRes, secondErr)
	}
	if execCount.Load() != 1 {
		t.Fatalf("handler executed %d times for same op_id, want 1", execCount.Load())
	}

	// 3. Expired op_id (> OpDedupWindow old) -> op_expired (mapped to ErrUnknownResult by CallMutating).
	oldOpID := proto.NewOpID(time.Now().Add(-proto.OpDedupWindow - time.Minute))
	oldReq := req
	oldReq.OpID = oldOpID
	_, expErr := cl.PaneSend(context.Background(), oldReq)
	if !errors.Is(expErr, gw.ErrUnknownResult) {
		t.Fatalf("expired op_id error = %v, want ErrUnknownResult", expErr)
	}
	if execCount.Load() != 1 {
		t.Fatalf("expired op_id executed handler (count=%d)", execCount.Load())
	}

	// 4. Snapshot/Restore across handoff (new epoch):
	//    - completed op_id stays cached (not re-executed)
	//    - in-flight op_id becomes OpStateUnknown -> op_unknown -> ErrUnknownResult
	inFlightOp := proto.NewOpID(time.Now())
	admitted, _, _ := srv.Ops().Begin(inFlightOp)
	if !admitted {
		t.Fatal("failed to begin synthetic in-flight op")
	}
	opsSnap := srv.Ops().Snapshot()
	srv.Ops().Finish(inFlightOp, nil, nil)

	_ = cl.Close()
	stop()

	// Start a fresh daemon (new epoch) restored from the handoff snapshot.
	srv2, _, sockPath2, stop2 := startPaneSendTestDaemon(t, func(cfg *daemon.ServerConfig) {
		cfg.InitialSnapshot = &daemon.HandoffSnapshot{
			Schema: daemon.HandoffSnapshotSchema,
			Ops:    opsSnap,
		}
	})
	defer stop2()

	var execCount2 atomic.Int32
	srv2.Register(proto.MethodPaneSend, daemon.MethodOptions{Mutating: true, Idempotent: true}, func(ctx context.Context, _ *daemon.Conn, raw json.RawMessage) (any, *proto.RPCError) {
		execCount2.Add(1)
		return proto.PaneSendResult{Exit: 0, Out: []byte("fresh-epoch")}, nil
	})

	cl2 := dialLocalDaemon(t, sockPath2)
	hello2, err := cl2.Hello(context.Background(), proto.HelloParams{
		Gateway: "gw.test", GatewayVersion: "1", ProtoMin: 1, ProtoMax: 1,
		Client: proto.ClientIdentity{CorralUser: "alice", HostID: "1"},
	})
	if err != nil {
		t.Fatalf("hello2: %v", err)
	}
	if hello2.Epoch == hello1.Epoch {
		t.Fatalf("expected new epoch after handoff, got same %q", hello2.Epoch)
	}

	// Completed op_id carried across handoff returns cached result without re-running.
	afterRes, afterErr := cl2.PaneSend(context.Background(), req)
	if afterErr != nil || afterRes == nil || string(afterRes.Out) != "run-w1:p1-1" {
		t.Fatalf("post-handoff completed op_id = (%+v, %v), want cached run-w1:p1-1", afterRes, afterErr)
	}
	if execCount2.Load() != 0 {
		t.Fatalf("post-handoff completed op_id re-executed handler (%d)", execCount2.Load())
	}

	// In-flight op_id carried across handoff returns op_unknown -> ErrUnknownResult.
	unkReq := req
	unkReq.OpID = inFlightOp
	_, unkErr := cl2.PaneSend(context.Background(), unkReq)
	if !errors.Is(unkErr, gw.ErrUnknownResult) {
		t.Fatalf("post-handoff in-flight op_id error = %v, want ErrUnknownResult", unkErr)
	}
	if execCount2.Load() != 0 {
		t.Fatalf("post-handoff in-flight op_id re-executed handler (%d)", execCount2.Load())
	}

	// Fresh op_id in new epoch executes normally.
	freshReq := req
	freshReq.OpID = proto.NewOpID(time.Now())
	freshRes, freshErr := cl2.PaneSend(context.Background(), freshReq)
	if freshErr != nil || freshRes == nil || string(freshRes.Out) != "fresh-epoch" {
		t.Fatalf("fresh op_id = (%+v, %v), want fresh-epoch", freshRes, freshErr)
	}
	if execCount2.Load() != 1 {
		t.Fatalf("fresh op_id execCount = %d, want 1", execCount2.Load())
	}
	_ = cl2.Close()
}
