//go:build linux

package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/ihainan/corral/internal/config"
	"github.com/ihainan/corral/internal/corrald/gw"
	"github.com/ihainan/corral/internal/corrald/proto"
	"github.com/ihainan/corral/internal/store"
	"github.com/ihainan/corral/internal/testutil"
)

type e2eFakeAgents struct {
	home       string
	herdrSock  string
	shellPID   int
	claudePane string
	claudeSID  string
	claudePID  int
	claudeLog  string
	codexPane  string
	codexPID   int
	codexLog   string
	cwd        string
}

const fakeLauncherSrc = `package main

import (
	"os"
	"os/exec"
	"strconv"
)

func main() {
	if len(os.Args) < 3 {
		os.Exit(2)
	}
	pidFile := os.Args[1]
	cmd := exec.Command(os.Args[2], os.Args[3:]...)
	cmd.Dir, _ = os.Getwd()
	cmd.Env = os.Environ()
	if err := cmd.Start(); err != nil {
		os.Exit(1)
	}
	_ = os.WriteFile(pidFile, []byte(strconv.Itoa(cmd.Process.Pid)), 0o600)
	_ = cmd.Wait()
}
`

func buildFakeLauncherBinary(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	src := filepath.Join(dir, "main.go")
	if err := os.WriteFile(src, []byte(fakeLauncherSrc), 0o600); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "launcher")
	cmd := exec.Command("go", "build", "-o", bin, src)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build fake launcher: %v\n%s", err, out)
	}
	return bin
}

