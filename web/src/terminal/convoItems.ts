import type { ConvoEntry, ConvoPart, ConvoTool } from './conversation'

// The conversation view's list, from the log's entries: no React, no DOM
// (convoItems.test.mts runs it as it is).

// What a turn holds: its steps, the text it wrote between them, the notes
// that came meanwhile.
export type Step = ConvoPart | ConvoEntry

export type Item =
  | { kind: 'user'; e: ConvoEntry }
  | { kind: 'note'; e: ConvoEntry }
  | { kind: 'summary'; e: ConvoEntry }
  // A run of steps (calls and thinking) between what the agent wrote: one line, opened on demand.
  | { kind: 'work'; key: string; steps: Step[]; live: boolean; moving: boolean; busy?: ConvoPart }
  // What the agent wrote between its steps: shown as it is, as its answer is.
  | { kind: 'text'; key: string; text: string; truncated?: boolean }
  | { kind: 'answer'; key: string; text: string; truncated?: boolean; ts?: string; span?: number; edits: Edits | null }

// What a turn changed: its files in the order it first changed them, each
// with its edits.
export interface Edits {
  files: FileEdits[]
  add: number
  del: number
  more?: number // (files a command changed beyond those kept)
}
export interface FileEdits {
  path: string
  add: number
  del: number
  tools: ConvoTool[]
}

// When each part was written (its entry's time).
export const partTime = new WeakMap<ConvoPart, string>()
// Where each part is in the log (its entry's seq, its place in it): what an
// item is known by, whatever turn it is found in (a turn read from its middle
// takes in its start as the part before is read in, and keeps its items).
const partKey = new WeakMap<ConvoPart, string>()
const stepKey = (s: Step) => (isNote(s) ? 'e' + s.seq : (partKey.get(s) ?? '?'))

