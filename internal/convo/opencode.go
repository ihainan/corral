package convo

import (
	"bytes"
	"encoding/json"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// OpenCode reads an OpenCode session into entries as Claude does Claude
// Code's log. OpenCode keeps no log: its sessions are rows of a SQLite
// database (~/.local/share/opencode/opencode.db), a "message" row for each
// message (its role, model, tokens, how it ended) and a "part" row for each
// piece of one (text, reasoning, a tool call and its result), rows that are
// written over as they go on (a tool call pending, running, completed). The
// gateway reads the rows changed since its last look and feeds them here,
// one JSON line each (see OpenCodeRow); a row fed again, changed, takes the
// place of what it was.
type OpenCode struct {
	entries []*Entry
	seq     int
	rev     int
	msgs    map[string]*ocMsg
	// orphans: parts of messages not read (yet): a part read before its
	// message, kept until it comes (bounded).
	orphans map[string]map[string]ocPart
	title   string
	meta    Meta
	tasks   []Task
	tasksAt int64 // (when the task list kept was written)
	// floor: the first message read back to (created, id): one before it,
	// fed as it is written over (an old one's summary, say), is not taken
	// for new; reading back goes on from it. (hasFloor: there is one.)
	floorC   int64
	floorID  string
	hasFloor bool
	working  bool
	known    bool
	trimmed  bool
	Skipped  int
	// What /undo took back (see gone): the message it went back to
	// (revertAt, "" for none) and whether it went back to a part of that
	// message (revertPart). OpenCode hides what came after; when the next
	// message is sent it deletes it (that message too, without a part).
	revertAt   string
	revertPart bool
}

// OpenCodeRow is one line fed: K "m" a message, "p" a part (of message M),
// "s" the session (its title); C and U when it was created and last
// written (unix milliseconds); D the row's own JSON, as it is stored.
type OpenCodeRow struct {
	K     string `json:"k"`
	ID    string `json:"id,omitempty"`
	M     string `json:"m,omitempty"`
	C     int64  `json:"c,omitempty"`
	U     int64  `json:"u,omitempty"`
	D     string `json:"d,omitempty"`
	Title string `json:"title,omitempty"`
	// "s": what /undo went back to (Revert: a message's id; RevertPart: to
	// a part of it), "" for none.
	Revert     string `json:"rv,omitempty"`
	RevertPart bool   `json:"rvp,omitempty"`
	// "ids": the messages there are now, of those made before U (the
	// last look's cursor) from the first read (sent when fewer are there
	// than were read: some were deleted).
	IDs []string `json:"ids,omitempty"`
}

type ocMsg struct {
	id      string
	created int64
	updated int64
	raw     string // (the row as last fed: one written over in the same millisecond is told by it)
	deleted bool   // (no longer in the database: taken back by /undo, then deleted)
	data    ocMessage
	parts   map[string]ocPart
	e       *Entry // (nil until it has something to show)
}

type ocPart struct {
	updated int64
	raw     string
	data    ocPartData
}

type ocMessage struct {
	Role       string `json:"role"`
	ModelID    string `json:"modelID"`
	ProviderID string `json:"providerID"`
	Model      *struct {
		ModelID string `json:"modelID"`
	} `json:"model"`
	Finish string `json:"finish"`
	Time   struct {
		Created   int64 `json:"created"`
		Completed int64 `json:"completed"`
	} `json:"time"`
	Tokens *struct {
		Input  int64 `json:"input"`
		Output int64 `json:"output"`
		Cache  struct {
			Read  int64 `json:"read"`
			Write int64 `json:"write"`
		} `json:"cache"`
	} `json:"tokens"`
	Error *struct {
		Name string `json:"name"`
		Data struct {
			Message    string `json:"message"`
			StatusCode int    `json:"statusCode"`
		} `json:"data"`
	} `json:"error"`
}

type ocPartData struct {
	Type      string       `json:"type"`
	Text      string       `json:"text"`
	Synthetic bool         `json:"synthetic"`
	Ignored   bool         `json:"ignored"`
	Mime      string       `json:"mime"`
	Filename  string       `json:"filename"`
	Tool      string       `json:"tool"`
	CallID    string       `json:"callID"`
	State     *ocToolState `json:"state"`
}

type ocToolState struct {
	Status   string          `json:"status"`
	Input    json.RawMessage `json:"input"`
	Output   string          `json:"output"`
	Error    string          `json:"error"`
	Title    string          `json:"title"`
	Metadata *struct {
		Exit     *int   `json:"exit"`
		Diff     string `json:"diff"`
		FileDiff *struct {
			Patch     string `json:"patch"`
			Additions int    `json:"additions"`
			Deletions int    `json:"deletions"`
		} `json:"filediff"`
	} `json:"metadata"`
}

// Bounds of what is held of rows not shown.
const (
	maxOcOrphanMsgs  = 64
	maxOcOrphanParts = 256
)

func NewOpenCode() *OpenCode {
	return &OpenCode{msgs: map[string]*ocMsg{}, orphans: map[string]map[string]ocPart{}}
}

func (c *OpenCode) Rev() int           { return c.rev }
func (c *OpenCode) Title() string      { return c.title }
func (c *OpenCode) Meta() Meta         { return c.meta }
func (c *OpenCode) Trimmed() bool      { return c.trimmed }
func (c *OpenCode) Tasks() []Task      { return c.tasks }
func (c *OpenCode) Queued() []Queued   { return nil }
func (c *OpenCode) Running() []Running { return nil }
func (c *OpenCode) QueueSince(int64)   {}

// Status is "busy" while its last answer goes on, "idle" after it, "" when
// nothing has said.
func (c *OpenCode) Status() string {
	switch {
	case !c.known:
		return ""
	case c.working:
		return "busy"
	}
	return "idle"
}

func (c *OpenCode) First() int {
	if len(c.entries) == 0 {
		return 0
	}
	return c.entries[0].Seq
}

func (c *OpenCode) Since(rev int) []Entry { return since(c.entries, rev) }

// SetFloor says the first message read (created, id): what is before it
// is read in as older (Prepend), not fed.
func (c *OpenCode) SetFloor(created int64, id string) {
	if !c.hasFloor || ocBefore(created, id, c.floorC, c.floorID) {
		c.floorC, c.floorID, c.hasFloor = created, id, true
	}
}

// Floor is the first message read: what is before it is read back.
func (c *OpenCode) Floor() (created int64, id string, ok bool) {
	return c.floorC, c.floorID, c.hasFloor
}

func ocBefore(c1 int64, id1 string, c2 int64, id2 string) bool {
	return c1 < c2 || (c1 == c2 && id1 < id2)
}

func (c *OpenCode) Feed(lines []byte) {
	c.rev++
	var rows []OpenCodeRow
	for len(lines) > 0 {
		i := bytes.IndexByte(lines, '\n')
		if i < 0 {
			break
		}
		var r OpenCodeRow
		if json.Unmarshal(lines[:i], &r) == nil {
			rows = append(rows, r)
		} else {
			c.Skipped++
		}
		lines = lines[i+1:]
	}
	c.take(rows)
	c.trim()
}

// take: messages first, oldest first (their entries in that order), then
// their parts.
func (c *OpenCode) take(rows []OpenCodeRow) {
	sort.SliceStable(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		if (a.K == "m") != (b.K == "m") {
			return a.K == "m"
		}
		if a.C != b.C {
			return a.C < b.C
		}
		return a.ID < b.ID
	})
	touched := map[*ocMsg]bool{}
	for _, r := range rows {
		switch r.K {
		case "ids":
			c.present(r.IDs, r.U)
		case "s":
			c.title = ocTitle(r.Title)
			c.revertAt, c.revertPart = clip(r.Revert, maxID), r.RevertPart
		case "m":
			if m := c.message(r); m != nil {
				touched[m] = true
			}
		case "p":
			if m := c.part(r); m != nil {
				touched[m] = true
			}
		}
	}
	for _, m := range c.inOrder(touched) {
		c.show(m)
	}
	c.goneNow(c.entries)
}

