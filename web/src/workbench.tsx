import { useEffect, useRef, useState, type ReactNode } from 'react'
import { Bot, Film, KeyRound, LogOut, ScrollText, Search, Server, Settings, Users } from 'lucide-react'
import { openPalette, PALETTE_KEY } from './palette'
import { signOut, useSession } from './app'
import { t } from './i18n'
import { linkHandler, usePath } from './router'
import { useWaitingCount } from './terminal/agents'
import { MenuLayer, useToast } from './components/ui'
import { isDesktopSettings, openSettingsModal } from './pages/Settings'

// The workbench: one frame round every page on a desktop or tablet (760 and
// up) — an icon rail on the left (where to go), a status bar along the
// bottom (what the window is at), the page between. Phones keep their tab bar
// (workbench design 11-23). Each page draws the rail and the bar in its own
// colours: a terminal's scheme reaches them through --wb-* (terminal.css).

const WB_QUERY = '(min-width: 760px)'
export function useWorkbench(): boolean {
  const [m, setM] = useState(() => window.matchMedia(WB_QUERY).matches)
  useEffect(() => {
    const mq = window.matchMedia(WB_QUERY)
    const on = () => setM(mq.matches)
    mq.addEventListener('change', on)
    return () => mq.removeEventListener('change', on)
  }, [])
  return m
}

export type RailKey = 'agents' | 'hosts' | 'keys' | 'settings' | 'recordings' | 'users' | 'audit'

export function Rail({ active }: { active?: RailKey }) {
  const { me, setMe, boot } = useSession()
  const toast = useToast()
  const path = usePath()
  // (Read where the page is: the Shell polls for its pages, the agents page and
  // the tabs read the sessions themselves — the rail only shows the count.)
  const waiting = useWaitingCount(false)
  const [account, setAccount] = useState<DOMRect | null>(null)
  const meBtn = useRef<HTMLButtonElement>(null) // (this rail's: another page's, hidden, has one too)
  const items: { key: RailKey; href: string; label: string; icon: ReactNode }[] = [
    { key: 'agents', href: '/agents', label: t('nav.agents'), icon: <Bot size={19} strokeWidth={1.75} /> },
    { key: 'hosts', href: '/', label: t('nav.hosts'), icon: <Server size={19} strokeWidth={1.75} /> },
    { key: 'keys', href: '/keys', label: t('nav.keys'), icon: <KeyRound size={19} strokeWidth={1.75} /> },
    ...(boot.recordings ? [{ key: 'recordings' as const, href: '/recordings', label: t('nav.recordings'), icon: <Film size={19} strokeWidth={1.75} /> }] : []),
    ...(me?.role === 'admin'
      ? [
          { key: 'users' as const, href: '/admin/users', label: t('nav.users'), icon: <Users size={19} strokeWidth={1.75} /> },
          { key: 'audit' as const, href: '/admin/audit', label: t('nav.audit'), icon: <ScrollText size={19} strokeWidth={1.75} /> },
        ]
      : []),
  ]
  const n = waiting.n ?? 0
  const name = me?.display_name || me?.username || ''
  return (
    <nav className="wb-rail" aria-label={t('nav.main')}>
      <button type="button" className="wb-rail-item" onClick={openPalette} aria-label={t('palette.title')} title={t('palette.title') + (PALETTE_KEY ? ` (${PALETTE_KEY})` : '')} aria-keyshortcuts={PALETTE_KEY ? 'Meta+K' : undefined}>
        <Search size={19} strokeWidth={1.75} />
      </button>
      <span className="wb-rail-sep" aria-hidden="true" />
      {items.map((i) => (
        <a
          key={i.key}
          className="wb-rail-item"
          href={i.href}
          onClick={linkHandler(i.href)}
          aria-current={i.key === active ? 'page' : undefined}
          title={i.label}
          aria-label={i.key === 'agents' && n ? `${i.label}，${t(waiting.stale ? 'nav.waitingStaleN' : 'nav.waitingN', { n })}` : i.label}
        >
          {i.icon}
          {i.key === 'agents' && n > 0 && (
            <span className={'wb-rail-badge' + (waiting.stale ? ' is-stale' : '')} aria-hidden="true">
              {n > 99 ? '99+' : n}
            </span>
          )}
        </a>
      ))}
      <span className="wb-rail-gap" />
      <a
        className="wb-rail-item"
        href="/settings"
        aria-current={active === 'settings' ? 'page' : undefined}
        title={t('nav.settings') + ' (' + (/Mac|iPhone|iPad/.test(navigator.platform) ? '⌘,' : 'Ctrl+,') + ')'}
        aria-label={t('nav.settings')}
        onClick={(e) => {
          if (!e.metaKey && !e.ctrlKey && !e.shiftKey && e.button === 0 && isDesktopSettings() && path !== '/settings') {
            e.preventDefault()
            openSettingsModal(active === 'agents' ? 'chat' : undefined) // (from the conversations: their own part)
            return
          }
          linkHandler('/settings')(e)
        }}
      >
        <Settings size={19} strokeWidth={1.75} />
      </a>
      <button
        type="button"
        ref={meBtn}
        className="wb-rail-item wb-rail-me"
        aria-label={t('wb.account', { name })}
        title={name}
        aria-haspopup="menu"
        aria-expanded={!!account}
        onClick={(e) => setAccount(e.currentTarget.getBoundingClientRect())}
      >
        <span className="wb-rail-avatar" aria-hidden="true">
          {(name.trim()[0] ?? '?').toUpperCase()}
        </span>
      </button>
      {account && (
        <MenuLayer
          label={t('wb.account', { name })}
          note={name} // (the popover shows no heading: who is signed in, said above the act)
          anchor={new DOMRect(account.right + 4, account.top, 0, 0)}
          at="point"
          sheet={false}
          trigger={() => meBtn.current}
          items={[{ label: t('nav.signOut'), icon: <LogOut size={16} />, onSelect: () => void signOut(setMe, toast) }]}
          onClose={() => setAccount(null)}
        />
      )}
    </nav>
  )
}

// The status bar: on its left what the page in front is at (its connection,
// branch, permission mode — the page gives it), on its right how many agents
// wait for the person and work, across every session.
export function StatusBar({ children }: { children?: ReactNode }) {
  const counts = useWaitingCount(false)
  const parts: string[] = []
  if (counts.n) parts.push(t('wb.waitingN', { n: counts.n }))
  if (counts.working) parts.push(t('wb.workingN', { n: counts.working }))
  return (
    <footer className="wb-status" aria-label={t('wb.status')}>
      <div className="wb-status-left">{children}</div>
      <span className="wb-status-gap" />
      {parts.length > 0 && (
        <span className={'wb-status-counts' + (counts.stale ? ' is-stale' : '')} title={counts.stale ? t('nav.waitingStale') : undefined}>
          {parts.join(' · ')}
        </span>
      )}
    </footer>
  )
}

// One item of the status bar's left: a mark, words, a title saying more.
export function StatusItem({ children, title, mark, onClick, tone }: { children: ReactNode; title?: string; mark?: 'on' | 'off' | 'trouble'; onClick?: () => void; tone?: 'danger' | 'warn' }) {
  const cls = 'wb-status-item' + (tone ? ' is-' + tone : '')
  const body = (
    <>
      {mark && <span className={'wb-status-mark is-' + mark} aria-hidden="true" />}
      {children}
    </>
  )
  return onClick ? (
    <button type="button" className={cls + ' is-btn'} title={title} onClick={onClick}>
      {body}
    </button>
  ) : (
    <span className={cls} title={title}>
      {body}
    </span>
  )
}
