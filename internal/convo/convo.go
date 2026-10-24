// Package convo reads a coding agent's own conversation log (Claude Code's
// session JSONL) into the turns a conversation view shows: what the person
// wrote, what the agent answered, and the tools it ran with what came back.
//
// The log comes from the host and is untrusted: every field is optional,
// lengths are bounded, unknown rows are skipped (and counted), and nothing
// here interprets text as markup; the browser renders it as text.
package convo

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
	KindImage    = "image" // an image was attached (its bytes are not kept)
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
}

// Queued is a message the person sent while the agent worked, which it has
// not taken in yet.
type Queued struct {
	Text string `json:"text"`
	Time string `json:"ts,omitempty"`
}

type Part struct {
	Kind      string `json:"kind"`
	Text      string `json:"text,omitempty"`
	Truncated bool   `json:"truncated,omitempty"`
	Tool      *Tool  `json:"tool,omitempty"`
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

	Done        bool   `json:"done,omitempty"`
	Error       bool   `json:"error,omitempty"`
	Result      string `json:"result,omitempty"`
	ResultLines int    `json:"result_lines,omitempty"` // of the whole result, before it was cut
	Truncated   bool   `json:"truncated,omitempty"`    // (Result is the start of it)
}

// Bounds on what one entry keeps.
const (
	maxText   = 64 << 10 // a text or thinking part
	maxDetail = 4 << 10  // a tool's detail, old and new text
	maxResult = 6 << 10  // a tool's result
	maxParts  = 64       // parts of one entry
	maxID     = 128
	maxTasks  = 50  // tasks of a task list
	maxTitle  = 300 // the session's title, a task's subject
	maxMeta   = 120 // a model's, a mode's, a branch's name
	maxQueue  = 20  // messages waiting to be taken in
)

// Meta is what a session runs with, as its log last said it: each field
// empty (zero) until a row says it.
type Meta struct {
	Model  string `json:"model,omitempty"`  // the model's id, as the API named it
	Mode   string `json:"mode,omitempty"`   // the permission mode: default, acceptEdits, plan, auto, ...
	Branch string `json:"branch,omitempty"` // the git branch it ran on
	// Context is how many tokens the latest request sent: what the
	// conversation fills of the model's window.
	Context int64 `json:"context,omitempty"`
}

// Task is one item of the agent's task list (TodoWrite, or TaskCreate and
// TaskUpdate), as it stands.
type Task struct {
	Subject string `json:"subject"`
	Status  string `json:"status"` // pending, in_progress, completed
}
