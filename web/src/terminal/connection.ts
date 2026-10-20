// TermConnection keeps a browser attached to a gateway session across drops.
//
// The gateway numbers every output byte. We remember how far we have taken in
// and ask to resume from there; the gateway either continues exactly or tells
// us to reset and replays what it still has. Phones suspend background tabs
// within seconds, so reconnecting is the normal path, not an edge case.

export type Status = 'connecting' | 'open' | 'reconnecting' | 'offline' | 'ended' | 'gone' | 'full' | 'revoked'

export interface EndInfo {
  state: 'exited' | 'lost' | 'closed'
  code: number
  reason: string
}

export interface Handlers {
  // modes: output to write after clearing, before the backlog (the terminal
  // modes set by output the backlog no longer holds).
  onReset(modes: string): boolean // false: could not be taken in (terminal overloaded)
  // Return false if the data could not be taken in (terminal overloaded); the
  // connection then resumes from the start of this chunk.
  // backlog is true for replayed output sent before the "live" marker.
  onData(data: Uint8Array, backlog: boolean): boolean
  onStatus(s: Status): void
  onEnd(info: EndInfo): void
  onLatency(ms: number): void
  onMeta?(m: any): void // side information such as herdr state
  // The terminal's size, and whether this window set it. Notes older than
  // this window's latest size request are left out (they predate it).
  onSize?(cols: number, rows: number, own: boolean): void
  onLive?(): void // the replayed backlog has been delivered
  onHello?(version: string): void // a new socket was accepted (by a gateway of this version); state from the old one is stale
  onDropped?(): void // input typed while disconnected was discarded (too much, or too old)
}

export type SendResult = 'sent' | 'queued' | 'dropped'

const PING_EVERY = 15_000
const PONG_TIMEOUT = 8_000
const QUEUE_MAX_BYTES = 8 << 10
const QUEUE_MAX_AGE = 10_000
const FRAME_MAX = 256 << 10 // the gateway accepts messages up to 1 MiB; stay well below

