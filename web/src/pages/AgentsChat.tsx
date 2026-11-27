import { useTaskAsked } from '../terminal/bgTaskOpen'
import { useCallback, Activity, useEffect, useLayoutEffect, useMemo, useReducer, useRef, useState, type ReactNode } from 'react'
import { ArrowDown, ArrowLeftRight, ArrowUp, ArrowUpDown, ArrowUpToLine, ChevronLeft, CircleStop, Code2, Columns2, Copy, Eye, FoldVertical, GitBranch, GitFork, Globe, History, Languages, ListPlus, Maximize2, MessageSquarePlus, Paperclip, Pencil, Pin, PinOff, Plus, Power, Rows2, Square, SquareSlash, SquareTerminal, Link2, TextCursorInput, Unlink, X, Zap } from 'lucide-react'
import { isBusyErrorCode, NOTHING_PRESSED } from '../terminal/busyCode'
import { api, ApiError, authGen, errorText, onAuthChange, uploadFile, type SessionInfo } from '../api'
import { useSession } from '../app'
import { t, type I18nKey } from '../i18n'
import { navigate, onNavigate } from '../router'
import { heldOpen, onHeldOpen, onOpenRequest, takeHeld } from '../notifyopen'
import { isDark, maxPinned, setChromeColor, setPrefs, usePrefs } from '../prefs'
import { local } from '../storage'
import { CLOSE_MENUS, Confirm, copyText, Menu, useToast, type MenuItem, type ToastAction } from '../components/ui'
import { chromeVars, corralDark, corralLight } from '../terminal/themes'
import { HostHistoryDialog } from '../terminal/hostHistory'
import { CardNotice, useClaudeCard, type ClaudeCard } from '../terminal/cardNotice'
import { attachedIn, expandForClaude, expandPastes, getDraft, keepDraft, keepPaste, keepSent, keptSent, remember, sentHistory, showDraft, unsendable } from '../terminal/drafts'
import { geminiPath, shellPath, type LineSpan } from '../terminal/paths'
import type { DockSink } from '../terminal/dock'
import { useCompletion } from '../terminal/Completion'
import { PanelWindow } from '../terminal/PanelWindow'
import { convoTarget, herdrTarget, InlineReply } from '../terminal/ReplyDialog'
import { SessionOpDialog, type SessionOp } from '../terminal/sessionOps'
import { NEW_AGENT, NewAgentDialog, takeNewAgentAsk, type NewSubagentParent } from '../terminal/newAgent'
import { type CodexRewind, ForkDialog, LinkParentDialog, type LinkPane, RenameDialog, type RenameTarget, RewindDialog, type RewindTarget } from '../terminal/agentOps'
import { isRewindIdle } from '../terminal/rewindIdle'
import { rewindEditedFiles } from '../terminal/rewindFiles'
import { agentWho, ChimeCounter, chime, countsOf } from '../terminal/chime'
import { AgentList, CODEX_EFFORTS, ComposerModelSlot, ConversationView, glanceOf, type ConvoAgent, type ConvoLineageBar, type ConvoListRow, type ConvoPending, type ConvoSplitPane, type ConvoWent, type FileActs, type ModelOpt } from '../terminal/Conversation'
import { saveTranslateConfig, useTranslateConfig } from '../terminal/thoughtTranslate'
import { FilesDialog } from '../terminal/FilesDialog'
import { PortsDialog } from '../terminal/PortsDialog'
import { ShareDialog } from '../terminal/ShareDialog'
import { useCode } from '../terminal/CodeDialog'
import { isLogPic, logPic, lookAt, PictureDialog, useDownload } from '../terminal/picture'
import { IMAGE_EXT, TextPreview } from '../terminal/textPreview'
import { attach, ATTACH_LABEL, AttachStrip, PICTURE_PATH, takeOut, type Going } from '../terminal/attach'
import { answerable, asking, callOut, useConversation, type ConvoEntry } from '../terminal/conversation'
import { ackBg, answeredHerdr, convoTitles, expectSession, paneWaits, waitingTitle, isAgy, isCodex, isDevin, isOpenCode, listAgents, paneName, paneTitle, readSessionsNow, readsLog, useRunningSessions, type AgentRow, type AgentStatus } from '../terminal/agents'
import { normalizeProjectPath, reorderPinned, rowTasks, type RowCmds, type RowTask } from '../terminal/agentOrder'
import { agentStatusMark, convoLastUserMs, keepTmuxBg, mergeRowTasks, readTmuxBg, reconcileAgentStatus, type AgentStatusMark, type TmuxBgCacheEntry } from '../terminal/sideState'
import { conversationReadable, prefetchConversation } from '../terminal/conversation'
import { holdTermMode, tmuxKey } from '../terminal/chatSend'
import { camesAfter, containedInMessage, isCommand, pairSent, sameMessage, squeezed, type Submission } from '../terminal/sentSeen'
import { viewRefusal } from '../terminal/sendView'
import { decideLiveTerminal, liveFor, shouldClearLiveTerminal } from '../terminal/liveTerminalPolicy'
import { agentKey, sessionWhich } from '../terminal/sessionName'
import { pickKey as isPickKey } from '../terminal/pageKeys'
import { firstShown, hideBox, keptBox, KeptPart, KeptView, letGoBox, modalShown, useEffectOnChange, useShownAt, useWhenGone, type KeptBox } from '../shown'
import { Rail, StatusBar, StatusItem, useWorkbench } from '../workbench'
import { CorraldStatusBarItem } from '../components/CorraldStatusBarItem'
import { isCorraldUnknownResult } from '../corraldLogic'
import { AgentTabBar, askPickInTab, isTabsKey, newTabId, OPEN_PICKER, readFront, readTabs, takePickInTab, writeFront, writeTabs, type AgentTab, type PickHow, type TabView } from './agentTabs'
import { agentMark } from '../terminal/agentIcons'
import { useComposerFit } from '../terminal/autosize'
import { keyboardIsUp } from '../keyboard'

// The session a Codex not yet begun on its log is sent to (the gateway's convoNoSession).
const NO_SESSION = '00000000-0000-0000-0000-000000000000'

// The agents page: every agent of the sessions the gateway has open, and the
// conversation of the one picked, with a composer that sends to it. It
// talks to each agent's pane directly (POST …/conversation/send, interrupt,
// send-now): what herdr has in front, or a terminal tab shows, is not
// changed. With tabs it is a tab of its own (the terminals stay open behind
// it), the list beside the conversation. Without (a phone), two screens: the
// list, and an agent's conversation opened from it.
//
// On a phone, the history is a ladder (as Collie's, after iOS's edge swipe
// went back through conversations seen rather than to the list): the list,
// then one conversation over it. Opening one from the list adds an entry
// (marked FROM_LIST); going to another from inside it replaces that entry;
// back goes back to the list, never adds one. One opened straight from an
// address (a notification, a bookmark) gets the list put under it first.

const LAST = 'corral.agentsPick'
const ONLY_MINE = 'corral.agentsOnlyMine'
const FROM_LIST = { agentsFromList: true }
// (Longer than going through a picker of its own from here takes, between two reads.)
const PANEL_AFTER_MS = 1500
// (How long one with no conversation to read is in front before it counts as looked at: not one passed over.)
const NONE_SEEN_AFTER_MS = 1000

// The "done" ones gone on from before their conversation came (see
// doneVisit), read one at a time: those with none to read marked seen.
let noneChecks: Promise<void> = Promise.resolve()
const NONE_CHECK_TRIES = 3
const NONE_CHECK_AGAIN_MS = 3000
const noneChecking = new Set<string>()
function seenIfNone(v: { sid: string; pane: string; round: number; session: string }) {
  const key = JSON.stringify(v)
  if (noneChecking.has(key)) return
  noneChecking.add(key)
  noneChecks = noneChecks.then(async () => {
    try {
      for (let tries = 1; ; tries++) {
        try {
          if (!(await conversationReadable(v.sid, v.pane)))
            await api('POST', `/api/sessions/${v.sid}/herdr/panes/${encodeURIComponent(v.pane)}/seen`, { round: v.round, session: v.session })
          return
        } catch (e) {
          // (The host busy with what was asked before: a little later, a few times.)
          if (!(e instanceof ApiError && isBusyErrorCode(e.code)) || tries >= NONE_CHECK_TRIES) throw e
          await new Promise((r) => setTimeout(r, NONE_CHECK_AGAIN_MS))
        }
      }
    } catch {
      // (Not known: it stays as it is, to be looked at again.)
    } finally {
      noneChecking.delete(key)
    }
  })
}
// Over the page, what an Escape would be meant for.
const ESC_HELD = '[aria-modal="true"], [role="menu"], [role="dialog"], .convo-search, .convo-sub-body'
// What is left without the "/" the commands button put in, or null when a word has been typed on to it.
const unslash = (s: string) => (s === '/' ? '' : s.startsWith('/ ') ? s.slice(2) : null)
const fromList = () => !!(history.state as { agentsFromList?: boolean } | null)?.agentsFromList
const chatAddress = (e: { s: SessionInfo | string; pane?: string }) => {
  const q = new URLSearchParams({ s: typeof e.s === 'string' ? e.s : e.s.id })
  if (e.pane) q.set('pane', e.pane)
  return '/agents?' + q
}
const PASTE_LINES = 12
// An installed app's window: the tab keys are taken there only (a browser keeps them for its own tabs).
const standalone = window.matchMedia('(display-mode: standalone)').matches || (navigator as { standalone?: boolean }).standalone === true
// A finished one not seen yet whose turn ended on an API error (a rate
// limit, the server's error): told apart from one that finished well.
const failedOf = (r: AgentRow, st: AgentStatus) => (!r.lost && !r.bg && st === 'done' && r.a.failed) || undefined
// Someone at the window: it has the focus, and was touched — a key, the
// pointer, a scroll — in the last minute. A finished agent shown on a screen
// nobody is at is not looked at (workbench 11-23: a page left open on a
// desk had every agent it showed marked seen).
const PRESENT_MS = 60000
let lastInputAt = 0 // (none yet: a page loaded or brought back by itself is not someone at it)
for (const ev of ['pointerdown', 'pointermove', 'keydown', 'wheel', 'touchstart'] as const) window.addEventListener(ev, () => (lastInputAt = Date.now()), { capture: true, passive: true })
const someoneHere = () => document.hasFocus() && Date.now() - lastInputAt < PRESENT_MS
const SEND_MS = 45000 // (the host may wait for another message to the pane to go first)
const PRESS_MS = 20000
// When Claude Code's suggestion is looked for after a turn ends (ms): it
// comes a moment after, at times a while after.
const SUGGEST_AT = [1500, 4000, 9000]
// (ms) Claude Code compacting says so only on its screen (its log, once
// done): looked at every COMPACT_LOOK while it works with nothing new in its
// log for COMPACT_QUIET, up to COMPACT_UNTIL (it compacts right after its
// log's last word, a minute or two: a silence longer than that is something
// else). Each look takes the pane's turn a moment, as a send does.
const COMPACT_LOOK = 10000
const COMPACT_QUIET = 15000
// (A Codex says so too of a request tried again — its retries come within
// seconds of going quiet: looked at sooner and more often.)
const CODEX_LOOK = 5000
// (How often a Codex's screen is looked at for "Implement this plan?" while its plan waits.)
const PLAN_LOOK = 3000
// (How often a Codex given a goal has its footer looked at for how the goal goes.)
const GOAL_LOOK = 10000
const CODEX_QUIET = 2000
const COMPACT_UNTIL = 240000
// Of each turn's end asked about (by its key): the looks left, what was
// found, the latest look (only its answer is taken), whether one is on its
// way, and the views showing it. Kept beyond a view, so that going to
// another agent and back does not look all over again.
type SuggestAsk = { left: number; text: string; seq: number; busy: boolean; views: Set<{ told: () => void; next: () => void }> }
const suggestAsks = new Map<string, SuggestAsk>()
const MODE_MS = 50000 // (to a mode picked: several presses, each read back; the gateway gives up at 45 s)
const MODEL_MS = 50000 // (the picker, step by step: see convo_model.go)
const MODEL_CACHE_MS = 10 * 60 * 1000 // (keep fetched model lists across agent switches for instant opens)
const keptModelCache = new Map<string, { list: ModelOpt[]; at: number; byLog: boolean }>()
onAuthChange(() => keptModelCache.clear())
const PASTE_CHARS = 2000
const isTouch = window.matchMedia('(pointer: coarse)').matches || navigator.maxTouchPoints > 0
const hasKeyboardPointer = () => !window.matchMedia('(pointer: coarse)').matches
const isMac = /Mac|iPhone|iPad/.test(navigator.platform)

// The agent picked: a herdr session's pane, or a tmux session. (key: its
// entry's, which outlasts the session: a gateway restarted, the same
// agent is in another one.)
type Pick = { s: string; pane?: string; key?: string }

// What the agents page's view holds that is not in its address — the side
// list's search, the agents closed from here, where the one shown is placed —
// kept across the page's being made anew (the
// window crossing the width where tabs come and go puts it under another
// parent), of the account signed in only. Only so: the page left and come
// back to later is as new (left: when it was let go; made anew at once, in
// the same moment, it goes on).
type Carried = { gen: number; left: number; query: string; gone: string[]; placed?: { key: string; as: ConvoListRow['placeAs'] } }
const carried: Carried = { gen: -1, left: 0, query: '', gone: [] }
function carry(): Carried {
  if (carried.gen !== authGen() || (carried.left && Date.now() - carried.left > 2000)) Object.assign(carried, { gen: authGen(), left: 0, query: '', gone: [], placed: undefined })
  return carried
}

// The pick an address names; none named, the fallback: the tab in front's
// (see agentTabs.tsx), or with no tabs ever kept the one last shown ('last').
function readPick(fallback: Pick | null | 'last' = 'last'): Pick | null {
  const q = new URLSearchParams(location.search)
  const s = q.get('s')
  if (s && /^[0-9a-f]+$/.test(s)) return { s, pane: q.get('pane') ?? undefined }
  if (fallback !== 'last') return fallback
  try {
    const p = JSON.parse(local.get(LAST) ?? 'null')
    if (p && typeof p.s === 'string') return { s: p.s, pane: typeof p.pane === 'string' ? p.pane : undefined, key: typeof p.key === 'string' ? p.key : undefined }
  } catch {
    /* none kept */
  }
  return null
}

// One entry of the list: a herdr agent, or a tmux session (whose agent is
// whatever runs in it; only the one shown has a known state).
type Entry = { key: string; s: SessionInfo; pane?: string; row?: AgentRow }

// Where an entry is, as its row says it: its host only where the list has
// more than one; its workspace (herdr's "~" at home says nothing), or tmux.
// Which program runs in it, its icon says (one without an icon of its own: its name).
const whereOf = (e: Entry, hosts: number) => {
  const host = hosts > 1 ? e.s.host_name : ''
  const r = e.row
  if (!r) return [host, 'tmux'].filter(Boolean).join(' · ')
  return [host, r.ws && r.ws !== '~' ? r.ws : '', tabOf(r), agentMark(r.a.agent) ? '' : r.a.agent].filter(Boolean).join(' · ')
}
// Its tab's name, where its workspace has more than one (one alone says nothing).
const tabOf = (r: AgentRow) => ((r.a.tabs ?? 0) > 1 ? (r.a.tab_label ?? '') : '')
const hostCount = (entries: Entry[]) => new Set(entries.map((e) => e.s.host_name)).size
const sessionGroupOf = (e: Entry, all: SessionInfo[]) => `${e.s.host_name} · ${sessionWhich(e.s, all)}`

// An agent not named yet (no title from its log or its pane): by the folder
// it runs in, before its program's name — several "claude" are not told
// apart. Not a home folder: its name is the account's, not the work's.
export const dirName = (cwd?: string): string => {
  const p = (cwd ?? '').replace(/\/+$/, '')
  if (!p || /^(?:\/home\/[^/]+|\/Users\/[^/]+|\/root)$/.test(p)) return ''
  return p.slice(p.lastIndexOf('/') + 1)
}

// What a row says of an entry, on either list (said: its title, as the log
// or its pane has it). The person's own name for it goes first, where there
// is one: a workspace they named says what the work is better than the name
// the agent gave the session, and it does not change under them. What the
// agent called it goes under it. (A session that is not in a workspace of
// its own has no such name: see whereOf.)
// (named: said is a name of the work's, not the folder or program it was
// told by for want of one — that says nothing under a workspace's name.)
const labelOf = (e: Entry, said: string, hosts: number, named = true, sameWsParent = false): { title: string; where: string } => {
  const r = e.row
  const ws = r && r.ws !== '~' ? r.ws : ''
  // (A name given its pane, the most of the person's own: before its workspace's.)
  const own = r?.a.label ?? ''
  const agentName = r?.a.name ?? ''
  // A subagent split inside its parent's workspace should not duplicate the
  // parent's workspace name as its own title: prefer its pane label, its
  // conversation title, or its herdr agent name (e.g. "antigravity").
  if (r && sameWsParent && !own) {
    const title = (named && said) || agentName || r.a.agent || said
    const host = hosts > 1 ? e.s.host_name : ''
    const where = [host, named && said && agentName && agentName !== title ? agentName : '', agentMark(r.a.agent) ? '' : r.a.agent].filter(Boolean)
    return { title, where: where.join(' · ') }
  }
  if (!r || (!ws && !own)) return { title: said, where: whereOf(e, hosts) }
  const host = hosts > 1 ? e.s.host_name : ''
  const title = own || ws
  const where = [host, own && !sameWsParent ? ws : '', sameWsParent ? '' : tabOf(r), named && said !== title ? said : '', agentMark(r.a.agent) ? '' : r.a.agent].filter(Boolean)
  // (Nothing else to say under it — a Codex not named, say, its first message
  // not used for a name — the folder it runs in, rather than an empty line.)
  if (!where.length && !sameWsParent && said && said !== title) where.push(said)
  return { title, where: where.join(' · ') }
}

// Commands its agent left running in the background, as its row says them:
// only while its turn is over (done or idle, as herdr says it: then nothing
// it runs in front is still going), and not when what is known is old.
function cmdsOf(r: AgentRow): RowCmds | undefined {
  const a = r.a
  if (r.lost || !a.bg_cmds || (a.status !== 'done' && a.status !== 'idle')) return undefined
  return { n: a.bg_cmds, list: (a.bg_cmd_list ?? []).map((c) => ({ task: c.task, title: c.title, desc: c.desc, since: c.since, out: c.out })), tasks: a.bg_cmd_tasks }
}

// What it set going in the background, to hang under its row (UI item 8,
// 11-25): its subagents still out and its commands — at work, those the
// gateway found it set going in the background; done or idle, all it left
// running. Not while it waits on the person (nothing is told of it then).
function tasksOf(r: AgentRow): RowTask[] | undefined {
  const a = r.a
  if (r.lost || a.status === 'blocked') return undefined
  return rowTasks(a.bg_agent_list, a.bg_cmds ? { n: a.bg_cmds, list: a.bg_cmd_list ?? [] } : undefined)
}

// Pins kept to at most max: those of agents not on the list let go, the
// oldest (first pinned) first, as many as must be; those on it never.
export function makeRoom(pins: string[], here: Set<string>, max: number): string[] {
  let over = pins.length - max
  return over <= 0 ? pins : pins.filter((k) => here.has(k) || over-- <= 0)
}

// What the agents' tab says of the conversation in front (its name, program, state).
export interface AgentsHead { title: string; program?: string; status: string }
// (How long a tab's agent is missing from the list before the tab says it ended: as the one in front is waited for.)
const MISSING_MS = 15000
// (How many tabs' conversations are kept drawn: plans/agent-tabs.md 4.4 — a safety valve, not a limit on tabs.)
const DRAWN_MAX = 20

