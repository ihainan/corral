package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/ihainan/corral/internal/config"
	"github.com/ihainan/corral/internal/corrald/daemon"
	"github.com/ihainan/corral/internal/corrald/gw"
	"github.com/ihainan/corral/internal/corrald/proto"
	"github.com/ihainan/corral/internal/store"
	"github.com/ihainan/corral/internal/testutil"
)

var (
	corraldBinOnce sync.Once
	corraldBinDir  string
	corraldBinErr  error
)

// buildTestCorraldDist compiles real cmd/corrald binaries once per test run for
// v1 (0.3.1, release 1000) and v2 (0.3.2, release 2000) with -tags corrald_test.
func buildTestCorraldDist(t *testing.T) (v1Dir, v2Dir string) {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("corrald E2E tests require Linux")
	}
	corraldBinOnce.Do(func() {
		base, err := os.MkdirTemp("", "corrald-e2e-bins-*")
		if err != nil {
			corraldBinErr = err
			return
		}
		corraldBinDir = base
		for _, spec := range []struct {
			sub, ver, rel, commit string
		}{
			{"v1", "0.3.1", "1000", "aaa111"},
			{"v2", "0.3.2", "2000", "bbb222"},
		} {
			dir := filepath.Join(base, spec.sub)
			nativeDir := filepath.Join(dir, "linux-"+runtime.GOARCH)
			otherArch := "arm64"
			if runtime.GOARCH == "arm64" {
				otherArch = "amd64"
			}
			otherDir := filepath.Join(dir, "linux-"+otherArch)
			if err := os.MkdirAll(nativeDir, 0o755); err != nil {
				corraldBinErr = err
				return
			}
			if err := os.MkdirAll(otherDir, 0o755); err != nil {
				corraldBinErr = err
				return
			}
			nativeBin := filepath.Join(nativeDir, "corrald")
			ldflags := fmt.Sprintf("-s -w -X main.version=%s -X main.release=%s -X main.commit=%s", spec.ver, spec.rel, spec.commit)
			cmd := exec.Command("go", "build", "-tags", "corrald_test", "-ldflags", ldflags, "-o", nativeBin, "../../cmd/corrald")
			cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
			if out, err := cmd.CombinedOutput(); err != nil {
				corraldBinErr = fmt.Errorf("build corrald %s: %v\n%s", spec.ver, err, out)
				return
			}
			// Populate the sibling arch path so LoadDist sees both architectures.
			b, err := os.ReadFile(nativeBin)
			if err != nil {
				corraldBinErr = err
				return
			}
			if err := os.WriteFile(filepath.Join(otherDir, "corrald"), b, 0o755); err != nil {
				corraldBinErr = err
				return
			}
		}
	})
	if corraldBinErr != nil {
		t.Fatalf("compile corrald test binaries: %v", corraldBinErr)
	}
	return filepath.Join(corraldBinDir, "v1"), filepath.Join(corraldBinDir, "v2")
}

// startCorraldSSH starts a test SSH server whose exec handler runs real shell
// commands inside a fresh test HOME directory (and cleans up any daemon on exit).
func startCorraldSSH(t *testing.T, extraEnv ...string) (*testutil.SSHServer, string) {
	t.Helper()
	home := t.TempDir()
	srv := testutil.StartSSHServer(t, "alice", "alice-pass", home)
	srv.HandleExec(testutil.ShellExec(home, func(cmd string) bool {
		return strings.Contains(cmd, gw.CommandTag) || strings.Contains(cmd, "corrald")
	}, extraEnv...))
	t.Cleanup(func() {
		bin := filepath.Join(home, ".local", "lib", "corral", "corrald")
		if _, err := os.Stat(bin); err == nil {
			c := exec.Command(bin, "stop")
			c.Env = append(os.Environ(), "HOME="+home)
			_ = c.Run()
		}
		// Clean up only this test's own /tmp/corrald-<uid>-<hash> fallback socket
		// directory (if one was created for home). Never touches other runs' dirs.
		var mid string
		for _, kv := range extraEnv {
			if v, ok := strings.CutPrefix(kv, "CORRALD_TEST_MACHINE_ID="); ok {
				mid = v
			}
		}
		uid := uint32(os.Getuid())
		if layout, err := daemon.ResolveLayout(daemon.PathOptions{Home: home, UID: &uid, MachineID: mid}); err == nil && layout.SocketUsesTmpDir {
			_ = os.RemoveAll(layout.SocketDir)
		}
	})
	return srv, home
}

// createAndPinHost creates a host pointing at srv and opens one session to pin its host key.
func createAndPinHost(t *testing.T, c *client, srv *testutil.SSHServer, name string) (int64, string) {
	t.Helper()
	r := c.do("POST", "/api/hosts", map[string]any{
		"name": name, "address": "127.0.0.1", "port": srv.Addr.Port,
		"username": "alice", "auth": "password", "password": "alice-pass",
	})
	mustStatus(t, r, http.StatusCreated, "create host "+name)
	hostID := int64(r.json()["id"].(float64))

	r = c.do("POST", "/api/sessions", map[string]any{"host_id": hostID})
	mustStatus(t, r, http.StatusCreated, "open session to pin host key")
	sessID := r.json()["id"].(string)
	return hostID, sessID
}

func startCorraldInstall(t *testing.T, c *client, hostID int64, extra ...map[string]any) resp {
	t.Helper()
	st := c.do("GET", fmt.Sprintf("/api/hosts/%d/corrald", hostID), nil)
	mustStatus(t, st, http.StatusOK, "get manifest_hash before install")
	mhash, _ := st.json()["manifest_hash"].(string)
	body := map[string]any{"manifest_hash": mhash}
	for _, m := range extra {
		for k, v := range m {
			body[k] = v
		}
	}
	r := c.do("POST", fmt.Sprintf("/api/hosts/%d/corrald/install", hostID), body)
	mustStatus(t, r, http.StatusAccepted, "start install")
	return r
}

