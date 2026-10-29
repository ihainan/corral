package convo

import (
	"bytes"
	"encoding/json"
	"html"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Claude reads Claude Code's session log, ~/.claude/projects/<folder>/<session>.jsonl:
// one JSON object per line, appended as the session goes. The rows that
// matter:
//
//   - "user" rows with string content are what the person typed, unless
//     marked isMeta (text Claude Code adds for the model), or one of the
//     envelopes it writes for slash commands, shell escapes and background
//     tasks; with promptSource "system" Claude Code sent it on its own.
//     isCompactSummary marks the summary written when the session was
//     compacted.
//   - "user" rows with list content mostly carry tool results, which belong
//     to the tool call (by tool_use_id) in an earlier assistant row; text and
//     image blocks there are the person's (a message with a picture).
//   - "assistant" rows carry text, thinking and tool_use blocks, usually one
//     block a row.
//   - "attachment" rows of type queued_command are messages typed while the
//     agent worked, which it took in during its turn (since 2.1.291 these
//     are not written as user rows). "queue-operation" rows say what waits
//     meanwhile: enqueue (with the text); the rest say less than what went
//     (dequeue names nothing, and takes what Claude Code puts first, which
//     is not always what came first), so a message stops waiting when it is
//     seen arrive (a person's row with its text), when removed by its text,
//     or put back in the draft (popOne: the latest; popAll, clear).
//   - isSidechain rows are a subagent's own conversation: not shown.
//   - "ai-title" rows hold the title Claude Code gave the session (written
//     again and again), "custom-title" rows the one the person gave it.
//   - A tool result's row also has Claude Code's own account of the call
//     (toolUseResult): the edit it made, the task it created.
//   - What the session runs with (see Meta): the model and the context's
//     size from the latest assistant row, the permission mode from
//     "permission-mode" rows and the person's rows, the branch from any row.
//
// Everything else (file snapshots, API bookkeeping) is skipped.
// Feed it whole lines in order.
type Claude struct {
	sub     bool // (a subagent's own log: its side-chain rows are its own)
	entries []*Entry
	tools   map[string]toolRef
	rev     int
	seq     int
	aiTitle string
	myTitle string     // (the person's, over Claude Code's)
	tasks   []taskItem // the task list as it stands
	meta    Meta
	queue   []queuedItem // what waits to be taken in, oldest first
	// running: work set going and left to itself (a command in the
	// background, a subagent on its own), until it is told to have ended;
	// runOrder keeps them as they were set going.
	running  map[string]Running
	runOrder []string
	// ranOut: told to have ended before what set it going was read (the
	// log read from its middle, or a notice that came first).
	ranOut map[string]bool
	// wrote: the files the agent itself wrote, a shell command's counted.
	// Claude Code says a file changed whenever its modification time did,
	// which is mostly the agent's own doing: only a change it did not
	// make is news (see attached). said: the person's last message, read
	// to tell the files they referred to from the ones a compaction
	// brought back by itself.
	wrote map[string]bool
	said  string
	// notedLast: the file the latest note of them named last, so that one
	// reported twice running is named once even past maxNoted.
	notedLast string
	gone      []goneItem // removed from it, to arrive still (as queued_command): that arrival is of these
	since     int64      // (unix milliseconds) what was queued before was to another process: not kept
	// Skipped counts lines that were not JSON or of no use to the view.
	Skipped int
	orphans map[string]orphan // results whose call is not in what was read (see Prepend)
	trimmed bool              // entries were dropped as old
	// What the part read said itself (a part before it, put before, says
	// only what this one did not): a custom title (even cleared), the whole
	// task list (TodoWrite). The changes made to the list since the last
	// whole one (one task added, one updated) are kept, to be made again: over
	// the list a part before gives, until a whole one is said; over that one
	// (taskBase) once a part before answers a call whose result came after it.
	myTitleSaid, tasksSaid bool
	branchSaid             bool // (a row said the branch, if only that there is none)
	ctxSaid                bool // (the context was said or, by a compaction, made unknown: the part before has no say)
	// lastReq: the model the conversation's latest request went to, and
	// whether it asked for the 1M window (the context-1m beta): read from
	// Claude Code's api-request rows, for Meta's window.
	lastReq struct {
		model string // (as claudeKey has it)
		wide  bool
	}
	taskBase    []taskItem
	taskOps     []taskOp
	taskOpsFull bool              // more were made than are kept: the list is not made again (see Prepend)
	late        map[string]taskOp // (while Prepend pairs results: their changes, to be made in their turn)
	tree        rowTree           // what each row hangs on, to tell what a rewind took back (see rewound)
}

// taskOp is a change made to the task list; or (orphan: the result's call
// id), the place of one its call, not read yet, may make, made there once
// the call is read (see Prepend).
type taskOp struct {
	f      func(c *Claude)
	whole  bool // (TodoWrite: the whole list)
	orphan string
}

// maxTaskOps is how many changes to the task list are kept to be made again;
// maxTaskPlaces how many places of results whose call is not read yet.
const (
	maxTaskOps    = 500
	maxTaskPlaces = 4 * maxOrphans
)

type orphan struct {
	b       claudeBlock
	account json.RawMessage
}

type toolRef struct {
	e *Entry
	t *Tool
	// task changes the task list, once the call has come back without an
	// error (TodoWrite, TaskUpdate; TaskCreate from its result).
	task func(c *Claude)
}

type goneItem struct {
	text string // (squashed)
	at   string // when it was removed
	from string // another session's, which: its arrival is told by that too
}

type queuedItem struct {
	from string
	note bool
	raw  string // as the log wrote it (a remove names it so)
	Queued
}

type taskItem struct {
	id string // (TaskCreate's; none for TodoWrite's)
	Task
}

// maxEntries is how many entries are kept; older ones are dropped (and,
// once some are, no earlier part is put before them: see Prepend).
const maxEntries = 6000

// maxTaskIDs is how many task ids one notice is read for: markers and ones
// already over are among them, so it is not the count of what may be
// taken off the list (maxRunning).
const maxTaskIDs = 8 * maxRunning

// maxOrphans is how many results of calls not seen are kept, for a part of
// the log read later from before (see Prepend): those at the start of the
// part read first.
const maxOrphans = 256

func NewClaude() *Claude {
	return &Claude{tools: map[string]toolRef{}, running: map[string]Running{}, ranOut: map[string]bool{}}
}

// NewClaudeSub reads a subagent's own log (subagents/agent-<id>.jsonl):
// all of its rows are of its side chain, which a session's log leaves out.
func NewClaudeSub() *Claude {
	c := NewClaude()
	c.sub = true
	return c
}

// Rev is the revision after the last Feed.
func (c *Claude) Rev() int { return c.rev }

// Title is the session's title: the one the person gave it, else Claude
// Code's; "" when the log read has none.
func (c *Claude) Title() string {
	if c.myTitle != "" {
		return c.myTitle
	}
	return c.aiTitle
}

// EarlierTitle takes the name the person gave the session in a part of its
// log before what was read (its "custom-title" row), unless what was read
// gives one: that is later, as with Prepend.
func (c *Claude) EarlierTitle(line []byte) {
	if c.myTitleSaid {
		return
	}
	var r struct {
		Type        string `json:"type"`
		CustomTitle string `json:"customTitle"`
	}
	if json.Unmarshal(line, &r) != nil || r.Type != "custom-title" {
		return
	}
	c.myTitle, c.myTitleSaid = strings.TrimSpace(clip(r.CustomTitle, maxTitle)), true
}

// Meta is what the session runs with, as the log read last said it.
func (c *Claude) Meta() Meta {
	m := c.meta
	wide := c.lastReq.model != "" && c.lastReq.model == claudeKey(m.Model) && c.lastReq.wide
	m.Window, m.WindowBy = claudeWindow(m.Model, wide)
	return m
}

// claudeWindow: the context window of a Claude model (by its id, or the
// name /model said it switched to), which Claude Code's log does not say:
// 1M where its requests asked for it (wide: the context-1m beta; the [1m]
// Claude Code puts after an id, "(1M context)" after a name), else the
// model's own. Every model from Opus and Sonnet 4.6 on has 1M (Haiku from
// 5, Fable and Mythos all), older ones 200K; one not known, nothing.
func claudeWindow(model string, wide bool) (int64, string) {
	low := strings.ToLower(strings.TrimSpace(model))
	if wide || strings.HasSuffix(low, "[1m]") || strings.HasSuffix(low, "(1m context)") {
		return 1_000_000, "request"
	}
	key := claudeKey(model)
	if claudeOld.MatchString(key) {
		return 200_000, "model"
	}
	m := claudeModelID.FindStringSubmatch(key)
	if m == nil {
		return 0, ""
	}
	family := m[1] + m[4] + m[7]
	major, _ := strconv.Atoi(m[2] + m[5])
	minor, _ := strconv.Atoi(m[3] + m[6])
	switch {
	case family == "fable" || family == "mythos":
	case family == "haiku" && major < 5:
		return 200_000, "model"
	case (family == "opus" || family == "sonnet") && (major < 4 || major == 4 && minor < 6):
		return 200_000, "model"
	}
	return 1_000_000, "model"
}

// claudeKey: a Claude model's id as one, however it is written: without a
// platform's prefix or suffix (Bedrock's anthropic. and us./eu./global.,
// -v1:0; Vertex's @date), a date, or what Claude Code puts after it ([1m],
// " (1M context)"): what a request named and what its answer did are then
// the same model. (A name as /model says it, lowered.)
func claudeKey(model string) string {
	k := strings.ToLower(strings.TrimSpace(model))
	k = claudeAfter.ReplaceAllString(k, "")
	k = claudePlatform.ReplaceAllString(k, "")
	return claudeDated.ReplaceAllString(k, "")
}

// (claude-opus-5-5, claude-mythos-preview; "opus 5.5", "mythos preview" as
// /model says them. claudeOld: the 3.x ids, family last.)
var (
	claudeModelID  = regexp.MustCompile(`^(?:claude-(opus|sonnet|haiku|fable|mythos)-(\d+)(?:-(\d{1,2}))?|(opus|sonnet|haiku|fable|mythos) (\d+)(?:\.(\d+))?)$|^(?:claude-|)(fable|mythos)\b`)
	claudeOld      = regexp.MustCompile(`^claude-3(?:-[57])?-(?:opus|sonnet|haiku)`)
	claudeAfter    = regexp.MustCompile(`(?:\[[^\]]*\]| \([^)]*\))$`)
	claudePlatform = regexp.MustCompile(`^(?:[a-z]{2,6}\.)?anthropic\.`)
	claudeDated    = regexp.MustCompile(`(?:-\d{8})?(?:-v\d+(?::\d+)?)?(?:@\d{8})?$`)
)

// Queued is what the person sent that waits to be taken in, oldest first.
// (Of the log read: what was queued before the part read is not known.)
// Running is what the agent set going and has not been told has ended.
func (c *Claude) Running() []Running {
	out := make([]Running, 0, len(c.running))
	for _, id := range c.runOrder {
		if r, ok := c.running[id]; ok {
			out = append(out, r)
		}
	}
	return out
}

func (c *Claude) Queued() []Queued {
	var out []Queued
	for _, q := range c.queue {
		x := q.Queued
		x.From, x.Note = q.from, q.note
		out = append(out, x)
	}
	return out
}

// ClearQueue forgets what waits.
func (c *Claude) ClearQueue() { c.queue, c.gone = nil, nil }

// QueueSince says when the process the log is read for started (unix
// milliseconds; 0 when not known): what was queued before was to another process
// of the same session (one gone, and resumed) and went with it, so it is
// dropped, and not kept when read.
func (c *Claude) QueueSince(started int64) {
	if started == c.since {
		return
	}
	c.since = started
	// (Removals of the process before: their arrivals are not this one's.)
	gone := c.gone[:0:0]
	for _, g := range c.gone {
		if c.ofNow(g.at) {
			gone = append(gone, g)
		}
	}
	c.gone = gone
	keep := c.queue[:0:0]
	for _, q := range c.queue {
		if c.ofNow(q.Time) {
			keep = append(keep, q)
		}
	}
	c.queue = keep
}

func (c *Claude) ofNow(ts string) bool {
	if c.since <= 0 {
		return true
	}
	at, err := time.Parse(time.RFC3339Nano, ts)
	return err == nil && at.UnixMilli() >= c.since
}

// Tasks is the agent's task list as the log read has it (a task created
// before the part read is not known). Nil when there is none.
func (c *Claude) Tasks() []Task {
	if len(c.tasks) == 0 {
		return nil
	}
	out := make([]Task, len(c.tasks))
	for i, t := range c.tasks {
		out[i] = t.Task
	}
	return out
}

// Feed reads a batch of whole lines (each ending in "\n"; a last line
// without one is ignored). Entries it adds or changes get the next revision.
func (c *Claude) Feed(lines []byte) {
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

// Since copies the entries changed after revision rev, in order.
func (c *Claude) Since(rev int) []Entry { return since(c.entries, rev) }

// since copies the entries changed after revision rev, in order.
func since(entries []*Entry, rev int) []Entry {
	out := []Entry{}
	for _, e := range entries {
		if e.Rev > rev {
			cp := *e
			cp.Parts = make([]Part, len(e.Parts))
			for i, p := range e.Parts {
				cp.Parts[i] = p
				if p.Tool != nil {
					t := *p.Tool
					cp.Parts[i].Tool = &t
				}
				if p.Noted != nil {
					n := *p.Noted
					n.Paths = append([]string(nil), p.Noted.Paths...)
					cp.Parts[i].Noted = &n
				}
			}
			out = append(out, cp)
		}
	}
	return out
}

// taskSaid notes a change made to the task list: all of it (whole), or one
// change kept to be made again (see taskOps, taskBase).
// Past maxTaskOps, the changes before are not all known any more: none are.
func (c *Claude) taskSaid(op taskOp) {
	if op.whole {
		c.tasksSaid, c.taskOps, c.taskOpsFull = true, nil, false
		c.taskBase = append([]taskItem(nil), c.tasks...)
		return
	}
	if c.taskOpsFull {
		return
	}
	// (A place for a result (op.orphan) counts apart: most are of other tools.)
	n := 0
	for _, o := range c.taskOps {
		if (o.orphan == "") == (op.orphan == "") {
			n++
		}
	}
	if (op.orphan == "" && n >= maxTaskOps) || (op.orphan != "" && n >= maxTaskPlaces) {
		c.taskOps, c.taskOpsFull = nil, true
		return
	}
	c.taskOps = append(c.taskOps, op)
}

// Trimmed says entries were dropped as old: nothing more is put before.
func (c *Claude) Trimmed() bool { return c.trimmed }

// Prepend puts before c's entries those of o, a parser fed the part of the
// log just before the part c was fed: they take Seqs below c's first (as
// low as it takes: they may be negative) and this revision, so a reader
// of Since gets them as changed. A call of o answered in c's part gets its
// result (kept by c, see maxOrphans); one not answered yet is c's to
// answer, as the log goes on. o's own results of calls further back are
// kept for the part before it. Past maxEntries, the oldest of o are left
// out, and Trimmed says so. Of o's state, what c's part did not say itself
// (a title, a custom one, the task list) is taken: the rest is c's later word.
func (c *Claude) Prepend(other Parser) {
	o, ok := other.(*Claude)
	if !ok {
		return
	}
	// (The changes to the task list those results make are made where c
	// read them, among its own: below.)
	late := map[string]taskOp{}
	o.late = late
	for id, r := range c.orphans {
		if _, ok := o.tools[id]; ok {
			o.result(r.b, r.account)
			delete(c.orphans, id)
		}
	}
	o.late = nil
	for id, r := range o.orphans {
		if c.orphans == nil {
			c.orphans = map[string]orphan{}
		}
		if len(c.orphans) < maxOrphans {
			c.orphans[id] = r
		}
	}
	// What the part before set going and was never told had ended: its own,
	// unless what was read since says it has ended (or set it going again).
	// What the part before was told had ended, whose start is older still:
	// kept, so that reading that start later does not set it going again.
	for id := range o.ranOut {
		if _, there := o.running[id]; !there {
			c.endRun(id)
		}
	}
	for _, id := range o.runOrder {
		r, there := o.running[id]
		if !there || c.ranOut[id] {
			delete(c.ranOut, id)
			continue
		}
		if _, 已 := c.running[id]; 已 || len(c.running) >= maxRunning {
			continue
		}
		c.running[id] = r
		c.runOrder = append([]string{id}, c.runOrder...)
	}
	// What the part before wrote is the agent's own doing as much (see
	// attached). What it says of files already read stands: an attachment
	// read then was answered by what was known then.
	for p := range o.wrote {
		c.wroteFile(p)
	}
	if len(c.entries) == 0 {
		c.notedLast = o.notedLast // (its last note is the one to go on from)
	}
	if c.aiTitle == "" {
		c.aiTitle = o.aiTitle
	}
	if !c.myTitleSaid {
		c.myTitle, c.myTitleSaid = o.myTitle, o.myTitleSaid
	}
	answered := false
	for _, op := range c.taskOps {
		if _, ok := late[op.orphan]; ok && op.orphan != "" {
			answered = true
			break
		}
	}
	if !c.taskOpsFull && (!c.tasksSaid || answered) {
		// (Over the list before, or the whole one c's part said, the changes
		// made since, in order.)
		ops := c.taskOps
		if c.tasksSaid {
			c.tasks, c.taskOps = append([]taskItem(nil), c.taskBase...), nil
		} else {
			c.tasks, c.tasksSaid, c.taskBase, c.taskOps, c.taskOpsFull = o.tasks, o.tasksSaid, o.taskBase, o.taskOps, o.taskOpsFull
		}
		for _, op := range ops {
			if l, ok := late[op.orphan]; ok && op.orphan != "" {
				op = l
			}
			if op.f != nil {
				op.f(c)
			}
			c.taskSaid(op)
		}
	}
	// (What the session ran with, as the part before last said it, where
	// c's part did not say.)
	if c.meta.Model == "" {
		c.meta.Model = o.meta.Model
	}
	if c.meta.Mode == "" {
		c.meta.Mode = o.meta.Mode
	}
	if c.meta.Effort == "" {
		c.meta.Effort = o.meta.Effort
	}
	if !c.branchSaid {
		c.meta.Branch, c.branchSaid = o.meta.Branch, o.branchSaid
	}
	if !c.ctxSaid {
		c.meta.Context, c.ctxSaid = o.meta.Context, o.ctxSaid
	}
	if c.meta.Cost == nil {
		c.meta.Cost = o.meta.Cost // (running totals: the later stands, never added to)
	}
	if c.lastReq.model == "" {
		c.lastReq = o.lastReq
	}
	// (What c's rewinds into o's part took back, there and here.)
	mine := c.treeBefore(o)
	add := o.entries
	if room := max(0, maxEntries-len(c.entries)); len(add) > room {
		add = add[len(add)-room:]
		c.trimmed = true
	}
	// o's calls still to be answered: of the entries put before (and, as in
	// trim, those that change the task list, whose entry was left out).
	kept := make(map[*Entry]bool, len(add))
	for _, e := range add {
		kept[e] = true
	}
	if c.tools == nil {
		c.tools = map[string]toolRef{}
	}
	for id, ref := range o.tools {
		if _, ok := c.tools[id]; !ok && (kept[ref.e] || ref.task != nil || ref.t.Name == "TaskCreate") {
			c.tools[id] = ref
		}
	}
	if len(add) > 0 || len(mine) > 0 {
		c.rev++
	}
	for _, e := range mine {
		e.Rev = c.rev
	}
	if len(add) == 0 {
		return
	}
	first := c.seq + 1
	if len(c.entries) > 0 {
		first = c.entries[0].Seq
	}
	for i, e := range add {
		e.Seq = first - len(add) + i
		e.Rev = c.rev
	}
	c.entries = append(append([]*Entry(nil), add...), c.entries...)
}

// First is the Seq of the oldest entry kept (0 when there are none).
func (c *Claude) First() int {
	if len(c.entries) == 0 {
		return 0
	}
	return c.entries[0].Seq
}

func (c *Claude) trim() {
	if n := len(c.entries) - maxEntries; n > 0 {
		c.trimmed = true
		for _, e := range c.entries[:n] {
			for _, p := range e.Parts {
				// (A log that reuses an id has the map point at the later call.
				// One that changes the task list stays, to be answered: the
				// list is not the entries'.)
				if p.Tool == nil {
					continue
				}
				if ref, ok := c.tools[p.Tool.ID]; ok && ref.t == p.Tool && ref.task == nil && p.Tool.Name != "TaskCreate" {
					delete(c.tools, p.Tool.ID)
				}
			}
		}
		c.entries = append([]*Entry(nil), c.entries[n:]...)
	}
}

type claudeRow struct {
	Type             string          `json:"type"`
	UUID             string          `json:"uuid"`
	ParentUUID       json.RawMessage `json:"parentUuid"`        // (null on a row hanging on none; absent in logs that keep no tree)
	LogicalParent    string          `json:"logicalParentUuid"` // (across a compaction, where parentUuid is empty)
	Retracted        []string        `json:"retractedMessageUuids"`
	Timestamp        string          `json:"timestamp"`
	IsMeta           bool            `json:"isMeta"`
	IsSidechain      bool            `json:"isSidechain"`
	IsCompactSummary bool            `json:"isCompactSummary"`
	PromptSource     string          `json:"promptSource"`
	AITitle          string          `json:"aiTitle"`
	CustomTitle      string          `json:"customTitle"`
	ToolUseResult    json.RawMessage `json:"toolUseResult"`
	Subtype          string          `json:"subtype"`
	Operation        string          `json:"operation"` // (queue-operation rows)
	Content          json.RawMessage `json:"content"`   // (system rows)
	GitBranch        *string         `json:"gitBranch"` // (present and empty: not on a branch)
	PermissionMode   string          `json:"permissionMode"`
	Effort           string          `json:"effort"` // (assistant rows: how hard it was asked to think)
	// (api-request rows: what a request to the model asked for.)
	QuerySource string `json:"querySource"`
	Params      *struct {
		Model string          `json:"model"`
		Betas json.RawMessage `json:"betas"` // (looked through, not read into a list: it may be long)
	} `json:"params"`
	// What went wrong, on the rows that say so (see trouble).
	// (system rows of a compaction, a scheduled task's turn, a retry: each
	// read on its own, so one of an unlooked-for kind costs only itself)
	CompactMetadata json.RawMessage `json:"compactMetadata"`
	Prompt          json.RawMessage `json:"prompt"`
	TaskKind        json.RawMessage `json:"taskKind"`
	TurnOrigin      json.RawMessage `json:"turnOrigin"`
	// (an assistant row Claude Code wrote of an error: the plan's usage limit reached)
	IsAPIErrorMessage bool            `json:"isApiErrorMessage"`
	Level             json.RawMessage `json:"level"` // (an informational's: notice, warning)
	QuotaLimits       json.RawMessage `json:"quotaLimits"`
	RetryAttempt      json.RawMessage `json:"retryAttempt"`
	MaxRetries        json.RawMessage `json:"maxRetries"`
	// (cost-state rows: what the session used so far)
	TotalCostUSD        float64         `json:"totalCostUSD"`
	TotalAPIDuration    int64           `json:"totalAPIDuration"`
	TotalLinesAdded     int64           `json:"totalLinesAdded"`
	TotalLinesRemoved   int64           `json:"totalLinesRemoved"`
	TotalDuration       int64           `json:"totalDuration"`
	StartTime           int64           `json:"startTime"`
	ModelUsage          json.RawMessage `json:"modelUsage"`
	HasUnknownModelCost bool            `json:"hasUnknownModelCost"`
	RetryInMs           json.RawMessage `json:"retryInMs"`

	OriginalModel         string   `json:"originalModel"`
	FallbackModel         string   `json:"fallbackModel"`
	APIRefusalExplanation string   `json:"apiRefusalExplanation"`
	HookErrors            []string `json:"hookErrors"`
	PreventedContinuation bool     `json:"preventedContinuation"`
	// Error is an object on the rows that carry one, and a word on others
	// ("error":"rate_limit", with what was said in the message): read as
	// it comes, so neither kind costs the row it is on.
	Error   json.RawMessage `json:"error"`
	Message *struct {
		Content    json.RawMessage `json:"content"`
		Model      string          `json:"model"`
		StopReason string          `json:"stop_reason"`
		Usage      *struct {
			Input      int64 `json:"input_tokens"`
			CacheWrite int64 `json:"cache_creation_input_tokens"`
			CacheRead  int64 `json:"cache_read_input_tokens"`
		} `json:"usage"`
	} `json:"message"`
	Attachment *struct {
		Type        string          `json:"type"`
		Prompt      json.RawMessage `json:"prompt"`
		CommandMode string          `json:"commandMode"`
		IsMeta      bool            `json:"isMeta"`
		// A file this one is about: its whole path (Filename for a file
		// that changed or was referred to, Path for a folder) and the
		// path as Claude Code shows it.
		Filename    string `json:"filename"`
		Path        string `json:"path"`
		DisplayPath string `json:"displayPath"`
		// The model the session runs with, as Claude Code tells the agent
		// at its start (before any answer has named one).
		Identity *struct {
			ModelID       string `json:"modelId"`
			MarketingName string `json:"marketingName"`
		} `json:"identity"`
		// A hook of the person's this one is about: which it is, what it
		// said, and what became of it.
		HookName   string `json:"hookName"`
		HookEvent  string `json:"hookEvent"`
		Stderr     string `json:"stderr"`
		ExitCode   int    `json:"exitCode"`
		TimedOut   bool   `json:"timedOut"`
		TimeoutMs  int64  `json:"timeoutMs"`
		DurationMs int64  `json:"durationMs"`
		// Background work this one is about (a "task_status" row): which,
		// how it stands, and the latest it has said of itself.
		TaskID       string `json:"taskId"`
		Status       string `json:"status"`
		DeltaSummary string `json:"deltaSummary"`
		// (goal_status: /goal's condition set, or judged)
		Met       json.RawMessage `json:"met"`
		Sentinel  json.RawMessage `json:"sentinel"`
		Condition json.RawMessage `json:"condition"`
		Reason    json.RawMessage `json:"reason"`
	} `json:"attachment"`
}

type claudeBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	Thinking  string          `json:"thinking"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
	IsError   bool            `json:"is_error"`
}

func (c *Claude) line(b []byte) {
	var r claudeRow
	if json.Unmarshal(b, &r) != nil || (r.IsSidechain && !c.sub) {
		c.Skipped++
		return
	}
	if r.GitBranch != nil {
		c.meta.Branch, c.branchSaid = clip(*r.GitBranch, maxMeta), true
	}
	switch r.Type {
	case "user", "assistant", "attachment", "system":
		var parent string
		_ = json.Unmarshal(r.ParentUUID, &parent)
		if parent == "" {
			parent = r.LogicalParent
		}
		c.tree.add(clip(r.UUID, maxID), clip(parent, maxID))
		c.tree.curRoot = parent == "" && string(r.ParentUUID) == "null"
	default:
		c.tree.cur = -1
	}
	switch r.Type {
	case "user":
		if r.Message != nil {
			c.user(&r)
			return
		}
	case "assistant":
		if r.Message != nil {
			c.assistant(&r)
			return
		}
	case "attachment":
		if a := r.Attachment; a != nil {
			if a.Type == "queued_command" && !a.IsMeta {
				c.queued(&r)
				return
			}
			if a.Type == "goal_status" {
				if ev := goalOf(a.Met, a.Sentinel, a.Condition, a.Reason); ev != nil {
					c.add(&r, RoleNote, []Part{{Kind: KindEvent, Event: ev}})
				}
				return
			}
			if c.attached(&r) {
				return
			}
		}
	case "api-request":
		c.request(&r)
		return
	case "cost-state":
		c.meta.Cost = costOf(&r)
		return
	case "ai-title":
		if t := strings.TrimSpace(clip(r.AITitle, maxTitle)); t != "" {
			c.aiTitle = t
		}
		return
	case "custom-title":
		c.myTitle = strings.TrimSpace(clip(r.CustomTitle, maxTitle))
		c.myTitleSaid = true
		return
	case "permission-mode":
		c.setMode(r.PermissionMode)
		return
	case "queue-operation":
		c.queueOp(&r)
		return
	case "system":
		if r.Subtype == "local_command" {
			var s string
			if json.Unmarshal(r.Content, &s) == nil {
				if text, ok := slashCommand(s); ok {
					c.add(&r, RoleUser, []Part{textPart(KindText, text)})
				}
			}
			return
		}
		if r.Subtype == "turn_duration" {
			c.turnOver(r.Timestamp)
		}
		if len(r.Retracted) > 0 { // (an answer the model's safeguards stopped, withdrawn as another model answers)
			c.withdraw(r.Retracted)
		}
		if t := c.systemTrouble(&r); t != nil {
			c.add(&r, RoleNote, []Part{{Kind: KindTrouble, Trouble: t}})
			return
		}
		if ev := systemEvent(&r); ev != nil {
			c.add(&r, RoleNote, []Part{{Kind: KindEvent, Event: ev}})
			return
		}
	}
	c.Skipped++
}

func (c *Claude) add(r *claudeRow, role string, parts []Part) {
	if len(parts) == 0 {
		return
	}
	if len(parts) > maxParts {
		parts = append([]Part(nil), parts[:maxParts]...)
	}
	c.seq++
	id := clip(r.UUID, maxID)
	// (Gone as it comes: it hangs on a row taken back.)
	c.entries = append(c.entries, &Entry{Seq: c.seq, Rev: c.rev, ID: id, Role: role, Time: clip(r.Timestamp, 64), Parts: parts, Gone: id != "" && c.tree.dead[id]})
	e := c.entries[len(c.entries)-1]
	for i := range e.Parts {
		if t := e.Parts[i].Tool; t != nil && t.ID != "" {
			c.tools[t.ID] = toolRef{e: e, t: t}
		}
	}
}

func (c *Claude) user(r *claudeRow) {
	// (A message of the person's, not a tool's result, tells by where it
	// hangs of a rewind. Not one Claude Code wrote itself (isMeta): /exit's
	// caveat hangs where the message before the last does, and the session
	// resumed goes on from that last one.)
	if r.IsMeta {
		// (Not shown. But one Claude Code sent on its own may be what waited:
		// another session's message, a scheduled task's prompt, which it
		// writes so when it takes them in — the waiting is over.)
		if r.PromptSource == "system" {
			var s string
			if json.Unmarshal(r.Message.Content, &s) == nil {
				c.metaArrived(s)
			}
		}
		return
	}
	c.setMode(r.PermissionMode)
	var s string
	if json.Unmarshal(r.Message.Content, &s) == nil {
		c.rewound()
		if strings.Contains(s, "<task-notification>") {
			c.ended(s)
		}
		if r.IsCompactSummary {
			c.compacted()
			c.add(r, RoleSummary, []Part{textPart(KindText, s)})
			return
		}
		if sh, ok := shellOutput(s); ok {
			if sh != nil { // (nothing printed: nothing shown, not its empty tags)
				c.add(r, RoleNote, []Part{{Kind: KindShell, Shell: sh}})
			}
			return
		}
		role, text := classifyUserText(s)
		if role == "" {
			return
		}
		if role == RoleNote {
			c.modelSet(text)
		}
		if role == RoleUser && word(r.TurnOrigin) == "scheduled" {
			c.scheduledTurn(r, text)
			return
		}
		if role == RoleUser && r.PromptSource == "system" {
			role = RoleNote
		}
		c.add(r, role, []Part{textPart(KindText, text)})
		if role == RoleUser {
			// (A shell command the person typed with "!": shown so, it waited
			// as the command alone.)
			if strings.Contains(s, "<bash-input>") {
				c.arrived(strings.TrimPrefix(text, "! "))
			} else {
				c.arrived(text)
			}
			c.wasSaid(text)
		}
		return
	}
	var blocks []claudeBlock
	if json.Unmarshal(r.Message.Content, &blocks) != nil {
		return
	}
	// (Claude Code's account of the call is the row's: of its one result.)
	results := 0
	for _, b := range blocks {
		if b.Type == "tool_result" {
			results++
		}
	}
	if results == 0 {
		c.rewound()
	}
	images := false
	for _, b := range blocks {
		images = images || b.Type == "image"
	}
	var parts, notes []Part
	for _, b := range blocks {
		switch b.Type {
		case "tool_result":
			if results == 1 {
				c.result(b, r.ToolUseResult)
			} else {
				c.result(b, nil)
			}
		case "text":
			if strings.Contains(b.Text, "<task-notification>") {
				c.ended(b.Text)
			}
			if sh, ok := shellOutput(b.Text); ok {
				if sh != nil {
					notes = append(notes, Part{Kind: KindShell, Shell: sh})
				}
				continue
			}
			switch role, text := classifyUserText(b.Text); role {
			case RoleUser:
				if images {
					text = noImageTokens(text)
				}
				if text != "" {
					parts = append(parts, textPart(KindText, text))
				}
			case RoleNote:
				c.modelSet(text)
				notes = append(notes, textPart(KindText, text))
			}
		case "image":
			parts = append(parts, Part{Kind: KindImage})
		}
	}
	if r.IsCompactSummary { // (the summary written as a session was compacted, as blocks)
		c.compacted()
		c.add(r, RoleSummary, parts)
		return
	}
	role := RoleUser
	if results == 0 && len(parts) > 0 && word(r.TurnOrigin) == "scheduled" {
		c.scheduledTurn(r, partsText(parts))
		c.add(r, RoleNote, notes)
		return
	}
	if r.PromptSource == "system" {
		role = RoleNote
	}
	c.add(r, role, parts)
	if role == RoleUser && len(parts) > 0 {
		c.arrived(partsText(parts))
		c.wasSaid(partsText(parts))
	}
	c.add(r, RoleNote, notes) // (Claude Code's own word that it was stopped, say)
}

func (c *Claude) queueOp(r *claudeRow) {
	var s string
	_ = json.Unmarshal(r.Content, &s)
	switch r.Operation {
	case "enqueue":
		// What waits there is the person's message, another session's
		// agent writing to this one, or a notice from the harness; what
		// belongs to no one of these (a background task's notice) is not
		// shown at all.
		text, from, note := queuedText(s)
		if text != "" && c.ofNow(r.Timestamp) {
			q := queuedItem{raw: s, from: from, note: note}
			q.Text = queuedCut(text)
			q.Time = clip(r.Timestamp, 64)
			c.queue = append(c.queue, q)
			if n := len(c.queue) - maxQueue; n > 0 {
				c.queue = append([]queuedItem(nil), c.queue[n:]...)
			}
		}
	case "remove":
		for i, q := range c.queue {
			if q.raw == s || sameCrossSession(q, s) {
				c.queue = append(c.queue[:i:i], c.queue[i+1:]...)
				c.gone = append(c.gone, goneItem{queueKey(q.Text), clip(r.Timestamp, 64), q.from})
				if n := len(c.gone) - maxQueue; n > 0 {
					c.gone = append([]goneItem(nil), c.gone[n:]...)
				}
				break
			}
		}
	case "popOne":
		if n := len(c.queue); n > 0 {
			c.queue = c.queue[:n-1]
		}
	case "popAll", "clear":
		c.queue = nil
	}
}

// bodyAfterTag: what follows an opening tag, without the tag and its
// attributes, and without its closing tag where there is one (the harness
// writes some of these open-ended).
func bodyAfterTag(s, tag string) string {
	i := strings.Index(s, "<"+tag)
	if i < 0 {
		return strings.TrimSpace(s)
	}
	j := strings.IndexByte(s[i:], '>')
	if j < 0 {
		return strings.TrimSpace(s)
	}
	body := s[i+j+1:]
	if k := strings.LastIndex(body, "</"+tag+">"); k >= 0 {
		body = body[:k]
	}
	return strings.TrimSpace(body)
}

// queuedText: what a message waiting to be taken in says, who it is from
// (another session's agent) and whether it is a notice rather than anyone's
// message. "" when it is of none of these.
func queuedText(s string) (text, from string, note bool) {
	if m := crossSession.FindStringSubmatch(s); m != nil {
		// (The envelope says where it came from, down to a socket and the
		// hops it took: of that, only which session it was.)
		return bodyAfterTag(s, "cross-session-message"), clip(m[1], maxMeta), true
	}
	if strings.HasPrefix(strings.TrimSpace(s), "[Cross-session idle notice]") {
		return strings.TrimSpace(s), "", true
	}
	if role, t := classifyUserText(s); role == RoleUser {
		return t, "", false
	}
	return "", "", false
}

// arrived: the person's message with this text came in (as a turn, or taken
// in during one): it waits no more.
// (One removed already, by its text, is the one arriving: not another of
// the same text that still waits.)
func (c *Claude) arrived(text string) {
	if c.goneArrived(text, "") {
		return
	}
	t := queueKey(text)
	k := queueKey(queuedCut(text)) // (as the queue keeps a long one, and its removals)
	for i, q := range c.queue {
		// (The person's: not another session's that has the same words.)
		if key := queueKey(q.Text); q.from == "" && (key == t || key == k) {
			c.queue = append(c.queue[:i:i], c.queue[i+1:]...)
			return
		}
	}
}

// metaArrived: a message Claude Code sent on its own (not shown): another
// session's (by who and its words) or one of its own making (a scheduled
// task's prompt: its words whole). What waited and is that, by the same
// sender and the same words, stops waiting — the first such, as they are
// taken in; one removed meanwhile (and queued again since) is that one.
func (c *Claude) metaArrived(s string) {
	text, from := s, ""
	if i := strings.Index(s, "<cross-session-message"); i >= 0 {
		m := crossSession.FindStringSubmatch(s[i:])
		if m == nil {
			return
		}
		text, from = bodyAfterTag(s[i:], "cross-session-message"), clip(m[1], maxMeta)
	}
	if text == "" || c.goneArrived(text, from) {
		return
	}
	for i, q := range c.queue {
		words := q.raw
		if q.from != "" {
			words = bodyAfterTag(q.raw, "cross-session-message")
		}
		if q.from == from && words == text {
			c.queue = append(c.queue[:i:i], c.queue[i+1:]...)
			return
		}
	}
}

// sameCrossSession: s is another session's message q waits as, by who and
// its words (its envelope's attributes are not always written alike: a
// hop-chain, in older versions, on its enqueue and not its removal).
func sameCrossSession(q queuedItem, s string) bool {
	if q.from == "" {
		return false
	}
	m := crossSession.FindStringSubmatch(s)
	return m != nil && clip(m[1], maxMeta) == q.from && bodyAfterTag(s, "cross-session-message") == bodyAfterTag(q.raw, "cross-session-message")
}

// turnOver: a turn has ended (its "turn_duration" row). A slash command
// that waited is not waiting now: Claude Code runs it itself as the turn
// ends, writing nothing of it. (Messages are told gone by their arrival.)
func (c *Claude) turnOver(at string) {
	end, err := time.Parse(time.RFC3339Nano, at)
	if err != nil {
		return
	}
	keep := c.queue[:0:0]
	for _, q := range c.queue {
		// (Queued before it ended, as times, not as text; when not known, kept.)
		t, err := time.Parse(time.RFC3339Nano, q.Time)
		if err != nil || !t.Before(end) || q.from != "" || !strings.HasPrefix(q.raw, "/") {
			keep = append(keep, q)
		}
	}
	c.queue = keep
}

// goneArrived: the message arriving is one removed from what waited (to
// arrive still): that is what it is, and it is forgotten. True if so.
func (c *Claude) goneArrived(text, from string) bool {
	t := queueKey(text)
	k := queueKey(queuedCut(text)) // (as the queue keeps a long one, and its removals)
	for i, g := range c.gone {
		if g.from == from && (g.text == t || g.text == k) {
			c.gone = append(c.gone[:i:i], c.gone[i+1:]...)
			return true
		}
	}
	return false
}

func squashed(s string) string { return strings.Join(strings.Fields(s), " ") }

// queueKey is what a queued message and its arrival are matched by: its
// words, without the "[Image #N]" a queued one has and one taken in may not.
func queueKey(s string) string { return squashed(imageToken.ReplaceAllString(s, " ")) }

// queuedCut is a waiting message's text as kept: without blank lines at its
// ends (a paste arrives without its wrapper's, see unpasted), then cut where
// long, so a long one is cut at the same place waiting and arriving.
func queuedCut(text string) string {
	kept, _ := cut(clean(blankEnds(text)), maxText)
	return kept
}

// mid marks the entry just added as taken in during the agent's turn.
func (c *Claude) mid() {
	if n := len(c.entries); n > 0 && c.entries[n-1].Rev == c.rev {
		c.entries[n-1].Mid = true
	}
}

func (c *Claude) queued(r *claudeRow) {
	a := r.Attachment
	var s string
	if json.Unmarshal(a.Prompt, &s) == nil {
		if a.CommandMode == "task-notification" {
			c.ended(s) // (what it says of work set going: ended, or at it still)
			if _, text := classifyUserText(s); text != "" {
				c.add(r, RoleNote, []Part{textPart(KindText, text)})
			}
			return
		}
		if a.CommandMode == "prompt" {
			if role, text := classifyUserText(s); role != "" {
				n := len(c.entries)
				c.add(r, role, []Part{textPart(KindText, text)})
				if role == RoleUser && len(c.entries) > n {
					c.mid()
					c.arrived(text)
					c.wasSaid(text)
				}
			}
		}
		return
	}
	var blocks []claudeBlock
	if a.CommandMode != "prompt" || json.Unmarshal(a.Prompt, &blocks) != nil {
		return
	}
	images := false
	for _, b := range blocks {
		images = images || b.Type == "image"
	}
	var parts []Part
	for _, b := range blocks {
		switch b.Type {
		case "text":
			if role, text := classifyUserText(b.Text); role == RoleUser {
				if images {
					text = noImageTokens(text)
				}
				if text != "" {
					parts = append(parts, textPart(KindText, text))
				}
			}
		case "image":
			parts = append(parts, Part{Kind: KindImage})
		}
	}
	n := len(c.entries)
	c.add(r, RoleUser, parts)
	if len(c.entries) > n {
		c.mid()
		c.arrived(partsText(parts))
		c.wasSaid(partsText(parts))
	}
}

// partsText is a message's text, its parts' joined (as it was queued).
func partsText(parts []Part) string {
	var t []string
	for _, p := range parts {
		if p.Kind == KindText {
			t = append(t, p.Text)
		}
	}
	return strings.Join(t, "\n")
}

// attached takes in an attachment row that is about a file: one Claude Code
// found changed, or one the person referred to in what they wrote. It
// answers whether the row was of use.
//
// Both come without being said in the conversation, so without them the
// agent is seen acting on something nobody mentioned. Both are also
// written for reasons no one would want to read:
//   - Claude Code reports a file as changed whenever its modification time
//     did, which is mostly the agent's own writing (a shell command's, which
//     the turn already shows as its own edit): only a change it did not make
//     is kept, and whose it is cannot be told (the person's, another
//     session's, a formatter's).
//   - It attaches a file for each one the person referred to, but also for
//     every file a turn had read before a compaction, which nobody asked
//     for: only those the person's last message names are kept.
func (c *Claude) attached(r *claudeRow) bool {
	a := r.Attachment
	if t := hookTrouble(a.Type, a.HookName, a.HookEvent, a.Stderr, a.ExitCode, a.TimedOut, a.TimeoutMs); t != nil {
		c.add(r, RoleNote, []Part{{Kind: KindTrouble, Trouble: t}})
		return true
	}
	switch a.Type {
	case "model":
		// Which model the session runs with, said at its start: until this
		// was read, nothing named one before the first answer came, so a
		// session not yet answered showed none and could not be switched.
		if a.Identity != nil {
			if id := strings.TrimSpace(a.Identity.ModelID); id != "" {
				c.meta.Model = clip(id, maxMeta)
			} else if name := strings.TrimSpace(a.Identity.MarketingName); name != "" {
				c.meta.Model = clip(name, maxMeta)
			}
		}
		return true // (read; nothing of it to show in the thread, the bar says it)
	case "edited_text_file":
		p, _ := cut(clean(a.Filename), maxDetail)
		if p == "" || c.wrote[p] {
			return false
		}
		return c.noted(r, "changed", p, "")
	case "task_status":
		// What Claude Code says of background work of its own accord.
		c.taskStatusSaid(a.TaskID, a.Status, a.DeltaSummary)
		return true // (read; nothing of it to show in the thread, the bar says it)
	case "file", "directory":
		whole := a.Filename
		if whole == "" {
			whole = a.Path
		}
		p, _ := cut(clean(whole), maxDetail)
		show, _ := cut(clean(a.DisplayPath), maxDetail)
		if p == "" && show == "" {
			return false
		}
		if !c.referred(show) && !c.referred(p) {
			return false
		}
		return c.noted(r, "brought", p, show)
	}
	return false
}

// referred: the person's last message named this file (as "@" and its path,
// which is how Claude Code puts one in), the whole of it and not the start
// of a longer name ("@x.go.bak" is not "x.go").
func (c *Claude) referred(path string) bool {
	if path == "" {
		return false
	}
	for rest := c.said; ; {
		i := strings.Index(rest, "@"+path)
		if i < 0 {
			return false
		}
		rest = rest[i+1+len(path):]
		if rest == "" {
			return true
		}
		if r, _ := utf8.DecodeRuneInString(rest); !isPathRune(r) {
			return true
		}
	}
}

// isPathRune: a character a file's name goes on with (so that a match
// ending in one is of a longer name). A letter or a digit of any script,
// or one of the marks a path is written with: what follows a reference in
// a sentence ("@x.go，再说" or "@x.go.") ends it.
func isPathRune(r rune) bool {
	switch r {
	case '.', '/', '-', '_', '~', '+', '#', '@', '%', '=', '&':
		return true
	}
	return unicode.IsLetter(r) || unicode.IsDigit(r)
}

// noted puts a file beside the ones a note before it named, where that note
// is the latest thing read and of the same kind; else it makes the note.
func (c *Claude) noted(r *claudeRow, kind, path, show string) bool {
	if show == "" {
		show = path
	}
	if n := c.lastNoted(kind); n != nil {
		if c.notedLast == show {
			return true
		}
		for _, had := range n.Paths {
			if had == show {
				return true
			}
		}
		c.notedLast = show
		if len(n.Paths) >= maxNoted {
			n.More++
		} else {
			n.Paths = append(n.Paths, show)
		}
		c.entries[len(c.entries)-1].Rev = c.rev
		return true
	}
	c.notedLast = show
	c.add(r, RoleNote, []Part{{Kind: KindNoted, Noted: &Noted{Kind: kind, Paths: []string{show}}}})
	return true
}

// lastNoted is the note of these files the log last gave, where it is the
// latest entry and names files of the same kind.
func (c *Claude) lastNoted(kind string) *Noted {
	if len(c.entries) == 0 {
		return nil
	}
	e := c.entries[len(c.entries)-1]
	if e.Role != RoleNote || len(e.Parts) != 1 || e.Parts[0].Kind != KindNoted {
		return nil
	}
	if n := e.Parts[0].Noted; n != nil && n.Kind == kind {
		return n
	}
	return nil
}

// wasSaid keeps the person's message, to read the files it refers to from
// (see attached). Only its start: a reference is typed, not pasted in bulk.
func (c *Claude) wasSaid(text string) {
	c.said, _ = cut(text, maxSaid)
}

// wroteFile remembers that the agent itself wrote a file (see attached).
func (c *Claude) wroteFile(path string) {
	if path == "" {
		return
	}
	if c.wrote == nil {
		c.wrote = map[string]bool{}
	}
	// (Full: a file written long ago may be reported as changed by someone
	// else. One note too many beats dropping what the agent did.)
	if len(c.wrote) < maxWrote {
		c.wrote[path] = true
	}
}

func (c *Claude) assistant(r *claudeRow) {
	if ev := limitOf(r); ev != nil {
		c.add(r, RoleNote, []Part{{Kind: KindEvent, Event: ev}})
		return
	}
	// (A row Claude Code made up itself, an error say, is of no model.)
	if m := r.Message.Model; m != "" && m != "<synthetic>" {
		c.meta.Model = clip(m, maxMeta)
		if e := r.Effort; e != "" {
			c.meta.Effort = clip(e, maxMeta)
		}
	}
	if u := r.Message.Usage; u != nil && r.Message.Model != "<synthetic>" {
		if n := u.Input + u.CacheWrite + u.CacheRead; n > 0 {
			c.meta.Context, c.ctxSaid = n, true
		}
	}
	var blocks []claudeBlock
	if json.Unmarshal(r.Message.Content, &blocks) != nil {
		return
	}
	var parts []Part
	var results []claudeBlock
	for _, b := range blocks {
		switch b.Type {
		case "text":
			if t := strings.TrimSpace(b.Text); t != "" && t != "(no content)" {
				parts = append(parts, textPart(KindText, b.Text))
			}
		case "thinking":
			if strings.TrimSpace(b.Thinking) != "" {
				parts = append(parts, textPart(KindThinking, b.Thinking))
			}
		case "tool_use", "server_tool_use":
			t := &Tool{ID: clip(b.ID, maxID), Name: clip(b.Name, 128)}
			t.Title, t.Detail, t.Old, t.New = toolInput(b.Name, b.Input)
			t.Add, t.Del = editLines(b.Name, b.Input)
			switch b.Name {
			case "AskUserQuestion":
				t.Asks = asksOf(b.Input)
			case "ExitPlanMode":
				t.Plan, t.PlanCut, t.PlanPath = planOf(b.Input)
			}
			parts = append(parts, Part{Kind: KindTool, Tool: t})
		default:
			// What a tool of the model's own answered comes back in a row
			// of its own ("…_tool_result": the advisor's, say): the call it
			// answers is done.
			if strings.HasSuffix(b.Type, "_tool_result") && b.ToolUseID != "" {
				results = append(results, b)
			}
		}
	}
	c.add(r, RoleAssistant, parts)
	// An answer that ran into the model's limit stops mid-sentence with
	// nothing said of why: a note after it says so. (One stopped by the
	// model's safeguards has a row of its own: see systemTrouble.)
	if r.Message.StopReason == "max_tokens" {
		c.add(r, RoleNote, []Part{{Kind: KindTrouble, Trouble: &Trouble{Kind: TroubleCut}}})
	}
	for _, b := range results {
		c.result(b, nil)
	}
	for _, b := range blocks {
		if f := taskChange(b.Name, b.Input); f != nil && b.Type == "tool_use" {
			if ref, ok := c.tools[clip(b.ID, maxID)]; ok {
				ref.task = f
				c.tools[clip(b.ID, maxID)] = ref
			}
		}
	}
}

// request takes in what a request of the conversation itself asked for
// (not one made beside it, as for a title): its model, and whether it was
// given the 1M window. (The latest says: a session switched back drops it.)
func (c *Claude) request(r *claudeRow) {
	p := r.Params
	if p == nil || p.Model == "" || (r.QuerySource != "" && r.QuerySource != "repl_main_thread") {
		return
	}
	c.lastReq.model = claudeKey(clip(p.Model, maxMeta))
	betas := bytes.TrimSpace(p.Betas)
	c.lastReq.wide = len(betas) > 0 && betas[0] == '[' && bytes.Contains(betas, []byte(`"context-1m`))
}

// compacted: what filled the context before is gone; how full it is now is
// not known until the next answer says.
func (c *Claude) compacted() {
	c.meta.Context, c.ctxSaid = 0, true
}

// modelSet takes in what /model said it switched to ("Set model to
// `Sonnet 5.5` and saved as…", "Set model to opus"): the model shown until
// the next answer names its id.
var modelSetText = regexp.MustCompile("^Set model to (?:`([^`]+)`|(.+?)(?:\\s+and saved\\b.*)?$)")

func (c *Claude) modelSet(text string) {
	m := modelSetText.FindStringSubmatch(text)
	if m == nil {
		return
	}
	name := strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(m[1]+m[2]), "(default)"))
	if name != "" {
		c.meta.Model = clip(name, maxMeta)
	}
}

