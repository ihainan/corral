package term

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/ihainan/corral/internal/sshx"
)

var (
	ErrNotFound = errors.New("session not found")
	ErrTooMany  = errors.New("too many open sessions")
)

const (
	setupTimeout = 20 * time.Second // PTY + shell request after the SSH handshake
	exitedLinger = 5 * time.Minute  // ended sessions stay listed so a returning tab sees why
	keptEndedMax = 10               // ended sessions kept per user
)

type Manager struct {
	mu            sync.Mutex
	sessions      map[string]*Session
	pending       map[int64]int // reserved slots per user (dialing, not yet registered)
	detachTimeout time.Duration
	maxPerUser    int
	OnEnd         func(s *Session, state string, duration time.Duration)
	OnAlert       func(s *Session, a Alert) // bells and notification escapes in output, called from one worker
	alerts        chan *Session             // sessions with an alert waiting
	spillMu       sync.Mutex
	spilled       []*Session // sessions whose alert did not fit in alerts; each at most once
	stop          chan struct{}
}

// alertQueue bounds sessions waiting for the alert worker; alertRetry is how
// often the worker picks up those that did not fit.
var (
	alertQueue = 256
	alertRetry = time.Second
)

func NewManager(detachTimeout time.Duration, maxPerUser int) *Manager {
	m := &Manager{sessions: map[string]*Session{}, pending: map[int64]int{}, detachTimeout: detachTimeout,
		maxPerUser: maxPerUser, alerts: make(chan *Session, alertQueue), stop: make(chan struct{})}
	go m.reap()
	go m.alertWorker(alertRetry)
	return m
}

// alertWorker delivers terminal alerts one at a time.
func (m *Manager) alertWorker(retry time.Duration) {
	t := time.NewTicker(retry)
	defer t.Stop()
	deliver := func(s *Session) {
		if a := s.takeAlert(); a != nil && s.onAlert != nil {
			s.onAlert(s, *a)
		}
	}
	for {
		select {
		case <-m.stop:
			return
		case s := <-m.alerts:
			deliver(s)
		case <-t.C:
			m.spillMu.Lock()
			spilled := m.spilled
			m.spilled = nil
			m.spillMu.Unlock()
			for _, s := range spilled {
				deliver(s)
			}
		}
	}
}

func (m *Manager) spill(s *Session) {
	m.spillMu.Lock()
	m.spilled = append(m.spilled, s)
	m.spillMu.Unlock()
}

func (m *Manager) Shutdown() {
	select {
	case <-m.stop:
	default:
		close(m.stop)
	}
	m.mu.Lock()
	all := make([]*Session, 0, len(m.sessions))
	for _, s := range m.sessions {
		all = append(all, s)
	}
	m.mu.Unlock()
	for _, s := range all {
		s.Close("server shutting down")
	}
}

// StartOptions describes what to run once the SSH connection is up.
type StartOptions struct {
	Cols, Rows   int
	Persistence  string // "none", "tmux" or "herdr"
	TmuxSession  string // validated by the caller: [A-Za-z0-9_-]{1,64}
	HerdrSession string // validated by the caller; "" attaches herdr's default session
	// HostConfig fingerprints the host settings the connection was made with,
	// so a session is only reused while the host still describes it.
	HostConfig string
}

// Reservation holds one of a user's session slots while a connection is being
// set up, so concurrent requests cannot exceed the limit.
type Reservation struct {
	m     *Manager
	owner int64
	once  sync.Once
}

func (r *Reservation) Release() {
	r.once.Do(func() {
		r.m.mu.Lock()
		r.m.pending[r.owner]--
		if r.m.pending[r.owner] <= 0 {
			delete(r.m.pending, r.owner)
		}
		r.m.mu.Unlock()
	})
}

// Reserve claims a slot before dialing; call Release on every exit path
// (Start releases it itself once the session is registered or has failed).
func (m *Manager) Reserve(owner int64) (*Reservation, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.countRunning(owner)+m.pending[owner] >= m.maxPerUser {
		return nil, ErrTooMany
	}
	m.pending[owner]++
	return &Reservation{m: m, owner: owner}, nil
}

func (m *Manager) countRunning(owner int64) int {
	n := 0
	for _, s := range m.sessions {
		if s.OwnerID == owner && s.State() == StateRunning {
			n++
		}
	}
	return n
}

