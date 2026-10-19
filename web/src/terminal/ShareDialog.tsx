import { useEffect, useRef, useState, type FormEvent } from 'react'
import { UserRound, X } from 'lucide-react'
import { api, ApiError, errorText, type Share } from '../api'
import { t } from '../i18n'
import { Dialog, LoadError, useToast } from '../components/ui'

// The owner lets other users watch the session, read-only, and sees who is
// watching now.
export function ShareDialog({ sessionId, onClose }: { sessionId: string; onClose: () => void }) {
  const toast = useToast()
  const base = `/api/sessions/${sessionId}/shares`
  const [shares, setShares] = useState<Share[] | null>(null)
  const [failed, setFailed] = useState(false)
  const [name, setName] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  // Only the latest request's answer is shown: a slow poll must not undo
  // what an add or a remove just showed.
  const seq = useRef(0)
  const load = () => {
    const n = ++seq.current
    return api<Share[]>('GET', base)
      .then((l) => {
        if (n !== seq.current) return
        setShares(l)
        setFailed(false)
      })
      .catch(() => {
        if (n === seq.current) setFailed(true)
      })
  }
  useEffect(() => {
    // Who is watching changes while the dialog is open: polled one request
    // at a time, the next a while after the last one answered (a slow server
    // still gets its answers shown).
    let live = true
    let timer: number | undefined
    const tick = async () => {
      await load()
      if (live) timer = window.setTimeout(tick, 5000)
    }
    void tick()
    return () => {
      live = false
      clearTimeout(timer)
      seq.current++ // answers still on their way are ignored
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [sessionId])

  const add = async (e: FormEvent) => {
    e.preventDefault()
    const n = name.trim()
    if (!n || busy) return
    setBusy(true)
    setError('')
    try {
      await api('POST', base, { username: n })
      setName('')
      load()
    } catch (err) {
      setError(err instanceof ApiError && err.code === 'no_such_user' ? t('share.noUser', { name: n }) : errorText(err))
    } finally {
      setBusy(false)
    }
  }
  const remove = async (s: Share) => {
    try {
      await api('DELETE', `${base}/${s.user_id}`)
      toast(t('share.removed', { name: s.display_name || s.username }))
      load()
    } catch (err) {
      toast(errorText(err), true)
    }
  }

  return (
    <Dialog title={t('share.title')} onClose={onClose}>
      <p className="lead">{t('share.lead')}</p>
      <form className="share-add" onSubmit={add}>
        <input
          className="input"
          value={name}
          placeholder={t('share.placeholder')}
          aria-label={t('share.username')}
          autoCapitalize="none"
          autoCorrect="off"
          spellCheck={false}
          onChange={(e) => setName(e.target.value)}
          data-touch-nofocus
        />
        <button className="btn btn-primary" disabled={busy || !name.trim()}>{t('share.add')}</button>
      </form>
      {error && <p className="err" role="alert">{error}</p>}
      {failed ? (
        <LoadError onRetry={() => void load()} />
      ) : shares && shares.length > 0 ? (
        <ul className="share-list">
          {shares.map((s) => (
            <li key={s.user_id}>
              <UserRound size={16} strokeWidth={1.75} className="row-icon" />
              <span className="share-name">
                {s.display_name || s.username}
                {s.display_name && <span className="hint mono"> {s.username}</span>}
              </span>
              <span className={'share-state' + (s.watching ? ' on' : '')}>{s.watching ? t('share.watching') : t('share.notWatching')}</span>
              <button className="icon-btn" onClick={() => void remove(s)} aria-label={t('share.remove', { name: s.username })} title={t('share.removeHint')}>
                <X size={16} />
              </button>
            </li>
          ))}
        </ul>
      ) : (
        shares && <p className="hint">{t('share.none')}</p>
      )}
      <div className="dialog-actions">
        <button className="btn" onClick={onClose}>{t('common.close')}</button>
      </div>
    </Dialog>
  )
}