// present: of the messages read that were made at or before cursor (unix
// ms), these are the ones in the database still; the rest were deleted
// (what /undo took back, once the next message was sent). One made since
// is not of them: it may be read in the same look.
func (c *OpenCode) present(ids []string, cursor int64) {
	there := make(map[string]bool, len(ids))
	for _, id := range ids {
		there[id] = true
	}
	for id, m := range c.msgs {
		if !there[id] && m.created <= cursor && (!c.hasFloor || !ocBefore(m.created, m.id, c.floorC, c.floorID)) {
			m.deleted = true
		}
	}
}

// KnownBefore counts the messages read that were made at or before cursor
// (unix ms), from the first read on: as many as the database has, unless
// some were deleted since (see present). -1 while /undo's state is on: the
// messages there are named at every look then, so that the state cleared
// (/redo, or the next message sent and what was taken back deleted) is
// told apart in the same look, not one shown again meanwhile.
func (c *OpenCode) KnownBefore(cursor int64) int {
	if c.revertAt != "" {
		return -1
	}
	n := 0
	for _, m := range c.msgs {
		if m.created <= cursor && !m.deleted && (!c.hasFloor || !ocBefore(m.created, m.id, c.floorC, c.floorID)) {
			n++
		}
	}
	return n
}

// gone: what /undo took back: the messages after the one it went back
// to, and that one unless it went back to a part of it (OpenCode's ids are
// in the order they were made); and what was deleted.
func (c *OpenCode) gone(m *ocMsg) bool {
	if m.deleted {
		return true
	}
	if c.revertAt == "" {
		return false
	}
	return m.id > c.revertAt || (m.id == c.revertAt && !c.revertPart)
}