func (c *Claude) setMode(m string) {
	if m = strings.TrimSpace(clip(m, maxMeta)); m != "" {
		c.meta.Mode = m
	}
}

// result fills in the call a tool_result answers, with Claude Code's
// account of it (account: the row's toolUseResult, or nil). A result for a
// call not kept (dropped as old, or never seen) is ignored.
func (c *Claude) result(b claudeBlock, account json.RawMessage) {
	b.ToolUseID = clip(b.ToolUseID, maxID) // (as its call's is kept)
	ref, ok := c.tools[b.ToolUseID]
	if !ok {
		if c.orphans == nil {
			c.orphans = map[string]orphan{}
		}
		if len(c.orphans) < maxOrphans && b.ToolUseID != "" {
			c.orphans[b.ToolUseID] = orphan{b, append(json.RawMessage(nil), account...)}
			if !b.IsError {
				c.taskSaid(taskOp{orphan: b.ToolUseID})
			}
		}
		return
	}
	text, images, refs := resultText(b.Content)
	// Every file the call wrote is the agent's own doing, whatever Claude
	// Code reports later of that file having changed (see attached): the
	// ones a command named as changed, beyond those whose patch is kept
	// (a command that failed may have written them all the same), and the
	// file an edit went through on.
	for _, p := range wroteFiles(ref.t.Name, account, ref.t.Detail, b.IsError) {
		c.wroteFile(p)
	}
	// Work the agent stopped itself has ended, though nothing is sent to
	// say so (the notice only comes for work that ends of its own accord).
	if ref.t.Name == "TaskStop" && !b.IsError {
		c.endRun(clip(ref.t.Detail, maxID))
	}
	if !b.IsError {
		ref.t.Add, ref.t.Del = madeLines(ref.t.Name, account, ref.t.Add, ref.t.Del)
		if ref.t.Name == "Edit" || ref.t.Name == "MultiEdit" || ref.t.Name == "Write" {
			if h, cut, said := patchOf(account); said {
				// (An empty patch: a file written over with what it held.)
				ref.t.Patch, ref.t.PatchCut, ref.t.Old, ref.t.New = h, cut, "", ""
				ref.t.Same = len(h) == 0
			}
		}
		// A subagent's report, where it came back later: what the call
		// answered then is only that it was sent off (it is sent again, to
		// the one waiting for it, as an agent-message).
		if rep := handbackReport(account); rep != "" {
			text = rep
		}
		c.setGoing(ref.t, account, ref.e.Time)
		// What a command changed, as Claude Code reports it: the files, each
		// with its patch. The lines of the turn's changes count them too.
		if files, more := bashFiles(account); len(files) > 0 {
			ref.t.Files, ref.t.MoreFiles = files, more
			for _, f := range files {
				ref.t.Add, ref.t.Del = ref.t.Add+f.Add, ref.t.Del+f.Del
			}
		}
		var op taskOp
		if ref.task != nil {
			op = taskOp{f: ref.task, whole: ref.t.Name == "TodoWrite"}
		} else if ref.t.Name == "TaskCreate" {
			acc, txt := append(json.RawMessage(nil), account...), text
			op = taskOp{f: func(c *Claude) { c.created(acc, txt) }}
		}
		if op.f != nil && c.late != nil {
			c.late[b.ToolUseID] = op
		} else if op.f != nil {
			op.f(c)
			c.taskSaid(op)
		}
	}
	if len(ref.t.Asks) > 0 && !b.IsError {
		ref.t.Answers = answersOf(ref.t.Asks, account)
	}
	if id := subagentOf(account); id != "" {
		ref.t.AgentID = id
	}
	ref.t.Done = true
	ref.t.Error = b.IsError
	ref.t.ResultLines = strings.Count(strings.TrimRight(text, "\n"), "\n") + 1
	if text == "" {
		ref.t.ResultLines = 0
	}
	ref.t.setResult(text)
	ref.t.Images, ref.t.Refs = images, refs
	ref.e.Rev = c.rev
	delete(c.tools, b.ToolUseID)
}

