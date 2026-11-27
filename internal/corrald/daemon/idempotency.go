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
	defaultOpTableTTL      = proto.OpDedupWindow
)

// OpState values serialized across hot-update handoffs.
const (
	OpStateDone    = "done"
	OpStateUnknown = "unknown" // was still in progress when v1 handed off; outcome unknown
)

// OpSnapshotEntry is the serializable record of a tracked write operation
// preserved across hot-update handoffs.
type OpSnapshotEntry struct {
	OpID        string          `json:"op_id"`
	State       string          `json:"state,omitempty"` // "done" (default) or "unknown"
	Result      json.RawMessage `json:"result,omitempty"`
	Error       *proto.RPCError `json:"error,omitempty"`
	CreatedAt   time.Time       `json:"created_at,omitempty"`
	CompletedAt time.Time       `json:"completed_at"`
}

// OpTableSnapshot captures all tracked op_id records plus the eviction
// watermark so post-handoff retries of evicted or in-flight ops are never
// re-executed.
type OpTableSnapshot struct {
	Entries           []OpSnapshotEntry `json:"entries,omitempty"`
	MinValidCreatedAt time.Time         `json:"min_valid_created_at,omitempty"`
}

type opEntry struct {
	inProgress  bool
	unknown     bool
	result      json.RawMessage
	rpcErr      *proto.RPCError
	createdAt   time.Time
	completedAt time.Time
}

// OpTable guarantees at-most-once execution for mutating RPCs keyed by a
// protocol-formatted timestamped op_id ("<unix_ms>-<token>").
//
//   - Invalid/missing op_id -> invalid_params.
//   - op_id older than the dedup window (or older than the capacity-eviction
//     watermark) -> op_expired (never re-executed).
//   - op_id currently executing -> retryable in_progress.
//   - op_id inherited in-flight across handoff -> op_unknown (never re-executed).
//   - op_id already completed -> returns cached first result without re-running fn.
type OpTable struct {
	mu                sync.Mutex
	entries           map[string]*opEntry
	capacity          int
	ttl               time.Duration
	minValidCreatedAt time.Time
	nowFn             func() time.Time
}

// NewOpTable creates an OpTable with bounded retention and strict window
// enforcement.
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

// Execute runs fn at most once for opID.
func (t *OpTable) Execute(opID string, fn func() (json.RawMessage, *proto.RPCError)) (json.RawMessage, *proto.RPCError) {
	createdAt, err := proto.ParseOpID(opID)
	if err != nil {
		return nil, &proto.RPCError{
			Code:    proto.ErrInvalidParams,
			Message: "invalid or missing op_id (expected <unix_ms>-<token>)",
		}
	}

	t.mu.Lock()
	now := t.nowFn().UTC()
	t.pruneLocked(now)

	if existing, ok := t.entries[opID]; ok {
		if existing.inProgress {
			t.mu.Unlock()
			return nil, &proto.RPCError{
				Code:      proto.ErrInProgress,
				Message:   "operation already in progress",
				Retryable: true,
			}
		}
		if existing.unknown {
			t.mu.Unlock()
			return nil, &proto.RPCError{
				Code:    proto.ErrOpUnknown,
				Message: "operation was in progress during handoff; outcome unknown",
			}
		}
		res := cloneRaw(existing.result)
		errCopy := cloneRPCError(existing.rpcErr)
		t.mu.Unlock()
		return res, errCopy
	}

	// Reject op_ids older than the protocol deduplication window or older than
	// any record evicted by capacity pressure so an evicted op can never be
	// silently re-executed.
	if now.Sub(createdAt) > t.ttl {
		t.mu.Unlock()
		return nil, &proto.RPCError{
			Code:    proto.ErrOpExpired,
			Message: "op_id is older than the deduplication window",
		}
	}
	if !t.minValidCreatedAt.IsZero() && !createdAt.After(t.minValidCreatedAt) {
		t.mu.Unlock()
		return nil, &proto.RPCError{
			Code:    proto.ErrOpExpired,
			Message: "op_id predates deduplication retention watermark",
		}
	}
	if createdAt.Sub(now) > proto.OpMaxClockSkew {
		t.mu.Unlock()
		return nil, &proto.RPCError{
			Code:    proto.ErrInvalidParams,
			Message: "op_id timestamp is too far in the future",
		}
	}

	entry := &opEntry{
		inProgress: true,
		createdAt:  createdAt,
	}
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
	entry.completedAt = t.nowFn().UTC()
	t.pruneLocked(entry.completedAt)
	t.mu.Unlock()

	return cloneRaw(res), cloneRPCError(rpcErr)
}

