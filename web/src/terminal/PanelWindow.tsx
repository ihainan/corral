import { useCallback, useEffect, useLayoutEffect, useMemo, useRef, useState } from 'react'
import { api, ApiError, errorText } from '../api'
import { t } from '../i18n'
import { isCoarse } from '../components/ui'
import { ansiLines, type AnsiRun } from './ansi'

// A panel of Claude Code's own (/rewind, /resume, /mcp, /config…), which it
// draws on its screen and tells its log nothing of till it is shut: the
// screen shown here, read over and over while it is open, and the keys pressed
// here passed on one at a time, each after the last has gone. See
// convo_panel.go, for what the host checks before each. A key that settles
// something goes only on the screen last read: one read since what was pressed
// before it. Escape is held back a moment after one: the first may have shut it,
// which the session file says only a moment after, and the second would stop
// what the agent does then.

const EVERY = 800
const ESC_AGAIN = 1000
const QUEUE = 16
const FAILS = 4

const settles = (k: string) => k === 'enter' || k === 'space' || /^[0-9]$/.test(k)

function keyOf(e: React.KeyboardEvent): string | null {
  if (e.altKey || e.metaKey || e.ctrlKey || e.nativeEvent.isComposing || e.keyCode === 229) return null
  const named: Record<string, string> = { ArrowUp: 'up', ArrowDown: 'down', ArrowLeft: 'left', ArrowRight: 'right', Enter: 'enter', Escape: 'esc', Backspace: 'backspace', ' ': 'space' }
  if (e.key === 'Tab') return e.shiftKey ? 'shift+tab' : 'tab'
  if (named[e.key]) return named[e.key]
  return /^[A-Za-z0-9._/]$/.test(e.key) ? e.key : null
}

const TOUCH_KEYS: [string, string][] = [['up', '↑'], ['down', '↓'], ['left', '←'], ['right', '→'], ['tab', 'Tab'], ['space', 'Space'], ['enter', 'Enter'], ['esc', 'Esc']]

const runStyle = (r: AnsiRun): React.CSSProperties => {
  const color = r.inverse ? r.background ?? 'var(--panel-bg)' : r.color
  const background = r.inverse ? r.color ?? 'var(--panel-fg)' : r.background
  return {
    color,
    background,
    fontWeight: r.bold ? 600 : undefined,
    opacity: r.dim ? 0.6 : undefined,
    fontStyle: r.italic ? 'italic' : undefined,
    textDecoration: r.underline ? 'underline' : undefined,
  }
}

