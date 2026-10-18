import { createContext, useContext, useEffect, useState, type ReactNode } from 'react'
import { CircleUserRound, Fence, KeyRound, LogOut, Menu as MenuIcon, ScrollText, Server, Settings, Users } from 'lucide-react'
import { api, ApiError, setUnauthorizedHandler, type Bootstrap, type Me } from './api'
import { applyDefaultLanguage, t } from './i18n'
import { syncPrefsFromServer } from './prefs'
import { match, navigate, usePath, linkHandler } from './router'
import { Menu, ToastProvider, useToast } from './components/ui'
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
export const useSession = () => useContext(SessionCtx)

export function App() {
  const [boot, setBoot] = useState<Bootstrap | null>(null)
  const [me, setMe] = useState<Me | null>(null)
  const [error, setError] = useState(false)
  const path = usePath()

  useEffect(() => {
    setUnauthorizedHandler(() => {
      setMe(null)
      if (!location.pathname.startsWith('/login')) navigate('/login?next=' + encodeURIComponent(location.pathname), true)
    })
    api<Bootstrap>('GET', '/api/bootstrap')
      .then((b) => {
        applyDefaultLanguage(b.default_language)
        setBoot(b)
        setMe(b.user)
        if (b.user) syncPrefsFromServer()
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
      <ToastProvider>{route(path, boot, me)}</ToastProvider>
    </SessionCtx.Provider>
  )
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

function Shell({ active, children }: { active: NavKey; children: ReactNode }) {
  const { me, setMe } = useSession()
  const toast = useToast()
  const items: { key: NavKey; href: string; label: string; icon: ReactNode; admin?: boolean }[] = [
    { key: 'hosts', href: '/', label: t('nav.hosts'), icon: <Server size={17} /> },
    { key: 'keys', href: '/keys', label: t('nav.keys'), icon: <KeyRound size={17} /> },
    { key: 'settings', href: '/settings', label: t('nav.settings'), icon: <Settings size={17} /> },
    { key: 'users', href: '/admin/users', label: t('nav.users'), icon: <Users size={17} />, admin: true },
    { key: 'audit', href: '/admin/audit', label: t('nav.audit'), icon: <ScrollText size={17} />, admin: true },
  ]
  const visible = items.filter((i) => !i.admin || me?.role === 'admin')
  const signOut = async () => {
    try {
      await api('POST', '/api/logout')
    } catch (e) {
      // A 401 means we were already signed out; anything else did not sign us out.
      if (!(e instanceof ApiError && e.status === 401)) {
        toast(t('nav.signOutFailed'), true)
        return
      }
    }
    setMe(null)
    navigate('/login', true)
  }
  return (
    <div className="shell">
      <header className="topbar">
        <a className="brand" href="/" onClick={linkHandler('/')}>
          <Fence size={20} aria-hidden="true" />
          {t('app.name')}
        </a>
        <nav className="topnav" aria-label={t('common.more')}>
          {visible.map((i) => (
            <a key={i.key} href={i.href} onClick={linkHandler(i.href)} aria-current={i.key === active ? 'page' : undefined}>
              {i.label}
            </a>
          ))}
        </nav>
        <div className="spacer" />
        <Menu
          button={(open, toggle) => (
            <button className="icon-btn" aria-label={t('nav.account')} aria-expanded={open} onClick={toggle}>
              <MenuIcon size={20} className="nav-menu-only" />
              <CircleUserRound size={21} className="nav-wide-only" />
            </button>
          )}
        >
          {(close) => (
            <>
              <div className="menu-note">{me?.display_name || me?.username}</div>
              {visible.map((i) => (
                <a key={i.key} href={i.href} className="nav-menu-only" onClick={(e) => { close(); linkHandler(i.href)(e) }}>
                  {i.icon}
                  {i.label}
                </a>
              ))}
              <hr className="nav-menu-only" />
              <button onClick={signOut}>
                <LogOut size={17} />
                {t('nav.signOut')}
              </button>
            </>
          )}
        </Menu>
      </header>
      {children}
    </div>
  )
}
