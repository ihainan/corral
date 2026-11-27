//go:build linux

package daemon

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ihainan/corral/internal/corrald/proto"
)

type builtBinaries struct {
	v1   string
	v2   string
	bad  string
	prod string
}

var (
	buildOnce sync.Once
	built     builtBinaries
	buildErr  error
)

func getTestBinaries(t *testing.T) builtBinaries {
	t.Helper()
	buildOnce.Do(func() {
		repoRoot, err := filepath.Abs(filepath.Join("..", "..", ".."))
		if err != nil {
			buildErr = err
			return
		}
		dir, err := os.MkdirTemp("", "corrald-itest-bins-*")
		if err != nil {
			buildErr = err
			return
		}

		buildOne := func(outPath, version, release, commit string, testTag bool) error {
			args := []string{"build", "-trimpath"}
			if testTag {
				args = append(args, "-tags", "corrald_test")
			}
			ld := fmt.Sprintf("-s -w -X main.version=%s -X main.release=%s -X main.commit=%s", version, release, commit)
			args = append(args, "-ldflags", ld, "-o", outPath, "./cmd/corrald")
			cmd := exec.Command("go", args...)
			cmd.Dir = repoRoot
			cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
			if out, err := cmd.CombinedOutput(); err != nil {
				return fmt.Errorf("go build %s failed: %w\n%s", outPath, err, out)
			}
			return os.Chmod(outPath, 0o700)
		}

		built.v1 = filepath.Join(dir, "corrald-v1")
		if err := buildOne(built.v1, "0.1.0", "100", "aaaa1111", true); err != nil {
			buildErr = err
			return
		}
		built.v2 = filepath.Join(dir, "corrald-v2")
		if err := buildOne(built.v2, "0.2.0", "200", "bbbb2222", true); err != nil {
			buildErr = err
			return
		}
		built.prod = filepath.Join(dir, "corrald-prod")
		if err := buildOne(built.prod, "0.1.0", "100", "cccc3333", false); err != nil {
			buildErr = err
			return
		}

		// Build a real standalone Go binary that immediately exits 1 without writing READY.
		badSrcDir := filepath.Join(dir, "badsrc")
		if err := os.MkdirAll(badSrcDir, 0o700); err != nil {
			buildErr = err
			return
		}
		badMain := filepath.Join(badSrcDir, "main.go")
		if err := os.WriteFile(badMain, []byte("package main\nimport \"os\"\nfunc main(){ os.Exit(1) }\n"), 0o600); err != nil {
			buildErr = err
			return
		}
		built.bad = filepath.Join(dir, "corrald-bad")
		cmd := exec.Command("go", "build", "-o", built.bad, badMain)
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
		if out, err := cmd.CombinedOutput(); err != nil {
			buildErr = fmt.Errorf("build bad binary: %w\n%s", err, out)
			return
		}
		_ = os.Chmod(built.bad, 0o700)
	})
	if buildErr != nil {
		t.Fatalf("build test binaries: %v", buildErr)
	}
	return built
}

func runCLI(t *testing.T, bin string, home string, extraEnv []string, args ...string) (string, int) {
	t.Helper()
	cmd := exec.Command(bin, args...)
	env := []string{
		"HOME=" + home,
		"PATH=/usr/bin:/bin",
	}
	env = append(env, extraEnv...)
	cmd.Env = env
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	code := 0
	if err != nil {
		var exitErr *exec.ExitError
		if ok := errorAs(err, &exitErr); ok {
			code = exitErr.ExitCode()
		} else {
			t.Fatalf("exec %s %v failed: %v", bin, args, err)
		}
	}
	return out.String(), code
}

func errorAs(err error, target **exec.ExitError) bool {
	if ee, ok := err.(*exec.ExitError); ok {
		*target = ee
		return true
	}
	return false
}

func parseKV(out string) map[string]string {
	m := make(map[string]string)
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), "=")
		if ok {
			m[k] = v
		}
	}
	return m
}

func stageBinary(t *testing.T, home, srcBin string) string {
	t.Helper()
	stage := filepath.Join(home, "stage-"+RandomHex(4))
	if err := os.MkdirAll(stage, 0o700); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(stage, "corrald")
	b, err := os.ReadFile(srcBin)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, b, 0o700); err != nil {
		t.Fatal(err)
	}
	return stage
}

type rpcClient struct {
	conn   net.Conn
	nextID atomic.Uint64
}

func dialRPC(t *testing.T, sock string) *rpcClient {
	t.Helper()
	c, err := net.DialTimeout("unix", sock, 3*time.Second)
	if err != nil {
		t.Fatalf("dial %s: %v", sock, err)
	}
	return &rpcClient{conn: c}
}

func (c *rpcClient) close() { _ = c.conn.Close() }

