package convo

import (
	"encoding/json"
	"strings"
	"testing"
)

// What a "!" command printed: a shell part, its text as printed (Claude
// Code's &gt; and the like undone), a long one kept by its start and end,
// stderr apart.
func TestClaudeShellOutput(t *testing.T) {
	n := 0
	row := func(content string) string {
		n++
		b, _ := json.Marshal(map[string]any{"type": "user", "uuid": "u" + string(rune('0'+n)), "timestamp": "2026-10-29T01:00:00Z", "message": map[string]any{"role": "user", "content": content}})
		return string(b) + "\n"
	}
	long := strings.Repeat("line &gt;&gt;&gt; x &amp; y\n", 2000)
	c := NewClaude()
	c.Feed([]byte(row("<bash-input>python3 t.py</bash-input>") + row("<bash-stdout>"+long+"</bash-stdout><bash-stderr>warn: &lt;x&gt;</bash-stderr>") + row("<bash-stdout></bash-stdout><bash-stderr></bash-stderr>")))
	es := c.Since(0)
	if len(es) != 2 {
		for _, e := range es {
			t.Logf("%s %+v", e.Role, e.Parts)
		}
		t.Fatalf("%d entries (nothing printed: none)", len(es))
	}
	if es[0].Role != RoleUser || es[0].Parts[0].Text != "! python3 t.py" {
		t.Fatalf("the command: %+v", es[0])
	}
	p := es[1].Parts[0]
	if es[1].Role != RoleNote || p.Kind != KindShell || p.Shell == nil {
		t.Fatalf("not a shell part: %+v", es[1])
	}
	sh := p.Shell
	if !strings.HasPrefix(sh.Out, "line >>> x & y\n") || strings.Contains(sh.Out+sh.OutTail, "&gt;") {
		t.Fatalf("not as printed: %.40q", sh.Out)
	}
	if !sh.Truncated || sh.OutTail == "" || len(sh.Out)+len(sh.OutTail) > maxResult+64 {
		t.Fatalf("a long one: start %d, end %d, cut %v", len(sh.Out), len(sh.OutTail), sh.Truncated)
	}
	if sh.Err != "warn: <x>" {
		t.Fatalf("stderr: %q", sh.Err)
	}
	// (The log may give the row's content as blocks: the same.)
	c = NewClaude()
	b, _ := json.Marshal(map[string]any{"type": "user", "uuid": "b1", "timestamp": "2026-10-29T01:00:00Z", "message": map[string]any{"role": "user", "content": []map[string]any{{"type": "text", "text": "<bash-stdout>a &gt; b</bash-stdout><bash-stderr></bash-stderr>"}}}})
	c.Feed(append(b, '\n'))
	if es := c.Since(0); len(es) != 1 || es[0].Parts[0].Kind != KindShell || es[0].Parts[0].Shell.Out != "a > b" {
		t.Fatalf("as blocks: %+v", es)
	}
}

// A paste comes wrapped, with newlines around: none shown at the message's
// ends, what it holds kept.
func TestUnpastedEnds(t *testing.T) {
	in := "\n\n<pasted_content id=\"1790\">\n按照你的指导配置了 workbuddy\n\n  发了一个鸡兔同笼。\n</pasted_content id=\"1790\">\n"
	if got := unpasted(in); got != "按照你的指导配置了 workbuddy\n\n  发了一个鸡兔同笼。" {
		t.Fatalf("%q", got)
	}
	if got := unpasted("<pasted_content id=\"a\">\n    indented code\n</pasted_content id=\"a\">"); got != "    indented code" {
		t.Fatalf("its first line's indent: %q", got)
	}
}

