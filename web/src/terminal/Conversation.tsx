import { memo, useCallback, useEffect, useLayoutEffect, useMemo, useRef, useState, type ReactNode } from 'react'
import { ArrowDown, ChevronDown, ChevronRight, GitBranch, PanelLeft, X } from 'lucide-react'
import { t, getLang, type I18nKey } from '../i18n'
import { copyText, isCoarse } from '../components/ui'
import { local } from '../storage'
import { Markdown } from './markdown'
import { Code } from './Code'
import type { Convo, ConvoEntry, ConvoHunk, ConvoMeta, ConvoPart, ConvoQueued, ConvoTask, ConvoTool } from './conversation'
import { category, isNote, items, ms, partTime, sameEdits, verbOf, type Category, type Edits, type Item } from './convoItems'

// The conversation view: the agent's log as turns, over the terminal (which
// stays mounted under it). Each turn is what the person wrote, then one line
// for all the agent did meanwhile ("worked for 2m 14s", opened on demand:
// a grey line a step), then its answer with what it changed. While it
// works, the turn's steps show as they come, the one under way last.
// Messages are still typed in the composer or the terminal: the view only
// reads. The composer (Terminal.tsx's) sits under the thread, in its column,
// with what the session runs with under it.








function count(cat: Category, n: number): string {
  return t(`convo.n.${cat}${n === 1 ? '1' : ''}` as I18nKey, { n })
}

// What the bar over the conversation says of the agent: where it is, what
// it is doing (unknown when that cannot be trusted now).
export interface ConvoAgent {
  where?: string // herdr: the workspace
  title?: string // the session's title (Claude Code's own), else the pane's
  status: 'working' | 'blocked' | 'done' | 'idle' | 'unknown'
  other?: string // the program in the pane, when it is not Claude Code
}

// A message sent from the composer while the conversation shows: "waiting"
// until the log has it (then it is dropped), "unseen" when that took too
// long, "sent" when it can no longer be told (another log came in front).
export interface ConvoPending {
  id: number
  text: string
  state: 'waiting' | 'unseen' | 'sent'
}

const HINT_AFTER = 20000 // working with nothing new this long: say why
const SIDE_KEY = 'corral.convoSide'
const SIDE_MIN = 900 // (px) the view's width from which the list goes beside the thread
const isTouchOnly = isCoarse()

// A duration as the view says it: 45 s, 2 min 14 s, 1 h 5 min.
export function duration(ms: number): string {
  const s = Math.max(0, Math.round(ms / 1000))
  if (s < 60) return t('convo.dur.s', { s })
  if (s < 3600) return t('convo.dur.ms', { m: Math.floor(s / 60), s: s % 60 })
  return t('convo.dur.hm', { h: Math.floor(s / 3600), m: Math.floor((s % 3600) / 60) })
}


// Now, every second while on: for what counts up.
function useTick(on: boolean): number {
  const [now, setNow] = useState(Date.now())
  useEffect(() => {
    if (!on) return
    setNow(Date.now())
    const timer = setInterval(() => setNow(Date.now()), 1000)
    return () => clearInterval(timer)
  }, [on])
  return now
}

