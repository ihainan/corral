package term

import (
	"errors"
	"math/rand/v2"
	"sync"
	"testing"
	"time"
)

// fakePTY records the sizes a session sends to the host, slowly and with
// jitter, so sends that are not ordered would overtake each other.
type fakePTY struct {
	mu   sync.Mutex
	size [2]int
	sent int
	fail bool
}

func (p *fakePTY) winch(rows, cols int) error {
	time.Sleep(time.Duration(rand.IntN(300)) * time.Microsecond)
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.fail {
		return errors.New("channel closed")
	}
	p.size = [2]int{cols, rows}
	p.sent++
	return nil
}

func (p *fakePTY) get() ([2]int, int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.size, p.sent
}

func sizedSession(p *fakePTY) *Session {
	return &Session{buf: newRing(1024), subs: map[*Sub]struct{}{}, state: StateRunning, finished: make(chan struct{}),
		cols: 80, rows: 24, ptySize: [2]int{80, 24}, winch: p.winch}
}

// Viewers taking the terminal over at the same time, and redraws after
// reconnects, leave the host at the size recorded last.
func TestTerminalSizeEndsAsRecorded(t *testing.T) {
	for round := 0; round < 10; round++ {
		p := &fakePTY{size: [2]int{80, 24}}
		s := sizedSession(p)
		var wg sync.WaitGroup
		for i := 0; i < 8; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for j := 0; j < 20; j++ {
					if i < 2 && j == 5 {
						s.Redraw()
						continue
					}
					_ = s.Resize(40+rand.IntN(100), 10+rand.IntN(40))
				}
			}()
		}
		wg.Wait()
		got, _ := p.get()
		if info := s.Info(); got != [2]int{info.Cols, info.Rows} {
			t.Fatalf("round %d: the host is at %v, the session says %dx%d", round, got, info.Cols, info.Rows)
		}
	}
}

// A resize asked for while a redraw runs is applied after it, not undone by
// the redraw restoring the size it started with.
func TestRedrawDoesNotRestoreAnOutdatedSize(t *testing.T) {
	p := &fakePTY{size: [2]int{80, 24}}
	s := sizedSession(p)
	done := make(chan struct{})
	go func() { s.Redraw(); close(done) }()
	time.Sleep(20 * time.Millisecond) // inside the redraw's pause
	if err := s.Resize(120, 40); err != nil {
		t.Fatal(err)
	}
	<-done
	if got, _ := p.get(); got != [2]int{120, 40} {
		t.Fatalf("the host is at %v after a redraw and a resize to 120x40", got)
	}
}

// A size that could not be sent is sent again when it is reported again,
// although it is already the recorded one.
func TestFailedResizeIsRetried(t *testing.T) {
	p := &fakePTY{size: [2]int{80, 24}}
	s := sizedSession(p)
	p.mu.Lock()
	p.fail = true
	p.mu.Unlock()
	if err := s.Resize(100, 30); err == nil {
		t.Fatal("a failed send was not reported")
	}
	p.mu.Lock()
	p.fail = false
	p.mu.Unlock()
	if err := s.Resize(100, 30); err != nil {
		t.Fatal(err)
	}
	if got, _ := p.get(); got != [2]int{100, 30} {
		t.Fatalf("the host is at %v; the repeated size was not sent", got)
	}
	_, sent := p.get()
	_ = s.Resize(100, 30)
	if _, again := p.get(); again != sent {
		t.Fatal("a size the host already has was sent again")
	}
}

// Held roomy for a read: the host is larger for the while, the size the
// viewers set is what it was; one set meanwhile, and a redraw, do not
// shrink it under the read, and the host ends at the size set last.
func TestRoomyHoldsAndPutsBack(t *testing.T) {
	p := &fakePTY{size: [2]int{47, 34}}
	s := sizedSession(p)
	s.cols, s.rows, s.ptySize = 47, 34, [2]int{47, 34}
	back, ok := s.Roomy(160, 50)
	if size, _ := p.get(); !ok || size != [2]int{160, 50} {
		t.Fatalf("held: ok=%v host=%v", ok, size)
	}
	if c, r := s.Size(); c != 47 || r != 34 {
		t.Errorf("the viewers' size changed: %dx%d", c, r)
	}
	if _, again := s.Roomy(160, 50); again {
		t.Error("held twice")
	}
	_ = s.Resize(50, 30) // (a viewer's, meanwhile)
	s.Redraw()
	if size, _ := p.get(); size != [2]int{160, 50} {
		t.Errorf("shrunk under the read: %v", size)
	}
	back()
	back()
	if size, _ := p.get(); size != [2]int{50, 30} {
		t.Errorf("put back at %v, not the size set last", size)
	}
	_ = s.Resize(60, 20)
	if size, _ := p.get(); size != [2]int{60, 20} {
		t.Errorf("after it, a size is sent as before: %v", size)
	}
	// Large already: nothing done. Taller only where it is short.
	if _, ok := s.Roomy(60, 20); ok {
		t.Error("held though large enough")
	}
	_ = s.Resize(200, 20)
	back, _ = s.Roomy(160, 50)
	if size, _ := p.get(); size != [2]int{200, 50} {
		t.Errorf("a wide, short terminal held at %v", size)
	}
	back()
}
