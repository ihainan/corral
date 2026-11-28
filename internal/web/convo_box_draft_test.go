package web

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestInspectScreenBlockerClaudeRealFixtures(t *testing.T) {
	readFixture := func(rel string) string {
		t.Helper()
		b, err := os.ReadFile(filepath.Join("testdata", rel))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}

	// Real Claude Code 2.1.312 screen with unsent text restored after stop.
	restored := readFixture(filepath.Join("view", "restored-after-stop.ansi"))
	got := inspectScreenBlocker("claude", restored, false)
	if got.Code != "box_draft" || got.Draft != "分析现在是否可以部署" || got.Digest == "" {
		t.Fatalf("restored-after-stop.ansi: got %+v, want box_draft with text and digest", got)
	}

	// Real Claude Code screen with unsent Chinese draft after NBSP.
	draftNBSP := readFixture(filepath.Join("view", "main-draft-nbsp.txt"))
	got = inspectScreenBlocker("claude", draftNBSP, false)
	if got.Code != "box_draft" || got.Draft != "一行草稿" || got.Digest == "" {
		t.Fatalf("main-draft-nbsp.txt: got %+v, want box_draft", got)
	}

	// Exact scenario from screenshot 7.png: idle Claude Code with unsent Chinese text.
	rule := strings.Repeat("─", 80)
	shot7 := "● Codex 会话要重新打开才能用上这个技能。\n\n✻ Cogitated for 16s · done 8:38 AM\n\n" +
		rule + "\n❯ 重新登录，我不要系统管理员，我要换一个用户\n" + rule +
		"\n  Opus 5.5·H │ Ctx 75.3K (7.5% / u 9.4%) │ $1.14 (10h2m) │ wk 23% (1d13h)\n" +
		"  ⏵⏵ auto mode on (shift+tab to cycle)\n"
	got = inspectScreenBlocker("claude", shot7, false)
	if got.Code != "box_draft" || got.Draft != "重新登录，我不要系统管理员，我要换一个用户" || got.Digest != boxDigest(shot7) {
		t.Fatalf("screenshot 7 scenario: got %+v", got)
	}

	// Once cleared or sent, the clean empty main input box resolves all blockers (Code == "").
	plainMain := readFixture(filepath.Join("view", "main-plain.txt"))
	if got := inspectScreenBlocker("claude", plainMain, false); got.Code != "" {
		t.Fatalf("main-plain.txt: got %+v, want clear", got)
	}

	// Dim suggestion or placeholder in main box must NOT be treated as a draft.
	dimSuggest := "● Done.\n" + rule + "\n❯ \x1b[2m提交并推送\x1b[0m\n" + rule + "\n  ⏵⏵ auto mode on\n"
	if got := inspectScreenBlocker("claude", dimSuggest, false); got.Code != "" {
		t.Fatalf("dim suggestion: got %+v, want clear", got)
	}

	// Main box with agents footer / mouse hover cursor stays clear.
	for _, name := range []string{"main-agents-footer.txt", "main-hover-main.txt"} {
		s := readFixture(filepath.Join("view", name))
		if got := inspectScreenBlocker("claude", s, false); got.Code != "" {
			t.Fatalf("%s: got %+v, want clear", name, got)
		}
	}

	// Subagent conversation shown in front.
	for _, name := range []string{"subagent-typed.txt", "subagent-live-fullscreen.txt"} {
		s := readFixture(filepath.Join("view", name))
		if got := inspectScreenBlocker("claude", s, false); got.Code != "subagent_view" {
			t.Fatalf("%s: got %+v, want subagent_view", name, got)
		}
	}

	// Agent list focus and background sessions view -> other_view.
	for _, name := range []string{"list-focus-main.txt", "sessions-view.txt"} {
		s := readFixture(filepath.Join("view", name))
		if got := inspectScreenBlocker("claude", s, false); got.Code != "other_view" {
			t.Fatalf("%s: got %+v, want other_view", name, got)
		}
	}

	// Permission dialog -> agent_waiting.
	perm := readFixture(filepath.Join("view", "permission-dialog.txt"))
	if got := inspectScreenBlocker("claude", perm, false); got.Code != "agent_waiting" {
		t.Fatalf("permission-dialog.txt: got %+v, want agent_waiting", got)
	}

	// Model picker -> picker.
	picker := readFixture("model-picker-120.txt")
	if got := inspectScreenBlocker("claude", picker, false); got.Code != "picker" {
		t.Fatalf("model-picker-120.txt: got %+v, want picker", got)
	}

	// Waiting flag with empty box still reports agent_waiting.
	if got := inspectScreenBlocker("claude", plainMain, true); got.Code != "agent_waiting" {
		t.Fatalf("waiting=true with plainMain: got %+v, want agent_waiting", got)
	}

	// Unrecognized / scrolled screen -> no_input (must not falsely report clear).
	if got := inspectScreenBlocker("claude", "$ ls\nREADME.md\n", false); got.Code != "no_input" {
		t.Fatalf("unrecognized screen: got %+v, want no_input", got)
	}
}

