import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { Plus, Search } from 'lucide-react'
import { api, type Host, type SessionInfo } from '../api'
import { t } from '../i18n'
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

// The user's running sessions as tabs, like a terminal app's: a tab once
// opened stays connected in the back (up to MAX_MOUNTED), so switching is
// instant. Ctrl+Tab and Ctrl+Shift+Tab go to the next and previous tab; a
// browser keeps them for its own tabs, an installed app passes them on.
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
        setSessions(l.filter((x) => x.state === 'running').sort((a, b) => a.created_at - b.created_at))
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
      const next = m.filter((x) => x === current || sessions.some((s) => s.id === x) || (mountedAt.current.get(x) ?? 0) >= listAt)
      for (const id of m) if (!next.includes(id)) mountedAt.current.delete(id)
      return next.length === m.length ? m : next
    })
  }, [sessions, listAt, current])

  const onInfo = useCallback((id: string, i: TabInfo) => {
    setInfos((all) => {
      const old = all[id]
      return old && old.status === i.status && old.statusText === i.statusText && old.waiting === i.waiting ? all : { ...all, [id]: i }
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
      if (e.key !== 'Tab' || !e.ctrlKey || e.altKey || e.metaKey || tabs.length < 2 || document.querySelector('[aria-modal="true"]')) return
      // Before the terminal's own listeners: the program never sees it.
      e.preventDefault()
      e.stopPropagation()
      go(e.shiftKey ? -1 : 1)
    }
    window.addEventListener('keydown', onKey, true)
    return () => window.removeEventListener('keydown', onKey, true)
  }, [go, tabs.length])

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

  const bar = <TabBar tabs={tabs} current={current} infos={infos} onNew={() => setNewOpen(true)} />
  return (
    <>
      {mounted.map((id) => (
        <TerminalPage key={id} id={id} active={id === current} tabs={bar} onInfo={onInfo} />
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
  const nth = (s: SessionInfo) => p.tabs.filter((x) => x.host_id === s.host_id).indexOf(s) + 1
  const several = (s: SessionInfo) => p.tabs.filter((x) => x.host_id === s.host_id).length > 1
  return (
    <nav className="term-tabs" aria-label={t('tabs.label')} ref={ref}>
      {p.tabs.map((s) => {
        const info = p.infos[s.id]
        const name = s.host_name ? s.host_name + (several(s) ? ' · ' + nth(s) : '') : t('term.connecting')
        const quiet = !info || info.status === 's-open'
        const tip = [s.host_name, s.host_name && t('hosts.sessionN', { n: nth(s) }), s.title, info?.statusText].filter(Boolean).join(' · ')
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
            {info && info.waiting > 0 && <span className="term-tab-wait" aria-label={t('herdr.waiting', { n: info.waiting })}>{info.waiting}</span>}
          </button>
        )
      })}
      <button type="button" className="icon-btn term-tab-new" onClick={p.onNew} aria-label={t('tabs.new')} title={t('tabs.new')}>
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