// items groups the entries by turn, and a turn into what the agent wrote
// (shown) and the runs of steps between (a line each). live: the last turn
// goes on (the agent works, or waits for an answer): it has no answer yet,
// its last run is the one going on; moving: it works (a step is under way).
// A turn's answer is the text it ends with (notes after it come after it);
// a turn that ends in a step has none.
// A message the person sent while the agent worked, taken in during the
// turn (mid), is the turn's: it shows where it was taken in (the work before
// it one run, the work after another), and does not end the turn.
export function items(entries: ConvoEntry[], live: boolean, moving: boolean): Item[] {
  const out: Item[] = []
  let turn: { from?: string; steps: Step[]; to?: string; key: string } | null = null
  const end = (last: boolean) => {
    if (!turn) return
    const { steps, key, from, to } = turn
    turn = null
    const goesOn = last && live
    // (Thinking with nothing in it, as Claude Code mostly logs it, is not a
    // step: it neither shows nor ends what the turn ends with.)
    const blank = (s: Step) => !isNote(s) && s.kind === 'thinking' && !s.text?.trim()
    let a = steps.length
    let tail = steps.length
    if (!goesOn) {
      while (tail > 0 && (isNote(steps[tail - 1]) || blank(steps[tail - 1]))) tail--
      a = tail
      while (a > 0 && (isText(steps[a - 1]) || blank(steps[a - 1]))) a--
    }
    const answer = steps.slice(a, tail).filter(isText) as ConvoPart[]
    // (A task call that failed stays: its error is to be seen. A run is
    // known by its first step, a text by itself: see partKey.)
    const work = steps.slice(0, a).filter((s) => (!isTaskCall(s) || (s as ConvoPart).tool!.error) && !blank(s))
    let run: Step[] = []
    let live1 = false // (a run going on was shown)
    const flush = (final: boolean) => {
      if (!run.length) return
      // Going on: the turn's last run, with a call not come back or while
      // it works. Only the last: a run something came after (a notice that
      // the person stopped it, say) is over, and a call of its own that
      // never came back is not still running.
      const on = goesOn && final && (run.some((x) => !isNote(x) && !!x.tool && !x.tool.done) || moving)
      if (on) live1 = true
      // What is under way is said whenever there is one, not only while the
      // agent is known to be working: a call that has not come back is
      // running whatever its pane last reported (it is "waiting for you",
      // not "working", while it asks leave to run a command), and a line
      // saying only "3 commands" says nothing of which one that is.
      out.push({ kind: 'work', key: 'g' + stepKey(run[0]), steps: run, live: on, moving: on && moving, busy: on ? underWay(run) : undefined })
      run = []
    }
    for (const s of work) {
      if (isNote(s)) {
        flush(false)
        out.push(noteItem(s))
      } else if (s.kind === 'text') {
        flush(false)
        out.push({ kind: 'text', key: 'x' + stepKey(s), text: s.text ?? '', truncated: s.truncated })
      } else run.push(s)
    }
    const endsInRun = run.length > 0
    flush(true)
    // Working on after what it wrote last (thinking, say): a line says so.
    if (goesOn && moving && !endsInRun && !live1) {
      let k = steps.length
      while (k > 0 && !isText(steps[k - 1]) && !isNote(steps[k - 1])) k--
      out.push({ kind: 'work', key: 'g' + key + '.on', steps: [], live: true, moving, busy: underWay(steps.slice(k)) })
    }
    // (How long it worked, where it did any work: thinking counts, shown or not.)
    const worked = steps.slice(0, tail).some((x) => !isNote(x) && x.kind !== 'text')
    if (answer.length)
      out.push({
        kind: 'answer',
        key: 'a' + stepKey(answer[0]),
        text: answer.map((p) => p.text ?? '').join('\n\n'),
        truncated: answer.some((p) => p.truncated),
        ts: partTime.get(answer[answer.length - 1]),
        span: worked && from && to && ms(to) >= ms(from) ? ms(to) - ms(from) : undefined,
        edits: edits(work),
      })
    for (const s of steps.slice(tail)) if (isNote(s)) out.push(noteItem(s))
  }
  for (const e of entries) {
    if (e.role === 'user' && e.mid && turn) {
      turn.steps.push(e) // (as a note is: in its place)
      continue
    }
    if (e.role === 'user' || (e.role === 'summary' && !turn)) {
      end(false)
      out.push({ kind: e.role, e } as Item)
      turn = { key: 't' + e.seq, from: e.role === 'user' ? e.ts : undefined, steps: [] }
      continue
    }
    if (!turn) turn = { key: 't' + e.seq, steps: [] } // (the log read begins mid-turn)
    const tr: { steps: Step[]; to?: string } = turn
    if (e.role !== 'assistant') {
      tr.steps.push(e) // (a note, or the summary of a compaction in the middle of the turn)
      continue
    }
    if (e.ts) tr.to = e.ts
    e.parts.forEach((p, i) => {
      if (p.kind === 'image') return
      if (e.ts) partTime.set(p, e.ts)
      partKey.set(p, e.seq + '.' + i)
      tr.steps.push(p)
    })
  }
  end(true)
  return out
}

export const noteItem = (e: ConvoEntry): Item => (e.role === 'summary' ? { kind: 'summary', e } : e.role === 'user' ? { kind: 'user', e } : { kind: 'note', e })

// The step under way: the latest call not come back (of several run at
// once, any), else thinking when that is the last thing.
export function underWay(steps: Step[]): ConvoPart | undefined {
  let last: ConvoPart | undefined
  for (let i = steps.length - 1; i >= 0; i--) {
    const s = steps[i]
    if (isNote(s) || isTaskCall(s)) continue
    if (s.tool && !s.tool.done) return s
    last ??= s
  }
  return last?.kind === 'thinking' ? last : undefined
}

export const isNote = (s: Step): s is ConvoEntry => 'role' in s
// The task list's own calls: it shows over the composer, not as steps.
export const isTaskCall = (s: Step) => !isNote(s) && !!s.tool && verbOf(s.tool.name) === 'task'

export const isText = (s: Step) => !isNote(s) && s.kind === 'text'

export function edits(steps: Step[]): Edits | null {
  const files = new Map<string, FileEdits>()
  let add = 0
  let del = 0
  let more = 0
  const put = (path: string, a: number, d: number, tl: ConvoTool) => {
    let f = files.get(path)
    if (!f) files.set(path, (f = { path, add: 0, del: 0, tools: [] }))
    f.add += a
    f.del += d
    f.tools.push(tl)
    add += a
    del += d
  }
  for (const s of steps) {
    const tl = !isNote(s) ? s.tool : undefined
    if (!tl || tl.error) continue
    // A command that changed files (its own patch for each) counts as an
    // edit does; its line stays a command's.
    if (tl.files?.length) {
      more += tl.more_files ?? 0
      const each = perFile(tl)
      tl.files.forEach((f, i) => put(f.path, f.add ?? 0, f.del ?? 0, each[i]))
      continue
    }
    if (category(tl.name) !== 'edit') continue
    put(tl.detail ?? '', tl.add ?? 0, tl.del ?? 0, tl)
  }
  return files.size ? { files: [...files.values()], add, del, more } : null
}

