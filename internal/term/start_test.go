package term

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/ihainan/corral/internal/testutil"
)

// startEnv: a manager, and a way to an SSH connection to a test server.
func startEnv(t *testing.T) (*Manager, *testutil.SSHServer, func() *ssh.Client) {
	t.Helper()
	srv := testutil.StartSSHServer(t, "u", "p", t.TempDir())
	m := NewManager(time.Hour, 5)
	dial := func() *ssh.Client {
		c, err := ssh.Dial("tcp", srv.Addr.String(), &ssh.ClientConfig{User: "u", Auth: []ssh.AuthMethod{ssh.Password("p")},
			HostKeyCallback: ssh.FixedHostKey(srv.HostKey), Timeout: 5 * time.Second})
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	return m, srv, dial
}

type ended struct {
	mu   sync.Mutex
	list []string // reason of each session ended
}

func (e *ended) hook(m *Manager) {
	m.OnEnd = func(s *Session, state string, d time.Duration) {
		e.mu.Lock()
		e.list = append(e.list, s.Snapshot().Reason)
		e.mu.Unlock()
	}
}

func (e *ended) reasons() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.list...)
}

// Registered before published: what keeps it sees it, the list does not
// yet; then it is published.
func TestStartRegistersBeforePublishing(t *testing.T) {
	m, _, dial := startEnv(t)
	res, _ := m.Reserve(1)
	var seen int
	s, err := m.Start(context.Background(), res, 1, 9, "h", dial(), StartOptions{Persistence: "none", Register: func(ctx context.Context, s *Session) error {
		seen = len(m.List(1))
		return nil
	}})
	if err != nil || s == nil {
		t.Fatal(err)
	}
	defer s.Close("test")
	if seen != 0 {
		t.Errorf("published before it was registered: %d listed", seen)
	}
	if len(m.List(1)) != 1 {
		t.Errorf("not published after: %+v", m.List(1))
	}
}

// Registering fails: nothing published, no end (nothing was kept).
func TestStartRegisterFails(t *testing.T) {
	m, _, dial := startEnv(t)
	var e ended
	e.hook(m)
	res, _ := m.Reserve(1)
	_, err := m.Start(context.Background(), res, 1, 9, "h", dial(), StartOptions{Persistence: "none", Register: func(context.Context, *Session) error {
		return errors.New("no")
	}})
	if err == nil || len(m.List(1)) != 0 || len(e.reasons()) != 0 {
		t.Errorf("err %v, listed %d, ended %v", err, len(m.List(1)), e.reasons())
	}
}

// The gateway begins to stop while a Start registers: not published, ended
// as the gateway's stop (what keeps it, kept); Start and Reserve refuse after.
func TestStartStoppingAfterRegister(t *testing.T) {
	m, _, dial := startEnv(t)
	var e ended
	e.hook(m)
	res, _ := m.Reserve(1)
	_, err := m.Start(context.Background(), res, 1, 9, "h", dial(), StartOptions{Persistence: "none", Register: func(context.Context, *Session) error {
		m.BeginShutdown()
		return nil
	}})
	if !errors.Is(err, ErrClosing) || len(m.List(1)) != 0 {
		t.Fatalf("err %v, listed %d", err, len(m.List(1)))
	}
	if r := e.reasons(); len(r) != 1 || r[0] != ReasonShutdown {
		t.Errorf("ended as %v", r)
	}
	if _, err := m.Reserve(1); !errors.Is(err, ErrClosing) {
		t.Errorf("reserve after: %v", err)
	}
	if _, err := m.Start(context.Background(), &Reservation{m: m, owner: 1}, 1, 9, "h", dial(), StartOptions{Persistence: "none"}); !errors.Is(err, ErrClosing) {
		t.Errorf("start after: %v", err)
	}
}

