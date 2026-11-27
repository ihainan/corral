package web

import (
	"context"
	"encoding/binary"
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
	paneOutWindow = 4 << 20 // 4 MiB max unacked output bytes to xterm
)

var (
	paneSetupTimeout = 10 * time.Second
	// paneSlotWait bounds how long an interactive single-pane live terminal queues
	// for a slot in the per-connection SSH session ledger before reporting busy.
	paneSlotWait = 2 * time.Second
)

// RFC 4254 Section 6.2.
type panePTYRequestMsg struct {
	Term     string
	Columns  uint32
	Rows     uint32
	Width    uint32
	Height   uint32
	Modelist string
}

// RFC 4254 Section 6.7.
type paneWindowChangeMsg struct {
	Columns uint32
	Rows    uint32
	Width   uint32
	Height  uint32
}

// paneSSHSignals matches golang.org/x/crypto/ssh's RFC 4254 signal-to-number
// table so exit-signal maps to 128 + signal (e.g. TERM -> 143, KILL -> 137).
var paneSSHSignals = map[ssh.Signal]int{
	ssh.SIGABRT: 6,
	ssh.SIGALRM: 14,
	ssh.SIGFPE:  8,
	ssh.SIGHUP:  1,
	ssh.SIGILL:  4,
	ssh.SIGINT:  2,
	ssh.SIGKILL: 9,
	ssh.SIGPIPE: 13,
	ssh.SIGQUIT: 3,
	ssh.SIGSEGV: 11,
	ssh.SIGTERM: 15,
}

func encodeTerminalModes(modes ssh.TerminalModes) string {
	var tm []byte
	for k, v := range modes {
		kv := struct {
			Key byte
			Val uint32
		}{k, v}
		tm = append(tm, ssh.Marshal(&kv)...)
	}
	tm = append(tm, 0) // RFC 4254 TTY_OP_END
	return string(tm)
}

