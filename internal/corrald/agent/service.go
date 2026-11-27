//go:build linux

package agent

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/ihainan/corral/internal/corrald/daemon"
	"github.com/ihainan/corral/internal/corrald/proto"
)

type connWatchState struct {
	mu     sync.Mutex
	active bool
	stopCh chan struct{}
}

// Service wires Locator, PathValidator, Tails, MessageTails, and Watcher into
// a corrald daemon.Server.
type Service struct {
	srv     *daemon.Server
	locator *Locator
	val     *PathValidator
	watcher *Watcher

	watchMu    sync.Mutex
	watchConns map[*daemon.Conn]*connWatchState
}

// ServiceOptions overrides default host paths or watcher parameters (mainly for
// tests).
type ServiceOptions struct {
	LocatorOpts  LocatorOptions
	MaxWatches   int
	PollInterval time.Duration
}

// Attach constructs a default agent Service for srv and registers its RPC
// methods, health checks, state provider, and shutdown hook.
func Attach(srv *daemon.Server) {
	_, _ = Register(srv, ServiceOptions{})
}

// Register constructs an agent Service with custom options and attaches it to
// srv.
func Register(srv *daemon.Server, opts ServiceOptions) (*Service, error) {
	layout := srv.Layout()
	locOpts := opts.LocatorOpts
	if locOpts.Home == "" && layout != nil {
		locOpts.Home = layout.Home
	}
	if locOpts.UID == nil && layout != nil {
		u := layout.UID
		locOpts.UID = &u
	}

	valUID := uint32(0)
	if locOpts.UID != nil {
		valUID = *locOpts.UID
	}
	if locOpts.Tmux == nil {
		locOpts.Tmux = NewTmuxExecutorWithPath(locOpts.Home, srv.TmuxPath(), valUID)
	}
	val := NewPathValidator(locOpts.Home, valUID, locOpts.ProcRoot)

	var svc *Service
	userOnDiscovered := locOpts.OnDiscovered
	locOpts.OnDiscovered = func(scope string, logs, claudeDirs, codexHomes []string) {
		val.UpdateDiscovered(scope, logs, claudeDirs, codexHomes)
		if svc != nil && svc.watcher != nil {
			svc.watcher.NotifyDiscovered()
		}
		if userOnDiscovered != nil {
			userOnDiscovered(scope, logs, claudeDirs, codexHomes)
		}
	}

	loc, err := NewLocator(locOpts)
	if err != nil {
		return nil, err
	}
	val.SetEnvDirs(loc.selfClaudeConfigDir(), loc.xdgDataHome())

	watcher := NewWatcher(WatchWatcherOptions{
		Locator:      loc,
		Validator:    val,
		Emit:         srv.Bus().Emit,
		MaxWatches:   opts.MaxWatches,
		PollInterval: opts.PollInterval,
	})

	svc = &Service{
		srv:        srv,
		locator:    loc,
		val:        val,
		watcher:    watcher,
		watchConns: make(map[*daemon.Conn]*connWatchState),
	}

	srv.Register(proto.MethodAgentLocate, daemon.MethodOptions{}, svc.handleLocate)
	srv.Register(proto.MethodConvoTails, daemon.MethodOptions{}, svc.handleTails)
	srv.Register(proto.MethodConvoMessageTails, daemon.MethodOptions{}, svc.handleMessageTails)
	srv.Register(proto.MethodAgentWatch, daemon.MethodOptions{}, svc.handleWatch)
	srv.Register(proto.MethodAgentUnwatch, daemon.MethodOptions{}, svc.handleUnwatch)

	srv.RegisterHealthCheck("locate", svc.healthLocate)
	srv.RegisterHealthCheck("tails", svc.healthTails)
	srv.RegisterHealthCheck("watch", svc.healthWatch)
	srv.RegisterStateProvider("agent", svc)
	srv.RegisterCloser(svc.close)

	return svc, nil
}