func (c *rpcClient) call(t *testing.T, method string, params any) (proto.Response, []proto.Event) {
	t.Helper()
	id := c.nextID.Add(1)
	var raw json.RawMessage
	if params != nil {
		b, err := json.Marshal(params)
		if err != nil {
			t.Fatal(err)
		}
		raw = b
	}
	_ = c.conn.SetDeadline(time.Now().Add(8 * time.Second))
	if err := proto.WriteJSON(c.conn, proto.Request{ID: id, Method: method, Params: raw}); err != nil {
		t.Fatalf("write %s: %v", method, err)
	}
	var events []proto.Event
	for {
		var env proto.Envelope
		if err := proto.ReadJSON(c.conn, &env); err != nil {
			t.Fatalf("read %s reply: %v", method, err)
		}
		if env.IsEvent() {
			events = append(events, env.AsEvent())
			continue
		}
		if env.ID == id {
			return env.AsResponse(), events
		}
	}
}

func (c *rpcClient) readEvent(t *testing.T, timeout time.Duration) proto.Event {
	t.Helper()
	_ = c.conn.SetReadDeadline(time.Now().Add(timeout))
	var env proto.Envelope
	if err := proto.ReadJSON(c.conn, &env); err != nil {
		t.Fatalf("readEvent: %v", err)
	}
	if !env.IsEvent() {
		t.Fatalf("expected event frame, got %+v", env)
	}
	return env.AsEvent()
}

