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
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
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
	// ErrManifestRequired is returned when manifest_hash is missing on install/update.
	ErrManifestRequired = errors.New("manifest_hash is required")
	// ErrManifestChanged is returned when the manifest_hash submitted by the user
	// does not match the current corrald manifest.
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
	// along with a release callback, the host key fingerprint, and SSH username.
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
	HealthInterval        time.Duration
	RestartBackoffs       []time.Duration
	CrashWindow           time.Duration
	CrashThreshold        int
	ProtoMin              int
	ProtoMax              int
	BusyRetryInterval     time.Duration
	BusyMaxRetries        int
	CapFailureThreshold   int
	HandoffTimeout        time.Duration
	WatchUnsubscribeDelay time.Duration
	WatchRPCTimeout       time.Duration

	// Callbacks (optional; can also be set via SetWatchCallbacks).
	OnAgentChanged    func(hostIDs []int64, ev proto.AgentChangedEventData)
	OnCoverageChanged func(hostIDs []int64)
	PerfCompare       func(ctx context.Context, hostID, consentAt int64) []PerfCompareItem

	// Test hooks (nil in production).
	ProbeOverride            func(ctx context.Context, c *ssh.Client) (*HostProbe, error)
	VerifySHA256Hook         func(actual string) string
	OnEvent                  func(fingerprint, sshUser string, ev proto.Event)
	OnResync                 func(fingerprint, sshUser string)
	BeforeInstallStart       func(hostID int64, opID string)
	BeforeUpdateStage        func(hostID int64, opID string)
	BeforeRebindMark         func(hostID int64, source string)
	AfterScopeClaim          func(hostID int64, opID string)
	AfterAttachHello         func(hostID int64)
	AfterAttachConnsInsert   func(hostID int64)
	BeforeAdminDisableCheck  func(hostID int64)
	BeforePendingCleanupSSH  func(hostID int64)
	BeforeCleanupUserDBReset func(hostID int64)
	AfterInstallServe        func(hostID int64, opID string)
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
	PerfComparison        []PerfCompareItem `json:"perf_comparison,omitempty"`
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

func (k accountKey) valid() bool {
	return k.Fingerprint != "" && k.Username != ""
}

type hostRef struct {
	HostID     int64
	OwnerID    int64
	CorralUser string
}

type runningOp struct {
	opID      string
	hostID    int64
	acct      accountKey
	kind      string
	step      string
	cancel    context.CancelFunc
	cancelled bool
	done      chan struct{}
}

// sshOwnership transfers ownership of an AcquireSSH release callback so an
// operation's deferred cleanup will not close a freshly dialed SSH connection
// that was adopted by a live sharedConn.
type sshOwnership struct {
	mu      sync.Mutex
	release func()
}

func newSSHOwnership(rel func()) *sshOwnership {
	if rel == nil {
		return nil
	}
	return &sshOwnership{release: rel}
}

func (o *sshOwnership) take() func() {
	if o == nil {
		return nil
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	r := o.release
	o.release = nil
	return r
}

func (o *sshOwnership) releaseIfOwned() {
	if r := o.take(); r != nil {
		r()
	}
}

type sharedConn struct {
	key        accountKey
	mu         sync.Mutex
	client     *Client
	sshClient  *ssh.Client
	sshRelease func()
	socketPath string
	machineID  string
	homeDir    string
	uid        int64
	libDir     string
	stateDir   string
	hello      *proto.HelloResult
	health     *proto.HealthResult
	epoch      string
	lastSeq    uint64
	hostRefs   map[int64]*hostRef

	// Cached failed releases observed from host failed.json or local failures.
	failedReleases []int64

	// Unacknowledged mutating RPC op_id -> epoch sent in (T1 epoch-scoped dedup).
	unackedWrites map[string]string

	// Hot-update tracking & 30-min crash-loop rollback state (also persisted in DB).
	updatedAt        time.Time
	updateTargetVer  string
	updateTargetRel  int64
	updateCrashCount int
	prevVersion      string
	prevRelease      int64
	prevRelTarget    string

	// Pending scope change detected against current Dist.
	scopeChange *ScopeChangeInfo

	// Per-capability consecutive failure tracking & local degradation (spec §9).
	capFailures map[string]int
	capDegraded map[string]bool

	// agent.watch subscription state for this shared connection.
	// Mutated ONLY through Manager.mutateWatchState.
	watchRemoteActive   bool
	watchHealthy        bool
	watchClient         *Client
	watchLogsSig        string
	watchCoveredHosts   map[int64]struct{}
	watchUnsubCancel    chan struct{}
	watchUnsubDue       bool
	watchReconciling    bool
	watchReconcileAgain bool

	// Lifecycle coordination.
	intentionalClose bool
	reconnecting     bool
	updating         bool
	recovering       bool
	stopCh           chan struct{}
	stopOnce         sync.Once
}

func newSharedConn(key accountKey) *sharedConn {
	return &sharedConn{
		key:               key,
		uid:               -1,
		hostRefs:          make(map[int64]*hostRef),
		unackedWrites:     make(map[string]string),
		capFailures:       make(map[string]int),
		capDegraded:       make(map[string]bool),
		watchCoveredHosts: make(map[int64]struct{}),
		stopCh:            make(chan struct{}),
	}
}

func (sc *sharedConn) stop() {
	sc.stopOnce.Do(func() { close(sc.stopCh) })
}

func (sc *sharedConn) addFailedReleasesLocked(rels []int64) {
	for _, r := range rels {
		if r > 0 && !slices.Contains(sc.failedReleases, r) {
			sc.failedReleases = append(sc.failedReleases, r)
		}
	}
}

// Manager coordinates all corrald connections, operations, auto-updates, health
// monitoring, and fallbacks on the gateway.
type Manager struct {
	cfg                    ManagerConfig
	store                  *store.Store
	log                    *slog.Logger
	ssh                    SSHProvider
	gatewayID              string
	gatewayVersion         string
	adminDisabled          atomic.Bool
	policyMu               sync.RWMutex
	policyGen              atomic.Uint64
	pendingFlagMu          sync.Mutex
	pendingFlagGen         uint64
	hasPendingCleanup      atomic.Bool
	pendingRetryMu         sync.Mutex
	pendingRetryAt         map[int64]time.Time
	pendingRetryActive     map[int64]bool
	pendingRetryAcctAt     map[accountKey]time.Time
	pendingRetryAcctActive map[accountKey]bool
	bgCtx                  context.Context
	bgCancel               context.CancelFunc
	healthInterval         time.Duration
	restartBackoffs        []time.Duration
	crashWindow            time.Duration
	crashThreshold         int
	protoMin               int
	protoMax               int
	busyRetry              BusyRetryPolicy
	capFailureThreshold    int
	handoffTimeout         time.Duration
	watchUnsubDelay        time.Duration
	watchRPCTimeout        time.Duration

	distMu sync.RWMutex
	dist   *Dist

	watchCbMu         sync.RWMutex
	onAgentChanged    func(hostIDs []int64, ev proto.AgentChangedEventData)
	onCoverageChanged func(hostIDs []int64)

	mu            sync.Mutex
	conns         map[accountKey]*sharedConn
	hostConn      map[int64]accountKey
	watchers      map[any]*watchRegistration
	hostExtraLogs map[int64]map[string]struct{}
	opsByHost     map[int64]*runningOp
	opsByAcct     map[accountKey]*runningOp
	closed        bool
	wg            sync.WaitGroup
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
	busyPolicy := DefaultBusyRetryPolicy
	if cfg.BusyRetryInterval > 0 {
		busyPolicy.Interval = cfg.BusyRetryInterval
	}
	if cfg.BusyMaxRetries > 0 {
		busyPolicy.MaxRetries = cfg.BusyMaxRetries
	}
	capThreshold := cfg.CapFailureThreshold
	if capThreshold <= 0 {
		capThreshold = 3
	}
	handoffTO := cfg.HandoffTimeout
	if handoffTO <= 0 {
		handoffTO = DefaultHandoffTimeout
	}
	watchUnsubDelay := cfg.WatchUnsubscribeDelay
	if watchUnsubDelay <= 0 {
		watchUnsubDelay = DefaultWatchUnsubscribeDelay
	}
	watchRPCTO := cfg.WatchRPCTimeout
	if watchRPCTO <= 0 {
		watchRPCTO = defaultWatchRPCTimeout
	}
	bgCtx, bgCancel := context.WithCancel(context.Background())
	mgr := &Manager{
		cfg:                    cfg,
		store:                  cfg.Store,
		dist:                   cfg.Dist,
		log:                    cfg.Log,
		ssh:                    cfg.SSH,
		gatewayID:              cfg.GatewayID,
		gatewayVersion:         cfg.GatewayVersion,
		bgCtx:                  bgCtx,
		bgCancel:               bgCancel,
		pendingRetryAt:         make(map[int64]time.Time),
		pendingRetryActive:     make(map[int64]bool),
		pendingRetryAcctAt:     make(map[accountKey]time.Time),
		pendingRetryAcctActive: make(map[accountKey]bool),
		healthInterval:         cfg.HealthInterval,
		restartBackoffs:        cfg.RestartBackoffs,
		crashWindow:            cfg.CrashWindow,
		crashThreshold:         cfg.CrashThreshold,
		protoMin:               cfg.ProtoMin,
		protoMax:               cfg.ProtoMax,
		busyRetry:              busyPolicy,
		capFailureThreshold:    capThreshold,
		handoffTimeout:         handoffTO,
		watchUnsubDelay:        watchUnsubDelay,
		watchRPCTimeout:        watchRPCTO,
		onAgentChanged:         cfg.OnAgentChanged,
		onCoverageChanged:      cfg.OnCoverageChanged,
		conns:                  make(map[accountKey]*sharedConn),
		hostConn:               make(map[int64]accountKey),
		watchers:               make(map[any]*watchRegistration),
		hostExtraLogs:          make(map[int64]map[string]struct{}),
		opsByHost:              make(map[int64]*runningOp),
		opsByAcct:              make(map[accountKey]*runningOp),
	}
	mgr.adminDisabled.Store(cfg.AdminDisabled)
	// Startup recovery: clear any orphaned active_op_id left behind if a
	// previous gateway process crashed after finalizing corrald_ops.
	if cfg.Store != nil {
		_, _ = cfg.Store.ClearStaleCorraldHostOps(context.Background())
		if has, err := cfg.Store.HasCorraldHostsInStates(context.Background(), StateRemovePending, StateRemoving); err == nil && has {
			mgr.hasPendingCleanup.Store(true)
		}
	}
	return mgr
}

// DefaultHandoffTimeout bounds daemon.handoff RPCs above the daemon's 60s
// lifecycle lock wait so real lock contention returns `busy` cleanly (C-7).
var DefaultHandoffTimeout = 75 * time.Second

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
	m.policyMu.Lock()
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		m.policyMu.Unlock()
		return
	}
	m.closed = true
	if m.bgCancel != nil {
		m.bgCancel()
	}
	for _, op := range m.opsByHost {
		if op.cancel != nil {
			op.cancel()
		}
	}
	for _, op := range m.opsByAcct {
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
	m.policyMu.Unlock()

	for _, sc := range conns {
		m.closeSharedConn(sc, false)
	}
	m.wg.Wait()
}

// ClientForCap is the unified fallback entrypoint (spec §9) used by stage 1
// features (T3) to decide whether a host capability should use corrald or fall
// back to the existing SSH path. Enforces both per-capability health degradation
// and consented manifest scope boundaries.
func (m *Manager) ClientForCap(userID, hostID int64, capName string) (*Client, bool) {
	client, _, ok := m.clientAndConnForCap(userID, hostID, capName)
	return client, ok
}

