import { memo, useMemo, useState, useSyncExternalStore } from 'react'
import { getLang, t } from '../i18n'

// The thread's turns at its side: a short line for each of the person's
// messages read in (as jump goes from one to the next: not those sent while
// it worked), the one read now marked; over one, what it said; one pressed
// is gone to. Only where there is room beside the column and a pointer to
// hover with (the keys [ ] and the turns panel do it otherwise).

export type RailTurn = { key: string; ts?: string; text: string }

// The turn read now: kept out of the thread's state, so that scrolling past
// one turn to the next draws the rail again and not the whole thread
// (review batch4).
export type RailCell = { get: () => string; set: (key: string) => void; sub: (f: () => void) => () => void }
export function railCell(): RailCell {
  let v = ''
  const subs = new Set<() => void>()
  return {
    get: () => v,
    set: (key) => {
      if (key === v) return
      v = key
      subs.forEach((f) => f())
    },
    sub: (f) => {
      subs.add(f)
      return () => void subs.delete(f)
    },
  }
}

// At most this many lines: more turns than that, a line stands for a few
// of them in a row (its first gone to), not lines past the rail's end.
const MOST = 40

// (When it was said: its time today, else its day too.)
function said(ts: string | undefined): string {
  const d = ts ? new Date(ts) : null
  if (!d || isNaN(d.getTime())) return ''
  const now = new Date()
  const time = d.toLocaleTimeString(getLang(), { hour: '2-digit', minute: '2-digit', hour12: false })
  return d.toDateString() === now.toDateString() ? time : d.toLocaleDateString(getLang(), { month: 'numeric', day: 'numeric' }) + ' ' + time
}

export const ConvoRail = memo(function ConvoRail({ turns, at, onGo }: { turns: RailTurn[]; at: RailCell; onGo: (key: string) => void }) {
  const now = useSyncExternalStore(at.sub, at.get)
  const [over, setOver] = useState<{ i: number; top: number } | null>(null)
  // (Each line: the turns it stands for, from..to.)
  const lines = useMemo(() => {
    const n = Math.min(turns.length, MOST)
    return Array.from({ length: n }, (_, b) => ({ from: Math.floor((b * turns.length) / n), to: Math.floor(((b + 1) * turns.length) / n) }))
  }, [turns.length])
  const nowAt = turns.findIndex((x) => x.key === now)
  const shown = over && lines[over.i]
  const first = shown && turns[shown.from]
  return (
    // (Hidden from readers of the screen: the keys and the turns panel are
    // theirs, and a hundred unnamed stops in the tab order would not be.)
    <nav className="convo-rail" aria-hidden="true" onPointerLeave={() => setOver(null)}>
      {lines.map((l, i) => (
        <button
          key={i}
          type="button"
          tabIndex={-1}
          className={'convo-rail-tick' + (nowAt >= l.from && nowAt < l.to ? ' is-at' : '')}
          onPointerEnter={(e) => setOver({ i, top: e.currentTarget.offsetTop + e.currentTarget.offsetHeight / 2 })}
          onClick={() => turns[l.from] && onGo(turns[l.from].key)}
        />
      ))}
      {first && (
        <div className="convo-rail-tip" style={{ top: over.top }}>
          {said(first.ts) && <span className="convo-rail-when">{said(first.ts)}</span>}
          <span className="convo-rail-text">{first.text || t('convoRail.noText')}</span>
          {shown.to - shown.from > 1 && <span className="convo-rail-when">{t('convoRail.more', { n: shown.to - shown.from - 1 })}</span>}
        </div>
      )}
    </nav>
  )
})
