// Package herdr talks to a herdr server running on a target host.
//
// herdr exposes a newline-delimited JSON API on a Unix socket owned by the user.
// We reach that socket through the SSH connection we already hold (OpenSSH's
// direct-streamlocal channel), so nothing extra has to be installed or opened
// on the host. Only the documented socket API is used, never herdr's internal
// client protocol, which herdr does not promise to keep stable.
package herdr

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strconv"
	"sync/atomic"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/ihainan/corral/internal/sshx"
)

// SessionInfo is one entry of `herdr session list --json`.
type SessionInfo struct {
	Name       string `json:"name"`
	Default    bool   `json:"default"`
	Running    bool   `json:"running"`
	SocketPath string `json:"socket_path"`
	SessionDir string `json:"session_dir"`
}

// DiscoverCommand prints herdr's session list. Fixed text: herdr is often
// installed in a per-user directory that a non-interactive SSH shell does not
// have on PATH, so the common locations are added.
const DiscoverCommand = `sh -c 'PATH="$HOME/.local/bin:$HOME/bin:$HOME/.cargo/bin:/opt/homebrew/bin:/usr/local/bin:$PATH"; ` +
	`command -v herdr >/dev/null 2>&1 || { echo "{\"missing\":true}"; exit 0; }; herdr session list --json'`

type discoverOutput struct {
	Missing  bool          `json:"missing"`
	Sessions []SessionInfo `json:"sessions"`
}

var ErrNotInstalled = errors.New("herdr is not installed on this host")

// Discover runs the session list on the host.
func Discover(ctx context.Context, c *ssh.Client) ([]SessionInfo, error) {
	out, err := sshx.Output(ctx, c, DiscoverCommand, sshx.MaxOutput)
	if err != nil {
		return nil, fmt.Errorf("herdr session list: %w", err)
	}
	var d discoverOutput
	if err := json.Unmarshal(out, &d); err != nil {
		return nil, fmt.Errorf("herdr session list: unexpected output")
	}
	if d.Missing {
		return nil, ErrNotInstalled
	}
	return d.Sessions, nil
}

// Snapshot is the subset of `session.snapshot` the gateway uses.
type Snapshot struct {
	Version          string      `json:"version"`
	FocusedWorkspace string      `json:"focused_workspace_id"`
	FocusedPane      string      `json:"focused_pane_id"`
	Workspaces       []Workspace `json:"workspaces"`
	Tabs             []Tab       `json:"tabs"`
	Panes            []Pane      `json:"panes"`
	Agents           []Pane      `json:"agents"` // the panes that run a detected agent
}

type Workspace struct {
	ID          string `json:"workspace_id"`
	Label       string `json:"label"`
	Number      int    `json:"number"`
	AgentStatus string `json:"agent_status"`
	Focused     bool   `json:"focused"`
	ActiveTab   string `json:"active_tab_id"`
	TabCount    int    `json:"tab_count"`
	PaneCount   int    `json:"pane_count"`
}

type Tab struct {
	ID          string `json:"tab_id"`
	WorkspaceID string `json:"workspace_id"`
	Label       string `json:"label"`
	Number      int    `json:"number"`
	AgentStatus string `json:"agent_status"`
	Focused     bool   `json:"focused"`
	PaneCount   int    `json:"pane_count"`
}

type Pane struct {
	ID          string `json:"pane_id"`
	TabID       string `json:"tab_id"`
	WorkspaceID string `json:"workspace_id"`
	AgentStatus string `json:"agent_status"`
	Agent       string `json:"agent,omitempty"`
	Cwd         string `json:"cwd"`
	Focused     bool   `json:"focused"`
	Title       string `json:"terminal_title_stripped,omitempty"`
}

// Status values herdr reports for agents.
const (
	StatusIdle    = "idle"
	StatusWorking = "working"
	StatusBlocked = "blocked" // waiting for the user
	StatusDone    = "done"
	StatusUnknown = "unknown"
)

// Client is one connection to a herdr socket. herdr closes a connection after
// an error, so callers open short-lived clients for requests and a dedicated one
// for the event stream.
type Client struct {
	conn net.Conn
	r    *bufio.Reader
	seq  atomic.Int64
}

// dialTimeout bounds opening the socket channel.
var dialTimeout = 10 * time.Second

// Dial opens the socket through the SSH connection, giving up when ctx ends
// or after dialTimeout. The connection carries a terminal, so herdr traffic is
// a side operation: it is never cut off by closing the connection, and while
// sshx.MaxAuxPerConn of them are unfinished, Dial fails with sshx.ErrBusy.
func Dial(ctx context.Context, c *ssh.Client, socketPath string) (*Client, error) {
	ctx, cancel := context.WithTimeout(ctx, dialTimeout)
	defer cancel()
	conn, err := sshx.DialVia(ctx, c, "unix", socketPath, sshx.DialOptions{Aux: true})
	if err != nil {
		return nil, fmt.Errorf("cannot reach herdr socket: %w", err)
	}
	return &Client{conn: conn, r: bufio.NewReaderSize(conn, 64<<10)}, nil
}

func (c *Client) Close() error { return c.conn.Close() }

type request struct {
	ID     string `json:"id"`
	Method string `json:"method"`
	Params any    `json:"params"`
}

