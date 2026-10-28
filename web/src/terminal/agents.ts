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
        byKey.set(key, m.lost ? { key, s, a, ws: w.label, status: 'unknown', lost: true, last: known === 'unknown' ? undefined : known } : { key, s, a, ws: w.label, ...withBg(key, a, known, now) })
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
export function waitingTitle(n: number) {
  document.title = n > 0 ? `(${n}) Corral` : 'Corral'
}
// (The last list read, for this page's life: a tab opened later starts from
// it rather than from nothing, so its list does not appear a moment later.)
let lastList: SessionInfo[] | null = null

// (Another account signed in, or none: nothing of the one before stays.)
export function forgetSessions() {
  lastList = null
}

// A session as the last list read has it (null when it has none).
export const knownSession = (id: string): SessionInfo | null => lastList?.find((x) => x.id === id) ?? null

export function useRunningSessions(on: boolean, every = 3000): { sessions: SessionInfo[] | null; failed: boolean } {
  const [sessions, setSessions] = useState<SessionInfo[] | null>(() => lastList)
  const [failed, setFailed] = useState(false)
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
      api<SessionInfo[]>('GET', '/api/sessions', undefined, { timeoutMs: 15000 })
        .then((l) => {
          if (stopped) return
          lastList = l.filter((x) => x.state === 'running')
          setSessions(lastList)
          setFailed(false)
        })
        .catch(() => !stopped && setFailed(true))
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
    document.addEventListener('visibilitychange', onVisible)
    load()
    return () => {
      stopped = true
      clearTimeout(timer)
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
