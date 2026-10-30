package convo

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Codex reads Codex's session log (~/.codex/sessions/…/rollout-*.jsonl),
// fed in order, into entries as Claude does Claude Code's:
//   - "event_msg" rows of type item_completed with a UserMessage item hold
//     what the person wrote (the "user" response items also hold what
//     Codex sends on their behalf: instructions, the environment);
//   - "response_item" rows hold the model's output: a message (its text),
//     reasoning (its summary, as thinking), a tool call (custom_tool_call:
//     a script calling tools.exec_command, tools.apply_patch…; or a
//     function_call) and, by call_id, what came back;
//   - task_started, task_complete and turn_aborted say whether it works;
//   - turn_context and thread_settings_applied say the model and the
//     approval policy; token_count how full the context is;
//   - "compacted" rows hold the summary of a compacted session.
type Codex struct {
	entries []*Entry
	seq     int
	rev     int
	tools   map[string][]*Tool // by call_id (an apply_patch of several files is one call)
	// seen: the things Codex tells of on their own (a command it ran, a
	// search it made), by the id it gives them, so that one told of again
	// (as it goes on, then as it ends) is the one line throughout.
	seen map[string]*Tool
	// itemCmds: it tells of each command on its own in this conversation,
	// so the lines of the calls that run them are not shown (they would be
	// the same commands again).
	itemCmds bool
	orphans  map[string]codexOut
	meta     Meta
	title    string // (its first message: Codex names no session)
	working  bool
	known    bool // (a row said whether it works)
	trimmed  bool
	ctxSaid  bool
	Skipped  int
	// Turns, to tell what a backtrack takes back: the first entry of each
	// turn not taken back (a turn starts at task_started; its first entry is
	// the person's message), the latest last. turnOpen: a turn has started
	// and has no entry yet. backPending: turns a backtrack took back that
	// began in the part before (told when it is put before).
	turns       []*Entry
	turnOpen    bool
	backPending int
	// plans: the task lists update_plan gave, each with the line it gave it
	// on (the latest not taken back is the list; see Tasks).
	plans []codexPlan
	// asked: questions put to the person (request_user_input) not yet
	// answered: the next message of theirs answers them.
	asked []*Tool
	// hidden: calls of the code mode's whose lines went (each of their
	// things told of on its own): what comes back of them is let be.
	hidden map[string]bool
}

type codexPlan struct {
	e     *Entry
	t     *Tool
	tasks []Task
}

// Bounds of what is kept of plans, questions and hidden calls.
const (
	maxPlans  = 200
	maxAsked  = 16
	maxHidden = 1024
)

// maxTurns is how many turns' starts are kept (a backtrack goes back one
// or two).
const maxTurns = 1000

type codexOut struct {
	text   string
	failed bool
	images int
}

func NewCodex() *Codex { return &Codex{tools: map[string][]*Tool{}} }

func (c *Codex) Rev() int      { return c.rev }
func (c *Codex) Title() string { return c.title }
func (c *Codex) Meta() Meta {
	m := c.meta
	if m.Window > 0 {
		m.WindowBy = "log"
	}
	return m
}
func (c *Codex) Trimmed() bool { return c.trimmed }

// Tasks: the list the last update_plan not taken back gave (none, where
// all of them were taken back or none was given).
func (c *Codex) Tasks() []Task {
	for i := len(c.plans) - 1; i >= 0; i-- {
		if !c.plans[i].e.Gone {
			return append([]Task(nil), c.plans[i].tasks...)
		}
	}
	return nil
}
func (c *Codex) Queued() []Queued   { return nil }
func (c *Codex) Running() []Running { return nil }

// (Codex's queue is not in its log.)
func (c *Codex) ClearQueue()      {}
func (c *Codex) QueueSince(int64) {}

// Status is "busy" while a turn goes on, "idle" after it, "" when no row
// has said.
func (c *Codex) Status() string {
	switch {
	case !c.known:
		return ""
	case c.working:
		return "busy"
	}
	return "idle"
}

func (c *Codex) First() int {
	if len(c.entries) == 0 {
		return 0
	}
	return c.entries[0].Seq
}

