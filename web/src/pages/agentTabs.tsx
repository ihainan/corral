import { useEffect, useRef, useState } from 'react'
import { ChevronDown, ExternalLink, Link2, Plus, X } from 'lucide-react'
import { t } from '../i18n'
import { local, session } from '../storage'
import { AgentIcon } from '../terminal/agentIcons'
import { useTabDrag } from '../terminal/tabDrag'
import { copyText, isCoarse, MenuLayer, useToast, type MenuItem } from '../components/ui'

// The agents' page's tabs (11-24, plans/agent-tabs.md): each a conversation
// the person has open, so several are at hand; the side list is every agent,
// the tabs the ones being looked at. A tab is a record only: the one in
// front is shown (with its split), going to another shows it in its place.
// Closing a tab only stops showing it: its agent goes on.

export type AgentTab = {
  id: string // (the tab's own: kept as the agent in it changes)
  key: string // (its agent's entry key: outlasts a session, as after a gateway restart)
  s: string // (the session it was last found in)
  pane?: string
  split?: string // (the entry key of the agent beside it, if split)
  focus?: 'primary' | 'secondary'
  title?: string // (as last shown: what the tab says once its agent is gone)
  program?: string
}

// Kept in the browser, of one account: the list shared by its windows, the
// one in front each window's own.
const TABS_KEY = 'corral.agentTabs'
const FRONT_KEY = 'corral.agentFront'
type Kept = { user: number; tabs: AgentTab[] }

// The tabs kept for this account; null when none were ever kept (then the
// page opens as it did before tabs: the agent last shown, else the first).
export function readTabs(user: number): AgentTab[] | null {
  try {
    const k = JSON.parse(local.get(TABS_KEY) ?? 'null') as Kept | null
    if (!k || k.user !== user || !Array.isArray(k.tabs)) return null
    return k.tabs.filter((x) => x && typeof x.id === 'string' && typeof x.key === 'string' && typeof x.s === 'string')
  } catch {
    return null
  }
}
export const writeTabs = (user: number, tabs: AgentTab[]) => local.set(TABS_KEY, JSON.stringify({ user, tabs } satisfies Kept))
export const readFront = (): string | null => session.get(FRONT_KEY)
export const writeFront = (id: string | null) => (id ? session.set(FRONT_KEY, id) : session.remove(FRONT_KEY))
export const isTabsKey = (k: string | null) => k === TABS_KEY
export const newTabId = () => Math.random().toString(36).slice(2, 10)

// ⌘T: the quick switcher opened for a new tab — what is picked in it next
// opens in one. (Dropped when the switcher closes without a pick.)
let pickInTab = false
export const askPickInTab = () => void (pickInTab = true)
export const takePickInTab = () => {
  const v = pickInTab
  pickInTab = false
  return v
}
export const dropPickInTab = () => void (pickInTab = false)
export const OPEN_PICKER = 'corral-open-picker'

// How a pick from a list opens: in the tab in front (none said), a new tab, or beside it.
export type PickHow = 'tab' | 'split'
// From a click: ⌘ or Ctrl a new tab, ⌥ or Alt beside it (as Claude Code's desktop app and Slack).
export const howOf = (e: { metaKey: boolean; ctrlKey: boolean; altKey: boolean }): PickHow | undefined => (e.metaKey || e.ctrlKey ? 'tab' : e.altKey ? 'split' : undefined)

// What a tab shows of its agent: its name, program and state, as the side list has them.
const STATUS_WORD: Record<string, 'agentTabs.status.blocked' | 'agentTabs.status.working' | 'agentTabs.status.done' | 'agentTabs.status.idle'> = {
  blocked: 'agentTabs.status.blocked',
  working: 'agentTabs.status.working',
  done: 'agentTabs.status.done',
  idle: 'agentTabs.status.idle',
}
const statusWord = (st: string) => (STATUS_WORD[st] ? t(STATUS_WORD[st]) : '')
export type TabView = { tab: AgentTab; title: string; program?: string; status: string; ended: boolean; address: string }

