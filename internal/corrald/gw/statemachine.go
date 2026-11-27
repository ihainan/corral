package gw

import (
	"slices"
	"strings"
	"time"

	"github.com/ihainan/corral/internal/corrald/proto"
)

// Host states per spec §7.
const (
	StateOff           = "off"
	StateInstalling    = "installing"
	StateRunning       = "running"
	StateStopped       = "stopped"
	StateUpdating      = "updating"
	StatePaused        = "paused"
	StateLost          = "lost"
	StateError         = "error"
	StateRemoving      = "removing"
	StateRemovePending = "remove_pending"
	StateRebind        = "rebind"
	StateIncompatible  = "incompatible"
)

// Operation kinds per spec §7.
const (
	OpInstall = "install"
	OpUpdate  = "update"
	OpPause   = "pause"
	OpResume  = "resume"
	OpRemove  = "remove"
)

// Operation states per spec §7.
const (
	OpPending   = "pending"
	OpRunning   = "running"
	OpDone      = "done"
	OpFailed    = "failed"
	OpCancelled = "cancelled"
	OpUnknown   = "unknown"
)

// Canonical operation step sequences matching web/src/corraldLogic.ts.
var (
	InstallSteps = []string{"check", "upload", "verify", "install", "start"}
	UpdateSteps  = []string{"upload", "install", "handoff", "verify"}
	RemoveSteps  = []string{"mark", "stop", "remove", "verify"}
)

// StepCancellable reports whether an active operation in (kind, step) may be
// cancelled by the user. Cancellation is only valid during an install before
// the daemon start step begins.
func StepCancellable(kind, step string) bool {
	if kind != OpInstall {
		return false
	}
	switch step {
	case "", "check", "upload", "verify", "install":
		return true
	default:
		return false
	}
}

// StepsForKind returns the canonical step list for an operation kind.
func StepsForKind(kind string) []string {
	switch kind {
	case OpInstall:
		return append([]string(nil), InstallSteps...)
	case OpUpdate:
		return append([]string(nil), UpdateSteps...)
	case OpRemove:
		return append([]string(nil), RemoveSteps...)
	default:
		return nil
	}
}

// MaxAutoRestarts is the maximum number of automatic restart attempts on lost
// daemon connections before transitioning to StateError (spec §7 / consent design).
const MaxAutoRestarts = 3

// StateSnapshot is the pure, immutable input/output state for the state machine.
type StateSnapshot struct {
	State            string
	Version          string
	Release          int64
	RestartCount     int
	LastErrorCode    string
	BoundFingerprint string
	BoundUsername    string
	BoundGateway     string
	PausedUsers      []int64
}

// EventKind identifies a state machine transition event.
type EventKind string

const (
	EvInstallStart        EventKind = "install_start"
	EvInstallDone         EventKind = "install_done"
	EvInstallFail         EventKind = "install_fail"
	EvInstallCancel       EventKind = "install_cancel"
	EvConnectOK           EventKind = "connect_ok"
	EvConnectIncompatible EventKind = "connect_incompatible"
	EvDaemonStopped       EventKind = "daemon_stopped"
	EvDisconnect          EventKind = "disconnect"
	EvRestartAttempt      EventKind = "restart_attempt"
	EvRestartExhausted    EventKind = "restart_exhausted"
	EvRetry               EventKind = "retry"
	EvUpdateStart         EventKind = "update_start"
	EvUpdateDone          EventKind = "update_done"
	EvUpdateRollback      EventKind = "update_rollback"
	EvUpdateFailError     EventKind = "update_fail_error"
	EvPause               EventKind = "pause"
	EvResume              EventKind = "resume"
	EvRemoveStart         EventKind = "remove_start"
	EvRemoveDone          EventKind = "remove_done"
	EvRemovePartial       EventKind = "remove_partial"
	EvRemoveStopFailed    EventKind = "remove_stop_failed"
	EvRemovePending       EventKind = "remove_pending"
	EvIdentityChanged     EventKind = "identity_changed"
	EvHostKeyRotatedKeep  EventKind = "host_key_rotated_keep"
	EvHostKeyRotatedDrop  EventKind = "host_key_rotated_drop"
)

