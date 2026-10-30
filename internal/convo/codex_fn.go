package convo

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Codex's function calls (plans/5a-codex-parse.md): the tools of Codex's
// own (exec_command, write_stdin, view_image, update_plan…), MCP servers'
// and its subagents', each made the tool the view knows that does the
// same (Bash, Read, Edit, WebSearch, Agent…), so that it shows as one does.
// Not its code mode's script (custom_tool_call exec: see codexTools).
//
// The things Codex tells of when they are done (item_completed) carry the
// call's id in this mode: the line of the call is the one they make up.

// codexFn: a function call's tools. tasks: an update_plan's list (nil for
// any other call; empty, a list cleared).
func codexFn(ns, name, args string) (tools []*Tool, tasks []Task) {
	var a map[string]json.RawMessage
	if json.Unmarshal([]byte(args), &a) != nil {
		a = nil
	}
	str := func(k string) string {
		var s string
		if json.Unmarshal(a[k], &s) == nil {
			return s
		}
		return ""
	}
	switch {
	case strings.HasPrefix(ns, "mcp__codex_apps__search_service") && strings.HasSuffix(name, "web_run"):
		return webRun(a), nil
	case strings.HasPrefix(ns, "mcp__"):
		full := ns
		if !strings.HasSuffix(full, "__") {
			full += "__"
		}
		return []*Tool{{Name: clip(full+name, 128), Detail: clip(compactJSON(args), maxDetail)}}, nil
	case ns == "collaboration" || ns == "multi_agent_v1" || strings.HasSuffix(name, "_agent") || name == "list_agents" || name == "send_message":
		return []*Tool{agentFn(name, str)}, nil
	}
	switch name {
	case "exec_command", "shell_command", "shell", "local_shell", "container.exec":
		cmd := str("cmd")
		if cmd == "" {
			cmd = str("command")
		}
		if cmd == "" {
			var argv []string
			if json.Unmarshal(a["command"], &argv) == nil {
				cmd = shellLine(argv)
			}
		}
		// (A patch applied by a command: its files, as the code mode's are.)
		if p := heredocPatch(cmd); p != "" {
			if ts := patchTools(p); len(ts) > 0 {
				return ts, nil
			}
		}
		return []*Tool{{Name: "Bash", Title: clip(strings.TrimSpace(str("justification")), maxTitle), Detail: clip(clean(cmd), maxDetail)}}, nil
	case "write_stdin":
		return []*Tool{stdinTool(a["session_id"], str("chars"))}, nil
	case "wait":
		return []*Tool{{Name: "BashOutput", Detail: clip("cell "+rawWord(a["cell_id"]), maxDetail), fromCall: true}}, nil
	case "view_image":
		return []*Tool{{Name: "Read", Detail: clip(strings.TrimPrefix(str("path"), "file://"), maxDetail)}}, nil
	case "update_plan":
		var in struct {
			Plan []struct {
				Step   string `json:"step"`
				Status string `json:"status"`
			} `json:"plan"`
		}
		_ = json.Unmarshal([]byte(args), &in)
		tasks = []Task{}
		for _, s := range in.Plan {
			if len(tasks) >= maxTasks {
				break
			}
			tasks = append(tasks, Task{Subject: clip(clean(s.Step), maxTitle), Status: taskState(s.Status)})
		}
		return []*Tool{{Name: "TodoWrite"}}, tasks
	case "request_user_input", "request_user_input_async":
		t := &Tool{Name: "request_user_input", Asks: codexAsks(a["questions"])}
		if len(t.Asks) > 0 {
			t.Title = clip(firstLineOf(t.Asks[0].Question), maxTitle)
		}
		return []*Tool{t}, nil
	case "get_goal", "update_goal", "create_goal":
		return []*Tool{{Name: name, Detail: clip(firstNonEmpty(str("objective"), str("status")), maxDetail)}}, nil
	}
	return []*Tool{{Name: clip(name, 128), Detail: clip(compactJSON(args), maxDetail)}}, nil
}

