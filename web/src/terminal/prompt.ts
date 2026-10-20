import type { Terminal } from '@xterm/xterm'

// Whether a cell of the screen is inside Claude Code's input box: rows
// between two rules of ─ that start in the same column, the first of them
// starting with ❯ there (herdr may draw a pane border after the rules). The
// terminal cursor must be in the box too, shown or not (herdr hides it while
// the agent works), so a box left in the output above does not count. Cells,
// not string offsets: a sidebar with wide characters shifts the text.

const MIN_RULE = 20 // cells of ─
const MAX_ROWS = 12 // input rows looked through, up and down

export function inPromptBox(term: Terminal, col: number, row: number): boolean {
  const buf = term.buffer.active
  if (buf.viewportY !== buf.baseY) return false // scrolled back: not the live screen
  const cell = buf.getNullCell()
  const charAt = (y: number, x: number) => buf.getLine(buf.baseY + y)?.getCell(x, cell)?.getChars() ?? ''
  // The run of ─ through column x on row y, as [start, end), or null.
  const rule = (y: number, x: number): [number, number] | null => {
    if (y < 0 || y >= term.rows || charAt(y, x) !== '─') return null
    let s = x
    let e = x + 1
    while (s > 0 && charAt(y, s - 1) === '─') s--
    while (e < term.cols && charAt(y, e) === '─') e++
    return e - s >= MIN_RULE ? [s, e] : null
  }
  if (row < 0 || row >= term.rows || col < 0 || col >= term.cols || rule(row, col)) return false // (a rule is not the box)
  // The rules above and below the tapped row, found where the column of the
  // tap meets them.
  let top = -1
  let bottom = -1
  for (let y = row - 1; y >= Math.max(0, row - MAX_ROWS); y--) {
    if (rule(y, col)) {
      top = y
      break
    }
  }
  for (let y = row + 1; y <= Math.min(term.rows - 1, row + MAX_ROWS); y++) {
    if (rule(y, col)) {
      bottom = y
      break
    }
  }
  if (top < 0 || bottom < 0 || bottom - top < 2) return false
  const a = rule(top, col)!
  const b = rule(bottom, col)!
  if (a[0] !== b[0] || a[1] !== b[1] || charAt(top + 1, a[0]) !== '❯') return false
  const cy = buf.cursorY
  const cx = Math.min(buf.cursorX, term.cols - 1)
  return cy > top && cy < bottom && cx >= a[0] && cx < a[1]
}