// spawnE2EFakeAgents starts self-contained fake Claude Code and Codex processes
// under a fake `herdr` -> `sh` process tree inside home with real session/log
// files so both SSH locate/tails scripts and corrald agent.locate/convo.tails
// discover them identically, including the herdr fast PIDs tree path and
// GET /api/sessions/{id}/conversation.
func spawnE2EFakeAgents(t *testing.T, home string) *e2eFakeAgents {
	t.Helper()
	helperBin := buildFakeAgentBinary(t)
	launcherBin := buildFakeLauncherBinary(t)
	binDir := filepath.Join(home, "fake-agents-bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	herdrBin := filepath.Join(binDir, "herdr")
	shellBin := filepath.Join(binDir, "sh")
	claudeBin := filepath.Join(binDir, "claude")
	codexBin := filepath.Join(binDir, "codex")
	for _, pair := range [][2]string{
		{launcherBin, herdrBin},
		{launcherBin, shellBin},
		{helperBin, claudeBin},
		{helperBin, codexBin},
	} {
		if err := os.Symlink(pair[0], pair[1]); err != nil {
			t.Fatal(err)
		}
	}

	cwd := filepath.Join(home, "work-proj")
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatal(err)
	}

	herdrSock := filepath.Join(home, "herdr-e2e.sock")
	claudePane := "w1:p1"
	claudeSID := "11111111-1111-4111-8111-111111111111"

	// Create Claude project log file with real conversation turns.
	encodedCwd := strings.NewReplacer("/", "-", "_", "-", ".", "-").Replace(cwd)
	claudeProjDir := filepath.Join(home, ".claude", "projects", encodedCwd)
	claudeSessDir := filepath.Join(home, ".claude", "sessions")
	if err := os.MkdirAll(claudeProjDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(claudeSessDir, 0o755); err != nil {
		t.Fatal(err)
	}
	claudeLog := filepath.Join(claudeProjDir, claudeSID+".jsonl")
	claudeLogBody := strings.Join([]string{
		`{"type":"user","uuid":"u-1","timestamp":"2026-11-27T10:00:00Z","message":{"role":"user","content":"Explain corrald architecture"}}`,
		`{"type":"assistant","uuid":"a-1","timestamp":"2026-11-27T10:00:02Z","message":{"role":"assistant","content":[{"type":"text","text":"Corrald runs per-host over SSH direct-streamlocal."}]}}`,
		"",
	}, "\n")
	if err := os.WriteFile(claudeLog, []byte(claudeLogBody), 0o600); err != nil {
		t.Fatal(err)
	}

	// Create Codex rollout log file.
	codexPane := "w1:p2"
	codexSID := "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	codexDir := filepath.Join(home, ".codex", "sessions", "2026", "11", "27")
	if err := os.MkdirAll(codexDir, 0o755); err != nil {
		t.Fatal(err)
	}
	codexLog := filepath.Join(codexDir, "rollout-2026-11-27T10-00-00-"+codexSID+".jsonl")
	codexLogBody := strings.Join([]string{
		fmt.Sprintf(`{"timestamp":"2026-11-27T10:00:00Z","type":"session_meta","payload":{"id":%q,"cwd":%q,"timestamp":"2026-11-27T10:00:00Z"}}`, codexSID, cwd),
		`{"timestamp":"2026-11-27T10:00:01Z","type":"event_msg","payload":{"type":"user_message","message":"Check codex tail parity"}}`,
		`{"timestamp":"2026-11-27T10:00:03Z","type":"event_msg","payload":{"type":"agent_message","message":"Codex tail parity verified."}}`,
		"",
	}, "\n")
	if err := os.WriteFile(codexLog, []byte(codexLogBody), 0o600); err != nil {
		t.Fatal(err)
	}

	// Spawn fake herdr -> sh -> claude process tree.
	claudeReady := filepath.Join(home, "claude.ready")
	shellPIDFile := filepath.Join(home, "shell.pid")
	claudePIDFile := filepath.Join(home, "claude.pid")
	claudeTreeCmd := exec.Command(herdrBin, shellPIDFile, shellBin, claudePIDFile, claudeBin)
	claudeTreeCmd.Dir = cwd
	claudeTreeCmd.Env = []string{
		"HOME=" + home,
		"HERDR_PANE_ID=" + claudePane,
		"HERDR_SOCKET_PATH=" + herdrSock,
		"FAKE_READY_FILE=" + claudeReady,
	}
	if err := claudeTreeCmd.Start(); err != nil {
		t.Fatalf("start fake claude tree: %v", err)
	}
	waitForFile(t, claudeReady)
	shellPID, err := strconv.Atoi(strings.TrimSpace(waitForFile(t, shellPIDFile)))
	if err != nil {
		t.Fatalf("parse shell pid: %v", err)
	}
	claudePID, err := strconv.Atoi(strings.TrimSpace(waitForFile(t, claudePIDFile)))
	if err != nil {
		t.Fatalf("parse claude pid: %v", err)
	}
	t.Cleanup(func() {
		if p, err := os.FindProcess(claudePID); err == nil {
			_ = p.Kill()
		}
		if p, err := os.FindProcess(shellPID); err == nil {
			_ = p.Kill()
		}
		_ = claudeTreeCmd.Process.Kill()
		_, _ = claudeTreeCmd.Process.Wait()
	})
	claudeStart := waitForProcStart(t, claudePID)

	// Write Claude session file ~/.claude/sessions/<pid>.json matching this process.
	sessJSON := fmt.Sprintf(`{"pid":%d,"sessionId":%q,"cwd":%q,"procStart":%q}`, claudePID, claudeSID, cwd, claudeStart)
	if err := os.WriteFile(filepath.Join(claudeSessDir, strconv.Itoa(claudePID)+".json"), []byte(sessJSON), 0o600); err != nil {
		t.Fatal(err)
	}

	// Spawn fake herdr -> codex process holding codexLog open.
	codexReady := filepath.Join(home, "codex.ready")
	codexPIDFile := filepath.Join(home, "codex.pid")
	codexTreeCmd := exec.Command(herdrBin, codexPIDFile, codexBin)
	codexTreeCmd.Dir = cwd
	codexTreeCmd.Env = []string{
		"HOME=" + home,
		"CODEX_HOME=" + filepath.Join(home, ".codex"),
		"HERDR_PANE_ID=" + codexPane,
		"HERDR_SOCKET_PATH=" + herdrSock,
		"FAKE_OPEN_FILES=" + codexLog,
		"FAKE_READY_FILE=" + codexReady,
	}
	if err := codexTreeCmd.Start(); err != nil {
		t.Fatalf("start fake codex tree: %v", err)
	}
	waitForFile(t, codexReady)
	codexPID, err := strconv.Atoi(strings.TrimSpace(waitForFile(t, codexPIDFile)))
	if err != nil {
		t.Fatalf("parse codex pid: %v", err)
	}
	t.Cleanup(func() {
		if p, err := os.FindProcess(codexPID); err == nil {
			_ = p.Kill()
		}
		_ = codexTreeCmd.Process.Kill()
		_, _ = codexTreeCmd.Process.Wait()
	})
	_ = waitForProcStart(t, codexPID)

	return &e2eFakeAgents{
		home:       home,
		herdrSock:  herdrSock,
		shellPID:   shellPID,
		claudePane: claudePane,
		claudeSID:  claudeSID,
		claudePID:  claudePID,
		claudeLog:  claudeLog,
		codexPane:  codexPane,
		codexPID:   codexPID,
		codexLog:   codexLog,
		cwd:        cwd,
	}
}

type locateSummary struct {
	PID         int64
	Agent       string
	SID         string
	Log         string
	Cwd         string
	HerdrPane   string
	HerdrSocket string
}

func summarizeLocated(l convoLocated, sock string) []locateSummary {
	var out []locateSummary
	for _, p := range l.procs {
		if p.herdrSocket != sock {
			continue
		}
		out = append(out, locateSummary{
			PID:         p.pid,
			Agent:       p.agent,
			SID:         p.sid,
			Log:         p.log,
			Cwd:         p.cwd,
			HerdrPane:   p.herdrPane,
			HerdrSocket: p.herdrSocket,
		})
	}
	return out
}

// startCountingCorraldSSH starts a real test SSH server that runs both corrald
// lifecycle commands and locate/tails shell scripts inside home, while counting
// non-corrald remote exec calls (locate/tails scripts).
func startCountingCorraldSSH(t *testing.T, extraEnv ...string) (*testutil.SSHServer, string, *atomic.Int32) {
	t.Helper()
	home := t.TempDir()
	srv := testutil.StartSSHServer(t, "alice", "alice-pass", home)
	isLocateOrTails := func(cmd string) bool {
		return strings.Contains(cmd, "hostnow=") ||
			strings.Contains(cmd, "tail -c") ||
			strings.Contains(cmd, "convo") ||
			strings.Contains(cmd, "/.claude/sessions")
	}
	isCorraldLifecycle := func(cmd string) bool {
		return strings.Contains(cmd, gw.CommandTag) || strings.Contains(cmd, "corrald")
	}
	realExec := testutil.ShellExec(home, func(cmd string) bool {
		return isCorraldLifecycle(cmd) || isLocateOrTails(cmd)
	}, extraEnv...)

	var nonCorraldExecs atomic.Int32
	srv.HandleExec(func(cmd string) func(ch ssh.Channel) {
		if isLocateOrTails(cmd) && !isCorraldLifecycle(cmd) {
			nonCorraldExecs.Add(1)
		}
		return realExec(cmd)
	})
	t.Cleanup(func() {
		bin := filepath.Join(home, ".local", "lib", "corral", "corrald")
		if _, err := os.Stat(bin); err == nil {
			c := exec.Command(bin, "stop")
			c.Env = append(os.Environ(), "HOME="+home)
			_ = c.Run()
		}
	})
	return srv, home, &nonCorraldExecs
}

func TestCorraldAgentLocateTailsParityAndZeroSSHExec(t *testing.T) {
	v1Dir, _ := buildTestCorraldDist(t)
	h := newHarness(t, func(c *config.Config) {
		c.CorraldDir = v1Dir
		c.GatewayID = "gw-e2e-parity.local"
	})
	admin := h.admin()
	srv, home, nonCorraldExecs := startCountingCorraldSSH(t)
	agents := spawnE2EFakeAgents(t, home)

	hostID, sessID := createAndPinHost(t, admin, srv, "parity-box")
	uid := int64(admin.do("GET", "/api/me", nil).json()["id"].(float64))
	sess, err := h.srv.terms.Get(uid, sessID)
	if err != nil {
		t.Fatalf("session not found: %v", err)
	}
	sess.Persistence = "herdr"
	h.srv.herdrSocks.set(sessID, agents.herdrSock)

	ctx := context.Background()
	fullCmd := convoLocateCommand("herdr", "", "")
	fullParams := proto.AgentLocateParams{Mode: "herdr"}
	pidsCmd := convoLocatePidsCommand(agents.shellPID, []int{agents.claudePID})
	pidsParams := proto.AgentLocateParams{Mode: "herdr", ShellPID: agents.shellPID, PIDs: []int{agents.claudePID}}
	logs := []string{agents.claudeLog, agents.codexLog}

	// --- Baseline: corrald OFF (everything goes through SSH scripts) ---
	nonCorraldExecs.Store(0)
	offFullOut, _, _, _, err := h.srv.runConvoScan(ctx, sess, fullParams, fullCmd, time.Time{})
	if err != nil {
		t.Fatalf("off full locate: %v", err)
	}
	offFull := summarizeLocated(parseConvoLocate(string(offFullOut)), agents.herdrSock)
	if len(offFull) != 2 {
		t.Fatalf("off full locate found %d test agents, want 2: %+v", len(offFull), offFull)
	}

	offPidsOut, _, _, _, err := h.srv.runConvoScan(ctx, sess, pidsParams, pidsCmd, time.Time{})
	if err != nil {
		t.Fatalf("off pids locate: %v", err)
	}
	offPidsLoc := parseConvoLocate(string(offPidsOut))
	offPids := summarizeLocated(offPidsLoc, agents.herdrSock)
	if offPidsLoc.fastIncomplete || len(offPids) != 1 || offPids[0].PID != int64(agents.claudePID) {
		t.Fatalf("off pids locate unexpected: incomplete=%v procs=%+v", offPidsLoc.fastIncomplete, offPids)
	}

	offTailsRaw, err := h.srv.convoTailsOutput(ctx, sess, logs, 256<<10)
	if err != nil {
		t.Fatalf("off tails: %v", err)
	}
	offTails := splitTails(string(offTailsRaw), logs)
	if len(offTails) != 2 {
		t.Fatalf("off tails returned %d logs, want 2", len(offTails))
	}

	// Full conversation API read with corrald OFF.
	rOff := admin.do("GET", fmt.Sprintf("/api/sessions/%s/conversation?pane=%s", sessID, agents.claudePane), nil)
	mustStatus(t, rOff, http.StatusOK, "conversation off")
	convoOff := rOff.json()
	bindOff, _ := convoOff["bind"].(map[string]any)
	if convoOff["available"] != true || bindOff == nil || bindOff["agent"] != "claude" || bindOff["session"] != agents.claudeSID {
		t.Fatalf("unexpected conversation off response: %s", rOff.Body)
	}
	entriesOff, _ := convoOff["entries"].([]any)
	if len(entriesOff) != 2 {
		t.Fatalf("expected 2 conversation entries, got: %s", rOff.Body)
	}
	if nonCorraldExecs.Load() == 0 {
		t.Fatal("expected SSH execs when corrald is off")
	}

	// --- Enable corrald (real compiled binary with agent module) ---
	startCorraldInstall(t, admin, hostID)
	st := waitCorraldState(t, admin, hostID, "running")
	caps, _ := st["caps"].(map[string]any)
	if caps["locate"] != "ok" || caps["tails"] != "ok" || caps["watch"] != "ok" {
		t.Fatalf("expected agent health caps ok, got: %v", caps)
	}

	// Clear scan cache & conversation cache and reset SSH exec counter.
	h.srv.convoScans.mu.Lock()
	h.srv.convoScans.m = nil
	h.srv.convoScans.mu.Unlock()
	h.srv.convos.mu.Lock()
	h.srv.convos.m = nil
	h.srv.convos.mu.Unlock()
	nonCorraldExecs.Store(0)

	// 1. Full locate via corrald.
	onFullOut, _, _, _, err := h.srv.runConvoScan(ctx, sess, fullParams, fullCmd, time.Time{})
	if err != nil {
		t.Fatalf("on full locate: %v", err)
	}
	onFull := summarizeLocated(parseConvoLocate(string(onFullOut)), agents.herdrSock)
	if !reflect.DeepEqual(onFull, offFull) {
		t.Fatalf("full locate mismatch:\n off=%+v\n  on=%+v", offFull, onFull)
	}

	// 2. Fast PIDs locate via corrald.
	onPidsOut, _, _, _, err := h.srv.runConvoScan(ctx, sess, pidsParams, pidsCmd, time.Time{})
	if err != nil {
		t.Fatalf("on pids locate: %v", err)
	}
	onPidsLoc := parseConvoLocate(string(onPidsOut))
	onPids := summarizeLocated(onPidsLoc, agents.herdrSock)
	if onPidsLoc.fastIncomplete != offPidsLoc.fastIncomplete || !reflect.DeepEqual(onPids, offPids) {
		t.Fatalf("pids locate mismatch:\n off=(incomplete=%v %+v)\n  on=(incomplete=%v %+v)", offPidsLoc.fastIncomplete, offPids, onPidsLoc.fastIncomplete, onPids)
	}

	// 3. Times locate via corrald + hostClockAhead parity.
	timesOut, usedCorrald := h.srv.tryCorraldLocate(ctx, sess, proto.AgentLocateParams{Mode: "herdr", Times: true})
	if !usedCorrald {
		t.Fatal("expected times locate to use corrald")
	}
	if _, hasClock := hostClockAhead(string(timesOut), time.Now().UnixMilli()); !hasClock {
		t.Fatalf("expected hostClockAhead to parse corrald times output: %q", string(timesOut))
	}

	// 4. convo.tails via corrald.
	onTailsRaw, err := h.srv.convoTailsOutput(ctx, sess, logs, 256<<10)
	if err != nil {
		t.Fatalf("on tails: %v", err)
	}
	onTails := splitTails(string(onTailsRaw), logs)
	if !reflect.DeepEqual(onTails, offTails) {
		t.Fatalf("convo.tails mismatch:\n off=%+v\n  on=%+v", offTails, onTails)
	}

	// 5. convo.message_tails via corrald.
	msgTailsRaw, usedMsgCorrald := h.srv.tryCorraldMessageTails(ctx, sess, logs)
	if !usedMsgCorrald {
		t.Fatal("expected convo.message_tails to succeed via corrald")
	}
	msgTails := splitTails(string(msgTailsRaw), logs)
	if len(msgTails) != 2 || !strings.Contains(msgTails[agents.codexLog], "Codex tail parity verified.") {
		t.Fatalf("unexpected message_tails output: %+v", msgTails)
	}

	// 6. Full HTTP conversation endpoint via corrald (fresh cache).
	h.srv.convoScans.mu.Lock()
	h.srv.convoScans.m = nil
	h.srv.convoScans.mu.Unlock()
	rOn := admin.do("GET", fmt.Sprintf("/api/sessions/%s/conversation?pane=%s", sessID, agents.claudePane), nil)
	mustStatus(t, rOn, http.StatusOK, "conversation on")
	convoOn := rOn.json()
	if convoOn["available"] != true || !reflect.DeepEqual(convoOn["bind"], convoOff["bind"]) || !reflect.DeepEqual(convoOn["entries"], convoOff["entries"]) {
		t.Fatalf("conversation API mismatch:\n off=%s\n  on=%s", rOff.Body, rOn.Body)
	}

	// 7. Assert ZERO remote SSH execs were run for locate or tails!
	if got := nonCorraldExecs.Load(); got != 0 {
		t.Fatalf("expected 0 remote SSH execs when corrald is enabled, got %d", got)
	}
}

func TestCorraldAgentFallbackScenarios(t *testing.T) {
	v1Dir, _ := buildTestCorraldDist(t)
	h := newHarness(t, func(c *config.Config) {
		c.CorraldDir = v1Dir
		c.GatewayID = "gw-e2e-fallback.local"
	})
	admin := h.admin()
	srv, home, nonCorraldExecs := startCountingCorraldSSH(t)
	agents := spawnE2EFakeAgents(t, home)

	hostID, sessID := createAndPinHost(t, admin, srv, "fallback-box")
	uid := int64(admin.do("GET", "/api/me", nil).json()["id"].(float64))
	sess, err := h.srv.terms.Get(uid, sessID)
	if err != nil {
		t.Fatalf("session not found: %v", err)
	}
	sess.Persistence = "herdr"
	h.srv.herdrSocks.set(sessID, agents.herdrSock)

	startCorraldInstall(t, admin, hostID)
	waitCorraldState(t, admin, hostID, "running")

	ctx := context.Background()
	fullCmd := convoLocateCommand("herdr", "", "")
	fullParams := proto.AgentLocateParams{Mode: "herdr"}
	logs := []string{agents.claudeLog, agents.codexLog}

	// Baseline via corrald.
	nonCorraldExecs.Store(0)
	baseOut, _, _, _, err := h.srv.runConvoScan(ctx, sess, fullParams, fullCmd, time.Time{})
	if err != nil {
		t.Fatalf("baseline locate: %v", err)
	}
	baseSummary := summarizeLocated(parseConvoLocate(string(baseOut)), agents.herdrSock)
	baseTailsRaw, err := h.srv.convoTailsOutput(ctx, sess, logs, 256<<10)
	if err != nil {
		t.Fatalf("baseline tails: %v", err)
	}
	baseTails := splitTails(string(baseTailsRaw), logs)
	if nonCorraldExecs.Load() != 0 {
		t.Fatalf("expected 0 SSH execs on baseline corrald call, got %d", nonCorraldExecs.Load())
	}

	clearCache := func() {
		h.srv.convoScans.mu.Lock()
		h.srv.convoScans.m = nil
		h.srv.convoScans.mu.Unlock()
	}

	// --- Scenario 1: Single capability marked degraded ("locate" degraded, "tails" ok) ---
	h.srv.CorraldManager().SetHealthCapsForTest(hostID, map[string]string{"locate": "degraded", "tails": "ok"})
	clearCache()
	nonCorraldExecs.Store(0)
	degOut, _, _, _, err := h.srv.runConvoScan(ctx, sess, fullParams, fullCmd, time.Time{})
	if err != nil {
		t.Fatalf("degraded locate fallback: %v", err)
	}
	if !reflect.DeepEqual(summarizeLocated(parseConvoLocate(string(degOut)), agents.herdrSock), baseSummary) {
		t.Fatal("degraded locate fallback result mismatch")
	}
	if nonCorraldExecs.Load() != 1 {
		t.Fatalf("expected 1 SSH exec for degraded locate, got %d", nonCorraldExecs.Load())
	}
	// Tails must still run over corrald (no additional SSH exec)!
	tailsOut, err := h.srv.convoTailsOutput(ctx, sess, logs, 256<<10)
	if err != nil || !reflect.DeepEqual(splitTails(string(tailsOut), logs), baseTails) {
		t.Fatalf("tails while locate degraded failed: %v", err)
	}
	if nonCorraldExecs.Load() != 1 {
		t.Fatalf("expected tails to stay on corrald (still 1 total SSH exec), got %d", nonCorraldExecs.Load())
	}

	// Restore healthy caps.
	h.srv.CorraldManager().SetHealthCapsForTest(hostID, map[string]string{"locate": "ok", "tails": "ok"})

	// --- Scenario 2: mode == "tmux" with empty tmux result falls back to SSH ---
	// Our real corrald has no tmux session named "missing-sess", so agent.locate returns tmux=\x00.
	nonCorraldExecs.Store(0)
	if _, ok := h.srv.tryCorraldLocate(ctx, sess, proto.AgentLocateParams{Mode: "tmux", TmuxName: "missing-sess"}); ok {
		t.Fatal("expected mode=tmux with empty tmux result to trigger fallback (ok=false)")
	}
	// Reset failure counter from the 1 empty tmux probe.
	h.srv.CorraldManager().ReportCapSuccess(hostID, "locate")

	// --- Scenario 3: Corrald stopped (paused/stopped) falls back seamlessly ---
	rPause := admin.do("POST", fmt.Sprintf("/api/hosts/%d/corrald/pause", hostID), nil)
	mustStatus(t, rPause, http.StatusOK, "pause corrald")
	clearCache()
	nonCorraldExecs.Store(0)
	pausedOut, _, _, _, err := h.srv.runConvoScan(ctx, sess, fullParams, fullCmd, time.Time{})
	if err != nil {
		t.Fatalf("paused locate fallback: %v", err)
	}
	if !reflect.DeepEqual(summarizeLocated(parseConvoLocate(string(pausedOut)), agents.herdrSock), baseSummary) {
		t.Fatal("paused locate fallback mismatch")
	}
	pausedTailsRaw, err := h.srv.convoTailsOutput(ctx, sess, logs, 256<<10)
	if err != nil || !reflect.DeepEqual(splitTails(string(pausedTailsRaw), logs), baseTails) {
		t.Fatalf("paused tails fallback mismatch: %v", err)
	}
	if nonCorraldExecs.Load() < 2 {
		t.Fatalf("expected SSH fallbacks for both locate and tails while paused, got %d", nonCorraldExecs.Load())
	}
}

func TestCorraldAgentBusyErrorFallsBackAndDegradesSingleCap(t *testing.T) {
	// Spin up an in-process mock corrald pipe that returns `busy` for agent.locate
	// and succeeds for convo.tails, verifying immediate fallback + 3-strike single-cap
	// degradation while `tails` continues using corrald.
	v1Dir, _ := buildTestCorraldDist(t)
	h := newHarness(t, func(c *config.Config) {
		c.CorraldDir = v1Dir
		c.GatewayID = "gw-busy-rpc.local"
	})
	admin := h.admin()
	srv, home, nonCorraldExecs := startCountingCorraldSSH(t)
	agents := spawnE2EFakeAgents(t, home)

	hostID, sessID := createAndPinHost(t, admin, srv, "busy-rpc-box")
	uid := int64(admin.do("GET", "/api/me", nil).json()["id"].(float64))
	sess, err := h.srv.terms.Get(uid, sessID)
	if err != nil {
		t.Fatalf("session not found: %v", err)
	}
	sess.Persistence = "herdr"
	h.srv.herdrSocks.set(sessID, agents.herdrSock)

	// Start a mock Unix socket pretending to be corrald that returns `busy` on agent.locate.
	// Use /tmp so the Unix socket path stays well below the 108-byte sun_path limit.
	sockDir, err := os.MkdirTemp("/tmp", "c-mock-*")
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

	var locateRPCs atomic.Int32
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				for {
					var req proto.Request
					if err := proto.ReadJSON(c, &req); err != nil {
						return
					}
					switch req.Method {
					case proto.MethodDaemonHello:
						res, _ := json.Marshal(proto.HelloResult{
							Version:  "0.3.1",
							Release:  1000,
							Protocol: proto.Protocol,
							Epoch:    "mock-epoch-1",
							PID:      999,
							Manifest: proto.DefaultManifest(),
							State:    proto.StateRunning,
						})
						_ = proto.WriteJSON(c, proto.Response{ID: req.ID, Result: res})
					case proto.MethodDaemonResume:
						res, _ := json.Marshal(proto.ResumeResult{Epoch: "mock-epoch-1"})
						_ = proto.WriteJSON(c, proto.Response{ID: req.ID, Result: res})
					case proto.MethodDaemonHealth:
						res, _ := json.Marshal(proto.HealthResult{
							Caps: map[string]string{"locate": "ok", "tails": "ok"},
							PID:  999,
						})
						_ = proto.WriteJSON(c, proto.Response{ID: req.ID, Result: res})
					case proto.MethodAgentLocate:
						locateRPCs.Add(1)
						_ = proto.WriteJSON(c, proto.Response{
							ID:    req.ID,
							Error: &proto.RPCError{Code: proto.ErrBusy, Message: "host busy", Retryable: true},
						})
					}
				}
			}(conn)
		}
	}()

	// Point host's corrald state to our mock socket via OnHostConnected with ProbeOverride.
	dist, _ := gw.LoadDist(v1Dir)
	h.srv.corrald.Close()
	h.srv.corrald = gw.NewManager(gw.ManagerConfig{
		Store:     h.store,
		Dist:      dist,
		GatewayID: "gw-busy-rpc.local",
		SSH:       &webSSHProvider{s: h.srv},
		ProbeOverride: func(context.Context, *ssh.Client) (*gw.HostProbe, error) {
			return &gw.HostProbe{
				OS:           "Linux",
				Machine:      "x86_64",
				Arch:         "amd64",
				Home:         home,
				HomeWritable: true,
				HasSymlink:   true,
				MachineID:    "0123456789ab",
				SocketPath:   sockPath,
				StateDir:     filepath.Join(home, ".local", "state", "corral", "corrald", "0123456789ab"),
			}, nil
		},
	})

	hk, err := h.store.HostKey(context.Background(), uid, hostID)
	if err != nil {
		t.Fatal(err)
	}
	mb, _ := proto.MarshalManifest(dist.Manifest)
	if err := h.store.UpsertCorraldHost(context.Background(), &store.CorraldHost{
		HostID:           hostID,
		State:            gw.StateRunning,
		BoundFingerprint: hk.Fingerprint,
		BoundUsername:    "alice",
		BoundGateway:     "gw-busy-rpc.local",
		ManifestJSON:     string(mb),
		Version:          "0.3.1",
		Release:          1000,
		MachineID:        "0123456789ab",
		SocketPath:       sockPath,
	}); err != nil {
		t.Fatal(err)
	}
	h.srv.corrald.OnHostConnected(context.Background(), uid, hostID, "admin", hk.Fingerprint, "alice", sess.Client())

	fullCmd := convoLocateCommand("herdr", "", "")
	fullParams := proto.AgentLocateParams{Mode: "herdr"}
	ctx := context.Background()

	// Run 4 scans: first 3 attempt corrald -> get `busy` -> fall back to SSH;
	// 4th skips corrald completely because `locate` degraded after 3 failures.
	for i := 0; i < 4; i++ {
		h.srv.convoScans.mu.Lock()
		h.srv.convoScans.m = nil
		h.srv.convoScans.mu.Unlock()
		out, _, _, _, err := h.srv.runConvoScan(ctx, sess, fullParams, fullCmd, time.Time{})
		if err != nil {
			t.Fatalf("scan %d failed: %v", i, err)
		}
		got := summarizeLocated(parseConvoLocate(string(out)), agents.herdrSock)
		if len(got) != 2 {
			t.Fatalf("scan %d returned %d agents, want 2", i, len(got))
		}
	}
	if locateRPCs.Load() != 3 {
		t.Fatalf("expected exactly 3 locate RPC attempts before single-cap degradation, got %d", locateRPCs.Load())
	}
	if nonCorraldExecs.Load() != 4 {
		t.Fatalf("expected 4 SSH fallback execs, got %d", nonCorraldExecs.Load())
	}
	if _, ok := h.srv.CorraldClient(uid, hostID, "locate"); ok {
		t.Fatal("locate should be degraded after 3 consecutive busy errors")
	}
	if _, ok := h.srv.CorraldClient(uid, hostID, "tails"); !ok {
		t.Fatal("tails should remain healthy when locate degrades")
	}
}

