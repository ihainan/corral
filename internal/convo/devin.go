package convo

import (
	"bytes"
	"encoding/json"
	"regexp"
	"strings"
)

// Devin reads Devin CLI's conversation, as the gateway's query of its
// database (…/devin/cli/sessions.db: message_nodes, a forest of messages,
// the session's own line through it from sessions.main_chain_id back) prints
// it, one JSON object a line (see web's dvQueryScript):
//
//   - "s": the session: its title, model, permission mode.
//   - "u": what the person wrote (a user message marked is_user_input).
//   - "a": an answer of the model's: its words, its thinking, the tools it
//     called (exec, read, edit, write, grep, …), why it stopped.
//   - "r": what a tool gave back, by the call's id: its text, whether it
//     went well, a command's exit code, the files an edit changed.
//   - "sum": the summary written when the conversation was compacted.
//
// What comes from the host is untrusted: every field optional, lengths
// bounded here again, nothing taken as markup.
type Devin struct {
	entries []*Entry
	seq     int
	rev     int
	title   string
	named   bool // (the session's own title said: not taken from a message)
	meta    Meta
	tasks   []Task
	trimmed bool
	Skipped int

	// calls: the tool calls whose result has not been read, by the call's id.
	calls map[string]dvCall
	// orphans: results read before their call (read back from the end: the
	// call is in what is read before), paired in Prepend.
	orphans []dvRow
	// last: the kind of the last row read ("u", "a", "r"), and of an
	// answer, why it stopped: whether it works on.
	last, lastFin string
	known         bool
	floor         int64 // the first node read: what is before it is read back
}

type dvCall struct {
	e *Entry
	t *Tool
}

type dvRow struct {
	K     string       `json:"k"`
	N     int64        `json:"n"`
	ID    string       `json:"id"`
	T     string       `json:"t"`
	C     string       `json:"c"`
	Th    string       `json:"th"`
	TC    []dvToolCall `json:"tc"`
	Fin   string       `json:"fin"`
	Model string       `json:"model"`
	Ctx   int64        `json:"ctx"`
	Out   int64        `json:"out"`
	Ms    int64        `json:"ms"`
	Call  string       `json:"call"`
	Lines int          `json:"lines"`
	OK    *bool        `json:"ok"`
	Exit  *int         `json:"exit"`
	Ops   []dvFileOp   `json:"ops"`
	Fail  string       `json:"fail"`
	Title string       `json:"title"`
	Mode  string       `json:"mode"`
	Bytes int64        `json:"bytes"`
	Role  string       `json:"role"`
}

type dvToolCall struct {
	ID   string         `json:"id"`
	Name string         `json:"name"`
	A    map[string]any `json:"a"`
}

type dvFileOp struct {
	Op   string `json:"op"`
	Path string `json:"path"`
	Add  int    `json:"add"`
	Del  int    `json:"del"`
	Old  string `json:"old"`
	New  string `json:"new"`
}

const (
	maxDvCalls   = 4096
	maxDvOrphans = 64
)

func NewDevin() *Devin { return &Devin{} }

// dvRefRe: how Devin cites a file, or lines of one, in what it says
// (<ref_file file="/w/a.go" />, <ref_snippet file="/w/a.go" lines="3-9" />):
// its own terminal shows them as links; here they would be raw markup.
var dvRefRe = regexp.MustCompile(`<ref_(?:file|snippet)\s+file="([^"<>\n]{1,1000})"(?:\s+lines="([0-9]{1,9}(?:-[0-9]{1,9})?)")?\s*/?>`)

// dvRefs puts each citation as the file it names, in code: with its lines,
// when it gives them (a.go:3-9).
func dvRefs(s string) string {
	if !strings.Contains(s, "<ref_") {
		return s
	}
	return dvRefRe.ReplaceAllStringFunc(s, func(m string) string {
		g := dvRefRe.FindStringSubmatch(m)
		at := g[1]
		if g[2] != "" {
			at += ":" + g[2]
		}
		if strings.Contains(at, "`") {
			return m
		}
		return "`" + at + "`"
	})
}

