package web

import (
	"bytes"
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
)

// lineageDiscovery is a confirmed child-creation event extracted from a parent
// agent's raw conversation log (or an agent_started enrichment for a pane
// already created by this parent).
type lineageDiscovery struct {
	ChildPane    string
	ChildTermID  string
	ChildSession string
	EvidenceKey  string
	Action       string // "workspace_created" | "tab_created" | "pane_split" | "agent_started"
}

var (
	herdrTermIDRe     = regexp.MustCompile(`^[A-Za-z0-9:_.-]{1,100}$`)
	agyTimestampRe    = regexp.MustCompile(`(?m)^\[\d{2}:\d{2}:\d{2}\]\s*`)
	codexExitCodeRe   = regexp.MustCompile(`(?i)(?:process exited with code|exit code:?)\s+(-?\d+)`)
	agyExitCodeRe     = regexp.MustCompile(`(?i)command exited with code\s+(-?\d+)`)
	simpleEnvAssignRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`)
)

const (
	lineageMaxPending = 128
	lineageMaxSeen    = 512
	lineageMaxCreated = 256
)

// lineageExtractor incrementally inspects raw JSONL log chunks (before display
// truncation) for Claude Code, Codex, and Antigravity CLI to detect provable
// herdr child-creation tool calls (workspace create, tab create, pane split)
// paired with matching successful JSON tool results.
type lineageExtractor struct {
	agent         string
	serverSocket  string
	serverSession string

	// Pending tool calls waiting for results.
	claudePending    map[string]herdrCmdIntent   // tool_use_id -> intent
	codexPending     map[string]herdrCmdIntent   // call_id -> intent
	codexCodePending map[string][]herdrCmdIntent // custom_tool_call call_id -> exec_command intents
	agyPending       []agyPendingIntent          // FIFO aligned with PLANNER_RESPONSE.tool_calls

	// Seen command/call IDs for deduplicating Codex CommandExecution vs function_call_output.
	seenIDs map[string]bool

	// Created panes/terms in this extractor, used ONLY to allow `herdr agent start`
	// to enrich child_session on an already-created pane (never standalone confirmation).
	createdByPane map[string]lineageDiscovery
	createdByTerm map[string]lineageDiscovery
}

type agyPendingIntent struct {
	id     string
	intent herdrCmdIntent
}

type herdrCmdIntent struct {
	action string // "workspace_created" | "tab_created" | "pane_split" | "agent_started"
	valid  bool
}

func newLineageExtractor(agent, serverSocket, serverSession string) *lineageExtractor {
	return &lineageExtractor{
		agent:         agent,
		serverSocket:  serverSocket,
		serverSession: serverSession,
	}
}

func (e *lineageExtractor) Reset() {
	e.claudePending = nil
	e.codexPending = nil
	e.codexCodePending = nil
	e.agyPending = nil
	e.seenIDs = nil
	e.createdByPane = nil
	e.createdByTerm = nil
}

// Feed consumes newline-delimited raw JSONL bytes and returns any newly
// confirmed discoveries (or enrichments for panes created earlier in this log).
func (e *lineageExtractor) Feed(lines []byte) []lineageDiscovery {
	if e == nil || len(lines) == 0 {
		return nil
	}
	var out []lineageDiscovery
	for len(lines) > 0 {
		i := bytes.IndexByte(lines, '\n')
		var line []byte
		if i < 0 {
			line = lines
			lines = nil
		} else {
			line = lines[:i]
			lines = lines[i+1:]
		}
		line = bytes.TrimSpace(line)
		if len(line) == 0 || line[0] != '{' {
			continue
		}
		switch e.agent {
		case "claude":
			out = append(out, e.feedClaudeLine(line)...)
		case "codex":
			out = append(out, e.feedCodexLine(line)...)
		case "agy":
			out = append(out, e.feedAgyLine(line)...)
		}
	}
	return out
}

func (e *lineageExtractor) recordSeen(id string) bool {
	if id == "" {
		return false
	}
	if e.seenIDs == nil {
		e.seenIDs = map[string]bool{}
	}
	if e.seenIDs[id] {
		return true
	}
	if len(e.seenIDs) >= lineageMaxSeen {
		e.seenIDs = map[string]bool{}
	}
	e.seenIDs[id] = true
	return false
}

func (e *lineageExtractor) rememberCreated(d lineageDiscovery) {
	if d.Action == "agent_started" {
		return
	}
	if e.createdByPane == nil {
		e.createdByPane = map[string]lineageDiscovery{}
		e.createdByTerm = map[string]lineageDiscovery{}
	}
	if len(e.createdByPane) >= lineageMaxCreated {
		e.createdByPane = map[string]lineageDiscovery{}
		e.createdByTerm = map[string]lineageDiscovery{}
	}
	if d.ChildPane != "" {
		e.createdByPane[d.ChildPane] = d
	}
	if d.ChildTermID != "" {
		e.createdByTerm[d.ChildTermID] = d
	}
}

func (e *lineageExtractor) enrichAgentStarted(paneID, termID, session string) (lineageDiscovery, bool) {
	if session == "" || (paneID == "" && termID == "") {
		return lineageDiscovery{}, false
	}
	var base lineageDiscovery
	var ok bool
	if termID != "" && e.createdByTerm != nil {
		base, ok = e.createdByTerm[termID]
	}
	if !ok && paneID != "" && e.createdByPane != nil {
		base, ok = e.createdByPane[paneID]
	}
	if !ok {
		return lineageDiscovery{}, false
	}
	if paneID != "" {
		base.ChildPane = paneID
	}
	if termID != "" {
		base.ChildTermID = termID
	}
	base.ChildSession = session
	base.Action = "agent_started"
	return base, true
}

// --- Claude Code JSONL ---

func (e *lineageExtractor) feedClaudeLine(line []byte) []lineageDiscovery {
	// Fast pre-filter before full JSON decode.
	if !bytes.Contains(line, []byte(`"tool_use"`)) && !bytes.Contains(line, []byte(`"tool_result"`)) {
		return nil
	}
	var row struct {
		Type        string `json:"type"`
		IsSidechain bool   `json:"isSidechain"`
		Message     struct {
			Content json.RawMessage `json:"content"`
		} `json:"message"`
	}
	if json.Unmarshal(line, &row) != nil || row.IsSidechain {
		return nil
	}
	var blocks []struct {
		Type      string          `json:"type"`
		ID        string          `json:"id"`
		Name      string          `json:"name"`
		Input     json.RawMessage `json:"input"`
		ToolUseID string          `json:"tool_use_id"`
		IsError   bool            `json:"is_error"`
		Content   json.RawMessage `json:"content"`
	}
	if json.Unmarshal(row.Message.Content, &blocks) != nil {
		return nil
	}
	var out []lineageDiscovery
	switch row.Type {
	case "assistant":
		for _, b := range blocks {
			if b.Type != "tool_use" || b.Name != "Bash" || b.ID == "" {
				continue
			}
			var in struct {
				Command string `json:"command"`
			}
			if json.Unmarshal(b.Input, &in) != nil {
				continue
			}
			intent := e.classifyHerdrCommand(in.Command)
			if !intent.valid {
				continue
			}
			if e.claudePending == nil {
				e.claudePending = map[string]herdrCmdIntent{}
			}
			if len(e.claudePending) >= lineageMaxPending {
				e.claudePending = map[string]herdrCmdIntent{}
			}
			e.claudePending[b.ID] = intent
		}
	case "user":
		for _, b := range blocks {
			if b.Type != "tool_result" || b.ToolUseID == "" {
				continue
			}
			intent, ok := e.claudePending[b.ToolUseID]
			if !ok {
				continue
			}
			delete(e.claudePending, b.ToolUseID)
			if b.IsError {
				continue
			}
			text := extractClaudeToolResultText(b.Content)
			if d, matched := e.matchResult(intent, text, "claude:"+b.ToolUseID); matched {
				out = append(out, d)
			}
		}
	}
	return out
}

func extractClaudeToolResultText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) == nil {
		var b strings.Builder
		for _, bl := range blocks {
			if bl.Type == "text" && bl.Text != "" {
				if b.Len() > 0 {
					b.WriteByte('\n')
				}
				b.WriteString(bl.Text)
			}
		}
		return b.String()
	}
	return ""
}

// --- Codex JSONL ---

func (e *lineageExtractor) feedCodexLine(line []byte) []lineageDiscovery {
	if !bytes.Contains(line, []byte(`"CommandExecution"`)) &&
		!bytes.Contains(line, []byte(`"function_call"`)) &&
		!bytes.Contains(line, []byte(`"function_call_output"`)) &&
		!bytes.Contains(line, []byte(`"custom_tool_call"`)) &&
		!bytes.Contains(line, []byte(`"custom_tool_call_output"`)) {
		return nil
	}
	var row struct {
		Type    string          `json:"type"`
		Payload json.RawMessage `json:"payload"`
	}
	if json.Unmarshal(line, &row) != nil {
		return nil
	}
	switch row.Type {
	case "event_msg":
		var p struct {
			Type string `json:"type"`
			Item *struct {
				Type     string          `json:"type"`
				ID       string          `json:"id"`
				Command  json.RawMessage `json:"command"`
				Status   string          `json:"status"`
				Output   string          `json:"aggregated_output"`
				ExitCode *int            `json:"exit_code"`
			} `json:"item"`
		}
		if json.Unmarshal(row.Payload, &p) != nil || p.Type != "item_completed" || p.Item == nil || p.Item.Type != "CommandExecution" {
			return nil
		}
		it := p.Item
		if it.Status == "failed" || (it.ExitCode != nil && *it.ExitCode != 0) {
			return nil
		}
		cmd := extractCodexCommand(it.Command)
		intent := e.classifyHerdrCommand(cmd)
		if !intent.valid {
			return nil
		}
		if e.recordSeen(it.ID) {
			return nil
		}
		key := "codex:" + it.ID
		if it.ID == "" {
			key = ""
		}
		if d, ok := e.matchResult(intent, it.Output, key); ok {
			return []lineageDiscovery{d}
		}
	case "response_item":
		var p struct {
			Type      string          `json:"type"`
			Name      string          `json:"name"`
			CallID    string          `json:"call_id"`
			Arguments json.RawMessage `json:"arguments"`
			Input     string          `json:"input"`
			Output    json.RawMessage `json:"output"`
		}
		if json.Unmarshal(row.Payload, &p) != nil {
			return nil
		}
		switch p.Type {
		case "function_call":
			if p.CallID == "" {
				return nil
			}
			switch p.Name {
			case "exec_command", "shell_command", "shell", "bash", "run_command":
			default:
				return nil
			}
			cmd := extractCodexFuncCallCmd(p.Arguments)
			intent := e.classifyHerdrCommand(cmd)
			if !intent.valid {
				return nil
			}
			if e.codexPending == nil {
				e.codexPending = map[string]herdrCmdIntent{}
			}
			if len(e.codexPending) >= lineageMaxPending {
				e.codexPending = map[string]herdrCmdIntent{}
			}
			e.codexPending[p.CallID] = intent
		case "function_call_output":
			if p.CallID == "" {
				return nil
			}
			intent, ok := e.codexPending[p.CallID]
			if !ok {
				return nil
			}
			delete(e.codexPending, p.CallID)
			if e.recordSeen(p.CallID) {
				return nil
			}
			outText := extractCodexOutputText(p.Output)
			if hasNonZeroExit(outText, codexExitCodeRe) {
				return nil
			}
			if d, matched := e.matchResult(intent, outText, "codex:"+p.CallID); matched {
				return []lineageDiscovery{d}
			}
		case "custom_tool_call":
			if p.CallID == "" || p.Name != "exec" || !strings.Contains(p.Input, "tools.exec_command") {
				return nil
			}
			cmds := extractCodexCodeModeCmds(p.Input)
			var intents []herdrCmdIntent
			for _, cmd := range cmds {
				if intent := e.classifyHerdrCommand(cmd); intent.valid {
					intents = append(intents, intent)
				}
			}
			if len(intents) == 0 {
				return nil
			}
			if e.codexCodePending == nil {
				e.codexCodePending = map[string][]herdrCmdIntent{}
			}
			if len(e.codexCodePending) >= lineageMaxPending {
				e.codexCodePending = map[string][]herdrCmdIntent{}
			}
			e.codexCodePending[p.CallID] = intents
		case "custom_tool_call_output":
			if p.CallID == "" {
				return nil
			}
			intents, ok := e.codexCodePending[p.CallID]
			if !ok {
				return nil
			}
			delete(e.codexCodePending, p.CallID)
			if e.recordSeen(p.CallID) {
				return nil
			}
			outText := extractCodexOutputText(p.Output)
			if strings.HasPrefix(strings.TrimSpace(outText), "Script failed") || hasNonZeroExit(outText, codexExitCodeRe) {
				return nil
			}
			var out []lineageDiscovery
			for i, intent := range intents {
				key := "codex:" + p.CallID
				if len(intents) > 1 {
					key += ":" + strconv.Itoa(i)
				}
				if d, matched := e.matchResult(intent, outText, key); matched {
					out = append(out, d)
				}
			}
			return out
		}
	}
	return nil
}

func extractCodexOutputText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) == nil {
		var b strings.Builder
		for _, bl := range blocks {
			if bl.Text != "" {
				b.WriteString(bl.Text)
			}
		}
		return b.String()
	}
	return ""
}

// extractCodexCodeModeCmds extracts literal `cmd` strings from
// `tools.exec_command({cmd: "..."})` calls inside a Codex code-mode script.
func extractCodexCodeModeCmds(script string) []string {
	var out []string
	const marker = "tools.exec_command("
	for off := 0; off < len(script); {
		idx := strings.Index(script[off:], marker)
		if idx < 0 {
			break
		}
		pos := off + idx + len(marker)
		rest := script[pos:]
		// Locate `cmd` key inside the argument object.
		cIdx := strings.Index(rest, "cmd")
		if cIdx < 0 || cIdx > 64 {
			off = pos
			continue
		}
		after := strings.TrimSpace(rest[cIdx+3:])
		// Optional quote around key name ("cmd": ...)
		if strings.HasPrefix(after, `"`) || strings.HasPrefix(after, `'`) {
			after = strings.TrimSpace(after[1:])
		}
		if !strings.HasPrefix(after, ":") {
			off = pos
			continue
		}
		after = strings.TrimSpace(after[1:])
		if len(after) == 0 || (after[0] != '"' && after[0] != '\'') {
			off = pos
			continue
		}
		q := after[0]
		var buf strings.Builder
		esc := false
		end := -1
		for i := 1; i < len(after); i++ {
			ch := after[i]
			if esc {
				switch ch {
				case 'n':
					buf.WriteByte('\n')
				case 'r':
					buf.WriteByte('\r')
				case 't':
					buf.WriteByte('\t')
				default:
					buf.WriteByte(ch)
				}
				esc = false
				continue
			}
			if ch == '\\' {
				esc = true
				continue
			}
			if ch == q {
				end = i
				break
			}
			buf.WriteByte(ch)
		}
		if end > 0 {
			out = append(out, buf.String())
			off = pos + cIdx + 3
		} else {
			off = pos
		}
	}
	return out
}

