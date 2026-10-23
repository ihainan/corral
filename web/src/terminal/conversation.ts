import { useEffect, useRef, useState } from 'react'
import { api, ApiError } from '../api'
import { local } from '../storage'

// The conversation view's data: the agent's own log of the session in the
// focused (herdr) or active (tmux) pane, as the gateway reads it
// (internal/web/conversation.go). Polled while the view is shown and the
// page visible; each answer brings the entries changed since the last one.

export interface ConvoTool {
  id?: string
  name: string
  title?: string
  detail?: string
  old?: string
  new?: string
  add?: number // (an edit) lines added and removed
  del?: number
  done?: boolean
  error?: boolean
  result?: string
  result_lines?: number
  truncated?: boolean
}

export interface ConvoPart {
  kind: 'text' | 'thinking' | 'tool' | 'image'
  text?: string
  truncated?: boolean
  tool?: ConvoTool
}

export interface ConvoEntry {
  seq: number
  rev: number
  id: string
  role: 'user' | 'assistant' | 'summary' | 'note'
  ts?: string
  parts: ConvoPart[]
}

// An item of the agent's task list.
export interface ConvoTask {
  subject: string
  status: 'pending' | 'in_progress' | 'completed'
}

// What the entries are of (internal/web/conversation.go convoBind).
export interface ConvoBind {
  pane: string
  pid: number
  start: number
  session: string
  title?: string
}

interface Answer {
  available: boolean
  reason?: string
  bind?: ConvoBind
  agent_status?: 'busy' | 'idle'
  title?: string
  tasks?: ConvoTask[]
  gen: number
  rev: number
  reset?: boolean
  older?: boolean
  more?: boolean
  entries: ConvoEntry[]
}

export type ConvoStatus = 'loading' | 'ready' | 'none' | 'error'

export interface Convo {
  status: ConvoStatus
  reason?: string // with 'none': no_agent, no_log, unsupported
  entries: ConvoEntry[] // (the same array while nothing in it changed)
  older: boolean
  gen: number
  bind?: ConvoBind
  agentStatus?: 'busy' | 'idle' // (tmux) as Claude Code's file for the process says
  title?: string // the session's own title (Claude Code's, or the person's)
  tasks?: ConvoTask[] // the agent's task list (the same array while it did not change)
  // Reading has failed since okAt (ms): what shows may be out of date.
  failing: boolean
  okAt: number
  changedAt: number // (ms) when an entry was last added or changed
}

const initial = (): Convo => ({ status: 'loading', entries: [], older: false, gen: 0, failing: false, okAt: 0, changedAt: Date.now() })

const sameBind = (a?: ConvoBind, b?: ConvoBind) =>
  a === b || (!!a && !!b && a.pane === b.pane && a.pid === b.pid && a.start === b.start && a.session === b.session && a.title === b.title)

const POLL = 2000
const RETRY = 5000
const MAX = 2000

