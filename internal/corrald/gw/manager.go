package gw

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/ihainan/corral/internal/corrald/proto"
	"github.com/ihainan/corral/internal/sshx"
	"github.com/ihainan/corral/internal/store"
)

// DefaultUninstallCmd is shown when manual cleanup on the host is needed.
const DefaultUninstallCmd = "~/.local/lib/corral/corrald uninstall"

var (
	// ErrUnavailable is returned when corrald binaries are not loaded on the gateway.
	ErrUnavailable = errors.New("corrald is not available on this gateway")
	// ErrAdminDisabled is returned when the administrator has disabled corrald.
	ErrAdminDisabled = errors.New("corrald is disabled by administrator")
	// ErrManifestChanged is returned when the manifest_hash submitted by the user
	// does not match the gateway's current corrald manifest.
	ErrManifestChanged = errors.New("corrald manifest changed; review details again")
	// ErrCannotCancel is returned when attempting to cancel an install that is not
	// active or has already reached the start phase.
	ErrCannotCancel = errors.New("operation cannot be cancelled")
	// ErrInvalidState is returned when an action is not valid in the host's state.
	ErrInvalidState = errors.New("action not valid in current corrald state")
	// ErrNotConnected is returned when no SSH connection can be obtained to reach the host.
	ErrNotConnected = errors.New("cannot connect to host")
)

// SSHProvider supplies SSH connections and host/user metadata to Manager.
type SSHProvider interface {
	// AcquireSSH returns an active or freshly dialed SSH client for (ownerID, hostID),
	// along with a release callback, the pinned host key fingerprint, and SSH username.
	AcquireSSH(ctx context.Context, ownerID, hostID int64) (client *ssh.Client, release func(), fingerprint, sshUser string, err error)
	// HostBinding returns the current SSH username and pinned host key fingerprint
	// (empty string if not yet pinned) for (ownerID, hostID).
	HostBinding(ctx context.Context, ownerID, hostID int64) (sshUser, fingerprint string, err error)
	// UserLabel returns the display/username label for a Corral user ID.
	UserLabel(ctx context.Context, userID int64) string
}

// ManagerConfig configures a corrald gateway Manager.
type ManagerConfig struct {
	Store          *store.Store
	Dist           *Dist
	GatewayID      string
	GatewayVersion string
	AdminDisabled  bool
	Log            *slog.Logger
	SSH            SSHProvider

	// Tunables (optional; defaults apply when zero).
	HealthInterval  time.Duration
	RestartBackoffs []time.Duration
	CrashWindow     time.Duration
	CrashThreshold  int
	ProtoMin        int
	ProtoMax        int

	// Test hooks (nil in production).
	ProbeOverride    func(ctx context.Context, c *ssh.Client) (*HostProbe, error)
	VerifySHA256Hook func(actual string) string
	// OnEvent receives all daemon events forwarded across hosts (for T3 & tests).
	OnEvent func(fingerprint, sshUser string, ev proto.Event)
	// OnResync is called when daemon.resume returns resync_required.
	OnResync func(fingerprint, sshUser string)
	// BeforeInstallStart is an optional test hook called right before the start
	// step of an install (useful for testing cancellation or gateway restart).
	BeforeInstallStart func(hostID int64, opID string)
}

// API view types matching web/src/corraldLogic.ts.

type RunningInfo struct {
	PID     int   `json:"pid"`
	UptimeS int64 `json:"uptime_s"`
	RSSKB   int64 `json:"rss_kb"`
}

type UsersInfo struct {
	Count  int      `json:"count"`
	Others []string `json:"others"`
}

type ConsentInfo struct {
	By string `json:"by"`
	At int64  `json:"at"`
}

type ScopeChangeInfo struct {
	TargetVersion string   `json:"target_version,omitempty"`
	Added         []string `json:"added"`
	Summary       string   `json:"summary,omitempty"`
}

type OpView struct {
	ID          string     `json:"id"`
	Kind        string     `json:"kind"`
	State       string     `json:"state"`
	Step        string     `json:"step"`
	Steps       []string   `json:"steps,omitempty"`
	Cancellable bool       `json:"cancellable"`
	ErrorCode   string     `json:"error_code,omitempty"`
	Detail      string     `json:"detail,omitempty"`
	Result      string     `json:"result,omitempty"`
	Left        []LeftItem `json:"left,omitempty"`
	PID         int        `json:"pid,omitempty"`
	HostInfo    string     `json:"host_info,omitempty"`
}

// opDetailPayload is stored in corrald_ops.detail as JSON for structured fields.
type opDetailPayload struct {
	Message  string     `json:"message,omitempty"`
	Result   string     `json:"result,omitempty"`
	Left     []LeftItem `json:"left,omitempty"`
	PID      int        `json:"pid,omitempty"`
	HostInfo string     `json:"host_info,omitempty"`
}

type StatusResponse struct {
	State                 string            `json:"state"`
	Version               string            `json:"version,omitempty"`
	Release               int64             `json:"release,omitempty"`
	GatewayVersion        string            `json:"gateway_version,omitempty"`
	ManifestHash          string            `json:"manifest_hash,omitempty"`
	EstimatedSizeMB       int               `json:"estimated_size_mb,omitempty"`
	AgentsCount           *int              `json:"agents_count,omitempty"`
	LastOKAt              int64             `json:"last_ok_at,omitempty"`
	Running               *RunningInfo      `json:"running"`
	Users                 *UsersInfo        `json:"users"`
	Consent               *ConsentInfo      `json:"consent"`
	Op                    *OpView           `json:"op"`
	Caps                  map[string]string `json:"caps"`
	ScopeChange           *ScopeChangeInfo  `json:"scope_change"`
	Error                 *OpFailure        `json:"error"`
	AdminDisabled         bool              `json:"admin_disabled"`
	Files                 []string          `json:"files"`
	CanReplaceWithGateway bool              `json:"can_replace_with_gateway,omitempty"`
	MachineID             string            `json:"machine_id,omitempty"`
}

// DeleteCorraldOutcome is returned inside DELETE /api/hosts/{id}.
type DeleteCorraldOutcome struct {
	State        string     `json:"state"`
	Left         []LeftItem `json:"left,omitempty"`
	PID          int        `json:"pid,omitempty"`
	Error        string     `json:"error,omitempty"`
	UninstallCmd string     `json:"uninstall_cmd,omitempty"`
}

type accountKey struct {
	Fingerprint string
	Username    string
}

type hostRef struct {
	HostID     int64
	OwnerID    int64
	CorralUser string
}

type runningOp struct {
	opID      string
	kind      string
	step      string
	cancel    context.CancelFunc
	cancelled bool
	done      chan struct{}
}

type sharedConn struct {
	key        accountKey
	mu         sync.Mutex
	client     *Client
	sshClient  *ssh.Client
	sshRelease func()
	socketPath string
	machineID  string
	libDir     string
	stateDir   string
	hello      *proto.HelloResult
	health     *proto.HealthResult
	epoch      string
	lastSeq    uint64
	hostRefs   map[int64]*hostRef

	// Hot-update tracking & 30-min crash-loop rollback state.
	updatedAt        time.Time
	updateTargetVer  string
	updateTargetRel  int64
	updateCrashCount int
	prevVersion      string
	prevRelease      int64
	prevRelTarget    string // relative symlink target under LibDir, e.g. corrald-0.1.0-100/corrald

	// Pending scope change detected against current Dist.
	scopeChange *ScopeChangeInfo

	// Lifecycle coordination.
	intentionalClose bool
	reconnecting     bool
	updating         bool
	recovering       bool
	stopCh           chan struct{}
	stopOnce         sync.Once
}

func (sc *sharedConn) stop() {
	sc.stopOnce.Do(func() { close(sc.stopCh) })
}

// Manager coordinates all corrald connections, operations, auto-updates, health
// monitoring, and fallbacks on the gateway.
type Manager struct {
	cfg             ManagerConfig
	store           *store.Store
	log             *slog.Logger
	ssh             SSHProvider
	gatewayID       string
	gatewayVersion  string
	adminDisabled   bool
	healthInterval  time.Duration
	restartBackoffs []time.Duration
	crashWindow     time.Duration
	crashThreshold  int
	protoMin        int
	protoMax        int

	distMu sync.RWMutex
	dist   *Dist

	mu       sync.Mutex
	conns    map[accountKey]*sharedConn
	hostConn map[int64]accountKey
	ops      map[int64]*runningOp
	closed   bool
	wg       sync.WaitGroup
}

