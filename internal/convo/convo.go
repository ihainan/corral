// Package convo reads a coding agent's own conversation log (Claude Code's
// session JSONL) into the turns a conversation view shows: what the person
// wrote, what the agent answered, and the tools it ran with what came back.
//
// The log comes from the host and is untrusted: every field is optional,
// lengths are bounded, unknown rows are skipped (and counted), and nothing
// here interprets text as markup; the browser renders it as text.
package convo

import (
	"strings"
	"unicode/utf8"
)

// Parser reads one agent's log: Claude Code's (Claude) or Codex's (Codex).
type Parser interface {
	Feed(lines []byte)     // whole lines, in order
	Since(rev int) []Entry // the entries changed after revision rev
	Prepend(older Parser)  // a part read before what was read, parsed on its own (of the same kind)
	Rev() int
	First() int // the Seq of the oldest entry kept
	Trimmed() bool
	Title() string
	Meta() Meta
	Tasks() []Task
	Queued() []Queued
	Running() []Running
	QueueSince(started int64)
}

// New is the parser for an agent's log: "codex", "opencode" (its session's
// rows, see OpenCode), else Claude Code's.
func New(agent string) Parser {
	switch agent {
	case "codex":
		return NewCodex()
	case "opencode":
		return NewOpenCode()
	}
	return NewClaude()
}

// Roles of an entry.
const (
	RoleUser      = "user"      // what the person wrote (or queued)
	RoleAssistant = "assistant" // the agent's text, thinking and tool calls
	RoleSummary   = "summary"   // the summary written when the session was compacted
	RoleNote      = "note"      // sent on the person's behalf: a background task ending, a command's output
)

// Part kinds.
const (
	KindText     = "text"
	KindThinking = "thinking"
	KindTool     = "tool"
	KindImage    = "image"   // an image was attached (its bytes are not kept)
	KindNoted    = "noted"   // files the turn was given without anyone saying so
	KindTrouble  = "trouble" // something went wrong that the thread would otherwise not show
	KindEvent    = "event"   // something that happened to the session itself (see Event)
	KindShell    = "shell"   // what a command the person ran with "!" printed (see Shell)
)

// What an Event is of.
const (
	EventCompact     = "compact"      // the conversation was compacted
	EventIdleCompact = "idle_compact" // (said after it) it was so while the session sat idle
	EventScheduled   = "scheduled"    // a scheduled task (or /loop) set a turn going
	// The plan's usage limit (Claude Code's subscription): reached (until
	// ResetsAt, of Kind), to go on of itself once it resets, reset, the
	// going on called off.
	EventLimit          = "limit"
	EventLimitWait      = "limit_wait"
	EventLimitReset     = "limit_reset"
	EventLimitCancelled = "limit_cancelled"
	EventRecap          = "recap"  // Claude Code's line of what went on while the person was away
	EventGoal           = "goal"   // (/goal) a goal set, or judged met or not (Met, Text: why)
	EventNotice         = "notice" // a notice of Claude Code's of any other kind (Level: notice, warning)
)

// What a Trouble is of.
const (
	TroubleRefused = "refused" // the model's safeguards stopped the message
	TroubleAPI     = "api"     // the interface answered with an error
	TroubleCut     = "cut"     // the answer ran into the model's limit
	TroubleHook    = "hook"    // a hook of the person's failed
)

// Entry is one row of the log that the view shows. Seq orders entries;
// Rev is the revision of the parser when the entry last changed (a tool's
// result arrives in a later row and fills in the call), so a reader that
// has seen up to revision r needs only the entries with Rev > r.
type Entry struct {
	Seq   int    `json:"seq"`
	Rev   int    `json:"rev"`
	ID    string `json:"id"`
	Role  string `json:"role"`
	Time  string `json:"ts,omitempty"`
	Parts []Part `json:"parts"`
	// Mid marks a message the person sent while the agent worked, which it
	// took in during its turn (the turn goes on).
	Mid bool `json:"mid,omitempty"`
	// Gone marks what was taken back: the conversation was rewound to
	// before it (Claude Code's Esc Esc or /rewind, a message taken back as
	// it went, Codex's backtrack, OpenCode's /undo), or the model's answer
	// was withdrawn. The agent no longer has it; the log still does. An
	// entry becomes gone later than it came (with a new Rev), never back,
	// but for OpenCode's /redo.
	Gone bool `json:"gone,omitempty"`
}

