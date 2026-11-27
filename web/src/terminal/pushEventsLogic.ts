// Pure, DOM-independent push event client & polling policy helpers for the
// browser <-> gateway /api/events/ws hint channel. Kept free of browser/React
// globals so Node test scripts can exercise every state transition directly.

export type PushServerMsg =
  | { t: 'convo'; session: string; pane: string }
  | { t: 'agents'; host: number }
  | { t: 'cover'; hosts: number[] }
  | { t: 'pong' }

export type ConvoRequestSource = 'push' | 'poll' | 'other'

export const PUSH_RECONNECT_BACKOFFS = [1000, 2000, 5000, 10000, 30000] as const

export const CONVO_POLL_DEFAULT = 2000
export const CONVO_POLL_ACTIVE = 1000
export const CONVO_POLL_COVERED = 10000
export const CONVO_POLL_RETRY = 5000
export const CONVO_POLL_MORE = 300

export const AGENTS_VISIBLE_DEFAULT = 15000
export const AGENTS_HIDDEN_DEFAULT = 30000
export const AGENTS_COVERED_POLL = 60000
export const AGENTS_COALESCE_MS = 300

export const PUSH_PING_EVERY = 30000

export function nextReconnectDelay(attempt: number): number {
  const idx = Math.max(0, Math.min(attempt, PUSH_RECONNECT_BACKOFFS.length - 1))
  return PUSH_RECONNECT_BACKOFFS[idx]
}

export function parsePushMessage(raw: string): PushServerMsg | null {
  try {
    const v = JSON.parse(raw) as Record<string, unknown>
    if (!v || typeof v !== 'object' || typeof v.t !== 'string') return null
    switch (v.t) {
      case 'convo':
        if (typeof v.session === 'string' && typeof v.pane === 'string') {
          return { t: 'convo', session: v.session, pane: v.pane }
        }
        return null
      case 'agents':
        if (typeof v.host === 'number' && Number.isFinite(v.host) && v.host > 0) {
          return { t: 'agents', host: v.host }
        }
        return null
      case 'cover':
        if (Array.isArray(v.hosts)) {
          const hosts = v.hosts
            .filter((x): x is number => typeof x === 'number' && Number.isFinite(x) && x > 0)
            .sort((a, b) => a - b)
          return { t: 'cover', hosts }
        }
        return null
      case 'pong':
        return { t: 'pong' }
      default:
        return null
    }
  } catch {
    return null
  }
}

export function matchesConvoHint(
  sessionId: string,
  pane: string | undefined,
  hintSession: string,
  hintPane: string,
): boolean {
  if (hintSession !== sessionId) return false
  if (!pane) return true // tmux session: active pane
  if (!hintPane) return true
  return hintPane === pane
}

export function convoNextPollDelay(opts: {
  covered: boolean
  active: boolean
  more: boolean
}): number {
  if (opts.more) return CONVO_POLL_MORE
  if (opts.covered) return CONVO_POLL_COVERED
  return opts.active ? CONVO_POLL_ACTIVE : CONVO_POLL_DEFAULT
}

export function agentsNextPollDelay(opts: {
  defaultEvery: number
  hidden: boolean
  allCovered: boolean
}): number {
  if (opts.hidden) return AGENTS_HIDDEN_DEFAULT
  if (opts.allCovered) return AGENTS_COVERED_POLL
  return opts.defaultEvery
}

export interface SocketLike {
  readyState: number
  send(data: string): void
  close(code?: number, reason?: string): void
  onopen: ((ev?: unknown) => void) | null
  onmessage: ((ev: { data: unknown }) => void) | null
  onerror: ((ev?: unknown) => void) | null
  onclose: ((ev?: unknown) => void) | null
}

export const SOCKET_OPEN = 1

export interface TimerAPI {
  setTimeout: (fn: () => void, ms: number) => ReturnType<typeof setTimeout>
  clearTimeout: (id: ReturnType<typeof setTimeout> | undefined) => void
}