func waitCorraldState(t *testing.T, c *client, hostID int64, wantStates ...string) map[string]any {
	t.Helper()
	want := make(map[string]bool, len(wantStates))
	for _, s := range wantStates {
		want[s] = true
	}
	deadline := time.Now().Add(20 * time.Second)
	var last map[string]any
	for time.Now().Before(deadline) {
		r := c.do("GET", fmt.Sprintf("/api/hosts/%d/corrald", hostID), nil)
		mustStatus(t, r, http.StatusOK, "poll corrald status")
		last = r.json()
		st, _ := last["state"].(string)
		op, _ := last["op"].(map[string]any)
		opState, _ := op["state"].(string)
		active := opState == "pending" || opState == "running"
		if want[st] && !active {
			return last
		}
		time.Sleep(50 * time.Millisecond)
	}
	b, _ := json.Marshal(last)
	t.Fatalf("timed out waiting for corrald state %v; last=%s", wantStates, b)
	return nil
}

func TestCorraldUnavailableWhenDistMissing(t *testing.T) {
	h := newHarness(t, func(c *config.Config) {
		c.CorraldDir = filepath.Join(t.TempDir(), "nonexistent")
	})
	admin := h.admin()
	srv, _ := startCorraldSSH(t)
	hostID, _ := createAndPinHost(t, admin, srv, "box")

	mustStatus(t, admin.do("GET", "/api/corrald/readme", nil), http.StatusNotFound, "readme unavailable")
	mustStatus(t, admin.do("GET", fmt.Sprintf("/api/hosts/%d/corrald", hostID), nil), http.StatusNotFound, "status unavailable")
	mustStatus(t, admin.do("POST", fmt.Sprintf("/api/hosts/%d/corrald/install", hostID), map[string]any{}), http.StatusNotFound, "install unavailable")
	// Other features unaffected.
	mustStatus(t, admin.do("GET", fmt.Sprintf("/api/hosts/%d", hostID), nil), http.StatusOK, "host still readable")
}

func TestCorraldInstallReadmeFallbackAndAudit(t *testing.T) {
	v1Dir, _ := buildTestCorraldDist(t)
	h := newHarness(t, func(c *config.Config) {
		c.CorraldDir = v1Dir
		c.GatewayID = "gw-test.local"
	})
	admin := h.admin()
	srv, home := startCorraldSSH(t)
	hostID, _ := createAndPinHost(t, admin, srv, "devbox")

	// Readme endpoint matches README.txt template and returns manifest_hash.
	rr := admin.do("GET", "/api/corrald/readme", nil)
	mustStatus(t, rr, http.StatusOK, "get readme")
	readmeJSON := rr.json()
	content, _ := readmeJSON["content"].(string)
	mhash, _ := readmeJSON["manifest_hash"].(string)
	if !strings.Contains(content, "Corral 主机辅助进程") || !strings.Contains(content, "Corral Host Helper Daemon") || mhash == "" {
		t.Fatalf("unexpected readme response: %s", rr.Body)
	}

	// Initial status is off.
	st0 := admin.do("GET", fmt.Sprintf("/api/hosts/%d/corrald", hostID), nil)
	mustStatus(t, st0, http.StatusOK, "initial status")
	if st0.json()["state"] != "off" {
		t.Fatalf("initial state = %v, want off", st0.json()["state"])
	}

	// Mismatched manifest_hash rejected with 409 manifest_changed.
	bad := admin.do("POST", fmt.Sprintf("/api/hosts/%d/corrald/install", hostID), map[string]any{"manifest_hash": "0000"})
	if bad.Status != http.StatusConflict || bad.code() != "manifest_changed" {
		t.Fatalf("wrong manifest hash: %d %s", bad.Status, bad.Body)
	}

	// Valid install starts and reaches running.
	ir := admin.do("POST", fmt.Sprintf("/api/hosts/%d/corrald/install", hostID), map[string]any{"manifest_hash": mhash})
	mustStatus(t, ir, http.StatusAccepted, "start install")
	if ir.json()["kind"] != "install" || ir.json()["id"] == "" {
		t.Fatalf("unexpected op response: %s", ir.Body)
	}

	st := waitCorraldState(t, admin, hostID, "running")
	if st["version"] != "0.3.1" || int64(st["release"].(float64)) != 1000 {
		t.Fatalf("unexpected running version/release: %v", st)
	}
	running, _ := st["running"].(map[string]any)
	if running == nil || running["pid"].(float64) <= 0 {
		t.Fatalf("expected live pid in running: %v", st)
	}
	if _, err := os.Stat(filepath.Join(home, ".local", "lib", "corral", "README.txt")); err != nil {
		t.Fatalf("README.txt not created on host: %v", err)
	}

	// Unified capability fallback entrypoint (spec §9).
	uid := int64(admin.do("GET", "/api/me", nil).json()["id"].(float64))
	if cl, ok := h.srv.CorraldClient(uid, hostID, "locate"); !ok || cl == nil {
		t.Fatal("expected CorraldClient(locate) to succeed while running")
	}
	h.srv.CorraldManager().SetHealthCapsForTest(hostID, map[string]string{"locate": "degraded", "watch": "ok"})
	if _, ok := h.srv.CorraldClient(uid, hostID, "locate"); ok {
		t.Fatal("expected degraded cap 'locate' to fall back (return false)")
	}
	if _, ok := h.srv.CorraldClient(uid, hostID, "watch"); !ok {
		t.Fatal("expected healthy cap 'watch' to stay enabled while 'locate' is degraded")
	}

	// Audit log contains corrald.install.
	entries, err := h.store.ListAudit(context.Background(), storeAuditQuery())
	if err != nil {
		t.Fatal(err)
	}
	foundInstall := false
	for _, e := range entries {
		if e.Action == "corrald.install" {
			foundInstall = true
			break
		}
	}
	if !foundInstall {
		t.Fatal("expected corrald.install audit log entry")
	}
}