func (c *Devin) Rev() int                 { return c.rev }
func (c *Devin) Title() string            { return c.title }
func (c *Devin) Meta() Meta               { return c.meta }
func (c *Devin) Trimmed() bool            { return c.trimmed }
func (c *Devin) Tasks() []Task            { return c.tasks }
func (c *Devin) Queued() []Queued         { return nil }
func (c *Devin) Running() []Running       { return nil }
func (c *Devin) QueueSince(started int64) {}
func (c *Devin) Since(rev int) []Entry    { return since(c.entries, rev) }

func (c *Devin) First() int {
	if len(c.entries) == 0 {
		return 0
	}
	return c.entries[0].Seq
}

// SetFloor: the first node read, where what is before it is read from.
func (c *Devin) SetFloor(n int64) {
	if n > 0 && (c.floor == 0 || n < c.floor) {
		c.floor = n
	}
}

// Floor is the first node read (0: none).
func (c *Devin) Floor() int64 { return c.floor }

// Status is "busy" while a turn goes on (the last row read the person's
// message, a tool's result, or an answer that called tools), "idle" after
// an answer that ended the turn, "" before any row was read.
func (c *Devin) Status() string {
	switch {
	case !c.known:
		return ""
	case c.last == "u" || c.last == "r" || (c.last == "a" && c.lastFin == "tool_calls"):
		return "busy"
	}
	return "idle"
}

func (c *Devin) Feed(lines []byte) {
	c.rev++
	for len(lines) > 0 {
		i := bytes.IndexByte(lines, '\n')
		if i < 0 {
			break
		}
		c.line(lines[:i])
		lines = lines[i+1:]
	}
	c.trim()
}

func (c *Devin) line(raw []byte) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return
	}
	var r dvRow
	if json.Unmarshal(raw, &r) != nil || r.K == "" {
		c.Skipped++
		return
	}
	switch r.K {
	case "s":
		if t := clip(firstLineOf(strings.TrimSpace(r.Title)), maxTitle); t != "" {
			c.title, c.named = t, true
		}
		if c.meta.Model == "" && r.Model != "" {
			c.meta.Model = clip(r.Model, maxMeta)
		}
		if r.Mode != "" {
			c.meta.Mode = clip(r.Mode, maxMeta)
		}
	case "u":
		c.user(r)
	case "a":
		c.answer(r)
	case "r":
		c.result(r)
	case "sum":
		c.summary(r)
	case "big":
		c.big(r)
	default:
		c.Skipped++
	}
}

func (c *Devin) add(e *Entry) {
	c.seq++
	e.Seq, e.Rev = c.seq, c.rev
	c.entries = append(c.entries, e)
}

func (c *Devin) trim() {
	if len(c.entries) <= maxEntries {
		return
	}
	c.entries = c.entries[len(c.entries)-maxEntries:]
	c.trimmed = true
}

func (c *Devin) mark(kind, fin string) {
	c.last, c.lastFin, c.known = kind, fin, true
}

func (c *Devin) big(r dvRow) {
	switch r.Role {
	case "user":
		c.mark("u", "")
	case "assistant":
		c.mark("a", "")
	case "tool":
		c.mark("r", "")
	}
	c.add(&Entry{Role: RoleNote, Parts: []Part{{Kind: KindTrouble, Trouble: &Trouble{Kind: TroubleBig, Bytes: r.Bytes}}}})
}

func (c *Devin) user(r dvRow) {
	c.mark("u", "")
	text := strings.TrimSpace(r.C)
	if text == "" {
		return
	}
	if !c.named && c.title == "" {
		c.title = clip(firstLineOf(text), maxTitle)
	}
	c.add(&Entry{ID: clip(r.ID, maxID), Role: RoleUser, Time: clip(r.T, 64), Parts: []Part{textPart(KindText, text)}})
}