func TestCorraldHotUpdateNewEpochResync(t *testing.T) {
	v1Dir, v2Dir := buildTestCorraldDist(t)
	h := newHarness(t, func(c *config.Config) {
		c.CorraldDir = v1Dir
		c.GatewayID = "gw-epoch.local"
	})
	admin := h.admin()
	srv, _ := startCorraldSSH(t)

	var mu sync.Mutex
	resyncCount := 0
	dist1, err := gw.LoadDist(v1Dir)
	if err != nil {
		t.Fatal(err)
	}
	h.srv.corrald.Close()
	h.srv.corrald = gw.NewManager(gw.ManagerConfig{
		Store:             h.store,
		Dist:              dist1,
		GatewayID:         "gw-epoch.local",
		SSH:               &webSSHProvider{s: h.srv},
		BusyRetryInterval: 20 * time.Millisecond,
		BusyMaxRetries:    5,
		OnResync: func(fp, sshUser string) {
			mu.Lock()
			resyncCount++
			mu.Unlock()
		},
	})

	hostID, sessID := createAndPinHost(t, admin, srv, "epoch-box")
	uid := int64(admin.do("GET", "/api/me", nil).json()["id"].(float64))

	startCorraldInstall(t, admin, hostID)
	waitCorraldState(t, admin, hostID, "running")

	cl1, ok := h.srv.CorraldClient(uid, hostID, "locate")
	if !ok || cl1 == nil {
		t.Fatal("expected corrald client on v1")
	}
	epoch1, _ := cl1.Cursor()
	if epoch1 == "" {
		t.Fatal("expected non-empty epoch on v1")
	}

	// Switch gateway dist to v2 (same manifest -> auto hot update on next connect).
	dist2, err := gw.LoadDist(v2Dir)
	if err != nil {
		t.Fatal(err)
	}
	h.srv.CorraldManager().SetDist(dist2)

	// Close the old session and open a fresh one to trigger OnHostConnected -> auto hot update to v2.
	mustStatus(t, admin.do("DELETE", "/api/sessions/"+sessID, nil), http.StatusOK, "close first session")
	r := admin.do("POST", "/api/sessions", map[string]any{"host_id": hostID})
	mustStatus(t, r, http.StatusCreated, "open session for auto update")

	// Wait for v2 running.
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		st := admin.do("GET", fmt.Sprintf("/api/hosts/%d/corrald", hostID), nil).json()
		op, _ := st["op"].(map[string]any)
		opState, _ := op["state"].(string)
		if st["state"] == "running" && int64(st["release"].(float64)) == 2000 && opState != "running" && opState != "pending" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	st := admin.do("GET", fmt.Sprintf("/api/hosts/%d/corrald", hostID), nil).json()
	if st["state"] != "running" || int64(st["release"].(float64)) != 2000 {
		t.Fatalf("expected running release 2000 after hot update, got: %v", st)
	}

	cl2, ok := h.srv.CorraldClient(uid, hostID, "locate")
	if !ok || cl2 == nil {
		t.Fatal("expected corrald client on v2 after hot update")
	}
	epoch2, _ := cl2.Cursor()
	if epoch2 == "" || epoch2 == epoch1 {
		t.Fatalf("expected new epoch after hot update: before=%q after=%q", epoch1, epoch2)
	}

	mu.Lock()
	rc := resyncCount
	mu.Unlock()
	if rc == 0 {
		t.Fatal("expected OnResync to be invoked on new epoch after hot update")
	}

	// Verify agent.locate succeeds immediately on the updated v2 daemon.
	out, err := cl2.Locate(context.Background(), proto.AgentLocateParams{Mode: "herdr"})
	if err != nil || !strings.Contains(string(out), "hostnow=") {
		t.Fatalf("agent.locate after hot update failed: err=%v out=%q", err, string(out))
	}
}
