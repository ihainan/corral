import { useEffect, useState } from 'react'
import { api, type HerdrAgent, type SessionInfo } from '../api'
import { agentKey } from './sessionName'

// The agents of the sessions the gateway has open, as the Agents page and
// the conversation view's list show them: what herdr reports for each herdr
// session (each agent once, however many tabs show its session), the
// tmux sessions apart (herdr's state is all there is to go by).

export type AgentStatus = 'blocked' | 'working' | 'done' | 'idle' | 'unknown'
export const STATUS_ORDER: AgentStatus[] = ['blocked', 'working', 'done', 'idle', 'unknown']

export interface AgentRow {
  key: string // agentKey: the agent, whichever tab shows its session
  s: SessionInfo
  a: HerdrAgent
  ws: string
  wsId: string // (its workspace's id: what renaming it names)
  status: AgentStatus
  last?: AgentStatus // (herdr lost: what it said last)
  lost?: boolean // (the gateway lost herdr after this summary)
  // Its turn over, subagents it sent off still out: it works on in the
  // background ("running"), or has for so long that it is to be looked at
  // ("stale"; see BG_STALE).
  bg?: 'running' | 'stale'
}

// How long subagents may be out before it is told as to be looked at
// (one stuck, or never back): an hour.
export const BG_STALE = 60 * 60 * 1000

// Looked at, on this device, while to be looked at so (by its agent, the
// time its subagents went out): no longer among those to look at.
const BG_ACK = 'corral.bgAck'
function bgAcks(): Record<string, number> {
  try {
    const v = JSON.parse(localStorage.getItem(BG_ACK) ?? '{}')
    return v && typeof v === 'object' ? v : {}
  } catch {
    return {}
  }
}
export function ackBg(key: string, since: number) {
  try {
    const all = bgAcks()
    if (all[key] === since) return
    all[key] = since
    const keys = Object.keys(all)
    for (const k of keys.slice(0, Math.max(0, keys.length - 100))) delete all[k]
    localStorage.setItem(BG_ACK, JSON.stringify(all))
  } catch {
    // (not kept: it is to be looked at again on this device)
  }
}

// What a herdr agent's status is told as, with what it left running: done
// or idle with subagents out works on; out for over BG_STALE, done, until
// looked at here. (While that is read for, as it is: not marked seen
// meanwhile, see AgentsChat; it moves only if it works on.)
function withBg(key: string, a: HerdrAgent, known: AgentStatus, now: number): { status: AgentStatus; bg?: AgentRow['bg'] } {
  if (known !== 'done' && known !== 'idle') return { status: known }
  if (a.bg !== 'running') return { status: known }
  if (a.bg_since && now - a.bg_since > BG_STALE) return bgAcks()[key] === a.bg_since ? { status: 'idle', bg: 'stale' } : { status: 'done', bg: 'stale' }
  return { status: 'working', bg: 'running' }
}

export const isClaude = (name?: string) => !!name && /claude/i.test(name)
export const isCodex = (name?: string) => !!name && /codex/i.test(name)
export const isOpenCode = (name?: string) => !!name && /opencode/i.test(name)
// (Whose conversation is read: Claude Code's and Codex's logs, OpenCode's database.)
export const readsLog = (name?: string) => isClaude(name) || isCodex(name) || isOpenCode(name)

// A pane's title without the mark Claude Code puts before it (its spinner's
// frames, ◐ and the like while it works, ✳ when idle).
export const paneTitle = (s: string) => s.replace(/^[\s·✢✳✶✻✽*⠀-⣿◐◑◒◓○●◌]+/u, '').trim()

// What a herdr agent is about, as its pane names it: a Codex's thread's
// name where the gateway has one (Codex titles its pane "<thread> |
// <folder>", or the folder alone); none where the title names only the
// folder (a row says that already, or need not); else the pane's title.
export const paneName = (a: HerdrAgent) => a.thread_title || (a.bare_title ? '' : paneTitle(a.title))

