package web

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ihainan/corral/internal/sshx"
)

// What may be typed into a pane: text, its line ends made "\n"; never a
// control character that would be pressed rather than typed.
func TestSendText(t *testing.T) {
	ok := map[string]string{
		"hi":                          "hi",
		"a\r\nb\rc\nd":                "a\nb\nc\nd",
		"tab\there":                   "tab\there",
		"中文，带标点！":                     "中文，带标点！",
		"  spaced  ":                  "  spaced  ",
		string(make([]byte, 0)) + "x": "x",
	}
	for in, want := range ok {
		if got, good := sendText(in); !good || got != want {
			t.Errorf("sendText(%q) = %q, %v; want %q", in, got, good, want)
		}
	}
	for _, bad := range []string{"", "   \n ", "esc\x1b[201~", "ctrl-b \x02", "del\x7f", "c1\u009b", "nul\x00", "ls ", "\xff\xfe", string(make([]byte, maxSendText+1))} {
		if _, good := sendText(bad); good {
			t.Errorf("sendText(%q) accepted", bad)
		}
	}
}

// convoSendCommand types nothing unless the process is still the one in
// front in that pane, and the text came whole: run here with a stand-in
// herdr that notes what it was asked to type.
func TestConvoSendCommandChecksFirst(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	if _, err := os.Stat("/proc/self/stat"); err != nil {
		t.Skip("no /proc")
	}
	dir := t.TempDir()
	typed := filepath.Join(dir, "typed")
	os.WriteFile(filepath.Join(dir, "herdr"), []byte("#!/bin/sh\nprintf '%s\\n' \"$*\" >>"+typed+"\n"), 0o755)
	// The agent: a process whose pane is p1 (not in front of any terminal: a test has none).
	agent := exec.Command("sleep", "30")
	agent.Env = append(os.Environ(), "HERDR_PANE_ID=p1")
	if err := agent.Start(); err != nil {
		t.Fatal(err)
	}
	defer agent.Process.Kill()
	stat, _ := os.ReadFile(fmt.Sprintf("/proc/%d/stat", agent.Process.Pid))
	rest := strings.Fields(string(stat[strings.LastIndexByte(string(stat), ')')+2:]))
	start, _ := strconv.ParseInt(rest[19], 10, 64)
	run := func(text string, length int, pid, st int64) int {
		cmd := convoSendCommand("herdr", "", "p1", "/run/x.sock", pid, st, sidA, length, nil, 0)
		c := exec.Command("sh", "-c", cmd) // (as the host runs it)
		c.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"), "XDG_RUNTIME_DIR="+dir)
		c.Stdin = strings.NewReader(text)
		err := c.Run()
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return ee.ExitCode()
		}
		return 0
	}
	if got := run("hi", 2, int64(agent.Process.Pid), start); got != 3 {
		t.Errorf("not in front: exit %d, want 3", got)
	}
	if got := run("hi", 2, int64(agent.Process.Pid), start+1); got != 3 {
		t.Errorf("another start: exit %d, want 3", got)
	}
	if got := run("h", 2, int64(agent.Process.Pid), start); got != 4 {
		t.Errorf("cut short: exit %d, want 4", got)
	}
	if _, err := os.Stat(typed); err == nil {
		t.Error("something was typed")
	}
}