func TestCorraldInstallFailures(t *testing.T) {
	v1Dir, _ := buildTestCorraldDist(t)
	cases := []struct {
		name       string
		probe      *gw.HostProbe
		verifyHook func(string) string
		wantState  string
		wantCode   string
	}{
		{
			name:      "unsupported_os_macos",
			probe:     &gw.HostProbe{OS: "Darwin", Machine: "arm64", Arch: "arm64", Home: "/Users/alice", HomeWritable: true, FreeKB: 100000},
			wantState: "off",
			wantCode:  "unsupported_os",
		},
		{
			name:      "unsupported_arch",
			probe:     &gw.HostProbe{OS: "Linux", Machine: "riscv64", Arch: "", Home: "/home/alice", HomeWritable: true, FreeKB: 100000},
			wantState: "off",
			wantCode:  "unsupported_arch",
		},
		{
			name:      "home_readonly",
			probe:     &gw.HostProbe{OS: "Linux", Machine: "x86_64", Arch: "amd64", Home: "/home/alice", StateDir: "/home/alice/.local/state/corral/corrald/0123456789ab", HomeWritable: false, FreeKB: 100000},
			wantState: "off",
			wantCode:  "home_readonly",
		},
		{
			name:      "no_space",
			probe:     &gw.HostProbe{OS: "Linux", Machine: "x86_64", Arch: "amd64", Home: "/home/alice", StateDir: "/home/alice/.local/state/corral/corrald/0123456789ab", HomeWritable: true, FreeKB: 1024},
			wantState: "off",
			wantCode:  "no_space",
		},
		{
			name:       "checksum_mismatch",
			verifyHook: func(string) string { return strings.Repeat("0", 64) },
			wantState:  "off",
			wantCode:   "checksum_mismatch",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, func(c *config.Config) {
				c.CorraldDir = v1Dir
				c.GatewayID = "gw-fail.local"
			})
			admin := h.admin()
			srv, home := startCorraldSSH(t)

			// Replace manager with test hooks injected BEFORE opening any session.
			dist, err := gw.LoadDist(v1Dir)
			if err != nil {
				t.Fatal(err)
			}
			var probeFn func(context.Context, *ssh.Client) (*gw.HostProbe, error)
			if tc.probe != nil {
				p := *tc.probe
				probeFn = func(context.Context, *ssh.Client) (*gw.HostProbe, error) { return &p, nil }
			}
			h.srv.corrald.Close()
			h.srv.corrald = gw.NewManager(gw.ManagerConfig{
				Store:            h.store,
				Dist:             dist,
				GatewayID:        "gw-fail.local",
				SSH:              &webSSHProvider{s: h.srv},
				ProbeOverride:    probeFn,
				VerifySHA256Hook: tc.verifyHook,
			})
			hostID, _ := createAndPinHost(t, admin, srv, "failbox")

			startCorraldInstall(t, admin, hostID)
			st := waitCorraldState(t, admin, hostID, tc.wantState)
			errObj, _ := st["error"].(map[string]any)
			if errObj == nil || errObj["code"] != tc.wantCode {
				t.Fatalf("error = %v, want code %q", errObj, tc.wantCode)
			}
			// Ensure no leftover stage directories remain on checksum failure.
			if tc.name == "checksum_mismatch" {
				matches, _ := filepath.Glob(filepath.Join(home, ".local", "state", "corral", "corrald", "*", "stage-*"))
				if len(matches) > 0 {
					t.Fatalf("expected stage directory to be deleted, found %v", matches)
				}
			}
		})
	}
}

func TestCorraldInstallCancel(t *testing.T) {
	v1Dir, _ := buildTestCorraldDist(t)
	h := newHarness(t, func(c *config.Config) {
		c.CorraldDir = v1Dir
		c.GatewayID = "gw-cancel.local"
	})
	admin := h.admin()
	srv, _ := startCorraldSSH(t)

	reached := make(chan struct{})
	release := make(chan struct{})
	dist, _ := gw.LoadDist(v1Dir)
	h.srv.corrald.Close()
	h.srv.corrald = gw.NewManager(gw.ManagerConfig{
		Store:     h.store,
		Dist:      dist,
		GatewayID: "gw-cancel.local",
		SSH:       &webSSHProvider{s: h.srv},
		BeforeInstallStart: func(int64, string) {
			close(reached)
			<-release
		},
	})
	hostID, _ := createAndPinHost(t, admin, srv, "cancelbox")

	startCorraldInstall(t, admin, hostID)
	select {
	case <-reached:
	case <-time.After(15 * time.Second):
		t.Fatal("timed out waiting for install to reach pre-start hook")
	}

	// Cancel before start begins.
	cancelDone := make(chan resp, 1)
	go func() {
		cancelDone <- admin.do("POST", fmt.Sprintf("/api/hosts/%d/corrald/cancel", hostID), nil)
	}()
	time.Sleep(50 * time.Millisecond)
	close(release)

	cr := <-cancelDone
	mustStatus(t, cr, http.StatusOK, "cancel install")
	st := waitCorraldState(t, admin, hostID, "off")
	op, _ := st["op"].(map[string]any)
	if op == nil || op["state"] != "cancelled" {
		t.Fatalf("expected cancelled op, got %v", op)
	}
	// Second cancel returns 409 cannot_cancel.
	r2 := admin.do("POST", fmt.Sprintf("/api/hosts/%d/corrald/cancel", hostID), nil)
	if r2.Status != http.StatusConflict || r2.code() != "cannot_cancel" {
		t.Fatalf("cancel when idle: %d %s", r2.Status, r2.Body)
	}
}

