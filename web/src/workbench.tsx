import { memo, useCallback, useEffect, useMemo, useRef, useState, type ReactNode } from 'react'
import { Bot, Film, KeyRound, LogOut, ScrollText, Search, Server, Settings, SquareTerminal, Users } from 'lucide-react'
import { openPalette, PALETTE_KEY } from './palette'
import { signOut, useSession } from './app'
import { t } from './i18n'
import { linkHandler, navigate, usePath } from './router'
import { countedAgents, rowTitle, useWaitingCount } from './terminal/agents'
import { AgentIcon } from './terminal/agentIcons'
import { MenuLayer, useToast, type MenuItem } from './components/ui'
import { Avatar } from './components/Avatar'
import { getPrefs } from './prefs'
import { modalShown } from './shown'
import { onMac, railKey, railKeyName, RAIL_KEYS, takenFromTerminal, terminalHasKeys } from './terminal/pageKeys'

// A field one types in: not a terminal's (its own textarea: the terminal's keys are terminalHasKeys').
const typedIn = (t: EventTarget | null) =>
  t instanceof HTMLElement && !t.classList.contains('xterm-helper-textarea') && (t.isContentEditable || t.tagName === 'TEXTAREA' || (t.tagName === 'INPUT' && !['checkbox', 'radio', 'button', 'submit', 'range', 'color', 'file'].includes((t as HTMLInputElement).type)))
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
    on() // (shown again from a tab behind: the width may have changed meanwhile)
    mq.addEventListener('change', on)
    return () => mq.removeEventListener('change', on)
  }, [])
  return m
}

export type RailKey = 'agents' | 'terminals' | 'hosts' | 'keys' | 'settings' | 'recordings' | 'users' | 'audit'