// NewManager creates a corrald gateway Manager.
func NewManager(cfg ManagerConfig) *Manager {
	if cfg.Log == nil {
		cfg.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if cfg.HealthInterval <= 0 {
		cfg.HealthInterval = 30 * time.Second
	}
	if len(cfg.RestartBackoffs) == 0 {
		cfg.RestartBackoffs = []time.Duration{2 * time.Second, 6 * time.Second, 18 * time.Second}
	}
	if cfg.CrashWindow <= 0 {
		cfg.CrashWindow = 30 * time.Minute
	}
	if cfg.CrashThreshold <= 0 {
		cfg.CrashThreshold = 2
	}
	if cfg.ProtoMin <= 0 {
		cfg.ProtoMin = proto.ProtoMin
	}
	if cfg.ProtoMax <= 0 {
		cfg.ProtoMax = proto.ProtoMax
	}
	if cfg.GatewayVersion == "" {
		cfg.GatewayVersion = "dev"
	}
	return &Manager{
		cfg:             cfg,
		store:           cfg.Store,
		dist:            cfg.Dist,
		log:             cfg.Log,
		ssh:             cfg.SSH,
		gatewayID:       cfg.GatewayID,
		gatewayVersion:  cfg.GatewayVersion,
		adminDisabled:   cfg.AdminDisabled,
		healthInterval:  cfg.HealthInterval,
		restartBackoffs: cfg.RestartBackoffs,
		crashWindow:     cfg.CrashWindow,
		crashThreshold:  cfg.CrashThreshold,
		protoMin:        cfg.ProtoMin,
		protoMax:        cfg.ProtoMax,
		conns:           make(map[accountKey]*sharedConn),
		hostConn:        make(map[int64]accountKey),
		ops:             make(map[int64]*runningOp),
	}
}

// Available reports whether corrald binaries are loaded on this gateway.
func (m *Manager) Available() bool {
	return m.Dist() != nil
}

// Dist returns the currently loaded distribution (may be nil).
func (m *Manager) Dist() *Dist {
	m.distMu.RLock()
	defer m.distMu.RUnlock()
	return m.dist
}

// SetDist replaces the loaded distribution (used in tests for upgrade/scope-change scenarios).
func (m *Manager) SetDist(d *Dist) {
	m.distMu.Lock()
	m.dist = d
	m.distMu.Unlock()
}

// SetProtoRange overrides the supported protocol range (used in tests for incompatible version checks).
func (m *Manager) SetProtoRange(minP, maxP int) {
	m.mu.Lock()
	m.protoMin, m.protoMax = minP, maxP
	m.mu.Unlock()
}

// Close shuts down all active corrald connections and waits for background work.
func (m *Manager) Close() {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	m.closed = true
	for _, op := range m.ops {
		if op.cancel != nil {
			op.cancel()
		}
	}
	conns := make([]*sharedConn, 0, len(m.conns))
	for _, sc := range m.conns {
		conns = append(conns, sc)
	}
	m.conns = make(map[accountKey]*sharedConn)
	m.hostConn = make(map[int64]accountKey)
	m.mu.Unlock()

	for _, sc := range conns {
		m.closeSharedConn(sc, false)
	}
	m.wg.Wait()
}

// ClientForCap is the unified fallback entrypoint (spec §9) used by stage 1
// features (T3) to decide whether a host capability should use corrald or fall
// back to the existing SSH path.
func (m *Manager) ClientForCap(userID, hostID int64, capName string) (*Client, bool) {
	if m == nil || m.adminDisabled || !m.Available() {
		return nil, false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	ch, err := m.store.CorraldHostByID(ctx, hostID)
	if err != nil {
		return nil, false
	}
	if EffectiveUserState(toSnapshot(ch), userID) != StateRunning {
		return nil, false
	}
	m.mu.Lock()
	key, ok := m.hostConn[hostID]
	var sc *sharedConn
	if ok {
		sc = m.conns[key]
	}
	m.mu.Unlock()
	if sc == nil {
		return nil, false
	}
	sc.mu.Lock()
	defer sc.mu.Unlock()
	if sc.client == nil || sc.client.Closed() {
		return nil, false
	}
	if capName != "" && sc.health != nil && sc.health.Caps != nil {
		if st, exists := sc.health.Caps[capName]; exists && st != "ok" {
			return nil, false
		}
	}
	return sc.client, true
}

// Readme returns the bilingual documentation text (identical source to README.txt)
// along with the current manifest hash.
func (m *Manager) Readme() (content, manifestHash string, err error) {
	dist := m.Dist()
	if dist == nil {
		return "", "", ErrUnavailable
	}
	text := proto.RenderReadme(proto.ReadmeData{
		Version: dist.Version,
		Release: dist.Release,
		Gateway: m.gatewayID,
	})
	return text, dist.ManifestHash, nil
}

// Status returns the full REST status snapshot for GET /api/hosts/{id}/corrald.
func (m *Manager) Status(ctx context.Context, userID, hostID int64) (*StatusResponse, error) {
	dist := m.Dist()
	if dist == nil {
		return nil, ErrUnavailable
	}
	res := &StatusResponse{
		State:           StateOff,
		GatewayVersion:  dist.Version,
		ManifestHash:    dist.ManifestHash,
		EstimatedSizeMB: dist.EstimatedSizeMB(),
		AdminDisabled:   m.adminDisabled,
		Files:           []string{},
	}

	sshUser, fp, _ := m.ssh.HostBinding(ctx, userID, hostID)
	ch, err := m.store.CorraldHost(ctx, userID, hostID)
	if errors.Is(err, store.ErrNotFound) {
		// Check if another Corral user on this gateway already enabled corrald on
		// the same (fingerprint, sshUser): share the running daemon automatically.
		if fp != "" && sshUser != "" {
			if adopted := m.tryAdoptPeer(ctx, userID, hostID, fp, sshUser); adopted != nil {
				ch = adopted
				err = nil
			}
		}
	}
	if errors.Is(err, store.ErrNotFound) {
		// Still attach latest op if a previous install failed/cancelled cleanly.
		if op, oerr := m.store.LatestCorraldOp(ctx, hostID); oerr == nil {
			res.Op = m.toOpView(op)
			if op.Kind == OpInstall && op.State == OpFailed && op.ErrorCode != "" {
				res.Error = &OpFailure{Code: op.ErrorCode, Detail: extractDetailMessage(op.Detail)}
			}
		}
		return res, nil
	}
	if err != nil {
		return nil, err
	}

	// Enforce authorization binding (fingerprint, username, gateway).
	if ch.State != StateOff && ch.State != StateRemoving && ch.State != StateRemovePending && ch.State != StateInstalling {
		if !CheckBinding(toSnapshot(ch), fp, sshUser, m.gatewayID) {
			if ch.State != StateRebind {
				ch.State = StateRebind
				_ = m.store.UpsertCorraldHost(ctx, ch)
				m.detachHost(hostID)
			}
		}
	}

	res.State = EffectiveUserState(toSnapshot(ch), userID)
	res.Version = ch.Version
	res.Release = ch.Release
	res.LastOKAt = ch.LastOKAt
	res.MachineID = ch.MachineID
	if len(ch.Files) > 0 {
		res.Files = append([]string(nil), ch.Files...)
	}
	if ch.ConsentBy != nil && *ch.ConsentBy > 0 {
		res.Consent = &ConsentInfo{
			By: m.ssh.UserLabel(ctx, *ch.ConsentBy),
			At: ch.ConsentAt,
		}
	}
	if ch.LastError != "" {
		res.Error = decodeLastError(ch.LastError)
	}
	if res.State == StateIncompatible {
		res.CanReplaceWithGateway = true
	}

	// Latest operation.
	if op, oerr := m.store.LatestCorraldOp(ctx, hostID); oerr == nil {
		res.Op = m.toOpView(op)
	}

	// Live connection details (running metrics, caps, scope_change, users).
	sc := m.sharedConnForHost(hostID, fp, sshUser)
	if sc != nil {
		sc.mu.Lock()
		if sc.client != nil && !sc.client.Closed() {
			if sc.health != nil {
				res.Running = &RunningInfo{
					PID:     sc.health.PID,
					UptimeS: sc.health.UptimeS,
					RSSKB:   sc.health.RSSKB,
				}
				if len(sc.health.Caps) > 0 {
					res.Caps = make(map[string]string, len(sc.health.Caps))
					for k, v := range sc.health.Caps {
						res.Caps[k] = v
					}
				}
			} else if sc.hello != nil {
				res.Running = &RunningInfo{PID: sc.hello.PID}
			}
			if sc.scopeChange != nil {
				cp := *sc.scopeChange
				cp.Added = slices.Clone(sc.scopeChange.Added)
				res.ScopeChange = &cp
			}
		}
		var remoteClients []proto.ClientInfo
		if sc.hello != nil {
			remoteClients = slices.Clone(sc.hello.Clients)
		}
		sc.mu.Unlock()
		res.Users = m.computeUsers(ctx, userID, fp, sshUser, remoteClients)
	} else if ch.State != StateOff && fp != "" && sshUser != "" {
		res.Users = m.computeUsers(ctx, userID, fp, sshUser, nil)
	}

	// Also compute scope_change from persisted manifest when stopped/running and dist is newer.
	if res.ScopeChange == nil && ch.Release > 0 && ch.State != StateOff && ch.State != StateIncompatible && ch.State != StateRemoving && ch.State != StateRemovePending {
		if consented, ok := parseManifestJSON(ch.ManifestJSON); ok {
			dec := DecideUpdate(ch.Release, proto.Protocol, consented, dist.Release, dist.Manifest, nil, m.protoMin, m.protoMax)
			if dec.Action == UpdateNeedConsent {
				res.ScopeChange = &ScopeChangeInfo{
					TargetVersion: dist.Version,
					Added:         dec.Added,
				}
			}
		}
	}

	return res, nil
}

// StartInstall authorizes and starts installing corrald on hostID.
func (m *Manager) StartInstall(ctx context.Context, userID, hostID int64, corralUser, manifestHash string, rebind bool) (*OpView, error) {
	dist := m.Dist()
	if dist == nil {
		return nil, ErrUnavailable
	}
	if m.adminDisabled {
		return nil, ErrAdminDisabled
	}
	if manifestHash != "" && manifestHash != dist.ManifestHash {
		return nil, ErrManifestChanged
	}

	ch, err := m.store.CorraldHost(ctx, userID, hostID)
	if errors.Is(err, store.ErrNotFound) {
		ch = &store.CorraldHost{HostID: hostID, State: StateOff}
	} else if err != nil {
		return nil, err
	}

	tr := Transition(toSnapshot(ch), Event{Kind: EvInstallStart})
	if !tr.Allowed {
		if tr.Reason == proto.ErrRemoving {
			return nil, fmt.Errorf("%w: removal in progress", ErrInvalidState)
		}
		return nil, fmt.Errorf("%w: %s", ErrInvalidState, tr.Reason)
	}

	opID := newHexID()
	op := &store.CorraldOp{
		ID:        opID,
		HostID:    hostID,
		Kind:      OpInstall,
		State:     OpRunning,
		Step:      "check",
		CreatedBy: userID,
	}
	if err := m.store.CreateCorraldOp(ctx, op, false); err != nil {
		return nil, err
	}

	ch.State = StateInstalling
	ch.LastError = ""
	ch.RestartCount = 0
	if err := m.store.UpsertCorraldHost(ctx, ch); err != nil {
		return nil, err
	}

	opCtx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	rop := &runningOp{opID: opID, kind: OpInstall, step: "check", cancel: cancel, done: make(chan struct{})}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		cancel()
		return nil, errors.New("manager closed")
	}
	m.ops[hostID] = rop
	m.wg.Add(1)
	m.mu.Unlock()

	view := m.toOpView(op)
	go func() {
		defer m.wg.Done()
		defer cancel()
		defer close(rop.done)
		defer m.clearRunningOp(hostID, rop)
		m.runInstallOp(opCtx, rop, op, ch, dist, userID, hostID, corralUser)
	}()

	return view, nil
}

// CancelInstall cancels an active install operation before the start step.
func (m *Manager) CancelInstall(ctx context.Context, userID, hostID int64) (*OpView, error) {
	if !m.Available() {
		return nil, ErrUnavailable
	}
	op, err := m.store.ActiveCorraldOp(ctx, hostID)
	if err != nil {
		return nil, ErrCannotCancel
	}
	if !StepCancellable(op.Kind, op.Step) {
		return nil, ErrCannotCancel
	}

	m.mu.Lock()
	rop := m.ops[hostID]
	if rop != nil && rop.opID == op.ID {
		if !StepCancellable(rop.kind, rop.step) {
			m.mu.Unlock()
			return nil, ErrCannotCancel
		}
		rop.cancelled = true
		if rop.cancel != nil {
			rop.cancel()
		}
	}
	m.mu.Unlock()

	if rop != nil {
		select {
		case <-rop.done:
		case <-time.After(5 * time.Second):
		case <-ctx.Done():
		}
	} else {
		// No in-memory goroutine (e.g. post-restart stale op); cancel directly in store.
		op.State = OpCancelled
		_ = m.store.UpdateCorraldOp(ctx, op)
		if ch, err := m.store.CorraldHostByID(ctx, hostID); err == nil && ch.State == StateInstalling {
			ch.State = StateOff
			_ = m.store.UpsertCorraldHost(ctx, ch)
		}
	}
	latest, err := m.store.LatestCorraldOp(ctx, hostID)
	if err != nil {
		return m.toOpView(op), nil
	}
	return m.toOpView(latest), nil
}

// Pause pauses corrald usage for userID on hostID only. If no other users
// remain on this host account, the daemon process is also stopped.
func (m *Manager) Pause(ctx context.Context, userID, hostID int64, corralUser string) error {
	if !m.Available() {
		return ErrUnavailable
	}
	ch, err := m.store.CorraldHost(ctx, userID, hostID)
	if err != nil {
		return ErrInvalidState
	}
	tr := Transition(toSnapshot(ch), Event{Kind: EvPause, UserID: userID})
	if !tr.Allowed {
		return ErrInvalidState
	}
	ch, err = m.store.SetCorraldUserPaused(ctx, hostID, userID, true)
	if err != nil {
		return err
	}

	sshUser, fp, _ := m.ssh.HostBinding(ctx, userID, hostID)
	if fp == "" {
		fp = ch.BoundFingerprint
	}
	if sshUser == "" {
		sshUser = ch.BoundUsername
	}
	sc := m.sharedConnForHost(hostID, fp, sshUser)
	if sc != nil {
		sc.mu.Lock()
		client := sc.client
		delete(sc.hostRefs, hostID)
		sc.mu.Unlock()

		remaining := -1
		if client != nil && !client.Closed() {
			if fres, ferr := client.Forget(ctx, proto.ForgetParams{
				Gateway:    m.gatewayID,
				CorralUser: corralUser,
				HostID:     strconv.FormatInt(hostID, 10),
			}); ferr == nil && fres != nil {
				remaining = fres.RemainingClients
			}
		}

		// Also verify whether any other non-paused peer exists on this gateway.
		otherActive := m.hasOtherActivePeers(ctx, userID, fp, sshUser)
		if !otherActive && remaining <= 0 {
			if client != nil && !client.Closed() {
				_, _ = client.Shutdown(ctx, "paused_no_users")
			}
			m.closeSharedConn(sc, true)
			ch.State = StateStopped
			_ = m.store.UpsertCorraldHost(ctx, ch)
		}
	}
	return nil
}

// Resume re-enables corrald for userID on hostID without re-consent.
func (m *Manager) Resume(ctx context.Context, userID, hostID int64, corralUser string) error {
	if !m.Available() {
		return ErrUnavailable
	}
	ch, err := m.store.CorraldHost(ctx, userID, hostID)
	if err != nil {
		return ErrInvalidState
	}
	ch, err = m.store.SetCorraldUserPaused(ctx, hostID, userID, false)
	if err != nil {
		return err
	}
	if ch.State == StatePaused {
		ch.State = StateStopped
		_ = m.store.UpsertCorraldHost(ctx, ch)
	}
	// Best-effort reconnect/start immediately if host is reachable.
	sshClient, release, fp, sshUser, err := m.ssh.AcquireSSH(ctx, userID, hostID)
	if err == nil && sshClient != nil {
		defer release()
		m.OnHostConnected(ctx, userID, hostID, corralUser, fp, sshUser, sshClient)
	}
	return nil
}

// StartRemove initiates withdrawal-first removal of corrald for all users on
// this host account.
func (m *Manager) StartRemove(ctx context.Context, userID, hostID int64) (*OpView, error) {
	if !m.Available() {
		return nil, ErrUnavailable
	}
	ch, err := m.store.CorraldHost(ctx, userID, hostID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, ErrInvalidState
	}
	if err != nil {
		return nil, err
	}
	tr := Transition(toSnapshot(ch), Event{Kind: EvRemoveStart})
	if !tr.Allowed {
		return nil, ErrInvalidState
	}

	// Cancel any active in-memory install/update op first (withdrawal-first).
	m.mu.Lock()
	if prev := m.ops[hostID]; prev != nil && (prev.kind == OpInstall || prev.kind == OpUpdate) {
		prev.cancelled = true
		if prev.cancel != nil {
			prev.cancel()
		}
	}
	m.mu.Unlock()

	opID := newHexID()
	op := &store.CorraldOp{
		ID:        opID,
		HostID:    hostID,
		Kind:      OpRemove,
		State:     OpRunning,
		Step:      "mark",
		CreatedBy: userID,
	}
	if err := m.store.CreateCorraldOp(ctx, op, true); err != nil {
		return nil, err
	}

	ch.State = StateRemoving
	ch.LastError = ""
	if err := m.store.UpsertCorraldHost(ctx, ch); err != nil {
		return nil, err
	}

	opCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	rop := &runningOp{opID: opID, kind: OpRemove, step: "mark", cancel: cancel, done: make(chan struct{})}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		cancel()
		return nil, errors.New("manager closed")
	}
	m.ops[hostID] = rop
	m.wg.Add(1)
	m.mu.Unlock()

	view := m.toOpView(op)
	go func() {
		defer m.wg.Done()
		defer cancel()
		defer close(rop.done)
		defer m.clearRunningOp(hostID, rop)
		m.runRemoveOp(opCtx, rop, op, ch, userID, hostID)
	}()

	return view, nil
}

