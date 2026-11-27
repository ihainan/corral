//go:build linux

package daemon

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
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

// stageBinary places srcBin into the spec-mandated per-host stage directory
// ~/.local/state/corral/corrald/<mid>/stage-<opID>/corrald.
func stageBinary(t *testing.T, home, mid, opID, srcBin string) string {
	t.Helper()
	if mid == "" {
		resolved, _, err := ResolveMachineID(nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		mid = resolved
	}
	stage := filepath.Join(home, ".local", "state", "corral", "corrald", mid, "stage-"+opID)
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

func assertStateDirCleanedKeepLocks(t *testing.T, stateDir string) {
	t.Helper()
	entries, err := os.ReadDir(stateDir)
	if os.IsNotExist(err) {
		return
	}
	if err != nil {
		t.Fatalf("readdir %s: %v", stateDir, err)
	}
	for _, e := range entries {
		name := e.Name()
		if name != "op.lock" && name != "corrald.lock" {
			t.Fatalf("unexpected leftover entry %q in %s after uninstall", name, stateDir)
		}
	}
}

func TestIntegrationLifecycleAndResume(t *testing.T) {
	bins := getTestBinaries(t)
	home := t.TempDir()
	const mid = "111122223333"
	env := []string{"CORRALD_TEST_MACHINE_ID=" + mid}

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
	if code != 0 || kv["result"] != "ok" || kv["mid"] != mid {
		t.Fatalf("selftest unexpected (code=%d):\n%s", code, out)
	}

	// 2. install.
	stage := stageBinary(t, home, mid, "op0001", bins.v1)
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
	stateDir := filepath.Join(home, ".local", "state", "corral", "corrald", mid)
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

	if _, err := os.Stat(filepath.Join(stateDir, "clients.json")); err != nil {
		t.Fatalf("clients.json not persisted: %v", err)
	}

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

	c3 := dialRPC(t, sock)
	resp, _ = c3.call(t, proto.MethodDaemonResume, proto.ResumeParams{Epoch: "different-epoch", LastSeq: 5})
	if resp.Error == nil || resp.Error.Code != proto.ErrResyncRequired {
		t.Fatalf("wrong epoch resume: got %+v, want resync_required", resp)
	}
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

	// 7. uninstall stops daemon, deletes files (leaving only lock files in StateDir), and post-check reports removed.
	out, code = runCLI(t, installedBin, home, env, "uninstall", "--op", "op0002")
	kv = parseKV(out)
	if code != 0 || kv["result"] != "removed" || kv["last_machine"] != "true" {
		t.Fatalf("uninstall unexpected (code=%d):\n%s", code, out)
	}
	assertStateDirCleanedKeepLocks(t, stateDir)
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
	const mid = "222233334444"
	env := []string{
		"CORRALD_TEST_MACHINE_ID=" + mid,
		"CORRALD_TEST_IDLE_TIMEOUT=300ms",
	}

	stage := stageBinary(t, home, mid, "op1", bins.v1)
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
	metaPath := filepath.Join(home, ".local", "state", "corral", "corrald", mid, "meta.json")
	meta, err := ReadMetaRecord(metaPath)
	if err != nil {
		t.Fatalf("read meta.json: %v", err)
	}

	c := dialRPC(t, sock)
	resp, _ := c.call(t, proto.MethodDaemonHealth, nil)
	if resp.Error != nil {
		t.Fatalf("health failed: %v", resp.Error)
	}
	c.close()

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
	const mid = "333344445555"
	env := []string{"CORRALD_TEST_MACHINE_ID=" + mid}

	stateDir := filepath.Join(home, ".local", "state", "corral", "corrald", mid)
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, "removing"), []byte("1\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	out, code := runCLI(t, bins.v1, home, env, "serve", "--gateway", "gw.test")
	kv := parseKV(out)
	if code == 0 || kv["error"] != proto.ErrRemoving {
		t.Fatalf("serve with tombstone: code=%d out=%s", code, out)
	}

	stage := stageBinary(t, home, mid, "op1", bins.v1)
	out, code = runCLI(t, filepath.Join(stage, "corrald"), home, env,
		"install", "--stage", stage, "--op", "op1", "--gateway", "gw.test", "--by", "alice")
	kv = parseKV(out)
	if code == 0 || kv["error"] != proto.ErrRemoving {
		t.Fatalf("install with tombstone: code=%d out=%s", code, out)
	}
}

func TestIntegrationInstallRejectsArbitraryStageAndSymlinkVersionDir(t *testing.T) {
	// Items 9 & 10:
	// - --stage pointing to an arbitrary working directory MUST be rejected and NOT deleted.
	// - Pre-existing symlink at version directory MUST be rejected without writing outside.
	bins := getTestBinaries(t)
	home := t.TempDir()
	const mid = "343434343434"
	env := []string{"CORRALD_TEST_MACHINE_ID=" + mid}

	// Item 9: decoy arbitrary directory containing corrald + precious file.
	arbitraryDir := t.TempDir()
	b, _ := os.ReadFile(bins.v1)
	_ = os.WriteFile(filepath.Join(arbitraryDir, "corrald"), b, 0o700)
	precious := filepath.Join(arbitraryDir, "keep.txt")
	_ = os.WriteFile(precious, []byte("precious"), 0o600)

	out, code := runCLI(t, bins.v1, home, env,
		"install", "--stage", arbitraryDir, "--op", "op9", "--gateway", "gw.test", "--by", "alice")
	if code == 0 || parseKV(out)["error"] != "invalid_stage" {
		t.Fatalf("arbitrary --stage should be rejected with invalid_stage: code=%d out=%s", code, out)
	}
	if got, err := os.ReadFile(precious); err != nil || string(got) != "precious" {
		t.Fatalf("arbitrary --stage directory was damaged: %q (%v)", got, err)
	}

	// Item 10: symlink at version dir ~/.local/lib/corral/corrald-0.1.0-100 -> outsideDir.
	outsideDir := t.TempDir()
	libDir := filepath.Join(home, ".local", "lib", "corral")
	_ = os.MkdirAll(libDir, 0o755)
	if err := os.Symlink(outsideDir, filepath.Join(libDir, "corrald-0.1.0-100")); err != nil {
		t.Fatal(err)
	}
	stage := stageBinary(t, home, mid, "op10", bins.v1)
	out, code = runCLI(t, filepath.Join(stage, "corrald"), home, env,
		"install", "--stage", stage, "--op", "op10", "--gateway", "gw.test", "--by", "alice")
	if code == 0 || parseKV(out)["error"] != "ver_dir_invalid" {
		t.Fatalf("symlinked version dir should be rejected: code=%d out=%s", code, out)
	}
	if _, err := os.Lstat(filepath.Join(outsideDir, "corrald")); !os.IsNotExist(err) {
		t.Fatal("install wrote binary through symlinked version dir into outsideDir")
	}
}

func TestIntegrationHotUpdateZeroGap(t *testing.T) {
	bins := getTestBinaries(t)
	home := t.TempDir()
	const mid = "444455556666"
	env := []string{"CORRALD_TEST_MACHINE_ID=" + mid}

	stage1 := stageBinary(t, home, mid, "op1", bins.v1)
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
	idemOpID := proto.NewOpID(time.Now())
	r, _ := obs.call(t, "test.write", map[string]any{"op_id": idemOpID, "value": "v1-payload"})
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
	stage2 := stageBinary(t, home, mid, "op2", bins.v2)
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

	// Continuous background client dialing fresh connections throughout handoff.
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

	time.Sleep(40 * time.Millisecond)

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

	reconnEv := obs.readEvent(t, 2*time.Second)
	if reconnEv.Event != proto.EventDaemonReconnect || reconnEv.Epoch != h1.Epoch || reconnEv.Seq != 5 {
		t.Fatalf("unexpected reconnect event: %+v", reconnEv)
	}
	obs.close()

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

	linkAfter, err := os.Readlink(installedBin)
	if err != nil || !strings.Contains(linkAfter, "0.2.0-200") {
		t.Fatalf("symlink not switched to v2 after handoff: %q (%v)", linkAfter, err)
	}

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
	// Rule 2: Hot updates ALWAYS rotate to a fresh epoch on v2 so clients resync.
	if h2.Version != "0.2.0" || h2.Release != 200 || h2.PID != hres.PID || h2.Epoch == "" || h2.Epoch == h1.Epoch {
		t.Fatalf("unexpected v2 hello: %+v (want version=0.2.0 release=200 pid=%d new epoch != %s)", h2, hres.PID, h1.Epoch)
	}

	// OpTable is still carried across handoff as defense-in-depth.
	r2, _ := c2.call(t, "test.write", map[string]any{"op_id": idemOpID, "value": "should-not-run"})
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

	// Rule 2: Resuming with v1's old epoch on v2 MUST require full resync.
	resp, _ = c2.call(t, proto.MethodDaemonResume, proto.ResumeParams{Epoch: h1.Epoch, LastSeq: 3})
	if resp.Error == nil || resp.Error.Code != proto.ErrResyncRequired {
		t.Fatalf("resume with old epoch on v2: got %+v, want resync_required", resp)
	}

	// Subscribing under v2's new epoch receives fresh seq=1..2 events.
	resp, _ = c2.call(t, proto.MethodDaemonResume, proto.ResumeParams{Epoch: h2.Epoch, LastSeq: 0})
	if resp.Error != nil {
		t.Fatalf("resume with new epoch on v2 failed: %v", resp.Error)
	}
	cEmit := dialRPC(t, sock)
	defer cEmit.close()
	for wantSeq := uint64(1); wantSeq <= 2; wantSeq++ {
		r, _ := cEmit.call(t, "test.emit", map[string]any{"event": "test.tick", "data": map[string]uint64{"n": wantSeq}})
		if r.Error != nil {
			t.Fatalf("emit %d on v2: %v", wantSeq, r.Error)
		}
		ev := c2.readEvent(t, 2*time.Second)
		if ev.Seq != wantSeq || ev.Epoch != h2.Epoch {
			t.Fatalf("post-handoff event seq=%d epoch=%s, want seq=%d epoch=%s", ev.Seq, ev.Epoch, wantSeq, h2.Epoch)
		}
	}

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

func TestIntegrationHandoffTimedOutWriteAndHardExit(t *testing.T) {
	// Items 1 & 5:
	// - An in-flight write exceeding DrainTimeout is handed off as state="unknown"
	//   so v2 returns op_unknown instead of re-executing.
	// - Even though the write handler ignores ctx and sleeps 10s, v1 exits hard
	//   within PostHandoffWriteCap (400ms) instead of hanging 10s!
	bins := getTestBinaries(t)
	home := t.TempDir()
	const mid = "454545454545"
	env := []string{
		"CORRALD_TEST_MACHINE_ID=" + mid,
		"CORRALD_TEST_DRAIN_TIMEOUT=150ms",
		"CORRALD_TEST_POST_HANDOFF_WRITE_CAP=400ms",
		"CORRALD_TEST_POST_HANDOFF_EXIT=200ms",
	}

	stage1 := stageBinary(t, home, mid, "op1", bins.v1)
	if out, code := runCLI(t, filepath.Join(stage1, "corrald"), home, env,
		"install", "--stage", stage1, "--op", "op1", "--gateway", "gw.hard", "--by", "alice"); code != 0 {
		t.Fatalf("install v1: %s", out)
	}
	installedBin := filepath.Join(home, ".local", "lib", "corral", "corrald")
	t.Cleanup(func() {
		if _, err := os.Lstat(installedBin); err == nil {
			_, _ = runCLI(t, installedBin, home, env, "uninstall")
		}
	})

	out, code := runCLI(t, installedBin, home, env, "serve", "--gateway", "gw.hard")
	kv := parseKV(out)
	if code != 0 || kv["result"] != "ok" {
		t.Fatalf("serve v1: %s", out)
	}
	pid1, _ := strconv.Atoi(kv["pid"])
	sock := kv["socket"]
	meta1, err := ReadMetaRecord(filepath.Join(home, ".local", "state", "corral", "corrald", mid, "meta.json"))
	if err != nil {
		t.Fatal(err)
	}

	stage2 := stageBinary(t, home, mid, "op2", bins.v2)
	out, code = runCLI(t, filepath.Join(stage2, "corrald"), home, env,
		"install", "--upgrade", "--stage", stage2, "--op", "op2", "--gateway", "gw.hard", "--by", "alice")
	if code != 0 {
		t.Fatalf("upgrade v2: %s", out)
	}
	v2Bin := parseKV(out)["binary"]
	v2SHA, _ := FileSHA256(v2Bin)

	// Start a slow 10-second mutating write on v1 that ignores ctx.
	slowOpID := proto.NewOpID(time.Now())
	cSlow := dialRPC(t, sock)
	defer cSlow.close()
	go func() {
		id := cSlow.nextID.Add(1)
		raw, _ := json.Marshal(map[string]any{"op_id": slowOpID, "value": "slow", "delay_ms": 10000})
		_ = proto.WriteJSON(cSlow.conn, proto.Request{ID: id, Method: "test.write", Params: raw})
	}()
	// Wait briefly so slowOpID enters OpTable.Execute on v1.
	time.Sleep(60 * time.Millisecond)

	// Trigger handoff; v1 drains for 150ms, then hands off slowOpID as unknown.
	cHandoff := dialRPC(t, sock)
	defer cHandoff.close()
	resp, _ := cHandoff.call(t, proto.MethodDaemonHandoff, proto.HandoffParams{
		Path: v2Bin, SHA256: v2SHA, Release: 200,
	})
	if resp.Error != nil {
		t.Fatalf("handoff with timed-out write failed: %v", resp.Error)
	}
	handoffDone := time.Now()

	// Item 1: Retry slowOpID on v2 -> MUST return op_unknown (never re-execute).
	c2 := dialRPC(t, sock)
	defer c2.close()
	r2, _ := c2.call(t, "test.write", map[string]any{"op_id": slowOpID, "value": "retry"})
	if r2.Error == nil || r2.Error.Code != proto.ErrOpUnknown {
		t.Fatalf("retry timed-out op_id on v2: got %+v, want op_unknown", r2)
	}

	// Item 5: Old v1 process MUST exit at the hard cap (~400ms), well before 10s!
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if !ProcessMatches(pid1, meta1.StartTime) {
			break
		}
		time.Sleep(30 * time.Millisecond)
	}
	if ProcessMatches(pid1, meta1.StartTime) {
		t.Fatalf("old v1 pid=%d survived past hard exit cap (%v since handoff)", pid1, time.Since(handoffDone))
	}
}

func TestIntegrationHotUpdateBadBinaryAndCutoverFailureRollback(t *testing.T) {
	// Tests bad binary fallback AND Items 4 & 17 (cutover failure must NOT send
	// READY, and rollback must restore meta.json + symlink to v1).
	bins := getTestBinaries(t)
	home := t.TempDir()
	const mid = "555566667777"
	env := []string{"CORRALD_TEST_MACHINE_ID=" + mid}

	stage1 := stageBinary(t, home, mid, "op1", bins.v1)
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

	c := dialRPC(t, sock)
	defer c.close()
	helloResp, _ := c.call(t, proto.MethodDaemonHello, proto.HelloParams{
		Gateway: "gw.bad", GatewayVersion: "1.0", ProtoMin: 1, ProtoMax: 1,
		Client: proto.ClientIdentity{CorralUser: "alice", HostID: "h1"},
	})
	if helloResp.Error != nil {
		t.Fatalf("hello before failed handoff: %v", helloResp.Error)
	}
	var hBefore proto.HelloResult
	_ = json.Unmarshal(helloResp.Result, &hBefore)

	// Open a second passive client connection to verify Rule 2 closes ALL existing clients on rollback.
	passive := dialRPC(t, sock)
	defer passive.close()

	resp, _ := c.call(t, proto.MethodDaemonHandoff, proto.HandoffParams{
		Path: badPath, SHA256: badSHA, Release: 999,
	})
	if resp.Error == nil || resp.Error.Code != proto.ErrHandoffFailed {
		t.Fatalf("expected handoff_failed for bad binary, got %+v", resp)
	}

	// Rule 2: Rollback must close existing client connections to force reconnect & resync.
	_ = passive.conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	var dummy proto.Envelope
	if err := proto.ReadJSON(passive.conn, &dummy); err == nil {
		t.Fatalf("expected passive client connection to be closed after rollback, got %+v", dummy)
	}

	failedPath := filepath.Join(home, ".local", "state", "corral", "corrald", mid, "failed.json")
	frec, err := ReadFailedRecord(failedPath)
	if err != nil || len(frec.Releases) != 1 || frec.Releases[0].Release != 999 {
		t.Fatalf("failed.json unexpected: %+v (%v)", frec, err)
	}

	// Verify meta.json still points to v1 PID and symlink still points to v1.
	metaAfter, err := ReadMetaRecord(filepath.Join(home, ".local", "state", "corral", "corrald", mid, "meta.json"))
	if err != nil || metaAfter.PID != pid1 || metaAfter.Release != 100 {
		t.Fatalf("meta.json corrupted after rollback: %+v (%v)", metaAfter, err)
	}
	linkAfter, _ := os.Readlink(installedBin)
	if !strings.Contains(linkAfter, "0.1.0-100") {
		t.Fatalf("symlink not pointing to v1 after rollback: %q", linkAfter)
	}

	c2 := dialRPC(t, sock)
	defer c2.close()
	// Rule 2: Reconnecting after rollback MUST observe a rotated epoch!
	resp, _ = c2.call(t, proto.MethodDaemonHello, proto.HelloParams{
		Gateway: "gw.bad", GatewayVersion: "1.0", ProtoMin: 1, ProtoMax: 1,
		Client: proto.ClientIdentity{CorralUser: "alice", HostID: "h1"},
	})
	if resp.Error != nil {
		t.Fatalf("hello after rollback: %v", resp.Error)
	}
	var hAfter proto.HelloResult
	_ = json.Unmarshal(resp.Result, &hAfter)
	if hAfter.Epoch == "" || hAfter.Epoch == hBefore.Epoch {
		t.Fatalf("expected rollback to rotate epoch: before=%q after=%q", hBefore.Epoch, hAfter.Epoch)
	}
	resp, _ = c2.call(t, proto.MethodDaemonHealth, nil)
	if resp.Error != nil {
		t.Fatalf("v1 health after failed handoff: %v", resp.Error)
	}
	var health proto.HealthResult
	_ = json.Unmarshal(resp.Result, &health)
	if health.PID != pid1 || health.State != proto.StateRunning {
		t.Fatalf("v1 state after rollback: pid=%d (want %d) state=%q (want running)", health.PID, pid1, health.State)
	}
	resp, _ = c2.call(t, "test.write", map[string]any{"op_id": proto.NewOpID(time.Now()), "value": "ok"})
	if resp.Error != nil {
		t.Fatalf("mutating write after rollback failed: %v", resp.Error)
	}
}

func TestIntegrationHandoffRejectsCorruptSnapshot(t *testing.T) {
	// Item 3 (receiver side): a replacement process receiving truncated/corrupt
	// snapshot JSON on --state-fd MUST fail closed with snapshot_invalid and
	// MUST NOT emit READY.
	bins := getTestBinaries(t)
	home := t.TempDir()
	const mid = "313131313131"
	stateDir := filepath.Join(home, ".local", "state", "corral", "corrald", mid)
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}

	stateR, stateW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	readyR, readyW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer stateR.Close()
	defer readyR.Close()

	// Write truncated JSON and close write end.
	_, _ = stateW.WriteString(`{"schema":2,"epoch":"abc","events":[`)
	_ = stateW.Close()

	cmd := exec.Command(bins.v2, "serve", "--gateway", "gw.corrupt", "--foreground", "--handoff", "--state-fd=3", "--ready-fd=4")
	cmd.Env = []string{"HOME=" + home, "PATH=/usr/bin:/bin", "CORRALD_TEST_MACHINE_ID=" + mid}
	cmd.ExtraFiles = []*os.File{stateR, readyW}
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	_ = readyW.Close()

	readyOut, _ := io.ReadAll(readyR)
	waitErr := cmd.Wait()
	if waitErr == nil {
		t.Fatalf("expected non-zero exit for corrupt snapshot, got 0 (ready=%q out=%q)", readyOut, out.String())
	}
	if strings.HasPrefix(string(readyOut), "READY") {
		t.Fatalf("corrupt snapshot unexpectedly emitted READY: %q", readyOut)
	}
	if parseKV(out.String())["error"] != "snapshot_invalid" {
		t.Fatalf("expected error=snapshot_invalid, got:\n%s", out.String())
	}
}

func TestIntegrationHandoffRejectsSchema1Snapshot(t *testing.T) {
	// Round-2 Item 12: Snapshot schema bumped to 2; valid JSON with schema=1
	// MUST be rejected with snapshot_invalid and MUST NOT emit READY.
	bins := getTestBinaries(t)
	home := t.TempDir()
	const mid = "121212121212"
	stateDir := filepath.Join(home, ".local", "state", "corral", "corrald", mid)
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}

	stateR, stateW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	readyR, readyW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer stateR.Close()
	defer readyR.Close()

	_, _ = stateW.WriteString(`{"schema":1,"epoch":"old","last_seq":1,"events":[],"clients":[]}`)
	_ = stateW.Close()

	cmd := exec.Command(bins.v2, "serve", "--gateway", "gw.s1", "--foreground", "--handoff", "--state-fd=3", "--ready-fd=4")
	cmd.Env = []string{"HOME=" + home, "PATH=/usr/bin:/bin", "CORRALD_TEST_MACHINE_ID=" + mid}
	cmd.ExtraFiles = []*os.File{stateR, readyW}
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	_ = readyW.Close()

	readyOut, _ := io.ReadAll(readyR)
	waitErr := cmd.Wait()
	if waitErr == nil {
		t.Fatalf("expected non-zero exit for schema=1 snapshot, got 0 (ready=%q out=%q)", readyOut, out.String())
	}
	if strings.HasPrefix(string(readyOut), "READY") {
		t.Fatalf("schema=1 snapshot unexpectedly emitted READY: %q", readyOut)
	}
	if parseKV(out.String())["error"] != "snapshot_invalid" {
		t.Fatalf("expected error=snapshot_invalid for schema=1, got:\n%s", out.String())
	}
}

