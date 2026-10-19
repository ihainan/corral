// Package record writes terminal sessions as asciicast v2 files
// (https://docs.asciinema.org/manual/asciicast/v2/), gzip-compressed: a JSON
// header line, then one JSON array per event — [seconds, "o", text] for
// output, [seconds, "r", "COLSxROWS"] for a resize, [seconds, "m", label] for
// a marker. Only what the host printed is kept, never what was typed.
//
// Recording never slows the terminal down: events go through a bounded queue
// to a writer goroutine, and when the disk cannot keep up they are dropped
// (a "gap" marker says so) instead of making the session wait.
package record

import (
	"bufio"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"time"
	"unicode/utf8"
)

const (
	queueLen   = 512
	flushEvery = 2 * time.Second // so a recording can be played while it is made
)

// Result is what the recording ended up with.
type Result struct {
	Bytes     int64 // output bytes kept
	Truncated bool  // the size limit was reached
	Gaps      bool  // events were dropped because the writer could not keep up
	Err       error // writing failed; the file holds what came before
}

type event struct {
	at         time.Duration
	kind       byte // 'o', 'r', 'm'
	data       []byte
	cols, rows int
}

type Recorder struct {
	start    time.Time
	maxBytes int64
	q        chan event
	done     chan struct{}
	hold     chan struct{} // tests: the writer waits for it before taking events

	mu       sync.Mutex
	closed   bool
	failed   bool // no file could be made: nothing is kept
	dropped  bool // since the last event that got through
	gaps     bool
	full     bool  // the size limit was reached
	accepted int64 // output bytes taken for writing
}

// Header is the asciicast header.
type Header struct {
	Version   int               `json:"version"`
	Width     int               `json:"width"`
	Height    int               `json:"height"`
	Timestamp int64             `json:"timestamp"`
	Title     string            `json:"title,omitempty"`
	Env       map[string]string `json:"env,omitempty"`
}

// New starts a recording. open provides the file; it runs on the writer
// goroutine, so a slow disk (or whatever else it does) never holds up the
// session — events meanwhile wait in the queue. At most maxBytes are
// recorded (output, plus a little for each other event); onDone is called
// once the file is complete (or could not be made), from the writer
// goroutine.
func New(cols, rows int, title string, maxBytes int64, open func() (io.WriteCloser, error), onDone func(Result)) *Recorder {
	return newRecorder(cols, rows, title, maxBytes, open, onDone, nil)
}

// Create is New with a new file at path (which must not exist).
func Create(path string, cols, rows int, title string, maxBytes int64, onDone func(Result)) *Recorder {
	return New(cols, rows, title, maxBytes, fileAt(path), onDone)
}

func fileAt(path string) func() (io.WriteCloser, error) {
	return func() (io.WriteCloser, error) { return os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) }
}

func newRecorder(cols, rows int, title string, maxBytes int64, open func() (io.WriteCloser, error), onDone func(Result), hold chan struct{}) *Recorder {
	r := &Recorder{start: time.Now(), maxBytes: maxBytes, q: make(chan event, queueLen), done: make(chan struct{}), hold: hold}
	h := Header{Version: 2, Width: cols, Height: rows, Timestamp: r.start.Unix(), Title: title, Env: map[string]string{"TERM": "xterm-256color"}}
	go r.write(open, h, onDone)
	return r
}

// eventCost is what an event counts against the size limit.
func eventCost(e event) int64 { return int64(len(e.data)) + 24 }

func (r *Recorder) put(e event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed || r.full || r.failed {
		return
	}
	if r.accepted+eventCost(e) > r.maxBytes {
		// Said by a marker if it fits now, else by the writer at the end.
		r.full = true
		e = event{kind: 'm', data: []byte("truncated")}
	} else if e.kind == 'o' {
		e.data = append([]byte(nil), e.data...)
	}
	e.at = time.Since(r.start)
	if r.dropped && e.kind != 'm' {
		// Tell the reader output is missing here; if even that does not fit,
		// the event is dropped as well and the gap stays open.
		select {
		case r.q <- event{at: e.at, kind: 'm', data: []byte("gap")}:
			r.dropped = false
		default:
			return
		}
	}
	select {
	case r.q <- e:
		r.accepted += eventCost(e)
	default:
		r.dropped, r.gaps = true, true
	}
}

// Output records what the host printed. It never blocks.
func (r *Recorder) Output(p []byte) {
	if len(p) > 0 {
		r.put(event{kind: 'o', data: p})
	}
}

// Resize records a new terminal size. It never blocks.
func (r *Recorder) Resize(cols, rows int) { r.put(event{kind: 'r', cols: cols, rows: rows}) }

// Close ends the recording; later events are ignored. It does not wait for
// the file to be written (Wait does).
func (r *Recorder) Close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.closed {
		r.closed = true
		close(r.q)
	}
}