func (c *Codex) Feed(lines []byte) {
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

func (c *Codex) Since(rev int) []Entry { return since(c.entries, rev) }

// Prepend puts o, a part of the log read on its own from before what c
// read, before c's entries; what of o's calls came back in c's part is
// filled in.
func (c *Codex) Prepend(other Parser) {
	o, ok := other.(*Codex)
	if !ok {
		return
	}
	for id, out := range c.orphans {
		if ts, ok := o.tools[id]; ok {
			o.answer(ts, out)
			delete(c.orphans, id)
		}
	}
	// A thing told of in this part whose call is in the part before, or
	// told of there as well (as it went on): one line, this part's (what is
	// latest of it), with what only the call said of it (what it was for).
	// The part before's line goes: none of it was shown yet.
	dropIDs := map[string]bool{}
	var changed []*Entry
	for id, ct := range c.seen {
		if ots, ok := o.tools[id]; ok && len(ots) > 0 {
			if e := mergeCall(ct, ots); e != nil {
				changed = append(changed, e)
				// (The call's id is this line's now: what comes back of it
				// is put on it.)
				var lines []*Tool
				for _, p := range e.Parts {
					if p.Tool != nil && p.Tool.ID == ct.ID {
						lines = append(lines, p.Tool)
					}
				}
				c.tools[id] = lines
			}
			dropIDs[id] = true
			delete(o.tools, id)
		}
		if ot := o.seen[id]; ot != nil && ot != ct {
			dropIDs[id] = true // (all of its lines there: a change of several files told twice)
			delete(o.seen, id)
		}
	}
	if len(dropIDs) > 0 {
		o.entries = dropToolIDs(o.entries, dropIDs)
		o.turns = keptTurns(o.turns, o.entries)
	}
	if len(changed) > 0 {
		c.rev++
		for _, e := range changed {
			e.Rev = c.rev
		}
	}
	// Its plans before this part's; its questions answered by a message of
	// the person's in this one.
	c.plans = append(append([]codexPlan(nil), o.plans...), c.plans...)
	if n := len(c.plans) - maxPlans; n > 0 {
		c.plans = c.plans[n:]
	}
	answered := false
	for _, e := range c.entries {
		if e.Role == RoleUser {
			answered = true
			break
		}
	}
	if answered {
		for _, t := range o.asked {
			t.Done = true
		}
	} else {
		c.asked = append(append([]*Tool(nil), o.asked...), c.asked...)
		if n := len(c.asked) - maxAsked; n > 0 {
			for _, t := range c.asked[:n] {
				t.Done = true // (let go of, as over)
			}
			c.asked = c.asked[n:]
		}
	}
	// (Each command told of on its own in either part: the lines of the
	// calls that run them go, in the part before this one too.)
	if c.itemCmds || o.itemCmds {
		if !c.itemCmds {
			c.rev++       // (this part's entries change: told as of a revision after the page's)
			c.itemsTell() // (told of on their own in the part before: this part's script lines go too)
		}
		o.itemCmds = false
		o.itemsTell()
	}
	// (Its calls whose lines went, then or now: what came back of them here is let be.)
	for id := range o.hidden {
		delete(c.orphans, id) // (what came back of a call whose things are told of on their own)
		if c.hidden == nil {
			c.hidden = map[string]bool{}
		}
		if len(c.hidden) < maxHidden {
			c.hidden[id] = true
		}
	}
	for id, t := range o.seen {
		if len(c.seen) >= maxSeenItems {
			break
		}
		if c.seen == nil {
			c.seen = map[string]*Tool{}
		}
		if _, there := c.seen[id]; !there {
			c.seen[id] = t
		}
	}
	if o.title != "" { // (its first message is the session's first)
		c.title = o.title
	}
	if c.meta.Model == "" {
		c.meta.Model = o.meta.Model
	}
	if c.meta.Mode == "" {
		c.meta.Mode = o.meta.Mode
	}
	if c.meta.Branch == "" {
		c.meta.Branch = o.meta.Branch
	}
	if !c.ctxSaid {
		c.meta.Context, c.ctxSaid = o.meta.Context, o.ctxSaid
	}
	if c.meta.Window == 0 {
		c.meta.Window = o.meta.Window
	}
	if !c.known {
		c.working, c.known = o.working, o.known
	}
	// What c's backtracks took back of o's part: its last turns. (A turn o
	// began with nothing in it yet is the one c's entries start, already
	// counted as of the part before: one fewer of o's.)
	if k := c.backPending; k > 0 {
		if o.turnOpen && len(c.entries) > 0 {
			k--
		}
		c.backPending = 0
		if k > 0 {
			if k <= len(o.turns) {
				from := o.turns[len(o.turns)-k].Seq
				o.turns = o.turns[:len(o.turns)-k]
				goneFrom(o.entries, from)
			} else {
				c.backPending = min(k-len(o.turns), maxTurns)
				o.turns = nil
				goneFrom(o.entries, minSeq)
			}
		}
	}
	c.backPending = min(c.backPending+o.backPending, maxTurns)
	// (A turn o began with nothing in it yet begins with c's first entry;
	// with none, with c's next.)
	if o.turnOpen && len(c.entries) == 0 {
		c.turnOpen = true
	}
	if o.turnOpen && len(c.entries) > 0 && !c.entries[0].Gone && (len(c.turns) == 0 || c.turns[0] != c.entries[0]) {
		c.turns = append([]*Entry{c.entries[0]}, c.turns...)
	}
	if len(o.turns) > 0 {
		c.turns = append(append([]*Entry(nil), o.turns...), c.turns...)
		if n := len(c.turns) - maxTurns; n > 0 {
			c.turns = c.turns[n:]
		}
	}
	add := o.entries
	if room := max(0, maxEntries-len(c.entries)); len(add) > room {
		add = add[len(add)-room:]
		c.trimmed = true
		// (Turns begun in what is left out: the first of them kept begins
		// with the first entry kept.)
		c.turns = keptTurns(c.turns, append(append([]*Entry(nil), add...), c.entries...))
	}
	for id, ts := range o.tools {
		if _, ok := c.tools[id]; !ok {
			c.tools[id] = ts
		}
	}
	// (What came back in o's part of calls before it: kept, for a part read before o.)
	for id, out := range o.orphans {
		if c.orphans == nil {
			c.orphans = map[string]codexOut{}
		}
		if _, ok := c.orphans[id]; !ok && len(c.orphans) < maxOrphans {
			c.orphans[id] = out
		}
	}
	if len(add) == 0 {
		c.keepOfEntries()
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
	}
	c.entries = append(append([]*Entry(nil), add...), c.entries...)
	c.keepOfEntries()
}

// keepOfEntries: of plans and questions, those of the entries kept (a
// part put before may have been cut to the room left).
func (c *Codex) keepOfEntries() {
	in := make(map[*Entry]bool, len(c.entries))
	for _, e := range c.entries {
		in[e] = true
	}
	plans := c.plans[:0]
	for _, p := range c.plans {
		if in[p.e] {
			plans = append(plans, p)
		}
	}
	c.plans = plans
	asked := c.asked[:0]
	for _, t := range c.asked {
		if in[t.at] {
			asked = append(asked, t)
		}
	}
	c.asked = asked
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func (c *Codex) trim() {
	if n := len(c.entries) - maxEntries; n > 0 {
		c.trimmed = true
		gone := map[*Tool]bool{}
		for _, e := range c.entries[:n] {
			for _, p := range e.Parts {
				if p.Tool != nil {
					gone[p.Tool] = true
				}
			}
		}
		for id, ts := range c.tools {
			if len(ts) > 0 && gone[ts[0]] {
				delete(c.tools, id)
			}
		}
		for id, t := range c.seen {
			if gone[t] {
				delete(c.seen, id)
			}
		}
		// (A plan, a question, of what is left out: no longer of the view.)
		plans := c.plans[:0]
		for _, p := range c.plans {
			if !gone[p.t] {
				plans = append(plans, p)
			}
		}
		c.plans = plans
		asked := c.asked[:0]
		for _, t := range c.asked {
			if !gone[t] {
				asked = append(asked, t)
			}
		}
		c.asked = asked
		c.entries = append([]*Entry(nil), c.entries[n:]...)
		c.turns = keptTurns(c.turns, c.entries)
	}
}

// keptTurns: of turns' starts, those among entries (what is left after
// some were left out); a turn begun in what was left out begins with the
// first entry kept.
func keptTurns(turns, entries []*Entry) []*Entry {
	in := make(map[*Entry]bool, len(entries))
	for _, e := range entries {
		in[e] = true
	}
	var out []*Entry
	for _, e := range turns {
		if in[e] {
			out = append(out, e)
		}
	}
	if len(out) < len(turns) && len(entries) > 0 && !entries[0].Gone && (len(out) == 0 || out[0] != entries[0]) {
		out = append([]*Entry{entries[0]}, out...)
	}
	return out
}

type codexRow struct {
	Timestamp string          `json:"timestamp"`
	Type      string          `json:"type"`
	Payload   json.RawMessage `json:"payload"`
}

type codexPayload struct {
	Type    string          `json:"type"`
	ID      string          `json:"id"`
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
	Summary []struct {
		Text string `json:"text"`
	} `json:"summary"`
	CallID    string          `json:"call_id"`
	Name      string          `json:"name"`
	Namespace string          `json:"namespace"`
	Input     string          `json:"input"`
	Arguments string          `json:"arguments"`
	Output    json.RawMessage `json:"output"`
	Item      *codexItem      `json:"item"`
	Info      *struct {
		Last *struct {
			Input int64 `json:"input_tokens"`
		} `json:"last_token_usage"`
		Window int64 `json:"model_context_window"`
	} `json:"info"`
	Reason   string `json:"reason"`
	NumTurns int    `json:"num_turns"` // (thread_rolled_back)
	// turn_context, thread_settings_applied
	Model          string `json:"model"`
	ApprovalPolicy string `json:"approval_policy"`
	Settings       *struct {
		Model          string `json:"model"`
		ApprovalPolicy string `json:"approval_policy"`
	} `json:"thread_settings"`
	Git *struct {
		Branch string `json:"branch"`
	} `json:"git"`
	// compacted; user_message and error (older Codex)
	Message string `json:"message"`
	// web_search_end, view_image_tool_call, mcp_tool_call_end (older Codex)
	Query      string `json:"query"`
	Path       string `json:"path"`
	Invocation *struct {
		Server    string          `json:"server"`
		Tool      string          `json:"tool"`
		Arguments json.RawMessage `json:"arguments"`
	} `json:"invocation"`
	Result json.RawMessage `json:"result"`
	// exited_review_mode
	Review *struct {
		Explanation string `json:"overall_explanation"`
	} `json:"review_output"`
	// thread_goal_updated
	Goal *struct {
		Objective string `json:"objective"`
		Status    string `json:"status"`
	} `json:"goal"`
}

func (c *Codex) line(b []byte) {
	var r codexRow
	if json.Unmarshal(b, &r) != nil {
		c.Skipped++
		return
	}
	var p codexPayload
	_ = json.Unmarshal(r.Payload, &p)
	switch r.Type {
	case "session_meta":
		if p.Git != nil && p.Git.Branch != "" {
			c.meta.Branch = clip(p.Git.Branch, maxMeta)
		}
	case "turn_context":
		c.settings(p.Model, p.ApprovalPolicy)
	case "compacted":
		if t := strings.TrimSpace(p.Message); t != "" {
			c.add(&r, p.ID, RoleSummary, []Part{textPart(KindText, t)})
		}
		c.meta.Context, c.ctxSaid = 0, true
	case "event_msg":
		c.event(&r, &p)
	case "response_item":
		c.item(&r, &p)
	default:
		c.Skipped++
	}
}

func (c *Codex) settings(model, policy string) {
	if m := strings.TrimSpace(model); m != "" {
		c.meta.Model = clip(m, maxMeta)
	}
	if m := strings.TrimSpace(policy); m != "" {
		c.meta.Mode = clip(m, maxMeta)
	}
}

func (c *Codex) event(r *codexRow, p *codexPayload) {
	switch p.Type {
	case "task_started":
		c.working, c.known = true, true
		c.turnOpen = true
	case "thread_rolled_back":
		// Codex's backtrack (Esc Esc, then a message edited and sent
		// again): the last turns go, the person's message with all that
		// came of it and the turn's interruption.
		c.backtrack(p.NumTurns)
	case "task_complete":
		c.working, c.known = false, true
	case "turn_aborted":
		c.working, c.known = false, true
		if p.Reason == "interrupted" {
			c.add(r, "", RoleNote, []Part{textPart(KindText, "[Request interrupted by user]")})
		}
	case "thread_settings_applied":
		if p.Settings != nil {
			c.settings(p.Settings.Model, p.Settings.ApprovalPolicy)
		}
	case "token_count":
		if p.Info != nil && p.Info.Last != nil && p.Info.Last.Input > 0 {
			c.meta.Context, c.ctxSaid = p.Info.Last.Input, true
		}
		if p.Info != nil && p.Info.Window > 0 {
			c.meta.Window = p.Info.Window
		}
	case "item_completed":
		if p.Item == nil {
			return
		}
		// (A command it ran, a search it made, a file it changed…: told of
		// here, where it is done: the line of the call of the same id, if
		// there is one, is the one it makes up. What it said and thought is
		// told of again as response_item rows, and is read from those.)
		it := p.Item
		switch it.Type {
		case "CommandExecution":
			c.ranCommand(r, it)
		case "WebSearch":
			c.searched(r, it.ID, it.Query)
		case "Extension":
			if it.Kind == "web.search" && it.Action != nil {
				c.searched(r, it.ID, strings.Join(append([]string{it.Action.Query}, it.Action.Queries...), "\n"))
			}
		case "FileChange":
			c.fileChanged(r, it.ID, it.Changes, it.Status == "failed")
		case "ImageView":
			c.done(r, it.ID, &Tool{Name: "Read", Detail: clip(strings.TrimPrefix(it.Path, "file://"), maxDetail)}, "", false)
		case "McpToolCall":
			text, failed, pics := mcpResult(it.Result)
			t := codexMCP(it.Server, it.Tool, it.Arguments)
			t.Images = pics
			c.done(r, it.ID, t, text, failed || it.Status == "failed")
		case "ContextCompaction":
			c.add(r, it.ID, RoleNote, []Part{{Kind: KindEvent, Event: &Event{Kind: EventCompact}}})
		case "UserMessage":
			var parts []Part
			for _, b := range codexContent(it.Content) {
				switch {
				case b.Type == "text" || b.Type == "input_text":
					if strings.TrimSpace(b.Text) != "" {
						parts = append(parts, textPart(KindText, b.Text))
					}
				case strings.Contains(b.Type, "image"):
					parts = append(parts, Part{Kind: KindImage})
				}
			}
			c.userSaid(r, it.ID, parts)
		}
	// An older Codex's account (to 0.147): what the person wrote, what went
	// wrong, a review, and what its tools did when it told of them so.
	case "user_message":
		if strings.TrimSpace(p.Message) != "" {
			c.userSaid(r, "", []Part{textPart(KindText, p.Message)})
		}
	case "error":
		if t := strings.TrimSpace(p.Message); t != "" {
			c.add(r, "", RoleNote, []Part{{Kind: KindTrouble, Trouble: &Trouble{Kind: "api", Text: clip(t, maxDetail)}}})
		}
	case "exited_review_mode":
		if p.Review != nil && strings.TrimSpace(p.Review.Explanation) != "" {
			c.add(r, "", RoleAssistant, []Part{textPart(KindText, p.Review.Explanation)})
		}
	case "context_compacted":
		c.add(r, "", RoleNote, []Part{{Kind: KindEvent, Event: &Event{Kind: EventCompact}}})
	case "web_search_end":
		c.searched(r, p.CallID, p.Query)
	case "view_image_tool_call":
		c.done(r, p.CallID, &Tool{Name: "Read", Detail: clip(strings.TrimPrefix(p.Path, "file://"), maxDetail)}, "", false)
	case "mcp_tool_call_end":
		if p.Invocation != nil {
			text, failed, pics := mcpResult(p.Result)
			t := codexMCP(p.Invocation.Server, p.Invocation.Tool, p.Invocation.Arguments)
			t.Images = pics
			c.done(r, p.CallID, t, text, failed)
		}
	case "thread_goal_updated":
		if p.Goal != nil && strings.TrimSpace(p.Goal.Objective) != "" {
			c.add(r, "", RoleAssistant, []Part{{Kind: KindTool, Tool: &Tool{Name: "update_goal", Detail: clip(clean(p.Goal.Objective), maxDetail), Done: true}}})
		}
	}
}

// userSaid: a message of the person's. The first names the conversation
// (Codex names none); it answers the questions put to them before.
func (c *Codex) userSaid(r *codexRow, id string, parts []Part) {
	if len(parts) == 0 {
		return
	}
	if c.title == "" {
		for _, pt := range parts {
			if pt.Kind == KindText {
				c.title = codexTitle(pt.Text)
				break
			}
		}
	}
	for _, t := range c.asked {
		t.Done = true
	}
	c.touch(c.asked)
	c.asked = nil
	c.add(r, id, RoleUser, parts)
}

// codexMCP: an MCP server's tool called (its name as Claude Code's are:
// mcp__<server>__<tool>; Codex's own web tool as a search).
func codexMCP(server, tool string, args json.RawMessage) *Tool {
	if server == "codex_apps" && strings.HasSuffix(tool, "web_run") {
		var a map[string]json.RawMessage
		_ = json.Unmarshal(args, &a)
		return webRun(a)[0]
	}
	return &Tool{Name: clip("mcp__"+server+"__"+tool, 128), Detail: clip(compactJSON(string(args)), maxDetail)}
}

// done: a thing Codex did, told of when done: on the line of its call (of
// the same id), else one of its own (once: told again, the same line). A
// question its call put that waits stays open: the person answers it.
func (c *Codex) done(r *codexRow, id string, t *Tool, text string, failed bool) {
	id = clip(id, maxID)
	if ts, ok := c.tools[id]; ok && len(ts) > 0 {
		for _, x := range ts {
			if !x.waits {
				x.Done = true
				x.Error = x.Error || failed
			}
		}
		if text != "" && ts[0].Result == "" {
			ts[0].setResult(clean(text))
			ts[0].ResultLines = lineCount(text)
		}
		if ts[0].Images == 0 {
			ts[0].Images = t.Images
		}
		c.touch(ts)
		return
	}
	if was := c.seen[id]; id != "" && was != nil {
		was.Done, was.Error = true, was.Error || failed
		c.touch([]*Tool{was})
		return
	}
	c.itemsTell() // (of no call of its id: the code mode's, its script's lines go)
	t.ID, t.Done, t.Error = id, true, failed
	if text != "" {
		t.setResult(clean(text))
		t.ResultLines = lineCount(text)
	}
	c.add(r, id, RoleAssistant, []Part{{Kind: KindTool, Tool: t}})
	if id != "" {
		c.mark(id, t)
	}
}

// fileChanged: files a change made (FileChange). On the lines of the call
// that made it (a patch applied by a command: one line a file), what the
// patch did not say (a file moved, one it did not name, its failing); else
// a line of its own, as the code mode's changes have.
func (c *Codex) fileChanged(r *codexRow, id string, changes map[string]codexChange, failed bool) {
	id = clip(id, maxID)
	if len(changes) == 0 || id == "" {
		return
	}
	ts, more := changeTools(changes)
	if was, ok := c.tools[id]; ok && len(was) > 0 {
		at := was[0].at
		byPath := map[string]*Tool{}
		for _, t := range was {
			byPath[t.Detail] = t
			if !t.waits { // (a question its script put: the person answers it)
				t.Done, t.Error = true, t.Error || failed
			}
		}
		for _, t := range ts {
			if old := byPath[t.Detail]; old != nil {
				if strings.Contains(t.Title, " → ") {
					old.Title = t.Title
				}
				continue
			}
			for _, x := range was {
				if strings.HasSuffix(t.Detail, "/"+x.Detail) && x.Detail != "" {
					if strings.Contains(t.Title, " → ") {
						x.Title = t.Title
					}
					t = nil
					break
				}
			}
			if t == nil || at == nil || len(at.Parts) >= maxParts {
				continue
			}
			t.ID, t.Done, t.Error, t.at = id, true, failed, at
			at.Parts = append(at.Parts, Part{Kind: KindTool, Tool: t})
			c.tools[id] = append(c.tools[id], t)
		}
		c.touch(c.tools[id])
		return
	}
	if c.seen[id] != nil {
		return
	}
	c.itemsTell()
	var parts []Part
	for _, t := range ts {
		t.ID, t.Done, t.Error = id, true, failed
		parts = append(parts, Part{Kind: KindTool, Tool: t})
	}
	if more > 0 && len(ts) > 0 {
		ts[len(ts)-1].MoreFiles = more
	}
	c.add(r, id, RoleAssistant, parts)
	if len(ts) > 0 {
		c.mark(id, ts[0])
	}
}

// codexItem is a thing the agent did, as Codex tells of it when it is done
// (its newer account: a command it ran, a search it made).
// ranCommand: a command the agent ran, with what it printed and how it
// ended. Where the same one was told of as a call (an older Codex, or a
// tool of its own), that one holds it and this is let be.
func (c *Codex) ranCommand(r *codexRow, it *codexItem) {
	id := clip(it.ID, maxID)
	if id == "" {
		return
	}
	cmd := strings.Join(it.Command, " ")
	if len(it.Parsed) > 0 && it.Parsed[0].Cmd != "" {
		cmd = it.Parsed[0].Cmd // (what it means to run, without the shell it runs it in)
	}
	failed := it.Status == "failed" || (it.ExitCode != nil && *it.ExitCode != 0)
	out := it.Output
	if it.ExitCode != nil && *it.ExitCode != 0 {
		out = fmt.Sprintf("Exit code %d\n%s", *it.ExitCode, out)
	}
	// (Of a function call's command: the call's line has it.)
	if ts, ok := c.tools[id]; ok {
		for _, t := range ts {
			if t.Name != "Bash" {
				continue
			}
			t.Done, t.Error = it.Status != "in_progress", failed
			t.setResult(clean(out))
			t.ResultLines = lineCount(out)
		}
		c.touch(ts)
		return
	}
	c.itemsTell()
	if strings.TrimSpace(cmd) == "" {
		return
	}
	t := &Tool{ID: id, Name: "Bash", Detail: clip(clean(cmd), maxDetail)}
	// (Told of again as it ends: the line it had is the one that changes.)
	if was, ok := c.seen[id]; ok {
		t = was
		t.Detail = clip(clean(cmd), maxDetail)
	}
	t.Done, t.Error = it.Status != "in_progress", failed
	t.setResult(clean(out))
	t.ResultLines = lineCount(out)
	if _, ok := c.seen[id]; ok {
		c.touch([]*Tool{t})
		return
	}
	c.add(r, id, RoleAssistant, []Part{{Kind: KindTool, Tool: t}})
	c.mark(id, t)
}

// itemsTell: Codex tells of each command on its own in this conversation.
// What the calls that run them showed of those commands goes: the same
// commands, told twice, with less of them in the line of the call.
func (c *Codex) itemsTell() {
	if c.itemCmds {
		return
	}
	c.itemCmds = true
	for _, e := range c.entries {
		kept, dropped := e.Parts[:0], false
		for _, p := range e.Parts {
			if p.Kind == KindTool && p.Tool != nil && p.Tool.fromCall {
				dropped = true
				if ts := c.tools[p.Tool.ID]; len(ts) > 0 && ts[0] == p.Tool {
					c.hide(p.Tool.ID)
				}
				continue
			}
			kept = append(kept, p)
		}
		if dropped {
			e.Parts, e.Rev = kept, c.rev
		}
	}
}

// searched: a search the agent made on the web (on its call's line, if
// it has one).
func (c *Codex) searched(r *codexRow, id, query string) {
	query = strings.TrimSpace(query)
	if id == "" || c.seen[clip(id, maxID)] != nil || (query == "" && c.tools[clip(id, maxID)] == nil) {
		return
	}
	c.done(r, id, &Tool{Name: "WebSearch", Detail: clip(clean(query), maxDetail)}, "", false)
}

// mark keeps the line a thing told of on its own was given, so that it is
// the one line throughout. (Bounded: beyond that, what comes again is not
// matched to its line and is let be rather than shown twice.)
func (c *Codex) mark(id string, t *Tool) {
	if c.seen == nil {
		c.seen = map[string]*Tool{}
	}
	c.seen[id] = t // (bounded by the entries kept: trim forgets those it leaves out)
}

type codexItem struct {
	Type     string          `json:"type"`
	ID       string          `json:"id"`
	Content  json.RawMessage `json:"content"`
	Command  []string        `json:"command"`
	Cwd      string          `json:"cwd"`
	Parsed   []codexParsed   `json:"parsed_cmd"`
	Status   string          `json:"status"`
	Output   string          `json:"aggregated_output"`
	ExitCode *int            `json:"exit_code"`
	Query    string          `json:"query"`
	// FileChange, ImageView, McpToolCall, Extension
	Changes   map[string]codexChange `json:"changes"`
	Path      string                 `json:"path"`
	Server    string                 `json:"server"`
	Tool      string                 `json:"tool"`
	Arguments json.RawMessage        `json:"arguments"`
	Result    json.RawMessage        `json:"result"`
	Kind      string                 `json:"kind"`
	Action    *struct {
		Query   string   `json:"query"`
		Queries []string `json:"queries"`
		URL     string   `json:"url"`
	} `json:"action"`
}

type codexParsed struct {
	Cmd string `json:"cmd"`
}

type codexBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

func codexContent(raw json.RawMessage) []codexBlock {
	var bs []codexBlock
	if json.Unmarshal(raw, &bs) == nil {
		return bs
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return []codexBlock{{Type: "text", Text: s}}
	}
	return nil
}

func (c *Codex) item(r *codexRow, p *codexPayload) {
	switch p.Type {
	case "message":
		if p.Role != "assistant" {
			return // (the person's is read from its event; the rest is Codex's own)
		}
		var parts []Part
		for _, b := range codexContent(p.Content) {
			if t := strings.TrimSpace(b.Text); t != "" {
				parts = append(parts, textPart(KindText, b.Text))
			}
		}
		c.add(r, p.ID, RoleAssistant, parts)
	case "reasoning":
		var ts []string
		for _, s := range p.Summary {
			if t := strings.TrimSpace(s.Text); t != "" {
				ts = append(ts, t)
			}
		}
		if len(ts) > 0 {
			c.add(r, p.ID, RoleAssistant, []Part{textPart(KindThinking, strings.Join(ts, "\n\n"))})
		}
	case "custom_tool_call", "function_call":
		id := clip(p.CallID, maxID)
		var tools []*Tool
		var plan []Task
		if p.Type == "function_call" {
			tools, plan = codexFn(p.Namespace, p.Name, p.Arguments)
		} else {
			// (The code mode's script: what it runs is read out of it; each
			// thing it does is told of on its own as well, in a newer
			// Codex, and then its lines go.)
			tools = codexTools(p.Name, p.Input)
			for _, t := range tools {
				if t.Name != "request_user_input" {
					t.fromCall = true
				}
			}
		}
		var parts []Part
		room := maxParts
		for _, t := range tools {
			if strings.HasPrefix(t.Name, "request_user_input") {
				room = maxParts - 1 // (a question it puts keeps its place: what it waits on)
			}
		}
		for _, t := range tools {
			t.ID = id
			if t.fromCall && c.itemCmds {
				continue // (each of its things is told of on its own)
			}
			if len(parts) >= room && !strings.HasPrefix(t.Name, "request_user_input") {
				parts[len(parts)-1].Tool.MoreFiles++ // (of what one call did, as many lines as an entry holds)
				continue
			}
			parts = append(parts, Part{Kind: KindTool, Tool: t})
		}
		if len(parts) == 0 {
			c.hide(id) // (its things are told of on their own: what comes back of it is let be)
			return
		}
		c.add(r, p.ID, RoleAssistant, parts)
		kept := make([]*Tool, 0, len(parts))
		for _, pt := range parts {
			kept = append(kept, pt.Tool)
		}
		if id != "" {
			c.tools[id] = kept
		}
		if plan != nil && len(kept) > 0 {
			c.plans = append(c.plans, codexPlan{e: kept[0].at, t: kept[0], tasks: plan})
			if n := len(c.plans) - maxPlans; n > 0 {
				c.plans = append([]codexPlan(nil), c.plans[n:]...)
			}
		}
		for _, t := range kept {
			if strings.HasPrefix(t.Name, "request_user_input") {
				if len(c.asked) >= maxAsked {
					c.asked[0].Done = true // (the oldest goes, as over: the latest is to be answered)
					c.touch(c.asked[:1])
					c.asked = append([]*Tool(nil), c.asked[1:]...)
				}
				c.asked = append(c.asked, t)
			}
		}
		if out, ok := c.orphans[id]; ok {
			c.answer(kept, out)
			delete(c.orphans, id)
		}
	case "custom_tool_call_output", "function_call_output":
		var out codexOut
		out.text, out.failed, out.images = codexResult(p.Output)
		id := clip(p.CallID, maxID)
		if ts, ok := c.tools[id]; ok {
			c.answer(ts, out)
		} else if id != "" && !c.hidden[id] && len(c.orphans) < maxOrphans {
			if c.orphans == nil {
				c.orphans = map[string]codexOut{}
			}
			c.orphans[id] = out
		}
	}
}

// hide: a call whose line went (each thing it did told of on its own):
// what comes back of it is let be. (Bounded: a full set starts anew, and
// what comes back of one forgotten is kept as of a call not read.)
func (c *Codex) hide(id string) {
	if id == "" {
		return
	}
	if c.hidden == nil || len(c.hidden) >= maxHidden {
		c.hidden = map[string]bool{}
	}
	c.hidden[id] = true
	delete(c.tools, id)
}

// touch marks the entries of these calls as changed now (each line knows
// its entry: none is looked for).
func (c *Codex) touch(ts []*Tool) {
	for _, t := range ts {
		if t != nil && t.at != nil {
			t.at.Rev = c.rev
		}
	}
}

// answer: what came back of a call, on its first tool (a call's tools are
// its file edits, or its one command): the rest only done. A question put
// by the _async call is not answered by its being taken (its result,
// whatever it says): their next message answers it; one put by the other,
// by its result. A plan that failed is no list.
func (c *Codex) answer(ts []*Tool, out codexOut) {
	at := 0 // (its command's, if it ran one; else its first file's)
	for i, t := range ts {
		if t.Name == "Bash" {
			at = i
			break
		}
	}
	failed := out.failed || codexExit.MatchString(out.text)
	for i, t := range ts {
		if t.waits && !failed {
			continue // (taken, not answered: their next message answers it)
		}
		t.Done = true
		if i == at && t.Name != "request_user_input" {
			t.setResult(clean(out.text))
			t.ResultLines = lineCount(out.text)
			t.Images = out.images
		}
		t.Error = t.Error || failed // (as told when done: what came back does not undo it)
		if failed && t.Name == "TodoWrite" {
			for j := len(c.plans) - 1; j >= 0; j-- {
				if c.plans[j].t == t {
					c.plans = append(c.plans[:j:j], c.plans[j+1:]...)
					break
				}
			}
		}
	}
	c.touch(ts)
}

var (
	codexFailed = regexp.MustCompile(`^Script (failed|error)`)
	codexExit   = regexp.MustCompile(`(?m)^Process exited with code [1-9]`)
	codexHead   = regexp.MustCompile(`^Script (completed|failed)\nWall time [^\n]*\n(Output:\n)?`)
)

var (
	jsString   = `"((?:[^"\\]|\\.)*)"`
	execCmd    = regexp.MustCompile(`tools\.exec_command\(\{\s*"?cmd"?\s*:\s*` + jsString)
	patchStr   = regexp.MustCompile(`"\*\*\* Begin Patch(?:[^"\\]|\\.)*"`)
	patchTmpl  = regexp.MustCompile("`\\*\\*\\* Begin Patch(?:[^`\\\\]|\\\\.)*`")
	patchFile  = regexp.MustCompile(`(?m)^\*\*\* (Update|Add|Delete) File: (.+)$`)
	toolCalled = regexp.MustCompile(`tools\.([A-Za-z0-9_]+)\(`)
)

// jsTemplate reads a template literal without substitutions as its text.
func jsTemplate(lit string) (string, bool) {
	s := lit[1 : len(lit)-1]
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c == '$' && i+1 < len(s) && s[i+1] == '{':
			return "", false // (a substitution: not known here)
		case c == '\\' && i+1 < len(s):
			i++
			switch s[i] {
			case 'n':
				b.WriteByte('\n')
			case 't':
				b.WriteByte('\t')
			case 'r':
				b.WriteByte('\r')
			default:
				b.WriteByte(s[i]) // (\` \$ \\ \' \": the character)
			}
		default:
			b.WriteByte(c)
		}
	}
	return b.String(), true
}

