//go:build linux

package web

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/ihainan/corral/internal/corrald/agent"
	"github.com/ihainan/corral/internal/corrald/proto"
)

// fakeAgentHelperSrc is a small self-contained Go binary compiled once per test
// run and symlinked under different basenames (codex, opencode, agy, devin,
// 2.1.318, node, claude) so /proc/<pid>/comm and /proc/<pid>/cmdline match real
// agent processes without an interpreter wrapper.
const fakeAgentHelperSrc = `package main

import (
	"net"
	"os"
	"os/exec"
	"strings"
	"time"
)

func main() {
	var held []*os.File
	if list := os.Getenv("FAKE_OPEN_FILES"); list != "" {
		for _, p := range strings.Split(list, ":") {
			if p == "" {
				continue
			}
			f, err := os.Open(p)
			if err == nil {
				held = append(held, f)
			}
		}
	}
	if addr := os.Getenv("FAKE_TCP_LISTEN"); addr != "" {
		ln, err := net.Listen("tcp", addr)
		if err == nil {
			defer ln.Close()
			if ready := os.Getenv("FAKE_LISTEN_FILE"); ready != "" {
				_ = os.WriteFile(ready, []byte(ln.Addr().String()), 0o600)
			}
			go func() {
				for {
					c, err := ln.Accept()
					if err != nil {
						return
					}
					defer c.Close()
				}
			}()
		}
	}
	if target := os.Getenv("FAKE_TCP_CONNECT"); target != "" {
		if c, err := net.DialTimeout("tcp", target, 2*time.Second); err == nil {
			defer c.Close()
		}
	}
	if os.Getenv("FAKE_DEVIN_SPAWN_ACP") == "1" {
		c := exec.Command(os.Args[0], "acp")
		c.Env = append(os.Environ(), "FAKE_DEVIN_SPAWN_ACP=0", "FAKE_OPEN_FILES="+os.Getenv("FAKE_DEVIN_ACP_FILES"))
		_ = c.Start()
		_ = c.Wait()
		return
	}
	if ready := os.Getenv("FAKE_READY_FILE"); ready != "" {
		_ = os.WriteFile(ready, []byte("ok"), 0o600)
	}
	time.Sleep(60 * time.Second)
	for _, f := range held {
		_ = f.Close()
	}
}
`

func buildFakeAgentBinary(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	src := filepath.Join(dir, "main.go")
	if err := os.WriteFile(src, []byte(fakeAgentHelperSrc), 0o600); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "helper")
	cmd := exec.Command("go", "build", "-o", bin, src)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build fake agent helper: %v\n%s", err, out)
	}
	return bin
}

