package web

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestReplyOK(t *testing.T) {
	d := strings.Repeat("a", 64)
	ok := []struct {
		keys []string
		text string
	}{{[]string{"1"}, ""}, {[]string{"down", "enter"}, ""}, {nil, "用 pnpm"}}
	for _, c := range ok {
		if !replyOK(d, c.keys, c.text) {
			t.Errorf("refused: %+v", c)
		}
	}
	bad := []struct {
		digest string
		keys   []string
		text   string
	}{
		{"", []string{"1"}, ""},                       // no screen named
		{strings.Repeat("A", 64), []string{"1"}, ""},  // not as the gateway writes it
		{d, []string{"1"}, "and text"},                // both
		{d, nil, ""},                                  // neither
		{d, []string{"C-c"}, ""},                      // not a key an answer presses
		{d, []string{"1", "1", "1", "1", "1"}, ""},    // too many
		{d, nil, "line\nbreak"},                       // a control character
		{d, nil, strings.Repeat("长", maxReplyText+1)}, // too long
	}
	for _, c := range bad {
		if replyOK(c.digest, c.keys, c.text) {
			t.Errorf("accepted: %+v", c)
		}
	}
}

func TestReplyPress(t *testing.T) {
	if got := strings.Join(replyPress("tmux", []string{"2", "esc", "down", "enter"}), " "); got != "2 Escape Down Enter" {
		t.Fatalf("tmux: %s", got)
	}
	if got := strings.Join(replyPress("herdr", []string{"2", "esc"}), " "); got != "2 esc" {
		t.Fatalf("herdr: %s", got)
	}
}

// The digest is of the screen as read, byte for byte: what sha256sum says
// of the same output on the host.
func TestScreenOfDigest(t *testing.T) {
	raw := []byte(" Do you want to proceed?\n ❯ 1. Yes\n\n\n")
	text, digest, cut := screenOf(raw)
	sum := sha256.Sum256(raw)
	if digest != hex.EncodeToString(sum[:]) || cut || text != " Do you want to proceed?\n ❯ 1. Yes" {
		t.Fatalf("%q %s %v", text, digest, cut)
	}
	if _, err := exec.LookPath("sha256sum"); err == nil {
		out, err := exec.Command("sh", "-c", `printf ' Do you want to proceed?\n ❯ 1. Yes\n\n\n' | sha256sum`).Output()
		if err != nil || strings.Fields(string(out))[0] != digest {
			t.Fatalf("sha256sum says %s", out)
		}
	}
}

// The gate is an argument of its own, quoted, before the keys.
func TestConvoGateArgument(t *testing.T) {
	cmd := convoGatedCommand("reply:"+strings.Repeat("b", 64), "tmux", "s", "%3", "", 7, 9, sidA, 0, []string{"1"}, 0)
	if !strings.HasSuffix(cmd, ` 0 'reply:`+strings.Repeat("b", 64)+`' '1'`) {
		t.Fatalf("%s", cmd[len(cmd)-100:])
	}
}