func extractCodexCommand(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var argv []string
	if json.Unmarshal(raw, &argv) == nil {
		return unwrapShellArgv(argv)
	}
	return ""
}

func extractCodexFuncCallCmd(raw json.RawMessage) string {
	// Arguments may be a JSON-encoded string containing JSON, or a JSON object.
	var asStr string
	if json.Unmarshal(raw, &asStr) == nil {
		raw = []byte(asStr)
	}
	var obj struct {
		Cmd     string          `json:"cmd"`
		Command json.RawMessage `json:"command"`
	}
	if json.Unmarshal(raw, &obj) != nil {
		return ""
	}
	if obj.Cmd != "" {
		return obj.Cmd
	}
	return extractCodexCommand(obj.Command)
}

func unwrapShellArgv(argv []string) string {
	if len(argv) == 0 {
		return ""
	}
	// Handle ["/bin/bash", "-lc", "..."] or ["sh", "-c", "..."]
	if len(argv) >= 3 {
		base := argv[0]
		if idx := strings.LastIndexByte(base, '/'); idx >= 0 {
			base = base[idx+1:]
		}
		if (base == "sh" || base == "bash" || base == "zsh" || base == "dash") &&
			(argv[1] == "-c" || argv[1] == "-lc" || argv[1] == "-cl") {
			return argv[2]
		}
	}
	// Direct argv invocation (e.g. ["herdr", "pane", "split", "--json"]).
	var parts []string
	for _, a := range argv {
		if strings.ContainsAny(a, " \t'\"") {
			parts = append(parts, "'"+strings.ReplaceAll(a, "'", `'\''`)+"'")
		} else {
			parts = append(parts, a)
		}
	}
	return strings.Join(parts, " ")
}

