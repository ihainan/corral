import { useEffect, useState, type FormEvent, type ReactNode } from 'react'
import { ChevronRight, LogOut, Minus, Plus, ScrollText, Users } from 'lucide-react'
import { disablePush, enablePush, pushState, syncPush, type PushState } from '../push'
import { api, errorText, type Me } from '../api'
import { getLang, setLang, t, type Lang } from '../i18n'
import { setPrefs, usePrefs } from '../prefs'
import { linkHandler } from '../router'
import { signOut, useSession } from '../app'
import { Field, useToast } from '../components/ui'
import { CurrentScheme, SchemeDialog } from '../terminal/SchemePicker'

const fontSizes = [11, 12, 13, 14, 15, 16, 18, 20]

function Setting(props: { label: string; hint?: ReactNode; id?: string; children: ReactNode }) {
  return (
    <div className="setting">
      <div className="setting-label">
        <span id={props.id}>{props.label}</span>
        {props.hint && <span className="hint">{props.hint}</span>}
      </div>
      <div className="setting-control">{props.children}</div>
    </div>
  )
}

function Switch(props: { label: string; checked: boolean; onChange: (v: boolean) => void; hint?: ReactNode }) {
  return (
    <label className="setting">
      <span className="setting-label">
        <span>{props.label}</span>
        {props.hint && <span className="hint">{props.hint}</span>}
      </span>
      <span className="setting-control">
        <input type="checkbox" role="switch" className="switch" checked={props.checked} onChange={(e) => props.onChange(e.target.checked)} />
      </span>
    </label>
  )
}

function Seg<T extends string>(props: { labelledBy: string; value: T; options: [T, string][]; onChange: (v: T) => void }) {
  return (
    <div className="seg" role="group" aria-labelledby={props.labelledBy}>
      {props.options.map(([v, label]) => (
        <button key={v} type="button" aria-pressed={props.value === v} onClick={() => props.onChange(v)}>
          {label}
        </button>
      ))}
    </div>
  )
}