func TestCorraldAutoUpdateAndNoDowngrade(t *testing.T) {
	v1Dir, v2Dir := buildTestCorraldDist(t)
	h := newHarness(t, func(c *config.Config) {
		c.CorraldDir = v1Dir
		c.GatewayID = "gw-upd.local"
	})
	admin := h.admin()
	srv, _ := startCorraldSSH(t)
	hostID, _ := createAndPinHost(t, admin, srv, "updbox")

	// Install v1.
	startCorraldInstall(t, admin, hostID)
	st := waitCorraldState(t, admin, hostID, "running")
	if st["version"] != "0.3.1" {
		t.Fatalf("expected v1 0.3.1, got %v", st["version"])
	}

	// Gateway upgrades to v2 (same manifest) -> reconnect triggers automatic hot update!
	distV2, err := gw.LoadDist(v2Dir)
	if err != nil {
		t.Fatal(err)
	}
	h.srv.CorraldManager().SetDist(distV2)

	// Simulate reconnect (open new shell session triggers OnHostConnected).
	mustStatus(t, admin.do("POST", "/api/sessions", map[string]any{"host_id": hostID, "open": map[string]any{"kind": "shell"}}), http.StatusCreated, "reconnect for auto-update")
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		st = waitCorraldState(t, admin, hostID, "running")
		if st["version"] == "0.3.2" && int64(st["release"].(float64)) == 2000 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if st["version"] != "0.3.2" {
		t.Fatalf("auto-update did not upgrade to 0.3.2; got %v", st)
	}

	// Never downgrade: swap gateway back to v1 and reconnect -> must remain 0.3.2!
	distV1, _ := gw.LoadDist(v1Dir)
	h.srv.CorraldManager().SetDist(distV1)
	mustStatus(t, admin.do("POST", "/api/sessions", map[string]any{"host_id": hostID, "open": map[string]any{"kind": "shell"}}), http.StatusCreated, "reconnect on older gateway")
	time.Sleep(300 * time.Millisecond)
	stAfter := waitCorraldState(t, admin, hostID, "running")
	if stAfter["version"] != "0.3.2" {
		t.Fatalf("gateway unexpectedly downgraded daemon to %v", stAfter["version"])
	}
}

func TestCorraldScopeChangeBlocksAutoUpdateUntilConsent(t *testing.T) {
	v1Dir, v2Dir := buildTestCorraldDist(t)
	h := newHarness(t, func(c *config.Config) {
		c.CorraldDir = v1Dir
		c.GatewayID = "gw-scope.local"
	})
	admin := h.admin()
	srv, _ := startCorraldSSH(t)
	hostID, _ := createAndPinHost(t, admin, srv, "scopebox")

	startCorraldInstall(t, admin, hostID)
	waitCorraldState(t, admin, hostID, "running")

	// Prepare v2 Dist with expanded manifest scope.
	distV2, err := gw.LoadDist(v2Dir)
	if err != nil {
		t.Fatal(err)
	}
	distV2.Manifest.Reads = append(distV2.Manifest.Reads, "agent-logs:future-agent")
	distV2.ManifestHash, _ = proto.ManifestSHA256(distV2.Manifest)
	h.srv.CorraldManager().SetDist(distV2)

	// Reconnect must NOT auto-update; instead status exposes scope_change.
	mustStatus(t, admin.do("POST", "/api/sessions", map[string]any{"host_id": hostID, "open": map[string]any{"kind": "shell"}}), http.StatusCreated, "reconnect")
	time.Sleep(300 * time.Millisecond)

	st := waitCorraldState(t, admin, hostID, "running")
	if st["version"] != "0.3.1" {
		t.Fatalf("scope growth must block auto-update; version=%v", st["version"])
	}
	sc, _ := st["scope_change"].(map[string]any)
	if sc == nil {
		t.Fatalf("expected scope_change in status, got %v", st)
	}
	added, _ := sc["added"].([]any)
	if len(added) != 1 || added[0] != "reads:agent-logs:future-agent" {
		t.Fatalf("unexpected scope_change.added: %v", added)
	}

	// User consents and approves update.
	ur := admin.do("POST", fmt.Sprintf("/api/hosts/%d/corrald/update", hostID), map[string]any{"manifest_hash": distV2.ManifestHash})
	mustStatus(t, ur, http.StatusAccepted, "approve update")
	st = waitCorraldState(t, admin, hostID, "running")
	if st["version"] != "0.3.2" {
		t.Fatalf("expected 0.3.2 after consent update, got %v", st)
	}
	if st["scope_change"] != nil {
		t.Fatalf("expected scope_change cleared after update, got %v", st["scope_change"])
	}
}

func TestCorraldBadVersionHandoffFailureAndRollback(t *testing.T) {
	v1Dir, v2Dir := buildTestCorraldDist(t)
	h := newHarness(t, func(c *config.Config) {
		c.CorraldDir = v1Dir
		c.GatewayID = "gw-badver.local"
	})
	admin := h.admin()
	// CORRALD_TEST_FAIL_READY=1 makes v2 exit before READY during handoff!
	srv, home := startCorraldSSH(t, "CORRALD_TEST_FAIL_READY=1")
	hostID, _ := createAndPinHost(t, admin, srv, "badverbox")

	startCorraldInstall(t, admin, hostID)
	st := waitCorraldState(t, admin, hostID, "running")
	if st["version"] != "0.3.1" {
		t.Fatalf("expected 0.3.1, got %v", st["version"])
	}

	// Trigger update to v2: handoff fails before READY -> v1 keeps running & failed.json records release 2000!
	distV2, _ := gw.LoadDist(v2Dir)
	h.srv.CorraldManager().SetDist(distV2)
	ur := admin.do("POST", fmt.Sprintf("/api/hosts/%d/corrald/update", hostID), map[string]any{"manifest_hash": distV2.ManifestHash})
	mustStatus(t, ur, http.StatusAccepted, "trigger bad update")

	st = waitCorraldState(t, admin, hostID, "running")
	if st["version"] != "0.3.1" {
		t.Fatalf("expected rollback/stay on 0.3.1, got %v", st["version"])
	}
	errObj, _ := st["error"].(map[string]any)
	if errObj == nil || errObj["code"] != "update_failed" || errObj["target_version"] != "0.3.2" || errObj["fallback_version"] != "0.3.1" {
		t.Fatalf("expected update_failed fallback notice, got %v", errObj)
	}
	// Verify failed.json recorded release 2000 on host.
	matches, _ := filepath.Glob(filepath.Join(home, ".local", "state", "corral", "corrald", "*", "failed.json"))
	if len(matches) == 0 {
		t.Fatal("expected failed.json to be written on host")
	}
	fb, _ := os.ReadFile(matches[0])
	if !strings.Contains(string(fb), "2000") {
		t.Fatalf("expected release 2000 in failed.json, got %s", fb)
	}
}

