import { createContext, useCallback, useContext, useEffect, useRef, useState, type ReactNode } from 'react'
import { Bot, Film, KeyRound, LogOut, ScrollText, Server, Settings, Users } from 'lucide-react'
import { api, ApiError, bumpAuthGen, setUnauthorizedHandler, type Bootstrap, type Me } from './api'
import { applyDefaultLanguage, t } from './i18n'
import { freezePrefs, resetPrefsSession, sendPendingPrefs, syncPrefsFromServer } from './prefs'
import { match, navigate, usePath, linkHandler } from './router'
import { BrandMark, ToastProvider, useToast } from './components/ui'
import { disablePush, syncPush } from './push'
import { LoginPage, NewPasswordPage, SetupPage, InvitePage } from './pages/Auth'
import { AgentsPage } from './pages/Agents'
import { HostsPage } from './pages/Hosts'
import { HostEditPage } from './pages/HostEdit'
import { KeysPage } from './pages/Keys'
import { SettingsPage } from './pages/Settings'
import { AdminUsersPage, AdminAuditPage } from './pages/Admin'
import { RecordingPlayerPage, RecordingsPage } from './pages/Recordings'
import { TerminalPage } from './pages/Terminal'
import { TerminalTabs, useTabsMode } from './pages/TerminalTabs'
import { draftsFor } from './terminal/drafts'

interface Session {
  boot: Bootstrap
  me: Me | null
  setMe: (m: Me | null) => void
}

const SessionCtx = createContext<Session>(null!)
const AUTH_EVENT = 'corral.authEvent'
export const useSession = () => useContext(SessionCtx)

// The service worker keeps alerts (their text) to put them in order: it is
// told which account is signed in (null: none), and drops them for any other;
// told again when another worker takes over (an upgrade: the one before may
// not have understood).
let alertsAccount: number | null | undefined // (undefined: not known yet)
function alertsFor(userId: number | null) {
  alertsAccount = userId
  navigator.serviceWorker?.ready
    .then((r) => r.active?.postMessage({ t: 'account', id: userId }))
    .catch(() => {})
}
navigator.serviceWorker?.addEventListener('controllerchange', () => alertsAccount !== undefined && alertsFor(alertsAccount))

export function App() {
  const [boot, setBoot] = useState<Bootstrap | null>(null)
  const [me, setMeState] = useState<Me | null>(null)
  // Every sign-in, sign-out or account switch: requests sent before it can no
  // longer sign this page out, and other tabs reload into the new state.
  // (Learning who is signed in at startup is not a change and tells no one.)
  const meRef = useRef<Me | null>(null)
  const booted = useRef(false)
  const loadedVersion = useRef('') // the gateway's version when this page loaded
  const setMe = useCallback((m: Me | null) => {
    if ((m?.id ?? null) !== (meRef.current?.id ?? null)) {
      bumpAuthGen()
      draftsFor(m?.id ?? null) // (the account's composer drafts end with it, a lapsed sign-in too; this tab keeps the user's own)
      alertsFor(m?.id ?? null)
      try {
        if (booted.current) localStorage.setItem(AUTH_EVENT, String(Date.now()))
      } catch {
        /* ignore */
      }
    }
    meRef.current = m
    setMeState(m)
  }, [])
  useEffect(() => {
    const onStorage = (e: StorageEvent) => e.key === AUTH_EVENT && location.reload()
    window.addEventListener('storage', onStorage)
    return () => window.removeEventListener('storage', onStorage)
  }, [])
  const [error, setError] = useState(false)
  const path = usePath()
  const tabs = useTabsMode()

  useEffect(() => {
    setUnauthorizedHandler(() => {
      resetPrefsSession()
      setMe(null)
      if (!location.pathname.startsWith('/login')) navigate('/login?next=' + encodeURIComponent(location.pathname + location.search), true)
    })
    api<Bootstrap>('GET', '/api/bootstrap', undefined, { timeoutMs: 20000 })
      .then((b) => {
        applyDefaultLanguage(b.default_language)
        loadedVersion.current = b.version
        setBoot(b)
        setMe(b.user)
        alertsFor(b.user?.id ?? null) // (also signed out while no page was open)
        booted.current = true
        if (b.user && !b.user.must_change_password) syncPrefsFromServer(b.user.id)
      })
      .catch(() => setError(true))
  }, [])

  // What the start-up answer says about the user (such as whether there are
  // recordings to see) is learnt again after signing in.
  useEffect(() => {
    if (!booted.current || !me) return
    api<Bootstrap>('GET', '/api/bootstrap')
      .then((b) => {
        setBoot((prev) => (prev ? { ...b, user: prev.user } : b))
        window.dispatchEvent(new CustomEvent(VERSION_EVENT, { detail: b.version }))
      })
      .catch(() => {})
  }, [me?.id, me?.must_change_password]) // (also once a required password change is done: it was not told before)

  if (error) {
    return (
      <div className="auth-wrap">
        <div className="auth">
          <p className="lead">{t('error.network')}</p>
          <button className="btn" onClick={() => location.reload()}>{t('common.retry')}</button>
        </div>
      </div>
    )
  }
  if (!boot) return <Booting />

  return (
    <SessionCtx.Provider value={{ boot, me, setMe }}>
      <ToastProvider>
        <PushSync userId={me?.must_change_password ? undefined : me?.id} />
        <UpdateNotice loaded={loadedVersion.current} />
        {route(path, boot, me, tabs)}
      </ToastProvider>
    </SessionCtx.Provider>
  )
}