// jsUnquote reads a JavaScript string literal in double quotes as its text
// (JSON's escapes, and \' for a quote).
func jsUnquote(lit string) (string, bool) {
	lit = strings.ReplaceAll(lit, `\'`, `'`)
	var s string
	if json.Unmarshal([]byte(lit), &s) != nil {
		return "", false
	}
	return s, true
}

// codexTools: the calls a tool call makes, as the view shows them. A
// script's commands are Bash; a patch is an Edit (or Write, a new file)
// for each file; anything else by its name.
func codexTools(name, src string) []*Tool {
	var out []*Tool
	// (apply_patch called on its own: its input the patch.)
	if name == "apply_patch" && strings.Contains(src, "*** Begin Patch") {
		if out = patchTools(src); len(out) > 0 {
			return out
		}
	}
	// A script that types to a running command (write_stdin: a password, a
	// key, however it comes by it) is read for nothing: one line, no text of
	// it. (Each thing it did is told of on its own, in a newer Codex.)
	if strings.Contains(src, "write_stdin") {
		return []*Tool{{Name: clip(name, 128)}}
	}
	// What is code in it (its strings' and comments' insides blanked): what
	// is read out of it is what stands there — a string the code itself
	// writes, a call it makes.
	code := jsCode(src)
	readable := func(at int) bool { // (a string's opening quote, in the code)
		return at >= 0 && at < len(code) && code[at] == src[at]
	}
	// (At most so many of each read: counted as read, not as found.)
	patches := 0
	for _, m := range patchStr.FindAllStringIndex(src, -1) {
		if patch, ok := jsUnquote(src[m[0]:m[1]]); ok && readable(m[0]) && patches < 8 {
			out = append(out, patchTools(patch)...)
			patches++
		}
	}
	for _, m := range patchTmpl.FindAllStringIndex(src, -1) {
		if patch, ok := jsTemplate(src[m[0]:m[1]]); ok && readable(m[0]) && patches < 8 {
			out = append(out, patchTools(patch)...)
			patches++
		}
	}
	// (Its commands, one line each, as one: what came back is of them all.)
	var cmds []string
	for _, m := range execCmd.FindAllStringSubmatchIndex(src, -1) {
		if len(cmds) >= 16 || !readable(m[0]) || !readable(m[2]-1) {
			continue // (a call written in a string or a comment)
		}
		if cmd, ok := jsUnquote(`"` + src[m[2]:m[3]] + `"`); ok {
			cmds = append(cmds, cmd)
		}
	}
	if len(cmds) > 0 {
		out = append(out, &Tool{Name: "Bash", Detail: clip(strings.Join(cmds, "\n"), maxDetail)})
	}
	// The rest of what it calls, by name (its arguments are code, not read
	// here: what each did is told of on its own, in a newer Codex). Not the
	// commands and patches read above, nor what it types to a command.
	seen := map[string]bool{}
	asks := scriptCalls(code, "request_user_input") // (called, wherever it is among however many calls)
	for _, m := range toolCalled.FindAllStringSubmatch(code, 64) {
		n := m[1]
		if seen[n] || (n == "exec_command" && len(cmds) > 0) || (n == "apply_patch" && len(out) > 0) || strings.HasPrefix(n, "request_user_input") {
			continue
		}
		seen[n] = true
		if t := scriptTool(n, src, code, readable); t != nil && len(out) < maxParts {
			out = append(out, t)
		}
	}
	// (It asks the person, last: what it waits on.)
	if asks {
		out = append(out, &Tool{Name: "request_user_input", waits: scriptCalls(code, "request_user_input_async")})
	}
	if len(out) > 0 {
		return out
	}
	// (Nothing it calls named: the script, none of it shown — it may hold
	// what it types to a command.)
	return []*Tool{{Name: clip(name, 128)}}
}

