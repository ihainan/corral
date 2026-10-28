package convo

import (
	"fmt"
	"sort"
	"strings"
	"testing"
)

// treeRow is a row of a Claude Code log as the tree tests write it.
func treeRow(kind, id, parent, body string) string {
	p := `null`
	if parent != "" {
		p = `"` + parent + `"`
	}
	switch kind {
	case "user":
		return fmt.Sprintf(`{"type":"user","uuid":%q,"parentUuid":%s,"message":{"role":"user","content":%q}}`, id, p, body)
	case "text":
		return fmt.Sprintf(`{"type":"assistant","uuid":%q,"parentUuid":%s,"message":{"content":[{"type":"text","text":%q}]}}`, id, p, body)
	case "call":
		return fmt.Sprintf(`{"type":"assistant","uuid":%q,"parentUuid":%s,"message":{"content":[{"type":"tool_use","id":%q,"name":"Bash","input":{"command":"true"}}]}}`, id, p, body)
	case "result":
		return fmt.Sprintf(`{"type":"user","uuid":%q,"parentUuid":%s,"message":{"content":[{"type":"tool_result","tool_use_id":%q,"content":"ok"}]}}`, id, p, body)
	case "note":
		return fmt.Sprintf(`{"type":"attachment","uuid":%q,"parentUuid":%s,"attachment":{"type":"total_tokens_reminder"}}`, id, p)
	}
	panic(kind)
}

// A conversation: a turn with two calls made together, a second message and
// some work on it, stopped; then the second message sent again in other
// words, hanging where the second one did: what came of the first try is
// taken back.
var treeLog = []string{
	treeRow("user", "u1", "", "first"),
	treeRow("text", "a1", "u1", "looking"),
	treeRow("call", "a2", "a1", "t1"),
	treeRow("call", "a3", "a2", "t2"), // (made with t1: it hangs on t1's call, as t1's result does)
	treeRow("result", "r1", "a2", "t1"),
	treeRow("result", "r2", "a3", "t2"),
	treeRow("text", "a4", "r2", "done"),
	treeRow("user", "u2", "a4", "second"),
	treeRow("note", "n1", "u2", ""),
	treeRow("text", "a5", "n1", "on it"),
	treeRow("call", "a6", "a5", "t3"),
	treeRow("user", "u3", "a6", "[Request interrupted by user]"),
	treeRow("user", "u4", "a4", "second, said again"),
	treeRow("text", "a7", "u4", "on the new one"),
}

func goneIDs(es []Entry) []string {
	var out []string
	for _, e := range es {
		if e.Gone {
			out = append(out, e.ID)
		}
	}
	sort.Strings(out)
	return out
}

func entriesOf(c *Claude) []Entry {
	out := make([]Entry, len(c.entries))
	for i, e := range c.entries {
		out[i] = *e
	}
	return out
}

func TestClaudeRewindTakesBack(t *testing.T) {
	c := NewClaude()
	c.Feed(lines(treeLog[:12]...))
	if g := goneIDs(entriesOf(c)); len(g) != 0 {
		t.Fatalf("taken back before any rewind: %v", g)
	}
	rev := c.Rev()
	c.Feed(lines(treeLog[12:]...))
	want := []string{"a5", "a6", "u2", "u3"}
	if g := goneIDs(entriesOf(c)); strings.Join(g, " ") != strings.Join(want, " ") {
		t.Fatalf("gone %v, want %v", g, want)
	}
	// (What was taken back is told as changed, with what is new.)
	ch := c.Since(rev)
	if g := goneIDs(ch); strings.Join(g, " ") != strings.Join(want, " ") {
		t.Fatalf("changed and gone %v, want %v", g, want)
	}
	for _, e := range ch {
		if e.ID == "a1" || e.ID == "r1" {
			t.Fatalf("an entry not taken back is told as changed: %+v", e)
		}
	}
}

