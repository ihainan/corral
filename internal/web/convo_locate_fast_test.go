package web

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"

	"github.com/ihainan/corral/internal/herdr"
	"github.com/ihainan/corral/internal/sftplite"
	"github.com/ihainan/corral/internal/sshx"
)

func TestPlanHerdrFastLocate(t *testing.T) {
	hpClaude := herdr.Pane{ID: "w1:p1", Agent: "claude"}
	hpCodex := herdr.Pane{ID: "w1:p1", Agent: "codex"}
	hpOC := herdr.Pane{ID: "w1:p1", Agent: "opencode"}

	// Nil result (e.g. older herdr without pane.process_info) falls back to full scan,
	// or OpenCode-only when snapshot says OpenCode.
	if got := planHerdrFastLocate(nil, "w1:p1", hpClaude, true); got.mode != herdrFastFull {
		t.Fatalf("nil pi for claude: got %+v, want full", got)
	}
	if got := planHerdrFastLocate(nil, "w1:p1", hpOC, true); got.mode != herdrFastOpenCode {
		t.Fatalf("nil pi for opencode: got %+v, want opencode", got)
	}

	// Codex never uses the subtree fast path (snapshot or process entry).
	codexPI := &herdr.PaneProcessInfoResult{
		PaneID:              "w1:p1",
		ShellPID:            50,
		ForegroundProcesses: []herdr.ProcessEntry{{PID: 100, Name: "codex"}},
	}
	if got := planHerdrFastLocate(codexPI, "w1:p1", hpCodex, true); got.mode != herdrFastFull {
		t.Fatalf("codex snapshot: got %+v, want full", got)
	}
	if got := planHerdrFastLocate(codexPI, "w1:p1", herdr.Pane{}, false); got.mode != herdrFastFull {
		t.Fatalf("codex proc: got %+v, want full", got)
	}

	// Missing shell_pid falls back because the pane's process tree cannot be verified.
	noShell := &herdr.PaneProcessInfoResult{
		PaneID:              "w1:p1",
		ShellPID:            0,
		ForegroundProcesses: []herdr.ProcessEntry{{PID: 100, Name: "claude"}},
	}
	if got := planHerdrFastLocate(noShell, "w1:p1", hpClaude, true); got.mode != herdrFastFull {
		t.Fatalf("missing shell_pid: got %+v, want full", got)
	}

	// Mismatched pane_id falls back.
	mismatch := &herdr.PaneProcessInfoResult{
		PaneID:              "w1:p9",
		ShellPID:            50,
		ForegroundProcesses: []herdr.ProcessEntry{{PID: 100, Name: "claude"}},
	}
	if got := planHerdrFastLocate(mismatch, "w1:p1", hpClaude, true); got.mode != herdrFastFull {
		t.Fatalf("mismatched pane_id: got %+v, want full", got)
	}

	// Empty foreground processes falls back.
	empty := &herdr.PaneProcessInfoResult{PaneID: "w1:p1", ShellPID: 50}
	if got := planHerdrFastLocate(empty, "w1:p1", hpClaude, true); got.mode != herdrFastFull {
		t.Fatalf("empty fg: got %+v, want full", got)
	}

	// Shell-only foreground falls back (could have a suspended/backgrounded agent).
	shellOnly := &herdr.PaneProcessInfoResult{
		PaneID:              "w1:p1",
		ShellPID:            50,
		ForegroundProcesses: []herdr.ProcessEntry{{PID: 50, Name: "zsh"}},
	}
	if got := planHerdrFastLocate(shellOnly, "w1:p1", hpClaude, true); got.mode != herdrFastFull {
		t.Fatalf("shell-only fg: got %+v, want full", got)
	}

	// Agent replacing the shell via `exec claude` (PID == ShellPID, Name == claude) must still use fast tree scan.
	execClaude := &herdr.PaneProcessInfoResult{
		PaneID:              "w1:p1",
		ShellPID:            50,
		ForegroundProcesses: []herdr.ProcessEntry{{PID: 50, Name: "claude"}},
	}
	if got := planHerdrFastLocate(execClaude, "w1:p1", hpClaude, true); got.mode != herdrFastPIDs || got.shellPID != 50 || !slices.Equal(got.pids, []int{50}) {
		t.Fatalf("exec claude: got %+v, want shellPID=50 pids=[50]", got)
	}

	// Normal foreground processes: deduplicated, sorted ascending, ignoring non-positive PIDs.
	normal := &herdr.PaneProcessInfoResult{
		PaneID:   "w1:p1",
		ShellPID: 50,
		ForegroundProcesses: []herdr.ProcessEntry{
			{PID: 300, Name: "claude"},
			{PID: 0, Name: "bad"},
			{PID: -5, Name: "neg"},
			{PID: 200, Name: "node"},
			{PID: 300, Name: "claude"},
		},
	}
	if got := planHerdrFastLocate(normal, "w1:p1", hpClaude, true); got.mode != herdrFastPIDs || got.shellPID != 50 || !slices.Equal(got.pids, []int{200, 300}) {
		t.Fatalf("normal fg: got %+v, want shellPID=50 pids=[200 300]", got)
	}

	// OpenCode detected by Name or Argv[0] switches directly to OpenCode-only scan.
	ocByName := &herdr.PaneProcessInfoResult{
		PaneID:              "w1:p1",
		ShellPID:            50,
		ForegroundProcesses: []herdr.ProcessEntry{{PID: 400, Name: "opencode"}},
	}
	if got := planHerdrFastLocate(ocByName, "w1:p1", herdr.Pane{}, false); got.mode != herdrFastOpenCode {
		t.Fatalf("opencode by name: got %+v, want opencode", got)
	}
	ocByArgv := &herdr.PaneProcessInfoResult{
		PaneID:              "w1:p1",
		ShellPID:            50,
		ForegroundProcesses: []herdr.ProcessEntry{{PID: 401, Name: "bun", Argv: []string{"/usr/local/bin/opencode"}}},
	}
	if got := planHerdrFastLocate(ocByArgv, "w1:p1", herdr.Pane{}, false); got.mode != herdrFastOpenCode {
		t.Fatalf("opencode by argv: got %+v, want opencode", got)
	}

	// Too many foreground PIDs falls back to full scan.
	many := &herdr.PaneProcessInfoResult{PaneID: "w1:p1", ShellPID: 50}
	for i := 1; i <= maxFastLocatePIDs+1; i++ {
		many.ForegroundProcesses = append(many.ForegroundProcesses, herdr.ProcessEntry{PID: 1000 + i, Name: "worker"})
	}
	if got := planHerdrFastLocate(many, "w1:p1", hpClaude, true); got.mode != herdrFastFull {
		t.Fatalf("too many pids: got %+v, want full", got)
	}
}

func TestFastLocateAccept(t *testing.T) {
	sock := "/run/herdr.sock"
	shellAnc := []convoAnc{{90, "zsh"}, {80, "herdr"}}
	directAnc := []convoAnc{{80, "herdr"}}
	nestedAnc := []convoAnc{{110, "bash"}, {100, "claude"}, {90, "zsh"}, {80, "herdr"}}
	tmuxAnc := []convoAnc{{90, "zsh"}, {80, "tmux"}}

	// noproc is accepted immediately (full scan would return the same).
	if p, ok := fastLocateAccept(convoLocated{noProc: true}, "w1:p1", sock, ""); !ok || p != nil {
		t.Fatalf("noproc: got p=%+v ok=%v, want nil true", p, ok)
	}

	// Empty procs -> fallback.
	if _, ok := fastLocateAccept(convoLocated{}, "w1:p1", sock, ""); ok {
		t.Fatal("empty procs should fall back")
	}

	// Valid Claude directly under shell -> herdr.
	claude := convoProc{pid: 100, start: 500, agent: "claude", sid: sidA, log: "/home/u/.claude-work/projects/-w/" + sidA + ".jsonl", herdrPane: "w1:p1", herdrSocket: sock, anc: shellAnc}
	if p, ok := fastLocateAccept(convoLocated{procs: []convoProc{claude}}, "w1:p1", sock, ""); !ok || p == nil || p.pid != 100 {
		t.Fatalf("valid claude: got p=%+v ok=%v", p, ok)
	}

	// fastIncomplete -> must fall back even if a proc record was present.
	if _, ok := fastLocateAccept(convoLocated{procs: []convoProc{claude}, fastIncomplete: true}, "w1:p1", sock, ""); ok {
		t.Fatal("fastIncomplete should fall back")
	}

	// Multiple candidate procs -> must fall back.
	sibling := claude
	sibling.pid = 101
	sibling.start = 600
	if _, ok := fastLocateAccept(convoLocated{procs: []convoProc{claude, sibling}}, "w1:p1", sock, ""); ok {
		t.Fatal("multiple candidate procs should fall back")
	}

	// Valid Claude directly under herdr (no intermediate shell, e.g. herdr agent start).
	claudeDirect := claude
	claudeDirect.anc = directAnc
	if p, ok := fastLocateAccept(convoLocated{procs: []convoProc{claudeDirect}}, "w1:p1", sock, sidA); !ok || p == nil || p.pid != 100 {
		t.Fatalf("direct under herdr: got p=%+v ok=%v", p, ok)
	}

	// Valid Claude with no log yet (fresh session before first message).
	claudeFresh := claude
	claudeFresh.log = ""
	if p, ok := fastLocateAccept(convoLocated{procs: []convoProc{claudeFresh}}, "w1:p1", sock, sidA); !ok || p == nil || p.pid != 100 {
		t.Fatalf("fresh claude: got p=%+v ok=%v", p, ok)
	}

	// Herdr reports a different session want -> must fall back.
	if _, ok := fastLocateAccept(convoLocated{procs: []convoProc{claude}}, "w1:p1", sock, sidB); ok {
		t.Fatal("want mismatch should fall back")
	}

	// Nested Claude under another Claude -> must fall back (full scan prefers the outer agent).
	nested := claude
	nested.pid = 400
	nested.anc = nestedAnc
	if _, ok := fastLocateAccept(convoLocated{procs: []convoProc{nested}}, "w1:p1", sock, ""); ok {
		t.Fatal("nested claude under claude should fall back")
	}

	// Process without herdr ancestor -> must fall back.
	noHerdr := claude
	noHerdr.anc = tmuxAnc
	if _, ok := fastLocateAccept(convoLocated{procs: []convoProc{noHerdr}}, "w1:p1", sock, ""); ok {
		t.Fatal("non-herdr ancestor should fall back")
	}

	// Wrong pane or wrong socket -> must fall back.
	otherPane := claude
	otherPane.herdrPane = "w2:p1"
	if _, ok := fastLocateAccept(convoLocated{procs: []convoProc{otherPane}}, "w1:p1", sock, ""); ok {
		t.Fatal("wrong pane should fall back")
	}

	// Agy accepted.
	agy := convoProc{pid: 200, start: 600, agent: "agy", sid: sidA, log: "/home/u/.gemini/antigravity-cli/brain/" + sidA + "/.system_generated/logs/transcript_full.jsonl", herdrPane: "w1:p1", herdrSocket: sock, anc: shellAnc}
	if p, ok := fastLocateAccept(convoLocated{procs: []convoProc{agy}}, "w1:p1", sock, ""); !ok || p == nil || p.pid != 200 {
		t.Fatalf("valid agy: got p=%+v ok=%v", p, ok)
	}

	// Codex (even with its own open log), OpenCode, and Devin must all fall back.
	codexOwn := convoProc{pid: 300, start: 700, agent: "codex", sid: sidA, log: "/home/u/.codex/sessions/2026/rollout-1-" + sidA + ".jsonl", herdrPane: "w1:p1", herdrSocket: sock, anc: shellAnc}
	if _, ok := fastLocateAccept(convoLocated{procs: []convoProc{codexOwn}}, "w1:p1", sock, ""); ok {
		t.Fatal("codex must not use fast path")
	}
	oc := convoProc{pid: 400, start: 800, agent: "opencode", sid: convoNoSession, log: "/home/u/.local/share/opencode/opencode.db", herdrPane: "w1:p1", herdrSocket: sock, anc: shellAnc}
	if _, ok := fastLocateAccept(convoLocated{procs: []convoProc{oc}}, "w1:p1", sock, ""); ok {
		t.Fatal("opencode should fall back")
	}
	dv := convoProc{pid: 500, start: 900, agent: "devin", sid: "brave-otter", log: "/home/u/.local/share/devin/cli/sessions.db", herdrPane: "w1:p1", herdrSocket: sock, anc: shellAnc}
	if _, ok := fastLocateAccept(convoLocated{procs: []convoProc{dv}}, "w1:p1", sock, ""); ok {
		t.Fatal("devin should fall back")
	}
}

