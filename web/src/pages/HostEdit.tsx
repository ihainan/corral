import { useCallback, useEffect, useRef, useState, type FormEvent } from 'react'
import { CheckCircle2, ChevronLeft, ChevronRight, FileKey, KeyRound, PlugZap, TriangleAlert, Upload } from 'lucide-react'
import { api, ApiError, errorText, type Host, type KeyCred } from '../api'
import { hasKey, t } from '../i18n'
import { linkHandler, navigate } from '../router'
import { CopyButton, Dialog, Field, LoadError, useToast } from '../components/ui'
import { checkText, hostFormErrors, type HostCheck } from './pageText'
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
  // The connection as it was loaded: Save checks it first where it is new or
  // any of it changed (address, port, user, how it signs in, its key or
  // password, its jump host).
  const orig = useRef<{ address: string; port: string; username: string; auth: Auth; credentialId: number | null; jumpId: number | null } | null>(null)
  // The last check: running ('checking'), what it found, and whether it was
  // Save's (then "Save anyway" is offered where it did not get through).
  const [checking, setChecking] = useState(false)
  // (body: the form it was of — "Save anyway" saves that; a field changed since, it is no more.)
  const [check, setCheck] = useState<{ c: HostCheck; forSave: boolean; body: Record<string, unknown> } | null>(null)
  // (Counted on at each change of the connection's fields, however made — a
  // key generated or imported meanwhile too: a check's answer of an older
  // count is of another form, dropped, and nothing saved on it.)
  const formRev = useRef(0)

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
    orig.current = { address: h.address, port: String(h.port), username: h.username, auth: h.auth, credentialId: h.auth === 'key' ? h.credential_id : null, jumpId: h.jump_host_id ?? null }
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
      formRev.current++ // (now, not after the render: a check answering meanwhile is of the key before)
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
  // (forCheck: split as a check of it starts — the split values are the form it is of, not a change after it.)
  const splitAddress = (forCheck = false) => {
    const m = address.trim().match(/^(?:([^@\s]+)@)?(\[[0-9a-fA-F:]+\]|[^:\s]+)(?::(\d{1,5}))?$/)
    if (!m || (!m[1] && !m[3])) return { address, username, port }
    const out = { address: m[2].replace(/^\[|\]$/g, ''), username: m[1] ?? username, port: m[3] ?? port }
    setAddress(out.address)
    setUsername(out.username)
    setPort(out.port)
    if (forCheck) checkedOf.current = JSON.stringify([out.address, out.port, out.username, auth, password, credentialId, jumpId])
    toast(t('host.addressSplit'))
    return out
  }
  const [advOpen, setAdvOpen] = useState(false)

  // Focus to a field by its name as the server says it (one with no list
  // to choose from, a key: the button beside its message).
  const focusField = (f: string) => {
    if (f === 'jump_host_id') setAdvOpen(true)
    const id: Record<string, string> = { name: 'h-name', address: 'h-addr', port: 'h-port', username: 'h-user', password: 'h-pass', credential_id: 'h-key', jump_host_id: 'h-jump' }
    const el = (id[f] && document.getElementById(id[f])) || (f === 'credential_id' ? document.getElementById('h-key-new') : f === 'auth' ? document.querySelector<HTMLElement>('[aria-labelledby="auth-label"] button') : null)
    el?.focus()
  }

  // The form as it is sent: to save, to check, or to put the key on the host with.
  const formBody = (a: { address: string; username: string; port: string }) => ({
    name,
    address: a.address,
    port: a.port === '' ? 22 : Number(a.port), // (left empty: the usual SSH port)
    username: a.username,
    auth,
    password: auth === 'password' ? password : '',
    credential_id: auth === 'key' ? credentialId : null,
    jump_host_id: jumpId,
  })
  // Checked here as the server would first, every field at fault marked at
  // once, the focus on the first; false where any is.
  const checkFields = (a: { address: string; username: string; port: string }) => {
    const found = hostFormErrors({ name, address: a.address, port: a.port, username: a.username, auth, credentialId, password, hadPassword })
    if (found.length === 0) return true
    setErrors(Object.fromEntries(found.map(([f, k]) => [f, t(k)])))
    focusField(found[0][0])
    return false
  }
  // What the server refused of the form (a field, the host changed, …): shown as Save shows it.
  const showRefusal = (err: unknown) => {
    const field = err instanceof ApiError ? err.field : undefined
    setStale(err instanceof ApiError && err.code === 'host_changed')
    setErrors({ [field ?? '']: err instanceof ApiError && err.code === 'invalid_field' ? fieldMessage(field, err.message) : errorText(err) })
    if (field) requestAnimationFrame(() => focusField(field))
  }
  // A check's finding on the form: its reason on the field it belongs to;
  // all of it in the box by the buttons.
  const showCheck = (c: HostCheck, forSave: boolean, body: Record<string, unknown>) => {
    setCheck({ c, forSave, body })
    if (!c.ok && c.field) {
      setErrors({ [c.field]: checkText(c, auth, false) })
      requestAnimationFrame(() => focusField(c.field!))
    }
  }
  // (null: the form changed while it ran — its answer is not of this form.)
  const runCheck = async (body: ReturnType<typeof formBody>) => {
    setChecking(true)
    const rev = formRev.current
    try {
      const c = await api<HostCheck>('POST', '/api/hosts/test', { ...body, id: editing ? id : 0 }, { timeoutMs: 60000 })
      return rev === formRev.current ? c : null
    } finally {
      setChecking(false)
    }
  }
  const connChanged = (body: ReturnType<typeof formBody>) => {
    const o = orig.current
    if (!editing || !o) return true
    return o.address !== body.address || o.port !== String(body.port) || o.username !== body.username || o.auth !== body.auth || o.credentialId !== body.credential_id || o.jumpId !== body.jump_host_id || (auth === 'password' && password !== '')
  }

  const save = async (body: ReturnType<typeof formBody>) => {
    setBusy(true)
    try {
      if (editing) await api('PUT', '/api/hosts/' + id, { ...body, version })
      else await api('POST', '/api/hosts', body)
      navigate('/')
    } catch (err) {
      showRefusal(err)
      setBusy(false)
    }
  }

  const submit = async (e: FormEvent) => {
    e.preventDefault()
    if (busy || checking || generating) return
    const a = splitAddress(true)
    if (!checkFields(a)) return
    setErrors({})
    setCheck(null)
    const body = formBody(a)
    // (A new host, or its connection changed: connected to first. Only the name or its own settings: saved.)
    if (connChanged(body)) {
      let c: HostCheck | null
      try {
        c = await runCheck(body)
      } catch (err) {
        showRefusal(err)
        return
      }
      if (!c) return // (changed meanwhile: neither said nor saved — Save again checks what it is now)
      if (!c.ok) return showCheck(c, true, body)
    }
    await save(body)
  }

  const testOnly = async () => {
    if (busy || checking || generating) return
    const a = splitAddress(true)
    if (!checkFields(a)) return
    setErrors({})
    setCheck(null)
    const body = formBody(a)
    try {
      const c = await runCheck(body)
      if (c) showCheck(c, false, body)
    } catch (err) {
      showRefusal(err)
    }
  }
  // (What was checked, as it was: the form cannot change meanwhile — it is held while a check runs, and a change after drops the check.)
  const saveAnyway = () => {
    if (busy || checking || !check) return
    const body = check.body as ReturnType<typeof formBody>
    setCheck(null)
    setErrors({})
    void save(body)
  }
  // A field of the connection changed after a check: what it found is not of this.
  const checkedOf = useRef('')
  useEffect(() => {
    const now = JSON.stringify([address, port, username, auth, password, credentialId, jumpId])
    if (checkedOf.current && now !== checkedOf.current) {
      setCheck(null)
      formRev.current++
    }
    checkedOf.current = now
  }, [address, port, username, auth, password, credentialId, jumpId])

  // The key put on the host with a password typed once (its own dialog).
  const [installing, setInstalling] = useState(false)

  const fe = (f: string) => errors[f] || undefined
  const portLong = !!check && !check.c.ok && check.c.field === 'port' && !!fe('port')
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
        {/* (Held while a check runs: what is saved after it is what was checked.) */}
        <fieldset className="form-hold" disabled={checking}>
        <div className="row2">
          <Field label={t('host.address')} htmlFor="h-addr" error={fe('address')}>
            <input id="h-addr" className="input" value={address} onChange={(e) => setAddress(e.target.value)} onBlur={() => splitAddress()} placeholder={t('host.addressPlaceholder')}
              autoCapitalize="none" autoCorrect="off" spellCheck={false} inputMode="url" required aria-invalid={!!fe('address')} />
          </Field>
          {/* (A Field as the others, its error under it: ux1101 PG-19.) */}
          <div className="port">
            <Field label={t('host.port')} htmlFor="h-port" error={portLong ? undefined : fe('port')}>
              <input id="h-port" className="input" value={port} onChange={(e) => setPort(e.target.value.replace(/\D/g, ''))} inputMode="numeric" placeholder="22" aria-invalid={!!fe('port')} aria-describedby={portLong ? 'h-port-err' : undefined} />
            </Field>
          </div>
        </div>
        {/* (A connection's reason on the port, longer than its narrow box: under the row, its width.) */}
        {portLong && (
          <p className="err" id="h-port-err" role="alert">
            {fe('port')}
          </p>
        )}
        <Field label={t('host.username')} htmlFor="h-user" error={fe('username')}>
          <input id="h-user" className="input" value={username} onChange={(e) => setUsername(e.target.value)} autoCapitalize="none" autoCorrect="off" spellCheck={false} required aria-invalid={!!fe('username')} />
        </Field>
        {/* (Optional: left empty, the host is called by its address — said in the box as it will be.) */}
        <Field label={t('host.name')} htmlFor="h-name" error={fe('name')} optional>
          <input id="h-name" className="input" value={name} onChange={(e) => setName(e.target.value)} placeholder={address.trim() ? t('host.nameDefault', { addr: address.trim() }) : t('host.namePlaceholder')} aria-invalid={!!fe('name')} />
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
                <div className="key-acts">
                  <button type="button" id="h-key-install" className="btn" onClick={() => setInstalling(true)} disabled={busy || checking}>
                    <Upload size={16} />
                    {t('host.installKey')}
                  </button>
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
        </fieldset>

        {errors[''] && <p className="err" role="alert">{errors['']}</p>}
        {check && <CheckNote c={check.c} auth={auth} onSaveAnyway={check.forSave && !check.c.ok ? saveAnyway : undefined} busy={busy} />}
        {stale && (
          <div>
            <button type="button" className="btn" onClick={reopen} disabled={busy}>{t('host.reopen')}</button>
          </div>
        )}
        <div className="form-actions">
          <a className="btn" href="/" onClick={linkHandler('/')}>{t('common.cancel')}</a>
          <button type="button" className="btn" onClick={testOnly} disabled={busy || checking || generating}>
            <PlugZap size={16} />
            {t('host.test')}
          </button>
          <button className="btn btn-primary" disabled={busy || checking || generating} aria-busy={checking || busy}>
            {checking ? t('host.connecting') : busy ? t('common.saving') : t('common.save')}
          </button>
        </div>
      </form>
      {importing && (
        <ImportDialog
          onClose={() => setImporting(false)}
          onDone={(k) => {
            setKeys((ks) => [...ks.filter((x) => x.id !== k.id), k])
            formRev.current++
            setCredentialId(k.id)
          }}
        />
      )}
      {installing && selectedKey && (
        <InstallKeyDialog
          body={() => ({ ...formBody({ address, username, port }), id: editing ? id : 0 })}
          user={username}
          address={address}
          onClose={() => setInstalling(false)}
          onDone={() => {
            setCheck(null)
            setErrors({})
          }}
        />
      )}
      {editing && loaded && <AskHookRow hostId={id} hostName={name} />}
      {editing && loaded && <BellHookRow hostId={id} hostName={name} />}
    </main>
  )
}

