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
}

export const isClaude = (name?: string) => !!name && /claude/i.test(name)
export const isCodex = (name?: string) => !!name && /codex/i.test(name)
// (Whose log the conversation reads: Claude Code's and Codex's.)
export const readsLog = (name?: string) => isClaude(name) || isCodex(name)

// A pane's title without the mark Claude Code puts before it (its spinner's
// frames, ◐ and the like while it works, ✳ when idle).
export const paneTitle = (s: string) => s.replace(/^[\s·✢✳✶✻✽*⠀-⣿◐◑◒◓○●◌]+/u, '').trim()

export function listAgents(sessions: SessionInfo[]): { rows: AgentRow[]; others: SessionInfo[] } {
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
        byKey.set(key, m.lost ? { key, s, a, ws: w.label, status: 'unknown', lost: true, last: known === 'unknown' ? undefined : known } : { key, s, a, ws: w.label, status: known })
      }
    }
  }
  const rows = [...byKey.values()]
  // (Of one status, the latest to change first.)
  rows.sort((x, y) => STATUS_ORDER.indexOf(x.status) - STATUS_ORDER.indexOf(y.status) || (y.a.since ?? 0) - (x.a.since ?? 0) || x.s.host_name.localeCompare(y.s.host_name) || x.ws.localeCompare(y.ws))
  return { rows, others }
}

// The running sessions, polled while on and the page is visible (one
// request at a time). null until the first answer.
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
    const load = () => {
      timer = undefined
      if (document.hidden || asking) return
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
          if (!stopped && !timer) timer = setTimeout(load, every)
        })
    }
    const onVisible = () => {
      if (!document.hidden && !timer && !asking) load()
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
