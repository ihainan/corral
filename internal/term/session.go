// Package term owns live terminal sessions.
//
// The gateway, not the browser, holds the SSH connection. Browsers come and go
// (phones suspend tabs within seconds of locking); a session keeps running and
// keeps a window of recent output so a returning browser can catch up exactly.
package term

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"slices"
	"strconv"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
)

const (
	ringSize     = 2 << 20 // recent output kept per session
	subBuffer    = 64      // queued chunks per viewer (≤ 2 MiB) before it is dropped as too slow
	readChunk    = 32 << 10
	inputQueue   = 64       // pending input chunks per session
	inputChunk   = 32 << 10 // so the queue holds at most ~2 MiB
	inputWait    = 10 * time.Second
	maxViewers   = 8 // open sockets per session, of its owner
	maxWatchers  = 4 // and of others watching
	StateRunning = "running"
	StateExited  = "exited" // the remote program ended normally
	StateLost    = "lost"   // the SSH connection dropped
	StateClosed  = "closed" // closed by the user or an administrator
	// The reason of sessions closed because the gateway is stopping (an
	// update): the browser opens tmux and herdr sessions again by itself.
	ReasonShutdown = "server shutting down"
)

var (
	ErrClosed         = errors.New("session closed")
	ErrInputBusy      = errors.New("the host is not reading input")
	ErrTooManyViewers = errors.New("too many open windows for this session")
)

// Chunk is a piece of output and the absolute offset where it starts.
type Chunk struct {
	Offset uint64
	Data   []byte
}

// Event is a state change pushed to viewers after all output before it.
type Event struct {
	State    string
	ExitCode int
	Reason   string
	Offset   uint64 // total output; a viewer has everything once it reached this
}

// Sub is one attached viewer.
type Sub struct {
	C      chan Chunk
	Events chan Event
	Meta   chan []byte // side information (e.g. herdr state); only the latest matters
	// The terminal size whenever it or the viewer that set it changes (and
	// once on subscribing); only the latest matters. Viewers of another size
	// follow it until they take the terminal over by sending their own size.
	Size chan SizeNote
	done chan struct{}
	once sync.Once
	// The browser window this is (one window reconnecting has a new Sub with
	// the same viewer), and the last of this Sub's resize requests handled;
	// both under Session.mu.
	viewer string
	ack    int64
	// watcher is the user watching through this Sub (read-only, by the
	// owner's leave), or 0 for one of the owner's own viewers.
	watcher int64
	leave   *grant // the watcher's leave this Sub was opened under
	first   bool   // the first Sub opened under it

	// Prelude goes before a full replay that does not start with the
	// session's first output: the terminal modes set before it.
	Prelude []byte
}

// grant is one leave to watch. Taking it back closes done, which every
// socket opened under it waits on (whether or not its Sub is still attached).
type grant struct {
	done    chan struct{}
	watched bool // a Sub was opened under it
}

// Revoked is closed once the leave this watcher's Sub was opened under is
// taken back; nil for the owner's own viewers.
func (s *Sub) Revoked() <-chan struct{} {
	if s.leave == nil {
		return nil
	}
	return s.leave.done
}

// FirstWatch reports whether this Sub is the watcher's first under their
// current leave (for the audit log: not every reconnect).
func (s *Sub) FirstWatch() bool { return s.first }

// SizeNote is the terminal size as told to one viewer: whether that viewer
// set it, and how many of its own resize requests had been applied, so a
// note older than the viewer's latest request can be recognised and ignored.
type SizeNote struct {
	Cols, Rows int
	Own        bool
	Ack        int64
}

func (s *Sub) Done() <-chan struct{} { return s.done }
func (s *Sub) close()                { s.once.Do(func() { close(s.done) }) }

