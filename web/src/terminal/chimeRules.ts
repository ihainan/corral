import type { Prefs } from '../prefs'
import type { SessionInfo } from '../api'

// The rules of the page's sounds (terminal/chime.ts), apart from the sound
// itself and the settings, so they can be tried out of a browser.

export type ChimeKind = 'done' | 'needs' | 'bell'

// wanted: whether a sound of this kind is asked for.
export function wanted(kind: ChimeKind, p: Pick<Prefs, 'chime' | 'chimeBells'>): boolean {
  if (p.chime === 'off') return false
  if (kind === 'needs') return p.chime === 'all'
  if (kind === 'bell') return p.chimeBells
  return true
}

// taken: whether a sound of key was taken already, by this page or another
// of this browser, in the last few seconds; if not, it is taken now.
const TAKEN = 'corral.chime.'
const TAKEN_FOR = 5000
export function taken(key: string, now = Date.now(), store: Pick<Storage, 'getItem' | 'setItem' | 'removeItem' | 'key' | 'length'> | null = safeStorage()): boolean {
  if (!store) return false
  try {
    const at = Number(store.getItem(TAKEN + key) ?? 0)
    if (at && now - at < TAKEN_FOR) return true
    store.setItem(TAKEN + key, String(now))
    // (Old ones let go of.)
    for (let i = store.length - 1; i >= 0; i--) {
      const k = store.key(i)
      if (k && k.startsWith(TAKEN) && now - Number(store.getItem(k) ?? 0) > TAKEN_FOR * 4) store.removeItem(k)
    }
  } catch {
    /* not kept: this page sounds */
  }
  return false
}

function safeStorage(): Storage | null {
  try {
    return window.localStorage
  } catch {
    return null
  }
}

// The counts a page has seen, by pane (its session, its pane id): what
// went up since.
export type Counts = { done: number; needs: number }
export class ChimeCounter {
  private seen = new Map<string, Counts>()
  // feed: the counts now, of every pane in what was read; what went up
  // (each once, with its count, for a key the same in every page).
  feed(now: Map<string, Counts>): { pane: string; kind: 'done' | 'needs'; n: number }[] {
    const out: { pane: string; kind: 'done' | 'needs'; n: number }[] = []
    for (const [pane, c] of now) {
      const had = this.seen.get(pane)
      this.seen.set(pane, c)
      if (!had) continue
      if (c.done > had.done) out.push({ pane, kind: 'done', n: c.done })
      if (c.needs > had.needs) out.push({ pane, kind: 'needs', n: c.needs })
    }
    return out
  }
  // forget: panes no longer read (a session gone): seen anew, should they come back.
  keep(only: Set<string>) {
    for (const k of this.seen.keys()) if (!only.has(k)) this.seen.delete(k)
  }
}

// countsOf: the counts of the herdr agents of sessions (a summary the
// gateway lost is no reading: none of it).
export function countsOf(sessions: SessionInfo[]): Map<string, Counts> {
  const out = new Map<string, Counts>()
  for (const s of sessions) {
    const m = s.meta
    if (s.persistence !== 'herdr' || !m || m.t !== 'herdr' || m.lost) continue
    for (const w of m.workspaces) for (const a of w.agents) out.set(s.id + ' ' + a.pane_id, { done: a.chime_done ?? 0, needs: a.chime_needs ?? 0 })
  }
  return out
}

// Bells: a program's bell, or its notice (OSC 9, 777), as live output of a
// terminal. Not a bell the person's own key just brought (a shell's at a
// completion not found: within BELL_AFTER_KEY of a key, a plain bell is
// not one); not two from one terminal within BELL_GAP; not in a herdr
// session within BELL_AFTER_AGENT of an agent's own sound there (herdr
// passes its panes' bells on: the agent's is told by its status).
export const BELL_AFTER_KEY = 1500
export const BELL_GAP = 3000
export const BELL_AFTER_AGENT = 3000
export class BellFilter {
  key = -Infinity
  rang = -Infinity
  agent = -Infinity
  ring(now: number, plain: boolean): boolean {
    if (plain && now - this.key < BELL_AFTER_KEY) return false
    if (now - this.rang < BELL_GAP || now - this.agent < BELL_AFTER_AGENT) return false
    this.rang = now
    return true
  }
}

// oscNotice: whether an OSC 9 or 777 payload is a notice (as the gateway's
// alert scanner reads them: OSC 9;4;… is a progress report).
export function oscNotice(code: number, data: string): boolean {
  if (code === 9) return data !== '' && !/^4;/.test(data)
  if (code === 777) return /^notify;./.test(data)
  return false
}