func TestClaudeConfigDirParsingAndPaths(t *testing.T) {
	// parseClaudeConfigDir reads only CLAUDE_CONFIG_DIR from NUL-delimited environ.
	env := []byte("HOME=/home/u\x00OTHER=x\nCLAUDE_CONFIG_DIR=/forged\x00CLAUDE_CONFIG_DIR=/opt/my cfg/.claude-work/\x00SECRET=top\x00")
	if got := parseClaudeConfigDir(env); got != "/opt/my cfg/.claude-work/" {
		t.Fatalf("parseClaudeConfigDir = %q, want %q", got, "/opt/my cfg/.claude-work/")
	}
	forged := []byte("OTHER=x\nCLAUDE_CONFIG_DIR=/forged\x00PATH=/usr/bin\x00")
	if got := parseClaudeConfigDir(forged); got != "" {
		t.Fatalf("forged CLAUDE_CONFIG_DIR = %q, want empty", got)
	}
	badNL := []byte("CLAUDE_CONFIG_DIR=/good\n/bad\x00")
	if got := parseClaudeConfigDir(badNL); got != "" {
		t.Fatalf("newline CLAUDE_CONFIG_DIR = %q, want empty", got)
	}

	// resolveClaudeConfigDir: unset, absolute, relative, spaces, traversal, invalid.
	cases := []struct {
		name, raw, cwd, home, want string
	}{
		{"unset", "", "/work", "/home/u", "/home/u/.claude"},
		{"absolute", "/some/dir/.claude-xxx", "/work", "/home/u", "/some/dir/.claude-xxx"},
		{"absolute trailing slash", "/some/dir/.claude-xxx///", "/work", "/home/u", "/some/dir/.claude-xxx"},
		{"absolute with spaces", "/home/u/My Configs/.claude work/", "/work", "/home/u", "/home/u/My Configs/.claude work"},
		{"relative with cwd", ".claude-alt", "/home/u/proj", "/home/u", "/home/u/proj/.claude-alt"},
		{"relative with spaces and dotdot", "../my cfg/.claude alt/", "/home/u/proj/sub", "/home/u", "/home/u/proj/my cfg/.claude alt"},
		{"relative without absolute cwd falls back", ".claude-alt", "", "/home/u", "/home/u/.claude"},
		{"relative with non-absolute cwd falls back", ".claude-alt", "rel/cwd", "/home/u", "/home/u/.claude"},
		{"embedded newline falls back", "/tmp/a\n/b", "/work", "/home/u", "/home/u/.claude"},
		{"embedded SOH falls back", "/tmp/a\x01/b", "/work", "/home/u", "/home/u/.claude"},
	}
	for _, tc := range cases {
		if got := resolveClaudeConfigDir(tc.raw, tc.cwd, tc.home); got != tc.want {
			t.Errorf("%s: resolveClaudeConfigDir(%q, %q, %q) = %q, want %q", tc.name, tc.raw, tc.cwd, tc.home, got, tc.want)
		}
	}

	// claudeSessionFilePath & claudeProjectLogGlob.
	if got, ok := claudeSessionFilePath("/home/u/my dir/.claude-alt/", 4242); !ok || got != "/home/u/my dir/.claude-alt/sessions/4242.json" {
		t.Errorf("claudeSessionFilePath = %q, %v", got, ok)
	}
	if _, ok := claudeSessionFilePath("relative/.claude", 4242); ok {
		t.Error("claudeSessionFilePath accepted relative path")
	}
	if _, ok := claudeSessionFilePath("/home/u/.claude", 0); ok {
		t.Error("claudeSessionFilePath accepted pid 0")
	}
	if got, ok := claudeProjectLogGlob("/home/u/my dir/.claude-alt/", sidA); !ok || got != "/home/u/my dir/.claude-alt/projects/*/"+sidA+".jsonl" {
		t.Errorf("claudeProjectLogGlob = %q, %v", got, ok)
	}
	if _, ok := claudeProjectLogGlob("/home/u/.claude", "not-a-uuid"); ok {
		t.Error("claudeProjectLogGlob accepted non-uuid")
	}

	// claudeConfigDirFromLog & claudeCustomConfigDir.
	logCases := []struct {
		log  string
		want string
		ok   bool
	}{
		{"/home/u/.claude/projects/-w/" + sidA + ".jsonl", "/home/u/.claude", true},
		{"/some/dir/.claude-xxx/projects/-home-u-proj/" + sidA + ".jsonl", "/some/dir/.claude-xxx", true},
		{"/home/u/my dir/.claude alt/projects/-home-u-proj/" + sidA + ".jsonl", "/home/u/my dir/.claude alt", true},
		{"/x/.codex/sessions/rollout.jsonl", "", false},
		{"/projects/-w/" + sidA + ".jsonl", "", false},
		{"/home/u/.claude/projects/" + sidA + ".jsonl", "", false},
		{"", "", false},
	}
	for _, tc := range logCases {
		got, ok := claudeConfigDirFromLog(tc.log)
		if ok != tc.ok || got != tc.want {
			t.Errorf("claudeConfigDirFromLog(%q) = (%q, %v), want (%q, %v)", tc.log, got, ok, tc.want, tc.ok)
		}
	}
	// claudeForkConfigDir always returns the source log's config dir (preferring
	// its physical path), including for default ~/.claude.
	if got := claudeForkConfigDir("/home/u/.claude/projects/-w/"+sidA+".jsonl", "/home/u/.claude"); got != "/home/u/.claude" {
		t.Errorf("default logical dir = %q, want /home/u/.claude", got)
	}
	if got := claudeForkConfigDir("/home/u/.claude/projects/-w/"+sidA+".jsonl", "/data/claude"); got != "/data/claude" {
		t.Errorf("symlinked default dir = %q, want physical /data/claude", got)
	}
	if got := claudeForkConfigDir("/opt/my cfg/.claude-alt/projects/-w/"+sidA+".jsonl", ""); got != "/opt/my cfg/.claude-alt" {
		t.Errorf("custom dir = %q, want /opt/my cfg/.claude-alt", got)
	}
	if got := claudeForkConfigDir("/x/.codex/sessions/rollout.jsonl", "/x/.codex"); got != "" {
		t.Errorf("non-claude log = %q, want empty", got)
	}
}