// StartUpdate starts an explicit user-approved update (after scope growth or
// when replacing an incompatible daemon with the gateway's bundled version).
func (m *Manager) StartUpdate(ctx context.Context, userID, hostID int64, corralUser, manifestHash string, replace bool) (*OpView, error) {
	dist := m.Dist()
	if dist == nil {
		return nil, ErrUnavailable
	}
	if m.adminDisabled {
		return nil, ErrAdminDisabled
	}
	if manifestHash != "" && manifestHash != dist.ManifestHash {
		return nil, ErrManifestChanged
	}
	ch, err := m.store.CorraldHost(ctx, userID, hostID)
	if err != nil {
		return nil, ErrInvalidState
	}
	tr := Transition(toSnapshot(ch), Event{Kind: EvUpdateStart})
	if !tr.Allowed {
		return nil, ErrInvalidState
	}

	// Persist newly consented manifest before launching update.
	mb, _ := proto.MarshalManifest(dist.Manifest)
	ch.ManifestJSON = string(mb)
	ch.State = StateUpdating
	ch.LastError = ""
	if err := m.store.UpsertCorraldHost(ctx, ch); err != nil {
		return nil, err
	}

	return m.launchUpdateOp(ctx, userID, hostID, corralUser, ch, dist, replace)
}

// Retry resets error/restart counters and attempts to reconnect/start corrald.
func (m *Manager) Retry(ctx context.Context, userID, hostID int64, corralUser string) (*OpView, error) {
	dist := m.Dist()
	if dist == nil {
		return nil, ErrUnavailable
	}
	if m.adminDisabled {
		return nil, ErrAdminDisabled
	}
	ch, err := m.store.CorraldHost(ctx, userID, hostID)
	if err != nil {
		return nil, ErrInvalidState
	}
	tr := Transition(toSnapshot(ch), Event{Kind: EvRetry})
	if !tr.Allowed {
		return nil, ErrInvalidState
	}
	ch.State = tr.Next.State
	ch.RestartCount = 0
	ch.LastError = ""
	if err := m.store.UpsertCorraldHost(ctx, ch); err != nil {
		return nil, err
	}

	opID := newHexID()
	op := &store.CorraldOp{
		ID:        opID,
		HostID:    hostID,
		Kind:      OpInstall,
		State:     OpRunning,
		Step:      "start",
		CreatedBy: userID,
	}
	if err := m.store.CreateCorraldOp(ctx, op, false); err != nil {
		return nil, err
	}

	opCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	rop := &runningOp{opID: opID, kind: OpInstall, step: "start", cancel: cancel, done: make(chan struct{})}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		cancel()
		return nil, errors.New("manager closed")
	}
	m.ops[hostID] = rop
	m.wg.Add(1)
	m.mu.Unlock()

	view := m.toOpView(op)
	go func() {
		defer m.wg.Done()
		defer cancel()
		defer close(rop.done)
		defer m.clearRunningOp(hostID, rop)

		sshClient, release, fp, sshUser, err := m.ssh.AcquireSSH(opCtx, userID, hostID)
		if err != nil {
			m.finishOpFail(op, ch, &OpFailure{Code: "disconnected", Detail: err.Error(), KeepInstalled: true})
			return
		}
		defer release()

		probe, err := m.probeHost(opCtx, sshClient)
		if err != nil {
			m.finishOpFail(op, ch, &OpFailure{Code: "disconnected", Detail: err.Error(), KeepInstalled: true})
			return
		}
		if probe.HasTombstone {
			m.finishOpFail(op, ch, &OpFailure{Code: proto.ErrRemoving, KeepInstalled: true})
			return
		}
		if !probe.HasSymlink {
			// Binary missing; run full install.
			m.runInstallWithSSH(opCtx, rop, op, ch, dist, userID, hostID, corralUser, fp, sshUser, sshClient)
			return
		}
		_, sock, fail := RunServe(opCtx, sshClient, probe.CurrentBin, m.gatewayID, probe.MachineID, probe.OS, probe.Arch)
		if fail != nil {
			m.finishOpFail(op, ch, fail)
			return
		}
		if sock == "" {
			sock = probe.SocketPath
		}
		if err := m.attachAndHello(opCtx, userID, hostID, corralUser, fp, sshUser, sshClient, probe, sock, ch); err != nil {
			m.finishOpFail(op, ch, &OpFailure{Code: "start_failed", Detail: err.Error(), MachineID: probe.MachineID, KeepInstalled: true})
			return
		}
		op.State = OpDone
		_ = m.store.UpdateCorraldOp(context.Background(), op)
	}()

	return view, nil
}

// OnHostConnected is called when an SSH connection is established to a host.
// It reconciles interrupted ops / remove_pending, enforces binding, connects to
// (or starts) corrald, checks version compatibility, and triggers auto-updates.
func (m *Manager) OnHostConnected(ctx context.Context, userID, hostID int64, corralUser, fingerprint, sshUser string, sshClient *ssh.Client) {
	if m == nil || !m.Available() || sshClient == nil {
		return
	}
	ch, err := m.store.CorraldHost(ctx, userID, hostID)
	if errors.Is(err, store.ErrNotFound) {
		if fingerprint != "" && sshUser != "" {
			ch = m.tryAdoptPeer(ctx, userID, hostID, fingerprint, sshUser)
		}
		if ch == nil {
			return
		}
	} else if err != nil {
		return
	}

	if m.adminDisabled {
		// Admin disabled: do not start; if running, stop when no other gateway uses it.
		return
	}

	// Check authorization binding (unless removing/remove_pending/off).
	if ch.State != StateOff && ch.State != StateRemoving && ch.State != StateRemovePending {
		if !CheckBinding(toSnapshot(ch), fingerprint, sshUser, m.gatewayID) {
			ch.State = StateRebind
			_ = m.store.UpsertCorraldHost(ctx, ch)
			m.detachHost(hostID)
			return
		}
	}

	// Check if there is an unfinished op from before a gateway restart, or remove_pending.
	m.mu.Lock()
	_, inMemoryOp := m.ops[hostID]
	m.mu.Unlock()
	if inMemoryOp {
		return
	}

	activeOp, activeErr := m.store.ActiveCorraldOp(ctx, hostID)
	needsReconcile := activeErr == nil || ch.State == StateRemovePending || ch.State == StateRemoving || ch.State == StateInstalling || ch.State == StateUpdating
	if needsReconcile {
		m.reconcileHost(ctx, userID, hostID, corralUser, fingerprint, sshUser, sshClient, ch, activeOp)
		// Reload after reconciliation.
		ch, err = m.store.CorraldHostByID(ctx, hostID)
		if err != nil {
			return
		}
	}

	// Nothing to connect if off, rebind, removing, remove_pending, or paused for this user.
	eff := EffectiveUserState(toSnapshot(ch), userID)
	switch eff {
	case StateOff, StateRebind, StateRemoving, StateRemovePending, StatePaused:
		return
	}

	dist := m.Dist()
	// In StateError after exhausted restarts, do NOT auto-restart the same release;
	// only retry if the gateway now carries a strictly newer Release.
	if ch.State == StateError && ch.RestartCount >= MaxAutoRestarts {
		if dist == nil || !proto.ShouldUpgrade(ch.Release, dist.Release) {
			return
		}
		ch.RestartCount = 0
		ch.LastError = ""
		_ = m.store.UpsertCorraldHost(ctx, ch)
	}

	// If a shared connection is already live, register this host/user on it!
	key := accountKey{Fingerprint: fingerprint, Username: sshUser}
	if sc := m.getSharedConn(key); sc != nil {
		sc.mu.Lock()
		live := sc.client != nil && !sc.client.Closed()
		client := sc.client
		sc.mu.Unlock()
		if live {
			hello, err := client.Hello(ctx, proto.HelloParams{
				Gateway:        m.gatewayID,
				GatewayVersion: m.gatewayVersion,
				ProtoMin:       m.protoMin,
				ProtoMax:       m.protoMax,
				Client:         proto.ClientIdentity{CorralUser: corralUser, HostID: strconv.FormatInt(hostID, 10)},
			})
			if err == nil {
				sc.mu.Lock()
				sc.hello = hello
				sc.hostRefs[hostID] = &hostRef{HostID: hostID, OwnerID: userID, CorralUser: corralUser}
				sc.mu.Unlock()
				m.mu.Lock()
				m.hostConn[hostID] = key
				m.mu.Unlock()
				ch.State = StateRunning
				ch.Version = hello.Version
				ch.Release = hello.Release
				ch.LastOKAt = time.Now().Unix()
				_ = m.store.UpsertCorraldHost(ctx, ch)
				m.evaluateUpdateAfterConnect(ctx, userID, hostID, corralUser, fingerprint, sshUser, nil, nil)
				return
			}
		}
	}

	// Probe host to discover socket, check tombstone, and start daemon if stopped.
	probe, err := m.probeHost(ctx, sshClient)
	if err != nil {
		return
	}
	if probe.HasTombstone {
		m.reconcileHost(ctx, userID, hostID, corralUser, fingerprint, sshUser, sshClient, ch, nil)
		return
	}
	if !probe.HasSymlink {
		ch.State = StateOff
		_ = m.store.UpsertCorraldHost(ctx, ch)
		return
	}

	files := ReadHostStateFiles(ctx, sshClient, probe, "")
	sock := probe.SocketPath
	if files.Meta != nil && files.Meta.SocketPath != "" {
		sock = files.Meta.SocketPath
	}
	if files.Install != nil && len(files.Install.Files) > 0 {
		ch.Files = files.Install.Files
	}
	ch.MachineID = probe.MachineID

	// Try connecting directly first; if not listening, start via `corrald serve`.
	if err := m.attachAndHello(ctx, userID, hostID, corralUser, fingerprint, sshUser, sshClient, probe, sock, ch); err != nil {
		var rpcErr *proto.RPCError
		if errors.As(err, &rpcErr) && rpcErr.Code == proto.ErrProtoMismatch {
			return // already transitioned to StateIncompatible
		}
		_, startedSock, fail := RunServe(ctx, sshClient, probe.CurrentBin, m.gatewayID, probe.MachineID, probe.OS, probe.Arch)
		if fail != nil {
			if fail.Code == proto.ErrRemoving {
				m.reconcileHost(ctx, userID, hostID, corralUser, fingerprint, sshUser, sshClient, ch, nil)
				return
			}
			ch.State = StateError
			ch.LastError = encodeLastError(fail)
			_ = m.store.UpsertCorraldHost(ctx, ch)
			return
		}
		if startedSock != "" {
			sock = startedSock
		}
		if err := m.attachAndHello(ctx, userID, hostID, corralUser, fingerprint, sshUser, sshClient, probe, sock, ch); err != nil {
			if !errors.As(err, &rpcErr) || rpcErr.Code != proto.ErrProtoMismatch {
				ch.State = StateError
				ch.LastError = encodeLastError(&OpFailure{Code: "start_failed", Detail: err.Error(), MachineID: probe.MachineID})
				_ = m.store.UpsertCorraldHost(ctx, ch)
			}
			return
		}
	}

	// After connecting cleanly, evaluate automatic update or scope-change prompt.
	m.evaluateUpdateAfterConnect(ctx, userID, hostID, corralUser, fingerprint, sshUser, probe, files.Failed)
}

