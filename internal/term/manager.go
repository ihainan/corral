package term

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"sort"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/ihainan/corral/internal/sshx"
)

var (
	ErrNotFound    = errors.New("session not found")
	ErrTooMany     = errors.New("too many open sessions")
)

type Manager struct {
	mu            sync.Mutex
	sessions      map[string]*Session
	detachTimeout time.Duration
	maxPerUser    int
	OnEnd         func(s *Session, state string, duration time.Duration)
	stop          chan struct{}
}

func NewManager(detachTimeout time.Duration, maxPerUser int) *Manager {
	m := &Manager{sessions: map[string]*Session{}, detachTimeout: detachTimeout, maxPerUser: maxPerUser, stop: make(chan struct{})}
	go m.reap()
	return m
}

func (m *Manager) Shutdown() {
	close(m.stop)
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
	Cols, Rows  int
	Persistence string // "none" or "tmux"
	TmuxSession string // validated by the caller: [A-Za-z0-9._-]{1,64}
}

// Reserve checks the per-user limit before dialing, so a burst of requests
// cannot open more SSH connections than allowed.
func (m *Manager) countFor(owner int64) int {
	n := 0
	for _, s := range m.sessions {
		if s.OwnerID == owner && s.State() == StateRunning {
			n++
		}
	}
	return n
}

// Start takes ownership of an established SSH client and runs the shell.
func (m *Manager) Start(owner, hostID int64, hostName string, client *ssh.Client, opt StartOptions) (*Session, error) {
	m.mu.Lock()
	if m.countFor(owner) >= m.maxPerUser {
		m.mu.Unlock()
		client.Close()
		return nil, ErrTooMany
	}
	m.mu.Unlock()

	sess, err := client.NewSession()
	if err != nil {
		client.Close()
		return nil, err
	}
	cols, rows := clamp(opt.Cols, 20, 1000, 80), clamp(opt.Rows, 5, 500, 24)
	modes := ssh.TerminalModes{ssh.ECHO: 1, ssh.TTY_OP_ISPEED: 115200, ssh.TTY_OP_OSPEED: 115200}
	_ = sess.Setenv("COLORTERM", "truecolor") // servers often refuse custom env; that is fine
	if err := sess.RequestPty("xterm-256color", rows, cols, modes); err != nil {
		sess.Close()
		client.Close()
		return nil, fmt.Errorf("cannot allocate terminal: %w", err)
	}
	stdin, err := sess.StdinPipe()
	if err != nil {
		sess.Close()
		client.Close()
		return nil, err
	}
	stdout, err := sess.StdoutPipe()
	if err != nil {
		sess.Close()
		client.Close()
		return nil, err
	}
	stderr, err := sess.StderrPipe()
	if err != nil {
		sess.Close()
		client.Close()
		return nil, err
	}

	switch opt.Persistence {
	case "tmux":
		err = sess.Start(TmuxCommand(opt.TmuxSession))
	default:
		err = sess.Shell()
	}
	if err != nil {
		sess.Close()
		client.Close()
		return nil, fmt.Errorf("cannot start shell: %w", err)
	}

	s := &Session{
		ID: newID(), OwnerID: owner, HostID: hostID, HostName: hostName, Created: time.Now(),
		client: client, ssh: sess, stdin: stdin,
		buf: newRing(ringSize), subs: map[*Sub]struct{}{}, state: StateRunning,
		cols: cols, rows: rows, idleSince: time.Now(),
	}
	s.onEnd = func(s *Session) {
		if m.OnEnd != nil {
			m.OnEnd(s, s.State(), time.Since(s.Created))
		}
	}

	m.mu.Lock()
	m.sessions[s.ID] = s
	m.mu.Unlock()

	go s.pump(io.MultiReader(stdout))
	go s.pump(stderr)
	go func() {
		err := sess.Wait()
		var exit *ssh.ExitError
		switch {
		case err == nil:
			s.end(StateExited, 0, "")
		case errors.As(err, &exit):
			s.end(StateExited, exit.ExitStatus(), "")
		default:
			s.end(StateLost, -1, "connection lost")
		}
	}()
	go func() {
		// The whole connection dropping (network, keepalive failure) ends the session too.
		_ = client.Wait()
		s.end(StateLost, -1, "connection lost")
	}()
	return s, nil
}

// TmuxCommand attaches to (or creates) a named tmux session, falling back to a
// login shell when tmux is missing. The script is fixed text; the only variable
// part is the session name, which is validated and shell-quoted.
func TmuxCommand(name string) string {
	script := `if command -v tmux >/dev/null 2>&1; then exec tmux -u new-session -A -s "$1"; ` +
		`else printf '\r\n[corral] tmux is not installed on this host; starting a plain shell.\r\n'; exec "${SHELL:-/bin/sh}" -l; fi`
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

// CloseUser ends every session a user owns, used when access is revoked.
func (m *Manager) CloseUser(owner int64, reason string) {
	m.mu.Lock()
	var mine []*Session
	for _, s := range m.sessions {
		if s.OwnerID == owner {
			mine = append(mine, s)
		}
	}
	m.mu.Unlock()
	for _, s := range mine {
		s.Close(reason)
	}
}

// CloseHost ends sessions to a host, used when the host entry is deleted.
func (m *Manager) CloseHost(owner, hostID int64, reason string) {
	m.mu.Lock()
	var hit []*Session
	for _, s := range m.sessions {
		if s.OwnerID == owner && s.HostID == hostID {
			hit = append(hit, s)
		}
	}
	m.mu.Unlock()
	for _, s := range hit {
		s.Close(reason)
	}
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

// reap closes sessions nobody has looked at for detachTimeout and forgets
// finished ones after a short grace period (so a returning tab can see why).
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
