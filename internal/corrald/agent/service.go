//go:build linux

package agent

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"time"

	"github.com/ihainan/corral/internal/corrald/daemon"
	"github.com/ihainan/corral/internal/corrald/proto"
)

// Service wires Locator, PathValidator, Tails, MessageTails, and Watcher into
// a corrald daemon.Server.
type Service struct {
	srv     *daemon.Server
	locator *Locator
	val     *PathValidator
	watcher *Watcher
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
	val := NewPathValidator(locOpts.Home, valUID, locOpts.ProcRoot)

	var svc *Service
	userOnDiscovered := locOpts.OnDiscovered
	locOpts.OnDiscovered = func(logs, claudeDirs, codexHomes []string) {
		val.UpdateDiscovered(logs, claudeDirs, codexHomes)
		if svc != nil && svc.watcher != nil {
			svc.watcher.NotifyDiscovered()
		}
		if userOnDiscovered != nil {
			userOnDiscovered(logs, claudeDirs, codexHomes)
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
		srv:     srv,
		locator: loc,
		val:     val,
		watcher: watcher,
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
	srv.RegisterCloser(watcher.Stop)

	return svc, nil
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

func (s *Service) handleWatch(_ context.Context, c *daemon.Conn, raw json.RawMessage) (any, *proto.RPCError) {
	var p proto.AgentWatchParams
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, &proto.RPCError{Code: proto.ErrInvalidParams, Message: err.Error()}
		}
	}
	if p.Stop {
		st := s.watcher.Unsubscribe(c)
		return proto.AgentWatchResult{
			Watching:     st.Watching,
			Mode:         st.Mode,
			WatchedFiles: st.WatchedFiles,
			MaxFiles:     st.MaxFiles,
			Epoch:        s.srv.Bus().Epoch(),
			Seq:          s.srv.Bus().CurrentSeq(),
		}, nil
	}

	seq := s.srv.Bus().Subscribe(c)
	st := s.watcher.Subscribe(c, p.Logs)

	// Automatically detach this connection from the watcher when it closes.
	if c != nil {
		go func(conn *daemon.Conn) {
			<-conn.Done()
			s.watcher.Unsubscribe(conn)
		}(c)
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
	st := s.watcher.Unsubscribe(c)
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
		if p != "" && filepath.IsAbs(p) && !strings.ContainsAny(p, "\x00\n") {
			clean = append(clean, p)
		}
	}
	s.val.UpdateDiscovered(clean, nil, nil)
	return nil
}