// heredocPatch: the patch a command applies with apply_patch (Codex's
// "apply_patch <<'PATCH' … PATCH"), else "".
func heredocPatch(cmd string) string {
	s := strings.TrimSpace(cmd)
	if !strings.HasPrefix(s, "apply_patch") {
		return ""
	}
	i := strings.Index(s, "*** Begin Patch")
	j := strings.LastIndex(s, "*** End Patch")
	if i < 0 || j < i {
		return ""
	}
	return s[i : j+len("*** End Patch")]
}

// shellLine: a command given as its words; of a shell's "-c" one, what it
// runs.
func shellLine(argv []string) string {
	if n := len(argv); n >= 3 && (argv[n-2] == "-c" || argv[n-2] == "-lc") {
		return argv[n-1]
	}
	return strings.Join(argv, " ")
}

// webRun: Codex's own web tool (search_service.web_run): a search, or a
// page opened or looked through.
func webRun(a map[string]json.RawMessage) []*Tool {
	var qs []struct {
		Q string `json:"q"`
	}
	if json.Unmarshal(a["system2_search_query"], &qs) == nil && len(qs) > 0 {
		var words []string
		for _, q := range qs {
			if s := strings.TrimSpace(q.Q); s != "" {
				words = append(words, s)
			}
		}
		if len(words) > 0 {
			return []*Tool{{Name: "WebSearch", Detail: clip(clean(strings.Join(words, "\n")), maxDetail)}}
		}
	}
	var refs []struct {
		RefID string `json:"ref_id"`
		URL   string `json:"url"`
	}
	for _, k := range []string{"open", "click", "find"} {
		if json.Unmarshal(a[k], &refs) == nil && len(refs) > 0 {
			return []*Tool{{Name: "WebFetch", Detail: clip(clean(firstNonEmpty(refs[0].URL, refs[0].RefID)), maxDetail)}}
		}
	}
	return []*Tool{{Name: "WebSearch"}}
}

// agentFn: a subagent's tool (spawn, wait, close, message, list).
func agentFn(name string, str func(string) string) *Tool {
	switch name {
	case "spawn_agent":
		return &Tool{Name: "Agent", Title: clip(clean(str("task_name")), maxTitle), Detail: clip(clean(firstNonEmpty(str("message"), str("prompt"))), maxDetail)}
	case "send_message":
		return &Tool{Name: "SendMessage", Detail: clip(clean(str("message")), maxDetail)}
	case "list_agents":
		return &Tool{Name: "ListAgents"}
	}
	return &Tool{Name: clip(name, 128), Detail: clip(shortID(str("target")), maxDetail)}
}

func shortID(s string) string {
	if len(s) > 13 {
		return s[:8] + "…"
	}
	return s
}

// stdinTool: what was put to a command still running (write_stdin): its
// session, and of what was typed only keys of a few known kinds by name —
// else how many characters, never the characters (a password, a key).
func stdinTool(session json.RawMessage, chars string) *Tool {
	in := &Stdin{Session: clip(rawWord(session), 32)}
	if keys, ok := knownKeys(chars); ok {
		in.Keys = keys
	} else {
		in.Chars = len([]rune(chars))
	}
	return &Tool{Name: "BashOutput", Detail: "#" + in.Session, Stdin: in}
}

// knownKeys: chars as the names of the keys it is, when all of it is such
// keys (none is no keys: a look at its output); else false.
func knownKeys(chars string) ([]string, bool) {
	var out []string
	for i := 0; i < len(chars); {
		matched := false
		for _, k := range stdinKeys {
			if strings.HasPrefix(chars[i:], k.seq) {
				if len(out) < 8 {
					out = append(out, k.name)
				}
				i += len(k.seq)
				matched = true
				break
			}
		}
		if !matched {
			return nil, false
		}
	}
	return out, true
}