// Whoever signs in on this device takes over its notifications. A failure
// is said out loud: otherwise alerts silently stop arriving.
function PushSync({ userId }: { userId?: number }) {
  const toast = useToast()
  useEffect(() => {
    if (!userId) return
    void syncPush().then((ok) => ok || toast(t('push.syncFailed'), true))
  }, [userId, toast])
  return null
}

// Before the start-up answer: nothing for a moment (it is usually there at
// once), then a word that the app is loading.
function Booting() {
  const [slow, setSlow] = useState(false)
  useEffect(() => {
    const tm = window.setTimeout(() => setSlow(true), 800)
    return () => clearTimeout(tm)
  }, [])
  return slow ? (
    <div className="auth-wrap">
      <div className="auth">
        <p className="hint">{t('common.loading')}</p>
      </div>
    </div>
  ) : null
}

function Redirect({ to }: { to: string }) {
  useEffect(() => navigate(to, true), [to])
  return null
}

function route(path: string, boot: Bootstrap, me: Me | null, tabs: boolean): ReactNode {
  if (path === '/setup') return boot.setup_needed ? <SetupPage /> : <Redirect to="/login" />
  // A password an administrator set: the user's own first, wherever they were going.
  if (me?.must_change_password) return <NewPasswordPage />
  let m = match('/invite/:token', path)
  if (m) return <InvitePage token={m.token} />
  if (path === '/login') return me ? <Redirect to={nextPath()} /> : <LoginPage />
  if (!me) return <Redirect to={boot.setup_needed ? '/setup' : '/login?next=' + encodeURIComponent(path + location.search)} />

  if ((m = match('/t/:id', path))) return tabs ? <TerminalTabs current={m.id} /> : <TerminalPage key={m.id} id={m.id} />
  if ((m = match('/w/:id', path))) return <TerminalPage key={'w' + m.id} id={m.id} watch />
  if ((m = match('/recordings/:id', path))) return <RecordingPlayerPage key={m.id} id={m.id} />
  if (path === '/recordings') return <Shell active="recordings"><RecordingsPage /></Shell>
  if (path === '/agents') return <Shell active="agents"><AgentsPage /></Shell>
  if (path === '/' || path === '/hosts') return <Shell active="hosts"><HostsPage /></Shell>
  if (path === '/hosts/new') return <Shell active="hosts"><HostEditPage /></Shell>
  if ((m = match('/hosts/:id/edit', path))) return <Shell active="hosts"><HostEditPage id={Number(m.id)} /></Shell>
  if (path === '/keys') return <Shell active="keys"><KeysPage /></Shell>
  if (path === '/settings') return <Shell active="settings"><SettingsPage /></Shell>
  if (me.role === 'admin') {
    if (path === '/admin/users') return <Shell active="users"><AdminUsersPage /></Shell>
    if (path === '/admin/audit') return <Shell active="audit"><AdminAuditPage /></Shell>
  }
  return <Redirect to="/" />
}

function nextPath(): string {
  const n = new URLSearchParams(location.search).get('next')
  return n && n.startsWith('/') && !n.startsWith('//') ? n : '/'
}

type NavKey = 'hosts' | 'agents' | 'keys' | 'settings' | 'recordings' | 'users' | 'audit'

// signOut ends the browser session. The push subscription of this device is
// removed first, so the next person signing in here does not get the previous
// account's alerts.
// One sign-out at a time: a second tap joins the first instead of starting
// another flow that could unfreeze settings while the first is still running.
let signingOut: Promise<void> | null = null
export function signOut(setMe: (m: Me | null) => void, toast: (m: string, e?: boolean) => void): Promise<void> {
  signingOut ??= signOutOnce(setMe, toast).finally(() => (signingOut = null))
  return signingOut
}

