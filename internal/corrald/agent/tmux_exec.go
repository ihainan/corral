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

	"github.com/ihainan/corral/internal/corrald/daemon"
	"github.com/ihainan/corral/internal/corrald/proto"
)

const (
	defaultTmuxTimeout = 5 * time.Second
	maxTmuxOutputBytes = 4096

	// Hard-coded read-only tmux format strings. Callers cannot pass arbitrary
	// formats (which prevents tmux `#(command)` shell expansion).
	tmuxFormatSessionPane = "#{pane_id} #{pid} #{pane_tty} #{pane_title}"
	tmuxFormatPaneTTY     = "#{pane_tty}"
)

var (
	tmuxSessionNameRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	tmuxPaneIDRe      = regexp.MustCompile(`^%[0-9]{1,9}$`)
	tmuxSocketNameRe  = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)
)

// TmuxExecutor is the single allowlisted external command runner permitted in
// host-side corrald production code. It executes only a validated `tmux`
// binary directly (never via a shell), with arguments restricted strictly to
// fixed read-only display-message forms and validated session/pane targets.
type TmuxExecutor struct {
	home       string
	uid        uint32
	binaryPath string // optional validated absolute path (e.g. from --tmux)
	socketName string // optional -L <name> for isolated test servers
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

// QuerySessionPane runs `tmux -u display-message -p -t =<session>: "#{pane_id} #{pid} #{pane_tty} #{pane_title}"`.
func (e *TmuxExecutor) QuerySessionPane(ctx context.Context, sessionName string) ([]byte, error) {
	assertDeclaredRead(proto.ReadPaneScreenTmux)
	if !tmuxSessionNameRe.MatchString(sessionName) {
		return nil, fmt.Errorf("invalid tmux session name %q", sessionName)
	}
	args := make([]string, 0, 8)
	if e.socketName != "" {
		args = append(args, "-L", e.socketName)
	}
	args = append(args, "-u", "display-message", "-p", "-t", "="+sessionName+":", tmuxFormatSessionPane)
	return e.run(ctx, args)
}

// QueryPaneTTY runs `tmux -u display-message -p -t <paneID> "#{pane_tty}"`.
func (e *TmuxExecutor) QueryPaneTTY(ctx context.Context, paneID string) ([]byte, error) {
	assertDeclaredRead(proto.ReadPaneScreenTmux)
	if !tmuxPaneIDRe.MatchString(paneID) {
		return nil, fmt.Errorf("invalid tmux pane id %q", paneID)
	}
	args := make([]string, 0, 8)
	if e.socketName != "" {
		args = append(args, "-L", e.socketName)
	}
	args = append(args, "-u", "display-message", "-p", "-t", paneID, tmuxFormatPaneTTY)
	return e.run(ctx, args)
}

func (e *TmuxExecutor) resolveBinary() (string, error) {
	uid := e.uid
	if e.binaryPath != "" {
		return daemon.ValidateTmuxBinary(e.binaryPath, uid)
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
		if resolved, err := daemon.ValidateTmuxBinary(cand, uid); err == nil {
			return resolved, nil
		}
	}
	return "", errors.New("tmux binary not found in standard paths")
}

// validateTmuxArgs enforces that the ENTIRE argument slice matches one of the
// exact hard-coded read-only display-message forms (no extra args, no
// arbitrary format strings, no `#(...)` or command chaining).
func validateTmuxArgs(args []string) error {
	rest := args
	if len(rest) >= 2 && rest[0] == "-L" {
		if !tmuxSocketNameRe.MatchString(rest[1]) {
			return fmt.Errorf("invalid tmux socket name %q", rest[1])
		}
		rest = rest[2:]
	}
	if len(rest) != 6 {
		return fmt.Errorf("unexpected tmux argument count %d", len(rest))
	}
	if rest[0] != "-u" || rest[1] != "display-message" || rest[2] != "-p" || rest[3] != "-t" {
		return fmt.Errorf("disallowed tmux command prefix %v", rest[:4])
	}
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
	case tmuxFormatPaneTTY:
		if !tmuxPaneIDRe.MatchString(target) {
			return fmt.Errorf("invalid pane target %q", target)
		}
		return nil
	default:
		return fmt.Errorf("disallowed tmux format %q", format)
	}
}

// run is the sole reviewed os/exec call site in host-side corrald code.
func (e *TmuxExecutor) run(ctx context.Context, args []string) ([]byte, error) {
	if err := validateTmuxArgs(args); err != nil {
		return nil, err
	}
	bin, err := e.resolveBinary()
	if err != nil {
		return nil, err
	}
	timeout := e.timeout
	if timeout <= 0 {
		timeout = defaultTmuxTimeout
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(runCtx, bin, args...)
	var stdout bytes.Buffer
	cmd.Stdout = &limitedWriter{w: &stdout, max: maxTmuxOutputBytes}
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