// Event carries inputs for a single pure state transition.
type Event struct {
	Kind        EventKind
	Version     string
	Release     int64
	ErrorCode   string
	UserID      int64
	Fingerprint string
	Username    string
	Gateway     string
	// KeepInstalled controls whether an install failure leaves the host in
	// StateError (binary/logs kept for troubleshooting, e.g. start_failed) vs
	// StateOff (clean failure before install, e.g. unsupported_os, checksum).
	KeepInstalled bool
}

// TransitionResult is returned by Transition.
type TransitionResult struct {
	Next    StateSnapshot
	Allowed bool
	Reason  string
}

// Transition computes the next Host state from cur and ev without side effects.
func Transition(cur StateSnapshot, ev Event) TransitionResult {
	next := cur
	next.PausedUsers = slices.Clone(cur.PausedUsers)
	if next.State == "" {
		next.State = StateOff
	}

	switch ev.Kind {
	case EvInstallStart:
		// Install allowed from off, rebind, or error (retry after failed install).
		// Refused when removing/remove_pending (withdrawal-first) or already active.
		switch cur.State {
		case "", StateOff, StateRebind, StateError:
			next.State = StateInstalling
			next.LastErrorCode = ""
			next.RestartCount = 0
			return TransitionResult{Next: next, Allowed: true}
		case StateRemoving, StateRemovePending:
			return TransitionResult{Next: cur, Allowed: false, Reason: proto.ErrRemoving}
		default:
			return TransitionResult{Next: cur, Allowed: false, Reason: "invalid_state"}
		}

	case EvInstallDone:
		if cur.State != StateInstalling {
			return TransitionResult{Next: cur, Allowed: false, Reason: "not_installing"}
		}
		next.State = StateRunning
		next.Version = ev.Version
		next.Release = ev.Release
		next.RestartCount = 0
		next.LastErrorCode = ""
		if ev.Fingerprint != "" {
			next.BoundFingerprint = ev.Fingerprint
		}
		if ev.Username != "" {
			next.BoundUsername = ev.Username
		}
		if ev.Gateway != "" {
			next.BoundGateway = ev.Gateway
		}
		return TransitionResult{Next: next, Allowed: true}

	case EvInstallFail:
		if cur.State != StateInstalling {
			return TransitionResult{Next: cur, Allowed: false, Reason: "not_installing"}
		}
		next.LastErrorCode = ev.ErrorCode
		if ev.KeepInstalled {
			next.State = StateError
		} else {
			next.State = StateOff
		}
		return TransitionResult{Next: next, Allowed: true}

	case EvInstallCancel:
		if cur.State != StateInstalling {
			return TransitionResult{Next: cur, Allowed: false, Reason: "not_installing"}
		}
		next.State = StateOff
		next.LastErrorCode = ""
		return TransitionResult{Next: next, Allowed: true}

	case EvConnectOK:
		// Never connect/start when off, removing, remove_pending, or rebind.
		switch cur.State {
		case "", StateOff, StateRemoving, StateRemovePending, StateRebind:
			return TransitionResult{Next: cur, Allowed: false, Reason: "not_authorized"}
		default:
			next.State = StateRunning
			if ev.Version != "" {
				next.Version = ev.Version
			}
			if ev.Release > 0 {
				next.Release = ev.Release
			}
			next.RestartCount = 0
			// Preserve update_failed banner if explicitly recorded; clear transient error.
			if next.LastErrorCode != "update_failed" {
				next.LastErrorCode = ""
			}
			return TransitionResult{Next: next, Allowed: true}
		}

	case EvConnectIncompatible:
		switch cur.State {
		case "", StateOff, StateRemoving, StateRemovePending:
			return TransitionResult{Next: cur, Allowed: false, Reason: "not_authorized"}
		default:
			next.State = StateIncompatible
			if ev.Version != "" {
				next.Version = ev.Version
			}
			if ev.Release > 0 {
				next.Release = ev.Release
			}
			next.LastErrorCode = proto.ErrProtoMismatch
			return TransitionResult{Next: next, Allowed: true}
		}

	case EvDaemonStopped:
		switch cur.State {
		case StateRunning, StateLost, StatePaused:
			next.State = StateStopped
			return TransitionResult{Next: next, Allowed: true}
		default:
			return TransitionResult{Next: cur, Allowed: false, Reason: "invalid_state"}
		}

	case EvDisconnect:
		switch cur.State {
		case StateRunning, StateUpdating:
			next.State = StateLost
			return TransitionResult{Next: next, Allowed: true}
		case StateLost:
			return TransitionResult{Next: next, Allowed: true}
		default:
			return TransitionResult{Next: cur, Allowed: false, Reason: "invalid_state"}
		}

	case EvRestartAttempt:
		if cur.State != StateLost && cur.State != StateRunning {
			return TransitionResult{Next: cur, Allowed: false, Reason: "invalid_state"}
		}
		next.RestartCount++
		if next.RestartCount >= MaxAutoRestarts {
			next.State = StateError
			if ev.ErrorCode != "" {
				next.LastErrorCode = ev.ErrorCode
			} else {
				next.LastErrorCode = "start_failed"
			}
		} else {
			next.State = StateLost
		}
		return TransitionResult{Next: next, Allowed: true}

	case EvRestartExhausted:
		next.State = StateError
		next.RestartCount = MaxAutoRestarts
		if ev.ErrorCode != "" {
			next.LastErrorCode = ev.ErrorCode
		} else {
			next.LastErrorCode = "start_failed"
		}
		return TransitionResult{Next: next, Allowed: true}

	case EvRetry:
		if cur.State != StateError && cur.State != StateLost {
			return TransitionResult{Next: cur, Allowed: false, Reason: "invalid_state"}
		}
		next.RestartCount = 0
		next.LastErrorCode = ""
		next.State = StateLost
		return TransitionResult{Next: next, Allowed: true}

	case EvUpdateStart:
		switch cur.State {
		case StateRunning, StateStopped, StateIncompatible, StateError:
			next.State = StateUpdating
			next.LastErrorCode = ""
			return TransitionResult{Next: next, Allowed: true}
		case StateRemoving, StateRemovePending:
			return TransitionResult{Next: cur, Allowed: false, Reason: proto.ErrRemoving}
		default:
			return TransitionResult{Next: cur, Allowed: false, Reason: "invalid_state"}
		}

	case EvUpdateDone:
		if cur.State != StateUpdating {
			return TransitionResult{Next: cur, Allowed: false, Reason: "not_updating"}
		}
		next.State = StateRunning
		next.Version = ev.Version
		next.Release = ev.Release
		next.RestartCount = 0
		next.LastErrorCode = ""
		return TransitionResult{Next: next, Allowed: true}

	case EvUpdateRollback:
		// Rolled back to a compatible previous version after handoff failure or crash loop.
		next.State = StateRunning
		if ev.Version != "" {
			next.Version = ev.Version
		}
		if ev.Release > 0 {
			next.Release = ev.Release
		}
		next.RestartCount = 0
		next.LastErrorCode = "update_failed"
		return TransitionResult{Next: next, Allowed: true}

	case EvUpdateFailError:
		next.State = StateError
		if ev.ErrorCode != "" {
			next.LastErrorCode = ev.ErrorCode
		} else {
			next.LastErrorCode = "update_failed"
		}
		return TransitionResult{Next: next, Allowed: true}

	case EvPause:
		switch cur.State {
		case StateRunning, StateStopped, StatePaused, StateLost:
			if ev.UserID > 0 && !slices.Contains(next.PausedUsers, ev.UserID) {
				next.PausedUsers = append(next.PausedUsers, ev.UserID)
			}
			return TransitionResult{Next: next, Allowed: true}
		default:
			return TransitionResult{Next: cur, Allowed: false, Reason: "invalid_state"}
		}

	case EvResume:
		if ev.UserID > 0 {
			next.PausedUsers = slices.DeleteFunc(next.PausedUsers, func(id int64) bool { return id == ev.UserID })
		}
		if cur.State == StatePaused {
			next.State = StateStopped
		}
		return TransitionResult{Next: next, Allowed: true}

	case EvRemoveStart:
		// Withdrawal-first: remove is allowed from ANY state except off.
		if cur.State == StateOff || cur.State == "" {
			return TransitionResult{Next: cur, Allowed: false, Reason: "not_installed"}
		}
		next.State = StateRemoving
		next.LastErrorCode = ""
		return TransitionResult{Next: next, Allowed: true}

	case EvRemoveDone:
		next.State = StateOff
		next.Version = ""
		next.Release = 0
		next.RestartCount = 0
		next.LastErrorCode = ""
		next.PausedUsers = nil
		return TransitionResult{Next: next, Allowed: true}

	case EvRemovePartial:
		next.State = StateError
		next.LastErrorCode = "partial"
		return TransitionResult{Next: next, Allowed: true}

	case EvRemoveStopFailed:
		next.State = StateError
		next.LastErrorCode = "stop_failed"
		return TransitionResult{Next: next, Allowed: true}

	case EvRemovePending:
		next.State = StateRemovePending
		next.LastErrorCode = ""
		return TransitionResult{Next: next, Allowed: true}

	case EvIdentityChanged:
		switch cur.State {
		case "", StateOff, StateRemoving, StateRemovePending:
			return TransitionResult{Next: cur, Allowed: false, Reason: "not_bound"}
		default:
			next.State = StateRebind
			next.LastErrorCode = ""
			return TransitionResult{Next: next, Allowed: true}
		}

	case EvHostKeyRotatedKeep:
		switch cur.State {
		case "", StateOff, StateRemoving, StateRemovePending, StateRebind:
			return TransitionResult{Next: cur, Allowed: false, Reason: "not_bound"}
		default:
			if ev.Fingerprint != "" {
				next.BoundFingerprint = ev.Fingerprint
			}
			return TransitionResult{Next: next, Allowed: true}
		}

	case EvHostKeyRotatedDrop:
		switch cur.State {
		case "", StateOff, StateRemoving, StateRemovePending:
			return TransitionResult{Next: cur, Allowed: false, Reason: "not_bound"}
		default:
			next.State = StateRebind
			next.LastErrorCode = ""
			return TransitionResult{Next: next, Allowed: true}
		}
	}

	return TransitionResult{Next: cur, Allowed: false, Reason: "unknown_event"}
}

