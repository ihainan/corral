import { useCallback, useEffect, useState, type FormEvent } from 'react'
import { ArrowLeft, ChevronRight, KeyRound } from 'lucide-react'
import { api, ApiError, errorText, type Host, type KeyCred } from '../api'
import { hasKey, t } from '../i18n'
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
  const [persist, setPersist] = useState<Host['persistence']>('tmux')
  const [herdrName, setHerdrName] = useState('')
  const [tmuxName, setTmuxName] = useState('')
  const [keys, setKeys] = useState<KeyCred[]>([])
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<{ field?: string; text: string } | null>(null)
  // The host's version the form shows; an edit is refused if it was saved
  // elsewhere since (stale), and the form can then be opened again.
  const [version, setVersion] = useState<number | undefined>(undefined)
  const [stale, setStale] = useState(false)

  const fill = useCallback((h: Host) => {
    setName(h.name)
    setAddress(h.address)
    setPort(String(h.port))
    setUsername(h.username)
    setAuth(h.auth)
    setPassword('')
    setHadPassword(h.auth === 'password')
    setCredentialId(h.auth === 'key' ? h.credential_id : null)
    setPersist(h.persistence)
    setHerdrName(h.herdr_session ?? '')
    setTmuxName(h.tmux_session)
    setVersion(h.version)
    setLoaded(true)
  }, [])

  useEffect(() => {
    api<KeyCred[]>('GET', '/api/credentials').then(setKeys).catch(() => {})
    if (!editing) return
    api<Host>('GET', '/api/hosts/' + id)
      .then(fill)
      .catch((e) => {
        toast(errorText(e), true)
        navigate('/', true)
      })
  }, [editing, id, toast, fill])

  const reopen = async () => {
    if (busy) return
    setBusy(true)
    try {
      fill(await api<Host>('GET', '/api/hosts/' + id))
      setError(null)
      setStale(false)
    } catch (e) {
      toast(errorText(e), true)
    } finally {
      setBusy(false)
    }
  }

  const [generating, setGenerating] = useState(false)
  const generateKey = async () => {
    if (generating) return
    setGenerating(true)
    try {
      const k = await api<KeyCred>('POST', '/api/credentials', { name: name ? `${name} key` : 'corral key', mode: 'generate' })
      setKeys((ks) => [...ks, k])
      setCredentialId(k.id)
    } catch (e) {
      toast(errorText(e), true)
    } finally {
      setGenerating(false)
    }
  }

  // "alice@10.0.0.5:2222" pasted into the address fills user name and port too.
  // Runs on blur and again on submit, since Enter submits without a blur.
  const splitAddress = () => {
    const m = address.trim().match(/^(?:([^@\s]+)@)?(\[[0-9a-fA-F:]+\]|[^:\s]+)(?::(\d{1,5}))?$/)
    if (!m || (!m[1] && !m[3])) return { address, username, port }
    const out = { address: m[2].replace(/^\[|\]$/g, ''), username: m[1] ?? username, port: m[3] ?? port }
    setAddress(out.address)
    setUsername(out.username)
    setPort(out.port)
    toast(t('host.addressSplit'))
    return out
  }
  const [advOpen, setAdvOpen] = useState(false)

  const submit = async (e: FormEvent) => {
    e.preventDefault()
    if (busy) return
    setBusy(true)
    setError(null)
    const a = splitAddress()
    // The session name of the mode not chosen is hidden, so it must never be
    // the reason a save fails: keep it only when it would pass validation.
    const keep = (v: string) => (/^[A-Za-z0-9_-]{1,64}$/.test(v.trim()) ? v : '')
    const body = {
      name, address: a.address, port: Number(a.port) || 22, username: a.username, auth,
      password: auth === 'password' ? password : '',
      credential_id: auth === 'key' ? credentialId : null,
      persistence: persist,
      tmux_session: persist === 'tmux' ? tmuxName : keep(tmuxName),
      herdr_session: persist === 'herdr' ? herdrName : keep(herdrName),
    }
    try {
      if (editing) await api('PUT', '/api/hosts/' + id, { ...body, version })
      else await api('POST', '/api/hosts', body)
      navigate('/')
    } catch (err) {
      const field = err instanceof ApiError ? err.field : undefined
      setStale(err instanceof ApiError && err.code === 'host_changed')
      setError({ field, text: err instanceof ApiError && err.code === 'invalid_field' ? fieldMessage(field, err.message) : errorText(err) })
      if (field === 'tmux_session' || field === 'herdr_session') setAdvOpen(true)
      setBusy(false)
    }
  }

  const fe = (f: string) => (error?.field === f ? error.text : undefined)
  const selectedKey = keys.find((k) => k.id === credentialId)
  if (!loaded) return <main className="page page-form"><p className="loading">{t('common.loading')}</p></main>

  return (
    <main className="page page-form">
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
        <div className="row2">
          <Field label={t('host.address')} htmlFor="h-addr" error={fe('address')}>
            <input id="h-addr" className="input" value={address} onChange={(e) => setAddress(e.target.value)} onBlur={splitAddress} placeholder={t('host.addressPlaceholder')}
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
              <button type="button" className="btn" onClick={generateKey} disabled={generating}>
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

        <div className="field">
          <span className="field-label" id="persist-label">{t('host.persistence')}</span>
          <div className="seg" role="group" aria-labelledby="persist-label">
            {(['tmux', 'herdr', 'none'] as const).map((v) => (
              <button type="button" key={v} aria-pressed={persist === v} onClick={() => setPersist(v)}>
                {v === 'tmux' ? t('host.persistTmux') : v === 'herdr' ? 'herdr' : t('host.persistNone')}
              </button>
            ))}
          </div>
          <p className="hint">
            {persist === 'tmux' ? t('host.persistTmuxHint') : persist === 'herdr' ? t('host.persistHerdrHint') : t('host.persistNoneHint')}
          </p>
        </div>

        <details className="adv" open={advOpen} onToggle={(e) => setAdvOpen(e.currentTarget.open)}>
          <summary>
            <ChevronRight size={16} />
            {t('host.advanced')}
          </summary>
          <div className="form">
            {persist === 'tmux' && (
              <Field label={t('host.tmuxSession')} htmlFor="h-tmux" optional error={fe('tmux_session')}>
                <input id="h-tmux" className="input mono" value={tmuxName} onChange={(e) => setTmuxName(e.target.value)} autoCapitalize="none" autoCorrect="off" spellCheck={false} />
              </Field>
            )}
            {persist === 'herdr' && (
              <Field label={t('host.herdrSession')} htmlFor="h-herdr" optional hint={t('host.herdrSessionHint')} error={fe('herdr_session')}>
                <input id="h-herdr" className="input mono" value={herdrName} onChange={(e) => setHerdrName(e.target.value)} autoCapitalize="none" autoCorrect="off" spellCheck={false} />
              </Field>
            )}
          </div>
        </details>

        {error && !error.field && <p className="err" role="alert">{error.text}</p>}
        {stale && (
          <div>
            <button type="button" className="btn" onClick={reopen} disabled={busy}>{t('host.reopen')}</button>
          </div>
        )}
        <div className="form-actions">
          <a className="btn" href="/" onClick={linkHandler('/')}>{t('common.cancel')}</a>
          <button className="btn btn-primary" disabled={busy}>{busy ? t('common.saving') : t('common.save')}</button>
        </div>
      </form>
    </main>
  )
}

// Server field messages are English; the common ones have a translation.
function fieldMessage(field: string | undefined, fallback: string): string {
  const key = field === 'herdr_session' ? 'hostField.tmux_session' : 'hostField.' + field
  return hasKey(key) ? t(key) : fallback
}