// The gate on the host (convoGatedCommand's script), run here: a stand-in
// agent in front of a terminal of its own (a pseudo-terminal), Claude Code's
// session file in a home of the test's, and a herdr that shows a screen and
// notes what it is asked to type.
func TestConvoGateOnTheHost(t *testing.T) {
	for _, p := range []string{"sh", "python3", "sha256sum"} {
		if _, err := exec.LookPath(p); err != nil {
			t.Skip("no " + p)
		}
	}
	if _, err := os.Stat("/proc/self/stat"); err != nil {
		t.Skip("no /proc")
	}
	dir := t.TempDir()
	typed := filepath.Join(dir, "typed")
	title := filepath.Join(dir, "title.json")
	// (Its screen: screen.txt; a text pasted makes it next.txt, where there is one.)
	screenFile, nextFile := filepath.Join(dir, "screen.txt"), filepath.Join(dir, "next.txt")
	asked := "Do you want to proceed?\n 1. Yes\n"
	os.WriteFile(filepath.Join(dir, "herdr"), []byte("#!/bin/sh\nif [ \"$1 $2\" = 'pane read' ]; then cat "+screenFile+"; [ -e "+screenFile+".fails ] && exit 1; exit 0; fi\nif [ \"$1 $2\" = 'pane get' ]; then cat "+title+"; exit 0; fi\nif [ \"$1 $2\" = 'pane send-text' ] && [ -f "+nextFile+" ]; then mv "+nextFile+" "+screenFile+"; fi\nprintf '%s\\n' \"$*\" >>"+typed+"\n"), 0o755)
	showing := func(s string) { os.WriteFile(screenFile, []byte(s), 0o644); os.Remove(nextFile) }
	showing(asked)
	// (The agent: sleep, a session's leader with the pseudo-terminal its own, in front of it.)
	helper := exec.Command("python3", "-c", `import pty, os, sys, time
pid, fd = pty.fork()
if pid == 0:
    os.environ["HERDR_PANE_ID"] = "p1"
    os.execvp("sleep", ["sleep", "30"])
print(pid, flush=True)
time.sleep(30)`)
	out, _ := helper.StdoutPipe()
	if err := helper.Start(); err != nil {
		t.Fatal(err)
	}
	defer helper.Process.Kill()
	var pid int
	if _, err := fmt.Fscan(out, &pid); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	stat, _ := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	rest := strings.Fields(string(stat[strings.LastIndexByte(string(stat), ')')+2:]))
	start, _ := strconv.ParseInt(rest[19], 10, 64)
	os.MkdirAll(filepath.Join(dir, ".claude", "sessions"), 0o755)
	state := func(status, why string) {
		os.WriteFile(filepath.Join(dir, ".claude", "sessions", strconv.Itoa(pid)+".json"), []byte(fmt.Sprintf(`{"pid":%d,"sessionId":%q,"status":%q,"waitingFor":%q}`, pid, sidA, status, why)), 0o644)
	}
	sum := sha256.Sum256([]byte("Do you want to proceed?\n 1. Yes\n"))
	shown := hex.EncodeToString(sum[:])
	run := func(gate string, keys []string) int {
		os.Remove(typed)
		cmd := convoGatedCommand(gate, "herdr", "", "p1", "/run/x.sock", int64(pid), start, sidA, 0, keys, 0)
		c := exec.Command("sh", "-c", cmd)
		c.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"), "XDG_RUNTIME_DIR="+dir, "HOME="+dir)
		err := c.Run()
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return ee.ExitCode()
		}
		return 0
	}
	wrote := func() string { b, _ := os.ReadFile(typed); return strings.TrimSpace(string(b)) }
	state("waiting", "permission prompt")
	if got := run("free", []string{"ctrl+x", "ctrl+s"}); got != 9 || wrote() != "" {
		t.Fatalf("keys while it waits: exit %d, typed %q", got, wrote())
	}
	if got := run("reply:"+strings.Repeat("0", 64), []string{"1"}); got != 10 || wrote() != "" {
		t.Fatalf("another screen: exit %d, typed %q", got, wrote())
	}
	if got := run("reply:"+shown, []string{"1"}); got != 0 || wrote() != "pane send-keys p1 1" {
		t.Fatalf("the screen answered: exit %d, typed %q", got, wrote())
	}
	state("waiting", "dialog open")
	if got := run("reply:"+shown, []string{"1"}); got != 9 || wrote() != "" {
		t.Fatalf("a dialog of its own is no question: exit %d, typed %q", got, wrote())
	}
	// Escape for a dialog of its own: only while one is open — not for a
	// question, nor once it was closed (a turn begun since would stop).
	for _, why := range []string{"dialog open", "other"} {
		state("waiting", why)
		if got := run("dialog", []string{"esc"}); got != 0 || wrote() != "pane send-keys p1 esc" {
			t.Fatalf("%s, Escape closes it: exit %d, typed %q", why, got, wrote())
		}
	}
	// A panel of its own: its input box back on the screen, the session file
	// not yet saying so: no key goes past it. Nor on a screen not read, nor one
	// showing a subagent's box. A key that settles something: on the screen shown.
	state("waiting", "dialog open")
	for _, g := range []string{"dialog", "panel"} {
		showing("──────────────────────────────\n❯ \n──────────────────────────────\n  ⏵⏵ auto mode on\n")
		if got := run(g, []string{"esc"}); got != 9 || wrote() != "" {
			t.Fatalf("%s, its box back: exit %d, typed %q", g, got, wrote())
		}
		os.Remove(screenFile)
		if got := run(g, []string{"esc"}); got != 9 || wrote() != "" {
			t.Fatalf("%s, no screen read: exit %d, typed %q", g, got, wrote())
		}
		showing(asked)
		if got := run(g, []string{"esc"}); got != 0 || wrote() != "pane send-keys p1 esc" {
			t.Fatalf("%s, open: exit %d, typed %q", g, got, wrote())
		}
	}
	onShow := panelDigest([]byte("\x1b[7m" + asked))
	if got := run("panel:"+onShow, []string{"enter"}); got != 0 || wrote() != "pane send-keys p1 enter" {
		t.Fatalf("on the screen shown: exit %d, typed %q", got, wrote())
	}
	if got := run("panel:"+strings.Repeat("a", 64), []string{"enter"}); got != 10 || wrote() != "" {
		t.Fatalf("on another: exit %d, typed %q", got, wrote())
	}
	// The terminal shown as it is: a key that settles goes on the screen shown, in
	// any state, nor where the state cannot be read; one that does not, as in the terminal.
	onLook := lookDigest([]byte("\x1b[7m" + asked))
	for _, st := range [][2]string{{"waiting", "dialog open"}, {"busy", ""}, {"idle", ""}, {"", ""}} {
		state(st[0], st[1])
		if got := run("look:"+onLook, []string{"enter"}); got != 0 || wrote() != "pane send-keys p1 enter" {
			t.Fatalf("look, %q, the screen shown: exit %d, typed %q", st[0], got, wrote())
		}
		if got := run("look:"+strings.Repeat("a", 64), []string{"enter"}); got != 10 || wrote() != "" {
			t.Fatalf("look, %q, another screen: exit %d, typed %q", st[0], got, wrote())
		}
		if got := run("look", []string{"esc"}); got != 0 || wrote() != "pane send-keys p1 esc" {
			t.Fatalf("look, %q, a key that settles nothing: exit %d, typed %q", st[0], got, wrote())
		}
	}
	// (With its colours and what moves, as a real screen is: read again a moment on.)
	moving := func(sec string) string {
		return "\x1b[38;5;246m●\x1b[39m Needs permission\n\x1b[1m✽ Nucleating… (" + sec + "s)\x1b[0m\n  ◯ main\n❯ ● general-purpose  Count the files      " + sec + "s\n" + asked
	}
	showing(moving("9"))
	seen := lookDigest([]byte(moving("10")))
	if got := run("look:"+seen, []string{"enter"}); got != 0 || wrote() != "pane send-keys p1 enter" {
		t.Fatalf("look, only what moves has moved: exit %d, typed %q", got, wrote())
	}
	showing("\x1b[1mDelete production?\x1b[0m\n" + asked)
	if got := run("look:"+seen, []string{"enter"}); got != 10 || wrote() != "" {
		t.Fatalf("look, another question: exit %d, typed %q", got, wrote())
	}
	showing(asked)
	os.WriteFile(screenFile+".fails", nil, 0o644)
	if got := run("look:"+onLook, []string{"enter"}); got != 10 || wrote() != "" {
		t.Fatalf("look, the read failed, its output as shown: exit %d, typed %q", got, wrote())
	}
	os.Remove(screenFile + ".fails")
	os.Remove(screenFile)
	if got := run("look:"+onLook, []string{"enter"}); got != 10 || wrote() != "" {
		t.Fatalf("look, no screen read: exit %d, typed %q", got, wrote())
	}
	showing(asked)
	state("waiting", "dialog open")
	for _, st := range [][2]string{{"waiting", "permission prompt"}, {"waiting", "input needed"}, {"busy", ""}, {"idle", ""}} {
		state(st[0], st[1])
		if got := run("dialog", []string{"esc"}); got != 9 || wrote() != "" {
			t.Fatalf("%v, no dialog: exit %d, typed %q", st, got, wrote())
		}
		if got := run("panel", []string{"esc"}); got != 9 || wrote() != "" {
			t.Fatalf("%v, no panel: exit %d, typed %q", st, got, wrote())
		}
	}
	state("idle", "")
	if got := run("reply:"+shown, []string{"1"}); got != 9 || wrote() != "" {
		t.Fatalf("no longer waiting: exit %d, typed %q", got, wrote())
	}
	// (Claude Code's own: keys and text only where its input box is the conversation's.)
	showing(asked)
	if got := run("free", []string{"ctrl+x"}); got != 15 || wrote() != "" {
		t.Fatalf("no input box on the screen: exit %d, typed %q", got, wrote())
	}
	ownerGateCases(t, dir, pid, start, run, wrote, showing, nextFile)
	showing(ownerMain)
	if got := run("free", []string{"ctrl+x", "ctrl+s"}); got != 0 || wrote() != "pane send-keys p1 ctrl+x\npane send-keys p1 ctrl+s" {
		t.Fatalf("not waiting, keys go: exit %d, typed %q", got, wrote())
	}
	// A command on the session: only while it is idle.
	if got := run("idle", []string{"enter"}); got != 0 || wrote() != "pane send-keys p1 enter" {
		t.Fatalf("idle, a command goes: exit %d, typed %q", got, wrote())
	}
	for _, st := range []string{"busy", "waiting", "shell"} {
		state(st, "")
		if got := run("idle", []string{"enter"}); got != 12 || wrote() != "" {
			t.Fatalf("%s, a command waits: exit %d, typed %q", st, got, wrote())
		}
	}
	// A Codex's gates (5d-2): its terminal's title says whether it waits, not
	// a session file of Claude Code's (here saying the other thing each time).
	// (The stand-in is not named codex: its identity is checked as Claude Code's.)
	state("idle", "")
	showing(asked) // (Codex's gates read no box: answered on the screen of a question)
	titled := func(ti string) {
		os.WriteFile(title, []byte(fmt.Sprintf(`{"result":{"pane":{"pane_id":"p1","terminal_title":%q}}}`, ti)), 0o644)
	}
	for _, ti := range []string{"[ ! ] Action Required | proj", "[ . ] Action Required | proj"} {
		titled(ti)
		if got := run("cfree", []string{"ctrl+x"}); got != 9 || wrote() != "" {
			t.Fatalf("%q: a message while it waits: exit %d, typed %q", ti, got, wrote())
		}
		if got := run("creply:"+strings.Repeat("0", 64), []string{"1"}); got != 10 || wrote() != "" {
			t.Fatalf("%q: another screen: exit %d, typed %q", ti, got, wrote())
		}
		if got := run("creply:"+shown, []string{"1"}); got != 0 || wrote() != "pane send-keys p1 1" {
			t.Fatalf("%q: the screen answered: exit %d, typed %q", ti, got, wrote())
		}
	}
	state("waiting", "permission prompt")
	for _, ti := range []string{"proj", "Action Required", "x [ ! ] Action Required"} {
		titled(ti)
		if got := run("creply:"+shown, []string{"1"}); got != 9 || wrote() != "" {
			t.Fatalf("%q: not waiting, no answer: exit %d, typed %q", ti, got, wrote())
		}
		if got := run("cfree", []string{"ctrl+x"}); got != 0 || wrote() != "pane send-keys p1 ctrl+x" {
			t.Fatalf("%q: not waiting, a message goes: exit %d, typed %q", ti, got, wrote())
		}
	}
	// A Codex's command (cscreen:): on the screen read, not while it waits.
	titled("proj")
	if got := run("cscreen:"+shown, []string{"ctrl+x"}); got != 0 || wrote() != "pane send-keys p1 ctrl+x" {
		t.Fatalf("command, the screen read, not waiting: exit %d, typed %q", got, wrote())
	}
	if got := run("cscreen:"+strings.Repeat("0", 64), []string{"ctrl+x"}); got != 10 || wrote() != "" {
		t.Fatalf("command, another screen: exit %d, typed %q", got, wrote())
	}
	titled("[ ! ] Action Required | proj")
	if got := run("cscreen:"+shown, []string{"ctrl+x"}); got != 9 || wrote() != "" {
		t.Fatalf("command while it waits: exit %d, typed %q", got, wrote())
	}
	codexUnreadCases(t, run, wrote, title, shown)
	// (Claude Code's file saying a status of its own that is no word of the
	// gate's, "unknown": a message goes, as before.)
	state("unknown", "")
	showing(ownerMain)
	if got := run("free", []string{"ctrl+x"}); got != 0 || wrote() != "pane send-keys p1 ctrl+x" {
		t.Fatalf("Claude Code, status unknown: exit %d, typed %q", got, wrote())
	}
}

