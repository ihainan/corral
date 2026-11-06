// What became of a message sent from the conversation, as far as its log
// says. Claude Code writes a row the moment it takes Enter (a user row at
// rest, a queue row while it works), but not the words as they went: a path
// to a picture alone on its line becomes "[Image #N]" at the front, blank
// lines go, a path among words stays (plans/chat-parity.md). So a row is
// never told by its words: the next submission in the log after a message
// went is taken to be it, for what the page shows only. The words count where
// a mistake would cost something: letting go of a draft (sameMessage).

// (A path to a picture, as the composer or a person writes one.)
const PICTURE = /'[^']*\.(?:png|jpe?g|gif|webp)'|"[^"]*\.(?:png|jpe?g|gif|webp)"|(?:~|\/)[^\s'"]*\.(?:png|jpe?g|gif|webp)\b/gi
const IMAGE_TOKEN = /\[Image #\d+\]/g

const picturesIn = (s: string) => (s.match(PICTURE) ?? []).map((p) => p.replace(/^['"]|['"]$/g, ''))
const wordsOf = (s: string) => s.replace(IMAGE_TOKEN, ' ').replace(PICTURE, ' ').replace(/\s+/g, '')

// The same message, however it was written down: the same words, and each
// path it was sent with still there as a path or gone for a placeholder. (A
// path the log has that was not sent is another message's: "delete a.png" is
// not "delete b.png". Nothing but pictures: nothing to tell it by.)
export function sameMessage(logged: string, sent: string): boolean {
  const w = wordsOf(sent)
  if (!w || wordsOf(logged) !== w) return false
  const rest = picturesIn(sent)
  for (const k of picturesIn(logged)) {
    const at = rest.indexOf(k)
    if (at < 0) return false
    rest.splice(at, 1)
  }
  return rest.length === (logged.match(IMAGE_TOKEN)?.length ?? 0)
}

// A row the log has of something submitted: its place, its time, its words.
export interface Submission {
  key: number
  seq?: number // (a turn's; a message still queued has none)
  ts?: string
  text: string
}
export interface Waiting {
  id: number
  text: string
  at: number
  after: number // the log's last seq when it went
  beforeQueued?: number[] // what waited in its queue when it went: not this one's
  afterTs?: string // and that row's time: read anew (other seqs), only later ones count
}

// (Clocks of the host and the browser may differ this much.)
const CLOCK_LEEWAY_MS = 60000

export const camesAfter = (s: Submission, p: Waiting) => {
  if (s.seq !== undefined && s.seq <= p.after) return false
  if (s.seq === undefined && p.beforeQueued?.includes(s.key)) return false
  const ts = s.ts ? Date.parse(s.ts) : NaN
  if (!isNaN(ts) && ts < p.at - CLOCK_LEEWAY_MS) return false
  return !(p.afterTs && !isNaN(ts) && ts <= Date.parse(p.afterTs))
}

// A command that opens a panel, or that Claude Code answers itself, may write
// no row at all: none is waited for, and only a row of that command is its own.
// (A command, not a path: a message may well begin with a picture's.)
export const isCommand = (text: string) => /^\/[\w:.-]+(?:\s|$)/.test(text.trimStart()) && !/\.(?:png|jpe?g|gif|webp)$/i.test(text.trimStart().split(/\s/, 1)[0])

// Oldest message first, each the first submission since it went that no other
// has: which ones the log now shows gone, and with what.
export function pairSent<P extends Waiting>(waiting: P[], subs: Submission[], used: Set<number>): { p: P; s: Submission }[] {
  const out: { p: P; s: Submission }[] = []
  const taken = new Set<number>()
  for (const p of waiting) {
    const command = isCommand(p.text) ? p.text.trimStart().split(/\s/, 1)[0] : ''
    const s = subs.find((x) => !used.has(x.key) && !taken.has(x.key) && camesAfter(x, p) && (!command || x.text.trimStart().split(/\s/, 1)[0] === command))
    if (!s) continue
    taken.add(s.key)
    out.push({ p, s })
  }
  return out
}
