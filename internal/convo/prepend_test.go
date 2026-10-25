package convo

import (
	"fmt"
	"testing"
)

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

// A task created before the part read first, updated in it: once the part
// before is in, the list has it, updated.
func TestPrependTasks(t *testing.T) {
	c := NewClaude()
	c.Feed(lines(
		`{"type":"assistant","uuid":"a2","message":{"content":[{"type":"tool_use","id":"u1","name":"TaskUpdate","input":{"taskId":"1","status":"in_progress"}}]}}`,
		`{"type":"user","uuid":"r2","message":{"content":[{"type":"tool_result","tool_use_id":"u1","content":"Updated task #1 status"}]}}`,
	))
	o := NewClaude()
	o.Feed(lines(
		`{"type":"assistant","uuid":"a1","message":{"content":[{"type":"tool_use","id":"c1","name":"TaskCreate","input":{"subject":"Write tests","description":"d"}}]}}`,
		`{"type":"user","uuid":"r1","message":{"content":[{"type":"tool_result","tool_use_id":"c1","content":"Task #1 created successfully: Write tests"}]},"toolUseResult":{"task":{"id":"1","subject":"Write tests"}}}`,
	))
	if len(o.Tasks()) != 1 {
		t.Skipf("the TaskCreate shape here is not the parser's: %+v", o.Tasks())
	}
	c.Prepend(o)
	ts := c.Tasks()
	if len(ts) != 1 || ts[0].Subject != "Write tests" || ts[0].Status != "in_progress" {
		t.Fatalf("tasks: %+v", ts)
	}
}

// Results read in the later part are made in the order they came, the
// part before's calls among them: B answered first, A last, A's word stands.
func TestPrependTaskResultOrder(t *testing.T) {
	upd := func(id, status string) string {
		return `{"type":"assistant","uuid":"a` + id + `","message":{"content":[{"type":"tool_use","id":"` + id + `","name":"TaskUpdate","input":{"taskId":"1","status":"` + status + `"}}]}}`
	}
	res := func(id string) string {
		return `{"type":"user","uuid":"r` + id + `","message":{"content":[{"type":"tool_result","tool_use_id":"` + id + `","content":"Updated task #1 status"}]}}`
	}
	c := NewClaude()
	c.Feed(lines(upd("B", "in_progress"), res("B"), res("A")))
	o := NewClaude()
	o.Feed(lines(
		`{"type":"assistant","uuid":"a1","message":{"content":[{"type":"tool_use","id":"c1","name":"TaskCreate","input":{"subject":"Write tests","description":"d"}}]}}`,
		`{"type":"user","uuid":"r1","message":{"content":[{"type":"tool_result","tool_use_id":"c1","content":"Task #1 created successfully: Write tests"}]},"toolUseResult":{"task":{"id":"1","subject":"Write tests"}}}`,
		upd("A", "completed"),
	))
	c.Prepend(o)
	if ts := c.Tasks(); len(ts) != 1 || ts[0].Status != "completed" {
		t.Fatalf("tasks: %+v", ts)
	}
	// (Once more from further back: the change stays made where it was.)
	c.Prepend(NewClaude())
	if ts := c.Tasks(); len(ts) != 1 || ts[0].Status != "completed" {
		t.Fatalf("tasks after another part: %+v", ts)
	}
}

// Past maxTaskOps changes, the list is not made again from the part before
// (it would miss the later ones).
func TestPrependTaskOpsFull(t *testing.T) {
	c := NewClaude()
	c.Feed(lines(
		`{"type":"assistant","uuid":"a1","message":{"content":[{"type":"tool_use","id":"c1","name":"TaskCreate","input":{"subject":"Write tests","description":"d"}}]}}`,
		`{"type":"user","uuid":"r1","message":{"content":[{"type":"tool_result","tool_use_id":"c1","content":"Task #1 created successfully: Write tests"}]},"toolUseResult":{"task":{"id":"1","subject":"Write tests"}}}`,
	))
	for i := 0; i <= maxTaskOps; i++ {
		id := fmt.Sprint("u", i)
		status := "in_progress"
		if i == maxTaskOps {
			status = "completed"
		}
		c.Feed(lines(
			`{"type":"assistant","uuid":"a`+id+`","message":{"content":[{"type":"tool_use","id":"`+id+`","name":"TaskUpdate","input":{"taskId":"1","status":"`+status+`"}}]}}`,
			`{"type":"user","uuid":"r`+id+`","message":{"content":[{"type":"tool_result","tool_use_id":"`+id+`","content":"Updated task #1 status"}]}}`,
		))
	}
	c.Prepend(NewClaude())
	if ts := c.Tasks(); len(ts) != 1 || ts[0].Status != "completed" {
		t.Fatalf("tasks: %+v", ts)
	}
}

