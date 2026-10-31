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
	home := filepath.Join(t.TempDir(), "user name") // (review478: a home with a space in it)
	proc := filepath.Join(t.TempDir(), "123")
	os.MkdirAll(filepath.Join(proc, "fd"), 0o755)
	const a, b, c, s = "01a18fc1-27bd-7b03-9702-4a4d49147ba8", "01a18fc1-3785-7b90-b732-c90708adac93", "01a18fc1-4444-7b90-b732-c90708adac93", "01a18ff7-432f-7a80-bb03-7c6c92a8fd35"
	logOf := func(id, source string) string {
		p := filepath.Join(home, ".codex/sessions/2026/10/31/rollout-2026-10-31T02-19-22-"+id+".jsonl")
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte(`{"timestamp":"x","type":"session_meta","payload":{"id":"`+id+`","thread_source": "`+source+`"}}`+"\n"), 0o644)
		at := time.Now().Add(-time.Minute) // (last written a while ago: before any lock of the test)
		os.Chtimes(p, at, at)
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
	var young string
	fds := func() (log, pend string) {
		cmd := exec.Command("sh", "-c", codexFdsSh+"\ncodex_fds \"$1\"; printf '%s\\n%s\\n%s' \"$cxlog\" \"$cxpend\" \"$cxnew\"", "sh", proc)
		cmd.Env = append(os.Environ(), "HOME="+home)
		out, err := cmd.Output()
		if err != nil {
			t.Fatal(err)
		}
		parts := strings.SplitN(string(out), "\n", 3)
		young = parts[2]
		return filepath.Base(parts[0]), parts[1]
	}
	open(lockAged(a, time.Minute)) // (taken at its start, the first)
	if log, pend := fds(); log != "." || pend != a {
		t.Errorf("just started, A's lock only: log %q, pending %q", log, pend)
	}
	open(logOf(a, "user"))
	if log, pend := fds(); !strings.Contains(log, a) || pend != "" {
		t.Errorf("A's log: log %q, pending %q", log, pend)
	}
	const y = "01a18ff7-5555-7a80-bb03-7c6c92a8fd35"
	open(lockAged(y, 2*time.Second)) // (review475: a subagent's lock, its log not open yet, two seconds old)
	if _, pend := fds(); pend != "" || young != "1" {
		t.Errorf("a lock just taken: not pending (%q), but new (%q)", pend, young)
	}
	open(logOf(y, "subagent"))
	if fds(); young != "" {
		t.Errorf("its log open: not new, %q", young)
	}
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
	aLog := filepath.Join(home, ".codex/sessions/2026/10/31/rollout-2026-10-31T02-19-22-"+a+".jsonl")
	os.Chtimes(aLog, time.Now(), time.Now()) // (review478: A's log written on after B's lock was taken)
	if _, pend := fds(); pend != b {
		t.Errorf("A's log written after B's lock: B still the one, pending %q", pend)
	}
	open(lockAged(c, 5*time.Second)) // (taken after B's: the one it is at)
	if _, pend := fds(); pend != c {
		t.Errorf("two such: the one taken last, pending %q", pend)
	}
	cLog := logOf(c, "user") // (review477: B left with no message, C begun: B's lock, taken before C's log, a leftover)
	os.Chtimes(cLog, time.Now(), time.Now())
	open(cLog)
	if log, pend := fds(); !strings.Contains(log, c) || pend != "" {
		t.Errorf("C's log open, B's lock left: log %q, pending %q", log, pend)
	}
	// (review478: two locks without a log taken in the same second, after
	// C's: which it is at not known — none pending, new.)
	const d, e = "01a18fc1-dddd-7b90-b732-c90708adac93", "01a18fc1-eeee-7b90-b732-c90708adac93"
	same := time.Now().Add(-4 * time.Second).Truncate(time.Second)
	for _, id := range []string{d, e} {
		p := lockOf(id)
		os.Chtimes(p, same, same)
		open(p)
	}
	if _, pend := fds(); pend != "" || young != "1" {
		t.Errorf("two locks of one second: pending %q, new %q", pend, young)
	}
}