export function ConversationView({
  convo,
  agent,
  pending,
  onReply,
  ask,
  onStop,
  stop,
  side,
  active = true,
  onDismiss,
  onShowTerminal,
  onCopied,
  composer,
  onSendNow,
  sendNow,
  lead,
}: {
  convo: Convo
  agent: ConvoAgent | null
  pending: ConvoPending[]
  onReply?: () => void // (the agent waits for an answer: a bar to open the reply)
  ask?: ReactNode // (it waits, and is answered here: over the composer)
  onStop?: () => void // (it works: press Esc in its pane)
  stop?: string // what became of the last stop pressed, to say
  side?: { rows: ConvoListRow[] } // the agents to go to: beside the thread where there is room, else from the bar
  active?: boolean // (its tab is the one shown: keys are for it)
  onDismiss: (id: number) => void
  onShowTerminal: () => void
  onCopied: () => void
  composer?: ReactNode // (the terminal's composer, while it is open)
  onSendNow?: () => void // (messages wait to be taken in: Claude Code's "send now")
  sendNow?: string // what became of the last "send now", to say
  lead?: ReactNode // (first in the bar: the agents page's way back to its list, on a phone)
}) {
  const working = agent?.status === 'working'
  const waits = agent?.status === 'blocked'
  // The list beside the thread: where the view is wide enough, unless folded away (kept on this device).
  const root = useRef<HTMLDivElement>(null)
  const [wide, setWide] = useState(false)
  const [folded, setFoldedState] = useState(() => local.get(SIDE_KEY) === '0')
  const setFolded = (v: boolean) => {
    setFoldedState(v)
    local.set(SIDE_KEY, v ? '0' : '1')
  }
  useLayoutEffect(() => {
    const el = root.current
    if (!el) return
    const look = () => setWide(el.clientWidth >= SIDE_MIN)
    look()
    const ro = new ResizeObserver(look)
    ro.observe(el)
    return () => ro.disconnect()
  }, [])
  const sideShown = !!side && wide && !folded
  const [picking, setPicking] = useState(false)
  const unpick = useCallback(() => setPicking(false), [])
  useEffect(() => {
    if (sideShown || !active) setPicking(false) // (a tab sent to the back keeps no dialog open)
  }, [sideShown, active])
  // Alt+Up and Alt+Down: the agent before and after on the list (not while
  // a field with text in it has the keys).
  const sideRef = useRef(side)
  sideRef.current = side
  useEffect(() => {
    if (!active) return
    const onKey = (e: KeyboardEvent) => {
      if (!e.altKey || e.ctrlKey || e.metaKey || e.shiftKey || (e.key !== 'ArrowUp' && e.key !== 'ArrowDown')) return
      const el = e.target as HTMLInputElement | null
      if (el && (el.tagName === 'INPUT' || el.tagName === 'TEXTAREA') && !el.classList.contains('xterm-helper-textarea') && el.value) return
      const list = listOrder(sideRef.current?.rows ?? [])
      if (list.length < 2) return
      e.preventDefault()
      e.stopPropagation()
      const at = list.findIndex((r) => r.current)
      const next = list[(at + (e.key === 'ArrowDown' ? 1 : list.length - 1) + (at < 0 ? 1 : 0)) % list.length]
      if (!next.current) next.onPick()
    }
    window.addEventListener('keydown', onKey, true)
    return () => window.removeEventListener('keydown', onKey, true)
  }, [active])
  const list = useMemo(() => items(convo.entries, working || waits, working), [convo.entries, working, waits])
  const box = useRef<HTMLDivElement>(null)
  const col = useRef<HTMLDivElement>(null)
  const stuck = useRef(true) // following the end
  const [away, setAway] = useState(false)
  const copiedRef = useRef(onCopied)
  copiedRef.current = onCopied
  const copy = useCallback((s: string) => void copyText(s).then((ok) => ok && copiedRef.current()), [])
  // A turn's work opened or closed by hand (else: open while it goes on).
  // Folded, also while it goes on (its line says what it does now): one opened
  // by a tap, or all with Ctrl+O, as in Claude Code.
  const [opened, setOpened] = useState<Record<string, boolean>>({})
  const toggle = useCallback((key: string, open: boolean) => setOpened((o) => ({ ...o, [key]: open })), [])
  const [allOpen, setAllOpen] = useState(false)
  useEffect(() => {
    if (!active) return
    const onKey = (e: KeyboardEvent) => {
      if (!e.ctrlKey || e.metaKey || e.altKey || e.shiftKey || e.key.toLowerCase() !== 'o' || e.isComposing) return
      e.preventDefault()
      e.stopPropagation()
      if (e.repeat) return // (held: once)
      setOpened({})
      setAllOpen((v) => !v)
    }
    window.addEventListener('keydown', onKey, true)
    return () => window.removeEventListener('keydown', onKey, true)
  }, [active])
  // (Of turns no longer kept, nothing is.)
  useEffect(() => {
    const keys = new Set(list.flatMap((it) => (it.kind === 'work' ? [it.key] : [])))
    setOpened((o) => (Object.keys(o).every((k) => keys.has(k)) ? o : Object.fromEntries(Object.entries(o).filter(([k]) => keys.has(k)))))
  }, [list])
  // The changes of a turn (by its answer's key), beside the thread.
  const [changes, setChanges] = useState<{ key: string; path?: string } | null>(null)
  const show = useCallback((key: string, path?: string) => setChanges({ key, path }), [])
  const unshow = useCallback(() => setChanges(null), [])
  const shownEdits = changes ? (list.find((it) => it.kind === 'answer' && it.key === changes.key) as Extract<Item, { kind: 'answer' }> | undefined)?.edits : null
  useEffect(() => {
    if (changes && !shownEdits) setChanges(null) // (the turn is no longer kept, or another conversation shows)
  }, [changes, shownEdits])

  // While it works, look again now and then whether nothing came for long:
  // counted from the last change, the start of this work, or coming back to
  // the page, whichever is latest.
  const [since, setSince] = useState(Date.now())
  useEffect(() => {
    if (!working) return
    setSince(Date.now())
    const onVisible = () => {
      if (!document.hidden) setSince(Date.now())
    }
    document.addEventListener('visibilitychange', onVisible)
    return () => document.removeEventListener('visibilitychange', onVisible)
  }, [working])
  const now = useTick(working)
  const quiet = working && now - Math.max(convo.changedAt, since) > HINT_AFTER
  // How long this turn has gone on: from the last thing the person wrote.
  const lastUser = useMemo(() => {
    for (let i = convo.entries.length - 1; i >= 0; i--) if (convo.entries[i].role === 'user' && !convo.entries[i].mid) return ms(convo.entries[i].ts)
    return 0
  }, [convo.entries])

  // Another conversation in front (another pane, another tab's): from its end.
  useLayoutEffect(() => {
    stuck.current = true
    setAway(false)
    const el = box.current
    if (el) el.scrollTop = el.scrollHeight
  }, [convo.of])
  // Reads in that bring nothing (parts all of one long line): after three
  // in a row it goes on only when asked (the note becomes a button).
  const [olderPaused, setOlderPaused] = useState(false)
  const olderPausedNow = useRef(false)
  olderPausedNow.current = olderPaused
  // (Brought something: the first entry is an earlier one. What came at the end meanwhile does not count.)
  const firstSeq = convo.entries.length ? convo.entries[0].seq : 0
  const emptyReads = useRef({ n: 0, first: 0, was: false })
  useEffect(() => {
    const r = emptyReads.current
    if (convo.loadingOlder && !r.was) r.first = firstSeq
    if (!convo.loadingOlder && r.was) {
      r.n = firstSeq < r.first ? 0 : r.n + 1
      setOlderPaused(r.n >= 3)
    }
    r.was = convo.loadingOlder
  }, [convo.loadingOlder, firstSeq])
  useEffect(() => {
    emptyReads.current = { n: 0, first: 0, was: false }
    setOlderPaused(false)
  }, [convo.of, convo.gen])
  // The part before, read in when its line comes near (as the top is
  // scrolled to): what was in view stays where it was as it goes in above.
  const olderRef = useRef<HTMLParagraphElement>(null)
  const convoNow = useRef(convo)
  convoNow.current = convo
  useEffect(() => {
    const el = olderRef.current
    const root = box.current
    if (!el || !root || !convo.older || convo.olderCut) return
    const io = new IntersectionObserver(
      (es) => {
        const c = convoNow.current
        if (es.some((e) => e.isIntersecting) && c.older && !c.olderCut && !c.loadingOlder && !olderPausedNow.current) c.loadOlder()
      },
      { root, rootMargin: '600px 0px 0px 0px' },
    )
    io.observe(el)
    return () => io.disconnect()
  }, [convo.older, convo.olderCut, convo.loadingOlder, convo.of, olderPaused])
  // What is in view stays where it is as items come in or change above it
  // (the part before read in, a turn opened higher up), unless the end is
  // kept in view: a few items in view are noted with where they are, and
  // after each change the first still there is put back. (By hand: Safari
  // has no scroll anchoring. The column's children, past its first note,
  // are the list's items in order.)
  // (Each also by its place from the end: kept when only what was before
  // changed, as a run read from its middle takes in its start, and its key.)
  const anchors = useRef<{ key: string; top: number; fromEnd: number; last: string }[]>([])
  const listNow = useRef(list)
  listNow.current = list
  const note = useCallback(() => {
    const el = box.current
    const c = col.current
    if (!el || !c) return
    const view = el.getBoundingClientRect()
    const kids = [...c.children].filter((k) => !k.classList.contains('convo-older'))
    const out: { key: string; top: number; fromEnd: number; last: string }[] = []
    const last = listNow.current.length ? itemKey(listNow.current[listNow.current.length - 1]) : ''
    // (The first in view by halving: a long conversation has thousands.)
    let lo = 0
    let hi = kids.length
    while (lo < hi) {
      const mid = (lo + hi) >> 1
      if (kids[mid].getBoundingClientRect().bottom <= view.top) lo = mid + 1
      else hi = mid
    }
    for (let i = lo; i < kids.length && out.length < 5; i++) {
      const it = listNow.current[i]
      const r = kids[i].getBoundingClientRect()
      if (!it || r.top >= view.bottom) break
      out.push({ key: itemKey(it), top: r.top - view.top, fromEnd: listNow.current.length - i, last })
    }
    anchors.current = out
  }, [])
  useLayoutEffect(() => {
    const el = box.current
    const c = col.current
    if (el && c && !stuck.current && anchors.current.length) {
      const view = el.getBoundingClientRect()
      const kids = [...c.children].filter((k) => !k.classList.contains('convo-older'))
      const at = new Map(list.map((it, i) => [itemKey(it), i]))
      const lastNow = list.length ? itemKey(list[list.length - 1]) : ''
      const hit =
        anchors.current.find((a) => at.has(a.key)) ??
        // (None there by its key, the end as it was: by its place from the end.)
        anchors.current.find((a) => a.last === lastNow && list.length - a.fromEnd >= 0)
      const i = hit ? (at.get(hit.key) ?? list.length - hit.fromEnd) : -1
      const k = hit ? kids[i] : undefined
      if (hit && k) {
        const d = k.getBoundingClientRect().top - view.top - hit.top
        if (Math.abs(d) >= 1) el.scrollTop += d
      }
    }
    note()
  })
  // Before the new content is laid out, note whether the end was in view;
  // after, keep it there.
  useLayoutEffect(() => {
    const el = box.current
    if (el && stuck.current) el.scrollTop = el.scrollHeight
  }, [list, pending, working, quiet, convo.tasks, !!ask])
  useEffect(() => {
    const el = box.current
    if (!el) return
    const onScroll = () => {
      const atEnd = el.scrollHeight - el.scrollTop - el.clientHeight < 48
      stuck.current = atEnd
      setAway(!atEnd)
      note()
    }
    el.addEventListener('scroll', onScroll, { passive: true })
    // (What grows after it is laid out, as the answer card once the screen is read, keeps the end in view too.)
    const grows = new ResizeObserver(() => {
      if (stuck.current) el.scrollTop = el.scrollHeight
    })
    if (col.current) grows.observe(col.current)
    grows.observe(el)
    return () => {
      el.removeEventListener('scroll', onScroll)
      grows.disconnect()
    }
  }, [])

  let body: ReactNode
  if (convo.status === 'loading') body = <p className="convo-state">{t('convo.loading')}</p>
  else if (convo.status === 'error') body = <p className="convo-state">{t('convo.error')}</p>
  else if (convo.status === 'none')
    body = (
      <div className="convo-state">
        <p>
          {convo.reason === 'no_log'
            ? t('convo.noLog')
            : convo.reason === 'unsupported'
              ? t('convo.unsupported')
              : agent?.other
                ? t('convo.otherAgent', { agent: agent.other })
                : t('convo.noAgent')}
        </p>
        <button type="button" className="btn btn-ghost" onClick={onShowTerminal}>{t('convo.showTerminal')}</button>
      </div>
    )
  else
    body = (
      <>
        {convo.older ? (
          // (Near the top, the part before is read in: see the observer.)
          <p className="convo-older" ref={olderRef} role="status">
            {convo.olderCut ? (
              t('convo.olderCut')
            ) : convo.loadingOlder ? (
              t('convo.loadingOlder')
            ) : olderPaused ? (
              <button
                type="button"
                className="link-btn"
                onClick={() => {
                  emptyReads.current.n = 0
                  setOlderPaused(false)
                  convo.loadOlder()
                }}
              >
                {t('convo.olderMore')}
              </button>
            ) : (
              t('convo.older')
            )}
          </p>
        ) : (
          convo.entries.length > 0 && <p className="convo-older">{t('convo.atStart')}</p>
        )}
        {list.map((it) => (
          <ListItem
            key={itemKey(it)}
            it={it}
            copy={copy}
            open={it.kind === 'work' ? (opened[it.key] ?? allOpen) : false}
            onToggle={toggle}
            onShow={show}
          />
        ))}
        {pending.map((p) => (
          <div key={p.id} className="convo-user convo-pending">
            <div className="convo-user-text">{p.text}</div>
            <div className="convo-meta">
              <span>{t(p.state === 'waiting' ? 'convo.sent' : p.state === 'unseen' ? 'convo.unseen' : 'convo.sentOnly')}</span>
              {p.state !== 'waiting' && (
                <button type="button" className="link-btn" onClick={() => onDismiss(p.id)}>{t('common.close')}</button>
              )}
            </div>
          </div>
        ))}
        {working && quiet && <p className="convo-working">{t('convo.quiet')}</p>}
      </>
    )

  let status = agent ? t(`convo.status.${agent.status}` as I18nKey) : ''
  if (working && lastUser && now >= lastUser) status = t('convo.workingFor', { d: duration(now - lastUser) })
  const name = agent ? [agent.title, agent.where].filter(Boolean) : []
  const rows = side?.rows ?? []
  const others = rows.filter((r) => r.status === 'blocked' && !r.current)
  const label = (
    <>
      <span className="convo-bar-name">{name[0] || t('convo.title')}</span>
      {name[1] && <span className="convo-bar-where">{name[1]}</span>}
      {side && !sideShown && <ChevronDown size={14} className="convo-bar-chev" aria-hidden="true" />}
    </>
  )
  return (
    <div className="convo" role="region" aria-label={t('convo.title')} ref={root}>
      {sideShown && (
        <nav className="convo-side" aria-label={t('convo.agents')}>
          <AgentList rows={rows} />
          {!isTouchOnly && <p className="convo-side-keys">{t('convo.switchKeys')}<br />{t(allOpen ? 'convo.foldKeys' : 'convo.openKeys')}</p>}
        </nav>
      )}
      <div className="convo-main">
      {(name.length > 0 || status || convo.failing || side || lead) && (
        <div className="convo-bar">
          {lead}
          {side && wide && (
            <button type="button" className="icon-btn convo-side-btn" aria-pressed={sideShown} onClick={() => setFolded(!folded)} aria-label={t('convo.agents')} title={t('convo.agents')}>
              <PanelLeft size={17} aria-hidden="true" />
            </button>
          )}
          {side && !sideShown ? (
            <button type="button" className="convo-bar-pick" aria-haspopup="dialog" aria-expanded={picking} aria-label={t('convo.pick')} onClick={() => setPicking(true)}>
              {label}
            </button>
          ) : (
            <span className="convo-bar-pick is-static">{label}</span>
          )}
          <span className={'convo-bar-status' + (agent && !convo.failing && !stop ? ' is-' + agent.status : '')}>
            {working && !convo.failing && !stop && <span className="convo-spin" aria-hidden="true" />}
            {convo.failing ? t('convo.paused', { time: clockMs(convo.okAt) }) : stop || status}
          </span>
          {onStop && (
            <button type="button" className="btn btn-ghost convo-stop" onClick={onStop}>
              {t('convo.stop')}
            </button>
          )}
        </div>
      )}
      {!sideShown && others.length > 0 && (
        // (Another agent waits: one tap to it, where the list is not in view.)
        <button type="button" className="convo-nudge" onClick={others[0].onPick}>
          <span className="convo-nudge-text">{t('convo.othersWaiting', { n: others.length, title: others[0].title })}</span>
          <ChevronRight size={15} aria-hidden="true" />
        </button>
      )}
      <div className="convo-scroll-wrap">
        <div className="convo-scroll" ref={box}>
          <div className="convo-col" ref={col}>
            {body}
          </div>
        </div>
        {away && convo.status === 'ready' && (
          <button
            type="button"
            className="convo-latest"
            onClick={() => {
              const el = box.current
              if (el) el.scrollTo({ top: el.scrollHeight, behavior: matchMedia('(prefers-reduced-motion: reduce)').matches ? 'auto' : 'smooth' })
            }}
          >
            <ArrowDown size={15} aria-hidden="true" />
            {t('convo.latest')}
          </button>
        )}
      </div>
      <div className="convo-dock">
        {convo.status === 'ready' && convo.tasks && <Tasks tasks={convo.tasks} fold={!!ask} />}
        {/* (Asked from the screen: also where its log is not read.) */}
        {(convo.status === 'ready' || convo.status === 'none') && ask && <div className="convo-dock-ask">{ask}</div>}
        {convo.status === 'ready' && (working || waits) && convo.queued && <Queue items={convo.queued} onSendNow={working ? onSendNow : undefined} said={sendNow} />}
        {waits && !ask && convo.status === 'ready' && (
          // (Where it cannot be answered here: in its terminal.)
          <div className="convo-waiting" role="status">
            <span>{t('convo.waiting')}</span>
            <button type="button" className="btn btn-primary" onClick={onReply ?? onShowTerminal}>{t(onReply ? 'convo.answer' : 'convo.answerThere')}</button>
          </div>
        )}
        {composer}
        {convo.status === 'ready' && convo.meta && <Runs meta={convo.meta} />}
      </div>
      </div>
      {changes && shownEdits && <ChangesPanel key={changes.key} edits={shownEdits} focus={changes.path} onClose={unshow} />}
      {picking && side && <Picker rows={rows} onClose={unpick} />}
    </div>
  )
}