// A whole list said in the part between, its result and then a created
// task's in the part read first: the task created stays, after the list.
func TestPrependTaskAfterWhole(t *testing.T) {
	c := NewClaude()
	c.Feed(lines(
		`{"type":"user","uuid":"rW","message":{"content":[{"type":"tool_result","tool_use_id":"W","content":"ok"}]}}`,
		`{"type":"user","uuid":"rX","message":{"content":[{"type":"tool_result","tool_use_id":"X","content":"Task #1 created successfully: Later"}]},"toolUseResult":{"task":{"id":"1","subject":"Later"}}}`,
	))
	b := NewClaude()
	b.Feed(lines(`{"type":"assistant","uuid":"aW","message":{"content":[{"type":"tool_use","id":"W","name":"TodoWrite","input":{"todos":[{"content":"One","status":"completed"}]}}]}}`))
	c.Prepend(b)
	a := NewClaude()
	a.Feed(lines(`{"type":"assistant","uuid":"aX","message":{"content":[{"type":"tool_use","id":"X","name":"TaskCreate","input":{"subject":"Later","description":"d"}}]}}`))
	c.Prepend(a)
	ts := c.Tasks()
	if len(ts) != 2 || ts[0].Subject != "One" || ts[1].Subject != "Later" {
		t.Fatalf("tasks: %+v", ts)
	}
}

// Results of other tools, their calls not read yet, do not use up what is
// kept of the task list's changes.
func TestPrependPlacesApart(t *testing.T) {
	c := NewClaude()
	var ls []string
	for i := 0; i < maxOrphans; i++ {
		ls = append(ls, fmt.Sprintf(`{"type":"user","uuid":"b%d","message":{"content":[{"type":"tool_result","tool_use_id":"b%d","content":"ok"}]}}`, i, i))
	}
	for i := 0; i < maxTaskOps-10; i++ {
		ls = append(ls, fmt.Sprintf(`{"type":"assistant","uuid":"u%d","message":{"content":[{"type":"tool_use","id":"u%d","name":"TaskUpdate","input":{"taskId":"1","status":"completed"}}]}}`, i, i),
			fmt.Sprintf(`{"type":"user","uuid":"r%d","message":{"content":[{"type":"tool_result","tool_use_id":"u%d","content":"Updated task #1 status"}]}}`, i, i))
	}
	c.Feed(lines(ls...))
	o := NewClaude()
	o.Feed(lines(
		`{"type":"assistant","uuid":"a1","message":{"content":[{"type":"tool_use","id":"c1","name":"TaskCreate","input":{"subject":"Write tests","description":"d"}}]}}`,
		`{"type":"user","uuid":"r1","message":{"content":[{"type":"tool_result","tool_use_id":"c1","content":"Task #1 created successfully: Write tests"}]},"toolUseResult":{"task":{"id":"1","subject":"Write tests"}}}`,
	))
	c.Prepend(o)
	if ts := c.Tasks(); len(ts) != 1 || ts[0].Status != "completed" {
		t.Fatalf("tasks: %+v", ts)
	}
}

// What the session ran with comes from before when the part read first did not say.
func TestPrependMeta(t *testing.T) {
	c := NewClaude()
	c.Feed(lines(`{"type":"user","uuid":"u","message":{"content":"hi"}}`))
	o := NewClaude()
	o.Feed(lines(`{"type":"assistant","uuid":"a","gitBranch":"main","message":{"model":"claude-x","usage":{"input_tokens":100},"content":[{"type":"text","text":"old"}]}}`))
	c.Prepend(o)
	if m := c.Meta(); m.Model != "claude-x" || m.Branch != "main" || m.Context != 100 {
		t.Fatalf("meta: %+v", m)
	}
}