const defaultTimers: TimerAPI = {
  setTimeout: (fn, ms) => globalThis.setTimeout(fn, ms),
  clearTimeout: (id) => {
    if (id !== undefined) globalThis.clearTimeout(id)
  },
}

export interface PushEventClientOptions {
  createSocket: () => SocketLike
  isHidden?: () => boolean
  timers?: TimerAPI
  pingEveryMs?: number
}

function sameSortedNumbers(a: number[], b: number[]): boolean {
  if (a.length !== b.length) return false
  for (let i = 0; i < a.length; i++) if (a[i] !== b[i]) return false
  return true
}

function sameNumberSet(a: Set<number>, b: number[]): boolean {
  if (a.size !== b.length) return false
  for (const x of b) if (!a.has(x)) return false
  return true
}

export class PushEventClient {
  private ws: SocketLike | null = null
  private gen = 0
  private active = false
  private connected = false
  private attempt = 0
  private reconnectTimer: ReturnType<typeof setTimeout> | undefined
  private pingTimer: ReturnType<typeof setTimeout> | undefined
  private coveredHosts = new Set<number>()
  private watchSources = new Map<symbol, number[]>()
  private watchedUnion: number[] = []
  private convoListeners = new Set<(session: string, pane: string) => void>()
  private agentsListeners = new Set<(host: number) => void>()
  private stateListeners = new Set<() => void>()
  private readonly createSocket: () => SocketLike
  private readonly isHidden: () => boolean
  private readonly timers: TimerAPI
  private readonly pingEveryMs: number

  constructor(opts: PushEventClientOptions) {
    this.createSocket = opts.createSocket
    this.isHidden = opts.isHidden ?? (() => false)
    this.timers = opts.timers ?? defaultTimers
    this.pingEveryMs = opts.pingEveryMs ?? PUSH_PING_EVERY
  }

  isConnected(): boolean {
    return this.connected
  }

  coveredList(): number[] {
    return [...this.coveredHosts].sort((a, b) => a - b)
  }

  isHostCovered(hostId: number | undefined): boolean {
    return this.connected && hostId !== undefined && this.coveredHosts.has(hostId)
  }

  areAllHostsCovered(hostIds: number[]): boolean {
    if (!this.connected || hostIds.length === 0) return false
    for (const id of hostIds) {
      if (!this.coveredHosts.has(id)) return false
    }
    return true
  }

  setActive(active: boolean) {
    if (this.active === active) return
    this.active = active
    if (active) {
      this.attempt = 0
      this.open()
    } else {
      this.timers.clearTimeout(this.reconnectTimer)
      this.reconnectTimer = undefined
      this.dropSocket(true)
    }
  }

  reconnectNow() {
    if (!this.active || this.ws) return
    this.timers.clearTimeout(this.reconnectTimer)
    this.reconnectTimer = undefined
    this.attempt = 0
    this.open()
  }

  watchHosts(source: symbol, hosts: number[]): () => void {
    const clean = [...new Set(hosts.filter((h) => Number.isFinite(h) && h > 0))].sort((a, b) => a - b)
    if (clean.length === 0) this.watchSources.delete(source)
    else this.watchSources.set(source, clean)
    this.syncWatch()
    return () => {
      if (this.watchSources.delete(source)) this.syncWatch()
    }
  }

  onConvo(fn: (session: string, pane: string) => void): () => void {
    this.convoListeners.add(fn)
    return () => void this.convoListeners.delete(fn)
  }

  onAgents(fn: (host: number) => void): () => void {
    this.agentsListeners.add(fn)
    return () => void this.agentsListeners.delete(fn)
  }

  onStateChange(fn: () => void): () => void {
    this.stateListeners.add(fn)
    return () => void this.stateListeners.delete(fn)
  }

  private notifyState() {
    this.stateListeners.forEach((fn) => fn())
  }

