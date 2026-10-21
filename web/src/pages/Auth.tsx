import { useEffect, useRef, useState, type FormEvent } from 'react'
import { api, ApiError, errorText, type Me } from '../api'
import { t } from '../i18n'
import { navigate } from '../router'
import { syncPrefsFromServer } from '../prefs'
import { useSession } from '../app'
import { BrandMark, Field } from '../components/ui'

function AuthFrame({ title, intro, children }: { title: string; intro?: string; children: React.ReactNode }) {
  return (
    <div className="auth-wrap">
      <main className="auth">
        <div className="brand">
          <BrandMark />
          {t('app.name')}
        </div>
        <div className="form">
          <h1>{title}</h1>
          {intro && <p className="lead">{intro}</p>}
          {children}
        </div>
      </main>
    </div>
  )
}

function nextPath() {
  const n = new URLSearchParams(location.search).get('next')
  return n && n.startsWith('/') && !n.startsWith('//') ? n : '/'
}

export function LoginPage() {
  const { setMe } = useSession()
  const [username, setUsername] = useState('')
  const [password, setPassword] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  const [challenge, setChallenge] = useState('') // set once the password was right and a code is due
  const signedIn = (user: Me) => {
    setMe(user)
    syncPrefsFromServer(user.id)
    navigate(nextPath(), true)
  }
  const submit = async (e: FormEvent) => {
    e.preventDefault()
    setBusy(true)
    setError('')
    try {
      const r = await api<{ user?: Me; code_required?: boolean; token?: string }>('POST', '/api/login', { username, password })
      if (r.code_required && r.token) {
        setPassword('')
        setChallenge(r.token)
        setBusy(false)
        return
      }
      signedIn(r.user!)
    } catch (err) {
      setError(errorText(err))
      setBusy(false)
    }
  }
  if (challenge) {
    return (
      <CodeStep
        token={challenge}
        onSignedIn={signedIn}
        onRestart={(why) => {
          setChallenge('')
          setError(why)
        }}
      />
    )
  }
  return (
    <AuthFrame title={t('login.title')}>
      <form className="form" onSubmit={submit}>
        <Field label={t('login.username')} htmlFor="login-user">
          <input id="login-user" className="input" autoComplete="username" autoCapitalize="none" autoCorrect="off" spellCheck={false}
            value={username} onChange={(e) => setUsername(e.target.value)} required />
        </Field>
        <Field label={t('login.password')} htmlFor="login-pass">
          <input id="login-pass" className="input" type="password" autoComplete="current-password" value={password}
            onChange={(e) => setPassword(e.target.value)} required />
        </Field>
        {error && <p className="err" role="alert">{error}</p>}
        <button className="btn btn-primary btn-block" disabled={busy}>
          {busy ? t('login.submitting') : t('login.submit')}
        </button>
      </form>
    </AuthFrame>
  )
}

// The second step of signing in: a code from the authenticator app, or a
// recovery code.
function CodeStep({ token, onSignedIn, onRestart }: { token: string; onSignedIn: (u: Me) => void; onRestart: (why: string) => void }) {
  // An answer that arrives after going back (this step is gone) is ignored.
  const live = useRef(true)
  useEffect(() => {
    live.current = true
    return () => void (live.current = false)
  }, [])
  const [code, setCode] = useState('')
  const [recovery, setRecovery] = useState(false)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  const submit = async (e?: FormEvent) => {
    e?.preventDefault()
    if (busy) return
    setBusy(true)
    setError('')
    try {
      const r = await api<{ user: Me }>('POST', '/api/login/code', { token, code: recovery ? code : code.replace(/\s+/g, '') })
      if (live.current) onSignedIn(r.user)
    } catch (err) {
      if (!live.current) return
      if (err instanceof ApiError && (err.code === 'mfa_expired' || err.code === 'locked')) return onRestart(errorText(err))
      setError(errorText(err))
      setCode('')
      setBusy(false)
    }
  }
  return (
    <AuthFrame title={t('login.codeTitle')} intro={recovery ? t('login.recoveryIntro') : t('login.codeIntro')}>
      <form className="form" onSubmit={submit}>
        {recovery ? (
          <Field label={t('login.recoveryCode')} htmlFor="login-code">
            <input id="login-code" key="recovery" className="input mono" autoComplete="off" autoCapitalize="none" autoCorrect="off" spellCheck={false}
              value={code} onChange={(e) => setCode(e.target.value)} required autoFocus />
          </Field>
        ) : (
          <Field label={t('login.code')} htmlFor="login-code">
            <input id="login-code" key="totp" className="input mono code-input" inputMode="numeric" autoComplete="one-time-code" pattern="[0-9 ]*"
              value={code} required autoFocus
              onChange={(e) => {
                const v = e.target.value.replace(/\D/g, '').slice(0, 6)
                setCode(v)
              }} />
          </Field>
        )}
        {error && <p className="err" role="alert">{error}</p>}
        <button className="btn btn-primary btn-block" disabled={busy || (!recovery && code.length !== 6) || !code}>
          {busy ? t('login.submitting') : t('login.verify')}
        </button>
        <div className="auth-links">
          <button type="button" className="link-btn" disabled={busy} onClick={() => { setRecovery(!recovery); setCode(''); setError('') }}>
            {recovery ? t('login.useApp') : t('login.useRecovery')}
          </button>
          <button type="button" className="link-btn" disabled={busy} onClick={() => onRestart('')}>{t('login.back')}</button>
        </div>
      </form>
    </AuthFrame>
  )
}

