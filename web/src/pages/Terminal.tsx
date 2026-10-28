import { useCallback, useEffect, useLayoutEffect, useMemo, useRef, useState } from 'react'
import { flushSync } from 'react-dom'
import { Terminal } from '@xterm/xterm'
import { FitAddon } from '@xterm/addon-fit'
import { fitToEdge } from '../terminal/fit'
import { Unicode11Addon } from '@xterm/addon-unicode11'
import { WebLinksAddon } from '@xterm/addon-web-links'
import { WebglAddon } from '@xterm/addon-webgl'
import { ArrowUp, Check, ChevronDown, ChevronLeft, CircleStop, Code2, Eye, FolderOpen, Globe, History, ImagePlus, MessageCircleReply, MoreVertical, Minus, Palette, Paperclip, Plus, TextSelect, Keyboard as KeyboardIcon, Power, RotateCw, Server, TextCursorInput, X } from 'lucide-react'
import { api, ApiError, choiceOf, errorText, uploadFile, type HerdrAgent, type HerdrSummary, type Host, type OpenChoice, type SessionInfo, probeBootstrap } from '../api'
import { t } from '../i18n'
import { navigate } from '../router'
import { getPrefs, prefsConfirmed, setChromeColor, setPrefs, termScheme, usePrefs } from '../prefs'
import { local } from '../storage'
import { CLOSE_MENUS, Confirm, Menu, copyText as copyToClipboard, keysInUse, useFocusTrap, useToast, type MenuItem } from '../components/ui'
import { useConnect } from '../connect'
import { clampSize, TermConnection, type EndInfo, type Status } from '../terminal/connection'
import { applyMods, specialKey, type KeysApart, type Mods, type SpecialKey } from '../terminal/keys'
import { KeyBar, type ModState } from '../terminal/KeyBar'
import { macroBytes, parseMacro, type Step } from '../terminal/macro'
import { chromeVars } from '../terminal/themes'
import { expandPastes, getDraft, keepDraft, keepPaste, remember, sentHistory, showDraft } from '../terminal/drafts'
import { useCompletion } from '../terminal/Completion'
import { SchemeDialog } from '../terminal/SchemePicker'
import { ShortcutPanel, lastPanelPage } from '../terminal/ShortcutPanel'
import { panelPages, type Shortcut } from '../terminal/shortcuts'
import { layoutIn } from '../terminal/layout'
import { startPreviews } from '../terminal/preview'
import { atCursor, inPromptBox } from '../terminal/prompt'
import { ReplyDialog } from '../terminal/ReplyDialog'
import { PortsDialog, openForward } from '../terminal/PortsDialog'
import { FilesDialog } from '../terminal/FilesDialog'
import { ShareDialog } from '../terminal/ShareDialog'
import { useCode } from '../terminal/CodeDialog'
import { onOpenRequest, takeTabs } from '../notifyopen'
import { offerAlerts } from '../push'
import { adoptComposer, composerWasOpen, rememberComposer } from '../terminal/composerOpen'
import { promptSuggestion } from '../terminal/suggestion'
import { sessionWhich } from '../terminal/sessionName'
import { knownSession } from '../terminal/agents'
import { holdTermMode } from '../terminal/chatSend'
import { useComposerFit } from '../terminal/autosize'
import { useSession, VERSION_EVENT } from '../app'
import { geminiPath, shellPath } from '../terminal/paths'

const isTouch = window.matchMedia('(pointer: coarse)').matches || navigator.maxTouchPoints > 0

const isMac = /Mac|iPhone|iPad/.test(navigator.platform)
const HIGH_LATENCY = 300 // ms; below this the status just says "connected"

// On a touch screen with the key bar, a tap on the terminal does not bring
// up the on-screen keyboard unless the user wants that (prefs.tapKeyboard):
// xterm gives its text field focus on every press, and while taps are quiet
// that is refused (the press still reaches the program: herdr's sidebar,
// mouse modes). The field takes focus when asked to (allowFocus): by the key
// bar's keyboard button, or by a key from a hardware keyboard, after which
// taps give it focus as usual (with one attached no on-screen keyboard
// shows) until the field is left or an on-screen keyboard is seen.
let hardwareKeys = false
const quietTaps = () => isTouch && getPrefs().keybar && !getPrefs().tapKeyboard && !hardwareKeys
let focusAsked = false
function allowFocus(f: () => void) {
  focusAsked = true
  try {
    f()
  } finally {
    focusAsked = false
  }
}
// iOS: a field may keep focus after the system's key put its keyboard away.
// That is seen when the visual viewport, unzoomed, grows back by the height
// of a keyboard (its layout viewport does not shrink for the keyboard, as in
// main.tsx). Only a keyboard seen going down counts: a floating one, or one
// under pinch zoom, is never taken for gone. (A docked keyboard undocked to
// float looks the same as one put away: the button then needs a second tap.)
let keyboardWentDown = false
if (isMac && isTouch && window.visualViewport) {
  const vv = window.visualViewport
  let up = false
  vv.addEventListener('resize', () => {
    if ((vv.scale ?? 1) > 1.01) {
      keyboardWentDown = false // (zoomed: not known, focus decides)
      return
    }
    const now = window.innerHeight - vv.height > 120
    if (up && !now) keyboardWentDown = true
    if (now) keyboardWentDown = hardwareKeys = false // (an on-screen keyboard: none attached)
    up = now
  })
  document.addEventListener('focusin', () => (keyboardWentDown = false))
}

const FONT_STACK = "'JetBrains Mono', 'SFMono-Regular', Menlo, Consolas, 'PingFang SC', 'Hiragino Sans GB', 'Microsoft YaHei', monospace"

// A sent message as one line of a menu.
const oneLine = (s: string) => {
  const flat = s.trim().replace(/\s*\n\s*/g, ' ⏎ ')
  return [...flat].length > 80 ? [...flat].slice(0, 79).join('') + '…' : flat
}

