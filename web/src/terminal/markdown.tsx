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
  return <>{blocks(text.replace(/\r\n?/g, '\n').split('\n'), copy)}</>
}

type Copy = { fn: (code: string) => void; label: string } | undefined

const fence = /^\s{0,3}(`{3,}|~{3,})\s*([^`\s]*)/
const heading = /^\s{0,3}(#{1,6})\s+(.*?)\s*#*\s*$/
const rule = /^\s{0,3}([-*_])(\s*\1){2,}\s*$/
const quote = /^\s{0,3}>\s?/
const listItem = /^(\s*)([-*+]|\d{1,9}[.)])\s+(.*)$/
const tableSep = /^\s*\|?\s*:?-{1,}:?\s*(\|\s*:?-{1,}:?\s*)*\|?\s*$/

function indentOf(s: string) {
  const m = /^\s*/.exec(s)![0]
  return m.replace(/\t/g, '    ').length
}

function blocks(lines: string[], copy: Copy): ReactNode[] {
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
      out.push(<H key={out.length}>{inline(m[2])}</H>)
      i++
      continue
    }
    if (rule.test(line) && !listItem.test(line)) {
      flush()
      out.push(<hr key={out.length} />)
      i++
      continue
    }
    if (quote.test(line)) {
      flush()
      const q: string[] = []
      while (i < lines.length && quote.test(lines[i])) q.push(lines[i++].replace(quote, ''))
      out.push(<blockquote key={out.length}>{blocks(q, copy)}</blockquote>)
      continue
    }
    if (line.includes('|') && i + 1 < lines.length && tableSep.test(lines[i + 1]) && lines[i + 1].includes('-')) {
      flush()
      const head = cells(line)
      const align = cells(lines[i + 1]).map((c) => (c.startsWith(':') && c.endsWith(':') ? 'center' : c.endsWith(':') ? 'right' : undefined))
      i += 2
      const rows: string[][] = []
      while (i < lines.length && lines[i].includes('|') && lines[i].trim()) rows.push(cells(lines[i++]))
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
    if ((m = listItem.exec(line))) {
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
                {sub.some((s) => s.trim()) && blocks(sub, copy)}
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
const spans: { re: RegExp; make: (m: RegExpExecArray, key: number) => ReactNode }[] = [
  { re: /`([^`\n]+)`/, make: (m, k) => <code key={k}>{m[1]}</code> },
  { re: /\[([^\]\n]+)\]\((https?:\/\/[^\s)]+)\)/, make: (m, k) => link(m[2], inline(m[1]), k) },
  { re: /https?:\/\/[^\s<>()　-〿＀-￯"']+[^\s<>()　-〿＀-￯"'.,;:!?]/, make: (m, k) => link(m[0], m[0], k) },
  { re: /\*\*(?=\S)([^\n]*?\S)\*\*/, make: (m, k) => <strong key={k}>{inline(m[1])}</strong> },
  { re: /~~(?=\S)([^\n]*?\S)~~/, make: (m, k) => <del key={k}>{inline(m[1])}</del> },
  { re: /(?<![*\w])\*(?=\S)([^*\n]*?\S)\*(?![*\w])/, make: (m, k) => <em key={k}>{inline(m[1])}</em> },
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
  let rest = s
  while (rest) {
    let best: { at: number; m: RegExpExecArray; make: (m: RegExpExecArray, key: number) => ReactNode } | null = null
    for (const sp of spans) {
      const m = sp.re.exec(rest)
      if (m && (!best || m.index < best.at)) best = { at: m.index, m, make: sp.make }
    }
    if (!best) {
      out.push(rest)
      break
    }
    if (best.at > 0) out.push(rest.slice(0, best.at))
    out.push(best.make(best.m, out.length))
    rest = rest.slice(best.at + best.m[0].length)
  }
  return out
}
