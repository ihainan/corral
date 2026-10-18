import { useCallback, useEffect, useRef, useState } from 'react'
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
import { Confirm, Menu, copyText as copyToClipboard, useToast } from '../components/ui'
import { useConnect } from '../connect'
import { TermConnection, type EndInfo, type Status } from '../terminal/connection'
import { applyMods, specialKey, type Mods, type SpecialKey } from '../terminal/keys'
import { KeyBar, type ModState } from '../terminal/KeyBar'
import { darkTheme, lightTheme } from '../terminal/themes'

const isTouch = window.matchMedia('(pointer: coarse)').matches || navigator.maxTouchPoints > 0
const FONT_STACK = "'JetBrains Mono', 'SFMono-Regular', Menlo, Consolas, 'PingFang SC', 'Hiragino Sans GB', 'Microsoft YaHei', monospace"

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
  const [draft, setDraft] = useState(() => sessionStorage.getItem('corral.draft.' + id) ?? '')
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
          try { localStorage.setItem('corral.sessHost.' + id, String(s.host_id)) } catch { /* ignore */ }
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
    const unicode = new Unicode11Addon()
    term.loadAddon(unicode)
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
    // iOS zooms into inputs smaller than 16px; the helper textarea is invisible anyway.
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
      onReset: () => term.reset(),
      onData: (d) => term.write(d),
      onStatus: setStatus,
      onEnd: setEnd,
      onLatency: setLatency,
    })
    connRef.current = conn
    conn.resize(term.cols, term.rows)

    const dataSub = term.onData((data) => {
      conn.send(applyMods(data, activeMods()))
      releaseOnce()
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
    term.attachCustomKeyEventHandler((e) => {
      if (e.type === 'keydown' && e.key === 'Enter' && e.shiftKey && !e.ctrlKey && !e.altKey && !e.metaKey) {
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
      if (rootRef.current) rootRef.current.style.height = `${Math.round(h)}px`
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

  // Touch: one finger scrolls (or wheels inside full-screen programs), two fingers resize text.
  useEffect(() => {
    const el = hostEl.current!
    let startY = 0
    let lastY = 0
    let lastT = 0
    let acc = 0
    let moved = false
    let velocity = 0
    let pinch: { dist: number; size: number } | null = null
    let inertia = 0

    const cellHeight = () => {
      const term = termRef.current
      return term && term.rows > 0 ? el.clientHeight / term.rows : 18
    }
    const scrollBy = (lines: number) => {
      const term = termRef.current
      const conn = connRef.current
      if (!term || !conn || lines === 0) return
      const n = Math.min(Math.abs(lines), 12)
      if (term.modes.mouseTrackingMode !== 'none') {
        const col = Math.max(1, Math.floor(term.cols / 2))
        const row = Math.max(1, Math.floor(term.rows / 2))
        const btn = lines > 0 ? 65 : 64
        conn.send(`\x1b[<${btn};${col};${row}M`.repeat(n))
      } else if (term.buffer.active.type === 'alternate') {
        const seq = specialKey(lines > 0 ? 'Down' : 'Up', { ctrl: false, alt: false, shift: false }, term.modes.applicationCursorKeysMode)
        conn.send(seq.repeat(Math.min(n, 6)))
      } else {
        term.scrollLines(lines)
      }
    }
    const dist = (t: TouchList) => Math.hypot(t[0].clientX - t[1].clientX, t[0].clientY - t[1].clientY)

    const onStart = (e: TouchEvent) => {
      cancelAnimationFrame(inertia)
      if (e.touches.length === 2) {
        pinch = { dist: dist(e.touches), size: getPrefs().fontSize }
        return
      }
      startY = lastY = e.touches[0].clientY
      lastT = performance.now()
      acc = 0
      moved = false
      velocity = 0
    }
    const onMove = (e: TouchEvent) => {
      if (pinch && e.touches.length === 2) {
        e.preventDefault()
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
      if (lines !== 0) {
        acc -= lines * ch
        scrollBy(lines)
      }
    }
    const onEnd = (e: TouchEvent) => {
      if (pinch) {
        if (e.touches.length < 2) pinch = null
        return
      }
      if (!moved) return
      e.preventDefault()
      let v = -velocity * 16 // px per frame
      const ch = cellHeight()
      let rest = 0
      const step = () => {
        v *= 0.94
        if (Math.abs(v) < 0.6) return
        rest += v
        const lines = Math.trunc(rest / ch)
        if (lines !== 0) {
          rest -= lines * ch
          scrollBy(lines)
        }
        inertia = requestAnimationFrame(step)
      }
      if (Math.abs(v) > 2) inertia = requestAnimationFrame(step)
    }
    el.addEventListener('touchstart', onStart, { passive: true, capture: true })
    el.addEventListener('touchmove', onMove, { passive: false, capture: true })
    el.addEventListener('touchend', onEnd, { passive: false, capture: true })
    return () => {
      cancelAnimationFrame(inertia)
      el.removeEventListener('touchstart', onStart, { capture: true })
      el.removeEventListener('touchmove', onMove, { capture: true })
      el.removeEventListener('touchend', onEnd, { capture: true })
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
          termRef.current?.paste(r.path + ' ')
          toast(t('term.uploaded'))
        } catch (e) {
          toast(errorText(e), true)
        }
      }
      if (!isTouch) termRef.current?.focus()
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

  const sendComposer = (submit: boolean) => {
    const text = draft
    if (!text && !submit) return
    if (text) termRef.current?.paste(text)
    if (submit) connRef.current?.send('\r')
    setDraft('')
    sessionStorage.removeItem('corral.draft.' + id)
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

  const hostIdForReconnect = info?.host_id ?? Number(localStorage.getItem('corral.sessHost.' + id) || 0)
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
        <button className="icon-btn" aria-pressed={composer} onClick={() => { setComposer((c) => !c); setTimeout(() => composerRef.current?.focus(), 50) }} aria-label={t('term.composer')} title={t('term.composer')}>
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
            placeholder={t('term.composerPlaceholder')}
            rows={Math.min(6, Math.max(1, draft.split('\n').length))}
            onChange={(e) => {
              setDraft(e.target.value)
              sessionStorage.setItem('corral.draft.' + id, e.target.value)
            }}
            onKeyDown={(e) => {
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

      {copyText !== null && (
        <div className="copy-mode" role="dialog" aria-label={t('term.copyMode')}>
          <div className="copy-bar">
            <span className="hint">{t('term.copyModeHint')}</span>
            <div className="spacer" />
            <button className="btn" onClick={async () => { const sel = window.getSelection()?.toString(); if (await copyToClipboard(sel || copyText)) toast(t('common.copied')) }}>{t('common.copy')}</button>
            <button className="icon-btn" onClick={() => setCopyText(null)} aria-label={t('common.close')}><X size={20} /></button>
          </div>
          <pre className="copy-text" ref={(el) => { if (el) el.scrollTop = el.scrollHeight }}>{copyText}</pre>
        </div>
      )}
      {confirm}
      {dialogs}
    </div>
  )
}