func TestIntegrationHandoffCutoverFailureDoesNotReportReady(t *testing.T) {
	// Items 4 & 17 (end-to-end): when v2 fails persistent cutover (symlink /
	// install.json / meta.json), v2 must NOT report READY, v1 must return
	// handoff_failed, and v1 must remain serving with v1's meta.json & symlink intact.
	bins := getTestBinaries(t)
	home := t.TempDir()
	const mid = "171717171717"
	env := []string{
		"CORRALD_TEST_MACHINE_ID=" + mid,
		"CORRALD_TEST_FAIL_CUTOVER=1",
	}

	stage1 := stageBinary(t, home, mid, "op1", bins.v1)
	if out, code := runCLI(t, filepath.Join(stage1, "corrald"), home, env,
		"install", "--stage", stage1, "--op", "op1", "--gateway", "gw.cut", "--by", "alice"); code != 0 {
		t.Fatalf("install v1: %s", out)
	}
	installedBin := filepath.Join(home, ".local", "lib", "corral", "corrald")
	t.Cleanup(func() {
		if _, err := os.Lstat(installedBin); err == nil {
			_, _ = runCLI(t, installedBin, home, env, "uninstall")
		}
	})

	out, code := runCLI(t, installedBin, home, env, "serve", "--gateway", "gw.cut")
	kv := parseKV(out)
	if code != 0 || kv["result"] != "ok" {
		t.Fatalf("serve v1: %s", out)
	}
	pid1, _ := strconv.Atoi(kv["pid"])
	sock := kv["socket"]

	stage2 := stageBinary(t, home, mid, "op2", bins.v2)
	out, code = runCLI(t, filepath.Join(stage2, "corrald"), home, env,
		"install", "--upgrade", "--stage", stage2, "--op", "op2", "--gateway", "gw.cut", "--by", "alice")
	if code != 0 {
		t.Fatalf("upgrade v2: %s", out)
	}
	v2Bin := parseKV(out)["binary"]
	v2SHA, _ := FileSHA256(v2Bin)

	c := dialRPC(t, sock)
	defer c.close()
	resp, _ := c.call(t, proto.MethodDaemonHandoff, proto.HandoffParams{
		Path: v2Bin, SHA256: v2SHA, Release: 200,
	})
	if resp.Error == nil || resp.Error.Code != proto.ErrHandoffFailed {
		t.Fatalf("expected handoff_failed when cutover fails, got %+v", resp)
	}

	linkAfter, _ := os.Readlink(installedBin)
	if !strings.Contains(linkAfter, "0.1.0-100") {
		t.Fatalf("symlink drifted after cutover failure: %q", linkAfter)
	}
	metaAfter, err := ReadMetaRecord(filepath.Join(home, ".local", "state", "corral", "corrald", mid, "meta.json"))
	if err != nil || metaAfter.PID != pid1 || metaAfter.Release != 100 {
		t.Fatalf("meta.json drifted after cutover failure: %+v (%v)", metaAfter, err)
	}
	var health proto.HealthResult
	if err := callLocalRPC(sock, proto.MethodDaemonHealth, nil, &health, 2*time.Second); err != nil || health.PID != pid1 {
		t.Fatalf("v1 not healthy after cutover failure rollback: health=%+v err=%v", health, err)
	}
}

