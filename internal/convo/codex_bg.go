package convo

import (
	"bytes"
	"encoding/json"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
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
	id   string // its step's (and running's) id
	t    *Tool  // (nil: set going before commands were told of on their own — the running list only)
	call string // the call whose script set it going
	cmd  string // its command, where told ("": several of the script's)
	n    int    // (its place among those the call set going)
	over bool   // told ended by the host before it had a step (EndedOnHost): its step, when made, is done
}

type codexScriptCmd struct {
	cmd string
	cut bool
}

// scriptRan notes the commands a code-mode call's script runs (as read out
// of it), in order.
func (c *Codex) scriptRan(id string, ts []*Tool) {
	var cmds []codexScriptCmd
	for _, t := range ts {
		if t.Name == "Bash" {
			cmds = append(cmds, codexScriptCmd{cmd: t.Detail, cut: t.detailCut})
		}
	}
	if id == "" || len(cmds) == 0 {
		return
	}
	if c.scriptCmds == nil || len(c.scriptCmds) >= maxScriptCalls {
		c.scriptCmds = map[string][]codexScriptCmd{}
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
// running (sids, codeModeGoing's): each in the running list till its end
// is told; and, where commands are told of on their own in this
// conversation (the script's lines gone), a step of this turn, running.
// (Before that, the script's line stands for it — its end, told on its
// own, comes as any command's.)
func (c *Codex) procsGoing(r *codexRow, id string, sids []string) {
	if len(sids) == 0 {
		return
	}
	// (Named by its command only where the script ran one, and left one
	// process: of several, the order they came back in is no word on which
	// is which — their ends tell it.)
	var cmds []codexScriptCmd
	for _, cm := range c.scriptCmds[id] {
		parts := strings.Split(cm.cmd, "\n")
		for _, part := range parts {
			cmds = append(cmds, codexScriptCmd{cmd: part, cut: cm.cut || len(parts) > 1})
		}
	}
	for _, sid := range sids {
		if c.procs[sid] != nil || c.endedJustNow(sid, r.Timestamp) {
			continue // (going already; or its end told already, just now — a session id
			// comes again later for another process: that one is set going)
		}
		cmd, cmdCut := "", true
		if len(sids) == 1 && len(cmds) == 1 {
			cmd, cmdCut = cmds[0].cmd, cmds[0].cut
		}
		step := clip("proc:"+sid+"@"+r.Timestamp, maxID) // (its id again later is another process's)
		if c.procs == nil {
			c.procs = map[string]*codexProc{}
		}
		for k := range c.procs { // (full: one makes way, not all — else those kept are set going anew)
			if len(c.procs) < maxRunning {
				break
			}
			delete(c.procs, k)
		}
		p := &codexProc{id: step, call: id, cmd: cmd, n: len(c.procs)}
		if c.itemCmds {
			p.t = p.step()
			c.add(r, step, RoleAssistant, []Part{{Kind: KindTool, Tool: p.t}})
			c.mark(step, p.t)
		}
		c.procs[sid] = p
		c.setGoingCmd(step, "cmd", cmd, cmd, r.Timestamp, cmdCut) // ("": the page says "a command in the background")
	}
}

// step: the step of a process a script left running, running.
func (p *codexProc) step() *Tool {
	detail, dcut := cut(clean(p.cmd), maxDetail)
	return &Tool{ID: p.id, Name: "Bash", Detail: detail, detailCut: dcut, Done: p.over}
}

// stepsInPlace: commands are told of on their own in this conversation now
// (itemsTell), the script lines of its calls gone: the processes a call
// left running before that was known (no step of their own: the call's
// line stood for them) get theirs where that line was, in its turn — as
// had it been known from the first (a conversation whose first command is
// one left running; the part of a log read first, whose rows that tell
// commands on their own all came before it). parts: an entry's, call the
// call whose line goes from it; what goes in its place.
func (c *Codex) stepsInPlace(call string) []Part {
	var ps []*codexProc
	for _, p := range c.procs {
		if p.t == nil && p.call == call {
			ps = append(ps, p)
		}
	}
	slices.SortFunc(ps, func(a, b *codexProc) int { return a.n - b.n })
	var parts []Part
	for _, p := range ps {
		p.t = p.step()
		c.mark(p.id, p.t)
		parts = append(parts, Part{Kind: KindTool, Tool: p.t})
	}
	return parts
}

// procEnded: a CommandExecution of a process a script left running: its
// end is that step's (true), not a new step of the turn going on now.
// (One with no step of its own — set going before commands were told of on
// their own: false, its end told as any command's — ended all the same.)
func (c *Codex) procEnded(it *codexItem, out string, failed bool) bool {
	sid := procID(it.ProcessID)
	p := c.procs[sid]
	if sid == "" || p == nil || it.Status == "in_progress" {
		return false
	}
	if p.t == nil && !c.itemCmds {
		c.itemsTell() // (told of on their own after all — this is one: its step, in its turn, first)
	}
	c.endRun(p.id)
	delete(c.procs, sid)
	if p.t == nil {
		return false
	}
	p.t.Done, p.t.Error = true, failed
	if strings.TrimSpace(p.t.Detail) == "" {
		run := strings.Join(it.Command, " ")
		if len(it.Parsed) > 0 && it.Parsed[0].Cmd != "" {
			run = it.Parsed[0].Cmd
		}
		p.t.Detail = clip(clean(run), maxDetail)
	}
	p.t.setResult(clean(out))
	p.t.ResultLines = lineCount(out)
	c.touch([]*Tool{p.t})
	c.overAt(sid, p.t)
	if id := clip(it.ID, maxID); id != "" {
		c.mark(id, p.t) // (told of again by its own id: the same step)
	}
	return true
}

// overAt keeps that process sid has ended, told by step t (its own, or one
// of its end's: see procOver).
func (c *Codex) overAt(sid string, t *Tool) {
	if c.procsOver == nil {
		c.procsOver = map[string]*Tool{}
	}
	for k := range c.procsOver {
		if len(c.procsOver) < maxRunning {
			break
		}
		delete(c.procsOver, k)
	}
	c.procsOver[sid] = t
	if c.procsOverAt == nil || len(c.procsOverAt) > 2*maxRunning {
		c.procsOverAt = map[string]string{}
	}
	c.procsOverAt[sid] = c.rowAt
}

// endedJustNow: process sid was told of as ended within a little while of
// at (its end told before what the script gave back, in the same moment).
func (c *Codex) endedJustNow(sid, at string) bool {
	if c.procsOver[sid] == nil && !hasKey(c.procsOver, sid) {
		return false
	}
	end, err1 := time.Parse(time.RFC3339Nano, c.procsOverAt[sid])
	now, err2 := time.Parse(time.RFC3339Nano, at)
	if err1 != nil || err2 != nil {
		return true // (no time to tell by: as ended)
	}
	return now.Sub(end) < 2*time.Minute
}

func hasKey(m map[string]*Tool, k string) bool {
	_, ok := m[k]
	return ok
}

// procOver: process sid told of as ended (by step t, made of that end)
// with no start of it read: kept, so that its start, read after (the part
// before, or later in what the script gives back), is not set going.
func (c *Codex) procOver(sid string, t *Tool) {
	if sid == "" || c.procs[sid] != nil {
		return
	}
	c.overAt(sid, t)
	c.endRun("proc:" + sid)
}

func (c *Codex) dropPart(e *Entry, t *Tool) bool {
	for i, p := range e.Parts {
		if p.Tool == t {
			e.Parts = append(e.Parts[:i:i], e.Parts[i+1:]...)
			return true
		}
	}
	return false
}

// endInto: what step end told of a process's end, put on its start's
// step t.
func endInto(t, end *Tool) {
	t.Done, t.Error = true, end.Error
	if strings.TrimSpace(t.Detail) == "" {
		t.Detail = end.Detail
	}
	t.setResult(end.Result)
	t.ResultLines = end.ResultLines
}

// prependProcs: the part before's processes, and this part's, put
// together: a start in one and its end in the other is one step, the
// start's, in its turn (the end's own step, if one was made, goes).
func (c *Codex) prependProcs(o *Codex) {
	var changed []*Entry
	var touched []*Tool
	settle := func(start *codexProc, end *Tool) {
		// (A step of the same id in both parts — this part told it running
		// too — is one line already, this part's: Prepend's merge.)
		if start.t != nil && end != nil && end != start.t && end.ID != start.t.ID {
			endInto(start.t, end)
			touched = append(touched, start.t)
			if end.at != nil && c.dropPart(end.at, end) {
				changed = append(changed, end.at)
			}
		}
	}
	// (Started before; ended in this part.)
	for sid, p := range o.procs {
		if end, over := c.procsOver[sid]; over {
			settle(p, end)
			if _, there := c.running[p.id]; !there {
				c.endRun(p.id) // (kept as ended: the part before's running goes)
			}
			continue
		}
		if c.procs == nil {
			c.procs = map[string]*codexProc{}
		}
		if _, there := c.procs[sid]; !there && len(c.procs) < maxRunning {
			c.procs[sid] = p
		}
	}
	// (Ended before; told of as running in this part, after.)
	for sid, end := range o.procsOver {
		if p := c.procs[sid]; p != nil && o.procs[sid] == nil {
			settle(p, end)
			c.endRun(p.id)
			delete(c.procs, sid)
		}
		if _, there := c.procsOver[sid]; !there {
			c.overAt(sid, end)
		}
	}
	if len(changed) > 0 || len(touched) > 0 {
		c.rev++
		for _, e := range changed {
			e.Rev = c.rev
		}
		c.touch(touched)
	}
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

// What a script polls, and what comes back of it: a process left running
// is looked at again with write_stdin (its session); its script yields
// ("Script running with cell ID …") and what it gave back comes with a
// wait on that cell, later. A process that has ended comes back with its
// exit code and no session: no word of it ending is told otherwise, at
// times (11-15, the person's own logs: no CommandExecution of it).

var (
	stdinOf  = regexp.MustCompile(`write_stdin\(\s*\{\s*session_id\s*:\s*"?(\d{1,10})`)
	execOf   = regexp.MustCompile(`exec_command\s*\(`)
	cellIDOf = regexp.MustCompile(`Script running with cell ID ([\w-]{1,64})`)
)

// scriptPolls notes the sessions call id's script looks at again.
func (c *Codex) scriptPolls(id, script string) {
	var sids []string
	for _, m := range stdinOf.FindAllStringSubmatch(script, maxRunning) {
		sids = append(sids, m[1])
	}
	if id == "" || len(sids) == 0 {
		return
	}
	if c.polls == nil || len(c.polls) >= maxScriptCalls {
		c.polls = map[string][]string{}
	}
	if execOf.MatchString(script) {
		sids = append(sids, "") // (a command of its own too: whose result is whose is not told — see procsPolled)
	}
	c.polls[id] = sids
}

// cellOf / waitFor: a script that yielded (by its cell), a wait on a cell
// (by the wait's call id): what comes back of the wait is the script's.
func (c *Codex) scriptYielded(id string, raw json.RawMessage) {
	text := rawOutput(raw)
	if m := cellIDOf.FindStringSubmatch(text); m != nil && id != "" {
		if c.cells == nil || len(c.cells) >= maxScriptCalls {
			c.cells = map[string]string{}
		}
		c.cells[m[1]] = id
	}
}

func (c *Codex) waitOn(id, args string) {
	var a struct {
		Cell string `json:"cell_id"`
	}
	if json.Unmarshal([]byte(args), &a) != nil || a.Cell == "" || id == "" {
		return
	}
	if c.waits == nil || len(c.waits) >= maxScriptCalls {
		c.waits = map[string]string{}
	}
	c.waits[id] = a.Cell
}

// scriptOfWait: the script whose results the wait (call id) brings, "" none.
func (c *Codex) scriptOfWait(id string) string {
	if cell, ok := c.waits[id]; ok {
		return c.cells[cell]
	}
	return ""
}

// procsPolled: what script (call id) gave back, its script done, where it
// looked again at one process and ran no command of its own: an exit code
// with no session in it is that process's end. (Of several, or with
// commands of its own, which result is whose is not told: left as it is.)
func (c *Codex) procsPolled(script string, raw json.RawMessage, going []string) {
	sids := c.polls[script]
	if len(sids) != 1 || len(c.scriptCmds[script]) != 0 || len(going) != 0 {
		return
	}
	text := rawOutput(raw) // (as Codex wrote it: codexResult takes its "Script completed" off)
	if !strings.HasPrefix(strings.TrimSpace(text), "Script completed") || !exitedChunk(text) {
		return
	}
	sid := sids[0]
	p := c.procs[sid]
	if p == nil {
		return
	}
	c.endRun(p.id)
	delete(c.procs, sid)
	if p.t != nil {
		p.t.Done = true
		c.touch([]*Tool{p.t})
	}
	c.overAt(sid, p.t)
}

// exitedChunk: text holds an exec_command/write_stdin result with an exit
// code and no session (a process that has ended).
func exitedChunk(text string) bool {
	for i := 0; i < len(text); {
		j := strings.Index(text[i:], `{"chunk_id"`)
		if j < 0 {
			return false
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
		if len(r.ExitCode) > 0 && string(r.ExitCode) != "null" && len(r.Session) == 0 {
			return true
		}
	}
	return false
}

// rawOutput: a call's output's words as they are, its blocks' joined (a
// string as it is).
func rawOutput(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []struct {
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) != nil {
		return ""
	}
	var b strings.Builder
	for _, x := range blocks {
		b.WriteString(x.Text)
	}
	return b.String()
}

// EndedOnHost: commands it set going and left running that the host has
// no process of any more (the caller's scan of the host's processes, of
// this conversation's own, begun after these were set going — Codex does
// not always tell of a process's end: a script that polls several, 11-15).
// gone(r): whether running r is one of them. Each is no longer running, its
// step done (what it gave is not known); its process kept, so that an end
// told after is put on that step all the same. Whether any changed.
func (c *Codex) EndedOnHost(gone func(Running) bool) bool {
	var ended []string
	for _, id := range c.runOrder {
		if r, ok := c.running[id]; ok && r.Kind == "cmd" && gone(r) {
			ended = append(ended, id)
		}
	}
	if len(ended) == 0 {
		return false
	}
	c.rev++
	for _, id := range ended {
		c.endRun(id)
		for _, p := range c.procs {
			if p.id != id {
				continue
			}
			p.over = true // (a step made for it later: done — review r2a-7)
			if p.t != nil && !p.t.Done {
				p.t.Done = true
				c.touch([]*Tool{p.t})
			}
		}
	}
	return true
}
