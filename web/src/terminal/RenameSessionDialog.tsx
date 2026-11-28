import { useState, type FormEvent } from 'react'
import { TextCursorInput } from 'lucide-react'
import { api, ApiError, choiceOf, errorText, type SessionInfo } from '../api'
import { t } from '../i18n'
import { local } from '../storage'
import { Dialog, Field, useToast } from '../components/ui'
import { notifySessionRenamed, validateDisplaySessionName, validateMuxSessionName } from './sessionRenamePolicy'

function initialNameOf(s: SessionInfo): string {
  if (s.persistence === 'tmux') return s.mux_session ?? ''
  if (s.display_name) return s.display_name
  if (s.persistence === 'herdr') return s.mux_session && s.mux_session !== 'default' ? s.mux_session : ''
  return ''
}

export function RenameSessionDialog({
  session,
  onRenamed,
  onClose,
}: {
  session: SessionInfo
  onRenamed?: (updated: SessionInfo) => void
  onClose: () => void
}) {
  const toast = useToast()
  const isTmux = session.persistence === 'tmux'
  const isHerdr = session.persistence === 'herdr'
  const [name, setName] = useState(() => initialNameOf(session))
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')

  const validate = (raw: string): string => {
    const err = isTmux ? validateMuxSessionName(raw) : validateDisplaySessionName(raw)
    if (!err) return ''
    if (err === 'empty') return t('sessionRename.empty')
    if (err === 'too_long') return t('sessionRename.tooLong')
    return t(isTmux ? 'sessionRename.invalidMux' : 'sessionRename.invalidDisplay')
  }

  const submit = async (e: FormEvent) => {
    e.preventDefault()
    if (busy) return
    const value = isTmux ? name : name.trim()
    const bad = validate(value)
    if (bad) {
      setError(bad)
      return
    }
    setBusy(true)
    setError('')
    try {
      const updated = await api<SessionInfo>('PUT', `/api/sessions/${encodeURIComponent(session.id)}/name`, { name: value })
      const c = choiceOf(updated)
      if (c) local.set('corral.sessOpen.' + updated.id, JSON.stringify(c))
      // Update OpenPicker's remembered choice if it pointed at the old tmux session.
      if (isTmux && session.mux_session && updated.mux_session && session.mux_session !== updated.mux_session) {
        const lastKey = 'corral.lastOpen.' + session.host_id
        try {
          const prev = JSON.parse(local.get(lastKey) ?? 'null') as { kind?: string; name?: string } | null
          if (prev?.kind === 'tmux' && prev.name === session.mux_session) {
            local.set(lastKey, JSON.stringify({ kind: 'tmux', name: updated.mux_session }))
          }
        } catch {
          /* ignore */
        }
      }
      notifySessionRenamed({
        id: updated.id,
        host_id: updated.host_id,
        persistence: updated.persistence,
        oldMuxSession: session.mux_session ?? '',
        newMuxSession: updated.mux_session ?? '',
        oldDisplayName: session.display_name ?? '',
        newDisplayName: updated.display_name ?? '',
      })
      onRenamed?.(updated)
      toast(t('sessionRename.done'))
      onClose()
    } catch (err) {
      if (err instanceof ApiError && err.code === 'session_name_taken') setError(t('error.session_name_taken'))
      else if (err instanceof ApiError && err.code === 'invalid_session_name') setError(t(isTmux ? 'sessionRename.invalidMux' : 'sessionRename.invalidDisplay'))
      else if (err instanceof ApiError && err.code === 'session_not_found') setError(t('error.session_not_found'))
      else setError(errorText(err))
      setBusy(false)
    }
  }

  return (
    <Dialog title={t('sessionRename.title')} onClose={() => !busy && onClose()} icon={<TextCursorInput size={18} />}>
      <form className="form" onSubmit={submit}>
        {isHerdr && <p className="hint">{t('sessionRename.herdrNote')}</p>}
        <Field
          label={t('sessionRename.label')}
          htmlFor="session-rename-input"
          hint={isTmux ? t('sessionRename.muxHint') : undefined}
          error={error}
        >
          <input
            id="session-rename-input"
            className="input"
            type="text"
            maxLength={64}
            autoComplete="off"
            spellCheck={false}
            autoFocus
            value={name}
            disabled={busy}
            placeholder={t('sessionRename.placeholder')}
            onChange={(e) => {
              setName(e.target.value)
              if (error) setError('')
            }}
          />
        </Field>
        <div className="dialog-actions">
          <button type="button" className="btn" disabled={busy} onClick={onClose}>
            {t('common.cancel')}
          </button>
          <button type="submit" className="btn btn-primary" disabled={busy || !name.trim()}>
            {t(busy ? 'common.saving' : 'common.save')}
          </button>
        </div>
      </form>
    </Dialog>
  )
}
