package convo

import "testing"

// /model's word is the model shown until an answer names its id; a
// compaction leaves how full the context is unknown, and the part before
// does not bring the old number back.
func TestReview231Meta(t *testing.T) {
	c := NewClaude()
	c.Feed([]byte(`{"type":"assistant","uuid":"a1","message":{"role":"assistant","model":"claude-sonnet-4-6","content":[{"type":"text","text":"hi"}],"usage":{"input_tokens":100000}}}
{"type":"user","uuid":"u2","message":{"role":"user","content":"<local-command-stdout>Set model to ` + "`Opus 5.5 (default)`" + ` and saved as your default for new sessions</local-command-stdout>"}}
`))
	if m := c.Meta(); m.Model != "Opus 5.5" || m.Context != 100000 {
		t.Fatalf("after /model: %+v", m)
	}
	c.Feed([]byte(`{"type":"user","uuid":"u3","message":{"role":"user","content":"<local-command-stdout>Set model to Fable 5 and saved as your default for new sessions</local-command-stdout>"}}
{"type":"user","uuid":"u4","isCompactSummary":true,"message":{"role":"user","content":"This session is being continued…"}}
`))
	if m := c.Meta(); m.Model != "Fable 5" || m.Context != 0 {
		t.Fatalf("after compaction: %+v", m)
	}
	o := NewClaude()
	o.Feed([]byte(`{"type":"assistant","uuid":"a0","message":{"role":"assistant","model":"claude-sonnet-4-6","content":[{"type":"text","text":"x"}],"usage":{"input_tokens":90000}}}
`))
	c.Prepend(o)
	if m := c.Meta(); m.Context != 0 || m.Model != "Fable 5" {
		t.Fatalf("after the part before: %+v", m)
	}
	c.Feed([]byte(`{"type":"assistant","uuid":"a5","message":{"role":"assistant","model":"claude-fable-5","content":[{"type":"text","text":"y"}],"usage":{"input_tokens":20000}}}
`))
	if m := c.Meta(); m.Model != "claude-fable-5" || m.Context != 20000 {
		t.Fatalf("after an answer: %+v", m)
	}
}

// A message with a picture, queued with its "[Image #1]", taken in without it.
func TestReview231QueuedImage(t *testing.T) {
	c := NewClaude()
	c.QueueSince(0)
	c.Feed([]byte(`{"type":"queue-operation","operation":"enqueue","timestamp":"2099-01-01T00:00:00Z","content":"[Image #1] look"}
{"type":"queue-operation","operation":"dequeue","timestamp":"2099-01-01T00:00:01Z"}
{"type":"user","uuid":"u1","timestamp":"2099-01-01T00:00:01Z","message":{"role":"user","content":[{"type":"text","text":"look"},{"type":"image","source":{"type":"base64","data":""}}]}}
`))
	if q := c.Queued(); len(q) != 0 {
		t.Fatalf("still queued: %+v", q)
	}
}

// The newer part says there is no branch: the part before does not bring one back.
func TestReview231BranchCleared(t *testing.T) {
	c := NewClaude()
	c.Feed([]byte(`{"type":"user","uuid":"u2","gitBranch":"","message":{"role":"user","content":"hi"}}
`))
	o := NewClaude()
	o.Feed([]byte(`{"type":"user","uuid":"u1","gitBranch":"main","message":{"role":"user","content":"before"}}
`))
	c.Prepend(o)
	if b := c.Meta().Branch; b != "" {
		t.Fatalf("branch %q", b)
	}
	d := NewClaude()
	d.Feed([]byte(`{"type":"user","uuid":"u3","message":{"role":"user","content":"no word of it"}}
`))
	o2 := NewClaude()
	o2.Feed([]byte(`{"type":"user","uuid":"u1","gitBranch":"main","message":{"role":"user","content":"before"}}
`))
	d.Prepend(o2)
	if b := d.Meta().Branch; b != "main" {
		t.Fatalf("not filled in: %q", b)
	}
}

// Compacted, as a part read before says: the part before that does not bring the old number back.
func TestReview232ContextThroughPrepend(t *testing.T) {
	c := NewClaude()
	c.Feed([]byte(`{"type":"user","uuid":"u9","message":{"role":"user","content":"now"}}
`))
	mid := NewClaude()
	mid.Feed([]byte(`{"type":"user","uuid":"u5","isCompactSummary":true,"message":{"role":"user","content":"summary"}}
`))
	c.Prepend(mid)
	old := NewClaude()
	old.Feed([]byte(`{"type":"assistant","uuid":"a1","message":{"role":"assistant","model":"m","content":[{"type":"text","text":"x"}],"usage":{"input_tokens":100000}}}
`))
	c.Prepend(old)
	if n := c.Meta().Context; n != 0 {
		t.Fatalf("context %d", n)
	}
}