// (Its title not read — herdr failing, or its answer without the title: no
// message and no answer go.)
func codexUnreadCases(t *testing.T, run func(string, []string) int, wrote func() string, title, shown string) {
	for _, how := range []string{"gone", `{"result":{"pane":{"pane_id":"p1"}}}`} {
		if how == "gone" {
			os.Remove(title)
		} else {
			os.WriteFile(title, []byte(how), 0o644)
		}
		if got := run("cfree", []string{"ctrl+x"}); got != 11 || wrote() != "" {
			t.Fatalf("%s: a message, its title not read: exit %d, typed %q", how, got, wrote())
		}
		if got := run("creply:"+shown, []string{"1"}); got != 9 || wrote() != "" {
			t.Fatalf("%s: an answer, its title not read: exit %d, typed %q", how, got, wrote())
		}
	}
}

// The same titles as the host's gate takes for a Codex that waits.
func TestCodexTitleWaits(t *testing.T) {
	for title, want := range map[string]bool{
		"[ ! ] Action Required | proj": true, "[ . ] Action Required | proj": true, "[ ! ] Action Required": true,
		"proj": false, "": false, "Action Required": false, "x [ ! ] Action Required": false, "[ ? ] Action Required": false,
	} {
		if got := codexTitleWaits(title); got != want {
			t.Errorf("%q: %v", title, got)
		}
	}
}

