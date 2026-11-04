import { ApiError } from '../api'

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
// "1:57", or in units, "45s", "1m 5s". The sentence whole, at a line's
// start, once on the screen: a command's own words that say so are not
// taken for it — review581.)
const WARNING = /^([^\S\n]*(?:⚠[^\S\n]*)?Claude\s+Code\s+will\s+automatically\s+(?:deny|block)\s+this\s+request\s+in\s+)(\d+(?::\d\d)+|\d+\s*[hms](?:\s*\d+\s*[hms])*)(,\s+to\s+avoid\s+(?:blocking\s+progress\s+on|holding\s+up)\s+an\s+unattended\s+session)/gm

// countdown: the screen with its count taken out, and the count (seconds);
// null where it shows none, or more than one.
function countdown(text: string): { rest: string; secs: number } | null {
  const all = [...text.matchAll(WARNING)]
  if (all.length !== 1) return null
  const m = all[0]
  const t = m[2]
  let secs = 0
  if (t.includes(':')) for (const p of t.split(':')) secs = secs * 60 + Number(p)
  else for (const [, n, u] of t.matchAll(/(\d+)\s*([hms])/g)) secs += Number(n) * (u === 'h' ? 3600 : u === 'm' ? 60 : 1)
  return { rest: text.slice(0, m.index) + m[1] + '…' + m[3] + text.slice(m.index! + m[0].length), secs }
}

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

type Screen = { text: string; digest: string; cut?: boolean }

// The screens read lately, by digest (whole, not cut), and when: what an
// answer's digest showed. (Module-wide: a target is made again at each render.)
const shown = new Map<string, { text: string; at: number }>()
export function keepShown(s: Screen): Screen {
  if (!s.cut) {
    shown.delete(s.digest)
    shown.set(s.digest, { text: s.text, at: Date.now() })
    if (shown.size > 64) shown.delete(shown.keys().next().value!)
  }
  return s
}

// shownAt: when the screen of that digest was read, where it was kept.
export const shownAt = (digest: string) => shown.get(digest)?.at

// steadily: an answer sent again, a few times, where it was refused as the
// screen changed and the screen read now is the one answered (steadySame) —
// with the digest of that read, which the host checks as ever. Refused,
// nothing was pressed: sending it again presses it once at most. Not once
// the one answering is gone (alive: the card no longer shown — review581).
export async function steadily<B extends { digest: string }>(send: (b: B) => Promise<unknown>, look: () => Promise<Screen>, b: B, alive: () => boolean = () => true): Promise<unknown> {
  const first = shown.get(b.digest)
  let body = b
  for (let i = 0; ; i++) {
    try {
      return await send(body)
    } catch (e) {
      if (!(e instanceof ApiError && e.code === 'screen_changed') || first === undefined || i >= 4 || !alive()) throw e
      // (Read again, not the refusal's screen: herdr's says it as drawn, its
      // GET as joined — review581.)
      let now: Screen
      try {
        now = keepShown(await look())
      } catch {
        throw e
      }
      if (now.cut || now.digest === body.digest || !steadySame(first.text, first.at, now.text, Date.now()) || !alive()) throw e
      body = { ...body, digest: now.digest }
    }
  }
}