  private syncWatch() {
    const set = new Set<number>()
    for (const list of this.watchSources.values()) {
      for (const id of list) set.add(id)
    }
    const next = [...set].sort((a, b) => a - b)
    if (sameSortedNumbers(this.watchedUnion, next)) return
    this.watchedUnion = next
    this.sendWatch()
  }

  private sendWatch() {
    if (this.ws && this.ws.readyState === SOCKET_OPEN) {
      try {
        this.ws.send(JSON.stringify({ t: 'watch', hosts: this.watchedUnion }))
      } catch {
        // ignore send errors; close handler will reconnect
      }
    }
  }

  private schedulePing(g: number) {
    this.timers.clearTimeout(this.pingTimer)
    if (this.pingEveryMs <= 0) return
    this.pingTimer = this.timers.setTimeout(() => {
      if (g !== this.gen || !this.ws || this.ws.readyState !== SOCKET_OPEN) return
      try {
        this.ws.send(JSON.stringify({ t: 'ping' }))
      } catch {
        // ignore
      }
      this.schedulePing(g)
    }, this.pingEveryMs)
  }

  private open() {
    if (!this.active || this.ws) return
    const g = ++this.gen
    let ws: SocketLike
    try {
      ws = this.createSocket()
    } catch {
      this.scheduleReconnect()
      return
    }
    this.ws = ws
    ws.onopen = () => {
      if (g !== this.gen) return
      this.attempt = 0
      this.connected = true
      if (this.watchedUnion.length > 0) this.sendWatch()
      this.schedulePing(g)
      this.notifyState()
    }
    ws.onmessage = (ev) => {
      if (g !== this.gen || typeof ev.data !== 'string') return
      const msg = parsePushMessage(ev.data)
      if (!msg) return
      switch (msg.t) {
        case 'cover':
          if (!sameNumberSet(this.coveredHosts, msg.hosts)) {
            this.coveredHosts = new Set(msg.hosts)
            this.notifyState()
          }
          break
        case 'convo':
          if (!this.isHidden()) {
            this.convoListeners.forEach((fn) => fn(msg.session, msg.pane))
          }
          break
        case 'agents':
          if (!this.isHidden()) {
            this.agentsListeners.forEach((fn) => fn(msg.host))
          }
          break
        case 'pong':
          break
      }
    }
    const onDown = () => {
      if (g !== this.gen) return
      this.dropSocket(false)
      if (this.active) this.scheduleReconnect()
    }
    ws.onclose = onDown
    ws.onerror = () => {
      if (g !== this.gen) return
      try {
        ws.close()
      } catch {
        // ignore
      }
    }
  }

  private dropSocket(intentional: boolean) {
    this.gen++
    this.timers.clearTimeout(this.pingTimer)
    this.pingTimer = undefined
    const ws = this.ws
    this.ws = null
    if (ws) {
      ws.onopen = null
      ws.onmessage = null
      ws.onerror = null
      ws.onclose = null
      if (intentional) {
        try {
          ws.close(1000)
        } catch {
          // ignore
        }
      }
    }
    const changed = this.connected || this.coveredHosts.size > 0
    this.connected = false
    this.coveredHosts.clear()
    if (changed) this.notifyState()
  }

  private scheduleReconnect() {
    if (!this.active || this.reconnectTimer !== undefined) return
    const delay = nextReconnectDelay(this.attempt++)
    this.reconnectTimer = this.timers.setTimeout(() => {
      this.reconnectTimer = undefined
      this.open()
    }, delay)
  }
}

export interface AgentsCoalescer {
  trigger: () => void
  cancel: () => void
}

