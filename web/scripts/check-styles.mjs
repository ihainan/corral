// Fail on style rules the project does not take: run first by `pnpm run
// build` (so by every build, the image's too) and by `make check`.
//
// A bar along an element's left edge — to mark the current one, a kind, a
// quote — is refused, whatever draws it: a left border of 2px or more, an
// inset shadow on one side. The one current or of a kind is told by a tint
// or the weight of its words. (A 1px rule between two panes is a divider,
// not a bar: allowed.) Asked three times already (08-04, 10-21, 11-04):
// checked here so that it cannot come back unseen.
import { readFileSync, readdirSync, statSync } from 'node:fs'
import { join, relative } from 'node:path'
import { fileURLToPath } from 'node:url'

const web = fileURLToPath(new URL('..', import.meta.url))
const RULES = [
  [/border-(?:left|inline-start)\s*:\s*(?:[2-9]|\d{2,})(?:\.\d+)?px/, 'a left border of 2px or more'],
  [/border-(?:left|inline-start)-width\s*:\s*(?:[2-9]|\d{2,})/, 'a left border of 2px or more'],
  [/borderLeft(?:Width)?\s*:\s*['"]?(?:[2-9]|\d{2,})/, 'a left border of 2px or more (inline style)'],
  [/inset\s+(?:[2-9]|\d{2,})(?:\.\d+)?px\s+0(?:px)?\s+0(?:px)?(?=[\s;,)])/, 'an inset shadow drawn as a left bar'],
]
const files = []
const walk = (d) => {
  for (const n of readdirSync(d)) {
    const p = join(d, n)
    if (statSync(p).isDirectory()) walk(p)
    else if (/\.(css|tsx)$/.test(n)) files.push(p)
  }
}
walk(join(web, 'src'))
const bad = []
for (const f of files) {
  readFileSync(f, 'utf8').split('\n').forEach((line, i) => {
    for (const [rx, what] of RULES) if (rx.test(line)) bad.push(`${relative(web, f)}:${i + 1}: ${what}: ${line.trim().slice(0, 120)}`)
  })
}
if (bad.length) {
  console.error(bad.join('\n'))
  console.error('left-edge bars are not used in this project: tint the element, or weight its words, instead')
  process.exit(1)
}

// A class given rules in both stylesheets: app.css's (the pages, their
// colours --fg, --line…) and terminal.css's (the terminal and the agents
// page, theirs --term-fg…). A new one is mostly a name taken twice by
// mistake, and the rule of the other sheet then applies where its colours
// are not set: a box drawn with no border, unseen till looked at (11-05,
// .list-search). Those meant to be shared — the terminal's own look of a
// common component — are named here.
const SHARED = new Set(['dialog', 'kb-key', 'reply-desc', 'reply-dialog', 'sheet-list', 'text-preview'])
const heads = (f) => {
  const css = readFileSync(join(web, 'src/styles', f), 'utf8').replace(/\/\*[\s\S]*?\*\//g, '')
  const out = new Set()
  for (const m of css.matchAll(/(?:^|\})\s*([^{}@]+)\{/g))
    for (const part of m[1].split(',')) {
      const c = /^\s*\.([a-zA-Z][\w-]*)/.exec(part)
      if (c) out.add(c[1])
    }
  return out
}
const term = heads('terminal.css')
const twice = [...heads('app.css')].filter((c) => term.has(c) && !SHARED.has(c))
if (twice.length) {
  console.error(`styles: class given rules in both app.css and terminal.css: ${twice.map((c) => '.' + c).join(', ')}`)
  console.error('rename one (or, if the terminal styles a common component on purpose, add it to SHARED in scripts/check-styles.mjs)')
  process.exit(1)
}
console.log('styles: no left-edge bars, no class styled by both sheets')