// goneNow marks these entries gone, or not (a /redo), as it is now.
func (c *OpenCode) goneNow(entries []*Entry) {
	for _, e := range entries {
		if m := c.msgs[e.ID]; m != nil && m.e == e {
			if g := c.gone(m); g != e.Gone {
				e.Gone, e.Rev = g, c.rev
			}
		}
	}
}

func (c *OpenCode) message(r OpenCodeRow) *ocMsg {
	if r.ID == "" {
		return nil
	}
	m := c.msgs[r.ID]
	if m != nil && (r.U < m.updated || (r.U == m.updated && r.D == m.raw)) {
		return nil // (the same row again, or one behind it)
	}
	if m == nil && c.hasFloor && ocBefore(r.C, r.ID, c.floorC, c.floorID) {
		return nil // (before what is read: read back in its turn)
	}
	var d ocMessage
	if json.Unmarshal([]byte(r.D), &d) != nil || (d.Role != "user" && d.Role != "assistant") {
		c.Skipped++
		return nil
	}
	if m == nil {
		m = &ocMsg{id: r.ID, created: r.C, parts: map[string]ocPart{}}
		c.msgs[r.ID] = m
		for id, p := range c.orphans[r.ID] {
			m.parts[id] = p
		}
		delete(c.orphans, r.ID)
	}
	m.updated, m.data, m.raw = r.U, d, r.D
	return m
}

func (c *OpenCode) part(r OpenCodeRow) *ocMsg {
	if r.ID == "" || r.M == "" {
		return nil
	}
	var d ocPartData
	if json.Unmarshal([]byte(r.D), &d) != nil {
		c.Skipped++
		return nil
	}
	p := ocPart{updated: r.U, raw: r.D, data: d}
	m := c.msgs[r.M]
	if m == nil {
		// (Its message is not read: before what was read, or its row not
		// come yet. Kept a while, so it is not lost if it comes.)
		o := c.orphans[r.M]
		if o == nil {
			if len(c.orphans) >= maxOcOrphanMsgs {
				return nil
			}
			o = map[string]ocPart{}
			c.orphans[r.M] = o
		}
		if len(o) < maxOcOrphanParts {
			if was, ok := o[r.ID]; !ok || r.U > was.updated || (r.U == was.updated && r.D != was.raw) {
				o[r.ID] = p
			}
		}
		return nil
	}
	if was, ok := m.parts[r.ID]; ok && (r.U < was.updated || (r.U == was.updated && r.D == was.raw)) {
		return nil
	}
	m.parts[r.ID] = p
	return m
}

func (c *OpenCode) inOrder(ms map[*ocMsg]bool) []*ocMsg {
	out := make([]*ocMsg, 0, len(ms))
	for m := range ms {
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].created != out[j].created {
			return out[i].created < out[j].created
		}
		return out[i].id < out[j].id
	})
	return out
}

