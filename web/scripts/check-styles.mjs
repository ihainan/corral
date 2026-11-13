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
const SHARED = new Set(['dialog', 'kb-key', 'reply-desc', 'reply-dialog', 'sheet-list', 'sheet-mode', 'text-preview'])
const heads = (f) => {
  const css = readFileSync(join(web, 'src/styles', f), 'utf8').replace(/\/\*[\s\S]*?\*\//g, '')
  const out = new Set()
  for (const m of css.matchAll(/(?:^|[{}])\s*([^{}@]+)\{/g))
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
// ---- appended to scripts/check-styles.mjs ----

// The scale (app.css, :root) is kept here, where a value off it fails the
// build. Without this the sheets drift back: they had, to six different lines
// for 13px text and a token block describing a scale nothing used.
//
// Type: a size and a line are taken together, from a role's pair. A number
// written in either is refused, so the two can never part company again.
// Corners: the five steps, a pill, a circle, or nothing.
// Spacing: plain numbers, but only these. A token named for its own value
// would buy nothing a number does not, and read worse at every use.
const STEP = new Set([0, 1, 2, 3, 4, 6, 8, 10, 12, 14, 16, 20, 24, 28, 32, 40, 48, 56, 64])
const SPACED = /^(padding|margin|gap|row-gap|column-gap)(-(top|right|bottom|left|inline|block)(-(start|end))?)?$/
// Measured against something that is not the scale: a touch target made up
// out of a line and its padding, a bar as tall as the text beside it, room
// kept for a control of a known width. Each says so where it stands.
const EXEMPT = [
  /\.sc-(row|item|label|desc)\b/, /\.kb-|\.keybar/, /\.term-(status|title|host)/,
  /\.convo-pic\b/, /\.convo-runs-item\b/, /\.md-code-copy\b/, /\.convo-trouble\b/,
  /\.convo-panelwin/, /\.reply-screen\b/, /\.text-preview-code\b/, /\.md-code\b/, /\.convo-pre/,
  /\.ime-debug\b/, /\.xterm/, /\.term-host-el\b/, /\.term-tab/, /\.swatch\b/,
  /\.(input|select|textarea)\b/, /\binput\b/, /\bcode\b/, /\.mono\b/, /\bkbd\b/,
  /\.convo-md h[1-6]/, /\.md-table\b/, /\.switch\b/, /\.handle\b/, /\.files-progress\b/,
  /\.convo-runs-bar\b/, /\.convo-rail-tick\b/, /\.attach-/, /\.claude-card-head\b/,
  /\.input-eye/, /\.brand\b/, /\.auth\b/,
  // Spacing measured from something that is not the scale. Each says what.
  /\.complete-item\b/,            // 8.5 and 11.5 against a 13px/1.3 line: rows of exactly 34 and 40
  /\.convo-running-what\b|\.convo-running-more\b/, // 13 above and below an 18px line: 44 to press, pulled back by -13 so the row does not grow
  /\.convo-summary summary\b|\.convo-bar-pick\b/,  // -11 for the same reason, against min-height 44
  /\.convo-ask-opt\b/,            // 11 above and below a 22px line: a choice is 44 to press
  /\.convo-latest\b|\.host-history-row\b/,        // 11 the same, on touch
  /\.convo-user-text\b|\.pick-item\b/,            // 9 and 14 round the words of a bubble and a row, sized to them
  /\.claude-card-body\b/,         // indented 30: the head's mark and its gap, above it
  /\.live-info\b/,                // 44 kept clear on the right for the control that sits over it
  /\.convo-list-item\b/,          // 26 kept clear for the menu button that appears over the row
  /\.convo-side-grip\b/,          // a 9px grip laid over a 1px divider, pulled back by half each side
  /\.list-search\b/,              // 9 and 11 so its words start where the rows' words do (12 there, less its border)
  /\.row-session .row-main\b/,    // indented 38 to clear the mark of the row above it
  // Type that is not a passage to read, and so takes no line from the scale.
  /\.sc-|\.complete-/,            // the completion panel: its rows are a height, set out above
  /\.kl-chip\b|\.reply-key\b|\.convo-ask-key\b|\.herdr-count\b/, // a badge: one glyph centred in a box, line 1
  /\.tab\b/,                      // a strip of tabs, as the terminal's own
  /\.picture-path\b/,             // a path in mono, sized to sit under the picture's name
  /\.convo-ask-label\b/,          // 22: with 11 above and below it, a choice is 44 to press
  /\.convo-queue-row\b/,          // 20: three of these lines are what the row is clamped to
  /\.agent-icon\b/,               // 28%: a squircle, not a corner
  /\.convo-search-text mark\b/,   // 2px behind the words, barely off square
  /\.convo-search-hit\b/,         // 16 on touch, so a hit reads at arm's length
]
const px = (v) => [...v.matchAll(/(-?\d*\.?\d+)px/g)].map((m) => Math.abs(parseFloat(m[1])))
// What a length means here, and not elsewhere: a shadow's blur, a thing moved,
// a width reserved, a breakpoint are all lengths and none of them is spacing.
const strip = (v) => v.replace(/\b(calc|clamp|min|max|env|var)\([^()]*(\([^()]*\))?[^()]*\)/g, ' ')
const offences = []
for (const f of ['app.css', 'terminal.css']) {
  const text = readFileSync(join(web, 'src/styles', f), 'utf8')
  const lines = text.split('\n')
  let sel = ''
  lines.forEach((raw, i) => {
    const line = raw.replace(/\/\*.*?\*\//g, '')
    if (/\{/.test(line)) {
      const h = line.slice(0, line.indexOf('{')).trim()
      if (h && !h.startsWith('@')) sel = h
      else if (h.startsWith('@')) { const r = line.slice(line.indexOf('{') + 1); const m = /([^{}]+)\{/.exec(r); if (m) sel = m[1].trim() }
    }
    if (EXEMPT.some((r) => r.test(sel)) || /^:root/.test(sel)) return
    for (const m of line.matchAll(/(?:^|[;{])\s*([a-z-]+)\s*:\s*([^;{}]+)/g)) {
      const prop = m[1], val = m[2].trim()
      const where = `${f}:${i + 1}: ${sel.slice(0, 48)}`
      if (prop === 'font-size' || prop === 'line-height') {
        if (/\d/.test(strip(val)) && !/^(inherit|normal|0)$/.test(val))
          offences.push(`${where}: ${prop}: ${val} — take the pair for its role (--fs-… with --lh-…), not a number`)
      } else if (prop === 'font' && /\d*\.?\d+(px|em|rem|pt)|\/\s*\d/.test(strip(val))) {
        offences.push(`${where}: font: ${val} — the shorthand sets a size and a line; take the role's pair instead`)
      } else if (prop === 'border-radius') {
        if (strip(val).split(/\s+/).some((p) => /\d/.test(p) && !/^(0|50%|999px)$/.test(p)))
          offences.push(`${where}: border-radius: ${val} — name its step (--r-xs … --r-xl), or a pill`)
      } else if (SPACED.test(prop)) {
        for (const n of px(strip(val))) if (!STEP.has(n)) offences.push(`${where}: ${prop}: ${val} — ${n}px is not a step of the scale`)
      }
    }
  })
}
if (offences.length) {
  console.error(offences.join('\n'))
  console.error(`\n${offences.length} place(s) off the scale. The scale is at the top of app.css; a value measured`)
  console.error('against something else belongs in EXEMPT in this file, with a line saying what it is measured against.')
  process.exit(1)
}
console.log('styles: type and spacing on the scale')
