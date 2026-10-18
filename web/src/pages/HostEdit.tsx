import { useEffect, useState, type FormEvent } from 'react'
import { ArrowLeft, ChevronRight, KeyRound } from 'lucide-react'
import { api, ApiError, errorText, type Host, type KeyCred } from '../api'
import { t } from '../i18n'
import { linkHandler, navigate } from '../router'
import { CopyButton, Field, useToast } from '../components/ui'

type Auth = Host['auth']

export function HostEditPage({ id }: { id?: number }) {
  const toast = useToast()
  const editing = id !== undefined
  const [loaded, setLoaded] = useState(!editing)
  const [name, setName] = useState('')
  const [address, setAddress] = useState('')
  const [port, setPort] = useState('22')
  const [username, setUsername] = useState('')
  const [auth, setAuth] = useState<Auth>('password')
  const [password, setPassword] = useState('')
  const [hadPassword, setHadPassword] = useState(false)
  const [credentialId, setCredentialId] = useState<number | null>(null)
  const [persist, setPersist] = useState(true)
  const [tmuxName, setTmuxName] = useState('')
  const [keys, setKeys] = useState<KeyCred[]>([])
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<{ field?: string; text: string } | null>(null)

  useEffect(() => {
    api<KeyCred[]>('GET', '/api/credentials').then(setKeys).catch(() => {})
    if (!editing) return
    api<Host>('GET', '/api/hosts/' + id)
      .then((h) => {
        setName(h.name)
        setAddress(h.address)
        setPort(String(h.port))
        setUsername(h.username)
        setAuth(h.auth)
        setHadPassword(h.auth === 'password')
        setCredentialId(h.auth === 'key' ? h.credential_id : null)
        setPersist(h.persistence === 'tmux')
        setTmuxName(h.tmux_session)
        setLoaded(true)
      })
      .catch((e) => {
        toast(errorText(e), true)
        navigate('/', true)
      })
  }, [editing, id, toast])

  const generateKey = async () => {
    try {
      const k = await api<KeyCred>('POST', '/api/credentials', { name: name ? `${name} key` : 'corral key', mode: 'generate' })
      setKeys((ks) => [...ks, k])
      setCredentialId(k.id)
    } catch (e) {
      toast(errorText(e), true)
    }
  }

  const submit = async (e: FormEvent) => {
    e.preventDefault()
    setBusy(true)
    setError(null)
    const body = {
      name, address, port: Number(port) || 22, username, auth,
      password: auth === 'password' ? password : '',
      credential_id: auth === 'key' ? credentialId : null,
      persistence: persist ? 'tmux' : 'none',
      tmux_session: tmuxName,
    }
    try {
      if (editing) await api('PUT', '/api/hosts/' + id, body)
      else await api('POST', '/api/hosts', body)
      navigate('/')
    } catch (err) {
      const field = err instanceof ApiError ? err.field : undefined
      setError({ field, text: err instanceof ApiError && err.code === 'invalid_field' ? fieldMessage(field, err.message) : errorText(err) })
      setBusy(false)
    }
  }

  const fe = (f: string) => (error?.field === f ? error.text : undefined)
  const selectedKey = keys.find((k) => k.id === credentialId)
  if (!loaded) return <main className="page page-narrow"><p className="empty-inline">{t('common.loading')}</p></main>

  return (
    <main className="page page-narrow">
      <div className="page-head">
        <a className="icon-btn" href="/" onClick={linkHandler('/')} aria-label={t('common.back')}>
          <ArrowLeft size={20} />
        </a>
        <h1>{editing ? t('host.editTitle') : t('host.newTitle')}</h1>
      </div>
      <form className="form" onSubmit={submit} noValidate>
        <Field label={t('host.name')} htmlFor="h-name" error={fe('name')}>
          <input id="h-name" className="input" value={name} onChange={(e) => setName(e.target.value)} placeholder={t('host.namePlaceholder')} required aria-invalid={!!fe('name')} />
        </Field>
        <div className="row">
          <Field label={t('host.address')} htmlFor="h-addr" error={fe('address')}>
            <input id="h-addr" className="input" value={address} onChange={(e) => setAddress(e.target.value)} placeholder={t('host.addressPlaceholder')}
              autoCapitalize="none" autoCorrect="off" spellCheck={false} inputMode="url" required aria-invalid={!!fe('address')} />
          </Field>
          <div className="field port">
            <label htmlFor="h-port">{t('host.port')}</label>
            <input id="h-port" className="input" value={port} onChange={(e) => setPort(e.target.value.replace(/\D/g, ''))} inputMode="numeric" aria-invalid={!!fe('port')} />
          </div>
        </div>
        {fe('port') && <p className="err">{fe('port')}</p>}
        <Field label={t('host.username')} htmlFor="h-user" error={fe('username')}>
          <input id="h-user" className="input" value={username} onChange={(e) => setUsername(e.target.value)} autoCapitalize="none" autoCorrect="off" spellCheck={false} required aria-invalid={!!fe('username')} />
        </Field>

        <div className="field">
          <span className="field-label" id="auth-label">{t('host.auth')}</span>
          <div className="seg" role="group" aria-labelledby="auth-label">
            {(['password', 'key', 'ask'] as Auth[]).map((a) => (
              <button type="button" key={a} aria-pressed={auth === a} onClick={() => setAuth(a)}>
                {a === 'password' ? t('host.authPassword') : a === 'key' ? t('host.authKey') : t('host.authAsk')}
              </button>
            ))}
          </div>
        </div>

        {auth === 'password' && (
          <Field label={t('host.password')} htmlFor="h-pass" error={fe('password')} hint={hadPassword ? t('host.passwordStored') : undefined}>
            <input id="h-pass" className="input" type="password" autoComplete="new-password" value={password} onChange={(e) => setPassword(e.target.value)}
              placeholder={hadPassword ? t('host.passwordKeep') : ''} aria-invalid={!!fe('password')} />
          </Field>
        )}
        {auth === 'ask' && <p className="hint">{t('host.askHint')}</p>}
        {auth === 'key' && (
          <div className="field">
            <label htmlFor="h-key">{t('host.key')}</label>
            {keys.length > 0 ? (
              <select id="h-key" className="select" value={credentialId ?? ''} onChange={(e) => setCredentialId(e.target.value ? Number(e.target.value) : null)} aria-invalid={!!fe('credential_id')}>
                <option value="">—</option>
                {keys.map((k) => (
                  <option key={k.id} value={k.id}>{k.name}</option>
                ))}
              </select>
            ) : (
              <p className="hint">{t('host.keyNone')}</p>
            )}
            {fe('credential_id') && <p className="err">{fe('credential_id')}</p>}
            <div>
              <button type="button" className="btn" onClick={generateKey}>
                <KeyRound size={16} />
                {t('host.keyCreate')}
              </button>
            </div>
            {selectedKey && (
              <>
                <p className="hint">{t('host.keyHint')}</p>
                <div className="pubkey">
                  <code>{selectedKey.public_key}</code>
                  <CopyButton text={selectedKey.public_key} />
                </div>
              </>
            )}
          </div>
        )}

        <label className="check">
          <input type="checkbox" checked={persist} onChange={(e) => setPersist(e.target.checked)} />
          <span>
            {t('host.persistTmux')}
            <br />
            <span className="hint">{t('host.persistTmuxHint')}</span>
          </span>
        </label>

        <details className="adv">
          <summary>
            <ChevronRight size={16} />
            {t('host.advanced')}
          </summary>
          <div className="form">
            <Field label={t('host.tmuxSession')} htmlFor="h-tmux" optional error={fe('tmux_session')}>
              <input id="h-tmux" className="input mono" value={tmuxName} onChange={(e) => setTmuxName(e.target.value)} autoCapitalize="none" autoCorrect="off" spellCheck={false} />
            </Field>
          </div>
        </details>

        {error && !error.field && <p className="err" role="alert">{error.text}</p>}
        <div className="dialog-actions">
          <a className="btn" href="/" onClick={linkHandler('/')}>{t('common.cancel')}</a>
          <button className="btn btn-primary" disabled={busy}>{busy ? t('common.saving') : t('common.save')}</button>
        </div>
      </form>
    </main>
  )
}

// Server field messages are English; map the common ones to the UI language.
function fieldMessage(field: string | undefined, fallback: string): string {
  const zh = document.documentElement.lang.startsWith('zh')
  if (!zh) return fallback
  switch (field) {
    case 'name': return '请填写名称，最多 64 个字符。'
    case 'address': return '请填写有效的主机名或 IP 地址。'
    case 'port': return '端口范围是 1 到 65535。'
    case 'username': return '请填写有效的用户名。'
    case 'password': return '请填写密码。'
    case 'credential_id': return '请选择一把密钥。'
    case 'tmux_session': return '只能包含字母、数字、点、短横线和下划线。'
    default: return fallback
  }
}
