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
  cut?: boolean // (previews beside) its words ran to its column's edge: more of it may be on the next line
  under?: string // (previews beside) what its column has under it, to the next choice: the rest of a long label
  ticked?: boolean // (a choice of many) [✔]
  desc?: string // (nothing beside) the lines drawn under it, indented: its description
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
const multiTabLine = /^\s*←\s+(?:[☐☒☑]\s+\S.*?\s+)+[✔✓]\s+Submit(?:\s+→)?\s*$/
const submitTabLine = /^\s*(?:←\s+)?(?:[☐☒]\s+\S.*?\s*)+[✔✓]\s+Submit(?:\s+→)?\s*$/
const loneTabLine = /^\s*[☐☒]\s+\S.*$/
const isMultiTabLine = (l: string) => multiTabLine.test(l) || submitTabLine.test(l)
const isTabLine = (l: string) => isMultiTabLine(l) || loneTabLine.test(l)

// The dialog's own tab bar, above the hint. Claude Code draws a rule right
// over it, a lone question's "☐ Header" too (seen on 2.1.312 and 2.1.330),
// and a question's words may have tab-like lines ("☐ Billing page", even
// "☐ Billing  ✔ Submit", a choice's description "←  ☐ A  ✔ Submit  →"): so
// the latest tab line under a rule — unless that one's dialog ended (a rule,
// a review page, a line all key hints) before a later whole bar of several,
// "←  ☐ A  ☐ B  ✔ Submit  →": then that bar, a newer dialog's. With no tab
// line under a rule (the screen cut above it), the latest such whole bar,
// unless a dialog ends between it and the latest tab line (those, or a
// choice): then that line, an earlier dialog's bar above it (review E, F, G,
// H; 11-24).
function findTabTop(lines: string[], hint: number): number {
  const ruled = (i: number) => {
    for (let j = i - 1; j >= 0; j--) if (lines[j].trim()) return rule.test(lines[j])
    return false
  }
  const latest = (ok: (l: string, i: number) => boolean) => lines.findLastIndex((l, i) => i < hint && ok(l, i))
  const endsBetween = (a: number, b: number, choices: boolean) =>
    lines.slice(a + 1, b).some((l) => rule.test(l) || /^\s*Review your answers\s*$/.test(l) || hintLine(l) || (choices && optionLine.test(besideAt(l))))
  const under = latest((l, i) => isTabLine(l) && ruled(i))
  const bar = latest((l) => multiTabLine.test(l))
  if (under >= 0) return bar > under && endsBetween(under, bar, false) ? bar : under
  const top = latest((l) => isTabLine(l))
  if (bar < 0 || bar === top) return top
  return endsBetween(bar, top, true) ? top : bar
}
const ownLabel = /^Type something\.?$/
const rule = /^\s*[─━╌]{8,}\s*$/
const scrollNote = /^[↑↓]\s+\d+\s+more\s+lines?$/
const cjk = /[\u3000-\u9fff\uff00-\uffef]/
const joinWrapped = (a: string, b: string) => (!a ? b : cjk.test(a.slice(-1)) || cjk.test(b[0]) ? a + b : a + ' ' + b)
const stripBar = (l: string) => l.replace(/^\s*\u2502\s?/, '').trim()

// Parse tab labels without splitting on "✔" inside a label such as "Color ✔ dark".
function parseTabs(line: string): { label: string; answered: boolean }[] {
  const re = /\s{2,}[☐☒☑]/.test(line)
    ? /([☐☒☑])\s+(\S.*?)(?=\s{2,}[☐☒☑]|\s+[✔✓]\s+Submit(?:\s+→)?\s*$|\s+→\s*$|\s*$)/g
    : /([☐☒☑])\s+(\S.*?)(?=\s+[☐☒☑]|\s+[✔✓]\s+Submit(?:\s+→)?\s*$|\s+→\s*$|\s*$)/g
  return [...line.matchAll(re)].map((m) => ({ label: m[2].trim(), answered: m[1] !== '☐' }))
}

// (Words that ran to their column's edge: what is beside them, a preview's
// box, begins at most 4 spaces after.)
const edge = (s: string) => {
  // (The first box on the line is the column's edge: not one of a preview's own lines.)
  const m = /\s{3,}[\u2500-\u257f]/.exec(s)
  return !!m && m[0].length - 1 <= 4 && /\S$/.test(s.slice(0, m.index))
}

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

