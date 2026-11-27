//go:build linux

package web

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/ihainan/corral/internal/config"
	"github.com/ihainan/corral/internal/corrald/gw"
	"github.com/ihainan/corral/internal/corrald/proto"
	"github.com/ihainan/corral/internal/store"
)

// C-4 & C-2: On an NFS shared home where Machine A already installed a newer
// Release (2000) and moved the shared symlink ~/.local/lib/corral/corrald to
// 2000, Machine B (which has no <midB>/install.json yet) enabled by a gateway
// carrying Release 1000 must NOT skip install and fail serve; it must run
// `corrald install` for midB, preserve the shared symlink at 2000 (no downgrade),
// and start running cleanly.
func TestCorraldNFSNewMachineFirstInstallDoesNotSkipOrDowngrade(t *testing.T) {
	v1Dir, v2Dir := buildTestCorraldDist(t)
	h := newHarness(t, func(c *config.Config) {
		c.CorraldDir = v1Dir // gateway carries older v1 (Release 1000)
		c.GatewayID = "gw-nfs-c4.local"
	})
	admin := h.admin()

	const midA = "aaaaaaaa1111"
	const midB = "bbbbbbbb2222"

	// Start SSH server for Machine B with CORRALD_TEST_MACHINE_ID=midB.
	srv, home := startCorraldSSH(t, "CORRALD_TEST_MACHINE_ID="+midB)

	// Pre-populate Machine A (midA) with v2 (Release 2000) in the same shared HOME,
	// so ~/.local/lib/corral/corrald already points to corrald-0.3.2-2000/corrald!
	v2Dist, err := gw.LoadDist(v2Dir)
	if err != nil {
		t.Fatal(err)
	}
	v2Bin := v2Dist.Binaries["amd64"]
	if v2Bin == nil {
		for _, b := range v2Dist.Binaries {
			v2Bin = b
			break
		}
	}
	stageA := filepath.Join(home, ".local", "state", "corral", "corrald", midA, "stage-opA")
	if err := os.MkdirAll(stageA, 0o700); err != nil {
		t.Fatal(err)
	}
	bBytes, err := os.ReadFile(v2Bin.Path)
	if err != nil {
		t.Fatal(err)
	}
	stageBinA := filepath.Join(stageA, "corrald")
	if err := os.WriteFile(stageBinA, bBytes, 0o700); err != nil {
		t.Fatal(err)
	}
	instCmd := exec.Command(stageBinA, "install", "--stage", stageA, "--op", "opA", "--gateway", "gw-other", "--by", "bob")
	instCmd.Env = append(os.Environ(), "HOME="+home, "CORRALD_TEST_MACHINE_ID="+midA)
	if out, err := instCmd.CombinedOutput(); err != nil {
		t.Fatalf("pre-install v2 on midA failed: %v\n%s", err, out)
	}
	sharedLink := filepath.Join(home, ".local", "lib", "corral", "corrald")
	linkBefore, err := os.Readlink(sharedLink)
	if err != nil || !strings.Contains(linkBefore, "2000") {
		t.Fatalf("expected shared symlink at 2000 before midB install, got %q (%v)", linkBefore, err)
	}

	// Override ProbeOverride so Machine B's probe reports midB (matching CORRALD_TEST_MACHINE_ID).
	v1Dist, _ := gw.LoadDist(v1Dir)
	h.srv.corrald.Close()
	h.srv.corrald = gw.NewManager(gw.ManagerConfig{
		Store:     h.store,
		Dist:      v1Dist,
		GatewayID: "gw-nfs-c4.local",
		SSH:       &webSSHProvider{s: h.srv},
		ProbeOverride: func(ctx context.Context, c *ssh.Client) (*gw.HostProbe, error) {
			p, err := gw.ProbeHost(ctx, c)
			if err != nil {
				return nil, err
			}
			p.MachineID = midB
			p.StateDir = p.StateRoot + "/" + midB
			p.InstallJSON = p.StateDir + "/install.json"
			p.MetaJSON = p.StateDir + "/meta.json"
			p.ClientsJSON = p.StateDir + "/clients.json"
			p.FailedJSON = p.StateDir + "/failed.json"
			p.OpsDir = p.StateDir + "/ops"
			p.TombstonePath = p.StateDir + "/removing"
			p.SocketPath = gw.ComputeSocketPath(p.Home, p.UID, midB)
			p.HasInstall = false
			return p, nil
		},
	})

	hostID, _ := createAndPinHost(t, admin, srv, "nfs-machine-b")
	startCorraldInstall(t, admin, hostID)
	st := waitCorraldState(t, admin, hostID, "running")
	if st["state"] != "running" {
		t.Fatalf("expected Machine B running after install, got %v", st)
	}
	// Verify midB's install.json was created!
	if _, err := os.Stat(filepath.Join(home, ".local", "state", "corral", "corrald", midB, "install.json")); err != nil {
		t.Fatalf("expected midB install.json to exist: %v", err)
	}
	// Verify shared symlink was NOT downgraded below 2000!
	linkAfter, err := os.Readlink(sharedLink)
	if err != nil || !strings.Contains(linkAfter, "2000") {
		t.Fatalf("shared symlink was downgraded! before=%q after=%q (%v)", linkBefore, linkAfter, err)
	}
}

