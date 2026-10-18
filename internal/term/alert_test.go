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
		alertq: m.alerts, onAlert: func(_ *Session, a Alert) {
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
