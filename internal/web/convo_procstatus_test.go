package web

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/pkg/sftp"

	"github.com/ihainan/corral/internal/sftplite"
)

// What Claude Code's file says of the bound process is read with its log
// not written yet too (a new conversation): on the host, in $HOME, where
// locating read that file — not where the SFTP session starts, which may be
// elsewhere. Without, a new conversation with /usage open showed no word of
// it, and a message to it was refused (10-31; review480).
func TestConvoFreshStatus(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh here")
	}
	home := t.TempDir()
	os.MkdirAll(filepath.Join(home, ".claude", "sessions"), 0o755)
	write := func(sid, status, why string) {
		os.WriteFile(filepath.Join(home, ".claude", "sessions", "4242.json"), []byte(`{"pid":4242,"sessionId":"`+sid+`","procStart":"777","status":"`+status+`","waitingFor":"`+why+`"}`), 0o644)
	}
	bind := &convoBind{PID: 4242, Start: 777, Session: sidA}
	read := func() string {
		c := exec.Command(sh, "-c", freshStatusCommand(bind.PID))
		c.Dir = t.TempDir() // (the start elsewhere: $HOME says where)
		c.Env = append(os.Environ(), "HOME="+home)
		out, _ := c.Output()
		return procStatusOf(out, bind)
	}
	write(sidA, "waiting", "dialog open")
	if got := read(); got != "waiting:dialog open" {
		t.Fatalf("no log yet: %q", got)
	}
	write(sidA, "idle", "")
	if got := read(); got != "idle" {
		t.Fatalf("no log yet, idle: %q", got)
	}
	// (Another session's file under that pid: nothing said.)
	write("00000000-0000-4000-8000-000000000001", "waiting", "dialog open")
	if got := read(); got != "" {
		t.Fatalf("another session's file: %q", got)
	}
	os.Remove(filepath.Join(home, ".claude", "sessions", "4242.json"))
	if got := read(); got != "" {
		t.Fatalf("no file: %q", got)
	}
}

// With a log, its file is read beside the log, over SFTP, as before.
func TestConvoProcStatusBesideLog(t *testing.T) {
	home := t.TempDir()
	cr, sw := io.Pipe()
	sr, cw := io.Pipe()
	srv, err := sftp.NewServer(struct {
		io.Reader
		io.WriteCloser
	}{sr, sw}, sftp.WithServerWorkingDirectory(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve()
	c, err := sftplite.New(cr, cw, func() { cw.Close(); cr.Close() })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close(); srv.Close() })
	os.MkdirAll(filepath.Join(home, ".claude", "sessions"), 0o755)
	os.WriteFile(filepath.Join(home, ".claude", "sessions", "4242.json"), []byte(`{"sessionId":"`+sidA+`","procStart":"777","status":"waiting","waitingFor":"dialog open"}`), 0o644)
	st := &convoState{bind: &convoBind{PID: 4242, Start: 777, Session: sidA}, log: home + "/.claude/projects/p/" + sidA + ".jsonl"}
	if got := convoProcStatus(c, st); got != "waiting:dialog open" {
		t.Fatalf("its log: %q", got)
	}
	for _, log := range []string{"", "/x/.codex/sessions/rollout.jsonl"} {
		st.log = log
		if got := convoProcStatus(c, st); got != "" {
			t.Fatalf("log %q: %q", log, got)
		}
	}
}
