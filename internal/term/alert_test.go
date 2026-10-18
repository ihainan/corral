package term

import (
	"bytes"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// A program ringing the bell in a loop must not start a goroutine per bell:
// alerts are merged per session and delivered by one worker.
func TestBellStormIsCoalesced(t *testing.T) {
	m := NewManager(time.Hour, 5)
	defer m.Shutdown()
	release := make(chan struct{})
	var calls atomic.Int32
	var mu sync.Mutex
	var last Alert
	s := &Session{buf: newRing(ringSize), subs: map[*Sub]struct{}{}, state: StateRunning, finished: make(chan struct{}),
		alertq: m.alerts, spill: m.spill, onAlert: func(_ *Session, a Alert) {
			if calls.Add(1) == 1 {
				<-release // the first delivery is slow; everything after it piles up
			}
			mu.Lock()
			last = a
			mu.Unlock()
		}}
	before := runtime.NumGoroutine()
	out := append(bytes.Repeat([]byte{0x07}, 4<<20), []byte("\x1b]9;build finished\x07")...)
	out = append(out, bytes.Repeat([]byte{0x07}, 64<<10)...)
	s.pumps.Add(1)
	s.pump(bytes.NewReader(out))
	if n := runtime.NumGoroutine(); n > before+5 {
		t.Fatalf("%d goroutines after a bell storm (was %d)", n, before)
	}
	close(release)
	deadline := time.Now().Add(3 * time.Second)
	for calls.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if c := calls.Load(); c != 2 || last.Message != "build finished" {
		t.Fatalf("%d deliveries, last %+v; want the first bell, then one merged alert keeping the message", c, last)
	}
}

// An alert that finds the queue full is still delivered, without waiting for
// more output from its session.
func TestAlertIsRetriedWhenTheQueueIsFull(t *testing.T) {
	oldQ, oldR := alertQueue, alertRetry
	alertQueue, alertRetry = 1, 20*time.Millisecond
	m := NewManager(time.Hour, 5)
	alertQueue, alertRetry = oldQ, oldR
	defer m.Shutdown()
	release := make(chan struct{})
	var mu sync.Mutex
	got := map[*Session]bool{}
	onAlert := func(s *Session, _ Alert) {
		mu.Lock()
		first := len(got) == 0
		got[s] = true
		mu.Unlock()
		if first {
			<-release // the worker is busy while the others ring
		}
	}
	ring := func() *Session {
		s := &Session{buf: newRing(ringSize), subs: map[*Sub]struct{}{}, state: StateRunning, finished: make(chan struct{}),
			alertq: m.alerts, spill: m.spill, onAlert: onAlert}
		s.pumps.Add(1)
		s.pump(bytes.NewReader([]byte{0x07}))
		return s
	}
	a := ring()
	for deadline := time.Now().Add(2 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		mu.Lock()
		busy := got[a]
		mu.Unlock()
		if busy || time.Now().After(deadline) {
			break
		}
	}
	b, c := ring(), ring() // b fills the queue, c does not fit and rings only once
	close(release)
	deadline := time.Now().Add(2 * time.Second)
	for {
		mu.Lock()
		done := got[b] && got[c]
		mu.Unlock()
		if done {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("an alert that did not fit in the queue was never delivered")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestAlertScanner(t *testing.T) {
	var got []Alert
	var a alertScanner
	feed := func(s string) { a.feed([]byte(s), func(x Alert) { got = append(got, x) }) }
	feed("plain \x07 bell")
	feed("\x1b]0;title\x07")     // window title: not an alert
	feed("\x1b]9;Claude needs ") // split across chunks
	feed("your permission\x07")
	feed("\x1b]777;notify;Codex;turn complete\x1b\\")
	feed("\x1b]9;4;1;50\x07") // progress, ignored
	if len(got) != 3 || got[0].Message != "" || got[1].Message != "Claude needs your permission" || got[2].Message != "Codex: turn complete" {
		t.Fatalf("alerts: %+v", got)
	}
}

// A size change reaches every viewer, so the others can follow it.
func TestResizeTellsEveryViewer(t *testing.T) {
	s := &Session{subs: map[*Sub]struct{}{}, state: StateExited, cols: 80, rows: 24}
	a, b := &Sub{Size: make(chan SizeNote, 1)}, &Sub{Size: make(chan SizeNote, 1)}
	s.subs[a], s.subs[b] = struct{}{}, struct{}{}
	if err := s.ResizeFrom(a, 1, true, 120, 40); err != nil {
		t.Fatal(err)
	}
	if got := <-a.Size; got != (SizeNote{Cols: 120, Rows: 40, Own: true, Ack: 1}) {
		t.Fatalf("the viewer that set it got %+v", got)
	}
	if got := <-b.Size; got != (SizeNote{Cols: 120, Rows: 40}) {
		t.Fatalf("the other viewer got %+v", got)
	}
	// The same size again from the same viewer: only it hears back (its
	// request was applied); the others have nothing new to learn.
	if err := s.ResizeFrom(a, 2, true, 120, 40); err != nil {
		t.Fatal(err)
	}
	if got := <-a.Size; got != (SizeNote{Cols: 120, Rows: 40, Own: true, Ack: 2}) {
		t.Fatalf("the repeated request was not answered: %+v", got)
	}
	select {
	case got := <-b.Size:
		t.Fatalf("an unchanged size was sent again: %+v", got)
	default:
	}
	// The same size taken over by the other viewer: both hear of it.
	if err := s.ResizeFrom(b, 1, true, 120, 40); err != nil {
		t.Fatal(err)
	}
	if got := <-a.Size; got != (SizeNote{Cols: 120, Rows: 40, Ack: 2}) {
		t.Fatalf("the first viewer got %+v", got)
	}
	if got := <-b.Size; got != (SizeNote{Cols: 120, Rows: 40, Own: true, Ack: 1}) {
		t.Fatalf("the viewer taking over got %+v", got)
	}
	// A window of a, resized without the user acting there (take false):
	// b's size stays; a hears back that it is not in charge.
	_ = s.ResizeFrom(a, 3, false, 100, 30)
	if got := <-a.Size; got != (SizeNote{Cols: 120, Rows: 40, Ack: 3}) {
		t.Fatalf("a resize without take: %+v", got)
	}
	select {
	case got := <-b.Size:
		t.Fatalf("b was told of a change that did not happen: %+v", got)
	default:
	}
	// b's own window change applies, b being in charge.
	_ = s.ResizeFrom(b, 2, false, 110, 35)
	if got := <-b.Size; got != (SizeNote{Cols: 110, Rows: 35, Own: true, Ack: 2}) {
		t.Fatalf("b's own resize: %+v", got)
	}
	<-a.Size
	// An older request number never lowers what was applied.
	_ = s.ResizeFrom(a, 1, true, 100, 30)
	if got := <-a.Size; got.Ack != 3 || !got.Own {
		t.Fatalf("a late request: %+v", got)
	}
	// Once the viewer in charge leaves, a resize without take applies again.
	s.subs[b] = struct{}{}
	_ = s.ResizeFrom(b, 3, true, 90, 30)
	<-a.Size
	<-b.Size
	delete(s.subs, b)
	s.sizeBy = nil // as Detach does
	_ = s.ResizeFrom(a, 4, false, 100, 30)
	if got := <-a.Size; got != (SizeNote{Cols: 100, Rows: 30, Own: true, Ack: 4}) {
		t.Fatalf("after the viewer in charge left: %+v", got)
	}
}
