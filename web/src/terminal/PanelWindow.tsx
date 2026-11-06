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
// something goes with the screen on show when it was pressed, not one read since
// (which could be a new picker, unseen): the host presses it only on that. So
// Down and Enter faster than the screen comes back is said to be pressed too soon,
// not taken for what it might have meant. Escape is not sent within a moment of
// the last one having gone: the first may have shut it.

const EVERY = 800
const ESC_AGAIN = 1000
const QUEUE = 16
const FAILS = 4

const settles = (k: string) => k === 'enter' || k === 'space' || /^[0-9]$/.test(k)
type Pressed = { k: string; digest: string }

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

// live: the terminal as it is, where the page cannot make out what is up there
// or was refused a message: closed here without a key, the whole terminal a tap
// away (words, which keys one by one cannot be, go there).
export type PanelLive = { onClose?: () => void; onFull?: () => void; note?: string }
export function PanelWindow({ sid, active, press, onSettled, toast, live }: { live?: PanelLive; sid: string; active: boolean; press: () => Record<string, unknown>; onSettled: () => void; toast: (s: string, bad?: boolean) => void }) {
  const [screen, setScreen] = useState({ text: '', digest: '' })
  const [lost, setLost] = useState(false)
  const [said, setSaid] = useState('')
  const box = useRef<HTMLPreElement>(null)
  const at = useRef({ shown: '', stale: true, reading: false, pumping: false, queue: [] as Pressed[], fails: 0, escDone: 0, escGoing: false, alive: true, look: null as AbortController | null })
  const isLive = useRef(!!live)
  isLive.current = !!live
  const activeNow = useRef(active)
  activeNow.current = active
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
    // (Given up where a key comes meanwhile: the host takes one at a time for a pane.)
    const look = new AbortController()
    a.look = look
    try {
      const r = await api<{ text: string; digest: string }>('POST', `/api/sessions/${sid}/conversation/panel-screen`, pressNow.current(), { timeoutMs: 12000, signal: look.signal })
      if (!a.alive) return
      a.stale = false
      a.fails = 0
      setLost(false)
      setScreen({ text: r.text, digest: r.digest })
    } catch (e) {
      // (Given up for a key, or another of its presses under way: no failure to read.)
      if (look.signal.aborted || (e instanceof ApiError && e.code === 'busy')) return
      a.fails++
      if (a.alive && a.fails >= FAILS) setLost(true)
    } finally {
      a.reading = false
      if (a.look === look) a.look = null
    }
  }, [sid])

  const pump = useCallback(async () => {
    const a = at.current
    if (a.pumping) return
    a.pumping = true
    try {
      // (Till none is left: one pressed during the read after the last is sent too.)
      while (a.alive && (a.queue.length || a.stale)) {
        if (!a.queue.length) {
          await read()
          if (a.stale && !a.queue.length) break // (not read: the next look tries again)
          continue
        }
        const { k, digest } = a.queue.shift()!
        if (k === 'esc') {
          if (Date.now() - a.escDone < ESC_AGAIN) continue // (the last may have shut it, however long that took to go)
          a.escGoing = true
        }
        try {
          // (What the key left comes back with it: another round trip for it would be
          // most of what a key takes on a far-off host.)
          const r = await api<{ text?: string; digest?: string }>('POST', `/api/sessions/${sid}/conversation/panel`, { ...pressNow.current(), keys: [k], ...(settles(k) ? { digest } : {}), ...(isLive.current ? { live: true } : {}) }, { timeoutMs: 15000 })
          a.stale = true
          if (a.alive) setSaid('')
          if (a.alive && typeof r?.text === 'string' && typeof r.digest === 'string') {
            a.stale = false
            setLost(false)
            setScreen({ text: r.text, digest: r.digest })
          }
          if (k === 'esc' || settles(k)) setTimeout(() => onSettledNow.current(), 700)
        } catch (e) {
          a.queue.length = 0 // (what was pressed after it was meant for what this would have shown)
          a.stale = true
          if (e instanceof ApiError && e.code === 'screen_changed') setSaid(t('panel.changed'))
          else if (e instanceof ApiError && e.code === 'no_dialog') onSettledNow.current()
          else toastNow.current(e instanceof ApiError ? errorText(e) : t('panel.failed'), true)
        } finally {
          if (k === 'esc') {
            a.escDone = Date.now()
            a.escGoing = false
          }
        }
      }
    } finally {
      a.pumping = false
    }
  }, [sid, read])

  const send = useCallback(
    (k: string) => {
      const a = at.current
      if (k === 'esc' && (a.escGoing || a.queue.some((x) => x.k === 'esc'))) return
      if (settles(k) && !a.shown) return // (nothing on show yet)
      if (a.queue.length >= QUEUE) return
      a.queue.push({ k, digest: a.shown })
      a.look?.abort() // (a look under way would hold it up behind it)
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
      // (Not while a key is going or waiting: the host takes one at a time for a
      // pane, and a look under way would hold the key up behind it.)
      if (!document.hidden && activeNow.current && !a.pumping && !a.queue.length) await read()
      if (a.alive) timer = setTimeout(tick, EVERY)
    }
    void tick()
    return () => {
      a.alive = false
      a.look?.abort()
      clearTimeout(timer)
    }
  }, [read])
  useEffect(() => {
    if (active && !isCoarse()) box.current?.focus({ preventScroll: true })
  }, [active])

  const lines = useMemo(() => ansiLines(screen.text), [screen.text])
  // (What a key goes with: what has been drawn, not what was read.)
  useLayoutEffect(() => {
    at.current.shown = screen.digest
  }, [screen.digest])
  // (Its foot in view, where a panel is: unless read further up.)
  const pinned = useRef(true)
  useLayoutEffect(() => {
    const el = box.current
    if (el && pinned.current) el.scrollTop = el.scrollHeight
  }, [lines])

  return (
    <div className="convo-panelwin" role="group" aria-label={t(live ? 'panel.liveTitle' : 'panel.title')}>
      <div className="convo-panelwin-head">
        <span className="convo-panelwin-title">{t(live ? 'panel.liveTitle' : 'panel.title')}</span>
        {!isCoarse() && <span className="convo-panelwin-hint">{t('panel.hint')}</span>}
        {live?.onFull && (
          <button type="button" className="link-btn convo-panelwin-close" onClick={live.onFull}>
            {t('panel.full')}
          </button>
        )}
        {live ? (
          live.onClose && (
            <button type="button" className="link-btn convo-panelwin-close" onClick={live.onClose}>
              {t('common.close')}
            </button>
          )
        ) : (
          <button type="button" className="link-btn convo-panelwin-close" onClick={() => send('esc')}>
            {t('agents.closeDialog')}
          </button>
        )}
      </div>
      {live?.note && <p className="convo-panelwin-note">{live.note}</p>}
      {/* (Takes the keys: what is pressed here goes to the panel.) */}
      <pre
        ref={box}
        className="convo-panelwin-screen"
        tabIndex={0}
        aria-label={t(live ? 'panel.liveScreen' : 'panel.screen')}
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