func (m *Manager) clientAndConnForCap(userID, hostID int64, capName string) (*Client, *sharedConn, bool) {
	if m == nil || m.AdminDisabled() || !m.Available() {
		return nil, nil, false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	ch, err := m.store.CorraldHostByID(ctx, hostID)
	if err != nil {
		return nil, nil, false
	}
	if EffectiveUserState(toSnapshot(ch), userID) != StateRunning {
		return nil, nil, false
	}
	m.mu.Lock()
	key, ok := m.hostConn[hostID]
	var sc *sharedConn
	if ok {
		sc = m.conns[key]
	}
	m.mu.Unlock()
	if sc == nil {
		return nil, nil, false
	}
	sc.mu.Lock()
	defer sc.mu.Unlock()
	if sc.client == nil || sc.client.Closed() {
		return nil, nil, false
	}
	// Enforce consented manifest scope against actual running daemon manifest (Item 6).
	if sc.hello != nil {
		if consented, hasConsented := parseManifestJSON(ch.ManifestJSON); hasConsented {
			if !CapAllowedByScope(consented, sc.hello.Manifest, capName) {
				return nil, nil, false
			}
		}
	}
	if capName != "" {
		if sc.capDegraded[capName] {
			return nil, nil, false
		}
		if sc.health != nil && sc.health.Caps != nil {
			if st, exists := sc.health.Caps[capName]; exists && st != "ok" {
				return nil, nil, false
			}
		}
	}
	return sc.client, sc, true
}

// ReportCapSuccess resets the consecutive failure counter for capName on hostID
// ONLY if client still matches the current live connection generation (C-8).
func (m *Manager) ReportCapSuccess(hostID int64, client *Client, capName string) {
	if m == nil || capName == "" {
		return
	}
	sc := m.sharedConnForHost(hostID, "", "")
	if sc == nil {
		return
	}
	sc.mu.Lock()
	if client != nil && sc.client != client {
		sc.mu.Unlock()
		return
	}
	delete(sc.capFailures, capName)
	sc.mu.Unlock()
}

// ReportCapFailure records a runtime failure of capName on hostID ONLY if
// client still matches the current live connection generation (C-8).
// Consecutive failures reaching CapFailureThreshold (default 3), or an explicit
// method_not_found/not_supported response, mark only capName as degraded so
// subsequent calls fall back to the SSH path while other capabilities remain
// active. Caller-initiated context cancellations are ignored.
func (m *Manager) ReportCapFailure(hostID int64, client *Client, capName string, err error) {
	if m == nil || capName == "" || err == nil {
		return
	}
	if errors.Is(err, context.Canceled) {
		return
	}
	sc := m.sharedConnForHost(hostID, "", "")
	if sc == nil {
		return
	}
	immediate := false
	var rpcErr *proto.RPCError
	if errors.As(err, &rpcErr) && (rpcErr.Code == proto.ErrMethodNotFound || rpcErr.Code == proto.ErrNotSupported) {
		immediate = true
	}
	m.mutateWatchState(sc, nil, func() {
		if client != nil && sc.client != client {
			return
		}
		if immediate {
			sc.capDegraded[capName] = true
		} else {
			sc.capFailures[capName]++
			if sc.capFailures[capName] >= m.capFailureThreshold {
				sc.capDegraded[capName] = true
			}
		}
	})
}

// CallMutating executes a mutating RPC (e.g. pane.send / pane.answer) with T1's
// protocol conventions:
//   - Generates op_id via proto.NewOpID if opID is empty.
//   - Refuses to resend an unacknowledged op_id across a changed daemon epoch
//     (returning ErrUnknownResult -> HTTP 409 corrald_unknown_result).
//   - Treats op_expired, op_unknown, and mid-flight disconnects as ErrUnknownResult.
func (m *Manager) CallMutating(
	ctx context.Context,
	userID, hostID int64,
	capName, method, opID string,
	buildParams func(opID string) any,
	dst any,
) (string, error) {
	if opID == "" {
		opID = NewWriteOpID()
	}
	client, sc, ok := m.clientAndConnForCap(userID, hostID, capName)
	if !ok {
		return opID, ErrNotConnected
	}

	sc.mu.Lock()
	curEpoch := sc.epoch
	if curEpoch == "" && sc.hello != nil {
		curEpoch = sc.hello.Epoch
	}
	if prevEpoch, seen := sc.unackedWrites[opID]; seen && prevEpoch != "" && prevEpoch != curEpoch {
		sc.mu.Unlock()
		return opID, fmt.Errorf("%w: daemon epoch changed (%s -> %s)", ErrUnknownResult, prevEpoch, curEpoch)
	}
	if len(sc.unackedWrites) >= 512 {
		// Evict entries when table grows large to bound memory.
		for k := range sc.unackedWrites {
			delete(sc.unackedWrites, k)
			if len(sc.unackedWrites) < 256 {
				break
			}
		}
	}
	sc.unackedWrites[opID] = curEpoch
	sc.mu.Unlock()

	var params any
	if buildParams != nil {
		params = buildParams(opID)
	}
	err := client.CallMutating(ctx, method, params, dst)
	if err == nil || !errors.Is(err, ErrUnknownResult) {
		sc.mu.Lock()
		delete(sc.unackedWrites, opID)
		sc.mu.Unlock()
	}
	return opID, err
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
		AdminDisabled:   m.AdminDisabled(),
		Files:           []string{},
	}

	sshUser, fp, _ := m.ssh.HostBinding(ctx, userID, hostID)
	sshVerified := m.store.IsSSHVerified(ctx, hostID, fp, sshUser)
	ch, err := m.store.CorraldHost(ctx, userID, hostID)
	if errors.Is(err, store.ErrNotFound) {
		// Only inherit/expose shared helper status if this host record has
		// actually completed at least one SSH authentication to (fp, sshUser) (Item 8).
		if sshVerified && fp != "" && sshUser != "" {
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

	// Enforce authorization binding (fingerprint, username, gateway) via atomic
	// CAS SQL comparing the row's current binding against the read snapshot so
	// neither a concurrent reinstall nor a stale observed identity can clobber
	// a newer binding.
	if ch.State != StateOff && ch.State != StateRemoving && ch.State != StateRemovePending && ch.State != StateInstalling && ch.State != StateRebind {
		if !CheckBinding(toSnapshot(ch), fp, sshUser, m.gatewayID) {
			if m.markRebindIfMismatch(ctx, hostID, ch.BoundFingerprint, ch.BoundUsername, ch.BoundGateway, fp, sshUser, false, "status") {
				ch.State = StateRebind
			} else if fresh, ferr := m.store.CorraldHost(ctx, userID, hostID); ferr == nil {
				ch = fresh
			}
		}
	}

	res.State = EffectiveUserState(toSnapshot(ch), userID)
	if ch.State == StateRemovePending || ch.State == StateRemoving {
		m.markPendingCleanup()
		m.MaybeRetryPendingCleanup(userID, hostID, "", nil)
	}
	res.Version = ch.Version
	res.Release = ch.Release
	res.LastOKAt = ch.LastOKAt
	res.MachineID = ch.MachineID

	// Latest operation.
	if op, oerr := m.store.LatestCorraldOp(ctx, hostID); oerr == nil {
		res.Op = m.toOpView(op)
	}
	if ch.LastError != "" {
		res.Error = decodeLastError(ch.LastError)
	}
	if res.State == StateIncompatible {
		res.CanReplaceWithGateway = true
	}

	// Shared metadata (files, consent_by, users, live running metrics) is ONLY
	// shown once this host record has completed a real SSH authentication for
	// its current (fingerprint, username) pair (C-3: consent_by never bypasses).
	canSeeShared := sshVerified
	if !canSeeShared && res.State != StateOff && res.State != StateInstalling && res.State != StateRebind && res.State != StateRemoving && res.State != StateRemovePending {
		res.State = StateOff
		res.Version = ""
		res.Release = 0
		res.LastOKAt = 0
		res.MachineID = ""
		return res, nil
	}

	if canSeeShared && res.State != StateOff {
		if len(ch.Files) > 0 {
			res.Files = append([]string(nil), ch.Files...)
		}
		if ch.ConsentBy != nil && *ch.ConsentBy > 0 {
			res.Consent = &ConsentInfo{
				By: m.ssh.UserLabel(ctx, *ch.ConsentBy),
				At: ch.ConsentAt,
			}
		}
	}

	// Live connection details (running metrics, caps, scope_change, users).
	var liveHello *proto.HelloResult
	if canSeeShared && res.State != StateOff && res.State != StateRebind {
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
					if len(sc.health.Caps) > 0 || len(sc.capDegraded) > 0 {
						res.Caps = make(map[string]string, len(sc.health.Caps)+len(sc.capDegraded))
						for k, v := range sc.health.Caps {
							res.Caps[k] = v
						}
						for k, deg := range sc.capDegraded {
							if deg {
								res.Caps[k] = "degraded"
							}
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
				cp := *sc.hello
				liveHello = &cp
				remoteClients = slices.Clone(sc.hello.Clients)
			}
			sc.mu.Unlock()
			res.Users = m.computeUsers(ctx, userID, fp, sshUser, remoteClients)
		} else if fp != "" && sshUser != "" {
			res.Users = m.computeUsers(ctx, userID, fp, sshUser, nil)
		}
	}

	// Compare running daemon's actual manifest against this user's consented manifest (Item 6).
	if res.State != StateOff && res.State != StateIncompatible && res.State != StateRemoving && res.State != StateRemovePending {
		consented, hasConsented := parseManifestJSON(ch.ManifestJSON)
		if hasConsented && liveHello != nil {
			if added := proto.ScopeGrew(consented, liveHello.Manifest); len(added) > 0 {
				res.ScopeChange = &ScopeChangeInfo{
					TargetVersion: liveHello.Version,
					Added:         added,
				}
				if liveHello.Release >= dist.Release {
					if mh, herr := proto.ManifestSHA256(liveHello.Manifest); herr == nil && mh != "" {
						res.ManifestHash = mh
					}
				}
			}
		}
		if res.ScopeChange == nil && ch.Release > 0 && hasConsented {
			dec := DecideUpdate(ch.Release, proto.Protocol, consented, dist.Release, dist.Manifest, ch.FailedReleases, m.protoMin, m.protoMax)
			if dec.Action == UpdateNeedConsent {
				res.ScopeChange = &ScopeChangeInfo{
					TargetVersion: dist.Version,
					Added:         dec.Added,
				}
			}
		}
	}

	if canSeeShared && !m.AdminDisabled() && (res.State == StateRunning || res.State == StateStopped) && ch.ConsentAt > 0 {
		if cmp := m.computeStatusPerfComparison(ctx, hostID, ch.ConsentAt); len(cmp) > 0 {
			res.PerfComparison = cmp
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
	if m.AdminDisabled() {
		return nil, ErrAdminDisabled
	}
	manifestHash = strings.TrimSpace(manifestHash)
	if manifestHash == "" {
		return nil, ErrManifestRequired
	}
	if manifestHash != dist.ManifestHash {
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

	sshUser, fp, _ := m.ssh.HostBinding(ctx, userID, hostID)
	acct := accountKey{Fingerprint: fp, Username: sshUser}

	opID := newHexID()
	op := &store.CorraldOp{
		ID:         opID,
		HostID:     hostID,
		AccountKey: store.CorraldAccountKey(fp, sshUser),
		Kind:       OpInstall,
		State:      OpRunning,
		Step:       "check",
		CreatedBy:  userID,
	}

	opCtx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	rop := &runningOp{opID: opID, hostID: hostID, acct: acct, kind: OpInstall, step: "check", cancel: cancel, done: make(chan struct{})}
	if err := m.registerRunningOp(rop); err != nil {
		cancel()
		return nil, err
	}
	if err := m.store.CreateCorraldOp(ctx, op, false); err != nil {
		m.clearRunningOp(rop)
		cancel()
		return nil, err
	}

	// Write authorization, binding, and active_op_id immediately when install starts (Item 4, Item 11).
	mb, _ := proto.MarshalManifest(dist.Manifest)
	uid := userID
	ch.State = StateInstalling
	ch.ConsentBy = &uid
	ch.ConsentAt = time.Now().Unix()
	ch.BoundFingerprint = fp
	ch.BoundUsername = sshUser
	ch.BoundGateway = m.gatewayID
	ch.ManifestJSON = string(mb)
	ch.ActiveOpID = opID
	ch.LastError = ""
	ch.RestartCount = 0
	if err := m.store.UpsertCorraldHost(ctx, ch); err != nil {
		op.State = OpFailed
		op.ErrorCode = "other"
		_ = m.store.UpdateCorraldOp(ctx, op)
		m.clearRunningOp(rop)
		cancel()
		return nil, err
	}

	view := m.toOpView(op)
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		defer cancel()
		defer close(rop.done)
		defer m.clearRunningOp(rop)
		defer func() {
			if m.AdminDisabled() {
				op.State = OpCancelled
				if ok, _ := m.store.ReleaseCorraldHostOpAndFinish(context.Background(), hostID, op, StateRemovePending); ok {
					m.markPendingCleanup()
				}
				return
			}
			op.State = OpFailed
			if op.ErrorCode == "" {
				op.ErrorCode = "other"
			}
			_, _ = m.store.ReleaseCorraldHostOpAndFinish(context.Background(), hostID, op, StateOff)
		}()
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
	rop := m.opsByHost[hostID]
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
		// No in-memory goroutine (e.g. post-restart stale op); cancel op and
		// release host row atomically in one transaction.
		op.State = OpCancelled
		if ch, err := m.store.CorraldHostByID(ctx, hostID); err == nil && ch.State == StateInstalling {
			ch.State = StateOff
			ch.ActiveOpID = ""
			_, _ = m.store.FinalizeCorraldOpWithHost(ctx, op, ch)
		} else {
			if released, _ := m.store.ReleaseCorraldHostOpAndFinish(ctx, hostID, op, ""); !released {
				_, _ = m.store.UpdateCorraldOpIfActive(ctx, op)
			}
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
		var client *Client
		m.mutateWatchState(sc, []int64{hostID}, func() {
			client = sc.client
			delete(sc.hostRefs, hostID)
		})

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

		// Also verify whether any other non-paused active peer exists on this gateway.
		otherActive := m.hasOtherActivePeers(ctx, userID, fp, sshUser)
		if !otherActive && remaining == 0 {
			if client != nil && !client.Closed() {
				_, _ = client.Shutdown(ctx, "paused_no_users")
				// Wait briefly for the daemon to close the stream so an
				// immediate Resume does not race against a still-exiting daemon (A2).
				select {
				case <-client.Done():
				case <-time.After(1 * time.Second):
				case <-ctx.Done():
				}
			}
			m.closeSharedConn(sc, true)
			_, _ = m.store.SetCorraldStateIfIdle(ctx, hostID, StateStopped, StateRunning, StatePaused, StateLost)
		} else {
			m.scheduleWatchReconcile(sc)
		}
	}
	m.notifyCoverageChanged([]int64{hostID})
	return nil
}

// Resume re-enables corrald for userID on hostID without re-consent.
func (m *Manager) Resume(ctx context.Context, userID, hostID int64, corralUser string) error {
	if !m.Available() {
		return ErrUnavailable
	}
	if m.AdminDisabled() {
		return ErrAdminDisabled
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
		_, _ = m.store.SetCorraldStateIfIdle(ctx, hostID, StateStopped, StatePaused)
	}
	// Best-effort reconnect/start immediately if host is reachable.
	sshClient, release, fp, sshUser, err := m.ssh.AcquireSSH(ctx, userID, hostID)
	if err == nil && sshClient != nil {
		own := newSSHOwnership(release)
		defer own.releaseIfOwned()
		m.onHostConnectedOwned(ctx, userID, hostID, corralUser, fp, sshUser, sshClient, own)
	}
	return nil
}

// StartRemove initiates withdrawal-first removal of corrald for all users on
// this host account. Any active install/update on the same host OR account is
// cancelled and awaited before removal begins (Items 3 & 4).
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

	sshUser, fp, _ := m.ssh.HostBinding(ctx, userID, hostID)
	if fp == "" {
		fp = ch.BoundFingerprint
	}
	if sshUser == "" {
		sshUser = ch.BoundUsername
	}
	acct := accountKey{Fingerprint: fp, Username: sshUser}

	// Cancel and WAIT for any active install/update op on this host or account
	// to exit completely before creating and running the remove op (Item 4).
	m.cancelAndWaitOps(ctx, hostID, acct, OpInstall, OpUpdate)

	opID := newHexID()
	op := &store.CorraldOp{
		ID:         opID,
		HostID:     hostID,
		AccountKey: store.CorraldAccountKey(fp, sshUser),
		Kind:       OpRemove,
		State:      OpRunning,
		Step:       "mark",
		CreatedBy:  userID,
	}

	opCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	rop := &runningOp{opID: opID, hostID: hostID, acct: acct, kind: OpRemove, step: "mark", cancel: cancel, done: make(chan struct{})}
	if err := m.registerRunningOp(rop); err != nil {
		cancel()
		return nil, err
	}
	if err := m.store.CreateCorraldOp(ctx, op, true); err != nil {
		m.clearRunningOp(rop)
		cancel()
		return nil, err
	}

	if err := m.store.ClaimCorraldHostForRemove(ctx, hostID, opID); err != nil {
		op.State = OpFailed
		op.ErrorCode = "other"
		_ = m.store.UpdateCorraldOp(ctx, op)
		m.clearRunningOp(rop)
		cancel()
		return nil, err
	}
	if fresh, ferr := m.store.CorraldHostByID(ctx, hostID); ferr == nil {
		ch = fresh
	} else {
		ch.State = StateRemoving
		ch.ActiveOpID = opID
		ch.LastError = ""
	}

	view := m.toOpView(op)
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		defer cancel()
		defer close(rop.done)
		defer m.clearRunningOp(rop)
		defer func() {
			op.State = OpUnknown
			_, _ = m.store.ReleaseCorraldHostOpAndFinish(context.Background(), hostID, op, "")
		}()
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
	if m.AdminDisabled() {
		return nil, ErrAdminDisabled
	}
	manifestHash = strings.TrimSpace(manifestHash)
	if manifestHash == "" {
		return nil, ErrManifestRequired
	}
	ch, err := m.store.CorraldHost(ctx, userID, hostID)
	if err != nil {
		return nil, ErrInvalidState
	}

	sshUser, fp, _ := m.ssh.HostBinding(ctx, userID, hostID)
	if fp == "" {
		fp = ch.BoundFingerprint
	}
	if sshUser == "" {
		sshUser = ch.BoundUsername
	}

	// Check whether the user is consenting to an expanded manifest of an already
	// running daemon installed by a newer gateway (Item 6, C-1).
	// Must acquire operation ownership and use conditional DB updates so it can
	// never overwrite a concurrent withdrawal-first remove.
	if !replace {
		if sc := m.sharedConnForHost(hostID, fp, sshUser); sc != nil {
			sc.mu.Lock()
			liveHello := sc.hello
			sc.mu.Unlock()
			if liveHello != nil && liveHello.Release >= dist.Release {
				liveHash, _ := proto.ManifestSHA256(liveHello.Manifest)
				if manifestHash == liveHash {
					tr := Transition(toSnapshot(ch), Event{Kind: EvUpdateStart})
					if !tr.Allowed {
						return nil, ErrInvalidState
					}
					acct := accountKey{Fingerprint: fp, Username: sshUser}
					opID := newHexID()
					op := &store.CorraldOp{
						ID:         opID,
						HostID:     hostID,
						AccountKey: store.CorraldAccountKey(fp, sshUser),
						Kind:       OpUpdate,
						State:      OpRunning,
						Step:       "verify",
						CreatedBy:  userID,
					}
					rop := &runningOp{opID: opID, hostID: hostID, acct: acct, kind: OpUpdate, step: "verify", done: make(chan struct{})}
					if err := m.registerRunningOp(rop); err != nil {
						return nil, err
					}
					defer func() {
						close(rop.done)
						m.clearRunningOp(rop)
					}()
					if err := m.store.CreateCorraldOp(ctx, op, false); err != nil {
						return nil, err
					}
					claimed, err := m.store.ClaimCorraldHostOp(ctx, hostID, opID, "", "")
					if err != nil {
						op.State = OpFailed
						op.ErrorCode = "other"
						_, _ = m.store.UpdateCorraldOpIfActive(context.Background(), op)
						return nil, err
					}
					if !claimed {
						op.State = OpCancelled
						_, _ = m.store.UpdateCorraldOpIfActive(context.Background(), op)
						return nil, ErrInvalidState
					}
					opFinalized := false
					defer func() {
						if !opFinalized {
							// B-3: Always release active_op_id and finalize op atomically
							// on failure, context cancel, or panic so host is never wedged.
							op.State = OpFailed
							if op.ErrorCode == "" {
								op.ErrorCode = "other"
							}
							if released, _ := m.store.ReleaseCorraldHostOpAndFinish(context.Background(), hostID, op, ""); !released {
								_, _ = m.store.UpdateCorraldOpIfActive(context.Background(), op)
							}
						}
					}()
					if m.cfg.AfterScopeClaim != nil {
						m.cfg.AfterScopeClaim(hostID, opID)
					}
					mb, _ := proto.MarshalManifest(liveHello.Manifest)
					committed, err := m.store.CommitCorraldManifestAndOp(ctx, hostID, op, string(mb), liveHello.Version, liveHello.Release)
					if err != nil {
						return nil, err
					}
					opFinalized = true
					if !committed {
						return nil, ErrInvalidState
					}
					sc.mu.Lock()
					sc.scopeChange = nil
					sc.mu.Unlock()
					return m.toOpView(op), nil
				}
			}
		}
	}

	if manifestHash != dist.ManifestHash {
		return nil, ErrManifestChanged
	}

	// Never start a release already marked failed on this host (crash
	// rollback, failed handoff), even when a user confirms an update from a
	// page that still shows the old prompt: that release only comes back
	// with a newer gateway release.
	failed := ch.FailedReleases
	if sc := m.sharedConnForHost(hostID, fp, sshUser); sc != nil {
		sc.mu.Lock()
		failed = mergeReleaseLists(failed, sc.failedReleases)
		sc.mu.Unlock()
	}
	if slices.Contains(failed, dist.Release) {
		return nil, ErrInvalidState
	}

	tr := Transition(toSnapshot(ch), Event{Kind: EvUpdateStart})
	if !tr.Allowed {
		return nil, ErrInvalidState
	}

	mb, _ := proto.MarshalManifest(dist.Manifest)
	ch.ManifestJSON = string(mb)
	return m.launchUpdateOp(ctx, userID, hostID, corralUser, fp, sshUser, ch, dist, string(mb), replace)
}

// Retry resets error/restart counters and attempts to reconnect/start corrald.
func (m *Manager) Retry(ctx context.Context, userID, hostID int64, corralUser string) (*OpView, error) {
	dist := m.Dist()
	if dist == nil {
		return nil, ErrUnavailable
	}
	if m.AdminDisabled() {
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

	sshUser, fp, _ := m.ssh.HostBinding(ctx, userID, hostID)
	if fp == "" {
		fp = ch.BoundFingerprint
	}
	if sshUser == "" {
		sshUser = ch.BoundUsername
	}
	acct := accountKey{Fingerprint: fp, Username: sshUser}

	opID := newHexID()
	op := &store.CorraldOp{
		ID:         opID,
		HostID:     hostID,
		AccountKey: store.CorraldAccountKey(fp, sshUser),
		Kind:       OpInstall,
		State:      OpRunning,
		Step:       "start",
		CreatedBy:  userID,
	}

	opCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	rop := &runningOp{opID: opID, hostID: hostID, acct: acct, kind: OpInstall, step: "start", cancel: cancel, done: make(chan struct{})}
	if err := m.registerRunningOp(rop); err != nil {
		cancel()
		return nil, err
	}
	if err := m.store.CreateCorraldOp(ctx, op, false); err != nil {
		m.clearRunningOp(rop)
		cancel()
		return nil, err
	}

	claimed, err := m.store.ClaimCorraldHostForRetry(ctx, hostID, opID, tr.Next.State)
	if err != nil || !claimed {
		op.State = OpCancelled
		if err != nil {
			op.State = OpFailed
			op.ErrorCode = "other"
		}
		_, _ = m.store.UpdateCorraldOpIfActive(context.Background(), op)
		m.clearRunningOp(rop)
		cancel()
		if err != nil {
			return nil, err
		}
		return nil, ErrInvalidState
	}
	ch.State = tr.Next.State
	ch.RestartCount = 0
	ch.LastError = ""
	ch.ActiveOpID = opID

	view := m.toOpView(op)
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		defer cancel()
		defer close(rop.done)
		defer m.clearRunningOp(rop)
		defer func() {
			op.State = OpFailed
			if op.ErrorCode == "" {
				op.ErrorCode = "other"
			}
			_, _ = m.store.ReleaseCorraldHostOpAndFinish(context.Background(), hostID, op, StateError)
		}()

		sshClient, release, gotFP, gotUser, err := m.ssh.AcquireSSH(opCtx, userID, hostID)
		if err != nil {
			m.finishOpFail(op, ch, &OpFailure{Code: "disconnected", Detail: err.Error(), KeepInstalled: true})
			return
		}
		own := newSSHOwnership(release)
		defer own.releaseIfOwned()

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
			m.runInstallWithSSH(opCtx, rop, op, ch, dist, userID, hostID, corralUser, gotFP, gotUser, sshClient, own)
			return
		}
		probe, cand, _, selErr := m.resolveStartBinary(opCtx, sshClient, probe, hostID, gotFP, gotUser, ch, nil, StartSelectOptions{})
		if selErr != nil {
			m.finishOpFail(op, ch, &OpFailure{Code: "start_failed", Detail: selErr.Error(), MachineID: probe.MachineID, KeepInstalled: true})
			return
		}
		_, sock, fail := RunServeWithRetry(opCtx, sshClient, cand.Path, m.gatewayID, probe.MachineID, probe.OS, probe.Arch, m.busyRetry)
		if fail != nil {
			m.finishOpFail(op, ch, fail)
			return
		}
		if sock == "" {
			sock = probe.SocketPath
		}
		if err := m.attachAndHello(opCtx, userID, hostID, corralUser, gotFP, gotUser, sshClient, own, probe, sock, ch, op.ID); err != nil {
			m.finishOpFail(op, ch, &OpFailure{Code: "start_failed", Detail: err.Error(), MachineID: probe.MachineID, KeepInstalled: true})
			return
		}
		op.State = OpDone
		_, _ = m.store.UpdateCorraldOpIfActive(context.Background(), op)
	}()

	return view, nil
}

// OnHostConnected is called when an SSH connection is established to a host
// (new terminal session or restored terminal after gateway restart).
func (m *Manager) OnHostConnected(ctx context.Context, userID, hostID int64, corralUser, fingerprint, sshUser string, sshClient *ssh.Client) {
	m.onHostConnectedOwned(ctx, userID, hostID, corralUser, fingerprint, sshUser, sshClient, nil)
}

func (m *Manager) onHostConnectedOwned(ctx context.Context, userID, hostID int64, corralUser, fingerprint, sshUser string, sshClient *ssh.Client, own *sshOwnership) {
	if m == nil || !m.Available() || sshClient == nil {
		return
	}
	// A1: Ignore stale asynchronous OnHostConnected callbacks whose (fingerprint,
	// sshUser) no longer matches the host's current pinned binding (e.g. a
	// previous session's goroutine waking up after host key rotation / rebind).
	if curUser, curFP, berr := m.ssh.HostBinding(ctx, userID, hostID); berr == nil {
		if (curFP != "" && fingerprint != "" && curFP != fingerprint) ||
			(curUser != "" && sshUser != "" && curUser != sshUser) {
			return
		}
	}
	if fingerprint != "" && sshUser != "" {
		_ = m.store.RecordSSHVerified(ctx, hostID, fingerprint, sshUser)
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

	if m.AdminDisabled() {
		switch ch.State {
		case StateRemovePending, StateRemoving, StateRunning, StateStopped, StateLost, StateError, StatePaused, StateUpdating, StateInstalling, StateIncompatible:
			if CheckBinding(toSnapshot(ch), fingerprint, sshUser, m.gatewayID) {
				acct := accountKey{Fingerprint: fingerprint, Username: sshUser}
				if !m.hasRunningOp(hostID, acct) {
					m.handleAdminDisabledOnConnect(ctx, userID, hostID, corralUser, fingerprint, sshUser, sshClient, ch)
				}
			}
		}
		return
	}

	// Check if an in-memory op is already running for this host or account.
	acct := accountKey{Fingerprint: fingerprint, Username: sshUser}
	if m.hasRunningOp(hostID, acct) {
		return
	}

	// Check authorization binding BEFORE connecting via atomic CAS SQL comparing
	// against the read binding so neither a concurrent reinstall nor a stale
	// callback identity can clobber a newer binding.
	if ch.State != StateOff && ch.State != StateRemoving && ch.State != StateRemovePending {
		mismatch := false
		if ch.State == StateInstalling {
			mismatch = (ch.BoundFingerprint != "" && ch.BoundFingerprint != fingerprint) ||
				(ch.BoundUsername != "" && ch.BoundUsername != sshUser) ||
				(ch.BoundGateway != "" && ch.BoundGateway != m.gatewayID)
		} else {
			mismatch = !CheckBinding(toSnapshot(ch), fingerprint, sshUser, m.gatewayID)
		}
		if mismatch {
			m.markRebindIfMismatch(ctx, hostID, ch.BoundFingerprint, ch.BoundUsername, ch.BoundGateway, fingerprint, sshUser, true, "on_host_connected")
			return
		}
	}

	// Self-heal any orphaned active_op_id pointing to a finished/missing op.
	if ch.ActiveOpID != "" {
		if cleared, _ := m.store.ClearStaleCorraldHostOp(ctx, hostID); cleared {
			ch.ActiveOpID = ""
		}
	}

	activeOp, activeErr := m.store.ActiveCorraldOp(ctx, hostID)
	if ch.State == StateRemovePending || ch.State == StateRemoving {
		if CheckBinding(toSnapshot(ch), fingerprint, sshUser, m.gatewayID) {
			m.handleAdminDisabledOnConnect(ctx, userID, hostID, corralUser, fingerprint, sshUser, sshClient, ch)
		}
		return
	}
	needsReconcile := activeErr == nil || ch.ActiveOpID != "" || ch.State == StateInstalling || ch.State == StateUpdating
	if needsReconcile {
		m.reconcileHost(ctx, userID, hostID, corralUser, fingerprint, sshUser, sshClient, ch, activeOp)
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
		_, _ = m.store.ClearCorraldErrorForUpgradeIfIdle(ctx, hostID)
		ch.RestartCount = 0
		ch.LastError = ""
	}

	// If a shared connection is already live, register this host/user on it!
	key := accountKey{Fingerprint: fingerprint, Username: sshUser}
	if sc := m.getSharedConn(key); sc != nil {
		sc.mu.Lock()
		live := sc.client != nil && !sc.client.Closed()
		client := sc.client
		stateDir := sc.stateDir
		cachedFailed := slices.Clone(sc.failedReleases)
		sc.mu.Unlock()
		if live && !m.AdminDisabled() {
			hello, err := client.Hello(ctx, proto.HelloParams{
				Gateway:        m.gatewayID,
				GatewayVersion: m.gatewayVersion,
				ProtoMin:       m.protoMin,
				ProtoMax:       m.protoMax,
				Client:         proto.ClientIdentity{CorralUser: corralUser, HostID: strconv.FormatInt(hostID, 10)},
			})
			if err == nil {
				// Refresh failed.json on shared-conn fast path too (Item 15).
				var failedRec *HostFailedRecord
				if stateDir != "" {
					if b, rerr := sftpReadFile(ctx, sshClient, stateDir+"/failed.json", 256<<10); rerr == nil {
						var fr HostFailedRecord
						if json.Unmarshal(b, &fr) == nil {
							failedRec = &fr
							_ = m.store.RecordCorraldFailedReleases(ctx, hostID, fingerprint, sshUser, fr.ReleaseList())
							sc.mu.Lock()
							sc.addFailedReleasesLocked(fr.ReleaseList())
							cachedFailed = slices.Clone(sc.failedReleases)
							sc.mu.Unlock()
						}
					}
				}
				m.policyMu.RLock()
				if m.adminDisabled.Load() {
					m.policyMu.RUnlock()
					m.handleAdminDisabledOnConnect(ctx, userID, hostID, corralUser, fingerprint, sshUser, sshClient, ch)
					return
				}
				m.mu.Lock()
				stillActive := m.conns[key] == sc
				if stillActive {
					m.hostConn[hostID] = key
				}
				m.mu.Unlock()
				if !stillActive {
					m.policyMu.RUnlock()
					if m.AdminDisabled() {
						m.handleAdminDisabledOnConnect(ctx, userID, hostID, corralUser, fingerprint, sshUser, sshClient, ch)
						return
					}
				} else {
					sc.mu.Lock()
					sc.hello = hello
					sc.hostRefs[hostID] = &hostRef{HostID: hostID, OwnerID: userID, CorralUser: corralUser}
					sc.mu.Unlock()
					_, _ = m.store.UpdateCorraldRunningIfIdle(
						ctx, hostID, fingerprint, sshUser, m.gatewayID,
						hello.Version, hello.Release, "", "", nil, time.Now().Unix(), false, nil,
					)
					m.policyMu.RUnlock()
					m.scheduleWatchReconcile(sc)
					m.notifyCoverageChanged([]int64{hostID})
					m.evaluateUpdateAfterConnect(ctx, userID, hostID, corralUser, fingerprint, sshUser, nil, mergeReleaseLists(failedRec.ReleaseList(), cachedFailed, ch.FailedReleases))
					return
				}
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
		_, _ = m.store.SetCorraldStateIfIdle(ctx, hostID, StateOff, StateRunning, StateStopped, StateLost, StateError, StateIncompatible)
		return
	}

	files := ReadHostStateFiles(ctx, sshClient, probe, "")
	if files.Failed != nil {
		_ = m.store.RecordCorraldFailedReleases(ctx, hostID, fingerprint, sshUser, files.Failed.ReleaseList())
		ch.FailedReleases = mergeReleaseLists(ch.FailedReleases, files.Failed.ReleaseList())
	}
	sock := probe.SocketPath
	if files.Meta != nil && files.Meta.SocketPath != "" {
		sock = files.Meta.SocketPath
	}
	if files.Install != nil && len(files.Install.Files) > 0 {
		ch.Files = files.Install.Files
	}
	ch.MachineID = probe.MachineID

	// Try connecting directly first; if not listening, start via `corrald serve`.
	if err := m.attachAndHello(ctx, userID, hostID, corralUser, fingerprint, sshUser, sshClient, own, probe, sock, ch, ""); err != nil {
		if errors.Is(err, ErrAdminDisabled) {
			return
		}
		var rpcErr *proto.RPCError
		if errors.As(err, &rpcErr) && rpcErr.Code == proto.ErrProtoMismatch {
			return // already transitioned to StateIncompatible
		}
		if m.AdminDisabled() {
			m.handleAdminDisabledOnConnect(ctx, userID, hostID, corralUser, fingerprint, sshUser, sshClient, ch)
			return
		}
		probe, cand, allFailed, selErr := m.resolveStartBinary(ctx, sshClient, probe, hostID, fingerprint, sshUser, ch, nil, StartSelectOptions{})
		if selErr != nil {
			_, _ = m.store.SetCorraldErrorIfIdle(ctx, hostID, encodeLastError(&OpFailure{Code: "start_failed", Detail: selErr.Error(), MachineID: probe.MachineID}))
			return
		}
		ch.FailedReleases = allFailed
		if m.AdminDisabled() {
			m.handleAdminDisabledOnConnect(ctx, userID, hostID, corralUser, fingerprint, sshUser, sshClient, ch)
			return
		}
		_, startedSock, fail := RunServeWithRetry(ctx, sshClient, cand.Path, m.gatewayID, probe.MachineID, probe.OS, probe.Arch, m.busyRetry)
		if fail != nil {
			if fail.Code == proto.ErrRemoving {
				m.reconcileHost(ctx, userID, hostID, corralUser, fingerprint, sshUser, sshClient, ch, nil)
				return
			}
			_, _ = m.store.SetCorraldErrorIfIdle(ctx, hostID, encodeLastError(fail))
			return
		}
		if m.AdminDisabled() {
			m.handleAdminDisabledOnConnect(ctx, userID, hostID, corralUser, fingerprint, sshUser, sshClient, ch)
			return
		}
		if startedSock != "" {
			sock = startedSock
		}
		if err := m.attachAndHello(ctx, userID, hostID, corralUser, fingerprint, sshUser, sshClient, own, probe, sock, ch, ""); err != nil {
			if errors.Is(err, ErrAdminDisabled) {
				return
			}
			if !errors.As(err, &rpcErr) || rpcErr.Code != proto.ErrProtoMismatch {
				_, _ = m.store.SetCorraldErrorIfIdle(ctx, hostID, encodeLastError(&OpFailure{Code: "start_failed", Detail: err.Error(), MachineID: probe.MachineID}))
			}
			return
		}
	}

	if sc := m.getSharedConn(key); sc != nil && files.Failed != nil {
		sc.mu.Lock()
		sc.addFailedReleasesLocked(files.Failed.ReleaseList())
		sc.mu.Unlock()
	}

	// After connecting cleanly, evaluate automatic update or scope-change prompt.
	m.evaluateUpdateAfterConnect(ctx, userID, hostID, corralUser, fingerprint, sshUser, probe, mergeReleaseLists(files.Failed.ReleaseList(), ch.FailedReleases))
}

// DeleteHostCleanup handles corrald cleanup during DELETE /api/hosts/{id}.
// Always cancels and waits for any active operation on this host (Item 5).
// When removeCorrald is true and sshClient is reachable, probes the host even
// if no local corrald_hosts row exists (Item 18).
func (m *Manager) DeleteHostCleanup(ctx context.Context, userID, hostID int64, corralUser, fingerprint, sshUser string, removeCorrald bool, sshClient *ssh.Client) *DeleteCorraldOutcome {
	ch, err := m.store.CorraldHost(ctx, userID, hostID)
	hasLocalRow := err == nil && ch != nil
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return &DeleteCorraldOutcome{State: "error", Error: err.Error()}
	}
	if hasLocalRow {
		if ch.BoundFingerprint != "" && fingerprint == "" {
			fingerprint = ch.BoundFingerprint
		}
		if ch.BoundUsername != "" && sshUser == "" {
			sshUser = ch.BoundUsername
		}
	}
	acct := accountKey{Fingerprint: fingerprint, Username: sshUser}

	// Cancel and wait for any active install/update on this host (and account
	// if removing the shared daemon) BEFORE proceeding (Item 5).
	if removeCorrald {
		m.cancelAndWaitOps(ctx, hostID, acct, OpInstall, OpUpdate)
		_ = m.store.CancelActiveCorraldOps(ctx, hostID, store.CorraldAccountKey(fingerprint, sshUser), "host_deleted")
	} else {
		m.cancelAndWaitOps(ctx, hostID, accountKey{}, OpInstall, OpUpdate)
		_ = m.store.CancelActiveCorraldOps(ctx, hostID, "", "host_deleted")
	}

	if !removeCorrald {
		// Unregister this user from the running daemon (best-effort) without removing files.
		if hasLocalRow && ch.State != StateOff {
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
		}
		m.detachHost(hostID)
		return &DeleteCorraldOutcome{State: "skipped"}
	}

	if sshClient == nil {
		m.detachHost(hostID)
		if !hasLocalRow || ch.State == StateOff {
			return &DeleteCorraldOutcome{State: "skipped"}
		}
		return &DeleteCorraldOutcome{
			State:        "unreachable",
			UninstallCmd: DefaultUninstallCmd,
		}
	}

	// Always probe the host when remove_corrald=true and reachable, even if
	// local corrald_hosts is missing or off (Item 18).
	probe, err := m.probeHost(ctx, sshClient)
	if err != nil {
		m.detachHost(hostID)
		return &DeleteCorraldOutcome{
			State:        "unreachable",
			Error:        err.Error(),
			UninstallCmd: DefaultUninstallCmd,
		}
	}

	hasOnHost := probe.HasSymlink || probe.HasInstall || probe.HasTombstone || len(probe.Versions) > 0 || len(probe.Stages) > 0
	if !hasOnHost {
		files := ReadHostStateFiles(ctx, sshClient, probe, "")
		hasOnHost = files.Install != nil || files.Meta != nil
	}
	if !hasOnHost {
		m.detachHost(hostID)
		return &DeleteCorraldOutcome{State: "skipped"}
	}

	// Close gateway connection to daemon before uninstalling.
	if sc := m.sharedConnForHost(hostID, fingerprint, sshUser); sc != nil {
		m.closeSharedConn(sc, true)
	}
	m.detachHost(hostID)

	out := RunUninstallWithRetry(ctx, sshClient, probe, newHexID(), m.busyRetry, nil)
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
// deleted or permanently disabled (NOT on password reset — Item 17).
// For each host owned by userID:
//   - Paused local peers count as users (Item 16).
//   - If remote user count cannot be determined, the helper daemon is preserved
//     and the result for that host is "not_cleaned" (Item 16).
//   - Only when no local peers (active or paused) AND confirmed zero remote
//     clients remain is the daemon uninstalled.
func (m *Manager) CleanupUserHosts(ctx context.Context, userID int64, corralUser string) map[int64]string {
	outcomes := make(map[int64]string)
	if m == nil {
		return outcomes
	}
	hosts, states, err := m.store.ListUserCorraldHosts(ctx, userID)
	if err != nil || len(hosts) == 0 {
		return outcomes
	}
	for i, h := range hosts {
		ch := states[i]
		fp := ch.BoundFingerprint
		sshUser := ch.BoundUsername
		if sshUser == "" {
			sshUser = h.Username
		}

		// Cancel any active install/update owned by this user on this host.
		m.cancelAndWaitOps(ctx, h.ID, accountKey{}, OpInstall, OpUpdate)

		// Paused peers still count as users who need the helper kept (Item 16).
		otherPeers := m.hasOtherPeersIncludingPaused(ctx, userID, fp, sshUser)

		// Check remote clients via live sharedConn first.
		remoteKnown := false
		remainingRemote := -1
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
					remoteKnown = true
					remainingRemote = fres.RemainingClients
				}
				cancel()
			}
		}

		if otherPeers || (remoteKnown && remainingRemote > 0) {
			m.detachHost(h.ID)
			outcomes[h.ID] = "kept_other_users"
			continue
		}

		// Acquire SSH to either check remote clients (if not yet known) and/or uninstall.
		sshClient, release, gotFP, gotUser, err := m.ssh.AcquireSSH(ctx, userID, h.ID)
		if err != nil || sshClient == nil {
			m.detachHost(h.ID)
			outcomes[h.ID] = "not_cleaned"
			continue
		}
		if fp != "" && (gotFP != fp || (sshUser != "" && gotUser != sshUser)) {
			release()
			m.detachHost(h.ID)
			outcomes[h.ID] = "not_cleaned"
			continue
		}

		// If remote users are still unknown, query daemon or clients.json over this SSH connection.
		if !remoteKnown {
			probe, perr := m.probeHost(ctx, sshClient)
			if perr == nil && probe != nil {
				files := ReadHostStateFiles(ctx, sshClient, probe, "")
				sock := probe.SocketPath
				if files.Meta != nil && files.Meta.SocketPath != "" {
					sock = files.Meta.SocketPath
				}
				if sock != "" {
					if tmpClient, derr := Dial(ctx, sshClient, sock, nil); derr == nil {
						_, herr := tmpClient.Hello(ctx, proto.HelloParams{
							Gateway:        m.gatewayID,
							GatewayVersion: m.gatewayVersion,
							ProtoMin:       m.protoMin,
							ProtoMax:       m.protoMax,
							Client:         proto.ClientIdentity{CorralUser: corralUser, HostID: strconv.FormatInt(h.ID, 10)},
						})
						if herr == nil {
							if fres, ferr := tmpClient.Forget(ctx, proto.ForgetParams{
								Gateway:    m.gatewayID,
								CorralUser: corralUser,
								HostID:     strconv.FormatInt(h.ID, 10),
							}); ferr == nil && fres != nil {
								remoteKnown = true
								remainingRemote = fres.RemainingClients
							}
						}
						_ = tmpClient.Close()
					}
				}
				// If daemon isn't running, fall back to clients.json on disk.
				if !remoteKnown && files.ClientsLoaded {
					otherRemote := 0
					hostIDStr := strconv.FormatInt(h.ID, 10)
					for _, rc := range files.Clients {
						if rc.Gateway == m.gatewayID && rc.CorralUser == corralUser && (rc.HostID == "" || rc.HostID == hostIDStr) {
							continue
						}
						otherRemote++
					}
					remoteKnown = true
					remainingRemote = otherRemote
				}
			}
		}

		if !remoteKnown {
			// Cannot confirm zero remote users -> preserve daemon! (Item 16)
			release()
			m.detachHost(h.ID)
			outcomes[h.ID] = "not_cleaned"
			continue
		}
		if remainingRemote > 0 {
			release()
			m.detachHost(h.ID)
			outcomes[h.ID] = "kept_other_users"
			continue
		}

		// Acquire per-account cleanup op lock, close our gateway connection
		// without unconditional shutdown, and execute host-side atomic
		// sole-gateway uninstall under .corrald-shared.lock so another gateway
		// joining concurrently can never have its daemon killed or deleted.
		acct := accountKey{Fingerprint: fp, Username: sshUser}
		var rop *runningOp
		if acct.valid() {
			var ok bool
			rop, ok = m.acquireCleanupOpLock(h.ID, acct)
			if !ok {
				release()
				m.detachHost(h.ID)
				outcomes[h.ID] = "not_cleaned"
				continue
			}
		}

		if sc := m.sharedConnForHost(h.ID, fp, sshUser); sc != nil {
			m.mu.Lock()
			if m.conns[sc.key] == sc {
				delete(m.conns, sc.key)
			}
			delete(m.hostConn, h.ID)
			m.mu.Unlock()
			m.closeSharedConn(sc, false)
		} else {
			m.detachHost(h.ID)
		}

		probe, perr := m.probeHost(ctx, sshClient)
		if perr != nil || probe == nil {
			if rop != nil {
				m.releaseCleanupOpLock(rop)
			}
			release()
			outcomes[h.ID] = "not_cleaned"
			continue
		}

		opID := newHexID()
		if rop != nil {
			opID = rop.opID
		}
		out := RunUninstallSoleGatewayWithRetry(ctx, sshClient, m.Dist(), probe, opID, m.gatewayID, m.busyRetry, nil)
		release()
		switch out.Result {
		case "removed":
			if m.cfg.BeforeCleanupUserDBReset != nil {
				m.cfg.BeforeCleanupUserDBReset(h.ID)
			}
			_ = m.store.CorraldResetByBindingIfIdle(ctx, fp, sshUser, opID)
			outcomes[h.ID] = "removed"
		case "other_gateways":
			outcomes[h.ID] = "kept_other_users"
		default:
			outcomes[h.ID] = "not_cleaned"
		}
		// Keep per-account cleanup op lock held until AFTER the conditional DB
		// reset completes so a concurrent install on a peer host cannot start
		// and get clobbered between uninstall and DB reset.
		if rop != nil {
			m.releaseCleanupOpLock(rop)
		}
	}
	return outcomes
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
	own := newSSHOwnership(release)
	defer own.releaseIfOwned()

	// If binding was unknown when StartInstall began, bind it now and enforce account exclusivity.
	acct := accountKey{Fingerprint: fp, Username: sshUser}
	if acct.valid() && !rop.acct.valid() {
		if err := m.attachOpAccount(rop, acct); err != nil {
			m.finishOpFail(op, ch, &OpFailure{Code: "other", Detail: err.Error()})
			return
		}
	}
	if ch.BoundFingerprint == "" && fp != "" {
		ch.BoundFingerprint = fp
		ch.BoundUsername = sshUser
		ok, _ := m.store.UpdateCorraldHostIfOp(context.Background(), ch, op.ID, false)
		if !ok {
			return // superseded by remove
		}
	}

	m.runInstallWithSSH(ctx, rop, op, ch, dist, userID, hostID, corralUser, fp, sshUser, sshClient, own)
}

func (m *Manager) runInstallWithSSH(ctx context.Context, rop *runningOp, op *store.CorraldOp, ch *store.CorraldHost, dist *Dist, userID, hostID int64, corralUser, fp, sshUser string, sshClient *ssh.Client, own *sshOwnership) {
	setStep := func(step string) {
		m.mu.Lock()
		rop.step = step
		m.mu.Unlock()
		op.Step = step
		_, _ = m.store.UpdateCorraldOpIfActive(context.Background(), op)
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

	// Check existing installation on THIS machine (<mid>/install.json) before
	// staging/installing (C-4): a shared NFS symlink alone is NOT enough because
	// `corrald serve` requires this machine's own install.json. Only skip install
	// when this machine already has install.json with Release >= dist.Release.
	existingFiles := ReadHostStateFiles(ctx, sshClient, probe, "")
	if probe.HasSymlink && existingFiles.Install != nil {
		existingRel := existingFiles.Install.Release
		if existingFiles.Meta != nil && existingFiles.Meta.Release > existingRel {
			existingRel = existingFiles.Meta.Release
		}
		if existingRel > 0 && existingRel >= dist.Release {
			setStep("start")
			sock := probe.SocketPath
			if existingFiles.Meta != nil && existingFiles.Meta.SocketPath != "" {
				sock = existingFiles.Meta.SocketPath
			}
			ch.MachineID = probe.MachineID
			ch.SocketPath = sock
			if existingFiles.Install != nil && len(existingFiles.Install.Files) > 0 {
				ch.Files = existingFiles.Install.Files
			}
			if err := m.attachAndHello(ctx, userID, hostID, corralUser, fp, sshUser, sshClient, own, probe, sock, ch, op.ID); err != nil {
				if errors.Is(err, ErrAdminDisabled) {
					op.State = OpCancelled
					_, _ = m.store.UpdateCorraldOpIfActive(context.Background(), op)
					return
				}
				var rpcErr *proto.RPCError
				if errors.As(err, &rpcErr) && rpcErr.Code == proto.ErrProtoMismatch {
					op.State = OpFailed
					op.ErrorCode = proto.ErrProtoMismatch
					_, _ = m.store.UpdateCorraldOpIfActive(context.Background(), op)
					return
				}
				probe, cand, _, selErr := m.resolveStartBinary(ctx, sshClient, probe, hostID, fp, sshUser, ch, nil, StartSelectOptions{})
				if selErr != nil {
					m.finishOpFail(op, ch, &OpFailure{
						Code:          "start_failed",
						Detail:        selErr.Error(),
						MachineID:     probe.MachineID,
						OS:            probe.OS,
						Arch:          probe.Arch,
						KeepInstalled: true,
					})
					return
				}
				_, startedSock, serveFail := RunServeWithRetry(ctx, sshClient, cand.Path, m.gatewayID, probe.MachineID, probe.OS, probe.Arch, m.busyRetry)
				if serveFail != nil {
					m.finishOpFail(op, ch, serveFail)
					return
				}
				if startedSock != "" {
					sock = startedSock
					ch.SocketPath = sock
				}
				if err := m.attachAndHello(ctx, userID, hostID, corralUser, fp, sshUser, sshClient, own, probe, sock, ch, op.ID); err != nil {
					if errors.Is(err, ErrAdminDisabled) {
						op.State = OpCancelled
						_, _ = m.store.UpdateCorraldOpIfActive(context.Background(), op)
						return
					}
					if errors.As(err, &rpcErr) && rpcErr.Code == proto.ErrProtoMismatch {
						op.State = OpFailed
						op.ErrorCode = proto.ErrProtoMismatch
						_, _ = m.store.UpdateCorraldOpIfActive(context.Background(), op)
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
			}
			op.State = OpDone
			op.ErrorCode = ""
			_, _ = m.store.UpdateCorraldOpIfActive(context.Background(), op)
			return
		}
	}

	_, sfail := StageAndInstallWithRetry(ctx, sshClient, dist, probe, bin, op.ID, m.gatewayID, corralUser, false, m.busyRetry, setStep, isCancelled, m.cfg.VerifySHA256Hook)
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
		// Cancelled right before start: uninstall ONLY if confirmed sole gateway
		// under host-side locks; never delete another gateway's installation.
		if m.gatewayID != "" {
			out := RunUninstallSoleGatewayWithRetry(context.Background(), sshClient, m.Dist(), probe, op.ID, m.gatewayID, m.busyRetry, nil)
			switch out.Result {
			case "removed":
				m.finishOpCancelled(op, ch)
			case "other_gateways", "unsupported":
				op.State = OpCancelled
				op.ErrorCode = ""
				ch.State = StateStopped
				ch.LastError = ""
				_, _ = m.store.FinalizeCorraldOpWithHost(context.Background(), op, ch)
			default:
				op.State = OpCancelled
				op.ErrorCode = ""
				ch.State = StateRemovePending
				ch.LastError = ""
				_, _ = m.store.FinalizeCorraldOpWithHost(context.Background(), op, ch)
				m.markPendingCleanup()
			}
			return
		}
		op.State = OpCancelled
		op.ErrorCode = ""
		ch.State = StateStopped
		ch.LastError = ""
		_, _ = m.store.FinalizeCorraldOpWithHost(context.Background(), op, ch)
		return
	}

	// Step: start (no longer cancellable past this point).
	// Re-probe so SelectStartBinary observes the freshly installed symlink & version dirs.
	if m.AdminDisabled() {
		m.handleAdminDisabledWhileHoldingOp(context.Background(), userID, hostID, corralUser, fp, sshUser, sshClient, ch, op.ID)
		op.State = OpCancelled
		_, _ = m.store.UpdateCorraldOpIfActive(context.Background(), op)
		return
	}
	setStep("start")
	freshProbe, cand, _, selErr := m.resolveStartBinary(ctx, sshClient, nil, hostID, fp, sshUser, ch, nil, StartSelectOptions{})
	if selErr != nil {
		ch.MachineID = probe.MachineID
		m.finishOpFail(op, ch, &OpFailure{
			Code:          "start_failed",
			Detail:        selErr.Error(),
			MachineID:     probe.MachineID,
			OS:            probe.OS,
			Arch:          probe.Arch,
			KeepInstalled: true,
		})
		return
	}
	if freshProbe != nil {
		probe = freshProbe
	}
	if m.AdminDisabled() {
		m.handleAdminDisabledWhileHoldingOp(context.Background(), userID, hostID, corralUser, fp, sshUser, sshClient, ch, op.ID)
		op.State = OpCancelled
		_, _ = m.store.UpdateCorraldOpIfActive(context.Background(), op)
		return
	}
	_, sock, serveFail := RunServeWithRetry(ctx, sshClient, cand.Path, m.gatewayID, probe.MachineID, probe.OS, probe.Arch, m.busyRetry)
	if serveFail != nil {
		ch.MachineID = probe.MachineID
		m.finishOpFail(op, ch, serveFail)
		return
	}
	if m.cfg.AfterInstallServe != nil {
		m.cfg.AfterInstallServe(hostID, op.ID)
	}
	if m.AdminDisabled() {
		m.handleAdminDisabledWhileHoldingOp(context.Background(), userID, hostID, corralUser, fp, sshUser, sshClient, ch, op.ID)
		op.State = OpCancelled
		_, _ = m.store.UpdateCorraldOpIfActive(context.Background(), op)
		return
	}
	if sock == "" {
		sock = probe.SocketPath
	}

	ch.MachineID = probe.MachineID
	ch.SocketPath = sock

	files := ReadHostStateFiles(ctx, sshClient, probe, op.ID)
	if files.Install != nil && len(files.Install.Files) > 0 {
		ch.Files = files.Install.Files
	}

	if err := m.attachAndHello(ctx, userID, hostID, corralUser, fp, sshUser, sshClient, own, probe, sock, ch, op.ID); err != nil {
		if errors.Is(err, ErrAdminDisabled) {
			op.State = OpCancelled
			_, _ = m.store.UpdateCorraldOpIfActive(context.Background(), op)
			return
		}
		var rpcErr *proto.RPCError
		if errors.As(err, &rpcErr) && rpcErr.Code == proto.ErrProtoMismatch {
			op.State = OpFailed
			op.ErrorCode = proto.ErrProtoMismatch
			_, _ = m.store.UpdateCorraldOpIfActive(context.Background(), op)
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
	_, _ = m.store.UpdateCorraldOpIfActive(context.Background(), op)
}

func (m *Manager) runRemoveOp(ctx context.Context, rop *runningOp, op *store.CorraldOp, ch *store.CorraldHost, userID, hostID int64) {
	setStep := func(step string) {
		m.mu.Lock()
		rop.step = step
		m.mu.Unlock()
		op.Step = step
		_, _ = m.store.UpdateCorraldOpIfActive(context.Background(), op)
	}

	sshClient, release, fp, sshUser, err := m.ssh.AcquireSSH(ctx, userID, hostID)
	if err != nil {
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

	out := RunUninstallWithRetry(ctx, sshClient, probe, op.ID, m.busyRetry, setStep)
	switch out.Result {
	case "removed":
		_ = m.store.CorraldResetByBinding(context.Background(), fp, sshUser)
		ch.State = StateOff
		ch.Version = ""
		ch.Release = 0
		ch.SocketPath = ""
		ch.Files = []string{}
		ch.PausedUsers = []int64{}
		ch.FailedReleases = []int64{}
		ch.LastError = ""
		ch.RestartCount = 0
		ch.UpdatedFromVer = ""
		ch.UpdatedFromRel = 0
		ch.UpdatedFromTarget = ""
		ch.UpdateTargetVer = ""
		ch.UpdateTargetRel = 0
		ch.UpdateReadyAt = 0
		ch.UpdateCrashCount = 0
		op.State = OpDone
		op.ErrorCode = ""
		op.Detail = encodeOpDetail(opDetailPayload{Result: "removed"})
		_, _ = m.store.FinalizeCorraldOpWithHost(context.Background(), op, ch)
	case "partial":
		fail := &OpFailure{Code: "partial", Left: out.Left}
		ch.State = StateError
		ch.LastError = encodeLastError(fail)
		op.State = OpFailed
		op.ErrorCode = "partial"
		op.Detail = encodeOpDetail(opDetailPayload{Result: "partial", Left: out.Left})
		_, _ = m.store.FinalizeCorraldOpWithHost(context.Background(), op, ch)
	case "stop_failed":
		fail := &OpFailure{Code: "stop_failed", PID: out.PID, Detail: out.Detail}
		ch.State = StateError
		ch.LastError = encodeLastError(fail)
		op.State = OpFailed
		op.ErrorCode = "stop_failed"
		op.Detail = encodeOpDetail(opDetailPayload{Result: "stop_failed", PID: out.PID, Message: out.Detail})
		_, _ = m.store.FinalizeCorraldOpWithHost(context.Background(), op, ch)
	default:
		m.markRemovePending(op, ch, out.Detail)
	}
}

func (m *Manager) markRemovePending(op *store.CorraldOp, ch *store.CorraldHost, reason string) {
	ch.State = StateRemovePending
	ch.LastError = ""
	op.State = OpUnknown
	op.ErrorCode = ""
	op.Detail = encodeOpDetail(opDetailPayload{Result: "pending", Message: reason})
	_, _ = m.store.FinalizeCorraldOpWithHost(context.Background(), op, ch)
}

func (m *Manager) launchUpdateOp(ctx context.Context, userID, hostID int64, corralUser, fp, sshUser string, ch *store.CorraldHost, dist *Dist, manifestJSON string, replace bool) (*OpView, error) {
	if fp == "" {
		fp = ch.BoundFingerprint
	}
	if sshUser == "" {
		sshUser = ch.BoundUsername
	}
	acct := accountKey{Fingerprint: fp, Username: sshUser}

	opID := newHexID()
	op := &store.CorraldOp{
		ID:         opID,
		HostID:     hostID,
		AccountKey: store.CorraldAccountKey(fp, sshUser),
		Kind:       OpUpdate,
		State:      OpRunning,
		Step:       "upload",
		CreatedBy:  userID,
	}

	opCtx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	rop := &runningOp{opID: opID, hostID: hostID, acct: acct, kind: OpUpdate, step: "upload", cancel: cancel, done: make(chan struct{})}
	if err := m.registerRunningOp(rop); err != nil {
		cancel()
		return nil, err
	}
	if err := m.store.CreateCorraldOp(ctx, op, false); err != nil {
		m.clearRunningOp(rop)
		cancel()
		return nil, err
	}

	claimed, err := m.store.ClaimCorraldHostOp(ctx, hostID, opID, StateUpdating, manifestJSON)
	if err != nil || !claimed {
		op.State = OpCancelled
		if err != nil {
			op.State = OpFailed
			op.ErrorCode = "other"
		}
		_, _ = m.store.UpdateCorraldOpIfActive(context.Background(), op)
		m.clearRunningOp(rop)
		cancel()
		if err != nil {
			return nil, err
		}
		return nil, ErrInvalidState
	}
	ch.State = StateUpdating
	ch.ActiveOpID = opID
	ch.LastError = ""
	if manifestJSON != "" {
		ch.ManifestJSON = manifestJSON
	}

	view := m.toOpView(op)
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		defer cancel()
		defer close(rop.done)
		defer m.clearRunningOp(rop)
		defer func() {
			if m.AdminDisabled() {
				op.State = OpCancelled
				if ok, _ := m.store.ReleaseCorraldHostOpAndFinish(context.Background(), hostID, op, StateRemovePending); ok {
					m.markPendingCleanup()
				}
				return
			}
			op.State = OpFailed
			if op.ErrorCode == "" {
				op.ErrorCode = "other"
			}
			_, _ = m.store.ReleaseCorraldHostOpAndFinish(context.Background(), hostID, op, StateRunning)
		}()
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
		_, _ = m.store.UpdateCorraldOpIfActive(context.Background(), op)
	}
	isCancelled := func() bool { return m.isOpCancelled(rop) || m.AdminDisabled() }

	prevVer := ch.Version
	prevRel := ch.Release

	sshClient, release, fp, sshUser, err := m.ssh.AcquireSSH(ctx, userID, hostID)
	if err != nil {
		if isCancelled() {
			return
		}
		m.finishUpdateFailKeepPrev(op, ch, prevVer, prevRel, dist.Version, err.Error())
		return
	}
	own := newSSHOwnership(release)
	defer own.releaseIfOwned()
	// Item #2: If admin disabled corrald during an in-flight update and this
	// update op still owns the host row, run safe sole-gateway cleanup while we
	// still hold the account op lock and live SSH (fallback in StartUpdate's
	// outer defer records remove_pending if cleanup could not complete).
	defer func() {
		if m.AdminDisabled() {
			if fresh, ferr := m.store.CorraldHostByID(context.Background(), hostID); ferr == nil && fresh.ActiveOpID == op.ID {
				m.handleAdminDisabledWhileHoldingOp(context.Background(), userID, hostID, corralUser, fp, sshUser, sshClient, ch, op.ID)
			}
		}
	}()

	probe, err := m.probeHost(ctx, sshClient)
	if err != nil {
		if isCancelled() {
			return
		}
		m.finishUpdateFailKeepPrev(op, ch, prevVer, prevRel, dist.Version, err.Error())
		return
	}
	bin, vfail := ValidateProbe(probe, dist)
	if vfail != nil {
		if isCancelled() {
			return
		}
		m.finishUpdateFailKeepPrev(op, ch, prevVer, prevRel, dist.Version, vfail.Error())
		return
	}

	if m.cfg.BeforeUpdateStage != nil {
		m.cfg.BeforeUpdateStage(hostID, op.ID)
	}
	if isCancelled() {
		return
	}

	// Replace mode (from incompatible state): stop current daemon, install non-upgrade, start.
	if replace {
		if sc := m.sharedConnForHost(hostID, fp, sshUser); sc != nil {
			m.closeSharedConn(sc, true)
		}
		_, _ = runSSH(ctx, sshClient, StopCommand(probe.CurrentBin), 4096, sshx.ExecOptions{})
		_, sfail := StageAndInstallWithRetry(ctx, sshClient, dist, probe, bin, op.ID, m.gatewayID, corralUser, false, m.busyRetry, setStep, isCancelled, m.cfg.VerifySHA256Hook)
		if sfail != nil {
			if isCancelled() {
				return
			}
			ch.State = StateError
			ch.LastError = encodeLastError(sfail)
			op.State = OpFailed
			op.ErrorCode = sfail.Code
			_, _ = m.store.FinalizeCorraldOpWithHost(context.Background(), op, ch)
			return
		}
		setStep("handoff")
		freshProbe, cand, _, selErr := m.resolveStartBinary(ctx, sshClient, nil, hostID, fp, sshUser, ch, nil, StartSelectOptions{
			Rollback:        true,
			PreferredTarget: fmt.Sprintf("corrald-%s-%d/corrald", dist.Version, dist.Release),
		})
		if selErr != nil {
			if isCancelled() {
				return
			}
			ch.State = StateError
			ch.LastError = encodeLastError(&OpFailure{Code: "start_failed", Detail: selErr.Error(), MachineID: probe.MachineID})
			op.State = OpFailed
			op.ErrorCode = "start_failed"
			_, _ = m.store.FinalizeCorraldOpWithHost(context.Background(), op, ch)
			return
		}
		if freshProbe != nil {
			probe = freshProbe
		}
		if isCancelled() {
			return
		}
		_, sock, serveFail := RunServeWithRetry(ctx, sshClient, cand.Path, m.gatewayID, probe.MachineID, probe.OS, probe.Arch, m.busyRetry)
		if serveFail != nil {
			if isCancelled() {
				return
			}
			ch.State = StateError
			ch.LastError = encodeLastError(serveFail)
			op.State = OpFailed
			op.ErrorCode = serveFail.Code
			_, _ = m.store.FinalizeCorraldOpWithHost(context.Background(), op, ch)
			return
		}
		if isCancelled() {
			if m.AdminDisabled() {
				m.handleAdminDisabledOnConnect(context.Background(), userID, hostID, corralUser, fp, sshUser, sshClient, ch)
			}
			return
		}
		setStep("verify")
		if sock == "" {
			sock = probe.SocketPath
		}
		if err := m.attachAndHello(ctx, userID, hostID, corralUser, fp, sshUser, sshClient, own, probe, sock, ch, op.ID); err != nil {
			if isCancelled() || errors.Is(err, ErrAdminDisabled) {
				return
			}
			ch.State = StateError
			ch.LastError = encodeLastError(&OpFailure{Code: "start_failed", Detail: err.Error()})
			op.State = OpFailed
			op.ErrorCode = "start_failed"
			_, _ = m.store.FinalizeCorraldOpWithHost(context.Background(), op, ch)
			return
		}
		op.State = OpDone
		_, _ = m.store.UpdateCorraldOpIfActive(context.Background(), op)
		return
	}

	// Normal update path: determine whether a live daemon is connected.
	// C-2: Never manipulate the shared symlink directly from the gateway;
	// cold-start upgrade uses `corrald install` (no --upgrade, which holds
	// SharedLock and enforces no-downgrade) + `corrald serve`.
	sc := m.sharedConnForHost(hostID, fp, sshUser)
	var client *Client
	if sc != nil {
		sc.mu.Lock()
		client = sc.client
		sc.mu.Unlock()
	}

	runColdStartUpgrade := func() {
		_, sfail := StageAndInstallWithRetry(ctx, sshClient, dist, probe, bin, op.ID, m.gatewayID, corralUser, false, m.busyRetry, setStep, isCancelled, m.cfg.VerifySHA256Hook)
		if sfail != nil {
			if sfail.Code == "cancelled" || isCancelled() {
				return
			}
			m.finishUpdateFailError(op, ch, dist.Version, probe.MachineID, sfail.Error())
			return
		}
		if isCancelled() {
			return
		}
		setStep("handoff")
		// Re-probe after install so SelectStartBinary inspects the actual
		// shared symlink target (e.g. an NFS peer's higher R3) and failed.json,
		// and returns an immutable versioned binary path (never the symlink).
		freshProbe, cand, _, selErr := m.resolveStartBinary(ctx, sshClient, nil, hostID, fp, sshUser, ch, sc, StartSelectOptions{})
		if selErr != nil {
			m.finishUpdateFailError(op, ch, dist.Version, probe.MachineID, selErr.Error())
			return
		}
		if freshProbe != nil {
			probe = freshProbe
		}
		attemptedVer := cand.Version
		if attemptedVer == "" {
			attemptedVer = dist.Version
		}
		tryColdRollback := func(primaryErr string, failedRel int64, stopRunning bool) {
			_, rbCand, _, rberr := m.resolveStartBinary(ctx, sshClient, probe, hostID, fp, sshUser, ch, sc, StartSelectOptions{
				Rollback:     true,
				BelowRelease: failedRel,
			})
			if rberr != nil || rbCand.Path == "" {
				m.finishUpdateFailError(op, ch, attemptedVer, probe.MachineID, fmt.Sprintf("%s; rollback unavailable: %v", primaryErr, rberr))
				return
			}
			// Fix5 #3: When a running incompatible daemon is still holding the
			// socket/lock (e.g. hello returned proto_mismatch), stop it first
			// (verified via meta.json PID + start_time in `corrald stop`) before
			// launching the rollback binary.
			if stopRunning {
				stopRes, stopErr := runSSH(ctx, sshClient, StopCommand(rbCand.Path), 4096, sshx.ExecOptions{})
				sf := ParseKeyValues(stopRes.Out)
				if sf["error"] == "stop_failed" || (stopErr != nil && sf["result"] != "ok") {
					detail := firstNonEmpty(sf["detail"], sf["error"], errStr(stopErr))
					m.finishUpdateFailError(op, ch, attemptedVer, probe.MachineID, fmt.Sprintf("%s; stop incompatible daemon failed: %s", primaryErr, detail))
					return
				}
			}
			_, rbSock, rbFail := RunServeWithRetry(ctx, sshClient, rbCand.Path, m.gatewayID, probe.MachineID, probe.OS, probe.Arch, m.busyRetry)
			if rbFail != nil {
				m.finishUpdateFailError(op, ch, attemptedVer, probe.MachineID, fmt.Sprintf("%s; rollback serve failed: %s", primaryErr, rbFail.Error()))
				return
			}
			if rbSock == "" {
				rbSock = probe.SocketPath
			}
			// Only transition to StateRunning if we genuinely connect and complete
			// hello on the rolled-back daemon; otherwise enter StateError so Retry works.
			if aerr := m.attachAndHello(ctx, userID, hostID, corralUser, fp, sshUser, sshClient, own, probe, rbSock, ch, ""); aerr != nil {
				m.finishUpdateFailError(op, ch, attemptedVer, probe.MachineID, fmt.Sprintf("%s; rollback connect failed: %v", primaryErr, aerr))
				return
			}
			rbVer, rbRel := prevVer, prevRel
			if curSC := m.sharedConnForHost(hostID, fp, sshUser); curSC != nil {
				curSC.mu.Lock()
				if curSC.hello != nil && curSC.hello.Release > 0 {
					rbVer = curSC.hello.Version
					rbRel = curSC.hello.Release
				}
				curSC.mu.Unlock()
			}
			m.finishUpdateFailKeepPrev(op, ch, rbVer, rbRel, attemptedVer, primaryErr)
		}

		if isCancelled() {
			return
		}
		_, sock, serveFail := RunServeWithRetry(ctx, sshClient, cand.Path, m.gatewayID, probe.MachineID, probe.OS, probe.Arch, m.busyRetry)
		if serveFail != nil {
			// B-4: Attribute failure to the ACTUAL started release (cand.Release),
			// NEVER dist.Release when another NFS machine held a higher release!
			if serveFail.Code != proto.ErrBusy && serveFail.Code != "disconnected" && serveFail.Code != proto.ErrRemoving {
				if cand.Release > 0 {
					_ = RecordFailedOnHost(ctx, sshClient, probe.Home, probe.UIDInt64(), probe.FailedJSON, cand.Release, serveFail.Error())
					_ = m.store.RecordCorraldFailedReleases(context.Background(), hostID, fp, sshUser, []int64{cand.Release})
					ch.FailedReleases = mergeReleaseLists(ch.FailedReleases, []int64{cand.Release})
					if sc != nil {
						sc.mu.Lock()
						sc.addFailedReleasesLocked([]int64{cand.Release})
						sc.mu.Unlock()
					}
				}
			}
			tryColdRollback(serveFail.Error(), cand.Release, false)
			return
		}
		if isCancelled() {
			if m.AdminDisabled() {
				m.handleAdminDisabledOnConnect(context.Background(), userID, hostID, corralUser, fp, sshUser, sshClient, ch)
			}
			return
		}
		setStep("verify")
		if sock == "" {
			sock = probe.SocketPath
		}
		if err := m.attachAndHello(ctx, userID, hostID, corralUser, fp, sshUser, sshClient, own, probe, sock, ch, op.ID); err != nil {
			if errors.Is(err, ErrAdminDisabled) || isCancelled() {
				return
			}
			tryColdRollback(err.Error(), cand.Release, true)
			return
		}
		// Arm crash-loop rollback window using ONLY the actual running hello
		// version/release and window start time (rollback target is computed
		// dynamically at rollback time by SelectStartBinary).
		var coldActualVer string
		var coldActualRel int64
		if curSC := m.sharedConnForHost(hostID, fp, sshUser); curSC != nil {
			curSC.mu.Lock()
			newHello := curSC.hello
			curSC.mu.Unlock()
			if newHello != nil {
				coldActualVer = newHello.Version
				coldActualRel = newHello.Release
				readyTime := time.Now()
				win := computePostUpdateCrashWindow(readyTime.Unix(), newHello.Version, newHello.Release, prevRel)
				if win.ReadyAt > 0 {
					curSC.mu.Lock()
					curSC.updatedAt = readyTime
					curSC.updateTargetVer = win.TargetVer
					curSC.updateTargetRel = win.TargetRel
					curSC.updateCrashCount = 0
					curSC.prevVersion = ""
					curSC.prevRelease = 0
					curSC.prevRelTarget = ""
					curSC.scopeChange = nil
					curSC.mu.Unlock()
					_ = m.store.UpdateCorraldCrashWindowByBinding(
						context.Background(), hostID, fp, sshUser,
						win.ReadyAt, win.TargetVer, win.TargetRel, "", 0, "", 0,
					)
				}
			}
		}
		op.State = OpDone
		if updated, _ := m.store.UpdateCorraldOpIfActive(context.Background(), op); updated {
			if coldActualVer == "" {
				coldActualVer = dist.Version
				coldActualRel = dist.Release
			}
			m.recordUpdateNotices(hostID, fp, sshUser, prevVer, coldActualVer, coldActualRel, true)
		}
	}

	if client == nil || client.Closed() {
		runColdStartUpgrade()
		return
	}

	// Live daemon present: stage & install with --upgrade (symlink untouched until v2 READY).
	installedBin, sfail := StageAndInstallWithRetry(ctx, sshClient, dist, probe, bin, op.ID, m.gatewayID, corralUser, true, m.busyRetry, setStep, isCancelled, m.cfg.VerifySHA256Hook)
	if sfail != nil {
		if sfail.Code == "cancelled" || isCancelled() {
			return
		}
		m.finishUpdateFailKeepPrev(op, ch, prevVer, prevRel, dist.Version, sfail.Error())
		return
	}

	if isCancelled() {
		return
	}

	setStep("handoff")
	if sc != nil {
		sc.mu.Lock()
		sc.updating = true
		client = sc.client
		sc.mu.Unlock()
		defer func() {
			var triggerRecovery bool
			m.mutateWatchState(sc, nil, func() {
				sc.updating = false
				if sc.client == nil || sc.client.Closed() {
					sc.clearWatchStateLocked()
					if !sc.intentionalClose && !sc.reconnecting && !sc.recovering && len(sc.hostRefs) > 0 {
						triggerRecovery = true
					}
				}
			})
			if triggerRecovery {
				m.wg.Add(1)
				go func() {
					defer m.wg.Done()
					m.handleUnexpectedDisconnect(sc)
				}()
			}
		}()
	}

	if client == nil || client.Closed() {
		// Daemon exited between staging and handoff; run cold-start install + serve.
		runColdStartUpgrade()
		return
	}

	// Execute zero-gap daemon.handoff on the live daemon.
	// C-7: Use timeout > daemon's 60s lifecycle lock wait (default 75s).
	// C-6: On busy / handoff_in_progress / closed-conn, wait and reconnect
	// FIRST before retrying so we never keep hitting an exited old daemon, and
	// if another gateway already completed the upgrade, accept it cleanly.
	iv, maxRetries := m.busyRetry.effective()
	var herr error
	alreadyAtTarget := false
	for attempt := 0; ; attempt++ {
		hctx, hcancel := context.WithTimeout(ctx, m.handoffTimeout)
		_, herr = client.Handoff(hctx, proto.HandoffParams{
			Path:    installedBin,
			SHA256:  bin.SHA256,
			Release: dist.Release,
		})
		hcancel()
		if herr == nil {
			break
		}
		var rpcErr *proto.RPCError
		isRPC := errors.As(herr, &rpcErr)
		if isRPC && (rpcErr.Code == proto.ErrHandoffFailed || rpcErr.Code == proto.ErrRollbackFailed) {
			break // explicit handoff/rollback failure: do not retry
		}
		isRetryable := (isRPC && (rpcErr.Code == proto.ErrBusy || rpcErr.Code == proto.ErrHandoffInProgress)) ||
			errors.Is(herr, ErrClientClosed) || errors.Is(herr, io.EOF)
		if isRetryable && attempt < maxRetries {
			if !waitBusyRetry(ctx, iv, isCancelled) {
				break
			}
			// Reconnect before retrying to attach to any newly handed-off daemon.
			if rerr := m.reconnectSharedConn(ctx, sc, sshClient, own); rerr == nil {
				sc.mu.Lock()
				newCl := sc.client
				newH := sc.hello
				sc.mu.Unlock()
				if newH != nil && newH.Release >= dist.Release {
					alreadyAtTarget = true
					herr = nil
					break
				}
				if newCl != nil && !newCl.Closed() {
					client = newCl
				}
			}
			continue
		}
		break
	}
	if herr != nil {
		if isCancelled() {
			return
		}
		// C-7: ONLY explicit handoff_failed / rollback_failed blacklist the release
		// in failed.json. Timeouts, disconnects, and busy errors NEVER blacklist.
		var rpcErr *proto.RPCError
		isExplicitHandoffFail := errors.As(herr, &rpcErr) && (rpcErr.Code == proto.ErrHandoffFailed || rpcErr.Code == proto.ErrRollbackFailed)
		if isExplicitHandoffFail {
			_ = RecordFailedOnHost(ctx, sshClient, probe.Home, probe.UIDInt64(), probe.FailedJSON, dist.Release, herr.Error())
			_ = m.store.RecordCorraldFailedReleases(context.Background(), hostID, fp, sshUser, []int64{dist.Release})
			ch.FailedReleases = mergeReleaseLists(ch.FailedReleases, []int64{dist.Release})
			sc.mu.Lock()
			sc.addFailedReleasesLocked([]int64{dist.Release})
			sc.mu.Unlock()
			m.finishUpdateFailKeepPrev(op, ch, prevVer, prevRel, dist.Version, herr.Error())
			return
		}
		// Non-explicit error (timeout/disconnect/busy): reconnect to inspect actual host version.
		if rerr := m.reconnectSharedConn(ctx, sc, sshClient, own); rerr == nil {
			sc.mu.Lock()
			curH := sc.hello
			sc.mu.Unlock()
			if curH != nil && curH.Release >= dist.Release {
				alreadyAtTarget = true
				herr = nil
			} else if curH != nil {
				prevVer, prevRel = curH.Version, curH.Release
			}
		}
		if herr != nil {
			m.finishUpdateFailKeepPrev(op, ch, prevVer, prevRel, dist.Version, herr.Error())
			return
		}
	}

	// Step: verify — reconnect to the new process and confirm its Release.
	if isCancelled() {
		return
	}
	setStep("verify")
	if !alreadyAtTarget {
		if err := m.reconnectSharedConn(ctx, sc, sshClient, own); err != nil {
			if isCancelled() || errors.Is(err, ErrAdminDisabled) {
				return
			}
			m.finishUpdateFailKeepPrev(op, ch, prevVer, prevRel, dist.Version, err.Error())
			return
		}
	}
	if isCancelled() {
		return
	}

	readyTime := time.Now()
	sc.mu.Lock()
	newHello := sc.hello
	sc.scopeChange = nil
	sc.mu.Unlock()

	actualVer := ""
	actualRel := int64(0)
	if newHello != nil {
		actualVer = newHello.Version
		actualRel = newHello.Release
	}

	// Unified crash-loop rollback window (Fix5 #4): record ONLY the actual
	// running version/release and window start time; rollback target is computed
	// dynamically at rollback time by SelectStartBinary (highest installed,
	// non-failed release strictly below actualRel).
	win := computePostUpdateCrashWindow(readyTime.Unix(), actualVer, actualRel, prevRel)

	sc.mu.Lock()
	if win.ReadyAt > 0 {
		sc.updatedAt = readyTime
	} else {
		sc.updatedAt = time.Time{}
	}
	sc.updateTargetVer = win.TargetVer
	sc.updateTargetRel = win.TargetRel
	sc.updateCrashCount = 0
	sc.prevVersion = ""
	sc.prevRelease = 0
	sc.prevRelTarget = ""
	sc.mu.Unlock()

	ch.State = StateRunning
	if actualVer != "" {
		ch.Version = actualVer
		ch.Release = actualRel
	}
	ch.UpdatedFromVer = ""
	ch.UpdatedFromRel = 0
	ch.UpdatedFromTarget = ""
	ch.UpdateTargetVer = win.TargetVer
	ch.UpdateTargetRel = win.TargetRel
	ch.UpdateReadyAt = win.ReadyAt
	ch.UpdateCrashCount = 0
	ch.LastOKAt = readyTime.Unix()
	ch.LastError = ""
	ch.RestartCount = 0
	if files := ReadHostStateFiles(ctx, sshClient, probe, op.ID); files.Install != nil && len(files.Install.Files) > 0 {
		ch.Files = files.Install.Files
	}
	op.State = OpDone
	op.ErrorCode = ""
	m.policyMu.RLock()
	if m.adminDisabled.Load() {
		m.policyMu.RUnlock()
		op.State = OpCancelled
		_, _ = m.store.UpdateCorraldOpIfActive(context.Background(), op)
		return
	}
	if sc != nil {
		m.mu.Lock()
		stillReg := !m.closed && m.conns[sc.key] == sc
		m.mu.Unlock()
		if !stillReg {
			m.policyMu.RUnlock()
			op.State = OpCancelled
			_, _ = m.store.UpdateCorraldOpIfActive(context.Background(), op)
			return
		}
	}
	finalizedOK, _ := m.store.FinalizeCorraldOpWithHost(context.Background(), op, ch)
	m.policyMu.RUnlock()
	if finalizedOK {
		// Persist post-update crash window across all peers sharing this account (Item 22).
		_ = m.store.UpdateCorraldCrashWindowByBinding(
			context.Background(), hostID, fp, sshUser,
			win.ReadyAt, win.TargetVer, win.TargetRel, "", 0, "", 0,
		)
		notifiedVer := actualVer
		notifiedRel := actualRel
		if notifiedVer == "" {
			notifiedVer = dist.Version
			notifiedRel = dist.Release
		}
		m.recordUpdateNotices(hostID, fp, sshUser, prevVer, notifiedVer, notifiedRel, true)
	}
	if sc != nil {
		m.scheduleWatchReconcile(sc)
		m.notifyCoverageForConn(sc)
	}
}

func (m *Manager) finishUpdateFailKeepPrev(op *store.CorraldOp, ch *store.CorraldHost, prevVer string, prevRel int64, targetVer, detail string) {
	fail := &OpFailure{
		Code:            "update_failed",
		Detail:          detail,
		TargetVersion:   targetVer,
		FallbackVersion: prevVer,
	}
	if prevVer != "" {
		ch.Version = prevVer
		ch.Release = prevRel
	}
	ch.LastError = encodeLastError(fail)
	op.State = OpFailed
	op.ErrorCode = "update_failed"
	op.Detail = encodeOpDetail(opDetailPayload{Message: detail})

	m.policyMu.RLock()
	if m.adminDisabled.Load() {
		m.policyMu.RUnlock()
		op.State = OpCancelled
		op.ErrorCode = ""
		_, _ = m.store.UpdateCorraldOpIfActive(context.Background(), op)
		return
	}
	if ch.BoundFingerprint != "" && ch.BoundUsername != "" {
		key := accountKey{Fingerprint: ch.BoundFingerprint, Username: ch.BoundUsername}
		m.mu.Lock()
		closed := m.closed
		_, hasConn := m.conns[key]
		m.mu.Unlock()
		if closed {
			m.policyMu.RUnlock()
			return
		}
		if hasConn {
			ch.State = StateRunning
		} else {
			ch.State = StateStopped
		}
	} else {
		ch.State = StateRunning
	}
	// Uses active_op_id IN ('', op.ID) so recovery after attachAndHello cleared
	// active_op_id (e.g. proto_mismatch) still writes the final state atomically.
	finalizedOK, _ := m.store.FinalizeCorraldOpErrorWithHost(context.Background(), op, ch)
	m.policyMu.RUnlock()
	if finalizedOK {
		m.recordUpdateNotices(ch.HostID, ch.BoundFingerprint, ch.BoundUsername, prevVer, targetVer, 0, false)
	}
}

func (m *Manager) finishUpdateFailError(op *store.CorraldOp, ch *store.CorraldHost, targetVer, machineID, detail string) {
	fail := &OpFailure{
		Code:          "update_failed",
		Detail:        detail,
		TargetVersion: targetVer,
		MachineID:     machineID,
	}
	ch.State = StateError
	ch.LastError = encodeLastError(fail)
	op.State = OpFailed
	op.ErrorCode = "update_failed"
	op.Detail = encodeOpDetail(opDetailPayload{Message: detail})
	// Fix5 #3: match active_op_id IN ('', op.ID) so error finalization still
	// succeeds even if an earlier step (e.g. proto_mismatch) cleared active_op_id.
	_, _ = m.store.FinalizeCorraldOpErrorWithHost(context.Background(), op, ch)
}

// attachAndHello dials socketPath via direct-streamlocal, performs daemon.hello
// and daemon.resume, creates/updates the sharedConn, and starts health/disconnect
// monitoring. If own is non-nil, ownership of the SSH release callback is
// transferred to sc so the caller will not close the live SSH connection (Item 2).
func (m *Manager) attachAndHello(
	ctx context.Context,
	userID, hostID int64,
	corralUser, fp, sshUser string,
	sshClient *ssh.Client,
	own *sshOwnership,
	probe *HostProbe,
	socketPath string,
	ch *store.CorraldHost,
	opID string,
) error {
	if m.AdminDisabled() {
		// Item #5 / Fix5 #3: If serve just succeeded right before admin disabled,
		// do not abandon the newly started daemon — reuse held op lock when
		// called from install/update (opID != ""), or acquire cleanup op lock
		// on normal connect, and reliably record remove_pending on failure.
		if opID != "" {
			m.handleAdminDisabledWhileHoldingOp(context.Background(), userID, hostID, corralUser, fp, sshUser, sshClient, ch, opID)
		} else if sshClient != nil && ch != nil {
			m.handleAdminDisabledOnConnect(context.Background(), userID, hostID, corralUser, fp, sshUser, sshClient, ch)
		} else if m.store != nil {
			_ = m.store.MarkCorraldHostsRemovePendingIfOp(context.Background(), []int64{hostID}, fp, sshUser, opID)
			m.markPendingCleanup()
		}
		return ErrAdminDisabled
	}
	key := accountKey{Fingerprint: fp, Username: sshUser}

	m.mu.Lock()
	sc := m.conns[key]
	m.mu.Unlock()
	if sc == nil {
		sc = newSharedConn(key)
	}

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
			lastErr := encodeLastError(&OpFailure{Code: proto.ErrProtoMismatch, Detail: rpcErr.Message})
			if opID != "" {
				ch.State = StateIncompatible
				ch.LastError = lastErr
				_, _ = m.store.UpdateCorraldHostIfOp(context.Background(), ch, opID, true)
			} else {
				_, _ = m.store.SetCorraldIncompatibleIfIdle(context.Background(), hostID, "", 0, lastErr)
			}
		}
		return err
	}
	if !proto.ProtocolInRange(hello.Protocol, m.protoMin, m.protoMax) {
		_ = client.Close()
		lastErr := encodeLastError(&OpFailure{Code: proto.ErrProtoMismatch, Detail: fmt.Sprintf("protocol %d outside [%d,%d]", hello.Protocol, m.protoMin, m.protoMax)})
		if opID != "" {
			ch.State = StateIncompatible
			ch.Version = hello.Version
			ch.Release = hello.Release
			ch.LastError = lastErr
			_, _ = m.store.UpdateCorraldHostIfOp(context.Background(), ch, opID, true)
		} else {
			_, _ = m.store.SetCorraldIncompatibleIfIdle(context.Background(), hostID, hello.Version, hello.Release, lastErr)
		}
		return &proto.RPCError{Code: proto.ErrProtoMismatch, Message: "daemon protocol out of gateway range"}
	}

	if m.cfg.AfterAttachHello != nil {
		m.cfg.AfterAttachHello(hostID)
	}
	refList := []*hostRef{{HostID: hostID, OwnerID: userID, CorralUser: corralUser}}
	if m.AdminDisabled() {
		// Item #3: Use a non-cancelled context (ctx may be the cancelled install/update
		// opCtx) so Health/ShutdownConditional and immediate uninstall can execute.
		bgCtx := context.Background()
		exclusive := m.enforceAdminDisableOnClient(bgCtx, client, hello, refList, []int64{hostID}, key, 0, opID)
		_ = client.Close()
		if exclusive {
			m.tryImmediateAdminUninstall(bgCtx, key, refList, []int64{hostID}, sshClient, 0, opID)
		}
		return ErrAdminDisabled
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
		// Epoch changed (e.g. cold restart or hot-update new epoch): full resync cleanly.
		if prevEpoch != "" && m.cfg.OnResync != nil {
			m.cfg.OnResync(fp, sshUser)
		}
		_, _ = client.Resume(ctx, hello.Epoch, hello.Seq)
	}

	health, _ := client.Health(ctx)

	// Publish connection, transfer SSH ownership, register monitorConn in m.wg,
	// and transition DB to running under policyMu.RLock so BOTH concurrent
	// SetAdminDisabled(true) AND Close() (which take policyMu.Lock()) are
	// strictly ordered with respect to initialization, ownership, and wg.Wait().
	m.policyMu.RLock()
	if m.adminDisabled.Load() {
		m.policyMu.RUnlock()
		bgCtx := context.Background()
		exclusive := m.enforceAdminDisableOnClient(bgCtx, client, hello, refList, []int64{hostID}, key, 0, opID)
		_ = client.Close()
		if exclusive {
			m.tryImmediateAdminUninstall(bgCtx, key, refList, []int64{hostID}, sshClient, 0, opID)
		}
		return ErrAdminDisabled
	}

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		m.policyMu.RUnlock()
		_ = client.Close()
		return errors.New("manager closed")
	}
	if existing := m.conns[key]; existing != nil {
		sc = existing
	} else {
		m.conns[key] = sc
	}
	m.hostConn[hostID] = key
	m.mu.Unlock()

	if m.cfg.AfterAttachConnsInsert != nil {
		m.cfg.AfterAttachConnsInsert(hostID)
	}

	// Transfer SSH release ownership to sc now that Dial+Hello succeeded and
	// Close() cannot observe sc until policyMu.RUnlock().
	newRelease := own.take()

	var oldClient *Client
	var oldSSH *ssh.Client
	var oldRelease func()
	sc.mu.Lock()
	oldClient = sc.client
	oldSSH = sc.sshClient
	oldRelease = sc.sshRelease
	if oldClient != client {
		sc.clearWatchStateLocked()
	}
	sc.client = client
	sc.sshClient = sshClient
	if sshClient != oldSSH {
		sc.sshRelease = newRelease
	} else if newRelease != nil && oldRelease == nil {
		sc.sshRelease = newRelease
	}
	sc.socketPath = socketPath
	if probe != nil {
		sc.machineID = probe.MachineID
		sc.homeDir = probe.Home
		sc.uid = probe.UIDInt64()
		sc.libDir = probe.LibDir
		sc.stateDir = probe.StateDir
	}
	sc.hello = hello
	sc.health = health
	if sc.epoch != hello.Epoch {
		sc.epoch = hello.Epoch
		sc.lastSeq = hello.Seq
	} else if hello.Seq > sc.lastSeq {
		sc.lastSeq = hello.Seq
	}
	clear(sc.capFailures)
	clear(sc.capDegraded)
	sc.addFailedReleasesLocked(ch.FailedReleases)
	// Hydrate persisted post-update crash window after gateway restart (Item 22).
	if sc.updatedAt.IsZero() && ch.UpdateReadyAt > 0 && ch.UpdateTargetRel > 0 && ch.UpdateTargetRel == hello.Release {
		sc.updatedAt = time.Unix(ch.UpdateReadyAt, 0)
		sc.updateTargetVer = ch.UpdateTargetVer
		sc.updateTargetRel = ch.UpdateTargetRel
		sc.updateCrashCount = ch.UpdateCrashCount
		sc.prevVersion = ch.UpdatedFromVer
		sc.prevRelease = ch.UpdatedFromRel
		sc.prevRelTarget = ch.UpdatedFromTarget
	}
	sc.intentionalClose = false
	sc.hostRefs[hostID] = &hostRef{HostID: hostID, OwnerID: userID, CorralUser: corralUser}
	sc.mu.Unlock()

	now := time.Now().Unix()
	if opID != "" {
		ch.State = StateRunning
		ch.Version = hello.Version
		ch.Release = hello.Release
		ch.SocketPath = socketPath
		ch.LastOKAt = now
		ch.RestartCount = 0
		if ch.LastError != "" {
			if le := decodeLastError(ch.LastError); le == nil || le.Code != "update_failed" {
				ch.LastError = ""
			}
		}
		_, _ = m.store.UpdateCorraldHostIfOp(context.Background(), ch, opID, true)
	} else {
		mid := ch.MachineID
		if probe != nil && probe.MachineID != "" {
			mid = probe.MachineID
		}
		_, _ = m.store.UpdateCorraldRunningIfIdle(
			context.Background(), hostID, fp, sshUser, m.gatewayID,
			hello.Version, hello.Release, socketPath, mid, ch.Files, now, true, nil,
		)
	}
	// Register monitor goroutine in m.wg BEFORE releasing policyMu.RLock so
	// Close() can never call m.wg.Wait() before this Add(1) completes.
	m.wg.Add(1)
	m.policyMu.RUnlock()

	if oldClient != nil && oldClient != client {
		_ = oldClient.Close()
	}
	if oldRelease != nil && oldSSH != sshClient {
		oldRelease()
	}

	// Start monitor loop for this client generation.
	go func() {
		defer m.wg.Done()
		m.monitorConn(sc, client)
	}()

	m.scheduleWatchReconcile(sc)
	m.notifyCoverageForConn(sc)

	return nil
}

func (m *Manager) handleDaemonEvent(sc *sharedConn, ev proto.Event) {
	sc.mu.Lock()
	if ev.Epoch != "" && ev.Epoch != sc.epoch {
		sc.epoch = ev.Epoch
		sc.lastSeq = ev.Seq
	} else if ev.Seq > sc.lastSeq {
		sc.lastSeq = ev.Seq
	}
	sc.mu.Unlock()

	if m.cfg.OnEvent != nil {
		m.cfg.OnEvent(sc.key.Fingerprint, sc.key.Username, ev)
	}

	switch ev.Event {
	case proto.EventDaemonReconnect:
		if m.AdminDisabled() {
			return
		}
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
				rctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
				err := m.reconnectSharedConn(rctx, sc, nil, nil)
				cancel()
				if err != nil {
					var triggerRecovery bool
					m.mutateWatchState(sc, nil, func() {
						sc.reconnecting = false
						if sc.client == nil || sc.client.Closed() {
							sc.clearWatchStateLocked()
							if !sc.intentionalClose && !sc.updating && !sc.recovering && len(sc.hostRefs) > 0 {
								triggerRecovery = true
							}
						}
					})
					if triggerRecovery {
						m.handleUnexpectedDisconnect(sc)
					}
				}
			}()
		}
	case proto.EventAgentChanged:
		m.handleAgentChangedEvent(sc, ev)
	}
}

func (m *Manager) reconnectSharedConn(ctx context.Context, sc *sharedConn, overrideSSH *ssh.Client, overrideOwn *sshOwnership) error {
	if m.AdminDisabled() {
		return ErrAdminDisabled
	}
	sc.mu.Lock()
	sock := sc.socketPath
	oldClient := sc.client
	prevEpoch := sc.epoch
	prevSeq := sc.lastSeq
	sshClient := overrideSSH
	own := overrideOwn
	if sshClient == nil {
		sshClient = sc.sshClient
	}
	refs := make([]*hostRef, 0, len(sc.hostRefs))
	for _, r := range sc.hostRefs {
		cp := *r
		refs = append(refs, &cp)
	}
	sc.mu.Unlock()

	var acquiredOwn *sshOwnership
	if sshClient == nil && len(refs) > 0 {
		c, rel, err := m.acquireMatchingSSH(ctx, sc.key, refs)
		if err != nil {
			return err
		}
		acquiredOwn = newSSHOwnership(rel)
		defer acquiredOwn.releaseIfOwned()
		sshClient = c
		own = acquiredOwn
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

	// Re-subscribe agent.watch immediately on the new client before swapping
	// sc.client when any browser is actively watching this account (zero-gap
	// handoff / reconnect).
	var newWatchRemote bool
	var newWatchHealthy bool
	var newWatchSig string
	var newWatchDegraded bool
	if isWatchHealthOK(health) {
		var mf *proto.Manifest
		if hello != nil {
			mf = &hello.Manifest
		}
		if wantWatch, extraLogs := m.hasAnyRunningWatchDemand(ctx, sc, mf); wantWatch {
			wctx, wcancel := context.WithTimeout(ctx, m.watchRPCTimeout)
			wres, werr := newClient.Watch(wctx, extraLogs)
			wcancel()
			if werr != nil || wres == nil || !wres.Watching {
				newWatchDegraded = true
				var rpcErr *proto.RPCError
				if werr != nil && !errors.As(werr, &rpcErr) && !newClient.Closed() {
					// Ambiguous timeout/error while connection stays alive:
					// track possible remote subscription so Unwatch still runs.
					newWatchRemote = true
				}
			} else {
				newWatchRemote = true
				newWatchSig = watchLogsSignature(extraLogs)
				if watchResultHealthy(wres) {
					newWatchHealthy = true
				} else {
					newWatchDegraded = true
				}
			}
		}
	}

	// Re-check policy under policyMu.RLock before publishing the reconnected
	// client so a concurrent SetAdminDisabled(true) cannot be bypassed.
	m.policyMu.RLock()
	if m.adminDisabled.Load() {
		m.policyMu.RUnlock()
		hostIDs := make([]int64, 0, len(refs))
		for _, r := range refs {
			hostIDs = append(hostIDs, r.HostID)
		}
		bgCtx := context.Background()
		exclusive := m.enforceAdminDisableOnClient(bgCtx, newClient, hello, refs, hostIDs, sc.key, 0, "")
		_ = newClient.Close()
		if exclusive {
			m.tryImmediateAdminUninstall(bgCtx, sc.key, refs, hostIDs, sshClient, 0, "")
		}
		return ErrAdminDisabled
	}
	m.mu.Lock()
	stillRegistered := !m.closed && m.conns[sc.key] == sc
	m.mu.Unlock()
	if !stillRegistered {
		m.policyMu.RUnlock()
		_ = newClient.Close()
		return ErrNotConnected
	}

	// Transfer SSH ownership to sc so caller does not close the live SSH conn (Item 2).
	newRelease := own.take()

	var oldSSH *ssh.Client
	var oldRelease func()
	sc.mu.Lock()
	oldSSH = sc.sshClient
	oldRelease = sc.sshRelease
	sc.clearWatchStateLocked()
	sc.reconnecting = false
	sc.client = newClient
	if sshClient != oldSSH {
		sc.sshClient = sshClient
		sc.sshRelease = newRelease
	} else if newRelease != nil && oldRelease == nil {
		sc.sshRelease = newRelease
	}
	sc.hello = hello
	sc.health = health
	if sc.epoch != hello.Epoch {
		sc.epoch = hello.Epoch
		sc.lastSeq = hello.Seq
	} else if hello.Seq > sc.lastSeq {
		sc.lastSeq = hello.Seq
	}
	clear(sc.capFailures)
	clear(sc.capDegraded)
	if newWatchDegraded {
		sc.capDegraded["watch"] = true
	}
	if newWatchRemote {
		sc.watchRemoteActive = true
		sc.watchHealthy = newWatchHealthy
		sc.watchClient = newClient
		sc.watchLogsSig = newWatchSig
	}
	sc.mu.Unlock()
	m.wg.Add(1)
	m.policyMu.RUnlock()

	if oldClient != nil && oldClient != newClient {
		_ = oldClient.Close()
	}
	if oldRelease != nil && oldSSH != sshClient {
		oldRelease()
	}

	go func() {
		defer m.wg.Done()
		m.monitorConn(sc, newClient)
	}()
	m.scheduleWatchReconcile(sc)
	m.notifyCoverageForConn(sc)
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
			var isCurrent, intentional bool
			m.mutateWatchState(sc, nil, func() {
				isCurrent = sc.client == client
				intentional = sc.intentionalClose || sc.updating || sc.reconnecting
				if sc.watchClient == client {
					sc.clearWatchStateLocked()
				}
			})
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
			var prevWatchOK, nowWatchOK bool
			var refs []int64
			m.mutateWatchState(sc, nil, func() {
				prevWatchOK = isWatchHealthOK(sc.health)
				sc.health = health
				nowWatchOK = isWatchHealthOK(health)
				refs = make([]int64, 0, len(sc.hostRefs))
				for id := range sc.hostRefs {
					refs = append(refs, id)
				}
			})
			for _, hostID := range refs {
				_ = m.store.TouchCorraldLastOK(context.Background(), hostID, now)
			}
			if !prevWatchOK && nowWatchOK {
				m.scheduleWatchReconcile(sc)
			}
		}
	}
}

func (m *Manager) handleUnexpectedDisconnect(sc *sharedConn) {
	if m.AdminDisabled() {
		return
	}
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
	homeDir := sc.homeDir
	uid := sc.uid
	sockPath := sc.socketPath
	machineID := sc.machineID

	// Check 30-minute post-update crash loop window (spec §6 item 7, persisted via Item 22).
	triggerRollback := false
	newCrashCount := sc.updateCrashCount
	if !updatedAt.IsZero() && updateTargetRel > 0 {
		sc.updateCrashCount++
		newCrashCount = sc.updateCrashCount
		if ShouldRollbackOnCrash(sc.updateCrashCount, time.Since(updatedAt), m.crashWindow, m.crashThreshold) {
			triggerRollback = true
		}
	}
	sc.mu.Unlock()

	recovered := false
	defer func() {
		var retryDrop bool
		sc.mu.Lock()
		sc.recovering = false
		if recovered && !sc.intentionalClose && !sc.updating && !sc.reconnecting && len(sc.hostRefs) > 0 && sc.client != nil && sc.client.Closed() {
			retryDrop = true
		}
		sc.mu.Unlock()
		if retryDrop {
			m.wg.Add(1)
			go func() {
				defer m.wg.Done()
				m.handleUnexpectedDisconnect(sc)
			}()
		}
	}()

	if len(refs) == 0 {
		return
	}

	// Persist incremented crash count so gateway restarts preserve the rollback window (Item 22).
	if !updatedAt.IsZero() && updateTargetRel > 0 {
		_ = m.store.UpdateCorraldCrashWindowByBinding(
			context.Background(), refs[0].HostID, sc.key.Fingerprint, sc.key.Username,
			updatedAt.Unix(), updateTargetVer, updateTargetRel, prevVer, prevRel, prevRelTarget, newCrashCount,
		)
	}

	// Transition all attached hosts to StateLost via conditional SQL.
	for _, ref := range refs {
		_, _ = m.store.SetCorraldStateIfIdle(context.Background(), ref.HostID, StateLost, StateRunning)
	}
	m.notifyCoverageForConn(sc)

	if triggerRollback {
		if m.attemptPostUpdateRollback(sc, refs, updateTargetVer, updateTargetRel, prevVer, prevRel, prevRelTarget, homeDir, uid, libDir, stateDir, sockPath, machineID) {
			return
		}
	}

	// Attempt automatic restart up to MaxAutoRestarts (3) with increasing backoff.
	for attempt := 0; attempt < MaxAutoRestarts; attempt++ {
		if m.AdminDisabled() {
			return
		}
		wait := m.restartBackoffs[min(attempt, len(m.restartBackoffs)-1)]
		select {
		case <-sc.stopCh:
			return
		case <-time.After(wait):
		}
		if m.AdminDisabled() {
			return
		}

		if m.tryRestartSharedConn(sc, refs) {
			if m.AdminDisabled() {
				return
			}
			recovered = true
			sc.mu.Lock()
			var restartVer string
			var restartRel int64
			if sc.hello != nil {
				restartVer = sc.hello.Version
				restartRel = sc.hello.Release
			}
			sc.mu.Unlock()
			m.policyMu.RLock()
			if m.adminDisabled.Load() {
				m.policyMu.RUnlock()
				return
			}
			m.mu.Lock()
			stillReg := !m.closed && m.conns[sc.key] == sc
			m.mu.Unlock()
			if !stillReg {
				m.policyMu.RUnlock()
				return
			}
			now := time.Now().Unix()
			for _, ref := range refs {
				_, _ = m.store.UpdateCorraldRunningIfIdle(
					context.Background(), ref.HostID, sc.key.Fingerprint, sc.key.Username, m.gatewayID,
					restartVer, restartRel, "", "", nil, now, false, nil,
				)
			}
			m.policyMu.RUnlock()
			m.scheduleWatchReconcile(sc)
			m.notifyCoverageForConn(sc)
			return
		}
		if m.AdminDisabled() {
			return
		}
		for _, ref := range refs {
			if ch, err := m.store.CorraldHostByID(context.Background(), ref.HostID); err == nil {
				tr := Transition(toSnapshot(ch), Event{Kind: EvRestartAttempt, ErrorCode: "start_failed"})
				if tr.Allowed {
					lastErr := ""
					if tr.Next.State == StateError {
						lastErr = encodeLastError(&OpFailure{Code: "start_failed", MachineID: machineID})
					}
					_, _ = m.store.RecordCorraldRestartAttemptIfLost(context.Background(), ref.HostID, tr.Next.State, tr.Next.RestartCount, lastErr)
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
	prevRelTarget, homeDir string,
	uid int64,
	libDir, stateDir, sockPath, machineID string,
) bool {
	rctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	sshClient, relFn, err := m.acquireMatchingSSH(rctx, sc.key, refs)
	if err != nil {
		return false
	}
	own := newSSHOwnership(relFn)
	defer own.releaseIfOwned()

	firstHostID := int64(0)
	if len(refs) > 0 {
		firstHostID = refs[0].HostID
	}

	// Record the broken release in failed.json on the host and in gateway DB/cache.
	if stateDir != "" && badRel > 0 {
		_ = RecordFailedOnHost(rctx, sshClient, homeDir, uid, stateDir+"/failed.json", badRel, "crash_loop_after_update")
	}
	if badRel > 0 && firstHostID > 0 {
		_ = m.store.RecordCorraldFailedReleases(context.Background(), firstHostID, sc.key.Fingerprint, sc.key.Username, []int64{badRel})
		sc.mu.Lock()
		sc.addFailedReleasesLocked([]int64{badRel})
		sc.mu.Unlock()
	}

	// Clear update window in memory & DB so we don't loop rollback.
	sc.mu.Lock()
	sc.updatedAt = time.Time{}
	sc.updateTargetVer = ""
	sc.updateTargetRel = 0
	sc.updateCrashCount = 0
	sc.prevVersion = ""
	sc.prevRelease = 0
	sc.prevRelTarget = ""
	sc.mu.Unlock()
	if firstHostID > 0 {
		_ = m.store.UpdateCorraldCrashWindowByBinding(
			context.Background(), firstHostID, sc.key.Fingerprint, sc.key.Username,
			0, "", 0, "", 0, "", 0,
		)
	}

	// Fix5 #4: Dynamically compute rollback binary at rollback time via
	// SelectStartBinary: highest installed, non-failed release strictly below
	// the crashed actual release (badRel). Never lowers the shared symlink.
	freshProbe, rbCand, _, selErr := m.resolveStartBinary(rctx, sshClient, nil, firstHostID, sc.key.Fingerprint, sc.key.Username, nil, sc, StartSelectOptions{
		Rollback:     true,
		BelowRelease: badRel,
	})
	if selErr != nil || rbCand.Path == "" {
		return false
	}
	if freshProbe != nil {
		if freshProbe.MachineID != "" {
			machineID = freshProbe.MachineID
		}
		if freshProbe.LibDir != "" {
			libDir = freshProbe.LibDir
		}
	}

	if m.AdminDisabled() {
		return false
	}
	_, _ = runSSH(rctx, sshClient, StopCommand(rbCand.Path), 4096, sshx.ExecOptions{})
	if m.AdminDisabled() {
		return false
	}
	_, startedSock, sfail := RunServeWithRetry(rctx, sshClient, rbCand.Path, m.gatewayID, machineID, "Linux", "", m.busyRetry)
	if sfail != nil {
		return false
	}
	if m.AdminDisabled() {
		return false
	}
	if startedSock != "" {
		sc.mu.Lock()
		sc.socketPath = startedSock
		sc.mu.Unlock()
	}
	if err := m.reconnectSharedConn(rctx, sc, sshClient, own); err != nil {
		return false
	}

	// Use the actual running daemon's hello version/release for DB state & banner.
	actualVer := rbCand.Version
	actualRel := rbCand.Release
	sc.mu.Lock()
	if sc.hello != nil {
		if sc.hello.Version != "" {
			actualVer = sc.hello.Version
		}
		if sc.hello.Release > 0 {
			actualRel = sc.hello.Release
		}
	}
	sc.mu.Unlock()
	if actualVer == "" {
		actualVer = prevVer
	}
	if actualRel == 0 {
		actualRel = prevRel
	}

	failBanner := encodeLastError(&OpFailure{
		Code:            "update_failed",
		TargetVersion:   badVer,
		FallbackVersion: actualVer,
		MachineID:       machineID,
	})
	m.policyMu.RLock()
	if m.adminDisabled.Load() {
		m.policyMu.RUnlock()
		return false
	}
	m.mu.Lock()
	stillReg := !m.closed && m.conns[sc.key] == sc
	m.mu.Unlock()
	if !stillReg {
		m.policyMu.RUnlock()
		return false
	}
	now := time.Now().Unix()
	for _, ref := range refs {
		_, _ = m.store.UpdateCorraldRunningIfIdle(
			context.Background(), ref.HostID, sc.key.Fingerprint, sc.key.Username, m.gatewayID,
			actualVer, actualRel, "", machineID, nil, now, false, &failBanner,
		)
	}
	m.policyMu.RUnlock()
	if firstHostID > 0 {
		m.recordUpdateNotices(firstHostID, sc.key.Fingerprint, sc.key.Username, actualVer, badVer, badRel, false)
	}
	return true
}

func (m *Manager) tryRestartSharedConn(sc *sharedConn, refs []*hostRef) bool {
	if m.AdminDisabled() {
		return false
	}
	rctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	sshClient, relFn, err := m.acquireMatchingSSH(rctx, sc.key, refs)
	if err != nil {
		return false
	}
	own := newSSHOwnership(relFn)
	defer own.releaseIfOwned()

	if m.AdminDisabled() {
		return false
	}
	// Try direct reconnect first (e.g. transient SSH blip while daemon kept running).
	if err := m.reconnectSharedConn(rctx, sc, sshClient, own); err == nil {
		return true
	} else if errors.Is(err, ErrAdminDisabled) {
		return false
	}

	firstHostID := int64(0)
	if len(refs) > 0 {
		firstHostID = refs[0].HostID
	}

	// B-1: Always probe host + read failed.json + choose binary via the single
	// unified SelectStartBinary entrypoint so auto-restart NEVER relaunches a
	// blacklisted release via the shared symlink.
	probe, cand, _, selErr := m.resolveStartBinary(rctx, sshClient, nil, firstHostID, sc.key.Fingerprint, sc.key.Username, nil, sc, StartSelectOptions{})
	if selErr != nil || probe == nil || probe.HasTombstone || cand.Path == "" {
		return false
	}
	sc.mu.Lock()
	sc.libDir = probe.LibDir
	sc.stateDir = probe.StateDir
	sc.homeDir = probe.Home
	sc.uid = probe.UIDInt64()
	sc.machineID = probe.MachineID
	sc.mu.Unlock()

	if m.AdminDisabled() {
		return false
	}
	_, sock, fail := RunServeWithRetry(rctx, sshClient, cand.Path, m.gatewayID, probe.MachineID, probe.OS, probe.Arch, m.busyRetry)
	if fail != nil {
		return false
	}
	if m.AdminDisabled() {
		if len(refs) > 0 && m.store != nil {
			if ch, cerr := m.store.CorraldHostByID(rctx, refs[0].HostID); cerr == nil {
				m.handleAdminDisabledOnConnect(rctx, refs[0].OwnerID, refs[0].HostID, refs[0].CorralUser, sc.key.Fingerprint, sc.key.Username, sshClient, ch)
			}
		}
		return false
	}
	if sock != "" {
		sc.mu.Lock()
		sc.socketPath = sock
		sc.mu.Unlock()
	}
	return m.reconnectSharedConn(rctx, sc, sshClient, own) == nil
}

func (m *Manager) evaluateUpdateAfterConnect(
	ctx context.Context,
	userID, hostID int64,
	corralUser, fp, sshUser string,
	probe *HostProbe,
	failedReleases []int64,
) {
	dist := m.Dist()
	if dist == nil || m.AdminDisabled() {
		return
	}
	sc := m.sharedConnForHost(hostID, fp, sshUser)
	if sc == nil {
		return
	}
	sc.mu.Lock()
	hello := sc.hello
	alreadyUpdating := sc.updating
	scFailed := slices.Clone(sc.failedReleases)
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
	// If the currently running daemon ALREADY grew beyond consented scope (Item 6),
	// surface scope_change and do NOT auto-update until the user consents.
	if runningAdded := proto.ScopeGrew(consented, hello.Manifest); len(runningAdded) > 0 {
		sc.mu.Lock()
		sc.scopeChange = &ScopeChangeInfo{
			TargetVersion: hello.Version,
			Added:         runningAdded,
		}
		sc.mu.Unlock()
		return
	}

	allFailed := mergeReleaseLists(failedReleases, scFailed, ch.FailedReleases)
	dec := DecideUpdate(hello.Release, hello.Protocol, consented, dist.Release, dist.Manifest, allFailed, m.protoMin, m.protoMax)
	switch dec.Action {
	case UpdateIncompatible:
		_, _ = m.store.SetCorraldIncompatibleIfIdle(ctx, hostID, hello.Version, hello.Release, "")
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
		_, _ = m.launchUpdateOp(ctx, userID, hostID, corralUser, fp, sshUser, ch, dist, "", false)
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
	if files.Failed != nil {
		_ = m.store.RecordCorraldFailedReleases(ctx, hostID, fp, sshUser, files.Failed.ReleaseList())
		ch.FailedReleases = mergeReleaseLists(ch.FailedReleases, files.Failed.ReleaseList())
	}

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

	writeReconciled := func(opToFinalize *store.CorraldOp) {
		_, _ = m.store.UpdateCorraldHostForReconcile(ctx, ch, opToFinalize, opID)
	}

	switch dec.Action {
	case ReconcileFinishRemove:
		out := RunUninstallWithRetry(ctx, sshClient, probe, opID, m.busyRetry, nil)
		switch out.Result {
		case "removed":
			_ = m.store.CorraldResetByBinding(ctx, fp, sshUser)
			ch.State = StateOff
			ch.Version = ""
			ch.Release = 0
			ch.Files = []string{}
			ch.ActiveOpID = ""
			ch.LastError = ""
			if activeOp != nil {
				activeOp.State = OpDone
				activeOp.Detail = encodeOpDetail(opDetailPayload{Result: "removed"})
			}
			writeReconciled(activeOp)
		case "partial":
			ch.State = StateError
			ch.ActiveOpID = ""
			ch.LastError = encodeLastError(&OpFailure{Code: "partial", Left: out.Left})
			if activeOp != nil {
				activeOp.State = OpFailed
				activeOp.ErrorCode = "partial"
				activeOp.Detail = encodeOpDetail(opDetailPayload{Result: "partial", Left: out.Left})
			}
			writeReconciled(activeOp)
		case "stop_failed":
			ch.State = StateError
			ch.ActiveOpID = ""
			ch.LastError = encodeLastError(&OpFailure{Code: "stop_failed", PID: out.PID, Detail: out.Detail})
			if activeOp != nil {
				activeOp.State = OpFailed
				activeOp.ErrorCode = "stop_failed"
				activeOp.Detail = encodeOpDetail(opDetailPayload{Result: "stop_failed", PID: out.PID, Message: out.Detail})
			}
			writeReconciled(activeOp)
		default:
			ch.State = StateRemovePending
			ch.ActiveOpID = ""
			writeReconciled(nil)
		}

	case ReconcileMarkOff:
		ch.State = StateOff
		ch.Version = ""
		ch.Release = 0
		ch.Files = []string{}
		ch.ActiveOpID = ""
		var opFin *store.CorraldOp
		if activeOp != nil && dec.OpFinalState != "" {
			activeOp.State = dec.OpFinalState
			activeOp.ErrorCode = dec.OpErrorCode
			opFin = activeOp
		}
		writeReconciled(opFin)

	case ReconcileMarkOpFailed:
		ch.State = dec.NextState
		ch.ActiveOpID = ""
		if dec.OpErrorCode != "" {
			ch.LastError = encodeLastError(&OpFailure{Code: dec.OpErrorCode})
		}
		if activeOp != nil {
			activeOp.State = dec.OpFinalState
			activeOp.ErrorCode = dec.OpErrorCode
		}
		writeReconciled(activeOp)

	case ReconcileConnect, ReconcileStartDaemon:
		if files.Install != nil {
			ch.Version = files.Install.Version
			ch.Release = files.Install.Release
			ch.Files = files.Install.Files
		}
		if ch.BoundFingerprint == "" && fp != "" {
			ch.BoundFingerprint = fp
		}
		if ch.BoundUsername == "" && sshUser != "" {
			ch.BoundUsername = sshUser
		}
		if ch.BoundGateway == "" {
			ch.BoundGateway = m.gatewayID
		}
		ch.State = dec.NextState
		ch.ActiveOpID = ""
		ch.MachineID = probe.MachineID
		var opFin *store.CorraldOp
		if activeOp != nil && dec.OpFinalState != "" {
			activeOp.State = dec.OpFinalState
			activeOp.ErrorCode = dec.OpErrorCode
			opFin = activeOp
		}
		writeReconciled(opFin)
	}
}

// --- Helpers ---

type postUpdateCrashWindow struct {
	ReadyAt   int64
	TargetVer string
	TargetRel int64
}

// computePostUpdateCrashWindow records ONLY the actual running daemon's
// version/release and window start timestamp when an upgrade succeeded
// (actualRel > prevRel). The rollback target is NOT pre-stored here — it is
// computed dynamically at rollback time by SelectStartBinary (highest
// installed, non-failed release strictly below TargetRel).
func computePostUpdateCrashWindow(readyAt int64, actualVer string, actualRel, prevRel int64) postUpdateCrashWindow {
	if actualRel <= 0 || actualRel <= prevRel {
		return postUpdateCrashWindow{}
	}
	return postUpdateCrashWindow{
		ReadyAt:   readyAt,
		TargetVer: actualVer,
		TargetRel: actualRel,
	}
}

// markRebindIfMismatch is the single gateway entrypoint for marking a host as
// StateRebind upon observing an identity mismatch. It uses an atomic CAS SQL
// update verifying both that the row's current binding still equals the
// caller's expected binding AND that it mismatches the observed identity, so
// neither concurrent reinstalls nor stale callbacks with an outdated observed
// identity can clobber a newer binding. Detaches only when the SQL update took effect.
func (m *Manager) markRebindIfMismatch(
	ctx context.Context,
	hostID int64,
	expectedBoundFP, expectedBoundUser, expectedBoundGW string,
	observedFP, observedUser string,
	includeInstalling bool,
	source string,
) bool {
	if m.cfg.BeforeRebindMark != nil {
		m.cfg.BeforeRebindMark(hostID, source)
	}
	changed, _ := m.store.MarkCorraldRebindIfMismatch(
		ctx, hostID,
		expectedBoundFP, expectedBoundUser, expectedBoundGW,
		observedFP, observedUser, m.gatewayID,
		includeInstalling,
	)
	if changed {
		m.detachHost(hostID)
	}
	return changed
}

// resolveStartBinary is the single gateway entrypoint for choosing which
// corrald binary to start on a host (first start, OnHostConnected, Retry,
// auto-restart, cold-start update, rollback). It always reads the host's
// actual files (probing fresh when probe == nil) and failed.json, unions them
// with gateway DB and sharedConn failed releases, and calls SelectStartBinary.
func (m *Manager) resolveStartBinary(
	ctx context.Context,
	sshClient *ssh.Client,
	probe *HostProbe,
	hostID int64,
	fp, sshUser string,
	ch *store.CorraldHost,
	sc *sharedConn,
	opts StartSelectOptions,
) (*HostProbe, StartCandidate, []int64, error) {
	if probe == nil {
		var err error
		probe, err = m.probeHost(ctx, sshClient)
		if err != nil {
			return nil, StartCandidate{}, nil, err
		}
	}
	if probe.HasTombstone {
		return probe, StartCandidate{}, nil, errors.New(proto.ErrRemoving)
	}
	files := ReadHostStateFiles(ctx, sshClient, probe, "")
	var hostFailed []int64
	if files.Failed != nil {
		hostFailed = files.Failed.ReleaseList()
		if len(hostFailed) > 0 && hostID > 0 {
			_ = m.store.RecordCorraldFailedReleases(context.Background(), hostID, fp, sshUser, hostFailed)
		}
	}
	var dbFailed []int64
	if ch != nil {
		dbFailed = ch.FailedReleases
	} else if hostID > 0 {
		if fresh, err := m.store.CorraldHostByID(context.Background(), hostID); err == nil {
			dbFailed = fresh.FailedReleases
		}
	}
	var memFailed []int64
	if sc != nil {
		sc.mu.Lock()
		if len(hostFailed) > 0 {
			sc.addFailedReleasesLocked(hostFailed)
		}
		memFailed = slices.Clone(sc.failedReleases)
		sc.mu.Unlock()
	}
	allFailed := mergeReleaseLists(hostFailed, dbFailed, memFailed)
	if ch != nil {
		ch.FailedReleases = allFailed
	}
	cand, ok := SelectStartBinary(probe, allFailed, opts)
	if !ok || cand.Path == "" {
		return probe, StartCandidate{}, allFailed, errors.New("no eligible non-failed corrald binary available")
	}
	return probe, cand, allFailed, nil
}

func (m *Manager) probeHost(ctx context.Context, c *ssh.Client) (*HostProbe, error) {
	if m.cfg.ProbeOverride != nil {
		return m.cfg.ProbeOverride(ctx, c)
	}
	return ProbeHost(ctx, c)
}

// acquireMatchingSSH acquires an SSH connection for one of refs, strictly
// verifying that the returned (fingerprint, sshUser) matches wantKey (Item 1).
// Any host ref whose identity changed is conditionally marked StateRebind via
// MarkCorraldRebindIfMismatch and detached from wantKey rather than attached
// back into the original shared group.
func (m *Manager) acquireMatchingSSH(ctx context.Context, wantKey accountKey, refs []*hostRef) (*ssh.Client, func(), error) {
	var lastErr error
	for _, ref := range refs {
		c, rel, fp, sshUser, err := m.ssh.AcquireSSH(ctx, ref.OwnerID, ref.HostID)
		if err != nil || c == nil {
			lastErr = err
			continue
		}
		if fp != wantKey.Fingerprint || sshUser != wantKey.Username {
			// Host address/key/username changed: never attach to the old shared group!
			if rel != nil {
				rel()
			}
			// Atomic CAS SQL: only marks rebind if the row is still bound to
			// wantKey (never clobbers a host already rebound to a newer identity B)
			// and is not installing/terminal.
			if m.cfg.BeforeRebindMark != nil {
				m.cfg.BeforeRebindMark(ref.HostID, "acquire_matching_ssh")
			}
			changed, _ := m.store.MarkCorraldRebindIfMismatch(
				context.Background(), ref.HostID,
				wantKey.Fingerprint, wantKey.Username, m.gatewayID,
				fp, sshUser, m.gatewayID,
				false,
			)
			if changed {
				m.detachHost(ref.HostID)
			} else {
				m.detachHostFromKey(ref.HostID, wantKey)
			}
			lastErr = ErrInvalidState
			continue
		}
		return c, rel, nil
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
				HostID:            hostID,
				State:             src.State,
				ConsentBy:         src.ConsentBy,
				ConsentAt:         src.ConsentAt,
				BoundFingerprint:  src.BoundFingerprint,
				BoundUsername:     src.BoundUsername,
				BoundGateway:      src.BoundGateway,
				ManifestJSON:      src.ManifestJSON,
				Version:           src.Version,
				Release:           src.Release,
				MachineID:         src.MachineID,
				SocketPath:        src.SocketPath,
				Files:             slices.Clone(src.Files),
				PausedUsers:       []int64{},
				FailedReleases:    slices.Clone(src.FailedReleases),
				UpdatedFromVer:    src.UpdatedFromVer,
				UpdatedFromRel:    src.UpdatedFromRel,
				UpdatedFromTarget: src.UpdatedFromTarget,
				UpdateTargetVer:   src.UpdateTargetVer,
				UpdateTargetRel:   src.UpdateTargetRel,
				UpdateReadyAt:     src.UpdateReadyAt,
				UpdateCrashCount:  src.UpdateCrashCount,
				LastOKAt:          src.LastOKAt,
			}
			if inserted, err := m.store.InsertCorraldHostIfAbsent(ctx, adopted); err == nil {
				if inserted {
					return adopted
				}
				if existing, gerr := m.store.CorraldHostByID(ctx, hostID); gerr == nil {
					return existing
				}
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

// hasOtherPeersIncludingPaused reports whether any other Corral user on this
// gateway has a non-off corrald record for (fp, sshUser), INCLUDING paused
// users (Item 16).
func (m *Manager) hasOtherPeersIncludingPaused(ctx context.Context, selfUserID int64, fp, sshUser string) bool {
	peers, err := m.store.CorraldPeersByBinding(ctx, fp, sshUser)
	if err != nil {
		return true // fail-safe: preserve helper on DB error
	}
	for _, p := range peers {
		if p.OwnerID == selfUserID {
			continue
		}
		if p.State != StateOff && p.State != StateRemoving && p.State != StateRemovePending {
			return true
		}
	}
	return false
}

func (m *Manager) registerRunningOp(rop *runningOp) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return errors.New("manager closed")
	}
	if _, exists := m.opsByHost[rop.hostID]; exists {
		return store.ErrOpInProgress
	}
	if rop.acct.valid() {
		if _, exists := m.opsByAcct[rop.acct]; exists {
			return store.ErrOpInProgress
		}
		m.opsByAcct[rop.acct] = rop
	}
	m.opsByHost[rop.hostID] = rop
	return nil
}

func (m *Manager) attachOpAccount(rop *runningOp, acct accountKey) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if prev, exists := m.opsByAcct[acct]; exists && prev != rop {
		return store.ErrOpInProgress
	}
	rop.acct = acct
	m.opsByAcct[acct] = rop
	return nil
}

func (m *Manager) clearRunningOp(rop *runningOp) {
	m.mu.Lock()
	if m.opsByHost[rop.hostID] == rop {
		delete(m.opsByHost, rop.hostID)
	}
	if rop.acct.valid() && m.opsByAcct[rop.acct] == rop {
		delete(m.opsByAcct, rop.acct)
	}
	m.mu.Unlock()
}

func (m *Manager) hasRunningOp(hostID int64, acct accountKey) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.opsByHost[hostID]; ok {
		return true
	}
	if acct.valid() {
		if _, ok := m.opsByAcct[acct]; ok {
			return true
		}
	}
	return false
}

// cancelAndWaitOps cancels all in-memory running ops matching hostID or acct
// (optionally filtered by kinds) and blocks until every cancelled op's
// goroutine has exited (Items 4 & 5).
func (m *Manager) cancelAndWaitOps(ctx context.Context, hostID int64, acct accountKey, kinds ...string) {
	m.mu.Lock()
	var toWait []*runningOp
	add := func(rop *runningOp) {
		if rop == nil {
			return
		}
		if len(kinds) > 0 && !slices.Contains(kinds, rop.kind) {
			return
		}
		for _, existing := range toWait {
			if existing == rop {
				return
			}
		}
		rop.cancelled = true
		if rop.cancel != nil {
			rop.cancel()
		}
		toWait = append(toWait, rop)
	}
	add(m.opsByHost[hostID])
	if acct.valid() {
		add(m.opsByAcct[acct])
	}
	m.mu.Unlock()

	for _, rop := range toWait {
		select {
		case <-rop.done:
		case <-ctx.Done():
			return
		case <-time.After(15 * time.Second):
		}
	}
}

func (m *Manager) getSharedConn(key accountKey) *sharedConn {
	if !key.valid() {
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
	delete(m.hostExtraLogs, hostID)
	var sc *sharedConn
	if ok {
		sc = m.conns[key]
	}
	m.mu.Unlock()
	if sc != nil {
		var empty bool
		m.mutateWatchState(sc, []int64{hostID}, func() {
			delete(sc.hostRefs, hostID)
			empty = len(sc.hostRefs) == 0
		})
		if empty {
			m.closeSharedConn(sc, true)
		} else {
			m.scheduleWatchReconcile(sc)
		}
	} else {
		m.notifyCoverageChanged([]int64{hostID})
	}
}

func (m *Manager) detachHostFromKey(hostID int64, wantKey accountKey) {
	m.mu.Lock()
	if curKey, ok := m.hostConn[hostID]; ok && curKey == wantKey {
		delete(m.hostConn, hostID)
	}
	delete(m.hostExtraLogs, hostID)
	sc := m.conns[wantKey]
	m.mu.Unlock()
	if sc != nil {
		var empty bool
		m.mutateWatchState(sc, []int64{hostID}, func() {
			delete(sc.hostRefs, hostID)
			empty = len(sc.hostRefs) == 0
		})
		if empty {
			m.closeSharedConn(sc, true)
		} else {
			m.scheduleWatchReconcile(sc)
		}
	} else {
		m.notifyCoverageChanged([]int64{hostID})
	}
}

func (m *Manager) closeSharedConn(sc *sharedConn, removeFromMap bool) {
	var client *Client
	var rel func()
	m.mutateWatchState(sc, nil, func() {
		sc.intentionalClose = true
		sc.clearWatchStateLocked()
		client = sc.client
		sc.client = nil
		rel = sc.sshRelease
		sc.sshRelease = nil
		sc.sshClient = nil
	})

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

func (m *Manager) isOpCancelled(rop *runningOp) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return rop.cancelled
}

func (m *Manager) finishOpCancelled(op *store.CorraldOp, ch *store.CorraldHost) {
	op.State = OpCancelled
	op.ErrorCode = ""
	ch.State = StateOff
	ch.LastError = ""
	// Atomic single-transaction finalization prevents leaving a dangling active_op_id.
	_, _ = m.store.FinalizeCorraldOpWithHost(context.Background(), op, ch)
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
	_, _ = m.store.FinalizeCorraldOpWithHost(context.Background(), op, ch)
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

func mergeReleaseLists(lists ...[]int64) []int64 {
	var out []int64
	for _, l := range lists {
		for _, r := range l {
			if r > 0 && !slices.Contains(out, r) {
				out = append(out, r)
			}
		}
	}
	return out
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
		for _, op := range m.opsByHost {
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
	var nowWatchOK bool
	m.mutateWatchState(sc, nil, func() {
		if sc.health == nil {
			sc.health = &proto.HealthResult{}
		}
		sc.health.Caps = caps
		nowWatchOK = isWatchHealthOK(sc.health)
	})
	if nowWatchOK {
		m.scheduleWatchReconcile(sc)
	}
}

// SetRunningManifestForTest overrides the cached hello manifest on the live
// sharedConn for testing scope-change enforcement against running daemons.
func (m *Manager) SetRunningManifestForTest(hostID int64, manifest proto.Manifest) {
	sc := m.sharedConnForHost(hostID, "", "")
	if sc == nil {
		return
	}
	sc.mu.Lock()
	if sc.hello != nil {
		cp := *sc.hello
		cp.Manifest = manifest
		sc.hello = &cp
	}
	sc.mu.Unlock()
}
