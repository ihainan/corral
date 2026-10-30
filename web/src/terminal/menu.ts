// The menu an agent asks with, as its screen shows it.

// choices finds the menu an agent asks with at the end of the screen, as
// Claude Code, Codex and Gemini CLI draw it: numbered lines in a row, one of
// them marked as selected ("❯ 1. Yes", "  2. No"), followed by at most a few
// lines of blank space, borders or key hints ("Esc to cancel"). The number
// is the key that picks a choice. A numbered list without the mark
// (ordinary output), or one followed by anything else (a new question), is
// no menu.
const menuLine = /^[\s│|]*([❯›>▶→●◉])?\s*([1-9])[.)]\s+(\S.*?)\s*[│|]?\s*$/
const hintLine = /^[\s│|╭╮╰╯─━┌┐└┘]*$|\b(esc|cancel|confirm)\b/i
const maxLabel = 400 // (cut where shown: what is read of it, to the end)

// A choice too long for the pane's width goes on in lines that begin where
// its text does (a narrow pane, a phone's). A key hint ("Esc to cancel") is
// none of them; a key's name alone may be.
const moreLine = /^[\s│|]{3,}(\S.*?)\s*[│|]?\s*$/
const keyHint = /^[\s│|]*(?:(?:esc|enter|tab|ctrl|shift|space)\b|[↑↓←→]+)(?:\s+to\b|\s*[·•])/i // ("Esc to cancel", "↑↓ to navigate · …")
const choiceHead = /^[\s│|]*(?:[❯›>▶→●◉])?\s*[1-9][.)]\s+/
const lead = (l: string) => /^[\s│|]*/.exec(l)![0].length

// Whether line i goes on with the choice above it.
function goesOn(lines: string[], i: number): boolean {
  if (!moreLine.test(lines[i]) || keyHint.test(lines[i])) return false
  for (let j = i - 1; j >= 0 && i - j <= 4; j--) {
    const head = choiceHead.exec(lines[j])
    if (head && menuLine.test(lines[j])) return lead(lines[i]) === head[0].length
    if (!moreLine.test(lines[j])) return false
  }
  return false
}

// A choice of Codex's wrapped: rejoined with the space the line broke at,
// unless it broke after a hyphen within a word, as Codex wraps ("made-by-"
// "codex.txt": a file's name). (A break at a space before "-C" is a space.
// Not Claude Code's, which breaks at spaces only: "foo-" "bar" is "foo- bar".)
const joinWrapped = (a: string, b: string) => (/[A-Za-z0-9]-$/.test(a) ? a + b : a + ' ' + b)

// A line between choices: a rule (Claude Code's questions put their last
// choices under one: "4. Chat about this"). Only in such a question.
const between = /^[\s│|]*[─━]{4,}[\s─━]*$/

// Claude Code's questions (AskUserQuestion) say how to go through them and
// end with a choice to type an answer or talk it over; they put a line
// under each choice that tells of it: there, a choice's further lines are
// that, not the rest of its words wrapped. A permission prompt has neither.
const describes = /to navigate/i
const ownAnswer = /^(?:Type something\.?|Chat about this)$/

export type Choice = { key: string; label: string; desc?: string }

// The menu at the end of the screen's lines, read as a question of Claude
// Code's when it is one (its hint there, and one of its own choices an
// answer of one's own), else as any other menu.
function menuAt(lines: string[], codex = false): { out: Choice[]; top: number; asks: boolean } {
  if (lines.slice(-6).some((l) => describes.test(l))) {
    const q = readMenu(lines, true)
    if (q.out.some((c) => ownAnswer.test(c.label))) return { ...q, asks: true }
  }
  return { ...readMenu(lines, false, codex), asks: false }
}

// The menu at the end of the screen's lines: its choices and the line it
// begins on (told: read as a question of Claude Code's).
function readMenu(lines: string[], told: boolean, codex = false): { out: Choice[]; top: number } {
  let end = lines.length
  while (end > 0 && !menuLine.test(lines[end - 1]) && hintLine.test(lines[end - 1]) && !goesOn(lines, end - 1) && lines.length - end < 3) end--
  const out: Choice[] = []
  let marked = 0
  let top = -1
  let more: string[] = [] // (lines going on from the choice above them)
  for (let i = end - 1; i >= 0; i--) {
    const m = menuLine.exec(lines[i])
    if (!m) {
      if (goesOn(lines, i)) {
        more.unshift(moreLine.exec(lines[i])![1])
        continue
      }
      // (A blank or a rule with more choices above it: still the menu.)
      if (told && out.length && !more.length && between.test(lines[i])) {
        let j = i - 1
        while (j >= 0 && i - j <= 3 && between.test(lines[j])) j--
        if (j >= 0 && i - j <= 3 && (menuLine.test(lines[j]) || goesOn(lines, j))) {
          i = j + 1
          continue
        }
      }
      break
    }
    const clipped = (s: string) => (s.length > maxLabel ? s.slice(0, maxLabel - 1) + '…' : s)
    const label = told ? m[3] : codex ? [m[3], ...more].reduce(joinWrapped) : [m[3], ...more].join(' ')
    const desc = told && more.length ? clipped(more.join(' ')) : undefined
    more = []
    top = i
    out.unshift({ key: m[2], label: clipped(label), ...(desc ? { desc } : {}) })
    if (m[1]) marked++
  }
  const inOrder = out.every((c, i) => c.key === String(i + 1))
  return out.length >= 2 && inOrder && marked === 1 ? { out, top } : { out: [], top: -1 }
}

