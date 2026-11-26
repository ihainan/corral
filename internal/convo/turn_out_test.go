package convo

import (
	"strconv"
	"testing"
)

// The turn under way's tokens (Meta.TurnOut), as Claude Code's spinner
// counts them: each answer since the person's last message once, however
// many rows it is written as; from nought at their next message; and the
// same when the log is read in two parts and the turn began in the first.
func TestClaudeTurnOut(t *testing.T) {
	said := func(req, at string, out int64) string {
		return `{"type":"assistant","uuid":"a` + req + at + `","requestId":"` + req + `","timestamp":"2026-11-26T02:00:` + at + `.000Z",` +
			`"message":{"model":"claude-opus-5-5","usage":{"input_tokens":2,"output_tokens":` + strconv.FormatInt(out, 10) + `},"content":[{"type":"text","text":"ok"}]}}` + "\n"
	}
	asked := func(uuid, text string) string {
		return `{"type":"user","uuid":"` + uuid + `","timestamp":"2026-11-26T02:00:00.000Z","message":{"role":"user","content":"` + text + `"}}` + "\n"
	}
	turn := asked("u1", "第一轮") + said("r1", "01", 100) + said("r1", "02", 100) + said("r2", "03", 40) + said("r2", "04", 40) + said("r3", "05", 7)
	c := NewClaude()
	c.Feed([]byte(turn))
	if got := c.Meta().TurnOut; got != 147 {
		t.Fatalf("one turn, three answers in five rows: %d, want 147", got)
	}
	// (An answer's later row saying more of it: the more.)
	c.Feed([]byte(said("r3", "06", 9)))
	if got := c.Meta().TurnOut; got != 149 {
		t.Fatalf("the answer being written grew: %d, want 149", got)
	}
	// The person's next message: counted from nought.
	c.Feed([]byte(asked("u2", "第二轮")))
	if got := c.Meta().TurnOut; got != 0 {
		t.Fatalf("a new turn: %d, want 0", got)
	}
	c.Feed([]byte(said("r4", "07", 12)))
	if got := c.Meta().TurnOut; got != 12 {
		t.Fatalf("the new turn's first answer: %d, want 12", got)
	}

	// Read in two parts, the turn begun in the first; cut between two rows
	// of one answer (r2), or between two answers.
	for name, cut := range map[string][2]string{
		"between answers": {asked("u1", "第一轮") + said("r1", "01", 100) + said("r1", "02", 100), said("r2", "03", 40) + said("r2", "04", 40) + said("r3", "05", 7)},
		"inside answer":   {asked("u1", "第一轮") + said("r1", "01", 100) + said("r2", "03", 40), said("r2", "04", 40) + said("r3", "05", 7)},
		"no answer after": {asked("u1", "第一轮") + said("r1", "01", 100) + said("r2", "03", 40) + said("r3", "05", 7), `{"type":"ai-title","aiTitle":"x"}` + "\n"},
	} {
		before, after := NewClaude(), NewClaude()
		before.Feed([]byte(cut[0]))
		after.Feed([]byte(cut[1]))
		after.Prepend(before)
		if got := after.Meta().TurnOut; got != 147 {
			t.Fatalf("%s: %d, want 147", name, got)
		}
		// (And the next row of the answer under way still counted once.)
		after.Feed([]byte(said("r3", "06", 9)))
		if got := after.Meta().TurnOut; got != 149 {
			t.Fatalf("%s, then more of the answer: %d, want 149", name, got)
		}
	}
	// The second part with a message of the person's of its own: the first
	// part's turn is over, none of it counted.
	before, after := NewClaude(), NewClaude()
	before.Feed([]byte(asked("u1", "第一轮") + said("r1", "01", 100)))
	after.Feed([]byte(asked("u2", "第二轮") + said("r2", "03", 5)))
	after.Prepend(before)
	if got := after.Meta().TurnOut; got != 5 {
		t.Fatalf("a turn of the second part's own: %d, want 5", got)
	}
}