func TestIntegrationLifecycleAndResume(t *testing.T) {
	bins := getTestBinaries(t)
	home := t.TempDir()
	env := []string{"CORRALD_TEST_MACHINE_ID=111122223333"}

	// Ensure clean teardown even on test failure.
	installedBin := filepath.Join(home, ".local", "lib", "corral", "corrald")
	t.Cleanup(func() {
		if _, err := os.Lstat(installedBin); err == nil {
			_, _ = runCLI(t, installedBin, home, env, "uninstall")
		}
	})

	// 1. version, manifest, selftest.
	out, code := runCLI(t, bins.v1, home, env, "version")
	kv := parseKV(out)
	if code != 0 || kv["result"] != "ok" || kv["version"] != "0.1.0" || kv["release"] != "100" || kv["protocol"] != "1" {
		t.Fatalf("version unexpected (code=%d):\n%s", code, out)
	}

	out, code = runCLI(t, bins.v1, home, env, "manifest")
	if code != 0 {
		t.Fatalf("manifest failed (code=%d): %s", code, out)
	}
	var mf proto.Manifest
	if err := json.Unmarshal([]byte(out), &mf); err != nil || mf.Schema != 1 || len(mf.Reads) == 0 {
		t.Fatalf("manifest JSON invalid: %v\n%s", err, out)
	}

	out, code = runCLI(t, bins.v1, home, env, "selftest")
	kv = parseKV(out)
	if code != 0 || kv["result"] != "ok" || kv["mid"] != "111122223333" {
		t.Fatalf("selftest unexpected (code=%d):\n%s", code, out)
	}

	// 2. install.
	stage := stageBinary(t, home, bins.v1)
	out, code = runCLI(t, filepath.Join(stage, "corrald"), home, env,
		"install", "--stage", stage, "--op", "op0001", "--gateway", "gw.test", "--by", "alice")
	kv = parseKV(out)
	if code != 0 || kv["result"] != "ok" {
		t.Fatalf("install failed (code=%d):\n%s", code, out)
	}
	if _, err := os.Stat(stage); !os.IsNotExist(err) {
		t.Fatalf("staging directory was not removed: %s", stage)
	}
	if _, err := os.Stat(filepath.Join(home, ".local", "lib", "corral", "README.txt")); err != nil {
		t.Fatalf("README.txt missing: %v", err)
	}
	stateDir := filepath.Join(home, ".local", "state", "corral", "corrald", "111122223333")
	if _, err := os.Stat(filepath.Join(stateDir, "install.json")); err != nil {
		t.Fatalf("install.json missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(stateDir, "ops", "op0001.json")); err != nil {
		t.Fatalf("ops/op0001.json missing: %v", err)
	}

	// 3. serve (detached).
	out, code = runCLI(t, installedBin, home, env, "serve", "--gateway", "gw.test")
	kv = parseKV(out)
	if code != 0 || kv["result"] != "ok" || kv["pid"] == "" || kv["socket"] == "" {
		t.Fatalf("serve failed (code=%d):\n%s", code, out)
	}
	pid1 := kv["pid"]
	sock := kv["socket"]

	// 4. Second serve returns result=running with exit 0.
	out, code = runCLI(t, installedBin, home, env, "serve", "--gateway", "gw.test")
	kv = parseKV(out)
	if code != 0 || kv["result"] != "running" || kv["pid"] != pid1 {
		t.Fatalf("second serve unexpected (code=%d):\n%s", code, out)
	}

	// 5. status reports running.
	out, code = runCLI(t, installedBin, home, env, "status")
	kv = parseKV(out)
	if code != 0 || kv["result"] != "ok" || kv["state"] != "running" || kv["pid"] != pid1 || kv["version"] != "0.1.0" {
		t.Fatalf("status unexpected (code=%d):\n%s", code, out)
	}

	// 6. RPC hello (including proto_mismatch), clients persistence, forget.
	c1 := dialRPC(t, sock)
	resp, _ := c1.call(t, proto.MethodDaemonHello, proto.HelloParams{
		Gateway: "gw.test", GatewayVersion: "1.0.0", ProtoMin: 2, ProtoMax: 3,
		Client: proto.ClientIdentity{CorralUser: "alice", HostID: "h1"},
	})
	if resp.Error == nil || resp.Error.Code != proto.ErrProtoMismatch {
		t.Fatalf("expected proto_mismatch, got %+v", resp)
	}

	resp, _ = c1.call(t, proto.MethodDaemonHello, proto.HelloParams{
		Gateway: "gw.test", GatewayVersion: "1.0.0", ProtoMin: 1, ProtoMax: 1,
		Client: proto.ClientIdentity{CorralUser: "alice", HostID: "h1"},
	})
	if resp.Error != nil {
		t.Fatalf("hello failed: %v", resp.Error)
	}
	var hello proto.HelloResult
	if err := json.Unmarshal(resp.Result, &hello); err != nil {
		t.Fatal(err)
	}
	if hello.Version != "0.1.0" || hello.Release != 100 || hello.Epoch == "" || len(hello.Clients) != 1 {
		t.Fatalf("unexpected hello result: %+v", hello)
	}

	// Verify clients.json persisted on disk.
	if _, err := os.Stat(filepath.Join(stateDir, "clients.json")); err != nil {
		t.Fatalf("clients.json not persisted: %v", err)
	}

	// Emit events 1..5 via test-build RPC, disconnect, emit 6..8, then reconnect & resume from 5.
	for i := 1; i <= 5; i++ {
		r, _ := c1.call(t, "test.emit", map[string]any{"event": "test.tick", "data": map[string]int{"n": i}})
		if r.Error != nil {
			t.Fatalf("emit %d: %v", i, r.Error)
		}
	}
	c1.close()

	cEmit := dialRPC(t, sock)
	for i := 6; i <= 8; i++ {
		r, _ := cEmit.call(t, "test.emit", map[string]any{"event": "test.tick", "data": map[string]int{"n": i}})
		if r.Error != nil {
			t.Fatalf("emit %d: %v", i, r.Error)
		}
	}
	cEmit.close()

	c2 := dialRPC(t, sock)
	defer c2.close()
	resp, _ = c2.call(t, proto.MethodDaemonResume, proto.ResumeParams{Epoch: hello.Epoch, LastSeq: 5})
	if resp.Error != nil {
		t.Fatalf("resume(5) failed: %v", resp.Error)
	}
	var resRes proto.ResumeResult
	if err := json.Unmarshal(resp.Result, &resRes); err != nil {
		t.Fatal(err)
	}
	if resRes.Replayed != 3 || resRes.CurrentSeq != 8 {
		t.Fatalf("resume result: got %+v, want replayed=3 current_seq=8", resRes)
	}
	for wantSeq := uint64(6); wantSeq <= 8; wantSeq++ {
		ev := c2.readEvent(t, 2*time.Second)
		if ev.Seq != wantSeq || ev.Epoch != hello.Epoch {
			t.Fatalf("replayed event mismatch: got seq=%d epoch=%s, want seq=%d", ev.Seq, ev.Epoch, wantSeq)
		}
	}

	// Wrong epoch returns resync_required.
	c3 := dialRPC(t, sock)
	resp, _ = c3.call(t, proto.MethodDaemonResume, proto.ResumeParams{Epoch: "different-epoch", LastSeq: 5})
	if resp.Error == nil || resp.Error.Code != proto.ErrResyncRequired {
		t.Fatalf("wrong epoch resume: got %+v, want resync_required", resp)
	}
	// Forget client.
	resp, _ = c3.call(t, proto.MethodDaemonForget, proto.ForgetParams{Gateway: "gw.test", CorralUser: "alice"})
	if resp.Error != nil {
		t.Fatalf("forget failed: %v", resp.Error)
	}
	var fg proto.ForgetResult
	_ = json.Unmarshal(resp.Result, &fg)
	if fg.RemainingClients != 0 {
		t.Fatalf("expected 0 remaining clients after forget, got %d", fg.RemainingClients)
	}
	c3.close()
	c2.close()

	// 7. uninstall stops daemon, deletes files, and post-check reports removed.
	out, code = runCLI(t, installedBin, home, env, "uninstall", "--op", "op0002")
	kv = parseKV(out)
	if code != 0 || kv["result"] != "removed" || kv["last_machine"] != "true" {
		t.Fatalf("uninstall unexpected (code=%d):\n%s", code, out)
	}
	if _, err := os.Lstat(stateDir); !os.IsNotExist(err) {
		t.Fatalf("stateDir still exists after uninstall")
	}
	if _, err := os.Lstat(installedBin); !os.IsNotExist(err) {
		t.Fatalf("symlink still exists after uninstall")
	}
	if _, err := os.Lstat(sock); !os.IsNotExist(err) {
		t.Fatalf("socket still exists after uninstall")
	}
}

func TestIntegrationIdleExit(t *testing.T) {
	bins := getTestBinaries(t)
	home := t.TempDir()
	env := []string{
		"CORRALD_TEST_MACHINE_ID=222233334444",
		"CORRALD_TEST_IDLE_TIMEOUT=300ms",
	}

	stage := stageBinary(t, home, bins.v1)
	if out, code := runCLI(t, filepath.Join(stage, "corrald"), home, env,
		"install", "--stage", stage, "--op", "op1", "--gateway", "gw.idle", "--by", "bob"); code != 0 {
		t.Fatalf("install failed: %s", out)
	}
	installedBin := filepath.Join(home, ".local", "lib", "corral", "corrald")
	t.Cleanup(func() {
		if _, err := os.Lstat(installedBin); err == nil {
			_, _ = runCLI(t, installedBin, home, env, "uninstall")
		}
	})

	out, code := runCLI(t, installedBin, home, env, "serve", "--gateway", "gw.idle")
	kv := parseKV(out)
	if code != 0 || kv["result"] != "ok" {
		t.Fatalf("serve failed: %s", out)
	}
	pid, _ := strconv.Atoi(kv["pid"])
	sock := kv["socket"]
	metaPath := filepath.Join(home, ".local", "state", "corral", "corrald", "222233334444", "meta.json")
	meta, err := ReadMetaRecord(metaPath)
	if err != nil {
		t.Fatalf("read meta.json: %v", err)
	}

	// Connect once and disconnect; idle timer should then exit daemon after ~300ms.
	c := dialRPC(t, sock)
	resp, _ := c.call(t, proto.MethodDaemonHealth, nil)
	if resp.Error != nil {
		t.Fatalf("health failed: %v", resp.Error)
	}
	c.close()

	// Poll process liveness without connecting to the socket (connecting via
	// status would reset the idle timer).
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		if !ProcessMatches(pid, meta.StartTime) {
			break
		}
		time.Sleep(40 * time.Millisecond)
	}
	if ProcessMatches(pid, meta.StartTime) {
		t.Fatalf("daemon pid %d did not exit after 300ms idle timeout", pid)
	}
	if _, err := os.Lstat(sock); !os.IsNotExist(err) {
		t.Fatalf("socket remained after idle exit: %s", sock)
	}
	out, code = runCLI(t, installedBin, home, env, "status")
	if code != 0 || parseKV(out)["state"] != "stopped" {
		t.Fatalf("status after idle exit unexpected (code=%d):\n%s", code, out)
	}
}

