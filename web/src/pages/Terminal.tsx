import { useCallback, useEffect, useRef, useState } from 'react'
import { flushSync } from 'react-dom'
import { Terminal } from '@xterm/xterm'
import { FitAddon } from '@xterm/addon-fit'
import { Unicode11Addon } from '@xterm/addon-unicode11'
import { WebLinksAddon } from '@xterm/addon-web-links'
import { WebglAddon } from '@xterm/addon-webgl'
import { ArrowLeft, ImagePlus, MessageSquareText, MoreVertical, Minus, Plus, TextSelect, Keyboard as KeyboardIcon, Power, X } from 'lucide-react'
import { api, errorText, uploadFile, type Host, type SessionInfo } from '../api'
import { t } from '../i18n'
import { navigate } from '../router'
import { getPrefs, isDark, setPrefs, usePrefs } from '../prefs'
import { local } from '../storage'
import { Confirm, Menu, copyText as copyToClipboard, useFocusTrap, useToast } from '../components/ui'
import { useConnect } from '../connect'
import { TermConnection, type EndInfo, type Status } from '../terminal/connection'
import { applyMods, specialKey, type Mods, type SpecialKey } from '../terminal/keys'
import { KeyBar, type ModState } from '../terminal/KeyBar'
import { darkTheme, lightTheme } from '../terminal/themes'

const isTouch = window.matchMedia('(pointer: coarse)').matches || navigator.maxTouchPoints > 0
const FONT_STACK = "'JetBrains Mono', 'SFMono-Regular', Menlo, Consolas, 'PingFang SC', 'Hiragino Sans GB', 'Microsoft YaHei', monospace"

// Quote a remote path for a POSIX shell when it contains anything beyond safe
// characters. Generated upload names are safe; the home directory may not be.
function shellPath(p: string): string | null {
  if (/[\x00-\x1f\x7f]/.test(p)) return null
  if (/^[\w@%+=:,./~-]+$/.test(p)) return p
  return "'" + p.replaceAll("'", "'\\''") + "'"
}

