import { useEffect, useState, type FormEvent } from 'react'
import { disablePush, enablePush, pushState, type PushState } from '../push'
import { api, errorText, type Me } from '../api'
import { getLang, setLang, t, type Lang } from '../i18n'
import { setPrefs, usePrefs } from '../prefs'
import { useSession } from '../app'
import { Field, useToast } from '../components/ui'

export function SettingsPage() {
  const { me, setMe } = useSession()
  const prefs = usePrefs()
  const toast = useToast()
  const [display, setDisplay] = useState(me?.display_name ?? '')
  const [cur, setCur] = useState('')
  const [next, setNext] = useState('')
  const [pwError, setPwError] = useState('')
  const [busy, setBusy] = useState<'' | 'profile' | 'password'>('')
  const [lang, setLangState] = useState<Lang>(getLang())

  const saveProfile = async (e: FormEvent) => {
    e.preventDefault()
    if (busy) return
    setBusy('profile')
    try {
      const m = await api<Me>('PUT', '/api/me', { display_name: display })
      setMe(m)
      toast(t('settings.saved'))
    } catch (err) {
      toast(errorText(err), true)
    } finally {
      setBusy('')
    }
  }
  const changePassword = async (e: FormEvent) => {
    e.preventDefault()
    if (busy) return
    setBusy('password')
    setPwError('')
    try {
      await api('POST', '/api/me/password', { current: cur, next })
      setCur('')
      setNext('')
      toast(t('settings.passwordChanged'))
    } catch (err) {
      setPwError(errorText(err))
    } finally {
      setBusy('')
    }
  }

  return (
    <main className="page page-narrow">
      <div className="page-head"><h1>{t('settings.title')}</h1></div>

      <PushSection />

      <section className="section">
        <h2>{t('settings.appearance')}</h2>
        <div className="field">
          <span className="field-label" id="theme-l">{t('settings.theme')}</span>
          <div className="seg" role="group" aria-labelledby="theme-l">
            {(['system', 'light', 'dark'] as const).map((v) => (
              <button key={v} type="button" aria-pressed={prefs.theme === v} onClick={() => setPrefs({ theme: v })}>
                {v === 'system' ? t('settings.themeSystem') : v === 'light' ? t('settings.themeLight') : t('settings.themeDark')}
              </button>
            ))}
          </div>
        </div>
        <div className="field">
          <span className="field-label" id="lang-l">{t('settings.language')}</span>
          <div className="seg" role="group" aria-labelledby="lang-l">
            {([['zh-CN', '简体中文'], ['en', 'English']] as const).map(([v, label]) => (
              <button key={v} type="button" aria-pressed={lang === v} onClick={() => { setLang(v); setLangState(v); location.reload() }}>
                {label}
              </button>
            ))}
          </div>
        </div>
      </section>

      <section className="section">
        <h2>{t('settings.terminal')}</h2>
        <Field label={t('settings.fontSize')} htmlFor="s-font">
          <select id="s-font" className="select" value={prefs.fontSize} onChange={(e) => setPrefs({ fontSize: Number(e.target.value) })}>
            {[11, 12, 13, 14, 15, 16, 18, 20].map((n) => <option key={n} value={n}>{n}</option>)}
          </select>
        </Field>
        <label className="check">
          <input type="checkbox" checked={prefs.keybar} onChange={(e) => setPrefs({ keybar: e.target.checked })} />
          <span>{t('settings.keybar')}</span>
        </label>
        <label className="check">
          <input type="checkbox" checked={prefs.webgl} onChange={(e) => setPrefs({ webgl: e.target.checked })} />
          <span>{t('settings.webgl')}<br /><span className="hint">{t('settings.webglHint')}</span></span>
        </label>
        <label className="check">
          <input type="checkbox" checked={prefs.optionIsMeta} onChange={(e) => setPrefs({ optionIsMeta: e.target.checked })} />
          <span>{t('settings.optionIsMeta')}</span>
        </label>
      </section>

      <section className="section">
        <h2>{t('settings.profile')}</h2>
        <form className="form" onSubmit={saveProfile}>
          <Field label={t('settings.displayName')} htmlFor="s-display">
            <input id="s-display" className="input" value={display} onChange={(e) => setDisplay(e.target.value)} />
          </Field>
          <div><button className="btn" disabled={busy === 'profile'}>{t('common.save')}</button></div>
        </form>
      </section>

      <section className="section">
        <h2>{t('settings.password')}</h2>
        <form className="form" onSubmit={changePassword}>
          <Field label={t('settings.currentPassword')} htmlFor="s-cur">
            <input id="s-cur" className="input" type="password" autoComplete="current-password" value={cur} onChange={(e) => setCur(e.target.value)} required />
          </Field>
          <Field label={t('settings.newPassword')} htmlFor="s-next" hint={t('setup.passwordHint')}>
            <input id="s-next" className="input" type="password" autoComplete="new-password" minLength={8} value={next} onChange={(e) => setNext(e.target.value)} required />
          </Field>
          {pwError && <p className="err" role="alert">{pwError}</p>}
          <div><button className="btn" disabled={busy === 'password'}>{t('settings.password')}</button></div>
        </form>
      </section>
    </main>
  )
}

function PushSection() {
  const toast = useToast()
  const [info, setInfo] = useState<{ enabled: boolean; public_key?: string; devices?: { id: number; user_agent: string; created_at: number }[] } | null>(null)
  const [state, setState] = useState<PushState | null>(null)
  const [busy, setBusy] = useState(false)
  const refresh = () => {
    api<typeof info>('GET', '/api/push').then(setInfo).catch(() => setInfo({ enabled: false }))
    pushState().then(setState).catch(() => setState('unsupported'))
  }
  useEffect(refresh, [])
  if (!info || !state) return null
  let body: React.ReactNode
  if (!info.enabled) body = <p className="hint">{t('push.unavailable')}</p>
  else if (state === 'needs-home-screen') body = <p className="hint">{t('push.needsHomeScreen')}</p>
  else if (state === 'unsupported') body = <p className="hint">{t('push.unsupported')}</p>
  else if (state === 'denied') body = <p className="err">{t('push.denied')}</p>
  else if (state === 'off')
    body = (
      <div>
        <button
          className="btn btn-primary"
          disabled={busy}
          onClick={async () => {
            setBusy(true)
            try {
              setState(await enablePush(info.public_key!))
              refresh()
            } catch {
              toast(t('push.failed'), true)
            } finally {
              setBusy(false)
            }
          }}
        >
          {t('push.enable')}
        </button>
      </div>
    )
  else
    body = (
      <div className="row" style={{ alignItems: 'center', flexWrap: 'wrap' }}>
        <span className="state state-running" style={{ flex: '1 1 auto' }}>{t('push.enabled')}</span>
        <button className="btn" style={{ flex: 'none' }} onClick={async () => { await api('POST', '/api/push/test'); toast(t('push.testSent')) }}>{t('push.test')}</button>
        <button className="btn btn-ghost" style={{ flex: 'none' }} onClick={async () => { await disablePush(); refresh() }}>{t('push.disable')}</button>
      </div>
    )
  return (
    <section className="section">
      <h2>{t('push.title')}</h2>
      <p className="hint">{t('push.intro')}</p>
      {body}
    </section>
  )
}
