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
	herdrPaneTokenRe  = regexp.MustCompile(`\b(w[A-Za-z0-9]+:p\d+)\b`)
	herdrTermTokenRe  = regexp.MustCompile(`"terminal_id"\s*:\s*"([A-Za-z0-9:_.-]{1,100})"`)
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
	claudePending    map[string][]herdrCmdIntent // tool_use_id -> intents
	codexPending     map[string][]herdrCmdIntent // call_id -> intents
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
	id      string
	intents []herdrCmdIntent
}

type herdrCmdIntent struct {
	action     string // "workspace_created" | "tab_created" | "pane_split" | "agent_started"
	valid      bool
	cmdPanes   map[string]bool // pane IDs literally mentioned in the command text (excluded from filtered-output fallback)
	hasListing bool            // true if script also ran herdr list/snapshot (disables filtered-output fallback)
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
			intents := e.classifyHerdrCommands(in.Command)
			if len(intents) == 0 {
				continue
			}
			if e.claudePending == nil {
				e.claudePending = map[string][]herdrCmdIntent{}
			}
			if len(e.claudePending) >= lineageMaxPending {
				e.claudePending = map[string][]herdrCmdIntent{}
			}
			e.claudePending[b.ID] = intents
		}
	case "user":
		for _, b := range blocks {
			if b.Type != "tool_result" || b.ToolUseID == "" {
				continue
			}
			intents, ok := e.claudePending[b.ToolUseID]
			if !ok {
				continue
			}
			delete(e.claudePending, b.ToolUseID)
			if b.IsError {
				continue
			}
			text := extractClaudeToolResultText(b.Content)
			out = append(out, e.matchMultiResults(intents, text, "claude:"+b.ToolUseID)...)
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
		intents := e.classifyHerdrCommands(cmd)
		if len(intents) == 0 {
			return nil
		}
		if e.recordSeen(it.ID) {
			return nil
		}
		key := "codex:" + it.ID
		if it.ID == "" {
			key = ""
		}
		return e.matchMultiResults(intents, it.Output, key)
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
			intents := e.classifyHerdrCommands(cmd)
			if len(intents) == 0 {
				return nil
			}
			if e.codexPending == nil {
				e.codexPending = map[string][]herdrCmdIntent{}
			}
			if len(e.codexPending) >= lineageMaxPending {
				e.codexPending = map[string][]herdrCmdIntent{}
			}
			e.codexPending[p.CallID] = intents
		case "function_call_output":
			if p.CallID == "" {
				return nil
			}
			intents, ok := e.codexPending[p.CallID]
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
			return e.matchMultiResults(intents, outText, "codex:"+p.CallID)
		case "custom_tool_call":
			if p.CallID == "" || p.Name != "exec" || !strings.Contains(p.Input, "tools.exec_command") {
				return nil
			}
			cmds := extractCodexCodeModeCmds(p.Input)
			var intents []herdrCmdIntent
			for _, cmd := range cmds {
				intents = append(intents, e.classifyHerdrCommands(cmd)...)
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
			return e.matchMultiResults(intents, outText, "codex:"+p.CallID)
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
				var intents []herdrCmdIntent
				if tc.Name == "run_command" {
					var args struct {
						CommandLine string `json:"CommandLine"`
					}
					if json.Unmarshal(tc.Args, &args) == nil {
						intents = e.classifyHerdrCommands(args.CommandLine)
					}
				}
				e.agyPending = append(e.agyPending, agyPendingIntent{
					id:      "agy:" + strconv.Itoa(row.StepIndex) + ":" + strconv.Itoa(idx),
					intents: intents,
				})
			}
		}
	case "GENERIC":
		if len(e.agyPending) == 0 {
			return nil
		}
		head := e.agyPending[0]
		e.agyPending = e.agyPending[1:]
		if len(head.intents) == 0 {
			return nil
		}
		if row.Status == "ERROR" || row.Status == "RUNNING" {
			return nil
		}
		body := agyTimestampRe.ReplaceAllString(row.Content, "")
		if hasNonZeroExit(body, agyExitCodeRe) {
			return nil
		}
		return e.matchMultiResults(head.intents, body, head.id)
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

// --- Safe shell proof (supports real agent scripts: pipes to head/tail/jq, VAR=$(herdr ...), multi-line) ---