export function TerminalPage({ id }: { id: string }) {
  const prefs = usePrefs()
  const toast = useToast()
  const rootRef = useRef<HTMLDivElement>(null)
  const hostEl = useRef<HTMLDivElement>(null)
  const termRef = useRef<Terminal | null>(null)
  const fitRef = useRef<FitAddon | null>(null)
  const connRef = useRef<TermConnection | null>(null)
  const fileRef = useRef<HTMLInputElement>(null)
  const composerRef = useRef<HTMLTextAreaElement>(null)

  const [info, setInfo] = useState<SessionInfo | null>(null)
  const [status, setStatus] = useState<Status>('connecting')
  const [latency, setLatency] = useState<number | null>(null)
  const [end, setEnd] = useState<EndInfo | null>(null)
  const [ctrl, setCtrl] = useState<ModState>('off')
  const [alt, setAlt] = useState<ModState>('off')
  const [fn, setFn] = useState(false)
  const [composer, setComposer] = useState(false)
  const [draft, setDraft] = useState('') // kept in memory only: drafts may hold secrets
  const [copyText, setCopyText] = useState<string | null>(null)
  const [dragOver, setDragOver] = useState(false)
  const [confirm, setConfirm] = useState<React.ReactNode>(null)
  const showKeybar = isTouch && prefs.keybar

  // Latest modifier state for the xterm data callback.
  const mods = useRef<{ ctrl: ModState; alt: ModState }>({ ctrl: 'off', alt: 'off' })
  mods.current = { ctrl, alt }
  const releaseOnce = useCallback(() => {
    if (mods.current.ctrl === 'once') setCtrl('off')
    if (mods.current.alt === 'once') setAlt('off')
  }, [])
  const activeMods = (): Mods => ({ ctrl: mods.current.ctrl !== 'off', alt: mods.current.alt !== 'off', shift: false })

  // Session metadata (host name, host id for reconnect).
  useEffect(() => {
    api<SessionInfo[]>('GET', '/api/sessions')
      .then((list) => {
        const s = list.find((x) => x.id === id)
        if (s) {
          setInfo(s)
          local.set('corral.sessHost.' + id, String(s.host_id))
        }
      })
      .catch(() => {})
  }, [id])

  // Terminal + connection lifecycle.
  useEffect(() => {
    const el = hostEl.current!
    const p = getPrefs()
    const term = new Terminal({
      fontFamily: FONT_STACK,
      fontSize: p.fontSize,
      lineHeight: 1.12,
      cursorBlink: true,
      scrollback: 10000,
      allowProposedApi: true,
      macOptionIsMeta: p.optionIsMeta,
      rescaleOverlappingGlyphs: true,
      theme: isDark() ? darkTheme : lightTheme,
      linkHandler: {
        activate: (_e, uri) => {
          if (/^https?:\/\//i.test(uri)) window.open(uri, '_blank', 'noopener,noreferrer')
        },
      },
    })
    term.loadAddon(new Unicode11Addon())
    term.unicode.activeVersion = '11'
    const fit = new FitAddon()
    term.loadAddon(fit)
    term.loadAddon(new WebLinksAddon((_e, uri) => window.open(uri, '_blank', 'noopener,noreferrer')))
    term.open(el)
    // The DOM renderer is the default: it is correct at every pixel ratio.
    // WebGL is faster on desktops with real GPUs and is opt-in.
    if (p.webgl) {
      try {
        const gl = new WebglAddon()
        gl.onContextLoss(() => gl.dispose())
        term.loadAddon(gl)
      } catch {
        /* stay on the DOM renderer */
      }
    }
    termRef.current = term
    fitRef.current = fit
    const ta = el.querySelector('textarea')
    ta?.setAttribute('autocapitalize', 'off')
    ta?.setAttribute('autocorrect', 'off')
    ta?.setAttribute('spellcheck', 'false')

    const safeFit = () => {
      try {
        fit.fit()
      } catch {
        /* not visible yet */
      }
    }
    safeFit()

    const conn = new TermConnection(id, {
      // RIS (ESC c) travels in the same write queue as the replay that follows,
      // so output still being parsed cannot land after the reset.
      onReset: () => {
        try {
          term.write('\x1bc')
          return true
        } catch {
          return false
        }
      },
      onData: (d) => {
        try {
          term.write(d)
          return true
        } catch {
          return false // xterm's write buffer is full; resume later from here
        }
      },
      onStatus: setStatus,
      onEnd: setEnd,
      onLatency: setLatency,
    })
    connRef.current = conn
    conn.resize(term.cols, term.rows)

    // Latched Ctrl/Alt from the key bar apply only to what the user typed:
    // a character applies when xterm emits exactly that character for the key
    // just pressed; special keys (Enter, arrows, Tab…) are encoded here. Pastes,
    // input-method results and the terminal's own replies pass through untouched.
    let typedChar: string | null = null
    let composing = false
    const latched = () => mods.current.ctrl !== 'off' || mods.current.alt !== 'off'
    const onCompStart = () => {
      composing = true
      typedChar = null
    }
    const onCompEnd = () => {
      composing = false
      typedChar = null
    }
    // iOS soft keyboards report printable keys through input events.
    const onBeforeInput = (e: Event) => {
      const ie = e as InputEvent
      typedChar = !ie.isComposing && ie.inputType === 'insertText' && ie.data && [...ie.data].length === 1 ? ie.data : null
    }
    const onPasteEv = () => (typedChar = null)
    ta?.addEventListener('compositionstart', onCompStart)
    ta?.addEventListener('compositionend', onCompEnd)
    ta?.addEventListener('beforeinput', onBeforeInput)
    ta?.addEventListener('paste', onPasteEv, true)
    const dataSub = term.onData((data) => {
      const typed = typedChar !== null && data === typedChar && !composing
      typedChar = null
      if (typed && latched()) {
        conn.send(applyMods(data, activeMods()))
        releaseOnce()
      } else {
        conn.send(data)
      }
    })
    const binSub = term.onBinary((data) => {
      const bytes = new Uint8Array(data.length)
      for (let i = 0; i < data.length; i++) bytes[i] = data.charCodeAt(i) & 0xff
      conn.send(bytes)
    })
    let resizeTimer: number | undefined
    const resizeSub = term.onResize(({ cols, rows }) => {
      // Keyboard animations fire many resizes; tell the host once things settle.
      clearTimeout(resizeTimer)
      resizeTimer = window.setTimeout(() => conn.resize(cols, rows), 120)
    })
    const specialFor: Record<string, SpecialKey> = {
      Enter: 'Enter', Tab: 'Tab', Escape: 'Esc', Backspace: 'Backspace', Delete: 'Delete', Insert: 'Insert',
      ArrowUp: 'Up', ArrowDown: 'Down', ArrowLeft: 'Left', ArrowRight: 'Right', Home: 'Home', End: 'End',
      PageUp: 'PageUp', PageDown: 'PageDown', F1: 'F1', F2: 'F2', F3: 'F3', F4: 'F4', F5: 'F5', F6: 'F6',
      F7: 'F7', F8: 'F8', F9: 'F9', F10: 'F10', F11: 'F11', F12: 'F12',
    }
    term.attachCustomKeyEventHandler((e) => {
      if (e.type !== 'keydown') return true
      if (e.isComposing || e.keyCode === 229) return true
      typedChar = e.key.length === 1 && !e.metaKey && !e.ctrlKey ? e.key : null
      // A physical Ctrl/Alt chord while a key-bar modifier is latched: combine both.
      // Clipboard shortcuts, AltGr and text-producing Option are left alone.
      const isMac = /Mac|iPhone|iPad/.test(navigator.platform)
      const physAlt = e.altKey && !e.getModifierState('AltGraph') && (!isMac || getPrefs().optionIsMeta)
      const pasteKey = e.ctrlKey && e.key.toLowerCase() === 'v'
      if (latched() && e.key.length === 1 && (e.ctrlKey || physAlt) && !e.metaKey && !pasteKey) {
        const m = activeMods()
        conn.send(applyMods(e.key, { ctrl: m.ctrl || e.ctrlKey, alt: m.alt || physAlt, shift: false }))
        releaseOnce()
        e.preventDefault()
        return false
      }
      const special = specialFor[e.key]
      if (special && latched() && !e.metaKey) {
        const m = activeMods()
        conn.send(specialKey(special, { ctrl: m.ctrl || e.ctrlKey, alt: m.alt || e.altKey, shift: e.shiftKey }, term.modes.applicationCursorKeysMode))
        releaseOnce()
        e.preventDefault()
        return false
      }
      if (e.key === 'Enter' && e.shiftKey && !e.ctrlKey && !e.altKey && !e.metaKey) {
        conn.send('\x1b\r') // newline without submitting, for Claude Code / Codex
        return false
      }
      return true
    })

    let fitTimer: number | undefined
    const ro = new ResizeObserver(() => {
      clearTimeout(fitTimer)
      fitTimer = window.setTimeout(safeFit, 60)
    })
    ro.observe(el)
    if (!isTouch) term.focus()

    return () => {
      ro.disconnect()
      clearTimeout(fitTimer)
      clearTimeout(resizeTimer)
      ta?.removeEventListener('compositionstart', onCompStart)
      ta?.removeEventListener('compositionend', onCompEnd)
      ta?.removeEventListener('beforeinput', onBeforeInput)
      ta?.removeEventListener('paste', onPasteEv, true)
      dataSub.dispose()
      binSub.dispose()
      resizeSub.dispose()
      conn.dispose()
      term.dispose()
      termRef.current = null
      connRef.current = null
    }
  }, [id, releaseOnce])

  // Preferences that can change while the terminal is open.
  useEffect(() => {
    const term = termRef.current
    if (!term) return
    term.options.fontSize = prefs.fontSize
    term.options.macOptionIsMeta = prefs.optionIsMeta
    term.options.theme = isDark() ? darkTheme : lightTheme
    try {
      fitRef.current?.fit()
    } catch {
      /* ignore */
    }
  }, [prefs])

  // Follow the visual viewport so the key bar sits right above the soft
  // keyboard. iOS Safari does not shrink the layout viewport for the keyboard.
  useEffect(() => {
    const vv = window.visualViewport
    const apply = () => {
      const h = vv ? vv.height : window.innerHeight
      const root = rootRef.current
      if (!root) return
      root.style.height = `${Math.round(h)}px`
      // While the keyboard is up iOS may pan the visual viewport; follow it. When
      // it is down, ignore offsetTop: iOS 26 can leave a stale value behind.
      const keyboardUp = vv ? window.innerHeight - vv.height > 120 : false
      root.style.transform = keyboardUp && vv ? `translateY(${Math.round(vv.offsetTop)}px)` : ''
      if (window.scrollY !== 0) window.scrollTo(0, 0)
    }
    apply()
    vv?.addEventListener('resize', apply)
    vv?.addEventListener('scroll', apply)
    window.addEventListener('resize', apply)
    document.documentElement.classList.add('term-mode')
    return () => {
      vv?.removeEventListener('resize', apply)
      vv?.removeEventListener('scroll', apply)
      window.removeEventListener('resize', apply)
      document.documentElement.classList.remove('term-mode')
    }
  }, [])

  // Touch: one finger scrolls, two fingers resize text. Scrolling is delivered
  // as wheel events to xterm, which encodes them for whatever mouse protocol the
  // program enabled (or scrolls its own scrollback when none is active).
  useEffect(() => {
    const el = hostEl.current!
    let lastY = 0
    let lastT = 0
    let startY = 0
    let acc = 0
    let moved = false
    let velocity = 0
    let pinch: { dist: number; size: number } | null = null
    let inertia = 0

    const cellHeight = () => {
      const term = termRef.current
      return term && term.rows > 0 ? el.clientHeight / term.rows : 18
    }
    // One wheel event per line: in mouse-reporting mode xterm sends one report
    // per event regardless of its delta.
    // Returns how many lines were dispatched (at most 10 per call; the rest
    // stays in the caller's accumulator for the next frame).
    const wheel = (lines: number): number => {
      const target = el.querySelector('.xterm-screen') ?? el
      const r = target.getBoundingClientRect()
      const n = Math.min(Math.abs(lines), 10)
      for (let i = 0; i < n; i++) {
        target.dispatchEvent(
          new WheelEvent('wheel', {
            deltaY: Math.sign(lines), deltaMode: WheelEvent.DOM_DELTA_LINE, bubbles: true, cancelable: true,
            clientX: r.left + r.width / 2, clientY: r.top + r.height / 2,
          }),
        )
      }
      return n * Math.sign(lines)
    }
    const dist = (t: TouchList) => Math.hypot(t[0].clientX - t[1].clientX, t[0].clientY - t[1].clientY)
    // Any change in the number of fingers starts a fresh gesture.
    const rebase = (e: TouchEvent) => {
      cancelAnimationFrame(inertia)
      acc = 0
      velocity = 0
      pinch = e.touches.length === 2 ? { dist: dist(e.touches), size: getPrefs().fontSize } : null
      if (e.touches.length === 1) {
        startY = lastY = e.touches[0].clientY
        lastT = performance.now()
      }
    }

    const onStart = (e: TouchEvent) => {
      if (e.touches.length === 1) moved = false
      rebase(e)
    }
    const onMove = (e: TouchEvent) => {
      if (pinch && e.touches.length === 2) {
        e.preventDefault()
        moved = true
        const size = Math.round(Math.min(28, Math.max(9, pinch.size * (dist(e.touches) / pinch.dist))))
        if (size !== getPrefs().fontSize) setPrefs({ fontSize: size })
        return
      }
      if (e.touches.length !== 1) return
      const y = e.touches[0].clientY
      if (!moved && Math.abs(y - startY) < 8) return
      moved = true
      e.preventDefault()
      e.stopPropagation()
      const now = performance.now()
      const dy = y - lastY
      velocity = dy / Math.max(1, now - lastT)
      lastY = y
      lastT = now
      acc -= dy
      const ch = cellHeight()
      const lines = Math.trunc(acc / ch)
      if (lines !== 0) acc -= wheel(lines) * ch
    }
    const onEnd = (e: TouchEvent) => {
      if (e.touches.length > 0) {
        rebase(e)
        return
      }
      if (!moved) return
      e.preventDefault()
      if (pinch) {
        pinch = null
        return
      }
      let v = -velocity * 16 // px per frame
      const ch = cellHeight()
      // Lines owed from the drag itself carry over into the fling.
      let rest = acc
      acc = 0
      if (Math.abs(v) <= 2) v = 0
      const step = () => {
        v *= 0.94
        if (Math.abs(v) < 0.6) v = 0
        rest += v
        const lines = Math.trunc(rest / ch)
        if (lines !== 0) rest -= wheel(lines) * ch
        // Keep going until the speed is gone and no whole line is owed.
        if (v !== 0 || Math.abs(rest) >= ch) inertia = requestAnimationFrame(step)
      }
      inertia = requestAnimationFrame(step)
    }
    const onCancel = () => {
      cancelAnimationFrame(inertia)
      pinch = null
      moved = false
    }
    el.addEventListener('touchstart', onStart, { passive: true, capture: true })
    el.addEventListener('touchmove', onMove, { passive: false, capture: true })
    el.addEventListener('touchend', onEnd, { passive: false, capture: true })
    el.addEventListener('touchcancel', onCancel, { capture: true })
    return () => {
      cancelAnimationFrame(inertia)
      el.removeEventListener('touchstart', onStart, { capture: true })
      el.removeEventListener('touchmove', onMove, { capture: true })
      el.removeEventListener('touchend', onEnd, { capture: true })
      el.removeEventListener('touchcancel', onCancel, { capture: true })
    }
  }, [])

  // Files: paste, drop, or pick. Uploaded over SFTP; the remote path is pasted
  // into the terminal, which is how Claude Code and Codex pick up images.
  const upload = useCallback(
    async (files: File[]) => {
      for (const f of files) {
        toast(t('term.uploading', { name: f.name || 'image' }))
        try {
          const r = await uploadFile(id, f)
          const quoted = shellPath(r.path)
          if (quoted) {
            termRef.current?.paste(quoted + ' ')
            toast(t('term.uploaded'))
          } else {
            toast(r.path)
          }
        } catch (e) {
          toast(errorText(e), true)
        }
      }
      if (!isTouch && !document.querySelector('[aria-modal="true"]')) termRef.current?.focus()
    },
    [id, toast],
  )

  useEffect(() => {
    const el = hostEl.current!
    const onPaste = (e: ClipboardEvent) => {
      const files = [...(e.clipboardData?.files ?? [])]
      if (files.length === 0) return
      e.preventDefault()
      e.stopPropagation()
      void upload(files)
    }
    el.addEventListener('paste', onPaste, { capture: true })
    return () => el.removeEventListener('paste', onPaste, { capture: true })
  }, [upload])

  const sendKey = (k: SpecialKey) => {
    const term = termRef.current
    connRef.current?.send(specialKey(k, activeMods(), term?.modes.applicationCursorKeysMode ?? false))
    releaseOnce()
  }
  const sendText = (s: string) => {
    connRef.current?.send(applyMods(s, activeMods()))
    releaseOnce()
  }
  const cycle = (m: ModState): ModState => (m === 'off' ? 'once' : m === 'once' ? 'lock' : 'off')
  const toggleKeyboard = () => {
    const ta = hostEl.current?.querySelector('textarea')
    if (ta && document.activeElement === ta) ta.blur()
    else termRef.current?.focus()
  }

  // The composer's text and the optional Enter go out as one message, and only
  // while connected; otherwise the draft stays so nothing is half-sent.
  const sendComposer = (submit: boolean) => {
    const term = termRef.current
    const conn = connRef.current
    if (!term || !conn) return
    if (!draft && !submit) return
    if (!conn.isOpen()) {
      toast(t('term.notConnected'), true)
      return
    }
    let payload = draft.replace(/\r?\n/g, '\r')
    if (payload && term.modes.bracketedPasteMode) payload = '\x1b[200~' + payload + '\x1b[201~'
    if (submit) payload += '\r'
    if (new TextEncoder().encode(payload).length > 4 << 20) {
      toast(t('term.tooLong'), true) // keep the draft; upload it as a file instead
      return
    }
    if (conn.send(payload) !== 'sent') {
      toast(t('term.notConnected'), true)
      return
    }
    setDraft('')
    composerRef.current?.focus()
  }

  const toggleComposer = () => {
    if (composer) {
      setComposer(false)
      return
    }
    // Mount and focus inside the tap itself: iOS only raises the keyboard for a
    // focus that happens during the user's gesture.
    flushSync(() => setComposer(true))
    composerRef.current?.focus()
  }

  const openCopyMode = () => {
    const term = termRef.current
    if (!term) return
    const buf = term.buffer.active
    const lines: string[] = []
    const start = Math.max(0, buf.length - 3000)
    for (let i = start; i < buf.length; i++) lines.push(buf.getLine(i)?.translateToString(true) ?? '')
    while (lines.length && lines[lines.length - 1] === '') lines.pop()
    setCopyText(lines.join('\n'))
  }

  const hostIdForReconnect = info?.host_id ?? Number(local.get('corral.sessHost.' + id) || 0)
  const { connect, dialogs } = useConnect((s) => navigate('/t/' + s.id, true))
  const reconnect = async () => {
    if (!hostIdForReconnect) return navigate('/')
    try {
      const h = await api<Host>('GET', '/api/hosts/' + hostIdForReconnect)
      connect(h)
    } catch (e) {
      toast(errorText(e), true)
    }
  }

  const statusText =
    status === 'open' ? (latency !== null ? t('term.latency', { ms: latency }) : '') :
    status === 'connecting' ? t('term.connecting') :
    status === 'reconnecting' ? t('term.reconnecting') :
    status === 'offline' ? t('term.offline') :
    status === 'ended' || status === 'gone' ? t('term.endedShort') : ''

  const endText = status === 'gone' ? t('term.ended.gone') :
    end ? (end.state === 'exited' ? t('term.ended.exited', { code: end.code }) : end.state === 'lost' ? t('term.ended.lost') : t('term.ended.closed')) : ''

  return (
    <div
      className="term-root"
      ref={rootRef}
      onDragOver={(e) => {
        if (e.dataTransfer.types.includes('Files')) {
          e.preventDefault()
          setDragOver(true)
        }
      }}
      onDragLeave={(e) => e.currentTarget === e.target && setDragOver(false)}
      onDrop={(e) => {
        e.preventDefault()
        setDragOver(false)
        const files = [...e.dataTransfer.files]
        if (files.length) void upload(files)
      }}
    >
      <header className="term-bar">
        <button className="icon-btn" onClick={() => navigate('/')} aria-label={t('term.backToHosts')} title={t('term.backToHosts')}>
          <ArrowLeft size={20} />
        </button>
        <div className="term-title">
          <span className="term-host">{info?.host_name ?? ''}</span>
          <span className={'term-status s-' + status}>
            <span className="dot" aria-hidden="true" />
            {statusText || (info?.title ?? '')}
          </span>
        </div>
        <button className="icon-btn" onClick={() => fileRef.current?.click()} aria-label={t('term.upload')} title={t('term.upload')}>
          <ImagePlus size={19} />
        </button>
        <button className="icon-btn" aria-pressed={composer} onClick={toggleComposer} aria-label={t('term.composer')} title={t('term.composer')}>
          <MessageSquareText size={19} />
        </button>
        <Menu
          button={(open, toggle) => (
            <button className="icon-btn" aria-label={t('common.more')} aria-expanded={open} onClick={toggle}>
              <MoreVertical size={19} />
            </button>
          )}
        >
          {(close) => (
            <>
              <button onClick={() => { close(); openCopyMode() }}><TextSelect size={16} />{t('term.copyMode')}</button>
              <button onClick={() => setPrefs({ fontSize: Math.min(28, prefs.fontSize + 1) })}><Plus size={16} />{t('term.fontLarger')}</button>
              <button onClick={() => setPrefs({ fontSize: Math.max(9, prefs.fontSize - 1) })}><Minus size={16} />{t('term.fontSmaller')}</button>
              {isTouch && (
                <button onClick={() => { close(); setPrefs({ keybar: !prefs.keybar }) }}>
                  <KeyboardIcon size={16} />
                  {prefs.keybar ? t('term.hideKeybar') : t('term.showKeybar')}
                </button>
              )}
              <hr />
              <button
                className="danger"
                onClick={() => {
                  close()
                  setConfirm(
                    <Confirm
                      title={t('term.closeSession')}
                      body={t('hosts.closeSessionConfirm')}
                      confirm={t('term.closeSession')}
                      danger
                      onClose={() => setConfirm(null)}
                      onConfirm={async () => {
                        await api('DELETE', '/api/sessions/' + id)
                        navigate('/', true)
                      }}
                    />,
                  )
                }}
              >
                <Power size={16} />
                {t('term.closeSession')}
              </button>
            </>
          )}
        </Menu>
      </header>

      <div className="term-body">
        <div className="term-host-el" ref={hostEl} />
        {endText && (
          <div className="term-ended" role="alert">
            <p>{endText}</p>
            <div className="dialog-actions">
              <button className="btn" onClick={() => navigate('/')}>{t('term.backToHosts')}</button>
              {hostIdForReconnect > 0 && <button className="btn btn-primary" onClick={reconnect}>{t('term.reopen')}</button>}
            </div>
          </div>
        )}
        {dragOver && <div className="term-drop">{t('term.dropHint')}</div>}
      </div>

      {composer && (
        <div className="composer">
          <textarea
            ref={composerRef}
            className="composer-input"
            value={draft}
            aria-label={t('term.composer')}
            placeholder={t('term.composerPlaceholder')}
            rows={Math.min(6, Math.max(1, draft.split('\n').length))}
            onChange={(e) => setDraft(e.target.value)}
            onKeyDown={(e) => {
              if (e.nativeEvent.isComposing || e.keyCode === 229) return
              if (e.key === 'Enter' && (e.metaKey || e.ctrlKey)) {
                e.preventDefault()
                sendComposer(true)
              }
            }}
          />
          <div className="composer-actions">
            <button className="btn" onClick={() => sendComposer(false)} disabled={!draft}>{t('term.insert')}</button>
            <button className="btn btn-primary" onClick={() => sendComposer(true)}>{t('term.send')}</button>
          </div>
        </div>
      )}

      {showKeybar && (
        <KeyBar
          ctrl={ctrl}
          alt={alt}
          fn={fn}
          onMod={(m) => (m === 'ctrl' ? setCtrl(cycle(ctrl)) : setAlt(cycle(alt)))}
          onFn={() => setFn((f) => !f)}
          onKey={sendKey}
          onText={sendText}
          onKeyboard={toggleKeyboard}
        />
      )}

      <input
        ref={fileRef}
        type="file"
        multiple
        hidden
        onChange={(e) => {
          const files = [...(e.target.files ?? [])]
          e.target.value = ''
          if (files.length) void upload(files)
        }}
      />

      {copyText !== null && <CopyMode text={copyText} onClose={() => setCopyText(null)} />}
      {confirm}
      {dialogs}
    </div>
  )
}

// Copy mode shows the scrollback as real text, so native selection works on touch screens.
function CopyMode({ text, onClose }: { text: string; onClose: () => void }) {
  const toast = useToast()
  const ref = useFocusTrap<HTMLDivElement>(onClose)
  return (
    <div className="copy-mode" role="dialog" aria-modal="true" aria-label={t('term.copyMode')} ref={ref}>
      <div className="copy-bar">
        <span className="hint">{t('term.copyModeHint')}</span>
        <div className="spacer" />
        <button
          className="btn"
          onClick={async () => {
            const sel = window.getSelection()?.toString()
            if (await copyToClipboard(sel || text)) toast(t('common.copied'))
          }}
        >
          {t('common.copy')}
        </button>
        <button className="icon-btn" onClick={onClose} aria-label={t('common.close')}><X size={20} /></button>
      </div>
      <pre className="copy-text" tabIndex={0} ref={(el) => { if (el) el.scrollTop = el.scrollHeight }}>{text}</pre>
    </div>
  )
}
