//go:build linux

package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ihainan/corral/internal/corrald/proto"
)

const (
	// DefaultIdleTimeout is the inactivity duration with zero connected clients
	// after which corrald exits cleanly (spec §5: 10 minutes).
	DefaultIdleTimeout = 10 * time.Minute

	// DefaultDrainTimeout is the maximum time handoff waits for in-flight
	// mutating RPCs before spawning the replacement process (spec §6: 5 s).
	DefaultDrainTimeout = 5 * time.Second

	// DefaultReadyTimeout is the maximum time v1 waits for v2 to signal READY
	// during hot-update handoff (spec §6: 5 s).
	DefaultReadyTimeout = 5 * time.Second

	// DefaultPostHandoffExit is the grace period after READY before v1 exits
	// when no mutating RPC is still in flight (spec §6: 3 s).
	DefaultPostHandoffExit = 3 * time.Second

	// DefaultPostHandoffWriteExit is the hard cap after READY when an
	// in-flight mutating RPC exceeded the 5 s drain window (spec §6: 15 s).
	DefaultPostHandoffWriteExit = 15 * time.Second

	// DefaultOldVersionGCDelay is the healthy runtime period after which older
	// versions beyond current+previous are deleted (spec §6: 5 minutes).
	DefaultOldVersionGCDelay = 5 * time.Minute

	// DefaultSocketWatchInterval is the interval for verifying that corrald.sock
	// still exists at the expected inode.
	DefaultSocketWatchInterval = 60 * time.Second

	connOutboundQueueSize = 2048
)

// MethodOptions configures dispatch behavior for a registered RPC method.
type MethodOptions struct {
	// Mutating marks write operations (such as pane.send and pane.answer).
	// During handoff draining, new mutating calls are immediately rejected with
	// retryable handoff_in_progress, and in-flight calls are tracked for drain.
	Mutating bool
}

// HandlerFunc handles an RPC request. Returning nil result and nil error sends
// an empty JSON object result.
type HandlerFunc func(ctx context.Context, conn *Conn, params json.RawMessage) (any, *proto.RPCError)

// StateProvider allows stage 1 agent modules (T3) to serialize and restore
// cursors or watch state across a hot-update handoff.
type StateProvider interface {
	Snapshot() (json.RawMessage, error)
	Restore(data json.RawMessage) error
}

// HealthCheckFunc returns "ok" when a daemon capability is healthy, or a short
// diagnostic error string otherwise.
type HealthCheckFunc func(ctx context.Context) string

// ReconcilerFunc runs level-triggered state reconciliation during handoff
// warmup before the replacement daemon signals READY.
type ReconcilerFunc func(ctx context.Context) error

// ServerConfig configures a Server instance.
type ServerConfig struct {
	Layout              *Layout
	Build               proto.BuildInfo
	Gateway             string
	TmuxPath            string
	ExpectedUID         *uint32
	Logger              *slog.Logger
	IdleTimeout         time.Duration
	DrainTimeout        time.Duration
	ReadyTimeout        time.Duration
	PostHandoffExit     time.Duration
	PostHandoffWriteCap time.Duration
	OldVersionGCDelay   time.Duration
	SocketWatchInterval time.Duration
	Lock                *FileLock
	Listener            *net.UnixListener
	InitialSnapshot     *HandoffSnapshot
	OnReady             func()
}

type registeredMethod struct {
	opts MethodOptions
	fn   HandlerFunc
}

var (
	modulesMu sync.Mutex
	modules   []func(*Server)
)

// RegisterModule registers a host module attachment callback invoked for each
// daemon Server started via CLI serve.
func RegisterModule(fn func(*Server)) {
	if fn == nil {
		return
	}
	modulesMu.Lock()
	defer modulesMu.Unlock()
	modules = append(modules, fn)
}

func attachRegisteredModules(s *Server) {
	modulesMu.Lock()
	list := append(([]func(*Server))(nil), modules...)
	modulesMu.Unlock()
	for _, fn := range list {
		fn(s)
	}
}

var errAlreadyReplied = errors.New("response already enqueued")