// A command's files, each as an edit of its own (the same ones each time,
// so that what shows of them is not made anew on every reading).
const files = new WeakMap<ConvoTool, ConvoTool[]>()
function perFile(tl: ConvoTool): ConvoTool[] {
  let out = files.get(tl)
  if (!out) files.set(tl, (out = (tl.files ?? []).map((f) => ({ ...tl, detail: f.path, add: f.add, del: f.del, patch: f.patch, patch_cut: f.patch_cut, old: undefined, new: undefined, same: false }))))
  return out
}

export const sameEdits = (a: Edits | null, b: Edits | null) =>
  a === b || (!!a && !!b && a.add === b.add && a.del === b.del && a.more === b.more && a.files.length === b.files.length && a.files.every((f, i) => f.path === b.files[i].path && f.tools.length === b.files[i].tools.length && f.tools.every((t, j) => t === b.files[i].tools[j])))

export type Category = 'cmd' | 'read' | 'edit' | 'search' | 'web' | 'agent' | 'other'

export function category(name: string): Category {
  switch (name) {
    case 'Bash':
    case 'BashOutput':
      return 'cmd'
    case 'Read':
      return 'read'
    case 'Edit':
    case 'MultiEdit':
    case 'Write':
    case 'NotebookEdit':
      return 'edit'
    case 'Grep':
    case 'Glob':
    case 'WebSearch':
    case 'ToolSearch':
      return 'search'
    case 'WebFetch':
      return 'web'
    case 'Agent':
    case 'Task':
      return 'agent'
  }
  return 'other'
}

// An MCP server's tool name (mcp__<server>__<tool>), split: the whole one
// is too long for a line, and most of it is the server's. The same split
// the log's reader makes (internal/convo: mcpTool).
export function mcpTool(name: string): { server: string; tool: string } | null {
  if (!name.startsWith('mcp__')) return null
  const rest = name.slice(5)
  const i = rest.lastIndexOf('__')
  if (i <= 0 || i + 2 >= rest.length) return rest ? { server: '', tool: rest } : null
  return { server: rest.slice(0, i), tool: rest.slice(i + 2) }
}

// The name a step's line shows for a tool nothing here knows: an MCP
// server's tool without the server, else the name as it is.
export const toolName = (name: string) => mcpTool(name)?.tool ?? name

// The word a step's line starts with: what it did, or (live) does.
export type Verb = Category | 'write' | 'task' | 'ask' | 'wait' | 'stop' | 'page' | 'send' | 'skill' | 'agents' | 'cron' | 'advisor' | 'browser' | 'doc' | 'mcp'
export function verbOf(name: string): Verb | null {
  switch (name) {
    case 'Write':
      return 'write'
    case 'TaskCreate':
    case 'TaskUpdate':
    case 'TodoWrite':
      return 'task'
    case 'AskUserQuestion':
      return 'ask'
    case 'Monitor':
      return 'wait'
    case 'TaskStop':
      return 'stop'
    case 'Artifact':
    case 'ArtifactData':
      return 'page'
    case 'SendMessage':
      return 'send'
    case 'Skill':
      return 'skill'
    case 'ListAgents':
      return 'agents'
    case 'CronCreate':
    case 'CronDelete':
    case 'CronList':
    case 'ScheduleWakeup':
      return 'cron'
    case 'advisor':
      return 'advisor'
  }
  const m = mcpTool(name)
  // (The browser's doings are its own word; a documents server's are a
  // document's. Any other server: a tool was called, and which.)
  if (m) return m.tool.startsWith('browser_') ? 'browser' : /notion|docs/i.test(m.server) ? 'doc' : 'mcp'
  const c = category(name)
  return c === 'other' ? null : c
}

export const ms = (ts?: string) => {
  const v = ts ? Date.parse(ts) : NaN
  return isNaN(v) ? 0 : v
}
