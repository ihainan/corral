package web

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ihainan/corral/internal/convo"
)

func TestCodexDaemonMessageTail(t *testing.T) {
	dir := t.TempDir()
	a, b := filepath.Join(dir, filepath.Base(cxA)), filepath.Join(dir, filepath.Base(cxB))
	const said = "原工作区和 tabs 会保留；运行中、存在后台任务、缺少映射或状态不明的会话先跳过。"
	tools := cxRow(t, "response_item", map[string]any{"type": "function_call_output", "output": strings.Repeat("tool output ", 50000)})
	for _, file := range []string{a, b} {
		message := said
		if file == b {
			message = "另一会话只负责独立的数据库和后端接口开发。"
		}
		if err := os.WriteFile(file, []byte(cxSaid(t, "m", message)+tools), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	logs := []string{a, b}
	read := func(command string) map[string]string {
		t.Helper()
		out, err := exec.Command("sh", "-c", command).Output()
		if err != nil || len(out) > len(logs)*(72<<10) {
			t.Fatalf("read failed or exceeded transfer bound: %v, %d bytes", err, len(out))
		}
		return splitTails(string(out), logs)
	}
	if log, _, _ := screenPick(letters(said), read(convoTailsCommand(logs))); log != "" {
		t.Fatal("fixture did not reproduce the ordinary tail losing the message")
	}
	tails := read(convoMessageTailsCommand(logs))
	if log, _, on := screenPick(letters(said), tails); log != a || on < screenEnough2 {
		t.Fatalf("busy daemon conversation not found: %q, evidence %d", log, on)
	}
	// Shared words remain ambiguous even when hidden behind tools in both logs.
	if err := os.WriteFile(b, []byte(cxSaid(t, "m", said)+tools), 0o600); err != nil {
		t.Fatal(err)
	}
	if log, _, _ := screenPick(letters(said), read(convoMessageTailsCommand(logs))); log != "" {
		t.Fatal("shared words were treated as proof of one session")
	}
	// A rollback retained after the message still invalidates that evidence.
	data := cxRow(t, "event_msg", map[string]any{"type": "task_started"}) + cxSaid(t, "m", said) + tools +
		cxRow(t, "event_msg", map[string]any{"type": "thread_rolled_back", "num_turns": 1})
	if err := os.WriteFile(a, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	if log, _, _ := screenPick(letters(said), read(convoMessageTailsCommand([]string{a}))); log != "" {
		t.Fatal("rolled-back message was used as evidence")
	}
}

func TestCodexDaemonSubagents(t *testing.T) {
	for _, tc := range []struct {
		meta string
		sub  bool
	}{
		{`{"source":"vscode","thread_source":"user"}`, false},
		{`{"source":"cli"}`, false},
		{`{"thread_source":"subagent"}`, true},
		{`{"source":{"subagent":{"other":"guardian"}},"thread_source":"guardian_review"}`, true},
		{`{"source": { "subagent": {"thread_spawn":{"parent_thread_id":"x"}}}}`, true},
	} {
		file := filepath.Join(t.TempDir(), "rollout.jsonl")
		if err := os.WriteFile(file, []byte(tc.meta), 0o600); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command("sh", "-c", codexFdsSh+"\ncodex_subagent \"$1\"", "sh", file)
		err := cmd.Run()
		if (err == nil) != tc.sub {
			t.Errorf("subagent detection for %s: %v", tc.meta, err)
		}
	}
}

func TestCodexDaemonToolEvidence(t *testing.T) {
	const command = "rg -n 'codex identity guards' internal/web/conversation.go"
	call := cxRow(t, "response_item", map[string]any{"type": "function_call", "name": "exec_command", "call_id": "c1", "arguments": `{"cmd":"` + command + `"}`})
	tails := map[string]string{cxA: call, cxB: cxSaid(t, "b", "另一会话正在处理数据库迁移和浏览器兼容问题。")}
	raw := "• Ran " + command + "\n  └ no matches\n• Working\n› Ask Codex to do anything"
	screen := letters(raw)
	if log, _, on := screenPick(screen, tails, screenToolLines(raw)...); log != cxA || on < screenEnough2 {
		t.Fatalf("tool-only screen not recognized: %q, %d", log, on)
	}
	quoted := "• Ran unrelated-command\n  └ " + command + "\n    • Ran " + command
	if log, _, _ := screenPick(letters(quoted), tails, screenToolLines(quoted)...); log != "" {
		t.Fatal("a command quoted inside tool output was used as identity evidence")
	}
	tails[cxB] = call
	if log, _, _ := screenPick(screen, tails, screenToolLines(raw)...); log != "" {
		t.Fatal("identical commands in different conversations were treated as unique")
	}
	tails[cxA] = cxRow(t, "event_msg", map[string]any{"type": "task_started"}) + call +
		cxRow(t, "event_msg", map[string]any{"type": "thread_rolled_back", "num_turns": 1})
	delete(tails, cxB)
	if log, _, _ := screenPick(screen, tails, screenToolLines(raw)...); log != "" {
		t.Fatal("a rolled-back tool call was used as evidence")
	}
	tails[cxA] = cxRow(t, "response_item", map[string]any{"type": "function_call_output", "call_id": "c2", "output": command})
	if log, _, _ := screenPick(screen, tails, screenToolLines(raw)...); log != "" {
		t.Fatal("arbitrary tool output was used as proof of session identity")
	}
}

func TestCodexDaemonCandidateLimit(t *testing.T) {
	l := convoLocated{}
	for i := 0; i <= maxScreenLogs; i++ {
		l.cands = append(l.cands, convoCand{log: "/unique/" + strconv.Itoa(i), cwd: "/w"})
	}
	// It must refuse before any SSH read, rather than dropping candidates
	// and claiming uniqueness against only part of the set.
	if _, sid, _, read := (&Server{}).screenSays(context.Background(), nil, "", &convoProc{cwd: "/w"}, l); sid != "" || read {
		t.Fatal("incomplete candidate set was used for session attribution")
	}
}

// Explicit opt-in, read-only inspection of live panes. No keys, input,
// focus changes, or CLI restarts. Each expected ID must be supplied by the
// operator independently of the matching algorithm.
func TestCodexDaemonLiveBinding(t *testing.T) {
	raw := os.Getenv("CORRAL_CODEX_LIVE_PANES")
	if raw == "" {
		t.Skip("set CORRAL_CODEX_LIVE_PANES to inspect explicitly selected live panes")
	}
	var panes map[string]string
	if err := json.Unmarshal([]byte(raw), &panes); err != nil || len(panes) == 0 {
		t.Fatalf("expected pane-to-session mapping: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	run := func(command string) []byte {
		t.Helper()
		out, err := exec.CommandContext(ctx, "sh", "-c", command).Output()
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	began := time.Now()
	l := parseConvoLocate(string(run(convoLocateCommand("herdr", "", ""))))
	t.Logf("host scan: %d processes, %d main candidates, %s", len(l.procs), len(l.cands), time.Since(began))
	for pane, want := range panes {
		t.Run(pane, func(t *testing.T) {
			p := pickConvoProc(l.procs, "herdr", pane, os.Getenv("CORRAL_CODEX_LIVE_SOCKET"), "", "", "")
			if p == nil || p.agent != "codex" {
				t.Fatal("live Codex was not located")
			}
			var logs []string
			for _, c := range l.cands {
				if c.cwd == p.cwd {
					logs = append(logs, c.log)
				}
			}
			if len(logs) == 0 || len(logs) > maxScreenLogs {
				t.Fatalf("invalid candidate count: %d", len(logs))
			}
			rawScreen := string(run(convoScreenCommand("herdr", pane, p.herdrSocket)))
			screen := letters(rawScreen)
			began := time.Now()
			out := run(convoMessageTailsCommand(logs))
			tails := splitTails(string(out), logs)
			if len(tails) != len(logs) {
				t.Fatal("incomplete log evidence")
			}
			log, sid, on := screenPick(screen, tails, screenToolLines(rawScreen)...)
			if sid != want || on < screenEnough2 {
				for file, tail := range tails {
					c := convo.NewCodex()
					c.Feed([]byte(tail))
					e := c.Since(0)
					n := 0
					for _, s := range lastSaid(e, 8) {
						if strings.Contains(screen, s) {
							n++
						}
					}
					t.Logf("candidate %s: %d bytes, %d entries, %d pieces visible", convoSessionOf(file), len(tail), len(e), n)
				}
				t.Fatalf("got session %s, evidence %d; expected %s", sid, on, want)
			}
			if candAt(l, log, p.cwd) == nil {
				t.Fatal("matched log is not held by the daemon in this directory")
			}
			// Execute only the actual send gate's front() function, without
			// running any code that can paste or send a key to the pane.
			gate := convoGated("", "herdr", "", pane, p.herdrSocket, p.pid, p.start, sid, "0", nil, 0)
			start := strings.Index(gate, "front() {")
			end := strings.Index(gate, "\nfront || exit 3")
			if start < 0 || end < start {
				t.Fatal("send gate could not be isolated")
			}
			front := strings.ReplaceAll(gate[start:end], `'\''`, "'")
			check := codexFdsSh + "\nmode=herdr\np=$1 pid=$2 start=$3 sid=$4\n" + front + "\nfront"
			args := []string{"-c", check, "corral-read-only", pane, strconv.FormatInt(p.pid, 10), strconv.FormatInt(p.start, 10), sid}
			if out, err := exec.CommandContext(ctx, "sh", args...).CombinedOutput(); err != nil {
				t.Fatalf("actual send identity check rejected this pane: %v, %s", err, out)
			}
			args[5] = "0" // a stale process generation must still be refused
			if err := exec.CommandContext(ctx, "sh", args...).Run(); err == nil {
				t.Fatal("stale process generation passed the send identity check")
			}
			t.Logf("pane %s pid %d -> %s, evidence %d, transfer %d bytes, match %s", pane, p.pid, sid, on, len(out), time.Since(began))
		})
	}
}