// Snapshot returns all completed AND currently in-flight operations (marked
// state="unknown") along with the eviction watermark for hot-update handoff.
// Including in-flight operations guarantees that a write exceeding the drain
// window on v1 cannot be re-executed on v2.
func (t *OpTable) Snapshot() OpTableSnapshot {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.nowFn().UTC()
	t.pruneLocked(now)
	out := make([]OpSnapshotEntry, 0, len(t.entries))
	for id, e := range t.entries {
		if e.inProgress || e.unknown {
			completedAt := e.completedAt
			if completedAt.IsZero() {
				completedAt = now
			}
			out = append(out, OpSnapshotEntry{
				OpID:        id,
				State:       OpStateUnknown,
				CreatedAt:   e.createdAt,
				CompletedAt: completedAt,
			})
			continue
		}
		out = append(out, OpSnapshotEntry{
			OpID:        id,
			State:       OpStateDone,
			Result:      cloneRaw(e.result),
			Error:       cloneRPCError(e.rpcErr),
			CreatedAt:   e.createdAt,
			CompletedAt: e.completedAt,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CompletedAt.Equal(out[j].CompletedAt) {
			return out[i].CompletedAt.Before(out[j].CompletedAt)
		}
		return out[i].OpID < out[j].OpID
	})
	return OpTableSnapshot{
		Entries:           out,
		MinValidCreatedAt: t.minValidCreatedAt,
	}
}

// Restore loads tracked operations and eviction watermark from a handoff
// snapshot.
func (t *OpTable) Restore(snap OpTableSnapshot) {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.nowFn().UTC()
	if snap.MinValidCreatedAt.After(t.minValidCreatedAt) {
		t.minValidCreatedAt = snap.MinValidCreatedAt
	}
	for _, op := range snap.Entries {
		if op.OpID == "" {
			continue
		}
		createdAt := op.CreatedAt
		if createdAt.IsZero() {
			if parsed, err := proto.ParseOpID(op.OpID); err == nil {
				createdAt = parsed
			}
		}
		if !createdAt.IsZero() && now.Sub(createdAt) > t.ttl {
			continue
		}
		if createdAt.IsZero() && now.Sub(op.CompletedAt) > t.ttl {
			continue
		}
		t.entries[op.OpID] = &opEntry{
			inProgress:  false,
			unknown:     op.State == OpStateUnknown,
			result:      cloneRaw(op.Result),
			rpcErr:      cloneRPCError(op.Error),
			createdAt:   createdAt,
			completedAt: op.CompletedAt,
		}
	}
	t.pruneLocked(now)
}

func (t *OpTable) pruneLocked(now time.Time) {
	type candidate struct {
		id        string
		createdAt time.Time
		at        time.Time
	}
	var retained []candidate
	for id, e := range t.entries {
		if e.inProgress {
			continue
		}
		refTime := e.createdAt
		if refTime.IsZero() {
			refTime = e.completedAt
		}
		if now.Sub(refTime) > t.ttl {
			delete(t.entries, id)
			continue
		}
		retained = append(retained, candidate{
			id:        id,
			createdAt: refTime,
			at:        e.completedAt,
		})
	}
	if len(retained) <= t.capacity {
		return
	}
	// Evict by oldest createdAt so advancing minValidCreatedAt never creates
	// gaps that could admit an evicted op_id while keeping newer ones.
	sort.Slice(retained, func(i, j int) bool {
		if !retained[i].createdAt.Equal(retained[j].createdAt) {
			return retained[i].createdAt.Before(retained[j].createdAt)
		}
		return retained[i].at.Before(retained[j].at)
	})
	excess := len(retained) - t.capacity
	for i := 0; i < excess; i++ {
		if retained[i].createdAt.After(t.minValidCreatedAt) {
			t.minValidCreatedAt = retained[i].createdAt
		}
		delete(t.entries, retained[i].id)
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