// EffectiveUserState returns the state presented to userID, applying per-user
// pause overlay when the host daemon itself is running/stopped.
func EffectiveUserState(s StateSnapshot, userID int64) string {
	if userID > 0 && slices.Contains(s.PausedUsers, userID) {
		switch s.State {
		case StateRunning, StateStopped, StateLost, StatePaused:
			return StatePaused
		}
	}
	if s.State == "" {
		return StateOff
	}
	return s.State
}

// CheckBinding verifies that the current host identity matches the bound
// authorization triple (host key fingerprint, SSH username, gateway ID).
func CheckBinding(s StateSnapshot, fingerprint, username, gateway string) bool {
	if s.BoundFingerprint == "" || s.BoundUsername == "" || s.BoundGateway == "" {
		return false
	}
	return s.BoundFingerprint == fingerprint &&
		s.BoundUsername == username &&
		s.BoundGateway == gateway
}

// UpdateAction describes what the gateway should do when comparing the host's
// running corrald against the gateway's bundled Dist.
type UpdateAction string

const (
	UpdateNone          UpdateAction = "none"
	UpdateAuto          UpdateAction = "auto"
	UpdateNeedConsent   UpdateAction = "need_consent"
	UpdateIncompatible  UpdateAction = "incompatible"
	UpdateSkippedFailed UpdateAction = "skipped_failed"
)

