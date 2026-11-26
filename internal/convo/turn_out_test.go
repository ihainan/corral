package convo

import (
	"strconv"
	"strings"
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
	// An answer cut in two whose first part said more (review b9-2), and an
	// answer's rows apart, another answer between them (b9-3): once, the most.
	before, after := NewClaude(), NewClaude()
	before.Feed([]byte(asked("u1", "第一轮") + said("r1", "01", 100)))
	after.Feed([]byte(said("r1", "02", 40) + said("r2", "03", 7)))
	after.Prepend(before)
	if got := after.Meta().TurnOut; got != 107 {
		t.Fatalf("cut in two, the larger before: %d, want 107", got)
	}
	c = NewClaude()
	c.Feed([]byte(asked("u1", "第一轮") + said("r1", "01", 100) + said("r2", "02", 40) + said("r1", "03", 110)))
	if got := c.Meta().TurnOut; got != 150 {
		t.Fatalf("rows of one answer apart: %d, want 150", got)
	}
	// Three parts, the turn begun in the first.
	p1, p2, p3 := NewClaude(), NewClaude(), NewClaude()
	p1.Feed([]byte(asked("u1", "第一轮") + said("r1", "01", 100)))
	p2.Feed([]byte(said("r1", "02", 100) + said("r2", "03", 40)))
	p3.Feed([]byte(said("r2", "04", 40) + said("r3", "05", 7)))
	p2.Prepend(p1)
	p3.Prepend(p2)
	if got := p3.Meta().TurnOut; got != 147 {
		t.Fatalf("three parts: %d, want 147", got)
	}
	// The second part with a message of the person's of its own: the first
	// part's turn is over, none of it counted.
	before, after = NewClaude(), NewClaude()
	before.Feed([]byte(asked("u1", "第一轮") + said("r1", "01", 100)))
	after.Feed([]byte(asked("u2", "第二轮") + said("r2", "03", 5)))
	after.Prepend(before)
	if got := after.Meta().TurnOut; got != 5 {
		t.Fatalf("a turn of the second part's own: %d, want 5", got)
	}
	// A turn of more answers than are kept apart, cut in two: joined, the
	// later part's latest answer is still known, so a further row of it read
	// after is not counted again (review, 11-26).
	var many strings.Builder
	many.WriteString(asked("u1", "长的一轮"))
	for i := 0; i < maxTurnAnswers-1; i++ {
		many.WriteString(said("q"+strconv.Itoa(i), "01", 1))
	}
	before, after = NewClaude(), NewClaude()
	before.Feed([]byte(many.String()))
	after.Feed([]byte(said("z1", "02", 500) + said("z2", "03", 500)))
	after.Prepend(before)
	want := int64(maxTurnAnswers - 1 + 1000)
	if got := after.Meta().TurnOut; got != want {
		t.Fatalf("joined, many answers: %d, want %d", got, want)
	}
	after.Feed([]byte(said("z2", "04", 500)))
	if got := after.Meta().TurnOut; got != want {
		t.Fatalf("a further row of the latest answer, after the join: %d, want %d", got, want)
	}
}
