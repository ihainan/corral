// When Claude Code's card above its box is looked for (see cardNotice.tsx and
// plans/headsup-card.md), kept apart from React so that its times can be
// tried on a clock of the test's own: after a turn's end, every minute while
// the page is in front, when it comes to the front again, after a key pressed.

// (ms) After a turn's end (the suggestion's own looks at 1.5, 4 and 9 s bring
// it too); every minute; after a key pressed (its explanation takes a while).
export const AFTER_TURN = [20000, 40000]
export const EVERY = 60000
export const AFTER_PRESS = [1000, 3000, 8000, 15000]

export interface LookerEnv<C> {
  // One look: the card there, or null for none; a throw is a failed look.
  fetch: () => Promise<C | null>
  // (Not to look now: the page not in front, say.)
  away: () => boolean
  setTimeout: (f: () => void, ms: number) => unknown
  clearTimeout: (t: unknown) => void
  setInterval: (f: () => void, ms: number) => unknown
  clearInterval: (t: unknown) => void
  // What a look found (the card, or null), or that it failed (stale: the
  // card shown as it was, not to be answered).
  found: (card: C | null) => void
  failed: () => void
}

export interface CardLooker {
  // Another conversation ('' none): nothing of the last one's, looked for now.
  bind: (key: string) => void
  // A turn ended (the suggestion's looks done): looked for later too.
  turnEnded: () => void
  // A key was pressed: followed while it takes effect.
  pressed: () => void
  // The page came to the front again.
  shown: () => void
  look: () => void
  stop: () => void
}

export function cardLooker<C>(env: LookerEnv<C>): CardLooker {
  let key = ''
  let n = 0 // (each look's number: only the latest one's answer is taken)
  let every: unknown = null
  let turn: unknown[] = []
  let press: unknown[] = []
  const clear = (ts: unknown[]) => ts.forEach((t) => env.clearTimeout(t))
  const look = () => {
    if (!key || env.away()) return
    const k = key
    const m = ++n
    env.fetch().then(
      (c) => {
        if (key === k && n === m) env.found(c)
      },
      () => {
        if (key === k && n === m) env.failed()
      },
    )
  }
  const stop = () => {
    if (every !== null) env.clearInterval(every)
    every = null
    clear(turn)
    clear(press)
    turn = []
    press = []
  }
  return {
    bind(k) {
      stop()
      key = k
      n++
      if (!k) return
      look()
      every = env.setInterval(look, EVERY)
    },
    turnEnded() {
      if (!key) return
      clear(turn)
      turn = AFTER_TURN.map((ms) => env.setTimeout(look, ms))
    },
    pressed() {
      if (!key) return
      clear(press)
      press = AFTER_PRESS.map((ms) => env.setTimeout(look, ms))
    },
    shown() {
      if (!env.away()) look()
    },
    look,
    stop() {
      stop()
      key = ''
      n++
    },
  }
}
