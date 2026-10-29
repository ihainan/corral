import { useMemo, useState, type FormEvent } from 'react'
import { api, ApiError, errorText, type SessionInfo } from '../api'
import { t } from '../i18n'
import { Dialog } from '../components/ui'
import { local } from '../storage'

// A new agent from the conversation page (herdr sessions): a workspace of
// its own in a folder, the agent started there (or Claude Code taking up a
// session of before); its first message, if one is written, waits in its
// composer. See the gateway's apiNewAgent.

export const NEW_AGENT = 'corral:new-agent' // (asked from the list: the page opens this)

type Kind = 'claude' | 'codex' | 'opencode'
type Old = { id: string; modified: number; title?: string; first?: string }

const RECENT = 'corral.newAgentDirs'
const recentDirs = (): string[] => {
  try {
    const v = JSON.parse(local.get(RECENT) ?? '[]')
    return Array.isArray(v) ? v.filter((x): x is string => typeof x === 'string').slice(0, 10) : []
  } catch {
    return []
  }
}
const keepDir = (d: string) => {
  try {
    local.set(RECENT, JSON.stringify([d, ...recentDirs().filter((x) => x !== d)].slice(0, 10)))
  } catch {
    // (not kept: no matter)
  }
}

const herdrOf = (s: SessionInfo) => (s.persistence === 'herdr' && s.meta?.t === 'herdr' && !s.meta.lost ? s.meta : null)