export function SettingsPage() {
  const { me, setMe } = useSession()
  const prefs = usePrefs()
  const toast = useToast()
  const [lang, setLangState] = useState<Lang>(getLang())
  const [schemesOpen, setSchemesOpen] = useState(false)
  const fontIdx = fontSizes.indexOf(prefs.fontSize)
  const stepFont = (d: number) => {
    const i = fontIdx === -1 ? fontSizes.indexOf(14) : fontIdx
    const n = fontSizes[Math.min(fontSizes.length - 1, Math.max(0, i + d))]
    setPrefs({ fontSize: n })
  }

  return (
    <main className="page page-form">
      <div className="page-head"><h1>{t('settings.title')}</h1></div>

      <section className="section">
        <h2>{t('settings.appearance')}</h2>
        <div className="settings-group">
          <Setting label={t('settings.theme')} id="theme-l">
            <Seg
              labelledBy="theme-l"
              value={prefs.theme}
              options={[['system', t('settings.themeSystem')], ['light', t('settings.themeLight')], ['dark', t('settings.themeDark')]]}
              onChange={(v) => setPrefs({ theme: v })}
            />
          </Setting>
          <Setting label={t('settings.language')} id="lang-l">
            <Seg
              labelledBy="lang-l"
              value={lang}
              options={[['zh-CN', '简体中文'], ['en', 'English']]}
              onChange={(v) => {
                setLang(v)
                setLangState(v)
                location.reload()
              }}
            />
          </Setting>
        </div>
      </section>

      <section className="section">
        <h2>{t('settings.terminal')}</h2>
        <div className="settings-group">
          <Setting label={t('settings.termTheme')} id="tt-l">
            <button type="button" className="btn scheme-btn" aria-labelledby="tt-l tt-v" onClick={() => setSchemesOpen(true)}>
              <span id="tt-v"><CurrentScheme /></span>
              <ChevronRight size={16} />
            </button>
          </Setting>
          <Setting label={t('settings.fontSize')} id="fs-l">
            <div className="stepper" role="group" aria-labelledby="fs-l">
              <button type="button" className="icon-btn" aria-label={t('settings.fontSmaller')} disabled={fontIdx === 0} onClick={() => stepFont(-1)}>
                <Minus size={16} />
              </button>
              <span className="value" aria-live="polite">{prefs.fontSize}</span>
              <button type="button" className="icon-btn" aria-label={t('settings.fontLarger')} disabled={fontIdx === fontSizes.length - 1} onClick={() => stepFont(1)}>
                <Plus size={16} />
              </button>
            </div>
          </Setting>
          <Switch label={t('settings.keybar')} checked={prefs.keybar} onChange={(v) => setPrefs({ keybar: v })} />
          <Switch label={t('settings.optionIsMeta')} checked={prefs.optionIsMeta} onChange={(v) => setPrefs({ optionIsMeta: v })} />
          <Switch label={t('settings.webgl')} hint={t('settings.webglHint')} checked={prefs.webgl} onChange={(v) => setPrefs({ webgl: v })} />
        </div>
      </section>

      {schemesOpen && <SchemeDialog onClose={() => setSchemesOpen(false)} />}

      <PushSection />

      <ProfileSection />
      <PasswordSection />

      {/* Phones have no sidebar: admin pages and sign-out live here. */}
      <section className="section phone-only">
        <h2>{t('nav.account')}</h2>
        <div className="settings-group">
          {me?.role === 'admin' && (
            <>
              <a className="setting setting-link" href="/admin/users" onClick={linkHandler('/admin/users')}>
                <Users size={18} strokeWidth={1.75} className="row-icon" />
                <span className="setting-label">{t('nav.users')}</span>
                <ChevronRight size={16} className="chev" />
              </a>
              <a className="setting setting-link" href="/admin/audit" onClick={linkHandler('/admin/audit')}>
                <ScrollText size={18} strokeWidth={1.75} className="row-icon" />
                <span className="setting-label">{t('nav.audit')}</span>
                <ChevronRight size={16} className="chev" />
              </a>
            </>
          )}
          <button className="setting setting-link danger" onClick={() => signOut(setMe, toast)}>
            <LogOut size={18} strokeWidth={1.75} className="row-icon" />
            <span className="setting-label">
              {t('nav.signOut')}
              <span className="hint">{me?.username}</span>
            </span>
          </button>
        </div>
      </section>
    </main>
  )
}

function ProfileSection() {
  const { me, setMe } = useSession()
  const toast = useToast()
  const [display, setDisplay] = useState(me?.display_name ?? '')
  const [busy, setBusy] = useState(false)
  const dirty = display !== (me?.display_name ?? '')
  const save = async (e: FormEvent) => {
    e.preventDefault()
    if (busy) return
    setBusy(true)
    try {
      const m = await api<Me>('PUT', '/api/me', { display_name: display })
      setMe(m)
      toast(t('settings.saved'))
    } catch (err) {
      toast(errorText(err), true)
    } finally {
      setBusy(false)
    }
  }
  return (
    <section className="section">
      <h2>{t('settings.profile')}</h2>
      <form className="form" onSubmit={save}>
        <Field label={t('settings.displayName')} htmlFor="s-display" hint={t('settings.username', { name: me?.username ?? '' })}>
          <input id="s-display" className="input" value={display} onChange={(e) => setDisplay(e.target.value)} />
        </Field>
        <div>
          <button className="btn" disabled={busy || !dirty}>{t('common.save')}</button>
        </div>
      </form>
    </section>
  )
}

function PasswordSection() {
  const toast = useToast()
  const [cur, setCur] = useState('')
  const [next, setNext] = useState('')
  const [error, setError] = useState('')
  const [busy, setBusy] = useState(false)
  const submit = async (e: FormEvent) => {
    e.preventDefault()
    if (busy) return
    setBusy(true)
    setError('')
    try {
      await api('POST', '/api/me/password', { current: cur, next })
      setCur('')
      setNext('')
      toast(t('settings.passwordChanged'))
    } catch (err) {
      setError(errorText(err))
    } finally {
      setBusy(false)
    }
  }
  return (
    <section className="section">
      <h2>{t('settings.password')}</h2>
      <form className="form" onSubmit={submit}>
        <Field label={t('settings.currentPassword')} htmlFor="s-cur">
          <input id="s-cur" className="input" type="password" autoComplete="current-password" value={cur} onChange={(e) => setCur(e.target.value)} required />
        </Field>
        <Field label={t('settings.newPassword')} htmlFor="s-next" hint={t('setup.passwordHint')}>
          <input id="s-next" className="input" type="password" autoComplete="new-password" minLength={8} value={next} onChange={(e) => setNext(e.target.value)} required />
        </Field>
        {error && <p className="err" role="alert">{error}</p>}
        <div>
          <button className="btn" disabled={busy || !cur || next.length < 8}>{t('settings.password')}</button>
        </div>
      </form>
    </section>
  )
}

