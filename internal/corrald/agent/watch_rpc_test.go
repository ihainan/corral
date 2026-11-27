//go:build linux

package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/ihainan/corral/internal/corrald/daemon"
	"github.com/ihainan/corral/internal/corrald/proto"
)

type rpcTestClient struct {
	conn   *net.UnixConn
	nextID uint64
}

func dialRPCTestClient(t *testing.T, sock string) *rpcTestClient {
	t.Helper()
	conn, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: sock, Net: "unix"})
	if err != nil {
		t.Fatalf("dial %s: %v", sock, err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return &rpcTestClient{conn: conn}
}

func (c *rpcTestClient) call(t *testing.T, method string, params any, out any) {
	t.Helper()
	c.nextID++
	id := c.nextID
	var raw json.RawMessage
	if params != nil {
		b, err := json.Marshal(params)
		if err != nil {
			t.Fatal(err)
		}
		raw = b
	}
	if err := proto.WriteJSON(c.conn, proto.Request{ID: id, Method: method, Params: raw}); err != nil {
		t.Fatalf("write %s: %v", method, err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		_ = c.conn.SetReadDeadline(deadline)
		var env proto.Envelope
		if err := proto.ReadJSON(c.conn, &env); err != nil {
			t.Fatalf("read %s response: %v", method, err)
		}
		if env.IsEvent() {
			continue
		}
		if env.ID != id {
			continue
		}
		if env.Error != nil {
			t.Fatalf("%s returned RPC error: %v", method, env.Error)
		}
		if out != nil && len(env.Result) > 0 {
			if err := json.Unmarshal(env.Result, out); err != nil {
				t.Fatalf("unmarshal %s result: %v", method, err)
			}
		}
		return
	}
}

func (c *rpcTestClient) readEvent(t *testing.T, timeout time.Duration, pred func(proto.Event) bool) proto.Event {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		_ = c.conn.SetReadDeadline(deadline)
		var env proto.Envelope
		if err := proto.ReadJSON(c.conn, &env); err != nil {
			t.Fatalf("waiting for event: %v", err)
		}
		if !env.IsEvent() {
			continue
		}
		ev := env.AsEvent()
		if pred == nil || pred(ev) {
			return ev
		}
	}
	t.Fatal("timed out waiting for matching event")
	return proto.Event{}
}

func (c *rpcTestClient) assertNoEvent(t *testing.T, wait time.Duration) {
	t.Helper()
	_ = c.conn.SetReadDeadline(time.Now().Add(wait))
	var env proto.Envelope
	err := proto.ReadJSON(c.conn, &env)
	if err == nil && env.IsEvent() {
		t.Fatalf("received unexpected event after unwatch: %+v", env.AsEvent())
	}
}

func countSelfFDs(t *testing.T) int {
	t.Helper()
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Fatalf("read /proc/self/fd: %v", err)
	}
	return len(entries)
}

// testPIDRegistry scopes Locator and Watcher PID enumeration strictly to
// processes spawned by the current test, preventing real agents running on the
// host under the same UID from polluting test assertions.
type testPIDRegistry struct {
	mu   sync.RWMutex
	pids map[int]struct{}
}

func newTestPIDRegistry() *testPIDRegistry {
	return &testPIDRegistry{pids: make(map[int]struct{})}
}

func (r *testPIDRegistry) add(pid int) {
	r.mu.Lock()
	r.pids[pid] = struct{}{}
	r.mu.Unlock()
}

func (r *testPIDRegistry) list() ([]string, error) {
	r.mu.RLock()
	pids := make([]int, 0, len(r.pids))
	for p := range r.pids {
		pids = append(pids, p)
	}
	r.mu.RUnlock()

	out := make([]string, 0, len(pids))
	for _, p := range pids {
		s := strconv.Itoa(p)
		if _, err := os.Stat(filepath.Join("/proc", s)); err == nil {
			out = append(out, s)
		}
	}
	return out, nil
}