export function listAgents(sessions: SessionInfo[]): { rows: AgentRow[]; others: SessionInfo[] } {
  const now = Date.now()
  const byKey = new Map<string, AgentRow>()
  const others: SessionInfo[] = []
  for (const s of sessions) {
    const m = s.meta
    if (s.persistence !== 'herdr' || !m || m.t !== 'herdr') {
      if (s.persistence === 'tmux' || s.persistence === 'herdr') others.push(s)
      continue
    }
    for (const w of m.workspaces) {
      for (const a of w.agents) {
        const key = agentKey(s, a.pane_id)
        const had = byKey.get(key)
        // (Of two tabs' word, a current summary's over one the gateway lost.)
        if (had && !(had.lost && !m.lost)) continue
        const known = (STATUS_ORDER as string[]).includes(a.status) ? (a.status as AgentStatus) : 'unknown'
        byKey.set(key, m.lost ? { key, s, a, ws: w.label, wsId: w.id, status: 'unknown', lost: true, last: known === 'unknown' ? undefined : known } : { key, s, a, ws: w.label, wsId: w.id, ...withBg(key, a, known, now) })
      }
    }
  }
  const rows = [...byKey.values()]
  // (Of one status, the latest to change first.)
  rows.sort((x, y) => STATUS_ORDER.indexOf(x.status) - STATUS_ORDER.indexOf(y.status) || (y.a.since ?? 0) - (x.a.since ?? 0) || x.s.host_name.localeCompare(y.s.host_name) || x.ws.localeCompare(y.ws))
  return { rows, others }
}

// The running sessions, polled while on (one request at a time; every
// HIDDEN_EVERY while the page is behind another). null until the first answer.
export const HIDDEN_EVERY = 30000

// The page's title says how many agents wait for an answer (n), so a tab
// behind others tells it.
// The app icon (where installed and allowed) says it too. null: no longer
// known here (the page left): the title plain, the icon as it was (alerts
// keep it, see sw.js).
export function waitingTitle(n: number | null, list?: SessionInfo[] | null) {
  document.title = n ? `(${n}) Corral` : 'Corral'
  if (n !== null) appBadge(n, (list && listReadAt.get(list)) || 0)
}

// The app icon's number (0: none), set by the page only. Not everywhere: an
// app not installed, or without leave to alert, has none; the browser
// refuses quietly. (Not by the service worker: there Chromium's badge
// calls, in its message events at least, crash the page, 11-02. Alerts say
// the gateway's count, Note.Badge, for when it can again.) seq: unused till
// then (see readAt).
export function appBadge(n: number, _seq = 0) {
  const nav = navigator as Navigator & { setAppBadge?: (n: number) => Promise<void>; clearAppBadge?: () => Promise<void> }
  try {
    void (n > 0 ? nav.setAppBadge?.(n) : nav.clearAppBadge?.())?.catch(() => {})
  } catch {
    // (none)
  }
}

// When a session list was read, by the gateway's clock alerts are numbered
// by (its X-Corral-Now, microseconds): each list its own.
const listReadAt = new WeakMap<SessionInfo[], number>()
export function readAt(res: Response): number {
  const t = Number(res.headers.get('X-Corral-Now'))
  return Number.isFinite(t) && t > 0 ? t : 0
}
export function markRead(list: SessionInfo[], at: number) {
  if (at) listReadAt.set(list, at)
}
// (The last list read, for this page's life: a tab opened later starts from
// it rather than from nothing, so its list does not appear a moment later.)
let lastList: SessionInfo[] | null = null

// (Another account signed in, or none: nothing of the one before stays.)
export function forgetSessions() {
  lastList = null
  listFailed = false
  told()
}

// Whoever reads the session list says so here (the agents page's poll, the
// hosts page's), and whoever shows what it says (the navigation's count)
// learns of it: one list, not a poll of its own beside each. failed: the
// last read did not come back (what is known is no longer current).
let listFailed = false
const listeners = new Set<() => void>()
const told = () => listeners.forEach((f) => f())
export function noteSessions(all: SessionInfo[] | null, at = 0) {
  if (all === null) listFailed = true
  else {
    rawList = all.filter((x) => x.state === 'running')
    lastList = withExpected(rawList)
    markRead(lastList, at)
    listFailed = false
  }
  told()
}