var safePipeReaders = map[string]bool{
	"head": true, "tail": true, "cat": true, "jq": true, "tee": true,
	"cut": true, "tr": true, "sed": true, "awk": true, "grep": true,
	"rg": true, "python": true, "python3": true, "node": true,
}

// classifyHerdrCommand checks if rawCmd contains at least one provable herdr
// creation/start invocation (kept for single-command tests).
func (e *lineageExtractor) classifyHerdrCommand(rawCmd string) herdrCmdIntent {
	list := e.classifyHerdrCommands(rawCmd)
	if len(list) == 0 {
		return herdrCmdIntent{}
	}
	return list[0]
}

// classifyHerdrCommands extracts every provable herdr creation/start command
// targeting this herdr server from a shell command or multi-statement script
// (supporting real agent idioms like `herdr ... 2>&1 | head -c 800` and
// `W=$(herdr workspace create ...); echo "$W"`), while strictly rejecting
// heredocs, `||` error fallbacks, `--remote`, `ssh`, `docker exec`, `--help`,
// echo/comment spoofing, or mismatched HERDR_SOCKET_PATH / --socket / --session.
func (e *lineageExtractor) classifyHerdrCommands(rawCmd string) []herdrCmdIntent {
	cmd := strings.TrimSpace(rawCmd)
	if cmd == "" || len(cmd) > 16384 || !strings.Contains(cmd, "herdr") {
		return nil
	}
	// Reject heredocs, backticks, fallback `||`, and self-spoofed result types in the command text.
	if strings.Contains(cmd, "<<") || strings.Contains(cmd, "`") || strings.Contains(cmd, "||") {
		return nil
	}
	if strings.Contains(cmd, "workspace_created") || strings.Contains(cmd, "tab_created") ||
		strings.Contains(cmd, "pane_info") || strings.Contains(cmd, "pane_split") {
		return nil
	}

	stmts, ok := splitShellStatements(cmd)
	if !ok || len(stmts) == 0 || len(stmts) > 32 {
		return nil
	}

	// Reject entire script if any statement invokes a remote host or container.
	envOverrides := map[string]string{}
	hasListing := false
	for _, st := range stmts {
		if stmtHasRemoteExec(st) {
			return nil
		}
		if stmtHasHerdrListing(st) {
			hasListing = true
		}
		if words, ok := splitSimpleShellWords(st); ok && len(words) >= 2 && words[0] == "export" {
			for _, w := range words[1:] {
				if simpleEnvAssignRe.MatchString(w) {
					k, v, _ := strings.Cut(w, "=")
					envOverrides[k] = v
				}
			}
		}
	}
	if sock, set := envOverrides["HERDR_SOCKET_PATH"]; set {
		if sock == "" || e.serverSocket == "" || sock != e.serverSocket {
			return nil
		}
	}

	var cmdPanes map[string]bool
	if matches := herdrPaneTokenRe.FindAllStringSubmatch(cmd, -1); len(matches) > 0 {
		cmdPanes = make(map[string]bool, len(matches))
		for _, m := range matches {
			cmdPanes[m[1]] = true
		}
	}

	var out []herdrCmdIntent
	for _, st := range stmts {
		// Unwrap simple variable capture: VAR=$(herdr ...) or export VAR=$(herdr ...)
		candidates := []string{st}
		if inner, captured := unwrapCommandSubAssign(st); captured {
			candidates = []string{inner}
		}
		for _, cand := range candidates {
			if intent, ok := e.classifyPipeline(cand, envOverrides); ok {
				intent.cmdPanes = cmdPanes
				intent.hasListing = hasListing
				out = append(out, intent)
			}
		}
	}
	return out
}

func stmtHasHerdrListing(stmt string) bool {
	if !strings.Contains(stmt, "herdr") {
		return false
	}
	return strings.Contains(stmt, " list") || strings.Contains(stmt, " snapshot")
}

func stmtHasRemoteExec(stmt string) bool {
	pipes, ok := splitShellPipes(stmt)
	if !ok {
		return false
	}
	for _, seg := range pipes {
		words, ok := splitSimpleShellWords(seg)
		if !ok || len(words) == 0 {
			continue
		}
		idx := 0
		for idx < len(words) && simpleEnvAssignRe.MatchString(words[idx]) {
			idx++
		}
		if idx >= len(words) {
			continue
		}
		bin := baseCmdName(words[idx])
		if bin == "ssh" || bin == "docker" || bin == "podman" || bin == "kubectl" {
			return true
		}
		for _, w := range words[idx+1:] {
			if w == "--remote" || strings.HasPrefix(w, "--remote=") {
				return true
			}
		}
	}
	return false
}