// Queued is a message the person sent while the agent worked, which it has
// not taken in yet.
type Queued struct {
	Text string `json:"text"`
	Time string `json:"ts,omitempty"`
	// From: another session whose agent sent it, where it is not the
	// person's own message; Note: it is a notice, not anyone's message.
	From string `json:"from,omitempty"`
	Note bool   `json:"note,omitempty"`
}

// Running is work the agent set going and left to itself: a command in the
// background, a subagent sent off on its own. It is there until the agent
// is told it has ended.
type Running struct {
	ID   string `json:"id"`
	What string `json:"what"`           // the command, or what the subagent was sent for
	Kind string `json:"kind"`           // "cmd" or "agent"
	Time string `json:"ts,omitempty"`   // when it was set going
	Note string `json:"note,omitempty"` // the latest it has said of itself, if any
}

type Part struct {
	Kind      string   `json:"kind"`
	Text      string   `json:"text,omitempty"`
	Truncated bool     `json:"truncated,omitempty"`
	Tool      *Tool    `json:"tool,omitempty"`
	Noted     *Noted   `json:"noted,omitempty"`
	Trouble   *Trouble `json:"trouble,omitempty"`
	Event     *Event   `json:"event,omitempty"`
	Shell     *Shell   `json:"shell,omitempty"`
}

// Shell is what a shell command the person ran with "!" printed: its
// output as a tool's result is kept (its start and end, see setResult),
// what it wrote to stderr apart, cut at maxShellErr.
type Shell struct {
	Out       string `json:"out,omitempty"`
	OutTail   string `json:"out_tail,omitempty"`
	Omitted   int    `json:"omitted,omitempty"`
	Truncated bool   `json:"truncated,omitempty"`
	Err       string `json:"err,omitempty"`
	ErrCut    bool   `json:"err_cut,omitempty"`
}

// Event is something that happened to the session itself, not said by
// anyone: without it, the view shows the conversation shrink to a summary
// for no reason, or the agent start a turn nobody asked for.
type Event struct {
	Kind string `json:"kind"` // EventCompact, EventIdleCompact, EventScheduled
	// Compact: by whom ("auto", "manual"), the tokens of the conversation
	// before and after, how long it took.
	Trigger string `json:"trigger,omitempty"`
	Pre     int64  `json:"pre,omitempty"`
	Post    int64  `json:"post,omitempty"`
	Ms      int64  `json:"ms,omitempty"`
	// Scheduled: the task's prompt (Cut: only its start), and whether it is
	// a loop's (/loop) rather than a task set for a time.
	Text string `json:"text,omitempty"`
	Cut  bool   `json:"cut,omitempty"`
	Loop bool   `json:"loop,omitempty"`
	// Echo: (scheduled) told by the turn's own row, not its firing; right
	// after its firing, the same turn told again.
	Echo bool `json:"echo,omitempty"`
	// (goal) the goal's condition, set (Set) or judged (Met, and Why).
	Goal string `json:"goal,omitempty"`
	Set  bool   `json:"set,omitempty"`
	Met  bool   `json:"met,omitempty"`
	Why  string `json:"why,omitempty"`
	// (notice) how loud: "notice", "warning".
	Level string `json:"level,omitempty"`
	// (limit) when it resets (Unix seconds), and of what: "five_hour", …
	ResetsAt  int64  `json:"resets_at,omitempty"`
	LimitKind string `json:"limit_kind,omitempty"`
}