// The parser reads the log as it comes, in batches of any size: what is
// taken back does not depend on where the batches end.
func TestClaudeRewindAnyBatches(t *testing.T) {
	want := strings.Join([]string{"a5", "a6", "u2", "u3"}, " ")
	for step := 1; step <= len(treeLog); step++ {
		c := NewClaude()
		for i := 0; i < len(treeLog); i += step {
			c.Feed(lines(treeLog[i:min(i+step, len(treeLog))]...))
		}
		if g := strings.Join(goneIDs(entriesOf(c)), " "); g != want {
			t.Fatalf("batches of %d: gone %v, want %v", step, g, want)
		}
	}
}

// The log is read from its end, the part before put before as the person
// scrolls up: a rewind to a row in that part is told once it is read,
// whichever side of the cut what it took back is.
func TestClaudeRewindAcrossPrepend(t *testing.T) {
	want := strings.Join([]string{"a5", "a6", "u2", "u3"}, " ")
	for cut := 1; cut < len(treeLog); cut++ {
		before, after := NewClaude(), NewClaude()
		before.Feed(lines(treeLog[:cut]...))
		after.Feed(lines(treeLog[cut:]...))
		rev := after.Rev()
		had := map[string]bool{}
		for _, id := range goneIDs(entriesOf(after)) {
			had[id] = true
		}
		after.Prepend(before)
		got := entriesOf(after)
		if g := strings.Join(goneIDs(got), " "); g != want {
			t.Fatalf("cut at %d: gone %v, want %v", cut, g, want)
		}
		// (What was taken back of the part read first, once the part before
		// was read, is told as changed.)
		for _, e := range got {
			if e.Gone && !had[e.ID] && e.Rev <= rev {
				t.Fatalf("cut at %d: %s taken back but not told (rev %d ≤ %d)", cut, e.ID, e.Rev, rev)
			}
		}
	}
}

// A part before with nothing to show (attachments only) still tells what
// the rewind took back of the part read.
func TestClaudeRewindPrependNothingToAdd(t *testing.T) {
	before, after := NewClaude(), NewClaude()
	before.Feed(lines(
		treeRow("note", "p0", "", ""),
		treeRow("note", "p1", "p0", ""),
	))
	after.Feed(lines(
		treeRow("user", "x1", "p1", "try"),
		treeRow("text", "x2", "x1", "trying"),
		treeRow("user", "x3", "p1", "try again"),
	))
	// (Not known yet: what x3 hangs on is in the part not read.)
	if g := goneIDs(entriesOf(after)); len(g) != 0 {
		t.Fatalf("gone before the part before was read: %v", g)
	}
	rev := after.Rev()
	after.Prepend(before)
	if g := strings.Join(goneIDs(after.Since(rev)), " "); g != "x1 x2" {
		t.Fatalf("told as gone: %q", g)
	}
}

// Across a compaction a row names the one before it as logicalParentUuid.
func TestClaudeRewindAcrossCompaction(t *testing.T) {
	c := NewClaude()
	c.Feed(lines(
		treeRow("user", "u1", "", "first"),
		treeRow("text", "a1", "u1", "answer"),
		`{"type":"system","subtype":"compact_boundary","uuid":"b1","parentUuid":null,"logicalParentUuid":"a1"}`,
		treeRow("text", "a2", "b1", "after the compaction"),
		treeRow("user", "u2", "a1", "back to before it"),
	))
	// (The compaction too, now shown: rewound to before it, it is undone.)
	if g := strings.Join(goneIDs(entriesOf(c)), " "); g != "a2 b1" {
		t.Fatalf("gone %q", g)
	}
}

// The model's safeguards stopped an answer; another model answered again:
// the first answer is withdrawn by name, read before or after.
func TestClaudeWithdrawn(t *testing.T) {
	rows := []string{
		treeRow("user", "u1", "", "ask"),
		treeRow("text", "a1", "u1", "half an answer"),
		`{"type":"system","subtype":"model_refusal_fallback","uuid":"s1","parentUuid":"a1","content":"stopped","fallbackModel":"claude-sonnet-5-5","retractedMessageUuids":["a1"]}`,
		treeRow("text", "a2", "s1", "the answer"),
	}
	c := NewClaude()
	c.Feed(lines(rows...))
	if g := strings.Join(goneIDs(entriesOf(c)), " "); g != "a1" {
		t.Fatalf("gone %q", g)
	}
	before, after := NewClaude(), NewClaude()
	before.Feed(lines(rows[:2]...))
	after.Feed(lines(rows[2:]...))
	after.Prepend(before)
	if g := strings.Join(goneIDs(entriesOf(after)), " "); g != "a1" {
		t.Fatalf("across the part before: gone %q", g)
	}
}