// DeleteHostCleanup handles corrald cleanup during DELETE /api/hosts/{id}.
func (m *Manager) DeleteHostCleanup(ctx context.Context, userID, hostID int64, corralUser, fingerprint, sshUser string, removeCorrald bool, sshClient *ssh.Client) *DeleteCorraldOutcome {
	ch, err := m.store.CorraldHost(ctx, userID, hostID)
	if errors.Is(err, store.ErrNotFound) || ch == nil || ch.State == StateOff {
		if !removeCorrald {
			return &DeleteCorraldOutcome{State: "skipped"}
		}
		return &DeleteCorraldOutcome{State: "skipped"}
	}
	if err != nil {
		return &DeleteCorraldOutcome{State: "error", Error: err.Error()}
	}

	if fp := ch.BoundFingerprint; fp != "" && fingerprint == "" {
		fingerprint = fp
	}
	if u := ch.BoundUsername; u != "" && sshUser == "" {
		sshUser = u
	}

	if !removeCorrald {
		// Unregister this user from the running daemon (best-effort) without removing files.
		if sc := m.sharedConnForHost(hostID, fingerprint, sshUser); sc != nil {
			sc.mu.Lock()
			client := sc.client
			delete(sc.hostRefs, hostID)
			sc.mu.Unlock()
			if client != nil && !client.Closed() {
				fctx, cancel := context.WithTimeout(ctx, 5*time.Second)
				_, _ = client.Forget(fctx, proto.ForgetParams{
					Gateway:    m.gatewayID,
					CorralUser: corralUser,
					HostID:     strconv.FormatInt(hostID, 10),
				})
				cancel()
			}
		}
		m.detachHost(hostID)
		return &DeleteCorraldOutcome{State: "skipped"}
	}

	if sshClient == nil {
		m.detachHost(hostID)
		return &DeleteCorraldOutcome{
			State:        "unreachable",
			UninstallCmd: DefaultUninstallCmd,
		}
	}

	probe, err := m.probeHost(ctx, sshClient)
	if err != nil {
		m.detachHost(hostID)
		return &DeleteCorraldOutcome{
			State:        "unreachable",
			Error:        err.Error(),
			UninstallCmd: DefaultUninstallCmd,
		}
	}

	// Close gateway connection to daemon before uninstalling.
	if sc := m.sharedConnForHost(hostID, fingerprint, sshUser); sc != nil {
		m.closeSharedConn(sc, true)
	}
	m.detachHost(hostID)

	out := RunUninstall(ctx, sshClient, probe, newHexID(), nil)
	switch out.Result {
	case "removed":
		_ = m.store.CorraldResetByBinding(ctx, fingerprint, sshUser)
		return &DeleteCorraldOutcome{State: "removed"}
	case "partial":
		return &DeleteCorraldOutcome{State: "partial", Left: out.Left, UninstallCmd: DefaultUninstallCmd}
	case "stop_failed":
		return &DeleteCorraldOutcome{State: "stop_failed", PID: out.PID, Error: out.Detail, UninstallCmd: DefaultUninstallCmd}
	default:
		return &DeleteCorraldOutcome{State: "remove_pending", Error: out.Detail, UninstallCmd: DefaultUninstallCmd}
	}
}

// CleanupUserHosts handles corrald cleanup when a Corral user account is
// deleted or permanently disabled: for every host owned by userID, unregisters
// them from the daemon; keeps the daemon running if another user still shares
// the same host account, otherwise removes it (best-effort when reachable).
func (m *Manager) CleanupUserHosts(ctx context.Context, userID int64, corralUser string) {
	if m == nil {
		return
	}
	hosts, states, err := m.store.ListUserCorraldHosts(ctx, userID)
	if err != nil || len(hosts) == 0 {
		return
	}
	for i, h := range hosts {
		ch := states[i]
		fp := ch.BoundFingerprint
		sshUser := ch.BoundUsername
		if sshUser == "" {
			sshUser = h.Username
		}
		otherPeers := m.hasOtherActivePeers(ctx, userID, fp, sshUser)

		// Check remote clients on shared connection as well.
		remainingRemote := 0
		if sc := m.sharedConnForHost(h.ID, fp, sshUser); sc != nil {
			sc.mu.Lock()
			client := sc.client
			delete(sc.hostRefs, h.ID)
			sc.mu.Unlock()
			if client != nil && !client.Closed() {
				fctx, cancel := context.WithTimeout(ctx, 5*time.Second)
				if fres, ferr := client.Forget(fctx, proto.ForgetParams{
					Gateway:    m.gatewayID,
					CorralUser: corralUser,
					HostID:     strconv.FormatInt(h.ID, 10),
				}); ferr == nil && fres != nil {
					remainingRemote = fres.RemainingClients
				}
				cancel()
			}
		}

		if otherPeers || remainingRemote > 0 {
			// Keep the helper daemon running for the remaining users.
			m.detachHost(h.ID)
			continue
		}

		// No other users: try to connect and uninstall cleanly.
		sshClient, release, _, _, err := m.ssh.AcquireSSH(ctx, userID, h.ID)
		if err == nil && sshClient != nil {
			m.DeleteHostCleanup(ctx, userID, h.ID, corralUser, fp, sshUser, true, sshClient)
			release()
		} else {
			m.detachHost(h.ID)
		}
	}
}

// --- Internal execution flows ---

func (m *Manager) runInstallOp(ctx context.Context, rop *runningOp, op *store.CorraldOp, ch *store.CorraldHost, dist *Dist, userID, hostID int64, corralUser string) {
	sshClient, release, fp, sshUser, err := m.ssh.AcquireSSH(ctx, userID, hostID)
	if err != nil {
		if m.isOpCancelled(rop) {
			m.finishOpCancelled(op, ch)
			return
		}
		m.finishOpFail(op, ch, &OpFailure{Code: "disconnected", Detail: err.Error()})
		return
	}
	defer release()

	m.runInstallWithSSH(ctx, rop, op, ch, dist, userID, hostID, corralUser, fp, sshUser, sshClient)
}

func (m *Manager) runInstallWithSSH(ctx context.Context, rop *runningOp, op *store.CorraldOp, ch *store.CorraldHost, dist *Dist, userID, hostID int64, corralUser, fp, sshUser string, sshClient *ssh.Client) {
	setStep := func(step string) {
		m.mu.Lock()
		rop.step = step
		m.mu.Unlock()
		op.Step = step
		_ = m.store.UpdateCorraldOp(context.Background(), op)
	}
	isCancelled := func() bool { return m.isOpCancelled(rop) }

	setStep("check")
	probe, err := m.probeHost(ctx, sshClient)
	if err != nil {
		if isCancelled() {
			m.finishOpCancelled(op, ch)
			return
		}
		m.finishOpFail(op, ch, &OpFailure{Code: "disconnected", Detail: err.Error()})
		return
	}
	bin, vfail := ValidateProbe(probe, dist)
	if vfail != nil {
		m.finishOpFail(op, ch, vfail)
		return
	}
	if isCancelled() {
		m.finishOpCancelled(op, ch)
		return
	}

	_, sfail := StageAndInstall(ctx, sshClient, dist, probe, bin, op.ID, m.gatewayID, corralUser, false, setStep, isCancelled, m.cfg.VerifySHA256Hook)
	if sfail != nil {
		if sfail.Code == "cancelled" || isCancelled() {
			m.finishOpCancelled(op, ch)
			return
		}
		m.finishOpFail(op, ch, sfail)
		return
	}

	if m.cfg.BeforeInstallStart != nil {
		m.cfg.BeforeInstallStart(hostID, op.ID)
	}
	if isCancelled() {
		// Cancelled right before start: uninstall so host is clean.
		_ = RunUninstall(context.Background(), sshClient, probe, op.ID, nil)
		m.finishOpCancelled(op, ch)
		return
	}

	// Step: start (no longer cancellable past this point).
	setStep("start")
	_, sock, serveFail := RunServe(ctx, sshClient, probe.CurrentBin, m.gatewayID, probe.MachineID, probe.OS, probe.Arch)
	if serveFail != nil {
		ch.MachineID = probe.MachineID
		m.finishOpFail(op, ch, serveFail)
		return
	}
	if sock == "" {
		sock = probe.SocketPath
	}

	// Save binding & consent metadata before hello.
	mb, _ := proto.MarshalManifest(dist.Manifest)
	uid := userID
	ch.ConsentBy = &uid
	ch.ConsentAt = time.Now().Unix()
	ch.BoundFingerprint = fp
	ch.BoundUsername = sshUser
	ch.BoundGateway = m.gatewayID
	ch.ManifestJSON = string(mb)
	ch.MachineID = probe.MachineID
	ch.SocketPath = sock

	files := ReadHostStateFiles(ctx, sshClient, probe, op.ID)
	if files.Install != nil && len(files.Install.Files) > 0 {
		ch.Files = files.Install.Files
	}

	if err := m.attachAndHello(ctx, userID, hostID, corralUser, fp, sshUser, sshClient, probe, sock, ch); err != nil {
		var rpcErr *proto.RPCError
		if errors.As(err, &rpcErr) && rpcErr.Code == proto.ErrProtoMismatch {
			op.State = OpFailed
			op.ErrorCode = proto.ErrProtoMismatch
			_ = m.store.UpdateCorraldOp(context.Background(), op)
			return
		}
		m.finishOpFail(op, ch, &OpFailure{
			Code:          "start_failed",
			Detail:        err.Error(),
			MachineID:     probe.MachineID,
			OS:            probe.OS,
			Arch:          probe.Arch,
			KeepInstalled: true,
		})
		return
	}

	op.State = OpDone
	op.ErrorCode = ""
	_ = m.store.UpdateCorraldOp(context.Background(), op)
}