// The largest size the gateway accepts; larger windows report this.
export const MAX_COLS = 1000
export const MAX_ROWS = 500
export const clampSize = (cols: number, rows: number) => ({ cols: Math.min(cols, MAX_COLS), rows: Math.min(rows, MAX_ROWS) })

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
  // Input typed while disconnected, with the output position it was typed at.
  private queue: { at: number; data: Uint8Array; offset: number | null }[] = []
  private size: { cols: number; rows: number } | null = null
  private backlog = false
  private everOpen = false
  private full = false
  private sizeSeq = 0 // this socket's size requests so far
  // Takeovers not known to have reached the gateway: one asked for but not
  // sent yet (not connected), and one sent as request number takeSeq on the
  // current socket and not answered yet (the socket may have lost it). Either
  // is sent again on the next socket.
  private takeUnsent = false
  private takeSeq = 0
  // Names this window to the gateway on every connection, so a reconnect
  // is the same viewer (still in charge of the size if it was).
  private viewer = Array.from(crypto.getRandomValues(new Uint8Array(12)), (b) => b.toString(36).padStart(2, '0')).join('')
  opens = 0 // counts accepted sockets; input held elsewhere checks it did not change
  status: Status = 'connecting'

  // watch: a read-only connection of someone the owner let watch. It sends
  // nothing but pings: no input, no sizes, no visibility.
  constructor(private id: string, private h: Handlers, private watch = false) {
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
    if (this.watch) return 'dropped'
    if (this.full) {
      this.h.onDropped?.()
      return 'dropped'
    }
    // Behind input still waiting (the socket is back but not yet confirmed), it waits too.
    if (this.isOpen() && this.queue.length === 0) {
      for (let i = 0; i < bytes.length; i += FRAME_MAX) this.ws!.send(bytes.subarray(i, i + FRAME_MAX))
      return 'sent'
    }
    const now = Date.now()
    const before = this.queue.length
    this.queue = this.queue.filter((q) => now - q.at < QUEUE_MAX_AGE)
    if (this.queue.length < before) this.h.onDropped?.()
    const queued = this.queue.reduce((n, q) => n + q.data.length, 0)
    if (queued + bytes.length > QUEUE_MAX_BYTES) {
      this.h.onDropped?.()
      return 'dropped'
    }
    this.queue.push({ at: now, data: bytes, offset: this.offset })
    return 'queued'
  }

  // sendNow sends at once, or not at all (false) when the connection is not
  // usable now or earlier input still waits; nothing is queued.
  sendNow(data: string): boolean {
    if (this.watch || !this.isOpen() || this.queue.length > 0) return false
    const bytes = new TextEncoder().encode(data)
    for (let i = 0; i < bytes.length; i += FRAME_MAX) this.ws!.send(bytes.subarray(i, i + FRAME_MAX))
    return true
  }

  // retry tries again after the session was full.
  retry() {
    if (!this.full) return
    this.full = false
    this.attempt = 0
    this.open()
  }

  // resize reports this window's size. take: the user acted here, so the
  // terminal is taken over; otherwise (the window changed size) the size only
  // applies while this window is the one in charge of it.
  resize(cols: number, rows: number, take = false) {
    this.size = clampSize(cols, rows)
    if (take && !this.full) this.takeUnsent = true // a session with no room takes no actions
    if (this.isOpen()) this.sendSize(this.ws!, false)
  }

  // sendSize sends this window's size, as a takeover with take or when one
  // is waiting to be sent.
  private sendSize(ws: WebSocket, take: boolean) {
    if (this.watch) return
    take ||= this.takeUnsent
    ws.send(JSON.stringify({ t: 'resize', ...this.size, seq: ++this.sizeSeq, take }))
    if (take) {
      this.takeUnsent = false
      this.takeSeq = this.sizeSeq
    }
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
    if (this.closed || this.ended || this.full) return
    clearTimeout(this.timer)
    this.drop()
    const gen = this.gen
    const proto = location.protocol === 'https:' ? 'wss:' : 'ws:'
    const q = `?viewer=${this.viewer}` + (this.offset !== null ? `&offset=${this.offset}` : '')
    const path = this.watch ? `/api/shared/${this.id}/ws` : `/api/sessions/${this.id}/ws`
    const ws = new WebSocket(`${proto}//${location.host}${path}${q}`)
    ws.binaryType = 'arraybuffer'
    this.ws = ws
    let gotHello = false

    ws.onmessage = (e) => {
      if (gen !== this.gen) return
      if (typeof e.data === 'string') {
        const m = JSON.parse(e.data)
        if (m.t === 'hello') {
          this.opens++
          gotHello = true
          this.attempt = 0
          this.backlog = true
          if ((m.reset || this.offset === null) && !this.h.onReset(typeof m.modes === 'string' ? m.modes : '')) {
            this.drop()
            this.scheduleReconnect(1000)
            return
          }
          this.offset = m.offset
          this.setStatus('open')
          // This window's size: opening the session takes it over; a
          // reconnect only keeps it if this window was in charge (a phone in
          // a pocket must not take the terminal back from the desk).
          const first = !this.everOpen
          this.everOpen = true
          // Numbered per socket; a takeover the old socket may have lost is
          // sent again.
          const resend = this.takeSeq > 0
          this.sizeSeq = 0
          this.takeSeq = 0
          if (this.size) this.sendSize(ws, first || resend)
          this.h.onHello?.(typeof m.version === 'string' ? m.version : '')
          if (!this.watch) ws.send(JSON.stringify({ t: 'visible', v: document.visibilityState === 'visible' }))
          this.startPing(ws, gen)
        } else if (m.t === 'state') {
          this.ended = true
          this.queue = []
          this.drop()
          this.setStatus('ended')
          this.h.onEnd({ state: m.state, code: m.code, reason: m.reason })
        } else if (m.t === 'live') {
          this.backlog = false
          this.flushQueue(ws) // caught up: now it can be told whether the screen changed
          if (this.takeUnsent && this.size) this.sendSize(ws, true)
          this.h.onLive?.()
        } else if (m.t === 'error' && m.code === 'too_many_viewers') {
          // Open in too many windows already: retrying would only be refused again.
          this.full = true
          if (this.queue.length > 0) this.h.onDropped?.()
          this.queue = []
          this.takeUnsent = false
          this.takeSeq = 0
          this.drop()
          this.setStatus('full')
        } else if (m.t === 'herdr') {
          this.h.onMeta?.(m)
        } else if (m.t === 'size') {
          if (this.takeSeq > 0 && (m.ack ?? 0) >= this.takeSeq) this.takeSeq = 0 // the takeover arrived
          if ((m.ack ?? 0) < this.sizeSeq) return // from before this window's latest request
          // Another window took over after this one's requests: a takeover
          // still waiting to be sent came before that, so it is dropped.
          if (!m.own) this.takeUnsent = false
          this.h.onSize?.(m.cols, m.rows, !!m.own)
        } else if (m.t === 'pong') {
          clearTimeout(this.pongDeadline)
          // The socket outlived a network change: send what waited meanwhile.
          if (!this.backlog) {
            this.flushQueue(ws)
            if (this.takeUnsent && this.size) this.sendSize(ws, true) // asked for while away
          }
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
      if (!this.h.onData(data, this.backlog)) {
        // Not taken in: come back for these bytes once the terminal has caught up.
        this.offset = start
        this.drop()
        this.scheduleReconnect(1000)
        return
      }
      this.offset = start + data.length
    }

    ws.onclose = (ev) => {
      if (gen !== this.gen) return
      this.stopPing()
      this.ws = null
      if (ev.code === 4403) {
        // The owner took back the leave to watch.
        this.ended = true
        this.setStatus('revoked')
        return
      }
      if (ev.code === 4429) {
        this.full = true
        if (this.queue.length > 0) this.h.onDropped?.()
        this.queue = []
        this.takeUnsent = false
        this.takeSeq = 0
        this.setStatus('full')
      }
      if (this.closed || this.ended || this.full) return
      // A socket refused before hello usually means the session is gone (404).
      if (!gotHello && this.attempt >= 1) void this.checkGone()
      this.scheduleReconnect()
    }
  }

  private async checkGone() {
    try {
      const r = await fetch(this.watch ? `/api/shared/${this.id}` : '/api/sessions', { credentials: 'same-origin' })
      if (r.status === 401) {
        this.ended = true
        clearTimeout(this.timer)
        location.href = '/login?next=' + encodeURIComponent(location.pathname + location.search)
        return
      }
      const gone = this.watch ? r.status === 404 : !((await r.json()) as { id: string }[]).some((s) => s.id === this.id)
      if (gone) {
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
    if (this.closed || this.ended || this.full) return
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
    // Input typed while away is sent only if nothing was output since it was
    // typed; otherwise it could land somewhere else than intended (another
    // device may have quit the program or switched panes meanwhile).
    if (this.queue.length === 0) return
    const unchanged = this.queue[0].offset !== null && this.queue[0].offset === this.offset
    const fresh = this.queue.filter((q) => now - q.at < QUEUE_MAX_AGE)
    if (!unchanged || fresh.length < this.queue.length) this.h.onDropped?.()
    if (unchanged) for (const q of fresh) ws.send(q.data)
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
    // Tell the gateway whether someone is looking, so it knows when to notify.
    if (this.isOpen() && !this.watch) this.ws!.send(JSON.stringify({ t: 'visible', v: document.visibilityState === 'visible' }))
    if (document.visibilityState !== 'visible' || this.closed || this.ended) return
    if (this.probe(2500)) return
    if (!this.ws || this.ws.readyState !== WebSocket.CONNECTING) {
      this.attempt = 0
      this.open()
    }
  }

  private onOnline = () => {
    if (this.closed || this.ended || this.full) return
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
    if (this.closed || this.ended || this.full) return
    this.setStatus('offline')
  }
}