// A change asked for here and answered, shown at once — not when the next
// read of the list, seconds later, says it (11-14: a name given, the list
// and the title said the old one for three seconds). Laid over each list
// read until one says it of itself (seen), or for a while at most; the list
// is read again at once.
type Expect = { id: string; apply: (s: SessionInfo) => SessionInfo; seen: (s: SessionInfo) => boolean; until: number }
let expects: Expect[] = []
let rawList: SessionInfo[] | null = null
const EXPECT_MS = 15000
function withExpected(list: SessionInfo[]): SessionInfo[] {
  const now = Date.now()
  expects = expects.filter((x) => {
    const s = list.find((y) => y.id === x.id)
    return x.until > now && !!s && !x.seen(s)
  })
  if (!expects.length) return list
  return list.map((s) => expects.reduce((acc, x) => (x.id === s.id ? x.apply(acc) : acc), s))
}
const readNow = new Set<() => void>()
// The list read again now, wherever it is read (a new agent started: in it sooner than the next read).
export function readSessionsNow() {
  readNow.forEach((f) => f())
}
// (ms: how long at most — short where what follows may undo it of itself: an
// agent answered may ask again a second later, and that must show.)
export function expectSession(id: string, apply: (s: SessionInfo) => SessionInfo, seen: (s: SessionInfo) => boolean, ms = EXPECT_MS) {
  expects.push({ id, apply, seen, until: Date.now() + ms })
  if (rawList) {
    lastList = withExpected(rawList)
    markRead(lastList, listReadAt.get(rawList) ?? 0)
  }
  told()
  readNow.forEach((f) => f())
}

// A pane's agent at work again, as an answer to it puts it (not waiting for the person).
export function answeredHerdr(s: SessionInfo, pane: string): SessionInfo {
  if (s.meta?.t !== 'herdr') return s
  return { ...s, meta: { ...s.meta, workspaces: s.meta.workspaces.map((w) => ({ ...w, agents: w.agents.map((a) => (a.pane_id === pane && a.status === 'blocked' ? { ...a, status: 'working' } : a)) })) } }
}
export const paneWaits = (s: SessionInfo, pane: string) => s.meta?.t === 'herdr' && s.meta.workspaces.some((w) => w.agents.some((a) => a.pane_id === pane && a.status === 'blocked'))

// herdr's names, as a rename gives them: a workspace's, a tab's, a pane's.
export function renamedHerdr(s: SessionInfo, ws?: { id: string; label: string }, tab?: { id: string; label: string }, pane?: { id: string; label: string }): SessionInfo {
  if (s.meta?.t !== 'herdr') return s
  return {
    ...s,
    meta: {
      ...s.meta,
      workspaces: s.meta.workspaces.map((w) => ({
        ...w,
        label: ws && w.id === ws.id ? ws.label : w.label,
        agents: w.agents.map((a) => ({ ...a, tab_label: tab && a.tab === tab.id ? tab.label : a.tab_label, label: pane && a.pane_id === pane.id ? pane.label : a.label })),
      })),
    },
  }
}

// The agents waiting for an answer in these sessions, each counted once
// (several tabs may show one herdr session), as herdr's summaries say; a
// summary herdr lost since counts for nothing (it was so then, not now).
export function waitingAgents(list: SessionInfo[]): number {
  const seen = new Set<string>()
  for (const s of list) {
    if (s.meta?.t !== 'herdr' || s.meta.lost) continue
    for (const w of s.meta.workspaces) for (const a of w.agents) if (a.status === 'blocked') seen.add(agentKey(s, a.pane_id))
  }
  return seen.size
}

// One session's agents, as its summary says: how many wait, work, are done
// and not looked at. lost: the summary is old (herdr lost since), or there is
// none yet — none of it is to be told as now. null: no agents' summary (a plain shell, tmux).
export function sessionAgents(s: SessionInfo): { waiting: number; working: number; done: number; lost: boolean } | null {
  // (A herdr session herdr has said nothing of yet, as it starts: not known —
  // told so, not as all well — review b006ffd.)
  if (s.meta?.t !== 'herdr') return s.persistence === 'herdr' ? { waiting: 0, working: 0, done: 0, lost: true } : null
  const n = (st: string) => s.meta!.t === 'herdr' ? s.meta!.workspaces.reduce((k, w) => k + w.agents.filter((a) => a.status === st).length, 0) : 0
  return { waiting: n('blocked'), working: n('working'), done: n('done'), lost: !!s.meta.lost }
}