func (c *Devin) answer(r dvRow) {
	c.mark("a", r.Fin)
	if r.Model != "" {
		c.meta.Model = clip(r.Model, maxMeta)
	}
	if r.Ctx > 0 {
		c.meta.Context = r.Ctx
	}
	if r.Out > 0 {
		c.meta.Answer = &Answer{Out: r.Out, Ms: max(r.Ms, 0)}
	}
	var parts []Part
	if th := strings.TrimSpace(r.Th); th != "" {
		parts = append(parts, textPart(KindThinking, th))
	}
	if tx := strings.TrimSpace(r.C); tx != "" {
		parts = append(parts, textPart(KindText, dvRefs(tx)))
	}
	e := &Entry{ID: clip(r.ID, maxID), Role: RoleAssistant, Time: clip(r.T, 64)}
	var made []*Tool
	for _, tc := range r.TC {
		if len(parts) >= maxParts {
			break
		}
		t := dvTool(tc)
		parts = append(parts, Part{Kind: KindTool, Tool: t})
		made = append(made, t)
		if tc.Name == "todo_write" {
			c.todos(tc.A)
		}
	}
	if r.Fin == "max_tokens" && len(parts) < maxParts {
		parts = append(parts, Part{Kind: KindTrouble, Trouble: &Trouble{Kind: TroubleCut}})
	}
	if len(parts) == 0 {
		return
	}
	e.Parts = parts
	c.add(e)
	for _, t := range made {
		if t.ID == "" {
			continue
		}
		if c.calls == nil {
			c.calls = map[string]dvCall{}
		}
		if len(c.calls) >= maxDvCalls {
			for k := range c.calls {
				delete(c.calls, k)
				break
			}
		}
		c.calls[t.ID] = dvCall{e, t}
	}
}

func (c *Devin) result(r dvRow) {
	c.mark("r", "")
	id := clip(r.Call, maxID)
	call, ok := c.calls[id]
	if !ok {
		if len(c.orphans) < maxDvOrphans {
			c.orphans = append(c.orphans, r)
		}
		return
	}
	delete(c.calls, id)
	dvApply(call.t, r)
	call.e.Rev = c.rev
}

func (c *Devin) summary(r dvRow) {
	c.mark("sum", "")
	c.add(&Entry{ID: clip(r.ID, maxID) + "-ev", Role: RoleNote, Time: clip(r.T, 64), Parts: []Part{{Kind: KindEvent, Event: &Event{Kind: EventCompact}}}})
	if t := strings.TrimSpace(r.C); t != "" {
		c.add(&Entry{ID: clip(r.ID, maxID), Role: RoleSummary, Time: clip(r.T, 64), Parts: []Part{textPart(KindText, t)}})
	}
}

// todos: the task list as todo_write last set it (the whole of it each time).
func (c *Devin) todos(a map[string]any) {
	list, ok := a["todos"].([]any)
	if !ok {
		return
	}
	var tasks []Task
	for _, x := range list {
		m, ok := x.(map[string]any)
		if !ok || len(tasks) >= maxTasks {
			continue
		}
		subj, _ := m["content"].(string)
		st, _ := m["status"].(string)
		switch st {
		case "pending", "in_progress", "completed":
		default:
			st = "pending"
		}
		if subj = clip(firstLineOf(subj), maxTitle); subj != "" {
			tasks = append(tasks, Task{Subject: subj, Status: st})
		}
	}
	c.tasks = tasks
}

// dvTools: Devin CLI's tools by the names the view knows them by.
var dvTools = map[string]string{
	"exec":              "Bash",
	"read":              "Read",
	"edit":              "Edit",
	"write":             "Write",
	"grep":              "Grep",
	"find_file_by_name": "Glob",
	"web_search":        "WebSearch",
	"todo_write":        "TodoWrite",
	"get_output":        "BashOutput",
	"kill_shell":        "KillShell",
}