func TestInspectScreenBlockerOtherAgents(t *testing.T) {
	// Codex: dim placeholder is clear.
	codexClear := "Some output\n\n› \x1b[2mAsk Codex to do anything\x1b[0m\n\n  ? for shortcuts\n"
	if got := inspectScreenBlocker("codex", codexClear, false); got.Code != "" {
		t.Fatalf("codex clear: got %+v, want clear", got)
	}

	// Codex: typed draft returns draft + text + digest.
	codexDraft := "Some output\n\n› 请帮我检查这个接口\n\n  gpt-5.4 high · ~/proj\n"
	got := inspectScreenBlocker("codex", codexDraft, false)
	if got.Code != "draft" || got.Draft != "请帮我检查这个接口" || got.Digest == "" {
		t.Fatalf("codex draft: got %+v, want draft with text and digest", got)
	}

	// Codex: folded paste returns draft without extractable text (falls back to terminal).
	codexFolded := "Some output\n\n› [Pasted Content 1200 chars]\n\n  gpt-5.4 high · ~/proj\n"
	got = inspectScreenBlocker("codex", codexFolded, false)
	if got.Code != "draft" || got.Draft != "" || got.Digest != "" {
		t.Fatalf("codex folded paste: got %+v, want draft without text/digest", got)
	}

	// Antigravity CLI (agy): empty/grey placeholder clear, typed text draft, prompt agent_waiting.
	agyRuleLine := strings.Repeat("─", 40)
	agyClear := "Done.\n" + agyRuleLine + "\n> \x1b[90mAuto mode: Type a message\x1b[0m\n" + agyRuleLine + "\n"
	if got := inspectScreenBlocker("agy", agyClear, false); got.Code != "" {
		t.Fatalf("agy clear: got %+v, want clear", got)
	}
	agyDraft := "Done.\n" + agyRuleLine + "\n> unsent agy draft\n" + agyRuleLine + "\n"
	if got := inspectScreenBlocker("agy", agyDraft, false); got.Code != "draft" {
		t.Fatalf("agy draft: got %+v, want draft", got)
	}
	agyAsk := "Requesting permission for:\n> 1. Yes\n  2. No\nenter Confirm · esc Skip\n"
	if got := inspectScreenBlocker("agy", agyAsk, false); got.Code != "agent_waiting" {
		t.Fatalf("agy ask: got %+v, want agent_waiting", got)
	}

	// OpenCode: empty placeholder clear, typed text draft, permission prompt agent_waiting.
	ocClear := "  ┃\n  ┃  Ask anything…\n  ┃\n  ┃  Build · DeepSeek V4.1\n  ╹▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀\n"
	if got := inspectScreenBlocker("opencode", ocClear, false); got.Code != "" {
		t.Fatalf("opencode clear: got %+v, want clear", got)
	}
	ocDraft := "  ┃\n  ┃  hello opencode\n  ┃\n  ┃  Build · DeepSeek V4.1\n  ╹▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀\n"
	if got := inspectScreenBlocker("opencode", ocDraft, false); got.Code != "draft" {
		t.Fatalf("opencode draft: got %+v, want draft", got)
	}
	ocAsk := "Permission required\nAllow once\nenter confirm · esc dismiss\n"
	if got := inspectScreenBlocker("opencode", ocAsk, false); got.Code != "agent_waiting" {
		t.Fatalf("opencode ask: got %+v, want agent_waiting", got)
	}
}