func (s *Service) close() {
	s.watchMu.Lock()
	conns := make([]*daemon.Conn, 0, len(s.watchConns))
	for c := range s.watchConns {
		conns = append(conns, c)
	}
	s.watchMu.Unlock()
	for _, c := range conns {
		s.detachWatchConn(c)
	}
	s.watcher.Stop()
}

// Locator returns the service's underlying Locator.
func (s *Service) Locator() *Locator { return s.locator }

// Validator returns the service's underlying PathValidator.
func (s *Service) Validator() *PathValidator { return s.val }

// Watcher returns the service's underlying Watcher.
func (s *Service) Watcher() *Watcher { return s.watcher }

func (s *Service) handleLocate(ctx context.Context, _ *daemon.Conn, raw json.RawMessage) (any, *proto.RPCError) {
	var p proto.AgentLocateParams
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, &proto.RPCError{Code: proto.ErrInvalidParams, Message: err.Error()}
		}
	}
	out := s.locator.Locate(ctx, p)
	return proto.AgentLocateResult{Out: out}, nil
}

func (s *Service) handleTails(_ context.Context, _ *daemon.Conn, raw json.RawMessage) (any, *proto.RPCError) {
	var p proto.ConvoTailsParams
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, &proto.RPCError{Code: proto.ErrInvalidParams, Message: err.Error()}
		}
	}
	out := Tails(s.val, p.Logs)
	return proto.ConvoTailsResult{Out: out}, nil
}

func (s *Service) handleMessageTails(_ context.Context, _ *daemon.Conn, raw json.RawMessage) (any, *proto.RPCError) {
	var p proto.ConvoTailsParams
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, &proto.RPCError{Code: proto.ErrInvalidParams, Message: err.Error()}
		}
	}
	out := MessageTails(s.val, p.Logs)
	return proto.ConvoTailsResult{Out: out}, nil
}

// lockConnState acquires the per-connection watch mutex for c while preserving
// strict lock order (cs.mu before s.watchMu). When create is true, a state
// record is created if absent; when false, returns nil if c is not registered.
func (s *Service) lockConnState(c *daemon.Conn, create bool) *connWatchState {
	for {
		s.watchMu.Lock()
		cs := s.watchConns[c]
		if cs == nil {
			if !create {
				s.watchMu.Unlock()
				return nil
			}
			cs = &connWatchState{}
			s.watchConns[c] = cs
		}
		s.watchMu.Unlock()

		cs.mu.Lock()
		s.watchMu.Lock()
		still := s.watchConns[c] == cs
		s.watchMu.Unlock()
		if still {
			return cs
		}
		cs.mu.Unlock()
	}
}

func (s *Service) detachWatchConnLocked(c *daemon.Conn, cs *connWatchState) WatchStatus {
	if cs.active && cs.stopCh != nil {
		close(cs.stopCh)
		cs.stopCh = nil
		cs.active = false
	}
	s.srv.Bus().Unsubscribe(c)
	st := s.watcher.Unsubscribe(c)
	s.watchMu.Lock()
	if s.watchConns[c] == cs {
		delete(s.watchConns, c)
	}
	s.watchMu.Unlock()
	return st
}

func (s *Service) detachWatchConn(c *daemon.Conn) WatchStatus {
	if c == nil {
		return s.watcher.Unsubscribe(nil)
	}
	cs := s.lockConnState(c, false)
	if cs == nil {
		return s.watcher.Status()
	}
	defer cs.mu.Unlock()
	return s.detachWatchConnLocked(c, cs)
}

