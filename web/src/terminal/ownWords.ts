import { ApiError } from '../api'
import type { ReplyTarget } from './ReplyDialog'

// The person's own words into a row of Claude Code's prompt that takes
// them: a question's "Type something.", a permission's amend (Tab on Yes or
// No), a plan's "Tell Claude what to change" (plans/5-claude-own-words.md).
// Each step from the screen read just before it (the gateway checks that
// on the host); the rest of the prompt as it was all along; Enter only once
// the row shows the words exactly, on one line. Never Esc: on these
// prompts it says no.

type Screen = { text: string; digest: string; cut?: boolean }

// Thrown where the words may be in the row, not sent: the person looks in
// the terminal (nothing is tried again from here).
export class Filled extends Error {}
// Thrown where Enter may have gone: it may be answered.
export class MaybeSent extends Error {}
// Thrown before anything was typed: the screen is not as the step needs.
export class NotAsShown extends Error {}

export type OwnRow = {
  n: number // its number on the screen
  ready: (rest: string) => boolean // the row, before the words: as it is to be (a placeholder)
  keys: string[] // pressed with the cursor on it, to make it take words (Tab), or none
  filled: (text: string) => string // the row, the words in it, as it then shows
}

const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms))

// The row the cursor is on: its number, and the rest of its line.
export function cursorRow(text: string): { n: number; rest: string; line: string } | null {
  const lines = text.split('\n')
  for (let i = lines.length - 1; i >= 0; i--) {
    const m = /^\s*❯\s*(\d+)\. (.*)$/.exec(lines[i])
    if (m) return { n: Number(m[1]), rest: m[2].replace(/\s+$/, ''), line: lines[i] }
  }
  return null
}

// The prompt without row n, its cursor mark, nor its hints at the foot
// (they change as the row does): what must stay as it was. For an active
// AskUserQuestion dialog, only the question itself (tab bar, question, and
// options other than row n) is compared so background-task timers, spinners,
// status bars, Tips, and changing footer hints never cause false refusals.
// For other prompts (plan / permission amend), known footers, live spinners,
// blinking bullets, and bottom agent-list ages are normalized.
const HINT_SEG = /^(?:Enter to (?:select|confirm|submit)|(?:[↑↓←→/]+|Tab\/Arrow keys|Arrow keys) to navigate|Tab to (?:switch questions|amend)|n to add notes|Esc to cancel|shift\+tab to approve with this feedback|ctrl\+g to edit in Vim(?: .*)?)$/
const NAV_HINT = /Enter to (?:select|confirm|submit)|to navigate|Tab to switch questions/
const SPINNER = /^[*·✢✳✶✻✽] (?:[A-Za-z0-9 -]*….*|Waiting for \d+ background (?:task|agent|shell)s?.*)$/
const AGENT_ROW = /^[ \t]*(?:❯[ \t]+)?(?:●|◯)[ \t]/
const AGENT_MAIN = /^[ \t]*(?:❯[ \t]+)?(?:●|◯)[ \t]+main[ \t]*$/
const ROW_AGE = /[ \t]+\d+[hms][ \t]*$/
const PURE_RULE = /^[ \t]*[─━╌▔]{8,}[ \t]*$/
const HINT_RULE = /^[ \t]*[─━╌▔]{2,}.*[─━╌▔]{2,}[ \t]*$/
const MULTI_TAB = /^[ \t]*(?:←[ \t]+)?(?:☐|☒|☑)[ \t]+[^ \t].*[ \t]+(?:✔|✓)[ \t]+Submit(?:[ \t]+→)?[ \t]*$/
const LONE_TAB = /^[ \t]*(?:☐|☒)[ \t]+[^ \t]/
const OPT_LINE = /^[ \t]*(❯)?[ \t]*(\d{1,2})\.[ \t]+(.*)$/
const SUBMIT_LINE = /^[ \t]*(?:❯[ \t]+)?Submit[ \t]*$/
const CHAT_LINE = /^[ \t]*(?:\d{1,2}\.[ \t]+)?Chat about this[ \t]*$/
const BG_MODE = /^[ \t]*(?:⏵|⏸|\? for shortcuts|Tip:[ \t])/
const BG_FOOTER = /\d+ background (?:task|agent|shell)s?|\d+ local agents?|↓ to manage|ctrl\+o to expand|ctrl\+t to hide/
const BG_STATUS = /│.*(?:Ctx |\$\d)|(?:Ctx |\$\d).*│/

