//go:build linux

package web

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/ihainan/corral/internal/corrald/agent"
	"github.com/ihainan/corral/internal/corrald/daemon"
	"github.com/ihainan/corral/internal/corrald/gw"
	"github.com/ihainan/corral/internal/corrald/proto"
	"github.com/ihainan/corral/internal/perf"
	"github.com/ihainan/corral/internal/store"
	"github.com/ihainan/corral/internal/testutil"
)

const fakeTUIAgentSrc = `package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

func setRaw(fd uintptr) {
	var t syscall.Termios
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, uintptr(syscall.TCGETS), uintptr(unsafe.Pointer(&t))); errno != 0 {
		return
	}
	t.Iflag &^= syscall.IGNBRK | syscall.BRKINT | syscall.PARMRK | syscall.ISTRIP | syscall.INLCR | syscall.IGNCR | syscall.ICRNL | syscall.IXON
	t.Oflag &^= syscall.OPOST
	t.Lflag &^= syscall.ECHO | syscall.ECHONL | syscall.ICANON | syscall.ISIG | syscall.IEXTEN
	t.Cflag &^= syscall.CSIZE | syscall.PARENB
	t.Cflag |= syscall.CS8
	t.Cc[syscall.VMIN] = 1
	t.Cc[syscall.VTIME] = 0
	syscall.Syscall(syscall.SYS_IOCTL, fd, uintptr(syscall.TCSETS), uintptr(unsafe.Pointer(&t)))
}

func drawScreen(content []byte, title []byte) {
	var buf bytes.Buffer
	buf.WriteString("\x1b[2J\x1b[H")
	// Convert bare LF to CRLF for raw tty rendering.
	s := strings.ReplaceAll(string(content), "\r\n", "\n")
	buf.WriteString(strings.ReplaceAll(s, "\n", "\r\n"))
	if len(title) > 0 {
		buf.WriteString("\x1b]2;")
		buf.Write(bytes.TrimRight(title, "\r\n"))
		buf.WriteString("\x07")
	}
	_, _ = os.Stdout.Write(buf.Bytes())
}

func main() {
	setRaw(os.Stdin.Fd())
	// Enable bracketed paste mode so tmux paste-buffer -p wraps pasted text.
	_, _ = os.Stdout.WriteString("\x1b[?2004h")

	var opened []*os.File
	if files := os.Getenv("FAKE_OPEN_FILES"); files != "" {
		for _, p := range strings.Split(files, ",") {
			if p == "" {
				continue
			}
			if f, err := os.Open(p); err == nil {
				opened = append(opened, f)
			}
		}
	}
	defer func() {
		for _, f := range opened {
			_ = f.Close()
		}
	}()

	stateDir := os.Getenv("FAKE_STATE_DIR")
	screenPath := filepath.Join(stateDir, "screen.txt")
	titlePath := filepath.Join(stateDir, "title.txt")
	inputLogPath := filepath.Join(stateDir, "input.log")
	readyPath := filepath.Join(stateDir, "ready")
	pidPath := filepath.Join(stateDir, "pid")
	sessPath := os.Getenv("FAKE_CLAUDE_SESS_FILE")

	_ = os.WriteFile(pidPath, []byte(strconv.Itoa(os.Getpid())), 0o600)

	var lastScreen, lastTitle []byte
	var flickerN int
	flickerPath := filepath.Join(stateDir, "flicker")
	refresh := func() {
		sc, _ := os.ReadFile(screenPath)
		ti, _ := os.ReadFile(titlePath)
		if _, ferr := os.Stat(flickerPath); ferr == nil {
			flickerN++
			sc = append(append([]byte(nil), sc...), []byte("\nframe-"+strconv.Itoa(flickerN)+"\n")...)
		}
		if !bytes.Equal(sc, lastScreen) || !bytes.Equal(ti, lastTitle) {
			lastScreen = append([]byte(nil), sc...)
			lastTitle = append([]byte(nil), ti...)
			drawScreen(sc, ti)
		}
	}
	refresh()
	_ = os.WriteFile(readyPath, []byte("ok"), 0o600)

	go func() {
		for {
			time.Sleep(40 * time.Millisecond)
			refresh()
		}
	}()

	buf := make([]byte, 4096)
	var acc []byte
	for {
		n, err := os.Stdin.Read(buf)
		if n > 0 {
			chunk := buf[:n]
			f, oerr := os.OpenFile(inputLogPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
			if oerr == nil {
				_, _ = f.Write(chunk)
				_ = f.Close()
			}
			acc = append(acc, chunk...)

			// Hook 1: bracketed paste completed.
			if bytes.Contains(acc, []byte("\x1b[201~")) {
				onPaste := filepath.Join(stateDir, "on_paste.screen")
				if b, rerr := os.ReadFile(onPaste); rerr == nil {
					_ = os.Remove(onPaste)
					delayMs := 0
					if db, derr := os.ReadFile(filepath.Join(stateDir, "on_paste.delay_ms")); derr == nil {
						delayMs, _ = strconv.Atoi(strings.TrimSpace(string(db)))
						_ = os.Remove(filepath.Join(stateDir, "on_paste.delay_ms"))
					}
					statusPath := filepath.Join(stateDir, "on_paste.status_json")
					statusBytes, hasStatus := os.ReadFile(statusPath)
					if hasStatus == nil {
						_ = os.Remove(statusPath)
					}
					go func(nextScreen []byte, d int, st []byte, applySt bool) {
						if d > 0 {
							time.Sleep(time.Duration(d) * time.Millisecond)
						}
						if applySt && sessPath != "" {
							_ = os.WriteFile(sessPath, st, 0o600)
						}
						_ = os.WriteFile(screenPath, nextScreen, 0o600)
					}(b, delayMs, statusBytes, hasStatus == nil)
				}
				acc = nil
			}

			// Hook 2: standalone Escape (not part of CSI sequence).
			if bytes.Equal(chunk, []byte{0x1b}) {
				onEsc := filepath.Join(stateDir, "on_esc.screen")
				if b, rerr := os.ReadFile(onEsc); rerr == nil {
					_ = os.Remove(onEsc)
					_ = os.WriteFile(screenPath, b, 0o600)
				}
			}

			// Hook 3: Enter key (\r).
			if bytes.ContainsRune(chunk, '\r') {
				onEnter := filepath.Join(stateDir, "on_enter.screen")
				if b, rerr := os.ReadFile(onEnter); rerr == nil {
					onEnter2 := filepath.Join(stateDir, "on_enter2.screen")
					if b2, rerr2 := os.ReadFile(onEnter2); rerr2 == nil {
						_ = os.Remove(onEnter2)
						_ = os.WriteFile(onEnter, b2, 0o600)
					} else {
						_ = os.Remove(onEnter)
					}
					_ = os.WriteFile(screenPath, b, 0o600)
				}
			}
		}
		if err != nil {
			return
		}
	}
}
`

var (
	fakeTUIBinOnce sync.Once
	fakeTUIBinPath string
	fakeTUIBinErr  error
)

func buildFakeTUIAgent(t *testing.T) string {
	t.Helper()
	fakeTUIBinOnce.Do(func() {
		dir, err := os.MkdirTemp("", "corral-fake-tui-*")
		if err != nil {
			fakeTUIBinErr = err
			return
		}
		src := filepath.Join(dir, "main.go")
		if err := os.WriteFile(src, []byte(fakeTUIAgentSrc), 0o600); err != nil {
			fakeTUIBinErr = err
			return
		}
		bin := filepath.Join(dir, "faketui")
		cmd := exec.Command("go", "build", "-o", bin, src)
		if out, err := cmd.CombinedOutput(); err != nil {
			fakeTUIBinErr = fmt.Errorf("build fake tui: %v\n%s", err, out)
			return
		}
		fakeTUIBinPath = bin
	})
	if fakeTUIBinErr != nil {
		t.Fatal(fakeTUIBinErr)
	}
	return fakeTUIBinPath
}

type isolatedTmuxHarness struct {
	t          *testing.T
	home       string
	tmuxTmpDir string
	socketName string
	tmuxBin    string
	wrapperDir string
	session    string
	paneID     string
	stateDir   string
	agentPID   int64
	agentStart int64
	sessFile   string
	sender     *agent.PaneSender
	locator    *agent.Locator
}