export function AgentsChat({ bar, active = true, onHead }: { bar?: ReactNode; active?: boolean; onHead?: (h: AgentsHead | null) => void }) {
  const toast = useToast()
  const prefs = usePrefs()
  // (Its own two looks, light and dark as the app is: not the terminal's colours.)
  const scheme = isDark() ? corralDark : corralLight
  const { sessions, failed } = useRunningSessions(active, 3000)
  const wb = useWorkbench()
  // A sound when one finishes or comes to need the person (terminal/chime.ts).
  const chimeCounts = useRef(new ChimeCounter())
  useEffect(() => {
    if (!sessions) return
    const now = countsOf(sessions)
    for (const c of chimeCounts.current.feed(now)) {
      const [sid, pane] = c.pane.split(' ')
      const s = sessions.find((x) => x.id === sid)
      if (s) chime(c.kind, agentWho(s, pane))
    }
    chimeCounts.current.keep(new Set(now.keys()))
  }, [sessions])
  // (On its own, not a tab: the title tells how many wait for an answer.)
  const waitingN = useMemo(() => (sessions ? listAgents(sessions).rows.filter((r) => r.status === 'blocked').length : null), [sessions])
  // (Said again at each reading, not only when it changes: an alert come
  // late may have set the icon to an older count meanwhile.)
  useEffect(() => {
    if (!bar) waitingTitle(waitingN, sessions)
  }, [bar, waitingN, sessions])
  useEffect(() => () => void (!bar && waitingTitle(null)), [bar])
  // The tabs (agentTabs.tsx): the conversations open, the one in front shown.
  // Kept for the account signed in; none ever kept, the page opens as it did
  // before tabs (the one last shown, else the first) and that is the first tab.
  const user = useSession().me?.id ?? 0
  const init = useRef<{ tabs: AgentTab[]; front: string | null; ever: boolean } | null>(null)
  if (!init.current) {
    const k = readTabs(user)
    const l = k ?? []
    const f = readFront()
    init.current = { tabs: l, front: l.find((x) => x.id === f)?.id ?? l[0]?.id ?? null, ever: k !== null }
  }
  const [tabs, setTabs] = useState<AgentTab[]>(init.current.tabs)
  const [front, setFront] = useState<string | null>(init.current.front)
  const tabsRef = useRef(tabs)
  tabsRef.current = tabs
  const frontRef = useRef(front)
  frontRef.current = front
  // (Set at once, for what follows in the same act: the state comes with the next drawing.)
  const putTabs = (l: AgentTab[]) => {
    tabsRef.current = l
    setTabs(l)
  }
  const putFront = (id: string | null) => {
    frontRef.current = id
    setFront(id)
  }
  const activeRef = useRef(active)
  activeRef.current = active
  const hadTabs = useRef(init.current.ever) // (once there were tabs, none is none: the first agent is not put in)
  if (tabs.length) hadTabs.current = true
  const frontTab = tabs.find((x) => x.id === front)
  const tabPick = (x: AgentTab): Pick => ({ s: x.s, pane: x.pane, key: x.key })
  const frontPick = () => {
    const f = tabsRef.current.find((x) => x.id === frontRef.current)
    return f ? tabPick(f) : hadTabs.current ? null : 'last'
  }
  useEffect(() => {
    if (hadTabs.current) writeTabs(user, tabs)
  }, [tabs, user])
  useEffect(() => writeFront(front), [front])
  // Another window of this account changed them: as it left them (the one in front here kept, if still there).
  // (Its agent or what is beside it changed there: shown here too — else this
  // window would show, address and write to the one before. Review of 11-25.)
  const fromOther = useRef<(l: AgentTab[]) => void>(() => {})
  useEffect(() => {
    const on = (e: StorageEvent) => {
      if (!isTabsKey(e.key)) return
      const l = readTabs(user)
      if (l) fromOther.current(l)
    }
    window.addEventListener('storage', on)
    return () => window.removeEventListener('storage', on)
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [user])
  const [pick, setPick] = useState<Pick | null>(() => readPick(init.current!.front ? tabPick(init.current!.tabs.find((x) => x.id === init.current!.front)!) : init.current!.ever ? null : 'last'))
  // The entry key of the one picked last (an address names only its session and pane).
  const pickKey = useRef<Pick | null>(pick?.key ? pick : null)
  const keyOf = (p: Pick) => (pickKey.current && pickKey.current.s === p.s && (pickKey.current.pane ?? '') === (p.pane ?? '') ? pickKey.current.key : p.key)
  // (A phone's two screens: the address says which.)
  const stack = !bar
  const [screen, setScreen] = useState<'list' | 'chat'>(() => (stack && !new URLSearchParams(location.search).get('s') ? 'list' : 'chat'))
  const screenRef = useRef(screen)
  screenRef.current = screen
  // The pick named by an address (see the pick's being made the one shown).
  const named = useRef(!!new URLSearchParams(location.search).get('s'))
  // Opened straight at a conversation: the list goes under it, once.
  useEffect(() => {
    if (!stack || screen !== 'chat' || fromList()) return
    const at = location.pathname + location.search
    history.replaceState(null, '', '/agents')
    history.pushState(FROM_LIST, '', at)
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])
  // The titles of agents read here (Claude Code's own), for their rows once another is shown.
  const titles = useRef<Titles>(convoTitles) // (the page's one map: the palette names agents by it too)

  // Agents closed from here: out of the list at once, before herdr's
  // summary says so (else the one shown would be waited for, or picked
  // again). (A pane's id is not used again.)
  useEffect(() => {
    carried.left = 0
    return () => void (carried.left = Date.now())
  }, [])
  const [gone, setGone] = useState<ReadonlySet<string>>(() => new Set(carry().gone))
  useEffect(() => void (carry().gone = [...gone]), [gone])
  // (Once herdr no longer has it, it is let go: a pane's id may come again
  // in a herdr session made anew, another agent's then.)
  useEffect(() => {
    if (!sessions || !gone.size) return
    const live = new Set(listAgents(sessions).rows.map((r) => r.key))
    if ([...gone].some((k) => !live.has(k))) setGone((g) => new Set([...g].filter((k) => live.has(k))))
  }, [sessions, gone])
  // What the side list is searched for: the page's, as each agent's view is its own.
  const [sideQuery, setSideQuery] = useState(() => carry().query)
  useEffect(() => void (carry().query = sideQuery), [sideQuery])
  const entries = useMemo((): Entry[] => {
    const { rows, others } = listAgents(sessions ?? [])
    const out: Entry[] = rows.filter((r) => !gone.has(r.key)).map((r) => ({ key: r.key, s: r.s, pane: r.a.pane_id, row: r }))
    const seen = new Set<string>()
    for (const s of others) {
      if (s.persistence !== 'tmux' || seen.has(tmuxKey(s))) continue
      seen.add(tmuxKey(s))
      out.push({ key: tmuxKey(s), s })
    }
    return out
  }, [sessions, gone])
  // The one shown: the one picked while it is there (a session without a
  // pane, from an older address: the agent herdr has in front there, else
  // its first), else the first (one waiting for an answer comes first).
  // (A tmux session, or a herdr agent, open in several connections is listed
  // once, by one of them: picked by another, it is that entry.)
  const pickedSession = pick ? sessions?.find((x) => x.id === pick.s) : undefined
  const picked = pick
    ? entries.find((e) => e.s.id === pick.s && (e.pane ?? '') === (pick.pane ?? '')) ??
      (pickedSession?.persistence === 'tmux' && !pick.pane ? entries.find((e) => !e.pane && e.key === tmuxKey(pickedSession)) : undefined) ??
      (pickedSession?.persistence === 'herdr' && pick.pane ? entries.find((e) => e.key === agentKey(pickedSession, pick.pane!)) : undefined) ??
      (pick.pane === undefined ? entries.find((e) => e.s.id === pick.s && e.s.meta?.t === 'herdr' && e.pane === e.s.meta.focused_pane) ?? entries.find((e) => e.s.id === pick.s) : undefined) ??
      (!pickedSession && keyOf(pick) ? entries.find((e) => e.key === keyOf(pick)) : undefined)
    : undefined
  // One picked that is not in the list for a moment, as herdr's summary of
  // its session is not there or not current, stays shown as last seen; one
  // a current summary no longer has (its pane closed) does not.
  const lastCur = useRef<Entry | undefined>(undefined)
  const summaryMissing = (x: SessionInfo) => x.persistence === 'herdr' && (x.meta?.t !== 'herdr' || !!x.meta.lost)
  const unsure = !!pickedSession && summaryMissing(pickedSession)
  const kept = !picked && pick && unsure && lastCur.current?.s.id === pick.s && (lastCur.current.pane ?? '') === (pick.pane ?? '') && !gone.has(lastCur.current.key) ? lastCur.current : undefined
  // (None seen yet, as opened from an address while the summary is missing:
  // waited for a while, rather than another shown in its place, and made the pick.)
  const [gaveUp, setGaveUp] = useState(false)
  if (picked && gaveUp) setGaveUp(false) // (found: the next time, waited for again)
  const waiting = !picked && !kept && !!pick && unsure && !gaveUp && !gone.has(keyOf(pick) ?? '') // (closed from here: not waited for)
  // Its session gone, its agent maybe coming back in another (a gateway
  // restarted, its sessions opened again one by one): for a while the pick
  // stays and nothing is shown in its place (another put there would be
  // written to as if it were this one); back, it is shown again. Only one
  // that was in front of the person: one remembered from before, never
  // shown since, is given up at once, and the first is the pick.
  const shownKey = useRef<string | undefined>(init.current.tabs.find((x) => x.id === init.current!.front)?.key) // (the last one shown: kept while none is; the tab in front here was, before a reload)
  const holding = !picked && !!pick && !pickedSession && !!keyOf(pick) && !gaveUp && shownKey.current === keyOf(pick) && !gone.has(keyOf(pick)!) // (closed from here: not coming back)
  useEffect(() => {
    if (!waiting && !holding) return
    const timer = setTimeout(() => setGaveUp(true), 15000)
    return () => clearTimeout(timer)
  }, [waiting, holding, pick?.s, pick?.pane]) // (another waited for: its own while)
  // (The first only where no tab ever was: once there were, none open is an empty page, and one ended stays so.)
  const cur = picked ?? kept ?? (sessions && !waiting && !holding && !hadTabs.current && !tabs.length ? entries[0] : undefined)
  // The pick not found, nor waited for: of the tab in front, it ended (said so,
  // the tab kept till closed); else (an address naming one gone) the tab in front again.
  const unresolved = !!sessions && !!pick && !cur && !waiting && !holding
  const pickIsFront = !!frontTab && !!pick && (keyOf(pick) === frontTab.key || (pick.s === frontTab.s && (pick.pane ?? '') === (frontTab.pane ?? '')))
  const ended = unresolved && pickIsFront
  const shownNow = useRef<string | undefined>(undefined)
  shownNow.current = cur?.key
  useEffect(() => {
    lastCur.current = cur
    if (cur && !(stack && screen === 'list')) shownKey.current = cur.key // (a phone's list: none of them is in front)
  })
  // Gone to: as said by how — a new tab ('tab'), beside the one shown
  // ('split'), else in the tab in front. Open already in a tab, that tab is
  // gone to instead (see the tabs' keeping below: one agent, one tab).
  const choose = (e: Entry, how?: PickHow) => {
    if (how === 'split') return openSplit(e)
    if (how === 'tab' && !stack) {
      const open = tabsRef.current.find((x) => x.key === e.key || (x.split === e.key && entries.some((en) => en.key === x.key))) // (beside one ended: not shown there)
      if (open) return goTab(open, open.split === e.key ? 'secondary' : undefined)
      const tab: AgentTab = { id: newTabId(), key: e.key, s: e.s.id, pane: e.pane }
      const l = [...tabsRef.current]
      l.splice(l.findIndex((x) => x.id === frontRef.current) + 1, 0, tab)
      putTabs(l)
      putFront(tab.id)
      setSplitKey(null)
      setFocusedPane('primary')
    }
    setPick({ s: e.s.id, pane: e.pane, key: e.key })
    pickKey.current = { s: e.s.id, pane: e.pane, key: e.key }
    try {
      local.set(LAST, JSON.stringify({ s: e.s.id, pane: e.pane, key: e.key }))
    } catch {
      /* not kept */
    }
    if (stack && screen === 'list') {
      setScreen('chat')
      navigate(chatAddress(e), false, FROM_LIST) // (down the ladder: an entry)
    } else navigate(chatAddress(e), true, history.state) // (another, in its place: its mark kept, wide or not)
  }
  // A new agent (or subagent) asked from the list or row menu: once started, it
  // is gone to (or opened in split view when spawned under the active parent on desktop),
  // its first message waiting in its composer.
  const [newAgent, setNewAgent] = useState<boolean | NewSubagentParent>(false)
  useEffect(() => {
    const on = () => {
      if (!active) return
      takeNewAgentAsk()
      setNewAgent(true)
    }
    if (active && takeNewAgentAsk()) setNewAgent(true) // (asked before this page was there, or in front)
    window.addEventListener(NEW_AGENT, on)
    return () => window.removeEventListener(NEW_AGENT, on)
  }, [active])
  // Split-screen secondary agent: supports both left-right ('row') and
  // top-bottom ('col') layouts, plus 'auto' (switches to top-bottom on portrait
  // or narrow screens so portrait tablets/monitors & phones can split cleanly).
  // (As the tab in front had them: else the first keeping wrote none over its split.)
  // (Only when what is shown is that tab's agent: an address naming another
  // puts that one in its place, the split going as it does. Review of 11-25.)
  const initFront = ((f) => {
    const q = new URLSearchParams(location.search)
    return f && (!q.get('s') || (q.get('s') === f.s && (q.get('pane') ?? '') === (f.pane ?? ''))) ? f : undefined
  })(init.current.tabs.find((x) => x.id === init.current!.front))
  const [splitKey, setSplitKey] = useState<string | null>(initFront?.split ?? null)
  const [focusedPane, setFocusedPane] = useState<'primary' | 'secondary'>(initFront?.split ? (initFront.focus ?? 'primary') : 'primary')
  const [splitDir, setSplitDir] = useState<'auto' | 'row' | 'col'>(() => {
    const v = local.get('corral.convoSplitDir')
    return v === 'row' || v === 'col' ? v : 'auto'
  })
  const toggleSplitDir = (currentEffective?: 'row' | 'col') => {
    const next: 'row' | 'col' = currentEffective ? (currentEffective === 'col' ? 'row' : 'col') : splitDir === 'col' ? 'row' : 'col'
    setSplitDir(next)
    local.set('corral.convoSplitDir', next)
  }
  const splitEntry = splitKey && cur && splitKey !== cur.key ? entries.find((e) => e.key === splitKey) : undefined
  useEffect(() => {
    if (!splitKey) return
    if (cur && splitKey === cur.key) {
      setSplitKey(null)
      setFocusedPane('primary')
      return
    }
    if (!sessions || sessions.some((x) => x.persistence === 'herdr' && (x.meta?.t !== 'herdr' || x.meta.lost))) return
    if (!cur || waiting || holding) return // (the one it is beside waited for: so is it — a gateway restarting lists none)
    if (!entries.some((e) => e.key === splitKey)) {
      setSplitKey(null)
      setFocusedPane('primary')
    }
  }, [splitKey, cur, sessions, entries, waiting, holding])
  // Another agent put in front, the split's other side not set with it (picked in
  // the list, a notification, the address, back): the split was of the one
  // before — closed. Set together (swapped, opened beside its parent), kept.
  // (The split stayed on, of another agent, said the person — 11-23.)
  const splitSeen = useRef<{ cur?: string; split: string | null }>({ split: null })
  useEffect(() => {
    const was = splitSeen.current
    splitSeen.current = { cur: cur?.key ?? was.cur, split: splitKey } // (through none, as one not read yet: the one before kept — review of 11-23)
    if (splitKey && was.cur && cur && cur.key !== was.cur && cur.key !== splitKey && splitKey === was.split) {
      setSplitKey(null)
      setFocusedPane('primary')
    }
  }, [cur?.key, splitKey]) // eslint-disable-line react-hooks/exhaustive-deps
  // The tab in front keeps what is beside it (each tab its own split: 11-24).
  // (Before the keeping below, which reads it.)
  useEffect(() => {
    const id = frontRef.current
    const l = tabsRef.current
    const f = l.find((x) => x.id === id)
    if (!f || ((f.split ?? null) === splitKey && (f.focus ?? 'primary') === focusedPane)) return
    putTabs(l.map((x) => (x.id === id ? { ...x, split: splitKey ?? undefined, focus: splitKey ? focusedPane : undefined } : x)))
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [splitKey, focusedPane])
  // The tabs kept with what is shown, however it came to be shown (picked in
  // the list, an address, an alert, back and forward): one agent, one tab —
  // shown in the tab in front, that tab is of it; open in another tab, that
  // tab comes to the front; beside the one shown in a tab, that tab, its
  // focus there; else it takes the tab in front's place (what was beside
  // that goes, as it did), or is the first tab.
  useEffect(() => {
    if (!cur || (stack && screen === 'list')) return
    const l = tabsRef.current
    const f = l.find((x) => x.id === frontRef.current)
    if (f && f.key === cur.key) {
      if (f.s !== cur.s.id || (f.pane ?? '') !== (cur.pane ?? '')) putTabs(l.map((x) => (x.id === f.id ? { ...x, s: cur.s.id, pane: cur.pane } : x)))
      return
    }
    const other = l.find((x) => x.key === cur.key)
    if (other) return goTab(other)
    const beside = l.find((x) => x.split === cur.key)
    if (beside && entries.some((e) => e.key === beside.key)) return goTab(beside, 'secondary')
    if (f) putTabs(l.map((x) => (x.id === f.id ? { ...x, key: cur.key, s: cur.s.id, pane: cur.pane, title: undefined, program: undefined } : x)))
    else {
      const tab: AgentTab = { id: newTabId(), key: cur.key, s: cur.s.id, pane: cur.pane }
      putTabs([...l, tab])
      putFront(tab.id)
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [cur?.key, cur?.s.id, cur?.pane, screen])
  // A tab to the front: its agent shown, with what was beside it.
  const goTab = (x: AgentTab, focus?: 'primary' | 'secondary') => {
    putFront(x.id)
    splitSeen.current = { cur: x.key, split: x.split ?? null } // (set together: the split is this tab's, not one left from before)
    setSplitKey(x.split ?? null)
    setFocusedPane(focus ?? (x.split ? (x.focus ?? 'primary') : 'primary'))
    const p = tabPick(x)
    setPick(p)
    pickKey.current = p
    setGaveUp(false)
    named.current = false
    try {
      local.set(LAST, JSON.stringify(p))
    } catch {
      /* not kept */
    }
    if (activeRef.current) navigate(chatAddress(p), true, history.state) // (the terminals' page in front: not taken from it)
  }
  fromOther.current = (l) => {
    const was = tabsRef.current.find((x) => x.id === frontRef.current)
    putTabs(l)
    const f = l.find((x) => x.id === frontRef.current)
    if (!f || !was) return // (closed there: see below)
    // (Only a change there of this tab's own: another's, or none, leaves what is shown — one ended stays so.)
    if (f.key !== was.key) goTab(f)
    else if ((f.split ?? null) !== (was.split ?? null) && (f.split ?? null) !== splitKey) {
      splitSeen.current = { cur: f.key, split: f.split ?? null }
      setSplitKey(f.split ?? null)
      setFocusedPane(f.split ? (f.focus ?? 'primary') : 'primary')
    }
  }
  // Tabs closed (their agents go on): the one in front closed, the next to its
  // right comes, else the one to its left; none left, an empty page.
  const closeTabs = (ids: ReadonlySet<string>) => {
    const l = tabsRef.current
    const at = l.findIndex((x) => x.id === frontRef.current)
    putTabs(l.filter((x) => !ids.has(x.id)))
    if (!frontRef.current || !ids.has(frontRef.current)) return
    const next = l.slice(at + 1).find((x) => !ids.has(x.id)) ?? l.slice(0, Math.max(at, 0)).reverse().find((x) => !ids.has(x.id))
    if (next) return goTab(next)
    putFront(null)
    setSplitKey(null)
    setFocusedPane('primary')
    setPick(null)
    pickKey.current = null
    if (activeRef.current) navigate('/agents', true, history.state)
  }
  const moveTab = (id: string, to: number) => {
    const l = [...tabsRef.current]
    const from = l.findIndex((x) => x.id === id)
    if (from < 0 || to < 0 || to >= l.length || from === to) return
    l.splice(to, 0, l.splice(from, 1)[0])
    putTabs(l)
  }
  // (The tab in front gone from the list — closed in another window —: the first, or none.)
  useEffect(() => {
    if (!front || tabs.some((x) => x.id === front)) return
    if (tabs[0]) goTab(tabs[0])
    else closeTabs(new Set([front]))
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [tabs, front])
  // An address naming an agent not there (an alert of one gone): said so, and the tab in front shown again.
  useEffect(() => {
    if (!active || !unresolved || pickIsFront) return
    if (named.current) toast(t('agents.namedGone'))
    named.current = false
    const f = tabsRef.current.find((x) => x.id === frontRef.current)
    if (f) goTab(f)
    else {
      setPick(null)
      pickKey.current = null
      navigate('/agents', true, history.state)
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [active, unresolved, pickIsFront])
  // A new tab: the quick switcher, its pick opened in one (none shown, the page's list is there already).
  const pickList = useRef<HTMLDivElement>(null)
  type Drawn = { el: ReactNode; key: string; seen: number; box: KeptBox }
  // (The kept views as last put on the page. A drawing works out the next from it — what stays, what is let go —
  // and only its commit makes them so: a drawing left unfinished, as React may, lets nothing go — review 8.)
  const drawn = useRef(new Map<string, Drawn>())
  const drawing = useRef<{ next: Map<string, Drawn>; gone: KeptBox[]; kidsGone: [KeptBox, string, KeptBox][]; front: string | null } | null>(null)
  // (Let go for good: its effects cleaned up as for a view closed — what it sent stopped. One shown cleans up as
  // it goes, in the commit; one hidden had cleaned up already, putting off the stopping: done here, just after.)
  useLayoutEffect(() => {
    const w = drawing.current // (kept, not taken: done again — as React's development replay does — it does the same)
    if (!w) return
    drawn.current = w.next
    for (const [id, v] of w.next) hideBox(v.box, id !== w.front)
    for (const b of w.gone) letGoBox(b)
    for (const [box, name, kid] of w.kidsGone)
      if (box.kids.get(name) === kid) {
        box.kids.delete(name)
        letGoBox(kid)
      }
  })
  // (The page itself going — wider or narrower than 700, a sign-out —: each kept view let go with it.)
  useLayoutEffect(
    () => () => {
      for (const v of drawn.current.values()) letGoBox(v.box) // (the one shown has nothing put off: its effects clean up)
    },
    [],
  )
  const newTab = () => {
    if (!cur) {
      // (None shown: the page's list is the switcher — its first row focused (↑↓ and Return), its pick in this tab.)
      pickList.current?.querySelector<HTMLElement>('input, .convo-list-row')?.focus()
      return
    }
    askPickInTab()
    window.dispatchEvent(new Event(OPEN_PICKER))
  }
  const openSplit = (e: Entry) => {
    if (!cur) {
      choose(e)
      return
    }
    // (Open in another tab: that tab, not a second view of it.)
    const elsewhere = tabsRef.current.find((x) => x.id !== frontRef.current && (x.key === e.key || (x.split === e.key && entries.some((en) => en.key === x.key))))
    if (elsewhere) return goTab(elsewhere, elsewhere.split === e.key ? 'secondary' : undefined)
    if (e.key === cur.key) {
      setFocusedPane('primary')
      return
    }
    setSplitKey(e.key)
    setFocusedPane('secondary')
  }
  const openParentChildSplit = (parent: Entry, child: Entry) => {
    choose(parent)
    setSplitKey(child.key)
    setFocusedPane('secondary')
  }
  const closeSplit = () => {
    setSplitKey(null)
    setFocusedPane('primary')
  }
  const swapSplit = () => {
    if (!cur || !splitEntry) return
    const prevCur = cur
    const prevSplit = splitEntry
    // (The tab is of the other now, the one shown beside it: as it is, not gone to as another.)
    putTabs(tabsRef.current.map((x) => (x.id === frontRef.current ? { ...x, key: prevSplit.key, s: prevSplit.s.id, pane: prevSplit.pane, split: prevCur.key, title: undefined, program: undefined } : x)))
    choose(prevSplit)
    setSplitKey(prevCur.key)
  }
  const expandSplit = () => {
    if (!splitEntry) return
    const target = splitEntry
    setSplitKey(null)
    setFocusedPane('primary')
    choose(target)
  }
  const chooseFromList = (e: Entry, how?: PickHow) => {
    if (!how && takePickInTab()) how = 'tab' // (picked in the switcher ⌘T opened)
    if (how) return choose(e, how)
    if (splitEntry && e.key === splitEntry.key) {
      setFocusedPane('secondary')
      return
    }
    choose(e)
    setFocusedPane('primary')
  }
  // An alert tapped while a dialog was up here: held, said over the page, gone to when asked.
  const [held, setHeld] = useState(() => !!heldOpen())
  useEffect(() => onHeldOpen(() => setHeld(!!heldOpen())), [])
  // (Gone to once the list has it: before, the list of before would put another in front.)
  const [arriving, setArriving] = useState<{ s: SessionInfo; pane: string; key: string; until: number; late?: boolean; openInSplit?: boolean } | null>(null)
  const started = (s: SessionInfo, pane: string, first: string, parent?: NewSubagentParent) => {
    setNewAgent(false)
    const key = s.persistence === 'tmux' ? tmuxKey(s) : agentKey(s, pane) // (a tmux session of its own: its row)
    if (first) keepDraft('agents ' + key, first)
    const openInSplit = !stack && !!parent && !!cur && cur.s.id === parent.sessionId && cur.pane === parent.pane
    setArriving({ s, pane, key, until: Date.now() + 30000, openInSplit })
    // (Read again at once and in a second — herdr's summary takes a moment to have it: not at the next read, seconds on. 11-14.)
    readSessionsNow()
    setTimeout(readSessionsNow, 1000)
  }
  useEffect(() => {
    if (!arriving) return
    const e = entries.find((x) => x.key === arriving.key)
    if (e) {
      const asSplit = arriving.openInSplit && !stack && cur && cur.key !== e.key
      setArriving(null)
      if (asSplit) {
        setSplitKey(e.key)
        setFocusedPane('secondary')
      } else {
        choose(e)
      }
      return
    }
    // (Not there in time: said so, with a way to look again or to its terminal — not dropped without a word.)
    if (arriving.late) return
    const tm = setTimeout(() => setArriving((a) => (a && Date.now() > a.until ? { ...a, late: true } : a ? { ...a } : a)), 1000)
    return () => clearTimeout(tm)
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [arriving, entries])
  // Back to the list: back the ladder where it came from it, else in place.
  const toList = () => {
    if (fromList()) return history.back()
    setScreen('list')
    navigate('/agents', true)
  }
  // Shown without being picked (none was, its session ended, an address
  // named one gone): it is the pick from now on, so the list's order
  // changing does not move the view. One shown is remembered, however picked.
  // (One an address named — an alert's, a link, back — not there: said so,
  // not another shown as if it were it.)
  useEffect(() => {
    if (!active || !cur || (stack && screen === 'list') || holding) return // (behind a terminal tab: its address is not ours)
    if (!(pick && pick.s === cur.s.id && (pick.pane ?? '') === (cur.pane ?? ''))) {
      if (named.current && pick && !picked) toast(t('agents.namedGone')) // (found under another connection of it: no)
      named.current = false
      return choose(cur)
    }
    named.current = false
    try {
      local.set(LAST, JSON.stringify({ s: cur.s.id, pane: cur.pane, key: cur.key }))
      pickKey.current = { s: cur.s.id, pane: cur.pane, key: cur.key }
    } catch {
      /* not kept */
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [cur?.key, cur?.s.id, pick?.s, pick?.pane, screen, active, holding])
  // The address says which (and, on a phone, which screen): back and
  // forward, a link within the app, a notification tapped.
  useEffect(() => {
    const follow = () => {
      if (location.pathname !== '/agents') return
      named.current = !!new URLSearchParams(location.search).get('s')
      const p = readPick(frontPick())
      setPick((was) => (was && p && was.s === p.s && (was.pane ?? '') === (p.pane ?? '') ? was : p))
      setGaveUp(false)
      if (stack) setScreen(new URLSearchParams(location.search).get('s') ? 'chat' : 'list')
    }
    const a = onNavigate(follow)
    // A notification tapped on a phone's list: the address it wrote in place
    // of the list's goes down the ladder instead (back comes to the list).
    const b = onOpenRequest(() => {
      if (stack && screenRef.current === 'list' && location.pathname === '/agents' && new URLSearchParams(location.search).get('s')) {
        const at = location.pathname + location.search
        history.replaceState(history.state, '', '/agents')
        return navigate(at, false, FROM_LIST)
      }
      follow()
    })
    return () => {
      a()
      b()
    }
  }, [stack])
  // What was sent to each agent and is not in its log yet, and whether a
  // message to it is on its way: kept here, so going to another agent and
  // back loses neither (a second send of the same could follow).
  // So are its draft as shown, and the log's turns that confirmed one.
  const [sentBy, setSentBy] = useState<Record<string, Sent[]>>(restoreSent)
  useEffect(() => keepSent(sentBy), [sentBy])
  const [sendingBy, setSendingBy] = useState<Record<string, boolean>>({})
  const [draftBy, setDraftBy] = useState<Record<string, string>>({})
  const usedBy = useRef(new Map<string, Map<string, Set<number>>>())
  // (Of agents gone: forgotten. A draft stays in drafts.ts's keeping. What
  // was sent and not seen yet is not: an agent out of the list may be back
  // (a gateway restarted), and only its own process is shown what was sent
  // to it; it goes a day on.)
  useEffect(() => {
    if (!sessions) return
    // (Kept while a herdr summary is missing or lost: its agents may only be out of the list for now.)
    if (sessions.some((x) => x.persistence === 'herdr' && (x.meta?.t !== 'herdr' || x.meta.lost))) return
    const keys = new Set(entries.map((e) => e.key))
    const live = (k: string) => keys.has(k)
    const prune = <T,>(all: Record<string, T>) => (Object.keys(all).every(live) ? all : Object.fromEntries(Object.entries(all).filter(([k]) => live(k))))
    setSentBy((all) => {
      const old = (p: Sent) => Date.now() - p.at >= SENT_KEPT_MS
      return Object.values(all).some((ps) => ps.some(old)) ? Object.fromEntries(Object.entries(all).map(([k, ps]) => [k, ps.filter((p) => !old(p))]).filter(([, ps]) => ps.length)) : all
    })
    setSendingBy(prune)
    setDraftBy(prune)
    for (const k of usedBy.current.keys()) if (!live(k)) usedBy.current.delete(k)
  }, [sessions, entries])

  // The ones likely to be gone to next (waiting for an answer, done and not
  // seen: a few, in the list's order) read beforehand, one at a time, into
  // what is kept: opened, each shows at once. Once the one in front has had
  // its turn (a phone's list: none is in front). Each once a status: not
  // again while it stays so. Not a view of it: nothing is marked seen.
  const prefetched = useRef(new Set<string>())
  const curKey = cur?.key
  const listing = stack && screen === 'list'
  const mark = (e: Entry) => `${e.key} ${e.s.id} ${e.row?.status} ${e.row?.a.round ?? 0}`
  const want = active && sessions !== null
    ? entries.filter((e) => {
        const r = e.row
        return !!r && !r.lost && (r.status === 'blocked' || r.status === 'done') && readsLog(r.a.agent) && (listing || e.key !== curKey) && !prefetched.current.has(mark(e))
      }).slice(0, PREFETCH_MOST)
    : []
  const wantRef = useRef(want)
  wantRef.current = want
  // (By who they are: the list asked for again with the same ones goes on as it was.)
  const wantKey = want.map(mark).join('\n')
  useEffect(() => {
    if (!wantKey) return
    const ctl = new AbortController()
    const timer = setTimeout(async () => {
      for (const e of wantRef.current) {
        if (ctl.signal.aborted || document.hidden) return
        try {
          await prefetchConversation(e.s.id, e.pane, ctl.signal)
          prefetched.current.add(mark(e)) // (read, or not to be: once)
        } catch {
          if (ctl.signal.aborted) return // (asked again, with the next list)
          prefetched.current.add(mark(e)) // (failed: not tried again and again)
        }
      }
    }, listing ? 300 : PREFETCH_AFTER_MS)
    return () => {
      clearTimeout(timer)
      ctl.abort()
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [wantKey, listing])

  // The page's colours and size: the terminal's, and the visible part of
  // the window (the soft keyboard does not shrink the layout viewport on iOS).
  const rootRef = useRef<HTMLDivElement>(null)
  useEffect(() => holdTermMode(), [])
  // (Sent behind a terminal tab: a menu of its own, put over the page, goes.)
  useEffect(() => {
    if (!active) window.dispatchEvent(new Event(CLOSE_MENUS))
  }, [active])
  useEffect(() => {
    if (!active) return
    const desktopBar = !isTouch && window.matchMedia('(pointer: fine)').matches
    setChromeColor(desktopBar ? (scheme.dark ? '#15181d' : '#f6f7f9') : (scheme.theme.background ?? null))
    document.documentElement.classList.toggle('term-dark', scheme.dark)
  }, [active, scheme])
  useEffect(() => {
    const vv = window.visualViewport
    const apply = () => {
      const zoomed = vv ? (vv.scale ?? 1) > 1.01 : false
      const root = rootRef.current
      if (!root) return
      const keyboardUp = keyboardIsUp()
      root.style.height = `${Math.round(vv && !zoomed ? vv.height : window.innerHeight)}px`
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

  // What each tab says: its agent's name, program and state as the list has them; one gone, as it was last.
  const summariesMissing = !!sessions?.some(summaryMissing)
  // A tab's agent not in the list said ended only once it has been missing a
  // while (a gateway restarting lists none for a moment: review of 11-25).
  const missingSince = useRef(new Map<string, number>())
  const [missTick, setMissTick] = useState(0)
  const missingKeys = sessions && !summariesMissing ? tabs.filter((x) => !entries.some((e) => e.key === x.key)).map((x) => x.key) : []
  const missingSig = missingKeys.join('\n')
  useEffect(() => {
    const m = missingSince.current
    const now = Date.now()
    for (const k of [...m.keys()]) if (!missingKeys.includes(k)) m.delete(k)
    for (const k of missingKeys) if (!m.has(k)) m.set(k, now)
    const due = Math.min(...[...m.values()].map((at) => at + MISSING_MS - now).filter((d) => d > 0))
    if (!Number.isFinite(due)) return
    const timer = setTimeout(() => setMissTick((n) => n + 1), due + 50)
    return () => clearTimeout(timer)
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [missingSig, missTick]) // (each due in its turn: the next one set once the first has come)
  const longMissing = (k: string) => {
    const at = missingSince.current.get(k)
    return at !== undefined && Date.now() - at >= MISSING_MS
  }
  const [frontLive, setFrontLive] = useState<{ key: string; status: string; mark: AgentStatusMark } | null>(null)
  const onLiveMark = useCallback((v: { key: string; status: string; mark: AgentStatusMark } | null) => {
    setFrontLive((was) => (was?.key === v?.key && was?.status === v?.status && was?.mark === v?.mark ? was : v))
  }, [])
  const tabViews: TabView[] = tabs.map((x) => {
    const e = entries.find((en) => en.key === x.key)
    const r = e?.row
    let title = x.title ?? ''
    if (e && r) {
      const named = ((isCodex(r.a.agent) || isAgy(r.a.agent)) && r.a.thread_title) || keptTitle(titles.current, e) || paneName(r.a)
      title = labelOf(e, named || dirName(r.a.cwd) || r.a.agent, hostCount(entries), !!named).title
    } else if (e) title = keptTitle(titles.current, e) || x.title || `${e.s.host_name} · ${sessionWhich(e.s, sessions ?? [])}` // (a tmux agent out of front: no title in the list, so as last shown)
    const inFront = x.id === front
    const live = cur && x.key === cur.key && frontLive?.key === x.key ? frontLive : null
    const status = live ? live.status : (r?.status ?? 'unknown')
    const mark: AgentStatusMark = live
      ? live.mark
      : r
        ? agentStatusMark({ status: r.status, bg: r.lost ? undefined : r.bg, cmds: cmdsOf(r), tasks: tasksOf(r), failed: failedOf(r, r.status), lost: r.lost })
        : e
          ? agentStatusMark({ status: 'unknown', cmds: tmuxBgOf(e).cmds, tasks: rowTasks(tmuxBgOf(e).agents, tmuxBgOf(e).cmds) })
          : ''
    return {
      tab: x,
      title: title || t('nav.agents'),
      program: r?.a.agent ?? x.program,
      status,
      mark,
      ended: inFront ? ended : !e && !!sessions && !summariesMissing && longMissing(x.key), // (in front: as its page says, waited for as one shown)
      address: chatAddress(x),
    }
  })
  // (Its name and program kept with it: what it says once its agent is gone.)
  const viewsKey = tabViews.map((v) => (entries.some((e) => e.key === v.tab.key) ? `${v.tab.id}\t${v.title}\t${v.program ?? ''}` : '')).join('\n')
  useEffect(() => {
    const l = tabsRef.current
    const next = l.map((x) => {
      const v = tabViews.find((w) => w.tab.id === x.id)
      return v && entries.some((e) => e.key === x.key) && (v.title !== x.title || v.program !== x.program) ? { ...x, title: v.title, program: v.program } : x
    })
    if (next.some((x, i) => x !== l[i])) putTabs(next)
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [viewsKey])
  // The tab keys, in an installed app's window (a browser keeps them for its
  // own tabs), as the terminals' page has them: Ctrl+Tab and Ctrl+Shift+Tab
  // the next and previous; on a Mac ⌘1…⌘8 by place, ⌘9 the last, ⌘⇧[ ⌘⇧]; ⌘T
  // (Ctrl+Shift+T elsewhere) a new tab; Ctrl+Shift+PageUp and PageDown move
  // the one in front. No key closes one (⌘W is the window's).
  useEffect(() => {
    if (!active || !bar || stack || !standalone) return
    const onKey = (e: KeyboardEvent) => {
      if (e.altKey || e.isComposing || e.keyCode === 229 || modalShown()) return
      const l = tabsRef.current
      const at = l.findIndex((x) => x.id === frontRef.current)
      const plain = /^[\x21-\x7e]$/.test(e.key)
      const isT = e.key.toLowerCase() === 't' || (!plain && e.code === 'KeyT')
      const bracket = e.key === '{' || (!plain && e.code === 'BracketLeft') ? -1 : e.key === '}' || (!plain && e.code === 'BracketRight') ? 1 : 0
      const step = (d: number) => () => l.length > 1 && at >= 0 && goTab(l[(at + d + l.length) % l.length])
      let act: (() => void) | null = null
      if (e.key === 'Tab' && e.ctrlKey && !e.metaKey) act = step(e.shiftKey ? -1 : 1)
      else if (e.ctrlKey && e.shiftKey && !e.metaKey && (e.key === 'PageUp' || e.key === 'PageDown')) act = () => at >= 0 && moveTab(l[at].id, at + (e.key === 'PageUp' ? -1 : 1))
      else if (isMac && e.metaKey && !e.ctrlKey) {
        const digit = /^Digit([1-9])$/.exec(e.code)
        if (isT && !e.shiftKey) act = newTab
        else if (digit && !e.shiftKey) {
          const n = Number(digit[1])
          const to = n === 9 ? l[l.length - 1] : l[n - 1]
          act = () => to && to.id !== frontRef.current && goTab(to)
        } else if (bracket && e.shiftKey) act = step(bracket)
      } else if (!isMac && e.ctrlKey && e.shiftKey && !e.metaKey && isT) act = newTab
      if (!act) return
      e.preventDefault()
      e.stopPropagation()
      if (!e.repeat) act()
    }
    window.addEventListener('keydown', onKey, true)
    return () => window.removeEventListener('keydown', onKey, true)
  })
  // ⌘P / Ctrl+P with none shown (no tab, or its agent ended): the page's list
  // is the switcher, its first row focused (the conversation's own key is gone with it).
  useEffect(() => {
    if (!active || stack || cur) return
    const onKey = (e: KeyboardEvent) => {
      if (!isPickKey(e, isMac) || e.defaultPrevented || modalShown()) return
      const box = pickList.current?.querySelector<HTMLElement>('input, .convo-list-row')
      if (!box) return
      e.preventDefault()
      box.focus()
    }
    window.addEventListener('keydown', onKey, true)
    return () => window.removeEventListener('keydown', onKey, true)
  })
  let body: ReactNode
  if (stack && screen === 'list' && sessions !== null && entries.length)
    body = <PhoneList entries={entries} all={sessions} titles={titles.current} query={sideQuery} onQuery={setSideQuery} onPick={choose} />
  else if (!cur && !stack && sessions !== null && !waiting && !holding && entries.length && (ended || (hadTabs.current && !frontTab)))
    // No tab open, or the one in front's agent ended: said so, the list to open one from (its pick: in this tab).
    body = (
      <div className="agents-chat-pick" ref={pickList}>
        <div className="agents-chat-pick-note">
          <p>{t(ended ? 'agentTabs.endedNote' : 'agentTabs.closedNote')}</p>
          {ended && frontTab && (
            <button type="button" className="btn btn-ghost" onClick={() => closeTabs(new Set([frontTab.id]))}>
              {t('agentTabs.close')}
            </button>
          )}
        </div>
        <PhoneList entries={entries} all={sessions} titles={titles.current} query={sideQuery} onQuery={setSideQuery} onPick={choose} />
      </div>
    )
  else if (!cur || (stack && screen === 'list')) {
    const held = holding && !(stack && screen === 'list') // (a phone's list waits for none)
    body = (
      <div className="agents-chat-empty">
        <p>{sessions === null ? (failed ? t('agents.failed') : t('common.loading')) : waiting ? t('common.loading') : held ? t('agents.holding') : t(sessions.some((x) => x.persistence === 'herdr') ? 'agents.noneInHerdr' : 'agents.chatNone')}</p>
        {sessions !== null && held && (
          <button type="button" className="btn btn-ghost" onClick={() => setGaveUp(true)}>
            {t('agents.holdStop')}
          </button>
        )}
        {sessions !== null && !waiting && !held && sessions.some((x) => x.persistence === 'herdr') && (
          <button type="button" className="btn btn-primary" onClick={() => setNewAgent(true)}>
            {t('newAgent.title')}
          </button>
        )}
        {sessions !== null && !waiting && !held && (
          <button type="button" className="btn btn-ghost" onClick={() => navigate('/')}>
            {t('agents.toHosts')}
          </button>
        )}
      </div>
    )
  } else {
    const k = cur.key // (its state by the agent, whichever connection shows it)
    const secondaryNode = splitEntry ? (() => {
      const sk = splitEntry.key
      return (
        <KeptPart name={'split ' + sk + ' ' + splitEntry.s.id}>
        <AgentChat
          key={'split ' + sk + ' ' + splitEntry.s.id}
          cur={splitEntry}
          entries={entries}
          all={sessions ?? []}
          titles={titles.current}
          onPick={(e, how) => {
            if (how === 'tab') return choose(e, 'tab')
            if (e.key === cur.key) setFocusedPane('primary')
            else openSplit(e)
          }}
          active={active}
          keysActive={active && focusedPane === 'secondary'}
          isSecondary
          primaryKey={cur.key}
          splitKey={splitEntry.key}
          onSplitOpen={openSplit}
          onSplitClose={closeSplit}
          onSplitSwap={swapSplit}
          onSplitExpand={expandSplit}
          splitDir={splitDir}
          onSplitDirToggle={toggleSplitDir}
          onNewSubagent={(p) => setNewAgent(p)}
          toast={toast}
          composerEnter={prefs.composerEnter}
          sent={sentBy[sk] ?? []}
          setSent={(f) => setSentBy((all) => ({ ...all, [sk]: f(all[sk] ?? []) }))}
          sending={!!sendingBy[sk]}
          setSending={(v) => setSendingBy((all) => ({ ...all, [sk]: v }))}
          draft={draftBy[sk]}
          setDraftShown={(f) => setDraftBy((all) => ({ ...all, [sk]: f(all[sk]) }))}
          used={usedBy.current.get(sk) ?? usedBy.current.set(sk, new Map()).get(sk)!}
          query={sideQuery}
          onQuery={setSideQuery}
          onStarted={started}
          onClosed={(key) => {
            setGone((g) => new Set(g).add(key))
            if (splitKey === key) closeSplit()
          }}
        />
        </KeptPart>
      )
    })() : null
    body = (
      <AgentChat
        key={k + ' ' + cur.s.id}
        cur={cur}
        entries={entries}
        all={sessions ?? []}
        titles={titles.current}
        onPick={chooseFromList}
        onOpenTab={stack ? undefined : (e) => choose(e, 'tab')}
        active={active}
        keysActive={active && (!splitEntry || focusedPane === 'primary')}
        splitPane={secondaryNode ? { node: secondaryNode, focused: focusedPane, onFocus: setFocusedPane, dir: splitDir, onDirToggle: toggleSplitDir } : undefined}
        splitKey={splitEntry?.key ?? null}
        splitDir={splitDir}
        onSplitDirToggle={toggleSplitDir}
        onSplitOpen={openSplit}
        onSplitParentChild={openParentChildSplit}
        onSplitClose={closeSplit}
        onNewSubagent={(p) => setNewAgent(p)}
        onHead={onHead}
        onLiveMark={onLiveMark}
        toast={toast}
        composerEnter={prefs.composerEnter}
        sent={sentBy[k] ?? []}
        setSent={(f) => setSentBy((all) => ({ ...all, [k]: f(all[k] ?? []) }))}
        sending={!!sendingBy[k]}
        setSending={(v) => setSendingBy((all) => ({ ...all, [k]: v }))}
        draft={draftBy[k]}
        setDraftShown={(f) => setDraftBy((all) => ({ ...all, [k]: f(all[k]) }))}
        used={usedBy.current.get(k) ?? usedBy.current.set(k, new Map()).get(k)!}
        query={sideQuery}
        onQuery={setSideQuery}
        onStarted={started}
        onClosed={(key) => {
          setGone((g) => new Set(g).add(key))
          if (splitKey === key) closeSplit()
          // (A phone: back to the list, if it is still the one in front; else the next is shown.)
          if (stack && lastCur.current?.key === key) toList()
        }}
        lead={
          stack ? (
            <button type="button" className="icon-btn agents-chat-back" onClick={toList} aria-label={t('agents.backToList')} title={t('agents.backToList')}>
              <ChevronLeft size={20} />
            </button>
          ) : undefined
        }
      />
    )
  }

  // The tabs' conversations kept drawn (phase 3 of plans/agent-tabs.md): the
  // one in front with what is beside it, and each looked at since, hidden, so
  // going back to it is at once, as it was left. Past DRAWN_MAX, the one not
  // looked at longest — not one waiting for an answer — is let go (back to it,
  // it is made again from the conversations' cache).
  // (In the tab of its agent — gone to another tab from the list, the tab in front is set a drawing later: that
  // one's kept view is not taken for it — else the tab in front, whose record catches up a drawing later.)
  const drawnFront = !stack && front && cur ? (tabs.find((x) => x.key === cur.key)?.id ?? front) : null
  const next = new Map(drawn.current)
  const goneBoxes: KeptBox[] = []
  const kidsGone: [KeptBox, string, KeptBox][] = []
  const letGo = (id: string) => {
    const d = next.get(id)
    if (d) goneBoxes.push(d.box)
    next.delete(id)
  }
  if (drawnFront) {
    const d = next.get(drawnFront)
    const same = !!d && d.key === cur!.key + ' ' + cur!.s.id
    if (d && !same) letGo(drawnFront) // (another agent in it now: that one's, anew)
    const box = same ? d!.box : keptBox()
    next.set(drawnFront, { el: body, key: cur!.key + ' ' + cur!.s.id, seen: Date.now(), box })
    // (The conversation beside it changed while it was behind — from another window —: the one that was there goes
    // now, its own box let go — review 7.)
    const beside = splitEntry ? 'split ' + splitEntry.key + ' ' + splitEntry.s.id : null
    for (const [name, kid] of box.kids) if (name !== beside) kidsGone.push([box, name, kid])
  }
  for (const id of [...next.keys()]) {
    const x = tabs.find((y) => y.id === id)
    // (Closed, or of another agent now — as another window changed it —: let go.)
    if (!x || (id !== drawnFront && !next.get(id)!.key.startsWith(x.key + ' '))) letGo(id)
    // (Behind, the conversation beside it changed or closed there: the one that was, let go now, not once shown — review 10.)
    else if (id !== drawnFront) {
      const box = next.get(id)!.box
      for (const [name, kid] of box.kids) if (!x.split || !name.startsWith('split ' + x.split + ' ')) kidsGone.push([box, name, kid])
    }
  }
  if (next.size > DRAWN_MAX) {
    const blocked = (k?: string) => !!k && entries.find((e) => e.key === k)?.row?.status === 'blocked'
    const waitingOn = new Set(tabs.filter((x) => blocked(x.key) || blocked(x.split)).map((x) => x.id)) // (its agent or the one beside it)
    const old = [...next].filter(([id]) => id !== drawnFront && !waitingOn.has(id)).sort((a, b) => a[1].seen - b[1].seen)
    for (const [id] of old.slice(0, next.size - DRAWN_MAX)) letGo(id)
  }
  // (Which are hidden: set by the commit only — a drawing left unfinished sets nothing a commit of a part alone
  // would go by, review 9. Set there before the views' effects are cleaned up: useWhenGone's are passive.)
  drawing.current = { next, gone: goneBoxes, kidsGone, front: drawnFront }

  // (The workbench's rail and status bar: with the tabs, 760 and up.)
  const wbOn = wb && !!bar && !stack
  const headerBar = bar ? (
    <header className="term-bar">
      {/* (The way back to the hosts is the rail's, on the workbench.) */}
      {!wbOn && (
        <button type="button" className="icon-btn" onClick={() => navigate('/')} aria-label={t('term.backToHosts')} title={t('term.backToHosts')}>
          <ChevronLeft size={20} />
        </button>
      )}
      {bar}
      {!stack && <AgentTabBar views={tabViews} front={front} onGo={goTab} onClose={(x) => closeTabs(new Set([x.id]))} onCloseMany={(l) => closeTabs(new Set(l.map((x) => x.id)))} onMove={moveTab} onNew={newTab} newKey={standalone ? (isMac ? '⌘T' : 'Ctrl+Shift+T') : undefined} />}
    </header>
  ) : stack && screen === 'chat' && cur ? null : stack && screen === 'chat' ? (
    // (A phone's conversation, none shown in it: back to the list, as from one.)
    <header className="term-bar">
      <button type="button" className="icon-btn" onClick={toList} aria-label={t('agents.backToList')} title={t('agents.backToList')}>
        <ChevronLeft size={20} />
      </button>
      <span className="agents-chat-title">{t('nav.agents')}</span>
    </header>
  ) : (
    <header className="term-bar">
      <button type="button" className="icon-btn" onClick={() => navigate('/')} aria-label={t('common.back')} title={t('common.back')}>
        <ChevronLeft size={20} />
      </button>
      <span className="agents-chat-title">{t('nav.agents')}</span>
    </header>
  )
  const banners = (
    <>
      {/* (A list read before, not refreshed since: what it says may be past.) */}
      {failed && sessions !== null && (
        <p className="agents-chat-stale" role="status">
          {t('agents.failed')}
        </p>
      )}
      {held && (
        <p className="agents-chat-stale is-warn" role="status">
          {t('agents.alertHeld')}
          <button type="button" className="link-btn agents-chat-stale-act" onClick={() => takeHeld()}>
            {t('agents.alertGo')}
          </button>
          <button type="button" className="link-btn agents-chat-stale-act" onClick={() => takeHeld(true)}>
            {t('common.close')}
          </button>
        </p>
      )}
      {/* (A new agent on its way: said over the page wherever the person is, until it shows in the list.) */}
      {arriving && (
        <p className={'agents-chat-stale' + (arriving.late ? ' is-warn' : '')} role="status">
          {arriving.late ? (
            <>
              {t('newAgent.late')}
              <button type="button" className="link-btn agents-chat-stale-act" onClick={() => setArriving((a) => a && { ...a, late: false, until: Date.now() + 30000 })}>
                {t('newAgent.again')}
              </button>
              <button type="button" className="link-btn agents-chat-stale-act" onClick={() => navigate(`/t/${arriving.s.id}?pane=` + encodeURIComponent(arriving.pane))}>
                {t('convo.showTerminal')}
              </button>
              <button type="button" className="link-btn agents-chat-stale-act" onClick={() => setArriving(null)}>
                {t('common.close')}
              </button>
            </>
          ) : (
            t('newAgent.arriving')
          )}
        </p>
      )}
    </>
  )
  const termBody = (
    <div className="term-body">
      {[...next].map(([id, v]) => (
        // (The tab in front shown as just made; the others as they were last, hidden: their effects — reading, timers,
        // a live terminal — stopped by React, their state and drawing kept, nothing made again as the list changes.)
        <Activity key={id} mode={id === drawnFront ? 'visible' : 'hidden'}>
          <KeptView.Provider value={v.box}>{id === drawnFront ? body : v.el}</KeptView.Provider>
        </Activity>
      ))}
      {!drawnFront && body}
    </div>
  )
  return (
    <div className={'term-root agents-chat ' + (scheme.dark ? 'dark' : 'light') + (active ? '' : ' inactive') + (stack ? ' is-stack is-' + screen : '') + (wbOn ? ' wb-on' : '')} inert={!active} aria-hidden={active ? undefined : true} style={chromeVars(scheme) as React.CSSProperties} ref={rootRef}>
      {newAgent && sessions && (
        <NewAgentDialog
          sessions={sessions}
          parent={typeof newAgent === 'object' ? newAgent : undefined}
          onDone={started}
          onOpen={(s, pane) => (setNewAgent(false), choose({ s, pane, key: agentKey(s, pane) }))}
          onClose={() => setNewAgent(false)}
        />
      )}
      {/* (On the workbench: the sidebar runs full-height on the left beside the rail, and the tab bar sits over the conversation area on the right.) */}
      {wbOn ? (
        <div className="wb-agent-layout">
          <div className="wb-side-slot" />
          <div className="wb-agent-main">
            {headerBar}
            {banners}
            {termBody}
          </div>
        </div>
      ) : (
        <>
          {headerBar}
          {banners}
          {termBody}
        </>
      )}
      {wbOn && <Rail active="agents" />}
      {wbOn && (
        <StatusBar>
          {cur && (
            <StatusItem mark={failed ? 'trouble' : 'on'} title={failed ? t('agents.failed') : undefined}>
              {cur.s.host_name}
              {cur.s.persistence && cur.s.persistence !== 'none' ? ' · ' + cur.s.persistence : ''}
            </StatusItem>
          )}
          {cur && <CorraldStatusBarItem hostId={cur.s.host_id} hostName={cur.s.host_name} />}
          {/* (The conversation in front fills this: its branch, the quota.) */}
          <span className="wb-status-slot" />
        </StatusBar>
      )}
    </div>
  )
}

// A title read of an agent's log, with what its pane's title was then and
// its session: a pane whose title has changed since (renamed, cleared), or
// that herdr says runs another session, is named by that, not by what was
// read before.
type Titles = Map<string, { title: string; pane: string; session: string }>
const keptTitle = (titles: Titles, e: Entry): string | undefined => {
  const k = titles.get(e.key)
  if (!k || (e.row && paneTitle(e.row.a.title) !== k.pane)) return undefined // (its spinner aside)
  return e.row?.a.session && k.session && e.row.a.session !== k.session ? undefined : k.title
}

// (was: the draft it went from, when whether it went is not known: seen in the log, that draft goes.
// afterTs: the time of the log's last entry when it went; read anew (other
// seqs), only a turn after that can be it.)
// (wentSeen: when one found gone to a subagent was first shown, its note let go of WENT_SHOWN_MS after.)
type Sent = ConvoPending & { bind: string; after: number; gen: number; at: number; was?: string; afterTs?: string; beforeQueued?: number[]; looked?: boolean; wentSeen?: number; sinceAt?: number /* (the agent's state's time as the gateway said it when sent) */; relook?: number /* (looked at again: a queued one, its turn over) */ }

// What was sent and not seen in the log yet, as this tab kept it (the page
// reloaded meanwhile): looked for again in all of the log, by its time as
// well as its text. One on its way when the page went is not known to have
// gone: it is waited for as any other. (A day on, it is let go.)
// Read beforehand (see above): at most so many at a time, and so long
// after the one in front was opened (its own read goes first).
const PREFETCH_MOST = 3
const PREFETCH_AFTER_MS = 1500

const SENT_KEPT_MS = 24 * 3600 * 1000
// A queue's rows as submissions: placed by their time, apart from the seqs by
// their sign, and of the same millisecond by their order. Not another session's, nor a notice.
function queueRows(queued: { text: string; ts?: string; from?: string; note?: boolean }[] | undefined): Submission[] {
  const seen = new Map<number, number>()
  return (queued ?? []).flatMap((q) => {
    const ms = q.ts ? Date.parse(q.ts) : NaN
    if (q.from || q.note || isNaN(ms)) return []
    const n = seen.get(ms) ?? 0
    seen.set(ms, n + 1)
    return [{ key: -(ms * 100 + n), ts: q.ts, text: q.text }]
  })
}
// (A Codex's commands that only show something in its terminal, and their menu words.)
const CODEX_LOOKS: [string, 'ops.codexStatus' | 'ops.codexPs' | 'ops.codexDiff' | 'ops.codexMcp'][] = [
  ['/status', 'ops.codexStatus'],
  ['/ps', 'ops.codexPs'],
  ['/diff', 'ops.codexDiff'],
  ['/mcp', 'ops.codexMcp'],
]
// (See the note on these where they are used.)
const LOOK_AFTER_MS = 4000
const LOOK_WITHIN_MS = 60000 // (later, as a page opened anew: a screen of now says nothing of then)
const QUIET_AFTER_MS = 30000
const COMMAND_NOTE_MS = 8000
// One found gone to a subagent (its own log has it): said for sure, and
// that subagent has dealt with it since — its note is let go of this long
// after it was first shown (not sent again by itself: an old instruction).
const WENT_SHOWN_MS = 10 * 60 * 1000

// (Claude Code's commands in the conversation's menu: each by its icon.)
const OP_ICONS = { compact: <FoldVertical size={16} />, rename: <Pencil size={16} />, clear: <MessageSquarePlus size={16} /> }
// ('unseen': as a tab kept it before 11-06.)
const SENT_STATES: readonly string[] = ['sending', 'waiting', 'stuck', 'unseen']
function restoreSent(): Record<string, Sent[]> {
  const out: Record<string, Sent[]> = {}
  for (const [k, v] of Object.entries(keptSent())) {
    const ps = (v as Partial<Sent>[])
      .filter((p) => !!p && typeof p.id === 'number' && typeof p.text === 'string' && typeof p.bind === 'string' && typeof p.at === 'number' && Date.now() - p.at < SENT_KEPT_MS && SENT_STATES.includes(p.state as string))
      // (A look at the screen under way when the page went is not asked again: the
      // screen of a later moment says nothing of that one.)
      .map((p) => ({ ...(p as Sent), state: p.state === 'stuck' ? ('stuck' as const) : ('waiting' as const), look: p.look && typeof p.look === 'object' && p.look.went ? p.look : undefined, looked: true, gen: -1, after: -Infinity }))
    if (ps.length) out[k] = ps
  }
  return out
}

// When a message last went to each agent (by its entry key), for the while
// its one button is not stop (see stopShown).
const lastSends = new Map<string, number>()
const STOP_AFTER_SEND_MS = 1500

// Which messages the pane was already looked at for, by their id: kept for
// the page's life, not the view's, so that leaving an agent and coming
// back does not ask the host the same thing again.
const lookedAt = new Set<number>()

// A tmux session's background commands, as its conversation last said
// them (only one read is told them: the list has no word of a tmux
// session's agent otherwise): its rows show them a while after, the phone
// list once gone back to, the side list once another is shown.
// (TMUX_CMDS_FRESH: how long that holds: the gateway looks once a minute.)
const TMUX_CMDS_FRESH = 2 * 60 * 1000
const tmuxCmds = new Map<string, TmuxBgCacheEntry>()
// The host's home folder, by session (asked once: the files list of no
// path answers with it), to make "~/…" whole. (Kept once had: the same
// account's home does not move.)
const homes = new Map<string, string | Promise<string>>()
// A path relative to the agent's folder ("src/foo.ts"), made whole with that
// folder as it is now — and looked for there first: the agent may have
// named it from another (a worktree, a folder gone to since), and a file not
// there is said so, not opened as another or as nothing. Its folder listed
// to find it (one the host lists only in part, and does not show it in that
// part, is taken as there: the act itself then says).
// (No folder of its row — a tmux session's has no row: its pane's, the
// conversation's own, asked of the host; never the focused one's, which may
// be another's — review, 11-15. No pane known: not known.)
async function underCwd(sid: string, cwd: string | undefined, pane: string | undefined, rel: string): Promise<string> {
  if (!cwd && pane) cwd = await api<{ path?: string }>('GET', `/api/sessions/${sid}/files/cwd?pane=` + encodeURIComponent(pane)).then((r) => r.path, () => undefined)
  if (!cwd?.startsWith('/')) throw new Error(t('convo.path.noCwd'))
  const base = cwd.replace(/\/+$/, '') // ("/": "", the root's files "/x")
  const full = base + '/' + rel
  const dir = full.slice(0, full.lastIndexOf('/')) || '/'
  const name = full.slice(full.lastIndexOf('/') + 1)
  const gone = () => new Error(t('convo.path.notUnder', { path: rel, cwd: base || '/' }))
  return api<{ entries?: { name: string; type: string }[]; truncated?: boolean }>('GET', `/api/sessions/${sid}/files?path=` + encodeURIComponent(dir)).then(
    (r) => {
      const e = r.entries?.find((x) => x.name === name)
      if (e ? e.type === 'dir' : !r.truncated) throw gone()
      return full
    },
    (e) => {
      throw e instanceof ApiError && (e.code === 'file_not_found' || e.code === 'not_a_folder' || e.status === 404) ? gone() : new Error(errorText(e))
    },
  )
}
function atHome(sid: string, p: string): Promise<string> {
  if (p !== '~' && !p.startsWith('~/')) return Promise.resolve(p)
  const join = (home: string) => home.replace(/\/+$/, '') + p.slice(1) || '/'
  let h = homes.get(sid)
  if (typeof h === 'string') return Promise.resolve(join(h))
  if (!h) {
    h = api<{ path: string }>('GET', `/api/sessions/${sid}/files`).then((r) => {
      if (!r.path?.startsWith('/')) throw new Error('no home')
      homes.set(sid, r.path)
      return r.path
    })
    h.catch(() => homes.delete(sid))
    homes.set(sid, h)
  }
  return h.then(join)
}

const tmuxBgOf = (e: Entry) => (e.row || e.pane ? {} : readTmuxBg(tmuxCmds, e.key, Date.now(), TMUX_CMDS_FRESH))

// The conversation of one agent, and what is sent to it. Keyed by the
// agent: nothing of another's (its draft, what was sent, a stop) carries over.
function AgentChat({ cur, entries, all, titles, onPick, onOpenTab, active, keysActive, isSecondary, primaryKey, splitPane, splitKey, splitDir, onSplitDirToggle, onSplitOpen, onSplitParentChild, onSplitClose, onSplitSwap, onSplitExpand, onNewSubagent, onHead, onLiveMark, toast, composerEnter, sent, setSent, sending, setSending, draft: shownDraft, setDraftShown, used, lead, onClosed, onStarted, query, onQuery }: {
  onHead?: (h: AgentsHead | null) => void
  onLiveMark?: (m: { key: string; status: string; mark: AgentStatusMark } | null) => void
  cur: Entry
  entries: Entry[]
  all: SessionInfo[]
  titles: Titles
  sent: Sent[]
  setSent: (f: (ps: Sent[]) => Sent[]) => void
  sending: boolean
  setSending: (v: boolean) => void
  draft: string | undefined // (undefined: as kept in drafts.ts)
  setDraftShown: (f: (d: string | undefined) => string) => void
  used: Map<string, Set<number>> // per reading: the turns that confirmed one
  lead?: ReactNode
  onClosed: (key: string) => void // (a pane closed from here: this one's, or another's from its row's menu)
  onStarted: (s: SessionInfo, pane: string, first: string, parent?: NewSubagentParent) => void // (a copy of one started: gone to, its first message in its composer)
  query: string // the side list's search
  onQuery: (q: string) => void
  onPick: (e: Entry, how?: PickHow) => void
  onOpenTab?: (e: Entry) => void
  active: boolean
  keysActive?: boolean
  isSecondary?: boolean
  primaryKey?: string
  splitPane?: ConvoSplitPane
  splitKey?: string | null
  splitDir?: 'auto' | 'row' | 'col'
  onSplitDirToggle?: (currentEffective?: 'row' | 'col') => void
  onSplitOpen?: (e: Entry) => void
  onSplitParentChild?: (parent: Entry, child: Entry) => void
  onSplitClose?: () => void
  onSplitSwap?: () => void
  onSplitExpand?: () => void
  onNewSubagent?: (parent: NewSubagentParent) => void
  toast: (s: string, error?: boolean, action?: ToastAction) => void
  composerEnter: string
}) {
  const kActive = keysActive ?? active
  const download = useDownload() // (a host file saved, and said so: picture.tsx)
  const { boot } = useSession()
  // (A task its side-list row hangs under it, tapped: for the conversation to open — UI item 8.)
  const taskAsked = useTaskAsked(cur.key)
  const [portsOpen, setPortsOpen] = useState(false)
  const [shareOpen, setShareOpen] = useState(false)
  const code = useCode()
  const refreshCode = code.refresh
  const codeHost = boot.code ? cur.s.host_id : undefined
  useEffect(() => {
    if (codeHost) refreshCode([codeHost])
  }, [codeHost, refreshCode])
  const sid = cur.s.id
  const herdr = cur.s.persistence === 'herdr'
  const convo = useConversation(sid, active, cur.pane)
  // A title given with /rename, shown at once — the log says it when Claude
  // Code writes it, a while on (11-14) — until the log says the same, or a
  // while at most.
  const [titleNow, setTitleNow] = useState<{ of: string; title: string; until: number } | null>(null)
  const convoOf = convo.bind ? `${convo.bind.pane} ${convo.bind.session}` : ''
  const convoTitle = titleNow && titleNow.of === convoOf && titleNow.title !== convo.title && Date.now() < titleNow.until ? titleNow.title : convo.title
  const row = cur.row
  const [hostHistory, setHostHistory] = useState(false)
  // A file the conversation names, shown in the files panel (its folder, it marked).
  const [filesAt, setFilesAt] = useState<string | null>(null)
  const [filesIn, setFilesIn] = useState(false) // (filesAt a folder to open, not a file to show in its folder)
  // A picture of its host looked at.
  const [viewing, setViewing] = useState<string | null>(null)
  const [viewSet, setViewSet] = useState<string[] | null>(null) // (the pictures sent with the one looked at, gone through there)
  // A text file of its host read in the page.
  const [reading, setReading] = useState<string | null>(null)
  const [readingEnd, setReadingEnd] = useState(false) // (looked at from its end: a command's output)
  const [readingAt, setReadingAt] = useState<LineSpan | undefined>() // (the lines it was opened at, scrolled to and marked)
  // (A path under the home folder, "~/…": made whole with the host's home
  // first. Only the latest asked is acted on, and not once the agent or the
  // page is left meanwhile: a slow first answer of the home must not open
  // an older one over a newer — review.)
  const actN = useRef(0)
  const activeNow = useRef(active)
  activeNow.current = active
  useEffect(() => void actN.current++, [sid, active])
  // (The agent's folder as it is when a relative path is acted on.)
  // (The dock's Files tab, while the conversation has one: where a text file looked at, a folder, go — dock.tsx.)
  const dock = useRef<DockSink | null>(null)
  const cwdNow = useRef<{ cwd?: string; pane?: string }>({})
  cwdNow.current = { cwd: row?.a.cwd, pane: convo.bind?.pane ?? cur.pane } // (its pane: the conversation's, or the entry's shown — never the focused one)
  const fileActs = useMemo((): FileActs => {
    const whole = (p: string, then: (p: string) => void) => {
      const n = ++actN.current
      const rel = !p.startsWith('/') && p !== '~' && !p.startsWith('~/')
      ;(rel ? underCwd(sid, cwdNow.current.cwd, cwdNow.current.pane, p) : atHome(sid, p)).then(
        (q) => n === actN.current && activeNow.current && then(q),
        (e) => n === actN.current && toast(rel ? (e instanceof Error ? e.message : String(e)) : t('convo.path.noHome'), true),
      )
    }
    // (Looked at: a picture as one, anything else as text — the host says if it is. One at a time.)
    const look = (q: string, end = false, group?: string[], at?: LineSpan) => (IMAGE_EXT.test(q) ? (setReading(null), setViewSet(group && group.length > 1 ? group : null), setViewing(q)) : dock.current && !end ? dock.current.file(q, at) : (setViewing(null), setReadingEnd(end), setReadingAt(at), setReading(q)))
    // (A picture of the log alone: no file to make whole, a picture whatever its name.)
    const showPic = (q: string, group?: string[]) => (setReading(null), setViewSet(group && group.length > 1 ? group : null), setViewing(q))
    return {
      logPic: (from) => logPic(cur.pane ?? '', from),
      reveal: (p) => whole(p, (q) => (dock.current ? dock.current.files(q, false) : (setFilesIn(false), setFilesAt(q)))),
      open: (p) => whole(p, (q) => (dock.current ? dock.current.files(q, true) : (setFilesIn(true), setFilesAt(q)))),
      download: (p) => (isLogPic(p) ? download(sid, p) : whole(p, (q) => download(sid, q))),
      view: (p, group, at) => (isLogPic(p) ? showPic(p, group) : whole(p, (q) => (IMAGE_EXT.test(q) ? showPic(q, group) : look(q, false, group, at)))),
      tail: (p) => whole(p, (q) => look(q, true)),
      thumb: (p, signal) => lookAt(sid, p, { signal }),
    }
  }, [sid, cur.pane, toast, download])
  // (Another tab shown — a terminal's — or another agent: what was opened of this one's files closes.)
  useEffect(() => {
    if (active) return
    setFilesAt(null)
    setViewing(null)
    setReading(null)
  }, [active])
  useEffectOnChange(() => {
    setFilesAt(null)
    setViewing(null)
    setReading(null)
  }, [sid])
  // Single reconciled status & background-task view shared by both the
  // conversation header and the sidebar's row for this agent.
  const convoWorkingSince = useMemo(() => convoLastUserMs(convo.entries) ?? (convo.changedAt || undefined), [convo.entries, convo.changedAt])
  const liveConvoRunning = useMemo(() => {
    if (convo.status !== 'ready' || convo.failing || !convo.bind) return undefined
    if (herdr && convo.bind.pane !== cur.pane) return undefined
    return convo.running
  }, [convo.status, convo.failing, convo.bind, convo.running, herdr, cur.pane])
  const reconciled = useMemo(() => {
    if (!herdr || !row) return null
    const raw = (['blocked', 'working', 'done', 'idle', 'unknown'] as const).includes(row.a.status as AgentStatus) ? (row.a.status as AgentStatus) : 'unknown'
    return reconcileAgentStatus({
      herdrStatus: row.status,
      herdrRawStatus: raw,
      herdrSince: row.a.since,
      herdrLost: row.lost,
      bg: row.lost ? undefined : row.bg,
      bgChecking: row.a.bg === 'checking',
      bgSince: row.a.bg_since,
      convoLive: !row.lost && !convo.failing && convo.bind?.pane === row.a.pane_id,
      convoAgent: convo.bind?.agent,
      convoStatus: convo.agentStatus,
      convoWaitingFor: convo.waitingFor,
      convoWorkingSince,
    })
  }, [herdr, row, convo.failing, convo.bind, convo.agentStatus, convo.waitingFor, convoWorkingSince])
  // What the bar says of it: reconciled from herdr and the live conversation
  // for a herdr agent; a tmux one's from its process file / log.
  const agent = useMemo((): ConvoAgent | null => {
    if (!herdr) {
      if (!convo.bind) return null
      const st = !convo.failing && convo.agentStatus
      // (Claude Code's session file: what it waits for. A question of the
      // log's not answered yet, where that file says nothing.)
      const asks = st === 'waiting' ? answerable(convo.waitingFor) : !!st && asking(convo.entries)
      const dialog = st === 'waiting' && !asks
      const status = asks ? 'blocked' : st === 'busy' ? 'working' : st === 'idle' || dialog ? 'idle' : 'unknown'
      const cmds = st && st !== 'waiting' ? convo.bgCmds : undefined
      const bgAgents = st && st !== 'waiting' ? convo.bgAgents : undefined
      return { title: convoTitle || convo.bind.title, status, dialog, cmds, tasks: mergeRowTasks(rowTasks(bgAgents, cmds), liveConvoRunning, { blocked: status === 'blocked' }), program: convo.bind.agent }
    }
    if (!row || !reconciled) return null
    // (A Codex's: its thread's name, as the gateway has it, before its log's
    // first message, which a session imported or compacted begins with what
    // is no name.)
    const title = ((isCodex(row.a.agent) || isAgy(row.a.agent)) && row.a.thread_title) || (convo.bind?.pane === row.a.pane_id && convoTitle) || paneName(row.a) || dirName(row.a.cwd)
    const tasks = mergeRowTasks(tasksOf(row), liveConvoRunning, { blocked: reconciled.sideStatus === 'blocked', lost: row.lost })
    return { where: row.ws === '~' ? '' : row.ws, title, status: reconciled.viewStatus, bg: reconciled.bg, dialog: reconciled.dialog, cmds: cmdsOf(row), tasks, other: readsLog(row.a.agent) ? undefined : row.a.agent, program: row.a.agent }
  }, [herdr, row, reconciled, convo.bind, convo.agentStatus, convo.waitingFor, convo.bgCmds, convo.bgAgents, convo.failing, convoTitle, convo.entries, liveConvoRunning])
  // (The tab of the agents' page says which conversation is in front.)
  useEffect(() => {
    onHead?.(agent ? { title: agent.title ?? '', program: agent.program, status: agent.bg && agent.status !== 'blocked' ? 'bg' : agent.status } : null)
    return () => onHead?.(null) // (none in front any more — the last closed, its session gone: the tab says so)
  }, [onHead, agent?.title, agent?.program, agent?.status, agent?.bg]) // eslint-disable-line react-hooks/exhaustive-deps
  // (Report the reconciled front-agent status mark back to AgentsChat so the front tab always matches its sidebar row.)
  const liveRowStatus = row ? (reconciled ? reconciled.sideStatus : row.status) : (agent?.status ?? 'unknown')
  const liveRowMark: AgentStatusMark = row
    ? agentStatusMark({
        status: liveRowStatus,
        bg: reconciled ? reconciled.bg : (row.lost ? undefined : row.bg),
        cmds: cmdsOf(row),
        tasks: tasksOf(row),
        failed: failedOf(row, liveRowStatus as AgentStatus),
        lost: row.lost,
      })
    : agentStatusMark({ status: liveRowStatus, cmds: agent?.cmds ?? tmuxBgOf(cur).cmds, tasks: agent?.tasks })
  useEffect(() => {
    if (!onLiveMark) return
    onLiveMark({ key: cur.key, status: liveRowStatus, mark: liveRowMark })
    return () => onLiveMark(null)
  }, [onLiveMark, cur.key, liveRowStatus, liveRowMark])
  // A finished one looked at here is seen: the gateway keeps it, so it is
  // no longer "done" in any browser (herdr's own word goes only with its
  // pane focused in herdr). Not while the page is behind another.
  // (Of the round it is done in, as told: a word that comes late, after
  // another round, marks nothing; a round after it is marked in its turn.)
  // (Not while what it left running is read for, nor while it is out.)
  // (What this tmux session's agent left running, kept for its row: see
  // tmuxCmds. Only as of a read this view made while looked at, this time —
  // a view brought back shows what it read before, till it reads again —
  // and dated by that read: as the agent's words change, and as the view
  // goes (reads that change nothing change no state). review483–486)
  const tmuxKeyNow = !herdr && !cur.pane ? cur.key : ''
  const tmuxCmdsNow = agent?.cmds
  const tmuxAgentsNow = !herdr && convo.agentStatus !== 'waiting' ? convo.bgAgents : undefined
  const lookedSince = useRef(0)
  useEffect(() => {
    if (active) lookedSince.current = Date.now()
  }, [active])
  const readAt = convo.readAt
  useEffect(() => {
    if (!tmuxKeyNow || !active || convo.status === 'loading' || convo.failing) return
    const key = tmuxKeyNow
    const keep = () => {
      const at = readAt()
      if (at < lookedSince.current) return // (none read since it was brought back)
      keepTmuxBg(tmuxCmds, key, tmuxCmdsNow, tmuxAgentsNow, at)
    }
    keep()
    return keep
  }, [tmuxKeyNow, tmuxCmdsNow, tmuxAgentsNow, convo.status, convo.okAt, convo.failing, active, readAt])
  // (Seen only once its conversation is in front and read afresh since it was
  // gone to — what was kept from before shows at once, and is no proof the
  // person sees what it did since; not while it fails to load — review B02.)
  // (A visit begins again when the page comes back to the front, the agents
  // page is gone back to, or this one finishes another round: each needs a
  // read of its own — review of 97ebccc.)
  const [visibleAt, setVisibleAt] = useState(() => Date.now())
  useEffect(() => {
    const on = () => !document.hidden && setVisibleAt(Date.now())
    document.addEventListener('visibilitychange', on)
    return () => document.removeEventListener('visibilitychange', on)
  }, [])
  useEffect(() => void (active && setVisibleAt(Date.now())), [active])
  const shownSince = useRef({ key: '', at: 0 })
  const visit = [cur.key, cur.s.id, row?.a.round ?? 0, row?.a.session ?? ''].join(' ')
  if (shownSince.current.key !== visit) shownSince.current = { key: visit, at: Date.now() }
  const shownAt = useShownAt() // (shown again from a tab behind: a read before then is not of this showing)
  const freshSince = Math.max(shownSince.current.at, visibleAt, shownAt)
  // (One whose conversation cannot be read — a Codex that keeps no log
  // open, say — shows only that, whatever a read brings: in front a moment,
  // it is looked at. Not waited for a read, which can take seconds there,
  // and the person gone on meanwhile: it stayed "done" however often it was
  // opened.)
  const shownNone = convo.status === 'none' && Date.now() - freshSince >= NONE_SEEN_AFTER_MS
  const doneHere = herdr && active && !document.hidden && someoneHere() && (convo.status === 'ready' || convo.status === 'none') && !convo.failing && (convo.readAt() >= freshSince || shownNone) && row && !row.lost && row.status === 'done' && row.a.status === 'done' && agent?.status === 'done' && !row.bg && row.a.bg !== 'checking' ? { pane: row.a.pane_id, round: row.a.round ?? 0, session: row.a.session ?? '', fresh: freshSince } : undefined
  const doneKey = doneHere && JSON.stringify(doneHere)
  // (Done and in front, its fresh read not in yet: looked at again every 2s —
  // a read that brought nothing new draws nothing, so nothing else asks.)
  const [, recheck] = useReducer((n: number) => n + 1, 0)
  const awaitingRead = herdr && active && !doneHere && row?.status === 'done' && row.a.status === 'done' && !row.bg
  useEffect(() => {
    if (!awaitingRead) return
    const timer = setInterval(recheck, 2000)
    return () => clearInterval(timer)
  }, [awaitingRead])
  useEffect(() => {
    if (!doneKey) return
    const { pane, round, session } = JSON.parse(doneKey) as { pane: string; round: number; session: string }
    seenSent.current = JSON.stringify({ sid, pane, round, session })
    api('POST', `/api/sessions/${sid}/herdr/panes/${encodeURIComponent(pane)}/seen`, { round, session }).catch(() => {})
  }, [sid, doneKey])
  // Gone on from a "done" one before its conversation came (a read there
  // can take seconds: a Codex that keeps no log open, say), after it was in
  // front a moment: what it is is read afterwards, and where there is no
  // conversation to read — all its view would have shown — the round it was
  // in front for is marked seen. One with a conversation is not: that is to
  // be seen. (The gateway marks nothing of a round over meanwhile.)
  const doneVisit = herdr && active && !document.hidden && row && !row.lost && row.status === 'done' && row.a.status === 'done' && agent?.status === 'done' && !row.bg ? JSON.stringify({ sid, pane: row.a.pane_id, round: row.a.round ?? 0, session: row.a.session ?? '' }) : ''
  const seenSent = useRef('')
  const convoStatus = useRef(convo.status)
  convoStatus.current = convo.status
  useEffect(() => {
    if (!doneVisit) return
    const began = Date.now()
    return () => {
      // (Whatever showed instead of a conversation — reading, none, a read
      // refused while the host was busy — is read afresh: only one shown is
      // left to the view's own rule.)
      // (Nor where nobody was at the window as it went — hidden, say, with no one there.)
      if (seenSent.current === doneVisit || convoStatus.current === 'ready' || Date.now() - began < NONE_SEEN_AFTER_MS || !someoneHere()) return
      seenIfNone(JSON.parse(doneVisit))
    }
  }, [doneVisit])
  // Looked at while its subagents have been out too long: no longer among
  // those to look at, on this device (see agents.ts withBg).
  const staleAt = herdr && active && row && !row.lost && row.bg === 'stale' ? (row.a.bg_since ?? 0) : 0
  const staleOf = row?.key ?? ''
  useEffect(() => {
    if (!staleAt) return
    const mark = () => !document.hidden && ackBg(staleOf, staleAt)
    mark()
    document.addEventListener('visibilitychange', mark)
    return () => document.removeEventListener('visibilitychange', mark)
  }, [staleOf, staleAt])
  // (A new conversation, its log not written yet, takes its first message.)
  const bound = (convo.status === 'ready' || (convo.status === 'none' && convo.reason === 'no_log')) && !!convo.bind && (!herdr || convo.bind.pane === cur.pane)
  // (The answer card reads the pane's screen: it needs no log read.)
  // (An answer card: herdr's, or, where Claude Code's file says it waits for an answer, the conversation's.)
  const waitsFor = herdr ? !!row && agent?.status === 'blocked' && !agent.other : !!convo.bind && convo.agentStatus === 'waiting' && answerable(convo.waitingFor)
  // (Claude Code's, its process known: through the conversation, which
  // checks on the host, right before the keys, that this process still
  // waits for an answer on the screen shown; a Codex in tmux likewise (its
  // title says it waits). Another agent in herdr: herdr's.)
  const viaConvo = !!convo.bind && (convo.bind.agent === 'claude' ? !herdr || convo.bind.pane === row?.a.pane_id : (convo.bind.agent === 'codex' || convo.bind.agent === 'agy') && !herdr)
  // What the log says it asks: the agent's last row, where that is a
  // question of Claude Code's or a plan not come back (anything after it,
  // a message of the person's above all: none).
  const asked = useMemo(() => {
    for (let i = convo.entries.length - 1; i >= 0; i--) {
      const e = convo.entries[i]
      if (e.gone || (e.role === 'note' && !e.parts.some((p) => p.text))) continue // (a notice: troubles, events)
      if (e.role !== 'assistant') return undefined
      const last = e.parts[e.parts.length - 1]?.tool
      if (last && !last.done && (last.name === 'AskUserQuestion' || last.name === 'ExitPlanMode')) return last
      // (Codex's plan: words of its after it in the same message — review p4-3.)
      const plan = e.parts.findLast((p) => p.tool)?.tool
      return plan && !plan.done && plan.name === 'ExitPlanMode' && convo.bind?.agent === 'codex' ? plan : undefined
    }
    return undefined
  }, [convo.entries, convo.bind?.agent])
  // (Any call of the agent's not come back yet: what a permission is of.)
  const pendingCall = useMemo(() => {
    for (let i = convo.entries.length - 1; i >= 0; i--) {
      const e = convo.entries[i]
      if (e.gone || (e.role === 'note' && !e.parts.some((p) => p.text))) continue
      if (e.role !== 'assistant') return undefined
      const last = e.parts[e.parts.length - 1]?.tool
      return last && !last.done ? last : undefined
    }
    return undefined
  }, [convo.entries])
  const replyTo = useMemo(
    () => {
      let to = viaConvo && convo.bind ? convoTarget(sid, convo.bind, cur.s.persistence === 'tmux' || cur.s.persistence === 'herdr') : herdr && row ? herdrTarget(sid, row.a) : null
      // (OpenCode's in herdr, answered by herdr: where what it asks is not
      // made out, its terminal here as it is — not its screen as words.)
      const b = convo.bind
      if (to && !to.live && b && (b.agent === 'opencode' || b.agent === 'agy') && b.pane === row?.a.pane_id) {
        to = { ...to, live: { sid, press: () => ({ pane: b.pane, pid: b.pid, start: b.start, session: b.session }) } }
      }
      // (The host it runs on, for the card's offer of Claude Code's question hook
      // there — terminal/askHook.tsx: only for one whose log is read, so found
      // under ~/.claude, the settings the hook goes in — review535-8.)
      return to && viaConvo && convo.bind ? { ...to, host: { id: cur.s.host_id, name: cur.s.host_name } } : to
    },
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [viaConvo, herdr, cur.s.persistence, cur.s.host_id, cur.s.host_name, sid, row?.a.pane_id, row?.a.agent, convo.bind?.pane, convo.bind?.pid, convo.bind?.start, convo.bind?.session, convo.bind?.agent],
  )
  const working = agent?.status === 'working'
  // (Answered from the card: till the agent's state moves on — read every few
  // seconds — the page says so, not "answer the question above".)
  const [answered, setAnswered] = useState(false)
  if (answered && agent?.status !== 'blocked') setAnswered(false)
  const bindKey = convo.bind ? `${convo.bind.pane} ${convo.bind.pid} ${convo.bind.start} ${convo.bind.session}` : ''
  // (A Codex's first message goes before its session is known: sent to the
  // zero one, it is of the session the same process then has.)
  const zeroKey = convo.bind ? `${convo.bind.pane} ${convo.bind.pid} ${convo.bind.start} ${NO_SESSION}` : ''
  const ofThis = (bind: string) => bind === bindKey || (!!zeroKey && bind === zeroKey)

  // Where the one shown is placed on the lists while it is (see asHeld): as
  // it was when it was gone to, or the page came back to, once its state is
  // known. (Not again till then: looked at or answered, it would move.)
  const [placedAs, setPlacedAs] = useState<ConvoListRow['placeAs']>(() => (carry().placed?.key === cur.key ? carry().placed!.as : undefined))
  useEffect(() => void (carry().placed = placedAs ? { key: cur.key, as: placedAs } : undefined), [placedAs, cur.key])
  // The list beside the thread (or from the bar on a narrow screen).
  // Pinned at the top (prefs, so on every device of the account): by key,
  // which no reconnect or gateway restart changes (see agentKey, tmuxKey).
  const pins = usePrefs().pinnedAgents
  const pinAt = (key: string) => (pins.includes(key) ? pins.indexOf(key) : undefined)
  // A row's menu (the side list's), of whichever agent it is: as of now, not
  // of when the rows were made (set below, with the bar's menu).
  const rowMenu = useRef<(e: Entry) => (MenuItem | null)[]>(() => [])
  const rows = useMemo((): ConvoListRow[] => {
    const hosts = hostCount(entries)
    const wsByKey = new Map<string, string>()
    for (const e of entries) if (e.row) wsByKey.set(e.key, e.row.wsId)
    const out = entries.map((e): ConvoListRow => {
      const current = e.key === cur.key && e.s.id === cur.s.id
      if (current && convoTitle) {
        titles.delete(e.key) // (the latest last: the map keeps its order of use)
        titles.set(e.key, { title: convoTitle, pane: paneTitle(e.row?.a.title ?? ''), session: convo.bind?.session ?? '' })
        if (titles.size > 100) titles.delete(titles.keys().next().value!)
      }
      if (e.row) {
        const r = e.row
        const named = ((isCodex(r.a.agent) || isAgy(r.a.agent)) && r.a.thread_title) || (current && convo.bind?.pane === r.a.pane_id && convoTitle) || keptTitle(titles, e) || paneName(r.a)
        const said = named || dirName(r.a.cwd) || r.a.agent
        const st = current && reconciled ? reconciled.sideStatus : r.status
        const bg = current && reconciled ? reconciled.bg : (r.lost ? undefined : r.bg)
        const since = current && reconciled ? reconciled.since : (r.lost ? r.a.since : r.bg ? r.a.bg_since : r.a.since)
        const tasks = current ? mergeRowTasks(tasksOf(r), liveConvoRunning, { blocked: st === 'blocked', lost: r.lost }) : tasksOf(r)
        const parentKey = r.lost || !r.a.parent_pane ? undefined : agentKey(e.s, r.a.parent_pane)
        const sameWsParent = !!parentKey && wsByKey.get(parentKey) === r.wsId
        return { key: e.key, ...labelOf(e, said, hosts, !!named, sameWsParent), now: r.lost || bg || st !== 'blocked' ? undefined : glanceOf(e.s.id, e.pane, st, r.a.since) || undefined /* (said only of one waiting on the person) */, program: r.a.agent, status: st, liveStatus: st, lost: r.lost, lastStatus: r.lost && r.last && r.last !== 'unknown' ? r.last : undefined, lostAt: r.lost ? r.lostAt : undefined, bg, failed: failedOf(r, st), model: r.lost ? undefined : r.a.model, glance: r.lost || bg ? undefined : () => glanceOf(e.s.id, e.pane, st, r.a.since), cmds: cmdsOf(r), tasks, since, current, inSplit: !isSecondary && !!splitKey && e.key === splitKey, pinned: pinAt(e.key), projectKey: normalizeProjectPath(r.a.cwd), sessionGroup: sessionGroupOf(e, all), parentKey, lineageSource: r.lost ? undefined : r.a.lineage_source, onPick: (how) => onPick(e, how), menu: () => rowMenu.current(e) }
      }
      const title = (current && agent?.title) || keptTitle(titles, e) || `${e.s.host_name} · ${sessionWhich(e.s, all)}`
      const tmuxStatus = current && agent ? agent.status : 'unknown'
      const tmuxBg = current ? { cmds: agent?.cmds, agents: convo.bgAgents } : tmuxBgOf(e)
      const tmuxTasks = current ? mergeRowTasks(rowTasks(tmuxBg.agents, tmuxBg.cmds), liveConvoRunning, { blocked: tmuxStatus === 'blocked' }) : rowTasks(tmuxBg.agents, tmuxBg.cmds)
      return { key: e.key, title, where: whereOf(e, hosts), program: current ? agent?.program : undefined, status: tmuxStatus, liveStatus: tmuxStatus, cmds: tmuxBg.cmds, tasks: tmuxTasks, since: current && tmuxStatus === 'working' ? convoWorkingSince : undefined, current, inSplit: !isSecondary && !!splitKey && e.key === splitKey, pinned: pinAt(e.key), sessionGroup: sessionGroupOf(e, all), onPick: (how) => onPick(e, how), menu: () => rowMenu.current(e) }
    })
    return placedAs ? out.map((r) => (r.current ? { ...r, placeAs: placedAs } : r)) : out
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [entries, cur, agent, reconciled, liveConvoRunning, convoWorkingSince, convoTitle, all, titles, onPick, placedAs, pins, splitKey, isSecondary])
  const shown = rows.find((r) => r.current)
  const known = !!shown && shown.status !== 'unknown'
  useEffect(() => {
    if (!active) return setPlacedAs(undefined)
    if (!known || !shown) return
    setPlacedAs((p) => {
      if (!p) return shown.status === 'done' ? { status: shown.status, since: shown.since, cmds: shown.cmds } : undefined
      if (p.status === 'done' && shown.status !== 'done' && shown.status !== 'idle') return undefined
      return p
    })
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [active, known, shown?.status])

  // Stop and "send now": pressed by the gateway in the agent's pane, for the
  // process the conversation shows; what became of it is said for a while.
  const [said, setSaid] = useState<{ what: 'stop' | 'now'; text: string; busy: boolean; n: number; bind: string; took?: boolean } | null>(null)
  const saidN = useRef(0)
  // (A stop that took — the agent no longer at work — is said a moment more,
  // not the full while: else "stop sent" stood over its next turn, begun at
  // once, as if that one were being stopped. Once seen to take, so it stays:
  // its next turn, begun meanwhile, does not keep it up.)
  if (said?.what === 'stop' && !said.busy && !said.took && agent?.status !== 'working') setSaid({ ...said, took: true })
  useEffect(() => {
    if (!said || said.busy) return
    const timer = setTimeout(() => setSaid((s) => (s?.n === said.n ? null : s)), said.took ? 1500 : said.what === 'stop' ? 15000 : 8000)
    return () => clearTimeout(timer)
  }, [said])
  const liveUiNow = useRef({ waits: false, panelOpen: false })
  const applyLive = (code: string | undefined, agentKind?: string): ToastAction | undefined => {
    const decision = decideLiveTerminal({
      code,
      agent: agentKind,
      hasBind: !!convo.bind,
      waits: liveUiNow.current.waits,
      panelOpen: liveUiNow.current.panelOpen,
    })
    if (decision === 'refresh') convo.refresh()
    else if (decision === 'open') setLiveOf(bindKey)
    else if (decision === 'hint') return { label: t('convo.showTerminal'), run: () => setLiveOf(bindKey) }
    return undefined
  }
  const refusal = (e: unknown, apply = true) => {
    if (!(e instanceof ApiError)) return errorText(e)
    if (apply) applyLive(e.code, convo.bind?.agent)
    // (Claude Code's input box not this conversation's: said what to do in the terminal.)
    const v = viewRefusal(e.code)
    if (v) return v
    if (e.code === 'agent_changed') {
      convo.refresh() // (read at once: the gateway looks again for what the pane runs)
      return t('convo.stopChanged')
    }
    if (e.code === 'not_in_front') return t('convo.notFront')
    if (e.code === 'agent_waiting' && agent?.dialog) return t('agents.dialogOpen')
    // (Of these the gateway says only in English: said here.)
    if (e.code === 'refused') return t('convo.err.refused')
    if (e.code === 'picker') return t(codex ? 'convo.err.codexPicker' : 'convo.err.picker')
    if (e.code === 'default_changed') return t('convo.err.defaultChanged')
    if (e.code === 'default_settling') return t('convo.err.defaultSettling')
    if (e.code === 'default_pending') return t('convo.err.defaultPending')
    if (e.code === 'model_elsewhere') return t('convo.err.modelElsewhere')
    if (e.code === 'draft') return t('convo.err.draft')
    if (e.code === 'box_draft') return t('convo.err.boxDraft', { draft: String(e.extra.draft ?? '') })
    if (e.code === 'screen_changed') return t('convo.err.screenChanged')
    if (e.code === 'working') return t('convo.err.working')
    if (e.code === 'unknown') return t('convo.err.unknown')
    if (e.code === 'host_error' || e.code === 'unsupported' || e.code === 'bad_request') return t('convo.err.other')
    return errorText(e)
  }
  const toastRefusal = (e: unknown) => {
    const action = e instanceof ApiError ? applyLive(e.code, convo.bind?.agent) : undefined
    toast(refusal(e, false), true, action)
  }
  const press = (what: 'stop' | 'now') => {
    const b = convo.bind
    if (!b || (said?.busy && said.bind === bindKey)) return
    const n = ++saidN.current
    const bind = bindKey // (what becomes of it is of that process: another in the pane shows none of it)
    setSaid({ what, text: t(what === 'stop' ? 'convo.stopping' : 'convo.sendingNow'), busy: true, n, bind })
    const settle = (text: string) => setSaid((cur) => (cur?.n === n ? { what, text, busy: false, n, bind } : cur))
    api('POST', `/api/sessions/${sid}/conversation/${what === 'stop' ? 'interrupt' : 'send-now'}`, { pane: b.pane, pid: b.pid, start: b.start, session: b.session }, { timeoutMs: PRESS_MS })
      .then(() => settle(t(what === 'stop' ? 'convo.stopped' : 'convo.sentNow')))
      .catch((e) => settle(refusal(e)))
  }
  // (Devin's conversation is only read here: nothing is sent to it, nor pressed in it.)
  const devin = convo.bind?.agent ? convo.bind.agent === 'devin' : !!row && isDevin(row.a.agent)
  const canPress = bound && working && !devin
  // A dialog of Claude Code's own open (/usage, a picker): Escape closes it,
  // pressed from here only when asked (it may be in use in the terminal),
  // and by the host only while one is still open (closed there meanwhile,
  // it would stop a turn begun since).
  const [shutting, setShutting] = useState(false)
  const closeDialog = () => {
    const b = convo.bind
    if (!b || shutting) return
    setShutting(true)
    // (Its session file says so a moment after: read then, not at the next
    // tick; pressed again only once that read had time to come.)
    const settle = () => setTimeout(() => {
      convo.refresh()
      setTimeout(() => setShutting(false), 1500)
    }, 700)
    api('POST', `/api/sessions/${sid}/conversation/close-dialog`, { pane: b.pane, pid: b.pid, start: b.start, session: b.session }, { timeoutMs: PRESS_MS })
      .then(settle)
      .catch((e) => {
        if (!(e instanceof ApiError && e.code === 'no_dialog')) toastRefusal(e) // (closed already: the read says so)
        settle()
      })
  }
  // (What Claude Code alone has: its permission modes, its model picker, its "send now".)
  // (Bound, the agent it is bound to says; the list's, of a reading before, only till then.)
  const codex = convo.bind?.agent ? convo.bind.agent === 'codex' : !!row && isCodex(row.a.agent)
  const agy = convo.bind?.agent ? convo.bind.agent === 'agy' : !!row && isAgy(row.a.agent)
  const opencode = convo.bind?.agent ? convo.bind.agent === 'opencode' : !!row && isOpenCode(row.a.agent)
  // (Claude Code's own keys and pickers: not Codex's, OpenCode's, Antigravity's, nor Devin's.)
  const notClaude = convo.bind?.agent ? convo.bind.agent !== 'claude' : !!row && (isCodex(row.a.agent) || isOpenCode(row.a.agent) || isAgy(row.a.agent) || isDevin(row.a.agent))

  // The permission mode, switched from here: Shift+Tab pressed in its pane,
  // the mode it went to read off its screen (the log says it only with the
  // next message: from then on, that is what shows). Not while it waits for
  // an answer: the key would be the question's.
  // (last: the log's last message of the person's then, of that reading; one
  // after it, or another reading, the log has said the mode since.)
  const [modeNow, setModeNow] = useState<{ mode: string; bind: string; was?: string; last: number; gen: number } | null>(null)
  const [modeBusy, setModeBusy] = useState(false)
  const lastSeq = convo.entries.findLast((e: ConvoEntry) => e.role === 'user')?.seq ?? 0
  // (A Codex's: its collaboration mode, plan or default — Shift+Tab goes from one to the other; its log says it at once.)
  const modeWas = codex ? convo.meta?.collab : convo.meta?.mode
  if (modeNow && (modeNow.bind !== bindKey || modeWas !== modeNow.was || lastSeq > modeNow.last || convo.gen !== modeNow.gen)) setModeNow(null)
  const canMode = bound && (!notClaude || codex || agy || opencode) && convo.status === 'ready' && !!convo.meta && agent?.status !== 'blocked' && !agent?.other
  // To the mode picked: Shift+Tab on the host until its screen shows it
  // (one it was not started with is not there: it is left as it was).
  // (The log as it is when a switch is answered: what it says by then is
  // of before the switch, and goes over it only once it changes again.)
  const logNow = useRef({ bind: bindKey, was: modeWas, last: lastSeq, gen: convo.gen })
  logNow.current = { bind: bindKey, was: modeWas, last: lastSeq, gen: convo.gen }
  const pickMode = (target: string) => {
    const b = convo.bind
    if (!b || modeBusy) return
    const bind = bindKey
    setModeBusy(true)
    const name = (m: string) => t(`convo.mode.${m}` as I18nKey)
    const shown = (mode: string) => {
      const n = logNow.current
      if (n.bind === bind) setModeNow({ mode, bind, was: n.was, last: n.last, gen: n.gen })
    }
    // (Pressed, and where it went not known: not shown as the mode it was.)
    const unknown = () => shown('unknown')
    api<{ ok: boolean; mode?: string; pressed?: boolean; missing?: boolean; incomplete?: boolean }>('POST', `/api/sessions/${sid}/conversation/mode`, { pane: b.pane, pid: b.pid, start: b.start, session: b.session, mode: target }, { timeoutMs: MODE_MS })
      .then((r) => {
        if (!r.pressed && !r.mode) return toast(t('convo.modeUnread'), true)
        if (r.mode) shown(r.mode)
        else unknown()
        if (r.missing) toast(t('convo.modeMissing', { mode: name(target) }), true)
        else if (r.incomplete) toast(t('convo.modeIncomplete', { mode: name(target), now: r.mode ? name(r.mode) : name('unknown') }), true)
        else if (!r.mode) toast(t('convo.modeUnknown'))
      })
      .catch((e) => {
        // (Unless it was refused before any key went, where it is is not known.)
        if (!(e instanceof ApiError && NOTHING_PRESSED.includes(e.code))) unknown()
        toastRefusal(e)
      })
      .finally(() => setModeBusy(false))
  }

  // The model, switched from a menu of what Claude Code's picker offers (read
  // when it opens, through the picker: see convo_model.go), for this session
  // only. Only while it is at rest: the picker takes over its input.
  const [models, setModels] = useState<{ list: ModelOpt[] | null; bind: string; at?: number; unread?: boolean; error?: string; byLog?: boolean } | null>(null)
  const [modelBusy, setModelBusy] = useState(false)
  // (Said once: a switch here left the default of new sessions changed, as
  // where its request was given up and it was found after.)
  const warnedDefault = useRef(false)
  useEffect(() => {
    if (!convo.defaultChanged || warnedDefault.current) return
    warnedDefault.current = true
    toast(t('convo.err.defaultChanged'), true)
  }, [convo.defaultChanged, toast])
  const [modelNow, setModelNow] = useState<{ model: string; bind: string; was?: string } | null>(null)
  if (modelNow && (modelNow.bind !== bindKey || convo.meta?.model !== modelNow.was)) setModelNow(null)
  const canModel = bound && (!notClaude || codex || agy || opencode) && convo.status === 'ready' && !!convo.meta?.model && (agent?.status === 'idle' || agent?.status === 'done') && !agent?.other
  // How hard it is asked to think, set through Claude Code's /effort (or agy's /effort, or OpenCode's model variant),
  // for this session only. Offered only where its log has said one (or on agy/opencode with a model).
  // (effortNow: what a switch took, until the log says it — an answer
  // names the level, and /effort writes no row of its own. Dropped when
  // the conversation changes under it, or the log says another.)
  const [effortNow, setEffortNow] = useState<{ effort: string; bind: string; was?: string } | null>(null)
  const [effortBusy, setEffortBusy] = useState(false)
  if (effortNow && (effortNow.bind !== bindKey || convo.meta?.effort !== effortNow.was)) setEffortNow(null)
  const canEffort = bound && ((!notClaude && !!convo.meta?.effort) || ((agy || opencode) && !!convo.meta?.model)) && convo.status === 'ready' && (agent?.status === 'idle' || agent?.status === 'done') && !agent?.other
  const pressBody = () => {
    const b = convo.bind!
    return { pane: b.pane, pid: b.pid, start: b.start, session: b.session }
  }
  // Going through a picker of its own from here (a model, an effort): that it
  // has a dialog open then is this, not a panel to show.
  const [ownPicker, setOwnPicker] = useState(0)
  const own = <T,>(p: Promise<T>): Promise<T> => {
    setOwnPicker((n) => n + 1)
    return p.finally(() => setOwnPicker((n) => n - 1))
  }
  // A panel of its own open, for longer than one of those takes to go by
  // (and a moment after it): shown here, and worked from here.
  const panelWanted = !!agent?.dialog && !!convo.bind && ownPicker === 0
  const [panelOpen, setPanelOpen] = useState(false)
  const panelShutAt = useRef(0)
  // (The terminal as it is, shown here: a message refused, or asked for. Of this agent, till closed.)
  const [liveOf, setLiveOf] = useState<string | null>(null)
  // (Before what waits on it reads it, below: effects run in order.)
  const panelHad = useRef(false)
  useEffect(() => {
    if (panelHad.current && !panelOpen) panelShutAt.current = Date.now()
    panelHad.current = panelOpen
  }, [panelOpen])
  useEffect(() => {
    if (!panelWanted) return setPanelOpen(false)
    const h = setTimeout(() => setPanelOpen(true), PANEL_AFTER_MS)
    return () => clearTimeout(h)
  }, [panelWanted, bindKey])
  const pickEffort = (level: string) => {
    if (!convo.bind || effortBusy) return
    const bind = bindKey
    const was = convo.meta?.effort
    setEffortBusy(true)
    own(api<{ effort?: string }>('POST', `/api/sessions/${sid}/conversation/effort`, { ...pressBody(), effort: level }, { timeoutMs: MODEL_MS }))
      .then((r) => {
        const took = r.effort || level
        setEffortNow({ effort: took, bind, was })
        toast(t('convo.effortSet', { e: t(`convo.effort.${took}` as I18nKey) }))
      })
      .catch((e) => toastRefusal(e))
      .finally(() => setEffortBusy(false))
  }

  // Do not share a host-level frontend cache for Codex: Codex processes on the
  // same host may use different CODEX_HOME or provider paths while sharing the
  // same via hostname and empty auth, and the gateway already serves Codex
  // lists in one fast path (server cache or models_cache.json).
  const modelCacheKey = agy ? `agy:${cur.s.host_id}` : opencode ? `oc:${cur.s.host_id}` : bindKey
  const openModels = (force = false) => {
    if (!convo.bind || modelBusy) return
    const now = Date.now()
    if (!force && !codex && models && models.bind === bindKey && models.list && now - (models.at ?? 0) < MODEL_CACHE_MS) return
    if (force) keptModelCache.delete(modelCacheKey)
    const hit = !force && !codex ? keptModelCache.get(modelCacheKey) : undefined
    if (hit && now - hit.at < MODEL_CACHE_MS) {
      setModels({ list: hit.list, bind: bindKey, at: hit.at, byLog: hit.byLog || agy || opencode })
      return
    }
    const bind = bindKey
    const ckey = modelCacheKey
    setModels({ list: null, bind })
    setModelBusy(true)
    // (kept: the list as the gateway kept it, its picker not opened — which
    // of them it is on is not said with it, and is marked by the
    // conversation's own log as that goes: see rowByLog.)
    own(api<{ models: ModelOpt[]; kept?: boolean; effort?: string }>('POST', `/api/sessions/${sid}/conversation/models`, pressBody(), { timeoutMs: MODEL_MS }))
      .then((r) => {
        const at = Date.now()
        if (!codex && r.models?.length) keptModelCache.set(ckey, { list: r.models, at, byLog: !!r.kept })
        setModels((m) => (m?.bind === bind ? { list: r.models, bind, at, byLog: !!r.kept } : m))
        if (agy && r.effort !== undefined) setEffortNow({ effort: r.effort, bind, was: convo.meta?.effort })
      })
      .catch((e) => {
        // (Said in the menu as well as in the toast: left on "reading" it
        // would look as though the picker were still being waited for.)
        const action = e instanceof ApiError ? applyLive(e.code, convo.bind?.agent) : undefined
        const why = refusal(e, false)
        setModels((m) => (m?.bind === bind ? { list: null, bind, unread: true, error: why } : m))
        toast(why, true, action)
      })
      .finally(() => setModelBusy(false))
  }
  const pickModel = (label: string, effort?: string) => {
    if (!convo.bind || modelBusy) return
    const bind = bindKey
    const ckey = modelCacheKey
    const wasModel = convo.meta?.model
    const wasEffort = convo.meta?.effort
    setModelBusy(true)
    own(api<{ model?: string; effort?: string; restored?: boolean; unconfirmed?: boolean }>('POST', `/api/sessions/${sid}/conversation/model`, { ...pressBody(), model: label, ...(effort ? { effort } : {}) }, { timeoutMs: MODEL_MS }))
      .then((r) => {
        toast(t('convo.modelSet', { m: effort ? `${label} · ${effort}` : label }) + (r.restored ? t('convo.modelRestored') : '') + (r.unconfirmed ? t('convo.modelUnconfirmed') : ''))
        // (Not said by its log: the list's mark is left where it was, not moved by a guess.)
        // (A list marked by the log — see rowByLog — is marked by it alone: read at once, not at the next tick,
        // for what was just switched to. Nothing is kept here of what was clicked: the log says it, as it says a
        // /model typed in the terminal.)
        if (!r.unconfirmed) {
          setModels((m) => {
            if (!m) return m
            const next = m.list && m.list.map((o) => ({ ...o, current: o.label === label }))
            if (!codex && next?.length) keptModelCache.set(ckey, { list: next, at: Date.now(), byLog: !!m.byLog })
            return { ...m, list: next }
          })
          if (agy || opencode) {
            setModelNow({ model: r.model || label, bind, was: wasModel })
            setEffortNow({ effort: r.effort ?? effort ?? '', bind, was: wasEffort })
          }
        }
        convo.refresh()
      })
      .catch((e) => toastRefusal(e))
      .finally(() => setModelBusy(false))
  }

  const queueShown = agent?.status !== 'idle' && agent?.status !== 'done'
  // What was sent, until the log shows a submission after it: see sentSeen.
  // Only what the page shows goes by that; a draft is let go of by the words.
  const sentN = useRef(0)
  // What was sent and has since been seen in the log, by its words: a request
  // of its own that ends badly is no reason to type it again.
  const arrived = useRef(new Set<number>())
  useEffect(() => {
    if (convo.status !== 'ready' || !sent.some((p) => ofThis(p.bind))) return
    const key = bindKey + ' ' + convo.gen
    let u = used.get(key)
    if (!u) {
      used.clear() // (only this reading's turns can be one)
      used.set(key, (u = new Set()))
    }
    let changed = false
    const mine = sent.map((p) => {
      if (!ofThis(p.bind)) return p
      let q = p
      if (q.bind !== bindKey) q = { ...q, bind: bindKey }
      // (The log it went to, now written, or read anew: by its time.)
      if (q.gen !== convo.gen) q = { ...q, gen: convo.gen, after: -Infinity }
      if (q !== p) changed = true
      return q
    })
    const subs: Submission[] = [
      ...convo.entries.filter((e: ConvoEntry) => e.role === 'user').map((e: ConvoEntry) => ({ key: e.seq, seq: e.seq, ts: e.ts, text: e.parts.map((x) => (x.kind === 'text' ? x.text : x.kind === 'image' ? '[Image #0]' : '')).join('\n') })),
      // (Not while the queue is not shown, or the message would show nowhere.)
      ...(queueShown ? queueRows(convo.queued) : []),
    ]
    const found = pairSent(mine.filter((p) => ofThis(p.bind)), subs, u)
    for (const { p, s } of found) {
      u.add(s.key)
      if (sameMessage(s.text, p.text)) {
        arrived.current.add(p.id)
        if (arrived.current.size > 200) arrived.current = new Set([...arrived.current].slice(-100))
        if (p.was !== undefined && getDraft(draftKey) === p.was) {
          keepDraft(draftKey, '')
          setDraftShown((d) => (d === undefined || d === p.was ? '' : d))
        }
      }
    }
    if (u.size > 200) for (const q of [...u].sort((x, y) => Math.abs(x) - Math.abs(y)).slice(0, 100)) u.delete(q)
    const gone = new Set(found.map((f) => f.p.id))
    // (Put back in its input together, sent by one Enter as one message: the
    // others in it went too, whichever of them it was paired with — the log
    // read before the Enter's answer came, the first by its own words. Only
    // where that message is exactly a run of them, one after another, as
    // Codex joins them: not one whose words merely appear in it — p14b-1.)
    for (const { p, s } of found) {
      if (!p.unqueued) continue
      const whole = squeezed(s.text)
      const group = mine.filter((q) => q.unqueued && ofThis(q.bind)).sort((a, b) => a.id - b.id)
      const at = group.findIndex((q) => q.id === p.id)
      run: for (let i = 0; i <= at; i++)
        for (let j = at; j < group.length; j++) {
          if (j === i) continue
          if (squeezed(group.slice(i, j + 1).map((q) => q.text).join('\n')) === whole) {
            for (const q of group.slice(i, j + 1)) gone.add(q.id)
            break run
          }
        }
    }
    // (When an agent — e.g. Antigravity CLI — combines multiple queued messages
    // into a single logged user turn, pairSent only pairs the first pending row
    // with that submission; clear any remaining pending rows whose content is
    // already included in a newer logged user turn.)
    const loggedSubs = subs.filter((s) => s.seq !== undefined)
    for (const q of mine) {
      if (!ofThis(q.bind) || gone.has(q.id) || isCommand(q.text)) continue
      const merged = loggedSubs.find((s) => camesAfter(s, q) && containedInMessage(s.text, q.text))
      if (!merged) continue
      gone.add(q.id)
      arrived.current.add(q.id)
      if (q.was !== undefined && getDraft(draftKey) === q.was) {
        keepDraft(draftKey, '')
        setDraftShown((d) => (d === undefined || d === q.was ? '' : d))
      }
    }
    if (gone.size || changed) {
      setSent(() => mine.filter((p) => !gone.has(p.id)))
    }
  }, [convo.entries, convo.queued, convo.gen, convo.status, sent, bindKey, queueShown])
  // Nothing in the log a moment on, which is rare (it writes a row on taking
  // Enter): the screen read once, for the one thing it can show for sure, the
  // words still in its box. A command writes no row of its own, and its note
  // is let go of soon (a panel it opened holds it, as that is its answer).
  // Later still, one quiet line: the conversation has not caught up. Nothing
  // here says a message did not arrive: nothing here can know that.
  useEffect(() => {
    const open = sent.filter((p) => p.state === 'waiting' && !p.unknownResult && ofThis(p.bind))
    if (!open.length) return
    // (A command's note, while its panel is open: held, nothing to wake for.)
    const dues = open.flatMap((p) => (isCommand(p.text) ? (panelOpen ? [] : [p.at + COMMAND_NOTE_MS]) : [...(p.looked ? [] : [p.at + LOOK_AFTER_MS]), ...(p.quiet ? [] : [p.at + QUIET_AFTER_MS])]))
    if (!dues.length) return
    const timer = setTimeout(() => {
      const now = Date.now()
      setSent((ps) => {
        const next = ps.flatMap((p) => {
          if (p.state !== 'waiting' || !ofThis(p.bind)) return [p]
          if (isCommand(p.text)) return now - p.at >= COMMAND_NOTE_MS && !panelNow.current ? [] : [p]
          let q = p
          if (!q.looked && now - q.at >= LOOK_AFTER_MS) q = { ...q, looked: true, ...(now - q.at < LOOK_WITHIN_MS ? { look: 'looking' as const } : {}) }
          if (!q.quiet && now - q.at >= QUIET_AFTER_MS) q = { ...q, quiet: true }
          return [q]
        })
        return next.length === ps.length && next.every((p, i) => p === ps[i]) ? ps : next
      })
    }, Math.max(0, Math.min(...dues) - Date.now()) + 50)
    return () => clearTimeout(timer)
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [sent, panelOpen])
  // (Which were asked after is kept outside this page: going to another
  // agent and back builds it anew, and the same read must not go twice.)
  useEffect(() => {
    for (const p of sent) {
      const lk = p.id * 10 + (p.relook ?? 0) // (each look of it once)
      if (p.look !== 'looking' || lookedAt.has(lk) || !convo.bind || !ofThis(p.bind)) continue
      lookedAt.add(lk)
      if (lookedAt.size > 200) for (const q of [...lookedAt].slice(0, 100)) lookedAt.delete(q)
      const bind = bindKey // (the answer is of that conversation, not of whichever is open when it comes)
      // (Only the look it answers: an older one answering late, after another
      // was asked or its fate known, changes nothing — review p14-3.)
      const relook = p.relook ?? 0
      const after = (f: (q: Sent) => Sent) => setSent((ps) => ps.map((q) => (q.id === p.id && q.bind === bind && (q.relook ?? 0) === relook && q.state === 'waiting' ? f(q) : q)))
      api<{ waiting: boolean; known: boolean; view?: string; went?: ConvoWent | null }>('POST', `/api/sessions/${sid}/conversation/sent-missing`, { ...pressBody(), text: p.text, ago: Date.now() - p.at }, { timeoutMs: PRESS_MS })
        .then((r) =>
          after((q) =>
            // (Found in a subagent's own log: where it went, for sure.)
            r.went
              ? { ...q, look: { went: r.went } }
              : // (Its words, on the screen, in the box of this conversation.)
                r.known && r.waiting && (!r.view || r.view === 'main')
                ? { ...q, look: undefined, state: 'stuck' as const, queued: false, ...(q.queued ? { unqueued: true } : {}) }
                : { ...q, look: undefined },
          ),
        )
        .catch(() => after((q) => ({ ...q, look: undefined })))
    }
  }, [sent, convo.bind, sid, bindKey])

  // (A Codex's message queued after its turn, the turn over — or stopped, which
  // puts what was queued back into its input, not sent —: its input looked at
  // again; its words there, said so, Enter a tap away.)
  // (Shown first not at work: looked at too — stopped while another agent was
  // shown, this page was not here to see it end — review p14-2.)
  const wasWorking = useRef(true)
  useEffect(() => {
    if (!codex || !convo.bind) return // (not yet known a Codex, nor its conversation: as if not yet shown — p14b-2)
    const ended = wasWorking.current && !working
    wasWorking.current = working
    if (!ended) return
    let done = false
    const tm = setTimeout(() => {
      done = true
      setSent((ps) => ps.map((p) => (p.queued && p.state === 'waiting' && ofThis(p.bind) ? { ...p, look: 'looking' as const, looked: true, relook: (p.relook ?? 0) + 1 } : p)))
    }, 1500)
    return () => {
      clearTimeout(tm)
      if (!done) wasWorking.current = true // (not looked yet, as a tab behind: looked when shown again)
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [working, codex, bindKey])
  const shownSent = sent.filter((p) => ofThis(p.bind))
  // Gone to a subagent: when it is first shown (this agent's view in front,
  // the page not hidden), and let go of WENT_SHOWN_MS after.
  const wentOf = (p: Sent) => !!(p.look && typeof p.look === 'object' && p.look.went)
  useEffect(() => {
    if (!active || document.hidden) return
    const now = Date.now()
    const ids = new Set(shownSent.filter((p) => wentOf(p) && !p.wentSeen).map((p) => p.id))
    if (ids.size) setSent((ps) => ps.map((p) => (ids.has(p.id) && !p.wentSeen ? { ...p, wentSeen: now } : p)))
  })
  useEffect(() => {
    const seen = sent.filter((p) => wentOf(p) && p.wentSeen)
    if (!seen.length) return
    const due = Math.min(...seen.map((p) => p.wentSeen! + WENT_SHOWN_MS)) - Date.now()
    const timer = setTimeout(() => setSent((ps) => ps.filter((p) => !(wentOf(p) && p.wentSeen && Date.now() - p.wentSeen >= WENT_SHOWN_MS))), Math.max(0, due) + 50)
    return () => clearTimeout(timer)
  }, [sent])

  // The composer: its draft kept per agent (as the terminal's per session;
  // a tmux session's per pane: another pane in front is another agent).
  const baseKey = 'agents ' + cur.key
  const draftKey = baseKey + (!herdr && convo.bind ? ' ' + convo.bind.pane : '')
  // (The box in front: what another agent's send puts back is not shown over this one's.)
  const draftKeyNow = useRef(draftKey)
  draftKeyNow.current = draftKey
  const draft = shownDraft ?? getDraft(draftKey)
  useEffect(() => {
    showDraft(draftKey, draft)
    return () => showDraft(draftKey, '')
  }, [draftKey, draft])
  // What is typed or put in (a path, a completion, a long paste) is the
  // draft, kept; an entry of what was sent, shown while going through them,
  // is not: the draft put aside stays kept (and only there, so that a send
  // of it, from here or from before, takes it), and comes back.
  const histPos = useRef<string | null>(null)
  const setDraft = (s: string) => {
    histPos.current = null
    setDraftShown(() => s)
    keepDraft(draftKey, s)
  }
  // (One picked from a history: in the composer, the cursor at its end; not sent.)
  const putBack = (h: string) => {
    showSent(h)
    requestAnimationFrame(() => {
      const el = field.current
      if (!el) return
      el.focus()
      el.selectionStart = el.selectionEnd = h.length
    })
  }
  const showSent = (h: string | null) => {
    if (h === null) return setDraft(getDraft(draftKey))
    if (histPos.current === null) keepDraft(draftKey, draft)
    histPos.current = h
    setDraftShown(() => h)
  }
  const lastKey = useRef(draftKey)
  useEffect(() => {
    const was = lastKey.current
    if (was === draftKey) return
    lastKey.current = draftKey
    histPos.current = null
    if (slashPut.current) {
      slashPut.current = false // (another agent shown with the keys: not left in front of that one's words)
      const left = unslash(getDraft(was))
      if (left !== null) keepDraft(was, left)
    }
    upAbort.current?.abort() // (what goes up has its path for the agent before)
    setUps([])
    // (Its pane first known: what was typed before was for it.)
    if (was === baseKey && !getDraft(draftKey)) {
      keepDraft(draftKey, getDraft(was))
      keepDraft(was, '')
    }
    setDraftShown(() => getDraft(draftKey))
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [draftKey])
  useEffect(
    () => () => {
      // (Another agent shown: this one's draft is as it was put aside.)
      if (histPos.current !== null) setDraftShown(() => getDraft(draftKey))
      histPos.current = null
    },
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [],
  )
  const field = useRef<HTMLTextAreaElement>(null)
  // (The box as it is now: taken out under the terminal view and made anew as it goes — its width and its height
  // measured again on the new one: review of 36e1069.)
  const [fieldEl, setFieldEl] = useState<HTMLTextAreaElement | null>(null)
  const fieldRef = useCallback((el: HTMLTextAreaElement | null) => {
    field.current = el
    setFieldEl(el)
  }, [])
  const completion = useCompletion({ sessionId: sid, field, draft, setDraft, agent: row?.a.agent ?? convo.bind?.agent ?? (agy ? 'agy' : codex ? 'codex' : 'claude'), pane: herdr ? cur.pane : convo.bind?.pane })
  // The commands button: a "/" put where a command goes, for the list that opens on one (the
  // same as typed: typing on narrows it). With words in the box it goes in front of them, the
  // command they are then for; given up (pressed again, Escape, the box left) that "/" goes.
  const slashPut = useRef(false)
  const takeSlash = () => {
    const left = slashPut.current ? unslash(draft) : null
    slashPut.current = false
    if (left === null) return false
    setDraft(left)
    requestAnimationFrame(() => field.current?.setSelectionRange(left.length, left.length))
    return true
  }
  const pressCommands = () => {
    const el = field.current
    if (!el) return
    const inIt = document.activeElement === el
    el.focus() // (now, in the press: a phone opens its keyboard only from one)
    if (inIt && draft === '/') {
      slashPut.current = false
      return setDraft('')
    }
    if (inIt && takeSlash()) return
    // (A command already there: its word, to change it. A path is not one.)
    const word = /^\s*\/[^\s/]*(?=\s|$)/.exec(draft)
    if (!word) {
      setDraft(draft ? '/ ' + draft : '/')
      slashPut.current = true
    }
    const caret = word ? word[0].length : 1
    requestAnimationFrame(() => {
      el.setSelectionRange(caret, caret)
      completion.update()
    })
  }
  // (A panel shut, its screen having had the keys: the box has them again; not
  // where the focus has gone somewhere since.)
  const panelNow = useRef(panelOpen)
  panelNow.current = panelOpen
  const panelWas = useRef(false)
  useEffect(() => {
    if (panelWas.current && !panelOpen && hasKeyboardPointer() && (document.activeElement === document.body || !document.activeElement)) field.current?.focus({ preventScroll: true })
    panelWas.current = panelOpen
  }, [panelOpen])
  // (Another agent picked with the keyboard, or the page back in front: its
  // box takes the keys, as the terminal's does. Not from a dialog over it.)
  useEffect(() => {
    if (!kActive || !hasKeyboardPointer()) return
    const frame = requestAnimationFrame(() => {
      const at = document.activeElement
      if ((!at || at === document.body) && !modalShown()) field.current?.focus()
    })
    return () => cancelAnimationFrame(frame)
  }, [kActive])
  // Text put in where the cursor is (or at the end, the field away).
  // (A path put in the composer as the agent takes one: quoted for a shell, or Gemini's @path.)
  const insertPath = (p: string) => {
    const quoted = /gemini/i.test(row?.a.agent ?? convo.bind?.agent ?? '') ? geminiPath(p) : shellPath(p)
    if (quoted) insert(quoted + ' ')
  }
  const insertPathNow = useRef(insertPath)
  insertPathNow.current = insertPath
  const dockSink = useMemo(() => ({ set: (s: DockSink | null) => void (dock.current = s), insert: (p: string) => insertPathNow.current(p) }), [])
  // (From what finishes later — a file gone up, behind or not —: the box as it is now, not as it was when it began — review 8.)
  const insertNow = useRef<{ key: string; insert: (text: string) => void } | null>(null)
  const insert = (text: string) => {
    const el = field.current
    // (No box — its tab behind —: the draft as kept, not as last drawn — one gone through history then shown — review 10.)
    const d = el ? el.value : getDraft(draftKey)
    // (Where the person is typing; else — a file come up after a tap elsewhere, a retry — at its end.)
    const typing = !!el && document.activeElement === el
    const a = typing ? el.selectionStart : d.length
    const b = typing ? el.selectionEnd : a
    setDraft(d.slice(0, a) + text + d.slice(b))
    requestAnimationFrame(() => {
      const f = field.current
      if (!f) return
      // (Not taken from where the person went meanwhile, an answer's box say: only from the composer, or nowhere.)
      const at = document.activeElement
      if (at && at !== document.body && !f.closest('.composer')?.contains(at)) return
      f.focus()
      f.setSelectionRange(a + text.length, a + text.length)
    })
  }
  // (With the draft it goes into: the one its file was for, or none — review 9; as committed, not as a drawing left
  // unfinished had it — review 10.)
  useLayoutEffect(() => void (insertNow.current = { key: draftKey, insert }))
  // Files (picked, pasted, dropped) go up to the host one at a time, each
  // path then put in the draft for the agent to read (not sent: there is
  // more to say, as a rule). Sending waits for them. 11-13: taken in while
  // others go up (they wait their turn), each shown over the box from the
  // first — how far it has gone drawn on it; one that failed left there, to
  // be tried again or taken out — not a line of its own, nor "wait" for a
  // second file.
  const [ups, setUps] = useState<(Going & { file: File; forKey: string; abort?: AbortController })[]>([])
  const up = ups.find((u) => u.state !== 'failed') ?? null // (still to go up: sending waits)
  const upN = useRef(0)
  const fileRef = useRef<HTMLInputElement>(null)
  // (Another agent shown, or the page left: what goes up is stopped, its path having nowhere to go.)
  const alive = useRef(true)
  const upAbort = useRef<AbortController | null>(null)
  const kept = useWhenGone()
  useEffect(() => {
    alive.current = true
    kept.back()
    // (Sent to a tab behind: what is going goes on up, the next after it — review 7; let go from there, stopped.)
    return () =>
      kept.gone(() => {
        alive.current = false
        upAbort.current?.abort()
      })
  }, [])
  const upload = (files: File[]) => {
    const forKey = draftKey // (its path is for this agent's draft only)
    setUps((l) => [
      ...l,
      ...files.map((f) => {
        const name = f.name || 'image'
        return { id: ++upN.current, name, image: /^image\//.test(f.type) || PICTURE_PATH.test(name), size: f.size, p: 0, state: 'wait' as const, file: f, forKey }
      }),
    ])
  }
  // (The next one up, once none is going.)
  useEffect(() => {
    if (ups.some((u) => u.state === 'going')) return
    const u = ups.find((x) => x.state === 'wait')
    if (!u) return
    if (lastKey.current !== u.forKey) return setUps((l) => l.filter((x) => x.forKey === lastKey.current))
    const abort = new AbortController()
    upAbort.current = abort
    const mine = (f: (x: (typeof ups)[number]) => (typeof ups)[number] | null) => setUps((l) => l.flatMap((x) => (x.id === u.id ? (f(x) ?? []) : [x])))
    mine((x) => ({ ...x, state: 'going', p: 0, error: undefined, abort }))
    uploadFile(sid, u.file, (p) => mine((x) => ({ ...x, p })), abort.signal).then(
      (r) => {
        if (!alive.current) return
        mine(() => null)
        const quoted = r.prefix === '@' ? geminiPath(r.path) : shellPath(r.path)
        if (lastKey.current !== u.forKey) return
        // (In the draft as a label, a chip over the box; sent as the path.)
        const into = insertNow.current
        if (!into || into.key !== u.forKey) return
        if (quoted) into.insert(attach(u.file, r.path, quoted, cur.s.host_id) + ' ')
        else toast(r.path)
      },
      (e) => {
        if (!alive.current) return
        if (e instanceof ApiError && e.code === 'aborted') return mine(() => null) // (taken out, or another agent shown)
        mine((x) => ({ ...x, state: 'failed', error: errorText(e), abort: undefined }))
      },
    )
  }, [ups])
  const retryUp = (id: number) => setUps((l) => l.map((x) => (x.id === id ? { ...x, state: 'wait', p: 0, error: undefined } : x)))
  const cancelUp = (id: number) =>
    setUps((l) => {
      l.find((x) => x.id === id)?.abort?.abort()
      return l.filter((x) => x.id !== id)
    })
  const [dragOver, setDragOver] = useState(false)
  // Going through what was sent (Up at the box's start, Down at its end).
  const historyStep = (dir: -1 | 1): boolean => {
    if (histPos.current === null && dir > 0) return false
    const h = sentHistory()
    let pos = histPos.current !== null ? h.lastIndexOf(histPos.current) : h.length
    if (pos < 0) pos = h.length
    if (dir < 0) {
      if (pos === 0) return true
      pos--
    } else pos++
    showSent(pos < h.length ? h[pos] : null)
    const text = pos < h.length ? h[pos] : getDraft(draftKey)
    requestAnimationFrame(() => {
      const el = field.current
      if (el) el.selectionStart = el.selectionEnd = dir < 0 ? 0 : text.length
    })
    return true
  }
  // Claude Code's suggestion of the next message, greyed in its input once
  // a turn ends (the terminal's composer reads it off its screen): read off
  // the pane's screen by the gateway a moment after the turn ended (then
  // again, Claude Code taking a while to think of it), shown in the box
  // while it is empty, taken with Tab or its button. Of that turn's end only.
  const endSeq = convo.entries.length ? convo.entries[convo.entries.length - 1].seq : 0
  // (A rewind takes back what is there with nothing new, at times: another turn's end too.)
  const goneN = useMemo(() => convo.entries.reduce((n: number, e: ConvoEntry) => n + (e.gone ? 1 : 0), 0), [convo.entries])
  const askKey = bound && !notClaude && active && (agent?.status === 'idle' || agent?.status === 'done') && !agent?.other ? `${bindKey} ${convo.gen} ${endSeq} ${goneN}` : ''
  // (The page come to the front again: what was left to ask for, asked.)
  const [inFront, setInFront] = useState(0)
  useEffect(() => {
    const on = () => !document.hidden && setInFront((n) => n + 1)
    document.addEventListener('visibilitychange', on)
    return () => document.removeEventListener('visibilitychange', on)
  }, [])
  // What is shown is what the turn's end's record says (told: drawn anew
  // when it changes, whichever view asked).
  const [, told] = useReducer((n: number) => n + 1, 0)
  // Claude Code's card above its box (Heads up, You should know): looked
  // for on its own times, besides what the suggestion's looks bring.
  const claudeCard = useClaudeCard(sid, bound && !notClaude && active ? `${bindKey} ${convo.gen}` : '', askKey, () => (convo.bind ? pressBody() : null))
  // Compacting (its log silent a minute or two meanwhile): read off its
  // screen while it works with nothing new; forgotten at the log's next word.
  const [compacting, setCompacting] = useState(false)
  const [retrying, setRetrying] = useState<{ n: number; of: number; why?: string } | null>(null)
  const [reviewing, setReviewing] = useState('')
  // (A Codex's goal, as its footer last said it: kept till another look says otherwise, of this agent.)
  const [goalNote, setGoalNote] = useState<{ g: string; bind: string } | null>(null)
  const compactNow = useRef({ changedAt: convo.changedAt, body: pressBody })
  compactNow.current = { changedAt: convo.changedAt, body: pressBody }
  // (Not while a tool it called runs, the long silences: it compacts only
  // between them.)
  const toolOut = useMemo(() => callOut(convo.entries), [convo.entries])
  // (A Codex's also while a call of its is out: its approvals reviewer looks
  // at that call's command then — its screen's word, its log's none.)
  const lookCompact = bound && (!notClaude || codex) && !!active && working && (!toolOut || codex)
  useEffect(() => {
    setCompacting(false)
    setRetrying(null)
    setReviewing('')
  }, [convo.changedAt, bindKey, lookCompact])
  useEffect(() => {
    if (!lookCompact) return
    let live = true
    let busy = false
    // (Quiet from when it went to work, as well as from its log's last word:
    // compacting while idle (Claude Code's, 54 minutes on, before the prompt
    // cache lapses) begins long after the log's last word, 11-15. Once seen
    // compacting, looked at till it is done, however long: up to 7 minutes.)
    const from = Date.now()
    let seenAt = -1
    const gone = new AbortController() // (a look under way when this ends: let go, not left holding the pane's turn)
    const timer = setInterval(() => {
      const at = compactNow.current.changedAt
      const quiet = Date.now() - Math.max(at, from)
      if (busy || document.hidden || quiet < (codex ? CODEX_QUIET : COMPACT_QUIET) || (quiet > COMPACT_UNTIL && seenAt !== at)) return
      busy = true
      // (for=busy: said apart from the suggestion's own looks, in the gateway's log; not read.)
      api<{ compacting?: boolean; retrying?: { n: number; of: number; why?: string }; reviewing?: string; goal?: string }>('POST', `/api/sessions/${sid}/conversation/suggestion?for=busy`, compactNow.current.body(), { timeoutMs: PRESS_MS, signal: gone.signal })
        .then(
          (r) => {
            if (live && codex) setGoalNote(r.goal ? { g: r.goal, bind: bindKey } : null)
            if (!live || compactNow.current.changedAt !== at) return
            seenAt = r.compacting || r.retrying || r.reviewing ? at : -1
            setCompacting(!!r.compacting)
            setRetrying(r.retrying && r.retrying.n > 0 ? r.retrying : null)
            setReviewing(r.reviewing ?? '')
          },
          () => {},
        )
        .finally(() => {
          busy = false
        })
    }, codex ? CODEX_LOOK : COMPACT_LOOK)
    return () => {
      live = false
      clearInterval(timer)
      gone.abort()
    }
  }, [lookCompact, bindKey, sid, codex])
  // (A Codex given a goal — its log says it set one —: its footer looked at
  // now and then while shown, whatever it does, for how the goal goes; its
  // log says nothing of that, 0.167. Its turns run on by themselves, the log
  // never quiet: the work's own looks do not come.)
  const goalSet = codex && convo.entries.some((e) => e.parts.some((p) => p.tool && (p.tool.name === 'create_goal' || p.tool.name === 'update_goal')))
  // (Its goal set before what was read of its log: its footer looked at once as it is opened — review b2b-2.)
  const goalSeen = !!goalNote && goalNote.bind === bindKey
  const lookGoal = bound && (goalSet || goalSeen) && !!active
  useEffect(() => {
    if (!codex || !bound || !active) return
    let live = true
    api<{ goal?: string }>('POST', `/api/sessions/${sid}/conversation/suggestion?for=goal`, compactNow.current.body(), { timeoutMs: PRESS_MS }).then(
      (r) => live && r.goal && setGoalNote({ g: r.goal, bind: bindKey }),
      () => {},
    )
    return () => {
      live = false
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [codex, bound, active, bindKey, sid])
  useEffect(() => {
    if (!lookGoal) return
    let live = true
    let busy = false
    const gone = new AbortController()
    const look = () => {
      if (busy || document.hidden) return
      busy = true
      api<{ goal?: string }>('POST', `/api/sessions/${sid}/conversation/suggestion?for=goal`, compactNow.current.body(), { timeoutMs: PRESS_MS, signal: gone.signal })
        .then(
          (r) => live && setGoalNote(r.goal ? { g: r.goal, bind: bindKey } : null),
          () => {},
        )
        .finally(() => {
          busy = false
        })
    }
    look()
    const timer = setInterval(look, GOAL_LOOK)
    return () => {
      live = false
      clearInterval(timer)
      gone.abort()
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [lookGoal, bindKey, sid])
  // A Codex's plan put to the person ("Implement this plan?", plan mode):
  // neither its title nor herdr says it waits, so its menu is looked for on
  // its screen while its log has a plan not answered and it is at rest; there,
  // it is answered as any question, from its card.
  const lookPlan = bound && codex && !!active && !working && asked?.name === 'ExitPlanMode' && !asked.done
  const [planAsked, setPlanAsked] = useState(false)
  useEffect(() => {
    setPlanAsked(false)
    if (!lookPlan) return
    let live = true
    let busy = false
    const gone = new AbortController()
    const look = () => {
      if (busy || document.hidden) return
      busy = true
      api<{ plan_asked?: boolean }>('POST', `/api/sessions/${sid}/conversation/suggestion?for=plan`, compactNow.current.body(), { timeoutMs: PRESS_MS, signal: gone.signal })
        .then(
          (r) => live && setPlanAsked(!!r.plan_asked),
          () => {},
        )
        .finally(() => {
          busy = false
        })
    }
    look()
    const timer = setInterval(look, PLAN_LOOK)
    return () => {
      live = false
      clearInterval(timer)
      gone.abort()
    }
  }, [lookPlan, bindKey, sid])
  const waits = waitsFor || (lookPlan && planAsked)
  liveUiNow.current = { waits, panelOpen }
  useEffect(() => {
    if (shouldClearLiveTerminal({ waits, panelOpen }) && liveOf === bindKey) setLiveOf(null)
  }, [waits, panelOpen, liveOf, bindKey])
  useEffect(() => {
    if (!askKey) return
    let a = suggestAsks.get(askKey)
    if (!a) {
      a = { left: SUGGEST_AT.length, text: '', seq: 0, busy: false, views: new Set() }
      suggestAsks.set(askKey, a)
      if (suggestAsks.size > 50) suggestAsks.delete(suggestAsks.keys().next().value!)
    } else if (a.text && !a.busy) {
      a.left = 1 // (found before: looked at once more, as it may be gone, taken or typed over in the terminal meanwhile)
    }
    const at = a
    let live = true
    let timer: ReturnType<typeof setTimeout> | undefined
    // The next look, if any is left and none is on its way (that one, back,
    // goes on from here: see view.next).
    const next = (wait?: number) => {
      clearTimeout(timer)
      if (at.left <= 0 || at.busy) return
      const i = SUGGEST_AT.length - at.left
      timer = setTimeout(
        () => {
          // (Behind: left for when it is in front again, none used up.)
          if (!live || document.hidden || at.left <= 0 || at.busy) return
          at.left--
          at.busy = true
          const mine = ++at.seq
          const had = at.text
          const done = (found: string | null) => {
            if (at.seq !== mine) return // (one asked since says, not this)
            at.busy = false
            if (found !== null) {
              at.text = found
              if (had && !found) at.left = 0 // (gone: not looked for again this turn)
            }
            for (const v of at.views) v.told()
            if (!at.text) [...at.views][0]?.next() // (any left: looked for again, by a view still there)
          }
          const card = claudeCard.shared() // (the card above its box, read off the same screen)
          api<{ suggestion: string; card?: ClaudeCard }>('POST', `/api/sessions/${sid}/conversation/suggestion`, pressBody(), { timeoutMs: PRESS_MS }).then(
            (r) => {
              card?.took(r.card ?? null)
              done(r.suggestion)
            },
            () => {
              card?.failed()
              done(null)
            },
          )
        },
        wait ?? (at.text ? 300 : SUGGEST_AT[i] - (i ? SUGGEST_AT[i - 1] : 0)),
      )
    }
    const view = { told, next: () => next() }
    at.views.add(view)
    told()
    next()
    return () => {
      live = false
      clearTimeout(timer)
      at.views.delete(view)
      if (!at.text) [...at.views][0]?.next() // (a look of this view's to come: another view still there takes it on)
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [askKey, inFront])
  const suggestion = (askKey && suggestAsks.get(askKey)?.text) || null // (askKey: nothing in the way of sending)
  const takeSuggestion = () => {
    if (!suggestion) return
    const s = suggestion
    setDraft(s)
    requestAnimationFrame(() => {
      const el = field.current
      if (!el) return
      el.focus()
      el.selectionStart = el.selectionEnd = s.length
    })
  }
  const to = agent ? [agent.title?.trim().startsWith('<') ? '' : agent.title, agent.where].filter((x) => x && x !== '~').join(' · ') : ''
  // (A Codex at work takes a message into the turn under way, as its own
  // Enter does: it says so.)
  const steers = convo.bind?.agent === 'codex' && agent?.status === 'working'
  // (Not its name: said by the bar above and its tab — the person's ask of 11-25, doc UI item 16.)
  const fieldW = useWidth(fieldEl)
  // (With a keyboard and room for it, the keys of what its two buttons did — gone there: T3 Code's way, the person's ask of 11-23.)
  const keysSaid = !steers && fieldW >= 480 && matchMedia(KEYED_COMPOSER).matches // (as the buttons go: the same query — a touch laptop used with its trackpad too)
  const placeholder = suggestion ?? (to ? t(steers ? 'convo.toPlaceholderSteer' : keysSaid ? 'convo.toPlaceholderKeys' : 'convo.toPlaceholder') : t('term.composerPlaceholder'))
  useComposerFit(field, draft, placeholder, !!fieldEl)
  // Why it cannot send now, if it cannot.
  // (Waiting for an answer, a message's Enter could take an option of its
  // question, or of a permission it asks: the answer first.)
  // (A Devin bound: read only, for good — not a moment's wait to write through.)
  const readOnly = bound && devin && !agent?.other
  const blocked = agent?.other
    ? t('convo.otherAgent', { agent: agent.other })
    : !bound
      ? t(convo.status === 'loading' ? 'agents.loadingSend' : convo.status === 'none' && convo.reason === 'no_agent' ? (agent?.program === 'claude' ? 'agents.claudeStartingSend' : 'agents.noAgentSend') : 'agents.cannotSend')
      : devin
        ? t('convo.devinReadOnly')
        : agent?.status === 'blocked'
        ? t(waits ? 'agents.answerAbove' : 'agents.answerFirst') // (its card over the composer, or the terminal only)
        : agent?.dialog
          ? t('agents.dialogOpen') // (a message's Enter would go to it: the host refuses it too)
          : null
  // Send and stop are one button while it works: send with something to
  // send, else stop. Not stop for a moment after a send, though: the box
  // empties as it goes, and a second tap meant as send would stop it.
  // (When, kept by the agent: this view goes with another shown, and comes back.)
  const [sentAt, setSentAtState] = useState(() => {
    const at = lastSends.get(cur.key) ?? 0
    return Date.now() - at < STOP_AFTER_SEND_MS ? at : 0
  })
  const setSentAt = (at: number) => {
    lastSends.set(cur.key, at)
    setSentAtState(at)
  }
  useEffect(() => {
    if (!sentAt) return
    const timer = setTimeout(() => setSentAtState(0), Math.max(0, sentAt + STOP_AFTER_SEND_MS - Date.now()))
    return () => clearTimeout(timer)
  }, [sentAt])
  const stopShown = canPress && !draft.trim() && !up && !sending && !sentAt
  // (The question's card in the box's place: the box, its stop too, out of sight.)
  const askHides = !!(waits && replyTo && agent?.status === 'blocked' && !answered && (convo.status === 'ready' || convo.status === 'none') && !draft.trim() && !up)
  // (queue: a Codex's, queued after its turn — Tab, its own "tab to queue
  // message" — not into the turn it is at, as Enter is.)
  const send = (queue?: true) => {
    const b = convo.bind
    if (sending || (!draft.trim() && !up)) return
    if (blocked || !b) return toast(blocked ?? t('agents.cannotSend'), true)
    if (up) return toast(t('term.waitUpload'), true) // (its path is not in the draft yet; nor, while it goes up, in an empty one)
    // (A file of another host's, or one whose path is lost: not sent as it is.)
    const bad = unsendable(draft, cur.s.host_id, ATTACH_LABEL, 100 << 10)
    if (bad) return toast(bad.other ? t('attach.wrongHost', { name: bad.other.name }) : t('attach.lost', { label: bad.lost! }), true)
    // (Claude Code takes a path for a picture only alone on its line.)
    const text = (b.agent ?? 'claude') === 'claude' ? expandForClaude(draft, 100 << 10) : expandPastes(draft, 100 << 10)
    if (text === null || new TextEncoder().encode(text).length > 100 << 10) return toast(t('agents.tooLong'), true)
    const was = draft // (what is typed meanwhile is a new draft: it stays)
    setSending(true)
    setSentAt(Date.now())
    const last = convo.entries.length ? convo.entries[convo.entries.length - 1] : undefined
    const after = last ? last.seq : 0
    const afterTs = last?.ts
    const at = Date.now()
    const account = authGen() // (signed out meanwhile: nothing of it is kept for the next account)
    const focused = document.activeElement === field.current
    // It shows at once, as sending, so the box emptying is not all there is
    // to see while the gateway types it on the host; what became of it is
    // said when the answer comes.
    const id = Date.now() * 100 + (++sentN.current % 100)
    const mark = (extra: Partial<Sent>) => setSent((ps) => ps.map((p) => (p.id === id ? { ...p, ...extra } : p)))
    const drop = () => setSent((ps) => ps.filter((p) => p.id !== id))
    setSent((ps) => [...ps, { id, text, shown: was, beforeQueued: queueRows(convo.queued).map((q) => q.key), state: 'sending' as const, bind: bindKey, after, afterTs, gen: convo.gen, at, sinceAt: cur.row && !cur.row.lost ? cur.row.a.since : undefined }].slice(-5))
    // The box is emptied with it (the draft kept too, even if put aside
    // meanwhile, or shown by another of this page since); what it held comes
    // back if the message is refused.
    const went = getDraft(draftKey) === was
    if (went) keepDraft(draftKey, '')
    if (histPos.current === was) showSent(null) // (an entry sent again: the draft put aside is back)
    else if (went) setDraftShown((d) => (d === undefined || d === was ? '' : d))
    // What was typed comes back to the box it was typed in, if nothing was
    // typed there since; shown again only while that box is the one in
    // front (another agent's is its own).
    const key = draftKey
    const putBack = () => {
      // (Seen in the log since, whatever the request then said: it went.)
      if (!went || arrived.current.has(id) || getDraft(key).trim()) return
      keepDraft(key, was)
      if (draftKeyNow.current === key) setDraftShown((d) => (d === undefined || !d.trim() ? was : d))
    }

    api<{ in_box?: boolean }>(
      'POST',
      `/api/sessions/${sid}/conversation/send`,
      {
        pane: b.pane,
        pid: b.pid,
        start: b.start,
        session: b.session,
        text,
        ...(queue && b.agent === 'codex' ? { queue: true } : {}),
        ...(queue && b.agent === 'agy' ? { immediate: true } : {}),
      },
      { timeoutMs: SEND_MS },
    )
      .then((r) => {
        if (authGen() !== account) return drop()
        // (The same words were in its box already — put back there by a stop: Enter only.)
        if (r?.in_box) toast(t('convo.inBoxSent'))
        remember(was)
        // (Queued: said so once it is — Tab pressed — not before: review q-2.)
        mark({ state: 'waiting', ...(queue && b.agent === 'codex' ? { queued: true } : {}) })
        convo.refresh() // (it is in its log already: read now, not at the next tick)
      })
      .catch((e) => {
        if (authGen() !== account) {
          drop()
          return
        }
        const liveHint = e instanceof ApiError ? applyLive(e.code, b.agent) : undefined
        // Whether it went is not known (the gateway lost the host on the way,
        // or the answer was lost on its way here): it is looked for as one
        // sent, the draft staying until it is seen.
        if (e instanceof ApiError && (e.code === 'typed_not_sent' || e.code === 'typed_other_view' || e.code === 'enter_not_taken')) {
          // (In its input, not sent: it came to ask first, or the terminal went
          // to another view before Enter. Not typed again from here — sent
          // again, it would be there twice: Enter, in the terminal. Looked for
          // as one sent meanwhile.)
          toast(e.code === 'typed_not_sent' ? t('agents.typedNotSent') : e.code === 'enter_not_taken' ? t('agents.enterNotTaken') : t('convo.err.typedOtherView'), true)
          mark({ state: 'waiting', was })
          return
        }
        if (isCorraldUnknownResult(e)) {
          // Helper daemon write result unconfirmed (409 corrald_unknown_result):
          // do not auto-resend; show warning right below the message in the thread.
          mark({ state: 'waiting', unknownResult: true })
          return
        }
        if (e instanceof ApiError && (e.code === 'unknown' || e.code === 'network')) {
          // (It is looked for as one sent; the draft comes back until it is seen.)
          toast(t('agents.sendUnknown'), true, liveHint)
          putBack()
          mark({ state: 'waiting', was })
        } else {
          putBack() // (it never went: what was typed comes back, to be sent again)
          drop()
          // (A Codex read with no log, which has begun one since — its first
          // message: not typed to the one it began, which the page has not
          // read yet; read now, to be sent again there.)
          if (e instanceof ApiError && e.code === 'agent_changed' && (b.agent === 'codex' || b.agent === 'agy') && b.session === NO_SESSION) {
            convo.refresh()
            return toast(t('agents.codexBegun'), true)
          }
          // (Words typed in its terminal input: not sent onto them — what to do, said.)
          if (e instanceof ApiError && e.code === 'draft' && (b.agent === 'codex' || b.agent === 'agy')) return toast(t('agents.codexDraft'), true)
          // (Another agent in the pane now: the page reads it at once — the
          // gateway looks again for it — to be seen before sending there.)
          if (e instanceof ApiError && e.code === 'agent_changed') {
            convo.refresh()
            return toast(t('agents.changedRefresh'), true)
          }
          toast(refusal(e, false), true, liveHint)
        }
      })
      .finally(() => {
        setSending(false)
        // (Back to the box only from it, and only if the focus went nowhere meanwhile.)
        const at = document.activeElement
        if (focused && (!at || at === document.body || at === field.current)) field.current?.focus() // (null once another agent is shown)
      })
  }
  const enterKey = isMac ? 'Return' : 'Enter'
  const modEnter = isMac ? '⌘Return' : 'Ctrl+Enter'
  const sendKey = composerEnter === 'send' ? enterKey : modEnter
  const composer = (
    // (A question over it: the box kept to a line, its tools back when it is typed in.)
    // (The card asking is up: the box gives its room to the card — one line says why — rather than stand empty under it, as on a small phone it pushed the card's own buttons out of sight.)
    <div className={'composer' + (waits && agent?.status === 'blocked' ? ' is-held' : '') + (askHides ? ' is-asked' : '') + (panelOpen ? ' is-paneled' : '')}>
      {claudeCard.card && (
        <CardNotice
          card={claudeCard.card}
          stale={claudeCard.stale}
          busyKey={claudeCard.busyKey}
          error={claudeCard.error}
          errorSeq={claudeCard.errorSeq}
          onPress={claudeCard.press}
        />
      )}
      {completion.list}
      {/* (Why nothing can be sent now: over the box, where a phone shows it too;
          answered from the card, the card says what goes on.) */}
      {blocked && !(answered && agent?.status === 'blocked') && (
        <p className="agents-chat-why">
          {blocked}
          {blocked === t('agents.dialogOpen') && (
            <button type="button" className="link-btn agents-chat-why-act" disabled={shutting} onClick={closeDialog}>
              {t('agents.closeDialog')}
            </button>
          )}
          {readOnly && (
            <button type="button" className="link-btn agents-chat-why-act" onClick={() => openTerminal()}>
              {t('convo.showTerminal')}
            </button>
          )}
        </p>
      )}
      {/* (Something written while it works: what will become of it, said while it is typed — the placeholder that said so is gone by then.) */}
      {!blocked && draft.trim() && agent?.status === 'working' && <p className="agents-chat-why is-note">{t(steers ? 'convo.steerNote' : 'convo.queueNote')}</p>}
      {panelOpen && <PanelWindow key={bindKey} sid={sid} active={active} press={pressBody} onSettled={convo.refresh} onFull={() => openTerminal()} toast={toast} />}
      {!panelOpen && !waits && liveOf === bindKey && <PanelWindow key={'live ' + bindKey} sid={sid} active={active} press={pressBody} onSettled={convo.refresh} toast={toast} live={{ onClose: () => setLiveOf(null), onFull: () => openTerminal(), letters: convo.bind?.agent === 'codex' }} />}
      {/* (Devin's is only read here: no box to write in, the terminal for that.) */}
      {!readOnly && (
      <div className="composer-box">
        <AttachStrip
          draft={draft}
          sid={sid}
          host={cur.s.host_id}
          onRemove={(l) => setDraft(takeOut(draft, l))}
          onOpen={(a) => (a.image ? (setViewSet(null), setViewing(a.path)) : setFilesAt(a.path))}
          going={ups}
          onRetry={retryUp}
          onCancel={cancelUp}
        />
        <textarea
          ref={fieldRef}
          className="composer-input"
          value={draft}
          aria-label={t('term.composer')}
          autoCapitalize="off"
          autoCorrect="off"
          spellCheck={false}
          placeholder={placeholder}
          rows={1}
          onChange={(e) => setDraft(e.target.value)} // (an entry edited is a draft of its own)
          onSelect={() => completion.update()}
          {...completion.field}
          onBlur={() => {
            takeSlash()
            setTimeout(completion.update)
          }}
          onPaste={(e) => {
            const files = [...(e.clipboardData?.files ?? [])]
            if (files.length > 0) {
              e.preventDefault()
              void upload(files)
              return
            }
            // A long paste goes in as a label, as in the terminal's composer; it is sent as the text.
            const text = e.clipboardData?.getData('text/plain') ?? ''
            // (One holding a file's label is not folded: the label stays one, its chip shown.)
            if ((text.split('\n').length <= PASTE_LINES && text.length <= PASTE_CHARS) || attachedIn(text).length) return
            e.preventDefault()
            const el = e.currentTarget
            const label = keepPaste(text, (n) => t('term.pasteLabel', { n, lines: text.split('\n').length }))
            const s = el.selectionStart
            setDraft(draft.slice(0, s) + label + draft.slice(el.selectionEnd))
            requestAnimationFrame(() => field.current?.setSelectionRange(s + label.length, s + label.length))
          }}
          onKeyDown={(e) => {
            if (e.nativeEvent.isComposing || e.keyCode === 229) return
            if (e.key === 'Tab' && !e.shiftKey && !e.altKey && !e.metaKey && !e.ctrlKey && !draft && suggestion) {
              e.preventDefault()
              takeSuggestion()
              return
            }
            if (e.key === 'Escape' && completion.open && takeSlash()) {
              e.preventDefault()
              return
            }
            if (completion.onKey(e)) return
            // (Escape stops it, as in its terminal: from an empty box only, where it has no words to
            // take back, and no panel over the page is what it would be meant for.)
            // (A key held: for what it first closed, not on to stopping it.)
            if (e.key === 'Escape' && !e.repeat && !e.shiftKey && !e.altKey && !e.metaKey && !e.ctrlKey && stopShown && !firstShown(ESC_HELD)) {
              e.preventDefault()
              press('stop')
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
              send()
            } else if (agy && working && e.altKey && !e.ctrlKey && !e.metaKey && !e.shiftKey && e.key.toLowerCase() === 'm' && draft.trim()) {
              e.preventDefault()
              send(true)
            } else if (e.key === 'Enter' && hasKeyboardPointer() && composerEnter === 'send' && !e.shiftKey && !e.altKey) {
              e.preventDefault()
              send()
            }
          }}
        />
        <div className="composer-tools">
          {/* (A phone: the three as one "+", its menu a sheet of large rows — workbench 11-23.) */}
          <span className="composer-plus">
            <Menu
              label={t('composer.more')}
              items={() => {
                const recent = [...sentHistory()].reverse().slice(0, 5)
                return [
                  { label: t('term.upload'), icon: <Paperclip size={16} />, onSelect: () => fileRef.current?.click() },
                  { label: t('term.commands'), icon: <SquareSlash size={16} />, onSelect: pressCommands },
                  ...recent.map((h, i) => ({ label: h.trim().replace(/\s*\n\s*/g, ' ⏎ ').slice(0, 80), icon: i === 0 ? <History size={16} /> : <span className="menu-ic-gap" aria-hidden="true" />, separator: i === 0, onSelect: () => putBack(h) })),
                  { label: t('hostHistory.open'), icon: recent.length ? <span className="menu-ic-gap" aria-hidden="true" /> : <History size={16} />, separator: recent.length === 0, onSelect: () => setHostHistory(true) },
                ]
              }}
              button={(open, toggle) => (
                <button type="button" className="icon-btn" aria-expanded={open} aria-haspopup="menu" onClick={toggle} aria-label={t('composer.more')} title={t('composer.more')}>
                  <Plus size={20} />
                </button>
              )}
            />
          </span>
          <span className="composer-tools-full">
          <button type="button" className="icon-btn" onClick={() => fileRef.current?.click()} aria-label={t('term.upload')} title={t('term.upload')}>
            <Paperclip size={18} />
          </button>
          <Menu
            label={t('term.history')}
            note={sentHistory().length === 0 ? t('term.historyEmpty') : undefined}
            items={() => [
              ...[...sentHistory()].reverse().map((h) => ({
                label: h.trim().replace(/\s*\n\s*/g, ' ⏎ ').slice(0, 80),
                onSelect: () => putBack(h),
              })),
              // (What was sent anywhere on the host, from any device: Claude Code keeps it.)
              { label: t('hostHistory.open'), separator: sentHistory().length > 0, onSelect: () => setHostHistory(true) },
            ]}
            button={(open, toggle) => (
              <button type="button" className="icon-btn composer-history-btn" aria-expanded={open} onClick={toggle} aria-label={t('term.history')} title={t('term.history') + (isTouch ? '' : ' (↑)')}>
                <History size={18} />
              </button>
            )}
          />
          <button
            type="button"
            className="icon-btn composer-slash-btn"
            aria-haspopup="listbox"
            aria-expanded={completion.open && draft.trimStart().startsWith('/')}
            onMouseDown={(e) => e.preventDefault()} // (the box keeps the focus, and the list open)
            onClick={pressCommands}
            aria-label={t('term.commands')}
            title={t('term.commands') + (isTouch ? '' : ' (/)')}
          >
            <SquareSlash size={18} />
          </button>
          </span>
          {hostHistory && (
            <HostHistoryDialog
              sid={sid}
              cwd={row?.a.cwd}
              onPick={(h) => {
                setHostHistory(false)
                putBack(h)
              }}
              onClose={() => setHostHistory(false)}
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
          <div className="spacer" />
          {suggestion && !draft && !hasKeyboardPointer() && (
            <button type="button" className="btn btn-ghost composer-insert" onClick={takeSuggestion} title={suggestion}>
              {t('term.useSuggestion')}
            </button>
          )}
          {!blocked && hasKeyboardPointer() && (
            <span className="composer-keys" aria-hidden="true">
              {suggestion && !draft ? t('term.suggestionKey') + ' · ' : ''}
              {composerEnter === 'send' ? t('term.composerKeysSend', { send: enterKey, newline: isMac ? 'Shift+Return' : 'Shift+Enter' }) : t('term.composerKeysNewline', { send: modEnter })}
            </span>
          )}
          {/* (One in its place: stop while it works and nothing is to be
              sent, see stopShown; else send. Send with nothing in the box
              is grey, but while a file goes up: pressed, it says to wait.) */}
          {/* (A phone, its bar without a stop: one kept beside send while
              something is written, so a draft never stands between the
              person and stopping it.) */}
          {!stopShown && lead && canPress && !sending && !sentAt && (
            <button type="button" className="composer-send is-stop" onClick={() => press('stop')} aria-label={t('convo.stop')} title={t('convo.stop')}>
              <Square size={14} fill="currentColor" strokeWidth={0} />
            </button>
          )}
          <ComposerModelSlot shown={!(waits && agent?.status === 'blocked') && !askHides && !panelOpen} />
          {stopShown ? (
            <button type="button" className="composer-send is-stop" onClick={() => press('stop')} aria-label={t('convo.stop')} title={t('convo.stop') + (isTouch ? '' : ' (Esc)')}>
              <Square size={14} fill="currentColor" strokeWidth={0} />
            </button>
          ) : (
            <>
              {/* (A Codex at work: a message queued after its turn, besides one into it.) */}
              {codex && working && !!draft.trim() && !sending && !blocked && !up && (
                <button type="button" className="composer-send is-alt" onClick={() => send(true)} aria-label={t('convo.queueAfter')} title={t('convo.queueAfterHint')}>
                  <ListPlus size={16} strokeWidth={2} aria-hidden="true" />
                </button>
              )}
              {/* (An Antigravity CLI at work: Alt+M sends immediately into the current turn instead of queuing.) */}
              {agy && working && !!draft.trim() && !sending && !blocked && !up && (
                <button type="button" className="composer-send is-alt" onClick={() => send(true)} aria-label={t('convo.agySendNow')} title={t('convo.agySendNowHint')}>
                  <Zap size={16} strokeWidth={2} aria-hidden="true" />
                </button>
              )}
            <button type="button" className="composer-send" onClick={() => send()} disabled={sending || !!blocked || (!draft.trim() && !up)} aria-label={t('term.send')} title={t('term.send') + (isTouch ? '' : ' (' + sendKey + ')')}>
              <ArrowUp size={18} strokeWidth={2} />
            </button>
            </>
          )}
        </div>
      </div>
      )}
    </div>
  )

  const terminalOf = (e: Entry) => () => navigate(`/t/${e.s.id}` + (e.pane ? '?pane=' + encodeURIComponent(e.pane) : ''))
  const openTerminal = terminalOf(cur)
  // Closing one: its herdr pane, asked first (what runs there ends; its
  // conversation stays on the host, to be taken up again in a terminal).
  // Not a tmux one's: its entry is the whole tmux session, other windows too.
  // This one, from the bar's menu; any, from its row's.
  const [closing, setClosing] = useState<Entry | null>(null)
  const [linking, setLinking] = useState<Entry | null>(null)
  const [stoppingFleet, setStoppingFleet] = useState<Entry[] | null>(null)
  const closable = (e: Entry) => e.s.persistence === 'herdr' && !!e.row && !e.row.lost
  const canClose = closable(cur)
  // Claude Code's own commands on its session: only while it is idle.
  // (Of the process it was opened on: another one there meanwhile, it goes.)
  const [op, setOp] = useState<{ op: SessionOp; bind: NonNullable<typeof convo.bind>; key: string } | null>(null)
  useEffect(() => {
    if (op && op.key !== bindKey) setOp(null)
  }, [op, bindKey])
  // (A Codex's: compact and start anew, as Claude Code's.)
  const opsOf = codex ? (['compact', 'rename', 'clear'] as const) : agy ? (['compact', 'clear'] as const) : notClaude ? ([] as const) : (['compact', 'rename', 'clear'] as const)
  const canOps = canModel || canEffort || (bound && opsOf.length > 0 && convo.status === 'ready' && (agent?.status === 'idle' || agent?.status === 'done') && !agent?.other)
  const runOp = (b: NonNullable<typeof convo.bind>, text: string) => {
    return api('POST', `/api/sessions/${sid}/conversation/command`, { pane: b.pane, pid: b.pid, start: b.start, session: b.session, text }, { timeoutMs: PRESS_MS }).then(
      () => {
        toast(t('ops.sent'))
        const m = /^\/rename\s+(.+)$/s.exec(text.trim())
        if (m && b.agent !== 'codex') setTitleNow({ of: `${b.pane} ${b.session}`, title: m[1].trim(), until: Date.now() + 15000 })
        convo.refresh()
      },
      (e) => {
        // (Another agent in the pane: read at once, as for a send — the dialog says why.)
        if (e instanceof ApiError && e.code === 'agent_changed') convo.refresh()
        // (Refused for a confirmed terminal blocker: open the inline terminal.)
        if (e instanceof ApiError) applyLive(e.code, b.agent)
        throw e
      },
    )
  }
  // A copy of its conversation, in a workspace of its own (a herdr one's:
  // where a new agent can be started; Claude Code's or Codex's, its session
  // known). Of the one shown, its session as bound; of another, as herdr
  // tells it. While it is at rest (as the list says it): not a turn in
  // the middle (a check when asked, not a hold on it).
  const [forking, setForking] = useState<{ s: SessionInfo; kind: 'claude' | 'codex'; source: string; at?: string; rewind?: CodexRewind; dir: string; name: string; key: string } | null>(null)
  const [rewinding, setRewinding] = useState<RewindTarget | null>(null)
  // (All of it: asked again as it is sent, of the list now — another
  // window may have set it to work meanwhile — review585-11.)
  const entriesNow = useRef(entries)
  entriesNow.current = entries
  // (A tmux session's agent: its state as its conversation says it, for the one shown.)
  const agentNow = useRef(agent)
  agentNow.current = agent
  const stillAtRest = (key: string) => () => {
    const e = entriesNow.current.find((x) => x.key === key)
    if (e && !e.row && e.s.persistence === 'tmux') {
      const st = key === cur.key ? agentNow.current?.status : undefined
      if (!st) return t('fork.gone')
      return st !== 'idle' && st !== 'done' ? t('fork.onlyIdle') : ''
    }
    const r = e?.row
    // (Not in the list any more, or its state not known: not sent on a guess — review588-7.)
    if (!r || r.lost) return t('fork.gone')
    return r.status !== 'idle' && r.status !== 'done' ? t('fork.onlyIdle') : ''
  }
  // (A Codex's copy up to before the person's message at: how many of the
  // person's messages, from that one on, its transcript is gone back over —
  // as its "←" steps them, one a message sent at rest — and that one's words.)
  const codexRewindOf = (at: string) => {
    const all = convoEntriesNow.current
    const i = all.findIndex((x) => x.id === at && x.role === 'user' && !x.gone)
    if (i < 0) return undefined
    // (Not one taken back, which its transcript no longer has — review fork2-9.)
    const typed = (x: ConvoEntry) => x.role === 'user' && !x.mid && !x.gone
    const users = all.filter(typed)
    const back = all.slice(i).filter(typed).length
    const words = (x: ConvoEntry) => x.parts.map((p) => (p.kind === 'text' ? p.text : '')).join('\n').trim()
    const text = words(all[i])
    // (Another of the person's messages beginning as it does — "continue" twice — is not told
    // from it by what comes back in the copy's input: its rewind not said to be sure.)
    const head = (x: string) => x.split('\n', 1)[0].replace(/\s+/g, '').slice(0, 40)
    // (Or one beginning with it, or it with that one — "continue", "continue please" — review fork4b-3.)
    const mine = head(text)
    // (One of pictures alone begins with nothing: not alike any.)
    const alike = users.filter((x) => x !== all[i] && head(words(x)) !== '' && (head(words(x)).startsWith(mine) || mine.startsWith(head(words(x))))).length > 0
    // (Taken to the page's input only whole: one with a picture, or too long for it, left in the copy's — review fork2-10.)
    const clear = !all[i].parts.some((p) => p.kind === 'image') && text.length <= 4000
    return back > 0 && text ? { back, text: text.slice(0, 4000), alike, clear, at } : undefined
  }
  const convoEntriesNow = useRef(convo.entries)
  convoEntriesNow.current = convo.entries
  const forkOf = (e: Entry, at?: string) => {
    const r = e.row
    const shown = e.key === cur.key && e.s.id === cur.s.id
    if (e.s.persistence === 'tmux' && !r) {
      // (A tmux session's: the one shown, by its conversation's own id; its
      // folder the log's, as the gateway finds it.)
      const b = shown ? convo.bind : null
      const kind = b?.agent === 'claude' || b?.agent === 'codex' ? b.agent : null
      if (!b || !kind || !b.session) return null
      const rewind = at && kind === 'codex' ? codexRewindOf(at) : undefined
      if (at && kind === 'codex' && !rewind) return null
      return { s: e.s, kind, source: b.session, at: rewind ? undefined : at, rewind, dir: '~', name: e.s.mux_session || 'tmux', key: e.key } as const
    }
    if (e.s.persistence !== 'herdr' || !r || r.lost) return null
    const prog = (shown && convo.bind?.agent) || (isCodex(r.a.agent) ? 'codex' : isOpenCode(r.a.agent) ? 'opencode' : isAgy(r.a.agent) ? 'agy' : /claude/i.test(r.a.agent) ? 'claude' : '')
    const source = (shown && convo.bind?.session) || r.a.session || ''
    if ((prog !== 'claude' && prog !== 'codex') || !source) return null
    // (A Codex's, up to a turn: its whole copy rewound in its own terminal — only the one shown, by its conversation.)
    const rewind = at && prog === 'codex' && shown ? codexRewindOf(at) : undefined
    if (at && prog === 'codex' && !rewind) return null
    const name = (r.ws && r.ws !== '~' ? r.ws : '') || dirName(r.a.cwd) || r.a.agent
    return { s: e.s, kind: prog, source, at: rewind ? undefined : at, rewind, dir: r.a.cwd, name, key: e.key } as const
  }
  const forkItem = (e: Entry): MenuItem | null => {
    const f = forkOf(e)
    if (!f) return null
    const st = e.row ? e.row.status : agent?.status
    const still = st === 'idle' || st === 'done'
    return { label: t('fork.item'), icon: <GitBranch size={16} />, separator: true, disabled: !still, hint: still ? undefined : t('fork.onlyIdle'), onSelect: () => setForking(f) }
  }
  // Of it up to a turn's end (Claude Code's, from the thread): any turn;
  // the last, as all of it, while it is at rest.
  const forkHere = (at: string | undefined) => {
    const f = forkOf(cur, at)
    // (A Codex's, before a message of pictures alone: no words of it to see come back — review fork4b-6.)
    if (!f) return void (at && convo.bind?.agent === 'codex' && toast(t('fork.atNoText'), true))
    const st = cur.row ? cur.row.status : agent?.status
    if (!at && st !== 'idle' && st !== 'done') return toast(t('fork.onlyIdle'), true)
    setForking(f)
  }
  // Its names in herdr: the workspace's, its tab's, its pane's own.
  const [renaming, setRenaming] = useState<RenameTarget | null>(null)
  const renameItem = (e: Entry): MenuItem | null => {
    const r = e.row
    if (e.s.persistence !== 'herdr' || !r || r.lost || !r.wsId) return null
    return {
      label: t('rename.item'),
      icon: <TextCursorInput size={16} />,
      onSelect: () => setRenaming({ sessionId: e.s.id, ws: { id: r.wsId, label: r.ws }, tab: r.a.tab ? { id: r.a.tab, label: r.a.tab_label ?? '' } : undefined, pane: { id: r.a.pane_id, label: r.a.label ?? '' } }),
    }
  }
  const copySessionItem = (e: Entry): MenuItem | null => {
    const shown = e.key === cur.key && e.s.id === cur.s.id
    const bound = shown && convo.bind && (!e.pane || convo.bind.pane === e.pane) ? convo.bind.session : ''
    const sess = bound && bound !== NO_SESSION ? bound : (e.row?.a.session || '')
    if (!sess || sess === NO_SESSION) return null
    return {
      label: t('convo.copySession'),
      icon: <Copy size={16} />,
      onSelect: async () => {
        const ok = await copyText(sess).catch(() => false)
        toast(ok ? t('convo.sessionCopied', { id: sess.slice(0, 8) }) : t('convo.sessionCopyFailed'), !ok)
      },
    }
  }
  // Pinning one, or letting it go.
  const pinItem = (key: string): MenuItem => {
    const pinnedNow = pins.includes(key)
    return {
      label: t(pinnedNow ? 'agents.unpin' : 'agents.pin'),
      icon: pinnedNow ? <PinOff size={16} /> : <Pin size={16} />,
      onSelect: () => {
        if (pinnedNow) return setPrefs({ pinnedAgents: pins.filter((k) => k !== key) })
        // Full only of those on the list: pins of agents no longer here (a
        // pane closed, a host away — alike from here) kept while there is
        // room, and let go, the oldest first, to make room for one pinned
        // now. 11-14: they were counted too, and 14 gone ones refused a pin.
        const here = new Set(entries.map((e) => e.key))
        const shown = pins.filter((k) => here.has(k))
        if (shown.length >= maxPinned) return toast(t('agents.pinFull', { n: maxPinned }), true)
        setPrefs({ pinnedAgents: makeRoom(pins, here, maxPinned - 1).concat(key) })
      },
    }
  }
  const prefsNow = usePrefs()
  const pinReorderItems = (key: string): (MenuItem | null)[] => {
    const here = new Set(entries.map((e) => e.key))
    const visible = pins.filter((k) => here.has(k))
    const pos = visible.indexOf(key)
    if (pos < 0 || visible.length < 2) return []
    const move = (dir: 'top' | 'up' | 'down') => () => {
      const next = reorderPinned(pins, key, here, dir)
      if (!next) return
      const autoFixed = !prefsNow.pinFixedOrder
      setPrefs({ pinnedAgents: next, pinFixedOrder: true })
      if (autoFixed) toast(t('agents.pinFixedAuto'))
    }
    return [
      {
        label: t('agents.pinOrder'),
        icon: <ArrowUpDown size={16} />,
        actions: [
          { label: t('agents.pinTop'), icon: <ArrowUpToLine size={15} />, disabled: pos === 0, onSelect: move('top') },
          { label: t('agents.pinUp'), icon: <ArrowUp size={15} />, disabled: pos === 0, onSelect: move('up') },
          { label: t('agents.pinDown'), icon: <ArrowDown size={15} />, disabled: pos === visible.length - 1, onSelect: move('down') },
        ],
      },
    ]
  }
  const unlinkItem = (e: Entry): MenuItem | null => {
    const r = e.row
    if (e.s.persistence !== 'herdr' || !r || r.lost || !r.a.parent_pane) return null
    return {
      label: t('convo.lineage.unlink'),
      icon: <Unlink size={16} />,
      onSelect: () => {
        void api('POST', `/api/sessions/${encodeURIComponent(e.s.id)}/herdr/lineage`, {
          action: 'unlink',
          child_pane: r.a.pane_id,
          child_term_id: r.a.terminal_id ?? '',
          child_session: r.a.session ?? '',
        }).then(
          () => {
            toast(t('convo.lineage.unlinked'))
            readSessionsNow()
          },
          (err) => toast(errorText(err), true),
        )
      },
    }
  }
  // Linking it under another by hand (one Corral did not see started by another: doc item 47). Those it may go
  // under: the live agents of the same herdr session, but itself and those under it (a cycle; the gateway refuses
  // one too), those of its workspace first.
  const linkPaneOf = (e: Entry): LinkPane | null => {
    const r = e.row
    if (!r) return null
    return {
      key: e.key,
      title: rows.find((x) => x.key === e.key)?.title || r.a.agent,
      where: whereOf(e, 1),
      pane: r.a.pane_id,
      term: r.a.terminal_id ?? '',
      session: r.a.session ?? '',
    }
  }
  const linkParentsOf = (e: Entry): LinkPane[] => {
    const r = e.row
    if (!r) return []
    // (Of the same herdr: one host's, one herdr session's — the list's own grouping, agentKey; one agent may be
    // listed under another of the gateway's connections to it than this one. A host record edited to another
    // machine while a connection to the old one is open is two servers alike here, as in the list itself; one
    // picked from the other is refused as gone — the gateway looks for it on this connection's server.)
    const herdrOf = (s: SessionInfo) => `${s.host_id}|${!s.mux_session || s.mux_session === 'default' ? '' : s.mux_session}`
    const same = entries.filter((x) => herdrOf(x.s) === herdrOf(e.s) && x.row && !x.row.lost)
    const byPane = new Map(same.map((x) => [x.row!.a.pane_id, x]))
    const under = (x: Entry) => {
      const seen = new Set<string>()
      for (let p = x.row?.a.parent_pane; p && !seen.has(p); p = byPane.get(p)?.row?.a.parent_pane) {
        if (p === r.a.pane_id) return true
        seen.add(p)
      }
      return false
    }
    return same
      .filter((x) => x.key !== e.key && !under(x))
      .sort((a, b) => Number(b.row!.ws === r.ws) - Number(a.row!.ws === r.ws))
      .map(linkPaneOf)
      .filter((x): x is LinkPane => !!x)
  }
  const linkItem = (e: Entry): MenuItem | null => {
    const r = e.row
    if (e.s.persistence !== 'herdr' || !r || r.lost || r.a.parent_pane || linkParentsOf(e).length === 0) return null
    return { label: t('convo.lineage.link'), icon: <Link2 size={16} />, onSelect: () => setLinking(e) }
  }
  const subagentParentOf = (e: Entry): NewSubagentParent | null => {
    const r = e.row
    if (e.s.persistence !== 'herdr' || !r || r.lost) return null
    const match = rows.find((x) => x.key === e.key)
    return {
      sessionId: e.s.id,
      pane: r.a.pane_id,
      title: match?.title || (r.ws && r.ws !== '~' ? r.ws : '') || dirName(r.a.cwd) || r.a.agent,
      dir: r.a.cwd || '~/',
      ws: r.ws,
    }
  }
  const newSubagentItem = (e: Entry): MenuItem | null => {
    if (!onNewSubagent) return null
    const p = subagentParentOf(e)
    if (!p) return null
    return { label: t('newAgent.subagentItem'), icon: <GitFork size={16} />, onSelect: () => onNewSubagent(p) }
  }
  const splitRowItem = (e: Entry): MenuItem | null => {
    if (!onSplitOpen || e.key === cur.key) return null
    if (splitKey === e.key && onSplitClose) {
      return { label: t('convo.split.close'), icon: <Columns2 size={16} />, onSelect: onSplitClose }
    }
    return { label: t('convo.split.open'), icon: <Columns2 size={16} />, onSelect: () => onSplitOpen(e) }
  }
  // (Claude Code's own commands on it are not there: they are of the one
  // shown, its process known.)
  rowMenu.current = (e) => [
    onOpenTab && e.key !== cur.key ? { label: t('agentTabs.newTab'), icon: <Plus size={16} />, onSelect: () => onOpenTab(e) } : null,
    { label: t('convo.showTerminal'), icon: <SquareTerminal size={16} />, onSelect: terminalOf(e) },
    splitRowItem(e),
    pinItem(e.key),
    ...pinReorderItems(e.key),
    forkItem(e),
    newSubagentItem(e),
    renameItem(e),
    copySessionItem(e),
    linkItem(e),
    unlinkItem(e),
    closable(e) ? { label: t('agents.close'), icon: <Power size={16} />, danger: true, separator: true, onSelect: () => setClosing(e) } : null,
  ]
  const { config: translateCfg } = useTranslateConfig()
  // (Thinking translation off, or on as a literal translation or as the original with a plain summary: one row of
  // the menu, the three as a segmented control — it was two items. Doc item 38, 11-24.)
  const setTranslate = (want: 'off' | 'translate' | 'summarize') => {
    void saveTranslateConfig(want === 'off' ? { enabled: false } : { enabled: true, mode: want }).then(
      (saved) => {
        if (!saved.enabled) toast(t('convo.translate.disabled'))
        else if (!saved.has_key && !/^https?:\/\/(localhost|127\.0\.0\.1|\[::1\])(?::\d+)?\b/i.test(saved.base_url)) toast(t('convo.translate.needKey'))
        else toast(t(saved.mode === 'summarize' ? 'convo.translate.modeSummarizeOn' : 'convo.translate.modeTranslateOn'))
      },
      (e) => toast(errorText(e), true),
    )
  }
  const splitEffectiveDir = (): 'row' | 'col' => {
    if (splitDir === 'row' || splitDir === 'col') return splitDir
    return window.innerWidth < 820 || window.innerHeight > window.innerWidth * 1.05 ? 'col' : 'row'
  }
  const curLineageRow = rows.find((r) => r.current)
  const parentLineageRow = curLineageRow?.parentKey ? rows.find((r) => r.key === curLineageRow.parentKey) : undefined
  const parentLineageEntry = parentLineageRow ? entries.find((e) => e.key === parentLineageRow.key) : undefined
  const childLineageRows = curLineageRow ? rows.filter((r) => r.parentKey === curLineageRow.key) : []
  const workingChildEntries = childLineageRows
    .filter((c) => (c.liveStatus ?? c.status) === 'working')
    .map((c) => entries.find((e) => e.key === c.key))
    .filter((e): e is Entry => !!e && e.s.persistence === 'herdr' && !!e.row && !e.row.lost)
  const otherSplitKey = isSecondary ? primaryKey : splitKey
  const otherSplitEntry = otherSplitKey ? entries.find((e) => e.key === otherSplitKey) : undefined
  const canCliZoom = cur.s.persistence === 'herdr' && !!cur.row && !cur.row.lost
  const canCliSwap =
    canCliZoom &&
    otherSplitEntry?.s.persistence === 'herdr' &&
    !!otherSplitEntry.row &&
    !otherSplitEntry.row.lost &&
    otherSplitEntry.s.id === cur.s.id &&
    !!cur.row!.a.tab &&
    otherSplitEntry.row.a.tab === cur.row!.a.tab &&
    otherSplitEntry.row.a.pane_id !== cur.row!.a.pane_id
  const more = (): (MenuItem | null)[] => [
    ...(isSecondary
      ? [
          onSplitDirToggle
            ? {
                label: t(splitEffectiveDir() === 'col' ? 'convo.split.toRow' : 'convo.split.toCol'),
                icon: splitEffectiveDir() === 'col' ? <Columns2 size={16} /> : <Rows2 size={16} />,
                onSelect: () => onSplitDirToggle(splitEffectiveDir()),
              }
            : null,
          onSplitSwap
            ? {
                label: t('convo.split.swap'),
                icon: splitEffectiveDir() === 'col' ? <ArrowUpDown size={16} /> : <ArrowLeftRight size={16} />,
                onSelect: onSplitSwap,
              }
            : null,
          onSplitExpand ? { label: t('convo.split.expand'), icon: <Maximize2 size={16} />, onSelect: onSplitExpand } : null,
        ]
      : []),
    { label: t('convo.showTerminal'), icon: <SquareTerminal size={16} />, group: t('menu.group.agent'), separator: isSecondary && (!!onSplitSwap || !!onSplitExpand), onSelect: openTerminal },
    // (Toggle zoom in the CLI: in the session card's, not here as well — doc item 38.)
    canCliSwap
      ? {
          label: t('convo.cliSwap'),
          icon: <ArrowLeftRight size={16} />,
          onSelect: () => {
            void api('POST', `/api/sessions/${encodeURIComponent(cur.s.id)}/herdr`, {
              action: 'swap_panes',
              target: cur.row!.a.pane_id,
              other: otherSplitEntry!.row!.a.pane_id,
            }).then(
              () => {
                onSplitSwap?.()
                toast(t('convo.cliSwapped'))
              },
              (e) => toast(errorText(e), true),
            )
          },
        }
      : null,
    pinItem(cur.key),
    // (Within its group here: no rule of its own, as it has in a row's menu.)
    ((f) => f && { ...f, separator: false })(forkItem(cur)),
    newSubagentItem(cur),
    renameItem(cur),
    copySessionItem(cur),
    linkItem(cur),
    unlinkItem(cur),
    ((on, sum) => ({
      label: t('convo.translate.menu'),
      icon: <Languages size={16} />,
      actions: [
        { label: t('convo.translate.offLong'), text: t('convo.translate.offShort'), on: !on, onSelect: () => on && setTranslate('off') },
        { label: t('convo.translate.literalLong'), text: t('convo.translate.literalShort'), on: on && !sum, onSelect: () => (!on || sum) && setTranslate('translate') },
        { label: t('convo.translate.summaryLong'), text: t('convo.translate.summaryShort'), on: on && sum, onSelect: () => (!on || !sum) && setTranslate('summarize') },
      ],
    }))(!!translateCfg?.enabled, translateCfg?.mode === 'summarize'),
    boot.forwarding ? { label: t('ports.title'), icon: <Globe size={16} />, separator: true, onSelect: () => setPortsOpen(true) } : null,
    boot.code ? { label: t('code.open'), icon: <Code2 size={16} />, separator: !boot.forwarding, onSelect: () => code.open(cur.s.host_id, cur.s.host_name) } : null,
    boot.code && code.running[cur.s.host_id] ? { label: t('code.stop'), icon: <CircleStop size={16} />, onSelect: () => code.stop(cur.s.host_id) } : null,
    { label: t('share.title'), icon: <Eye size={16} />, separator: !boot.forwarding && !boot.code, onSelect: () => setShareOpen(true) },
    ...opsOf.map((o, i): MenuItem | null =>
      bound ? { label: t(codex && o === 'clear' ? 'ops.clear.itemCodex' : `ops.${o}.item`), icon: OP_ICONS[o], separator: i === 0, group: i === 0 ? t('menu.group.convo') : undefined, disabled: !canOps, title: canOps ? undefined : t('ops.onlyIdle'), hint: canOps || i < opsOf.length - 1 ? undefined : t(opsOf.length > 1 ? 'ops.onlyIdleGroup' : 'ops.onlyIdle'), onSelect: () => convo.bind && setOp({ op: o, bind: convo.bind, key: bindKey }) } : null,
    ),
    // (A Codex's commands that only show something in its terminal: run, then its terminal shown here.)
    // (Codex's commands that only show something in its terminal: one row, each its command — they were four.)
    codex && bound
      ? {
          label: t('ops.codexLooks'),
          icon: <SquareTerminal size={16} />,
          separator: true,
          actions: CODEX_LOOKS.map(([cmd, key]) => ({
            label: t(key) + (canOps ? '' : ' · ' + t('ops.onlyIdle')),
            text: cmd,
            disabled: !canOps,
            onSelect: () => {
              const b = convo.bind
              if (!b) return
              runOp(b, cmd).then(
                () => setLiveOf(bindKey),
                (e) => toastRefusal(e),
              )
            },
          })),
        }
      : null,
    workingChildEntries.length >= 2
      ? {
          label: t('convo.fleet.stopAll', { n: workingChildEntries.length }),
          icon: <CircleStop size={16} />,
          danger: true,
          separator: true,
          onSelect: () => setStoppingFleet(workingChildEntries),
        }
      : null,
    canClose ? { label: t('agents.close'), icon: <Power size={16} />, danger: true, separator: workingChildEntries.length < 2, onSelect: () => setClosing(cur) } : null,
  ]
  const curSubagentParent = subagentParentOf(cur)
  const lineageBar: ConvoLineageBar | undefined =
    parentLineageRow || childLineageRows.length
      ? {
          parent: parentLineageRow
            ? {
                key: parentLineageRow.key,
                title: parentLineageRow.title,
                status: parentLineageRow.liveStatus ?? parentLineageRow.status,
                onPick: parentLineageRow.onPick,
              }
            : undefined,
          children: childLineageRows.map((c) => ({
            key: c.key,
            title: c.title,
            where: c.where,
            status: c.liveStatus ?? c.status,
            onPick: c.onPick,
          })),
          splitKey: isSecondary ? primaryKey : (splitKey ?? undefined),
          canSplit: !isSecondary && !!onSplitOpen,
          onSplitChild:
            !isSecondary && onSplitOpen
              ? (key) => {
                  const target = entries.find((e) => e.key === key)
                  if (target) onSplitOpen(target)
                }
              : undefined,
          onSplitWithParent:
            !isSecondary && parentLineageEntry && onSplitParentChild
              ? () => onSplitParentChild(parentLineageEntry, cur)
              : undefined,
          onCloseSplit: !isSecondary ? onSplitClose : undefined,
          onNewSubagent: curSubagentParent && onNewSubagent ? () => onNewSubagent(curSubagentParent) : undefined,
        }
      : undefined
  const effDir = splitEffectiveDir()
  const barExtra =
    isSecondary && onSplitClose ? (
      <>
        {onSplitDirToggle && (
          <button
            type="button"
            className="icon-btn convo-bar-split-act"
            onClick={() => onSplitDirToggle(effDir)}
            aria-label={t(effDir === 'col' ? 'convo.split.toRow' : 'convo.split.toCol')}
            title={t(effDir === 'col' ? 'convo.split.toRow' : 'convo.split.toCol')}
          >
            {effDir === 'col' ? <Columns2 size={15} aria-hidden="true" /> : <Rows2 size={15} aria-hidden="true" />}
          </button>
        )}
        <button type="button" className="icon-btn convo-bar-split-act" onClick={onSplitClose} aria-label={t('convo.split.close')} title={t('convo.split.close')}>
          <X size={16} aria-hidden="true" />
        </button>
      </>
    ) : undefined
  // (Files dropped on the conversation go up as picked ones do.)
  const dropping = (e: React.DragEvent) => e.dataTransfer.types.includes('Files')
  return (
    <div
      className="agents-chat-drop"
      onDragOver={(e) => {
        if (!dropping(e)) return
        e.preventDefault()
        setDragOver(true)
      }}
      onDragLeave={(e) => {
        if (!e.currentTarget.contains(e.relatedTarget as Node | null)) setDragOver(false)
      }}
      onDrop={(e) => {
        if (!dropping(e)) return
        e.preventDefault()
        setDragOver(false)
        const files = [...e.dataTransfer.files]
        if (files.length) void upload(files)
      }}
    >
    {dragOver && <div className="term-drop">{t('term.dropHint')}</div>}
    <ConversationView
      onHostHistory={() => setHostHistory(true)}
      convo={convo}
      agent={agent}
      taskAsked={taskAsked}
      compacting={compacting}
      goal={goalNote?.bind === bindKey ? goalNote.g : undefined}
      retrying={retrying}
      reviewing={reviewing}
      pending={panelOpen ? shownSent.map((p) => (p.state === 'waiting' && isCommand(p.text) ? { ...p, opened: true } : p)) : shownSent}
      ask={
        !readOnly && waits && replyTo ? (
          // (One a pane, and a process in it: nothing of another's carries over. Not tied to a terminal: no size to wait for.)
          <InlineReply key={replyTo.key} target={replyTo} resizedAt={0} active={kActive} held={() => false} call={asked} pending={pendingCall} agentVersion={convo.meta?.version} onAnswered={(yes) => {
            setAnswered(yes)
            // (Answered: not waiting in the list and the title either, at once — not at its next read. 11-14.)
            const pane = cur.pane
            if (yes && pane) expectSession(cur.s.id, (s) => answeredHerdr(s, pane), (s) => !paneWaits(s, pane), 4000)
          }} waitSince={cur.row && !cur.row.lost && cur.row.a.since ? cur.row.a.since : undefined} />
        ) : undefined
      }
      // (The bar's stop only where the box's one is not there: words or files
      // in the box make it send, or a question's card stands in its place —
      // 11-04. A phone: the composer's button stops it, the bar keeps the room
      // for the name.)
      onStop={canPress && !lead && (!!draft.trim() || !!up || askHides) ? () => press('stop') : undefined}
      stop={said?.what === 'stop' && said.bind === bindKey ? said.text : undefined}
      side={isSecondary ? undefined : { rows }}
      active={active}
      keysActive={kActive}
      splitPane={isSecondary ? undefined : splitPane}
      barExtra={barExtra}
      lineageBar={lineageBar}
      onDismiss={(id) => setSent((ps) => ps.filter((p) => p.id !== id))}
      onResend={(id) => {
        const p = sent.find((q) => q.id === id)
        if (!p) return
        setSent((ps) => ps.filter((q) => q.id !== id))
        // (After what is in the box already, if anything: the person sends it when ready.)
        const next = draft.trim() ? draft.replace(/\s+$/, '') + '\n' + p.text : p.text
        setDraft(next)
        requestAnimationFrame(() => {
          const el = field.current
          if (!el) return
          el.focus()
          el.selectionStart = el.selectionEnd = next.length
        })
      }}
      onEnter={async (id) => {
        const p = sent.find((q) => q.id === id)
        if (!p) return false
        const enter = (text: string) => api<{ pressed: boolean }>('POST', `/api/sessions/${sid}/conversation/sent-enter`, { ...pressBody(), text }, { timeoutMs: PRESS_MS })
        // (Several queued, their turn stopped: Codex puts them all back in its
        // input, one after another, one message now — Enter sends them as one.
        // Asked for as that, the box holding all of them and nothing else; else
        // this one alone, as the box may hold it, the others taken out there.)
        const group = p.unqueued ? sent.filter((q) => q.unqueued && q.state === 'stuck' && q.bind === p.bind).sort((a, b) => a.id - b.id) : []
        const all = group.length > 1 ? group.map((q) => q.text).join('\n') : ''
        let r = all ? await enter(all) : await enter(p.text)
        const joined = !!all && r.pressed
        if (all && !r.pressed) r = await enter(p.text)
        if (r.pressed) {
          // (Waited for in the log again, as when it was sent: all of them, as the one they now are.)
          lookedAt.delete(id * 10 + (p.relook ?? 0))
          const others = new Set(joined ? group.filter((q) => q.id !== id).map((q) => q.id) : [])
          const shown = joined ? group.map((q) => q.shown ?? q.text).join('\n') : undefined
          setSent((ps) =>
            ps.flatMap((q) => (others.has(q.id) ? [] : q.id === id ? [{ ...q, ...(joined ? { text: all, shown } : {}), state: 'waiting' as const, look: undefined, looked: false, quiet: false, at: Date.now() }] : [q])),
          )
          // (Read now and a little later, as a send is: an answer quick enough
          // may come and go between two of herdr's words, none to wake a read.)
          convo.refresh()
          setTimeout(() => convo.refresh(), 1500)
          setTimeout(() => convo.refresh(), 4000)
        }
        return r.pressed
      }}
      onShowTerminal={openTerminal}
      dockSink={dockSink}
      dockTerminal={{ can: liveFor(convo.bind?.agent) && !!convo.bind, press: pressBody, onFull: openTerminal, letters: convo.bind?.agent === 'codex' }}
      inlinePanel={panelOpen || (!waits && liveOf === bindKey)}
      // (herdr's only: its pane's terminal is had live, tmux's not — review of c2ae00c.)
      viewOf={cur.s.persistence === 'herdr' ? cur.key : undefined}
      onLook={liveFor(convo.bind?.agent) && convo.bind ? () => setLiveOf(bindKey) : undefined}
      onCopied={() => toast(t('common.copied'))}
      composer={composer}
      onSendNow={canPress && (!notClaude || agy) ? () => press('now') : undefined}
      sendNow={said?.what === 'now' && said.bind === bindKey ? said.text : undefined}
      mode={{ now: modeNow?.bind === bindKey ? modeNow.mode : undefined, busy: modeBusy, onPick: canMode ? pickMode : undefined, codex, agy, opencode }}
      model={canModel || modelBusy ? { list: models?.bind === bindKey ? models.list : null, now: modelNow?.bind === bindKey ? modelNow.model : undefined, unread: models?.bind === bindKey && !!models.unread, error: models?.bind === bindKey ? models.error : undefined, byLog: models?.bind === bindKey && !!models.byLog, fresh: !!models?.at && Date.now() - models.at < 60000, busy: modelBusy, can: canModel, onOpen: () => openModels(false), onRetry: () => openModels(true), onPick: pickModel, of: bindKey, ...(codex ? { efforts: CODEX_EFFORTS } : {}) } : undefined}
      effort={{ now: effortNow?.bind === bindKey ? effortNow.effort : undefined, busy: effortBusy, can: canEffort, onPick: pickEffort }}
      lead={lead}
      more={more}
      query={query}
      onQuery={onQuery}
      answered={answered}
      files={fileActs}
      // (The session card's request of the agent: in the composer to be read and sent — after a draft there, never over it.)
      onDraft={(text) => {
        const d = field.current ? field.current.value : draft
        setDraft(d.trim() ? d.replace(/\s*$/, '\n\n') + text : text)
        requestAnimationFrame(() => {
          const f = field.current
          if (!f) return
          f.focus()
          f.setSelectionRange(f.value.length, f.value.length)
        })
      }}
      onForkAt={forkOf(cur) ? forkHere : undefined}
      forkWhole={cur.row ? cur.row.status === 'idle' || cur.row.status === 'done' : cur.s.persistence === 'tmux' && (agent?.status === 'idle' || agent?.status === 'done')}
      onRewindAt={
        convo.bind?.agent === 'claude' && convo.bind.session
          ? (entry, text) => {
              const b = convo.bind
              if (!b || b.agent !== 'claude' || !b.session) return
              setRewinding({
                sessionId: sid,
                pane: b.pane,
                terminalId: b.terminal_id || cur.row?.a.terminal_id || '',
                pid: b.pid,
                start: b.start,
                session: b.session,
                messageId: entry.id,
                text,
                files: rewindEditedFiles(convo.entries, entry.id),
              })
            }
          : undefined
      }
      rewindIdle={isRewindIdle(agent, cur.row?.status, cur.s.persistence)}
    />
    {viewing && active && (
      <PictureDialog
        sid={sid}
        path={viewing}
        group={viewSet && viewSet.includes(viewing) ? viewSet : undefined}
        onGo={setViewing}
        onClose={() => setViewing(null)}
        onReveal={() => {
          setViewing(null)
          setFilesAt(viewing)
        }}
      />
    )}
    {reading && active && (
      <TextPreview
        sid={sid}
        path={reading}
        tail={readingEnd}
        lines={readingAt}
        onClose={() => setReading(null)}
        onReveal={() => {
          setReading(null)
          setFilesAt(reading)
        }}
      />
    )}
    {filesAt && active && (
      <FilesDialog
        sessionId={sid}
        start={filesIn ? filesAt : null}
        reveal={filesIn ? undefined : filesAt}
        onPath={() => {}}
        onInsert={(p) => {
          setFilesAt(null)
          insertPath(p)
        }}
        onClose={() => setFilesAt(null)}
      />
    )}
    {forking && (
      <ForkDialog
        sessionId={forking.s.id}
        hostId={forking.s.host_id}
        kind={forking.kind}
        source={forking.source}
        at={forking.at}
        rewind={forking.rewind}
        rewindNow={forking.rewind ? () => (convo.bind?.session === forking.source ? codexRewindOf(forking.rewind!.at) : undefined) : undefined}
        dir={forking.dir}
        name={forking.name}
        check={forking.at ? undefined : stillAtRest(forking.key)}
        onDone={(pane, first, opened, rewound, named, rw) => {
          const f = forking
          setForking(null)
          onStarted(opened ?? f.s, pane, first)
          // (A Codex's copy is told by its name: one not given it may not be shown here.)
          if (f.kind === 'codex' && named === false && rewound !== false) toast(t('fork.notNamed'), true)
          // (As counted when it was sent, not when the dialog opened — review fork5-3.)
          const sent = rw ?? f.rewind
          if (sent) {
            if (rewound === false) toast(t('fork.notRewound'), true)
            else if (sent.alike) toast(t('fork.rewoundAlike'), true)
            else if (!sent.clear) toast(t('fork.leftInTerminal'))
          }
        }}
        onClose={() => setForking(null)}
      />
    )}
    {rewinding && (
      <RewindDialog
        target={rewinding}
        onRefresh={() => convo.refresh()}
        onDone={(text, warning, restoredFiles) => {
          setRewinding(null)
          setDraft(text)
          requestAnimationFrame(() => {
            const f = field.current
            if (!f) return
            f.focus()
            f.setSelectionRange(f.value.length, f.value.length)
          })
          convo.refresh()
          if (warning === 'input_not_cleared') toast(t('rewind.err.input_not_cleared'), true)
          else if (restoredFiles && restoredFiles > 0) toast(t(restoredFiles === 1 ? 'rewind.doneCode1' : 'rewind.doneCode', { n: restoredFiles }))
        }}
        onClose={() => setRewinding(null)}
      />
    )}
    {renaming && <RenameDialog target={renaming} onClose={() => setRenaming(null)} />}
    {linking &&
      ((child) =>
        child && (
          <LinkParentDialog
            sessionId={linking.s.id}
            child={child}
            parents={linkParentsOf(linking)}
            onClose={() => setLinking(null)}
            onLinked={(p) => {
              toast(t('convo.lineage.linked', { name: p.title }))
              readSessionsNow()
            }}
          />
        ))(linkPaneOf(linking))}
    {portsOpen && active && <PortsDialog hostId={cur.s.host_id} onClose={() => setPortsOpen(false)} />}
    {shareOpen && active && <ShareDialog sessionId={sid} onClose={() => setShareOpen(false)} />}
    {code.dialog}
    {op && <SessionOpDialog op={op.op} name={convoTitle} codex={op.bind.agent === 'codex'} onSend={(text) => runOp(op.bind, text)} onClose={() => setOp(null)} onShowTerminal={openTerminal} />}
    {closing?.row && (
      <Confirm
        title={closing.key === cur.key ? t('agents.closeTitle') : t('agents.closeTitleNamed', { name: rows.find((r) => r.key === closing.key)?.title ?? '' })}
        body={t((closing.key === cur.key ? working : (entries.find((e) => e.key === closing.key)?.row ?? closing.row).status === 'working') ? 'agents.closeBodyWorking' : 'agents.closeBody')}
        confirm={t('agents.closeConfirm')}
        danger
        onConfirm={async () => {
          try {
            await api('POST', `/api/sessions/${closing.s.id}/herdr`, { action: 'close_pane', target: closing.row!.a.pane_id })
          } catch (e) {
            throw { human: e instanceof ApiError && e.code === 'herdr_busy' ? t('herdr.busy') : t('sc.herdrFailed') }
          }
          onClosed(closing.key)
        }}
        onClose={() => setClosing(null)}
      />
    )}
    {stoppingFleet && stoppingFleet.length > 0 && (
      <Confirm
        title={t('convo.fleet.stopAllTitle')}
        body={t('convo.fleet.stopAllConfirm', { n: stoppingFleet.length })}
        confirm={t('convo.stop')}
        danger
        onConfirm={async () => {
          const targets = stoppingFleet
          const results = await Promise.allSettled(
            targets.map((e) =>
              api('POST', `/api/sessions/${encodeURIComponent(e.s.id)}/herdr`, {
                action: 'interrupt_pane',
                target: e.row!.a.pane_id,
              }),
            ),
          )
          const ok = results.filter((r) => r.status === 'fulfilled').length
          toast(t('convo.fleet.stopped', { ok, total: targets.length }), ok === 0)
          readSessionsNow()
        }}
        onClose={() => setStoppingFleet(null)}
      />
    )}
    </div>
  )
}

// An element's inner width, as it changes (0 till it is laid out).
function useWidth(el: HTMLElement | null): number {
  const [w, setW] = useState(0)
  useLayoutEffect(() => {
    if (!el) return
    const look = () => setW(el.clientWidth)
    look()
    const ro = new ResizeObserver(look)
    ro.observe(el)
    return () => ro.disconnect()
  }, [el])
  return w
}

// (Wide, a mouse and keyboard: the composer's history and commands are their keys, ↑ and /, said in its placeholder.)
const KEYED_COMPOSER = '(min-width: 760px) and (hover: hover) and (pointer: fine)'

// A phone's list of the agents: those for you first (waiting, then done and
// not looked at, as the side list's first group), then working, then the
// rest, the latest to change first in each, as it was when the list showed
// (statuses change in place; rows do not move under the finger; one new comes
// last). Its summary line puts them in order again and goes to the first
// waiting; "only mine" keeps those waiting, and those done since. In the
// order the side list has them (listOrder, of these rows: see asRow), with
// no group headings — each row says its state — and the old ones folded
// past a few of each of their groups, as there: a line where the rest of
// the group would be, opened and shut by the group, as there.
let listScroll = 0
// query, onQuery: what it is searched for, kept by the page (as the side
// list's is): still searched, back from a conversation.
function PhoneList({ entries, all, titles, query, onQuery, onPick }: { entries: Entry[]; all: SessionInfo[]; titles: Titles; query: string; onQuery: (q: string) => void; onPick: (e: Entry) => void }) {
  // A phone's list of the agents: the side list's own (AgentList), the
  // whole screen — its groups, its rows, its search — as the switcher in a
  // conversation shows it. 11-13: before, a list of its own (a line under
  // every row, the state said twice beside each, a coloured count over
  // them, no groups) that the person found ugly beside the side list.
  // Over it, the way to a new agent and "waiting on me" only.
  const [mine, setMineState] = useState(() => local.get(ONLY_MINE) === '1')
  const setMine = (v: boolean) => {
    setMineState(v)
    try {
      local.set(ONLY_MINE, v ? '1' : '0')
    } catch {
      /* not kept */
    }
  }
  const pins = usePrefs().pinnedAgents
  // (Its rows as the side list's are made — see AgentChat — none of them the one shown.)
  const rows = useMemo((): ConvoListRow[] => {
    const hosts = hostCount(entries)
    const wsByKey = new Map<string, string>()
    for (const e of entries) if (e.row) wsByKey.set(e.key, e.row.wsId)
    const pinAt = (key: string) => (pins.includes(key) ? pins.indexOf(key) : undefined)
    return entries.map((e): ConvoListRow => {
      if (e.row) {
        const r = e.row
        const named = ((isCodex(r.a.agent) || isAgy(r.a.agent)) && r.a.thread_title) || keptTitle(titles, e) || paneName(r.a)
        const said = named || dirName(r.a.cwd) || r.a.agent
        const parentKey = r.lost || !r.a.parent_pane ? undefined : agentKey(e.s, r.a.parent_pane)
        const sameWsParent = !!parentKey && wsByKey.get(parentKey) === r.wsId
        return { key: e.key, ...labelOf(e, said, hosts, !!named, sameWsParent), now: r.lost || r.bg || r.status !== 'blocked' ? undefined : glanceOf(e.s.id, e.pane, r.status, r.a.since) || undefined, program: r.a.agent, status: r.status, liveStatus: r.status, lost: r.lost, lastStatus: r.lost && r.last && r.last !== 'unknown' ? r.last : undefined, lostAt: r.lost ? r.lostAt : undefined, bg: r.lost ? undefined : r.bg, failed: failedOf(r, r.status), model: r.lost ? undefined : r.a.model, glance: r.lost || r.bg ? undefined : () => glanceOf(e.s.id, e.pane, r.status, r.a.since), cmds: cmdsOf(r), tasks: tasksOf(r), since: r.lost ? r.a.since : r.bg ? r.a.bg_since : r.a.since, current: false, pinned: pinAt(e.key), projectKey: normalizeProjectPath(r.a.cwd), sessionGroup: sessionGroupOf(e, all), parentKey, lineageSource: r.lost ? undefined : r.a.lineage_source, onPick: () => onPick(e) }
      }
      const title = keptTitle(titles, e) || `${e.s.host_name} · ${sessionWhich(e.s, all)}`
      const tmuxBg = tmuxBgOf(e)
      return { key: e.key, title, where: whereOf(e, hosts), status: 'unknown', cmds: tmuxBg.cmds, tasks: rowTasks(tmuxBg.agents, tmuxBg.cmds), current: false, pinned: pinAt(e.key), sessionGroup: sessionGroupOf(e, all), onPick: () => onPick(e) }
    })
  }, [entries, titles, all, pins, onPick])
  const shown = mine ? rows.filter((r) => r.status === 'blocked' || r.status === 'done') : rows
  // (Back from a conversation: where it was scrolled to.)
  const box = useRef<HTMLDivElement>(null)
  useLayoutEffect(() => {
    const el = box.current
    if (el) el.scrollTop = listScroll
    return () => {
      if (el) listScroll = el.scrollTop
    }
  }, [])
  return (
    <div className="agents-list agents-phone" ref={box}>
      {/* (Its toolbar as the side list's: search, "waiting on me", the order, new — one row of small buttons. 11-24) */}
      <AgentList
        rows={shown}
        query={query}
        onQuery={onQuery}
        compact
        showNew
        tools={
          <button type="button" className={'agents-phone-mine' + (mine ? ' is-on' : '')} aria-pressed={mine} onClick={() => setMine(!mine)}>
            {t('agents.onlyMine')}
          </button>
        }
        empty={<p className="agents-list-none">{t('agents.noneMine')}</p>}
      />
    </div>
  )
}
