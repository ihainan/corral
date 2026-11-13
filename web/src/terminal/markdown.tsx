import { createContext, Fragment, useContext, useLayoutEffect, useRef, useState, type ReactNode } from 'react'
import { Code } from './Code'
import { t } from '../i18n'
import { Check } from 'lucide-react'

// A small Markdown renderer for an agent's answers in the conversation view:
// headings, paragraphs, lists (nested by indent), fenced code, quotes, GFM
// tables and rules; inline code, bold, italic, strike-through and links.
// The text comes from the host: it only ever becomes text nodes (never
// HTML), and links are only made for http(s) addresses. A single newline
// inside a paragraph is kept as a line break, as the agent shows it in the
// terminal (joining lines with spaces would also space Chinese apart).

// soft: a paragraph's lines are one line, as Markdown files mean them (a file
// looked at); else each line break is kept, as an agent's words mean theirs.
export function Markdown({ text, onCopyCode, copyLabel = 'Copy', soft = false }: { text: string; onCopyCode?: (code: string) => void; copyLabel?: string; soft?: boolean }) {
  const copy = onCopyCode ? { fn: onCopyCode, label: copyLabel } : undefined
  return <>{blocks(text.replace(/\r\n?/g, '\n').split('\n'), copy, 0, soft)}</>
}

type Copy = { fn: (code: string) => void; label: string } | undefined

// What an inline code span is drawn as, where its page has more to say of
// some (the conversation: a file's path, to be acted on); null: as code.
export const CodeSpanOf = createContext<((text: string) => ReactNode | null) | undefined>(undefined)
function InlineCode({ text }: { text: string }) {
  const draw = useContext(CodeSpanOf)
  return draw?.(text) ?? <code>{breakable(text)}</code>
}
// A long name in code (a test's, a path): may wrap after its _ / . - :, as a
// reader would break it, before it is cut anywhere (see terminal.css).
export function breakable(text: string): ReactNode {
  if (text.length < 24) return text
  const out: ReactNode[] = []
  let at = 0
  for (const m of text.matchAll(/[_/.:-](?=[^_/.:-])/g)) {
    out.push(text.slice(at, m.index + 1), <wbr key={m.index} />)
    at = m.index + 1
  }
  out.push(text.slice(at))
  return out
}
// A link to a file on the host, as Codex writes them ("[a.go:12](/home/me/a.go:12)"):
// its words, as a path to act on where the page can (null: as code, the
// whole path in its title). Never drawn as the markdown it was written in.
export const FileLinkOf = createContext<((label: string, target: string) => ReactNode | null) | undefined>(undefined)
function FileLink({ label, target }: { label: string; target: string }) {
  const draw = useContext(FileLinkOf)
  const words = label.replace(/^`(.*)`$/, '$1') // ("[`a.go`](…)": the words, not their quotes)
  return draw?.(words, target) ?? <code title={target}>{words}</code>
}

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