// (beside: previews drawn beside the choices, so a label too long for its
// column goes on, wrapped, under it. That line is not taken in — it could
// as well be some other line — but the choice is marked cut where its words
// ran to the column's edge, and is then known by its start: see askCard.)
function options(lines: string[], beside = false): ScreenOption[] {
  const out: ScreenOption[] = []
  for (const l of lines) {
    const m = optionLine.exec(besideAt(l))
    if (!m) {
      const last = out[out.length - 1]
      const words = besideAt(l).trim()
      // (Its own hint, under the last choice, is no choice's.)
      if (beside && last && words && !/^[\u2500-\u257f]/.test(words) && !/^Notes: press n/.test(words)) last.under = (last.under ?? '') + words
      continue
    }
    out.push({ n: Number(m[2]), label: m[4], cursor: !!m[1], ...(m[3] !== undefined ? { ticked: m[3] === '✔' } : {}), ...(beside && edge(l) ? { cut: true } : {}) })
  }
  return out
}

// (Nothing beside: the choices, each with the lines under it, indented
// past its number, as its description — a label too long for a line goes
// on there too, read as of the description: its words all shown still.)
function described(lines: string[]): ScreenOption[] {
  const out: ScreenOption[] = []
  let indent = 0
  for (const l of lines) {
    const m = optionLine.exec(l)
    if (m) {
      out.push({ n: Number(m[2]), label: m[4], cursor: !!m[1], ...(m[3] !== undefined ? { ticked: m[3] === '✔' } : {}) })
      indent = l.indexOf(m[2] + '.')
      continue
    }
    const last = out[out.length - 1]
    const words = l.trim()
    if (!last || !words || /^(❯\s*)?Submit$/.test(words) || l.search(/\S/) <= indent) continue
    last.desc = last.desc ? last.desc + ' ' + words : words
  }
  return out
}