// Trouble is something that went wrong which the thread would otherwise
// not show at all: the agent is seen falling silent, or answering half a
// sentence, with nothing said of why.
type Trouble struct {
	// Kind: "refused" (the model's safeguards stopped the message),
	// "api" (the interface answered with an error), "cut" (the answer ran
	// into the model's limit), "hook" (a hook of the person's failed).
	Kind string `json:"kind"`
	// Text: what was said of it, in the words it was said in.
	Text string `json:"text,omitempty"`
	// Name: whose it is — the model it went on with, the hook that failed.
	Name string `json:"name,omitempty"`
	// Status: the interface's status code, or the hook's exit code.
	Status int `json:"status,omitempty"`
	// Secs: how long a hook was given before it was cut off, in seconds.
	Secs int `json:"secs,omitempty"`
	// Cut: Text is only the start of what was said; the rest is in the
	// pane. (What an interface says of an error can run very long.)
	Cut bool `json:"cut,omitempty"`
	// Again: it was tried again, by the model in Name; without it, the
	// turn was left unanswered.
	Again bool `json:"again,omitempty"`
	// (api) Attempt of Max: it is tried again, that time of as many, in
	// RetryMs.
	Attempt int   `json:"attempt,omitempty"`
	Max     int   `json:"max,omitempty"`
	RetryMs int64 `json:"retry_ms,omitempty"`
}

// Noted is a file the turn was given without it being said in the
// conversation: one that changed while the agent worked (Claude Code tells
// it so), or the ones the person referred to in what they wrote. Without
// it, the agent is seen acting on a change nobody mentioned.
type Noted struct {
	// Kind: "changed" (a file changed, by whom cannot be told) or
	// "brought" (the person referred to it).
	Kind  string   `json:"kind"`
	Paths []string `json:"paths"`
	More  int      `json:"more,omitempty"` // (there were that many more)
}

// Tool is one tool call and, once it has come back, its result.
type Tool struct {
	ID   string `json:"id,omitempty"`
	Name string `json:"name"`
	// Title says what the call is for: Bash's description, a subagent's
	// task; else empty.
	Title string `json:"title,omitempty"`
	// Detail is what it acts on: the command, the file, the pattern, the
	// address; for an edit, the text replaced (Old) and its replacement (New).
	Detail string `json:"detail,omitempty"`
	Old    string `json:"old,omitempty"`
	New    string `json:"new,omitempty"`

	// Add and Del count the lines an edit added and removed: from what
	// Claude Code reported of the edit made, else from its input.
	Add int `json:"add,omitempty"`
	Del int `json:"del,omitempty"`
	// Patch is the edit as Claude Code reported making it: its hunks, with
	// line numbers and the lines around (then Old and New are dropped).
	// PatchCut: there was more of it than is kept.
	Patch    []Hunk `json:"patch,omitempty"`
	PatchCut bool   `json:"patch_cut,omitempty"`
	Same     bool   `json:"same,omitempty"` // (an edit that changed nothing)

	// Files are the files a command changed, as Claude Code reports them
	// (a shell command that edits, since 2.1.274): each with its own
	// patch, as an edit's. MoreFiles: it changed that many more than are
	// here.
	Files     []ToolFile `json:"files,omitempty"`
	MoreFiles int        `json:"more_files,omitempty"`

	Done        bool   `json:"done,omitempty"`
	Error       bool   `json:"error,omitempty"`
	Result      string `json:"result,omitempty"`
	ResultLines int    `json:"result_lines,omitempty"` // of the whole result, before it was cut
	// Truncated: Result is the start of it; ResultTail, when there is one,
	// its end (what a command ends on: its failure, a test run's tally),
	// with Omitted lines left out between, where whole lines are (else 0).
	Truncated  bool   `json:"truncated,omitempty"`
	ResultTail string `json:"result_tail,omitempty"`
	Omitted    int    `json:"omitted,omitempty"`
	// Asks: an AskUserQuestion's questions, as it asked them; Answers,
	// once answered, what was answered to each (in the same order; "" for
	// one not answered).
	Asks    []Ask    `json:"asks,omitempty"`
	Answers []string `json:"answers,omitempty"`
	// AgentID: (Agent, Task) the subagent it sent off, whose own log can be
	// read (see NewClaudeSub).
	AgentID string `json:"agent_id,omitempty"`
	// Plan: (ExitPlanMode) the plan put to the person, as Claude Code
	// wrote it in the call (PlanCut: only its start), and its file.
	Plan     string `json:"plan,omitempty"`
	PlanCut  bool   `json:"plan_cut,omitempty"`
	PlanPath string `json:"plan_path,omitempty"`

	// What came back that is not words: pictures read (a screenshot, a
	// PDF's pages), tools a search found.
	Images int `json:"images,omitempty"`
	Refs   int `json:"refs,omitempty"`

	// fromCall: (Codex) this line is of a call the agent made, which tells
	// of the commands it means to run; where it tells of each command on
	// its own as well, those are what is shown and this goes.
	fromCall bool
}