func (m *Manager) runRemoveOp(ctx context.Context, rop *runningOp, op *store.CorraldOp, ch *store.CorraldHost, userID, hostID int64) {
	setStep := func(step string) {
		m.mu.Lock()
		rop.step = step
		m.mu.Unlock()
		op.Step = step
		_ = m.store.UpdateCorraldOp(context.Background(), op)
	}

	sshClient, release, fp, sshUser, err := m.ssh.AcquireSSH(ctx, userID, hostID)
	if err != nil {
		// Unreachable: mark remove_pending so next connect finishes removal.
		m.markRemovePending(op, ch, err.Error())
		return
	}
	defer release()
	if fp == "" {
		fp = ch.BoundFingerprint
	}
	if sshUser == "" {
		sshUser = ch.BoundUsername
	}

	// Close any active gateway connection to the daemon before uninstalling.
	if sc := m.sharedConnForHost(hostID, fp, sshUser); sc != nil {
		m.closeSharedConn(sc, true)
	}
	m.detachHost(hostID)

	probe, err := m.probeHost(ctx, sshClient)
	if err != nil {
		m.markRemovePending(op, ch, err.Error())
		return
	}

	out := RunUninstall(ctx, sshClient, probe, op.ID, setStep)
	switch out.Result {
	case "removed":
		_ = m.store.CorraldResetByBinding(context.Background(), fp, sshUser)
		ch.State = StateOff
		ch.Version = ""
		ch.Release = 0
		ch.SocketPath = ""
		ch.Files = []string{}
		ch.PausedUsers = []int64{}
		ch.LastError = ""
		ch.RestartCount = 0
		_ = m.store.UpsertCorraldHost(context.Background(), ch)
		op.State = OpDone
		op.ErrorCode = ""
		op.Detail = encodeOpDetail(opDetailPayload{Result: "removed"})
		_ = m.store.UpdateCorraldOp(context.Background(), op)
	case "partial":
		fail := &OpFailure{Code: "partial", Left: out.Left}
		ch.State = StateError
		ch.LastError = encodeLastError(fail)
		_ = m.store.UpsertCorraldHost(context.Background(), ch)
		op.State = OpFailed
		op.ErrorCode = "partial"
		op.Detail = encodeOpDetail(opDetailPayload{Result: "partial", Left: out.Left})
		_ = m.store.UpdateCorraldOp(context.Background(), op)
	case "stop_failed":
		fail := &OpFailure{Code: "stop_failed", PID: out.PID, Detail: out.Detail}
		ch.State = StateError
		ch.LastError = encodeLastError(fail)
		_ = m.store.UpsertCorraldHost(context.Background(), ch)
		op.State = OpFailed
		op.ErrorCode = "stop_failed"
		op.Detail = encodeOpDetail(opDetailPayload{Result: "stop_failed", PID: out.PID, Message: out.Detail})
		_ = m.store.UpdateCorraldOp(context.Background(), op)
	default:
		m.markRemovePending(op, ch, out.Detail)
	}
}

func (m *Manager) markRemovePending(op *store.CorraldOp, ch *store.CorraldHost, reason string) {
	ch.State = StateRemovePending
	ch.LastError = ""
	_ = m.store.UpsertCorraldHost(context.Background(), ch)
	op.State = OpUnknown
	op.ErrorCode = ""
	op.Detail = encodeOpDetail(opDetailPayload{Result: "pending", Message: reason})
	_ = m.store.UpdateCorraldOp(context.Background(), op)
}

func (m *Manager) launchUpdateOp(ctx context.Context, userID, hostID int64, corralUser string, ch *store.CorraldHost, dist *Dist, replace bool) (*OpView, error) {
	opID := newHexID()
	op := &store.CorraldOp{
		ID:        opID,
		HostID:    hostID,
		Kind:      OpUpdate,
		State:     OpRunning,
		Step:      "upload",
		CreatedBy: userID,
	}
	if err := m.store.CreateCorraldOp(ctx, op, false); err != nil {
		return nil, err
	}

	opCtx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	rop := &runningOp{opID: opID, kind: OpUpdate, step: "upload", cancel: cancel, done: make(chan struct{})}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		cancel()
		return nil, errors.New("manager closed")
	}
	m.ops[hostID] = rop
	m.wg.Add(1)
	m.mu.Unlock()

	view := m.toOpView(op)
	go func() {
		defer m.wg.Done()
		defer cancel()
		defer close(rop.done)
		defer m.clearRunningOp(hostID, rop)
		m.runUpdateOp(opCtx, rop, op, ch, dist, userID, hostID, corralUser, replace)
	}()

	return view, nil
}

func (m *Manager) runUpdateOp(ctx context.Context, rop *runningOp, op *store.CorraldOp, ch *store.CorraldHost, dist *Dist, userID, hostID int64, corralUser string, replace bool) {
	setStep := func(step string) {
		m.mu.Lock()
		rop.step = step
		m.mu.Unlock()
		op.Step = step
		_ = m.store.UpdateCorraldOp(context.Background(), op)
	}

	prevVer := ch.Version
	prevRel := ch.Release

	sshClient, release, fp, sshUser, err := m.ssh.AcquireSSH(ctx, userID, hostID)
	if err != nil {
		m.finishUpdateFailKeepPrev(op, ch, prevVer, prevRel, dist.Version, err.Error())
		return
	}
	defer release()

	probe, err := m.probeHost(ctx, sshClient)
	if err != nil {
		m.finishUpdateFailKeepPrev(op, ch, prevVer, prevRel, dist.Version, err.Error())
		return
	}
	bin, vfail := ValidateProbe(probe, dist)
	if vfail != nil {
		m.finishUpdateFailKeepPrev(op, ch, prevVer, prevRel, dist.Version, vfail.Error())
		return
	}

	// Replace mode (from incompatible state): stop current daemon, install non-upgrade, start.
	if replace {
		if sc := m.sharedConnForHost(hostID, fp, sshUser); sc != nil {
			m.closeSharedConn(sc, true)
		}
		_, _ = runSSH(ctx, sshClient, StopCommand(probe.CurrentBin), 4096, sshx.ExecOptions{})
		_, sfail := StageAndInstall(ctx, sshClient, dist, probe, bin, op.ID, m.gatewayID, corralUser, false, setStep, nil, m.cfg.VerifySHA256Hook)
		if sfail != nil {
			ch.State = StateError
			ch.LastError = encodeLastError(sfail)
			_ = m.store.UpsertCorraldHost(context.Background(), ch)
			op.State = OpFailed
			op.ErrorCode = sfail.Code
			_ = m.store.UpdateCorraldOp(context.Background(), op)
			return
		}
		setStep("handoff")
		_, sock, serveFail := RunServe(ctx, sshClient, probe.CurrentBin, m.gatewayID, probe.MachineID, probe.OS, probe.Arch)
		if serveFail != nil {
			ch.State = StateError
			ch.LastError = encodeLastError(serveFail)
			_ = m.store.UpsertCorraldHost(context.Background(), ch)
			op.State = OpFailed
			op.ErrorCode = serveFail.Code
			_ = m.store.UpdateCorraldOp(context.Background(), op)
			return
		}
		setStep("verify")
		if sock == "" {
			sock = probe.SocketPath
		}
		if err := m.attachAndHello(ctx, userID, hostID, corralUser, fp, sshUser, sshClient, probe, sock, ch); err != nil {
			ch.State = StateError
			ch.LastError = encodeLastError(&OpFailure{Code: "start_failed", Detail: err.Error()})
			_ = m.store.UpsertCorraldHost(context.Background(), ch)
			op.State = OpFailed
			op.ErrorCode = "start_failed"
			_ = m.store.UpdateCorraldOp(context.Background(), op)
			return
		}
		op.State = OpDone
		_ = m.store.UpdateCorraldOp(context.Background(), op)
		return
	}

	// Normal hot-update path: stage & install with --upgrade (symlink untouched until v2 READY).
	prevRelTarget := probe.CurrentTarget
	if prevRelTarget == "" && prevVer != "" && prevRel > 0 {
		prevRelTarget = fmt.Sprintf("corrald-%s-%d/corrald", prevVer, prevRel)
	}

	installedBin, sfail := StageAndInstall(ctx, sshClient, dist, probe, bin, op.ID, m.gatewayID, corralUser, true, setStep, nil, m.cfg.VerifySHA256Hook)
	if sfail != nil {
		m.finishUpdateFailKeepPrev(op, ch, prevVer, prevRel, dist.Version, sfail.Error())
		return
	}

	setStep("handoff")
	sc := m.sharedConnForHost(hostID, fp, sshUser)
	var client *Client
	if sc != nil {
		sc.mu.Lock()
		sc.updating = true
		client = sc.client
		sc.mu.Unlock()
		defer func() {
			sc.mu.Lock()
			sc.updating = false
			sc.mu.Unlock()
		}()
	}

	if client == nil || client.Closed() {
		// Daemon is not running; switch symlink and cold-start new version.
		relTarget := fmt.Sprintf("corrald-%s-%d/corrald", dist.Version, dist.Release)
		_, _ = runSSH(ctx, sshClient, SwitchSymlinkCommand(probe.LibDir, relTarget), 4096, sshx.ExecOptions{})
		_, sock, serveFail := RunServe(ctx, sshClient, probe.CurrentBin, m.gatewayID, probe.MachineID, probe.OS, probe.Arch)
		if serveFail != nil {
			_ = RecordFailedOnHost(ctx, sshClient, probe.FailedJSON, dist.Release, serveFail.Error())
			if prevRelTarget != "" {
				_, _ = runSSH(ctx, sshClient, SwitchSymlinkCommand(probe.LibDir, prevRelTarget), 4096, sshx.ExecOptions{})
				_, _, _ = RunServe(ctx, sshClient, probe.CurrentBin, m.gatewayID, probe.MachineID, probe.OS, probe.Arch)
			}
			m.finishUpdateFailKeepPrev(op, ch, prevVer, prevRel, dist.Version, serveFail.Error())
			return
		}
		if sock == "" {
			sock = probe.SocketPath
		}
		if err := m.attachAndHello(ctx, userID, hostID, corralUser, fp, sshUser, sshClient, probe, sock, ch); err != nil {
			m.finishUpdateFailKeepPrev(op, ch, prevVer, prevRel, dist.Version, err.Error())
			return
		}
		op.State = OpDone
		_ = m.store.UpdateCorraldOp(context.Background(), op)
		return
	}

	// Execute zero-gap daemon.handoff on the live daemon.
	hctx, hcancel := context.WithTimeout(ctx, 15*time.Second)
	_, herr := client.Handoff(hctx, proto.HandoffParams{
		Path:    installedBin,
		SHA256:  bin.SHA256,
		Release: dist.Release,
	})
	hcancel()
	if herr != nil {
		// Handoff failed before READY: old daemon keeps serving seamlessly!
		_ = RecordFailedOnHost(ctx, sshClient, probe.FailedJSON, dist.Release, herr.Error())
		m.finishUpdateFailKeepPrev(op, ch, prevVer, prevRel, dist.Version, herr.Error())
		return
	}

	// Step: verify — reconnect to the new process and confirm its Release.
	setStep("verify")
	if err := m.reconnectSharedConn(ctx, sc, sshClient); err != nil {
		m.finishUpdateFailKeepPrev(op, ch, prevVer, prevRel, dist.Version, err.Error())
		return
	}

	sc.mu.Lock()
	sc.updatedAt = time.Now()
	sc.updateTargetVer = dist.Version
	sc.updateTargetRel = dist.Release
	sc.updateCrashCount = 0
	sc.prevVersion = prevVer
	sc.prevRelease = prevRel
	sc.prevRelTarget = prevRelTarget
	sc.scopeChange = nil
	newHello := sc.hello
	sc.mu.Unlock()

	ch.State = StateRunning
	if newHello != nil {
		ch.Version = newHello.Version
		ch.Release = newHello.Release
	} else {
		ch.Version = dist.Version
		ch.Release = dist.Release
	}
	ch.LastOKAt = time.Now().Unix()
	ch.LastError = ""
	ch.RestartCount = 0
	if files := ReadHostStateFiles(ctx, sshClient, probe, op.ID); files.Install != nil && len(files.Install.Files) > 0 {
		ch.Files = files.Install.Files
	}
	_ = m.store.UpsertCorraldHost(context.Background(), ch)

	op.State = OpDone
	op.ErrorCode = ""
	_ = m.store.UpdateCorraldOp(context.Background(), op)
}

func (m *Manager) finishUpdateFailKeepPrev(op *store.CorraldOp, ch *store.CorraldHost, prevVer string, prevRel int64, targetVer, detail string) {
	fail := &OpFailure{
		Code:            "update_failed",
		Detail:          detail,
		TargetVersion:   targetVer,
		FallbackVersion: prevVer,
	}
	ch.State = StateRunning
	if prevVer != "" {
		ch.Version = prevVer
		ch.Release = prevRel
	}
	ch.LastError = encodeLastError(fail)
	_ = m.store.UpsertCorraldHost(context.Background(), ch)

	op.State = OpFailed
	op.ErrorCode = "update_failed"
	op.Detail = encodeOpDetail(opDetailPayload{Message: detail})
	_ = m.store.UpdateCorraldOp(context.Background(), op)
}

