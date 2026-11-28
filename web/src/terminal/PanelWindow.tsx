import { Fragment, useCallback, useEffect, useId, useLayoutEffect, useMemo, useRef, useState } from 'react'
import { useWhenGone } from '../shown'
import { shouldIgnorePanelPollError } from './busyCode'
import { api, ApiError, errorText } from '../api'
import { t, type I18nKey } from '../i18n'
import { isCoarse } from '../components/ui'
import { AppLink } from '../components/AppLink'
import { ansiLines, type AnsiRun } from './ansi'
import { readNoticeDialog, type NoticeAction } from './noticeDialog'
import { PaneTerminal } from './PaneTerminal'
import { claimPaneTerminal } from './termViewKeep'
import type { ScreenBlocker } from './liveTerminalPolicy'

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
const EVERY_LIVE = 350
const ESC_AGAIN = 1000
const QUEUE = 16
const FAILS = 4
// (The look for a notice before the live terminal, at most this long: not read by then — the host busy,
// slow — the terminal as it was before the look, not "loading" till it answers. Review of 11-24.)
const NOTICE_LOOK_MS = 4000

// (Shift+Tab: under a plan, it approves.)
const settles = (k: string) => k === 'enter' || k === 'space' || k === 'shift+tab' || /^[0-9]$/.test(k)
type Pressed = { k: string; digest: string }

function keyOf(e: React.KeyboardEvent): string | null {
  if (e.altKey || e.metaKey || e.ctrlKey || e.nativeEvent.isComposing || e.keyCode === 229) return null
  const named: Record<string, string> = { ArrowUp: 'up', ArrowDown: 'down', ArrowLeft: 'left', ArrowRight: 'right', Enter: 'enter', Escape: 'esc', Backspace: 'backspace', ' ': 'space' }
  if (e.key === 'Tab') return e.shiftKey ? 'shift+tab' : 'tab'
  if (named[e.key]) return named[e.key]
  return /^[A-Za-z0-9._/]$/.test(e.key) ? e.key : null
}