type Info struct {
	ID        string `json:"id"`
	HostID    int64  `json:"host_id"`
	HostName  string `json:"host_name"`
	State     string `json:"state"`
	CreatedAt int64  `json:"created_at"`
	Viewers   int    `json:"viewers"`
	Cols      int    `json:"cols"`
	Rows      int    `json:"rows"`
	Title     string `json:"title"`
	Recording bool   `json:"recording"` // its output is being recorded
	// What it runs in: "none" (a plain shell), "tmux" or "herdr", and the
	// tmux or herdr session it is attached to ("" for herdr's default).
	Persistence string `json:"persistence"`
	MuxSession  string `json:"mux_session"`
	Watching    int    `json:"watching"` // windows of other users watching, read-only
	// Side information such as the herdr summary, as last published.
	Meta json.RawMessage `json:"meta,omitempty"`
	// Counts the previews sent (see Preview); 0 if none yet.
	PreviewRev int64 `json:"preview_rev,omitempty"`
}

// Preview is the screen as one of the owner's windows last showed it, as
// text: for the session list, which shows it in small. It is sent by the
// browser (the gateway keeps no screen of its own) and kept in memory only.
type Preview struct {
	Cols  int      `json:"cols"`
	Lines []string `json:"lines"`
	At    int64    `json:"at"`  // unix seconds
	Rev   int64    `json:"rev"` // as PreviewRev in the list
}

type Session struct {
	ID       string
	OwnerID  int64
	HostID   int64
	HostName string
	Created  time.Time
	// The host configuration the session was opened with (see StartOptions).
	HostConfig string
	// What the shell runs in, as in StartOptions; fixed for the session's life.
	Persistence  string
	TmuxSession  string
	HerdrSession string

	client *ssh.Client
	ssh    *ssh.Session
	stdin  io.WriteCloser
	input  chan []byte
	pumps  sync.WaitGroup

	// sizeMu puts changes of the remote terminal size in one order: it is
	// held from recording a size until it was sent, so the host always ends
	// up at the size recorded last. Taken before mu, never while holding it.
	sizeMu  sync.Mutex
	ptySize [2]int                     // cols, rows the host was last sent; under sizeMu
	winch   func(rows, cols int) error // sends a size to the host (ssh.Session.WindowChange)

	mu        sync.Mutex
	buf       *ring
	subs      map[*Sub]struct{}
	state     string
	exitCode  int
	reason    string
	cols      int
	rows      int
	sizeBy    string    // the viewer (window) in charge of the size; "" if none, or it left
	subSeq    int       // numbers viewers that did not name themselves
	idleSince time.Time // when the last viewer left; zero while viewed
	endedAt   time.Time
	title     string
	finished  chan struct{}    // closed when the session reached a final state
	grants    map[int64]*grant // users the owner let watch, read-only

	onEnd   func(*Session)
	ends    *sync.WaitGroup // the manager's: counts onEnd calls under way (see Manager.WaitEnds); nil in tests
	rec     Recorder        // nil when the session is not recorded; fixed before output starts
	uploads chan struct{}   // limits concurrent file transfers on this connection
	meta    []byte          // latest side information, replayed to new viewers
	preview *Preview        // what a window last showed; nil until one sent it
	prevRev int64           // previews kept so far
	winSeqs []winSeq        // the windows that sent previews (the latest last), and their numbers for them
	alerts  alertScanner
	onAlert func(*Session, Alert) // from Manager.OnAlert, fixed before output starts
	alertq  chan<- *Session       // the manager's alert queue
	spill   func(*Session)        // the manager's overflow list, for when alertq is full
	alert   *Alert                // waiting for the alert worker; later alerts merge into it
	queued  bool                  // the session is in alertq
}

// noteAlert merges an alert into the one waiting; called with mu held. A
// notification with text wins over a plain bell.
func (s *Session) noteAlert(a Alert) {
	if s.alert == nil || a.Message != "" {
		s.alert = &a
	}
}

// queueAlert hands a waiting alert to the manager's worker; called with mu
// held. A session is queued at most once, so a program ringing the bell in a
// loop costs one queue slot, not a goroutine per bell. When the queue is full
// the session goes on the manager's overflow list (also at most once), which
// the worker drains on a timer, so a last alert is not lost to a busy moment.
func (s *Session) queueAlert() {
	if s.alert == nil || s.queued || s.alertq == nil {
		return
	}
	s.queued = true
	select {
	case s.alertq <- s:
	default:
		s.spill(s)
	}
}

// takeAlert is the worker's side of the hand-off.
func (s *Session) takeAlert() *Alert {
	s.mu.Lock()
	defer s.mu.Unlock()
	a := s.alert
	s.alert, s.queued = nil, false
	return a
}

