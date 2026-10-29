import { createContext, Fragment, memo, useCallback, useContext, useEffect, useLayoutEffect, useMemo, useRef, useState, type ReactNode } from 'react'
import { ArrowDown, Check, ChevronDown, ChevronRight, ChevronUp, ChevronsUpDown, Clock, GitBranch, Loader, MoreVertical, PanelLeft, Search, TriangleAlert, X, Plus } from 'lucide-react'
import { t, getLang, type I18nKey } from '../i18n'
import { copyText, isCoarse, Menu, type MenuItem } from '../components/ui'
import { local } from '../storage'
import { Markdown } from './markdown'
import { useSubConvo } from './subagent'
import { NEW_AGENT } from './newAgent'
import { Code } from './Code'
import { ConvoSearchPanel } from './convoSearchPanel'
import { itemKey } from './convoSearch'
import { exportMarkdown } from './convoExport'
import { AgentIcon } from './agentIcons'
import { matchQuery } from './agents'
import type { Convo, ConvoCost, ConvoEntry, ConvoShell, ConvoHunk, ConvoMeta, ConvoPart, ConvoQueued, ConvoRunning, ConvoTask, ConvoTool, ConvoTrouble, ConvoEvent } from './conversation'
import { category, isNote, items, ms, partTime, sameEdits, toolName, verbOf, type Category, type Edits, type Item, type Verb } from './convoItems'

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
  bg?: 'running' | 'stale' | 'checking' // (its turn over: subagents still out, or that being read for; see agents.ts)
  dialog?: boolean // (tmux: Claude Code has a dialog of its own open, a picker, say: not a question to answer here)
  other?: string // the program in the pane, when it is not Claude Code
  program?: string // the program in the pane, for its icon
}

// A message sent from the composer while the conversation shows: "waiting"
// until the log has it (then it is dropped), "unseen" when that took too
// long, "sent" when it can no longer be told (another log came in front).
export interface ConvoPending {
  id: number
  text: string
  state: 'sending' | 'waiting' | 'unseen' | 'sent'
  // What the pane's screen said when the message had not turned up in the
  // log: waiting (the words are in its input, so it was typed and not
  // taken in), taken (the input no longer holds them), or neither, where
  // the screen does not say. Undefined while it is still being looked at,
  // or where it could not be read.
  look?: { waiting: boolean; known: boolean; screen: string } | 'looking' | 'failed'
}