// Server is the corrald Unix domain socket RPC and event server.
type Server struct {
	cfg         ServerConfig
	layout      *Layout
	build       proto.BuildInfo
	expectedUID uint32
	log         *slog.Logger
	startedAt   time.Time
	procStart   string

	bus     *EventBus
	ops     *OpTable
	clients *ClientRegistry

	mu             sync.RWMutex
	methods        map[string]registeredMethod
	stateProviders map[string]StateProvider
	healthChecks   map[string]HealthCheckFunc
	reconcilers    []ReconcilerFunc
	closers        []func()

	stateMu        sync.Mutex
	state          string // proto.StateRunning or proto.StateDraining
	inflightWrites int
	writeDoneCh    chan struct{}
	handoffActive  bool
	handedOff      bool

	acceptMu      sync.Mutex
	acceptCond    *sync.Cond
	acceptPaused  bool
	acceptInCall  bool
	acceptStopped bool

	connMu      sync.Mutex
	conns       map[*Conn]struct{}
	idleTimer   *time.Timer
	idleStopped bool

	sockDev uint64
	sockIno uint64
	sockOK  atomic.Bool

	ln       *net.UnixListener
	lock     *FileLock
	cancelFn context.CancelFunc
	stopOnce sync.Once
	wg       sync.WaitGroup
}

// NewServer constructs a corrald RPC server with built-in daemon.* methods
// registered. Call Serve(ctx) to begin accepting connections.
func NewServer(cfg ServerConfig) (*Server, error) {
	if cfg.Layout == nil {
		return nil, errors.New("server layout is required")
	}
	if cfg.Logger == nil {
		cfg.Logger = NewLogger(io.Discard)
	}
	if cfg.IdleTimeout <= 0 {
		cfg.IdleTimeout = DefaultIdleTimeout
	}
	if cfg.DrainTimeout <= 0 {
		cfg.DrainTimeout = DefaultDrainTimeout
	}
	if cfg.ReadyTimeout <= 0 {
		cfg.ReadyTimeout = DefaultReadyTimeout
	}
	if cfg.PostHandoffExit <= 0 {
		cfg.PostHandoffExit = DefaultPostHandoffExit
	}
	if cfg.PostHandoffWriteCap <= 0 {
		cfg.PostHandoffWriteCap = DefaultPostHandoffWriteExit
	}
	if cfg.OldVersionGCDelay <= 0 {
		cfg.OldVersionGCDelay = DefaultOldVersionGCDelay
	}
	if cfg.SocketWatchInterval <= 0 {
		cfg.SocketWatchInterval = DefaultSocketWatchInterval
	}
	expectedUID := cfg.Layout.UID
	if cfg.ExpectedUID != nil {
		expectedUID = *cfg.ExpectedUID
	}

	clients, err := OpenClientRegistry(cfg.Layout.ClientsJSON)
	if err != nil {
		return nil, fmt.Errorf("open client registry: %w", err)
	}
	procStart, _, _ := ProcStartTime(os.Getpid())

	s := &Server{
		cfg:            cfg,
		layout:         cfg.Layout,
		build:          cfg.Build,
		expectedUID:    expectedUID,
		log:            cfg.Logger,
		startedAt:      time.Now().UTC(),
		procStart:      procStart,
		bus:            NewEventBus(DefaultEventRingSize),
		ops:            NewOpTable(defaultOpTableCapacity, defaultOpTableTTL),
		clients:        clients,
		methods:        make(map[string]registeredMethod),
		stateProviders: make(map[string]StateProvider),
		healthChecks:   make(map[string]HealthCheckFunc),
		state:          proto.StateRunning,
		writeDoneCh:    make(chan struct{}, 1),
		conns:          make(map[*Conn]struct{}),
		ln:             cfg.Listener,
		lock:           cfg.Lock,
	}
	s.acceptCond = sync.NewCond(&s.acceptMu)
	s.sockOK.Store(true)

	s.registerBuiltinMethods()
	registerTestMethods(s)
	return s, nil
}

// Register adds an RPC method to the server's dispatch table.
func (s *Server) Register(method string, opts MethodOptions, fn HandlerFunc) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.methods[method] = registeredMethod{opts: opts, fn: fn}
}

// RegisterStateProvider attaches a T3 module state snapshot/restore hook.
func (s *Server) RegisterStateProvider(name string, p StateProvider) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stateProviders[name] = p
}