func TestCommandText(t *testing.T) {
	for in, ok := range map[string]bool{
		"/compact": true, "/compact keep the plan": true, "/clear": true, "/rename 部署排查": true,
		"/rename ": false, "/rename": false, "/compact\n/clear": false, "/model opus": false, "hello": false, "/clear now": false,
	} {
		if _, got := commandText(in); got != ok {
			t.Errorf("%q: %v", in, got)
		}
	}
}

// The screen shown for an answer has the lines the terminal wrapped joined;
// its digest is still that of the screen as shown, which the host checks
// before the answer's keys (10-31; review496). A herdr with no such source:
// the screen as shown.
func TestScreenJoinedForAnswer(t *testing.T) {
	for _, p := range []string{"sh", "python3", "sha256sum"} {
		if _, err := exec.LookPath(p); err != nil {
			t.Skip("no " + p)
		}
	}
	if _, err := os.Stat("/proc/self/stat"); err != nil {
		t.Skip("no /proc")
	}
	dir := t.TempDir()
	typed := filepath.Join(dir, "typed")
	old := filepath.Join(dir, "old") // (there: a herdr with no such source)
	const wrapped = " Do you want to proceed?\n ❯ 1. Yes\n   2. Yes, and switch to auto mode · auto mode handles these prompts f\nor you\n   3. No\n"
	// (Joined, herdr's goes back above the screen: an old prompt there.)
	const joined = " Old question?\n ❯ 1. Something\n Do you want to proceed?\n ❯ 1. Yes\n   2. Yes, and switch to auto mode · auto mode handles these prompts for you\n   3. No\n"
	os.WriteFile(filepath.Join(dir, "herdr"), []byte("#!/bin/sh\nif [ \"$1 $2\" = 'pane read' ]; then\n  if [ \"$5\" = recent-unwrapped ]; then [ -e "+old+" ] && exit 2; printf '%s' '"+joined+"'; exit 0; fi\n  printf '%s' '"+wrapped+"'; exit 0\nfi\nprintf '%s\\n' \"$*\" >>"+typed+"\n"), 0o755)
	helper := exec.Command("python3", "-c", `import pty, os, sys, time
pid, fd = pty.fork()
if pid == 0:
    os.environ["HERDR_PANE_ID"] = "p1"
    os.execvp("sleep", ["sleep", "30"])
print(pid, flush=True)
time.sleep(30)`)
	out, _ := helper.StdoutPipe()
	if err := helper.Start(); err != nil {
		t.Fatal(err)
	}
	defer helper.Process.Kill()
	var pid int
	if _, err := fmt.Fscan(out, &pid); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	stat, _ := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	rest := strings.Fields(string(stat[strings.LastIndexByte(string(stat), ')')+2:]))
	start, _ := strconv.ParseInt(rest[19], 10, 64)
	os.MkdirAll(filepath.Join(dir, ".claude", "sessions"), 0o755)
	os.WriteFile(filepath.Join(dir, ".claude", "sessions", strconv.Itoa(pid)+".json"), []byte(fmt.Sprintf(`{"pid":%d,"sessionId":%q,"status":"waiting","waitingFor":"permission prompt"}`, pid, sidA)), 0o644)
	env := append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"), "XDG_RUNTIME_DIR="+dir, "HOME="+dir)
	shownFor := func() (text, digest string) {
		c := exec.Command("sh", "-c", convoScreenJoinedCommand("herdr", "p1", "/run/x.sock"))
		c.Env = env
		raw, err := c.Output()
		if err != nil {
			t.Fatal(err)
		}
		shown, j, ok := screenJoined(raw)
		if !ok {
			t.Fatalf("not framed: %q", raw)
		}
		_, digest, _ = screenOf(shown)
		text, _, _ = screenOf([]byte(j))
		return text, digest
	}
	answer := func(digest string) (int, string) {
		os.Remove(typed)
		c := exec.Command("sh", "-c", convoGatedCommand("reply:"+digest, "herdr", "", "p1", "/run/x.sock", int64(pid), start, sidA, 0, []string{"1"}, 0))
		c.Env = env
		err := c.Run()
		b, _ := os.ReadFile(typed)
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return ee.ExitCode(), strings.TrimSpace(string(b))
		}
		return 0, strings.TrimSpace(string(b))
	}
	text, digest := shownFor()
	if !strings.Contains(text, "prompts for you") || strings.Contains(text, "\nor you") || strings.Contains(text, "Old question") {
		t.Fatalf("not the screen, joined: %q", text)
	}
	vis := sha256.Sum256([]byte(wrapped))
	if digest != hex.EncodeToString(vis[:]) {
		t.Fatal("its digest not that of the screen as shown")
	}
	if got, keys := answer(digest); got != 0 || keys != "pane send-keys p1 1" {
		t.Fatalf("its screen as shown: exit %d, typed %q", got, keys)
	}
	os.WriteFile(old, nil, 0o644)
	if text, digest = shownFor(); !strings.Contains(text, "\nor you") {
		t.Fatalf("an older herdr: %q", text)
	}
	if got, keys := answer(digest); got != 0 || keys != "pane send-keys p1 1" {
		t.Fatalf("an older herdr, its screen as shown: exit %d, typed %q", got, keys)
	}
}

