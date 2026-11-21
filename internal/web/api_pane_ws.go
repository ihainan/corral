package web

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"golang.org/x/crypto/ssh"

	"github.com/ihainan/corral/internal/sshx"
)

const (
	maxPaneAttachesPerClient = 4
	paneSetupTimeout         = 10 * time.Second
	paneOutWindow            = 4 << 20 // 4 MiB max unacked output bytes to xterm
)

// herdrTermID validates a herdr terminal_id before passing it to `herdr terminal attach`.
var herdrTermID = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,100}$`)

// paneAttachKey uniquely identifies a live direct terminal attach on a shared SSH client.
type paneAttachKey struct {
	client *ssh.Client
	termID string
}

// paneAttachSlots enforces two invariants:
//  1. At most one live direct attach per (ssh.Client, terminal_id) so an inline
//     panel and the dock terminal never fight over --takeover.
//  2. At most maxPaneAttachesPerClient concurrent pane-attach SSH channels per
//     shared SSH client, released only when the remote channel actually ends.
type paneAttachSlots struct {
	mu       sync.Mutex
	active   map[paneAttachKey]struct{}
	byClient map[*ssh.Client]int
}

func (p *paneAttachSlots) acquire(client *ssh.Client, termID string) (release func(), ok bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.active == nil {
		p.active = map[paneAttachKey]struct{}{}
		p.byClient = map[*ssh.Client]int{}
	}
	k := paneAttachKey{client: client, termID: termID}
	if _, dup := p.active[k]; dup {
		return nil, false
	}
	if p.byClient[client] >= maxPaneAttachesPerClient {
		return nil, false
	}
	p.active[k] = struct{}{}
	p.byClient[client]++
	var once sync.Once
	return func() {
		once.Do(func() {
			p.mu.Lock()
			defer p.mu.Unlock()
			if _, exists := p.active[k]; exists {
				delete(p.active, k)
				if p.byClient[client] <= 1 {
					delete(p.byClient, client)
				} else {
					p.byClient[client]--
				}
			}
		})
	}, true
}

// herdrTerminalAttachCommand attaches directly to a single herdr pane's live
// terminal stream without changing herdr's focused workspace/pane or layout.
// --takeover replaces any stale direct terminal attach for the same terminal_id
// (e.g. from a refreshed browser tab) while leaving herdr's TUI clients intact.
func herdrTerminalAttachCommand(socket, terminalID string) string {
	return `sh -c 'PATH="$HOME/.local/bin:$HOME/bin:$HOME/.cargo/bin:/opt/homebrew/bin:/usr/local/bin:$PATH"; ` +
		`unset HERDR_ENV HERDR_PANE_ID HERDR_TAB_ID HERDR_WORKSPACE_ID; ` +
		`export HERDR_SOCKET_PATH="$1"; exec herdr terminal attach "$2" --takeover' corral ` +
		sshx.QuoteShell(socket) + " " + sshx.QuoteShell(terminalID)
}

func clampPaneDim(raw string, def, lo, hi int) int {
	if raw == "" {
		return def
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return def
	}
	if n < lo {
		return lo
	}
	if n > hi {
		return hi
	}
	return n
}

type paneFlow struct {
	mu      sync.Mutex
	unacked int64
	wake    chan struct{}
}

func newPaneFlow() *paneFlow {
	return &paneFlow{wake: make(chan struct{}, 1)}
}

func (f *paneFlow) waitRoom(ctx context.Context, n int64) bool {
	for {
		f.mu.Lock()
		if f.unacked < paneOutWindow {
			f.unacked += n
			f.mu.Unlock()
			return true
		}
		f.mu.Unlock()
		select {
		case <-ctx.Done():
			return false
		case <-f.wake:
		}
	}
}

func (f *paneFlow) ack(n int64) {
	if n <= 0 {
		return
	}
	f.mu.Lock()
	f.unacked -= n
	if f.unacked < 0 {
		f.unacked = 0
	}
	f.mu.Unlock()
	select {
	case f.wake <- struct{}{}:
	default:
	}
}