// scriptTool: a tool a script of the code mode calls, as the tool the view
// knows that does the same (its arguments not read: they are code).
func scriptTool(name, src, code string, readable func(int) bool) *Tool {
	switch name {
	case "exec_command":
		return &Tool{Name: "Bash"}
	case "write_stdin", "wait":
		return &Tool{Name: "BashOutput"}
	case "view_image":
		return &Tool{Name: "Read"}
	case "update_plan":
		return &Tool{Name: "TodoWrite"}
	case "web__run", "web_run":
		var qs []string
		if sp := callSpans(code, "tools."+name+"("); len(sp) > 0 {
			from, to := sp[0][0], sp[0][1]
			for _, m := range scriptQuery.FindAllStringSubmatchIndex(src[from:to], -1) {
				if len(qs) >= 8 || !readable(from+m[2]-1) {
					continue
				}
				if q, ok := jsUnquote(`"` + src[from+m[2]:from+m[3]] + `"`); ok && strings.TrimSpace(q) != "" {
					qs = append(qs, q)
				}
			}
		}
		return &Tool{Name: "WebSearch", Detail: clip(clean(strings.Join(qs, "\n")), maxDetail)}
	case "text", "image", "store", "load", "notify":
		return nil // (the script's own helpers: what it shows, keeps)
	}
	return &Tool{Name: clip(name, 128)}
}