// The screen as shown is framed by its length: no letters of its own (or
// of the joined read) move where its digest's bytes end; one that cannot be
// read fails the command, as it did (review497).
func TestScreenJoinedFraming(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh here")
	}
	dir := t.TempDir()
	fail := filepath.Join(dir, "fail")
	shownText := "a line\n\x01corral-joined\nnot a mark\n"
	joinedText := "earlier\n\x01corral-joined\nx\na line\n\x01corral-joined\nnot a mark\n"
	os.WriteFile(filepath.Join(dir, "herdr"), []byte("#!/bin/sh\n[ -e "+fail+" ] && exit 3\nif [ \"$5\" = recent-unwrapped ]; then printf '%b' '"+strings.ReplaceAll(joinedText, "\n", "\\n")+"'; exit 0; fi\nprintf '%b' '"+strings.ReplaceAll(shownText, "\n", "\\n")+"'\n"), 0o755)
	run := func() ([]byte, error) {
		c := exec.Command("sh", "-c", convoScreenJoinedCommand("herdr", "p1", "/run/x.sock"))
		c.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"), "HOME="+dir) // (the script looks in $HOME's bin first)
		return c.Output()
	}
	raw, err := run()
	if err != nil {
		t.Fatal(err)
	}
	shown, _, ok := screenJoined(raw)
	if !ok || string(shown) != shownText {
		t.Fatalf("framed: %v %q", ok, shown)
	}
	os.WriteFile(fail, nil, 0o644)
	if out, err := run(); err == nil {
		t.Fatalf("a screen not read: %q, no failure", out)
	}
	if _, _, ok := screenJoined([]byte("12\nshort")); ok {
		t.Fatal("a length past the output taken")
	}
	if _, _, ok := screenJoined([]byte("9223372036854775807\nx")); ok {
		t.Fatal("a length too great to add taken")
	}
}

