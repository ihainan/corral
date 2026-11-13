import { useCallback, useEffect, useState, type FormEvent } from 'react'
import { ChevronLeft, ChevronRight, FileKey, KeyRound } from 'lucide-react'
import { api, ApiError, errorText, type Host, type KeyCred } from '../api'
import { hasKey, t } from '../i18n'
import { linkHandler, navigate } from '../router'
import { CopyButton, Field, LoadError, useToast } from '../components/ui'
import { hostFormErrors } from './pageText'
import { AskHookRow, BellHookRow } from '../terminal/askHook'
import { ImportDialog } from './Keys'

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
  const [keys, setKeys] = useState<KeyCred[]>([])
  const [keysFailed, setKeysFailed] = useState(false)
  const [jumpId, setJumpId] = useState<number | null>(null)
  const [hosts, setHosts] = useState<Host[] | null>(null) // null until loaded
  const [hostsFailed, setHostsFailed] = useState(false)
  const [busy, setBusy] = useState(false)
  // Each field at fault with its message (several at once: ux1101 PG-01);
  // '' for one that belongs to no field.
  const [errors, setErrors] = useState<Record<string, string>>({})
  // The host's version the form shows; an edit is refused if it was saved
  // elsewhere since (stale), and the form can then be opened again.
  const [version, setVersion] = useState<number | undefined>(undefined)
  const [stale, setStale] = useState(false)

  // The user's hosts, for the jump host choice.
  const loadHosts = useCallback(() => {
    setHostsFailed(false)
    api<Host[]>('GET', '/api/hosts')
      .then(setHosts)
      .catch(() => setHostsFailed(true))
  }, [])

  const fill = useCallback((h: Host) => {
    setName(h.name)
    setAddress(h.address)
    setPort(String(h.port))
    setUsername(h.username)
    setAuth(h.auth)
    setPassword('')
    setHadPassword(h.auth === 'password')
    setCredentialId(h.auth === 'key' ? h.credential_id : null)
    setJumpId(h.jump_host_id ?? null)
    if (h.jump_host_id) setAdvOpen(true)
    setVersion(h.version)
    setLoaded(true)
  }, [])

  const loadKeys = () => {
    setKeysFailed(false)
    api<KeyCred[]>('GET', '/api/credentials')
      .then(setKeys)
      .catch(() => setKeysFailed(true))
  }
  useEffect(() => {
    loadKeys()
    loadHosts()
    if (!editing) return
    api<Host>('GET', '/api/hosts/' + id)
      .then(fill)
      .catch((e) => {
        toast(errorText(e), true)
        navigate('/', true)
      })
  }, [editing, id, toast, fill, loadHosts])

  const reopen = async () => {
    if (busy) return
    setBusy(true)
    try {
      fill(await api<Host>('GET', '/api/hosts/' + id))
      setErrors({})
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

  // A key there is no need to leave the form for (pages-1105 2.1): imported
  // here, and chosen. (Its dialog is outside the form: its own submit is not
  // the form's.)
  const [importing, setImporting] = useState(false)

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

  // Focus to a field by its name as the server says it (one with no list
  // to choose from, a key: the button beside its message).
  const focusField = (f: string) => {
    const id: Record<string, string> = { name: 'h-name', address: 'h-addr', port: 'h-port', username: 'h-user', password: 'h-pass', credential_id: 'h-key', jump_host_id: 'h-jump' }
    const el = (id[f] && document.getElementById(id[f])) || (f === 'credential_id' ? document.getElementById('h-key-new') : f === 'auth' ? document.querySelector<HTMLElement>('[aria-labelledby="auth-label"] button') : null)
    el?.focus()
  }

  const submit = async (e: FormEvent) => {
    e.preventDefault()
    if (busy) return
    const a = splitAddress()
    // Every field at fault is marked at once, focus on the first; the
    // server's answer stays the last word for what is not checked here.
    const found = hostFormErrors({ name, address: a.address, port: a.port, username: a.username, auth, credentialId, password, hadPassword })
    if (found.length > 0) {
      setErrors(Object.fromEntries(found.map(([f, k]) => [f, t(k)])))
      focusField(found[0][0])
      return
    }
    setBusy(true)
    setErrors({})
    // Left empty, the usual SSH port.
    const portN = a.port === '' ? 22 : Number(a.port)
    const body = {
      name, address: a.address, port: portN, username: a.username, auth,
      password: auth === 'password' ? password : '',
      credential_id: auth === 'key' ? credentialId : null,
      jump_host_id: jumpId,
    }
    try {
      if (editing) await api('PUT', '/api/hosts/' + id, { ...body, version })
      else await api('POST', '/api/hosts', body)
      navigate('/')
    } catch (err) {
      const field = err instanceof ApiError ? err.field : undefined
      setStale(err instanceof ApiError && err.code === 'host_changed')
      setErrors({ [field ?? '']: err instanceof ApiError && err.code === 'invalid_field' ? fieldMessage(field, err.message) : errorText(err) })
      if (field === 'jump_host_id') setAdvOpen(true)
      setBusy(false)
      if (field) requestAnimationFrame(() => focusField(field))
    }
  }

  const fe = (f: string) => errors[f] || undefined
  // Hosts this one can go through: the user's others that are reached
  // directly and have a saved credential (and the one chosen, whatever it is now).
  const jumpChoices = (hosts ?? []).filter((h) => h.id === jumpId || (h.id !== id && !h.jump_host_id && h.auth !== 'ask'))
  // The chosen one, when it is not (yet) among them: still shown as chosen.
  const jumpMissing = jumpId !== null && !jumpChoices.some((h) => h.id === jumpId)
  const selectedKey = keys.find((k) => k.id === credentialId)
  if (!loaded) return <main className="page page-form"><p className="loading">{t('common.loading')}</p></main>

  return (
    <main className="page page-form">
      <div className="page-head">
        <a className="icon-btn" href="/" onClick={linkHandler('/')} aria-label={t('common.back')}>
          <ChevronLeft size={20} />
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
          {/* (A Field as the others, its error under it: ux1101 PG-19.) */}
          <div className="port">
            <Field label={t('host.port')} htmlFor="h-port" error={fe('port')}>
              <input id="h-port" className="input" value={port} onChange={(e) => setPort(e.target.value.replace(/\D/g, ''))} inputMode="numeric" placeholder="22" aria-invalid={!!fe('port')} />
            </Field>
          </div>
        </div>
        <Field label={t('host.username')} htmlFor="h-user" error={fe('username')}>
          <input id="h-user" className="input" value={username} onChange={(e) => setUsername(e.target.value)} autoCapitalize="none" autoCorrect="off" spellCheck={false} required aria-invalid={!!fe('username')} />
        </Field>

        <div className="field field-group-start">
          <span className="field-label" id="auth-label">{t('host.auth')}</span>
          <div className="seg" role="group" aria-labelledby="auth-label">
            {(['password', 'key', 'ask'] as Auth[]).map((a) => (
              <button type="button" key={a} aria-pressed={auth === a} onClick={() => setAuth(a)}>
                {a === 'password' ? t('host.authPassword') : a === 'key' ? t('host.authKey') : t('host.authAsk')}
              </button>
            ))}
          </div>
          {fe('auth') && (
            <p className="err" role="alert">
              {fe('auth')}
            </p>
          )}
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
            ) : keysFailed ? (
              <LoadError text={t('host.keysFailed')} onRetry={loadKeys} />
            ) : (
              <p className="hint">{t('host.keyNone')}</p>
            )}
            {fe('credential_id') && <p className="err" role="alert">{fe('credential_id')}</p>}
            <div className="key-acts">
              <button type="button" id="h-key-new" className="btn" onClick={generateKey} disabled={generating}>
                <KeyRound size={16} />
                {t('host.keyCreate')}
              </button>
              <button type="button" className="btn" onClick={() => setImporting(true)}>
                <FileKey size={16} />
                {t('keys.import')}
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

        <details className="adv" open={advOpen} onToggle={(e) => setAdvOpen(e.currentTarget.open)}>
          <summary>
            <ChevronRight size={16} />
            {t('host.advanced')}
          </summary>
          <div className="form">
            <Field label={t('host.jump')} htmlFor="h-jump" hint={t('host.jumpHint')} error={fe('jump_host_id')}>
              <select
                id="h-jump"
                className="select"
                value={jumpId ?? ''}
                onChange={(e) => setJumpId(e.target.value ? Number(e.target.value) : null)}
                aria-invalid={!!fe('jump_host_id')}
                aria-describedby="h-jump-desc"
              >
                <option value="">{t('host.jumpNone')}</option>
                {jumpMissing && <option value={jumpId!}>{hostsFailed ? t('host.jumpUnknown') : hosts === null ? t('common.loading') : t('host.jumpGone')}</option>}
                {jumpChoices.map((h) => (
                  <option key={h.id} value={h.id}>
                    {t('host.jumpOption', { name: h.name, target: h.username + '@' + h.address })}
                  </option>
                ))}
              </select>
              {hostsFailed && (
                <p className="err" role="alert">
                  {t('host.jumpLoadFailed')}{' '}
                  <button type="button" className="btn btn-ghost" onClick={loadHosts}>
                    {t('common.retry')}
                  </button>
                </p>
              )}
            </Field>
          </div>
        </details>

        {errors[''] && <p className="err" role="alert">{errors['']}</p>}
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
      {importing && (
        <ImportDialog
          onClose={() => setImporting(false)}
          onDone={(k) => {
            setKeys((ks) => [...ks.filter((x) => x.id !== k.id), k])
            setCredentialId(k.id)
          }}
        />
      )}
      {editing && loaded && <AskHookRow hostId={id} hostName={name} />}
      {editing && loaded && <BellHookRow hostId={id} hostName={name} />}
    </main>
  )
}

// Server field messages are English; the common ones have a translation.
const fieldMessages: Record<string, Parameters<typeof t>[0]> = {
  'a host cannot be reached through itself': 'hostField.jumpSelf',
  'choose one of your hosts': 'hostField.jumpChoose',
  'that host is itself reached through another one': 'hostField.jumpChained',
  'that host needs a saved password or key to be a jump host': 'hostField.jumpNoCred',
  'other hosts are reached through this one, so it cannot use a jump host itself': 'hostField.jumpHasDependents',
  'other hosts are reached through this one: it needs a saved password or key': 'hostField.authJumpNeeds',
}

function fieldMessage(field: string | undefined, fallback: string): string {
  const byMessage = fieldMessages[fallback]
  if (byMessage) return t(byMessage)
  const key = field === 'herdr_session' ? 'hostField.tmux_session' : 'hostField.' + field
  return hasKey(key) ? t(key) : fallback
}
