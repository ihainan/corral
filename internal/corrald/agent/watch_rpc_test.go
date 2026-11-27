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

func startTestDaemonWithAgent(t *testing.T, home string, opts ServiceOptions) (*daemon.Server, *Service, string) {
	t.Helper()
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
	srv, err := daemon.NewServer(daemon.ServerConfig{
		Layout:  layout,
		Build:   proto.BuildInfo{Version: "test", Release: 1, Commit: "abc"},
		Gateway: "gw.test",
		OnReady: func() { close(ready) },
	})
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
	return srv, svc, layout.SocketPath
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

	_, svc, sock := startTestDaemonWithAgent(t, home, ServiceOptions{
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

	// Check daemon.health reports ok for locate, tails, watch.
	var health proto.HealthResult
	cl.call(t, proto.MethodDaemonHealth, nil, &health)
	if health.Caps["locate"] != "ok" || health.Caps["tails"] != "ok" || health.Caps["watch"] != "ok" {
		t.Fatalf("unexpected health caps: %+v", health.Caps)
	}

	// Wait briefly for inotify watch to be registered, then append secret-like content.
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

	// Wait for reconcile to switch to poll mode.
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

	// Verify growth is still detected in poll mode.
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
	svc.Validator().UpdateDiscovered([]string{logPath}, nil, nil)
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

	loc, err := NewLocator(LocatorOptions{Home: home})
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
	// Let initial seed + watch setup complete before measuring idle window.
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

	// Sanity guard: idle watcher must stay very lightweight (<20% CPU even on a busy CI box).
	if cpuPct > 20.0 {
		t.Fatalf("idle watcher CPU too high: %.2f%%", cpuPct)
	}
	_ = strconv.Itoa(0)
}

func timevalDuration(tv syscall.Timeval) time.Duration {
	return time.Duration(tv.Sec)*time.Second + time.Duration(tv.Usec)*time.Microsecond
}