const TOUCH_KEYS: [string, string][] = [['up', '↑'], ['down', '↓'], ['left', '←'], ['right', '→'], ['tab', 'Tab'], ['space', 'Space'], ['enter', 'Enter'], ['esc', 'Esc']]
const KEY_WORD: Record<string, string> = Object.fromEntries([...TOUCH_KEYS, ['shift+tab', 'Shift+Tab'], ['backspace', 'Backspace']])
const keyWord = (k: string) => KEY_WORD[k] ?? k
const NOTICE_LABELS: Record<string, I18nKey> = {
  continue: 'notice.act.continue',
  confirm: 'notice.act.confirm',
  cancel: 'notice.act.cancel',
  close: 'notice.act.close',
  'stay in auto mode': 'notice.act.stayAuto',
  'leave for accept edits mode and stop': 'notice.act.leaveAuto',
}
const noticeLabel = (a: NoticeAction) => {
  const k = NOTICE_LABELS[a.hint.toLowerCase()]
  return k ? t(k) : a.hint.charAt(0).toUpperCase() + a.hint.slice(1)
}
const URL_PART = /(https?:\/\/[^\s<>()]+[^\s<>().,;:!?'"])/g
function renderLinked(text: string) {
  const parts = text.split(URL_PART)
  return parts.map((p, i) =>
    i % 2 === 1 ? (
      <AppLink key={i} href={p}>
        {p}
      </AppLink>
    ) : (
      <Fragment key={i}>{p}</Fragment>
    ),
  )
}
// (How long a key's "sent" stays said; a screen not read for longer than STALE, while it is looked at, is said to be maybe old.)
const SAID_FOR = 2000
const STALE = 3000

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
// (letters: a letter, as Enter or a number, only on the screen shown — Codex's approvals answer to "y", "p".)
// (onLeave: where F6 takes the focus from its screen — the dock's tabs; none: its own head's first button.)
// (who: the program the keys go to, as named to the person ("Claude Code"); onComposer: the message box, gone back to.)
// (main: the conversation's whole place, its terminal view — no head of its own, the person's terminal scheme and size. UI 14, 11-25.)
export type PanelLive = { onClose?: () => void; onFull?: () => void; note?: string; letters?: boolean; onLeave?: () => void; who?: string; onComposer?: () => void; canNotice?: boolean; onNoticeSent?: () => void; onBlocker?: (b: ScreenBlocker | null) => void; main?: boolean; bare?: boolean }
const PTY_TRIES = 2
// active: in sight (its screen read, said when old); keys: whether it may take the keys as it comes — not so the side
// of a split without them (review of 36e1069). Unsaid, as active.
export function PanelWindow({ sid, active, keys, press, onSettled, toast, live, onFull }: { live?: PanelLive; sid: string; active: boolean; keys?: boolean; press: () => Record<string, unknown>; onSettled: () => void; toast: (s: string, bad?: boolean) => void; onFull?: () => void }) {
  const [screen, setScreen] = useState({ text: '', digest: '' })
  const [loaded, setLoaded] = useState(false)
  const [pumping, setPumping] = useState(false)
  const [rawView, setRawView] = useState(false)
  const [noticeDismissed, setNoticeDismissed] = useState(false)
  const [lost, setLost] = useState(false)
  const [said, setSaid] = useState('')
  const [ptyFailed, setPtyFailed] = useState(false)
  const allowNotice = !live || !!live.canNotice
  // (Wherever a notice may be shown as a card, the screen is read once before the live terminal takes over: the
  // panel opened from a conversation, not only the reply's. Since the PTY stream, 96d9b7c, that one went straight
  // to the terminal, and Claude Code's "Enter to continue" notices were left as a raw screen — doc item 44, 11-24.)
  const needNoticeCheck = allowNotice
  const paneId = String(press().pane ?? '')
  const isMain = !!live?.main
  useEffect(() => {
    if (!isMain && active && paneId) claimPaneTerminal(sid, paneId)
  }, [isMain, active, sid, paneId])
  const [lookGaveUp, setLookGaveUp] = useState(false)
  // (The live terminal not opened: tried again, twice, before the screen is read instead — come back to its
  // tab, the connection it had may not be let go yet, 409. Each coming back, tried afresh. Plans/agent-tabs 4.4.)
  // (The tries counted apart from the terminal's key: counted afresh as it is shown again, the terminal it has
  // not made again for that — else each coming back opened one more connection. Review of 11-25.)
  const [ptyTry, setPtyTry] = useState(0)
  const tries = useRef(0)
  const retry = useRef<ReturnType<typeof setTimeout>>(undefined)
  useEffect(() => {
    setPtyFailed(false)
    setLookGaveUp(false)
    tries.current = 0
    return () => clearTimeout(retry.current)
  }, [sid, paneId])
  const ptyFellBack = () => {
    clearTimeout(retry.current)
    if (tries.current >= PTY_TRIES) return setPtyFailed(true)
    tries.current++
    retry.current = setTimeout(() => setPtyTry((n) => n + 1), 500 * tries.current)
  }
  const fullAction = live?.onFull ?? onFull
  // What became of the last key, said by the keys (a key sent said for a moment;
  // one not sent, or not taken, till the next) — set only when it changes, so
  // that it is said once to a screen reader.
  const [keySaid, setKeySaid] = useState<{ text: string; bad: boolean } | null>(null)
  const keySaidTimer = useRef<ReturnType<typeof setTimeout>>(undefined)
  // (clears: a key sent, said for a moment; one going stays till it has gone, however long that takes.)
  const sayKey = useCallback((text: string, bad: boolean, clears = false) => {
    clearTimeout(keySaidTimer.current)
    setKeySaid((was) => (was?.text === text && was.bad === bad ? was : { text, bad }))
    if (clears) keySaidTimer.current = setTimeout(() => setKeySaid(null), SAID_FOR)
  }, [])
  useEffect(() => () => clearTimeout(keySaidTimer.current), [])
  // (Its keys taken: the screen focused. Said above it, as what the keys do then.)
  const [taking, setTaking] = useState(false)
  const box = useRef<HTMLPreElement>(null)
  const screenId = useId()
  const at = useRef({ shown: '', stale: true, reading: false, pumping: false, queue: [] as Pressed[], fails: 0, escDone: 0, escGoing: false, alive: true, looks: 0, look: null as AbortController | null, readAt: 0 }) // (looks: which run of the looking is the current)
  const isLive = useRef(!!live)
  const kept = useWhenGone()
  const letters = useRef(!!live?.letters)
  letters.current = !!live?.letters
  const settlesHere = (k: string) => settles(k) || (letters.current && /^[A-Za-z0-9._/]$/.test(k))
  isLive.current = !!live
  const allowNoticeNow = useRef(allowNotice)
  allowNoticeNow.current = allowNotice
  const rawViewNow = useRef(rawView)
  rawViewNow.current = rawView
  const hasNoticeNow = useRef(false)
  const onNoticeSentNow = useRef(live?.onNoticeSent)
  onNoticeSentNow.current = live?.onNoticeSent
  const onBlockerNow = useRef(live?.onBlocker)
  onBlockerNow.current = live?.onBlocker
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
      const r = await api<{ text: string; digest: string; look?: string; blocker?: ScreenBlocker }>('POST', `/api/sessions/${sid}/conversation/panel-screen`, pressNow.current(), { timeoutMs: 12000, signal: look.signal })
      if (!a.alive) return
      a.stale = false
      a.fails = 0
      a.readAt = Date.now()
      setLoaded(true)
      setLost(false)
      setScreen({ text: r.text, digest: (isLive.current ? r.look : r.digest) ?? r.digest })
      if (r.blocker !== undefined) onBlockerNow.current?.(r.blocker)
    } catch (e) {
      // (Given up for a key, or another of its presses / SSH channel cap busy: no failure to read.)
      if (shouldIgnorePanelPollError(look.signal.aborted, e instanceof ApiError ? e.code : undefined)) return
      a.fails++
      if (a.alive && a.fails >= FAILS) {
        setLoaded(true)
        setLost(true)
      }
    } finally {
      a.reading = false
      if (a.look === look) a.look = null
    }
  }, [sid])

  const pump = useCallback(async () => {
    const a = at.current
    if (a.pumping) return
    a.pumping = true
    setPumping(true)
    try {
      // (Till none is left: one pressed during the read after the last is sent too.)
      while (a.alive && (a.queue.length || a.stale)) {
        if (!a.queue.length) {
          await read()
          if (a.stale && !a.queue.length) break // (not read: the next look tries again)
          continue
        }
        const { k, digest } = a.queue.shift()!
        const fromNotice = allowNoticeNow.current && !rawViewNow.current && hasNoticeNow.current
        if (k === 'esc') {
          if (Date.now() - a.escDone < ESC_AGAIN) continue // (the last may have shut it, however long that took to go)
          a.escGoing = true
        }
        if (a.alive) sayKey(t('panel.sending', { key: keyWord(k) }), false)
        try {
          // (What the key left comes back with it: another round trip for it would be
          // most of what a key takes on a far-off host.)
          const r = await api<{ text?: string; digest?: string; look?: string }>('POST', `/api/sessions/${sid}/conversation/panel`, { ...pressNow.current(), keys: [k], ...(settlesHere(k) ? { digest } : {}), ...(isLive.current ? { live: true } : {}) }, { timeoutMs: 15000 })
          a.stale = true
          if (a.alive) setSaid('')
          if (a.alive) sayKey(t('panel.sent', { key: keyWord(k) }), false, true)
          if (a.alive && typeof r?.text === 'string' && typeof r.digest === 'string') {
            const nextDigest = (isLive.current ? r.look : r.digest) ?? r.digest
            a.stale = false
            a.readAt = Date.now()
            a.shown = nextDigest
            setLoaded(true)
            setLost(false)
            setScreen({ text: r.text, digest: nextDigest })
            if (fromNotice) {
              const nextPlain = ansiLines(r.text).map((runs) => runs.map((x) => x.text).join(''))
              if (!readNoticeDialog(nextPlain)) {
                setNoticeDismissed(true)
                onNoticeSentNow.current?.()
                onSettledNow.current()
              }
            }
          }
          if (k === 'esc' || settlesHere(k)) setTimeout(() => a.alive && onSettledNow.current(), 700) // (not for a view gone meanwhile: review 7)
        } catch (e) {
          a.queue.length = 0 // (what was pressed after it was meant for what this would have shown)
          a.stale = true
          if (!a.alive) {
            /* (gone: nothing to say it on) */
          } else if (e instanceof ApiError && e.code === 'screen_changed') sayKey(t('panel.changed'), true)
          else if (e instanceof ApiError && e.code === 'no_dialog') {
            sayKey('', false)
            if (fromNotice) {
              setNoticeDismissed(true)
              onNoticeSentNow.current?.()
            }
            onSettledNow.current()
          } else {
            const why = e instanceof ApiError ? errorText(e) : t('panel.failed')
            sayKey(why, true)
            if (!isLive.current) toastNow.current(why, true) // (a panel of its own: as before, too)
          }
        } finally {
          if (k === 'esc') {
            a.escDone = Date.now()
            a.escGoing = false
          }
        }
      }
    } finally {
      a.pumping = false
      if (a.alive) setPumping(false)
    }
  }, [sid, read, sayKey])

  const send = useCallback(
    (k: string) => {
      const a = at.current
      if (k === 'esc' && (a.escGoing || a.queue.some((x) => x.k === 'esc'))) return
      if (settlesHere(k) && !a.shown) return // (nothing on show yet)
      if (a.queue.length >= QUEUE) return sayKey(t('panel.tooFast', { key: keyWord(k) }), true)
      a.queue.push({ k, digest: a.shown })
      a.look?.abort() // (a look under way would hold it up behind it)
      void pump()
    },
    [pump, sayKey],
  )

  const lines = useMemo(() => ansiLines(screen.text), [screen.text])
  const notice = useMemo(
    () => (allowNotice && loaded ? readNoticeDialog(lines.map((runs) => runs.map((r) => r.text).join(''))) : null),
    [allowNotice, loaded, lines],
  )
  const usePty = !!paneId && !ptyFailed && (!needNoticeCheck || rawView || (loaded && !notice) || (lookGaveUp && !loaded))
  // (Counted only while it is in front: one behind is not read — its look waits — and would go to the terminal unread.)
  const waitingLook = active && !!paneId && !ptyFailed && needNoticeCheck && !loaded && !lookGaveUp
  useEffect(() => {
    if (!waitingLook) return
    const timer = setTimeout(() => setLookGaveUp(true), NOTICE_LOOK_MS)
    return () => clearTimeout(timer)
  }, [waitingLook])

  // Read over and over while it is open and seen; skipped whenever the live
  // WebSocket PTY (PaneTerminal) is handling the terminal directly.
  useEffect(() => {
    if (usePty) return
    const a = at.current
    a.alive = true
    kept.back()
    let timer: ReturnType<typeof setTimeout>
    const run = a.looks
    const tick = async () => {
      // (Not while a key is going or waiting: the host takes one at a time for a
      // pane, and a look under way would hold the key up behind it.)
      if (!document.hidden && activeNow.current && !a.pumping && !a.queue.length) await read()
      if (a.alive && a.looks === run) timer = setTimeout(tick, isLive.current ? EVERY_LIVE : EVERY)
    }
    void tick()
    return () => {
      a.look?.abort()
      clearTimeout(timer)
      // (Sent to a tab behind: keys pressed still go, one by one, and its looks stop — the timer is this run's; the
      // next run, shown again, looks anew. Closed, or let go from behind: all stops.)
      a.looks++
      kept.gone(() => void (a.alive = false))
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [read, usePty])
  useEffect(() => {
    // (A panel opened on purpose takes the keys. The terminal shown by itself
    // does not: what is being typed in the box goes on going there, a key in
    // the terminal only once the person has gone to it.)
    if ((keys ?? active) && !isCoarse() && !isLive.current && !usePty) box.current?.focus({ preventScroll: true })
  }, [active, keys, loaded, rawView, usePty])

  // Whether what is shown may be old: not read for STALE while it is looked at
  // and no key is going or waiting (the looks wait for those) — counted from
  // when it came back into sight, not from a read before: a tab in the
  // background, or the page another tab's, is not old the moment it is back.
  const [now, setNow] = useState(() => Date.now())
  const seenSince = useRef(Date.now())
  const wasActive = useRef(active)
  if (active && !wasActive.current) seenSince.current = Date.now() // (back in sight: from now, this very render)
  wasActive.current = active
  useEffect(() => {
    if (!active || usePty) return
    const back = () => {
      if (!document.hidden) {
        seenSince.current = Date.now()
        setNow(Date.now())
      }
    }
    document.addEventListener('visibilitychange', back)
    const i = setInterval(() => setNow(Date.now()), 1000)
    return () => {
      document.removeEventListener('visibilitychange', back)
      clearInterval(i)
    }
  }, [active, usePty])
  const busy = pumping || at.current.pumping || at.current.queue.length > 0
  if (busy) seenSince.current = Math.max(seenSince.current, Date.now()) // (a key going: counted again from when it has gone)
  const readAt = at.current.readAt
  const oldFor = readAt ? now - Math.max(readAt, seenSince.current) : 0
  const old = !!live && active && !lost && !busy && !!readAt && !document.hidden && oldFor > STALE ? Math.round((now - readAt) / 1000) : 0

  hasNoticeNow.current = !!notice
  const noticeTitle = notice?.title ?? ''
  useEffect(() => {
    if (noticeTitle) {
      setRawView(false)
      setNoticeDismissed(false)
    }
  }, [noticeTitle])
  // (A screen wider than its room: a fade on the side more of it is on.)
  const view = useRef<HTMLDivElement>(null)
  const [more, setMore] = useState({ left: false, right: false })
  const measure = useCallback(() => {
    const el = box.current
    if (!el) return
    const left = el.scrollLeft > 1
    const right = el.scrollWidth - el.clientWidth - el.scrollLeft > 1
    setMore((m) => (m.left === left && m.right === right ? m : { left, right }))
  }, [])
  useLayoutEffect(measure, [lines, measure, loaded, rawView, noticeTitle])
  useEffect(() => {
    const el = box.current
    if (!el || typeof ResizeObserver === 'undefined') return
    const ro = new ResizeObserver(measure)
    ro.observe(el)
    return () => ro.disconnect()
  }, [measure, loaded, rawView, noticeTitle])
  // (What a key goes with: what has been drawn, not what was read.)
  useLayoutEffect(() => {
    at.current.shown = screen.digest
  }, [screen.digest])
  // (Its foot in view, where a panel is: unless read further up.)
  const pinned = useRef(true)
  useLayoutEffect(() => {
    const el = box.current
    if (el && pinned.current) el.scrollTop = el.scrollHeight
  }, [lines, loaded, rawView, noticeTitle])

  if (usePty) {
    return (
      <PaneTerminal
        key={ptyTry}
        sid={sid}
        pane={paneId}
        active={keys ?? active}
        live={live}
        fullAction={fullAction}
        backToCard={allowNotice && notice && rawView ? () => setRawView(false) : undefined}
        onSettled={onSettled}
        onFallback={ptyFellBack}
      />
    )
  }

  if (allowNotice && noticeDismissed && !notice) {
    return (
      <div className="convo-notice" role="status">
        <p className="convo-notice-clip">{t('notice.sent')}</p>
      </div>
    )
  }
  if (allowNotice && !loaded && !lost) {
    return (
      <div className="convo-notice" role="status">
        <p className="convo-notice-clip">{t('notice.loading')}</p>
      </div>
    )
  }
  if (allowNotice && notice && !rawView) {
    const primaryIdx = notice.actions.findIndex((a) => a.key !== 'esc' && !a.disabled)
    return (
      <div className="convo-notice" role="group" aria-label={notice.title}>
        <div className="convo-notice-head">
          <span className="convo-notice-tag">{t('notice.tag')}</span>
          <div className="convo-notice-links">
            <button type="button" className="link-btn convo-notice-link" onClick={() => setRawView(true)}>
              {t('notice.showTerminal')}
            </button>
            {fullAction && (
              <button type="button" className="link-btn convo-notice-link" onClick={fullAction}>
                {t('panel.full')}
              </button>
            )}
          </div>
        </div>
        <h3 className="convo-notice-title">{notice.title}</h3>
        {notice.paragraphs.length > 0 && (
          <div className="convo-notice-body">
            {notice.paragraphs.map((p, i) => (
              <p key={i}>{renderLinked(p)}</p>
            ))}
          </div>
        )}
        {notice.clipped && <p className="convo-notice-clip">{t('notice.clipped')}</p>}
        <div className="convo-notice-acts">
          {notice.actions.map((a, i) => (
            <button
              key={a.key}
              type="button"
              className={(i === primaryIdx ? 'btn btn-primary' : 'btn btn-ghost') + ' convo-notice-act'}
              disabled={busy || !!a.disabled || !screen.digest}
              title={a.disabled ? t('notice.tallerWindow') : undefined}
              onClick={() => send(a.key)}
            >
              <span>{noticeLabel(a)}</span>
              <span className="convo-notice-key" aria-hidden="true">
                {keyWord(a.key)}
              </span>
            </button>
          ))}
        </div>
        {notice.actions.some((a) => a.disabled) && <p className="convo-notice-clip">{t('notice.tallerWindow')}</p>}
        {(lost || keySaid?.text) && (
          <p className={'convo-notice-stat' + (keySaid?.bad ? ' is-bad' : '')} role="status">
            {lost ? t('panel.lost') : keySaid?.text}
          </p>
        )}
      </div>
    )
  }

  const who = live?.who || t('panel.whoTerminal')
  // (The keys as buttons: on a touch screen, and by a live screen everywhere — an
  // approval answered without the screen focused, nor its keys learnt.)
  const keyBar = isCoarse() || !!live
  const keyStatus = (
    <span className={'convo-panelwin-stat' + (keySaid?.bad ? ' is-bad' : '')} role="status">
      {keySaid?.text ?? ''}
    </span>
  )
  return (
    <div className="convo-panelwin" role="group" aria-label={t(live ? 'panel.liveTitle' : 'panel.title')}>
      {/* (Bare — the dock's tab: no line over it, its whole-terminal button in the tab row; one only to go back to a card.) */}
      {(!live?.bare || (allowNotice && notice && rawView)) && (
      <div className={'convo-panelwin-head' + (live ? ' is-live' : '')}>
        {/* (Live: one short line — what a click does, and where words go; the rest said once its keys are taken.) */}
        {!live && <span className="convo-panelwin-title">{t('panel.title')}</span>}
        {!isCoarse() && !live?.bare && <span className="convo-panelwin-hint">{t(live ? 'panel.liveHint' : 'panel.hint')}</span>}
        <div className="convo-panelwin-head-acts">
          {allowNotice && notice && rawView && (
            <button type="button" className="link-btn convo-panelwin-close" onClick={() => setRawView(false)}>
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
            <button type="button" className="link-btn convo-panelwin-close" onClick={() => send('esc')}>
              {t('agents.closeDialog')}
            </button>
          )}
        </div>
      </div>
      )}
      {live?.note && <p className="convo-panelwin-note">{live.note}</p>}
      {/* (Its keys taken: said plainly, with the ways out — F6, or back to the message box.) */}
      {live && taking && !isCoarse() && (
        <div className="convo-panelwin-taking" id={screenId + '-taking'}>
          <span>{t('panel.taking', { who })}</span>
          {live.onComposer && (
            <button
              type="button"
              className="link-btn"
              // (Pressed with the screen focused: not a blur first that would take the bar away under the pointer.)
              onMouseDown={(e) => e.preventDefault()}
              onClick={live.onComposer}
            >
              {t('panel.toComposer')}
            </button>
          )}
        </div>
      )}
      <div ref={view} className={'convo-panelwin-view' + (more.left ? ' more-left' : '') + (more.right ? ' more-right' : '')}>
        {/* (Takes the keys: what is pressed here goes to the panel.) */}
        <pre
          ref={box}
          id={screenId}
          className="convo-panelwin-screen"
          tabIndex={0}
          aria-label={t(live ? 'panel.liveScreen' : 'panel.screen')}
          aria-describedby={live && taking ? screenId + '-taking' : undefined}
          onFocus={() => setTaking(true)}
          onBlur={() => setTaking(false)}
          onScroll={(e) => {
            const el = e.currentTarget
            pinned.current = el.scrollHeight - el.scrollTop - el.clientHeight < 24
            measure()
          }}
          onPaste={(e) => {
            // (Words are not keys one by one: said, not sent in part.)
            e.preventDefault()
            sayKey(t('panel.noPaste'), true)
          }}
          onKeyDown={(e) => {
            // (Its screen takes Tab and Esc for the terminal: F6, as between an app's
            // panes, is the way out — never a key of the host's — review, 11-15.)
            if (live && e.key === 'F6' && !e.altKey && !e.ctrlKey && !e.metaKey && !e.shiftKey) {
              e.preventDefault()
              e.stopPropagation()
              if (live.onLeave) live.onLeave()
              else e.currentTarget.parentElement?.parentElement?.querySelector<HTMLElement>('.convo-panelwin-head button')?.focus()
              return
            }
            const k = keyOf(e)
            if (!k) {
              // (A character it cannot pass on — another script's, an input method's: said, not dropped unseen.)
              if (!e.altKey && !e.ctrlKey && !e.metaKey && (e.key.length === 1 || e.nativeEvent.isComposing || e.keyCode === 229)) {
                e.preventDefault()
                sayKey(t('panel.noText'), true)
              }
              return
            }
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
      </div>
      {(said || lost || old > 0) && (
        // (What is said once: the seconds, which count on, beside it and not read out each time.)
        <p className="convo-panelwin-note">
          <span role="status">{lost ? t('panel.lost') : said || t('panel.old')}</span>
          {!lost && !said && old > 0 && <span aria-hidden="true">{' ' + t('panel.oldAgo', { s: old })}</span>}
        </p>
      )}
      {keyBar ? (
        <div className={'convo-panelwin-keys' + (isCoarse() ? '' : ' is-compact')}>
          {TOUCH_KEYS.map(([k, label]) => (
            <button key={k} type="button" className="convo-panelwin-key" onClick={() => send(k)} aria-label={t('panel.sendKey', { key: label, who })} title={t('panel.sendKey', { key: label, who })}>
              {label}
            </button>
          ))}
          {keyStatus}
        </div>
      ) : (
        <p className="convo-panelwin-note convo-panelwin-statline">{keyStatus}</p>
      )}
    </div>
  )
}