export function PanelWindow({ sid, press, onSettled, toast }: { sid: string; press: () => Record<string, unknown>; onSettled: () => void; toast: (s: string, bad?: boolean) => void }) {
  const [screen, setScreen] = useState('')
  const [lost, setLost] = useState(false)
  const [said, setSaid] = useState('')
  const box = useRef<HTMLPreElement>(null)
  const at = useRef({ digest: '', stale: true, reading: false, pumping: false, queue: [] as string[], fails: 0, lastEsc: 0, alive: true })
  const pressNow = useRef(press)
  pressNow.current = press
  const onSettledNow = useRef(onSettled)
  onSettledNow.current = onSettled
  const toastNow = useRef(toast)
  toastNow.current = toast

  const read = useCallback(async () => {
    const a = at.current
    if (a.reading) return
    a.reading = true
    try {
      const r = await api<{ text: string; digest: string }>('POST', `/api/sessions/${sid}/conversation/panel-screen`, pressNow.current(), { timeoutMs: 12000 })
      if (!a.alive) return
      a.digest = r.digest
      a.stale = false
      a.fails = 0
      setLost(false)
      setScreen(r.text)
    } catch (e) {
      // (Another of its presses under way is no failure to read.)
      if (e instanceof ApiError && e.code === 'busy') return
      a.fails++
      if (a.alive && a.fails >= FAILS) setLost(true)
    } finally {
      a.reading = false
    }
  }, [sid])

  const pump = useCallback(async () => {
    const a = at.current
    if (a.pumping) return
    a.pumping = true
    try {
      while (a.queue.length && a.alive) {
        const k = a.queue[0]
        if (settles(k) && a.stale) {
          await read()
          if (a.stale) {
            a.queue.length = 0 // (the screen not read: nothing goes on one not seen)
            break
          }
        }
        a.queue.shift()
        try {
          await api('POST', `/api/sessions/${sid}/conversation/panel`, { ...pressNow.current(), keys: [k], ...(settles(k) ? { digest: a.digest } : {}) }, { timeoutMs: 15000 })
          a.stale = true
          if (a.alive) setSaid('')
          if (k === 'esc' || settles(k)) setTimeout(() => onSettledNow.current(), 700)
        } catch (e) {
          a.queue.length = 0 // (what was pressed after it was meant for what this would have shown)
          a.stale = true
          if (e instanceof ApiError && e.code === 'screen_changed') setSaid(t('panel.changed'))
          else if (e instanceof ApiError && e.code === 'no_dialog') onSettledNow.current()
          else toastNow.current(e instanceof ApiError ? errorText(e) : t('panel.failed'), true)
        }
      }
      if (a.stale && a.alive) await read()
    } finally {
      a.pumping = false
    }
  }, [sid, read])

  const send = useCallback(
    (k: string) => {
      const a = at.current
      if (k === 'esc') {
        const now = Date.now()
        if (now - a.lastEsc < ESC_AGAIN || a.queue.includes('esc')) return
        a.lastEsc = now
      }
      if (a.queue.length >= QUEUE) return
      a.queue.push(k)
      void pump()
    },
    [pump],
  )

  // Read over and over while it is open and seen; the focus on it for its keys.
  useEffect(() => {
    const a = at.current
    a.alive = true
    let timer: ReturnType<typeof setTimeout>
    const tick = async () => {
      if (!document.hidden && !a.pumping) await read()
      if (a.alive) timer = setTimeout(tick, EVERY)
    }
    void tick()
    return () => {
      a.alive = false
      clearTimeout(timer)
    }
  }, [read])
  useEffect(() => {
    if (!isCoarse()) box.current?.focus({ preventScroll: true })
  }, [])

  const lines = useMemo(() => ansiLines(screen), [screen])
  // (Its foot in view, where a panel is: unless read further up.)
  const pinned = useRef(true)
  useLayoutEffect(() => {
    const el = box.current
    if (el && pinned.current) el.scrollTop = el.scrollHeight
  }, [lines])

  return (
    <div className="convo-panelwin" role="group" aria-label={t('panel.title')}>
      <div className="convo-panelwin-head">
        <span className="convo-panelwin-title">{t('panel.title')}</span>
        {!isCoarse() && <span className="convo-panelwin-hint">{t('panel.hint')}</span>}
        <button type="button" className="link-btn convo-panelwin-close" onClick={() => send('esc')}>
          {t('agents.closeDialog')}
        </button>
      </div>
      {/* (Takes the keys: what is pressed here goes to the panel.) */}
      <pre
        ref={box}
        className="convo-panelwin-screen"
        tabIndex={0}
        aria-label={t('panel.screen')}
        onScroll={(e) => {
          const el = e.currentTarget
          pinned.current = el.scrollHeight - el.scrollTop - el.clientHeight < 24
        }}
        onKeyDown={(e) => {
          const k = keyOf(e)
          if (!k) return
          e.preventDefault()
          e.stopPropagation()
          send(k)
        }}
      >
        {lines.map((runs, i) => (
          <div key={i} className="convo-panelwin-line">
            {runs.length ? runs.map((r, j) => <span key={j} style={runStyle(r)}>{r.text}</span>) : ' '}
          </div>
        ))}
      </pre>
      {(said || lost) && <p className="convo-panelwin-note">{lost ? t('panel.lost') : said}</p>}
      {isCoarse() && (
        <div className="convo-panelwin-keys">
          {TOUCH_KEYS.map(([k, label]) => (
            <button key={k} type="button" className="convo-panelwin-key" onClick={() => send(k)} aria-label={label}>
              {label}
            </button>
          ))}
        </div>
      )}
    </div>
  )
}