// --- Antigravity CLI (agy) JSONL ---

func (e *lineageExtractor) feedAgyLine(line []byte) []lineageDiscovery {
	var row struct {
		StepIndex int    `json:"step_index"`
		Type      string `json:"type"`
		Status    string `json:"status"`
		Content   string `json:"content"`
		ToolCalls []struct {
			Name string          `json:"name"`
			Args json.RawMessage `json:"args"`
		} `json:"tool_calls"`
	}
	if json.Unmarshal(line, &row) != nil || row.Type == "" {
		return nil
	}
	switch row.Type {
	case "USER_INPUT":
		e.agyPending = nil
	case "PLANNER_RESPONSE":
		if len(row.ToolCalls) > 0 {
			e.agyPending = e.agyPending[:0]
			for idx, tc := range row.ToolCalls {
				if len(e.agyPending) >= lineageMaxPending {
					break
				}
				var intent herdrCmdIntent
				if tc.Name == "run_command" {
					var args struct {
						CommandLine string `json:"CommandLine"`
					}
					if json.Unmarshal(tc.Args, &args) == nil {
						intent = e.classifyHerdrCommand(args.CommandLine)
					}
				}
				e.agyPending = append(e.agyPending, agyPendingIntent{
					id:     "agy:" + strconv.Itoa(row.StepIndex) + ":" + strconv.Itoa(idx),
					intent: intent,
				})
			}
		}
	case "GENERIC":
		if len(e.agyPending) == 0 {
			return nil
		}
		head := e.agyPending[0]
		e.agyPending = e.agyPending[1:]
		if !head.intent.valid {
			return nil
		}
		if row.Status == "ERROR" || row.Status == "RUNNING" {
			return nil
		}
		body := agyTimestampRe.ReplaceAllString(row.Content, "")
		if hasNonZeroExit(body, agyExitCodeRe) {
			return nil
		}
		if d, matched := e.matchResult(head.intent, body, head.id); matched {
			return []lineageDiscovery{d}
		}
	}
	return nil
}