function blocks(lines: string[], copy: Copy, depth: number, soft = false): ReactNode[] {
  const out: ReactNode[] = []
  let i = 0
  let para: string[] = []
  const flush = () => {
    if (para.length) out.push(<p key={out.length}>{inlineLines(para, soft)}</p>)
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
          {/* (A bar of its own: the button covers none of the code.) */}
          {(copy || lang) && (
            <div className="md-code-head">
              <span className="md-code-lang">{lang}</span>
              {copy && (
                <button type="button" className="md-code-copy" onClick={() => copy.fn(body)} aria-label={lang ? `${copy.label} (${lang})` : copy.label}>
                  {copy.label}
                </button>
              )}
            </div>
          )}
          <CodePre text={body} lang={lang || undefined} />
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
      out.push(<blockquote key={out.length}>{blocks(q, copy, depth + 1, soft)}</blockquote>)
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
            let sub = dedent(it.rest)
            // (Soft: the lines right under an item's first, before a blank
            // or a block of its own, are more of its words, as in a paragraph.)
            let n = 0
            if (soft) while (n < sub.length && sub[n].trim() && !listItem.test(sub[n]) && !rule.test(sub[n]) && !sub[n].includes('|') && !/^\s*(```|~~~|>|#{1,6}\s)/.test(sub[n])) n++
            const words = [it.first, ...sub.slice(0, n)]
            sub = sub.slice(n)
            // (A task, as GitHub writes them: "[ ] " or "[x] " first, a box to see — not to tick.)
            const task = /^\[([ xX])\]\s+/.exec(words[0])
            if (task) words[0] = words[0].slice(task[0].length)
            return (
              <li key={k} className={task ? 'md-task-item' : undefined}>
                {task && (
                  <span className={'md-task' + (task[1] !== ' ' ? ' is-done' : '')} role="img" aria-label={t(task[1] !== ' ' ? 'convo.taskDone' : 'convo.taskTodo')}>
                    {task[1] !== ' ' && <Check size={11} strokeWidth={3} aria-hidden="true" />}
                  </span>
                )}
                {n ? inlineLines(words, true) : inline(words[0])}
                {sub.some((s) => s.trim()) && blocks(sub, copy, depth + 1, soft)}
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
  // (By a loop: Math.min(...lines) of a long list goes past how many arguments a call takes — review.)
  let n = Infinity
  for (const l of lines) if (l.trim()) n = Math.min(n, indentOf(l))
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

// (Soft: where a line ends in an odd run of backslashes — one escaped is no
// mark — or two spaces, the break is kept; the backslash is the mark, not shown.)
const hardEnd = (l: string) => / {2,}$/.test(l) || /(?:^|[^\\])(?:\\\\)*\\$/.test(l)
// Writing that sets no space between its words: Chinese and Japanese (CJK
// ideographs, kana, their punctuation and full-width forms). Not Korean: it
// spaces its words, so its line break is a space, as browsers make it.
const unspaced = /[\u3000-\u9fff\uf900-\ufaff\uff00-\uffef]/
// (Its last seen character, past any closing marks of emphasis or code.)
const lastSeen = (l: string) => l.trimEnd().replace(/[*_~`]+$/, '').slice(-1)
const firstSeen = (l: string) => l.trimStart().replace(/^[*_~`]+/, '').slice(0, 1)

function inlineLines(lines: string[], soft = false): ReactNode {
  return lines.map((l, k) => {
    const brk = k > 0 && (!soft || hardEnd(lines[k - 1]))
    const glued = k > 0 && !brk && unspaced.test(lastSeen(lines[k - 1])) && unspaced.test(firstSeen(l))
    // (Soft, a line marked to break with a backslash: the mark goes.)
    const text = soft && k < lines.length - 1 && !/ {2,}$/.test(l) && hardEnd(l) ? l.replace(/\\$/, '') : l
    return (
      <Fragment key={k}>
        {brk ? <br /> : k > 0 && !glued ? ' ' : null}
        {inline(text.trim())}
      </Fragment>
    )
  })
}

// Inline spans, left to right; the first match at the earliest place wins.
// Text reaches here a line at a time (no newlines). Each pattern is searched
// forward from where the text has got to, and its next match kept, so a long
// line is not searched again from the start for every span on it.
const wide = '\\u3000-\\u303f\\uff00-\\uffef' // full-width punctuation ends a bare address
const spans: { re: RegExp; make: (m: RegExpExecArray, key: number) => ReactNode }[] = [
  { re: /`([^`]+)`/g, make: (m, k) => <InlineCode key={k} text={m[1]} /> },
  // (A mark written to be shown as itself — \* — not as emphasis; the backslash not shown. Not in code: that is matched first.)
  { re: /\\([!-/:-@[-`{-~])/g, make: (m, k) => <Fragment key={k}>{m[1]}</Fragment> },
  // (A line break written as HTML, as in a table's cell: the one tag taken; any other stays text.)
  { re: /<br\s*\/?>/gi, make: (_m, k) => <br key={k} /> },
  // (A picture: never loaded from elsewhere — a link to it, by its words.)
  { re: /!\[([^\]]{0,500})\]\((https?:\/\/[^\s)]{1,2000})\)/g, make: (m, k) => link(m[2], m[1] || m[2], k) },
  // (A link's words are only words: no code span in them is drawn as more — a button in a link opens both.)
  { re: /\[([^\]]{1,500})\]\((https?:\/\/[^\s)]{1,2000})\)/g, make: (m, k) => link(m[2], <CodeSpanOf.Provider value={undefined}>{inline(m[1])}</CodeSpanOf.Provider>, k) },
  // (Its words may hold one level of [brackets] — a route's [id].ts; its path one level of (parens).)
  { re: /\[((?:[^[\]\n]|\[[^[\]\n]{0,200}\]){1,500})\]\((\/(?!\/)(?:[^\s()]|\([^\s()]{0,200}\)){1,2000})\)/g, make: (m, k) => <FileLink key={k} label={m[1]} target={m[2]} /> },
  { re: new RegExp(`https?://[^\\s<>()${wide}"']{0,2000}[^\\s<>()${wide}"'.,;:!?]`, 'g'), make: (m, k) => link(m[0], m[0], k) },
  // (Its last a code span whole, or a mark that is neither a space nor a backtick: **`SKILL.md`** bold too — 11-13,
  // before, a bold ending in code kept its stars.)
  { re: /\*\*\*(?=[^\s*])([^*]{0,1000}?[^\s*])\*\*\*/g, make: (m, k) => <strong key={k}><em>{inline(m[1])}</em></strong> },
  { re: /\*\*(?=\S)((?:`[^`]{0,500}`|[^`]){0,1000}?(?:`[^`]{0,500}`|[^`\s]))\*\*/g, make: (m, k) => <strong key={k}>{inline(m[1])}</strong> },
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

// A code block's lines: where they run past its right edge, that edge
// fades, so a line cut there is seen to go on (it scrolls sideways), not
// taken for its end.
function CodePre({ text, lang }: { text: string; lang?: string }) {
  const ref = useRef<HTMLPreElement>(null)
  const [more, setMore] = useState(false)
  useLayoutEffect(() => {
    const el = ref.current
    if (!el) return
    const look = () => setMore(el.scrollWidth - el.scrollLeft - el.clientWidth > 2)
    look()
    el.addEventListener('scroll', look, { passive: true })
    const ro = new ResizeObserver(look)
    ro.observe(el)
    return () => {
      el.removeEventListener('scroll', look)
      ro.disconnect()
    }
  }, [text])
  return (
    <pre ref={ref} className={more ? 'is-more' : undefined}>
      <Code text={text} lang={lang} />
    </pre>
  )
}