var stdinKeys = []struct{ seq, name string }{
	{"\x1b[A", "Up"}, {"\x1b[B", "Down"}, {"\x1b[C", "Right"}, {"\x1b[D", "Left"},
	{"\x03", "Ctrl+C"}, {"\x04", "Ctrl+D"}, {"\x1a", "Ctrl+Z"}, {"\x1c", "Ctrl+\\"},
	{"\r\n", "Enter"}, {"\r", "Enter"}, {"\n", "Enter"}, {"\t", "Tab"}, {"\x1b", "Esc"},
}

// rawWord: a number or a string, as a word.
func rawWord(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var n json.Number
	if json.Unmarshal(raw, &n) == nil {
		return n.String()
	}
	return ""
}

// compactJSON: arguments on one line, as written (not JSON: as they are).
func compactJSON(s string) string {
	var v any
	if json.Unmarshal([]byte(s), &v) != nil {
		return strings.TrimSpace(s)
	}
	b, err := json.Marshal(v)
	if err != nil {
		return strings.TrimSpace(s)
	}
	return string(b)
}

func firstLineOf(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// codexAsks: Codex's questions to the person (request_user_input): each
// one's words and its choices.
func codexAsks(raw json.RawMessage) []Ask {
	var qs []struct {
		Header   string            `json:"header"`
		Title    string            `json:"title"`
		Question string            `json:"question"`
		Options  []json.RawMessage `json:"options"`
	}
	if json.Unmarshal(raw, &qs) != nil {
		return nil
	}
	var out []Ask
	for _, q := range qs {
		if len(out) >= maxAsks {
			break
		}
		a := Ask{Header: clip(clean(q.Header), maxTitle), Question: clip(clean(firstNonEmpty(q.Question, q.Title)), maxDetail)}
		for _, o := range q.Options {
			if len(a.Options) >= maxOptions {
				a.Cut = true
				break
			}
			var label string
			var full struct {
				Label       string `json:"label"`
				Description string `json:"description"`
			}
			switch {
			case json.Unmarshal(o, &label) == nil:
				a.Options = append(a.Options, AskOption{Label: clip(clean(label), maxTitle)})
			case json.Unmarshal(o, &full) == nil && full.Label != "":
				a.Options = append(a.Options, AskOption{Label: clip(clean(full.Label), maxTitle), Description: clip(clean(full.Description), maxTitle)})
			}
		}
		if a.Question != "" {
			out = append(out, a)
		}
	}
	return out
}

// codexResult: what came back of a call, as text, whether it failed and
// the pictures in it. Only Codex's own wrappings are taken off: a
// command's head (its chunk, the time it took, its exit code), a script's,
// {"output", "metadata": {"exit_code"}}; content blocks are read for their
// words. Anything else is as it came.
func codexResult(raw json.RawMessage) (text string, failed bool, images int) {
	var s string
	if t := strings.TrimSpace(string(raw)); strings.HasPrefix(t, "{") {
		s = t // (an object as it is: its wrapping, if it is Codex's, comes off below)
	} else if json.Unmarshal(raw, &s) != nil {
		var parts []string
		var blocks []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if json.Unmarshal(raw, &blocks) != nil {
			return "", false, 0
		}
		for _, b := range blocks {
			if strings.Contains(b.Type, "image") {
				images++
				continue
			}
			parts = append(parts, b.Text)
		}
		s = strings.Join(parts, "")
	}
	if t := strings.TrimSpace(s); strings.HasPrefix(t, "{") && strings.Contains(t, `"content"`) {
		var w map[string]json.RawMessage
		if json.Unmarshal([]byte(t), &w) == nil && w["content"] != nil && len(w) <= 3 && (len(w) == 1 || w["isError"] != nil || w["structuredContent"] != nil) {
			var blocks []json.RawMessage
			if json.Unmarshal(w["content"], &blocks) == nil {
				text, failed, pics := mcpResult(json.RawMessage(t))
				return text, failed, images + pics
			}
		}
	}
	if t := strings.TrimSpace(s); strings.HasPrefix(t, "{") && strings.Contains(t, `"output"`) {
		var w map[string]json.RawMessage
		if json.Unmarshal([]byte(t), &w) == nil && len(w) <= 2 && w["output"] != nil && (len(w) == 1 || w["metadata"] != nil) {
			var out string
			var meta struct {
				ExitCode *int `json:"exit_code"`
			}
			if json.Unmarshal(w["output"], &out) == nil {
				_ = json.Unmarshal(w["metadata"], &meta)
				if meta.ExitCode != nil && *meta.ExitCode != 0 {
					return fmt.Sprintf("Exit code %d\n%s", *meta.ExitCode, out), true, images
				}
				return out, false, images
			}
		}
	}
	if m := cmdHead.FindStringSubmatch(s); m != nil {
		s = s[len(m[0]):]
		if code, _ := strconv.Atoi(m[1]); code != 0 {
			return fmt.Sprintf("Exit code %d\n%s", code, s), true, images
		}
		return s, false, images
	}
	return codexHead.ReplaceAllString(s, ""), codexFailed.MatchString(s), images
}

// cmdHead: the head Codex puts on what a command printed.
var cmdHead = regexp.MustCompile(`^(?:Chunk ID: \S+\n)?Wall time: [^\n]*\n(?:Process exited with code (\d+)\n)?(?:Process running with session ID \S+\n)?(?:Original token count: \d+\n)?Output:\n`)

// mcpResult: what an MCP server's tool gave back (its content's words, its
// pictures counted), as Codex keeps it: {content, isError}, or (older)
// {Ok: …} / {Err: …}.
func mcpResult(raw json.RawMessage) (string, bool, int) {
	var r struct {
		Ok      json.RawMessage `json:"Ok"`
		Err     json.RawMessage `json:"Err"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	if json.Unmarshal(raw, &r) != nil {
		return "", false, 0
	}
	if r.Err != nil {
		var s string
		if json.Unmarshal(r.Err, &s) == nil {
			return s, true, 0
		}
		return string(r.Err), true, 0
	}
	if r.Ok != nil {
		return mcpResult(r.Ok)
	}
	var parts []string
	images := 0
	for _, c := range r.Content {
		if strings.Contains(c.Type, "image") {
			images++
		} else if c.Text != "" {
			parts = append(parts, c.Text)
		}
	}
	return strings.Join(parts, "\n"), r.IsError, images
}

// codexChange is a file a change made, as Codex tells of it (FileChange,
// patch_apply_end).
type codexChange struct {
	Type    string `json:"type"` // add, update, delete
	Diff    string `json:"unified_diff"`
	Content string `json:"content"`
	Move    string `json:"move_path"`
}

// changeTools: a change's files, each an edit with its hunks; at most
// maxFiles of them (sorted, so that one told twice is told alike).
func changeTools(changes map[string]codexChange) (out []*Tool, more int) {
	paths := make([]string, 0, len(changes))
	for p := range changes {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		if len(out) >= maxFiles {
			more++
			continue
		}
		out = append(out, changeTool(p, changes[p]))
	}
	return out, more
}

func changeTool(path string, ch codexChange) *Tool {
	t := &Tool{Name: "Edit", Detail: clip(path, maxDetail)}
	diff := ch.Diff
	switch ch.Type {
	case "add":
		t.Name = "Write"
		lines := strings.Split(strings.TrimSuffix(ch.Content, "\n"), "\n")
		diff = fmt.Sprintf("@@ -0,0 +1,%d @@\n+%s", len(lines), strings.Join(lines, "\n+"))
	case "delete":
		t.Title, diff = "delete", ""
	}
	t.Patch, t.Add, t.Del, t.PatchCut = unifiedHunks(diff)
	if ch.Move != "" {
		t.Title = clip(path+" → "+ch.Move, maxTitle)
	}
	return t
}