// Ask is one question of an AskUserQuestion: its tab's header, the
// question, whether more than one choice may be ticked, its choices. Cut:
// more of it than is kept (the view then reads it off the screen).
type Ask struct {
	Header   string      `json:"header,omitempty"`
	Question string      `json:"question"`
	Multi    bool        `json:"multi,omitempty"`
	Options  []AskOption `json:"options"`
	Cut      bool        `json:"cut,omitempty"`
}

// AskOption is a choice of a question: its label, what it means, and what
// it shows beside (a preview: code, a sketch).
type AskOption struct {
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
	Preview     string `json:"preview,omitempty"`
	PreviewCut  bool   `json:"preview_cut,omitempty"`
}

// Hunk is a run of an edit's lines: each begins with " " (kept), "-"
// (removed) or "+" (added); OldStart and NewStart are the first line's
// numbers before and after.
// ToolFile is one file a call changed.
type ToolFile struct {
	Path     string `json:"path"`
	Add      int    `json:"add,omitempty"`
	Del      int    `json:"del,omitempty"`
	Patch    []Hunk `json:"patch,omitempty"`
	PatchCut bool   `json:"patch_cut,omitempty"`
}

type Hunk struct {
	OldStart int      `json:"old_start"`
	NewStart int      `json:"new_start"`
	Lines    []string `json:"lines"`
}

// Bounds on what one entry keeps.
const (
	maxText      = 64 << 10 // a text or thinking part
	maxDetail    = 4 << 10  // a tool's detail, old and new text
	maxResult    = 6 << 10  // a tool's result (its start and end)
	maxShellErr  = 2 << 10  // what a "!" command wrote to stderr
	maxHead      = 2 << 10  // of which its start
	maxParts     = 64       // parts of one entry
	maxID        = 128
	maxTasks     = 50       // tasks of a task list
	maxTitle     = 300      // the session's title, a task's subject
	maxMeta      = 120      // a model's, a mode's, a branch's name
	maxQueue     = 20       // messages waiting to be taken in
	maxPatch     = 12 << 10 // an edit's patch, all its lines
	maxFiles     = 20       // the files of one call whose patches are kept
	maxRunning   = 50       // work set going that is kept
	maxSeenItems = 4096     // (Codex) the things told of as done that are remembered
	maxPatchLine = 400      // one line of it
	maxTrouble   = 600      // of what was said of something that went wrong
	maxNoted     = 8        // files one note of them names (the rest are counted)
	maxWrote     = 4096     // the files written that are remembered (see Noted)
	maxSaid      = 16 << 10 // of the person's last message, what is read for the files it names
	maxAsks      = 4        // questions of one AskUserQuestion
	maxOptions   = 8        // choices of one question
	maxPreview   = 2 << 10  // a choice's preview
	maxPlan      = 32 << 10 // a plan put to the person
	maxRecap     = 1200     // a recap of what went on, a goal and why
	maxNotice    = 300      // a notice of Claude Code's
)