// RegisterHealthCheck attaches a capability health check included in daemon.health.
func (s *Server) RegisterHealthCheck(name string, fn HealthCheckFunc) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.healthChecks[name] = fn
}

// RegisterReconciler attaches a level-triggered reconciliation hook invoked
// during handoff warmup before signaling READY.
func (s *Server) RegisterReconciler(fn ReconcilerFunc) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reconcilers = append(s.reconcilers, fn)
}

// RegisterCloser attaches a cleanup callback invoked when Serve shuts down.
func (s *Server) RegisterCloser(fn func()) {
	if fn == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closers = append(s.closers, fn)
}

// Layout returns the server's resolved host path layout.
func (s *Server) Layout() *Layout { return s.layout }

// TmuxPath returns the validated optional tmux binary path passed via --tmux.
func (s *Server) TmuxPath() string { return s.cfg.TmuxPath }

// Bus returns the server's EventBus for emitting events.
func (s *Server) Bus() *EventBus { return s.bus }

// Ops returns the server's OpTable for idempotent write execution.
func (s *Server) Ops() *OpTable { return s.ops }

// Clients returns the server's persisted ClientRegistry.
func (s *Server) Clients() *ClientRegistry { return s.clients }

// HandedOff reports whether the server exited because control was handed off
// cleanly to a replacement process.
func (s *Server) HandedOff() bool {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	return s.handedOff
}

// Serve binds (or adopts) the Unix listener, writes meta.json, invokes OnReady,
// and serves RPC connections until shutdown, idle timeout, context cancel, or
// hot-update handoff.
func (s *Server) Serve(parentCtx context.Context) error {
	ctx, cancel := context.WithCancel(parentCtx)
	s.cancelFn = cancel
	defer cancel()

	// Restore handoff snapshot if starting as a replacement daemon.
	if snap := s.cfg.InitialSnapshot; snap != nil {
		if snap.Schema == HandoffSnapshotSchema {
			s.bus.Restore(snap.Epoch, snap.LastSeq, snap.Events)
			if err := s.clients.Restore(snap.Clients); err != nil {
				s.log.Warn("restore clients failed", "err", err)
			}
			s.ops.Restore(snap.CompletedOps)
			s.restoreModuleStates(snap.Modules)
		} else {
			s.log.Warn("unsupported handoff snapshot schema; starting fresh epoch", "schema", snap.Schema)
		}
	}

	if s.ln == nil {
		if err := s.layout.EnsureSocketDir(); err != nil {
			return fmt.Errorf("prepare socket dir: %w", err)
		}
		_ = os.Remove(s.layout.SocketPath)
		addr := &net.UnixAddr{Name: s.layout.SocketPath, Net: "unix"}
		ln, err := net.ListenUnix("unix", addr)
		if err != nil {
			return fmt.Errorf("listen unix %s: %w", s.layout.SocketPath, err)
		}
		if err := os.Chmod(s.layout.SocketPath, 0o600); err != nil {
			_ = ln.Close()
			return fmt.Errorf("chmod socket %s: %w", s.layout.SocketPath, err)
		}
		s.ln = ln
	}
	// Never let Go auto-unlink the socket on Close(); we explicitly remove the
	// socket only on clean shutdown/idle exit and NEVER on handoff.
	s.ln.SetUnlinkOnClose(false)

	if dev, ino, err := FileInode(s.layout.SocketPath); err == nil {
		s.sockDev, s.sockIno = dev, ino
	}

	// Run level-triggered reconcilers before signaling READY.
	if s.cfg.InitialSnapshot != nil {
		if err := s.reconcileAll(ctx); err != nil {
			s.log.Warn("handoff reconciliation returned error", "err", err)
		}
	}

	// Write meta.json before signaling readiness.
	meta := &MetaRecord{
		PID:        os.Getpid(),
		StartTime:  s.procStart,
		Version:    s.build.Version,
		Release:    s.build.Release,
		Commit:     s.build.Commit,
		Epoch:      s.bus.Epoch(),
		StartedAt:  s.startedAt.Format(time.RFC3339),
		SocketPath: s.layout.SocketPath,
		Gateway:    s.cfg.Gateway,
		TmuxPath:   s.cfg.TmuxPath,
	}
	if err := WriteMetaRecord(s.layout.MetaJSON, meta); err != nil {
		return fmt.Errorf("write meta.json: %w", err)
	}

	s.log.Info("corrald serving",
		"pid", meta.PID,
		"version", meta.Version,
		"release", meta.Release,
		"epoch", meta.Epoch,
		"socket", meta.SocketPath,
		"handoff", s.cfg.InitialSnapshot != nil,
	)

	// Start idle timer (starts immediately; cancelled as soon as a client connects).
	s.armIdleTimer()

	// Start accept loop before notifying readiness so incoming connections are
	// accepted immediately.
	s.wg.Add(1)
	go s.acceptLoop(ctx)

	// Start periodic socket inode watchdog.
	s.wg.Add(1)
	go s.socketWatchLoop(ctx)

	// Schedule old version cleanup after staying healthy for OldVersionGCDelay.
	s.wg.Add(1)
	go s.oldVersionGCLoop(ctx)

	if s.cfg.OnReady != nil {
		s.cfg.OnReady()
	}

	<-ctx.Done()

	s.runClosers()
	s.stopIdleTimer()
	s.closeListenerForExit()
	s.closeAllConns()
	s.wg.Wait()

	if !s.HandedOff() {
		_ = os.Remove(s.layout.SocketPath)
		_ = os.Remove(s.layout.MetaJSON)
		s.log.Info("corrald stopped", "pid", os.Getpid())
	} else {
		s.log.Info("corrald exited after handoff", "pid", os.Getpid())
	}
	return nil
}