func TestConvoProcStatusCustomConfigDir(t *testing.T) {
	base := t.TempDir()
	cfgDir := filepath.Join(base, "my config", ".claude-custom")
	if err := os.MkdirAll(filepath.Join(cfgDir, "sessions"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfgDir, "sessions", "4242.json"),
		[]byte(`{"sessionId":"`+sidA+`","procStart":"777","status":"waiting","waitingFor":"permission prompt"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	cr, sw := io.Pipe()
	sr, cw := io.Pipe()
	srv, err := sftp.NewServer(struct {
		io.Reader
		io.WriteCloser
	}{sr, sw}, sftp.WithServerWorkingDirectory(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve()
	c, err := sftplite.New(cr, cw, func() { cw.Close(); cr.Close() })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close(); srv.Close() })

	st := &convoState{
		bind: &convoBind{PID: 4242, Start: 777, Session: sidA},
		log:  filepath.Join(cfgDir, "projects", "-home-u-proj", sidA+".jsonl"),
	}
	if got := convoProcStatus(c, st); got != "waiting:permission prompt" {
		t.Fatalf("convoProcStatus custom dir = %q, want waiting:permission prompt", got)
	}
}

// spawnFakeHerdrTree launches a real process tree:
//
//	<tmp>/herdr (symlink to /bin/sh) -> sh (shellPID) -> N sleep children
//
// so /proc/<child>/stat has herdr as grandparent via shellPID, matching real herdr panes.
func spawnFakeHerdrTree(t *testing.T, workDir, home string, childEnvs [][]string) (shellPID int, childPIDs []int, childStarts []string) {
	t.Helper()
	shPath, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	binDir := t.TempDir()
	herdrBin := filepath.Join(binDir, "herdr")
	if err := os.Symlink(shPath, herdrBin); err != nil {
		t.Fatal(err)
	}
	pidFile := filepath.Join(binDir, "pids.txt")
	var childScript strings.Builder
	childScript.WriteString("echo \"shell=$$\" > " + strconv.Quote(pidFile) + "\n")
	for i, env := range childEnvs {
		if i > 0 {
			childScript.WriteString("sleep 0.05\n")
		}
		childScript.WriteString("(\n")
		for _, kv := range env {
			k, v, _ := strings.Cut(kv, "=")
			childScript.WriteString(k + "=$(printf '%s' " + shellSingleQuote(v) + "); export " + k + "\n")
		}
		childScript.WriteString("exec sleep 60\n) &\n")
		childScript.WriteString(fmt.Sprintf("echo \"child%d=$!\" >> %s\n", i, strconv.Quote(pidFile)))
	}
	childScript.WriteString("echo done >> " + strconv.Quote(pidFile) + "\nwait\n")

	outer := exec.Command(herdrBin, "-c", shPath+" -c "+shellSingleQuote(childScript.String())+" & wait")
	outer.Dir = workDir
	outer.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + home}
	if err := outer.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for _, p := range childPIDs {
			if pr, err := os.FindProcess(p); err == nil {
				_ = pr.Kill()
			}
		}
		if shellPID > 0 {
			if pr, err := os.FindProcess(shellPID); err == nil {
				_ = pr.Kill()
			}
		}
		_ = outer.Process.Kill()
		_ = outer.Wait()
	})

	for i := 0; i < 100; i++ {
		if b, err := os.ReadFile(pidFile); err == nil && strings.Contains(string(b), "done\n") {
			childPIDs = make([]int, len(childEnvs))
			for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
				if v, ok := strings.CutPrefix(line, "shell="); ok {
					shellPID, _ = strconv.Atoi(v)
				} else if strings.HasPrefix(line, "child") {
					k, v, _ := strings.Cut(line, "=")
					idx, _ := strconv.Atoi(strings.TrimPrefix(k, "child"))
					childPIDs[idx], _ = strconv.Atoi(v)
				}
			}
			break
		}
		time.Sleep(15 * time.Millisecond)
	}
	if shellPID <= 0 {
		t.Fatalf("failed to start fake herdr tree (pidFile=%q)", pidFile)
	}
	childStarts = make([]string, len(childPIDs))
	for i, pid := range childPIDs {
		for try := 0; try < 100 && childStarts[i] == ""; try++ {
			commBytes, _ := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/comm")
			if strings.TrimSpace(string(commBytes)) != "sleep" {
				time.Sleep(10 * time.Millisecond)
				continue
			}
			b, _ := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
			if j := strings.LastIndex(string(b), ") "); j >= 0 {
				if f := strings.Fields(string(b)[j+2:]); len(f) > 19 {
					childStarts[i] = f[19]
				}
			}
			if childStarts[i] == "" {
				time.Sleep(10 * time.Millisecond)
			}
		}
		if childStarts[i] == "" {
			t.Fatalf("could not read /proc/%d/stat after waiting for exec sleep", pid)
		}
	}
	return shellPID, childPIDs, childStarts
}

func shellSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// Review Item 1: When a pane's shell has TWO sibling Claude Code processes
// (e.g. old in foreground, new in background), the fast path must detect both
// in the pane subtree, emit fast_incomplete=1, and fall back to full scan so
// pickConvoProc / convoPress still selects the newer sibling (agent_changed).
func TestFastLocateSiblingAgentFallsBack(t *testing.T) {
	if _, err := os.Stat("/proc/self/stat"); err != nil {
		t.Skip("no /proc")
	}
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	home := t.TempDir()
	work := filepath.Join(home, "work")
	sessDir := filepath.Join(home, ".claude", "sessions")
	projDir := filepath.Join(home, ".claude", "projects", "-work")
	for _, d := range []string{work, sessDir, projDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	shellPID, pids, starts := spawnFakeHerdrTree(t, work, home, [][]string{
		{"HERDR_PANE_ID=w1:p1", "HERDR_SOCKET_PATH=/run/herdr.sock"},
		{"HERDR_PANE_ID=w1:p1", "HERDR_SOCKET_PATH=/run/herdr.sock"},
	})
	oldPID, newPID := pids[0], pids[1]
	oldStart, newStart := starts[0], starts[1]
	os.WriteFile(filepath.Join(projDir, sidA+".jsonl"), []byte(`{"type":"user"}`+"\n"), 0o644)
	os.WriteFile(filepath.Join(sessDir, strconv.Itoa(oldPID)+".json"),
		[]byte(`{"pid":`+strconv.Itoa(oldPID)+`,"sessionId":"`+sidA+`","procStart":"`+oldStart+`"}`), 0o644)

	// Case 1: Only ONE Claude candidate in the pane subtree -> fast path hits and accepts oldPID.
	runTree := func(shPID int, fg []int) convoLocated {
		c := exec.Command(sh, "-c", convoLocatePidsCommand(shPID, fg))
		c.Env = []string{"HOME=" + home, "PATH=/usr/bin:/bin"}
		out, err := c.Output()
		if err != nil {
			t.Fatalf("convoLocatePidsCommand failed: %v", err)
		}
		return parseConvoLocate(string(out))
	}
	single := runTree(shellPID, []int{oldPID})
	if single.fastIncomplete {
		t.Fatalf("single candidate unexpectedly marked fastIncomplete: %+v", single)
	}
	if p, ok := fastLocateAccept(single, "w1:p1", "/run/herdr.sock", ""); !ok || p == nil || p.pid != int64(oldPID) {
		t.Fatalf("single candidate fastLocateAccept = (%+v, %v), want pid %d", p, ok, oldPID)
	}

	// Case 2: Second sibling Claude appears in the same pane (even with same sessionId,
	// while pane.process_info only lists oldPID in foreground!) -> must mark fastIncomplete
	// and fall back to full scan, which picks the newer sibling (preserving agent_changed).
	os.WriteFile(filepath.Join(sessDir, strconv.Itoa(newPID)+".json"),
		[]byte(`{"pid":`+strconv.Itoa(newPID)+`,"sessionId":"`+sidA+`","procStart":"`+newStart+`"}`), 0o644)
	both := runTree(shellPID, []int{oldPID})
	if !both.fastIncomplete {
		t.Fatalf("two siblings in pane must set fastIncomplete=true, got %+v", both)
	}
	if p, ok := fastLocateAccept(both, "w1:p1", "/run/herdr.sock", ""); ok || p != nil {
		t.Fatalf("two siblings must fall back to full scan, got (%+v, %v)", p, ok)
	}

	// Verify full scan picks the later-started sibling (so convoPress for oldPID returns agent_changed).
	cFull := exec.Command(sh, "-c", convoLocateCommand("herdr", "", ""))
	cFull.Env = []string{"HOME=" + home, "PATH=/usr/bin:/bin"}
	fullOut, err := cFull.Output()
	if err != nil {
		t.Fatalf("full scan failed: %v", err)
	}
	fullLoc := parseConvoLocate(string(fullOut))
	picked := pickConvoProc(fullLoc.procs, "herdr", "w1:p1", "/run/herdr.sock", "", "", "")
	if picked == nil || picked.pid != int64(newPID) {
		t.Fatalf("full scan picked %+v, want newer sibling pid %d", picked, newPID)
	}

	// Case 3: Stale fgPID not in subtree -> fastIncomplete.
	stale := runTree(shellPID, []int{9999999})
	if !stale.fastIncomplete {
		t.Fatalf("stale fgPID outside subtree must set fastIncomplete=true, got %+v", stale)
	}

	// Case 4: Root PID whose parent is NOT herdr -> fastIncomplete.
	notHerdr := runTree(os.Getpid(), []int{os.Getpid()})
	if !notHerdr.fastIncomplete {
		t.Fatalf("shell_pid not parented by herdr must set fastIncomplete=true, got %+v", notHerdr)
	}
}

// Review Item 2 + Round 2 P1: convoClaimed.seeAliveOn must be isolated per
// remote host and SSH identity, and OpenCode-only scans must not suppress
// Codex liveness refresh.
func TestConvoClaimsAliveRefreshOnFastHit(t *testing.T) {
	var claims convoClaims
	const (
		hostA = "1\x0010\x001|host-a|22|alice|key|1\x00alice@10.0.0.1:22"
		hostB = "1\x0020\x001|host-b|22|bob|key|2\x00bob@10.0.0.2:22"
	)
	now := time.Now()
	if claims.needsAliveRefresh(hostA, now, convoLocateTTL) {
		t.Fatal("empty claims must not trigger refresh")
	}
	claims.contest(sidB) // contested-only (pid==0) is not an active owner
	if claims.needsAliveRefresh(hostA, now, convoLocateTTL) {
		t.Fatal("contested-only claim must not trigger refresh")
	}

	codexA := procID{pid: 100, start: 1000}
	codexB := procID{pid: 200, start: 2000}
	logA := "/home/u/.codex/sessions/2026/11/27/rollout-2026-11-27T01-00-00-" + sidA + ".jsonl"
	if !claims.takeOn(hostA, sidA, codexA, 1000, map[procID]bool{codexA: true}, 1000, false) {
		t.Fatal("codexA should claim sidA")
	}
	if !claims.needsAliveRefresh(hostA, now, convoLocateTTL) {
		t.Fatal("active claim on hostA with no seeAliveOn yet must need refresh")
	}
	// Host B has no Codex claims of its own -> does not need refresh.
	if claims.needsAliveRefresh(hostB, now, convoLocateTTL) {
		t.Fatal("hostB with no claims must not need refresh")
	}
	// Host B scanning continuously MUST NOT refresh hostA's lastAlive or seen!
	claims.seeAliveOn(hostB, convoLocated{cands: []convoCand{{log: logA}}}, map[procID]bool{codexA: true}, 5000)
	if !claims.needsAliveRefresh(hostA, time.Now(), convoLocateTTL) {
		t.Fatal("hostB seeAliveOn suppressed hostA refresh")
	}
	// Host B holding a refresh lock MUST NOT block hostA from starting a refresh.
	const sidHostB = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
	if !claims.takeOn(hostB, sidHostB, codexB, 2000, map[procID]bool{codexB: true}, 2000, false) {
		t.Fatal("codexB should claim sidHostB on hostB")
	}
	afterTTL := time.Now().Add(convoLocateTTL + time.Second)
	if !claims.beginAliveRefresh(hostB, afterTTL, convoLocateTTL) {
		t.Fatal("hostB should begin refresh once TTL elapsed")
	}
	if !claims.beginAliveRefresh(hostA, afterTTL, convoLocateTTL) {
		t.Fatal("hostB refreshing blocked hostA from beginning refresh")
	}
	claims.endAliveRefresh(hostB)
	claims.endAliveRefresh(hostA)

	// Simulate periodic full-scan seeAliveOn for hostA while codexA and codexB coexist.
	claims.seeAliveOn(hostA, convoLocated{cands: []convoCand{{log: logA}}}, map[procID]bool{codexA: true}, 5000)
	if claims.needsAliveRefresh(hostA, time.Now(), convoLocateTTL) {
		t.Fatal("right after seeAliveOn(hostA), refresh should not be due yet")
	}
	if !claims.needsAliveRefresh(hostA, time.Now().Add(convoLocateTTL+time.Second), convoLocateTTL) {
		t.Fatal("after TTL elapses, active claim on hostA must need refresh again")
	}
	// After codexA exits, codexB (started at 2000 <= seen 5000) must be blocked from claiming sidA.
	if claims.takeOn(hostA, sidA, codexB, 2000, map[procID]bool{codexB: true}, 9000, false) {
		t.Fatal("codexB stole codexA's session despite seeAliveOn refresh")
	}
}

// Round 2 P1 (end-to-end): While Host B continuously runs full scans and Host A
// runs OpenCode-only scans, a fast-path hit on Host A still triggers Host A's
// Codex claim liveness refresh on schedule.
func TestConvoClaimsPerHostRefreshIsolation(t *testing.T) {
	h := newHarness(t)
	admin := h.admin()

	hdA := newFakeHerdr(t)
	hdA.setSnapshot(
		[]any{
			map[string]any{"pane_id": "w1:p1", "tab_id": "t1", "workspace_id": "w1", "agent": "claude", "agent_status": "idle", "cwd": "/work"},
			map[string]any{"pane_id": "w1:p2", "tab_id": "t1", "workspace_id": "w1", "agent": "opencode", "agent_status": "idle", "cwd": "/work"},
		},
		[]any{
			map[string]any{"pane_id": "w1:p1", "tab_id": "t1", "workspace_id": "w1", "agent": "claude", "agent_status": "idle", "cwd": "/work"},
			map[string]any{"pane_id": "w1:p2", "tab_id": "t1", "workspace_id": "w1", "agent": "opencode", "agent_status": "idle", "cwd": "/work"},
		},
	)
	hdA.procInfo = map[string]herdr.PaneProcessInfoResult{
		"w1:p1": {PaneID: "w1:p1", ShellPID: 50, ForegroundProcesses: []herdr.ProcessEntry{{PID: 100, Name: "claude"}}},
	}
	hdB := newFakeHerdr(t)
	hdB.setSnapshot(
		[]any{map[string]any{"pane_id": "w1:p1", "tab_id": "t1", "workspace_id": "w1", "agent": "claude", "agent_status": "idle", "cwd": "/work"}},
		[]any{map[string]any{"pane_id": "w1:p1", "tab_id": "t1", "workspace_id": "w1", "agent": "claude", "agent_status": "idle", "cwd": "/work"}},
	)

	claudeOutA := convoRec(
		"pid=100", "start=500", "fg=1", "procstart=500", "sid="+sidA,
		"log=/home/u/.claude/projects/-w/"+sidA+".jsonl",
		"anc=50:zsh,40:herdr,", "agent=claude",
		"HERDR_PANE_ID=w1:p1", "HERDR_SOCKET_PATH="+hdA.path,
	)
	ocOutA := convoRec(
		"pid=150", "start=600", "fg=1", "procstart=600", "sid="+convoNoSession,
		"log=/home/u/.local/share/opencode/opencode.db",
		"anc=51:zsh,40:herdr,", "agent=opencode",
		"HERDR_PANE_ID=w1:p2", "HERDR_SOCKET_PATH="+hdA.path,
	)
	codexLogA := "/home/u/.codex/sessions/2026/11/27/rollout-2026-11-27T01-00-00-" + sidB + ".jsonl"
	fullOutA := claudeOutA + convoRec(
		"pid=300", "start=700", "fg=1", "procstart=700", "sid=", "log=", "cwd=/work",
		"anc=52:zsh,40:herdr,", "agent=codex",
		"HERDR_PANE_ID=w1:p3", "HERDR_SOCKET_PATH="+hdA.path,
	) + fmt.Sprintf("cand\t%s\t/work\t1000\t9000\nclock\t1000\t100\n", codexLogA)
	fullOutB := convoRec(
		"pid=400", "start=800", "fg=1", "procstart=800", "sid="+sidA,
		"log=/home/u/.claude/projects/-w/"+sidA+".jsonl",
		"anc=60:zsh,41:herdr,", "agent=claude",
		"HERDR_PANE_ID=w1:p1", "HERDR_SOCKET_PATH="+hdB.path,
	)

	var fullCallsA, ocCallsA, fullCallsB atomic.Int32
	sessIDA := herdrExecEnv(t, h, admin, hdA, t.TempDir(), func(cmd string) func(ssh.Channel) {
		if strings.Contains(cmd, "'tree:50:100'") {
			return sshExecReply(claudeOutA, 0)
		}
		if strings.HasSuffix(cmd, ` corral 'herdr' '' '' '' 'opencode'`) {
			ocCallsA.Add(1)
			return sshExecReply(ocOutA, 0)
		}
		if strings.HasSuffix(cmd, ` corral 'herdr' '' '' '' ''`) {
			fullCallsA.Add(1)
			return sshExecReply(fullOutA, 0)
		}
		return nil
	})
	sessIDB := herdrExecEnv(t, h, admin, hdB, t.TempDir(), func(cmd string) func(ssh.Channel) {
		if strings.HasSuffix(cmd, ` corral 'herdr' '' '' '' ''`) {
			fullCallsB.Add(1)
			return sshExecReply(fullOutB, 0)
		}
		return nil
	})
	sessA, err := h.terms.Get(1, sessIDA)
	if err != nil {
		t.Fatal(err)
	}
	sessB, err := h.terms.Get(1, sessIDB)
	if err != nil {
		t.Fatal(err)
	}

	hostA := convoClaimHostKey(sessA)
	hostB := convoClaimHostKey(sessB)
	if hostA == hostB {
		t.Fatalf("distinct hosts produced identical convoClaimHostKey: %q", hostA)
	}

	// Claim a Codex conversation on Host A (lastAlive not set yet).
	codexProcA := procID{pid: 300, start: 700}
	if !h.srv.convoClaimed.takeOn(hostA, sidB, codexProcA, 1000, map[procID]bool{codexProcA: true}, 1000, false) {
		t.Fatal("failed to claim codex on host A")
	}

	// 1. Host B runs a full scan continuously -> updates Host B's lastAlive ONLY.
	stB := &convoState{}
	if err := h.srv.locateScan(context.Background(), sessB, stB, "w1:p1", false, nil); err != nil {
		t.Fatalf("host B full scan failed: %v", err)
	}
	if fullCallsB.Load() != 1 {
		t.Fatalf("host B fullCalls = %d, want 1", fullCallsB.Load())
	}
	if !h.srv.convoClaimed.needsAliveRefresh(hostA, time.Now(), convoLocateTTL) {
		t.Fatal("host B full scan cleared host A's needsAliveRefresh")
	}

	// 2. Host A hits the fast path on Claude pane w1:p1 -> MUST trigger Host A's
	// background full scan refresh even though Host B just ran a full scan!
	stA := &convoState{}
	if err := h.srv.locateScan(context.Background(), sessA, stA, "w1:p1", true, nil); err != nil {
		t.Fatalf("host A fast locate failed: %v", err)
	}
	eventually(t, "host A fast hit did not trigger host A Codex claim refresh", func() bool {
		return fullCallsA.Load() >= 1 && !h.srv.convoClaimed.needsAliveRefresh(hostA, time.Now(), convoLocateTTL)
	})

	// 3. Age Host A's lastAlive past TTL, then run an OpenCode-only scan on Host A.
	// OpenCode-only scan must NOT satisfy Host A's lastAlive by itself; it must
	// trigger a full scan refresh on Host A instead.
	prevFullA := fullCallsA.Load()
	h.srv.convoClaimed.mu.Lock()
	h.srv.convoClaimed.hosts[hostA].lastAlive = time.Now().Add(-2 * convoLocateTTL)
	h.srv.convoClaimed.mu.Unlock()
	// Expire convoScans cache so refresh runs a fresh full scan.
	h.srv.convoScans.mu.Lock()
	h.srv.convoScans.m = nil
	h.srv.convoScans.mu.Unlock()

	stOC := &convoState{}
	if err := h.srv.locateScan(context.Background(), sessA, stOC, "w1:p2", true, nil); err != nil {
		t.Fatalf("host A opencode scan failed: %v", err)
	}
	if ocCallsA.Load() < 1 {
		t.Fatalf("expected opencode-only scan on host A, got %d", ocCallsA.Load())
	}
	eventually(t, "opencode-only scan on host A did not trigger Codex full refresh", func() bool {
		return fullCallsA.Load() > prevFullA
	})
}

func sshExecReply(out string, code uint32) func(ssh.Channel) {
	return func(ch ssh.Channel) {
		if out != "" {
			_, _ = ch.Write([]byte(out))
		}
		_ = ch.CloseWrite()
		var b [4]byte
		binary.BigEndian.PutUint32(b[:], code)
		_, _ = ch.SendRequest("exit-status", false, b[:])
	}
}

func herdrExecEnv(t *testing.T, h *harness, admin *client, hd *fakeHerdr, sftpRoot string, extra func(cmd string) func(ssh.Channel)) string {
	t.Helper()
	srv := h.sshServer(sftpRoot)
	srv.HandleExec(func(cmd string) func(ssh.Channel) {
		if cmd == herdr.KeysCommand {
			return noHerdrConfig
		}
		if cmd == herdr.DiscoverCommand {
			return func(ch ssh.Channel) {
				b, _ := json.Marshal(map[string]any{"sessions": []any{map[string]any{"name": "default", "default": true, "running": true, "socket_path": hd.path}}})
				_, _ = ch.Write(b)
				_ = ch.CloseWrite()
				time.Sleep(50 * time.Millisecond)
				_, _ = ch.SendRequest("exit-status", false, []byte{0, 0, 0, 0})
			}
		}
		if extra != nil {
			if fn := extra(cmd); fn != nil {
				return fn
			}
		}
		return nil
	})
	r := admin.do("POST", "/api/hosts", map[string]any{"name": "box", "address": "127.0.0.1", "port": srv.Addr.Port,
		"username": "alice", "auth": "password", "password": "alice-pass", "persistence": "herdr"})
	mustStatus(t, r, 201, "create host")
	r = admin.do("POST", "/api/sessions", map[string]any{"host_id": int64(r.json()["id"].(float64))})
	mustStatus(t, r, 201, "open herdr session")
	id := r.json()["id"].(string)
	eventually(t, "herdr session was never discovered", func() bool { return h.srv.herdrSocks.get(id) == hd.path })
	return id
}

// Review Item 3: Execution failures on the fast-path scan in locateScan,
// convoPress, and apiSideQuestion must fall back to the full scan (including
// reusing a cached full scan in locateScan) rather than failing early.
func TestFastLocateErrorFallsBackToFull(t *testing.T) {
	h := newHarness(t)
	admin := h.admin()
	hd := newFakeHerdr(t)
	hd.setSnapshot(
		[]any{map[string]any{"pane_id": "w1:p1", "tab_id": "t1", "workspace_id": "w1", "agent": "claude", "agent_status": "idle", "cwd": "/work"}},
		[]any{map[string]any{"pane_id": "w1:p1", "tab_id": "t1", "workspace_id": "w1", "agent": "claude", "agent_status": "idle", "cwd": "/work"}},
	)
	hd.procInfo = map[string]herdr.PaneProcessInfoResult{
		"w1:p1": {
			PaneID:              "w1:p1",
			ShellPID:            50,
			ForegroundProcesses: []herdr.ProcessEntry{{PID: 100, Name: "claude"}},
		},
	}

	fullOut := convoRec(
		"pid=100", "start=500", "fg=1", "procstart=500", "sid="+sidA,
		"log=/home/u/.claude/projects/-w/"+sidA+".jsonl",
		"anc=50:zsh,40:herdr,", "agent=claude",
		"HERDR_PANE_ID=w1:p1", "HERDR_SOCKET_PATH="+hd.path,
	)

	var treeCalls, fullCalls atomic.Int32
	sessID := herdrExecEnv(t, h, admin, hd, t.TempDir(), func(cmd string) func(ssh.Channel) {
		if strings.Contains(cmd, "'tree:50:100'") {
			treeCalls.Add(1)
			return sshExecReply("", 1)
		}
		if strings.HasSuffix(cmd, ` corral 'herdr' '' '' '' ''`) {
			fullCalls.Add(1)
			return sshExecReply(fullOut, 0)
		}
		return nil
	})
	sess, err := h.terms.Get(1, sessID)
	if err != nil {
		t.Fatal(err)
	}

	// 1. locateScan: pre-populate a fresh cached full scan in convoScans; fast
	// tree scan fails -> locateScan falls back and reuses the cached full scan
	// with zero additional full-scan SSH call!
	fullCmd := convoLocateCommand("herdr", "", "")
	cached := &convoScan{done: make(chan struct{}), out: []byte(fullOut), at: time.Now(), began: time.Now()}
	close(cached.done)
	h.srv.convoScans.mu.Lock()
	if h.srv.convoScans.m == nil {
		h.srv.convoScans.m = map[string]*convoScan{}
	}
	h.srv.convoScans.m[sess.ID+"\x00"+fullCmd] = cached
	h.srv.convoScans.mu.Unlock()

	st := &convoState{}
	if err := h.srv.locateScan(context.Background(), sess, st, "w1:p1", true, nil); err != nil {
		t.Fatalf("locateScan should fall back to cached full scan, got err: %v", err)
	}
	if treeCalls.Load() != 1 || fullCalls.Load() != 0 {
		t.Fatalf("expected 1 failed tree call and 0 new full calls (reused cache), got tree=%d full=%d", treeCalls.Load(), fullCalls.Load())
	}
	if st.bind == nil || st.bind.PID != 100 || st.sid != sidA {
		t.Fatalf("locateScan fallback bind = %+v sid=%q, want pid=100 sid=%s", st.bind, st.sid, sidA)
	}

	// 2. convoPress: fast tree scan fails -> falls back to full scan and returns
	// 409 agent_changed for wrong expected PID (proving full scan ran instead of
	// returning 502 host_error on the fast-scan failure).
	r := admin.do("POST", "/api/sessions/"+sessID+"/conversation/interrupt", map[string]any{
		"pane": "w1:p1", "pid": 999, "start": 500, "session": sidA,
	})
	if r.Status != 409 || r.code() != "agent_changed" {
		t.Fatalf("convoPress fast-fail fallback: status=%d code=%q body=%s, want 409 agent_changed", r.Status, r.code(), r.Body)
	}
	if treeCalls.Load() < 2 || fullCalls.Load() < 1 {
		t.Fatalf("convoPress did not fall back: tree=%d full=%d", treeCalls.Load(), fullCalls.Load())
	}

	// 3. apiSideQuestion: fast tree scan fails -> falls back to full scan and
	// returns 409 agent_changed for wrong expected PID (not 502 host_error).
	prevTree, prevFull := treeCalls.Load(), fullCalls.Load()
	r2 := admin.do("POST", "/api/sessions/"+sessID+"/conversation/side", map[string]any{
		"pane": "w1:p1", "pid": 999, "start": 500, "session": sidA, "question": "hello?",
	})
	if r2.Status != 409 || r2.code() != "agent_changed" {
		t.Fatalf("apiSideQuestion fast-fail fallback: status=%d code=%q body=%s, want 409 agent_changed", r2.Status, r2.code(), r2.Body)
	}
	if treeCalls.Load() <= prevTree || fullCalls.Load() <= prevFull {
		t.Fatalf("apiSideQuestion did not fall back: tree=%d->%d full=%d->%d", prevTree, treeCalls.Load(), prevFull, fullCalls.Load())
	}
}

// isolateClaudeCfgProc points claude_cfg_dirs's /proc/[0-9]* loop at procDir
// so tests are isolated from real Claude/Node processes or concurrent test runs
// under the same user without changing production scripts.
func isolateClaudeCfgProc(t *testing.T, cmd, procDir string) string {
	t.Helper()
	const want = "for _p in /proc/[0-9]*; do"
	if !strings.Contains(cmd, want) {
		t.Fatalf("expected claude_cfg_dirs /proc scan in command: %s", cmd)
	}
	if strings.ContainsAny(procDir, "'\"$`\\\n") {
		t.Fatalf("unsupported characters in procDir %q", procDir)
	}
	return strings.ReplaceAll(cmd, want, `for _p in "`+procDir+`"/[0-9]*; do`)
}

// Review Items 5 & 6:
//   - Physical deduplication (pwd -P) when $HOME/.claude is a symlink to the same
//     directory as CLAUDE_CONFIG_DIR so forkFindCommand returns 1 path (not 2).
//   - Forking a Claude session in a custom CLAUDE_CONFIG_DIR passes
//     CLAUDE_CONFIG_DIR to both tmux (QuoteShell'd) and herdr (workspace.create env).
func TestClaudeConfigDirsPhysicalDedupAndFork(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	base := t.TempDir()
	fakeProc := t.TempDir()
	sidC := "33333333-3333-4333-8333-333333333333"
	// Run an unrelated fake claude process under the same user whose

	realCfg := filepath.Join(base, "real data", "claude-dir")
	projDir := filepath.Join(realCfg, "projects", "-work")
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(base, "home")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	// $HOME/.claude is a symlink to realCfg!
	if err := os.Symlink(realCfg, filepath.Join(home, ".claude")); err != nil {
		t.Fatal(err)
	}
	logFile := filepath.Join(projDir, sidA+".jsonl")
	if err := os.WriteFile(logFile, []byte(`{"type":"user","cwd":"/work","message":{"role":"user","content":"hi"}}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Both CLAUDE_CONFIG_DIR (physical) and $HOME/.claude (symlink) point to realCfg:
	// forkFindCommand must deduplicate by physical path and emit strictly 1 line.
	for _, bin := range []string{sh, "dash"} {
		p, err := exec.LookPath(bin)
		if err != nil {
			continue
		}
		c := exec.Command(p, "-c", isolateClaudeCfgProc(t, forkFindCommand("claude", sidA), fakeProc))
		c.Env = []string{"HOME=" + home, "CLAUDE_CONFIG_DIR=" + realCfg, "PATH=/usr/bin:/bin"}
		out, err := c.Output()
		if err != nil {
			t.Fatalf("%s forkFindCommand failed: %v", bin, err)
		}
		lines := strings.Split(strings.TrimSpace(string(out)), "\n")
		if len(lines) != 1 || lines[0] != logFile {
			t.Fatalf("%s forkFindCommand with symlinked .claude = %q, want single line %q", bin, string(out), logFile)
		}
	}

	// Review Item 6 (tmux): agentTmuxCmd sets QuoteShell'd CLAUDE_CONFIG_DIR for Claude.
	customDir := "/opt/user's cfg/.claude-alt"
	tmuxCmd := agentTmuxCmd("fork-claude-1", "/work", "claude", customDir, agentStart{resume: sidA})
	if strings.Contains(tmuxCmd, "CODEX_HOME=") {
		t.Fatalf("agentTmuxCmd for claude must not set CODEX_HOME: %s", tmuxCmd)
	}
	wantRunPrefix := "CLAUDE_CONFIG_DIR=" + sshx.QuoteShell(customDir) + " exec 'claude'"
	wantInnerPrefix := sshx.QuoteShell(`exec "${SHELL:-/bin/sh}" -lc ` + sshx.QuoteShell(wantRunPrefix))
	// Check the quoted prefix up to the closing quotes of the inner command.
	if !strings.Contains(tmuxCmd, strings.TrimSuffix(wantInnerPrefix, `'\'''`)) {
		t.Fatalf("agentTmuxCmd quoting mismatch:\n got: %s\nwant contains: %s", tmuxCmd, strings.TrimSuffix(wantInnerPrefix, `'\'''`))
	}

	// Review Item 6 (herdr): POST /api/sessions/{id}/agents with fork=true for a
	// Claude session in a custom CLAUDE_CONFIG_DIR passes env.CLAUDE_CONFIG_DIR
	// to workspace.create.
	h := newHarness(t)
	admin := h.admin()
	hd := newFakeHerdr(t)
	workDir := filepath.Join(base, "projwork")
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		t.Fatal(err)
	}
	customForkCfg := filepath.Join(base, "my custom", ".claude-fork")
	customForkProj := filepath.Join(customForkCfg, "projects", "-projwork")
	if err := os.MkdirAll(customForkProj, 0o755); err != nil {
		t.Fatal(err)
	}
	forkLog := filepath.Join(customForkProj, sidB+".jsonl")
	if err := os.WriteFile(forkLog, []byte(`{"type":"user","uuid":"aaaaaaaa-0000-4000-8000-000000000001","sessionId":"`+sidB+`","cwd":`+strconv.Quote(workDir)+`,"message":{"role":"user","content":"hello"}}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Use a separate home whose ~/.claude does NOT contain sidB.
	forkHome := filepath.Join(base, "forkhome")
	os.MkdirAll(filepath.Join(forkHome, ".claude"), 0o755)
	sessID := herdrExecEnv(t, h, admin, hd, forkHome, func(cmd string) func(ssh.Channel) {
		if strings.Contains(cmd, "herdr agent start") {
			return sshExecReply(`{"result":{"ok":true}}`+"\n", 0)
		}
		if strings.Contains(cmd, "claude_cfg_dirs") || strings.Contains(cmd, "mktemp") || strings.Contains(cmd, "wc -l") {
			if strings.Contains(cmd, "claude_cfg_dirs") {
				cmd = isolateClaudeCfgProc(t, cmd, fakeProc)
			}
			c := exec.Command(sh, "-c", cmd)
			c.Env = []string{"HOME=" + forkHome, "CLAUDE_CONFIG_DIR=" + customForkCfg, "PATH=/usr/bin:/bin"}
			out, err := c.CombinedOutput()
			var code uint32
			if err != nil {
				code = 1
			}
			return sshExecReply(string(out), code)
		}
		return nil
	})
	resp := admin.do("POST", "/api/sessions/"+sessID+"/agents", map[string]any{
		"kind": "claude", "fork": true, "resume": sidB, "key": "k-custom-fork-1234",
	})
	if resp.Status != 200 {
		t.Fatalf("herdr custom-dir fork failed: status=%d body=%s", resp.Status, resp.Body)
	}
	hd.mu.Lock()
	defer hd.mu.Unlock()
	foundCreate := false
	for _, s := range hd.sent {
		if s["method"] == "workspace.create" {
			foundCreate = true
			envMap, _ := s["env"].(map[string]any)
			if envMap == nil || envMap["CLAUDE_CONFIG_DIR"] != customForkCfg {
				t.Fatalf("workspace.create env = %#v, want CLAUDE_CONFIG_DIR=%q", envMap, customForkCfg)
			}
		}
	}
	if !foundCreate {
		t.Fatalf("workspace.create not called: sent=%#v", hd.sent)
	}

	// Round 2 P2-5: Forking from DEFAULT ~/.claude (even when symlinked to
	// realCfg, and even when the remote environment has another account's
	// CLAUDE_CONFIG_DIR set) MUST still explicitly pass the default dir's
	// physical path to workspace.create and to agentTmuxCmd!
	defForkHome := filepath.Join(base, "defhome")
	defRealDir := filepath.Join(base, "real default claude")
	defProj := filepath.Join(defRealDir, "projects", "-projwork")
	otherCfg := filepath.Join(base, "other-account-claude")
	for _, d := range []string{defForkHome, defProj, otherCfg} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(defRealDir, filepath.Join(defForkHome, ".claude")); err != nil {
		t.Fatal(err)
	}
	defForkLog := filepath.Join(defProj, sidC+".jsonl")
	if err := os.WriteFile(defForkLog, []byte(`{"type":"user","uuid":"aaaaaaaa-0000-4000-8000-000000000002","sessionId":"`+sidC+`","cwd":`+strconv.Quote(workDir)+`,"message":{"role":"user","content":"from default"}}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	hdDef := newFakeHerdr(t)
	sessIDDef := herdrExecEnv(t, h, admin, hdDef, defForkHome, func(cmd string) func(ssh.Channel) {
		if strings.Contains(cmd, "herdr agent start") {
			return sshExecReply(`{"result":{"ok":true}}`+"\n", 0)
		}
		if strings.Contains(cmd, "claude_cfg_dirs") || strings.Contains(cmd, "mktemp") || strings.Contains(cmd, "wc -l") {
			if strings.Contains(cmd, "claude_cfg_dirs") {
				cmd = isolateClaudeCfgProc(t, cmd, fakeProc)
			}
			c := exec.Command(sh, "-c", cmd)
			// Remote environment has another account's CLAUDE_CONFIG_DIR!
			c.Env = []string{"HOME=" + defForkHome, "CLAUDE_CONFIG_DIR=" + otherCfg, "PATH=/usr/bin:/bin"}
			out, err := c.CombinedOutput()
			var code uint32
			if err != nil {
				code = 1
			}
			return sshExecReply(string(out), code)
		}
		return nil
	})
	respDef := admin.do("POST", "/api/sessions/"+sessIDDef+"/agents", map[string]any{
		"kind": "claude", "fork": true, "resume": sidC, "key": "k-default-fork-1234",
	})
	if respDef.Status != 200 {
		t.Fatalf("herdr default-dir fork failed: status=%d body=%s", respDef.Status, respDef.Body)
	}
	hdDef.mu.Lock()
	defer hdDef.mu.Unlock()
	foundDefCreate := false
	for _, s := range hdDef.sent {
		if s["method"] == "workspace.create" {
			foundDefCreate = true
			envMap, _ := s["env"].(map[string]any)
			if envMap == nil || envMap["CLAUDE_CONFIG_DIR"] != defRealDir {
				t.Fatalf("default-dir fork workspace.create env = %#v, want physical CLAUDE_CONFIG_DIR=%q", envMap, defRealDir)
			}
		}
	}
	if !foundDefCreate {
		t.Fatalf("default-dir fork workspace.create not called: sent=%#v", hdDef.sent)
	}

	// tmux path for default ~/.claude fork: explicitly exports physical path.
	defTmuxCmd := agentTmuxCmd("fork-claude-def", workDir, "claude", defRealDir, agentStart{resume: sidC})
	wantDefPrefix := sshx.QuoteShell(`exec "${SHELL:-/bin/sh}" -lc ` + sshx.QuoteShell("CLAUDE_CONFIG_DIR="+sshx.QuoteShell(defRealDir)+" exec 'claude'"))
	if !strings.Contains(defTmuxCmd, strings.TrimSuffix(wantDefPrefix, `'\'''`)) {
		t.Fatalf("default-dir tmux fork command missing explicit physical CLAUDE_CONFIG_DIR:\n got: %s", defTmuxCmd)
	}
}

// Round 2 P2-2: Missing log on the LAST session in a batch must exit 0 and
// return all earlier records intact; genuine read errors on an existing file
// are reported separately via readErr without breaking the rest of the batch.
func TestBgTailBatchMissingLastAndReadError(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	home := t.TempDir()
	fakeProc := t.TempDir()
	proj := filepath.Join(home, ".claude", "projects", "-work")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	logA := filepath.Join(proj, sidA+".jsonl")
	if err := os.WriteFile(logA, []byte(`{"type":"system","subtype":"turn_duration","uuid":"u-first","pendingBackgroundAgentCount":2}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sidMissing := "99999999-9999-4999-8999-999999999999"

	for _, bin := range []string{sh, "dash"} {
		p, err := exec.LookPath(bin)
		if err != nil {
			continue
		}
		// Unpinned batch where LAST SID has no log -> must exit 0 and return sidA!
		c := exec.Command(p, "-c", isolateClaudeCfgProc(t, bgTailCommand([]string{sidA, sidMissing}, 4096), fakeProc))
		c.Env = []string{"HOME=" + home, "PATH=/usr/bin:/bin"}
		out, err := c.Output()
		if err != nil {
			t.Fatalf("%s bgTailCommand with missing last sid failed: %v (out=%q)", bin, err, string(out))
		}
		got := parseBgTails(out)
		if r, ok := got[sidA]; !ok || r.uuid != "u-first" || r.n != 2 || r.readErr {
			t.Fatalf("%s first record lost when last missing: got=%+v (raw=%q)", bin, got, string(out))
		}
		if _, ok := got[sidMissing]; ok {
			t.Fatalf("%s missing last sid should be absent from parsed map, got=%+v", bin, got[sidMissing])
		}
	}

	// Genuine read error expressed separately from missing (and discards partial lines).
	errOut := strings.Join([]string{
		"",
		"@@corral-bg " + sidA,
		`{"type":"system","subtype":"turn_duration","uuid":"u-ok","pendingBackgroundAgentCount":1}`,
		"",
		"@@corral-bg " + sidB,
		`{"type":"system","subtype":"turn_duration","uuid":"u-partial","pendingBackgroundAgentCount":9}`,
		"",
		"@@corral-bg-err " + sidB,
		"",
		"@@corral-bg-missing " + sidMissing,
		"",
	}, "\n")
	parsed := parseBgTails([]byte(errOut))
	if r, ok := parsed[sidA]; !ok || r.uuid != "u-ok" || r.n != 1 || r.readErr {
		t.Fatalf("sidA after err/missing = (%+v, %v)", r, ok)
	}
	if r, ok := parsed[sidB]; !ok || !r.readErr || r.uuid != "" || r.n != 0 {
		t.Fatalf("sidB readErr must clear partial record and set readErr=true, got (%+v, %v)", r, ok)
	}
	if _, ok := parsed[sidMissing]; ok {
		t.Fatalf("sidMissing must be omitted, got %+v", parsed[sidMissing])
	}

	// Live chmod-000 read error test (when running non-root).
	if os.Geteuid() != 0 {
		unreadable := filepath.Join(proj, sidB+".jsonl")
		if err := os.WriteFile(unreadable, []byte(`{"type":"system","subtype":"turn_duration","uuid":"secret"}`+"\n"), 0o000); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(unreadable, 0o600) })
		c := exec.Command(sh, "-c", isolateClaudeCfgProc(t, bgTailCommand([]string{sidA, sidB}, 4096), fakeProc))
		c.Env = []string{"HOME=" + home, "PATH=/usr/bin:/bin"}
		out, err := c.Output()
		if err != nil {
			t.Fatalf("bgTailCommand with unreadable file exited non-zero: %v", err)
		}
		live := parseBgTails(out)
		if r, ok := live[sidA]; !ok || r.uuid != "u-first" {
			t.Fatalf("sidA lost when sidB unreadable: %+v", live)
		}
		if r, ok := live[sidB]; !ok || !r.readErr {
			t.Fatalf("sidB unreadable should set readErr=true, got (%+v, %v) raw=%q", r, ok, string(out))
		}
	}
}

// Round 3: Stateless bgTailCommand & cmdScanCommand rules:
//  1. When strictly 1 log exists across physical-deduplicated claude_cfg_dirs,
//     use it and derive subagents strictly from its sibling subagents dir.
//  2. When the sole log has no sibling subagents dir, never pull subagents from
//     another project or config dir.
//  3. When >1 logs exist for the same SID (across projects or config dirs),
//     refuse to guess by mtime (emit nothing, exit 0).
func TestBgTailAndCmdScanStatelessSoleAndAmbiguous(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	base := t.TempDir()
	home := filepath.Join(base, "home")
	realDefCfg := filepath.Join(base, "real default claude")
	altCfg := filepath.Join(base, "my cfg", ".claude-alt")
	defProjA := filepath.Join(realDefCfg, "projects", "-work-a")
	defProjB := filepath.Join(realDefCfg, "projects", "-work-b")
	altProj := filepath.Join(altCfg, "projects", "-work-c")
	for _, d := range []string{home, defProjA, defProjB, altProj} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// $HOME/.claude is a symlink to realDefCfg (must deduplicate to 1 dir).
	if err := os.Symlink(realDefCfg, filepath.Join(home, ".claude")); err != nil {
		t.Fatal(err)
	}

	sidSole := "44444444-4444-4444-8444-444444444444"
	sidNoSubs := "55555555-5555-4555-8555-555555555555"

	// sidSole: strictly 1 log (in realDefCfg, reachable via both $HOME/.claude and CLAUDE_CONFIG_DIR).
	soleLog := filepath.Join(defProjA, sidSole+".jsonl")
	os.WriteFile(soleLog, []byte(`{"type":"system","subtype":"turn_duration","uuid":"sole-u","pendingBackgroundAgentCount":2}`+"\n"), 0o644)
	soleSubs := filepath.Join(defProjA, sidSole, "subagents")
	os.MkdirAll(soleSubs, 0o755)
	os.WriteFile(filepath.Join(soleSubs, "agent-solesub.jsonl"), []byte(`{"version":"2.1.305"}`+"\n"), 0o644)
	os.WriteFile(filepath.Join(soleSubs, "agent-solesub.meta.json"), []byte(`{"requestShape":"background","description":"sole sub"}`), 0o644)

	// sidNoSubs: strictly 1 log in defProjA (with NO subagents dir), while
	// altProj has an orphan subagents dir for the same SID -> must NOT pull altProj's!
	os.WriteFile(filepath.Join(defProjA, sidNoSubs+".jsonl"), []byte(`{"type":"system","subtype":"turn_duration","uuid":"nosub-u","pendingBackgroundAgentCount":0}`+"\n"), 0o644)
	orphanSubs := filepath.Join(altProj, sidNoSubs, "subagents")
	os.MkdirAll(orphanSubs, 0o755)
	os.WriteFile(filepath.Join(orphanSubs, "agent-orphansub.jsonl"), []byte(`{"version":"2.1.305"}`+"\n"), 0o644)
	os.WriteFile(filepath.Join(orphanSubs, "agent-orphansub.meta.json"), []byte(`{"requestShape":"background","description":"wrong dir sub"}`), 0o644)

	// sidOrphanOnly: NO <sid>.jsonl anywhere, only a sole orphan subagents dir ->
	// must NOT be returned (Round 4 P2-2: subagents only derived from sole log).
	sidOrphanOnly := "66666666-6666-4666-8666-666666666666"
	orphanOnlySubs := filepath.Join(defProjA, sidOrphanOnly, "subagents")
	os.MkdirAll(orphanOnlySubs, 0o755)
	os.WriteFile(filepath.Join(orphanOnlySubs, "agent-orphonly.jsonl"), []byte(`{"version":"2.1.305"}`+"\n"), 0o644)
	os.WriteFile(filepath.Join(orphanOnlySubs, "agent-orphonly.meta.json"), []byte(`{"requestShape":"background","description":"orphan only"}`), 0o644)

	// sidA: TWO logs across config dirs (different mtimes) -> ambiguous!
	defLogA := filepath.Join(defProjA, sidA+".jsonl")
	altLogA := filepath.Join(altProj, sidA+".jsonl")
	os.WriteFile(defLogA, []byte(`{"type":"system","subtype":"turn_duration","uuid":"old-uuid","pendingBackgroundAgentCount":1}`+"\n"), 0o644)
	os.WriteFile(altLogA, []byte(`{"type":"system","subtype":"turn_duration","uuid":"new-uuid","pendingBackgroundAgentCount":3}`+"\n"), 0o644)
	oldTime := time.Now().Add(-10 * time.Minute)
	newTime := time.Now().Add(-1 * time.Minute)
	os.Chtimes(defLogA, oldTime, oldTime)
	os.Chtimes(altLogA, newTime, newTime)
	altSubsA := filepath.Join(altProj, sidA, "subagents")
	os.MkdirAll(altSubsA, 0o755)
	os.WriteFile(filepath.Join(altSubsA, "agent-ambigsub.jsonl"), []byte(`{"version":"2.1.305"}`+"\n"), 0o644)
	os.WriteFile(filepath.Join(altSubsA, "agent-ambigsub.meta.json"), []byte(`{"requestShape":"background","description":"ambig"}`), 0o644)

	// sidB: TWO logs across two project dirs inside the SAME config dir -> ambiguous!
	os.WriteFile(filepath.Join(defProjA, sidB+".jsonl"), []byte(`{"type":"system","subtype":"turn_duration","uuid":"proj-a","pendingBackgroundAgentCount":1}`+"\n"), 0o644)
	os.WriteFile(filepath.Join(defProjB, sidB+".jsonl"), []byte(`{"type":"system","subtype":"turn_duration","uuid":"proj-b","pendingBackgroundAgentCount":2}`+"\n"), 0o644)

	run := func(bin, cmd string, envCfg string) []byte {
		c := exec.Command(bin, "-c", cmd)
		c.Env = []string{"HOME=" + home, "CLAUDE_CONFIG_DIR=" + envCfg, "PATH=/usr/bin:/bin"}
		out, err := c.Output()
		if err != nil {
			t.Fatalf("%s cmd failed: %v (out=%q)", bin, err, string(out))
		}
		return out
	}

	for _, binName := range []string{sh, "dash"} {
		bin, err := exec.LookPath(binName)
		if err != nil {
			continue
		}
		// Batch with sole match + ambiguous cross-dir + ambiguous cross-project:
		// must exit 0, return sidSole & sidNoSubs, omit sidA & sidB!
		got := parseBgTails(run(bin, bgTailCommand([]string{sidSole, sidNoSubs, sidA, sidB}, 4096), altCfg))
		if r, ok := got[sidSole]; !ok || r.uuid != "sole-u" || r.n != 2 {
			t.Fatalf("%s sidSole = (%+v, %v), want sole-u n=2", binName, r, ok)
		}
		if r, ok := got[sidNoSubs]; !ok || r.uuid != "nosub-u" || r.n != 0 {
			t.Fatalf("%s sidNoSubs = (%+v, %v), want nosub-u n=0", binName, r, ok)
		}
		if _, ok := got[sidA]; ok {
			t.Fatalf("%s ambiguous cross-dir sidA guessed a log: %+v", binName, got[sidA])
		}
		if _, ok := got[sidB]; ok {
			t.Fatalf("%s ambiguous cross-project sidB guessed a log: %+v", binName, got[sidB])
		}

		// Symlink dedup: CLAUDE_CONFIG_DIR == realDefCfg == readlink($HOME/.claude)
		// must still count sidSole as 1 (not 2!).
		gotDedup := parseBgTails(run(bin, bgTailCommand([]string{sidSole}, 4096), realDefCfg))
		if r, ok := gotDedup[sidSole]; !ok || r.uuid != "sole-u" || r.n != 2 {
			t.Fatalf("%s symlink dedup sidSole = (%+v, %v), want sole-u n=2", binName, r, ok)
		}

		// cmdScanCommand: sidSole returns its sibling subagent; sidNoSubs does NOT
		// pull orphanSubs from altCfg; sidOrphanOnly (no parent log) returns empty;
		// ambiguous sidA returns empty.
		subs := parseBgSubScan(string(run(bin, cmdScanCommand("w:"+sidSole, "s:"+sidNoSubs, "s:"+sidOrphanOnly, "w:"+sidA), altCfg)))
		if len(subs[sidSole]) != 1 || subs[sidSole][0].ID != "solesub" {
			t.Fatalf("%s sidSole subs = %+v, want solesub", binName, subs[sidSole])
		}
		if len(subs[sidNoSubs]) != 0 {
			t.Fatalf("%s sidNoSubs pulled subagents from another config dir: %+v", binName, subs[sidNoSubs])
		}
		if len(subs[sidOrphanOnly]) != 0 {
			t.Fatalf("%s orphan subagents without parent log should not be returned: %+v", binName, subs[sidOrphanOnly])
		}
		if len(subs[sidA]) != 0 {
			t.Fatalf("%s ambiguous sidA guessed subagents: %+v", binName, subs[sidA])
		}
	}

	// Stateless verification: pinning syntax "<sid>=<path>" is ignored by command builders.
	if cmd := bgTailCommand([]string{sidSole + "=" + soleLog}, 4096); strings.Contains(cmd, soleLog) {
		t.Fatalf("bgTailCommand must not accept pinned log paths: %s", cmd)
	}
	if cmd := cmdScanCommand("w:" + sidSole + "=" + soleLog); strings.Contains(cmd, soleLog) {
		t.Fatalf("cmdScanCommand must not accept pinned log paths: %s", cmd)
	}
}

// Round 4 P3: Uppercase session UUIDs from herdr must be normalized to
// lowercase in probe arguments and internal lookup keys so lowercase disk
// logs are found consistently.
func TestBgAndCmdUppercaseSIDNormalized(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	home := t.TempDir()
	proj := filepath.Join(home, ".claude", "projects", "-work")
	sidLower := "abcdef12-3456-4789-8abc-def012345678"
	sidUpper := strings.ToUpper(sidLower)
	subsDir := filepath.Join(proj, sidLower, "subagents")
	if err := os.MkdirAll(subsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(proj, sidLower+".jsonl")
	os.WriteFile(logPath, []byte(
		`{"type":"assistant","message":{"model":"claude-opus-5-5","content":[{"type":"text","text":"done"}]}}`+"\n"+
			`{"type":"system","subtype":"turn_duration","uuid":"u-upper","pendingBackgroundAgentCount":2}`+"\n",
	), 0o644)
	os.WriteFile(filepath.Join(subsDir, "agent-uppersub.jsonl"), []byte(`{"version":"2.1.305"}`+"\n"), 0o644)
	os.WriteFile(filepath.Join(subsDir, "agent-uppersub.meta.json"), []byte(`{"requestShape":"background","description":"upper sid sub"}`), 0o644)

	run := func(cmd string) []byte {
		c := exec.Command(sh, "-c", cmd)
		c.Env = []string{"HOME=" + home, "PATH=/usr/bin:/bin"}
		out, err := c.Output()
		if err != nil {
			t.Fatalf("cmd failed: %v", err)
		}
		return out
	}

	// 1. bgTailCommand + parseBgTails + bgProber with uppercase SID.
	bg := newBgProber(func(_ context.Context, sids []string, tail int) (map[string]bgRecord, error) {
		return parseBgTails(run(bgTailCommand(sids, tail))), nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	go bg.run(ctx)
	bg.consider("w1:p1", "claude", herdr.StatusDone, sidUpper, 1)
	if st := bg.await(ctx, "w1:p1", 1); st != bgRunning {
		t.Fatalf("bgProber uppercase SID state = %s, want running", st)
	}
	sum := herdrSummary{Workspaces: []herdrWorkspace{{Agents: []herdrAgent{
		{PaneID: "w1:p1", Agent: "claude", Status: herdr.StatusDone, Session: sidUpper},
	}}}}
	bg.decorate(&sum)
	a := sum.Workspaces[0].Agents[0]
	if a.Bg != bgRunning || a.BgN != 2 || a.Model != "claude-opus-5-5" {
		t.Fatalf("bgProber decorate uppercase SID = %+v, want running n=2 claude-opus-5-5", a)
	}

	// 2. cmdScanCommand with uppercase SID finds lowercase disk log & subagent.
	subs := parseBgSubScan(string(run(cmdScanCommand("w:" + sidUpper))))
	if len(subs[sidLower]) != 1 || subs[sidLower][0].ID != "uppersub" {
		t.Fatalf("cmdScanCommand uppercase SID subs = %+v, want uppersub under %s", subs, sidLower)
	}

	// 3. logsWroteCommand with uppercase SID finds lowercase disk log.
	lo, hi := parseLogsWrote(string(run(logsWroteCommand([]string{"claude:" + sidUpper}))), []string{"w1:p1"})
	if lo["w1:p1"] <= 0 || hi["w1:p1"] <= 0 {
		t.Fatalf("logsWroteCommand uppercase SID returned unknown time: lo=%v hi=%v", lo, hi)
	}
}

// Round 4 P2-3 & P2-1:
//   - logsWroteCommand requires strictly 1 matching log; ambiguous copies across
//     config dirs or project dirs return unknown time (empty "w=").
//   - When config dir enumeration is incomplete (e.g. unreadable custom config
//     dir or projects dir), bgTailCommand, cmdScanCommand, and logsWroteCommand
//     all refuse to treat a remaining copy as sole (exit 0, emit empty/missing).
func TestLogsWroteSoleAndIncompleteConfigDirs(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	base := t.TempDir()
	home := filepath.Join(base, "home")
	defCfg := filepath.Join(home, ".claude")
	altCfg := filepath.Join(base, "alt-claude")
	defProjA := filepath.Join(defCfg, "projects", "-a")
	defProjB := filepath.Join(defCfg, "projects", "-b")
	altProj := filepath.Join(altCfg, "projects", "-c")
	for _, d := range []string{defProjA, defProjB, altProj} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	sidUnique := "77777777-7777-4777-8777-777777777777"
	sidDupDir := "88888888-8888-4888-8888-888888888888"
	sidDupProj := "99999999-8888-4777-8666-555555555555"

	os.WriteFile(filepath.Join(defProjA, sidUnique+".jsonl"), []byte(`{"type":"system","subtype":"turn_duration","uuid":"u-uniq","pendingBackgroundAgentCount":1}`+"\n"), 0o644)
	uniqSubs := filepath.Join(defProjA, sidUnique, "subagents")
	os.MkdirAll(uniqSubs, 0o755)
	os.WriteFile(filepath.Join(uniqSubs, "agent-uniqsub.jsonl"), []byte(`{"version":"2.1.305"}`+"\n"), 0o644)
	os.WriteFile(filepath.Join(uniqSubs, "agent-uniqsub.meta.json"), []byte(`{"requestShape":"background","description":"uniq"}`), 0o644)

	// Duplicate across config dirs (newer in altCfg).
	oldCopy := filepath.Join(defProjA, sidDupDir+".jsonl")
	newCopy := filepath.Join(altProj, sidDupDir+".jsonl")
	os.WriteFile(oldCopy, []byte(`{"type":"user"}`+"\n"), 0o644)
	os.WriteFile(newCopy, []byte(`{"type":"user"}`+"\n"), 0o644)
	os.Chtimes(oldCopy, time.Now().Add(-10*time.Minute), time.Now().Add(-10*time.Minute))

	// Duplicate across two project dirs inside defCfg.
	os.WriteFile(filepath.Join(defProjA, sidDupProj+".jsonl"), []byte(`{"type":"user"}`+"\n"), 0o644)
	os.WriteFile(filepath.Join(defProjB, sidDupProj+".jsonl"), []byte(`{"type":"user"}`+"\n"), 0o644)

	runWith := func(bin, cmd, envCfg string) []byte {
		c := exec.Command(bin, "-c", cmd)
		c.Env = []string{"HOME=" + home, "CLAUDE_CONFIG_DIR=" + envCfg, "PATH=/usr/bin:/bin"}
		out, err := c.Output()
		if err != nil {
			t.Fatalf("%s cmd failed: %v (out=%q)", bin, err, string(out))
		}
		return out
	}

	// P2-3: logsWroteCommand returns mtime ONLY for sidUnique, unknown for duplicates.
	for _, binName := range []string{sh, "dash"} {
		bin, err := exec.LookPath(binName)
		if err != nil {
			continue
		}
		out := runWith(bin, logsWroteCommand([]string{"claude:" + sidUnique, "claude:" + sidDupDir, "claude:" + sidDupProj}), altCfg)
		lo, hi := parseLogsWrote(string(out), []string{"p1", "p2", "p3"})
		if lo["p1"] <= 0 || hi["p1"] <= 0 {
			t.Fatalf("%s sole log missing mtime: out=%q lo=%v hi=%v", binName, string(out), lo, hi)
		}
		if _, ok := lo["p2"]; ok {
			t.Fatalf("%s cross-dir duplicate must return unknown time, got lo=%v out=%q", binName, lo, string(out))
		}
		if _, ok := lo["p3"]; ok {
			t.Fatalf("%s cross-project duplicate must return unknown time, got lo=%v out=%q", binName, lo, string(out))
		}
	}

	// P2-1: Unreadable custom CLAUDE_CONFIG_DIR (chmod 000) marks enumeration
	// incomplete -> bgTail, cmdScan, logsWrote all refuse to trust defCfg's copy.
	if os.Geteuid() != 0 {
		lockedCfg := filepath.Join(base, "locked-claude")
		if err := os.MkdirAll(lockedCfg, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(lockedCfg, 0o000); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(lockedCfg, 0o755) })

		for _, binName := range []string{sh, "dash"} {
			bin, err := exec.LookPath(binName)
			if err != nil {
				continue
			}
			bgGot := parseBgTails(runWith(bin, bgTailCommand([]string{sidUnique}, 4096), lockedCfg))
			if len(bgGot) != 0 {
				t.Fatalf("%s incomplete scan bgTailCommand returned %+v, want empty", binName, bgGot)
			}
			subGot := parseBgSubScan(string(runWith(bin, cmdScanCommand("s:"+sidUnique), lockedCfg)))
			if len(subGot) != 0 {
				t.Fatalf("%s incomplete scan cmdScanCommand returned %+v, want empty", binName, subGot)
			}
			lo, _ := parseLogsWrote(string(runWith(bin, logsWroteCommand([]string{"claude:" + sidUnique}), lockedCfg)), []string{"p1"})
			if len(lo) != 0 {
				t.Fatalf("%s incomplete scan logsWroteCommand returned %+v, want unknown", binName, lo)
			}
		}

		// Also verify unreadable projects/ subdir inside a readable config dir.
		lockedProjCfg := filepath.Join(base, "locked-proj-claude")
		lockedProjDir := filepath.Join(lockedProjCfg, "projects")
		if err := os.MkdirAll(lockedProjDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(lockedProjDir, 0o000); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(lockedProjDir, 0o755) })

		bgGot := parseBgTails(runWith(sh, bgTailCommand([]string{sidUnique}, 4096), lockedProjCfg))
		if len(bgGot) != 0 {
			t.Fatalf("unreadable projects/ dir bgTailCommand returned %+v, want empty", bgGot)
		}
	}
}

// Round 3: emit_claude must NOT blindly take the first glob match when multiple
// project directories under the same config dir hold <sid>.jsonl. It should
// confirm via the process's open /proc/<pid>/fd/* when unambiguous, and leave
// log empty when still ambiguous (both full scan and fast tree scan).
func TestEmitClaudeDisambiguatesByOpenFD(t *testing.T) {
	if _, err := os.Stat("/proc/self/stat"); err != nil {
		t.Skip("no /proc")
	}
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	base := t.TempDir()
	work := filepath.Join(base, "work")
	home := filepath.Join(base, "home")
	realCfg := filepath.Join(base, "real-claude")
	sessDir := filepath.Join(realCfg, "sessions")
	projA := filepath.Join(realCfg, "projects", "-a-archived")
	projB := filepath.Join(realCfg, "projects", "-b-active")
	for _, d := range []string{work, home, sessDir, projA, projB} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(realCfg, filepath.Join(home, ".claude")); err != nil {
		t.Fatal(err)
	}
	logA := filepath.Join(projA, sidA+".jsonl")
	logB := filepath.Join(projB, sidA+".jsonl")
	os.WriteFile(logA, []byte(`{"type":"user","cwd":"/a"}`+"\n"), 0o644)
	os.WriteFile(logB, []byte(`{"type":"user","cwd":"/b"}`+"\n"), 0o644)

	spawnWithFDs := func(pane string, openPaths ...string) (shellPID, childPID int, start string) {
		binDir := t.TempDir()
		herdrBin := filepath.Join(binDir, "herdr")
		if err := os.Symlink(sh, herdrBin); err != nil {
			t.Fatal(err)
		}
		pidFile := filepath.Join(binDir, "pids.txt")
		var sb strings.Builder
		sb.WriteString("echo \"shell=$$\" > " + strconv.Quote(pidFile) + "\n")
		sb.WriteString("(\n")
		sb.WriteString("export HERDR_PANE_ID=" + shellSingleQuote(pane) + "\n")
		sb.WriteString("export HERDR_SOCKET_PATH=/run/herdr.sock\n")
		for i, p := range openPaths {
			sb.WriteString(fmt.Sprintf("exec %d< %s\n", 7+i, shellSingleQuote(p)))
		}
		sb.WriteString("exec sleep 60\n) &\n")
		sb.WriteString("echo \"child=$!\" >> " + strconv.Quote(pidFile) + "\n")
		sb.WriteString("echo done >> " + strconv.Quote(pidFile) + "\nwait\n")
		outer := exec.Command(herdrBin, "-c", sh+" -c "+shellSingleQuote(sb.String())+" & wait")
		outer.Dir = work
		outer.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + home}
		if err := outer.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if childPID > 0 {
				if pr, err := os.FindProcess(childPID); err == nil {
					_ = pr.Kill()
				}
			}
			if shellPID > 0 {
				if pr, err := os.FindProcess(shellPID); err == nil {
					_ = pr.Kill()
				}
			}
			_ = outer.Process.Kill()
			_ = outer.Wait()
		})
		for i := 0; i < 100; i++ {
			if b, err := os.ReadFile(pidFile); err == nil && strings.Contains(string(b), "done\n") {
				for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
					if v, ok := strings.CutPrefix(line, "shell="); ok {
						shellPID, _ = strconv.Atoi(v)
					} else if v, ok := strings.CutPrefix(line, "child="); ok {
						childPID, _ = strconv.Atoi(v)
					}
				}
				break
			}
			time.Sleep(15 * time.Millisecond)
		}
		for try := 0; try < 50 && start == ""; try++ {
			b, _ := os.ReadFile("/proc/" + strconv.Itoa(childPID) + "/stat")
			if j := strings.LastIndex(string(b), ") "); j >= 0 {
				if f := strings.Fields(string(b)[j+2:]); len(f) > 19 {
					start = f[19]
				}
			}
			if start == "" {
				time.Sleep(10 * time.Millisecond)
			}
		}
		os.WriteFile(filepath.Join(sessDir, strconv.Itoa(childPID)+".json"),
			[]byte(`{"pid":`+strconv.Itoa(childPID)+`,"sessionId":"`+sidA+`","procStart":"`+start+`"}`), 0o644)
		return shellPID, childPID, start
	}

	// Case 1: No open log fd -> ambiguous across -a-archived and -b-active -> log must be "".
	sh1, pid1, _ := spawnWithFDs("w1:p1")
	// Case 2: Open fd to -b-active (even though -a-archived sorts first in glob, and $HOME/.claude is a symlink!) -> confirms logB!
	sh2, pid2, _ := spawnWithFDs("w1:p2", logB)
	// Case 3: Open fds to BOTH logA and logB -> still ambiguous -> log must be "".
	sh3, pid3, _ := spawnWithFDs("w1:p3", logA, logB)

	check := func(name, cmd string, wantPID int, wantLog string) {
		c := exec.Command(sh, "-c", cmd)
		c.Env = []string{"HOME=" + home, "PATH=/usr/bin:/bin"}
		out, err := c.Output()
		if err != nil {
			t.Fatalf("%s failed: %v", name, err)
		}
		loc := parseConvoLocate(string(out))
		var found *convoProc
		for i := range loc.procs {
			if loc.procs[i].pid == int64(wantPID) {
				found = &loc.procs[i]
				break
			}
		}
		if found == nil {
			t.Fatalf("%s: pid %d not found in %+v", name, wantPID, loc.procs)
		}
		if found.log != wantLog {
			t.Fatalf("%s: log = %q, want %q", name, found.log, wantLog)
		}
	}

	// Verify both fast tree scan and full scan agree on all 3 cases!
	check("tree-none-open", convoLocatePidsCommand(sh1, []int{pid1}), pid1, "")
	check("tree-fd-confirms-b", convoLocatePidsCommand(sh2, []int{pid2}), pid2, logB)
	check("tree-both-open", convoLocatePidsCommand(sh3, []int{pid3}), pid3, "")

	fullCmd := convoLocateCommand("herdr", "", "")
	check("full-none-open", fullCmd, pid1, "")
	check("full-fd-confirms-b", fullCmd, pid2, logB)
	check("full-both-open", fullCmd, pid3, "")
}

// Round 3 Item 4: codexTmuxNamed must claim under convoClaimHostKey(sess) (never
// hostless "") so a tmux named Codex claim on Host A does not leak to Host B's
// refresh checks or liveness updates.
func TestCodexTmuxNamedClaimScopedByHost(t *testing.T) {
	h := newHarness(t)
	admin := h.admin()

	codexLogA := "/home/u/.codex/sessions/2026/11/27/rollout-2026-11-27T01-00-00-" + sidA + ".jsonl"
	paneRec := strings.Join([]string{
		"%1", "300", "700", "1000", "/work",
		sidA, codexLogA,
		"my-thread", "", "",
		"1", "0", "db", "0", "=", "0", "my-thread | work",
	}, "\x1f")

	// Open a tmux session on Host A and a separate session on Host B.
	srvA := h.sshServer(t.TempDir())
	srvA.HandleExec(func(cmd string) func(ssh.Channel) {
		if strings.Contains(cmd, "python3") && strings.Contains(cmd, "tmux") {
			return sshExecReply(paneRec+"\x00", 0)
		}
		return nil
	})
	r := admin.do("POST", "/api/hosts", map[string]any{"name": "tmux-a", "address": "127.0.0.1", "port": srvA.Addr.Port,
		"username": "alice", "auth": "password", "password": "alice-pass"})
	mustStatus(t, r, 201, "create host A")
	r = admin.do("POST", "/api/sessions", map[string]any{"host_id": int64(r.json()["id"].(float64))})
	mustStatus(t, r, 201, "open session A")
	sessA, err := h.terms.Get(1, r.json()["id"].(string))
	if err != nil {
		t.Fatal(err)
	}

	srvB := h.sshServer(t.TempDir())
	r = admin.do("POST", "/api/hosts", map[string]any{"name": "box-b", "address": "127.0.0.1", "port": srvB.Addr.Port,
		"username": "alice", "auth": "password", "password": "alice-pass"})
	mustStatus(t, r, 201, "create host B")
	r = admin.do("POST", "/api/sessions", map[string]any{"host_id": int64(r.json()["id"].(float64))})
	mustStatus(t, r, 201, "open session B")
	sessB, err := h.terms.Get(1, r.json()["id"].(string))
	if err != nil {
		t.Fatal(err)
	}

	hostA := convoClaimHostKey(sessA)
	hostB := convoClaimHostKey(sessB)
	if hostA == "" || hostB == "" || hostA == hostB {
		t.Fatalf("invalid host keys: hostA=%q hostB=%q", hostA, hostB)
	}

	p := &convoProc{pid: 300, start: 700, started: 1000, agent: "codex", tmuxPane: "%1", cwd: "/work"}
	l := convoLocated{
		procs:   []convoProc{*p},
		cands:   []convoCand{{log: codexLogA, cwd: "/work", began: 1000, wrote: 2000}},
		hostNow: 2000,
	}
	got, ok := h.srv.codexTmuxNamed(context.Background(), sessA, p, l)
	if !ok || got.Thread != sidA {
		t.Fatalf("codexTmuxNamed = (%+v, %v), want thread %s", got, ok, sidA)
	}

	// Claim must belong strictly to hostA — NOT hostless "" and NOT hostB.
	if !h.srv.convoClaimed.ownsOn(hostA, sidA, p.id()) {
		t.Fatal("hostA should own sidA after codexTmuxNamed")
	}
	if h.srv.convoClaimed.ownsOn("", sidA, p.id()) {
		t.Fatal("codexTmuxNamed created a hostless claim")
	}
	if h.srv.convoClaimed.ownsOn(hostB, sidA, p.id()) {
		t.Fatal("hostB must not see hostA's codexTmuxNamed claim")
	}
	if !h.srv.convoClaimed.needsAliveRefresh(hostA, time.Now(), convoLocateTTL) {
		t.Fatal("hostA should need refresh after codexTmuxNamed claim")
	}
	if h.srv.convoClaimed.needsAliveRefresh(hostB, time.Now(), convoLocateTTL) {
		t.Fatal("hostA's codexTmuxNamed claim leaked into hostB's needsAliveRefresh")
	}

	// Even if Host B scans an identical rollout UUID + procID, it must not update
	// Host A's claim seen timestamp or lastAlive!
	h.srv.convoClaimed.seeAliveOn(hostB, l, map[procID]bool{p.id(): true}, 999999)
	if !h.srv.convoClaimed.needsAliveRefresh(hostA, time.Now(), convoLocateTTL) {
		t.Fatal("hostB seeAliveOn suppressed hostA's refresh")
	}
	h.srv.convoClaimed.mu.Lock()
	clA := h.srv.convoClaimed.by[convoClaimKey(hostA, sidA)]
	h.srv.convoClaimed.mu.Unlock()
	if clA.seen == 999999 {
		t.Fatalf("hostB seeAliveOn mutated hostA's claim seen: %+v", clA)
	}
}

func TestConvoLocatePidsAndCustomConfigDirHost(t *testing.T) {
	if _, err := os.Stat("/proc/self/stat"); err != nil {
		t.Skip("no /proc")
	}
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}

	home := t.TempDir()
	workDir := filepath.Join(home, "work dir")
	absCustom := filepath.Join(home, "my cfg", ".claude-work")
	relCustomName := filepath.Join("sub cfg", ".claude-rel")
	relCustom := filepath.Join(workDir, relCustomName)
	defaultCfg := filepath.Join(home, ".claude")

	for _, dir := range []string{
		filepath.Join(absCustom, "sessions"),
		filepath.Join(absCustom, "projects", "-work"),
		filepath.Join(relCustom, "sessions"),
		filepath.Join(relCustom, "projects", "-work"),
		filepath.Join(defaultCfg, "sessions"),
		filepath.Join(defaultCfg, "projects", "-work"),
	} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	sid1 := "11111111-1111-4111-8111-111111111111"
	sid2 := "22222222-2222-4222-8222-222222222222"
	sid3 := "33333333-3333-4333-8333-333333333333"

	// Spawn 3 independent single-pane herdr trees:
	// 1. Absolute CLAUDE_CONFIG_DIR with spaces.
	shellAbs, pidsAbs, stsAbs := spawnFakeHerdrTree(t, workDir, home, [][]string{{
		"CLAUDE_CONFIG_DIR=" + absCustom,
		"HERDR_PANE_ID=w1:p1",
		"HERDR_SOCKET_PATH=/run/herdr.sock",
	}})
	pidAbs, stAbs := pidsAbs[0], stsAbs[0]
	logAbs := filepath.Join(absCustom, "projects", "-work", sid1+".jsonl")
	os.WriteFile(filepath.Join(absCustom, "sessions", strconv.Itoa(pidAbs)+".json"),
		[]byte(`{"pid":`+strconv.Itoa(pidAbs)+`,"sessionId":"`+sid1+`","procStart":"`+stAbs+`","status":"waiting","waitingFor":"dialog open"}`), 0o644)
	os.WriteFile(logAbs, []byte(`{"type":"system","subtype":"turn_duration","uuid":"u1","pendingBackgroundAgentCount":2}`+"\n"), 0o644)

	// 2. Relative CLAUDE_CONFIG_DIR with spaces (resolved against /proc/<pid>/cwd).
	shellRel, pidsRel, stsRel := spawnFakeHerdrTree(t, workDir, home, [][]string{{
		"CLAUDE_CONFIG_DIR=" + relCustomName,
		"HERDR_PANE_ID=w1:p2",
		"HERDR_SOCKET_PATH=/run/herdr.sock",
	}})
	pidRel, stRel := pidsRel[0], stsRel[0]
	logRel := filepath.Join(relCustom, "projects", "-work", sid2+".jsonl")
	os.WriteFile(filepath.Join(relCustom, "sessions", strconv.Itoa(pidRel)+".json"),
		[]byte(`{"pid":`+strconv.Itoa(pidRel)+`,"sessionId":"`+sid2+`","procStart":"`+stRel+`","status":"idle"}`), 0o644)
	os.WriteFile(logRel, []byte(`{"type":"user"}`+"\n"), 0o644)

	// 3. Default $HOME/.claude (unset CLAUDE_CONFIG_DIR, with a forged CLAUDE_CONFIG_DIR inside OTHER).
	shellDef, pidsDef, stsDef := spawnFakeHerdrTree(t, workDir, home, [][]string{{
		"OTHER=foo\nCLAUDE_CONFIG_DIR=" + absCustom,
		"HERDR_PANE_ID=w1:p3",
		"HERDR_SOCKET_PATH=/run/herdr.sock",
	}})
	pidDef, stDef := pidsDef[0], stsDef[0]
	logDef := filepath.Join(defaultCfg, "projects", "-work", sid3+".jsonl")
	os.WriteFile(filepath.Join(defaultCfg, "sessions", strconv.Itoa(pidDef)+".json"),
		[]byte(`{"pid":`+strconv.Itoa(pidDef)+`,"sessionId":"`+sid3+`","procStart":"`+stDef+`"}`), 0o644)
	os.WriteFile(logDef, []byte(`{"type":"user"}`+"\n"), 0o644)

	runTree := func(shPID, pid int) convoLocated {
		c := exec.Command(sh, "-c", convoLocatePidsCommand(shPID, []int{pid}))
		c.Env = []string{"HOME=" + home, "PATH=/usr/bin:/bin"}
		out, err := c.Output()
		if err != nil {
			t.Fatalf("convoLocatePidsCommand failed: %v", err)
		}
		return parseConvoLocate(string(out))
	}

	// Pane 1 (abs custom with spaces): finds ONLY pidAbs, does not scan pidRel/pidDef.
	lAbs := runTree(shellAbs, pidAbs)
	if lAbs.fastIncomplete || len(lAbs.procs) != 1 || lAbs.procs[0].pid != int64(pidAbs) || lAbs.procs[0].sid != sid1 || lAbs.procs[0].log != logAbs {
		t.Fatalf("abs custom tree mismatch: %+v (want log %q)", lAbs, logAbs)
	}

	// Pane 2 (rel custom with spaces): resolves against /proc/<pid>/cwd.
	lRel := runTree(shellRel, pidRel)
	if lRel.fastIncomplete || len(lRel.procs) != 1 || lRel.procs[0].pid != int64(pidRel) || lRel.procs[0].sid != sid2 || lRel.procs[0].log != logRel {
		t.Fatalf("rel custom tree mismatch: %+v (want log %q)", lRel, logRel)
	}

	// Pane 3 (forged CLAUDE_CONFIG_DIR inside OTHER): falls back to $HOME/.claude.
	lDef := runTree(shellDef, pidDef)
	if lDef.fastIncomplete || len(lDef.procs) != 1 || lDef.procs[0].pid != int64(pidDef) || lDef.procs[0].sid != sid3 || lDef.procs[0].log != logDef {
		t.Fatalf("default/forged tree mismatch: %+v (want log %q)", lDef, logDef)
	}

	// freshStatusCommand reads from the process's custom CLAUDE_CONFIG_DIR (with spaces).
	cFresh := exec.Command(sh, "-c", freshStatusCommand(int64(pidAbs)))
	cFresh.Env = []string{"HOME=" + home, "PATH=/usr/bin:/bin"}
	outFresh, err := cFresh.Output()
	if err != nil {
		t.Fatalf("freshStatusCommand failed: %v", err)
	}
	stAbsNum, _ := strconv.ParseInt(stAbs, 10, 64)
	if got := procStatusOf(outFresh, &convoBind{PID: int64(pidAbs), Start: stAbsNum, Session: sid1}); got != "waiting:dialog open" {
		t.Fatalf("freshStatusCommand custom dir = %q, want waiting:dialog open", got)
	}

	// lineAliveCommand reads sessionId from the process's relative custom CLAUDE_CONFIG_DIR.
	stRelNum, _ := strconv.ParseInt(stRel, 10, 64)
	cAlive := exec.Command(sh, "-c", lineAliveCommand(int64(pidRel), stRelNum))
	cAlive.Env = []string{"HOME=" + home, "PATH=/usr/bin:/bin"}
	outAlive, err := cAlive.Output()
	if err != nil {
		t.Fatalf("lineAliveCommand failed: %v", err)
	}
	if got := strings.TrimSpace(string(outAlive)); got != sid2 {
		t.Fatalf("lineAliveCommand custom dir = %q, want %q", got, sid2)
	}

	// bgTailCommand, forkFindCommand, logsWroteCommand, cmdScanCommand with spaces.
	// Isolate claude_cfg_dirs's /proc scan so unrelated host Claude/Node processes
	// or concurrent tests under the same user cannot interfere with this test.
	fakeProc := t.TempDir()
	cBg := exec.Command(sh, "-c", isolateClaudeCfgProc(t, bgTailCommand([]string{sid1}, 4096), fakeProc))
	cBg.Env = []string{"HOME=" + home, "CLAUDE_CONFIG_DIR=" + absCustom, "PATH=/usr/bin:/bin"}
	outBg, err := cBg.Output()
	if err != nil {
		t.Fatalf("bgTailCommand failed: %v", err)
	}
	if r, ok := parseBgTails(outBg)[sid1]; !ok || r.uuid != "u1" || r.n != 2 {
		t.Fatalf("bgTailCommand custom dir = %+v, want uuid=u1 n=2", r)
	}

	cFork := exec.Command(sh, "-c", isolateClaudeCfgProc(t, forkFindCommand("claude", sid1), fakeProc))
	cFork.Env = []string{"HOME=" + home, "CLAUDE_CONFIG_DIR=" + absCustom, "PATH=/usr/bin:/bin"}
	outFork, err := cFork.Output()
	if err != nil {
		t.Fatalf("forkFindCommand failed: %v", err)
	}
	if got := strings.TrimSpace(string(outFork)); got != logAbs {
		t.Fatalf("forkFindCommand custom dir = %q, want %q", got, logAbs)
	}

	cWrote := exec.Command(sh, "-c", isolateClaudeCfgProc(t, logsWroteCommand([]string{"claude:" + sid1}), fakeProc))
	cWrote.Env = []string{"HOME=" + home, "CLAUDE_CONFIG_DIR=" + absCustom, "PATH=/usr/bin:/bin"}
	outWrote, err := cWrote.Output()
	if err != nil {
		t.Fatalf("logsWroteCommand failed: %v", err)
	}
	lo, _ := parseLogsWrote(string(outWrote), []string{"w1:p1"})
	if lo["w1:p1"] <= 0 {
		t.Fatalf("logsWroteCommand custom dir returned no mtime: %q", string(outWrote))
	}

	subDir := filepath.Join(absCustom, "projects", "-work", sid1, "subagents")
	if err := os.MkdirAll(subDir, 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(subDir, "agent-abc123.jsonl"), []byte(`{"version":"2.1.305"}`+"\n"), 0o644)
	os.WriteFile(filepath.Join(subDir, "agent-abc123.meta.json"), []byte(`{"requestShape":"background","description":"custom cfg subagent"}`), 0o644)
	cCmdScan := exec.Command(sh, "-c", isolateClaudeCfgProc(t, cmdScanCommand("w:"+sid1), fakeProc))
	cCmdScan.Env = []string{"HOME=" + home, "CLAUDE_CONFIG_DIR=" + absCustom, "PATH=/usr/bin:/bin"}
	outCmdScan, err := cCmdScan.Output()
	if err != nil {
		t.Fatalf("cmdScanCommand failed: %v", err)
	}
	subs := parseBgSubScan(string(outCmdScan))
	if len(subs[sid1]) != 1 || subs[sid1][0].ID != "abc123" || subs[sid1][0].What != "custom cfg subagent" {
		t.Fatalf("cmdScanCommand custom dir subs = %+v, want abc123", subs)
	}
}

// Regression: a zombie (Z) or dead (X) Claude/versioned process in /proc has
// no readable /proc/<pid>/exe or /cwd while /proc/<pid> still exists.
// claude_cfg_dirs must skip zombie/dead processes instead of setting
// cldirs_incomplete=1 (which would cause bgTailCommand to report missing).
func TestBgTailIgnoresZombieClaudeProcessInProc(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	home := t.TempDir()
	customCfg := filepath.Join(home, "custom-claude")
	projDir := filepath.Join(customCfg, "projects", "-work")
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatal(err)
	}
	sid := "11111111-1111-4111-8111-111111111111"
	logPath := filepath.Join(projDir, sid+".jsonl")
	if err := os.WriteFile(logPath, []byte(`{"type":"system","subtype":"turn_duration","uuid":"u-z","pendingBackgroundAgentCount":1}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	fakeProc := t.TempDir()
	// Simulate a zombie versioned Claude Code binary (comm=2.1.105, state=Z, no exe symlink).
	zDir := filepath.Join(fakeProc, "42424")
	if err := os.MkdirAll(zDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(zDir, "comm"), []byte("2.1.105\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(zDir, "stat"), []byte("42424 (2.1.105) Z 1 42424 42424 0 -1 4194304 0 0 0 0 0 0 0 0 20 0 1 0 12345 0 0\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	for _, binName := range []string{sh, "dash"} {
		bin, err := exec.LookPath(binName)
		if err != nil {
			continue
		}
		cmd := exec.Command(bin, "-c", isolateClaudeCfgProc(t, bgTailCommand([]string{sid}, 4096), fakeProc))
		cmd.Env = []string{"HOME=" + home, "CLAUDE_CONFIG_DIR=" + customCfg, "PATH=/usr/bin:/bin"}
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("%s bgTailCommand failed: %v", binName, err)
		}
		if r, ok := parseBgTails(out)[sid]; !ok || r.uuid != "u-z" || r.n != 1 {
			t.Fatalf("%s bgTailCommand with zombie process in /proc = %+v (out=%q), want uuid=u-z n=1", binName, r, string(out))
		}
	}
}

func TestClaudeRewindCheckCustomConfigDir(t *testing.T) {
	binDir, homeDir, fakeProc, _ := setupFakeRewindHost(t, 5555, 12345, sidA, "w1:p1", "/run/herdr.sock")
	os.Remove(filepath.Join(homeDir, ".claude", "sessions", "5555.json"))

	customDir := filepath.Join(homeDir, "my cfg", ".claude-rewind")
	if err := os.MkdirAll(filepath.Join(customDir, "sessions"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(customDir, "sessions", "5555.json"),
		[]byte(`{"sessionId":"`+sidA+`","procStart":12345}`), 0o644); err != nil {
		t.Fatal(err)
	}
	pdir := filepath.Join(fakeProc, "5555")
	envBytes := "HERDR_PANE_ID=w1:p1\x00HERDR_SOCKET_PATH=/run/herdr.sock\x00CLAUDE_CONFIG_DIR=" + customDir + "\x00"
	if err := os.WriteFile(filepath.Join(pdir, "environ"), []byte(envBytes), 0o644); err != nil {
		t.Fatal(err)
	}

	script := claudeRewindCheckSh("herdr", "w1:p1", "/run/herdr.sock", "", 5555, 12345, sidA)
	if out, code := runRewindSh(t, script, binDir, homeDir, fakeProc); code != 0 {
		t.Fatalf("claudeRewindCheckSh with custom CLAUDE_CONFIG_DIR exited %d (out=%q), want 0", code, out)
	}
}

func TestConvoGatedCommandCustomConfigDir(t *testing.T) {
	if _, err := os.Stat("/proc/self/stat"); err != nil {
		t.Skip("no /proc")
	}
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("no python3")
	}
	dir := t.TempDir()
	customCfg := filepath.Join(dir, "my cfg", ".claude-gated")
	if err := os.MkdirAll(filepath.Join(customCfg, "sessions"), 0o755); err != nil {
		t.Fatal(err)
	}
	typed := filepath.Join(dir, "typed.txt")
	screenFile := filepath.Join(dir, "screen.txt")
	os.WriteFile(screenFile, []byte("────────\n❯ \n────────\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "herdr"), []byte("#!/bin/sh\nif [ \"$1 $2\" = 'pane read' ]; then cat "+screenFile+"; exit 0; fi\nprintf '%s\\n' \"$*\" >>"+typed+"\n"), 0o755)

	helper := exec.Command("python3", "-c", `import pty, os, sys, time
pid, fd = pty.fork()
if pid == 0:
    os.environ["HERDR_PANE_ID"] = "p1"
    os.environ["CLAUDE_CONFIG_DIR"] = sys.argv[1]
    os.execvp("sleep", ["sleep", "15"])
print(pid, flush=True)
time.sleep(15)`, customCfg)
	out, _ := helper.StdoutPipe()
	if err := helper.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { helper.Process.Kill(); helper.Wait() })
	b := make([]byte, 64)
	n, rerr := out.Read(b)
	if rerr != nil {
		t.Fatal(rerr)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b[:n])))
	if err != nil {
		t.Fatalf("bad pid %q: %v", string(b[:n]), err)
	}
	var start int64
	for i := 0; i < 50 && start == 0; i++ {
		if stat, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat"); err == nil {
			if j := strings.LastIndex(string(stat), ") "); j >= 0 {
				if f := strings.Fields(string(stat)[j+2:]); len(f) > 19 {
					start, _ = strconv.ParseInt(f[19], 10, 64)
				}
			}
		}
		if start == 0 {
			time.Sleep(10 * time.Millisecond)
		}
	}
	os.WriteFile(filepath.Join(customCfg, "sessions", strconv.Itoa(pid)+".json"),
		[]byte(`{"pid":`+strconv.Itoa(pid)+`,"sessionId":"`+sidA+`","procStart":"`+strconv.FormatInt(start, 10)+`","status":"idle"}`), 0o644)

	cmd := convoGatedCommand("idle", "herdr", "", "p1", "/run/x.sock", int64(pid), start, sidA, 0, []string{"enter"}, 0)
	c := exec.Command("sh", "-c", cmd)
	c.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"), "XDG_RUNTIME_DIR="+dir, "HOME="+dir)
	if outBytes, err := c.CombinedOutput(); err != nil {
		t.Fatalf("convoGatedCommand with custom CLAUDE_CONFIG_DIR failed: %v (out=%q)", err, string(outBytes))
	}
	got, _ := os.ReadFile(typed)
	if strings.TrimSpace(string(got)) != "pane send-keys p1 enter" {
		t.Fatalf("typed = %q, want %q", strings.TrimSpace(string(got)), "pane send-keys p1 enter")
	}
}