// lineCount is how many lines s has ("" none; a last newline ends a line).
func lineCount(s string) int {
	if s == "" {
		return 0
	}
	return strings.Count(strings.TrimSuffix(s, "\n"), "\n") + 1
}

// replaced counts the lines a replacement of old by new adds and removes,
// leaving out the lines both begin and end with (a last newline ends a line,
// it does not begin one).
func replaced(old, new string) (add, del int) {
	split := func(s string) []string {
		if s == "" {
			return nil
		}
		return strings.Split(strings.TrimSuffix(s, "\n"), "\n")
	}
	a, b := split(old), split(new)
	for len(a) > 0 && len(b) > 0 && a[0] == b[0] {
		a, b = a[1:], b[1:]
	}
	for len(a) > 0 && len(b) > 0 && a[len(a)-1] == b[len(b)-1] {
		a, b = a[:len(a)-1], b[:len(b)-1]
	}
	return len(b), len(a)
}

// editLines is what an edit's input says it changes: a guess (a file
// written over may have had lines), until its result says.
func editLines(name string, raw json.RawMessage) (add, del int) {
	var in struct {
		OldString string `json:"old_string"`
		NewString string `json:"new_string"`
		Content   string `json:"content"`
		Edits     []struct {
			OldString string `json:"old_string"`
			NewString string `json:"new_string"`
		} `json:"edits"`
	}
	if json.Unmarshal(raw, &in) != nil {
		return 0, 0
	}
	switch name {
	case "Edit":
		return replaced(in.OldString, in.NewString)
	case "MultiEdit":
		for _, e := range in.Edits {
			a, d := replaced(e.OldString, e.NewString)
			add, del = add+a, del+d
		}
		return add, del
	case "Write":
		return lineCount(in.Content), 0
	}
	return 0, 0
}