export function createAgentsHintCoalescer(opts: {
  delayMs?: number
  isHidden: () => boolean
  onFlush: () => void
  timers?: TimerAPI
}): AgentsCoalescer {
  const delay = opts.delayMs ?? AGENTS_COALESCE_MS
  const timers = opts.timers ?? defaultTimers
  let timer: ReturnType<typeof setTimeout> | undefined
  return {
    trigger() {
      if (opts.isHidden()) return
      if (timer !== undefined) return
      timer = timers.setTimeout(() => {
        timer = undefined
        if (!opts.isHidden()) opts.onFlush()
      }, delay)
    },
    cancel() {
      timers.clearTimeout(timer)
      timer = undefined
    },
  }
}

// Serialized push+poll scheduler for conversation fetching: never allows
// concurrent requests; if a push hint arrives while a fetch is in flight,
// triggers an immediate follow-up push fetch as soon as the in-flight request
// finishes. Switching out of coverage immediately shortens a pending 10s poll
// back to the default interval.
export interface ConvoScheduler {
  start: () => void
  onPushHint: () => void
  onCoverageChange: () => void
  onVisible: () => void
  refreshNow: () => void
  stop: () => void
}

export function createConvoScheduler(opts: {
  fetchOnce: (src: ConvoRequestSource) => Promise<{ active: boolean; more: boolean; error?: boolean }>
  isCovered: () => boolean
  isHidden: () => boolean
  timers?: TimerAPI
}): ConvoScheduler {
  const timers = opts.timers ?? defaultTimers
  let stopped = false
  let inFlight = false
  let again = false
  let againSrc: ConvoRequestSource = 'other'
  let timer: ReturnType<typeof setTimeout> | undefined
  let scheduledMs: number | null = null
  let lastActive = false
  let lastMore = false
  let lastError = false

  const clearPoll = () => {
    timers.clearTimeout(timer)
    timer = undefined
    scheduledMs = null
  }

  const schedulePoll = (ms: number) => {
    if (stopped || opts.isHidden()) return
    clearPoll()
    scheduledMs = ms
    timer = timers.setTimeout(() => {
      timer = undefined
      scheduledMs = null
      void run('poll')
    }, ms)
  }

  const run = async (src: ConvoRequestSource) => {
    clearPoll()
    if (stopped || opts.isHidden()) return
    if (inFlight) {
      again = true
      if (src === 'push' || againSrc !== 'push') againSrc = src
      return
    }
    inFlight = true
    let next = CONVO_POLL_DEFAULT
    try {
      const res = await opts.fetchOnce(src)
      if (stopped) return
      lastError = !!res.error
      lastActive = !!res.active
      lastMore = !!res.more
      if (res.error) next = CONVO_POLL_RETRY
      else next = convoNextPollDelay({ covered: opts.isCovered(), active: lastActive, more: lastMore })
    } catch {
      if (stopped) return
      lastError = true
      next = CONVO_POLL_RETRY
    } finally {
      inFlight = false
    }
    if (stopped || opts.isHidden()) return
    if (again) {
      const nextSrc = againSrc
      again = false
      againSrc = 'other'
      void run(nextSrc)
      return
    }
    schedulePoll(next)
  }

  return {
    start() {
      stopped = false
      void run('other')
    },
    onPushHint() {
      if (stopped || opts.isHidden()) return
      if (inFlight) {
        again = true
        againSrc = 'push'
        return
      }
      clearPoll()
      void run('push')
    },
    onCoverageChange() {
      if (stopped || opts.isHidden() || inFlight || timer === undefined || lastError) return
      const want = convoNextPollDelay({ covered: opts.isCovered(), active: lastActive, more: lastMore })
      if (scheduledMs !== null && want !== scheduledMs) {
        schedulePoll(want)
      }
    },
    onVisible() {
      if (stopped || opts.isHidden()) return
      if (!inFlight && timer === undefined) void run('other')
    },
    refreshNow() {
      if (stopped) return
      if (inFlight) {
        again = true
        if (againSrc !== 'push') againSrc = 'other'
        return
      }
      clearPoll()
      void run('other')
    },
    stop() {
      stopped = true
      clearPoll()
    },
  }
}
