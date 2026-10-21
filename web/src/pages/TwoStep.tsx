import { useEffect, useMemo, useRef, useState, type FormEvent } from 'react'
import { Copy, Download } from 'lucide-react'
import { encode } from 'uqr'
import { api, ApiError, errorText } from '../api'
import { t } from '../i18n'
import { useSession } from '../app'
import { Dialog, Field, LoadError, copyText, useToast } from '../components/ui'

type Status = { on: boolean; recovery_left: number }

// A pasted one-time code may come with spaces ("123 456"); recovery codes
// are passed on as typed (the server ignores case, spaces and dashes).
const normCode = (c: string) => (/^[\d\s]+$/.test(c) ? c.replace(/\s+/g, '') : c.trim())

// useStepFocus moves focus to the start of each step of a dialog, so a
// keyboard or screen reader user lands on what changed.
function useStepFocus(step: unknown) {
  const ref = useRef<HTMLParagraphElement>(null)
  const first = useRef(true)
  useEffect(() => {
    if (first.current) {
      first.current = false
      return
    }
    ref.current?.focus()
  }, [step])
  return ref
}

// Two-step sign-in, a row of the settings' account group: turn it on with
// an authenticator app, replace the recovery codes, or turn it off.
export function TwoStepSetting() {
  const { me, setMe } = useSession()
  const [status, setStatus] = useState<Status | null>(null)
  const [failed, setFailed] = useState(false)
  const [dialog, setDialog] = useState<'enrol' | 'recovery' | 'disable' | null>(null)
  const load = () => {
    setFailed(false)
    api<Status>('GET', '/api/me/totp')
      .then(setStatus)
      .catch(() => setFailed(true))
  }
  useEffect(load, [])
  const changed = (on: boolean) => {
    if (me) setMe({ ...me, totp: on })
    load()
  }
  return (
    <div className="setting setting-wide">
      <div className="setting-label">
        <span>{t('totp.title')}</span>
        {failed ? (
          <LoadError onRetry={load} />
        ) : status === null ? (
          <span className="hint">{t('common.loading')}</span>
        ) : status.on ? (
          <span className="hint">
            {t('totp.onLead')}
            {status.recovery_left <= 3 ? <span className="warn-text">{t('totp.fewLeft', { n: status.recovery_left })}</span> : t('totp.left', { n: status.recovery_left })}
          </span>
        ) : (
          <span className="hint">{t('totp.offLead')}</span>
        )}
      </div>
      {status && !failed && (
        <div className="setting-control">
          {status.on ? (
            <>
              <button className="btn" onClick={() => setDialog('recovery')}>{t('totp.newCodes')}</button>
              <button className="btn btn-ghost danger-text" aria-label={t('totp.turnOff')} onClick={() => setDialog('disable')}>{t('totp.off')}</button>
            </>
          ) : (
            <button className="btn" aria-label={t('totp.turnOn')} onClick={() => setDialog('enrol')}>{t('totp.on')}</button>
          )}
        </div>
      )}
      {dialog === 'enrol' && <EnrolDialog onClose={() => setDialog(null)} onDone={() => changed(true)} />}
      {dialog === 'recovery' && <ConfirmDialog kind="recovery" onClose={() => setDialog(null)} onDone={() => changed(true)} />}
      {dialog === 'disable' && <ConfirmDialog kind="disable" onClose={() => setDialog(null)} onDone={() => changed(false)} />}
    </div>
  )
}

function QRCode({ text, label }: { text: string; label: string }) {
  const path = useMemo(() => {
    const qr = encode(text, { ecc: 'M', border: 2 })
    let d = ''
    qr.data.forEach((row, y) => row.forEach((on, x) => on && (d += `M${x} ${y}h1v1h-1z`)))
    return { d, size: qr.size }
  }, [text])
  return (
    <svg className="qr" viewBox={`0 0 ${path.size} ${path.size}`} role="img" aria-label={label} shapeRendering="crispEdges">
      <rect width={path.size} height={path.size} fill="#fff" />
      <path d={path.d} fill="#000" />
    </svg>
  )
}