// Ended while it reads (the request given up: SIGTERM), the screen read
// leaves no file of the screen behind (review499).
func TestScreenJoinedEndedLeavesNothing(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh here")
	}
	dir := t.TempDir()
	tmp := filepath.Join(dir, "tmp")
	os.Mkdir(tmp, 0o700)
	// (The joined read hangs: the command is ended there.)
	os.WriteFile(filepath.Join(dir, "herdr"), []byte("#!/bin/sh\nif [ \"$5\" = recent-unwrapped ]; then exec sleep 20; fi\nprintf 'a screen\\n'\n"), 0o755)
	c := exec.Command("sh", "-c", convoScreenJoinedCommand("herdr", "p1", "/run/x.sock"))
	c.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"), "HOME="+dir, "TMPDIR="+tmp)
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} // (its group ended, as the SSH session's end ends it)
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if es, _ := os.ReadDir(tmp); len(es) == 2 || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if es, _ := os.ReadDir(tmp); len(es) != 2 {
		t.Fatalf("its files not made: %d", len(es))
	}
	syscall.Kill(-c.Process.Pid, syscall.SIGTERM)
	c.Wait()
	for time.Now().Before(deadline.Add(5 * time.Second)) {
		if es, _ := os.ReadDir(tmp); len(es) == 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if es, _ := os.ReadDir(tmp); len(es) != 0 {
		t.Fatalf("left behind: %d files", len(es))
	}
}