// UpdateDecision is the pure result of DecideUpdate.
type UpdateDecision struct {
	Action UpdateAction
	Added  []string // non-empty when Action == UpdateNeedConsent
}

// DecideUpdate applies spec §2 & §6 compatibility, monotonicity, failed.json,
// and manifest scope growth rules.
func DecideUpdate(
	hostRelease int64,
	hostProto int,
	consentedManifest proto.Manifest,
	distRelease int64,
	distManifest proto.Manifest,
	failedReleases []int64,
	protoMin, protoMax int,
) UpdateDecision {
	if !proto.ProtocolInRange(hostProto, protoMin, protoMax) {
		return UpdateDecision{Action: UpdateIncompatible}
	}
	if !proto.ShouldUpgrade(hostRelease, distRelease) {
		return UpdateDecision{Action: UpdateNone}
	}
	if slices.Contains(failedReleases, distRelease) {
		return UpdateDecision{Action: UpdateSkippedFailed}
	}
	added := proto.ScopeGrew(consentedManifest, distManifest)
	if len(added) > 0 {
		return UpdateDecision{Action: UpdateNeedConsent, Added: added}
	}
	return UpdateDecision{Action: UpdateAuto}
}

// CapAllowedByScope reports whether capName may be served by a running daemon
// whose actual manifest is runningManifest, given the user's previously
// consentedManifest. When the running daemon expands global scope (schema,
// writes, egress, or resource budget), all capabilities are blocked until the
// user consents. When only specific reads/actions/optional items grew, newly
// added capabilities are blocked while previously consented ones remain usable.
func CapAllowedByScope(consented, running proto.Manifest, capName string) bool {
	added := proto.ScopeGrew(consented, running)
	if len(added) == 0 {
		return true
	}
	for _, item := range added {
		if strings.HasPrefix(item, "schema:") ||
			strings.HasPrefix(item, "writes:") ||
			strings.HasPrefix(item, "egress:") ||
			strings.HasPrefix(item, "budget:") {
			return false
		}
	}
	capName = strings.TrimSpace(capName)
	if capName == "" {
		return false
	}
	for _, item := range added {
		if item == capName ||
			item == "reads:"+capName ||
			item == "actions:"+capName ||
			item == "optional:"+capName {
			return false
		}
		switch capName {
		case "locate", "watch", "agent.state", "agent.events", "agent.locate", "agent.watch", "convo.subscribe", "fs.watch":
			if item == "reads:"+proto.ReadProc ||
				strings.HasPrefix(item, "reads:agent-logs:") ||
				strings.HasPrefix(item, "reads:env:") ||
				strings.HasPrefix(item, "reads:pane-screen:") {
				return false
			}
		case "tails", "convo.tails", "convo.message_tails":
			if item == "reads:"+proto.ReadProc ||
				strings.HasPrefix(item, "reads:agent-logs:") ||
				strings.HasPrefix(item, "reads:env:") {
				return false
			}
		case "proc.monitor":
			if item == "reads:"+proto.ReadProc {
				return false
			}
		case "terminal.hint":
			if strings.HasPrefix(item, "reads:pane-screen:") {
				return false
			}
		case "pane.send", proto.ActionPaneInput:
			if item == "actions:"+proto.ActionPaneInput ||
				strings.HasPrefix(item, "reads:pane-screen:") ||
				item == "reads:"+proto.ReadProc {
				return false
			}
		case "pane.answer", proto.ActionAnswerCard:
			if item == "actions:"+proto.ActionAnswerCard ||
				strings.HasPrefix(item, "reads:pane-screen:") ||
				item == "reads:"+proto.ReadProc {
				return false
			}
		}
	}
	return true
}

