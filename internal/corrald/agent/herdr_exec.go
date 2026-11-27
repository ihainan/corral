//go:build linux

package agent

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"syscall"
	"time"

	"github.com/ihainan/corral/internal/corrald/daemon"
	"github.com/ihainan/corral/internal/corrald/proto"
	"github.com/ihainan/corral/internal/herdr/wire"
)

const (
	defaultHerdrDialTimeout = 5 * time.Second
	defaultHerdrCallTimeout = 5 * time.Second
)

// HerdrExecutor talks directly to a discovered herdr Unix socket on the host
// using the shared newline-delimited JSON protocol (internal/herdr/wire)
// without executing any external `herdr` binary.
//
// Before connecting, it verifies:
//   - sockPath is a clean absolute path previously observed in locate results
//   - Lstat(sockPath) is NOT a symlink, IS a Unix socket, and is owned by uid
//   - After connecting, SO_PEERCRED confirms the listening server's uid == uid
type HerdrExecutor struct {
	uid         uint32
	rd          *capReader
	val         *PathValidator
	dialTimeout time.Duration
	callTimeout time.Duration
}

// NewHerdrExecutor creates a HerdrExecutor bound to uid and val.
func NewHerdrExecutor(uid uint32, rd *capReader, val *PathValidator) *HerdrExecutor {
	return &HerdrExecutor{
		uid:         uid,
		rd:          rd,
		val:         val,
		dialTimeout: defaultHerdrDialTimeout,
		callTimeout: defaultHerdrCallTimeout,
	}
}

// NewHerdrExecutorForTest creates a HerdrExecutor with a fresh capReader rooted
// at procRoot, for cross-package tests.
func NewHerdrExecutorForTest(uid uint32, procRoot string, val *PathValidator) *HerdrExecutor {
	return NewHerdrExecutor(uid, newCapReader(procRoot), val)
}

func (e *HerdrExecutor) dial(ctx context.Context, sockPath string) (*wire.Conn, error) {
	if !isCleanAbsPath(sockPath) {
		return nil, fmt.Errorf("invalid herdr socket path %q", sockPath)
	}
	if e.val == nil || !e.val.HasHerdrSocket(sockPath) {
		return nil, fmt.Errorf("herdr socket %q not in located sockets", sockPath)
	}
	info, err := e.rd.lstat(proto.ReadPaneScreenHerdr, sockPath)
	if err != nil {
		return nil, fmt.Errorf("lstat herdr socket %q: %w", sockPath, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("herdr socket %q is a symlink", sockPath)
	}
	if info.Mode()&os.ModeSocket == 0 {
		return nil, fmt.Errorf("herdr socket %q is not a unix socket", sockPath)
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || st.Uid != e.uid {
		return nil, fmt.Errorf("herdr socket %q owner mismatch", sockPath)
	}

	timeout := e.dialTimeout
	if timeout <= 0 {
		timeout = defaultHerdrDialTimeout
	}
	d := net.Dialer{Timeout: timeout}
	rawConn, err := d.DialContext(ctx, "unix", sockPath)
	if err != nil {
		return nil, err
	}
	uconn, ok := rawConn.(*net.UnixConn)
	if !ok {
		_ = rawConn.Close()
		return nil, errors.New("herdr dial did not yield unix connection")
	}
	if _, _, err := daemon.CheckPeerCred(uconn, e.uid); err != nil {
		_ = uconn.Close()
		return nil, fmt.Errorf("herdr socket peer uid mismatch: %w", err)
	}
	return wire.NewConn(uconn), nil
}

func (e *HerdrExecutor) callTimeoutDur() time.Duration {
	if e.callTimeout > 0 {
		return e.callTimeout
	}
	return defaultHerdrCallTimeout
}

// ReadPane reads pane text via herdr pane.read.
func (e *HerdrExecutor) ReadPane(ctx context.Context, sockPath, paneID, source string, lines int, ansi bool) (string, error) {
	assertDeclaredRead(proto.ReadPaneScreenHerdr)
	c, err := e.dial(ctx, sockPath)
	if err != nil {
		return "", err
	}
	defer c.Close()

	var res struct {
		Read struct {
			Text string `json:"text"`
		} `json:"read"`
	}
	params := map[string]any{"pane_id": paneID, "source": source}
	if ansi {
		params["format"] = "ansi"
	} else {
		params["strip_ansi"] = true
	}
	if lines > 0 {
		params["lines"] = lines
	}
	if err := c.CallWithin(ctx, e.callTimeoutDur(), "pane.read", params, &res); err != nil {
		return "", err
	}
	return res.Read.Text, nil
}

// GetPaneTitle queries herdr pane.get and returns terminal_title and whether
// the field was present as a JSON string.
func (e *HerdrExecutor) GetPaneTitle(ctx context.Context, sockPath, paneID string) (string, bool, error) {
	assertDeclaredRead(proto.ReadPaneScreenHerdr)
	c, err := e.dial(ctx, sockPath)
	if err != nil {
		return "", false, err
	}
	defer c.Close()

	var res struct {
		Pane *struct {
			TerminalTitle *string `json:"terminal_title"`
		} `json:"pane"`
		TerminalTitle *string `json:"terminal_title"`
	}
	if err := c.CallWithin(ctx, e.callTimeoutDur(), "pane.get", map[string]any{"pane_id": paneID}, &res); err != nil {
		return "", false, err
	}
	if res.Pane != nil && res.Pane.TerminalTitle != nil {
		return *res.Pane.TerminalTitle, true, nil
	}
	if res.TerminalTitle != nil {
		return *res.TerminalTitle, true, nil
	}
	return "", false, nil
}

// SendKeys presses one or more named keys via herdr pane.send_keys.
func (e *HerdrExecutor) SendKeys(ctx context.Context, actionCap, sockPath, paneID string, keys []string) error {
	assertDeclaredAction(actionCap)
	c, err := e.dial(ctx, sockPath)
	if err != nil {
		return err
	}
	defer c.Close()
	return c.CallWithin(ctx, e.callTimeoutDur(), "pane.send_keys", map[string]any{
		"pane_id": paneID,
		"keys":    keys,
	}, nil)
}

// SendText types literal text via herdr pane.send_text.
func (e *HerdrExecutor) SendText(ctx context.Context, actionCap, sockPath, paneID, text string) error {
	assertDeclaredAction(actionCap)
	c, err := e.dial(ctx, sockPath)
	if err != nil {
		return err
	}
	defer c.Close()
	return c.CallWithin(ctx, e.callTimeoutDur(), "pane.send_text", map[string]any{
		"pane_id": paneID,
		"text":    text,
	}, nil)
}
