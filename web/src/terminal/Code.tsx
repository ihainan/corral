import { useEffect, useState, type ReactNode } from 'react'
import type { Element, Root, RootContent } from 'hast'

// Code coloured by its language, in the terminal's colours (see
// terminal.css, --term-syn-*): shown as text at once, coloured once the
// grammars have come (their own chunk, fetched with the first code shown).
// A language none of them knows stays text.

type Tokens = (code: string, lang: string) => Root | null
let loading: Promise<void> | null = null
let ready: Tokens | null = null
// Every code shown waits for the same load; one that failed is tried
// again by the next code shown, and its success colours all of them.
const waiting = new Set<() => void>()
const load = () =>
  (loading ??= import('./highlight').then(
    (m) => {
      ready = m.tokens
      for (const w of waiting) w()
      waiting.clear()
    },
    () => {
      loading = null // (shown as text meanwhile)
    },
  ))

// (The same code is coloured once: a poll draws its turn again.)
const done = new Map<string, ReactNode>()
const KEEP = 300

// (Another account signed in: none of the code it saw stays in memory.)
export function forgetColours() {
  done.clear()
}

function colour(code: string, lang: string, tokens: Tokens): ReactNode | null {
  const key = lang + '\u0000' + code
  const had = done.get(key)
  if (had !== undefined) return had
  const tree = tokens(code, lang)
  const out = tree ? tree.children.map(node) : null
  done.set(key, out)
  if (done.size > KEEP) done.delete(done.keys().next().value!)
  return out
}

function node(n: RootContent, i: number): ReactNode {
  if (n.type === 'text') return n.value
  if (n.type !== 'element') return null
  const cls = (n as Element).properties?.className
  return (
    <span key={i} className={Array.isArray(cls) ? cls.join(' ') : undefined}>
      {n.children.map(node)}
    </span>
  )
}

export function useColoured(code: string, lang: string | undefined): ReactNode | null {
  const [, setReady] = useState(!!ready)
  useEffect(() => {
    if (!lang || ready) return
    const done = () => setReady(true)
    waiting.add(done)
    void load()
    return () => {
      waiting.delete(done)
    }
  }, [lang])
  return lang && ready ? colour(code, lang, ready) : null
}

// <code> of a code block, a command.
export function Code({ text, lang, className }: { text: string; lang?: string; className?: string }) {
  const c = useColoured(text, lang)
  return <code className={className}>{c ?? text}</code>
}
