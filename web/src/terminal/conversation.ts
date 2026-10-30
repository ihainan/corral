import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { api, ApiError, authGen } from '../api'

// The conversation view's data: the agent's own log of the session in the
// focused (herdr) or active (tmux) pane, as the gateway reads it
// (internal/web/conversation.go). Polled while the view is shown and the
// page visible; each answer brings the entries changed since the last one.

export interface ConvoTool {
  id?: string
  name: string
  title?: string
  detail?: string
  // (Codex's write_stdin) what was put to a command still running: the keys
  // pressed by name, else how many characters were typed (never what).
  stdin?: { session: string; keys?: string[]; chars?: number }
  old?: string
  new?: string
  add?: number // (an edit) lines added and removed
  del?: number
  patch?: ConvoHunk[] // (an edit) as Claude Code reported making it; then old and new are not sent
  patch_cut?: boolean
  same?: boolean // (an edit that changed nothing: a file written over with what it held)
  // The files a command changed, each with its own patch (a shell command
  // that edits); more_files: it changed that many more than are here.
  files?: ConvoToolFile[]
  more_files?: number
  done?: boolean
  error?: boolean
  result?: string
  result_lines?: number
  // truncated: result is the start of it; result_tail, its end (where kept),
  // with omitted whole lines left out between (0: not told in lines).
  truncated?: boolean
  result_tail?: string
  omitted?: number
  // (AskUserQuestion) its questions as asked, and once answered, what to each
  asks?: ConvoAsk[]
  answers?: string[]
  agent_id?: string // (Agent, Task) the subagent it sent off: its own conversation can be read
  // (ExitPlanMode) the plan put to the person (plan_cut: only its start), its file
  plan?: string
  plan_cut?: boolean
  plan_path?: string
  images?: number // (pictures read: a screenshot, a PDF's pages)
  refs?: number // (tools a search found)
}

// A question of an AskUserQuestion (cut: more of it than is kept).
export interface ConvoAsk {
  header?: string
  question: string
  multi?: boolean
  options: { label: string; description?: string; preview?: string; preview_cut?: boolean }[]
  cut?: boolean
}

// One file a call changed.
export interface ConvoToolFile {
  path: string
  add?: number
  del?: number
  patch?: ConvoHunk[]
  patch_cut?: boolean
}

// A run of an edit's lines (" " kept, "-" removed, "+" added), from these line numbers.
export interface ConvoHunk {
  old_start: number
  new_start: number
  lines: string[]
}

export interface ConvoPart {
  kind: 'text' | 'thinking' | 'tool' | 'image' | 'noted' | 'trouble' | 'event' | 'shell'
  text?: string
  truncated?: boolean
  tool?: ConvoTool
  noted?: ConvoNoted
  trouble?: ConvoTrouble
  event?: ConvoEvent
  shell?: ConvoShell
}

// What a command the person ran with "!" printed (internal/convo Shell):
// its output's start and end where long, stderr apart.
export interface ConvoShell {
  out?: string
  out_tail?: string
  omitted?: number
  truncated?: boolean
  err?: string
  err_cut?: boolean
}

// Something that happened to the session itself (internal/convo/convo.go
// Event): it was compacted (trigger "auto" or "manual"; the tokens before
// and after; how long it took), a notice that that was while it sat idle,
// a scheduled task (or a loop's) set a turn going with its prompt.
export interface ConvoEvent {
  kind: 'compact' | 'idle_compact' | 'scheduled' | 'limit' | 'limit_wait' | 'limit_reset' | 'limit_cancelled' | 'recap' | 'goal' | 'notice' | 'task'
  trigger?: string
  pre?: number
  post?: number
  ms?: number
  text?: string
  cut?: boolean
  loop?: boolean
  echo?: boolean // (scheduled) told by the turn's own row: right after its firing, the same turn
  goal?: string // (goal) its condition, set or judged met (or not), and why
  set?: boolean
  met?: boolean
  why?: string
  level?: string // (notice) "notice" or "warning"
  resets_at?: number // (limit) when it resets, Unix seconds
  limit_kind?: string // (limit) "five_hour", "seven_day"…
  task?: 'cmd' | 'agent' | 'monitor' // (task) work set going in the background, ended or waiting: what it was,
  status?: 'done' | 'failed' | 'stopped' | 'waiting' // how it ended,
  code?: number // its exit code, if said (text: its name, a command whole where it had no description)
}

