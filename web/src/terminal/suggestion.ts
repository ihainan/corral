import type { IBufferCell, IBufferLine, Terminal } from '@xterm/xterm'

// Claude Code suggests the next prompt in its input box once a turn ends:
// shown as greyed text after "❯ " (its first character inverse, being where
// the cursor is), taken with Tab. The composer offers the same text. It is
// read off the live screen (not the scrollback), as drawn by Claude Code or
// by herdr around it: the box is a row with "❯ " between two rows of "─",
// the lowest such in its pane, with only its own few lines drawn under it
// (not a menu, nor another prompt: then that box is an old one), as
// convo_suggest.go reads a pane. (Not near the screen's foot: Claude Code
// draws in the screen's flow, so with little above it on a tall screen the
// box is high and the rows under it blank.) The text counts only when every character of it is greyed
// (dim, or a grey colour), so what the user typed (in the default or a bright
// colour) never does. Claude Code's own example ('Try "…"', in an empty box)
// is not a suggestion. Everything is in cell columns, as the screen is.

const FOOT = 10 // rows drawn under the box at most (convo_suggest.go's typedFoot)
const EDGES = '│▕▐┃' // borders herdr draws at a pane's edge
// A row of Claude Code's agents' list, and main's (convo_view.go's ownerAgentRow, ownerMainRow).
const AGENT_ROW = /^\s*(?:❯\s+)?[◯●] \S+(?:\s\s+.*|\s*)$/
const MAIN_ROW = /^\s*(?:❯\s+)?[◯●] main\s*$/
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
  // Under the box at row y (in its columns): its own lines only, at most a
  // few, each set in from the pane's edge as Claude Code draws them (a row
  // from the edge is a shell's prompt: the box an old one), none a prompt's
  // ❯ or a numbered choice. A ❯ on a row of its agents' list is where one
  // was clicked, not the list's cursor, only with the box's own mode line
  // over the list, as convo_view.go reads it.
  const current = (y: number, x: number) => {
    const foot: string[] = []
    const wrapped: boolean[] = [] // (the rest of a line too long for the screen: from the edge)
    for (let j = y + 2; j < rows; j++) {
      foot.push(from(line(j), x))
      wrapped.push(!!line(j)?.isWrapped)
    }
    while (foot.length && !foot[foot.length - 1].trim()) foot.pop()
    const agents = foot.map(() => false)
    for (let k = 0; k < foot.length; ) {
      if (!AGENT_ROW.test(foot[k])) {
        k++
        continue
      }
      let e = k
      let main = false
      for (; e < foot.length && AGENT_ROW.test(foot[e]); e++) main ||= MAIN_ROW.test(foot[e])
      for (; k < e; k++) agents[k] = main
    }
    // (Its mode line whole, where a narrow screen wrapped it.)
    let end = -1
    for (let k = 0; k < foot.length && !agents[k]; k++) if (foot[k].trim()) end = k
    let start = end
    while (start > 0 && wrapped[start]) start--
    const keys = end < 0 ? '' : foot.slice(start, end + 1).join('')
    const clicked = /^\s*[⏵⏸]/.test(keys) || /^\?forshortcuts/.test(keys.replace(/\s+/g, ''))
    // (The list's rows are not counted: many subagents are no dialog. They
    // are set in as the rest, but for a clicked one's ❯, drawn from the edge.)
    if (agents.filter((a) => !a).length > FOOT) return false
    return foot.every(
      (s, k) =>
        !s.trim() ||
        (agents[k]
          ? /^\s/.test(s)
            ? !s.includes('❯') || clicked
            : s.startsWith('❯') && clicked
          : (/^\s/.test(s) || wrapped[k]) && !s.includes('❯') && !/^\s*\d+\.\s/.test(s)),
    )
  }
  // The lowest box decides for its pane (the column it starts at): one
  // higher up there is an old one. Another pane's box is looked at too.
  const decided = new Set<number>()
  for (let y = rows - 2; y >= 1; y--) {
    const l = line(y)
    if (!l) continue
    // Every mark on the row (a sidebar next to the box may have one too).
    for (let x = 0; x < term.cols - 1; x++) {
      if (chars(l, x) !== '❯' || !/^[ \u00a0]$/.test(chars(l, x + 1))) continue
      if (!ruled(line(y - 1), x) || !ruled(line(y + 1), x) || decided.has(x)) continue
      decided.add(x)
      const s = current(y, x) ? greyedAfter(term, l, x + 2) : null
      if (s !== null) return s
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
