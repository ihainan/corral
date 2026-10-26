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