// madeLines is what Claude Code says an edit changed: the lines of its
// patch, or all of a file it created; else what was guessed.
func madeLines(name string, account json.RawMessage, add, del int) (int, int) {
	if len(account) == 0 || (name != "Edit" && name != "MultiEdit" && name != "Write") {
		return add, del
	}
	var a struct {
		Type    string `json:"type"`
		Content string `json:"content"`
		Patch   *[]struct {
			Lines []string `json:"lines"`
		} `json:"structuredPatch"`
	}
	if json.Unmarshal(account, &a) != nil {
		return add, del
	}
	if a.Type == "create" {
		return lineCount(a.Content), 0
	}
	// (A patch, even an empty one: a file written over with what it held.)
	if a.Patch != nil {
		add, del = 0, 0
		for _, h := range *a.Patch {
			for _, l := range h.Lines {
				switch {
				case strings.HasPrefix(l, "+"):
					add++
				case strings.HasPrefix(l, "-"):
					del++
				}
			}
		}
		return add, del
	}
	return add, del
}

// rawHunk is a patch's hunk as Claude Code writes it.
type rawHunk struct {
	OldStart int      `json:"oldStart"`
	NewStart int      `json:"newStart"`
	Lines    []string `json:"lines"`
}

// hunksOf reads them, within what is left of a budget (size, counted up):
// short says some of it was left out.
func hunksOf(in []rawHunk, size *int) (hunks []Hunk, short bool) {
	for _, h := range in {
		out := Hunk{OldStart: max(h.OldStart, 0), NewStart: max(h.NewStart, 0)}
		for _, l := range h.Lines {
			if l == "" || (l[0] != ' ' && l[0] != '-' && l[0] != '+') {
				continue // ("\\ No newline at end of file", and the like)
			}
			l, c := cut(clean(l), maxPatchLine)
			if *size+len(l) > maxPatch {
				if len(out.Lines) > 0 {
					hunks = append(hunks, out)
				}
				return hunks, true
			}
			*size += len(l)
			out.Lines = append(out.Lines, l)
			short = short || c
		}
		if len(out.Lines) > 0 {
			hunks = append(hunks, out)
		}
	}
	return hunks, short
}