const ownerMain = "some answer\n──────────\n❯ \n──────────\n  ⏵⏵ auto mode on\n"

// Who has Claude Code's input box, on the host (convo_view.go): keys and a
// text go only to the conversation's own; where the box goes to another
// between the paste and Enter, Enter is held back.
func ownerGateCases(t *testing.T, dir string, pid int, start int64, run func(string, []string) int, wrote func() string, showing func(string), nextFile string) {
	t.Helper()
	sub := "──────────\n❯ Message @general-purpose…\n──────────\n  ◯ main\n  ● general-purpose  x\n"
	list := "──────────\n❯ \n──────────\n  ↑/↓ to select\n\n❯ ● main\n  ◯ Explore  x\n"
	sessions := "──────────\n❯ describe a task for a new session\n──────────\n  enter to return · space to reply\n"
	for _, c := range []struct {
		name, screen string
		gate         string
		want         int
		typed        string
	}{
		{"a subagent's", sub, "free", 13, ""}, {"the sessions' view", sessions, "free", 14, ""},
		{"a subagent's, a command", sub, "idle", 13, ""},
		// (The list holding the keys: refused, no key pressed: the person's to let go of.)
		{"the list of agents", list, "free", 14, ""},
		{"the list of agents, Enter again", list, "enter", 14, ""},
		// (Stop on a subagent's placeholder alone, its list out of sight: not pressed — review560.)
		{"a subagent's placeholder alone, stop", "──────────\n❯ Message @general-purpose…\n──────────\n  ⏵⏵ auto mode on\n", "stop", 13, ""},
	} {
		showing(c.screen)
		if got := run(c.gate, []string{"enter"}); got != c.want || wrote() != c.typed {
			t.Fatalf("%s: exit %d, typed %q", c.name, got, wrote())
		}
	}
	// (Enter for words already in the box, the message's: only while the box
	// still holds them as read — review571.)
	same := "──────────\n❯ the same words\n──────────\n  ⏵⏵ auto mode on\n"
	showing(same)
	if got := run("box:"+boxDigest(same), []string{"enter"}); got != 0 || wrote() != "pane send-keys p1 enter" {
		t.Fatalf("Enter for the same words in the box: exit %d, typed %q", got, wrote())
	}
	if got := run("box:"+boxDigest("──────────\n❯ other words\n──────────\n"), []string{"enter"}); got != 10 || wrote() != "" {
		t.Fatalf("the box changed since it was read: exit %d, typed %q", got, wrote())
	}
	// (Keys go after a text of ours: what the box holds is not read.)
	showing("──────────\n❯ Message @team, the report\n──────────\n  ⏵⏵ auto mode on\n")
	if got := run("enter", []string{"enter"}); got != 0 || wrote() != "pane send-keys p1 enter" {
		t.Fatalf("Enter again after a text of ours: exit %d, typed %q", got, wrote())
	}
	text := func(gate, msg string) int {
		os.Remove(filepath.Join(dir, "typed"))
		cmd := convoGatedCommand(gate, "herdr", "", "p1", "/run/x.sock", int64(pid), start, sidA, len(msg), nil, 0)
		c := exec.Command("sh", "-c", cmd)
		c.Stdin = strings.NewReader(msg)
		c.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"), "XDG_RUNTIME_DIR="+dir, "HOME="+dir)
		err := c.Run()
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return ee.ExitCode()
		}
		return 0
	}
	showing(ownerMain)
	if got := text("free", "hello"); got != 0 || !strings.HasSuffix(wrote(), "pane send-keys p1 enter") {
		t.Fatalf("a text to the conversation: exit %d, typed %q", got, wrote())
	}
	showing(sub)
	if got := text("free", "hello"); got != 13 || wrote() != "" {
		t.Fatalf("a text to a subagent's: exit %d, typed %q", got, wrote())
	}
	// (Gone to a subagent's once pasted: typed, Enter held back.)
	showing(ownerMain)
	os.WriteFile(nextFile, []byte("──────────\n❯ hello\n──────────\n  ◯ main\n  ● general-purpose  x\n"), 0o644)
	if got := text("free", "hello"); got != 16 || strings.Contains(wrote(), "send-keys") || !strings.Contains(wrote(), "send-text") {
		t.Fatalf("the box went to a subagent before Enter: exit %d, typed %q", got, wrote())
	}
	// (A dialog come over it once pasted, its keys' hint wrapped: Enter held back — review551-1.)
	showing(ownerMain)
	os.WriteFile(nextFile, []byte("──────────\n❯ hello\n──────────\n  Esc to\n  cancel\n"), 0o644)
	if got := text("free", "hello"); got != 16 || strings.Contains(wrote(), "send-keys") {
		t.Fatalf("a dialog's wrapped hint before Enter: exit %d, typed %q", got, wrote())
	}
	// (A message of ours beginning as the placeholder does: once pasted, not read.)
	showing(ownerMain)
	os.WriteFile(nextFile, []byte("──────────\n❯ Message @team, the report\n──────────\n  ⏵⏵ auto mode on\n"), 0o644)
	if got := text("free", "Message @team, the report"); got != 0 {
		t.Fatalf("a message beginning Message @: exit %d, typed %q", got, wrote())
	}
}
