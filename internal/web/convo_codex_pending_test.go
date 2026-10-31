package web

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
)

// codex_pending (codexPendingSh, the same words in locating's script and the
// gated command's), run here on a stand-in /proc dir whose fd links point
// into a home of the test's: a writer lock held without its log open is a
// conversation begun anew (/new), its log not begun yet.
func TestCodexPending(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	home := t.TempDir()
	proc := filepath.Join(t.TempDir(), "123")
	os.MkdirAll(filepath.Join(proc, "fd"), 0o755)
	const a, b = "01a18fc1-27bd-7b03-9702-4a4d49147ba8", "01a18fc1-3785-7b90-b732-c90708adac93"
	logOf := func(id string) string {
		return filepath.Join(home, ".codex/sessions/2026/10/31/rollout-2026-10-31T02-19-22-"+id+".jsonl")
	}
	lockOf := func(id string) string { return filepath.Join(home, ".codex/thread-writer-locks/"+id+".lock") }
	n := 3
	open := func(target string) {
		os.Symlink(target, filepath.Join(proc, "fd", strconv.Itoa(n)))
		n++
	}
	pending := func() bool {
		c := exec.Command("sh", "-c", codexPendingSh+"\ncodex_pending \"$1\"; printf %s \"$pending\"", "sh", proc)
		c.Env = append(os.Environ(), "HOME="+home)
		out, err := c.Output()
		if err != nil {
			t.Fatal(err)
		}
		return string(out) == "1"
	}
	open(logOf(a))
	open(lockOf(a))
	if pending() {
		t.Error("A's log and lock: not pending")
	}
	open("/elsewhere/.codex/thread-writer-locks/" + b + ".lock") // (not under its home: not its)
	open(filepath.Join(home, ".codex/thread-writer-locks/not-an-id.lock"))
	if pending() {
		t.Error("locks not its, or not a session's: not pending")
	}
	open(lockOf(b))
	if !pending() {
		t.Error("B's lock, no log of B: pending (begun anew)")
	}
	open(logOf(b))
	if pending() {
		t.Error("B's log open too: not pending")
	}
}
