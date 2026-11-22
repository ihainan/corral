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
	FocusedTab       string      `json:"focused_tab_id"` // (not from herdr 0.8: its workspace's active tab then)
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

type AgentSessionInfo struct {
	Source string `json:"source,omitempty"`
	Agent  string `json:"agent,omitempty"`
	Kind   string `json:"kind,omitempty"`
	Value  string `json:"value"`
}

type Pane struct {
	ID          string `json:"pane_id"`
	TabID       string `json:"tab_id"`
	WorkspaceID string `json:"workspace_id"`
	AgentStatus string `json:"agent_status"`
	Agent       string `json:"agent,omitempty"`
	Name        string `json:"name,omitempty"`
	Cwd         string `json:"cwd"`
	Focused     bool   `json:"focused"`
	Label       string `json:"label,omitempty"` // (the name given it, if one was: pane.rename)
	Title       string `json:"terminal_title_stripped,omitempty"`
	// AgentSession is the agent's own id for what runs there, as herdr's
	// integration reports it (Claude Code's session id, Codex's id/path, etc.).
	AgentSession *AgentSessionInfo `json:"agent_session,omitempty"`
	// Tokens are pane metadata tokens reported via pane.report_metadata.
	Tokens map[string]string `json:"tokens,omitempty"`
	// TerminalID names the terminal the pane runs (a new one when herdr or
	// the pane starts again); StateSeq is herdr's count of status changes
	// at its agent's last one. Together they tell one status of the pane's
	// from another, whatever the gateway saw meanwhile. (In the agents'
	// list only, and not from every herdr: "", 0 where not said.)
	TerminalID string `json:"terminal_id,omitempty"`
	StateSeq   int64  `json:"state_change_seq,omitempty"`
}

// Status values herdr reports for agents.
const (
	StatusIdle    = "idle"
	StatusWorking = "working"
	StatusBlocked = "blocked" // waiting for the user
	StatusDone    = "done"
	// StatusGone is not herdr's: a transition to it reports a pane that
	// closed.
	StatusGone    = "gone"
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
	return DialWait(ctx, c, socketPath, 0)
}

// DialWait is Dial waiting up to await for a side operation's slot on c
// where all are taken (sshx.DialOptions.AwaitSlot): for what a person asked.
func DialWait(ctx context.Context, c *ssh.Client, socketPath string, await time.Duration) (*Client, error) {
	ctx, cancel := context.WithTimeout(ctx, dialTimeout+await)
	defer cancel()
	conn, err := sshx.DialVia(ctx, c, "unix", socketPath, sshx.DialOptions{Aux: true, AwaitSlot: await})
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
	return c.CallWithin(ctx, callTimeout, method, params, out)
}

// CallWithin is Call with its own bound on the exchange (one that waits on
// herdr itself, an agent starting, is longer).
func (c *Client) CallWithin(ctx context.Context, within time.Duration, method string, params any, out any) error {
	if params == nil {
		params = struct{}{}
	}
	id := strconv.FormatInt(c.seq.Add(1), 10)
	b, err := json.Marshal(request{ID: id, Method: method, Params: params})
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, within)
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
	return c.ReadPaneSource(ctx, paneID, "visible", lines)
}

// ReadPaneSource is ReadPane from another of herdr's sources ("recent",
// "recent_unwrapped": the lines it wrapped joined). One a herdr does not
// have is refused, and the connection closed: asked last on it.
func (c *Client) ReadPaneSource(ctx context.Context, paneID, source string, lines int) (string, error) {
	var res struct {
		Read struct {
			Text string `json:"text"`
		} `json:"read"`
	}
	params := map[string]any{"pane_id": paneID, "source": source, "strip_ansi": true}
	if lines > 0 {
		params["lines"] = lines
	}
	if err := c.Call(ctx, "pane.read", params, &res); err != nil {
		return "", err
	}
	return res.Read.Text, nil
}