// Start takes ownership of an established SSH client and runs the shell. The
// client is closed on every failure path.
// Cancelling ctx (access revoked, request gone) aborts setup before anything
// runs on the host.
func (m *Manager) Start(ctx context.Context, res *Reservation, owner, hostID int64, hostName string, client *ssh.Client, opt StartOptions) (*Session, error) {
	defer res.Release()
	cols, rows := clamp(opt.Cols, 20, 1000, 80), clamp(opt.Rows, 5, 500, 24)

	type setup struct {
		sess *ssh.Session
		s    *Session
		err  error
	}
	done := make(chan setup, 1)
	go func() {
		sess, err := client.NewSession()
		if err != nil {
			done <- setup{err: err}
			return
		}
		fail := func(err error) { sess.Close(); done <- setup{err: err} }
		modes := ssh.TerminalModes{ssh.ECHO: 1, ssh.TTY_OP_ISPEED: 115200, ssh.TTY_OP_OSPEED: 115200}
		_ = sess.Setenv("COLORTERM", "truecolor") // servers often refuse custom env; that is fine
		if err := sess.RequestPty("xterm-256color", rows, cols, modes); err != nil {
			fail(fmt.Errorf("cannot allocate terminal: %w", err))
			return
		}
		stdin, err := sess.StdinPipe()
		if err != nil {
			fail(err)
			return
		}
		stdout, err := sess.StdoutPipe()
		if err != nil {
			fail(err)
			return
		}
		stderr, err := sess.StderrPipe()
		if err != nil {
			fail(err)
			return
		}
		if ctx.Err() != nil {
			fail(ctx.Err())
			return
		}
		switch opt.Persistence {
		case "tmux":
			err = sess.Start(TmuxCommand(opt.TmuxSession))
		case "herdr":
			err = sess.Start(HerdrCommand(opt.HerdrSession))
		default:
			err = sess.Shell()
		}
		if err != nil {
			fail(fmt.Errorf("cannot start shell: %w", err))
			return
		}
		s := &Session{
			ID: newID(), OwnerID: owner, HostID: hostID, HostName: hostName, Created: time.Now(), HostConfig: opt.HostConfig,
			client: client, ssh: sess, stdin: stdin, input: make(chan []byte, inputQueue),
			buf: newRing(ringSize), subs: map[*Sub]struct{}{}, state: StateRunning,
			cols: cols, rows: rows, idleSince: time.Now(), finished: make(chan struct{}), uploads: make(chan struct{}, 2),
			onAlert: m.OnAlert, alertq: m.alerts, spill: m.spill,
		}
		s.pumps.Add(2)
		go s.pump(stdout)
		go s.pump(stderr)
		done <- setup{sess: sess, s: s}
	}()

	var st setup
	select {
	case st = <-done:
	case <-ctx.Done():
		client.Close() // unblocks the setup goroutine
		<-done
		return nil, ctx.Err()
	case <-time.After(setupTimeout):
		client.Close()
		<-done
		return nil, errors.New("the host did not start a terminal in time")
	}
	if st.err != nil {
		client.Close()
		return nil, st.err
	}
	s := st.s
	s.onEnd = func(s *Session) {
		m.trimEnded(s.OwnerID)
		if m.OnEnd != nil {
			m.OnEnd(s, s.State(), time.Since(s.Created))
		}
	}

	m.mu.Lock()
	m.sessions[s.ID] = s
	m.mu.Unlock()

	go s.writer()
	// Both the program exiting and the connection dropping end the session.
	// Either way, remaining output is read first, and an exit status that
	// arrived wins over "connection lost".
	exited := make(chan struct{})
	go func() {
		err := st.sess.Wait()
		var exit *ssh.ExitError
		switch {
		case err == nil:
			s.waitPumps(3 * time.Second)
			s.end(StateExited, 0, "")
		case errors.As(err, &exit):
			s.waitPumps(3 * time.Second)
			s.end(StateExited, exit.ExitStatus(), "")
		default:
			s.waitPumps(3 * time.Second)
			s.end(StateLost, -1, "connection lost")
		}
		close(exited)
	}()
	go func() {
		_ = client.Wait()
		s.waitPumps(3 * time.Second)
		select {
		case <-exited:
		case <-time.After(500 * time.Millisecond):
		}
		s.end(StateLost, -1, "connection lost")
	}()
	return s, nil
}

// TmuxCommand attaches to (or creates) a named tmux session, falling back to a
// login shell when tmux is missing. The script is fixed text; the only variable
// part is the session name, which is validated and shell-quoted.
func TmuxCommand(name string) string {
	script := `PATH="$HOME/.local/bin:$HOME/bin:/opt/homebrew/bin:/usr/local/bin:$PATH"; ` +
		`if command -v tmux >/dev/null 2>&1; then exec tmux -u new-session -A -s "$1"; ` +
		`else printf '\r\n[corral] tmux is not installed on this host; starting a plain shell.\r\n'; exec "${SHELL:-/bin/sh}" -l; fi`
	return "sh -c " + sshx.QuoteShell(script) + " corral " + sshx.QuoteShell(name)
}