// attachAndHello dials socketPath via direct-streamlocal, performs daemon.hello
// and daemon.resume, creates/updates the sharedConn, and starts health/disconnect
// monitoring.
func (m *Manager) attachAndHello(
	ctx context.Context,
	userID, hostID int64,
	corralUser, fp, sshUser string,
	sshClient *ssh.Client,
	probe *HostProbe,
	socketPath string,
	ch *store.CorraldHost,
) error {
	key := accountKey{Fingerprint: fp, Username: sshUser}

	var sc *sharedConn
	m.mu.Lock()
	sc = m.conns[key]
	if sc == nil {
		sc = &sharedConn{
			key:      key,
			hostRefs: make(map[int64]*hostRef),
			stopCh:   make(chan struct{}),
		}
		m.conns[key] = sc
	}
	m.hostConn[hostID] = key
	m.mu.Unlock()

	onEv := func(ev proto.Event) {
		m.handleDaemonEvent(sc, ev)
	}
	client, err := Dial(ctx, sshClient, socketPath, onEv)
	if err != nil {
		return err
	}

	hello, err := client.Hello(ctx, proto.HelloParams{
		Gateway:        m.gatewayID,
		GatewayVersion: m.gatewayVersion,
		ProtoMin:       m.protoMin,
		ProtoMax:       m.protoMax,
		Client: proto.ClientIdentity{
			CorralUser: corralUser,
			HostID:     strconv.FormatInt(hostID, 10),
		},
	})
	if err != nil {
		_ = client.Close()
		var rpcErr *proto.RPCError
		if errors.As(err, &rpcErr) && rpcErr.Code == proto.ErrProtoMismatch {
			ch.State = StateIncompatible
			ch.LastError = encodeLastError(&OpFailure{Code: proto.ErrProtoMismatch, Detail: rpcErr.Message})
			_ = m.store.UpsertCorraldHost(context.Background(), ch)
		}
		return err
	}
	if !proto.ProtocolInRange(hello.Protocol, m.protoMin, m.protoMax) {
		_ = client.Close()
		ch.State = StateIncompatible
		ch.Version = hello.Version
		ch.Release = hello.Release
		ch.LastError = encodeLastError(&OpFailure{Code: proto.ErrProtoMismatch, Detail: fmt.Sprintf("protocol %d outside [%d,%d]", hello.Protocol, m.protoMin, m.protoMax)})
		_ = m.store.UpsertCorraldHost(context.Background(), ch)
		return &proto.RPCError{Code: proto.ErrProtoMismatch, Message: "daemon protocol out of gateway range"}
	}

	// Subscribe to events via daemon.resume.
	sc.mu.Lock()
	prevEpoch, prevSeq := sc.epoch, sc.lastSeq
	sc.mu.Unlock()
	if prevEpoch != "" && prevEpoch == hello.Epoch {
		if _, rerr := client.Resume(ctx, prevEpoch, prevSeq); rerr != nil {
			var rpcErr *proto.RPCError
			if errors.As(rerr, &rpcErr) && rpcErr.Code == proto.ErrResyncRequired {
				if m.cfg.OnResync != nil {
					m.cfg.OnResync(fp, sshUser)
				}
				_, _ = client.Resume(ctx, hello.Epoch, hello.Seq)
			}
		}
	} else {
		_, _ = client.Resume(ctx, hello.Epoch, hello.Seq)
	}

	health, _ := client.Health(ctx)

	sc.mu.Lock()
	oldClient := sc.client
	sc.client = client
	sc.sshClient = sshClient
	sc.socketPath = socketPath
	if probe != nil {
		sc.machineID = probe.MachineID
		sc.libDir = probe.LibDir
		sc.stateDir = probe.StateDir
	}
	sc.hello = hello
	sc.health = health
	sc.epoch = hello.Epoch
	if hello.Seq > sc.lastSeq {
		sc.lastSeq = hello.Seq
	}
	sc.intentionalClose = false
	sc.hostRefs[hostID] = &hostRef{HostID: hostID, OwnerID: userID, CorralUser: corralUser}
	sc.mu.Unlock()

	if oldClient != nil && oldClient != client {
		_ = oldClient.Close()
	}

	ch.State = StateRunning
	ch.Version = hello.Version
	ch.Release = hello.Release
	ch.SocketPath = socketPath
	ch.LastOKAt = time.Now().Unix()
	ch.RestartCount = 0
	if ch.LastError != "" {
		if le := decodeLastError(ch.LastError); le == nil || le.Code != "update_failed" {
			ch.LastError = ""
		}
	}
	_ = m.store.UpsertCorraldHost(context.Background(), ch)

	// Start monitor loop for this client generation.
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		m.monitorConn(sc, client)
	}()

	return nil
}

func (m *Manager) handleDaemonEvent(sc *sharedConn, ev proto.Event) {
	sc.mu.Lock()
	if ev.Epoch != "" {
		sc.epoch = ev.Epoch
	}
	if ev.Seq > sc.lastSeq {
		sc.lastSeq = ev.Seq
	}
	sc.mu.Unlock()

	if m.cfg.OnEvent != nil {
		m.cfg.OnEvent(sc.key.Fingerprint, sc.key.Username, ev)
	}

	if ev.Event == proto.EventDaemonReconnect {
		// Hot-update handoff completed: reconnect immediately unless runUpdateOp is already doing it.
		sc.mu.Lock()
		already := sc.reconnecting || sc.updating
		if !already {
			sc.reconnecting = true
		}
		sc.mu.Unlock()
		if !already {
			m.wg.Add(1)
			go func() {
				defer m.wg.Done()
				defer func() {
					sc.mu.Lock()
					sc.reconnecting = false
					sc.mu.Unlock()
				}()
				rctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
				defer cancel()
				_ = m.reconnectSharedConn(rctx, sc, nil)
			}()
		}
	}
}

