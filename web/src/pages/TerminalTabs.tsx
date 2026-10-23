import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { Bot, Check, MessageCircleReply, Plus, Search } from 'lucide-react'
import { api, type Host, type SessionInfo } from '../api'
import { sessionWhich } from '../terminal/sessionName'
import { t } from '../i18n'
import { local } from '../storage'
import { navigate } from '../router'
import { takeTabs } from '../notifyopen'
import { Dialog } from '../components/ui'
import { useConnect } from '../connect'
import { TerminalPage, type TabInfo } from './Terminal'

// Tabs where there is room for them: desktops and tablets (an iPad with a
// keyboard switches with Ctrl+Tab too). Phones keep the session switcher.
const TABS_QUERY = '(min-width: 700px)'
export function useTabsMode(): boolean {
  const [m, setM] = useState(() => window.matchMedia(TABS_QUERY).matches)
  useEffect(() => {
    const mq = window.matchMedia(TABS_QUERY)
    const on = () => setM(mq.matches)
    mq.addEventListener('change', on)
    return () => mq.removeEventListener('change', on)
  }, [])
  return m
}

// Terminals kept connected in the back; the one used longest ago goes first.
const MAX_MOUNTED = 8
const POLL_MS = 5000

// An installed app's window (no tabs of the browser's) passes on the keys a
// terminal app uses for its tabs; a browser keeps them for its own, and some
// of them a page could take from it, so they are taken only there.
const standalone = window.matchMedia('(display-mode: standalone)').matches || (navigator as { standalone?: boolean }).standalone === true
// (iPads say Mac too: their keyboards have ⌘.)
const isMac = /Mac|iPhone|iPad/.test(navigator.platform)
const newTabKey = isMac ? '⌘T' : 'Ctrl+Shift+T'

// The user's running sessions as tabs, like a terminal app's: a tab once
// opened stays connected in the back (up to MAX_MOUNTED), so switching is
// instant. Ctrl+Tab and Ctrl+Shift+Tab go to the next and previous tab; a
// browser keeps them for its own tabs, an installed app passes them on.
// The installed app also has a new tab (⌘T; Ctrl+Shift+T elsewhere, as
// Ctrl+letters belong to the programs) and, on a Mac, ⌘1…⌘8 for a tab by
// its place, ⌘9 for the last and ⌘⇧[ ⌘⇧] for the previous and next. No key
// closes a tab: that ends its session.
// Tabs keep their places. A tab is the session it shows: tmux's or herdr's
// session on its host (opened again after a gateway restart or a lost
// connection, it is a new session of the gateway's, in the same place), a
// plain shell itself. Places are kept on this device; a new tab goes last.
// A tab gone for longer than a reconnection takes loses its place: opened
// again later, it goes last.
const ORDER_KEY = 'corral.tabOrder'
const KEEP_GONE_MS = 10 * 60 * 1000
const placeKey = (s: SessionInfo) =>
  s.persistence === 'tmux' || s.persistence === 'herdr' ? `${s.host_id}|${s.persistence}|${s.mux_session ?? ''}` : 'id|' + s.id

function inPlace(list: SessionInfo[]): SessionInfo[] {
  const now = Date.now()
  let order: [string, number][] = [] // place key, last seen
  try {
    const v = JSON.parse(local.get(ORDER_KEY) ?? '[]')
    if (Array.isArray(v)) order = v.filter((x) => Array.isArray(x) && typeof x[0] === 'string' && typeof x[1] === 'number')
  } catch {
    /* not ours */
  }
  const open = new Set(list.map(placeKey))
  order = order.filter(([k, seen]) => open.has(k) || now - seen < KEEP_GONE_MS)
  const known = new Set(order.map(([k]) => k))
  for (const s of [...list].sort((a, b) => a.created_at - b.created_at)) {
    const k = placeKey(s)
    if (!known.has(k)) {
      order.push([k, now])
      known.add(k)
    }
  }
  order = order.map(([k, seen]) => [k, open.has(k) ? now : seen])
  local.set(ORDER_KEY, JSON.stringify(order))
  const keys = order.map(([k]) => k)
  const at = (s: SessionInfo) => keys.indexOf(placeKey(s))
  return [...list].sort((a, b) => at(a) - at(b) || a.created_at - b.created_at)
}