// review490: where which conversation it is at cannot be told from its
// locks, none is pending and sending waits (cxnew): a lock taken in the
// second the mark was; no mark for the log it writes. A fork's log
// (<id>_<part>) is its conversation's.
func TestCodexFdsUntold(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	const a, b, f = "01a18fc1-27bd-7b03-9702-4a4d49147ba8", "01a18fc1-3785-7b90-b732-c90708adac93", "01a18fc1-ffff-7b90-b732-c90708adac93"
	run := func(t *testing.T, logName string, locks map[string]time.Time) (log, pend, young string) {
		home := t.TempDir()
		proc := filepath.Join(t.TempDir(), "123")
		os.MkdirAll(filepath.Join(proc, "fd"), 0o755)
		n := 3
		open := func(target string) {
			os.Symlink(target, filepath.Join(proc, "fd", strconv.Itoa(n)))
			n++
		}
		if logName != "" {
			p := filepath.Join(home, ".codex/sessions/2026/10/31/"+logName)
			os.MkdirAll(filepath.Dir(p), 0o755)
			os.WriteFile(p, []byte(`{"type":"session_meta","payload":{"thread_source": "user"}}`+"\n"), 0o644)
			open(p)
		}
		for id, at := range locks {
			p := filepath.Join(home, ".codex/thread-writer-locks/"+id+".lock")
			os.MkdirAll(filepath.Dir(p), 0o755)
			os.WriteFile(p, nil, 0o644)
			os.Chtimes(p, at, at)
			open(p)
		}
		cmd := exec.Command("sh", "-c", codexFdsSh+"\ncodex_fds \"$1\"; printf '%s\\n%s\\n%s' \"$cxlog\" \"$cxpend\" \"$cxnew\"", "sh", proc)
		cmd.Env = append(os.Environ(), "HOME="+home)
		out, err := cmd.Output()
		if err != nil {
			t.Fatal(err)
		}
		parts := strings.SplitN(string(out), "\n", 3)
		return filepath.Base(parts[0]), parts[1], parts[2]
	}
	sec := time.Now().Add(-time.Minute).Truncate(time.Second)
	if _, pend, young := run(t, "rollout-2026-10-31T02-19-22-"+a+".jsonl", map[string]time.Time{a: sec, b: sec.Add(500 * time.Millisecond)}); pend != "" || young != "1" {
		t.Errorf("B's lock in A's second: pending %q, new %q", pend, young)
	}
	if _, pend, young := run(t, "rollout-2026-10-31T02-19-22-"+a+".jsonl", map[string]time.Time{b: sec}); pend != "" || young != "1" {
		t.Errorf("no lock of A's: pending %q, new %q", pend, young)
	}
	// (A fork's log, A_F, is F's, as locating reads it; review491: A's lock
	// and a lock B left between them are not taken for the one it is at.)
	fork := "rollout-2026-10-31T02-19-22-" + a + "_" + f + ".jsonl"
	if _, pend, young := run(t, fork, map[string]time.Time{f: sec.Add(20 * time.Second), a: sec, b: sec.Add(10 * time.Second)}); pend != "" || young != "" {
		t.Errorf("a fork's log, A's lock and B's left: F's, pending %q, new %q", pend, young)
	}
	if _, pend, young := run(t, fork, map[string]time.Time{f: sec, b: sec.Add(10 * time.Second)}); pend != b || young != "" {
		t.Errorf("a fork's log, then /new: B, pending %q, new %q", pend, young)
	}
}

// Codex's input empty at rest: not a draft of one line or more, under a
// first line empty too (review475).
func TestCodexIdleInput(t *testing.T) {
	foot := "\n\n  gpt-mock default · ~/p\n  ? for shortcuts\n"
	for screen, want := range map[string]error{
		"• done\n\n› Ask Codex to do anything" + foot:                                                                     nil,
		"• done\n\n› half a draft" + foot:                                                                                 errDraft,
		"• done\n\n› \n  keep this draft" + foot:                                                                          errDraft,
		"• done\n\n› \n  I want to edit this file" + foot:                                                                 errDraft,
		"• done\n\n› \n  Use the app to view this report" + foot:                                                          errDraft,
		"• done\n\n› \n  \n  keep this draft" + foot:                                                                      errDraft,
		"• done\n\n› Ask Codex to do anything\n\n  gpt-6-sol high · /srv/x\n  esc again to edit previous message\n":       nil,
		"• done\n\n› first\n  second" + foot:                                                                              errDraft,
		"• done\n\n› Ask Codex to do anything\n\n  gpt-5.1-codex default · ~/my project\n  ? for shortcuts\n":             nil,
		"• done\n\n› Ask Codex to do anything\n\n  gpt-mock default · ~/p\n  ? for shortcuts  ⚠ 1 warning · f2 to view\n": nil,
		"• done\n\n› Ask Codex to do anything\n\n  openai/gpt-5.1-codex high · ~/p\n  ? for shortcuts\n":                  nil,
		"• done\n\n› Ask Codex to do anything\n\n  Context 100% left       ⚠ 2 warnings · f2 to view\n":                   nil,
		"• done\n\n› \n  ? for shortcuts in this document" + foot:                                                         errDraft,
		"• done\n\n› \n  text  ⚠ 2 warnings · f2 to view" + foot:                                                          errDraft,
		"• done\n\n› Implement {feature}" + foot:                                                                          errDraft,
		"• Working (3s • esc to interrupt)\n\n› Ask Codex to do anything" + foot:                                          errBusyAgent,
		"no input here" + foot: errScreen,
	} {
		if got := codexIdleInput(screen); got != want {
			t.Errorf("%q: %v, want %v", screen[:20], got, want)
		}
	}
}