async function signOutOnce(setMe: (m: Me | null) => void, toast: (m: string, e?: boolean) => void) {
  // Settings changed just before signing out still belong to this account.
  freezePrefs(true)
  await sendPendingPrefs(3000)
  await disablePush().catch(() => {})
  try {
    await api('POST', '/api/logout')
  } catch (e) {
    // A 401 means we were already signed out; anything else did not sign us out.
    if (!(e instanceof ApiError && e.status === 401)) {
      toast(t('nav.signOutFailed'), true)
      freezePrefs(false)
      return
    }
  }
  resetPrefsSession()
  freezePrefs(false)
  setMe(null)
  navigate('/login', true)
}

function Shell({ active, children }: { active: NavKey; children: ReactNode }) {
  const { me, setMe, boot } = useSession()
  const toast = useToast()
  const main: { key: NavKey; href: string; label: string; icon: ReactNode }[] = [
    { key: 'hosts', href: '/', label: t('nav.hosts'), icon: <Server size={17} strokeWidth={1.75} /> },
    { key: 'agents', href: '/agents', label: t('nav.agents'), icon: <Bot size={17} strokeWidth={1.75} /> },
    { key: 'keys', href: '/keys', label: t('nav.keys'), icon: <KeyRound size={17} strokeWidth={1.75} /> },
    { key: 'settings', href: '/settings', label: t('nav.settings'), icon: <Settings size={17} strokeWidth={1.75} /> },
  ]
  // Recordings: only where sessions are recorded; on phones under Settings.
  const extra: typeof main = boot.recordings
    ? [{ key: 'recordings', href: '/recordings', label: t('nav.recordings'), icon: <Film size={17} strokeWidth={1.75} /> }]
    : []
  const admin: typeof main = me?.role === 'admin'
    ? [
        { key: 'users', href: '/admin/users', label: t('nav.users'), icon: <Users size={17} strokeWidth={1.75} /> },
        { key: 'audit', href: '/admin/audit', label: t('nav.audit'), icon: <ScrollText size={17} strokeWidth={1.75} /> },
      ]
    : []
  const item = (i: (typeof main)[number]) => (
    <a key={i.key} className="nav-item" href={i.href} onClick={linkHandler(i.href)} aria-current={i.key === active ? 'page' : undefined} title={i.label}>
      {i.icon}
      <span className="label">{i.label}</span>
    </a>
  )
  // On phones the admin pages sit under Settings in the tab bar.
  const tabActive = active === 'users' || active === 'audit' || active === 'recordings' ? 'settings' : active
  return (
    <div className="shell">
      <nav className="sidebar" aria-label={t('nav.main')}>
        <a className="brand" href="/" onClick={linkHandler('/')} aria-label={t('app.name')}>
          <BrandMark />
          <span className="label">{t('app.name')}</span>
        </a>
        {main.map(item)}
        {extra.map(item)}
        {admin.length > 0 && <div className="nav-group">{t('nav.admin')}</div>}
        {admin.map(item)}
        <div className="sidebar-foot">
          <span className="sidebar-user">{me?.display_name || me?.username}</span>
          <button className="icon-btn" onClick={() => signOut(setMe, toast)} aria-label={t('nav.signOut')} title={t('nav.signOut')}>
            <LogOut size={16} strokeWidth={1.75} />
          </button>
        </div>
      </nav>
      <div className="content">{children}</div>
      <nav className="tabbar" aria-label={t('nav.main')}>
        {main.map((i) => (
          <a key={i.key} className="tab" href={i.href} onClick={linkHandler(i.href)} aria-current={i.key === tabActive ? 'page' : undefined}>
            {i.icon}
            <span>{i.label}</span>
          </a>
        ))}
      </nav>
    </div>
  )
}

// A home-screen app comes back from the background without reloading, so
// after a deploy it keeps running the old code. A newer gateway, seen when
// the page becomes visible again or a terminal reconnects (VERSION_EVENT),
// offers a reload, once.
export const VERSION_EVENT = 'corral.gatewayVersion'
function UpdateNotice({ loaded }: { loaded: string }) {
  const toast = useToast()
  useEffect(() => {
    let told = false
    let pending = false // seen while hidden: told once visible
    const tell = () => {
      if (told) return
      if (document.visibilityState !== 'visible') return void (pending = true)
      told = true
      toast(t('app.updated'), false, { label: t('app.reload'), run: () => location.reload() })
    }
    const seen = (v: string) => {
      if (loaded && v && v !== loaded) tell()
    }
    const onVersion = (e: Event) => seen((e as CustomEvent<string>).detail)
    const onVisible = () => {
      if (document.visibilityState !== 'visible') return
      if (pending) tell()
      if (told) return
      api<Bootstrap>('GET', '/api/bootstrap').then((b) => seen(b.version)).catch(() => {})
    }
    window.addEventListener(VERSION_EVENT, onVersion)
    document.addEventListener('visibilitychange', onVisible)
    return () => {
      window.removeEventListener(VERSION_EVENT, onVersion)
      document.removeEventListener('visibilitychange', onVisible)
    }
  }, [loaded, toast])
  return null
}