function RecoveryCodes({ codes, leadRef }: { codes: string[]; leadRef?: React.Ref<HTMLParagraphElement> }) {
  const toast = useToast()
  const text = codes.join('\n')
  const download = () => {
    const url = URL.createObjectURL(new Blob([t('totp.fileHeader') + '\n\n' + text + '\n'], { type: 'text/plain' }))
    const a = document.createElement('a')
    a.href = url
    a.download = 'corral-recovery-codes.txt'
    a.click()
    setTimeout(() => URL.revokeObjectURL(url), 1000)
  }
  return (
    <>
      <p className="lead" ref={leadRef} tabIndex={-1} role="status">{t('totp.codesLead')}</p>
      <ol className="recovery-codes mono">{codes.map((c) => <li key={c}>{c}</li>)}</ol>
      <div className="btn-row">
        <button
          type="button"
          className="btn"
          onClick={async () => {
            if (await copyText(text)) toast(t('common.copied'))
          }}
        >
          <Copy size={16} />
          {t('common.copy')}
        </button>
        <button type="button" className="btn" onClick={download}>
          <Download size={16} />
          {t('totp.download')}
        </button>
      </div>
    </>
  )
}

// Turning on: the password, then the QR code and a first code, then the
// recovery codes.
function EnrolDialog({ onClose, onDone }: { onClose: () => void; onDone: () => void }) {
  const toast = useToast()
  // Not closed while a request runs: its answer (the recovery codes) is
  // shown only once.
  const busyRef = useRef(false)
  const close = () => !busyRef.current && onClose()
  const [password, setPassword] = useState('')
  const [setup, setSetup] = useState<{ secret: string; uri: string } | null>(null)
  const [code, setCode] = useState('')
  const [codes, setCodes] = useState<string[] | null>(null)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  const run = async (f: () => Promise<void>) => {
    if (busyRef.current) return
    busyRef.current = true
    setBusy(true)
    setError('')
    try {
      await f()
    } catch (err) {
      // The set-up timed out (or used up its tries): back to the start.
      if (err instanceof ApiError && err.code === 'no_enrolment') {
        setSetup(null)
        setCode('')
      }
      setError(errorText(err))
    } finally {
      busyRef.current = false
      setBusy(false)
    }
  }
  const begin = (e: FormEvent) => {
    e.preventDefault()
    void run(async () => {
      setSetup(await api<{ secret: string; uri: string }>('POST', '/api/me/totp/begin', { password }))
      setPassword('')
    })
  }
  const enable = (e: FormEvent) => {
    e.preventDefault()
    void run(async () => {
      const r = await api<{ recovery_codes: string[] }>('POST', '/api/me/totp/enable', { code: normCode(code) })
      setCodes(r.recovery_codes)
      onDone()
    })
  }
  const stepRef = useStepFocus(codes ? 2 : setup ? 1 : 0)
  // Grouped in fours, as apps show it.
  const spaced = setup?.secret.replace(/(.{4})/g, '$1 ').trim() ?? ''
  return (
    <Dialog title={t('totp.turnOn')} onClose={close}>
      {codes ? (
        <>
          <RecoveryCodes codes={codes} leadRef={stepRef} />
          <div className="dialog-actions">
            <button className="btn btn-primary" onClick={onClose}>{t('totp.saved')}</button>
          </div>
        </>
      ) : setup ? (
        <form className="form" onSubmit={enable}>
          <p className="lead" ref={stepRef} tabIndex={-1}>{t('totp.scanLead')}</p>
          <div className="qr-wrap">
            <QRCode text={setup.uri} label={t('totp.qrLabel')} />
          </div>
          <div className="totp-secret">
            <span className="hint">{t('totp.manual')}</span>
            <button
              type="button"
              className="link-btn mono"
              onClick={async () => {
                if (await copyText(setup.secret)) toast(t('common.copied'))
              }}
            >
              {spaced}
            </button>
          </div>
          <Field label={t('login.code')} htmlFor="totp-code">
            <input id="totp-code" className="input mono code-input" inputMode="numeric" autoComplete="one-time-code" value={code}
              onChange={(e) => setCode(e.target.value.replace(/\D/g, '').slice(0, 6))} required data-touch-nofocus />
          </Field>
          {error && <p className="err" role="alert">{error}</p>}
          <div className="dialog-actions">
            <button type="button" className="btn" onClick={close} disabled={busy}>{t('common.cancel')}</button>
            <button className="btn btn-primary" disabled={busy || code.length !== 6}>{t('totp.confirm')}</button>
          </div>
        </form>
      ) : (
        <form className="form" onSubmit={begin}>
          <p className="lead" ref={stepRef} tabIndex={-1}>{t('totp.passwordLead')}</p>
          <Field label={t('login.password')} htmlFor="totp-pass">
            <input id="totp-pass" className="input" type="password" autoComplete="current-password" value={password} onChange={(e) => setPassword(e.target.value)} required />
          </Field>
          {error && <p className="err" role="alert">{error}</p>}
          <div className="dialog-actions">
            <button type="button" className="btn" onClick={close} disabled={busy}>{t('common.cancel')}</button>
            <button className="btn btn-primary" disabled={busy || !password}>{t('totp.next')}</button>
          </div>
        </form>
      )}
    </Dialog>
  )
}

