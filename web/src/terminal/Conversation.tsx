import { createContext, Fragment, memo, useCallback, useContext, useEffect, useLayoutEffect, useMemo, useRef, useState, type ReactNode } from 'react'
import { ArrowDown, ArrowUp, Bot, Check, MessagesSquare, FilePen, Hand, ListChecks, ShieldOff, Sparkles, Copy, Cpu, FileText, Gauge as GaugeIcon, LogOut, Radar, SquarePlus, SquareTerminal, Target, TextCursorInput, FolderOpen, Info, Image as ImageIcon, ImageOff, ChevronDown, ChevronLeft, ChevronRight, ChevronUp, ChevronsDownUp, ChevronsUpDown, Clock, Download, Eye, GitBranch, Keyboard, ListOrdered, Loader, MoreHorizontal, MoreVertical, PanelLeft, Pin, RotateCw, Search, TriangleAlert, X, Plus } from 'lucide-react'
import { t, getLang, type I18nKey } from '../i18n'
import { api } from '../api'
import { asHeld, dayStart, FOLDED, foldAt, GROUPS, inGroup, listOrder, pinned, type ConvoListRow, type RowCmds } from './agentOrder'
import { copyText, Dialog, DialogHead, isCoarse, Menu, MenuLayer, useToast, type MenuItem } from '../components/ui'
import { saveFile } from '../saveFile'
import { composing, foldAllKey, helpKey, jumpKey, onMac, sideKey, switchKey } from './pageKeys'
import { keepNames, keptNames, local } from '../storage'
import { useFlip } from '../flip'
import { CodeSpanOf, FileLinkOf, Markdown } from './markdown'
import { codePath } from './paths'
import { useSubConvo } from './subagent'
import { useSubScan, type SubScanAgent } from './subscan'
import { NEW_AGENT } from './newAgent'
import { Code } from './Code'
import { ConvoSearchPanel } from './convoSearchPanel'
import { ConvoTurnsPanel } from './convoTurnsPanel'
import { ConvoRail, railCell, type RailTurn } from './convoRail'
import { IMAGE_EXT, previewable, readError } from './textPreview'
import { useObjectURL } from './picture'
import { previewOf } from './convoTurns'
import { itemKey } from './convoSearch'
import { exportMarkdown } from './convoExport'
import { AgentIcon } from './agentIcons'
import { matchQuery } from './agents'
import { callOut, keptEntries } from './conversation'
import { ShowTerminal } from './showTerminal'
import { FileTypeIcon } from './fileIcons'
import { categoryIcon, stepIcon, ThinkIcon, WorkIcon } from './stepIcons'
import { SessionCard, type CardData } from './sessionCard'
import type { Convo, ConvoAnswer, ConvoCost, ConvoQuota, ConvoQuotaWindow, ConvoEntry, ConvoShell, ConvoHunk, ConvoMeta, ConvoPart, ConvoQueued, ConvoRunning, ConvoTask, ConvoTool, ConvoTrouble, ConvoEvent } from './conversation'
import { agentName } from './askText'
import { duration, hhmm, when } from './convoTime'
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

