import type { ConvoEntry, ConvoEvent, ConvoPart, ConvoTool } from './conversation'
import { t, type I18nKey } from '../i18n'

// The conversation view's list, from the log's entries: no React, no DOM
// (convoItems.test.mts runs it as it is).

// What a turn holds: its steps, the text it wrote between them, the notes
// that came meanwhile.
export type Step = ConvoPart | ConvoEntry

export type Item =
  | { kind: 'user'; e: ConvoEntry }
  // (idle: a compaction said so by a notice after it; last: of retries
  // next to each other, the last, told on the first's line)
  | { kind: 'note'; e: ConvoEntry; idle?: boolean; last?: ConvoEntry }
  | { kind: 'summary'; e: ConvoEntry }
  // A run of steps (calls and thinking) between what the agent wrote: one line, opened on demand.
  | { kind: 'work'; key: string; steps: Step[]; live: boolean; moving: boolean; busy?: ConvoPart }
  // What the agent wrote between its steps: shown as it is, as its answer is.
  | { kind: 'text'; key: string; text: string; truncated?: boolean }
  | { kind: 'answer'; key: string; text: string; truncated?: boolean; ts?: string; span?: number; edits: Edits | null }
  // What a rewind took back (the agent no longer has it): one line, in its place.
  | { kind: 'gone'; key: string; entries: ConvoEntry[] }

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
// A goal set with /goal is told three times over, each the whole of it: Claude
// Code's event, the person's message, its "Goal set: …". The message says it;
// the event goes and the note is the short line that it took. (By their words
// and being next to each other: read from the middle of a log, or the goal set
// otherwise, each stands as it is.)
const goalWords = (s: string) => s.replace(/\s+/g, ' ').trim().slice(0, 200)
export function goalOnce(entries: ConvoEntry[]): ConvoEntry[] {
  let out: ConvoEntry[] | null = null
  for (let i = 0; i < entries.length; i++) {
    const e = entries[i]
    const said = e.role === 'user' ? e.parts[0]?.text : undefined
    if (!said?.startsWith('/goal ')) continue
    const words = goalWords(said.slice(6))
    const before = entries[i - 1]
    const ev = before?.role === 'note' ? before.parts[0]?.event : undefined
    const after = entries[i + 1]
    const note = after?.role === 'note' ? after.parts[0]?.text : undefined
    const set = !!note?.startsWith('Goal set: ') && goalWords(note.slice(10)) === words
    const dropped = ev?.kind === 'goal' && ev.set && goalWords(ev.goal ?? '') === words
    if (!set && !dropped) continue
    out ??= [...entries]
    if (dropped) out[i - 1] = { ...before, parts: [] }
    if (set) out[i + 1] = { ...after, parts: [{ kind: 'event', event: { kind: 'goal', set: true } }] }
  }
  return out ? out.filter((e) => e.role !== 'note' || e.parts.length > 0) : entries
}