export function TerminalTabs({ current }: { current: string }) {
  const [sessions, setSessions] = useState<SessionInfo[] | null>(null)
  const [mounted, setMounted] = useState<string[]>([current])
  const [infos, setInfos] = useState<Record<string, TabInfo>>({})
  const [newOpen, setNewOpen] = useState(false)

  // A list counts from when it was asked for. One request at a time: asked
  // again meanwhile, it is asked once more when the first one is done.
  const [listAt, setListAt] = useState(0)
  const loading = useRef<'no' | 'yes' | 'again'>('no')
  const load = useCallback(() => {
    if (loading.current !== 'no') {
      loading.current = 'again'
      return
    }
    loading.current = 'yes'
    const at = Date.now()
    api<SessionInfo[]>('GET', '/api/sessions')
      .then((l) => {
        setSessions(inPlace(l.filter((x) => x.state === 'running')))
        setListAt(at)
      })
      .catch(() => {}) // (the tabs stay as they were; the next poll tries again)
      .finally(() => {
        const again = loading.current === 'again'
        loading.current = 'no'
        if (again) load()
      })
  }, [])
  useEffect(() => {
    load()
    const timer = window.setInterval(() => document.visibilityState === 'visible' && load(), POLL_MS)
    return () => clearInterval(timer)
  }, [load])
  useEffect(load, [current, load]) // a session just opened shows its tab with its name

  // The current one moves to the end of the mounted list; the oldest is let go.
  const mountedAt = useRef(new Map<string, number>())
  useEffect(() => {
    if (!mountedAt.current.has(current)) mountedAt.current.set(current, Date.now())
    setMounted((m) => {
      const next = [...m.filter((x) => x !== current), current]
      const kept = next.length > MAX_MOUNTED ? next.slice(next.length - MAX_MOUNTED) : next
      for (const id of next) if (!kept.includes(id)) mountedAt.current.delete(id)
      return kept
    })
  }, [current])
  // A mounted session that ended goes (unless shown): one missing from a list
  // asked for after it was opened (an older list may simply not have it yet).
  useEffect(() => {
    if (!sessions) return
    setMounted((m) => {
      // (One being opened again by itself stays until that is done.)
      const next = m.filter((x) => x === current || sessions.some((s) => s.id === x) || (mountedAt.current.get(x) ?? 0) >= listAt || infos[x]?.reopening)
      for (const id of m) if (!next.includes(id)) mountedAt.current.delete(id)
      return next.length === m.length ? m : next
    })
  }, [sessions, listAt, current, infos])

  // A tab let go of tells no more: what its page last said goes (the session
  // list's word stands for it again).
  useEffect(() => {
    setInfos((all) => {
      const keys = Object.keys(all).filter((k) => mounted.includes(k))
      return keys.length === Object.keys(all).length ? all : Object.fromEntries(keys.map((k) => [k, all[k]]))
    })
  }, [mounted])
  const onInfo = useCallback((id: string, i: TabInfo) => {
    setInfos((all) => {
      const old = all[id]
      return old && old.status === i.status && old.statusText === i.statusText && old.waiting === i.waiting && old.done === i.done && old.reopening === i.reopening ? all : { ...all, [id]: i }
    })
  }, [])

  // The tabs: the running sessions, and the current one even before the list
  // has it (just opened) or after it ended (its end is shown).
  const tabs = useMemo(() => {
    const list = sessions ?? []
    return list.some((s) => s.id === current) ? list : [...list, { id: current, host_id: 0, host_name: '', title: '' } as SessionInfo]
  }, [sessions, current])

  const go = useCallback(
    (dir: 1 | -1) => {
      const i = tabs.findIndex((s) => s.id === current)
      if (tabs.length < 2 || i < 0) return
      navigate('/t/' + tabs[(i + dir + tabs.length) % tabs.length].id, true)
    },
    [tabs, current],
  )
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      // (Not under a dialog: it keeps the keys and the focus.)
      // (Nor while an input method composes: the key is its.)
      if (e.altKey || e.isComposing || e.keyCode === 229 || document.querySelector('[aria-modal="true"]')) return
      // T and the brackets by the character the layout gives (Dvorak's T is
      // where QWERTY has K), by the key's place only where it gives none
      // (Cyrillic). The digits by place: a French keyboard's ⌘1 is ⌘&, as in
      // the Mac's own apps.
      const plain = /^[\x21-\x7e]$/.test(e.key)
      const isT = e.key.toLowerCase() === 't' || (!plain && e.code === 'KeyT')
      const bracket = e.key === '{' || (!plain && e.code === 'BracketLeft') ? -1 : e.key === '}' || (!plain && e.code === 'BracketRight') ? 1 : 0
      let act: (() => void) | null = null
      if (e.key === 'Tab' && e.ctrlKey && !e.metaKey) {
        if (tabs.length >= 2) act = () => go(e.shiftKey ? -1 : 1)
      } else if (standalone && isMac && e.metaKey && !e.ctrlKey) {
        const digit = /^Digit([1-9])$/.exec(e.code)
        if (isT && !e.shiftKey) act = () => setNewOpen(true)
        else if (digit && !e.shiftKey) {
          const n = Number(digit[1])
          const to = n === 9 ? tabs[tabs.length - 1] : tabs[n - 1]
          act = () => to && to.id !== current && navigate('/t/' + to.id, true)
        } else if (bracket && e.shiftKey) act = () => go(bracket as 1 | -1)
      } else if (standalone && !isMac && e.ctrlKey && e.shiftKey && !e.metaKey && isT) act = () => setNewOpen(true)
      if (!act) return
      // Before the terminal's own listeners: the program never sees it.
      e.preventDefault()
      e.stopPropagation()
      // One step per press: a key held a little long repeats (after the
      // system's delay, which can be short), and each repeat went one tab on.
      if (!e.repeat) act()
    }
    window.addEventListener('keydown', onKey, true)
    return () => window.removeEventListener('keydown', onKey, true)
  }, [go, tabs, current])

  // A notification of another session (tapped while this window shows
  // tabs) opens as its tab here, with what its address asks for (?reply=).
  useEffect(() => {
    takeTabs((u) => {
      if (!/^\/t\/[0-9a-f]+$/.test(u.pathname) || document.querySelector('[aria-modal="true"]')) return false
      navigate(u.pathname + u.search)
      return true
    })
    return () => takeTabs(null)
  }, [])

  const { connect, cancel, busyHost, dialogs } = useConnect((s) => {
    setNewOpen(false)
    navigate('/t/' + s.id)
  })
  // The host's own questions (what to open, a password) replace the list.
  useEffect(() => {
    if (dialogs) setNewOpen(false)
  }, [dialogs])

  // A session the user ended: its neighbour (the next tab, else the one
  // before) comes to the front, or the host list when it was the last.
  // (Read when it happens: the confirmation holds an older copy of this.)
  const tabsRef = useRef(tabs)
  tabsRef.current = tabs
  const endTab = (id: string) => {
    const tabs = tabsRef.current
    const i = tabs.findIndex((s) => s.id === id)
    const rest = tabs.filter((s) => s.id !== id && s.host_id !== 0)
    const next = rest[Math.min(Math.max(i, 0), rest.length - 1)]
    setSessions((l) => l && l.filter((s) => s.id !== id))
    navigate(next ? '/t/' + next.id : '/', true)
    load()
  }

  const bar = <TabBar tabs={tabs} current={current} infos={infos} onNew={() => setNewOpen(true)} />
  return (
    <>
      {mounted.map((id) => (
        <TerminalPage key={id} id={id} active={id === current} tabs={bar} onInfo={onInfo} onEnd={() => endTab(id)} />
      ))}
      {newOpen && (
        <NewTabDialog
          busyHost={busyHost}
          onPick={(h) => connect(h)}
          onClose={() => {
            cancel() // (a connection under way is dropped, not opened later)
            setNewOpen(false)
          }}
        />
      )}
      {dialogs}
    </>
  )
}

