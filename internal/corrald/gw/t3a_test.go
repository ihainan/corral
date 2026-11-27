package gw

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/ihainan/corral/internal/corrald/proto"
	"github.com/ihainan/corral/internal/store"
	"github.com/ihainan/corral/internal/testutil"
)

func TestServeCommandTmuxFlag(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("linux shell test")
	}

	tmp := t.TempDir()
	argsFile := filepath.Join(tmp, "args.txt")
	stubBin := filepath.Join(tmp, "corrald")
	stubScript := `#!/bin/sh
printf '%s\n' "$@" >> ` + argsFile + `
if [ "$STUB_REJECT_TMUX" = "1" ]; then
  for a in "$@"; do
    if [ "$a" = "--tmux" ]; then
      echo "error=invalid_tmux"
      exit 1
    fi
  done
fi
echo "result=ok"
echo "pid=4321"
echo "socket=/tmp/test.sock"
`
	if err := os.WriteFile(stubBin, []byte(stubScript), 0o700); err != nil {
		t.Fatal(err)
	}

	// 1. When tmux is found on PATH, ServeCommand passes --tmux <path>.
	binDir := filepath.Join(tmp, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	fakeTmux := filepath.Join(binDir, "tmux")
	if err := os.WriteFile(fakeTmux, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	cmdStr := ServeCommand(stubBin, "gw.test")
	c1 := exec.Command("sh", "-c", cmdStr)
	c1.Env = []string{"PATH=" + binDir + ":/usr/bin:/bin"}
	out1, err := c1.CombinedOutput()
	if err != nil {
		t.Fatalf("serve with tmux failed: %v\n%s", err, out1)
	}
	gotArgs1, _ := os.ReadFile(argsFile)
	if !strings.Contains(string(gotArgs1), "--tmux\n"+fakeTmux+"\n") {
		t.Fatalf("expected --tmux %s in argv, got:\n%s", fakeTmux, gotArgs1)
	}

	// 2. When tmux is NOT on PATH, ServeCommand omits --tmux.
	_ = os.Remove(argsFile)
	emptyBin := filepath.Join(tmp, "emptybin")
	_ = os.MkdirAll(emptyBin, 0o755)
	// Provide only minimal shell utils (no tmux) by symlinking nothing and using PATH without tmux;
	// check whether /usr/bin/tmux or /bin/tmux exists — if so, hide it via wrapper dir.
	noTmuxDir := filepath.Join(tmp, "notmux")
	_ = os.MkdirAll(noTmuxDir, 0o755)
	for _, util := range []string{"sh", "grep", "printf"} {
		if p, err := exec.LookPath(util); err == nil {
			_ = os.Symlink(p, filepath.Join(noTmuxDir, util))
		}
	}
	c2 := exec.Command("/bin/sh", "-c", cmdStr)
	c2.Env = []string{"PATH=" + noTmuxDir}
	out2, err := c2.CombinedOutput()
	if err != nil {
		t.Fatalf("serve without tmux failed: %v\n%s", err, out2)
	}
	gotArgs2, _ := os.ReadFile(argsFile)
	if strings.Contains(string(gotArgs2), "--tmux") {
		t.Fatalf("expected no --tmux when tmux absent, got:\n%s", gotArgs2)
	}

	// 3. When corrald rejects --tmux with error=invalid_tmux, ServeCommand retries without --tmux.
	_ = os.Remove(argsFile)
	c3 := exec.Command("sh", "-c", cmdStr)
	c3.Env = []string{"PATH=" + binDir + ":/usr/bin:/bin", "STUB_REJECT_TMUX=1"}
	out3, err := c3.CombinedOutput()
	if err != nil {
		t.Fatalf("serve invalid_tmux fallback failed: %v\n%s", err, out3)
	}
	if ParseKeyValues(out3)["result"] != "ok" {
		t.Fatalf("expected result=ok after fallback, got:\n%s", out3)
	}
	gotArgs3 := string(mustReadFile(t, argsFile))
	if !strings.Contains(gotArgs3, "--tmux\n"+fakeTmux) || !strings.HasSuffix(gotArgs3, "serve\n--gateway\ngw.test\n") {
		t.Fatalf("expected retry without --tmux after invalid_tmux, got:\n%s", gotArgs3)
	}
}

func TestLifecycleBusyAutoRetry(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("linux test")
	}
	home := t.TempDir()
	srv := testutil.StartSSHServer(t, "alice", "pass", home)

	var serveCalls, uninstallCalls atomic.Int32
	writeReply := func(out string, rc uint32) func(ch ssh.Channel) {
		return func(ch ssh.Channel) {
			defer ch.Close()
			_, _ = ch.Write([]byte(out))
			_, _ = ch.SendRequest("exit-status", false, ssh.Marshal(&struct{ Status uint32 }{rc}))
		}
	}
	srv.HandleExec(func(cmd string) func(ch ssh.Channel) {
		switch {
		case strings.Contains(cmd, " serve --gateway"):
			n := serveCalls.Add(1)
			if n <= 2 {
				return writeReply("error=busy\ndetail=lock held\n", 1)
			}
			return writeReply("result=ok\npid=777\nsocket=/tmp/corrald-test.sock\n", 0)
		case strings.Contains(cmd, " uninstall"):
			n := uninstallCalls.Add(1)
			if n <= 2 {
				return writeReply("error=busy\ndetail=lock held\n", 1)
			}
			return writeReply("result=removed\nlast_machine=true\n", 0)
		case strings.Contains(cmd, "/removing"):
			return writeReply("result=ok\n", 0)
		default:
			return writeReply("result=ok\n", 0)
		}
	})

	cc := &ssh.ClientConfig{
		User:            "alice",
		Auth:            []ssh.AuthMethod{ssh.Password("pass")},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         5 * time.Second,
	}
	sshClient, err := ssh.Dial("tcp", srv.Addr.String(), cc)
	if err != nil {
		t.Fatalf("dial test ssh: %v", err)
	}
	defer sshClient.Close()

	fastPolicy := BusyRetryPolicy{Interval: 10 * time.Millisecond, MaxRetries: 5}
	pid, sock, fail := RunServeWithRetry(context.Background(), sshClient, "/bin/corrald", "gw.test", "0123456789ab", "Linux", "amd64", fastPolicy)
	if fail != nil {
		t.Fatalf("RunServeWithRetry failed: %v", fail)
	}
	if pid != 777 || sock != "/tmp/corrald-test.sock" || serveCalls.Load() != 3 {
		t.Fatalf("unexpected serve result: pid=%d sock=%q calls=%d", pid, sock, serveCalls.Load())
	}

	probe := &HostProbe{LibDir: "/lib", StateDir: "/state", SocketPath: "/tmp/corrald-test.sock"}
	uout := RunUninstallWithRetry(context.Background(), sshClient, probe, "op12345678", fastPolicy, nil)
	if uout.Result != "removed" || uninstallCalls.Load() != 3 {
		t.Fatalf("unexpected uninstall result: %+v calls=%d", uout, uninstallCalls.Load())
	}
}