// What Claude Code said above its question on the screen, since the
// person's last message there (its lines as drawn, "●" not kept): shown
// with a question its log does not have yet (see askCard's screenCall).
// None where the screen does not show where it began.
export function askLead(text: string): string[] {
  const lines = lastLines(text)
  const hint = lines.findLastIndex((l, i) => /Enter to select/.test(l) && /navigate/.test(l + ' ' + (lines[i + 1] ?? '')))
  const top = hint >= 0 ? findTabTop(lines, hint) : -1
  if (hint < 0 || top < 0) return []
  // (The dialog's own rule over it, then the words; the person's message, ❯ or >, before them.)
  const over = lines.slice(0, top).findLastIndex((l) => rule.test(l))
  const end = over >= 0 ? over : top
  const asked = lines.slice(0, end).findLastIndex((l) => /^[❯>] \S/.test(l))
  if (asked < 0) return []
  // (Its words only: a block "● …" that is not a tool's call, "● Name(…)", with
  // the lines under it; a call's lines, and what it gave back ("⎿ …"), not.
  // review528-6)
  // (Each block whole, then told: one with a tool's output under it ("⎿")
  // is a call, whatever its title says. review529-4)
  const blocks: string[][] = []
  for (const l of lines.slice(asked + 1, end)) {
    if (/^●\s/.test(l)) blocks.push([l])
    else if (blocks.length) blocks[blocks.length - 1].push(l)
  }
  const out: string[] = []
  for (const b of blocks) {
    const head = b[0].replace(/^●\s+/, '')
    const rest = b.slice(1)
    if (/^[\w.:-]+\(/.test(head) || /^User answered/.test(head) || rest.some((l) => /^\s*⎿/.test(l))) continue
    if (rest.some((l) => l.trim() && !/^ {2}/.test(l))) continue // (not drawn as its words are: not known)
    if (out.length) out.push('')
    out.push(head.trimEnd(), ...rest.map((l) => l.slice(2).trimEnd()))
  }
  while (out.length && !out[out.length - 1].trim()) out.pop()
  while (out.length && !out[0].trim()) out.shift()
  return out.slice(-12)
}

// A menu drawn whole: numbered 1, 2, 3… once each, the cursor on one of
// them (or, cursorElsewhere, on none: a question's Submit line).
const whole = (o: ScreenOption[], cursorElsewhere = false) =>
  o.length > 0 && o.every((x, i) => x.n === i + 1) && o.filter((x) => x.cursor).length === (cursorElsewhere ? 0 : 1)

// (Lines after a menu that are its own: known key hint segments, its file.
// Anything else: the menu is not the last thing on the screen, so not what a
// key would answer.)
const hintSegments = [
  /^shift\+tab to approve with this feedback$/i,
  /^ctrl\+g to edit in Vim$/i,
  /^Enter to (?:select|confirm)$/i,
  /^(?:[↑↓←→/]+|Tab\/Arrow keys|Arrow keys)\s+to navigate$/i,
  /^Tab to (?:switch questions|amend)$/i,
  /^n to add notes$/i,
  /^Esc to cancel$/i,
]
const hintLine = (l: string) => {
  const s = l.trim()
  if (!s) return false
  // Plan dialogs may append a plan file path (which can contain spaces) after "ctrl+g to edit in Vim · ".
  if (/^ctrl\+g to edit in Vim(?: · \S.*)?$/i.test(s)) return true
  const parts = s.split(/\s*·\s*/).filter(Boolean)
  return parts.length > 0 && parts.every((p) => hintSegments.some((h) => h.test(p)))
}
const tailOK = (rest: string[]) => rest.length <= 4 && rest.every((l) => !l.trim() || /^[\s\u2500-\u257f]+$/.test(l) || hintLine(l))

// Parse Claude Code's "Review your answers" page, which may appear with or
// without "Ready to submit your answers?", with a leading "│" gutter and
// multi-line wrapped questions/answers, scroll notes ("↑ 1 more line"), and
// either no footer or known "Enter to select · ... · Esc to cancel" hints.
function parseReview(lines: string[]): Extract<AskScreen, { kind: 'review' }> | null {
  const review = lines.findLastIndex((l) => /^\s*Review your answers\s*$/.test(l))
  if (review < 0) return null
  const rest = lines.slice(review + 1)
  // (A tab bar after "Review your answers" belongs to a newer question drawn below an old review in scrollback.)
  if (rest.some((l) => isTabLine(l))) return null
  const ready = rest.findIndex((l) => /Ready to submit your answers\?/.test(l))
  const firstOpt = rest.findIndex((l, i) => i > ready && optionLine.test(besideAt(l)))
  if (firstOpt < 0) return null
  const summaryEnd = ready >= 0 ? ready : firstOpt
  // Any divider rule between "Review your answers" and the menu means another
  // dialog (such as a permission prompt) started below a stale review summary.
  if (rest.slice(0, firstOpt).some((l) => rule.test(l))) return null
  if (ready >= 0 && rest.slice(ready + 1, firstOpt).some((l) => l.trim() && !scrollNote.test(stripBar(l)))) return null
  const after = rest.slice(firstOpt)
  const opts = options(after)
  const last = after.findLastIndex((l) => optionLine.test(besideAt(l)))
  if (!whole(opts) || !tailOK(after.slice(last + 1))) return null
  const isSubmitMenu = opts.length === 2 && /^Submit answers$/.test(opts[0].label) && /^Cancel$/.test(opts[1].label)
  if (ready < 0 && !isSubmitMenu) return null

  const summary = rest.slice(0, summaryEnd)
  const answers: { question: string; answer: string }[] = []
  let cur: { question: string; answer: string; mode: 'q' | 'a' } | null = null
  let blankSeen = false
  const flush = () => {
    if (cur && cur.question && cur.answer) answers.push({ question: cur.question, answer: cur.answer })
    cur = null
  }
  for (const raw of summary) {
    if (!raw.trim()) {
      blankSeen = true
      continue
    }
    const hasBar = /^\s*\u2502/.test(raw)
    const indent = raw.search(/\S/)
    const l = stripBar(raw)
    if (!l) {
      blankSeen = true
      continue
    }
    if (scrollNote.test(l)) {
      blankSeen = false
      continue
    }
    if (/not answered all questions/i.test(l)) {
      flush()
      blankSeen = true
      continue
    }
    const q = /^[●•]\s+(.*\S)\s*$/.exec(l)
    if (q) {
      flush()
      cur = { question: q[1], answer: '', mode: 'q' }
      blankSeen = false
      continue
    }
    const a = /^→\s+(.*\S)\s*$/.exec(l)
    if (a && cur) {
      cur.answer = a[1]
      cur.mode = 'a'
      blankSeen = false
      continue
    }
    if (cur && !blankSeen && (hasBar || indent >= 2)) {
      if (cur.mode === 'q') cur.question = joinWrapped(cur.question, l)
      else cur.answer = joinWrapped(cur.answer, l)
      continue
    }
    // Unrecognized text inside the summary region (e.g. a subsequent permission
    // dialog below a stale review) disqualifies this as a review screen.
    return null
  }
  flush()
  const incomplete = summary.some((l) => /not answered all questions/i.test(l))
  return { kind: 'review', answers, incomplete, options: opts }
}

export function readAskScreen(text: string): AskScreen {
  const lines = lastLines(text)
  // Check the review page first so a review screen carrying both the tab bar
  // and "Enter to select · ... navigate ..." is not hijacked as a question.
  const reviewed = parseReview(lines)
  if (reviewed) return reviewed
  // (The key hints, last: wrapped on a narrow screen.)
  const hint = lines.findLastIndex((l, i) => /Enter to select/.test(l) && /navigate/.test(l + ' ' + (lines[i + 1] ?? '')))
  // A question: its tabs (or a lone one's header), the question, its choices
  // to the rule above "Chat about this", the key hints last.
  if (hint >= 0 && hint >= lines.length - 3) {
    const top = findTabTop(lines, hint)
    if (top < 0) return { kind: 'unknown' }
    const tabs = parseTabs(lines[top])
    const body = lines.slice(top + 1, hint)
    const first = body.findIndex((l) => optionLine.test(besideAt(l)))
    if (first <= 0) return { kind: 'unknown' }
    const headLines = body.slice(0, first)
    const afterFirst = body.slice(first)
    const end = afterFirst.findIndex((l) => rule.test(l))
    const optBlock = end >= 0 ? afterFirst.slice(0, end) : afterFirst
    // (A long question is drawn with a bar at its left, "│", on each line.)
    const question = headLines
      .filter((l) => !rule.test(l))
      .map(stripBar)
      .filter((l) => !!l && !scrollNote.test(l))
      .join(' ')
    const besideNow = optBlock.some((l) => /\s{3,}[\u2500-\u257f]/.test(l)) || optBlock.some((l) => /Notes: press n/.test(l))
    let opts = besideNow ? options(optBlock, true) : described(optBlock)
    // (Some versions draw "Chat about this" after "Type something." without a divider rule.)
    if (opts.length >= 2 && /^Chat about this$/i.test(opts[opts.length - 1].label) && ownLabel.test(opts[opts.length - 2].label)) {
      opts = opts.slice(0, -1)
    }
    const isSubmitMenu = opts.length === 2 && /^Submit answers$/.test(opts[0].label) && /^Cancel$/.test(opts[1].label)
    if (isSubmitMenu && headLines.some((l) => /^\s*Review your answers\s*$/.test(l))) return { kind: 'unknown' }
    const sub = optBlock.find((l) => /^\s*(❯\s*)?Submit\s*$/.test(l))
    if (!question || !whole(opts, !!sub && /❯/.test(sub))) return { kind: 'unknown' }
    const own = opts.find((o) => ownLabel.test(o.label))?.n
    const multi = opts.some((o) => o.ticked !== undefined)
    const beside = besideNow
    // (Tabs of one question shown as one: a lone question.)
    return { kind: 'question', tabs: tabs.length > 1 ? tabs : [], question, options: opts, ...(own ? { own } : {}), multi, beside, ...(sub ? { submit: { cursor: /❯/.test(sub) } } : {}) }
  }
  // (Codex's plan put to the person — plan mode, 0.167: "Implement this
  // plan?", its choices marked "›", what each does beside it, its keys
  // last.)
  const cx = lines.findLastIndex((l) => /^\s*Implement this plan\?\s*$/.test(l))
  if (cx >= 0) {
    const after = lines.slice(cx + 1)
    const opts: ScreenOption[] = []
    let last = -1
    after.forEach((l, i) => {
      const m = /^\s*(›)?\s*(\d{1,2})\.\s+(.*?)\s*$/.exec(l)
      if (!m) return
      const [label, desc] = m[3].split(/\s{2,}/, 2)
      opts.push({ n: Number(m[2]), label, cursor: !!m[1], ...(desc ? { desc } : {}) })
      last = i
    })
    const keys = after.slice(last + 1).filter((l) => l.trim())
    if (whole(opts) && keys.length === 1 && /^\s*enter (?:to )?select\b.*\besc\b/i.test(keys[0])) return { kind: 'plan', options: opts }
    return { kind: 'unknown' }
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

// A review page with nothing in a log to check it against: only as Claude
// Code draws one, its choices Submit answers and Cancel, its answers listed
// or it saying not all are (words a command shows above its own prompt are
// not one. review528-1, 529-3)
export const reviewAsDrawn = (s: AskScreen) =>
  s.kind === 'review' && (s.answers.length > 0 || s.incomplete) && s.options.length === 2 && /^Submit answers$/.test(s.options[0].label) && /^Cancel$/.test(s.options[1].label)

// The same words, however the screen wrapped or spaced them.
export const sameWords = (a: string, b: string) => a.replace(/\s+/g, ' ').trim() === b.replace(/\s+/g, ' ').trim()
// The same, spaces not counted at all (a line broken within a word, as a
// long path is, puts a space in it when joined): only where nothing else it
// could be is the same so (see askCard's askOn).
export const sameLoose = (a: string, b: string) => a.replace(/\s+/g, '') === b.replace(/\s+/g, '')
