import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { modalShown } from '../shown'
import { ArrowLeftToLine, ArrowRightToLine, Bot, ChevronLeft, ExternalLink, Link2, Plus, Server, SquareTerminal, TextCursorInput, X } from 'lucide-react'
import { AgentsChat } from './AgentsChat'
import { api, authGen, choiceOf, probeBootstrap, type OpenChoice, type SessionInfo } from '../api'
import { sessionWhich } from '../terminal/sessionName'
import { renameOrderEntries, SESSION_RENAMED_EVENT, type SessionRenamedPayload } from '../terminal/sessionRenamePolicy'
import { RenameSessionDialog } from '../terminal/RenameSessionDialog'
import { HIDDEN_EVERY, markRead, noteSessions, readAt, waitingAgents, waitingTitle } from '../terminal/agents'
import { t } from '../i18n'
import { local } from '../storage'
import { navigate } from '../router'
import { agentsAddress, takeTabs } from '../notifyopen'
import { Confirm, copyText, isCoarse, MenuLayer, useToast, type MenuItem } from '../components/ui'
import { estimateSize, useConnect } from '../connect'
import type { TabInfo } from './Terminal'
import { TerminalPage } from './lazyPages'
import { NewTabDialog } from '../terminal/NewSessionDialog'
import { showFront, useTabDrag, useTabSlide } from '../terminal/tabDrag'
import { chromeVars, corralDark, corralLight } from '../terminal/themes'
import { isDark, usePrefs } from '../prefs'
import { Rail, StatusBar, useWorkbench } from '../workbench'

// Tabs where there is room for them: desktops and tablets (an iPad with a
// keyboard switches with Ctrl+Tab too). Phones keep the session switcher.
const TABS_QUERY = '(min-width: 700px)'
export const AGENTS = 'agents' // (TerminalTabs' current for the agents page)
// (TerminalTabs' current for the terminals' page itself, /t: the terminal last in
// front there, else the first; none running, a page that says so.)
export const TERMINALS = 'terminals'
// The terminal last in front, for /t (the rail's way to the terminals: 11-24,
// the terminals a page of their own, apart from the agents').
// (By its place key, not its id: the same terminal after a gateway restart.)
let lastTerminal: string | null = null
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
// Kept in the browser, not the tab: a phone's page put away while the
// gateway restarted is often thrown out, and came back with no word of what
// was open (10-31). Of one account (another signed in here opens none of
// it), and only fresh: a device away for long would open again what was
// closed elsewhere since (KEPT_FRESH: a deploy's restart is minutes; hours
// away, what it saw last is not what was open when it went).
const KEPT_KEY = 'corral.tabsKept2' // (corral.tabsKept: the tab's own, of before)
const KEPT_FRESH = 3 * 60 * 60 * 1000
interface KeptTabs {
  instance: string
  user: number
  at: number // when it was written (ms)
  tabs: { id: string; host_id: number; open: OpenChoice }[]
}
function readKept(user: number | undefined): KeptTabs | null {
  try {
    const k = JSON.parse(local.get(KEPT_KEY) ?? 'null')
    return k && typeof k.instance === 'string' && Array.isArray(k.tabs) && typeof k.at === 'number' && typeof k.user === 'number' && k.user === user ? k : null
  } catch {
    return null
  }
}
const writeKept = (k: KeptTabs) => local.set(KEPT_KEY, JSON.stringify(k))
// (A kept tab open now: the same tab, or one of the same session, as another
// device opened it again: not opened once more.)
const keptOpen = (k: KeptTabs['tabs'][number], running: SessionInfo[]) =>
  running.some((s) => s.id === k.id || (s.host_id === k.host_id && choiceOf(s)?.kind === k.open.kind && sameMuxName(k.open.kind, choiceOf(s)?.name ?? '', k.open.name ?? '')))
const sameMuxName = (kind: string, a: string, b: string) => a === b || (kind === 'herdr' && (a === 'default' ? '' : a) === (b === 'default' ? '' : b))

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

