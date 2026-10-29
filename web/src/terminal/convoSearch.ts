import type { Item } from './convoItems'
import type { ConvoEntry, ConvoPart } from './conversation'

// Searching what of a conversation is read in: the person's messages, what
// the agent wrote, its thinking, its tools' commands and output, summaries
// and notices (not what a rewind took back). No React, no DOM.
//
// A hit is of an item as the page shows it (its key), so going to one is
// going to that item; where in it (field) is said, and how many times. The
// text is looked through a step at a time (STEP characters, overlapping by
// the search's length less one so nothing across a step's edge is missed),
// so a long search gives way between steps and can be called off.

export type Field = 'user' | 'answer' | 'text' | 'thinking' | 'tool' | 'output' | 'summary' | 'note'
export interface Hit {
  key: string // the item's (as the page keys it)
  field: Field
  tool?: string // (a tool's command or output) its id: its card is opened when gone to
  count: number
  before: string
  match: string
  after: string
}

export const STEP = 64 * 1024
export const MAX_HITS = 500
const AROUND = 40

export const itemKey = (it: Item) => (it.kind === 'work' || it.kind === 'answer' || it.kind === 'text' || it.kind === 'gone' ? it.key : 'e' + it.e.seq)

const isEntry = (s: ConvoPart | ConvoEntry): s is ConvoEntry => 'seq' in s
type Text = [Field, string, string?] // (field, text, the tool's id)
// (Each text as it is, none joined to another: a long one is not copied
// before it is looked through.)
const partTexts = (p: ConvoPart, field: Field): Text[] => {
  const out: Text[] = []
  const add = (f: Field, s?: string, tool?: string) => s && out.push([f, s, tool])
  add(p.kind === 'thinking' ? 'thinking' : field, p.text)
  const ev = p.event
  if (ev) for (const s of [ev.text, ev.goal, ev.why]) add(field, s)
  if (p.trouble) for (const s of [p.trouble.name, p.trouble.text]) add(field, s)
  if (p.noted) for (const s of p.noted.paths) add(field, s)
  const t = p.tool
  if (t) {
    for (const s of [t.name, t.title, t.detail, t.plan]) add('tool', s, t.id)
    for (const s of [t.result, t.result_tail]) add('output', s, t.id)
  }
  return out
}

// What of an item is looked through, by field. (Task-list calls the page
// leaves out of a turn, shown in its task list instead, are not.)
export function fieldsOf(it: Item): Text[] {
  switch (it.kind) {
    case 'user':
      return it.e.parts.flatMap((p) => partTexts(p, 'user'))
    case 'summary':
      return it.e.parts.flatMap((p) => partTexts(p, 'summary'))
    case 'note':
      return [it.e, ...(it.last ? [it.last] : [])].flatMap((e) => e.parts.flatMap((p) => partTexts(p, 'note')))
    case 'text':
      return [['text', it.text]]
    case 'answer':
      return [['answer', it.text]]
    case 'work':
      return it.steps.flatMap((s) => (isEntry(s) ? s.parts.flatMap((p) => partTexts(p, s.role === 'summary' ? 'summary' : 'note')) : partTexts(s, 'tool')))
    case 'gone':
      return [] // (taken back: not searched)
  }
}

// The search as a pattern: its letters whatever their case, by the
// Unicode rules (no text lower-cased first: a letter whose lower case is
// longer would put every place after it out).
export const patternOf = (q: string) => new RegExp(q.replace(/[.*+?^${}()|[\]\\]/g, '\\$&'), 'giu')

// One step of looking through text for re (patternOf) from at: the places
// begun within [at, at+STEP), each with its length, and where the next step
// begins (-1: done).
export function stepThrough(text: string, re: RegExp, qLen: number, at: number): { found: [number, number][]; next: number } {
  const end = Math.min(text.length, at + STEP)
  const window = text.slice(at, Math.min(text.length, end + qLen * 2))
  const found: [number, number][] = []
  let past = end // (the next step begins after the last one found: none counted twice)
  re.lastIndex = 0
  for (let m = re.exec(window); m && at + m.index < end; m = re.exec(window)) {
    found.push([at + m.index, m[0].length])
    past = Math.max(past, at + m.index + m[0].length)
    if (!m[0].length) re.lastIndex++
  }
  return { found, next: past < text.length ? past : -1 }
}

const excerpt = (text: string, at: number, n: number) => ({
  before: (at > AROUND ? '…' : '') + text.slice(Math.max(0, at - AROUND), at).replace(/\s+/g, ' '),
  match: text.slice(at, at + n),
  after: text.slice(at + n, at + n + AROUND).replace(/\s+/g, ' ') + (at + n + AROUND < text.length ? '…' : ''),
})

// search goes through items for q, giving way every slice ms; done is told
// the hits (in the page's order) and whether there were more than MAX_HITS.
// It returns a function that calls it off.
// (MAX_HITS places at most: past it, it stops there, said so.)
export function search(items: Item[], query: string, done: (hits: Hit[], capped: boolean) => void, slice = 8, later: (f: () => void) => void = (f) => setTimeout(f, 0)): () => void {
  const q = query
  const re = patternOf(q)
  let off = false
  if (!q) {
    done([], false)
    return () => {}
  }
  const hits: Hit[] = []
  let total = 0
  let i = 0 // (the item)
  let fields: Text[] | null = null
  let f = 0 // (its field)
  let at = 0 // (where in it)
  let mine = new Map<Field, Hit>() // (the item's hits, one a field)
  const run = () => {
    if (off) return
    const until = performance.now() + slice
    while (i < items.length) {
      if (performance.now() > until) return later(run)
      if (!fields) {
        fields = fieldsOf(items[i])
        f = 0
        at = 0
        mine = new Map()
      }
      if (f >= fields.length) {
        i++
        fields = null
        continue
      }
      const [field, text, tool] = fields[f]
      const step = stepThrough(text, re, q.length, at)
      for (const [p, n] of step.found) {
        if (++total > MAX_HITS) return done(hits, true)
        const h = mine.get(field)
        if (h) h.count++
        else {
          const hit: Hit = { key: itemKey(items[i]), field, tool, count: 1, ...excerpt(text, p, n) }
          mine.set(field, hit)
          hits.push(hit)
        }
      }
      if (step.next < 0) {
        f++
        at = 0
      } else at = step.next
    }
    done(hits, false)
  }
  run()
  return () => {
    off = true
  }
}