// One message at a time to a pane; a pane given back by all is forgotten,
// one waited for in vain too.
func TestPaneTurns(t *testing.T) {
	p := paneTurns{m: map[string]*paneTurn{}}
	r1, ok := p.take(context.Background(), "a")
	if !ok {
		t.Fatal("first: no turn")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, ok := p.take(ctx, "a"); ok {
		t.Fatal("second while the first has it: a turn")
	}
	got := make(chan bool)
	go func() {
		_, ok := p.take(context.Background(), "a")
		got <- ok
	}()
	time.Sleep(20 * time.Millisecond)
	r1()
	r1() // (once only)
	if !<-got {
		t.Fatal("after the first: no turn")
	}
	p.mu.Lock()
	n := p.m["a"].n
	p.mu.Unlock()
	if n != 1 {
		t.Fatalf("holders %d, want 1", n)
	}
}

// A message in parts (convoGatedPasteCommand): its parts' lengths checked
// before anything — none empty, none too long, their sum the message's —
// and each cut out whole into a folder of its own, gone after (review505).
// (Not in front here: a valid one gets as far as that check, exit 3.)
func TestConvoPasteParts(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	if _, err := os.Stat("/proc/self/stat"); err != nil {
		t.Skip("no /proc")
	}
	dir, tmp := t.TempDir(), t.TempDir()
	typed := filepath.Join(dir, "typed")
	os.WriteFile(filepath.Join(dir, "herdr"), []byte("#!/bin/sh\nprintf '%s\\n' \"$*\" >>"+typed+"\n"), 0o755)
	agent := exec.Command("sleep", "30")
	agent.Env = append(os.Environ(), "HERDR_PANE_ID=p1")
	if err := agent.Start(); err != nil {
		t.Fatal(err)
	}
	defer agent.Process.Kill()
	stat, _ := os.ReadFile(fmt.Sprintf("/proc/%d/stat", agent.Process.Pid))
	rest := strings.Fields(string(stat[strings.LastIndexByte(string(stat), ')')+2:]))
	start, _ := strconv.ParseInt(rest[19], 10, 64)
	run := func(text, lens string) int {
		cmd := convoGatedCommand("", "herdr", "", "p1", "/run/x.sock", int64(agent.Process.Pid), start, sidA, 0, nil, 0)
		cmd = strings.Replace(cmd, " 0 '/run/x.sock'", " "+lens+" '/run/x.sock'", 1) // (the length as convoGatedPasteCommand gives it, or not)
		c := exec.Command("sh", "-c", cmd)
		c.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"), "XDG_RUNTIME_DIR="+dir, "TMPDIR="+tmp)
		c.Stdin = strings.NewReader(text)
		err := c.Run()
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return ee.ExitCode()
		}
		return 0
	}
	msg := "看看 /tmp/a.png 说说"
	n1, n2 := len("看看 "), len("/tmp/a.png")
	ok := fmt.Sprintf("%d+%d+%d", n1, n2, len(msg)-n1-n2)
	if !strings.Contains(convoGatedPasteCommand("", "herdr", "", "p1", "/run/x.sock", 1, 1, sidA, []int{n1, n2, len(msg) - n1 - n2}, 0), " "+ok+" ") {
		t.Fatalf("its lengths not given as %s", ok)
	}
	for _, c := range []struct {
		lens string
		want int
	}{
		{ok, 3}, // (cut out whole: on to the check of the process in front)
		{fmt.Sprintf("%d+0+%d", n1, len(msg)-n1), 4},
		{fmt.Sprintf("%d+%d", n1, len(msg)), 4},          // (its sum not the message's)
		{"9223372036854775807+9223372036854775807+7", 4}, // (too long: no sum run over)
		{"+3", 4},
		{"3+x", 4},
	} {
		if got := run(msg, c.lens); got != c.want {
			t.Errorf("%s: exit %d, want %d", c.lens, got, c.want)
		}
	}
	if _, err := os.Stat(typed); err == nil {
		t.Error("something was typed")
	}
	if left, _ := os.ReadDir(tmp); len(left) != 0 {
		t.Errorf("left behind: %v", left)
	}
}

