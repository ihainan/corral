package web

import (
	"bytes"
	"context"
	"errors"
	"time"

	"github.com/ihainan/corral/internal/corrald/proto"
	"github.com/ihainan/corral/internal/sshx"
	"github.com/ihainan/corral/internal/term"
)

var errCorraldTmuxEmpty = errors.New("corrald agent.locate returned empty tmux info")

// maxCorraldRPCBudget caps the portion of a request's remaining deadline spent
// waiting on corrald before falling back to the original SSH script (C-5).
var maxCorraldRPCBudget = 3 * time.Second

func corraldBudgetDuration(ctx context.Context) time.Duration {
	budget := maxCorraldRPCBudget
	if dl, ok := ctx.Deadline(); ok {
		rem := time.Until(dl)
		if rem <= 0 {
			return 0
		}
		if half := rem / 2; half < budget {
			budget = half
		}
	}
	return budget
}

func corraldCallBudget(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, corraldBudgetDuration(ctx))
}

// tryCorraldLocate attempts to satisfy a locate scan via corrald's agent.locate
// RPC through the unified Server.CorraldClient entrypoint (spec §9).
// Returns (out, true) on success, or (nil, false) when the caller must fall
// back immediately to the original SSH locate script.
func (s *Server) tryCorraldLocate(ctx context.Context, sess *term.Session, params proto.AgentLocateParams) ([]byte, bool) {
	return s.tryCorraldLocateBudget(ctx, sess, params, corraldBudgetDuration(ctx))
}

func (s *Server) tryCorraldLocateBudget(ctx context.Context, sess *term.Session, params proto.AgentLocateParams, budget time.Duration) ([]byte, bool) {
	if s == nil || sess == nil || sess.HostID <= 0 {
		return nil, false
	}
	cl, ok := s.CorraldClient(sess.OwnerID, sess.HostID, "locate")
	if !ok {
		return nil, false
	}
	rpcCtx, cancel := context.WithTimeout(ctx, budget)
	out, err := cl.Locate(rpcCtx, params)
	cancel()
	if err != nil {
		if s.corrald != nil && ctx.Err() == nil {
			s.corrald.ReportCapFailure(sess.HostID, cl, "locate", err)
		}
		return nil, false
	}
	if locateTmuxEmpty(params, out) {
		if s.corrald != nil {
			s.corrald.ReportCapFailure(sess.HostID, cl, "locate", errCorraldTmuxEmpty)
		}
		return nil, false
	}
	if s.corrald != nil {
		s.corrald.ReportCapSuccess(sess.HostID, cl, "locate")
	}
	if params.Times {
		out = appendHostNowRecord(out)
	}
	return out, true
}

// runConvoScan executes a deduplicated host locate scan via s.convoScans,
// preferring corrald agent.locate when available and falling back to the
// original SSH script cmd while keeping the convoScans cache key unchanged.
func (s *Server) runConvoScan(ctx context.Context, sess *term.Session, params proto.AgentLocateParams, cmd string, after time.Time) ([]byte, time.Time, time.Time, bool, error) {
	// Capture the caller's RPC time budget BEFORE convoScans detaches ctx via
	// context.WithoutCancel so a hung corrald still leaves time for SSH fallback
	// within the caller's original deadline (C-5).
	budget := corraldBudgetDuration(ctx)
	return s.convoScans.runWith(ctx, sess, cmd, after, func(sctx context.Context) ([]byte, bool) {
		return s.tryCorraldLocateBudget(sctx, sess, params, budget)
	})
}

// tryCorraldTails attempts to read log tails via corrald's convo.tails RPC.
func (s *Server) tryCorraldTails(ctx context.Context, sess *term.Session, logs []string) ([]byte, bool) {
	return s.tryCorraldTailsWith(ctx, sess, logs, false)
}

// tryCorraldMessageTails attempts to read filtered message tails via corrald's
// convo.message_tails RPC.
func (s *Server) tryCorraldMessageTails(ctx context.Context, sess *term.Session, logs []string) ([]byte, bool) {
	return s.tryCorraldTailsWith(ctx, sess, logs, true)
}

func (s *Server) tryCorraldTailsWith(ctx context.Context, sess *term.Session, logs []string, messageOnly bool) ([]byte, bool) {
	if s == nil || sess == nil || sess.HostID <= 0 || len(logs) == 0 {
		return nil, false
	}
	cl, ok := s.CorraldClient(sess.OwnerID, sess.HostID, "tails")
	if !ok {
		return nil, false
	}
	rpcCtx, cancel := corraldCallBudget(ctx)
	var out []byte
	var err error
	if messageOnly {
		out, err = cl.MessageTails(rpcCtx, logs)
	} else {
		out, err = cl.Tails(rpcCtx, logs)
	}
	cancel()
	if err != nil {
		if s.corrald != nil && ctx.Err() == nil {
			s.corrald.ReportCapFailure(sess.HostID, cl, "tails", err)
		}
		return nil, false
	}
	if s.corrald != nil {
		s.corrald.ReportCapSuccess(sess.HostID, cl, "tails")
	}
	if len(splitTails(string(out), logs)) != len(logs) {
		return nil, false
	}
	return out, true
}

// convoTailsOutput reads raw log tails via corrald convo.tails when available,
// falling back immediately to convoTailsCommand over SSH.
func (s *Server) convoTailsOutput(ctx context.Context, sess *term.Session, logs []string, maxBytes int) ([]byte, error) {
	if out, ok := s.tryCorraldTails(ctx, sess, logs); ok {
		return out, nil
	}
	return sshx.Output(ctx, sess.Client(), convoTailsCommand(logs), maxBytes)
}

func locateTmuxEmpty(params proto.AgentLocateParams, out []byte) bool {
	if params.Mode != "tmux" {
		return false
	}
	foundTmux := false
	foundPaneTTY := params.TmuxPane == ""
	for _, rec := range bytes.Split(out, []byte{0}) {
		if val, ok := bytes.CutPrefix(rec, []byte("tmux=")); ok {
			if len(bytes.TrimSpace(val)) > 0 {
				foundTmux = true
			}
		} else if params.TmuxPane != "" {
			if val, ok := bytes.CutPrefix(rec, []byte("panetty=")); ok {
				if len(bytes.TrimSpace(val)) > 0 {
					foundPaneTTY = true
				}
			}
		}
	}
	return !foundTmux || !foundPaneTTY
}

func appendHostNowRecord(out []byte) []byte {
	for _, rec := range bytes.Split(out, []byte{0}) {
		if val, ok := bytes.CutPrefix(rec, []byte("hostnow=")); ok && len(val) > 0 {
			res := make([]byte, 0, len(out)+len("now=")+len(val)+1)
			res = append(res, out...)
			res = append(res, "now="...)
			res = append(res, val...)
			res = append(res, 0)
			return res
		}
	}
	return out
}
