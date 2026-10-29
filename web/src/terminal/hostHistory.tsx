import { useEffect, useRef, useState } from 'react'
import { api, ApiError, errorText } from '../api'
import { t } from '../i18n'
import { Dialog } from '../components/ui'

// What the person sent before, as the host keeps it (Claude Code's
// ~/.claude/history.jsonl, every project's): read a page at a time from the
// latest, searched as it is read; one picked goes into the composer, not
// sent. See the gateway's apiInputHistory.

type Item = { preview: string; text?: string; text_cut?: boolean; ts?: number; project?: string; pasted?: boolean }
type Page = { items: Item[]; cursor?: number; fp?: string; skip?: string; skipped?: number; done: boolean }
type Next = { cursor: number; fp: string; skip?: string }

const when = (ms?: number) => {
  if (!ms) return ''
  const d = new Date(ms)
  const now = new Date()
  const hm = d.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' })
  return d.toDateString() === now.toDateString() ? hm : d.toLocaleDateString([], { month: 'numeric', day: 'numeric' }) + ' ' + hm
}
const base = (p?: string) => (p ? p.replace(/\/+$/, '').split('/').pop() || p : '')

export function HostHistoryDialog({ sid, cwd, onPick, onClose }: { sid: string; cwd?: string; onPick: (text: string) => void; onClose: () => void }) {
  const [q, setQ] = useState('')
  const [items, setItems] = useState<Item[]>([])
  const [next, setNext] = useState<Next | null>(null)
  const [done, setDone] = useState(false)
  const [skipped, setSkipped] = useState(0)
  const [busy, setBusy] = useState(false)
  const [trouble, setTrouble] = useState('')
  const [note, setNote] = useState('')
  const [shownQ, setShownQ] = useState('') // (the search the list and its next page are of)
  // (One request at a time: a new one cancels the one before, and a page
  // that comes back for what is no longer asked is not shown.)
  const asked = useRef<{ n: number; ctl?: AbortController }>({ n: 0 })

  const load = async (query: string, from: Next | null, fresh: boolean) => {
    asked.current.ctl?.abort()
    const ctl = new AbortController()
    const n = ++asked.current.n
    asked.current.ctl = ctl
    setBusy(true)
    setTrouble('')
    if (fresh) {
      setShownQ(query)
      setItems([])
      setNext(null)
      setDone(false)
      setSkipped(0)
    }
    const params = new URLSearchParams()
    if (query) params.set('q', query)
    if (from) {
      params.set('cursor', String(from.cursor))
      params.set('fp', from.fp)
      if (from.skip) params.set('skip', from.skip)
    }
    try {
      const p = await api<Page>('GET', `/api/sessions/${sid}/input-history?` + params, undefined, { signal: ctl.signal, timeoutMs: 15000 })
      if (n !== asked.current.n) return
      setItems((was) => (fresh ? p.items : [...was, ...p.items]))
      setSkipped((s) => (fresh ? 0 : s) + (p.skipped ?? 0))
      setDone(p.done)
      setNext(p.done || p.cursor === undefined || !p.fp ? null : { cursor: p.cursor, fp: p.fp, skip: p.skip })
    } catch (e) {
      if (n !== asked.current.n || ctl.signal.aborted) return
      if (e instanceof ApiError && e.code === 'history_changed') {
        // (Rewritten under the pages read: begun again from the latest.)
        setNote(t('hostHistory.changed'))
        void load(query, null, true)
        return
      }
      setTrouble(e instanceof ApiError && e.code === 'history_timeout' ? t('hostHistory.slow') : e instanceof ApiError && e.code === 'history_busy' ? t('hostHistory.busy') : errorText(e))
    } finally {
      if (n === asked.current.n) setBusy(false)
    }
  }

  // The first page now; a search 400 ms after the last key (what was asked
  // before no longer stands from the first key: neither its answer nor its
  // next page).
  useEffect(() => {
    asked.current.ctl?.abort()
    asked.current.n++
    setNext(null)
    setTrouble('')
    setBusy(true)
    const tm = setTimeout(() => void load(q.trim().toLowerCase(), null, true), q ? 400 : 0)
    return () => clearTimeout(tm)
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [q, sid])
  useEffect(() => () => asked.current.ctl?.abort(), [])

  const here = cwd?.replace(/\/+$/, '')
  return (
    <Dialog title={t('hostHistory.title')} onClose={onClose} wide className="host-history">
      <input className="input" type="search" autoFocus placeholder={t('hostHistory.search')} aria-label={t('hostHistory.search')} value={q} onChange={(e) => (setNote(''), setQ(e.target.value))} />
      {note && <p className="hint">{note}</p>}
      <div className="host-history-list" role="list" aria-busy={busy}>
        {items.map((it, i) => {
          const can = !it.text_cut && it.text !== undefined
          return (
            <div role="listitem" key={i}>
            <button
              type="button"
              className={'host-history-row' + (here && it.project?.replace(/\/+$/, '') === here ? ' is-here' : '')}
              aria-disabled={!can || undefined}
              onClick={() => can && onPick(it.text!)}
            >
              <span className="host-history-text">{it.preview}</span>
              <span className="host-history-meta">
                {base(it.project) && <span className="host-history-project" title={it.project}>{base(it.project)}</span>}
                <span>{when(it.ts)}</span>
                {it.text_cut && <span className="host-history-flag">{t('hostHistory.cut')}</span>}
                {it.pasted && <span className="host-history-flag">{t('hostHistory.pastedWhy')}</span>}
              </span>
            </button>
            </div>
          )
        })}
        {!busy && done && items.length === 0 && <p className="hint">{q ? t('hostHistory.noneFound') : t('hostHistory.empty')}</p>}
      </div>
      {skipped > 0 && <p className="hint">{t('hostHistory.skipped', { n: skipped })}</p>}
      {trouble && (
        <p className="error" role="alert">
          {trouble}{' '}
          <button type="button" className="btn btn-ghost" onClick={() => void load(shownQ, next, !next)}>
            {t('hostHistory.retry')}
          </button>
        </p>
      )}
      <div className="host-history-foot">
        {busy ? (
          <span className="hint">{t('hostHistory.reading')}</span>
        ) : (
          next &&
          !trouble && (
            <button type="button" className="btn btn-ghost" onClick={() => void load(shownQ, next, false)}>
              {shownQ && items.length === 0 ? t('hostHistory.notYet') : t('hostHistory.more')}
            </button>
          )
        )}
        <button type="button" className="btn" onClick={onClose}>
          {t('common.close')}
        </button>
      </div>
    </Dialog>
  )
}