func baseCmdName(bin string) string {
	if slash := strings.LastIndexByte(bin, '/'); slash >= 0 {
		return bin[slash+1:]
	}
	return bin
}

// unwrapCommandSubAssign unwraps `VAR=$(...)` or `export VAR=$(...)` when the
// right-hand side is a single balanced `$(...)` expression.
func unwrapCommandSubAssign(stmt string) (string, bool) {
	s := strings.TrimSpace(stmt)
	if strings.HasPrefix(s, "export ") {
		s = strings.TrimSpace(strings.TrimPrefix(s, "export "))
	}
	eq := strings.IndexByte(s, '=')
	if eq <= 0 || !simpleEnvAssignRe.MatchString(s[:eq+1]) {
		return "", false
	}
	rhs := strings.TrimSpace(s[eq+1:])
	if strings.HasPrefix(rhs, `"`) && strings.HasSuffix(rhs, `"`) && len(rhs) >= 2 {
		rhs = rhs[1 : len(rhs)-1]
	}
	if !strings.HasPrefix(rhs, "$(") || !strings.HasSuffix(rhs, ")") {
		return "", false
	}
	// Ensure the opening `$(` matches the final `)` (no trailing tokens).
	depth := 0
	var quote byte
	esc := false
	for i := 0; i < len(rhs); i++ {
		c := rhs[i]
		if esc {
			esc = false
			continue
		}
		if quote == '\'' {
			if c == '\'' {
				quote = 0
			}
			continue
		}
		if quote == '"' {
			if c == '\\' {
				esc = true
			} else if c == '"' {
				quote = 0
			}
			continue
		}
		switch c {
		case '\\':
			esc = true
		case '\'', '"':
			quote = c
		case '$':
			if i+1 < len(rhs) && rhs[i+1] == '(' {
				depth++
				i++
			}
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 && i != len(rhs)-1 {
				return "", false
			}
		}
	}
	if depth != 0 {
		return "", false
	}
	inner := strings.TrimSpace(rhs[2 : len(rhs)-1])
	if inner == "" || strings.Contains(inner, "$(") {
		return "", false
	}
	return inner, true
}