// Claude Code's own commands and what it says of them, told in the chat as
// if the person had written them: "/model", "/exit" and its "Goodbye!", "Set
// effort level to …", a Monitor's "Monitor event: …" four hundred times over.
// One short line each, in the page's words, where they carry anything; none
// where they do not (a bare /compact: the compaction's own line says it).
// 11-14: the model menu here went through /model, and each use left the
// command and its English in the thread. Read from 288 real logs: the
// commands and notes below are all that came up more than once.
const MODEL_SET = /^Set model to\s+(?:`([^`]+)`|(.+?))(?=\s+and saved\b|\s+for\s+this\b|\s+with\s+\S+\s+effort\b|\s*$)/
const MODEL_KEPT = /^Kept model as\s+`?([^`]+?)`?\.?\s*$/
const EFFORT_SET = /^Set effort level to\s+`?(\w+)`?\s*\((this session only|saved as your default[^)]*)\)/
const BYE = /^(Goodbye|Bye|See ya|Catch you later)!?$/
const plainName = (m: string) => m.replace(/\s*\(default\)\s*$/, '').trim()
export function modelEvent(text: string | undefined): ConvoEvent | undefined {
  const s = text?.trim()
  if (!s) return undefined
  const kept = MODEL_KEPT.exec(s)
  if (kept) return { kind: 'model', model: plainName(kept[1]), kept: true }
  const set = MODEL_SET.exec(s)
  if (!set) return undefined
  return { kind: 'model', model: plainName(set[1] ?? set[2]), effort: /\bwith\s+`?(\w+)`?\s+effort\b/.exec(s)?.[1], session: /for this session only/.test(s), saved: /and saved as your default/.test(s) }
}
const said = (text: string, ic: string, warn = false): ConvoEvent => ({ kind: 'said', text, ic, level: warn ? 'warning' : undefined })
// What a note of Claude Code's own comes to: a line (an event), nothing
// (null: dropped), or undefined — left as it is.
export function noteEvent(text: string | undefined): ConvoEvent | null | undefined {
  const s = text?.trim()
  if (!s) return undefined
  let m: RegExpExecArray | null
  const model = modelEvent(s)
  if (model) return model
  if ((m = EFFORT_SET.exec(s))) return said(t(m[2] === 'this session only' ? 'convo.cmd.effortSession' : 'convo.cmd.effortSaved', { e: effortWord(m[1]) }), 'effort')
  if (BYE.test(s)) return said(t('convo.cmd.exit'), 'exit')
  if (/^Compacted\b/.test(s)) return null
  if ((m = /^Session renamed to:\s*(.+)$/.exec(s))) return said(t('convo.cmd.renamed', { name: m[1].trim() }), 'rename')
  // (Its event under its first line, where the gateway kept it — aaf62bd: "gpt done", a few lines at most.)
  if ((m = /^Monitor event:\s*"(.+?)"[ \t]*(?:\n([\s\S]*))?$/.exec(s))) {
    const ev = m[2]?.trim().split(/\s*\n\s*/).filter(Boolean).join(' · ')
    return said(ev ? t('convo.cmd.monitorSaid', { what: m[1], ev }) : t('convo.cmd.monitorEvent', { what: m[1] }), 'monitor')
  }
  if ((m = /^Monitor "(.+)" stream ended$/s.exec(s))) return said(t('convo.cmd.monitorEnded', { what: m[1] }), 'monitor')
  if ((m = /^(?:(\d+) background shell command tasks|Background shell command) didn't finish before the previous session ended/.exec(s))) return said(t('convo.cmd.bgLeft', { n: m[1] ?? '1' }), 'bg')
  if ((m = /^Background command "(.+)" was stopped because the system is running low on memory$/s.exec(s))) return said(t('convo.cmd.bgLowMemory', { what: m[1] }), 'bg', true)
  if ((m = /^Background command "(.+?)" was stopped\b/s.exec(s))) return said(t('convo.cmd.bgStopped', { what: m[1] }), 'bg')
  if ((m = /^Monitor "(.+)" stopped$/s.exec(s))) return said(t('convo.cmd.monitorStopped', { what: m[1] }), 'monitor')
  if ((m = /^Background agent "(.+)" didn't finish before the previous session ended/s.exec(s))) return said(t('convo.cmd.agentLeft', { what: m[1] }), 'agent')
  if ((m = /^(\d+) background shell command task\(s\) from the previous session have no completion record/.exec(s))) return said(t('convo.cmd.bgLeft', { n: m[1] }), 'bg')
  if ((m = /^Stopped watching Artifact:\s*"(.+)"\s*\(connection lost\)$/s.exec(s))) return said(t('convo.cmd.artifactLost', { what: m[1] }), 'artifact')
  if ((m = /^Auto-replied to comment thread on Artifact:\s*"(.+)"$/s.exec(s))) return said(t('convo.cmd.artifactReplied', { what: m[1] }), 'artifact')
  if ((m = /^Agent "(.+?)" failed:\s*(.*)$/s.exec(s))) return said(t('convo.cmd.agentFailed', { what: m[1], why: m[2].trim() }), 'agent', true)
  if ((m = /^Agent "(.+)" was stopped by Claude$/s.exec(s))) return said(t('convo.cmd.agentStopped', { what: m[1] }), 'agent')
  if (/^Goal check-in: background work still running$/.test(s)) return said(t('convo.cmd.goalCheck'), 'goal')
  if (/^Can't restart while work is running in the background/.test(s)) return said(t('convo.cmd.restartBusy'), 'restart', true)
  if ((m = /^Successfully updated from \S+ to version (\S+)$/.exec(s))) return said(t('convo.cmd.updated', { v: m[1] }), 'restart')
  return undefined
}
const effortWord = (e: string) => {
  const k = `convo.effort.${e.toLowerCase()}`
  const w = t(k as I18nKey)
  return w === k ? e : w
}
// A command of the person's, alone (the words it was given aside): its line
// where it says what it did, null where something after says it, undefined
// to keep it as the person's message.
function commandEvent(cmd: string, rest: string): ConvoEvent | null | undefined {
  switch (cmd) {
    case '/model':
    case '/effort':
      return rest ? undefined : null // (the menu shut on nothing, or its answer follows)
    case '/compact':
      return rest ? undefined : null // (the compaction's own line says it)
    case '/exit':
    case '/quit':
      return said(t('convo.cmd.exit'), 'exit')
    case '/clear':
      return said(t('convo.cmd.clear'), 'clear')
    case '/restart':
      return said(t('convo.cmd.restart'), 'restart')
    case '/rename':
      return rest ? said(t('convo.cmd.renamed', { name: rest }), 'rename') : null
  }
  return undefined
}
// (Commands whose answer, right after, says what they did, given words or not.)
const ANSWERED = new Set(['/model', '/effort', '/exit', '/quit', '/compact', '/rename', '/restart', '/clear'])
export function commandsOnce(entries: ConvoEntry[]): ConvoEntry[] {
  let out: ConvoEntry[] | null = null
  const dropped = new Set<number>()
  const put = (i: number, ev: ConvoEvent | null) => {
    out ??= [...entries]
    if (ev) out[i] = { ...entries[i], role: 'note', parts: [{ kind: 'event', event: ev }] }
    else dropped.add(i)
  }
  for (let i = 0; i < entries.length; i++) {
    const e = entries[i]
    if (e.parts.length !== 1 || e.gone) continue
    const text = e.parts[0]?.text?.trim()
    if (e.role === 'user' && text && /^\/[a-z][\w-]*(\s|$)/.test(text) && e.parts[0].kind === 'text') {
      const [cmd, ...more] = text.split(/\s+/)
      const ev = commandEvent(cmd, more.join(' '))
      // (Its answer, right after: the line, the command's place taken by it —
      // "/model opus" and its "Set model to opus" as much as a bare one.)
      const after = entries[i + 1]
      const answer = after?.role === 'note' && after.parts.length === 1 ? noteEvent(after.parts[0]?.text) : undefined
      if (ev === undefined && (answer === undefined || !ANSWERED.has(cmd))) continue
      if (answer !== undefined) {
        put(i, null)
        put(i + 1, answer ?? ev ?? null)
        i++
      } else if (ev !== undefined) put(i, ev)
      continue
    }
    if (e.role === 'note' && e.parts[0].kind === 'text') {
      const ev = noteEvent(text)
      if (ev !== undefined) put(i, ev)
    }
  }
  return out ? (out as ConvoEntry[]).filter((_, i) => !dropped.has(i)) : entries
}

export function items(entriesIn: ConvoEntry[], live: boolean, moving: boolean): Item[] {
  const entries = commandsOnce(goalOnce(entriesIn))
  const out: Item[] = []
  const merged = mergeNotes(entries)
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
        // (Under a second, nothing worth the words — and a history imported
        // all at once, its turns told as of that moment, says "0s" each.)
        span: worked && from && to && ms(to) - ms(from) >= 1000 ? ms(to) - ms(from) : undefined,
        edits: edits(work),
      })
    for (const s of steps.slice(tail)) if (isNote(s)) out.push(noteItem(s))
  }
  for (let i = 0; i < entries.length; i++) {
    const e = entries[i]
    if (merged.has(e)) continue
    // What was taken back is of no turn: it neither starts one nor counts in
    // one (its time, its edits, its answer). One line in its place: between
    // turns where it begins with a message of the person's (a rewind), else
    // among the steps of the turn it is in, where that goes on after it (an
    // answer withdrawn and given again).
    if (e.gone) {
      let j = i + 1
      while (j < entries.length && entries[j].gone) j++
      const it: Item = { kind: 'gone', key: 'r' + e.seq, entries: entries.slice(i, j) }
      i = j - 1
      if (turn && e.role !== 'user' && j < entries.length && entries[j].role !== 'user') {
        const mark: ConvoEntry = { seq: e.seq, rev: e.rev, id: it.key, role: 'note', parts: [] }
        goneMark.set(mark, it)
        turn.steps.push(mark)
      } else {
        end(false)
        out.push(it)
      }
      continue
    }
    if (e.role === 'user' && e.mid && turn) {
      turn.steps.push(e) // (as a note is: in its place)
      continue
    }
    // (A scheduled task's turn is a turn of its own, as one the person
    // asked for: its line shows where the person's message would.)
    if (e.role === 'user' || (e.role === 'summary' && !turn) || eventOf(e) === 'scheduled') {
      end(false)
      out.push(e.role === 'note' ? noteItem(e) : ({ kind: e.role, e } as Item))
      turn = { key: 't' + e.seq, from: e.role !== 'summary' ? e.ts : undefined, steps: [] }
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

export const noteItem = (e: ConvoEntry): Item => {
  const m = goneMark.get(e)
  if (m) return m
  if (e.role === 'summary' || e.role === 'user') return { kind: e.role, e }
  const x = told.get(e)
  return x ? { kind: 'note', e, ...x } : { kind: 'note', e }
}

const eventOf = (e: ConvoEntry) => (e.role === 'note' ? e.parts[0]?.event?.kind : undefined)
const retryOf = (e: ConvoEntry) => (e.role === 'note' && !e.gone && e.parts[0]?.trouble?.kind === 'api' && e.parts[0].trouble.attempt ? e.parts[0].trouble : undefined)

// What a note's line tells of the notes after it (see mergeNotes).
const told = new WeakMap<ConvoEntry, { idle?: boolean; last?: ConvoEntry }>()

// The notes said on another's line, not on their own: the notice that a
// compaction was made while the session sat idle (on that compaction's,
// where it comes after it with no turn begun between), and retries of a
// request next to each other (on the first's: the last of them told).
// Among the entries read only: one whose fellow is not read in shows alone.
function mergeNotes(entries: ConvoEntry[]): Set<ConvoEntry> {
  const merged = new Set<ConvoEntry>()
  let compact: ConvoEntry | null = null
  let retry: ConvoEntry | null = null // (the first of a run of them)
  for (let i = 0; i < entries.length; i++) {
    const e = entries[i]
    const ev = eventOf(e)
    // (A scheduled turn told twice, by its firing and its own row: once.)
    // (Its firing just before it: not another turn's own row, told so too.)
    if (ev === 'scheduled' && e.parts[0].event!.echo && !e.gone && i > 0 && eventOf(entries[i - 1]) === 'scheduled' && !entries[i - 1].parts[0].event!.echo && !entries[i - 1].gone) {
      merged.add(e)
      continue
    }
    if (e.role === 'user' || ev === 'scheduled') compact = null
    if (ev === 'compact' && !e.gone) {
      compact = e
      told.delete(e)
    } else if (ev === 'idle_compact' && compact && !e.gone) {
      told.set(compact, { ...told.get(compact), idle: true })
      merged.add(e)
      compact = null
    }
    // (Its safeguards stopped it and it tried once more, then stopped again: the note of that says it.)
    const stop = e.parts[0]?.event
    if (stop?.kind === 'notice' && !e.gone && /safeguards stopped the response above/.test(stop.text ?? '') && entries.slice(i + 1).find((x) => x.role !== 'assistant' || x.parts.some((q) => q.kind !== 'thinking'))?.parts[0]?.trouble?.kind === 'refused') {
      merged.add(e)
      continue
    }
    // (A notice said again next to itself: once.)
    const n = e.parts[0]?.event
    if (n?.kind === 'notice' && !e.gone && i > 0) {
      const p = entries[i - 1].parts[0]?.event
      if (p?.kind === 'notice' && !entries[i - 1].gone && p.text === n.text && p.level === n.level) {
        merged.add(e)
        continue
      }
    }
    if (retryOf(e)) {
      if (retry && retryOf(entries[i - 1])) {
        told.set(retry, { ...told.get(retry), last: e })
        merged.add(e)
      } else {
        retry = e
        told.delete(e)
      }
    } else retry = null
  }
  return merged
}

// (What was taken back within a turn, as a step of it: the line it shows as.)
const goneMark = new WeakMap<ConvoEntry, Item>()

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
    case 'request_user_input': // (Codex's)
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
