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
// (they change as the row does): what must stay as it was.
const FOOT = /to cancel|Tab to amend|to approve with this feedback|to edit in Vim|to navigate/
export function frame(text: string, n: number): string {
  const row = new RegExp(`^\\s*❯?\\s*${n}\\. `)
  return text
    .split('\n')
    .filter((l) => !row.test(l) && !FOOT.test(l))
    // (Its tool's bullet blinks while it waits; a rule may carry a hint that
    // changes as it waits, "◐ medium · /effort", "tmux detected · …": a rule.)
    .map((l) => (RULE.test(l) ? '—' : l.replace('❯', ' ').replace(/^●/, ' ').replace(/\s+$/, '')))
    .join('\n')
    .trim()
}

const RULE = /^\s*[─▔]{2,}.*[─▔]\s*$/

// How wide a line is drawn: wide letters (CJK, full-width) two columns.
const WIDE = /[ᄀ-ᅟ⺀-꓏가-힣豈-﫿︰-﹏＀-｠￠-￦\u{20000}-\u{3fffd}]/u
export function width(s: string): number {
  let w = 0
  for (const ch of s) w += WIDE.test(ch) ? 2 : 1
  return w
}

// A key's or text's answer, then the screen read again till done says it shows what it was to (3.5 s at most).
async function after(target: ReplyTarget, done: (text: string) => boolean): Promise<Screen | null> {
  const until = Date.now() + 3500
  for (let i = 0; Date.now() < until; i++) {
    await sleep(i === 0 ? 150 : 250)
    const s = await target.screen()
    if (done(s.text)) return s
  }
  return null
}

const refusal = (e: unknown) => e instanceof ApiError && [409, 429].includes(e.status)

// (Claude Code blinks its tool's bullet while it waits: a screen read is
// often not the one a moment later, and the press is refused (nothing
// pressed). Read again and tried again — a few times — where the prompt is
// still the one, as the press needs it (pre).)
async function tries(target: ReplyTarget, from: Screen, pre: (text: string) => boolean, act: (s: Screen) => Promise<unknown>): Promise<void> {
  let s = from
  for (let i = 0; ; i++) {
    try {
      await act(s)
      return
    } catch (e) {
      if (!refusal(e) || i >= 3) throw e
    }
    s = await target.screen()
    if (!pre(s.text)) throw new NotAsShown()
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
export async function ownWords(target: ReplyTarget, now: Screen, r: OwnRow, text: string): Promise<Screen> {
  if (!target.fill) throw new NotAsShown()
  const base = frame(now.text, r.n)
  const same = (t: string) => frame(t, r.n) === base
  // (From the screen as it is now — the one shown was read a while ago, as
  // the words were being written — where it is still the prompt shown.)
  let s: Screen
  try {
    s = await target.screen()
  } catch {
    throw new NotAsShown()
  }
  if (!same(s.text)) throw new NotAsShown()
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
  const shows = (t: string) => {
    const c = cursorRow(t)
    return !!c && c.n === r.n && c.rest === want && same(t)
  }
  const got = await after(target, shows).catch(() => null)
  if (!got) throw new Filled()
  // (On one line, not wrapped nor cut: its line narrower than the screen's widest.)
  const widest = Math.max(...got.text.split('\n').map(width))
  if (width(cursorRow(got.text)!.line) >= widest - 1) throw new Filled()
  try {
    await tries(target, got, shows, (x) => target.reply({ digest: x.digest, keys: ['enter'] }))
  } catch (e) {
    if (e instanceof NotAsShown || refusal(e)) throw new Filled() // (the words there; Enter not pressed)
    throw new MaybeSent()
  }
  const done = await after(target, (t) => !shows(t)).catch(() => null)
  if (!done) throw new MaybeSent()
  return done
}

// The rows of Claude Code 2.1.312 (as tried: ccprobe5–8.py).
export const questionOwn = (n: number): OwnRow => ({ n, ready: (r) => r === 'Type something.', keys: [], filled: (t) => t })
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