// asksQuestion: the menu is a question of Claude Code's (AskUserQuestion),
// its choices all there is to it: no command or edit to be shown first.
export function asksQuestion(text: string): boolean {
  const m = menuAt(text.split('\n'))
  return m.top >= 0 && m.asks
}

// choices: the menu's, as the agent whose screen it is draws it (agent:
// claude, codex…; Codex's own hint lines and wrapping known).
export function choices(text: string, agent?: string): Choice[] {
  return agent === 'codex' ? menuAt(codexLines(text), true).out : menuAt(text.split('\n')).out
}

// codexLines: the screen's lines without what Codex draws under its menu:
// its key hints ("tab to add notes | enter to submit answer | ←/→ to
// navigate questions", "esc to interrupt"; "Press enter to confirm or esc
// to cancel"), wrapped to however many lines the pane's width makes them.
// Anything else under the menu, kept: then there is no menu at the end.
function codexLines(text: string): string[] {
  const lines = text.split('\n')
  let last = -1
  for (let i = lines.length - 1; i >= 0 && lines.length - i <= 12; i--) {
    if (menuLine.test(lines[i])) {
      last = i
      break
    }
  }
  if (last < 0) return lines
  let end = last + 1
  while (end < lines.length && goesOn(lines, end)) end++
  const tail = lines.slice(end).map((l) => l.trim()).filter(Boolean).join(' ')
  const hints = /^(?:(?:press\s+)?(?:tab|enter|esc|space|←\/→|↑\/↓)\b.*?\bto\b[^|]*?(?:\s+or\s+(?:tab|enter|esc)\b[^|]*?)?(?:\s*\|\s*|$))+$/i
  return !tail || hints.test(tail) ? lines.slice(0, end) : lines
}

// The question asked over a menu on the screen: the line above it that
// asks (ends in a question mark), else "".
//
// A question of Claude Code's is its quoted block ("│ …" lines, a blank
// one among them too) whole, its context with it (its end, if very long);
// such a question's line wrapped is taken with the lines it goes on from.
// Another menu's question is its own line and what it goes on from wrapped
// (not a permission's words above it).
export function question(text: string): string {
  const lines = text.split('\n')
  const { top, asks } = menuAt(lines)
  const strip = (l: string) => l.replace(/^[\s│|╭╮╰╯─━┌┐└┘]+|[\s│|╭╮╰╯─━┌┐└┘]+$/g, '')
  for (let i = top - 1, n = 0; top > 0 && i >= 0 && n < 4; i--, n++) {
    const l = strip(lines[i])
    if (!l || (asks && /^[(（].*[)）]$/.test(l))) continue // (a question's "(Choose one)" under it)
    if (!/[?？]$/.test(l)) return ''
    const quoted = asks && /^\s*│/.test(lines[i])
    const parts = [l]
    // (cut: the lines it may take taken, and more of it above them.)
    let j = i - 1
    for (; j >= 0 && i - j <= (quoted ? 24 : 3); j--) {
      if (quoted ? !/^\s*│/.test(lines[j]) : !strip(lines[j]) || dashedRule.test(lines[j])) break
      const above = strip(lines[j])
      // (Another menu's: only what it goes on from, wrapped: a line not
      // ended, the next going on in lower case or in Chinese.)
      if (!asks && (/[.。!！?？:：]$/.test(above) || !/^[a-z\u3000-\u9fff]/.test(parts[0]))) break
      if (above) parts.unshift(above)
    }
    const cut = asks && j >= 0 && i - j > (quoted ? 24 : 3) && (quoted ? /^\s*│/.test(lines[j]) : !!strip(lines[j]))
    const cjk = /[\u3000-\u9fff\uff00-\uffef]/
    const all = parts.reduce((a, b) => (cjk.test(a.slice(-1)) || cjk.test(b[0]) ? a + b : a + ' ' + b))
    if (cut) return '…' + (all.length <= 599 ? all : all.slice(-598))
    return all.length <= 600 ? all : '…' + all.slice(-599)
  }
  return ''
}