const isRule = (l: string) => PURE_RULE.test(l) || (HINT_RULE.test(l) && !l.includes('❯'))
const isFoot = (l: string): boolean => {
  const t = l.trim()
  if (!t || t.includes('❯')) return false
  if (t === 'ctrl+g to edit in Vim' || t.startsWith('ctrl+g to edit in Vim · ')) return true
  const parts = t.split('·').map((p) => p.trim()).filter(Boolean)
  return parts.length > 0 && parts.every((p) => HINT_SEG.test(p))
}
const isBgFooter = (l: string) => !l.includes('❯') && !OPT_LINE.test(l) && (BG_MODE.test(l) || BG_FOOTER.test(l) || BG_STATUS.test(l))

// Extract the [top..end] lines of an active Claude Code AskUserQuestion dialog
// at the bottom of the screen (matching Go's askDialogBounds); null otherwise.
export function extractAskRegion(text: string): string[] | null {
  const lines = text.replace(/\r/g, '').split('\n')
  let n = lines.length
  while (n > 0 && !lines[n - 1].trim()) n--
  if (n < 2) return null
  let k = n - 1
  let sawAskHint = false
  let sawChat = false
  while (k >= 0) {
    const l = lines[k]
    if (!l.trim()) {
      k--
      continue
    }
    if (AGENT_ROW.test(l)) {
      let j = k
      let hasMain = false
      while (j >= 0 && AGENT_ROW.test(lines[j])) {
        if (AGENT_MAIN.test(lines[j])) hasMain = true
        j--
      }
      if (hasMain) {
        k = j
        continue
      }
      break
    }
    if (isFoot(l)) {
      if (NAV_HINT.test(l)) sawAskHint = true
      k--
      continue
    }
    if (k >= 1 && !l.includes('❯') && !OPT_LINE.test(l) && lines[k - 1].trim() && !lines[k - 1].includes('❯') && !OPT_LINE.test(lines[k - 1])) {
      const prev = lines[k - 1].trim()
      const cur = l.trim()
      const js = prev + ' ' + cur
      const jt = prev + cur
      if (isFoot(js)) {
        if (NAV_HINT.test(js)) sawAskHint = true
        k -= 2
        continue
      }
      if (isFoot(jt)) {
        if (NAV_HINT.test(jt)) sawAskHint = true
        k -= 2
        continue
      }
    }
    if (isRule(l)) {
      k--
      continue
    }
    if (CHAT_LINE.test(l)) {
      sawChat = true
      k--
      continue
    }
    if (SPINNER.test(l) || isBgFooter(l)) {
      k--
      continue
    }
    break
  }
  const end = k
  if (end < 1 || (!sawAskHint && !sawChat)) return null
  let submitCursor = 0
  let scanStart = end
  if (SUBMIT_LINE.test(lines[end])) {
    if (lines[end].includes('❯')) submitCursor = 1
    scanStart = end - 1
  }
  let optCount = 0
  let lastNum = 0
  let cursorCount = 0
  let firstOpt = -1
  let maxOptLine = -1
  let badOpt = false
  const limit = Math.max(0, scanStart - 80)
  for (let i = scanStart; i >= limit; i--) {
    const l = lines[i]
    if (isRule(l) || MULTI_TAB.test(l) || LONE_TAB.test(l)) break
    const m = OPT_LINE.exec(l)
    if (m) {
      const num = Number(m[2])
      if (optCount === 0) {
        lastNum = num
        maxOptLine = i
      } else if (num !== lastNum - 1) {
        badOpt = true
        break
      } else {
        lastNum = num
      }
      optCount++
      if (m[1]) cursorCount++
      firstOpt = i
      if (num === 1) break
    } else if (l.includes('❯')) {
      badOpt = true
      break
    }
  }
  if (!badOpt && maxOptLine >= 0) {
    let seenBlank = false
    for (let m = maxOptLine + 1; m <= end; m++) {
      if (SUBMIT_LINE.test(lines[m])) continue
      if (!lines[m].trim()) {
        seenBlank = true
        continue
      }
      if (seenBlank && !lines[m].startsWith('  ') && !lines[m].startsWith('\t')) {
        badOpt = true
        break
      }
    }
  }
  if (badOpt || optCount < 2 || lastNum !== 1 || cursorCount + submitCursor !== 1 || firstOpt <= 0) return null
  const tabLimit = Math.max(0, firstOpt - 60)
  let firstNonBlank = -1
  let nonBlankCount = 0
  for (let j = firstOpt - 1; j >= tabLimit; j--) {
    const l = lines[j]
    if (MULTI_TAB.test(l)) return nonBlankCount > 0 ? lines.slice(j, end + 1) : null
    if (isRule(l)) return firstNonBlank >= 0 && LONE_TAB.test(lines[firstNonBlank]) && nonBlankCount >= 2 ? lines.slice(firstNonBlank, end + 1) : null
    if (l.trim()) {
      firstNonBlank = j
      nonBlankCount++
    }
  }
  if (tabLimit === 0 && firstNonBlank >= 0 && LONE_TAB.test(lines[firstNonBlank]) && nonBlankCount >= 2) return lines.slice(firstNonBlank, end + 1)
  return null
}