// Active reports whether output is still kept: not after Close, nor once
// the size limit was reached.
func (r *Recorder) Active() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return !r.closed && !r.full && !r.failed
}

// Wait blocks until the file is complete.
func (r *Recorder) Wait() { <-r.done }

func (r *Recorder) write(open func() (io.WriteCloser, error), h Header, onDone func(Result)) {
	defer close(r.done)
	var res Result
	f, err := open()
	if err != nil {
		// Said at once (the session is not recorded from now on), then the
		// queue is emptied until the session closes the recorder.
		r.mu.Lock()
		r.failed = true
		r.mu.Unlock()
		if onDone != nil {
			onDone(Result{Err: err})
		}
		for range r.q {
		}
		return
	}
	zw := gzip.NewWriter(f)
	bw := bufio.NewWriterSize(zw, 32<<10)
	var carry []byte // the start of a character split across chunks
	fail := func(err error) {
		if res.Err == nil {
			res.Err = err
			// Nothing more is kept: the session stops saying it is recorded.
			r.mu.Lock()
			r.failed = true
			r.mu.Unlock()
		}
	}
	line := func(v any) {
		if res.Err != nil {
			return
		}
		b, err := json.Marshal(v)
		if err != nil {
			fail(err)
			return
		}
		b = append(b, '\n')
		if _, err := bw.Write(b); err != nil {
			fail(err)
		}
	}
	flush := func() {
		if res.Err == nil {
			if err := bw.Flush(); err != nil {
				fail(err)
			} else if err := zw.Flush(); err != nil {
				fail(err)
			}
		}
	}
	line(h)
	flush() // the file is a readable recording from the start
	if r.hold != nil {
		<-r.hold
	}
	tick := time.NewTicker(flushEvery)
	defer tick.Stop()
	secs := func(d time.Duration) float64 { return float64(d.Microseconds()) / 1e6 }
	sawTruncated := false
loop:
	for {
		select {
		case e, ok := <-r.q:
			if !ok {
				break loop
			}
			switch e.kind {
			case 'o':
				text, rest := splitUTF8(append(carry, e.data...))
				carry = rest
				res.Bytes += int64(len(e.data))
				if text != "" {
					line([]any{secs(e.at), "o", text})
				}
			case 'r':
				line([]any{secs(e.at), "r", fmt.Sprintf("%dx%d", e.cols, e.rows)})
			case 'm':
				// A character cut by missing output is not completed by
				// what follows the gap.
				if len(carry) > 0 {
					line([]any{secs(e.at), "o", string(carry)})
					carry = nil
				}
				sawTruncated = sawTruncated || string(e.data) == "truncated"
				line([]any{secs(e.at), "m", string(e.data)})
			}
		case <-tick.C:
			flush()
		}
	}
	if len(carry) > 0 {
		line([]any{secs(time.Since(r.start)), "o", string(carry)})
	}
	// Markers that did not fit in the queue when they were due.
	r.mu.Lock()
	full, dropped := r.full, r.dropped
	res.Gaps = r.gaps
	r.mu.Unlock()
	end := secs(time.Since(r.start))
	if dropped {
		line([]any{end, "m", "gap"})
	}
	if full && !sawTruncated {
		line([]any{end, "m", "truncated"})
	}
	res.Truncated = full
	flush()
	if err := zw.Close(); err != nil {
		fail(err)
	}
	if err := f.Close(); err != nil {
		fail(err)
	}
	if onDone != nil {
		onDone(res)
	}
}

// splitUTF8 returns the text of b up to an incomplete character at its end,
// and that incomplete tail. Bytes that are not UTF-8 become U+FFFD, as JSON
// requires text.
func splitUTF8(b []byte) (string, []byte) {
	cut := len(b)
	// A character is at most 4 bytes: look back at most 3 for its start.
	for i := len(b) - 1; i >= 0 && i >= len(b)-3; i-- {
		if utf8.RuneStart(b[i]) {
			if !utf8.FullRune(b[i:]) {
				cut = i
			}
			break
		}
	}
	return string(b[:cut]), append([]byte(nil), b[cut:]...)
}

// Open reads a recording as plain asciicast text. A recording still being
// written ends at its last flush: the reader then reports io.EOF there.
func Open(path string) (io.ReadCloser, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	zr, err := gzip.NewReader(f)
	if err != nil {
		f.Close()
		return nil, err
	}
	return &reader{f: f, zr: zr}, nil
}

type reader struct {
	f  *os.File
	zr *gzip.Reader
}

func (r *reader) Read(p []byte) (int, error) {
	n, err := r.zr.Read(p)
	if errors.Is(err, io.ErrUnexpectedEOF) {
		err = io.EOF // the part written so far
	}
	return n, err
}

func (r *reader) Close() error {
	r.zr.Close()
	return r.f.Close()
}
