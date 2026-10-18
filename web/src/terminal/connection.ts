// TermConnection keeps a browser attached to a gateway session across drops.
//
// The gateway numbers every output byte. We remember how far we have taken in
// and ask to resume from there; the gateway either continues exactly or tells
// us to reset and replays what it still has. Phones suspend background tabs
// within seconds, so reconnecting is the normal path, not an edge case.

export type Status = 'connecting' | 'open' | 'reconnecting' | 'offline' | 'ended' | 'gone'

export interface EndInfo {
  state: 'exited' | 'lost' | 'closed'
  code: number
  reason: string
}

export interface Handlers {
  onReset(): boolean // false: could not be taken in (terminal overloaded)
  // Return false if the data could not be taken in (terminal overloaded); the
  // connection then resumes from the start of this chunk.
  onData(data: Uint8Array): boolean
  onStatus(s: Status): void
  onEnd(info: EndInfo): void
  onLatency(ms: number): void
}

export type SendResult = 'sent' | 'queued' | 'dropped'

const PING_EVERY = 15_000
const PONG_TIMEOUT = 8_000
const QUEUE_MAX_BYTES = 8 << 10
const QUEUE_MAX_AGE = 10_000
const FRAME_MAX = 256 << 10 // the gateway accepts messages up to 1 MiB; stay well below

export class TermConnection {
  private ws: WebSocket | null = null
  private gen = 0 // increases with every socket; callbacks from older sockets are ignored
  private offset: number | null = null // next byte we expect; null until the first hello
  private attempt = 0
  private timer: number | undefined
  private pingTimer: number | undefined
  private pongDeadline: number | undefined
  private closed = false
  private ended = false
  private queue: { at: number; data: Uint8Array }[] = []
  private size: { cols: number; rows: number } | null = null
  status: Status = 'connecting'

  constructor(private id: string, private h: Handlers) {
    window.addEventListener('online', this.onOnline)
    window.addEventListener('offline', this.onOffline)
    document.addEventListener('visibilitychange', this.onVisible)
    this.open()
  }

  dispose() {
    this.closed = true
    window.removeEventListener('online', this.onOnline)
    window.removeEventListener('offline', this.onOffline)
    document.removeEventListener('visibilitychange', this.onVisible)
    clearTimeout(this.timer)
    this.drop()
  }

  isOpen() {
    return this.status === 'open' && this.ws?.readyState === WebSocket.OPEN
  }

  // send delivers keyboard input. While reconnecting, a little input is held
  // for a few seconds; anything larger or older is dropped instead of being
  // replayed long after the user stopped expecting it.
  send(data: string | Uint8Array): SendResult {
    const bytes = typeof data === 'string' ? new TextEncoder().encode(data) : data
    if (this.isOpen()) {
      for (let i = 0; i < bytes.length; i += FRAME_MAX) this.ws!.send(bytes.subarray(i, i + FRAME_MAX))
      return 'sent'
    }
    const now = Date.now()
    this.queue = this.queue.filter((q) => now - q.at < QUEUE_MAX_AGE)
    const queued = this.queue.reduce((n, q) => n + q.data.length, 0)
    if (queued + bytes.length > QUEUE_MAX_BYTES) return 'dropped'
    this.queue.push({ at: now, data: bytes })
    return 'queued'
  }

  resize(cols: number, rows: number) {
    this.size = { cols, rows }
    if (this.isOpen()) this.ws!.send(JSON.stringify({ t: 'resize', cols, rows }))
  }

  private setStatus(s: Status) {
    if (this.status === s) return
    this.status = s
    this.h.onStatus(s)
  }

  // drop closes the current socket and forgets it, so its late events are ignored.
  private drop() {
    this.stopPing()
    const ws = this.ws
    this.ws = null
    this.gen++
    if (ws && ws.readyState !== WebSocket.CLOSED) {
      ws.onmessage = ws.onclose = ws.onerror = null
      try {
        ws.close()
      } catch {
        /* ignore */
      }
    }
  }

