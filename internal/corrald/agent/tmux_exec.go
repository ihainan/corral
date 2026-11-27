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
	"strings"
	"time"

	"github.com/ihainan/corral/internal/corrald/proto"
)

const (
	defaultTmuxTimeout = 5 * time.Second
	maxTmuxOutputBytes = 4096
)

// TmuxExecutor is the single allowlisted external command runner permitted in
// host-side corrald production code. It executes only the `tmux` binary
// directly (never via a shell), with arguments passed individually and only
// allowlisted read-only subcommands permitted.
type TmuxExecutor struct {
	home       string
	binaryPath string // optional explicit path (used in tests); basename must still be "tmux"
	socketName string // optional -L <name> for isolated test servers
	timeout    time.Duration
}

// NewTmuxExecutor constructs a TmuxExecutor searching standard host paths
// under home without reading $PATH.
func NewTmuxExecutor(home string) *TmuxExecutor {
	return &TmuxExecutor{
		home:    home,
		timeout: defaultTmuxTimeout,
	}
}

// DisplayMessage runs `tmux -u display-message -p -t <target> <format>` via
// the allowlisted executor and returns its raw stdout bytes.
func (e *TmuxExecutor) DisplayMessage(ctx context.Context, target, format string) ([]byte, error) {
	assertDeclaredRead(proto.ReadPaneScreenTmux)
	if strings.ContainsAny(target, "\x00") || strings.ContainsAny(format, "\x00") {
		return nil, errors.New("invalid NUL byte in tmux argument")
	}
	args := make([]string, 0, 8)
	if e.socketName != "" {
		args = append(args, "-L", e.socketName)
	}
	args = append(args, "-u", "display-message", "-p", "-t", target, format)
	return e.run(ctx, args)
}

func (e *TmuxExecutor) resolveBinary() (string, error) {
	if e.binaryPath != "" {
		if filepath.Base(e.binaryPath) != "tmux" {
			return "", fmt.Errorf("tmux executor refuses non-tmux binary %q", e.binaryPath)
		}
		if info, err := os.Stat(e.binaryPath); err == nil && !info.IsDir() && info.Mode()&0o111 != 0 {
			return e.binaryPath, nil
		}
		return "", fmt.Errorf("tmux binary not executable: %s", e.binaryPath)
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
		if info, err := os.Stat(cand); err == nil && !info.IsDir() && info.Mode()&0o111 != 0 {
			return cand, nil
		}
	}
	return "", errors.New("tmux binary not found in standard paths")
}

// validateTmuxArgs enforces that only allowlisted read-only tmux invocations
// can ever reach exec.CommandContext.
func validateTmuxArgs(args []string) error {
	i := 0
	for i < len(args) {
		switch args[i] {
		case "-u":
			i++
		case "-L":
			if i+1 >= len(args) || args[i+1] == "" {
				return errors.New("missing tmux socket name after -L")
			}
			i += 2
		default:
			if args[i] != "display-message" {
				return fmt.Errorf("disallowed tmux subcommand %q", args[i])
			}
			return nil
		}
	}
	return errors.New("missing tmux subcommand")
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
