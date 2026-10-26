package convo

import (
	"bytes"
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
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
	orphans map[string]codexOut
	meta    Meta
	title   string // (its first message: Codex names no session)
	working bool
	known   bool // (a row said whether it works)
	trimmed bool
	ctxSaid bool
	Skipped int
}

type codexOut struct {
	text   string
	failed bool
}

func NewCodex() *Codex { return &Codex{tools: map[string][]*Tool{}} }

func (c *Codex) Rev() int         { return c.rev }
func (c *Codex) Title() string    { return c.title }
func (c *Codex) Meta() Meta       { return c.meta }
func (c *Codex) Trimmed() bool    { return c.trimmed }
func (c *Codex) Tasks() []Task    { return nil }
func (c *Codex) Queued() []Queued { return nil }

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
			o.answer(ts, out.text, out.failed)
			delete(c.orphans, id)
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
	if !c.known {
		c.working, c.known = o.working, o.known
	}
	add := o.entries
	if room := max(0, maxEntries-len(c.entries)); len(add) > room {
		add = add[len(add)-room:]
		c.trimmed = true
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
		c.entries = append([]*Entry(nil), c.entries[n:]...)
	}
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
	Input     string          `json:"input"`
	Arguments string          `json:"arguments"`
	Output    json.RawMessage `json:"output"`
	Item      *struct {
		Type    string          `json:"type"`
		ID      string          `json:"id"`
		Content json.RawMessage `json:"content"`
	} `json:"item"`
	Info *struct {
		Last *struct {
			Input int64 `json:"input_tokens"`
		} `json:"last_token_usage"`
	} `json:"info"`
	Reason string `json:"reason"`
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
	// compacted
	Message string `json:"message"`
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
	case "item_completed":
		if p.Item == nil || p.Item.Type != "UserMessage" {
			return
		}
		var parts []Part
		for _, b := range codexContent(p.Item.Content) {
			switch {
			case b.Type == "text" || b.Type == "input_text":
				if strings.TrimSpace(b.Text) != "" {
					parts = append(parts, textPart(KindText, b.Text))
				}
			case strings.Contains(b.Type, "image"):
				parts = append(parts, Part{Kind: KindImage})
			}
		}
		if len(parts) == 0 {
			return
		}
		if c.title == "" {
			for _, pt := range parts {
				if pt.Kind == KindText {
					c.title = clip(strings.Join(strings.Fields(pt.Text), " "), maxTitle)
					break
				}
			}
		}
		c.add(r, p.Item.ID, RoleUser, parts)
	}
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
		src := p.Input
		if p.Type == "function_call" {
			src = p.Arguments
		}
		tools := codexTools(p.Name, src)
		id := clip(p.CallID, maxID)
		for _, t := range tools {
			t.ID = id
		}
		var parts []Part
		for _, t := range tools {
			parts = append(parts, Part{Kind: KindTool, Tool: t})
		}
		c.add(r, p.ID, RoleAssistant, parts)
		if id != "" && len(tools) > 0 {
			c.tools[id] = tools
		}
		if out, ok := c.orphans[id]; ok {
			c.answer(tools, out.text, out.failed)
			delete(c.orphans, id)
		}
	case "custom_tool_call_output", "function_call_output":
		text, failed := codexOutput(p.Output)
		id := clip(p.CallID, maxID)
		if ts, ok := c.tools[id]; ok {
			c.answer(ts, text, failed)
			c.touch(ts) // (the call's entry changed)
		} else if id != "" && len(c.orphans) < maxOrphans {
			if c.orphans == nil {
				c.orphans = map[string]codexOut{}
			}
			c.orphans[id] = codexOut{text, failed}
		}
	}
}

// touch marks the entries of these calls as changed now.
func (c *Codex) touch(ts []*Tool) {
	for _, e := range c.entries {
		for _, p := range e.Parts {
			for _, t := range ts {
				if p.Tool == t {
					e.Rev = c.rev
				}
			}
		}
	}
}

// answer: what came back of a call, on its first tool (a call's tools are
// its file edits, or its one command): the rest only done.
func (c *Codex) answer(ts []*Tool, text string, failed bool) {
	at := 0 // (its command's, if it ran one; else its first file's)
	for i, t := range ts {
		if t.Name == "Bash" {
			at = i
			break
		}
	}
	for i, t := range ts {
		t.Done = true
		if i == at {
			t.Result, t.Truncated = cut(clean(text), maxResult)
			t.ResultLines = lineCount(text)
		}
		t.Error = failed || codexExit.MatchString(text)
	}
}

var (
	codexFailed = regexp.MustCompile(`^Script (failed|error)`)
	codexExit   = regexp.MustCompile(`(?m)^Process exited with code [1-9]`)
	codexHead   = regexp.MustCompile(`^Script (completed|failed)\nWall time [^\n]*\n(Output:\n)?`)
)

// codexOutput: what came back, as text (its "Script completed / Wall time"
// lines left out), and whether the script failed (as those lines said).
func codexOutput(raw json.RawMessage) (string, bool) {
	var parts []string
	for _, b := range codexContent(raw) {
		parts = append(parts, b.Text)
	}
	all := strings.Join(parts, "")
	return codexHead.ReplaceAllString(all, ""), codexFailed.MatchString(all)
}

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
	for _, lit := range patchStr.FindAllString(src, 8) {
		if patch, ok := jsUnquote(lit); ok {
			out = append(out, patchTools(patch)...)
		}
	}
	for _, lit := range patchTmpl.FindAllString(src, 8) {
		if patch, ok := jsTemplate(lit); ok {
			out = append(out, patchTools(patch)...)
		}
	}
	// (Its commands, one line each, as one: what came back is of them all.)
	var cmds []string
	for _, m := range execCmd.FindAllStringSubmatch(src, 16) {
		if cmd, ok := jsUnquote(`"` + m[1] + `"`); ok {
			cmds = append(cmds, cmd)
		}
	}
	if len(cmds) > 0 {
		out = append(out, &Tool{Name: "Bash", Detail: clip(strings.Join(cmds, "\n"), maxDetail)})
	}
	// (It asks the person, last: what it waits on.)
	if len(out) > 0 && strings.Contains(src, "tools.request_user_input") {
		out = append(out, &Tool{Name: "request_user_input"})
	}
	if len(out) > 0 {
		return out
	}
	called := ""
	if m := toolCalled.FindStringSubmatch(src); m != nil {
		called = m[1]
	}
	t := &Tool{Name: clip(firstNonEmpty(called, name), 128)}
	if name == "function_call" || called == "" {
		t.Detail = clip(strings.TrimSpace(src), maxDetail)
	}
	return []*Tool{t}
}

// patchTools: an apply_patch's files, each with its lines added and
// removed and its hunks.
func patchTools(patch string) []*Tool {
	var out []*Tool
	var t *Tool
	var h *Hunk
	size := 0
	for _, l := range strings.Split(patch, "\n") {
		if m := patchFile.FindStringSubmatch(l); m != nil {
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
		if t == nil || strings.HasPrefix(l, "*** ") {
			continue
		}
		if strings.HasPrefix(l, "@@") {
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
	return out
}

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
	c.entries = append(c.entries, &Entry{Seq: c.seq, Rev: c.rev, ID: clip(id, maxID), Role: role, Time: clip(r.Timestamp, 64), Parts: parts})
}