// HerdrCommand attaches to herdr (its default session, or a named one),
// falling back to a login shell when herdr is missing. Fixed script; the only
// variable part is the validated, quoted session name.
func HerdrCommand(name string) string {
	script := `PATH="$HOME/.local/bin:$HOME/bin:$HOME/.cargo/bin:/opt/homebrew/bin:/usr/local/bin:$PATH"; ` +
		`if command -v herdr >/dev/null 2>&1; then if [ -n "$1" ]; then exec herdr --session "$1"; else exec herdr; fi; ` +
		`else printf '\r\n[corral] herdr is not installed on this host; starting a plain shell.\r\n'; exec "${SHELL:-/bin/sh}" -l; fi`
	return "sh -c " + sshx.QuoteShell(script) + " corral " + sshx.QuoteShell(name)
}

// Get returns a session only if it belongs to owner.
func (m *Manager) Get(owner int64, id string) (*Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[id]
	if !ok || s.OwnerID != owner {
		return nil, ErrNotFound
	}
	return s, nil
}

func (m *Manager) List(owner int64) []Info {
	m.mu.Lock()
	var mine []*Session
	for _, s := range m.sessions {
		if s.OwnerID == owner {
			mine = append(mine, s)
		}
	}
	m.mu.Unlock()
	out := make([]Info, 0, len(mine))
	for _, s := range mine {
		out = append(out, s.Info())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt > out[j].CreatedAt })
	return out
}

// Running returns a user's live sessions to a host (used for side channels).
func (m *Manager) Running(owner, hostID int64) []*Session {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []*Session
	for _, s := range m.sessions {
		if s.OwnerID == owner && s.HostID == hostID && s.State() == StateRunning {
			out = append(out, s)
		}
	}
	return out
}

func (m *Manager) closeMatching(match func(*Session) bool, reason string) {
	m.mu.Lock()
	var hit []*Session
	for _, s := range m.sessions {
		if match(s) {
			hit = append(hit, s)
		}
	}
	m.mu.Unlock()
	// In parallel: one host that is slow to close must not hold up the others.
	var wg sync.WaitGroup
	for _, s := range hit {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.Close(reason)
		}()
	}
	wg.Wait()
}

// CloseUser ends every session a user owns, used when access is revoked.
func (m *Manager) CloseUser(owner int64, reason string) {
	m.closeMatching(func(s *Session) bool { return s.OwnerID == owner }, reason)
}

// CloseHost ends sessions to a host, used when the host entry is deleted.
func (m *Manager) CloseHost(owner, hostID int64, reason string) {
	m.closeMatching(func(s *Session) bool { return s.OwnerID == owner && s.HostID == hostID }, reason)
}

func (m *Manager) Remove(owner int64, id string) error {
	s, err := m.Get(owner, id)
	if err != nil {
		return err
	}
	s.Close("closed by user")
	m.mu.Lock()
	delete(m.sessions, id)
	m.mu.Unlock()
	return nil
}

// trimEnded keeps only the most recent ended sessions of a user.
func (m *Manager) trimEnded(owner int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var ended []*Session
	for _, s := range m.sessions {
		if s.OwnerID == owner && s.State() != StateRunning {
			ended = append(ended, s)
		}
	}
	if len(ended) <= keptEndedMax {
		return
	}
	sort.Slice(ended, func(i, j int) bool { return ended[i].endedAtTime().After(ended[j].endedAtTime()) })
	for _, s := range ended[keptEndedMax:] {
		delete(m.sessions, s.ID)
	}
}

func (s *Session) endedAtTime() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.endedAt
}

// reap closes sessions nobody has looked at for detachTimeout and forgets
// finished ones after a short grace period.
func (m *Manager) reap() {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-m.stop:
			return
		case <-t.C:
		}
		now := time.Now()
		var idle []*Session
		m.mu.Lock()
		for id, s := range m.sessions {
			s.mu.Lock()
			state, idleSince, endedAt := s.state, s.idleSince, s.endedAt
			s.mu.Unlock()
			if state != StateRunning {
				if now.Sub(endedAt) > exitedLinger {
					delete(m.sessions, id)
				}
				continue
			}
			if !idleSince.IsZero() && now.Sub(idleSince) > m.detachTimeout {
				idle = append(idle, s)
			}
		}
		m.mu.Unlock()
		for _, s := range idle {
			s.Close("no viewer for too long")
		}
	}
}

func newID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

func clamp(v, lo, hi, def int) int {
	if v == 0 {
		return def
	}
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
