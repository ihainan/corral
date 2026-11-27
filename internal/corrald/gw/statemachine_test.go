package gw

import (
	"testing"
	"time"

	"github.com/ihainan/corral/internal/corrald/proto"
)

func TestStateMachineTransitions(t *testing.T) {
	tests := []struct {
		name        string
		cur         StateSnapshot
		ev          Event
		wantAllowed bool
		wantState   string
		wantRestart int
		wantErr     string
		wantFP      string
	}{
		{
			name:        "off to installing",
			cur:         StateSnapshot{State: StateOff},
			ev:          Event{Kind: EvInstallStart},
			wantAllowed: true,
			wantState:   StateInstalling,
		},
		{
			name:        "installing to running",
			cur:         StateSnapshot{State: StateInstalling},
			ev:          Event{Kind: EvInstallDone, Version: "0.3.1", Release: 100, Fingerprint: "SHA256:abc", Username: "alice", Gateway: "gw.test"},
			wantAllowed: true,
			wantState:   StateRunning,
			wantFP:      "SHA256:abc",
		},
		{
			name:        "install fail clean returns to off",
			cur:         StateSnapshot{State: StateInstalling},
			ev:          Event{Kind: EvInstallFail, ErrorCode: "unsupported_os", KeepInstalled: false},
			wantAllowed: true,
			wantState:   StateOff,
			wantErr:     "unsupported_os",
		},
		{
			name:        "install fail after install goes to error",
			cur:         StateSnapshot{State: StateInstalling},
			ev:          Event{Kind: EvInstallFail, ErrorCode: "start_failed", KeepInstalled: true},
			wantAllowed: true,
			wantState:   StateError,
			wantErr:     "start_failed",
		},
		{
			name:        "install cancel returns to off",
			cur:         StateSnapshot{State: StateInstalling},
			ev:          Event{Kind: EvInstallCancel},
			wantAllowed: true,
			wantState:   StateOff,
		},
		{
			name:        "withdrawal-first blocks install during removing",
			cur:         StateSnapshot{State: StateRemoving},
			ev:          Event{Kind: EvInstallStart},
			wantAllowed: false,
			wantState:   StateRemoving,
		},
		{
			name:        "withdrawal-first blocks install during remove_pending",
			cur:         StateSnapshot{State: StateRemovePending},
			ev:          Event{Kind: EvInstallStart},
			wantAllowed: false,
			wantState:   StateRemovePending,
		},
		{
			name:        "withdrawal-first blocks update during remove_pending",
			cur:         StateSnapshot{State: StateRemovePending},
			ev:          Event{Kind: EvUpdateStart},
			wantAllowed: false,
			wantState:   StateRemovePending,
		},
		{
			name:        "remove allowed from installing (withdrawal-first)",
			cur:         StateSnapshot{State: StateInstalling},
			ev:          Event{Kind: EvRemoveStart},
			wantAllowed: true,
			wantState:   StateRemoving,
		},
		{
			name:        "remove allowed from running",
			cur:         StateSnapshot{State: StateRunning},
			ev:          Event{Kind: EvRemoveStart},
			wantAllowed: true,
			wantState:   StateRemoving,
		},
		{
			name:        "remove refused when already off",
			cur:         StateSnapshot{State: StateOff},
			ev:          Event{Kind: EvRemoveStart},
			wantAllowed: false,
			wantState:   StateOff,
		},
		{
			name:        "remove done resets to off",
			cur:         StateSnapshot{State: StateRemoving, Version: "0.3.1", Release: 100},
			ev:          Event{Kind: EvRemoveDone},
			wantAllowed: true,
			wantState:   StateOff,
		},
		{
			name:        "remove partial enters error",
			cur:         StateSnapshot{State: StateRemoving},
			ev:          Event{Kind: EvRemovePartial},
			wantAllowed: true,
			wantState:   StateError,
			wantErr:     "partial",
		},
		{
			name:        "remove stop_failed enters error",
			cur:         StateSnapshot{State: StateRemoving},
			ev:          Event{Kind: EvRemoveStopFailed},
			wantAllowed: true,
			wantState:   StateError,
			wantErr:     "stop_failed",
		},
		{
			name:        "remove pending enters remove_pending",
			cur:         StateSnapshot{State: StateRemoving},
			ev:          Event{Kind: EvRemovePending},
			wantAllowed: true,
			wantState:   StateRemovePending,
		},
		{
			name:        "running disconnect enters lost",
			cur:         StateSnapshot{State: StateRunning},
			ev:          Event{Kind: EvDisconnect},
			wantAllowed: true,
			wantState:   StateLost,
		},
		{
			name:        "restart attempt 1 stays lost",
			cur:         StateSnapshot{State: StateLost, RestartCount: 0},
			ev:          Event{Kind: EvRestartAttempt},
			wantAllowed: true,
			wantState:   StateLost,
			wantRestart: 1,
		},
		{
			name:        "restart attempt 2 stays lost",
			cur:         StateSnapshot{State: StateLost, RestartCount: 1},
			ev:          Event{Kind: EvRestartAttempt},
			wantAllowed: true,
			wantState:   StateLost,
			wantRestart: 2,
		},
		{
			name:        "restart attempt 3 enters error",
			cur:         StateSnapshot{State: StateLost, RestartCount: 2},
			ev:          Event{Kind: EvRestartAttempt, ErrorCode: "start_failed"},
			wantAllowed: true,
			wantState:   StateError,
			wantRestart: 3,
			wantErr:     "start_failed",
		},
		{
			name:        "retry from error resets restart count",
			cur:         StateSnapshot{State: StateError, RestartCount: 3, LastErrorCode: "start_failed"},
			ev:          Event{Kind: EvRetry},
			wantAllowed: true,
			wantState:   StateLost,
			wantRestart: 0,
			wantErr:     "",
		},
		{
			name:        "connect incompatible enters incompatible",
			cur:         StateSnapshot{State: StateStopped},
			ev:          Event{Kind: EvConnectIncompatible, Version: "0.9.0", Release: 999},
			wantAllowed: true,
			wantState:   StateIncompatible,
			wantErr:     proto.ErrProtoMismatch,
		},
		{
			name:        "update start from incompatible allowed (replace)",
			cur:         StateSnapshot{State: StateIncompatible},
			ev:          Event{Kind: EvUpdateStart},
			wantAllowed: true,
			wantState:   StateUpdating,
		},
		{
			name:        "update done returns to running",
			cur:         StateSnapshot{State: StateUpdating, Version: "0.3.1", Release: 100},
			ev:          Event{Kind: EvUpdateDone, Version: "0.3.2", Release: 200},
			wantAllowed: true,
			wantState:   StateRunning,
		},
		{
			name:        "update rollback keeps running with update_failed banner",
			cur:         StateSnapshot{State: StateUpdating, Version: "0.3.1", Release: 100},
			ev:          Event{Kind: EvUpdateRollback, Version: "0.3.1", Release: 100},
			wantAllowed: true,
			wantState:   StateRunning,
			wantErr:     "update_failed",
		},
		{
			name:        "identity changed enters rebind",
			cur:         StateSnapshot{State: StateRunning, BoundFingerprint: "SHA256:old"},
			ev:          Event{Kind: EvIdentityChanged},
			wantAllowed: true,
			wantState:   StateRebind,
		},
		{
			name:        "rebind can re-install",
			cur:         StateSnapshot{State: StateRebind},
			ev:          Event{Kind: EvInstallStart},
			wantAllowed: true,
			wantState:   StateInstalling,
		},
		{
			name:        "host key rotated keep updates fingerprint",
			cur:         StateSnapshot{State: StateRunning, BoundFingerprint: "SHA256:old"},
			ev:          Event{Kind: EvHostKeyRotatedKeep, Fingerprint: "SHA256:new"},
			wantAllowed: true,
			wantState:   StateRunning,
			wantFP:      "SHA256:new",
		},
		{
			name:        "host key rotated drop enters rebind",
			cur:         StateSnapshot{State: StateRunning, BoundFingerprint: "SHA256:old"},
			ev:          Event{Kind: EvHostKeyRotatedDrop},
			wantAllowed: true,
			wantState:   StateRebind,
		},
		{
			name:        "connect refused while rebind",
			cur:         StateSnapshot{State: StateRebind},
			ev:          Event{Kind: EvConnectOK},
			wantAllowed: false,
			wantState:   StateRebind,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			res := Transition(tc.cur, tc.ev)
			if res.Allowed != tc.wantAllowed {
				t.Fatalf("Allowed = %v (reason=%q), want %v", res.Allowed, res.Reason, tc.wantAllowed)
			}
			if res.Next.State != tc.wantState {
				t.Fatalf("State = %q, want %q", res.Next.State, tc.wantState)
			}
			if tc.wantRestart != 0 && res.Next.RestartCount != tc.wantRestart {
				t.Fatalf("RestartCount = %d, want %d", res.Next.RestartCount, tc.wantRestart)
			}
			if tc.wantErr != "" && res.Next.LastErrorCode != tc.wantErr {
				t.Fatalf("LastErrorCode = %q, want %q", res.Next.LastErrorCode, tc.wantErr)
			}
			if tc.wantFP != "" && res.Next.BoundFingerprint != tc.wantFP {
				t.Fatalf("BoundFingerprint = %q, want %q", res.Next.BoundFingerprint, tc.wantFP)
			}
		})
	}
}

