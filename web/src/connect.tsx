import { useEffect, useRef, useState, type FormEvent, type ReactNode } from 'react'
import { api, ApiError, choiceOf, errorText, type Host, type HostChoices, type OpenChoice, type SessionInfo } from './api'
import { hasKey, t } from './i18n'
import { Dialog, Field, useToast } from './components/ui'
import { OpenPicker, rememberChoice } from './pages/OpenPicker'

// Rough terminal size for the first frame, before xterm measures the real one.
export function estimateSize() {
  const cols = Math.max(40, Math.min(240, Math.floor(window.innerWidth / 8.4)))
  const rows = Math.max(12, Math.min(90, Math.floor((window.innerHeight - 100) / 17)))
  return { cols, rows }
}

// choice: what is being opened (none yet: the host is asked what it has).
type Pending =
  | { kind: 'password'; host: Host; accept?: string; choice?: OpenChoice }
  | { kind: 'hostkey'; host: Host; expected: string; got: string; password?: string; choice?: OpenChoice }
  | { kind: 'choose'; host: Host; found: HostChoices }
  | null

// useConnect opens a session to a host and resolves the interactive cases
// (password prompt, changed host key) with dialogs.
export function useConnect(onOpen: (s: SessionInfo) => void): {
  // With a choice, it is opened; without, the host is asked what it has.
  connect: (h: Host, choice?: OpenChoice) => void
  cancel: () => void // drops an attempt under way and its dialogs
  busyHost: number | null
  dialogs: ReactNode
} {
  const toast = useToast()
  const [busyHost, setBusyHost] = useState<number | null>(null)
  const [pending, setPending] = useState<Pending>(null)
  // Each attempt gets a number; cancelling bumps it so a late success is undone
  // instead of navigating the user somewhere they backed out of.
  const attempt = useRef(0)
  const inflight = useRef(false)
  // Leaving the page counts as cancelling.
  useEffect(() => () => {
    attempt.current++
  }, [])

  const cancel = () => {
    attempt.current++
    inflight.current = false
    setPending(null)
    setBusyHost(null)
  }

  const open = async (h: Host, password?: string, accept?: string, choice?: OpenChoice, token?: string): Promise<'done' | 'retry-password'> => {
    if (inflight.current) return 'done' // a second tap while connecting does nothing
    inflight.current = true
    const mine = ++attempt.current
    setBusyHost(h.id)
    try {
      // reused: an already running tmux/herdr session was returned, which
      // other devices may be using.
      const s = await api<(SessionInfo & { reused?: boolean }) | HostChoices>('POST', '/api/sessions', {
        host_id: h.id, ...estimateSize(), password, accept_fingerprint: accept, open: choice ?? null, pending: token,
      })
      if ('choose' in s) {
        // The host was asked what it has: the user picks (the connection is
        // kept a minute for it). Gone back meanwhile, it simply expires.
        if (mine === attempt.current) setPending({ kind: 'choose', host: h, found: s })
        return 'done'
      }
      if (mine !== attempt.current) {
        // Cancelled while connecting: close what was opened for this (never a
        // reused session). If that fails the session simply shows up in the
        // running list.
        if (!s.reused) void api('DELETE', '/api/sessions/' + s.id).catch(() => {})
        return 'done'
      }
      // Remembered as what was opened (a new tmux session by its name).
      const opened = choiceOf(s) ?? choice
      if (opened) rememberChoice(h.id, opened)
      setPending(null)
      onOpen(s)
      return 'done'
    } catch (e) {
      if (mine !== attempt.current) return 'done'
      if (e instanceof ApiError) {
        if (e.code === 'password_required') {
          setPending({ kind: 'password', host: h, accept, choice })
          return 'done'
        }
        if (e.code === 'host_key_changed') {
          setPending({ kind: 'hostkey', host: h, expected: String(e.extra.expected), got: String(e.extra.got), password, choice })
          return 'done'
        }
        if (e.code === 'auth_failed' && h.auth === 'ask') {
          toast(e.human, true)
          return 'retry-password'
        }
        const key = 'connect.reason.' + String(e.extra.reason ?? '')
        toast(hasKey(key) ? e.human + ' ' + t(key) : e.human, true)
      } else toast(errorText(e), true)
      setPending(null)
      return 'done'
    } finally {
      // Only the current attempt holds the lock; a cancelled one already released it.
      if (mine === attempt.current) {
        inflight.current = false
        setBusyHost(null)
      }
    }
  }

  let dialogs: ReactNode = null
  if (pending?.kind === 'password') {
    const p = pending
    dialogs = <PasswordDialog host={p.host} onClose={cancel} onSubmit={(pw) => open(p.host, pw, p.accept, p.choice)} />
  } else if (pending?.kind === 'hostkey') {
    const p = pending
    dialogs = (
      <Dialog title={t('hostkey.title')} onClose={cancel}>
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
          <button className="btn" onClick={cancel}>{t('common.cancel')}</button>
          <button
            className="btn btn-danger"
            disabled={busyHost === p.host.id}
            onClick={() => {
              // Retry with the exact fingerprint shown above: the gateway re-pins
              // only if the host presents that very key.
              if (p.host.auth === 'ask' && !p.password) setPending({ kind: 'password', host: p.host, accept: p.got, choice: p.choice })
              else void open(p.host, p.password, p.got, p.choice)
            }}
          >
            {t('hostkey.trust')}
          </button>
        </div>
      </Dialog>
    )
  } else if (pending?.kind === 'choose') {
    const p = pending
    dialogs = (
      <OpenPicker
        host={p.host}
        found={p.found}
        busy={busyHost === p.host.id}
        onClose={cancel}
        onPick={(choice) => void open(p.host, undefined, undefined, choice, p.found.pending)}
      />
    )
  }

  return { connect: (h, choice) => void open(h, undefined, undefined, choice), cancel, busyHost, dialogs }
}

function PasswordDialog(props: { host: Host; onClose: () => void; onSubmit: (pw: string) => Promise<'done' | 'retry-password'> }) {
  const [pw, setPw] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  const submit = async (e: FormEvent) => {
    e.preventDefault()
    if (busy) return
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
