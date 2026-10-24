package convo

import (
	"bytes"
	"encoding/json"
	"regexp"
	"sort"
	"strings"
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
//     are not written as user rows).
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
	entries []*Entry
	tools   map[string]toolRef
	rev     int
	seq     int
	aiTitle string
	myTitle string     // (the person's, over Claude Code's)
	tasks   []taskItem // the task list as it stands
	meta    Meta
	// Skipped counts lines that were not JSON or of no use to the view.
	Skipped int
}

type toolRef struct {
	e *Entry
	t *Tool
	// task changes the task list, once the call has come back without an
	// error (TodoWrite, TaskUpdate; TaskCreate from its result).
	task func(c *Claude)
}

type taskItem struct {
	id string // (TaskCreate's; none for TodoWrite's)
	Task
}

// maxEntries is how many entries are kept; older ones are dropped.
const maxEntries = 2000

func NewClaude() *Claude { return &Claude{tools: map[string]toolRef{}} }

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

// Meta is what the session runs with, as the log read last said it.
func (c *Claude) Meta() Meta { return c.meta }

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
			return
		}
		c.line(lines[:i])
		lines = lines[i+1:]
	}
	c.trim()
}

// Since copies the entries changed after revision rev, in order.
func (c *Claude) Since(rev int) []Entry {
	out := []Entry{}
	for _, e := range c.entries {
		if e.Rev > rev {
			cp := *e
			cp.Parts = make([]Part, len(e.Parts))
			for i, p := range e.Parts {
				cp.Parts[i] = p
				if p.Tool != nil {
					t := *p.Tool
					cp.Parts[i].Tool = &t
				}
			}
			out = append(out, cp)
		}
	}
	return out
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
		for _, e := range c.entries[:n] {
			for _, p := range e.Parts {
				// (A log that reuses an id has the map point at the later call.)
				if p.Tool != nil && c.tools[p.Tool.ID].t == p.Tool {
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
	Timestamp        string          `json:"timestamp"`
	IsMeta           bool            `json:"isMeta"`
	IsSidechain      bool            `json:"isSidechain"`
	IsCompactSummary bool            `json:"isCompactSummary"`
	PromptSource     string          `json:"promptSource"`
	AITitle          string          `json:"aiTitle"`
	CustomTitle      string          `json:"customTitle"`
	ToolUseResult    json.RawMessage `json:"toolUseResult"`
	Subtype          string          `json:"subtype"`
	Content          json.RawMessage `json:"content"`   // (system rows)
	GitBranch        *string         `json:"gitBranch"` // (present and empty: not on a branch)
	PermissionMode   string          `json:"permissionMode"`
	Message          *struct {
		Content json.RawMessage `json:"content"`
		Model   string          `json:"model"`
		Usage   *struct {
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
	if json.Unmarshal(b, &r) != nil || r.IsSidechain {
		c.Skipped++
		return
	}
	if r.GitBranch != nil {
		c.meta.Branch = clip(*r.GitBranch, maxMeta)
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
		if a := r.Attachment; a != nil && a.Type == "queued_command" && !a.IsMeta {
			c.queued(&r)
			return
		}
	case "ai-title":
		if t := strings.TrimSpace(clip(r.AITitle, maxTitle)); t != "" {
			c.aiTitle = t
		}
		return
	case "custom-title":
		c.myTitle = strings.TrimSpace(clip(r.CustomTitle, maxTitle))
		return
	case "permission-mode":
		c.setMode(r.PermissionMode)
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
	c.entries = append(c.entries, &Entry{Seq: c.seq, Rev: c.rev, ID: clip(r.UUID, maxID), Role: role, Time: clip(r.Timestamp, 64), Parts: parts})
	e := c.entries[len(c.entries)-1]
	for i := range e.Parts {
		if t := e.Parts[i].Tool; t != nil && t.ID != "" {
			c.tools[t.ID] = toolRef{e: e, t: t}
		}
	}
}

func (c *Claude) user(r *claudeRow) {
	if r.IsMeta {
		return
	}
	c.setMode(r.PermissionMode)
	var s string
	if json.Unmarshal(r.Message.Content, &s) == nil {
		if r.IsCompactSummary {
			c.add(r, RoleSummary, []Part{textPart(KindText, s)})
			return
		}
		role, text := classifyUserText(s)
		if role == "" {
			return
		}
		if role == RoleUser && r.PromptSource == "system" {
			role = RoleNote
		}
		c.add(r, role, []Part{textPart(KindText, text)})
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
	var parts []Part
	for _, b := range blocks {
		switch b.Type {
		case "tool_result":
			if results == 1 {
				c.result(b, r.ToolUseResult)
			} else {
				c.result(b, nil)
			}
		case "text":
			if role, text := classifyUserText(b.Text); role == RoleUser {
				parts = append(parts, textPart(KindText, text))
			}
		case "image":
			parts = append(parts, Part{Kind: KindImage})
		}
	}
	role := RoleUser
	if r.PromptSource == "system" {
		role = RoleNote
	}
	c.add(r, role, parts)
}

func (c *Claude) queued(r *claudeRow) {
	a := r.Attachment
	var s string
	if json.Unmarshal(a.Prompt, &s) == nil {
		if a.CommandMode == "task-notification" {
			if _, text := classifyUserText(s); text != "" {
				c.add(r, RoleNote, []Part{textPart(KindText, text)})
			}
			return
		}
		if a.CommandMode == "prompt" {
			if role, text := classifyUserText(s); role != "" {
				c.add(r, role, []Part{textPart(KindText, text)})
			}
		}
		return
	}
	var blocks []claudeBlock
	if a.CommandMode != "prompt" || json.Unmarshal(a.Prompt, &blocks) != nil {
		return
	}
	var parts []Part
	for _, b := range blocks {
		switch b.Type {
		case "text":
			if role, text := classifyUserText(b.Text); role == RoleUser {
				parts = append(parts, textPart(KindText, text))
			}
		case "image":
			parts = append(parts, Part{Kind: KindImage})
		}
	}
	c.add(r, RoleUser, parts)
}

func (c *Claude) assistant(r *claudeRow) {
	// (A row Claude Code made up itself, an error say, is of no model.)
	if m := r.Message.Model; m != "" && m != "<synthetic>" {
		c.meta.Model = clip(m, maxMeta)
	}
	if u := r.Message.Usage; u != nil && r.Message.Model != "<synthetic>" {
		if n := u.Input + u.CacheWrite + u.CacheRead; n > 0 {
			c.meta.Context = n
		}
	}
	var blocks []claudeBlock
	if json.Unmarshal(r.Message.Content, &blocks) != nil {
		return
	}
	var parts []Part
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
			parts = append(parts, Part{Kind: KindTool, Tool: t})
		}
	}
	c.add(r, RoleAssistant, parts)
	for _, b := range blocks {
		if f := taskChange(b.Name, b.Input); f != nil && b.Type == "tool_use" {
			if ref, ok := c.tools[clip(b.ID, maxID)]; ok {
				ref.task = f
				c.tools[clip(b.ID, maxID)] = ref
			}
		}
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
	ref, ok := c.tools[b.ToolUseID]
	if !ok {
		return
	}
	text := resultText(b.Content)
	if !b.IsError {
		ref.t.Add, ref.t.Del = madeLines(ref.t.Name, account, ref.t.Add, ref.t.Del)
		if ref.task != nil {
			ref.task(c)
		} else if ref.t.Name == "TaskCreate" {
			c.created(account, text)
		}
	}
	ref.t.Done = true
	ref.t.Error = b.IsError
	ref.t.ResultLines = strings.Count(strings.TrimRight(text, "\n"), "\n") + 1
	if text == "" {
		ref.t.ResultLines = 0
	}
	ref.t.Result, ref.t.Truncated = cut(text, maxResult)
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

func resultText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return clean(s)
	}
	var blocks []claudeBlock
	if json.Unmarshal(raw, &blocks) != nil {
		return ""
	}
	var parts []string
	for _, b := range blocks {
		if b.Type == "text" {
			parts = append(parts, b.Text)
		}
	}
	return clean(strings.Join(parts, "\n"))
}

// Envelopes in user rows. Claude Code wraps what it writes there for its own
// purposes in tags; the person's own text that happens to start with "<" is
// kept as it is.
var (
	envelopeTag  = regexp.MustCompile(`^\s*<([a-z][a-z0-9-]*)[\s>]`)
	taskSummary  = regexp.MustCompile(`(?s)<summary>(.*?)</summary>`)
	taskStatus   = regexp.MustCompile(`(?s)<status>(.*?)</status>`)
	commandName  = regexp.MustCompile(`(?s)<command-name>(.*?)</command-name>`)
	commandArgs  = regexp.MustCompile(`(?s)<command-args>(.*?)</command-args>`)
	tagContent   = regexp.MustCompile(`(?s)^\s*<([a-z-]+)>(.*?)</[a-z-]+>\s*$`)
	ansiSequence = regexp.MustCompile("\x1b\\[[0-9;?]*[ -/]*[@-~]|\x1b\\][^\x07\x1b]*(\x07|\x1b\\\\)|\x1b[@-_]")
)

// classifyUserText says what a user row's text is: the person's ("user"),
// a note, or nothing to show (""), and the text to show for it.
func classifyUserText(s string) (role, text string) {
	m := envelopeTag.FindStringSubmatch(s)
	if m == nil {
		if strings.TrimSpace(s) == "" {
			return "", ""
		}
		return RoleUser, s
	}
	switch m[1] {
	case "system-reminder", "local-command-caveat", "command-message", "user-memory-input":
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
	return RoleUser, s
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
	// Another tool (an MCP server's): its first text field, by name.
	keys := make([]string, 0, len(in))
	for k := range in {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if s, ok := in[k].(string); ok && strings.TrimSpace(s) != "" {
			return "", d(s), "", ""
		}
	}
	return "", "", "", ""
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