func hasNonZeroExit(text string, re *regexp.Regexp) bool {
	m := re.FindStringSubmatch(text)
	if len(m) < 2 {
		return false
	}
	n, err := strconv.Atoi(m[1])
	return err == nil && n != 0
}

// --- Strict simple-command shell proof ---

// classifyHerdrCommand validates that rawCmd is a simple, provable invocation
// of a herdr creation/start command targeting this herdr server (no pipes,
// subshells, `;`/`||` chains, heredocs, `--remote`, `ssh`, `docker`, `--help`,
// or mismatched HERDR_SOCKET_PATH / --socket / --session).
func (e *lineageExtractor) classifyHerdrCommand(rawCmd string) herdrCmdIntent {
	cmd := strings.TrimSpace(rawCmd)
	if cmd == "" || len(cmd) > 4096 {
		return herdrCmdIntent{}
	}
	// Reject multiline scripts, heredocs, subshells, pipes, `;`, `||`, background `&`.
	if strings.ContainsAny(cmd, "\r\n;`") || strings.Contains(cmd, "$(") || strings.Contains(cmd, "<<") || strings.Contains(cmd, "||") {
		return herdrCmdIntent{}
	}
	// Split on "&&": allow only harmless `cd ...` or `export K=V` prefix stages before the final herdr command.
	stages := splitAndAnd(cmd)
	if len(stages) == 0 || len(stages) > 4 {
		return herdrCmdIntent{}
	}

	envOverrides := map[string]string{}
	for i := 0; i < len(stages)-1; i++ {
		words, ok := splitSimpleShellWords(stages[i])
		if !ok || len(words) == 0 {
			return herdrCmdIntent{}
		}
		switch words[0] {
		case "cd":
			if len(words) > 2 {
				return herdrCmdIntent{}
			}
		case "export":
			for _, w := range words[1:] {
				if !simpleEnvAssignRe.MatchString(w) {
					return herdrCmdIntent{}
				}
				k, v, _ := strings.Cut(w, "=")
				envOverrides[k] = v
			}
		default:
			// Allow pure env assignments only if there is nothing else.
			for _, w := range words {
				if !simpleEnvAssignRe.MatchString(w) {
					return herdrCmdIntent{}
				}
				k, v, _ := strings.Cut(w, "=")
				envOverrides[k] = v
			}
		}
	}

	words, ok := splitSimpleShellWords(stages[len(stages)-1])
	if !ok || len(words) == 0 {
		return herdrCmdIntent{}
	}

	// Consume leading inline VAR=val assignments.
	idx := 0
	for idx < len(words) && simpleEnvAssignRe.MatchString(words[idx]) {
		k, v, _ := strings.Cut(words[idx], "=")
		envOverrides[k] = v
		idx++
	}
	if idx >= len(words) {
		return herdrCmdIntent{}
	}

	// Executable must be `herdr` (or `/path/to/herdr`), NOT echo/printf/ssh/docker/etc.
	bin := words[idx]
	if slash := strings.LastIndexByte(bin, '/'); slash >= 0 {
		bin = bin[slash+1:]
	}
	if bin != "herdr" {
		return herdrCmdIntent{}
	}

	// Verify HERDR_SOCKET_PATH env override if set.
	if sock, set := envOverrides["HERDR_SOCKET_PATH"]; set {
		if sock == "" || (e.serverSocket != "" && sock != e.serverSocket) {
			return herdrCmdIntent{}
		}
		// If we don't know our own serverSocket, reject unverified socket override.
		if e.serverSocket == "" {
			return herdrCmdIntent{}
		}
	}

	args := words[idx+1:]
	var pos []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		// Allow harmless stderr redirections like 2>&1 or 2>/dev/null.
		if a == "2>&1" || a == "2>/dev/null" {
			continue
		}
		if a == "--" {
			break
		}
		if a == "-h" || a == "--help" || a == "help" {
			return herdrCmdIntent{}
		}
		if a == "--remote" || strings.HasPrefix(a, "--remote=") {
			return herdrCmdIntent{}
		}
		if a == "--socket" || strings.HasPrefix(a, "--socket=") {
			var v string
			if a == "--socket" {
				if i+1 >= len(args) {
					return herdrCmdIntent{}
				}
				i++
				v = args[i]
			} else {
				_, v, _ = strings.Cut(a, "=")
			}
			if v == "" || e.serverSocket == "" || v != e.serverSocket {
				return herdrCmdIntent{}
			}
			continue
		}
		if a == "--session" || strings.HasPrefix(a, "--session=") || a == "-s" {
			var v string
			if a == "--session" || a == "-s" {
				if i+1 >= len(args) {
					return herdrCmdIntent{}
				}
				i++
				v = args[i]
			} else {
				_, v, _ = strings.Cut(a, "=")
			}
			if v == "" || e.serverSession == "" || v != e.serverSession {
				return herdrCmdIntent{}
			}
			continue
		}
		if strings.HasPrefix(a, "-") {
			// Skip value of flags that take an argument (when not `--flag=val`).
			if !strings.Contains(a, "=") && herdrFlagTakesValue(a) && i+1 < len(args) {
				i++
			}
			continue
		}
		pos = append(pos, a)
	}

	if len(pos) < 2 {
		return herdrCmdIntent{}
	}
	switch pos[0] + " " + pos[1] {
	case "workspace create":
		return herdrCmdIntent{action: "workspace_created", valid: true}
	case "tab create":
		return herdrCmdIntent{action: "tab_created", valid: true}
	case "pane split":
		return herdrCmdIntent{action: "pane_split", valid: true}
	case "agent start":
		return herdrCmdIntent{action: "agent_started", valid: true}
	default:
		return herdrCmdIntent{}
	}
}