func dvTool(tc dvToolCall) *Tool {
	name := dvTools[tc.Name]
	if name == "" {
		name = clip(tc.Name, 128)
	}
	t := &Tool{ID: clip(tc.ID, maxID), Name: name}
	a := tc.A
	str := func(keys ...string) string {
		for _, k := range keys {
			if v, ok := a[k].(string); ok && v != "" {
				return v
			}
		}
		return ""
	}
	d := func(s string) string { s, _ = cut(clean(s), maxDetail); return s }
	switch name {
	case "Bash":
		t.Detail = d(str("command"))
		if w := str("workdir"); w != "" {
			t.Title = d(w)
		}
	case "Read":
		t.Detail = d(str("file_path", "path"))
	case "Write":
		t.Detail = d(str("file_path", "path"))
		if s := str("content"); s != "" {
			t.New, t.Add = d(s), lineCount(s)
		}
	case "Edit":
		t.Detail = d(str("file_path", "path"))
		o, n := str("old_string"), str("new_string")
		t.Old, t.New = d(o), d(n)
		if o == n {
			t.Same = o != ""
		} else {
			t.Add, t.Del = replaced(o, n)
		}
	case "Grep", "Glob":
		q := str("pattern")
		if p := str("path"); p != "" {
			q += "  " + p
		}
		t.Detail = d(q)
	case "WebSearch":
		t.Detail = d(str("query"))
	case "BashOutput", "KillShell":
		t.Detail = d(str("shell_id"))
	case "TodoWrite":
	default:
		t.Detail = d(str("command", "path", "file_path", "query", "pattern", "url", "description", "prompt", "scope"))
	}
	return t
}

// dvApply fills in a call with what it gave back.
func dvApply(t *Tool, r dvRow) {
	t.Done = true
	failed := (r.OK != nil && !*r.OK) || (r.Exit != nil && *r.Exit != 0)
	t.Error = failed
	text := r.C
	if f := strings.TrimSpace(r.Fail); f != "" && failed && !strings.Contains(text, f) {
		text = strings.TrimSpace(f + "\n" + text)
	}
	text = clean(text)
	t.setResult(text)
	t.ResultLines = lineCount(text)
	if r.Lines > t.ResultLines {
		t.ResultLines = r.Lines
	}
	// (The files an edit or a write changed, as Devin reported them: its
	// own count of lines in place of one from what was asked.)
	add, del, any := 0, 0, false
	for _, op := range r.Ops {
		if op.Add < 0 || op.Del < 0 {
			continue
		}
		add, del, any = add+op.Add, del+op.Del, true
	}
	if any && (t.Name == "Edit" || t.Name == "Write") {
		t.Add, t.Del = add, del
		t.Same = add == 0 && del == 0 && t.Same
	}
}

// Prepend puts o, a part read on its own from before what c read, before
// c's entries; the results c read first, whose calls are in o, are paired
// with them.
func (c *Devin) Prepend(other Parser) {
	o, ok := other.(*Devin)
	if !ok {
		return
	}
	var left []dvRow
	for _, r := range c.orphans {
		id := clip(r.Call, maxID)
		if call, ok := o.calls[id]; ok {
			delete(o.calls, id)
			dvApply(call.t, r)
		} else {
			left = append(left, r)
		}
	}
	c.orphans = append(append([]dvRow(nil), o.orphans...), left...)
	if len(c.orphans) > maxDvOrphans {
		c.orphans = c.orphans[:maxDvOrphans]
	}
	if c.title == "" {
		c.title, c.named = o.title, o.named
	}
	if c.meta.Model == "" {
		c.meta.Model = o.meta.Model
	}
	if c.meta.Mode == "" {
		c.meta.Mode = o.meta.Mode
	}
	if c.meta.Context == 0 {
		c.meta.Context = o.meta.Context
	}
	if c.meta.Answer == nil {
		c.meta.Answer = o.meta.Answer
	}
	if c.tasks == nil {
		c.tasks = o.tasks
	}
	if !c.known && o.known {
		c.last, c.lastFin, c.known = o.last, o.lastFin, true
	}
	c.SetFloor(o.floor)
	if len(o.entries) == 0 {
		return
	}
	room := maxEntries - len(c.entries)
	if room <= 0 {
		c.trimmed = true
		return
	}
	take := o.entries
	if len(take) > room {
		take = take[len(take)-room:]
		c.trimmed = true
	}
	c.rev++
	base := c.First()
	if base == 0 {
		base = len(take) + 1
	}
	for i, e := range take {
		e.Seq = base - len(take) + i
		e.Rev = c.rev
	}
	c.entries = append(append(make([]*Entry, 0, len(take)+len(c.entries)), take...), c.entries...)
}
