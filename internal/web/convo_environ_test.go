package web

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

// A process's environment is read a variable a line: a value with a line
// break in it ("OTHER=x\nHERDR_PANE_ID=…") is not taken for a variable of
// its own — a process is not bound to a pane it is not in.
func TestConvoLocateEnvironNotForged(t *testing.T) {
	if _, err := os.Stat("/proc/self/stat"); err != nil {
		t.Skip("no /proc")
	}
	home := t.TempDir()
	sessions := filepath.Join(home, ".claude", "sessions")
	os.MkdirAll(sessions, 0o755)
	start := func(env ...string) int {
		p := exec.Command("sleep", "60")
		p.Env = append([]string{"PATH=/usr/bin:/bin"}, env...)
		if err := p.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { p.Process.Kill(); p.Wait() })
		pid := p.Process.Pid
		var st string
		for i := 0; i < 50 && st == ""; i++ {
			b, _ := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
			if j := strings.LastIndex(string(b), ") "); j >= 0 {
				if f := strings.Fields(string(b)[j+2:]); len(f) > 19 {
					st = f[19]
				}
			}
			if st == "" {
				time.Sleep(10 * time.Millisecond)
			}
		}
		doc := `{"pid":` + strconv.Itoa(pid) + `,"sessionId":"0b7c2a1e-1111-4222-8333-44445555` + strconv.Itoa(1000+pid%9000) + `","procStart":"` + st + `"}`
		os.WriteFile(filepath.Join(sessions, strconv.Itoa(pid)+".json"), []byte(doc), 0o644)
		return pid
	}
	forged := start("OTHER=x\nHERDR_PANE_ID=w1:p9\nHERDR_SOCKET_PATH=/forged.sock\nTMUX_PANE=%9")
	real := start("HERDR_PANE_ID=w2:p1", "HERDR_SOCKET_PATH=/real.sock")
	c := exec.Command("sh", "-c", convoLocateCommand("herdr", "", ""))
	c.Env = []string{"HOME=" + home, "PATH=/usr/bin:/bin"}
	out, err := c.Output()
	if err != nil {
		t.Fatal(err)
	}
	by := map[int64]convoProc{}
	for _, p := range parseConvoLocate(string(out)).procs {
		by[p.pid] = p
	}
	if p, ok := by[int64(forged)]; !ok || p.herdrPane != "" || p.herdrSocket != "" || p.tmuxPane != "" {
		t.Errorf("forged in a value: found %v, pane %q socket %q tmux %q", ok, p.herdrPane, p.herdrSocket, p.tmuxPane)
	}
	if p := by[int64(real)]; p.herdrPane != "w2:p1" || p.herdrSocket != "/real.sock" {
		t.Errorf("its own: pane %q socket %q", p.herdrPane, p.herdrSocket)
	}
}

// No script of the scan's or the gated command's reads an environment
// with its NULs alone made line breaks. (A guard: the gated command's own
// read is of a process in front on a terminal, not run here.)
func TestConvoEnvironReadsByRecord(t *testing.T) {
	// (Its quotes as the command has them, quoted for the shell: ' as '\''.)
	old := regexp.MustCompile(`tr ['\\]*\\0['\\]* ['\\]*\\n['\\]* <"[^"]*environ"`)
	for name, script := range map[string]string{
		"locate herdr": convoLocateCommand("herdr", "", ""),
		"locate pids":  convoLocatePidsCommand(50, []int{100, 200}),
		"locate tmux":  convoLocateCommand("tmux", "main", "%1"),
		"fresh status": freshStatusCommand(100),
		"line alive":   lineAliveCommand(100, 200),
		"bg tail":      bgTailCommand([]string{"0b7c2a1e-1111-4222-8333-444455556666"}, 4096),
		"cmd scan":     cmdScanCommand("w:0b7c2a1e-1111-4222-8333-444455556666"),
		"fork find":    forkFindCommand("claude", "0b7c2a1e-1111-4222-8333-444455556666"),
		"logs wrote":   logsWroteCommand([]string{"claude:0b7c2a1e-1111-4222-8333-444455556666"}),
		"gated herdr":  convoGated("", "herdr", "", "w1:p1", "/s", 1, 1, "0b7c2a1e-1111-4222-8333-444455556666", "0", nil, 0),
		"gated tmux":   convoGated("", "tmux", "main", "%1", "", 1, 1, "0b7c2a1e-1111-4222-8333-444455556666", "0", nil, 0),
	} {
		if m := old.FindString(script); m != "" {
			t.Errorf("%s: %s", name, m)
		}
	}
}