// What a permission prompt asks to be allowed, as the screen shows it: the
// lines between the rule Claude Code draws over the prompt and its question,
// its head ("Bash command", "Edit file") first. Only the screen says what is
// being approved (the agent's log may be behind it, or of another call), and
// it is kept as it is: the dashed rules too (dimmed when shown), with only
// the hint over the content ("Tip: ...", before the first dashed rule) and
// the line under it ("This command requires approval", after the last) left
// out. Null when there is no menu, no rule over its question near enough, or
// what follows the rule is no head: then the screen itself is to be shown.
const solidRule = /^[\s─━]*[─━]{8,}[\s─━]*$/
export const dashedRule = /^[\s╌┄┈]*[╌┄┈]{8,}[\s╌┄┈]*$/
// The heads Claude Code puts on its permission prompts (any other line after
// a rule may be a command's own).
const promptHead = /^(?:Bash command|Edit file|Create file|Write file|Overwrite file|Read file|Edit notebook|Fetch|Web fetch|Web search)$/i
export function promptBlock(text: string): { head: string; lines: string[] } | null {
  const lines = text.split('\n')
  const { top } = menuAt(lines)
  if (top < 0) return null
  let q = top - 1
  while (q >= 0 && !lines[q].trim()) q--
  if (q < 1 || !/[?？]\s*$/.test(lines[q])) return null
  let start = -1
  for (let i = q - 1; i >= 0 && q - i <= 200; i--) {
    if (solidRule.test(lines[i])) {
      start = i + 1
      break
    }
  }
  if (start < 0) return null
  let body = lines.slice(start, q).map((l) => l.replace(/\s+$/, ''))
  while (body.length && !body[0].trim()) body.shift()
  while (body.length && !body[body.length - 1].trim()) body.pop()
  if (body.length < 2 || !promptHead.test(body[0].trim())) return null
  const firstRule = body.findIndex((l) => dashedRule.test(l))
  let lastRule = -1
  body.forEach((l, i) => dashedRule.test(l) && (lastRule = i))
  body = body.filter((l, i) => !(firstRule > 0 && i < firstRule && /^\s*Tip:/.test(l)) && !(lastRule >= 0 && i > lastRule && /^\s*This command requires approval\s*$/.test(l)))
  const words = body.filter((l) => l.trim() && !dashedRule.test(l))
  const indent = Math.min(...words.map((l) => /^ */.exec(l)![0].length))
  body = body.map((l) => (dashedRule.test(l) ? l.trim() : l.slice(indent)))
  return { head: body[0].trim(), lines: body.slice(1) }
}

// What Codex (0.164) asks over the menu at the end of the screen: a command
// to be run or edits to be made (its question, then what it says of them
// down to the menu: Reason, $ command; Description, Destination — the
// edits themselves are above, in the conversation), or a question of
// request_user_input's ("Question i/n", its words). Looked for only within
// the menu's own block: a line of the conversation above it ("• …", the
// person's "› …") ends the search. Null: not told apart.
export type CodexAsk = { kind: 'run' | 'edits'; question: string; lines: string[] } | { kind: 'question'; question: string; index: number; total: number }
export function codexAsk(text: string): CodexAsk | null {
  const lines = codexLines(text)
  const { top } = menuAt(lines, true)
  if (top < 0) return null
  const body = (from: number) => {
    const out = lines.slice(from, top).map((l) => l.replace(/\s+$/, ''))
    while (out.length && !out[0].trim()) out.shift()
    while (out.length && !out[out.length - 1].trim()) out.pop()
    return out.filter((l, i) => l.trim() || out[i - 1]?.trim()) // (one blank line between, at most)
  }
  // (The topmost such line of the block: the same words further down are a
  // command's or a file's own, under its title.)
  let found: CodexAsk | null = null
  for (let i = top - 1; i >= 0 && top - i <= 40; i--) {
    if (/^[•■›]\s/.test(lines[i])) break
    const l = lines[i].trim()
    const m = /^Would you like to (run the following command|make the following edits)\?$/.exec(l)
    if (m) {
      const run = m[1].startsWith('run')
      const said = body(i + 1).map((x) => x.replace(/^ {2}/, ''))
      // (What Codex says under it: its environment, reason, command; or the edits' description, destination.)
      const first = said.find((x) => x.trim()) ?? ''
      if (!first || (run ? /^(?:Environment|Reason|Permission rule):|^\$ /.test(first) : /^(?:Description|Destination|Reason):/.test(first))) found = { kind: run ? 'run' : 'edits', question: l, lines: said }
      continue
    }
    const q = /^Questions? (\d+)\/(\d+)\b/.exec(l)
    if (q) {
      const words = body(i + 1).map((x) => x.trim()).filter(Boolean).join(' ')
      if (words) found = { kind: 'question', question: words, index: +q[1], total: +q[2] }
    }
  }
  return found
}

// A Codex question's choices: each one's label and description, drawn in
// two columns, the descriptions all at one column (the longest label's end
// and two spaces): the column where every choice with two parts has a run
// of spaces ending, else each split at its first run of two or more.
export function codexOptions(labels: string[]): { label: string; desc?: string }[] {
  const ends = (s: string) => new Set([...s.matchAll(/\S(\s{2,})(?=\S)/g)].map((m) => m.index! + 1 + m[1].length))
  const two = labels.map(ends).filter((e) => e.size)
  let col = -1
  if (two.length > 1) for (const c of two[0]) if (two.every((e) => e.has(c))) col = Math.max(col, c)
  return labels.map((s) => {
    if (col > 0 && ends(s).has(col)) return { label: s.slice(0, col).trimEnd(), desc: s.slice(col) }
    const m = /^(.*?\S)\s{2,}(\S.*)$/.exec(s)
    return m ? { label: m[1], desc: m[2] } : { label: s }
  })
}