func TestIntegrationTombstoneBlocksServeAndInstall(t *testing.T) {
	bins := getTestBinaries(t)
	home := t.TempDir()
	env := []string{"CORRALD_TEST_MACHINE_ID=333344445555"}

	stateDir := filepath.Join(home, ".local", "state", "corral", "corrald", "333344445555")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, "removing"), []byte("1\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// serve refused with error=removing.
	out, code := runCLI(t, bins.v1, home, env, "serve", "--gateway", "gw.test")
	kv := parseKV(out)
	if code == 0 || kv["error"] != proto.ErrRemoving {
		t.Fatalf("serve with tombstone: code=%d out=%s", code, out)
	}

	// install refused with error=removing.
	stage := stageBinary(t, home, bins.v1)
	out, code = runCLI(t, filepath.Join(stage, "corrald"), home, env,
		"install", "--stage", stage, "--op", "op1", "--gateway", "gw.test", "--by", "alice")
	kv = parseKV(out)
	if code == 0 || kv["error"] != proto.ErrRemoving {
		t.Fatalf("install with tombstone: code=%d out=%s", code, out)
	}
}

func TestIntegrationHotUpdateZeroGap(t *testing.T) {
	bins := getTestBinaries(t)
	home := t.TempDir()
	env := []string{"CORRALD_TEST_MACHINE_ID=444455556666"}

	// Install & start v1 (release=100).
	stage1 := stageBinary(t, home, bins.v1)
	if out, code := runCLI(t, filepath.Join(stage1, "corrald"), home, env,
		"install", "--stage", stage1, "--op", "op1", "--gateway", "gw.hot", "--by", "alice"); code != 0 {
		t.Fatalf("install v1: %s", out)
	}
	installedBin := filepath.Join(home, ".local", "lib", "corral", "corrald")
	t.Cleanup(func() {
		if _, err := os.Lstat(installedBin); err == nil {
			_, _ = runCLI(t, installedBin, home, env, "uninstall")
		}
	})

	out, code := runCLI(t, installedBin, home, env, "serve", "--gateway", "gw.hot")
	kv := parseKV(out)
	if code != 0 || kv["result"] != "ok" {
		t.Fatalf("serve v1: %s", out)
	}
	pid1, _ := strconv.Atoi(kv["pid"])
	sock := kv["socket"]

	// Connect observer client, hello, emit events 1..5.
	obs := dialRPC(t, sock)
	defer obs.close()
	resp, _ := obs.call(t, proto.MethodDaemonHello, proto.HelloParams{
		Gateway: "gw.hot", GatewayVersion: "1.0", ProtoMin: 1, ProtoMax: 1,
		Client: proto.ClientIdentity{CorralUser: "alice", HostID: "h1"},
	})
	if resp.Error != nil {
		t.Fatalf("hello v1: %v", resp.Error)
	}
	var h1 proto.HelloResult
	_ = json.Unmarshal(resp.Result, &h1)

	for i := 1; i <= 5; i++ {
		r, _ := obs.call(t, "test.emit", map[string]any{"event": "test.tick", "data": map[string]int{"n": i}})
		if r.Error != nil {
			t.Fatalf("emit %d: %v", i, r.Error)
		}
	}
	// Execute an idempotent write on v1 to verify OpTable survives handoff.
	r, _ := obs.call(t, "test.write", map[string]any{"op_id": "idem-handoff-1", "value": "v1-payload"})
	if r.Error != nil {
		t.Fatalf("test.write on v1: %v", r.Error)
	}
	var w1 struct {
		PID int `json:"pid"`
	}
	_ = json.Unmarshal(r.Result, &w1)
	if w1.PID != pid1 {
		t.Fatalf("v1 write pid=%d, want %d", w1.PID, pid1)
	}

	// Stage v2 with --upgrade; symlink must STILL point to v1 before handoff!
	stage2 := stageBinary(t, home, bins.v2)
	out, code = runCLI(t, filepath.Join(stage2, "corrald"), home, env,
		"install", "--upgrade", "--stage", stage2, "--op", "op2", "--gateway", "gw.hot", "--by", "alice")
	kv2 := parseKV(out)
	if code != 0 || kv2["result"] != "ok" {
		t.Fatalf("upgrade install v2: %s", out)
	}
	v2BinPath := kv2["binary"]
	linkBefore, err := os.Readlink(installedBin)
	if err != nil || !strings.Contains(linkBefore, "0.1.0-100") {
		t.Fatalf("symlink changed prematurely on --upgrade: %q (%v)", linkBefore, err)
	}

	v2SHA, err := FileSHA256(v2BinPath)
	if err != nil {
		t.Fatal(err)
	}

	// Continuous background client dialing fresh connections and calling
	// daemon.health throughout handoff — ZERO ECONNREFUSED allowed!
	stopHammer := make(chan struct{})
	var hammerErrs atomic.Int64
	var hammerOK atomic.Int64
	var lastHammerErr atomic.Value
	var hammerWG sync.WaitGroup
	hammerWG.Add(1)
	go func() {
		defer hammerWG.Done()
		for {
			select {
			case <-stopHammer:
				return
			default:
			}
			var health proto.HealthResult
			if err := callLocalRPC(sock, proto.MethodDaemonHealth, nil, &health, 3*time.Second); err != nil {
				hammerErrs.Add(1)
				lastHammerErr.Store(err.Error())
			} else {
				hammerOK.Add(1)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()

	// Let hammer run briefly before triggering handoff.
	time.Sleep(40 * time.Millisecond)

	// Trigger daemon.handoff on v1.
	handoffStart := time.Now()
	resp, _ = obs.call(t, proto.MethodDaemonHandoff, proto.HandoffParams{
		Path:    v2BinPath,
		SHA256:  v2SHA,
		Release: 200,
	})
	handoffElapsed := time.Since(handoffStart)
	if resp.Error != nil {
		close(stopHammer)
		hammerWG.Wait()
		t.Fatalf("daemon.handoff failed: %v", resp.Error)
	}
	var hres proto.HandoffResult
	if err := json.Unmarshal(resp.Result, &hres); err != nil {
		t.Fatal(err)
	}
	if hres.PID == 0 || hres.PID == pid1 || hres.Release != 200 {
		t.Fatalf("unexpected handoff result: %+v (old pid=%d)", hres, pid1)
	}
	t.Logf("hot-update handoff to READY: server_reported=%dms wall=%v (v1 pid=%d -> v2 pid=%d)",
		hres.DurationMS, handoffElapsed, pid1, hres.PID)

	// Existing connection must receive daemon.reconnect control event.
	reconnEv := obs.readEvent(t, 2*time.Second)
	if reconnEv.Event != proto.EventDaemonReconnect || reconnEv.Epoch != h1.Epoch || reconnEv.Seq != 5 {
		t.Fatalf("unexpected reconnect event: %+v", reconnEv)
	}
	obs.close()

	// Continue hammering v2 briefly, then stop hammer and check 0 errors.
	time.Sleep(60 * time.Millisecond)
	close(stopHammer)
	hammerWG.Wait()
	if hammerErrs.Load() > 0 {
		t.Fatalf("continuous requests experienced %d errors during handoff (ok=%d, last=%v)",
			hammerErrs.Load(), hammerOK.Load(), lastHammerErr.Load())
	}
	if hammerOK.Load() == 0 {
		t.Fatal("hammer made 0 successful requests")
	}

	// Verify symlink now points to v2.
	linkAfter, err := os.Readlink(installedBin)
	if err != nil || !strings.Contains(linkAfter, "0.2.0-200") {
		t.Fatalf("symlink not switched to v2 after handoff: %q (%v)", linkAfter, err)
	}

	// Reconnect to v2: verify v2 metadata, same epoch, and continuous event seq!
	c2 := dialRPC(t, sock)
	defer c2.close()
	resp, _ = c2.call(t, proto.MethodDaemonHello, proto.HelloParams{
		Gateway: "gw.hot", GatewayVersion: "1.0", ProtoMin: 1, ProtoMax: 1,
		Client: proto.ClientIdentity{CorralUser: "alice", HostID: "h1"},
	})
	if resp.Error != nil {
		t.Fatalf("hello v2: %v", resp.Error)
	}
	var h2 proto.HelloResult
	_ = json.Unmarshal(resp.Result, &h2)
	if h2.Version != "0.2.0" || h2.Release != 200 || h2.PID != hres.PID || h2.Epoch != h1.Epoch {
		t.Fatalf("unexpected v2 hello: %+v (want version=0.2.0 release=200 pid=%d epoch=%s)", h2, hres.PID, h1.Epoch)
	}

	// Replaying the pre-handoff op_id on v2 must return v1's cached result (same v1 PID).
	r2, _ := c2.call(t, "test.write", map[string]any{"op_id": "idem-handoff-1", "value": "should-not-run"})
	if r2.Error != nil {
		t.Fatalf("replay op_id on v2: %v", r2.Error)
	}
	var w2 struct {
		PID   int    `json:"pid"`
		Value string `json:"value"`
	}
	_ = json.Unmarshal(r2.Result, &w2)
	if w2.PID != pid1 || w2.Value != "v1-payload" {
		t.Fatalf("op_id was re-executed on v2 instead of returning cached v1 result: %+v", w2)
	}

	// Resume from seq=3 -> must replay 4, 5 from snapshot, then emit 6, 7 on v2 cleanly.
	resp, _ = c2.call(t, proto.MethodDaemonResume, proto.ResumeParams{Epoch: h1.Epoch, LastSeq: 3})
	if resp.Error != nil {
		t.Fatalf("resume on v2 failed: %v", resp.Error)
	}
	for wantSeq := uint64(4); wantSeq <= 5; wantSeq++ {
		ev := c2.readEvent(t, 2*time.Second)
		if ev.Seq != wantSeq {
			t.Fatalf("replayed seq=%d, want %d", ev.Seq, wantSeq)
		}
	}

	// Emit 6 and 7 on v2 via separate conn and verify c2 receives seq=6, seq=7 continuously.
	cEmit := dialRPC(t, sock)
	defer cEmit.close()
	for wantSeq := uint64(6); wantSeq <= 7; wantSeq++ {
		r, _ := cEmit.call(t, "test.emit", map[string]any{"event": "test.tick", "data": map[string]uint64{"n": wantSeq}})
		if r.Error != nil {
			t.Fatalf("emit %d on v2: %v", wantSeq, r.Error)
		}
		ev := c2.readEvent(t, 2*time.Second)
		if ev.Seq != wantSeq || ev.Epoch != h1.Epoch {
			t.Fatalf("post-handoff event seq=%d epoch=%s, want seq=%d epoch=%s", ev.Seq, ev.Epoch, wantSeq, h1.Epoch)
		}
	}

	// Verify old v1 process exited.
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		if !ProcessMatches(pid1, h1.StartTime) {
			break
		}
		time.Sleep(30 * time.Millisecond)
	}
	if ProcessMatches(pid1, h1.StartTime) {
		t.Fatalf("old v1 process pid=%d is still running after handoff", pid1)
	}
}

func TestIntegrationHotUpdateBadBinaryFallback(t *testing.T) {
	bins := getTestBinaries(t)
	home := t.TempDir()
	env := []string{"CORRALD_TEST_MACHINE_ID=555566667777"}

	stage1 := stageBinary(t, home, bins.v1)
	if out, code := runCLI(t, filepath.Join(stage1, "corrald"), home, env,
		"install", "--stage", stage1, "--op", "op1", "--gateway", "gw.bad", "--by", "alice"); code != 0 {
		t.Fatalf("install v1: %s", out)
	}
	installedBin := filepath.Join(home, ".local", "lib", "corral", "corrald")
	t.Cleanup(func() {
		if _, err := os.Lstat(installedBin); err == nil {
			_, _ = runCLI(t, installedBin, home, env, "uninstall")
		}
	})

	out, code := runCLI(t, installedBin, home, env, "serve", "--gateway", "gw.bad")
	kv := parseKV(out)
	if code != 0 || kv["result"] != "ok" {
		t.Fatalf("serve v1: %s", out)
	}
	pid1, _ := strconv.Atoi(kv["pid"])
	sock := kv["socket"]

	// Place bad binary inside LibDir with mode 0700.
	badDir := filepath.Join(home, ".local", "lib", "corral", "corrald-0.9.9-999")
	if err := os.MkdirAll(badDir, 0o700); err != nil {
		t.Fatal(err)
	}
	badPath := filepath.Join(badDir, "corrald")
	b, err := os.ReadFile(bins.bad)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(badPath, b, 0o700); err != nil {
		t.Fatal(err)
	}
	badSHA, err := FileSHA256(badPath)
	if err != nil {
		t.Fatal(err)
	}

	// Attempt handoff to bad binary -> must return handoff_failed.
	c := dialRPC(t, sock)
	defer c.close()
	resp, _ := c.call(t, proto.MethodDaemonHandoff, proto.HandoffParams{
		Path:    badPath,
		SHA256:  badSHA,
		Release: 999,
	})
	if resp.Error == nil || resp.Error.Code != proto.ErrHandoffFailed {
		t.Fatalf("expected handoff_failed for bad binary, got %+v", resp)
	}

	// failed.json must record release 999.
	failedPath := filepath.Join(home, ".local", "state", "corral", "corrald", "555566667777", "failed.json")
	frec, err := ReadFailedRecord(failedPath)
	if err != nil || len(frec.Releases) != 1 || frec.Releases[0].Release != 999 {
		t.Fatalf("failed.json unexpected: %+v (%v)", frec, err)
	}

	// Old v1 process must still accept new connections, be state=running, and serve mutating writes!
	c2 := dialRPC(t, sock)
	defer c2.close()
	resp, _ = c2.call(t, proto.MethodDaemonHealth, nil)
	if resp.Error != nil {
		t.Fatalf("v1 health after failed handoff: %v", resp.Error)
	}
	var health proto.HealthResult
	_ = json.Unmarshal(resp.Result, &health)
	if health.PID != pid1 || health.State != proto.StateRunning {
		t.Fatalf("v1 state after rollback: pid=%d (want %d) state=%q (want running)", health.PID, pid1, health.State)
	}
	resp, _ = c2.call(t, "test.write", map[string]any{"op_id": "after-rollback", "value": "ok"})
	if resp.Error != nil {
		t.Fatalf("mutating write after rollback failed (draining not cleared?): %v", resp.Error)
	}
}

func TestIntegrationNFSMultiMachineUninstallAndPreserveSiblings(t *testing.T) {
	bins := getTestBinaries(t)
	sharedHome := t.TempDir()
	envA := []string{"CORRALD_TEST_MACHINE_ID=aaaaaaaaaaaa"}
	envB := []string{"CORRALD_TEST_MACHINE_ID=bbbbbbbbbbbb"}

	// Pre-create sibling code-server directory under ~/.local/lib/corral/.
	siblingFile := filepath.Join(sharedHome, ".local", "lib", "corral", "code-server-4.90.0", "bin", "code-server")
	if err := os.MkdirAll(filepath.Dir(siblingFile), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(siblingFile, []byte("keep-me"), 0o755); err != nil {
		t.Fatal(err)
	}

	installedBin := filepath.Join(sharedHome, ".local", "lib", "corral", "corrald")
	t.Cleanup(func() {
		if _, err := os.Lstat(installedBin); err == nil {
			_, _ = runCLI(t, installedBin, sharedHome, envA, "uninstall")
			_, _ = runCLI(t, installedBin, sharedHome, envB, "uninstall")
		}
	})

	// Install & serve on Machine A.
	stageA := stageBinary(t, sharedHome, bins.v1)
	if out, code := runCLI(t, filepath.Join(stageA, "corrald"), sharedHome, envA,
		"install", "--stage", stageA, "--op", "opA", "--gateway", "gw.nfs", "--by", "alice"); code != 0 {
		t.Fatalf("install A: %s", out)
	}
	if out, code := runCLI(t, installedBin, sharedHome, envA, "serve", "--gateway", "gw.nfs"); code != 0 {
		t.Fatalf("serve A: %s", out)
	}

	// Install & serve on Machine B in the same shared HOME.
	stageB := stageBinary(t, sharedHome, bins.v1)
	if out, code := runCLI(t, filepath.Join(stageB, "corrald"), sharedHome, envB,
		"install", "--stage", stageB, "--op", "opB", "--gateway", "gw.nfs", "--by", "alice"); code != 0 {
		t.Fatalf("install B: %s", out)
	}
	outB, code := runCLI(t, installedBin, sharedHome, envB, "serve", "--gateway", "gw.nfs")
	if code != 0 {
		t.Fatalf("serve B: %s", outB)
	}
	sockB := parseKV(outB)["socket"]

	// Uninstall Machine A: must delete ONLY Machine A's state, preserving shared binaries & Machine B.
	out, code := runCLI(t, installedBin, sharedHome, envA, "uninstall")
	kv := parseKV(out)
	if code != 0 || kv["result"] != "removed" || kv["last_machine"] != "false" {
		t.Fatalf("uninstall A unexpected (code=%d):\n%s", code, out)
	}
	stateA := filepath.Join(sharedHome, ".local", "state", "corral", "corrald", "aaaaaaaaaaaa")
	if _, err := os.Lstat(stateA); !os.IsNotExist(err) {
		t.Fatal("Machine A state dir still exists")
	}
	if _, err := os.Lstat(installedBin); err != nil {
		t.Fatalf("shared symlink deleted while Machine B still installed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(sharedHome, ".local", "lib", "corral", "README.txt")); err != nil {
		t.Fatalf("shared README.txt deleted while Machine B still installed: %v", err)
	}
	// Machine B's daemon must still be healthy.
	var healthB proto.HealthResult
	if err := callLocalRPC(sockB, proto.MethodDaemonHealth, nil, &healthB, 2*time.Second); err != nil {
		t.Fatalf("Machine B daemon broken after Machine A uninstall: %v", err)
	}

	// Now uninstall Machine B (the last machine): must delete shared corrald files too,
	// while leaving code-server-4.90.0 untouched!
	out, code = runCLI(t, installedBin, sharedHome, envB, "uninstall")
	kv = parseKV(out)
	if code != 0 || kv["result"] != "removed" || kv["last_machine"] != "true" {
		t.Fatalf("uninstall B unexpected (code=%d):\n%s", code, out)
	}
	stateB := filepath.Join(sharedHome, ".local", "state", "corral", "corrald", "bbbbbbbbbbbb")
	if _, err := os.Lstat(stateB); !os.IsNotExist(err) {
		t.Fatal("Machine B state dir still exists")
	}
	if _, err := os.Lstat(installedBin); !os.IsNotExist(err) {
		t.Fatal("shared symlink still exists after last machine uninstall")
	}
	if _, err := os.Stat(filepath.Join(sharedHome, ".local", "lib", "corral", "README.txt")); !os.IsNotExist(err) {
		t.Fatal("shared README.txt still exists after last machine uninstall")
	}
	verDir := filepath.Join(sharedHome, ".local", "lib", "corral", "corrald-0.1.0-100")
	if _, err := os.Lstat(verDir); !os.IsNotExist(err) {
		t.Fatal("shared version dir still exists after last machine uninstall")
	}
	// code-server sibling MUST remain completely untouched!
	gotSibling, err := os.ReadFile(siblingFile)
	if err != nil || string(gotSibling) != "keep-me" {
		t.Fatalf("sibling code-server file disturbed: %q (%v)", gotSibling, err)
	}
}

func TestIntegrationUninstallPartialResult(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("chmod 0500 does not block root from unlinking")
	}
	bins := getTestBinaries(t)
	home := t.TempDir()
	env := []string{"CORRALD_TEST_MACHINE_ID=666677778888"}

	stage := stageBinary(t, home, bins.v1)
	if out, code := runCLI(t, filepath.Join(stage, "corrald"), home, env,
		"install", "--stage", stage, "--op", "op1", "--gateway", "gw.part", "--by", "alice"); code != 0 {
		t.Fatalf("install: %s", out)
	}
	installedBin := filepath.Join(home, ".local", "lib", "corral", "corrald")

	// Lock a subdirectory inside StateDir as 0500 so removing its child fails.
	lockedDir := filepath.Join(home, ".local", "state", "corral", "corrald", "666677778888", "locked-sub")
	if err := os.MkdirAll(lockedDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(lockedDir, "stuck"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(lockedDir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = os.Chmod(lockedDir, 0o700)
	})

	out, code := runCLI(t, installedBin, home, env, "uninstall")
	kv := parseKV(out)
	if code == 0 || kv["result"] != "partial" || !strings.Contains(out, "left=") {
		t.Fatalf("expected result=partial with left= entries, got code=%d:\n%s", code, out)
	}
}

func TestIntegrationProductionBinaryHasNoTestMethods(t *testing.T) {
	bins := getTestBinaries(t)
	home := t.TempDir()
	installedBin := filepath.Join(home, ".local", "lib", "corral", "corrald")
	t.Cleanup(func() {
		if _, err := os.Lstat(installedBin); err == nil {
			_, _ = runCLI(t, installedBin, home, nil, "uninstall")
		}
	})

	stage := stageBinary(t, home, bins.prod)
	if out, code := runCLI(t, filepath.Join(stage, "corrald"), home, nil,
		"install", "--stage", stage, "--op", "op1", "--gateway", "gw.prod", "--by", "alice"); code != 0 {
		t.Fatalf("install prod: %s", out)
	}
	out, code := runCLI(t, installedBin, home, nil, "serve", "--gateway", "gw.prod")
	kv := parseKV(out)
	if code != 0 || kv["result"] != "ok" {
		t.Fatalf("serve prod: %s", out)
	}
	sock := kv["socket"]

	c := dialRPC(t, sock)
	defer c.close()
	resp, _ := c.call(t, "test.emit", map[string]any{"event": "x"})
	if resp.Error == nil || resp.Error.Code != proto.ErrMethodNotFound {
		t.Fatalf("production binary must not expose test.emit, got %+v", resp)
	}

	// Measure idle RSS of the production binary.
	resp, _ = c.call(t, proto.MethodDaemonHealth, nil)
	if resp.Error != nil {
		t.Fatalf("prod health: %v", resp.Error)
	}
	var health proto.HealthResult
	_ = json.Unmarshal(resp.Result, &health)
	t.Logf("production corrald idle RSS: %d KiB (%.2f MiB)", health.RSSKB, float64(health.RSSKB)/1024.0)
}