// What Claude Code writes itself tells of no rewind: /exit's caveat hangs
// where the message before the last does, and the session, resumed, goes
// on from the last (a session of this machine's, as it was written). A
// tool's result never tells of one either.
func TestClaudeRewindNotByMetaNorResult(t *testing.T) {
	c := NewClaude()
	c.Feed(lines(
		treeRow("user", "u1", "", "first"),
		treeRow("text", "a1", "u1", "answer"),
		treeRow("user", "u2", "a1", "second"),
		treeRow("note", "n2", "u2", ""),
		`{"type":"user","uuid":"m1","parentUuid":"a1","isMeta":true,"message":{"content":"<local-command-caveat>Caveat</local-command-caveat>"}}`,
		`{"type":"user","uuid":"m2","parentUuid":"m1","message":{"content":"<command-name>/exit</command-name>"}}`,
		`{"type":"user","uuid":"m3","parentUuid":"n2","isMeta":true,"message":{"content":[{"type":"text","text":"Continue from where you left off."}]}}`,
		treeRow("text", "a3", "m3", "going on with the second"),
	))
	if g := goneIDs(entriesOf(c)); len(g) != 0 {
		t.Fatalf("gone %v", g)
	}
	// A result hanging on an older call (calls made together) takes nothing back.
	d := NewClaude()
	d.Feed(lines(
		treeRow("user", "u1", "", "first"),
		treeRow("call", "a1", "u1", "t1"),
		treeRow("call", "a2", "a1", "t2"),
		treeRow("text", "a3", "a2", "between"),
		treeRow("result", "r1", "a1", "t1"),
	))
	if g := goneIDs(entriesOf(d)); len(g) != 0 {
		t.Fatalf("a result took back %v", g)
	}
}

// Back to the session's start: the message said again hangs on none, and
// all before it is taken back, in the part before too.
func TestClaudeRewindToStart(t *testing.T) {
	rows := []string{
		treeRow("user", "u1", "", "first"),
		treeRow("text", "a1", "u1", "answer"),
		treeRow("user", "u2", "", "first, said again"),
		treeRow("text", "a2", "u2", "the answer"),
	}
	c := NewClaude()
	c.Feed(lines(rows...))
	if g := strings.Join(goneIDs(entriesOf(c)), " "); g != "a1 u1" {
		t.Fatalf("gone %q", g)
	}
	for cut := 1; cut < len(rows); cut++ {
		before, after := NewClaude(), NewClaude()
		before.Feed(lines(rows[:cut]...))
		after.Feed(lines(rows[cut:]...))
		after.Prepend(before)
		if g := strings.Join(goneIDs(entriesOf(after)), " "); g != "a1 u1" {
			t.Fatalf("cut at %d: gone %q", cut, g)
		}
	}
}

// Going back to the same row again and again: each time what the last
// went on to is taken back, and the work does not grow with the tries.
func TestClaudeRewindAgainAndAgain(t *testing.T) {
	rows := []string{treeRow("user", "u0", "", "start"), treeRow("text", "a0", "u0", "ok")}
	for i := 1; i <= 3; i++ {
		rows = append(rows,
			treeRow("user", fmt.Sprintf("u%d", i), "a0", "try"),
			treeRow("text", fmt.Sprintf("a%d", i), fmt.Sprintf("u%d", i), "tried"),
		)
	}
	c := NewClaude()
	c.Feed(lines(rows...))
	if g := strings.Join(goneIDs(entriesOf(c)), " "); g != "a1 a2 u1 u2" {
		t.Fatalf("gone %q", g)
	}
}