func TestAgentChangedEventAcceptedAndCursorTracksEpoch(t *testing.T) {
	c1, c2 := net.Pipe()
	defer c1.Close()
	defer c2.Close()

	var events []proto.Event
	done := make(chan struct{})
	client := NewClient(c1, func(ev proto.Event) {
		events = append(events, ev)
		if len(events) == 2 {
			close(done)
		}
	})
	defer client.Close()

	// Emit an agent.changed event in epoch-1 seq 42, then a new epoch-2 seq 1.
	data1, _ := json.Marshal(proto.AgentChangedEventData{
		AddedPIDs:   []int{1234},
		ChangedLogs: []string{"/home/alice/.claude/projects/p/s.jsonl"},
	})
	if err := proto.WriteJSON(c2, proto.Event{
		Event: proto.EventAgentChanged,
		Epoch: "epoch-1",
		Seq:   42,
		Data:  data1,
	}); err != nil {
		t.Fatal(err)
	}
	if err := proto.WriteJSON(c2, proto.Event{
		Event: proto.EventAgentChanged,
		Epoch: "epoch-2",
		Seq:   1,
		Data:  data1,
	}); err != nil {
		t.Fatal(err)
	}

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for agent.changed events")
	}
	if client.Closed() {
		t.Fatal("client should stay open after agent.changed events")
	}
	ep, seq := client.Cursor()
	if ep != "epoch-2" || seq != 1 {
		t.Fatalf("cursor = (%q, %d), want (epoch-2, 1)", ep, seq)
	}
}