// RequestShutdown cancels the server's main loop cleanly.
func (s *Server) RequestShutdown() {
	s.stopOnce.Do(func() {
		if s.cancelFn != nil {
			s.cancelFn()
		}
	})
}

func (s *Server) closeListenerForExit() {
	s.acceptMu.Lock()
	s.acceptStopped = true
	s.acceptPaused = false
	if s.ln != nil {
		s.ln.SetUnlinkOnClose(false)
		_ = s.ln.Close()
	}
	s.acceptCond.Broadcast()
	s.acceptMu.Unlock()
}

func (s *Server) acceptLoop(ctx context.Context) {
	defer s.wg.Done()
	for {
		s.acceptMu.Lock()
		for s.acceptPaused && !s.acceptStopped {
			s.acceptCond.Wait()
		}
		if s.acceptStopped || ctx.Err() != nil {
			s.acceptStopped = true
			s.acceptCond.Broadcast()
			s.acceptMu.Unlock()
			return
		}
		s.acceptInCall = true
		ln := s.ln
		s.acceptMu.Unlock()

		rawConn, err := ln.AcceptUnix()

		s.acceptMu.Lock()
		s.acceptInCall = false
		paused := s.acceptPaused
		stopped := s.acceptStopped
		s.acceptCond.Broadcast()
		s.acceptMu.Unlock()

		if err != nil {
			if paused {
				// Expected wakeup from ln.SetDeadline(time.Now()) during handoff.
				continue
			}
			if stopped || ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				continue
			}
			s.log.Error("accept failed", "err", err)
			time.Sleep(20 * time.Millisecond)
			continue
		}

		peerUID, peerPID, credErr := CheckPeerCred(rawConn, s.expectedUID)
		if credErr != nil {
			s.log.Warn("rejected peer with mismatched uid",
				"peer_uid", peerUID,
				"peer_pid", peerPID,
				"expected_uid", s.expectedUID,
			)
			_ = rawConn.Close()
			continue
		}

		c := s.newConn(rawConn, peerPID)
		s.addConn(c)
		s.wg.Add(1)
		go s.serveConn(ctx, c)
	}
}

func (s *Server) socketWatchLoop(ctx context.Context) {
	defer s.wg.Done()
	t := time.NewTicker(s.cfg.SocketWatchInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if s.sockIno == 0 {
				continue
			}
			dev, ino, err := FileInode(s.layout.SocketPath)
			if err != nil || dev != s.sockDev || ino != s.sockIno {
				s.sockOK.Store(false)
				s.log.Warn("socket path missing or inode changed", "socket", s.layout.SocketPath, "err", err)
			} else {
				s.sockOK.Store(true)
			}
		}
	}
}