  private open() {
    if (this.closed || this.ended) return
    clearTimeout(this.timer)
    this.drop()
    const gen = this.gen
    const proto = location.protocol === 'https:' ? 'wss:' : 'ws:'
    const q = this.offset !== null ? `?offset=${this.offset}` : ''
    const ws = new WebSocket(`${proto}//${location.host}/api/sessions/${this.id}/ws${q}`)
    ws.binaryType = 'arraybuffer'
    this.ws = ws
    let gotHello = false

    ws.onmessage = (e) => {
      if (gen !== this.gen) return
      if (typeof e.data === 'string') {
        const m = JSON.parse(e.data)
        if (m.t === 'hello') {
          gotHello = true
          this.attempt = 0
          if ((m.reset || this.offset === null) && !this.h.onReset()) {
            this.drop()
            this.scheduleReconnect(1000)
            return
          }
          this.offset = m.offset
          this.setStatus('open')
          if (this.size) ws.send(JSON.stringify({ t: 'resize', ...this.size }))
          this.flushQueue(ws)
          this.startPing(ws, gen)
        } else if (m.t === 'state') {
          this.ended = true
          this.queue = []
          this.drop()
          this.setStatus('ended')
          this.h.onEnd({ state: m.state, code: m.code, reason: m.reason })
        } else if (m.t === 'pong') {
          clearTimeout(this.pongDeadline)
          this.h.onLatency(Math.max(0, Date.now() - m.ts))
        }
        return
      }
      if (this.offset === null) return
      const buf = e.data as ArrayBuffer
      const at = Number(new DataView(buf).getBigUint64(0))
      let data = new Uint8Array(buf, 8)
      let start = at
      // Skip any overlap with what we already took in.
      if (at < this.offset) {
        const skip = this.offset - at
        if (skip >= data.length) return
        data = data.subarray(skip)
        start = this.offset
      }
      if (!this.h.onData(data)) {
        // Not taken in: come back for these bytes once the terminal has caught up.
        this.offset = start
        this.drop()
        this.scheduleReconnect(1000)
        return
      }
      this.offset = start + data.length
    }

    ws.onclose = () => {
      if (gen !== this.gen) return
      this.stopPing()
      this.ws = null
      if (this.closed || this.ended) return
      // A socket refused before hello usually means the session is gone (404).
      if (!gotHello && this.attempt >= 1) void this.checkGone()
      this.scheduleReconnect()
    }
  }

  private async checkGone() {
    try {
      const r = await fetch('/api/sessions', { credentials: 'same-origin' })
      if (r.status === 401) {
        this.ended = true
        clearTimeout(this.timer)
        location.href = '/login?next=' + encodeURIComponent(location.pathname)
        return
      }
      const list: { id: string }[] = await r.json()
      if (!list.some((s) => s.id === this.id)) {
        this.ended = true
        clearTimeout(this.timer)
        this.drop()
        this.setStatus('gone')
      }
    } catch {
      /* network still down; keep trying */
    }
  }

  private scheduleReconnect(minDelay = 0) {
    if (this.closed || this.ended) return
    if (!navigator.onLine) {
      this.setStatus('offline')
      return
    }
    this.setStatus(this.offset === null ? 'connecting' : 'reconnecting')
    const base = Math.min(15_000, 400 * 2 ** this.attempt)
    const delay = Math.max(minDelay, base / 2 + Math.random() * (base / 2))
    this.attempt++
    clearTimeout(this.timer)
    this.timer = window.setTimeout(() => this.open(), delay)
  }

  private flushQueue(ws: WebSocket) {
    const now = Date.now()
    for (const q of this.queue) if (now - q.at < QUEUE_MAX_AGE) ws.send(q.data)
    this.queue = []
  }

  private startPing(ws: WebSocket, gen: number) {
    this.stopPing()
    const ping = () => {
      if (gen !== this.gen || ws.readyState !== WebSocket.OPEN) return
      ws.send(JSON.stringify({ t: 'ping', ts: Date.now() }))
      clearTimeout(this.pongDeadline)
      this.pongDeadline = window.setTimeout(() => {
        if (gen === this.gen) ws.close()
      }, PONG_TIMEOUT)
    }
    ping()
    this.pingTimer = window.setInterval(ping, PING_EVERY)
  }

  private stopPing() {
    clearInterval(this.pingTimer)
    clearTimeout(this.pongDeadline)
  }

  // probe checks a socket that looks open but may be dead (after the tab was in
  // the background or the network flapped): no pong soon means reconnect.
  private probe(timeout: number) {
    const ws = this.ws
    if (!ws || ws.readyState !== WebSocket.OPEN) return false
    const gen = this.gen
    ws.send(JSON.stringify({ t: 'ping', ts: Date.now() }))
    clearTimeout(this.pongDeadline)
    this.pongDeadline = window.setTimeout(() => {
      if (gen === this.gen) ws.close()
    }, timeout)
    return true
  }

  private onVisible = () => {
    if (document.visibilityState !== 'visible' || this.closed || this.ended) return
    if (this.probe(2500)) return
    if (!this.ws || this.ws.readyState !== WebSocket.CONNECTING) {
      this.attempt = 0
      this.open()
    }
  }

  private onOnline = () => {
    if (this.closed || this.ended) return
    if (this.probe(2500)) {
      // The socket survived the outage; show it as open while the probe runs.
      if (this.offset !== null) this.setStatus('open')
      return
    }
    if (this.ws && this.ws.readyState === WebSocket.CONNECTING) return
    this.attempt = 0
    this.open()
  }

  private onOffline = () => {
    if (this.closed || this.ended) return
    this.setStatus('offline')
  }
}