func herdrFlagTakesValue(flag string) bool {
	switch flag {
	case "--cwd", "--label", "--direction", "-d", "--pane", "--workspace", "--tab",
		"--kind", "--timeout", "--env", "-e", "--name", "--source", "--token",
		"--ttl-ms", "--profile", "--workdir", "--command", "--ratio", "--size":
		return true
	}
	return false
}

// splitAndAnd splits a shell line on unquoted `&&` while rejecting unquoted `|`,
// single `&` (except inside `2>&1`), `<`, or `>` (except `2>&1` / `2>/dev/null`).
func splitAndAnd(s string) []string {
	var parts []string
	var cur strings.Builder
	var quote byte
	escaped := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if escaped {
			cur.WriteByte(c)
			escaped = false
			continue
		}
		if quote == '\'' {
			if c == '\'' {
				quote = 0
			}
			cur.WriteByte(c)
			continue
		}
		if quote == '"' {
			if c == '\\' {
				escaped = true
				cur.WriteByte(c)
				continue
			}
			if c == '"' {
				quote = 0
			}
			cur.WriteByte(c)
			continue
		}
		switch c {
		case '\\':
			escaped = true
			cur.WriteByte(c)
		case '\'', '"':
			quote = c
			cur.WriteByte(c)
		case '|':
			return nil
		case '&':
			if i+1 < len(s) && s[i+1] == '&' {
				parts = append(parts, strings.TrimSpace(cur.String()))
				cur.Reset()
				i++
				continue
			}
			// Allow 2>&1 only.
			if i >= 2 && s[i-2:i] == "2>" && i+1 < len(s) && s[i+1] == '1' {
				cur.WriteByte(c)
				continue
			}
			return nil
		case '#':
			// Comment at word start rejects the command.
			if cur.Len() == 0 || s[i-1] == ' ' || s[i-1] == '\t' {
				return nil
			}
			cur.WriteByte(c)
		default:
			cur.WriteByte(c)
		}
	}
	if quote != 0 || escaped {
		return nil
	}
	if tail := strings.TrimSpace(cur.String()); tail != "" {
		parts = append(parts, tail)
	}
	return parts
}