// herdrTermID validates a herdr terminal_id before passing it to `herdr terminal attach`.
var herdrTermID = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,100}$`)

// paneAttachKey uniquely identifies a live direct terminal attach on a shared SSH client.
type paneAttachKey struct {
	client *ssh.Client
	termID string
}

var errPaneDuplicate = errors.New("pane already has an active direct terminal")

// paneAttachSlots enforces two invariants:
//  1. At most one live direct attach per (ssh.Client, terminal_id) so an inline
//     panel and the dock terminal never fight over --takeover.
//  2. Per-client and total SSH session channel caps are enforced through the
//     unified sshx session ledger (sshx.SessionPane, up to sshx.MaxPanePerConn
//     and sshx.MaxSessionChannels), released only when the remote channel
//     actually ends.
type paneAttachSlots struct {
	mu     sync.Mutex
	active map[paneAttachKey]struct{}
}

func (p *paneAttachSlots) claimKey(client *ssh.Client, termID string) (release func(), ok bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.active == nil {
		p.active = map[paneAttachKey]struct{}{}
	}
	k := paneAttachKey{client: client, termID: termID}
	if _, dup := p.active[k]; dup {
		return nil, false
	}
	p.active[k] = struct{}{}
	var once sync.Once
	return func() {
		once.Do(func() {
			p.mu.Lock()
			defer p.mu.Unlock()
			delete(p.active, k)
		})
	}, true
}

// acquire claims both the unique (client, termID) key and a SessionPane slot
// from the unified sshx session ledger, waiting up to await for a channel slot.
func (p *paneAttachSlots) acquire(ctx context.Context, client *ssh.Client, termID string, await time.Duration) (release func(), err error) {
	relKey, ok := p.claimKey(client, termID)
	if !ok {
		return nil, errPaneDuplicate
	}
	relChan, err := sshx.AcquireSession(ctx, client, sshx.SessionPane, true, await)
	if err != nil {
		relKey()
		return nil, err
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			relChan()
			relKey()
		})
	}, nil
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

	// Atomically claim the unique key for (client, terminal_id) before
	// upgrading so two panels on the same pane never ping-pong --takeover.
	releaseKey, ok := s.paneAttaches.claimKey(client, hp.TerminalID)
	if !ok {
		writeError(w, http.StatusConflict, "attach_unavailable", "pane already has an active direct terminal")
		return
	}
	// Accept the WebSocket BEFORE queueing for a session channel or running
	// any remote SSH command (--takeover) so plain HTTP GET / cross-site
	// navigation can never cause side effects, and so channel-busy errors can
	// be delivered to the browser as a structured message.
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true, CompressionMode: websocket.CompressionDisabled})
	if err != nil {
		releaseKey()
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

	// Reserve a SessionPane slot from the unified SSH session ledger,
	// briefly queueing so transient background/panel churn does not reject
	// an interactive open unnecessarily.
	releaseChan, serr := sshx.AcquireSession(ctx, client, sshx.SessionPane, true, paneSlotWait)
	if serr != nil {
		releaseKey()
		if errors.Is(serr, sshx.ErrBusy) {
			_ = writeText(map[string]any{"t": "error", "code": "ssh_channels_busy"})
			c.Close(statusTooManyViewers, "ssh_channels_busy")
		}
		return
	}
	var releaseOnce sync.Once
	releaseSlot := func() {
		releaseOnce.Do(func() {
			releaseChan()
			releaseKey()
		})
	}

	// Open the SSH session channel directly (rather than ssh.Session) so we
	// observe the channel's incoming request stream (reqs). reqs closes ONLY
	// once the remote peer replies SSH_MSG_CHANNEL_CLOSE or the underlying
	// *ssh.Client transport ends, ensuring every success, failure, and timeout
	// path holds its ledger slot until the remote channel has really ended.
	type setupRes struct {
		ch      ssh.Channel
		reqDone <-chan int
		err     error
	}
	setupDone := make(chan setupRes, 1)
	setupCtx, setupCancel := context.WithTimeout(ctx, paneSetupTimeout)
	defer setupCancel()

	// abandonChannel tears down an already-opened channel in the background and
	// releases the ledger slot + pane key ONLY once reqs closes. If a host never
	// replies SSH_MSG_CHANNEL_CLOSE, the slot intentionally stays occupied so
	// repeated reconnects cannot exceed MaxSessionChannels or MaxPanePerConn;
	// when the owning term.Session ends (user close, detach timeout, connection
	// loss, or gateway shutdown), term.Session.end calls s.client.Close(), which
	// tears down the SSH transport, closes reqs, and reclaims the slot.
	abandonChannel := func(ch ssh.Channel, reqDone <-chan int, sendHUP bool) {
		go func() {
			if sendHUP {
				_ = ch.CloseWrite()
				_, _ = ch.SendRequest("signal", false, ssh.Marshal(&struct{ Signal string }{string(ssh.SIGHUP)}))
			}
			_ = ch.Close()
			var drainWG sync.WaitGroup
			drainWG.Add(2)
			go func() { defer drainWG.Done(); _, _ = io.Copy(io.Discard, ch) }()
			go func() { defer drainWG.Done(); _, _ = io.Copy(io.Discard, ch.Stderr()) }()
			<-reqDone
			drainWG.Wait()
			releaseSlot()
		}()
	}

	go func() {
		defer close(setupDone)
		ch, reqs, serr := client.OpenChannel("session", nil)
		if serr != nil {
			// Channel open failed or was rejected by the peer; no remote channel
			// exists, so releasing the slot immediately is safe.
			releaseSlot()
			setupDone <- setupRes{err: serr}
			return
		}
		reqDone := make(chan int, 1)
		go func() {
			exitStatus := -1
			exitSignal := ""
			for req := range reqs {
				switch req.Type {
				case "exit-status":
					if len(req.Payload) >= 4 {
						exitStatus = int(binary.BigEndian.Uint32(req.Payload))
					}
				case "exit-signal":
					var sigval struct {
						Signal     string
						CoreDumped bool
						Error      string
						Lang       string
					}
					if ssh.Unmarshal(req.Payload, &sigval) == nil {
						exitSignal = sigval.Signal
					}
				}
				if req.WantReply {
					_ = req.Reply(false, nil)
				}
			}
			// Match golang.org/x/crypto/ssh.Session.wait semantics:
			// exit-status takes precedence over exit-signal; when only
			// exit-signal was sent, map to 128 + signal number (or 128 if
			// unknown); when neither arrived, report -1 (mapped to 1).
			exitCode := exitStatus
			if exitCode < 0 && exitSignal != "" {
				exitCode = 128
				if num, ok := paneSSHSignals[ssh.Signal(exitSignal)]; ok {
					exitCode += num
				}
			}
			reqDone <- exitCode
			close(reqDone)
		}()

		stopEarly := context.AfterFunc(setupCtx, func() { _ = ch.Close() })
		defer stopEarly()

		modes := ssh.TerminalModes{
			ssh.ECHO:          1,
			ssh.TTY_OP_ISPEED: 14400,
			ssh.TTY_OP_OSPEED: 14400,
		}
		ptyReq := panePTYRequestMsg{
			Term:     "xterm-256color",
			Columns:  uint32(cols),
			Rows:     uint32(rows),
			Width:    uint32(cols * 8),
			Height:   uint32(rows * 8),
			Modelist: encodeTerminalModes(modes),
		}
		ptyOK, serr := ch.SendRequest("pty-req", true, ssh.Marshal(&ptyReq))
		if serr == nil && !ptyOK {
			serr = errors.New("ssh: pty-req failed")
		}
		if serr != nil {
			abandonChannel(ch, reqDone, false)
			setupDone <- setupRes{err: serr}
			return
		}
		execOK, serr := ch.SendRequest("exec", true, ssh.Marshal(&struct{ Command string }{herdrTerminalAttachCommand(socket, hp.TerminalID)}))
		if serr == nil && !execOK {
			serr = errors.New("ssh: exec failed")
		}
		if serr != nil {
			abandonChannel(ch, reqDone, false)
			setupDone <- setupRes{err: serr}
			return
		}
		if !stopEarly() {
			// Setup context already fired while exec was completing.
			abandonChannel(ch, reqDone, true)
			setupDone <- setupRes{err: setupCtx.Err()}
			return
		}
		setupDone <- setupRes{ch: ch, reqDone: reqDone}
	}()

	var res setupRes
	select {
	case res = <-setupDone:
	case <-setupCtx.Done():
		// Drain late setup result; if setup succeeded right as timeout fired,
		// tear down the channel and release the slot only once reqs closes.
		// (If setup failed, the setup goroutine already called releaseSlot or
		// abandonChannel.)
		go func() {
			if late := <-setupDone; late.ch != nil {
				abandonChannel(late.ch, late.reqDone, true)
			}
		}()
		_ = writeText(map[string]any{"t": "exit", "code": 1, "bytes": 0})
		return
	}
	if res.err != nil {
		// Setup goroutine already released the slot (if OpenChannel failed) or
		// handed it to abandonChannel to release once reqs closes.
		_ = writeText(map[string]any{"t": "exit", "code": 1, "bytes": 0})
		return
	}

	ch, reqDone := res.ch, res.reqDone
	var closeOnce sync.Once
	closeSSH := func() {
		closeOnce.Do(func() {
			_ = ch.CloseWrite()
			_, _ = ch.SendRequest("signal", false, ssh.Marshal(&struct{ Signal string }{string(ssh.SIGHUP)}))
			_ = ch.Close()
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
					_, _ = io.Copy(io.Discard, rd)
					return
				}
				sentBytes.Add(int64(n))
				if werr := writeBin(buf[:n]); werr != nil {
					cancel()
					_, _ = io.Copy(io.Discard, rd)
					return
				}
			}
			if rerr != nil {
				return
			}
		}
	}
	outWG.Add(2)
	go pump(ch)
	go pump(ch.Stderr())

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

	// Wait for remote channel close; release the per-client slot ONLY after
	// reqs has closed (peer replied CHANNEL_CLOSE or transport ended) and both
	// output pumps have exited.
	go func() {
		defer releaseSlot()
		exitCode := <-reqDone
		outWG.Wait()
		code := exitCode
		if code < 0 {
			code = 1
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
				if _, werr := ch.Write(data); werr != nil {
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
			wreq := paneWindowChangeMsg{
				Columns: uint32(nc),
				Rows:    uint32(nr),
				Width:   uint32(nc * 8),
				Height:  uint32(nr * 8),
			}
			_, _ = ch.SendRequest("window-change", false, ssh.Marshal(&wreq))
		case "ack":
			flow.ack(msg.N)
		case "ping":
			_ = writeText(map[string]any{"t": "pong"})
		}
	}
}