func TestCapAllowedByScopeCoversAgentCaps(t *testing.T) {
	base := proto.DefaultManifest()
	grown := proto.DefaultManifest()
	grown.Reads = append(grown.Reads, "agent-logs:newagent")

	for _, capName := range []string{"locate", "tails", "watch", "agent.locate", "convo.tails", "convo.message_tails"} {
		if !CapAllowedByScope(base, base, capName) {
			t.Fatalf("CapAllowedByScope(base, base, %q) = false, want true", capName)
		}
		if CapAllowedByScope(base, grown, capName) {
			t.Fatalf("CapAllowedByScope(base, grown, %q) = true, want false", capName)
		}
	}
}

func TestReportCapFailureConsecutiveDegradation(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	u, err := st.CreateUser(ctx, "alice", "Alice", "hash", "admin")
	if err != nil {
		t.Fatal(err)
	}
	h, err := st.CreateHost(ctx, &store.Host{OwnerID: u.ID, Name: "box", Address: "127.0.0.1", Port: 22, Username: "alice", Auth: "password"})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertCorraldHost(ctx, &store.CorraldHost{
		HostID:           h.ID,
		State:            StateRunning,
		BoundFingerprint: "fp-1",
		BoundUsername:    "alice",
		BoundGateway:     "gw-1",
		Version:          "0.3.1",
		Release:          1000,
	}); err != nil {
		t.Fatal(err)
	}

	c1, c2 := net.Pipe()
	defer c1.Close()
	defer c2.Close()
	cl := NewClient(c1, nil)
	defer cl.Close()

	mgr := NewManager(ManagerConfig{
		Store:               st,
		Dist:                &Dist{Version: "0.3.1", Release: 1000, Manifest: proto.DefaultManifest(), ManifestHash: "mh"},
		GatewayID:           "gw-1",
		CapFailureThreshold: 3,
	})
	defer mgr.Close()

	key := accountKey{Fingerprint: "fp-1", Username: "alice"}
	sc := newSharedConn(key)
	sc.client = cl
	sc.health = &proto.HealthResult{Caps: map[string]string{"locate": "ok", "tails": "ok"}}
	mgr.mu.Lock()
	mgr.conns[key] = sc
	mgr.hostConn[h.ID] = key
	mgr.mu.Unlock()

	// Caller cancellation does not degrade capability.
	for i := 0; i < 5; i++ {
		mgr.ReportCapFailure(h.ID, "locate", context.Canceled)
	}
	if _, ok := mgr.ClientForCap(u.ID, h.ID, "locate"); !ok {
		t.Fatal("context.Canceled should not degrade locate")
	}

	// Two consecutive failures + one success resets counter.
	busyErr := &proto.RPCError{Code: proto.ErrBusy, Message: "busy"}
	mgr.ReportCapFailure(h.ID, "locate", busyErr)
	mgr.ReportCapFailure(h.ID, "locate", busyErr)
	mgr.ReportCapSuccess(h.ID, "locate")
	mgr.ReportCapFailure(h.ID, "locate", busyErr)
	if _, ok := mgr.ClientForCap(u.ID, h.ID, "locate"); !ok {
		t.Fatal("locate should still be active after reset + 1 failure")
	}

	// Reach 3 consecutive failures -> locate degrades while tails stays active.
	mgr.ReportCapFailure(h.ID, "locate", busyErr)
	mgr.ReportCapFailure(h.ID, "locate", busyErr)
	if _, ok := mgr.ClientForCap(u.ID, h.ID, "locate"); ok {
		t.Fatal("locate should be degraded after 3 consecutive failures")
	}
	if _, ok := mgr.ClientForCap(u.ID, h.ID, "tails"); !ok {
		t.Fatal("tails should remain active when only locate is degraded")
	}

	// method_not_found degrades immediately on first error.
	mgr.ReportCapFailure(h.ID, "tails", &proto.RPCError{Code: proto.ErrMethodNotFound, Message: "missing"})
	if _, ok := mgr.ClientForCap(u.ID, h.ID, "tails"); ok {
		t.Fatal("tails should degrade immediately on method_not_found")
	}
	if _, ok := mgr.ClientForCap(u.ID, h.ID, "watch"); !ok {
		t.Fatal("other capabilities (watch) should remain active")
	}
}

func mustReadFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

var _ = errors.Is
