package sshx

import (
	"context"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
)

// SessionClass identifies the category of an SSH "session" channel.
// OpenSSH's MaxSessions (default 10) caps all "session" channels on a single
// connection regardless of whether they run a PTY shell, an exec command, an
// SFTP subsystem, or a direct single-pane attach. Forwarded channels
// ("direct-tcpip" and "direct-streamlocal@openssh.com") do not consume
// MaxSessions and are NOT accounted here.
type SessionClass int

const (
	// SessionTerminal is the main interactive PTY terminal of a Corral session.
	SessionTerminal SessionClass = iota
	// SessionAux is an auxiliary exec channel (background scans or user actions).
	SessionAux
	// SessionSFTP is an SFTP subsystem channel (file panel or file transfers).
	SessionSFTP
	// SessionPane is a direct single-pane live terminal attach.
	SessionPane

	sessionClassCount
)

const (
	// DefaultMaxSessionChannels leaves 1 headroom below OpenSSH's default MaxSessions (10).
	DefaultMaxSessionChannels = 9
	// DefaultTerminalReserve keeps 1 slot exclusively for SessionTerminal so other
	// classes can never lock out the main terminal on a connection.
	DefaultTerminalReserve = 1
	// DefaultInteractiveReserve keeps headroom for user-triggered interactive
	// operations (file panel, live pane terminal, interactive exec) so background
	// scans cannot fill the connection to capacity.
	DefaultInteractiveReserve = 2
	// MinMaxSessionChannels is the smallest per-connection cap that still allows
	// a background operation on an empty connection without violating
	// DefaultTerminalReserve and DefaultInteractiveReserve (1 + 1 + 2 = 4).
	MinMaxSessionChannels = 1 + DefaultTerminalReserve + DefaultInteractiveReserve
)

var (
	// MaxSessionChannels bounds the total unfinished SSH "session" channels across
	// all classes on one *ssh.Client. Configurable via CORRAL_SSH_MAX_SESSIONS.
	MaxSessionChannels = DefaultMaxSessionChannels

	// Per-class caps on one *ssh.Client.
	MaxTerminalPerConn = 1
	MaxSFTPPerConn     = 4
	MaxPanePerConn     = 4

	// Reserves within MaxSessionChannels.
	TerminalReserve    = DefaultTerminalReserve
	InteractiveReserve = DefaultInteractiveReserve
)

type clientSessionState struct {
	total   int
	byClass [sessionClassCount]int
	changed chan struct{}
}

type sessionLedger struct {
	mu      sync.Mutex
	clients map[*ssh.Client]*clientSessionState
}

var defaultSessionLedger = sessionLedger{
	clients: map[*ssh.Client]*clientSessionState{},
}

func sessionClassLimit(class SessionClass) int {
	switch class {
	case SessionTerminal:
		return MaxTerminalPerConn
	case SessionAux:
		return MaxAuxPerConn
	case SessionSFTP:
		return MaxSFTPPerConn
	case SessionPane:
		return MaxPanePerConn
	default:
		return 0
	}
}

func (l *sessionLedger) canTakeLocked(st *clientSessionState, class SessionClass, interactive bool) bool {
	totalLimit := MaxSessionChannels
	if totalLimit <= 0 {
		return false
	}
	clsLimit := sessionClassLimit(class)
	if clsLimit <= 0 {
		return false
	}
	curTotal, curClass, curTerm := 0, 0, 0
	if st != nil {
		curTotal = st.total
		curClass = st.byClass[class]
		curTerm = st.byClass[SessionTerminal]
	}
	if curClass >= clsLimit {
		return false
	}
	if curTotal+1 > totalLimit {
		return false
	}
	// Non-terminal classes must always leave TerminalReserve slots for
	// SessionTerminal whenever fewer than TerminalReserve terminals are held.
	termReserve := 0
	if class != SessionTerminal && TerminalReserve > curTerm {
		termReserve = TerminalReserve - curTerm
	}
	if curTotal+1+termReserve > totalLimit {
		return false
	}
	// Background operations (non-terminal, non-interactive) must leave
	// InteractiveReserve slots free so user-clicked actions are not starved.
	if class != SessionTerminal && !interactive && InteractiveReserve > 0 {
		if curTotal+1+termReserve+InteractiveReserve > totalLimit {
			return false
		}
	}
	return true
}

func (l *sessionLedger) tryTake(c *ssh.Client, class SessionClass, interactive bool) (release func(), waitCh <-chan struct{}, ok bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if class < 0 || class >= sessionClassCount {
		return nil, nil, false
	}
	st := l.clients[c]
	if !l.canTakeLocked(st, class, interactive) {
		if st != nil {
			return nil, st.changed, false
		}
		return nil, nil, false
	}
	if st == nil {
		st = &clientSessionState{changed: make(chan struct{})}
		l.clients[c] = st
	}
	st.total++
	st.byClass[class]++
	return oneShot(func() {
		l.release(c, class)
	}), nil, true
}

func (l *sessionLedger) release(c *ssh.Client, class SessionClass) {
	l.mu.Lock()
	defer l.mu.Unlock()
	st := l.clients[c]
	if st == nil {
		return
	}
	if st.byClass[class] > 0 {
		st.byClass[class]--
	}
	if st.total > 0 {
		st.total--
	}
	close(st.changed)
	if st.total <= 0 {
		delete(l.clients, c)
		return
	}
	st.changed = make(chan struct{})
}

func (l *sessionLedger) inUse(c *ssh.Client) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	if st := l.clients[c]; st != nil {
		return st.total
	}
	return 0
}

func (l *sessionLedger) classInUse(c *ssh.Client, class SessionClass) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	if class < 0 || class >= sessionClassCount {
		return 0
	}
	if st := l.clients[c]; st != nil {
		return st.byClass[class]
	}
	return 0
}

// AcquireSession reserves one SSH "session" channel slot on c from the unified
// per-client ledger. The caller must hold the returned release function until
// the remote channel has really ended (or failed to open); release is safe to
// call multiple times.
//
// interactive controls whether the caller may draw from InteractiveReserve
// (user-triggered actions pass true; background polling/pre-warming/scanning
// pass false and must leave InteractiveReserve slots free).
//
// When no slot is immediately available under the caller's priority rules:
//   - If await <= 0, AcquireSession returns ErrBusy at once.
//   - If await > 0, AcquireSession waits up to await for a slot on c to be
//     released that satisfies the caller's class and interactive priority
//     (a non-interactive waiter never steals InteractiveReserve), returning
//     ErrBusy on timeout or ctx.Err() if ctx is cancelled first.
func AcquireSession(ctx context.Context, c *ssh.Client, class SessionClass, interactive bool, await time.Duration) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	rel, waitCh, ok := defaultSessionLedger.tryTake(c, class, interactive)
	if ok {
		return rel, nil
	}
	if await <= 0 || waitCh == nil {
		return nil, ErrBusy
	}
	timer := time.NewTimer(await)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-timer.C:
			return nil, ErrBusy
		case <-waitCh:
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		rel, waitCh, ok = defaultSessionLedger.tryTake(c, class, interactive)
		if ok {
			return rel, nil
		}
		if waitCh == nil {
			return nil, ErrBusy
		}
	}
}

// SessionInUse reports the total unfinished session channels accounted on c.
func SessionInUse(c *ssh.Client) int {
	return defaultSessionLedger.inUse(c)
}

// SessionClassInUse reports the unfinished session channels of class accounted on c.
func SessionClassInUse(c *ssh.Client, class SessionClass) int {
	return defaultSessionLedger.classInUse(c, class)
}