// The agents' page and the terminals' (11-24: two pages, each its own row of
// tabs — the terminals no longer tabs of the agents' page, nor it one of
// theirs), kept in one frame: going from one to the other leaves the other's
// terminals connected and its conversation as it was, as the tabs did.
// current: the session in front, "agents" (the agents page) or "terminals" (/t).
export function TerminalTabs({ current }: { current: string }) {
  const agents = current === AGENTS
  const termId = agents || current === TERMINALS ? null : current // (the terminal in front, if one is)
  const [sessions, setSessions] = useState<SessionInfo[] | null>(null)
  const [mounted, setMounted] = useState<string[]>(termId ? [termId] : [])
  const [agentsSeen, setAgentsSeen] = useState(agents)
  if (agents && !agentsSeen) setAgentsSeen(true)
  const [infos, setInfos] = useState<Record<string, TabInfo>>({})
  const [newOpen, setNewOpen] = useState(false)
  const toast = useToast()

  // A list counts from when it was asked for. One request at a time: asked
  // again meanwhile, it is asked once more when the first one is done.
  const [listAt, setListAt] = useState(0)
  // Sessions this page ended, and when: a list asked for before that still
  // has them (one in flight as the tab closed) does not bring them back.
  const endedAt = useRef(new Map<string, number>())
  const loading = useRef<'no' | 'yes' | 'again'>('no')
  const load = useCallback(() => {
    if (loading.current !== 'no') {
      loading.current = 'again'
      return
    }
    loading.current = 'yes'
    const at = Date.now()
    const inst = instanceRef.current // (the gateway's run as known when the list was asked for: see keepTabs)
    const signedIn = authGen() // (and whose: a list of another sign-in is kept as no one's)
    let readAtGw = 0
    api<SessionInfo[]>('GET', '/api/sessions', undefined, { onResponse: (r) => (readAtGw = readAt(r)) })
      .then((l) => {
        // (The rail's and the status bar's counts, from the same list — not one of a sign-in gone since.)
        if (authGen() === signedIn) noteSessions(l, readAtGw)
        const running = l.filter((x) => x.state === 'running' && !((endedAt.current.get(x.id) ?? -1) >= at))
        const next = inPlace(running)
        markRead(next, readAtGw)
        setSessions(next)
        setListAt(at)
        void keepTabs(running, inst, signedIn)
      })
      // (The tabs stay as they were; the next poll tries again. The counts are said to be of before.)
      .catch(() => authGen() === signedIn && noteSessions(null))
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
  const userRef = useRef<number | undefined>(undefined) // (whose tabs: as the gateway said, with its run)
  const userGen = useRef(-1) // (the sign-in userRef was told in: another since, it is asked again)
  const keepTabs = async (running: SessionInfo[], inst: string | null, signedIn: number) => {
    if (reopening.current || authGen() !== signedIn) return
    if (userGen.current !== signedIn) {
      userRef.current = undefined
      userGen.current = signedIn
    }
    let kept = readKept(userRef.current)
    reopening.current = true
    try {
      // (The gateway's run asked again only when tabs went, as their ids say:
      // a restart, or closed. Not by name: another device opening them again
      // after a restart gives new ids, of a run this page has not seen. review488)
      const gone = !!kept && kept.tabs.some((k) => !running.some((s) => s.id === k.id))
      const b = gone || !instanceRef.current || userRef.current === undefined ? await probeBootstrap() : { instance: instanceRef.current, user: { id: userRef.current } }
      if (!b?.instance || !b.user || authGen() !== signedIn) return
      instanceRef.current = b.instance
      if (userRef.current !== b.user.id) {
        userRef.current = b.user.id
        kept = readKept(b.user.id)
      }
      // (Which to open again: by host, kind and name, as another device or
      // tab may have opened them again already.)
      const missing = kept ? kept.tabs.filter((k) => !keptOpen(k, running)) : []
      const user = b.user.id
      // (A list of the run before, or of one not known: asked for again, so it is not kept as this run's.)
      if (b.instance !== inst) {
        loadRef.current()
        return
      }
      if (kept && missing.length && kept.instance !== b.instance && Date.now() - kept.at < KEPT_FRESH) {
        let failed = 0
        for (const k of missing) {
          if (authGen() !== signedIn) return
          try {
            const r = await api<SessionInfo | { choices?: unknown }>('POST', '/api/sessions', { host_id: k.host_id, ...estimateSize(), open: k.open })
            if (!('id' in r)) failed++
          } catch {
            failed++
          }
        }
        if (failed) toast(t('tabs.reopenFailed', { n: failed }), true)
        // (Opened once: what the tabs are now is the next list's.)
        if (authGen() !== signedIn) return
        writeKept({ instance: b.instance, user, at: Date.now(), tabs: [] })
        loadRef.current()
        return
      }
      const tabs = running.flatMap((s) => {
        const open = choiceOf(s)
        return open && (open.kind === 'tmux' || open.kind === 'herdr') ? [{ id: s.id, host_id: s.host_id, open }] : []
      })
      if (authGen() !== signedIn) return
      writeKept({ instance: b.instance, user, at: Date.now(), tabs })
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
  const waiting = sessions ? waitingAgents(sessions) : null // (null: not read yet)
  useEffect(() => waitingTitle(waiting, sessions), [waiting, sessions]) // (again at each reading: see AgentsChat)
  useEffect(() => () => waitingTitle(null), [])
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
    if (!termId) return // (no terminal of its own: the others stay as they are)
    if (!mountedAt.current.has(termId)) mountedAt.current.set(termId, Date.now())
    setMounted((m) => {
      const next = [...m.filter((x) => x !== termId), termId]
      const kept = next.length > MAX_MOUNTED ? next.slice(next.length - MAX_MOUNTED) : next
      for (const id of next) if (!kept.includes(id)) mountedAt.current.delete(id)
      return kept
    })
  }, [termId])
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
    return !termId || list.some((s) => s.id === termId) ? list : [...list, { id: termId, host_id: 0, host_name: '', title: '' } as SessionInfo]
  }, [sessions, termId])

  // The terminals' page itself (/t): the terminal last in front there, else
  // the first; none running, it stays, saying so.
  useEffect(() => {
    if (!termId || !sessions) return
    const k = placeKeys(sessions).get(termId)
    if (k) lastTerminal = k // (only once the list has it: not a tab still reopening)
  }, [termId, sessions])
  useEffect(() => {
    if (current !== TERMINALS || !sessions?.length) return
    const keyOf = placeKeys(sessions)
    const to = sessions.find((s) => keyOf.get(s.id) === lastTerminal) ?? sessions[0]
    navigate('/t/' + to.id, true)
  }, [current, sessions])

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

  // (Within the page in front: the terminals' tabs go round among themselves.)
  const go = useCallback(
    (dir: 1 | -1) => {
      const ids = tabs.map((s) => s.id)
      const i = termId ? ids.indexOf(termId) : -1
      if (ids.length < 2 || i < 0) return
      navigate('/t/' + ids[(i + dir + ids.length) % ids.length], true)
    },
    [tabs, termId],
  )
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      // (Not under a dialog: it keeps the keys and the focus.)
      // (Nor while an input method composes: the key is its.)
      if (e.altKey || e.isComposing || e.keyCode === 229 || modalShown()) return
      // T and the brackets by the character the layout gives (Dvorak's T is
      // where QWERTY has K), by the key's place only where it gives none
      // (Cyrillic). The digits by place: a French keyboard's ⌘1 is ⌘&, as in
      // the Mac's own apps.
      const plain = /^[\x21-\x7e]$/.test(e.key)
      const isT = e.key.toLowerCase() === 't' || (!plain && e.code === 'KeyT')
      const bracket = e.key === '{' || (!plain && e.code === 'BracketLeft') ? -1 : e.key === '}' || (!plain && e.code === 'BracketRight') ? 1 : 0
      let act: (() => void) | null = null
      // (In an installed app only, as the rest: in a browser's tab the
      // browser has Ctrl+Tab; where it did not, the program would lose it.)
      // (The terminals' keys on their page; the agents' page has one tab yet, and keeps them as a page does.)
      const terms = current !== AGENTS
      if (standalone && terms && e.key === 'Tab' && e.ctrlKey && !e.metaKey) {
        if (termId) act = () => go(e.shiftKey ? -1 : 1) // (one tab only: still not the program's)
      } else if (standalone && terms && e.ctrlKey && e.shiftKey && !e.metaKey && (e.key === 'PageUp' || e.key === 'PageDown')) {
        // (At either end it goes nowhere, and the program does not get it either.)
        const i = tabs.findIndex((s) => s.id === termId && s.host_id !== 0)
        const to = i + (e.key === 'PageUp' ? -1 : 1)
        act = () => termId && i >= 0 && to >= 0 && to < tabs.filter((s) => s.host_id !== 0).length && move(termId, to)
      } else if (standalone && terms && isMac && e.metaKey && !e.ctrlKey) {
        const digit = /^Digit([1-9])$/.exec(e.code)
        if (isT && !e.shiftKey) act = () => setNewOpen(true)
        else if (digit && !e.shiftKey) {
          // (As the bar shows them.)
          const ids = tabs.map((s) => s.id)
          const n = Number(digit[1])
          const to = n === 9 ? ids[ids.length - 1] : ids[n - 1]
          act = () => to && to !== termId && navigate('/t/' + to, true)
        } else if (bracket && e.shiftKey) act = () => go(bracket as 1 | -1)
      } else if (standalone && terms && !isMac && e.ctrlKey && e.shiftKey && !e.metaKey && isT) act = () => setNewOpen(true)
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
  }, [go, move, tabs, current, termId])

  // A notification of another session (tapped while this window shows
  // tabs) opens as its tab here, with what its address asks for (?reply=).
  useEffect(() => {
    takeTabs((u) => {
      if (!(/^\/t\/[0-9a-f]+$/.test(u.pathname) || agentsAddress(u)) || modalShown()) return false
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
  // before) comes to the front, or the terminals' page, saying none runs,
  // when it was the last (still in this frame: not the hosts' page).
  // (Read when it happens: the confirmation holds an older copy of this.)
  const tabsRef = useRef(tabs)
  tabsRef.current = tabs
  const endTab = (id: string) => {
    const tabs = tabsRef.current
    const i = tabs.findIndex((s) => s.id === id)
    const rest = tabs.filter((s) => s.id !== id && s.host_id !== 0)
    const next = rest[Math.min(Math.max(i, 0), rest.length - 1)]
    endedAt.current.set(id, Date.now())
    setSessions((l) => l && l.filter((s) => s.id !== id))
    navigate(next ? '/t/' + next.id : '/t', true)
    load()
  }

  // Closing a tab ends its session (no key does: see above): asked first, as the terminal's own "end session" is.
  const [closing, setClosing] = useState<SessionInfo | null>(null)
  // (Several at once, from a tab's menu: the others, or those to its right.)
  const [closingMany, setClosingMany] = useState<{ keep: SessionInfo; list: SessionInfo[] } | null>(null)
  const [renaming, setRenaming] = useState<SessionInfo | null>(null)

  // Keep tab order, lastTerminal, and displayed names in sync immediately when
  // any terminal session is renamed (from the tab menu or the terminal menu).
  useEffect(() => {
    const onRenamed = (e: Event) => {
      const d = (e as CustomEvent<SessionRenamedPayload>).detail
      if (!d) return
      if (d.persistence === 'tmux' && d.oldMuxSession && d.newMuxSession && d.oldMuxSession !== d.newMuxSession) {
        const res = renameOrderEntries(readOrder(), d.host_id, 'tmux', d.oldMuxSession, d.newMuxSession)
        if (res.changed) writeOrder(res.order)
        const oldPrefix = `${d.host_id}|tmux|${d.oldMuxSession}`
        const newPrefix = `${d.host_id}|tmux|${d.newMuxSession}`
        if (lastTerminal === oldPrefix) lastTerminal = newPrefix
        else if (lastTerminal?.startsWith(oldPrefix + '#')) lastTerminal = newPrefix + lastTerminal.slice(oldPrefix.length)
      }
      setSessions((list) =>
        list
          ? list.map((s) => {
              if (s.id === d.id) return { ...s, mux_session: d.newMuxSession, display_name: d.newDisplayName || undefined }
              if (d.persistence === 'tmux' && s.host_id === d.host_id && s.persistence === 'tmux' && s.mux_session === d.oldMuxSession) {
                return { ...s, mux_session: d.newMuxSession }
              }
              if (
                d.persistence === 'herdr' &&
                s.host_id === d.host_id &&
                s.persistence === 'herdr' &&
                sameMuxName('herdr', s.mux_session ?? '', d.oldMuxSession)
              ) {
                return { ...s, display_name: d.newDisplayName || undefined }
              }
              return s
            })
          : list,
      )
      loadRef.current()
    }
    window.addEventListener(SESSION_RENAMED_EVENT, onRenamed)
    return () => window.removeEventListener(SESSION_RENAMED_EVENT, onRenamed)
  }, [])

  // (The agents' page draws its own tabs, its conversations': from here only the way to the terminals' page.)
  const bar = agents ? <PageSwitch agentsPage waiting={waitingAgents(tabs)} /> : <TabBar tabs={tabs} current={current} infos={infos} onNew={() => setNewOpen(true)} onMove={move} onRename={setRenaming} onClose={setClosing} onCloseMany={(keep, list) => setClosingMany({ keep, list })} />
  // (The tab shown is drawn in the same frame as the one left goes: not a
  // frame later, once the effect above has mounted it — a frame of neither,
  // the page's own background, flashed between them.)
  const drawn = termId && !mounted.includes(termId) ? [...mounted, termId] : mounted
  return (
    <>
      {drawn.map((id) => (
        <TerminalPage key={id} id={id} active={id === current} tabs={bar} onInfo={onInfo} onEnd={() => endTab(id)} />
      ))}
      {current === TERMINALS && <TerminalsHome bar={bar} none={sessions?.length === 0} onNew={() => setNewOpen(true)} />}
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
      {closingMany && (
        <Confirm
          title={t('tabs.closeMany', { n: closingMany.list.length })}
          body={t('tabs.closeManyConfirm', { n: closingMany.list.length, names: closingMany.list.map((x) => (x.host_name ?? '') + ' · ' + sessionWhich(x, tabs)).join('、') })}
          confirm={t('tabs.closeMany', { n: closingMany.list.length })}
          danger
          onClose={() => setClosingMany(null)}
          onConfirm={async () => {
            const { keep, list } = closingMany
            // (Each ended on its own: one that fails does not keep the others open.)
            await Promise.allSettled(list.map((x) => api('DELETE', '/api/sessions/' + x.id)))
            const gone = new Set(list.map((x) => x.id))
            for (const id of gone) endedAt.current.set(id, Date.now())
            setSessions((l) => l && l.filter((x) => !gone.has(x.id)))
            if (gone.has(current)) navigate('/t/' + keep.id, true) // (the one shown closed: the tab the menu was of)
            load()
          }}
        />
      )}
      {renaming && <RenameSessionDialog session={renaming} onClose={() => setRenaming(null)} />}
      {closing && (
        <Confirm
          title={t('term.closeSession')}
          body={t('tabs.closeConfirm', { name: (closing.host_name ?? '') + ' · ' + sessionWhich(closing, tabs) })}
          confirm={t('term.closeSession')}
          danger
          onClose={() => setClosing(null)}
          onConfirm={async () => {
            await api('DELETE', '/api/sessions/' + closing.id)
            endTab(closing.id)
          }}
        />
      )}
    </>
  )
}

function TabBar(p: { tabs: SessionInfo[]; current: string; infos: Record<string, TabInfo>; onNew: () => void; onMove: (id: string, to: number) => void; onRename: (s: SessionInfo) => void; onClose: (s: SessionInfo) => void; onCloseMany: (keep: SessionInfo, list: SessionInfo[]) => void }) {
  const toast = useToast()
  // A tab's own menu, at a right click (or the menu key) — as a browser's
  // tabs have one (11-15): open it in a window of its own, copy its link, a
  // new session, rename, move it to either end, close it or the others. Closing
  // ends sessions: asked first, as the close is.
  const [menu, setMenu] = useState<{ id: string; anchor: DOMRect } | null>(null)
  const menuItems = (s: SessionInfo): MenuItem[] => {
    const open = p.tabs.filter((x) => x.host_id !== 0 && x.host_name)
    const at = open.findIndex((x) => x.id === s.id)
    const others = open.filter((x) => x.id !== s.id)
    const right = at < 0 ? [] : open.slice(at + 1)
    const url = location.origin + '/t/' + s.id
    return [
      { label: t('tabs.menu.rename'), icon: <TextCursorInput size={16} />, onSelect: () => p.onRename(s) },
      { label: t('tabs.menu.window'), icon: <ExternalLink size={16} />, separator: true, onSelect: () => window.open(url, '_blank', 'noopener') },
      { label: t('tabs.menu.copyLink'), icon: <Link2 size={16} />, onSelect: () => void copyText(url).then((ok) => toast(ok ? t('tabs.menu.copied') : url)) },
      { label: t('tabs.new'), icon: <Plus size={16} />, onSelect: p.onNew },
      { label: t('tabs.menu.first'), icon: <ArrowLeftToLine size={16} />, separator: true, disabled: at <= 0, onSelect: () => p.onMove(s.id, 0) },
      { label: t('tabs.menu.last'), icon: <ArrowRightToLine size={16} />, disabled: at < 0 || at === open.length - 1, onSelect: () => p.onMove(s.id, open.length - 1) },
      { label: t('tabs.close'), icon: <X size={16} />, separator: true, danger: true, onSelect: () => p.onClose(s) },
      { label: t('tabs.menu.closeOthers', { n: others.length }), icon: <span className="menu-icon-space" />, danger: true, disabled: !others.length, onSelect: () => p.onCloseMany(s, others) },
      { label: t('tabs.menu.closeRight', { n: right.length }), icon: <span className="menu-icon-space" />, danger: true, disabled: !right.length, onSelect: () => p.onCloseMany(s, right) },
    ]
  }
  const menuTab = menu && p.tabs.find((x) => x.id === menu.id)
  const ref = useRef<HTMLElement>(null)
  const shown = p.tabs
  // (The tab of a session just opened, not in the list yet, stays last.)
  const movable = shown.filter((s) => s.host_id !== 0).map((s) => s.id)
  const { drag, onPointerDown, offset, dragged, moved, dropped } = useTabDrag(ref, movable, p.current, p.onMove)
  // The current tab is scrolled into view when it changes, or the tabs do
  // (moved by a key too; not by a drag: the tab dragged is where it was let go).
  const order = shown.map((s) => s.id).join()
  useTabSlide(ref, order, dropped, !!drag)
  useEffect(() => {
    if (moved.current) {
      moved.current = false
      return
    }
    showFront(ref.current)
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [p.current, order])
  const which = (s: SessionInfo) => sessionWhich(s, p.tabs)
  // Tabs beyond either end of the bar: that end fades out (ux1101 T27).
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
  // Agents waiting in all tabs' sessions: the way to the agents page says so.
  // (Each agent once: two tabs of one herdr session list its agents twice.)
  const waitingAll = waitingAgents(p.tabs)
  return (
    <>
    <PageSwitch waiting={waitingAll} />
    <nav className={'term-tabs' + (drag ? ' dragging' : '') + fade} aria-label={t('tabs.label')} ref={ref}>
      {shown.map((s) => {
        const info = p.infos[s.id]
        // (Its host kept as long as it fits: what follows gives way first — ux1101 T27.)
        const name = s.host_name ? (
          <>
            <span className="term-tab-host">{s.host_name}</span>
            <span className="term-tab-which">{which(s)}</span>
          </>
        ) : (
          t('term.connecting')
        )
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
          // (A tab and its close, side by side: the close is not inside the
          // button that goes to it — 11-14, closing one asked a menu before.)
          <div
            key={s.id}
            data-tab={s.id}
            className={'term-tab has-x' + (drag?.id === s.id ? ' held' : '')}
            data-place={j < 0 ? undefined : j}
            style={dx ? { transform: `translateX(${dx}px)` } : undefined}
            aria-current={s.id === p.current ? 'page' : undefined}
            title={tip}
            onPointerDown={j < 0 ? undefined : (e) => onPointerDown(e, s.id, j)}
            // (The middle button closes it, as a browser's tab — asked first, as the close is; mousedown prevents autoscroll.)
            onMouseDown={(e) => e.button === 1 && s.host_name && e.preventDefault()}
            onAuxClick={(e) => e.button === 1 && s.host_name && (e.preventDefault(), p.onClose(s))}
            onContextMenu={
              s.host_name
                ? (e) => {
                    e.preventDefault()
                    // (From the keyboard's menu key: at the tab, not at 0,0.)
                    const r = e.clientX || e.clientY ? new DOMRect(e.clientX, e.clientY, 0, 0) : e.currentTarget.getBoundingClientRect()
                    setMenu({ id: s.id, anchor: r })
                  }
                : undefined
            }
          >
          <button
            type="button"
            className="term-tab-main"
            aria-current={s.id === p.current ? 'page' : undefined}
            onClick={() => !dragged() && s.id !== p.current && navigate('/t/' + s.id, true)}
          >
            {/* (A connection's trouble before agents at work: it is the one to see to.) */}
            {!quiet && <span className={'term-tab-dot ' + info.status} aria-hidden="true" />}
            <span className="term-tab-name">{name}</span>
            {/* (The terminal's own title — a path, a prompt — is in the tab's tooltip and the
                switcher, not on the tab: "host · which" is what tells tabs apart, 11-05.) */}
            {waiting > 0 && (
              <span className="term-tab-count term-tab-wait" aria-label={t('herdr.waiting', { n: waiting })}>
                <span className="term-tab-sdot" aria-hidden="true" />
                {waiting}
              </span>
            )}
            {/* Agents at work: a dot and a count as the others are, not a turning
                ring that reads as the tab still loading (ux1101 T28). One mark
                for every state, its colour telling which (redesign 11-23). */}
            {working > 0 && (
              <span className="term-tab-count term-tab-busy" role="img" aria-label={t('herdr.working', { n: working })}>
                <span className="term-tab-sdot" aria-hidden="true" />
                {working}
              </span>
            )}
            {/* (Finished and not looked at yet, when none waits.) */}
            {waiting === 0 && done > 0 && (
              <span className="term-tab-count term-tab-done" aria-label={t('herdr.done', { n: done })}>
                <span className="term-tab-sdot" aria-hidden="true" />
                {done}
              </span>
            )}
          </button>
          {s.host_name && (
            <button
              type="button"
              className="term-tab-x"
              aria-label={t('tabs.closeTab', { name: s.host_name + ' · ' + which(s) })}
              title={t('tabs.close')}
              onPointerDown={(e) => e.stopPropagation()}
              onClick={() => p.onClose(s)}
            >
              <X size={14} aria-hidden="true" />
            </button>
          )}
          </div>
        )
      })}
    </nav>
    {menu && menuTab && (
      <MenuLayer
        label={t('tabs.menu.label', { name: (menuTab.host_name ?? '') + ' · ' + which(menuTab) })}
        heading={(menuTab.host_name ?? '') + ' · ' + which(menuTab)}
        anchor={menu.anchor}
        at="point"
        sheet={isCoarse()}
        trigger={() => ref.current?.querySelector<HTMLElement>(`[data-tab="${menu.id}"] .term-tab-main`) ?? null}
        items={menuItems(menuTab)}
        onClose={() => setMenu(null)}
      />
    )}
    {/* (Outside the tabs that scroll: always in sight — ux1101 N2.) */}
    <button type="button" className="icon-btn term-tab-new" onClick={p.onNew} aria-label={t('tabs.new')} aria-keyshortcuts={standalone ? (isMac ? 'Meta+T' : 'Control+Shift+T') : undefined} title={standalone ? `${t('tabs.new')} (${newTabKey})` : t('tabs.new')}>
        <Plus size={17} />
      </button>
    </>
  )
}

// The way between the agents' page and the terminals' where no rail is (700
// to 760 wide): what the rail does wider. Agents waiting, on its agents' half.
export function PageSwitch({ agentsPage, waiting }: { agentsPage?: boolean; waiting: number }) {
  const wb = useWorkbench()
  if (wb) return null
  return (
    <div className="term-pages" role="group" aria-label={t('tabs.pages')}>
      <button type="button" className="term-page" aria-current={agentsPage ? 'page' : undefined} onClick={() => !agentsPage && navigate('/agents')} title={t('nav.agents')} aria-label={waiting > 0 ? `${t('nav.agents')}，${t('nav.waitingN', { n: waiting })}` : t('nav.agents')}>
        <Bot size={15} aria-hidden="true" />
        {waiting > 0 && <span className="term-page-badge" aria-hidden="true">{waiting > 99 ? '99+' : waiting}</span>}
      </button>
      <button type="button" className="term-page" aria-current={agentsPage ? undefined : 'page'} onClick={() => agentsPage && navigate('/t')} title={t('nav.terminals')} aria-label={t('nav.terminals')}>
        <SquareTerminal size={15} aria-hidden="true" />
      </button>
    </div>
  )
}

// The terminals' page with none of them in front (/t): while the list is
// read, nothing; none running, says so, with the ways to one.
function TerminalsHome({ bar, none, onNew }: { bar: React.ReactNode; none: boolean; onNew: () => void }) {
  const wb = useWorkbench()
  usePrefs() // (drawn again when the theme changes, the system's included)
  const scheme = isDark() ? corralDark : corralLight
  return (
    <div className={'term-root terms-home ' + (scheme.dark ? 'dark' : 'light') + (wb ? ' wb-on' : '')} style={chromeVars(scheme) as React.CSSProperties}>
      <header className="term-bar">
        {/* (The way back to the hosts is the rail's, on the workbench.) */}
        {!wb && (
          <button type="button" className="icon-btn" onClick={() => navigate('/')} aria-label={t('term.backToHosts')} title={t('term.backToHosts')}>
            <ChevronLeft size={20} />
          </button>
        )}
        {bar}
      </header>
      <div className="term-body">
        {none && (
          <div className="agents-chat-empty">
            <p>{t('terms.none')}</p>
            <button type="button" className="btn btn-primary" onClick={onNew}>
              <Plus size={16} aria-hidden="true" />
              {t('tabs.new')}
            </button>
            <button type="button" className="btn btn-ghost" onClick={() => navigate('/')}>
              <Server size={16} aria-hidden="true" />
              {t('agents.toHosts')}
            </button>
          </div>
        )}
      </div>
      {wb && <Rail active="terminals" />}
      {wb && <StatusBar />}
    </div>
  )
}

