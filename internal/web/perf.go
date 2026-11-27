package web

import (
	"context"
	"errors"
	"net/http"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/ihainan/corral/internal/corrald/gw"
	"github.com/ihainan/corral/internal/perf"
	"github.com/ihainan/corral/internal/sshx"
)

// GET /api/admin/perf: bounded in-process latency and outcome summary for key
// gateway operations (administrators only).
func (s *Server) apiAdminPerf(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, s.perf.Snapshot())
}

func (s *Server) observeSSHBusy(c *ssh.Client, hostID int64) {
	if s == nil || s.perf == nil || c == nil {
		return
	}
	sshx.SetBusyObserver(c, func(kind string) {
		s.perf.Count("ssh.busy", kind, hostID)
	})
}

func (s *Server) perfObserve(op string, start time.Time, outcome string, hostID int64, bucket string) {
	if s == nil || s.perf == nil {
		return
	}
	s.perf.Observe(op, time.Since(start), outcome, hostID, bucket)
}

func (s *Server) perfGated(what string, start time.Time, err error, hostID int64) {
	s.perfGatedVia(what, "ssh", start, err, hostID)
}

func (s *Server) perfGatedVia(what, via string, start time.Time, err error, hostID int64) {
	if s == nil || s.perf == nil {
		return
	}
	s.perf.Observe("gated."+what, time.Since(start), gatedOutcome(err, what), hostID, via)
}

func (s *Server) perfLocate(op string, start time.Time, err error, procs int, hostID int64) {
	if s == nil || s.perf == nil {
		return
	}
	bucket := ""
	if err == nil {
		bucket = perf.ProcBucket(procs)
	}
	s.perf.Observe(op, time.Since(start), locateOutcome(err), hostID, bucket)
}

func (s *Server) perfLocateOutcome(op string, start time.Time, outcome string, procs int, hasProcs bool, hostID int64) {
	if s == nil || s.perf == nil {
		return
	}
	bucket := ""
	if hasProcs {
		bucket = perf.ProcBucket(procs)
	}
	s.perf.Observe(op, time.Since(start), outcome, hostID, bucket)
}

// gatedOutcome classifies a convoGated* script result into a fixed outcome
// label based on the script's exit codes and sshx errors.
func gatedOutcome(err error, what string) string {
	if err == nil {
		return "ok"
	}
	if errors.Is(err, sshx.ErrBusy) {
		return "channel_busy"
	}
	if errors.Is(err, gw.ErrUnknownResult) {
		return "corrald_unknown_result"
	}
	var exitErr *sshx.ExitError
	if errors.As(err, &exitErr) {
		switch exitErr.Status {
		case 3:
			return "agent_changed"
		case 4:
			return "send_failed"
		case 5:
			return "typed_not_sent"
		case 6:
			return "type_failed"
		case 7:
			return "pane_busy"
		case 8:
			return "screen_unread"
		case 9:
			switch what {
			case "reply":
				return "not_waiting"
			case "close_dialog", "panel":
				return "no_dialog"
			default:
				return "agent_waiting"
			}
		case 10:
			return "screen_changed"
		case 11:
			return "cannot_verify"
		case 12:
			return "agent_busy"
		case exitOwnerSubagent:
			return "subagent_view"
		case exitOwnerOther:
			return "other_view"
		case exitOwnerNone:
			return "no_box"
		case exitOwnerTyped:
			return "owner_changed_typed"
		case 17:
			return "draft"
		case 18:
			return "enter_not_taken"
		case exitOwnerUnread:
			return "no_screen"
		default:
			return "exit_other"
		}
	}
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, context.Canceled):
		return "canceled"
	default:
		return "error"
	}
}

func locateOutcome(err error) string {
	switch {
	case err == nil:
		return "ok"
	case errors.Is(err, sshx.ErrBusy):
		return "busy"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, context.Canceled):
		return "canceled"
	default:
		return "error"
	}
}

func pressPerfOutcome(outcome, reason string) string {
	switch outcome {
	case "typed", "pressed", "read", "kept", "looked":
		if reason == "" || reason == "list_kept" {
			return "ok"
		}
		return reason
	default:
		if reason != "" {
			return reason
		}
		if outcome != "" {
			return outcome
		}
		return "unknown"
	}
}

func convoReadErrOutcome(err error) string {
	if errors.Is(err, sshx.ErrBusy) {
		return "busy"
	}
	return "read_error"
}

func (s *Server) perfRecordPress(what string, start time.Time, outcome, reason string, modelsFastKept bool, hostID int64) {
	switch what {
	case "models":
		if outcome == "kept" {
			o := "scan_kept"
			if modelsFastKept {
				o = "fast_kept"
			}
			s.perfObserve("convo.models_repeat", start, o, hostID, "")
		} else {
			s.perfObserve("convo.models_first", start, pressPerfOutcome(outcome, reason), hostID, "")
		}
	case "model", "effort", "mode", "send", "reply", "card":
		s.perfObserve("convo."+what, start, pressPerfOutcome(outcome, reason), hostID, "")
	}
}

// locateOp names a locate scan for the perf table: "locate.shared" when the
// scan reused one another request had just made (its time is not this
// path's), else the path it took (op).
func locateOp(op string, shared bool) string {
	if shared {
		return "locate.shared"
	}
	return op
}