// ShouldRollbackOnCrash reports whether repeated abnormal exits after an update
// should trigger an automatic rollback to the previous version (spec §6 item 7:
// 2+ abnormal exits within 30 minutes of READY).
func ShouldRollbackOnCrash(crashCount int, timeSinceUpdate, crashWindow time.Duration, threshold int) bool {
	if threshold <= 0 {
		threshold = 2
	}
	if crashWindow <= 0 {
		crashWindow = 30 * time.Minute
	}
	return timeSinceUpdate >= 0 && timeSinceUpdate <= crashWindow && crashCount >= threshold
}

// HostObservedState summarizes what the gateway observed on the host after a
// reconnect or gateway restart (reading install.json, meta.json, ops, tombstone,
// and leftover stage-* directories).
type HostObservedState struct {
	HasTombstone   bool
	HasInstall     bool
	DaemonRunning  bool
	InstalledVer   string
	InstalledRel   int64
	OpRecordResult string // "ok", "failed", or "" if ops/<op>.json absent
	LeftoverStages []string
}

// ReconcileAction describes what the gateway must do after reading host state.
type ReconcileAction string

const (
	ReconcileNone         ReconcileAction = "none"
	ReconcileFinishRemove ReconcileAction = "finish_remove"
	ReconcileConnect      ReconcileAction = "connect"
	ReconcileStartDaemon  ReconcileAction = "start_daemon"
	ReconcileMarkOff      ReconcileAction = "mark_off"
	ReconcileMarkOpFailed ReconcileAction = "mark_op_failed"
)