// Something that went wrong which the thread would otherwise not show: the
// agent is seen falling silent, or answering half a sentence, with nothing
// said of why (internal/convo/convo.go Trouble).
export interface ConvoTrouble {
  kind: 'refused' | 'api' | 'cut' | 'hook'
  text?: string // what was said of it, in the words it was said in
  name?: string // the model it went on with, or the hook that failed
  status?: number // the interface's status code, or the hook's exit code
  secs?: number // how long a hook was given before it was cut off
  again?: boolean // it was tried again (by name), or the hook held the agent back
  cut?: boolean // text is only the start of what was said
  attempt?: number // (api) it is tried again: this time, of max
  max?: number
  retry_ms?: number
}

// Files the turn was given without the conversation saying so: ones that
// changed while the agent worked ('changed': by whom cannot be told), or
// the ones the person referred to in what they wrote ('brought').
export interface ConvoNoted {
  kind: 'changed' | 'brought'
  paths: string[]
  more?: number
}

export interface ConvoEntry {
  seq: number
  rev: number
  id: string
  role: 'user' | 'assistant' | 'summary' | 'note'
  ts?: string
  parts: ConvoPart[]
  mid?: boolean // (a message sent while the agent worked, taken in during its turn)
  gone?: boolean // (taken back: the conversation was rewound to before it; the agent no longer has it)
}

// A message the person sent while the agent worked, not taken in yet.
export interface ConvoQueued {
  text: string
  ts?: string
  from?: string // (another session's agent, where it is not the person's own)
  note?: boolean // (a notice, not anyone's message)
}

// Work the agent set going and left to itself: a command in the
// background, a subagent on its own.
export interface ConvoRunning {
  id: string
  what: string
  kind: string // 'cmd' | 'agent'
  ts?: string
  note?: string // (the latest it has said of itself)
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
  effort?: string // how hard it is asked to think: low, medium, high, xhigh, max
  branch?: string
  context?: number // tokens the latest request sent
  window?: number // the model's context window, where it is known: as the log says it (Codex's), or of the model (Claude's)
  window_by?: 'log' | 'model' | 'request' // how: said by the log, the model's own, the 1M its requests asked for
  cost?: ConvoCost // the latest of what the session used, as the log read said it (convo.Cost)
  quota?: ConvoQuota // the subscription's use, as the log last said it (Codex's; convo.Quota)
}

// A subscription's use of its 5-hour and weekly windows (convo.Quota).
export interface ConvoQuotaWindow {
  used: number // percent
  resets_at: number // unix seconds
}
export interface ConvoQuota {
  updated_at?: number // unix seconds
  five_hour?: ConvoQuotaWindow
  weekly?: ConvoQuotaWindow
}

// What a session used so far, as Claude Code's log last said it (running
// totals; usd its own estimate at list prices).
export interface ConvoCost {
  usd: number
  unknown?: boolean // some model's price not known: usd short of it
  added?: number
  removed?: number
  api_ms?: number
  at?: number // about when (unix ms)
  models?: { model: string; in?: number; out?: number; cache_read?: number; cache_write?: number; usd?: number }[]
}

// What the entries are of (internal/web/conversation.go convoBind).
export interface ConvoBind {
  pane: string
  pid: number
  start: number
  session: string
  agent?: string // "claude", "codex" or "opencode": whose process it is
  title?: string
}