func TestCorraldPauseResumeRemoveAndPending(t *testing.T) {
	v1Dir, _ := buildTestCorraldDist(t)
	h := newHarness(t, func(c *config.Config) {
		c.CorraldDir = v1Dir
		c.GatewayID = "gw-life.local"
	})
	admin := h.admin()
	srv, home := startCorraldSSH(t)
	hostID, _ := createAndPinHost(t, admin, srv, "lifebox")

	startCorraldInstall(t, admin, hostID)
	waitCorraldState(t, admin, hostID, "running")
	runDir := filepath.Join(home, ".local", "state", "corral", "run")
	if info, err := os.Lstat(runDir); err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("expected corrald startup to create %s: info=%v err=%v", runDir, info, err)
	}

	// Pause (only user -> stops daemon too).
	mustStatus(t, admin.do("POST", fmt.Sprintf("/api/hosts/%d/corrald/pause", hostID), nil), http.StatusOK, "pause")
	st := waitCorraldState(t, admin, hostID, "paused")
	if st["state"] != "paused" {
		t.Fatalf("expected paused, got %v", st["state"])
	}
	uid := int64(admin.do("GET", "/api/me", nil).json()["id"].(float64))
	if _, ok := h.srv.CorraldClient(uid, hostID, "locate"); ok {
		t.Fatal("CorraldClient must return false while paused")
	}

	// Resume restarts daemon without re-consent.
	mustStatus(t, admin.do("POST", fmt.Sprintf("/api/hosts/%d/corrald/resume", hostID), nil), http.StatusOK, "resume")
	st = waitCorraldState(t, admin, hostID, "running")
	if st["state"] != "running" {
		t.Fatalf("expected running after resume, got %v", st["state"])
	}

	// Remove cleans everything and logs audit.
	mustStatus(t, admin.do("POST", fmt.Sprintf("/api/hosts/%d/corrald/remove", hostID), nil), http.StatusAccepted, "remove")
	st = waitCorraldState(t, admin, hostID, "off")
	op, _ := st["op"].(map[string]any)
	if op == nil || op["result"] != "removed" {
		t.Fatalf("expected removed result, got %v", op)
	}
	if _, err := os.Stat(filepath.Join(home, ".local", "lib", "corral", "corrald")); !os.IsNotExist(err) {
		t.Fatal("corrald symlink should be removed after uninstall")
	}
	if _, err := os.Lstat(runDir); !os.IsNotExist(err) {
		t.Fatalf("corrald run dir %s should be removed after uninstall (err=%v)", runDir, err)
	}
	if _, err := os.Lstat(filepath.Join(home, ".local", "state", "corral")); !os.IsNotExist(err) {
		t.Fatalf("~/.local/state/corral should be removed when empty after uninstall (err=%v)", err)
	}

	// Re-install then shut down SSH server to test remove_pending!
	startCorraldInstall(t, admin, hostID)
	waitCorraldState(t, admin, hostID, "running")
	h.terms.CloseHost(uid, hostID, "drop")
	srv.Close()

	mustStatus(t, admin.do("POST", fmt.Sprintf("/api/hosts/%d/corrald/remove", hostID), nil), http.StatusAccepted, "remove while offline")
	st = waitCorraldState(t, admin, hostID, "remove_pending")
	if st["state"] != "remove_pending" {
		t.Fatalf("expected remove_pending, got %v", st)
	}
}

func TestCorraldRemovePartialAndStopFailed(t *testing.T) {
	v1Dir, _ := buildTestCorraldDist(t)
	h := newHarness(t, func(c *config.Config) {
		c.CorraldDir = v1Dir
		c.GatewayID = "gw-rmerr.local"
	})
	admin := h.admin()
	srv, _ := startCorraldSSH(t)
	hostID, _ := createAndPinHost(t, admin, srv, "rmerrbox")

	startCorraldInstall(t, admin, hostID)
	waitCorraldState(t, admin, hostID, "running")

	// Mock partial uninstall response from host.
	srv.HandleExec(func(cmd string) func(ssh.Channel) {
		if strings.Contains(cmd, "uninstall") {
			return func(ch ssh.Channel) {
				_, _ = ch.Write([]byte("result=partial\nleft=/home/alice/.local/lib/corral/corrald\tpermission denied\n"))
				ch.CloseWrite()
				_, _ = ch.SendRequest("exit-status", false, []byte{0, 0, 0, 1})
			}
		}
		return testutil.ShellExec(t.TempDir(), func(string) bool { return true })(cmd)
	})
	mustStatus(t, admin.do("POST", fmt.Sprintf("/api/hosts/%d/corrald/remove", hostID), nil), http.StatusAccepted, "remove partial")
	st := waitCorraldState(t, admin, hostID, "error")
	op, _ := st["op"].(map[string]any)
	if op == nil || op["result"] != "partial" {
		t.Fatalf("expected partial remove op, got %v", op)
	}

	// Mock stop_failed uninstall response from host.
	srv.HandleExec(func(cmd string) func(ssh.Channel) {
		if strings.Contains(cmd, "uninstall") {
			return func(ch ssh.Channel) {
				_, _ = ch.Write([]byte("error=stop_failed\npid=54321\ndetail=still alive\n"))
				ch.CloseWrite()
				_, _ = ch.SendRequest("exit-status", false, []byte{0, 0, 0, 1})
			}
		}
		return testutil.ShellExec(t.TempDir(), func(string) bool { return true })(cmd)
	})
	mustStatus(t, admin.do("POST", fmt.Sprintf("/api/hosts/%d/corrald/remove", hostID), nil), http.StatusAccepted, "remove stop_failed")
	st = waitCorraldState(t, admin, hostID, "error")
	op, _ = st["op"].(map[string]any)
	if op == nil || op["result"] != "stop_failed" || int(op["pid"].(float64)) != 54321 {
		t.Fatalf("expected stop_failed pid=54321, got %v", op)
	}
}