// callSpans: where what a script's calls that begin so are given lies, in
// its code (strings and comments blanked: see jsCode): from after each
// one's "(" to the ")" that closes it (or the script's end).
func callSpans(code, call string) [][2]int {
	var out [][2]int
	for from := 0; len(out) < 64; {
		i := strings.Index(code[from:], call)
		if i < 0 {
			break
		}
		start, depth, end := from+i+len(call), 1, len(code)
		for j := start; j < len(code); j++ {
			if code[j] == '(' {
				depth++
			} else if code[j] == ')' {
				if depth--; depth == 0 {
					end = j
					break
				}
			}
		}
		out = append(out, [2]int{start, end})
		from = start
	}
	return out
}

// jsCode: a script as its code alone, the same length: what is inside its
// strings, template texts and regular expressions blanked ('_', their
// delimiters kept), its comments blanked (' '); a template's ${…} is code,
// blanked so in turn (to maxTplDepth deep: beyond, all of it as text).
func jsCode(src string) string {
	b := []byte(src)
	jsCodeFrom(b, 0, 0)
	return string(b)
}

const maxTplDepth = 32

// jsLineEnd: b[i] ends a line (LF, CR, U+2028, U+2029).
func jsLineEnd(b []byte, i int) bool {
	return b[i] == '\n' || b[i] == '\r' || (b[i] == 0xe2 && i+2 < len(b) && b[i+1] == 0x80 && (b[i+2] == 0xa8 || b[i+2] == 0xa9))
}

