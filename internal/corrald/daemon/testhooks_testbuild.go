//go:build corrald_test

package daemon

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"strconv"
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

func defaultLifecycleLockTimeout() time.Duration {
	if v := os.Getenv("CORRALD_TEST_LOCK_TIMEOUT"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return DefaultLifecycleLockTimeout
}

type blockingLogWriter struct {
	inner    io.Writer
	substr   string
	gateFile string
}

func (b *blockingLogWriter) Write(p []byte) (int, error) {
	if len(p) > 0 && containsSubstr(string(p), b.substr) {
		if b.gateFile == "" {
			select {} // intentionally block forever to test Rule 3 hard-exit timer
		}
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			if _, err := os.Stat(b.gateFile); err == nil {
				break
			}
			time.Sleep(15 * time.Millisecond)
		}
	}
	return b.inner.Write(p)
}

func containsSubstr(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
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
	if v := os.Getenv("CORRALD_TEST_LOCK_TIMEOUT"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			cfg.LockTimeout = d
		}
	}
	if os.Getenv("CORRALD_TEST_BLOCK_LOG_ON_HANDOFF") == "1" {
		cfg.Logger = NewLogger(&blockingLogWriter{inner: io.Discard, substr: "handoff completed"})
	}
	if os.Getenv("CORRALD_TEST_BLOCK_LOG_ON_ROLLBACK") == "1" {
		cfg.Logger = NewLogger(&blockingLogWriter{
			inner:    io.Discard,
			substr:   "handoff failed",
			gateFile: os.Getenv("CORRALD_TEST_BLOCK_LOG_GATE"),
		})
	}
}

func shouldFailBeforeReady(handoff bool) bool {
	return handoff && os.Getenv("CORRALD_TEST_FAIL_READY") == "1"
}

func shouldFailCutover() bool {
	return os.Getenv("CORRALD_TEST_FAIL_CUTOVER") == "1"
}

func waitCutoverTestGate() {
	if marker := os.Getenv("CORRALD_TEST_CUTOVER_MARKER"); marker != "" {
		_ = os.WriteFile(marker, []byte(strconv.Itoa(os.Getpid())), 0o600)
	}
	if gate := os.Getenv("CORRALD_TEST_CUTOVER_GATE"); gate != "" {
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			if _, err := os.Stat(gate); err == nil {
				return
			}
			time.Sleep(15 * time.Millisecond)
		}
	}
}

func registerTestMethods(s *Server) {
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
			time.Sleep(time.Duration(p.DelayMS) * time.Millisecond)
		}
		return map[string]any{
			"op_id": p.OpID,
			"value": p.Value,
			"pid":   os.Getpid(),
		}, nil
	})
}
