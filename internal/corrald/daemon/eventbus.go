package daemon

import (
	"encoding/json"
	"sync"

	"github.com/ihainan/corral/internal/corrald/proto"
)

// DefaultEventRingSize is the bounded replay buffer capacity from spec §5.
const DefaultEventRingSize = 1000

// EventSubscriber receives live events pushed by EventBus. Deliver must not
// block indefinitely; returning false signals the subscriber has been detached.
type EventSubscriber interface {
	DeliverEvent(ev proto.Event) bool
}

// EventBus assigns monotonically increasing sequence numbers under a daemon
// epoch, keeps a bounded ring buffer of recent events for daemon.resume, and
// fans out live events to active subscribers.
type EventBus struct {
	mu       sync.Mutex
	epoch    string
	seq      uint64
	capacity int
	ring     []proto.Event
	subs     map[EventSubscriber]struct{}
}

// NewEventBus creates a fresh EventBus with a new random epoch.
func NewEventBus(capacity int) *EventBus {
	if capacity <= 0 {
		capacity = DefaultEventRingSize
	}
	return &EventBus{
		epoch:    RandomHex(16),
		capacity: capacity,
		ring:     make([]proto.Event, 0, capacity),
		subs:     make(map[EventSubscriber]struct{}),
	}
}

// Epoch returns the bus's current daemon epoch identifier.
func (b *EventBus) Epoch() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.epoch
}

// CurrentSeq returns the highest emitted sequence number (0 if none).
func (b *EventBus) CurrentSeq() uint64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.seq
}

// Emit appends a new event to the ring buffer and delivers it to all active
// subscribers in sequence order.
func (b *EventBus) Emit(eventName string, data any) (proto.Event, error) {
	var raw json.RawMessage
	switch v := data.(type) {
	case nil:
	case json.RawMessage:
		raw = cloneRaw(v)
	case []byte:
		raw = cloneRaw(v)
	default:
		encoded, err := json.Marshal(v)
		if err != nil {
			return proto.Event{}, err
		}
		raw = encoded
	}

	b.mu.Lock()
	b.seq++
	ev := proto.Event{
		Event: eventName,
		Epoch: b.epoch,
		Seq:   b.seq,
		Data:  raw,
	}
	if len(b.ring) == b.capacity {
		copy(b.ring, b.ring[1:])
		b.ring[len(b.ring)-1] = ev
	} else {
		b.ring = append(b.ring, ev)
	}
	for sub := range b.subs {
		if !sub.DeliverEvent(ev) {
			delete(b.subs, sub)
		}
	}
	b.mu.Unlock()
	return ev, nil
}

// Subscribe attaches sub to receive future live events without replay.
func (b *EventBus) Subscribe(sub EventSubscriber) uint64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.subs[sub] = struct{}{}
	return b.seq
}

// Unsubscribe detaches sub from live event delivery.
func (b *EventBus) Unsubscribe(sub EventSubscriber) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.subs, sub)
}

// Resume validates (epoch, lastSeq), collects all missed events in
// (lastSeq, currentSeq], invokes onReplayLocked while holding the bus lock so
// the caller can enqueue its RPC response and replayed frames atomically before
// any newer live event, and registers sub for subsequent events.
//
// If epoch differs or the ring buffer no longer contains lastSeq+1, Resume
// returns ErrResyncRequired without invoking onReplayLocked.
func (b *EventBus) Resume(
	epoch string,
	lastSeq uint64,
	sub EventSubscriber,
	onReplayLocked func(replayed []proto.Event, currentSeq uint64),
) (replayedCount int, currentSeq uint64, rpcErr *proto.RPCError) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if epoch == "" || epoch != b.epoch {
		return 0, b.seq, &proto.RPCError{
			Code:    proto.ErrResyncRequired,
			Message: "daemon epoch changed; full resync required",
		}
	}
	if lastSeq > b.seq {
		return 0, b.seq, &proto.RPCError{
			Code:    proto.ErrResyncRequired,
			Message: "last_seq ahead of daemon sequence; full resync required",
		}
	}

	var missed []proto.Event
	if lastSeq < b.seq {
		if len(b.ring) == 0 || b.ring[0].Seq > lastSeq+1 {
			return 0, b.seq, &proto.RPCError{
				Code:    proto.ErrResyncRequired,
				Message: "requested sequence evicted from replay buffer; full resync required",
			}
		}
		for _, ev := range b.ring {
			if ev.Seq > lastSeq {
				missed = append(missed, ev)
			}
		}
	}

	if onReplayLocked != nil {
		onReplayLocked(missed, b.seq)
	}
	if sub != nil {
		b.subs[sub] = struct{}{}
	}
	return len(missed), b.seq, nil
}

// Snapshot returns the epoch, current sequence number, and a copy of the ring
// buffer for hot-update handoff.
func (b *EventBus) Snapshot() (epoch string, seq uint64, events []proto.Event) {
	b.mu.Lock()
	defer b.mu.Unlock()
	cp := make([]proto.Event, len(b.ring))
	copy(cp, b.ring)
	return b.epoch, b.seq, cp
}

// Restore installs epoch, seq, and ring buffer from a hot-update handoff
// snapshot. If epoch is empty, Restore keeps the freshly generated epoch.
func (b *EventBus) Restore(epoch string, seq uint64, events []proto.Event) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if epoch == "" {
		return
	}
	b.epoch = epoch
	b.seq = seq
	if len(events) > b.capacity {
		events = events[len(events)-b.capacity:]
	}
	b.ring = make([]proto.Event, len(events), b.capacity)
	copy(b.ring, events)
}