// What the person sent while the agent works, not taken in yet: over the
// composer, with Claude Code's "send now" (which stops the step under way).
function Queue({ items, onSendNow, said }: { items: ConvoQueued[]; onSendNow?: () => void; said?: string }) {
  return (
    <div className="convo-queue" role="status">
      <div className="convo-queue-head">
        <span className="convo-queue-sum">{t('convo.queued', { n: items.length })}</span>
        {said ? (
          <span className="convo-queue-said">{said}</span>
        ) : (
          onSendNow && (
            <button type="button" className="btn btn-ghost convo-queue-now" onClick={onSendNow} title={t('convo.sendNowHint')}>
              {t('convo.sendNow')}
            </button>
          )
        )}
      </div>
      {items.map((q, i) => (
        <div key={i} className="convo-queue-row">
          {q.text}
        </div>
      ))}
    </div>
  )
}

// A model's id as people call it: claude-opus-5-5 is Opus 5.5.
export function modelName(id: string): string {
  const m = /^claude-(opus|sonnet|haiku|fable)-(\d+)(?:-(\d{1,2}))?(?:-\d{8})?(?:\[.*\])?$/.exec(id)
  if (!m) return id
  return m[1][0].toUpperCase() + m[1].slice(1) + ' ' + m[2] + (m[3] ? '.' + m[3] : '')
}

