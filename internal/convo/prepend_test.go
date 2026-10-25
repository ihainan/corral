package convo

import "testing"

// A part of the log read later from before goes before what was read: its
// entries first, with lower Seqs, as changed; a call there answered in the
// part read first gets its result.
func TestPrepend(t *testing.T) {
	c := NewClaude()
	c.Feed(lines(
		`{"type":"user","uuid":"r1","message":{"content":[{"type":"tool_result","tool_use_id":"t1","content":"a\nb"}]}}`,
		`{"type":"assistant","uuid":"a2","message":{"content":[{"type":"text","text":"done"}]}}`,
	))
	rev := c.Rev()
	o := NewClaude()
	o.Feed(lines(
		`{"type":"user","uuid":"u0","message":{"content":"list it"}}`,
		`{"type":"assistant","uuid":"a0","message":{"content":[{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"ls"}}]}}`,
		`{"type":"assistant","uuid":"a1","message":{"content":[{"type":"tool_use","id":"t9","name":"Read","input":{"file_path":"/x"}}]}}`,
	))
	c.Prepend(o)
	all := c.Since(0)
	if len(all) != 4 || all[0].ID != "u0" || all[3].ID != "a2" {
		t.Fatalf("order: %+v", all)
	}
	for i := 1; i < len(all); i++ {
		if all[i].Seq <= all[i-1].Seq {
			t.Fatalf("seqs not rising: %d then %d", all[i-1].Seq, all[i].Seq)
		}
	}
	got := c.Since(rev)
	if len(got) != 3 || got[0].ID != "u0" {
		t.Fatalf("not told as changed: %+v", got)
	}
	bash := all[1].Parts[0].Tool
	if bash == nil || !bash.Done || bash.Result != "a\nb" {
		t.Fatalf("answered call: %+v", bash)
	}
	read := all[2].Parts[0].Tool
	if read == nil || read.Done {
		t.Fatalf("call not answered yet: %+v", read)
	}
	// Its answer comes as the log goes on: told as a change.
	rev = c.Rev()
	c.Feed(lines(`{"type":"user","uuid":"r9","message":{"content":[{"type":"tool_result","tool_use_id":"t9","content":"x"}]}}`))
	if got := c.Since(rev); len(got) != 1 || got[0].ID != "a1" || !got[0].Parts[0].Tool.Done || got[0].Parts[0].Tool.Result != "x" {
		t.Fatalf("answered later: %+v", got)
	}
	if c.Trimmed() {
		t.Fatal("trimmed with room to spare")
	}
}

// Past maxEntries, the oldest of a part put before are left out, and no
// more goes before.
func TestPrependFull(t *testing.T) {
	c := NewClaude()
	var b []string
	for i := 0; i < maxEntries-1; i++ {
		b = append(b, `{"type":"assistant","uuid":"n","message":{"content":[{"type":"text","text":"x"}]}}`)
	}
	c.Feed(lines(b...))
	o := NewClaude()
	o.Feed(lines(
		`{"type":"user","uuid":"old","message":{"content":"first"}}`,
		`{"type":"user","uuid":"kept","message":{"content":"second"}}`,
	))
	c.Prepend(o)
	all := c.Since(0)
	if len(all) != maxEntries || all[0].ID != "kept" || !c.Trimmed() {
		t.Fatalf("got %d, first %q, trimmed %v", len(all), all[0].ID, c.Trimmed())
	}
}

// A call in one part, its result two parts later: kept across the part between.
func TestPrependAcrossParts(t *testing.T) {
	c := NewClaude()
	c.Feed(lines(`{"type":"assistant","uuid":"z","message":{"content":[{"type":"text","text":"end"}]}}`))
	b := NewClaude()
	b.Feed(lines(`{"type":"user","uuid":"r1","message":{"content":[{"type":"tool_result","tool_use_id":"t1","content":"out"}]}}`))
	c.Prepend(b)
	a := NewClaude()
	a.Feed(lines(`{"type":"assistant","uuid":"a0","message":{"content":[{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"ls"}}]}}`))
	c.Prepend(a)
	all := c.Since(0)
	if tl := all[0].Parts[0].Tool; tl == nil || !tl.Done || tl.Result != "out" {
		t.Fatalf("across parts: %+v", tl)
	}
}

// A custom title cleared later stays cleared; one the later part never set comes from before.
func TestPrependTitles(t *testing.T) {
	c := NewClaude()
	c.Feed(lines(`{"type":"custom-title","customTitle":""}`))
	o := NewClaude()
	o.Feed(lines(`{"type":"custom-title","customTitle":"old name"}`, `{"type":"ai-title","aiTitle":"ai name"}`))
	c.Prepend(o)
	if c.Title() != "ai name" {
		t.Fatalf("title %q, want the cleared custom one to stay cleared", c.Title())
	}
}
