package convo

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
)

// The rows of a real session (OpenCode 1.19.0: a bug fixed, then checked
// by running a command), as the gateway reads them.
func ocFixture(t *testing.T) [][]byte {
	b, err := os.ReadFile("testdata/opencode-fix.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	var out [][]byte
	for _, l := range bytes.Split(bytes.TrimSpace(b), []byte("\n")) {
		out = append(out, append(append([]byte(nil), l...), '\n'))
	}
	return out
}

func ocKinds(es []Entry) string {
	var s []string
	for _, e := range es {
		k := e.Role[:1] + ":"
		for _, p := range e.Parts {
			switch p.Kind {
			case KindTool:
				k += p.Tool.Name + ","
			default:
				k += p.Kind + ","
			}
		}
		s = append(s, k)
	}
	return strings.Join(s, " ")
}

func TestOpenCodeSession(t *testing.T) {
	c := NewOpenCode()
	c.Feed(bytes.Join(ocFixture(t), nil))
	es := c.Since(0)
	want := "u:text, a:thinking,Read, a:thinking,Edit, a:thinking,Bash, a:thinking,text,"
	if got := ocKinds(es); got != want {
		t.Fatalf("entries\n got %s\nwant %s", got, want)
	}
	if !strings.Contains(es[0].Parts[0].Text, "add 函数有 bug") || es[0].Time == "" {
		t.Errorf("the person's message: %+v", es[0])
	}
	read := es[1].Parts[1].Tool
	if read.Detail != "/home/ihainan/tmp/oc-probe/calc.py" || !read.Done || !strings.Contains(read.Result, "return a - b") {
		t.Errorf("read: %+v", read)
	}
	edit := es[2].Parts[1].Tool
	if edit.Detail != "/home/ihainan/tmp/oc-probe/calc.py" || edit.Add != 1 || edit.Del != 1 || len(edit.Patch) != 1 || edit.Patch[0].OldStart != 1 {
		t.Errorf("edit: %+v", edit)
	}
	bash := es[3].Parts[1].Tool
	if !strings.Contains(bash.Detail, "calc.add(2,3)") || bash.Result != "5\n" || bash.Error {
		t.Errorf("bash: %+v", bash)
	}
	if !strings.Contains(es[4].Parts[1].Text, "已修复") {
		t.Errorf("the answer: %+v", es[4].Parts)
	}
	if c.Status() != "idle" || c.Meta().Model != "qwen" || c.Meta().Context == 0 {
		t.Errorf("status %q, meta %+v", c.Status(), c.Meta())
	}
	if c.Title() != "" {
		t.Errorf("the title OpenCode gives before naming it: %q", c.Title())
	}
	// The same rows again change nothing.
	rev := c.Rev()
	c.Feed(bytes.Join(ocFixture(t), nil))
	if n := len(c.Since(rev)); n != 0 {
		t.Errorf("the same rows again changed %d entries", n)
	}
}

// Fed as they were written, a row at a time (a look after each): the same
// entries, each changed where its rows changed.
func TestOpenCodeLive(t *testing.T) {
	rows := ocFixture(t)
	c := NewOpenCode()
	for _, r := range rows {
		c.Feed(r)
	}
	all := NewOpenCode()
	all.Feed(bytes.Join(rows, nil))
	if a, b := ocKinds(c.Since(0)), ocKinds(all.Since(0)); a != b {
		t.Fatalf("a row at a time\n got %s\nwant %s", a, b)
	}
	// A tool call running, then done: the one entry, changed.
	c = NewOpenCode()
	c.Feed([]byte(`{"k":"m","id":"msg_1","c":10,"u":10,"d":"{\"role\":\"assistant\",\"modelID\":\"glm-5\",\"time\":{\"created\":10}}"}` + "\n"))
	c.Feed([]byte(`{"k":"p","id":"prt_1","m":"msg_1","c":11,"u":11,"d":"{\"type\":\"tool\",\"tool\":\"bash\",\"callID\":\"x\",\"state\":{\"status\":\"running\",\"input\":{\"command\":\"go test ./...\",\"description\":\"Run tests\"}}}"}` + "\n"))
	es := c.Since(0)
	if len(es) != 1 || es[0].Parts[0].Tool.Done || es[0].Parts[0].Tool.Title != "Run tests" || c.Status() != "busy" {
		t.Fatalf("running: %+v, %s", es, c.Status())
	}
	rev := c.Rev()
	c.Feed([]byte(`{"k":"p","id":"prt_1","m":"msg_1","c":11,"u":20,"d":"{\"type\":\"tool\",\"tool\":\"bash\",\"callID\":\"x\",\"state\":{\"status\":\"completed\",\"input\":{\"command\":\"go test ./...\"},\"output\":\"FAIL\\n\",\"metadata\":{\"exit\":1}}}"}` + "\n"))
	es = c.Since(rev)
	if len(es) != 1 || !es[0].Parts[0].Tool.Done || !es[0].Parts[0].Tool.Error || es[0].Seq != 1 {
		t.Fatalf("done: %+v", es)
	}
	c.Feed([]byte(`{"k":"m","id":"msg_1","c":10,"u":30,"d":"{\"role\":\"assistant\",\"modelID\":\"glm-5\",\"time\":{\"created\":10,\"completed\":30}}"}` + "\n"))
	if c.Status() != "idle" {
		t.Errorf("its answer done: %s", c.Status())
	}
}

// Older messages read on their own go before; a part of one read before
// its message is kept for it.
func TestOpenCodePrepend(t *testing.T) {
	rows := ocFixture(t)
	var old, recent [][]byte
	cutAt := int64(0)
	for _, r := range rows {
		var x OpenCodeRow
		_ = json.Unmarshal(r, &x)
		if x.K == "m" && strings.Contains(x.D, `"finish":"stop"`) {
			cutAt = x.C
		}
	}
	msgOf := map[string]int64{}
	for _, r := range rows {
		var x OpenCodeRow
		_ = json.Unmarshal(r, &x)
		if x.K == "m" {
			msgOf[x.ID] = x.C
		}
	}
	for _, r := range rows {
		var x OpenCodeRow
		_ = json.Unmarshal(r, &x)
		at := x.C
		if x.K == "p" {
			at = msgOf[x.M]
		}
		if x.K != "s" && at < cutAt {
			old = append(old, r)
		} else {
			recent = append(recent, r)
		}
	}
	c := NewOpenCode()
	c.Feed(bytes.Join(recent, nil))
	if got := ocKinds(c.Since(0)); got != "a:thinking,text," {
		t.Fatalf("the latest alone: %s", got)
	}
	o := NewOpenCode()
	o.Feed(bytes.Join(old, nil))
	c.Prepend(o)
	all := NewOpenCode()
	all.Feed(bytes.Join(rows, nil))
	es := c.Since(0)
	if a, b := ocKinds(es), ocKinds(all.Since(0)); a != b {
		t.Fatalf("put before\n got %s\nwant %s", a, b)
	}
	for i := 1; i < len(es); i++ {
		if es[i].Seq <= es[i-1].Seq {
			t.Fatalf("out of order: %d then %d", es[i-1].Seq, es[i].Seq)
		}
	}
	if c.Status() != "idle" || c.Meta().Model != "qwen" {
		t.Errorf("status %q, meta %+v", c.Status(), c.Meta())
	}

	// A part before its message.
	d := NewOpenCode()
	d.Feed([]byte(`{"k":"p","id":"prt_9","m":"msg_9","c":5,"u":5,"d":"{\"type\":\"text\",\"text\":\"hello\"}"}` + "\n"))
	if len(d.Since(0)) != 0 {
		t.Fatal("a part without its message shown")
	}
	d.Feed([]byte(`{"k":"m","id":"msg_9","c":4,"u":4,"d":"{\"role\":\"user\",\"time\":{\"created\":4}}"}` + "\n"))
	if es := d.Since(0); len(es) != 1 || es[0].Parts[0].Text != "hello" {
		t.Fatalf("its message come: %+v", es)
	}
}

func TestOpenCodeTrouble(t *testing.T) {
	c := NewOpenCode()
	c.Feed([]byte(`{"k":"m","id":"msg_1","c":10,"u":12,"d":"{\"role\":\"assistant\",\"time\":{\"created\":10},\"error\":{\"name\":\"APIError\",\"data\":{\"message\":\"Rate limit exceeded\",\"statusCode\":429}}}"}` + "\n" +
		`{"k":"m","id":"msg_2","c":20,"u":22,"d":"{\"role\":\"assistant\",\"time\":{\"created\":20},\"error\":{\"name\":\"MessageAbortedError\",\"data\":{\"message\":\"aborted\"}}}"}` + "\n" +
		`{"k":"p","id":"prt_2","m":"msg_2","c":21,"u":21,"d":"{\"type\":\"text\",\"text\":\"half\"}"}` + "\n"))
	es := c.Since(0)
	if len(es) != 2 || es[0].Parts[0].Trouble == nil || es[0].Parts[0].Trouble.Status != 429 || es[0].Parts[0].Trouble.Text != "Rate limit exceeded" {
		t.Fatalf("an error: %+v", es)
	}
	if len(es[1].Parts) != 1 || es[1].Parts[0].Kind != KindText {
		t.Errorf("stopped by the person is no trouble: %+v", es[1].Parts)
	}
	if c.Status() != "idle" {
		t.Errorf("ended in an error: %s", c.Status())
	}
	// A task list, the latest kept.
	c.Feed([]byte(`{"k":"m","id":"msg_3","c":30,"u":30,"d":"{\"role\":\"assistant\",\"time\":{\"created\":30}}"}` + "\n" +
		`{"k":"p","id":"prt_3","m":"msg_3","c":31,"u":31,"d":"{\"type\":\"tool\",\"tool\":\"todowrite\",\"state\":{\"status\":\"completed\",\"input\":{\"todos\":[{\"content\":\"读代码\",\"status\":\"completed\"},{\"content\":\"改\",\"status\":\"in_progress\"}]}}}"}` + "\n"))
	if ts := c.Tasks(); len(ts) != 2 || ts[1].Status != "in_progress" {
		t.Errorf("tasks: %+v", ts)
	}
}

// A row written over within the same millisecond is told by what it holds.
func TestOpenCodeSameMillisecond(t *testing.T) {
	c := NewOpenCode()
	c.Feed([]byte(`{"k":"m","id":"msg_1","c":10,"u":10,"d":"{\"role\":\"assistant\",\"time\":{\"created\":10}}"}` + "\n" +
		`{"k":"p","id":"prt_1","m":"msg_1","c":11,"u":20,"d":"{\"type\":\"text\",\"text\":\"before\"}"}` + "\n"))
	c.Feed([]byte(`{"k":"p","id":"prt_1","m":"msg_1","c":11,"u":20,"d":"{\"type\":\"text\",\"text\":\"after\"}"}` + "\n"))
	if es := c.Since(0); es[0].Parts[0].Text != "after" {
		t.Fatalf("written over in the same millisecond: %q", es[0].Parts[0].Text)
	}
	// One behind it (an older write, fed late) is not taken.
	c.Feed([]byte(`{"k":"p","id":"prt_1","m":"msg_1","c":11,"u":19,"d":"{\"type\":\"text\",\"text\":\"older\"}"}` + "\n"))
	if es := c.Since(0); es[0].Parts[0].Text != "after" {
		t.Fatalf("an older write taken: %q", es[0].Parts[0].Text)
	}
}

// A message before the floor (read back, not yet), written over, is not
// taken for a new one at the end; read back, it goes before.
func TestOpenCodeFloor(t *testing.T) {
	c := NewOpenCode()
	c.Feed([]byte(`{"k":"m","id":"msg_5","c":50,"u":50,"d":"{\"role\":\"user\",\"time\":{\"created\":50}}"}` + "\n" +
		`{"k":"p","id":"prt_5","m":"msg_5","c":50,"u":50,"d":"{\"type\":\"text\",\"text\":\"five\"}"}` + "\n"))
	c.SetFloor(50, "msg_5")
	c.Feed([]byte(`{"k":"m","id":"msg_1","c":10,"u":60,"d":"{\"role\":\"user\",\"time\":{\"created\":10}}"}` + "\n" +
		`{"k":"p","id":"prt_1","m":"msg_1","c":10,"u":60,"d":"{\"type\":\"text\",\"text\":\"one\"}"}` + "\n"))
	if es := c.Since(0); len(es) != 1 || es[0].Parts[0].Text != "five" {
		t.Fatalf("one before the floor taken as new: %+v", es)
	}
	if fc, fid, ok := c.Floor(); !ok || fc != 50 || fid != "msg_5" {
		t.Fatalf("the floor moved: %d %s", fc, fid)
	}
	o := NewOpenCode()
	o.Feed([]byte(`{"k":"m","id":"msg_1","c":10,"u":60,"d":"{\"role\":\"user\",\"time\":{\"created\":10}}"}` + "\n" +
		`{"k":"p","id":"prt_1","m":"msg_1","c":10,"u":60,"d":"{\"type\":\"text\",\"text\":\"one\"}"}` + "\n"))
	o.SetFloor(10, "msg_1")
	c.Prepend(o)
	es := c.Since(0)
	if len(es) != 2 || es[0].Parts[0].Text != "one" || es[1].Parts[0].Text != "five" {
		t.Fatalf("read back: %+v", es)
	}
	if fc, _, _ := c.Floor(); fc != 10 {
		t.Fatalf("the floor after reading back: %d", fc)
	}
}

// Rows of messages let go (too many kept) fed again do not come back.
func TestOpenCodeTrimmed(t *testing.T) {
	var b strings.Builder
	for i := 0; i < maxEntries+5; i++ {
		id := fmt.Sprintf("msg_%05d", i)
		fmt.Fprintf(&b, `{"k":"m","id":"%s","c":%d,"u":1,"d":"{\"role\":\"user\"}"}`+"\n", id, 100+i)
		fmt.Fprintf(&b, `{"k":"p","id":"prt_%05d","m":"%s","c":%d,"u":1,"d":"{\"type\":\"text\",\"text\":\"t%d\"}"}`+"\n", i, id, 100+i, i)
	}
	c := NewOpenCode()
	c.Feed([]byte(b.String()))
	first := c.Since(0)[0].ID
	c.Feed([]byte(b.String()))
	es := c.Since(0)
	if len(es) != maxEntries || es[0].ID != first || es[len(es)-1].ID != fmt.Sprintf("msg_%05d", maxEntries+4) {
		t.Fatalf("fed again: %d entries, first %s, last %s", len(es), es[0].ID, es[len(es)-1].ID)
	}
}

// Read back with no room left: nothing more, no fault.
func TestOpenCodePrependFull(t *testing.T) {
	var b strings.Builder
	for i := 0; i < maxEntries; i++ {
		fmt.Fprintf(&b, `{"k":"m","id":"msg_%05d","c":%d,"u":1,"d":"{\"role\":\"user\"}"}`+"\n", 1000+i, 1000+i)
		fmt.Fprintf(&b, `{"k":"p","id":"prt_%05d","m":"msg_%05d","c":1,"u":1,"d":"{\"type\":\"text\",\"text\":\"t\"}"}`+"\n", 1000+i, 1000+i)
	}
	c := NewOpenCode()
	c.Feed([]byte(b.String()))
	o := NewOpenCode()
	o.Feed([]byte(`{"k":"m","id":"msg_00001","c":1,"u":1,"d":"{\"role\":\"user\"}"}` + "\n" + `{"k":"p","id":"prt_00001","m":"msg_00001","c":1,"u":1,"d":"{\"type\":\"text\",\"text\":\"old\"}"}` + "\n"))
	c.Prepend(o)
	if n := len(c.Since(0)); n != maxEntries || !c.Trimmed() {
		t.Fatalf("%d entries, trimmed %v", n, c.Trimmed())
	}
	// (The floor stays where it was: the one let go, written over, is not new.)
	c.Feed([]byte(`{"k":"m","id":"msg_00001","c":1,"u":5,"d":"{\"role\":\"user\"}"}` + "\n" + `{"k":"p","id":"prt_00001","m":"msg_00001","c":1,"u":5,"d":"{\"type\":\"text\",\"text\":\"older\"}"}` + "\n"))
	es := c.Since(0)
	if es[len(es)-1].ID == "msg_00001" || es[0].ID != "msg_01000" {
		t.Fatalf("one let go came back: first %s, last %s", es[0].ID, es[len(es)-1].ID)
	}
}

// A message read back with nothing to show, whose text was read before it
// (a part of it written over meanwhile): shown, in its place.
func TestOpenCodePrependOrphan(t *testing.T) {
	c := NewOpenCode()
	c.Feed([]byte(`{"k":"m","id":"msg_00009","c":90,"u":90,"d":"{\"role\":\"user\"}"}` + "\n" + `{"k":"p","id":"prt_00009","m":"msg_00009","c":90,"u":90,"d":"{\"type\":\"text\",\"text\":\"nine\"}"}` + "\n" +
		`{"k":"p","id":"prt_00005","m":"msg_00005","c":50,"u":95,"d":"{\"type\":\"text\",\"text\":\"five\"}"}` + "\n"))
	o := NewOpenCode()
	o.Feed([]byte(`{"k":"m","id":"msg_00004","c":40,"u":40,"d":"{\"role\":\"user\"}"}` + "\n" + `{"k":"p","id":"prt_00004","m":"msg_00004","c":40,"u":40,"d":"{\"type\":\"text\",\"text\":\"four\"}"}` + "\n" +
		`{"k":"m","id":"msg_00005","c":50,"u":50,"d":"{\"role\":\"user\"}"}` + "\n"))
	c.Prepend(o)
	var got []string
	for _, e := range c.Since(0) {
		got = append(got, e.Parts[0].Text)
	}
	if strings.Join(got, ",") != "four,five,nine" {
		t.Fatalf("got %v", got)
	}
}
