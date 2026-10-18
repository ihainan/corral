import { useEffect, useState, type FormEvent } from 'react'
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
  const submit = async (e: FormEvent) => {
    e.preventDefault()
    setBusy(true)
    setError('')
    try {
      const r = await api<{ user: Me }>('POST', '/api/login', { username, password })
      setMe(r.user)
      syncPrefsFromServer(r.user.id)
      navigate(nextPath(), true)
    } catch (err) {
      setError(errorText(err))
      setBusy(false)
    }
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
  const [state, setState] = useState<'checking' | 'ok' | 'bad'>('checking')
  const [role, setRole] = useState('')
  useEffect(() => {
    api<{ role: string }>('GET', '/api/invites/' + encodeURIComponent(token))
      .then((r) => {
        setRole(r.role)
        setState('ok')
      })
      .catch(() => setState('bad'))
  }, [token])
  if (state === 'checking') return <AuthFrame title={t('invite.title')} intro={t('invite.checking')}>{null}</AuthFrame>
  if (state === 'bad') return <AuthFrame title={t('invite.title')} intro={t('error.bad_invite')}>{null}</AuthFrame>
  return (
    <AuthFrame title={t('invite.title')} intro={t('invite.intro') + (role === 'admin' ? ' ' + t('invite.roleAdmin') : '')}>
      <AccountForm
        submitLabel={t('invite.submit')}
        onSubmit={async (v) => {
          const r = await api<{ user: Me }>('POST', '/api/invites/' + encodeURIComponent(token) + '/redeem', v)
          setMe(r.user)
          navigate('/', true)
        }}
      />
    </AuthFrame>
  )
}