function stripBottomAgentAges(lines: string[]): string[] {
  const out = [...lines]
  let n = out.length
  while (n > 0 && !out[n - 1].trim()) n--
  let i = n
  let hasMain = false
  while (i > 0 && AGENT_ROW.test(out[i - 1])) {
    i--
    if (AGENT_MAIN.test(out[i])) hasMain = true
  }
  if (hasMain) {
    for (let k = i; k < n; k++) {
      while (ROW_AGE.test(out[k])) out[k] = out[k].replace(ROW_AGE, '')
    }
  }
  return out
}

export function frame(text: string, n: number): string {
  const row = new RegExp(`^\\s*❯?\\s*${n}\\. `)
  const region = extractAskRegion(text)
  if (region) {
    return region
      .filter((l) => !row.test(l))
      .map((l) => l.replace('❯', ' ').replace(/\s+$/, ''))
      .join('\n')
      .trim()
  }
  const lines = stripBottomAgentAges(text.replace(/\r/g, '').split('\n'))
  return lines
    .filter((l) => !row.test(l) && !isFoot(l) && !SPINNER.test(l))
    // (Its tool's bullet blinks while it waits; a rule may carry a hint that
    // changes as it waits, "◐ medium · /effort", "tmux detected · …": a rule.)
    .map((l) => (RULE.test(l) ? '—' : l.replace('❯', ' ').replace(/^([ \t]*)[●⏺]/, '$1 ').replace(/\s+$/, '')))
    .join('\n')
    .trim()
}

// The row whole on its line: what comes under it is no more of it — another
// choice, a rule, a blank line, or a hint at the foot. (Claude Code wraps a
// long row itself, its words going on under it: then the row is not all
// there, nor sent — review513.)
export function wholeRow(text: string, n: number): boolean {
  const lines = text.replace(/\r/g, '').split('\n')
  const re = new RegExp(`^\\s*❯\\s*${n}\\. `)
  let i = -1
  for (let k = lines.length - 1; k >= 0; k--) {
    if (re.test(lines[k])) {
      i = k
      break
    }
  }
  if (i < 0) return false
  const next = lines[i + 1] ?? ''
  return !next.trim() || /^\s*(❯\s*)?\d+\. /.test(next) || RULE.test(next) || isFoot(next) || CHAT_LINE.test(next)
}

const RULE = /^\s*[─━╌▔]{2,}.*[─━╌▔]\s*$/

// A key's or text's answer, then the screen read again till done says it shows what it was to (3.5 s at most).
async function after(target: ReplyTarget, done: (text: string) => boolean): Promise<Screen | null> {
  const until = Date.now() + 3500
  for (let i = 0; Date.now() < until; i++) {
    await sleep(i === 0 ? 150 : 250)
    const s = await target.screen()
    if (!s.cut && done(s.text)) return s
  }
  return null
}

const refusal = (e: unknown) => e instanceof ApiError && [409, 429].includes(e.status)

// (Claude Code blinks its tool's bullet while it waits: a screen read is
// often not the one a moment later, and the press is refused (nothing
// pressed). Read again and tried again — a few times — where the prompt is
// still the one, as the press needs it (pre).)
async function tries(target: ReplyTarget, from: Screen, pre: (text: string) => boolean, act: (s: Screen) => Promise<unknown>): Promise<void> {
  // (A screen cut short is not the prompt known: not tried on.)
  let s = from
  for (let i = 0; ; i++) {
    // (Before each try, the first too: the prompt as it is to be, the card
    // still the person's — review515.)
    if (!pre(s.text)) throw new NotAsShown()
    try {
      await act(s)
      return
    } catch (e) {
      if (!refusal(e) || i >= 3) throw e
    }
    s = await target.screen()
    if (s.cut || !pre(s.text)) throw new NotAsShown()
  }
}

async function press(target: ReplyTarget, from: Screen, keys: string[], done: (text: string) => boolean, fail: () => Error, pre: (text: string) => boolean): Promise<Screen> {
  try {
    await tries(target, from, pre, (s) => target.reply({ digest: s.digest, keys }))
  } catch (e) {
    if (e instanceof NotAsShown || refusal(e)) throw new NotAsShown()
    throw fail()
  }
  const s = await after(target, done).catch(() => null)
  if (!s) throw fail()
  return s
}