// PublishMeta replaces the session's side information and offers it to every
// viewer. A viewer that has not picked up the previous value gets the newer
// one instead (state, not a log: dropping intermediate values is fine).
func (s *Session) PublishMeta(b []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.meta = b
	for sub := range s.subs {
		select {
		case <-sub.Meta:
		default:
		}
		select {
		case sub.Meta <- b:
		default:
		}
	}
}

// UploadSlot reserves one of the session's transfer slots.
func (s *Session) UploadSlot() (release func(), ok bool) {
	select {
	case s.uploads <- struct{}{}:
		return func() { <-s.uploads }, true
	default:
		return nil, false
	}
}

func (s *Session) Info() Info {
	s.mu.Lock()
	defer s.mu.Unlock()
	own, watching := 0, 0
	for sub := range s.subs {
		if sub.watcher == 0 {
			own++
		} else {
			watching++
		}
	}
	return Info{ID: s.ID, HostID: s.HostID, HostName: s.HostName, State: s.state, CreatedAt: s.Created.Unix(),
		Viewers: own, Watching: watching, Cols: s.cols, Rows: s.rows, Title: s.title, Recording: s.rec != nil && s.rec.Active(),
		Persistence: s.Persistence, MuxSession: s.muxSession(),
		Meta: append(json.RawMessage(nil), s.meta...), PreviewRev: s.prevRev}
}

type winSeq struct {
	win string
	seq int64
}

// The windows remembered per session for ordering previews.
const maxPreviewWindows = 16

// SetPreview keeps p (already checked by the caller) as the latest preview,
// unless its window already sent one after it: a window numbers its
// previews (seq), and requests can arrive out of order. Previews of
// different windows are kept in the order they arrive. It reports whether p
// was kept.
func (s *Session) SetPreview(p Preview, win string, seq int64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if win != "" {
		i := slices.IndexFunc(s.winSeqs, func(w winSeq) bool { return w.win == win })
		if i >= 0 {
			if seq <= s.winSeqs[i].seq {
				return false
			}
			s.winSeqs = slices.Delete(s.winSeqs, i, i+1)
		}
		if len(s.winSeqs) >= maxPreviewWindows {
			s.winSeqs = slices.Delete(s.winSeqs, 0, 1)
		}
		s.winSeqs = append(s.winSeqs, winSeq{win, seq})
	}
	s.prevRev++
	p.At, p.Rev = time.Now().Unix(), s.prevRev
	s.preview = &p
	return true
}

// Preview returns the latest preview, or nil.
func (s *Session) Preview() *Preview {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.preview == nil {
		return nil
	}
	p := *s.preview
	return &p
}

// Client exposes the SSH connection for side channels (file upload, herdr).
func (s *Session) Client() *ssh.Client { return s.client }

// Finished is closed when the session ends.
func (s *Session) Finished() <-chan struct{} { return s.finished }

// Snapshot is what is kept of a session (see the web package's open_tabs),
// read at once: its state, why it ended, since when no viewer of its owner
// has looked (zero while one does), when it ended, its size.
type Snapshot struct {
	State     string
	Reason    string
	IdleSince time.Time
	EndedAt   time.Time
	Cols      int
	Rows      int
}

func (s *Session) Snapshot() Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	return Snapshot{State: s.state, Reason: s.reason, IdleSince: s.idleSince, EndedAt: s.endedAt, Cols: s.cols, Rows: s.rows}
}

func (s *Session) State() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state
}

// pump copies remote output into the ring and fans it out to viewers.
func (s *Session) pump(r io.Reader) {
	defer s.pumps.Done()
	buf := make([]byte, readChunk)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			s.mu.Lock()
			off := s.buf.end()
			s.buf.write(buf[:n])
			if s.rec != nil {
				s.rec.Output(buf[:n])
			}
			s.scanTitle(buf[:n])
			if s.onAlert != nil {
				s.alerts.feed(buf[:n], s.noteAlert)
				s.queueAlert()
			}
			for sub := range s.subs {
				chunk := Chunk{Offset: off, Data: append([]byte(nil), buf[:n]...)}
				select {
				case sub.C <- chunk:
				default:
					// Too slow to keep up: drop it; the browser reconnects from its offset.
					s.removeSub(sub)
				}
			}
			s.mu.Unlock()
		}
		if err != nil {
			return
		}
	}
}

