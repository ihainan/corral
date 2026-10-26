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
const maxLabel = 120

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

// A line between choices: blank, or a rule (Claude Code's questions put
// their last choices under one: "4. Chat about this").
const between = /^[\s│|]*(?:[─━]{4,}[\s─━]*)?$/

// Claude Code's questions (AskUserQuestion) say how to go through them, and
// put a line under each choice that tells of it: there, a choice's further
// lines are that, not the rest of its words wrapped.
const describes = /to navigate/i

export type Choice = { key: string; label: string; desc?: string }

// The menu at the end of the screen's lines: its choices and the line it
// begins on.
function menuAt(lines: string[]): { out: Choice[]; top: number } {
  const told = lines.slice(-6).some((l) => describes.test(l))
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
      if (out.length && !more.length && between.test(lines[i])) {
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
    const label = told ? m[3] : [m[3], ...more].join(' ')
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
  const lines = text.split('\n')
  return menuAt(lines).top >= 0 && lines.slice(-6).some((l) => describes.test(l))
}

export function choices(text: string): Choice[] {
  return menuAt(text.split('\n')).out
}

// The question asked over a menu on the screen: the line above it that
// asks (ends in a question mark), else "".
export function question(text: string): string {
  const lines = text.split('\n')
  const { top } = menuAt(lines)
  for (let i = top - 1, n = 0; top > 0 && i >= 0 && n < 4; i--, n++) {
    const l = lines[i].replace(/^[\s│|╭╮╰╯─━┌┐└┘]+|[\s│|╭╮╰╯─━┌┐└┘]+$/g, '')
    if (!l) continue
    return /[?？]$/.test(l) && l.length <= 200 ? l : ''
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
  for (let i = q - 1; i >= 0 && q - i <= 40; i--) {
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
