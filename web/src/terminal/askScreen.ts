// What Claude Code's own dialogs show on its screen, read as they are drawn
// (2.1.312): a question of AskUserQuestion's (one of several, with tabs; with
// previews beside; ticks for a choice of many), the page that reviews the
// answers before they go, a plan put to the person. Read only to be checked
// against what its log says it asked, and where each key would go; anything
// not drawn as known is 'unknown' (the screen itself is shown then).

export interface ScreenOption {
  n: number // its number, the key that moves to it (or picks it)
  label: string
  cursor: boolean // the one the cursor is on (❯)
  ticked?: boolean // (a choice of many) [✔]
}

export type AskScreen =
  | {
      kind: 'question'
      tabs: { label: string; answered: boolean }[] // (none for a lone question)
      question: string
      options: ScreenOption[] // its choices, then "Type something" (own: its number)
      own?: number
      multi: boolean // ticks
      beside: boolean // previews beside: a number only moves the cursor
      submit?: { cursor: boolean } // (a choice of many) the line that goes on
    }
  | { kind: 'review'; answers: { question: string; answer: string }[]; incomplete: boolean; options: ScreenOption[] }
  | { kind: 'plan'; options: ScreenOption[] }
  | { kind: 'unknown' }

const optionLine = /^\s*(❯)?\s*(\d{1,2})\.\s+(?:\[( |✔)\]\s+)?(.*?)\s*$/
const tabLine = /^\s*(?:←\s+)?((?:[☐☒]\s+\S.*?\s*)+)(?:✔\s+Submit\s+→)?\s*$/
const ownLabel = /^Type something\.?$/
const rule = /^\s*[─━╌]{8,}\s*$/

// (A choice's words, without what is drawn beside it: a preview's box.)
const besideAt = (s: string) => {
  const m = /\s{3,}[\u2500-\u257f]/.exec(s)
  return m ? s.slice(0, m.index) : s
}

function lastLines(text: string): string[] {
  const lines = text.replace(/\r/g, '').split('\n')
  while (lines.length && !lines[lines.length - 1].trim()) lines.pop()
  return lines.slice(-60)
}

function options(lines: string[]): ScreenOption[] {
  const out: ScreenOption[] = []
  for (const l of lines) {
    const m = optionLine.exec(besideAt(l))
    if (!m) continue
    out.push({ n: Number(m[2]), label: m[4], cursor: !!m[1], ...(m[3] !== undefined ? { ticked: m[3] === '✔' } : {}) })
  }
  return out
}

// A menu drawn whole: numbered 1, 2, 3… once each, the cursor on one of
// them (or, cursorElsewhere, on none: a question's Submit line).
const whole = (o: ScreenOption[], cursorElsewhere = false) =>
  o.length > 0 && o.every((x, i) => x.n === i + 1) && o.filter((x) => x.cursor).length === (cursorElsewhere ? 0 : 1)

// (Lines after a menu that are its own: hints, its file. Anything else: the
// menu is not the last thing on the screen, so not what a key would answer.)
const hints = [/^\s*shift\+tab to approve with this feedback\s*$/i, /^\s*ctrl\+g to edit in Vim(?: · \S.*)?$/i, /^\s*Esc to cancel(?: · Tab to amend)?\s*$/i]
const tailOK = (rest: string[]) => rest.length <= 3 && rest.every((l) => !l.trim() || /^[\s\u2500-\u257f]+$/.test(l) || hints.some((h) => h.test(l)))

export function readAskScreen(text: string): AskScreen {
  const lines = lastLines(text)
  // (The key hints, last: wrapped on a narrow screen.)
  const hint = lines.findLastIndex((l, i) => /Enter to select/.test(l) && /navigate/.test(l + ' ' + (lines[i + 1] ?? '')))
  // A question: its tabs (or a lone one's header), the question, its choices
  // to the rule above "Chat about this", the key hints last.
  if (hint >= 0 && hint >= lines.length - 3) {
    const top = lines.findLastIndex((l, i) => i < hint && tabLine.test(l))
    if (top < 0) return { kind: 'unknown' }
    const tabs = [...lines[top].matchAll(/([☐☒])\s+(\S.*?)(?=\s{2,}[☐☒✔]|\s*$)/g)].map((m) => ({ label: m[2].trim(), answered: m[1] === '☒' }))
    const body = lines.slice(top + 1, hint)
    const end = body.findIndex((l) => rule.test(l))
    const block = end >= 0 ? body.slice(0, end) : body
    const first = block.findIndex((l) => optionLine.test(besideAt(l)))
    if (first <= 0) return { kind: 'unknown' }
    const question = block
      .slice(0, first)
      .map((l) => l.trim())
      .filter(Boolean)
      .join(' ')
    const opts = options(block.slice(first))
    const sub = block.slice(first).find((l) => /^\s*(❯\s*)?Submit\s*$/.test(l))
    if (!question || !whole(opts, !!sub && /❯/.test(sub))) return { kind: 'unknown' }
    const own = opts.find((o) => ownLabel.test(o.label))?.n
    const multi = opts.some((o) => o.ticked !== undefined)
    const beside = block.slice(first).some((l) => /\s{3,}[\u2500-\u257f]/.test(l)) || block.some((l) => /Notes: press n/.test(l))
    // (Tabs of one question shown as one: a lone question.)
    return { kind: 'question', tabs: tabs.length > 1 ? tabs : [], question, options: opts, ...(own ? { own } : {}), multi, beside, ...(sub ? { submit: { cursor: /❯/.test(sub) } } : {}) }
  }
  const review = lines.findLastIndex((l) => /^\s*Review your answers\s*$/.test(l))
  if (review >= 0) {
    const rest = lines.slice(review + 1)
    const ready = rest.findIndex((l) => /Ready to submit your answers\?/.test(l))
    if (ready < 0) return { kind: 'unknown' }
    const answers: { question: string; answer: string }[] = []
    for (let i = 0; i < ready; i++) {
      const q = /^\s*●\s+(.*\S)\s*$/.exec(rest[i])
      const a = q ? /^\s*→\s+(.*\S)\s*$/.exec(rest[i + 1] ?? '') : null
      if (q && a) answers.push({ question: q[1], answer: a[1] })
    }
    const after = rest.slice(ready + 1)
    const opts = options(after)
    const last = after.findLastIndex((l) => optionLine.test(besideAt(l)))
    if (!whole(opts) || !tailOK(after.slice(last + 1))) return { kind: 'unknown' }
    return { kind: 'review', answers, incomplete: rest.slice(0, ready).some((l) => /not answered all questions/.test(l)), options: opts }
  }
  // (A plan's question, the last one on the screen, its menu last.)
  const asks = lines.findLastIndex((l) => /Would you like to proceed\?|Claude wants to exit plan mode/.test(l))
  if (asks >= 0 && !lines.slice(asks + 1).some((l) => /\?\s*$/.test(l)) && lines.slice(0, asks + 1).some((l) => /Ready to code\?|Exit plan mode\?/.test(l))) {
    const after = lines.slice(asks + 1)
    const opts = options(after)
    const last = after.findLastIndex((l) => optionLine.test(besideAt(l)))
    if (whole(opts) && tailOK(after.slice(last + 1))) return { kind: 'plan', options: opts }
  }
  return { kind: 'unknown' }
}

// The same words, however the screen wrapped or spaced them.
export const sameWords = (a: string, b: string) => a.replace(/\s+/g, ' ').trim() === b.replace(/\s+/g, ' ').trim()