function TabBar(p: { tabs: SessionInfo[]; current: string; infos: Record<string, TabInfo>; onNew: () => void }) {
  const ref = useRef<HTMLElement>(null)
  // The current tab is scrolled into view when it changes.
  useEffect(() => {
    ref.current?.querySelector<HTMLElement>('.term-tab[aria-current="page"]')?.scrollIntoView({ block: 'nearest', inline: 'nearest' })
  }, [p.current, p.tabs])
  const which = (s: SessionInfo) => sessionWhich(s, p.tabs)
  // Agents waiting in all tabs' sessions: the way to the agents page says so.
  const waitingAll = p.tabs.reduce((n, s) => {
    const info = p.infos[s.id]
    if (info) return n + info.waiting
    return n + (s.meta?.t === 'herdr' && !s.meta.lost ? s.meta.workspaces.reduce((k, w) => k + w.agents.filter((a) => a.status === 'blocked').length, 0) : 0)
  }, 0)
  return (
    <nav className="term-tabs" aria-label={t('tabs.label')} ref={ref}>
      <button
        type="button"
        className="icon-btn term-tab-agents"
        onClick={() => navigate('/agents')}
        aria-label={waitingAll ? `${t('nav.agents')}，${t('herdr.waiting', { n: waitingAll })}` : t('nav.agents')}
        title={t('nav.agents')}
      >
        <Bot size={17} />
        {waitingAll > 0 && <span className="term-tab-count term-tab-wait">{waitingAll}</span>}
      </button>
      {p.tabs.map((s) => {
        const info = p.infos[s.id]
        const name = s.host_name ? s.host_name + ' · ' + which(s) : t('term.connecting')
        const quiet = !info || info.status === 's-open'
        // Agents: as the tab's page tells (mounted), else as the session list last said.
        const count = (st: string) => (s.meta?.t === 'herdr' ? s.meta.workspaces.reduce((n, w) => n + w.agents.filter((a) => a.status === st).length, 0) : 0)
        const waiting = info ? info.waiting : count('blocked')
        const done = info ? info.done : count('done')
        const agents = waiting ? t('herdr.waiting', { n: waiting }) : done ? t('herdr.done', { n: done }) : ''
        const tip = [s.host_name, s.host_name && which(s), s.title, info?.statusText, agents].filter(Boolean).join(' · ')
        return (
          <button
            key={s.id}
            type="button"
            className="term-tab"
            aria-current={s.id === p.current ? 'page' : undefined}
            title={tip}
            onClick={() => s.id !== p.current && navigate('/t/' + s.id, true)}
          >
            {!quiet && <span className={'term-tab-dot ' + info.status} aria-hidden="true" />}
            <span className="term-tab-name">{name}</span>
            {s.title && <span className="term-tab-title">{s.title}</span>}
            {waiting > 0 && (
              <span className="term-tab-count term-tab-wait" aria-label={t('herdr.waiting', { n: waiting })}>
                <MessageCircleReply size={13} strokeWidth={2.25} aria-hidden="true" />
                {waiting}
              </span>
            )}
            {/* (Finished and not looked at yet, when none waits.) */}
            {waiting === 0 && done > 0 && (
              <span className="term-tab-count term-tab-done" aria-label={t('herdr.done', { n: done })}>
                <Check size={13} strokeWidth={2.5} aria-hidden="true" />
                {done}
              </span>
            )}
          </button>
        )
      })}
      <button type="button" className="icon-btn term-tab-new" onClick={p.onNew} aria-label={t('tabs.new')} aria-keyshortcuts={standalone ? (isMac ? 'Meta+T' : 'Control+Shift+T') : undefined} title={standalone ? `${t('tabs.new')} (${newTabKey})` : t('tabs.new')}>
        <Plus size={17} />
      </button>
    </nav>
  )
}