// The request given up once registered: not published, ended as given up
// (what keeps it, gone) — not as the gateway's stop.
func TestStartGivenUpAfterRegister(t *testing.T) {
	m, _, dial := startEnv(t)
	var e ended
	e.hook(m)
	ctx, cancel := context.WithCancel(context.Background())
	res, _ := m.Reserve(1)
	_, err := m.Start(ctx, res, 1, 9, "h", dial(), StartOptions{Persistence: "none", Register: func(context.Context, *Session) error {
		cancel()
		return nil
	}})
	if err == nil || len(m.List(1)) != 0 {
		t.Fatalf("err %v, listed %d", err, len(m.List(1)))
	}
	if r := e.reasons(); len(r) != 1 || r[0] == ReasonShutdown {
		t.Errorf("ended as %v", r)
	}
}

// PreExec refuses: nothing is run on the host.
func TestStartPreExecRefuses(t *testing.T) {
	m, srv, dial := startEnv(t)
	res, _ := m.Reserve(1)
	_, err := m.Start(context.Background(), res, 1, 9, "h", dial(), StartOptions{Persistence: "tmux", TmuxSession: "s", PreExec: func(context.Context) error {
		return errors.New("changed")
	}})
	if err == nil {
		t.Fatal("started")
	}
	time.Sleep(100 * time.Millisecond)
	srv.HandleExec(nil)
	if n := len(srv.Execs); n != 0 {
		t.Errorf("ran %d commands: %v", n, srv.Execs)
	}
}

// Attaching only: tmux's exact attach, no shell instead; herdr's with no shell.
func TestAttachOnlyCommands(t *testing.T) {
	tm := TmuxAttachCommand("main")
	if !strings.Contains(tm, `attach-session -t "=$1"`) || strings.Contains(tm, "new-session") || strings.Contains(tm, "SHELL") {
		t.Errorf("tmux: %s", tm)
	}
	hd := HerdrAttachCommand("")
	if strings.Contains(hd, "SHELL") || !strings.Contains(hd, "exit 127") {
		t.Errorf("herdr: %s", hd)
	}
	m, srv, dial := startEnv(t)
	res, _ := m.Reserve(1)
	s, err := m.Start(context.Background(), res, 1, 9, "h", dial(), StartOptions{Persistence: "tmux", TmuxSession: "main", AttachOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close("test")
	time.Sleep(100 * time.Millisecond)
	srv.HandleExec(nil)
	if len(srv.Execs) != 1 || !strings.Contains(srv.Execs[0], "attach-session") {
		t.Errorf("ran %v", srv.Execs)
	}
}

// WaitEnds waits for every onEnd under way, of a session another goroutine
// ended too; it does not return while one is held.
func TestWaitEnds(t *testing.T) {
	m, _, dial := startEnv(t)
	hold := make(chan struct{})
	m.OnEnd = func(*Session, string, time.Duration) { <-hold }
	res, _ := m.Reserve(1)
	s, err := m.Start(context.Background(), res, 1, 9, "h", dial(), StartOptions{Persistence: "none"})
	if err != nil {
		t.Fatal(err)
	}
	go s.Close("elsewhere")
	time.Sleep(50 * time.Millisecond)
	m.Shutdown() // (its Close returns at once: the session ended already)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if m.WaitEnds(ctx) {
		t.Fatal("waited past an onEnd still held")
	}
	close(hold)
	ctx2, cancel2 := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel2()
	if !m.WaitEnds(ctx2) {
		t.Fatal("did not see it end")
	}
	if !m.WaitStarts(ctx2) {
		t.Fatal("starts")
	}
}

// A session opened again keeps its unattended time.
func TestStartIdleSince(t *testing.T) {
	m, _, dial := startEnv(t)
	at := time.Now().Add(-time.Hour).Truncate(time.Second)
	res, _ := m.Reserve(1)
	s, err := m.Start(context.Background(), res, 1, 9, "h", dial(), StartOptions{Persistence: "none", IdleSince: at})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close("test")
	if got := s.Snapshot().IdleSince; !got.Equal(at) {
		t.Errorf("idle since %v, want %v", got, at)
	}
}