// countLines: what the hunks added and removed, whole (the patch kept may
// be less of it, where there was no room for all of it).
func countLines(hunks []rawHunk) (add, del int) {
	for _, h := range hunks {
		for _, l := range h.Lines {
			switch {
			case strings.HasPrefix(l, "+"):
				add++
			case strings.HasPrefix(l, "-"):
				del++
			}
		}
	}
	return add, del
}

// setGoing: the call set work going that is left to itself (a command in
// the background, a subagent sent off on its own). It stays there until
// the agent is told it has ended.
func (c *Claude) setGoing(t *Tool, account json.RawMessage, at string) {
	if len(account) == 0 {
		return
	}
	var a struct {
		Background string `json:"backgroundTaskId"`
		Agent      string `json:"agentId"`
		Async      bool   `json:"isAsync"`
		Status     string `json:"status"`
	}
	if json.Unmarshal(account, &a) != nil {
		return
	}
	id, kind := "", "cmd"
	switch {
	case a.Background != "":
		id = a.Background
	case a.Async && a.Agent != "" && a.Status != "completed":
		id, kind = a.Agent, "agent"
	}
	id = clip(id, maxID)
	if id == "" {
		return
	}
	// (Told it had ended before this came: it is over, not going.)
	if c.ranOut[id] {
		delete(c.ranOut, id)
		return
	}
	if _, there := c.running[id]; there || len(c.running) >= maxRunning {
		return
	}
	what := t.Title
	if what == "" {
		what = t.Detail
	}
	c.running[id] = Running{ID: id, What: clip(clean(what), maxTitle), Kind: kind, Time: at}
	c.runOrder = append(c.runOrder, id)
}

// ended: what the agent was told of work it set going — that it has ended
// (a status), or what it said of itself meanwhile (an event).
func (c *Claude) ended(s string) {
	// The file the work writes as it goes is the agent's own, however
	// Claude Code later reports that file as changed (see attached).
	if f := taskOutFile.FindStringSubmatch(s); f != nil {
		if p, _ := cut(clean(strings.TrimSpace(f[1])), maxDetail); p != "" {
			c.wroteFile(p)
		}
	}
	// One notice may be of several at once: a session that ended with work
	// still going says so of all of it in one ("marked stopped"), with a
	// marker among them that is no task of its own.
	ids := taskID.FindAllStringSubmatch(s, maxTaskIDs)
	if ids == nil {
		return
	}
	over := taskStatus.MatchString(s)
	var note string
	if ev := taskEvent.FindStringSubmatch(s); ev != nil {
		note = clip(clean(strings.TrimSpace(ev[1])), maxTitle)
	}
	for _, m := range ids {
		id := clip(strings.TrimSpace(m[1]), maxID)
		if id == "" || strings.HasPrefix(id, "__orphan_summary") {
			continue
		}
		if over {
			c.endRun(id)
			continue
		}
		if r, ok := c.running[id]; ok && note != "" {
			r.Note = note
			c.running[id] = r
		}
	}
}

