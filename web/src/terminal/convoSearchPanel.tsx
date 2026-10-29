import { useEffect, useRef, useState } from 'react'
import { ChevronDown, ChevronUp, X } from 'lucide-react'
import { t } from '../i18n'
import type { Item } from './convoItems'
import { itemKey, search, type Hit } from './convoSearch'

// The conversation searched (what is read in): its hits as excerpts, gone
// to one by one (Enter, ↓; Shift+Enter, ↑) or by a tap; the part before read
// in on asking, a batch at a time, and searched again.

const FIELD: Record<Hit['field'], string> = {
  user: 'convoSearch.user',
  answer: 'convoSearch.answer',
  text: 'convoSearch.text',
  thinking: 'convoSearch.thinking',
  tool: 'convoSearch.tool',
  output: 'convoSearch.output',
  summary: 'convoSearch.summary',
  note: 'convoSearch.note',
}

export function ConvoSearchPanel({ list, onGo, older, onOlder, onClose }: { list: Item[]; onGo: (key: string) => boolean; older?: { can: boolean; busy: boolean }; onOlder: () => void; onClose: () => void }) {
  const [q, setQ] = useState('')
  const [hits, setHits] = useState<Hit[] | null>(null)
  const [capped, setCapped] = useState(false)
  const [at, setAt] = useState(-1)
  const [gone, setGone] = useState(false)
  const input = useRef<HTMLInputElement>(null)
  const listNow = useRef(list)
  listNow.current = list
  // (Searched again 250 ms after the last key, and as what is read in grows
  // or begins further back; not on every word the agent writes.)
  const shape = list.length ? `${list.length}:${itemKey(list[0])}:${itemKey(list[list.length - 1])}` : '0'
  useEffect(() => {
    const query = q.trim()
    if (!query) {
      setHits(null)
      setAt(-1)
      return
    }
    let stop = () => {}
    const tm = setTimeout(() => {
      stop = search(listNow.current, query, (h, more) => {
        setHits(h)
        setCapped(more)
        setAt((a) => (a >= h.length ? -1 : a))
      })
    }, 250)
    return () => {
      clearTimeout(tm)
      stop()
    }
  }, [q, shape])
  useEffect(() => setAt(-1), [q])
  const go = (i: number) => {
    if (!hits?.length) return
    const n = (i + hits.length) % hits.length
    setAt(n)
    setGone(!onGo(hits[n].key))
  }
  // (Kept in view in the list: the one gone to.)
  const listBox = useRef<HTMLDivElement>(null)
  useEffect(() => {
    if (at >= 0) listBox.current?.querySelectorAll('.convo-search-hit')[at]?.scrollIntoView({ block: 'nearest' })
  }, [at])
  const count = hits?.reduce((n, h) => n + h.count, 0) ?? 0
  return (
    <div className="convo-search" role="search">
      <div className="convo-search-bar">
        <input
          ref={input}
          className="convo-search-input"
          type="search"
          autoFocus
          placeholder={t('convoSearch.placeholder')}
          aria-label={t('convoSearch.placeholder')}
          value={q}
          onChange={(e) => setQ(e.target.value)}
          onKeyDown={(e) => {
            if (e.key === 'Escape') {
              e.preventDefault()
              onClose()
            } else if (e.key === 'Enter' || e.key === 'ArrowDown' || e.key === 'ArrowUp') {
              if (e.nativeEvent.isComposing) return
              e.preventDefault()
              go(at < 0 ? (e.shiftKey || e.key === 'ArrowUp' ? -1 : 0) : at + (e.shiftKey || e.key === 'ArrowUp' ? -1 : 1))
            }
          }}
        />
        <span className="convo-search-count" aria-live="polite">
          {q.trim() && hits ? (hits.length ? (at >= 0 ? `${at + 1}/${hits.length} · ` : '') + t('convoSearch.count', { n: count }) : t('convoSearch.none')) : ''}
        </span>
        <button type="button" className="icon-btn" disabled={!hits?.length} aria-label={t('convoSearch.prev')} title={t('convoSearch.prev')} onClick={() => go(at < 0 ? -1 : at - 1)}>
          <ChevronUp size={16} />
        </button>
        <button type="button" className="icon-btn" disabled={!hits?.length} aria-label={t('convoSearch.next')} title={t('convoSearch.next')} onClick={() => go(at + 1)}>
          <ChevronDown size={16} />
        </button>
        <button type="button" className="icon-btn" aria-label={t('common.close')} title={t('common.close')} onClick={onClose}>
          <X size={16} />
        </button>
      </div>
      {!!hits?.length && (
        <div className="convo-search-hits" ref={listBox}>
          {hits.map((h, i) => (
            <button key={h.key + h.field} type="button" className={'convo-search-hit' + (i === at ? ' is-at' : '')} aria-current={i === at || undefined} onClick={() => go(i)}>
              <span className="convo-search-field">
                {t(FIELD[h.field] as Parameters<typeof t>[0])}
                {h.count > 1 ? ' ×' + h.count : ''}
              </span>
              <span className="convo-search-text">
                {h.before}
                <mark>{h.match}</mark>
                {h.after}
              </span>
            </button>
          ))}
        </div>
      )}
      {(capped || gone || older?.can) && (
        <div className="convo-search-foot">
          {capped && <span>{t('convoSearch.capped')}</span>}
          {gone && <span>{t('convoSearch.gone')}</span>}
          {older?.can && (
            <button type="button" className="btn btn-ghost" disabled={older.busy} onClick={onOlder}>
              {older.busy ? t('convoSearch.reading') : t('convoSearch.older')}
            </button>
          )}
        </div>
      )}
    </div>
  )
}
