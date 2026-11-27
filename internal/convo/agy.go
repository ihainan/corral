package convo

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Agy reads Antigravity CLI's full session transcript
// (~/.gemini/antigravity-cli/brain/<uuid>/.system_generated/logs/transcript_full.jsonl),
// one JSON object per line appended in step_index order:
//   - USER_INPUT: what the person wrote (inside <USER_REQUEST>…</USER_REQUEST>,
//     with <ADDITIONAL_METADATA> stripped) plus attached media paths.
//   - PLANNER_RESPONSE: the model's thinking, markdown text, and tool_calls,
//     along with model id and token usage.
//   - GENERIC: tool results, paired in FIFO order with the tool_calls of the
//     preceding PLANNER_RESPONSE (or held as orphans for Prepend). Background
//     tasks have status "RUNNING" with a task id and log path.
//   - SYSTEM_MESSAGE: background task completion notifications and system
//     wakeups.
//   - CHECKPOINT: context compaction summary written by agy.
type Agy struct {
	entries []*Entry
	seq     int
	rev     int
	title   string
	meta    Meta
	working bool
	known   bool
	trimmed bool
	Skipped int

	// pending: tool calls awaiting their GENERIC result rows, in call order.
	pending []agyPending
	// orphans: leading GENERIC rows read before their PLANNER_RESPONSE (when
	// reading from the tail of a long log), paired in Prepend.
	orphans []agyRow

	// running: background tasks (run_command with status RUNNING) until a
	// SYSTEM_MESSAGE reports them finished.
	running  map[string]Running
	runOrder []string
	ranOut   map[string]bool
	since    int64

	// maxStep tracks the highest step_index seen so far (JSONL or DB errors),
	// and seenDBErrs deduplicates step_type = 17 rows merged from conversations/<sid>.db.
	maxStep    int
	seenDBErrs map[int]bool
}

type agyPending struct {
	entry *Entry
	tool  *Tool
}

type agyRow struct {
	StepIndex       int           `json:"step_index"`
	Source          string        `json:"source"`
	Type            string        `json:"type"`
	Status          string        `json:"status"`
	CreatedAt       string        `json:"created_at"`
	Model           string        `json:"model"`
	InputTokens     int64         `json:"input_tokens"`
	CacheReadTokens int64         `json:"cache_read_tokens"`
	OutputTokens    int64         `json:"output_tokens"`
	Thinking        string        `json:"thinking"`
	Content         string        `json:"content"`
	Error           string        `json:"error"`
	ToolCalls       []agyToolCall `json:"tool_calls"`
	Media           []agyMedia    `json:"media"`
}

type agyToolCall struct {
	Name string          `json:"name"`
	Args json.RawMessage `json:"args"`
}

type agyMedia struct {
	MimeType string `json:"mime_type"`
	URI      string `json:"uri"`
}

const (
	maxAgyPending = 64
	maxAgyOrphans = 64
)

func NewAgy() *Agy { return &Agy{} }

func (c *Agy) Rev() int         { return c.rev }
func (c *Agy) Title() string    { return c.title }
func (c *Agy) SetTitle(t string) {
	if c.title == "" && t != "" {
		c.title = clip(clean(firstLineOf(t)), maxTitle)
	}
}
func (c *Agy) Meta() Meta       { return c.meta }
func (c *Agy) Trimmed() bool    { return c.trimmed }
func (c *Agy) Tasks() []Task    { return nil }
func (c *Agy) Queued() []Queued { return nil }

func (c *Agy) Running() []Running {
	var out []Running
	for _, id := range c.runOrder {
		if r, ok := c.running[id]; ok && c.ofNow(r.Time) {
			out = append(out, r)
		}
	}
	return out
}

func (c *Agy) QueueSince(started int64) { c.since = started }

func (c *Agy) ofNow(ts string) bool {
	if c.since <= 0 {
		return true
	}
	at, err := time.Parse(time.RFC3339Nano, ts)
	return err == nil && at.UnixMilli() >= c.since
}