func (s *Server) oldVersionGCLoop(ctx context.Context) {
	defer s.wg.Done()
	timer := time.NewTimer(s.cfg.OldVersionGCDelay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return
	case <-timer.C:
		if s.HandedOff() {
			return
		}
		removed, err := PruneOldVersions(s.layout, s.build.Version, s.build.Release)
		if err != nil {
			s.log.Warn("old version gc error", "err", err)
		} else if len(removed) > 0 {
			s.log.Info("pruned old corrald versions", "removed", removed)
		}
	}
}

func (s *Server) armIdleTimer() {
	s.connMu.Lock()
	defer s.connMu.Unlock()
	if s.idleStopped || len(s.conns) > 0 {
		return
	}
	if s.idleTimer != nil {
		s.idleTimer.Stop()
	}
	s.idleTimer = time.AfterFunc(s.cfg.IdleTimeout, func() {
		s.connMu.Lock()
		empty := len(s.conns) == 0 && !s.idleStopped
		s.connMu.Unlock()
		if !empty {
			return
		}
		s.stateMu.Lock()
		handingOff := s.handoffActive || s.handedOff
		s.stateMu.Unlock()
		if handingOff {
			return
		}
		s.log.Info("idle timeout reached with no connected gateways; exiting", "timeout", s.cfg.IdleTimeout)
		s.RequestShutdown()
	})
}

func (s *Server) stopIdleTimer() {
	s.connMu.Lock()
	defer s.connMu.Unlock()
	s.idleStopped = true
	if s.idleTimer != nil {
		s.idleTimer.Stop()
		s.idleTimer = nil
	}
}

func (s *Server) addConn(c *Conn) {
	s.connMu.Lock()
	s.conns[c] = struct{}{}
	if s.idleTimer != nil {
		s.idleTimer.Stop()
		s.idleTimer = nil
	}
	s.connMu.Unlock()
}

func (s *Server) removeConn(c *Conn) {
	s.bus.Unsubscribe(c)
	s.connMu.Lock()
	delete(s.conns, c)
	needIdle := len(s.conns) == 0 && !s.idleStopped
	s.connMu.Unlock()
	if needIdle {
		s.armIdleTimer()
	}
}

func (s *Server) connectedCount() int {
	s.connMu.Lock()
	defer s.connMu.Unlock()
	return len(s.conns)
}

func (s *Server) closeAllConns() {
	s.connMu.Lock()
	list := make([]*Conn, 0, len(s.conns))
	for c := range s.conns {
		list = append(list, c)
	}
	s.connMu.Unlock()
	for _, c := range list {
		c.Close()
	}
}

func (s *Server) broadcastControlEvent(ev proto.Event) {
	s.connMu.Lock()
	list := make([]*Conn, 0, len(s.conns))
	for c := range s.conns {
		list = append(list, c)
	}
	s.connMu.Unlock()
	for _, c := range list {
		c.DeliverEvent(ev)
	}
}

// Conn represents one authenticated Unix socket client connection.
type Conn struct {
	server  *Server
	raw     *net.UnixConn
	peerPID int

	mu      sync.Mutex
	closed  bool
	outCh   chan []byte
	doneCh  chan struct{}
	gateway string
	user    string
}

func (s *Server) newConn(raw *net.UnixConn, peerPID int) *Conn {
	return &Conn{
		server:  s,
		raw:     raw,
		peerPID: peerPID,
		outCh:   make(chan []byte, connOutboundQueueSize),
		doneCh:  make(chan struct{}),
	}
}

// DeliverEvent enqueues ev onto the connection's ordered outbound channel.
// It never blocks; a saturated slow connection is closed and detached.
func (c *Conn) DeliverEvent(ev proto.Event) bool {
	b, err := json.Marshal(ev)
	if err != nil {
		return true
	}
	return c.enqueueRaw(b)
}

func (c *Conn) enqueueRaw(payload []byte) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return false
	}
	select {
	case c.outCh <- payload:
		return true
	default:
		// Outbound queue saturated; close slow connection rather than blocking bus.
		go c.Close()
		return false
	}
}