// writer is the only goroutine that writes to the remote stdin, so a host that
// stops reading input blocks this goroutine instead of request handlers.
func (s *Session) writer() {
	for {
		select {
		case <-s.finished:
			return
		case p := <-s.input:
			if _, err := s.stdin.Write(p); err != nil {
				return
			}
		}
	}
}

// Attach registers a viewer. It returns the output the viewer is missing and
// whether that output is a full replay (the viewer must clear its screen first).
func (s *Session) Attach(fromOffset uint64, haveOffset bool) (sub *Sub, backlog []byte, start uint64, reset bool, ev *Event, err error) {
	return s.AttachAs("", fromOffset, haveOffset)
}

// AttachAs is Attach for the browser window named viewer (the same name on
// each of its connections); "" stands for a viewer of its own.
func (s *Session) AttachAs(viewer string, fromOffset uint64, haveOffset bool) (sub *Sub, backlog []byte, start uint64, reset bool, ev *Event, err error) {
	return s.attach(viewer, 0, fromOffset, haveOffset)
}

// AttachWatcher is Attach for a user the owner let watch. Such a viewer
// never sets the size and does not keep the session from detaching; the
// caller passes it no input. It fails once the user may no longer watch.
func (s *Session) AttachWatcher(user int64, fromOffset uint64, haveOffset bool) (sub *Sub, backlog []byte, start uint64, reset bool, ev *Event, err error) {
	return s.attach("", user, fromOffset, haveOffset)
}

func (s *Session) attach(viewer string, watcher int64, fromOffset uint64, haveOffset bool) (sub *Sub, backlog []byte, start uint64, reset bool, ev *Event, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var g *grant
	if watcher != 0 {
		if g = s.grants[watcher]; g == nil {
			return nil, nil, 0, false, nil, ErrNotFound
		}
	}
	// Watchers have room of their own: they never crowd out the owner.
	if s.state == StateRunning {
		n := 0
		for x := range s.subs {
			if (x.watcher != 0) == (watcher != 0) {
				n++
			}
		}
		if (watcher == 0 && n >= maxViewers) || (watcher != 0 && n >= maxWatchers) {
			return nil, nil, 0, false, nil, ErrTooManyViewers
		}
	}
	if haveOffset {
		if b, ok := s.buf.since(fromOffset); ok {
			backlog, start = b, fromOffset
		} else {
			reset = true
		}
	} else {
		reset = true
	}
	var prelude []byte
	if reset {
		backlog, start, prelude = s.buf.all()
	}
	if viewer == "" {
		s.subSeq++
		viewer = "#" + strconv.Itoa(s.subSeq) // cannot clash with a named one (see the web layer)
	}
	sub = &Sub{C: make(chan Chunk, subBuffer), Events: make(chan Event, 4), Meta: make(chan []byte, 1), Size: make(chan SizeNote, 1), done: make(chan struct{}), viewer: viewer, watcher: watcher, leave: g, Prelude: prelude}
	if g != nil {
		sub.first = !g.watched
		g.watched = true
	}
	sub.Size <- SizeNote{Cols: s.cols, Rows: s.rows, Own: s.sizeBy == viewer}
	if s.meta != nil {
		sub.Meta <- s.meta
	}
	if s.state != StateRunning {
		e := Event{State: s.state, ExitCode: s.exitCode, Reason: s.reason, Offset: s.buf.end()}
		ev = &e
		sub.close()
		return
	}
	s.subs[sub] = struct{}{}
	if watcher == 0 {
		s.idleSince = time.Time{}
	}
	return
}

// Grant lets a user watch the session, read-only.
func (s *Session) Grant(user int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.grants == nil {
		s.grants = map[int64]*grant{}
	}
	if s.grants[user] == nil {
		s.grants[user] = &grant{done: make(chan struct{})}
	}
}

// Revoke ends a user's leave to watch, and closes what they have open.
func (s *Session) Revoke(user int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if g := s.grants[user]; g != nil {
		close(g.done)
		delete(s.grants, user)
	}
	for sub := range s.subs {
		if sub.watcher == user {
			s.removeSub(sub)
		}
	}
}

