import { createContext, useCallback, useContext, useEffect, useRef, useState, type ReactNode } from 'react'
import { KeyRound, LogOut, ScrollText, Server, Settings, Users } from 'lucide-react'
import { api, ApiError, bumpAuthGen, setUnauthorizedHandler, type Bootstrap, type Me } from './api'
import { applyDefaultLanguage, t } from './i18n'
import { resetPrefsSession, sendPendingPrefs, syncPrefsFromServer } from './prefs'
import { match, navigate, usePath, linkHandler } from './router'
import { BrandMark, ToastProvider, useToast } from './components/ui'
import { disablePush, syncPush } from './push'
import { LoginPage, SetupPage, InvitePage } from './pages/Auth'
import { HostsPage } from './pages/Hosts'
import { HostEditPage } from './pages/HostEdit'
import { KeysPage } from './pages/Keys'
import { SettingsPage } from './pages/Settings'
import { AdminUsersPage, AdminAuditPage } from './pages/Admin'
import { TerminalPage } from './pages/Terminal'

interface Session {
  boot: Bootstrap
  me: Me | null
  setMe: (m: Me | null) => void
}

const SessionCtx = createContext<Session>(null!)
const AUTH_EVENT = 'corral.authEvent'
export const useSession = () => useContext(SessionCtx)

export function App() {
  const [boot, setBoot] = useState<Bootstrap | null>(null)
  const [me, setMeState] = useState<Me | null>(null)
  // Every sign-in, sign-out or account switch: requests sent before it can no
  // longer sign this page out, and other tabs reload into the new state.
  // (Learning who is signed in at startup is not a change and tells no one.)
  const meRef = useRef<Me | null>(null)
  const booted = useRef(false)
  const setMe = useCallback((m: Me | null) => {
    if ((m?.id ?? null) !== (meRef.current?.id ?? null)) {
      bumpAuthGen()
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

  useEffect(() => {
    setUnauthorizedHandler(() => {
      resetPrefsSession()
      setMe(null)
      if (!location.pathname.startsWith('/login')) navigate('/login?next=' + encodeURIComponent(location.pathname), true)
    })
    api<Bootstrap>('GET', '/api/bootstrap')
      .then((b) => {
        applyDefaultLanguage(b.default_language)
        setBoot(b)
        setMe(b.user)
        booted.current = true
        if (b.user) syncPrefsFromServer(b.user.id)
      })
      .catch(() => setError(true))
  }, [])

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
  if (!boot) return null

  return (
    <SessionCtx.Provider value={{ boot, me, setMe }}>
      <ToastProvider>
        <PushSync userId={me?.id} />
        {route(path, boot, me)}
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

function Redirect({ to }: { to: string }) {
  useEffect(() => navigate(to, true), [to])
  return null
}

function route(path: string, boot: Bootstrap, me: Me | null): ReactNode {
  if (path === '/setup') return boot.setup_needed ? <SetupPage /> : <Redirect to="/login" />
  let m = match('/invite/:token', path)
  if (m) return <InvitePage token={m.token} />
  if (path === '/login') return me ? <Redirect to={nextPath()} /> : <LoginPage />
  if (!me) return <Redirect to={boot.setup_needed ? '/setup' : '/login?next=' + encodeURIComponent(path)} />

  if ((m = match('/t/:id', path))) return <TerminalPage key={m.id} id={m.id} />
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

type NavKey = 'hosts' | 'keys' | 'settings' | 'users' | 'audit'

// signOut ends the browser session. The push subscription of this device is
// removed first, so the next person signing in here does not get the previous
// account's alerts.
export async function signOut(setMe: (m: Me | null) => void, toast: (m: string, e?: boolean) => void) {
  // Settings changed just before signing out still belong to this account.
  await sendPendingPrefs(3000)
  await disablePush().catch(() => {})
  try {
    await api('POST', '/api/logout')
  } catch (e) {
    // A 401 means we were already signed out; anything else did not sign us out.
    if (!(e instanceof ApiError && e.status === 401)) {
      toast(t('nav.signOutFailed'), true)
      return
    }
  }
  resetPrefsSession()
  setMe(null)
  navigate('/login', true)
}

function Shell({ active, children }: { active: NavKey; children: ReactNode }) {
  const { me, setMe } = useSession()
  const toast = useToast()
  const main: { key: NavKey; href: string; label: string; icon: ReactNode }[] = [
    { key: 'hosts', href: '/', label: t('nav.hosts'), icon: <Server size={17} strokeWidth={1.75} /> },
    { key: 'keys', href: '/keys', label: t('nav.keys'), icon: <KeyRound size={17} strokeWidth={1.75} /> },
    { key: 'settings', href: '/settings', label: t('nav.settings'), icon: <Settings size={17} strokeWidth={1.75} /> },
  ]
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
  const tabActive = active === 'users' || active === 'audit' ? 'settings' : active
  return (
    <div className="shell">
      <nav className="sidebar" aria-label={t('nav.main')}>
        <a className="brand" href="/" onClick={linkHandler('/')} aria-label={t('app.name')}>
          <BrandMark />
          <span className="label">{t('app.name')}</span>
        </a>
        {main.map(item)}
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
