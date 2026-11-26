import { useCallback, useEffect, useId, useRef, useState } from 'react'
import { Terminal } from '@xterm/xterm'
import { FitAddon } from '@xterm/addon-fit'
import { Unicode11Addon } from '@xterm/addon-unicode11'
import { WebLinksAddon } from '@xterm/addon-web-links'
import { t } from '../i18n'
import { termScheme, usePrefs } from '../prefs'
import { isCoarse } from '../components/ui'
import { darkTheme } from './themes'
import type { PanelLive } from './PanelWindow'

const FONT_STACK = "'JetBrains Mono', 'SFMono-Regular', Menlo, Consolas, 'PingFang SC', 'Hiragino Sans GB', 'Microsoft YaHei', monospace"

// Keep each binary WS input frame well under the backend's 1 MiB read limit.
const MAX_INPUT_CHUNK = 256 * 1024
const PING_EVERY_MS = 25000
const PONG_TIMEOUT_MS = 12000
const ACK_BATCH_BYTES = 64 * 1024

const PANEL_THEME = {
  ...darkTheme,
  background: '#14161a',
  cursorAccent: '#14161a',
}

const TOUCH_KEYS: [string, string, string][] = [
  ['up', '↑', '\x1b[A'],
  ['down', '↓', '\x1b[B'],
  ['left', '←', '\x1b[D'],
  ['right', '→', '\x1b[C'],
  ['tab', 'Tab', '\t'],
  ['space', 'Space', ' '],
  ['enter', 'Enter', '\r'],
  ['esc', 'Esc', '\x1b'],
]