func TestEffectiveUserStatePauseOverlay(t *testing.T) {
	s := StateSnapshot{State: StateRunning, PausedUsers: []int64{42}}
	if got := EffectiveUserState(s, 42); got != StatePaused {
		t.Fatalf("paused user saw %q, want paused", got)
	}
	if got := EffectiveUserState(s, 99); got != StateRunning {
		t.Fatalf("other user saw %q, want running", got)
	}
	// Pause does not override error or rebind.
	s.State = StateRebind
	if got := EffectiveUserState(s, 42); got != StateRebind {
		t.Fatalf("rebind user saw %q, want rebind", got)
	}
}

func TestDecideUpdate(t *testing.T) {
	base := proto.DefaultManifest()
	grown := proto.DefaultManifest()
	grown.Reads = append(grown.Reads, "agent-logs:newagent")

	tests := []struct {
		name      string
		hostRel   int64
		hostProto int
		consented proto.Manifest
		distRel   int64
		distMan   proto.Manifest
		failed    []int64
		want      UpdateAction
		wantAdded int
	}{
		{
			name:      "same release does nothing",
			hostRel:   100,
			hostProto: 1,
			consented: base,
			distRel:   100,
			distMan:   base,
			want:      UpdateNone,
		},
		{
			name:      "older gateway never downgrades",
			hostRel:   200,
			hostProto: 1,
			consented: base,
			distRel:   100,
			distMan:   base,
			want:      UpdateNone,
		},
		{
			name:      "protocol out of range is incompatible",
			hostRel:   200,
			hostProto: 2,
			consented: base,
			distRel:   100,
			distMan:   base,
			want:      UpdateIncompatible,
		},
		{
			name:      "newer release same scope auto-updates",
			hostRel:   100,
			hostProto: 1,
			consented: base,
			distRel:   200,
			distMan:   base,
			want:      UpdateAuto,
		},
		{
			name:      "newer release in failed.json skipped",
			hostRel:   100,
			hostProto: 1,
			consented: base,
			distRel:   200,
			distMan:   base,
			failed:    []int64{200},
			want:      UpdateSkippedFailed,
		},
		{
			name:      "newer release with scope growth requires consent",
			hostRel:   100,
			hostProto: 1,
			consented: base,
			distRel:   200,
			distMan:   grown,
			want:      UpdateNeedConsent,
			wantAdded: 1,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dec := DecideUpdate(tc.hostRel, tc.hostProto, tc.consented, tc.distRel, tc.distMan, tc.failed, 1, 1)
			if dec.Action != tc.want {
				t.Fatalf("Action = %q, want %q", dec.Action, tc.want)
			}
			if len(dec.Added) != tc.wantAdded {
				t.Fatalf("Added len = %d (%v), want %d", len(dec.Added), dec.Added, tc.wantAdded)
			}
		})
	}
}