func (m *Manager) reconnectSharedConn(ctx context.Context, sc *sharedConn, overrideSSH *ssh.Client) error {
	sc.mu.Lock()
	sock := sc.socketPath
	oldClient := sc.client
	prevEpoch := sc.epoch
	prevSeq := sc.lastSeq
	sshClient := overrideSSH
	if sshClient == nil {
		sshClient = sc.sshClient
	}
	refs := make([]*hostRef, 0, len(sc.hostRefs))
	for _, r := range sc.hostRefs {
		cp := *r
		refs = append(refs, &cp)
	}
	sc.mu.Unlock()

	if sshClient == nil && len(refs) > 0 {
		c, rel, _, _, err := m.ssh.AcquireSSH(ctx, refs[0].OwnerID, refs[0].HostID)
		if err != nil {
			return err
		}
		defer rel()
		sshClient = c
	}
	if sshClient == nil {
		return ErrNotConnected
	}

	var newClient *Client
	var dialErr error
	for attempt := 0; attempt < 5; attempt++ {
		newClient, dialErr = Dial(ctx, sshClient, sock, func(ev proto.Event) {
			m.handleDaemonEvent(sc, ev)
		})
		if dialErr == nil {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	if dialErr != nil {
		return dialErr
	}

	var hello *proto.HelloResult
	for i, ref := range refs {
		h, err := newClient.Hello(ctx, proto.HelloParams{
			Gateway:        m.gatewayID,
			GatewayVersion: m.gatewayVersion,
			ProtoMin:       m.protoMin,
			ProtoMax:       m.protoMax,
			Client:         proto.ClientIdentity{CorralUser: ref.CorralUser, HostID: strconv.FormatInt(ref.HostID, 10)},
		})
		if err != nil {
			_ = newClient.Close()
			return err
		}
		if i == 0 {
			hello = h
		}
	}
	if hello == nil {
		h, err := newClient.Hello(ctx, proto.HelloParams{
			Gateway:        m.gatewayID,
			GatewayVersion: m.gatewayVersion,
			ProtoMin:       m.protoMin,
			ProtoMax:       m.protoMax,
		})
		if err != nil {
			_ = newClient.Close()
			return err
		}
		hello = h
	}

	// Replay missed events if epoch matches, otherwise signal resync.
	if prevEpoch != "" && prevEpoch == hello.Epoch {
		if _, err := newClient.Resume(ctx, prevEpoch, prevSeq); err != nil {
			var rpcErr *proto.RPCError
			if errors.As(err, &rpcErr) && rpcErr.Code == proto.ErrResyncRequired {
				if m.cfg.OnResync != nil {
					m.cfg.OnResync(sc.key.Fingerprint, sc.key.Username)
				}
				_, _ = newClient.Resume(ctx, hello.Epoch, hello.Seq)
			}
		}
	} else {
		if prevEpoch != "" && m.cfg.OnResync != nil {
			m.cfg.OnResync(sc.key.Fingerprint, sc.key.Username)
		}
		_, _ = newClient.Resume(ctx, hello.Epoch, hello.Seq)
	}

	health, _ := newClient.Health(ctx)

	sc.mu.Lock()
	sc.client = newClient
	sc.hello = hello
	sc.health = health
	sc.epoch = hello.Epoch
	if hello.Seq > sc.lastSeq {
		sc.lastSeq = hello.Seq
	}
	sc.mu.Unlock()

	if oldClient != nil && oldClient != newClient {
		_ = oldClient.Close()
	}

	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		m.monitorConn(sc, newClient)
	}()
	return nil
}

// monitorConn runs periodic daemon.health checks and handles unexpected disconnects.
func (m *Manager) monitorConn(sc *sharedConn, client *Client) {
	ticker := time.NewTicker(m.healthInterval)
	defer ticker.Stop()

	for {
		select {
		case <-sc.stopCh:
			return
		case <-client.Done():
			sc.mu.Lock()
			isCurrent := sc.client == client
			intentional := sc.intentionalClose || sc.updating || sc.reconnecting
			sc.mu.Unlock()
			if !isCurrent || intentional {
				return
			}
			m.handleUnexpectedDisconnect(sc)
			return
		case <-ticker.C:
			sc.mu.Lock()
			isCurrent := sc.client == client
			sc.mu.Unlock()
			if !isCurrent {
				return
			}
			hctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			health, err := client.Health(hctx)
			cancel()
			if err != nil {
				_ = client.Close()
				continue
			}
			now := time.Now().Unix()
			sc.mu.Lock()
			sc.health = health
			refs := make([]int64, 0, len(sc.hostRefs))
			for id := range sc.hostRefs {
				refs = append(refs, id)
			}
			sc.mu.Unlock()
			for _, hostID := range refs {
				if ch, err := m.store.CorraldHostByID(context.Background(), hostID); err == nil && ch.State == StateRunning {
					ch.LastOKAt = now
					_ = m.store.UpsertCorraldHost(context.Background(), ch)
				}
			}
		}
	}
}

func (m *Manager) handleUnexpectedDisconnect(sc *sharedConn) {
	sc.mu.Lock()
	if sc.recovering || sc.intentionalClose {
		sc.mu.Unlock()
		return
	}
	sc.recovering = true
	refs := make([]*hostRef, 0, len(sc.hostRefs))
	for _, r := range sc.hostRefs {
		cp := *r
		refs = append(refs, &cp)
	}
	updatedAt := sc.updatedAt
	updateTargetVer := sc.updateTargetVer
	updateTargetRel := sc.updateTargetRel
	prevVer := sc.prevVersion
	prevRel := sc.prevRelease
	prevRelTarget := sc.prevRelTarget
	libDir := sc.libDir
	stateDir := sc.stateDir
	sockPath := sc.socketPath
	machineID := sc.machineID

	// Check 30-minute post-update crash loop window (spec §6 item 7).
	triggerRollback := false
	if !updatedAt.IsZero() && updateTargetRel > 0 {
		sc.updateCrashCount++
		if ShouldRollbackOnCrash(sc.updateCrashCount, time.Since(updatedAt), m.crashWindow, m.crashThreshold) {
			triggerRollback = true
		}
	}
	sc.mu.Unlock()

	defer func() {
		sc.mu.Lock()
		sc.recovering = false
		sc.mu.Unlock()
	}()

	if len(refs) == 0 {
		return
	}

	// Transition all attached hosts to StateLost.
	for _, ref := range refs {
		if ch, err := m.store.CorraldHostByID(context.Background(), ref.HostID); err == nil {
			if tr := Transition(toSnapshot(ch), Event{Kind: EvDisconnect}); tr.Allowed {
				ch.State = tr.Next.State
				_ = m.store.UpsertCorraldHost(context.Background(), ch)
			}
		}
	}

	if triggerRollback {
		if m.attemptPostUpdateRollback(sc, refs, updateTargetVer, updateTargetRel, prevVer, prevRel, prevRelTarget, libDir, stateDir, sockPath, machineID) {
			return
		}
	}

	// Attempt automatic restart up to MaxAutoRestarts (3) with increasing backoff.
	for attempt := 0; attempt < MaxAutoRestarts; attempt++ {
		wait := m.restartBackoffs[min(attempt, len(m.restartBackoffs)-1)]
		select {
		case <-sc.stopCh:
			return
		case <-time.After(wait):
		}

		if m.tryRestartSharedConn(sc, refs) {
			for _, ref := range refs {
				if ch, err := m.store.CorraldHostByID(context.Background(), ref.HostID); err == nil {
					ch.State = StateRunning
					ch.RestartCount = 0
					ch.LastOKAt = time.Now().Unix()
					_ = m.store.UpsertCorraldHost(context.Background(), ch)
				}
			}
			return
		}
		for _, ref := range refs {
			if ch, err := m.store.CorraldHostByID(context.Background(), ref.HostID); err == nil {
				tr := Transition(toSnapshot(ch), Event{Kind: EvRestartAttempt, ErrorCode: "start_failed"})
				if tr.Allowed {
					ch.State = tr.Next.State
					ch.RestartCount = tr.Next.RestartCount
					if ch.State == StateError {
						ch.LastError = encodeLastError(&OpFailure{Code: "start_failed", MachineID: machineID})
					}
					_ = m.store.UpsertCorraldHost(context.Background(), ch)
				}
			}
		}
	}
}

func (m *Manager) attemptPostUpdateRollback(
	sc *sharedConn,
	refs []*hostRef,
	badVer string,
	badRel int64,
	prevVer string,
	prevRel int64,
	prevRelTarget, libDir, stateDir, sockPath, machineID string,
) bool {
	rctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	sshClient, relFn, err := m.acquireAnySSH(rctx, refs)
	if err != nil {
		return false
	}
	defer relFn()

	// Record the broken release in failed.json on the host.
	if stateDir != "" && badRel > 0 {
		_ = RecordFailedOnHost(rctx, sshClient, stateDir+"/failed.json", badRel, "crash_loop_after_update")
	}

	// Clear update window so we don't loop rollback.
	sc.mu.Lock()
	sc.updatedAt = time.Time{}
	sc.updateCrashCount = 0
	sc.mu.Unlock()

	if prevRelTarget == "" || libDir == "" {
		return false
	}

	// Stop bad version (best-effort), switch symlink back to previous version, start previous version.
	_, _ = runSSH(rctx, sshClient, StopCommand(libDir+"/corrald"), 4096, sshx.ExecOptions{})
	sres, serr := runSSH(rctx, sshClient, SwitchSymlinkCommand(libDir, prevRelTarget), 4096, sshx.ExecOptions{})
	if serr != nil || ParseKeyValues(sres.Out)["result"] != "ok" {
		return false
	}
	_, startedSock, sfail := RunServe(rctx, sshClient, libDir+"/corrald", m.gatewayID, machineID, "Linux", "")
	if sfail != nil {
		return false
	}
	if startedSock != "" {
		sc.mu.Lock()
		sc.socketPath = startedSock
		sc.mu.Unlock()
	}
	if err := m.reconnectSharedConn(rctx, sc, sshClient); err != nil {
		return false
	}

	failBanner := encodeLastError(&OpFailure{
		Code:            "update_failed",
		TargetVersion:   badVer,
		FallbackVersion: prevVer,
		MachineID:       machineID,
	})
	for _, ref := range refs {
		if ch, err := m.store.CorraldHostByID(context.Background(), ref.HostID); err == nil {
			ch.State = StateRunning
			ch.Version = prevVer
			ch.Release = prevRel
			ch.RestartCount = 0
			ch.LastError = failBanner
			ch.LastOKAt = time.Now().Unix()
			_ = m.store.UpsertCorraldHost(context.Background(), ch)
		}
	}
	return true
}

func (m *Manager) tryRestartSharedConn(sc *sharedConn, refs []*hostRef) bool {
	rctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	sshClient, relFn, err := m.acquireAnySSH(rctx, refs)
	if err != nil {
		return false
	}
	defer relFn()

	// Try direct reconnect first (e.g. transient SSH blip while daemon kept running).
	if err := m.reconnectSharedConn(rctx, sc, sshClient); err == nil {
		return true
	}

	sc.mu.Lock()
	libDir := sc.libDir
	mid := sc.machineID
	sc.mu.Unlock()
	bin := libDir + "/corrald"
	if libDir == "" {
		probe, perr := m.probeHost(rctx, sshClient)
		if perr != nil || probe.HasTombstone || !probe.HasSymlink {
			return false
		}
		bin = probe.CurrentBin
		mid = probe.MachineID
		sc.mu.Lock()
		sc.libDir = probe.LibDir
		sc.stateDir = probe.StateDir
		sc.machineID = probe.MachineID
		sc.mu.Unlock()
	}

	_, sock, fail := RunServe(rctx, sshClient, bin, m.gatewayID, mid, "Linux", "")
	if fail != nil {
		return false
	}
	if sock != "" {
		sc.mu.Lock()
		sc.socketPath = sock
		sc.mu.Unlock()
	}
	return m.reconnectSharedConn(rctx, sc, sshClient) == nil
}

func (m *Manager) evaluateUpdateAfterConnect(
	ctx context.Context,
	userID, hostID int64,
	corralUser, fp, sshUser string,
	probe *HostProbe,
	failed *HostFailedRecord,
) {
	dist := m.Dist()
	if dist == nil || m.adminDisabled {
		return
	}
	sc := m.sharedConnForHost(hostID, fp, sshUser)
	if sc == nil {
		return
	}
	sc.mu.Lock()
	hello := sc.hello
	alreadyUpdating := sc.updating
	sc.mu.Unlock()
	if hello == nil || alreadyUpdating {
		return
	}

	ch, err := m.store.CorraldHostByID(ctx, hostID)
	if err != nil {
		return
	}
	consented, ok := parseManifestJSON(ch.ManifestJSON)
	if !ok {
		consented = hello.Manifest
	}
	dec := DecideUpdate(hello.Release, hello.Protocol, consented, dist.Release, dist.Manifest, failed.ReleaseList(), m.protoMin, m.protoMax)
	switch dec.Action {
	case UpdateIncompatible:
		ch.State = StateIncompatible
		_ = m.store.UpsertCorraldHost(ctx, ch)
		m.closeSharedConn(sc, true)
	case UpdateNeedConsent:
		sc.mu.Lock()
		sc.scopeChange = &ScopeChangeInfo{
			TargetVersion: dist.Version,
			Added:         dec.Added,
		}
		sc.mu.Unlock()
	case UpdateAuto:
		sc.mu.Lock()
		sc.scopeChange = nil
		sc.mu.Unlock()
		ch.State = StateUpdating
		_ = m.store.UpsertCorraldHost(ctx, ch)
		_, _ = m.launchUpdateOp(ctx, userID, hostID, corralUser, ch, dist, false)
	}
}

func (m *Manager) reconcileHost(
	ctx context.Context,
	userID, hostID int64,
	corralUser, fp, sshUser string,
	sshClient *ssh.Client,
	ch *store.CorraldHost,
	activeOp *store.CorraldOp,
) {
	probe, err := m.probeHost(ctx, sshClient)
	if err != nil {
		return
	}
	opID := ""
	opKind := ""
	if activeOp != nil {
		opID = activeOp.ID
		opKind = activeOp.Kind
	}
	files := ReadHostStateFiles(ctx, sshClient, probe, opID)

	daemonRunning := false
	if probe.HasSymlink {
		if sres, serr := runSSH(ctx, sshClient, StatusCommand(probe.CurrentBin), 8192, sshx.ExecOptions{}); serr == nil || sres.Ended {
			sf := ParseKeyValues(sres.Out)
			daemonRunning = sf["state"] == proto.StateRunning || sf["state"] == proto.StateDraining
		}
	}

	obs := HostObservedState{
		HasTombstone:   probe.HasTombstone,
		HasInstall:     probe.HasInstall || files.Install != nil,
		DaemonRunning:  daemonRunning,
		LeftoverStages: probe.Stages,
	}
	if files.Install != nil {
		obs.InstalledVer = files.Install.Version
		obs.InstalledRel = files.Install.Release
	}
	if files.Op != nil {
		obs.OpRecordResult = files.Op.Result
	}

	dec := ReconcileAfterRestart(ch.State, opKind, obs)

	// Clean any leftover stage-* directories from interrupted uploads.
	for _, stage := range dec.CleanupStages {
		_, _ = runSSH(ctx, sshClient, RmStageCommand(stage), 4096, sshx.ExecOptions{})
	}

	switch dec.Action {
	case ReconcileFinishRemove:
		out := RunUninstall(ctx, sshClient, probe, opID, nil)
		switch out.Result {
		case "removed":
			_ = m.store.CorraldResetByBinding(ctx, fp, sshUser)
			ch.State = StateOff
			ch.Version = ""
			ch.Release = 0
			ch.Files = []string{}
			ch.LastError = ""
			_ = m.store.UpsertCorraldHost(ctx, ch)
			if activeOp != nil {
				activeOp.State = OpDone
				activeOp.Detail = encodeOpDetail(opDetailPayload{Result: "removed"})
				_ = m.store.UpdateCorraldOp(ctx, activeOp)
			}
		case "partial":
			ch.State = StateError
			ch.LastError = encodeLastError(&OpFailure{Code: "partial", Left: out.Left})
			_ = m.store.UpsertCorraldHost(ctx, ch)
			if activeOp != nil {
				activeOp.State = OpFailed
				activeOp.ErrorCode = "partial"
				activeOp.Detail = encodeOpDetail(opDetailPayload{Result: "partial", Left: out.Left})
				_ = m.store.UpdateCorraldOp(ctx, activeOp)
			}
		case "stop_failed":
			ch.State = StateError
			ch.LastError = encodeLastError(&OpFailure{Code: "stop_failed", PID: out.PID, Detail: out.Detail})
			_ = m.store.UpsertCorraldHost(ctx, ch)
			if activeOp != nil {
				activeOp.State = OpFailed
				activeOp.ErrorCode = "stop_failed"
				activeOp.Detail = encodeOpDetail(opDetailPayload{Result: "stop_failed", PID: out.PID, Message: out.Detail})
				_ = m.store.UpdateCorraldOp(ctx, activeOp)
			}
		default:
			ch.State = StateRemovePending
			_ = m.store.UpsertCorraldHost(ctx, ch)
		}

	case ReconcileMarkOff:
		ch.State = StateOff
		ch.Version = ""
		ch.Release = 0
		ch.Files = []string{}
		_ = m.store.UpsertCorraldHost(ctx, ch)
		if activeOp != nil && dec.OpFinalState != "" {
			activeOp.State = dec.OpFinalState
			activeOp.ErrorCode = dec.OpErrorCode
			_ = m.store.UpdateCorraldOp(ctx, activeOp)
		}

	case ReconcileMarkOpFailed:
		ch.State = dec.NextState
		if dec.OpErrorCode != "" {
			ch.LastError = encodeLastError(&OpFailure{Code: dec.OpErrorCode})
		}
		_ = m.store.UpsertCorraldHost(ctx, ch)
		if activeOp != nil {
			activeOp.State = dec.OpFinalState
			activeOp.ErrorCode = dec.OpErrorCode
			_ = m.store.UpdateCorraldOp(ctx, activeOp)
		}

	case ReconcileConnect, ReconcileStartDaemon:
		if files.Install != nil {
			ch.Version = files.Install.Version
			ch.Release = files.Install.Release
			ch.Files = files.Install.Files
		}
		ch.State = dec.NextState
		ch.MachineID = probe.MachineID
		_ = m.store.UpsertCorraldHost(ctx, ch)
		if activeOp != nil && dec.OpFinalState != "" {
			activeOp.State = dec.OpFinalState
			activeOp.ErrorCode = dec.OpErrorCode
			_ = m.store.UpdateCorraldOp(ctx, activeOp)
		}
	}
}

// --- Helpers ---

func (m *Manager) probeHost(ctx context.Context, c *ssh.Client) (*HostProbe, error) {
	if m.cfg.ProbeOverride != nil {
		return m.cfg.ProbeOverride(ctx, c)
	}
	return ProbeHost(ctx, c)
}

func (m *Manager) acquireAnySSH(ctx context.Context, refs []*hostRef) (*ssh.Client, func(), error) {
	var lastErr error
	for _, ref := range refs {
		c, rel, _, _, err := m.ssh.AcquireSSH(ctx, ref.OwnerID, ref.HostID)
		if err == nil && c != nil {
			return c, rel, nil
		}
		lastErr = err
	}
	if lastErr == nil {
		lastErr = ErrNotConnected
	}
	return nil, nil, lastErr
}

func (m *Manager) tryAdoptPeer(ctx context.Context, userID, hostID int64, fp, sshUser string) *store.CorraldHost {
	peers, err := m.store.CorraldPeersByBinding(ctx, fp, sshUser)
	if err != nil || len(peers) == 0 {
		return nil
	}
	for _, p := range peers {
		if p.HostID == hostID {
			continue
		}
		if p.State == StateRunning || p.State == StateStopped || p.State == StateLost || p.State == StateUpdating {
			src, err := m.store.CorraldHostByID(ctx, p.HostID)
			if err != nil || src.BoundGateway != m.gatewayID {
				continue
			}
			adopted := &store.CorraldHost{
				HostID:           hostID,
				State:            src.State,
				ConsentBy:        src.ConsentBy,
				ConsentAt:        src.ConsentAt,
				BoundFingerprint: src.BoundFingerprint,
				BoundUsername:    src.BoundUsername,
				BoundGateway:     src.BoundGateway,
				ManifestJSON:     src.ManifestJSON,
				Version:          src.Version,
				Release:          src.Release,
				MachineID:        src.MachineID,
				SocketPath:       src.SocketPath,
				Files:            slices.Clone(src.Files),
				PausedUsers:      []int64{},
				LastOKAt:         src.LastOKAt,
			}
			if err := m.store.UpsertCorraldHost(ctx, adopted); err == nil {
				return adopted
			}
		}
	}
	return nil
}

func (m *Manager) computeUsers(ctx context.Context, selfUserID int64, fp, sshUser string, remoteClients []proto.ClientInfo) *UsersInfo {
	others := make(map[string]struct{})
	selfLabel := m.ssh.UserLabel(ctx, selfUserID)

	if peers, err := m.store.CorraldPeersByBinding(ctx, fp, sshUser); err == nil {
		for _, p := range peers {
			if p.OwnerID == selfUserID {
				continue
			}
			if slices.Contains(p.PausedUsers, p.OwnerID) {
				continue
			}
			label := p.DisplayName
			if label == "" {
				label = p.Username
			}
			if label != "" {
				others[label] = struct{}{}
			}
		}
	}

	for _, rc := range remoteClients {
		if rc.Gateway == m.gatewayID && (rc.CorralUser == selfLabel || rc.CorralUser == "") {
			continue
		}
		label := rc.CorralUser
		if rc.Gateway != "" && rc.Gateway != m.gatewayID {
			label = rc.CorralUser + "@" + rc.Gateway
		}
		if label != "" && label != selfLabel {
			others[label] = struct{}{}
		}
	}

	list := make([]string, 0, len(others))
	for name := range others {
		list = append(list, name)
	}
	sort.Strings(list)
	return &UsersInfo{
		Count:  1 + len(list),
		Others: list,
	}
}

func (m *Manager) hasOtherActivePeers(ctx context.Context, selfUserID int64, fp, sshUser string) bool {
	peers, err := m.store.CorraldPeersByBinding(ctx, fp, sshUser)
	if err != nil {
		return false
	}
	for _, p := range peers {
		if p.OwnerID == selfUserID {
			continue
		}
		if slices.Contains(p.PausedUsers, p.OwnerID) {
			continue
		}
		if p.State == StateRunning || p.State == StateStopped || p.State == StateLost || p.State == StateUpdating {
			return true
		}
	}
	return false
}

func (m *Manager) getSharedConn(key accountKey) *sharedConn {
	if key.Fingerprint == "" || key.Username == "" {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.conns[key]
}

func (m *Manager) sharedConnForHost(hostID int64, fp, sshUser string) *sharedConn {
	m.mu.Lock()
	defer m.mu.Unlock()
	if key, ok := m.hostConn[hostID]; ok {
		if sc, ok := m.conns[key]; ok {
			return sc
		}
	}
	if fp != "" && sshUser != "" {
		return m.conns[accountKey{Fingerprint: fp, Username: sshUser}]
	}
	return nil
}

// DetachHost removes hostID from its active shared connection (e.g. on rebind or host delete).
func (m *Manager) DetachHost(hostID int64) {
	m.detachHost(hostID)
}

func (m *Manager) detachHost(hostID int64) {
	m.mu.Lock()
	key, ok := m.hostConn[hostID]
	delete(m.hostConn, hostID)
	var sc *sharedConn
	if ok {
		sc = m.conns[key]
	}
	m.mu.Unlock()
	if sc != nil {
		sc.mu.Lock()
		delete(sc.hostRefs, hostID)
		empty := len(sc.hostRefs) == 0
		sc.mu.Unlock()
		if empty {
			m.closeSharedConn(sc, true)
		}
	}
}

func (m *Manager) closeSharedConn(sc *sharedConn, removeFromMap bool) {
	sc.mu.Lock()
	sc.intentionalClose = true
	client := sc.client
	sc.client = nil
	rel := sc.sshRelease
	sc.sshRelease = nil
	sc.mu.Unlock()

	sc.stop()
	if client != nil {
		_ = client.Close()
	}
	if rel != nil {
		rel()
	}
	if removeFromMap {
		m.mu.Lock()
		if m.conns[sc.key] == sc {
			delete(m.conns, sc.key)
		}
		m.mu.Unlock()
	}
}

func (m *Manager) clearRunningOp(hostID int64, rop *runningOp) {
	m.mu.Lock()
	if m.ops[hostID] == rop {
		delete(m.ops, hostID)
	}
	m.mu.Unlock()
}

func (m *Manager) isOpCancelled(rop *runningOp) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return rop.cancelled
}

func (m *Manager) finishOpCancelled(op *store.CorraldOp, ch *store.CorraldHost) {
	op.State = OpCancelled
	op.ErrorCode = ""
	_ = m.store.UpdateCorraldOp(context.Background(), op)
	ch.State = StateOff
	ch.LastError = ""
	_ = m.store.UpsertCorraldHost(context.Background(), ch)
}

func (m *Manager) finishOpFail(op *store.CorraldOp, ch *store.CorraldHost, fail *OpFailure) {
	tr := Transition(toSnapshot(ch), Event{
		Kind:          EvInstallFail,
		ErrorCode:     fail.Code,
		KeepInstalled: fail.KeepInstalled,
	})
	if tr.Allowed {
		ch.State = tr.Next.State
	} else if fail.KeepInstalled {
		ch.State = StateError
	} else {
		ch.State = StateOff
	}
	ch.LastError = encodeLastError(fail)
	_ = m.store.UpsertCorraldHost(context.Background(), ch)

	op.State = OpFailed
	op.ErrorCode = fail.Code
	hostInfo := ""
	if fail.OS != "" || fail.Arch != "" {
		hostInfo = strings.Trim(fail.OS+"/"+fail.Arch, "/")
	}
	op.Detail = encodeOpDetail(opDetailPayload{
		Message:  fail.Detail,
		Left:     fail.Left,
		PID:      fail.PID,
		HostInfo: hostInfo,
	})
	_ = m.store.UpdateCorraldOp(context.Background(), op)
}

func (m *Manager) toOpView(op *store.CorraldOp) *OpView {
	if op == nil {
		return nil
	}
	v := &OpView{
		ID:          op.ID,
		Kind:        op.Kind,
		State:       op.State,
		Step:        op.Step,
		Steps:       StepsForKind(op.Kind),
		Cancellable: (op.State == OpPending || op.State == OpRunning) && StepCancellable(op.Kind, op.Step),
		ErrorCode:   op.ErrorCode,
	}
	if op.Detail != "" {
		var p opDetailPayload
		if json.Unmarshal([]byte(op.Detail), &p) == nil && (p.Message != "" || p.Result != "" || len(p.Left) > 0 || p.PID > 0 || p.HostInfo != "") {
			v.Detail = p.Message
			v.Result = p.Result
			v.Left = p.Left
			v.PID = p.PID
			v.HostInfo = p.HostInfo
		} else {
			v.Detail = op.Detail
		}
	}
	return v
}

func toSnapshot(ch *store.CorraldHost) StateSnapshot {
	if ch == nil {
		return StateSnapshot{State: StateOff}
	}
	return StateSnapshot{
		State:            ch.State,
		Version:          ch.Version,
		Release:          ch.Release,
		RestartCount:     ch.RestartCount,
		BoundFingerprint: ch.BoundFingerprint,
		BoundUsername:    ch.BoundUsername,
		BoundGateway:     ch.BoundGateway,
		PausedUsers:      ch.PausedUsers,
	}
}

func parseManifestJSON(raw string) (proto.Manifest, bool) {
	if strings.TrimSpace(raw) == "" {
		return proto.Manifest{}, false
	}
	var m proto.Manifest
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return proto.Manifest{}, false
	}
	return m, true
}

