//go:build linux

package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"time"

	"golang.org/x/sys/unix"

	"github.com/ihainan/corral/internal/corrald/daemon"
	"github.com/ihainan/corral/internal/corrald/proto"
)

const (
	defaultTmuxTimeout  = 5 * time.Second
	maxTmuxOutputBytes  = 4096
	maxTmuxCaptureBytes = 512 << 10

	// Hard-coded read-only tmux format strings. Callers cannot pass arbitrary
	// formats (which prevents tmux `#(command)` shell expansion).
	tmuxFormatSessionPane = "#{pane_id} #{pid} #{pane_tty} #{pane_title}"
	tmuxFormatPaneTTY     = "#{pane_tty}"
	tmuxFormatPaneTitle   = "#{pane_title}"
	tmuxFormatSessionName = "#{session_name}"
)

var (
	tmuxSessionNameRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	tmuxPaneIDRe      = regexp.MustCompile(`^%[0-9]{1,9}$`)
	tmuxSocketNameRe  = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)
	tmuxBufNameRe     = regexp.MustCompile(`^corral-send-[0-9]{1,10}-[0-9]{1,12}$`)
	tmuxSingleKeyRe   = regexp.MustCompile(`^[A-Za-z0-9._/]$`)

	allowedTmuxNamedKeys = map[string]bool{
		"Enter":  true,
		"Escape": true,
		"Tab":    true,
		"BTab":   true,
		"Space":  true,
		"BSpace": true,
		"Up":     true,
		"Down":   true,
		"Left":   true,
		"Right":  true,
		"C-x":    true,
		"C-s":    true,
		"C-c":    true,
		"M-m":    true,
	}
)

func isAllowedTmuxKey(key string) bool {
	return allowedTmuxNamedKeys[key] || tmuxSingleKeyRe.MatchString(key)
}

// TmuxExecutor is the single allowlisted external command runner permitted in
// host-side corrald production code. It executes only a validated `tmux`
// binary directly (never via a shell), with arguments restricted strictly to
// fixed allowlisted subcommands and validated session/pane/key/buffer arguments.
type TmuxExecutor struct {
	home       string
	uid        uint32
	binaryPath string // optional validated absolute path (e.g. from --tmux)
	socketName string // optional -L <name> for isolated test servers
	tmuxTmpDir string // optional TMUX_TMPDIR for isolated test servers
	timeout    time.Duration
}

// NewTmuxExecutor constructs a TmuxExecutor searching standard fixed host paths
// under home without reading $PATH.
func NewTmuxExecutor(home string) *TmuxExecutor {
	return &TmuxExecutor{
		home:    home,
		uid:     uint32(os.Geteuid()),
		timeout: defaultTmuxTimeout,
	}
}

// NewTmuxExecutorWithPath constructs a TmuxExecutor preferring binaryPath (if
// non-empty and valid under daemon.ValidateTmuxBinary) and falling back to
// fixed standard directories otherwise.
func NewTmuxExecutorWithPath(home, binaryPath string, uid uint32) *TmuxExecutor {
	return &TmuxExecutor{
		home:       home,
		uid:        uid,
		binaryPath: binaryPath,
		timeout:    defaultTmuxTimeout,
	}
}

// SetSocketForTest configures an isolated tmux server socket (-L) and
// TMUX_TMPDIR for parity and integration tests.
func (e *TmuxExecutor) SetSocketForTest(socketName, tmuxTmpDir string) {
	e.socketName = socketName
	e.tmuxTmpDir = tmuxTmpDir
}

func (e *TmuxExecutor) withSocket(args ...string) []string {
	out := make([]string, 0, len(args)+2)
	if e.socketName != "" {
		out = append(out, "-L", e.socketName)
	}
	return append(out, args...)
}

// QuerySessionPane runs `tmux -u display-message -p -t =<session>: "#{pane_id} #{pid} #{pane_tty} #{pane_title}"`.
func (e *TmuxExecutor) QuerySessionPane(ctx context.Context, sessionName string) ([]byte, error) {
	assertDeclaredRead(proto.ReadPaneScreenTmux)
	if !tmuxSessionNameRe.MatchString(sessionName) {
		return nil, fmt.Errorf("invalid tmux session name %q", sessionName)
	}
	args := e.withSocket("-u", "display-message", "-p", "-t", "="+sessionName+":", tmuxFormatSessionPane)
	return e.run(ctx, args, nil, maxTmuxOutputBytes)
}

