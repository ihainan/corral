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
const partTexts = (p: ConvoPart, field: Field): [Field, string][] => {
  const out: [Field, string][] = []
  if (p.text) out.push([p.kind === 'thinking' ? 'thinking' : field, p.text])
  if (p.event?.text) out.push([field, p.event.text])
  const t = p.tool
  if (t) {
    const said = [t.name, t.title, t.detail, t.plan].filter(Boolean).join('\n')
    if (said) out.push(['tool', said])
    const res = [t.result, t.result_tail].filter(Boolean).join('\n')
    if (res) out.push(['output', res])
  }
  return out
}

// What of an item is looked through, by field.
export function fieldsOf(it: Item): [Field, string][] {
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
      return it.steps.flatMap((s) => (isEntry(s) ? s.parts.flatMap((p) => partTexts(p, 'note')) : partTexts(s, 'tool')))
    case 'gone':
      return [] // (taken back: not searched)
  }
}

// One step of looking through text for q (lower case) from at: the places
// begun within [at, at+STEP), and where the next step begins (-1: done).
export function stepThrough(text: string, q: string, at: number): { found: number[]; next: number } {
  const end = Math.min(text.length, at + STEP)
  const window = text.slice(at, Math.min(text.length, end + q.length - 1)).toLowerCase()
  const found: number[] = []
  for (let i = window.indexOf(q); i >= 0 && at + i < end; i = window.indexOf(q, i + Math.max(1, q.length))) found.push(at + i)
  return { found, next: end < text.length ? end : -1 }
}

const excerpt = (text: string, at: number, n: number) => ({
  before: (at > AROUND ? '…' : '') + text.slice(Math.max(0, at - AROUND), at).replace(/\s+/g, ' '),
  match: text.slice(at, at + n),
  after: text.slice(at + n, at + n + AROUND).replace(/\s+/g, ' ') + (at + n + AROUND < text.length ? '…' : ''),
})

// search goes through items for q, giving way every slice ms; done is told
// the hits (in the page's order) and whether there were more than MAX_HITS.
// It returns a function that calls it off.
export function search(items: Item[], query: string, done: (hits: Hit[], capped: boolean) => void, slice = 8, later: (f: () => void) => void = (f) => setTimeout(f, 0)): () => void {
  const q = query.toLowerCase()
  let off = false
  if (!q) {
    done([], false)
    return () => {}
  }
  const hits: Hit[] = []
  let total = 0
  let i = 0 // (the item)
  let fields: [Field, string][] | null = null
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
      const [field, text] = fields[f]
      const step = stepThrough(text, q, at)
      for (const p of step.found) {
        total++
        const h = mine.get(field)
        if (h) h.count++
        else if (hits.length < MAX_HITS) {
          const n: Hit = { key: itemKey(items[i]), field, count: 1, ...excerpt(text, p, q.length) }
          mine.set(field, n)
          hits.push(n)
        }
      }
      if (step.next < 0) {
        f++
        at = 0
      } else at = step.next
    }
    done(hits, total > hits.reduce((n, h) => n + h.count, 0))
  }
  run()
  return () => {
    off = true
  }
}