// Status is "busy" while a turn is in progress, "idle" after a final answer
// without pending tool calls, "" before any row has been read.
func (c *Agy) Status() string {
	switch {
	case !c.known:
		return ""
	case c.working:
		return "busy"
	}
	return "idle"
}

func (c *Agy) First() int {
	if len(c.entries) == 0 {
		return 0
	}
	return c.entries[0].Seq
}

func (c *Agy) Since(rev int) []Entry { return since(c.entries, rev) }

func (c *Agy) Feed(lines []byte) {
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

func (c *Agy) line(raw []byte) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return
	}
	var r agyRow
	if json.Unmarshal(raw, &r) != nil || r.Type == "" {
		c.Skipped++
		return
	}
	if r.StepIndex > c.maxStep {
		c.maxStep = r.StepIndex
	}
	switch r.Type {
	case "USER_INPUT":
		c.userInput(r)
	case "PLANNER_RESPONSE":
		c.plannerResponse(r)
	case "GENERIC":
		c.generic(r)
	case "SYSTEM_MESSAGE":
		c.systemMessage(r)
	case "CHECKPOINT":
		c.checkpoint(r)
	case "ERROR_MESSAGE":
		c.errorMessage(r)
	default:
		c.Skipped++
	}
}

func (c *Agy) appendEntry(e *Entry) {
	c.seq++
	e.Seq = c.seq
	e.Rev = c.rev
	c.entries = append(c.entries, e)
}

func (c *Agy) trim() {
	if len(c.entries) <= maxEntries {
		return
	}
	drop := len(c.entries) - maxEntries
	c.entries = c.entries[drop:]
	c.trimmed = true
}

func (c *Agy) userInput(r agyRow) {
	// A new user message starts a new turn; any stale unmatched tool calls
	// from an interrupted turn should not pair with later results.
	c.pending = nil
	c.working, c.known = true, true

	text := agyExtractUserText(r.Content)
	var parts []Part
	if text != "" {
		t, truncated := cut(clean(text), maxText)
		parts = append(parts, Part{Kind: KindText, Text: t, Truncated: truncated})
		if c.title == "" {
			c.title = clip(clean(firstLineOf(text)), maxTitle)
		}
	}
	for _, m := range r.Media {
		if len(parts) >= maxParts {
			break
		}
		if strings.HasPrefix(strings.ToLower(m.MimeType), "image/") {
			p := agyMediaPath(m.URI)
			parts = append(parts, Part{Kind: KindImage, Path: clip(p, maxImagePath)})
		}
	}
	if len(parts) == 0 {
		return
	}
	c.appendEntry(&Entry{
		ID:    fmt.Sprintf("step-%d", r.StepIndex),
		Role:  RoleUser,
		Time:  clip(r.CreatedAt, 64),
		Parts: parts,
	})
}

func (c *Agy) plannerResponse(r agyRow) {
	if r.Model != "" {
		c.meta.Model = clip(r.Model, maxMeta)
		if eff := agyEffortOfModel(r.Model); eff != "" {
			c.meta.Effort = eff
		}
		if w := agyModelWindow(r.Model); w > 0 {
			c.meta.Window = w
			c.meta.WindowBy = "model"
		}
	}
	if ctx := r.InputTokens + r.CacheReadTokens; ctx > 0 {
		c.meta.Context = ctx
	}
	if r.OutputTokens > 0 {
		c.meta.Answer = &Answer{Out: r.OutputTokens}
	}

	var parts []Part
	if th := strings.TrimSpace(r.Thinking); th != "" {
		t, truncated := cut(clean(th), maxText)
		parts = append(parts, Part{Kind: KindThinking, Text: t, Truncated: truncated})
	}
	if txt := strings.TrimSpace(r.Content); txt != "" {
		t, truncated := cut(clean(txt), maxText)
		parts = append(parts, Part{Kind: KindText, Text: t, Truncated: truncated})
	}

	e := &Entry{
		ID:   fmt.Sprintf("step-%d", r.StepIndex),
		Role: RoleAssistant,
		Time: clip(r.CreatedAt, 64),
	}

	// Replace pending queue with this planner response's tool calls (if any).
	if len(r.ToolCalls) > 0 {
		c.pending = nil
		for idx, tc := range r.ToolCalls {
			if len(parts) >= maxParts {
				break
			}
			t := agyBuildTool(fmt.Sprintf("step-%d-%d", r.StepIndex, idx), tc)
			parts = append(parts, Part{Kind: KindTool, Tool: t})
			if len(c.pending) < maxAgyPending {
				c.pending = append(c.pending, agyPending{entry: e, tool: t})
			}
		}
		c.working, c.known = true, true
	} else {
		c.working, c.known = false, true
	}

	if r.Error != "" || r.Status == "ERROR" {
		msg := strings.TrimSpace(r.Error)
		if msg == "" {
			msg = "error"
		}
		var atUnix int64
		if ts, err := time.Parse(time.RFC3339Nano, r.CreatedAt); err == nil {
			atUnix = ts.Unix()
		}
		if tr := agyClassifyTrouble(msg, "", "", "", 0, atUnix, 0); tr != nil {
			parts = append(parts, Part{Kind: KindTrouble, Trouble: tr})
		}
	}

	if len(parts) == 0 {
		return
	}
	e.Parts = parts
	c.appendEntry(e)
}