// QueryPaneTTY runs `tmux -u display-message -p -t <paneID> "#{pane_tty}"`.
func (e *TmuxExecutor) QueryPaneTTY(ctx context.Context, paneID string) ([]byte, error) {
	assertDeclaredRead(proto.ReadPaneScreenTmux)
	if !tmuxPaneIDRe.MatchString(paneID) {
		return nil, fmt.Errorf("invalid tmux pane id %q", paneID)
	}
	args := e.withSocket("-u", "display-message", "-p", "-t", paneID, tmuxFormatPaneTTY)
	return e.run(ctx, args, nil, maxTmuxOutputBytes)
}

// QueryPaneTitle runs `tmux -u display-message -p -t <paneID> "#{pane_title}"`.
func (e *TmuxExecutor) QueryPaneTitle(ctx context.Context, paneID string) ([]byte, error) {
	assertDeclaredRead(proto.ReadPaneScreenTmux)
	if !tmuxPaneIDRe.MatchString(paneID) {
		return nil, fmt.Errorf("invalid tmux pane id %q", paneID)
	}
	args := e.withSocket("-u", "display-message", "-p", "-t", paneID, tmuxFormatPaneTitle)
	return e.run(ctx, args, nil, maxTmuxOutputBytes)
}

// QueryPaneSessionName runs `tmux -u display-message -p -t <paneID> "#{session_name}"`.
func (e *TmuxExecutor) QueryPaneSessionName(ctx context.Context, paneID string) ([]byte, error) {
	assertDeclaredRead(proto.ReadPaneScreenTmux)
	if !tmuxPaneIDRe.MatchString(paneID) {
		return nil, fmt.Errorf("invalid tmux pane id %q", paneID)
	}
	args := e.withSocket("-u", "display-message", "-p", "-t", paneID, tmuxFormatSessionName)
	return e.run(ctx, args, nil, maxTmuxOutputBytes)
}

// CapturePane runs `tmux -u capture-pane -p [-e] -t <paneID>`.
func (e *TmuxExecutor) CapturePane(ctx context.Context, paneID string, ansi bool) ([]byte, error) {
	assertDeclaredRead(proto.ReadPaneScreenTmux)
	if !tmuxPaneIDRe.MatchString(paneID) {
		return nil, fmt.Errorf("invalid tmux pane id %q", paneID)
	}
	var args []string
	if ansi {
		args = e.withSocket("-u", "capture-pane", "-p", "-e", "-t", paneID)
	} else {
		args = e.withSocket("-u", "capture-pane", "-p", "-t", paneID)
	}
	return e.run(ctx, args, nil, maxTmuxCaptureBytes)
}

// SendKey runs `tmux send-keys -t <paneID> <key>` under a declared action capability.
func (e *TmuxExecutor) SendKey(ctx context.Context, actionCap, paneID, key string) error {
	assertDeclaredAction(actionCap)
	if !tmuxPaneIDRe.MatchString(paneID) {
		return fmt.Errorf("invalid tmux pane id %q", paneID)
	}
	if !isAllowedTmuxKey(key) {
		return fmt.Errorf("disallowed tmux key %q", key)
	}
	args := e.withSocket("send-keys", "-t", paneID, key)
	_, err := e.run(ctx, args, nil, maxTmuxOutputBytes)
	return err
}

// LoadBuffer runs `tmux load-buffer -b <bufName> -` reading data from stdin.
// On Linux, data is backed by an anonymous in-memory memfd (regular seekable
// file, zero disk writes) rather than a pipe, eliminating tmux's non-blocking
// stdin pipe EOF race under load; falls back to bytes.NewReader if memfd_create
// is unavailable.
func (e *TmuxExecutor) LoadBuffer(ctx context.Context, actionCap, bufName string, data []byte) error {
	assertDeclaredAction(actionCap)
	if !tmuxBufNameRe.MatchString(bufName) {
		return fmt.Errorf("invalid tmux buffer name %q", bufName)
	}
	args := e.withSocket("load-buffer", "-b", bufName, "-")
	var stdin io.Reader
	if mf, err := makeMemfdReader(data); err == nil {
		defer mf.Close()
		stdin = mf
	} else {
		stdin = bytes.NewReader(data)
	}
	_, err := e.run(ctx, args, stdin, maxTmuxOutputBytes)
	return err
}

