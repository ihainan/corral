package proto

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// OpDedupWindow is the protocol-defined deduplication window for mutating RPC
// op_id values within a single daemon epoch.
//
// Deduplication scope contract (enforced jointly by corrald and the gateway):
//   - Within the same daemon epoch (including across clean zero-gap hot updates
//     that preserve epoch and transfer the OpTable and eviction watermark),
//     corrald guarantees at-most-once execution per op_id: duplicates return
//     in_progress, op_unknown (if still in flight across handoff), the cached
//     first result, or op_expired (if older than OpDedupWindow or evicted by
//     capacity pressure).
//   - Across a daemon cold restart or epoch rotation, daemon.hello returns a
//     new epoch. Whenever the gateway reconnects and observes a changed epoch,
//     it MUST treat any previously unacknowledged mutating RPC from the old
//     epoch as outcome-unknown and MUST NOT retry it with the same op_id.
//   - On cold start, corrald begins a fresh epoch with an empty in-memory
//     OpTable and treats any valid non-expired op_id as a new request.
const OpDedupWindow = 30 * time.Minute

// OpMaxClockSkew is the maximum forward clock skew tolerated between the
// gateway that generated an op_id and the target host running corrald.
const OpMaxClockSkew = 2 * time.Minute

var (
	// ErrInvalidOpID indicates that an op_id does not follow the timestamped
	// "<unix_ms>-<token>" format required by the protocol.
	ErrInvalidOpID = errors.New("invalid op_id format")
)

// NewOpID generates a protocol-compliant timestamped op_id ("<unix_ms>-<hex>")
// anchored at now (or time.Now() if now is zero). Gateways should use this
// helper for mutating RPCs (pane.send, pane.answer) so corrald can distinguish
// expired retries from fresh requests.
func NewOpID(now time.Time) string {
	if now.IsZero() {
		now = time.Now()
	}
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(fmt.Sprintf("crypto/rand failed: %v", err))
	}
	return fmt.Sprintf("%d-%s", now.UTC().UnixMilli(), hex.EncodeToString(b[:]))
}

// ParseOpID validates an op_id of the form "<unix_ms>-<token>" and returns its
// embedded UTC creation timestamp.
func ParseOpID(opID string) (time.Time, error) {
	if opID == "" || len(opID) > 96 {
		return time.Time{}, ErrInvalidOpID
	}
	tsStr, suffix, ok := strings.Cut(opID, "-")
	if !ok || tsStr == "" || len(suffix) < 4 || len(suffix) > 64 {
		return time.Time{}, ErrInvalidOpID
	}
	ms, err := strconv.ParseInt(tsStr, 10, 64)
	if err != nil || ms <= 0 {
		return time.Time{}, ErrInvalidOpID
	}
	for i := 0; i < len(suffix); i++ {
		c := suffix[i]
		isDigit := c >= '0' && c <= '9'
		isLower := c >= 'a' && c <= 'z'
		isUpper := c >= 'A' && c <= 'Z'
		if !isDigit && !isLower && !isUpper && c != '_' && c != '-' {
			return time.Time{}, ErrInvalidOpID
		}
	}
	return time.UnixMilli(ms).UTC(), nil
}