// A reply's text only pasted (Fill): its length "N-", nothing else, checked
// before anything as a send's; then it pastes and does not press Enter
// (not in front here: a valid one gets as far as that check, exit 3).
func TestConvoFillLength(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	if _, err := os.Stat("/proc/self/stat"); err != nil {
		t.Skip("no /proc")
	}
	dir := t.TempDir()
	typed := filepath.Join(dir, "typed")
	os.WriteFile(filepath.Join(dir, "herdr"), []byte("#!/bin/sh\nprintf '%s\\n' \"$*\" >>"+typed+"\n"), 0o755)
	agent := exec.Command("sleep", "30")
	agent.Env = append(os.Environ(), "HERDR_PANE_ID=p1")
	if err := agent.Start(); err != nil {
		t.Fatal(err)
	}
	defer agent.Process.Kill()
	stat, _ := os.ReadFile(fmt.Sprintf("/proc/%d/stat", agent.Process.Pid))
	rest := strings.Fields(string(stat[strings.LastIndexByte(string(stat), ')')+2:]))
	start, _ := strconv.ParseInt(rest[19], 10, 64)
	cmd := convoGatedFillCommand("", "herdr", "", "p1", "/run/x.sock", int64(agent.Process.Pid), start, sidA, 5, 0)
	if !strings.Contains(cmd, " 5- '/run/x.sock'") {
		t.Fatalf("its length not given as 5-: %s", cmd[len(cmd)-80:])
	}
	run := func(text, n string) int {
		c := exec.Command("sh", "-c", strings.Replace(cmd, " 5- '/run/x.sock'", " "+n+" '/run/x.sock'", 1))
		c.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"), "XDG_RUNTIME_DIR="+dir)
		c.Stdin = strings.NewReader(text)
		err := c.Run()
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return ee.ExitCode()
		}
		return 0
	}
	for _, c := range []struct {
		text, n string
		want    int
	}{{"hello", "5-", 3}, {"hello", "4-", 4}, {"hello", "-", 4}, {"hello", "5x-", 4}, {"hello", "5--", 4}} {
		if got := run(c.text, c.n); got != c.want {
			t.Errorf("%q %s: exit %d, want %d", c.text, c.n, got, c.want)
		}
	}
	if _, err := os.Stat(typed); err == nil {
		t.Error("something was typed")
	}
}

// fillOK: one line, not blank, no control nor separator, at most 1000 characters.
func TestFillOK(t *testing.T) {
	d := strings.Repeat("a", 64)
	for text, want := range map[string]bool{
		"用 MongoDB 吧":             true,
		" leading space":          true,
		"-x flag":                 true,
		"":                        false,
		"   ":                     false,
		"two\nlines":              false,
		"a\tb":                    false,
		"a b":                     false,
		"a b":                     false,
		strings.Repeat("字", 1000): true,
		strings.Repeat("字", 1001): false,
	} {
		if got := fillOK(d, text); got != want {
			t.Errorf("%q: %v", text[:min(len(text), 12)], got)
		}
	}
	if fillOK("nothex", "ok") {
		t.Error("no digest: ok")
	}
}

// modeLines must not ask convoGated to sleep and re-read the screen after a
// send: convoPress ignores res.Out for "send", and a failing post-send screen
// read used to exit 8 and surface as 502 "unknown" even after Enter succeeded.
func TestModeLinesDoesNotReadAfterSend(t *testing.T) {
	if got := modeLines("send"); got != 0 {
		t.Fatalf("modeLines(send) = %d, want 0", got)
	}
	if got := modeLines("mode"); got <= 0 {
		t.Fatalf("modeLines(mode) = %d, want > 0", got)
	}
}

