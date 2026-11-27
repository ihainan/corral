package web

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ihainan/corral/internal/convo"
)

func TestIdentifyBgCLI(t *testing.T) {
	argv := func(parts ...string) string { return strings.Join(parts, "\x01") + "\x01" }
	cases := []struct {
		name string
		comm string
		exe  string
		argv string
		want string
	}{
		{
			name: "agy -p with model",
			comm: "agy",
			exe:  "/home/u/.local/bin/agy",
			argv: argv("agy", "-p", "Run Neon on WP1", "--model", "gemini-4-neon-high"),
			want: "agy",
		},
		{
			name: "agy -p prompt mentioning utility words",
			comm: "agy",
			exe:  "/home/u/.local/bin/agy",
			argv: argv("agy", "-p", "help me update mcp config"),
			want: "agy",
		},
		{
			name: "agy utility subcommand rejected",
			comm: "agy",
			exe:  "/home/u/.local/bin/agy",
			argv: argv("agy", "models"),
			want: "",
		},
		{
			name: "agy --help rejected",
			comm: "agy",
			exe:  "/home/u/.local/bin/agy",
			argv: argv("agy", "--help"),
			want: "",
		},
		{
			name: "claude native binary -p",
			comm: "claude",
			exe:  "/home/u/.local/share/claude/versions/2.1.331",
			argv: argv("claude", "-p", "review diff"),
			want: "claude",
		},
		{
			name: "claude node cli.js -p",
			comm: "node",
			exe:  "/usr/bin/node",
			argv: argv("node", "--max-old-space-size=4096", "/usr/lib/node_modules/@anthropic-ai/claude-code/cli.js", "-p", "check tests"),
			want: "claude",
		},
		{
			name: "claude -p --no-session-persistence rejected",
			comm: "claude",
			exe:  "/home/u/.local/bin/claude",
			argv: argv("claude", "-p", "--no-session-persistence", "hello"),
			want: "",
		},
		{
			name: "claude utility subcommand rejected",
			comm: "claude",
			exe:  "/home/u/.local/bin/claude",
			argv: argv("claude", "mcp", "list"),
			want: "",
		},
		{
			name: "codex exec",
			comm: "codex",
			exe:  "/home/u/.nvm/versions/node/v20/lib/node_modules/@openai/codex/vendor/codex",
			argv: argv("codex", "exec", "fix failing tests"),
			want: "codex",
		},
		{
			name: "codex exec --ephemeral rejected",
			comm: "codex",
			exe:  "/usr/local/bin/codex",
			argv: argv("codex", "exec", "--ephemeral", "quick check"),
			want: "",
		},
		{
			name: "codex app-server rejected",
			comm: "codex",
			exe:  "/usr/local/bin/codex",
			argv: argv("codex", "app-server"),
			want: "",
		},
		{
			name: "grep claude is NOT an agent",
			comm: "grep",
			exe:  "/usr/bin/grep",
			argv: argv("grep", "-rn", "claude", "README.md"),
			want: "",
		},
		{
			name: "cat claude.md is NOT an agent",
			comm: "cat",
			exe:  "/bin/cat",
			argv: argv("cat", "claude.md"),
			want: "",
		},
		{
			name: "node arbitrary script with claude arg is NOT an agent",
			comm: "node",
			exe:  "/usr/bin/node",
			argv: argv("node", "/home/u/scripts/check.js", "claude"),
			want: "",
		},
		{
			name: "node -e eval mentioning claude is NOT an agent",
			comm: "node",
			exe:  "/usr/bin/node",
			argv: argv("node", "-e", "console.log('claude')"),
			want: "",
		},
		{
			name: "wrapper sh/timeout/env themselves are NOT agents",
			comm: "timeout",
			exe:  "/usr/bin/timeout",
			argv: argv("timeout", "300", "env", "FOO=1", "agy", "-p", "hi"),
			want: "",
		},
		{
			name: "spoofed comm on non-agent exe rejected",
			comm: "claude",
			exe:  "/usr/bin/grep",
			argv: argv("claude", "foo"),
			want: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := identifyBgCLI(tc.comm, tc.exe, tc.argv); got != tc.want {
				t.Errorf("identifyBgCLI(%q, %q, %q) = %q, want %q", tc.comm, tc.exe, tc.argv, got, tc.want)
			}
		})
	}
}

