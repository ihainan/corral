import { useCallback, useEffect, useLayoutEffect, useRef, useState } from 'react'
import { flushSync } from 'react-dom'
import { Terminal } from '@xterm/xterm'
import { FitAddon } from '@xterm/addon-fit'
import { Unicode11Addon } from '@xterm/addon-unicode11'
import { WebLinksAddon } from '@xterm/addon-web-links'
import { WebglAddon } from '@xterm/addon-webgl'
import { ArrowUp, Check, ChevronDown, ChevronLeft, CircleStop, Code2, Globe, LayoutGrid, ImagePlus, MessageSquareText, MoreVertical, Minus, Palette, Paperclip, Plus, TextSelect, Keyboard as KeyboardIcon, Power, RotateCw, Server, X } from 'lucide-react'
import { api, ApiError, errorText, uploadFile, type HerdrSummary, type Host, type SessionInfo } from '../api'
import { t } from '../i18n'
import { navigate } from '../router'
import { getPrefs, setChromeColor, setPrefs, termScheme, usePrefs } from '../prefs'
import { local } from '../storage'
import { Confirm, Menu, copyText as copyToClipboard, useFocusTrap, useToast, type MenuItem } from '../components/ui'
import { useConnect } from '../connect'
import { clampSize, TermConnection, type EndInfo, type Status } from '../terminal/connection'
import { applyMods, specialKey, type Mods, type SpecialKey } from '../terminal/keys'
import { KeyBar, type ModState } from '../terminal/KeyBar'
import { chromeVars } from '../terminal/themes'
import { SchemeDialog } from '../terminal/SchemePicker'
import { HerdrPanel } from '../terminal/HerdrPanel'
import { PortsDialog, openForward } from '../terminal/PortsDialog'
import { useCode } from '../terminal/CodeDialog'
import { useSession } from '../app'

const isTouch = window.matchMedia('(pointer: coarse)').matches || navigator.maxTouchPoints > 0
const isMac = /Mac|iPhone|iPad/.test(navigator.platform)
const HIGH_LATENCY = 300 // ms; below this the status just says "connected"

function useMedia(q: string) {
  const [m, setM] = useState(() => window.matchMedia(q).matches)
  useEffect(() => {
    const mq = window.matchMedia(q)
    const on = () => setM(mq.matches)
    mq.addEventListener('change', on)
    return () => mq.removeEventListener('change', on)
  }, [q])
  return m
}
const FONT_STACK = "'JetBrains Mono', 'SFMono-Regular', Menlo, Consolas, 'PingFang SC', 'Hiragino Sans GB', 'Microsoft YaHei', monospace"