interface Answer {
  available: boolean
  reason?: string
  bind?: ConvoBind
  agent_status?: 'busy' | 'idle' | 'waiting'
  waiting_for?: string // (with waiting: "permission prompt", "input needed", "sandbox request", "worker request", "dialog open", "other")
  quiet_ms?: number // (how long since its log last grew, by the gateway's clock)
  title?: string
  tasks?: ConvoTask[]
  meta?: ConvoMeta
  queued?: ConvoQueued[]
  running?: ConvoRunning[]
  default_changed?: boolean // (a model switch here left the default of new sessions changed)
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
  reason?: string // with 'none': no_agent, no_log, not_told, unsupported, no_reader
  entries: ConvoEntry[] // (the same array while nothing in it changed)
  older: boolean // the conversation began before the first entry
  olderCut: boolean // and what is before cannot be read in as well
  loadingOlder: boolean // the part before is being read in (see loadOlder)
  // Reads in the part of the log before the first entry (its entries come
  // first, with lower seqs); while one is on its way, nothing more.
  loadOlder: () => void
  refresh: () => void // read now (a message sent: what it brings, at once)
  gen: number
  bind?: ConvoBind
  agentStatus?: 'busy' | 'idle' | 'waiting' // (tmux) as Claude Code's file for the process says
  waitingFor?: string // (with waiting) what for
  // When its log last grew (ms, this page's clock: told as how long since,
  // so no clocks are compared); kept while it moves by less than a few
  // seconds, so a poll that brings nothing new changes nothing.
  quietAt?: number
  title?: string // the session's own title (Claude Code's, or the person's)
  tasks?: ConvoTask[] // the agent's task list (the same array while it did not change)
  meta?: ConvoMeta // (the same object while it did not change)
  queued?: ConvoQueued[] // (likewise)
  running?: ConvoRunning[] // (likewise)
  defaultChanged?: boolean // (a model switch here left the default of new sessions changed, lately)
  // Reading has failed since okAt (ms): what shows may be out of date.
  failing: boolean
  okAt: number
  changedAt: number // (ms) when an entry was last added or changed
  of?: string // (the session and pane it is of: see kept)
}

const noop = () => {}
const initial = (of?: string): Convo => ({ status: 'loading', entries: [], older: false, olderCut: false, loadingOlder: false, loadOlder: noop, refresh: noop, gen: 0, failing: false, okAt: 0, changedAt: Date.now(), of })

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

// prefetchConversation reads a pane's conversation into what is kept, as a
// view of it reads it first, so that a view of it opened next shows it at
// once (and goes on from there). Not one kept already, nor where a view
// read it meanwhile (its own is the newer), nor across a change of account.
// True when it was read.
export async function prefetchConversation(sessionId: string, pane: string | undefined, signal?: AbortSignal): Promise<boolean> {
  const key = keyOf(sessionId, pane)
  if (kept.has(key)) return false
  const account = authGen()
  const q = new URLSearchParams({ gen: '0', rev: '0' })
  if (pane) q.set('pane', pane)
  const a = await api<Answer>('GET', `/api/sessions/${sessionId}/conversation?${q}`, undefined, { signal, timeoutMs: 25000 })
  if (kept.has(key) || authGen() !== account) return false
  if (!a.available) {
    remember(key, a.gen, 0, new Map(), a)
    return true
  }
  const entries = a.entries.slice(-MAX)
  remember(key, a.gen, a.rev, new Map(entries.map((e) => [e.seq, e])), a)
  return true
}

// (Another account signed in, or none: nothing of the one before stays.)
export function forgetConversations() {
  kept.clear()
}

const sameBind = (a?: ConvoBind, b?: ConvoBind) =>
  a === b || (!!a && !!b && a.pane === b.pane && a.pid === b.pid && a.start === b.start && a.session === b.session && a.title === b.title)

const sameMeta = (a?: ConvoMeta, b?: ConvoMeta) =>
  a === b || (!!a && !!b && a.model === b.model && a.mode === b.mode && a.effort === b.effort && a.branch === b.branch && a.context === b.context && a.window === b.window && a.window_by === b.window_by && JSON.stringify(a.cost) === JSON.stringify(b.cost))

