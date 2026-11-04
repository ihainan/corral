import { useState, type FormEvent } from 'react'
import { api, ApiError, errorText } from '../api'
import { t } from '../i18n'
import { Dialog } from '../components/ui'

// What the conversation page does to an agent's place rather than to its
// conversation: a copy of the conversation started in a workspace of its
// own (fork), and the names herdr shows (its workspace, tab, pane).

// Forking: of all of it, or of it up to the end of one turn (at: the next
// message the person typed, cut before; Claude Code's only). The copy is
// started as a new agent is (the gateway's apiNewAgent, fork): in the
// folder the conversation began in, its first message waiting in its
// composer (not on the agent's command line, where it could be taken for
// one of its options).
export function ForkDialog({
  sessionId,
  kind,
  source,
  at,
  dir,
  name,
  check,
  onDone,
  onClose,
}: {
  sessionId: string
  kind: 'claude' | 'codex'
  source: string
  at?: string
  dir: string
  name: string
  check?: () => string // (asked again as it is sent: why not now, or '')
  onDone: (pane: string, first: string) => void
  onClose: () => void
}) {
  const [label, setLabel] = useState(() => t('fork.defaultName', { name }).slice(0, 40))
  const [first, setFirst] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  // (One key for one thing asked: said again unchanged, the same copy.)
  const keyFor = useState(() => ({ sig: '', key: '' }))[0]
  const keyOf = (sig: string) => {
    if (keyFor.sig !== sig) {
      keyFor.sig = sig
      keyFor.key = crypto.randomUUID()
    }
    return keyFor.key
  }
  const submit = async (e?: FormEvent) => {
    e?.preventDefault()
    if (busy) return
    const no = check?.()
    if (no) return setError(no)
    setBusy(true)
    setError('')
    try {
      const body = { dir, kind, label: label.trim(), resume: source, fork: true, at: at ?? '' }
      const r = await api<{ pane: string }>('POST', `/api/sessions/${sessionId}/agents`, { ...body, key: keyOf(JSON.stringify(body)) }, { timeoutMs: 90000 })
      onDone(r.pane, first.trim())
    } catch (err) {
      if (err instanceof ApiError && err.code === 'no_session') setError(t('fork.noSession'))
      else if (err instanceof ApiError && err.code === 'bad_message') setError(t('fork.badMessage'))
      else if (err instanceof ApiError && err.code === 'start_failed') setError(t('newAgent.failed', { why: err.message }) + (err.extra.closed === false ? ' ' + t('newAgent.notClosed') : ''))
      else if (err instanceof ApiError && (err.code === 'busy' || err.code === 'in_progress')) setError(t('newAgent.busy'))
      else if (err instanceof ApiError && err.code === 'unknown') setError(t('newAgent.unknown') + (typeof err.extra.workspace === 'string' && err.extra.workspace ? ' ' + t('newAgent.which', { w: err.extra.workspace }) : ''))
      else setError(errorText(err))
      setBusy(false)
    }
  }
  const close = () => !busy && onClose()
  return (
    <Dialog title={t(at ? 'fork.titleAt' : 'fork.title')} onClose={close}>
      <form className="form" onSubmit={submit}>
        <p className="lead">{t(at ? 'fork.bodyAt' : 'fork.body')}</p>
        <p className="hint">{t('fork.shared')}</p>
        <label className="field">
          <span>{t('newAgent.label')}</span>
          <input className="input" value={label} maxLength={40} autoComplete="off" onChange={(e) => setLabel(e.target.value)} />
        </label>
        <label className="field">
          <span>{t('fork.first')}</span>
          <textarea className="input" rows={3} value={first} placeholder={t('fork.firstHint')} onChange={(e) => setFirst(e.target.value)} />
        </label>
        {error && <p className="err">{error}</p>}
        <div className="dialog-actions">
          <button type="button" className="btn" onClick={close} disabled={busy}>
            {t('common.cancel')}
          </button>
          <button type="submit" className="btn btn-primary" disabled={busy}>
            {t(busy ? 'fork.busy' : 'fork.confirm')}
          </button>
        </div>
      </form>
    </Dialog>
  )
}

// The names herdr shows: its workspace's and its tab's (a name each must
// have), the pane's own (none: herdr's own again). Only what was changed is
// sent, each by the id it had when this was opened.
export type RenameTarget = {
  sessionId: string
  ws: { id: string; label: string }
  tab?: { id: string; label: string }
  pane: { id: string; label: string }
}

export function RenameDialog({ target, onClose }: { target: RenameTarget; onClose: () => void }) {
  const [ws, setWs] = useState(target.ws.label === '~' ? '' : target.ws.label)
  const [tab, setTab] = useState(target.tab?.label ?? '')
  const [pane, setPane] = useState(target.pane.label)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  const clean = (s: string) => s.replace(/\s+/g, ' ').trim()
  const changes: { action: string; target: string; label: string }[] = []
  if (clean(ws) !== clean(target.ws.label === '~' ? '' : target.ws.label)) changes.push({ action: 'rename_workspace', target: target.ws.id, label: clean(ws) })
  if (target.tab && clean(tab) !== clean(target.tab.label)) changes.push({ action: 'rename_tab', target: target.tab.id, label: clean(tab) })
  if (clean(pane) !== clean(target.pane.label)) changes.push({ action: 'rename_pane', target: target.pane.id, label: clean(pane) })
  // (A name required only of what is changed: a pane named alone, its
  // workspace's unnamed as it was — review585-12.)
  const ready = changes.length > 0 && changes.every((c) => c.action === 'rename_pane' || !!c.label)
  const submit = async (e?: FormEvent) => {
    e?.preventDefault()
    if (!ready || busy) return
    setBusy(true)
    setError('')
    // (One after another: herdr answers one request per connection, and a
    // name taken is not undone if a later one fails — what failed is said.)
    for (const c of changes) {
      try {
        await api('POST', `/api/sessions/${target.sessionId}/herdr`, c)
      } catch (err) {
        setError(t('rename.failed', { what: t(`rename.${c.action}` as 'rename.rename_workspace'), why: err instanceof ApiError && err.code === 'herdr_busy' ? t('herdr.busy') : errorText(err) }))
        setBusy(false)
        return
      }
    }
    onClose()
  }
  const close = () => !busy && onClose()
  return (
    <Dialog title={t('rename.title')} onClose={close}>
      <form className="form" onSubmit={submit}>
        <label className="field">
          <span>{t('rename.workspace')}</span>
          <input className="input" value={ws} maxLength={40} autoFocus autoComplete="off" onChange={(e) => setWs(e.target.value)} />
        </label>
        {target.tab && (
          <label className="field">
            <span>{t('rename.tab')}</span>
            <input className="input" value={tab} maxLength={40} autoComplete="off" onChange={(e) => setTab(e.target.value)} />
          </label>
        )}
        <label className="field">
          <span>{t('rename.pane')}</span>
          <input className="input" value={pane} maxLength={40} autoComplete="off" placeholder={t('rename.paneHint')} onChange={(e) => setPane(e.target.value)} />
        </label>
        <p className="hint">{t('rename.hint')}</p>
        {error && <p className="err">{error}</p>}
        <div className="dialog-actions">
          <button type="button" className="btn" onClick={close} disabled={busy}>
            {t('common.cancel')}
          </button>
          <button type="submit" className="btn btn-primary" disabled={busy || !ready}>
            {t('rename.confirm')}
          </button>
        </div>
      </form>
    </Dialog>
  )
}