type PushInfo = { enabled: boolean; public_key?: string; devices?: { id: number; user_agent: string; created_at: number }[] }

// What the notification section shows: a server-side reason it cannot work,
// a browser state, or "registered" only once the server has confirmed it.
type PushView = PushState | 'loading' | 'load-failed' | 'unregistered'

function PushSection() {
  const toast = useToast()
  const [info, setInfo] = useState<PushInfo | null>(null)
  const [view, setView] = useState<PushView>('loading')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')

  const refresh = async () => {
    setError('')
    let i: PushInfo
    try {
      i = await api<PushInfo>('GET', '/api/push')
    } catch {
      setView('load-failed')
      return
    }
    setInfo(i)
    if (!i.enabled) return setView('off')
    const st = await pushState().catch((): PushState => 'unsupported')
    // A subscription that exists in the browser but not on the server for
    // this account would never deliver, so confirm it before saying "on".
    if (st === 'on' && !(await syncPush())) return setView('unregistered')
    setView(st)
  }
  useEffect(() => {
    void refresh()
  }, [])

  const run = async (fn: () => Promise<unknown>, failText: string) => {
    if (busy) return
    setBusy(true)
    setError('')
    try {
      await fn()
    } catch (e) {
      setError(failText + (e instanceof Error && e.message ? ' ' + errorText(e) : ''))
    } finally {
      setBusy(false)
    }
  }

  let body: ReactNode = null
  if (view === 'loading') body = <p className="loading">{t('common.loading')}</p>
  else if (view === 'load-failed')
    body = (
      <p className="inline-error" role="alert">
        <span>{t('common.loadFailed')}</span>
        <button className="link-btn" onClick={refresh}>{t('common.retry')}</button>
      </p>
    )
  else if (info && !info.enabled) body = <p className="hint">{t('push.unavailable')}</p>
  else if (view === 'needs-home-screen') body = <p className="hint">{t('push.needsHomeScreen')}</p>
  else if (view === 'unsupported') body = <p className="hint">{t('push.unsupported')}</p>
  else if (view === 'denied') body = <p className="err">{t('push.denied')}</p>
  else {
    const on = view === 'on'
    body = (
      <div className="settings-group">
        <label className="setting">
          <span className="setting-label">
            <span>{t('push.thisDevice')}</span>
            <span className={view === 'unregistered' ? 'hint state-danger' : 'hint'}>
              {view === 'unregistered' ? t('push.unregistered') : on ? t('push.enabled') : t('push.offHint')}
            </span>
          </span>
          <span className="setting-control">
            <input
              type="checkbox"
              role="switch"
              className="switch"
              checked={on}
              disabled={busy}
              onChange={(e) =>
                run(async () => {
                  if (e.target.checked) await enablePush(info!.public_key!)
                  else await disablePush()
                  await refresh()
                }, e.target.checked ? t('push.failed') : t('push.disableFailed'))
              }
            />
          </span>
        </label>
        {on && (
          <Setting label={t('push.test')} hint={t('push.testHint')}>
            <button
              className="btn"
              disabled={busy}
              onClick={() =>
                run(async () => {
                  await api('POST', '/api/push/test')
                  toast(t('push.testSent'))
                }, t('push.testFailed'))
              }
            >
              {t('push.testSend')}
            </button>
          </Setting>
        )}
      </div>
    )
  }
  return (
    <section className="section">
      <h2>{t('push.title')}</h2>
      <p className="hint">{t('push.intro')}</p>
      {body}
      {error && <p className="err" role="alert">{error}</p>}
    </section>
  )
}