func (e *lineageExtractor) classifyPipeline(stmt string, inheritedEnv map[string]string) (herdrCmdIntent, bool) {
	if strings.Contains(stmt, "$(") {
		return herdrCmdIntent{}, false
	}
	pipes, ok := splitShellPipes(stmt)
	if !ok || len(pipes) == 0 || len(pipes) > 4 {
		return herdrCmdIntent{}, false
	}
	// Downstream pipe stages (if any) must be harmless output readers/formatters.
	for i := 1; i < len(pipes); i++ {
		pwords, pok := splitSimpleShellWords(pipes[i])
		if !pok || len(pwords) == 0 {
			return herdrCmdIntent{}, false
		}
		idx := 0
		for idx < len(pwords) && simpleEnvAssignRe.MatchString(pwords[idx]) {
			idx++
		}
		if idx >= len(pwords) || !safePipeReaders[baseCmdName(pwords[idx])] {
			return herdrCmdIntent{}, false
		}
	}

	words, ok := splitSimpleShellWords(pipes[0])
	if !ok || len(words) == 0 {
		return herdrCmdIntent{}, false
	}

	envOverrides := map[string]string{}
	for k, v := range inheritedEnv {
		envOverrides[k] = v
	}
	// Consume leading inline VAR=val assignments.
	idx := 0
	for idx < len(words) && simpleEnvAssignRe.MatchString(words[idx]) {
		k, v, _ := strings.Cut(words[idx], "=")
		envOverrides[k] = v
		idx++
	}
	if idx >= len(words) {
		return herdrCmdIntent{}, false
	}

	// Executable must be `herdr` (or `/path/to/herdr`), NOT echo/printf/ssh/docker/etc.
	if baseCmdName(words[idx]) != "herdr" {
		return herdrCmdIntent{}, false
	}

	// Verify HERDR_SOCKET_PATH env override if set.
	if sock, set := envOverrides["HERDR_SOCKET_PATH"]; set {
		if sock == "" || e.serverSocket == "" || sock != e.serverSocket {
			return herdrCmdIntent{}, false
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
			return herdrCmdIntent{}, false
		}
		if a == "--remote" || strings.HasPrefix(a, "--remote=") {
			return herdrCmdIntent{}, false
		}
		if a == "--socket" || strings.HasPrefix(a, "--socket=") {
			var v string
			if a == "--socket" {
				if i+1 >= len(args) {
					return herdrCmdIntent{}, false
				}
				i++
				v = args[i]
			} else {
				_, v, _ = strings.Cut(a, "=")
			}
			if v == "" || e.serverSocket == "" || v != e.serverSocket {
				return herdrCmdIntent{}, false
			}
			continue
		}
		if a == "--session" || strings.HasPrefix(a, "--session=") || a == "-s" {
			var v string
			if a == "--session" || a == "-s" {
				if i+1 >= len(args) {
					return herdrCmdIntent{}, false
				}
				i++
				v = args[i]
			} else {
				_, v, _ = strings.Cut(a, "=")
			}
			if v == "" || e.serverSession == "" || v != e.serverSession {
				return herdrCmdIntent{}, false
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
		return herdrCmdIntent{}, false
	}
	switch pos[0] + " " + pos[1] {
	case "workspace create":
		return herdrCmdIntent{action: "workspace_created", valid: true}, true
	case "tab create":
		return herdrCmdIntent{action: "tab_created", valid: true}, true
	case "pane split":
		return herdrCmdIntent{action: "pane_split", valid: true}, true
	case "agent start":
		return herdrCmdIntent{action: "agent_started", valid: true}, true
	default:
		return herdrCmdIntent{}, false
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

// splitShellStatements splits a shell script on unquoted `\n`, `;`, and `&&`
// while stripping unquoted `# ...` line comments and respecting quotes & `$(...)`.
func splitShellStatements(s string) ([]string, bool) {
	var parts []string
	var cur strings.Builder
	var quote byte
	escaped := false
	parenDepth := 0
	flush := func() {
		if t := strings.TrimSpace(cur.String()); t != "" {
			parts = append(parts, t)
		}
		cur.Reset()
	}

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
		case '(':
			parenDepth++
			cur.WriteByte(c)
		case ')':
			if parenDepth > 0 {
				parenDepth--
			}
			cur.WriteByte(c)
		case '#':
			if parenDepth == 0 && (cur.Len() == 0 || s[i-1] == ' ' || s[i-1] == '\t' || s[i-1] == '\n' || s[i-1] == ';') {
				// Skip comment through end of line.
				for i < len(s) && s[i] != '\n' {
					i++
				}
				flush()
				continue
			}
			cur.WriteByte(c)
		case '\r', '\n', ';':
			if parenDepth == 0 {
				flush()
				continue
			}
			cur.WriteByte(c)
		case '&':
			if parenDepth == 0 && i+1 < len(s) && s[i+1] == '&' {
				flush()
				i++
				continue
			}
			// Allow 2>&1; reject backgrounding `&` at top level.
			if i >= 2 && s[i-2:i] == "2>" && i+1 < len(s) && s[i+1] == '1' {
				cur.WriteByte(c)
				continue
			}
			if parenDepth == 0 {
				return nil, false
			}
			cur.WriteByte(c)
		default:
			cur.WriteByte(c)
		}
	}
	if quote != 0 || escaped || parenDepth != 0 {
		return nil, false
	}
	flush()
	return parts, true
}

// splitShellPipes splits a single shell statement on unquoted `|` (rejecting `||`).
func splitShellPipes(s string) ([]string, bool) {
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
			if i+1 < len(s) && s[i+1] == '|' {
				return nil, false
			}
			t := strings.TrimSpace(cur.String())
			if t == "" {
				return nil, false
			}
			parts = append(parts, t)
			cur.Reset()
		default:
			cur.WriteByte(c)
		}
	}
	if quote != 0 || escaped {
		return nil, false
	}
	if t := strings.TrimSpace(cur.String()); t != "" {
		parts = append(parts, t)
	}
	return parts, true
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
	payloads := extractHerdrJSONPayloads(output, 0)
	for _, payload := range payloads {
		if !herdrResultTypeMatches(intent.action, payload.Type) {
			continue
		}
		if d, ok := e.discoveryFromPayload(intent.action, payload, evidenceKey); ok {
			return d, true
		}
	}
	return lineageDiscovery{}, false
}

