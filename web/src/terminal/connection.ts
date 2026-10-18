// TermConnection keeps a browser attached to a gateway session across drops.
//
// The gateway numbers every output byte. We remember how far we have written
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
  onReset(): void
  onData(data: Uint8Array): void
  onStatus(s: Status): void
  onEnd(info: EndInfo): void
  onLatency(ms: number): void
}

const PING_EVERY = 15_000
const PONG_TIMEOUT = 8_000
const QUEUE_MAX_BYTES = 8 << 10
const QUEUE_MAX_AGE = 10_000

export class TermConnection {
  private ws: WebSocket | null = null
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
    this.stopPing()
    this.ws?.close()
  }

  send(data: string | Uint8Array) {
    const bytes = typeof data === 'string' ? new TextEncoder().encode(data) : data
    if (this.ws?.readyState === WebSocket.OPEN && this.status === 'open') {
      this.ws.send(bytes)
      return
    }
    // Hold a little input across a brief drop; older or larger input is dropped
    // rather than replayed long after the user stopped expecting it.
    const now = Date.now()
    this.queue = this.queue.filter((q) => now - q.at < QUEUE_MAX_AGE)
    const queued = this.queue.reduce((n, q) => n + q.data.length, 0)
    if (queued + bytes.length <= QUEUE_MAX_BYTES) this.queue.push({ at: now, data: bytes })
  }

  resize(cols: number, rows: number) {
    this.size = { cols, rows }
    if (this.ws?.readyState === WebSocket.OPEN) this.ws.send(JSON.stringify({ t: 'resize', cols, rows }))
  }

  private setStatus(s: Status) {
    if (this.status === s) return
    this.status = s
    this.h.onStatus(s)
  }

  private open() {
    if (this.closed || this.ended) return
    clearTimeout(this.timer)
    const proto = location.protocol === 'https:' ? 'wss:' : 'ws:'
    const q = this.offset !== null ? `?offset=${this.offset}` : ''
    const ws = new WebSocket(`${proto}//${location.host}/api/sessions/${this.id}/ws${q}`)
    ws.binaryType = 'arraybuffer'
    this.ws = ws
    let gotHello = false

    ws.onmessage = (e) => {
      if (ws !== this.ws) return
      if (typeof e.data === 'string') {
        const m = JSON.parse(e.data)
        if (m.t === 'hello') {
          gotHello = true
          this.attempt = 0
          if (m.reset || this.offset === null) this.h.onReset()
          this.offset = m.offset
          this.setStatus('open')
          if (this.size) ws.send(JSON.stringify({ t: 'resize', ...this.size }))
          this.flushQueue()
          this.startPing()
        } else if (m.t === 'state') {
          this.ended = true
          this.setStatus('ended')
          this.h.onEnd({ state: m.state, code: m.code, reason: m.reason })
        } else if (m.t === 'pong') {
          clearTimeout(this.pongDeadline)
          this.h.onLatency(Math.max(0, Date.now() - m.ts))
        }
        return
      }
      const buf = e.data as ArrayBuffer
      const view = new DataView(buf)
      const at = Number(view.getBigUint64(0))
      let data = new Uint8Array(buf, 8)
      if (this.offset === null) return
      // Skip any overlap with what we already wrote.
      if (at < this.offset) {
        const skip = this.offset - at
        if (skip >= data.length) return
        data = data.subarray(skip)
      }
      this.offset = Math.max(this.offset, at) + data.length
      this.h.onData(data)
    }

    ws.onclose = (e) => {
      if (ws !== this.ws) return
      this.stopPing()
      if (this.closed || this.ended) return
      // 404 during the handshake shows up as a close before hello: the session is gone.
      if (!gotHello && e.code === 1006 && this.attempt >= 2) {
        void this.checkGone()
      }
      this.scheduleReconnect()
    }
    ws.onerror = () => {
      /* onclose follows */
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
        this.setStatus('gone')
      }
    } catch {
      /* network still down; keep trying */
    }
  }

  private scheduleReconnect() {
    if (!navigator.onLine) {
      this.setStatus('offline')
      return
    }
    this.setStatus(this.offset === null ? 'connecting' : 'reconnecting')
    const base = Math.min(15_000, 400 * 2 ** this.attempt)
    const delay = base / 2 + Math.random() * (base / 2)
    this.attempt++
    clearTimeout(this.timer)
    this.timer = window.setTimeout(() => this.open(), delay)
  }

  private flushQueue() {
    const now = Date.now()
    for (const q of this.queue) if (now - q.at < QUEUE_MAX_AGE) this.ws?.send(q.data)
    this.queue = []
  }

  private startPing() {
    this.stopPing()
    const ping = () => {
      if (this.ws?.readyState !== WebSocket.OPEN) return
      this.ws.send(JSON.stringify({ t: 'ping', ts: Date.now() }))
      clearTimeout(this.pongDeadline)
      this.pongDeadline = window.setTimeout(() => this.ws?.close(), PONG_TIMEOUT)
    }
    ping()
    this.pingTimer = window.setInterval(ping, PING_EVERY)
  }

  private stopPing() {
    clearInterval(this.pingTimer)
    clearTimeout(this.pongDeadline)
  }

  // Coming back to the foreground: the socket may be dead without knowing it.
  private onVisible = () => {
    if (document.visibilityState !== 'visible' || this.closed || this.ended) return
    if (this.ws?.readyState === WebSocket.OPEN) {
      this.ws.send(JSON.stringify({ t: 'ping', ts: Date.now() }))
      clearTimeout(this.pongDeadline)
      this.pongDeadline = window.setTimeout(() => this.ws?.close(), 2500)
    } else {
      this.attempt = 0
      this.open()
    }
  }

  private onOnline = () => {
    if (this.closed || this.ended) return
    this.attempt = 0
    this.open()
  }

  private onOffline = () => {
    if (this.closed || this.ended) return
    this.setStatus('offline')
  }
}