// show makes a message's entry anew from what it holds now: one with
// nothing to show has none (yet).
func (c *OpenCode) show(m *ocMsg) {
	role := RoleAssistant
	if m.data.Role == "user" {
		role = RoleUser
	}
	parts := c.partsOf(m)
	if role == RoleAssistant {
		c.saw(m)
	}
	if len(parts) == 0 {
		return
	}
	if len(parts) > maxParts {
		parts = parts[:maxParts]
	}
	if m.e == nil {
		c.seq++
		m.e = &Entry{Seq: c.seq, ID: clip(m.id, maxID), Role: role, Time: ocTime(m.created)}
		c.entries = append(c.entries, m.e)
	}
	m.e.Rev, m.e.Parts = c.rev, parts
}

// saw takes what an answer says of the session: its model, how full the
// context is, whether it goes on (the latest answer's say).
func (c *OpenCode) saw(m *ocMsg) {
	latest := true
	for _, o := range c.msgs {
		if o.data.Role == "assistant" && (o.created > m.created || (o.created == m.created && o.id > m.id)) {
			latest = false
			break
		}
	}
	if !latest {
		return
	}
	model := m.data.ModelID
	if model == "" && m.data.Model != nil {
		model = m.data.Model.ModelID
	}
	if model != "" {
		c.meta.Model = clip(model, maxMeta)
	}
	if t := m.data.Tokens; t != nil && t.Input+t.Cache.Read+t.Cache.Write > 0 {
		c.meta.Context = t.Input + t.Cache.Read + t.Cache.Write
	}
	c.known = true
	c.working = m.data.Time.Completed == 0 && m.data.Error == nil
}

func (c *OpenCode) partsOf(m *ocMsg) []Part {
	ids := make([]string, 0, len(m.parts))
	for id := range m.parts {
		ids = append(ids, id)
	}
	sort.Strings(ids) // (OpenCode's ids are in the order they were made)
	var out []Part
	for _, id := range ids {
		p := m.parts[id].data
		switch p.Type {
		case "text":
			if p.Synthetic || p.Ignored || strings.TrimSpace(p.Text) == "" {
				continue
			}
			t, cut := cut(clean(strings.TrimLeft(p.Text, "\n")), maxText)
			out = append(out, Part{Kind: KindText, Text: t, Truncated: cut})
		case "reasoning":
			if strings.TrimSpace(p.Text) == "" {
				continue
			}
			t, cut := cut(clean(strings.TrimSpace(p.Text)), maxText)
			out = append(out, Part{Kind: KindThinking, Text: t, Truncated: cut})
		case "file":
			if strings.HasPrefix(p.Mime, "image/") {
				out = append(out, Part{Kind: KindImage})
			}
		case "tool":
			if t := c.tool(p, m.parts[id].updated); t != nil {
				out = append(out, Part{Kind: KindTool, Tool: t})
			}
		}
	}
	if e := m.data.Error; e != nil && e.Name != "MessageAbortedError" {
		text := e.Data.Message
		if text == "" {
			text = e.Name
		}
		t, cut := cut(clean(text), maxTrouble)
		out = append(out, Part{Kind: KindTrouble, Trouble: &Trouble{Kind: TroubleAPI, Text: t, Status: e.Data.StatusCode, Cut: cut}})
	} else if m.data.Finish == "length" {
		out = append(out, Part{Kind: KindTrouble, Trouble: &Trouble{Kind: TroubleCut}})
	}
	return out
}

// ocTools: OpenCode's tools by the names the view knows them by (Claude
// Code's), their inputs' keys as Claude Code's are.
var ocTools = map[string]string{
	"bash": "Bash", "read": "Read", "edit": "Edit", "multiedit": "MultiEdit", "write": "Write",
	"glob": "Glob", "grep": "Grep", "list": "LS", "webfetch": "WebFetch", "websearch": "WebSearch",
	"codesearch": "WebSearch", "task": "Task", "todowrite": "TodoWrite", "todoread": "TodoRead",
	"skill": "Skill", "question": "AskUserQuestion",
}

var ocKeys = map[string]string{"filePath": "file_path", "oldString": "old_string", "newString": "new_string", "replaceAll": "replace_all"}