// Quote a remote path for a POSIX shell when it contains anything beyond safe
// characters. Generated upload names are safe; the home directory may not be.
// While replayed output is parsed, xterm answers the old queries in it. The
// answers have fixed shapes: device attributes and status (CSI … c / n / R),
// mode reports (CSI … $ y), window reports (CSI … t), focus (CSI I / O),
// colour and DCS reports. This is only the fallback for telling them apart
// from typing, see userInputEvent.
const REPLY = /^(?:\x1b\[[?>]?[\d;]*[cnRt]|\x1b\[\??[\d;]*\$y|\x1b\[[IO]|\x1bP[\s\S]*?\x1b\\|\x1b\][\s\S]*?(?:\x07|\x1b\\))+$/
// The button code of a mouse report (SGR or X10/default encoding), or null.
// A wheel is buttons 64-67, with any modifier bits.
function mouseButton(data: string): number | null {
  const sgr = /^\x1b\[<(\d+);\d+;\d+[Mm]$/.exec(data)
  if (sgr) return Number(sgr[1])
  return /^\x1b\[M...$/s.test(data) ? data.charCodeAt(3) - 32 : null
}
const isMouseReport = (data: string) => mouseButton(data) !== null
const isWheelReport = (data: string) => {
  const b = mouseButton(data)
  return b !== null && (b & 0xc0) === 0x40
}

export function isTerminalReply(data: string): boolean {
  return REPLY.test(data)
}

// xterm marks data it sends because of the user (keys, paste, IME, mouse)
// with an internal onUserInput event fired right before onData; its own
// replies come without it. That is the exact signal, but not public API, so
// it is looked up defensively (checked against @xterm/xterm 6.0.0).
type Listen = (cb: () => void) => { dispose(): void }
function userInputEvent(term: Terminal): Listen | null {
  const ev = (term as unknown as { _core?: { coreService?: { onUserInput?: Listen } } })._core?.coreService?.onUserInput
  return typeof ev === 'function' ? ev : null
}

// Resolves once no composition is in progress (at once if none is). After a
// compositionend the check is repeated once xterm has sent it, since a new
// composition may already have started (Korean does this between syllables).
function untilNotComposing(composingNow: () => boolean, ta: HTMLTextAreaElement | null): Promise<void> {
  if (!composingNow() || !ta) return Promise.resolve()
  return new Promise((resolve) => {
    const check = () => setTimeout(() => {
      if (composingNow() && document.activeElement === ta) return // wait for the next end
      ta.removeEventListener('compositionend', check)
      ta.removeEventListener('blur', check)
      resolve()
    }, 0)
    ta.addEventListener('compositionend', check)
    ta.addEventListener('blur', check)
  })
}

// The first image on the clipboard as a file, or null if there is none or
// the browser refuses (it may ask for permission the first time).
async function clipboardImage(): Promise<File | null> {
  try {
    for (const item of await navigator.clipboard.read()) {
      const type = item.types.find((t) => t.startsWith('image/'))
      if (type) {
        const blob = await item.getType(type)
        return new File([blob], 'clipboard.' + (type.split('/')[1] || 'png').replace('jpeg', 'jpg'), { type })
      }
    }
  } catch {
    /* no permission, or nothing readable */
  }
  return null
}

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
  // Text from the key bar or an upload goes in at the composer's cursor.
  const insertDraft = useCallback((s: string) => {
    const el = composerRef.current
    if (!el) return setDraft((d) => d + s) // closed meanwhile: the draft keeps it
    const a = el.selectionStart ?? el.value.length
    const b = el.selectionEnd ?? a
    setDraft(el.value.slice(0, a) + s + el.value.slice(b))
    requestAnimationFrame(() => {
      el.selectionStart = el.selectionEnd = a + s.length
    })
  }, [])
  const [copyText, setCopyText] = useState<string | null>(null)
  const [dragOver, setDragOver] = useState(false)
  const [confirm, setConfirm] = useState<React.ReactNode>(null)
  const [herdr, setHerdr] = useState<HerdrSummary | null>(null)
  // Docked on wide screens (remembered, off by default: herdr draws its own
  // sidebar too), a sheet on narrow ones (always starts closed).
  const sheetPanel = useMedia('(max-width: 999px)')
  const [panel, setPanelState] = useState(() => !sheetPanel && local.get('corral.herdrPanel') === '1')
  // The form the panel was opened in. A panel is only drawn in that form, so
  // crossing the breakpoint never mounts the other form, even for one render.
  const [panelSheet, setPanelSheet] = useState(sheetPanel)
  const setPanel = (v: boolean) => {
    setPanelState(v)
    setPanelSheet(sheetPanel)
    if (!sheetPanel) local.set('corral.herdrPanel', v ? '1' : '0')
    if (!v && !isTouch) termRef.current?.focus()
  }
  const [sessions, setSessions] = useState<SessionInfo[] | null>(null)
  const [imeLines, setImeLines] = useState<string[]>([])
  const [sessionsFailed, setSessionsFailed] = useState(false)
  // False from a disconnect until this connection sends a new herdr summary.
  const [herdrFresh, setHerdrFresh] = useState(true)
  // Crossing the width where the panel turns from docked to a sheet closes it,
  // so it never reappears in another form above a dialog that is open.
  const firstSheet = useRef(sheetPanel)
  useEffect(() => {
    if (firstSheet.current === sheetPanel) return
    firstSheet.current = sheetPanel
    setPanelState(false)
  }, [sheetPanel])
  const ended = status === 'ended' || status === 'gone' || end !== null
  const showKeybar = isTouch && prefs.keybar && !ended
  const panelOpen = panel && panelSheet === sheetPanel
  const { boot } = useSession()
  const [portsOpen, setPortsOpen] = useState(false)
  const code = useCode()
  const refreshCode = code.refresh
  const codeHost = boot.code ? info?.host_id : undefined
  useEffect(() => {
    if (codeHost) refreshCode([codeHost])
  }, [codeHost, refreshCode])
  const [schemesOpen, setSchemesOpen] = useState(false)
  const scheme = termScheme()
  // Links to localhost in the terminal open through port forwarding.
  const linkTarget = useRef<(uri: string) => void>(() => {})

  // Latest modifier state for the xterm data callback.
  const openComposerRef = useRef<() => void>(() => {})
  const settleRef = useRef<() => void>(() => {})
  const takeOverRef = useRef<() => void>(() => {})
  const [following, setFollowing] = useState<{ cols: number; rows: number } | null>(null)
  const outRef = useRef<(d: string | Uint8Array) => void>(() => {})
  const safeFitRef = useRef(() => {})
  const heldRef = useRef<() => boolean>(() => false) // input is waiting for a Ctrl+V clipboard check or upload
  const composingRef = useRef<() => boolean>(() => false)
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
      lineHeight: isTouch ? 1.2 : 1.25,
      cursorBlink: true,
      scrollback: 10000,
      allowProposedApi: true,
      macOptionIsMeta: p.optionIsMeta,
      rescaleOverlappingGlyphs: true,
      theme: termScheme().theme,
      linkHandler: { activate: (_e, uri) => linkTarget.current(uri) },
    })
    term.loadAddon(new Unicode11Addon())
    term.unicode.activeVersion = '11'
    const fit = new FitAddon()
    term.loadAddon(fit)
    term.loadAddon(new WebLinksAddon((_e, uri) => linkTarget.current(uri)))
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
    const ta = el.querySelector('textarea')
    ta?.setAttribute('autocapitalize', 'off')
    ta?.setAttribute('autocorrect', 'off')
    ta?.setAttribute('spellcheck', 'false')

    // Set while another viewer's size is shown (see onSize); fitting to this
    // window then waits until this viewer takes the terminal over.
    let following: { cols: number; rows: number } | null = null
    // Whether the gateway last said this window is in charge of the size,
    // and when this window last claimed it.
    let inCharge = false
    let claimedAt = 0
    let claimTimer: number | undefined
    // The user acted here (typed, pasted, clicked): this window takes the
    // terminal over. While it is in charge the claim is repeated at most once
    // a second (an action within the second is claimed when it ends), in case
    // another window took over meanwhile and the news has not arrived yet:
    // the latest action at the gateway wins.
    takeOverRef.current = () => {
      const now = Date.now()
      if (!following && inCharge && now - claimedAt < 1000) {
        claimTimer ??= window.setTimeout(() => {
          claimTimer = undefined
          takeOverRef.current()
        }, claimedAt + 1000 - now)
        return
      }
      clearTimeout(claimTimer)
      claimTimer = undefined
      claimedAt = now
      inCharge = true
      if (following) {
        following = null
        setFollowing(null)
        applyScale()
        safeFit()
      }
      conn.resize(term.cols, term.rows, true) // tell the host even if nothing changed locally
    }
    const safeFit = () => {
      if (following) return void requestAnimationFrame(applyScale)
      try {
        fit.fit()
      } catch {
        /* not visible yet */
      }
    }
    safeFitRef.current = safeFit
    // Another viewer's grid can be larger than this window (a desktop's on a
    // phone): it is shown scaled down to fit, so all of it can be seen. At
    // this window's own size nothing is scaled.
    let scale = 1
    const applyScale = () => {
      const x = el.querySelector<HTMLElement>('.xterm')
      const screen = el.querySelector<HTMLElement>('.xterm-screen')
      if (!x || !screen) return
      const cs = getComputedStyle(el)
      const w = el.clientWidth - parseFloat(cs.paddingLeft) - parseFloat(cs.paddingRight)
      const h = el.clientHeight - parseFloat(cs.paddingTop) - parseFloat(cs.paddingBottom)
      const next = following && screen.offsetWidth > 0 && screen.offsetHeight > 0
        ? Math.min(1, w / screen.offsetWidth, h / screen.offsetHeight)
        : 1
      if (next === scale) return
      scale = next
      x.style.transformOrigin = '0 0'
      x.style.transform = scale < 1 ? `scale(${scale})` : ''
    }
    safeFit()
    // Cell size is measured when the font is set. The web font may still be
    // loading at that point (glyphs then get the fallback's width), so set the
    // font again once it is ready, which makes xterm measure afresh.
    void Promise.all([
      document.fonts?.load(`${p.fontSize}px "JetBrains Mono"`),
      document.fonts?.load(`bold ${p.fontSize}px "JetBrains Mono"`),
    ])
      .then(() => {
        if (termRef.current !== term) return
        term.options.fontFamily = FONT_STACK + ', monospace'
        safeFit()
      })
      .catch(() => {})

    // While replayed output is being parsed, xterm answers old queries found in
    // it (device attributes, cursor position…). Those answers are stale and
    // must not reach the host as typing; they are dropped until the replay ends.
    let replaying = false
    let lastDropToast = 0
    const droppedInput = () => {
      const now = Date.now()
      if (now - lastDropToast < 5000) return
      lastDropToast = now
      toast(t('term.inputDropped'), true)
    }
    const conn = new TermConnection(id, {
      // RIS (ESC c) travels in the same write queue as the replay that follows,
      // so output still being parsed cannot land after the reset.
      onReset: () => {
        replaying = true
        try {
          term.write('\x1bc')
          return true
        } catch {
          return false
        }
      },
      onData: (d, backlog) => {
        try {
          if (backlog) replaying = true
          term.write(d)
          return true
        } catch {
          return false // xterm's write buffer is full; resume later from here
        }
      },
      onStatus: setStatus,
      onEnd: setEnd,
      onLatency: setLatency,
      onSize: (cols, rows, own) => {
        inCharge = own
        const mine = fit.proposeDimensions()
        if (own || !mine || (mine.cols === cols && mine.rows === rows)) {
          // This window's size (it set it, or it is the same grid anyway):
          // fit to this window, in case another viewer's size was shown
          // until now.
          if (following) {
            following = null
            setFollowing(null)
            applyScale()
          }
          safeFit()
          // In charge, but the gateway has another size than this window now
          // shows (it reported one from before the window changed): report
          // the current one, unless a report is already on its way.
          const shown = clampSize(term.cols, term.rows) // a larger window reports the largest size
          if (own && (shown.cols !== cols || shown.rows !== rows) && resizeTimer === undefined) conn.resize(term.cols, term.rows)
          return
        }
        // Another viewer set this size: show the terminal at it, so what the
        // programs draw for that size lines up, until this one takes over.
        following = { cols, rows }
        setFollowing({ cols, rows })
        if (term.cols !== cols || term.rows !== rows) term.resize(cols, rows)
        requestAnimationFrame(applyScale)
      },
      onMeta: (m) => {
        setHerdr(m as HerdrSummary)
        setHerdrFresh(true)
      },
      onLive: () => term.write('', () => (replaying = false)),
      // Every new socket: the herdr summary is old until this one sends its own.
      onHello: () => setHerdrFresh(false),
      // Input typed while disconnected is sent once the connection is back,
      // unless there was too much of it or it waited too long.
      onDropped: () => droppedInput(),
    })
    connRef.current = conn
    conn.resize(term.cols, term.rows) // sent once connected

    // Everything typed goes out through here, so it can wait its turn while
    // Ctrl+V checks the clipboard and uploads an image: keys typed meanwhile
    // (Enter right after pasting a screenshot) go out after the image's path,
    // and several Ctrl+V are handled one after another, in order.
    type Held = string | Uint8Array | { clip: Promise<File | null> }
    let held: Held[] | null = null
    let heldSince = 0 // the connection's open count when input started waiting
    let draining = false
    let direct = false // the path of the image being handled skips the queue
    // Held input goes out only over the connection it was typed on: after a
    // reconnect the screen may have changed (as for input typed offline).
    const stillSame = () => conn.isOpen() && conn.opens === heldSince
    const send = (d: string | Uint8Array) => void conn.send(d)
    const out = (d: string | Uint8Array) => {
      if (held && !direct) held.push(d)
      else send(d)
    }
    outRef.current = out
    heldRef.current = () => held !== null
    const pasteDirect = (s: string) => {
      if (!stillSame()) return droppedInput()
      direct = true
      try {
        term.paste(s)
      } finally {
        direct = false
      }
    }
    const drain = async () => {
      if (draining) return
      draining = true
      try {
        while (held && held.length > 0) {
          if (!stillSame()) {
            droppedInput()
            break
          }
          const d = held[0]
          if (typeof d === 'object' && 'clip' in d) {
            const file = await d.clip
            if (!stillSame()) {
              droppedInput()
              break
            }
            if (file) await uploadRef.current([file], pasteDirect)
            else send('\x16')
          } else {
            send(d)
          }
          held?.shift()
        }
      } finally {
        held = null
        draining = false
      }
    }

    // Latched Ctrl/Alt from the key bar apply only to what the user typed:
    // a character applies when xterm emits exactly that character for the key
    // just pressed; special keys (Enter, arrows, Tab…) are encoded here. Pastes,
    // input-method results and the terminal's own replies pass through untouched.
    let typedChar: string | null = null
    let composing = false
    const latched = () => mods.current.ctrl !== 'off' || mods.current.alt !== 'off'
    const onCompStart = () => {
      dropClaims() // the key became part of a composition
      composing = true
      typedChar = null
    }
    const onCompEnd = () => {
      composing = false
      typedChar = null
    }
    // iOS soft keyboards report printable keys through input events.
    // Printable keys are sent as the text the browser actually inserts, not
    // as the character the key reports. With an input method active the two
    // differ: a Chinese input method reports keydown "," and inserts "，". xterm
    // sends the reported character on keydown and cancels the event, so the
    // inserted mark was lost. Now a plain printable keydown is left to the
    // browser, and the insertText it produces next is sent (and kept from
    // xterm, which would send it again). The browser emits these in key order,
    // so nothing has to be guessed or settled. Keys with Ctrl/Alt/Meta, latched
    // key-bar modifiers, special keys, compositions, and the key right after a
    // dead key or AltGraph (whose state xterm tracks itself) take xterm's path.
    // xterm internals this relies on (checked against @xterm/xterm 6.0.0): the
    // composition helper, so a finished composition is sent before the next
    // key, and the dead-key flag, so xterm keeps handling the key after one.
    type Core = { _compositionHelper?: { keydown(e: KeyboardEvent): boolean; isComposing?: boolean }; _unprocessedDeadKey?: boolean }
    const core = (term as unknown as { _core?: Core })._core
    const internalsOk = !!core && typeof core._compositionHelper?.keydown === 'function' && typeof core._unprocessedDeadKey === 'boolean'
    // The key left to the browser, identified by its physical code, until its
    // text arrives, it is released, or another key is pressed. Browsers insert
    // a key's text (an input method's commit included) before the next key
    // event, so a claim never spans keys; text arriving outside one is
    // ordinary input for xterm.
    let claim: string | null = null
    const IME_KEY = '\u0000ime' // a claim made for a 229 key (its event.code is often empty)
    // After a 229 key xterm reads its textarea change in a zero-delay timer;
    // no punctuation is claimed until that has run. A timer queued from inside
    // our own zero-delay timer runs after xterm's (timers of equal delay run in
    // order), which is the completion signal.
    let imeKeyPending = 0
    let lastKeyAlt = false // the last key was pressed with Option/Alt
    const onPrintableInput = (e: Event) => {
      const ie = e as InputEvent
      if (e.target !== ta || claim === null || ie.inputType !== 'insertText' || !ie.data || ie.isComposing || composing) return
      claim = null
      e.stopPropagation() // ours alone; xterm must not send it as well
      // Take exactly the inserted text back out of the textarea: xterm never
      // has typed characters in it (its composition handling compares the
      // textarea's contents), and anything else in it is left alone.
      const t = ta as HTMLTextAreaElement
      const end = t.selectionStart
      const start = end - ie.data.length
      if (start >= 0 && t.value.slice(start, end) === ie.data) {
        t.value = t.value.slice(0, start) + t.value.slice(end)
        t.selectionStart = t.selectionEnd = start
      }
      term.input(ie.data, true)
    }
    const dropClaims = () => {
      claim = null
      lastKeyAlt = false
    }
    // Before anything is sent past xterm (key bar, Shift+Enter, latched
    // modifiers, composer, upload paths): send a composition that just ended
    // and clear xterm's dead-key flag, as xterm does for a key it handles.
    // A composition still in progress is never forced out: only one that has
    // ended and is waiting to be sent is.
    settleRef.current = () => {
      if (!internalsOk) return
      if (!composing && !core!._compositionHelper!.isComposing) core!._compositionHelper!.keydown(new KeyboardEvent('keydown'))
      core!._unprocessedDeadKey = false
      claim = null
    }
    composingRef.current = () => composing || (internalsOk && !!core!._compositionHelper!.isComposing)

    const onBeforeInput = (e: Event) => {
      const ie = e as InputEvent
      typedChar = !lastKeyAlt && !ie.isComposing && ie.inputType === 'insertText' && ie.data && [...ie.data].length === 1 ? ie.data : null
    }
    const onPasteEv = () => (typedChar = null)
    ta?.addEventListener('compositionstart', onCompStart)
    ta?.addEventListener('compositionend', onCompEnd)
    ta?.addEventListener('beforeinput', onBeforeInput)
    // Capture on the container: it runs before any listener on the textarea,
    // xterm's own (also capturing) included, so a claimed event stops here.
    el.addEventListener('input', onPrintableInput, true)
    ta?.addEventListener('blur', dropClaims)
    ta?.addEventListener('paste', onPasteEv, true)
    const onUserInput = userInputEvent(term)
    let userData = false
    const userSub = onUserInput?.(() => (userData = true))
    // Diagnostics for input methods on devices we cannot reproduce: with
    // #imedebug in the address, the last events and what was sent are shown.
    const imeLog = location.hash.includes('imedebug') ? (line: string) => setImeLines((l) => [...l.slice(-29), line]) : null
    if (imeLog && ta) {
      const describe = (e: Event) => {
        const k = e as KeyboardEvent & InputEvent & CompositionEvent
        const parts = [e.type, k.key !== undefined ? 'key=' + JSON.stringify(k.key) : '', k.keyCode ? 'kc=' + k.keyCode : '',
          k.inputType ? 'it=' + k.inputType : '', k.data != null ? 'data=' + JSON.stringify(k.data) : '', k.isComposing ? 'composing' : '',
          'val=' + JSON.stringify(ta.value.slice(-12))]
        imeLog(parts.filter(Boolean).join(' '))
      }
      for (const t of ['keydown', 'keyup', 'beforeinput', 'input', 'compositionstart', 'compositionupdate', 'compositionend']) ta.addEventListener(t, describe, true)
    }
    const dataSub = term.onData((data) => {
      imeLog?.('→ sent ' + JSON.stringify(data))
      const typed = typedChar !== null && data === typedChar && !composing
      const fromUser = onUserInput ? userData : !isTerminalReply(data)
      typedChar = null
      userData = false
      // xterm's own answers (to queries, focus changes) during a replay are
      // stale; while another viewer's size is shown, that viewer answers.
      if (!fromUser && (replaying || following)) return
      // Typing, pasting or clicking here takes the terminal over; scrolling
      // with the wheel only scrolls, at the size being shown. A click on a
      // scaled-down grid lands elsewhere than it looks, so it only takes over.
      if (fromUser && !isWheelReport(data)) {
        const scaledClick = following && scale < 1 && isMouseReport(data)
        takeOverRef.current()
        if (scaledClick) return
      }
      if (typed && latched()) {
        out(applyMods(data, activeMods()))
        releaseOnce()
      } else {
        out(data)
      }
    })
    // Mouse reports in the default encoding come here; they are the user's.
    const binSub = term.onBinary((data) => {
      if (!isWheelReport(data)) {
        const scaledClick = following && scale < 1
        takeOverRef.current()
        if (scaledClick) return
      }
      const bytes = new Uint8Array(data.length)
      for (let i = 0; i < data.length; i++) bytes[i] = data.charCodeAt(i) & 0xff
      out(bytes)
    })
    let resizeTimer: number | undefined
    const resizeSub = term.onResize(({ cols, rows }) => {
      if (following) return // showing another viewer's size; not ours to report
      // Keyboard animations fire many resizes; tell the host once things settle.
      clearTimeout(resizeTimer)
      resizeTimer = window.setTimeout(() => {
        resizeTimer = undefined
        if (!following) conn.resize(cols, rows)
      }, 120)
    })
    const specialFor: Record<string, SpecialKey> = {
      Enter: 'Enter', Tab: 'Tab', Escape: 'Esc', Backspace: 'Backspace', Delete: 'Delete', Insert: 'Insert',
      ArrowUp: 'Up', ArrowDown: 'Down', ArrowLeft: 'Left', ArrowRight: 'Right', Home: 'Home', End: 'End',
      PageUp: 'PageUp', PageDown: 'PageDown', F1: 'F1', F2: 'F2', F3: 'F3', F4: 'F4', F5: 'F5', F6: 'F6',
      F7: 'F7', F8: 'F8', F9: 'F9', F10: 'F10', F11: 'F11', F12: 'F12',
    }
    term.attachCustomKeyEventHandler((e) => {
      // The keypress of the claimed key: its input event carries the text.
      if (e.type === 'keypress' && claim !== null && e.code === claim) return false
      if (e.type === 'keyup') {
        if (claim !== null && (e.code === claim || (claim === IME_KEY && e.keyCode === 229))) claim = null
        if (e.key === 'Alt') lastKeyAlt = false
        return true
      }
      if (e.type !== 'keydown') return true
      if (e.isComposing || e.keyCode === 229) {
        claim = null
        lastKeyAlt = false
        // iOS keyboards (Chinese among them) commit a candidate as a 229
        // keydown followed by a plain insertText, with no composition at all.
        // xterm then relies on comparing the textarea before and after, which
        // on iOS Safari sent nothing (or a stray DEL). Outside a composition
        // the inserted text is claimed and sent like a punctuation key's, and
        // taken back out of the textarea, so xterm's comparison finds nothing.
        if (!e.isComposing && !composing && internalsOk && !core!._compositionHelper!.isComposing && !term.options.screenReaderMode) {
          claim = IME_KEY
        }
        imeKeyPending++
        setTimeout(() => setTimeout(() => imeKeyPending--, 0), 0)
        return true
      }
      const modifierOnly = ['Shift', 'Control', 'Alt', 'Meta', 'CapsLock', 'AltGraph'].includes(e.key)
      if (!modifierOnly) {
        claim = null
        lastKeyAlt = e.altKey
      }
      // Ctrl+V with an image on the clipboard uploads it and types its path,
      // as Claude Code and Codex do with a local clipboard (they cannot reach
      // this browser's). With no image, or no clipboard access, ^V goes to
      // the host as before (vim and readline use it).
      if (e.ctrlKey && !e.metaKey && !e.altKey && !e.shiftKey && e.code === 'KeyV' && !latched() && typeof navigator.clipboard?.read === 'function') {
        e.preventDefault()
        // Keys typed while the clipboard is checked wait, so ^V keeps its place
        // (Ctrl+V then Enter in vim inserts a literal CR).
        takeOverRef.current()
        settleRef.current()
        if (!held) {
          held = []
          heldSince = conn.opens
        }
        held.push({ clip: Promise.race([clipboardImage(), new Promise<null>((r) => setTimeout(() => r(null), 1500))]) })
        void drain()
        return false
      }
      // Only punctuation: that is what input methods turn into full-width marks
      // without a composition. Letters and digits keep xterm's path.
      if (
        internalsOk && !term.options.screenReaderMode && !core!._unprocessedDeadKey &&
        /^[!-/:-@[-`{-~]$/.test(e.key) && e.code &&
        !e.ctrlKey && !e.altKey && !e.metaKey && !e.getModifierState('AltGraph') && !latched() &&
        ta!.selectionStart === ta!.selectionEnd &&
        imeKeyPending === 0
      ) {
        // What xterm does first for every key: send a composition that just
        // ended. If it says the key belongs to a composition, leave it to xterm.
        if (core!._compositionHelper!.keydown(e)) {
          claim = e.code
          return false // no preventDefault: the browser (and input method) insert the text
        }
        return true
      }
      // Text made with Option/Alt (when it is not Meta) is a character of its
      // own, not a key to apply latched key-bar modifiers to.
      typedChar = e.key.length === 1 && !e.metaKey && !e.ctrlKey && !e.altKey ? e.key : null
      // A physical Ctrl/Alt chord while a key-bar modifier is latched: combine both.
      // Clipboard shortcuts, AltGr and text-producing Option are left alone.
      const physAlt = e.altKey && !e.getModifierState('AltGraph') && (!isMac || getPrefs().optionIsMeta)
      const pasteKey = e.ctrlKey && e.key.toLowerCase() === 'v'
      if (latched() && e.key.length === 1 && (e.ctrlKey || physAlt) && !e.metaKey && !pasteKey && !e.getModifierState('AltGraph')) {
        const m = activeMods()
        takeOverRef.current()
        settleRef.current()
        out(applyMods(e.key, { ctrl: m.ctrl || e.ctrlKey, alt: m.alt || physAlt, shift: false }))
        releaseOnce()
        e.preventDefault()
        return false
      }
      const special = specialFor[e.key]
      if (special && latched() && !e.metaKey) {
        const m = activeMods()
        takeOverRef.current()
        settleRef.current()
        out(specialKey(special, { ctrl: m.ctrl || e.ctrlKey, alt: m.alt || e.altKey, shift: e.shiftKey }, term.modes.applicationCursorKeysMode))
        releaseOnce()
        e.preventDefault()
        return false
      }
      // Shortcuts handled here cancel the key, or its keypress would reach
      // xterm and send a plain CR as well (submitting the prompt).
      if (e.key === 'Enter' && e.shiftKey && (isMac ? e.metaKey : e.ctrlKey)) {
        e.preventDefault()
        openComposerRef.current()
        return false
      }
      if (e.key === 'Enter' && e.shiftKey && !e.ctrlKey && !e.altKey && !e.metaKey) {
        e.preventDefault()
        takeOverRef.current()
        settleRef.current()
        out('\x1b\r') // newline without submitting, for Claude Code / Codex
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
      clearTimeout(claimTimer)
      ta?.removeEventListener('compositionstart', onCompStart)
      ta?.removeEventListener('compositionend', onCompEnd)
      ta?.removeEventListener('beforeinput', onBeforeInput)
      el.removeEventListener('input', onPrintableInput, true)
      ta?.removeEventListener('blur', dropClaims)
      ta?.removeEventListener('paste', onPasteEv, true)
      dataSub.dispose()
      userSub?.dispose()
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
    const scheme = termScheme()
    term.options.theme = scheme.theme
    setChromeColor(scheme.theme.background ?? null)
    // Menus and dialogs opened over the terminal follow its colours.
    document.documentElement.classList.toggle('term-dark', scheme.dark)
    // Through the follow check: while another viewer's size is shown the grid
    // stays at that size (only its scale changes).
    safeFitRef.current()
  }, [prefs])
  useEffect(
    () => () => {
      setChromeColor(null)
      document.documentElement.classList.remove('term-dark')
    },
    [],
  )

  // An ended session: dim it, stop the cursor, close the herdr panel.
  useEffect(() => {
    if (!ended) return
    const term = termRef.current
    if (term) {
      term.options.cursorBlink = false
      term.options.cursorInactiveStyle = 'none'
      term.blur()
    }
    setPanelState(false)
    setComposer(false)
  }, [ended])

  // Follow the visual viewport so the key bar sits right above the soft
  // keyboard. iOS Safari does not shrink the layout viewport for the keyboard.
  useEffect(() => {
    const vv = window.visualViewport
    const apply = () => {
      // Pinch zoom also shrinks the visual viewport; only an unzoomed one
      // that lost height is the keyboard (as in main.tsx).
      const zoomed = vv ? (vv.scale ?? 1) > 1.01 : false
      const h = vv && !zoomed ? vv.height : window.innerHeight
      const root = rootRef.current
      if (!root) return
      root.style.height = `${Math.round(h)}px`
      // While the keyboard is up iOS may pan the visual viewport; follow it. When
      // it is down, ignore offsetTop: iOS 26 can leave a stale value behind.
      const keyboardUp = vv && !zoomed ? window.innerHeight - vv.height > 120 : false
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
  // Paths go where the files came from: uploads started in the terminal go
  // there (Ctrl+V passes its own paste, a paste sets toTerm); the others
  // (file picker, drop, the composer) go into the composer's draft when it
  // is open.
  const uploadRef = useRef<(files: File[], paste?: (s: string) => void, toTerm?: boolean) => Promise<void>>(async () => {})
  const upload = useCallback(
    async (files: File[], paste?: (s: string) => void, toTerm = false) => {
      // Decided when the upload starts, not when it ends: the target, and
      // for the terminal the connection (after a reconnect the screen may
      // have changed, so the path is shown instead of pasted).
      const toDraft = !paste && !toTerm && !!composerRef.current
      const opens = connRef.current?.opens
      for (const f of files) {
        toast(t('term.uploading', { name: f.name || 'image' }))
        try {
          const r = await uploadFile(id, f)
          const quoted = shellPath(r.path)
          if (quoted && toDraft) {
            insertDraft(quoted + ' ')
            toast(t('term.uploaded'))
          } else if (quoted) {
            // Never paste into a composition in progress: wait for it to end.
            await untilNotComposing(composingRef.current, hostEl.current?.querySelector('textarea') ?? null)
            settleRef.current()
            const s = quoted + ' '
            const c = connRef.current
            if (paste) paste(s)
            else if (!c?.isOpen() || c.opens !== opens) {
              toast(t('term.uploadNotPasted', { path: r.path }), true)
              continue
            } else termRef.current?.paste(s)
            toast(t('term.uploaded'))
          } else {
            toast(r.path)
          }
        } catch (e) {
          toast(errorText(e), true)
        }
      }
      if (!isTouch && !document.querySelector('[aria-modal="true"]')) (composerRef.current ?? termRef.current)?.focus()
    },
    [id, toast, insertDraft],
  )
  uploadRef.current = upload

  useEffect(() => {
    const el = hostEl.current!
    const onPaste = (e: ClipboardEvent) => {
      const files = [...(e.clipboardData?.files ?? [])]
      if (files.length === 0) return
      e.preventDefault()
      e.stopPropagation()
      void upload(files, undefined, true)
    }
    el.addEventListener('paste', onPaste, { capture: true })
    return () => el.removeEventListener('paste', onPaste, { capture: true })
  }, [upload])

  // A key-bar key pressed while an input method is still composing goes out
  // once the composition is committed, so the committed text comes first.
  const afterComposition = (f: () => void) => {
    if (!composingRef.current()) return f()
    void untilNotComposing(composingRef.current, hostEl.current?.querySelector('textarea') ?? null).then(f)
  }
  const sendKey = (k: SpecialKey) =>
    afterComposition(() => {
      takeOverRef.current()
      settleRef.current()
      const term = termRef.current
      outRef.current(specialKey(k, activeMods(), term?.modes.applicationCursorKeysMode ?? false))
      releaseOnce()
    })
  const sendText = (s: string) => {
    // Typing in the composer: symbols go into the draft. Keys (Esc, arrows,
    // Tab, Ctrl…) still go to the terminal, so a running program can be
    // interrupted or steered while a message is being written.
    const m = activeMods()
    if (composerRef.current && document.activeElement === composerRef.current && !m.ctrl && !m.alt) {
      insertDraft(s)
      return
    }
    afterComposition(() => {
      takeOverRef.current()
      settleRef.current()
      outRef.current(applyMods(s, activeMods()))
      releaseOnce()
    })
  }
  const cycle = (m: ModState): ModState => (m === 'off' ? 'once' : m === 'once' ? 'lock' : 'off')
  const toggleKeyboard = () => {
    // With the composer open the keyboard belongs to it.
    const target = composerRef.current ?? hostEl.current?.querySelector('textarea')
    if (target && document.activeElement === target) {
      settleRef.current() // a composition that just ended is sent, not cleared by the blur
      target.blur()
    } else {
      target?.focus()
    }
  }

  // The composer's text and the optional Enter go out as one message, and only
  // while connected; otherwise the draft stays so nothing is half-sent.
  const sendComposer = (submit: boolean) => {
    takeOverRef.current()
    settleRef.current()
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
    // Sent at once or not at all: the draft stays until it surely went out.
    if (heldRef.current()) {
      toast(t('term.waitUpload'), true) // a clipboard image is still being uploaded
      return
    }
    if (!conn.sendNow(payload)) {
      toast(t('term.notConnected'), true)
      return
    }
    setDraft('')
    composerRef.current?.focus()
  }

  const toggleComposer = () => {
    if (composer) {
      setComposer(false)
      if (!isTouch) termRef.current?.focus()
      return
    }
    // Mount and focus inside the tap itself: iOS only raises the keyboard for a
    // focus that happens during the user's gesture.
    flushSync(() => setComposer(true))
    composerRef.current?.focus()
  }
  openComposerRef.current = () => {
    if (ended) return
    flushSync(() => setComposer(true))
    composerRef.current?.focus()
  }

  // The composer grows with its text up to 40% of what is visible, and less
  // on very short screens (a phone on its side with the keyboard up), so the
  // terminal and the key bar keep some room.
  useEffect(() => {
    const el = composerRef.current
    if (!el) return
    const fit = () => {
      const vh = window.visualViewport?.height ?? window.innerHeight
      el.style.height = 'auto'
      el.style.height = Math.min(el.scrollHeight, Math.max(40, Math.min(vh * 0.4, vh - 260))) + 'px'
    }
    fit()
    const vv = window.visualViewport
    vv?.addEventListener('resize', fit)
    return () => vv?.removeEventListener('resize', fit)
  }, [draft, composer])

  const openCopyMode = () => {
    const term = termRef.current
    if (!term) return
    const buf = term.buffer.active
    const lines: string[] = []
    const start = Math.max(0, buf.length - 3000)
    // A soft-wrapped row continues the previous line: join them, so copied
    // commands and URLs come out whole. Trimming drops only cells nothing was
    // printed in (such as the gap a wide character leaves before a wrap);
    // printed spaces stay.
    for (let i = start; i < buf.length; i++) {
      const line = buf.getLine(i)
      const text = line?.translateToString(true) ?? ''
      if (line?.isWrapped && lines.length > 0) lines[lines.length - 1] += text
      else lines.push(text)
    }
    while (lines.length && lines[lines.length - 1] === '') lines.pop()
    setCopyText(lines.join('\n'))
  }

  linkTarget.current = (uri: string) => {
    if (!/^https?:\/\//i.test(uri)) return
    const u = new URL(uri)
    const local = ['localhost', '127.0.0.1', '0.0.0.0', '[::1]'].includes(u.hostname)
    const hostId = info?.host_id
    if (local && boot.forwarding && hostId) {
      void openForward(hostId, Number(u.port || (u.protocol === 'https:' ? 443 : 80)), u.pathname + u.search, toast, u.hash)
    } else {
      window.open(uri, '_blank', 'noopener,noreferrer')
    }
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

  const slow = status === 'open' && latency !== null && latency > HIGH_LATENCY
  const statusText =
    ended ? t('term.endedShort') :
    status === 'open' ? (slow ? t('term.latency', { ms: latency }) : t('term.connected')) :
    status === 'connecting' ? t('term.connecting') :
    status === 'reconnecting' ? t('term.reconnecting') :
    status === 'offline' ? t('term.offline') :
    status === 'full' ? t('term.fullShort') : ''
  const statusClass = ended ? 's-ended' : slow ? 's-slow' : 's-' + status
  const paused = !ended && (status === 'reconnecting' || status === 'offline')
  const herdrStale = paused || !herdrFresh

  const endText = status === 'gone' ? t('term.ended.gone') :
    end ? (end.state === 'exited' ? t('term.ended.exited', { code: end.code }) : end.state === 'lost' ? t('term.ended.lost') : t('term.ended.closed')) :
    ended ? t('term.ended.closed') : ''

  const loadSessions = () => {
    setSessionsFailed(false)
    setSessions(null)
    api<SessionInfo[]>('GET', '/api/sessions')
      .then((l) => setSessions(l.filter((x) => x.state === 'running').sort((a, b) => a.created_at - b.created_at)))
      .catch(() => setSessionsFailed(true))
  }
  const switcherItems = (): (MenuItem | null)[] => {
    const list = sessions ?? []
    const nth = (x: SessionInfo) => list.filter((y) => y.host_id === x.host_id).indexOf(x) + 1
    return [
      sessionsFailed ? { label: t('common.retry'), icon: <RotateCw size={16} />, keepOpen: true, onSelect: loadSessions } : null,
      ...list.map((x) => ({
        label: `${x.host_name} · ${t('hosts.sessionN', { n: nth(x) })}${x.title ? ' · ' + x.title : ''}`,
        icon: x.id === id ? <Check size={16} /> : <span className="menu-icon-space" />,
        onSelect: () => x.id !== id && navigate('/t/' + x.id),
      })),
      { label: t('term.backToHosts'), icon: <Server size={16} />, separator: list.length > 0 || sessionsFailed, onSelect: () => navigate('/') },
    ]
  }

  const moreItems = (): (MenuItem | null)[] => [
    { label: t('term.upload'), icon: <ImagePlus size={16} />, onSelect: () => fileRef.current?.click() },
    { label: t('term.copyMode'), icon: <TextSelect size={16} />, onSelect: openCopyMode },
    boot.forwarding && info ? { label: t('ports.title'), icon: <Globe size={16} />, onSelect: () => setPortsOpen(true) } : null,
    boot.code && info ? { label: t('code.open'), icon: <Code2 size={16} />, onSelect: () => code.open(info.host_id, info.host_name) } : null,
    boot.code && info && code.running[info.host_id] ? { label: t('code.stop'), icon: <CircleStop size={16} />, onSelect: () => code.stop(info.host_id) } : null,
    { label: t('settings.termTheme'), icon: <Palette size={16} />, separator: true, onSelect: () => setSchemesOpen(true) },
    { label: t('term.fontLarger'), icon: <Plus size={16} />, onSelect: () => setPrefs({ fontSize: Math.min(28, getPrefs().fontSize + 1) }) },
    { label: t('term.fontSmaller'), icon: <Minus size={16} />, onSelect: () => setPrefs({ fontSize: Math.max(9, getPrefs().fontSize - 1) }) },
    isTouch ? { label: prefs.keybar ? t('term.hideKeybar') : t('term.showKeybar'), icon: <KeyboardIcon size={16} />, onSelect: () => setPrefs({ keybar: !prefs.keybar }) } : null,
    ended
      ? null
      : {
          label: t('term.closeSession'),
          icon: <Power size={16} />,
          danger: true,
          separator: true,
          onSelect: () =>
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
            ),
        },
  ]

  return (
    <div
      className={'term-root ' + (scheme.dark ? 'dark' : 'light')}
      style={chromeVars(scheme) as React.CSSProperties}
      ref={rootRef}
      onDragOver={(e) => {
        if (e.dataTransfer.types.includes('Files')) {
          e.preventDefault()
          setDragOver(true)
        }
      }}
      onDragLeave={(e) => e.currentTarget === e.target && setDragOver(false)}
      onDrop={(e) => {
        setDragOver(false)
        // Only files are ours; dragged text keeps the browser's own handling
        // (dropping into the composer inserts it).
        const files = [...e.dataTransfer.files]
        if (files.length === 0) return
        e.preventDefault()
        void upload(files)
      }}
    >
      <header className="term-bar">
        <button className="icon-btn" onClick={() => navigate('/')} aria-label={t('term.backToHosts')} title={t('term.backToHosts')}>
          <ChevronLeft size={20} />
        </button>
        <Menu
          label={t('term.switchSession')}
          items={switcherItems}
          note={sessionsFailed ? t('term.sessionsFailed') : sessions === null ? t('term.sessionsLoading') : undefined}
          button={(open, toggle) => (
            <button
              className="term-title"
              aria-expanded={open}
              aria-label={`${info?.host_name ?? ''}，${t('term.switchSession')}`}
              onClick={(e) => {
                if (!open) void loadSessions()
                toggle(e)
              }}
            >
              <span className="term-host">
                <span className="term-host-name">{info?.host_name ?? ''}</span>
                <ChevronDown size={12} strokeWidth={2.25} aria-hidden="true" />
              </span>
              <span className={'term-status ' + statusClass} aria-live="polite">
                <span className="dot" aria-hidden="true" />
                {statusText}
              </span>
            </button>
          )}
        />
        <div className="spacer" />
        {herdr && !ended && (
          <button className="icon-btn herdr-btn" aria-pressed={panelOpen} onClick={() => setPanel(!panelOpen)} aria-label={t('herdr.title') + (herdr.waiting > 0 && !herdrStale ? ' · ' + t('herdr.waiting', { n: herdr.waiting }) : '')} title={t('herdr.title')}>
            <LayoutGrid size={19} />
            {herdr.waiting > 0 && !herdrStale && <span className="herdr-count">{herdr.waiting}</span>}
          </button>
        )}
        {!ended && (
          <button className="icon-btn" aria-pressed={composer} onClick={toggleComposer} aria-label={t('term.composer')} title={t('term.composer') + (isTouch ? '' : isMac ? ' (⇧⌘↩)' : ' (Ctrl+Shift+Enter)')}>
            <MessageSquareText size={19} />
          </button>
        )}
        <Menu
          label={t('common.more')}
          items={moreItems}
          button={(open, toggle) => (
            <button
              className="icon-btn"
              aria-label={t('common.more')}
              aria-expanded={open}
              onClick={(e) => {
                // VS Code may have been started or stopped on another device.
                if (!open && boot.code && info) refreshCode([info.host_id])
                toggle(e)
              }}
            >
              <MoreVertical size={19} />
            </button>
          )}
        />
      </header>

      {paused && <div className="term-paused" role="status">{t('term.paused')}</div>}
      {status === 'full' && (
        <div className="term-paused term-following term-full" role="alert">
          <span>{t('term.full')}</span>
          <button className="link-btn" onClick={() => connRef.current?.retry()}>{t('common.retry')}</button>
        </div>
      )}
      {following && !paused && !ended && (
        <div className="term-paused term-following" role="status">
          <span>{t('term.following', { cols: following.cols, rows: following.rows })}</span>
          <button className="link-btn" onClick={() => takeOverRef.current()}>{t('term.takeOver')}</button>
        </div>
      )}
      {imeLines.length > 0 && (
        <div className="ime-debug">
          <button className="btn" onClick={() => void copyToClipboard(imeLines.join('\n'))}>{t('common.copy')}</button>
          <pre>{imeLines.join('\n')}</pre>
        </div>
      )}
      <div className={'term-body' + (herdr && panelOpen && !sheetPanel ? ' with-panel' : '') + (paused ? ' is-paused' : '') + (ended ? ' is-ended' : '')}>
        <div className="term-host-el" ref={hostEl} />
        {herdr && panelOpen && !ended && (
          <HerdrPanel
            summary={herdr}
            sheet={sheetPanel}
            stale={herdrStale}
            onClose={(viaKeyboard) => {
              setPanel(false)
              // A keyboard user (also an iPad with a hardware keyboard) keeps typing.
              if (viaKeyboard) termRef.current?.focus()
            }}
            onFocus={async (action, target) => {
              try {
                await api('POST', `/api/sessions/${id}/herdr`, { action, target })
                if (sheetPanel) setPanel(false)
              } catch (e) {
                toast(e instanceof ApiError && e.code === 'herdr_busy' ? t('herdr.busy') : t('herdr.focusFailed'), true)
              }
            }}
          />
        )}
        {dragOver && <div className="term-drop">{t('term.dropHint')}</div>}
      </div>

      {ended ? (
        <div className="term-ended" role="alert">
          <p>{endText}</p>
          <div className="term-ended-actions">
            <button className="btn btn-ghost" onClick={() => navigate('/')}>{t('common.back')}</button>
            {hostIdForReconnect > 0 && <button className="btn btn-primary" onClick={reconnect}>{t('term.reopen')}</button>}
          </div>
        </div>
      ) : (
        <>
          {composer && (
            <div className="composer">
              <div className="composer-box">
                <textarea
                  ref={composerRef}
                  className="composer-input"
                  value={draft}
                  aria-label={t('term.composer')}
                  placeholder={isTouch ? t('term.composerPlaceholder') : t('term.composerPlaceholderKey', { key: isMac ? '⌘↩' : 'Ctrl+Enter' })}
                  rows={1}
                  onChange={(e) => setDraft(e.target.value)}
                  onPaste={(e) => {
                    const files = [...(e.clipboardData?.files ?? [])]
                    if (files.length === 0) return
                    e.preventDefault()
                    void upload(files)
                  }}
                  onKeyDown={(e) => {
                    if (e.nativeEvent.isComposing || e.keyCode === 229) return
                    if (e.key === 'Enter' && (e.metaKey || e.ctrlKey)) {
                      e.preventDefault()
                      sendComposer(true)
                    } else if (e.key === 'Escape') {
                      e.preventDefault()
                      toggleComposer()
                    }
                  }}
                />
                <div className="composer-tools">
                  <button type="button" className="icon-btn" onClick={() => fileRef.current?.click()} aria-label={t('term.upload')} title={t('term.upload')}>
                    <Paperclip size={18} />
                  </button>
                  <button type="button" className="btn btn-ghost composer-insert" onClick={() => sendComposer(false)} disabled={!draft} title={t('term.insertHint')}>
                    {t('term.insert')}
                  </button>
                  <div className="spacer" />
                  <button type="button" className="composer-send" onClick={() => sendComposer(true)} aria-label={t('term.send')} title={t('term.send')}>
                    <ArrowUp size={18} strokeWidth={2.25} />
                  </button>
                </div>
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
        </>
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
      {schemesOpen && <SchemeDialog onClose={() => setSchemesOpen(false)} />}
      {portsOpen && info && <PortsDialog hostId={info.host_id} onClose={() => setPortsOpen(false)} />}
      {code.dialog}
      {confirm}
      {dialogs}
    </div>
  )
}

// Copy mode shows the scrollback as real text, so native selection works on touch screens.
function CopyMode({ text, onClose }: { text: string; onClose: () => void }) {
  const toast = useToast()
  const ref = useFocusTrap<HTMLDivElement>(onClose)
  // Start at the bottom once; later renders must not move a reader back down.
  const pre = useRef<HTMLPreElement>(null)
  useLayoutEffect(() => {
    if (pre.current) pre.current.scrollTop = pre.current.scrollHeight
  }, [])
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
      <pre className="copy-text" tabIndex={0} ref={pre}>{text}</pre>
    </div>
  )
}
