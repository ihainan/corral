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
