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
//
// Everything else (modes, titles, file snapshots, API bookkeeping) is
// skipped. Feed it whole lines in order.
type Claude struct {
	entries []*Entry
	tools   map[string]toolRef
	rev     int
	seq     int
	// Skipped counts lines that were not JSON or of no use to the view.
	Skipped int
}

type toolRef struct {
	e *Entry
	t *Tool
}

// maxEntries is how many entries are kept; older ones are dropped.
const maxEntries = 2000

func NewClaude() *Claude { return &Claude{tools: map[string]toolRef{}} }

// Rev is the revision after the last Feed.
func (c *Claude) Rev() int { return c.rev }

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
				if p.Tool != nil && p.Tool.ID != "" {
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
	Subtype          string          `json:"subtype"`
	Content          json.RawMessage `json:"content"` // (system rows)
	Message          *struct {
		Content json.RawMessage `json:"content"`
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
		parts = parts[:maxParts]
	}
	c.seq++
	c.entries = append(c.entries, &Entry{Seq: c.seq, Rev: c.rev, ID: clip(r.UUID, maxID), Role: role, Time: clip(r.Timestamp, 64), Parts: parts})
	e := c.entries[len(c.entries)-1]
	for i := range e.Parts {
		if t := e.Parts[i].Tool; t != nil && t.ID != "" {
			c.tools[t.ID] = toolRef{e, t}
		}
	}
}

func (c *Claude) user(r *claudeRow) {
	if r.IsMeta {
		return
	}
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
	var parts []Part
	for _, b := range blocks {
		switch b.Type {
		case "tool_result":
			c.result(b)
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
			parts = append(parts, Part{Kind: KindTool, Tool: t})
		}
	}
	c.add(r, RoleAssistant, parts)
}

// result fills in the call a tool_result answers. A result for a call not
// kept (dropped as old, or never seen) is ignored.
func (c *Claude) result(b claudeBlock) {
	ref, ok := c.tools[b.ToolUseID]
	if !ok {
		return
	}
	text := resultText(b.Content)
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

// cut shortens s to at most n bytes, at a character boundary.
func cut(s string, n int) (string, bool) {
	if len(s) <= n {
		return s, false
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n], true
}

func clip(s string, n int) string {
	s, _ = cut(clean(s), n)
	return s
}
