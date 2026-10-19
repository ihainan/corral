package record

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func readAll(t *testing.T, path string) (Header, [][]any) {
	t.Helper()
	rc, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	sc := bufio.NewScanner(rc)
	sc.Buffer(nil, 1<<20)
	var h Header
	var evs [][]any
	for i := 0; sc.Scan(); i++ {
		if i == 0 {
			if err := json.Unmarshal(sc.Bytes(), &h); err != nil {
				t.Fatal(err)
			}
			continue
		}
		var e []any
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			t.Fatalf("line %d: %v", i, err)
		}
		evs = append(evs, e)
	}
	return h, evs
}

func TestRecordingRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.cast.gz")
	var res Result
	r := Create(path, 80, 24, "box", 1<<20, func(x Result) { res = x })
	// "中" is e4 b8 ad: split across two chunks it must come out whole.
	r.Output([]byte("hello \xe4\xb8"))
	r.Output([]byte("\xad!\r\n"))
	r.Resize(100, 30)
	r.Output([]byte("bad \xff byte"))
	r.Close()
	r.Close()
	r.Output([]byte("after close"))
	r.Wait()
	if res.Err != nil || res.Truncated || res.Gaps {
		t.Fatalf("result %+v", res)
	}
	h, evs := readAll(t, path)
	if h.Version != 2 || h.Width != 80 || h.Height != 24 || h.Title != "box" {
		t.Fatalf("header %+v", h)
	}
	var out strings.Builder
	sawResize := false
	for _, e := range evs {
		switch e[1] {
		case "o":
			out.WriteString(e[2].(string))
		case "r":
			sawResize = e[2] == "100x30"
		}
	}
	if got := out.String(); got != "hello 中!\r\nbad � byte" {
		t.Fatalf("output %q", got)
	}
	if !sawResize {
		t.Fatal("resize missing")
	}
}

func TestRecordingStopsAtLimit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.cast.gz")
	var res Result
	r := Create(path, 80, 24, "", 40, func(x Result) { res = x })
	r.Output([]byte("12345"))    // 5 + 24
	r.Output([]byte("67890abc")) // over the limit: dropped, and the recording ends
	r.Output([]byte("x"))
	r.Close()
	r.Wait()
	if !res.Truncated || res.Bytes != 5 {
		t.Fatalf("result %+v", res)
	}
	_, evs := readAll(t, path)
	if last := evs[len(evs)-1]; last[1] != "m" || last[2] != "truncated" {
		t.Fatalf("last event %v", last)
	}
}

func TestRecordingMarksGaps(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.cast.gz")
	var res Result
	hold := make(chan struct{})
	r := newRecorder(80, 24, "", 1<<30, fileAt(path), func(x Result) { res = x }, hold)
	for i := 0; i < queueLen; i++ {
		r.Output([]byte("x"))
	}
	r.Output([]byte("dropped")) // the writer is held: the queue is full
	close(hold)
	for len(r.q) > 0 {
		time.Sleep(time.Millisecond)
	}
	r.Output([]byte("after"))
	r.Close()
	r.Wait()
	if !res.Gaps {
		t.Fatal("no gap reported")
	}
	_, evs := readAll(t, path)
	var out strings.Builder
	gapBeforeAfter := false
	for _, e := range evs {
		if e[1] == "m" && e[2] == "gap" {
			gapBeforeAfter = !strings.Contains(out.String(), "after")
		}
		if e[1] == "o" {
			out.WriteString(e[2].(string))
		}
	}
	if !gapBeforeAfter || strings.Contains(out.String(), "dropped") || !strings.HasSuffix(out.String(), "after") {
		t.Fatalf("gap marker or output wrong: %q", out.String())
	}
}