// What a check found, by the buttons: got through (and how far — a host
// asked for its password each time is reached, not signed in to), or why
// not; Save's failed, "Save anyway" beside it (a machine may only be off).
function CheckNote({ c, auth, onSaveAnyway, busy }: { c: HostCheck; auth: Auth; onSaveAnyway?: () => void; busy: boolean }) {
  return (
    <div className={'host-check' + (c.ok ? ' is-ok' : ' is-bad')} role={c.ok ? 'status' : 'alert'}>
      {c.ok ? <CheckCircle2 size={18} aria-hidden="true" /> : <TriangleAlert size={18} aria-hidden="true" />}
      <div className="host-check-text">
        {/* (A reason on a field: said there; here, that it is, and what more there is to it.) */}
        <p>{c.ok || !c.field ? checkText(c, auth) : t('host.check.seeField')}</p>
        {!c.ok && c.field && c.stage === 'hostkey' && c.expected && <p className="host-check-fp">{t('host.check.hostkeyDetail', { expected: c.expected, got: c.fingerprint ?? '' })}</p>}
        {c.ok && c.fingerprint && <p className="host-check-fp">{t('host.check.fingerprint', { fp: c.fingerprint })}</p>}
        {onSaveAnyway && (
          <div className="host-check-acts">
            {/* (Only where it may be: not reached at all.) */}
            {c.stage === 'connect' && <span className="hint">{t('host.check.saveAnywayHint')}</span>}
            <button type="button" className="btn" onClick={onSaveAnyway} disabled={busy}>
              {t('host.check.saveAnyway')}
            </button>
          </div>
        )}
      </div>
    </div>
  )
}