// Meta is what a session runs with, as its log last said it: each field
// empty (zero) until a row says it.
type Meta struct {
	Model string `json:"model,omitempty"` // the model's id, as the API named it
	Mode  string `json:"mode,omitempty"`  // the permission mode: default, acceptEdits, plan, auto, ...
	// Effort is how hard the model is asked to think (Claude Code's
	// /effort, its status line's "·H"): low, medium, high, xhigh, max.
	Effort string `json:"effort,omitempty"`
	Branch string `json:"branch,omitempty"` // the git branch it ran on
	// Context is how many tokens the latest request sent: what the
	// conversation fills of the model's window.
	Context int64 `json:"context,omitempty"`
	// Window is the model's context window, where it is known: as the log
	// says it (Codex's), or of the model and what its requests asked for
	// (Claude Code's, whose log does not say it).
	Window int64 `json:"window,omitempty"`
	// WindowBy says how Window is known: "log", "model" (its model's
	// window), "request" (its requests asked for the 1M window).
	WindowBy string `json:"window_by,omitempty"`
	// Cost is the latest of what the session used, as Claude Code's log
	// last said it in what was read (its cost-state rows: running totals,
	// not added up); nil, none read.
	Cost *Cost `json:"cost,omitempty"`
}

// Cost is a cost-state row of Claude Code's: what the session used so far.
// USD is Claude Code's own estimate, at list prices.
type Cost struct {
	USD     float64 `json:"usd"`
	Unknown bool    `json:"unknown,omitempty"` // some model's price not known: USD short of it
	Added   int64   `json:"added,omitempty"`   // lines
	Removed int64   `json:"removed,omitempty"`
	APIMs   int64   `json:"api_ms,omitempty"` // time waiting on the API
	// At is about when it was written (unix ms): the session's start and
	// how long it has run, as the row says them (it has no time of its own).
	At     int64       `json:"at,omitempty"`
	Models []ModelCost `json:"models,omitempty"` // the costliest first, at most maxCostModels
}

// ModelCost is one model's part of a Cost.
type ModelCost struct {
	Model      string  `json:"model"`
	In         int64   `json:"in,omitempty"`
	Out        int64   `json:"out,omitempty"`
	CacheRead  int64   `json:"cache_read,omitempty"`
	CacheWrite int64   `json:"cache_write,omitempty"`
	USD        float64 `json:"usd,omitempty"`
}

const maxCostModels = 8

// Task is one item of the agent's task list (TodoWrite, or TaskCreate and
// TaskUpdate), as it stands.
type Task struct {
	Subject string `json:"subject"`
	Status  string `json:"status"` // pending, in_progress, completed
}

// setResult keeps what a tool gave back within maxResult: all of it, else
// its start (maxHead) and its end (the rest), each cut at a line where one
// ends in the latter half of its share, else within a line.
func (t *Tool) setResult(text string) {
	t.Result, t.ResultTail, t.Omitted, t.Truncated = "", "", 0, false
	if len(text) <= maxResult {
		t.Result = strings.Clone(text)
		return
	}
	head, headLine := maxHead, false
	if i := strings.LastIndexByte(text[:head], '\n'); i >= head/2 {
		head, headLine = i+1, true
	} else {
		for head > 0 && !utf8.RuneStart(text[head]) {
			head--
		}
	}
	from, tailLine := len(text)-(maxResult-maxHead), false
	if text[from-1] == '\n' {
		tailLine = true // (at a line's start already)
	} else if j := strings.IndexByte(text[from:], '\n'); j >= 0 && j < (maxResult-maxHead)/2 && from+j+1 < len(text) {
		from, tailLine = from+j+1, true
	} else {
		for from < len(text) && !utf8.RuneStart(text[from]) {
			from++
		}
	}
	t.Result, t.ResultTail, t.Truncated = strings.Clone(text[:head]), strings.Clone(text[from:]), true
	if headLine && tailLine {
		t.Omitted = strings.Count(text[head:from], "\n")
	}
}