// C-5: When corrald hangs and never replies to agent.locate / convo.tails, the
// gateway must cap corrald's RPC time budget to at most half the remaining
// context deadline (and <= 3s) so the fallback SSH script still completes
// within the caller's original deadline.
func TestCorraldHungRPCTimeBudgetAllowsSSHFallback(t *testing.T) {
	v1Dir, _ := buildTestCorraldDist(t)
	h := newHarness(t, func(c *config.Config) {
		c.CorraldDir = v1Dir
		c.GatewayID = "gw-budget.local"
	})
	admin := h.admin()
	srv, home, nonCorraldExecs := startCountingCorraldSSH(t)
	agents := spawnE2EFakeAgents(t, home)

	hostID, sessID := createAndPinHost(t, admin, srv, "hang-box")
	uid := int64(admin.do("GET", "/api/me", nil).json()["id"].(float64))
	sess, err := h.srv.terms.Get(uid, sessID)
	if err != nil {
		t.Fatalf("session not found: %v", err)
	}
	sess.Persistence = "herdr"
	h.srv.herdrSocks.set(sessID, agents.herdrSock)

	// Start a mock corrald socket that answers hello/resume/health normally,
	// then HANGS forever on agent.locate and convo.tails!
	sockDir, err := os.MkdirTemp("/tmp", "c-hang-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(sockDir) })
	sockPath := filepath.Join(sockDir, "corrald.sock")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	stopHang := make(chan struct{})
	t.Cleanup(func() { close(stopHang) })

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
						res, _ := json.Marshal(proto.HelloResult{
							Version: "0.3.1", Release: 1000, Protocol: proto.Protocol,
							Epoch: "hang-ep-1", PID: 555, Manifest: proto.DefaultManifest(), State: proto.StateRunning,
						})
						_ = proto.WriteJSON(conn, proto.Response{ID: req.ID, Result: res})
					case proto.MethodDaemonResume:
						res, _ := json.Marshal(proto.ResumeResult{Epoch: "hang-ep-1"})
						_ = proto.WriteJSON(conn, proto.Response{ID: req.ID, Result: res})
					case proto.MethodDaemonHealth:
						res, _ := json.Marshal(proto.HealthResult{Caps: map[string]string{"locate": "ok", "tails": "ok"}, PID: 555})
						_ = proto.WriteJSON(conn, proto.Response{ID: req.ID, Result: res})
					case proto.MethodAgentLocate, proto.MethodConvoTails, proto.MethodConvoMessageTails:
						// Hang until test ends — never reply!
						<-stopHang
						return
					}
				}
			}(c)
		}
	}()

	dist, _ := gw.LoadDist(v1Dir)
	h.srv.corrald.Close()
	h.srv.corrald = gw.NewManager(gw.ManagerConfig{
		Store:     h.store,
		Dist:      dist,
		GatewayID: "gw-budget.local",
		SSH:       &webSSHProvider{s: h.srv},
		ProbeOverride: func(context.Context, *ssh.Client) (*gw.HostProbe, error) {
			return &gw.HostProbe{
				OS: "Linux", Machine: "x86_64", Arch: "amd64", Home: home, HomeWritable: true,
				HasSymlink: true, MachineID: "0123456789ab", SocketPath: sockPath,
				StateDir: filepath.Join(home, ".local", "state", "corral", "corrald", "0123456789ab"),
			}, nil
		},
	})
	hk, err := h.store.HostKey(context.Background(), uid, hostID)
	if err != nil {
		t.Fatal(err)
	}
	mb, _ := proto.MarshalManifest(dist.Manifest)
	if err := h.store.UpsertCorraldHost(context.Background(), &store.CorraldHost{
		HostID: hostID, State: gw.StateRunning, BoundFingerprint: hk.Fingerprint,
		BoundUsername: "alice", BoundGateway: "gw-budget.local", ManifestJSON: string(mb),
		Version: "0.3.1", Release: 1000, MachineID: "0123456789ab", SocketPath: sockPath,
	}); err != nil {
		t.Fatal(err)
	}
	h.srv.corrald.OnHostConnected(context.Background(), uid, hostID, "admin", hk.Fingerprint, "alice", sess.Client())

	// Verify budget calculation directly for both half-remaining and 3s cap.
	shortCtx, shortCancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer shortCancel()
	if b := corraldBudgetDuration(shortCtx); b <= 0 || b > 550*time.Millisecond {
		t.Fatalf("corraldBudgetDuration(1s) = %v, want <= 500ms", b)
	}
	longCtx, longCancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer longCancel()
	if b := corraldBudgetDuration(longCtx); b != maxCorraldRPCBudget {
		t.Fatalf("corraldBudgetDuration(20s) = %v, want %v", b, maxCorraldRPCBudget)
	}

	// 1. Locate (fast tree path so SSH fallback itself takes ~15ms instead of
	// scanning every host /proc entry under -race) with 2s deadline: corrald
	// gets <= 1s (half of 2s, < 3s cap), leaving >= 1s for SSH fallback.
	locCtx, locCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer locCancel()
	nonCorraldExecs.Store(0)
	pidsCmd := convoLocatePidsCommand(agents.shellPID, []int{agents.claudePID})
	pidsParams := proto.AgentLocateParams{Mode: "herdr", ShellPID: agents.shellPID, PIDs: []int{agents.claudePID}}
	out, _, _, _, err := h.srv.runConvoScan(locCtx, sess, pidsParams, pidsCmd, time.Time{})
	if err != nil {
		t.Fatalf("locate fallback after hung corrald failed within deadline: %v", err)
	}
	gotLoc := summarizeLocated(parseConvoLocate(string(out)), agents.herdrSock)
	if len(gotLoc) != 1 || gotLoc[0].PID != int64(agents.claudePID) {
		t.Fatalf("unexpected fallback locate result: %+v", gotLoc)
	}
	if nonCorraldExecs.Load() != 1 {
		t.Fatalf("expected 1 SSH fallback exec for locate, got %d", nonCorraldExecs.Load())
	}

	// 2. Tails with 2s deadline: corrald gets <= 1s, falls back to SSH and succeeds within 2s!
	tailCtx, tailCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer tailCancel()
	logs := []string{agents.claudeLog, agents.codexLog}
	tailsOut, err := h.srv.convoTailsOutput(tailCtx, sess, logs, 256<<10)
	if err != nil {
		t.Fatalf("tails fallback after hung corrald failed within deadline: %v", err)
	}
	if len(splitTails(string(tailsOut), logs)) != 2 {
		t.Fatalf("expected 2 tails after SSH fallback, got %+v", splitTails(string(tailsOut), logs))
	}
	if nonCorraldExecs.Load() != 2 {
		t.Fatalf("expected 2 total SSH fallback execs, got %d", nonCorraldExecs.Load())
	}
}
