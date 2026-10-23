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

interface Answer {
  available: boolean
  reason?: string
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
  entries: ConvoEntry[]
  older: boolean
}

const POLL = 2000
const RETRY = 5000
const MAX = 2000

export function useConversation(sessionId: string, enabled: boolean, pane: string | undefined): Convo {
  const [convo, setConvo] = useState<Convo>({ status: 'loading', entries: [], older: false })
  const paneRef = useRef(pane)
  paneRef.current = pane
  const poke = useRef<() => void>(() => {})

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
      ctl = new AbortController()
      const q = new URLSearchParams({ gen: String(gen), rev: String(rev) })
      if (paneRef.current) q.set('pane', paneRef.current)
      let next = POLL
      try {
        const a = await api<Answer>('GET', `/api/sessions/${sessionId}/conversation?${q}`, undefined, { signal: ctl.signal, timeoutMs: 25000 })
        if (stopped) return
        if (!a.available) {
          gen = a.gen
          rev = 0
          byseq = new Map()
          setConvo({ status: 'none', reason: a.reason, entries: [], older: false })
        } else {
          if (a.reset || a.gen !== gen) byseq = new Map()
          gen = a.gen
          rev = a.rev
          for (const e of a.entries) byseq.set(e.seq, e)
          let entries = [...byseq.values()].sort((x, y) => x.seq - y.seq)
          if (entries.length > MAX) {
            entries = entries.slice(-MAX)
            byseq = new Map(entries.map((e) => [e.seq, e]))
          }
          setConvo({ status: 'ready', entries, older: !!a.older || entries.length === MAX })
          if (a.more) next = 300
        }
      } catch (e) {
        if (stopped || (e instanceof ApiError && e.code === 'aborted')) return
        next = RETRY
        setConvo((c) => ({ ...c, status: c.entries.length ? c.status : 'error' }))
      } finally {
        ctl = null
      }
      if (!stopped) timer = setTimeout(run, next)
    }
    const onVisible = () => {
      if (!document.hidden && !timer && !ctl) void run()
    }
    poke.current = () => {
      if (timer) {
        clearTimeout(timer)
        timer = undefined
        void run()
      }
    }
    document.addEventListener('visibilitychange', onVisible)
    setConvo({ status: 'loading', entries: [], older: false })
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
