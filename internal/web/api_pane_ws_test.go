package web

import (
	"context"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

func TestHerdrTerminalAttachCommand(t *testing.T) {
	cmd := herdrTerminalAttachCommand("/home/u/.config/herdr/herdr.sock", "term_660ca5e7e9eef1")
	if !strings.Contains(cmd, "exec herdr terminal attach \"$2\" --takeover") {
		t.Fatalf("unexpected command: %s", cmd)
	}
	if !strings.Contains(cmd, "unset HERDR_ENV HERDR_PANE_ID HERDR_TAB_ID HERDR_WORKSPACE_ID") {
		t.Fatalf("missing HERDR_* unset: %s", cmd)
	}
	if !strings.Contains(cmd, "'/home/u/.config/herdr/herdr.sock'") || !strings.Contains(cmd, "'term_660ca5e7e9eef1'") {
		t.Fatalf("args not shell-quoted: %s", cmd)
	}
}

func TestHerdrTermIDValidation(t *testing.T) {
	for _, good := range []string{"term_660ca5e7e9eef1", "t-1:2.3", "abc123"} {
		if !herdrTermID.MatchString(good) {
			t.Errorf("expected valid terminal id: %q", good)
		}
	}
	for _, bad := range []string{"", "term;id", "term id", "../x", "$(id)", strings.Repeat("a", 101)} {
		if herdrTermID.MatchString(bad) {
			t.Errorf("expected invalid terminal id: %q", bad)
		}
	}
}

func TestClampPaneDim(t *testing.T) {
	if got := clampPaneDim("", 80, 20, 400); got != 80 {
		t.Fatalf("empty: got %d", got)
	}
	if got := clampPaneDim("abc", 80, 20, 400); got != 80 {
		t.Fatalf("non-int: got %d", got)
	}
	if got := clampPaneDim("5", 80, 20, 400); got != 20 {
		t.Fatalf("below min: got %d", got)
	}
	if got := clampPaneDim("999", 80, 20, 400); got != 400 {
		t.Fatalf("above max: got %d", got)
	}
	if got := clampPaneDim("120", 80, 20, 400); got != 120 {
		t.Fatalf("in range: got %d", got)
	}
}

func TestPaneAttachSlots(t *testing.T) {
	var slots paneAttachSlots
	c1 := &ssh.Client{}
	c2 := &ssh.Client{}

	rel1, ok := slots.acquire(c1, "term_1")
	if !ok {
		t.Fatal("first acquire should succeed")
	}
	// Duplicate (client, termID) must be rejected so two panels never fight.
	if _, ok := slots.acquire(c1, "term_1"); ok {
		t.Fatal("duplicate acquire for same terminal should fail")
	}
	// Different client with same termID is allowed.
	relOther, ok := slots.acquire(c2, "term_1")
	if !ok {
		t.Fatal("acquire on distinct client should succeed")
	}
	relOther()

	// Fill up to maxPaneAttachesPerClient on c1.
	rels := []func(){rel1}
	for i := 2; i <= maxPaneAttachesPerClient; i++ {
		rel, ok := slots.acquire(c1, "term_"+string(rune('0'+i)))
		if !ok {
			t.Fatalf("acquire #%d should succeed", i)
		}
		rels = append(rels, rel)
	}
	if _, ok := slots.acquire(c1, "term_overflow"); ok {
		t.Fatal("acquire above per-client cap should fail")
	}
	// Releasing frees both the terminal key and client quota (idempotent).
	rels[0]()
	rels[0]()
	relAgain, ok := slots.acquire(c1, "term_1")
	if !ok {
		t.Fatal("re-acquire after release should succeed")
	}
	relAgain()
	for _, r := range rels[1:] {
		r()
	}
}

func TestPaneFlowBackpressure(t *testing.T) {
	f := newPaneFlow()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if !f.waitRoom(ctx, paneOutWindow) {
		t.Fatal("initial window should allow write")
	}
	done := make(chan bool, 1)
	go func() {
		done <- f.waitRoom(ctx, 1024)
	}()
	f.ack(1024)
	if ok := <-done; !ok {
		t.Fatal("ack should unblock writer")
	}
}