// jsRegexHere: a "/" at i begins a regular expression (not a division):
// what comes before it is no value.
func jsRegexHere(b []byte, i int) bool {
	j := i - 1
	for j >= 0 && (b[j] == ' ' || b[j] == '\t' || b[j] == '\n' || b[j] == '\r') {
		j--
	}
	if j < 0 {
		return true
	}
	if j > 0 && (b[j] == '+' || b[j] == '-') && b[j-1] == b[j] {
		return false // (x++ / 2)
	}
	if strings.IndexByte("(,=:[!&|?{};+-*%<>~^", b[j]) >= 0 {
		return true
	}
	for _, w := range []string{"return", "typeof", "case", "do", "else", "in", "of", "void", "await", "yield"} {
		k := j + 1 - len(w)
		if k >= 0 && string(b[k:j+1]) == w && (k == 0 || !jsWordByte(b[k-1])) {
			return true
		}
	}
	return false
}

func jsWordByte(c byte) bool {
	return c == '_' || c == '$' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= 0x80
}

// jsCodeFrom blanks from i on; in a template's ${…} (depth > 0), up to the
// "}" that closes it, whose index it returns (else len(b)).
func jsCodeFrom(b []byte, i, depth int) int {
	braces := 0
	for ; i < len(b); i++ {
		switch ch := b[i]; {
		case ch == '/' && i+1 < len(b) && b[i+1] == '/':
			for ; i < len(b) && !jsLineEnd(b, i); i++ {
				b[i] = ' '
			}
		case ch == '/' && i+1 < len(b) && b[i+1] == '*':
			for ; i < len(b) && !(b[i] == '*' && i+1 < len(b) && b[i+1] == '/'); i++ {
				if b[i] != '\n' {
					b[i] = ' '
				}
			}
			if i+1 < len(b) {
				b[i], b[i+1] = ' ', ' '
				i++
			}
		case ch == '/' && jsRegexHere(b, i):
			class := false
			for i++; i < len(b) && !jsLineEnd(b, i) && (class || b[i] != '/'); i++ {
				switch {
				case b[i] == '\\' && i+1 < len(b):
					b[i] = '_'
					i++
				case b[i] == '[':
					class = true
				case b[i] == ']':
					class = false
				}
				b[i] = '_'
			}
		case ch == '"' || ch == '\'':
			for i++; i < len(b) && b[i] != ch && b[i] != '\n' && b[i] != '\r'; i++ { // (U+2028/9 may stand in a string)
				if b[i] == '\\' && i+1 < len(b) {
					b[i] = '_'
					i++
					if b[i] == '\r' && i+1 < len(b) && b[i+1] == '\n' { // (a line continued: CRLF)
						b[i] = '_'
						i++
					}
				}
				b[i] = '_'
			}
		case ch == '`':
			for i++; i < len(b) && b[i] != '`'; i++ {
				switch {
				case b[i] == '\\' && i+1 < len(b):
					b[i], b[i+1] = '_', '_'
					i++
				case b[i] == '$' && i+1 < len(b) && b[i+1] == '{' && depth >= maxTplDepth:
					for ; i < len(b); i++ { // (nested past reason: all the rest as text)
						if b[i] != '\n' {
							b[i] = '_'
						}
					}
					return len(b)
				case b[i] == '$' && i+1 < len(b) && b[i+1] == '{':
					i = jsCodeFrom(b, i+2, depth+1) // (its code, to its "}")
				default:
					b[i] = '_'
				}
			}
		case depth > 0 && ch == '{':
			braces++
		case depth > 0 && ch == '}':
			if braces == 0 {
				return i
			}
			braces--
		}
	}
	return len(b)
}

