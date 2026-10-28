import { useState, type FormEvent } from 'react'
import { ApiError, errorText } from '../api'
import { t } from '../i18n'
import { Dialog } from '../components/ui'

// Claude Code's own commands on its session, from the conversation's menu:
// compact it (with what to keep, if said), rename it, start anew (/clear).
// Sent only while it is idle (the gateway checks on the host).
export type SessionOp = 'compact' | 'rename' | 'clear'

export function SessionOpDialog({ op, name, onSend, onClose, onShowTerminal }: { op: SessionOp; name?: string; onSend: (text: string) => Promise<unknown>; onClose: () => void; onShowTerminal: () => void }) {
  const [text, setText] = useState(op === 'rename' ? (name ?? '') : '')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  // (Typed into its input, not sent: what is there now is for the terminal to see to, not to be typed again.)
  const [left, setLeft] = useState(false)
  const line = text.replace(/\s+/g, ' ').trim()
  const command = op === 'compact' ? (line ? '/compact ' + line : '/compact') : op === 'rename' ? '/rename ' + line : '/clear'
  const ready = op !== 'rename' || !!line
  const submit = async (e?: FormEvent) => {
    e?.preventDefault()
    if (!ready || busy || left) return
    setBusy(true)
    setError('')
    try {
      await onSend(command)
      onClose()
    } catch (err) {
      // (Refused before anything was typed: tried again at will; else what
      // became of it is for the terminal to tell.)
      const refused = err instanceof ApiError && err.code !== 'typed_not_sent' && ([400, 403, 404, 409, 429].includes(err.status) || (err.status === 502 && (err.code === 'host_error' || err.code === 'send_failed')))
      if (!refused) {
        setLeft(true)
        setError(t(err instanceof ApiError && err.code === 'typed_not_sent' ? 'ops.typedNotSent' : 'ops.unsure'))
        setBusy(false)
        return
      }
      setError(err instanceof ApiError && err.code === 'agent_busy' ? t('ops.notIdle') : err instanceof ApiError && err.code === 'agent_waiting' ? t('ops.waits') : errorText(err))
      setBusy(false)
    }
  }
  const close = () => !busy && onClose()
  return (
    <Dialog title={t(`ops.${op}.title`)} onClose={close}>
      <form onSubmit={submit}>
        <p className="lead">{t(`ops.${op}.body`)}</p>
        {op !== 'clear' && (
          <input
            className="input"
            value={text}
            maxLength={op === 'rename' ? 80 : 500}
            placeholder={t(`ops.${op}.placeholder`)}
            aria-label={t(`ops.${op}.placeholder`)}
            autoFocus
            onChange={(e) => setText(e.target.value)}
          />
        )}
        {error && <p className="err">{error}</p>}
        <div className="dialog-actions">
          <button type="button" className="btn" onClick={close} disabled={busy}>
            {t('common.cancel')}
          </button>
          {left ? (
            <button type="button" className="btn btn-primary" onClick={onShowTerminal}>
              {t('convo.showTerminal')}
            </button>
          ) : (
            <button type="submit" className={'btn ' + (op === 'clear' ? 'btn-danger' : 'btn-primary')} disabled={busy || !ready}>
              {t(`ops.${op}.confirm`)}
            </button>
          )}
        </div>
      </form>
    </Dialog>
  )
}