// The windows a conversation may fill: the log does not say which a session
// has, so how full it is is said of both.
const WINDOWS = [200_000, 1_000_000]

function tokens(n: number): string {
  if (n >= 1_000_000) return (Math.round(n / 100_000) / 10).toString() + 'M'
  if (n >= 1000) return (Math.round(n / 100) / 10).toString() + 'K'
  return String(n)
}

const MODES = ['default', 'acceptEdits', 'plan', 'auto', 'bypassPermissions', 'dontAsk'] as const
const modeName = (m: string) => ((MODES as readonly string[]).includes(m) ? t(`convo.mode.${m}` as I18nKey) : m)

// What the session runs with, under the composer: its permission mode on
// the left; its branch, model and how full its context is on the right
// (the numbers on a tap).
function Runs({ meta }: { meta: ConvoMeta }) {
  const [open, setOpen] = useState(false)
  const box = useRef<HTMLDivElement>(null)
  useEffect(() => {
    if (!open) return
    const away = (e: PointerEvent) => {
      if (!box.current?.contains(e.target as Node)) setOpen(false)
    }
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') {
        e.preventDefault()
        e.stopPropagation()
        setOpen(false)
      }
    }
    document.addEventListener('pointerdown', away, true)
    document.addEventListener('keydown', onKey, true)
    return () => {
      document.removeEventListener('pointerdown', away, true)
      document.removeEventListener('keydown', onKey, true)
    }
  }, [open])
  const used = meta.context ?? 0
  return (
    <div className="convo-runs" ref={box}>
      {meta.mode && (
        <span className="convo-runs-item convo-runs-mode" title={t('convo.modeHint')}>
          {modeName(meta.mode)}
        </span>
      )}
      <span className="spacer" />
      {meta.branch && (
        <span className="convo-runs-item convo-runs-branch" title={t('convo.branch', { b: meta.branch })}>
          <GitBranch size={13} aria-hidden="true" />
          <span className="convo-runs-branch-name">{meta.branch}</span>
        </span>
      )}
      {meta.model && <span className="convo-runs-item convo-runs-model" title={meta.model}>{modelName(meta.model)}</span>}
      {used > 0 && (
        <button type="button" className="convo-runs-item convo-runs-ctx" aria-expanded={open} aria-label={t('convo.ctxLabel', { n: tokens(used) })} onClick={() => setOpen(!open)}>
          {t('convo.ctxShort', { n: tokens(used) })}
        </button>
      )}
      {open && (
        <div className="convo-runs-pop" role="dialog" aria-label={t('convo.ctxTitle')}>
          <div className="convo-runs-row">
            <span>{t('convo.ctxTitle')}</span>
            <span>{t('convo.ctxTokens', { n: used.toLocaleString(getLang()) })}</span>
          </div>
          {WINDOWS.map((w) => {
            const p = Math.min(100, Math.round((used / w) * 100))
            return (
              <div key={w} className="convo-runs-win">
                <div className="convo-runs-row">
                  <span>{t('convo.ctxOf', { w: tokens(w) })}</span>
                  <span>{p}%</span>
                </div>
                <div className="convo-runs-bar"><i style={{ width: p + '%' }} /></div>
              </div>
            )
          })}
          {meta.model && (
            <div className="convo-runs-row">
              <span>{t('convo.model')}</span>
              <span>{meta.model}</span>
            </div>
          )}
          <p className="convo-runs-note">{t('convo.ctxNote')}</p>
        </div>
      )}
    </div>
  )
}

// A row of the list of agents to go to (see Terminal.tsx).
export interface ConvoListRow {
  key: string
  title: string
  where: string // its host, and its workspace or session
  status: 'working' | 'blocked' | 'done' | 'idle' | 'unknown'
  since?: number // (unix ms) when its status last changed
  current: boolean
  onPick: () => void
}