// ReadPaneANSI returns the pane's text with ANSI colour escape sequences intact
// (format: "ansi"), directly over the herdr Unix socket without spawning a shell.
func (c *Client) ReadPaneANSI(ctx context.Context, paneID, source string, lines int) (string, error) {
	var res struct {
		Read struct {
			Text string `json:"text"`
		} `json:"read"`
	}
	params := map[string]any{"pane_id": paneID, "source": source, "format": "ansi"}
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

// NewWorkspace opens a workspace and focuses it; its folder follows the
// focused pane of sourceWS ("" for none) as herdr's settings say.
func (c *Client) NewWorkspace(ctx context.Context, sourceWS string) error {
	params := map[string]any{"focus": true}
	if sourceWS != "" {
		params["source_workspace_id"] = sourceWS
	}
	return c.Call(ctx, "workspace.create", params, nil)
}

// CreateWorkspace opens a workspace in folder cwd, named label, not
// focused: its id and its first pane's.
func (c *Client) CreateWorkspace(ctx context.Context, cwd, label string) (workspace, pane string, err error) {
	ws, p, _, err := c.CreateWorkspaceWithEnv(ctx, cwd, label, nil)
	return ws, p, err
}

// CreateWorkspaceWithEnv opens a workspace in folder cwd, named label, not
// focused, optionally injecting env vars into its root pane, and returns the
// workspace id, root pane id, and root pane terminal id.
func (c *Client) CreateWorkspaceWithEnv(ctx context.Context, cwd, label string, env map[string]string) (workspace, pane, terminalID string, err error) {
	var res struct {
		Workspace struct {
			ID string `json:"workspace_id"`
		} `json:"workspace"`
		RootPane struct {
			ID         string `json:"pane_id"`
			TerminalID string `json:"terminal_id"`
		} `json:"root_pane"`
	}
	params := map[string]any{"cwd": cwd, "label": label, "focus": false}
	if len(env) > 0 {
		params["env"] = env
	}
	if err := c.Call(ctx, "workspace.create", params, &res); err != nil {
		return "", "", "", err
	}
	return res.Workspace.ID, res.RootPane.ID, res.RootPane.TerminalID, nil
}

// ReportMetadata attaches metadata tokens to a pane via pane.report_metadata.
func (c *Client) ReportMetadata(ctx context.Context, paneID, source string, tokens map[string]string, ttlMs int64) error {
	params := map[string]any{
		"pane_id": paneID,
		"source":  source,
		"tokens":  tokens,
	}
	if ttlMs > 0 {
		params["ttl_ms"] = ttlMs
	}
	return c.Call(ctx, "pane.report_metadata", params, nil)
}

// CloseWorkspace closes a workspace, ending what runs in its panes.
func (c *Client) CloseWorkspace(ctx context.Context, workspace string) error {
	return c.Call(ctx, "workspace.close", map[string]any{"workspace_id": workspace}, nil)
}

// ClosePane closes a pane, ending what runs in it.
func (c *Client) ClosePane(ctx context.Context, paneID string) error {
	return c.Call(ctx, "pane.close", map[string]any{"pane_id": paneID}, nil)
}

// SwapPanes swaps two panes in the same tab.
func (c *Client) SwapPanes(ctx context.Context, sourcePaneID, targetPaneID string) error {
	return c.Call(ctx, "pane.swap", map[string]any{
		"source_pane_id": sourcePaneID,
		"target_pane_id": targetPaneID,
	}, nil)
}

// WorktreeInfo describes one Git worktree known to Herdr.
type WorktreeInfo struct {
	Path             string `json:"path"`
	Branch           string `json:"branch,omitempty"`
	Label            string `json:"label,omitempty"`
	OpenWorkspaceID  string `json:"open_workspace_id,omitempty"`
	IsBare           bool   `json:"is_bare"`
	IsDetached       bool   `json:"is_detached"`
	IsPrunable       bool   `json:"is_prunable"`
	IsLinkedWorktree bool   `json:"is_linked_worktree"`
}

// WorktreeSourceInfo describes the source Git repository for worktree.list.
type WorktreeSourceInfo struct {
	RepoName string `json:"repo_name,omitempty"`
	RepoRoot string `json:"repo_root,omitempty"`
}

// WorktreeListResult is the response payload of worktree.list.
type WorktreeListResult struct {
	Source    WorktreeSourceInfo `json:"source"`
	Worktrees []WorktreeInfo     `json:"worktrees"`
}

// CreateWorktree creates a Git worktree-backed workspace via worktree.create
// without stealing focus, returning workspace id, root pane id, terminal id,
// checkout path, and branch name.
func (c *Client) CreateWorktree(ctx context.Context, cwd, branch, label string) (workspace, pane, terminalID, wtPath, wtBranch string, err error) {
	var res struct {
		Workspace struct {
			ID string `json:"workspace_id"`
		} `json:"workspace"`
		RootPane struct {
			ID         string `json:"pane_id"`
			TerminalID string `json:"terminal_id"`
		} `json:"root_pane"`
		Worktree WorktreeInfo `json:"worktree"`
	}
	params := map[string]any{"cwd": cwd, "focus": false}
	if branch != "" {
		params["branch"] = branch
	}
	if label != "" {
		params["label"] = label
	}
	// Creating a git worktree can take a few seconds on large repos.
	if err := c.CallWithin(ctx, 30*time.Second, "worktree.create", params, &res); err != nil {
		return "", "", "", "", "", err
	}
	return res.Workspace.ID, res.RootPane.ID, res.RootPane.TerminalID, res.Worktree.Path, res.Worktree.Branch, nil
}

// ListWorktrees queries Herdr's worktree.list for cwd or workspaceID.
func (c *Client) ListWorktrees(ctx context.Context, cwd, workspaceID string) (*WorktreeListResult, error) {
	params := map[string]any{}
	if workspaceID != "" {
		params["workspace_id"] = workspaceID
	} else if cwd != "" {
		params["cwd"] = cwd
	}
	var res WorktreeListResult
	if err := c.Call(ctx, "worktree.list", params, &res); err != nil {
		return nil, err
	}
	return &res, nil
}

// RemoveWorktree closes and removes a worktree-backed workspace via worktree.remove.
func (c *Client) RemoveWorktree(ctx context.Context, workspaceID string, force bool) error {
	return c.CallWithin(ctx, 20*time.Second, "worktree.remove", map[string]any{
		"workspace_id": workspaceID,
		"force":        force,
	}, nil)
}

// ProcessEntry is one foreground process reported by pane.process_info.
type ProcessEntry struct {
	PID     int      `json:"pid"`
	Name    string   `json:"name"`
	Argv    []string `json:"argv,omitempty"`
	Cmdline string   `json:"cmdline,omitempty"`
	Cwd     string   `json:"cwd,omitempty"`
}

// PaneProcessInfoResult is the structured process info for a Herdr pane.
type PaneProcessInfoResult struct {
	PaneID                   string         `json:"pane_id"`
	ShellPID                 int            `json:"shell_pid"`
	ForegroundProcessGroupID int            `json:"foreground_process_group_id"`
	ForegroundProcesses      []ProcessEntry `json:"foreground_processes"`
}

// PaneProcessInfo queries Herdr's pane.process_info for paneID.
func (c *Client) PaneProcessInfo(ctx context.Context, paneID string) (*PaneProcessInfoResult, error) {
	var res struct {
		ProcessInfo PaneProcessInfoResult `json:"process_info"`
	}
	if err := c.Call(ctx, "pane.process_info", map[string]any{"pane_id": paneID}, &res); err != nil {
		return nil, err
	}
	return &res.ProcessInfo, nil
}

// AgentExplain queries Herdr's agent.explain for target (pane id or agent name)
// and returns the raw JSON explain object.
func (c *Client) AgentExplain(ctx context.Context, target string) (json.RawMessage, error) {
	var res struct {
		Explain json.RawMessage `json:"explain"`
	}
	if err := c.Call(ctx, "agent.explain", map[string]any{"target": target}, &res); err != nil {
		return nil, err
	}
	return res.Explain, nil
}

// Split splits the focused pane, "right" or "down", focusing the new one.
func (c *Client) Split(ctx context.Context, direction string) error {
	return c.Call(ctx, "pane.split", map[string]any{"direction": direction, "focus": true}, nil)
}

// SplitPaneWithEnv splits targetPaneID ("right" or "down") without stealing
// focus, optionally setting cwd and env vars on the new pane, and returns the
// workspace id, new pane id, and new pane terminal id.
func (c *Client) SplitPaneWithEnv(ctx context.Context, targetPaneID, direction, cwd string, env map[string]string) (workspace, pane, terminalID string, err error) {
	if direction == "" {
		direction = "right"
	}
	var res struct {
		Pane struct {
			ID          string `json:"pane_id"`
			WorkspaceID string `json:"workspace_id"`
			TerminalID  string `json:"terminal_id"`
		} `json:"pane"`
	}
	params := map[string]any{
		"direction":      direction,
		"target_pane_id": targetPaneID,
		"focus":          false,
	}
	if cwd != "" {
		params["cwd"] = cwd
	}
	if len(env) > 0 {
		params["env"] = env
	}
	if err := c.Call(ctx, "pane.split", params, &res); err != nil {
		return "", "", "", err
	}
	return res.Pane.WorkspaceID, res.Pane.ID, res.Pane.TerminalID, nil
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

// RenameWorkspace names a workspace (herdr sets even an empty name: not
// asked for one).
func (c *Client) RenameWorkspace(ctx context.Context, workspaceID, label string) error {
	return c.Call(ctx, "workspace.rename", map[string]any{"workspace_id": workspaceID, "label": label}, nil)
}

func (c *Client) RenameTab(ctx context.Context, tabID, label string) error {
	return c.Call(ctx, "tab.rename", map[string]any{"tab_id": tabID, "label": label}, nil)
}

// RenamePane gives a pane a name of its own; an empty one takes it away
// (herdr names it as before). herdr tells no event of it.
func (c *Client) RenamePane(ctx context.Context, paneID, label string) error {
	return c.Call(ctx, "pane.rename", map[string]any{"pane_id": paneID, "label": label}, nil)
}

// Subscribe starts an event stream on this connection. Each event is reported
// to fn as a raw JSON line; the call returns when the stream ends. herdr may
// drop a slow subscriber, so callers treat any event as "refresh the snapshot"
// and re-subscribe after the stream ends.
func (c *Client) Subscribe(ctx context.Context, subs []map[string]any, fn func(event json.RawMessage)) error {
	if err := c.StartSubscribe(ctx, subs); err != nil {
		return err
	}
	return c.ReadEvents(ctx, fn)
}

// StartSubscribe sends events.subscribe and waits for its acknowledgement.
// If it returns an error, herdr closes the connection and callers must Dial a
// fresh client before retrying.
func (c *Client) StartSubscribe(ctx context.Context, subs []map[string]any) error {
	return c.Call(ctx, "events.subscribe", map[string]any{"subscriptions": subs}, nil)
}

// ReadEvents reads raw event lines after StartSubscribe succeeded.
func (c *Client) ReadEvents(ctx context.Context, fn func(event json.RawMessage)) error {
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
