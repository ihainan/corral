package web

import (
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/pkg/sftp"

	"github.com/ihainan/corral/internal/sftplite"
)

// What Claude Code's file says of the bound process is read with its log
// not written yet too (a new conversation): from the home, where locating
// read that file. Without, a new conversation with /usage open showed no
// word of it, and a message to it was refused (10-31).
func TestConvoProcStatusWithoutLog(t *testing.T) {
	home := t.TempDir()
	cr, sw := io.Pipe()
	sr, cw := io.Pipe()
	srv, err := sftp.NewServer(struct {
		io.Reader
		io.WriteCloser
	}{sr, sw}, sftp.WithServerWorkingDirectory(home))
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
	write := func(sid, status, why string) {
		os.WriteFile(filepath.Join(home, ".claude", "sessions", "4242.json"), []byte(`{"pid":4242,"sessionId":"`+sid+`","procStart":"777","status":"`+status+`","waitingFor":"`+why+`"}`), 0o644)
	}
	st := &convoState{bind: &convoBind{PID: 4242, Start: 777, Session: sidA}}
	write(sidA, "waiting", "dialog open")
	if got := convoProcStatus(c, st); got != "waiting:dialog open" {
		t.Fatalf("no log yet: %q", got)
	}
	write(sidA, "idle", "")
	if got := convoProcStatus(c, st); got != "idle" {
		t.Fatalf("no log yet, idle: %q", got)
	}
	// (Another session's file under that pid: nothing said.)
	write("00000000-0000-4000-8000-000000000001", "waiting", "dialog open")
	if got := convoProcStatus(c, st); got != "" {
		t.Fatalf("another session's file: %q", got)
	}
	// (A log that is not Claude Code's: nothing read, as before.)
	write(sidA, "waiting", "dialog open")
	st.log = "/x/.codex/sessions/rollout.jsonl"
	if got := convoProcStatus(c, st); got != "" {
		t.Fatalf("another agent's log: %q", got)
	}
	st.log = home + "/.claude/projects/p/" + sidA + ".jsonl"
	if got := convoProcStatus(c, st); got != "waiting:dialog open" {
		t.Fatalf("its log: %q", got)
	}
}