// matchMultiResults pairs an ordered list of intents from a single code-mode
// script against extracted Herdr JSON results in order, consuming each result
// at most once so multi-spawn scripts never duplicate the first child or miss
// subsequent children.
func (e *lineageExtractor) matchMultiResults(intents []herdrCmdIntent, output, baseKey string) []lineageDiscovery {
	if len(intents) == 0 || strings.TrimSpace(output) == "" {
		return nil
	}
	payloads := extractHerdrJSONPayloads(output, 0)
	if len(payloads) == 0 {
		return e.matchFilteredCreationFallback(intents, output, baseKey)
	}
	used := make([]bool, len(payloads))
	var out []lineageDiscovery
	for i, intent := range intents {
		if !intent.valid {
			continue
		}
		key := baseKey
		if len(intents) > 1 {
			key += ":" + strconv.Itoa(i)
		}
		for pi, payload := range payloads {
			if used[pi] || !herdrResultTypeMatches(intent.action, payload.Type) {
				continue
			}
			if d, ok := e.discoveryFromPayload(intent.action, payload, key); ok {
				used[pi] = true
				out = append(out, d)
				break
			}
		}
	}
	return out
}

// matchFilteredCreationFallback handles real-world agent shell idioms where the
// parent ran a verified herdr creation command (workspace create / tab create /
// pane split) and filtered or truncated its JSON output (e.g. `jq -r .result.pane.pane_id`,
// `grep -oE '"pane_id":"[^"]+"'`, `echo "pane $pid"`, or `head -c 200`), so no
// complete JSON envelope remains in output. Safety guards:
//   - Only runs when extractHerdrJSONPayloads found zero complete Herdr JSON payloads.
//   - Rejects outputs containing Herdr error markers or multi-pane list dumps.
//   - Rejects scripts that also invoked `herdr ... list` or `herdr api snapshot`.
//   - Excludes every pane ID literally present in the command text (blocking
//     target-pane confusion like `pane split w33:p4` and `echo w9:p9` spoofing).
//   - Confirms only when the count of distinct new pane IDs does not exceed the
//     number of creation intents (never guesses from noisy multi-pane output).
func (e *lineageExtractor) matchFilteredCreationFallback(intents []herdrCmdIntent, output, baseKey string) []lineageDiscovery {
	type creationSlot struct {
		idx    int
		intent herdrCmdIntent
	}
	var creations []creationSlot
	for i, intent := range intents {
		if !intent.valid || intent.hasListing {
			continue
		}
		switch intent.action {
		case "workspace_created", "tab_created", "pane_split":
			creations = append(creations, creationSlot{idx: i, intent: intent})
		}
	}
	if len(creations) == 0 || outputHasHerdrErrorOrList(output) {
		return nil
	}
	text := unwrapCodexExecText(output)
	if text == "" || outputHasHerdrErrorOrList(text) {
		return nil
	}
	cmdPanes := creations[0].intent.cmdPanes
	var panes []string
	seen := map[string]bool{}
	for _, m := range herdrPaneTokenRe.FindAllStringSubmatch(text, -1) {
		p := m[1]
		if !herdrPaneID.MatchString(p) || cmdPanes[p] || seen[p] {
			continue
		}
		seen[p] = true
		panes = append(panes, p)
	}
	if len(panes) == 0 || len(panes) > len(creations) {
		return nil
	}
	// If truncated JSON still retained terminal_id for a single creation, capture it too.
	termID := ""
	if len(panes) == 1 {
		if tm := herdrTermTokenRe.FindStringSubmatch(text); len(tm) == 2 {
			termID = sanitizeTermID(tm[1])
		}
	}
	var out []lineageDiscovery
	for i, paneID := range panes {
		slot := creations[i]
		key := baseKey
		if len(intents) > 1 {
			key += ":" + strconv.Itoa(slot.idx)
		}
		d := lineageDiscovery{
			ChildPane:   paneID,
			ChildTermID: termID,
			EvidenceKey: key,
			Action:      slot.intent.action,
		}
		e.rememberCreated(d)
		out = append(out, d)
	}
	return out
}