func startIsolatedTmuxAgent(t *testing.T, agentComm, claudeCfgSubdir string, extraEnv []string, openFiles []string) *isolatedTmuxHarness {
	t.Helper()
	sysTmuxBin, err := exec.LookPath("tmux")
	if err != nil {
		t.Skip("tmux not installed")
	}
	for _, p := range []string{"sh", "awk", "sha256sum"} {
		if _, err := exec.LookPath(p); err != nil {
			t.Skip("missing " + p)
		}
	}
	tuiBin := buildFakeTUIAgent(t)

	home := t.TempDir()
	// Copy the system tmux binary into a 0755 directory inside home and pin
	// daemon's trusted-tmux root there so tests never depend on host directory
	// permissions or user-namespace UID mapping outside t.TempDir().
	trustedRoot := filepath.Join(home, "trusted-tmux")
	if err := os.MkdirAll(trustedRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(trustedRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	tmuxBin := filepath.Join(trustedRoot, "tmux")
	if rawTmux, err := os.ReadFile(sysTmuxBin); err != nil {
		t.Fatalf("read tmux binary: %v", err)
	} else if err := os.WriteFile(tmuxBin, rawTmux, 0o755); err != nil {
		t.Fatalf("copy tmux binary: %v", err)
	}
	if err := os.Chmod(tmuxBin, 0o755); err != nil {
		t.Fatal(err)
	}
	restoreTmuxRoot := daemon.SetTrustedTmuxCheckRootForTest(trustedRoot)
	t.Cleanup(restoreTmuxRoot)
	tmuxTmpDir, err := os.MkdirTemp("/tmp", "ctm-*")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(tmuxTmpDir, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(tmuxTmpDir) })
	stateDir := filepath.Join(home, "tui-state")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	binDir := filepath.Join(home, "agent-bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	agentExec := filepath.Join(binDir, agentComm)
	if err := os.Symlink(tuiBin, agentExec); err != nil {
		t.Fatal(err)
	}

	socketName := fmt.Sprintf("p%d-%d", os.Getpid(), time.Now().UnixNano()%1000000)
	sessionName := "p1b_sess"

	// Wrapper `tmux` binary in wrapperDir so the original convoGated shell script
	// automatically targets the exact isolated `-L socketName` tmux server.
	wrapperDir := filepath.Join(home, "shim-bin")
	if err := os.MkdirAll(wrapperDir, 0o755); err != nil {
		t.Fatal(err)
	}
	shim := fmt.Sprintf("#!/bin/sh\nTMUX_TMPDIR=%q exec %q -L %q \"$@\"\n", tmuxTmpDir, tmuxBin, socketName)
	if err := os.WriteFile(filepath.Join(wrapperDir, "tmux"), []byte(shim), 0o755); err != nil {
		t.Fatal(err)
	}

	clcfg := filepath.Join(home, ".claude")
	if claudeCfgSubdir != "" {
		clcfg = filepath.Join(home, claudeCfgSubdir)
	}
	if err := os.MkdirAll(filepath.Join(clcfg, "sessions"), 0o755); err != nil {
		t.Fatal(err)
	}

	// Build tmux new-session command launching our fake agent directly as pane PID.
	envArgs := []string{
		"HOME=" + home,
		"FAKE_STATE_DIR=" + stateDir,
		"FAKE_OPEN_FILES=" + strings.Join(openFiles, ","),
	}
	if claudeCfgSubdir != "" {
		envArgs = append(envArgs, "CLAUDE_CONFIG_DIR="+clcfg)
	}
	envArgs = append(envArgs, extraEnv...)

	var shellCmd strings.Builder
	shellCmd.WriteString("exec env")
	for _, kv := range envArgs {
		shellCmd.WriteString(" ")
		shellCmd.WriteString(strconv.Quote(kv))
	}
	shellCmd.WriteString(" ")
	shellCmd.WriteString(strconv.Quote(agentExec))

	mkCmd := exec.Command(tmuxBin, "-L", socketName, "new-session", "-d", "-x", "100", "-y", "30", "-s", sessionName, shellCmd.String())
	mkCmd.Env = append(os.Environ(), "TMUX_TMPDIR="+tmuxTmpDir)
	if out, err := mkCmd.CombinedOutput(); err != nil {
		t.Fatalf("tmux new-session failed: %v\n%s", err, out)
	}
	t.Cleanup(func() {
		killCmd := exec.Command(tmuxBin, "-L", socketName, "kill-server")
		killCmd.Env = append(os.Environ(), "TMUX_TMPDIR="+tmuxTmpDir)
		_ = killCmd.Run()
	})

	// Wait for agent to start and draw initial frame.
	waitForFile(t, filepath.Join(stateDir, "ready"))
	pidBytes := waitForFile(t, filepath.Join(stateDir, "pid"))
	agentPID, err := strconv.ParseInt(strings.TrimSpace(pidBytes), 10, 64)
	if err != nil {
		t.Fatalf("parse agent pid: %v", err)
	}
	t.Cleanup(func() {
		if p, perr := os.FindProcess(int(agentPID)); perr == nil {
			_ = p.Kill()
		}
		killCmd := exec.Command(tmuxBin, "-L", socketName, "kill-server")
		killCmd.Env = append(os.Environ(), "TMUX_TMPDIR="+tmuxTmpDir)
		_ = killCmd.Run()
		procPath := filepath.Join("/proc", strconv.FormatInt(agentPID, 10))
		for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); {
			if _, serr := os.Stat(procPath); serr != nil {
				break
			}
			time.Sleep(15 * time.Millisecond)
		}
	})
	startStr := waitForProcStart(t, int(agentPID))
	agentStart, err := strconv.ParseInt(startStr, 10, 64)
	if err != nil {
		t.Fatalf("parse agent start: %v", err)
	}

	// Query pane ID from isolated tmux server.
	qCmd := exec.Command(tmuxBin, "-L", socketName, "display-message", "-p", "-t", "="+sessionName+":", "#{pane_id}")
	qCmd.Env = append(os.Environ(), "TMUX_TMPDIR="+tmuxTmpDir)
	paneOut, err := qCmd.Output()
	if err != nil {
		t.Fatalf("query pane_id: %v", err)
	}
	paneID := strings.TrimSpace(string(paneOut))

	sessFile := filepath.Join(clcfg, "sessions", fmt.Sprintf("%d.json", agentPID))

	// Configure Go agent.PaneSender against the exact same isolated tmux server.
	uid := uint32(os.Geteuid())
	tmuxExec := agent.NewTmuxExecutorWithPath(home, tmuxBin, uid)
	tmuxExec.SetSocketForTest(socketName, tmuxTmpDir)
	val := agent.NewPathValidator(home, uid, "/proc")
	loc, err := agent.NewLocator(agent.LocatorOptions{
		Home: home,
		UID:  &uid,
		Tmux: tmuxExec,
	})
	if err != nil {
		t.Fatalf("NewLocator: %v", err)
	}
	sender := agent.NewPaneSender(loc, val, tmuxExec, nil)

	return &isolatedTmuxHarness{
		t:          t,
		home:       home,
		tmuxTmpDir: tmuxTmpDir,
		socketName: socketName,
		tmuxBin:    tmuxBin,
		wrapperDir: wrapperDir,
		session:    sessionName,
		paneID:     paneID,
		stateDir:   stateDir,
		agentPID:   agentPID,
		agentStart: agentStart,
		sessFile:   sessFile,
		sender:     sender,
		locator:    loc,
	}
}

func (h *isolatedTmuxHarness) setClaudeSession(sid, status, waitingFor string) {
	h.t.Helper()
	body := fmt.Sprintf(`{"pid":%d,"sessionId":%q,"procStart":"%d","status":%q,"waitingFor":%q}`,
		h.agentPID, sid, h.agentStart, status, waitingFor)
	if err := os.WriteFile(h.sessFile, []byte(body), 0o600); err != nil {
		h.t.Fatal(err)
	}
}

func (h *isolatedTmuxHarness) setScreen(screen, title string) {
	h.t.Helper()
	_ = os.Remove(filepath.Join(h.stateDir, "on_paste.screen"))
	_ = os.Remove(filepath.Join(h.stateDir, "on_paste.delay_ms"))
	_ = os.Remove(filepath.Join(h.stateDir, "on_paste.status_json"))
	_ = os.Remove(filepath.Join(h.stateDir, "on_esc.screen"))
	_ = os.Remove(filepath.Join(h.stateDir, "on_enter.screen"))
	_ = os.Remove(filepath.Join(h.stateDir, "on_enter2.screen"))
	_ = os.WriteFile(filepath.Join(h.stateDir, "screen.txt"), []byte(screen), 0o600)
	_ = os.WriteFile(filepath.Join(h.stateDir, "title.txt"), []byte(title), 0o600)
	// Wait for tmux pane to reflect updated frame & title.
	time.Sleep(60 * time.Millisecond)
}

func (h *isolatedTmuxHarness) capture(ansi bool) []byte {
	h.t.Helper()
	args := []string{"-L", h.socketName, "-u", "capture-pane", "-p"}
	if ansi {
		args = append(args, "-e")
	}
	args = append(args, "-t", h.paneID)
	cmd := exec.Command(h.tmuxBin, args...)
	cmd.Env = append(os.Environ(), "TMUX_TMPDIR="+h.tmuxTmpDir)
	out, err := cmd.Output()
	if err != nil {
		h.t.Fatalf("capture-pane: %v", err)
	}
	return out
}

func (h *isolatedTmuxHarness) resetInputLog() {
	_ = os.Remove(filepath.Join(h.stateDir, "input.log"))
}

func (h *isolatedTmuxHarness) readInputLog() []byte {
	time.Sleep(40 * time.Millisecond)
	b, _ := os.ReadFile(filepath.Join(h.stateDir, "input.log"))
	return b
}

func (h *isolatedTmuxHarness) runShell(gate, session, pane string, pid, start int64, sid, lenSpec string, keys []string, read int, text string) (int, []byte) {
	h.t.Helper()
	cmdStr := convoGated(gate, "tmux", session, pane, "", pid, start, sid, lenSpec, keys, read)
	c := exec.Command("sh", "-c", cmdStr)
	c.Env = append(os.Environ(),
		"HOME="+h.home,
		"PATH="+h.wrapperDir+":"+os.Getenv("PATH"),
		"XDG_RUNTIME_DIR="+h.home,
	)
	if lenSpec != "keys" {
		c.Stdin = strings.NewReader(text)
	}
	out, err := c.Output()
	if err == nil {
		return 0, out
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode(), out
	}
	h.t.Fatalf("shell exec unexpected error: %v", err)
	return -1, nil
}

func (h *isolatedTmuxHarness) runGo(gate, session, pane string, pid, start int64, sid, lenSpec string, keys []string, read int, text string) (int, []byte) {
	h.t.Helper()
	p := proto.PaneSendParams{
		OpID:    proto.NewOpID(time.Now()),
		Mode:    "tmux",
		Session: session,
		Pane:    pane,
		PID:     pid,
		Start:   start,
		SID:     sid,
		LenSpec: lenSpec,
		Keys:    keys,
		Read:    read,
		Gate:    gate,
	}
	if lenSpec != "keys" {
		p.Text = []byte(text)
	}
	res := h.sender.Send(context.Background(), p)
	return res.Exit, res.Out
}

func (h *isolatedTmuxHarness) assertParity(name string, setup func(), gate, session, pane string, pid, start int64, sid, lenSpec string, keys []string, read int, text string, wantExit int) {
	h.t.Helper()
	h.t.Run(name, func(t *testing.T) {
		// Run original shell script first.
		setup()
		h.resetInputLog()
		shExit, shOut := h.runShell(gate, session, pane, pid, start, sid, lenSpec, keys, read, text)
		shIn := h.readInputLog()

		// Reset and run Go implementation.
		setup()
		h.resetInputLog()
		goExit, goOut := h.runGo(gate, session, pane, pid, start, sid, lenSpec, keys, read, text)
		goIn := h.readInputLog()

		if shExit != wantExit || goExit != wantExit {
			t.Fatalf("exit mismatch: shell=%d go=%d want=%d (shOut=%q goOut=%q)", shExit, goExit, wantExit, shOut, goOut)
		}
		if !bytes.Equal(shOut, goOut) {
			t.Fatalf("stdout mismatch:\nshell: %q\ngo:    %q", shOut, goOut)
		}
		if !bytes.Equal(shIn, goIn) {
			t.Fatalf("pane input mismatch:\nshell: %q\ngo:    %q", shIn, goIn)
		}
	})
}

const (
	claudeMainIdleScreen = "● Previous reply\n\n" +
		"────────────────────────────────────────────────────────\n" +
		"❯ \n" +
		"────────────────────────────────────────────────────────\n" +
		"  ⏵⏵ auto mode on (shift+tab to cycle)\n"

	claudeMainPastedScreen = "● Previous reply\n\n" +
		"────────────────────────────────────────────────────────\n" +
		"❯ hello world\n" +
		"────────────────────────────────────────────────────────\n" +
		"  ⏵⏵ auto mode on (shift+tab to cycle)\n"

	claudeSubagentScreen = "────────────────────────────────────────────────────────\n" +
		"❯ Message @worker…\n" +
		"────────────────────────────────────────────────────────\n" +
		"  ◯ main\n" +
		"  ● worker  doing work\n"

	claudeListScreen = "────────────────────────────────────────────────────────\n" +
		"❯ \n" +
		"────────────────────────────────────────────────────────\n" +
		"  ❯ ● main\n" +
		"  ↑/↓ to select · Enter to switch\n"

	claudeNoBoxScreen = "Some output without any prompt box at all\n"

	claudeDialogScreen = "Select a model\n\n" +
		"  ❯ 1. Sonnet\n" +
		"    2. Opus\n\n" +
		"Enter to confirm · Esc to cancel\n"

	claudeQuestionScreen = "Do you want to run `ls`?\n\n" +
		"  ❯ 1. Yes\n" +
		"    2. No\n"

	claudeCardScreen = "✦ Heads up · Remember to run tests\n" +
		"  1: Learn more   2: Knew this already   0: Dismiss\n" +
		"────────────────────────────────────────────────────────\n" +
		"❯ \n" +
		"────────────────────────────────────────────────────────\n" +
		"  ⏵⏵ auto mode on\n"

	claudePaletteScreen = "  /compact   Compact conversation\n" +
		"  /clear     Clear conversation\n"
)

func TestConvoGatedTmuxParityClaude(t *testing.T) {
	sid := "11111111-1111-4111-8111-111111111111"
	h := startIsolatedTmuxAgent(t, "claude", "", nil, nil)

	// Normal send with paste->box update->Enter->box cleared transition.
	h.assertParity("claude_free_send", func() {
		h.setClaudeSession(sid, "idle", "")
		h.setScreen(claudeMainIdleScreen, "")
		_ = os.WriteFile(filepath.Join(h.stateDir, "on_paste.screen"), []byte(claudeMainPastedScreen), 0o600)
		_ = os.WriteFile(filepath.Join(h.stateDir, "on_enter.screen"), []byte(claudeMainIdleScreen), 0o600)
	}, "free", h.session, h.paneID, h.agentPID, h.agentStart, sid, "11", nil, 0, "hello world", 0)

	// Send with read > 0 (captures non-blank tail lines).
	h.assertParity("claude_nogate_send_read", func() {
		h.setClaudeSession(sid, "idle", "")
		h.setScreen(claudeMainIdleScreen, "")
		_ = os.WriteFile(filepath.Join(h.stateDir, "on_paste.screen"), []byte(claudeMainPastedScreen), 0o600)
		_ = os.WriteFile(filepath.Join(h.stateDir, "on_enter.screen"), []byte(claudeMainIdleScreen), 0o600)
	}, "", h.session, h.paneID, h.agentPID, h.agentStart, sid, "11", nil, 4, "hello world", 0)

	// Waiting -> exit 9.
	h.assertParity("claude_free_waiting_9", func() {
		h.setClaudeSession(sid, "waiting", "permission prompt")
		h.setScreen(claudeQuestionScreen, "")
	}, "free", h.session, h.paneID, h.agentPID, h.agentStart, sid, "2", nil, 0, "hi", 9)

	// Owner refusals: subagent (13), list (14), no box (15).
	h.assertParity("claude_free_subagent_13", func() {
		h.setClaudeSession(sid, "idle", "")
		h.setScreen(claudeSubagentScreen, "")
	}, "free", h.session, h.paneID, h.agentPID, h.agentStart, sid, "2", nil, 0, "hi", 13)

	h.assertParity("claude_free_list_14", func() {
		h.setClaudeSession(sid, "idle", "")
		h.setScreen(claudeListScreen, "")
	}, "free", h.session, h.paneID, h.agentPID, h.agentStart, sid, "2", nil, 0, "hi", 14)

	h.assertParity("claude_free_nobox_15", func() {
		h.setClaudeSession(sid, "idle", "")
		h.setScreen(claudeNoBoxScreen, "")
	}, "free", h.session, h.paneID, h.agentPID, h.agentStart, sid, "2", nil, 0, "hi", 15)

	// Stop gate: main ok (0), dialog without ◯ main ok (0), subagent ◯ main -> 13.
	h.assertParity("claude_stop_main_0", func() {
		h.setClaudeSession(sid, "busy", "")
		h.setScreen(claudeMainIdleScreen, "")
	}, "stop", h.session, h.paneID, h.agentPID, h.agentStart, sid, "keys", []string{"Escape"}, 0, "", 0)

	h.assertParity("claude_stop_nobox_0", func() {
		h.setClaudeSession(sid, "busy", "")
		h.setScreen(claudeNoBoxScreen, "")
	}, "stop", h.session, h.paneID, h.agentPID, h.agentStart, sid, "keys", []string{"Escape"}, 0, "", 0)

	h.assertParity("claude_stop_subagent_13", func() {
		h.setClaudeSession(sid, "busy", "")
		h.setScreen(claudeSubagentScreen, "")
	}, "stop", h.session, h.paneID, h.agentPID, h.agentStart, sid, "keys", []string{"Escape"}, 0, "", 13)

	// Idle gate: idle -> 0, busy -> 12.
	h.assertParity("claude_idle_ok_0", func() {
		h.setClaudeSession(sid, "idle", "")
		h.setScreen(claudeMainIdleScreen, "")
		_ = os.WriteFile(filepath.Join(h.stateDir, "on_paste.screen"), []byte(claudeMainPastedScreen), 0o600)
		_ = os.WriteFile(filepath.Join(h.stateDir, "on_enter.screen"), []byte(claudeMainIdleScreen), 0o600)
	}, "idle", h.session, h.paneID, h.agentPID, h.agentStart, sid, "11", nil, 0, "hello world", 0)

	h.assertParity("claude_idle_busy_12", func() {
		h.setClaudeSession(sid, "busy", "")
		h.setScreen(claudeMainIdleScreen, "")
	}, "idle", h.session, h.paneID, h.agentPID, h.agentStart, sid, "2", nil, 0, "hi", 12)

	// Dialog & panel:<digest> gates.
	h.setClaudeSession(sid, "waiting", "dialog open")
	h.setScreen(claudeDialogScreen, "")
	pDig := panelDigest(h.capture(true))
	h.assertParity("claude_dialog_open_0", func() {
		h.setClaudeSession(sid, "waiting", "dialog open")
		h.setScreen(claudeDialogScreen, "")
	}, "dialog", h.session, h.paneID, h.agentPID, h.agentStart, sid, "keys", []string{"Escape"}, 0, "", 0)

	h.assertParity("claude_dialog_perm_9", func() {
		h.setClaudeSession(sid, "waiting", "permission prompt")
		h.setScreen(claudeQuestionScreen, "")
	}, "dialog", h.session, h.paneID, h.agentPID, h.agentStart, sid, "keys", []string{"Escape"}, 0, "", 9)

	h.assertParity("claude_panel_match_0", func() {
		h.setClaudeSession(sid, "waiting", "dialog open")
		h.setScreen(claudeDialogScreen, "")
	}, "panel:"+pDig, h.session, h.paneID, h.agentPID, h.agentStart, sid, "keys", []string{"Enter"}, 0, "", 0)

	h.assertParity("claude_panel_changed_10", func() {
		h.setClaudeSession(sid, "waiting", "dialog open")
		h.setScreen(claudeDialogScreen, "")
	}, "panel:"+strings.Repeat("0", 64), h.session, h.paneID, h.agentPID, h.agentStart, sid, "keys", []string{"Enter"}, 0, "", 10)

	// Look & look:<digest>.
	h.setScreen(claudeDialogScreen, "")
	lDig := lookDigest(h.capture(true))
	h.assertParity("claude_look_match_0", func() {
		h.setClaudeSession(sid, "idle", "")
		h.setScreen(claudeDialogScreen, "")
	}, "look:"+lDig, h.session, h.paneID, h.agentPID, h.agentStart, sid, "keys", []string{"Down"}, 0, "", 0)

	h.assertParity("claude_look_changed_10", func() {
		h.setClaudeSession(sid, "idle", "")
		h.setScreen(claudeDialogScreen, "")
	}, "look:"+strings.Repeat("f", 64), h.session, h.paneID, h.agentPID, h.agentStart, sid, "keys", []string{"Down"}, 0, "", 10)

	// Reply:<digest>.
	h.setScreen(claudeQuestionScreen, "")
	_, qDig, _ := screenOf(h.capture(false))
	h.assertParity("claude_reply_match_0", func() {
		h.setClaudeSession(sid, "waiting", "permission prompt")
		h.setScreen(claudeQuestionScreen, "")
	}, "reply:"+qDig, h.session, h.paneID, h.agentPID, h.agentStart, sid, "keys", []string{"1"}, 0, "", 0)

	h.assertParity("claude_reply_changed_10", func() {
		h.setClaudeSession(sid, "waiting", "permission prompt")
		h.setScreen(claudeQuestionScreen, "")
	}, "reply:"+strings.Repeat("a", 64), h.session, h.paneID, h.agentPID, h.agentStart, sid, "keys", []string{"1"}, 0, "", 10)

	h.assertParity("claude_reply_not_waiting_9", func() {
		h.setClaudeSession(sid, "idle", "")
		h.setScreen(claudeQuestionScreen, "")
	}, "reply:"+qDig, h.session, h.paneID, h.agentPID, h.agentStart, sid, "keys", []string{"1"}, 0, "", 9)

	// Reply fill (length ending "-"): pastes only, no Enter, reads screen.
	h.assertParity("claude_reply_fill_0", func() {
		h.setClaudeSession(sid, "waiting", "input needed")
		h.setScreen(claudeQuestionScreen, "")
	}, "reply:"+qDig, h.session, h.paneID, h.agentPID, h.agentStart, sid, "6-", nil, 3, "mynote", 0)

	// Card:<digest>.
	h.setScreen(claudeCardScreen, "")
	card := cardOf(string(h.capture(true)))
	if card == nil || card.digest == "" {
		t.Fatalf("failed to parse test card from tmux capture")
	}
	h.assertParity("claude_card_match_0", func() {
		h.setClaudeSession(sid, "idle", "")
		h.setScreen(claudeCardScreen, "")
	}, "card:"+card.digest, h.session, h.paneID, h.agentPID, h.agentStart, sid, "keys", []string{"1"}, 0, "", 0)

	h.assertParity("claude_card_changed_10", func() {
		h.setClaudeSession(sid, "idle", "")
		h.setScreen(claudeCardScreen, "")
	}, "card:"+strings.Repeat("b", 64), h.session, h.paneID, h.agentPID, h.agentStart, sid, "keys", []string{"1"}, 0, "", 10)

	// Box:<digest>.
	h.setScreen(claudeMainPastedScreen, "")
	bDig := boxDigest(string(h.capture(false)))
	h.assertParity("claude_box_match_0", func() {
		h.setClaudeSession(sid, "idle", "")
		h.setScreen(claudeMainPastedScreen, "")
	}, "box:"+bDig, h.session, h.paneID, h.agentPID, h.agentStart, sid, "keys", []string{"Enter"}, 0, "", 0)

	h.assertParity("claude_box_changed_10", func() {
		h.setClaudeSession(sid, "idle", "")
		h.setScreen(claudeMainIdleScreen, "")
	}, "box:"+bDig, h.session, h.paneID, h.agentPID, h.agentStart, sid, "keys", []string{"Enter"}, 0, "", 10)

	// Slash command palette dismissal before Enter.
	h.assertParity("claude_slash_palette_escape", func() {
		h.setClaudeSession(sid, "idle", "")
		h.setScreen(claudeMainIdleScreen, "")
		_ = os.WriteFile(filepath.Join(h.stateDir, "on_paste.screen"), []byte(claudePaletteScreen), 0o600)
		_ = os.WriteFile(filepath.Join(h.stateDir, "on_esc.screen"), []byte(claudeMainPastedScreen), 0o600)
		_ = os.WriteFile(filepath.Join(h.stateDir, "on_enter.screen"), []byte(claudeMainIdleScreen), 0o600)
	}, "free", h.session, h.paneID, h.agentPID, h.agentStart, sid, "8", nil, 0, "/compact", 0)

	// Screen switches to subagent after paste -> exit 16 (typed, Enter held back).
	h.assertParity("claude_owner_changed_after_paste_16", func() {
		h.setClaudeSession(sid, "idle", "")
		h.setScreen(claudeMainIdleScreen, "")
		_ = os.WriteFile(filepath.Join(h.stateDir, "on_paste.screen"), []byte(claudeSubagentScreen), 0o600)
	}, "free", h.session, h.paneID, h.agentPID, h.agentStart, sid, "5", nil, 0, "hello", 16)

	// Picture path (@) two-step [Image #1] transition -> retries Enter and succeeds (0).
	picBox := strings.Replace(claudeMainPastedScreen, "hello world", "[Image #1] see this", 1)
	h.assertParity("claude_haspic_twostep_0", func() {
		h.setClaudeSession(sid, "idle", "")
		h.setScreen(claudeMainIdleScreen, "")
		_ = os.WriteFile(filepath.Join(h.stateDir, "on_paste.screen"), []byte(picBox), 0o600)
		_ = os.WriteFile(filepath.Join(h.stateDir, "on_enter.screen"), []byte(strings.Replace(picBox, "[Image #1]", "[Image #2]", 1)), 0o600)
		_ = os.WriteFile(filepath.Join(h.stateDir, "on_enter2.screen"), []byte(claudeMainIdleScreen), 0o600)
	}, "free", h.session, h.paneID, h.agentPID, h.agentStart, sid, "12@", nil, 0, "/tmp/a.png x", 0)

	// Keys with pause and empty keys read-only screen.
	h.assertParity("claude_keys_pause_0", func() {
		h.setClaudeSession(sid, "idle", "")
		h.setScreen(claudeMainIdleScreen, "")
	}, "enter", h.session, h.paneID, h.agentPID, h.agentStart, sid, "keys", []string{"BTab", "pause", "Enter"}, 0, "", 0)

	h.assertParity("claude_empty_keys_read_0", func() {
		h.setClaudeSession(sid, "idle", "")
		h.setScreen(claudeMainIdleScreen, "")
	}, "", h.session, h.paneID, h.agentPID, h.agentStart, sid, "keys", []string{}, 3, "", 0)

	// Process identity checks: PID reuse (wrong start), wrong sid, wrong tmux session, bad lenSpec, unknown gate.
	h.assertParity("pid_reuse_start_mismatch_3", func() {
		h.setClaudeSession(sid, "idle", "")
		h.setScreen(claudeMainIdleScreen, "")
	}, "free", h.session, h.paneID, h.agentPID, h.agentStart+1, sid, "2", nil, 0, "hi", 3)

	h.assertParity("sid_mismatch_3", func() {
		h.setClaudeSession(sid, "idle", "")
		h.setScreen(claudeMainIdleScreen, "")
	}, "free", h.session, h.paneID, h.agentPID, h.agentStart, "22222222-2222-4222-8222-222222222222", "2", nil, 0, "hi", 3)

	h.assertParity("tmux_session_mismatch_3", func() {
		h.setClaudeSession(sid, "idle", "")
		h.setScreen(claudeMainIdleScreen, "")
	}, "", "other_sess", h.paneID, h.agentPID, h.agentStart, sid, "2", nil, 0, "hi", 3)

	h.assertParity("length_mismatch_4", func() {
		h.setClaudeSession(sid, "idle", "")
		h.setScreen(claudeMainIdleScreen, "")
	}, "free", h.session, h.paneID, h.agentPID, h.agentStart, sid, "99", nil, 0, "hi", 4)

	h.assertParity("unknown_gate_4", func() {
		h.setClaudeSession(sid, "idle", "")
		h.setScreen(claudeMainIdleScreen, "")
	}, "bogus", h.session, h.paneID, h.agentPID, h.agentStart, sid, "2", nil, 0, "hi", 4)

	// Bare panel & look gates (read-only digest output on stdout) + unstable screen (exit 11) + taken timeout (exit 18).
	h.assertParity("claude_panel_bare_0", func() {
		h.setClaudeSession(sid, "waiting", "dialog open")
		h.setScreen(claudeDialogScreen, "")
	}, "panel", h.session, h.paneID, h.agentPID, h.agentStart, sid, "keys", []string{}, 0, "", 0)

	h.assertParity("claude_look_bare_0", func() {
		h.setClaudeSession(sid, "idle", "")
		h.setScreen(claudeDialogScreen, "")
	}, "look", h.session, h.paneID, h.agentPID, h.agentStart, sid, "keys", []string{}, 0, "", 0)

	h.assertParity("claude_screen_unstable_10", func() {
		h.setClaudeSession(sid, "waiting", "permission prompt")
		h.setScreen(claudeQuestionScreen, "")
		_ = os.WriteFile(filepath.Join(h.stateDir, "flicker"), []byte("1"), 0o600)
		t.Cleanup(func() { _ = os.Remove(filepath.Join(h.stateDir, "flicker")) })
	}, "reply:"+qDig, h.session, h.paneID, h.agentPID, h.agentStart, sid, "keys", []string{"1"}, 0, "", 10)
	_ = os.Remove(filepath.Join(h.stateDir, "flicker"))

	h.assertParity("claude_taken_timeout_18", func() {
		h.setClaudeSession(sid, "idle", "")
		h.setScreen(claudeMainIdleScreen, "")
		// Paste enters box, Enter leaves it stuck in box -> taken() times out -> exit 18.
		_ = os.WriteFile(filepath.Join(h.stateDir, "on_paste.screen"), []byte(claudeMainPastedScreen), 0o600)
	}, "free", h.session, h.paneID, h.agentPID, h.agentStart, sid, "11", nil, 0, "hello world", 18)

	// Per-pane lock contention -> exit 7.
	t.Run("pane_lock_contention_7", func(t *testing.T) {
		h.sender.SetLockTimeoutForTest(150 * time.Millisecond)
		t.Cleanup(func() { h.sender.SetLockTimeoutForTest(10 * time.Second) })
		h.setClaudeSession(sid, "idle", "")
		h.setScreen(claudeMainIdleScreen, "")

		// Hold the pane lock via a concurrent read=1 call (sleeps 700ms).
		done := make(chan int, 1)
		go func() {
			rc, _ := h.runGo("", h.session, h.paneID, h.agentPID, h.agentStart, sid, "keys", []string{}, 1, "")
			done <- rc
		}()
		time.Sleep(40 * time.Millisecond)
		rc2, _ := h.runGo("", h.session, h.paneID, h.agentPID, h.agentStart, sid, "keys", []string{}, 0, "")
		if rc2 != 7 {
			t.Fatalf("contended pane lock exit = %d, want 7", rc2)
		}
		if rc1 := <-done; rc1 != 0 {
			t.Fatalf("first lock holder exit = %d, want 0", rc1)
		}
	})
}

func TestConvoGatedTmuxParityCodexOpenCodeAgy(t *testing.T) {
	// 1. Codex: cfree, creply, cscreen, preply, parts (+), queue (~).
	codexSID := "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	codexHomeDir := t.TempDir()
	rolloutDir := filepath.Join(codexHomeDir, "sessions", "2026", "11", "27")
	if err := os.MkdirAll(rolloutDir, 0o755); err != nil {
		t.Fatal(err)
	}
	rolloutFile := filepath.Join(rolloutDir, "rollout-2026-11-27T10-00-00-"+codexSID+".jsonl")
	if err := os.WriteFile(rolloutFile, []byte(`{"type":"session_meta","payload":{"id":"`+codexSID+`"}}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cx := startIsolatedTmuxAgent(t, "codex", "", []string{"CODEX_HOME=" + codexHomeDir}, []string{rolloutFile})

	cx.assertParity("codex_cfree_normal_0", func() {
		cx.setScreen("› \n", "codex | proj")
	}, "cfree", cx.session, cx.paneID, cx.agentPID, cx.agentStart, codexSID, "5", nil, 0, "hello", 0)

	cx.assertParity("codex_cfree_waiting_9", func() {
		cx.setScreen("Approve?\n1. Yes\n", "[ ! ] Action Required | proj")
	}, "cfree", cx.session, cx.paneID, cx.agentPID, cx.agentStart, codexSID, "5", nil, 0, "hello", 9)

	cx.assertParity("codex_parts_paste_0", func() {
		cx.setScreen("› \n", "codex | proj")
	}, "cfree", cx.session, cx.paneID, cx.agentPID, cx.agentStart, codexSID, "3+4+3", nil, 0, "aaabbbbccc", 0)

	cx.assertParity("codex_parts_bad_4", func() {
		cx.setScreen("› \n", "codex | proj")
	}, "cfree", cx.session, cx.paneID, cx.agentPID, cx.agentStart, codexSID, "0+3", nil, 0, "abc", 4)

	cx.assertParity("codex_queue_tab_0", func() {
		cx.setScreen("› \n", "codex | proj")
	}, "cfree", cx.session, cx.paneID, cx.agentPID, cx.agentStart, codexSID, "5~", nil, 0, "later", 0)

	cx.assertParity("codex_parts_queue_tab_0", func() {
		cx.setScreen("› \n", "codex | proj")
	}, "cfree", cx.session, cx.paneID, cx.agentPID, cx.agentStart, codexSID, "2+3~", nil, 0, "ab123", 0)

	cx.setScreen("Approve tool?\n1. Yes\n2. No\n", "[ . ] Action Required | proj")
	_, cxDig, _ := screenOf(cx.capture(false))
	cx.assertParity("codex_creply_match_0", func() {
		cx.setScreen("Approve tool?\n1. Yes\n2. No\n", "[ . ] Action Required | proj")
	}, "creply:"+cxDig, cx.session, cx.paneID, cx.agentPID, cx.agentStart, codexSID, "keys", []string{"1"}, 0, "", 0)

	cx.assertParity("codex_creply_not_waiting_9", func() {
		cx.setScreen("Approve tool?\n1. Yes\n2. No\n", "codex | proj")
	}, "creply:"+cxDig, cx.session, cx.paneID, cx.agentPID, cx.agentStart, codexSID, "keys", []string{"1"}, 0, "", 9)

	cx.assertParity("codex_preply_match_0", func() {
		cx.setScreen("Approve tool?\n1. Yes\n2. No\n", "codex | proj")
	}, "preply:"+cxDig, cx.session, cx.paneID, cx.agentPID, cx.agentStart, codexSID, "keys", []string{"1"}, 0, "", 0)

	cx.assertParity("codex_cscreen_match_0", func() {
		cx.setScreen("Approve tool?\n1. Yes\n2. No\n", "codex | proj")
	}, "cscreen:"+cxDig, cx.session, cx.paneID, cx.agentPID, cx.agentStart, codexSID, "keys", []string{"Escape"}, 0, "", 0)

	cx.assertParity("codex_cscreen_changed_10", func() {
		cx.setScreen("Approve tool?\n1. Yes\n2. No\n", "codex | proj")
	}, "cscreen:"+strings.Repeat("1", 64), cx.session, cx.paneID, cx.agentPID, cx.agentStart, codexSID, "keys", []string{"Escape"}, 0, "", 10)

	// 2. OpenCode: ofree:<digest> (match -> 0, draft -> 17, missing box -> 9).
	oc := startIsolatedTmuxAgent(t, "opencode", "", nil, nil)
	ocEmpty := "  ┃ Ask anything...\n  ┃ Build  GPT-5\n  ╹▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀\n"
	ocDraft := "  ┃ draft words\n  ┃ Build  GPT-5\n  ╹▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀\n"
	oc.setScreen(ocEmpty, "")
	ocDig, ok := ocBoxDigest(oc.capture(false))
	if !ok {
		t.Fatal("failed to compute ocBoxDigest")
	}
	oc.assertParity("opencode_ofree_match_0", func() {
		oc.setScreen(ocEmpty, "")
	}, "ofree:"+ocDig, oc.session, oc.paneID, oc.agentPID, oc.agentStart, "00000000-0000-0000-0000-000000000000", "4", nil, 0, "ping", 0)

	oc.assertParity("opencode_ofree_draft_17", func() {
		oc.setScreen(ocDraft, "")
	}, "ofree:"+ocDig, oc.session, oc.paneID, oc.agentPID, oc.agentStart, "00000000-0000-0000-0000-000000000000", "4", nil, 0, "ping", 17)

	oc.assertParity("opencode_ofree_waiting_9", func() {
		oc.setScreen("Permission required?\n", "")
	}, "ofree:"+ocDig, oc.session, oc.paneID, oc.agentPID, oc.agentStart, "00000000-0000-0000-0000-000000000000", "4", nil, 0, "ping", 9)

	// 3. Antigravity CLI (agy): afree:<digest>, immediate (!), amode, areply:<digest>.
	agySID := "33333333-3333-4333-8333-333333333333"
	agyBase := t.TempDir()
	presDir := filepath.Join(agyBase, "presence")
	if err := os.MkdirAll(presDir, 0o755); err != nil {
		t.Fatal(err)
	}
	presLock := filepath.Join(presDir, agySID+".lock")
	if err := os.WriteFile(presLock, []byte("1"), 0o600); err != nil {
		t.Fatal(err)
	}
	ag := startIsolatedTmuxAgent(t, "agy", "", nil, []string{presLock})
	agyRule := strings.Repeat("─", 40)
	agyEmpty := agyRule + "\n> Ask anything...\n" + agyRule + "\n  auto · Gemini\n"
	agyDraft := agyRule + "\n> unsent text\n" + agyRule + "\n  auto · Gemini\n"
	agyAsk := "Allow command?\n1. Allow\n2. Deny\n"

	ag.setScreen(agyEmpty, "")
	agyDig, ok := agyBoxDigest(ag.capture(false))
	if !ok {
		t.Fatal("failed to compute agyBoxDigest")
	}
	ag.assertParity("agy_afree_immediate_0", func() {
		ag.setScreen(agyEmpty, "")
	}, "afree:"+agyDig, ag.session, ag.paneID, ag.agentPID, ag.agentStart, agySID, "3!", nil, 0, "now", 0)

	ag.assertParity("agy_afree_draft_17", func() {
		ag.setScreen(agyDraft, "")
	}, "afree:"+agyDig, ag.session, ag.paneID, ag.agentPID, ag.agentStart, agySID, "3", nil, 0, "now", 17)

	ag.assertParity("agy_amode_present_0", func() {
		ag.setScreen(agyEmpty, "")
	}, "amode", ag.session, ag.paneID, ag.agentPID, ag.agentStart, agySID, "keys", []string{"BTab"}, 0, "", 0)

	ag.assertParity("agy_amode_absent_9", func() {
		ag.setScreen(agyAsk, "")
	}, "amode", ag.session, ag.paneID, ag.agentPID, ag.agentStart, agySID, "keys", []string{"BTab"}, 0, "", 9)

	ag.setScreen(agyAsk, "")
	_, agAskDig, _ := screenOf(ag.capture(false))
	ag.assertParity("agy_areply_match_0", func() {
		ag.setScreen(agyAsk, "")
	}, "areply:"+agAskDig, ag.session, ag.paneID, ag.agentPID, ag.agentStart, agySID, "keys", []string{"1"}, 0, "", 0)

	ag.assertParity("agy_areply_box_present_9", func() {
		ag.setScreen(agyEmpty, "")
	}, "areply:"+agAskDig, ag.session, ag.paneID, ag.agentPID, ag.agentStart, agySID, "keys", []string{"1"}, 0, "", 9)
}

type recordedHerdrRPC struct {
	Method string
	Params map[string]any
}

// TestConvoGatedHerdrParity runs both the original convoGated shell script
// (through a thin CLI-to-JSON-RPC shim talking to a fake herdr Unix socket
// server) and Go agent.PaneSender (talking directly to the same Unix socket)
// and verifies identical exit codes, stdout bytes, and recorded RPC sequences.
func TestConvoGatedHerdrParity(t *testing.T) {
	for _, p := range []string{"sh", "python3", "awk", "sha256sum"} {
		if _, err := exec.LookPath(p); err != nil {
			t.Skip("missing " + p)
		}
	}

	home := t.TempDir()
	sockDir, err := os.MkdirTemp("", "corral-hdp-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(sockDir) })
	sockPath := filepath.Join(sockDir, "h.sock")

	var mu sync.Mutex
	var recorded []recordedHerdrRPC
	screen := ""
	nextAfterPaste := ""
	nextAfterEnter := ""
	title := ""
	rejectAltM := false
	failPaneGet := false

	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				defer conn.Close()
				sc := bufio.NewScanner(conn)
				if !sc.Scan() {
					return
				}
				var req struct {
					ID     string         `json:"id"`
					Method string         `json:"method"`
					Params map[string]any `json:"params"`
				}
				if json.Unmarshal(sc.Bytes(), &req) != nil {
					return
				}
				mu.Lock()
				recorded = append(recorded, recordedHerdrRPC{Method: req.Method, Params: req.Params})
				var resp map[string]any
				switch req.Method {
				case "pane.read":
					resp = map[string]any{"id": req.ID, "result": map[string]any{"read": map[string]any{"text": screen}}}
				case "pane.get":
					if failPaneGet {
						resp = map[string]any{"id": req.ID, "error": map[string]any{"code": "not_found", "message": "pane unavailable"}}
					} else {
						resp = map[string]any{"id": req.ID, "result": map[string]any{"pane": map[string]any{"pane_id": req.Params["pane_id"], "terminal_title": title}}}
					}
				case "pane.send_text":
					if nextAfterPaste != "" {
						screen = nextAfterPaste
						nextAfterPaste = ""
					}
					resp = map[string]any{"id": req.ID, "result": map[string]any{"ok": true}}
				case "pane.send_keys":
					keys, _ := req.Params["keys"].([]any)
					if rejectAltM && len(keys) == 1 && keys[0] == "alt+m" {
						resp = map[string]any{"id": req.ID, "error": map[string]any{"code": "invalid_key", "message": "alt+m unsupported"}}
					} else {
						if len(keys) == 1 && keys[0] == "enter" && nextAfterEnter != "" {
							screen = nextAfterEnter
							nextAfterEnter = ""
						}
						resp = map[string]any{"id": req.ID, "result": map[string]any{"ok": true}}
					}
				default:
					resp = map[string]any{"id": req.ID, "result": map[string]any{"ok": true}}
				}
				mu.Unlock()
				b, _ := json.Marshal(resp)
				_, _ = conn.Write(append(b, '\n'))
			}(c)
		}
	}()

	// Write a tiny python3 `herdr` CLI shim in PATH that translates the exact
	// `herdr pane ...` invocations used by convoGated into the exact same Unix
	// socket JSON-RPC messages.
	shimDir := filepath.Join(home, "herdr-shim")
	if err := os.MkdirAll(shimDir, 0o755); err != nil {
		t.Fatal(err)
	}
	herdrShim := `#!/usr/bin/env python3
import json, os, socket, sys
sock = os.environ["HERDR_SOCKET_PATH"]
args = sys.argv[1:]
assert args[0] == "pane"
sub = args[1]
pane = args[2]
if sub == "read":
    params = {"pane_id": pane, "source": "visible"}
    fmt_ansi = False
    i = 3
    while i < len(args):
        if args[i] == "--source":
            params["source"] = args[i+1]; i += 2
        elif args[i] == "--lines":
            params["lines"] = int(args[i+1]); i += 2
        elif args[i] == "--format" and args[i+1] == "ansi":
            params["format"] = "ansi"; fmt_ansi = True; i += 2
        else:
            i += 1
    if not fmt_ansi:
        params["strip_ansi"] = True
    req = {"id": "1", "method": "pane.read", "params": params}
elif sub == "get":
    req = {"id": "1", "method": "pane.get", "params": {"pane_id": pane}}
elif sub == "send-text":
    req = {"id": "1", "method": "pane.send_text", "params": {"pane_id": pane, "text": args[3]}}
elif sub == "send-keys":
    req = {"id": "1", "method": "pane.send_keys", "params": {"pane_id": pane, "keys": args[3:]}}
else:
    sys.exit(2)
s = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
s.connect(sock)
s.sendall((json.dumps(req) + "\n").encode())
data = b""
while b"\n" not in data:
    chunk = s.recv(65536)
    if not chunk: break
    data += chunk
s.close()
resp = json.loads(data.decode())
if resp.get("error"):
    sys.exit(1)
if sub == "read":
    sys.stdout.write(resp["result"]["read"]["text"])
elif sub == "get":
    sys.stdout.write(json.dumps(resp))
`
	if err := os.WriteFile(filepath.Join(shimDir, "herdr"), []byte(herdrShim), 0o755); err != nil {
		t.Fatal(err)
	}

	// Start a foreground pseudo-terminal child with HERDR_PANE_ID & HERDR_SOCKET_PATH.
	paneID := "w1:p1"
	sid := "44444444-4444-4444-8444-444444444444"
	helper := exec.Command("python3", "-c", fmt.Sprintf(`import pty, os, time
pid, fd = pty.fork()
if pid == 0:
    os.environ["HERDR_PANE_ID"] = %q
    os.environ["HERDR_SOCKET_PATH"] = %q
    os.execvp("sleep", ["sleep", "60"])
print(pid, flush=True)
time.sleep(60)`, paneID, sockPath))
	outPipe, err := helper.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := helper.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = helper.Process.Kill()
		_, _ = helper.Process.Wait()
	})
	var pid int64
	if _, err := fmt.Fscan(outPipe, &pid); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if p, err := os.FindProcess(int(pid)); err == nil {
			_ = p.Kill()
		}
	})
	time.Sleep(100 * time.Millisecond)
	startStr := waitForProcStart(t, int(pid))
	start, err := strconv.ParseInt(startStr, 10, 64)
	if err != nil {
		t.Fatal(err)
	}

	if err := os.MkdirAll(filepath.Join(home, ".claude", "sessions"), 0o755); err != nil {
		t.Fatal(err)
	}
	sessFile := filepath.Join(home, ".claude", "sessions", fmt.Sprintf("%d.json", pid))
	setSess := func(status, wf string) {
		body := fmt.Sprintf(`{"pid":%d,"sessionId":%q,"procStart":"%d","status":%q,"waitingFor":%q}`, pid, sid, start, status, wf)
		if err := os.WriteFile(sessFile, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	uid := uint32(os.Geteuid())
	val := agent.NewPathValidator(home, uid, "/proc")
	loc, err := agent.NewLocator(agent.LocatorOptions{
		Home: home,
		UID:  &uid,
		OnHerdrSockets: func(scope string, sockets []string) {
			val.UpdateHerdrSockets(scope, sockets)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	// Discover herdr socket via real Locate so HerdrExecutor accepts sockPath!
	setSess("idle", "")
	_ = loc.Locate(context.Background(), proto.AgentLocateParams{Mode: "herdr"})
	if !val.HasHerdrSocket(sockPath) {
		t.Fatalf("Locator did not discover herdr socket %q", sockPath)
	}

	herdrExec := agent.NewHerdrExecutorForTest(uid, "/proc", val)
	sender := agent.NewPaneSender(loc, val, agent.NewTmuxExecutor(home), herdrExec)

	runPair := func(name string, setup func(), gate, lenSpec string, keys []string, read int, text string, wantExit int) {
		t.Run(name, func(t *testing.T) {
			// 1. Shell script run.
			setup()
			mu.Lock()
			recorded = nil
			mu.Unlock()
			cmdStr := convoGated(gate, "herdr", "", paneID, sockPath, pid, start, sid, lenSpec, keys, read)
			c := exec.Command("sh", "-c", cmdStr)
			c.Env = append(os.Environ(), "HOME="+home, "PATH="+shimDir+":"+os.Getenv("PATH"), "XDG_RUNTIME_DIR="+home)
			if lenSpec != "keys" {
				c.Stdin = strings.NewReader(text)
			}
			shOut, shErr := c.Output()
			shExit := 0
			if shErr != nil {
				var ee *exec.ExitError
				if errors.As(shErr, &ee) {
					shExit = ee.ExitCode()
				} else {
					t.Fatalf("shell err: %v", shErr)
				}
			}
			mu.Lock()
			shReqs := append([]recordedHerdrRPC(nil), recorded...)
			mu.Unlock()

			// 2. Go PaneSender run.
			setup()
			mu.Lock()
			recorded = nil
			mu.Unlock()
			p := proto.PaneSendParams{
				OpID:    proto.NewOpID(time.Now()),
				Mode:    "herdr",
				Pane:    paneID,
				Socket:  sockPath,
				PID:     pid,
				Start:   start,
				SID:     sid,
				LenSpec: lenSpec,
				Keys:    keys,
				Read:    read,
				Gate:    gate,
			}
			if lenSpec != "keys" {
				p.Text = []byte(text)
			}
			goRes := sender.Send(context.Background(), p)
			mu.Lock()
			goReqs := append([]recordedHerdrRPC(nil), recorded...)
			mu.Unlock()

			if shExit != wantExit || goRes.Exit != wantExit {
				t.Fatalf("exit mismatch: sh=%d go=%d want=%d", shExit, goRes.Exit, wantExit)
			}
			if !bytes.Equal(shOut, goRes.Out) {
				t.Fatalf("stdout mismatch: sh=%q go=%q", shOut, goRes.Out)
			}
			if !reflect.DeepEqual(shReqs, goReqs) {
				t.Fatalf("herdr RPC sequence mismatch:\nshell: %+v\ngo:    %+v", shReqs, goReqs)
			}
		})
	}

	runPair("herdr_send_free", func() {
		setSess("idle", "")
		mu.Lock()
		screen = claudeMainIdleScreen
		nextAfterPaste = claudeMainPastedScreen
		nextAfterEnter = claudeMainIdleScreen
		rejectAltM = false
		mu.Unlock()
	}, "free", "11", nil, 0, "hello world", 0)

	runPair("herdr_keys_shifttab_and_pause", func() {
		setSess("idle", "")
		mu.Lock()
		screen = claudeMainIdleScreen
		mu.Unlock()
	}, "free", "keys", []string{"shift+tab", "pause", "enter"}, 3, "", 0)

	runPair("herdr_parts_and_queue", func() {
		setSess("idle", "")
		mu.Lock()
		screen = claudeMainIdleScreen
		mu.Unlock()
	}, "", "2+3~", nil, 0, "ab123", 0)

	runPair("herdr_immediate_altm_fallback", func() {
		setSess("idle", "")
		mu.Lock()
		screen = claudeMainIdleScreen
		nextAfterPaste = ""
		nextAfterEnter = ""
		rejectAltM = true
		mu.Unlock()
	}, "", "3!", nil, 0, "now", 0)

	_, qDig, _ := screenOf([]byte(claudeQuestionScreen))
	runPair("herdr_reply_match", func() {
		setSess("waiting", "permission prompt")
		mu.Lock()
		screen = claudeQuestionScreen
		mu.Unlock()
	}, "reply:"+qDig, "keys", []string{"1"}, 0, "", 0)

	cDig := cardOf(claudeCardScreen).digest
	runPair("herdr_card_match", func() {
		setSess("idle", "")
		mu.Lock()
		screen = claudeCardScreen
		mu.Unlock()
	}, "card:"+cDig, "keys", []string{"1"}, 0, "", 0)

	runPair("herdr_codex_cfree_waiting_9", func() {
		setSess("idle", "")
		mu.Lock()
		title = "[ ! ] Action Required | perm"
		mu.Unlock()
	}, "cfree", "2", nil, 0, "hi", 9)

	runPair("herdr_codex_cfree_normal_0", func() {
		setSess("idle", "")
		mu.Lock()
		title = "codex | proj"
		failPaneGet = false
		mu.Unlock()
	}, "cfree", "2", nil, 0, "hi", 0)

	runPair("herdr_codex_title_unread_11", func() {
		setSess("idle", "")
		mu.Lock()
		failPaneGet = true
		mu.Unlock()
	}, "cfree", "2", nil, 0, "hi", 11)
}

// TestCorraldGatedLatencyComparison measures end-to-end latency on a real
// isolated tmux server + real SSH server + real corrald daemon over SSH
// direct-streamlocal for Claude Code normal send (`free`) and answer card
// (`card:<digest>`).
func TestCorraldGatedLatencyComparison(t *testing.T) {
	sid := "55555555-5555-4555-8555-555555555555"
	h := startIsolatedTmuxAgent(t, "claude", "", nil, nil)

	// Start a real corrald daemon + agent Service bound to the same isolated tmux server.
	sockDir, err := os.MkdirTemp("/tmp", "corrald-bench-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(sockDir) })
	uid := uint32(os.Geteuid())
	layout, err := daemon.ResolveLayout(daemon.PathOptions{
		Home:      h.home,
		MachineID: "aabbccddeeff",
		TmpBase:   sockDir,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := layout.EnsureStateDir(); err != nil {
		t.Fatal(err)
	}

	dSrv, err := daemon.NewServer(daemon.ServerConfig{
		Layout:      layout,
		Build:       proto.BuildInfo{Version: "bench", Release: 2000},
		Gateway:     "gw-bench",
		IdleTimeout: 5 * time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	tmuxExec := agent.NewTmuxExecutorWithPath(h.home, h.tmuxBin, uid)
	tmuxExec.SetSocketForTest(h.socketName, h.tmuxTmpDir)
	if _, err := agent.Register(dSrv, agent.ServiceOptions{
		LocatorOpts: agent.LocatorOptions{
			Home: h.home,
			UID:  &uid,
			Tmux: tmuxExec,
		},
	}); err != nil {
		t.Fatal(err)
	}

	dCtx, dCancel := context.WithCancel(context.Background())
	dDone := make(chan error, 1)
	go func() { dDone <- dSrv.Serve(dCtx) }()
	t.Cleanup(func() {
		dCancel()
		<-dDone
	})
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
		if c, derr := net.Dial("unix", layout.SocketPath); derr == nil {
			_ = c.Close()
			break
		}
		time.Sleep(15 * time.Millisecond)
	}

	// Start real SSH server running real shell scripts (with PATH containing our
	// isolated tmux shim) AND forwarding direct-streamlocal to corrald.sock.
	sshSrv := testutil.StartSSHServer(t, "alice", "pass", h.home)
	sshSrv.HandleExec(func(cmd string) func(ssh.Channel) {
		return func(ch ssh.Channel) {
			defer ch.Close()
			c := exec.Command("sh", "-c", cmd)
			c.Env = append(os.Environ(),
				"HOME="+h.home,
				"PATH="+h.wrapperDir+":"+os.Getenv("PATH"),
				"XDG_RUNTIME_DIR="+h.home,
			)
			c.Stdin = ch
			c.Stdout = ch
			c.Stderr = ch.Stderr()
			rc := uint32(0)
			if err := c.Run(); err != nil {
				var ee *exec.ExitError
				if errors.As(err, &ee) {
					rc = uint32(ee.ExitCode())
				} else {
					rc = 1
				}
			}
			_, _ = ch.SendRequest("exit-status", false, ssh.Marshal(&struct{ Status uint32 }{rc}))
		}
	})
	sshSrv.HandleChannel(func(nc ssh.NewChannel) bool {
		if nc.ChannelType() != "direct-streamlocal@openssh.com" {
			return false
		}
		var payload struct {
			SocketPath string
			Reserved0  string
			Reserved1  uint32
		}
		if err := ssh.Unmarshal(nc.ExtraData(), &payload); err != nil {
			_ = nc.Reject(ssh.ConnectionFailed, "bad")
			return true
		}
		uconn, err := net.Dial("unix", payload.SocketPath)
		if err != nil {
			_ = nc.Reject(ssh.ConnectionFailed, err.Error())
			return true
		}
		ch, reqs, err := nc.Accept()
		if err != nil {
			_ = uconn.Close()
			return true
		}
		go ssh.DiscardRequests(reqs)
		go func() {
			defer ch.Close()
			defer uconn.Close()
			buf := make([]byte, 32<<10)
			for {
				n, rerr := ch.Read(buf)
				if n > 0 {
					if _, werr := uconn.Write(buf[:n]); werr != nil {
						return
					}
				}
				if rerr != nil {
					return
				}
			}
		}()
		go func() {
			defer ch.Close()
			defer uconn.Close()
			buf := make([]byte, 32<<10)
			for {
				n, rerr := uconn.Read(buf)
				if n > 0 {
					if _, werr := ch.Write(buf[:n]); werr != nil {
						return
					}
				}
				if rerr != nil {
					return
				}
			}
		}()
		return true
	})

	sshClient, err := ssh.Dial("tcp", sshSrv.Addr.String(), &ssh.ClientConfig{
		User: "alice", Auth: []ssh.AuthMethod{ssh.Password("pass")},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(), Timeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sshClient.Close() })

	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()
	u, _ := st.CreateUser(ctx, "alice", "Alice", "hash", "admin")
	hostRec, _ := st.CreateHost(ctx, &store.Host{OwnerID: u.ID, Name: "box", Address: "127.0.0.1", Port: 22, Username: "alice", Auth: "password"})
	mb, _ := proto.MarshalManifest(proto.DefaultManifest())
	_ = st.UpsertCorraldHost(ctx, &store.CorraldHost{
		HostID: hostRec.ID, State: gw.StateRunning, BoundFingerprint: "fp-b",
		BoundUsername: "alice", BoundGateway: "gw-bench", ManifestJSON: string(mb),
		Version: "bench", Release: 2000,
	})
	_ = st.RecordSSHVerified(ctx, hostRec.ID, "fp-b", "alice")

	mgr := gw.NewManager(gw.ManagerConfig{
		Store:     st,
		SSH:       &testGatedSSHProvider{client: sshClient, fp: "fp-b", user: "alice"},
		Dist:      &gw.Dist{Version: "bench", Release: 2000, Manifest: proto.DefaultManifest(), ManifestHash: "mh"},
		GatewayID: "gw-bench", GatewayVersion: "bench",
		ProbeOverride: func(ctx context.Context, c *ssh.Client) (*gw.HostProbe, error) {
			return &gw.HostProbe{
				OS: "Linux", Arch: runtime.GOARCH, MachineID: "aabbccddeeff", Home: h.home, UID: strconv.Itoa(int(uid)),
				HomeWritable: true, LibDir: sockDir, StateDir: sockDir, SocketPath: layout.SocketPath,
				HasSymlink: true, HasInstall: true,
			}, nil
		},
	})
	t.Cleanup(func() { mgr.Close() })
	mgr.OnHostConnected(ctx, u.ID, hostRec.ID, "alice", "fp-b", "alice", sshClient)

	sess := newTestTermSession(t, u.ID, hostRec.ID, "tmux", sshClient)
	sess.TmuxSession = h.session

	srvCorrald := &Server{store: st, corrald: mgr, perf: perf.New(perf.Options{})}
	srvSSH := &Server{store: st, corrald: nil, perf: perf.New(perf.Options{})}

	const iters = 5
	measure := func(label string, srv *Server, setup func(), req gatedReq) (avgMs, p50Ms float64) {
		durs := make([]time.Duration, 0, iters)
		for i := 0; i < iters; i++ {
			setup()
			t0 := time.Now()
			if _, err := srv.runGated(ctx, sess, req); err != nil {
				t.Fatalf("%s iter %d failed: %v", label, i, err)
			}
			durs = append(durs, time.Since(t0))
		}
		var sum time.Duration
		for _, d := range durs {
			sum += d
		}
		slices.Sort(durs)
		avg := float64(sum/time.Duration(iters)) / float64(time.Millisecond)
		p50 := float64(durs[iters/2]) / float64(time.Millisecond)
		t.Logf("%-28s avg=%.2fms p50=%.2fms samples=%v", label, avg, p50, durs)
		return avg, p50
	}

	sendSetup := func() {
		h.setClaudeSession(sid, "idle", "")
		h.setScreen(claudeMainIdleScreen, "")
		_ = os.WriteFile(filepath.Join(h.stateDir, "on_paste.screen"), []byte(claudeMainPastedScreen), 0o600)
		_ = os.WriteFile(filepath.Join(h.stateDir, "on_enter.screen"), []byte(claudeMainIdleScreen), 0o600)
	}
	sendReq := gatedReq{
		Gate: "free", Mode: "tmux", Session: h.session, Pane: h.paneID,
		PID: h.agentPID, Start: h.agentStart, SID: sid, Text: "hello world", What: "send",
	}
	measure("claude_send (corrald)", srvCorrald, sendSetup, sendReq)
	measure("claude_send (ssh)", srvSSH, sendSetup, sendReq)

	h.setScreen(claudeCardScreen, "")
	card := cardOf(string(h.capture(true)))
	cardSetup := func() {
		h.setClaudeSession(sid, "idle", "")
		h.setScreen(claudeCardScreen, "")
	}
	cardReq := gatedReq{
		Gate: "card:" + card.digest, Mode: "tmux", Session: h.session, Pane: h.paneID,
		PID: h.agentPID, Start: h.agentStart, SID: sid, Keys: []string{"1"}, What: "card",
	}
	measure("claude_card (corrald)", srvCorrald, cardSetup, cardReq)
	measure("claude_card (ssh)", srvSSH, cardSetup, cardReq)

	// Confirm perf tracker distinguishes buckets.
	snapC := srvCorrald.perf.Snapshot()
	snapS := srvSSH.perf.Snapshot()
	if snapC.Ops["gated.send"].ByBucket["corrald"].Count != iters || snapC.Ops["gated.card"].ByBucket["corrald"].Count != iters {
		t.Fatalf("unexpected corrald perf counts: %+v", snapC.Ops)
	}
	if snapS.Ops["gated.send"].ByBucket["ssh"].Count != iters || snapS.Ops["gated.card"].ByBucket["ssh"].Count != iters {
		t.Fatalf("unexpected ssh perf counts: %+v", snapS.Ops)
	}
}