export function useConversation(sessionId: string, enabled: boolean, pane: string | undefined): Convo {
  const [convo, setConvo] = useState<Convo>(initial)
  const paneRef = useRef(pane)
  paneRef.current = pane
  const poke = useRef<() => void>(() => {})
  const lastOk = useRef(0) // (ms) the last answer, whatever it brought

  useEffect(() => {
    if (!enabled) return
    let stopped = false
    let timer: ReturnType<typeof setTimeout> | undefined
    let ctl: AbortController | null = null
    let gen = 0
    let rev = 0
    let byseq = new Map<number, ConvoEntry>()
    const run = async () => {
      timer = undefined
      if (stopped) return
      if (document.hidden) return // (visibilitychange starts it again)
      const mine = new AbortController()
      ctl = mine
      const pane = paneRef.current
      const q = new URLSearchParams({ gen: String(gen), rev: String(rev) })
      if (pane) q.set('pane', pane)
      let next = POLL
      try {
        const a = await api<Answer>('GET', `/api/sessions/${sessionId}/conversation?${q}`, undefined, { signal: mine.signal, timeoutMs: 25000 })
        if (stopped || mine.signal.aborted) return
        // (An answer for the pane that was in front: ask again for this one.)
        const now = Date.now()
        lastOk.current = now
        if (pane !== paneRef.current) next = 0
        else if (!a.available) {
          gen = a.gen
          rev = 0
          byseq = new Map()
          setConvo((c) =>
            c.status === 'none' && c.reason === a.reason && c.gen === a.gen && sameBind(c.bind, a.bind) && !c.failing
              ? c
              : { ...initial(), status: 'none', reason: a.reason, gen: a.gen, bind: a.bind, okAt: now, changedAt: now },
          )
        } else {
          const fresh = a.reset || a.gen !== gen
          if (fresh) byseq = new Map()
          gen = a.gen
          rev = a.rev
          for (const e of a.entries) byseq.set(e.seq, e)
          const changed = fresh || a.entries.length > 0
          let entries: ConvoEntry[] | null = null
          if (changed) {
            entries = [...byseq.values()].sort((x, y) => x.seq - y.seq)
            if (entries.length > MAX) {
              entries = entries.slice(-MAX)
              byseq = new Map(entries.map((e) => [e.seq, e]))
            }
          }
          // Nothing new: the same state (no rendering), unless its facts changed.
          setConvo((c) => {
            const older = !!a.older || (entries ?? c.entries).length === MAX
            const tasks = a.tasks?.length ? (JSON.stringify(a.tasks) === JSON.stringify(c.tasks) ? c.tasks : a.tasks) : undefined
            if (!entries && c.status === 'ready' && c.gen === a.gen && c.older === older && sameBind(c.bind, a.bind) && c.agentStatus === a.agent_status && c.title === a.title && c.tasks === tasks && !c.failing) return c
            return { status: 'ready', entries: entries ?? c.entries, older, gen: a.gen, bind: a.bind, agentStatus: a.agent_status, title: a.title || undefined, tasks, failing: false, okAt: now, changedAt: entries ? now : c.changedAt }
          })
          if (a.more) next = 300
        }
      } catch (e) {
        if (stopped || mine.signal.aborted || (e instanceof ApiError && e.code === 'aborted')) return
        next = RETRY
        setConvo((c) => (c.failing ? c : { ...c, status: c.entries.length ? c.status : 'error', failing: true, okAt: lastOk.current }))
      } finally {
        if (ctl === mine) ctl = null
      }
      if (!stopped) timer = setTimeout(run, next)
    }
    const onVisible = () => {
      if (!document.hidden && !timer && !ctl) void run()
    }
    // Another pane in front: what is on its way is for the old one.
    poke.current = () => {
      clearTimeout(timer)
      timer = undefined
      ctl?.abort()
      ctl = null
      // (Nothing of the pane that was in front shows meanwhile.)
      gen = 0
      rev = 0
      byseq = new Map()
      setConvo(initial())
      void run()
    }
    document.addEventListener('visibilitychange', onVisible)
    setConvo(initial())
    void run()
    return () => {
      stopped = true
      clearTimeout(timer)
      ctl?.abort()
      document.removeEventListener('visibilitychange', onVisible)
      poke.current = () => {}
    }
  }, [sessionId, enabled])

  // Another pane in front: read at once rather than at the next tick.
  useEffect(() => {
    poke.current()
  }, [pane])

  return convo
}

// Whether the agent is asking the person something (AskUserQuestion, not
// come back): the last thing in the log.
export function asking(entries: ConvoEntry[]): boolean {
  const e = entries[entries.length - 1]
  const p = e?.role === 'assistant' ? e.parts[e.parts.length - 1] : undefined
  return !!p?.tool && p.tool.name === 'AskUserQuestion' && !p.tool.done
}

// Whether the conversation view was shown, by session, on this device.
const KEY = 'corral.convoOpen'

export function convoWasOpen(session: string): boolean {
  try {
    const v = JSON.parse(local.get(KEY) ?? '[]')
    return Array.isArray(v) && v.includes(session)
  } catch {
    return false
  }
}

export function rememberConvo(session: string, open: boolean) {
  let v: string[] = []
  try {
    const got = JSON.parse(local.get(KEY) ?? '[]')
    if (Array.isArray(got)) v = got.filter((x) => typeof x === 'string')
  } catch {
    /* not ours */
  }
  v = v.filter((x) => x !== session)
  if (open) v.push(session)
  local.set(KEY, JSON.stringify(v.slice(-50)))
}