func (s *Service) handleWatch(_ context.Context, c *daemon.Conn, raw json.RawMessage) (any, *proto.RPCError) {
	var p proto.AgentWatchParams
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, &proto.RPCError{Code: proto.ErrInvalidParams, Message: err.Error()}
		}
	}
	if p.Stop {
		st := s.detachWatchConn(c)
		return proto.AgentWatchResult{
			Watching:     st.Watching,
			Mode:         st.Mode,
			WatchedFiles: st.WatchedFiles,
			MaxFiles:     st.MaxFiles,
			Epoch:        s.srv.Bus().Epoch(),
			Seq:          s.srv.Bus().CurrentSeq(),
		}, nil
	}

	if c == nil {
		seq := s.srv.Bus().Subscribe(nil)
		st := s.watcher.Subscribe(nil, p.Logs)
		return proto.AgentWatchResult{
			Watching:     st.Watching,
			Mode:         st.Mode,
			WatchedFiles: st.WatchedFiles,
			MaxFiles:     st.MaxFiles,
			Epoch:        s.srv.Bus().Epoch(),
			Seq:          seq,
		}, nil
	}

	// Serialize all watch/unwatch/disconnect transitions on this connection in
	// one critical section so EventBus and Watcher can never diverge.
	cs := s.lockConnState(c, true)
	defer cs.mu.Unlock()

	select {
	case <-c.Done():
		st := s.detachWatchConnLocked(c, cs)
		return proto.AgentWatchResult{
			Watching:     st.Watching,
			Mode:         st.Mode,
			WatchedFiles: st.WatchedFiles,
			MaxFiles:     st.MaxFiles,
			Epoch:        s.srv.Bus().Epoch(),
			Seq:          s.srv.Bus().CurrentSeq(),
		}, nil
	default:
	}

	seq := s.srv.Bus().Subscribe(c)
	st := s.watcher.Subscribe(c, p.Logs)

	// Maintain at most one disconnect-watcher goroutine per connection; it
	// exits immediately when unwatch/stop is called or when the connection closes.
	if !cs.active {
		stopCh := make(chan struct{})
		cs.active = true
		cs.stopCh = stopCh
		go func(conn *daemon.Conn, stop <-chan struct{}) {
			select {
			case <-conn.Done():
				s.detachWatchConn(conn)
			case <-stop:
			}
		}(c, stopCh)
	}

	return proto.AgentWatchResult{
		Watching:     st.Watching,
		Mode:         st.Mode,
		WatchedFiles: st.WatchedFiles,
		MaxFiles:     st.MaxFiles,
		Epoch:        s.srv.Bus().Epoch(),
		Seq:          seq,
	}, nil
}

func (s *Service) handleUnwatch(_ context.Context, c *daemon.Conn, _ json.RawMessage) (any, *proto.RPCError) {
	st := s.detachWatchConn(c)
	return proto.AgentWatchResult{
		Watching:     st.Watching,
		Mode:         st.Mode,
		WatchedFiles: st.WatchedFiles,
		MaxFiles:     st.MaxFiles,
		Epoch:        s.srv.Bus().Epoch(),
		Seq:          s.srv.Bus().CurrentSeq(),
	}, nil
}

func (s *Service) healthLocate(_ context.Context) string {
	if !s.locator.rd.isReadableFile(proto.ReadProc, filepath.Join(s.locator.procRoot, "self", "stat")) {
		return "noproc"
	}
	return "ok"
}

func (s *Service) healthTails(_ context.Context) string {
	return "ok"
}

func (s *Service) healthWatch(_ context.Context) string {
	return s.watcher.Health()
}

type agentSnapshot struct {
	KnownLogs []string `json:"known_logs,omitempty"`
}

// Snapshot implements daemon.StateProvider so known located log paths survive
// hot-update handoff cleanly.
func (s *Service) Snapshot() (json.RawMessage, error) {
	return json.Marshal(agentSnapshot{
		KnownLogs: s.val.KnownLogs(),
	})
}

// Restore implements daemon.StateProvider.
func (s *Service) Restore(data json.RawMessage) error {
	if len(data) == 0 {
		return nil
	}
	var snap agentSnapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		return err
	}
	var clean []string
	for _, p := range snap.KnownLogs {
		if isCleanAbsPath(p) && !strings.ContainsAny(p, "\x00\n") {
			clean = append(clean, p)
		}
	}
	s.val.UpdateDiscovered(LocateScopeFull, clean, nil, nil)
	return nil
}