// How many agents wait for the user, for the navigation: null until known;
// stale when the last read failed (the number shown is what was so then).
// poll: read the list here too (pages where nothing else does) — every
// VISIBLE_EVERY while in sight, HIDDEN_EVERY behind another tab.
const VISIBLE_EVERY = 15000
export function useWaitingCount(poll: boolean): { n: number | null; stale: boolean } {
  const read = () => ({ n: lastList ? waitingAgents(lastList) : null, stale: listFailed })
  const [v, setV] = useState(read)
  useEffect(() => {
    const f = () => setV((cur) => {
      const next = read()
      return cur.n === next.n && cur.stale === next.stale ? cur : next
    })
    listeners.add(f)
    f()
    return () => void listeners.delete(f)
  }, [])
  useEffect(() => {
    if (!poll) return
    let stopped = false
    let asking = false
    let timer: ReturnType<typeof setTimeout> | undefined
    const load = () => {
      timer = undefined
      if (asking) return
      asking = true
      let at = 0
      api<SessionInfo[]>('GET', '/api/sessions', undefined, { timeoutMs: 15000, onResponse: (r) => (at = readAt(r)) })
        .then((l) => !stopped && noteSessions(l, at))
        .catch(() => !stopped && noteSessions(null))
        .finally(() => {
          asking = false
          if (!stopped && !timer) timer = setTimeout(load, document.hidden ? HIDDEN_EVERY : VISIBLE_EVERY)
        })
    }
    const onVisible = () => {
      if (!document.hidden && !asking) {
        clearTimeout(timer)
        load()
      }
    }
    document.addEventListener('visibilitychange', onVisible)
    load()
    return () => {
      stopped = true
      clearTimeout(timer)
      document.removeEventListener('visibilitychange', onVisible)
    }
  }, [poll])
  return v
}

// A session as the last list read has it (null when it has none).
export const knownSession = (id: string): SessionInfo | null => lastList?.find((x) => x.id === id) ?? null

export function useRunningSessions(on: boolean, every = 3000): { sessions: SessionInfo[] | null; failed: boolean } {
  const [sessions, setSessions] = useState<SessionInfo[] | null>(() => lastList)
  const [failed, setFailed] = useState(false)
  // (A change laid over the list here, or read by another: shown as it is.)
  useEffect(() => {
    const f = () => setSessions(lastList)
    listeners.add(f)
    return () => void listeners.delete(f)
  }, [])
  useEffect(() => {
    if (!on) return
    let stopped = false
    let timer: ReturnType<typeof setTimeout> | undefined
    let asking = false
    // (Behind another tab, now and then still: what waits is told in the title.)
    const load = () => {
      timer = undefined
      if (asking) return
      asking = true
      // (One that never comes back, as after a network change, is given up: the next goes.)
      let at = 0
      api<SessionInfo[]>('GET', '/api/sessions', undefined, { timeoutMs: 15000, onResponse: (r) => (at = readAt(r)) })
        .then((l) => {
          if (stopped) return
          noteSessions(l, at)
          setSessions(lastList)
          setFailed(false)
        })
        .catch(() => {
          if (stopped) return
          setFailed(true)
          noteSessions(null)
        })
        .finally(() => {
          asking = false
          if (!stopped && !timer) timer = setTimeout(load, document.hidden ? HIDDEN_EVERY : every)
        })
    }
    const onVisible = () => {
      if (!document.hidden && !asking) {
        clearTimeout(timer)
        load()
      }
    }
    // (A change just asked for: read again at once, to see it said.)
    const now = () => {
      if (asking) return
      clearTimeout(timer)
      load()
    }
    readNow.add(now)
    document.addEventListener('visibilitychange', onVisible)
    load()
    return () => {
      stopped = true
      clearTimeout(timer)
      readNow.delete(now)
      document.removeEventListener('visibilitychange', onVisible)
    }
  }, [on, every])
  return { sessions, failed }
}

// matchQuery: every word of what was typed is in one of the fields (case
// aside), as the agents' lists are searched.
export function matchQuery(q: string, ...fields: (string | undefined)[]): boolean {
  const words = q.toLowerCase().split(/\s+/).filter(Boolean)
  if (!words.length) return true
  const hay = fields.filter(Boolean).join('\n').toLowerCase()
  return words.every((w) => hay.includes(w))
}
