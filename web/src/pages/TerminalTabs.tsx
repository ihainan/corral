import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { Bot, Check, MessageCircleReply, Plus, Search } from 'lucide-react'
import { AgentsChat } from './AgentsChat'
import { api, choiceOf, probeBootstrap, type Host, type OpenChoice, type SessionInfo } from '../api'
import { agentKey, sessionWhich } from '../terminal/sessionName'
import { HIDDEN_EVERY, waitingTitle } from '../terminal/agents'
import { t } from '../i18n'
import { local, session as tabStore } from '../storage'
import { navigate } from '../router'
import { takeTabs } from '../notifyopen'
import { Dialog, useToast } from '../components/ui'
import { estimateSize, useConnect } from '../connect'
import type { TabInfo } from './Terminal'
import { TerminalPage } from './lazyPages'

// Tabs where there is room for them: desktops and tablets (an iPad with a
// keyboard switches with Ctrl+Tab too). Phones keep the session switcher.
const TABS_QUERY = '(min-width: 700px)'
export const AGENTS = 'agents' // (TerminalTabs' current for the agents page)
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
// its place, ⌘9 for the last and ⌘⇧[ ⌘⇧] for the previous and next;
// Ctrl+Shift+PageUp and PageDown move the tab in front (as in Chrome). No
// key closes a tab: that ends its session.
// Tabs keep their places. A tab is the session it shows: tmux's or herdr's
// session on its host (opened again after a gateway restart or a lost
// connection, it is a new session of the gateway's, in the same place), a
// plain shell itself. Places are kept on this device; a new tab goes last,
// and the user drags a tab to another place (see useTabDrag).
// A tab gone for longer than a reconnection takes loses its place: opened
// again later, it goes last.
// The tmux and herdr tabs of this window, with the gateway's run they were
// of: when it restarts (an update), its sessions end, but what ran in them
// goes on on the host. A tab shown since the page loaded opens itself again
// (see the terminal page); one never shown is opened again from here, so all
// the tabs come back, in their places (kept by host, kind and name).
const KEPT_KEY = 'corral.tabsKept'
interface KeptTabs {
  instance: string
  tabs: { id: string; host_id: number; open: OpenChoice }[]
}
function readKept(): KeptTabs | null {
  try {
    const k = JSON.parse(tabStore.get(KEPT_KEY) ?? 'null')
    return k && typeof k.instance === 'string' && Array.isArray(k.tabs) ? k : null
  } catch {
    return null
  }
}

const ORDER_KEY = 'corral.tabOrder2' // (corral.tabOrder held when each was last seen: another meaning)
const KEEP_GONE_MS = 10 * 60 * 1000
// The session a tab shows (herdr's default session, named or not, is one).
// Several tabs of one tmux or herdr session are told apart by when they were
// opened, so a new one goes last.
const baseKey = (s: SessionInfo) =>
  s.persistence === 'tmux' || s.persistence === 'herdr'
    ? `${s.host_id}|${s.persistence}|${s.persistence === 'herdr' && (s.mux_session ?? '') === 'default' ? '' : (s.mux_session ?? '')}`
    : 'id|' + s.id

// The places as written last where storage would not keep them (blocked or
// full): they hold for this page's life (else the next list undid a move).
let unstored: string | null = null
function writeOrder(order: [string, number][]) {
  const v = JSON.stringify(order)
  local.set(ORDER_KEY, v)
  unstored = local.get(ORDER_KEY) === v ? null : v
}

// The places, in order, each with when a list first lacked it (0: it was there).
function readOrder(): [string, number][] {
  try {
    const v = JSON.parse(unstored ?? local.get(ORDER_KEY) ?? '[]')
    if (Array.isArray(v)) return v.filter((x) => Array.isArray(x) && typeof x[0] === 'string' && typeof x[1] === 'number')
  } catch {
    /* not ours */
  }
  return []
}

