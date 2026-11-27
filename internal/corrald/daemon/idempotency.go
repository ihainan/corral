package daemon

import (
	"encoding/json"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/ihainan/corral/internal/corrald/proto"
)

const (
	defaultOpTableCapacity = 1000
	defaultOpTableTTL      = 30 * time.Minute
)

// CompletedOp is the serializable record of an executed idempotent write
// operation, preserved across hot-update handoffs.
type CompletedOp struct {
	OpID        string          `json:"op_id"`
	Result      json.RawMessage `json:"result,omitempty"`
	Error       *proto.RPCError `json:"error,omitempty"`
	CompletedAt time.Time       `json:"completed_at"`
}

type opEntry struct {
	inProgress  bool
	result      json.RawMessage
	rpcErr      *proto.RPCError
	completedAt time.Time
}

// OpTable guarantees at-most-once execution for mutating RPCs keyed by op_id.
// Duplicate requests arriving while an operation is still running receive a
// retryable in_progress error; duplicates arriving after completion receive
// the cached first result without re-running the operation.
type OpTable struct {
	mu       sync.Mutex
	entries  map[string]*opEntry
	capacity int
	ttl      time.Duration
	nowFn    func() time.Time
}

// NewOpTable creates an OpTable with bounded retention.
func NewOpTable(capacity int, ttl time.Duration) *OpTable {
	if capacity <= 0 {
		capacity = defaultOpTableCapacity
	}
	if ttl <= 0 {
		ttl = defaultOpTableTTL
	}
	return &OpTable{
		entries:  make(map[string]*opEntry),
		capacity: capacity,
		ttl:      ttl,
		nowFn:    time.Now,
	}
}

// Execute runs fn at most once for opID. Empty opID is rejected with
// invalid_params.
func (t *OpTable) Execute(opID string, fn func() (json.RawMessage, *proto.RPCError)) (json.RawMessage, *proto.RPCError) {
	if opID == "" {
		return nil, &proto.RPCError{
			Code:    proto.ErrInvalidParams,
			Message: "missing op_id",
		}
	}

	t.mu.Lock()
	t.pruneLocked(t.nowFn())
	if existing, ok := t.entries[opID]; ok {
		if existing.inProgress {
			t.mu.Unlock()
			return nil, &proto.RPCError{
				Code:      proto.ErrInProgress,
				Message:   "operation already in progress",
				Retryable: true,
			}
		}
		res := cloneRaw(existing.result)
		errCopy := cloneRPCError(existing.rpcErr)
		t.mu.Unlock()
		return res, errCopy
	}
	entry := &opEntry{inProgress: true}
	t.entries[opID] = entry
	t.mu.Unlock()

	var (
		res    json.RawMessage
		rpcErr *proto.RPCError
	)
	func() {
		defer func() {
			if r := recover(); r != nil {
				rpcErr = &proto.RPCError{
					Code:    proto.ErrInternal,
					Message: fmt.Sprintf("operation panic: %v", r),
				}
			}
		}()
		res, rpcErr = fn()
	}()

	t.mu.Lock()
	entry.inProgress = false
	entry.result = cloneRaw(res)
	entry.rpcErr = cloneRPCError(rpcErr)
	entry.completedAt = t.nowFn()
	t.pruneLocked(entry.completedAt)
	t.mu.Unlock()

	return cloneRaw(res), cloneRPCError(rpcErr)
}

// Snapshot returns all completed operations for hot-update handoff.
func (t *OpTable) Snapshot() []CompletedOp {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.pruneLocked(t.nowFn())
	out := make([]CompletedOp, 0, len(t.entries))
	for id, e := range t.entries {
		if e.inProgress {
			continue
		}
		out = append(out, CompletedOp{
			OpID:        id,
			Result:      cloneRaw(e.result),
			Error:       cloneRPCError(e.rpcErr),
			CompletedAt: e.completedAt,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].CompletedAt.Before(out[j].CompletedAt)
	})
	return out
}

// Restore loads completed operations from a handoff snapshot.
func (t *OpTable) Restore(ops []CompletedOp) {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.nowFn()
	for _, op := range ops {
		if op.OpID == "" {
			continue
		}
		if now.Sub(op.CompletedAt) > t.ttl {
			continue
		}
		t.entries[op.OpID] = &opEntry{
			inProgress:  false,
			result:      cloneRaw(op.Result),
			rpcErr:      cloneRPCError(op.Error),
			completedAt: op.CompletedAt,
		}
	}
	t.pruneLocked(now)
}

func (t *OpTable) pruneLocked(now time.Time) {
	var completed []struct {
		id string
		at time.Time
	}
	for id, e := range t.entries {
		if e.inProgress {
			continue
		}
		if now.Sub(e.completedAt) > t.ttl {
			delete(t.entries, id)
			continue
		}
		completed = append(completed, struct {
			id string
			at time.Time
		}{id: id, at: e.completedAt})
	}
	if len(completed) <= t.capacity {
		return
	}
	sort.Slice(completed, func(i, j int) bool {
		return completed[i].at.Before(completed[j].at)
	})
	excess := len(completed) - t.capacity
	for i := 0; i < excess; i++ {
		delete(t.entries, completed[i].id)
	}
}

func cloneRaw(b json.RawMessage) json.RawMessage {
	if b == nil {
		return nil
	}
	cp := make(json.RawMessage, len(b))
	copy(cp, b)
	return cp
}

func cloneRPCError(e *proto.RPCError) *proto.RPCError {
	if e == nil {
		return nil
	}
	cp := *e
	return &cp
}