func TestParseCmdScanBgCLI(t *testing.T) {
	const (
		agySID1   = "11111111-2222-4333-8444-555555555551"
		agySID2   = "11111111-2222-4333-8444-555555555552"
		claudeSID = "22222222-3333-4444-8555-666666666661"
		codexSID  = "33333333-4444-4555-8666-777777777771"
		codexSID2 = "33333333-4444-4555-8666-777777777772"
	)
	agyLog1 := "/home/u/.gemini/antigravity-cli/brain/" + agySID1 + "/.system_generated/logs/transcript_full.jsonl"
	agyLog2 := "/home/u/.gemini/antigravity-cli/brain/" + agySID2 + "/.system_generated/logs/transcript_full.jsonl"
	claudeLog := "/home/u/.claude/projects/-home-u-proj/" + claudeSID + ".jsonl"
	codexLog := "/home/u/.codex/sessions/2026/11/27/rollout-2026-11-27T09-00-00-" + codexSID + ".jsonl"
	codexLog2 := "/home/u/.codex/sessions/2026/11/27/rollout-2026-11-27T09-01-00-" + codexSID2 + ".jsonl"
	dir := "/tmp/claude-1000/-home-u-proj/" + cmdSess + "/tasks/"
	argv := func(parts ...string) string { return strings.Join(parts, "\x01") + "\x01" }

	out := strings.Join([]string{
		"clock\t1700000000\t100",
		// 1. agy with session
		"cmd\t101\t4000\t" + dir + "b1.output\tagy -p 'WP1' --model gemini-4-neon-high' < /dev/null",
		"desc\t" + cmdSess + "\tb1\t{\"description\":\"Run Neon on WP1\"}",
		"cli\t101\t102\tagy\t/home/u/.local/bin/agy\t" + agySID1 + "\t" + agyLog1 + "\t" + argv("agy", "-p", "WP1", "--model", "gemini-4-neon-high"),
		// 2. agy without session -> falls back to normal bg command
		"cmd\t110\t4100\t" + dir + "b2.output\tagy -p 'WP2'' < /dev/null",
		"desc\t" + cmdSess + "\tb2\t{\"description\":\"Run Neon on WP2\"}",
		"cli\t110\t111\tagy\t/home/u/.local/bin/agy\t\t\t" + argv("agy", "-p", "WP2"),
		// 3. Claude Code -p with session
		"cmd\t120\t4200\t" + dir + "b3.output\tclaude -p 'audit auth'' < /dev/null",
		"desc\t" + cmdSess + "\tb3\t{\"description\":\"Audit auth module\"}",
		"cli\t120\t121\tclaude\t/home/u/.local/share/claude/versions/2.1.331\t" + claudeSID + "\t" + claudeLog + "\t" + argv("claude", "-p", "audit auth"),
		// 4. Codex exec with session
		"cmd\t130\t4300\t" + dir + "b4.output\tcodex exec 'write tests'' < /dev/null",
		"desc\t" + cmdSess + "\tb4\t{\"description\":\"Write unit tests\"}",
		"cli\t130\t131\tcodex\t/usr/local/bin/codex\t" + codexSID + "\t" + codexLog + "\t" + argv("codex", "exec", "write tests"),
		// 5. Wrapped in sh -c / timeout / env -> wrappers ignored, inner agy recognized
		"cmd\t140\t4400\t" + dir + "b5.output\ttimeout 600 env FOO=1 sh -c 'agy -p WP5'' < /dev/null",
		"desc\t" + cmdSess + "\tb5\t{\"description\":\"Wrapped Neon WP5\"}",
		"cli\t140\t141\ttimeout\t/usr/bin/timeout\t\t\t" + argv("timeout", "600", "env", "FOO=1", "sh", "-c", "agy -p WP5"),
		"cli\t140\t142\tenv\t/usr/bin/env\t\t\t" + argv("env", "FOO=1", "sh", "-c", "agy -p WP5"),
		"cli\t140\t143\tsh\t/bin/sh\t\t\t" + argv("sh", "-c", "agy -p WP5"),
		"cli\t140\t144\tagy\t/home/u/.local/bin/agy\t" + agySID2 + "\t" + agyLog2 + "\t" + argv("agy", "-p", "WP5"),
		// 6. Command line mentions claude but is grep / cat -> NOT an agent
		"cmd\t150\t4500\t" + dir + "b6.output\tgrep -rn claude . && cat claude.md' < /dev/null",
		"desc\t" + cmdSess + "\tb6\t{\"description\":\"Search claude refs\"}",
		"cli\t150\t151\tgrep\t/usr/bin/grep\t" + claudeSID + "\t" + claudeLog + "\t" + argv("grep", "-rn", "claude", "."),
		"cli\t150\t152\tcat\t/bin/cat\t" + claudeSID + "\t" + claudeLog + "\t" + argv("cat", "claude.md"),
		// 7. Multiple agent CLIs under the same background command
		"cmd\t160\t4600\t" + dir + "b7.output\tagy -p 'part A' & codex exec 'part B' & wait' < /dev/null",
		"desc\t" + cmdSess + "\tb7\t{\"description\":\"Parallel review\"}",
		"cli\t160\t161\tagy\t/home/u/.local/bin/agy\t" + agySID1 + "\t" + agyLog1 + "\t" + argv("agy", "-p", "part A"),
		"cli\t160\t162\tcodex\t/usr/local/bin/codex\t" + codexSID2 + "\t" + codexLog2 + "\t" + argv("codex", "exec", "part B"),
		// 8. Self-reference guard: child somehow pointing at parent session ID is rejected
		"cmd\t170\t4700\t" + dir + "b8.output\tclaude -p 'self'' < /dev/null",
		"cli\t170\t171\tclaude\t/home/u/.local/bin/claude\t" + cmdSess + "\t/home/u/.claude/projects/-home-u-proj/" + cmdSess + ".jsonl\t" + argv("claude", "-p", "self"),
		// 9. Traversal / invalid log path is rejected
		"cmd\t180\t4800\t" + dir + "b9.output\tagy -p 'badlog'' < /dev/null",
		"cli\t180\t181\tagy\t/home/u/.local/bin/agy\t" + agySID1 + "\t/home/u/../../etc/passwd\t" + argv("agy", "-p", "badlog"),
	}, "\n") + "\n"

	got, whole := parseCmdScan(out)
	if !whole {
		t.Fatal("expected whole=true")
	}
	all := got[cmdSess]
	if len(all) != 9 {
		t.Fatalf("expected 9 parsed commands, got %d: %+v", len(all), all)
	}

	plain, cliAgents := splitBgCmds(all)

	// Plain commands should be: b2 (agy no session), b6 (grep/cat claude), b8 (self-ref), b9 (bad log).
	var plainTasks []string
	for _, c := range plain {
		plainTasks = append(plainTasks, c.Task)
	}
	wantPlain := []string{"b2", "b6", "b8", "b9"}
	if strings.Join(plainTasks, ",") != strings.Join(wantPlain, ",") {
		t.Fatalf("plain tasks = %v, want %v", plainTasks, wantPlain)
	}

	// Promoted CLI subagents: b1 (agySID1), b3 (claudeSID), b4 (codexSID), b5 (agySID2), b7 (codexSID2; agySID1 deduped across session).
	// Wait: test multiple agents in ONE command in isolation too so dedupe across b1/b7 doesn't hide b7's first agent.
	if len(cliAgents) != 5 {
		t.Fatalf("expected 5 unique CLI agents, got %d: %+v", len(cliAgents), cliAgents)
	}
	if cliAgents[0].Agent != "agy" || cliAgents[0].ID != agySID1 || cliAgents[0].What != "Run Neon on WP1" || cliAgents[0].Task != "b1" || cliAgents[0].log != agyLog1 {
		t.Errorf("cliAgents[0] = %+v", cliAgents[0])
	}
	if cliAgents[1].Agent != "claude" || cliAgents[1].ID != claudeSID || cliAgents[1].What != "Audit auth module" || cliAgents[1].Task != "b3" {
		t.Errorf("cliAgents[1] = %+v", cliAgents[1])
	}
	if cliAgents[2].Agent != "codex" || cliAgents[2].ID != codexSID || cliAgents[2].What != "Write unit tests" || cliAgents[2].Task != "b4" {
		t.Errorf("cliAgents[2] = %+v", cliAgents[2])
	}
	if cliAgents[3].Agent != "agy" || cliAgents[3].ID != agySID2 || cliAgents[3].What != "Wrapped Neon WP5" || cliAgents[3].Task != "b5" {
		t.Errorf("cliAgents[3] = %+v", cliAgents[3])
	}
	if cliAgents[4].Agent != "codex" || cliAgents[4].ID != codexSID2 || cliAgents[4].Task != "b7" {
		t.Errorf("cliAgents[4] = %+v", cliAgents[4])
	}

	// Explicitly test multiple distinct agents inside a single command (both retained).
	multiPlain, multiCLIs := splitBgCmds([]bgCmd{all[6]})
	if len(multiPlain) != 0 || len(multiCLIs) != 2 {
		t.Fatalf("single command with 2 CLIs: plain=%+v clis=%+v", multiPlain, multiCLIs)
	}
	if multiCLIs[0].Agent != "agy" || multiCLIs[0].ID != agySID1 || multiCLIs[1].Agent != "codex" || multiCLIs[1].ID != codexSID2 {
		t.Errorf("unexpected multi CLIs: %+v", multiCLIs)
	}

	// upgradeRunningWithCLIs replaces the single running cmd b7 with both CLI agents.
	upgraded := upgradeRunningWithCLIs([]convo.Running{
		{ID: "b7", Kind: "cmd", What: "Parallel review", Cmd: "agy -p 'part A' & codex exec 'part B' & wait"},
		{ID: "b2", Kind: "cmd", What: "Run Neon on WP2", Cmd: "agy -p 'WP2'"},
	}, multiCLIs)
	if len(upgraded) != 3 {
		t.Fatalf("upgraded len=%d, want 3: %+v", len(upgraded), upgraded)
	}
	if upgraded[0].Kind != "agent" || upgraded[0].Agent != "agy" || upgraded[0].ID != agySID1 || upgraded[0].Task != "b7" {
		t.Errorf("upgraded[0] = %+v", upgraded[0])
	}
	if upgraded[1].Kind != "agent" || upgraded[1].Agent != "codex" || upgraded[1].ID != codexSID2 || upgraded[1].Task != "b7" {
		t.Errorf("upgraded[1] = %+v", upgraded[1])
	}
	if upgraded[2].Kind != "cmd" || upgraded[2].ID != "b2" {
		t.Errorf("upgraded[2] = %+v", upgraded[2])
	}
}

