package convo

import (
	"fmt"
	"strings"
	"testing"
)

// A Codex conversation of turns A, B, C (B with a message sent mid-turn),
// as rows; each turn: started, the person's message, an answer, complete.
func codexTurns(t *testing.T) []string {
	turn := func(id, said string, mid string) []string {
		rows := []string{
			codexLine(t, "event_msg", map[string]any{"type": "task_started"}),
			codexLine(t, "event_msg", map[string]any{"type": "item_completed", "item": map[string]any{"type": "UserMessage", "id": "u" + id, "content": []any{map[string]any{"type": "text", "text": said}}}}),
			codexLine(t, "response_item", map[string]any{"type": "message", "id": "m" + id, "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": "on " + said}}}),
		}
		if mid != "" {
			rows = append(rows,
				codexLine(t, "event_msg", map[string]any{"type": "item_completed", "item": map[string]any{"type": "UserMessage", "id": "v" + id, "content": []any{map[string]any{"type": "text", "text": mid}}}}),
				codexLine(t, "response_item", map[string]any{"type": "message", "id": "n" + id, "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": "and " + mid}}}),
			)
		}
		return append(rows, codexLine(t, "event_msg", map[string]any{"type": "task_complete"}))
	}
	var rows []string
	rows = append(rows, turn("A", "a", "")...)
	rows = append(rows, turn("B", "b", "b too")...)
	rows = append(rows, turn("C", "c", "")...)
	return rows
}

func codexBack(t *testing.T, n int) string {
	return codexLine(t, "event_msg", map[string]any{"type": "thread_rolled_back", "num_turns": n})
}

func codexGone(c *Codex) string {
	var out []string
	for _, e := range c.entries {
		if e.Gone {
			out = append(out, e.ID)
		}
	}
	return strings.Join(out, " ")
}

func TestCodexBacktrack(t *testing.T) {
	rows := codexTurns(t)
	// One turn back: C, its message and answer.
	c := NewCodex()
	c.Feed([]byte(strings.Join(append(append([]string(nil), rows...), codexBack(t, 1)), "")))
	if g := codexGone(c); g != "uC mC" {
		t.Fatalf("one back: %q", g)
	}
	// Then another: B, with what was sent during it — not C again.
	rev := c.Rev()
	c.Feed([]byte(codexBack(t, 1)))
	if g := codexGone(c); g != "uB mB vB nB uC mC" {
		t.Fatalf("two backtracks: %q", g)
	}
	if ch := c.Since(rev); len(ch) != 4 {
		t.Fatalf("told as changed: %+v", ch)
	}
	// Two turns at once.
	d := NewCodex()
	d.Feed([]byte(strings.Join(append(append([]string(nil), rows...), codexBack(t, 2)), "")))
	if g := codexGone(d); g != "uB mB vB nB uC mC" {
		t.Fatalf("two at once: %q", g)
	}
	// A new turn after a backtrack, then a backtrack: the new one goes.
	e := NewCodex()
	more := append(append([]string(nil), rows...), codexBack(t, 1))
	more = append(more,
		codexLine(t, "event_msg", map[string]any{"type": "task_started"}),
		codexLine(t, "event_msg", map[string]any{"type": "item_completed", "item": map[string]any{"type": "UserMessage", "id": "uD", "content": []any{map[string]any{"type": "text", "text": "d"}}}}),
		codexLine(t, "event_msg", map[string]any{"type": "task_complete"}),
		codexBack(t, 1),
	)
	e.Feed([]byte(strings.Join(more, "")))
	if g := codexGone(e); g != "uC mC uD" {
		t.Fatalf("again after a new turn: %q", g)
	}
}

// The backtrack read in the last part, the turns it takes back begun in
// the part before: told once that part is put before, wherever it is cut.
func TestCodexBacktrackAcrossPrepend(t *testing.T) {
	rows := append(codexTurns(t), codexBack(t, 2))
	for cut := 1; cut < len(rows); cut++ {
		before, after := NewCodex(), NewCodex()
		before.Feed([]byte(strings.Join(rows[:cut], "")))
		after.Feed([]byte(strings.Join(rows[cut:], "")))
		after.Prepend(before)
		if g := codexGone(after); g != "uB mB vB nB uC mC" {
			t.Fatalf("cut at %d: %q", cut, g)
		}
	}
}