func (c *OpenCode) tool(p ocPartData, at int64) *Tool {
	st := p.State
	if st == nil {
		return nil
	}
	if p.Tool == "apply_patch" || p.Tool == "patch" {
		// (What GPT models edit with: Codex's patch, whose files are told of
		// each as an edit.)
		var in struct {
			Patch     string `json:"patchText"`
			PatchText string `json:"patch"`
		}
		_ = json.Unmarshal(st.Input, &in)
		ts := patchTools(firstNonEmpty(in.Patch, in.PatchText))
		if len(ts) > 0 {
			t := ts[0]
			c.done(t, st)
			return t
		}
	}
	name := ocTools[p.Tool]
	if name == "" {
		name = clip(p.Tool, 128)
	}
	// Its input, keyed as Claude Code's tools' are, says what it acts on.
	var raw map[string]json.RawMessage
	_ = json.Unmarshal(st.Input, &raw)
	in := map[string]json.RawMessage{}
	for k, v := range raw {
		if kk, ok := ocKeys[k]; ok {
			k = kk
		}
		in[k] = v
	}
	b, _ := json.Marshal(in)
	t := &Tool{ID: clip(p.CallID, maxID), Name: name}
	t.Title, t.Detail, t.Old, t.New = toolInput(name, b)
	switch name {
	case "Edit", "MultiEdit", "Write":
		t.Add, t.Del = editLines(name, b)
	case "TodoWrite":
		c.todos(b, at)
	}
	if md := st.Metadata; md != nil && (name == "Edit" || name == "MultiEdit" || name == "Write") {
		diff := md.Diff
		if md.FileDiff != nil && md.FileDiff.Patch != "" {
			diff = md.FileDiff.Patch
		}
		if hunks, add, del, short := unifiedHunks(diff); len(hunks) > 0 {
			t.Patch, t.PatchCut, t.Add, t.Del, t.Old, t.New = hunks, short, add, del, "", ""
		}
	}
	c.done(t, st)
	return t
}

// done: what came back of a call, once it has.
func (c *OpenCode) done(t *Tool, st *ocToolState) {
	switch st.Status {
	case "completed":
		t.Done = true
		t.Result, t.Truncated = cut(clean(st.Output), maxResult)
		t.ResultLines = lineCount(st.Output)
		if md := st.Metadata; md != nil && md.Exit != nil && *md.Exit != 0 {
			t.Error = true
		}
	case "error":
		t.Done, t.Error = true, true
		t.Result, t.Truncated = cut(clean(st.Error), maxResult)
		t.ResultLines = lineCount(st.Error)
	}
}

// todos: the task list a todowrite set, if it is the latest.
func (c *OpenCode) todos(in []byte, at int64) {
	if at < c.tasksAt {
		return
	}
	var v struct {
		Todos []struct {
			Content string `json:"content"`
			Status  string `json:"status"`
		} `json:"todos"`
	}
	if json.Unmarshal(in, &v) != nil {
		return
	}
	tasks := []Task{}
	for _, t := range v.Todos {
		if len(tasks) >= maxTasks {
			break
		}
		tasks = append(tasks, Task{Subject: clip(clean(t.Content), maxTitle), Status: clip(t.Status, 32)})
	}
	c.tasks, c.tasksAt = tasks, at
}

var hunkHead = regexp.MustCompile(`^@@ -(\d+)(?:,\d+)? \+(\d+)`)

// unifiedHunks reads a unified diff (OpenCode's of an edit): its hunks,
// the lines it adds and removes (all of them, where fewer are kept), and
// whether some were left out.
func unifiedHunks(diff string) (hunks []Hunk, add, del int, short bool) {
	size := 0
	var h *Hunk
	for _, l := range strings.Split(diff, "\n") {
		if strings.HasPrefix(l, "@@") {
			if m := hunkHead.FindStringSubmatch(l); m != nil {
				a, _ := strconv.Atoi(m[1])
				b, _ := strconv.Atoi(m[2])
				hunks = append(hunks, Hunk{OldStart: a, NewStart: b})
				h = &hunks[len(hunks)-1]
			}
			continue
		}
		if h == nil || l == "" || (l[0] != ' ' && l[0] != '-' && l[0] != '+') {
			continue // (the header before the first hunk; "\ No newline at end of file")
		}
		switch l[0] {
		case '+':
			add++
		case '-':
			del++
		}
		l, c := cut(clean(l), maxPatchLine)
		if size+len(l) > maxPatch {
			short = true
			continue
		}
		size += len(l)
		h.Lines = append(h.Lines, l)
		short = short || c
	}
	out := hunks[:0]
	for _, h := range hunks {
		if len(h.Lines) > 0 {
			out = append(out, h)
		}
	}
	return out, add, del, short
}

