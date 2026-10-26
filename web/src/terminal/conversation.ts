import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { api, ApiError } from '../api'

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
  patch?: ConvoHunk[] // (an edit) as Claude Code reported making it; then old and new are not sent
  patch_cut?: boolean
  same?: boolean // (an edit that changed nothing: a file written over with what it held)
  done?: boolean
  error?: boolean
  result?: string
  result_lines?: number
  truncated?: boolean
}

// A run of an edit's lines (" " kept, "-" removed, "+" added), from these line numbers.
export interface ConvoHunk {
  old_start: number
  new_start: number
  lines: string[]
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
  mid?: boolean // (a message sent while the agent worked, taken in during its turn)
}

// A message the person sent while the agent worked, not taken in yet.
export interface ConvoQueued {
  text: string
  ts?: string
}

// An item of the agent's task list.
export interface ConvoTask {
  subject: string
  status: 'pending' | 'in_progress' | 'completed'
}

// What the session runs with, as its log last said it (internal/convo Meta).
export interface ConvoMeta {
  model?: string // the model's id, as the API named it
  mode?: string // the permission mode: default, acceptEdits, plan, auto, ...
  branch?: string
  context?: number // tokens the latest request sent
}

// What the entries are of (internal/web/conversation.go convoBind).
export interface ConvoBind {
  pane: string
  pid: number
  start: number
  session: string
  agent?: string // "claude" or "codex": whose process it is
  title?: string
}

interface Answer {
  available: boolean
  reason?: string
  bind?: ConvoBind
  agent_status?: 'busy' | 'idle'
  title?: string
  tasks?: ConvoTask[]
  meta?: ConvoMeta
  queued?: ConvoQueued[]
  gen: number
  rev: number
  reset?: boolean
  older?: boolean
  older_cut?: boolean // (what is before cannot be read in as well)
  more?: boolean
  entries: ConvoEntry[]
}

export type ConvoStatus = 'loading' | 'ready' | 'none' | 'error'

export interface Convo {
  status: ConvoStatus
  reason?: string // with 'none': no_agent, no_log, unsupported
  entries: ConvoEntry[] // (the same array while nothing in it changed)
  older: boolean // the conversation began before the first entry
  olderCut: boolean // and what is before cannot be read in as well
  loadingOlder: boolean // the part before is being read in (see loadOlder)
  // Reads in the part of the log before the first entry (its entries come
  // first, with lower seqs); while one is on its way, nothing more.
  loadOlder: () => void
  gen: number
  bind?: ConvoBind
  agentStatus?: 'busy' | 'idle' // (tmux) as Claude Code's file for the process says
  title?: string // the session's own title (Claude Code's, or the person's)
  tasks?: ConvoTask[] // the agent's task list (the same array while it did not change)
  meta?: ConvoMeta // (the same object while it did not change)
  queued?: ConvoQueued[] // (likewise)
  // Reading has failed since okAt (ms): what shows may be out of date.
  failing: boolean
  okAt: number
  changedAt: number // (ms) when an entry was last added or changed
  of?: string // (the session and pane it is of: see kept)
}

const noop = () => {}
const initial = (of?: string): Convo => ({ status: 'loading', entries: [], older: false, olderCut: false, loadingOlder: false, loadOlder: noop, gen: 0, failing: false, okAt: 0, changedAt: Date.now(), of })

// What was last read of a session's pane, for this page's life: shown at
// once when it is in front again (its tab back to the front, the pane
// focused again, a tab of it opened again), then brought up to date from
// where it was. Without it, each switch drew "reading…" first: a flash.
// (All of it from the same answer, kept when it is read: what shows from it
// is made from it again, never from a state not yet drawn.)
interface Kept {
  gen: number
  rev: number
  byseq: Map<number, ConvoEntry>
  a: Omit<Answer, 'entries' | 'gen' | 'rev' | 'reset' | 'more'>
}
const kept = new Map<string, Kept>()
const KEEP = 12
const keyOf = (session: string, pane?: string) => session + ' ' + (pane ?? '')
function remember(key: string, gen: number, rev: number, byseq: Map<number, ConvoEntry>, a: Answer) {
  const { entries: _e, gen: _g, rev: _r, reset: _x, more: _m, ...facts } = a
  kept.delete(key) // (the latest last: the map keeps its order of use)
  kept.set(key, { gen, rev, byseq, a: facts })
  while (kept.size > KEEP) kept.delete(kept.keys().next().value!)
}

