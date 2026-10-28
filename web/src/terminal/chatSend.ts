import type { SessionInfo } from '../api'
import type { ConvoEntry } from './conversation'

// The agents page's message sent to an agent: when its log shows it, and
// how long to wait.

export const PENDING_MS = 30000 // a message sent from the conversation not seen in the log by then: say so

// The user turn the log has for a message sent: the same text, however its
// white space came out (a long one, cut short in the log: what was kept of
// it), and without the paths of pictures in it (Claude Code takes those out
// and carries the pictures themselves).
const PICTURE = /'[^']*\.(?:png|jpe?g|gif|webp)'|"[^"]*\.(?:png|jpe?g|gif|webp)"|(?:~|\/)[^\s'"]*\.(?:png|jpe?g|gif|webp)\b/gi
export const sameTurn = (e: ConvoEntry, sent: string) => {
  const n = (s: string) => s.replace(/\s+/g, ' ').trim()
  const parts = e.parts.filter((p) => p.kind === 'text')
  const got = n(parts.map((p) => p.text).join('\n'))
  // (As many pictures as paths: another message's picture, of the same words, is not this one.)
  const images = e.parts.filter((p) => p.kind === 'image').length
  if (images && (sent.match(PICTURE)?.length ?? 0) !== images) return false
  const want = n(images ? sent.replace(PICTURE, ' ') : sent)
  return parts.some((p) => p.truncated) ? got.length >= 1000 && want.startsWith(got) : got === want
}
// One turn of several messages sent one after another: the first's Enter
// let go of, the next typed on after it in the box, all taken in as one.
// Their words run together, spaces aside, are the turn's.
export const sameRun = (e: ConvoEntry, sent: string[]) => {
  if (sent.length < 2 || e.parts.some((p) => p.kind === 'image' || p.truncated)) return false
  const n = (s: string) => s.replace(/\s+/g, '')
  return n(e.parts.filter((p) => p.kind === 'text').map((p) => p.text).join('')) === n(sent.join(''))
}
// A turn with these words in it, alone or with others (spaces aside).
export const holdsWords = (e: ConvoEntry, sent: string) => {
  const n = (s: string) => s.replace(/\s+/g, '')
  const want = n(sent)
  return !!want && n(e.parts.filter((p) => p.kind === 'text').map((p) => p.text).join('')).includes(want)
}
// Written after it was sent, by the host's clock where the log has one (a
// minute's leeway for clocks that differ): an older turn of the same text,
// say one sent again, is not taken for it.
export const afterSend = (e: ConvoEntry, at: number) => {
  const ts = e.ts ? Date.parse(e.ts) : NaN
  return isNaN(ts) || ts >= at - 60000
}

// A tmux session's agent, whichever of the gateway's sessions shows it.
export const tmuxKey = (s: SessionInfo) => `${s.host_id}|tmux|${s.mux_session ?? ''}`

// The terminal's look on the whole page (no page scroll, toasts under the
// bar) while any page that holds it is mounted.
let held = 0
export function holdTermMode(): () => void {
  held++
  document.documentElement.classList.add('term-mode')
  return () => {
    if (--held > 0) return
    document.documentElement.classList.remove('term-dark', 'term-mode')
  }
}
