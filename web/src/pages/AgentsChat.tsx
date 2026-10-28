import { useEffect, useLayoutEffect, useMemo, useReducer, useRef, useState, type ReactNode } from 'react'
import { ArrowUp, ChevronLeft, History, Paperclip, Square, X } from 'lucide-react'
import { api, ApiError, authGen, errorText, uploadFile, type SessionInfo } from '../api'
import { t, type I18nKey } from '../i18n'
import { navigate, onNavigate } from '../router'
import { onOpenRequest } from '../notifyopen'
import { isDark, setChromeColor, usePrefs } from '../prefs'
import { local } from '../storage'
import { CLOSE_MENUS, Confirm, Menu, useToast, type MenuItem } from '../components/ui'
import { chromeVars, corralDark, corralLight } from '../terminal/themes'
import { expandPastes, getDraft, keepDraft, keepPaste, keepSent, keptSent, remember, sentHistory } from '../terminal/drafts'
import { geminiPath, shellPath } from '../terminal/paths'
import { useCompletion } from '../terminal/Completion'
import { convoTarget, herdrTarget, InlineReply } from '../terminal/ReplyDialog'
import { ConversationView, ListSearch, SEARCH_FROM, when, type ConvoAgent, type ConvoListRow, type ConvoPending, type ModelOpt } from '../terminal/Conversation'
import { answerable, asking, useConversation, type ConvoEntry } from '../terminal/conversation'
import { ackBg, waitingTitle, isCodex, isOpenCode, listAgents, matchQuery, paneTitle, readsLog, useRunningSessions, type AgentRow } from '../terminal/agents'
import { prefetchConversation } from '../terminal/conversation'
import { afterSend, holdsWords, holdTermMode, PENDING_MS, sameRun, sameTurn, tmuxKey } from '../terminal/chatSend'
import { agentKey, sessionWhich } from '../terminal/sessionName'
import { AgentIcon, agentMark } from '../terminal/agentIcons'
import { useComposerFit } from '../terminal/autosize'

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
const fromList = () => !!(history.state as { agentsFromList?: boolean } | null)?.agentsFromList
const chatAddress = (e: { s: SessionInfo; pane?: string }) => {
  const q = new URLSearchParams({ s: e.s.id })
  if (e.pane) q.set('pane', e.pane)
  return '/agents?' + q
}
const PASTE_LINES = 12
const SEND_MS = 45000 // (the host may wait for another message to the pane to go first)
const PRESS_MS = 20000
// When Claude Code's suggestion is looked for after a turn ends (ms): it
// comes a moment after, at times a while after.
const SUGGEST_AT = [1500, 4000, 9000]
// Of each turn's end asked about (by its key): the looks left, what was
// found, the latest look (only its answer is taken), whether one is on its
// way, and the views showing it. Kept beyond a view, so that going to
// another agent and back does not look all over again.
type SuggestAsk = { left: number; text: string; seq: number; busy: boolean; views: Set<{ told: () => void; next: () => void }> }
const suggestAsks = new Map<string, SuggestAsk>()
// Refusals of a switch made before any key was pressed (see convoPress, convoModeTo).
const NOTHING_PRESSED = ['bad_request', 'unauthorized', 'unsupported', 'busy', 'host_error', 'agent_changed', 'not_in_front', 'refused', 'no_screen', 'not_found', 'forbidden']
const MODE_MS = 50000 // (to a mode picked: several presses, each read back; the gateway gives up at 45 s)
const MODEL_MS = 50000 // (the picker, step by step: see convo_model.go)
const PASTE_CHARS = 2000
const isTouch = window.matchMedia('(pointer: coarse)').matches || navigator.maxTouchPoints > 0
const hasKeyboardPointer = () => !window.matchMedia('(pointer: coarse)').matches
const isMac = /Mac|iPhone|iPad/.test(navigator.platform)

// The agent picked: a herdr session's pane, or a tmux session. (key: its
// entry's, which outlasts the session: a gateway restarted, the same
// agent is in another one.)
type Pick = { s: string; pane?: string; key?: string }

function readPick(): Pick | null {
  const q = new URLSearchParams(location.search)
  const s = q.get('s')
  if (s && /^[0-9a-f]+$/.test(s)) return { s, pane: q.get('pane') ?? undefined }
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
  return [host, r.ws && r.ws !== '~' ? r.ws : '', agentMark(r.a.agent) ? '' : r.a.agent].filter(Boolean).join(' · ')
}
const hostCount = (entries: Entry[]) => new Set(entries.map((e) => e.s.host_name)).size

// What a row says of an entry, on either list (said: its title, as the log
// or its pane has it). The person's own name for it goes first, where there
// is one: a workspace they named says what the work is better than the name
// the agent gave the session, and it does not change under them. What the
// agent called it goes under it. (A session that is not in a workspace of
// its own has no such name: see whereOf.)
const labelOf = (e: Entry, said: string, hosts: number): { title: string; where: string } => {
  const r = e.row
  const ws = r && r.ws !== '~' ? r.ws : ''
  if (!r || !ws) return { title: said, where: whereOf(e, hosts) }
  const host = hosts > 1 ? e.s.host_name : ''
  return { title: ws, where: [host, said !== ws ? said : '', agentMark(r.a.agent) ? '' : r.a.agent].filter(Boolean).join(' · ') }
}

