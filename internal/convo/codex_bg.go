package convo

import (
	"bytes"
	"encoding/json"
	"strconv"
	"strings"
)

// A command a code-mode script set going and left running: exec_command
// yielded ({"session_id": N, …} with no exit code in what the script gave
// back), the process going on — past the turn, at times ("1 background
// terminal running" in Codex's own view). Codex tells of it once more only
// when it ends: a CommandExecution of that process (process_id N), in
// whatever turn is going on then. So it is told of here as it starts — a
// step of its turn, running — and that end goes to that step, not to a
// turn it was no part of (11-15, the parity audit).

// maxScriptCalls: code-mode calls whose commands are kept, for a process
// one of them leaves running to be named by its command.
const maxScriptCalls = 64

type codexProc struct {
	id string // its step's (and running's) id
	t  *Tool
}

// scriptRan notes the commands a code-mode call's script runs (as read out
// of it), in order.
func (c *Codex) scriptRan(id string, ts []*Tool) {
	var cmds []string
	for _, t := range ts {
		if t.Name == "Bash" {
			cmds = append(cmds, t.Detail)
		}
	}
	if id == "" || len(cmds) == 0 {
		return
	}
	if c.scriptCmds == nil || len(c.scriptCmds) >= maxScriptCalls {
		c.scriptCmds = map[string][]string{}
	}
	c.scriptCmds[id] = cmds
}

// codeModeGoing: the processes a code-mode script's output says are still
// running — exec_command's results ({"chunk_id", …, "session_id"}) with no
// exit code — by their session ids, in order.
func codeModeGoing(raw json.RawMessage) []string {
	text, _, _ := codexResult(raw)
	var out []string
	for i := 0; i < len(text) && len(out) < maxRunning; {
		j := strings.Index(text[i:], `{"chunk_id"`)
		if j < 0 {
			break
		}
		i += j
		var r struct {
			Session  json.RawMessage `json:"session_id"`
			ExitCode json.RawMessage `json:"exit_code"`
		}
		d := json.NewDecoder(strings.NewReader(text[i:]))
		if d.Decode(&r) != nil {
			i++
			continue
		}
		i += int(d.InputOffset())
		if sid := procID(r.Session); sid != "" && (len(r.ExitCode) == 0 || bytes.Equal(r.ExitCode, []byte("null"))) {
			out = append(out, sid)
		}
	}
	return out
}

// procID: a process id as Codex writes it, a number or a string of one.
func procID(raw json.RawMessage) string {
	var n json.Number
	if json.Unmarshal(raw, &n) == nil {
		if _, err := strconv.ParseInt(string(n), 10, 64); err == nil {
			return string(n)
		}
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		if _, err := strconv.ParseInt(s, 10, 64); err == nil {
			return s
		}
	}
	return ""
}

// procsGoing: what a code-mode call (id) gave back says it left processes
// running: each a step of this turn, running, till its end is told.
func (c *Codex) procsGoing(r *codexRow, id string, raw json.RawMessage) {
	sids := codeModeGoing(raw)
	if len(sids) == 0 {
		return
	}
	// (Its commands, one each, where the script's lines tell them so: a
	// line of several is no word on which of them a process is — its end
	// tells it.)
	var cmds []string
	for _, cm := range c.scriptCmds[id] {
		cmds = append(cmds, strings.Split(cm, "\n")...)
	}
	// (Its script's own lines, where they are shown — no command told of on
	// its own in this conversation yet: they go, its processes' steps in
	// their place, not shown twice; nor would they stay, once commands are
	// told of on their own.)
	if len(c.tools[id]) > 0 {
		c.dropScriptLines(id)
	}
	for i, sid := range sids {
		if c.procs[sid] != nil || c.procsOver[sid] != nil {
			continue // (going already; or its end told already, its step that one)
		}
		cmd := ""
		if len(cmds) == len(sids) {
			cmd = cmds[i]
		}
		step := clip("proc:"+sid, maxID)
		if c.procs == nil || len(c.procs) >= maxRunning {
			c.procs = map[string]*codexProc{}
		}
		t := &Tool{ID: step, Name: "Bash", Detail: clip(clean(cmd), maxDetail)}
		c.add(r, step, RoleAssistant, []Part{{Kind: KindTool, Tool: t}})
		c.mark(step, t)
		c.procs[sid] = &codexProc{id: step, t: t}
		what := cmd
		if what == "" {
			what = "process " + sid
		}
		c.setGoing(step, "cmd", what, r.Timestamp)
	}
}

