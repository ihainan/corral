import { useEffect, useRef, useState } from 'react'
import { X } from 'lucide-react'
import { api } from '../api'
import { t } from '../i18n'
import type { Item } from './convoItems'

// The person's turns in the whole log (plans/6-turn-index.md): each of their
// messages, newest first, filtered by what it says; one tapped is gone to —
// where it is not read in yet, the part before is read in, a batch at a
// time, till it is (or there is no more, or the person does something
// else meanwhile).

type Turn = { at: number; ts?: string; preview: string }
type Answer = { gen: number; turns: Turn[]; more: boolean; scanned: number; size: number; cut: boolean }

// At most this many batches read in on the way to one turn.
const MAX_BATCHES = 200

// The item of a turn among those read in: the person's message of that
// time, beginning as it does.
function itemOf(list: Item[], turn: Turn): string | null {
  const flat = (s: string) => s.replace(/\s+/g, ' ').trim()
  const head = flat(turn.preview).replace(/…$/, '').slice(0, 40)
  for (const it of list) {
    if (it.kind !== 'user' || !turn.ts || it.e.ts !== turn.ts) continue
    const text = flat(it.e.parts.filter((p) => p.kind === 'text').map((p) => p.text ?? '').join(' '))
    if (!head || text.startsWith(head)) return 'e' + it.e.seq
  }
  return null
}

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
  // (On the way to one not read in: as each batch comes, looked for again; more read, or given up.)
  const listNow = useRef(list)
  listNow.current = list
  useEffect(() => {
    if (!want) return
    const key = itemOf(list, want.turn)
    if (key) {
      setWant(null)
      setNote('')
      if (!onGo({ key, field: 'user' }, () => setNote(t('turns.gone')))) setNote(t('turns.gone'))
      return
    }
    if (older?.busy) return
    if (!older?.can || want.batches >= MAX_BATCHES) {
      setWant(null)
      setNote(t(want.batches >= MAX_BATCHES ? 'turns.tooFar' : 'turns.notFound'))
      return
    }
    setWant({ ...want, batches: want.batches + 1 })
    onOlder()
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [want, list, older?.busy, older?.can])
  // (The person scrolling or typing meanwhile: the way back called off.)
  useEffect(() => {
    if (!want) return
    const off = () => setWant(null)
    window.addEventListener('wheel', off, { passive: true })
    window.addEventListener('touchmove', off, { passive: true })
    return () => {
      window.removeEventListener('wheel', off)
      window.removeEventListener('touchmove', off)
    }
  }, [want])
  const go = (turn: Turn) => {
    setNote('')
    const key = itemOf(listNow.current, turn)
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
    <div className="convo-search convo-turns" role="navigation" aria-label={t('turns.title')}>
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
        {want && <p className="list-search-none" role="status">{t('turns.reading')}</p>}
        {note && <p className="list-search-none" role="status">{note}</p>}
        {turns !== null && !shown.length && <p className="list-search-none">{t(q.trim() ? 'convo.searchNone' : 'turns.none')}</p>}
        {shown.map((x) => (
          <button key={x.at} type="button" className="convo-search-hit convo-turn" onClick={() => go(x)} disabled={!!want}>
            <span className="convo-turn-when">{when(x.ts)}</span>
            <span className="convo-turn-text">{x.preview}</span>
          </button>
        ))}
      </div>
    </div>
  )
}
