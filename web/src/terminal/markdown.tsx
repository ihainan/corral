import { Fragment, type ReactNode } from 'react'

// A small Markdown renderer for an agent's answers in the conversation view:
// headings, paragraphs, lists (nested by indent), fenced code, quotes, GFM
// tables and rules; inline code, bold, italic, strike-through and links.
// The text comes from the host: it only ever becomes text nodes (never
// HTML), and links are only made for http(s) addresses. A single newline
// inside a paragraph is kept as a line break, as the agent shows it in the
// terminal (joining lines with spaces would also space Chinese apart).

export function Markdown({ text, onCopyCode, copyLabel = 'Copy' }: { text: string; onCopyCode?: (code: string) => void; copyLabel?: string }) {
  const copy = onCopyCode ? { fn: onCopyCode, label: copyLabel } : undefined
  return <>{blocks(text.replace(/\r\n?/g, '\n').split('\n'), copy, 0)}</>
}

type Copy = { fn: (code: string) => void; label: string } | undefined

const fence = /^\s{0,3}(`{3,}|~{3,})\s*([^`\s]*)/
const heading = /^ {0,3}(#{1,6})[ \t]+(.*)$/
// Quotes and lists nest this deep at most; deeper is shown as text.
const maxDepth = 8
// A table shows at most this many columns and cells; the rows past that are
// shown as text (a few bytes a row could otherwise make millions of cells).
const maxCols = 32
const maxCells = 4000
const rule = /^\s{0,3}([-*_])(\s*\1){2,}\s*$/
const quote = /^\s{0,3}>\s?/
const listItem = /^(\s*)([-*+]|\d{1,9}[.)])\s+(.*)$/

// A table's line under its header ("| --- | :-: |"), checked cell by cell (a
// single pattern for it backtracks badly on a long run of spaces).
function isTableSep(line: string): boolean {
  let t = line.trim()
  if (t.startsWith('|')) t = t.slice(1)
  if (t.endsWith('|')) t = t.slice(0, -1)
  const cells = t.split('|')
  return cells.length > 0 && cells.every((c) => /^:?-+:?$/.test(c.trim()))
}

function indentOf(s: string) {
  const m = /^\s*/.exec(s)![0]
  return m.replace(/\t/g, '    ').length
}

// A heading's closing hashes ("## Title ##"), found by scanning: a pattern
// for them backtracks badly on a long run of spaces.
function headingText(s: string): string {
  let end = s.length
  while (end > 0 && (s[end - 1] === ' ' || s[end - 1] === '\t')) end--
  let h = end
  while (h > 0 && s[h - 1] === '#') h--
  if (h < end && (h === 0 || s[h - 1] === ' ' || s[h - 1] === '\t')) end = h
  while (end > 0 && (s[end - 1] === ' ' || s[end - 1] === '\t')) end--
  return s.slice(0, end)
}

function blocks(lines: string[], copy: Copy, depth: number): ReactNode[] {
  const out: ReactNode[] = []
  let i = 0
  let para: string[] = []
  const flush = () => {
    if (para.length) out.push(<p key={out.length}>{inlineLines(para)}</p>)
    para = []
  }
  while (i < lines.length) {
    const line = lines[i]
    let m: RegExpExecArray | null
    if ((m = fence.exec(line))) {
      flush()
      const mark = m[1]
      const lang = m[2].slice(0, 32)
      const code: string[] = []
      i++
      while (i < lines.length && !lines[i].trimStart().startsWith(mark)) code.push(lines[i++])
      i++
      const body = code.join('\n')
      out.push(
        <div className="md-code" key={out.length}>
          {copy && (
            <button type="button" className="md-code-copy" onClick={() => copy.fn(body)} aria-label={lang ? `${copy.label} (${lang})` : copy.label}>
              {copy.label}
            </button>
          )}
          <pre>
            <code>{body}</code>
          </pre>
        </div>,
      )
      continue
    }
    if (!line.trim()) {
      flush()
      i++
      continue
    }
    if ((m = heading.exec(line))) {
      flush()
      const level = Math.min(m[1].length + 2, 6)
      const H = `h${level}` as 'h3'
      out.push(<H key={out.length}>{inline(headingText(m[2]))}</H>)
      i++
      continue
    }
    if (rule.test(line) && !listItem.test(line)) {
      flush()
      out.push(<hr key={out.length} />)
      i++
      continue
    }
    if (quote.test(line) && depth < maxDepth) {
      flush()
      const q: string[] = []
      while (i < lines.length && quote.test(lines[i])) q.push(lines[i++].replace(quote, ''))
      out.push(<blockquote key={out.length}>{blocks(q, copy, depth + 1)}</blockquote>)
      continue
    }
    if (line.includes('|') && i + 1 < lines.length && isTableSep(lines[i + 1])) {
      flush()
      const head = cells(line).slice(0, maxCols)
      const align = cells(lines[i + 1]).slice(0, maxCols).map((c) => (c.startsWith(':') && c.endsWith(':') ? 'center' : c.endsWith(':') ? 'right' : undefined))
      i += 2
      const rows: string[][] = []
      const maxRows = Math.floor(maxCells / head.length) - 1
      while (i < lines.length && rows.length < maxRows && lines[i].includes('|') && lines[i].trim()) rows.push(cells(lines[i++]).slice(0, maxCols))
      out.push(
        <div className="md-table" key={out.length}>
          <table>
            <thead>
              <tr>{head.map((c, j) => <th key={j} style={{ textAlign: align[j] }}>{inline(c)}</th>)}</tr>
            </thead>
            <tbody>
              {rows.map((r, k) => (
                <tr key={k}>{head.map((_, j) => <td key={j} style={{ textAlign: align[j] }}>{inline(r[j] ?? '')}</td>)}</tr>
              ))}
            </tbody>
          </table>
        </div>,
      )
      continue
    }
    if (depth < maxDepth && (m = listItem.exec(line))) {
      flush()
      const base = indentOf(line)
      const ordered = /\d/.test(m[2])
      const items: { first: string; rest: string[]; start?: number }[] = []
      while (i < lines.length) {
        const l = lines[i]
        const lm = listItem.exec(l)
        if (lm && indentOf(l) <= base + 1) {
          if (/\d/.test(lm[2]) !== ordered) break
          items.push({ first: lm[3], rest: [], start: ordered ? parseInt(lm[2], 10) : undefined })
          i++
          continue
        }
        if (!l.trim()) {
          // A blank line ends the list unless an indented line or another item follows.
          const next = lines[i + 1]
          if (next === undefined || (!(indentOf(next) > base) && !(listItem.test(next) && indentOf(next) <= base + 1))) break
          items[items.length - 1].rest.push('')
          i++
          continue
        }
        if (indentOf(l) > base) {
          items[items.length - 1].rest.push(l)
          i++
          continue
        }
        break
      }
      const List = ordered ? 'ol' : 'ul'
      out.push(
        <List key={out.length} start={ordered && items[0].start !== 1 ? items[0].start : undefined}>
          {items.map((it, k) => {
            const sub = dedent(it.rest)
            return (
              <li key={k}>
                {inline(it.first)}
                {sub.some((s) => s.trim()) && blocks(sub, copy, depth + 1)}
              </li>
            )
          })}
        </List>,
      )
      continue
    }
    para.push(line)
    i++
  }
  flush()
  return out
}

