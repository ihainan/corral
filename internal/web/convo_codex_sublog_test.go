package web

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ihainan/corral/internal/sftplite"
	"github.com/pkg/sftp"
)

func sftpOf(t *testing.T) *sftplite.Client {
	t.Helper()
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
	return c
}

// review495: a subagent's log is looked for in the days from its session's
// log's to today's (a session run on for days); no folder on the way a
// link; a folder listed only in part, it not found, is not a refusal.
func TestCodexSubagentLog(t *testing.T) {
	c := sftpOf(t)
	home := t.TempDir()
	sessions := filepath.Join(home, ".codex", "sessions")
	const main, sub = "01a18ff7-4138-7890-9ecd-1aee1502e132", "01a19097-c850-7c33-b8f4-e6d581ae9e27"
	day := func(d string) string {
		p := filepath.Join(sessions, d)
		os.MkdirAll(p, 0o755)
		return p
	}
	root := filepath.Join(day("2026/10/29"), "rollout-2026-10-29T09-00-00-"+main+".jsonl")
	os.WriteFile(root, []byte(`{"type":"session_meta","payload":{"id":"`+main+`"}}`+"\n"), 0o644)
	meta := `{"type":"session_meta","payload":{"thread_source":"subagent","parent_thread_id":"` + main + `"}}` + "\n"
	now := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	// (Sent off two days after its session began.)
	want := filepath.Join(day("2026/10/31"), "rollout-2026-10-31T10-00-00-"+sub+".jsonl")
	os.WriteFile(want, []byte(meta), 0o644)
	if got, err := codexSubagentLog(c, root, main, sub, now); err != nil || got != want {
		t.Fatalf("two days on: %q %v", got, err)
	}
	// (Found before, read again: still plain; a link put in its place, or in
	// a folder's, since: refused — review500.)
	if err := codexSubagentStill(c, root, want); err != nil {
		t.Errorf("still plain: %v", err)
	}
	os.Rename(want, want+".was")
	os.Symlink(want+".was", want)
	if err := codexSubagentStill(c, root, want); !errors.Is(err, errSubagentFile) {
		t.Errorf("a link in its place: %v", err)
	}
	os.Remove(want)
	os.Rename(want+".was", want)
	moved := filepath.Join(sessions, "2026", "10", "31.was")
	os.Rename(filepath.Join(sessions, "2026", "10", "31"), moved)
	os.Symlink(moved, filepath.Join(sessions, "2026", "10", "31"))
	if err := codexSubagentStill(c, root, want); !errors.Is(err, errSubagentFile) {
		t.Errorf("its day's folder a link: %v", err)
	}
	os.Remove(filepath.Join(sessions, "2026", "10", "31"))
	os.Rename(moved, filepath.Join(sessions, "2026", "10", "31"))
	// (Not one of another session's.)
	if _, err := codexSubagentLog(c, root, "01a18ff7-0000-7890-9ecd-1aee1502e132", sub, now); !errors.Is(err, errSubagentFile) {
		t.Errorf("another session's: %v", err)
	}
	// (A month's folder on the way a link: refused, whatever it leads to.)
	os.RemoveAll(filepath.Join(sessions, "2026/10/31"))
	elsewhere := t.TempDir()
	os.MkdirAll(filepath.Join(elsewhere, "01"), 0o755)
	os.WriteFile(filepath.Join(elsewhere, "01", "rollout-2026-11-01T10-00-00-"+sub+".jsonl"), []byte(meta), 0o644)
	os.Symlink(elsewhere, filepath.Join(sessions, "2026/11"))
	if _, err := codexSubagentLog(c, root, main, sub, now.AddDate(0, 0, 1)); !errors.Is(err, errSubagentFile) {
		t.Errorf("a link on the way: %v", err)
	}
	os.Remove(filepath.Join(sessions, "2026/11"))
	// (A folder of more than is listed, it not among those listed: not said
	// to be no subagent's.)
	big := day("2026/10/30")
	for i := 0; i < 4100; i++ {
		os.WriteFile(filepath.Join(big, fmt.Sprintf("rollout-2026-10-30T00-00-00-%08d.jsonl", i)), nil, 0o644)
	}
	if _, err := codexSubagentLog(c, root, main, sub, now); !errors.Is(err, errSubagentUnsure) {
		t.Errorf("a big folder: %v", err)
	}
	// (None anywhere, no folder cut short: none.)
	os.RemoveAll(big)
	if _, err := codexSubagentLog(c, root, main, sub, now); !errors.Is(err, errSubagentFile) {
		t.Errorf("none: %v", err)
	}
}
