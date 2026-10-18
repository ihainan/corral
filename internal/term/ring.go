package term

// ring keeps the most recent output of a terminal, addressed by absolute byte
// offset since the session started. A reconnecting browser asks for "everything
// after offset N"; if N is still in the buffer it gets an exact continuation.
type ring struct {
	cap   int
	data  []byte
	start uint64 // absolute offset of data[0]
}

func newRing(capacity int) *ring { return &ring{cap: capacity} }

func (r *ring) end() uint64 { return r.start + uint64(len(r.data)) }

func (r *ring) write(p []byte) {
	r.data = append(r.data, p...)
	// Trim in batches so we are not copying on every write.
	if len(r.data) > r.cap+r.cap/2 {
		drop := len(r.data) - r.cap
		r.data = append(r.data[:0:0], r.data[drop:]...)
		r.start += uint64(drop)
	}
}

// since returns the bytes after offset. ok is false when offset is older than
// the buffer (the caller must reset the screen and replay what we still have)
// or ahead of it (a client from another session or a server restart).
func (r *ring) since(offset uint64) (out []byte, ok bool) {
	if offset < r.start || offset > r.end() {
		return nil, false
	}
	rel := offset - r.start
	return append([]byte(nil), r.data[rel:]...), true
}

// all returns the whole buffer for a full replay, starting at a line boundary
// when the head has been trimmed so we do not begin inside an escape sequence.
func (r *ring) all() (out []byte, from uint64) {
	data := r.data
	from = r.start
	if r.start > 0 {
		for i, b := range data {
			if b == '\n' {
				data = data[i+1:]
				from = r.start + uint64(i+1)
				break
			}
		}
	}
	return append([]byte(nil), data...), from
}