func encodeLastError(f *OpFailure) string {
	if f == nil {
		return ""
	}
	b, _ := json.Marshal(f)
	return string(b)
}

func decodeLastError(raw string) *OpFailure {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	var f OpFailure
	if err := json.Unmarshal([]byte(raw), &f); err == nil && f.Code != "" {
		return &f
	}
	return &OpFailure{Code: "other", Detail: raw}
}

func encodeOpDetail(p opDetailPayload) string {
	b, _ := json.Marshal(p)
	return string(b)
}

func extractDetailMessage(raw string) string {
	var p opDetailPayload
	if json.Unmarshal([]byte(raw), &p) == nil && p.Message != "" {
		return p.Message
	}
	return raw
}

func newHexID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// WaitOpsIdle waits until all active in-memory ops finish (helper for tests).
func (m *Manager) WaitOpsIdle(ctx context.Context) error {
	for {
		m.mu.Lock()
		var ch chan struct{}
		for _, op := range m.ops {
			ch = op.done
			break
		}
		m.mu.Unlock()
		if ch == nil {
			return nil
		}
		select {
		case <-ch:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// SharedConnCount reports the number of active shared corrald connections (for tests).
func (m *Manager) SharedConnCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, sc := range m.conns {
		sc.mu.Lock()
		if sc.client != nil && !sc.client.Closed() {
			n++
		}
		sc.mu.Unlock()
	}
	return n
}

// ForceDropConn closes the underlying corrald Unix socket connection for
// (fingerprint, sshUser) without setting intentionalClose, simulating a daemon
// crash or disconnect in tests.
func (m *Manager) ForceDropConn(fp, sshUser string) bool {
	sc := m.getSharedConn(accountKey{Fingerprint: fp, Username: sshUser})
	if sc == nil {
		return false
	}
	sc.mu.Lock()
	c := sc.client
	sc.mu.Unlock()
	if c == nil {
		return false
	}
	_ = c.Close()
	return true
}

// SetHealthCapsForTest overrides cached health caps for a host (used in tests
// to verify single-capability fallback via ClientForCap).
func (m *Manager) SetHealthCapsForTest(hostID int64, caps map[string]string) {
	sc := m.sharedConnForHost(hostID, "", "")
	if sc == nil {
		return
	}
	sc.mu.Lock()
	if sc.health == nil {
		sc.health = &proto.HealthResult{}
	}
	sc.health.Caps = caps
	sc.mu.Unlock()
}

// Keep unused path import clean if needed.
var _ = path.Join