// The words into row r, then Enter; now: the screen the person saw.
// alive: the card still shown, its page in front, the same agent's — else
// nothing more is done (review513: one gone to another agent, or left, went
// on typing and pressing Enter).
export async function ownWords(target0: ReplyTarget, now: Screen, r: OwnRow, text: string, alive: () => boolean = () => true): Promise<Screen> {
  // (Its keys not sent again past a countdown (steadyReply.ts): its frames,
  // the row taken out, cannot tell the prompt's countdown from a command's
  // words that read so — review586; a prompt that counts down refused here
  // as before, nothing typed.)
  const target: ReplyTarget = { ...target0, reply: (b) => target0.reply({ ...b, once: true }) }
  if (!target.fill || now.cut) throw new NotAsShown()
  const base = frame(now.text, r.n)
  // (A screen cut short — more of it than the gateway keeps — cannot be told
  // the same: what changed may be in the part cut off; refused at each read — review513.)
  const same = (t: string) => frame(t, r.n) === base && alive()
  // (From the screen as it is now — the one shown was read a while ago, as
  // the words were being written — where it is still the prompt shown.)
  let s: Screen
  try {
    s = await target.screen()
  } catch {
    throw new NotAsShown()
  }
  if (!same(s.text) || s.cut || !alive()) throw new NotAsShown()
  // (The cursor to it: by ↑↓, four at most a step, each read again.)
  for (let guard = 0; guard < 4; guard++) {
    const at = cursorRow(s.text)
    if (!at || !same(s.text)) throw new NotAsShown()
    if (at.n === r.n) break
    const d = r.n - at.n
    const keys = Array(Math.min(4, Math.abs(d))).fill(d > 0 ? 'down' : 'up')
    const want = at.n + (d > 0 ? keys.length : -keys.length)
    const from = at.n
    s = await press(target, s, keys, (t) => same(t) && cursorRow(t)?.n === want, () => new NotAsShown(), (t) => same(t) && cursorRow(t)?.n === from)
  }
  let at = cursorRow(s.text)
  if (!at || at.n !== r.n || !same(s.text)) throw new NotAsShown()
  if (r.keys.length) {
    const was = at.rest
    s = await press(target, s, r.keys, (t) => same(t) && cursorRow(t)?.n === r.n && r.ready(cursorRow(t)!.rest), () => new NotAsShown(), (t) => same(t) && cursorRow(t)?.n === r.n && cursorRow(t)!.rest === was)
  }
  at = cursorRow(s.text)
  if (!at || at.n !== r.n || !r.ready(at.rest) || !same(s.text)) throw new NotAsShown()
  // (The words: pasted, no Enter.)
  const fill = target.fill
  try {
    await tries(target, s, (t) => same(t) && cursorRow(t)?.n === r.n && r.ready(cursorRow(t)!.rest), (x) => fill({ digest: x.digest, text }))
  } catch (e) {
    if (e instanceof NotAsShown || refusal(e)) throw new NotAsShown() // (refused before anything was typed)
    throw new Filled()
  }
  const want = r.filled(text).replace(/\s+$/, '')
  // (The words in the row, all of them, the row whole on its line — Claude
  // Code wraps a long one itself, under it — and the rest as it was: checked
  // again before each try of Enter too.)
  const rowOK = (t: string) => {
    const c = cursorRow(t)
    return !!c && c.n === r.n && c.rest === want && wholeRow(t, r.n) && frame(t, r.n) === base
  }
  const shows = (t: string) => rowOK(t) && alive()
  const got = await after(target, shows).catch(() => null)
  if (!got) throw new Filled()
  try {
    await tries(target, got, shows, (x) => target.reply({ digest: x.digest, keys: ['enter'] }))
  } catch (e) {
    if (e instanceof NotAsShown || refusal(e)) throw new Filled() // (the words there; Enter not pressed)
    throw new MaybeSent()
  }
  // (Gone, the row as it was no longer: what Enter did. The card no longer
  // the person's meanwhile says nothing of that: not known — review515.)
  const done = await after(target, (t) => {
    if (!alive()) throw new MaybeSent()
    return !rowOK(t)
  }).catch(() => null)
  if (!done) throw new MaybeSent()
  return done
}

// The rows of Claude Code 2.1.312 (as tried: ccprobe5–8.py).
export const questionOwn = (n: number): OwnRow => ({ n, ready: (r) => /^Type something\.?$/.test(r), keys: [], filled: (t) => t })
export const permissionAmend = (n: number, yes: boolean): OwnRow => ({
  n,
  ready: (r) => r === (yes ? 'Yes, and tell Claude what to do next' : 'No, and tell Claude what to do differently'),
  keys: ['tab'],
  filled: (t) => (yes ? 'Yes, ' : 'No, ') + t,
})
export const planChange = (n: number): OwnRow => ({ n, ready: (r) => r === 'Tell Claude what to change', keys: [], filled: (t) => t })

// A text the gateway takes (fillOK): one line, not blank, no control nor separator, 1000 characters at most.
export const ownOK = (t: string) => !!t.trim() && [...t].length <= 1000 && !OWN_BAD.test(t)
const OWN_BAD = new RegExp('[\\u0000-\\u001f\\u007f-\\u009f\\u2028\\u2029]')