// What the agent says on its way that is only that — a line or two before a
// step ("Let me look at the code first") — drawn a step quieter than what it
// concludes. Not by where it stands alone: a finding of length or with a
// structure (a heading, a list, a table, code) is read as body text wherever
// it is, as one followed by some last small step is still the answer.
function brief(text: string): boolean {
  const s = text.trim()
  if (s.length > 160 || s.split('\n').filter((l) => l.trim()).length > 2) return false
  return !/^\s*(?:#{1,6}\s|[-*+]\s|\d+[.)]\s|>|\||```)/m.test(s)
}

const LONG_LINES = 8 // a message of the person's longer than this is folded
const LONG_CHARS = 700
const clip = (s: string, n: number) => (s.length > n ? s.slice(0, n - 1) + '…' : s)
const baseName = (p: string) => p.split('\n')[0].trim().replace(/\/+$/, '').split('/').pop() ?? ''
// A kind of step in a turn's line: the files changed by name (two, then how
// many more), the one command run by what it is; else how many.
function said(cat: Category, tools: ConvoTool[]): string {
  const n = tools.length
  if (cat === 'edit') {
    // (By the whole path, named by the last part — with its folder where two share it.)
    const paths = [...new Set(tools.map((tl) => (tl.detail ?? '').split('\n')[0].trim()).filter(Boolean))]
    const names = paths.map(baseName)
    const files = paths.map((p, i) => (names.indexOf(names[i]) !== names.lastIndexOf(names[i]) ? p.split('/').slice(-2).join('/') : names[i]))
    if (!files.length) return count(cat, n)
    const two = files.slice(0, 2).join(t('convo.listSep'))
    return files.length > 2 ? t('convo.sum.editedMany', { files: two, n: files.length, m: files.length - 2 }) : t('convo.sum.edited', { files: two })
  }
  if (cat === 'cmd' && n === 1) {
    const o = object(tools[0])
    return o.text ? (o.code ? t('convo.sum.ran', { s: clip(o.text, 40) }) : clip(o.text, 48)) : count(cat, n)
  }
  return count(cat, n)
}

// What the bar over the conversation says of the agent: where it is, what
// it is doing (unknown when that cannot be trusted now).
export interface ConvoAgent {
  where?: string // herdr: the workspace
  title?: string // the session's title (Claude Code's own), else the pane's
  status: 'working' | 'blocked' | 'done' | 'idle' | 'unknown'
  bg?: 'running' | 'stale' | 'checking' // (its turn over: subagents still out, or that being read for; see agents.ts)
  dialog?: boolean // (Claude Code has a dialog of its own open, a picker or /usage, say: not a question to answer here)
  cmds?: RowCmds // (its turn over: commands it left running in the background)
  other?: string // the program in the pane, when it is not Claude Code
  program?: string // the program in the pane, for its icon
}

// A message sent from the composer while the conversation shows: "waiting"
// until the log has it (then it is dropped); "stuck" where the screen showed
// its words still in the input of this conversation.
export interface ConvoPending {
  id: number
  text: string
  shown?: string // (as the box had it: a picture by its label, not its path)
  state: 'sending' | 'waiting' | 'stuck'
  opened?: boolean // (a command that has opened a panel: taken in; the log tells of it once that is shut)
  quiet?: boolean // (long since, nothing in the log: said so, quietly)
  // The screen, looked at once it had not turned up: being read; or went: the
  // subagent that took it in, found in its own log (Claude Code had that
  // subagent's conversation in front as it was typed): said for sure.
  look?: { went: ConvoWent } | 'looking'
}
export type ConvoWent = { agent: string; type?: string; description?: string }
const wentOf = (p: ConvoPending) => (p.look && typeof p.look === 'object' ? p.look.went : undefined)

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
// (Of fewer turns than this, the rail would say nothing the eye does not see.)
const RAIL_MIN = 3
// (How far up, in screens, the line of what came since is gone to on coming
// back, and how long ago read; further and older, the end shown.)
const UNREAD_FAR = 4
const UNREAD_STALE_MS = 30 * 60 * 1000
// (A message's preview for the rail, made once.)
const railText = new WeakMap<ConvoEntry, string>()
const mac = onMac()

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

// An animation run only while it can be seen: on the screen, the page in
// front (data-run on the element; terminal.css pauses it without). Measured
// 11-13: the live line's band of light, scrolled out of view, still painted
// every frame — 580 paints in 5 s.
// (el: the element itself, from a callback ref — the line may be drawn anew, a div then a button,
// and the one observed must be the one shown.)
function useRunWhileSeen(el: HTMLElement | null, on: boolean) {
  useEffect(() => {
    if (!on || !el) return
    if (typeof IntersectionObserver === 'undefined') {
      el.dataset.run = ''
      return
    }
    let seen = false
    const set = () => {
      if (seen && !document.hidden) el.dataset.run = ''
      else delete el.dataset.run
    }
    const io = new IntersectionObserver((es) => {
      seen = es[es.length - 1].isIntersecting
      set()
    })
    io.observe(el)
    document.addEventListener('visibilitychange', set)
    return () => {
      io.disconnect()
      document.removeEventListener('visibilitychange', set)
      delete el.dataset.run
    }
  }, [el, on])
}

// (Where the side list was scrolled to, across the views made for each agent: see ConversationView.)
let sideScrolled = 0

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
  onResend,
  onLook,
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
  answered,
  files,
  onForkAt,
  forkWhole,
  onDraft,
  compacting,
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
  onResend?: (id: number) => void // (one a subagent took in: back in the composer, to send to this one)
  onLook?: () => void // (its terminal shown here, where it can be)
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
  answered?: boolean // (its question answered here, its list's word of it not come yet)
  files?: FileActs // (its session live: a file it names can be shown, fetched, looked at)
  forkWhole?: boolean // (all of it can be forked now: its turn over and nothing of it running, as the list says)
  onForkAt?: (at: string | undefined) => void // (a copy of it up to the end of a turn can be started: cut before at, the next message typed; none, all of it)
  onDraft?: (text: string) => void // (words put in its composer, not sent: the session card's requests of the agent)
  compacting?: boolean // (its screen says it compacts the conversation: its log silent till done)
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
  // The side list as it was scrolled: this view is made anew for each agent
  // gone to (the agents page keys it by the agent), its list with it, which
  // came back at its top — a row picked low in a long list went from under
  // the pointer. Where it was is put back before it is drawn; the one gone
  // to, where that leaves it out of sight (gone to from elsewhere), is
  // brought in, no further than needed.
  useLayoutEffect(() => {
    const el = sideEl.current
    if (!sideShown || !el) return
    el.scrollTop = sideScrolled
    const row = el.querySelector<HTMLElement>('.convo-list-row[aria-current="true"]')
    if (row) {
      const r = row.getBoundingClientRect()
      const box = el.getBoundingClientRect()
      if (r.top < box.top || r.bottom > box.bottom) row.scrollIntoView({ block: 'nearest' })
    }
    const keep = () => void (sideScrolled = el.scrollTop)
    keep()
    el.addEventListener('scroll', keep, { passive: true })
    return () => el.removeEventListener('scroll', keep)
  }, [sideShown])
  sideShownNow.current = sideShown
  const wideNow = useRef(wide)
  wideNow.current = wide
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
  const [bgCmd, setBgCmd] = useState<ConvoRunning | null>(null) // (a background command's details, open)
  // (Another agent shown: its own commands, none of the one before's open.)
  const agentOf = convo.bind ? `${convo.bind.pane}|${convo.bind.pid}|${convo.bind.start}|${convo.bind.session}` : ''
  useEffect(() => setBgCmd(null), [agentOf])
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
      const dir = switchKey(e)
      if (!dir || underDialog(e)) return
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
      const next = at < 0 ? list[dir > 0 ? 0 : list.length - 1] : list[(at + (dir > 0 ? 1 : list.length - 1)) % list.length]
      if (!next.current) next.onPick()
    }
    window.addEventListener('keydown', onKey, true)
    return () => window.removeEventListener('keydown', onKey, true)
  }, [active])
  const list = useMemo(() => items(convo.entries, working || waits, working), [convo.entries, working, waits])
  // A compaction and the summary it wrote, next to each other: one line, the
  // summary opened from it (not a line for each).
  const compactSum = useMemo(() => {
    const sum = new Map<Item, ConvoEntry>()
    const taken = new Set<Item>()
    list.forEach((it, i) => {
      const next = list[i + 1]
      const k = it.kind === 'note' ? it.e.parts[0]?.event?.kind : undefined
      if ((k === 'compact' || k === 'idle_compact') && next?.kind === 'summary') {
        sum.set(it, next.e)
        taken.add(next)
      }
    })
    return { sum, taken }
  }, [list])
  // A finished turn's way there — its runs of steps and what it said between
  // them — folded to one line over its answer, opened by a tap; not the
  // latest turn's (the one being read, or under way), not one of a single
  // line (nothing to gain). 11-13, the person's choice: before, every turn
  // showed its way there in full, a step quieter than the answer.
  const [unfolded, setUnfolded] = useState<Record<string, boolean>>({})
  const folds = useMemo(() => {
    const at = new Map<number, { key: string; label: string }>() // (the first of a fold: its line goes over it)
    const of = new Map<Item, string>()
    const starts: number[] = []
    list.forEach((it, i) => {
      if (it.kind === 'user' && !it.e.mid) starts.push(i)
    })
    starts.forEach((a, s) => {
      const b = starts[s + 1]
      if (b === undefined) return // (the latest turn: as it is)
      let ans = -1
      for (let j = b - 1; j > a; j--) if (list[j].kind === 'answer') { ans = j; break }
      if (ans < 0) return
      const way: number[] = []
      for (let j = a + 1; j < ans; j++) if (list[j].kind === 'work' || list[j].kind === 'text') way.push(j)
      if (way.length < 2) return
      const key = 'fold:' + itemKey(list[ans])
      const n = new Map<Category, number>()
      for (const j of way) {
        const it = list[j]
        if (it.kind !== 'work') continue
        for (const st of it.steps) if ('kind' in st && st.kind === 'tool' && st.tool) n.set(category(st.tool.name), (n.get(category(st.tool.name)) ?? 0) + 1)
      }
      const said = (['cmd', 'edit', 'read', 'search', 'web', 'agent', 'other'] as Category[]).filter((c) => n.get(c)).map((c) => count(c, n.get(c)!))
      at.set(way[0], { key, label: said.length ? said.join(t('convo.listSep')) : t('convo.fold.way') })
      for (const j of way) of.set(list[j], key)
    })
    return { at, of }
  }, [list])
  // (As a run's line: opened or shut by hand, else as all are — Ctrl+O.)
  const foldShut = (key: string | undefined) => !!key && !(unfolded[key] ?? allOpen)
  const heldNow = useRef({ compactSum, folds })
  heldNow.current = { compactSum, folds }
  // The rail's turns, and where each is in the list: of the same turns as
  // before, the same array (the rail not drawn again for an answer coming
  // in); each message's preview made once (review batch4).
  const railWas = useRef<{ turns: RailTurn[]; at: number[] }>({ turns: [], at: [] })
  const rail = useMemo(() => {
    const turns: RailTurn[] = []
    const at: number[] = []
    list.forEach((it, i) => {
      if (it.kind !== 'user' || it.e.mid) return
      let text = railText.get(it.e)
      if (text === undefined) railText.set(it.e, (text = previewOf(it.e.parts.filter((p) => p.kind === 'text').map((p) => p.text ?? '').join(' ').trimStart().slice(0, 2000))))
      turns.push({ key: itemKey(it), ts: it.e.ts, text })
      at.push(i)
    })
    const was = railWas.current
    const same = was.turns.length === turns.length && turns.every((x, i) => x.key === was.turns[i].key && x.text === was.turns[i].text && x.ts === was.turns[i].ts)
    railWas.current = { turns: same ? was.turns : turns, at }
    return railWas.current
  }, [list])
  const railTurns = rail.turns
  const box = useRef<HTMLDivElement>(null)
  const col = useRef<HTMLDivElement>(null)
  const stuck = useRef(true) // following the end
  const [away, setAway] = useState(false) // (not following, the end out of view: the button to it shows)
  // (Scrolled by the person just now: the buttons that float over the thread
  // keep out of the way of what is read till it rests — a phone's, see CSS.)
  const [scrolling, setScrolling] = useState(false)
  // What the person did to the scroll lately (see the listeners below):
  // only that leaves the end, or comes back to following it.
  const did = useRef({ pressed: false, downAt: -1e9, drag: false, touch: false, touchY: 0, fling: 0, touchEndAt: -1e9, top: 0, h: 0, ch: 0, set: -1, glide: false, moving: false, rest: 0 })
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
  // by a tap, or all with Ctrl+O on a Mac, as in Claude Code (see pageKeys).
  const [opened, setOpened] = useState<Record<string, boolean>>({})
  const toggle = useCallback((key: string, open: boolean) => setOpened((o) => ({ ...o, [key]: open })), [])
  const [allOpen, setAllOpen] = useState(false)
  useEffect(() => {
    if (!active) return
    const onKey = (e: KeyboardEvent) => {
      // (In a field its own: a Mac's Ctrl+O opens a line there.)
      if (!foldAllKey(e, mac) || editable(e.target) || underDialog(e) || subsNow.current) return
      e.preventDefault()
      e.stopPropagation()
      if (e.repeat) return // (held: once)
      setOpened({})
      setUnfolded({})
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
  const subsNow = useRef<Sub[] | null>(subs) // (a subagent's conversation, or a command's, in front: the thread's keys are not the thread's)
  subsNow.current = subs ?? (bgCmd ? [] : null)
  const show = useCallback((key: string, path?: string) => {
    setSubs(null)
    setBgCmd(null)
    setChanges({ key, path })
  }, [])
  const unshow = useCallback(() => setChanges(null), [])
  const openSub = useCallback((s: Sub) => {
    setChanges(null)
    setBgCmd(null)
    setSubs([s])
  }, [])
  // A background command (or a Monitor) in the same place: beside the thread,
  // not a dialog — its output under it, no second dialog over the first (11-14).
  const openCmd = useCallback((r: ConvoRunning) => {
    setChanges(null)
    setSubs(null)
    setBgCmd(r)
  }, [])
  // On a touch screen, a subagent's panel is a screen of its own: the phone's
  // back (a swipe, its key) shuts it, as it would a page, rather than leaving
  // the agent. One entry for the panel, put on when it opens; taken off when it
  // is shut otherwise — only while it is still the one on top (a page gone to
  // since has its own, not to be undone).
  const subEntry = useRef(false)
  const activeRef = useRef(active)
  activeRef.current = active
  useEffect(() => {
    const open = !!subs?.length || !!bgCmd
    if (open && !subEntry.current && active && isCoarse()) {
      history.pushState({ ...(history.state ?? {}), corralSub: true }, '')
      subEntry.current = true
    } else if (!open && subEntry.current) {
      subEntry.current = false
      if (history.state?.corralSub) history.back()
    }
  }, [subs, bgCmd, active])
  useEffect(() => {
    // (An entry still marked for a panel no longer open — the page went to
    // another agent with it open, in place of its entry: the mark taken off.)
    if (activeRef.current && !subEntry.current && history.state?.corralSub) {
      const { corralSub: _, ...rest } = history.state
      history.replaceState(rest, '')
    }
    const onPop = () => {
      if (!subEntry.current || !activeRef.current) return
      subEntry.current = false
      setSubs(null)
      setBgCmd(null)
    }
    window.addEventListener('popstate', onPop)
    return () => window.removeEventListener('popstate', onPop)
  }, [])
  // All the work going on, as the bar over the composer lists it: subagents
  // (where they can be looked into) and commands — one after another from a
  // panel, whichever it is (11-14). By the log's ids only.
  const subsOk = !convo.bind?.agent || convo.bind.agent === 'claude' || convo.bind.agent === 'codex'
  const work = (convo.running ?? []).filter((r) => (r.kind === 'agent' && isSubId(r.id) && subsOk) || r.kind === 'cmd')
  const workNav = (id: string): WorkNav | undefined => {
    const i = work.findIndex((r) => r.id === id)
    if (i < 0 || work.length < 2) return undefined
    const go = (j: number) => {
      const r = work[(j + work.length) % work.length]
      if (r.kind === 'agent') openSub({ agent: r.id, title: r.what })
      else openCmd(r)
    }
    return { i, n: work.length, prev: () => go(i - 1), next: () => go(i + 1) }
  }
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
  // (Subagents it sent off still out, as its log says: what it waits on —
  // unless a call of its own is out, a command it runs meanwhile.)
  const outCall = useMemo(() => callOut(convo.entries), [convo.entries])
  const agentsOut = working && !outCall ? (convo.running?.filter((r) => r.kind === 'agent').length ?? 0) : 0
  // (Quiet, waiting on those: the list over the composer says so in its head, not a line of the thread.)
  const waitingOn = convo.status === 'ready' && (quiet || quietLong > 0) && agentsOut > 0 ? agentsOut : 0
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
  const toUnread = useRef<{ after: number; until: number; readAt: number } | null>(null)
  // (That line further up than UNREAD_FAR screens: shown from its end, the
  // latest — read elsewhere meanwhile, another device, or a long while — a
  // button at the top to that line. Gone once it is gone to, or in view.)
  const [farUnread, setFarUnread] = useState(false)
  const farUnreadNow = useRef(false)
  farUnreadNow.current = farUnread
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
  const unfoldedNow = useRef(unfolded)
  unfoldedNow.current = unfolded
  const keepPlace = useCallback(() => {
    markRead()
    const was = shownOf.current
    // (When its end was last read, as it is left: what coming back goes by —
    // reading it again on the way back must not make that "just now", review.)
    const r = was?.of ? reads.get(was.of) : undefined
    if (was?.of && r) {
      leftRead.delete(was.of)
      leftRead.set(was.of, r.at)
      while (leftRead.size > 50) leftRead.delete(leftRead.keys().next().value!)
    }
    if (was?.of) keepAt(was.of, { gen: was.gen, stuck: stuck.current, anchors: anchors.current, noted: notedList.current, opened: openedNow.current, allOpen: allOpenNow.current, unfolded: unfoldedNow.current })
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
    setUnfolded(back ? p.unfolded : {}) // (keys are of one conversation: never another's)
    setChanges(null)
    setSubs(null)
    anchors.current = back ? p.anchors : []
    notedList.current = back ? p.noted : []
    restoring.current = back ? p : null
    toUnread.current = !back && after !== null && seen ? { after, until: Date.now() + 8000, readAt: leftRead.get(convo.of!) ?? seen.at } : null
    setFarUnread(false)
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
    if (stuck.current) toUnread.current = { after: seen.seq, until: Date.now() + 8000, readAt: (o.of && leftRead.get(o.of)) || seen.at }
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
  const [turnsShown, setTurnsShown] = useState(false) // (the panel of the person's turns in the whole log: convoTurnsPanel)
  olderPausedNow.current = olderPaused || searching || turnsShown // (the turns panel reads back on its own, as asked: not the top's reading too — review517)
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
  }, [convo.older, convo.olderCut, convo.loadingOlder, convo.of, olderPaused, searching, turnsShown])
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
  // (The turn read now, for the rail at the side: of the person's messages,
  // the last at the top quarter of the view or above it. Only while the
  // rail is shown.)
  const railAt = useMemo(railCell, [])
  const railNow = useRef(rail)
  railNow.current = rail
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
    if (farUnreadNow.current) {
      const line = c.querySelector<HTMLElement>(':scope > .convo-unread')
      const lr = line?.getBoundingClientRect()
      if (!lr || (lr.bottom > view.top && lr.top < view.bottom)) setFarUnread(false) // (in view, or gone — review)
    }
    const r = railNow.current
    const shown = r.turns.length > 0 && (el.parentElement?.querySelector<HTMLElement>(':scope > .convo-rail')?.offsetParent ?? null) !== null
    if (!shown) return
    // (Of the list, the last item begun in the view's top quarter — the
    // first in it, at least; at the end, the last of all.)
    let upTo = lo
    if (el.scrollTop + el.clientHeight >= el.scrollHeight - 4) upTo = kids.length - 1
    else while (upTo + 1 < kids.length && kids[upTo + 1].getBoundingClientRect().top < view.top + view.height / 4) upTo++
    // (The last turn at or before it: by halving.)
    let a = 0
    let b = r.at.length
    while (a < b) {
      const m = (a + b) >> 1
      if (r.at[m] <= upTo) a = m + 1
      else b = m
    }
    railAt.set(a > 0 ? r.turns[a - 1].key : '')
  }, [railAt])
  useLayoutEffect(() => {
    const el = box.current
    const c = col.current
    // (Being put back: not before its items are opened as they were, nor noted till then.)
    const r = restoring.current
    if (r && (opened !== r.opened || allOpen !== r.allOpen || unfolded !== r.unfolded)) return
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
      const below = line ? el.scrollHeight - (line.offsetTop - c.offsetTop) : 0
      // (More than a screen of it: shown from that line — unless it is far up
      // and was read long ago, then from the end, a button to it — 11-05:
      // back on an iPad three hours after reading on a Mac, it went sixteen
      // screens up. Back soon, read on from there however much came.)
      if (line && below > el.clientHeight * UNREAD_FAR && Date.now() - tu.readAt > UNREAD_STALE_MS) setFarUnread(true)
      else if (line && below > el.clientHeight) {
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
        if (!g.moving) {
          g.moving = true
          setScrolling(true)
        }
        clearTimeout(g.rest)
        g.rest = window.setTimeout(() => {
          g.moving = false
          setScrolling(false)
        }, 700)
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
      clearTimeout(g.rest)
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
  // (Brought to the top of the view, by the person's asking: not following the end then.)
  const land = useCallback(
    (to: HTMLElement) => {
      const el = box.current
      if (!el) return
      stuck.current = false
      did.current.glide = false
      setAway(true)
      el.scrollTop += to.getBoundingClientRect().top - el.getBoundingClientRect().top - 8
      did.current.set = el.scrollTop
      jumpedTo.current = to
      note()
    },
    [note],
  )
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
    land(to)
  }, [land])
  // (One of the rail's turns: gone to as jump goes to one.)
  const goTurn = useCallback(
    (key: string) => {
      upAfterOlder.current = null
      const c = col.current
      if (!c) return
      const kids = [...c.children].filter((k) => !k.hasAttribute('data-aside'))
      const to = kids[listNow.current.findIndex((it) => itemKey(it) === key)] as HTMLElement | undefined
      if (to) land(to)
    },
    [land],
  )
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
      let i = listNow.current.findIndex((it) => itemKey(it) === hit.key)
      if (i < 0) return false
      let key = hit.key
      let field = hit.field
      // (A summary told on its compaction's line: that line, opened; one in a folded turn: the turn opened.)
      const { compactSum: cs, folds: fs } = heldNow.current
      if (cs.taken.has(listNow.current[i]) && i > 0) {
        i -= 1
        key = itemKey(listNow.current[i])
        field = 'summary'
      }
      const fk = fs.of.get(listNow.current[i])
      if (fk) setUnfolded((u) => ({ ...u, [fk]: true }))
      if (listNow.current[i].kind === 'work') toggle(key, true)
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
        return kids[listNow.current.findIndex((it) => itemKey(it) === key)] as HTMLElement | undefined
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
        if (field === 'summary') {
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
      const dir = jumpKey(e)
      if (!dir || editable(e.target) || underDialog(e) || subsNow.current) return
      e.preventDefault()
      jump(dir)
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [active, jump])
  // ⌘K on a Mac where the agents are listed: their search. Elsewhere "/",
  // not while typing (the browsers there keep Ctrl+K for their own search).
  useEffect(() => {
    if (!active || !side) return
    const onKey = (e: KeyboardEvent) => {
      if (composing(e) || underDialog(e) || e.altKey) return
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
  // ⌘B (Ctrl+B elsewhere): the list beside folded or opened, as its button
  // does; where it fits beside. Typing too, not in a terminal (tmux's prefix).
  // (Folded with the focus in it: the focus to its button, not lost.)
  useEffect(() => {
    if (!active || !side) return
    const onKey = (e: KeyboardEvent) => {
      if (!sideKey(e, mac) || underDialog(e) || !wideNow.current) return
      if (e.target instanceof Element && e.target.closest('.xterm')) return
      e.preventDefault()
      const inSide = !!sideEl.current?.contains(document.activeElement)
      setFoldedState((f) => {
        local.set(SIDE_KEY, f ? '1' : '0')
        return !f
      })
      if (inSide) requestAnimationFrame(() => root.current?.querySelector<HTMLElement>('.convo-side-btn')?.focus())
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [active, side])
  // "?" where nothing is typed in: what keys there are (also in ⋮). (⌘F and
  // Ctrl+F are the browser's find; this one's search is in ⋮.)
  // (Opened from ⋮: the focus goes back to ⋮ after, the menu being gone.)
  const [keysShown, setKeysShown] = useState<'' | 'key' | 'menu'>('')
  // The session card: shut, over the thread under its button, or a sheet from the bottom (narrow, or from the menu).
  const [card, setCard] = useState<'' | 'pop' | 'sheet'>('')
  const cardBtn = useRef<HTMLButtonElement>(null)
  if (card && !active) setCard('') // (behind a terminal tab: shut)
  // (Over the thread under its button where the button is shown; else — narrow, the button hidden — a sheet.
  // Made narrow while open over the thread: a sheet then, not a card off the screen.)
  const cardMode = () => (cardBtn.current && cardBtn.current.getBoundingClientRect().width > 0 ? 'pop' : 'sheet')
  useEffect(() => {
    if (card !== 'pop') return
    const onResize = () => cardMode() === 'sheet' && setCard('sheet')
    window.addEventListener('resize', onResize)
    return () => window.removeEventListener('resize', onResize)
  }, [card])
  useEffect(() => {
    if (!active) return
    const onKey = (e: KeyboardEvent) => {
      // (Nor over a menu that is open: it would open under it.)
      if (!helpKey(e) || editable(e.target) || underDialog(e) || document.querySelector('[role="menu"]')) return
      e.preventDefault()
      setKeysShown('key')
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [active])

  // What was sent, not in the log yet (also before a new conversation's log is written).
  const pendingList = pending.map((p) => {
    const went = wentOf(p)
    return (
      <div key={p.id} className={'convo-user convo-pending' + (p.state === 'sending' ? ' is-sending' : '')}>
        <div className="convo-user-text">{p.shown ?? p.text}</div>
        <div className="convo-meta">
          <span role="status">{t(p.opened ? 'convo.sentOnly' : p.state === 'sending' ? 'convo.sending' : went ? 'convo.wentSub' : p.state === 'stuck' ? 'convo.stuck' : 'convo.sent')}</span>
          {(went || p.quiet || p.state === 'stuck') && (
            <button type="button" className="link-btn" onClick={() => onDismiss(p.id)}>{t('common.close')}</button>
          )}
        </div>
        <SentNote p={p} onEnter={onEnter && (() => onEnter(p.id))} onResend={onResend && (() => onResend(p.id))} onShowTerminal={onLook ?? onShowTerminal} />
      </div>
    )
  })
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
              {t(convo.meta?.history_partial ? 'convo.afterRewind' : convo.meta?.forked ? 'convo.atFork' : 'convo.atStart')}
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
            {folds.at.has(i) && (
              <FoldLine label={folds.at.get(i)!.label} open={!foldShut(folds.at.get(i)!.key)} onToggle={(o) => setUnfolded((u) => ({ ...u, [folds.at.get(i)!.key]: o }))} />
            )}
            {/* (Not shown, held in its place: the column's children stay one to an item, as jumps and the scroll kept count them.) */}
            {compactSum.taken.has(it) || foldShut(folds.of.get(it)) ? (
              <div className="convo-held" aria-hidden="true" />
            ) : (
              <ListItem it={it} sum={compactSum.sum.get(it)} copy={copy} open={it.kind === 'work' ? (opened[it.key] ?? allOpen) : false} onToggle={toggle} onShow={show} />
            )}
          </Fragment>
        ))}
        {pendingList}
        {working && (quiet || quietLong > 0) && !waitingOn && (
          <p className="convo-working">
            {/* (Quiet with subagents it sent off still out: it waits on them, listed below, each to be opened — not "nothing new". While it goes on at work itself, it says nothing of them.) */}
            {agentsOut > 0
              ? t('convo.waitingAgents', { n: agentsOut })
              : compacting
                ? t('convo.compacting')
                : quietLong
                ? t('convo.quietLong', { d: quietLong < 3600000 ? t('convo.for.m', { n: Math.floor(quietLong / 60000) }) : t('convo.for.h', { n: Math.floor(quietLong / 3600000) }) })
                : t('convo.quiet')}
            {/* (Long without a word: where to see what it shows meanwhile.) */}
            {quietLong > 0 && agentsOut === 0 && !compacting && (
              <button type="button" className="link-btn convo-working-act" onClick={onShowTerminal}>
                {t('convo.lookTerminal')}
              </button>
            )}
          </p>
        )}
      </>
    )

  let status = agent ? t(`convo.status.${agent.status}` as I18nKey) : ''
  let workedFor = ''
  if (working && lastUser && now >= lastUser) workedFor = duration(now - lastUser)
  if (answered && waits) status = t('convo.answered')
  if (agent?.bg) status = t(agent.bg === 'running' ? 'convo.status.bg' : agent.bg === 'stale' ? 'convo.status.bgStale' : 'convo.status.working')
  else if (agent?.cmds && agent.status !== 'blocked') status = agent.cmds.n > 1 ? t('convo.bgCmds', { n: agent.cmds.n }) : t('convo.bgCmds1')
  // (Its name as the list names its row — the workspace's or pane's name first, the
  // session's title under it — so the bar and the list say one thing: 11-13, the
  // bar had them the other way round. Where the list has no row of it, as before.)
  const rowShown = side?.rows.find((r) => r.current)
  const name = rowShown ? [rowShown.title, rowShown.where].filter(Boolean) : agent ? [agent.title, agent.where].filter(Boolean) : []
  // What is read in, as a Markdown file to keep (see convoExport).
  const toast = useToast()
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
          // (Background work ended: as its line says it, its state and exit code.)
          if (ev.kind === 'task')
            return paren(t(`convo.task.${ev.task ?? 'cmd'}.${ev.status ?? 'done'}` as Parameters<typeof t>[0]) + (ev.code ? t('convo.task.code', { n: ev.code }) : '')) + (ev.text ? ' ' + ev.text : '')
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
        const name = (title.replace(/[\\/:*?"<>|\s]+/g, '-').slice(0, 60) || 'conversation') + '-' + new Date().toISOString().slice(0, 10) + '.md'
        void saveFile(name, md).then((saved) => {
          if (saved.how === 'saved') toast(t('file.saved', { name: saved.as }))
          else if (saved.how === 'started') toast(t('file.started', { name }))
          else if (saved.how === 'failed') toast(t('file.saveFailed'), true)
        })
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
  // (A path in an answer's code span: acted on, its session live.)
  const codeSpan = useCallback((text: string) => {
    const p = files ? codePath(text) : null
    return p ? <PathCode path={p} text={text} /> : null
  }, [files])
  // (A file link of Codex's: its words shown, the path behind them acted on and copied.)
  const fileLink = useCallback((label: string, target: string) => {
    const p = files ? codePath(target) : null
    return p ? <PathCode path={p} text={label} copy={p} /> : null
  }, [files])
  // A copy of it up to the end of one of the person's turns: cut before the
  // next message they typed (one taken in during that turn is of it).
  const entriesRef = useRef(convo.entries)
  entriesRef.current = convo.entries
  const atRest = !!forkWhole
  const forkAt = useMemo(() => {
    if (!onForkAt) return undefined
    const next = (e: ConvoEntry) => {
      const all = entriesRef.current
      const i = all.findIndex((x) => x.seq === e.seq)
      return i < 0 ? null : all.slice(i + 1).find((x) => x.role === 'user' && !x.mid && x.id)?.id
    }
    return {
      go: (e: ConvoEntry) => {
        const at = next(e)
        if (at !== null) onForkAt(at)
      },
      // (The last turn is all of it: only while it is at rest — not offered
      // while it works, rather than offered and refused.)
      can: (e: ConvoEntry) => atRest || !!next(e),
    }
  }, [onForkAt, atRest])
  return (
    <ForkAtOf.Provider value={forkAt}>
    <FileActsOf.Provider value={files}>
    <CopyOf.Provider value={copy}>
    <CodeSpanOf.Provider value={codeSpan}>
    <FileLinkOf.Provider value={fileLink}>
    <div className="convo" role="region" aria-label={t('convo.title')} ref={root}>
      {sideShown && (
        <nav className="convo-side" aria-label={t('convo.agents')} style={{ width: sideWidth }} ref={sideEl}>
          <NewAgentRow />
          <AgentList rows={rows} menus query={sideQuery} onQuery={setSideQuery} search={searchAsked} onSearchLeft={() => setSearchAsked(0)} hint={!isTouchOnly} />
          {!isTouchOnly && (
            <p className="convo-side-keys">
              {t(mac ? 'convo.switchKeysMac' : 'convo.switchKeys')}
              {mac && (
                <>
                  <br />
                  {t(allOpen ? 'convo.foldKeys' : 'convo.openKeys')}
                </>
              )}
            </p>
          )}
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
              aria-keyshortcuts={mac ? 'Meta+B' : 'Control+B'}
              title={`${t(sideShown ? 'convo.foldSide' : 'convo.openSide')} (${KEY_SIDE})`}
            >
              <PanelLeft size={18} aria-hidden="true" />
            </button>
          )}
          {side && !sideShown ? (
            <button type="button" className="convo-bar-pick" aria-haspopup="dialog" aria-expanded={!!picking} aria-label={t('convo.pick')} onClick={() => setPicking(true)}>
              {label}
            </button>
          ) : (
            <span className="convo-bar-pick is-static">{label}</span>
          )}
          <span className={'convo-bar-status' + (agent && !convo.failing && !stop ? ' is-' + (agent.bg === 'running' || agent.bg === 'checking' ? 'working' : answered && waits ? 'answered' : agent.status) : '')}>
            {working && !convo.failing && !stop && <span className="convo-spin" aria-hidden="true" />}
            {convo.failing ? (
              t('convo.paused', { time: clockMs(convo.okAt) })
            ) : (
              stop || (
                <>
                  {status}
                  {/* (How long: dropped where the bar is narrow, the name needing the room.) */}
                  {workedFor && !agent?.bg && !(agent?.cmds && agent.status !== 'blocked') && <span className="convo-bar-dur"> · {workedFor}</span>}
                </>
              )
            )}
          </span>
          {convo.of && convo.status === 'ready' && (
            <button
              ref={cardBtn}
              type="button"
              className="icon-btn convo-card-btn"
              aria-expanded={card === 'pop'}
              aria-haspopup="dialog"
              onClick={() => setCard(card ? '' : cardMode())}
              aria-label={t('card.open')}
              title={t('card.open')}
            >
              <Info size={18} aria-hidden="true" />
            </button>
          )}
          {onStop && (
            <button type="button" className="btn btn-ghost convo-stop" onClick={onStop}>
              {t('convo.stop')}
            </button>
          )}
          {more && (
            <Menu
              label={t('common.more')}
              items={() => [
                // (In three groups, each headed: finding and reading here; the agent; the conversation itself.)
                { label: t('convoSearch.item'), icon: <Search size={16} />, group: t('menu.group.find'), onSelect: () => setSearching(true) },
                ...(convo.of && (convo.bind?.agent === 'claude' || convo.bind?.agent === 'codex') ? [{ label: t('turns.item'), icon: <ListOrdered size={16} />, onSelect: () => (setSearching(false), setTurnsShown(true)) }] : []),
                // (All steps opened or folded: the menu's, as Ctrl+O is a Mac's only.)
                { label: t(allOpen ? 'pageKeys.foldAll' : 'pageKeys.openAll'), icon: allOpen ? <ChevronsDownUp size={16} /> : <ChevronsUpDown size={16} />, disabled: !list.length, onSelect: () => (setOpened({}), setUnfolded({}), setAllOpen((v) => !v)) },
                ...(convo.of && convo.status === 'ready' ? [{ label: t('card.open'), icon: <Info size={16} />, onSelect: () => setCard(cardMode()) }] : []),
                { label: t('convoExport.item'), icon: <Download size={16} />, disabled: !list.length, onSelect: () => doExport() },
                // (On every device: a tablet's keyboard has these keys too.)
                { label: t('pageKeys.item'), icon: <Keyboard size={16} />, onSelect: () => setKeysShown('menu') },
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
      {card && convo.of && (
        <SessionCard
          data={cardData(convo, list, mode, effort)}
          acts={{
            onFolder: files?.open ? (dir) => files.open!(dir) : undefined,
            onChanges: (key, path) => show(key, path),
            onRunning: (id) => {
              const r = convo.running?.find((x) => x.id === id)
              if (!r) return
              if (r.kind === 'agent' && isSubId(r.id)) openSub({ agent: r.id, title: r.what })
              else openCmd(r)
            },
            onDraft,
          }}
          anchor={card === 'pop' ? cardBtn.current : null}
          sheet={card === 'sheet'}
          onClose={() => setCard('')}
        />
      )}
      {keysShown && (
        <PageKeysHelp
          side={!!side}
          onClose={() => {
            const from = keysShown
            setKeysShown('')
            if (from === 'menu') requestAnimationFrame(() => root.current?.querySelector<HTMLElement>('.convo-bar-more')?.focus())
          }}
        />
      )}
      {turnsShown && convo.of && (
        <ConvoTurnsPanel
          of={convo.of}
          gen={convo.gen}
          list={list}
          older={convo.older && !convo.olderCut ? { can: true, busy: convo.loadingOlder } : undefined}
          onOlder={() => convo.loadOlder()}
          onGo={goTo}
          onClose={() => setTurnsShown(false)}
        />
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
          <span className="convo-nudge-text">{others.length > 1 ? t('convo.othersWaitingMany', { n: others.length, m: others.length - 1, title: others[0].title }) : t('convo.othersWaiting', { n: 1, title: others[0].title })}</span>
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
        {railTurns.length >= RAIL_MIN && convo.status === 'ready' && <ConvoRail turns={railTurns} at={railAt} onGo={goTurn} />}
        {away && convo.status === 'ready' && (
          <div className={'convo-jump' + (scrolling ? ' is-moving' : '')} role="group" aria-label={t('convo.jump')}>
            <button type="button" className="icon-btn" onClick={() => jump(-1)} aria-label={t('convo.jumpPrev')} title={t('convo.jumpPrev') + (isTouchOnly ? '' : ' ([)')}>
              <ChevronUp size={16} aria-hidden="true" />
            </button>
            <button type="button" className="icon-btn" onClick={() => jump(1)} aria-label={t('convo.jumpNext')} title={t('convo.jumpNext') + (isTouchOnly ? '' : ' (])')}>
              <ChevronDown size={16} aria-hidden="true" />
            </button>
          </div>
        )}
        {farUnread && convo.status === 'ready' && (
          <button
            type="button"
            className="convo-latest convo-to-unread"
            onClick={() => {
              const el = box.current
              const line = col.current?.querySelector<HTMLElement>(':scope > .convo-unread')
              setFarUnread(false)
              if (!el || !line) return
              stuck.current = false
              did.current.glide = false
              setAway(true)
              el.scrollTop += line.getBoundingClientRect().top - el.getBoundingClientRect().top - 8
              did.current.set = el.scrollTop
              note()
            }}
          >
            <ArrowUp size={15} aria-hidden="true" />
            {t('convo.toUnread')}
          </button>
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
        {convo.status === 'ready' && convo.tasks && <Tasks tasks={convo.tasks} fold={!!ask} />}
        {/* (Asked from the screen: also where its log is not read.) */}
        {(convo.status === 'ready' || convo.status === 'none') && ask && (
          <ShowTerminal.Provider value={onShowTerminal}>
            <AskDock>{ask}</AskDock>
          </ShowTerminal.Provider>
        )}
        {/* (Not of one at rest, which would have taken them in: not when its state is not known.) */}
        {convo.status === 'ready' && agent?.status !== 'idle' && agent?.status !== 'done' && convo.queued && <Queue items={convo.queued} onSendNow={working ? onSendNow : undefined} said={sendNow} />}
        {waits && !ask && convo.status === 'ready' && (
          // (Where it cannot be answered here: in its terminal.)
          <div className="convo-waiting" role="status">
            <span>{t('convo.waiting', { agent: agentName(convo.bind?.agent ?? agent?.program) })}</span>
            <button type="button" className="btn btn-primary" onClick={onReply ?? onShowTerminal}>{t(onReply ? 'convo.answer' : 'convo.answerThere')}</button>
          </div>
        )}
        {/* (Last over the composer, which it sits on as one piece.) */}
        {convo.status === 'ready' && convo.running && (
          <RunningWork
            scanOf={active && (!convo.bind?.agent || convo.bind.agent === 'claude') ? convo.of : undefined}
            items={convo.running}
            waiting={waitingOn}
            openCmd={openCmd}
            openSub={!convo.bind?.agent || convo.bind.agent === 'claude' || convo.bind.agent === 'codex' ? openSub : undefined}
          />
        )}
        {composer}
        {convo.status === 'ready' && convo.meta && (
          <Runs
            meta={convo.meta}
            active={active}
            mode={mode}
            model={model}
            effort={effort}
            quota={
              convo.bind?.agent === 'codex'
                ? { codex: true as const, said: convo.meta.quota }
                : // The subscription's use is the account's, which this session
                  // does not draw on when its requests go elsewhere: not shown
                  // then, in place of a number that is about something else.
                  // (A key set is no such case: Claude Code asks whether to use
                  // one, and goes on with the account where that was refused.)
                  convo.of && convo.bind?.agent === 'claude' && convo.bind.session && !convo.bind.via && convo.bind.auth !== 'bedrock' && convo.bind.auth !== 'vertex'
                  ? { sid: convo.of.split(' ')[0], session: convo.bind.session }
                  : undefined
            }
            via={convo.bind?.via}
            auth={convo.bind?.auth}
          />
        )}
      </div>
      </div>
      {changes && shownEdits && <ChangesPanel key={changes.key} edits={shownEdits} focus={changes.path} onClose={unshow} active={active} onCopy={copy} />}
      {subs && subs.length > 0 && convo.of && (
        // (A subagent's messages are not the session's turns: no fork from them.)
        <ForkAtOf.Provider value={undefined}>
        <SubPanel
          key={subs[subs.length - 1].agent}
          of={convo.of}
          sub={subs[subs.length - 1]}
          depth={subs.length}
          live={(a) => !!convo.running?.some((r) => r.kind === 'agent' && r.id === a)}
          nav={subs.length === 1 ? workNav(subs[0].agent) : undefined}
          onOpen={(s) => setSubs((cur) => [...(cur ?? []), s])}
          onBack={() => setSubs((cur) => (cur && cur.length > 1 ? cur.slice(0, -1) : null))}
          onClose={() => setSubs(null)}
          active={active}
          copy={copy}
        />
        </ForkAtOf.Provider>
      )}
      {/* (Kept when the command ends — gone from the list — saying so, its output still there: review 1ad2c3d.) */}
      {bgCmd && convo.of && (
        <BgPanel
          key={bgCmd.id}
          of={convo.of}
          r={convo.running?.find((x) => x.id === bgCmd.id) ?? bgCmd}
          gone={!convo.running?.some((x) => x.id === bgCmd.id)}
          nav={workNav(bgCmd.id)}
          onClose={() => setBgCmd(null)}
          active={active}
        />
      )}
      {picking && side && <Picker rows={rows} onClose={unpick} searchFirst={picking === 'search'} />}
    </div>
    </FileLinkOf.Provider>
    </CodeSpanOf.Provider>
    </CopyOf.Provider>
    </FileActsOf.Provider>
    </ForkAtOf.Provider>
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

// What a subagent does now, and how much it has in hand, as the list opened says.
const scanNow = (a?: SubScanAgent) => (a?.tool ? (a.tool.done ? t('convo.live.thinking') : liveText(a.tool)) : '')
const scanTokens = (a?: SubScanAgent) => (a?.tokens ? t('convo.bgTokens', { n: tokens(a.tokens) }) : '')

function RunningWork({ scanOf, items, waiting, openSub, openCmd }: { scanOf?: string; items: ConvoRunning[]; waiting?: number; openSub?: (s: Sub) => void; openCmd?: (r: ConvoRunning) => void }) {
  const [open, setOpen] = useState(false)
  // (Those its subagents sent off in turn, known only from their logs.)
  const scan = useSubScan(scanOf, items.some((r) => r.kind === 'agent'), open)
  const nested = scan.filter((a) => (a.depth ?? 1) >= 2)
  const scanned = new Map(scan.map((a) => [a.id, a]))
  const many = items.length > 1 || nested.length > 0
  // (Down to one: opened again later, it starts folded as it did at first.)
  useEffect(() => {
    if (!many) setOpen(false)
  }, [many])
  const first = items[0]
  // One row: what it is (to open, where there is something to open), its
  // figures in one column at the end, what it does now under it (opened).
  const what = (label: string, onOpen: (() => void) | undefined, hint: string) =>
    onOpen ? (
      <button type="button" className="convo-running-what" title={hint} onClick={onOpen}>
        {label}
      </button>
    ) : (
      <span className="convo-running-what" title={label}>{label}</span>
    )
  const openOf = (r: ConvoRunning) =>
    r.kind === 'agent' && openSub && isSubId(r.id) ? () => openSub({ agent: r.id, title: r.what }) : r.kind === 'cmd' && (r.cmd || r.out) && openCmd ? () => openCmd(r) : undefined
  const hintOf = (r: ConvoRunning) => t(r.kind === 'agent' ? 'sub.open' : 'convo.bgDetails')
  const labelOf = (r: ConvoRunning) => r.what || t(r.kind === 'agent' ? 'convo.bgAgent' : 'convo.bgCmd')
  const figures = (a: SubScanAgent | undefined, ts?: string) => [scanTokens(a), since(ts)].filter(Boolean).join(' · ')
  const toggle = many && (
    <button type="button" className="convo-running-more" aria-expanded={open} onClick={() => setOpen(!open)}>
      <span>{t(open ? 'convo.bgLess' : 'convo.bgAll', { n: items.length + nested.length })}</span>
      {/* (Up: on the composer it opens upwards; turned down once open, the way it shuts.) */}
      <ChevronUp size={14} aria-hidden="true" />
    </button>
  )
  // (Waited on with nothing else going on: said so, here, in place of a line in the thread.)
  const sum = (
    <span className="convo-running-sum" role="status">
      {waiting ? t(waiting === 1 ? 'convo.bgWaiting1' : 'convo.bgWaiting', { n: waiting }) : t(items.length === 1 ? 'convo.bg1' : 'convo.bg', { n: items.length })}
      {nested.length > 0 && t('convo.bgNested', { n: nested.length })}
    </span>
  )
  return (
    <div className={'convo-running' + (open ? ' is-open' : '')}>
      <div className="convo-running-head">
        <Loader size={14} className="convo-running-spin" aria-hidden="true" />
        {sum}
        {!open && first && (
          <span className="convo-running-first">
            {what(labelOf(first), openOf(first), hintOf(first))}
            {first.note && <span className="convo-running-note" title={first.note}>{first.note}</span>}
            <span className="convo-running-since">{since(first.ts)}</span>
          </span>
        )}
        {toggle}
      </div>
      {/* (Kept while shut, its height to nothing, so that it opens and shuts as a
          drawer does — upwards, from the composer — not in a jump; not to be
          tabbed into while shut.) */}
      {many && (
        <div className={'convo-running-fold' + (open ? ' is-open' : '')} inert={!open}>
        <div className="convo-running-list">
          {items.map((r) => {
            const a = scanned.get(r.id)
            return (
              <div key={r.id} className="convo-running-row">
                {what(labelOf(r), openOf(r), hintOf(r))}
                <span className="convo-running-meta">{figures(a, r.ts)}</span>
                {r.note && <span className="convo-running-note" title={r.note}>{r.note}</span>}
                {scanNow(a) && <span className="convo-running-now" title={scanNow(a)}>{scanNow(a)}</span>}
              </div>
            )
          })}
          {nested.length > 0 && <div className="convo-running-label">{t('convo.bgNestedHead')}</div>}
          {nested.map((a) => (
            <div key={a.id} className="convo-running-row">
              {what(a.what || t('convo.bgAgent'), openSub ? () => openSub({ agent: a.id, title: a.what ?? '' }) : undefined, t('sub.open'))}
              <span className="convo-running-meta">{figures(a, a.ts)}</span>
              {scanNow(a) && <span className="convo-running-now" title={scanNow(a)}>{scanNow(a)}</span>}
            </div>
          ))}
        </div>
        </div>
      )}
    </div>
  )
}

// A command left running in the background: what it is for, how it goes,
// the command whole (coloured, its start shown and the rest a tap away), and
// its output — the file Claude Code writes it to, on the host, looked at from
// its end on asking (read once each time, no polling). gone: it has ended
// since the dialog opened (its output stays to be looked at).
const BG_CMD_SHUT = 8 // (lines of the command shown before "show all")
const BG_WHAT_LONG = 80 // (characters of what it is for past which the title, two lines, may not hold it)
// One way through the work going on, from whichever panel shows one of it.
type WorkNav = { i: number; n: number; prev: () => void; next: () => void }

function WorkStep({ nav }: { nav: WorkNav }) {
  return (
    <span className="convo-sub-step">
      <button type="button" className="icon-btn" onClick={nav.prev} aria-label={t('convo.workPrev')} title={t('convo.workPrev')}>
        <ChevronLeft size={18} aria-hidden="true" />
      </button>
      <button type="button" className="icon-btn" onClick={nav.next} aria-label={t('convo.workNext')} title={t('convo.workNext')}>
        <ChevronRight size={18} aria-hidden="true" />
      </button>
    </span>
  )
}

type OutRead = { text: string; size: number; cut: boolean }
const BG_OUT_EVERY = 3000 // (its output's end read again so often, while it runs and is looked at)

// A command left running in the background, or a Monitor, beside the thread
// where a subagent's conversation shows (11-14: it was a dialog, and its
// output a second dialog over it). Its command; its output's end under it,
// read again every few seconds while it runs — only while this panel is
// open, the page in sight and the command one with a file of output; once
// when it has ended; never for a Monitor, whose latest event comes with the
// list.
function BgPanel({ of, r, gone, nav, onClose, active }: { of: string; r: ConvoRunning; gone: boolean; nav?: WorkNav; onClose: () => void; active: boolean }) {
  const sid = of.split(' ')[0]
  const acts = useContext(FileActsOf)
  const toast = useToast()
  useTick(!gone && !!r.ts)
  const panel = useRef<HTMLElement>(null)
  // (Shut: the first lines; "show all" only where there is more than they hold.)
  const cmdRef = useRef<HTMLPreElement>(null)
  const [all, setAll] = useState(false)
  const [more, setMore] = useState(false)
  useLayoutEffect(() => {
    const el = cmdRef.current
    if (!el || all) return
    const measure = () => setMore(el.scrollHeight > el.clientHeight + 1)
    measure()
    if (typeof ResizeObserver === 'undefined') return
    const ro = new ResizeObserver(measure)
    ro.observe(el)
    return () => ro.disconnect()
  }, [r.cmd, all])
  // Its output's end (a path the host can read as it is: an absolute one).
  const out = r.out && r.out.startsWith('/') ? r.out : ''
  const outName = r.out ? r.out.slice(r.out.lastIndexOf('/') + 1) : ''
  const outDir = r.out ? shortPath(r.out.slice(0, r.out.lastIndexOf('/') + 1) || '/').replace(/\/?$/, '/') : ''
  const [read, setRead] = useState<OutRead | null>(null)
  const [readErr, setReadErr] = useState('')
  const [reading, setReading] = useState(false)
  const [again, setAgain] = useState(0)
  useEffect(() => {
    if (!out || !active) return
    let ctl: AbortController | null = null
    let failed = false // (not read again by itself after a failure: by hand)
    let over = false
    const readNow = () => {
      if (ctl || failed || document.hidden) return
      const c = (ctl = new AbortController())
      setReading(true)
      api<OutRead>('GET', `/api/sessions/${sid}/files/text?path=` + encodeURIComponent(out) + '&tail=1', undefined, { signal: c.signal })
        .then(
          (x) => {
            if (c.signal.aborted) return
            setRead(x)
            setReadErr('')
          },
          (e) => {
            if (c.signal.aborted) return
            failed = true
            setReadErr(readError(e))
          },
        )
        .finally(() => {
          if (ctl !== c) return
          ctl = null
          if (!over) setReading(false)
        })
    }
    readNow()
    const id = gone ? 0 : window.setInterval(readNow, BG_OUT_EVERY)
    const onSeen = () => !gone && readNow()
    document.addEventListener('visibilitychange', onSeen)
    return () => {
      over = true
      window.clearInterval(id)
      document.removeEventListener('visibilitychange', onSeen)
      ctl?.abort()
    }
  }, [sid, out, gone, active, again])
  // The output, in a box of its own under the command (kept in sight), at its
  // newest as it grows — unless the person has scrolled up in it to read.
  const outBox = useRef<HTMLPreElement>(null)
  const follow = useRef(true)
  const onScroll = () => {
    const el = outBox.current
    if (el) follow.current = el.scrollHeight - el.scrollTop - el.clientHeight < 48
  }
  useLayoutEffect(() => {
    const el = outBox.current
    if (el && read && follow.current) el.scrollTop = el.scrollHeight
  }, [read])
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
      onClose()
    }
    window.addEventListener('keydown', onKey, true)
    return () => window.removeEventListener('keydown', onKey, true)
  }, [onClose, active])
  const state = gone ? t(r.out ? 'convo.bgEnded' : 'convo.bgEndedNoOut') : r.ts ? t('convo.bgFor', { for: since(r.ts) }) : t('convo.bgRunning')
  return (
    <aside className="convo-panel convo-bg" aria-label={r.what || t('convo.bgCmds1')} tabIndex={-1} ref={panel}>
      <div className="convo-panel-head">
        <div className="convo-panel-headtext">
          <span className="convo-panel-eyebrow">{nav ? t('convo.workOf', { what: t('convo.bgCmds1'), i: nav.i + 1, n: nav.n }) : t('convo.bgCmds1')}</span>
          <h2 className="convo-panel-title is-one" title={r.what}>
            {r.what || t('convo.bgCmds1')}
          </h2>
          <span className="convo-panel-sub">
            <span>
              {!gone && <span className="bg-cmd-live" aria-hidden="true" />}
              {state}
            </span>
          </span>
        </div>
        {nav && <WorkStep nav={nav} />}
        {out && (
          <button type="button" className="icon-btn convo-sub-refresh" onClick={() => setAgain((n) => n + 1)} disabled={reading} aria-label={t('preview.reload')} title={t('preview.reload')}>
            <RotateCw size={16} className={reading ? 'convo-running-spin' : ''} />
          </button>
        )}
        <button type="button" className="icon-btn" onClick={onClose} aria-label={t('common.close')} title={t('common.close') + ' (Esc)'}>
          <X size={18} />
        </button>
      </div>
      <div className="convo-panel-body convo-bg-body">
        {/* (Longer than the title's one line holds: all of it here.) */}
        {r.what && r.what.length > BG_WHAT_LONG && <p className="bg-cmd-what">{r.what}</p>}
        {r.cmd && (
          <section className="bg-cmd-sec">
            <div className="bg-cmd-label">{t('convo.bgCmdLabel')}</div>
            <div className="bg-cmd-box">
              <pre ref={cmdRef} className={'bg-cmd-text' + (all ? '' : ' is-shut') + (more && !all ? ' is-more' : '')} style={{ ['--shut' as string]: BG_CMD_SHUT }}>
                <Code text={r.cmd} lang="bash" />
              </pre>
              <button
                type="button"
                className="icon-btn bg-cmd-copy"
                aria-label={t('common.copy')}
                title={t('common.copy')}
                onClick={async () => {
                  if (await copyText(r.cmd!)) toast(t('common.copied'))
                }}
              >
                <Copy size={15} />
              </button>
            </div>
            {(more || all) && (
              <button type="button" className="link-btn bg-cmd-all" aria-expanded={all} onClick={() => setAll(!all)}>
                {t(all ? 'convo.bgShowLess' : 'convo.bgShowAll')}
                {all ? <ChevronUp size={14} /> : <ChevronDown size={14} />}
              </button>
            )}
          </section>
        )}
        {/* (A Monitor: no file of output, its latest event instead.) */}
        {!r.out && r.note && (
          <section className="bg-cmd-sec">
            <div className="bg-cmd-label">{t('convo.bgNoteLabel')}</div>
            <p className="bg-cmd-note">{r.note}</p>
          </section>
        )}
        {r.out && (
          <section className="bg-cmd-sec">
            <div className="bg-cmd-label bg-out-head">
              <span>{t('convo.bgOutLabel')}</span>
              <span className="bg-out-file" title={r.out}>{outName}</span>
              {acts?.tail && (
                <button type="button" className="link-btn bg-out-whole" onClick={() => acts.tail!(r.out!)}>
                  {t('convo.bgOpenWhole')}
                </button>
              )}
            </div>
            {!out ? (
              // (A path the host must make whole first: shown as a file to open.)
              <button type="button" className="bg-cmd-file" onClick={acts?.tail ? () => acts.tail!(r.out!) : undefined} disabled={!acts?.tail} title={r.out}>
                <FileText size={18} className="bg-cmd-file-ic" aria-hidden="true" />
                <span className="bg-cmd-file-text">
                  <span className="bg-cmd-file-name">{outName}</span>
                  <span className="bg-cmd-file-dir">{outDir}</span>
                </span>
              </button>
            ) : readErr ? (
              <p className="bg-out-note">{readErr}</p>
            ) : !read ? (
              <p className="bg-out-note">{t('common.loading')}</p>
            ) : read.text ? (
              <pre className="bg-out" ref={outBox} onScroll={onScroll}>
                {read.cut && <span className="bg-out-cut">{t('convo.bgOutCut') + '\n'}</span>}
                {read.text}
              </pre>
            ) : (
              <p className="bg-out-note">{t('convo.bgOutEmpty')}</p>
            )}
          </section>
        )}
      </div>
    </aside>
  )
}

// Commands an agent left running in the background, said beside where it
// is: how many, and, held over, which and for how long.
export function BgCmds({ cmds }: { cmds: RowCmds }) {
  const hint = cmds.list.map((c) => [c.title || t('convo.bgCmd'), c.since ? t('convo.bgCmdFor', { for: duration(Math.max(Date.now() - c.since, 0)) }) : ''].filter(Boolean).join(' · '))
  if (cmds.n > cmds.list.length) hint.push(t('convo.bgCmdsMore', { n: cmds.n - cmds.list.length }))
  return (
    <span className="convo-list-cmds" title={hint.join('\n')}>
      {t(cmds.n === 1 ? 'convo.bgCmds1' : 'convo.bgCmds', { n: cmds.n })}
    </span>
  )
}

// How long ago, as the view says it elsewhere ("6 分 25 秒").
function since(ts?: string): string {
  const v = ts ? Date.parse(ts) : NaN
  if (!Number.isFinite(v)) return ''
  return duration(Math.max(Date.now() - v, 0))
}

// (Claude Code's placeholders for its pictures say nothing here: how many, then the words.)
function queueWords(text: string): string {
  const n = text.match(/\[Image #\d+\]/g)?.length ?? 0
  return n ? t('convo.queuedPics', { n }) + text.replace(/\[Image #\d+\]\s*/g, '') : text
}

function Queue({ items, onSendNow, said }: { items: ConvoQueued[]; onSendNow?: () => void; said?: string }) {
  // The person's own, and what another session (or the system) sent: told
  // apart (11-14) — the others' not the person's to hurry, nor counted as
  // theirs; shown as from where they came, a few lines, the whole at a tap.
  const mine = items.filter((q) => !q.note)
  const others = items.filter((q) => q.note)
  return (
    <div className="convo-queue" role="status">
      <div className="convo-queue-head">
        <span className="convo-queue-sum">{mine.length ? t('convo.queued', { n: mine.length }) : t('convo.queuedOthers', { n: others.length })}</span>
        {said ? (
          <span className="convo-queue-said">{said}</span>
        ) : (
          onSendNow &&
          mine.length > 0 && (
            <button type="button" className="btn btn-ghost convo-queue-now" onClick={onSendNow} title={t('convo.sendNowHint')}>
              {t('convo.sendNow')}
            </button>
          )
        )}
      </div>
      {mine.map((q, i) => (
        <div key={'m' + i} className="convo-queue-row">
          <div className="convo-queue-text">{queueWords(q.text)}</div>
        </div>
      ))}
      {others.map((q, i) => (
        <QueuedNote key={'n' + i} q={q} />
      ))}
    </div>
  )
}

function QueuedNote({ q }: { q: ConvoQueued }) {
  const [whole, setWhole] = useState(false)
  return (
    <button type="button" className={'convo-queue-row is-note' + (whole ? ' is-whole' : '')} aria-expanded={whole} onClick={() => setWhole(!whole)}>
      <span className="convo-queue-from">
        <MessagesSquare size={14} aria-hidden="true" />
        {q.from ? t('convo.queuedFromName', { name: q.from }) : t('convo.queuedNoticeName')}
      </span>
      <span className="convo-queue-text">{queueWords(q.text)}</span>
    </button>
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
// (One of none of these: OpenCode's agent — build, or one of the person's own — as its status line has it, capitalised.)
const modeName = (m: string) => ((MODES as readonly string[]).includes(m) ? t(`convo.mode.${m}` as I18nKey) : m.charAt(0).toUpperCase() + m.slice(1))

// Switching the permission mode from the page, to one picked from a menu
// (Shift+Tab until it is there, on the host): now, what the last switch went
// to (the log says it only with the next message); busy, one on its way.
export type ModeSwitch = { now?: string; busy: boolean; onPick?: (mode: string) => void }
// (Those Shift+Tab goes through, in its order; which a session has depends on how it was started.)
const PICKED_MODES = ['default', 'acceptEdits', 'plan', 'auto', 'bypassPermissions'] as const
// Each mode's mark, and a line of what it lets the agent do (11-14: five names to tell apart by name alone).
const MODE_ICON: Record<(typeof PICKED_MODES)[number], typeof Info> = { default: Hand, acceptEdits: FilePen, plan: ListChecks, auto: Sparkles, bypassPermissions: ShieldOff }

// Switching the model, from a menu of what Claude Code's picker offers
// (list: null while it is read), for the session only.
export type ModelOpt = { label: string; desc?: string; current?: boolean }
// (fresh: the list read a moment ago, its "current" to go by; can: it may be switched now.)
// (efforts: a Codex's, chosen with its model, its picker's second step.)
// A Codex's efforts as its picker names them (its second step), their
// words here and the word its log says ("Extra high" is xhigh).
const EFFORT_WORDS: Record<string, 'convo.effort.low' | 'convo.effort.medium' | 'convo.effort.high' | 'convo.effort.xhigh'> = {
  Low: 'convo.effort.low',
  Medium: 'convo.effort.medium',
  High: 'convo.effort.high',
  'Extra high': 'convo.effort.xhigh',
}
const effortWord = (e: string) => (e === 'Extra high' ? 'xhigh' : e.toLowerCase())
export const CODEX_EFFORTS = Object.keys(EFFORT_WORDS)

// unread: the last read of the picker came to nothing. Without it a list
// that never arrives leaves the menu saying it is still reading, for as
// long as it is left open.
// (byLog: the list is one the gateway kept, with no word of which it is on — marked by the log's model, see rowByLog.)
export type ModelSwitch = { list: ModelOpt[] | null; fresh: boolean; busy: boolean; can: boolean; unread?: boolean; byLog?: boolean; onOpen: () => void; onPick: (label: string, effort?: string) => void; efforts?: string[]; of?: string }

// How hard the agent is asked to think (Claude Code's /effort), for the
// session only. The levels are its slider's, in its order.
export const EFFORTS = ['low', 'medium', 'high', 'xhigh', 'max'] as const
// (now: what a switch took, until the log says it — an answer names the
// level, and none comes of the switch itself.)
export type EffortSwitch = { now?: string; busy: boolean; can: boolean; onPick: (level: string) => void }

// named: what to show a model as, and what is left to say of it. A picker
// names the model short and says the rest beside it, beginning with the
// full name: "Opus", "Opus 5.5 · Best for everyday, complex tasks · $4/$20
// per Mtok" (Claude Code 2.1.319). The full name is shown, the rest under
// it, so the name is not read twice; where the two do not go together
// (another agent's picker, another wording), both stay as they came. The
// name the pick is made with is o.label either way, untouched.
// rowByLog: which row of a list the log's model is, where one alone is it
// by name and version both ("Sonnet 5.5": the log's "claude-sonnet-5-5", or
// what /model said it was set to, against the row's "Sonnet 5.5 · …").
// None otherwise — another version of that family, a name of the person's
// own, a variant the id marks ("[1m]") that the rows do not tell apart, two
// rows of the same model: no row is marked rather than one guessed.
export function rowByLog(list: ModelOpt[], model: string | undefined): string | undefined {
  if (!model || model.includes('[')) return undefined
  const said = modelName(model).toLowerCase()
  const hits = list.filter((o) => named(o)[0].toLowerCase() === said)
  return hits.length === 1 ? hits[0].label : undefined
}

export function named(o: ModelOpt): [string, string] {
  const desc = o.desc ?? ''
  const separator = /\s+·\s+/.exec(desc)
  const at = separator?.index ?? -1
  const head = at > 0 ? desc.slice(0, at) : ''
  const word = o.label.split(/\s+/)[0]
  return head && !head.includes('\n') && word && head.toLowerCase().startsWith(word.toLowerCase()) ? [head, desc.slice(at + separator![0].length)] : [o.label, desc]
}
const effortName = (e: string) => ((EFFORTS as readonly string[]).includes(e) ? t(`convo.effort.${e}` as I18nKey) : e)

// What the session runs with, under the composer: its permission mode on
// the left (a tap: a menu of them); its branch, model and how
// full its context is on the right (the numbers on a tap).
// (via, auth: where its requests go and how it signs in, as its process on
// the host says — see convoBind. Shown only where that is not the model
// maker's own door: the quiet case, which is most of them, says nothing.)
function Runs({ meta, active, mode, model, effort, quota, via, auth }: { meta: ConvoMeta; active: boolean; mode?: ModeSwitch; model?: ModelSwitch; effort?: EffortSwitch; quota?: QuotaOf; via?: string; auth?: string }) {
  const [open, setOpen] = useState<false | 'ctx' | 'model' | 'effort' | 'mode'>(false) // (which of its pop-ups)
  // (Another agent bound meanwhile — the pane's program changed: its model and effort menus, of the one before, closed;
  // opened again, the list read is the new one's.)
  useEffect(() => {
    setOpen((o) => (o === 'model' || o === 'effort' ? false : o))
  }, [model?.of])
  const [effortsOf, setEffortsOf] = useState('') // (a Codex's model whose efforts show, chosen with it)
  // The model menu's choice, held while it is open: a model and how hard it
  // thinks are chosen there together and taken at once, by its button — one
  // switch, one wait (11-13; before, each click switched and closed it).
  // Cleared as it opens and closes. (going: what a switch asked for is
  // going to, said on the chip until it ends.)
  const [stage, setStage] = useState<{ model?: string; effort?: string }>({})
  const [going, setGoing] = useState('')
  const applyBtn = useRef<HTMLButtonElement>(null)
  useEffect(() => setStage({}), [open])
  useEffect(() => {
    if (!model?.busy) setGoing('')
  }, [model?.busy])
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
  const mergedNow = useRef(false)
  const moveEffort = useRef<(d: 1 | -1) => void>(() => {})
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
      } else if (open === 'model' && mergedNow.current && (e.key === 'ArrowLeft' || e.key === 'ArrowRight') && menu.current?.contains(document.activeElement)) {
        // (As in Claude Code's own picker: ← → for how hard it thinks, wherever the focus is in the list.)
        e.preventDefault()
        moveEffort.current(e.key === 'ArrowRight' ? 1 : -1)
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
  // (A narrow screen: from the bottom, over a dimmed page, as its other menus
  // are — not a box with the thread's words cut off around it. Decided as it opens.)
  const [sheet, setSheet] = useState(false)
  useLayoutEffect(() => {
    if (open) setSheet(matchMedia('(max-width: 640px)').matches)
  }, [open])
  useEffect(() => {
    if (!open) return
    const vv = window.visualViewport
    const fit = () => {
      const pop = box.current?.querySelector<HTMLElement>('.convo-runs-pop')
      if (!pop || !box.current) return
      const seen = vv ? vv.height : window.innerHeight
      if (pop.classList.contains('is-sheet')) {
        pop.style.bottom = ''
        pop.style.maxHeight = Math.round(seen * 0.8) + 'px'
        return
      }
      const at = box.current.getBoundingClientRect()
      // Each of them opens straight above this row, the composer under it
      // covered while it is open — one way for all four, where the
      // context's used to be kept clear of the composer and the three
      // menus were not. (It is the room they have: kept clear, the
      // context's details had the least room of the four and the most to
      // say.)
      pop.style.bottom = ''
      pop.style.maxHeight = Math.max(0, Math.min(at.top - (vv ? vv.offsetTop : 0), seen) - 16) + 'px'
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
  }, [open, sheet])
  // Where its requests go, when that is not the model maker's own door:
  // the host, or, for Amazon's and Google's, which have no address of their
  // own to show, their name. "" is the quiet case, most of them.
  const where = via || (auth === 'bedrock' ? 'Amazon Bedrock' : auth === 'vertex' ? 'Google Vertex AI' : '')
  // merged: the model and how hard it thinks are chosen in one menu, which
  // the effort chip opens as well. Both must be there to switch, and the
  // agent must not already choose the two together (a Codex's picker
  // does: its efforts hang under each model instead).
  const merged = !!model?.can && !!effort?.can && !model?.efforts
  // (The one it is on, as the rows tick it; and how hard it thinks now.)
  const nowOf = (o: ModelOpt) => (model!.byLog ? o.label === rowByLog(model!.list!, meta.model) : model!.fresh && model!.list!.some((x) => x.current) ? !!o.current : modelName(meta.model!).toLowerCase() === o.label.toLowerCase())
  const nowRow = model?.list && meta.model ? model.list.find(nowOf) : undefined
  const nowEffort = merged ? effort!.now || meta.effort : meta.effort?.toLowerCase()
  const toModel = stage.model ?? nowRow?.label
  const toEffort = stage.effort ?? (merged ? nowEffort : undefined)
  const toRow = model?.list?.find((o) => o.label === toModel)
  const modelMoved = !!toModel && toModel !== nowRow?.label
  const effortMoved = !!stage.effort && (model?.efforts ? effortWord(stage.effort) !== nowEffort || modelMoved : stage.effort !== nowEffort)
  const toSaid = [toRow ? named(toRow)[0] : toModel, toEffort ? (model?.efforts ? t(EFFORT_WORDS[toEffort] ?? 'convo.effort') : effortName(toEffort)) : ''].filter(Boolean).join(' · ')
  // Taken: the model and the effort in one switch (Claude Code's picker takes both
  // with one "s", a Codex's with its model); the effort alone, with the model it is
  // on; the model alone, as before (a line typed, where that is known to work).
  mergedNow.current = merged
  moveEffort.current = (d) => {
    const i = Math.max(0, EFFORTS.indexOf((toEffort ?? 'medium') as (typeof EFFORTS)[number]))
    setStage((st) => ({ ...st, effort: EFFORTS[Math.min(EFFORTS.length - 1, Math.max(0, i + d))] }))
  }
  const apply = () => {
    if (!model || model.busy || (!modelMoved && !effortMoved)) return
    setOpen(false)
    modelBtn.current?.focus()
    setGoing(toSaid)
    if (model.efforts) model.onPick(toModel!, stage.effort)
    else if (effortMoved && toModel) model.onPick(toModel, stage.effort)
    else if (effortMoved) effort?.onPick(stage.effort!)
    else model.onPick(toModel!)
  }
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
            // (Not disabled while busy, as the mode's: a button disabled with the
            // focus on it loses it — Chrome 156 a frame later, measured on a
            // real one 2026-11-11 — and the focus leaving closes the menu this
            // very click opened. What it asks for is refused while busy.)
            aria-disabled={model.busy || undefined}
            aria-busy={model.busy || undefined}
            title={t(merged ? 'convo.modelEffortSwitch' : 'convo.modelSwitch')}
            onClick={() => {
              // (Busy with a switch: not opened — what was picked from it would be let fall. One open may be closed.)
              if (model.busy && open !== 'model') return
              if (open !== 'model') model.onOpen()
              setOpen(open === 'model' ? false : 'model')
            }}
          >
            {/* Merged: the model and how hard it thinks are the one choice,
                so they are the one chip — the menu behind them is one. */}
            {going && model.busy
              ? t('convo.modelGoing', { what: going })
              : merged && meta.effort
                ? t('convo.modelEffortChip', { m: modelName(meta.model), e: effortName(effort!.now || meta.effort!) })
                : modelName(meta.model)}
            {going && model.busy ? <Loader size={12} className="convo-running-spin" aria-hidden="true" /> : <ChevronsUpDown size={12} aria-hidden="true" />}
          </button>
        ) : (
          <span className="convo-runs-item convo-runs-model" title={meta.model}>{modelName(meta.model)}</span>
        ))}
      {meta.effort && !(merged && meta.model && model) &&
        (() => {
          const level = effort?.now || meta.effort! // (what the switch took, else what the log says)
          return effort?.can || effort?.busy ? (
          <button
            ref={effortBtn}
            type="button"
            className="convo-runs-item convo-runs-effort is-switch"
            aria-expanded={open === (merged ? 'model' : 'effort')}
            aria-haspopup="menu"
            aria-label={t('convo.effort') + ': ' + effortName(level)}
            aria-disabled={effort.busy || undefined} // (not disabled: see the model's chip)
            aria-busy={effort.busy || undefined}
            title={t(merged ? 'convo.modelEffortSwitch' : 'convo.effortSwitch')}
            // (Merged: the one menu, the models read as the model chip reads them.)
            onClick={() => {
              const which = merged ? 'model' : 'effort'
              if (effort.busy && open !== which) return
              if (open !== which && merged) model!.onOpen()
              setOpen(open === which ? false : which)
            }}
          >
            {t('convo.effortShort', { e: effortName(level) })}
            <ChevronsUpDown size={12} aria-hidden="true" />
          </button>
          ) : (
            <span className="convo-runs-item convo-runs-effort" title={t('convo.effort') + ': ' + effortName(level)}>{t('convo.effortShort', { e: effortName(level) })}</span>
          )
        })()}
      {/* Where its requests go, when that is somewhere of the person's own
          choosing: the host and nothing else of the address. */}
      {where && (
        <span className="convo-runs-item convo-runs-via" title={t('convo.viaHint', { host: where })}>
          {t('convo.via', { host: where })}
        </span>
      )}
      {used > 0 && (
        <button type="button" className="convo-runs-item convo-runs-ctx" aria-expanded={open === 'ctx'} aria-label={t('convo.ctxLabel', { n: tokens(used) })} title={t('convo.ctxLabel', { n: tokens(used) })} onClick={() => setOpen(open === 'ctx' ? false : 'ctx')}>
          <Gauge part={full} />
          {tokens(used)}
        </button>
      )}
      {open && sheet && <div className="convo-runs-back" aria-hidden="true" onPointerDown={() => setOpen(false)} />}
      {open === 'model' && model && (
        <div
          className={"convo-runs-pop convo-models" + (sheet ? " is-sheet" : "")}
          role="menu"
          aria-label={t('convo.model')}
          ref={menu}
          onBlur={(e) => !box.current?.contains(e.relatedTarget as Node | null) && setOpen(false)} // (Tab out of it: closed)
        >
          {model.list === null ? (
            <p className="convo-runs-note" role="status">{t(model.unread && !model.busy ? 'convo.modelUnread' : 'convo.modelReading')}</p>
          ) : (
            model.list.map((o) => {
              // (As its picker last said, which the log may not yet. A Codex's
              // log names it in small letters: "gpt-6-sol" is "GPT-6-Sol".)
              const now = nowOf(o)
              const chosen = (toModel ?? '') === o.label
              const [name, says] = named(o)
              // (Chosen, not taken: the button under the list takes it — the focus goes there.)
              const pick = (effort?: string) => {
                setStage((st) => ({ model: o.label, effort: effort ?? (model.efforts ? undefined : st.effort) }))
                requestAnimationFrame(() => applyBtn.current?.focus())
              }
              return (
                <Fragment key={o.label}>
                  <button
                    type="button"
                    role="menuitemradio"
                    aria-checked={chosen}
                    aria-expanded={model.efforts ? effortsOf === o.label : undefined}
                    className={'convo-models-item' + (now ? ' is-now' : '')}
                    title={o.label}
                    onClick={() => (model.efforts ? (setEffortsOf(o.label), setStage({ model: o.label })) : pick())}
                  >
                    <span className="convo-models-label">
                      {name}
                      {now && !chosen && <span className="convo-models-now">{t('convo.modelNowTag')}</span>}
                    </span>
                    {says && <span className="convo-models-desc">{says}</span>}
                    {chosen && <Check size={14} aria-hidden="true" />}
                  </button>
                  {model.efforts && effortsOf === o.label && (
                    <div className="convo-models-efforts" role="group" aria-label={t('convo.effort')}>
                      {model.efforts.map((e) => (
                        <button key={e} type="button" role="menuitemradio" aria-checked={chosen && (stage.effort ? stage.effort === e : now && meta.effort?.toLowerCase() === effortWord(e))} className={'convo-models-effort' + ((chosen && (stage.effort ? stage.effort === e : now && meta.effort?.toLowerCase() === effortWord(e))) ? ' is-current' : '')} onClick={() => pick(e)}>
                          {t(EFFORT_WORDS[e] ?? 'convo.effort')}
                        </button>
                      ))}
                    </div>
                  )}
                </Fragment>
              )
            })
          )}
          {/* How hard it is asked to think, in the same menu as the model
              — as Claude Code's own picker has it (its slider sits under
              the models) and as the chat apps have come to. A Codex's
              efforts are already chosen with its model, above. */}
          {merged && effort && (
            <>
              <p className="convo-models-group" aria-hidden="true">{t('convo.effort')}</p>
              <div className="convo-models-efforts" role="group" aria-label={t('convo.effort')}>
                {EFFORTS.map((e) => (
                  <button
                    key={e}
                    type="button"
                    role="menuitemradio"
                    aria-checked={toEffort === e}
                    className={'convo-models-effort' + (toEffort === e ? ' is-current' : '') + (nowEffort === e ? ' is-now' : '')}
                    title={nowEffort === e ? t('convo.modelNowTag') : undefined}
                    onClick={() => setStage((st) => ({ ...st, effort: e }))}
                  >
                    {effortName(e)}
                  </button>
                ))}
              </div>
            </>
          )}
          {model.list !== null && (
            <div className="convo-models-foot">
              <p className="convo-runs-note">{t(merged ? 'convo.modelEffortOnly' : 'convo.modelOnly')}</p>
              <button ref={applyBtn} type="button" className="btn btn-primary convo-models-apply" disabled={model.busy || (!modelMoved && !effortMoved)} onClick={apply}>
                {modelMoved || effortMoved ? (modelMoved || model.efforts ? t('convo.applySwitch', { what: toSaid }) : t('convo.applyEffort', { e: effortName(toEffort!) })) : t('convo.applyNone')}
              </button>
            </div>
          )}
        </div>
      )}
      {open === 'mode' && mode?.onPick && (
        <div
          className={"convo-runs-pop convo-models convo-modes" + (sheet ? " is-sheet" : "")}
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
                className={'convo-models-item is-mode' + (m === 'bypassPermissions' ? ' is-risky' : '')}
                onClick={() => {
                  setOpen(false)
                  modeBtn.current?.focus()
                  mode.onPick!(m) // (the one ticked too: what is ticked may be past; there already, nothing is pressed)
                }}
              >
                {(() => {
                  const Ic = MODE_ICON[m]
                  return <Ic size={16} className="convo-mode-ic" aria-hidden="true" />
                })()}
                <span className="convo-models-label">{modeName(m)}</span>
                <span className="convo-models-desc">{t(`convo.modeDesc.${m}` as I18nKey)}</span>
                {now && <Check size={14} className="convo-mode-check" aria-hidden="true" />}
              </button>
            )
          })}
          <p className="convo-runs-note">{t('convo.modeNote')}</p>
        </div>
      )}
      {open === 'effort' && effort && (
        <div
          className={"convo-runs-pop convo-models" + (sheet ? " is-sheet" : "")}
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
        <div className={'convo-runs-pop' + (sheet ? ' is-sheet' : '')} role="dialog" aria-label={t('convo.ctxTitle')}>
          {/* Blocks of one look each (11-14: three kinds of heading, a count
              said twice, a long note under every bar): this session — how
              full, of what window, the model, its last answer — then the
              account's quota, then the usage. */}
          <section className="convo-pop-sec">
            <h3 className="convo-pop-head">{t('convo.ctxHead')}</h3>
            {windows.map((w) => {
              const p = Math.min(100, Math.round((used / w) * 100))
              return (
                <div key={w} className="convo-runs-win">
                  <div className="convo-runs-row">
                    <span>{t('convo.ctxTitle')}</span>
                    <span title={t('convo.ctxTokens', { n: used.toLocaleString(getLang()) })}>{t('convo.ctxOfShort', { n: tokens(used), w: tokens(w), p })}</span>
                  </div>
                  <div className={'convo-runs-bar' + (p >= 90 ? ' is-danger' : p >= 80 ? ' is-warn' : '')}><i style={{ width: p + '%' }} /></div>
                </div>
              )
            })}
            {/* (A word on the window only where its size is not the model's own, known.) */}
            {!(known && meta.window_by === 'model') && (
              <p className="convo-runs-note">
                {known ? t(meta.window_by === 'request' ? 'convo.ctxNoteWide' : 'convo.ctxNoteSaid', { w: tokens(known) }) : t(windows.length > 1 ? 'convo.ctxNote' : 'convo.ctxNoteBig')}
              </p>
            )}
            {meta.model && (
              <div className="convo-runs-row">
                <span>{t('convo.model')}</span>
                <span title={meta.model}>{modelName(meta.model)}</span>
              </div>
            )}
            {meta.answer && <AnswerPart answer={meta.answer} />}
          </section>
          {quota && ('codex' in quota ? <QuotaSaid quota={quota.said} /> : <QuotaPart key={quota.sid + ' ' + quota.session} sid={quota.sid} session={quota.session} />)}
          {/* The subscription's use, where it is left out because this
              session does not draw on it: said, so that its place is not
              simply empty. */}
          {!quota && where && <p className="convo-runs-note convo-pop-sec">{t('convo.viaNote', { host: where })}</p>}
          {/* (No cost read yet: nothing said — a line saying "no usage" under the context's count read as if that count were none.) */}
          {meta.cost && <CostPart cost={meta.cost} />}
        </div>
      )}
    </div>
  )
}

// The subscription's quota (5 hours, the week), as Claude Code keeps it on
// the host (plans/3c-quota.md): read when the popover opens and each minute
// while it is open (that program writes it once a minute), kept a minute
// across openings. The account's, whichever session says it.
interface Quota {
  known: boolean
  updated_at?: number // (unix seconds)
  five_hour?: { used: number; resets_at: number }
  weekly?: { used: number; resets_at: number }
}
// (Whence it is: a Claude Code session's, read from the host; a Codex
// session's, as its log said it: meta.quota.)
type QuotaOf = { sid: string; session: string } | { codex: true; said?: ConvoQuota }
const QUOTA_EVERY = 60000
type QuotaSeen = { at: number; q: Quota | 'failed'; last?: Quota; lastAt?: number; n: number }
const quotaSeen = new Map<string, QuotaSeen>()
// (One read at a time for a session, whoever asks; each numbered, so that
// one answered late never stands over a later one.)
const quotaAsked = new Map<string, Promise<QuotaSeen>>()
let quotaN = 0
function askQuota(sid: string, session: string, key: string): Promise<QuotaSeen> {
  const going = quotaAsked.get(key)
  if (going) return going
  const n = ++quotaN
  const keep = (v: Omit<QuotaSeen, 'n'>) => {
    const was = quotaSeen.get(key)
    if (!was || was.n < n) {
      quotaSeen.set(key, { ...v, n })
      if (quotaSeen.size > 50) quotaSeen.delete(quotaSeen.keys().next().value!)
    }
    return quotaSeen.get(key)!
  }
  const p = api<Quota>('GET', `/api/sessions/${encodeURIComponent(sid)}/conversation/quota?session=${encodeURIComponent(session)}`, undefined, { timeoutMs: 20000 })
    .then(
      (q) => keep({ at: Date.now(), q, last: q.known ? q : undefined, lastAt: q.known ? Date.now() : undefined }),
      // (Failed: what was read before, as it is now, still shown, said to be of then.)
      () => keep({ at: Date.now(), q: 'failed', last: quotaSeen.get(key)?.last, lastAt: quotaSeen.get(key)?.lastAt }),
    )
    .finally(() => quotaAsked.delete(key))
  quotaAsked.set(key, p)
  return p
}

// (Keyed by its session where it is drawn: another session, another one.)
function QuotaPart({ sid, session }: { sid: string; session: string }) {
  useRedraw(30000)
  const key = sid + ' ' + session
  const [seen, setSeen] = useState(() => quotaSeen.get(key))
  useEffect(() => {
    let live = true
    // (Opened: what was read in the last minute, if any; each minute after, read.)
    const look = (opened: boolean) => {
      const was = quotaSeen.get(key)
      if (opened && was && was.q !== 'failed' && Date.now() - was.at < QUOTA_EVERY) return setSeen(was)
      void askQuota(sid, session, key).then((v) => live && setSeen(v))
    }
    look(true)
    const timer = setInterval(() => look(false), QUOTA_EVERY)
    return () => {
      live = false
      clearInterval(timer)
    }
  }, [key, sid, session])
  const shown = !seen ? undefined : seen.q === 'failed' ? seen.last : seen.q.known ? seen.q : undefined
  const now = Date.now() / 1000
  const stale = shown?.updated_at && now - shown.updated_at > 1200 ? Math.round((now - shown.updated_at) / 60) : 0
  return (
    <QuotaView shown={shown}>
      {!seen && <p className="convo-runs-note">{t('quota.reading')}</p>}
      {seen && !shown && <p className="convo-runs-note">{t(seen.q === 'failed' ? 'quota.failed' : 'quota.none')}</p>}
      {seen?.q === 'failed' && shown && <p className="convo-runs-note">{t('quota.failedKept', { at: quotaClock((seen.lastAt ?? 0) / 1000) })}</p>}
      {!!stale && <p className="convo-runs-note">{t('quota.stale', { n: stale })}</p>}
    </QuotaView>
  )
}

// A Codex session's quota, as its log last said it (each request's
// token_count): nothing read, its time said when it is not of just now.
function QuotaSaid({ quota }: { quota?: ConvoQuota }) {
  useRedraw(30000)
  const shown = quota && (quota.five_hour || quota.weekly) ? quota : undefined
  const now = Date.now() / 1000
  return (
    <QuotaView shown={shown}>
      {!shown && <p className="convo-runs-note">{t('quota.noneCodex')}</p>}
      {!!shown?.updated_at && now - shown.updated_at > 600 && <p className="convo-runs-note">{t('quota.saidAt', { at: quotaClock(shown.updated_at) })}</p>}
    </QuotaView>
  )
}

// useRedraw: drawn again each so often while shown (a quota's window reset,
// its figures grown old, are said so with nothing else changing).
function useRedraw(ms: number) {
  const [, tick] = useState(0)
  useEffect(() => {
    const timer = setInterval(() => tick((n) => n + 1), ms)
    return () => clearInterval(timer)
  }, [ms])
}

function quotaClock(sec: number) {
  const d = new Date(sec * 1000)
  const time = hhmm(d)
  // (Another day: its date and weekday apart, "11月2日 周一 16:43". The
  // month short: spelt out, an English date ran the line it is on to two
  // — "resets November 15 Sun 22:59" — while "11月15日" never did.)
  return d.toDateString() === new Date().toDateString() ? time : [d.toLocaleDateString(getLang(), { month: 'short', day: 'numeric' }), d.toLocaleDateString(getLang(), { weekday: 'short' }), time].join(' ')
}

// resetClock: when a window of the quota comes back, as a line that fits
// beside "19% used ·". A window runs five hours or a week, so within the
// week ahead the weekday says it ("Sun 22:59") and the date would only
// make the line too long to fit — which is how it read before, wrapped
// under itself. Further off than that (nothing in a quota should be), the
// date as well.
function resetClock(sec: number) {
  const d = new Date(sec * 1000)
  const time = hhmm(d)
  if (d.toDateString() === new Date().toDateString()) return time
  const week = d.toLocaleDateString(getLang(), { weekday: 'short' })
  return sec * 1000 - Date.now() < 7 * 86400000 ? [week, time].join(' ') : [d.toLocaleDateString(getLang(), { month: 'short', day: 'numeric' }), week, time].join(' ')
}

// The two windows' use, bars at 80% and 95% coloured; then what is said of it.
function QuotaView({ shown, children }: { shown?: { five_hour?: ConvoQuotaWindow; weekly?: ConvoQuotaWindow }; children?: ReactNode }) {
  const clock = resetClock
  const now = Date.now() / 1000
  return (
    <section className="convo-pop-sec convo-quota">
      <h3 className="convo-pop-head">{t('quota.title')}</h3>
      {shown &&
        (
          [
            ['quota.fiveHour', shown.five_hour],
            ['quota.weekly', shown.weekly],
          ] as const
        ).map(([name, w]) =>
          !w ? null : (
            <div key={name} className="convo-runs-win">
              <div className="convo-runs-row">
                <span>{t(name)}</span>
                <span>{w.resets_at <= now ? t('quota.reset') : t('quota.used', { n: Math.round(w.used), at: clock(w.resets_at) })}</span>
              </div>
              {w.resets_at > now && (
                <div className={'convo-runs-bar' + (w.used >= 95 ? ' is-danger' : w.used >= 80 ? ' is-warn' : '')}>
                  <i style={{ width: Math.min(100, w.used) + '%' }} />
                </div>
              )}
            </div>
          ),
        )}
      {children}
    </section>
  )
}

// How the session's latest answer went, of the log's own rows: the tokens
// the model wrote, how long it took, and the rate of the two — which is
// what says whether the address a session's requests go to is a quick one.
// Where the log did not say how long, the tokens alone: a rate worked out
// of a time nobody said would be made up.
function AnswerPart({ answer }: { answer: ConvoAnswer }) {
  const secs = answer.ms && answer.ms > 0 ? answer.ms / 1000 : 0
  const rate = secs > 0 ? Math.round(answer.out / secs) : 0
  // (Under ten seconds to a tenth: most answers are that short, and whole
  // seconds there would round a rate by a fifth.)
  const s = secs >= 10 ? Math.round(secs).toLocaleString(getLang()) : secs.toFixed(1)
  const said = { o: answer.out.toLocaleString(getLang()), s, r: rate.toLocaleString(getLang()) }
  return (
    <div className="convo-runs-row">
      <span>{t('convo.lastAnswer')}</span>
      <span title={t(rate > 0 ? 'convo.answerHint' : 'convo.answerHintOut', said)}>
        {rate > 0 ? t('convo.answerRate', { o: tokens(answer.out), s, r: rate }) : t('convo.answerOut', { o: tokens(answer.out) })}
      </span>
    </div>
  )
}

// What the session used, as Claude Code's log last said it (cost-state):
// its tokens by model and its estimated cost, with when that was said.
function CostPart({ cost }: { cost?: ConvoCost }) {
  if (!cost) return <p className="convo-runs-note convo-pop-sec">{t('convo.costNone')}</p>
  const d = cost.at ? new Date(cost.at) : null
  const at = !d ? '' : (d.toDateString() === new Date().toDateString() ? '' : d.toLocaleDateString(getLang(), { month: 'numeric', day: 'numeric' }) + ' ') + hhmm(d)
  const money = (n: number) => '$' + (n < 10 ? n.toFixed(2) : n.toFixed(1))
  return (
    <section className="convo-pop-sec convo-cost">
      <h3 className="convo-pop-head">
        <span>{t('convo.costTitle')}</span>
        <span className="convo-cost-total">{money(cost.usd)}</span>
      </h3>
      {at && <p className="convo-runs-note convo-cost-at">{t('convo.costAt', { at })}</p>}
      {(cost.models ?? []).map((m) => (
        <div key={m.model} className="convo-cost-model">
          <div className="convo-runs-row">
            <span className="convo-cost-name" title={m.model}>{costModelName(m.model)}</span>
            {/* (One model: its cost is the total, said once.) */}
            {(cost.models ?? []).length > 1 && <span>{money(m.usd ?? 0)}</span>}
          </div>
          <div className="convo-cost-tokens">
            {[t('convo.costIn', { n: tokens(m.in ?? 0) }), t('convo.costOut', { n: tokens(m.out ?? 0) }), t('convo.costRead', { n: tokens(m.cache_read ?? 0) }), t('convo.costWrite', { n: tokens(m.cache_write ?? 0) })].map((x) => (
              <span key={x}>{x}</span>
            ))}
          </div>
        </div>
      ))}
      {(!!cost.added || !!cost.removed) && (
        <div className="convo-runs-row">
          <span>{t('convo.costLines')}</span>
          <span>
            +{(cost.added ?? 0).toLocaleString(getLang())} −{(cost.removed ?? 0).toLocaleString(getLang())}
          </span>
        </div>
      )}
      {cost.unknown && <p className="convo-runs-note">{t('convo.costUnknown')}</p>}
      <p className="convo-runs-note">{t('convo.costNote')}</p>
    </section>
  )
}

// A model's id as a gateway names it ("group/auto-claude-opus-5-5"): its
// Claude name where one is in it, the whole on hover.
function costModelName(id: string): string {
  const m = /claude-[a-z]+-\d+(?:-\d+)*(?:\[1m\])?/i.exec(id)
  return modelName(m ? m[0] : id.slice(id.lastIndexOf('/') + 1))
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

// The time on a row of the list, short: how long it has waited or worked;
// else when it came to rest — the clock's time today, the date before.
function stamp(r: ConvoListRow, now: number): string {
  // (Waiting or at work: how long, short — its colour and group say which; the room is the title's.)
  if (r.status === 'working' || r.status === 'blocked') {
    if (!r.since || now < r.since) return ''
    const m = Math.floor((now - r.since) / 60000)
    return m < 1 ? t('convo.short.now') : m < 60 ? t('convo.short.m', { n: m }) : m < 48 * 60 ? t('convo.short.h', { n: Math.floor(m / 60) }) : t('convo.short.d', { n: Math.floor(m / 1440) })
  }
  if (!r.since || now < r.since) return ''
  const d = new Date(r.since)
  if (r.since >= dayStart(now)) return hhmm(d)
  // (Before today: its date, short — never a word wider than a time.)
  return d.toLocaleDateString(getLang(), { month: 'numeric', day: 'numeric' })
}



// placed: where rows go (their group, their order) by another status than
// their own, by key (the picker's: as when it opened).
// query, onQuery: what it is searched for, where that is kept outside it
// (else its own).
// search: its search box shown whatever the count (asked for by ⌘K) and
// the focus put in it; onSearchLeft: the box left, empty (shown by the
// count again); hint: the key that asks for it, said in the box.
// (showSearch: shown whatever the count, the focus left to who opened it.)
// The side list's groups of old ones opened out: kept on this device, so a
// reload keeps them so.
const SIDE_OPEN_KEY = 'corral.convo.sideOpen'

export function AgentList({ rows, menus, onPicked, placed, query: given, onQuery, search, showSearch, onSearchLeft, hint }: { rows: ConvoListRow[]; menus?: boolean; onPicked?: () => void; placed?: Map<string, ConvoListRow>; query?: string; onQuery?: (q: string) => void; search?: number; showSearch?: boolean; onSearchLeft?: () => void; hint?: boolean }) {
  // Its rows' menu (menus: the side list's; the switcher's rows have none):
  // one for the list, of the row it was opened on by its key, as that row
  // may go to another group meanwhile and be drawn anew; at its "more"
  // button, or where it was right-clicked. Gone with the row, or when the
  // row is no longer shown (searched past, folded away).
  const [rowMenu, setRowMenu] = useState<{ key: string; anchor: DOMRect; at: 'below' | 'point'; sheet: boolean } | null>(null)
  const menuRow = rowMenu ? rows.find((r) => r.key === rowMenu.key && r.menu) : undefined
  useEffect(() => {
    if (rowMenu && (!menuRow || !dotsOf(rowMenu.key))) setRowMenu(null)
  })
  const dotsOf = (key: string) => document.querySelector<HTMLElement>(`.convo-list-dots[data-key="${CSS.escape(key)}"]`)
  const closeMenu = useCallback(() => setRowMenu(null), [])
  const menuTrigger = useCallback(() => (rowMenu ? dotsOf(rowMenu.key) : null), [rowMenu])
  const openMenu = (key: string, at?: { x: number; y: number }) => {
    const b = dotsOf(key)
    if (!at && !b) return
    // (Its button the one the focus goes back to, a sheet's too: a tap
    // focuses nothing on some systems, the focus elsewhere meanwhile.)
    if (document.activeElement !== b) b?.focus({ preventScroll: true })
    setRowMenu({ key, anchor: at ? new DOMRect(at.x, at.y, 0, 0) : b!.getBoundingClientRect(), at: at ? 'point' : 'below', sheet: isCoarse() })
  }
  // (Opened from the keyboard, its menu key or Shift+F10: at the button; the
  // contextmenu that may follow it, while that menu is open, is the same
  // asking.)
  const keyOpened = useRef<{ key: string; at: number } | null>(null)
  const menuKey = (key: string) => (e: React.KeyboardEvent) => {
    if (e.key !== 'ContextMenu' && !(e.key === 'F10' && e.shiftKey)) return
    e.preventDefault()
    keyOpened.current = { key, at: Date.now() }
    openMenu(key)
  }
  // Something picked from it: the focus put on its button first (a dialog
  // it opens gives it back there), and onto that button again where it was
  // drawn anew meanwhile with the focus left nowhere (pinned: its row in
  // another group); on the first row where it is gone (closed). Looked at
  // once what was picked is done: as the list is drawn, and every 150 ms
  // while a dialog it opened is open (wherever the focus is meanwhile, its
  // button greyed as it asks, its backdrop pressed; shut, it need not draw
  // the list again).
  const refocus = useRef<{ key: string; el: HTMLElement | null; timer: number; dialog?: Element } | null>(null)
  const settle = useCallback(() => {
    const f = refocus.current
    if (!f) return
    // (The dialog it opened: the first seen since, none being open as it
    // was picked; waited for while that one is, not another after it.)
    f.dialog ??= modalNow() ?? undefined
    if (f.dialog?.isConnected) return
    // (Its button where it was: nothing to do, the focus there or gone on
    // since.)
    forget()
    const a = document.activeElement
    if (!f.el?.isConnected && (!a || a === document.body) && !modalNow()) (dotsOf(f.key) ?? document.querySelector<HTMLElement>('.convo-side .convo-list-row'))?.focus({ preventScroll: true })
  }, [])
  const forget = () => {
    if (refocus.current) clearInterval(refocus.current.timer)
    refocus.current = null
  }
  useEffect(settle)
  useEffect(() => forget, [])
  const menuItems = (r: ConvoListRow) =>
    (r.menu!().filter(Boolean) as MenuItem[]).map((it): MenuItem => ({
      ...it,
      onSelect: it.onSelect && (() => {
        const el = dotsOf(r.key)
        el?.focus({ preventScroll: true })
        forget()
        refocus.current = { key: r.key, el, timer: window.setInterval(settle, 150) }
        it.onSelect!()
      }),
    }))
  // (The times move on: drawn again every half minute.)
  const [now, setNow] = useState(Date.now())
  useEffect(() => {
    const timer = setInterval(() => setNow(Date.now()), 30000)
    return () => clearInterval(timer)
  }, [])
  // Which of the long groups are opened out (by their name; shut again at
  // a tap).
  // (Groups opened out stay so as another agent is gone to, and across a reload: SIDE_OPEN_KEY.)
  const [open, setOpenNow] = useState<string[]>(() => keptNames(SIDE_OPEN_KEY))
  // (Rows regrouped by a poll slide to their place; a search's are its own — useFlip.)
  const groupsEl = useRef<HTMLDivElement>(null)
  const setOpen = (f: (o: string[]) => string[]) =>
    setOpenNow((o) => {
      const now = f(o)
      keepNames(SIDE_OPEN_KEY, now)
      return now
    })
  // What it is searched for (by its title, where it is, its program): the
  // rest goes, and no group is folded.
  const [own, setOwn] = useState('')
  const query = given ?? own
  const setQuery = onQuery ?? setOwn
  // (Not a group opened or shut by a tap, nor a search typed: the change is the person's own.)
  useFlip(groupsEl, query.trim() + '|' + open.join())
  // (Held as it was when the switcher opened: its status and time; whether it
  // is pinned, as it is now, another tab or device may change it meanwhile.)
  const at = (r: ConvoListRow) => {
    const p = placed?.get(r.key)
    return p ? { ...p, pinned: r.pinned } : asHeld(r)
  }
  // (Placed: in their order then, ties too; new ones after.)
  const first = placed ? [...placed.keys()] : []
  const place = (r: ConvoListRow) => (first.includes(r.key) ? first.indexOf(r.key) : first.length)
  const found = query.trim() ? rows.filter((r) => matchQuery(query, r.title, r.where, r.program)) : rows
  const inOrder = placed ? [...found].sort((a, b) => place(a) - place(b)) : found
  const groups = GROUPS.flatMap((g) => {
    const of = inGroup(g, inOrder.map(at))
    if (g.name !== 'convo.group.recent') return [{ name: g.name, of }]
    // (At rest: one group, latest first — its time says which day — not a
    // heading a day for a row or two; those of no known time last.)
    return [
      { name: 'convo.group.idle' as I18nKey, of: of.filter((p) => (p.since ?? 0) > 0) },
      { name: 'convo.group.other' as I18nKey, of: of.filter((p) => (p.since ?? 0) <= 0) },
    ]
  })
  return (
    <>
      {(rows.length > SEARCH_FROM || query || !!search || showSearch) && <ListSearch value={query} onChange={setQuery} focus={search} onLeft={onSearchLeft} hint={hint ? KEY_SEARCH : undefined} />}
      {query.trim() && !found.length && <p className="list-search-none">{t('convo.searchNone')}</p>}
      <div className="convo-list-groups" ref={groupsEl}>
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
            <div className="convo-list-head" data-flip={'g:' + g.name} data-flip-fade="">
              {t(g.name)}
              <span>{all.length}</span>
            </div>
            {of.map((r) => {
              // (At rest, nothing to say of it but when.)
              const said = r.status === 'blocked' || r.status === 'working' || r.status === 'done'
              const since = stamp(r, now)
              // (Its turn over, work of its left going — subagents, commands —: said in
              // words by its time, as its hollow dot alone did not say it; what goes on,
              // in its tooltip. As the dot: running, or commands, not while it waits.)
              const bgSaid = r.bg === 'running' || (r.cmds && r.status !== 'blocked') ? 'bg' : r.bg === 'stale' ? 'stale' : ''
              const bgHint = !bgSaid
                ? ''
                : [r.bg === 'running' ? t('convo.list.bgAgents') : r.bg === 'stale' ? t('convo.status.bgStale') : '', r.cmds?.list.length ? t('convo.list.bgCmds', { list: r.cmds.list.map((c) => c.title || '?').join(t('convo.listSep')) }) : '']
                    .filter(Boolean)
                    .join('\n')
              const hasMenu = !!(menus && r.menu)
              return (
                <div
                  key={r.key}
                  data-flip={'r:' + r.key}
                  className={'convo-list-item' + (hasMenu ? ' has-menu' : '') + (rowMenu?.key === r.key ? ' is-menu' : '')}
                  onContextMenu={
                    hasMenu
                      ? (e) => {
                          e.preventDefault()
                          const k = keyOpened.current
                          if (k && k.key === r.key && rowMenu?.key === r.key && Date.now() - k.at < 1000) return
                          openMenu(r.key, { x: e.clientX, y: e.clientY })
                        }
                      : undefined
                  }
                >
                <button
                  type="button"
                  className={'convo-list-row is-' + r.status}
                  aria-current={r.current || undefined}
                  onClick={() => {
                    onPicked?.()
                    r.onPick() // (of the one shown: nothing, or the end of a switch to another; the list may be of before)
                  }}
                  onKeyDown={hasMenu ? menuKey(r.key) : undefined}
                >
                  <AgentIcon program={r.program} size={30} dot={r.bg === 'running' || r.cmds ? 'bg' : said ? (r.status as 'blocked' | 'working' | 'done') : undefined} />
                  {/* (Two lines a row, always: its name; where it is — the session's title,
                      which tells alike names apart — and when. A third only for what
                      waits on the person: what it asks, or messages not seen taken in.
                      The whole of each in its tooltip and the conversation's bar.) */}
                  <span className="convo-list-text" title={[r.title, r.where, r.now, bgHint].filter(Boolean).join('\n')}>
                    <span className="convo-list-line">
                      <span className="convo-list-title">{r.title}</span>
                      {pinned(r) && g.name !== 'convo.group.pinned' && (
                        <span className="convo-list-pin">
                          <Pin size={12} aria-hidden="true" />
                          <span className="sr-only">{t('convo.pinnedRow')}</span>
                        </span>
                      )}
                    </span>
                    <span className="convo-list-meta">
                      <span className="convo-list-where">{r.where || '\u00a0'}</span>
                      {(since || bgSaid) && (
                        <span className="convo-list-when">
                          {bgSaid && <span className={'convo-list-bg' + (bgSaid === 'stale' ? ' is-stale' : '')}>{t(bgSaid === 'bg' ? 'convo.list.bg' : 'convo.list.bgStale')}</span>}
                          {bgSaid && since && ' · '}
                          {since}
                        </span>
                      )}
                    </span>
                    {/* (Its status by its dot only, and its group: in words for a screen
                        reader, and shown where the system's colours take the dot's away.) */}
                    {/* (Commands it left running: at work in the background, as its group says — review batch4.) */}
                    {(said || r.bg || r.cmds) && <span className={'convo-list-status is-' + (r.cmds && r.status !== 'blocked' ? 'working' : r.status)}>{t(r.bg === 'running' ? 'convo.status.bg' : r.bg === 'stale' ? 'convo.status.bgStale' : r.cmds && r.status !== 'blocked' ? 'convo.status.bg' : (`convo.status.${r.status}` as I18nKey))}</span>}
                    {r.status === 'blocked' && r.now && <span className="convo-list-now is-blocked">{r.now}</span>}
                  </span>
                </button>
                {hasMenu && (
                  <button
                    type="button"
                    className="convo-list-dots"
                    data-key={r.key}
                    aria-label={t('convo.rowMenu', { name: r.title })}
                    aria-haspopup="menu"
                    aria-expanded={rowMenu?.key === r.key}
                    title={t('common.more')}
                    onClick={() => (rowMenu?.key === r.key ? closeMenu() : openMenu(r.key))}
                    onKeyDown={menuKey(r.key)}
                  >
                    <MoreHorizontal size={16} aria-hidden="true" />
                  </button>
                )}
                </div>
              )
            })}
            {(folds || (long && !shut)) && (
              <button
                type="button"
                className="convo-list-more"
                data-flip={'m:' + g.name}
                aria-expanded={!folds}
                onClick={() => setOpen((o) => (folds ? [...o, g.name] : o.filter((n) => n !== g.name)))}
              >
                {folds ? t('convo.group.more', { n: rest }) : t('convo.group.less')}
              </button>
            )}
          </div>
        )
      })}
      </div>
      {rowMenu && menuRow && (
        <MenuLayer label={t('convo.rowMenu', { name: menuRow.title })} anchor={rowMenu.anchor} at={rowMenu.at} sheet={rowMenu.sheet} trigger={menuTrigger} items={menuItems(menuRow)} onClose={closeMenu} />
      )}
    </>
  )
}

// (A dialog open over the page.)
const modalNow = () => document.querySelector('[role="dialog"][aria-modal="true"]')

// A list of agents longer than this has a search box over it.
export const SEARCH_FROM = 5

// The box an agents' list is searched with (see matchQuery). Escape in it
// empties it first.
// (⌘K on a Mac; elsewhere "/", as the browsers there keep Ctrl+K for their
// own search: as the key is said.)
const KEY_SEARCH = mac ? '⌘K' : '/'
const KEY_SIDE = mac ? '⌘B' : 'Ctrl+B'

// What the session card shows of the conversation as the page has it: the
// files its answers changed (by the log, of what is loaded: each with the
// latest answer that changed it, whose changes panel opens on it), what runs,
// its tasks, its context (its window only where known: past the window, not
// that one), its model, effort and mode as they are now (a switch's, before
// the log says it).
function cardData(convo: Convo, list: Item[], mode?: ModeSwitch, effort?: EffortSwitch): CardData {
  const [sid, pane = ''] = (convo.of ?? '').split(' ')
  const files = new Map<string, { path: string; add: number; del: number; key: string }>()
  let add = 0
  let del = 0
  for (const it of list) {
    if (it.kind !== 'answer' || !it.edits) continue
    for (const f of it.edits.files) {
      const was = files.get(f.path)
      files.set(f.path, { path: f.path, add: (was?.add ?? 0) + f.add, del: (was?.del ?? 0) + f.del, key: it.key })
      add += f.add
      del += f.del
    }
  }
  const meta = convo.meta ?? ({} as ConvoMeta)
  const used = meta.context ?? 0
  const eff = effort?.now || meta.effort
  const md = mode?.now || meta.mode
  return {
    sid,
    pane,
    edits: files.size ? { files: [...files.values()].reverse(), add, del } : null,
    running: convo.running ?? [],
    tasks: convo.tasks?.length ? convo.tasks : null,
    ctx: used ? { used, window: meta.window && used <= meta.window ? meta.window : undefined } : null,
    model: meta.model ? modelName(meta.model) : undefined,
    effort: eff ? effortName(eff) : undefined,
    mode: md ? modeName(md) : undefined,
  }
}

// The keys of this page, as this computer has them (plans/4a-shortcuts.md);
// side: the list of agents beside it, with its own.
function PageKeysHelp({ onClose, side }: { onClose: () => void; side: boolean }) {
  // (Each key in a box of its own; two that do the same, or go each way, apart: not a chord.)
  // In groups by where they act, each a heading over its rows.
  type Row = [string[], I18nKey]
  const groups: [I18nKey, Row[]][] = [
    ...(side ? ([['pageKeys.gAgents', [[mac ? ['⌥↑', '⌥↓'] : ['Alt+↑', 'Alt+↓'], 'pageKeys.switch'], [[KEY_SEARCH], 'pageKeys.search'], [[KEY_SIDE], 'pageKeys.side']]]] as [I18nKey, Row[]][]) : []),
    ['pageKeys.gConvo', [[['[', ']'], 'pageKeys.jump'], ...(mac ? ([[['Ctrl+O'], 'pageKeys.fold']] as Row[]) : []), [['?'], 'pageKeys.help']]],
    [
      'pageKeys.gInput',
      [
        [mac ? ['Return', '⌘Return'] : ['Enter', 'Ctrl+Enter'], 'pageKeys.send'],
        [['Shift+' + (mac ? 'Return' : 'Enter')], 'pageKeys.newline'],
        [['↑', '↓'], 'pageKeys.history'],
        [['Tab'], 'pageKeys.suggestion'],
        [['Esc'], 'pageKeys.stop'],
      ],
    ],
    ['pageKeys.gCard', [[['1–9', 'Esc'], 'pageKeys.card']]],
  ]
  return (
    <Dialog title={t('pageKeys.title')} onClose={onClose} hideTitle className="dlg page-keys-dialog">
      <DialogHead icon={<Keyboard size={16} />} title={t('pageKeys.title')} onClose={onClose} />
      <div className="dlg-body">
        {groups.map(([g, rows]) => (
          <section key={g} className="page-keys-group">
            <h3 className="page-keys-head">{t(g)}</h3>
            <dl className="page-keys">
              {rows.map(([k, what]) => (
                <div key={what} className="page-keys-row">
                  <dt>{t(what)}</dt>
                  <dd>
                    {k.map((x, i) => (
                      <Fragment key={x}>
                        {i > 0 && <span className="page-keys-or">/</span>}
                        <kbd>{x}</kbd>
                      </Fragment>
                    ))}
                  </dd>
                </div>
              ))}
            </dl>
          </section>
        ))}
        <p className="page-keys-note">{t(mac ? 'pageKeys.noteMac' : 'pageKeys.note')}</p>
      </div>
    </Dialog>
  )
}

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
// line (how far, what now) that opens to all of it. Folded at first on
// every screen: the line says what is under way; the thread keeps the room.
// fold: a question is being answered over it: one line, unless opened then.
function Tasks({ tasks, fold }: { tasks: ConvoTask[]; fold?: boolean }) {
  const [opened, setOpened] = useState(false)
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
              {/* (Its state is in its look: said in words too; the one under way, to all.) */}
              {x.status === 'in_progress' ? (
                <span className="convo-tasks-now">{' · ' + t('convo.taskDoing')}</span>
              ) : (
                <span className="sr-only">{' · ' + t(x.status === 'completed' ? 'convo.taskDone' : 'convo.taskTodo')}</span>
              )}
            </li>
          ))}
        </ol>
      )}
    </div>
  )
}

// A notice with a command's output in it: each of its parts in turn, what
// it printed as a terminal shows it, any words as a notice's.
function NoteParts({ e, onCopy }: { e: ConvoEntry; onCopy: (s: string) => void }) {
  // (One element for the one item: the list's places are counted by them.)
  return (
    <div className="convo-note-parts">
      {e.parts.map((p, i) =>
        p.shell ? (
          <ShellOut key={i} sh={p.shell} onCopy={onCopy} />
        ) : p.text?.trim() ? (
          <p key={i} className="convo-note">
            {p.text}
          </p>
        ) : null,
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
  // (Copied: what it printed, and what it wrote to stderr after.)
  const copied = [all, sh.err ? sh.err + (sh.err_cut ? '\n' + t('convo.cut') : '') : ''].filter(Boolean).join('\n')
  // (All of it on a tap where it is held short: by what is cut off, a long
  // line wrapped too, not by its count of lines.)
  const outEl = useRef<HTMLPreElement>(null)
  const errEl = useRef<HTMLPreElement>(null)
  const [over, setOver] = useState(false)
  useLayoutEffect(() => {
    const cut = (e: HTMLPreElement | null) => !!e && e.scrollHeight > e.clientHeight + 2
    const look = () => setOver(cut(outEl.current) || cut(errEl.current))
    look()
    // (Narrowed later, a long line wraps to more: looked at again.)
    const ro = new ResizeObserver(look)
    for (const e of [outEl.current, errEl.current]) if (e) ro.observe(e)
    return () => ro.disconnect()
  }, [all, sh.err])
  return (
    <div className="convo-shell">
      {all && (
        <>
          <div className="convo-pre-wrap">
            <pre ref={outEl} className={'convo-pre convo-result' + (full ? ' is-full' : '')}>
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
            <button type="button" className="md-code-copy" onClick={() => onCopy(copied)}>
              {t('common.copy')}
            </button>
          </div>
        </>
      )}
      {sh.err && (
        <div className="convo-pre-wrap">
          <pre ref={errEl} className={'convo-pre convo-result convo-shell-err' + (full ? ' is-full' : '')}>
            {sh.err}
            {sh.err_cut && <span className="convo-omitted">{'\n' + t('convo.cut')}</span>}
          </pre>
          {!all && (
            <button type="button" className="md-code-copy" onClick={() => onCopy(copied)}>
              {t('common.copy')}
            </button>
          )}
        </div>
      )}
      {!full && over && (
        <button type="button" className="link-btn" onClick={() => setFull(true)}>
          {t('convo.showAll')}
        </button>
      )}
    </div>
  )
}

// One item of the list. Memoised: a poll that changed one entry renders
// that one (the others keep their objects, see conversation.ts).
const ListItem = memo(
  function ListItem({ it, sum, copy, open, onToggle, onShow }: { it: Item; sum?: ConvoEntry; copy: (s: string) => void; open: boolean; onToggle: (key: string, open: boolean) => void; onShow: (key: string, path?: string) => void }) {
    switch (it.kind) {
      case 'user':
        return <UserTurn e={it.e} onCopy={copy} />
      case 'note': {
        const p = it.e.parts[0]
        if (p?.event) return <EventNote ev={p.event} idle={it.idle} sum={sum} />
        // (Retries next to each other: on the first's line, the last's.)
        if (p?.trouble) return <TroubleNote key={troubleKey(p.trouble)} t={(it.last ?? it.e).parts[0]?.trouble ?? p.trouble} />
        if (it.e.parts.some((x) => x.shell)) return <NoteParts e={it.e} onCopy={copy} />
        return <p className="convo-note">{noteText(it.e)}</p>
      }
      case 'summary':
        return (
          <details className="convo-summary">
            <summary>
              <ChevronRight size={16} strokeWidth={1.75} className="convo-chev" aria-hidden="true" />
              {t('convo.summary')}
            </summary>
            <div className="convo-md">
              <Markdown text={it.e.parts[0]?.text ?? ''} />
            </div>
          </details>
        )
      case 'answer':
        return <Answer it={it} onCopy={copy} onShow={onShow} />
      case 'text':
        return (
          <div className={'convo-md convo-mid' + (brief(it.text) ? ' is-brief' : '')}>
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
  (a, b) => a.copy === b.copy && a.open === b.open && a.onToggle === b.onToggle && a.onShow === b.onShow && a.sum === b.sum && sameItem(a.it, b.it),
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
    case 'limit':
      head = tr.until ? t('convo.trouble.limitUntil', { w: tr.until }) : t('convo.trouble.limit')
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
        {tr.kind === 'refused' && !tr.again && <p className="convo-trouble-said">{t('convo.trouble.refusedHint')}</p>}
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
// A finished turn's way there, folded: what it did, in a line, to open it by.
function FoldLine({ label, open, onToggle }: { label: string; open: boolean; onToggle: (open: boolean) => void }) {
  return (
    <button type="button" className="convo-work-head convo-fold" data-aside="" aria-expanded={open} onClick={() => onToggle(!open)}>
      <ChevronRight size={16} strokeWidth={1.75} className="convo-chev" aria-hidden="true" />
      <span className="convo-work-text">{label}</span>
    </button>
  )
}

// The marks of Claude Code's word on its own commands (convoItems noteEvent).
const SAID_ICONS: Record<string, typeof Info> = { effort: GaugeIcon, exit: LogOut, clear: SquarePlus, restart: RotateCw, rename: TextCursorInput, monitor: Radar, bg: SquareTerminal, agent: Bot, goal: Target, artifact: FileText }

function EventNote({ ev, idle, sum }: { ev: ConvoEvent; idle?: boolean; sum?: ConvoEntry }) {
  const [open, setOpen] = useState(false)
  const showTerminal = useContext(ShowTerminal)
  // What went on while the person was away; a goal set or judged; any other notice.
  if (ev.kind === 'recap' || ev.kind === 'goal' || ev.kind === 'notice') {
    const head = ev.kind === 'recap' ? t('convo.recap') : ev.kind === 'goal' ? t(ev.set ? 'convo.goal.set' : ev.met ? 'convo.goal.met' : 'convo.goal.unmet') : ev.text ?? ''
    const said = ev.kind === 'recap' ? recapText(ev.text ?? '') : ev.kind === 'goal' ? (ev.goal ?? '') : ''
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
  // A model set (or kept) with /model, here or in the terminal: one line (convoItems commandsOnce).
  if (ev.kind === 'said') {
    const Ic = SAID_ICONS[ev.ic ?? ''] ?? Info
    return (
      <div className={'convo-event is-limit is-said' + (ev.level === 'warning' ? ' is-warn' : '')}>
        <Ic size={14} aria-hidden="true" />
        <div className="convo-trouble-body">
          <p className="convo-trouble-head">{ev.text}</p>
        </div>
      </div>
    )
  }
  if (ev.kind === 'model') {
    const what = [ev.model ? modelName(ev.model) : '', ev.effort ? t('convo.model.effort', { e: effortName(ev.effort) }) : ''].filter(Boolean).join(' · ') // (as the chip says it)
    return (
      <div className="convo-event is-limit is-model">
        <Cpu size={14} aria-hidden="true" />
        <div className="convo-trouble-body">
          <p className="convo-trouble-head">{t(ev.kept ? 'convo.model.kept' : ev.session ? 'convo.model.setSession' : ev.saved ? 'convo.model.setSaved' : 'convo.model.set', { m: what })}</p>
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
  // Work set going in the background, ended (or waiting): one line, its
  // name's first line; all of it (a command of many lines) behind a tap.
  if (ev.kind === 'task') {
    const name = ev.text ?? ''
    const head = t(`convo.task.${ev.task ?? 'cmd'}.${ev.status ?? 'done'}` as I18nKey) + (ev.code ? t('convo.task.code', { n: ev.code }) : '')
    // (One line, its name's first line cut where the line ends: the line
    // itself opens all of it, however much was cut.)
    return (
      <div className={'convo-event is-task' + (ev.status === 'failed' ? ' is-warn' : '')}>
        {name ? (
          <button type="button" className="convo-task-line" aria-expanded={open} onClick={() => setOpen(!open)} title={name.length > 400 ? name.slice(0, 400) + '…' : name}>
            <span className="convo-task-head">{head}</span>
            <span className="convo-task-name">{firstLine(name)}</span>
          </button>
        ) : (
          <p className="convo-task-line">
            <span className="convo-task-head">{head}</span>
          </p>
        )}
        {name && open && (
          <pre className="convo-pre convo-task-full">
            {name}
            {ev.cut && '\n' + t('convo.task.cut')}
          </pre>
        )}
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
  const said = [t(ev.kind === 'idle_compact' || idle ? 'convo.event.compactIdle' : ev.trigger === 'manual' ? 'convo.event.compactManual' : ev.trigger === 'auto' ? 'convo.event.compactAuto' : 'convo.event.compact')]
  if (ev.pre && ev.post) said.push(t('convo.event.sizes', { a: tokens(ev.pre), b: tokens(ev.post) }))
  if (ev.ms) said.push(duration(Math.max(1000, ev.ms)))
  const line = said.map((x, i) => (
    <span key={i} className="convo-event-part">
      {i > 0 && t('convo.event.sep')}
      {x}
    </span>
  ))
  // With the summary it wrote: one line, opened to it.
  if (sum)
    return (
      <details className="convo-summary is-compact">
        <summary>
          <ChevronRight size={14} className="convo-chev" aria-hidden="true" />
          <span className="convo-event-said">{line}</span>
        </summary>
        <div className="convo-md">
          <Markdown text={sum.parts[0]?.text ?? ''} />
        </div>
      </details>
    )
  // (Each part whole on a line: a narrow screen breaks between them.)
  return (
    <p className="convo-event is-compact">
      <span className="convo-event-said">{line}</span>
    </p>
  )
}

// A recap as Claude Code writes it, without its closing word on turning
// recaps off ("(disable recaps in /config)"): a setting of its terminal.
export const recapText = (s: string) => s.replace(/\s*\((?:disable|turn off)[^()]*\/config\)\.?\s*$/i, '').trim()

// (The conversation's terminal, for a line that sends one there.)

// A subagent sent off, whose own conversation can be shown: by its id.
type Sub = { agent: string; title?: string }
const OpenSub = createContext<((s: Sub) => void) | undefined>(undefined)
// (In a subagent's conversation "the person" is the agent that sent it: what it
// wrote is written in Markdown, for a model, and shown so.)
const WordsAsWritten = createContext(false)

// What can be done with a file the agent names, on its host (through the
// session the conversation is read through): shown in the files panel,
// downloaded, looked at (a picture). Not there once that session has ended.
// (tail: a file looked at from its end — a command's output, newest last.)
export type FileActs = { logPic?: (from: string) => string; reveal: (path: string) => void; download: (path: string) => void; view: (path: string, group?: string[]) => void; tail?: (path: string) => void; open?: (dir: string) => void; thumb?: (path: string, signal: AbortSignal) => Promise<Blob> } // (open: a folder, opened in the file panel; group: the pictures shown with it, gone through in the viewer; thumb: a picture to show small, asked of the host as the others are)
const FileActsOf = createContext<FileActs | undefined>(undefined)

// A path the host can be asked for: absolute. (A relative one is not made
// whole with the agent's folder as it is now: the call may have been made
// in another — a worktree, a folder gone to since. It is copied only.)
export function fullPath(p: string): string | null {
  return p.startsWith('/') && !/[\x00-\x1f\x7f]/.test(p) ? p : null
}


// Such a span, while its session is live: a menu of what can be done with it.
function PathCode({ path, text, copy = text }: { path: string; text: string; copy?: string }) {
  const acts = useContext(FileActsOf)!
  const copyOf = useContext(CopyOf)
  const dir = path.endsWith('/')
  return (
    <Menu
      label={path}
      heading={path}
      items={() =>
        [
          { label: t('convo.path.copy'), icon: <Copy size={16} />, onSelect: () => copyOf?.(copy) },
          !dir && previewable(path) ? { label: t(IMAGE_EXT.test(path) ? 'convo.path.view' : 'preview.item'), icon: IMAGE_EXT.test(path) ? <ImageIcon size={16} /> : <Eye size={16} />, onSelect: () => acts.view(path) } : null,
          { label: t('convo.path.reveal'), icon: <FolderOpen size={16} />, onSelect: () => acts.reveal(dir ? path.replace(/\/+$/, '') || '/' : path) },
          dir ? null : { label: t('convo.path.download'), icon: <Download size={16} />, onSelect: () => acts.download(path) },
        ].filter(Boolean) as MenuItem[]
      }
      button={(open, toggle) => (
        <button type="button" className="md-path" aria-expanded={open} aria-haspopup="menu" onClick={toggle} title={t('convo.path.menu')}>
          <code>{text}</code>
        </button>
      )}
    />
  )
}
const CopyOf = createContext<((s: string) => void) | undefined>(undefined)
// (A copy of the conversation up to the end of one of the person's turns, where one can be started.)
const ForkAtOf = createContext<{ go: (e: ConvoEntry) => void; can: (e: ConvoEntry) => boolean } | undefined>(undefined)

// A file a step acted on: its whole path, and what can be done with it.
// (acts: only what can be done, the path being said beside it already.)
// What can be done with a file a panel shows, as icons on its head row:
// copy its path, look at it, show it in the file panel, download it.
function FileHeadTools({ path, onCopy }: { path: string; onCopy: (s: string) => void }) {
  const acts = useContext(FileActsOf)
  const full = fullPath(path)
  const tool = (label: string, onClick: () => void, icon: ReactNode) => (
    <button type="button" className="icon-btn" onClick={onClick} aria-label={label} title={label}>
      {icon}
    </button>
  )
  return (
    <span className="convo-file-tools">
      {tool(t('convo.path.copy'), () => onCopy(full ?? path), <Copy size={15} />)}
      {acts && full && previewable(full) && tool(t(IMAGE_EXT.test(full) ? 'convo.path.view' : 'preview.item'), () => acts.view(full), <Eye size={15} />)}
      {acts && full && tool(t('convo.path.reveal'), () => acts.reveal(full), <FolderOpen size={15} />)}
      {acts && full && tool(t('convo.path.download'), () => acts.download(full), <Download size={15} />)}
    </span>
  )
}

// The file a step read or changed, opened (11-14: its whole path again
// under the line that names it, and four words to press in a row): its
// name and folder, its tools as the panels' icons; a picture, as a picture.
function FileCard({ path, onCopy }: { path: string; onCopy: (s: string) => void }) {
  const acts = useContext(FileActsOf)
  const full = fullPath(path)
  const whole = full ?? path
  const name = baseName(whole)
  const dir = whole.slice(0, whole.length - name.length).replace(/\/$/, '') || '/'
  const pic = !!full && !!acts?.thumb && IMAGE_EXT.test(full)
  return (
    <div className="convo-file-card">
      {pic && <PicGrid paths={[full!]} thumb={acts!.thumb!} onOpen={(p) => acts!.view(p)} />}
      <div className="convo-file-card-row">
        <FileTypeIcon name={name} />
        <span className="convo-file-card-text" title={whole}>
          <span className="convo-file-card-name">{name}</span>
          <span className="convo-file-card-dir">{shortPath(dir)}</span>
        </span>
        <FileHeadTools path={path} onCopy={onCopy} />
      </div>
    </div>
  )
}
// (Claude Code's subagents' ids; a Codex's, its thread's UUID.)
const isSubId = (id: string) => /^[a-z0-9]{6,40}$/.test(id) || /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/.test(id)

// A subagent's own conversation, beside the thread (over it, where narrow),
// read only: what it was asked, what it did and answered. Read when shown;
// again while it is known to go on. One sent off by it opens here in turn
// (back: to the one that sent it).
function SubPanel({ of, sub, depth, live, nav, onOpen, onBack, onClose, active, copy }: { of: string; sub: Sub; depth: number; live: (agent: string) => boolean; nav?: WorkNav; onOpen: (s: Sub) => void; onBack: () => void; onClose: () => void; active: boolean; copy: (s: string) => void }) {
  const [sessionId, pane = ''] = of.split(' ')
  // (Of one sent off by the session's agent, as its running list says; of
  // one sent off by a subagent, while its log keeps growing.)
  const told = depth === 1 && live(sub.agent)
  const c = useSubConvo(sessionId, pane, sub.agent, told, active, depth > 1)
  const going = depth === 1 ? told : c.growing
  const panel = useRef<HTMLElement>(null)
  const [opened, setOpened] = useState<Record<string, boolean>>({})
  const toggle = useCallback((key: string, open: boolean) => setOpened((o) => ({ ...o, [key]: open })), [])
  // What it handed back is its answer, shown as one, not a step among the steps.
  const { steps, report } = useMemo(() => subReport(c.entries), [c.entries])
  const list = useMemo(() => items(steps, going, going), [steps, going])
  // (How it stands now, while it goes on: read off its log with the others': subscan.ts.)
  const scanned = useSubScan(active ? of : undefined, going, true).find((a) => a.id === sub.agent)
  useTick(going) // (its time said goes on)
  // While it goes on: shown at its latest, and kept there as it goes, unless
  // the person has scrolled up to read (then left where they are). 11-13:
  // before, it opened at its top — a long task's words over the whole of a
  // phone's screen — and what it did next came in out of sight.
  const body = useRef<HTMLDivElement>(null)
  const follow = useRef(going)
  const onScroll = () => {
    const el = body.current
    if (el) follow.current = el.scrollHeight - el.scrollTop - el.clientHeight < 48
  }
  useLayoutEffect(() => {
    const el = body.current
    if (el && going && follow.current) el.scrollTop = el.scrollHeight
  }, [list, going, report])
  // (What is drawn after — a line opened, a time said, a picture — grows it under the latest: kept to it.)
  useEffect(() => {
    const el = body.current
    if (!el || !going) return
    const keep = () => {
      if (follow.current) el.scrollTop = el.scrollHeight
    }
    const mo = new MutationObserver(keep)
    mo.observe(el, { childList: true, subtree: true, characterData: true })
    const ro = new ResizeObserver(keep)
    ro.observe(el)
    return () => {
      mo.disconnect()
      ro.disconnect()
    }
  }, [going])
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
        {/* (As the changes panel's head: a small word for what it is, its name, how it goes.) */}
        <div className="convo-panel-headtext">
          <span className="convo-panel-eyebrow">{nav ? t('convo.workOf', { what: t('sub.kind'), i: nav.i + 1, n: nav.n }) : t('sub.title')}</span>
          <h2 className="convo-panel-title is-one" title={sub.title}>
            {sub.title || t('sub.title')}
          </h2>
          {(going || report) && <span className="convo-panel-sub convo-sub-state">{going ? [t('sub.going'), since(scanned?.ts), scanTokens(scanned)].filter(Boolean).join(t('convo.sep')) : t('sub.handedBack')}</span>}
        </div>
        {nav && <WorkStep nav={nav} />}
        {/* (Read again every few seconds while it goes on: by hand, where a read failed or it has ended — not a phone's room for it.) */}
        <button type="button" className="icon-btn convo-sub-refresh" onClick={c.read} disabled={c.loading} aria-label={t('sub.refresh')} title={t('sub.refresh')}>
          <RotateCw size={16} className={c.loading ? 'convo-running-spin' : ''} />
        </button>
        <button type="button" className="icon-btn" onClick={onClose} aria-label={t('common.close')} title={t('common.close') + ' (Esc)'}>
          <X size={18} />
        </button>
      </div>
      {going && scanNow(scanned) && <p className="convo-sub-now" title={scanNow(scanned)}>{scanNow(scanned)}</p>}
      <div className="convo-panel-body convo-sub-body" ref={body} onScroll={onScroll}>
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
          <WordsAsWritten.Provider value={true}>
          {list.map((it) => (
            <ListItem key={itemKey(it)} it={it} copy={copy} open={it.kind === 'work' ? !!opened[it.key] : false} onToggle={toggle} onShow={noShow} />
          ))}
          </WordsAsWritten.Provider>
          {report && (
            <section className="convo-sub-report" aria-label={t('sub.report')}>
              <h3 className="convo-sub-report-head">{t('sub.report')}</h3>
              <Markdown text={report.text} onCopyCode={copy} copyLabel={t('common.copy')} />
              {report.cut && <p className="convo-older">{t('sub.reportCut')}</p>}
            </section>
          )}
        </OpenSub.Provider>
      </div>
      {going && c.running.length > 0 && (
        <div className="convo-sub-foot">
          <RunningWork items={c.running} openSub={onOpen} />
        </div>
      )}
    </aside>
  )
}

// A subagent's log: its steps, without the call it ended with; and what that
// handed back. Only where it did end with it: taken up again since, it is a step
// like any, not this one's answer yet.
function subReport(entries: ConvoEntry[]): { steps: ConvoEntry[]; report?: { text: string; cut?: boolean } } {
  const said = (e: ConvoEntry) => e.role === 'user' || (e.role === 'assistant' && e.parts.some((p) => p.kind !== 'thinking' || p.text?.trim()))
  const hasReport = (e: ConvoEntry) => e.role === 'assistant' && e.parts.some((p) => p.tool?.report)
  let at = entries.length - 1
  while (at >= 0 && !said(entries[at])) at--
  const e = entries[at]
  if (!e || !hasReport(e) || e.parts[e.parts.length - 1].tool?.report === undefined) return { steps: entries }
  const last = e.parts[e.parts.length - 1].tool!
  const parts = e.parts.slice(0, -1)
  return { steps: [...entries.slice(0, at), ...(parts.length ? [{ ...e, parts }] : []), ...entries.slice(at + 1)], report: { text: last.report!, cut: last.report_cut } }
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
  const time = hhmm(d)
  return d.toDateString() === new Date().toDateString() ? time : d.toLocaleDateString(getLang(), { month: 'short', day: 'numeric' }) + ' ' + time
}

// What is known of a message sent that has not turned up in the log, where
// anything is: it went to a subagent; its words are still in the box (Enter
// for it here is the person's own); or only that the conversation has not
// caught up, the terminal a tap away. Nothing else is said: not seeing it in
// the log says nothing of whether it arrived.
function SentNote({ p, onEnter, onResend, onShowTerminal }: { p: ConvoPending; onEnter?: () => Promise<boolean>; onResend?: () => void; onShowTerminal: () => void }) {
  // (Enter pressed from here: under way, or what became of it.)
  const [entering, setEntering] = useState<'' | 'busy' | 'gone' | 'failed'>('')
  const went = wentOf(p)
  if (went)
    return (
      <div className="convo-missing">
        <p>{t('convo.missing.went', { name: went.description || went.type || went.agent })}</p>
        {onResend && (
          <button type="button" className="btn btn-ghost convo-missing-enter" onClick={onResend}>
            {t('convo.missing.resend')}
          </button>
        )}
      </div>
    )
  if (p.state === 'stuck')
    return (
      <div className="convo-missing">
        <p>{t(onEnter ? 'convo.missing.waitingEnter' : 'convo.stuck')}</p>
        {onEnter && (
          <button
            type="button"
            className="btn btn-ghost convo-missing-enter"
            disabled={entering === 'busy'}
            onClick={() => {
              setEntering('busy')
              onEnter().then(
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
        <button type="button" className="link-btn" onClick={onShowTerminal}>{t('convo.showTerminal')}</button>
      </div>
    )
  if (p.quiet && !p.opened)
    return (
      <div className="convo-missing">
        <p>{t('convo.notCaughtUp')}</p>
        <button type="button" className="link-btn" onClick={onShowTerminal}>{t('convo.showTerminal')}</button>
      </div>
    )
  return null
}

const noShow = () => {}
// (An item where it is: what the agent writes is the same text written
// and, its turn over, its answer.)
const placeKey = (it: Item) => itemKey(it).replace(/^[xa](?=\d)/, 'p')

// An item noted in view, where it was (see note).
type Anchor = { key: string; top: number; fromEnd: number; last: string }

// Where each conversation was left, for this page's life: as many as are
// kept to be shown at once (conversation.ts), the latest last.
type Place = { gen: number; stuck: boolean; anchors: Anchor[]; noted: Item[]; opened: Record<string, boolean>; allOpen: boolean; unfolded: Record<string, boolean> }
const places = new Map<string, Place>()
function keepAt(of: string, p: Place) {
  places.delete(of)
  places.set(of, p)
  while (places.size > 12) places.delete(places.keys().next().value!)
}

// Where an item begins in the log (its first entry's seq). (A key's seq with
// its sign: a log read from its end numbers what is before that end below
// zero — "x-151.0" is of -151, not 151; read as 151 the line of what is new
// went to the top of a long log's conversation on every coming back, 11-05.)
const startSeq = (it: Item): number => (it.kind === 'gone' ? it.entries[0].seq : 'e' in it ? it.e.seq : Number(/-?\d+/.exec(it.key)?.[0] ?? 0))

// What was read of each conversation, for this page's life: the last entry
// seen at its end (of that reading of its log).
// (at: when it was last seen at its end, ms — read on since, or looked at again with nothing new.)
const reads = new Map<string, { gen: number; seq: number; at: number }>()
// (When each was last read at its end as it was left: see keepPlace.)
const leftRead = new Map<string, number>()
function readUpTo(of: string, gen: number, seq: number) {
  const was = reads.get(of)
  const at = Date.now()
  if (was && was.gen === gen && was.seq >= seq) return void (was.at = at)
  reads.delete(of)
  reads.set(of, { gen, seq, at })
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
        <ChevronRight size={16} strokeWidth={1.75} className="convo-chev" aria-hidden="true" />
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
  return ms ? hhmm(new Date(ms)) : '—'
}

function clock(ts?: string): string {
  if (!ts) return ''
  const d = new Date(ts)
  if (isNaN(d.getTime())) return ''
  return hhmm(d)
}

// The pictures' paths written in a message's words: absolute or under the
// home folder, of a kind a picture is looked at as, at most a few.
// (The shortest that ends a picture's name and then a word's end: "/a.png，再看/b.png"
// is /a.png and words after it, not one path of both — review 3374eba.)
const PICTURE_PATH = /(?:^|[\s"'(（「])((?:\/(?!\/)|~\/)[^\s"'()（）「」<>]{1,1000}?\.(?:png|jpe?g|gif|webp))(?=$|[\s"'),.;:!?，。；：！？）」])/gi
// A picture put up from Corral: its name made up on upload (a time, a few
// letters) — told as "图片 1", not as its path (11-05).
const UPLOADED = /\/\.cache\/corral\/uploads\/[^/]+$/
const baseOf = (p: string) => p.slice(p.lastIndexOf('/') + 1)

// The person's words with the pictures they name in their places: each path
// a chip to look at it — one put up from here "图片 1", another by its file's
// name — the path whole in its title, the words copied as they were.
function withPictures(text: string, open: ((p: string) => void) | null): ReactNode {
  const parts: ReactNode[] = []
  const ups = [...text.matchAll(PICTURE_PATH)].map((m) => m[1]).filter((p, i, all) => UPLOADED.test(p) && all.indexOf(p) === i)
  let at = 0
  let k = 0
  for (const m of text.matchAll(PICTURE_PATH)) {
    const path = m[1]
    const start = m.index! + m[0].length - path.length
    if (start > at) parts.push(text.slice(at, start))
    const n = ups.indexOf(path)
    const said = n < 0 ? baseOf(path) : ups.length > 1 ? t('convo.picN', { n: n + 1 }) : t('convo.pic')
    parts.push(
      open ? (
        <button key={k++} type="button" className="convo-pic is-inline" onClick={() => open(path)} title={path}>
          <ImageIcon size={13} aria-hidden="true" />
          <span>{said}</span>
        </button>
      ) : (
        <span key={k++} className="convo-pic is-inline is-unknown" title={path}>
          <ImageIcon size={13} aria-hidden="true" />
          <span>{said}</span>
        </span>
      ),
    )
    at = start + path.length
  }
  if (at === 0) return text
  if (at < text.length) parts.push(text.slice(at))
  return parts
}

// A message's pictures, small, over its words (11-13: before, a chip each,
// "图片 1", its picture seen only once opened). One: its own shape, a fixed
// height (what comes in later moves nothing), as wide as its proportions say
// within bounds; two or more, tiles in a frame. Each asked of the host only
// once it is near the view — one at a time, as all pictures are (lookAt);
// one not there any more says so.
function PicGrid({ paths, thumb, onOpen }: { paths: (string | null)[]; thumb: (p: string, signal: AbortSignal) => Promise<Blob>; onOpen: (p: string) => void }) {
  const n = paths.length
  return (
    <div className={'convo-thumbs is-n' + Math.min(n, 4) + (n > 4 && n % 2 ? ' is-odd' : '')}>
      {paths.map((p, i) => (
        <PicTile key={i} path={p} n={i + 1} one={n === 1} thumb={thumb} onOpen={onOpen} />
      ))}
    </div>
  )
}

function PicTile({ path, n, one, thumb, onOpen }: { path: string | null; n: number; one: boolean; thumb: (p: string, signal: AbortSignal) => Promise<Blob>; onOpen: (p: string) => void }) {
  const ref = useRef<HTMLButtonElement>(null)
  const [near, setNear] = useState(false)
  const [blob, setBlob] = useState<Blob | null>(null)
  const [gone, setGone] = useState(false)
  const [ratio, setRatio] = useState(0)
  const url = useObjectURL(blob)
  useEffect(() => {
    const el = ref.current
    if (!el || !path || near) return
    const io = new IntersectionObserver((es) => es.some((x) => x.isIntersecting) && (setNear(true), io.disconnect()), { rootMargin: '300px 0px' })
    io.observe(el)
    return () => io.disconnect()
  }, [path, near])
  useEffect(() => {
    if (!near || !path || blob) return
    const ctl = new AbortController()
    thumb(path, ctl.signal).then(
      (b) => !ctl.signal.aborted && setBlob(b),
      () => !ctl.signal.aborted && setGone(true),
    )
    return () => ctl.abort()
  }, [near, path, blob, thumb])
  // (One alone: 180 high, as wide as its shape says, 96 to 320.)
  const width = one && ratio ? Math.round(Math.min(320, Math.max(96, 180 * Math.min(4, Math.max(0.25, ratio))))) : undefined
  const said = t('convo.picN', { n })
  return (
    <button
      ref={ref}
      type="button"
      className={'convo-thumb' + (gone ? ' is-gone' : '')}
      style={width ? { width } : undefined}
      disabled={!path}
      onClick={() => path && onOpen(path)}
      aria-label={gone ? t('convo.picGone', { name: said }) : said}
      title={path ? (gone ? t('convo.picGone', { name: path }) : path) : t('convo.picUnknown')}
    >
      {url && !gone ? (
        <img
          src={url}
          alt=""
          decoding="async"
          onLoad={(ev) => setRatio(ev.currentTarget.naturalWidth / Math.max(1, ev.currentTarget.naturalHeight))}
          onError={() => setGone(true)}
        />
      ) : (
        <span className="convo-thumb-mark" aria-hidden="true">
          {gone ? <ImageOff size={18} /> : <ImageIcon size={18} />}
        </span>
      )}
    </button>
  )
}

function UserTurn({ e, onCopy }: { e: ConvoEntry; onCopy: (s: string) => void }) {
  const text = e.parts
    .filter((p) => p.kind === 'text')
    .map((p) => p.text)
    .join('\n')
  const pics = e.parts.filter((p) => p.kind === 'image')
  const images = pics.length
  // (Of the pictures, the ones whose file is told: each to be looked at, its session live.)
  const acts = useContext(FileActsOf)
  // (Each in its place, numbered by it; one whose file is not told, not to be
  // opened. And a picture's path written in its words — one sent from here
  // goes as its path, "/…/uploads/x.png look at this" — after them, each
  // once: 11-04.)
  // (No file told — sent while the agent worked, pasted in its terminal: the log's own copy, where there is one.)
  const told = acts ? pics.map((p) => (p.path && fullPath(p.path) ? p.path : p.from && acts.logPic ? acts.logPic(p.from) : null)) : []
  // (A path written in the words is a chip where it stands — withPictures —
  // not again under them: these are the pictures with no words of theirs.)
  const files = acts ? told : []
  // Shown small over the words, where the page can ask the host for them: the
  // pictures it came with, and those it names by their whole path (one put up
  // from here goes so) — each once, in order; gone through together in the viewer.
  const grid = useMemo(() => {
    if (!acts?.thumb) return null
    const out: (string | null)[] = []
    for (const f of files) if (!f || !out.includes(f)) out.push(f)
    for (const m of text.matchAll(PICTURE_PATH)) if (m[1].startsWith('/') && !out.includes(m[1])) out.push(m[1])
    return out.length ? out : null
  }, [acts, text, e.parts])
  const group = grid ? (grid.filter(Boolean) as string[]) : undefined
  const view = acts ? (p: string) => acts.view(p, group) : null
  const shown = useMemo(() => withPictures(text, view), [text, acts, grid])
  const any = files.some(Boolean)
  // (A shell command run with "!": in the terminal's letters, as typed.)
  const shell = /^! \S/.test(text)
  const forkAt = useContext(ForkAtOf)
  // (A long one — a pasted command, a log — folded to its first lines, opened at a tap.)
  const written = useContext(WordsAsWritten) && !shell
  // (A subagent's task, in its panel: what it was asked, a few lines of it —
  // what it does now is what is looked for there, most of all on a phone.)
  const long = written ? text.split('\n').length > 4 || text.length > 280 : text.split('\n').length > LONG_LINES || text.length > LONG_CHARS
  const [whole, setWhole] = useState(false)
  const words = written ? <Markdown text={text} /> : shown
  return (
    <div className={'convo-user' + (e.mid ? ' is-mid' : '')}>
      {grid && <PicGrid paths={grid} thumb={acts!.thumb!} onOpen={(p) => acts!.view(p, group)} />}
      {(text.trim() || !grid) && (
      <div className={'convo-user-text' + (shell ? ' is-shell' : '') + (written ? ' is-written' : '')}>
        {long && !whole ? <span className="convo-user-fold">{words}</span> : words}
        {long && (
          <button type="button" className="link-btn convo-user-more" aria-expanded={whole} onClick={() => setWhole(!whole)}>
            {t(whole ? 'convo.userLess' : 'convo.userMore')}
          </button>
        )}
        {images > 0 && !told.some(Boolean) && !/\[Image #\d+\]/.test(text) && <span className="convo-image">{t(images === 1 ? 'convo.images1' : 'convo.images', { n: images })}</span>}
        {any && !grid && (
          <span className="convo-pics">
            {files.map((f, i) => {
              const said = files.length > 1 ? t('convo.picN', { n: i + 1 }) : t('convo.pic')
              return f ? (
                <button key={i} type="button" className="convo-pic" onClick={() => acts!.view(f)} title={f}>
                  <ImageIcon size={14} aria-hidden="true" />
                  <span>{said}</span>
                </button>
              ) : (
                <span key={i} className="convo-pic is-unknown" title={t('convo.picUnknown')}>
                  <ImageIcon size={14} aria-hidden="true" />
                  <span>{said}</span>
                </span>
              )
            })}
          </span>
        )}
        {e.parts.some((p) => p.truncated) && <p className="convo-cut">{t('convo.cut')}</p>}
      </div>
      )}
      <div className="convo-meta">
        {e.mid && <span>{t('convo.midSent')}</span>}
        <span>{clock(e.ts)}</span>
        <MetaActs copy={text ? () => onCopy(text) : undefined} fork={forkAt && !e.mid && !e.gone && e.id && forkAt.can(e) ? () => forkAt.go(e) : undefined} />
      </div>
    </div>
  )
}

// What can be done with a message, after its time: words where a pointer
// hovers (shown as it does, see terminal.css); behind a "···" on a touch
// screen, so they take no line of their own under every message.
function MetaActs({ copy, fork }: { copy?: () => void; fork?: () => void }) {
  if (!copy && !fork) return null
  return (
    <>
      <span className="convo-meta-acts">
        {copy && (
          <button type="button" className="link-btn" onClick={copy}>
            {t('common.copy')}
          </button>
        )}
        {fork && (
          <button type="button" className="link-btn" onClick={fork} title={t('fork.atHint')}>
            {t('fork.at')}
          </button>
        )}
      </span>
      <Menu
        label={t('common.more')}
        items={() => [
          copy ? { label: t('common.copy'), icon: <Copy size={16} />, onSelect: copy } : null,
          fork ? { label: t('fork.at'), icon: <GitBranch size={16} />, hint: t('fork.atHint'), onSelect: fork } : null,
        ]}
        button={(open, toggle) => (
          <button type="button" className="convo-meta-more" aria-expanded={open} aria-haspopup="menu" onClick={toggle} aria-label={t('common.more')}>
            <MoreHorizontal size={16} aria-hidden="true" />
          </button>
        )}
      />
    </>
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
        {it.span !== undefined && <span>{t('convo.worked', { d: duration(it.span) })}</span>}
        <span>{clock(it.ts)}</span>
        <MetaActs copy={() => onCopy(it.text)} />
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

const SENT_SHOWN = 6

// A run of steps between what the agent wrote: one line (what it did; while
// it goes on, the step under way and for how long); opened, a line a step,
// each opened on its own (a call's command, output and changes, a thought).
function Work({ it, open, onToggle, onCopy }: { it: Extract<Item, { kind: 'work' }>; open: boolean; onToggle: (key: string, open: boolean) => void; onCopy: (s: string) => void }) {
  const tools = it.steps.flatMap((s) => (!isNote(s) && s.tool ? [s.tool] : []))
  const counts = new Map<Category, number>()
  for (const tl of tools) counts.set(category(tl.name), (counts.get(category(tl.name)) ?? 0) + 1)
  const order: Category[] = ['edit', 'cmd', 'read', 'search', 'web', 'agent', 'other']
  // (What it did, named where a name says more than a count: the files it
  // changed, the one command it ran; counts for the rest.)
  const sum = order
    .filter((c) => counts.has(c))
    .map((c) => said(c, tools.filter((tl) => category(tl.name) === c)))
    .join(t('convo.sep'))
  const failedOnes = tools.filter((tl) => tl.error)
  const failed = failedOnes.length
  const busy = it.busy
  const now = useTick(!!busy)
  // (Of thinking only: that it thought, and the start of it.)
  const thought = !tools.length ? it.steps.find((x): x is ConvoPart => !isNote(x) && x.kind === 'thinking') : undefined
  const head = sum ? '' : thought ? t('convo.thinking') : it.live ? t('convo.workingNow') : t('convo.steps')
  // While a step is under way, the line says it (what it runs, for how long);
  // the counts come back once it is done.
  const since = busy && partTime.has(busy) ? ms(partTime.get(busy)) : 0
  // (Under way while the agent is not at work: it waits for the person, to
  // be let run it or to be answered — not running, not for that long.)
  const asks = !!busy && !it.moving
  // (At work between its steps — "Working", no step under way: the same
  // light over its word as over a step's, not a word that looks left behind. 11-14.)
  const between = !busy && !!it.live && !thought && !sum
  const [shine, setShine] = useState<HTMLSpanElement | null>(null)
  useRunWhileSeen(shine, (!!busy && !asks) || between)
  // Shut, what is said of it without opening: the subagents it sent off, by
  // name, each to be read; and with calls beside it the last of what it
  // thought, a line (at most one for a run, however long it thought).
  const openSub = useContext(OpenSub)
  const sent = tools.filter((tl) => category(tl.name) === 'agent' && tl.title)
  const lastThought = tools.length
    ? it.steps.findLast((x): x is ConvoPart => !isNote(x) && x.kind === 'thinking' && !!x.text?.trim())
    : undefined
  // The line's mark, from what its words say: the step under way (or waited
  // on); thinking; else the kind its counts name first.
  const first = order.find((c) => counts.has(c))
  const Mark = busy ? (busy.tool ? stepIcon(busy.tool) : ThinkIcon) : thought ? ThinkIcon : first ? categoryIcon(first) : WorkIcon
  // (The fold mark after the words: the kind is what comes first.)
  const chev = (
    <span className="convo-chev-box" aria-hidden="true">
      <ChevronRight size={14} className="convo-chev" />
    </span>
  )
  // (Nothing yet to open — the agent just begun, its step under way said on this line: a line, not a
  // button that opens on nothing. 11-13: "Working ›" opened on an empty run every time.)
  const shows = it.steps.some((x) => isNote(x) || (x !== busy && (!!x.tool || x.kind === 'thinking')))
  if (!shows)
    return (
      <div className="convo-work" data-work={it.key}>
        <div className={'convo-work-head is-static' + ((busy && !asks) || between ? ' is-live' : '')}>
          <Mark size={16} strokeWidth={1.75} className="convo-step-ic" aria-hidden="true" />
          {busy && asks ? (
            <span className="convo-work-text is-live is-asking">
              <span className="convo-asking-text">{waitText(busy.tool)}</span>
              {since > 0 && now >= since && <span className="convo-live-time">{t('convo.waitedFor', { d: duration(now - since) })}</span>}
            </span>
          ) : busy ? (
            <span className="convo-work-text is-live">
              <span ref={setShine} className="convo-shimmer">{busy.tool ? liveText(busy.tool) : t('convo.live.thinking')}</span>
              {since > 0 && now >= since && <span className="convo-live-time">{duration(now - since)}</span>}
            </span>
          ) : between ? (
            <span className="convo-work-text is-live">
              <span ref={setShine} className="convo-shimmer">{head}</span>
            </span>
          ) : (
            <span className="convo-work-text">{head || sum}</span>
          )}
        </div>
      </div>
    )
  return (
    <div className="convo-work" data-work={it.key}>
      <button type="button" className={'convo-work-head' + ((busy && !asks) || between ? ' is-live' : '') + (!busy && failed > 0 ? ' is-failed' : '')} aria-expanded={open} onClick={() => onToggle(it.key, !open)} title={isTouchOnly ? undefined : t(open ? (mac ? 'convo.foldHint' : 'convo.foldHintPlain') : mac ? 'convo.openHint' : 'convo.openHintPlain')}>
        <Mark size={16} strokeWidth={1.75} className="convo-step-ic" aria-hidden="true" />
        {busy && asks ? (
          <span className="convo-work-text is-live is-asking">
            <span className="convo-asking-text">{waitText(busy.tool)}</span>
            {since > 0 && now >= since && <span className="convo-live-time">{t('convo.waitedFor', { d: duration(now - since) })}</span>}
            {chev}
          </span>
        ) : busy ? (
          <span className="convo-work-text is-live">
            <span ref={setShine} className="convo-shimmer">{busy.tool ? liveText(busy.tool) : t('convo.live.thinking')}</span>
            {since > 0 && now >= since && <span className="convo-live-time">{duration(now - since)}</span>}
            {chev}
          </span>
        ) : (
          <span className={'convo-work-text' + (thought ? ' is-thought' : '')}>
            {/* (Thinking: the mark says it was; the words are its first line.) */}
            {thought && thoughtTitle(thought.text ?? '') && <span className="sr-only">{head} </span>}
            {thought ? thoughtTitle(thought.text ?? '') || head : between ? <span ref={setShine} className="convo-shimmer">{head}</span> : head || sum}
            {failed > 0 && (
              <>
                {t('convo.sep')}
                <span className="convo-failed is-inline">{failed === 1 ? t('convo.failedOne', { s: clip(object(failedOnes[0]).text, 32) }) : t('convo.failed', { n: failed })}</span>
              </>
            )}
            {chev}
          </span>
        )}
      </button>
      {!open && lastThought && <p className="convo-work-said" title={lastThought.text}>{thoughtTitle(lastThought.text ?? '')}</p>}
      {!open && sent.length > 0 && (
        <ul className="convo-work-sent">
          {sent.slice(0, SENT_SHOWN).map((tl, i) => (
            <li key={tl.id ?? i}>
              {tl.agent_id && openSub ? (
                <button type="button" className="link-btn" title={t('sub.open')} onClick={() => openSub({ agent: tl.agent_id!, title: tl.title })}>
                  {tl.title}
                </button>
              ) : (
                tl.title
              )}
            </li>
          ))}
          {sent.length > SENT_SHOWN && <li className="convo-work-sent-more">{t('convo.sentMore', { n: sent.length - SENT_SHOWN })}</li>}
        </ul>
      )}
      {open && (
        <div className="convo-work-body">
          {it.steps.map((s, i) =>
            isNote(s) ? (
              s.role === 'summary' ? (
                <ListItem key={i} it={{ kind: 'summary', e: s }} copy={onCopy} open={false} onToggle={onToggle} onShow={noShow} />
              ) : (
                s.parts[0]?.event ? (
                  <EventNote key={i} ev={s.parts[0].event} />
                ) : s.parts.some((x) => x.shell) ? (
                  <NoteParts key={i} e={s} onCopy={onCopy} />
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
  if (tool.stdin) return { text: stdinSaid(tool.stdin).obj, code: false }
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
// What was put to a command still running (Codex's write_stdin): keys by
// name, else how much was typed (never what); a look at what it printed
// when nothing was.
function stdinSaid(s: NonNullable<ConvoTool['stdin']>): { verb: string; obj: string } {
  const at = s.session ? t('convo.stdin.at', { s: s.session }) : ''
  if (s.keys?.length) return { verb: t('convo.stdin.press'), obj: s.keys.join(' ') + at }
  if (s.chars) return { verb: t('convo.stdin.type'), obj: t('convo.stdin.chars', { n: s.chars }) + at }
  return { verb: t('convo.stdin.look'), obj: t('convo.stdin.output') + at }
}

// What a step not come back says while the agent waits for the person:
// leave to run it, or an answer.
function waitText(tool?: ConvoTool): string {
  if (!tool) return t('convo.status.blocked')
  // (A command: the command itself, as while it runs — that is what is to be let run.)
  const cmd = firstLine((tool.detail ?? '').replace(/^cd\s+(?:"[^"]*"|'[^']*'|[^\s&;|]+)\s*&&\s*/, ''))
  const o = tool.stdin ? '' : verbOf(tool.name) === 'cmd' && cmd ? cmd : object(tool).text
  const ask = verbOf(tool.name) === 'ask' || tool.name === 'AskUserQuestion' || tool.name === 'ExitPlanMode'
  return o ? t(ask ? 'convo.waitAnswer' : 'convo.waitApprove', { s: o }) : t('convo.status.blocked')
}

// A row's line of what it is about now, from what is kept of its
// conversation (see keptEntries): what it waits for the person to allow or
// answer; the step under way; or how its answer begins. '' where nothing is
// known (never read) or nothing says more than its state.
// since: when its state last changed (ms): what was kept before then is not
// of this state and says nothing (a new turn begun, a process replaced).
export function glanceOf(sessionId: string, pane: string | undefined, status: string, since?: number): string {
  if (status !== 'blocked' && status !== 'working' && status !== 'done') return ''
  const k = keptEntries(sessionId, pane)
  if (!k || (since && k.at < since)) return ''
  const es = k.entries.filter((e) => !e.gone) // (taken back by a rewind or undo: not what it is about)
  let i = es.length - 1
  while (i >= 0 && !(es[i].role === 'user' && !es[i].mid)) i--
  const parts = es.slice(i + 1).flatMap((e) => (e.role === 'assistant' ? e.parts : []))
  const open = [...parts].reverse().find((p) => p.kind === 'tool' && p.tool && !p.tool.done)
  if (status === 'blocked') return open?.tool ? waitText(open.tool) : ''
  if (status === 'working' && open?.tool) return liveText(open.tool)
  const said = [...parts].reverse().find((p) => p.kind === 'text' && p.text?.trim())
  if (!said?.text) return ''
  // (Its first line of words: no heading or list marks, no code quotes.)
  const line = said.text.split('\n').map((l) => l.replace(/^\s*(?:#{1,6}\s+|[-*+]\s+|\d+[.)]\s+|>\s*)/, '').replace(/[`*_]/g, '').trim()).find(Boolean) ?? ''
  return line.length > 120 ? line.slice(0, 120) + '…' : line
}

function liveText(tool: ConvoTool): string {
  if (tool.stdin) {
    const s = stdinSaid(tool.stdin)
    return s.verb + ' ' + s.obj
  }
  const v = verbOf(tool.name)
  // (The folder it goes to first comes off before the first line is taken:
  //  a command of several lines begins "cd … &&" on a line of its own.)
  const cmd = firstLine((tool.detail ?? '').replace(/^cd\s+(?:"[^"]*"|'[^']*'|[^\s&;|]+)\s*&&\s*/, ''))
  const o = v === 'cmd' && cmd ? cmd : object(tool).text
  return v ? t(VERB_SAID[v].live, { s: o }) : t('convo.live.other', { s: toolName(tool.name) })
}

function Thinking({ text, live }: { text: string; live?: boolean }) {
  const [open, setOpen] = useState(false)
  const title = thoughtTitle(text)
  if (live) return null // (said under the steps, as the step under way)
  return (
    <div className="convo-step is-thought">
      {/* (The mark stands for "thinking": said to a reader by the button's name, not
          by words of its own in the line — copied, or read as its text, the line is its title.) */}
      <button type="button" className="convo-step-head" aria-expanded={open} aria-label={title ? `${t('convo.thinking')}: ${title}` : undefined} onClick={() => setOpen(!open)}>
        <ThinkIcon size={16} strokeWidth={1.75} className="convo-step-ic" aria-hidden="true" />
        <span className="convo-step-obj">{title || t('convo.thinking')}</span>
      </button>
      {open && <div className="convo-thinking"><Markdown text={text} /></div>}
    </div>
  )
}

export function thoughtTitle(text: string): string {
  return firstLine(text).replace(/^\s*(?:Thinking\s*:?\s*)?/i, '').replace(/^#{1,6}\s+/, '').replace(/\*\*([^*]+)\*\*/g, '$1').replace(/`([^`]+)`/g, '$1').trim()
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
  const StepMark = stepIcon(tool)
  // (A command shown by what it is for, as Claude Code said it.)
  const told = !tool.stdin && v === 'cmd' && !!tool.title?.trim() && obj.text === tool.title
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
  // (A picture read: shown on its card — not Claude Code's "1 image(s) read" under it.)
  const picRead = cat === 'read' && /^\d+ image\(s\) read\.?$/.test(tool.result?.trim() ?? '')
  const head = tool.done && !receipt && !picRead ? (code ? tool.result?.replace(/^Exit code \d+\n?/, '') : tool.result) : undefined
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
        <StepMark size={16} strokeWidth={1.75} className="convo-step-ic" aria-hidden="true" />
        {/* (The verb, but where the line goes on with what it is for in words — a command Claude Code said the purpose of — the mark
            says it alone: "Run local server…" is not to be read after "Run". It is kept for the screen reader.) */}
        <span className={told ? 'sr-only' : 'convo-step-verb'}>{tool.stdin ? stdinSaid(tool.stdin).verb : v ? t(VERB_SAID[v].v) : toolName(tool.name)}</span>
        <span ref={objRef} className={'convo-step-obj' + (obj.code ? ' is-code' : '')}>{v === 'cmd' && obj.code ? <Code text={obj.text} lang="bash" /> : obj.text}</span>
        {cat === 'edit' && <LineCounts add={tool.add ?? 0} del={tool.del ?? 0} />}
        {tool.error && <span className="convo-failed">{code ? t('convo.exitCode', { n: code }) : t('convo.toolFailed')}</span>}
      </button>
      {open && (
        <div className="convo-step-body">
          {(cat === 'read' || cat === 'edit') && !!d && !d.includes('\n') && <FileCard path={d} onCopy={onCopy} />}
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
          {tool.asks && !tool.answers && (
            // (A question put to the person, its choices: Codex's, read only.)
            <dl className="convo-ask-answers">
              {tool.asks.map((a, i) => (
                <div key={i}>
                  <dt>{a.question}</dt>
                  {(a.options ?? []).map((o, j) => (
                    <dd key={j}>
                      {o.label}
                      {o.description && <span className="convo-ask-desc"> — {o.description}</span>}
                    </dd>
                  ))}
                </div>
              ))}
            </dl>
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
            !receipt && !picRead && (
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

// Whether an edit made the file: Claude Code's Write says so in its result
// ("File created successfully"); a patch that adds a whole file (Codex's
// "*** Add File") is one hunk from line 0 of nothing but added lines. Not
// "a Write with no patch": that may have written over a file there before.
const madeFile = (tool: ConvoTool): boolean =>
  (tool.name === 'Write' && /^File created successfully/i.test(tool.result ?? '')) ||
  (!!tool.patch && tool.patch.length === 1 && tool.patch[0].old_start === 0 && tool.patch[0].lines.every((l) => l[0] === '+'))

// A file a turn made anew: all of it, as added lines.
const created = (tool: ConvoTool): ConvoHunk[] | undefined => {
  if (tool.name !== 'Write' || tool.patch || tool.same || !tool.new) return undefined
  const lines = tool.new.replace(/\n$/, '').split('\n')
  return [{ old_start: 0, new_start: 1, lines: lines.map((l) => '+' + l) }]
}

// The changes a turn made, beside the thread (over it, where narrow): its
// files, each with its edits in order.
function ChangesPanel({ edits, focus, onClose, active, onCopy }: { edits: Edits; focus?: string; onClose: () => void; active: boolean; onCopy: (s: string) => void }) {
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
        <div className="convo-panel-headtext">
          <h2 className="convo-panel-title">{t('convo.changesTitle')}</h2>
          <span className="convo-panel-sub">
            {t(edits.files.length === 1 ? 'convo.changes1' : 'convo.changes', { n: edits.files.length })}
            <LineCounts add={edits.add} del={edits.del} />
          </span>
        </div>
        <button type="button" className="icon-btn" onClick={onClose} aria-label={t('common.close')} title={t('common.close') + ' (Esc)'}>
          <X size={18} />
        </button>
      </div>
      {/* (A list to go to each file by: only where there is more than one.) */}
      {edits.files.length > 1 && <div className="convo-panel-files">
        {edits.files.map((f) => (
          <button key={f.path} type="button" className="convo-changes-file" onClick={() => box.current?.querySelector(`[data-file="${CSS.escape(f.path)}"]`)?.scrollIntoView({ block: 'start', behavior: matchMedia('(prefers-reduced-motion: reduce)').matches ? 'auto' : 'smooth' })} title={f.path}>
            <span className="convo-changes-path">{shortPath(f.path)}</span>
            <LineCounts add={f.add} del={f.del} />
          </button>
        ))}
      </div>}
      <div className="convo-panel-body" ref={box}>
        {edits.files.map((f) => (
          <section key={f.path} className="convo-panel-file" data-file={f.path}>
            {/* (Its name to know it by, its folder after it — whole on hover — what
                it gained and lost, and what can be done with it, as icons.) */}
            <div className="convo-panel-file-head" title={f.path}>
              <FileTypeIcon name={baseName(f.path)} />
              <span className="convo-panel-file-name">{baseName(f.path)}</span>
              <span className="convo-panel-file-dir">{shortPath(f.path.slice(0, f.path.lastIndexOf('/') + 1) || '/')}</span>
              {madeFile(f.tools[0]) && <span className="convo-new-tag">{t('convo.newFile')}</span>}
              <LineCounts add={f.add} del={f.del} />
              <FileHeadTools path={f.path} onCopy={onCopy} />
            </div>
            {f.tools.map((tl, i) => {
              const h = tl.patch ?? created(tl)
              return (
                <div key={i} className={'convo-panel-edit' + (madeFile(tl) ? ' is-new' : '')}>
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

export { listOrder, type ConvoListRow, duration, when }