// endRun takes work off the list of what is still going. Where what set it
// going has not been read (the log read from its middle), it is noted as
// over, so that reading the start later does not set it going again.
func (c *Claude) endRun(id string) {
	if id == "" {
		return
	}
	if _, ok := c.running[id]; !ok {
		if c.ranOut == nil {
			c.ranOut = map[string]bool{}
		}
		// (Full: the newest word is the one worth keeping, so one makes
		// way. Work told of as over twice takes one place, not two.)
		for len(c.ranOut) >= maxRunning && !c.ranOut[id] {
			for k := range c.ranOut {
				delete(c.ranOut, k)
				break
			}
		}
		c.ranOut[id] = true
		return
	}
	delete(c.running, id)
	c.runOrder = slices.DeleteFunc(c.runOrder, func(x string) bool { return x == id })
}

// taskSaid takes in what Claude Code says of a background task of its own
// accord (a "task_status" attachment): that it has ended, or what it has
// said of itself since. Without this, work the agent stopped itself stays
// on the list as going for as long as the conversation is read.
func (c *Claude) taskStatusSaid(id, status, note string) {
	id = clip(id, maxID)
	if id == "" {
		return
	}
	if status != "" && status != "running" && status != "pending" {
		c.endRun(id)
		return
	}
	if r, ok := c.running[id]; ok && note != "" {
		r.Note = clip(clean(strings.TrimSpace(note)), maxTitle)
		c.running[id] = r
	}
}

// handbackReport: the report a subagent handed back, as Claude Code keeps
// it with the call that sent it off (since 2.1.300). "" when there is none.
func handbackReport(account json.RawMessage) string {
	if len(account) == 0 {
		return ""
	}
	var a struct {
		Report string `json:"handbackReport"`
	}
	if json.Unmarshal(account, &a) != nil {
		return ""
	}
	return clean(a.Report)
}

// wroteFiles are the files a call wrote, as Claude Code's account of it
// says: the ones a command changed (all that it names, beyond those whose
// patch is kept) and, for an edit that went through, the file it was of.
func wroteFiles(name string, account json.RawMessage, path string, failed bool) []string {
	var out []string
	if !failed {
		switch name {
		case "Edit", "Write", "MultiEdit", "NotebookEdit":
			if path != "" {
				out = append(out, path)
			}
		}
	}
	if len(account) == 0 {
		return out
	}
	var a struct {
		Diff *struct {
			Files []struct {
				Path string `json:"filePath"`
			} `json:"files"`
			Changed []string `json:"changedFiles"`
		} `json:"bashEditDiff"`
	}
	if json.Unmarshal(account, &a) != nil || a.Diff == nil {
		return out
	}
	had := make(map[string]bool, len(a.Diff.Files)+len(a.Diff.Changed))
	for _, p := range out {
		had[p] = true
	}
	add := func(p string) {
		p, _ = cut(clean(p), maxDetail)
		if p == "" || had[p] || len(out) >= maxWrote {
			return
		}
		had[p] = true
		out = append(out, p)
	}
	for _, f := range a.Diff.Files {
		add(f.Path)
	}
	for _, p := range a.Diff.Changed {
		add(p)
	}
	return out
}

// bashFiles are the files a command changed, as Claude Code reports them
// (toolUseResult's bashEditDiff): each with its patch, within maxPatch for
// all of them together; more: it changed that many more than are kept.
func bashFiles(account json.RawMessage) (files []ToolFile, more int) {
	if len(account) == 0 {
		return nil, 0
	}
	var a struct {
		Diff *struct {
			Files []struct {
				Path  string    `json:"filePath"`
				Hunks []rawHunk `json:"hunks"`
			} `json:"files"`
			More int `json:"moreFiles"`
		} `json:"bashEditDiff"`
	}
	if json.Unmarshal(account, &a) != nil || a.Diff == nil {
		return nil, 0
	}
	size := 0
	for _, f := range a.Diff.Files {
		path, _ := cut(clean(f.Path), maxDetail)
		if path == "" {
			continue
		}
		if len(files) >= maxFiles {
			more++
			continue
		}
		out := ToolFile{Path: path}
		out.Patch, out.PatchCut = hunksOf(f.Hunks, &size)
		out.Add, out.Del = countLines(f.Hunks)
		out.PatchCut = out.PatchCut || len(out.Patch) == 0
		files = append(files, out)
	}
	return files, more + max(a.Diff.More, 0)
}

// patchOf is the edit Claude Code reports having made (toolUseResult's
// structuredPatch), within maxPatch; said: it reports one (even empty: a
// file written over with what it held), not for a file it created (whose
// content New already holds). short: some of it was left out.
func patchOf(account json.RawMessage) (hunks []Hunk, short, said bool) {
	if len(account) == 0 {
		return nil, false, false
	}
	var a struct {
		Type  string     `json:"type"`
		Patch *[]rawHunk `json:"structuredPatch"`
	}
	if json.Unmarshal(account, &a) != nil || a.Patch == nil || (a.Type == "create" && len(*a.Patch) == 0) {
		return nil, false, false
	}
	size := 0
	for _, h := range *a.Patch {
		out := Hunk{OldStart: max(h.OldStart, 0), NewStart: max(h.NewStart, 0)}
		for _, l := range h.Lines {
			if l == "" || (l[0] != ' ' && l[0] != '-' && l[0] != '+') {
				continue // ("\\ No newline at end of file", and the like)
			}
			l, c := cut(clean(l), maxPatchLine)
			if size+len(l) > maxPatch {
				if len(out.Lines) > 0 {
					hunks = append(hunks, out)
				}
				return hunks, true, true
			}
			size += len(l)
			out.Lines = append(out.Lines, l)
			short = short || c
		}
		if len(out.Lines) > 0 {
			hunks = append(hunks, out)
		}
	}
	return hunks, short, true
}

// The task list. TodoWrite gives all of it; TaskCreate adds one (its id is
// in its result), TaskUpdate changes one by its id.
var taskCreated = regexp.MustCompile(`^Task #(\w{1,32}) created successfully: (.*)`)

func taskState(s string) string {
	switch s {
	case "pending", "in_progress", "completed":
		return s
	}
	return "pending"
}

func taskChange(name string, raw json.RawMessage) func(c *Claude) {
	switch name {
	case "TodoWrite":
		var in struct {
			Todos []struct {
				Content string `json:"content"`
				Status  string `json:"status"`
			} `json:"todos"`
		}
		if json.Unmarshal(raw, &in) != nil {
			return nil
		}
		var list []taskItem
		for _, t := range in.Todos {
			list = append(list, taskItem{Task: Task{Subject: clip(t.Content, maxTitle), Status: taskState(t.Status)}})
		}
		return func(c *Claude) { c.tasks = list; c.trimTasks() }
	case "TaskUpdate":
		var in struct {
			TaskID  string `json:"taskId"`
			Status  string `json:"status"`
			Subject string `json:"subject"`
		}
		if json.Unmarshal(raw, &in) != nil || in.TaskID == "" {
			return nil
		}
		return func(c *Claude) {
			for i := range c.tasks {
				if c.tasks[i].id != in.TaskID {
					continue
				}
				if in.Status == "deleted" {
					c.tasks = append(c.tasks[:i:i], c.tasks[i+1:]...)
					return
				}
				if in.Status != "" {
					c.tasks[i].Status = taskState(in.Status)
				}
				if in.Subject != "" {
					c.tasks[i].Subject = clip(in.Subject, maxTitle)
				}
				return
			}
		}
	}
	return nil
}

// created adds the task a TaskCreate made, from Claude Code's account of it
// or, without one, its result's text.
func (c *Claude) created(account json.RawMessage, text string) {
	var a struct {
		Task struct {
			ID      string `json:"id"`
			Subject string `json:"subject"`
		} `json:"task"`
	}
	id, subject := "", ""
	if json.Unmarshal(account, &a) == nil && a.Task.ID != "" {
		id, subject = a.Task.ID, a.Task.Subject
	} else if m := taskCreated.FindStringSubmatch(text); m != nil {
		id, subject = m[1], m[2]
	}
	if id == "" {
		return
	}
	c.tasks = append(c.tasks, taskItem{id: clip(id, maxID), Task: Task{Subject: clip(subject, maxTitle), Status: "pending"}})
	c.trimTasks()
}

func (c *Claude) trimTasks() {
	if n := len(c.tasks) - maxTasks; n > 0 {
		c.tasks = append([]taskItem(nil), c.tasks[n:]...)
	}
}

// resultText: what came back from a call, as words; images and refs count
// what came back that is not words (pictures read, tools found), for the
// view to say in the language it is read in.
func resultText(raw json.RawMessage) (text string, images, refs int) {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return clean(s), 0, 0
	}
	var blocks []claudeBlock
	if json.Unmarshal(raw, &blocks) != nil {
		return "", 0, 0
	}
	var parts []string
	for _, b := range blocks {
		switch b.Type {
		case "text":
			parts = append(parts, b.Text)
		case "image":
			images++ // (a picture read: not shown here, but that it came back is)
		case "tool_reference":
			refs++ // (tools found by a search)
		}
	}
	return clean(strings.Join(parts, "\n")), images, refs
}

// Envelopes in user rows. Claude Code wraps what it writes there for its own
// purposes in tags; the person's own text that happens to start with "<" is
// kept as it is.
var (
	envelopeTag   = regexp.MustCompile(`^\s*<([a-z][a-z0-9-]*)[\s>]`)
	taskSummary   = regexp.MustCompile(`(?s)<summary>(.*?)</summary>`)
	taskStatus    = regexp.MustCompile(`(?s)<status>(.*?)</status>`)
	taskID        = regexp.MustCompile(`(?s)<task-id>(.*?)</task-id>`)
	crossSession  = regexp.MustCompile(`^\s*<cross-session-message[^>]*\bfrom-name="([^"]*)"`)
	taskEvent     = regexp.MustCompile(`(?s)<event>(.*?)</event>`)
	taskOutFile   = regexp.MustCompile(`(?s)<output-file>(.*?)</output-file>`)
	commandName   = regexp.MustCompile(`(?s)<command-name>(.*?)</command-name>`)
	commandArgs   = regexp.MustCompile(`(?s)<command-args>(.*?)</command-args>`)
	tagContent    = regexp.MustCompile(`(?s)^\s*<([a-z-]+)>(.*?)</[a-z-]+>\s*$`)
	pastedContent = regexp.MustCompile(`(?s)<pasted_content id="([^"]*)">\n?(.*?)\n?</pasted_content id="([^"]*)">`)
	imageToken    = regexp.MustCompile(`\[Image #\d+\]`)
	ansiSequence  = regexp.MustCompile("\x1b\\[[0-9;?]*[ -/]*[@-~]|\x1b\\][^\x07\x1b]*(\x07|\x1b\\\\)|\x1b[@-_]")
)

// classifyUserText says what a user row's text is: the person's ("user"),
// a note, or nothing to show (""), and the text to show for it.
func classifyUserText(s string) (role, text string) {
	// (What Claude Code writes when it was stopped: a note, not the person's.)
	if t := strings.TrimSpace(s); strings.HasPrefix(t, "[Request interrupted by user") && strings.HasSuffix(t, "]") && len(t) < 80 {
		return RoleNote, t
	}
	m := envelopeTag.FindStringSubmatch(s)
	if m == nil {
		if strings.TrimSpace(s) == "" {
			return "", ""
		}
		return RoleUser, unpasted(s)
	}
	switch m[1] {
	case "command-message":
		// (Its name may follow: the command, as when the name comes first.)
		if text, ok := slashCommand(s); ok {
			return RoleUser, text
		}
		return "", ""
	case "system-reminder", "local-command-caveat", "user-memory-input":
		return "", ""
	// Another session's agent writing to this one: its words, not the
	// person's (the envelope says the socket it came by and the hops it
	// took, which is nothing to read).
	case "cross-session-message":
		if t := clean(bodyAfterTag(s, "cross-session-message")); t != "" {
			return RoleNote, t
		}
		return "", ""
	// (A subagent handing its report back to the session that sent it: the
	// agent's own, not the person's, though it waits in the queue as one.)
	case "agent-message":
		return "", ""
	case "command-name":
		if text, ok := slashCommand(s); ok {
			return RoleUser, text
		}
		return "", ""
	case "local-command-stdout", "local-command-stderr", "bash-stdout", "bash-stderr":
		if t := strings.TrimSpace(clean(inner(s))); t != "" {
			return RoleNote, t
		}
		return "", ""
	case "bash-input":
		return RoleUser, "! " + strings.TrimSpace(inner(s))
	case "task-notification":
		if sm := taskSummary.FindStringSubmatch(s); sm != nil {
			return RoleNote, strings.TrimSpace(sm[1])
		}
		if st := taskStatus.FindStringSubmatch(s); st != nil {
			return RoleNote, strings.TrimSpace(st[1])
		}
		return "", ""
	}
	return RoleUser, unpasted(s)
}