const GROUPS: { name: I18nKey; of: (r: ConvoListRow) => boolean }[] = [
  { name: 'convo.group.blocked', of: (r) => r.status === 'blocked' },
  { name: 'convo.group.working', of: (r) => r.status === 'working' },
  { name: 'convo.group.recent', of: (r) => r.status !== 'blocked' && r.status !== 'working' },
]

// The latest to change first; those of no known time last, as they were.
const byRecent = (rows: ConvoListRow[]) => rows.map((r, i) => [r, i] as const).sort(([a, i], [b, j]) => (b.since ?? 0) - (a.since ?? 0) || i - j).map(([r]) => r)

// The agents in their order on the list: waiting first, then working, then
// the rest, the latest to change first in each (those of no known time, as
// after the gateway restarted, last).
export function listOrder(rows: ConvoListRow[]): ConvoListRow[] {
  return GROUPS.flatMap((g) => byRecent(rows.filter(g.of)))
}

// How long a status has lasted (at work, waiting), or how long ago it came
// (done, idle), in minutes at least.
export function when(r: { since?: number; status: string }, now: number): string {
  if (!r.since || now < r.since) return ''
  const m = Math.floor((now - r.since) / 60000)
  const going = r.status === 'working' || r.status === 'blocked'
  if (m < 1) return t(going ? 'convo.for.now' : 'convo.ago.now')
  if (m < 60) return t(going ? 'convo.for.m' : 'convo.ago.m', { n: m })
  if (m < 48 * 60) return t(going ? 'convo.for.h' : 'convo.ago.h', { n: Math.floor(m / 60) })
  return t(going ? 'convo.for.d' : 'convo.ago.d', { n: Math.floor(m / 1440) })
}

function AgentList({ rows, onPicked }: { rows: ConvoListRow[]; onPicked?: () => void }) {
  // (The times move on: drawn again every half minute.)
  const [now, setNow] = useState(Date.now())
  useEffect(() => {
    const timer = setInterval(() => setNow(Date.now()), 30000)
    return () => clearInterval(timer)
  }, [])
  return (
    <>
      {GROUPS.map((g) => {
        const of = byRecent(rows.filter(g.of))
        if (!of.length) return null
        return (
          <div key={g.name} className="convo-list-group" role="group" aria-label={t(g.name)}>
            <div className="convo-list-head">
              {t(g.name)}
              <span>{of.length}</span>
            </div>
            {of.map((r) => (
              <button
                key={r.key}
                type="button"
                className="convo-list-row"
                aria-current={r.current || undefined}
                onClick={() => {
                  onPicked?.()
                  r.onPick() // (of the one shown: nothing, or the end of a switch to another; the list may be of before)
                }}
              >
                <span className="convo-list-title">{r.title}</span>
                <span className="convo-list-sub">
                  {r.status === 'working' && <span className="convo-spin is-small" aria-hidden="true" />}
                  {r.status !== 'unknown' && (
                    <span className={'is-' + r.status}>
                      {t(`convo.status.${r.status}` as I18nKey)}
                      {when(r, now) && ' · ' + when(r, now)}
                    </span>
                  )}
                  <span className="convo-list-where">{r.where}</span>
                </span>
              </button>
            ))}
          </div>
        )
      })}
    </>
  )
}

// The list over the conversation: under the bar on a wide screen, from the
// bottom on a phone. Its rows are those of when it opened: statuses
// changing meanwhile do not move them under the finger.
function Picker({ rows, onClose }: { rows: ConvoListRow[]; onClose: () => void }) {
  const [shown] = useState(rows)
  const box = useRef<HTMLDivElement>(null)
  // (Once, when it opens: focus moved on in it stays where it was moved.)
  useEffect(() => {
    const prev = document.activeElement as HTMLElement | null
    ;(box.current?.querySelector<HTMLElement>('[aria-current="true"]') ?? box.current?.querySelector<HTMLElement>('button'))?.focus()
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') {
        e.preventDefault()
        e.stopPropagation()
        onClose()
      }
    }
    document.addEventListener('keydown', onKey, true)
    return () => {
      document.removeEventListener('keydown', onKey, true)
      prev?.focus?.()
    }
  }, [onClose])
  return (
    <>
      <div className="convo-picker-back" onClick={onClose} />
      <div className="convo-picker" role="dialog" aria-modal="true" aria-label={t('convo.pick')} ref={box}>
        <div className="convo-picker-title">{t('convo.pick')}</div>
        <AgentList rows={shown} onPicked={onClose} />
      </div>
    </>
  )
}

// The agent's task list, over the composer while some of it is left: one
// line (how far, what now) that opens to all of it. Folded at first on a
// small screen, where the thread needs the room.
// fold: a question is being answered over it: one line, unless opened then.
function Tasks({ tasks, fold }: { tasks: ConvoTask[]; fold?: boolean }) {
  const [opened, setOpened] = useState(() => !matchMedia('(max-width: 640px)').matches)
  const [asked, setAsked] = useState<boolean | null>(null) // (chosen while folded for a question)
  useEffect(() => setAsked(null), [fold])
  const open = fold ? (asked ?? false) : opened
  const setOpen = (v: boolean) => (fold ? setAsked(v) : setOpened(v))
  const done = tasks.filter((x) => x.status === 'completed').length
  if (done === tasks.length) return null
  const now = tasks.find((x) => x.status === 'in_progress')
  const next = now ?? tasks.find((x) => x.status !== 'completed')
  return (
    <div className="convo-tasks">
      <button type="button" className="convo-tasks-head" aria-expanded={open} onClick={() => setOpen(!open)}>
        <span className="convo-tasks-sum">
          {t('convo.tasks', { done, n: tasks.length })}
          {!open && next && <> · {t(now ? 'convo.taskNow' : 'convo.taskNext', { s: next.subject })}</>}
        </span>
        <span className="convo-tasks-toggle">{t(open ? 'convo.tasksHide' : 'convo.tasksShow')}</span>
      </button>
      {open && (
        <ol className="convo-tasks-list">
          {tasks.map((x, i) => (
            <li key={i} className={'is-' + x.status}>{x.subject}</li>
          ))}
        </ol>
      )}
    </div>
  )
}