func TestShouldRollbackOnCrash(t *testing.T) {
	if ShouldRollbackOnCrash(1, 5*time.Minute, 30*time.Minute, 2) {
		t.Fatal("1 crash should not trigger rollback")
	}
	if !ShouldRollbackOnCrash(2, 5*time.Minute, 30*time.Minute, 2) {
		t.Fatal("2 crashes within 30 min should trigger rollback")
	}
	if ShouldRollbackOnCrash(3, 31*time.Minute, 30*time.Minute, 2) {
		t.Fatal("crashes after 30 min window should not trigger rollback")
	}
}

func TestReconcileAfterRestart(t *testing.T) {
	tests := []struct {
		name       string
		curState   string
		opKind     string
		obs        HostObservedState
		wantAction ReconcileAction
		wantState  string
		wantOp     string
	}{
		{
			name:       "tombstone always finishes remove",
			curState:   StateInstalling,
			opKind:     OpInstall,
			obs:        HostObservedState{HasTombstone: true, HasInstall: true, LeftoverStages: []string{"/s1"}},
			wantAction: ReconcileFinishRemove,
			wantState:  StateRemoving,
		},
		{
			name:       "remove_pending with nothing left marks off",
			curState:   StateRemovePending,
			opKind:     OpRemove,
			obs:        HostObservedState{HasTombstone: false, HasInstall: false, DaemonRunning: false},
			wantAction: ReconcileMarkOff,
			wantState:  StateOff,
			wantOp:     OpDone,
		},
		{
			name:       "interrupted install with running daemon connects",
			curState:   StateInstalling,
			opKind:     OpInstall,
			obs:        HostObservedState{HasInstall: true, DaemonRunning: true},
			wantAction: ReconcileConnect,
			wantState:  StateRunning,
			wantOp:     OpDone,
		},
		{
			name:       "interrupted install with ok op record starts daemon",
			curState:   StateInstalling,
			opKind:     OpInstall,
			obs:        HostObservedState{HasInstall: true, DaemonRunning: false, OpRecordResult: "ok"},
			wantAction: ReconcileStartDaemon,
			wantState:  StateStopped,
			wantOp:     OpDone,
		},
		{
			name:       "interrupted install before install cleans up and fails",
			curState:   StateInstalling,
			opKind:     OpInstall,
			obs:        HostObservedState{HasInstall: false, DaemonRunning: false, LeftoverStages: []string{"/st"}},
			wantAction: ReconcileMarkOpFailed,
			wantState:  StateOff,
			wantOp:     OpFailed,
		},
		{
			name:       "interrupted update with running daemon connects",
			curState:   StateUpdating,
			opKind:     OpUpdate,
			obs:        HostObservedState{HasInstall: true, DaemonRunning: true},
			wantAction: ReconcileConnect,
			wantState:  StateRunning,
			wantOp:     OpDone,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dec := ReconcileAfterRestart(tc.curState, tc.opKind, tc.obs)
			if dec.Action != tc.wantAction {
				t.Fatalf("Action = %q, want %q", dec.Action, tc.wantAction)
			}
			if dec.NextState != tc.wantState {
				t.Fatalf("NextState = %q, want %q", dec.NextState, tc.wantState)
			}
			if tc.wantOp != "" && dec.OpFinalState != tc.wantOp {
				t.Fatalf("OpFinalState = %q, want %q", dec.OpFinalState, tc.wantOp)
			}
		})
	}
}