func (c *Conn) enqueueResponse(id uint64, result any, rpcErr *proto.RPCError) bool {
	b := marshalResponseBytes(id, result, rpcErr)
	return c.enqueueRaw(b)
}

// Done returns a channel that is closed when the connection terminates.
func (c *Conn) Done() <-chan struct{} {
	return c.doneCh
}

// Close terminates the connection and stops its writer goroutine.
func (c *Conn) Close() {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	close(c.doneCh)
	c.mu.Unlock()
	_ = c.raw.Close()
}

// FlushAndClose drains queued outbound frames (up to timeout) before closing.
func (c *Conn) FlushAndClose(timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		c.mu.Lock()
		empty := len(c.outCh) == 0 || c.closed
		c.mu.Unlock()
		if empty {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	c.Close()
}

func (s *Server) serveConn(ctx context.Context, c *Conn) {
	defer s.wg.Done()
	defer s.removeConn(c)
	defer c.Close()

	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		for {
			select {
			case payload := <-c.outCh:
				if err := proto.WriteFrame(c.raw, payload); err != nil {
					c.Close()
					return
				}
			case <-c.doneCh:
				// Best-effort drain of any frames already queued before close.
				for {
					select {
					case payload := <-c.outCh:
						if err := proto.WriteFrame(c.raw, payload); err != nil {
							return
						}
					default:
						return
					}
				}
			}
		}
	}()

	var reqWG sync.WaitGroup
	for {
		var req proto.Request
		if err := proto.ReadJSON(c.raw, &req); err != nil {
			if !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) && ctx.Err() == nil {
				s.log.Warn("connection read error", "peer_pid", c.peerPID, "err", err)
			}
			break
		}
		if req.Method == "" {
			c.enqueueResponse(req.ID, nil, &proto.RPCError{
				Code:    proto.ErrInvalidParams,
				Message: "missing method",
			})
			continue
		}
		// Handle daemon.resume synchronously on the reader goroutine so
		// subscription ordering relative to subsequent requests on this
		// connection is deterministic. Other methods execute concurrently so
		// slow writes or handoff do not block concurrent reads.
		if req.Method == proto.MethodDaemonResume || req.Method == proto.MethodDaemonHello {
			s.dispatchRequest(ctx, c, req)
			continue
		}
		reqWG.Add(1)
		go func(r proto.Request) {
			defer reqWG.Done()
			s.dispatchRequest(ctx, c, r)
		}(req)
	}

	reqWG.Wait()
	c.Close()
	<-writerDone
}

func (s *Server) dispatchRequest(ctx context.Context, c *Conn, req proto.Request) {
	switch req.Method {
	case proto.MethodDaemonResume:
		s.dispatchResume(c, req)
		return
	case proto.MethodDaemonHandoff:
		s.dispatchHandoff(ctx, c, req)
		return
	}

	s.mu.RLock()
	reg, ok := s.methods[req.Method]
	s.mu.RUnlock()
	if !ok {
		c.enqueueResponse(req.ID, nil, &proto.RPCError{
			Code:    proto.ErrMethodNotFound,
			Message: fmt.Sprintf("unknown method %q", req.Method),
		})
		return
	}

	if reg.opts.Mutating {
		if !s.enterMutating() {
			c.enqueueResponse(req.ID, nil, &proto.RPCError{
				Code:      proto.ErrHandoffInProgress,
				Message:   "daemon handoff in progress; retry after reconnect",
				Retryable: true,
			})
			return
		}
		defer s.leaveMutating()
	}

	res, rpcErr := reg.fn(ctx, c, req.Params)
	if errors.Is(asErr(res), errAlreadyReplied) {
		return
	}
	c.enqueueResponse(req.ID, res, rpcErr)
}

func asErr(v any) error {
	e, _ := v.(error)
	return e
}

func (s *Server) enterMutating() bool {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	if s.state == proto.StateDraining || s.handedOff {
		return false
	}
	s.inflightWrites++
	return true
}

func (s *Server) leaveMutating() {
	s.stateMu.Lock()
	s.inflightWrites--
	zero := s.inflightWrites == 0
	s.stateMu.Unlock()
	if zero {
		select {
		case s.writeDoneCh <- struct{}{}:
		default:
		}
	}
}