const HINT_AFTER = 20000 // working with nothing new this long: say why
const QUIET_LONG = 5 * 60 * 1000 // its log not grown this long: said, in minutes
const SIDE_KEY = 'corral.convoSide'
const SIDE_MIN = 900 // (px) the view's width from which the list goes beside the thread
// The list's width beside the thread (px): dragged by its edge, kept in the
// browser; never past half the view.
const SIDE_W_KEY = 'corral.convoSideWidth'
const SIDE_W = 288
const SIDE_W_MIN = 220
const SIDE_W_MAX = 520
const sideWidthMax = (view: number) => Math.min(SIDE_W_MAX, Math.floor(view / 2))
const sideWidthOf = (w: number, view: number) => Math.max(SIDE_W_MIN, Math.min(Math.round(w), sideWidthMax(view)))
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
  onEnter,
  onShowTerminal,
  onCopied,
  composer,
  onSendNow,
  sendNow,
  mode,
  model,
  effort,
  lead,
  more,
  query,
  onQuery,
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
  onEnter?: (id: number) => Promise<boolean> // (a message seen still in the input: Enter for it, where it still is)
  onShowTerminal: () => void
  onCopied: () => void
  composer?: ReactNode // (the terminal's composer, while it is open)
  onSendNow?: () => void // (messages wait to be taken in: Claude Code's "send now")
  sendNow?: string // what became of the last "send now", to say
  mode?: ModeSwitch // (its permission mode can be switched from here)
  more?: () => (MenuItem | null)[] // (a menu at the bar's end: what else is done with it)
  query?: string // what the side list is searched for, kept by the page (it outlives this view)
  onQuery?: (q: string) => void
  model?: ModelSwitch // (so can its model)
  effort?: EffortSwitch // (and how hard it is asked to think)
  lead?: ReactNode // (first in the bar: the agents page's way back to its list, on a phone)
}) {
  const working = agent?.status === 'working'
  const waits = agent?.status === 'blocked'
  // The list beside the thread: where the view is wide enough, unless folded away (kept on this device).
  const root = useRef<HTMLDivElement>(null)
  const [wide, setWide] = useState(false)
  const [viewW, setViewW] = useState(0)
  const [sideW, setSideW] = useState(() => Number(local.get(SIDE_W_KEY)) || SIDE_W)
  const sideWidth = sideWidthOf(sideW, viewW || SIDE_MIN)
  const sideEl = useRef<HTMLElement>(null)
  const keepSideW = (w: number) => {
    const v = sideWidthOf(w, viewW || SIDE_MIN)
    setSideW(v)
    if (v === SIDE_W) local.remove(SIDE_W_KEY)
    else local.set(SIDE_W_KEY, String(v))
    if (sideEl.current) sideEl.current.style.width = v + 'px' // (as it is drawn: a drag sets it by hand)
    return v
  }
  // (A drag sizes the list by its style, a frame at a time, and is told
  // once, when it ends: the thread is not drawn again for each move.)
  const drag = useRef<{ id: number; x: number; w: number; to: number; frame: number } | null>(null)
  const endDrag = (keep: boolean) => {
    const d = drag.current
    if (!d) return
    drag.current = null
    cancelAnimationFrame(d.frame)
    keepSideW(keep ? d.to : d.w)
  }
  const [folded, setFoldedState] = useState(() => local.get(SIDE_KEY) === '0')
  const setFolded = (v: boolean) => {
    setFoldedState(v)
    local.set(SIDE_KEY, v ? '0' : '1')
  }
  useLayoutEffect(() => {
    const el = root.current
    if (!el) return
    const look = () => {
      setWide(el.clientWidth >= SIDE_MIN)
      setViewW(el.clientWidth)
    }
    look()
    const ro = new ResizeObserver(look)
    ro.observe(el)
    return () => ro.disconnect()
  }, [])
  const sideShown = !!side && wide && !folded
  const sideShownNow = useRef(sideShown)
  sideShownNow.current = sideShown
  useEffect(() => {
    // (The view narrowed mid-drag: what is dragged to keeps within it.)
    const d = drag.current
    if (!d) return
    d.to = sideWidthOf(d.to, viewW || SIDE_MIN)
    if (sideEl.current) sideEl.current.style.width = d.to + 'px'
  }, [viewW])
  useEffect(() => {
    if (!sideShown && drag.current) {
      cancelAnimationFrame(drag.current.frame)
      drag.current = null // (its edge gone mid-drag: what was dragged is let go)
    }
  }, [sideShown])
  const [picking, setPicking] = useState<boolean | 'search'>(false) // ('search': by ⌘K, its search box first)
  // ⌘K (Ctrl+K): the list's search box, shown and in focus; where the list
  // is not beside the thread, the switcher, its box first. (A count: each
  // press puts the focus there again.)
  const [searchAsked, setSearchAsked] = useState(0)
  const unpick = useCallback(() => setPicking(false), [])
  useEffect(() => {
    if (sideShown || !active) setPicking(false) // (a tab sent to the back keeps no dialog open)
  }, [sideShown, active])
  // What the side list is searched for (the page's, else kept here: the keys below go by it).
  const [ownQuery, setOwnQuery] = useState('')
  const sideQuery = query ?? ownQuery
  const setSideQuery = onQuery ?? setOwnQuery
  const shownQuery = useRef('')
  shownQuery.current = sideShown ? sideQuery : ''
  // Alt+Up and Alt+Down: the agent before and after on the list, of those
  // it shows when searched (not while a field with text in it has the keys,
  // but for its search box).
  const sideRef = useRef(side)
  sideRef.current = side
  useEffect(() => {
    if (!active) return
    const onKey = (e: KeyboardEvent) => {
      if (!e.altKey || e.ctrlKey || e.metaKey || e.shiftKey || (e.key !== 'ArrowUp' && e.key !== 'ArrowDown') || underDialog(e)) return
      const el = e.target as HTMLInputElement | null
      if (el && (el.tagName === 'INPUT' || el.tagName === 'TEXTAREA') && !el.classList.contains('xterm-helper-textarea') && el.value && !el.closest('.list-search')) return
      const q = shownQuery.current
      const list = listOrder((sideRef.current?.rows ?? []).filter((r) => !q.trim() || matchQuery(q, r.title, r.where, r.program)))
      // (Searched, the one shown may not be among them: one found is gone to.)
      if (!list.length || (list.length === 1 && list[0].current)) return
      e.preventDefault()
      e.stopPropagation()
      const at = list.findIndex((r) => r.current)
      // (The one shown not among them: from the first down, the last up.)
      const next = at < 0 ? list[e.key === 'ArrowDown' ? 0 : list.length - 1] : list[(at + (e.key === 'ArrowDown' ? 1 : list.length - 1)) % list.length]
      if (!next.current) next.onPick()
    }
    window.addEventListener('keydown', onKey, true)
    return () => window.removeEventListener('keydown', onKey, true)
  }, [active])
  const list = useMemo(() => items(convo.entries, working || waits, working), [convo.entries, working, waits])
  const box = useRef<HTMLDivElement>(null)
  const col = useRef<HTMLDivElement>(null)
  const stuck = useRef(true) // following the end
  const [away, setAway] = useState(false) // (not following, the end out of view: the button to it shows)
  // What the person did to the scroll lately (see the listeners below):
  // only that leaves the end, or comes back to following it.
  const did = useRef({ pressed: false, downAt: -1e9, drag: false, touch: false, touchY: 0, fling: 0, touchEndAt: -1e9, top: 0, h: 0, ch: 0, set: -1, glide: false })
  // The end put in view (its place noted: the scroll it makes is not theirs).
  const pin = useCallback(() => {
    const el = box.current
    if (!el) return
    el.scrollTop = el.scrollHeight
    did.current.set = el.scrollTop
  }, [])
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
      if (!e.ctrlKey || e.metaKey || e.altKey || e.shiftKey || e.key.toLowerCase() !== 'o' || e.isComposing || underDialog(e) || subsNow.current) return
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
  // The changes of a turn (by its answer's key), beside the thread; or a
  // subagent's own conversation (those it sent off in turn, gone into and
  // back out of), in the same place: one at a time.
  const [changes, setChanges] = useState<{ key: string; path?: string } | null>(null)
  const [subs, setSubs] = useState<Sub[] | null>(null)
  const subsNow = useRef(subs) // (a subagent's conversation in front: the thread's keys are not the thread's)
  subsNow.current = subs
  const show = useCallback((key: string, path?: string) => {
    setSubs(null)
    setChanges({ key, path })
  }, [])
  const unshow = useCallback(() => setChanges(null), [])
  const openSub = useCallback((s: Sub) => {
    setChanges(null)
    setSubs([s])
  }, [])
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
  // (Long: by how long since its log last grew, as the gateway counts it;
  // not while reading fails, which is said otherwise.)
  const quietLong = working && !convo.failing && convo.quietAt !== undefined && now - convo.quietAt >= QUIET_LONG ? now - convo.quietAt : 0
  // How long this turn has gone on: from the last thing the person wrote.
  const lastUser = useMemo(() => {
    for (let i = convo.entries.length - 1; i >= 0; i--) if (convo.entries[i].role === 'user' && !convo.entries[i].mid && !convo.entries[i].gone) return ms(convo.entries[i].ts)
    return 0
  }, [convo.entries])

  // Another conversation in front (another pane, another tab's, or another
  // log of the same pane: /clear, /resume): from its end, nothing of the one
  // before opened or shown (its items' keys may come again in this one).
  // What was read of each conversation (the last entry in view at its end,
  // shown in front): coming back, what came after it is marked so (one
  // line before the first item that begins after it), for this visit.
  const [unreadAfter, setUnreadAfter] = useState<number | null>(null)
  const unreadAt = unreadAfter === null ? -1 : list.findIndex((it) => startSeq(it) > unreadAfter)
  // (Coming back to it while following: shown from that line, if there is
  // more than a screen of it; once, as soon as the line is known.)
  const toUnread = useRef<{ after: number; until: number } | null>(null)
  const lastSeq = convo.entries.length ? convo.entries[convo.entries.length - 1].seq : 0
  const ofNow = useRef({ of: convo.of, gen: convo.gen, last: lastSeq })
  ofNow.current = { of: convo.of, gen: convo.gen, last: lastSeq }
  const markRead = useCallback(() => {
    const el = box.current
    const o = ofNow.current
    if (!el || !o.of || !o.last || !activeNow.current || document.hidden) return
    if (el.scrollHeight - el.scrollTop - el.clientHeight < 48) readUpTo(o.of, o.gen, o.last)
  }, [])
  // Back to one read before (its entries kept, of the same reading of its
  // log): where it was left, followed or read at (its items opened as they were).
  const shownOf = useRef<{ of?: string; gen: number } | null>(null)
  const restoring = useRef<Place | null>(null) // (until its items are opened as they were: put back then)
  const openedNow = useRef(opened)
  openedNow.current = opened
  const allOpenNow = useRef(allOpen)
  allOpenNow.current = allOpen
  const keepPlace = useCallback(() => {
    markRead()
    const was = shownOf.current
    if (was?.of) keepAt(was.of, { gen: was.gen, stuck: stuck.current, anchors: anchors.current, noted: notedList.current, opened: openedNow.current, allOpen: allOpenNow.current })
  }, [])
  useLayoutEffect(() => {
    if (shownOf.current?.of !== convo.of) keepPlace()
    shownOf.current = { of: convo.of, gen: convo.gen }
    const seen = convo.of ? reads.get(convo.of) : undefined
    // (What is new may come with the next read, after what was kept shows.)
    const after = seen && seen.gen === convo.gen ? seen.seq : null
    setUnreadAfter(after)
    const p = convo.of ? places.get(convo.of) : undefined
    const back = !!p && p.gen === convo.gen && !p.stuck && p.anchors.length > 0 && convo.entries.length > 0
    if (back) {
      // (Of its runs opened, those still there: as the list keeps them, so
      // what is put back is what will be drawn.)
      const keys = new Set(list.flatMap((it) => (it.kind === 'work' ? [it.key] : [])))
      if (!Object.keys(p.opened).every((k) => keys.has(k))) p.opened = Object.fromEntries(Object.entries(p.opened).filter(([k]) => keys.has(k)))
    }
    stuck.current = !back
    setAway(back)
    setOpened(back ? p.opened : {})
    setAllOpen(back ? p.allOpen : false)
    setChanges(null)
    setSubs(null)
    anchors.current = back ? p.anchors : []
    notedList.current = back ? p.noted : []
    restoring.current = back ? p : null
    toUnread.current = !back && after !== null ? { after, until: Date.now() + 8000 } : null
    if (!back) pin()
  }, [convo.of, convo.gen])
  useEffect(() => keepPlace, [keepPlace]) // (and as the view goes)
  // (And as it goes to the back: another tab, the page hidden; coming to
  // the front again, what came meanwhile is marked as on coming back to it.)
  const comeBack = useCallback(() => {
    const o = ofNow.current
    const seen = o.of ? reads.get(o.of) : undefined
    if (!seen || seen.gen !== o.gen) return
    setUnreadAfter(seen.seq)
    if (stuck.current) toUnread.current = { after: seen.seq, until: Date.now() + 8000 }
  }, [])
  const wasActive = useRef(active)
  useEffect(() => {
    if (!active) keepPlace()
    else if (!wasActive.current) comeBack()
    wasActive.current = active
    const onSeen = () => (document.hidden ? keepPlace() : activeNow.current && comeBack())
    document.addEventListener('visibilitychange', onSeen)
    return () => document.removeEventListener('visibilitychange', onSeen)
  }, [active, keepPlace, comeBack])
  // Reads in that bring nothing (parts all of one long line): after three
  // in a row it goes on only when asked (the note becomes a button).
  const [olderPaused, setOlderPaused] = useState(false)
  const olderPausedNow = useRef(false)
  // (Searching, the part before is read in only when asked: a batch read in
  // for the search, its top near, does not read on by itself.)
  const [searching, setSearching] = useState(false)
  olderPausedNow.current = olderPaused || searching
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
  }, [convo.older, convo.olderCut, convo.loadingOlder, convo.of, olderPaused, searching])
  // What is in view stays where it is as items come in or change above it
  // (the part before read in, a turn opened higher up), unless the end is
  // kept in view: a few items in view are noted with where they are, and
  // after each change the first still there is put back. (By hand: Safari
  // has no scroll anchoring. The column's children, past its first note,
  // are the list's items in order.)
  // (Each also by its place from the end: kept when only what was before
  // changed, as a run read from its middle takes in its start, and its key.)
  const anchors = useRef<Anchor[]>([])
  const notedList = useRef<typeof list>([]) // (the list they were noted in)
  const listNow = useRef(list)
  listNow.current = list
  const note = useCallback(() => {
    const el = box.current
    const c = col.current
    if (!el || !c) return
    const view = el.getBoundingClientRect()
    const kids = [...c.children].filter((k) => !k.hasAttribute('data-aside'))
    const out: Anchor[] = []
    const last = listNow.current.length ? placeKey(listNow.current[listNow.current.length - 1]) : ''
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
      out.push({ key: placeKey(it), top: r.top - view.top, fromEnd: listNow.current.length - i, last })
    }
    anchors.current = out
    notedList.current = listNow.current
  }, [])
  useLayoutEffect(() => {
    const el = box.current
    const c = col.current
    // (Being put back: not before its items are opened as they were, nor noted till then.)
    const r = restoring.current
    if (r && (opened !== r.opened || allOpen !== r.allOpen)) return
    if (el && c && !stuck.current && anchors.current.length) {
      const view = el.getBoundingClientRect()
      const kids = [...c.children].filter((k) => !k.hasAttribute('data-aside'))
      const at = new Map(list.map((it, i) => [placeKey(it), i]))
      const lastNow = list.length ? placeKey(list[list.length - 1]) : ''
      // (None there by its key, the end as it was: by its place from the end,
      // if what is there now is new: one there before is another item, moved.)
      const byPlace = () => {
        const before = new Set(notedList.current.map(placeKey))
        return anchors.current.find((a) => {
          const j = list.length - a.fromEnd
          return a.last === lastNow && j >= 0 && j < list.length && !before.has(placeKey(list[j]))
        })
      }
      const hit = anchors.current.find((a) => at.has(a.key)) ?? byPlace()
      const i = hit ? (at.get(hit.key) ?? list.length - hit.fromEnd) : -1
      const k = hit ? kids[i] : undefined
      if (hit && k) {
        const d = k.getBoundingClientRect().top - view.top - hit.top
        if (Math.abs(d) >= 1) {
          el.scrollTop += d
          did.current.set = el.scrollTop
        }
      } else if (restoring.current) {
        // (Where it was left is not among what is read: its end, followed.)
        stuck.current = true
        setAway(false)
        pin()
      }
    }
    restoring.current = null
    const tu = toUnread.current
    if (tu && Date.now() > tu.until) toUnread.current = null // (too late: what comes now is followed as it comes)
    else if (tu && tu.after === unreadAfter && el && c && unreadAt >= 0) {
      toUnread.current = null
      const line = stuck.current ? c.querySelector<HTMLElement>(':scope > .convo-unread') : null
      if (line && el.scrollHeight - (line.offsetTop - c.offsetTop) > el.clientHeight) {
        stuck.current = false
        setAway(true)
        el.scrollTop += line.getBoundingClientRect().top - el.getBoundingClientRect().top - 8
        did.current.set = el.scrollTop
      }
    }
    note()
    markRead()
  })
  // Before the new content is laid out, note whether the end was in view;
  // after, keep it there.
  useLayoutEffect(() => {
    if (stuck.current) pin()
    markRead()
  }, [list, pending, working, quiet, convo.tasks, !!ask])
  // Following the end is left by any move the page did not make itself:
  // the person's scrolling, or the browser's own jump (finding in the page,
  // a screen reader); a move made by the list laid out anew (a card opened
  // or shut, the keyboard come or gone, what is above read in) puts the end
  // back in view. It is taken up again only by the person's own scrolling
  // to the end (a wheel, a finger, a key, the scroll bar), or the button: a
  // layout that presses the view to the end does not.
  const activeNow = useRef(active)
  activeNow.current = active
  useEffect(() => {
    const el = box.current
    if (!el) return
    const g = did.current
    const SOON = 300
    const atEnd = () => el.scrollHeight - el.scrollTop - el.clientHeight < 48
    const follow = () => {
      stuck.current = true
      g.glide = false
      setAway(false)
    }
    let noting = 0
    const onScroll = () => {
      const now = performance.now()
      const top = el.scrollTop
      const back = top < g.top - 0.5
      const laidOut = el.scrollHeight !== g.h || el.clientHeight !== g.ch
      // (Pressed to the end by the list shrinking: where it was is past
      // where it can be now.)
      const pressed = g.top > el.scrollHeight - el.clientHeight + 0.5
      const ours = Math.abs(top - g.set) < 1
      g.top = top
      g.h = el.scrollHeight
      g.ch = el.clientHeight
      g.pressed = pressed
      if (ours) g.set = -1
      const end = atEnd()
      // (Away from the end while following: not where the list was laid out
      // anew since the last move, that move the layout's, whatever the
      // person did just before.)
      const theirs = !ours && !laidOut
      // (To the end, by the person: a wheel, keys, a finger and the glide of
      // its fling once it is off, the bar; not pressed there.)
      const down = !ours && !pressed && (g.drag || now - g.downAt < SOON || (!g.touch && g.fling < 0 && now - g.touchEndAt < 3000))
      if (!ours) {
        jumpedTo.current = null
        upAfterOlder.current = null
      }
      if (stuck.current) {
        if (end) g.glide = false
        else if (g.glide && !back) {
          // (on its way to the end, as the button asked)
        } else if (theirs) {
          stuck.current = false
          g.glide = false
        } else if (!ours) pin()
      } else if (end && down) follow()
      setAway(!stuck.current && !atEnd())
      if (!noting) {
        noting = requestAnimationFrame(() => {
          noting = 0
          note()
          markRead()
        })
      }
    }
    const onWheel = (e: WheelEvent) => {
      if (e.deltaY > 0) g.downAt = performance.now()
    }
    const onTouchStart = (e: TouchEvent) => {
      g.touch = true
      g.fling = 0
      g.touchY = e.touches[0]?.clientY ?? 0
    }
    const onTouchMove = (e: TouchEvent) => {
      const y = e.touches[0]?.clientY ?? g.touchY
      const dy = y - g.touchY
      if (Math.abs(dy) < 4) return
      g.touchY = y
      g.fling = dy
      if (dy < 0) g.downAt = performance.now()
    }
    const onTouchEnd = () => {
      g.touch = false
      g.touchEndAt = performance.now()
      if (!stuck.current && g.fling < 0 && atEnd() && !g.pressed) follow()
    }
    // (The bar: a press on the box itself, past its content's width.)
    const onPointerDown = (e: PointerEvent) => {
      if (e.target === el && e.offsetX >= el.clientWidth) g.drag = true
    }
    const onPointerUp = () => {
      if (!g.drag) return
      g.drag = false
      if (!stuck.current && atEnd() && !g.pressed) follow()
    }
    const onKey = (e: KeyboardEvent) => {
      if (!activeNow.current || e.ctrlKey || e.metaKey || e.altKey || editable(e.target)) return
      const k = e.key
      if (k === 'PageDown' || k === 'ArrowDown' || k === 'End' || (k === ' ' && !e.shiftKey)) g.downAt = performance.now()
    }
    el.addEventListener('scroll', onScroll, { passive: true })
    el.addEventListener('wheel', onWheel, { passive: true })
    el.addEventListener('touchstart', onTouchStart, { passive: true })
    el.addEventListener('touchmove', onTouchMove, { passive: true })
    el.addEventListener('touchend', onTouchEnd, { passive: true })
    el.addEventListener('touchcancel', onTouchEnd, { passive: true })
    el.addEventListener('pointerdown', onPointerDown)
    window.addEventListener('pointerup', onPointerUp)
    window.addEventListener('pointercancel', onPointerUp)
    window.addEventListener('keydown', onKey)
    // (What grows after it is laid out, as the answer card once the screen is read, keeps the end in view too.)
    const grows = new ResizeObserver(() => {
      if (stuck.current) pin()
      else setAway(!atEnd())
    })
    if (col.current) grows.observe(col.current)
    grows.observe(el)
    return () => {
      el.removeEventListener('scroll', onScroll)
      el.removeEventListener('wheel', onWheel)
      el.removeEventListener('touchstart', onTouchStart)
      el.removeEventListener('touchmove', onTouchMove)
      el.removeEventListener('touchend', onTouchEnd)
      el.removeEventListener('touchcancel', onTouchEnd)
      el.removeEventListener('pointerdown', onPointerDown)
      window.removeEventListener('pointerup', onPointerUp)
      window.removeEventListener('pointercancel', onPointerUp)
      window.removeEventListener('keydown', onKey)
      grows.disconnect()
      cancelAnimationFrame(noting)
    }
  }, [])

  // The person's own messages, up and down from what is in view (of those
  // read in: past the first, the part before is read in, to go on from).
  // (From the one last gone to, while the person has not scrolled since;
  // else from what is in view: going up while following the end, first to
  // the latest one on the screen; reading higher up, the one above.)
  const jumpedTo = useRef<HTMLElement | null>(null)
  // (Going up past what was read in: the first entry then, of that log; any
  // other doing of the person's meanwhile calls it off.)
  const upAfterOlder = useRef<{ seq: number; of?: string; gen: number } | null>(null)
  const jump = useCallback((dir: 1 | -1) => {
    upAfterOlder.current = null
    const el = box.current
    const c = col.current
    if (!el || !c) return
    const kids = [...c.children].filter((k) => !k.hasAttribute('data-aside'))
    const top = el.getBoundingClientRect().top
    const mine = listNow.current.flatMap((it, i) => (it.kind === 'user' && !it.e.mid && kids[i] ? [kids[i] as HTMLElement] : []))
    const at = (k: HTMLElement) => k.getBoundingClientRect().top - top
    const from = jumpedTo.current && jumpedTo.current.isConnected ? mine.indexOf(jumpedTo.current) : -1
    let to: HTMLElement | undefined
    if (from >= 0) to = mine[from + dir]
    // (Following the end: the latest one on the screen; reading higher up: the one above.)
    else if (dir < 0) to = mine.filter((k) => (stuck.current ? at(k) < el.clientHeight - 24 && Math.abs(at(k) - 8) > 4 : at(k) < 4)).pop()
    else to = mine.find((k) => at(k) > 12)
    if (!to) {
      const cv = convoNow.current
      if (dir < 0 && cv.older && !cv.olderCut && !cv.loadingOlder && !olderPausedNow.current) {
        upAfterOlder.current = { seq: cv.entries[0]?.seq ?? 0, of: cv.of, gen: cv.gen }
        cv.loadOlder()
      }
      return
    }
    stuck.current = false
    did.current.glide = false
    setAway(true)
    el.scrollTop += at(to) - 8
    did.current.set = el.scrollTop
    jumpedTo.current = to
    note()
  }, [note])
  // An item gone to from a search: opened where it is a run of steps (and
  // the tool card the hit is in, a summary's text), then, once laid out,
  // brought to the top of the view as jump brings one; false (or gone
  // told, by then), it is not among those read in. A later one, another log
  // in front meanwhile: this one called off.
  const going = useRef(0)
  const goTo = useCallback(
    (hit: { key: string; field: string; tool?: string }, gone: () => void): boolean => {
      const n = ++going.current
      const { of, gen } = convoNow.current
      const i = listNow.current.findIndex((it) => itemKey(it) === hit.key)
      if (i < 0) return false
      if (listNow.current[i].kind === 'work') toggle(hit.key, true)
      if (hit.tool) toolShown.set(hit.tool, { open: true, full: toolShown.get(hit.tool)?.full ?? false })
      // (The person scrolling meanwhile calls it off too.)
      const el0 = box.current
      const off = () => {
        if (n === going.current) going.current++
      }
      const stop = () => ['wheel', 'touchstart', 'pointerdown', 'keydown'].forEach((k) => el0?.removeEventListener(k, off))
      ;['wheel', 'touchstart', 'pointerdown', 'keydown'].forEach((k) => el0?.addEventListener(k, off, { passive: true }))
      setTimeout(stop, 1000)
      const then = (f: () => void) => requestAnimationFrame(() => requestAnimationFrame(() => n === going.current && convoNow.current.of === of && convoNow.current.gen === gen && f()))
      const kidOf = () => {
        const c = col.current
        if (!c) return undefined
        const kids = [...c.children].filter((k) => !k.hasAttribute('data-aside'))
        return kids[listNow.current.findIndex((it) => itemKey(it) === hit.key)] as HTMLElement | undefined
      }
      const place = (to: HTMLElement) => {
        const el = box.current
        if (!el) return
        stuck.current = false
        did.current.glide = false
        setAway(true)
        el.scrollTop += to.getBoundingClientRect().top - el.getBoundingClientRect().top - 8
        did.current.set = el.scrollTop
        jumpedTo.current = null
        note()
        to.classList.remove('convo-found')
        void to.offsetWidth
        to.classList.add('convo-found')
      }
      const cardIn = (to: HTMLElement) => (hit.tool ? to.querySelector<HTMLElement>(`[data-tool="${CSS.escape(hit.tool)}"]`) : null)
      then(() => {
        const to = kidOf()
        if (!to) return gone()
        // (A card already shown keeps its own state: opened here.)
        cardIn(to)?.querySelector<HTMLElement>(':scope > .convo-step-head[aria-expanded="false"]')?.click()
        if (hit.field === 'summary') {
          if (to instanceof HTMLDetailsElement) to.open = true
          to.querySelectorAll('details:not([open])').forEach((d) => d.setAttribute('open', ''))
        }
        then(() => {
          // (Found again: gone meanwhile, or drawn anew.)
          const now = kidOf()
          if (!now) return gone()
          place(cardIn(now) ?? now)
        })
      })
      return true
    },
    [note, toggle],
  )
  // (The part before read in, with something in it: on up, once.)
  useEffect(() => {
    const was = upAfterOlder.current
    if (was === null) return
    // (Another log in front meanwhile: called off, whatever is still read in.)
    if (was.of !== convo.of || was.gen !== convo.gen) {
      upAfterOlder.current = null
      return
    }
    if (convo.loadingOlder) return
    upAfterOlder.current = null
    if (firstSeq < was.seq) jump(-1)
  }, [convo.loadingOlder, firstSeq, jump, convo.of, convo.gen])
  useEffect(() => {
    if (!active) return
    const onKey = (e: KeyboardEvent) => {
      if ((e.key !== '[' && e.key !== ']') || e.ctrlKey || e.metaKey || e.altKey || e.isComposing || editable(e.target) || underDialog(e) || subsNow.current) return
      e.preventDefault()
      jump(e.key === '[' ? -1 : 1)
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [active, jump])
  // ⌘K on a Mac where the agents are listed: their search. Elsewhere "/",
  // not while typing (the browsers there keep Ctrl+K for their own search).
  useEffect(() => {
    if (!active || !side) return
    const mac = /Mac|iPhone|iPad/.test(navigator.platform)
    const onKey = (e: KeyboardEvent) => {
      if (e.isComposing || underDialog(e) || e.altKey) return
      // (Shift as some layouts need it for "/", as the German's.)
      const asked = mac ? e.key.toLowerCase() === 'k' && e.metaKey && !e.ctrlKey && !e.shiftKey : e.key === '/' && !e.ctrlKey && !e.metaKey && !editable(e.target)
      if (!asked) return
      e.preventDefault()
      if (sideShownNow.current) setSearchAsked((n) => n + 1)
      else setPicking('search')
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [active, side])
  // Ctrl/⌘+F with the conversation in front (the focus in it, or nowhere):
  // its own search, which knows what is folded away; elsewhere (the
  // composer, a dialog) the browser's.
  useEffect(() => {
    if (!active || !more) return
    const onKey = (e: KeyboardEvent) => {
      if (e.key.toLowerCase() !== 'f' || !(e.ctrlKey || e.metaKey) || e.altKey || e.shiftKey || e.isComposing || underDialog(e)) return
      // (In the thread itself, or nowhere: not in a panel beside it, a subagent's say.)
      const at = document.activeElement
      if (editable(e.target) || (at && at !== document.body && !box.current?.contains(at))) return
      e.preventDefault()
      setSearching(true)
      requestAnimationFrame(() => root.current?.querySelector<HTMLInputElement>('.convo-search-input')?.focus())
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [active, more])

  // What was sent, not in the log yet (also before a new conversation's log is written).
  const pendingList = pending.map((p) => (
    <div key={p.id} className={'convo-user convo-pending' + (p.state === 'sending' ? ' is-sending' : '')}>
      <div className="convo-user-text">{p.text}</div>
      <div className="convo-meta">
        <span role="status">{t(p.state === 'sending' ? 'convo.sending' : p.state === 'waiting' ? 'convo.sent' : p.state === 'unseen' ? 'convo.unseen' : 'convo.sentOnly')}</span>
        {(p.state === 'unseen' || p.state === 'sent') && (
          <button type="button" className="link-btn" onClick={() => onDismiss(p.id)}>{t('common.close')}</button>
        )}
      </div>
      {p.state === 'unseen' && <Missing look={p.look} onEnter={onEnter && (() => onEnter(p.id))} />}
    </div>
  ))
  let body: ReactNode
  if (convo.status === 'loading') body = <p className="convo-state">{t('convo.loading')}</p>
  else if (convo.status === 'error') body = <p className="convo-state">{t('convo.error')}</p>
  else if (convo.status === 'none')
    body = (
      <div className="convo-state">
        <p>
          {convo.reason === 'not_told'
            ? t('convo.notTold')
            : convo.reason === 'no_log'
              ? t('convo.noLog')
              : convo.reason === 'unsupported'
                ? t('convo.unsupported')
                : convo.reason === 'no_reader'
                  ? t('convo.noReader')
                : agent?.other
                  ? t('convo.otherAgent', { agent: agent.other })
                  : t('convo.noAgent')}
        </p>
        <button type="button" className="btn btn-ghost" onClick={onShowTerminal}>{t('convo.showTerminal')}</button>
        {convo.reason === 'no_log' && pending.length > 0 && <div className="convo-state-pending">{pendingList}</div>}
      </div>
    )
  else
    body = (
      <>
        {convo.older ? (
          // (Near the top, the part before is read in: see the observer.)
          <p className="convo-older" data-aside="" ref={olderRef} role="status">
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
          convo.entries.length > 0 && (
            <p className="convo-older" data-aside="">
              {t('convo.atStart')}
            </p>
          )
        )}
        {list.map((it, i) => (
          <Fragment key={itemKey(it)}>
            {i === unreadAt && (
              <p className="convo-unread" data-aside="" role="separator">
                <span>{t('convo.unread')}</span>
              </p>
            )}
            <ListItem it={it} copy={copy} open={it.kind === 'work' ? (opened[it.key] ?? allOpen) : false} onToggle={toggle} onShow={show} />
          </Fragment>
        ))}
        {pendingList}
        {working && (quiet || quietLong > 0) && <p className="convo-working">{quietLong ? t('convo.quietLong', { d: quietLong < 3600000 ? t('convo.for.m', { n: Math.floor(quietLong / 60000) }) : t('convo.for.h', { n: Math.floor(quietLong / 3600000) }) }) : t('convo.quiet')}</p>}
      </>
    )

  let status = agent ? t(`convo.status.${agent.status}` as I18nKey) : ''
  if (working && lastUser && now >= lastUser) status = t('convo.workingFor', { d: duration(now - lastUser) })
  if (agent?.bg) status = t(agent.bg === 'running' ? 'convo.status.bg' : agent.bg === 'stale' ? 'convo.status.bgStale' : 'convo.status.working')
  const name = agent ? [agent.title, agent.where].filter(Boolean) : []
  // What is read in, as a Markdown file to keep (see convoExport).
  const exporting = useRef<() => void>(() => {})
  const paren = (x: string) => (getLang() === 'zh-CN' ? '（' + x + '）' : '(' + x + ')')
  const doExport = () => {
    exporting.current()
    const title = name[0] || t('convo.title')
    const when = (ts?: string | number) => (ts ? new Date(ts).toLocaleString(getLang()) : '')
    exporting.current = exportMarkdown(
      list,
      {
        title,
        exported: when(Date.now()),
        from: convo.older ? when(convo.entries[0]?.ts) || '?' : undefined,
        you: t('convoExport.you'),
        agent: t('convoExport.agent'),
        summary: t('convoExport.summary'),
        note: t('convoExport.note'),
        thinking: t('convoExport.thinking'),
        cut: t('convoExport.cut'),
        image: t('convoExport.image'),
        gone: t('convoExport.gone'),
        tool: (n, what, state) => t('convoExport.tool', { name: n, what: what ? (getLang() === 'zh-CN' ? '：' : ': ') + what : '', state: t(state === 'running' ? 'convoExport.running' : state === 'failed' ? 'convoExport.failed' : 'convoExport.ok') }),
        event: (p) => {
          const ev = p.event
          if (!ev) return ''
          const said = [ev.text, ev.goal, ev.why].filter(Boolean).join(' · ')
          return paren(t(('convoExport.ev.' + ev.kind) as Parameters<typeof t>[0])) + (said ? ' ' + said : '')
        },
        trouble: (p) => {
          const tr = p.trouble
          if (!tr) return ''
          return paren(t(('convoExport.tr.' + tr.kind) as Parameters<typeof t>[0])) + [tr.name, tr.text].filter(Boolean).map((x) => ' ' + x).join('')
        },
        head: (tt, ex, from) => `# ${tt}\n\n${t('convoExport.head', { at: ex })}${from ? t('convoExport.from', { from }) : t('convoExport.whole')}`,
      },
      (md) => {
        const a = document.createElement('a')
        a.href = URL.createObjectURL(md)
        a.download = (title.replace(/[\\/:*?"<>|\s]+/g, '-').slice(0, 60) || 'conversation') + '-' + new Date().toISOString().slice(0, 10) + '.md'
        document.body.appendChild(a)
        a.click()
        a.remove()
        setTimeout(() => URL.revokeObjectURL(a.href), 10000)
      },
    )
  }
  const rows = side?.rows ?? []
  const others = rows.filter((r) => r.status === 'blocked' && !r.current)
  const label = (
    <>
      {agent && <AgentIcon program={agent.program} size={22} />}
      <span className="convo-bar-name">{name[0] || t('convo.title')}</span>
      {name[1] && <span className="convo-bar-where">{name[1]}</span>}
      {side && !sideShown && <ChevronDown size={14} className="convo-bar-chev" aria-hidden="true" />}
    </>
  )
  return (
    <div className="convo" role="region" aria-label={t('convo.title')} ref={root}>
      {sideShown && (
        <nav className="convo-side" aria-label={t('convo.agents')} style={{ width: sideWidth }} ref={sideEl}>
          <NewAgentRow />
          <AgentList rows={rows} query={sideQuery} onQuery={setSideQuery} search={searchAsked} onSearchLeft={() => setSearchAsked(0)} hint={!isTouchOnly} />
          {!isTouchOnly && <p className="convo-side-keys">{t('convo.switchKeys')}<br />{t(allOpen ? 'convo.foldKeys' : 'convo.openKeys')}</p>}
        </nav>
      )}
      {sideShown && (
        <div
          className="convo-side-grip"
          role="separator"
          aria-orientation="vertical"
          aria-label={t('convo.sideWidth')}
          title={t('convo.sideWidth')}
          aria-valuenow={sideWidth}
          aria-valuemin={SIDE_W_MIN}
          aria-valuemax={sideWidthMax(viewW || SIDE_MIN)}
          tabIndex={0}
          onPointerDown={(e) => {
            if (e.button !== 0 || drag.current) return // (one pointer at a time)
            e.preventDefault()
            e.currentTarget.setPointerCapture(e.pointerId)
            drag.current = { id: e.pointerId, x: e.clientX, w: sideWidth, to: sideWidth, frame: 0 }
          }}
          onPointerMove={(e) => {
            const d = drag.current
            if (!d || e.pointerId !== d.id) return
            if (e.pointerType === 'mouse' && e.buttons === 0) return endDrag(true) // (let go where no word of it came: outside the window)
            d.to = sideWidthOf(d.w + e.clientX - d.x, viewW || SIDE_MIN)
            cancelAnimationFrame(d.frame)
            d.frame = requestAnimationFrame(() => {
              if (sideEl.current) sideEl.current.style.width = d.to + 'px'
            })
          }}
          onPointerUp={(e) => {
            if (drag.current?.id === e.pointerId) endDrag(true)
          }}
          onPointerCancel={(e) => {
            if (drag.current?.id === e.pointerId) endDrag(false)
          }}
          onLostPointerCapture={(e) => {
            if (drag.current?.id === e.pointerId) endDrag(true)
          }}
          onDoubleClick={() => keepSideW(SIDE_W)}
          onKeyDown={(e) => {
            const step = e.shiftKey ? 48 : 16
            if (e.key === 'ArrowLeft') keepSideW(sideWidth - step)
            else if (e.key === 'ArrowRight') keepSideW(sideWidth + step)
            else if (e.key === 'Home') keepSideW(SIDE_W_MIN)
            else if (e.key === 'End') keepSideW(SIDE_W_MAX)
            else return
            e.preventDefault()
          }}
        />
      )}
      <div className="convo-main">
      {(name.length > 0 || status || convo.failing || side || lead) && (
        <div className="convo-bar">
          {lead}
          {side && wide && (
            <button
              type="button"
              className="icon-btn convo-side-btn"
              aria-pressed={sideShown}
              onClick={() => setFolded(!folded)}
              aria-label={t(sideShown ? 'convo.foldSide' : 'convo.openSide')}
              title={t(sideShown ? 'convo.foldSide' : 'convo.openSide')}
            >
              <PanelLeft size={17} aria-hidden="true" />
            </button>
          )}
          {side && !sideShown ? (
            <button type="button" className="convo-bar-pick" aria-haspopup="dialog" aria-expanded={!!picking} aria-label={t('convo.pick')} onClick={() => setPicking(true)}>
              {label}
            </button>
          ) : (
            <span className="convo-bar-pick is-static">{label}</span>
          )}
          <span className={'convo-bar-status' + (agent && !convo.failing && !stop ? ' is-' + (agent.bg === 'running' || agent.bg === 'checking' ? 'working' : agent.status) : '')}>
            {working && !convo.failing && !stop && <span className="convo-spin" aria-hidden="true" />}
            {convo.failing ? t('convo.paused', { time: clockMs(convo.okAt) }) : stop || status}
          </span>
          {onStop && (
            <button type="button" className="btn btn-ghost convo-stop" onClick={onStop}>
              {t('convo.stop')}
            </button>
          )}
          {more && (
            <Menu
              label={t('common.more')}
              items={() => [
                { label: t('convoSearch.item'), onSelect: () => setSearching(true) },
                { label: t('convoExport.item'), disabled: !list.length, onSelect: () => doExport() },
                ...more().map((m, i) => (m && i === 0 ? { ...m, separator: true } : m)),
              ]}
              button={(open, toggle) => (
                <button type="button" className="icon-btn convo-bar-more" aria-expanded={open} onClick={toggle} aria-label={t('common.more')} title={t('common.more')}>
                  <MoreVertical size={18} />
                </button>
              )}
            />
          )}
        </div>
      )}
      {searching && (
        <ConvoSearchPanel
          list={list}
          of={(convo.of ?? '') + ':' + convo.gen}
          onGo={goTo}
          older={convo.older && !convo.olderCut ? { can: true, busy: convo.loadingOlder } : undefined}
          onOlder={() => convo.loadOlder()}
          onClose={() => setSearching(false)}
        />
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
            <ShowTerminal.Provider value={onShowTerminal}>
              <OpenSub.Provider value={openSub}>{body}</OpenSub.Provider>
            </ShowTerminal.Provider>
          </div>
        </div>
        {away && convo.status === 'ready' && (
          <div className="convo-jump" role="group" aria-label={t('convo.jump')}>
            <button type="button" className="icon-btn" onClick={() => jump(-1)} aria-label={t('convo.jumpPrev')} title={t('convo.jumpPrev') + (isTouchOnly ? '' : ' ([)')}>
              <ChevronUp size={16} aria-hidden="true" />
            </button>
            <button type="button" className="icon-btn" onClick={() => jump(1)} aria-label={t('convo.jumpNext')} title={t('convo.jumpNext') + (isTouchOnly ? '' : ' (])')}>
              <ChevronDown size={16} aria-hidden="true" />
            </button>
          </div>
        )}
        {away && convo.status === 'ready' && (
          <button
            type="button"
            className="convo-latest"
            onClick={() => {
              const el = box.current
              if (!el) return
              // (Following again: the end is glided to, not jumped to, on the way.)
              upAfterOlder.current = null
              stuck.current = true
              did.current.glide = true
              setAway(false)
              el.scrollTo({ top: el.scrollHeight, behavior: matchMedia('(prefers-reduced-motion: reduce)').matches ? 'auto' : 'smooth' })
            }}
          >
            <ArrowDown size={15} aria-hidden="true" />
            {t('convo.latest')}
          </button>
        )}
      </div>
      <div className="convo-dock">
        {convo.status === 'ready' && convo.running && <RunningWork items={convo.running} openSub={!convo.bind?.agent || convo.bind.agent === 'claude' ? openSub : undefined} />}
        {convo.status === 'ready' && convo.tasks && <Tasks tasks={convo.tasks} fold={!!ask} />}
        {/* (Asked from the screen: also where its log is not read.) */}
        {(convo.status === 'ready' || convo.status === 'none') && ask && <AskDock>{ask}</AskDock>}
        {/* (Not of one at rest, which would have taken them in: not when its state is not known.) */}
        {convo.status === 'ready' && agent?.status !== 'idle' && agent?.status !== 'done' && convo.queued && <Queue items={convo.queued} onSendNow={working ? onSendNow : undefined} said={sendNow} />}
        {waits && !ask && convo.status === 'ready' && (
          // (Where it cannot be answered here: in its terminal.)
          <div className="convo-waiting" role="status">
            <span>{t('convo.waiting')}</span>
            <button type="button" className="btn btn-primary" onClick={onReply ?? onShowTerminal}>{t(onReply ? 'convo.answer' : 'convo.answerThere')}</button>
          </div>
        )}
        {composer}
        {convo.status === 'ready' && convo.meta && <Runs meta={convo.meta} active={active} mode={mode} model={model} effort={effort} />}
      </div>
      </div>
      {changes && shownEdits && <ChangesPanel key={changes.key} edits={shownEdits} focus={changes.path} onClose={unshow} active={active} />}
      {subs && subs.length > 0 && convo.of && (
        <SubPanel
          key={subs[subs.length - 1].agent}
          of={convo.of}
          sub={subs[subs.length - 1]}
          depth={subs.length}
          live={(a) => !!convo.running?.some((r) => r.kind === 'agent' && r.id === a)}
          onOpen={(s) => setSubs((cur) => [...(cur ?? []), s])}
          onBack={() => setSubs((cur) => (cur && cur.length > 1 ? cur.slice(0, -1) : null))}
          onClose={() => setSubs(null)}
          active={active}
          copy={copy}
        />
      )}
      {picking && side && <Picker rows={rows} onClose={unpick} searchFirst={picking === 'search'} />}
    </div>
  )
}

// What the person sent while the agent works, not taken in yet: over the
// composer, with Claude Code's "send now" (which stops the step under way).
// What the agent set going and left to itself: a command in the
// background, a subagent on its own. It stays here until the agent is told
// it has ended.
// The question over the composer, scrolled in itself where the screen is
// short (a small phone): where more of it is below, its foot fades out, so
// that it is not taken for cut off (not while it is being typed in: what
// is typed, at its foot with the keyboard up, is not faded).
function AskDock({ children }: { children: ReactNode }) {
  const ref = useRef<HTMLDivElement>(null)
  const [more, setMore] = useState(false)
  useEffect(() => {
    const el = ref.current
    if (!el) return
    const look = () => setMore(el.scrollHeight - el.scrollTop - el.clientHeight > 2)
    look()
    el.addEventListener('scroll', look, { passive: true })
    const ro = new ResizeObserver(look)
    // (What is in it is watched as it grows; a question answered gives way
    // to another, then that one is.)
    const watch = () => {
      ro.disconnect()
      ro.observe(el)
      for (const c of el.children) ro.observe(c)
      look()
    }
    watch()
    const mo = new MutationObserver(watch)
    mo.observe(el, { childList: true })
    return () => {
      el.removeEventListener('scroll', look)
      ro.disconnect()
      mo.disconnect()
    }
  }, [])
  return (
    <div className={'convo-dock-ask' + (more ? ' has-more' : '')} ref={ref}>
      {children}
    </div>
  )
}

function RunningWork({ items, openSub }: { items: ConvoRunning[]; openSub?: (s: Sub) => void }) {
  const [open, setOpen] = useState(false)
  // (Down to one: opened again later, it starts folded as it did at first.)
  useEffect(() => {
    if (items.length < 2) setOpen(false)
  }, [items.length])
  const shown = open ? items : items.slice(0, 1)
  return (
    <div className="convo-running" role="status">
      <Loader size={13} className="convo-running-spin" aria-hidden="true" />
      <div className="convo-running-body">
      <div className="convo-running-head">
        <span className="convo-running-sum">{t('convo.bg', { n: items.length })}</span>
        {items.length > 1 && (
          <button type="button" className="link-btn convo-running-more" aria-expanded={open} onClick={() => setOpen(!open)}>
            {t(open ? 'convo.bgLess' : 'convo.bgAll', { n: items.length })}
          </button>
        )}
      </div>
      {shown.map((r) => (
        <div key={r.id} className="convo-running-row">
          {r.kind === 'agent' && openSub && isSubId(r.id) ? (
            <button type="button" className="link-btn convo-running-what" title={t('sub.open')} onClick={() => openSub({ agent: r.id, title: r.what })}>
              {r.what || t('convo.bgAgent')}
            </button>
          ) : (
            <span className="convo-running-what" title={r.what}>{r.what || t(r.kind === 'agent' ? 'convo.bgAgent' : 'convo.bgCmd')}</span>
          )}
          {r.note && <span className="convo-running-note" title={r.note}>{r.note}</span>}
          <span className="convo-running-since">{since(r.ts)}</span>
        </div>
      ))}
      </div>
    </div>
  )
}

// How long ago, as the view says it elsewhere ("6 分 25 秒").
function since(ts?: string): string {
  const v = ts ? Date.parse(ts) : NaN
  if (!Number.isFinite(v)) return ''
  return duration(Math.max(Date.now() - v, 0))
}

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
        <div key={i} className={'convo-queue-row' + (q.note ? ' is-note' : '')}>
          {q.note && <span className="convo-queue-from">{q.from ? t('convo.queuedFrom', { name: q.from }) : t('convo.queuedNotice')}</span>}
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

// A key for a dialog open over the page (or already taken): not the page's.
const underDialog = (e: KeyboardEvent) => e.defaultPrevented || !!document.querySelector('[aria-modal="true"]')

// The windows a conversation may fill, where which one is not known (a
// model the gateway does not know): how full it is is said of both.
const WINDOWS = [200_000, 1_000_000]

function tokens(n: number): string {
  if (n >= 1_000_000) return (Math.round(n / 100_000) / 10).toString() + 'M'
  if (n >= 1000) return (Math.round(n / 100) / 10).toString() + 'K'
  return String(n)
}

// (Claude Code's permission modes; Codex's approval policies.)
const MODES = ['default', 'acceptEdits', 'plan', 'auto', 'bypassPermissions', 'dontAsk', 'untrusted', 'on-failure', 'on-request', 'never', 'unknown'] as const
const modeName = (m: string) => ((MODES as readonly string[]).includes(m) ? t(`convo.mode.${m}` as I18nKey) : m)

// Switching the permission mode from the page, to one picked from a menu
// (Shift+Tab until it is there, on the host): now, what the last switch went
// to (the log says it only with the next message); busy, one on its way.
export type ModeSwitch = { now?: string; busy: boolean; onPick?: (mode: string) => void }
// (Those Shift+Tab goes through, in its order; which a session has depends on how it was started.)
const PICKED_MODES = ['default', 'acceptEdits', 'plan', 'auto', 'bypassPermissions'] as const

// Switching the model, from a menu of what Claude Code's picker offers
// (list: null while it is read), for the session only.
export type ModelOpt = { label: string; desc?: string; current?: boolean }
// (fresh: the list read a moment ago, its "current" to go by; can: it may be switched now.)
export type ModelSwitch = { list: ModelOpt[] | null; fresh: boolean; busy: boolean; can: boolean; onOpen: () => void; onPick: (label: string) => void }

// How hard the agent is asked to think (Claude Code's /effort), for the
// session only. The levels are its slider's, in its order.
export const EFFORTS = ['low', 'medium', 'high', 'xhigh', 'max'] as const
// (now: what a switch took, until the log says it — an answer names the
// level, and none comes of the switch itself.)
export type EffortSwitch = { now?: string; busy: boolean; can: boolean; onPick: (level: string) => void }
const effortName = (e: string) => ((EFFORTS as readonly string[]).includes(e) ? t(`convo.effort.${e}` as I18nKey) : e)

// What the session runs with, under the composer: its permission mode on
// the left (a tap: a menu of them); its branch, model and how
// full its context is on the right (the numbers on a tap).
function Runs({ meta, active, mode, model, effort }: { meta: ConvoMeta; active: boolean; mode?: ModeSwitch; model?: ModelSwitch; effort?: EffortSwitch }) {
  const [open, setOpen] = useState<false | 'ctx' | 'model' | 'effort' | 'mode'>(false) // (which of its pop-ups)
  const box = useRef<HTMLDivElement>(null)
  const modelBtn = useRef<HTMLButtonElement>(null)
  const effortBtn = useRef<HTMLButtonElement>(null)
  const modeBtn = useRef<HTMLButtonElement>(null)
  if (open && !active) setOpen(false) // (behind a terminal tab: its Escape is the terminal's)
  if (open === 'model' && !model?.can) setOpen(false) // (it works now: no switching)
  if (open === 'effort' && !effort?.can) setOpen(false)
  if (open === 'mode' && !mode?.onPick) setOpen(false)
  // The model menu: its current one (else its first) takes the focus once
  // read; Up and Down go through it; closed by Escape, back to its button.
  const menu = useRef<HTMLDivElement>(null)
  const listed = (open === 'model' && !!model?.list) || open === 'effort' || open === 'mode'
  useEffect(() => {
    // (Only where the focus still is with it: not taken back from where it went meanwhile.)
    if (!listed || !box.current?.contains(document.activeElement)) return
    ;(menu.current?.querySelector<HTMLElement>('[aria-checked="true"]') ?? menu.current?.querySelector<HTMLElement>('button'))?.focus()
  }, [open, listed])
  useEffect(() => {
    if (!open) return
    const away = (e: PointerEvent) => {
      if (!box.current?.contains(e.target as Node)) setOpen(false)
    }
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') {
        e.preventDefault()
        e.stopPropagation()
        if (open === 'model' && box.current?.contains(document.activeElement)) modelBtn.current?.focus()
        if (open === 'effort' && box.current?.contains(document.activeElement)) effortBtn.current?.focus()
        if (open === 'mode' && box.current?.contains(document.activeElement)) modeBtn.current?.focus()
        setOpen(false)
      } else if ((open === 'model' || open === 'effort' || open === 'mode') && (e.key === 'ArrowDown' || e.key === 'ArrowUp') && menu.current?.contains(document.activeElement)) {
        const all = [...menu.current.querySelectorAll<HTMLElement>('button')]
        const at = all.indexOf(document.activeElement as HTMLElement)
        e.preventDefault()
        all[(at + (e.key === 'ArrowDown' ? 1 : all.length - 1)) % all.length]?.focus()
      }
    }
    document.addEventListener('pointerdown', away, true)
    document.addEventListener('keydown', onKey, true)
    return () => {
      document.removeEventListener('pointerdown', away, true)
      document.removeEventListener('keydown', onKey, true)
    }
  }, [open])
  // (A pop-up open: no taller than the room above the line in what can be
  // seen, the keyboard up too — a phone's view shrinks then, not its page.)
  useEffect(() => {
    if (!open) return
    const vv = window.visualViewport
    const fit = () => {
      const pop = box.current?.querySelector<HTMLElement>('.convo-runs-pop')
      if (!pop || !box.current) return
      const top = box.current.getBoundingClientRect().top - (vv ? vv.offsetTop : 0)
      pop.style.maxHeight = Math.max(0, Math.min(top, vv ? vv.height : window.innerHeight) - 16) + 'px'
    }
    fit()
    vv?.addEventListener('resize', fit)
    vv?.addEventListener('scroll', fit)
    window.addEventListener('resize', fit)
    return () => {
      vv?.removeEventListener('resize', fit)
      vv?.removeEventListener('scroll', fit)
      window.removeEventListener('resize', fit)
    }
  }, [open])
  const used = meta.context ?? 0
  // (Where it is not known: past the smaller window, the session has the
  // larger one. Known and filled past, it was not that one after all.)
  const known = meta.window && used <= meta.window ? meta.window : 0
  const windows = known ? [known] : WINDOWS.filter((w) => w >= used || w === WINDOWS[WINDOWS.length - 1])
  // (The gauge: of its window, else of the smaller it may be.)
  const full = Math.min(1, used / windows[0])
  return (
    // (The focus gone elsewhere, as by Tab, while the model menu is open, the list read or not: closed.)
    <div className="convo-runs" ref={box} onBlur={(e) => (open === 'model' || open === 'effort' || open === 'mode') && !box.current?.contains(e.relatedTarget as Node | null) && setOpen(false)}>
      {(mode?.now || meta.mode) &&
        (mode?.onPick || mode?.busy ? (
          <button
            ref={modeBtn}
            type="button"
            className="convo-runs-item convo-runs-mode is-switch"
            aria-expanded={open === 'mode'}
            aria-haspopup="menu"
            aria-disabled={mode.busy || undefined}
            aria-busy={mode.busy || undefined}
            title={t('convo.modeSwitch')}
            onClick={() => !mode.busy && setOpen(open === 'mode' ? false : 'mode')} // (not disabled while busy: the focus it has stays)
          >
            {mode.busy ? <span className="convo-spin" aria-hidden="true" /> : null}
            {mode.busy ? t('convo.modeSwitching') : modeName(mode.now || meta.mode!)}
            {!mode.busy && <ChevronsUpDown size={12} aria-hidden="true" />}
          </button>
        ) : (
          <span className="convo-runs-item convo-runs-mode" title={t('convo.modeHint')}>
            {modeName(mode?.now || meta.mode!)}
          </span>
        ))}
      <span className="spacer" />
      {meta.branch && (
        <span className="convo-runs-item convo-runs-branch" title={t('convo.branch', { b: meta.branch })}>
          <GitBranch size={13} aria-hidden="true" />
          <span className="convo-runs-branch-name">{meta.branch}</span>
        </span>
      )}
      {meta.model &&
        (model ? (
          <button
            ref={modelBtn}
            type="button"
            className="convo-runs-item convo-runs-model is-switch"
            aria-expanded={open === 'model'}
            aria-haspopup="menu"
            disabled={model.busy}
            aria-busy={model.busy || undefined}
            title={t('convo.modelSwitch')}
            onClick={() => {
              if (open !== 'model') model.onOpen()
              setOpen(open === 'model' ? false : 'model')
            }}
          >
            {modelName(meta.model)}
            <ChevronsUpDown size={12} aria-hidden="true" />
          </button>
        ) : (
          <span className="convo-runs-item convo-runs-model" title={meta.model}>{modelName(meta.model)}</span>
        ))}
      {meta.effort &&
        (() => {
          const level = effort?.now || meta.effort! // (what the switch took, else what the log says)
          return effort?.can || effort?.busy ? (
          <button
            ref={effortBtn}
            type="button"
            className="convo-runs-item convo-runs-effort is-switch"
            aria-expanded={open === 'effort'}
            aria-haspopup="menu"
            aria-label={t('convo.effort') + ': ' + effortName(level)}
            disabled={effort.busy}
            aria-busy={effort.busy || undefined}
            title={t('convo.effortSwitch')}
            onClick={() => setOpen(open === 'effort' ? false : 'effort')}
          >
            {effortName(level)}
            <ChevronsUpDown size={12} aria-hidden="true" />
          </button>
          ) : (
            <span className="convo-runs-item convo-runs-effort" title={t('convo.effort') + ': ' + effortName(level)}>{effortName(level)}</span>
          )
        })()}
      {used > 0 && (
        <button type="button" className="convo-runs-item convo-runs-ctx" aria-expanded={open === 'ctx'} aria-label={t('convo.ctxLabel', { n: tokens(used) })} title={t('convo.ctxLabel', { n: tokens(used) })} onClick={() => setOpen(open === 'ctx' ? false : 'ctx')}>
          <Gauge part={full} />
          {tokens(used)}
        </button>
      )}
      {open === 'model' && model && (
        <div
          className="convo-runs-pop convo-models"
          role="menu"
          aria-label={t('convo.model')}
          ref={menu}
          onBlur={(e) => !box.current?.contains(e.relatedTarget as Node | null) && setOpen(false)} // (Tab out of it: closed)
        >
          {model.list === null ? (
            <p className="convo-runs-note" role="status">{t('convo.modelReading')}</p>
          ) : (
            model.list.map((o) => {
              // (As its picker last said, which the log may not yet.)
              const now = model.fresh && model.list!.some((x) => x.current) ? !!o.current : modelName(meta.model!) === o.label
              return (
                <button
                  key={o.label}
                  type="button"
                  role="menuitemradio"
                  aria-checked={now}
                  className="convo-models-item"
                  onClick={() => {
                    setOpen(false)
                    modelBtn.current?.focus()
                    model.onPick(o.label) // (the one ticked too: what is ticked may be past)
                  }}
                >
                  <span className="convo-models-label">{o.label}</span>
                  {o.desc && <span className="convo-models-desc">{o.desc}</span>}
                  {now && <Check size={14} aria-hidden="true" />}
                </button>
              )
            })
          )}
          <p className="convo-runs-note">{t('convo.modelOnly')}</p>
        </div>
      )}
      {open === 'mode' && mode?.onPick && (
        <div
          className="convo-runs-pop convo-models convo-modes"
          role="menu"
          aria-label={t('convo.modeTitle')}
          ref={menu}
          onBlur={(e) => !box.current?.contains(e.relatedTarget as Node | null) && setOpen(false)} // (Tab out of it: closed)
        >
          {PICKED_MODES.map((m) => {
            const now = (mode.now || meta.mode) === m
            return (
              <button
                key={m}
                type="button"
                role="menuitemradio"
                aria-checked={now}
                className="convo-models-item"
                onClick={() => {
                  setOpen(false)
                  modeBtn.current?.focus()
                  mode.onPick!(m) // (the one ticked too: what is ticked may be past; there already, nothing is pressed)
                }}
              >
                <span className="convo-models-label">{modeName(m)}</span>
                {now && <Check size={14} aria-hidden="true" />}
              </button>
            )
          })}
          <p className="convo-runs-note">{t('convo.modeNote')}</p>
        </div>
      )}
      {open === 'effort' && effort && (
        <div
          className="convo-runs-pop convo-models"
          role="menu"
          aria-label={t('convo.effort')}
          ref={menu}
          onBlur={(e) => !box.current?.contains(e.relatedTarget as Node | null) && setOpen(false)} // (Tab out of it: closed)
        >
          {EFFORTS.map((e) => (
            <button
              key={e}
              type="button"
              role="menuitemradio"
              aria-checked={(effort.now || meta.effort) === e}
              className="convo-models-item"
              onClick={() => {
                setOpen(false)
                effortBtn.current?.focus()
                effort.onPick(e)
              }}
            >
              <span className="convo-models-label">{effortName(e)}</span>
              {(effort.now || meta.effort) === e && <Check size={14} aria-hidden="true" />}
            </button>
          ))}
          <p className="convo-runs-note">{t('convo.effortOnly')}</p>
        </div>
      )}
      {open === 'ctx' && (
        <div className="convo-runs-pop" role="dialog" aria-label={t('convo.ctxTitle')}>
          <div className="convo-runs-row">
            <span>{t('convo.ctxTitle')}</span>
            <span>{t('convo.ctxTokens', { n: used.toLocaleString(getLang()) })}</span>
          </div>
          {windows.map((w) => {
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
          <p className="convo-runs-note">
            {known
              ? t(meta.window_by === 'model' ? 'convo.ctxNoteModel' : meta.window_by === 'request' ? 'convo.ctxNoteWide' : 'convo.ctxNoteSaid', { w: tokens(known) })
              : t(windows.length > 1 ? 'convo.ctxNote' : 'convo.ctxNoteBig')}
          </p>
          {(meta.cost || /claude/i.test(meta.model ?? '')) && <CostPart cost={meta.cost} />}
        </div>
      )}
    </div>
  )
}

// What the session used, as Claude Code's log last said it (cost-state):
// its tokens by model and its estimated cost, with when that was said.
function CostPart({ cost }: { cost?: ConvoCost }) {
  if (!cost) return <p className="convo-runs-note convo-cost-none">{t('convo.costNone')}</p>
  const d = cost.at ? new Date(cost.at) : null
  const at = !d ? '' : (d.toDateString() === new Date().toDateString() ? '' : d.toLocaleDateString(getLang(), { month: 'numeric', day: 'numeric' }) + ' ') + d.toLocaleTimeString(getLang(), { hour: '2-digit', minute: '2-digit' })
  const money = (n: number) => '$' + (n < 10 ? n.toFixed(2) : n.toFixed(1))
  return (
    <div className="convo-cost">
      <div className="convo-runs-row convo-cost-head">
        <span>{at ? t('convo.costTitleAt', { at }) : t('convo.costTitle')}</span>
        <span>{money(cost.usd)}</span>
      </div>
      {(cost.models ?? []).map((m) => (
        <div key={m.model} className="convo-cost-model">
          <div className="convo-runs-row">
            <span className="convo-cost-name">{modelName(m.model)}</span>
            <span>{money(m.usd ?? 0)}</span>
          </div>
          <div className="convo-cost-tokens">
            {t('convo.costTokens', { i: tokens(m.in ?? 0), o: tokens(m.out ?? 0), r: tokens(m.cache_read ?? 0), w: tokens(m.cache_write ?? 0) })}
          </div>
        </div>
      ))}
      {(!!cost.added || !!cost.removed) && (
        <div className="convo-runs-row">
          <span>{t('convo.costLines')}</span>
          <span>
            +{cost.added ?? 0} −{cost.removed ?? 0}
          </span>
        </div>
      )}
      {cost.unknown && <p className="convo-runs-note">{t('convo.costUnknown')}</p>}
      <p className="convo-runs-note">{t('convo.costNote')}</p>
    </div>
  )
}

// How full the context is, as a ring: past 80%, in the warning colour.
function Gauge({ part }: { part: number }) {
  const r = 5.5
  const c = 2 * Math.PI * r
  return (
    <svg className={'convo-gauge' + (part >= 0.8 ? ' is-full' : '')} viewBox="0 0 14 14" width="14" height="14" aria-hidden="true">
      <circle cx="7" cy="7" r={r} className="convo-gauge-track" />
      <circle cx="7" cy="7" r={r} className="convo-gauge-fill" strokeDasharray={`${(part * c).toFixed(2)} ${c.toFixed(2)}`} transform="rotate(-90 7 7)" />
    </svg>
  )
}

// A row of the list of agents to go to (see AgentsChat.tsx).
export interface ConvoListRow {
  key: string
  title: string
  where: string // its workspace or session (its host too, where there are several)
  program?: string // the program in it, for its icon
  status: 'working' | 'blocked' | 'done' | 'idle' | 'unknown'
  bg?: 'running' | 'stale' // (its turn over, subagents still out)
  since?: number // (unix ms) when its status last changed
  current: boolean
  placeAs?: { status: ConvoListRow['status']; since?: number } // (the one shown: where it is placed meanwhile, see asHeld)
  onPick: () => void
}

// What is for the person to do: one waiting for an answer, one done and not
// looked at since. Above the rest, those waiting first: working ones are
// not, nothing being for the person there yet (and coming and going often).
const forYou = (r: ConvoListRow) => r.status === 'blocked' || r.status === 'done'
const GROUPS: { name: I18nKey; of: (r: ConvoListRow) => boolean }[] = [
  { name: 'convo.group.todo', of: forYou },
  { name: 'convo.group.working', of: (r) => r.status === 'working' },
  { name: 'convo.group.recent', of: (r) => !forYou(r) && r.status !== 'working' },
]
// A group's rows in their order: the latest to change first (those waiting
// before those done, in the first).
const inGroup = (g: (typeof GROUPS)[number], rows: ConvoListRow[]) => {
  const of = byRecent(rows.filter(g.of))
  return g.name === 'convo.group.todo' ? [...of.filter((r) => r.status === 'blocked'), ...of.filter((r) => r.status !== 'blocked')] : of
}

// The rest, as the list shows them: by the day their status last changed;
// those of no known time (a tmux session, the gateway restarted) last.
// (today, yesterday: the starts of the two calendar days, a day of a clock
// change being 23 or 25 hours long.)
const DAYS: { name: I18nKey; of: (since: number, today: number, yesterday: number) => boolean }[] = [
  { name: 'convo.group.today', of: (since, today) => since >= today },
  { name: 'convo.group.yesterday', of: (since, today, yesterday) => since >= yesterday && since < today },
  { name: 'convo.group.earlier', of: (since, _, yesterday) => since > 0 && since < yesterday },
  { name: 'convo.group.other', of: (since) => since <= 0 },
]
// The groups of old ones that are folded when long, and how many of such
// a group are shown unfolded. (Today's and yesterday's are not: they are
// what the person is at, and are short.)
const FOLDED: I18nKey[] = ['convo.group.earlier', 'convo.group.other']
const foldAt = 5

const dayStart = (now: number) => new Date(now).setHours(0, 0, 0, 0)
const dayBefore = (start: number) => {
  const d = new Date(start)
  d.setDate(d.getDate() - 1)
  return d.setHours(0, 0, 0, 0) // (a midnight skipped by a clock change: its first hour)
}

// The time on a row of the list: how long it has waited or worked; else
// when it came to rest, the clock's time (its day said by its group), or
// the date before yesterday.
function stamp(r: ConvoListRow, now: number, yesterday: number): string {
  if (r.status === 'working' || r.status === 'blocked') return r.since && now >= r.since && now - r.since < 60000 ? t('convo.ago.now') : when(r, now)
  if (!r.since || now < r.since) return ''
  const d = new Date(r.since)
  if (r.since >= yesterday) return d.toLocaleTimeString(getLang(), { hour: '2-digit', minute: '2-digit', hour12: false })
  return d.toLocaleDateString(getLang(), { month: 'numeric', day: 'numeric' })
}

// The latest to change first; those of no known time last, as they were.
const byRecent = (rows: ConvoListRow[]) => rows.map((r, i) => [r, i] as const).sort(([a, i], [b, j]) => (b.since ?? 0) - (a.since ?? 0) || i - j).map(([r]) => r)

// The agents in their order on the list: those for the person (waiting,
// then done and not looked at), then working, then the rest, the latest to
// change first in each (those of no known time, as after the gateway
// restarted, last).
export function listOrder(rows: ConvoListRow[]): ConvoListRow[] {
  return GROUPS.flatMap((g) => inGroup(g, rows.map(asHeld)))
}

// The one being read stays where it was when it was gone to, whatever
// becomes of it meanwhile (looked at, a done one is no longer for the
// person; answered, a waiting one works): it goes to its place once another
// is (else it would move from under the pointer as it is read). Placed so
// by the lists and by Alt+Up/Down alike (placeAs, which its page sets);
// what its row says is as it is.
const asHeld = (r: ConvoListRow): ConvoListRow => (r.placeAs && r.current ? { ...r, status: r.placeAs.status, since: r.placeAs.since } : r)

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

// placed: where rows go (their group, their order) by another status than
// their own, by key (the picker's: as when it opened).
// query, onQuery: what it is searched for, where that is kept outside it
// (else its own).
// search: its search box shown whatever the count (asked for by ⌘K) and
// the focus put in it; onSearchLeft: the box left, empty (shown by the
// count again); hint: the key that asks for it, said in the box.
// (showSearch: shown whatever the count, the focus left to who opened it.)
function AgentList({ rows, onPicked, placed, query: given, onQuery, search, showSearch, onSearchLeft, hint }: { rows: ConvoListRow[]; onPicked?: () => void; placed?: Map<string, ConvoListRow>; query?: string; onQuery?: (q: string) => void; search?: number; showSearch?: boolean; onSearchLeft?: () => void; hint?: boolean }) {
  // (The times move on: drawn again every half minute.)
  const [now, setNow] = useState(Date.now())
  useEffect(() => {
    const timer = setInterval(() => setNow(Date.now()), 30000)
    return () => clearInterval(timer)
  }, [])
  // Which of the long groups are opened out (by their name; shut again at
  // a tap, and shut when the list is drawn anew).
  const [open, setOpen] = useState<string[]>([])
  // What it is searched for (by its title, where it is, its program): the
  // rest goes, and no group is folded.
  const [own, setOwn] = useState('')
  const query = given ?? own
  const setQuery = onQuery ?? setOwn
  const at = (r: ConvoListRow) => placed?.get(r.key) ?? asHeld(r)
  // (Placed: in their order then, ties too; new ones after.)
  const first = placed ? [...placed.keys()] : []
  const place = (r: ConvoListRow) => (first.includes(r.key) ? first.indexOf(r.key) : first.length)
  const found = query.trim() ? rows.filter((r) => matchQuery(query, r.title, r.where, r.program)) : rows
  const inOrder = placed ? [...found].sort((a, b) => place(a) - place(b)) : found
  const today = dayStart(now)
  const yesterday = dayBefore(today)
  const groups = GROUPS.flatMap((g) => {
    const of = inGroup(g, inOrder.map(at))
    if (g.name !== 'convo.group.recent') return [{ name: g.name, of }]
    return DAYS.map((d) => ({ name: d.name, of: of.filter((p) => d.of(p.since ?? 0, today, yesterday)) }))
  })
  return (
    <>
      {(rows.length > SEARCH_FROM || query || !!search || showSearch) && <ListSearch value={query} onChange={setQuery} focus={search} onLeft={onSearchLeft} hint={hint ? KEY_SEARCH : undefined} />}
      {query.trim() && !found.length && <p className="list-search-none">{t('convo.searchNone')}</p>}
      {groups.map((g) => {
        const all = g.of.map((p) => rows.find((r) => r.key === p.key)!)
        if (!all.length) return null
        // The old ones are many and seldom wanted: a few of them are shown
        // and the rest wait behind a tap. The one being read stays shown
        // wherever in the group it is, so going to it never hides it, and
        // the group can still be shut around it.
        const long = FOLDED.includes(g.name) && all.length > foldAt && !query.trim()
        const shut = long && !open.includes(g.name)
        const of = shut ? all.filter((r, i) => i < foldAt || r.current) : all
        const rest = all.length - of.length
        const folds = shut && rest > 0
        return (
          <div key={g.name} className="convo-list-group" role="group" aria-label={t(g.name)}>
            <div className="convo-list-head">
              {t(g.name)}
              <span>{all.length}</span>
            </div>
            {of.map((r) => {
              // (At rest, nothing to say of it but when.)
              const said = r.status === 'blocked' || r.status === 'working' || r.status === 'done'
              const since = stamp(r, now, yesterday)
              return (
                <button
                  key={r.key}
                  type="button"
                  className={'convo-list-row is-' + r.status}
                  aria-current={r.current || undefined}
                  onClick={() => {
                    onPicked?.()
                    r.onPick() // (of the one shown: nothing, or the end of a switch to another; the list may be of before)
                  }}
                >
                  <AgentIcon program={r.program} size={30} dot={r.bg === 'running' ? 'bg' : said ? (r.status as 'blocked' | 'working' | 'done') : undefined} />
                  <span className="convo-list-text" title={[r.title, r.where].filter(Boolean).join(' · ')}>
                    <span className="convo-list-line">
                      <span className="convo-list-title">{r.title}</span>
                      {since && <span className="convo-list-when">{since}</span>}
                    </span>
                    {/* (Its status by its dot only, and its group: in words for a screen
                        reader, and shown where the system's colours take the dot's away.) */}
                    {(said || r.bg) && <span className={'convo-list-status is-' + r.status}>{t(r.bg === 'running' ? 'convo.status.bg' : r.bg === 'stale' ? 'convo.status.bgStale' : (`convo.status.${r.status}` as I18nKey))}</span>}
                    {r.where && (
                      <span className="convo-list-sub">
                        <span className="convo-list-where">{r.where}</span>
                      </span>
                    )}
                  </span>
                </button>
              )
            })}
            {(folds || (long && !shut)) && (
              <button
                type="button"
                className="convo-list-more"
                aria-expanded={!folds}
                onClick={() => setOpen((o) => (folds ? [...o, g.name] : o.filter((n) => n !== g.name)))}
              >
                {folds ? t('convo.group.more', { n: rest }) : t('convo.group.less')}
              </button>
            )}
          </div>
        )
      })}
    </>
  )
}

// A list of agents longer than this has a search box over it.
export const SEARCH_FROM = 5

// The box an agents' list is searched with (see matchQuery). Escape in it
// empties it first.
// (⌘K on a Mac; elsewhere "/", as the browsers there keep Ctrl+K for their
// own search: as the key is said.)
const KEY_SEARCH = /Mac|iPhone|iPad/.test(navigator.platform) ? '⌘K' : '/'

export function ListSearch({ value, onChange, focus, onLeft, hint }: { value: string; onChange: (v: string) => void; focus?: number; onLeft?: () => void; hint?: string }) {
  const ref = useRef<HTMLInputElement>(null)
  const [inside, setInside] = useState(false)
  // (The pointer pressed down in the list it heads, until it is let go:
  // what took the focus from it, see onBlur.)
  const down = useRef<number | null>(null)
  const leaving = useRef(0) // (which leaving a wait is for: the latest only)
  useEffect(() => {
    const list = ref.current?.closest('.convo-side, .convo-picker')
    if (!list) return
    const on = (e: Event) => (down.current = (e as PointerEvent).pointerId)
    const off = (e: Event) => {
      if (down.current === (e as PointerEvent).pointerId) down.current = null
    }
    list.addEventListener('pointerdown', on, true)
    window.addEventListener('pointerup', off, true)
    window.addEventListener('pointercancel', off, true)
    return () => {
      list.removeEventListener('pointerdown', on, true)
      window.removeEventListener('pointerup', off, true)
      window.removeEventListener('pointercancel', off, true)
    }
  }, [])
  useEffect(() => {
    if (focus) {
      ref.current?.focus()
      ref.current?.select()
    }
  }, [focus])
  return (
    <div className="list-search" role="search">
      <span className="list-search-icon" aria-hidden="true">
        <Search size={15} />
      </span>
      <input
        ref={ref}
        onFocus={() => {
          setInside(true)
          leaving.current++ // (a wait for an earlier leaving, called off)
        }}
        onBlur={() => {
          setInside(false)
          // (Left empty: let go once the press in the list that took the
          // focus is over — its click on a row under it landed — before
          // the list moves up: that pointer's own release or cancel, or 5 s.)
          if (!value && onLeft) {
            const n = ++leaving.current
            const leave = () => n === leaving.current && document.activeElement !== ref.current && !ref.current?.value && onLeft()
            const id = down.current
            if (id === null) return void setTimeout(leave, 0)
            let done = false
            const end = (e?: PointerEvent) => {
              if (done || (e && e.pointerId !== id)) return
              done = true
              window.removeEventListener('pointerup', end, true)
              window.removeEventListener('pointercancel', end, true)
              setTimeout(leave, 0)
            }
            window.addEventListener('pointerup', end, true)
            window.addEventListener('pointercancel', end, true)
            setTimeout(() => end(), 5000)
          }
        }}
        type="text"
        enterKeyHint="search"
        value={value}
        placeholder={t('convo.search')}
        aria-label={t('convo.search')}
        autoCapitalize="none"
        autoCorrect="off"
        spellCheck={false}
        onChange={(e) => onChange(e.target.value)}
        onKeyDown={(e) => {
          if (e.key !== 'Escape' || !value) return
          e.preventDefault()
          e.stopPropagation()
          e.nativeEvent.stopImmediatePropagation()
          onChange('')
        }}
      />
      {hint && !value && !inside && <span className="list-search-hint" aria-hidden="true">{hint}</span>}
      {value && (
        <button type="button" className="icon-btn list-search-clear" onClick={() => (onChange(''), ref.current?.focus())} aria-label={t('convo.searchClear')} title={t('convo.searchClear')}>
          <X size={14} />
        </button>
      )}
    </div>
  )
}

// The list over the conversation: under the bar on a wide screen, from the
// bottom on a phone. Its rows stay where they were when it opened: statuses
// changing meanwhile do not move them under the finger (what they say is
// current; one gone goes, one new comes in).
function Picker({ rows, onClose, searchFirst }: { rows: ConvoListRow[]; onClose: () => void; searchFirst?: boolean }) {
  const [first] = useState(() => new Map(rows.map((r) => [r.key, asHeld(r)])))
  const box = useRef<HTMLDivElement>(null)
  // (Once, when it opens: focus moved on in it stays where it was moved.)
  useEffect(() => {
    const prev = document.activeElement as HTMLElement | null
    // (Opened by ⌘K: its search box first, which shows it.)
    if (searchFirst) box.current?.querySelector<HTMLInputElement>('.list-search input')?.focus()
    else (box.current?.querySelector<HTMLElement>('[aria-current="true"]') ?? box.current?.querySelector<HTMLElement>('button'))?.focus()
    const onKey = (e: KeyboardEvent) => {
      // (Its search box with words in it: emptied first, see ListSearch.)
      const field = e.target as HTMLInputElement | null
      if (e.key === 'Escape' && field?.tagName === 'INPUT' && field.value && box.current?.contains(field)) return
      if (e.key === 'Escape') {
        e.preventDefault()
        e.stopPropagation()
        onClose()
      } else if (e.key === 'Tab' && box.current) {
        // (Tab goes round in it: what is behind is not to be reached.)
        const all = [...box.current.querySelectorAll<HTMLElement>('button, input')]
        if (!all.length) return
        const at = all.indexOf(document.activeElement as HTMLElement)
        const to = e.shiftKey ? (at <= 0 ? all.length - 1 : at - 1) : at < 0 || at === all.length - 1 ? 0 : at + 1
        e.preventDefault()
        all[to].focus()
      }
    }
    document.addEventListener('keydown', onKey, true)
    return () => {
      document.removeEventListener('keydown', onKey, true)
      prev?.focus?.()
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [onClose])
  return (
    <>
      <div className="convo-picker-back" onClick={onClose} />
      <div className="convo-picker" role="dialog" aria-modal="true" aria-label={t('convo.pick')} ref={box}>
        <div className="convo-picker-title">
          <span>{t('convo.pick')}</span>
          <button type="button" className="icon-btn convo-picker-close" onClick={onClose} aria-label={t('common.close')} title={t('common.close')}>
            <X size={16} />
          </button>
        </div>
        <NewAgentRow onPicked={onClose} />
        <AgentList rows={rows} placed={first} onPicked={onClose} showSearch={searchFirst} />
      </div>
    </>
  )
}

// "New agent", as a row of the list it heads: its plus where the agents'
// icons are, its words where their titles are.
function NewAgentRow({ onPicked }: { onPicked?: () => void }) {
  return (
    <button
      type="button"
      className="convo-side-new"
      onClick={() => {
        onPicked?.()
        window.dispatchEvent(new Event(NEW_AGENT))
      }}
    >
      <span className="convo-side-new-icon" aria-hidden="true">
        <Plus size={16} />
      </span>
      {t('newAgent.title')}
    </button>
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
            <li key={i} className={'is-' + x.status}>
              {x.subject}
              {/* (Its state is in its look: said in words too.) */}
              <span className="sr-only">{' · ' + t(x.status === 'completed' ? 'convo.taskDone' : x.status === 'in_progress' ? 'convo.taskDoing' : 'convo.taskTodo')}</span>
            </li>
          ))}
        </ol>
      )}
    </div>
  )
}

// What a "!" command printed: as a tool's output is shown (the terminal's
// letters, a long one by its start and end, all of it on a tap), what it
// wrote to stderr under it.
function ShellOut({ sh, onCopy }: { sh: ConvoShell; onCopy: (s: string) => void }) {
  const [full, setFull] = useState(false)
  const head = sh.out ?? ''
  const tail = sh.out_tail
  const gap = tail ? (sh.omitted ? t('convo.omittedLines', { n: sh.omitted }) : t('convo.omitted')) : ''
  const all = tail ? head + (head && !head.endsWith('\n') ? '\n' : '') + gap + '\n' + tail : head
  return (
    <div className="convo-shell">
      {all && (
        <>
          <div className="convo-pre-wrap">
            <pre className={'convo-pre convo-result' + (full ? ' is-full' : '')}>
              {tail ? (
                <>
                  {head && !head.endsWith('\n') ? head + '\n' : head}
                  <span className="convo-omitted">{gap}</span>
                  {'\n' + tail}
                </>
              ) : (
                head
              )}
            </pre>
            <button type="button" className="md-code-copy" onClick={() => onCopy(all)}>
              {t('common.copy')}
            </button>
          </div>
          {!full && all.split('\n').length > 12 && (
            <button type="button" className="link-btn" onClick={() => setFull(true)}>
              {t('convo.showAll')}
            </button>
          )}
        </>
      )}
      {sh.err && (
        <pre className="convo-pre convo-shell-err">
          {sh.err}
          {sh.err_cut && <span className="convo-omitted">{'\n' + t('convo.cut')}</span>}
        </pre>
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
      case 'note': {
        const p = it.e.parts[0]
        if (p?.event) return <EventNote ev={p.event} idle={it.idle} />
        // (Retries next to each other: on the first's line, the last's.)
        if (p?.trouble) return <TroubleNote key={troubleKey(p.trouble)} t={(it.last ?? it.e).parts[0]?.trouble ?? p.trouble} />
        if (p?.shell) return <ShellOut sh={p.shell} onCopy={copy} />
        return <p className="convo-note">{noteText(it.e)}</p>
      }
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
      case 'gone':
        return <Gone it={it} copy={copy} />
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
    case 'gone': {
      const o = b as typeof a
      return a.entries.length === o.entries.length && a.entries.every((e, i) => e === o.entries[i])
    }
    case 'note': {
      const o = b as typeof a
      return a.e === o.e && a.idle === o.idle && a.last === o.last
    }
    default:
      return a.e === (b as typeof a).e
  }
}

// What went wrong, where the thread would otherwise show nothing: why the
// agent fell silent, or stopped mid-sentence. Its own words, where it had
// any, are behind a tap: they can run long and are often in English.
// (A trouble is told from another by what it is of, so that a log read
// anew does not leave the one in its place opened out.)
const troubleKey = (tr: ConvoTrouble) => [tr.kind, tr.name ?? '', tr.status ?? '', (tr.text ?? '').slice(0, 40)].join('|')

function TroubleNote({ t: tr }: { t: ConvoTrouble }) {
  const [open, setOpen] = useState(false)
  const said = (tr.text ?? '').trim()
  let head: string
  switch (tr.kind) {
    case 'refused':
      head = tr.again && tr.name ? t('convo.trouble.refusedAgain', { m: modelName(tr.name) }) : t('convo.trouble.refused')
      break
    case 'api':
      head = tr.status ? t('convo.trouble.apiStatus', { n: tr.status }) : t('convo.trouble.api')
      if (tr.attempt && tr.max) head += t('convo.trouble.retry', { a: tr.attempt, m: tr.max })
      break
    case 'cut':
      head = t('convo.trouble.cut')
      break
    default: {
      const why = tr.secs ? t('convo.trouble.hookTimedOut', { n: tr.secs }) : tr.status ? t('convo.trouble.hookExit', { n: tr.status }) : ''
      head = tr.name ? t('convo.trouble.hook', { h: tr.name + (why ? ' · ' + why : '') }) : why ? t('convo.trouble.hook', { h: why }) : t('convo.trouble.hookPlain')
      if (tr.again && !tr.name) head = t('convo.trouble.hookStopped')
    }
  }
  return (
    <div className={'convo-trouble is-' + tr.kind}>
      <TriangleAlert size={14} aria-hidden="true" />
      <div className="convo-trouble-body">
        <p className="convo-trouble-head">{head}</p>
        {said && open && (
          <p className="convo-trouble-said">
            {said}
            {tr.cut && <span className="convo-trouble-cut">{t('convo.trouble.cut.more')}</span>}
          </p>
        )}
        {said && (
          <button type="button" className="link-btn" aria-expanded={open} onClick={() => setOpen(!open)}>
            {t(open ? 'convo.trouble.less' : 'convo.trouble.more')}
          </button>
        )}
      </div>
    </div>
  )
}

// Something that happened to the session itself: a compaction, as a line
// across (what it came to, how long it took: the summary it wrote follows,
// folded), or a turn a scheduled task set going, its prompt behind a tap.
function EventNote({ ev, idle }: { ev: ConvoEvent; idle?: boolean }) {
  const [open, setOpen] = useState(false)
  const showTerminal = useContext(ShowTerminal)
  // What went on while the person was away; a goal set or judged; any other notice.
  if (ev.kind === 'recap' || ev.kind === 'goal' || ev.kind === 'notice') {
    const head = ev.kind === 'recap' ? t('convo.recap') : ev.kind === 'goal' ? t(ev.set ? 'convo.goal.set' : ev.met ? 'convo.goal.met' : 'convo.goal.unmet') : ev.text ?? ''
    const said = ev.kind === 'recap' ? (ev.text ?? '') : ev.kind === 'goal' ? (ev.goal ?? '') : ''
    return (
      <div className={'convo-event is-' + ev.kind + (ev.level === 'warning' ? ' is-warn' : '') + (ev.kind === 'goal' && ev.met ? ' is-met' : '')}>
        <div className="convo-trouble-body">
          <p className="convo-trouble-head">{head}</p>
          {said && <p className="convo-trouble-said">{said}</p>}
          {ev.kind === 'goal' && ev.why && open && <p className="convo-trouble-said">{ev.why}</p>}
          {ev.kind === 'goal' && ev.why && (
            <button type="button" className="link-btn" aria-expanded={open} onClick={() => setOpen(!open)}>
              {t(open ? 'convo.trouble.less' : 'convo.goal.why')}
            </button>
          )}
        </div>
      </div>
    )
  }
  // The plan's usage limit: reached (until when), to go on of itself, reset, called off.
  if (ev.kind === 'limit' || ev.kind === 'limit_wait' || ev.kind === 'limit_reset' || ev.kind === 'limit_cancelled') {
    let head: string
    if (ev.kind === 'limit') {
      const which = ev.limit_kind === 'five_hour' ? t('convo.limit.fiveHour') : ev.limit_kind?.startsWith('seven_day') ? t('convo.limit.week') : ''
      head = t('convo.limit.reached', { which: which ? t('convo.limit.which', { w: which }) : '', at: ev.resets_at ? resetsAt(ev.resets_at) : '?' })
    } else head = t(ev.kind === 'limit_wait' ? 'convo.limit.wait' : ev.kind === 'limit_reset' ? 'convo.limit.reset' : 'convo.limit.cancelled')
    return (
      <div className={'convo-event is-limit' + (ev.kind === 'limit' ? ' is-warn' : '')}>
        <Clock size={14} aria-hidden="true" />
        <div className="convo-trouble-body">
          <p className="convo-trouble-head">{head}</p>
          {ev.kind === 'limit_cancelled' && ev.text && <p className="convo-trouble-said">{ev.text}</p>}
          {ev.kind === 'limit_wait' && showTerminal && (
            <button type="button" className="link-btn" onClick={showTerminal}>
              {t('convo.showTerminal')}
            </button>
          )}
        </div>
      </div>
    )
  }
  if (ev.kind === 'scheduled') {
    const said = (ev.text ?? '').trim()
    return (
      <div className="convo-event is-scheduled">
        <Clock size={14} aria-hidden="true" />
        <div className="convo-trouble-body">
          <p className="convo-trouble-head">{t(ev.loop ? 'convo.event.loop' : 'convo.event.scheduled')}</p>
          {said && open && (
            <p className="convo-trouble-said">
              {said}
              {ev.cut && '…'}
            </p>
          )}
          {said && (
            <button type="button" className="link-btn" aria-expanded={open} onClick={() => setOpen(!open)}>
              {t(open ? 'convo.trouble.less' : 'convo.event.prompt')}
            </button>
          )}
        </div>
      </div>
    )
  }
  const how = ev.kind === 'idle_compact' || idle ? t('convo.event.idle') : ev.trigger === 'manual' ? t('convo.event.manual') : ev.trigger === 'auto' ? t('convo.event.auto') : ''
  const said = [how ? t('convo.event.compactHow', { how }) : t('convo.event.compact')]
  if (ev.pre && ev.post) said.push(t('convo.event.sizes', { a: tokens(ev.pre), b: tokens(ev.post) }))
  if (ev.ms) said.push(t('convo.event.took', { n: Math.max(1, Math.round(ev.ms / 1000)) }))
  // (Each part whole on a line: a narrow screen breaks between them.)
  return (
    <p className="convo-event is-compact">
      <span className="convo-event-said">
        {said.map((x, i) => (
          <span key={i} className="convo-event-part">
            {i > 0 && ' · '}
            {x}
          </span>
        ))}
      </span>
    </p>
  )
}

// (The conversation's terminal, for a line that sends one there.)
const ShowTerminal = createContext<(() => void) | undefined>(undefined)

// A subagent sent off, whose own conversation can be shown: by its id.
type Sub = { agent: string; title?: string }
const OpenSub = createContext<((s: Sub) => void) | undefined>(undefined)
const isSubId = (id: string) => /^[a-z0-9]{6,40}$/.test(id)

// A subagent's own conversation, beside the thread (over it, where narrow),
// read only: what it was asked, what it did and answered. Read when shown;
// again while it is known to go on. One sent off by it opens here in turn
// (back: to the one that sent it).
function SubPanel({ of, sub, depth, live, onOpen, onBack, onClose, active, copy }: { of: string; sub: Sub; depth: number; live: (agent: string) => boolean; onOpen: (s: Sub) => void; onBack: () => void; onClose: () => void; active: boolean; copy: (s: string) => void }) {
  const [sessionId, pane = ''] = of.split(' ')
  // (Of one sent off by the session's agent, as its running list says; of
  // one sent off by a subagent, while its log keeps growing.)
  const told = depth === 1 && live(sub.agent)
  const c = useSubConvo(sessionId, pane, sub.agent, told, active, depth > 1)
  const going = depth === 1 ? told : c.growing
  const panel = useRef<HTMLElement>(null)
  const [opened, setOpened] = useState<Record<string, boolean>>({})
  const toggle = useCallback((key: string, open: boolean) => setOpened((o) => ({ ...o, [key]: open })), [])
  const list = useMemo(() => items(c.entries, going, going), [c.entries, going])
  useEffect(() => {
    const prev = document.activeElement as HTMLElement | null
    panel.current?.focus({ preventScroll: true })
    return () => {
      if (prev?.isConnected) prev.focus({ preventScroll: true })
    }
  }, [])
  useEffect(() => {
    if (!active) return
    const onKey = (e: KeyboardEvent) => {
      const el = e.target as HTMLElement | null
      if (e.key !== 'Escape' || (el && (el.tagName === 'INPUT' || el.tagName === 'TEXTAREA') && !el.classList.contains('xterm-helper-textarea'))) return
      if (document.querySelector('[aria-modal="true"], [role="menu"]')) return
      e.preventDefault()
      e.stopPropagation()
      if (depth > 1) onBack()
      else onClose()
    }
    window.addEventListener('keydown', onKey, true)
    return () => window.removeEventListener('keydown', onKey, true)
  }, [onBack, onClose, active, depth])
  return (
    <aside className="convo-panel convo-sub" aria-label={t('sub.title')} tabIndex={-1} ref={panel}>
      <div className="convo-panel-head">
        {depth > 1 && (
          <button type="button" className="icon-btn" onClick={onBack} aria-label={t('common.back')} title={t('common.back')}>
            <ChevronRight size={16} className="is-back" />
          </button>
        )}
        <span className="convo-panel-title" title={sub.title}>
          {sub.title || t('sub.title')}
        </span>
        <span className="convo-sub-state">{going ? t('sub.going') : ''}</span>
        <span className="spacer" />
        <button type="button" className="icon-btn" onClick={c.read} disabled={c.loading} aria-label={t('sub.refresh')} title={t('sub.refresh')}>
          <Loader size={15} className={c.loading ? 'convo-running-spin' : ''} />
        </button>
        <button type="button" className="icon-btn" onClick={onClose} aria-label={t('common.close')} title={t('common.close') + ' (Esc)'}>
          <X size={16} />
        </button>
      </div>
      <div className="convo-panel-body convo-sub-body">
        {c.older && (
          <p className="convo-older">
            {c.olderCut ? (
              t('convo.olderCut')
            ) : (
              <button type="button" className="link-btn" onClick={c.readOlder} disabled={c.loading}>
                {t('sub.older')}
              </button>
            )}
          </p>
        )}
        {c.failed && <LoadErrorLine onRetry={c.read} />}
        {!c.failed && !c.loading && list.length === 0 && <p className="convo-older">{t('sub.empty')}</p>}
        <OpenSub.Provider value={onOpen}>
          {list.map((it) => (
            <ListItem key={itemKey(it)} it={it} copy={copy} open={it.kind === 'work' ? !!opened[it.key] : false} onToggle={toggle} onShow={noShow} />
          ))}
        </OpenSub.Provider>
      </div>
    </aside>
  )
}

function LoadErrorLine({ onRetry }: { onRetry: () => void }) {
  return (
    <p className="convo-older">
      {t('sub.failed')}{' '}
      <button type="button" className="link-btn" onClick={onRetry}>
        {t('common.retry')}
      </button>
    </p>
  )
}

// When a usage limit resets, in the page's time: the day too, when not today.
function resetsAt(sec: number): string {
  const d = new Date(sec * 1000)
  const time = d.toLocaleTimeString(getLang(), { hour: '2-digit', minute: '2-digit' })
  return d.toDateString() === new Date().toDateString() ? time : d.toLocaleDateString(getLang(), { month: 'short', day: 'numeric' }) + ' ' + time
}

// What became of a message that was typed and has not turned up in the
// log, read off the pane's own screen when it did not: it settles whether
// the words are still sitting in the input, which nothing else can say
// afterwards. The screen is shown on a tap, since it is what was looked at.
function Missing({ look, onEnter }: { look?: ConvoPending['look']; onEnter?: () => Promise<boolean> }) {
  const [open, setOpen] = useState(false)
  // (Enter pressed from here: under way, or what became of it.)
  const [entering, setEntering] = useState<'' | 'busy' | 'gone' | 'failed'>('')
  if (!look) return null
  if (look === 'looking') return <p className="convo-missing" role="status">{t('convo.missing.looking')}</p>
  if (look === 'failed') return <p className="convo-missing">{t('convo.missing.failed')}</p>
  const canEnter = !!onEnter && look.known && look.waiting
  return (
    <div className="convo-missing">
      <p>{t(!look.known ? 'convo.missing.unsure' : look.waiting ? (canEnter ? 'convo.missing.waitingEnter' : 'convo.missing.waiting') : 'convo.missing.taken')}</p>
      {canEnter && (
        <button
          type="button"
          className="btn btn-ghost convo-missing-enter"
          disabled={entering === 'busy'}
          onClick={() => {
            setEntering('busy')
            onEnter!().then(
              (pressed) => setEntering(pressed ? '' : 'gone'),
              () => setEntering('failed'),
            )
          }}
        >
          {entering === 'busy' ? t('convo.missing.entering') : t('convo.missing.enter')}
        </button>
      )}
      {entering === 'gone' && <p role="status">{t('convo.missing.enterGone')}</p>}
      {entering === 'failed' && <p role="alert">{t('convo.missing.enterFailed')}</p>}
      <button type="button" className="link-btn" aria-expanded={open} onClick={() => setOpen(!open)}>
        {t(open ? 'convo.missing.hide' : 'convo.missing.show')}
      </button>
      {open && <pre className="convo-missing-screen">{look.screen}</pre>}
    </div>
  )
}

const noShow = () => {}
// (An item where it is: what the agent writes is the same text written
// and, its turn over, its answer.)
const placeKey = (it: Item) => itemKey(it).replace(/^[xa](?=\d)/, 'p')

// An item noted in view, where it was (see note).
type Anchor = { key: string; top: number; fromEnd: number; last: string }

// Where each conversation was left, for this page's life: as many as are
// kept to be shown at once (conversation.ts), the latest last.
type Place = { gen: number; stuck: boolean; anchors: Anchor[]; noted: Item[]; opened: Record<string, boolean>; allOpen: boolean }
const places = new Map<string, Place>()
function keepAt(of: string, p: Place) {
  places.delete(of)
  places.set(of, p)
  while (places.size > 12) places.delete(places.keys().next().value!)
}

// Where an item begins in the log (its first entry's seq).
const startSeq = (it: Item): number => (it.kind === 'gone' ? it.entries[0].seq : 'e' in it ? it.e.seq : Number(/\d+/.exec(it.key)?.[0] ?? 0))

// What was read of each conversation, for this page's life: the last entry
// seen at its end (of that reading of its log).
const reads = new Map<string, { gen: number; seq: number }>()
function readUpTo(of: string, gen: number, seq: number) {
  const was = reads.get(of)
  if (was && was.gen === gen && was.seq >= seq) return
  reads.delete(of)
  reads.set(of, { gen, seq })
  while (reads.size > 50) reads.delete(reads.keys().next().value!)
}

// How the tool calls opened by hand are shown (by call id), the latest last.
const toolShown = new Map<string, { open: boolean; full: boolean }>()
function keepShown(id: string | undefined, v: { open: boolean; full: boolean }) {
  if (!id) return
  toolShown.delete(id)
  if (v.open || v.full) toolShown.set(id, v)
  while (toolShown.size > 500) toolShown.delete(toolShown.keys().next().value!)
}

// (Keys typed there are for what is typed in: a box, the terminal.)
const editable = (t: EventTarget | null) => t instanceof HTMLElement && (t.isContentEditable || /^(INPUT|TEXTAREA|SELECT)$/.test(t.tagName) || !!t.closest('.xterm'))

// What a rewind took back: a line saying so, what it was behind a tap (as
// it showed, faded: the agent no longer has it).
function Gone({ it, copy }: { it: Extract<Item, { kind: 'gone' }>; copy: (s: string) => void }) {
  const [open, setOpen] = useState(false)
  const [opened, setOpened] = useState<Record<string, boolean>>({})
  const toggle = useCallback((key: string, o: boolean) => setOpened((m) => ({ ...m, [key]: o })), [])
  // (Shown as they showed: as entries not taken back, or they would fold into this line again.)
  const inner = useMemo(() => (open ? items(it.entries.map((e) => ({ ...e, gone: false })), false, false) : []), [open, it.entries])
  const said = it.entries.filter((e) => e.role === 'user').length
  return (
    <div className="convo-gone">
      <button type="button" className="convo-work-head convo-gone-head" aria-expanded={open} onClick={() => setOpen(!open)}>
        <ChevronRight size={15} className="convo-chev" aria-hidden="true" />
        <span className="convo-work-text">{t(said ? 'convo.gone.said' : 'convo.gone.plain', { n: said })}</span>
      </button>
      {open && (
        <div className="convo-gone-body">
          {inner.map((x) => (
            <ListItem key={itemKey(x)} it={x} copy={copy} open={!!opened[itemKey(x)]} onToggle={toggle} onShow={noShow} />
          ))}
        </div>
      )}
    </div>
  )
}
// (Claude Code's own words that it was stopped, in the person's language.)
const noteText = (e: ConvoEntry) => {
  const n = e.parts[0]?.noted
  if (n) {
    const list = n.paths.map(shortPath).join(t('convo.noted.sep'))
    return t(`convo.noted.${n.kind}` as I18nKey, { s: n.more ? list + t('convo.noted.more', { n: n.more }) : list })
  }
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
  // (A shell command run with "!": in the terminal's letters, as typed.)
  const shell = /^! \S/.test(text)
  return (
    <div className={'convo-user' + (e.mid ? ' is-mid' : '')}>
      <div className={'convo-user-text' + (shell ? ' is-shell' : '')}>
        {text}
        {images > 0 && !/\[Image #\d+\]/.test(text) && <span className="convo-image">{t(images === 1 ? 'convo.images1' : 'convo.images', { n: images })}</span>}
        {e.parts.some((p) => p.truncated) && <p className="convo-cut">{t('convo.cut')}</p>}
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
          {Math.max(it.edits.files.length - CARD_FILES, 0) + (it.edits.more ?? 0) > 0 && (
            <button type="button" className="convo-changes-file convo-changes-more" onClick={() => onShow(it.key)}>
              {t('convo.moreFiles', { n: Math.max(it.edits.files.length - CARD_FILES, 0) + (it.edits.more ?? 0) })}
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
                s.parts[0]?.event ? (
                  <EventNote key={i} ev={s.parts[0].event} />
                ) : s.parts[0]?.trouble ? (
                  <TroubleNote key={i + ':' + troubleKey(s.parts[0].trouble!)} t={s.parts[0].trouble!} />
                ) : (
                  <p key={i} className="convo-note">{noteText(s)}</p>
                )
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
  return { text: d, code: v === 'cmd' || v === 'search' || v === 'web' || v === 'browser' || v === 'mcp' || v === 'doc' || v === 'page' || v === 'stop' }
}

// The word each verb says, done and under way. A table, not a name put
// together: a verb without words of its own does not compile.
const VERB_SAID: Record<Verb, { v: I18nKey; live: I18nKey }> = {
  cmd: { v: 'convo.v.cmd', live: 'convo.live.cmd' },
  read: { v: 'convo.v.read', live: 'convo.live.read' },
  edit: { v: 'convo.v.edit', live: 'convo.live.edit' },
  write: { v: 'convo.v.write', live: 'convo.live.write' },
  search: { v: 'convo.v.search', live: 'convo.live.search' },
  web: { v: 'convo.v.web', live: 'convo.live.web' },
  agent: { v: 'convo.v.agent', live: 'convo.live.agent' },
  task: { v: 'convo.v.task', live: 'convo.live.task' },
  ask: { v: 'convo.v.ask', live: 'convo.live.ask' },
  wait: { v: 'convo.v.wait', live: 'convo.live.wait' },
  stop: { v: 'convo.v.stop', live: 'convo.live.stop' },
  page: { v: 'convo.v.page', live: 'convo.live.page' },
  send: { v: 'convo.v.send', live: 'convo.live.send' },
  skill: { v: 'convo.v.skill', live: 'convo.live.skill' },
  agents: { v: 'convo.v.agents', live: 'convo.live.agents' },
  cron: { v: 'convo.v.cron', live: 'convo.live.cron' },
  advisor: { v: 'convo.v.advisor', live: 'convo.live.advisor' },
  browser: { v: 'convo.v.browser', live: 'convo.live.browser' },
  doc: { v: 'convo.v.doc', live: 'convo.live.doc' },
  mcp: { v: 'convo.v.mcp', live: 'convo.live.mcp' },
  other: { v: 'convo.v.cmd', live: 'convo.live.other' }, // (verbOf gives no "other")
}

// What a step under way says. A command says the command itself, not what
// Claude Code says it is for: while it runs, what is running is the thing
// wanted; the line goes back to saying what it was for once it is done.
function liveText(tool: ConvoTool): string {
  const v = verbOf(tool.name)
  // (The folder it goes to first comes off before the first line is taken:
  //  a command of several lines begins "cd … &&" on a line of its own.)
  const cmd = firstLine((tool.detail ?? '').replace(/^cd\s+(?:"[^"]*"|'[^']*'|[^\s&;|]+)\s*&&\s*/, ''))
  const o = v === 'cmd' && cmd ? cmd : object(tool).text
  return v ? t(VERB_SAID[v].live, { s: o }) : t('convo.live.other', { s: toolName(tool.name) })
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
  const openSub = useContext(OpenSub)
  // (Opened or shown in full: kept by the call, so it is so when its
  // conversation is shown again, as where it was read is.)
  const [open, setOpenNow] = useState(() => toolShown.get(tool.id ?? '')?.open ?? false)
  const [full, setFullNow] = useState(() => toolShown.get(tool.id ?? '')?.full ?? false)
  const setOpen = (v: boolean) => {
    setOpenNow(v)
    keepShown(tool.id, { open: v, full })
  }
  const setFull = (v: boolean) => {
    setFullNow(v)
    keepShown(tool.id, { open, full: v })
  }
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
  const receipt = (isEdit && !tool.error && tool.name !== 'NotebookEdit') || (!!tool.answers && !tool.error)
  const head = tool.done && !receipt ? (code ? tool.result?.replace(/^Exit code \d+\n?/, '') : tool.result) : undefined
  // (A long result: its start and its end, what was left out between said
  // on a line of its own; all of it is what shows, is counted, is copied.)
  const tail = head !== undefined ? tool.result_tail : undefined
  const gap = tail ? (tool.omitted ? t('convo.omittedLines', { n: tool.omitted }) : t('convo.omitted')) : ''
  const result = tail ? (head ?? '') + (head && !head.endsWith('\n') ? '\n' : '') + gap + '\n' + tail : head
  return (
    <div className={'convo-step' + (tool.error ? ' is-error' : '')} data-tool={tool.id}>
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
        <span className="convo-step-verb">{v ? t(VERB_SAID[v].v) : toolName(tool.name)}</span>
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
          {tool.agent_id && openSub && (
            <button type="button" className="link-btn convo-sub-open" onClick={() => openSub({ agent: tool.agent_id!, title: tool.title || firstLine(tool.detail ?? '') })}>
              {t('sub.open')}
            </button>
          )}
          {tool.asks && tool.answers && (
            // (A question of Claude Code's, answered: what was asked, what was answered.)
            <dl className="convo-ask-answers">
              {tool.asks.map((a, i) => (
                <div key={i}>
                  <dt>{a.question}</dt>
                  <dd>{tool.answers![i] || t('ask.notAnswered')}</dd>
                </div>
              ))}
            </dl>
          )}
          {tool.same ? <div className="convo-result-head">{t('convo.noChange')}</div> : tool.patch ? <Hunks hunks={tool.patch} cut={tool.patch_cut} /> : isEdit && <Diff old={tool.old ?? ''} new={tool.new ?? ''} />}
          {!tool.done ? (
            <div className="convo-result-head">{t('convo.running')}</div>
          ) : result ? (
            <>
              <div className="convo-result-head">
                {t('convo.output', { n: Math.max(0, (tool.result_lines ?? 0) - (code && head !== tool.result ? 1 : 0)) })}
                {tool.truncated && <span> · {t(tail ? 'convo.cutMiddle' : 'convo.cut')}</span>}
              </div>
              <div className="convo-pre-wrap">
                <pre className={'convo-pre convo-result' + (full ? ' is-full' : '')}>
                  {tail ? (
                    <>
                      {head && !head.endsWith('\n') ? head + '\n' : head}
                      <span className="convo-omitted">{gap}</span>
                      {'\n' + tail}
                    </>
                  ) : (
                    result
                  )}
                </pre>
                <button type="button" className="md-code-copy" onClick={() => onCopy(result)}>{t('common.copy')}</button>
              </div>
              {!full && result.split('\n').length > 12 && (
                <button type="button" className="link-btn" onClick={() => setFull(true)}>{t('convo.showAll')}</button>
              )}
            </>
          ) : (
            !receipt && (
              <div className="convo-result-head">
                {tool.images ? t('convo.gotImages', { n: tool.images }) : tool.refs ? t('convo.gotTools', { n: tool.refs }) : t('convo.noOutput')}
              </div>
            )
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
function ChangesPanel({ edits, focus, onClose, active }: { edits: Edits; focus?: string; onClose: () => void; active: boolean }) {
  const box = useRef<HTMLDivElement>(null)
  const panel = useRef<HTMLElement>(null)
  // (Opened, the keys go to it; closed, back where they were.)
  useEffect(() => {
    const prev = document.activeElement as HTMLElement | null
    panel.current?.focus({ preventScroll: true })
    return () => {
      if (prev?.isConnected) prev.focus({ preventScroll: true })
    }
  }, [])
  useEffect(() => {
    const el = focus ? box.current?.querySelector(`[data-file="${CSS.escape(focus)}"]`) : null
    el?.scrollIntoView({ block: 'start' })
  }, [focus])
  useEffect(() => {
    if (!active) return // (behind a terminal tab: its Escape is the terminal's)
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
  }, [onClose, active])
  return (
    <aside className="convo-panel" aria-label={t('convo.changesTitle')} tabIndex={-1} ref={panel}>
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
                  {tl.same ? (
                    <div className="convo-hunk-gap">{t('convo.noChange')}</div>
                  ) : h?.length ? (
                    <Hunks hunks={h} cut={tl.patch_cut} />
                  ) : tl.patch_cut ? (
                    <div className="convo-hunk-gap">{t('convo.patchCut')}</div>
                  ) : (
                    <Diff old={tl.old ?? ''} new={tl.new ?? ''} />
                  )}
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