function dedent(lines: string[]): string[] {
  const n = Math.min(...lines.filter((l) => l.trim()).map(indentOf))
  if (!isFinite(n)) return lines
  return lines.map((l) => l.replace(/\t/g, '    ').slice(n))
}

function cells(line: string): string[] {
  let s = line.trim()
  if (s.startsWith('|')) s = s.slice(1)
  if (s.endsWith('|') && !s.endsWith('\\|')) s = s.slice(0, -1)
  const out: string[] = []
  let cur = ''
  let code = false
  for (let k = 0; k < s.length; k++) {
    const c = s[k]
    if (c === '\\' && s[k + 1] === '|') {
      cur += '|'
      k++
    } else if (c === '`') {
      code = !code
      cur += c
    } else if (c === '|' && !code) {
      out.push(cur.trim())
      cur = ''
    } else cur += c
  }
  out.push(cur.trim())
  return out
}

function inlineLines(lines: string[]): ReactNode {
  return lines.map((l, k) => (
    <Fragment key={k}>
      {k > 0 && <br />}
      {inline(l.trim())}
    </Fragment>
  ))
}

// Inline spans, left to right; the first match at the earliest place wins.
// Text reaches here a line at a time (no newlines). Each pattern is searched
// forward from where the text has got to, and its next match kept, so a long
// line is not searched again from the start for every span on it.
const wide = '\\u3000-\\u303f\\uff00-\\uffef' // full-width punctuation ends a bare address
const spans: { re: RegExp; make: (m: RegExpExecArray, key: number) => ReactNode }[] = [
  { re: /`([^`]+)`/g, make: (m, k) => <code key={k}>{m[1]}</code> },
  { re: /\[([^\]]{1,500})\]\((https?:\/\/[^\s)]{1,2000})\)/g, make: (m, k) => link(m[2], inline(m[1]), k) },
  { re: new RegExp(`https?://[^\\s<>()${wide}"']{0,2000}[^\\s<>()${wide}"'.,;:!?]`, 'g'), make: (m, k) => link(m[0], m[0], k) },
  { re: /\*\*(?=\S)((?:`[^`]{0,500}`|[^`]){0,1000}?\S)\*\*/g, make: (m, k) => <strong key={k}>{inline(m[1])}</strong> },
  { re: /~~(?=\S)([^~]{0,2000}?\S)~~/g, make: (m, k) => <del key={k}>{inline(m[1])}</del> },
  { re: /(?<![*\w])\*(?=\S)([^*]{0,2000}?\S)\*(?![*\w])/g, make: (m, k) => <em key={k}>{inline(m[1])}</em> },
]

function link(href: string, children: ReactNode, key: number) {
  return (
    <a key={key} href={href} target="_blank" rel="noopener noreferrer">
      {children}
    </a>
  )
}

function inline(s: string): ReactNode[] {
  const out: ReactNode[] = []
  const next: (RegExpExecArray | null | undefined)[] = spans.map(() => undefined) // undefined: not searched yet
  let pos = 0
  while (pos < s.length) {
    let best = -1
    for (let k = 0; k < spans.length; k++) {
      let m = next[k]
      if (m === undefined || (m && m.index < pos)) {
        const re = spans[k].re
        re.lastIndex = pos
        m = next[k] = re.exec(s)
      }
      if (m && (best < 0 || m.index < next[best]!.index)) best = k
    }
    if (best < 0) break
    const m = next[best]!
    if (m.index > pos) out.push(s.slice(pos, m.index))
    out.push(spans[best].make(m, out.length))
    pos = m.index + m[0].length
  }
  if (pos < s.length) out.push(s.slice(pos))
  return out
}
