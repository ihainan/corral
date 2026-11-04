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
console.log('styles: no left-edge bars')