// One item of the list. Memoised: a poll that changed one entry renders
// that one (the others keep their objects, see conversation.ts).
const ListItem = memo(
  function ListItem({ it, copy, open, onToggle, onShow }: { it: Item; copy: (s: string) => void; open: boolean; onToggle: (key: string, open: boolean) => void; onShow: (key: string, path?: string) => void }) {
    switch (it.kind) {
      case 'user':
        return <UserTurn e={it.e} onCopy={copy} />
      case 'note':
        return <p className="convo-note">{noteText(it.e)}</p>
      case 'summary':
        return (
          <details className="convo-summary">
            <summary>{t('convo.summary')}</summary>
            <div className="convo-md">
              <Markdown text={it.e.parts[0]?.text ?? ''} />
            </div>
          </details>
        )
      case 'answer':
        return <Answer it={it} onCopy={copy} onShow={onShow} />
      case 'text':
        return (
          <div className="convo-md convo-mid">
            <Markdown text={it.text} onCopyCode={copy} copyLabel={t('common.copy')} />
            {it.truncated && <p className="convo-cut">{t('convo.cut')}</p>}
          </div>
        )
      case 'work':
        return <Work it={it} open={open} onToggle={onToggle} onCopy={copy} />
    }
  },
  (a, b) => a.copy === b.copy && a.open === b.open && a.onToggle === b.onToggle && a.onShow === b.onShow && sameItem(a.it, b.it),
)

function sameItem(a: Item, b: Item): boolean {
  if (a.kind !== b.kind) return false
  switch (a.kind) {
    case 'answer': {
      const o = b as typeof a
      return a.text === o.text && a.truncated === o.truncated && a.ts === o.ts && a.span === o.span && sameEdits(a.edits, o.edits)
    }
    case 'text': {
      const o = b as typeof a
      return a.text === o.text && a.truncated === o.truncated
    }
    case 'work': {
      const o = b as typeof a
      return a.live === o.live && a.moving === o.moving && a.busy === o.busy && a.steps.length === o.steps.length && a.steps.every((p, i) => p === o.steps[i])
    }
    default:
      return a.e === (b as typeof a).e
  }
}

const noShow = () => {}
const itemKey = (it: Item) => (it.kind === 'work' || it.kind === 'answer' || it.kind === 'text' ? it.key : 'e' + it.e.seq)
// (Claude Code's own words that it was stopped, in the person's language.)
const noteText = (e: ConvoEntry) => {
  const s = e.parts.map((p) => p.text).join(' ')
  if (s === '[Request interrupted by user]') return t('convo.interrupted')
  if (s === '[Request interrupted by user for tool use]') return t('convo.interruptedTool')
  return s
}

function clockMs(ms: number): string {
  return ms ? new Date(ms).toLocaleTimeString(getLang(), { hour: '2-digit', minute: '2-digit' }) : '—'
}

function clock(ts?: string): string {
  if (!ts) return ''
  const d = new Date(ts)
  if (isNaN(d.getTime())) return ''
  return d.toLocaleTimeString(getLang(), { hour: '2-digit', minute: '2-digit' })
}