func TestIntegrationNFSMultiMachineUninstallAndPreserveSiblings(t *testing.T) {
	// Includes Item 8 verification: corrald-backups and code-server-* MUST remain untouched!
	bins := getTestBinaries(t)
	sharedHome := t.TempDir()
	const midA = "aaaaaaaaaaaa"
	const midB = "bbbbbbbbbbbb"
	envA := []string{"CORRALD_TEST_MACHINE_ID=" + midA}
	envB := []string{"CORRALD_TEST_MACHINE_ID=" + midB}

	siblingFile := filepath.Join(sharedHome, ".local", "lib", "corral", "code-server-4.90.0", "bin", "code-server")
	if err := os.MkdirAll(filepath.Dir(siblingFile), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(siblingFile, []byte("keep-me"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Item 8: unrelated corrald-backups directory MUST NOT be deleted by uninstall!
	backupFile := filepath.Join(sharedHome, ".local", "lib", "corral", "corrald-backups", "notes.txt")
	if err := os.MkdirAll(filepath.Dir(backupFile), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(backupFile, []byte("user-backup"), 0o600); err != nil {
		t.Fatal(err)
	}

	installedBin := filepath.Join(sharedHome, ".local", "lib", "corral", "corrald")
	t.Cleanup(func() {
		if _, err := os.Lstat(installedBin); err == nil {
			_, _ = runCLI(t, installedBin, sharedHome, envA, "uninstall")
			_, _ = runCLI(t, installedBin, sharedHome, envB, "uninstall")
		}
	})

	stageA := stageBinary(t, sharedHome, midA, "opA", bins.v1)
	if out, code := runCLI(t, filepath.Join(stageA, "corrald"), sharedHome, envA,
		"install", "--stage", stageA, "--op", "opA", "--gateway", "gw.nfs", "--by", "alice"); code != 0 {
		t.Fatalf("install A: %s", out)
	}
	if out, code := runCLI(t, installedBin, sharedHome, envA, "serve", "--gateway", "gw.nfs"); code != 0 {
		t.Fatalf("serve A: %s", out)
	}

	stageB := stageBinary(t, sharedHome, midB, "opB", bins.v1)
	if out, code := runCLI(t, filepath.Join(stageB, "corrald"), sharedHome, envB,
		"install", "--stage", stageB, "--op", "opB", "--gateway", "gw.nfs", "--by", "alice"); code != 0 {
		t.Fatalf("install B: %s", out)
	}
	outB, code := runCLI(t, installedBin, sharedHome, envB, "serve", "--gateway", "gw.nfs")
	if code != 0 {
		t.Fatalf("serve B: %s", outB)
	}
	sockB := parseKV(outB)["socket"]

	// Uninstall Machine A: only Machine A state removed; shared binaries + Machine B intact.
	out, code := runCLI(t, installedBin, sharedHome, envA, "uninstall")
	kv := parseKV(out)
	if code != 0 || kv["result"] != "removed" || kv["last_machine"] != "false" {
		t.Fatalf("uninstall A unexpected (code=%d):\n%s", code, out)
	}
	stateA := filepath.Join(sharedHome, ".local", "state", "corral", "corrald", midA)
	assertStateDirCleanedKeepLocks(t, stateA)
	if _, err := os.Lstat(installedBin); err != nil {
		t.Fatalf("shared symlink deleted while Machine B still installed: %v", err)
	}
	var healthB proto.HealthResult
	if err := callLocalRPC(sockB, proto.MethodDaemonHealth, nil, &healthB, 2*time.Second); err != nil {
		t.Fatalf("Machine B daemon broken after Machine A uninstall: %v", err)
	}

	// Uninstall Machine B (last machine): shared corrald files removed, code-server & corrald-backups intact!
	out, code = runCLI(t, installedBin, sharedHome, envB, "uninstall")
	kv = parseKV(out)
	if code != 0 || kv["result"] != "removed" || kv["last_machine"] != "true" {
		t.Fatalf("uninstall B unexpected (code=%d):\n%s", code, out)
	}
	if _, err := os.Lstat(installedBin); !os.IsNotExist(err) {
		t.Fatal("shared symlink still exists after last machine uninstall")
	}
	if _, err := os.Lstat(filepath.Join(sharedHome, ".local", "lib", "corral", "corrald-0.1.0-100")); !os.IsNotExist(err) {
		t.Fatal("shared version dir still exists after last machine uninstall")
	}
	if gotSibling, err := os.ReadFile(siblingFile); err != nil || string(gotSibling) != "keep-me" {
		t.Fatalf("sibling code-server file disturbed: %q (%v)", gotSibling, err)
	}
	if gotBackup, err := os.ReadFile(backupFile); err != nil || string(gotBackup) != "user-backup" {
		t.Fatalf("unrelated corrald-backups file disturbed: %q (%v)", gotBackup, err)
	}
}

func TestIntegrationUninstallAbortsWhenTombstoneWriteFails(t *testing.T) {
	// Item 12: If removing tombstone cannot be written, uninstall MUST abort
	// without stopping the running daemon or deleting files.
	if os.Geteuid() == 0 {
		t.Skip("chmod 0500 does not block root")
	}
	bins := getTestBinaries(t)
	home := t.TempDir()
	const mid = "777788889999"
	env := []string{"CORRALD_TEST_MACHINE_ID=" + mid}

	stage := stageBinary(t, home, mid, "op1", bins.v1)
	if out, code := runCLI(t, filepath.Join(stage, "corrald"), home, env,
		"install", "--stage", stage, "--op", "op1", "--gateway", "gw.tomb", "--by", "alice"); code != 0 {
		t.Fatalf("install: %s", out)
	}
	installedBin := filepath.Join(home, ".local", "lib", "corral", "corrald")
	stateDir := filepath.Join(home, ".local", "state", "corral", "corrald", mid)
	t.Cleanup(func() {
		_ = os.Chmod(stateDir, 0o700)
		if _, err := os.Lstat(installedBin); err == nil {
			_, _ = runCLI(t, installedBin, home, env, "uninstall")
		}
	})

	out, code := runCLI(t, installedBin, home, env, "serve", "--gateway", "gw.tomb")
	if code != 0 {
		t.Fatalf("serve: %s", out)
	}
	sock := parseKV(out)["socket"]

	// Make StateDir read-only so step 1 (removing install.json) fails cleanly
	// without stopping the running daemon!
	if err := os.Chmod(stateDir, 0o500); err != nil {
		t.Fatal(err)
	}

	out, code = runCLI(t, installedBin, home, env, "uninstall")
	if code == 0 || parseKV(out)["error"] != "remove_install_json_failed" {
		t.Fatalf("expected remove_install_json_failed when StateDir is read-only, got code=%d:\n%s", code, out)
	}

	// Running daemon must STILL be alive because uninstall aborted before stop!
	var health proto.HealthResult
	if err := callLocalRPC(sock, proto.MethodDaemonHealth, nil, &health, 2*time.Second); err != nil {
		t.Fatalf("daemon was stopped despite install.json remove failure: %v", err)
	}

	// Also verify tombstone_failed specifically (e.g. a non-empty directory at removing).
	_ = os.Chmod(stateDir, 0o700)
	tombDir := filepath.Join(stateDir, "removing")
	if err := os.MkdirAll(filepath.Join(tombDir, "blocker"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(tombDir) })
	out, code = runCLI(t, installedBin, home, env, "uninstall")
	if code == 0 || parseKV(out)["error"] != "tombstone_failed" {
		t.Fatalf("expected tombstone_failed when removing cannot be replaced, got code=%d:\n%s", code, out)
	}
	if err := callLocalRPC(sock, proto.MethodDaemonHealth, nil, &health, 2*time.Second); err != nil {
		t.Fatalf("daemon was stopped despite tombstone write failure: %v", err)
	}
}

func TestIntegrationUninstallPartialResult(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("chmod 0500 does not block root from unlinking")
	}
	bins := getTestBinaries(t)
	home := t.TempDir()
	const mid = "666677778888"
	env := []string{"CORRALD_TEST_MACHINE_ID=" + mid}

	stage := stageBinary(t, home, mid, "op1", bins.v1)
	if out, code := runCLI(t, filepath.Join(stage, "corrald"), home, env,
		"install", "--stage", stage, "--op", "op1", "--gateway", "gw.part", "--by", "alice"); code != 0 {
		t.Fatalf("install: %s", out)
	}
	installedBin := filepath.Join(home, ".local", "lib", "corral", "corrald")

	lockedDir := filepath.Join(home, ".local", "state", "corral", "corrald", mid, "locked-sub")
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

func TestIntegrationOpLockSerializesTombstoneAndServe(t *testing.T) {
	// Item 13: While op.lock is held, serve waits; once tombstone is written
	// before releasing op.lock, serve immediately rejects with error=removing!
	bins := getTestBinaries(t)
	home := t.TempDir()
	const mid = "898989898989"
	env := []string{"CORRALD_TEST_MACHINE_ID=" + mid}

	stage := stageBinary(t, home, mid, "op1", bins.v1)
	if out, code := runCLI(t, filepath.Join(stage, "corrald"), home, env,
		"install", "--stage", stage, "--op", "op1", "--gateway", "gw.lock", "--by", "alice"); code != 0 {
		t.Fatalf("install: %s", out)
	}
	installedBin := filepath.Join(home, ".local", "lib", "corral", "corrald")
	stateDir := filepath.Join(home, ".local", "state", "corral", "corrald", mid)
	opLock, err := AcquireLock(filepath.Join(stateDir, "op.lock"))
	if err != nil {
		t.Fatal(err)
	}

	type cliRes struct {
		out  string
		code int
	}
	done := make(chan cliRes, 1)
	go func() {
		out, code := runCLI(t, installedBin, home, env, "serve", "--gateway", "gw.lock")
		done <- cliRes{out: out, code: code}
	}()

	// Ensure serve is blocked waiting on op.lock (must not complete while held).
	select {
	case res := <-done:
		_ = opLock.Release()
		t.Fatalf("serve did not wait for op.lock: code=%d out=%s", res.code, res.out)
	case <-time.After(150 * time.Millisecond):
	}

	// Write removing tombstone while holding op.lock, then release op.lock.
	if err := WriteFileAtomic(filepath.Join(stateDir, "removing"), []byte("1\n"), 0o600); err != nil {
		_ = opLock.Release()
		t.Fatal(err)
	}
	_ = opLock.Release()

	res := <-done
	if res.code == 0 || parseKV(res.out)["error"] != proto.ErrRemoving {
		t.Fatalf("serve after op.lock tombstone: code=%d out=%s, want error=removing", res.code, res.out)
	}
}

func TestIntegrationSharedLockPreventsNFSRace(t *testing.T) {
	// Item 14: While .corrald-shared.lock is held, Machine A's uninstall waits
	// before checking isLastMachine; if Machine B writes its install.json before
	// releasing the shared lock, Machine A sees Machine B and preserves shared binaries!
	bins := getTestBinaries(t)
	sharedHome := t.TempDir()
	const midA = "abababababab"
	const midB = "cdcdcdcdcdcd"
	envA := []string{"CORRALD_TEST_MACHINE_ID=" + midA}

	stageA := stageBinary(t, sharedHome, midA, "opA", bins.v1)
	if out, code := runCLI(t, filepath.Join(stageA, "corrald"), sharedHome, envA,
		"install", "--stage", stageA, "--op", "opA", "--gateway", "gw.nfs2", "--by", "alice"); code != 0 {
		t.Fatalf("install A: %s", out)
	}
	installedBin := filepath.Join(sharedHome, ".local", "lib", "corral", "corrald")
	sharedLockPath := filepath.Join(sharedHome, ".local", "lib", "corral", ".corrald-shared.lock")
	sharedLock, err := AcquireLock(sharedLockPath)
	if err != nil {
		t.Fatal(err)
	}

	type cliRes struct {
		out  string
		code int
	}
	done := make(chan cliRes, 1)
	go func() {
		out, code := runCLI(t, installedBin, sharedHome, envA, "uninstall")
		done <- cliRes{out: out, code: code}
	}()

	// Round-2 Item 5: While uninstall waits on SharedLock, Machine A MUST NOT
	// have deleted its StateDir or tombstone yet!
	stateA := filepath.Join(sharedHome, ".local", "state", "corral", "corrald", midA)
	time.Sleep(120 * time.Millisecond)
	if _, err := os.Lstat(stateA); err != nil {
		_ = sharedLock.Release()
		t.Fatalf("Machine A deleted StateDir before acquiring SharedLock: %v", err)
	}

	// Simulate Machine B finishing install under SharedLock right before releasing it!
	stateB := filepath.Join(sharedHome, ".local", "state", "corral", "corrald", midB)
	if err := os.MkdirAll(stateB, 0o700); err != nil {
		_ = sharedLock.Release()
		t.Fatal(err)
	}
	if err := WriteInstallRecord(filepath.Join(stateB, "install.json"), &InstallRecord{
		Version: "0.1.0", Release: 100, MachineID: midB,
	}); err != nil {
		_ = sharedLock.Release()
		t.Fatal(err)
	}
	_ = sharedLock.Release()

	res := <-done
	kv := parseKV(res.out)
	if res.code != 0 || kv["result"] != "removed" || kv["last_machine"] != "false" {
		t.Fatalf("expected Machine A to see Machine B under shared lock: code=%d out=%s", res.code, res.out)
	}
	if _, err := os.Lstat(installedBin); err != nil {
		t.Fatalf("shared symlink was deleted despite concurrent Machine B install: %v", err)
	}
	assertStateDirCleanedKeepLocks(t, stateA)
}

func TestIntegrationUninstallWithoutInstallJSONPreservesSharedAndReturnsPartial(t *testing.T) {
	// Round-2 Item 13 / Rule 6: When install.json is missing/corrupt, uninstall
	// MUST NOT delete shared program directories, symlink, or README.txt; it
	// cleans this machine's non-lock state and reports result=partial with left=.
	bins := getTestBinaries(t)
	home := t.TempDir()
	const mid = "131313131313"
	env := []string{"CORRALD_TEST_MACHINE_ID=" + mid}

	stage := stageBinary(t, home, mid, "op1", bins.v1)
	if out, code := runCLI(t, filepath.Join(stage, "corrald"), home, env,
		"install", "--stage", stage, "--op", "op1", "--gateway", "gw.noinst", "--by", "alice"); code != 0 {
		t.Fatalf("install: %s", out)
	}
	installedBin := filepath.Join(home, ".local", "lib", "corral", "corrald")
	readmePath := filepath.Join(home, ".local", "lib", "corral", "README.txt")
	verDir := filepath.Join(home, ".local", "lib", "corral", "corrald-0.1.0-100")
	stateDir := filepath.Join(home, ".local", "state", "corral", "corrald", mid)

	// Remove install.json to simulate missing manifest.
	if err := os.Remove(filepath.Join(stateDir, "install.json")); err != nil {
		t.Fatal(err)
	}

	out, code := runCLI(t, installedBin, home, env, "uninstall")
	kv := parseKV(out)
	if code == 0 || kv["result"] != "partial" {
		t.Fatalf("expected result=partial when install.json missing, got code=%d:\n%s", code, out)
	}
	assertStateDirCleanedKeepLocks(t, stateDir)
	// Shared symlink, version dir, and README.txt MUST be preserved and listed in left=!
	for _, preserved := range []string{installedBin, readmePath, verDir} {
		if _, err := os.Lstat(preserved); err != nil {
			t.Fatalf("shared path %s was deleted without install.json: %v", preserved, err)
		}
		if !strings.Contains(out, "left="+preserved+"\t") {
			t.Fatalf("preserved path %s not listed in left=:\n%s", preserved, out)
		}
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

	stage := stageBinary(t, home, "", "op1", bins.prod)
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

	resp, _ = c.call(t, proto.MethodDaemonHealth, nil)
	if resp.Error != nil {
		t.Fatalf("prod health: %v", resp.Error)
	}
	var health proto.HealthResult
	_ = json.Unmarshal(resp.Result, &health)
	t.Logf("production corrald idle RSS: %d KiB (%.2f MiB)", health.RSSKB, float64(health.RSSKB)/1024.0)
}

func TestIntegrationConcurrentHandoffAndUninstall(t *testing.T) {
	// Rule 1: Concurrent daemon.handoff and corrald uninstall are strictly
	// serialized by SharedLock -> OpLock. Regardless of which runs first, final
	// state must be completely consistent: uninstall finishes removed, no live
	// daemon remains, and no dangling symlink is left behind.
	bins := getTestBinaries(t)
	home := t.TempDir()
	const mid = "c0c0c0c0c0c0"
	env := []string{"CORRALD_TEST_MACHINE_ID=" + mid}

	stage1 := stageBinary(t, home, mid, "op1", bins.v1)
	if out, code := runCLI(t, filepath.Join(stage1, "corrald"), home, env,
		"install", "--stage", stage1, "--op", "op1", "--gateway", "gw.race", "--by", "alice"); code != 0 {
		t.Fatalf("install v1: %s", out)
	}
	installedBin := filepath.Join(home, ".local", "lib", "corral", "corrald")
	stateDir := filepath.Join(home, ".local", "state", "corral", "corrald", mid)
	t.Cleanup(func() {
		if _, err := os.Lstat(installedBin); err == nil {
			_, _ = runCLI(t, installedBin, home, env, "uninstall")
		}
	})

	out, code := runCLI(t, installedBin, home, env, "serve", "--gateway", "gw.race")
	if code != 0 {
		t.Fatalf("serve v1: %s", out)
	}
	sock := parseKV(out)["socket"]

	stage2 := stageBinary(t, home, mid, "op2", bins.v2)
	out, code = runCLI(t, filepath.Join(stage2, "corrald"), home, env,
		"install", "--upgrade", "--stage", stage2, "--op", "op2", "--gateway", "gw.race", "--by", "alice")
	if code != 0 {
		t.Fatalf("upgrade v2: %s", out)
	}
	v2Bin := parseKV(out)["binary"]
	v2SHA, _ := FileSHA256(v2Bin)

	// Launch handoff and uninstall concurrently!
	var wg sync.WaitGroup
	var uninstOut string
	var uninstCode int
	var handoffErr error

	start := make(chan struct{})
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		var hres proto.HandoffResult
		handoffErr = callLocalRPC(sock, proto.MethodDaemonHandoff, proto.HandoffParams{
			Path: v2Bin, SHA256: v2SHA, Release: 200,
		}, &hres, 10*time.Second)
	}()
	go func() {
		defer wg.Done()
		<-start
		uninstOut, uninstCode = runCLI(t, bins.v1, home, env, "uninstall")
	}()
	close(start)
	wg.Wait()

	t.Logf("concurrent handoffErr=%v uninstallCode=%d out=%q", handoffErr, uninstCode, uninstOut)
	kv := parseKV(uninstOut)
	if uninstCode != 0 || kv["result"] != "removed" {
		t.Fatalf("uninstall did not finish cleanly after concurrent handoff: code=%d out=%s", uninstCode, uninstOut)
	}
	// No symlink (especially no dangling symlink) may remain.
	if _, err := os.Lstat(installedBin); !os.IsNotExist(err) {
		t.Fatalf("symlink remained after concurrent handoff+uninstall: err=%v", err)
	}
	// No version dirs may remain.
	for _, d := range []string{"corrald-0.1.0-100", "corrald-0.2.0-200"} {
		if _, err := os.Lstat(filepath.Join(home, ".local", "lib", "corral", d)); !os.IsNotExist(err) {
			t.Fatalf("version dir %s remained after concurrent handoff+uninstall", d)
		}
	}
	// StateDir must contain only lock files.
	assertStateDirCleanedKeepLocks(t, stateDir)
	// No daemon may be listening on sock.
	var health proto.HealthResult
	if err := callLocalRPC(sock, proto.MethodDaemonHealth, nil, &health, 300*time.Millisecond); err == nil {
		t.Fatalf("daemon still responding after uninstall: %+v", health)
	}
}

func TestIntegrationLockTimeoutReturnsBusy(t *testing.T) {
	// Rule 1: Holding .corrald-shared.lock past LockTimeout causes install,
	// uninstall, and daemon.handoff to return error=busy.
	bins := getTestBinaries(t)
	home := t.TempDir()
	const mid = "b0b0b0b0b0b0"
	env := []string{
		"CORRALD_TEST_MACHINE_ID=" + mid,
		"CORRALD_TEST_LOCK_TIMEOUT=150ms",
	}

	stage1 := stageBinary(t, home, mid, "op1", bins.v1)
	if out, code := runCLI(t, filepath.Join(stage1, "corrald"), home, env,
		"install", "--stage", stage1, "--op", "op1", "--gateway", "gw.busy", "--by", "alice"); code != 0 {
		t.Fatalf("install v1: %s", out)
	}
	installedBin := filepath.Join(home, ".local", "lib", "corral", "corrald")
	t.Cleanup(func() {
		if _, err := os.Lstat(installedBin); err == nil {
			_, _ = runCLI(t, installedBin, home, []string{"CORRALD_TEST_MACHINE_ID=" + mid}, "uninstall")
		}
	})

	out, code := runCLI(t, installedBin, home, env, "serve", "--gateway", "gw.busy")
	if code != 0 {
		t.Fatalf("serve v1: %s", out)
	}
	sock := parseKV(out)["socket"]

	stage2 := stageBinary(t, home, mid, "op2", bins.v2)
	out, code = runCLI(t, filepath.Join(stage2, "corrald"), home, env,
		"install", "--upgrade", "--stage", stage2, "--op", "op2", "--gateway", "gw.busy", "--by", "alice")
	if code != 0 {
		t.Fatalf("upgrade v2: %s", out)
	}
	v2Bin := parseKV(out)["binary"]
	v2SHA, _ := FileSHA256(v2Bin)

	// Hold SharedLock to simulate another host/operation holding lifecycle lock.
	sharedLock, err := AcquireLock(filepath.Join(home, ".local", "lib", "corral", ".corrald-shared.lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer sharedLock.Release()

	// 1. install -> busy.
	stage3 := stageBinary(t, home, mid, "op3", bins.v2)
	out, code = runCLI(t, filepath.Join(stage3, "corrald"), home, env,
		"install", "--upgrade", "--stage", stage3, "--op", "op3", "--gateway", "gw.busy", "--by", "alice")
	if code == 0 || parseKV(out)["error"] != proto.ErrBusy {
		t.Fatalf("install while SharedLock held: code=%d out=%s, want error=busy", code, out)
	}

	// 2. uninstall -> busy.
	out, code = runCLI(t, installedBin, home, env, "uninstall")
	if code == 0 || parseKV(out)["error"] != proto.ErrBusy {
		t.Fatalf("uninstall while SharedLock held: code=%d out=%s, want error=busy", code, out)
	}

	// 3. daemon.handoff -> retryable RPC error busy.
	c := dialRPC(t, sock)
	defer c.close()
	resp, _ := c.call(t, proto.MethodDaemonHandoff, proto.HandoffParams{
		Path: v2Bin, SHA256: v2SHA, Release: 200,
	})
	if resp.Error == nil || resp.Error.Code != proto.ErrBusy || !resp.Error.Retryable {
		t.Fatalf("handoff while SharedLock held: got %+v, want retryable busy", resp)
	}
}

func TestIntegrationHardExitEvenWhenLogBlocksAfterReady(t *testing.T) {
	// Rule 3: Even if the logger blocks forever immediately after READY (on the
	// "handoff completed" log write), the time.AfterFunc armed BEFORE any I/O on
	// READY still exits v1 cleanly within PostHandoffExit!
	bins := getTestBinaries(t)
	home := t.TempDir()
	const mid = "e3e3e3e3e3e3"
	envV1 := []string{
		"CORRALD_TEST_MACHINE_ID=" + mid,
		"CORRALD_TEST_POST_HANDOFF_EXIT=300ms",
		"CORRALD_TEST_BLOCK_LOG_ON_HANDOFF=1",
	}
	envClean := []string{"CORRALD_TEST_MACHINE_ID=" + mid}

	stage1 := stageBinary(t, home, mid, "op1", bins.v1)
	if out, code := runCLI(t, filepath.Join(stage1, "corrald"), home, envClean,
		"install", "--stage", stage1, "--op", "op1", "--gateway", "gw.logblk", "--by", "alice"); code != 0 {
		t.Fatalf("install v1: %s", out)
	}
	installedBin := filepath.Join(home, ".local", "lib", "corral", "corrald")
	t.Cleanup(func() {
		if _, err := os.Lstat(installedBin); err == nil {
			_, _ = runCLI(t, installedBin, home, envClean, "uninstall")
		}
	})

	out, code := runCLI(t, installedBin, home, envV1, "serve", "--gateway", "gw.logblk")
	if code != 0 {
		t.Fatalf("serve v1: %s", out)
	}
	kv := parseKV(out)
	pid1, _ := strconv.Atoi(kv["pid"])
	sock := kv["socket"]
	meta1, err := ReadMetaRecord(filepath.Join(home, ".local", "state", "corral", "corrald", mid, "meta.json"))
	if err != nil {
		t.Fatal(err)
	}

	stage2 := stageBinary(t, home, mid, "op2", bins.v2)
	out, code = runCLI(t, filepath.Join(stage2, "corrald"), home, envClean,
		"install", "--upgrade", "--stage", stage2, "--op", "op2", "--gateway", "gw.logblk", "--by", "alice")
	if code != 0 {
		t.Fatalf("upgrade v2: %s", out)
	}
	v2Bin := parseKV(out)["binary"]
	v2SHA, _ := FileSHA256(v2Bin)

	c := dialRPC(t, sock)
	defer c.close()
	resp, _ := c.call(t, proto.MethodDaemonHandoff, proto.HandoffParams{
		Path: v2Bin, SHA256: v2SHA, Release: 200,
	})
	if resp.Error != nil {
		t.Fatalf("handoff failed: %v", resp.Error)
	}

	// v1's goroutine is now blocked forever inside s.log.Info("handoff completed..."),
	// yet the hard-exit AfterFunc armed on READY must still terminate v1 within ~300ms!
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if !ProcessMatches(pid1, meta1.StartTime) {
			break
		}
		time.Sleep(30 * time.Millisecond)
	}
	if ProcessMatches(pid1, meta1.StartTime) {
		t.Fatalf("v1 pid=%d hung because log write blocked after READY!", pid1)
	}

	// Verify v2 is healthy and serving!
	var health proto.HealthResult
	if err := callLocalRPC(sock, proto.MethodDaemonHealth, nil, &health, 2*time.Second); err != nil || health.Release != 200 {
		t.Fatalf("v2 unhealthy after v1 hard exit: health=%+v err=%v", health, err)
	}
}

func TestIntegrationServeRejectedAfterUninstallDeletesInstallJSONAndFreshStateCheck(t *testing.T) {
	// Rule 4:
	// 1. A serve process waiting on op.lock while uninstall deletes install.json
	//    (even after tombstone is removed) MUST be rejected with error=not_installed.
	// 2. An uninstall waiting on SharedLock when StateDir did NOT exist yet MUST
	//    re-read StateDir AFTER acquiring SharedLock, seeing a concurrent install
	//    that created StateDir + install.json while uninstall was waiting!
	bins := getTestBinaries(t)
	home := t.TempDir()
	const mid = "f4f4f4f4f4f4"
	env := []string{"CORRALD_TEST_MACHINE_ID=" + mid}

	stage := stageBinary(t, home, mid, "op1", bins.v1)
	if out, code := runCLI(t, filepath.Join(stage, "corrald"), home, env,
		"install", "--stage", stage, "--op", "op1", "--gateway", "gw.r4", "--by", "alice"); code != 0 {
		t.Fatalf("install: %s", out)
	}
	installedBin := filepath.Join(home, ".local", "lib", "corral", "corrald")
	stateDir := filepath.Join(home, ".local", "state", "corral", "corrald", mid)
	opLock, err := AcquireLock(filepath.Join(stateDir, "op.lock"))
	if err != nil {
		t.Fatal(err)
	}

	type cliRes struct {
		out  string
		code int
	}
	serveDone := make(chan cliRes, 1)
	go func() {
		out, code := runCLI(t, installedBin, home, env, "serve", "--gateway", "gw.r4")
		serveDone <- cliRes{out: out, code: code}
	}()

	select {
	case res := <-serveDone:
		_ = opLock.Release()
		t.Fatalf("serve did not wait on op.lock: code=%d out=%s", res.code, res.out)
	case <-time.After(120 * time.Millisecond):
	}

	// Delete install.json (no tombstone left!) then release op.lock.
	if err := os.Remove(filepath.Join(stateDir, "install.json")); err != nil {
		_ = opLock.Release()
		t.Fatal(err)
	}
	_ = opLock.Release()

	res := <-serveDone
	if res.code == 0 || parseKV(res.out)["error"] != "not_installed" {
		t.Fatalf("serve after install.json removed: code=%d out=%s, want error=not_installed", res.code, res.out)
	}

	// Part 2: Uninstall waits on SharedLock BEFORE StateDir exists; while it
	// waits, install creates StateDir + install.json -> uninstall MUST re-check
	// StateDir AFTER SharedLock and clean it up completely!
	home2 := t.TempDir()
	const mid2 = "f5f5f5f5f5f5"
	env2 := []string{"CORRALD_TEST_MACHINE_ID=" + mid2}
	libDir2 := filepath.Join(home2, ".local", "lib", "corral")
	if err := os.MkdirAll(libDir2, 0o755); err != nil {
		t.Fatal(err)
	}
	sharedLock2, err := AcquireLock(filepath.Join(libDir2, ".corrald-shared.lock"))
	if err != nil {
		t.Fatal(err)
	}

	uninstDone := make(chan cliRes, 1)
	go func() {
		out, code := runCLI(t, bins.v1, home2, env2, "uninstall")
		uninstDone <- cliRes{out: out, code: code}
	}()

	// Wait until uninstall is definitely blocked on SharedLock (StateDir does not exist yet!).
	time.Sleep(120 * time.Millisecond)
	stateDir2 := filepath.Join(home2, ".local", "state", "corral", "corrald", mid2)
	if err := os.MkdirAll(stateDir2, 0o700); err != nil {
		_ = sharedLock2.Release()
		t.Fatal(err)
	}
	verDir2 := filepath.Join(libDir2, "corrald-0.1.0-100")
	_ = os.MkdirAll(verDir2, 0o700)
	if err := WriteInstallRecord(filepath.Join(stateDir2, "install.json"), &InstallRecord{
		Version: "0.1.0", Release: 100, MachineID: mid2, Files: []string{verDir2},
	}); err != nil {
		_ = sharedLock2.Release()
		t.Fatal(err)
	}
	_ = sharedLock2.Release()

	ures := <-uninstDone
	if ures.code != 0 || parseKV(ures.out)["result"] != "removed" {
		t.Fatalf("uninstall after concurrent install during SharedLock wait: code=%d out=%s", ures.code, ures.out)
	}
	// Both the newly created StateDir entries AND verDir2 must be cleaned up!
	assertStateDirCleanedKeepLocks(t, stateDir2)
	if _, err := os.Lstat(verDir2); !os.IsNotExist(err) {
		t.Fatal("verDir2 not removed after re-checking StateDir under SharedLock")
	}
}

func TestIntegrationRollbackFailureReportsRollbackFailedAndDegradesHealth(t *testing.T) {
	// Rule 5: When cutover fails AND restoring backup encounters a write error
	// (read-only StateDir), daemon.handoff MUST return error=rollback_failed and
	// daemon.health MUST report caps["state"] = "rollback_failed: ...".
	if os.Geteuid() == 0 {
		t.Skip("chmod 0500 does not block root")
	}
	bins := getTestBinaries(t)
	home := t.TempDir()
	const mid = "e5e5e5e5e5e5"
	env := []string{"CORRALD_TEST_MACHINE_ID=" + mid}

	stage1 := stageBinary(t, home, mid, "op1", bins.v1)
	if out, code := runCLI(t, filepath.Join(stage1, "corrald"), home, env,
		"install", "--stage", stage1, "--op", "op1", "--gateway", "gw.rbf", "--by", "alice"); code != 0 {
		t.Fatalf("install v1: %s", out)
	}
	installedBin := filepath.Join(home, ".local", "lib", "corral", "corrald")
	libDir := filepath.Join(home, ".local", "lib", "corral")
	t.Cleanup(func() {
		_ = os.Chmod(libDir, 0o755)
		if _, err := os.Lstat(installedBin); err == nil {
			_, _ = runCLI(t, installedBin, home, env, "uninstall")
		}
	})

	out, code := runCLI(t, installedBin, home, env, "serve", "--gateway", "gw.rbf")
	if code != 0 {
		t.Fatalf("serve v1: %s", out)
	}
	sock := parseKV(out)["socket"]

	stage2 := stageBinary(t, home, mid, "op2", bins.v2)
	out, code = runCLI(t, filepath.Join(stage2, "corrald"), home, env,
		"install", "--upgrade", "--stage", stage2, "--op", "op2", "--gateway", "gw.rbf", "--by", "alice")
	if code != 0 {
		t.Fatalf("upgrade v2: %s", out)
	}
	v2Bin := parseKV(out)["binary"]
	v2SHA, _ := FileSHA256(v2Bin)

	// Make LibDir 0500: existing .corrald-shared.lock can still be opened and
	// readlink/README read succeed, v2 fails AtomicSymlink in LibDir, and v1's
	// unconditional restoreCutoverBackup fails writing symlink/README in LibDir!
	if err := os.Chmod(libDir, 0o500); err != nil {
		t.Fatal(err)
	}

	c := dialRPC(t, sock)
	defer c.close()
	resp, _ := c.call(t, proto.MethodDaemonHandoff, proto.HandoffParams{
		Path: v2Bin, SHA256: v2SHA, Release: 200,
	})
	if resp.Error == nil || resp.Error.Code != proto.ErrRollbackFailed {
		t.Fatalf("expected rollback_failed, got %+v", resp)
	}

	var health proto.HealthResult
	if err := callLocalRPC(sock, proto.MethodDaemonHealth, nil, &health, 2*time.Second); err != nil {
		t.Fatalf("health after rollback_failed: %v", err)
	}
	if !strings.HasPrefix(health.Caps["state"], "rollback_failed:") {
		t.Fatalf("expected daemon.health caps[state] rollback_failed, got %+v", health.Caps)
	}
}

func TestIntegrationUninstallMissingInstallJSONNonLastMachineReturnsPartial(t *testing.T) {
	// Rule 6: Even when another machine is still installed (last_machine=false),
	// missing/corrupt install.json on Machine A MUST still return result=partial
	// with left= listing preserved shared paths, without disturbing Machine B.
	bins := getTestBinaries(t)
	sharedHome := t.TempDir()
	const midA = "6a6a6a6a6a6a"
	const midB = "6b6b6b6b6b6b"
	envA := []string{"CORRALD_TEST_MACHINE_ID=" + midA}
	envB := []string{"CORRALD_TEST_MACHINE_ID=" + midB}
	installedBin := filepath.Join(sharedHome, ".local", "lib", "corral", "corrald")
	t.Cleanup(func() {
		if _, err := os.Lstat(installedBin); err == nil {
			_, _ = runCLI(t, installedBin, sharedHome, envA, "uninstall")
			_, _ = runCLI(t, installedBin, sharedHome, envB, "uninstall")
		}
	})

	stageA := stageBinary(t, sharedHome, midA, "opA", bins.v1)
	if out, code := runCLI(t, filepath.Join(stageA, "corrald"), sharedHome, envA,
		"install", "--stage", stageA, "--op", "opA", "--gateway", "gw.r6", "--by", "alice"); code != 0 {
		t.Fatalf("install A: %s", out)
	}
	stageB := stageBinary(t, sharedHome, midB, "opB", bins.v1)
	if out, code := runCLI(t, filepath.Join(stageB, "corrald"), sharedHome, envB,
		"install", "--stage", stageB, "--op", "opB", "--gateway", "gw.r6", "--by", "alice"); code != 0 {
		t.Fatalf("install B: %s", out)
	}
	outB, code := runCLI(t, installedBin, sharedHome, envB, "serve", "--gateway", "gw.r6")
	if code != 0 {
		t.Fatalf("serve B: %s", outB)
	}
	sockB := parseKV(outB)["socket"]

	// Corrupt/delete Machine A's install.json while Machine B is still installed!
	stateA := filepath.Join(sharedHome, ".local", "state", "corral", "corrald", midA)
	if err := os.Remove(filepath.Join(stateA, "install.json")); err != nil {
		t.Fatal(err)
	}

	out, code := runCLI(t, installedBin, sharedHome, envA, "uninstall")
	kv := parseKV(out)
	if code == 0 || kv["result"] != "partial" || kv["last_machine"] != "false" {
		t.Fatalf("expected result=partial last_machine=false, got code=%d:\n%s", code, out)
	}
	if !strings.Contains(out, "left="+installedBin+"\t") {
		t.Fatalf("expected left= listing preserved shared symlink:\n%s", out)
	}
	assertStateDirCleanedKeepLocks(t, stateA)

	// Machine B and shared binary must remain untouched and healthy.
	var healthB proto.HealthResult
	if err := callLocalRPC(sockB, proto.MethodDaemonHealth, nil, &healthB, 2*time.Second); err != nil {
		t.Fatalf("Machine B broken after Machine A partial uninstall: %v", err)
	}
}