// scriptCalls: a script's code calls tools.<name> (or <name>_async):
// the name, then — past any space of JavaScript's, an optional "?." — its
// "(" (not only names it).
func scriptCalls(code, name string) bool {
	call := "tools." + name
	for from := 0; from < len(code); {
		i := strings.Index(code[from:], call)
		if i < 0 {
			return false
		}
		at := from + i
		j := at + len(call)
		from = j
		if at > 0 && (jsWordByte(code[at-1]) || code[at-1] == '.') {
			continue // (another's: x.tools.…, mytools.…)
		}
		if strings.HasPrefix(code[j:], "_async") {
			j += len("_async")
		}
		if r, _ := utf8.DecodeRuneInString(code[j:]); r == '_' || r == '$' || unicode.IsLetter(r) || unicode.IsDigit(r) {
			continue // (another name: request_user_input_other)
		}
		rest := strings.TrimLeftFunc(code[j:], func(r rune) bool { return unicode.IsSpace(r) || r == '\u2028' || r == '\u2029' || r == '\ufeff' })
		if strings.HasPrefix(strings.TrimPrefix(rest, "?."), "(") {
			return true
		}
	}
	return false
}

// scriptQuery: a search's words in a call's arguments ({q: "…"}), found
// in its code, read from the script.
var scriptQuery = regexp.MustCompile(`\bq"?\s*:\s*"((?:[^"\\]|\\.)*)"`)