function UserTurn({ e, onCopy }: { e: ConvoEntry; onCopy: (s: string) => void }) {
  const text = e.parts
    .filter((p) => p.kind === 'text')
    .map((p) => p.text)
    .join('\n')
  const images = e.parts.filter((p) => p.kind === 'image').length
  return (
    <div className={'convo-user' + (e.mid ? ' is-mid' : '')}>
      <div className="convo-user-text">
        {text}
        {images > 0 && !/\[Image #\d+\]/.test(text) && <span className="convo-image">{t(images === 1 ? 'convo.images1' : 'convo.images', { n: images })}</span>}
      </div>
      <div className="convo-meta">
        {e.mid && <span>{t('convo.midSent')}</span>}
        <span>{clock(e.ts)}</span>
        {text && (
          <button type="button" className="link-btn" onClick={() => onCopy(text)}>
            {t('common.copy')}
          </button>
        )}
      </div>
    </div>
  )
}

// The agent's answer: its text, what the turn changed, copy and when.
const CARD_FILES = 6 // files listed on a turn's card; the rest in the panel

function Answer({ it, onCopy, onShow }: { it: Extract<Item, { kind: 'answer' }>; onCopy: (s: string) => void; onShow: (key: string, path?: string) => void }) {
  return (
    <div className="convo-answer">
      <div className="convo-md">
        <Markdown text={it.text} onCopyCode={onCopy} copyLabel={t('common.copy')} />
        {it.truncated && <p className="convo-cut">{t('convo.cut')}</p>}
      </div>
      {it.edits && (
        <div className="convo-changes">
          <div className="convo-changes-head">
            <span>
              {t(it.edits.files.length === 1 ? 'convo.changes1' : 'convo.changes', { n: it.edits.files.length })}{' '}
              <LineCounts add={it.edits.add} del={it.edits.del} />
            </span>
            <button type="button" className="btn btn-ghost convo-changes-see" onClick={() => onShow(it.key)}>
              {t('convo.seeChanges')}
            </button>
          </div>
          {it.edits.files.slice(0, CARD_FILES).map((f) => (
            <button key={f.path} type="button" className="convo-changes-file" onClick={() => onShow(it.key, f.path)} title={f.path}>
              <span className="convo-changes-path">{shortPath(f.path)}</span>
              <LineCounts add={f.add} del={f.del} />
              <ChevronRight size={14} aria-hidden="true" />
            </button>
          ))}
          {it.edits.files.length > CARD_FILES && (
            <button type="button" className="convo-changes-file convo-changes-more" onClick={() => onShow(it.key)}>
              {t('convo.moreFiles', { n: it.edits.files.length - CARD_FILES })}
            </button>
          )}
        </div>
      )}
      <div className="convo-meta">
        <button type="button" className="link-btn" onClick={() => onCopy(it.text)}>
          {t('common.copy')}
        </button>
        {it.span !== undefined && <span>{t('convo.worked', { d: duration(it.span) })}</span>}
        <span>{clock(it.ts)}</span>
      </div>
    </div>
  )
}

function LineCounts({ add, del }: { add: number; del: number }) {
  if (!add && !del) return null
  return (
    <span className="convo-lines">
      {add > 0 && <span className="convo-lines-add">+{add}</span>}
      {del > 0 && <span className="convo-lines-del">−{del}</span>}
    </span>
  )
}

// A run of steps between what the agent wrote: one line (what it did; while
// it goes on, the step under way and for how long); opened, a line a step,
// each opened on its own (a call's command, output and changes, a thought).
function Work({ it, open, onToggle, onCopy }: { it: Extract<Item, { kind: 'work' }>; open: boolean; onToggle: (key: string, open: boolean) => void; onCopy: (s: string) => void }) {
  const tools = it.steps.flatMap((s) => (!isNote(s) && s.tool ? [s.tool] : []))
  const counts = new Map<Category, number>()
  for (const tl of tools) counts.set(category(tl.name), (counts.get(category(tl.name)) ?? 0) + 1)
  const order: Category[] = ['edit', 'cmd', 'read', 'search', 'web', 'agent', 'other']
  const sum = order
    .filter((c) => counts.has(c))
    .map((c) => count(c, counts.get(c)!))
    .join(t('convo.sep'))
  const failed = tools.filter((tl) => tl.error).length
  const busy = it.busy
  const now = useTick(!!busy)
  // (Of thinking only: that it thought, and the start of it.)
  const thought = !tools.length ? it.steps.find((x): x is ConvoPart => !isNote(x) && x.kind === 'thinking') : undefined
  const head = sum ? '' : thought ? t('convo.thinking') : it.live ? t('convo.workingNow') : t('convo.steps')
  // While a step is under way, the line says it (what it runs, for how long);
  // the counts come back once it is done.
  const since = busy && partTime.has(busy) ? ms(partTime.get(busy)) : 0
  return (
    <div className="convo-work" data-work={it.key}>
      <button type="button" className="convo-work-head" aria-expanded={open} onClick={() => onToggle(it.key, !open)} title={isTouchOnly ? undefined : t(open ? 'convo.foldHint' : 'convo.openHint')}>
        <ChevronRight size={15} className="convo-chev" aria-hidden="true" />
        {busy ? (
          <span className="convo-work-text is-live">
            <span className="convo-shimmer">{busy.tool ? liveText(busy.tool) : t('convo.live.thinking')}</span>
            {since > 0 && now >= since && <span className="convo-live-time">{duration(now - since)}</span>}
          </span>
        ) : (
          <span className={'convo-work-text' + (thought ? ' is-thought' : '')}>
            {head || sum}
            {thought && <span className="convo-work-sum"> · {firstLine(thought.text ?? '')}</span>}
            {failed > 0 && <span className="convo-failed">{t('convo.failed', { n: failed })}</span>}
          </span>
        )}
      </button>
      {open && (
        <div className="convo-work-body">
          {it.steps.map((s, i) =>
            isNote(s) ? (
              s.role === 'summary' ? (
                <ListItem key={i} it={{ kind: 'summary', e: s }} copy={onCopy} open={false} onToggle={onToggle} onShow={noShow} />
              ) : (
                <p key={i} className="convo-note">{noteText(s)}</p>
              )
            ) : s.tool ? (
              <ToolRow key={i} tool={s.tool} onCopy={onCopy} live={s === busy} />
            ) : s.kind === 'thinking' ? (
              <Thinking key={i} text={s.text ?? ''} live={s === busy} />
            ) : null,
          )}
        </div>
      )}
    </div>
  )
}

function firstLine(s: string): string {
  const l = s.split('\n').find((x) => x.trim()) ?? ''
  return l.length > 160 ? l.slice(0, 160) + '…' : l
}

// A file's path as a step shows it: its last few parts.
function shortPath(p: string): string {
  const parts = p.split('/').filter(Boolean)
  return parts.length > 3 ? '…/' + parts.slice(-3).join('/') : p
}

// What a step's line says it acted on, and whether that is code (a
// command, a path, a pattern) to show as such.
function object(tool: ConvoTool): { text: string; code: boolean } {
  const v = verbOf(tool.name)
  const d = firstLine(tool.detail ?? '')
  // A command: what it is for, as Claude Code says it; else the command,
  // without the "cd <folder> &&" that begins many alike (all of it opened).
  if (v === 'cmd' && tool.title) return { text: tool.title, code: false }
  if (v === 'cmd' && d) return { text: d.replace(/^cd\s+(?:"[^"]*"|'[^']*'|[^\s&;|]+)\s*&&\s*/, ''), code: true }
  if (tool.title) return { text: tool.title, code: false }
  if (v === 'read' || v === 'edit' || v === 'write') return { text: shortPath(d), code: true }
  return { text: d, code: v === 'cmd' || v === 'search' || v === 'web' }
}

function liveText(tool: ConvoTool): string {
  const v = verbOf(tool.name)
  const o = object(tool).text
  return v ? t(`convo.live.${v}` as I18nKey, { s: o }) : t('convo.live.other', { s: tool.name })
}

function Thinking({ text, live }: { text: string; live?: boolean }) {
  const [open, setOpen] = useState(false)
  if (live) return null // (said under the steps, as the step under way)
  return (
    <div className="convo-step is-thought">
      <button type="button" className="convo-step-head" aria-expanded={open} onClick={() => setOpen(!open)}>
        <span className="convo-step-verb">{t('convo.thinking')}</span>
        {!open && <span className="convo-step-obj">{firstLine(text)}</span>}
      </button>
      {open && <div className="convo-thinking">{text}</div>}
    </div>
  )
}

// Of a command that failed, its exit code (Claude Code's result begins so).
const exitCode = (tool: ConvoTool) => (tool.error ? /^Exit code (\d{1,3})\b/.exec(tool.result ?? '')?.[1] : undefined)

function ToolRow({ tool, onCopy, live }: { tool: ConvoTool; onCopy: (s: string) => void; live?: boolean }) {
  const [open, setOpen] = useState(false)
  const [full, setFull] = useState(false)
  const objRef = useRef<HTMLSpanElement>(null)
  const [clipped, setClipped] = useState(false) // (opened while the line cut its object short)
  if (live) return null // (said on the turn's line, as the step under way)
  const v = verbOf(tool.name)
  const obj = object(tool)
  const cat = category(tool.name)
  const isEdit = !!tool.patch || !!tool.same || tool.old !== undefined || tool.new !== undefined
  const code = exitCode(tool)
  // What the line does not show of what it acted on: a command of more
  // lines or longer than a line holds (it shows the first), a subagent's
  // prompt. A file's path is on the line and in its tooltip.
  const d = tool.detail ?? ''
  const more = cat === 'cmd' ? !!tool.title || clipped || d.includes('\n') || d.length > 80 || obj.text !== d : cat !== 'edit' && cat !== 'read' && !!d && (!!tool.title || d.includes('\n') || d.length > 60)
  // An edit that went through says so to the model ("The file … has been
  // updated"): nothing for the person. What it changed shows instead (not
  // for a notebook's: its result is all there is of it).
  // (A failed command's exit code is on the line: its output goes without it.)
  const receipt = isEdit && !tool.error && tool.name !== 'NotebookEdit'
  const result = tool.done && !receipt ? (code ? tool.result?.replace(/^Exit code \d+\n?/, '') : tool.result) : undefined
  return (
    <div className={'convo-step' + (tool.error ? ' is-error' : '')}>
      <button
        type="button"
        className="convo-step-head"
        aria-expanded={open}
        onClick={() => {
          const el = objRef.current
          if (!open) setClipped(!!el && el.scrollWidth > el.clientWidth + 1)
          setOpen(!open)
        }}
        title={cat === 'cmd' ? firstLine(d) : d || tool.title}
      >
        <span className="convo-step-verb">{v ? t(`convo.v.${v}` as I18nKey) : tool.name}</span>
        <span ref={objRef} className={'convo-step-obj' + (obj.code ? ' is-code' : '')}>{v === 'cmd' && obj.code ? <Code text={obj.text} lang="bash" /> : obj.text}</span>
        {cat === 'edit' && <LineCounts add={tool.add ?? 0} del={tool.del ?? 0} />}
        {tool.error && <span className="convo-failed">{code ? t('convo.exitCode', { n: code }) : t('convo.toolFailed')}</span>}
      </button>
      {open && (
        <div className="convo-step-body">
          {more && (
            <div className="convo-pre-wrap">
              <pre className="convo-pre">{v === 'cmd' ? <Code text={d} lang="bash" /> : d}</pre>
              <button type="button" className="md-code-copy" onClick={() => onCopy(d)}>{t('common.copy')}</button>
            </div>
          )}
          {tool.same ? <div className="convo-result-head">{t('convo.noChange')}</div> : tool.patch ? <Hunks hunks={tool.patch} cut={tool.patch_cut} /> : isEdit && <Diff old={tool.old ?? ''} new={tool.new ?? ''} />}
          {!tool.done ? (
            <div className="convo-result-head">{t('convo.running')}</div>
          ) : result ? (
            <>
              <div className="convo-result-head">
                {t('convo.output', { n: Math.max(0, (tool.result_lines ?? 0) - (code && result !== tool.result ? 1 : 0)) })}
                {tool.truncated && <span> · {t('convo.cut')}</span>}
              </div>
              <pre className={'convo-pre convo-result' + (full ? ' is-full' : '')}>{result}</pre>
              {!full && result.split('\n').length > 12 && (
                <button type="button" className="link-btn" onClick={() => setFull(true)}>{t('convo.showAll')}</button>
              )}
            </>
          ) : (
            !receipt && <div className="convo-result-head">{t('convo.noOutput')}</div>
          )}
        </div>
      )}
    </div>
  )
}

// An edit as Claude Code reported making it: its hunks with the lines'
// numbers (after the edit; before it, for a line removed), the lines left
// out between them said.
function Hunks({ hunks, cut }: { hunks: ConvoHunk[]; cut?: boolean }) {
  const rows: ReactNode[] = []
  let end = 0 // (the new numbering just past the hunk before)
  hunks.forEach((h, hi) => {
    if (hi > 0 && h.new_start > end) rows.push(<div key={'g' + hi} className="convo-hunk-gap">{t('convo.unchanged', { n: h.new_start - end })}</div>)
    let o = h.old_start
    let n = h.new_start
    h.lines.forEach((l, li) => {
      const k = l[0]
      const no = k === '-' ? o++ : k === '+' ? n++ : (o++, n++)
      rows.push(
        <div key={hi + '.' + li} className={'convo-hunk-line' + (k === '+' ? ' is-add' : k === '-' ? ' is-del' : '')}>
          <span className="convo-hunk-no">{no}</span>
          <span className="convo-hunk-sign">{k === ' ' ? '' : k === '-' ? '−' : '+'}</span>
          <span className="convo-hunk-text">{l.slice(1)}</span>
        </div>,
      )
    })
    end = n
  })
  return (
    <div className="convo-hunks">
      {rows}
      {cut && <div className="convo-hunk-gap">{t('convo.patchCut')}</div>}
    </div>
  )
}

// A file a turn made anew: all of it, as added lines.
const created = (tool: ConvoTool): ConvoHunk[] | undefined => {
  if (tool.name !== 'Write' || tool.patch || tool.same || !tool.new) return undefined
  const lines = tool.new.replace(/\n$/, '').split('\n')
  return [{ old_start: 0, new_start: 1, lines: lines.map((l) => '+' + l) }]
}

// The changes a turn made, beside the thread (over it, where narrow): its
// files, each with its edits in order.
function ChangesPanel({ edits, focus, onClose }: { edits: Edits; focus?: string; onClose: () => void }) {
  const box = useRef<HTMLDivElement>(null)
  useEffect(() => {
    const el = focus ? box.current?.querySelector(`[data-file="${CSS.escape(focus)}"]`) : null
    el?.scrollIntoView({ block: 'start' })
  }, [focus])
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      const el = e.target as HTMLElement | null
      if (e.key !== 'Escape' || (el && (el.tagName === 'INPUT' || el.tagName === 'TEXTAREA') && !el.classList.contains('xterm-helper-textarea'))) return
      if (document.querySelector('[aria-modal="true"], [role="menu"]')) return
      e.preventDefault()
      e.stopPropagation()
      onClose()
    }
    window.addEventListener('keydown', onKey, true)
    return () => window.removeEventListener('keydown', onKey, true)
  }, [onClose])
  return (
    <aside className="convo-panel" aria-label={t('convo.changesTitle')}>
      <div className="convo-panel-head">
        <span className="convo-panel-title">{t('convo.changesTitle')}</span>
        <LineCounts add={edits.add} del={edits.del} />
        <span className="spacer" />
        <button type="button" className="icon-btn" onClick={onClose} aria-label={t('common.close')} title={t('common.close') + ' (Esc)'}>
          <X size={16} />
        </button>
      </div>
      <div className="convo-panel-files">
        {edits.files.map((f) => (
          <button key={f.path} type="button" className="convo-changes-file" onClick={() => box.current?.querySelector(`[data-file="${CSS.escape(f.path)}"]`)?.scrollIntoView({ block: 'start', behavior: 'smooth' })} title={f.path}>
            <span className="convo-changes-path">{shortPath(f.path)}</span>
            <LineCounts add={f.add} del={f.del} />
          </button>
        ))}
      </div>
      <div className="convo-panel-body" ref={box}>
        {edits.files.map((f) => (
          <section key={f.path} className="convo-panel-file" data-file={f.path}>
            <div className="convo-panel-file-head" title={f.path}>
              <span className="convo-changes-path">{shortPath(f.path)}</span>
              <LineCounts add={f.add} del={f.del} />
            </div>
            {f.tools.map((tl, i) => {
              const h = tl.patch ?? created(tl)
              return (
                <div key={i} className="convo-panel-edit">
                  {f.tools.length > 1 && <div className="convo-hunk-gap">{t('convo.editN', { n: i + 1 })}</div>}
                  {tl.same ? <div className="convo-hunk-gap">{t('convo.noChange')}</div> : h ? <Hunks hunks={h} cut={tl.patch_cut} /> : <Diff old={tl.old ?? ''} new={tl.new ?? ''} />}
                </div>
              )
            })}
          </section>
        ))}
      </div>
    </aside>
  )
}

function Diff({ old, new: nw }: { old: string; new: string }) {
  const rows: [string, string][] = []
  if (old) for (const l of old.split('\n')) rows.push(['del', l])
  if (nw) for (const l of nw.split('\n')) rows.push(['add', l])
  const shown = rows.slice(0, 60)
  return (
    <pre className="convo-pre convo-diff">
      {shown.map(([k, l], i) => (
        <span key={i} className={'convo-' + k}>
          {(k === 'del' ? '- ' : '+ ') + l + '\n'}
        </span>
      ))}
      {rows.length > shown.length && <span className="convo-more">{t('convo.moreLines', { n: rows.length - shown.length })}</span>}
    </pre>
  )
}