func makeMemfdReader(data []byte) (*os.File, error) {
	fd, err := unix.MemfdCreate("corral-send", unix.MFD_CLOEXEC)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), "corral-send-memfd")
	if len(data) > 0 {
		if _, err := f.Write(data); err != nil {
			_ = f.Close()
			return nil, err
		}
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			_ = f.Close()
			return nil, err
		}
	}
	return f, nil
}

// PasteBuffer runs `tmux paste-buffer -p -r -d -b <bufName> -t <paneID>`.
func (e *TmuxExecutor) PasteBuffer(ctx context.Context, actionCap, bufName, paneID string) error {
	assertDeclaredAction(actionCap)
	if !tmuxBufNameRe.MatchString(bufName) {
		return fmt.Errorf("invalid tmux buffer name %q", bufName)
	}
	if !tmuxPaneIDRe.MatchString(paneID) {
		return fmt.Errorf("invalid tmux pane id %q", paneID)
	}
	args := e.withSocket("paste-buffer", "-p", "-r", "-d", "-b", bufName, "-t", paneID)
	_, err := e.run(ctx, args, nil, maxTmuxOutputBytes)
	return err
}

// DeleteBuffer runs `tmux delete-buffer -b <bufName>`.
func (e *TmuxExecutor) DeleteBuffer(ctx context.Context, actionCap, bufName string) error {
	assertDeclaredAction(actionCap)
	if !tmuxBufNameRe.MatchString(bufName) {
		return fmt.Errorf("invalid tmux buffer name %q", bufName)
	}
	args := e.withSocket("delete-buffer", "-b", bufName)
	_, err := e.run(ctx, args, nil, maxTmuxOutputBytes)
	return err
}

func (e *TmuxExecutor) openTrustedBinary() (*os.File, error) {
	uid := e.uid
	if e.binaryPath != "" {
		f, _, err := daemon.OpenTrustedTmuxBinary(e.binaryPath, uid)
		return f, err
	}
	searchDirs := []string{
		filepath.Join(e.home, ".local", "bin"),
		filepath.Join(e.home, "bin"),
		"/opt/homebrew/bin",
		"/usr/local/bin",
		"/usr/bin",
		"/bin",
		"/usr/sbin",
		"/sbin",
	}
	for _, dir := range searchDirs {
		if dir == "" {
			continue
		}
		cand := filepath.Join(dir, "tmux")
		if f, _, err := daemon.OpenTrustedTmuxBinary(cand, uid); err == nil {
			return f, nil
		}
	}
	return nil, errors.New("trusted tmux binary not found in standard paths")
}