func splitSimpleShellWords(s string) ([]string, bool) {
	var words []string
	var cur strings.Builder
	var quote byte
	inWord := false
	escaped := false

	for i := 0; i < len(s); i++ {
		c := s[i]
		if escaped {
			cur.WriteByte(c)
			inWord = true
			escaped = false
			continue
		}
		if quote == '\'' {
			if c == '\'' {
				quote = 0
			} else {
				cur.WriteByte(c)
			}
			inWord = true
			continue
		}
		if quote == '"' {
			if c == '\\' && i+1 < len(s) {
				escaped = true
				continue
			}
			if c == '"' {
				quote = 0
			} else {
				cur.WriteByte(c)
			}
			inWord = true
			continue
		}
		switch c {
		case ' ', '\t':
			if inWord {
				words = append(words, cur.String())
				cur.Reset()
				inWord = false
			}
		case '\\':
			escaped = true
			inWord = true
		case '\'', '"':
			quote = c
			inWord = true
		case '<':
			return nil, false
		case '>':
			// Allow only exact 2>&1 or 2>/dev/null tokens.
			if cur.String() == "2" && (strings.HasPrefix(s[i:], ">&1") || strings.HasPrefix(s[i:], ">/dev/null")) {
				cur.WriteByte(c)
				inWord = true
				continue
			}
			return nil, false
		default:
			cur.WriteByte(c)
			inWord = true
		}
	}
	if quote != 0 || escaped {
		return nil, false
	}
	if inWord {
		words = append(words, cur.String())
	}
	return words, true
}