func (c *Agy) generic(r agyRow) {
	if len(c.pending) == 0 {
		for _, id := range agyParseBgCancel(agyStripTimestamps(r.Content)) {
			c.endRun(id)
		}
		// Read from the tail of a long log: keep for Prepend to pair.
		if len(c.orphans) < maxAgyOrphans {
			c.orphans = append(c.orphans, r)
		}
		return
	}
	p := c.pending[0]
	c.pending = c.pending[1:]
	c.applyGeneric(p.entry, p.tool, r)
}

func (c *Agy) applyGeneric(e *Entry, t *Tool, r agyRow) {
	body := agyStripTimestamps(r.Content)

	// Background task launched by run_command (status RUNNING).
	if r.Status == "RUNNING" || strings.Contains(body, "Tool is running as a background task with task id:") {
		if id, desc, logPath := agyParseBgStart(body); id != "" {
			what := desc
			if what == "" {
				what = firstNonEmpty(t.Title, t.Detail)
			}
			c.setGoing(id, "cmd", what, t.Detail, t.detailCut, logPath, r.CreatedAt)
		}
	}
	// Background task cancelled/killed via manage_task.
	for _, id := range agyParseBgCancel(body) {
		c.endRun(id)
	}

	// Structured answers for ask_question (mapped to AskUserQuestion).
	if t.Name == "AskUserQuestion" && len(t.Asks) > 0 {
		if ans := agyParseAnswers(body, len(t.Asks)); len(ans) > 0 {
			t.Answers = ans
		}
	}

	// Subagent conversationId returned by invoke_subagent (mapped to Agent).
	if t.Name == "Agent" && t.AgentID == "" {
		if subID := agyParseSubagentID(body); subID != "" {
			t.AgentID = subID
		}
	}

	// Count image media returned (e.g. read_file on a PNG/JPEG).
	imgCount := 0
	for _, m := range r.Media {
		if strings.HasPrefix(strings.ToLower(m.MimeType), "image/") {
			imgCount++
		}
	}
	if imgCount > 0 {
		t.Images = imgCount
	}

	failed := r.Status == "ERROR" || strings.TrimSpace(r.Error) != ""
	resText := body
	if failed && strings.TrimSpace(r.Error) != "" {
		if resText == "" {
			resText = strings.TrimSpace(r.Error)
		} else {
			resText = strings.TrimSpace(r.Error) + "\n" + resText
		}
	}

	t.Done = true
	t.Error = failed
	cleaned := clean(resText)
	t.setResult(cleaned)
	t.ResultLines = lineCount(cleaned)
	e.Rev = c.rev
}

