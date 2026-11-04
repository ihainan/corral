import { ApiError } from '../api'

// Claude Code counts down on a prompt of an unattended session ("⚠ Claude
// Code will automatically deny this request in 1:57, to avoid blocking
// progress on an unattended session"): its screen changes every second, and
// an answer, checked against the screen the person saw (its digest, on the
// host right before the keys), was refused every time — the screen read
// again, refused again. The screen as compared: that count left out. (Its
// words differ by version: 2.1.318 "will automatically block this request
// in …, to avoid holding up an unattended session"; the time, as "1:57",
// or in units, "45s", "1m 5s".)
const COUNTDOWN = /(automatically\s+(?:deny|block|reject|refuse)\s+this\s+request\s+in\s+)\d[\d:.]*(?:\s*\d*[hms]\b)*/g

export const steadyText = (text: string) => text.replace(COUNTDOWN, '$1…')

type Screen = { text: string; digest: string; cut?: boolean }

// The screens read lately, by digest (whole, not cut): what an answer's
// digest showed. (Module-wide: a target is made again at each render.)
const shown = new Map<string, string>()
export function keepShown(s: Screen): Screen {
  if (!s.cut) {
    shown.delete(s.digest)
    shown.set(s.digest, s.text)
    if (shown.size > 64) shown.delete(shown.keys().next().value!)
  }
  return s
}

// steadily: an answer sent again, a few times, where it was refused as the
// screen changed and the screen read now is the one answered, its countdown
// aside — with the digest of that read, which the host checks as ever.
// Refused, nothing was pressed: sending it again presses it once at most.
export async function steadily<B extends { digest: string }>(send: (b: B) => Promise<unknown>, look: () => Promise<Screen>, b: B): Promise<unknown> {
  let body = b
  for (let i = 0; ; i++) {
    try {
      return await send(body)
    } catch (e) {
      const was = shown.get(body.digest)
      if (!(e instanceof ApiError && e.code === 'screen_changed') || was === undefined || i >= 4) throw e
      // (herdr's refusal says the screen as it is now; the conversation's is read.)
      const now = keepShown(typeof e.extra.digest === 'string' ? { text: String(e.extra.text ?? ''), digest: e.extra.digest, cut: e.extra.cut === true } : await look())
      if (now.cut || now.digest === body.digest || steadyText(now.text) !== steadyText(was)) throw e
      body = { ...body, digest: now.digest }
    }
  }
}