// procEnded: a CommandExecution of a process a script left running: its
// end is that step's (true), not a new step of the turn going on now.
func (c *Codex) procEnded(it *codexItem, out string, failed bool) bool {
	sid := procID(it.ProcessID)
	p := c.procs[sid]
	if sid == "" || p == nil || it.Status == "in_progress" {
		return false
	}
	p.t.Done, p.t.Error = true, failed
	if cmd := strings.TrimSpace(p.t.Detail); cmd == "" {
		run := strings.Join(it.Command, " ")
		if len(it.Parsed) > 0 && it.Parsed[0].Cmd != "" {
			run = it.Parsed[0].Cmd
		}
		p.t.Detail = clip(clean(run), maxDetail)
	}
	p.t.setResult(clean(out))
	p.t.ResultLines = lineCount(out)
	c.touch([]*Tool{p.t})
	c.endRun(p.id)
	delete(c.procs, sid)
	if id := clip(it.ID, maxID); id != "" {
		c.mark(id, p.t) // (told of again by its own id: the same step)
	}
	return true
}

// provOut: the call id's lines just added (the last entry) are shown only
// while it is out.
func (c *Codex) provOut(id string) {
	if id == "" || len(c.entries) == 0 {
		return
	}
	if c.provs == nil || len(c.provs) >= maxRunning {
		c.provAllDone()
		c.provs = map[string]*Entry{}
	}
	c.provs[id] = c.entries[len(c.entries)-1]
}

// provDone: what call id gave back came: its lines shown meanwhile go
// (true: they were; nothing else of it is to be read).
func (c *Codex) provDone(id string) bool {
	e, ok := c.provs[id]
	if !ok {
		return false
	}
	delete(c.provs, id)
	c.dropProv(e, id)
	c.hide(id)
	delete(c.tools, id)
	return true
}

// provAllDone: the turn is over: lines of calls never answered go too.
func (c *Codex) provAllDone() {
	for id, e := range c.provs {
		c.dropProv(e, id)
		c.hide(id)
		delete(c.tools, id)
	}
	c.provs = nil
}

func (c *Codex) dropProv(e *Entry, id string) {
	kept, dropped := e.Parts[:0], false
	for _, p := range e.Parts {
		if p.Kind == KindTool && p.Tool != nil && p.Tool.prov && p.Tool.ID == id {
			dropped = true
			continue
		}
		kept = append(kept, p)
	}
	if dropped {
		e.Parts, e.Rev = kept, c.rev
	}
}

// provTold: a command told of on its own (done): a line shown for it while
// its call was out, of the same command, goes — the first such.
func (c *Codex) provTold(cmd string) {
	cmd = strings.TrimSpace(cmd)
	if cmd == "" {
		return
	}
	for id, e := range c.provs {
		for i, p := range e.Parts {
			if p.Kind == KindTool && p.Tool != nil && p.Tool.prov && p.Tool.ID == id && strings.TrimSpace(p.Tool.Detail) == clip(clean(cmd), maxDetail) {
				e.Parts = append(e.Parts[:i:i], e.Parts[i+1:]...)
				e.Rev = c.rev
				return
			}
		}
	}
}

// dropScriptLines: the lines of call id's script, shown as it told them,
// go (its processes' steps in their place).
func (c *Codex) dropScriptLines(id string) {
	for _, t := range c.tools[id] {
		if t.fromCall && t.at != nil {
			c.dropPart(t.at, t)
		}
	}
	c.hide(id)
	delete(c.tools, id)
}

func (c *Codex) dropPart(e *Entry, t *Tool) {
	for i, p := range e.Parts {
		if p.Tool == t {
			e.Parts = append(e.Parts[:i:i], e.Parts[i+1:]...)
			e.Rev = c.rev
			return
		}
	}
}

// procOver: process sid told of as ended (by step t) with no start of it
// read: kept, so that its start, read after (the part before, or later in
// what the script gives back), is not set going.
func (c *Codex) procOver(sid string, t *Tool) {
	if sid == "" || c.procs[sid] != nil {
		return
	}
	if c.procsOver == nil || len(c.procsOver) >= maxRunning {
		c.procsOver = map[string]*Tool{}
	}
	c.procsOver[sid] = t
	c.endRun("proc:" + sid)
}

// prependProcs: the part before's processes left running: still going,
// unless this part told of their end — then that end is their step's, in
// its turn, and the step this part made of it goes.
func (c *Codex) prependProcs(o *Codex) {
	for sid, p := range o.procs {
		if end := c.procsOver[sid]; end != nil {
			p.t.Done, p.t.Error = true, end.Error
			if strings.TrimSpace(p.t.Detail) == "" {
				p.t.Detail = end.Detail
			}
			p.t.setResult(end.Result)
			p.t.ResultLines = end.ResultLines
			c.touch([]*Tool{p.t})
			if end.at != nil {
				c.dropPart(end.at, end)
			}
			delete(c.procsOver, sid)
			continue
		}
		if c.procs == nil {
			c.procs = map[string]*codexProc{}
		}
		if _, there := c.procs[sid]; !there && len(c.procs) < maxRunning {
			c.procs[sid] = p
		}
	}
	for sid, t := range o.procsOver {
		if c.procsOver == nil {
			c.procsOver = map[string]*Tool{}
		}
		if _, there := c.procsOver[sid]; !there && len(c.procsOver) < maxRunning {
			c.procsOver[sid] = t
		}
	}
}