// "+": a host to open something on; what to open is then asked as from the
// host list (the session picker).
function NewTabDialog(p: { busyHost: number | null; onPick: (h: Host) => void; onClose: () => void }) {
  const [hosts, setHosts] = useState<Host[] | null>(null)
  const [failed, setFailed] = useState(false)
  const [q, setQ] = useState('')
  useEffect(() => {
    api<Host[]>('GET', '/api/hosts')
      .then((l) => setHosts([...l].sort((a, b) => b.last_connected_at - a.last_connected_at || a.name.localeCompare(b.name))))
      .catch(() => setFailed(true))
  }, [])
  const needle = q.trim().toLowerCase()
  const shown = (hosts ?? []).filter((h) => !needle || h.name.toLowerCase().includes(needle) || h.address.toLowerCase().includes(needle))
  return (
    <Dialog title={t('tabs.new')} onClose={p.onClose} className="new-tab">
      <label className="new-tab-search">
        <Search size={16} aria-hidden="true" />
        <input
          type="search"
          value={q}
          onChange={(e) => setQ(e.target.value)}
          placeholder={t('tabs.search')}
          aria-label={t('tabs.search')}
          autoFocus
          onKeyDown={(e) => {
            if (e.key === 'Enter' && shown.length > 0) {
              e.preventDefault()
              p.onPick(shown[0])
            }
          }}
        />
      </label>
      <div className="new-tab-list" role="list">
        {failed ? (
          <p className="hint">{t('tabs.hostsFailed')}</p>
        ) : hosts === null ? (
          <p className="hint">{t('common.loading')}</p>
        ) : hosts.length === 0 ? (
          <p className="hint">{t('tabs.noHosts')}</p>
        ) : shown.length === 0 ? (
          <p className="hint">{t('tabs.noMatch')}</p>
        ) : (
          shown.map((h) => (
            <button key={h.id} type="button" role="listitem" className="new-tab-host" disabled={p.busyHost !== null} aria-busy={p.busyHost === h.id || undefined} onClick={() => p.onPick(h)}>
              <span className="new-tab-name">{h.name}</span>
              <span className="new-tab-addr">{h.username}@{h.address}{h.port !== 22 ? ':' + h.port : ''}</span>
              {p.busyHost === h.id && <span className="new-tab-busy">{t('term.connecting')}</span>}
            </button>
          ))
        )}
      </div>
      <div className="dialog-actions">
        <button
          type="button"
          className="btn btn-ghost"
          onClick={() => {
            p.onClose()
            navigate('/')
          }}
        >
          {t('tabs.manage')}
        </button>
        <button type="button" className="btn" onClick={p.onClose}>{t('common.cancel')}</button>
      </div>
    </Dialog>
  )
}