func outputHasHerdrErrorOrList(s string) bool {
	if strings.Contains(s, `"error":`) && !strings.Contains(s, `"error":null`) && !strings.Contains(s, `"error": null`) {
		return true
	}
	if strings.Contains(s, `"ok":false`) || strings.Contains(s, `"ok": false`) {
		return true
	}
	if strings.Contains(s, `"agents":[`) || strings.Contains(s, `"panes":[`) || strings.Contains(s, `"workspaces":[`) {
		return true
	}
	return false
}

// unwrapCodexExecText extracts inner output strings from Codex code-mode
// {"exit_code":0,"output":"..."} wrappers when present, rejecting non-zero exits.
func unwrapCodexExecText(s string) string {
	if !strings.Contains(s, `"output"`) || !strings.Contains(s, `"exit_code"`) {
		return s
	}
	var b strings.Builder
	found := false
	raw := []byte(s)
	for off := 0; off < len(raw); {
		idx := bytes.IndexByte(raw[off:], '{')
		if idx < 0 {
			break
		}
		off += idx
		dec := json.NewDecoder(bytes.NewReader(raw[off:]))
		var obj struct {
			ExitCode *int    `json:"exit_code"`
			Output   *string `json:"output"`
		}
		if err := dec.Decode(&obj); err == nil && obj.Output != nil {
			found = true
			if obj.ExitCode == nil || *obj.ExitCode == 0 {
				if b.Len() > 0 {
					b.WriteByte('\n')
				}
				b.WriteString(*obj.Output)
			}
			off += int(dec.InputOffset())
		} else {
			off++
		}
	}
	if found {
		return b.String()
	}
	return s
}

func (e *lineageExtractor) discoveryFromPayload(action string, payload herdrResultPayload, evidenceKey string) (lineageDiscovery, bool) {
	switch action {
	case "workspace_created", "tab_created":
		if payload.RootPane == nil || !herdrPaneID.MatchString(payload.RootPane.PaneID) {
			return lineageDiscovery{}, false
		}
		termID := sanitizeTermID(payload.RootPane.TerminalID)
		d := lineageDiscovery{
			ChildPane:   payload.RootPane.PaneID,
			ChildTermID: termID,
			EvidenceKey: evidenceKey,
			Action:      action,
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
			Action:      action,
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

// extractHerdrJSONPayloads scans output in order for all successful Herdr JSON
// result objects. It also unwraps Codex code-mode exec wrapper objects of the
// form {"exit_code":0,"output":"..."} (skipping any wrapper with non-zero
// exit_code).
func extractHerdrJSONPayloads(output string, depth int) []herdrResultPayload {
	if depth > 2 || strings.TrimSpace(output) == "" {
		return nil
	}
	var out []herdrResultPayload
	decodeObj := func(raw []byte) {
		raw = bytes.TrimSpace(raw)
		if len(raw) < 2 || raw[0] != '{' {
			return
		}
		var env struct {
			OK       *bool           `json:"ok"`
			Error    json.RawMessage `json:"error"`
			Result   json.RawMessage `json:"result"`
			Type     string          `json:"type"`
			ExitCode *int            `json:"exit_code"`
			Output   *string         `json:"output"`
		}
		if json.Unmarshal(raw, &env) != nil {
			return
		}
		// Unwrap Codex code-mode `tools.exec_command` result wrapper:
		// {"chunk_id":"...","exit_code":0,"output":"{\"result\":...}"}
		if env.Output != nil && env.Result == nil && env.Type == "" {
			if env.ExitCode != nil && *env.ExitCode != 0 {
				return
			}
			out = append(out, extractHerdrJSONPayloads(*env.Output, depth+1)...)
			return
		}
		if env.OK != nil && !*env.OK {
			return
		}
		if len(env.Error) > 0 && string(bytes.TrimSpace(env.Error)) != "null" {
			return
		}
		target := raw
		if len(env.Result) > 0 && bytes.TrimSpace(env.Result)[0] == '{' {
			target = env.Result
		}
		var p herdrResultPayload
		if json.Unmarshal(target, &p) != nil || p.Type == "" {
			return
		}
		out = append(out, p)
	}

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
			decodeObj(raw)
			off += int(dec.InputOffset())
		} else {
			off++
		}
	}
	return out
}