export function AgentsChat({ bar, active = true }: { bar?: ReactNode; active?: boolean }) {
  const toast = useToast()
  const prefs = usePrefs()
  // (Its own two looks, light and dark as the app is: not the terminal's colours.)
  const scheme = isDark() ? corralDark : corralLight
  const { sessions, failed } = useRunningSessions(active, 3000)
  // (On its own, not a tab: the title tells how many wait for an answer.)
  const waitingN = useMemo(() => (sessions ? listAgents(sessions).rows.filter((r) => r.status === 'blocked').length : 0), [sessions])
  useEffect(() => {
    if (!bar) waitingTitle(waitingN)
  }, [bar, waitingN])
  useEffect(() => () => void (!bar && waitingTitle(0)), [bar])
  const [pick, setPick] = useState<Pick | null>(readPick)
  // The entry key of the one picked last (an address names only its session and pane).
  const pickKey = useRef<Pick | null>(pick?.key ? pick : null)
  const keyOf = (p: Pick) => (pickKey.current && pickKey.current.s === p.s && (pickKey.current.pane ?? '') === (p.pane ?? '') ? pickKey.current.key : p.key)
  // (A phone's two screens: the address says which.)
  const stack = !bar
  const [screen, setScreen] = useState<'list' | 'chat'>(() => (stack && !new URLSearchParams(location.search).get('s') ? 'list' : 'chat'))
  // Opened straight at a conversation: the list goes under it, once.
  useEffect(() => {
    if (!stack || screen !== 'chat' || fromList()) return
    const at = location.pathname + location.search
    history.replaceState(null, '', '/agents')
    history.pushState(FROM_LIST, '', at)
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])
  // The titles of agents read here (Claude Code's own), for their rows once another is shown.
  const titles = useRef<Titles>(new Map())

  // Agents closed from here: out of the list at once, before herdr's
  // summary says so (else the one shown would be waited for, or picked
  // again). (A pane's id is not used again.)
  const [gone, setGone] = useState<ReadonlySet<string>>(() => new Set())
  // (Once herdr no longer has it, it is let go: a pane's id may come again
  // in a herdr session made anew, another agent's then.)
  useEffect(() => {
    if (!sessions || !gone.size) return
    const live = new Set(listAgents(sessions).rows.map((r) => r.key))
    if ([...gone].some((k) => !live.has(k))) setGone((g) => new Set([...g].filter((k) => live.has(k))))
  }, [sessions, gone])
  // What the side list is searched for: the page's, as each agent's view is its own.
  const [sideQuery, setSideQuery] = useState('')
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
  const shownKey = useRef<string | undefined>(undefined) // (the last one shown: kept while none is)
  const holding = !picked && !!pick && !pickedSession && !!keyOf(pick) && !gaveUp && shownKey.current === keyOf(pick) && !gone.has(keyOf(pick)!) // (closed from here: not coming back)
  useEffect(() => {
    if (!waiting && !holding) return
    const timer = setTimeout(() => setGaveUp(true), 15000)
    return () => clearTimeout(timer)
  }, [waiting, holding, pick?.s, pick?.pane]) // (another waited for: its own while)
  const cur = picked ?? kept ?? (sessions && !waiting && !holding ? entries[0] : undefined)
  useEffect(() => {
    lastCur.current = cur
    if (cur && !(stack && screen === 'list')) shownKey.current = cur.key // (a phone's list: none of them is in front)
  })
  const choose = (e: Entry) => {
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
  // Back to the list: back the ladder where it came from it, else in place.
  const toList = () => {
    if (fromList()) return history.back()
    setScreen('list')
    navigate('/agents', true)
  }
  // Shown without being picked (none was, its session ended, an address
  // named one gone): it is the pick from now on, so the list's order
  // changing does not move the view. One shown is remembered, however picked.
  useEffect(() => {
    if (!active || !cur || (stack && screen === 'list') || holding) return // (behind a terminal tab: its address is not ours)
    if (!(pick && pick.s === cur.s.id && (pick.pane ?? '') === (cur.pane ?? ''))) return choose(cur)
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
      const p = readPick()
      setPick((was) => (was && p && was.s === p.s && (was.pane ?? '') === (p.pane ?? '') ? was : p))
      setGaveUp(false)
      if (stack) setScreen(new URLSearchParams(location.search).get('s') ? 'chat' : 'list')
    }
    const a = onNavigate(follow)
    const b = onOpenRequest(follow)
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
    setChromeColor(scheme.theme.background ?? null)
    document.documentElement.classList.toggle('term-dark', scheme.dark)
  }, [active, scheme])
  useEffect(() => {
    const vv = window.visualViewport
    const apply = () => {
      const zoomed = vv ? (vv.scale ?? 1) > 1.01 : false
      const root = rootRef.current
      if (!root) return
      root.style.height = `${Math.round(vv && !zoomed ? vv.height : window.innerHeight)}px`
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

  let body: ReactNode
  if (stack && screen === 'list' && sessions !== null && entries.length)
    body = <PhoneList entries={entries} all={sessions} titles={titles.current} query={sideQuery} onQuery={setSideQuery} onPick={choose} />
  else if (!cur || (stack && screen === 'list')) {
    const held = holding && !(stack && screen === 'list') // (a phone's list waits for none)
    body = (
      <div className="agents-chat-empty">
        <p>{sessions === null ? (failed ? t('agents.failed') : t('common.loading')) : waiting ? t('common.loading') : held ? t('agents.holding') : t('agents.chatNone')}</p>
        {sessions !== null && held && (
          <button type="button" className="btn btn-ghost" onClick={() => setGaveUp(true)}>
            {t('agents.holdStop')}
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
    body = (
      <AgentChat
        key={k + ' ' + cur.s.id}
        cur={cur}
        entries={entries}
        all={sessions ?? []}
        titles={titles.current}
        onPick={choose}
        active={active}
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
        onClosed={() => {
          setGone((g) => new Set(g).add(k))
          // (A phone: back to the list, if it is still the one in front; else the next is shown.)
          if (stack && lastCur.current?.key === k) toList()
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

  return (
    <div className={'term-root agents-chat ' + (scheme.dark ? 'dark' : 'light') + (active ? '' : ' inactive') + (stack ? ' is-stack is-' + screen : '')} inert={!active} aria-hidden={active ? undefined : true} style={chromeVars(scheme) as React.CSSProperties} ref={rootRef}>
      {/* (A phone's conversation has its way back in its own bar.) */}
      {/* (The tabs in the bar the terminals have, with its way back to the hosts.) */}
      {bar ? (
        <header className="term-bar">
          <button type="button" className="icon-btn" onClick={() => navigate('/')} aria-label={t('term.backToHosts')} title={t('term.backToHosts')}>
            <ChevronLeft size={20} />
          </button>
          {bar}
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
      )}
      {/* (A list read before, not refreshed since: what it says may be past.) */}
      {failed && sessions !== null && (
        <p className="agents-chat-stale" role="status">
          {t('agents.failed')}
        </p>
      )}
      <div className="term-body">{body}</div>
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
// (not: the log's turns another of the same words was found as, which are
// not this one: kept with it, so a reload does not find this one there.)
// (How many turns a message waiting is kept not to be: see marked.)
const NOT_KEPT = 100
type Sent = ConvoPending & { bind: string; after: number; gen: number; at: number; was?: string; afterTs?: string; not?: string[] }

// What was sent and not seen in the log yet, as this tab kept it (the page
// reloaded meanwhile): looked for again in all of the log, by its time as
// well as its text. One on its way when the page went is not known to have
// gone: it is waited for as any other. (A day on, it is let go.)
// Read beforehand (see above): at most so many at a time, and so long
// after the one in front was opened (its own read goes first).
const PREFETCH_MOST = 3
const PREFETCH_AFTER_MS = 1500

const SENT_KEPT_MS = 24 * 3600 * 1000
const SENT_STATES: readonly string[] = ['sending', 'waiting', 'unseen', 'sent']
function restoreSent(): Record<string, Sent[]> {
  const out: Record<string, Sent[]> = {}
  for (const [k, v] of Object.entries(keptSent())) {
    const ps = (v as Partial<Sent>[])
      .filter((p) => !!p && typeof p.id === 'number' && typeof p.text === 'string' && typeof p.bind === 'string' && typeof p.at === 'number' && Date.now() - p.at < SENT_KEPT_MS && SENT_STATES.includes(p.state as string))
      // (A look at the screen under way when the page went is not asked again: the
      // screen of a later moment says nothing of that one.)
      .map((p) => ({ ...(p as Sent), state: p.state === 'sending' ? ('waiting' as const) : p.state!, look: p.look === 'looking' ? undefined : p.look, gen: -1, after: -Infinity }))
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

// The conversation of one agent, and what is sent to it. Keyed by the
// agent: nothing of another's (its draft, what was sent, a stop) carries over.
function AgentChat({ cur, entries, all, titles, onPick, active, toast, composerEnter, sent, setSent, sending, setSending, draft: shownDraft, setDraftShown, used, lead, onClosed, query, onQuery }: {
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
  onClosed: () => void // (its pane closed from here)
  query: string // the side list's search
  onQuery: (q: string) => void
  onPick: (e: Entry) => void
  active: boolean
  toast: (s: string, error?: boolean) => void
  composerEnter: string
}) {
  const sid = cur.s.id
  const herdr = cur.s.persistence === 'herdr'
  const convo = useConversation(sid, active, cur.pane)
  const row = cur.row
  // What the bar says of it: herdr's word for a herdr agent (as the session
  // list has it, every few seconds); a tmux one's from its process file.
  const agent = useMemo((): ConvoAgent | null => {
    if (!herdr) {
      if (!convo.bind) return null
      const st = !convo.failing && convo.agentStatus
      // (Claude Code's session file: what it waits for. A question of the
      // log's not answered yet, where that file says nothing.)
      const asks = st === 'waiting' ? answerable(convo.waitingFor) : !!st && asking(convo.entries)
      const dialog = st === 'waiting' && !asks
      return { title: convo.title || convo.bind.title, status: asks ? 'blocked' : st === 'busy' ? 'working' : st === 'idle' || dialog ? 'idle' : 'unknown', dialog, program: convo.bind.agent }
    }
    if (!row) return null
    const title = (convo.bind?.pane === row.a.pane_id && convo.title) || paneTitle(row.a.title)
    // (herdr's name for a workspace at home, "~": nothing to tell)
    // (Its turn over, with subagents out or that being read for: the thread
    // is of a turn that ended, whatever the list says of the agent.)
    const ended = !row.lost && (row.a.bg === 'running' || row.a.bg === 'checking') && (row.a.status === 'done' || row.a.status === 'idle')
    const status = row.lost ? 'unknown' : ended && row.bg !== 'stale' ? (row.a.status as 'done' | 'idle') : row.status
    const bg = row.lost ? undefined : row.bg
    return { where: row.ws === '~' ? '' : row.ws, title, status, bg, other: readsLog(row.a.agent) ? undefined : row.a.agent, program: row.a.agent }
  }, [herdr, row, convo.bind, convo.agentStatus, convo.waitingFor, convo.failing, convo.title, convo.entries])
  // A finished one looked at here is seen: the gateway keeps it, so it is
  // no longer "done" in any browser (herdr's own word goes only with its
  // pane focused in herdr). Not while the page is behind another.
  // (Of the round it is done in, as told: a word that comes late, after
  // another round, marks nothing; a round after it is marked in its turn.)
  // (Not while what it left running is read for, nor while it is out.)
  const doneHere = herdr && active && row && !row.lost && row.status === 'done' && row.a.status === 'done' && !row.bg && row.a.bg !== 'checking' ? { pane: row.a.pane_id, round: row.a.round ?? 0, session: row.a.session ?? '' } : undefined
  const doneKey = doneHere && JSON.stringify(doneHere)
  useEffect(() => {
    if (!doneKey) return
    const { pane, round, session } = JSON.parse(doneKey) as { pane: string; round: number; session: string }
    const mark = () => {
      if (!document.hidden) api('POST', `/api/sessions/${sid}/herdr/panes/${encodeURIComponent(pane)}/seen`, { round, session }).catch(() => {})
    }
    mark()
    document.addEventListener('visibilitychange', mark)
    return () => document.removeEventListener('visibilitychange', mark)
  }, [sid, doneKey])
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
  const waits = herdr ? !!row && agent?.status === 'blocked' && !agent.other : !!convo.bind && convo.agentStatus === 'waiting' && answerable(convo.waitingFor)
  // (Claude Code's, its process known: through the conversation, which
  // checks on the host, right before the keys, that this process still
  // waits for an answer on the screen shown. Another agent in herdr: herdr's.)
  const viaConvo = !!convo.bind && convo.bind.agent === 'claude' && (!herdr || convo.bind.pane === row?.a.pane_id)
  const replyTo = useMemo(
    () => (viaConvo && convo.bind ? convoTarget(sid, convo.bind) : herdr && row ? herdrTarget(sid, row.a) : null),
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [viaConvo, herdr, sid, row?.a.pane_id, convo.bind?.pane, convo.bind?.pid, convo.bind?.start, convo.bind?.session],
  )
  const working = agent?.status === 'working'
  const bindKey = convo.bind ? `${convo.bind.pane} ${convo.bind.pid} ${convo.bind.start} ${convo.bind.session}` : ''
  // (A Codex's first message goes before its session is known: sent to the
  // zero one, it is of the session the same process then has.)
  const zeroKey = convo.bind ? `${convo.bind.pane} ${convo.bind.pid} ${convo.bind.start} ${NO_SESSION}` : ''
  const ofThis = (bind: string) => bind === bindKey || (!!zeroKey && bind === zeroKey)

  // Where the one shown is placed on the lists while it is (see asHeld): as
  // it was when it was gone to, or the page came back to, once its state is
  // known. (Not again till then: looked at or answered, it would move.)
  const [placedAs, setPlacedAs] = useState<ConvoListRow['placeAs']>(undefined)
  // The list beside the thread (or from the bar on a narrow screen).
  const rows = useMemo((): ConvoListRow[] => {
    const hosts = hostCount(entries)
    const out = entries.map((e): ConvoListRow => {
      const current = e.key === cur.key && e.s.id === cur.s.id
      if (current && convo.title) {
        titles.delete(e.key) // (the latest last: the map keeps its order of use)
        titles.set(e.key, { title: convo.title, pane: paneTitle(e.row?.a.title ?? ''), session: convo.bind?.session ?? '' })
        if (titles.size > 100) titles.delete(titles.keys().next().value!)
      }
      if (e.row) {
        const r = e.row
        const said = (current && agent?.title) || keptTitle(titles, e) || paneTitle(r.a.title) || r.a.agent
        return { key: e.key, ...labelOf(e, said, hosts), program: r.a.agent, status: current && agent && !r.bg ? agent.status : r.status, bg: r.lost ? undefined : r.bg, since: r.lost ? undefined : r.bg ? r.a.bg_since : r.a.since, current, onPick: () => onPick(e) }
      }
      const title = (current && agent?.title) || keptTitle(titles, e) || `${e.s.host_name} · ${sessionWhich(e.s, all)}`
      return { key: e.key, title, where: whereOf(e, hosts), program: current ? agent?.program : undefined, status: current && agent ? agent.status : 'unknown', current, onPick: () => onPick(e) }
    })
    return placedAs ? out.map((r) => (r.current ? { ...r, placeAs: placedAs } : r)) : out
  }, [entries, cur, agent, convo.title, all, titles, onPick, placedAs])
  const shown = rows.find((r) => r.current)
  const known = !!shown && shown.status !== 'unknown'
  useEffect(() => {
    if (!active) return setPlacedAs(undefined)
    if (known && shown) setPlacedAs((p) => p ?? { status: shown.status, since: shown.since })
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [active, known])

  // Stop and "send now": pressed by the gateway in the agent's pane, for the
  // process the conversation shows; what became of it is said for a while.
  const [said, setSaid] = useState<{ what: 'stop' | 'now'; text: string; busy: boolean; n: number; bind: string } | null>(null)
  const saidN = useRef(0)
  useEffect(() => {
    if (!said || said.busy) return
    const timer = setTimeout(() => setSaid((s) => (s?.n === said.n ? null : s)), said.what === 'stop' ? 15000 : 8000)
    return () => clearTimeout(timer)
  }, [said])
  const refusal = (e: unknown) => {
    if (!(e instanceof ApiError)) return errorText(e)
    if (e.code === 'agent_changed') return t('convo.stopChanged')
    if (e.code === 'not_in_front') return t('convo.notFront')
    // (Of these the gateway says only in English: said here.)
    if (e.code === 'refused') return t('convo.err.refused')
    if (e.code === 'picker') return t('convo.err.picker')
    if (e.code === 'default_changed') return t('convo.err.defaultChanged')
    if (e.code === 'draft') return t('convo.err.draft')
    if (e.code === 'working') return t('convo.err.working')
    if (e.code === 'unknown') return t('convo.err.unknown')
    if (e.code === 'host_error' || e.code === 'unsupported' || e.code === 'bad_request') return t('convo.err.other')
    return errorText(e)
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
  const canPress = bound && working
  // (What Claude Code alone has: its permission modes, its model picker, its "send now".)
  const codex = convo.bind?.agent === 'codex' || (!!row && isCodex(row.a.agent))
  // (Claude Code's own keys and pickers: not Codex's, nor OpenCode's.)
  const notClaude = codex || convo.bind?.agent === 'opencode' || (!!row && isOpenCode(row.a.agent))

  // The permission mode, switched from here: Shift+Tab pressed in its pane,
  // the mode it went to read off its screen (the log says it only with the
  // next message: from then on, that is what shows). Not while it waits for
  // an answer: the key would be the question's.
  // (last: the log's last message of the person's then, of that reading; one
  // after it, or another reading, the log has said the mode since.)
  const [modeNow, setModeNow] = useState<{ mode: string; bind: string; was?: string; last: number; gen: number } | null>(null)
  const [modeBusy, setModeBusy] = useState(false)
  const lastSeq = convo.entries.findLast((e: ConvoEntry) => e.role === 'user')?.seq ?? 0
  if (modeNow && (modeNow.bind !== bindKey || convo.meta?.mode !== modeNow.was || lastSeq > modeNow.last || convo.gen !== modeNow.gen)) setModeNow(null)
  const canMode = bound && !notClaude && convo.status === 'ready' && !!convo.meta && agent?.status !== 'blocked' && !agent?.other
  // To the mode picked: Shift+Tab on the host until its screen shows it
  // (one it was not started with is not there: it is left as it was).
  // (The log as it is when a switch is answered: what it says by then is
  // of before the switch, and goes over it only once it changes again.)
  const logNow = useRef({ bind: bindKey, was: convo.meta?.mode, last: lastSeq, gen: convo.gen })
  logNow.current = { bind: bindKey, was: convo.meta?.mode, last: lastSeq, gen: convo.gen }
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
        toast(refusal(e), true)
      })
      .finally(() => setModeBusy(false))
  }

  // The model, switched from a menu of what Claude Code's picker offers (read
  // when it opens, through the picker: see convo_model.go), for this session
  // only. Only while it is at rest: the picker takes over its input.
  const [models, setModels] = useState<{ list: ModelOpt[] | null; bind: string; at?: number } | null>(null)
  const [modelBusy, setModelBusy] = useState(false)
  // (Said once: a switch here left the default of new sessions changed, as
  // where its request was given up and it was found after.)
  const warnedDefault = useRef(false)
  useEffect(() => {
    if (!convo.defaultChanged || warnedDefault.current) return
    warnedDefault.current = true
    toast(t('convo.err.defaultChanged'), true)
  }, [convo.defaultChanged, toast])
  const canModel = bound && !notClaude && convo.status === 'ready' && !!convo.meta?.model && (agent?.status === 'idle' || agent?.status === 'done') && !agent?.other
  // How hard it is asked to think, set through Claude Code's /effort, for
  // this session only. Offered only where its log has said one: a Claude
  // Code without /effort never does, and nothing is typed to find out.
  // (effortNow: what a switch took, until the log says it — an answer
  // names the level, and /effort writes no row of its own. Dropped when
  // the conversation changes under it, or the log says another.)
  const [effortNow, setEffortNow] = useState<{ effort: string; bind: string; was?: string } | null>(null)
  const [effortBusy, setEffortBusy] = useState(false)
  if (effortNow && (effortNow.bind !== bindKey || convo.meta?.effort !== effortNow.was)) setEffortNow(null)
  const canEffort = bound && !notClaude && convo.status === 'ready' && !!convo.meta?.effort && (agent?.status === 'idle' || agent?.status === 'done') && !agent?.other
  const pressBody = () => {
    const b = convo.bind!
    return { pane: b.pane, pid: b.pid, start: b.start, session: b.session }
  }
  const pickEffort = (level: string) => {
    if (!convo.bind || effortBusy) return
    const bind = bindKey
    const was = convo.meta?.effort
    setEffortBusy(true)
    api<{ effort?: string }>('POST', `/api/sessions/${sid}/conversation/effort`, { ...pressBody(), effort: level }, { timeoutMs: MODEL_MS })
      .then((r) => {
        const took = r.effort || level
        setEffortNow({ effort: took, bind, was })
        toast(t('convo.effortSet', { e: t(`convo.effort.${took}` as I18nKey) }))
      })
      .catch((e) => toast(refusal(e), true))
      .finally(() => setEffortBusy(false))
  }

  const openModels = () => {
    // (Read again after a minute: what it offers and which it has now may have changed.)
    if (!convo.bind || modelBusy || (models && models.bind === bindKey && models.list && Date.now() - (models.at ?? 0) < 60000)) return
    const bind = bindKey
    setModels({ list: null, bind })
    setModelBusy(true)
    api<{ models: ModelOpt[] }>('POST', `/api/sessions/${sid}/conversation/models`, pressBody(), { timeoutMs: MODEL_MS })
      .then((r) => setModels((m) => (m?.bind === bind ? { list: r.models, bind, at: Date.now() } : m)))
      .catch((e) => {
        setModels(null)
        toast(refusal(e), true)
      })
      .finally(() => setModelBusy(false))
  }
  const pickModel = (label: string) => {
    if (!convo.bind || modelBusy) return
    setModelBusy(true)
    api<{ restored?: boolean }>('POST', `/api/sessions/${sid}/conversation/model`, { ...pressBody(), model: label }, { timeoutMs: MODEL_MS })
      .then((r) => {
        toast(t('convo.modelSet', { m: label }) + (r.restored ? t('convo.modelRestored') : ''))
        setModels((m) => m && { ...m, list: m.list && m.list.map((o) => ({ ...o, current: o.label === label })) })
      })
      .catch((e) => toast(refusal(e), true))
      .finally(() => setModelBusy(false))
  }

  // What was sent, until the log has a user turn of its text after it (each
  // turn answers one); not seen in time, it says so. One the agent queued
  // shows in its queue instead.
  const norm = (s: string) => s.replace(/\s+/g, ' ').trim()
  // (Only while the queue shows, not of one at rest: else the message would show nowhere.)
  const queueShown = agent?.status !== 'idle' && agent?.status !== 'done'
  const queued = useMemo(() => new Set((queueShown ? (convo.queued ?? []) : []).map((q) => norm(q.text))), [convo.queued, queueShown])
  const queuedRef = useRef(queued)
  queuedRef.current = queued
  const sentN = useRef(0)
  // What was sent and has since been seen in the log: a request of its own
  // that ends badly is no reason to type it again.
  const arrived = useRef(new Set<number>())
  useEffect(() => {
    if (convo.status !== 'ready' || !sent.some((p) => p.state !== 'sent' && ofThis(p.bind))) return
    const key = bindKey + ' ' + convo.gen
    let u = used.get(key)
    if (!u) {
      used.clear() // (only this reading's turns can confirm one)
      used.set(key, (u = new Set()))
    }
    const users = convo.entries.filter((e: ConvoEntry) => e.role === 'user')
    let changed = false
    const taken: ConvoEntry[] = []
    // (Found: arrived, and its draft, still in the box, let go of.)
    const got = (p: Sent) => {
      arrived.current.add(p.id)
      if (arrived.current.size > 200) arrived.current = new Set([...arrived.current].slice(-100))
      if (p.was !== undefined && getDraft(draftKey) === p.was) {
        keepDraft(draftKey, '')
        setDraftShown((d) => (d === undefined || d === p.was ? '' : d))
      }
    }
    const after = (e: ConvoEntry, p: Sent) => e.seq > p.after && !u.has(e.seq) && afterSend(e, p.at) && !(p.afterTs && e.ts && Date.parse(e.ts) <= Date.parse(p.afterTs))
    let next = sent.flatMap((p) => {
      if (p.state === 'sent' || !ofThis(p.bind)) return [p]
      if (p.bind !== bindKey) {
        changed = true
        p = { ...p, bind: bindKey }
      }
      if (p.gen !== convo.gen) {
        // (The log it went to, now written, or read anew: looked for in all
        // of it, by its time as well as its text.)
        changed = true
        p = { ...p, gen: convo.gen, after: -Infinity }
      }
      const hit = users.find((e) => after(e, p) && !p.not?.includes(e.id) && sameTurn(e, p.text))
      if (!hit) return [p]
      u.add(hit.seq)
      if (hit.id) taken.push(hit)
      got(p)
      if (u.size > 200) for (const q of [...u].sort((a, b) => a - b).slice(0, 100)) u.delete(q)
      changed = true
      return []
    })
    const merged: ConvoEntry[] = [] // (turns found as several: no other's, after a reload too)
    // Two or more sent one after another, not found each as its own turn,
    // and taken in as one (the first's Enter let go of): all of them arrived.
    for (let i = 0; i < next.length; i++) {
      const at = next.slice(i)
      const n = at.findIndex((q) => q.state === 'sent' || !ofThis(q.bind))
      const run = n < 0 ? at.slice(0, 5) : at.slice(0, Math.min(n, 5))
      for (let k = run.length; k >= 2; k--) {
        // (Written after each of them went, and not a turn any of them was found not to be.)
        const hit = users.find((e) => run.slice(0, k).every((q) => after(e, q) && !q.not?.includes(e.id)) && sameRun(e, run.slice(0, k).map((q) => q.text)))
        if (!hit) continue
        u.add(hit.seq)
        if (hit.id) merged.push(hit)
        const gone = new Set(run.slice(0, k).map((q) => q.id))
        run.slice(0, k).forEach(got)
        next = next.filter((q) => !gone.has(q.id))
        changed = true
        i--
        break
      }
    }
    // (Any other it would be found as is not that turn, after a reload too.)
    // (Of each still waiting, those of its words: it could be found as them,
    // alone or with others, and is not. Kept for long: one let go of could
    // be found again, after a reload.)
    const marked =
      taken.length || merged.length
        ? next.map((q) => {
            const add = [...taken, ...merged].filter((x) => !q.not?.includes(x.id) && (sameTurn(x, q.text) || holdsWords(x, q.text))).map((x) => x.id)
            return add.length ? { ...q, not: [...(q.not ?? []), ...add].slice(-NOT_KEPT) } : q
          })
        : next
    if (changed) setSent(() => marked)
  }, [convo.entries, convo.gen, convo.status, sent, bindKey])
  useEffect(() => {
    const waiting = sent.filter((p) => p.state === 'waiting' && !queued.has(norm(p.text)))
    if (!waiting.length) return
    const due = Math.min(...waiting.map((p) => p.at)) + PENDING_MS - Date.now()
    const timer = setTimeout(() => {
      const late = (p: Sent) => p.state === 'waiting' && Date.now() - p.at >= PENDING_MS && !queuedRef.current.has(norm(p.text))
      setSent((ps) => (ps.some(late) ? ps.map((p) => (late(p) ? { ...p, state: 'unseen' as const, look: 'looking' as const } : p)) : ps))
    }, Math.max(0, due) + 50)
    return () => clearTimeout(timer)
  }, [sent, queued])
  // A message that has not turned up in the log: the pane's screen is read
  // once, there and then, which is the only moment that can say whether the
  // words are still sitting in its input. (Afterwards nothing can.)
  // (Which were asked after is kept outside this page: going to another
  // agent and back builds it anew, and the same read must not go twice.)
  useEffect(() => {
    for (const p of sent) {
      if (p.look !== 'looking' || lookedAt.has(p.id) || !convo.bind || !ofThis(p.bind)) continue
      lookedAt.add(p.id)
      if (lookedAt.size > 200) for (const q of [...lookedAt].slice(0, 100)) lookedAt.delete(q)
      const bind = bindKey // (the answer is of that conversation, not of whichever is open when it comes)
      const at = (look: ConvoPending['look']) => setSent((ps) => ps.map((q) => (q.id === p.id && q.bind === bind ? { ...q, look } : q)))
      api<{ screen: string; waiting: boolean; known: boolean }>('POST', `/api/sessions/${sid}/conversation/sent-missing`, { ...pressBody(), text: p.text }, { timeoutMs: PRESS_MS })
        .then((r) => at({ waiting: r.waiting, known: r.known, screen: r.screen }))
        .catch(() => at('failed'))
    }
  }, [sent, convo.bind, sid, bindKey])

  const shownSent = sent.filter((p) => ofThis(p.bind) && !(p.state !== 'sent' && queued.has(norm(p.text))))

  // The composer: its draft kept per agent (as the terminal's per session;
  // a tmux session's per pane: another pane in front is another agent).
  const baseKey = 'agents ' + cur.key
  const draftKey = baseKey + (!herdr && convo.bind ? ' ' + convo.bind.pane : '')
  // (The box in front: what another agent's send puts back is not shown over this one's.)
  const draftKeyNow = useRef(draftKey)
  draftKeyNow.current = draftKey
  const draft = shownDraft ?? getDraft(draftKey)
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
    upAbort.current?.abort() // (what goes up has its path for the agent before)
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
  const completion = useCompletion({ sessionId: sid, field, draft, setDraft, agent: row?.a.agent ?? convo.bind?.agent ?? (codex ? 'codex' : 'claude'), pane: herdr ? cur.pane : undefined })
  // (Another agent picked with the keyboard, or the page back in front: its
  // box takes the keys, as the terminal's does. Not from a dialog over it.)
  useEffect(() => {
    if (!active || !hasKeyboardPointer()) return
    const frame = requestAnimationFrame(() => {
      const at = document.activeElement
      if ((!at || at === document.body) && !document.querySelector('[aria-modal="true"]')) field.current?.focus()
    })
    return () => cancelAnimationFrame(frame)
  }, [active])
  // Text put in where the cursor is (or at the end, the field away).
  const insert = (text: string) => {
    const el = field.current
    const d = el ? el.value : draft
    const a = el?.selectionStart ?? d.length
    const b = el?.selectionEnd ?? a
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
  // Files (picked, pasted, dropped) go up to the host one at a time, each
  // path then put in the draft for the agent to read (not sent: there is
  // more to say, as a rule). Sending waits for them.
  const [up, setUp] = useState<{ name: string; p: number; abort: AbortController } | null>(null)
  const fileRef = useRef<HTMLInputElement>(null)
  // (Another agent shown, or the page left: what goes up is stopped, its path having nowhere to go.)
  const alive = useRef(true)
  const upAbort = useRef<AbortController | null>(null)
  useEffect(() => {
    alive.current = true
    return () => {
      alive.current = false
      upAbort.current?.abort()
    }
  }, [])
  const upload = async (files: File[]) => {
    if (up) return toast(t('term.waitUpload'), true)
    const forKey = draftKey // (its path is for this agent's draft only)
    for (const f of files) {
      if (!alive.current || lastKey.current !== forKey) break
      const abort = new AbortController()
      upAbort.current = abort
      const name = f.name || 'image'
      setUp({ name, p: 0, abort })
      try {
        const r = await uploadFile(sid, f, (p) => setUp((u) => (u?.abort === abort ? { ...u, p } : u)), abort.signal)
        setUp((u) => (u?.abort === abort ? null : u))
        const quoted = r.prefix === '@' ? geminiPath(r.path) : shellPath(r.path)
        if (!alive.current || lastKey.current !== forKey) break
        if (quoted) insert(quoted + ' ')
        else toast(r.path)
      } catch (e) {
        setUp((u) => (u?.abort === abort ? null : u))
        if (e instanceof ApiError && e.code === 'aborted') {
          if (alive.current) toast(t('term.uploadStopped', { name }))
          break
        }
        toast(errorText(e), true)
      }
    }
  }
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
          api<{ suggestion: string }>('POST', `/api/sessions/${sid}/conversation/suggestion`, pressBody(), { timeoutMs: PRESS_MS }).then(
            (r) => done(r.suggestion),
            () => done(null),
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
  const to = agent ? [agent.title, agent.where].filter((x) => x && x !== '~').join(' · ') : ''
  const placeholder = suggestion ?? (to ? t('convo.toPlaceholder', { name: to }) : t('term.composerPlaceholder'))
  useComposerFit(field, draft, placeholder)
  // Why it cannot send now, if it cannot.
  // (Waiting for an answer, a message's Enter could take an option of its
  // question, or of a permission it asks: the answer first.)
  const blocked = agent?.other
    ? t('convo.otherAgent', { agent: agent.other })
    : !bound
      ? t(convo.status === 'loading' ? 'agents.loadingSend' : convo.status === 'none' && convo.reason === 'no_agent' ? 'agents.noAgentSend' : 'agents.cannotSend')
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
  const send = () => {
    const b = convo.bind
    if (sending || (!draft.trim() && !up)) return
    if (blocked || !b) return toast(blocked ?? t('agents.cannotSend'), true)
    if (up) return toast(t('term.waitUpload'), true) // (its path is not in the draft yet; nor, while it goes up, in an empty one)
    const text = expandPastes(draft, 100 << 10)
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
    setSent((ps) => [...ps, { id, text, state: 'sending' as const, bind: bindKey, after, afterTs, gen: convo.gen, at }].slice(-5))
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

    api('POST', `/api/sessions/${sid}/conversation/send`, { pane: b.pane, pid: b.pid, start: b.start, session: b.session, text }, { timeoutMs: SEND_MS })
      .then(() => {
        if (authGen() !== account) return drop()
        remember(was)
        mark({ state: 'waiting' })
        convo.refresh() // (it is in its log in a moment: read now, not at the next tick)
      })
      .catch((e) => {
        if (authGen() !== account) {
          drop()
          return
        }
        // Whether it went is not known (the gateway lost the host on the way,
        // or the answer was lost on its way here): it is looked for as one
        // sent, the draft staying until it is seen.
        if (e instanceof ApiError && e.code === 'typed_not_sent') {
          // (In its input, not sent: it came to ask first. Not typed again from
          // here — sent again, it would be there twice: Enter, in the terminal,
          // once it is answered. Looked for as one sent meanwhile.)
          toast(t('agents.typedNotSent'), true)
          mark({ state: 'waiting', was })
          return
        }
        if (e instanceof ApiError && (e.code === 'unknown' || e.code === 'network')) {
          // (It is looked for as one sent; the draft comes back until it is seen.)
          toast(t('agents.sendUnknown'), true)
          putBack()
          mark({ state: 'waiting', was })
        } else {
          putBack() // (it never went: what was typed comes back, to be sent again)
          drop()
          toast(refusal(e), true)
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
    <div className="composer">
      {completion.list}
      {up && (
        <div className="agents-chat-up" role="status">
          <span>{t('term.uploading', { name: up.name })} {Math.round(up.p * 100)}%</span>
          <button type="button" className="icon-btn" onClick={() => up.abort.abort()} aria-label={t('term.uploadStop')} title={t('term.uploadStop')}>
            <X size={15} />
          </button>
        </div>
      )}
      {/* (Why nothing can be sent now: over the box, where a phone shows it too) */}
      {blocked && <p className="agents-chat-why">{blocked}</p>}
      <div className="composer-box">
        <textarea
          ref={field}
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
          onBlur={() => setTimeout(completion.update)}
          onPaste={(e) => {
            const files = [...(e.clipboardData?.files ?? [])]
            if (files.length > 0) {
              e.preventDefault()
              void upload(files)
              return
            }
            // A long paste goes in as a label, as in the terminal's composer; it is sent as the text.
            const text = e.clipboardData?.getData('text/plain') ?? ''
            if (text.split('\n').length <= PASTE_LINES && text.length <= PASTE_CHARS) return
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
            if (completion.onKey(e)) return
            const el = e.currentTarget
            const plain = !e.shiftKey && !e.altKey && !e.metaKey && !e.ctrlKey && el.selectionStart === el.selectionEnd
            if (plain && e.key === 'ArrowUp' && el.selectionStart === 0 && historyStep(-1)) {
              e.preventDefault()
            } else if (plain && e.key === 'ArrowDown' && el.selectionEnd === el.value.length && historyStep(1)) {
              e.preventDefault()
            } else if (e.key === 'Enter' && (e.metaKey || e.ctrlKey)) {
              e.preventDefault()
              send()
            } else if (e.key === 'Enter' && hasKeyboardPointer() && composerEnter === 'send' && !e.shiftKey && !e.altKey) {
              e.preventDefault()
              send()
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
                label: h.trim().replace(/\s*\n\s*/g, ' ⏎ ').slice(0, 80),
                onSelect: () => {
                  showSent(h)
                  requestAnimationFrame(() => {
                    const el = field.current
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
          {stopShown ? (
            <button type="button" className="composer-send is-stop" onClick={() => press('stop')} aria-label={t('convo.stop')} title={t('convo.stop')}>
              <Square size={14} fill="currentColor" strokeWidth={0} />
            </button>
          ) : (
            <button type="button" className="composer-send" onClick={send} disabled={sending || !!blocked || (!draft.trim() && !up)} aria-label={t('term.send')} title={t('term.send') + (isTouch ? '' : ' (' + sendKey + ')')}>
              <ArrowUp size={18} strokeWidth={2.25} />
            </button>
          )}
        </div>
      </div>
    </div>
  )

  const openTerminal = () => navigate(`/t/${sid}` + (cur.pane ? '?pane=' + encodeURIComponent(cur.pane) : ''))
  // Closing it: its herdr pane, asked first (what runs there ends; its
  // conversation stays on the host, to be taken up again in a terminal).
  // Not a tmux one's: its entry is the whole tmux session, other windows too.
  const [closing, setClosing] = useState(false)
  const canClose = herdr && !!row && !row.lost
  const more = (): (MenuItem | null)[] => [
    { label: t('convo.showTerminal'), onSelect: openTerminal },
    canClose ? { label: t('agents.close'), danger: true, separator: true, onSelect: () => setClosing(true) } : null,
  ]
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
      convo={convo}
      agent={agent}
      pending={shownSent}
      ask={
        waits && replyTo ? (
          // (One a pane, and a process in it: nothing of another's carries over. Not tied to a terminal: no size to wait for.)
          <InlineReply key={replyTo.key} target={replyTo} resizedAt={0} active={active} held={() => false} />
        ) : undefined
      }
      onStop={canPress && !lead ? () => press('stop') : undefined} // (a phone: the composer's button stops it, the bar keeps the room for the name)
      stop={said?.what === 'stop' && said.bind === bindKey ? said.text : undefined}
      side={{ rows }}
      active={active}
      onDismiss={(id) => setSent((ps) => ps.filter((p) => p.id !== id))}
      onShowTerminal={openTerminal}
      onCopied={() => toast(t('common.copied'))}
      composer={composer}
      onSendNow={canPress && !notClaude ? () => press('now') : undefined}
      sendNow={said?.what === 'now' && said.bind === bindKey ? said.text : undefined}
      mode={{ now: modeNow?.bind === bindKey ? modeNow.mode : undefined, busy: modeBusy, onPick: canMode ? pickMode : undefined }}
      model={canModel || modelBusy ? { list: models?.bind === bindKey ? models.list : null, fresh: !!models?.at && Date.now() - models.at < 60000, busy: modelBusy, can: canModel, onOpen: openModels, onPick: pickModel } : undefined}
      effort={{ now: effortNow?.bind === bindKey ? effortNow.effort : undefined, busy: effortBusy, can: canEffort, onPick: pickEffort }}
      lead={lead}
      more={more}
      query={query}
      onQuery={onQuery}
    />
    {closing && row && (
      <Confirm
        title={t('agents.closeTitle')}
        body={t(working ? 'agents.closeBodyWorking' : 'agents.closeBody')}
        confirm={t('agents.closeConfirm')}
        danger
        onConfirm={async () => {
          try {
            await api('POST', `/api/sessions/${sid}/herdr`, { action: 'close_pane', target: row.a.pane_id })
          } catch (e) {
            throw { human: e instanceof ApiError && e.code === 'herdr_busy' ? t('herdr.busy') : t('sc.herdrFailed') }
          }
          onClosed()
        }}
        onClose={() => setClosing(false)}
      />
    )}
    </div>
  )
}

// A phone's list of the agents: those for you first (waiting, then done and
// not looked at, as the side list's first group), then working, then the
// rest, the latest to change first in each, as it was when the list showed
// (statuses change in place; rows do not move under the finger; one new comes
// last). Its summary line puts them in order again and goes to the first
// waiting; "only mine" keeps those waiting, and those done since.
let listScroll = 0
// query, onQuery: what it is searched for, kept by the page (as the side
// list's is): still searched, back from a conversation.
function PhoneList({ entries, all, titles, query, onQuery, onPick }: { entries: Entry[]; all: SessionInfo[]; titles: Titles; query: string; onQuery: (q: string) => void; onPick: (e: Entry) => void }) {
  const [now, setNow] = useState(Date.now())
  useEffect(() => {
    const timer = setInterval(() => setNow(Date.now()), 30000)
    return () => clearInterval(timer)
  }, [])
  const [mine, setMineState] = useState(() => local.get(ONLY_MINE) === '1')
  const setMine = (v: boolean) => {
    setMineState(v)
    try {
      local.set(ONLY_MINE, v ? '1' : '0')
    } catch {
      /* not kept */
    }
  }
  const rank = (e: Entry) => ['blocked', 'done', 'working'].indexOf(e.row?.status ?? '') + 1 || 4
  const sorted = (list: Entry[]) => list.map((e, i) => [e, i] as const).sort(([a, i], [b, j]) => rank(a) - rank(b) || (b.row?.a.since ?? 0) - (a.row?.a.since ?? 0) || i - j).map(([e]) => e.key + ' ' + e.s.id)
  const [order, setOrder] = useState<string[]>(() => sorted(entries))
  useEffect(() => {
    setOrder((o) => {
      const have = new Set(entries.map((e) => e.key + ' ' + e.s.id))
      const kept = o.filter((k) => have.has(k))
      const added = entries.filter((e) => !kept.includes(e.key + ' ' + e.s.id)).map((e) => e.key + ' ' + e.s.id)
      return kept.length === o.length && !added.length ? o : [...kept, ...added]
    })
  }, [entries])
  const byKey = new Map(entries.map((e) => [e.key + ' ' + e.s.id, e]))
  const list = order.flatMap((k) => (byKey.has(k) ? [byKey.get(k)!] : []))
  const n = (st: string) => entries.filter((e) => e.row?.status === st).length
  const waiting = n('blocked')
  const working = n('working')
  const done = n('done')
  const box = useRef<HTMLDivElement>(null)
  // (Back from a conversation: where the list was scrolled to.)
  useLayoutEffect(() => {
    const el = box.current
    if (el) el.scrollTop = listScroll
    return () => {
      listScroll = el?.scrollTop ?? 0
    }
  }, [])
  const regroup = () => {
    setOrder(sorted(entries))
    requestAnimationFrame(() => box.current?.scrollTo({ top: 0, behavior: matchMedia('(prefers-reduced-motion: reduce)').matches ? 'auto' : 'smooth' }))
  }
  const hosts = hostCount(entries)
  // (Searched: by its title, where it is, its program; "only mine" still holds.)
  const labelled = (e: Entry) => labelOf(e, keptTitle(titles, e) || (e.row ? paneTitle(e.row.a.title) || e.row.a.agent : `${e.s.host_name} · ${sessionWhich(e.s, all)}`), hosts)
  const shown = (mine ? list.filter((e) => e.row?.status === 'blocked' || e.row?.status === 'done') : list).filter((e) => {
    if (!query.trim()) return true
    const { title, where } = labelled(e)
    return matchQuery(query, title, where, e.row?.a.agent)
  })
  return (
    <div className="agents-list" ref={box}>
      <div className="agents-list-top">
        <button type="button" className="agents-list-sum" onClick={regroup} title={t('agents.sumHint')}>
          {waiting > 0 && <span className="is-blocked">{t('agents.sumWaiting', { n: waiting })}</span>}
          {working > 0 && <span className="is-working">{t('agents.sumWorking', { n: working })}</span>}
          {done > 0 && <span className="is-done">{t('agents.sumDone', { n: done })}</span>}
          {!waiting && !working && !done && <span>{t('agents.sumAll', { n: entries.length })}</span>}
        </button>
        <button type="button" className="btn btn-ghost agents-list-mine" aria-pressed={mine} onClick={() => setMine(!mine)}>
          {t('agents.onlyMine')}
        </button>
      </div>
      {(entries.length > SEARCH_FROM || query) && <ListSearch value={query} onChange={onQuery} />}
      {shown.length === 0 && <p className="agents-list-none">{t(query.trim() ? 'convo.searchNone' : 'agents.noneMine')}</p>}
      <ul className="agents-list-rows">
        {shown.map((e) => {
          const r = e.row
          const status = r?.lost ? 'unknown' : (r?.status ?? 'unknown')
          const { title, where } = labelled(e)
          const since = r && !r.lost ? when({ since: r.a.since, status }, now) : ''
          return (
            <li key={e.key + ' ' + e.s.id}>
              <button type="button" className={'agents-list-row is-' + status} onClick={() => onPick(e)}>
                <AgentIcon program={r?.a.agent} size={36} dot={r?.bg === 'running' && !r.lost ? 'bg' : status === 'blocked' || status === 'working' || status === 'done' ? status : undefined} />
                <span className="agents-list-text">
                  <span className="agents-list-title">{title}</span>
                  {/* (Done and not seen, or at work: said by its dot, its time beside it, as the
                      side list does; in words for a screen reader, and where forced colours take the dot's.) */}
                  {status === 'done' && since && <span className="convo-list-status">{t('agents.unseen')}</span>}
                  {status === 'working' && since && <span className="convo-list-status">{t('convo.status.working')}</span>}
                  <span className="agents-list-where">{where}</span>
                </span>
                {/* (Waiting for you, in words: no group heading says it here.) */}
                <span className={'agents-list-side is-' + (status === 'done' && since ? 'idle' : status)}>
                  {r?.bg && !r.lost
                    ? t(r.bg === 'running' ? 'convo.status.bg' : 'convo.status.bgStale')
                    : status === 'blocked'
                      ? t('convo.status.blocked')
                      : status === 'done' && !since
                        ? t('agents.unseen')
                        : since || (status === 'working' ? t('convo.status.working') : '')}
                </span>
              </button>
            </li>
          )
        })}
      </ul>
    </div>
  )
}