// A long message waiting, typed with blank lines first, arriving as a paste
// (the wrapper's blank lines let go): the same one, cut at the same place.
func TestQueuedLongPasteArrives(t *testing.T) {
	body := strings.Repeat("x", maxText+10)
	enq, _ := json.Marshal(map[string]any{"type": "queue-operation", "operation": "enqueue", "content": "\n\n" + body})
	user, _ := json.Marshal(map[string]any{"type": "user", "uuid": "u1", "message": map[string]any{"role": "user", "content": "<pasted_content id=\"7\">\n\n" + body + "\n</pasted_content id=\"7\">"}})
	c := NewClaude()
	c.Feed(append(append(enq, '\n'), append(user, '\n')...))
	if q := c.Queued(); len(q) != 0 {
		t.Fatalf("still waiting: %d", len(q))
	}
	// (Taken off the queue before it came: forgotten when it comes.)
	rm, _ := json.Marshal(map[string]any{"type": "queue-operation", "operation": "remove", "content": "\n\n" + body})
	user2, _ := json.Marshal(map[string]any{"type": "user", "uuid": "u2", "message": map[string]any{"role": "user", "content": "<pasted_content id=\"8\">\n\n" + body + "\n</pasted_content id=\"8\">"}})
	c = NewClaude()
	c.Feed(append(append(append(enq, '\n'), append(rm, '\n')...), append(user2, '\n')...))
	if len(c.Queued()) != 0 || len(c.gone) != 0 {
		t.Fatalf("removed, then arrived: queue %d, gone %d", len(c.Queued()), len(c.gone))
	}
	// Arriving as blocks, unwrapped (cut with its blank lines): the same one;
	// as a row of its own and as one taken in during a turn; waiting and
	// removed.
	blocks, _ := json.Marshal(map[string]any{"type": "user", "uuid": "u3", "message": map[string]any{"role": "user", "content": []map[string]any{{"type": "text", "text": "\n\n" + body}}}})
	taken, _ := json.Marshal(map[string]any{"type": "attachment", "uuid": "u4", "attachment": map[string]any{"type": "queued_command", "commandMode": "prompt", "prompt": []map[string]any{{"type": "text", "text": "\n\n" + body}}}})
	for name, arrive := range map[string][]byte{"blocks": blocks, "taken in": taken} {
		c = NewClaude()
		c.Feed(append(append(enq, '\n'), append(arrive, '\n')...))
		if q := c.Queued(); len(q) != 0 {
			t.Fatalf("%s: still waiting: %d", name, len(q))
		}
		c = NewClaude()
		c.Feed(append(append(append(enq, '\n'), append(rm, '\n')...), append(arrive, '\n')...))
		if len(c.gone) != 0 {
			t.Fatalf("%s: removed, then arrived: gone %d", name, len(c.gone))
		}
	}
	// (Two long ones of other words: each its own.)
	other, _ := json.Marshal(map[string]any{"type": "user", "uuid": "u5", "message": map[string]any{"role": "user", "content": strings.Repeat("y", maxText+10)}})
	c = NewClaude()
	c.Feed(append(append(enq, '\n'), append(other, '\n')...))
	if len(c.Queued()) != 1 {
		t.Fatalf("another long one arrived: %d waiting", len(c.Queued()))
	}
	// (A short one whose words begin a long one waiting: not it.)
	short, _ := json.Marshal(map[string]any{"type": "user", "uuid": "u6", "message": map[string]any{"role": "user", "content": "xxxx"}})
	c = NewClaude()
	c.Feed(append(append(enq, '\n'), append(short, '\n')...))
	if len(c.Queued()) != 1 {
		t.Fatalf("a short one arrived: %d waiting", len(c.Queued()))
	}
	// (Two long ones, neither cut, one the other's start and more: the one
	// arriving is the one of its words, not the other; waiting and removed.)
	a := strings.Repeat("x", maxText-4<<10+1)
	row := func(op, text string) []byte {
		b, _ := json.Marshal(map[string]any{"type": "queue-operation", "operation": op, "content": text})
		return append(b, '\n')
	}
	said := func(uuid, text string) []byte {
		b, _ := json.Marshal(map[string]any{"type": "user", "uuid": uuid, "message": map[string]any{"role": "user", "content": text}})
		return append(b, '\n')
	}
	c = NewClaude()
	c.Feed(append(append(row("enqueue", a), row("enqueue", a+"different")...), said("u7", a+"different")...))
	if q := c.Queued(); len(q) != 1 || q[0].Text != a {
		t.Fatalf("not cut, one the other's start: %d waiting", len(q))
	}
	c = NewClaude()
	c.Feed(append(append(append(row("enqueue", a), row("remove", a)...), row("enqueue", a+"different")...), said("u8", a+"different")...))
	if len(c.Queued()) != 0 || len(c.gone) != 1 {
		t.Fatalf("not cut, the other removed: queue %d, gone %d", len(c.Queued()), len(c.gone))
	}
	// (Control characters in its words: let go of on both sides.)
	c = NewClaude()
	c.Feed(append(row("enqueue", "ab\u0000cd"), said("u9", "<pasted_content id=\"9\">ab\u0000cd</pasted_content id=\"9\">")...))
	if len(c.Queued()) != 0 {
		t.Fatalf("control characters: still waiting")
	}
	// (Per review404. One just under the cut, never cut, and one of its
	// words and more: not taken for each other.)
	u := strings.Repeat("x", maxText-3)
	c = NewClaude()
	c.Feed(append(row("enqueue", u), said("u10", u+"yz")...))
	if q := c.Queued(); len(q) != 1 || q[0].Text != u {
		t.Fatalf("just under the cut: %d waiting", len(q))
	}
	// (One removed, cut, its words the start of one waiting of the same
	// words as the arrival: that one goes, the removed is kept.)
	c = NewClaude()
	cutA, wholeB := "  "+strings.Repeat("x", maxText+10), strings.Repeat("x", maxText-2)+"z"
	c.Feed(append(append(append(row("enqueue", cutA), row("remove", cutA)...), row("enqueue", wholeB)...), said("u11", wholeB)...))
	if len(c.Queued()) != 0 || len(c.gone) != 1 {
		t.Fatalf("the same words before a cut one: queue %d, gone %d", len(c.Queued()), len(c.gone))
	}
	// (Arriving as two blocks, the first cut: what follows it not compared.)
	two, _ := json.Marshal(map[string]any{"type": "user", "uuid": "u12", "message": map[string]any{"role": "user", "content": []map[string]any{{"type": "text", "text": "\n\n" + body}, {"type": "text", "text": "tail"}}}})
	c = NewClaude()
	c.Feed(append(row("enqueue", "\n\n"+body+"\ntail"), append(two, '\n')...))
	if len(c.Queued()) != 0 {
		t.Fatalf("two blocks, the first cut: still waiting")
	}
	// (Per review405: the same, arriving as Claude Code's own message.)
	meta, _ := json.Marshal(map[string]any{"type": "user", "uuid": "u13", "isMeta": true, "promptSource": "system", "message": map[string]any{"role": "user", "content": wholeB}})
	c = NewClaude()
	c.Feed(append(append(append(row("enqueue", cutA), row("remove", cutA)...), row("enqueue", wholeB)...), append(meta, '\n')...))
	if len(c.Queued()) != 0 || len(c.gone) != 1 {
		t.Fatalf("its own message, the same words before a cut one: queue %d, gone %d", len(c.Queued()), len(c.gone))
	}
}
