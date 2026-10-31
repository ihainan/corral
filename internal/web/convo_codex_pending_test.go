package web

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// codex_fds (codexFdsSh, the same words in locating's script and the gated
// command's), run here on a stand-in /proc dir whose fd links point into a
// home of the test's: its own log (not a subagent's), and a conversation's
// writer lock held without its log open — its session the lock's.
func TestCodexFds(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	home := t.TempDir()
	proc := filepath.Join(t.TempDir(), "123")
	os.MkdirAll(filepath.Join(proc, "fd"), 0o755)
	const a, b, c, s = "01a18fc1-27bd-7b03-9702-4a4d49147ba8", "01a18fc1-3785-7b90-b732-c90708adac93", "01a18fc1-4444-7b90-b732-c90708adac93", "01a18ff7-432f-7a80-bb03-7c6c92a8fd35"
	logOf := func(id, source string) string {
		p := filepath.Join(home, ".codex/sessions/2026/10/31/rollout-2026-10-31T02-19-22-"+id+".jsonl")
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte(`{"timestamp":"x","type":"session_meta","payload":{"id":"`+id+`","thread_source": "`+source+`"}}`+"\n"), 0o644)
		return p
	}
	// (A lock held a while: two seconds at least; young, a subagent's just begun.)
	lockAged := func(id string, age time.Duration) string {
		p := filepath.Join(home, ".codex/thread-writer-locks/"+id+".lock")
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, nil, 0o644)
		at := time.Now().Add(-age)
		os.Chtimes(p, at, at)
		return p
	}
	lockOf := func(id string) string { return lockAged(id, 10*time.Second) }
	n := 3
	open := func(target string) {
		os.Symlink(target, filepath.Join(proc, "fd", strconv.Itoa(n)))
		n++
	}
	fds := func() (log, pend string) {
		cmd := exec.Command("sh", "-c", codexFdsSh+"\ncodex_fds \"$1\"; printf '%s\\n%s' \"$cxlog\" \"$cxpend\"", "sh", proc)
		cmd.Env = append(os.Environ(), "HOME="+home)
		out, err := cmd.Output()
		if err != nil {
			t.Fatal(err)
		}
		l, p, _ := strings.Cut(string(out), "\n")
		return filepath.Base(l), p
	}
	open(lockOf(a))
	if log, pend := fds(); log != "." || pend != a {
		t.Errorf("just started, A's lock only: log %q, pending %q", log, pend)
	}
	open(logOf(a, "user"))
	if log, pend := fds(); !strings.Contains(log, a) || pend != "" {
		t.Errorf("A's log: log %q, pending %q", log, pend)
	}
	const y = "01a18ff7-5555-7a80-bb03-7c6c92a8fd35"
	open(lockAged(y, 0)) // (review475: a subagent's lock, its log not open yet, a moment old)
	if _, pend := fds(); pend != "" {
		t.Errorf("a lock just taken: not pending, %q", pend)
	}
	open(logOf(y, "subagent"))
	open(lockOf(s))
	open(logOf(s, "subagent"))
	if log, pend := fds(); !strings.Contains(log, a) || pend != "" {
		t.Errorf("a subagent's log too: still A's, log %q, pending %q", log, pend)
	}
	open("/elsewhere/.codex/thread-writer-locks/" + b + ".lock") // (not under its home: not its)
	open(filepath.Join(home, ".codex/thread-writer-locks/not-an-id.lock"))
	if _, pend := fds(); pend != "" {
		t.Errorf("locks not its, or not a session's: pending %q", pend)
	}
	open(lockOf(b))
	if log, pend := fds(); !strings.Contains(log, a) || pend != b {
		t.Errorf("/new: B's lock, no log of B: log %q (A's still), pending %q", log, pend)
	}
	open(lockOf(c))
	if _, pend := fds(); pend != "?" {
		t.Errorf("two such: not told which, pending %q", pend)
	}
	open(logOf(c, "user"))
	open(logOf(b, "user"))
	if _, pend := fds(); pend != "" {
		t.Errorf("their logs open: pending %q", pend)
	}
}

// Codex's input empty at rest: not a draft of one line or more, under a
// first line empty too (review475).
func TestCodexIdleInput(t *testing.T) {
	foot := "\n\n  gpt-mock default · ~/p\n  ? for shortcuts\n"
	for screen, want := range map[string]error{
		"• done\n\n› Ask Codex to do anything" + foot:                            nil,
		"• done\n\n› half a draft" + foot:                                        errDraft,
		"• done\n\n› \n  keep this draft" + foot:                                 errDraft,
		"• done\n\n› first\n  second" + foot:                                     errDraft,
		"• done\n\n› Implement {feature}" + foot:                                 errDraft,
		"• Working (3s • esc to interrupt)\n\n› Ask Codex to do anything" + foot: errBusyAgent,
		"no input here" + foot:                                                   errScreen,
	} {
		if got := codexIdleInput(screen); got != want {
			t.Errorf("%q: %v, want %v", screen[:20], got, want)
		}
	}
}
