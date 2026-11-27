//go:build corrald_test

package daemon

import (
	"context"
	"encoding/json"
	"os"
	"time"

	"github.com/ihainan/corral/internal/corrald/proto"
)

// Test-build-only hooks (compiled only with -tags corrald_test, never included
// in production corrald binaries).

func defaultPathOptions() PathOptions {
	opts := PathOptions{}
	if mid := os.Getenv("CORRALD_TEST_MACHINE_ID"); mid != "" {
		opts.MachineID = mid
		opts.MachineIDSource = MachineIDSourceOverride
	}
	return opts
}

func applyTestServerConfig(cfg *ServerConfig) {
	if v := os.Getenv("CORRALD_TEST_IDLE_TIMEOUT"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			cfg.IdleTimeout = d
		}
	}
	if v := os.Getenv("CORRALD_TEST_OLD_GC_DELAY"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			cfg.OldVersionGCDelay = d
		}
	}
	if v := os.Getenv("CORRALD_TEST_READY_TIMEOUT"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			cfg.ReadyTimeout = d
		}
	}
	if v := os.Getenv("CORRALD_TEST_DRAIN_TIMEOUT"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			cfg.DrainTimeout = d
		}
	}
	if v := os.Getenv("CORRALD_TEST_POST_HANDOFF_WRITE_CAP"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			cfg.PostHandoffWriteCap = d
		}
	}
	if v := os.Getenv("CORRALD_TEST_POST_HANDOFF_EXIT"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			cfg.PostHandoffExit = d
		}
	}
}

func shouldFailBeforeReady(handoff bool) bool {
	return handoff && os.Getenv("CORRALD_TEST_FAIL_READY") == "1"
}

func shouldFailCutover() bool {
	return os.Getenv("CORRALD_TEST_FAIL_CUTOVER") == "1"
}

func registerTestMethods(s *Server) {
	// test.emit emits a synthetic event on the daemon's EventBus. Marked
	// Mutating so emissions drain cleanly before a hot-update snapshot.
	s.Register("test.emit", MethodOptions{Mutating: true}, func(_ context.Context, _ *Conn, raw json.RawMessage) (any, *proto.RPCError) {
		var p struct {
			Event string          `json:"event"`
			Data  json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, &proto.RPCError{Code: proto.ErrInvalidParams, Message: err.Error()}
		}
		if p.Event == "" {
			p.Event = "test.tick"
		}
		ev, err := s.Bus().Emit(p.Event, p.Data)
		if err != nil {
			return nil, &proto.RPCError{Code: proto.ErrInternal, Message: err.Error()}
		}
		return ev, nil
	})

	// test.write uses MethodOptions{Mutating: true, Idempotent: true} so
	// dispatchRequest atomically admits and registers op_id under stateMu BEFORE
	// this handler even starts executing.
	s.Register("test.write", MethodOptions{Mutating: true, Idempotent: true}, func(_ context.Context, _ *Conn, raw json.RawMessage) (any, *proto.RPCError) {
		var p struct {
			OpID    string `json:"op_id"`
			Value   string `json:"value"`
			DelayMS int    `json:"delay_ms"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, &proto.RPCError{Code: proto.ErrInvalidParams, Message: err.Error()}
		}
		if p.DelayMS > 0 {
			// Deliberately sleep without honoring ctx to test hard exit cap
			// and timed-out write handoff behavior.
			time.Sleep(time.Duration(p.DelayMS) * time.Millisecond)
		}
		return map[string]any{
			"op_id": p.OpID,
			"value": p.Value,
			"pid":   os.Getpid(),
		}, nil
	})
}
