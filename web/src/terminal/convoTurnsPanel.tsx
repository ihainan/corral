import { useEffect, useRef, useState } from 'react'
import { X } from 'lucide-react'
import { api } from '../api'
import { t } from '../i18n'
import type { Item } from './convoItems'
import { itemOf, type Turn } from './convoTurns'

// The person's turns in the whole log (plans/6-turn-index.md): each of their
// messages, newest first, filtered by what it says; one tapped is gone to —
// where it is not read in yet, the part before is read in, a batch at a
// time, till it is (or there is no more, or the person does something
// else meanwhile).

type Answer = { gen: number; turns: Turn[]; more: boolean; scanned: number; size: number; cut: boolean; from: number }

// At most this many batches read in on the way to one turn.
const MAX_BATCHES = 200

export function ConvoTurnsPanel({
  of,
  gen,
  list,
  older,
  onOlder,
  onGo,
  onClose,
}: {
  of: string // "session pane": whose log
  gen: number
  list: Item[]
  older?: { can: boolean; busy: boolean }
  onOlder: () => void
  onGo: (hit: { key: string; field: string }, gone: () => void) => boolean
  onClose: () => void
}) {
  const [sessionId, pane = ''] = of.split(' ')
  const [turns, setTurns] = useState<Turn[] | null>(null)
  const [more, setMore] = useState(false)
  const [cut, setCut] = useState(false)
  const [failed, setFailed] = useState(false)
  const [q, setQ] = useState('')
  const [want, setWant] = useState<{ turn: Turn; batches: number } | null>(null)
  const [note, setNote] = useState('')
  // (Read, and read on while it says there is more, a second apart.)
  useEffect(() => {
    let stopped = false
    let timer = 0
    setTurns(null)
    setWant(null)
    setNote('')
    const load = () => {
      const qs = new URLSearchParams(pane ? { pane } : {})
      api<Answer>('GET', `/api/sessions/${sessionId}/conversation/turns?` + qs.toString())
        .then((a) => {
          if (stopped) return
          if (a.gen !== gen) return // (of another log than shown: asked again with it)
          setTurns(a.turns)
          setMore(a.more)
          setCut(a.cut)
          setFailed(false)
          if (a.more) timer = window.setTimeout(load, 1000)
        })
        .catch(() => !stopped && setFailed(true))
    }
    load()
    return () => {
      stopped = true
      clearTimeout(timer)
    }
  }, [sessionId, pane, gen])
  // (On the way to one not read in: as each batch comes, looked for again;
  // where what is read in begins already before it (asked of the gateway)
  // and it is not there, given up — not read on to the end — review517.)
  const listNow = useRef(list)
  listNow.current = list
  const turnsNow = useRef(turns)
  turnsNow.current = turns
  useEffect(() => {
    if (!want || older?.busy) return
    let stopped = false
    const key = itemOf(list, turnsNow.current ?? [], want.turn)
    if (key) {
      setWant(null)
      setNote('')
      if (!onGo({ key, field: 'user' }, () => setNote(t('turns.gone')))) setNote(t('turns.gone'))
      return
    }
    const qs = new URLSearchParams(pane ? { pane } : {})
    api<Answer>('GET', `/api/sessions/${sessionId}/conversation/turns?` + qs.toString())
      .then((a) => {
        if (stopped) return
        if (a.gen !== gen || (a.from >= 0 && a.from <= want.turn.at)) {
          setWant(null)
          setNote(t('turns.notFound'))
          return
        }
        if (!older?.can || want.batches >= MAX_BATCHES) {
          setWant(null)
          setNote(t(want.batches >= MAX_BATCHES ? 'turns.tooFar' : 'turns.notFound'))
          return
        }
        setWant({ ...want, batches: want.batches + 1 })
        onOlder()
      })
      .catch(() => {
        if (stopped) return
        setWant(null)
        setNote(t('turns.failed'))
      })
    return () => {
      stopped = true
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [want?.turn, want?.batches, list, older?.busy, older?.can])
  // (The person doing anything else meanwhile — scrolling (wheel, touch, a
  // drag of the scrollbar), a key, the page put behind: the way back called
  // off — review517.)
  const panel = useRef<HTMLDivElement>(null)
  useEffect(() => {
    if (!want) return
    // (Not what is done in the panel: another of them tapped goes to that one instead — review518.)
    const off = (e: Event) => {
      if (e.target instanceof Node && panel.current?.contains(e.target)) return
      setWant(null)
    }
    const kinds = ['wheel', 'touchmove', 'pointerdown', 'keydown'] as const
    kinds.forEach((k) => window.addEventListener(k, off, { passive: true, capture: true }))
    const hid = () => document.visibilityState !== 'visible' && setWant(null)
    document.addEventListener('visibilitychange', hid)
    return () => {
      kinds.forEach((k) => window.removeEventListener(k, off, { capture: true }))
      document.removeEventListener('visibilitychange', hid)
    }
  }, [want])
  const go = (turn: Turn) => {
    setNote('')
    const key = itemOf(listNow.current, turns ?? [], turn)
    if (key) {
      if (!onGo({ key, field: 'user' }, () => setNote(t('turns.gone')))) setNote(t('turns.gone'))
      return
    }
    setWant({ turn, batches: 0 })
  }
  const shown = (turns ?? [])
    .filter((x) => !q.trim() || x.preview.toLowerCase().includes(q.trim().toLowerCase()))
    .slice()
    .reverse()
  const when = (ts?: string) => {
    if (!ts) return ''
    const d = new Date(ts)
    if (isNaN(d.getTime())) return ''
    const today = new Date().toDateString() === d.toDateString()
    return today ? d.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' }) : d.toLocaleDateString([], { month: 'numeric', day: 'numeric' }) + ' ' + d.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' })
  }
  return (
    <div className="convo-search convo-turns" role="navigation" aria-label={t('turns.title')} ref={panel}>
      <div className="convo-search-bar">
        <input
          className="convo-search-input"
          type="search"
          autoFocus
          placeholder={t('turns.filter')}
          aria-label={t('turns.filter')}
          value={q}
          onChange={(e) => setQ(e.target.value)}
          onKeyDown={(e) => {
            if (e.key === 'Escape') {
              e.preventDefault()
              onClose()
            }
          }}
        />
        <button type="button" className="icon-btn" onClick={onClose} aria-label={t('common.close')} title={t('common.close')}>
          <X size={16} />
        </button>
      </div>
      <div className="convo-search-hits">
        {failed && <p className="list-search-none">{t('turns.failed')}</p>}
        {turns === null && !failed && <p className="list-search-none">{t('common.loading')}</p>}
        {more && <p className="list-search-none">{t('turns.more')}</p>}
        {cut && <p className="list-search-none">{t('turns.cut')}</p>}
        {want && <p className="list-search-none" role="status">{t('turns.reading')}</p>}
        {note && <p className="list-search-none" role="status">{note}</p>}
        {turns !== null && !shown.length && <p className="list-search-none">{t(q.trim() ? 'convo.searchNone' : 'turns.none')}</p>}
        {shown.map((x) => (
          <button key={x.at} type="button" className="convo-search-hit convo-turn" onClick={() => go(x)} disabled={more}>
            <span className="convo-turn-when">{when(x.ts)}</span>
            <span className="convo-turn-text">{x.preview}</span>
          </button>
        ))}
      </div>
    </div>
  )
}