export function Rail({ active }: { active?: RailKey }) {
  const { me, setMe, boot } = useSession()
  const toast = useToast()
  const path = usePath()
  // (Read where the page is: the Shell polls for its pages, the agents page and
  // the tabs read the sessions themselves — the rail only shows the count.)
  const waiting = useWaitingCount(false)
  const [account, setAccount] = useState<DOMRect | null>(null)
  const meBtn = useRef<HTMLButtonElement>(null) // (this rail's: another page's, hidden, has one too)
  const navRef = useRef<HTMLElement>(null)
  // Alt+1…9: its pages, as they go down (railKey). Every page's rail listens; the one shown goes.
  // Not under a dialog; a terminal focused and it first, the program's (as ⌥↑↓); before the
  // terminal's own listener (capture), as it stops the key once it sends it. On a Mac, not
  // in a field one types in: ⌥ and a digit type there (⌥8 •, ⌥2 ™).
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      const n = railKey(e)
      const nav = navRef.current
      if (!n || !nav || !nav.getClientRects().length || e.defaultPrevented || modalShown() || terminalHasKeys(e, getPrefs().keysFirst)) return
      if (onMac() && typedIn(e.target)) return
      const to = nav.querySelectorAll<HTMLAnchorElement>('a[data-rail-key]')[n - 1]
      if (!to) return
      e.preventDefault()
      e.stopPropagation()
      if (e.repeat) return
      takenFromTerminal(e, RAIL_KEYS, (m) => toast(m), (key) => t('keys.taken', { key }))
      if (to.getAttribute('aria-current') !== 'page') to.click()
    }
    window.addEventListener('keydown', onKey, true)
    return () => window.removeEventListener('keydown', onKey, true)
  }, [toast])
  const items: { key: RailKey; href: string; label: string; icon: ReactNode }[] = [
    { key: 'agents', href: '/agents', label: t('nav.agents'), icon: <Bot size={19} strokeWidth={1.75} /> },
    // (The terminals' page, apart from the agents' since 11-24: the terminal last in front there.)
    { key: 'terminals', href: '/t', label: t('nav.terminals'), icon: <SquareTerminal size={19} strokeWidth={1.75} /> },
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
    <nav className="wb-rail" ref={navRef} aria-label={t('nav.main')}>
      <button type="button" className="wb-rail-item" onClick={openPalette} aria-label={t('palette.title')} title={t('palette.title') + (PALETTE_KEY ? ` (${PALETTE_KEY})` : '')} aria-keyshortcuts={PALETTE_KEY ? 'Meta+K' : undefined}>
        <Search size={19} strokeWidth={1.75} />
      </button>
      <span className="wb-rail-sep" aria-hidden="true" />
      {items.map((i, at) => (
        <a
          key={i.key}
          className="wb-rail-item"
          href={i.href}
          onClick={linkHandler(i.href)}
          aria-current={i.key === active ? 'page' : undefined}
          data-rail-key
          title={at < 9 ? `${i.label} (${railKeyName(at + 1)})` : i.label}
          aria-keyshortcuts={at < 9 ? `Alt+${at + 1}` : undefined}
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
        <Avatar id={me?.id} at={me?.avatar_at} name={name} className="wb-rail-avatar" />
      </button>
      {account && (
        <MenuLayer
          label={t('wb.account', { name })}
          note={name} // (the popover shows no heading: who is signed in, said above the act)
          // (From its bottom corner: at the rail's foot the menu goes up, so its foot meets the avatar's, beside
          // it — not over the settings button above, as it did from the top. Doc UI item 5, 11-24.)
          anchor={new DOMRect(account.right + 4, account.bottom, 0, 0)}
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
  // (Pressed: which they are — those waiting, then at work —, each to go to. As
  // they were as it opened: redrawn by each read, it moved the focus back to its
  // first, or kept those of before — review of 11-23.)
  const [list, setList] = useState<{ at: DOMRect; items: MenuItem[]; note: string } | null>(null)
  const btn = useRef<HTMLButtonElement>(null)
  const trigger = useCallback(() => btn.current, [])
  const close = useCallback(() => setList(null), [])
  const items = (): MenuItem[] => {
    const { waiting, working } = countedAgents()
    const item = (r: (typeof waiting)[number], i: number, first: boolean): MenuItem => ({
      label: rowTitle(r) + (r.s.host_name ? ' · ' + r.s.host_name : ''),
      icon: <AgentIcon program={r.a.agent} size={16} bare />,
      separator: first && i === 0,
      onSelect: () => navigate('/agents?' + new URLSearchParams({ s: r.s.id, pane: r.a.pane_id })),
    })
    return [...waiting.map((r, i) => item(r, i, false)), ...working.map((r, i) => item(r, i, waiting.length > 0))]
  }
  return (
    <footer className="wb-status" aria-label={t('wb.status')}>
      <div className="wb-status-left">{children}</div>
      <span className="wb-status-gap" />
      {parts.length > 0 && (
        <button
          type="button"
          ref={btn}
          className={'wb-status-counts wb-status-item is-btn' + (counts.stale ? ' is-stale' : '')}
          title={counts.stale ? t('nav.waitingStale') : t('wb.countsList')}
          aria-haspopup="menu"
          aria-expanded={!!list}
          onClick={(e) => setList({ at: e.currentTarget.getBoundingClientRect(), items: items(), note: parts.join(' · ') })}
        >
          {parts.join(' · ')}
        </button>
      )}
      {list && <CountsMenu at={list.at} items={list.items} note={list.note} trigger={trigger} onClose={close} />}
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

// (Its anchor made once: a new one each draw would open it again.)
const CountsMenu = memo(function CountsMenu({ at, items, note, trigger, onClose }: { at: DOMRect; items: MenuItem[]; note: string; trigger: () => HTMLElement | null; onClose: () => void }) {
  const anchor = useMemo(() => new DOMRect(at.right, at.top - 4, 0, 0), [at])
  return <MenuLayer label={t('wb.countsList')} note={note} anchor={anchor} at="point" sheet={false} trigger={trigger} items={items} onClose={onClose} />
})