const POLL = 2000
const ACTIVE = 1000 // (after an answer that brought something: more is likely coming)
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
  const pokeNow = useRef<() => void>(() => {})

  useEffect(() => {
    if (!enabled) return
    let stopped = false
    let timer: ReturnType<typeof setTimeout> | undefined
    let ctl: AbortController | null = null
    let gen = 0
    let rev = 0
    let byseq = new Map<number, ConvoEntry>()
    let again = false // (asked for while one was on its way: once it is answered)
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
            c.of === key && c.status === 'none' && c.reason === a.reason && c.gen === a.gen && sameBind(c.bind, a.bind) && !c.failing && c.defaultChanged === a.default_changed
              ? c
              : { ...initial(key), status: 'none', reason: a.reason, gen: a.gen, bind: a.bind, okAt: now, changedAt: now, defaultChanged: a.default_changed },
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
            const running = a.running?.length ? (JSON.stringify(a.running) === JSON.stringify(c.running) ? c.running : a.running) : undefined
            const told = a.quiet_ms !== undefined ? now - a.quiet_ms : undefined
            const quietAt = told !== undefined && c.quietAt !== undefined && Math.abs(told - c.quietAt) < 3000 ? c.quietAt : told
            if (!entries && c.of === key && c.status === 'ready' && c.quietAt === quietAt && c.gen === a.gen && c.older === older && c.olderCut === olderCut && c.loadingOlder === loadingOlder && sameBind(c.bind, a.bind) && c.agentStatus === a.agent_status && c.waitingFor === a.waiting_for && c.title === a.title && c.tasks === tasks && c.meta === meta && c.queued === queued && c.running === running && !c.failing && c.defaultChanged === a.default_changed) return c
            return { ...initial(key), status: 'ready', entries: entries ?? c.entries, older, olderCut, loadingOlder, gen: a.gen, bind: a.bind, agentStatus: a.agent_status, waitingFor: a.waiting_for, quietAt, title: a.title || undefined, tasks, meta, queued, running, failing: false, okAt: now, changedAt: entries ? now : c.changedAt, of: key, defaultChanged: a.default_changed }
          })
          remember(key, gen, rev, byseq, a)
          if (a.entries.length > 0 && !fresh) next = ACTIVE
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
      if (again) (again = false), (next = 0)
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
          setConvo({ ...initial(key), status: 'ready', entries, older, olderCut: older && (!!k.older_cut || entries.length === MAX), gen, bind: k.bind, agentStatus: k.agent_status, waitingFor: k.waiting_for, title: k.title || undefined, tasks: k.tasks?.length ? k.tasks : undefined, meta: k.meta, queued: k.queued?.length ? k.queued : undefined, running: k.running?.length ? k.running : undefined, failing: false, okAt: now, changedAt: now, of: key })
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
    // Read now: at once, or as soon as what is on its way is answered.
    pokeNow.current = () => {
      if (stopped) return
      if (ctl) return void (again = true)
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
      pokeNow.current = () => {}
    }
  }, [sessionId, enabled])

  // Another pane in front: read at once rather than at the next tick.
  // (Not on the first: the reading begun above is for it already.)
  const paneSeen = useRef(false)
  useEffect(() => {
    if (!paneSeen.current) return void (paneSeen.current = true)
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
  const refresh = useCallback(() => pokeNow.current(), [])
  return useMemo(() => ({ ...convo, loadOlder, refresh }), [convo, loadOlder, refresh])
}

// Whether the agent is asking the person something (AskUserQuestion, not
// come back): the last thing it did (notes may have come since).
// answerable: what Claude Code waits for (its session file's waitingFor)
// is answered with the card: a permission, a question, a request of its
// sandbox or of a worker's; not a dialog of its own (a picker), nor one
// not known.
export const answerable = (w?: string) => w === 'permission prompt' || w === 'input needed' || w === 'sandbox request' || w === 'worker request'

export function asking(entries: ConvoEntry[]): boolean {
  for (let i = entries.length - 1; i >= 0; i--) {
    const e = entries[i]
    if (e.role === 'note' || e.gone) continue
    const p = e.role === 'assistant' ? e.parts[e.parts.length - 1] : undefined
    // (Claude Code's question; Codex's request for the person's input.)
    return !!p?.tool && (p.tool.name === 'AskUserQuestion' || p.tool.name.startsWith('request_user_input')) && !p.tool.done
  }
  return false
}
