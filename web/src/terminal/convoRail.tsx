import { memo, useState } from 'react'
import { getLang, t } from '../i18n'

// The thread's turns at its side: a short line for each of the person's
// messages read in (as jump goes from one to the next: not those sent while
// it worked), the one read now marked; over one, what it said; one pressed
// is gone to. Only where there is room beside the column and a pointer to
// hover with (the keys [ ] and the turns panel do it otherwise).

export type RailTurn = { key: string; ts?: string; text: string }

// (When it was said: its time today, else its day too.)
function said(ts: string | undefined): string {
  const d = ts ? new Date(ts) : null
  if (!d || isNaN(d.getTime())) return ''
  const now = new Date()
  const time = d.toLocaleTimeString(getLang(), { hour: '2-digit', minute: '2-digit', hour12: false })
  return d.toDateString() === now.toDateString() ? time : d.toLocaleDateString(getLang(), { month: 'numeric', day: 'numeric' }) + ' ' + time
}

export const ConvoRail = memo(function ConvoRail({ turns, at, onGo }: { turns: RailTurn[]; at: string; onGo: (key: string) => void }) {
  const [over, setOver] = useState<{ i: number; top: number } | null>(null)
  const shown = over && turns[over.i]
  return (
    // (Hidden from readers of the screen: the keys and the turns panel are
    // theirs, and a hundred unnamed stops in the tab order would not be.)
    <nav className="convo-rail" aria-hidden="true" onPointerLeave={() => setOver(null)}>
      {turns.map((x, i) => (
        <button
          key={x.key}
          type="button"
          tabIndex={-1}
          className={'convo-rail-tick' + (x.key === at ? ' is-at' : '')}
          onPointerEnter={(e) => setOver({ i, top: e.currentTarget.offsetTop + e.currentTarget.offsetHeight / 2 })}
          onClick={() => onGo(x.key)}
        />
      ))}
      {shown && (
        <div className="convo-rail-tip" style={{ top: over.top }}>
          {said(shown.ts) && <span className="convo-rail-when">{said(shown.ts)}</span>}
          <span className="convo-rail-text">{shown.text || t('convoRail.noText')}</span>
        </div>
      )}
    </nav>
  )
})
