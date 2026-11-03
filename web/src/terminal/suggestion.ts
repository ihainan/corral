import type { IBufferCell, IBufferLine, Terminal } from '@xterm/xterm'

// Claude Code suggests the next prompt in its input box once a turn ends:
// shown as greyed text after "❯ " (its first character inverse, being where
// the cursor is), taken with Tab. The composer offers the same text. It is
// read off the live screen (not the scrollback), as drawn by Claude Code or
// by herdr around it: the box is a row with "❯ " between two rows of "─",
// the lowest such, with at most a few rows drawn under it in its columns (its
// status lines; not a menu, nor another prompt: then that box is an old one),
// as convo_suggest.go reads a pane. (Not near the screen's foot: Claude Code
// draws in the screen's flow, so with little above it on a tall screen the
// box is high and the rows under it blank.) The text counts only when every character of it is greyed
// (dim, or a grey colour), so what the user typed (in the default or a bright
// colour) never does. Claude Code's own example ('Try "…"', in an empty box)
// is not a suggestion. Everything is in cell columns, as the screen is.

const FOOT = 10 // rows drawn under the box at most (convo_suggest.go's typedFoot)
const EDGES = '│▕▐┃' // borders herdr draws at a pane's edge
const RULE_MIN = 8 // cells of "─" over and under the prompt's mark

export function promptSuggestion(term: Terminal): string | null {
  const buf = term.buffer.active
  if (buf.viewportY !== buf.baseY) return null // (scrolled back: not what is there now)
  const rows = term.rows
  const line = (y: number) => buf.getLine(buf.baseY + y)
  const chars = (l: IBufferLine | undefined, x: number) => l?.getCell(x)?.getChars() ?? ''
  const ruled = (l: IBufferLine | undefined, x: number) => {
    for (let i = 0; i < RULE_MIN; i++) if (chars(l, x + i) !== '─') return false
    return true
  }
  // A row from column x up to a pane's edge (not a sidebar beside it).
  const from = (l: IBufferLine | undefined, x: number) => {
    let s = ''
    for (let i = x; i < term.cols; i++) {
      const ch = chars(l, i)
      if (ch && EDGES.includes(ch)) break
      s += ch || ' '
    }
    return s
  }
  // Under the box at row y: its status lines only.
  const current = (y: number, x: number) => {
    let n = 0
    let blank = 0
    for (let j = y + 2; j < rows; j++) {
      const s = from(line(j), x)
      if (!s.trim()) {
        blank++
        continue
      }
      // (A ❯ on a row of its agents' list is where one was last clicked, or
      // the list's cursor: still that box.)
      if ((s.includes('❯') && !/^\s*❯\s+[◯●] /.test(s)) || /^\s*\d+\.\s/.test(s)) return false
      n += blank + 1
      blank = 0
      if (n > FOOT) return false
    }
    return true
  }
  for (let y = rows - 2; y >= 1; y--) {
    const l = line(y)
    if (!l) continue
    // Every mark on the row (a sidebar next to the box may have one too).
    for (let x = 0; x < term.cols - 1; x++) {
      if (chars(l, x) !== '❯' || !/^[  ]$/.test(chars(l, x + 1))) continue
      if (!ruled(line(y - 1), x) || !ruled(line(y + 1), x)) continue
      return current(y, x) ? greyedAfter(term, l, x + 2) : null
    }
  }
  return null
}

// The greyed text from column `from` of a line, or null.
function greyedAfter(term: Terminal, l: IBufferLine, from: number): string | null {
  let out = ''
  let first = true
  for (let x = from; x < term.cols; x++) {
    const c = l.getCell(x)
    if (!c || c.getWidth() === 0) continue // (the second cell of a wide character)
    const ch = c.getChars()
    if (ch === '' || ch === ' ' || ch === ' ') {
      out += ' '
      continue
    }
    // A border herdr draws at the pane's edge ends it.
    if ('│▕▐┃'.includes(ch)) break
    // (The first is where the cursor is: inverse, or, as herdr draws it, in a block of colour.)
    if (!(first && (c.isInverse() || !c.isBgDefault())) && !greyed(c)) return null
    first = false
    out += ch
  }
  const s = out.trim() // (spaces inside are the suggestion's own)
  // (Nor the box's placeholder while a subagent's conversation, or the
  // background sessions' view, is the one shown: "Message @…", "describe a
  // task for a new session" — agent-view.md.)
  if (!s || /^Try "/.test(s) || /^Message @/.test(s) || s === 'describe a task for a new session') return null
  return s
}

function greyed(c: IBufferCell): boolean {
  if (c.isDim()) return true
  if (c.isFgRGB()) {
    const v = c.getFgColor()
    const r = (v >> 16) & 255
    const g = (v >> 8) & 255
    const b = v & 255
    const lum = (r + g + b) / 3
    return Math.max(r, g, b) - Math.min(r, g, b) <= 24 && lum >= 80 && lum <= 190
  }
  // The palette's greys: bright black, and the 256 colours' grey ramp (not
  // white or black).
  if (c.isFgPalette()) {
    const i = c.getFgColor()
    return i === 8 || (i >= 239 && i <= 248)
  }
  return false
}
