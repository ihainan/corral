package convo

import (
	"strconv"
	"testing"
)

// The latest answer's tokens and how long it took (Meta.Turn): from the
// api-request row that sent it to the last row of the answer, counted once
// however many rows the answer was written as.
func TestClaudeAnswer(t *testing.T) {
	sent := func(at string) string {
		return `{"type":"api-request","id":"r","timestamp":"` + at + `","querySource":"repl_main_thread","params":{"model":"claude-opus-5-5","betas":[]}}` + "\n"
	}
	// (An answer's row: its request, when it was whole, the tokens the
	// whole answer came to — the same on every row of it.)
	said := func(req, at string, out int64, extra string) string {
		return `{"type":"assistant","uuid":"a` + at + `","requestId":"` + req + `","timestamp":"` + at + `",` + extra +
			`"message":{"model":"claude-opus-5-5","stop_reason":"end_turn","usage":{"input_tokens":2,"cache_creation_input_tokens":30,"cache_read_input_tokens":1000,"output_tokens":` + strconv.FormatInt(out, 10) + `},"content":[{"type":"text","text":"ok"}]}}` + "\n"
	}
	// One answer, two rows of it: its tokens once, and the time to the last.
	c := NewClaude()
	c.Feed([]byte(sent("2026-11-09T06:00:00.000Z") +
		said("req_1", "2026-11-09T06:00:04.000Z", 1200, "") +
		said("req_1", "2026-11-09T06:00:10.000Z", 1200, "")))
	if answer := c.Meta().Answer; answer == nil || answer.Out != 1200 || answer.Ms != 10000 {
		t.Fatalf("one answer in two rows: %+v", answer)
	}
	// The next answer stands in its place, timed from its own request.
	c.Feed([]byte(sent("2026-11-09T06:01:00.000Z") + said("req_2", "2026-11-09T06:01:02.500Z", 80, "")))
	if answer := c.Meta().Answer; answer == nil || answer.Out != 80 || answer.Ms != 2500 {
		t.Fatalf("the next answer: %+v", answer)
	}
	// A request sent and not answered yet leaves the answer before it as it
	// was: what is shown is the last answer there was, not nothing.
	c.Feed([]byte(sent("2026-11-09T06:02:00.000Z")))
	if answer := c.Meta().Answer; answer == nil || answer.Out != 80 || answer.Ms != 2500 {
		t.Fatalf("while the next is under way: %+v", answer)
	}
	// No request row read: the tokens still, nothing of how long.
	c = NewClaude()
	c.Feed([]byte(said("req_1", "2026-11-09T06:00:04.000Z", 300, "")))
	if answer := c.Meta().Answer; answer == nil || answer.Out != 300 || answer.Ms != 0 {
		t.Fatalf("no request row: %+v", answer)
	}
	// Further apart than maxAnswerTook: the two are not taken for one pair.
	c = NewClaude()
	c.Feed([]byte(sent("2026-11-09T04:00:00.000Z") + said("req_1", "2026-11-09T06:00:00.000Z", 300, "")))
	if answer := c.Meta().Answer; answer == nil || answer.Ms != 0 {
		t.Fatalf("hours apart: %+v", answer)
	}
	// Out of order (the answer before the request that followed it).
	c = NewClaude()
	c.Feed([]byte(sent("2026-11-09T06:00:05.000Z") + said("req_1", "2026-11-09T06:00:04.000Z", 300, "")))
	if answer := c.Meta().Answer; answer == nil || answer.Ms != 0 {
		t.Fatalf("answered before it was sent: %+v", answer)
	}
	// A row Claude Code wrote itself, and a subagent's row: neither is an
	// answer of this session's.
	c = NewClaude()
	c.Feed([]byte(sent("2026-11-09T06:00:00.000Z") +
		`{"type":"assistant","uuid":"s1","requestId":"req_s","timestamp":"2026-11-09T06:00:01.000Z","message":{"model":"<synthetic>","usage":{"output_tokens":99},"content":[{"type":"text","text":"API Error"}]}}` + "\n" +
		said("req_sub", "2026-11-09T06:00:02.000Z", 500, `"isSidechain":true,`)))
	if answer := c.Meta().Answer; answer != nil {
		t.Fatalf("of a row that is not an answer: %+v", answer)
	}
	// No requestId: nothing to tell one answer's rows from another's.
	c = NewClaude()
	c.Feed([]byte(sent("2026-11-09T06:00:00.000Z") + said("", "2026-11-09T06:00:02.000Z", 500, "")))
	if answer := c.Meta().Answer; answer != nil {
		t.Fatalf("without a requestId: %+v", answer)
	}
	// Nothing of the kind read at all: nil, not nought.
	c = NewClaude()
	c.Feed([]byte(`{"type":"user","uuid":"u1","timestamp":"2026-11-09T06:00:00.000Z","message":{"content":"hi"}}` + "\n"))
	if c.Meta().Answer != nil {
		t.Fatal("an answer from nothing")
	}
	// Prepend: the later part's answer stands; where that part has none,
	// the part before it gives the latest answer there was.
	older := NewClaude()
	older.Feed([]byte(sent("2026-11-09T06:00:00.000Z") + said("req_old", "2026-11-09T06:00:06.000Z", 600, "")))
	later := NewClaude()
	later.Feed([]byte(sent("2026-11-09T06:10:00.000Z") + said("req_new", "2026-11-09T06:10:03.000Z", 90, "")))
	later.Prepend(older)
	if answer := later.Meta().Answer; answer == nil || answer.Out != 90 {
		t.Fatalf("the later part's: %+v", answer)
	}
	none := NewClaude()
	none.Feed([]byte(`{"type":"user","uuid":"u2","timestamp":"2026-11-09T06:10:00.000Z","message":{"content":"hi"}}` + "\n"))
	none.Prepend(older)
	if answer := none.Meta().Answer; answer == nil || answer.Out != 600 || answer.Ms != 6000 {
		t.Fatalf("the part before, where the later has none: %+v", answer)
	}
}
