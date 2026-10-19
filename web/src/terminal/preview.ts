import type { Terminal } from '@xterm/xterm'

// Screen previews for the session list. The gateway keeps no screen of its
// own, so the owner's window sends what it shows, as text: every so often
// while it changes and the page is visible, when the page is hidden, and when
// the terminal is left. ready says whether the screen is worth sending (the
// connection is live and its backlog has been written).

const EVERY = 10000 // ms between sends while the screen changes
const MAX_COLS = 500 // the gateway's limits
const MAX_LINES = 100
const MAX_RUNES = 1000 // per line (a cell can hold combining characters)
// Under the browser's 64 KiB for requests sent while a page closes
// (keepalive), and the gateway's limit on the text.
const MAX_BODY = 60000

// This page, and the number of its latest preview (of any session, and for
// terminals opened again on the page).
const WIN = Math.random().toString(36).slice(2) + Date.now().toString(36)
let seq = 0

export function startPreviews(id: string, term: Terminal, ready: () => boolean): () => void {
  let dirty = true
  let sent = 0 // the number of the latest sent, and of the latest the gateway took
  let taken = 0
  const marks = [term.onWriteParsed(() => (dirty = true)), term.onResize(() => (dirty = true))]
  const enc = new TextEncoder()
  // What the screen shows now, as the request body.
  const capture = () => {
    const buf = term.buffer.active
    const cols = Math.min(term.cols, MAX_COLS)
    // The bottom of a tall screen: where the prompt is.
    const lines: string[] = []
    for (let y = Math.max(0, term.rows - MAX_LINES); y < term.rows; y++) lines.push(buf.getLine(buf.baseY + y)?.translateToString(true, 0, cols) ?? '')
    for (let i = 0; i < lines.length; i++) if (lines[i].length > MAX_RUNES) lines[i] = Array.from(lines[i]).slice(0, MAX_RUNES).join('')
    // Numbered, so the gateway drops one that arrives after a newer one.
    const body = (l: string[]) => JSON.stringify({ cols, lines: l, win: WIN, seq: ++seq })
    let out = body(lines)
    while (enc.encode(out).length > MAX_BODY && lines.length > 1) {
      lines.splice(0, Math.max(1, lines.length >> 3))
      out = body(lines)
    }
    return out
  }
  const again = () => (dirty = true)
  const send = (leaving: boolean) => {
    // When leaving, also the screen of a request still on its way, which
    // the browser may cancel as the page closes.
    if (!(dirty || (leaving && taken < sent)) || !ready()) return
    dirty = false
    const body = capture()
    const n = (sent = seq)
    void fetch(`/api/sessions/${id}/preview`, {
      method: 'PUT',
      credentials: 'same-origin',
      headers: { 'Content-Type': 'application/json' },
      body,
      keepalive: leaving, // (a page being closed still sends it)
    })
      .then((r) => {
        if (r.ok || r.status === 404 || r.status === 409) taken = Math.max(taken, n) // (not again for a session gone or ended)
        else again()
      })
      .catch(again)
  }
  const timer = window.setInterval(() => document.visibilityState === 'visible' && send(false), EVERY)
  const onHide = () => document.visibilityState === 'hidden' && send(true)
  document.addEventListener('visibilitychange', onHide)
  return () => {
    send(true)
    clearInterval(timer)
    document.removeEventListener('visibilitychange', onHide)
    marks.forEach((m) => m.dispose())
  }
}