// --- Structured JSON result matching ---

type herdrResultPayload struct {
	Type     string `json:"type"`
	RootPane *struct {
		PaneID     string `json:"pane_id"`
		TerminalID string `json:"terminal_id"`
	} `json:"root_pane"`
	Pane *struct {
		PaneID     string `json:"pane_id"`
		TerminalID string `json:"terminal_id"`
	} `json:"pane"`
	Agent *struct {
		PaneID       string `json:"pane_id"`
		TerminalID   string `json:"terminal_id"`
		AgentSession *struct {
			Value string `json:"value"`
		} `json:"agent_session"`
	} `json:"agent"`
}

func (e *lineageExtractor) matchResult(intent herdrCmdIntent, output, evidenceKey string) (lineageDiscovery, bool) {
	if !intent.valid || strings.TrimSpace(output) == "" {
		return lineageDiscovery{}, false
	}
	payload, ok := findMatchingHerdrJSON(output, intent.action)
	if !ok {
		return lineageDiscovery{}, false
	}
	switch intent.action {
	case "workspace_created", "tab_created":
		if payload.RootPane == nil || !herdrPaneID.MatchString(payload.RootPane.PaneID) {
			return lineageDiscovery{}, false
		}
		termID := sanitizeTermID(payload.RootPane.TerminalID)
		d := lineageDiscovery{
			ChildPane:   payload.RootPane.PaneID,
			ChildTermID: termID,
			EvidenceKey: evidenceKey,
			Action:      intent.action,
		}
		e.rememberCreated(d)
		return d, true
	case "pane_split":
		if payload.Pane == nil || !herdrPaneID.MatchString(payload.Pane.PaneID) {
			return lineageDiscovery{}, false
		}
		termID := sanitizeTermID(payload.Pane.TerminalID)
		d := lineageDiscovery{
			ChildPane:   payload.Pane.PaneID,
			ChildTermID: termID,
			EvidenceKey: evidenceKey,
			Action:      intent.action,
		}
		e.rememberCreated(d)
		return d, true
	case "agent_started":
		if payload.Agent == nil {
			return lineageDiscovery{}, false
		}
		paneID := ""
		if herdrPaneID.MatchString(payload.Agent.PaneID) {
			paneID = payload.Agent.PaneID
		}
		termID := sanitizeTermID(payload.Agent.TerminalID)
		session := ""
		if payload.Agent.AgentSession != nil && len(payload.Agent.AgentSession.Value) <= 128 {
			session = payload.Agent.AgentSession.Value
		}
		// Enrichment only: only emits if this extractor already saw creation of this pane/term!
		return e.enrichAgentStarted(paneID, termID, session)
	default:
		return lineageDiscovery{}, false
	}
}