func TestCorraldReconcileAfterGatewayRestart(t *testing.T) {
	v1Dir, _ := buildTestCorraldDist(t)
	h := newHarness(t, func(c *config.Config) {
		c.CorraldDir = v1Dir
		c.GatewayID = "gw-restart.local"
	})
	admin := h.admin()
	srv, home := startCorraldSSH(t)
	hostID, _ := createAndPinHost(t, admin, srv, "restartbox")

	// Install cleanly so daemon is running on the host.
	startCorraldInstall(t, admin, hostID)
	waitCorraldState(t, admin, hostID, "running")

	// Create a leftover stage dir and simulate an interrupted installing op right before restart.
	stateMatches, _ := filepath.Glob(filepath.Join(home, ".local", "state", "corral", "corrald", "*"))
	if len(stateMatches) == 0 {
		t.Fatal("missing state dir")
	}
	staleStage := filepath.Join(stateMatches[0], "stage-deadbeef12345678")
	if err := os.MkdirAll(staleStage, 0o700); err != nil {
		t.Fatal(err)
	}
	ch, err := h.store.CorraldHostByID(context.Background(), hostID)
	if err != nil {
		t.Fatal(err)
	}
	ch.State = gw.StateInstalling
	_ = h.store.UpsertCorraldHost(context.Background(), ch)
	_ = h.store.CreateCorraldOp(context.Background(), &store.CorraldOp{
		ID: "deadbeef12345678", HostID: hostID, Kind: gw.OpInstall, State: gw.OpRunning, Step: "start",
	}, false)

	// Restart gateway on the same database.
	h.restart()

	// Reconnect to host -> gateway reads host truth, removes stale stage dir, finishes op as done, reconnects!
	mustStatus(t, admin.do("POST", "/api/sessions", map[string]any{"host_id": hostID}), http.StatusCreated, "reconnect after restart")
	st := waitCorraldState(t, admin, hostID, "running")
	if st["state"] != "running" {
		t.Fatalf("expected reconciled running state, got %v", st)
	}
	if _, err := os.Stat(staleStage); !os.IsNotExist(err) {
		t.Fatalf("expected leftover stage dir %s to be cleaned on reconcile", staleStage)
	}
}

func TestCorraldDeleteHostOptions(t *testing.T) {
	v1Dir, _ := buildTestCorraldDist(t)
	h := newHarness(t, func(c *config.Config) {
		c.CorraldDir = v1Dir
		c.GatewayID = "gw-delhost.local"
	})
	admin := h.admin()

	// Case 1: Reachable with remove_corrald=true removes corrald before deleting host.
	srv1, home1 := startCorraldSSH(t)
	id1, _ := createAndPinHost(t, admin, srv1, "del1")
	startCorraldInstall(t, admin, id1)
	waitCorraldState(t, admin, id1, "running")

	dr1 := admin.do("DELETE", fmt.Sprintf("/api/hosts/%d", id1), map[string]any{
		"remove_corrald": true, "stop_vscode": false, "remove_vscode": false,
	})
	mustStatus(t, dr1, http.StatusOK, "delete host 1")
	c1, _ := dr1.json()["corrald"].(map[string]any)
	if c1 == nil || c1["state"] != "removed" {
		t.Fatalf("expected corrald removed on delete, got %s", dr1.Body)
	}
	if _, err := os.Stat(filepath.Join(home1, ".local", "lib", "corral", "corrald")); !os.IsNotExist(err) {
		t.Fatal("expected corrald removed from host 1")
	}

	// Case 2: Reachable with remove_corrald=false leaves corrald installed.
	srv2, home2 := startCorraldSSH(t)
	id2, _ := createAndPinHost(t, admin, srv2, "del2")
	startCorraldInstall(t, admin, id2)
	waitCorraldState(t, admin, id2, "running")

	dr2 := admin.do("DELETE", fmt.Sprintf("/api/hosts/%d", id2), map[string]any{
		"remove_corrald": false, "stop_vscode": false, "remove_vscode": false,
	})
	mustStatus(t, dr2, http.StatusOK, "delete host 2")
	if _, err := os.Stat(filepath.Join(home2, ".local", "lib", "corral", "corrald")); err != nil {
		t.Fatal("expected corrald kept when remove_corrald=false")
	}

	// Case 3: Unreachable host still deletes record and returns unreachable + uninstall_cmd.
	srv3, _ := startCorraldSSH(t)
	id3, _ := createAndPinHost(t, admin, srv3, "del3")
	startCorraldInstall(t, admin, id3)
	waitCorraldState(t, admin, id3, "running")
	uid := int64(admin.do("GET", "/api/me", nil).json()["id"].(float64))
	h.terms.CloseHost(uid, id3, "drop")
	srv3.Close()

	dr3 := admin.do("DELETE", fmt.Sprintf("/api/hosts/%d", id3), map[string]any{
		"remove_corrald": true, "stop_vscode": false, "remove_vscode": false,
	})
	mustStatus(t, dr3, http.StatusOK, "delete unreachable host")
	j3 := dr3.json()
	c3, _ := j3["corrald"].(map[string]any)
	if j3["reachable"] != false || c3 == nil || c3["state"] != "unreachable" || c3["uninstall_cmd"] == "" {
		t.Fatalf("expected unreachable cleanup info, got %s", dr3.Body)
	}
	mustStatus(t, admin.do("GET", fmt.Sprintf("/api/hosts/%d", id3), nil), http.StatusNotFound, "host 3 deleted")
}