func TestCmdProberDecorateBgCLI(t *testing.T) {
	const agySID = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	agyLog := "/home/u/.gemini/antigravity-cli/brain/" + agySID + "/.system_generated/logs/transcript_full.jsonl"

	p := newCmdSubProber(nil)
	now := time.Now()
	p.at = now
	p.cmds = map[string][]bgCmd{
		cmdSess: {
			{
				Task:  "b1",
				Title: "agy -p 'WP1'",
				Desc:  "Run Neon on WP1",
				Since: 1700000040000,
				clis:  []bgCLI{{Agent: "agy", Session: agySID, Log: agyLog}},
			},
			{
				Task:  "b2",
				Title: "npm run dev",
				Desc:  "Start dev server",
				Since: 1700000050000,
			},
		},
	}
	p.bgIDs = map[string]map[string]bool{cmdSess: {"b1": true, "b2": true}}
	p.recordCLILogsLocked(p.cmds, now)
	p.consider("w1:p1", "claude", "done", cmdSess)
	p.at = time.Now() // scan after ended.from

	// Even when bgProber says bg=none (no native Claude Task subagents),
	// promoted CLI subagents MUST appear in BgAgentList and NOT count in BgCmds.
	sum := herdrSummary{
		Workspaces: []herdrWorkspace{{
			Agents: []herdrAgent{{PaneID: "w1:p1", Agent: "claude", Status: "done", Session: cmdSess, Bg: bgNone}},
		}},
	}
	p.decorate(&sum)
	a := sum.Workspaces[0].Agents[0]
	if a.BgCmds != 1 || len(a.BgCmdList) != 1 || a.BgCmdList[0].Task != "b2" {
		t.Fatalf("expected only plain cmd b2 in BgCmds, got BgCmds=%d list=%+v", a.BgCmds, a.BgCmdList)
	}
	if len(a.BgAgentList) != 1 || a.BgAgentList[0].Agent != "agy" || a.BgAgentList[0].ID != agySID || a.BgAgentList[0].What != "Run Neon on WP1" || a.BgAgentList[0].Task != "b1" {
		t.Fatalf("expected promoted agy subagent in BgAgentList, got %+v", a.BgAgentList)
	}

	// If the background command finishes on the next scan, it disappears from
	// BgAgentList immediately while its verified log remains readable briefly.
	p.cmds = map[string][]bgCmd{}
	sum2 := herdrSummary{
		Workspaces: []herdrWorkspace{{
			Agents: []herdrAgent{{PaneID: "w1:p1", Agent: "claude", Status: "done", Session: cmdSess, Bg: bgNone}},
		}},
	}
	p.decorate(&sum2)
	if len(sum2.Workspaces[0].Agents[0].BgAgentList) != 0 || sum2.Workspaces[0].Agents[0].BgCmds != 0 {
		t.Fatalf("expected finished CLI command to disappear, got %+v", sum2.Workspaces[0].Agents[0])
	}
	if logPath, ok := p.cliLog("agy", agySID); !ok || logPath != agyLog {
		t.Fatalf("expected cliLog cache to keep %q briefly after exit, got (%q, %v)", agyLog, logPath, ok)
	}
}