func waitForProcStart(t *testing.T, pid int) string {
	t.Helper()
	for i := 0; i < 100; i++ {
		b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
		if err == nil {
			if j := strings.LastIndex(string(b), ") "); j >= 0 {
				if f := strings.Fields(string(b)[j+2:]); len(f) > 19 {
					return f[19]
				}
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("could not read /proc/%d/stat", pid)
	return ""
}

func waitForFile(t *testing.T, path string) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(path); err == nil && len(b) > 0 {
			return string(b)
		}
		time.Sleep(15 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for ready file %s", path)
	return ""
}

func filterToTestDescendants(l convoLocated, selfPID int64, home string) convoLocated {
	out := l
	out.procs = nil
	testPIDs := map[int64]bool{}
	for _, p := range l.procs {
		isMine := false
		for _, a := range p.anc {
			if a.pid == selfPID {
				isMine = true
				break
			}
		}
		if isMine {
			out.procs = append(out.procs, p)
			testPIDs[p.pid] = true
		}
	}
	sort.Slice(out.procs, func(i, j int) bool { return out.procs[i].pid < out.procs[j].pid })

	out.cands = nil
	for _, c := range l.cands {
		if strings.HasPrefix(c.log, home+"/") {
			out.cands = append(out.cands, c)
		}
	}
	sort.Slice(out.cands, func(i, j int) bool { return out.cands[i].log < out.cands[j].log })

	if len(l.resumeOf) > 0 {
		rm := map[int64]string{}
		for pid, id := range l.resumeOf {
			if testPIDs[pid] {
				rm[pid] = id
			}
		}
		if len(rm) > 0 {
			out.resumeOf = rm
		} else {
			out.resumeOf = nil
		}
	}
	// hostNow may tick by 1 second between sh and Go invocations; check separately.
	out.hostNow = 0
	return out
}

func compareLocated(t *testing.T, label string, shRaw, goRaw []byte, selfPID int64, home string) (convoLocated, convoLocated) {
	t.Helper()
	shLoc := parseConvoLocate(string(shRaw))
	goLoc := parseConvoLocate(string(goRaw))

	if shLoc.hostNow <= 0 || goLoc.hostNow <= 0 {
		t.Fatalf("%s: missing hostNow (sh=%d, go=%d)", label, shLoc.hostNow, goLoc.hostNow)
	}
	diffNow := shLoc.hostNow - goLoc.hostNow
	if diffNow < 0 {
		diffNow = -diffNow
	}
	if diffNow > 5000 {
		t.Fatalf("%s: hostNow diverged by %dms (sh=%d, go=%d)", label, diffNow, shLoc.hostNow, goLoc.hostNow)
	}

	shF := filterToTestDescendants(shLoc, selfPID, home)
	goF := filterToTestDescendants(goLoc, selfPID, home)

	if !reflect.DeepEqual(shF, goF) {
		t.Fatalf("%s: parseConvoLocate mismatch!\n--- SH (%d procs, %d cands) ---\n%+v\n--- GO (%d procs, %d cands) ---\n%+v",
			label, len(shF.procs), len(shF.cands), shF, len(goF.procs), len(goF.cands), goF)
	}
	return shF, goF
}

// TestCorraldAgentDifferentialAgainstShellScripts launches fake processes for
// every supported agent kind (Claude Code, Codex, OpenCode, Agy, Devin) plus
// edge cases (custom CLAUDE_CONFIG_DIR, relative CLAUDE_CONFIG_DIR + FD
// disambiguation, reused PID with mismatched procStart, forged newline env,
// side-copy exclusion, Codex resume/fork/exec/app-server/subagent/config.toml,
// OpenCode 2 serve+TCP loopback, Agy lock+fresh+noninter, Devin acp+fresh+-p,
// times mode, opencode-only mode, and herdr tree fast path) and verifies that
// agent.Locator produces identical parseConvoLocate output to the original
// gateway shell scripts.
func TestCorraldAgentDifferentialAgainstShellScripts(t *testing.T) {
	if _, err := os.Stat("/proc/self/stat"); err != nil {
		t.Skip("no /proc")
	}
	shPath, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}

	helperBin := buildFakeAgentBinary(t)
	home := t.TempDir()
	binDir := filepath.Join(home, "bins")
	workDir := filepath.Join(home, "work")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Create symlinks (or copies where /proc/<pid>/exe path matters) under
	// distinct basenames so /proc/<pid>/comm matches each agent.
	makeBin := func(rel string, copyBin bool) string {
		p := filepath.Join(binDir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if copyBin {
			data, err := os.ReadFile(helperBin)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(p, data, 0o755); err != nil {
				t.Fatal(err)
			}
		} else if err := os.Symlink(helperBin, p); err != nil {
			t.Fatal(err)
		}
		return p
	}
	claudeBin := makeBin("claude", false)
	claudeVerBin := makeBin("versions/claude/2.1.318", true)
	nodeBin := makeBin("node", false)
	codexBin := makeBin("codex", false)
	ocBin := makeBin("opencode", false)
	agyBin := makeBin("agy", false)
	devinBin := makeBin("devin", false)

	spawn := func(bin, dir string, env []string, args ...string) (int, string) {
		readyFile := filepath.Join(t.TempDir(), "ready")
		c := exec.Command(bin, args...)
		c.Dir = dir
		c.Env = append([]string{"HOME=" + home, "PATH=/usr/bin:/bin", "FAKE_READY_FILE=" + readyFile}, env...)
		c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		if err := c.Start(); err != nil {
			t.Fatalf("start %s: %v", bin, err)
		}
		pid := c.Process.Pid
		t.Cleanup(func() {
			_ = syscall.Kill(-pid, syscall.SIGKILL)
			_ = c.Wait()
		})
		waitForFile(t, readyFile)
		return pid, waitForProcStart(t, pid)
	}

	// --- 1. Claude Code scenarios ---
	defaultClaudeSess := filepath.Join(home, ".claude", "sessions")
	defaultClaudeProj := filepath.Join(home, ".claude", "projects", "-work")
	customAbsClaude := filepath.Join(home, "my cfg", ".claude-abs")
	customRelClaude := filepath.Join(workDir, ".claude-rel")
	for _, d := range []string{
		defaultClaudeSess,
		defaultClaudeProj,
		filepath.Join(customAbsClaude, "sessions"),
		filepath.Join(customAbsClaude, "projects", "-abs"),
		filepath.Join(customRelClaude, "sessions"),
		filepath.Join(customRelClaude, "projects", "-p1"),
		filepath.Join(customRelClaude, "projects", "-p2"),
		filepath.Join(workDir, ".claude"),
	} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	// Claude settings.json exercising ep, auth, and ac (modelSettings + global + pct).
	_ = os.WriteFile(filepath.Join(home, ".claude", "settings.json"), []byte(`{
		"env": {
			"ANTHROPIC_BASE_URL": "https://user:secret@proxy.example.com:8443/v1?key=abc",
			"ANTHROPIC_AUTH_TOKEN": "tok-should-not-leak",
			"CLAUDE_AUTOCOMPACT_PCT_OVERRIDE": "85"
		},
		"autoCompactWindow": 200000,
		"modelSettings": {
			"Claude-Sonnet-4": {"autoCompactWindow": 150000},
			"claude-opus-4": {"autoCompactWindow": 180000}
		}
	}`), 0o600)

	const (
		clSID1 = "11111111-1111-4111-8111-111111111111"
		clSID2 = "22222222-2222-4222-8222-222222222222"
		clSID3 = "33333333-3333-4333-8333-333333333333"
		clSID4 = "44444444-4444-4444-8444-444444444444"
		clSID5 = "55555555-5555-4555-8555-555555555555"
		clSID6 = "66666666-6666-4666-8666-666666666666"
	)

	// Claude 1: default dir, herdr pane, JSONL with timestamp for times mode.
	clLog1 := filepath.Join(defaultClaudeProj, clSID1+".jsonl")
	_ = os.WriteFile(clLog1, []byte(`{"type":"user","timestamp":"2026-11-20T10:00:00.5Z"}`+"\n"+`{"type":"assistant","created_at":"2026-11-20T10:05:00.123456Z"}`+"\n"), 0o600)
	cl1PID, cl1Start := spawn(claudeBin, workDir, []string{
		"HERDR_PANE_ID=w1:p1",
		"HERDR_SOCKET_PATH=/tmp/fake-herdr.sock",
	})
	_ = os.WriteFile(filepath.Join(defaultClaudeSess, strconv.Itoa(cl1PID)+".json"),
		[]byte(fmt.Sprintf(`{"pid":%d,"sessionId":"%s","procStart":"%s"}`, cl1PID, clSID1, cl1Start)), 0o600)

	// Claude 2: custom absolute CLAUDE_CONFIG_DIR with spaces, TMUX env, Bedrock override.
	clLog2 := filepath.Join(customAbsClaude, "projects", "-abs", clSID2+".jsonl")
	_ = os.WriteFile(clLog2, []byte(`{"type":"user","timestamp":"2026-11-21T08:00:00Z"}`+"\n"), 0o600)
	cl2PID, cl2Start := spawn(claudeVerBin, workDir, []string{
		"CLAUDE_CONFIG_DIR=" + customAbsClaude,
		"TMUX=/tmp/tmux-1000/default,12345,0",
		"TMUX_PANE=%4",
		"CLAUDE_CODE_USE_BEDROCK=1",
		"ANTHROPIC_BASE_URL=https://should-be-cleared-by-bedrock.example",
	})
	_ = os.WriteFile(filepath.Join(customAbsClaude, "sessions", strconv.Itoa(cl2PID)+".json"),
		[]byte(fmt.Sprintf(`{"pid":%d,"sessionId":"%s","procStart":%s}`, cl2PID, clSID2, cl2Start)), 0o600)

	// Claude 3: relative CLAUDE_CONFIG_DIR + duplicate project logs disambiguated by open FD!
	clLog3A := filepath.Join(customRelClaude, "projects", "-p1", clSID3+".jsonl")
	clLog3B := filepath.Join(customRelClaude, "projects", "-p2", clSID3+".jsonl")
	_ = os.WriteFile(clLog3A, []byte(`{"type":"user"}`+"\n"), 0o600)
	_ = os.WriteFile(clLog3B, []byte(`{"type":"user"}`+"\n"), 0o600)
	cl3PID, cl3Start := spawn(nodeBin, workDir, []string{
		"CLAUDE_CONFIG_DIR=.claude-rel",
		"FAKE_OPEN_FILES=" + clLog3B,
		"OTHER=x\nHERDR_PANE_ID=w9:p9\nTMUX_PANE=%99", // forged newline env must NOT take effect
		"ANTHROPIC_API_KEY=sk-ant-fake",
	}, "/usr/local/bin/claude")
	_ = os.WriteFile(filepath.Join(customRelClaude, "sessions", strconv.Itoa(cl3PID)+".json"),
		[]byte(fmt.Sprintf(`{"pid":%d,"sessionId":"%s","procStart":"%s"}`, cl3PID, clSID3, cl3Start)), 0o600)

	// Claude 4: reused PID (procStart mismatch) -> MUST NOT appear in either sh or Go!
	cl4PID, _ := spawn(claudeBin, workDir, []string{"HERDR_PANE_ID=w1:p4"})
	_ = os.WriteFile(filepath.Join(defaultClaudeSess, strconv.Itoa(cl4PID)+".json"),
		[]byte(fmt.Sprintf(`{"pid":%d,"sessionId":"%s","procStart":"999999999"}`, cl4PID, clSID4)), 0o600)

	// Claude 5: side-question copy (entrypoint=sdk-cli + CORRAL_SIDE_COPY=1) -> MUST be excluded!
	cl5PID, cl5Start := spawn(claudeBin, workDir, []string{"CORRAL_SIDE_COPY=1", "HERDR_PANE_ID=w1:p5"})
	_ = os.WriteFile(filepath.Join(defaultClaudeSess, strconv.Itoa(cl5PID)+".json"),
		[]byte(fmt.Sprintf(`{"pid":%d,"sessionId":"%s","procStart":"%s","entrypoint":"sdk-cli"}`, cl5PID, clSID5, cl5Start)), 0o600)

	// --- 2. Codex scenarios ---
	cxHome := filepath.Join(home, ".codex")
	cxSessDir := filepath.Join(cxHome, "sessions", "2026", "11", "27")
	cxLockDir := filepath.Join(cxHome, "thread-writer-locks")
	for _, d := range []string{cxSessDir, cxLockDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	_ = os.WriteFile(filepath.Join(cxHome, "config.toml"), []byte(`
profile = "work"

[profiles.work]
model_provider = "corp"

[model_providers.corp]
base_url = "https://codex-gw.example.net/v1"
`), 0o600)

	const (
		cxSID1    = "aaaa1111-1111-4111-8111-111111111111"
		cxSIDFork = "bbbb2222-2222-4222-8222-222222222222"
		cxSIDCand = "cccc3333-3333-4333-8333-333333333333"
		cxSIDSub  = "dddd4444-4444-4444-8444-444444444444"
		cxSIDRes  = "eeee5555-5555-4555-8555-555555555555"
	)
	cxLog1 := filepath.Join(cxSessDir, "rollout-2026-11-27T01-00-00-"+cxSID1+".jsonl")
	_ = os.WriteFile(cxLog1, []byte(`{"type":"session_meta","payload":{"cwd":"/work/a","timestamp":"2026-11-27T01:00:00Z"}}`+"\n"), 0o600)

	cxCandLog := filepath.Join(cxSessDir, "rollout-2026-11-27T02-00-00-"+cxSIDCand+".jsonl")
	_ = os.WriteFile(cxCandLog, []byte(`{"type":"session_meta","payload":{"cwd":"`+workDir+`","timestamp":"2026-11-27T02:00:00Z","forked_from_id":"`+cxSID1+`"}}`+"\n"), 0o600)

	cxSubLog := filepath.Join(cxSessDir, "rollout-2026-11-27T02-05-00-"+cxSIDSub+".jsonl")
	_ = os.WriteFile(cxSubLog, []byte(`{"type":"session_meta","payload":{"thread_source":"subagent","cwd":"`+workDir+`"}}`+"\n"), 0o600)

	cxForkLock := filepath.Join(cxLockDir, cxSIDFork+".lock")
	_ = os.WriteFile(cxForkLock, nil, 0o600)
	oldTime := time.Now().Add(-10 * time.Second)
	_ = os.Chtimes(cxForkLock, oldTime, oldTime)

	// Codex 1: interactive with open rollout + profile from config.toml.
	cx1PID, _ := spawn(codexBin, workDir, []string{"FAKE_OPEN_FILES=" + cxLog1, "HERDR_PANE_ID=w2:p1"})
	// Codex 2: `codex resume <uuid>` with no open rollout yet (fresh/zero sid + resumeof=).
	cx2PID, _ := spawn(codexBin, workDir, []string{"HERDR_PANE_ID=w2:p2"}, "resume", cxSIDRes)
	// Codex 3: `codex fork <uuid>` holding aged writer lock -> pending sid.
	cx3PID, _ := spawn(codexBin, workDir, []string{"FAKE_OPEN_FILES=" + cxForkLock, "HERDR_PANE_ID=w2:p3"}, "fork", cxSID1)
	// Codex 4: `codex -p exec` (option value is "exec", NOT subcommand) with custom CODEX_HOME + OPENAI_BASE_URL -> MUST be included!
	customCxHome := filepath.Join(home, ".codex-alt")
	_ = os.MkdirAll(customCxHome, 0o755)
	cx4PID, _ := spawn(codexBin, workDir, []string{"CODEX_HOME=" + customCxHome, "OPENAI_BASE_URL=https://oa.example.org/v1", "HERDR_PANE_ID=w2:p4"}, "-p", "exec")
	// Codex 5: `codex exec "do something"` -> MUST be excluded!
	cxExecPID, _ := spawn(codexBin, workDir, []string{"HERDR_PANE_ID=w2:p5"}, "exec", "do something")
	// Codex 6: `codex app-server` holding main cand + subagent rollout.
	_, _ = spawn(codexBin, workDir, []string{"FAKE_OPEN_FILES=" + cxCandLog + ":" + cxSubLog}, "app-server")

	// --- 3. OpenCode scenarios (direct + OpenCode 2 serve via loopback TCP) ---
	ocDir := filepath.Join(home, ".local", "share", "opencode")
	if err := os.MkdirAll(ocDir, 0o755); err != nil {
		t.Fatal(err)
	}
	ocDB := filepath.Join(ocDir, "opencode.db")
	_ = os.WriteFile(ocDB, []byte("sqlite"), 0o600)

	// Direct interactive opencode holding opencode.db.
	oc1PID, _ := spawn(ocBin, workDir, []string{"FAKE_OPEN_FILES=" + ocDB, "HERDR_PANE_ID=w3:p1"})
	// Non-interactive `opencode run` -> excluded.
	ocRunPID, _ := spawn(ocBin, workDir, []string{"FAKE_OPEN_FILES=" + ocDB, "HERDR_PANE_ID=w3:p2"}, "run", "hello")

	// OpenCode 2: `opencode serve` listening on 127.0.0.1:0 holding opencode.db,
	// plus an interactive `opencode` connected to it on loopback!
	listenAddrFile := filepath.Join(t.TempDir(), "oc-addr")
	_, _ = spawn(ocBin, workDir, []string{
		"FAKE_OPEN_FILES=" + ocDB,
		"FAKE_TCP_LISTEN=127.0.0.1:0",
		"FAKE_LISTEN_FILE=" + listenAddrFile,
	}, "serve", "--service")
	ocServeAddr := strings.TrimSpace(waitForFile(t, listenAddrFile))
	if _, _, err := net.SplitHostPort(ocServeAddr); err != nil {
		t.Fatalf("bad oc serve addr %q: %v", ocServeAddr, err)
	}
	oc2PID, _ := spawn(ocBin, workDir, []string{
		"FAKE_TCP_CONNECT=" + ocServeAddr,
		"HERDR_PANE_ID=w3:p3",
	})

	// --- 4. Agy scenarios ---
	const agySID = "77777777-7777-4777-8777-777777777777"
	agyBase := filepath.Join(home, ".gemini", "antigravity-cli")
	agyLock := filepath.Join(agyBase, "presence", agySID+".lock")
	agyLog := filepath.Join(agyBase, "brain", agySID, ".system_generated", "logs", "transcript_full.jsonl")
	_ = os.MkdirAll(filepath.Dir(agyLock), 0o755)
	_ = os.MkdirAll(filepath.Dir(agyLog), 0o755)
	_ = os.WriteFile(agyLock, nil, 0o600)
	_ = os.WriteFile(agyLog, []byte(`{"timestamp":"2026-11-25T12:00:00Z"}`+"\n"), 0o600)

	agy1PID, _ := spawn(agyBin, workDir, []string{"FAKE_OPEN_FILES=" + agyLock, "HERDR_PANE_ID=w4:p1"})
	agyFreshPID, _ := spawn(agyBin, workDir, []string{"HERDR_PANE_ID=w4:p2"})
	agyPrintPID, _ := spawn(agyBin, workDir, []string{"FAKE_OPEN_FILES=" + agyLock, "HERDR_PANE_ID=w4:p3"}, "-p", "hi")
	agyModelsPID, _ := spawn(agyBin, workDir, []string{"HERDR_PANE_ID=w4:p4"}, "models")

	// --- 5. Devin scenarios ---
	dvData := filepath.Join(home, ".local", "share", "devin", "cli")
	dvLock := filepath.Join(dvData, "session_locks", "cuddly-dime.lock")
	dvDB := filepath.Join(dvData, "sessions.db")
	_ = os.MkdirAll(filepath.Dir(dvLock), 0o755)
	_ = os.WriteFile(dvLock, nil, 0o600)
	_ = os.WriteFile(dvDB, []byte("sqlite"), 0o600)

	dv1PID, _ := spawn(devinBin, workDir, []string{
		"FAKE_DEVIN_SPAWN_ACP=1",
		"FAKE_DEVIN_ACP_FILES=" + dvLock + ":" + dvDB,
		"HERDR_PANE_ID=w5:p1",
	}, "--model", "swe-1", "-r", "cuddly-dime")
	dvFreshPID, _ := spawn(devinBin, workDir, []string{"HERDR_PANE_ID=w5:p2"})
	dvPrintPID, _ := spawn(devinBin, workDir, []string{"HERDR_PANE_ID=w5:p3"}, "-p", "hi")
	// Give devin acp child a moment to open its lock & db.
	time.Sleep(150 * time.Millisecond)

	// --- Run side-by-side comparisons across all modes! ---
	emptyStr := ""
	loc, err := agent.NewLocator(agent.LocatorOptions{
		Home:            home,
		ClaudeConfigDir: &emptyStr,
		XDGDataHome:     &emptyStr,
	})
	if err != nil {
		t.Fatal(err)
	}
	runSh := func(cmdStr string) []byte {
		t.Helper()
		c := exec.Command(shPath, "-c", cmdStr)
		c.Env = []string{"HOME=" + home, "PATH=/usr/bin:/bin"}
		out, err := c.Output()
		if err != nil {
			t.Fatalf("sh script failed: %v", err)
		}
		return out
	}

	selfPID := int64(os.Getpid())

	// Mode 1: Full locate (convoLocateCommand).
	shFull := runSh(convoLocateCommand("herdr", "", ""))
	goFull := loc.Locate(context.Background(), proto.AgentLocateParams{Mode: "herdr"})
	_, gotFull := compareLocated(t, "full-herdr", shFull, goFull, selfPID, home)

	// Sanity assertions so a vacuously-empty bug cannot pass.
	byPID := map[int64]convoProc{}
	for _, p := range gotFull.procs {
		byPID[p.pid] = p
	}
	if p, ok := byPID[int64(cl1PID)]; !ok || p.sid != clSID1 || p.log != clLog1 || p.ep != "proxy.example.com:8443" || p.auth != "token" || !strings.Contains(p.ac, "global=200000") || !strings.Contains(p.ac, "pct=85") {
		t.Fatalf("claude1 mismatch: ok=%v %+v", ok, p)
	}
	if p, ok := byPID[int64(cl2PID)]; !ok || p.sid != clSID2 || p.log != clLog2 || p.auth != "bedrock" || p.ep != "" || p.tmuxPane != "%4" {
		t.Fatalf("claude2 mismatch: ok=%v %+v", ok, p)
	}
	if p, ok := byPID[int64(cl3PID)]; !ok || p.sid != clSID3 || p.log != clLog3B || p.herdrPane != "" || p.tmuxPane != "" || p.auth != "key" {
		t.Fatalf("claude3 mismatch: ok=%v %+v", ok, p)
	}
	if _, ok := byPID[int64(cl4PID)]; ok {
		t.Fatal("stale reused PID cl4 unexpectedly located")
	}
	if _, ok := byPID[int64(cl5PID)]; ok {
		t.Fatal("side-copy cl5 unexpectedly located")
	}
	if p, ok := byPID[int64(cx1PID)]; !ok || p.sid != cxSID1 || p.ep != "codex-gw.example.net" {
		t.Fatalf("codex1 mismatch: ok=%v %+v", ok, p)
	}
	if gotFull.resumeOf[int64(cx2PID)] != cxSIDRes {
		t.Fatalf("codex2 resumeOf = %q, want %q", gotFull.resumeOf[int64(cx2PID)], cxSIDRes)
	}
	if p, ok := byPID[int64(cx3PID)]; !ok || p.sid != cxSIDFork || p.log != "" {
		t.Fatalf("codex3 fork lock mismatch: ok=%v %+v", ok, p)
	}
	if p, ok := byPID[int64(cx4PID)]; !ok || p.ep != "oa.example.org" {
		t.Fatalf("codex4 (-p exec) mismatch: ok=%v %+v", ok, p)
	}
	if _, ok := byPID[int64(cxExecPID)]; ok {
		t.Fatal("codex exec unexpectedly located")
	}
	if len(gotFull.cands) != 1 || gotFull.cands[0].log != cxCandLog || gotFull.cands[0].forkedFrom != cxSID1 {
		t.Fatalf("codex app-server cands mismatch: %+v", gotFull.cands)
	}
	if p, ok := byPID[int64(oc1PID)]; !ok || p.log != ocDB || p.ocService {
		t.Fatalf("opencode1 mismatch: ok=%v %+v", ok, p)
	}
	if _, ok := byPID[int64(ocRunPID)]; ok {
		t.Fatal("opencode run unexpectedly located")
	}
	if p, ok := byPID[int64(oc2PID)]; !ok || p.log != ocDB || !p.ocService {
		t.Fatalf("opencode2 service mismatch: ok=%v %+v", ok, p)
	}
	if p, ok := byPID[int64(agy1PID)]; !ok || p.sid != agySID || p.log != agyLog {
		t.Fatalf("agy1 mismatch: ok=%v %+v", ok, p)
	}
	if p, ok := byPID[int64(agyFreshPID)]; !ok || p.sid != convoNoSession || p.log != "" {
		t.Fatalf("agyFresh mismatch: ok=%v %+v", ok, p)
	}
	if _, ok := byPID[int64(agyPrintPID)]; ok {
		t.Fatal("agy -p unexpectedly located")
	}
	if _, ok := byPID[int64(agyModelsPID)]; ok {
		t.Fatal("agy models unexpectedly located")
	}
	if p, ok := byPID[int64(dv1PID)]; !ok || p.sid != "cuddly-dime" || p.log != dvDB {
		t.Fatalf("devin1 mismatch: ok=%v %+v", ok, p)
	}
	if p, ok := byPID[int64(dvFreshPID)]; !ok || p.sid != convoNoSession || p.log != dvDB {
		t.Fatalf("devinFresh mismatch: ok=%v %+v", ok, p)
	}
	if _, ok := byPID[int64(dvPrintPID)]; ok {
		t.Fatal("devin -p unexpectedly located")
	}

	// Mode 2: OpenCode-only scan (convoLocateOpenCodeCommand).
	shOC := runSh(convoLocateOpenCodeCommand("herdr", ""))
	goOC := loc.Locate(context.Background(), proto.AgentLocateParams{Mode: "herdr", Only: "opencode"})
	_, gotOC := compareLocated(t, "opencode-only", shOC, goOC, selfPID, home)
	if len(gotOC.procs) != 2 {
		t.Fatalf("opencode-only expected 2 procs, got %d: %+v", len(gotOC.procs), gotOC.procs)
	}

	// Mode 3: Times mode (convoLocateTimesCommand).
	shTimes := runSh(convoLocateTimesCommand("herdr", ""))
	goTimes := loc.Locate(context.Background(), proto.AgentLocateParams{Mode: "herdr", Times: true})
	_, gotTimes := compareLocated(t, "times", shTimes, goTimes, selfPID, home)
	for _, p := range gotTimes.procs {
		if p.pid == int64(cl1PID) {
			if !p.wroteTold || p.wrote <= 0 || p.ep != "" || p.auth != "" || p.ac != "" {
				t.Fatalf("times mode cl1 unexpected fields: %+v", p)
			}
		}
	}

	// Mode 4: Herdr tree fast path (convoLocatePidsCommand).
	shellPID, treePIDs, treeStarts := spawnFakeHerdrTree(t, workDir, home, [][]string{
		{"HERDR_PANE_ID=w9:p1", "HERDR_SOCKET_PATH=/tmp/herdr.sock"},
		{"HERDR_PANE_ID=w9:p1", "HERDR_SOCKET_PATH=/tmp/herdr.sock"},
	})
	_ = os.WriteFile(filepath.Join(defaultClaudeSess, strconv.Itoa(treePIDs[0])+".json"),
		[]byte(fmt.Sprintf(`{"pid":%d,"sessionId":"%s","procStart":"%s"}`, treePIDs[0], clSID6, treeStarts[0])), 0o600)

	// 4a. Single candidate hit.
	shTree1 := runSh(convoLocatePidsCommand(shellPID, []int{treePIDs[0]}))
	goTree1 := loc.Locate(context.Background(), proto.AgentLocateParams{Mode: "herdr", ShellPID: shellPID, PIDs: []int{treePIDs[0]}})
	_, gotTree1 := compareLocated(t, "tree-single", shTree1, goTree1, selfPID, home)
	if gotTree1.fastIncomplete || len(gotTree1.procs) != 1 || gotTree1.procs[0].pid != int64(treePIDs[0]) {
		t.Fatalf("tree-single unexpected: %+v", gotTree1)
	}

	// 4b. Two sibling candidates -> fastIncomplete=true.
	_ = os.WriteFile(filepath.Join(defaultClaudeSess, strconv.Itoa(treePIDs[1])+".json"),
		[]byte(fmt.Sprintf(`{"pid":%d,"sessionId":"%s","procStart":"%s"}`, treePIDs[1], clSID6, treeStarts[1])), 0o600)
	shTree2 := runSh(convoLocatePidsCommand(shellPID, []int{treePIDs[0]}))
	goTree2 := loc.Locate(context.Background(), proto.AgentLocateParams{Mode: "herdr", ShellPID: shellPID, PIDs: []int{treePIDs[0]}})
	_, gotTree2 := compareLocated(t, "tree-two-siblings", shTree2, goTree2, selfPID, home)
	if !gotTree2.fastIncomplete {
		t.Fatalf("tree-two-siblings expected fastIncomplete=true")
	}

	// 4c. Stale fgPID outside subtree -> fastIncomplete=true.
	shTree3 := runSh(convoLocatePidsCommand(shellPID, []int{9999999}))
	goTree3 := loc.Locate(context.Background(), proto.AgentLocateParams{Mode: "herdr", ShellPID: shellPID, PIDs: []int{9999999}})
	_, gotTree3 := compareLocated(t, "tree-stale-fg", shTree3, goTree3, selfPID, home)
	if !gotTree3.fastIncomplete {
		t.Fatalf("tree-stale-fg expected fastIncomplete=true")
	}

	// 4d. Non-herdr root -> fastIncomplete=true.
	shTree4 := runSh(convoLocatePidsCommand(os.Getpid(), []int{os.Getpid()}))
	goTree4 := loc.Locate(context.Background(), proto.AgentLocateParams{Mode: "herdr", ShellPID: os.Getpid(), PIDs: []int{os.Getpid()}})
	_, gotTree4 := compareLocated(t, "tree-non-herdr", shTree4, goTree4, selfPID, home)
	if !gotTree4.fastIncomplete {
		t.Fatalf("tree-non-herdr expected fastIncomplete=true")
	}
}

// TestCorraldTailsByteForByteAgainstShellScripts compares agent.Tails and
// agent.MessageTails byte-for-byte against convoTailsCommand and
// convoMessageTailsCommand across diverse edge cases.
func TestCorraldTailsByteForByteAgainstShellScripts(t *testing.T) {
	shPath, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	home := t.TempDir()
	projDir := filepath.Join(home, ".claude", "projects", "-tails")
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// 1. Small file without trailing newline.
	fSmall := filepath.Join(projDir, "small.jsonl")
	_ = os.WriteFile(fSmall, []byte(`{"type":"message","text":"hello world"}`), 0o600)

	// 2. Empty file.
	fEmpty := filepath.Join(projDir, "empty.jsonl")
	_ = os.WriteFile(fEmpty, nil, 0o600)

	// 3. Large UTF-8 file (>16 KiB) where 16384 byte boundary cuts mid-rune.
	fLargeUTF8 := filepath.Join(projDir, "large-utf8.jsonl")
	_ = os.WriteFile(fLargeUTF8, []byte("x"+strings.Repeat(`{"type":"agent_message","text":"中文字段测试内容"}`+"\n", 500)), 0o600)

	// 4. Busy Codex log with >4 MiB tool output, >65536-char line, and valid messages.
	fBusy := filepath.Join(projDir, "busy.jsonl")
	var busyBuf bytes.Buffer
	busyBuf.WriteString(cxRow(t, "event_msg", map[string]any{"type": "task_started"}))
	busyBuf.WriteString(cxSaid(t, "m1", "早期消息会被 4 MiB 窗口截掉"))
	// Over 4 MiB of tool output before the last 4 MiB window.
	for i := 0; i < 40; i++ {
		busyBuf.WriteString(cxRow(t, "response_item", map[string]any{"type": "function_call_output", "output": strings.Repeat("A", 120000)}))
	}
	busyBuf.WriteString(cxSaid(t, "m2", "窗口内的保留消息：原工作区和 tabs 会保留"))
	// A >65536 char matching line that must be skipped by length($0) <= 65536.
	busyBuf.WriteString(`{"type":"message","text":"` + strings.Repeat("Z", 70000) + `"}` + "\n")
	busyBuf.WriteString(cxRow(t, "response_item", map[string]any{"type": "function_call", "name": "exec_command", "arguments": `{"cmd":"ls"}`}))
	busyBuf.WriteString(cxRow(t, "event_msg", map[string]any{"type": "task_complete"}))
	_ = os.WriteFile(fBusy, busyBuf.Bytes(), 0o600)

	// 5. File with >64 KiB of matching messages (triggers final `tail -c 65536`).
	fManyMsgs := filepath.Join(projDir, "many-msgs.jsonl")
	var manyBuf bytes.Buffer
	for i := 0; i < 1200; i++ {
		fmt.Fprintf(&manyBuf, `{"type":"user_message","n":%d,"text":"message payload number %06d with padding text"}`+"\n", i, i)
	}
	_ = os.WriteFile(fManyMsgs, manyBuf.Bytes(), 0o600)

	// 6. File containing \x01 error marker byte.
	fBad := filepath.Join(projDir, "bad-soh.jsonl")
	_ = os.WriteFile(fBad, []byte(`{"type":"message","text":"good"}`+"\n"+`{"type":"message","text":"bad`+"\x01"+`"}`+"\n"), 0o600)

	// 7. Missing file (both sh and Go must skip).
	fMissing := filepath.Join(projDir, "missing.jsonl")

	logs := []string{fSmall, fEmpty, fLargeUTF8, fBusy, fManyMsgs, fBad, fMissing}

	val := agent.NewPathValidator(home, uint32(os.Geteuid()), "/proc")

	runSh := func(cmdStr string) []byte {
		t.Helper()
		out, err := exec.Command(shPath, "-c", cmdStr).Output()
		if err != nil {
			t.Fatalf("sh failed: %v", err)
		}
		return out
	}

	shTails := runSh(convoTailsCommand(logs))
	goTails := agent.Tails(val, logs)
	if !bytes.Equal(shTails, goTails) {
		t.Fatalf("convo.tails byte mismatch: sh=%d bytes, go=%d bytes", len(shTails), len(goTails))
	}

	shMsgTails := runSh(convoMessageTailsCommand(logs))
	goMsgTails := agent.MessageTails(val, logs)
	if !bytes.Equal(shMsgTails, goMsgTails) {
		t.Fatalf("convo.message_tails byte mismatch: sh=%d bytes, go=%d bytes", len(shMsgTails), len(goMsgTails))
	}
}

// TestCorraldLocatePerformance50Agents spawns 50 fake Claude Code processes and
// reports the wall-clock latency of the original shell script vs Go Locator.
func TestCorraldLocatePerformance50Agents(t *testing.T) {
	if _, err := os.Stat("/proc/self/stat"); err != nil {
		t.Skip("no /proc")
	}
	shPath, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	sleepPath, err := exec.LookPath("sleep")
	if err != nil {
		t.Skip("no sleep")
	}

	home := t.TempDir()
	sessDir := filepath.Join(home, ".claude", "sessions")
	projDir := filepath.Join(home, ".claude", "projects", "-perf")
	for _, d := range []string{sessDir, projDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	_ = os.WriteFile(filepath.Join(home, ".claude", "settings.json"), []byte(`{"env":{"ANTHROPIC_BASE_URL":"https://api.example.test/v1"},"autoCompactWindow":200000}`), 0o600)

	const nProcs = 50
	cmds := make([]*exec.Cmd, 0, nProcs)
	t.Cleanup(func() {
		for _, c := range cmds {
			if c.Process != nil {
				_ = c.Process.Kill()
				_ = c.Wait()
			}
		}
	})

	for i := 0; i < nProcs; i++ {
		c := exec.Command(sleepPath, "60")
		c.Env = []string{
			"HOME=" + home,
			"PATH=/usr/bin:/bin",
			fmt.Sprintf("HERDR_PANE_ID=w1:p%d", i+1),
			"HERDR_SOCKET_PATH=/tmp/perf-herdr.sock",
		}
		if err := c.Start(); err != nil {
			t.Fatalf("start sleep %d: %v", i, err)
		}
		cmds = append(cmds, c)
	}
	for i, c := range cmds {
		pid := c.Process.Pid
		st := waitForProcStart(t, pid)
		sid := fmt.Sprintf("99999999-9999-4999-8999-%012d", i+1)
		_ = os.WriteFile(filepath.Join(projDir, sid+".jsonl"), []byte(`{"type":"user","timestamp":"2026-11-27T10:00:00Z"}`+"\n"), 0o600)
		_ = os.WriteFile(filepath.Join(sessDir, strconv.Itoa(pid)+".json"),
			[]byte(fmt.Sprintf(`{"pid":%d,"sessionId":"%s","procStart":"%s"}`, pid, sid, st)), 0o600)
	}

	emptyStr := ""
	loc, err := agent.NewLocator(agent.LocatorOptions{
		Home:            home,
		ClaudeConfigDir: &emptyStr,
		XDGDataHome:     &emptyStr,
	})
	if err != nil {
		t.Fatal(err)
	}

	shStart := time.Now()
	shCmd := exec.Command(shPath, "-c", convoLocateCommand("herdr", "", ""))
	shCmd.Env = []string{"HOME=" + home, "PATH=/usr/bin:/bin"}
	shOut, err := shCmd.Output()
	if err != nil {
		t.Fatalf("sh locate 50 agents failed: %v", err)
	}
	shElapsed := time.Since(shStart)

	goStart := time.Now()
	goOut := loc.Locate(context.Background(), proto.AgentLocateParams{Mode: "herdr"})
	goElapsed := time.Since(goStart)

	selfPID := int64(os.Getpid())
	shLoc, goLoc := compareLocated(t, "perf-50-agents", shOut, goOut, selfPID, home)
	wantPIDs := make(map[int64]bool, nProcs)
	for _, c := range cmds {
		wantPIDs[int64(c.Process.Pid)] = true
	}
	foundSh, foundGo := 0, 0
	for _, p := range shLoc.procs {
		if wantPIDs[p.pid] {
			foundSh++
		}
	}
	for _, p := range goLoc.procs {
		if wantPIDs[p.pid] {
			foundGo++
		}
	}
	if foundSh != nProcs || foundGo != nProcs {
		t.Fatalf("expected %d spawned procs, got sh=%d go=%d", nProcs, foundSh, foundGo)
	}

	speedup := float64(shElapsed) / float64(goElapsed)
	t.Logf("50 fake Claude agents locate latency: sh=%v, go=%v (speedup=%.1fx)",
		shElapsed.Round(time.Millisecond), goElapsed.Round(time.Microsecond), speedup)
}