func TestCorraldIdentityRebindAndHostKeyRotation(t *testing.T) {
	v1Dir, _ := buildTestCorraldDist(t)
	h := newHarness(t, func(c *config.Config) {
		c.CorraldDir = v1Dir
		c.GatewayID = "gw-bind.local"
	})
	admin := h.admin()
	srv, _ := startCorraldSSH(t)
	hostID, _ := createAndPinHost(t, admin, srv, "bindbox")

	startCorraldInstall(t, admin, hostID)
	waitCorraldState(t, admin, hostID, "running")

	// 1. Rotate host key -> host_key_changed includes corrald_enabled=true.
	srv.RotateHostKey()
	r := admin.do("POST", "/api/sessions", map[string]any{"host_id": hostID, "open": map[string]any{"kind": "shell"}})
	if r.Status != http.StatusConflict || r.code() != "host_key_changed" {
		t.Fatalf("expected host_key_changed, got %d %s", r.Status, r.Body)
	}
	extra, _ := r.json()["extra"].(map[string]any)
	if extra["corrald_enabled"] != true {
		t.Fatalf("expected corrald_enabled=true in host_key_changed extra, got %v", extra)
	}
	newFP := extra["got"].(string)

	// Confirm new key with default keep_corrald=true -> authorization carries over!
	r = admin.do("POST", "/api/sessions", map[string]any{"host_id": hostID, "accept_fingerprint": newFP, "keep_corrald": true, "open": map[string]any{"kind": "shell"}})
	mustStatus(t, r, http.StatusCreated, "accept rotated key keeping corrald")
	st := waitCorraldState(t, admin, hostID, "running")
	if st["state"] != "running" {
		t.Fatalf("expected running after key rotation with keep_corrald=true, got %v", st["state"])
	}

	// 2. Rotate again and confirm with keep_corrald=false -> enters rebind!
	srv.RotateHostKey()
	r = admin.do("POST", "/api/sessions", map[string]any{"host_id": hostID, "open": map[string]any{"kind": "shell"}})
	newFP2 := r.json()["extra"].(map[string]any)["got"].(string)
	r = admin.do("POST", "/api/sessions", map[string]any{"host_id": hostID, "accept_fingerprint": newFP2, "keep_corrald": false, "open": map[string]any{"kind": "shell"}})
	mustStatus(t, r, http.StatusCreated, "accept rotated key dropping corrald")
	st = waitCorraldState(t, admin, hostID, "rebind")
	if st["state"] != "rebind" {
		t.Fatalf("expected rebind after keep_corrald=false, got %v", st["state"])
	}

	// Re-confirm & re-enable from rebind.
	startCorraldInstall(t, admin, hostID, map[string]any{"rebind": true})
	waitCorraldState(t, admin, hostID, "running")

	// 3. Changing host login username -> enters rebind!
	mustStatus(t, admin.do("PUT", fmt.Sprintf("/api/hosts/%d", hostID), map[string]any{
		"name": "bindbox", "address": "127.0.0.1", "port": srv.Addr.Port,
		"username": "bob", "auth": "password", "password": "alice-pass",
	}), http.StatusOK, "change username")
	st = waitCorraldState(t, admin, hostID, "rebind")
	if st["state"] != "rebind" {
		t.Fatalf("expected rebind after username change, got %v", st["state"])
	}
}

func TestCorraldMultiUserSharedDaemonAndAccountCleanup(t *testing.T) {
	v1Dir, _ := buildTestCorraldDist(t)
	h := newHarness(t, func(c *config.Config) {
		c.CorraldDir = v1Dir
		c.GatewayID = "gw-multi.local"
	})
	admin := h.admin()
	alice := h.member(admin, "alice")
	bob := h.member(admin, "bob")

	srv, home := startCorraldSSH(t)
	aliceHost, _ := createAndPinHost(t, alice, srv, "shared-a")
	bobHost, _ := createAndPinHost(t, bob, srv, "shared-b")

	// Alice installs corrald.
	startCorraldInstall(t, alice, aliceHost)
	stA := waitCorraldState(t, alice, aliceHost, "running")
	pidA := int(stA["running"].(map[string]any)["pid"].(float64))

	// Bob checks status and connects -> shares the EXACT SAME daemon PID and single shared connection!
	stB := waitCorraldState(t, bob, bobHost, "running")
	mustStatus(t, bob.do("POST", "/api/sessions", map[string]any{"host_id": bobHost, "open": map[string]any{"kind": "shell"}}), http.StatusCreated, "bob connect")
	time.Sleep(200 * time.Millisecond)
	stB = waitCorraldState(t, bob, bobHost, "running")
	pidB := int(stB["running"].(map[string]any)["pid"].(float64))
	if pidA != pidB {
		t.Fatalf("alice pid %d != bob pid %d; expected shared daemon", pidA, pidB)
	}
	if n := h.srv.CorraldManager().SharedConnCount(); n != 1 {
		t.Fatalf("SharedConnCount = %d, want 1", n)
	}
	usersA, _ := waitCorraldState(t, alice, aliceHost, "running")["users"].(map[string]any)
	if usersA == nil || int(usersA["count"].(float64)) < 2 {
		t.Fatalf("expected alice to see >= 2 users, got %v", usersA)
	}

	// Bob pauses -> Bob sees paused, Alice STILL sees running with same PID!
	mustStatus(t, bob.do("POST", fmt.Sprintf("/api/hosts/%d/corrald/pause", bobHost), nil), http.StatusOK, "bob pause")
	if waitCorraldState(t, bob, bobHost, "paused")["state"] != "paused" {
		t.Fatal("bob should be paused")
	}
	stA2 := waitCorraldState(t, alice, aliceHost, "running")
	if int(stA2["running"].(map[string]any)["pid"].(float64)) != pidA {
		t.Fatal("bob pausing must not stop alice's shared daemon")
	}

	// Account cleanup for Bob leaves daemon alive for Alice!
	bobUID := int64(bob.do("GET", "/api/me", nil).json()["id"].(float64))
	h.srv.CorraldManager().CleanupUserHosts(context.Background(), bobUID, "bob")
	if _, err := os.Stat(filepath.Join(home, ".local", "lib", "corral", "corrald")); err != nil {
		t.Fatal("cleaning up bob must keep shared daemon for alice")
	}
}