type response struct {
	ID     string          `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// APIError is an error reported by herdr itself (for example an unknown method
// on an older herdr), as opposed to a transport failure.
type APIError struct{ Code, Message string }

func (e *APIError) Error() string { return "herdr: " + e.Code + ": " + e.Message }

const maxLine = 4 << 20

// callTimeout bounds one request/response exchange.
var callTimeout = 10 * time.Second

func (c *Client) readLine() ([]byte, error) {
	var buf []byte
	for {
		chunk, isPrefix, err := c.r.ReadLine()
		if err != nil {
			return nil, err
		}
		buf = append(buf, chunk...)
		if len(buf) > maxLine {
			return nil, errors.New("herdr: response too large")
		}
		if !isPrefix {
			return buf, nil
		}
	}
}

// Call sends one request and waits for its response, for at most
// callTimeout or until ctx ends. SSH channels have no deadlines, so giving up
// means closing the client, which is unusable afterwards.
func (c *Client) Call(ctx context.Context, method string, params any, out any) error {
	if params == nil {
		params = struct{}{}
	}
	id := strconv.FormatInt(c.seq.Add(1), 10)
	b, err := json.Marshal(request{ID: id, Method: method, Params: params})
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	stop := context.AfterFunc(ctx, func() { c.conn.Close() })
	defer stop()
	err = c.exchange(id, b, out)
	if err != nil && ctx.Err() != nil {
		return fmt.Errorf("herdr: %s: %w", method, ctx.Err())
	}
	return err
}

func (c *Client) exchange(id string, req []byte, out any) error {
	if _, err := c.conn.Write(append(req, '\n')); err != nil {
		return err
	}
	for {
		line, err := c.readLine()
		if err != nil {
			return err
		}
		var r response
		if err := json.Unmarshal(line, &r); err != nil {
			return fmt.Errorf("herdr: bad response: %w", err)
		}
		if r.ID != id {
			continue // an event or a stale reply
		}
		if r.Error != nil {
			return &APIError{Code: r.Error.Code, Message: r.Error.Message}
		}
		if out != nil {
			return json.Unmarshal(r.Result, out)
		}
		return nil
	}
}

func (c *Client) Snapshot(ctx context.Context) (*Snapshot, error) {
	var res struct {
		Snapshot Snapshot `json:"snapshot"`
	}
	if err := c.Call(ctx, "session.snapshot", nil, &res); err != nil {
		return nil, err
	}
	return &res.Snapshot, nil
}

// ReadPane returns the visible text of a pane (ANSI stripped), used to confirm
// a prompt is still on screen before answering it.
func (c *Client) ReadPane(ctx context.Context, paneID string, lines int) (string, error) {
	var res struct {
		Read struct {
			Text string `json:"text"`
		} `json:"read"`
	}
	params := map[string]any{"pane_id": paneID, "source": "visible", "strip_ansi": true}
	if lines > 0 {
		params["lines"] = lines
	}
	if err := c.Call(ctx, "pane.read", params, &res); err != nil {
		return "", err
	}
	return res.Read.Text, nil
}

func (c *Client) SendKeys(ctx context.Context, paneID string, keys []string) error {
	return c.Call(ctx, "pane.send_keys", map[string]any{"pane_id": paneID, "keys": keys}, nil)
}

// SendInput types text into a pane, then presses keys, in one request.
func (c *Client) SendInput(ctx context.Context, paneID, text string, keys []string) error {
	return c.Call(ctx, "pane.send_input", map[string]any{"pane_id": paneID, "text": text, "keys": keys}, nil)
}

// ZoomPane zooms a pane (focusing it) or ends the zoom, mode "on" or
// "off"; paneID "" is the focused pane.
func (c *Client) ZoomPane(ctx context.Context, paneID, mode string) error {
	params := map[string]any{"mode": mode}
	if paneID != "" {
		params["pane_id"] = paneID
	}
	return c.Call(ctx, "pane.zoom", params, nil)
}

// ToggleZoom zooms the focused pane, or ends its zoom.
func (c *Client) ToggleZoom(ctx context.Context) error {
	return c.Call(ctx, "pane.zoom", map[string]any{"mode": "toggle"}, nil)
}

// NewTab opens a tab in a workspace ("" for the focused one) and focuses it.
func (c *Client) NewTab(ctx context.Context, workspaceID string) error {
	params := map[string]any{"focus": true}
	if workspaceID != "" {
		params["workspace_id"] = workspaceID
	}
	return c.Call(ctx, "tab.create", params, nil)
}

// Split splits the focused pane, "right" or "down", focusing the new one.
func (c *Client) Split(ctx context.Context, direction string) error {
	return c.Call(ctx, "pane.split", map[string]any{"direction": direction, "focus": true}, nil)
}

// FocusDirection focuses the pane next to the focused one: "left", "right",
// "up" or "down".
func (c *Client) FocusDirection(ctx context.Context, direction string) error {
	return c.Call(ctx, "pane.focus_direction", map[string]any{"direction": direction}, nil)
}

func (c *Client) FocusTab(ctx context.Context, tabID string) error {
	return c.Call(ctx, "tab.focus", map[string]any{"tab_id": tabID}, nil)
}

func (c *Client) FocusPane(ctx context.Context, paneID string) error {
	return c.Call(ctx, "pane.focus", map[string]any{"pane_id": paneID}, nil)
}

func (c *Client) FocusWorkspace(ctx context.Context, workspaceID string) error {
	return c.Call(ctx, "workspace.focus", map[string]any{"workspace_id": workspaceID}, nil)
}

// Subscribe starts an event stream on this connection. Each event is reported
// to fn as a raw JSON line; the call returns when the stream ends. herdr may
// drop a slow subscriber, so callers treat any event as "refresh the snapshot"
// and re-subscribe after the stream ends.
func (c *Client) Subscribe(ctx context.Context, subs []map[string]any, fn func(event json.RawMessage)) error {
	if err := c.Call(ctx, "events.subscribe", map[string]any{"subscriptions": subs}, nil); err != nil {
		return err
	}
	stop := context.AfterFunc(ctx, func() { c.conn.Close() })
	defer stop()
	for {
		line, err := c.readLine()
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return err
		}
		fn(json.RawMessage(line))
	}
}