// ReconcileDecision is the pure result of ReconcileAfterRestart.
type ReconcileDecision struct {
	NextState     string
	OpFinalState  string // "", OpDone, OpFailed, OpCancelled
	OpErrorCode   string
	Action        ReconcileAction
	CleanupStages []string
}

// ReconcileAfterRestart decides how to settle an interrupted operation or host
// state after a gateway restart or SSH disconnect, strictly trusting host truth
// over gateway memory (spec §7 & consent-design "操作的一致性").
func ReconcileAfterRestart(curState string, opKind string, obs HostObservedState) ReconcileDecision {
	// Rule 1: Withdrawal-first. If the host has a tombstone or gateway recorded
	// removing / remove_pending, always finish removal and clean stages.
	if obs.HasTombstone || curState == StateRemoving || curState == StateRemovePending || opKind == OpRemove {
		if !obs.HasInstall && !obs.DaemonRunning && !obs.HasTombstone {
			return ReconcileDecision{
				NextState:     StateOff,
				OpFinalState:  OpDone,
				Action:        ReconcileMarkOff,
				CleanupStages: obs.LeftoverStages,
			}
		}
		return ReconcileDecision{
			NextState:     StateRemoving,
			Action:        ReconcileFinishRemove,
			CleanupStages: obs.LeftoverStages,
		}
	}

	// Rule 2: Interrupted install.
	if curState == StateInstalling || opKind == OpInstall {
		if obs.DaemonRunning {
			return ReconcileDecision{
				NextState:     StateRunning,
				OpFinalState:  OpDone,
				Action:        ReconcileConnect,
				CleanupStages: obs.LeftoverStages,
			}
		}
		if obs.HasInstall && obs.OpRecordResult == "ok" {
			// Binary was installed cleanly before gateway restarted; finish by starting daemon.
			return ReconcileDecision{
				NextState:     StateStopped,
				OpFinalState:  OpDone,
				Action:        ReconcileStartDaemon,
				CleanupStages: obs.LeftoverStages,
			}
		}
		// Install did not finish on host; clean leftover stages and report disconnected.
		return ReconcileDecision{
			NextState:     StateOff,
			OpFinalState:  OpFailed,
			OpErrorCode:   "disconnected",
			Action:        ReconcileMarkOpFailed,
			CleanupStages: obs.LeftoverStages,
		}
	}

	// Rule 3: Interrupted update.
	if curState == StateUpdating || opKind == OpUpdate {
		if obs.DaemonRunning {
			return ReconcileDecision{
				NextState:     StateRunning,
				OpFinalState:  OpDone,
				Action:        ReconcileConnect,
				CleanupStages: obs.LeftoverStages,
			}
		}
		if obs.HasInstall {
			return ReconcileDecision{
				NextState:     StateStopped,
				OpFinalState:  OpFailed,
				OpErrorCode:   "disconnected",
				Action:        ReconcileStartDaemon,
				CleanupStages: obs.LeftoverStages,
			}
		}
		return ReconcileDecision{
			NextState:     StateOff,
			OpFinalState:  OpFailed,
			OpErrorCode:   "disconnected",
			Action:        ReconcileMarkOff,
			CleanupStages: obs.LeftoverStages,
		}
	}

	// Rule 4: Normal steady states.
	if !obs.HasInstall && !obs.DaemonRunning {
		if curState != StateOff && curState != StateRebind {
			return ReconcileDecision{
				NextState:     StateOff,
				Action:        ReconcileMarkOff,
				CleanupStages: obs.LeftoverStages,
			}
		}
		return ReconcileDecision{NextState: curState, Action: ReconcileNone, CleanupStages: obs.LeftoverStages}
	}
	if curState == StateRebind {
		return ReconcileDecision{NextState: StateRebind, Action: ReconcileNone, CleanupStages: obs.LeftoverStages}
	}
	if obs.DaemonRunning {
		return ReconcileDecision{NextState: StateRunning, Action: ReconcileConnect, CleanupStages: obs.LeftoverStages}
	}
	return ReconcileDecision{NextState: StateStopped, Action: ReconcileStartDaemon, CleanupStages: obs.LeftoverStages}
}