// Granted reports whether the user may watch.
func (s *Session) Granted(user int64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.grants[user] != nil
}

// Watchers lists the users who may watch, with how many windows each has
// open on the session.
func (s *Session) Watchers() map[int64]int {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[int64]int, len(s.grants))
	for u := range s.grants {
		out[u] = 0
	}
	for sub := range s.subs {
		if sub.watcher != 0 {
			out[sub.watcher]++
		}
	}
	return out
}

func (s *Session) Detach(sub *Sub) {
	if sub == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.subs[sub]; ok {
		s.removeSub(sub)
	}
}

// removeSub ends a viewer's subscription; called with mu held. A window
// whose last connection this was is no longer in charge of the size.
func (s *Session) removeSub(sub *Sub) {
	delete(s.subs, sub)
	sub.close()
	// Detached once none of the owner's viewers is left (watchers do not count).
	if sub.watcher == 0 && s.idleSince.IsZero() {
		owned := false
		for other := range s.subs {
			owned = owned || other.watcher == 0
		}
		if !owned {
			s.idleSince = time.Now()
		}
	}
	if s.sizeBy != sub.viewer {
		return
	}
	for other := range s.subs {
		if other.viewer == sub.viewer {
			return
		}
	}
	s.sizeBy = ""
}

// Write queues input. When the host is slow to read, it waits (which pauses
// reading from the browser, i.e. back-pressure) for up to inputWait, then
// fails loudly: silently dropping keystrokes could turn a command into another.
func (s *Session) Write(ctx context.Context, p []byte) error {
	for len(p) > 0 {
		n := min(len(p), inputChunk)
		chunk := append([]byte(nil), p[:n]...)
		t := time.NewTimer(inputWait)
		select {
		case s.input <- chunk:
			t.Stop()
		case <-s.finished:
			t.Stop()
			return ErrClosed
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		case <-t.C:
			return ErrInputBusy
		}
		p = p[n:]
	}
	return nil
}

// Resize records a size set by no particular viewer (see ResizeFrom).
func (s *Session) Resize(cols, rows int) error { return s.ResizeFrom(nil, 0, true, cols, rows) }

const maxCols, maxRows = 1000, 500 // the largest size accepted

// ResizeFrom handles request number seq of viewer from: its size. With take
// (the user acted in that viewer) the size is taken over; without (its
// window changed size) it applies only while that viewer set the size, or
// nobody did. The size is sent to the host and every viewer hears of a change
// of size or of the viewer that set it; the asking viewer always hears back,
// so it knows its request was handled and whether it is the one in charge.
// An invalid size changes nothing, but is answered like any request.
func (s *Session) ResizeFrom(from *Sub, seq int64, take bool, cols, rows int) error {
	valid := cols >= 2 && rows >= 1 && cols <= maxCols && rows <= maxRows
	s.sizeMu.Lock()
	defer s.sizeMu.Unlock()
	s.mu.Lock()
	by := ""
	if from != nil {
		if _, ok := s.subs[from]; !ok { // it left while this request waited
			s.mu.Unlock()
			return nil
		}
		if seq > from.ack {
			from.ack = seq
		}
		by = from.viewer
	}
	setter := by
	if !valid || (!take && s.sizeBy != "" && s.sizeBy != by) {
		cols, rows, setter = s.cols, s.rows, s.sizeBy // the size stays as it is
	}
	changed := s.cols != cols || s.rows != rows || s.sizeBy != setter
	s.cols, s.rows, s.sizeBy = cols, rows, setter
	running := s.state == StateRunning
	for sub := range s.subs {
		if !changed && sub != from {
			continue
		}
		note := SizeNote{Cols: cols, Rows: rows, Own: setter != "" && sub.viewer == setter, Ack: sub.ack}
		select {
		case <-sub.Size:
		default:
		}
		select {
		case sub.Size <- note:
		default:
		}
	}
	s.mu.Unlock()
	if !valid {
		return errors.New("invalid size")
	}
	// Compared with what the host was sent, not with what was recorded: a
	// size whose sending failed is sent again when it is reported again.
	if !running || s.ptySize == [2]int{cols, rows} {
		return nil
	}
	return s.sendSize(cols, rows)
}