func (s *Server) currentState() string {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	return s.state
}

func (s *Server) registerBuiltinMethods() {
	s.Register(proto.MethodDaemonHello, MethodOptions{}, s.handleHello)
	s.Register(proto.MethodDaemonResume, MethodOptions{}, s.handleResume)
	s.Register(proto.MethodDaemonHealth, MethodOptions{}, s.handleHealth)
	s.Register(proto.MethodDaemonHandoff, MethodOptions{}, s.handleHandoff)
	s.Register(proto.MethodDaemonShutdown, MethodOptions{}, s.handleShutdown)
	s.Register(proto.MethodDaemonForget, MethodOptions{}, s.handleForget)
}

func (s *Server) handleHello(_ context.Context, c *Conn, raw json.RawMessage) (any, *proto.RPCError) {
	var p proto.HelloParams
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, &proto.RPCError{Code: proto.ErrInvalidParams, Message: err.Error()}
		}
	}
	if p.ProtoMin != 0 || p.ProtoMax != 0 {
		if !proto.ProtocolInRange(proto.Protocol, p.ProtoMin, p.ProtoMax) {
			return nil, &proto.RPCError{
				Code:    proto.ErrProtoMismatch,
				Message: fmt.Sprintf("daemon protocol %d outside gateway range [%d,%d]", proto.Protocol, p.ProtoMin, p.ProtoMax),
			}
		}
	}

	c.mu.Lock()
	c.gateway = p.Gateway
	c.user = p.Client.CorralUser
	c.mu.Unlock()

	clients, err := s.clients.Touch(p.Gateway, p.Client.CorralUser, p.Client.HostID)
	if err != nil {
		s.log.Warn("persist client registry failed", "err", err)
	}
	s.log.Info("client hello", "gateway", p.Gateway, "user", p.Client.CorralUser, "host_id", p.Client.HostID)

	return proto.HelloResult{
		Version:   s.build.Version,
		Release:   s.build.Release,
		Commit:    s.build.Commit,
		Protocol:  proto.Protocol,
		Epoch:     s.bus.Epoch(),
		Seq:       s.bus.CurrentSeq(),
		PID:       os.Getpid(),
		StartTime: s.procStart,
		Manifest:  proto.DefaultManifest(),
		Clients:   clients,
		State:     s.currentState(),
	}, nil
}

func (s *Server) handleResume(_ context.Context, c *Conn, raw json.RawMessage) (any, *proto.RPCError) {
	// We need the request ID to enqueue the response atomically under bus.mu;
	// handleResume is invoked via dispatchResume below when called from RPC,
	// or directly via params when we capture reqID in dispatchRequest.
	// Wait: see dispatchRequest override below for atomic response+replay.
	return nil, &proto.RPCError{Code: proto.ErrInternal, Message: "resume called outside request context"}
}

func (s *Server) dispatchResume(c *Conn, req proto.Request) {
	var p proto.ResumeParams
	if len(req.Params) > 0 {
		if err := json.Unmarshal(req.Params, &p); err != nil {
			c.enqueueResponse(req.ID, nil, &proto.RPCError{Code: proto.ErrInvalidParams, Message: err.Error()})
			return
		}
	}
	_, _, rpcErr := s.bus.Resume(p.Epoch, p.LastSeq, c, func(replayed []proto.Event, curSeq uint64) {
		res := proto.ResumeResult{
			Epoch:      p.Epoch,
			Replayed:   len(replayed),
			CurrentSeq: curSeq,
		}
		c.enqueueResponse(req.ID, res, nil)
		for _, ev := range replayed {
			c.DeliverEvent(ev)
		}
	})
	if rpcErr != nil {
		c.enqueueResponse(req.ID, nil, rpcErr)
	}
}