// While replayed output is parsed, xterm answers the old queries in it. The
// answers have fixed shapes: device attributes and status (CSI … c / n / R),
// mode reports (CSI … $ y), window reports (CSI … t), focus (CSI I / O),
// colour and DCS reports. This is only the fallback for telling them apart
// from typing, see userInputEvent.
const REPLY = /^(?:\x1b\[[?>]?[\d;]*[cnRt]|\x1b\[\??[\d;]*\$y|\x1b\[[IO]|\x1bP[\s\S]*?\x1b\\|\x1b\][\s\S]*?(?:\x07|\x1b\\))+$/
// A computer (a mouse or trackpad first), touch screen or not: Enter in the
// composer acts as set there. Phones and tablets keep Enter for new lines.
const hasKeyboardPointer = () => !window.matchMedia('(pointer: coarse)').matches
const AUTO_RECONNECT_S = 3 // seconds before a tmux/herdr session gone with the gateway is opened again
const CLAIM_GAP = 100 // ms between two takeover claims of one window while it types
// A paste into the composer longer than this shows as a label (see keepPaste).
const PASTE_LINES = 12
const PASTE_CHARS = 1000

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

// The colour scheme as mode 2031 reports it: CSI ? 997 ; 1 n dark, 2 light.
const schemeReport = (dark: boolean) => `\x1b[?997;${dark ? 1 : 2}n`

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

// remoteCopyLimit bounds the text a program on the host may copy (in base64
// characters: about 750 KB of text).
const remoteCopyLimit = 1 << 20

// clipboardText is the text of an OSC 52 "set the clipboard" request
// ("<targets>;<base64>"), or null for anything else: a request to read the
// clipboard ("?"), to clear it, one too large, or not UTF-8.
function clipboardText(data: string): string | null {
  const i = data.indexOf(';')
  if (i < 0) return null
  const b64 = data.slice(i + 1)
  if (b64 === '' || b64 === '?' || b64.length > remoteCopyLimit) return null
  try {
    const bin = atob(b64)
    return new TextDecoder('utf-8', { fatal: true }).decode(Uint8Array.from(bin, (c) => c.charCodeAt(0))) || null
  } catch {
    return null
  }
}


// watch: a session someone else owns and lets this user watch, read-only.
// What a tab shows of its terminal (see Tabs.tsx).
export interface TabInfo {
  status: string // the status class: s-open, s-slow, s-reconnecting, s-ended…
  statusText: string
  waiting: number // herdr agents waiting for an answer
  done: number // herdr agents that finished and were not looked at since (herdr's "done")
  working: number // herdr agents at work
  reopening?: boolean // being opened again by itself (see autoAgain): kept mounted meanwhile
}

// active: shown (with tabs, the others stay connected but hidden: they take
// no focus or keys and show no dialogs). tabs: drawn in the top bar in place
// of the session switcher.
// dropParam takes what a notification asked for (?name=value) out of the
// address of this session's page once it is dealt with.
function dropParam(id: string, name: string, value: string) {
  const q = new URLSearchParams(location.search)
  if (location.pathname !== '/t/' + id || q.get(name) !== value) return
  q.delete(name)
  const rest = q.toString()
  history.replaceState(history.state, '', location.pathname + (rest ? '?' + rest : ''))
}

// onEnd: where to go once the user has ended this session (with tabs, the
// next one), else the host list.
export function TerminalPage({ id, watch = false, active = true, tabs, onInfo, onEnd }: { id: string; watch?: boolean; active?: boolean; tabs?: React.ReactNode; onInfo?: (id: string, i: TabInfo) => void; onEnd?: () => void }) {
  const activeRef = useRef(active)
  activeRef.current = active
  const prefs = usePrefs()
  const toast = useToast()
  const rootRef = useRef<HTMLDivElement>(null)
  const hostEl = useRef<HTMLDivElement>(null)
  const termRef = useRef<Terminal | null>(null)
  const connRef = useRef<TermConnection | null>(null)
  const fileRef = useRef<HTMLInputElement>(null)
  const composerRef = useRef<HTMLTextAreaElement>(null)

  // (Known at once when the list of agents had it: the conversation view shows without the terminal first.)
  const [info, setInfo] = useState<SessionInfo | null>(() => (watch ? null : knownSession(id)))
  const [status, setStatus] = useState<Status>('connecting')
  const [latency, setLatency] = useState<number | null>(null)
  const [end, setEnd] = useState<EndInfo | null>(null)
  const [ctrl, setCtrl] = useState<ModState>('off')
  const [alt, setAlt] = useState<ModState>('off')
  const [fn, setFn] = useState(false)
  // Open again where it was left open (see composerOpen.ts); never for a watcher.
  const [composer, setComposer] = useState(() => !watch && composerWasOpen(id))
  // An address from before the agents page (?convo=1, the conversation of
  // this session's agent): that page, at the same agent.
  useEffect(() => {
    const q = new URLSearchParams(location.search)
    if (watch || q.get('convo') !== '1') return
    const to = new URLSearchParams({ s: id })
    if (q.get('pane')) to.set('pane', q.get('pane')!)
    navigate('/agents?' + to, true)
  }, [id, watch])
  useEffect(() => {
    if (!watch) adoptComposer(id, composerWasOpen(id))
  }, [id, watch])
  const [draft, setDraft] = useState(() => (watch ? '' : getDraft(id)))
  // Going through the history (Up/Down at the box's start/end): the entry
  // shown, and the draft there when it started (back after the newest entry).
  const histPos = useRef<string | null>(null) // (the entry: other tabs change the list)
  const histStash = useRef('')
  // While going through the history, the draft put aside is what is kept
  // (for when the page is left, or drawn again, meanwhile).
  useEffect(() => {
    if (!watch) keepDraft(id, histPos.current !== null ? histStash.current : draft)
    if (!watch) showDraft(id, draft)
  }, [id, draft, watch])
  // Claude Code's suggested next prompt, offered while the composer is open
  // and empty (see suggestion.ts): read again a moment after the screen changes.
  const [suggestion, setSuggestion] = useState<string | null>(null)
  const draftEmpty = draft === ''
  useEffect(() => {
    const term = termRef.current
    if (!composer || !draftEmpty || !active || watch || !term) {
      setSuggestion(null)
      return
    }
    let timer: number | undefined
    // (Not while the browser tab is in the back: read again once it is back.)
    const read = () => document.visibilityState === 'visible' && setSuggestion(promptSuggestion(term))
    const soon = () => {
      clearTimeout(timer)
      timer = window.setTimeout(read, 250)
    }
    read()
    const subs = [term.onWriteParsed(soon), term.onScroll(soon)] // (scrolled back to the live screen too)
    document.addEventListener('visibilitychange', soon)
    return () => {
      clearTimeout(timer)
      subs.forEach((x) => x.dispose())
      document.removeEventListener('visibilitychange', soon)
    }
  }, [composer, draftEmpty, active, watch, status]) // (status: the terminal exists once it connects)
  // Text from the key bar or an upload goes in at the composer's cursor.
  const insertDraft = useCallback((s: string) => {
    histPos.current = null // (an entry added to is a draft of its own)
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
  const [sessions, setSessions] = useState<SessionInfo[] | null>(null)
  const [imeLines, setImeLines] = useState<string[]>([])
  const [sessionsFailed, setSessionsFailed] = useState(false)
  // False from a disconnect until this connection sends a new herdr summary.
  const [herdrFresh, setHerdrFresh] = useState(true)
  // The agent being answered, and the pane a notification asked to answer
  // (?reply=<pane>), opened once this connection's summary has come.
  const [replyTo, setReplyTo] = useState<HerdrAgent | null>(null)
  // When this connection last had the terminal's size set (0: not yet):
  // herdr redraws its panes for it, so a pane's screen is read after that.
  const [sized, setSized] = useState(0)
  const [wantReply, setWantReply] = useState<string | null>(() => (watch ? null : new URLSearchParams(location.search).get('reply')))
  // The pane stays in the address until it is dealt with (the dialog has
  // shown its screen, or it no longer waits), so signing in again on the way
  // (the login may have ended) brings it back here.
  const forgetReply = (pane: string) => dropParam(id, 'reply', pane)
  // The pane a notification is about (?pane=): herdr is asked to focus it
  // (its workspace and tab too) once this connection's summary has come.
  // (An address from before the agents page, ?convo=1&pane=, is that page's: see below.)
  const [wantPane, setWantPane] = useState<string | null>(() => {
    const q = new URLSearchParams(location.search)
    return watch || q.get('convo') === '1' ? null : q.get('pane')
  })
  // Tried again with the next summary if herdr did not take it (busy, the
  // connection gone meanwhile), a few times.
  const paneTries = useRef<{ pane: string; n: number; busy: boolean }>({ pane: '', n: 0, busy: false })
  useEffect(() => {
    if (!wantPane || !herdr || !herdrFresh || status !== 'open') return
    const pane = wantPane
    const tr = paneTries.current.pane === pane ? paneTries.current : (paneTries.current = { pane, n: 0, busy: false })
    if (tr.busy) return
    const done = () => {
      setWantPane((cur) => (cur === pane ? null : cur))
      dropParam(id, 'pane', pane)
    }
    // (Closed since: nothing to go to.)
    if (!herdr.workspaces.some((w) => w.agents.some((a) => a.pane_id === pane))) return done()
    // Given up for now; the address keeps it (a reload tries again).
    if (tr.n >= 3) return setWantPane((cur) => (cur === pane ? null : cur))
    tr.n++
    tr.busy = true
    api('POST', `/api/sessions/${id}/herdr`, { action: 'focus_pane', target: pane })
      .then(done, () => {})
      .finally(() => (tr.busy = false))
  }, [wantPane, herdr, herdrFresh, status, id])
  // A pane not (yet) seen waiting gets a while: the summary shown may be one
  // from before the connection came back. Each new summary looks again.
  const replyGiveUp = useRef<{ pane: string; timer: number } | null>(null)
  const stopGiveUp = () => {
    clearTimeout(replyGiveUp.current?.timer)
    replyGiveUp.current = null
  }
  useEffect(() => stopGiveUp, [])
  useEffect(() => {
    if (!wantReply) return
    // The address is where the request is kept: gone from there, dropped.
    if (new URLSearchParams(location.search).get('reply') !== wantReply) {
      setWantReply(null)
      return
    }
    if (!herdr || !herdrFresh || status !== 'open' || !sized) {
      stopGiveUp() // the wait starts again with the next connection's summary
      return
    }
    // Still waiting: answered elsewhere since the notification, it is not.
    const a = herdr.workspaces.flatMap((w) => w.agents).find((x) => x.pane_id === wantReply && x.status === 'blocked')
    if (a) {
      stopGiveUp()
      // Shortly after the size was set, so the screen read is the redrawn one.
      const wait = Math.max(0, sized + 800 - Date.now())
      const tm = window.setTimeout(() => {
        setWantReply((cur) => (cur === wantReply ? null : cur))
        setReplyTo(a)
      }, wait)
      return () => clearTimeout(tm)
    }
    if (replyGiveUp.current?.pane === wantReply) return
    stopGiveUp()
    const pane = wantReply
    const timer = window.setTimeout(() => {
      replyGiveUp.current = null
      setWantReply((cur) => (cur === pane ? null : cur))
      if (new URLSearchParams(location.search).get('reply') !== pane) return
      forgetReply(pane)
      toast(t('reply.gone'), true)
    }, 8000)
    replyGiveUp.current = { pane, timer }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [wantReply, herdr, herdrFresh, status, sized, toast])
  // Without tabs (a phone), a notification of another session tapped while
  // this page is in front goes there: this session keeps running, the
  // window shows the one asked for (an app on a phone's home screen may not
  // open a window of its own for it). Not while a dialog of this page is up.
  useEffect(() => {
    if (tabs || watch) return
    takeTabs((u) => {
      if (!/^\/t\/[0-9a-f]+$/.test(u.pathname) || document.querySelector('[aria-modal="true"]')) return false
      navigate(u.pathname + u.search)
      return true
    })
    return () => takeTabs(null)
  }, [tabs, watch])
  // Agents seen here for the first time: alerts are offered for this device
  // (when the browser has not been asked yet; see offerAlerts).
  const hasAgents = !!herdr?.workspaces.some((w) => w.agents.length > 0)
  useEffect(() => {
    if (!hasAgents || !active || watch) return
    void offerAlerts((ask) =>
      toast(t('push.offer'), false, {
        label: t('push.offerAction'),
        run: () =>
          void ask()
            .then((st) => toast(st === 'on' ? t('push.offerOn') : st === 'denied' ? t('push.denied') : t('push.offerLater'), st === 'denied'))
            .catch(() => toast(t('push.failed'), true)),
      }),
    )
  }, [hasAgents, active, watch, toast])
  // A notification tapped while this page is open already.
  useEffect(() => {
    if (watch) return
    return onOpenRequest(() => {
      const q = new URLSearchParams(location.search)
      if (location.pathname !== '/t/' + id) return
      if (q.get('convo') === '1') {
        // (From before the agents page: there, at that agent.)
        const to = new URLSearchParams({ s: id })
        if (q.get('pane')) to.set('pane', q.get('pane')!)
        return navigate('/agents?' + to, true)
      }
      if (q.get('pane')) {
        paneTries.current = { pane: '', n: 0, busy: false }
        setWantPane(q.get('pane'))
      }
      if (q.get('reply')) setWantReply(q.get('reply'))
    })
  }, [id, watch])
  const ended = status === 'ended' || status === 'gone' || status === 'revoked' || end !== null
  const showKeybar = isTouch && prefs.keybar && !ended && !watch
  const [owner, setOwner] = useState('') // watching: whose session it is
  const { boot } = useSession()
  const [portsOpen, setPortsOpen] = useState(false)
  const [filesOpen, setFilesOpen] = useState(false)
  const [shareOpen, setShareOpen] = useState(false)
  const filesPath = useRef<string | null>(null) // where the file panel was, for this page
  const filesOpens = useRef(-1) // the connection the panel was opened on
  const openFiles = () => {
    filesOpens.current = connRef.current?.isOpen() ? connRef.current.opens : -1
    setFilesOpen(true)
  }
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
  const keysApartRef = useRef<() => KeysApart>(() => null) // see the CSI handlers in the terminal's setup
  const helloInstance = useRef('') // the run of the gateway this window last connected to
  const pageGone = useRef(false) // (waits and reconnects stop with the page)
  const swipeRef = useRef<(dir: 1 | -1, what: 'session' | 'workspace') => void>(() => {}) // two fingers moved together (see the touch handler)
  const syncSchemeRef = useRef<(again?: boolean) => void>(() => {}) // tells programs that asked of a dark/light change (mode 2031)
  const heldRef = useRef<() => boolean>(() => false) // input is waiting for a Ctrl+V clipboard check or upload
  const composingRef = useRef<() => boolean>(() => false)
  const forgetTypedRef = useRef<(one?: boolean) => void>(() => {}) // see the key handler
  const mods = useRef<{ ctrl: ModState; alt: ModState }>({ ctrl: 'off', alt: 'off' })
  mods.current = { ctrl, alt }
  const releaseOnce = useCallback(() => {
    if (mods.current.ctrl === 'once') setCtrl('off')
    if (mods.current.alt === 'once') setAlt('off')
  }, [])
  const activeMods = (): Mods => ({ ctrl: mods.current.ctrl !== 'off', alt: mods.current.alt !== 'off', shift: false })

  // Session metadata (host name, host id for reconnect).
  const loadInfo = useCallback(() => {
    if (watch) {
      api<SessionInfo & { owner_name: string }>('GET', '/api/shared/' + id)
        .then((s) => {
          setInfo(s)
          setOwner(s.owner_name)
        })
        .catch(() => {})
      return
    }
    api<SessionInfo[]>('GET', '/api/sessions')
      .then((list) => {
        const s = list.find((x) => x.id === id)
        if (s) {
          setInfo(s)
          local.set('corral.sessHost.' + id, String(s.host_id))
          // What it was opened as, to open it again after it is gone.
          const c = choiceOf(s)
          if (c) local.set('corral.sessOpen.' + id, JSON.stringify(c))
        }
      })
      .catch(() => {})
  }, [id, watch])
  useEffect(loadInfo, [loadInfo])
  // Kept current: a recording stops at its size limit, and others start or
  // stop watching; the status says both.
  useEffect(() => {
    const t = setInterval(() => document.visibilityState === 'visible' && loadInfo(), 30000)
    return () => clearInterval(t)
  }, [loadInfo])

  // Terminal + connection lifecycle.
  useEffect(() => {
    const el = hostEl.current!
    const p = getPrefs()
    const term = new Terminal({
      disableStdin: watch, // keys typed while watching go nowhere
      fontFamily: FONT_STACK,
      fontSize: p.fontSize,
      // (JetBrains Mono's own line is 1.32 of its size, more than most
      // terminal fonts: little is added, or a phone shows a fifth fewer rows.)
      lineHeight: isTouch ? 1.0 : 1.1,
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
    fitToEdge(fit, term)
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
    // Trackpads, in programs that take the mouse (Claude Code's fullscreen
    // view, herdr, tmux): xterm sends one wheel report per event however far
    // it moved, and scales small moves by 0.3, so a swipe scrolls a fraction
    // of its length. Here it sends a report per line moved, as native
    // terminals do. A mouse wheel's notch stays one report (Chrome and Safari
    // give 120 a notch in wheelDeltaY, and 3× deltaY for a trackpad). The
    // reports are untrusted line events, one report each (as for touch).
    let wheelRest = 0
    term.attachCustomWheelEventHandler((e) => {
      const mode = term.modes.mouseTrackingMode // (x10 reports presses only, not the wheel)
      if (!e.isTrusted || mode === 'none' || mode === 'x10' || e.deltaMode !== WheelEvent.DOM_DELTA_PIXEL || e.shiftKey || e.deltaY === 0) return true
      const legacy = (e as WheelEvent & { wheelDeltaY?: number }).wheelDeltaY
      if (legacy && legacy % 120 === 0 && Math.abs(legacy) !== Math.abs(e.deltaY) * 3) return true
      if (Math.sign(wheelRest) !== Math.sign(e.deltaY)) wheelRest = 0
      wheelRest += e.deltaY / (el.clientHeight / Math.max(1, term.rows))
      // At most 20 at once; the rest is kept for the next event.
      const n = Math.sign(wheelRest) * Math.min(Math.trunc(Math.abs(wheelRest)), 20)
      wheelRest -= n
      const target = el.querySelector('.xterm-screen') ?? el
      for (let i = 0; i < Math.abs(n); i++) {
        target.dispatchEvent(
          new WheelEvent('wheel', {
            deltaY: Math.sign(n), deltaMode: WheelEvent.DOM_DELTA_LINE, bubbles: true, cancelable: true, clientX: e.clientX, clientY: e.clientY,
            ctrlKey: e.ctrlKey, altKey: e.altKey, metaKey: e.metaKey,
          }),
        )
      }
      return false
    })
    const ta = el.querySelector('textarea')
    ta?.setAttribute('autocapitalize', 'off')
    ta?.setAttribute('autocorrect', 'off')
    ta?.setAttribute('spellcheck', 'false')
    // Quiet taps (see quietTaps): requests for focus that follow a press are
    // refused (xterm's own on every press), those that follow keys are not,
    // nor the one that follows a tap where the program takes typing (at its
    // cursor, in Claude Code's input box: terminal/prompt.ts, onQuietPress):
    // the keyboard comes up with that tap, as for a text field.
    let promptTap: { x: number; y: number; at: number } | null = null // the press, while it is still a tap
    let promptArmed = false // during the mousedown that tap brings
    // With #imedebug in the address, how taps were judged (see imeLog).
    const tapLog = location.hash.includes('imedebug') ? (line: string) => setImeLines((l) => [...l.slice(-29), line]) : null
    if (ta) {
      const focus = ta.focus
      ta.focus = function (this: HTMLTextAreaElement, o?: FocusOptions) {
        const ok = focusAsked || !quietTaps() || keysInUse() || promptArmed
        tapLog?.(`focus asked: ${ok ? 'allowed' : 'refused'}${promptArmed ? ' (input box)' : ''}`)
        if (ok) focus.call(this, o)
      }
      if (tapLog) for (const k of ['focus', 'blur']) ta.addEventListener(k, () => tapLog(k))
    }
    // A key while nothing has focus comes from a hardware keyboard: the field
    // in use takes focus (the composer when it is open). A character's text
    // then arrives there; this keydown itself never reaches xterm, so for the
    // terminal a key that acts on keydown alone (Enter, arrows, Esc, Ctrl or
    // a latched modifier with a letter) is sent from here.
    const onStrayKey = (e: KeyboardEvent) => {
      const a = document.activeElement
      if (!activeRef.current || !ta || !isTouch || (a && a !== document.body) || document.querySelector('[aria-modal="true"]')) return
      hardwareKeys = true
      if (composerRef.current) return composerRef.current.focus()
      allowFocus(() => ta.focus())
      if (watch || e.metaKey || e.isComposing) return
      const special = specialFor[e.key]
      if (!special && !(e.key.length === 1 && (e.ctrlKey || latched()))) return
      const m = activeMods()
      const mods = { ctrl: m.ctrl || e.ctrlKey, alt: m.alt || e.altKey }
      e.preventDefault()
      takeOverRef.current()
      settleRef.current()
      out(special ? specialKey(special, { ...mods, shift: e.shiftKey }, term.modes.applicationCursorKeysMode, keysApart()) : applyMods(e.key, { ...mods, shift: false }))
      releaseOnce()
    }
    // Once the field is left, taps are quiet again until the next key.
    const onFieldBlur = () => (hardwareKeys = false)
    ta?.addEventListener('blur', onFieldBlur)
    // A quiet tap takes focus off a button (one that closed the composer, say)
    // as a tap that focused the field did, so hardware keys find no focus and
    // come to the terminal (onStrayKey). A text field keeps it.
    const onQuietPress = (e: PointerEvent) => {
      promptTap = null
      if (!quietTaps()) return
      // Decided as the press starts, from the screen as it is then, on a
      // live terminal (promptReady, checked again as it ends).
      const screenEl = el.querySelector('.xterm-screen')
      if (tapLog && !promptReady()) tapLog(`tap: terminal not ready (open ${conn.isOpen()}, replaying ${replaying}, settling ${settling}, following ${!!following})`)
      if (e.pointerType !== 'mouse' && e.isPrimary && screenEl && promptReady()) {
        const r = screenEl.getBoundingClientRect()
        const col = Math.floor(((e.clientX - r.left) * term.cols) / r.width)
        const row = Math.floor(((e.clientY - r.top) * term.rows) / r.height)
        const hit = atCursor(term, col, row) || inPromptBox(term, col, row)
        const b = term.buffer.active
        tapLog?.(`tap ${e.pointerType} at ${col},${row} cursor ${b.cursorX},${b.cursorY} input box: ${hit}`)
        if (hit) {
          promptTap = { x: e.clientX, y: e.clientY, at: Date.now() }
          // Focused with its keyboard put away (iOS): it takes focus afresh.
          if (ta && document.activeElement === ta && keyboardWentDown) ta.blur()
        }
      }
      const a = document.activeElement as HTMLElement | null
      if (!a || a === document.body || a === ta) return
      if (a.tagName === 'TEXTAREA' || a.tagName === 'INPUT' || a.tagName === 'SELECT' || a.isContentEditable) return
      a.blur()
    }
    el.addEventListener('pointerdown', onQuietPress, true)
    // It stays a tap only without a second finger, movement or cancelling;
    // it then lets through xterm's request for focus in the mousedown that
    // follows (and nothing later).
    const onPromptMove = (e: PointerEvent) => {
      if (promptTap && (!e.isPrimary || Math.abs(e.clientX - promptTap.x) > 8 || Math.abs(e.clientY - promptTap.y) > 8)) {
        tapLog?.('tap moved: not a tap')
        promptTap = null
      }
    }
    const onPromptCancel = () => {
      if (promptTap) tapLog?.('tap cancelled')
      promptTap = null
    }
    // A live terminal, its backlog written, taking this window's input as shown.
    const promptReady = () => conn.isOpen() && !replaying && !settling && !watch && !composerRef.current && !(following && scale < 1)
    const onPromptMouse = () => {
      tapLog?.(`mousedown${promptTap ? ' after an input-box tap' : ''}`)
      promptArmed = !!promptTap && Date.now() - promptTap.at < 1000 && promptReady()
      promptTap = null
      if (promptArmed) setTimeout(() => (promptArmed = false))
    }
    el.addEventListener('pointermove', onPromptMove, true)
    el.addEventListener('pointercancel', onPromptCancel, true)
    el.addEventListener('mousedown', onPromptMouse, true)
    document.addEventListener('keydown', onStrayKey, true)

    // Set while another viewer's size is shown (see onSize); fitting to this
    // window then waits until this viewer takes the terminal over.
    let following: { cols: number; rows: number } | null = null
    // Whether the gateway last said this window is in charge of the size,
    // and when this window last claimed it.
    let inCharge = false
    let claimedAt = 0
    let claimTimer: number | undefined
    let claimOpens = 0 // the connection claimTimer was made on (see conn.opens)
    // The user acted here (typed, pasted, clicked): this window takes the
    // terminal over. While it is in charge the claim is repeated with the
    // actions, in case another window took over meanwhile and the news has
    // not arrived yet: the latest action at the gateway wins. Claims are at
    // least CLAIM_GAP apart (an action within the gap is claimed when it
    // ends, unless another window is reported to have taken over first;
    // actions of two windows that close together count as simultaneous).
    takeOverRef.current = () => {
      if (conn.status === 'full') return // refused: actions here take nothing over
      const now = Date.now()
      if (!following && inCharge && now - claimedAt < CLAIM_GAP) {
        // Valid only on the connection it was made for: after a reconnect
        // (or a refusal and retry) the gateway's state is news to this
        // window, and an action on the new connection replaces it.
        if (claimTimer !== undefined && claimOpens !== conn.opens) {
          clearTimeout(claimTimer)
          claimTimer = undefined
        }
        if (claimTimer === undefined) {
          claimOpens = conn.opens
          claimTimer = window.setTimeout(() => {
            claimTimer = undefined
            if (conn.opens === claimOpens) takeOverRef.current()
          }, claimedAt + CLAIM_GAP - now)
        }
        return
      }
      clearTimeout(claimTimer)
      claimTimer = undefined
      claimedAt = now
      const tookOver = !inCharge
      inCharge = true
      if (following) {
        following = null
        setFollowing(null)
        applyScale()
        safeFit()
      }
      conn.resize(term.cols, term.rows, true) // tell the host even if nothing changed locally
      syncSchemeRef.current(tookOver)
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
    let settling = true // from a new socket until its backlog was written (for previews)
    // Each socket's replay is numbered: the end of an earlier one, reported
    // late, does not end a later one.
    let replayRun = 0
    let lastSize = { cols: 0, rows: 0 } // as the gateway last reported it
    let alive = true
    let lastDropToast = 0
    const droppedInput = () => {
      const now = Date.now()
      if (now - lastDropToast < 5000) return
      lastDropToast = now
      toast(t('term.inputDropped'), true)
    }
    // Programs on the host may put text on this device's clipboard (OSC 52:
    // tmux's copy mode, Claude Code, editors). Only the live output of the
    // owner's own terminal counts, never replayed output, and nothing is ever
    // read back. Where the browser allows writing only on a tap (Safari), the
    // user gets a button to tap.
    const mayCopy = () => alive && !watch && activeRef.current && prefsConfirmed() && getPrefs().remoteClipboard // (not a tab in the back)
    const remoteCopy = async (text: string) => {
      const n = [...text].length
      try {
        await navigator.clipboard.writeText(text)
        toast(t('term.remoteCopied', { n }))
      } catch {
        if (!mayCopy()) return
        toast(t('term.remoteCopyAsk', { n }), false, {
          label: t('term.remoteCopyBtn'),
          // Still this terminal's and still allowed when tapped.
          run: () =>
            mayCopy()
              ? void copyToClipboard(text).then((ok) => toast(ok ? t('term.remoteCopied', { n }) : t('term.remoteCopyFailed'), !ok))
              : toast(t('term.remoteCopyGone'), true),
        })
      }
    }
    // Programs that ask for keys to be told apart (kitty's keyboard protocol,
    // xterm's modifyOtherKeys; Claude Code and Codex do, herdr too) get
    // Ctrl+Enter encoded as they asked, not as a plain Enter ("ctrl+enter to
    // send now"); see specialKey. xterm implements neither, so only the
    // requests are noted here, kitty's flags per screen as the protocol has
    // it (the gateway restores them for a late replay).
    const kitty = { main: [] as number[], alt: [] as number[] } // pushed flags, latest last
    const flags = () => kitty[term.buffer.active.type === 'alternate' ? 'alt' : 'main']
    let otherKeys = 0
    let schemeReports = false
    let reportedDark = termScheme().dark // what programs were last told (or found out)
    const num = (p: (number | number[])[], i: number, d: number) => {
      const v = p[i]
      return typeof v === 'number' && v > 0 ? v : d
    }
    const kbSubs = [
      term.parser.registerCsiHandler({ prefix: '>', final: 'u' }, (p) => {
        const f = flags()
        if (f.length >= 16) f.shift() // (full: the oldest goes)
        f.push(num(p, 0, 0))
        return true
      }),
      term.parser.registerCsiHandler({ prefix: '<', final: 'u' }, (p) => {
        const f = flags()
        f.splice(Math.max(0, f.length - num(p, 0, 1)))
        return true
      }),
      term.parser.registerCsiHandler({ prefix: '=', final: 'u' }, (p) => {
        const f = flags()
        const v = num(p, 0, 0)
        const mode = num(p, 1, 1)
        const top = f.at(-1) ?? 0
        const next = mode === 2 ? top | v : mode === 3 ? top & ~v : v
        if (f.length) f[f.length - 1] = next
        else f.push(next)
        return true
      }),
      // CSI > 4 ; n m sets it, CSI > 4 m and CSI > m reset it, CSI > 4 n turns it off.
      term.parser.registerCsiHandler({ prefix: '>', final: 'm' }, (p) => {
        const r = num(p, 0, 0)
        if (r === 4) otherKeys = num(p, 1, 0)
        else if (r === 0) otherKeys = 0
        return false
      }),
      term.parser.registerCsiHandler({ prefix: '>', final: 'n' }, (p) => {
        if (num(p, 0, 0) === 4) otherKeys = 0
        return false
      }),
      term.parser.registerEscHandler({ final: 'c' }, () => {
        kitty.main = []
        kitty.alt = []
        otherKeys = 0
        schemeReports = false
        return false // (xterm resets the rest)
      }),
      // Colour scheme reports (mode 2031, as contour and ghostty have them):
      // a program that turns them on (Claude Code's "Auto" theme, herdr for
      // its panes) is told when the terminal goes dark or light, and may ask
      // (CSI ? 996 n). xterm.js answers the background colour itself (OSC 11).
      term.parser.registerCsiHandler({ prefix: '?', final: 'h' }, (p) => {
        if (p.includes(2031)) {
          schemeReports = true
          if (!replaying) reportedDark = termScheme().dark // (it learns it by asking: OSC 11, CSI ? 996 n)
        }
        return false
      }),
      term.parser.registerCsiHandler({ prefix: '?', final: 'l' }, (p) => {
        if (p.includes(2031)) schemeReports = false
        return false
      }),
      term.parser.registerCsiHandler({ prefix: '?', final: 'n' }, (p) => {
        if (num(p, 0, 0) !== 996) return false
        // (Answered by the window in charge; in a replay the answer is dropped.)
        if (replaying || following || !inCharge) return true
        reportedDark = termScheme().dark
        term.input(schemeReport(reportedDark), false)
        return true
      }),
    ]
    // A change is told by the window in charge of the terminal (another may
    // have another scheme), and not while a replay is parsed (the answer
    // would be dropped). After a replay, or when this window takes over, it
    // is told again whatever was last told from here (another window may
    // have told otherwise, or a report been dropped).
    syncSchemeRef.current = (again = false) => {
      if (!schemeReports || replaying || following || !inCharge) return
      const dark = termScheme().dark
      if (dark === reportedDark && !again) return
      reportedDark = dark
      term.input(schemeReport(dark), false)
    }
    // Disambiguate (1) or report all keys (8); else modifyOtherKeys.
    const keysApart = (): KeysApart => ((flags().at(-1) ?? 0) & 9 ? 'kitty' : otherKeys > 0 ? 'xterm' : null)
    keysApartRef.current = keysApart
    const clipSub = term.parser.registerOscHandler(52, (data) => {
      if (replaying || !mayCopy()) return true
      const text = clipboardText(data)
      if (text !== null) void remoteCopy(text)
      return true
    })
    const conn = new TermConnection(id, {
      // RIS (ESC c), and the modes the replay starts in, travel in the same
      // write queue as the replay that follows, so output still being parsed
      // cannot land after the reset.
      onReset: (modes) => {
        replaying = true
        try {
          term.write('\x1bc' + modes)
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
        // (Only a change: claims of the same size do not redraw anything.)
        if (cols !== lastSize.cols || rows !== lastSize.rows) {
          lastSize = { cols, rows }
          setSized(Date.now())
        }
        const tookOver = own && !inCharge
        inCharge = own
        if (!own) {
          // Another window took over after this one's last claim: an action
          // still waiting to be claimed came before that, so it is dropped.
          clearTimeout(claimTimer)
          claimTimer = undefined
        }
        const mine = fit.proposeDimensions()
        // A watcher always shows the owner's size: it never reports its own,
        // so fitting to its window could not be put right later.
        if (!watch && (own || !mine || (mine.cols === cols && mine.rows === rows))) {
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
          syncSchemeRef.current(tookOver)
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
        if (watch) return // herdr belongs to the owner
        setHerdr(m as HerdrSummary)
        // (lost: the gateway lost herdr; kept for the pane in front, but what it says was so then)
        setHerdrFresh(!(m as HerdrSummary).lost)
      },
      // Where the replay ends, CAN cancels a sequence it left unfinished
      // (an OSC 52 whose end would arrive live, though the request was
      // not): nothing that began in the replay acts as live output. It is
      // written as a byte, so it also ends a UTF-8 character cut there (the
      // first half of a C1 introducer). CAN shows nothing; the rest of a
      // sequence cut there shows as text.
      onLive: () => {
        const run = replayRun
        term.write(Uint8Array.of(0x18), () => {
          if (run === replayRun) {
            replaying = false
            settling = false
            syncSchemeRef.current(true)
          }
        })
      },
      // Every new socket: the herdr summary is old until this one sends its
      // own, and a replay begins.
      onHello: (version, instance) => {
        window.dispatchEvent(new CustomEvent(VERSION_EVENT, { detail: version })) // (a deploy: this page may be old)
        helloInstance.current = instance
        replayRun++
        settling = true
        setHerdrFresh(false)
        lastSize = { cols: 0, rows: 0 }
        setSized(0)
      },
      // Input typed while disconnected is sent once the connection is back,
      // unless there was too much of it or it waited too long.
      onDropped: () => droppedInput(),
    }, watch)
    connRef.current = conn
    conn.setShown(activeRef.current)
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
    let typedGen = 0 // bumped by a composition or text going in (see forgetTypedRef)
    const latched = () => mods.current.ctrl !== 'off' || mods.current.alt !== 'off'
    const onCompStart = () => {
      dropClaims() // the key became part of a composition
      ctx = '' // (what follows the composition's text, which stays in the field)
      ctxInField = false
      composing = true
      typedGen++
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
    type Core = { _compositionHelper?: { keydown(e: KeyboardEvent): boolean; isComposing?: boolean; _isSendingComposition?: boolean }; _unprocessedDeadKey?: boolean }
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
    let keySeq = 0 // keydowns seen (see onPrintableInput)
    // The plain ASCII typed on this line, kept in the textarea as what
    // precedes the caret. An emptied field reads to iOS input methods
    // (WeType) as a sentence start: each next letter came up capitalised,
    // autocapitalize="off" notwithstanding. While a claimed key is in flight
    // the field is empty and this holds it: xterm's comparison then sees no
    // change, whatever clears the field meanwhile (Enter, a paste). Chinese
    // is not kept: as what precedes, it made WeType space the Latin text
    // after it.
    let ctx = ''
    let ctxInField = false // the field holds ctx (put there by us)
    // At a claimed key: empty the field before xterm takes it as the baseline
    // of its comparison; ctx is put back with the key's text (see
    // onPrintableInput). Anything else in it (a composition's text, what a
    // key like ^U left) is dropped from the context. Not while a composition
    // that ended waits to be sent: xterm reads it from the field once the
    // input method is done with it, and the field is left as it is.
    const stashCtx = () => {
      if (!ta) return
      if (core!._compositionHelper!._isSendingComposition !== false) {
        ctx = ''
        ctxInField = false
        return
      }
      // (An empty field: a key is still in flight, or xterm emptied it.)
      if (!(ctxInField && ta.value === ctx) && ta.value !== '') ctx = ''
      ta.value = ''
      ctxInField = false
    }
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
        // The context goes back in, with plain ASCII added to it, once
        // xterm's comparison for the key has run (its zero-delay timer was
        // queued at keydown, before this one), unless a key, text or a
        // composition came meanwhile (the next key's own puts it back).
        if (t.value === '') {
          if (latched()) ctx = '' // sent as a control key (^U, ^W…)
          else if (/^[ -~]+$/.test(ie.data)) ctx += ie.data
          const seq = keySeq
          const gen = typedGen
          setTimeout(() => {
            if (seq !== keySeq || gen !== typedGen || composingRef.current() || t.value !== '') return
            t.value = ctx
            t.selectionStart = t.selectionEnd = ctx.length
            ctxInField = true
          }, 0)
        }
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
    forgetTypedRef.current = (one) => {
      ctx = one ? [...ctx].slice(0, -1).join('') : ''
      const gen = typedGen
      setTimeout(() => {
        // (Text that went in meanwhile, a whole composition even, may still
        // be read from the textarea by xterm: the clear is dropped.)
        if (gen !== typedGen) return
        // Not while a 229 key's text is pending either (iOS, see claim): xterm
        // compares the textarea before and after it, and an emptied one reads
        // as a deletion (a stray DEL).
        if (!ta || composingRef.current() || imeKeyPending !== 0 || claim !== null || ta.selectionStart !== ta.selectionEnd) return
        // Backspace takes only the last character (from ctx, above, so a
        // restore that ran meanwhile is not trimmed twice): an emptied field
        // brings the next letter up capitalised (see ctx).
        ta.value = ctx
        ta.selectionStart = ta.selectionEnd = ctx.length
        ctxInField = true
      }, 0)
    }

    const onBeforeInput = (e: Event) => {
      const ie = e as InputEvent
      typedGen++
      typedChar = !lastKeyAlt && !ie.isComposing && ie.inputType === 'insertText' && ie.data && [...ie.data].length === 1 ? ie.data : null
    }
    let pasting = false // a paste being sent (xterm sends it while the event runs)
    const onPasteEv = () => {
      ctx = '' // (xterm empties the field)
      typedChar = null
      pasting = true
      setTimeout(() => (pasting = false), 0)
    }
    ta?.addEventListener('compositionstart', onCompStart)
    ta?.addEventListener('compositionend', onCompEnd)
    ta?.addEventListener('beforeinput', onBeforeInput)
    // Capture on the container: it runs before any listener on the textarea,
    // xterm's own (also capturing) included, so a claimed event stops here.
    el.addEventListener('input', onPrintableInput, true)
    ta?.addEventListener('blur', dropClaims)
    el.addEventListener('paste', onPasteEv, true) // (xterm takes pastes on its element too)
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
      // Input methods that space Latin text from Chinese (WeType, Sogou, on
      // macOS too) still take the last committed Chinese as what precedes,
      // after a newline: the first text of the new line comes as " 3". Only
      // then is that space dropped; a space typed on its own comes alone,
      // and a paste is left as it is.
      if (conn.newLineAfterCJK && !pasting && /^ \S/.test(data) && !isTerminalReply(data)) data = data.slice(1)
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
      // Watching: no shortcut acts (composer, Ctrl+V upload, takeover);
      // keys reach nothing, as the terminal takes no input.
      if (watch) return true
      // The keypress of the claimed key: its input event carries the text.
      if (e.type === 'keypress' && claim !== null && e.code === claim) return false
      if (e.type === 'keyup') {
        if (claim !== null && (e.code === claim || (claim === IME_KEY && e.keyCode === 229))) claim = null
        if (e.key === 'Alt') lastKeyAlt = false
        return true
      }
      if (e.type !== 'keydown') return true
      keySeq++
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
          stashCtx() // before xterm takes the field's contents to compare
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
      // A key that moves on (a newline, the cursor) empties the textarea once
      // xterm is done with the key; Backspace takes its last character. The
      // text typed so far stays in it otherwise, and input methods take it as
      // what precedes the caret: WeType then puts a space between that
      // Chinese and the digits typed on a new line.
      // (With a modifier Backspace may take a word: the field is emptied.)
      if (specialFor[e.key]) forgetTypedRef.current(e.key === 'Backspace' && !e.altKey && !e.ctrlKey && !e.metaKey && !latched())
      else if ((e.ctrlKey || e.metaKey) && !modifierOnly) forgetTypedRef.current() // ^C, ^U…
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
          stashCtx()
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
        out(specialKey(special, { ctrl: m.ctrl || e.ctrlKey, alt: m.alt || e.altKey, shift: e.shiftKey }, term.modes.applicationCursorKeysMode, keysApart()))
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
      // Ctrl+Enter, for a program that tells it apart (keysApart).
      if (e.key === 'Enter' && e.ctrlKey && !e.altKey && !e.metaKey && keysApart() !== null) {
        e.preventDefault()
        takeOverRef.current()
        settleRef.current()
        out(specialKey('Enter', { ctrl: true, alt: false, shift: e.shiftKey }, false, keysApart()))
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

    // What the screen shows, for the session list (the owner's windows only).
    const stopPreviews = watch ? () => {} : startPreviews(id, term, () => conn.isOpen() && !replaying && !settling)

    let fitTimer: number | undefined
    const ro = new ResizeObserver(() => {
      clearTimeout(fitTimer)
      fitTimer = window.setTimeout(safeFit, 60)
    })
    ro.observe(el)
    if (!isTouch && activeRef.current) term.focus()

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
      document.removeEventListener('keydown', onStrayKey, true)
      ta?.removeEventListener('blur', onFieldBlur)
      el.removeEventListener('pointerdown', onQuietPress, true)
      el.removeEventListener('pointermove', onPromptMove, true)
      el.removeEventListener('pointercancel', onPromptCancel, true)
      el.removeEventListener('mousedown', onPromptMouse, true)
      el.removeEventListener('paste', onPasteEv, true)
      alive = false
      dataSub.dispose()
      clipSub.dispose()
      kbSubs.forEach((d) => d.dispose())
      userSub?.dispose()
      binSub.dispose()
      resizeSub.dispose()
      stopPreviews()
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
    syncSchemeRef.current()
    setChromeColor(scheme.theme.background ?? null)
    // Menus and dialogs opened over the terminal follow its colours.
    document.documentElement.classList.toggle('term-dark', scheme.dark)
    // Through the follow check: while another viewer's size is shown the grid
    // stays at that size (only its scale changes).
    safeFitRef.current()
  }, [prefs])
  useEffect(() => {
    const release = holdTermMode()
    return () => {
      release()
      if (!document.documentElement.classList.contains('term-mode')) setChromeColor(null)
    }
  }, [])
  // A tab brought to the front: its colours, its size (the window may have
  // changed meanwhile) and, on a desktop, the keyboard. Sent to the back:
  // open menus close (dialogs are not drawn while hidden, see below).
  const firstActive = useRef(true)
  useEffect(() => {
    connRef.current?.setShown(active) // (a tab in the back is not looked at: its alerts are sent)
    if (firstActive.current) {
      firstActive.current = false
      return
    }
    if (!active) {
      window.dispatchEvent(new Event(CLOSE_MENUS))
      setShortcutsOpen(false) // (its listeners are on the window)
      return
    }
    const scheme = termScheme()
    setChromeColor(scheme.theme.background ?? null)
    document.documentElement.classList.toggle('term-dark', scheme.dark)
    safeFitRef.current()
    // (Not under a dialog of this tab's, drawn again and focused just before.)
    if (!isTouch && !document.querySelector('[aria-modal="true"]')) termRef.current?.focus()
    // Brought here by a notification about an agent (?pane=), asking to
    // answer it (?reply=).
    const q = new URLSearchParams(location.search)
    if (!watch && location.pathname === '/t/' + id) {
      if (q.get('pane') && q.get('convo') !== '1') setWantPane(q.get('pane'))
      if (q.get('reply')) setWantReply(q.get('reply'))
      if (q.get('convo') === '1') {
        // (From before the agents page: there, at that agent.)
        const to = new URLSearchParams({ s: id })
        if (q.get('pane')) to.set('pane', q.get('pane')!)
        navigate('/agents?' + to, true)
      }
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [active])

  // An ended session: dim it, stop the cursor, close the composer.
  useEffect(() => {
    if (!ended) return
    const term = termRef.current
    if (term) {
      term.options.cursorBlink = false
      term.options.cursorInactiveStyle = 'none'
      term.blur()
    }
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
    return () => {
      vv?.removeEventListener('resize', apply)
      vv?.removeEventListener('scroll', apply)
      window.removeEventListener('resize', apply)
    }
  }, [])

  // Touch: one finger scrolls, two fingers resize text. Scrolling is delivered
  // as wheel events to xterm, which encodes them for whatever mouse protocol the
  // program enabled (or scrolls its own scrollback when none is active).
  useEffect(() => {
    const el = hostEl.current!
    let lastY = 0
    let startY = 0
    let at = { x: 0, y: 0 } // where reports point (herdr scrolls the pane there)
    let acc = 0
    let moved = false
    // The finger's last 100 ms, for the speed it leaves with.
    let trail: { y: number; t: number }[] = []
    let pinch: { dist: number; size: number } | null = null
    // Two fingers: a pinch (the font size) or, moved together, a swipe (the
    // next session sideways, herdr's next workspace up or down); which one
    // is decided by their first movement.
    let two: { x: number; y: number; cx: number; cy: number; dist: number; mode: 'undecided' | 'pinch' | 'swipe' } | null = null
    let swiped = false // a swipe ended: the finger still down moves nothing
    let inertia = 0
    const centre = (t: TouchList) => ({ x: (t[0].clientX + t[1].clientX) / 2, y: (t[0].clientY + t[1].clientY) / 2 })

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
      const n = Math.min(Math.abs(lines), 10)
      for (let i = 0; i < n; i++) {
        target.dispatchEvent(
          new WheelEvent('wheel', {
            deltaY: Math.sign(lines), deltaMode: WheelEvent.DOM_DELTA_LINE, bubbles: true, cancelable: true,
            clientX: at.x, clientY: at.y,
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
      trail = []
      pinch = e.touches.length === 2 ? { dist: dist(e.touches), size: getPrefs().fontSize } : null
      two = e.touches.length === 2 ? (() => { const c = centre(e.touches); return { ...c, cx: c.x, cy: c.y, dist: dist(e.touches), mode: 'undecided' as const } })() : null
      if (e.touches.length === 1) {
        startY = lastY = e.touches[0].clientY
        at = { x: e.touches[0].clientX, y: e.touches[0].clientY }
        trail = [{ y: lastY, t: performance.now() }]
      }
    }

    const onStart = (e: TouchEvent) => {
      if (e.touches.length === 1) {
        moved = false
        swiped = false
      }
      rebase(e)
    }
    const onMove = (e: TouchEvent) => {
      if (pinch && two && e.touches.length === 2) {
        e.preventDefault()
        moved = true
        const c = centre(e.touches)
        two.cx = c.x
        two.cy = c.y
        if (two.mode === 'undecided') {
          const along = Math.hypot(c.x - two.x, c.y - two.y)
          const apart = Math.abs(dist(e.touches) - two.dist)
          if (along > 24 && apart < along / 2) two.mode = 'swipe'
          else if (apart > 16) two.mode = 'pinch'
        }
        if (two.mode !== 'pinch') return
        const size = Math.round(Math.min(28, Math.max(9, pinch.size * (dist(e.touches) / pinch.dist))))
        if (size !== getPrefs().fontSize) setPrefs({ fontSize: size })
        return
      }
      if (e.touches.length !== 1 || swiped) return
      const y = e.touches[0].clientY
      if (!moved && Math.abs(y - startY) < 8) return
      moved = true
      e.preventDefault()
      e.stopPropagation()
      const now = performance.now()
      const dy = y - lastY
      trail.push({ y, t: now })
      while (trail.length > 2 && now - trail[0].t > 100) trail.shift()
      lastY = y
      acc -= dy
      const ch = cellHeight()
      const lines = Math.trunc(acc / ch)
      if (lines !== 0) acc -= wheel(lines) * ch
    }
    const onEnd = (e: TouchEvent) => {
      // A swipe is over when its first finger lifts: how far the two moved,
      // between them, while both were down.
      if (two?.mode === 'swipe') {
        const dx = two.cx - two.x
        const dy = two.cy - two.y
        if (Math.abs(dx) > 60 && Math.abs(dx) > 1.5 * Math.abs(dy)) swipeRef.current(dx < 0 ? 1 : -1, 'session')
        else if (Math.abs(dy) > 60 && Math.abs(dy) > 1.5 * Math.abs(dx)) swipeRef.current(dy < 0 ? 1 : -1, 'workspace')
        two = null
        pinch = null
        swiped = true
      }
      if (swiped) {
        e.preventDefault()
        return
      }
      if (e.touches.length > 0) {
        rebase(e)
        return
      }
      if (!moved) return
      e.preventDefault()
      if (pinch) {
        pinch = null
        two = null
        return
      }
      // The speed the finger left with (px per ms) over its last 100 ms;
      // none if it rested before lifting. It slows as iOS lists do, by time
      // (not frames: iPads draw 120 a second).
      const first = trail[0]
      const last = trail[trail.length - 1]
      const now = performance.now()
      let v = first && last && last.t > first.t && now - last.t < 60 ? -(last.y - first.y) / (last.t - first.t) : 0
      const ch = cellHeight()
      // Lines owed from the drag itself carry over into the fling.
      let rest = acc
      acc = 0
      if (Math.abs(v) < 0.15) v = 0
      let prev = now
      const step = (ts: number) => {
        const dt = Math.min(32, Math.max(0, ts - prev))
        prev = ts
        v *= Math.pow(0.998, dt) // (UIScrollView's normal rate)
        if (Math.abs(v) < 0.03) v = 0
        rest += v * dt
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
      two = null
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
  // The file going up now, shown over the terminal until it is there.
  const [sending, setSending] = useState<{ name: string; p: number; abort: AbortController } | null>(null)
  // One upload at a time, in the order asked for; leaving the page stops the
  // one going up and those waiting.
  const uploadTurn = useRef<Promise<void>>(Promise.resolve())
  const uploadAbort = useRef<AbortController | null>(null)
  const uploadsLive = useRef(true)
  useEffect(() => {
    uploadsLive.current = true
    return () => {
      uploadsLive.current = false
      uploadAbort.current?.abort()
    }
  }, [])
  const upload = useCallback(
    async (files: File[], paste?: (s: string) => void, toTerm = false) => {
      // Decided when the upload starts, not when it ends: the target, and
      // for the terminal the connection (after a reconnect the screen may
      // have changed, so the path is shown instead of pasted).
      const toDraft = !paste && !toTerm && !!composerRef.current
      const opens = connRef.current?.opens
      const before = uploadTurn.current
      let done = () => {}
      uploadTurn.current = new Promise<void>((r) => (done = r))
      await before
      try {
        for (const f of files) {
          if (!uploadsLive.current) break
          const abort = new AbortController()
          uploadAbort.current = abort
          const clear = () => setSending((u) => (u?.abort === abort ? null : u))
          setSending({ name: f.name || 'image', p: 0, abort })
          try {
            const r = await uploadFile(id, f, (p) => setSending((u) => (u?.abort === abort ? { ...u, p } : u)), abort.signal)
            clear()
            const quoted = r.prefix === '@' ? geminiPath(r.path) : shellPath(r.path)
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
            clear()
            if (e instanceof ApiError && e.code === 'aborted') {
              if (uploadsLive.current) toast(t('term.uploadStopped', { name: f.name || 'image' }))
              break
            }
            toast(errorText(e), true)
          }
        }
      } finally {
        done()
      }
      if (!uploadsLive.current) return
      if (!isTouch && !document.querySelector('[aria-modal="true"]')) (composerRef.current ?? termRef.current)?.focus()
    },
    [id, toast, insertDraft],
  )
  uploadRef.current = upload

  useEffect(() => {
    const el = hostEl.current!
    const onPaste = (e: ClipboardEvent) => {
      const files = [...(e.clipboardData?.files ?? [])]
      if (files.length === 0 || watch) return
      e.preventDefault()
      e.stopPropagation()
      void upload(files, undefined, true)
    }
    el.addEventListener('paste', onPaste, { capture: true })
    return () => el.removeEventListener('paste', onPaste, { capture: true })
  }, [upload, watch])

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
      const m = activeMods()
      outRef.current(specialKey(k, m, term?.modes.applicationCursorKeysMode ?? false, keysApartRef.current()))
      releaseOnce()
      forgetTypedRef.current(k === 'Backspace' && !m.ctrl && !m.alt)
    })
  // A custom key sends its own sequence; latched Ctrl/Alt do not apply to it.
  // then: once it has gone (after a composition it waited for); ok: whether
  // it is still to go, asked then.
  const sendSteps = (steps: Step[], then?: () => void, ok?: () => boolean) =>
    afterComposition(() => {
      if (ok && !ok()) return
      takeOverRef.current()
      settleRef.current()
      outRef.current(macroBytes(steps, termRef.current?.modes.applicationCursorKeysMode ?? false, keysApartRef.current()))
      releaseOnce()
      then?.()
    })
  const keyLayout = useMemo(() => layoutIn(prefs.keyLayout, prefs.customKeys), [prefs.keyLayout, prefs.customKeys])
  // A key of one's own or a long press (one that no longer parses does nothing).
  const sendKeys = (keys: string, then?: () => void, ok?: () => boolean) => {
    const r = parseMacro(keys)
    if ('steps' in r) sendSteps(r.steps, then, ok)
  }
  // A shortcut's keys sent again a moment after they went (OpenCode stops
  // on a second Esc, not on two at once): not once the focused pane, the
  // connection or the page has changed meanwhile (they would go elsewhere),
  // nor another such pick made. (Each pick's own number.)
  const againGen = useRef(0)
  useEffect(() => () => void againGen.current++, [herdr?.focused_pane, status, active])
  // The shortcut panel, over the key bar.
  const [shortcutsOpen, setShortcutsOpen] = useState(false)
  const closeShortcuts = useCallback(() => setShortcutsOpen(false), [])
  // Closing a pane is asked first, for the pane focused when it was picked.
  const [closingPane, setClosingPane] = useState<string | null>(null)
  const herdrFailed = (e: unknown) => (e instanceof ApiError && e.code === 'herdr_busy' ? t('herdr.busy') : t('sc.herdrFailed'))
  const pickShortcut = async (s: Shortcut) => {
    setShortcutsOpen(false)
    const gen = ++againGen.current // (another pick: an Esc still to follow goes no more)
    if ('keys' in s) {
      if (!s.again) return sendKeys(s.keys)
      const live = () => gen === againGen.current
      // (Sent with the next at once, it is read as one key: OpenCode's Esc.
      // So the first must go out then, not be held in a queue, and the
      // second goes over the same connection, nothing held before it.)
      const conn = connRef.current
      const opens = conn?.opens
      const straight = () => live() && !!conn && conn === connRef.current && conn.goesAtOnce() && conn.opens === opens && !heldRef.current()
      let first = false
      const go = () => live() && ((first = straight()), true)
      sendKeys(s.keys, () => void (first && window.setTimeout(() => sendKeys(s.keys, undefined, straight), 300)), go)
      return
    }
    if (s.herdr === 'close_pane') {
      // Only from a summary known to be current (not one kept from before a
      // reconnect, where focus may have moved elsewhere meanwhile).
      if (herdr?.focused_pane && herdrFresh && status === 'open') setClosingPane(herdr.focused_pane)
      else toast(t('sc.herdrFailed'), true)
      return
    }
    try {
      await api('POST', `/api/sessions/${id}/herdr`, { action: s.herdr })
    } catch (e) {
      toast(herdrFailed(e), true)
    }
  }
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
  // A path picked in the file panel goes where an uploaded file's path would;
  // after a reconnect since the panel opened, the screen may have changed,
  // so it is only shown.
  const insertPath = (p: string) => {
    setFilesOpen(false)
    const quoted = shellPath(p)
    if (!quoted) return toast(p)
    if (composerRef.current) return insertDraft(quoted + ' ')
    const c = connRef.current
    const opens = filesOpens.current
    if (!c?.isOpen() || c.opens !== opens) return toast(t('term.uploadNotPasted', { path: p }), true)
    afterComposition(() => {
      if (!c.isOpen() || c.opens !== opens) return toast(t('term.uploadNotPasted', { path: p }), true)
      settleRef.current()
      termRef.current?.paste(quoted + ' ')
      if (!isTouch) termRef.current?.focus()
    })
  }
  const cycle = (m: ModState): ModState => (m === 'off' ? 'once' : m === 'once' ? 'lock' : 'off')
  const keyboardField = () => {
    // The field in use: the one focused, else the composer when it is open.
    const field = hostEl.current?.querySelector('textarea')
    const composer = composerRef.current
    const target = document.activeElement === field ? field : composer ?? field
    if (!target) return null
    const focused = document.activeElement === target
    // A focused field shows its keyboard, unless the keyboard was seen put
    // away (iOS, see keyboardWentDown).
    const shown = focused && !keyboardWentDown
    return { target, focused, shown }
  }
  const leaveKeyboardField = (target: HTMLElement) => {
    settleRef.current() // a composition that just ended is sent, not cleared by the blur
    target.blur()
  }
  // iOS brings up the keyboard for a field that takes focus on a tap, but
  // not reliably for one left and focused again within the same event: a
  // focused field whose keyboard was put away is left as the press starts
  // and takes focus when it ends (toggleKeyboard).
  const leftOnDown = useRef<{ el: HTMLElement; at: number } | null>(null)
  const keyboardDown = () => {
    if (leftOnDown.current?.el.isConnected && document.activeElement !== leftOnDown.current.el) return // (a second finger)
    const k = keyboardField()
    leftOnDown.current = null
    if (!k?.focused || k.shown) return
    leaveKeyboardField(k.target)
    leftOnDown.current = { el: k.target, at: Date.now() }
  }
  const toggleKeyboard = () => {
    // The field left as this press started is the one to bring back.
    const left = leftOnDown.current
    leftOnDown.current = null
    const k = keyboardField()
    if (!k) return
    const target = left && Date.now() - left.at < 1000 && left.el.isConnected ? left.el : k.target
    if (target === k.target && k.focused) leaveKeyboardField(target)
    if (target === k.target && k.shown) return
    allowFocus(() => target.focus())
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
    const text = expandPastes(draft, 4 << 20)
    if (text === null) {
      toast(t('term.tooLong'), true)
      return
    }
    let payload = text.replace(/\r?\n/g, '\r')
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
    remember(draft)
    histPos.current = null
    setDraft('')
    composerRef.current?.focus()
  }

  // Up from the start of the box: the entry before the one shown (the draft
  // there is put aside); Down from its end: the next, then the draft again.
  const historyStep = (dir: -1 | 1): boolean => {
    if (histPos.current === null && dir > 0) return false
    const h = sentHistory()
    if (histPos.current === null) histStash.current = draft
    // Where the box is in the list now (an entry gone: past the newest).
    let pos = histPos.current !== null ? h.lastIndexOf(histPos.current) : h.length
    if (pos < 0) pos = h.length
    if (dir < 0) {
      if (pos === 0) return true
      pos--
    } else {
      pos++
    }
    histPos.current = pos < h.length ? h[pos] : null
    const text = histPos.current ?? histStash.current
    setDraft(text)
    requestAnimationFrame(() => {
      const el = composerRef.current
      if (el) el.selectionStart = el.selectionEnd = dir < 0 ? 0 : text.length
    })
    return true
  }

  // "@", "/" and "$" in the composer offer files, commands and skills.
  const completion = useCompletion({
    sessionId: id,
    field: composerRef,
    draft,
    setDraft: (s) => {
      histPos.current = null
      setDraft(s)
    },
    agent: herdr?.focused_agent,
  })

  // What the keys do in the composer, shown by the send button on a computer.
  // Return and Shift as words on a Mac: ↩ and ⇧ are not in every UI font,
  // and the one they fall back to sets them small and low beside the text.
  const enterKey = isMac ? 'Return' : 'Enter'
  const modEnter = isMac ? '⌘Return' : 'Ctrl+Enter'
  const composerSendKey = prefs.composerEnter === 'send' ? enterKey : modEnter
  const composerKeys =
    prefs.composerEnter === 'send'
      ? t('term.composerKeysSend', { send: enterKey, newline: isMac ? 'Shift+Return' : 'Shift+Enter' })
      : t('term.composerKeysNewline', { send: modEnter })
  // The suggestion goes into the box, to be edited or sent.
  const takeSuggestion = () => {
    if (!suggestion) return
    const s = suggestion
    histPos.current = null
    setDraft(s)
    requestAnimationFrame(() => {
      const el = composerRef.current
      if (!el) return
      el.focus()
      el.selectionStart = el.selectionEnd = s.length
    })
  }
  const toggleComposer = () => {
    if (watch) return
    if (composer) {
      setComposer(false)
      rememberComposer(id, false)
      if (!isTouch) termRef.current?.focus()
      return
    }
    // Mount and focus inside the tap itself: iOS only raises the keyboard for a
    // focus that happens during the user's gesture.
    flushSync(() => setComposer(true))
    rememberComposer(id, true)
    composerRef.current?.focus()
  }
  openComposerRef.current = () => {
    if (ended || watch) return
    flushSync(() => setComposer(true))
    rememberComposer(id, true)
    composerRef.current?.focus()
  }

  // (Fitted to its text up to a part of what is visible: the terminal and the key bar keep some room.)
  useComposerFit(composerRef, draft, suggestion ?? t('term.composerPlaceholder'), composer)

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

  const hostIdForReconnect = watch ? 0 : info?.host_id ?? Number(local.get('corral.sessHost.' + id) || 0)
  // (A tab in the back opened again stays in the back: its new session shows up as a tab.)
  const { connect, busyHost, dialogs } = useConnect((s) => activeRef.current && navigate('/t/' + s.id, true))
  // Closed as the gateway stopped (internal/term: ReasonShutdown).
  const gatewayStopped = end?.state === 'closed' && end.reason === 'server shutting down'
  // What this terminal was opened as, again (no choosing).
  const againChoice = (): OpenChoice | undefined => {
    const c = info ? choiceOf(info) : undefined
    if (c) return c
    try {
      return JSON.parse(local.get('corral.sessOpen.' + id) ?? 'null') ?? undefined
    } catch {
      return undefined
    }
  }
  // (connecting: from asking for the host until the attempt is over, a
  // question it asked included; see reopening.)
  const reconnect = async () => {
    if (!hostIdForReconnect) return navigate('/')
    setConnecting(true)
    try {
      const h = await api<Host>('GET', '/api/hosts/' + hostIdForReconnect)
      if (pageGone.current) return // (left meanwhile)
      connect(h, againChoice())
      setAsked(true)
    } catch (e) {
      if (pageGone.current) return
      setConnecting(false)
      toast(errorText(e), true)
    }
  }
  const [connecting, setConnecting] = useState(false)
  const [asked, setAsked] = useState(false) // connect() was called (its busyHost shows the attempt)
  useEffect(() => {
    if (connecting && asked && busyHost === null && !dialogs) {
      setConnecting(false)
      setAsked(false)
    }
  }, [connecting, asked, busyHost, dialogs])
  // Gone with the gateway (restarted: an update) or with the connection to
  // the host, a tmux or herdr session is still there on the host: it is
  // opened again by itself after a short countdown, in the tab in front, once
  // (a failure leaves the button). A plain shell is not: what ran in it ended,
  // and a new one is the user's choice.
  const againKind = againChoice()?.kind
  // Gone: by itself, also what someone ended elsewhere meanwhile; only when
  // the gateway has restarted since this window last connected (another run
  // answers) was it not ended by anyone.
  const [goneByRestart, setGoneByRestart] = useState(false)
  const [probing, setProbing] = useState(false)
  useEffect(() => {
    if (status !== 'gone' || watch || !helloInstance.current) return
    const was = helloInstance.current
    let live = true
    setProbing(true)
    void (async () => {
      // (The gateway may still be starting: for a minute.)
      const until = Date.now() + 60_000
      while (live && Date.now() < until) {
        const b = await probeBootstrap()
        if (b) {
          if (live) setGoneByRestart(!!b.instance && b.instance !== was)
          break
        }
        await new Promise((r) => setTimeout(r, 2000))
      }
      if (live) setProbing(false)
    })()
    return () => {
      live = false
    }
  }, [status, watch])
  const autoAgain = ended && !watch && hostIdForReconnect > 0 && (goneByRestart || end?.state === 'lost' || gatewayStopped) && (againKind === 'tmux' || againKind === 'herdr')
  const [autoIn, setAutoIn] = useState<number | null>(null)
  const autoTried = useRef(false)
  // In front, after a countdown; a tab in the back (with tabs) at once, so
  // its session is back as a tab too.
  useEffect(() => {
    if (!autoAgain || autoTried.current) return
    setAutoIn((n) => n ?? (activeRef.current ? AUTO_RECONNECT_S : 0))
  }, [autoAgain])
  // The gateway may still be starting: it is asked every 2 s, for a minute,
  // before the session is opened.
  const [autoWaiting, setAutoWaiting] = useState(false)
  useEffect(() => {
    pageGone.current = false // (mounted again: React's development mode does)
    return () => {
      pageGone.current = true
    }
  }, [])
  useEffect(() => {
    if (autoIn === null) return
    if (autoIn <= 0) {
      autoTried.current = true
      setAutoIn(null)
      void (async () => {
        setAutoWaiting(true)
        const until = Date.now() + 60_000
        while (Date.now() < until && !pageGone.current && autoWaitingRef.current !== 'stopped') {
          if (await probeBootstrap()) break
          await new Promise((r) => setTimeout(r, 2000))
        }
        if (pageGone.current) return
        setAutoWaiting(false)
        if (autoWaitingRef.current !== 'stopped') void reconnect()
      })()
      return
    }
    const tm = window.setTimeout(() => setAutoIn((n) => (n === null ? null : n - 1)), 1000)
    return () => clearTimeout(tm)
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [autoIn])
  const autoWaitingRef = useRef<'' | 'stopped'>('')
  const stopAuto = () => {
    autoTried.current = true
    autoWaitingRef.current = 'stopped'
    setAutoIn(null)
    setAutoWaiting(false)
  }

  const slow = status === 'open' && latency !== null && latency > HIGH_LATENCY
  const statusText =
    ended ? t('term.endedShort') :
    status === 'open' ? (slow ? t('term.latency', { ms: latency }) : t('term.connected')) + (info?.recording ? ' · ' + t('term.recording') : '') +
      (!watch && info?.watching ? ' · ' + t('term.watchedBy', { n: info.watching }) : '') :
    status === 'connecting' ? t('term.connecting') :
    status === 'reconnecting' ? t('term.reconnecting') :
    status === 'offline' ? t('term.offline') :
    status === 'full' ? t('term.fullShort') : ''
  const statusClass = ended ? 's-ended' : slow ? 's-slow' : 's-' + status
  // With tabs, the open session's state beyond "connected": slow, recorded, watched.
  const tabNote =
    status === 'open' && !ended
      ? [slow && t('term.latency', { ms: latency }), info?.recording && t('term.recording'), !watch && info?.watching && t('term.watchedBy', { n: info.watching })].filter(Boolean).join(' · ')
      : ''
  const paused = !ended && (status === 'reconnecting' || status === 'offline')
  const herdrStale = paused || !herdrFresh
  // Agents waiting for an answer, while what herdr reported is current.
  const waiting = herdr && !herdrStale && !ended && !watch
    ? herdr.workspaces.flatMap((w) => w.agents.filter((a) => a.status === 'blocked').map((a) => ({ a, ws: w.label })))
    : []
  const waitingShown = useRef(waiting) // the list the waiting menu opened with
  // Two fingers moved together (touch screens): sideways the next or previous
  // running session, in the order of the tabs; up or down herdr's next or
  // previous workspace. Where it went is said briefly.
  swipeRef.current = (dir, what) => {
    if (watch || ended) return
    if (what === 'workspace') {
      const ws = herdr && !herdrStale ? herdr.workspaces : []
      const i = ws.findIndex((w) => w.focused)
      if (ws.length < 2 || i < 0) return
      const next = ws[(i + dir + ws.length) % ws.length]
      api('POST', `/api/sessions/${id}/herdr`, { action: 'focus_workspace', target: next.id })
        .then(() => toast(t('swipe.workspace', { name: next.label || String(next.number) })))
        .catch((e) => toast(e instanceof ApiError && e.code === 'herdr_busy' ? t('herdr.busy') : t('sc.herdrFailed'), true))
      return
    }
    api<SessionInfo[]>('GET', '/api/sessions')
      .then((all) => {
        const list = all.filter((x) => x.state === 'running').sort((a, b) => a.created_at - b.created_at)
        const i = list.findIndex((x) => x.id === id)
        if (list.length < 2 || i < 0) return toast(t('swipe.only'))
        const next = list[(i + dir + list.length) % list.length]
        navigate('/t/' + next.id, true)
        toast(t('swipe.session', { name: next.host_name + (next.title ? ' · ' + next.title : '') }))
      })
      .catch(() => toast(t('term.sessionsFailed'), true))
  }
  const [waitingOpen, setWaitingOpen] = useState(false) // (it stays while open, even with none left)
  const countOf = (st: string) => (herdr && !herdrStale && !ended && !watch ? herdr.workspaces.reduce((n, w) => n + w.agents.filter((a) => a.status === st).length, 0) : 0)
  const doneCount = countOf('done')
  const workingCount = countOf('working')
  useEffect(() => {
    onInfo?.(id, { status: statusClass, statusText, waiting: waiting.length, done: doneCount, working: workingCount, reopening: probing || autoIn !== null || autoWaiting || connecting })
  }, [onInfo, id, statusClass, statusText, waiting.length, doneCount, workingCount, probing, autoIn, autoWaiting, connecting])

  const endText = status === 'revoked' ? t('watch.revoked') : status === 'gone' ? (watch ? t('watch.gone') : t('term.ended.gone')) :
    end ? (end.state === 'exited' ? t('term.ended.exited', { code: end.code }) : end.state === 'lost' ? t('term.ended.lost') : gatewayStopped ? t('term.ended.restart') : t('term.ended.closed')) :
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
    return [
      sessionsFailed ? { label: t('common.retry'), icon: <RotateCw size={16} />, keepOpen: true, onSelect: loadSessions } : null,
      ...list.map((x) => ({
        label: `${x.host_name} · ${sessionWhich(x, list)}${x.title ? ' · ' + x.title : ''}`,
        icon: x.id === id ? <Check size={16} /> : <span className="menu-icon-space" />,
        onSelect: () => x.id !== id && navigate('/t/' + x.id),
      })),
      { label: t('term.backToHosts'), icon: <Server size={16} />, separator: list.length > 0 || sessionsFailed, onSelect: () => navigate('/') },
    ]
  }

  // Watching: only what changes this window's view.
  const watchItems = (): (MenuItem | null)[] => [
    { label: t('term.copyMode'), icon: <TextSelect size={16} />, onSelect: openCopyMode },
    { label: t('settings.termTheme'), icon: <Palette size={16} />, separator: true, onSelect: () => setSchemesOpen(true) },
    { label: t('term.fontLarger'), icon: <Plus size={16} />, onSelect: () => setPrefs({ fontSize: Math.min(28, getPrefs().fontSize + 1) }) },
    { label: t('term.fontSmaller'), icon: <Minus size={16} />, onSelect: () => setPrefs({ fontSize: Math.max(9, getPrefs().fontSize - 1) }) },
  ]
  const moreItems = (): (MenuItem | null)[] => watch ? watchItems() : [
    { label: t('term.upload'), icon: <ImagePlus size={16} />, onSelect: () => fileRef.current?.click() },
    ended ? null : { label: t('files.menu'), icon: <FolderOpen size={16} />, onSelect: openFiles },
    ended ? null : { label: t('share.title'), icon: <Eye size={16} />, onSelect: () => setShareOpen(true) },
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
                  if (onEnd) onEnd()
                  else navigate('/', true)
                }}
              />,
            ),
        },
  ]

  // The composer, under the terminal.
  const composerEl = (
    <div className="composer">
      {completion.list}
      <div className="composer-box">
        <textarea
          ref={composerRef}
          className="composer-input"
          value={draft}
          aria-label={t('term.composer')}
          // Commands and code: no capital first letter, corrected
          // words or curly quotes (iOS), as in the terminal's field.
          autoCapitalize="off"
          autoCorrect="off"
          spellCheck={false}
          placeholder={suggestion ?? t('term.composerPlaceholder')}
          rows={1}
          onChange={(e) => {
            histPos.current = null // (an entry edited is a draft of its own)
            setDraft(e.target.value)
          }}
          onSelect={() => completion.update()}
          {...completion.field}
          onBlur={() => setTimeout(completion.update)}
          onPaste={(e) => {
            const files = [...(e.clipboardData?.files ?? [])]
            if (files.length > 0) {
              e.preventDefault()
              void upload(files)
              return
            }
            // A long paste goes in as a label, to keep the box readable; it is sent as
            // the text (deleting the label drops it).
            const text = e.clipboardData?.getData('text/plain') ?? ''
            const lines = text.split('\n').length
            if (lines <= PASTE_LINES && text.length <= PASTE_CHARS) return
            e.preventDefault()
            insertDraft(keepPaste(text, (n) => t('term.pasteLabel', { n, lines })))
          }}
          onKeyDown={(e) => {
            if (e.nativeEvent.isComposing || e.keyCode === 229) return
            if (completion.onKey(e)) return
            if (e.key === 'Tab' && !e.shiftKey && !e.altKey && !e.metaKey && !e.ctrlKey && !draft && suggestion) {
              e.preventDefault()
              takeSuggestion()
              return
            }
            const el = e.currentTarget
            const plain = !e.shiftKey && !e.altKey && !e.metaKey && !e.ctrlKey && el.selectionStart === el.selectionEnd
            if (plain && e.key === 'ArrowUp' && el.selectionStart === 0 && historyStep(-1)) {
              e.preventDefault()
            } else if (plain && e.key === 'ArrowDown' && el.selectionEnd === el.value.length && historyStep(1)) {
              e.preventDefault()
            } else if (e.key === 'Enter' && (e.metaKey || e.ctrlKey)) {
              e.preventDefault()
              sendComposer(true)
            } else if (e.key === 'Enter' && hasKeyboardPointer() && prefs.composerEnter === 'send' && !e.shiftKey && !e.altKey) {
              // On a computer Enter sends, as in the agents' own input boxes and chat apps;
              // Shift+Enter is a new line (or the other way round, as set).
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
          <Menu
            label={t('term.history')}
            note={sentHistory().length === 0 ? t('term.historyEmpty') : undefined}
            items={() =>
              [...sentHistory()].reverse().map((h) => ({
                label: oneLine(h),
                onSelect: () => {
                  // The draft there comes back with Down after the newest entry.
                  if (histPos.current === null) histStash.current = draft
                  histPos.current = h
                  setDraft(h)
                  requestAnimationFrame(() => {
                    const el = composerRef.current
                    if (!el) return
                    el.focus()
                    el.selectionStart = el.selectionEnd = h.length
                  })
                },
              }))
            }
            button={(open, toggle) => (
              <button type="button" className="icon-btn" aria-expanded={open} onClick={toggle} aria-label={t('term.history')} title={t('term.history') + (isTouch ? '' : ' (↑)')}>
                <History size={18} />
              </button>
            )}
          />
          <button type="button" className="btn btn-ghost composer-insert" onClick={() => sendComposer(false)} disabled={!draft} title={t('term.insertHint')}>
            {t('term.insert')}
          </button>
          <div className="spacer" />
          {suggestion && !draft && !hasKeyboardPointer() && (
            <button type="button" className="btn btn-ghost composer-insert" onClick={takeSuggestion} title={suggestion}>
              {t('term.useSuggestion')}
            </button>
          )}
          {hasKeyboardPointer() && <span className="composer-keys" aria-hidden="true">{suggestion && !draft ? t('term.suggestionKey') + ' · ' : ''}{composerKeys}</span>}
          <button type="button" className="composer-send" onClick={() => sendComposer(true)} aria-label={t('term.send')} title={t('term.send') + (isTouch ? '' : ' (' + composerSendKey + ')')}>
            <ArrowUp size={18} strokeWidth={2.25} />
          </button>
        </div>
      </div>
    </div>
  )

  return (
    <div
      className={'term-root ' + (scheme.dark ? 'dark' : 'light') + (active ? '' : ' inactive')}
      inert={!active}
      aria-hidden={active ? undefined : true}
      style={chromeVars(scheme) as React.CSSProperties}
      ref={rootRef}
      onDragOver={(e) => {
        if (!watch && e.dataTransfer.types.includes('Files')) {
          e.preventDefault()
          setDragOver(true)
        }
      }}
      // Leaving the page or the window, or a drag given up (Esc): the
      // element left is often one inside it, and relatedTarget is then
      // outside or null. (Moving between elements inside brings a dragover
      // in the same task, so the hint does not flicker.)
      onDragLeave={(e) => {
        if (!(e.relatedTarget instanceof Node) || !e.currentTarget.contains(e.relatedTarget)) setDragOver(false)
      }}
      onDrop={(e) => {
        setDragOver(false)
        // Only files are ours; dragged text keeps the browser's own handling
        // (dropping into the composer inserts it).
        const files = [...e.dataTransfer.files]
        if (files.length === 0 || watch) return
        e.preventDefault()
        void upload(files)
      }}
    >
      <header className="term-bar">
        <button className="icon-btn" onClick={() => navigate('/')} aria-label={t('term.backToHosts')} title={t('term.backToHosts')}>
          <ChevronLeft size={20} />
        </button>
        {watch ? (
          <div className="term-title term-title-static">
            <span className="term-host">
              <span className="term-host-name">{info?.host_name ?? ''}{owner ? ' · ' + t('watch.owner', { owner }) : ''}</span>
            </span>
            <span className={'term-status ' + statusClass} aria-live="polite">
              <span className="dot" aria-hidden="true" />
              {t('watch.status', { status: statusText })}
            </span>
          </div>
        ) : tabs ? (
          <>
            {tabs}
            {/* (Connection trouble shows on the tab; what else the bar would say, here.) */}
            {tabNote ? (
              <span className={'term-status term-tab-note ' + statusClass} aria-live="polite">{tabNote}</span>
            ) : (
              <span className="sr-only" aria-live="polite">{statusText}</span>
            )}
          </>
        ) : (
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
        )}
        <div className="spacer" />
        {(waiting.length > 0 || waitingOpen) && (
          // Only while an agent waits (herdr's own sidebar lists them all):
          // one is answered at once, several are picked from a menu. The
          // menu keeps the list it opened with, so a summary arriving while
          // it is open moves nothing under the pointer or the focus.
          <Menu
            label={t('herdr.waitingMenu')}
            onOpenChange={setWaitingOpen}
            items={() => waitingShown.current.map(({ a, ws }) => ({ label: `${ws} · ${a.agent}${a.title ? ' · ' + a.title : ''}`, onSelect: () => setReplyTo(a) }))}
            button={(open, toggle) => (
              <button
                className="icon-btn herdr-btn"
                aria-expanded={open || waiting.length > 1 ? open : undefined}
                onClick={(e) => {
                  if (open || waiting.length > 1) {
                    if (!open) waitingShown.current = waiting
                    return toggle(e)
                  }
                  setReplyTo(waiting[0].a)
                }}
                aria-label={waiting.length === 1 ? t('reply.open', { agent: waiting[0].a.agent }) : t('herdr.waiting', { n: waiting.length })}
                title={t('herdr.waiting', { n: waiting.length })}
              >
                <MessageCircleReply size={19} />
                <span className="herdr-count">{waitingOpen ? waitingShown.current.length : waiting.length}</span>
              </button>
            )}
          />
        )}
        {!ended && !watch && (
          <button className="icon-btn" aria-pressed={composer} onClick={toggleComposer} aria-label={t('term.composer')} title={t('term.composer') + (isTouch ? '' : isMac ? ' (⇧⌘↩)' : ' (Ctrl+Shift+Enter)')}>
            <TextCursorInput size={19} />
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

      {paused && <div className="term-paused" role="status">{watch ? t('watch.paused') : t('term.paused')}</div>}
      {status === 'full' && (
        <div className="term-paused term-following term-full" role="alert">
          <span>{t('term.full')}</span>
          <button className="link-btn" onClick={() => connRef.current?.retry()}>{t('common.retry')}</button>
        </div>
      )}
      {following && !paused && !ended && !watch && (
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
      <div className={'term-body' + (paused ? ' is-paused' : '') + (ended ? ' is-ended' : '')}>
        <div className="term-host-el" ref={hostEl} />
        {dragOver && <div className="term-drop">{t('term.dropHint')}</div>}
        {sending && (
          <div className="term-sending" role="status">
            <span className="term-sending-text">{t('term.uploading', { name: sending.name })} {Math.round(sending.p * 100)}%</span>
            <button type="button" className="icon-btn" onClick={() => sending.abort.abort()} aria-label={t('term.uploadStop')} title={t('term.uploadStop')}>
              <X size={16} />
            </button>
          </div>
        )}
        {replyTo && !watch && !ended && active && (
          // One dialog per pane (keyed): nothing of another pane's carries over.
          <ReplyDialog
            key={replyTo.pane_id}
            sessionId={id}
            agent={replyTo}
            resizedAt={sized}
            onLoaded={() => forgetReply(replyTo.pane_id)}
            onClose={() => {
              forgetReply(replyTo.pane_id)
              setWantReply((cur) => (cur === replyTo.pane_id ? null : cur))
              setReplyTo((cur) => (cur?.pane_id === replyTo.pane_id ? null : cur))
            }}
          />
        )}
      </div>

      {ended ? (
        <div className="term-ended" role="alert">
          <p>{endText}</p>
          {(autoIn !== null || autoWaiting) && <p className="hint" aria-live="polite">{autoWaiting ? t('term.autoWaiting') : t('term.autoReopen', { n: autoIn ?? 0 })}</p>}
          <div className="term-ended-actions">
            {autoIn !== null || autoWaiting ? (
              <button className="btn btn-ghost" onClick={stopAuto}>{t('common.cancel')}</button>
            ) : (
              <button className="btn btn-ghost" onClick={() => navigate('/')}>{t('common.back')}</button>
            )}
            {hostIdForReconnect > 0 && (
              <button className="btn btn-primary" onClick={() => { stopAuto(); void reconnect() }}>
                {autoIn !== null || autoWaiting ? t('term.reopenNow') : t('term.reopen')}
              </button>
            )}
          </div>
        </div>
      ) : (
        <>
          {composer && composerEl}

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
              onKeyboardDown={keyboardDown}
              layout={keyLayout}
              onSteps={sendKeys}
              onPanel={() => setShortcutsOpen((o) => !o)}
              panel={
                shortcutsOpen && active && (
                  <ShortcutPanel
                    {...panelPages(info?.persistence, herdr?.focused_agent, lastPanelPage())}
                    herdrKeys={herdr?.keys}
                    keysUnknown={!herdr || herdr.keys_unknown} // (no summary yet: not known either)
                    onPick={(s) => void pickShortcut(s)}
                    onClose={closeShortcuts}
                  />
                )
              }
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

      {/* Dialogs are drawn above everything (portals) and stack as layers:
          a hidden tab keeps them open but draws them only when back. */}
      {active && (
        <>
          {copyText !== null && <CopyMode text={copyText} onClose={() => setCopyText(null)} />}
          {schemesOpen && <SchemeDialog onClose={() => setSchemesOpen(false)} />}
          {closingPane && !ended && (
            <Confirm
              title={t('sc.closePaneTitle')}
              body={t('sc.closePaneBody')}
              confirm={t('sc.closePane')}
              danger
              onConfirm={async () => {
                try {
                  await api('POST', `/api/sessions/${id}/herdr`, { action: 'close_pane', target: closingPane })
                } catch (e) {
                  throw { human: herdrFailed(e) }
                }
              }}
              onClose={() => setClosingPane(null)}
            />
          )}
          {portsOpen && info && <PortsDialog hostId={info.host_id} onClose={() => setPortsOpen(false)} />}
          {shareOpen && !ended && <ShareDialog sessionId={id} onClose={() => setShareOpen(false)} />}
          {filesOpen && !ended && (
            <FilesDialog sessionId={id} start={filesPath.current} onPath={(p) => (filesPath.current = p)} onInsert={insertPath} onClose={() => setFilesOpen(false)} />
          )}
          {code.dialog}
          {confirm}
          {dialogs}
        </>
      )}
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