// Each session's place key: the oldest of a session's tabs has its base key,
// the later ones a number after it. (Opened in one second, by their ids: not
// by the list's order, which a move changes.)
const byOpened = (a: SessionInfo, b: SessionInfo) => a.created_at - b.created_at || a.id.localeCompare(b.id)
function placeKeys(list: SessionInfo[]): Map<string, string> {
  const seen = new Map<string, number>()
  const keyOf = new Map<string, string>()
  for (const s of [...list].sort(byOpened)) {
    const base = baseKey(s)
    const n = (seen.get(base) ?? 0) + 1
    seen.set(base, n)
    keyOf.set(s.id, n === 1 ? base : base + '#' + n)
  }
  return keyOf
}

function inPlace(list: SessionInfo[]): SessionInfo[] {
  const now = Date.now()
  const keyOf = placeKeys(list)
  const open = new Set(keyOf.values())
  // Gone longer than a reconnection takes (counted from when a list lacked
  // it, not from the last list: a page in the background asks none): its
  // place is let go, and back later it goes last.
  const order = readOrder()
    .filter(([, gone]) => !(gone && now - gone >= KEEP_GONE_MS))
    .map(([k, gone]): [string, number] => [k, open.has(k) ? 0 : gone || now])
  const known = new Set(order.map(([k]) => k))
  for (const s of [...list].sort(byOpened)) {
    const k = keyOf.get(s.id)!
    if (!known.has(k)) {
      order.push([k, 0])
      known.add(k)
    }
  }
  writeOrder(order)
  const at = new Map(order.map(([k], i) => [k, i]))
  return [...list].sort((a, b) => at.get(keyOf.get(a.id)!)! - at.get(keyOf.get(b.id)!)!)
}

// The user moved the tab of session id to place `to` among the tabs as shown
// (list, in their places as this window last read them: another window may
// have moved some since, and the move is the one the user saw): the open tabs
// take the places they hold, in the order shown with the move; the places
// kept for gone ones stay where they were (back soon, a tab goes back there).
function moveTab(list: SessionInfo[], id: string, to: number) {
  inPlace(list) // (every open tab has a place)
  const keyOf = placeKeys(list)
  const keys = list.map((s) => keyOf.get(s.id)!)
  const from = list.findIndex((s) => s.id === id)
  if (from < 0 || to < 0 || to >= keys.length || from === to) return
  keys.splice(to, 0, keys.splice(from, 1)[0])
  const open = new Set(keys)
  let i = 0
  const order = readOrder().map(([k, gone]): [string, number] => (open.has(k) ? [keys[i++], 0] : [k, gone]))
  writeOrder(order)
}

// The agents waiting for an answer in these sessions, each counted once
// (several tabs may show one herdr session), as herdr's summaries say.
function waitingAgents(list: SessionInfo[]): number {
  const seen = new Set<string>()
  for (const s of list) {
    if (s.meta?.t !== 'herdr' || s.meta.lost) continue
    for (const w of s.meta.workspaces) for (const a of w.agents) if (a.status === 'blocked') seen.add(agentKey(s, a.pane_id))
  }
  return seen.size
}