func (c *Agy) systemMessage(r agyRow) {
	raw := r.Content
	// Background task completion notification.
	if id, status, code, summary := agyParseBgDone(raw); id != "" {
		what := ""
		if cur, ok := c.running[id]; ok {
			what = cur.What
		}
		c.endRun(id)
		ev := &Event{
			Kind:   EventTask,
			Task:   "cmd",
			Status: status,
			Code:   code,
			Text:   clip(clean(firstNonEmpty(what, firstNonEmpty(summary, id))), maxTitle),
		}
		c.appendEntry(&Entry{
			ID:    fmt.Sprintf("step-%d", r.StepIndex),
			Role:  RoleNote,
			Time:  clip(r.CreatedAt, 64),
			Parts: []Part{{Kind: KindEvent, Event: ev}},
		})
		return
	}
	// Other system wakeups (subagent/timer messages): keep concise notice if meaningful.
	inner := agyExtractTag(raw, "SYSTEM_MESSAGE")
	if inner == "" {
		return
	}
	line := firstLineOf(inner)
	if line == "" || strings.HasPrefix(line, "Reminder:") {
		return
	}
	t, _ := cut(clean(line), maxNotice)
	c.appendEntry(&Entry{
		ID:    fmt.Sprintf("step-%d", r.StepIndex),
		Role:  RoleNote,
		Time:  clip(r.CreatedAt, 64),
		Parts: []Part{{Kind: KindEvent, Event: &Event{Kind: EventNotice, Level: "notice", Text: t}}},
	})
}

func (c *Agy) checkpoint(r agyRow) {
	pre := r.InputTokens + r.CacheReadTokens
	if pre > 0 {
		c.meta.Context = pre
	}
	c.appendEntry(&Entry{
		ID:   fmt.Sprintf("step-%d-ev", r.StepIndex),
		Role: RoleNote,
		Time: clip(r.CreatedAt, 64),
		Parts: []Part{{Kind: KindEvent, Event: &Event{
			Kind:    EventCompact,
			Trigger: "auto",
			Pre:     pre,
		}}},
	})
	if txt := strings.TrimSpace(r.Content); txt != "" {
		t, truncated := cut(clean(txt), maxText)
		c.appendEntry(&Entry{
			ID:    fmt.Sprintf("step-%d", r.StepIndex),
			Role:  RoleSummary,
			Time:  clip(r.CreatedAt, 64),
			Parts: []Part{{Kind: KindText, Text: t, Truncated: truncated}},
		})
	}
}

func (c *Agy) errorMessage(r agyRow) {
	c.pending = nil
	c.working, c.known = false, true
	msg := firstNonEmpty(strings.TrimSpace(r.Error), strings.TrimSpace(r.Content))
	if msg == "" {
		msg = "error"
	}
	var atUnix int64
	if ts, err := time.Parse(time.RFC3339Nano, r.CreatedAt); err == nil {
		atUnix = ts.Unix()
	}
	tr := agyClassifyTrouble(msg, "", "", "", 0, atUnix, 0)
	if tr == nil {
		return
	}
	c.appendEntry(&Entry{
		ID:    fmt.Sprintf("step-%d", r.StepIndex),
		Role:  RoleNote,
		Time:  clip(r.CreatedAt, 64),
		Parts: []Part{{Kind: KindTrouble, Trouble: tr}},
	})
}

func (c *Agy) setGoing(id, kind, what, cmd string, cmdCut bool, outPath, at string) {
	if id == "" || c.ranOut[id] {
		delete(c.ranOut, id)
		return
	}
	if _, there := c.running[id]; there || len(c.running) >= maxRunning {
		return
	}
	if c.running == nil {
		c.running = map[string]Running{}
	}
	keptCmd, cutCmd := cut(clean(cmd), maxDetail)
	c.running[id] = Running{
		ID:     clip(id, maxID),
		What:   clip(clean(firstLineOf(what)), maxTitle),
		Kind:   kind,
		Time:   clip(at, 64),
		Cmd:    keptCmd,
		CmdCut: cmdCut || cutCmd,
		Out:    clip(outPath, maxImagePath),
	}
	c.runOrder = append(c.runOrder, id)
}