// unpasted is the text with what Claude Code wraps a paste in taken off
// (<pasted_content id="…">…</pasted_content id="…">, the same id at both
// ends): as it was typed.
// (The wrapper comes with newlines of its own around it, and the paste's
// own at its ends: blank lines at the message's start and end are let go,
// what is indented at its first line's start kept.)
func unpasted(s string) string {
	if !strings.Contains(s, "<pasted_content id=") {
		return s
	}
	s = pastedContent.ReplaceAllStringFunc(s, func(w string) string {
		m := pastedContent.FindStringSubmatch(w)
		if m[1] != m[3] {
			return w
		}
		return m[2]
	})
	return blankEnds(s)
}

// blankEnds is a text without blank lines at its start and end (what is
// indented at its first line's start kept).
func blankEnds(s string) string {
	return strings.TrimRight(blankLead.ReplaceAllString(s, ""), " \t\r\n")
}

// (Blank lines at a text's start.)
var blankLead = regexp.MustCompile(`^(?:[ \t\r]*\n)+`)

// noImageTokens is a message's text without the "[Image #N]" Claude Code
// puts in for each picture it carries (the pictures are parts of their own).
func noImageTokens(s string) string {
	t := imageToken.ReplaceAllString(s, "")
	if t == s {
		return s
	}
	return strings.TrimSpace(t)
}

func slashCommand(s string) (string, bool) {
	m := commandName.FindStringSubmatch(s)
	if m == nil {
		return "", false
	}
	name := strings.TrimSpace(m[1])
	if name == "" {
		return "", false
	}
	if a := commandArgs.FindStringSubmatch(s); a != nil && strings.TrimSpace(a[1]) != "" {
		name += " " + strings.TrimSpace(a[1])
	}
	return name, true
}

func inner(s string) string {
	if m := tagContent.FindStringSubmatch(s); m != nil {
		return m[2]
	}
	return s
}

// toolInput picks what to show of a call's input: what it is for and what
// it acts on.
func toolInput(name string, raw json.RawMessage) (title, detail, old, new string) {
	var in map[string]any
	if json.Unmarshal(raw, &in) != nil {
		return "", "", "", ""
	}
	str := func(k string) string {
		v, _ := in[k].(string)
		return v
	}
	d := func(s string) string { s, _ = cut(clean(s), maxDetail); return s }
	switch name {
	case "Bash":
		return d(str("description")), d(str("command")), "", ""
	case "Read", "Write", "Edit", "MultiEdit", "NotebookEdit":
		p := str("file_path")
		if p == "" {
			p = str("notebook_path")
		}
		switch name {
		case "Edit":
			return "", d(p), d(str("old_string")), d(str("new_string"))
		case "Write":
			return "", d(p), "", d(str("content"))
		case "MultiEdit":
			var olds, news []string
			if edits, ok := in["edits"].([]any); ok {
				for _, e := range edits {
					if m, ok := e.(map[string]any); ok {
						o, _ := m["old_string"].(string)
						n, _ := m["new_string"].(string)
						olds, news = append(olds, o), append(news, n)
					}
				}
			}
			return "", d(p), d(strings.Join(olds, "\n…\n")), d(strings.Join(news, "\n…\n"))
		}
		return "", d(p), "", ""
	case "Grep", "Glob":
		s := str("pattern")
		if p := str("path"); p != "" {
			s += "  " + p
		}
		return "", d(s), "", ""
	case "WebFetch":
		return "", d(str("url")), "", ""
	case "WebSearch", "ToolSearch":
		return "", d(str("query")), "", ""
	case "Agent", "Task":
		return d(str("description")), d(str("prompt")), "", ""
	case "Skill":
		return "", d(str("skill")), "", ""
	case "Monitor":
		return d(str("description")), d(str("command")), "", ""
	case "TaskStop":
		return "", d(str("task_id")), "", ""
	case "Artifact":
		// Which of the tool's doings it is (publishing, when it does not say).
		act := str("action")
		if act == "" {
			act = "publish"
		}
		return "", d(strings.TrimSpace(act + " " + told(in, "title", "file_path", "url", "type_url", "intent"))), "", ""
	case "ArtifactData":
		return "", d(strings.TrimSpace(str("action") + " " + told(in, "collection", "doc_id", "url"))), "", ""
	case "SendMessage":
		return d(str("to")), d(told(in, "summary", "message", "content")), "", ""
	case "ScheduleWakeup":
		return "", d(told(in, "reason", "prompt")), "", ""
	case "CronCreate", "CronDelete", "CronList":
		return "", d(strings.TrimSpace(str("cron") + " " + told(in, "prompt", "id"))), "", ""
	case "ListAgents", "advisor":
		return "", "", "", "" // (nothing of its own to say: its line is the verb)
	case "TaskCreate", "TaskUpdate":
		s := str("subject")
		if name == "TaskUpdate" {
			s = strings.TrimSpace("#" + str("taskId") + " " + str("status") + " " + s)
		}
		return "", d(s), "", ""
	case "TodoWrite":
		var lines []string
		if todos, ok := in["todos"].([]any); ok {
			for _, t := range todos {
				if m, ok := t.(map[string]any); ok {
					s, _ := m["content"].(string)
					mark := "[ ]"
					switch m["status"] {
					case "completed":
						mark = "[x]"
					case "in_progress":
						mark = "[~]"
					}
					lines = append(lines, mark+" "+s)
				}
			}
		}
		return "", d(strings.Join(lines, "\n")), "", ""
	case "AskUserQuestion":
		var qs []string
		if list, ok := in["questions"].([]any); ok {
			for _, q := range list {
				if m, ok := q.(map[string]any); ok {
					s, _ := m["question"].(string)
					qs = append(qs, s)
				}
			}
		}
		return "", d(strings.Join(qs, "\n")), "", ""
	}
	// An MCP server's tool: its own name (most of the whole one is the
	// server's) and what it acts on. A browser's doing says "browser" on
	// its line already, so its name keeps only the doing.
	if _, tool, ok := mcpTool(name); ok {
		tool = strings.TrimPrefix(tool, "browser_")
		return "", d(strings.TrimSpace(tool + " " + anyText(in))), "", ""
	}
	// Another tool nothing here knows: what it acts on, if its input says.
	return "", d(anyText(in)), "", ""
}

// mcpTool splits an MCP server's tool name (mcp__<server>__<tool>) into the
// server and the tool's own name.
func mcpTool(name string) (server, tool string, ok bool) {
	rest, ok := strings.CutPrefix(name, "mcp__")
	if !ok {
		return "", "", false
	}
	i := strings.LastIndex(rest, "__")
	if i <= 0 || i+2 >= len(rest) {
		return "", rest, rest != ""
	}
	return rest[:i], rest[i+2:], true
}

// toldFirst are the fields that say what a call acts on, most telling
// first: a tool nothing here knows is read by them before anything else
// its input holds (which is how "engine", "level" or "colorScheme" used to
// end up on the line).
var toldFirst = []string{
	"url", "path", "file_path", "filename", "file", "dir", "query", "q", "sql",
	"command", "cmd", "text", "prompt", "message", "content", "summary", "name",
	"app", "page_id", "doc_id", "id", "element", "key", "pattern", "regex",
	"code", "function", "collection", "skill", "reason", "cron", "task_id",
	"action", "kind", "intent",
}

// told is the first of the given fields the input holds.
func told(in map[string]any, keys ...string) string {
	for _, k := range keys {
		if s, ok := in[k].(string); ok && strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
}

// anyText is what an unknown tool's input says it acts on: the most telling
// field it holds, else its first text field by name.
func anyText(in map[string]any) string {
	if s := told(in, toldFirst...); s != "" {
		return s
	}
	keys := make([]string, 0, len(in))
	for k := range in {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if s, ok := in[k].(string); ok && strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
}

func textPart(kind, s string) Part {
	s, trunc := cut(clean(s), maxText)
	return Part{Kind: kind, Text: s, Truncated: trunc}
}

// clean makes text from the log safe to show as text: valid UTF-8, without
// terminal escape sequences, control characters other than tab and newline,
// or the direction overrides that make text read other than it is.
func clean(s string) string {
	s = strings.ToValidUTF8(s, "�")
	if strings.IndexByte(s, 0x1b) >= 0 {
		s = ansiSequence.ReplaceAllString(s, "")
	}
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' {
			return r
		}
		if r == '\r' {
			return -1
		}
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) || (r >= 0x202a && r <= 0x202e) || (r >= 0x2066 && r <= 0x2069) {
			return -1
		}
		return r
	}, s)
}