// validateTmuxArgs enforces that the ENTIRE argument slice matches one of the
// exact hard-coded allowlisted tmux forms (no extra args, no arbitrary format
// strings, no `#(...)` or command chaining).
func validateTmuxArgs(args []string) error {
	rest := args
	if len(rest) >= 2 && rest[0] == "-L" {
		if !tmuxSocketNameRe.MatchString(rest[1]) {
			return fmt.Errorf("invalid tmux socket name %q", rest[1])
		}
		rest = rest[2:]
	}
	if len(rest) == 0 {
		return errors.New("empty tmux argument list")
	}
	switch {
	case len(rest) == 6 && rest[0] == "-u" && rest[1] == "display-message" && rest[2] == "-p" && rest[3] == "-t":
		target := rest[4]
		format := rest[5]
		switch format {
		case tmuxFormatSessionPane:
			if len(target) < 3 || target[0] != '=' || target[len(target)-1] != ':' {
				return fmt.Errorf("invalid session target %q", target)
			}
			sess := target[1 : len(target)-1]
			if !tmuxSessionNameRe.MatchString(sess) {
				return fmt.Errorf("invalid session name in target %q", target)
			}
			return nil
		case tmuxFormatPaneTTY, tmuxFormatPaneTitle, tmuxFormatSessionName:
			if !tmuxPaneIDRe.MatchString(target) {
				return fmt.Errorf("invalid pane target %q", target)
			}
			return nil
		default:
			return fmt.Errorf("disallowed tmux format %q", format)
		}

	case len(rest) == 5 && rest[0] == "-u" && rest[1] == "capture-pane" && rest[2] == "-p" && rest[3] == "-t":
		if !tmuxPaneIDRe.MatchString(rest[4]) {
			return fmt.Errorf("invalid capture-pane target %q", rest[4])
		}
		return nil

	case len(rest) == 6 && rest[0] == "-u" && rest[1] == "capture-pane" && rest[2] == "-p" && rest[3] == "-e" && rest[4] == "-t":
		if !tmuxPaneIDRe.MatchString(rest[5]) {
			return fmt.Errorf("invalid capture-pane target %q", rest[5])
		}
		return nil

	case len(rest) == 4 && rest[0] == "send-keys" && rest[1] == "-t":
		if !tmuxPaneIDRe.MatchString(rest[2]) {
			return fmt.Errorf("invalid send-keys target %q", rest[2])
		}
		if !isAllowedTmuxKey(rest[3]) {
			return fmt.Errorf("disallowed send-keys key %q", rest[3])
		}
		return nil

	case len(rest) == 4 && rest[0] == "load-buffer" && rest[1] == "-b" && rest[3] == "-":
		if !tmuxBufNameRe.MatchString(rest[2]) {
			return fmt.Errorf("invalid load-buffer name %q", rest[2])
		}
		return nil

	case len(rest) == 8 && rest[0] == "paste-buffer" && rest[1] == "-p" && rest[2] == "-r" && rest[3] == "-d" && rest[4] == "-b" && rest[6] == "-t":
		if !tmuxBufNameRe.MatchString(rest[5]) {
			return fmt.Errorf("invalid paste-buffer name %q", rest[5])
		}
		if !tmuxPaneIDRe.MatchString(rest[7]) {
			return fmt.Errorf("invalid paste-buffer target %q", rest[7])
		}
		return nil

	case len(rest) == 3 && rest[0] == "delete-buffer" && rest[1] == "-b":
		if !tmuxBufNameRe.MatchString(rest[2]) {
			return fmt.Errorf("invalid delete-buffer name %q", rest[2])
		}
		return nil

	default:
		return fmt.Errorf("disallowed tmux command %v", rest)
	}
}

// run is the sole reviewed os/exec call site in host-side corrald code.
// It executes the already-opened and verified tmux file descriptor via
// /proc/self/fd/3 (ExtraFiles[0]), eliminating the TOCTOU window between
// safe-path validation and exec.
func (e *TmuxExecutor) run(ctx context.Context, args []string, stdin io.Reader, maxOut int) ([]byte, error) {
	if err := validateTmuxArgs(args); err != nil {
		return nil, err
	}
	binFile, err := e.openTrustedBinary()
	if err != nil {
		return nil, err
	}
	defer binFile.Close()

	timeout := e.timeout
	if timeout <= 0 {
		timeout = defaultTmuxTimeout
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(runCtx, "/proc/self/fd/3", args...)
	cmd.Args[0] = "tmux"
	cmd.ExtraFiles = []*os.File{binFile}
	if e.tmuxTmpDir != "" {
		cmd.Env = []string{"TMUX_TMPDIR=" + e.tmuxTmpDir}
	}
	if stdin != nil {
		cmd.Stdin = stdin
	}
	if maxOut <= 0 {
		maxOut = maxTmuxOutputBytes
	}
	var stdout bytes.Buffer
	cmd.Stdout = &limitedWriter{w: &stdout, max: maxOut}
	cmd.Stderr = io.Discard
	if err := cmd.Run(); err != nil {
		return nil, err
	}
	return stdout.Bytes(), nil
}

type limitedWriter struct {
	w   io.Writer
	max int
	n   int
}

func (l *limitedWriter) Write(p []byte) (int, error) {
	origLen := len(p)
	if l.n < l.max {
		rem := l.max - l.n
		if len(p) > rem {
			p = p[:rem]
		}
		written, err := l.w.Write(p)
		l.n += written
		if err != nil {
			return written, err
		}
	}
	// Discard excess bytes cleanly so the child never receives SIGPIPE.
	return origLen, nil
}