// (Another account signed in, or none: nothing of the one before stays.)
export function forgetConversations() {
  kept.clear()
}

const sameBind = (a?: ConvoBind, b?: ConvoBind) =>
  a === b || (!!a && !!b && a.pane === b.pane && a.pid === b.pid && a.start === b.start && a.session === b.session && a.title === b.title)

const sameMeta = (a?: ConvoMeta, b?: ConvoMeta) =>
  a === b || (!!a && !!b && a.model === b.model && a.mode === b.mode && a.branch === b.branch && a.context === b.context)

const POLL = 2000
const RETRY = 5000
const MAX = 6000 // (entries kept, as the gateway keeps at most)

export function useConversation(sessionId: string, enabled: boolean, pane: string | undefined): Convo {
  const [convo, setConvo] = useState<Convo>(() => initial())
  const paneRef = useRef(pane)
  paneRef.current = pane
  const poke = useRef<() => void>(() => {})
  const lastOk = useRef(0) // (ms) the last answer, whatever it brought
  // The part before asked for (by the reading of its gen), until an answer to that came.
  const wantOlder = useRef<number | null>(null)
  const pokeOlder = useRef<() => void>(() => {})

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
      const key = keyOf(sessionId, pane)
      const q = new URLSearchParams({ gen: String(gen), rev: String(rev) })
      if (pane) q.set('pane', pane)
      const asksOlder = wantOlder.current !== null && wantOlder.current === gen
      if (asksOlder) q.set('older', '1')
      let next = POLL
      try {
        const a = await api<Answer>('GET', `/api/sessions/${sessionId}/conversation?${q}`, undefined, { signal: mine.signal, timeoutMs: 25000 })
        if (stopped || mine.signal.aborted) return
        // (An answer for the pane that was in front: ask again for this one.)
        const now = Date.now()
        lastOk.current = now
        if (pane !== paneRef.current) next = 0
        else if (!a.available) {
          wantOlder.current = null // (nothing to read before: what was asked of is gone)
          gen = a.gen
          rev = 0
          byseq = new Map()
          setConvo((c) =>
            c.of === key && c.status === 'none' && c.reason === a.reason && c.gen === a.gen && sameBind(c.bind, a.bind) && !c.failing
              ? c
              : { ...initial(key), status: 'none', reason: a.reason, gen: a.gen, bind: a.bind, okAt: now, changedAt: now },
          )
          remember(key, gen, rev, byseq, a)
        } else {
          const fresh = a.reset || a.gen !== gen
          // (Answered, or another reading: asked of that no more. Asked of a
          // reading before this one, though it is still this gen: that too.)
          if (asksOlder || fresh || (wantOlder.current !== null && wantOlder.current !== a.gen)) wantOlder.current = null
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
            const olderCut = older && (!!a.older_cut || (entries ?? c.entries).length === MAX)
            const loadingOlder = wantOlder.current !== null
            const tasks = a.tasks?.length ? (JSON.stringify(a.tasks) === JSON.stringify(c.tasks) ? c.tasks : a.tasks) : undefined
            const meta = a.meta ? (sameMeta(a.meta, c.meta) ? c.meta : a.meta) : undefined
            const queued = a.queued?.length ? (JSON.stringify(a.queued) === JSON.stringify(c.queued) ? c.queued : a.queued) : undefined
            if (!entries && c.of === key && c.status === 'ready' && c.gen === a.gen && c.older === older && c.olderCut === olderCut && c.loadingOlder === loadingOlder && sameBind(c.bind, a.bind) && c.agentStatus === a.agent_status && c.title === a.title && c.tasks === tasks && c.meta === meta && c.queued === queued && !c.failing) return c
            return { ...initial(key), status: 'ready', entries: entries ?? c.entries, older, olderCut, loadingOlder, gen: a.gen, bind: a.bind, agentStatus: a.agent_status, title: a.title || undefined, tasks, meta, queued, failing: false, okAt: now, changedAt: entries ? now : c.changedAt, of: key }
          })
          remember(key, gen, rev, byseq, a)
          if (a.more) next = 300
          if (wantOlder.current !== null) next = 0 // (asked for meanwhile)
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
    // What shows first of the pane in front: what was last read of it, if
    // anything (its entries as read up to where reading goes on from), else
    // "reading". Nothing of another pane shows meanwhile.
    const begin = () => {
      wantOlder.current = null // (of the pane before)
      const key = keyOf(sessionId, paneRef.current)
      const was = kept.get(key)
      if (was) {
        gen = was.gen
        rev = was.rev
        byseq = new Map(was.byseq)
        const now = Date.now()
        const k = was.a
        if (!k.available) setConvo({ ...initial(key), status: 'none', reason: k.reason, gen, bind: k.bind, okAt: now, changedAt: now })
        else {
          const entries = [...byseq.values()].sort((x, y) => x.seq - y.seq)
          const older = !!k.older || entries.length === MAX
          setConvo({ ...initial(key), status: 'ready', entries, older, olderCut: older && (!!k.older_cut || entries.length === MAX), gen, bind: k.bind, agentStatus: k.agent_status, title: k.title || undefined, tasks: k.tasks?.length ? k.tasks : undefined, meta: k.meta, queued: k.queued?.length ? k.queued : undefined, failing: false, okAt: now, changedAt: now, of: key })
        }
      } else {
        gen = 0
        rev = 0
        byseq = new Map()
        setConvo(initial(key))
      }
    }
    // The part before, asked for: read now, or once what is on its way is answered.
    pokeOlder.current = () => {
      if (ctl || stopped) return
      clearTimeout(timer)
      timer = undefined
      void run()
    }
    // Another pane in front: what is on its way is for the old one.
    poke.current = () => {
      clearTimeout(timer)
      timer = undefined
      ctl?.abort()
      ctl = null
      begin()
      void run()
    }
    document.addEventListener('visibilitychange', onVisible)
    begin()
    void run()
    return () => {
      stopped = true
      clearTimeout(timer)
      ctl?.abort()
      document.removeEventListener('visibilitychange', onVisible)
      poke.current = () => {}
      pokeOlder.current = () => {}
    }
  }, [sessionId, enabled])

  // Another pane in front: read at once rather than at the next tick.
  useEffect(() => {
    poke.current()
  }, [pane])

  const loadOlder = useCallback(() => {
    if (wantOlder.current !== null) return
    setConvo((c) => {
      if (c.status !== 'ready' || !c.older || c.olderCut || c.loadingOlder) return c
      wantOlder.current = c.gen
      return { ...c, loadingOlder: true }
    })
    // (After the state above: the request goes with what it asks for.)
    setTimeout(() => pokeOlder.current())
  }, [])
  return useMemo(() => ({ ...convo, loadOlder }), [convo, loadOlder])
}

// Whether the agent is asking the person something (AskUserQuestion, not
// come back): the last thing it did (notes may have come since).
export function asking(entries: ConvoEntry[]): boolean {
  for (let i = entries.length - 1; i >= 0; i--) {
    const e = entries[i]
    if (e.role === 'note') continue
    const p = e.role === 'assistant' ? e.parts[e.parts.length - 1] : undefined
    return !!p?.tool && p.tool.name === 'AskUserQuestion' && !p.tool.done
  }
  return false
}