export function NewAgentDialog({ sessions, onDone, onOpen, onClose }: { sessions: SessionInfo[]; onDone: (s: SessionInfo, pane: string, first: string) => void; onOpen: (s: SessionInfo, pane: string) => void; onClose: () => void }) {
  const herdrs = sessions.filter((s) => herdrOf(s))
  const [sid, setSid] = useState(herdrs[0]?.id ?? '')
  const sess = herdrs.find((s) => s.id === sid)
  // (Folders the agents there work in, then the ones started from here lately.)
  const dirs = useMemo(() => {
    const m = sess ? herdrOf(sess) : null
    const seen = (m?.workspaces ?? []).flatMap((w) => w.agents.map((a) => a.cwd)).filter(Boolean)
    return [...new Set([...seen, ...recentDirs()])].slice(0, 12)
  }, [sess])
  const [dir, setDir] = useState(() => dirs[0] ?? '~/')
  const [kind, setKind] = useState<Kind>('claude')
  const [label, setLabel] = useState('')
  const [first, setFirst] = useState('')
  const [olds, setOlds] = useState<Old[] | null>(null)
  const asked = useState(() => ({ n: 0 }))[0] // (a list come back for what is no longer asked: not shown)
  const [oldsBusy, setOldsBusy] = useState(false)
  const [resume, setResume] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  // (One key for one thing asked: said again unchanged (its answer not
  // known), the same, one agent; changed, a new one.)
  const keyFor = useState(() => ({ sig: '', key: '' }))[0]
  const keyOf = (sig: string) => {
    if (keyFor.sig !== sig) {
      keyFor.sig = sig
      keyFor.key = crypto.randomUUID()
    }
    return keyFor.key
  }
  const running = useMemo(() => {
    const m = sess ? herdrOf(sess) : null
    const out = new Map<string, string>()
    for (const w of m?.workspaces ?? []) for (const a of w.agents) if (a.session) out.set(a.session, a.pane_id)
    return out
  }, [sess])
  if (!herdrs.length) {
    return (
      <Dialog title={t('newAgent.title')} onClose={onClose}>
        <p className="lead">{t('newAgent.noHerdr')}</p>
        <div className="dialog-actions">
          <button className="btn" onClick={onClose}>
            {t('common.close')}
          </button>
        </div>
      </Dialog>
    )
  }
  const listOld = async () => {
    if (!sess || oldsBusy) return
    const n = ++asked.n
    setOldsBusy(true)
    setError('')
    try {
      const r = await api<{ sessions: Old[]; partial?: boolean }>('GET', `/api/sessions/${sess.id}/claude-sessions?` + new URLSearchParams({ dir }))
      if (n === asked.n) setOlds(r.sessions)
    } catch (e) {
      if (n === asked.n) setError(e instanceof ApiError && e.code === 'bad_folder' ? t('newAgent.badFolder') : errorText(e))
    } finally {
      if (n === asked.n) setOldsBusy(false)
    }
  }
  const submit = async (e?: FormEvent) => {
    e?.preventDefault()
    if (!sess || busy || !dir.trim()) return
    setBusy(true)
    setError('')
    try {
      const body = { dir: dir.trim(), kind, label: label.trim(), resume: kind === 'claude' ? resume : '' }
      const r = await api<{ pane: string }>('POST', `/api/sessions/${sess.id}/agents`, { ...body, key: keyOf(sess.id + '\n' + JSON.stringify(body)) }, { timeoutMs: 90000 })
      keepDir(dir.trim())
      onDone(sess, r.pane, first.trim())
    } catch (err) {
      if (err instanceof ApiError && err.code === 'bad_folder') setError(t('newAgent.badFolder'))
      else if (err instanceof ApiError && err.code === 'start_failed') setError(t('newAgent.failed', { why: err.message }) + (err.extra.closed === false ? ' ' + t('newAgent.notClosed') : ''))
      else if (err instanceof ApiError && (err.code === 'busy' || err.code === 'in_progress')) setError(t('newAgent.busy'))
      else setError(t('newAgent.unknown') + (err instanceof ApiError && typeof err.extra.workspace === 'string' && err.extra.workspace ? ' ' + t('newAgent.which', { w: err.extra.workspace }) : ''))
      setBusy(false)
    }
  }
  const close = () => !busy && onClose()
  return (
    <Dialog title={t('newAgent.title')} onClose={close}>
      <form className="new-agent" onSubmit={submit}>
        {herdrs.length > 1 && (
          <label className="field">
            <span>{t('newAgent.where')}</span>
            <select
              className="input"
              value={sid}
              onChange={(e) => {
                asked.n++
                setSid(e.target.value)
                setOlds(null)
                setResume('')
                setOldsBusy(false)
              }}
            >
              {herdrs.map((s) => (
                <option key={s.id} value={s.id}>
                  {s.host_name + (s.mux_session ? ' · ' + s.mux_session : '')}
                </option>
              ))}
            </select>
          </label>
        )}
        <label className="field">
          <span>{t('newAgent.dir')}</span>
          <input
            className="input"
            value={dir}
            list="new-agent-dirs"
            autoCapitalize="none"
            autoCorrect="off"
            spellCheck={false}
            onChange={(e) => {
              asked.n++
              setDir(e.target.value)
              setOlds(null)
              setResume('')
              setOldsBusy(false)
            }}
          />
          <datalist id="new-agent-dirs">
            {dirs.map((d) => (
              <option key={d} value={d} />
            ))}
          </datalist>
        </label>
        <div className="field">
          <span>{t('newAgent.kind')}</span>
          <div className="seg" role="radiogroup">
            {(['claude', 'codex', 'opencode'] as const).map((k) => (
              <button key={k} type="button" role="radio" aria-checked={kind === k} aria-pressed={kind === k} onClick={() => setKind(k)}>
                {t(`newAgent.kind.${k}`)}
              </button>
            ))}
          </div>
        </div>
        <label className="field">
          <span>{t('newAgent.label')}</span>
          <input className="input" value={label} maxLength={40} placeholder={t('newAgent.labelHint')} onChange={(e) => setLabel(e.target.value)} />
        </label>
        {kind === 'claude' && (
          <div className="field new-agent-old">
            {olds === null ? (
              <button type="button" className="link-btn" onClick={() => void listOld()} disabled={oldsBusy || !dir.trim()}>
                {t(oldsBusy ? 'common.loading' : 'newAgent.listOld')}
              </button>
            ) : olds.length === 0 ? (
              <p className="hint">{t('newAgent.noOld')}</p>
            ) : (
              <div className="new-agent-olds" role="radiogroup" aria-label={t('newAgent.old')}>
                <button type="button" role="radio" aria-checked={!resume} className={'new-agent-oldrow' + (!resume ? ' on' : '')} onClick={() => setResume('')}>
                  {t('newAgent.fresh')}
                </button>
                {olds.map((o) => {
                  const open = running.get(o.id)
                  return (
                    <button
                      key={o.id}
                      type="button"
                      role="radio"
                      aria-checked={resume === o.id}
                      className={'new-agent-oldrow' + (resume === o.id ? ' on' : '')}
                      onClick={() => (open && sess ? onOpen(sess, open) : setResume(o.id))}
                    >
                      <span className="new-agent-oldtitle">{o.title || o.first || o.id.slice(0, 8)}</span>
                      <span className="new-agent-oldwhen">{open ? t('newAgent.open') : new Date(o.modified).toLocaleString()}</span>
                    </button>
                  )
                })}
              </div>
            )}
          </div>
        )}
        {!resume && (
          <label className="field">
            <span>{t('newAgent.first')}</span>
            <textarea className="input" rows={3} value={first} placeholder={t('newAgent.firstHint')} onChange={(e) => setFirst(e.target.value)} />
          </label>
        )}
        {error && <p className="err">{error}</p>}
        <div className="dialog-actions">
          <button type="button" className="btn" onClick={close} disabled={busy}>
            {t('common.cancel')}
          </button>
          <button type="submit" className="btn btn-primary" disabled={busy || !dir.trim()}>
            {t(busy ? 'newAgent.starting' : resume ? 'newAgent.resume' : 'newAgent.start')}
          </button>
        </div>
      </form>
    </Dialog>
  )
}
