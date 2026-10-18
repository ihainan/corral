package term

import (
	"bytes"
	"testing"
)

func TestRingResume(t *testing.T) {
	r := newRing(10)
	r.write([]byte("hello "))
	r.write([]byte("world"))
	if b, ok := r.since(6); !ok || string(b) != "world" {
		t.Fatalf("since(6) = %q %v", b, ok)
	}
	if _, ok := r.since(99); ok {
		t.Fatal("offset beyond the end must not resume")
	}
	// Overflow trims the head; old offsets then require a reset.
	r.write(bytes.Repeat([]byte("x"), 20))
	if _, ok := r.since(0); ok {
		t.Fatal("trimmed offset must not resume")
	}
	if b, ok := r.since(r.end()); !ok || len(b) != 0 {
		t.Fatal("resuming at the end returns nothing")
	}
}

func TestRingReplayStartsAtLine(t *testing.T) {
	r := newRing(8)
	r.write([]byte("abc\x1b[31mred\nnext line\n"))
	b, from := r.all()
	if from == 0 || bytes.Contains(b, []byte("\x1b[31m")) && b[0] != 'n' {
		t.Fatalf("replay should begin after a newline, got %q from %d", b, from)
	}
}