func TestAgyBgFdsDBFallback(t *testing.T) {
	const sid = "99999999-8888-4777-8666-555555555555"
	tmp := t.TempDir()
	base := filepath.Join(tmp, ".gemini", "antigravity-cli")
	convDir := filepath.Join(base, "conversations")
	logsDir := filepath.Join(base, "brain", sid, ".system_generated", "logs")
	procFD := filepath.Join(tmp, "proc", "42", "fd")
	for _, d := range []string{convDir, logsDir, procFD} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	dbFile := filepath.Join(convDir, sid+".db")
	logFile := filepath.Join(logsDir, "transcript_full.jsonl")
	if err := os.WriteFile(dbFile, []byte("sqlite"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(logFile, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Only conversations/<sid>.db-wal open (no presence lock): production agyBgFdsSh must resolve it.
	if err := os.Symlink(dbFile+"-wal", filepath.Join(procFD, "7")); err != nil {
		t.Fatal(err)
	}
	script := agyFdsSh + agyBgFdsSh + "\nagy_bg_fds \"$1\"\nprintf '%s\\t%s' \"$agysid\" \"$agylog\"\n"
	out, err := exec.Command("sh", "-c", script, "sh", filepath.Join(tmp, "proc", "42")).Output()
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != sid+"\t"+logFile {
		t.Fatalf("agy_bg_fds = %q, want %q", string(out), sid+"\t"+logFile)
	}
	// If transcript_full.jsonl does not exist, agy_bg_fds returns empty so caller falls back.
	if err := os.Remove(logFile); err != nil {
		t.Fatal(err)
	}
	out2, err := exec.Command("sh", "-c", script, "sh", filepath.Join(tmp, "proc", "42")).Output()
	if err != nil {
		t.Fatal(err)
	}
	if string(out2) != "\t" {
		t.Fatalf("agy_bg_fds without transcript = %q, want empty", string(out2))
	}
}

func TestCheckBgCLILogSFTP(t *testing.T) {
	c := sftpOf(t)
	tmp := t.TempDir()
	const (
		agySID    = "11111111-2222-4333-8444-555555555551"
		claudeSID = "22222222-3333-4444-8555-666666666662"
		codexSID  = "33333333-4444-4555-8666-777777777763"
	)

	// 1. Valid agy transcript
	agyLog := filepath.Join(tmp, ".gemini", "antigravity-cli", "brain", agySID, ".system_generated", "logs", "transcript_full.jsonl")
	if err := os.MkdirAll(filepath.Dir(agyLog), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(agyLog, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := checkBgCLILog(c, "agy", agyLog, agySID); err != nil {
		t.Fatalf("valid agy log rejected: %v", err)
	}

	// 2. Valid claude -p log
	claudeLog := filepath.Join(tmp, ".claude", "projects", "-home-u-proj", claudeSID+".jsonl")
	if err := os.MkdirAll(filepath.Dir(claudeLog), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(claudeLog, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := checkBgCLILog(c, "claude", claudeLog, claudeSID); err != nil {
		t.Fatalf("valid claude log rejected: %v", err)
	}

	// 3. Valid codex exec rollout log
	codexLog := filepath.Join(tmp, ".codex", "sessions", "2026", "11", "27", "rollout-2026-11-27T09-00-00-"+codexSID+".jsonl")
	if err := os.MkdirAll(filepath.Dir(codexLog), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(codexLog, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := checkBgCLILog(c, "codex", codexLog, codexSID); err != nil {
		t.Fatalf("valid codex log rejected: %v", err)
	}

	// 4. Symlinked log file must be refused with errSubagentFile.
	if err := os.Rename(claudeLog, claudeLog+".real"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(claudeLog+".real", claudeLog); err != nil {
		t.Fatal(err)
	}
	if err := checkBgCLILog(c, "claude", claudeLog, claudeSID); !errors.Is(err, errSubagentFile) {
		t.Fatalf("symlinked claude log got %v, want errSubagentFile", err)
	}
}

func TestConvoStateParserForStandaloneClaudeBgCLI(t *testing.T) {
	// Standalone claude -p (sub=true, sidechain=false) uses NewClaude (ignores sidechain rows),
	// whereas native Claude Task subagent (sub=true, sidechain=true) uses NewClaudeSub.
	lineMain := []byte(`{"type":"assistant","uuid":"a1","message":{"content":[{"type":"text","text":"main output"}]}}` + "\n")
	lineSide := []byte(`{"type":"assistant","uuid":"a2","isSidechain":true,"message":{"content":[{"type":"text","text":"sidechain output"}]}}` + "\n")

	cliState := &convoState{sub: true, sidechain: false, agent: "claude"}
	pCLI := cliState.newParser()
	pCLI.Feed(lineMain)
	pCLI.Feed(lineSide)
	if entries := pCLI.Since(0); len(entries) != 1 {
		t.Fatalf("standalone claude -p parser kept sidechain rows: got %d entries", len(entries))
	}

	subState := &convoState{sub: true, sidechain: true, agent: "claude"}
	pSub := subState.newParser()
	pSub.Feed(lineSide)
	if entries := pSub.Since(0); len(entries) != 1 {
		t.Fatalf("native claude subagent parser dropped sidechain row: got %d entries", len(entries))
	}
}
