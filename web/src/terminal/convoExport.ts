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
    case 'work': {
      const lines: string[] = []
      let thought = false
      for (const s of it.steps) {
        if (isEntry(s)) {
          const t = partsText(s.parts, say)
          if (t.trim()) lines.push(`*${say.note}* ` + t)
        } else if (s.kind === 'thinking') {
          if (!thought && s.text?.trim()) lines.push(say.thinking)
          thought = true
        } else if (s.kind === 'tool' && s.tool) lines.push(toolLine(s.tool, say))
        else if (s.text?.trim()) lines.push(partsText([s], say))
      }
      return lines.join('\n')
    }
    case 'gone':
      return `*${say.gone}*\n\n` + quote(it.entries.map((e) => partsText(e.parts, say)).join('\n\n'))
  }
}

// exportMarkdown makes the Markdown of items, a slice of them at a time
// (giving way between: a long one does not hold up the page), telling done
// the text; it returns a function that calls it off.
export function exportMarkdown(items: Item[], say: Say, done: (md: string) => void, slice = 8, later: (f: () => void) => void = (f) => setTimeout(f, 0)): () => void {
  const out: string[] = [say.head(say.title, say.exported, say.from)]
  let i = 0
  let off = false
  const run = () => {
    if (off) return
    const until = performance.now() + slice
    for (; i < items.length; i++) {
      if (performance.now() > until) return later(run)
      const t = itemText(items[i], say)
      if (t.trim()) out.push(t)
    }
    done(out.join('\n\n') + '\n')
  }
  run()
  return () => {
    off = true
  }
}