func (c *Agy) endRun(id string) {
	if id == "" {
		return
	}
	if _, there := c.running[id]; there {
		delete(c.running, id)
		for i, x := range c.runOrder {
			if x == id {
				c.runOrder = append(c.runOrder[:i:i], c.runOrder[i+1:]...)
				break
			}
		}
		return
	}
	if c.ranOut == nil {
		c.ranOut = map[string]bool{}
	}
	for len(c.ranOut) >= maxRunning && !c.ranOut[id] {
		for k := range c.ranOut {
			delete(c.ranOut, k)
			break
		}
	}
	c.ranOut[id] = true
}

// Prepend puts o, a part of the log read on its own from before what c read,
// before c's entries. Leading orphan GENERIC rows in c pair with trailing
// pending tool calls in o.
func (c *Agy) Prepend(other Parser) {
	o, ok := other.(*Agy)
	if !ok {
		return
	}
	// Pair c's leading orphan GENERIC rows with o's trailing pending tool calls.
	for len(c.orphans) > 0 && len(o.pending) > 0 {
		r := c.orphans[0]
		c.orphans = c.orphans[1:]
		p := o.pending[0]
		o.pending = o.pending[1:]
		o.applyGeneric(p.entry, p.tool, r)
	}
	// Keep o's own leading orphans for an even older chunk.
	if len(o.orphans) > 0 {
		c.orphans = append(append([]agyRow(nil), o.orphans...), c.orphans...)
		if len(c.orphans) > maxAgyOrphans {
			c.orphans = c.orphans[:maxAgyOrphans]
		}
	}
	// Merge background tasks (same logic as Codex/Claude).
	for id := range o.ranOut {
		if _, there := o.running[id]; !there {
			c.endRun(id)
		}
	}
	var before []string
	for _, id := range o.runOrder {
		r, there := o.running[id]
		if !there || c.ranOut[id] {
			delete(c.ranOut, id)
			continue
		}
		if _, already := c.running[id]; already || len(c.running) >= maxRunning {
			continue
		}
		if c.running == nil {
			c.running = map[string]Running{}
		}
		c.running[id] = r
		before = append(before, id)
	}
	if len(before) > 0 {
		c.runOrder = append(before, c.runOrder...)
	}
	if c.title == "" {
		c.title = o.title
	}
	if c.meta.Model == "" {
		c.meta.Model = o.meta.Model
	}
	if c.meta.Effort == "" {
		c.meta.Effort = o.meta.Effort
	}
	if c.meta.Window == 0 && o.meta.Window > 0 {
		c.meta.Window, c.meta.WindowBy = o.meta.Window, o.meta.WindowBy
	}
	if c.meta.Context == 0 {
		c.meta.Context = o.meta.Context
	}
	if c.meta.Answer == nil {
		c.meta.Answer = o.meta.Answer
	}
	if o.maxStep > c.maxStep {
		c.maxStep = o.maxStep
	}
	if !c.known && o.known {
		c.known, c.working = true, o.working
	}
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

// agyTools maps Antigravity CLI tool names to Corral's canonical tool names.
var agyTools = map[string]string{
	"run_command":                "Bash",
	"read_file":                  "Read",
	"write_file":                 "Write",
	"edit_file":                  "Edit",
	"replace_file_content":       "Edit",
	"multi_replace_file_content": "MultiEdit",
	"grep_search":                "Grep",
	"grep":                       "Grep",
	"find_by_name":               "Glob",
	"glob":                       "Glob",
	"list_dir":                   "LS",
	"search_web":                 "WebSearch",
	"read_url_content":           "WebFetch",
	"web_fetch":                  "WebFetch",
	"ask_question":               "AskUserQuestion",
	"invoke_subagent":            "Agent",
	"send_message":               "SendMessage",
	"schedule":                   "CronCreate",
}

func agyBuildTool(id string, tc agyToolCall) *Tool {
	name := agyTools[tc.Name]
	if name == "" {
		name = clip(tc.Name, 128)
	}
	t := &Tool{ID: clip(id, maxID), Name: name}

	var args map[string]any
	if len(tc.Args) > 0 {
		_ = json.Unmarshal(tc.Args, &args)
	}
	str := func(keys ...string) string {
		for _, k := range keys {
			if v, ok := args[k].(string); ok && v != "" {
				return v
			}
		}
		return ""
	}
	d := func(s string) string { s, _ = cut(clean(s), maxDetail); return s }

	switch name {
	case "Bash":
		t.Title = d(str("toolSummary", "description"))
		t.Detail, t.detailCut = cut(clean(str("CommandLine", "command")), maxDetail)
	case "Read":
		t.Detail = d(str("Path", "AbsolutePath", "file_path", "path"))
	case "Write":
		t.Detail = d(str("Path", "TargetFile", "file_path", "path"))
		content := str("Content", "CodeContent", "content")
		t.New = d(content)
		if content != "" {
			t.Add = lineCount(content)
		}
	case "Edit":
		t.Detail = d(str("Path", "TargetFile", "file_path", "path"))
		oldStr := str("Target", "TargetContent", "old_string")
		newStr := str("Replacement", "ReplacementContent", "new_string")
		t.Old = d(oldStr)
		t.New = d(newStr)
		if oldStr != "" || newStr != "" {
			if oldStr == newStr {
				t.Same = true
			} else {
				if oldStr != "" {
					t.Del = lineCount(oldStr)
				}
				if newStr != "" {
					t.Add = lineCount(newStr)
				}
			}
		}
	case "MultiEdit":
		t.Detail = d(str("Path", "TargetFile", "file_path", "path"))
		if chunks, ok := args["ReplacementChunks"].([]any); ok {
			var olds, news []string
			for _, ch := range chunks {
				if m, ok := ch.(map[string]any); ok {
					o, _ := m["TargetContent"].(string)
					n, _ := m["ReplacementContent"].(string)
					olds = append(olds, o)
					news = append(news, n)
					if o != "" {
						t.Del += lineCount(o)
					}
					if n != "" {
						t.Add += lineCount(n)
					}
				}
			}
			t.Old = d(strings.Join(olds, "\n…\n"))
			t.New = d(strings.Join(news, "\n…\n"))
		}
	case "Grep":
		q := str("Query", "pattern", "query")
		if p := str("SearchPath", "path"); p != "" {
			q += "  " + p
		}
		t.Detail = d(q)
	case "Glob":
		q := str("Pattern", "pattern")
		if p := str("SearchDirectory", "path"); p != "" {
			q += "  " + p
		}
		t.Detail = d(q)
	case "LS":
		t.Detail = d(str("DirectoryPath", "path"))
	case "WebSearch":
		t.Detail = d(str("query", "Query"))
	case "WebFetch":
		t.Detail = d(str("Url", "url"))
	case "Agent":
		t.Title = d(firstNonEmpty(str("Role"), firstNonEmpty(str("TypeName"), str("description"))))
		t.Detail = d(str("Prompt", "prompt"))
	case "SendMessage":
		t.Title = d(str("Recipient", "to"))
		t.Detail = d(str("Message", "message", "content"))
	case "CronCreate":
		when := str("CronExpression")
		if when == "" {
			if sec, ok := args["DurationSeconds"].(float64); ok && sec > 0 {
				when = fmt.Sprintf("%ds", int(sec))
			}
		}
		t.Detail = d(strings.TrimSpace(when + " " + str("Prompt", "prompt")))
	case "AskUserQuestion":
		t.Asks = agyParseAsks(args)
		if len(t.Asks) > 0 {
			t.Detail = d(t.Asks[0].Question)
		}
	default:
		switch tc.Name {
		case "manage_task":
			t.Detail = d(strings.TrimSpace(str("Action") + " " + str("TaskId")))
		case "swarm":
			t.Detail = d(strings.TrimSpace(str("action") + " " + str("name") + " " + str("title")))
		case "run_workflow":
			t.Detail = d(firstNonEmpty(str("Description"), str("ScriptPath")))
		default:
			t.Detail = d(str("Description", "description", "Prompt", "prompt", "Path", "path", "Query", "query", "Action", "action", "Name", "name"))
		}
	}
	return t
}

func agyParseAsks(args map[string]any) []Ask {
	qs, ok := args["questions"].([]any)
	if !ok || len(qs) == 0 {
		return nil
	}
	var out []Ask
	for _, item := range qs {
		if len(out) >= maxAsks {
			break
		}
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		qText, _ := m["question"].(string)
		qText = strings.TrimSpace(qText)
		if qText == "" {
			continue
		}
		qClean, qCut := cut(clean(qText), maxAskText)
		multi, _ := m["is_multi_select"].(bool)
		var opts []AskOption
		optCut := false
		if rawOpts, ok := m["options"].([]any); ok {
			for _, ro := range rawOpts {
				if len(opts) >= maxOptions {
					optCut = true
					break
				}
				switch v := ro.(type) {
				case string:
					l, c := cut(clean(v), maxAskText)
					optCut = optCut || c
					opts = append(opts, AskOption{Label: l})
				case map[string]any:
					lbl, _ := v["label"].(string)
					desc, _ := v["description"].(string)
					l, c1 := cut(clean(lbl), maxAskText)
					ds, c2 := cut(clean(desc), maxAskText)
					optCut = optCut || c1 || c2
					opts = append(opts, AskOption{Label: l, Description: ds})
				}
			}
		}
		out = append(out, Ask{
			Question: qClean,
			Multi:    multi,
			Options:  opts,
			Cut:      qCut || optCut,
		})
	}
	return out
}

// agyExtractUserText unwraps <USER_REQUEST>…</USER_REQUEST> and strips
// <ADDITIONAL_METADATA> / <USER_SETTINGS_CHANGE> envelopes.
func agyExtractUserText(s string) string {
	if req := agyExtractTag(s, "USER_REQUEST"); req != "" {
		return req
	}
	s = agyStripBlock(s, "ADDITIONAL_METADATA")
	s = agyStripBlock(s, "USER_SETTINGS_CHANGE")
	return strings.TrimSpace(s)
}

func agyExtractTag(s, tag string) string {
	openTag := "<" + tag + ">"
	closeTag := "</" + tag + ">"
	if j := strings.Index(s, closeTag); j >= 0 {
		// Use the last openTag before closeTag so an inline mention in a
		// preamble (e.g. "The following is a <SYSTEM_MESSAGE> not actually sent
		// by the user...") is skipped in favor of the actual tagged block.
		if i := strings.LastIndex(s[:j], openTag); i >= 0 {
			return strings.TrimSpace(s[i+len(openTag) : j])
		}
	}
	i := strings.Index(s, openTag)
	if i < 0 {
		return ""
	}
	return strings.TrimSpace(s[i+len(openTag):])
}

func agyStripBlock(s, tag string) string {
	openTag := "<" + tag + ">"
	closeTag := "</" + tag + ">"
	for {
		i := strings.Index(s, openTag)
		if i < 0 {
			return s
		}
		j := strings.Index(s[i+len(openTag):], closeTag)
		if j < 0 {
			return strings.TrimSpace(s[:i])
		}
		s = s[:i] + s[i+len(openTag)+j+len(closeTag):]
	}
}

func agyMediaPath(uri string) string {
	uri = strings.TrimSpace(uri)
	if strings.HasPrefix(uri, "file://") {
		return strings.TrimPrefix(uri, "file://")
	}
	return uri
}

func agyEffortOfModel(model string) string {
	m := strings.ToLower(strings.TrimSpace(model))
	for _, suf := range []string{"-xhigh", "-high", "-medium", "-low", "-max"} {
		if strings.HasSuffix(m, suf) {
			return suf[1:]
		}
	}
	return ""
}

func agyModelWindow(model string) int64 {
	m := strings.ToLower(strings.TrimSpace(model))
	switch {
	case strings.HasPrefix(m, "gemini-"):
		return 1_000_000
	case strings.HasPrefix(m, "claude-opus-5"), strings.HasPrefix(m, "claude-sonnet-5"):
		return 1_000_000
	case strings.HasPrefix(m, "claude-"):
		return 200_000
	case strings.HasPrefix(m, "gpt-oss-"):
		return 128_000
	}
	return 0
}

var agySubagentIDRe = regexp.MustCompile(`(?i)(?:conversation[_\s]?id\s*[:=]\s*"?|["\s(])([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})`)

func agyParseSubagentID(body string) string {
	m := agySubagentIDRe.FindStringSubmatch(body)
	if m == nil {
		return ""
	}
	return strings.ToLower(m[1])
}

// agyStripTimestamps drops leading "Created At: ..." and "Completed At: ..."
// metadata lines that agy prepends to GENERIC tool results.
func agyStripTimestamps(s string) string {
	for {
		trimmed := strings.TrimLeft(s, "\r\n")
		if strings.HasPrefix(trimmed, "Created At:") || strings.HasPrefix(trimmed, "Completed At:") {
			if nl := strings.IndexByte(trimmed, '\n'); nl >= 0 {
				s = trimmed[nl+1:]
				continue
			}
			return ""
		}
		return strings.TrimSpace(trimmed)
	}
}

var (
	agyBgIDRe   = regexp.MustCompile(`(?m)^Tool is running as a background task with task id:\s*(\S+)`)
	agyBgDescRe = regexp.MustCompile(`(?m)^Task Description:\s*(.+)$`)
	agyBgLogRe  = regexp.MustCompile(`(?m)^Task logs are available at:\s*(\S+)`)
)

func agyParseBgStart(body string) (id, desc, logPath string) {
	if m := agyBgIDRe.FindStringSubmatch(body); m != nil {
		id = strings.TrimSpace(m[1])
	}
	if m := agyBgDescRe.FindStringSubmatch(body); m != nil {
		desc = strings.TrimSpace(m[1])
	}
	if m := agyBgLogRe.FindStringSubmatch(body); m != nil {
		logPath = agyMediaPath(m[1])
	}
	return id, desc, logPath
}

var (
	agyBgDoneRe   = regexp.MustCompile(`Task (?:id )?"([^"]+)" (?:finished with result:|cancelled|canceled|killed|stopped|failed)`)
	agyBgCancelRe = regexp.MustCompile(`Task (?:id )?"([^"]+)" (?:cancelled|canceled|killed|stopped)`)
	agyExitRe     = regexp.MustCompile(`Command exited with code (-?\d+)`)
)

func agyParseBgCancel(body string) []string {
	matches := agyBgCancelRe.FindAllStringSubmatch(body, -1)
	if len(matches) == 0 {
		return nil
	}
	out := make([]string, 0, len(matches))
	for _, m := range matches {
		if m[1] != "" {
			out = append(out, m[1])
		}
	}
	return out
}

func agyParseBgDone(raw string) (id, status string, code *int, summary string) {
	m := agyBgDoneRe.FindStringSubmatch(raw)
	if m == nil {
		return "", "", nil, ""
	}
	id = m[1]
	status = "done"
	lower := strings.ToLower(m[0])
	switch {
	case strings.Contains(lower, "cancel"), strings.Contains(lower, "kill"), strings.Contains(lower, "stop"):
		status = "stopped"
	case strings.Contains(lower, "fail"):
		status = "failed"
	}
	if em := agyExitRe.FindStringSubmatch(raw); em != nil {
		if n, err := strconv.Atoi(em[1]); err == nil {
			code = &n
			if n != 0 {
				status = "failed"
			}
		}
	}
	return id, status, code, ""
}

var agyAnswerLineRe = regexp.MustCompile(`(?m)^A(\d+):\s*(.*)$`)

func agyParseAnswers(body string, count int) []string {
	matches := agyAnswerLineRe.FindAllStringSubmatch(body, -1)
	if len(matches) == 0 {
		return nil
	}
	ans := make([]string, count)
	found := false
	for _, m := range matches {
		idx, err := strconv.Atoi(m[1])
		if err != nil || idx < 1 || idx > count {
			continue
		}
		ans[idx-1] = clip(clean(strings.TrimSpace(m[2])), maxAskText)
		found = true
	}
	if !found {
		return nil
	}
	return ans
}