func startTestDaemonWithAgent(t *testing.T, home string, opts ServiceOptions, logBuf ...*bytes.Buffer) (*daemon.Server, *Service, string, *testPIDRegistry) {
	t.Helper()
	reg := newTestPIDRegistry()
	emptyStr := ""
	if opts.LocatorOpts.ListPIDs == nil && opts.LocatorOpts.ProcRoot == "" {
		opts.LocatorOpts.ListPIDs = reg.list
	}
	if opts.LocatorOpts.ClaudeConfigDir == nil {
		opts.LocatorOpts.ClaudeConfigDir = &emptyStr
	}
	if opts.LocatorOpts.XDGDataHome == nil {
		opts.LocatorOpts.XDGDataHome = &emptyStr
	}

	sockTmp, err := os.MkdirTemp("/tmp", "cd-ag-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(sockTmp) })
	layout, err := daemon.ResolveLayout(daemon.PathOptions{
		Home:      home,
		MachineID: "112233445566",
		TmpBase:   sockTmp,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := layout.EnsureStateDir(); err != nil {
		t.Fatal(err)
	}
	ready := make(chan struct{})
	cfg := daemon.ServerConfig{
		Layout:  layout,
		Build:   proto.BuildInfo{Version: "test", Release: 1, Commit: "abc"},
		Gateway: "gw.test",
		OnReady: func() { close(ready) },
	}
	if len(logBuf) > 0 && logBuf[0] != nil {
		cfg.Logger = daemon.NewLogger(logBuf[0])
	}
	srv, err := daemon.NewServer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	svc, err := Register(srv, opts)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- srv.Serve(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	select {
	case <-ready:
	case err := <-done:
		t.Fatalf("daemon Serve exited early: %v", err)
	case <-time.After(3 * time.Second):
		t.Fatal("daemon did not become ready")
	}
	return srv, svc, layout.SocketPath, reg
}

func TestRPCMethodsAndWatchInotifyAndFallback(t *testing.T) {
	home := t.TempDir()
	projDir := filepath.Join(home, ".claude", "projects", "-work")
	sessDir := filepath.Join(home, ".claude", "sessions")
	for _, d := range []string{projDir, sessDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	logPath := filepath.Join(projDir, "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa.jsonl")
	if err := os.WriteFile(logPath, []byte(`{"type":"message","text":"initial"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, svc, sock, reg := startTestDaemonWithAgent(t, home, ServiceOptions{
		MaxWatches:   4,
		PollInterval: 100 * time.Millisecond,
	})
	cl := dialRPCTestClient(t, sock)

	// 1. agent.locate RPC returns base64-encoded out with hostnow=.
	var locRes proto.AgentLocateResult
	cl.call(t, proto.MethodAgentLocate, proto.AgentLocateParams{Mode: "herdr"}, &locRes)
	if !bytes.Contains(locRes.Out, []byte("hostnow=")) {
		t.Fatalf("agent.locate out missing hostnow=: %q", locRes.Out)
	}

	// 2. convo.tails and convo.message_tails over RPC read allowed log and skip disallowed path.
	outsideFile := filepath.Join(t.TempDir(), "secret.txt")
	_ = os.WriteFile(outsideFile, []byte("top-secret\n"), 0o600)

	var tailRes proto.ConvoTailsResult
	cl.call(t, proto.MethodConvoTails, proto.ConvoTailsParams{Logs: []string{logPath, outsideFile}}, &tailRes)
	wantTail := logPath + "\n" + `{"type":"message","text":"initial"}` + "\n\x00"
	if string(tailRes.Out) != wantTail {
		t.Fatalf("convo.tails:\ngot:  %q\nwant: %q", tailRes.Out, wantTail)
	}

	var msgRes proto.ConvoTailsResult
	cl.call(t, proto.MethodConvoMessageTails, proto.ConvoTailsParams{Logs: []string{logPath, outsideFile}}, &msgRes)
	if string(msgRes.Out) != wantTail {
		t.Fatalf("convo.message_tails:\ngot:  %q\nwant: %q", msgRes.Out, wantTail)
	}

	// 3. agent.watch in inotify mode detects file growth and emits hint-only agent.changed.
	var watchRes proto.AgentWatchResult
	cl.call(t, proto.MethodAgentWatch, proto.AgentWatchParams{Logs: []string{logPath}}, &watchRes)
	if !watchRes.Watching || watchRes.Mode != WatchModeInotify || watchRes.MaxFiles != 4 {
		t.Fatalf("unexpected watch result: %+v", watchRes)
	}

	var health proto.HealthResult
	cl.call(t, proto.MethodDaemonHealth, nil, &health)
	if health.Caps["locate"] != "ok" || health.Caps["tails"] != "ok" || health.Caps["watch"] != "ok" {
		t.Fatalf("unexpected health caps: %+v", health.Caps)
	}

	time.Sleep(80 * time.Millisecond)
	const secretBody = "SENSITIVE_CONVO_PAYLOAD_MUST_NOT_APPEAR_IN_EVENT"
	f, err := os.OpenFile(logPath, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString(`{"type":"message","text":"` + secretBody + `"}` + "\n")
	_ = f.Close()

	ev := cl.readEvent(t, 3*time.Second, func(e proto.Event) bool {
		if e.Event != proto.EventAgentChanged {
			return false
		}
		var d proto.AgentChangedEventData
		_ = json.Unmarshal(e.Data, &d)
		for _, p := range d.ChangedLogs {
			if p == logPath {
				return true
			}
		}
		return false
	})
	if strings.Contains(string(ev.Data), secretBody) {
		t.Fatalf("agent.changed leaked file content: %s", ev.Data)
	}

	// 4. Process arrival & exit detection: spawn a fake binary named "claude".
	fakeBinDir := t.TempDir()
	sleepBin, err := exec.LookPath("sleep")
	if err != nil {
		t.Skip("no sleep binary")
	}
	fakeClaude := filepath.Join(fakeBinDir, "claude")
	if err := os.Symlink(sleepBin, fakeClaude); err != nil {
		t.Fatal(err)
	}
	proc := exec.Command(fakeClaude, "30")
	if err := proc.Start(); err != nil {
		t.Fatal(err)
	}
	childPID := proc.Process.Pid
	reg.add(childPID)
	t.Cleanup(func() {
		_ = proc.Process.Kill()
		_ = proc.Wait()
	})

	cl.readEvent(t, 3*time.Second, func(e proto.Event) bool {
		if e.Event != proto.EventAgentChanged {
			return false
		}
		var d proto.AgentChangedEventData
		_ = json.Unmarshal(e.Data, &d)
		for _, p := range d.AddedPIDs {
			if p == childPID {
				return true
			}
		}
		return false
	})

	_ = proc.Process.Kill()
	_ = proc.Wait()

	cl.readEvent(t, 3*time.Second, func(e proto.Event) bool {
		if e.Event != proto.EventAgentChanged {
			return false
		}
		var d proto.AgentChangedEventData
		_ = json.Unmarshal(e.Data, &d)
		for _, p := range d.RemovedPIDs {
			if p == childPID {
				return true
			}
		}
		return false
	})

	// 5. Exceeding MaxWatches (4) triggers poll fallback and updates daemon.health!
	extraLogs := make([]string, 0, 5)
	for i := 0; i < 5; i++ {
		p := filepath.Join(projDir, fmt.Sprintf("bbbbbbbb-bbbb-4bbb-8bbb-00000000000%d.jsonl", i))
		if err := os.WriteFile(p, []byte("seed\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		extraLogs = append(extraLogs, p)
	}
	cl.call(t, proto.MethodAgentWatch, proto.AgentWatchParams{Logs: extraLogs}, &watchRes)

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		cl.call(t, proto.MethodDaemonHealth, nil, &health)
		if health.Caps["watch"] == "degraded:poll_fallback(limit=4)" {
			break
		}
		time.Sleep(30 * time.Millisecond)
	}
	if health.Caps["watch"] != "degraded:poll_fallback(limit=4)" {
		t.Fatalf("expected poll fallback health after exceeding limit, got %q", health.Caps["watch"])
	}

	targetPoll := extraLogs[4]
	pf, err := os.OpenFile(targetPoll, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = pf.WriteString("poll-growth\n")
	_ = pf.Close()

	cl.readEvent(t, 3*time.Second, func(e proto.Event) bool {
		if e.Event != proto.EventAgentChanged {
			return false
		}
		var d proto.AgentChangedEventData
		_ = json.Unmarshal(e.Data, &d)
		for _, p := range d.ChangedLogs {
			if p == targetPoll {
				return true
			}
		}
		return false
	})

	// 6. StateProvider Snapshot/Restore preserves known located logs.
	svc.Validator().UpdateDiscovered(LocateScopeFull, []string{logPath}, nil, nil)
	snap, err := svc.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	freshVal := NewPathValidator(t.TempDir(), uint32(os.Geteuid()), "/proc")
	freshSvc := &Service{val: freshVal}
	if err := freshSvc.Restore(snap); err != nil {
		t.Fatal(err)
	}
	if _, ok := freshVal.ValidatePath(logPath); !ok {
		t.Fatalf("restored validator lost known located log %s", logPath)
	}

	// 7. Stop watching via agent.watch {stop:true} returns to idle.
	cl.call(t, proto.MethodAgentWatch, proto.AgentWatchParams{Stop: true}, &watchRes)
	if watchRes.Watching || watchRes.Mode != WatchModeIdle {
		t.Fatalf("expected idle after stop, got %+v", watchRes)
	}
}

// Review #3, #4, #5, #6:
//   - unwatch/stop unsubscribes from EventBus even while other clients remain watching (#6)
//   - repeated watch/unwatch/disconnect/reconnect 1000 times under -race leaks zero goroutines and zero FDs (#3, #4, #5)
func TestWatchChurn1000IterationsNoLeakAndUnwatchStopsEvents(t *testing.T) {
	home := t.TempDir()
	projDir := filepath.Join(home, ".claude", "projects", "-churn")
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(projDir, "dddddddd-dddd-4ddd-8ddd-dddddddddddd.jsonl")
	if err := os.WriteFile(logPath, []byte("seed\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, svc, sock, _ := startTestDaemonWithAgent(t, home, ServiceOptions{
		MaxWatches:   DefaultMaxInotifyWatches,
		PollInterval: 500 * time.Millisecond,
	})

	// Part A (#6): c1 unwatches while c2 keeps watching; c1 must receive NO events.
	c1 := dialRPCTestClient(t, sock)
	c2 := dialRPCTestClient(t, sock)
	var wr proto.AgentWatchResult
	c1.call(t, proto.MethodAgentWatch, proto.AgentWatchParams{Logs: []string{logPath}}, &wr)
	c2.call(t, proto.MethodAgentWatch, proto.AgentWatchParams{Logs: []string{logPath}}, &wr)
	time.Sleep(60 * time.Millisecond)

	c1.call(t, proto.MethodAgentUnwatch, nil, &wr)

	f, err := os.OpenFile(logPath, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString("after-c1-unwatch\n")
	_ = f.Close()

	// c2 MUST receive the event; c1 MUST NOT.
	c2.readEvent(t, 2*time.Second, func(e proto.Event) bool {
		return e.Event == proto.EventAgentChanged
	})
	c1.assertNoEvent(t, 250*time.Millisecond)

	// Same check for agent.watch {stop:true}:
	c1.call(t, proto.MethodAgentWatch, proto.AgentWatchParams{Logs: []string{logPath}}, &wr)
	c1.call(t, proto.MethodAgentWatch, proto.AgentWatchParams{Stop: true}, &wr)
	f, _ = os.OpenFile(logPath, os.O_APPEND|os.O_WRONLY, 0o600)
	_, _ = f.WriteString("after-c1-stop\n")
	_ = f.Close()
	c2.readEvent(t, 2*time.Second, func(e proto.Event) bool {
		return e.Event == proto.EventAgentChanged
	})
	c1.assertNoEvent(t, 250*time.Millisecond)

	// Detach c2 and close c1/c2 to establish clean baseline.
	c2.call(t, proto.MethodAgentUnwatch, nil, &wr)
	_ = c1.conn.Close()
	_ = c2.conn.Close()
	time.Sleep(80 * time.Millisecond)

	runtime.GC()
	baseGoroutines := runtime.NumGoroutine()
	baseFDs := countSelfFDs(t)

	// Part B (#5): 500 watch/unwatch cycles on one persistent connection (including repeated watch updates).
	persist := dialRPCTestClient(t, sock)
	for i := 0; i < 500; i++ {
		persist.call(t, proto.MethodAgentWatch, proto.AgentWatchParams{Logs: []string{logPath}}, &wr)
		if i%2 == 0 {
			persist.call(t, proto.MethodAgentUnwatch, nil, &wr)
		}
	}
	persist.call(t, proto.MethodAgentUnwatch, nil, &wr)
	_ = persist.conn.Close()

	// Part C (#3 & #4): 500 concurrent/rapid connect -> watch -> disconnect cycles.
	const totalReconnects = 500
	const workers = 4
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			for i := 0; i < totalReconnects/workers; i++ {
				conn, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: sock, Net: "unix"})
				if err != nil {
					t.Errorf("worker %d iter %d dial: %v", workerID, i, err)
					return
				}
				raw, _ := json.Marshal(proto.AgentWatchParams{Logs: []string{logPath}})
				_ = proto.WriteJSON(conn, proto.Request{ID: 1, Method: proto.MethodAgentWatch, Params: raw})
				_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
				var env proto.Envelope
				_ = proto.ReadJSON(conn, &env)
				if i%2 == 0 {
					_ = proto.WriteJSON(conn, proto.Request{ID: 2, Method: proto.MethodAgentUnwatch})
					_ = proto.ReadJSON(conn, &env)
				}
				_ = conn.Close()
			}
		}(w)
	}
	wg.Wait()

	// Wait briefly for server connection cleanup goroutines to finish.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if !svc.Watcher().Status().Watching && runtime.NumGoroutine() <= baseGoroutines+6 && countSelfFDs(t) <= baseFDs+4 {
			break
		}
		runtime.GC()
		time.Sleep(25 * time.Millisecond)
	}

	st := svc.Watcher().Status()
	if st.Watching || st.Mode != WatchModeIdle || st.WatchedFiles != 0 {
		t.Fatalf("watcher not idle after churn: %+v", st)
	}
	afterGoroutines := runtime.NumGoroutine()
	afterFDs := countSelfFDs(t)
	t.Logf("after 1000 watch/unwatch/reconnect cycles: goroutines %d->%d, fds %d->%d",
		baseGoroutines, afterGoroutines, baseFDs, afterFDs)

	if afterGoroutines > baseGoroutines+6 {
		t.Fatalf("goroutine leak detected: baseline=%d after=%d", baseGoroutines, afterGoroutines)
	}
	if afterFDs > baseFDs+4 {
		t.Fatalf("FD leak detected: baseline=%d after=%d", baseFDs, afterFDs)
	}
}

// Review #7: after log rotation (rename+create or remove+create), Watcher must
// evict the stale watch descriptor and re-bind to the new file at the same path.
func TestWatchRebindsAfterLogRotation(t *testing.T) {
	home := t.TempDir()
	projDir := filepath.Join(home, ".claude", "projects", "-rot")
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(projDir, "eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee.jsonl")
	if err := os.WriteFile(logPath, []byte("gen1\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, _, sock, _ := startTestDaemonWithAgent(t, home, ServiceOptions{
		MaxWatches:   DefaultMaxInotifyWatches,
		PollInterval: 80 * time.Millisecond,
	})
	cl := dialRPCTestClient(t, sock)
	var wr proto.AgentWatchResult
	cl.call(t, proto.MethodAgentWatch, proto.AgentWatchParams{Logs: []string{logPath}}, &wr)
	time.Sleep(80 * time.Millisecond)

	// Rotate via rename + recreate at the original path.
	if err := os.Rename(logPath, logPath+".1"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(logPath, []byte("gen2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Give watcher time to receive IN_MOVE_SELF and re-bind to the new inode.
	time.Sleep(250 * time.Millisecond)

	// Append to the newly rotated file -> must emit agent.changed!
	f, err := os.OpenFile(logPath, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString("gen2-appended\n")
	_ = f.Close()

	cl.readEvent(t, 3*time.Second, func(e proto.Event) bool {
		if e.Event != proto.EventAgentChanged {
			return false
		}
		var d proto.AgentChangedEventData
		_ = json.Unmarshal(e.Data, &d)
		for _, p := range d.ChangedLogs {
			if p == logPath {
				return true
			}
		}
		return false
	})
}

// Review #8: Watcher detects both (a) an existing non-agent PID exec'ing into
// an agent (same PID & starttime, changed comm/exe) and (b) rapid PID reuse
// where (pid, starttime) changes between polls.
func TestWatchDetectsExecIntoAgentAndPIDReuse(t *testing.T) {
	// Part A: real process running `sh` -> `exec <fake-codex>` (same PID & starttime).
	shPath, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	sleepPath, err := exec.LookPath("sleep")
	if err != nil {
		t.Skip("no sleep")
	}
	fakeBinDir := t.TempDir()
	fakeCodex := filepath.Join(fakeBinDir, "codex")
	if err := os.Symlink(sleepPath, fakeCodex); err != nil {
		t.Fatal(err)
	}

	home := t.TempDir()
	readyFile := filepath.Join(t.TempDir(), "sh-ready")
	triggerFile := filepath.Join(t.TempDir(), "go-exec")
	// Script waits as `sh` until triggerFile exists, then `exec`s into `codex`.
	script := fmt.Sprintf("echo ok > %q; while [ ! -f %q ]; do sleep 0.02; done; exec %q 60", readyFile, triggerFile, fakeCodex)
	cmd := exec.Command(shPath, "-c", script)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(readyFile); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	_, _, sock, reg := startTestDaemonWithAgent(t, home, ServiceOptions{
		PollInterval: 60 * time.Millisecond,
	})
	reg.add(pid)
	cl := dialRPCTestClient(t, sock)
	var wr proto.AgentWatchResult
	cl.call(t, proto.MethodAgentWatch, proto.AgentWatchParams{}, &wr)
	// Wait for seedProcesses() to observe `pid` while it is still `sh`.
	time.Sleep(150 * time.Millisecond)

	// Now trigger `exec codex` on the exact same PID!
	if err := os.WriteFile(triggerFile, []byte("go"), 0o600); err != nil {
		t.Fatal(err)
	}

	cl.readEvent(t, 3*time.Second, func(e proto.Event) bool {
		if e.Event != proto.EventAgentChanged {
			return false
		}
		var d proto.AgentChangedEventData
		_ = json.Unmarshal(e.Data, &d)
		for _, p := range d.AddedPIDs {
			if p == pid {
				return true
			}
		}
		return false
	})

	// Part B: synthetic /proc test for rapid PID reuse (same PID, new starttime).
	fakeProc := t.TempDir()
	pidDir := filepath.Join(fakeProc, "4242")
	if err := os.MkdirAll(pidDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFakeStat := func(st string) {
		// 20 fields after ") ": state ppid pgrp session tty_nr tpgid ... field20=starttime
		statLine := fmt.Sprintf("4242 (codex) S 1 4242 4242 0 4242 0 0 0 0 0 0 0 0 0 20 0 1 0 %s 0 0\n", st)
		if err := os.WriteFile(filepath.Join(pidDir, "stat"), []byte(statLine), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	_ = os.WriteFile(filepath.Join(pidDir, "comm"), []byte("codex\n"), 0o600)
	writeFakeStat("1000")

	loc, err := NewLocator(LocatorOptions{Home: home, ProcRoot: fakeProc})
	if err != nil {
		t.Fatal(err)
	}
	val := NewPathValidator(home, uint32(os.Geteuid()), fakeProc)
	w := NewWatcher(WatchWatcherOptions{Locator: loc, Validator: val})
	w.seedProcesses()

	// Simulate PID 4242 exiting and a new codex reusing PID 4242 with starttime=2000.
	writeFakeStat("2000")
	added, removed := w.pollProcesses()
	if len(removed) != 1 || removed[0] != 4242 || len(added) != 1 || added[0] != 4242 {
		t.Fatalf("PID reuse (starttime 1000->2000): got added=%v removed=%v, want [4242] and [4242]", added, removed)
	}
}

// Review #9: auth secret values (ANTHROPIC_AUTH_TOKEN, ANTHROPIC_API_KEY, and
// unrelated secrets in settings.json env) must only be tested for presence,
// never returned in locate output or logs, and readProcEnv must refuse secret caps.
func TestSecretsNeverMaterializedOrLeaked(t *testing.T) {
	const (
		sentinelTok   = "SENTINEL_AUTH_TOKEN_SECRET_987654321"
		sentinelKey   = "SENTINEL_API_KEY_SECRET_123456789"
		sentinelOther = "SENTINEL_OTHER_SETTINGS_SECRET_555555"
	)

	// 1. capReader.readProcEnv must panic if a secret env cap is requested.
	rd := newCapReader("/proc")
	for _, secretCap := range []string{proto.ReadEnvAnthropicAuthToken, proto.ReadEnvAnthropicAPIKey} {
		panicked := false
		func() {
			defer func() {
				if recover() != nil {
					panicked = true
				}
			}()
			_, _ = rd.readProcEnv("/proc/self", secretCap)
		}()
		if !panicked {
			t.Fatalf("readProcEnv did not refuse secret capability %q", secretCap)
		}
	}

	// 2. End-to-end RPC locate with sentinels in BOTH settings.json AND process environ.
	sleepBin, err := exec.LookPath("sleep")
	if err != nil {
		t.Skip("no sleep")
	}
	home := t.TempDir()
	sessDir := filepath.Join(home, ".claude", "sessions")
	projDir := filepath.Join(home, ".claude", "projects", "-sec")
	for _, d := range []string{sessDir, projDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	_ = os.WriteFile(filepath.Join(home, ".claude", "settings.json"), []byte(fmt.Sprintf(`{
		"env": {
			"ANTHROPIC_BASE_URL": "https://user:urlsecret@gw.example.com/v1?token=%s",
			"ANTHROPIC_AUTH_TOKEN": "%s",
			"ANTHROPIC_API_KEY": "%s",
			"UNRELATED_SECRET": "%s",
			"CLAUDE_AUTOCOMPACT_PCT_OVERRIDE": "80"
		},
		"autoCompactWindow": 200000
	}`, sentinelOther, sentinelTok, sentinelKey, sentinelOther)), 0o600)

	// Spawn a fake Claude process with secret env vars.
	p1 := exec.Command(sleepBin, "60")
	p1.Env = []string{
		"HOME=" + home,
		"PATH=/usr/bin:/bin",
		"ANTHROPIC_API_KEY=" + sentinelKey,
		"ANTHROPIC_AUTH_TOKEN=" + sentinelTok,
		"HERDR_PANE_ID=w1:p1",
	}
	if err := p1.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p1.Process.Kill(); _ = p1.Wait() })

	st, _, err := daemon.ProcStartTime(p1.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	const sid = "ffffffff-ffff-4fff-8fff-ffffffffffff"
	_ = os.WriteFile(filepath.Join(sessDir, strconv.Itoa(p1.Process.Pid)+".json"),
		[]byte(fmt.Sprintf(`{"pid":%d,"sessionId":"%s","procStart":"%s"}`, p1.Process.Pid, sid, st)), 0o600)

	var logBuf bytes.Buffer
	_, _, sock, reg := startTestDaemonWithAgent(t, home, ServiceOptions{}, &logBuf)
	reg.add(p1.Process.Pid)
	cl := dialRPCTestClient(t, sock)

	var locRes proto.AgentLocateResult
	cl.call(t, proto.MethodAgentLocate, proto.AgentLocateParams{Mode: "herdr"}, &locRes)

	outStr := string(locRes.Out)
	if !strings.Contains(outStr, "auth=token\n") || !strings.Contains(outStr, "ep=gw.example.com\n") || !strings.Contains(outStr, "pct=80") {
		t.Fatalf("locate did not derive expected non-secret metadata: %q", outStr)
	}

	// Assert none of the sentinels appear in RPC output or daemon logs.
	for _, s := range []string{sentinelTok, sentinelKey, sentinelOther, "urlsecret"} {
		if strings.Contains(outStr, s) {
			t.Fatalf("secret sentinel %q leaked into agent.locate output", s)
		}
		if strings.Contains(logBuf.String(), s) {
			t.Fatalf("secret sentinel %q leaked into daemon logs", s)
		}
	}
}

func TestLocateNoProc(t *testing.T) {
	loc, err := NewLocator(LocatorOptions{
		Home:     t.TempDir(),
		ProcRoot: filepath.Join(t.TempDir(), "no-such-proc"),
	})
	if err != nil {
		t.Fatal(err)
	}
	got := loc.Locate(context.Background(), proto.AgentLocateParams{Mode: "herdr"})
	if string(got) != "noproc\x00" {
		t.Fatalf("noproc output = %q, want %q", got, "noproc\x00")
	}
}

func TestAgentWatchIdleOverhead(t *testing.T) {
	home := t.TempDir()
	projDir := filepath.Join(home, ".claude", "projects", "-bench")
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatal(err)
	}
	logs := make([]string, 0, 50)
	for i := 0; i < 50; i++ {
		p := filepath.Join(projDir, fmt.Sprintf("cccccccc-cccc-4ccc-8ccc-%012d.jsonl", i))
		if err := os.WriteFile(p, []byte(`{"type":"message","text":"idle"}`+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		logs = append(logs, p)
	}

	emptyStr := ""
	loc, err := NewLocator(LocatorOptions{
		Home:            home,
		ListPIDs:        func() ([]string, error) { return nil, nil },
		ClaudeConfigDir: &emptyStr,
		XDGDataHome:     &emptyStr,
	})
	if err != nil {
		t.Fatal(err)
	}
	val := NewPathValidator(home, uint32(os.Geteuid()), "/proc")
	w := NewWatcher(WatchWatcherOptions{
		Locator:      loc,
		Validator:    val,
		MaxWatches:   DefaultMaxInotifyWatches,
		PollInterval: 1 * time.Second,
	})
	defer w.Stop()

	st := w.Subscribe("bench-sub", logs)
	if !st.Watching || st.Mode != WatchModeInotify {
		t.Fatalf("expected inotify watch mode, got %+v", st)
	}
	time.Sleep(150 * time.Millisecond)

	runtime.GC()
	var memBefore, memAfter runtime.MemStats
	runtime.ReadMemStats(&memBefore)
	var ruBefore, ruAfter syscall.Rusage
	_ = syscall.Getrusage(syscall.RUSAGE_SELF, &ruBefore)
	wallStart := time.Now()

	time.Sleep(2200 * time.Millisecond)

	wall := time.Since(wallStart)
	_ = syscall.Getrusage(syscall.RUSAGE_SELF, &ruAfter)
	runtime.ReadMemStats(&memAfter)

	userDelta := timevalDuration(ruAfter.Utime) - timevalDuration(ruBefore.Utime)
	sysDelta := timevalDuration(ruAfter.Stime) - timevalDuration(ruBefore.Stime)
	cpuTotal := userDelta + sysDelta
	cpuPct := float64(cpuTotal) / float64(wall) * 100
	allocDeltaKB := float64(memAfter.TotalAlloc-memBefore.TotalAlloc) / 1024.0
	heapInuseKB := float64(memAfter.HeapInuse) / 1024.0
	rssKB := daemon.ProcRSSKB(0)

	t.Logf("agent.watch idle (50 files, inotify, 1s /proc poll, wall=%v): cpu=%v (user=%v sys=%v, %.2f%%), totalAllocDelta=%.1f KiB, heapInuse=%.1f KiB, procRSS=%d KiB",
		wall.Round(time.Millisecond), cpuTotal, userDelta, sysDelta, cpuPct, allocDeltaKB, heapInuseKB, rssKB)

	if cpuPct > 20.0 {
		t.Fatalf("idle watcher CPU too high: %.2f%%", cpuPct)
	}
}

// TestAgentTestsIsolatedFrom20HostNoiseAgents spawns 20 unrelated background
// processes that look like real Claude Code & Codex agents with live log files
// under the current UID, proves an un-scoped Locator discovers all 20 of them
// (reproducing the multi-agent host condition), and then verifies that an
// isolated Service (ListPIDs scoped to test PIDs, MaxWatches=4) stays in
// inotify mode with WatchedFiles=1 and ignores noise log growth & exits.
func TestAgentTestsIsolatedFrom20HostNoiseAgents(t *testing.T) {
	shPath, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	sleepPath, err := exec.LookPath("sleep")
	if err != nil {
		t.Skip("no sleep")
	}

	noiseHome := t.TempDir()
	noiseClaudeCfg := filepath.Join(noiseHome, "noise-claude")
	noiseClaudeSess := filepath.Join(noiseClaudeCfg, "sessions")
	noiseClaudeProj := filepath.Join(noiseClaudeCfg, "projects", "-noise")
	noiseCodexHome := filepath.Join(noiseHome, "noise-codex")
	noiseCodexSess := filepath.Join(noiseCodexHome, "sessions", "2026", "11", "27")
	noiseBinDir := filepath.Join(noiseHome, "bin")
	for _, d := range []string{noiseClaudeSess, noiseClaudeProj, noiseCodexSess, noiseBinDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	noiseClaudeBin := filepath.Join(noiseBinDir, "claude")
	noiseCodexBin := filepath.Join(noiseBinDir, "codex")
	if err := os.Symlink(sleepPath, noiseClaudeBin); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(shPath, noiseCodexBin); err != nil {
		t.Fatal(err)
	}

	var noiseCmds []*exec.Cmd
	var noiseLogs []string
	t.Cleanup(func() {
		for _, c := range noiseCmds {
			if c.Process != nil {
				_ = c.Process.Kill()
				_ = c.Wait()
			}
		}
	})

	// Spawn 10 noise Claude processes with custom CLAUDE_CONFIG_DIR + session JSON + log.
	for i := 0; i < 10; i++ {
		c := exec.Command(noiseClaudeBin, "60")
		c.Env = []string{
			"HOME=" + noiseHome,
			"PATH=/usr/bin:/bin",
			"CLAUDE_CONFIG_DIR=" + noiseClaudeCfg,
			fmt.Sprintf("HERDR_PANE_ID=noise:c%d", i),
		}
		if err := c.Start(); err != nil {
			t.Fatal(err)
		}
		noiseCmds = append(noiseCmds, c)
		pid := c.Process.Pid
		st, _, err := daemon.ProcStartTime(pid)
		if err != nil {
			t.Fatal(err)
		}
		sid := fmt.Sprintf("10101010-1010-4010-8010-%012d", i+1)
		logFile := filepath.Join(noiseClaudeProj, sid+".jsonl")
		if err := os.WriteFile(logFile, []byte(`{"type":"user"}`+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(noiseClaudeSess, strconv.Itoa(pid)+".json"),
			[]byte(fmt.Sprintf(`{"pid":%d,"sessionId":"%s","procStart":"%s"}`, pid, sid, st)), 0o600); err != nil {
			t.Fatal(err)
		}
		noiseLogs = append(noiseLogs, logFile)
	}

	// Spawn 10 noise Codex processes holding open rollout logs via shell redirection.
	for i := 0; i < 10; i++ {
		sid := fmt.Sprintf("20202020-2020-4020-8020-%012d", i+1)
		logFile := filepath.Join(noiseCodexSess, "rollout-2026-11-27T01-00-00-"+sid+".jsonl")
		if err := os.WriteFile(logFile, []byte(`{"type":"session_meta","payload":{"cwd":"/tmp"}}`+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		// Keep logFile open on FD 3 while process comm is "codex".
		c := exec.Command(noiseCodexBin, "-c", fmt.Sprintf("exec 3<%q; while :; do read -t 1 _ </dev/null || :; done", logFile))
		c.Env = []string{
			"HOME=" + noiseHome,
			"PATH=/usr/bin:/bin",
			"CODEX_HOME=" + noiseCodexHome,
			fmt.Sprintf("HERDR_PANE_ID=noise:x%d", i),
		}
		if err := c.Start(); err != nil {
			t.Fatal(err)
		}
		noiseCmds = append(noiseCmds, c)
		noiseLogs = append(noiseLogs, logFile)
	}
	time.Sleep(150 * time.Millisecond)

	// 1. Confirm an un-scoped Locator (production /proc scan) actually discovers
	// the noise agents' logs (proving that without isolation MaxWatches=4 fails).
	var unscopedLogs []string
	emptyStr := ""
	unscopedLoc, err := NewLocator(LocatorOptions{
		Home:            t.TempDir(),
		ClaudeConfigDir: &emptyStr,
		XDGDataHome:     &emptyStr,
		OnDiscovered: func(_ string, logs, _, _ []string) {
			unscopedLogs = logs
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = unscopedLoc.Locate(context.Background(), proto.AgentLocateParams{Mode: "herdr"})
	noiseSeen := 0
	for _, l := range unscopedLogs {
		if strings.HasPrefix(l, noiseHome+"/") {
			noiseSeen++
		}
	}
	if noiseSeen < 10 {
		t.Fatalf("expected un-scoped Locator to see noise agent logs (proving host-noise reproduction), got %d", noiseSeen)
	}

	// 2. Now run the isolated Service with MaxWatches=4 while all 20 noise agents
	// are alive: locate + watch must NOT see any noise agent or degrade to poll.
	testHome := t.TempDir()
	projDir := filepath.Join(testHome, ".claude", "projects", "-isolated")
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatal(err)
	}
	ownLog := filepath.Join(projDir, "30303030-3030-4030-8030-303030303030.jsonl")
	if err := os.WriteFile(ownLog, []byte("seed\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, _, sock, _ := startTestDaemonWithAgent(t, testHome, ServiceOptions{
		MaxWatches:   4,
		PollInterval: 60 * time.Millisecond,
	})
	cl := dialRPCTestClient(t, sock)

	var locRes proto.AgentLocateResult
	cl.call(t, proto.MethodAgentLocate, proto.AgentLocateParams{Mode: "herdr"}, &locRes)
	if strings.Contains(string(locRes.Out), noiseHome) {
		t.Fatalf("isolated agent.locate leaked noise agent from host: %q", locRes.Out)
	}

	var watchRes proto.AgentWatchResult
	cl.call(t, proto.MethodAgentWatch, proto.AgentWatchParams{Logs: []string{ownLog}}, &watchRes)
	if !watchRes.Watching || watchRes.Mode != WatchModeInotify || watchRes.WatchedFiles != 1 {
		t.Fatalf("isolated watch affected by 20 noise agents: %+v", watchRes)
	}

	var health proto.HealthResult
	cl.call(t, proto.MethodDaemonHealth, nil, &health)
	if health.Caps["watch"] != "ok" {
		t.Fatalf("isolated watch health = %q, want ok", health.Caps["watch"])
	}

	// Modify a noise log and kill a noise agent: isolated watcher must emit NO event.
	time.Sleep(80 * time.Millisecond)
	nf, err := os.OpenFile(noiseLogs[0], os.O_APPEND|os.O_WRONLY, 0o600)
	if err == nil {
		_, _ = nf.WriteString("noise-write\n")
		_ = nf.Close()
	}
	_ = noiseCmds[0].Process.Kill()
	_ = noiseCmds[0].Wait()
	noiseCmds[0].Process = nil
	cl.assertNoEvent(t, 200*time.Millisecond)

	// Modifying the test's own log still fires agent.changed immediately.
	of, err := os.OpenFile(ownLog, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = of.WriteString("own-write\n")
	_ = of.Close()
	cl.readEvent(t, 2*time.Second, func(e proto.Event) bool {
		if e.Event != proto.EventAgentChanged {
			return false
		}
		var d proto.AgentChangedEventData
		_ = json.Unmarshal(e.Data, &d)
		for _, p := range d.ChangedLogs {
			if p == ownLog {
				return true
			}
		}
		return false
	})
}

func timevalDuration(tv syscall.Timeval) time.Duration {
	return time.Duration(tv.Sec)*time.Second + time.Duration(tv.Usec)*time.Microsecond
}

// Round 3 #4: concurrent agent.watch and agent.unwatch calls on the SAME
// connection must serialize atomically so EventBus and Watcher never diverge.
func TestWatchUnwatchSameConnSerialized(t *testing.T) {
	home := t.TempDir()
	projDir := filepath.Join(home, ".claude", "projects", "-r4")
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(projDir, "44444444-4444-4444-8444-444444444444.jsonl")
	if err := os.WriteFile(logPath, []byte("seed\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, svc, sock, _ := startTestDaemonWithAgent(t, home, ServiceOptions{
		MaxWatches:   DefaultMaxInotifyWatches,
		PollInterval: 2 * time.Second,
	})

	// Race watch + unwatch concurrently on the same connection over pipelined RPC.
	cl := dialRPCTestClient(t, sock)
	var wr proto.AgentWatchResult
	cl.call(t, proto.MethodAgentWatch, proto.AgentWatchParams{Logs: []string{logPath}}, &wr)

	// Extract the server-side *daemon.Conn from watchConns to directly stress
	// concurrent handleWatch / handleUnwatch calls on the exact same *daemon.Conn.
	svc.watchMu.Lock()
	var serverConn *daemon.Conn
	for c := range svc.watchConns {
		serverConn = c
		break
	}
	svc.watchMu.Unlock()
	if serverConn == nil {
		t.Fatal("serverConn not registered in watchConns")
	}

	rawWatch, _ := json.Marshal(proto.AgentWatchParams{Logs: []string{logPath}})
	for iter := 0; iter < 50; iter++ {
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, _ = svc.handleWatch(context.Background(), serverConn, rawWatch)
		}()
		go func() {
			defer wg.Done()
			_, _ = svc.handleUnwatch(context.Background(), serverConn, nil)
		}()
		wg.Wait()

		// After every concurrent race, EventBus subscription and Watcher state
		// MUST agree: either both subscribed or both unsubscribed!
		svc.watchMu.Lock()
		_, inMap := svc.watchConns[serverConn]
		svc.watchMu.Unlock()
		st := svc.Watcher().Status()
		if inMap != st.Watching {
			t.Fatalf("iter %d: Bus/watchConns (%v) diverged from Watcher.Watching (%v)", iter, inMap, st.Watching)
		}
	}

	// End cleanly with unwatch and confirm no stale subscription remains.
	_, _ = svc.handleUnwatch(context.Background(), serverConn, nil)
	if st := svc.Watcher().Status(); st.Watching {
		t.Fatalf("expected watcher idle after final unwatch, got %+v", st)
	}
	f, err := os.OpenFile(logPath, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString("should-not-deliver\n")
	_ = f.Close()
	cl.assertNoEvent(t, 150*time.Millisecond)
}

// Round 3 #6: when a watched log is rotated (rename + immediate create/write)
// before reconcileWatches finishes, Watcher must immediately emit a change hint
// for that log path even without any subsequent append.
func TestWatchRotationEmitsImmediateHintBeforeRebind(t *testing.T) {
	home := t.TempDir()
	projDir := filepath.Join(home, ".claude", "projects", "-rot-immediate")
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(projDir, "66666666-6666-4666-8666-666666666666.jsonl")
	if err := os.WriteFile(logPath, []byte("gen1\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Use a long PollInterval so stat polling cannot mask a missed inotify hint.
	_, _, sock, _ := startTestDaemonWithAgent(t, home, ServiceOptions{
		MaxWatches:   DefaultMaxInotifyWatches,
		PollInterval: 30 * time.Second,
	})
	cl := dialRPCTestClient(t, sock)
	var wr proto.AgentWatchResult
	cl.call(t, proto.MethodAgentWatch, proto.AgentWatchParams{Logs: []string{logPath}}, &wr)
	time.Sleep(80 * time.Millisecond)

	// Rotate atomically and write new content IMMEDIATELY with NO later append.
	if err := os.Rename(logPath, logPath+".old"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(logPath, []byte("gen2-first-write-only\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	cl.readEvent(t, 2*time.Second, func(e proto.Event) bool {
		if e.Event != proto.EventAgentChanged {
			return false
		}
		var d proto.AgentChangedEventData
		_ = json.Unmarshal(e.Data, &d)
		for _, p := range d.ChangedLogs {
			if p == logPath {
				return true
			}
		}
		return false
	})
}

// Round 3 #7: when an existing agent PID is re-classified to a different agent
// kind (e.g. claude exec'ing into codex with the same PID & starttime) OR its
// active log set changes, Watcher must treat it as changed and emit hints.
func TestWatchDetectsAgentToAgentExecAndLogChange(t *testing.T) {
	shPath, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	sleepPath, err := exec.LookPath("sleep")
	if err != nil {
		t.Skip("no sleep")
	}

	fakeBinDir := t.TempDir()
	fakeClaudeSh := filepath.Join(fakeBinDir, "claude")
	fakeCodex := filepath.Join(fakeBinDir, "codex")
	if err := os.Symlink(shPath, fakeClaudeSh); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(sleepPath, fakeCodex); err != nil {
		t.Fatal(err)
	}

	home := t.TempDir()
	readyFile := filepath.Join(t.TempDir(), "claude-ready")
	triggerFile := filepath.Join(t.TempDir(), "exec-codex")
	// Starts with /proc/<pid>/comm == "claude", then execs into "codex" on the
	// exact same PID and starttime.
	script := fmt.Sprintf("echo ok > %q; while [ ! -f %q ]; do sleep 0.02; done; exec %q 60", readyFile, triggerFile, fakeCodex)
	cmd := exec.Command(fakeClaudeSh, "-c", script)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(readyFile); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	_, _, sock, reg := startTestDaemonWithAgent(t, home, ServiceOptions{
		PollInterval: 60 * time.Millisecond,
	})
	reg.add(pid)
	cl := dialRPCTestClient(t, sock)
	var wr proto.AgentWatchResult
	cl.call(t, proto.MethodAgentWatch, proto.AgentWatchParams{}, &wr)
	// Let seedProcesses observe pid while it is classified as "claude".
	time.Sleep(150 * time.Millisecond)

	// Trigger in-place exec from claude -> codex.
	if err := os.WriteFile(triggerFile, []byte("go"), 0o600); err != nil {
		t.Fatal(err)
	}

	cl.readEvent(t, 3*time.Second, func(e proto.Event) bool {
		if e.Event != proto.EventAgentChanged {
			return false
		}
		var d proto.AgentChangedEventData
		_ = json.Unmarshal(e.Data, &d)
		sawAdd, sawRem := false, false
		for _, p := range d.AddedPIDs {
			if p == pid {
				sawAdd = true
			}
		}
		for _, p := range d.RemovedPIDs {
			if p == pid {
				sawRem = true
			}
		}
		return sawAdd && sawRem
	})

	// Part B: same PID, same agentKind ("claude"), same starttime/comm/exe,
	// but active session/log changes -> pollProcesses reports the PID change.
	fakeProc := t.TempDir()
	pidDir := filepath.Join(fakeProc, "5151")
	sessDir := filepath.Join(home, ".claude", "sessions")
	projDir := filepath.Join(home, ".claude", "projects", "-p")
	for _, d := range []string{pidDir, sessDir, projDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	statLine := "5151 (claude) S 1 5151 5151 0 5151 0 0 0 0 0 0 0 0 0 20 0 1 0 12345 0 0\n"
	_ = os.WriteFile(filepath.Join(pidDir, "stat"), []byte(statLine), 0o600)
	_ = os.WriteFile(filepath.Join(pidDir, "comm"), []byte("claude\n"), 0o600)
	_ = os.WriteFile(filepath.Join(pidDir, "environ"), []byte("HOME="+home+"\x00"), 0o600)

	const (
		sidA = "11111111-1111-4111-8111-111111111111"
		sidB = "22222222-2222-4222-8222-222222222222"
	)
	_ = os.WriteFile(filepath.Join(projDir, sidA+".jsonl"), []byte("a\n"), 0o600)
	_ = os.WriteFile(filepath.Join(projDir, sidB+".jsonl"), []byte("b\n"), 0o600)
	_ = os.WriteFile(filepath.Join(sessDir, "5151.json"), []byte(`{"pid":5151,"sessionId":"`+sidA+`","procStart":"12345"}`), 0o600)

	emptyStr := ""
	loc, err := NewLocator(LocatorOptions{
		Home:            home,
		ProcRoot:        fakeProc,
		ClaudeConfigDir: &emptyStr,
		XDGDataHome:     &emptyStr,
	})
	if err != nil {
		t.Fatal(err)
	}
	val := NewPathValidator(home, uint32(os.Geteuid()), fakeProc)
	w := NewWatcher(WatchWatcherOptions{Locator: loc, Validator: val})
	w.seedProcesses()

	// Switch session file to sidB without changing PID/starttime/comm/exe.
	_ = os.WriteFile(filepath.Join(sessDir, "5151.json"), []byte(`{"pid":5151,"sessionId":"`+sidB+`","procStart":"12345"}`), 0o600)
	added, _ := w.pollProcesses()
	if len(added) != 1 || added[0] != 5151 {
		t.Fatalf("expected log-set change on PID 5151 to emit added=[5151], got added=%v", added)
	}
}

// Round 4 #2: when a watched log is deleted, the delete hint is emitted,
// reconcileWatches runs while the file is still absent (simulating a gateway
// seeing ENOENT), and THEN a new file is created and completely written before
// rebinding (with no subsequent append), rebinding MUST emit a second change
// hint for the new file before adopting its size as the new baseline.
func TestWatchRotationHintAfterDeleteGapBeforeRebind(t *testing.T) {
	home := t.TempDir()
	projDir := filepath.Join(home, ".claude", "projects", "-rot-gap")
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(projDir, "77777777-7777-4777-8777-777777777777.jsonl")
	if err := os.WriteFile(logPath, []byte("gen1-initial\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, _, sock, _ := startTestDaemonWithAgent(t, home, ServiceOptions{
		MaxWatches:   DefaultMaxInotifyWatches,
		PollInterval: 60 * time.Millisecond,
	})
	cl := dialRPCTestClient(t, sock)
	var wr proto.AgentWatchResult
	cl.call(t, proto.MethodAgentWatch, proto.AgentWatchParams{Logs: []string{logPath}}, &wr)
	time.Sleep(80 * time.Millisecond)

	isTargetLogEvent := func(e proto.Event) bool {
		if e.Event != proto.EventAgentChanged {
			return false
		}
		var d proto.AgentChangedEventData
		_ = json.Unmarshal(e.Data, &d)
		for _, p := range d.ChangedLogs {
			if p == logPath {
				return true
			}
		}
		return false
	}

	// 1. Delete the file without recreating it yet.
	if err := os.Remove(logPath); err != nil {
		t.Fatal(err)
	}
	// Consume hint #1 (immediate delete hint).
	cl.readEvent(t, 2*time.Second, isTargetLogEvent)

	// 2. Wait across multiple poll ticks while logPath does not exist so
	// reconcileWatches observes the missing file (gateway would see ENOENT).
	time.Sleep(200 * time.Millisecond)

	// 3. Create the new file and finish writing its content in one shot with
	// NO further appends afterward.
	if err := os.WriteFile(logPath, []byte("gen2-complete-before-rebind\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// 4. Rebinding must detect the new file and immediately emit hint #2!
	cl.readEvent(t, 2*time.Second, isTargetLogEvent)

	// 5. After adopting the new baseline, no spurious extra hints should fire.
	cl.assertNoEvent(t, 200*time.Millisecond)

	// 6. A subsequent append to the newly bound file still triggers hint #3.
	f, err := os.OpenFile(logPath, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString("gen2-later-append\n")
	_ = f.Close()
	cl.readEvent(t, 2*time.Second, isTargetLogEvent)
}
