package web

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/pkg/sftp"

	"github.com/ihainan/corral/internal/herdr"
	"github.com/ihainan/corral/internal/sftplite"
)

func TestPlanHerdrFastLocate(t *testing.T) {
	hpClaude := herdr.Pane{ID: "w1:p1", Agent: "claude"}
	hpOC := herdr.Pane{ID: "w1:p1", Agent: "opencode"}

	// Nil result (e.g. older herdr without pane.process_info) falls back to full scan,
	// or OpenCode-only when snapshot says OpenCode.
	if got := planHerdrFastLocate(nil, "w1:p1", hpClaude, true); got.mode != herdrFastFull {
		t.Fatalf("nil pi for claude: got %+v, want full", got)
	}
	if got := planHerdrFastLocate(nil, "w1:p1", hpOC, true); got.mode != herdrFastOpenCode {
		t.Fatalf("nil pi for opencode: got %+v, want opencode", got)
	}

	// Mismatched pane_id falls back.
	mismatch := &herdr.PaneProcessInfoResult{
		PaneID:              "w1:p9",
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

	// Agent replacing the shell via `exec claude` (PID == ShellPID, Name == claude) must still use fast PIDs.
	execClaude := &herdr.PaneProcessInfoResult{
		PaneID:              "w1:p1",
		ShellPID:            50,
		ForegroundProcesses: []herdr.ProcessEntry{{PID: 50, Name: "claude"}},
	}
	if got := planHerdrFastLocate(execClaude, "w1:p1", hpClaude, true); got.mode != herdrFastPIDs || !slices.Equal(got.pids, []int{50}) {
		t.Fatalf("exec claude: got %+v, want pids [50]", got)
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
	if got := planHerdrFastLocate(normal, "w1:p1", hpClaude, true); got.mode != herdrFastPIDs || !slices.Equal(got.pids, []int{200, 300}) {
		t.Fatalf("normal fg: got %+v, want pids [200 300]", got)
	}

	// OpenCode detected by Name or Argv[0] switches directly to OpenCode-only scan.
	ocByName := &herdr.PaneProcessInfoResult{
		PaneID:              "w1:p1",
		ForegroundProcesses: []herdr.ProcessEntry{{PID: 400, Name: "opencode"}},
	}
	if got := planHerdrFastLocate(ocByName, "w1:p1", herdr.Pane{}, false); got.mode != herdrFastOpenCode {
		t.Fatalf("opencode by name: got %+v, want opencode", got)
	}
	ocByArgv := &herdr.PaneProcessInfoResult{
		PaneID:              "w1:p1",
		ForegroundProcesses: []herdr.ProcessEntry{{PID: 401, Name: "bun", Argv: []string{"/usr/local/bin/opencode"}}},
	}
	if got := planHerdrFastLocate(ocByArgv, "w1:p1", herdr.Pane{}, false); got.mode != herdrFastOpenCode {
		t.Fatalf("opencode by argv: got %+v, want opencode", got)
	}

	// Too many foreground PIDs falls back to full scan.
	many := &herdr.PaneProcessInfoResult{PaneID: "w1:p1"}
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

	// Herdr reports a different session want -> must fall back so a sibling holding want isn't missed.
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

	// Codex with its own open rollout log -> accepted; daemon-held Codex (log=="") -> must fall back.
	codexOwn := convoProc{pid: 300, start: 700, agent: "codex", sid: sidA, log: "/home/u/.codex/sessions/2026/rollout-1-" + sidA + ".jsonl", herdrPane: "w1:p1", herdrSocket: sock, anc: shellAnc}
	if p, ok := fastLocateAccept(convoLocated{procs: []convoProc{codexOwn}}, "w1:p1", sock, ""); !ok || p == nil || p.pid != 300 {
		t.Fatalf("codex with open log: got p=%+v ok=%v", p, ok)
	}
	codexDaemon := codexOwn
	codexDaemon.log = ""
	if _, ok := fastLocateAccept(convoLocated{procs: []convoProc{codexDaemon}}, "w1:p1", sock, ""); ok {
		t.Fatal("daemon-held codex (empty log) should fall back")
	}

	// OpenCode and Devin must fall back.
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
	// Forged inside another variable's value only -> empty.
	forged := []byte("OTHER=x\nCLAUDE_CONFIG_DIR=/forged\x00PATH=/usr/bin\x00")
	if got := parseClaudeConfigDir(forged); got != "" {
		t.Fatalf("forged CLAUDE_CONFIG_DIR = %q, want empty", got)
	}
	// Embedded newline in CLAUDE_CONFIG_DIR -> rejected.
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

	// claudeConfigDirFromLog: default, custom, spaces, non-claude rejected.
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

	startSleep := func(cwd string, env ...string) (int, string) {
		p := exec.Command("sleep", "60")
		p.Dir = cwd
		p.Env = append([]string{"PATH=/usr/bin:/bin", "HOME=" + home}, env...)
		if err := p.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { p.Process.Kill(); p.Wait() })
		pid := p.Process.Pid
		var st string
		for i := 0; i < 50 && st == ""; i++ {
			b, _ := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
			if j := strings.LastIndex(string(b), ") "); j >= 0 {
				if f := strings.Fields(string(b)[j+2:]); len(f) > 19 {
					st = f[19]
				}
			}
			if st == "" {
				time.Sleep(10 * time.Millisecond)
			}
		}
		if st == "" {
			t.Fatalf("could not read /proc/%d/stat", pid)
		}
		return pid, st
	}

	sid1 := "11111111-1111-4111-8111-111111111111"
	sid2 := "22222222-2222-4222-8222-222222222222"
	sid3 := "33333333-3333-4333-8333-333333333333"

	// 1. Absolute CLAUDE_CONFIG_DIR with spaces.
	pidAbs, stAbs := startSleep(workDir,
		"CLAUDE_CONFIG_DIR="+absCustom,
		"HERDR_PANE_ID=w1:p1",
		"HERDR_SOCKET_PATH=/run/herdr.sock",
	)
	logAbs := filepath.Join(absCustom, "projects", "-work", sid1+".jsonl")
	os.WriteFile(filepath.Join(absCustom, "sessions", strconv.Itoa(pidAbs)+".json"),
		[]byte(`{"pid":`+strconv.Itoa(pidAbs)+`,"sessionId":"`+sid1+`","procStart":"`+stAbs+`","status":"waiting","waitingFor":"dialog open"}`), 0o644)
	os.WriteFile(logAbs, []byte(`{"type":"system","subtype":"turn_duration","uuid":"u1","pendingBackgroundAgentCount":2}`+"\n"), 0o644)

	// 2. Relative CLAUDE_CONFIG_DIR with spaces (resolved against /proc/<pid>/cwd).
	pidRel, stRel := startSleep(workDir,
		"CLAUDE_CONFIG_DIR="+relCustomName,
		"HERDR_PANE_ID=w1:p2",
		"HERDR_SOCKET_PATH=/run/herdr.sock",
	)
	logRel := filepath.Join(relCustom, "projects", "-work", sid2+".jsonl")
	os.WriteFile(filepath.Join(relCustom, "sessions", strconv.Itoa(pidRel)+".json"),
		[]byte(`{"pid":`+strconv.Itoa(pidRel)+`,"sessionId":"`+sid2+`","procStart":"`+stRel+`","status":"idle"}`), 0o644)
	os.WriteFile(logRel, []byte(`{"type":"user"}`+"\n"), 0o644)

	// 3. Default $HOME/.claude (unset CLAUDE_CONFIG_DIR, with a forged CLAUDE_CONFIG_DIR inside OTHER).
	pidDef, stDef := startSleep(workDir,
		"OTHER=foo\nCLAUDE_CONFIG_DIR="+absCustom,
		"HERDR_PANE_ID=w1:p3",
		"HERDR_SOCKET_PATH=/run/herdr.sock",
	)
	logDef := filepath.Join(defaultCfg, "projects", "-work", sid3+".jsonl")
	os.WriteFile(filepath.Join(defaultCfg, "sessions", strconv.Itoa(pidDef)+".json"),
		[]byte(`{"pid":`+strconv.Itoa(pidDef)+`,"sessionId":"`+sid3+`","procStart":"`+stDef+`"}`), 0o644)
	os.WriteFile(logDef, []byte(`{"type":"user"}`+"\n"), 0o644)

	// Fast path scoped only to [pidAbs, pidRel] must find both with their custom logs,
	// and must NOT scan pidDef.
	c := exec.Command(sh, "-c", convoLocatePidsCommand([]int{pidAbs, pidRel}))
	c.Env = []string{"HOME=" + home, "PATH=/usr/bin:/bin"}
	out, err := c.Output()
	if err != nil {
		t.Fatalf("convoLocatePidsCommand failed: %v", err)
	}
	by := map[int64]convoProc{}
	for _, p := range parseConvoLocate(string(out)).procs {
		by[p.pid] = p
	}
	if len(by) != 2 {
		t.Fatalf("expected 2 procs from pids fast path, got %d: %+v", len(by), by)
	}
	if p := by[int64(pidAbs)]; p.agent != "claude" || p.sid != sid1 || p.log != logAbs || p.herdrPane != "w1:p1" {
		t.Errorf("abs custom proc mismatch: %+v (want log %q)", p, logAbs)
	}
	if p := by[int64(pidRel)]; p.agent != "claude" || p.sid != sid2 || p.log != logRel || p.herdrPane != "w1:p2" {
		t.Errorf("rel custom proc mismatch: %+v (want log %q)", p, logRel)
	}
	if _, leaked := by[int64(pidDef)]; leaked {
		t.Errorf("pids fast path should not have scanned unrelated pid %d", pidDef)
	}

	// Forged CLAUDE_CONFIG_DIR inside OTHER must fall back to $HOME/.claude.
	cDef := exec.Command(sh, "-c", convoLocatePidsCommand([]int{pidDef}))
	cDef.Env = []string{"HOME=" + home, "PATH=/usr/bin:/bin"}
	outDef, err := cDef.Output()
	if err != nil {
		t.Fatalf("convoLocatePidsCommand default failed: %v", err)
	}
	procsDef := parseConvoLocate(string(outDef)).procs
	if len(procsDef) != 1 || procsDef[0].pid != int64(pidDef) || procsDef[0].sid != sid3 || procsDef[0].log != logDef {
		t.Fatalf("default/forged proc mismatch: %+v (want log %q)", procsDef, logDef)
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

	// bgTailCommand finds log under CLAUDE_CONFIG_DIR with spaces.
	cBg := exec.Command(sh, "-c", bgTailCommand([]string{sid1}, 4096))
	cBg.Env = []string{"HOME=" + home, "CLAUDE_CONFIG_DIR=" + absCustom, "PATH=/usr/bin:/bin"}
	outBg, err := cBg.Output()
	if err != nil {
		t.Fatalf("bgTailCommand failed: %v", err)
	}
	bgRecs := parseBgTails(outBg)
	if r, ok := bgRecs[sid1]; !ok || r.uuid != "u1" || r.n != 2 {
		t.Fatalf("bgTailCommand custom dir = %+v, want uuid=u1 n=2", bgRecs)
	}

	// forkFindCommand finds log under CLAUDE_CONFIG_DIR with spaces.
	cFork := exec.Command(sh, "-c", forkFindCommand("claude", sid1))
	cFork.Env = []string{"HOME=" + home, "CLAUDE_CONFIG_DIR=" + absCustom, "PATH=/usr/bin:/bin"}
	outFork, err := cFork.Output()
	if err != nil {
		t.Fatalf("forkFindCommand failed: %v", err)
	}
	if got := strings.TrimSpace(string(outFork)); got != logAbs {
		t.Fatalf("forkFindCommand custom dir = %q, want %q", got, logAbs)
	}

	// logsWroteCommand finds log under CLAUDE_CONFIG_DIR with spaces.
	cWrote := exec.Command(sh, "-c", logsWroteCommand([]string{"claude:" + sid1}))
	cWrote.Env = []string{"HOME=" + home, "CLAUDE_CONFIG_DIR=" + absCustom, "PATH=/usr/bin:/bin"}
	outWrote, err := cWrote.Output()
	if err != nil {
		t.Fatalf("logsWroteCommand failed: %v", err)
	}
	lo, _ := parseLogsWrote(string(outWrote), []string{"w1:p1"})
	if lo["w1:p1"] <= 0 {
		t.Fatalf("logsWroteCommand custom dir returned no mtime: %q", string(outWrote))
	}

	// cmdScanCommand finds background subagent under custom CLAUDE_CONFIG_DIR with spaces.
	subDir := filepath.Join(absCustom, "projects", "-work", sid1, "subagents")
	if err := os.MkdirAll(subDir, 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(subDir, "agent-abc123.jsonl"), []byte(`{"version":"2.1.305"}`+"\n"), 0o644)
	os.WriteFile(filepath.Join(subDir, "agent-abc123.meta.json"), []byte(`{"requestShape":"background","description":"custom cfg subagent"}`), 0o644)
	cCmdScan := exec.Command(sh, "-c", cmdScanCommand("w:"+sid1))
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

func TestClaudeRewindCheckCustomConfigDir(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	_ = sh
	binDir, homeDir, fakeProc, _ := setupFakeRewindHost(t, 5555, 12345, sidA, "w1:p1", "/run/herdr.sock")
	// Remove default $HOME/.claude/sessions/5555.json so test FAILS if CLAUDE_CONFIG_DIR is ignored!
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
	var pid int
	if _, err := strconv.Atoi(strings.TrimSpace("0")); err == nil {
		b := make([]byte, 64)
		n, rerr := out.Read(b)
		if rerr != nil {
			t.Fatal(rerr)
		}
		pid, err = strconv.Atoi(strings.TrimSpace(string(b[:n])))
		if err != nil {
			t.Fatalf("bad pid %q: %v", string(b[:n]), err)
		}
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
	// Put session file ONLY in customCfg (NOT in $HOME/.claude) so test fails if CLAUDE_CONFIG_DIR is ignored.
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