// current: the session in front, or "agents" (the agents page, a tab of its own).
export function TerminalTabs({ current }: { current: string }) {
  const agents = current === AGENTS
  const [sessions, setSessions] = useState<SessionInfo[] | null>(null)
  const [mounted, setMounted] = useState<string[]>(current === AGENTS ? [] : [current])
  const [agentsSeen, setAgentsSeen] = useState(agents)
  if (agents && !agentsSeen) setAgentsSeen(true)
  const [infos, setInfos] = useState<Record<string, TabInfo>>({})
  const [newOpen, setNewOpen] = useState(false)
  const toast = useToast()

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
    const inst = instanceRef.current // (the gateway's run as known when the list was asked for: see keepTabs)
    api<SessionInfo[]>('GET', '/api/sessions')
      .then((l) => {
        const running = l.filter((x) => x.state === 'running')
        setSessions(inPlace(running))
        setListAt(at)
        void keepTabs(running, inst)
      })
      .catch(() => {}) // (the tabs stay as they were; the next poll tries again)
      .finally(() => {
        const again = loading.current === 'again'
        loading.current = 'no'
        if (again) load()
      })
  }, [])
  // What the tabs are, kept with the gateway's run; tabs gone with a restart
  // opened again (each asked for, shown or not: one shown may not have gone
  // to open itself again yet, and the gateway gives the same tab to both).
  // Ones that need a password are said to, not asked for.
  const reopening = useRef(false)
  const instanceRef = useRef<string | null>(null)
  const keepTabs = async (running: SessionInfo[], inst: string | null) => {
    if (reopening.current) return
    const kept = readKept()
    const missing = kept ? kept.tabs.filter((k) => !running.some((s) => s.id === k.id)) : []
    reopening.current = true
    try {
      // (The gateway's run asked again only when tabs went: a restart, or closed.)
      const b = missing.length || !instanceRef.current ? await probeBootstrap() : { instance: instanceRef.current }
      if (!b?.instance) return
      instanceRef.current = b.instance
      // (A list of the run before, or of one not known: asked for again, so it is not kept as this run's.)
      if (b.instance !== inst) {
        loadRef.current()
        return
      }
      if (kept && missing.length && kept.instance !== b.instance) {
        let failed = 0
        for (const k of missing) {
          try {
            const r = await api<SessionInfo | { choices?: unknown }>('POST', '/api/sessions', { host_id: k.host_id, ...estimateSize(), open: k.open })
            if (!('id' in r)) failed++
          } catch {
            failed++
          }
        }
        if (failed) toast(t('tabs.reopenFailed', { n: failed }), true)
        // (Opened once: what the tabs are now is the next list's.)
        tabStore.set(KEPT_KEY, JSON.stringify({ instance: b.instance, tabs: [] }))
        loadRef.current()
        return
      }
      const tabs = running.flatMap((s) => {
        const open = choiceOf(s)
        return open && (open.kind === 'tmux' || open.kind === 'herdr') ? [{ id: s.id, host_id: s.host_id, open }] : []
      })
      tabStore.set(KEPT_KEY, JSON.stringify({ instance: b.instance, tabs }))
    } finally {
      reopening.current = false
    }
  }
  const loadRef = useRef(load)
  loadRef.current = load
  useEffect(() => {
    load()
    // (Behind another tab, now and then still: the title tells what waits.)
    let last = Date.now()
    const timer = window.setInterval(() => {
      if (document.visibilityState === 'visible' || Date.now() - last >= HIDDEN_EVERY) {
        last = Date.now()
        load()
      }
    }, POLL_MS)
    return () => clearInterval(timer)
  }, [load])
  const waiting = sessions ? waitingAgents(sessions) : 0
  useEffect(() => waitingTitle(waiting), [waiting])
  useEffect(() => () => waitingTitle(0), [])
  // A session just opened shows its tab with its name. (Not when mounted:
  // the effect above has asked already, and a second ask would be queued.)
  const currentSeen = useRef(false)
  useEffect(() => {
    if (!currentSeen.current) return void (currentSeen.current = true)
    load()
  }, [current, load])

  // The current one moves to the end of the mounted list; the oldest is let go.
  const mountedAt = useRef(new Map<string, number>())
  useEffect(() => {
    if (current === AGENTS) return // (no terminal of its own: the others stay as they are)
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
      return old && old.status === i.status && old.statusText === i.statusText && old.waiting === i.waiting && old.done === i.done && old.working === i.working && old.reopening === i.reopening ? all : { ...all, [id]: i }
    })
  }, [])

  // The tabs: the running sessions, and the current one even before the list
  // has it (just opened) or after it ended (its end is shown).
  const tabs = useMemo(() => {
    const list = sessions ?? []
    return current === AGENTS || list.some((s) => s.id === current) ? list : [...list, { id: current, host_id: 0, host_name: '', title: '' } as SessionInfo]
  }, [sessions, current])

  // A tab moved by the user: its place is kept, and the bar shows it there now.
  const sessionsRef = useRef(sessions)
  sessionsRef.current = sessions
  const move = useCallback((id: string, to: number) => {
    const l = sessionsRef.current
    if (!l) return
    const from = l.findIndex((s) => s.id === id)
    if (from < 0 || to < 0 || to >= l.length || from === to) return
    moveTab(l, id, to)
    // (Shown moved whether or not the place could be kept: storage may be blocked.)
    const next = [...l]
    next.splice(to, 0, next.splice(from, 1)[0])
    setSessions(next)
  }, [])

  // (The agents page first, as the bar shows it.)
  const go = useCallback(
    (dir: 1 | -1) => {
      const ids = [AGENTS, ...tabs.map((s) => s.id)]
      const i = ids.indexOf(current)
      if (ids.length < 2 || i < 0) return
      const to = ids[(i + dir + ids.length) % ids.length]
      navigate(to === AGENTS ? '/agents' : '/t/' + to, true)
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
        if (tabs.length >= 1) act = () => go(e.shiftKey ? -1 : 1)
      } else if (standalone && e.ctrlKey && e.shiftKey && !e.metaKey && (e.key === 'PageUp' || e.key === 'PageDown')) {
        // (At either end it goes nowhere, and the program does not get it either.)
        const i = tabs.findIndex((s) => s.id === current && s.host_id !== 0)
        const to = i + (e.key === 'PageUp' ? -1 : 1)
        act = () => i >= 0 && to >= 0 && to < tabs.filter((s) => s.host_id !== 0).length && move(current, to)
      } else if (standalone && isMac && e.metaKey && !e.ctrlKey) {
        const digit = /^Digit([1-9])$/.exec(e.code)
        if (isT && !e.shiftKey) act = () => setNewOpen(true)
        else if (digit && !e.shiftKey) {
          // (As the bar shows them, the agents page first.)
          const ids = [AGENTS, ...tabs.map((s) => s.id)]
          const n = Number(digit[1])
          const to = n === 9 ? ids[ids.length - 1] : ids[n - 1]
          act = () => to && to !== current && navigate(to === AGENTS ? '/agents' : '/t/' + to, true)
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
  }, [go, move, tabs, current])

  // A notification of another session (tapped while this window shows
  // tabs) opens as its tab here, with what its address asks for (?reply=).
  useEffect(() => {
    takeTabs((u) => {
      if (!/^\/t\/[0-9a-f]+$/.test(u.pathname) || document.querySelector('[aria-modal="true"]')) return false
      if (u.searchParams.get('convo') === '1') {
        // (An address from before the agents page: there, at that agent.)
        const to = new URLSearchParams({ s: u.pathname.slice(3) })
        if (u.searchParams.get('pane')) to.set('pane', u.searchParams.get('pane')!)
        navigate('/agents?' + to)
        return true
      }
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

  const bar = <TabBar tabs={tabs} current={current} infos={infos} onNew={() => setNewOpen(true)} onMove={move} />
  // (The tab shown is drawn in the same frame as the one left goes: not a
  // frame later, once the effect above has mounted it — a frame of neither,
  // the page's own background, flashed between them.)
  const drawn = current !== AGENTS && !mounted.includes(current) ? [...mounted, current] : mounted
  return (
    <>
      {drawn.map((id) => (
        <TerminalPage key={id} id={id} active={id === current} tabs={bar} onInfo={onInfo} onEnd={() => endTab(id)} />
      ))}
      {/* (Kept once shown, as the terminals are: what it is sending, and what was sent, stay.) */}
      {agentsSeen && <AgentsChat bar={bar} active={agents} />}
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

// Dragging a tab to another place. A mouse drags once it moves a few
// pixels (a click stays a click); a finger holds still a moment first, as
// moving at once scrolls the bar. The others make room as it passes, the bar
// scrolls by itself at its ends, and Esc (or the browser taking the touch)
// leaves the tabs as they were.
interface Drag {
  id: string
  from: number
  to: number
  dx: number
  w: number // the dragged tab's width and the gap after it: what the others move by
}
const DRAG_MOUSE_PX = 4
const DRAG_HOLD_MS = 300
const DRAG_SLOP_PX = 8 // (a finger that moves more before then is scrolling)
const DRAG_EDGE_PX = 32

function useTabDrag(nav: React.RefObject<HTMLElement | null>, ids: string[], current: string, onMove: (id: string, to: number) => void) {
  const [drag, setDrag] = useState<Drag | null>(null)
  const cur = useRef<{
    id: string
    from: number
    pointer: number
    x0: number // where the pointer went down (client)
    x: number // where it is
    on: boolean
    at?: number // where it went down in the bar's content, once dragging
    tabs?: { left: number; width: number }[] // the draggable tabs, in the bar's content
    gap?: number
    to?: number
    hold?: number
    frame?: number
    dead?: boolean // let go of before the release (see drop)
    stop: () => void
    drop: () => void
  } | null>(null)
  const endedAt = useRef(0)
  const moved = useRef(false) // a drop changed the order (and the bar is not to scroll for it)

  const update = () => {
    const d = cur.current
    const el = nav.current
    if (!d?.on || !el || !d.tabs || d.at === undefined) return
    const box = el.getBoundingClientRect()
    const me = d.tabs[d.from]
    const first = d.tabs[0]
    const last = d.tabs[d.tabs.length - 1]
    // Drawn within the row; placed by where the pointer is: past another
    // tab's middle, it goes beyond that one (whatever the widths: a wide tab
    // held at the start goes before a narrow first one). Within its own place
    // it is past none it was not past before.
    const px = d.x - box.left + el.scrollLeft
    const dx = Math.min(Math.max(px - d.at, first.left - me.left), last.left + last.width - me.left - me.width)
    let to = 0
    d.tabs.forEach((r, j) => j !== d.from && r.left + r.width / 2 < px && to++)
    d.to = to
    setDrag({ id: d.id, from: d.from, to, dx, w: me.width + (d.gap ?? 0) })
  }

  // The bar scrolls while the pointer is near either end of it.
  const scrollAtEnds = () => {
    const d = cur.current
    const el = nav.current
    if (!d?.on || !el) return
    const box = el.getBoundingClientRect()
    const by = d.x < box.left + DRAG_EDGE_PX ? -8 : d.x > box.right - DRAG_EDGE_PX ? 8 : 0
    if (by && el.scrollWidth > el.clientWidth) {
      el.scrollLeft += by
      update()
    }
    d.frame = requestAnimationFrame(scrollAtEnds)
  }

  const begin = () => {
    const d = cur.current
    const el = nav.current
    if (!d || !el) return
    const box = el.getBoundingClientRect()
    const tabs = [...el.querySelectorAll<HTMLElement>('.term-tab[data-place]')].map((t) => {
      const r = t.getBoundingClientRect()
      return { left: r.left - box.left + el.scrollLeft, width: r.width }
    })
    if (tabs.length < 2 || !tabs[d.from]) return d.stop()
    d.on = true
    d.tabs = tabs
    d.gap = parseFloat(getComputedStyle(el).columnGap) || 0
    d.at = d.x0 - box.left + el.scrollLeft
    update()
    d.frame = requestAnimationFrame(scrollAtEnds)
  }

  const onPointerDown = (e: React.PointerEvent, id: string, from: number) => {
    if (e.button !== 0 || cur.current || ids.length < 2) return
    const mouse = e.pointerType === 'mouse'
    const pointer = e.pointerId
    const move = (ev: PointerEvent) => {
      const d = cur.current
      if (!d || d.dead || ev.pointerId !== pointer) return
      d.x = ev.clientX
      if (d.on) update()
      else if (mouse && Math.abs(d.x - d.x0) > DRAG_MOUSE_PX) begin()
      else if (!mouse && Math.abs(d.x - d.x0) > DRAG_SLOP_PX) d.stop()
    }
    const up = (ev: PointerEvent) => {
      const d = cur.current
      if (!d || ev.pointerId !== pointer) return
      if (d.on || d.dead) endedAt.current = performance.now()
      if (d.on && d.to !== undefined && d.to !== d.from) {
        moved.current = true
        onMove(d.id, d.to)
      }
      d.stop()
    }
    const cancel = (ev: PointerEvent) => ev.pointerId === pointer && cur.current?.stop()
    const key = (ev: KeyboardEvent) => {
      if (ev.key !== 'Escape' || !cur.current?.on) return
      ev.preventDefault()
      ev.stopPropagation()
      cur.current.drop()
    }
    // (A finger held still is not asking for the page's menu.)
    const menu = (ev: Event) => ev.preventDefault()
    const stop = () => {
      const d = cur.current
      if (d?.hold) clearTimeout(d.hold)
      if (d?.frame) cancelAnimationFrame(d.frame)
      cur.current = null
      window.removeEventListener('pointermove', move)
      window.removeEventListener('pointerup', up)
      window.removeEventListener('pointercancel', cancel)
      window.removeEventListener('keydown', key, true)
      window.removeEventListener('contextmenu', menu, true)
      setDrag(null)
    }
    // Let go of with no move (Esc, the tabs changed, another one in front):
    // the rest of the press is still the drag's, and its release no click.
    const drop = () => {
      const d = cur.current
      if (d?.dead) return // (let go of already: the release is still awaited)
      if (!d?.on) return stop()
      d.on = false
      d.dead = true
      if (d.frame) cancelAnimationFrame(d.frame)
      setDrag(null)
    }
    cur.current = { id, from, pointer, x0: e.clientX, x: e.clientX, on: false, stop, drop }
    window.addEventListener('pointermove', move)
    window.addEventListener('pointerup', up)
    window.addEventListener('pointercancel', cancel)
    window.addEventListener('keydown', key, true)
    if (!mouse) {
      window.addEventListener('contextmenu', menu, true)
      cur.current.hold = window.setTimeout(begin, DRAG_HOLD_MS)
    }
  }

  // A finger holding a tab does not scroll the bar. (The bar's own listener,
  // there from the start: whether a touch may be held back from scrolling is
  // settled when it begins, so one added once the drag starts comes too late.)
  useEffect(() => {
    const el = nav.current
    if (!el) return
    const touchMove = (ev: TouchEvent) => {
      if (cur.current?.on && ev.cancelable) ev.preventDefault()
    }
    el.addEventListener('touchmove', touchMove, { passive: false })
    return () => el.removeEventListener('touchmove', touchMove)
  }, [nav])

  // The tabs changed under a drag (one opened or ended, or moved by another
  // window), or another tab came to the front (this bar is hidden then): the
  // drag is let go of.
  const idsKey = ids.join()
  useEffect(() => () => cur.current?.drop(), [idsKey, current])
  useEffect(() => () => cur.current?.stop(), [])

  // Where tab j of the draggable ones is drawn meanwhile.
  const offset = (j: number): number => {
    if (!drag) return 0
    if (j === drag.from) return drag.dx
    if (drag.from < drag.to && j > drag.from && j <= drag.to) return -drag.w
    if (drag.to < drag.from && j >= drag.to && j < drag.from) return drag.w
    return 0
  }
  // A click that ends a drag is not one.
  const dragged = () => performance.now() - endedAt.current < 400
  return { drag, onPointerDown, offset, dragged, moved }
}

function TabBar(p: { tabs: SessionInfo[]; current: string; infos: Record<string, TabInfo>; onNew: () => void; onMove: (id: string, to: number) => void }) {
  const ref = useRef<HTMLElement>(null)
  // (The tab of a session just opened, not in the list yet, stays last.)
  const movable = p.tabs.filter((s) => s.host_id !== 0).map((s) => s.id)
  const { drag, onPointerDown, offset, dragged, moved } = useTabDrag(ref, movable, p.current, p.onMove)
  // The current tab is scrolled into view when it changes, or the tabs do
  // (moved by a key too; not by a drag: the tab dragged is where it was let go).
  const order = p.tabs.map((s) => s.id).join()
  useEffect(() => {
    if (moved.current) {
      moved.current = false
      return
    }
    ref.current?.querySelector<HTMLElement>('[aria-current="page"]')?.scrollIntoView({ block: 'nearest', inline: 'nearest' })
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [p.current, order])
  const which = (s: SessionInfo) => sessionWhich(s, p.tabs)
  // Agents waiting in all tabs' sessions: the way to the agents page says so.
  // (Each agent once: two tabs of one herdr session list its agents twice.)
  const waitingAll = waitingAgents(p.tabs)
  return (
    <nav className={'term-tabs' + (drag ? ' dragging' : '')} aria-label={t('tabs.label')} ref={ref}>
      <button
        type="button"
        className="term-tab term-tab-agents"
        aria-current={p.current === AGENTS ? 'page' : undefined}
        onClick={() => p.current !== AGENTS && navigate('/agents')}
        title={t('nav.agents')}
      >
        <Bot size={15} aria-hidden="true" />
        <span className="term-tab-name">{t('nav.agents')}</span>
        {waitingAll > 0 && (
          <span className="term-tab-count term-tab-wait" aria-label={t('herdr.waiting', { n: waitingAll })}>
            <MessageCircleReply size={13} strokeWidth={2.25} aria-hidden="true" />
            {waitingAll}
          </span>
        )}
      </button>
      {p.tabs.map((s) => {
        const info = p.infos[s.id]
        const name = s.host_name ? s.host_name + ' · ' + which(s) : t('term.connecting')
        const quiet = !info || info.status === 's-open'
        // Agents: as the tab's page tells (mounted), else as the session list last said.
        const count = (st: string) => (s.meta?.t === 'herdr' && !s.meta.lost ? s.meta.workspaces.reduce((n, w) => n + w.agents.filter((a) => a.status === st).length, 0) : 0)
        const waiting = info ? info.waiting : count('blocked')
        const done = info ? info.done : count('done')
        const working = info ? info.working : count('working')
        const agents = [waiting && t('herdr.waiting', { n: waiting }), working && t('herdr.working', { n: working }), !waiting && done && t('herdr.done', { n: done })].filter(Boolean).join(' · ')
        const tip = [s.host_name, s.host_name && which(s), s.title, info?.statusText, agents].filter(Boolean).join(' · ')
        const j = movable.indexOf(s.id)
        const dx = j < 0 ? 0 : offset(j)
        return (
          <button
            key={s.id}
            type="button"
            className={'term-tab' + (drag?.id === s.id ? ' held' : '')}
            data-place={j < 0 ? undefined : j}
            style={dx ? { transform: `translateX(${dx}px)` } : undefined}
            aria-current={s.id === p.current ? 'page' : undefined}
            title={tip}
            onPointerDown={j < 0 ? undefined : (e) => onPointerDown(e, s.id, j)}
            onClick={() => !dragged() && s.id !== p.current && navigate('/t/' + s.id, true)}
          >
            {/* (A connection's trouble before agents at work: it is the one to see to.) */}
            {!quiet ? <span className={'term-tab-dot ' + info.status} aria-hidden="true" /> : working > 0 && <span className="term-tab-working" role="img" aria-label={t('herdr.working', { n: working })} />}
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
