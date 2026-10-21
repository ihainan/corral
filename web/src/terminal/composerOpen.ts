import { local } from '../storage'

// Whether the composer was open, by session, on this device: it is opened
// again where it was left open (a reload, coming back). A session not seen
// yet follows the last choice made anywhere. A few dozen sessions are kept.
const KEY = 'corral.composerOpen'
const MAX = 50

type Saved = { last: boolean; s: [string, boolean][] }

function read(): Saved {
  try {
    const v = JSON.parse(local.get(KEY) ?? 'null')
    if (v && typeof v.last === 'boolean' && Array.isArray(v.s)) return { last: v.last, s: v.s.filter((x: unknown) => Array.isArray(x) && typeof x[0] === 'string' && typeof x[1] === 'boolean') }
  } catch {
    /* not ours */
  }
  return { last: false, s: [] }
}

export function composerWasOpen(session: string): boolean {
  const v = read()
  const own = v.s.find(([id]) => id === session)
  return own ? own[1] : v.last
}

export function rememberComposer(session: string, open: boolean) {
  const v = read()
  const s = [...v.s.filter(([id]) => id !== session), [session, open] as [string, boolean]].slice(-MAX)
  local.set(KEY, JSON.stringify({ last: open, s }))
}

// A session seen for the first time keeps what it started with as its own
// (a later choice elsewhere does not change it).
export function adoptComposer(session: string, open: boolean) {
  const v = read()
  if (v.s.some(([id]) => id === session)) return
  local.set(KEY, JSON.stringify({ last: v.last, s: [...v.s, [session, open] as [string, boolean]].slice(-MAX) }))
}