// Replacing the recovery codes or turning off: the password and a code.
function ConfirmDialog({ kind, onClose, onDone }: { kind: 'recovery' | 'disable'; onClose: () => void; onDone: () => void }) {
  const toast = useToast()
  const busyRef = useRef(false) // see EnrolDialog
  const close = () => !busyRef.current && onClose()
  const [password, setPassword] = useState('')
  const [code, setCode] = useState('')
  const [codes, setCodes] = useState<string[] | null>(null)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  const submit = async (e: FormEvent) => {
    e.preventDefault()
    if (busyRef.current) return
    busyRef.current = true
    setBusy(true)
    setError('')
    try {
      if (kind === 'recovery') {
        const r = await api<{ recovery_codes: string[] }>('POST', '/api/me/totp/recovery', { password, code: normCode(code) })
        setCodes(r.recovery_codes)
        onDone()
      } else {
        await api('POST', '/api/me/totp/disable', { password, code: normCode(code) })
        toast(t('totp.offDone'))
        onDone()
        busyRef.current = false
        onClose()
        return
      }
    } catch (err) {
      setError(errorText(err))
    }
    busyRef.current = false
    setBusy(false)
  }
  const stepRef = useStepFocus(!!codes)
  const title = kind === 'recovery' ? t('totp.newCodes') : t('totp.turnOff')
  return (
    <Dialog title={title} onClose={close}>
      {codes ? (
        <>
          <RecoveryCodes codes={codes} leadRef={stepRef} />
          <div className="dialog-actions">
            <button className="btn btn-primary" onClick={onClose}>{t('totp.saved')}</button>
          </div>
        </>
      ) : (
        <form className="form" onSubmit={submit}>
          <p className="lead">{kind === 'recovery' ? t('totp.newCodesLead') : t('totp.offConfirmLead')}</p>
          <Field label={t('login.password')} htmlFor="totp-pass">
            <input id="totp-pass" className="input" type="password" autoComplete="current-password" value={password} onChange={(e) => setPassword(e.target.value)} required />
          </Field>
          <Field label={t('totp.codeOrRecovery')} htmlFor="totp-code">
            <input id="totp-code" className="input mono" autoComplete="one-time-code" autoCapitalize="none" autoCorrect="off" spellCheck={false} value={code}
              onChange={(e) => setCode(e.target.value)} required />
          </Field>
          {error && <p className="err" role="alert">{error}</p>}
          <div className="dialog-actions">
            <button type="button" className="btn" onClick={close} disabled={busy}>{t('common.cancel')}</button>
            <button className={'btn ' + (kind === 'disable' ? 'btn-danger' : 'btn-primary')} disabled={busy || !password || !code.trim()}>{title}</button>
          </div>
        </form>
      )}
    </Dialog>
  )
}
