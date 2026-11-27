package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ihainan/corral/internal/corrald/gw"
	"github.com/ihainan/corral/internal/corrald/proto"
	"github.com/ihainan/corral/internal/sshx"
	"github.com/ihainan/corral/internal/term"
)

// gatedHandoffRetryBase is the initial backoff when corrald returns
// handoff_in_progress during a hot-update drain (overridable in tests).
var gatedHandoffRetryBase = 100 * time.Millisecond

// gatedInProgressPollInterval is the poll interval when corrald reports the
// same op_id is still executing within the current epoch.
var gatedInProgressPollInterval = 150 * time.Millisecond

// gatedReq describes a single gated pane send / keypress / card-answer
// operation previously executed directly via convoGated*Command over SSH.
type gatedReq struct {
	Gate      string
	Mode      string
	Session   string
	Pane      string
	Socket    string
	PID       int64
	Start     int64
	SID       string
	LenSpec   string   // optional explicit length spec (e.g. "12+", "12~", "12!", "12-", "12@"); empty derives from Keys/Text
	Keys      []string // nil = text paste mode; non-nil (including empty slice) = keys/read mode
	Read      int
	Text      string
	MaxOut    int
	Release   func() // invoked once when host execution completes
	AwaitSlot time.Duration
	What      string // perf label ("send", "reply", "card", "mode", "model", "effort", "sent_enter", "model_line", ...)
}

func gatedCapName(gate string) string {
	switch {
	case strings.HasPrefix(gate, "reply:"),
		strings.HasPrefix(gate, "creply:"),
		strings.HasPrefix(gate, "preply:"),
		strings.HasPrefix(gate, "areply:"),
		strings.HasPrefix(gate, "card:"):
		return "pane.answer"
	default:
		return "pane.send"
	}
}

func writeCorraldUnknownResult(w http.ResponseWriter) {
	writeError(w, http.StatusConflict, "corrald_unknown_result", "write operation result unknown; do not retry blindly")
}