// A recording being written can be read up to its last flush.
func TestRecordingReadableWhileWritten(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.cast.gz")
	hold := make(chan struct{})
	r := newRecorder(80, 24, "live", 1<<20, fileAt(path), nil, hold)
	defer func() { close(hold); r.Close(); r.Wait() }()
	deadline := time.Now().Add(2 * time.Second)
	for {
		rc, err := Open(path)
		if err == nil {
			b, rerr := io.ReadAll(rc)
			rc.Close()
			if rerr != nil {
				t.Fatalf("partial read: %v", rerr)
			}
			if strings.Contains(string(b), `"title":"live"`) {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("header never readable")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Resizes count against the limit too, and markers that found the queue
// full are written at the end.
func TestRecordingLimitAndLateMarkers(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.cast.gz")
	var res Result
	hold := make(chan struct{})
	r := newRecorder(80, 24, "", 24*queueLen, fileAt(path), func(x Result) { res = x }, hold) // full exactly when the queue is
	for i := 0; i < queueLen+10; i++ {                                                        // fills the queue, then the limit
		r.Resize(80+i%2, 24)
	}
	close(hold)
	r.Close()
	r.Wait()
	if !res.Truncated || !res.Gaps {
		t.Fatalf("result %+v", res)
	}
	_, evs := readAll(t, path)
	var marks []string
	for _, e := range evs {
		if e[1] == "m" {
			marks = append(marks, e[2].(string))
		}
	}
	if strings.Join(marks, ",") != "gap,truncated" {
		t.Fatalf("markers %v", marks)
	}
}

// A character is not put together across a gap.
func TestRecordingGapEndsSplitCharacter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.cast.gz")
	hold := make(chan struct{})
	r := newRecorder(80, 24, "", 1<<30, fileAt(path), nil, hold)
	for i := 0; i < queueLen-1; i++ {
		r.Output([]byte("x"))
	}
	r.Output([]byte{0xe4})             // the last event to fit: the start of a character
	r.Output([]byte{0xb8, 0xad, 0xe5}) // dropped: the queue is full
	close(hold)
	for len(r.q) > 0 {
		time.Sleep(time.Millisecond)
	}
	r.Output([]byte{0xa5, 0xbd}) // with E5 this would be 好; joined to E4 it would make up a character
	r.Close()
	r.Wait()
	_, evs := readAll(t, path)
	var out strings.Builder
	for _, e := range evs {
		switch e[1] {
		case "o":
			out.WriteString(e[2].(string))
		case "m":
			out.WriteString("|" + e[2].(string) + "|")
		}
	}
	got := out.String()
	if !strings.Contains(got, "\ufffd|gap|") {
		t.Fatalf("the cut character should end before the gap marker: %q", got[len(got)-min(len(got), 40):])
	}
	if strings.ContainsAny(got, "中好") {
		t.Fatalf("a character was made up across the gap: %q", got)
	}
}

// Without a file the recorder still takes (and drops) events.
func TestRecordingWithoutFile(t *testing.T) {
	var res Result
	reported := make(chan struct{})
	r := New(80, 24, "", 1<<20, func() (io.WriteCloser, error) { return nil, os.ErrPermission }, func(x Result) { res = x; close(reported) })
	// Reported at once, before the session ends.
	select {
	case <-reported:
	case <-time.After(2 * time.Second):
		t.Fatal("failure not reported while the session runs")
	}
	if res.Err == nil || r.Active() {
		t.Fatalf("result %+v, active %v", res, r.Active())
	}
	for i := 0; i < 3*queueLen; i++ {
		r.Output([]byte("x"))
	}
	r.Close()
	r.Wait()
}

type failingWriter struct{}

func (failingWriter) Write(p []byte) (int, error) { return 0, os.ErrClosed }
func (failingWriter) Close() error                { return nil }

// A file that stops taking data ends the recording, visibly.
func TestRecordingWriteFailureStopsIt(t *testing.T) {
	var res Result
	r := New(80, 24, "", 1<<20, func() (io.WriteCloser, error) { return failingWriter{}, nil }, func(x Result) { res = x })
	deadline := time.Now().Add(2 * time.Second)
	for r.Active() && time.Now().Before(deadline) {
		r.Output([]byte("data"))
		time.Sleep(10 * time.Millisecond)
	}
	if r.Active() {
		t.Fatal("still active after the file failed")
	}
	r.Close()
	r.Wait()
	if res.Err == nil {
		t.Fatal("no error reported")
	}
}
