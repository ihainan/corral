import { useState, type FormEvent, type ReactNode } from 'react'
import { api, ApiError, errorText, type Host, type SessionInfo } from './api'
import { t } from './i18n'
import { Dialog, Field, useToast } from './components/ui'

// Rough terminal size for the first frame, before xterm measures the real one.
function estimateSize() {
  const cols = Math.max(40, Math.min(240, Math.floor(window.innerWidth / 8.4)))
  const rows = Math.max(12, Math.min(90, Math.floor((window.innerHeight - 100) / 17)))
  return { cols, rows }
}

type Pending =
  | { kind: 'password'; host: Host }
  | { kind: 'hostkey'; host: Host; expected: string; got: string; password?: string }
  | null

// useConnect opens a session to a host and resolves the interactive cases
// (password prompt, changed host key) with dialogs.
export function useConnect(onOpen: (s: SessionInfo) => void): {
  connect: (h: Host) => void
  busyHost: number | null
  dialogs: ReactNode
} {
  const toast = useToast()
  const [busyHost, setBusyHost] = useState<number | null>(null)
  const [pending, setPending] = useState<Pending>(null)

  const open = async (h: Host, password?: string): Promise<'done' | 'retry-password'> => {
    setBusyHost(h.id)
    try {
      const s = await api<SessionInfo>('POST', '/api/sessions', { host_id: h.id, ...estimateSize(), password })
      setPending(null)
      onOpen(s)
      return 'done'
    } catch (e) {
      if (e instanceof ApiError) {
        if (e.code === 'password_required') {
          setPending({ kind: 'password', host: h })
          return 'done'
        }
        if (e.code === 'host_key_changed') {
          setPending({ kind: 'hostkey', host: h, expected: String(e.extra.expected), got: String(e.extra.got), password })
          return 'done'
        }
        if (e.code === 'auth_failed' && h.auth === 'ask') {
          toast(e.human, true)
          return 'retry-password'
        }
        const detail = typeof e.extra.detail === 'string' ? `（${e.extra.detail}）` : ''
        toast(e.human + detail, true)
      } else toast(errorText(e), true)
      setPending(null)
      return 'done'
    } finally {
      setBusyHost(null)
    }
  }

  let dialogs: ReactNode = null
  if (pending?.kind === 'password') {
    dialogs = <PasswordDialog host={pending.host} onClose={() => setPending(null)} onSubmit={(pw) => open(pending.host, pw)} />
  } else if (pending?.kind === 'hostkey') {
    const p = pending
    dialogs = (
      <Dialog title={t('hostkey.title')} onClose={() => setPending(null)}>
        <p className="lead">{t('hostkey.body', { name: p.host.name })}</p>
        <div className="field">
          <span className="field-label">{t('hostkey.expected')}</span>
          <code className="mono" style={{ wordBreak: 'break-all' }}>{p.expected}</code>
        </div>
        <div className="field">
          <span className="field-label">{t('hostkey.got')}</span>
          <code className="mono" style={{ wordBreak: 'break-all' }}>{p.got}</code>
        </div>
        <div className="dialog-actions">
          <button className="btn" onClick={() => setPending(null)}>{t('common.cancel')}</button>
          <button
            className="btn btn-danger"
            onClick={async () => {
              await api('DELETE', `/api/hosts/${p.host.id}/hostkey`).catch(() => {})
              if (p.host.auth === 'ask' && !p.password) setPending({ kind: 'password', host: p.host })
              else void open(p.host, p.password)
            }}
          >
            {t('hostkey.trust')}
          </button>
        </div>
      </Dialog>
    )
  }

  return { connect: (h) => void open(h), busyHost, dialogs }
}

function PasswordDialog(props: { host: Host; onClose: () => void; onSubmit: (pw: string) => Promise<'done' | 'retry-password'> }) {
  const [pw, setPw] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  const submit = async (e: FormEvent) => {
    e.preventDefault()
    setBusy(true)
    setError('')
    const r = await props.onSubmit(pw)
    if (r === 'retry-password') {
      setError(t('error.auth_failed'))
      setBusy(false)
      setPw('')
    }
  }
  const target = `${props.host.username}@${props.host.address}`
  return (
    <Dialog title={t('connect.passwordTitle')} onClose={props.onClose}>
      <form className="form" onSubmit={submit}>
        <p className="lead">{t('connect.passwordBody', { target })}</p>
        <Field label={t('host.password')} htmlFor="connect-pw" error={error}>
          <input id="connect-pw" className="input" type="password" autoComplete="off" value={pw} onChange={(e) => setPw(e.target.value)} required />
        </Field>
        <div className="dialog-actions">
          <button type="button" className="btn" onClick={props.onClose}>{t('common.cancel')}</button>
          <button className="btn btn-primary" disabled={busy || !pw}>{busy ? t('hosts.connecting') : t('connect.submit')}</button>
        </div>
      </form>
    </Dialog>
  )
}