func gatedSleep(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// runGated is the single unified gateway entrypoint for all convoGated pane
// operations (typing messages, pressing keys, answering prompts/cards, and
// gated screen checks).
//
// Routing & safety contract (spec §5, §6, §9 & consent-design):
//   - When corrald is available and the required capability ("pane.send" or
//     "pane.answer") is healthy & within consented scope, sends a `pane.send`
//     RPC with a fresh protocol `op_id`.
//   - If corrald returns `handoff_in_progress` (draining during hot update),
//     waits and retries with the SAME `op_id` within ctx's remaining deadline.
//   - If the request has ALREADY been sent to corrald and the connection drops,
//     times out, changes epoch, or returns op_unknown/op_expired, returns
//     gw.ErrUnknownResult (which callers map to HTTP 409 corrald_unknown_result)
//     and NEVER falls back to SSH.
//   - Only pre-send unavailability (no client, degraded cap, closed before
//     write, or method_not_found/not_supported) falls back to the original SSH
//     convoGated script.
func (s *Server) runGated(ctx context.Context, sess *term.Session, req gatedReq) ([]byte, error) {
	t0 := time.Now()
	lenSpec := req.LenSpec
	if lenSpec == "" {
		if req.Keys != nil {
			lenSpec = "keys"
		} else {
			lenSpec = strconv.Itoa(len(req.Text))
		}
	}
	maxOut := req.MaxOut
	if maxOut <= 0 {
		maxOut = sendOut
	}
	readLines := max(0, min(req.Read, 60))

	var relOnce sync.Once
	release := func() {
		if req.Release != nil {
			relOnce.Do(req.Release)
		}
	}

	sshFallback := func() ([]byte, error) {
		cmd := convoGated(req.Gate, req.Mode, req.Session, req.Pane, req.Socket, req.PID, req.Start, req.SID, lenSpec, req.Keys, readLines)
		res, err := sshx.Exec(ctx, sess.Client(), cmd, maxOut, sshx.ExecOptions{
			Stdin:       strings.NewReader(req.Text),
			Release:     release,
			Interactive: true,
			AwaitSlot:   req.AwaitSlot,
		})
		if req.What != "" {
			s.perfGatedVia(req.What, "ssh", t0, err, sess.HostID)
		}
		return res.Out, err
	}

	corraldDone := func(out []byte, err error) ([]byte, error) {
		release()
		if req.What != "" {
			s.perfGatedVia(req.What, "corrald", t0, err, sess.HostID)
		}
		return out, err
	}

	if s == nil || sess == nil || sess.HostID <= 0 {
		return sshFallback()
	}

	capName := gatedCapName(req.Gate)
	cl, ok := s.CorraldClient(sess.OwnerID, sess.HostID, capName)
	if !ok || cl == nil || cl.Closed() {
		return sshFallback()
	}

	params := proto.PaneSendParams{
		OpID:    gw.NewWriteOpID(),
		Mode:    req.Mode,
		Session: req.Session,
		Pane:    req.Pane,
		Socket:  req.Socket,
		PID:     req.PID,
		Start:   req.Start,
		SID:     req.SID,
		LenSpec: lenSpec,
		Keys:    req.Keys,
		Read:    readLines,
		Gate:    req.Gate,
	}
	if lenSpec != "keys" {
		params.Text = []byte(req.Text)
	}

	retryDelay := gatedHandoffRetryBase
	if retryDelay <= 0 {
		retryDelay = 100 * time.Millisecond
	}
	maxDelay := 5 * retryDelay
	inProgressEpoch := ""

	for {
		ep, _ := cl.Cursor()
		if inProgressEpoch != "" && ep != inProgressEpoch {
			if s.corrald != nil {
				s.corrald.ReportCapFailure(sess.HostID, cl, capName, gw.ErrUnknownResult)
			}
			return corraldDone(nil, gw.ErrUnknownResult)
		}

		res, err := cl.PaneSend(ctx, params)
		if err == nil {
			if s.corrald != nil {
				s.corrald.ReportCapSuccess(sess.HostID, cl, capName)
			}
			out := res.Out
			if len(out) > maxOut {
				out = out[:maxOut]
			}
			if res.Exit != 0 {
				return corraldDone(out, &sshx.ExitError{Status: res.Exit})
			}
			return corraldDone(out, nil)
		}

		var rpcErr *proto.RPCError
		if errors.As(err, &rpcErr) {
			switch rpcErr.Code {
			case proto.ErrHandoffInProgress:
				// Rejected cleanly before admitting op_id during drain; retry
				// with the SAME op_id within the caller's original deadline
				// (never fall back to SSH).
				if inProgressEpoch != "" {
					if s.corrald != nil {
						s.corrald.ReportCapFailure(sess.HostID, cl, capName, gw.ErrUnknownResult)
					}
					return corraldDone(nil, gw.ErrUnknownResult)
				}
				if !gatedSleep(ctx, retryDelay) {
					return corraldDone(nil, ctx.Err())
				}
				if retryDelay < maxDelay {
					retryDelay += gatedHandoffRetryBase
				}
				for {
					nextCl, nextOK := s.CorraldClient(sess.OwnerID, sess.HostID, capName)
					if nextOK && nextCl != nil && !nextCl.Closed() {
						cl = nextCl
						break
					}
					if !gatedSleep(ctx, gatedHandoffRetryBase) {
						return corraldDone(nil, ctx.Err())
					}
				}
				continue

			case proto.ErrInProgress:
				if inProgressEpoch == "" {
					inProgressEpoch = ep
				}
				if !gatedSleep(ctx, gatedInProgressPollInterval) {
					return corraldDone(nil, fmt.Errorf("%w: %v", gw.ErrUnknownResult, ctx.Err()))
				}
				nextCl, nextOK := s.CorraldClient(sess.OwnerID, sess.HostID, capName)
				if !nextOK || nextCl == nil || nextCl.Closed() {
					return corraldDone(nil, gw.ErrUnknownResult)
				}
				cl = nextCl
				continue

			default:
				if inProgressEpoch == "" && proto.IsPreExecutionRejectionCode(rpcErr.Code) {
					if s.corrald != nil {
						s.corrald.ReportCapFailure(sess.HostID, cl, capName, err)
					}
					return sshFallback()
				}
				// Every non-whitelisted RPC error (internal_error, panic,
				// unknown code, etc.) may have occurred after partial or full
				// execution -> ErrUnknownResult (409), NEVER fall back to SSH.
				if s.corrald != nil {
					s.corrald.ReportCapFailure(sess.HostID, cl, capName, err)
				}
				return corraldDone(nil, fmt.Errorf("%w: %w", gw.ErrUnknownResult, err))
			}
		}

		// Whitelist-only SSH fallback: ONLY errors proven to have failed BEFORE
		// the request frame began writing on the wire (gw.ErrNotSent) may fall
		// back to SSH (and never if the caller's ctx already expired or an
		// earlier attempt was already in_progress).
		if inProgressEpoch == "" && errors.Is(err, gw.ErrNotSent) {
			if ctx.Err() != nil {
				return corraldDone(nil, ctx.Err())
			}
			if s.corrald != nil {
				s.corrald.ReportCapFailure(sess.HostID, cl, capName, err)
			}
			return sshFallback()
		}

		// Everything else (decode errors, write errors, disconnects, timeouts,
		// op_expired, op_unknown, unknown errors) -> ErrUnknownResult (409),
		// NEVER fall back to SSH and NEVER retry with a new op_id.
		if s.corrald != nil {
			s.corrald.ReportCapFailure(sess.HostID, cl, capName, err)
		}
		return corraldDone(nil, fmt.Errorf("%w: %w", gw.ErrUnknownResult, err))
	}
}
