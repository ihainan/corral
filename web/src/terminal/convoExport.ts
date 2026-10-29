import type { Item } from './convoItems'
import type { ConvoEntry, ConvoPart, ConvoTool } from './conversation'

// A conversation as read in, as Markdown: what the person said and what the
// agent answered; its steps one line each (output not put in); thinking
// left out, said so; what a rewind took back, and text cut short, marked.
// No React, no DOM; words from say (the page's language).

export interface Say {
  title: string
  exported: string // when, as the page writes it
  from?: string // when the part read in begins, as the page writes it; none: from the start
  you: string
  agent: string
  summary: string
  note: string
  thinking: string
  cut: string
  image: string
  gone: string
  tool: (name: string, what: string, state: 'running' | 'ok' | 'failed') => string
  event: (p: ConvoPart) => string
  trouble: (p: ConvoPart) => string
  head: (title: string, exported: string, from?: string) => string
}

const isEntry = (s: ConvoPart | ConvoEntry): s is ConvoEntry => 'seq' in s
const quote = (s: string) =>
  s
    .split('\n')
    .map((l) => ('> ' + l).trimEnd())
    .join('\n')

function partsText(parts: ConvoPart[], say: Say): string {
  return parts
    .map((p) => {
      if (p.kind === 'image') return say.image
      if (p.kind === 'event') return say.event(p)
      if (p.kind === 'trouble') return say.trouble(p)
      if (p.kind === 'tool' && p.tool) return toolLine(p.tool, say)
      if (p.kind === 'thinking') return ''
      return (p.text ?? '') + (p.truncated ? '\n\n' + say.cut : '')
    })
    .filter((x) => x.trim())
    .join('\n\n')
}

function toolLine(t: ConvoTool, say: Say): string {
  const what = (t.title || t.detail || '').split('\n')[0].slice(0, 160)
  return '- ' + say.tool(t.name, what, !t.done ? 'running' : t.error ? 'failed' : 'ok')
}

// One item's Markdown (empty: nothing of it is put in).
function itemText(it: Item, say: Say): string {
  switch (it.kind) {
    case 'user':
      return `---\n\n**${say.you}**\n\n` + partsText(it.e.parts, say)
    case 'summary':
      return `*${say.summary}*\n\n` + quote(partsText(it.e.parts, say))
    case 'note': {
      const t = [it.e, ...(it.last ? [it.last] : [])].map((e) => partsText(e.parts, say)).join('\n\n')
      return t.trim() ? `*${say.note}* ` + t : ''
    }
    case 'text':
    case 'answer':
      return (it.kind === 'answer' ? `**${say.agent}**\n\n` : '') + it.text + (it.truncated ? '\n\n' + say.cut : '')
    case 'work':
      return '' // (its steps, one piece each: see pieces)
    case 'gone':
      return `*${say.gone}*` // (its entries follow, one piece each: see pieces)
  }
}

// An item's Markdown in pieces, so that a big one (a long run taken back)
// gives way between them too.
function* pieces(it: Item, say: Say): Generator<string> {
  yield itemText(it, say)
  if (it.kind === 'gone') for (const e of it.entries) yield quote(partsText(e.parts, say))
  if (it.kind === 'work') {
    let thought = false
    for (const s of it.steps) {
      if (isEntry(s)) {
        const t = partsText(s.parts, say)
        if (t.trim()) yield s.role === 'summary' ? `*${say.summary}*\n\n` + quote(t) : `*${say.note}* ` + t
      } else if (s.kind === 'thinking') {
        if (!thought && s.text?.trim()) yield say.thinking
        thought = true
      } else if (s.kind === 'tool' && s.tool) yield toolLine(s.tool, say)
      else if (s.text?.trim()) yield partsText([s], say)
    }
  }
}

// exportMarkdown makes the Markdown of items, a slice of them at a time
// (giving way between: a long one does not hold up the page), telling done
// the text; it returns a function that calls it off.
export function exportMarkdown(items: Item[], say: Say, done: (md: Blob) => void, slice = 8, later: (f: () => void) => void = (f) => setTimeout(f, 0)): () => void {
  // (Made into the file a slice at a time too: the text gone into a Blob
  // as each slice ends, so no one step at the end handles all of it.)
  let out: string[] = [say.head(say.title, say.exported, say.from)]
  const made: Blob[] = []
  const keep = () => {
    if (out.length) made.push(new Blob(out.flatMap((p, i) => (i || made.length ? ['\n\n', p] : [p]))))
    out = []
  }
  let i = 0
  let cur: Generator<string> | null = null
  let off = false
  const run = () => {
    if (off) return
    const until = performance.now() + slice
    for (; i < items.length; i++) {
      if (!cur) cur = pieces(items[i], say)
      for (let p = cur.next(); !p.done; p = cur.next()) {
        if (p.value.trim()) out.push(p.value)
        if (performance.now() > until) {
          keep()
          return later(run)
        }
      }
      cur = null
    }
    keep()
    done(new Blob([...made, '\n'], { type: 'text/markdown;charset=utf-8' }))
  }
  run()
  return () => {
    off = true
  }
}