// BenchmarkClaudeRewindRetries: a thousand tries from one row.
func BenchmarkClaudeRewindRetries(b *testing.B) {
	rows := []string{treeRow("user", "u0", "", "start"), treeRow("text", "a0", "u0", "ok")}
	for i := 1; i <= 1500; i++ {
		rows = append(rows, treeRow("user", fmt.Sprintf("u%d", i), "a0", "try"), treeRow("text", fmt.Sprintf("a%d", i), fmt.Sprintf("u%d", i), "tried"))
	}
	data := lines(rows...)
	for b.Loop() {
		NewClaude().Feed(data)
	}
}

// A result come late, hanging on a call taken back, is taken back too —
// also after going back to the same row again.
func TestClaudeRewindLateRow(t *testing.T) {
	c := NewClaude()
	c.Feed(lines(
		treeRow("user", "u0", "", "start"),
		treeRow("text", "a0", "u0", "ok"),
		treeRow("user", "u1", "a0", "try"),
		treeRow("call", "c1", "u1", "t1"),
		treeRow("user", "u2", "a0", "try again"),
		treeRow("result", "r1", "c1", "t1"), // (late: of the call taken back)
		treeRow("text", "x1", "r1", "said on it"),
		treeRow("user", "u3", "a0", "and again"),
	))
	if g := strings.Join(goneIDs(entriesOf(c)), " "); g != "c1 u1 u2 x1" {
		t.Fatalf("gone %q", g)
	}
	if tl := c.entries[3].Parts[0].Tool; !tl.Done {
		t.Fatalf("the late result did not reach its call: %+v", tl)
	}
}

// The rewind's row in the part before; in the part read first, the branch
// gone back from, the message going back, then a row come late on that
// branch: taken back once the part before is read, the late one too.
func TestClaudeRewindLateRowAcrossPrepend(t *testing.T) {
	rows := []string{
		treeRow("user", "u0", "", "start"),
		treeRow("text", "a0", "u0", "ok"),
		treeRow("user", "u1", "a0", "try"),
		treeRow("call", "c1", "u1", "t1"),
		treeRow("user", "u2", "a0", "try again"),
		treeRow("result", "r1", "c1", "t1"),
		treeRow("text", "x1", "r1", "said on it"),
	}
	before, after := NewClaude(), NewClaude()
	before.Feed(lines(rows[:2]...))
	after.Feed(lines(rows[2:]...))
	after.Prepend(before)
	if g := strings.Join(goneIDs(entriesOf(after)), " "); g != "c1 u1 x1" {
		t.Fatalf("gone %q", g)
	}
}

// A log that keeps no tree (no parentUuid at all, as older ones and hand
// written ones): nothing is taken back.
func TestClaudeNoTreeNothingTakenBack(t *testing.T) {
	c := NewClaude()
	c.Feed(lines(
		`{"type":"user","uuid":"u1","message":{"content":"one"}}`,
		`{"type":"assistant","uuid":"a1","message":{"content":[{"type":"text","text":"ok"}]}}`,
		`{"type":"user","uuid":"u2","message":{"content":"two"}}`,
		`{"type":"assistant","uuid":"a2","message":{"content":[{"type":"text","text":"ok"}]}}`,
	))
	if g := goneIDs(entriesOf(c)); len(g) != 0 {
		t.Fatalf("gone %v", g)
	}
}

// Many tries from one row in the part before (more than the rewinds kept
// to be told): all but the last taken back once it is read.
func TestClaudeRewindManyTriesAcrossPrepend(t *testing.T) {
	before := NewClaude()
	before.Feed(lines(treeRow("user", "u0", "", "start"), treeRow("text", "a0", "u0", "ok")))
	var rows []string
	n := maxTreeCuts + 5
	for i := 1; i <= n; i++ {
		rows = append(rows, treeRow("user", fmt.Sprintf("u%d", i), "a0", "try"), treeRow("text", fmt.Sprintf("a%d", i), fmt.Sprintf("u%d", i), "tried"))
	}
	after := NewClaude()
	after.Feed(lines(rows...))
	after.Prepend(before)
	for _, e := range after.entries {
		last := e.ID == fmt.Sprintf("u%d", n) || e.ID == fmt.Sprintf("a%d", n)
		if e.ID != "u0" && e.ID != "a0" && e.Gone == last {
			t.Fatalf("%s gone=%v", e.ID, e.Gone)
		}
	}
}