// cut shortens s to at most n bytes, at a character boundary. What it keeps
// is a copy: a slice of a long line would keep the whole line in memory.
func cut(s string, n int) (string, bool) {
	if len(s) <= n {
		return strings.Clone(s), false
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return strings.Clone(s[:n]), true
}

func clip(s string, n int) string {
	s, _ = cut(clean(s), n)
	return s
}

// What went wrong that the thread would otherwise not show at all. Claude
// Code writes each of these as a row of its own and says it on the screen;
// without them the agent is seen falling silent, or answering half a
// sentence, with nothing said of why.
//
// (An answer stopped by the model's safeguards also has stop_reason
// "refusal" and no words in it; of 13 sessions where that happened, every
// one had the system row too, so that row alone is shown.)
func (c *Claude) systemTrouble(r *claudeRow) *Trouble {
	switch r.Subtype {
	case "model_refusal_fallback", "model_refusal_no_fallback":
		// (Its own words first, as Claude Code shows them; else what the
		// interface said of it.)
		text := ""
		var said string
		if json.Unmarshal(r.Content, &said) == nil {
			text = strings.TrimSpace(said)
		}
		if text == "" {
			text = strings.TrimSpace(r.APIRefusalExplanation)
		}
		said, cutOff := cut(text, maxTrouble)
		t := &Trouble{Kind: TroubleRefused, Text: said, Cut: cutOff}
		if m := strings.TrimSpace(r.FallbackModel); m != "" && r.Subtype == "model_refusal_fallback" {
			t.Name, t.Again = clip(m, maxMeta), true
		}
		return t
	case "api_error":
		var e struct {
			Message   string `json:"message"`
			Formatted string `json:"formatted"`
			Status    int    `json:"status"`
		}
		if len(r.Error) == 0 || json.Unmarshal(r.Error, &e) != nil {
			return nil
		}
		text := strings.TrimSpace(e.Formatted)
		if text == "" {
			text = strings.TrimSpace(e.Message)
		}
		if text == "" && e.Status == 0 {
			return nil
		}
		said, cutOff := cut(text, maxTrouble)
		t := &Trouble{Kind: TroubleAPI, Text: said, Status: e.Status, Cut: cutOff}
		if a, m := count(r.RetryAttempt), count(r.MaxRetries); a > 0 && m >= a && m < 1000 {
			t.Attempt, t.Max, t.RetryMs = int(a), int(m), count(r.RetryInMs)
		}
		return t
	case "stop_hook_summary":
		// (Written after every turn, almost always with nothing to say:
		// only one that failed or held the agent back is shown.)
		text := ""
		for _, e := range r.HookErrors {
			if e = strings.TrimSpace(e); e != "" {
				text = e
				break
			}
		}
		if text == "" && !r.PreventedContinuation {
			return nil
		}
		said, cutOff := cut(text, maxTrouble)
		return &Trouble{Kind: TroubleHook, Text: said, Cut: cutOff, Again: r.PreventedContinuation}
	}
	return nil
}

// scheduledTurn: a scheduled task's prompt, written as a message of the
// person's (not as Claude Code's own, as it mostly is): not theirs, but its
// firing, told as such (Echo: where its firing was written just before it,
// the view shows one of the two, whatever part of the log each was read in).
func (c *Claude) scheduledTurn(r *claudeRow, text string) {
	said, cutOff := cut(text, maxTrouble)
	c.add(r, RoleNote, []Part{{Kind: KindEvent, Event: &Event{Kind: EventScheduled, Text: said, Cut: cutOff, Echo: true}}})
}

// systemEvent: what a system row tells of the session itself (see Event),
// else nil.
func systemEvent(r *claudeRow) *Event {
	switch r.Subtype {
	case "compact_boundary":
		var m struct {
			Trigger    json.RawMessage `json:"trigger"`
			PreTokens  json.RawMessage `json:"preTokens"`
			PostTokens json.RawMessage `json:"postTokens"`
			DurationMs json.RawMessage `json:"durationMs"`
		}
		_ = json.Unmarshal(r.CompactMetadata, &m)
		trigger := word(m.Trigger)
		if trigger != "auto" && trigger != "manual" {
			trigger = ""
		}
		return &Event{Kind: EventCompact, Trigger: trigger, Pre: count(m.PreTokens), Post: count(m.PostTokens), Ms: count(m.DurationMs)}
	case "informational":
		var s string
		if json.Unmarshal(r.Content, &s) != nil {
			return nil
		}
		switch {
		case strings.HasPrefix(s, "Compacted while idle"):
			return &Event{Kind: EventIdleCompact}
		case strings.HasPrefix(s, "Usage limit reached") && strings.Contains(s, "continuing automatically"):
			return &Event{Kind: EventLimitWait}
		case strings.HasPrefix(s, "Usage limit reset"):
			return &Event{Kind: EventLimitReset}
		case strings.HasPrefix(s, "Automatic continue cancelled"):
			said, cutOff := cut(clean(s), maxTrouble)
			return &Event{Kind: EventLimitCancelled, Text: said, Cut: cutOff}
		}
		// (Any other: as it was said, how loud it was.)
		if t := strings.TrimSpace(s); t != "" {
			said, cutOff := cut(clean(t), maxNotice)
			level := word(r.Level)
			if level != "warning" {
				level = "notice"
			}
			return &Event{Kind: EventNotice, Text: said, Cut: cutOff, Level: level}
		}
	case "away_summary":
		var s string
		if json.Unmarshal(r.Content, &s) == nil && strings.TrimSpace(s) != "" {
			said, cutOff := cut(clean(strings.TrimSpace(s)), maxRecap)
			return &Event{Kind: EventRecap, Text: said, Cut: cutOff}
		}
	case "scheduled_task_fire":
		var s string
		_ = json.Unmarshal(r.Prompt, &s)
		said, cutOff := cut(clean(strings.TrimSpace(s)), maxTrouble)
		return &Event{Kind: EventScheduled, Text: said, Cut: cutOff, Loop: word(r.TaskKind) == "loop"}
	}
	return nil
}

// limitOf: an error row of Claude Code's that says the plan's usage limit
// was reached ({status: "rejected", resetsAt}), as an event; else nil (any
// other error, a rate limit of the interface's own, shows as it is).
func limitOf(r *claudeRow) *Event {
	if !r.IsAPIErrorMessage || word(r.Error) != "rate_limit" || len(r.QuotaLimits) == 0 {
		return nil
	}
	var q struct {
		Status        json.RawMessage `json:"status"`
		ResetsAt      json.RawMessage `json:"resetsAt"`
		RateLimitType json.RawMessage `json:"rateLimitType"`
	}
	if json.Unmarshal(r.QuotaLimits, &q) != nil || word(q.Status) != "rejected" {
		return nil
	}
	at := count(q.ResetsAt)
	if at == 0 {
		return nil
	}
	var text string
	var blocks []claudeBlock
	if json.Unmarshal(r.Message.Content, &blocks) == nil {
		for _, b := range blocks {
			if b.Type == "text" {
				text = b.Text
				break
			}
		}
	}
	said, cutOff := cut(clean(strings.TrimSpace(text)), maxTrouble)
	return &Event{Kind: EventLimit, ResetsAt: at, LimitKind: word(q.RateLimitType), Text: said, Cut: cutOff}
}

// goalOf: a goal_status attachment as an event: the goal set (sentinel),
// or judged met or not, with why; nil without a condition.
func goalOf(met, sentinel, condition, reason json.RawMessage) *Event {
	var cond, why string
	_ = json.Unmarshal(condition, &cond)
	_ = json.Unmarshal(reason, &why)
	if strings.TrimSpace(cond) == "" {
		return nil
	}
	var m, s bool
	_ = json.Unmarshal(met, &m)
	_ = json.Unmarshal(sentinel, &s)
	g, cutG := cut(clean(strings.TrimSpace(cond)), maxRecap)
	w, cutW := cut(clean(strings.TrimSpace(why)), maxRecap)
	return &Event{Kind: EventGoal, Goal: g, Set: s && !m, Met: m, Why: w, Cut: cutG || cutW}
}

// count: a number a row says of something counted (0 for none, or one
// that cannot be a count: not a number, below 0, past any real count).
func count(raw json.RawMessage) int64 {
	var f float64
	if json.Unmarshal(raw, &f) != nil || f < 0 || f > 1<<50 {
		return 0
	}
	return int64(f)
}

// word: a short string a row says ("" for none, or one not a string).
func word(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return ""
	}
	return clip(s, maxMeta)
}

// hookTrouble: a hook of the person's that failed, by the kind of row
// Claude Code wrote for it. nil for anything else (a hook that worked, or
// one with something to say, is no trouble of theirs).
func hookTrouble(kind, name, event, stderr string, exit int, timedOut bool, timeout int64) *Trouble {
	which := strings.TrimSpace(name)
	if which == "" {
		which = strings.TrimSpace(event)
	}
	switch kind {
	case "hook_cancelled":
		t := &Trouble{Kind: TroubleHook, Name: clip(which, maxMeta)}
		if timedOut && timeout > 0 {
			t.Secs = int(min((timeout+999)/1000, 1<<20)) // (what it was given, in whole seconds)
		}
		return t
	case "hook_non_blocking_error":
		said, cutOff := cut(strings.TrimSpace(stderr), maxTrouble)
		return &Trouble{Kind: TroubleHook, Name: clip(which, maxMeta), Text: said, Cut: cutOff, Status: exit}
	}
	return nil
}

// subagentOf: the subagent a call sent off (Agent, Task), by its id as
// Claude Code's account of the call gives it; "" for none, or one not of
// the form of its log's name.
func subagentOf(account json.RawMessage) string {
	if len(account) == 0 {
		return ""
	}
	var a struct {
		Agent json.RawMessage `json:"agentId"`
	}
	if json.Unmarshal(account, &a) != nil {
		return ""
	}
	if id := word(a.Agent); SubagentID(id) {
		return id
	}
	return ""
}

// SubagentID: an id of a subagent, as its log is named (agent-<id>.jsonl).
func SubagentID(id string) bool {
	if len(id) < 6 || len(id) > 40 {
		return false
	}
	for _, r := range id {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9') {
			return false
		}
	}
	return true
}

// asksOf: the questions of an AskUserQuestion's input, within bounds.
func asksOf(input json.RawMessage) []Ask {
	var in struct {
		Questions []struct {
			Header      string `json:"header"`
			Question    string `json:"question"`
			MultiSelect bool   `json:"multiSelect"`
			Options     []struct {
				Label       string `json:"label"`
				Description string `json:"description"`
				Preview     string `json:"preview"`
			} `json:"options"`
		} `json:"questions"`
	}
	if json.Unmarshal(input, &in) != nil {
		return nil
	}
	var out []Ask
	for i, q := range in.Questions {
		if i == maxAsks {
			for j := range out { // (more than are kept: none of them is drawn from here)
				out[j].Cut = true
			}
			break
		}
		a := Ask{Header: clip(q.Header, maxMeta), Multi: q.MultiSelect}
		var cutQ bool
		a.Question, cutQ = cut(clean(q.Question), maxTitle)
		a.Cut = cutQ || len(q.Options) > maxOptions
		for j, o := range q.Options {
			if j == maxOptions {
				break
			}
			label, cutL := cut(clean(o.Label), maxTitle)
			p, cutP := cut(clean(o.Preview), maxPreview)
			a.Cut = a.Cut || cutL
			a.Options = append(a.Options, AskOption{Label: label, Description: clip(o.Description, maxTitle), Preview: p, PreviewCut: cutP})
		}
		out = append(out, a)
	}
	return out
}

// answersOf: what was answered to each question, as Claude Code's account
// of the call says it ({answers: {question: answer}}).
func answersOf(asks []Ask, account json.RawMessage) []string {
	var acc struct {
		Answers map[string]string `json:"answers"`
	}
	if json.Unmarshal(account, &acc) != nil || len(acc.Answers) == 0 {
		return nil
	}
	// (By the question as it is kept: cut and cleaned as it was; two that
	// are kept alike are told by neither.)
	got := make(map[string]string, len(acc.Answers))
	alike := map[string]bool{}
	for q, v := range acc.Answers {
		k, _ := cut(clean(q), maxTitle)
		if _, ok := got[k]; ok {
			alike[k] = true
		}
		got[k] = v
	}
	for k := range alike {
		delete(got, k)
	}
	kept := map[string]int{}
	for _, a := range asks {
		kept[a.Question]++
	}
	out := make([]string, len(asks))
	for i, a := range asks {
		if kept[a.Question] == 1 {
			out[i] = clip(got[a.Question], maxTitle)
		}
	}
	return out
}

// planOf: the plan an ExitPlanMode puts to the person (Claude Code writes
// it into the call's input, from the plan's file), and that file.
func planOf(input json.RawMessage) (string, bool, string) {
	var in struct {
		Plan         string `json:"plan"`
		PlanFilePath string `json:"planFilePath"`
	}
	if json.Unmarshal(input, &in) != nil {
		return "", false, ""
	}
	p, cutP := cut(clean(in.Plan), maxPlan)
	return p, cutP, clip(in.PlanFilePath, maxDetail)
}

// costOf reads a cost-state row. Its modelUsage is an object by model, or
// (some versions) that object as a string.
func costOf(r *claudeRow) *Cost {
	c := &Cost{USD: r.TotalCostUSD, Unknown: r.HasUnknownModelCost, Added: r.TotalLinesAdded, Removed: r.TotalLinesRemoved, APIMs: r.TotalAPIDuration}
	if r.StartTime > 0 && r.TotalDuration > 0 {
		c.At = r.StartTime + r.TotalDuration
	}
	raw := r.ModelUsage
	var s string
	if json.Unmarshal(raw, &s) == nil {
		raw = json.RawMessage(s)
	}
	var by map[string]struct {
		In         int64   `json:"inputTokens"`
		Out        int64   `json:"outputTokens"`
		CacheRead  int64   `json:"cacheReadInputTokens"`
		CacheWrite int64   `json:"cacheCreationInputTokens"`
		USD        float64 `json:"costUSD"`
	}
	if json.Unmarshal(raw, &by) == nil {
		for m, u := range by {
			c.Models = append(c.Models, ModelCost{Model: clip(m, maxMeta), In: u.In, Out: u.Out, CacheRead: u.CacheRead, CacheWrite: u.CacheWrite, USD: u.USD})
		}
		sort.Slice(c.Models, func(i, j int) bool {
			if c.Models[i].USD != c.Models[j].USD {
				return c.Models[i].USD > c.Models[j].USD
			}
			return c.Models[i].Model < c.Models[j].Model
		})
		if len(c.Models) > maxCostModels {
			c.Models = c.Models[:maxCostModels]
		}
	}
	return c
}

// shellOutput reads what a "!" command printed: Claude Code's
// <bash-stdout>…</bash-stdout><bash-stderr>…</bash-stderr> (its text with
// &lt; &gt; &amp; written for < > &, as in XML); ok false where s is not
// that, sh nil where nothing was printed.
func shellOutput(s string) (sh *Shell, ok bool) {
	t := strings.TrimSpace(s)
	if !strings.HasPrefix(t, "<bash-stdout>") && !strings.HasPrefix(t, "<bash-stderr>") {
		return nil, false
	}
	of := func(re *regexp.Regexp) string {
		m := re.FindStringSubmatch(t)
		if m == nil {
			return ""
		}
		return strings.TrimRight(clean(html.UnescapeString(m[1])), "\n ")
	}
	out, errs := of(shellStdout), of(shellStderr)
	if strings.TrimSpace(out) == "" && strings.TrimSpace(errs) == "" {
		return nil, true
	}
	var as Tool
	as.setResult(out)
	sh = &Shell{Out: as.Result, OutTail: as.ResultTail, Omitted: as.Omitted, Truncated: as.Truncated}
	if errs != "" {
		sh.Err = clip(errs, maxShellErr)
		sh.ErrCut = len(sh.Err) < len(errs)
	}
	return sh, true
}

var (
	shellStdout = regexp.MustCompile(`(?s)<bash-stdout>(.*?)</bash-stdout>`)
	shellStderr = regexp.MustCompile(`(?s)<bash-stderr>(.*?)</bash-stderr>`)
)