function AccountForm(props: { submitLabel: string; onSubmit: (v: { username: string; display_name: string; password: string }) => Promise<void> }) {
  const [username, setUsername] = useState('')
  const [display, setDisplay] = useState('')
  const [password, setPassword] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  const [field, setField] = useState('')
  const submit = async (e: FormEvent) => {
    e.preventDefault()
    setBusy(true)
    setError('')
    try {
      await props.onSubmit({ username, display_name: display, password })
    } catch (err) {
      setError(errorText(err))
      setField(err instanceof ApiError ? err.code : '')
      setBusy(false)
    }
  }
  return (
    <form className="form" onSubmit={submit}>
      <Field label={t('login.username')} htmlFor="acct-user" error={field === 'bad_username' || field === 'username_taken' ? error : undefined}>
        <input id="acct-user" className="input" autoComplete="username" autoCapitalize="none" autoCorrect="off" spellCheck={false}
          value={username} onChange={(e) => setUsername(e.target.value)} required aria-invalid={field === 'bad_username' || field === 'username_taken'} />
      </Field>
      <Field label={t('setup.displayName')} htmlFor="acct-display" optional>
        <input id="acct-display" className="input" autoComplete="name" value={display} onChange={(e) => setDisplay(e.target.value)} />
      </Field>
      <Field label={t('login.password')} htmlFor="acct-pass" hint={t('setup.passwordHint')} error={field === 'weak_password' ? error : undefined}>
        <input id="acct-pass" className="input" type="password" autoComplete="new-password" minLength={8} value={password}
          onChange={(e) => setPassword(e.target.value)} required aria-invalid={field === 'weak_password'} />
      </Field>
      {error && !['bad_username', 'username_taken', 'weak_password'].includes(field) && <p className="err" role="alert">{error}</p>}
      <button className="btn btn-primary btn-block" disabled={busy}>{props.submitLabel}</button>
    </form>
  )
}

export function SetupPage() {
  const { setMe } = useSession()
  // The token travels in the URL fragment so it never reaches proxies or access logs.
  const token = location.hash.slice(1)
  if (!token) return <AuthFrame title={t('setup.title')} intro={t('setup.noToken')}>{null}</AuthFrame>
  return (
    <AuthFrame title={t('setup.title')} intro={t('setup.intro')}>
      <AccountForm
        submitLabel={t('setup.submit')}
        onSubmit={async (v) => {
          const r = await api<{ user: Me }>('POST', '/api/setup', { token, ...v })
          setMe(r.user)
          history.replaceState(null, '', '/setup')
          location.replace('/')
        }}
      />
    </AuthFrame>
  )
}

export function InvitePage({ token }: { token: string }) {
  const { setMe } = useSession()
  // failed: not answered (the link may be fine), unlike bad.
  const [state, setState] = useState<'checking' | 'ok' | 'bad' | 'failed'>('checking')
  const [role, setRole] = useState('')
  const [tries, setTries] = useState(0)
  useEffect(() => {
    setState('checking')
    api<{ role: string }>('GET', '/api/invites/' + encodeURIComponent(token), undefined, { timeoutMs: 20000 })
      .then((r) => {
        setRole(r.role)
        setState('ok')
      })
      .catch((e) => setState(e instanceof ApiError && e.code === 'bad_invite' ? 'bad' : 'failed'))
  }, [token, tries])
  if (state === 'checking') return <AuthFrame title={t('invite.title')} intro={t('invite.checking')}>{null}</AuthFrame>
  if (state === 'bad') return <AuthFrame title={t('invite.title')} intro={t('error.bad_invite')}>{null}</AuthFrame>
  if (state === 'failed')
    return (
      <AuthFrame title={t('invite.title')} intro={t('error.network')}>
        <button className="btn" onClick={() => setTries((n) => n + 1)}>{t('common.retry')}</button>
      </AuthFrame>
    )
  return (
    <AuthFrame title={t('invite.title')} intro={t('invite.intro') + (role === 'admin' ? ' ' + t('invite.roleAdmin') : '')}>
      <AccountForm
        submitLabel={t('invite.submit')}
        onSubmit={async (v) => {
          const r = await api<{ user: Me }>('POST', '/api/invites/' + encodeURIComponent(token) + '/redeem', v)
          setMe(r.user)
          syncPrefsFromServer(r.user.id)
          navigate('/', true)
        }}
      />
    </AuthFrame>
  )
}