// sendSize sends a size to the host; called with sizeMu held.
func (s *Session) sendSize(cols, rows int) error {
	// Recorded before the host hears of it, so output drawn at the new size
	// comes after the size in the recording; put back if it was not sent.
	s.recordSize(cols, rows)
	if err := s.winch(rows, cols); err != nil {
		s.recordSize(s.ptySize[0], s.ptySize[1])
		return err
	}
	s.ptySize = [2]int{cols, rows}
	return nil
}

// recordSize notes a size in the recording, in order with output (both
// under mu).
func (s *Session) recordSize(cols, rows int) {
	if s.rec == nil {
		return
	}
	s.mu.Lock()
	s.rec.Resize(cols, rows)
	s.mu.Unlock()
}

// Redraw nudges full-screen programs (tmux, vim) to repaint by briefly changing
// the window size. Used after a full replay, whose head may be incomplete.
// It holds the size for its whole run, so a resize asked for meanwhile is
// applied after it and is never undone by it.
func (s *Session) Redraw() {
	s.sizeMu.Lock()
	defer s.sizeMu.Unlock()
	s.mu.Lock()
	cols, rows, running := s.cols, s.rows, s.state == StateRunning
	s.mu.Unlock()
	if !running || cols < 3 {
		return
	}
	_ = s.sendSize(cols-1, rows)
	time.Sleep(60 * time.Millisecond)
	_ = s.sendSize(cols, rows)
}

// end moves the session to a final state once and tells every viewer. When the
// remote side ended on its own, the caller waits for the output pumps first so
// the final event follows the last byte.
func (s *Session) end(state string, code int, reason string) {
	s.mu.Lock()
	if s.state != StateRunning {
		s.mu.Unlock()
		return
	}
	s.state, s.exitCode, s.reason, s.endedAt = state, code, reason, time.Now()
	// (Counted where the final state is set: one who sees it ended knows its
	// onEnd is counted, done or under way.)
	ends := s.onEnd != nil && s.ends != nil
	if ends {
		s.ends.Add(1)
	}
	ev := Event{State: state, ExitCode: code, Reason: reason, Offset: s.buf.end()}
	for sub := range s.subs {
		select {
		case sub.Events <- ev:
		default:
		}
		sub.close()
	}
	s.subs = map[*Sub]struct{}{}
	close(s.finished)
	if s.rec != nil {
		s.rec.Close() // output after this point reaches nobody
	}
	s.mu.Unlock()
	// The connection first: closing it cannot block, and it makes the channel
	// close below fail fast instead of waiting on a stalled transport.
	if s.client != nil {
		s.client.Close()
	}
	if s.ssh != nil {
		s.ssh.Close()
	}
	if s.onEnd != nil {
		s.onEnd(s)
	}
	if ends {
		s.ends.Done()
	}
}

func (s *Session) Close(reason string) { s.end(StateClosed, 0, reason) }

// waitPumps waits (briefly) for remaining output to be read after the remote
// program exited.
func (s *Session) waitPumps(limit time.Duration) {
	done := make(chan struct{})
	go func() {
		s.pumps.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(limit):
	}
}

// scanTitle picks up OSC 0/2 window titles so the session list can show what is
// running. It only looks within a single chunk, which is enough for titles.
func (s *Session) scanTitle(p []byte) {
	for i := 0; i+3 < len(p); i++ {
		if p[i] != 0x1b || p[i+1] != ']' || (p[i+2] != '0' && p[i+2] != '2') || p[i+3] != ';' {
			continue
		}
		j := i + 4
		for j < len(p) && p[j] != 0x07 && !(p[j] == 0x1b && j+1 < len(p) && p[j+1] == '\\') {
			j++
		}
		if j < len(p) && j-(i+4) <= 200 {
			s.title = string(p[i+4 : j])
		}
		i = j
	}
}

// muxSession is the tmux or herdr session the shell is attached to.
func (s *Session) muxSession() string {
	switch s.Persistence {
	case "tmux":
		return s.TmuxSession
	case "herdr":
		return s.HerdrSession
	}
	return ""
}