// When herdr pane read fails before typing, owned() must exit exitOwnerUnread
// (19 → 502 no_screen) rather than exitOwnerNone (15 → 409 no_input), and
// callers must treat it as nothing typed / gate refused.
func TestConvoGatedOwnerUnreadExits19(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	cmd := convoGatedCommand("free", "herdr", "", "p1", "/run/x.sock", 1, 1, sidA, 2, nil, 0)
	// Extract the shell body of convoGated and test owner()/owned() directly with
	// a stub herdr whose `pane read` fails.
	bodyStart := strings.Index(cmd, "sh -c ") + len("sh -c ")
	bodyEnd := strings.Index(cmd, " corral ")
	quotedScript := cmd[bodyStart:bodyEnd]
	dir := t.TempDir()
	scriptPath := filepath.Join(dir, "gated.sh")
	if out, err := exec.Command("sh", "-c", "printf %s "+quotedScript+" > "+scriptPath).CombinedOutput(); err != nil {
		t.Fatalf("extract script: %v %s", err, out)
	}
	raw, err := os.ReadFile(scriptPath)
	if err != nil {
		t.Fatal(err)
	}
	// Keep only owner() and owned() so we can invoke owned 0 0 in isolation.
	ownerIdx := strings.Index(string(raw), "\nowner() {")
	idx := strings.Index(string(raw), "\nb=corral-send-$$")
	if ownerIdx < 0 || idx < 0 {
		t.Fatal("could not find owner()/owned() markers")
	}
	harness := string(raw[ownerIdx:idx]) + "\nkind=claude cx= mode=herdr p=p1 sock=/s gate=free\nowned 0 0\n"
	os.WriteFile(filepath.Join(dir, "herdr"), []byte("#!/bin/sh\nexit 1\n"), 0o755)
	c := exec.Command("sh", "-c", harness)
	c.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"))
	err = c.Run()
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != exitOwnerUnread {
		t.Fatalf("owned with unreadable screen: err=%v, want exit %d", err, exitOwnerUnread)
	}
	// Also verify stop gate with unreadable screen exits 19 rather than 15.
	harnessStop := string(raw[ownerIdx:idx]) + "\nkind=claude cx= mode=herdr p=p1 sock=/s gate=stop\nowned 0 0\n"
	c2 := exec.Command("sh", "-c", harnessStop)
	c2.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"))
	err = c2.Run()
	if !errors.As(err, &ee) || ee.ExitCode() != exitOwnerUnread {
		t.Fatalf("stop owned with unreadable screen: err=%v, want exit %d", err, exitOwnerUnread)
	}
	// lineNotTyped and gateRefused must recognize exitOwnerUnread as nothing typed.
	unreadErr := &sshx.ExitError{Status: exitOwnerUnread}
	if !lineNotTyped(unreadErr) {
		t.Error("lineNotTyped(exitOwnerUnread) = false, want true")
	}
	if !gateRefused(unreadErr) {
		t.Error("gateRefused(exitOwnerUnread) = false, want true")
	}
}