func herdrResultTypeMatches(want, got string) bool {
	if got == want {
		return true
	}
	// Real Herdr v0.8.0 `pane.split` returns `result.type = "pane_info"`.
	if want == "pane_split" && got == "pane_info" {
		return true
	}
	if want == "agent_started" && got == "agent_info" {
		return true
	}
	return false
}

func sanitizeTermID(s string) string {
	if s != "" && herdrTermIDRe.MatchString(s) {
		return s
	}
	return ""
}

func findMatchingHerdrJSON(output, wantType string) (herdrResultPayload, bool) {
	// Try whole output first (handles pretty-printed multi-line JSON),
	// then individual JSON objects / lines (handles leading stdout/stderr chatter).
	tryDecode := func(raw []byte) (herdrResultPayload, bool) {
		raw = bytes.TrimSpace(raw)
		if len(raw) < 2 || raw[0] != '{' {
			return herdrResultPayload{}, false
		}
		var env struct {
			OK     *bool           `json:"ok"`
			Error  json.RawMessage `json:"error"`
			Result json.RawMessage `json:"result"`
			Type   string          `json:"type"`
		}
		if json.Unmarshal(raw, &env) != nil {
			return herdrResultPayload{}, false
		}
		if env.OK != nil && !*env.OK {
			return herdrResultPayload{}, false
		}
		if len(env.Error) > 0 && string(bytes.TrimSpace(env.Error)) != "null" {
			return herdrResultPayload{}, false
		}
		target := raw
		if len(env.Result) > 0 && bytes.TrimSpace(env.Result)[0] == '{' {
			target = env.Result
		}
		var p herdrResultPayload
		if json.Unmarshal(target, &p) != nil {
			return herdrResultPayload{}, false
		}
		if !herdrResultTypeMatches(wantType, p.Type) {
			return herdrResultPayload{}, false
		}
		return p, true
	}

	if p, ok := tryDecode([]byte(output)); ok {
		return p, true
	}

	// Scan for JSON objects starting at '{' in the output using json.Decoder.
	b := []byte(output)
	for off := 0; off < len(b); {
		idx := bytes.IndexByte(b[off:], '{')
		if idx < 0 {
			break
		}
		off += idx
		dec := json.NewDecoder(bytes.NewReader(b[off:]))
		var raw json.RawMessage
		if err := dec.Decode(&raw); err == nil {
			if p, ok := tryDecode(raw); ok {
				return p, true
			}
			off += int(dec.InputOffset())
		} else {
			off++
		}
	}
	return herdrResultPayload{}, false
}