func TestStepCancellableAndCheckBinding(t *testing.T) {
	for _, step := range []string{"check", "upload", "verify", "install"} {
		if !StepCancellable(OpInstall, step) {
			t.Errorf("install step %q should be cancellable", step)
		}
	}
	if StepCancellable(OpInstall, "start") {
		t.Error("install step start must NOT be cancellable")
	}
	if StepCancellable(OpRemove, "mark") || StepCancellable(OpUpdate, "upload") {
		t.Error("remove/update steps must NOT be cancellable")
	}

	s := StateSnapshot{BoundFingerprint: "fp1", BoundUsername: "alice", BoundGateway: "gw1"}
	if !CheckBinding(s, "fp1", "alice", "gw1") {
		t.Fatal("exact binding should match")
	}
	if CheckBinding(s, "fp2", "alice", "gw1") {
		t.Fatal("different fingerprint must fail binding")
	}
	if CheckBinding(s, "fp1", "bob", "gw1") {
		t.Fatal("different username must fail binding")
	}
	if CheckBinding(s, "fp1", "alice", "gw2") {
		t.Fatal("different gateway must fail binding")
	}
}

func TestCapAllowedByScope(t *testing.T) {
	base := proto.DefaultManifest()

	// No scope growth -> all caps allowed.
	if !CapAllowedByScope(base, base, "agent.state") || !CapAllowedByScope(base, base, "pane.send") || !CapAllowedByScope(base, base, "") {
		t.Fatal("expected all caps allowed when scope did not grow")
	}

	// Optional capability added -> previously consented caps remain usable, new optional cap is blocked.
	optAdded := proto.DefaultManifest()
	optAdded.Optional = append(optAdded.Optional, "terminal.record")
	if !CapAllowedByScope(base, optAdded, "agent.state") {
		t.Fatal("expected previously consented agent.state to remain allowed when only optional cap grew")
	}
	if CapAllowedByScope(base, optAdded, "terminal.record") {
		t.Fatal("expected newly added optional cap terminal.record to be blocked")
	}
	if CapAllowedByScope(base, optAdded, "") {
		t.Fatal("expected empty capName to be blocked when scope grew")
	}

	// Action added -> pane.send blocked while read-only agent.state remains allowed.
	reduced := proto.DefaultManifest()
	reduced.Actions = []string{proto.ActionAnswerCard} // consented without pane-input
	if CapAllowedByScope(reduced, base, "pane.send") {
		t.Fatal("expected pane.send blocked when actions:pane-input is not consented")
	}
	if !CapAllowedByScope(reduced, base, "agent.state") {
		t.Fatal("expected agent.state allowed when only actions:pane-input grew")
	}

	// Global budget/writes/egress/schema growth -> ALL caps blocked.
	budgetGrew := proto.DefaultManifest()
	budgetGrew.Budget.RSSMB = base.Budget.RSSMB + 50
	if CapAllowedByScope(base, budgetGrew, "agent.state") {
		t.Fatal("expected all caps blocked when budget grew")
	}
	egressGrew := proto.DefaultManifest()
	egressGrew.Egress = append(egressGrew.Egress, "internet")
	if CapAllowedByScope(base, egressGrew, "agent.state") {
		t.Fatal("expected all caps blocked when egress grew")
	}
}