// Sending text identical to Claude Code's greyed-out suggestion (single-line or
// wrapped across lines) must NOT make pasted() spin 47 iterations waiting for
// plain-text boxread() to differ from b0; taken() must succeed immediately when
// Enter clears the box and still exit 18 if Enter truly fails.
func TestConvoGatedSuggestionMatchSkipsPasteSpin(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	cmd := convoGatedCommand("free", "herdr", "", "p1", "/run/x.sock", 1, 1, sidA, 2, nil, 0)
	bodyStart := strings.Index(cmd, "sh -c ") + len("sh -c ")
	bodyEnd := strings.Index(cmd, " corral ")
	quotedScript := cmd[bodyStart:bodyEnd]
	dir := t.TempDir()
	scriptPath := filepath.Join(dir, "gated.sh")
	if out, err := exec.Command("sh", "-c", "printf %s "+quotedScript+" > "+scriptPath).CombinedOutput(); err != nil {
		t.Fatalf("extract script: %v %s", err, out)
	}
	raw, err := os.ReadFile(scriptPath)
	if err != nil {
		t.Fatal(err)
	}
	// Extract owner/owned/boxread/boxbefore/pasted/taken from convoGated.
	// Find start of owner() and end of taken().
	ownerIdx := strings.Index(string(raw), "\nowner() {")
	ownedEnd := strings.Index(string(raw), "\nb=corral-send-$$")
	boxIdx := strings.Index(string(raw), "\ncc= b0= b1=")
	takenEnd := strings.Index(string(raw), "\nif [ -n \"$gate\" ]; then")
	if ownerIdx < 0 || ownedEnd < 0 || boxIdx < 0 || takenEnd < 0 {
		t.Fatal("could not locate convoGated functions")
	}
	funcs := string(raw[ownerIdx:ownedEnd]) + "\n" + string(raw[boxIdx:takenEnd]) + "\n"
	rule := strings.Repeat("─", 40)
	foot := "  Opus 5.5·H | Ctx 132.6K\n  ⏵⏵ auto mode on (shift+tab to cycle)"

	for _, tc := range []struct {
		name      string
		boxBefore string
		msg       string
	}{
		{"single-line suggestion", "❯ 帮我开好实验 2", "帮我开好实验 2"},
		{"wrapped suggestion", "❯ 帮我开好\n  实验 2", "帮我开好实验 2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			subDir := t.TempDir()
			msgFile := filepath.Join(subDir, "msg")
			os.WriteFile(msgFile, []byte(tc.msg), 0o600)
			beforeScreen := filepath.Join(subDir, "before.txt")
			afterScreen := filepath.Join(subDir, "after.txt")
			os.WriteFile(beforeScreen, []byte(rule+"\n"+tc.boxBefore+"\n"+rule+"\n"+foot+"\n"), 0o600)
			os.WriteFile(afterScreen, []byte("> "+tc.msg+"\n\n* Enchanting… (1s)\n"+rule+"\n❯ \n"+rule+"\n"+foot+"\n"), 0o600)
			countFile := filepath.Join(subDir, "reads")
			stateFile := filepath.Join(subDir, "state")
			os.WriteFile(stateFile, []byte("before"), 0o600)
			herdrStub := fmt.Sprintf(`#!/bin/sh
if [ "$1" = pane ] && [ "$2" = read ]; then
  printf x >> %q
  if [ "$(cat %q)" = before ]; then cat %q; else cat %q; fi
  exit 0
fi
exit 0
`, countFile, stateFile, beforeScreen, afterScreen)
			os.WriteFile(filepath.Join(subDir, "herdr"), []byte(herdrStub), 0o755)
			harness := funcs + fmt.Sprintf(`
kind=claude cx= oc= noenter= mode=herdr p=p1 sock=/s gate=free f=%q
gate_enter() { return 0; }
boxbefore
pasted
reads_after_pasted=$(wc -c < %q | tr -d ' ')
printf after > %q
taken
reads_after_taken=$(wc -c < %q | tr -d ' ')
printf '%%s %%s\n' "$reads_after_pasted" "$reads_after_taken"
`, msgFile, countFile, stateFile, countFile)
			c := exec.Command("sh", "-c", harness)
			c.Env = append(os.Environ(), "PATH="+subDir+":"+os.Getenv("PATH"))
			out, err := c.CombinedOutput()
			if err != nil {
				t.Fatalf("harness failed: %v\n%s", err, out)
			}
			fields := strings.Fields(string(out))
			if len(fields) != 2 {
				t.Fatalf("unexpected output: %q", out)
			}
			// boxbefore() does 1 read; pasted() must do 0 reads (not 47!); taken() does 1 read.
			if fields[0] != "1" {
				t.Errorf("reads after boxbefore+pasted = %s, want 1 (pasted must not spin 47x on suggestion match)", fields[0])
			}
			if fields[1] != "2" {
				t.Errorf("reads after taken = %s, want 2", fields[1])
			}
		})
	}
	// If Enter truly fails to send the matched suggestion, taken() must still
	// retry Enter once and exit 18 (enter_not_taken).
	t.Run("stuck suggestion still exits 18", func(t *testing.T) {
		subDir := t.TempDir()
		msg := "帮我开好实验 2"
		msgFile := filepath.Join(subDir, "msg")
		os.WriteFile(msgFile, []byte(msg), 0o600)
		stuckScreen := filepath.Join(subDir, "stuck.txt")
		os.WriteFile(stuckScreen, []byte(rule+"\n❯ "+msg+"\n"+rule+"\n"+foot+"\n"), 0o600)
		enterLog := filepath.Join(subDir, "enters")
		herdrStub := fmt.Sprintf(`#!/bin/sh
if [ "$1" = pane ] && [ "$2" = read ]; then cat %q; exit 0; fi
if [ "$1" = pane ] && [ "$2" = send-keys ]; then printf x >> %q; exit 0; fi
exit 0
`, stuckScreen, enterLog)
		os.WriteFile(filepath.Join(subDir, "herdr"), []byte(herdrStub), 0o755)
		harness := funcs + fmt.Sprintf(`
kind=claude cx= oc= noenter= mode=herdr p=p1 sock=/s gate=free f=%q
gate_enter() { return 0; }
boxbefore
pasted
taken
`, msgFile)
		c := exec.Command("sh", "-c", harness)
		c.Env = append(os.Environ(), "PATH="+subDir+":"+os.Getenv("PATH"))
		err := c.Run()
		var ee *exec.ExitError
		if !errors.As(err, &ee) || ee.ExitCode() != 18 {
			t.Fatalf("stuck suggestion: err=%v, want exit 18", err)
		}
		if b, _ := os.ReadFile(enterLog); len(b) != 1 {
			t.Errorf("retry enters = %d, want 1", len(b))
		}
	})
}