// The part read first begins with a turn's message, the turn begun at the
// end of the part before; the backtrack comes after both are read: it
// takes back that turn, not the one before it.
func TestCodexBacktrackAfterPrepend(t *testing.T) {
	rows := codexTurns(t)
	for cut := 1; cut < len(rows); cut++ {
		before, after := NewCodex(), NewCodex()
		before.Feed([]byte(strings.Join(rows[:cut], "")))
		after.Feed([]byte(strings.Join(rows[cut:], "")))
		after.Prepend(before)
		after.Feed([]byte(codexBack(t, 1)))
		if g := codexGone(after); g != "uC mC" {
			t.Fatalf("cut at %d: %q", cut, g)
		}
	}
}

// The part before ends with a turn begun; the part read first has nothing
// to show yet (settings only): the turn begins with what comes next.
func TestCodexBacktrackOpenTurnEmptyPart(t *testing.T) {
	rows := codexTurns(t)
	// (A whole, then B's task_started only.)
	before, after := NewCodex(), NewCodex()
	before.Feed([]byte(strings.Join(rows[:5], "")))
	after.Feed([]byte(codexLine(t, "turn_context", map[string]any{"model": "gpt-6.1-sol"})))
	after.Prepend(before)
	after.Feed([]byte(strings.Join(rows[5:9], "")))
	after.Feed([]byte(codexBack(t, 1)))
	if g := codexGone(after); g != "uB mB vB nB" {
		t.Fatalf("gone %q", g)
	}
}

// Put before past the bound on entries: a turn whose start is left out
// keeps the rest; a backtrack of it takes all of it back.
func TestCodexBacktrackTrimmedStart(t *testing.T) {
	msg := func(id string) string {
		return codexLine(t, "response_item", map[string]any{"type": "message", "id": id, "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": id}}})
	}
	var old []string
	old = append(old, codexLine(t, "event_msg", map[string]any{"type": "task_started"}),
		codexLine(t, "event_msg", map[string]any{"type": "item_completed", "item": map[string]any{"type": "UserMessage", "id": "uA", "content": []any{map[string]any{"type": "text", "text": "a"}}}}),
		codexLine(t, "event_msg", map[string]any{"type": "task_complete"}),
		codexLine(t, "event_msg", map[string]any{"type": "task_started"}),
		codexLine(t, "event_msg", map[string]any{"type": "item_completed", "item": map[string]any{"type": "UserMessage", "id": "uB", "content": []any{map[string]any{"type": "text", "text": "b"}}}}))
	for i := 0; i < 30; i++ {
		old = append(old, msg(fmt.Sprintf("mB%d", i)))
	}
	var now []string
	for i := 0; i < maxEntries-10; i++ {
		now = append(now, msg(fmt.Sprintf("tail%d", i)))
	}
	before, after := NewCodex(), NewCodex()
	before.Feed([]byte(strings.Join(old, "")))
	after.Feed([]byte(strings.Join(now, "")))
	after.Prepend(before)
	after.Feed([]byte(codexBack(t, 1)))
	for _, e := range after.entries {
		if !e.Gone {
			t.Fatalf("%s still shown after B was taken back", e.ID)
		}
	}
}

// The part before is past the bound itself (its oldest entries left out
// as it was read, the start of its last turn with them): a backtrack of
// that turn after it is put before takes all of it back.
func TestCodexBacktrackPartTrimmedItself(t *testing.T) {
	msg := func(id string) string {
		return codexLine(t, "response_item", map[string]any{"type": "message", "id": id, "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": id}}})
	}
	old := []string{
		codexLine(t, "event_msg", map[string]any{"type": "task_started"}),
		codexLine(t, "event_msg", map[string]any{"type": "item_completed", "item": map[string]any{"type": "UserMessage", "id": "uB", "content": []any{map[string]any{"type": "text", "text": "b"}}}}),
	}
	for i := 0; i < maxEntries+5; i++ {
		old = append(old, msg(fmt.Sprintf("mB%d", i)))
	}
	before, after := NewCodex(), NewCodex()
	before.Feed([]byte(strings.Join(old, "")))
	after.Feed([]byte(codexLine(t, "turn_context", map[string]any{"model": "gpt-6.1-sol"}))) // (room for all of the part before)
	after.Prepend(before)
	after.Feed([]byte(msg("tail") + codexBack(t, 1)))
	for _, e := range after.entries {
		if !e.Gone {
			t.Fatalf("%s still shown after the turn was taken back", e.ID)
		}
	}
}