func TestHerdrAgentKind(t *testing.T) {
	if got := herdrAgentKind("claude", false); got != "claude" {
		t.Fatalf("claude: %q", got)
	}
	if got := herdrAgentKind("codex", false); got != "codex" {
		t.Fatalf("codex: %q", got)
	}
	if got := herdrAgentKind("opencode", false); got != "opencode" {
		t.Fatalf("opencode: %q", got)
	}
	if got := herdrAgentKind("claude", true); got != "agy" {
		t.Fatalf("agyPane=true: %q", got)
	}
	if got := herdrAgentKind("antigravity", false); got != "agy" {
		t.Fatalf("antigravity: %q", got)
	}
}

// Verify the host-side box:<digest> gate for both sending (enter) and clearing
// (ctrl+c) an unsent terminal draft: keys are sent only when the exact draft
// is still in the main input box and Claude Code is not waiting; any screen
// change, empty box, waiting state, or subagent switch refuses with zero keys.
func TestBoxDraftClearGateOnHost(t *testing.T) {
	for _, p := range []string{"sh", "python3", "sha256sum"} {
		if _, err := exec.LookPath(p); err != nil {
			t.Skip("no " + p)
		}
	}
	dir := t.TempDir()
	// Foreground process on a pseudo-terminal, no hardcoded uid/HOME.
	child := exec.Command("python3", "-c", `
import os, pty, signal, sys, time
m, s = pty.openpty()
pid = os.fork()
if pid == 0:
    os.setsid()
    import fcntl, termios
    fcntl.ioctl(s, termios.TIOCSCTTY, 0)
    os.dup2(s, 0); os.dup2(s, 1); os.dup2(s, 2)
    os.close(m); os.close(s)
    os.write(3, f"{os.getpid()}\n".encode())
    os.close(3)
    while True:
        time.sleep(10)
os.close(s)
signal.signal(signal.SIGTERM, lambda *_: (os.kill(pid, signal.SIGTERM), sys.exit(0)))
while True:
    time.sleep(10)
`)
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	child.ExtraFiles = []*os.File{pw}
	child.Env = append(os.Environ(), "HERDR_PANE_ID=p1", "HOME="+dir)
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	pw.Close()
	defer func() {
		_ = child.Process.Kill()
		_ = child.Wait()
	}()
	buf := make([]byte, 32)
	n, err := pr.Read(buf)
	pr.Close()
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(buf[:n])))
	if err != nil {
		t.Fatal(err)
	}
	statBytes, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		t.Skip("no /proc")
	}
	rest := strings.Fields(string(statBytes)[strings.LastIndex(string(statBytes), ") ")+2:])
	start, err := strconv.ParseInt(rest[19], 10, 64)
	if err != nil {
		t.Fatal(err)
	}

	sessDir := filepath.Join(dir, ".claude", "sessions")
	if err := os.MkdirAll(sessDir, 0o755); err != nil {
		t.Fatal(err)
	}
	sessFile := filepath.Join(sessDir, fmt.Sprintf("%d.json", pid))
	setState := func(st string) {
		body := fmt.Sprintf(`{"sessionId":"%s","status":"%s"}`, sidA, st)
		if err := os.WriteFile(sessFile, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	setState("idle")

	screenFile := filepath.Join(dir, "screen")
	typedFile := filepath.Join(dir, "typed")
	herdrBin := filepath.Join(dir, "herdr")
	herdrScript := "#!/bin/sh\n" +
		`if [ "$1" = pane ] && [ "$2" = read ]; then cat ` + screenFile + `; exit 0; fi` + "\n" +
		`printf '%s\n' "$*" >> ` + typedFile + "\n"
	if err := os.WriteFile(herdrBin, []byte(herdrScript), 0o755); err != nil {
		t.Fatal(err)
	}

	run := func(gate string, keys []string) (int, string) {
		_ = os.Remove(typedFile)
		cmd := convoGatedCommand(gate, "herdr", "", "p1", "/run/x.sock", int64(pid), start, sidA, 0, keys, 0)
		c := exec.Command("sh", "-c", cmd)
		c.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"), "XDG_RUNTIME_DIR="+dir, "HOME="+dir)
		err := c.Run()
		code := 0
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			code = ee.ExitCode()
		} else if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		b, _ := os.ReadFile(typedFile)
		return code, strings.TrimSpace(string(b))
	}

	draftScreen := "● Done.\n──────────\n❯ 重新登录，我不要系统管理员，我要换一个用户\n──────────\n  ⏵⏵ auto mode on\n"
	digest := boxDigest(draftScreen)
	if digest == "" {
		t.Fatal("empty digest")
	}

	// 1. Matching digest + send (enter) -> succeeds and sends enter.
	_ = os.WriteFile(screenFile, []byte(draftScreen), 0o600)
	if code, typed := run("box:"+digest, []string{"enter"}); code != 0 || typed != "pane send-keys p1 enter" {
		t.Fatalf("send matching draft: exit=%d typed=%q", code, typed)
	}

	// 2. Matching digest + clear (ctrl+c) -> succeeds and sends ctrl+c.
	if code, typed := run("box:"+digest, []string{"ctrl+c"}); code != 0 || typed != "pane send-keys p1 ctrl+c" {
		t.Fatalf("clear matching draft: exit=%d typed=%q", code, typed)
	}

	// 3. Screen changed to different draft -> refuses (exit 10), presses nothing.
	otherScreen := "● Done.\n──────────\n❯ 另一句话\n──────────\n  ⏵⏵ auto mode on\n"
	_ = os.WriteFile(screenFile, []byte(otherScreen), 0o600)
	if code, typed := run("box:"+digest, []string{"ctrl+c"}); code != 10 || typed != "" {
		t.Fatalf("clear after screen changed: exit=%d typed=%q", code, typed)
	}

	// 4. Screen already emptied -> refuses (exit 10), never sends ctrl+c on empty box.
	emptyScreen := "● Done.\n──────────\n❯ \n──────────\n  ⏵⏵ auto mode on\n"
	_ = os.WriteFile(screenFile, []byte(emptyScreen), 0o600)
	if code, typed := run("box:"+digest, []string{"ctrl+c"}); code != 10 || typed != "" {
		t.Fatalf("clear on empty box: exit=%d typed=%q", code, typed)
	}

	// 5. Agent started waiting -> refuses (exit 9), presses nothing.
	_ = os.WriteFile(screenFile, []byte(draftScreen), 0o600)
	setState("waiting")
	if code, typed := run("box:"+digest, []string{"enter"}); code != 9 || typed != "" {
		t.Fatalf("send while waiting: exit=%d typed=%q", code, typed)
	}

	// 6. Terminal switched to subagent with same text in box -> refuses (exit 13), presses nothing.
	setState("idle")
	subScreen := "● Done.\n──────────\n❯ 重新登录，我不要系统管理员，我要换一个用户\n──────────\n  ◯ main\n  ● general-purpose  Done.\n"
	_ = os.WriteFile(screenFile, []byte(subScreen), 0o600)
	if code, typed := run("box:"+digest, []string{"ctrl+c"}); code != 13 || typed != "" {
		t.Fatalf("clear in subagent view: exit=%d typed=%q", code, typed)
	}
}