// patchTools: an apply_patch's files, each with its lines added and
// removed and its hunks.
func patchTools(patch string) []*Tool {
	var out []*Tool
	var t *Tool
	var h *Hunk
	size := 0
	for _, l := range strings.Split(patch, "\n") {
		if m := patchFile.FindStringSubmatch(l); m != nil {
			if len(out) >= maxFiles {
				out[len(out)-1].MoreFiles++
				t, h = nil, nil
				continue
			}
			name := "Edit"
			if m[1] == "Add" {
				name = "Write"
			}
			t = &Tool{Name: name, Detail: clip(strings.TrimSpace(m[2]), maxDetail)}
			if m[1] == "Delete" {
				t.Title = "delete"
			}
			out, h = append(out, t), nil
			continue
		}
		if t != nil && strings.HasPrefix(l, "*** Move to: ") {
			t.Title = clip(t.Detail+" → "+strings.TrimSpace(strings.TrimPrefix(l, "*** Move to: ")), maxTitle)
			continue
		}
		if t == nil || strings.HasPrefix(l, "*** ") {
			continue
		}
		if strings.HasPrefix(l, "@@") {
			if len(t.Patch) >= maxHunks {
				t.PatchCut, h = true, nil
				continue
			}
			t.Patch = append(t.Patch, Hunk{})
			h = &t.Patch[len(t.Patch)-1]
			continue
		}
		if l == "" || (l[0] != '+' && l[0] != '-' && l[0] != ' ') {
			continue
		}
		switch l[0] {
		case '+':
			t.Add++
		case '-':
			t.Del++
		}
		if h == nil {
			if len(t.Patch) >= maxHunks {
				t.PatchCut = true
				continue
			}
			t.Patch = append(t.Patch, Hunk{})
			h = &t.Patch[len(t.Patch)-1]
		}
		if size+len(l) > maxPatch {
			t.PatchCut = true
			continue
		}
		size += len(l)
		h.Lines = append(h.Lines, clip(l, maxPatchLine))
	}
	// (A hunk none of whose lines were kept is none: the view draws lines.)
	for _, t := range out {
		kept := t.Patch[:0]
		for _, h := range t.Patch {
			if len(h.Lines) > 0 {
				kept = append(kept, h)
			}
		}
		t.Patch = kept
	}
	return out
}

// maxHunks: the hunks of one file's patch kept (its lines are bounded by
// maxPatch; this, how many it is cut into).
const maxHunks = 200

func (c *Codex) add(r *codexRow, id, role string, parts []Part) {
	if len(parts) == 0 {
		return
	}
	if len(parts) > maxParts {
		parts = parts[:maxParts]
	}
	c.seq++
	if id == "" {
		id = "c" + strconv.Itoa(c.seq)
	}
	e := &Entry{Seq: c.seq, Rev: c.rev, ID: clip(id, maxID), Role: role, Time: clip(r.Timestamp, 64), Parts: parts}
	for _, p := range parts {
		if p.Tool != nil {
			p.Tool.at = e
		}
	}
	c.entries = append(c.entries, e)
	if c.turnOpen {
		c.turnOpen = false
		c.turns = append(c.turns, c.entries[len(c.entries)-1])
		if n := len(c.turns) - maxTurns; n > 0 {
			c.turns = append([]*Entry(nil), c.turns[n:]...)
		}
	}
}

// backtrack takes back the last n turns not taken back. Where fewer began
// in what was read, all of it goes, and the rest are of the part before
// (the entries before the first turn here are of its last turn).
func (c *Codex) backtrack(n int) {
	if n <= 0 {
		return
	}
	if n <= len(c.turns) {
		from := c.turns[len(c.turns)-n].Seq
		c.turns = c.turns[:len(c.turns)-n]
		c.takeBack(c.entries, from)
		return
	}
	c.backPending = min(c.backPending+n-len(c.turns), maxTurns)
	c.turns = nil
	c.takeBack(c.entries, minSeq)
}

// mergeCall: what only the call said of a thing told of when done, put on
// the line the telling made (it read in another part): what it was for,
// its command or file as the call named it, and what came back of it if
// the telling had nothing — as reading both together would have shown.
// Each of the line's files is matched to the call's of the same path; one
// of the call's the telling did not name is put with them. The entry
// changed, if any.
func mergeCall(ct *Tool, calls []*Tool) *Entry {
	e := ct.at
	if e == nil {
		return nil
	}
	var lines []*Tool
	for _, p := range e.Parts {
		if p.Tool != nil && p.Tool.ID == ct.ID {
			lines = append(lines, p.Tool)
		}
	}
	used := map[*Tool]bool{}
	for _, l := range lines {
		var call *Tool
		for _, x := range calls {
			if !used[x] && x.Name == l.Name && (x.Detail == l.Detail || (x.Detail != "" && strings.HasSuffix(l.Detail, "/"+x.Detail))) {
				call = x
				break
			}
		}
		if call == nil && len(calls) == 1 && len(lines) == 1 {
			call = calls[0]
		}
		if call == nil {
			continue
		}
		used[call] = true
		if call.Detail != "" {
			l.Detail = call.Detail
		}
		if l.Title == "" || !strings.Contains(l.Title, " → ") {
			l.Title = firstNonEmpty(call.Title, l.Title)
		}
		if l.Result == "" && call.Result != "" {
			l.Result, l.ResultLines, l.Truncated, l.ResultTail, l.Omitted = call.Result, call.ResultLines, call.Truncated, call.ResultTail, call.Omitted
		}
		if l.Images == 0 {
			l.Images = call.Images
		}
		l.Error = l.Error || call.Error
		if l.Stdin == nil {
			l.Stdin = call.Stdin
		}
	}
	for _, x := range calls {
		if !used[x] && len(e.Parts) < maxParts {
			x.at, x.Done, x.Error = e, x.Done || ct.Done && !x.waits, x.Error || ct.Error && !x.waits
			e.Parts = append(e.Parts, Part{Kind: KindTool, Tool: x})
		}
	}
	return e
}

// dropToolIDs: the entries without the lines of these ids (one with none
// left goes).
func dropToolIDs(entries []*Entry, drop map[string]bool) []*Entry {
	out := entries[:0]
	for _, e := range entries {
		kept := e.Parts[:0]
		for _, p := range e.Parts {
			if p.Tool != nil && p.Tool.ID != "" && drop[p.Tool.ID] {
				continue
			}
			kept = append(kept, p)
		}
		e.Parts = kept
		if len(kept) > 0 {
			out = append(out, e)
		}
	}
	return out
}

// minSeq is below any Seq (they go below 0 as parts are put before).
const minSeq = -1 << 62

// takeBack marks gone, as of this revision, the entries from Seq from on.
func (c *Codex) takeBack(entries []*Entry, from int) {
	for _, e := range entries {
		if e.Seq >= from && !e.Gone {
			e.Gone, e.Rev = true, c.rev
		}
	}
}

// goneFrom marks gone the entries from Seq from on (their Rev is the
// caller's to set: Prepend's, as it puts them before).
func goneFrom(entries []*Entry, from int) {
	for _, e := range entries {
		if e.Seq >= from {
			e.Gone = true
		}
	}
}