func (s *Server) handleHealth(ctx context.Context, _ *Conn, _ json.RawMessage) (any, *proto.RPCError) {
	caps := map[string]string{
		"daemon": "ok",
		"socket": "ok",
	}
	if !s.sockOK.Load() {
		caps["socket"] = "socket_missing"
	}
	s.mu.RLock()
	checks := make(map[string]HealthCheckFunc, len(s.healthChecks))
	for k, fn := range s.healthChecks {
		checks[k] = fn
	}
	s.mu.RUnlock()
	for name, fn := range checks {
		caps[name] = fn(ctx)
	}
	return proto.HealthResult{
		Caps:              caps,
		RSSKB:             ProcRSSKB(0),
		UptimeS:           int64(time.Since(s.startedAt).Seconds()),
		Version:           s.build.Version,
		Release:           s.build.Release,
		PID:               os.Getpid(),
		State:             s.currentState(),
		Socket:            s.layout.SocketPath,
		ConnectedGateways: s.connectedCount(),
	}, nil
}

func (s *Server) handleShutdown(_ context.Context, _ *Conn, raw json.RawMessage) (any, *proto.RPCError) {
	var p proto.ShutdownParams
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &p)
	}
	s.log.Info("shutdown requested via rpc", "reason", p.Reason)
	go func() {
		time.Sleep(15 * time.Millisecond)
		s.RequestShutdown()
	}()
	return proto.ShutdownResult{OK: true}, nil
}

func (s *Server) handleForget(_ context.Context, _ *Conn, raw json.RawMessage) (any, *proto.RPCError) {
	var p proto.ForgetParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, &proto.RPCError{Code: proto.ErrInvalidParams, Message: err.Error()}
	}
	if p.Gateway == "" && p.CorralUser == "" {
		return nil, &proto.RPCError{Code: proto.ErrInvalidParams, Message: "gateway or corral_user is required"}
	}
	remaining, err := s.clients.Forget(p.Gateway, p.CorralUser, p.HostID)
	if err != nil {
		return nil, &proto.RPCError{Code: proto.ErrInternal, Message: err.Error()}
	}
	s.log.Info("client forgotten", "gateway", p.Gateway, "user", p.CorralUser, "remaining", remaining)
	return proto.ForgetResult{RemainingClients: remaining}, nil
}

func (s *Server) snapshotModuleStates() map[string]json.RawMessage {
	s.mu.RLock()
	providers := make(map[string]StateProvider, len(s.stateProviders))
	for k, p := range s.stateProviders {
		providers[k] = p
	}
	s.mu.RUnlock()
	if len(providers) == 0 {
		return nil
	}
	out := make(map[string]json.RawMessage, len(providers))
	for name, p := range providers {
		raw, err := p.Snapshot()
		if err != nil {
			s.log.Warn("module snapshot failed", "module", name, "err", err)
			continue
		}
		out[name] = raw
	}
	return out
}

func (s *Server) restoreModuleStates(mods map[string]json.RawMessage) {
	if len(mods) == 0 {
		return
	}
	s.mu.RLock()
	providers := make(map[string]StateProvider, len(s.stateProviders))
	for k, p := range s.stateProviders {
		providers[k] = p
	}
	s.mu.RUnlock()
	for name, raw := range mods {
		if p, ok := providers[name]; ok {
			if err := p.Restore(raw); err != nil {
				s.log.Warn("module restore failed", "module", name, "err", err)
			}
		}
	}
}

func (s *Server) runClosers() {
	s.mu.RLock()
	fns := append(([]func())(nil), s.closers...)
	s.mu.RUnlock()
	for _, fn := range fns {
		fn()
	}
}

func (s *Server) reconcileAll(ctx context.Context) error {
	s.mu.RLock()
	recs := append([]ReconcilerFunc(nil), s.reconcilers...)
	s.mu.RUnlock()
	for _, fn := range recs {
		if err := fn(ctx); err != nil {
			return err
		}
	}
	return nil
}

func marshalResponseBytes(id uint64, result any, rpcErr *proto.RPCError) []byte {
	resp := proto.Response{ID: id, Error: rpcErr}
	if rpcErr == nil {
		if result == nil {
			resp.Result = json.RawMessage(`{}`)
		} else if raw, ok := result.(json.RawMessage); ok {
			resp.Result = raw
		} else {
			encoded, err := json.Marshal(result)
			if err != nil {
				resp.Error = &proto.RPCError{Code: proto.ErrInternal, Message: err.Error()}
			} else {
				resp.Result = encoded
			}
		}
	}
	b, _ := json.Marshal(resp)
	return b
}