// Live single-pane terminal backed by GET /api/sessions/{id}/pane-ws
// (`herdr terminal attach <terminal_id> --takeover`). Gives Chat Mode's dock
// and inline terminal panels the same WebSocket + PTY interactivity as the
// full CLI terminal without changing herdr's focused workspace or layout.
export function PaneTerminal({
  sid,
  pane,
  active,
  live,
  fullAction,
  backToCard,
  onSettled,
  onFallback,
}: {
  sid: string
  pane: string
  active: boolean
  live?: PanelLive
  fullAction?: () => void
  backToCard?: () => void
  onSettled: () => void
  onFallback: () => void
}) {
  const prefs = usePrefs()
  // (The main view's colours: the person's terminal scheme, which follows the page's light or dark — read anew on each.)
  const schemeId = live?.main ? termScheme().id : ''
  const screenId = useId()
  const hostRef = useRef<HTMLDivElement>(null)
  const wsRef = useRef<WebSocket | null>(null)
  const termRef = useRef<Terminal | null>(null)
  const fitRef = useRef<FitAddon | null>(null)
  const [status, setStatus] = useState<'connecting' | 'open' | 'closed'>('connecting')
  const [taking, setTaking] = useState(false)
  const [connSeq, setConnSeq] = useState(0)

  const onSettledRef = useRef(onSettled)
  onSettledRef.current = onSettled
  const onFallbackRef = useRef(onFallback)
  onFallbackRef.current = onFallback
  const liveRef = useRef(live)
  liveRef.current = live
  const activeRef = useRef(active)
  activeRef.current = active
  const settleTimer = useRef<ReturnType<typeof setTimeout>>(undefined)

  const scheduleSettle = useCallback(() => {
    clearTimeout(settleTimer.current)
    settleTimer.current = setTimeout(() => onSettledRef.current(), 550)
  }, [])

  const sendBytes = useCallback(
    (s: string) => {
      const ws = wsRef.current
      if (!ws || ws.readyState !== WebSocket.OPEN) return
      const encoded = new TextEncoder().encode(s)
      for (let off = 0; off < encoded.byteLength; off += MAX_INPUT_CHUNK) {
        ws.send(encoded.subarray(off, Math.min(off + MAX_INPUT_CHUNK, encoded.byteLength)))
      }
      if (s.includes('\r') || s.includes('\x1b')) scheduleSettle()
    },
    [scheduleSettle],
  )
  const sendBytesRef = useRef(sendBytes)
  sendBytesRef.current = sendBytes

  useEffect(() => () => clearTimeout(settleTimer.current), [])

  // Mount xterm.js once per (sid, pane) instance.
  useEffect(() => {
    const el = hostRef.current
    if (!el) return
    const main = !!liveRef.current?.main
    const term = new Terminal({
      fontFamily: FONT_STACK,
      fontSize: main ? prefs.fontSize : Math.max(11, Math.min(16, prefs.fontSize - 1)),
      lineHeight: 1.25,
      cursorBlink: true,
      scrollback: 3000,
      allowProposedApi: true,
      macOptionIsMeta: prefs.optionIsMeta,
      theme: main ? termScheme().theme : PANEL_THEME,
    })
    const fit = new FitAddon()
    const uni = new Unicode11Addon()
    const links = new WebLinksAddon((_e, uri) => {
      window.open(uri, '_blank', 'noopener,noreferrer')
    })
    term.loadAddon(fit)
    term.loadAddon(uni)
    term.loadAddon(links)
    term.unicode.activeVersion = '11'
    term.open(el)
    fit.fit()

    term.attachCustomKeyEventHandler((e) => {
      if (e.type === 'keydown' && e.key === 'Enter' && e.shiftKey && !e.ctrlKey && !e.altKey && !e.metaKey) {
        e.preventDefault()
        e.stopPropagation()
        sendBytesRef.current('\x1b\r')
        return false
      }
      if (e.type === 'keydown' && liveRef.current && e.key === 'F6' && !e.altKey && !e.ctrlKey && !e.metaKey && !e.shiftKey) {
        e.preventDefault()
        e.stopPropagation()
        if (liveRef.current.onLeave) liveRef.current.onLeave()
        else el.closest('.convo-panelwin')?.querySelector<HTMLElement>('.convo-panelwin-head button')?.focus()
        return false
      }
      return true
    })

    const dataSub = term.onData((data) => {
      sendBytesRef.current(data)
    })

    const ta = term.textarea
    const onFocus = () => setTaking(true)
    const onBlur = () => setTaking(false)
    ta?.addEventListener('focus', onFocus)
    ta?.addEventListener('blur', onBlur)

    termRef.current = term
    fitRef.current = fit

    if (activeRef.current && !isCoarse() && (!liveRef.current || main)) {
      term.focus()
    }

    let fitTimer: ReturnType<typeof setTimeout> | undefined
    const ro = new ResizeObserver(() => {
      clearTimeout(fitTimer)
      fitTimer = setTimeout(() => {
        try {
          fit.fit()
          const ws = wsRef.current
          if (ws && ws.readyState === WebSocket.OPEN) {
            ws.send(JSON.stringify({ t: 'resize', cols: term.cols, rows: term.rows }))
          }
        } catch {
          /* unmounted */
        }
      }, 40)
    })
    ro.observe(el)

    return () => {
      clearTimeout(fitTimer)
      ro.disconnect()
      dataSub.dispose()
      ta?.removeEventListener('focus', onFocus)
      ta?.removeEventListener('blur', onBlur)
      term.dispose()
      termRef.current = null
      fitRef.current = null
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [sid, pane])

  // Keep font size and Option-as-Meta synced with user preferences.
  useEffect(() => {
    const term = termRef.current
    if (!term) return
    const main = !!liveRef.current?.main
    term.options.fontSize = main ? prefs.fontSize : Math.max(11, Math.min(16, prefs.fontSize - 1))
    term.options.macOptionIsMeta = prefs.optionIsMeta
    if (main) term.options.theme = termScheme().theme
    try {
      fitRef.current?.fit()
      const ws = wsRef.current
      if (ws && ws.readyState === WebSocket.OPEN) {
        ws.send(JSON.stringify({ t: 'resize', cols: term.cols, rows: term.rows }))
      }
    } catch {
      /* ignore */
    }
  }, [prefs.fontSize, prefs.optionIsMeta, schemeId])

  // Open WebSocket stream to /api/sessions/{id}/pane-ws.
  useEffect(() => {
    let alive = true
    let opened = false
    let receivedBytes = 0
    let fellBack = false
    let ackPending = 0
    let pongTimer: ReturnType<typeof setTimeout> | undefined
    setStatus('connecting')

    const term = termRef.current
    const cols = term?.cols ?? 80
    const rows = term?.rows ?? 24
    const proto = location.protocol === 'https:' ? 'wss:' : 'ws:'
    const url = `${proto}//${location.host}/api/sessions/${encodeURIComponent(sid)}/pane-ws?pane=${encodeURIComponent(pane)}&cols=${cols}&rows=${rows}`
    const ws = new WebSocket(url)
    ws.binaryType = 'arraybuffer'
    wsRef.current = ws

    const clearPong = () => {
      clearTimeout(pongTimer)
      pongTimer = undefined
    }

    const flushAck = (force = false) => {
      if (ackPending <= 0 || ws.readyState !== WebSocket.OPEN) return
      if (!force && ackPending < ACK_BATCH_BYTES) return
      const n = ackPending
      ackPending = 0
      ws.send(JSON.stringify({ t: 'ack', n }))
    }

    const triggerFallback = () => {
      if (!alive || fellBack) return
      fellBack = true
      onFallbackRef.current()
    }

    const ping = setInterval(() => {
      if (ws.readyState !== WebSocket.OPEN) return
      ws.send(JSON.stringify({ t: 'ping' }))
      clearPong()
      pongTimer = setTimeout(() => {
        if (!alive) return
        setStatus('closed')
        try {
          ws.close()
        } catch {
          /* ignore */
        }
      }, PONG_TIMEOUT_MS)
    }, PING_EVERY_MS)

    ws.onopen = () => {
      if (!alive) return
      opened = true
      setStatus('open')
      if (termRef.current) {
        try {
          fitRef.current?.fit()
          ws.send(JSON.stringify({ t: 'resize', cols: termRef.current.cols, rows: termRef.current.rows }))
        } catch {
          /* ignore */
        }
      }
    }

    ws.onmessage = (ev) => {
      if (!alive) return
      clearPong()
      if (ev.data instanceof ArrayBuffer) {
        const bytes = new Uint8Array(ev.data)
        const len = bytes.byteLength
        receivedBytes += len
        const tRef = termRef.current
        if (tRef) {
          tRef.write(bytes, () => {
            if (!alive) return
            ackPending += len
            flushAck(len >= ACK_BATCH_BYTES || ackPending >= ACK_BATCH_BYTES)
          })
          // Flush smaller writes promptly after the microtask/frame so small bursts stay unblocked.
          if (len < ACK_BATCH_BYTES) {
            setTimeout(() => alive && flushAck(true), 50)
          }
        } else {
          ackPending += len
          flushAck(true)
        }
        return
      }
      if (typeof ev.data === 'string') {
        try {
          const msg = JSON.parse(ev.data) as { t?: string; code?: number; bytes?: number }
          if (msg.t === 'exit') {
            if ((msg.code ?? 0) !== 0 && Math.max(receivedBytes, msg.bytes ?? 0) < 256) {
              triggerFallback()
              return
            }
            setStatus('closed')
            onSettledRef.current()
          }
        } catch {
          /* ignore */
        }
      }
    }

    ws.onerror = () => {
      if (!alive) return
      if (!opened) triggerFallback()
    }

    ws.onclose = () => {
      if (!alive) return
      clearPong()
      if (!opened) {
        triggerFallback()
        return
      }
      if (!fellBack) setStatus('closed')
    }

    return () => {
      alive = false
      clearInterval(ping)
      clearPong()
      if (wsRef.current === ws) wsRef.current = null
      try {
        ws.close()
      } catch {
        /* ignore */
      }
    }
  }, [sid, pane, connSeq])

  const who = live?.who || t('panel.whoTerminal')
  const keyBar = isCoarse()

  return (
    <div className={'convo-panelwin' + (live?.main ? ' is-main' : '')} role="group" aria-label={t(live ? 'panel.liveTitle' : 'panel.title')}>
      {!live?.main && (!live?.bare || backToCard) && (
      <div className={'convo-panelwin-head' + (live ? ' is-live' : '')}>
        {!live && <span className="convo-panelwin-title">{t('panel.title')}</span>}
        {!isCoarse() && !live?.bare && <span className="convo-panelwin-hint">{t(live ? 'panel.liveHint' : 'panel.hint')}</span>}
        <div className="convo-panelwin-head-acts">
          {backToCard && (
            <button type="button" className="link-btn convo-panelwin-close" onClick={backToCard}>
              {t('notice.backToCard')}
            </button>
          )}
          {fullAction && !live?.bare && (
            <button type="button" className="link-btn convo-panelwin-close" onClick={fullAction}>
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
            <button type="button" className="link-btn convo-panelwin-close" onClick={() => sendBytes('\x1b')}>
              {t('agents.closeDialog')}
            </button>
          )}
        </div>
      </div>
      )}
      {live?.note && <p className="convo-panelwin-note">{live.note}</p>}
      {live && !live.main && taking && !isCoarse() && (
        <div className="convo-panelwin-taking" id={screenId + '-taking'}>
          <span>{t('panel.taking', { who })}</span>
          {live.onComposer && (
            <button
              type="button"
              className="link-btn"
              onMouseDown={(e) => e.preventDefault()}
              onClick={live.onComposer}
            >
              {t('panel.toComposer')}
            </button>
          )}
        </div>
      )}
      {/* (The main view's ground in its scheme's colour, round its rows: no band of the page's round it.) */}
      <div className="convo-pty-wrap" style={live?.main ? { background: termScheme().theme.background } : undefined}>
        <div
          ref={hostRef}
          className="convo-pty-term"
          onMouseDown={() => termRef.current?.focus()}
          onKeyDown={(e) => {
            if (e.key === 'Escape') e.stopPropagation()
          }}
        />
      </div>
      {status !== 'open' && (
        <div className="convo-pty-bar" role="status">
          <span>{t(status === 'connecting' ? 'panel.ptyConnecting' : 'panel.ptyClosed')}</span>
          {status === 'closed' && (
            <button
              type="button"
              className="link-btn"
              onClick={() => {
                termRef.current?.reset()
                setConnSeq((n) => n + 1)
              }}
            >
              {t('panel.ptyReconnect')}
            </button>
          )}
        </div>
      )}
      {keyBar && (
        <div className="convo-panelwin-keys">
          {TOUCH_KEYS.map(([k, label, seq]) => (
            <button
              key={k}
              type="button"
              className="convo-panelwin-key"
              onClick={() => sendBytes(seq)}
              aria-label={t('panel.sendKey', { key: label, who })}
              title={t('panel.sendKey', { key: label, who })}
            >
              {label}
            </button>
          ))}
        </div>
      )}
    </div>
  )
}