// The form's key put on the host: signed in with the password typed here,
// this once (not kept, nor sent anywhere else), the key added to its
// ~/.ssh/authorized_keys, then signed in with the key to see it take.
function InstallKeyDialog({ body, user, address, onClose, onDone }: { body: () => Record<string, unknown>; user: string; address: string; onClose: () => void; onDone: () => void }) {
  const [pw, setPw] = useState('')
  const [going, setGoing] = useState(false)
  const [res, setRes] = useState<HostCheck | null>(null)
  const [err, setErr] = useState('')
  const go = async (e: FormEvent) => {
    e.preventDefault()
    if (going || !pw) return
    setGoing(true)
    setErr('')
    setRes(null)
    try {
      const r = await api<HostCheck>('POST', '/api/hosts/install-key', { ...body(), install_password: pw }, { timeoutMs: 90000 })
      setRes(r)
      if (r.ok) {
        setPw('')
        onDone()
      }
    } catch (x) {
      setErr(x instanceof ApiError && x.code === 'invalid_field' ? fieldMessage(x.field, x.message) : errorText(x))
    } finally {
      setGoing(false)
    }
  }
  return (
    <Dialog title={t('host.install.title')} onClose={onClose} busy={going} icon={<Upload size={18} />} sub={t('host.install.sub', { target: (user || '?') + '@' + (address || '?') })}>
      <form className="form" onSubmit={go} noValidate>
        {!res?.ok && (
          <Field label={t('host.install.password')} htmlFor="h-install-pass" hint={t('host.install.passwordHint')} error={res && !res.ok && res.field === 'install_password' ? checkText(res, 'key') : undefined}>
            <input id="h-install-pass" className="input" type="password" autoComplete="off" value={pw} onChange={(e) => setPw(e.target.value)} disabled={going} />
          </Field>
        )}
        {res && (res.ok || res.field !== 'install_password') && (
          <div className={'host-check' + (res.ok ? ' is-ok' : ' is-bad')} role={res.ok ? 'status' : 'alert'}>
            {res.ok ? <CheckCircle2 size={18} aria-hidden="true" /> : <TriangleAlert size={18} aria-hidden="true" />}
            <div className="host-check-text">
              <p>{checkText(res, 'key')}</p>
            </div>
          </div>
        )}
        {err && <p className="err" role="alert">{err}</p>}
        <div className="dialog-actions">
          <button type="button" className="btn" onClick={onClose} disabled={going}>
            {t(res?.ok ? 'common.close' : 'common.cancel')}
          </button>
          {!res?.ok && (
            <button className="btn btn-primary" disabled={going || !pw} aria-busy={going}>
              {going ? t('host.connecting') : t('host.install.go')}
            </button>
          )}
        </div>
      </form>
    </Dialog>
  )
}

// Server field messages are English; the common ones have a translation.
const fieldMessages: Record<string, Parameters<typeof t>[0]> = {
  'a host cannot be reached through itself': 'hostField.jumpSelf',
  'the address changed: enter the password to check it': 'hostField.passwordMoved',
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