func TestCorraldIncompatibleAndRestartExhaustionRetry(t *testing.T) {
	v1Dir, _ := buildTestCorraldDist(t)
	h := newHarness(t, func(c *config.Config) {
		c.CorraldDir = v1Dir
		c.GatewayID = "gw-rec.local"
	})
	admin := h.admin()
	srv, home := startCorraldSSH(t)

	// Use short restart backoffs for fast deterministic test.
	dist, _ := gw.LoadDist(v1Dir)
	h.srv.corrald.Close()
	h.srv.corrald = gw.NewManager(gw.ManagerConfig{
		Store:           h.store,
		Dist:            dist,
		GatewayID:       "gw-rec.local",
		SSH:             &webSSHProvider{s: h.srv},
		HealthInterval:  5 * time.Second,
		RestartBackoffs: []time.Duration{20 * time.Millisecond, 20 * time.Millisecond, 20 * time.Millisecond},
	})
	hostID, _ := createAndPinHost(t, admin, srv, "recbox")

	startCorraldInstall(t, admin, hostID)
	waitCorraldState(t, admin, hostID, "running")

	// Break binary on host & stop daemon + drop conn -> 3 restarts fail -> enters error.
	bin := filepath.Join(home, ".local", "lib", "corral", "corrald")
	stopCmd := exec.Command(bin, "stop")
	stopCmd.Env = append(os.Environ(), "HOME="+home)
	_ = stopCmd.Run()
	realBin, _ := filepath.EvalSymlinks(bin)
	origBytes, _ := os.ReadFile(realBin)
	_ = os.WriteFile(realBin, []byte("#!/bin/sh\nexit 1\n"), 0o700)

	hk, _ := h.store.HostKey(context.Background(), int64(admin.do("GET", "/api/me", nil).json()["id"].(float64)), hostID)
	h.srv.CorraldManager().ForceDropConn(hk.Fingerprint, "alice")

	st := waitCorraldState(t, admin, hostID, "error")
	if st["state"] != "error" {
		t.Fatalf("expected error after 3 failed restarts, got %v", st)
	}

	// Restore binary and retry -> recovers to running!
	_ = os.WriteFile(realBin, origBytes, 0o700)
	mustStatus(t, admin.do("POST", fmt.Sprintf("/api/hosts/%d/corrald/retry", hostID), nil), http.StatusAccepted, "retry")
	st = waitCorraldState(t, admin, hostID, "running")
	if st["state"] != "running" {
		t.Fatalf("expected running after retry, got %v", st)
	}

	// Protocol mismatch -> incompatible!
	h.srv.CorraldManager().SetProtoRange(2, 2)
	h.srv.CorraldManager().DetachHost(hostID)
	mustStatus(t, admin.do("POST", "/api/sessions", map[string]any{"host_id": hostID, "open": map[string]any{"kind": "shell"}}), http.StatusCreated, "connect incompatible")
	st = waitCorraldState(t, admin, hostID, "incompatible")
	if st["state"] != "incompatible" || st["can_replace_with_gateway"] != true {
		t.Fatalf("expected incompatible with can_replace_with_gateway, got %v", st)
	}
}

func TestCorraldPostUpdateCrashLoopRollback(t *testing.T) {
	v1Dir, v2Dir := buildTestCorraldDist(t)
	h := newHarness(t, func(c *config.Config) {
		c.CorraldDir = v1Dir
		c.GatewayID = "gw-crashrb.local"
	})
	admin := h.admin()
	srv, home := startCorraldSSH(t)

	distV1, _ := gw.LoadDist(v1Dir)
	h.srv.corrald.Close()
	h.srv.corrald = gw.NewManager(gw.ManagerConfig{
		Store:           h.store,
		Dist:            distV1,
		GatewayID:       "gw-crashrb.local",
		SSH:             &webSSHProvider{s: h.srv},
		HealthInterval:  5 * time.Second,
		RestartBackoffs: []time.Duration{20 * time.Millisecond, 20 * time.Millisecond, 20 * time.Millisecond},
		CrashWindow:     30 * time.Minute,
		CrashThreshold:  2,
	})
	hostID, _ := createAndPinHost(t, admin, srv, "crashrbbox")

	// Install v1, then hot-update to v2 cleanly.
	startCorraldInstall(t, admin, hostID)
	waitCorraldState(t, admin, hostID, "running")

	distV2, _ := gw.LoadDist(v2Dir)
	h.srv.CorraldManager().SetDist(distV2)
	mustStatus(t, admin.do("POST", fmt.Sprintf("/api/hosts/%d/corrald/update", hostID), map[string]any{"manifest_hash": distV2.ManifestHash}), http.StatusAccepted, "update to v2")
	st := waitCorraldState(t, admin, hostID, "running")
	if st["version"] != "0.3.2" {
		t.Fatalf("expected 0.3.2 before crash loop, got %v", st["version"])
	}

	uid := int64(admin.do("GET", "/api/me", nil).json()["id"].(float64))
	hk, _ := h.store.HostKey(context.Background(), uid, hostID)

	// Simulate 2 abnormal exits within 30 minutes of update:
	// 1st disconnect recovers on v2; 2nd disconnect triggers automatic rollback to v1!
	if !h.srv.CorraldManager().ForceDropConn(hk.Fingerprint, "alice") {
		t.Fatal("first ForceDropConn returned false")
	}
	waitCorraldState(t, admin, hostID, "running")

	if !h.srv.CorraldManager().ForceDropConn(hk.Fingerprint, "alice") {
		t.Fatal("second ForceDropConn returned false")
	}
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		st = waitCorraldState(t, admin, hostID, "running")
		if st["version"] == "0.3.1" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if st["version"] != "0.3.1" {
		t.Fatalf("expected automatic rollback to 0.3.1 after 2 crashes, got %v", st)
	}
	errObj, _ := st["error"].(map[string]any)
	if errObj == nil || errObj["code"] != "update_failed" || errObj["target_version"] != "0.3.2" || errObj["fallback_version"] != "0.3.1" {
		t.Fatalf("expected update_failed rollback error, got %v", errObj)
	}
	matches, _ := filepath.Glob(filepath.Join(home, ".local", "state", "corral", "corrald", "*", "failed.json"))
	if len(matches) == 0 {
		t.Fatal("expected failed.json to be written on rollback")
	}
	fb, _ := os.ReadFile(matches[0])
	if !strings.Contains(string(fb), "2000") {
		t.Fatalf("expected release 2000 in failed.json after rollback, got %s", fb)
	}
}
