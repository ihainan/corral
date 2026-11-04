import { ApiError } from '../api'
import type { ReplyTarget } from './ReplyDialog'

// Claude Code counts down on a prompt of an unattended session ("⚠ Claude
// Code will automatically deny this request in 1:57, to avoid blocking
// progress on an unattended session"): its screen changes every second, and
// an answer, checked against the screen the person saw (its digest, on the
// host right before the keys), was refused every time — the screen read
// again, refused again. Two screens are the same prompt still where all but
// that count is the same, and the count has gone down since, no more than
// the time between them (review581: a prompt drawn anew, the same words, its
// count starts again — not the one answered).
// (Its words differ by version — 2.1.318: "will automatically block this
// request in …, to avoid holding up an unattended session" — and its time:
// "1:57", or in units, "45s", "1m 5s". The sentence whole, its lines its
// own, once on the screen, and the prompt's choices right under it: a
// command's own words that say so are not taken for it — review581/582.)
const WARNING = /^([^\S\n]*(?:⚠[^\S\n]*)?Claude\s+Code\s+will\s+automatically\s+(?:deny|block)\s+this\s+request\s+in\s+)(\d+(?::\d\d)+|\d+\s*[hms](?:\s*\d+\s*[hms])*)(,\s+to\s+avoid\s+(?:blocking\s+progress\s+on|holding\s+up)\s+an\s+unattended\s+session[^\S\n]*)$/gm
const CHOICES_UNDER = /^\n(?:[^\S\n]*\n){0,2}[^\S\n]*(?:❯[^\S\n]*)?\d+\.[^\S\n]/

// countdown: the screen with its count taken out, and the count (seconds);
// null where it shows none, or more than one.
function countdown(text: string): { rest: string; secs: number } | null {
  const all = [...text.matchAll(WARNING)]
  if (all.length !== 1) return null
  const m = all[0]
  // (Its choices the screen's last: no row "1." after their first — a
  // command's numbered words under such a sentence, the prompt's own
  // choices further down, are not it — review584.)
  const under = CHOICES_UNDER.exec(text.slice(m.index! + m[0].length))
  if (!under || /^[^\S\n]*(?:❯[^\S\n]*)?1\.[^\S\n]/m.test(text.slice(m.index! + m[0].length + under[0].length))) return null
  const t = m[2]
  let secs = 0
  if (t.includes(':')) for (const p of t.split(':')) secs = secs * 60 + Number(p)
  else for (const [, n, u] of t.matchAll(/(\d+)\s*([hms])/g)) secs += Number(n) * (u === 'h' ? 3600 : u === 'm' ? 60 : 1)
  return { rest: text.slice(0, m.index) + m[1] + '…' + m[3] + text.slice(m.index! + m[0].length), secs }
}

export const countsDown = (text: string) => countdown(text) !== null

// steadySame: two screens (texts read at was and now, ms) the same prompt
// still: alike, or but for its count, gone down by no more than the time
// between them (a second more allowed each side: a count shown rounded).
export function steadySame(a: string, was: number, b: string, now: number): boolean {
  if (a === b) return true
  const x = countdown(a)
  const y = countdown(b)
  if (!x || !y || x.rest !== y.rest) return false
  return y.secs <= x.secs && x.secs - y.secs <= (now - was) / 1000 + 2
}

// (at: when it was read, as kept.)
type Screen = { text: string; digest: string; cut?: boolean; at?: number }

// The screens read lately, by digest (whole, not cut), and when: what an
// answer's digest showed. (Module-wide: a target is made again at each render.)
const shown = new Map<string, { text: string; at: number }>()
export function keepShown<S extends Screen>(s: S): S {
  const at = Date.now()
  if (!s.cut) {
    shown.delete(s.digest)
    shown.set(s.digest, { text: s.text, at })
    if (shown.size > 64) shown.delete(shown.keys().next().value!)
  }
  return { ...s, at }
}

// steadily: an answer sent again, a few times, where it was refused as the
// screen changed and the screen read now is the one answered (steadySame,
// with the first and with each read since: its count never up — review582)
// — with the digest of that read, which the host checks as ever. Refused,
// nothing was pressed: sending it again presses it once at most. Only
// where the screen answered counts down (any other, as before); not once
// the one answering is gone (alive: the card no longer shown — review581).
export async function steadily<B extends { digest: string }>(send: (b: B) => Promise<unknown>, look: () => Promise<Screen>, b: B, alive: () => boolean = () => true): Promise<unknown> {
  const first = shown.get(b.digest)
  let prev = first
  let body = b
  for (let i = 0; ; i++) {
    try {
      return await send(body)
    } catch (e) {
      if (!(e instanceof ApiError && e.code === 'screen_changed') || !first || !prev || !countsDown(first.text) || i >= 4 || !alive()) throw e
      // (Read again, not the refusal's screen: herdr's says it as drawn, its
      // GET as joined — review581.)
      let now: Screen
      try {
        now = keepShown(await look())
      } catch {
        throw e
      }
      const at = now.at ?? Date.now()
      if (now.cut || now.digest === body.digest || !steadySame(first.text, first.at, now.text, at) || !steadySame(prev.text, prev.at, now.text, at) || !alive()) throw e
      prev = { text: now.text, at }
      body = { ...body, digest: now.digest }
    }
  }
}

// (A continuation of a choice's words that begins "1. " is taken for
// another menu: not counting down, refused as before — review586, safe.)

// withAlive: a target whose answers say who answers is still there (alive),
// unless they say so themselves — for those made in many steps.
export const withAlive = (t: ReplyTarget, alive: () => boolean): ReplyTarget => ({ ...t, reply: (b) => t.reply({ alive, ...b }) })