// GET /api/sessions/{id}/pane-ws?pane=<pane>&cols=<c>&rows=<r>
//
// Streams a single herdr pane directly over a live SSH PTY (`herdr terminal
// attach <terminal_id> --takeover`), giving Chat Mode's embedded terminal the
// same millisecond-level interactivity as the full CLI terminal without
// changing herdr's focused workspace or layout. Returns 409 attach_unavailable
// when the session is not herdr, the pane has no terminal_id, or another live
// attach already owns the same pane so the browser can fall back cleanly.
func (s *Server) apiPaneSocket(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	sess, err := s.terms.Get(u.ID, r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "no such session")
		return
	}
	pane := r.URL.Query().Get("pane")
	if !herdrPaneID.MatchString(pane) {
		writeError(w, http.StatusBadRequest, "bad_pane", "invalid pane id")
		return
	}
	socket := s.herdrSocks.get(sess.ID)
	if socket == "" {
		writeError(w, http.StatusConflict, "attach_unavailable", "direct pane attach is not available for this session")
		return
	}
	hp, ok := s.herdrSocks.paneOf(sess.ID, pane)
	if !ok || !herdrTermID.MatchString(hp.TerminalID) {
		writeError(w, http.StatusConflict, "attach_unavailable", "pane terminal id is not available")
		return
	}
	client := sess.Client()
	if client == nil {
		writeError(w, http.StatusConflict, "attach_unavailable", "session connection is closed")
		return
	}

	cols := clampPaneDim(r.URL.Query().Get("cols"), 80, 20, 400)
	rows := clampPaneDim(r.URL.Query().Get("rows"), 24, 6, 200)

	hash := loginHash(r)
	expiry, err := s.auth.LoginExpiry(r.Context(), hash)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "unauthorized", "please sign in")
		return
	}
	ctx, cancel := context.WithDeadline(context.WithoutCancel(r.Context()), expiry)
	defer cancel()
	me := &liveSocket{loginHash: hash, cancel: cancel}
	untrack, ok := s.trackSocket(u.ID, me)
	if !ok {
		writeError(w, http.StatusTooManyRequests, "too_many_windows", "too many open terminal windows")
		return
	}
	defer untrack()
	if _, err := s.auth.CheckLogin(ctx, hash); err != nil {
		writeError(w, http.StatusUnauthorized, "unauthorized", "please sign in")
		return
	}

	// Atomically claim the slot for (client, terminal_id) before upgrading so
	// two panels on the same pane never ping-pong --takeover.
	releaseSlot, ok := s.paneAttaches.acquire(client, hp.TerminalID)
	if !ok {
		writeError(w, http.StatusConflict, "attach_unavailable", "pane already has an active direct terminal")
		return
	}
	// Accept the WebSocket BEFORE running any remote SSH command (--takeover)
	// so plain HTTP GET / cross-site navigation can never cause side effects.
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true, CompressionMode: websocket.CompressionDisabled})
	if err != nil {
		releaseSlot()
		return
	}
	c.SetReadLimit(1 << 20)
	defer c.Close(websocket.StatusNormalClosure, "")

	var writeMu sync.Mutex
	writeText := func(v any) error {
		b, _ := json.Marshal(v)
		writeMu.Lock()
		defer writeMu.Unlock()
		wctx, wcancel := context.WithTimeout(ctx, 10*time.Second)
		defer wcancel()
		return c.Write(wctx, websocket.MessageText, b)
	}
	writeBin := func(p []byte) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		wctx, wcancel := context.WithTimeout(ctx, 10*time.Second)
		defer wcancel()
		return c.Write(wctx, websocket.MessageBinary, p)
	}

	// Open the SSH session and start `herdr terminal attach` with a bounded
	// setup timeout so a dead host or early disconnect cannot hang forever.
	type setupRes struct {
		sshSess *ssh.Session
		stdin   io.WriteCloser
		stdout  io.Reader
		stderr  io.Reader
		err     error
	}
	setupDone := make(chan setupRes, 1)
	setupCtx, setupCancel := context.WithTimeout(ctx, paneSetupTimeout)
	defer setupCancel()

	go func() {
		defer close(setupDone)
		sshSess, serr := client.NewSession()
		if serr != nil {
			setupDone <- setupRes{err: serr}
			return
		}
		stopEarly := context.AfterFunc(setupCtx, func() { _ = sshSess.Close() })
		defer stopEarly()
		modes := ssh.TerminalModes{
			ssh.ECHO:          1,
			ssh.TTY_OP_ISPEED: 14400,
			ssh.TTY_OP_OSPEED: 14400,
		}
		if serr = sshSess.RequestPty("xterm-256color", rows, cols, modes); serr != nil {
			_ = sshSess.Close()
			setupDone <- setupRes{err: serr}
			return
		}
		stdin, serr := sshSess.StdinPipe()
		if serr != nil {
			_ = sshSess.Close()
			setupDone <- setupRes{err: serr}
			return
		}
		stdout, serr := sshSess.StdoutPipe()
		if serr != nil {
			_ = sshSess.Close()
			setupDone <- setupRes{err: serr}
			return
		}
		stderr, serr := sshSess.StderrPipe()
		if serr != nil {
			_ = sshSess.Close()
			setupDone <- setupRes{err: serr}
			return
		}
		err = sshSess.Start(herdrTerminalAttachCommand(socket, hp.TerminalID))
		if err != nil {
			_ = sshSess.Close()
			setupDone <- setupRes{err: err}
			return
		}
		if !stopEarly() {
			// Setup context already fired while Start was completing.
			_ = stdin.Close()
			_ = sshSess.Signal(ssh.SIGHUP)
			_ = sshSess.Close()
			_ = sshSess.Wait()
			setupDone <- setupRes{err: setupCtx.Err()}
			return
		}
		setupDone <- setupRes{sshSess: sshSess, stdin: stdin, stdout: stdout, stderr: stderr}
	}()

	var res setupRes
	select {
	case res = <-setupDone:
	case <-setupCtx.Done():
		// Drain late setup result and release the slot only once remote cleanup completes.
		go func() {
			if late := <-setupDone; late.sshSess != nil {
				_ = late.stdin.Close()
				_ = late.sshSess.Signal(ssh.SIGHUP)
				_ = late.sshSess.Close()
				_ = late.sshSess.Wait()
			}
			releaseSlot()
		}()
		_ = writeText(map[string]any{"t": "exit", "code": 1, "bytes": 0})
		return
	}
	if res.err != nil {
		releaseSlot()
		_ = writeText(map[string]any{"t": "exit", "code": 1, "bytes": 0})
		return
	}

	sshSess, stdin, stdout, stderr := res.sshSess, res.stdin, res.stdout, res.stderr
	var closeOnce sync.Once
	closeSSH := func() {
		closeOnce.Do(func() {
			_ = stdin.Close()
			_ = sshSess.Signal(ssh.SIGHUP)
			_ = sshSess.Close()
		})
	}

	flow := newPaneFlow()
	var sentBytes atomic.Int64
	var outWG sync.WaitGroup
	pump := func(rd io.Reader) {
		defer outWG.Done()
		buf := make([]byte, 32<<10)
		for {
			n, rerr := rd.Read(buf)
			if n > 0 {
				if !flow.waitRoom(ctx, int64(n)) {
					return
				}
				sentBytes.Add(int64(n))
				if werr := writeBin(buf[:n]); werr != nil {
					cancel()
					return
				}
			}
			if rerr != nil {
				return
			}
		}
	}
	outWG.Add(2)
	go pump(stdout)
	go pump(stderr)

	// Periodic login recheck matching serveTerminal.
	go func() {
		tick := time.NewTicker(loginRecheck)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				if _, err := s.auth.CheckLogin(ctx, hash); err != nil {
					cancel()
					return
				}
			}
		}
	}()

	// Wait for remote attach exit; release the per-client slot ONLY after the
	// remote session and both output pumps have actually exited.
	go func() {
		defer releaseSlot()
		werr := sshSess.Wait()
		outWG.Wait()
		code := 0
		if werr != nil {
			var ee *ssh.ExitError
			if errors.As(werr, &ee) {
				code = ee.ExitStatus()
			} else {
				code = 1
			}
		}
		_ = writeText(map[string]any{"t": "exit", "code": code, "bytes": sentBytes.Load()})
		cancel()
	}()

	// Close the SSH channel promptly when the WebSocket or login ends.
	go func() {
		<-ctx.Done()
		closeSSH()
	}()

	// Read WebSocket frames (binary -> PTY stdin, text -> resize/ping/ack).
	for {
		typ, data, rerr := c.Read(ctx)
		if rerr != nil {
			cancel()
			return
		}
		if typ == websocket.MessageBinary {
			if len(data) > 0 {
				if _, werr := stdin.Write(data); werr != nil {
					cancel()
					return
				}
			}
			continue
		}
		var msg struct {
			T    string `json:"t"`
			Cols int    `json:"cols"`
			Rows int    `json:"rows"`
			N    int64  `json:"n"`
		}
		if json.Unmarshal(data, &msg) != nil {
			continue
		}
		switch msg.T {
		case "resize":
			nc := clampPaneDim(strconv.Itoa(msg.Cols), cols, 20, 400)
			nr := clampPaneDim(strconv.Itoa(msg.Rows), rows, 6, 200)
			_ = sshSess.WindowChange(nr, nc)
		case "ack":
			flow.ack(msg.N)
		case "ping":
			_ = writeText(map[string]any{"t": "pong"})
		}
	}
}
