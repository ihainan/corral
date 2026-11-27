package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/ihainan/corral/internal/convo"
	"github.com/ihainan/corral/internal/sshx"
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
			name: "node --eval=code with claude path arg is NOT an agent",
			comm: "node",
			exe:  "/usr/bin/node",
			argv: argv("node", "--eval=console.log(1)", "/usr/local/bin/claude"),
			want: "",
		},
		{
			name: "node -ecode with claude path arg is NOT an agent",
			comm: "node",
			exe:  "/usr/bin/node",
			argv: argv("node", "-econsole.log(1)", "/usr/local/bin/claude"),
			want: "",
		},
		{
			name: "node --print=1 with claude path arg is NOT an agent",
			comm: "node",
			exe:  "/usr/bin/node",
			argv: argv("node", "--print=1", "/usr/local/bin/claude"),
			want: "",
		},
		{
			name: "node -p1 / -pe with claude path arg is NOT an agent",
			comm: "node",
			exe:  "/usr/bin/node",
			argv: argv("node", "-pe", "1", "/usr/local/bin/claude"),
			want: "",
		},
		{
			name: "node -r preload then claude cli.js IS claude",
			comm: "node",
			exe:  "/usr/bin/node",
			argv: argv("node", "-r", "/tmp/preload.js", "/usr/lib/node_modules/@anthropic-ai/claude-code/cli.js", "-p", "hi"),
			want: "claude",
		},
		{
			name: "shell wrapper script named agy IS agy",
			comm: "agy",
			exe:  "/bin/dash",
			argv: argv("/bin/sh", "/tmp/bin/agy", "-p", "WP1"),
			want: "agy",
		},
		{
			name: "sh -c running agy is NOT itself an agent",
			comm: "sh",
			exe:  "/bin/dash",
			argv: argv("/bin/sh", "-c", "agy -p WP1"),
			want: "",
		},
		{
			name: "sh script not named agy/claude/codex is NOT an agent",
			comm: "helper.sh",
			exe:  "/bin/dash",
			argv: argv("/bin/sh", "/tmp/helper.sh", "claude"),
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

// Backend review #3: Codex and agy parents emit synthetic task IDs (codex-<pid>,
// agy-<pid>) while their log parsers emit tool-call / background-task IDs.
// upgradeRunningWithCLIs must match them by command text and upgrade them.
func TestUpgradeRunningWithCLIsSyntheticParent(t *testing.T) {
	const (
		agySID    = "11111111-2222-4333-8444-555555555501"
		claudeSID = "22222222-3333-4444-8555-666666666602"
		codexSID  = "33333333-4444-4555-8666-777777777703"
	)
	cliAgents := []bgAgent{
		{ID: agySID, Agent: "agy", Task: "codex-8597", Cmd: "agy -p 'WP1' --model gemini-4-neon-high", What: "agy -p 'WP1' --model gemini-4-neon-high", Since: 1700000010000},
		{ID: codexSID, Agent: "codex", Task: "codex-8597", Cmd: "agy -p 'WP1' --model gemini-4-neon-high", What: "agy -p 'WP1' --model gemini-4-neon-high", Since: 1700000010000},
		{ID: claudeSID, Agent: "claude", Task: "agy-9001", Cmd: "claude -p 'audit auth'", What: "claude -p 'audit auth'", Since: 1700000020000},
	}
	running := []convo.Running{
		{ID: "call_codex_1", Kind: "cmd", What: "agy -p 'WP1' --model gemini-4-neon-high", Cmd: "agy -p 'WP1' --model gemini-4-neon-high"},
		{ID: "agy-task-9", Kind: "cmd", What: "Audit auth module", Cmd: "claude -p 'audit auth'"},
		{ID: "call_plain_2", Kind: "cmd", What: "go test ./...", Cmd: "go test ./..."},
	}
	got := upgradeRunningWithCLIs(running, cliAgents)
	if len(got) != 4 {
		t.Fatalf("upgraded len = %d, want 4: %+v", len(got), got)
	}
	if got[0].Kind != "agent" || got[0].Agent != "agy" || got[0].ID != agySID || got[0].Task != "codex-8597" {
		t.Errorf("got[0] = %+v", got[0])
	}
	if got[1].Kind != "agent" || got[1].Agent != "codex" || got[1].ID != codexSID || got[1].Task != "codex-8597" {
		t.Errorf("got[1] = %+v", got[1])
	}
	if got[2].Kind != "agent" || got[2].Agent != "claude" || got[2].ID != claudeSID || got[2].Task != "agy-9001" || got[2].What != "Audit auth module" {
		t.Errorf("got[2] = %+v", got[2])
	}
	if got[3].Kind != "cmd" || got[3].ID != "call_plain_2" {
		t.Errorf("got[3] = %+v", got[3])
	}
}

// Round 3 review #3: synthetic task matching MUST require exact normalized
// command equality and 1-to-1 uniqueness — never substring containment (e.g.
// plain "sleep 30" must never match "sleep 30; agy -p audit") or ambiguous
// multi-candidate matches.
func TestUpgradeRunningWithCLIsStrictExactAndUnique(t *testing.T) {
	const (
		agySID1 = "11111111-2222-4333-8444-555555555601"
		agySID2 = "11111111-2222-4333-8444-555555555602"
		agySID3 = "11111111-2222-4333-8444-555555555603"
	)

	// 1. Plain "sleep 30" appears BEFORE "sleep 30; agy -p audit": must NOT be
	// falsely upgraded; only the exact match upgrades.
	got := upgradeRunningWithCLIs(
		[]convo.Running{
			{ID: "call_1", Kind: "cmd", What: "sleep 30", Cmd: "sleep 30"},
			{ID: "call_2", Kind: "cmd", What: "sleep 30; agy -p audit", Cmd: "sleep 30; agy -p audit"},
		},
		[]bgAgent{
			{ID: agySID1, Agent: "agy", Task: "codex-901", Cmd: "sleep 30; agy -p audit", What: "sleep 30; agy -p audit"},
		},
	)
	if len(got) != 2 {
		t.Fatalf("len=%d, want 2: %+v", len(got), got)
	}
	if got[0].Kind != "cmd" || got[0].ID != "call_1" {
		t.Fatalf("plain sleep 30 was wrongly upgraded: %+v", got[0])
	}
	if got[1].Kind != "agent" || got[1].Agent != "agy" || got[1].ID != agySID1 || got[1].Task != "codex-901" {
		t.Fatalf("exact match failed to upgrade: %+v", got[1])
	}

	// 2. Ambiguous running commands (two identical commands, one synthetic CLI):
	// neither is upgraded.
	ambigRun := upgradeRunningWithCLIs(
		[]convo.Running{
			{ID: "call_a", Kind: "cmd", What: "agy -p audit", Cmd: "agy -p audit"},
			{ID: "call_b", Kind: "cmd", What: "agy -p audit", Cmd: "agy -p audit"},
		},
		[]bgAgent{
			{ID: agySID1, Agent: "agy", Task: "codex-902", Cmd: "agy -p audit"},
		},
	)
	if ambigRun[0].Kind != "cmd" || ambigRun[1].Kind != "cmd" {
		t.Fatalf("ambiguous running candidates should not upgrade: %+v", ambigRun)
	}

	// 3. Ambiguous synthetic CLI tasks (one running command, two synthetic tasks
	// with the same command text): must not upgrade.
	ambigSynth := upgradeRunningWithCLIs(
		[]convo.Running{
			{ID: "call_c", Kind: "cmd", What: "agy -p audit", Cmd: "agy -p audit"},
		},
		[]bgAgent{
			{ID: agySID2, Agent: "agy", Task: "codex-903", Cmd: "agy -p audit"},
			{ID: agySID3, Agent: "agy", Task: "codex-904", Cmd: "agy -p audit"},
		},
	)
	if ambigSynth[0].Kind != "cmd" {
		t.Fatalf("ambiguous synthetic candidates should not upgrade: %+v", ambigSynth)
	}

	// 4. Round 3 P2-3: Long commands (> 80 chars, <= 4 KiB) keep their display
	// Title truncated at 80 chars, yet upgrade 1-to-1 via their full matchCmd.
	const parentThread = "01a1dd7f-4856-7042-8ad2-f46c56372e85"
	longCmd := "agy -p 'Run comprehensive security audit across authentication and session token rotation pipeline' --model gemini-4-neon-high --output-format json"
	if len(longCmd) <= 80 {
		t.Fatalf("test command must exceed 80 chars, got %d", len(longCmd))
	}
	agyLog := "/home/u/.gemini/antigravity-cli/brain/" + agySID1 + "/.system_generated/logs/transcript_full.jsonl"
	argv := strings.Join([]string{"agy", "-p", "audit"}, "\x01") + "\x01"
	scanOut := strings.Join([]string{
		"clock\t1700000000\t100",
		"cmd\t8597\t4000\tcodex:" + parentThread + "\t" + longCmd,
		"cli\t8597\t8598\tagy\t/home/u/.local/bin/agy\t" + agySID1 + "\t" + agyLog + "\t" + argv,
	}, "\n") + "\n"
	parsed, whole, budgetCut := parseCmdScanDetail(scanOut)
	if !whole || budgetCut || len(parsed[parentThread]) != 1 {
		t.Fatalf("parseCmdScanDetail = whole:%v budgetCut:%v len:%d", whole, budgetCut, len(parsed[parentThread]))
	}
	if len([]rune(parsed[parentThread][0].Title)) > 80 {
		t.Fatalf("display Title was not truncated to 80 runes: %q", parsed[parentThread][0].Title)
	}
	_, longCLIs := splitBgCmds(parsed[parentThread])
	if len(longCLIs) != 1 || longCLIs[0].Cmd != parsed[parentThread][0].Title {
		t.Fatalf("splitBgCmds longCLIs = %+v", longCLIs)
	}
	upgradedLong := upgradeRunningWithCLIs(
		[]convo.Running{
			{ID: "call_long_1", Kind: "cmd", What: longCmd, Cmd: longCmd},
			{ID: "call_other_2", Kind: "cmd", What: "sleep 30", Cmd: "sleep 30"},
		},
		longCLIs,
	)
	if len(upgradedLong) != 2 || upgradedLong[0].Kind != "agent" || upgradedLong[0].Agent != "agy" || upgradedLong[0].ID != agySID1 || upgradedLong[0].Task != "codex-8597" {
		t.Fatalf("long >80-char command failed to upgrade 1-to-1: %+v", upgradedLong)
	}
	if upgradedLong[1].Kind != "cmd" || upgradedLong[1].ID != "call_other_2" {
		t.Fatalf("unrelated command wrongly upgraded: %+v", upgradedLong[1])
	}

	// 5. Round 4 #3 & #4: Oversized (> 4 KiB) synthetic command stays in plain
	// (0 promoted CLI subagents) and does NOT upgrade, so there is never both a
	// plain command and a CLI subagent shown simultaneously.
	hugeCmd := "agy -p '" + strings.Repeat("z", maxBgCmdMatchBytes+32) + "'"
	hugeScan := strings.Join([]string{
		"clock\t1700000000\t100",
		"cmd\t8599\t4100\tcodex:" + parentThread + "\t" + hugeCmd,
		"cli\t8599\t8600\tagy\t/home/u/.local/bin/agy\t" + agySID2 + "\t/home/u/.gemini/antigravity-cli/brain/" + agySID2 + "/.system_generated/logs/transcript_full.jsonl\t" + argv,
	}, "\n") + "\n"
	parsedHuge, _, _ := parseCmdScanDetail(hugeScan)
	hugePlain, hugeCLIs := splitBgCmds(parsedHuge[parentThread])
	if len(hugePlain) != 1 || len(hugeCLIs) != 0 {
		t.Fatalf(">4 KiB synthetic cmd must stay plain with 0 CLI subagents: plain=%+v clis=%+v", hugePlain, hugeCLIs)
	}
	upgradedHuge := upgradeRunningWithCLIs(
		[]convo.Running{{ID: "call_huge", Kind: "cmd", What: hugeCmd, Cmd: hugeCmd, CmdCut: true}},
		hugeCLIs,
	)
	if len(upgradedHuge) != 1 || upgradedHuge[0].Kind != "cmd" {
		t.Fatalf(">4 KiB command should not participate in synthetic upgrade: %+v", upgradedHuge)
	}
}

// Round 4 #3 & #4: End-to-end test through the real convo.Codex parser and
// Monitor host-scan parser:
// 1. A 301..4096-byte Codex background command populates Running.Cmd with
//    CmdCut=false and upgrades 1-to-1 (never relying on 300-byte r.What).
// 2. An oversized (> 4 KiB) Codex background command sets Running.CmdCut=true
//    on the parser side AND matchCut=true on the host side, so it is NOT
//    upgraded and NOT promoted to BgAgentList (zero plain + CLI duplication).
// 3. A Monitor (> 400 bytes, <= 4 KiB) command upgrades 1-to-1, while a > 4 KiB
//    Monitor command stays plain and never matches a truncated prefix.
func TestOversizedCodexAndMonitorRealParserNoUpgradeNoDuplicate(t *testing.T) {
	const (
		parentThread = "01a1dd7f-4856-7042-8ad2-f46c56372e85"
		agySIDMid    = "11111111-2222-4333-8444-555555555701"
		agySIDHuge   = "11111111-2222-4333-8444-555555555702"
		agySIDMon    = "11111111-2222-4333-8444-555555555703"
	)
	argv := strings.Join([]string{"agy", "-p", "run"}, "\x01") + "\x01"
	logOf := func(id string) string {
		return "/home/u/.gemini/antigravity-cli/brain/" + id + "/.system_generated/logs/transcript_full.jsonl"
	}

	// 1. Real convo.Codex parser with a 500-byte command (exceeds old 300-byte What limit, <= 4096B).
	midCmd := "agy -p '" + strings.Repeat("m", 460) + "' --model gemini-4-neon-high"
	pMid := convo.NewCodex()
	pMid.Feed([]byte(strings.Join([]string{
		`{"timestamp":"2026-11-27T10:00:00Z","type":"response_item","payload":{"type":"function_call","name":"exec_command","call_id":"call_mid_1","arguments":"{\"cmd\":\"` + midCmd + `\"}"}}`,
		`{"timestamp":"2026-11-27T10:00:01Z","type":"response_item","payload":{"type":"function_call_output","call_id":"call_mid_1","output":"Chunk ID: fca447\nWall time: 1.0014 seconds\nProcess running with session ID 42\nOriginal token count: 0\nOutput:\n"}}`,
	}, "\n") + "\n"))
	midRunning := pMid.Running()
	if len(midRunning) != 1 || midRunning[0].Kind != "cmd" || midRunning[0].Cmd != midCmd || midRunning[0].CmdCut {
		t.Fatalf("real Codex 500B running = %+v, want full Cmd and CmdCut=false", midRunning)
	}
	midScan := strings.Join([]string{
		"clock\t1700000000\t100",
		"cmd\t7001\t4000\tcodex:" + parentThread + "\t" + midCmd,
		"cli\t7001\t7002\tagy\t/home/u/.local/bin/agy\t" + agySIDMid + "\t" + logOf(agySIDMid) + "\t" + argv,
	}, "\n") + "\n"
	midParsed, _, _ := parseCmdScanDetail(midScan)
	midPlain, midCLIs := splitBgCmds(midParsed[parentThread])
	if len(midPlain) != 0 || len(midCLIs) != 1 {
		t.Fatalf("500B codex cmd splitBgCmds: plain=%+v clis=%+v", midPlain, midCLIs)
	}
	midUp := upgradeRunningWithCLIs(midRunning, midCLIs)
	if len(midUp) != 1 || midUp[0].Kind != "agent" || midUp[0].Agent != "agy" || midUp[0].ID != agySIDMid || midUp[0].Task != "codex-7001" {
		t.Fatalf("500B codex cmd failed to upgrade through real parser: %+v", midUp)
	}

	// 2. Real convo.Codex parser with an oversized (> 4 KiB) command (both exec_command and code-mode exec).
	hugeCmd := "agy -p '" + strings.Repeat("q", maxBgCmdMatchBytes+200) + "'"
	pHuge := convo.NewCodex()
	pHuge.Feed([]byte(strings.Join([]string{
		`{"timestamp":"2026-11-27T10:01:00Z","type":"response_item","payload":{"type":"function_call","name":"exec_command","call_id":"call_huge_1","arguments":"{\"cmd\":\"` + hugeCmd + `\"}"}}`,
		`{"timestamp":"2026-11-27T10:01:01Z","type":"response_item","payload":{"type":"function_call_output","call_id":"call_huge_1","output":"Chunk ID: fca448\nWall time: 1.0014 seconds\nProcess running with session ID 99\nOriginal token count: 0\nOutput:\n"}}`,
	}, "\n") + "\n"))
	hugeRunning := pHuge.Running()
	if len(hugeRunning) != 1 || hugeRunning[0].Kind != "cmd" || !hugeRunning[0].CmdCut {
		t.Fatalf("real Codex >4KiB running = %+v, want Kind=cmd and CmdCut=true", hugeRunning)
	}
	pCodeHuge := convo.NewCodex()
	pCodeHuge.Feed([]byte(strings.Join([]string{
		`{"timestamp":"2026-11-27T10:01:00Z","type":"response_item","payload":{"type":"custom_tool_call","status":"completed","call_id":"call_cm_1","name":"exec","input":"await tools.exec_command({cmd:\"` + hugeCmd + `\"})"}}`,
		`{"timestamp":"2026-11-27T10:01:01Z","type":"response_item","payload":{"type":"custom_tool_call_output","call_id":"call_cm_1","output":[{"type":"input_text","text":"{\"chunk_id\":\"659348\",\"wall_time_seconds\":0.5,\"session_id\":37552,\"output\":\"\"}"}]}}`,
	}, "\n") + "\n"))
	codeHugeRunning := pCodeHuge.Running()
	if len(codeHugeRunning) != 1 || codeHugeRunning[0].Kind != "cmd" || !codeHugeRunning[0].CmdCut {
		t.Fatalf("real Codex code-mode >4KiB running = %+v, want Kind=cmd and CmdCut=true", codeHugeRunning)
	}
	hugeScan := strings.Join([]string{
		"clock\t1700000000\t100",
		"cmd\t7010\t4100\tcodex:" + parentThread + "\t" + hugeCmd,
		"cli\t7010\t7011\tagy\t/home/u/.local/bin/agy\t" + agySIDHuge + "\t" + logOf(agySIDHuge) + "\t" + argv,
	}, "\n") + "\n"
	hugeParsed, _, _ := parseCmdScanDetail(hugeScan)
	if len(hugeParsed[parentThread]) != 1 || !hugeParsed[parentThread][0].matchCut {
		t.Fatalf("host parseCmdScanDetail did not mark >4KiB cmd matchCut=true: %+v", hugeParsed[parentThread])
	}
	hugePlain, hugeCLIs := splitBgCmds(hugeParsed[parentThread])
	if len(hugePlain) != 1 || len(hugeCLIs) != 0 {
		t.Fatalf(">4KiB codex cmd must stay plain (0 cliAgents to prevent duplicate display): plain=%+v clis=%+v", hugePlain, hugeCLIs)
	}
	hugeUp := upgradeRunningWithCLIs(hugeRunning, hugeCLIs)
	if len(hugeUp) != 1 || hugeUp[0].Kind != "cmd" || hugeUp[0].ID != "call_huge_1" {
		t.Fatalf(">4KiB codex cmd must remain plain cmd, got %+v", hugeUp)
	}
	// Even if a synthetic bgAgent with matchCut=false were somehow paired against
	// a log-side truncated Running (CmdCut=true), exactBgCmdMatch must still refuse.
	refusedByLogCut := upgradeRunningWithCLIs(hugeRunning, []bgAgent{
		{ID: agySIDHuge, Agent: "agy", Task: "codex-7010", Cmd: hugeRunning[0].Cmd, matchCmd: hugeRunning[0].Cmd},
	})
	if len(refusedByLogCut) != 1 || refusedByLogCut[0].Kind != "cmd" {
		t.Fatalf("CmdCut=true on log side must prevent synthetic upgrade: %+v", refusedByLogCut)
	}

	// 3. Monitor (> 400 bytes, <= 4 KiB) upgrades cleanly, whereas > 4 KiB Monitor stays plain.
	mon500 := "agy -p '" + strings.Repeat("w", 460) + "'"
	monScan := strings.Join([]string{
		"clock\t1700000000\t100",
		"cmd\t7020\t4200\tmonitor:" + cmdSess + "\t" + mon500,
		"cli\t7020\t7021\tagy\t/home/u/.local/bin/agy\t" + agySIDMon + "\t" + logOf(agySIDMon) + "\t" + argv,
		"cmd\t7022\t4201\tmonitor:" + cmdSess + "\t" + hugeCmd,
		"cli\t7022\t7023\tagy\t/home/u/.local/bin/agy\t" + agySIDHuge + "\t" + logOf(agySIDHuge) + "\t" + argv,
	}, "\n") + "\n"
	monParsed, _, _ := parseCmdScanDetail(monScan)
	monPlain, monCLIs := splitBgCmds(monParsed[cmdSess])
	if len(monPlain) != 1 || monPlain[0].Task != "monitor-7022" || len(monCLIs) != 1 || monCLIs[0].Task != "monitor-7020" {
		t.Fatalf("monitor split: plain=%+v clis=%+v", monPlain, monCLIs)
	}
	monUp := upgradeRunningWithCLIs([]convo.Running{
		{ID: "mon_1", Kind: "cmd", What: "Watch", Cmd: mon500},
		{ID: "mon_2", Kind: "cmd", What: "Watch huge", Cmd: hugeCmd[:400], CmdCut: true},
	}, monCLIs)
	if len(monUp) != 2 || monUp[0].Kind != "agent" || monUp[0].ID != agySIDMon || monUp[1].Kind != "cmd" || monUp[1].ID != "mon_2" {
		t.Fatalf("monitor upgrade result = %+v", monUp)
	}
}

// Round 3 review #2 & #5:
// - On sshx.ErrOutputTooLarge, cmdProber.run keeps the previous cmds & subs
//   lists instead of wiping them out.
// - When > maxCmdsShown plain commands exist, decorate emits BgCmdTasks with
//   all plain task IDs so the frontend can safely adjust overflow counts.
func TestCmdProberOverflowKeepsPrevious(t *testing.T) {
	const agySID = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeee99"
	agyLog := "/home/u/.gemini/antigravity-cli/brain/" + agySID + "/.system_generated/logs/transcript_full.jsonl"

	var mode atomic.Int32 // 0=ok, 1=ErrOutputTooLarge, 2=errScanBudget ("budget"), 3=incomplete ("more")
	scans := make(chan struct{}, 8)
	p := newCmdSubProber(func(context.Context, []string) (map[string][]bgCmd, map[string][]bgAgent, map[string]map[string]bool, bool, error) {
		defer func() { scans <- struct{}{} }()
		switch mode.Load() {
		case 1:
			return nil, nil, nil, false, sshx.ErrOutputTooLarge
		case 2:
			_, _, budgetCut := parseCmdScanDetail("clock\t1700000000\t100\nbudget\n")
			if !budgetCut {
				return nil, nil, nil, false, errors.New("expected budgetCut=true")
			}
			return nil, nil, nil, false, errScanBudget
		case 3:
			got, whole, _ := parseCmdScanDetail("clock\t1700000000\t100\nmore\n")
			return got, nil, nil, whole, nil
		}
		cmds := make([]bgCmd, 0, 10)
		for i := 1; i <= 9; i++ {
			cmds = append(cmds, bgCmd{Task: fmt.Sprintf("b%d", i), Title: fmt.Sprintf("cmd %d", i)})
		}
		cmds = append(cmds, bgCmd{
			Task:  "bcli",
			Title: "agy -p WP1",
			Desc:  "Run Neon on WP1",
			clis:  []bgCLI{{Agent: "agy", Session: agySID, Log: agyLog}},
		})
		subs := map[string][]bgAgent{
			cmdSess: {{ID: "subnative1", What: "Explore repo", Since: 1700000000000}},
		}
		return map[string][]bgCmd{cmdSess: cmds}, subs, nil, true, nil
	})
	p.every = 40 * time.Millisecond
	p.soonBy = 0
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go p.run(ctx)

	p.consider("w1:p1", "claude", "done", cmdSess)
	<-scans
	// Wait until p.at is recorded.
	deadline := time.Now().Add(time.Second)
	for {
		p.mu.Lock()
		ready := !p.at.IsZero()
		p.mu.Unlock()
		if ready || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	readSummary := func() herdrAgent {
		s := herdrSummary{Workspaces: []herdrWorkspace{{Agents: []herdrAgent{{PaneID: "w1:p1", Agent: "claude", Status: "done", Session: cmdSess}}}}}
		p.decorate(&s)
		return s.Workspaces[0].Agents[0]
	}

	before := readSummary()
	if before.BgCmds != 9 || len(before.BgCmdList) != maxCmdsShown || len(before.BgCmdTasks) != 9 || before.BgCmdTasks[8] != "b9" {
		t.Fatalf("before overflow: BgCmds=%d list=%d tasks=%v", before.BgCmds, len(before.BgCmdList), before.BgCmdTasks)
	}
	if len(before.BgAgentList) != 2 {
		t.Fatalf("before overflow: BgAgentList=%+v, want 2 (1 native + 1 cli)", before.BgAgentList)
	}

	// 1. Simulate scan output exceeding cmdScanMax (ErrOutputTooLarge): keep previous.
	mode.Store(1)
	<-scans
	time.Sleep(20 * time.Millisecond)
	after := readSummary()
	if after.BgCmds != 9 || len(after.BgCmdList) != maxCmdsShown || len(after.BgCmdTasks) != 9 || len(after.BgAgentList) != 2 {
		t.Fatalf("ErrOutputTooLarge wiped previous state: BgCmds=%d list=%d tasks=%v agents=%+v", after.BgCmds, len(after.BgCmdList), after.BgCmdTasks, after.BgAgentList)
	}

	// 2. Round 3 P2-4: Simulate script unified budget truncation ("budget" -> errScanBudget): keep previous.
	mode.Store(2)
	<-scans
	time.Sleep(20 * time.Millisecond)
	afterBudget := readSummary()
	if afterBudget.BgCmds != 9 || len(afterBudget.BgCmdList) != maxCmdsShown || len(afterBudget.BgCmdTasks) != 9 || len(afterBudget.BgAgentList) != 2 {
		t.Fatalf("budget truncation wiped previous state: BgCmds=%d list=%d tasks=%v agents=%+v", afterBudget.BgCmds, len(afterBudget.BgCmdList), afterBudget.BgCmdTasks, afterBudget.BgAgentList)
	}

	// 3. Genuine non-budget incomplete scan ("more"): still clears stale lists as before.
	mode.Store(3)
	<-scans
	time.Sleep(20 * time.Millisecond)
	afterMore := readSummary()
	if afterMore.BgCmds != 0 || len(afterMore.BgAgentList) != 0 {
		t.Fatalf("non-budget 'more' scan should clear lists, got BgCmds=%d agents=%+v", afterMore.BgCmds, afterMore.BgAgentList)
	}
}

// End-to-end real script test on Linux /proc:
// 1. Spawns a fake `claude` parent whose background task runs a fake `agy`
//    executable script holding conversations/<uuid>.db open with a matching
//    brain/<uuid>/.system_generated/logs/transcript_full.jsonl, runs the real
//    `sh -c cmdScanCommand(...)`, and verifies parseCmdScan discovers the agy
//    CLI subagent (deleting `scan_cmd_tree` fails this test).
// 2. Verifies CLI byte budget cap (cmdScanCommandWithBudget) drops CLI lines
//    when exceeded without emitting `more` or losing plain `cmd` lines.
// 3. Verifies a log path containing a literal tab is never rewritten to spaces
//    to alias onto another directory.
func TestCmdScanRealFakeAgyProcess(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("reads /proc")
	}
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}

	const (
		realSess = "99999999-1111-4222-8333-444444444444"
		agySID   = "44444444-5555-4666-8777-888888888881"
	)
	home := t.TempDir()
	binDir := filepath.Join(home, "bin")
	tasksDir := filepath.Join(home, "tmp", "claude-1000", "-home-u-proj", realSess, "tasks")
	agyBase := filepath.Join(home, ".gemini", "antigravity-cli")
	agyConv := filepath.Join(agyBase, "conversations")
	agyLogDir := filepath.Join(agyBase, "brain", agySID, ".system_generated", "logs")
	for _, d := range []string{binDir, tasksDir, agyConv, agyLogDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	dbFile := filepath.Join(agyConv, agySID+".db")
	logFile := filepath.Join(agyLogDir, "transcript_full.jsonl")
	if err := os.WriteFile(dbFile, []byte("sqlite"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(logFile, []byte(`{"step_index":1,"type":"USER_INPUT","content":"hello"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Fake `claude` parent and fake `agy` executable script that keeps dbFile open on fd 3.
	fakeClaude := filepath.Join(binDir, "claude")
	if err := os.Symlink(sh, fakeClaude); err != nil {
		t.Fatal(err)
	}
	fakeAgy := filepath.Join(binDir, "agy")
	agyScript := "#!/bin/sh\necho \"$$\" > \"$2\"\nexec 3<\"$1\"\nsleep 30 &\n_sp=$!\ntrap 'kill $_sp 2>/dev/null; exit 0' TERM INT\nwait $_sp\n"
	if err := os.WriteFile(fakeAgy, []byte(agyScript), 0o755); err != nil {
		t.Fatal(err)
	}
	killPidFile := func(pidFile string) {
		if b, err := os.ReadFile(pidFile); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil && pid > 1 {
				_ = syscall.Kill(pid, syscall.SIGTERM)
			}
		}
	}

	outFile := filepath.Join(tasksDir, "breal0001.output")
	pidFile1 := filepath.Join(home, "agy1.pid")
	// Spawn fake claude -> child sh writing to tasks/breal0001.output -> fake agy opening dbFile.
	proc := exec.Command(fakeClaude, "-c", `sh -c '"$1" "$2" "$3" -p WP1' bgwrap "$1" "$2" "$3" > "$4" & wait`, "claude", fakeAgy, dbFile, pidFile1, outFile)
	if err := proc.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		killPidFile(pidFile1)
		_ = proc.Process.Kill()
		_ = proc.Wait()
	})

	// Wait until the grandchild `agy` has opened dbFile and cmdScanCommand sees it.
	var foundCmd bgCmd
	var lastOut string
	for i := 0; i < 40; i++ {
		time.Sleep(50 * time.Millisecond)
		cmd := exec.Command("sh", "-c", cmdScanCommand("s:"+realSess))
		cmd.Env = append(os.Environ(), "HOME="+home)
		out, err := cmd.Output()
		if err != nil {
			t.Fatal(err)
		}
		lastOut = string(out)
		got, whole := parseCmdScan(lastOut)
		if !whole {
			t.Fatalf("scan not whole:\n%s", lastOut)
		}
		for _, c := range got[realSess] {
			if c.Task == "breal0001" && len(c.clis) > 0 {
				foundCmd = c
				break
			}
		}
		if len(foundCmd.clis) > 0 {
			break
		}
	}
	if len(foundCmd.clis) != 1 || foundCmd.clis[0].Agent != "agy" || foundCmd.clis[0].Session != agySID || foundCmd.clis[0].Log != logFile {
		t.Fatalf("real cmdScanCommand did not recognize fake agy: found=%+v\noutput:\n%s", foundCmd, lastOut)
	}

	// Subtest (Round 4 #5): tiny CLI byte budget drops the required cli line,
	// preserves plain cmd, and emits "budget" (whole=false, budgetCut=true) so
	// cmdProber keeps previous lists instead of treating dropped CLIs as ended.
	budgetCmd := exec.Command("sh", "-c", cmdScanCommandWithBudget(16, "s:"+realSess))
	budgetCmd.Env = append(os.Environ(), "HOME="+home)
	budgetOut, err := budgetCmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(budgetOut), "\ncli\t") {
		t.Fatalf("expected cli line to be dropped under 16-byte budget:\n%s", budgetOut)
	}
	bgot, bwhole, bcut := parseCmdScanDetail(string(budgetOut))
	if bwhole || !bcut {
		t.Fatalf("dropping a required cli line must report whole=false, budgetCut=true, got whole=%v budgetCut=%v:\n%s", bwhole, bcut, budgetOut)
	}
	if len(bgot[realSess]) != 1 || bgot[realSess][0].Task != "breal0001" || len(bgot[realSess][0].clis) != 0 {
		t.Fatalf("expected plain cmd preserved when CLI budget exhausted, got %+v", bgot[realSess])
	}

	// Subtest (Round 3 P2-4): when even plain cmd lines exceed totalBudget, the
	// script emits "budget" (not "more") so parseCmdScanDetail reports budgetCut=true.
	tinyCmd := exec.Command("sh", "-c", cmdScanCommandWithBudgets(40, cliScanBudget, "s:"+realSess))
	tinyCmd.Env = append(os.Environ(), "HOME="+home)
	tinyOut, err := tinyCmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	_, tinyWhole, tinyBudgetCut := parseCmdScanDetail(string(tinyOut))
	if tinyWhole || !tinyBudgetCut {
		t.Fatalf("expected whole=false, budgetCut=true on 40-byte total budget, got whole=%v budgetCut=%v\n%s", tinyWhole, tinyBudgetCut, tinyOut)
	}

	// Subtest (Backend review #1): a log directory containing a literal TAB must
	// NOT be rewritten to spaces to alias onto a sibling space-named directory.
	const tabSID = "55555555-6666-4777-8888-999999999991"
	tabBase := filepath.Join(home, "dir\twith\ttab", ".gemini", "antigravity-cli")
	spaceBase := filepath.Join(home, "dir with tab", ".gemini", "antigravity-cli")
	for _, b := range []string{tabBase, spaceBase} {
		for _, sub := range []string{"conversations", filepath.Join("brain", tabSID, ".system_generated", "logs")} {
			if err := os.MkdirAll(filepath.Join(b, sub), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.WriteFile(filepath.Join(b, "conversations", tabSID+".db"), []byte("sqlite"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(b, "brain", tabSID, ".system_generated", "logs", "transcript_full.jsonl"), []byte("{}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	tabOut := filepath.Join(tasksDir, "btab00001.output")
	pidFile2 := filepath.Join(home, "agy2.pid")
	tabProc := exec.Command(fakeClaude, "-c", `sh -c '"$1" "$2" "$3" -p tab' bgwrap "$1" "$2" "$3" > "$4" & wait`, "claude", fakeAgy, filepath.Join(tabBase, "conversations", tabSID+".db"), pidFile2, tabOut)
	if err := tabProc.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		killPidFile(pidFile2)
		_ = tabProc.Process.Kill()
		_ = tabProc.Wait()
	})
	// Wait until btab00001 appears in scan output, then verify it has NO cli promoted onto spaceBase.
	seenTabTask := false
	for i := 0; i < 30; i++ {
		time.Sleep(50 * time.Millisecond)
		cmd := exec.Command("sh", "-c", cmdScanCommand("s:"+realSess))
		cmd.Env = append(os.Environ(), "HOME="+home)
		out, err := cmd.Output()
		if err != nil {
			t.Fatal(err)
		}
		got, _ := parseCmdScan(string(out))
		for _, c := range got[realSess] {
			if c.Task == "btab00001" {
				seenTabTask = true
				if len(c.clis) != 0 {
					t.Fatalf("tab-containing path was aliased into CLI subagent: %+v\n%s", c.clis, out)
				}
			}
		}
		if seenTabTask {
			break
		}
	}
	if !seenTabTask {
		t.Fatal("btab00001 did not appear in scan")
	}
}

// Round 4 #5 & #6: Standalone real-script test for subagent budget & candidate cap:
// 1. Omitting ONLY subagent description text still counts as complete (whole=true, budgetCut=false).
// 2. Dropping a required sub line emits "budget" (whole=false, budgetCut=true).
// 3. Candidates are scanned newest-first by mtime (20 -> newest 16 with whole=true).
// 4. Hitting subCandPerSession emits "budget" (whole=false, budgetCut=true) so
//    cmdProber retains previous lists instead of treating unscanned active subagents as done.
func TestCmdScanSubagentBudgetAndCap(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("reads /proc")
	}
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	const (
		budgetSess = "99999999-1111-4222-8333-444444444488"
		capSess    = "99999999-1111-4222-8333-444444444499"
	)
	if strings.Contains(cmdScanCommand("s:"+budgetSess), "_sc_subf=$(") {
		t.Fatalf("cmdScanCommand must stream find output instead of buffering into a shell variable")
	}

	home := t.TempDir()
	budgetSubDir := filepath.Join(home, ".claude", "projects", "-home-u-proj", budgetSess, "subagents")
	if err := os.MkdirAll(budgetSubDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(budgetSubDir, "agent-subbudget1.jsonl"), []byte(`{"version":"2.1.331"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	longDesc := strings.Repeat("x", 580)
	if err := os.WriteFile(filepath.Join(budgetSubDir, "agent-subbudget1.meta.json"), []byte(`{"requestShape":"background","description":"`+longDesc+`"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	// Measure exact baseline without watched subagents (clock + any unrelated host procs).
	baseCmd := exec.Command("sh", "-c", cmdScanCommandWithBudgets(cmdScanTotalBudget, cliScanBudget))
	baseCmd.Env = append(os.Environ(), "HOME="+home)
	baseOut, err := baseCmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	// If any unrelated CLI line is present on the host, subtract it for the drop-sub budget.
	cliLen := 0
	if idx := strings.Index(string(baseOut), "\ncli\t"); idx >= 0 {
		cliLen = len(baseOut) - (idx + 1)
	}
	if cliLen > 0 {
		t.Skip("unrelated live CLI process on host interferes with exact sub-only byte budget")
	}

	// 1. Budget fits baseline + description-less sub (~80B), but NOT the 580B description -> whole=true, budgetCut=false.
	descOnlyTotal := len(baseOut) + 120
	tightCmd := exec.Command("sh", "-c", cmdScanCommandWithBudgets(descOnlyTotal, cliScanBudget, "s:"+budgetSess))
	tightCmd.Env = append(os.Environ(), "HOME="+home)
	tightOut, err := tightCmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	if len(tightOut) > descOnlyTotal {
		t.Fatalf("unified budget exceeded: len=%d > budget=%d\n%s", len(tightOut), descOnlyTotal, tightOut)
	}
	_, tWhole, tCut := parseCmdScanDetail(string(tightOut))
	if !tWhole || tCut {
		t.Fatalf("omitting only sub description must stay whole=true, budgetCut=false; got whole=%v budgetCut=%v:\n%s", tWhole, tCut, tightOut)
	}
	tSubs := parseBgSubScan(string(tightOut))
	if len(tSubs[budgetSess]) != 1 || tSubs[budgetSess][0].ID != "subbudget1" || tSubs[budgetSess][0].What != "" {
		t.Fatalf("expected sub preserved with description omitted under tight budget, got %+v\n%s", tSubs[budgetSess], tightOut)
	}

	// 2. Budget fits baseline, but NOT even a description-less sub line -> whole=false, budgetCut=true.
	dropSubTotal := len(baseOut) + 10
	dropSubCmd := exec.Command("sh", "-c", cmdScanCommandWithBudgets(dropSubTotal, cliScanBudget, "s:"+budgetSess))
	dropSubCmd.Env = append(os.Environ(), "HOME="+home)
	dropSubOut, err := dropSubCmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	_, dsWhole, dsCut := parseCmdScanDetail(string(dropSubOut))
	if dsWhole || !dsCut {
		t.Fatalf("dropping a required sub line must report whole=false, budgetCut=true; got whole=%v budgetCut=%v:\n%s", dsWhole, dsCut, dropSubOut)
	}

	// 3. Newest-first mtime ordering (20 recent active subagents -> newest 16, whole=true, budgetCut=false).
	capSubDir := filepath.Join(home, ".claude", "projects", "-home-u-proj", capSess, "subagents")
	if err := os.MkdirAll(capSubDir, 0o755); err != nil {
		t.Fatal(err)
	}
	baseTime := time.Now().Add(-2 * time.Minute)
	for i := 0; i < 20; i++ {
		id := fmt.Sprintf("subord%02d", i)
		jp := filepath.Join(capSubDir, "agent-"+id+".jsonl")
		mp := filepath.Join(capSubDir, "agent-"+id+".meta.json")
		if err := os.WriteFile(jp, []byte(`{"version":"2.1.331"}`+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(mp, []byte(`{"requestShape":"background","description":"Task"}`), 0o644); err != nil {
			t.Fatal(err)
		}
		mt := baseTime.Add(time.Duration(i) * time.Second)
		if err := os.Chtimes(jp, mt, mt); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(mp, mt, mt); err != nil {
			t.Fatal(err)
		}
	}
	ordCmd := exec.Command("sh", "-c", cmdScanCommand("s:"+capSess))
	ordCmd.Env = append(os.Environ(), "HOME="+home)
	ordOut, err := ordCmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	_, ordWhole, ordCut := parseCmdScanDetail(string(ordOut))
	if !ordWhole || ordCut {
		t.Fatalf("20 candidates (< %d cap) must complete with whole=true, budgetCut=false; got whole=%v cut=%v\n%s", subCandPerSession, ordWhole, ordCut, ordOut)
	}
	ordSubs := parseBgSubScan(string(ordOut))
	if len(ordSubs[capSess]) != subsScanned {
		t.Fatalf("newest-first subagent scan got %d subs, want %d", len(ordSubs[capSess]), subsScanned)
	}
	seenSub := map[string]bool{}
	for _, s := range ordSubs[capSess] {
		seenSub[s.ID] = true
	}
	for i := 4; i < 20; i++ {
		id := fmt.Sprintf("subord%02d", i)
		if !seenSub[id] {
			t.Fatalf("newest-first scan missed newer subagent %s (got %+v)", id, ordSubs[capSess])
		}
	}

	// 4. Exceeding subCandPerSession with finished noise files -> whole=false, budgetCut=true.
	for i := 0; i < subCandPerSession+5; i++ {
		id := fmt.Sprintf("subdone%04d", i)
		if err := os.WriteFile(filepath.Join(capSubDir, "agent-"+id+".jsonl"), []byte(`{"version":"2.1.331"}`+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(capSubDir, "agent-"+id+".meta.json"), []byte(`{"requestShape":"background","finished":true}`), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	capCmd := exec.Command("sh", "-c", cmdScanCommand("s:"+capSess))
	capCmd.Env = append(os.Environ(), "HOME="+home)
	capOut, err := capCmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	_, capWhole, capCut := parseCmdScanDetail(string(capOut))
	if capWhole || !capCut {
		t.Fatalf("exceeding subCandPerSession (%d) must report whole=false, budgetCut=true; got whole=%v cut=%v\n%s", subCandPerSession, capWhole, capCut, capOut)
	}
}

// HTTP + SFTP integration test for GET /api/sessions/{id}/conversation/subagent:
// - First-level scan-registered background CLIs (agy, claude, codex) return 200 OK.
// - Unregistered UUIDs return 404 Not Found.
// - Any request carrying cli_parent or cli_chain is rejected with 400 Bad Request
//   (nested CLI subagent traversal is intentionally unsupported).
func TestApiConversationSubagentBgCLI(t *testing.T) {
	h := newHarness(t)
	admin := h.admin()
	home := t.TempDir()
	srv := h.sshServer(home)

	r := admin.do("POST", "/api/hosts", map[string]any{
		"name": "box", "address": "127.0.0.1", "port": srv.Addr.Port,
		"username": "alice", "auth": "password", "password": "alice-pass",
	})
	mustStatus(t, r, 201, "create host")
	hostID := int64(r.json()["id"].(float64))

	r = admin.do("POST", "/api/sessions", map[string]any{"host_id": hostID})
	mustStatus(t, r, 201, "open session")
	sessID := r.json()["id"].(string)

	me := admin.do("GET", "/api/me", nil)
	adminID := int64(me.json()["id"].(float64))
	sess, err := h.terms.Get(adminID, sessID)
	if err != nil {
		t.Fatalf("terms.Get: %v", err)
	}
	sess.Persistence = "herdr"

	const (
		agySID      = "66666666-7777-4888-8999-aaaaaaaaaaa1"
		claudeSID   = "77777777-8888-4999-8aaa-bbbbbbbbbbb2"
		codexSID    = "01a1dd7f-4856-7042-8ad2-f46c56372e01"
		nestedSubID = "a987654321"
		unknownUUID = "88888888-9999-4aaa-8bbb-ccccccccccc3"
	)

	// 1. Write a real agy transcript_full.jsonl.
	agyLog := filepath.Join(home, ".gemini", "antigravity-cli", "brain", agySID, ".system_generated", "logs", "transcript_full.jsonl")
	if err := os.MkdirAll(filepath.Dir(agyLog), 0o755); err != nil {
		t.Fatal(err)
	}
	agyContent := strings.Join([]string{
		`{"step_index":1,"type":"USER_INPUT","content":"<USER_REQUEST>\nRun WP1\n</USER_REQUEST>"}`,
		`{"step_index":2,"type":"PLANNER_RESPONSE","content":"WP1 complete."}`,
	}, "\n") + "\n"
	if err := os.WriteFile(agyLog, []byte(agyContent), 0o644); err != nil {
		t.Fatal(err)
	}

	// 2. Write a real claude -p log.
	claudeLog := filepath.Join(home, ".claude", "projects", "-home-u-proj", claudeSID+".jsonl")
	if err := os.MkdirAll(filepath.Dir(claudeLog), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(claudeLog, []byte(`{"type":"assistant","uuid":"m1","message":{"content":[{"type":"text","text":"claude bg main reply"}]}}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// 3. Write a real codex exec rollout log.
	codexLog := filepath.Join(home, ".codex", "sessions", "2026", "11", "27", "rollout-2026-11-27T09-00-00-"+codexSID+".jsonl")
	if err := os.MkdirAll(filepath.Dir(codexLog), 0o755); err != nil {
		t.Fatal(err)
	}
	codexContent := strings.Join([]string{
		`{"timestamp":"2026-11-27T09:00:00Z","type":"session_meta","payload":{"id":"` + codexSID + `"}}`,
		`{"timestamp":"2026-11-27T09:00:01Z","type":"response_item","payload":{"type":"message","id":"m1","role":"assistant","content":[{"type":"output_text","text":"codex bg main reply"}]}}`,
	}, "\n") + "\n"
	if err := os.WriteFile(codexLog, []byte(codexContent), 0o644); err != nil {
		t.Fatal(err)
	}

	// Seed cmdProber with verified host-scanned CLI logs.
	p := newCmdSubProber(nil)
	p.recordCLILogsLocked(map[string][]bgCmd{
		cmdSess: {
			{Task: "b1", clis: []bgCLI{{Agent: "agy", Session: agySID, Log: agyLog}}},
			{Task: "b2", clis: []bgCLI{{Agent: "claude", Session: claudeSID, Log: claudeLog}}},
			{Task: "b3", clis: []bgCLI{{Agent: "codex", Session: codexSID, Log: codexLog}}},
		},
	}, time.Now())
	h.srv.herdrSocks.setCmds(sessID, p)

	// 1. Verified first-level agy, claude, codex background CLI conversations succeed.
	resAgy := admin.do("GET", "/api/sessions/"+sessID+"/conversation/subagent?pane=w1:p1&agent="+agySID+"&cli=agy&gen=0&rev=0", nil)
	mustStatus(t, resAgy, http.StatusOK, "read agy bg cli")
	if !strings.Contains(string(resAgy.Body), "WP1 complete.") {
		t.Fatalf("expected parsed agy reply in body, got: %s", resAgy.Body)
	}

	resClaude := admin.do("GET", "/api/sessions/"+sessID+"/conversation/subagent?pane=w1:p1&agent="+claudeSID+"&cli=claude&gen=0&rev=0", nil)
	mustStatus(t, resClaude, http.StatusOK, "read claude bg cli")
	if !strings.Contains(string(resClaude.Body), "claude bg main reply") {
		t.Fatalf("expected parsed claude reply in body, got: %s", resClaude.Body)
	}

	resCodex := admin.do("GET", "/api/sessions/"+sessID+"/conversation/subagent?pane=w1:p1&agent="+codexSID+"&cli=codex&gen=0&rev=0", nil)
	mustStatus(t, resCodex, http.StatusOK, "read codex bg cli")
	if !strings.Contains(string(resCodex.Body), "codex bg main reply") {
		t.Fatalf("expected parsed codex reply in body, got: %s", resCodex.Body)
	}

	// 2. Unverified UUID is rejected with 404.
	res404 := admin.do("GET", "/api/sessions/"+sessID+"/conversation/subagent?pane=w1:p1&agent="+unknownUUID+"&cli=agy&gen=0&rev=0", nil)
	mustStatus(t, res404, http.StatusNotFound, "unverified agy sid")

	// 3. Any request with cli_parent or cli_chain MUST return 400 Bad Request.
	for _, tc := range []struct {
		name string
		q    string
	}{
		{"cli_parent with cli=claude", "pane=w1:p1&agent=" + nestedSubID + "&cli=claude&cli_parent=" + claudeSID + "&gen=0&rev=0"},
		{"cli_parent with cli=agy", "pane=w1:p1&agent=" + unknownUUID + "&cli=agy&cli_parent=" + agySID + "&gen=0&rev=0"},
		{"cli_chain with cli=codex", "pane=w1:p1&agent=" + unknownUUID + "&cli=codex&cli_chain=" + codexSID + "&gen=0&rev=0"},
		{"cli_parent without cli", "pane=w1:p1&agent=" + nestedSubID + "&cli_parent=" + claudeSID + "&gen=0&rev=0"},
		{"cli_chain without cli", "pane=w1:p1&agent=" + nestedSubID + "&cli_chain=hop1&gen=0&rev=0"},
	} {
		res400 := admin.do("GET", "/api/sessions/"+sessID+"/conversation/subagent?"+tc.q, nil)
		mustStatus(t, res400, http.StatusBadRequest, tc.name)
	}
}
