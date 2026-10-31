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
	os.WriteFile(filepath.Join(dir, "herdr"), []byte("#!/bin/sh\nif [ \"$1 $2\" = 'pane read' ]; then printf 'Do you want to proceed?\\n 1. Yes\\n'; exit 0; fi\nif [ \"$1 $2\" = 'pane get' ]; then cat "+title+"; exit 0; fi\nprintf '%s\\n' \"$*\" >>"+typed+"\n"), 0o755)
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
	for _, st := range [][2]string{{"waiting", "permission prompt"}, {"waiting", "input needed"}, {"busy", ""}, {"idle", ""}} {
		state(st[0], st[1])
		if got := run("dialog", []string{"esc"}); got != 9 || wrote() != "" {
			t.Fatalf("%v, no dialog: exit %d, typed %q", st, got, wrote())
		}
	}
	state("idle", "")
	if got := run("reply:"+shown, []string{"1"}); got != 9 || wrote() != "" {
		t.Fatalf("no longer waiting: exit %d, typed %q", got, wrote())
	}
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
	codexUnreadCases(t, run, wrote, title, shown)
	// (Claude Code's file saying a status of its own that is no word of the
	// gate's, "unknown": a message goes, as before.)
	state("unknown", "")
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