// Prepend puts o, older messages read on their own, before c's.
func (c *OpenCode) Prepend(other Parser) {
	o, ok := other.(*OpenCode)
	if !ok {
		return
	}
	resort := false
	for id, m := range o.msgs {
		if c.msgs[id] == nil {
			c.msgs[id] = m
			merged := false
			for pid, p := range c.orphans[id] {
				if was, ok := m.parts[pid]; !ok || p.updated > was.updated {
					m.parts[pid], merged = p, true
				}
			}
			delete(c.orphans, id)
			if merged {
				if m.e != nil {
					m.e.Parts = o.partsOf(m) // (as it is now: its Rev is set below)
				} else {
					o.show(m)
					resort = true
				}
			}
		}
	}
	before := func() {} // (o's floor, once it is known what of o is kept)
	if o.hasFloor {
		before = func() { c.SetFloor(o.floorC, o.floorID) }
	}
	if c.title == "" {
		c.title = o.title
	}
	if c.meta.Model == "" {
		c.meta.Model = o.meta.Model
	}
	if c.meta.Context == 0 {
		c.meta.Context = o.meta.Context
	}
	if o.tasksAt > c.tasksAt {
		c.tasks, c.tasksAt = o.tasks, o.tasksAt
	}
	if !c.known {
		c.working, c.known = o.working, o.known
	}
	if resort { // (one shown only now, its text read after it: in its place)
		at := func(e *Entry) *ocMsg { return o.msgs[e.ID] }
		sort.SliceStable(o.entries, func(i, j int) bool {
			a, b := at(o.entries[i]), at(o.entries[j])
			return a != nil && b != nil && ocBefore(a.created, a.id, b.created, b.id)
		})
	}
	var add []*Entry
	for _, e := range o.entries {
		if c.msgs[e.ID] != nil && c.msgs[e.ID].e == e {
			add = append(add, e)
		}
	}
	if room := max(0, maxEntries-len(c.entries)); len(add) > room {
		for _, e := range add[:len(add)-room] {
			delete(c.msgs, e.ID)
		}
		add = add[len(add)-room:]
		c.trimmed = true
		// (What was let go stays below the floor; with none kept, it stays.)
		before = func() {}
		first := c.entries
		if len(add) > 0 {
			first = add
		}
		if len(first) > 0 {
			if m := c.msgs[first[0].ID]; m != nil {
				c.floorC, c.floorID, c.hasFloor = m.created, m.id, true
			}
		}
	}
	before()
	if len(add) == 0 {
		return
	}
	c.rev++
	first := c.seq + 1
	if len(c.entries) > 0 {
		first = c.entries[0].Seq
	}
	for i, e := range add {
		e.Seq = first - len(add) + i
		e.Rev = c.rev
		if m := c.msgs[e.ID]; m != nil {
			e.Gone = c.gone(m) // (what /undo took back, as c knows it now)
		}
	}
	c.entries = append(append([]*Entry(nil), add...), c.entries...)
}

func (c *OpenCode) trim() {
	if n := len(c.entries) - maxEntries; n > 0 {
		c.trimmed = true
		for _, e := range c.entries[:n] {
			delete(c.msgs, e.ID)
		}
		c.entries = append([]*Entry(nil), c.entries[n:]...)
		// (What was let go is before the floor now: fed again, not new.)
		if m := c.msgs[c.entries[0].ID]; m != nil {
			c.floorC, c.floorID, c.hasFloor = m.created, m.id, true
		}
	}
}

// ocTitle: the session's title, but for the one OpenCode gives a session
// before it has named it ("New session - 2026-10-23T01:46:00.484Z").
func ocTitle(t string) string {
	if strings.HasPrefix(t, "New session - ") || strings.HasPrefix(t, "Child session - ") {
		return ""
	}
	return clip(clean(t), maxTitle)
}

func ocTime(ms int64) string {
	if ms <= 0 {
		return ""
	}
	return time.UnixMilli(ms).UTC().Format("2006-01-02T15:04:05.000Z")
}