export function AgentTabBar(p: {
  views: TabView[]
  front: string | null
  onGo: (tab: AgentTab) => void
  onClose: (tab: AgentTab) => void
  onCloseMany: (list: AgentTab[]) => void
  onMove: (id: string, to: number) => void
  onNew: () => void
  newKey?: string
}) {
  const toast = useToast()
  const ref = useRef<HTMLElement>(null)
  const ids = p.views.map((v) => v.tab.id)
  const { drag, onPointerDown, offset, dragged, moved } = useTabDrag(ref, ids, p.front ?? '', p.onMove)
  const order = ids.join()
  useEffect(() => {
    if (moved.current) {
      moved.current = false
      return
    }
    ref.current?.querySelector<HTMLElement>('[aria-current="page"]')?.scrollIntoView({ block: 'nearest', inline: 'nearest' })
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [p.front, order])
  // Tabs beyond either end: that end fades, and the list of them all is at hand (as Firefox's, JetBrains').
  const [fade, setFade] = useState('')
  useEffect(() => {
    const el = ref.current
    if (!el) return
    const look = () => {
      const l = el.scrollLeft > 1
      const r = el.scrollLeft + el.clientWidth < el.scrollWidth - 1
      setFade((l ? ' fade-l' : '') + (r ? ' fade-r' : ''))
    }
    look()
    el.addEventListener('scroll', look, { passive: true })
    const ro = new ResizeObserver(look)
    ro.observe(el)
    for (const c of el.children) ro.observe(c)
    return () => {
      el.removeEventListener('scroll', look)
      ro.disconnect()
    }
  }, [order])
  const [menu, setMenu] = useState<{ id: string; anchor: DOMRect } | null>(null)
  const [all, setAll] = useState<DOMRect | null>(null)
  const allBtn = useRef<HTMLButtonElement>(null)
  const menuItems = (v: TabView): MenuItem[] => {
    const at = ids.indexOf(v.tab.id)
    const others = p.views.filter((x) => x.tab.id !== v.tab.id).map((x) => x.tab)
    const right = p.views.slice(at + 1).map((x) => x.tab)
    const url = location.origin + v.address
    return [
      { label: t('tabs.menu.window'), icon: <ExternalLink size={16} />, onSelect: () => window.open(url, '_blank', 'noopener') },
      { label: t('tabs.menu.copyLink'), icon: <Link2 size={16} />, onSelect: () => void copyText(url).then((ok) => toast(ok ? t('tabs.menu.copied') : url)) },
      { label: t('agentTabs.close'), icon: <X size={16} />, separator: true, onSelect: () => p.onClose(v.tab) },
      { label: t('agentTabs.closeOthers', { n: others.length }), icon: <span className="menu-icon-space" />, disabled: !others.length, onSelect: () => p.onCloseMany(others) },
      { label: t('agentTabs.closeRight', { n: right.length }), icon: <span className="menu-icon-space" />, disabled: !right.length, onSelect: () => p.onCloseMany(right) },
    ]
  }
  const menuView = menu && p.views.find((v) => v.tab.id === menu.id)
  const dot = (st: string) => (st === 'blocked' ? 'term-tab-wait' : st === 'working' ? 'term-tab-busy' : st === 'done' ? 'term-tab-done' : '')
  return (
    <>
      <nav className={'term-tabs agent-tabs' + (drag ? ' dragging' : '') + fade} aria-label={t('tabs.agentsLabel')} ref={ref}>
        {p.views.map((v, j) => {
          const dx = offset(j)
          const front = v.tab.id === p.front
          const tip = [v.title, v.ended ? t('agentTabs.ended') : statusWord(v.status)].filter(Boolean).join(' · ')
          return (
            <div
              key={v.tab.id}
              data-tab={v.tab.id}
              className={'term-tab has-x' + (drag?.id === v.tab.id ? ' held' : '') + (v.ended ? ' is-ended' : '')}
              data-place={j}
              style={dx ? { transform: `translateX(${dx}px)` } : undefined}
              aria-current={front ? 'page' : undefined}
              title={tip}
              onPointerDown={(e) => onPointerDown(e, v.tab.id, j)}
              onAuxClick={(e) => e.button === 1 && (e.preventDefault(), p.onClose(v.tab))}
              onContextMenu={(e) => {
                e.preventDefault()
                const r = e.clientX || e.clientY ? new DOMRect(e.clientX, e.clientY, 0, 0) : e.currentTarget.getBoundingClientRect()
                setMenu({ id: v.tab.id, anchor: r })
              }}
            >
              <button type="button" className="term-tab-main" aria-current={front ? 'page' : undefined} onClick={() => !dragged() && !front && p.onGo(v.tab)}>
                <AgentIcon program={v.program} size={15} bare />
                <span className="term-tab-name">{v.title}</span>
              </button>
              {/* (Its state and its close in one place: see terminal.css.) */}
              <span className="term-tab-end">
                {!v.ended && dot(v.status) && (
                  <span className={'term-tab-count ' + dot(v.status)} role="img" aria-label={statusWord(v.status)}>
                    <span className="term-tab-sdot" aria-hidden="true" />
                  </span>
                )}
                <button type="button" className="term-tab-x" aria-label={t('agentTabs.closeTab', { name: v.title })} title={t('agentTabs.close')} onPointerDown={(e) => e.stopPropagation()} onClick={() => p.onClose(v.tab)}>
                  <X size={14} aria-hidden="true" />
                </button>
              </span>
            </div>
          )
        })}
      </nav>
      {fade && (
        <button type="button" ref={allBtn} className="icon-btn term-tab-new" aria-haspopup="menu" aria-expanded={!!all} aria-label={t('agentTabs.all')} title={t('agentTabs.all')} onClick={(e) => setAll(e.currentTarget.getBoundingClientRect())}>
          <ChevronDown size={17} />
        </button>
      )}
      <button type="button" className="icon-btn term-tab-new" onClick={p.onNew} aria-label={t('agentTabs.new')} title={p.newKey ? `${t('agentTabs.new')} (${p.newKey})` : t('agentTabs.new')}>
        <Plus size={17} />
      </button>
      {menu && menuView && (
        <MenuLayer
          label={t('tabs.menu.label', { name: menuView.title })}
          heading={menuView.title}
          anchor={menu.anchor}
          at="point"
          sheet={isCoarse()}
          trigger={() => ref.current?.querySelector<HTMLElement>(`[data-tab="${menu.id}"] .term-tab-main`) ?? null}
          items={menuItems(menuView)}
          onClose={() => setMenu(null)}
        />
      )}
      {all && (
        <MenuLayer
          label={t('agentTabs.all')}
          heading={t('agentTabs.all')}
          anchor={all}
          sheet={isCoarse()}
          trigger={() => allBtn.current}
          items={p.views.map((v) => ({
            label: v.title,
            icon: <AgentIcon program={v.program} size={16} bare />,
            hint: v.ended ? t('agentTabs.ended') : statusWord(v.status) || undefined,
            onSelect: () => v.tab.id !== p.front && p.onGo(v.tab),
          }))}
          onClose={() => setAll(null)}
        />
      )}
    </>
  )
}
