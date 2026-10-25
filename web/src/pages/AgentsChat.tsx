import { useEffect, useMemo, useRef, useState, type ReactNode } from 'react'
import { ArrowUp, ChevronLeft, History, Paperclip, Square, X } from 'lucide-react'
import { api, ApiError, errorText, uploadFile, type SessionInfo } from '../api'
import { t } from '../i18n'
import { navigate } from '../router'
import { setChromeColor, termScheme, usePrefs } from '../prefs'
import { local } from '../storage'
import { Menu, useToast } from '../components/ui'
import { chromeVars } from '../terminal/themes'
import { expandPastes, getDraft, keepDraft, keepPaste, remember, sentHistory } from '../terminal/drafts'
import { geminiPath, shellPath } from '../terminal/paths'
import { useCompletion } from '../terminal/Completion'
import { InlineReply } from '../terminal/ReplyDialog'
import { ConversationView, when, type ConvoAgent, type ConvoListRow, type ConvoPending } from '../terminal/Conversation'
import { asking, useConversation, type ConvoEntry } from '../terminal/conversation'
import { isClaude, listAgents, paneTitle, useRunningSessions, type AgentRow } from '../terminal/agents'
import { afterSend, holdTermMode, PENDING_MS, sameTurn, tmuxKey } from '../terminal/chatSend'
import { sessionWhich } from '../terminal/sessionName'

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
const PASTE_CHARS = 2000
const isTouch = window.matchMedia('(pointer: coarse)').matches || navigator.maxTouchPoints > 0
const hasKeyboardPointer = () => !window.matchMedia('(pointer: coarse)').matches
const isMac = /Mac|iPhone|iPad/.test(navigator.platform)

// The agent picked: a herdr session's pane, or a tmux session.
type Pick = { s: string; pane?: string }

function readPick(): Pick | null {
  const q = new URLSearchParams(location.search)
  const s = q.get('s')
  if (s && /^[0-9a-f]+$/.test(s)) return { s, pane: q.get('pane') ?? undefined }
  try {
    const p = JSON.parse(local.get(LAST) ?? 'null')
    if (p && typeof p.s === 'string') return { s: p.s, pane: typeof p.pane === 'string' ? p.pane : undefined }
  } catch {
    /* none kept */
  }
  return null
}

// One entry of the list: a herdr agent, or a tmux session (whose agent is
// whatever runs in it; only the one shown has a known state).
type Entry = { key: string; s: SessionInfo; pane?: string; row?: AgentRow }

export function AgentsChat({ bar, active = true }: { bar?: ReactNode; active?: boolean }) {
  const toast = useToast()
  const prefs = usePrefs()
  const scheme = termScheme()
  const { sessions, failed } = useRunningSessions(active, 3000)
  const [pick, setPick] = useState<Pick | null>(readPick)
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
  const titles = useRef(new Map<string, string>())

  const entries = useMemo((): Entry[] => {
    const { rows, others } = listAgents(sessions ?? [])
    const out: Entry[] = rows.map((r) => ({ key: r.key, s: r.s, pane: r.a.pane_id, row: r }))
    const seen = new Set<string>()
    for (const s of others) {
      if (s.persistence !== 'tmux' || seen.has(tmuxKey(s))) continue
      seen.add(tmuxKey(s))
      out.push({ key: tmuxKey(s), s })
    }
    return out
  }, [sessions])
  // The one shown: the one picked while it is there (a session without a
  // pane, from an older address: the agent herdr has in front there, else
  // its first), else the first (one waiting for an answer comes first).
  // (A tmux session open in several connections is listed once, by one of
  // them: picked by another, it is that entry.)
  const pickedSession = pick ? sessions?.find((x) => x.id === pick.s) : undefined
  const picked = pick
    ? entries.find((e) => e.s.id === pick.s && (e.pane ?? '') === (pick.pane ?? '')) ??
      (pickedSession?.persistence === 'tmux' && !pick.pane ? entries.find((e) => !e.pane && e.key === tmuxKey(pickedSession)) : undefined) ??
      (pick.pane === undefined ? entries.find((e) => e.s.id === pick.s && e.s.meta?.t === 'herdr' && e.pane === e.s.meta.focused_pane) ?? entries.find((e) => e.s.id === pick.s) : undefined)
    : undefined
  // One picked that is not in the list for a moment, as herdr's summary of
  // its session is not there or not current, stays shown as last seen; one
  // a current summary no longer has (its pane closed) does not.
  const lastCur = useRef<Entry | undefined>(undefined)
  const unsure = !!pickedSession && pickedSession.persistence === 'herdr' && (pickedSession.meta?.t !== 'herdr' || !!pickedSession.meta.lost)
  const kept = !picked && pick && unsure && lastCur.current?.s.id === pick.s && (lastCur.current.pane ?? '') === (pick.pane ?? '') ? lastCur.current : undefined
  const cur = picked ?? kept ?? (sessions ? entries[0] : undefined)
  useEffect(() => {
    lastCur.current = cur
  })
  const choose = (e: Entry) => {
    setPick({ s: e.s.id, pane: e.pane })
    try {
      local.set(LAST, JSON.stringify({ s: e.s.id, pane: e.pane }))
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
    if (!cur || (stack && screen === 'list')) return
    if (!(pick && pick.s === cur.s.id && (pick.pane ?? '') === (cur.pane ?? ''))) return choose(cur)
    try {
      local.set(LAST, JSON.stringify({ s: cur.s.id, pane: cur.pane }))
    } catch {
      /* not kept */
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [cur?.key, cur?.s.id, pick?.s, pick?.pane, screen])
  // Back and forward between agents: the address says which (and, on a
  // phone, which screen).
  useEffect(() => {
    const onPop = () => {
      if (location.pathname !== '/agents') return
      setPick(readPick())
      if (stack) setScreen(new URLSearchParams(location.search).get('s') ? 'chat' : 'list')
    }
    window.addEventListener('popstate', onPop)
    return () => window.removeEventListener('popstate', onPop)
  }, [stack])
  // What was sent to each agent and is not in its log yet, and whether a
  // message to it is on its way: kept here, so going to another agent and
  // back loses neither (a second send of the same could follow).
  // So are its draft as shown, and the log's turns that confirmed one.
  const [sentBy, setSentBy] = useState<Record<string, Sent[]>>({})
  const [sendingBy, setSendingBy] = useState<Record<string, boolean>>({})
  const [draftBy, setDraftBy] = useState<Record<string, string>>({})
  const usedBy = useRef(new Map<string, Map<string, Set<number>>>())
  // (Of agents gone: forgotten. A draft stays in drafts.ts's keeping.)
  useEffect(() => {
    if (!sessions) return
    // (Kept while a herdr summary is missing or lost: its agents may only be out of the list for now.)
    if (sessions.some((x) => x.persistence === 'herdr' && (x.meta?.t !== 'herdr' || x.meta.lost))) return
    const keys = new Set(entries.map((e) => e.key))
    const live = (k: string) => keys.has(k)
    const prune = <T,>(all: Record<string, T>) => (Object.keys(all).every(live) ? all : Object.fromEntries(Object.entries(all).filter(([k]) => live(k))))
    setSentBy(prune)
    setSendingBy(prune)
    setDraftBy(prune)
    for (const k of usedBy.current.keys()) if (!live(k)) usedBy.current.delete(k)
  }, [sessions, entries])

  // The page's colours and size: the terminal's, and the visible part of
  // the window (the soft keyboard does not shrink the layout viewport on iOS).
  const rootRef = useRef<HTMLDivElement>(null)
  useEffect(() => holdTermMode(), [])
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
    body = <PhoneList entries={entries} all={sessions} titles={titles.current} onPick={choose} />
  else if (!cur || (stack && screen === 'list'))
    body = (
      <div className="agents-chat-empty">
        <p>{sessions === null ? (failed ? t('agents.failed') : t('common.loading')) : t('agents.chatNone')}</p>
        {sessions !== null && (
          <button type="button" className="btn btn-ghost" onClick={() => navigate('/')}>
            {t('agents.toHosts')}
          </button>
        )}
      </div>
    )
  else {
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
      {bar ?? (stack && screen === 'chat' && cur ? null : (
        <header className="term-bar">
          <button type="button" className="icon-btn" onClick={() => navigate('/')} aria-label={t('common.back')} title={t('common.back')}>
            <ChevronLeft size={20} />
          </button>
          <span className="agents-chat-title">{t('nav.agents')}</span>
        </header>
      ))}
      <div className="term-body">{body}</div>
    </div>
  )
}

type Sent = ConvoPending & { bind: string; after: number; gen: number; at: number }

// The conversation of one agent, and what is sent to it. Keyed by the
// agent: nothing of another's (its draft, what was sent, a stop) carries over.
function AgentChat({ cur, entries, all, titles, onPick, active, toast, composerEnter, sent, setSent, sending, setSending, draft: shownDraft, setDraftShown, used, lead }: {
  cur: Entry
  entries: Entry[]
  all: SessionInfo[]
  titles: Map<string, string>
  sent: Sent[]
  setSent: (f: (ps: Sent[]) => Sent[]) => void
  sending: boolean
  setSending: (v: boolean) => void
  draft: string | undefined // (undefined: as kept in drafts.ts)
  setDraftShown: (f: (d: string | undefined) => string) => void
  used: Map<string, Set<number>> // per reading: the turns that confirmed one
  lead?: ReactNode
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
      const asks = !!st && asking(convo.entries)
      return { title: convo.title || convo.bind.title, status: asks ? 'blocked' : st === 'busy' ? 'working' : st === 'idle' ? 'idle' : 'unknown' }
    }
    if (!row) return null
    const title = (convo.bind?.pane === row.a.pane_id && convo.title) || paneTitle(row.a.title)
    return { where: row.ws, title, status: row.lost ? 'unknown' : row.status, other: isClaude(row.a.agent) ? undefined : row.a.agent }
  }, [herdr, row, convo.bind, convo.agentStatus, convo.failing, convo.title, convo.entries])
  const bound = convo.status === 'ready' && !!convo.bind && (!herdr || convo.bind.pane === cur.pane)
  // (The answer card reads the pane's screen: it needs no log read.)
  const waits = herdr && !!row && agent?.status === 'blocked' && !agent.other
  const working = agent?.status === 'working'
  const bindKey = convo.bind ? `${convo.bind.pane} ${convo.bind.pid} ${convo.bind.start} ${convo.bind.session}` : ''

  // The list beside the thread (or from the bar on a narrow screen).
  const rows = useMemo((): ConvoListRow[] => {
    return entries.map((e) => {
      const current = e.key === cur.key && e.s.id === cur.s.id
      if (current && convo.title) {
        titles.delete(e.key) // (the latest last: the map keeps its order of use)
        titles.set(e.key, convo.title)
        if (titles.size > 100) titles.delete(titles.keys().next().value!)
      }
      if (e.row) {
        const r = e.row
        const title = (current && agent?.title) || titles.get(e.key) || paneTitle(r.a.title) || r.a.agent
        return { key: e.key, title, where: [r.s.host_name, r.ws, isClaude(r.a.agent) ? '' : r.a.agent].filter(Boolean).join(' · '), status: current && agent ? agent.status : r.status, since: r.lost ? undefined : r.a.since, current, onPick: () => onPick(e) }
      }
      const title = (current && agent?.title) || titles.get(e.key) || `${e.s.host_name} · ${sessionWhich(e.s, all)}`
      return { key: e.key, title, where: e.s.host_name + ' · tmux', status: current && agent ? agent.status : 'unknown', current, onPick: () => onPick(e) }
    })
  }, [entries, cur, agent, convo.title, all, titles, onPick])

  // Stop and "send now": pressed by the gateway in the agent's pane, for the
  // process the conversation shows; what became of it is said for a while.
  const [said, setSaid] = useState<{ what: 'stop' | 'now'; text: string; busy: boolean; n: number; bind: string } | null>(null)
  const saidN = useRef(0)
  useEffect(() => {
    if (!said || said.busy) return
    const timer = setTimeout(() => setSaid((s) => (s?.n === said.n ? null : s)), said.what === 'stop' ? 15000 : 8000)
    return () => clearTimeout(timer)
  }, [said])
  const refusal = (e: unknown) =>
    e instanceof ApiError && e.code === 'agent_changed' ? t('convo.stopChanged') : e instanceof ApiError && e.code === 'not_in_front' ? t('convo.notFront') : errorText(e)
  const press = (what: 'stop' | 'now') => {
    const b = convo.bind
    if (!b || (said?.busy && said.bind === bindKey)) return
    const n = ++saidN.current
    const bind = bindKey // (what becomes of it is of that process: another in the pane shows none of it)
    setSaid({ what, text: t(what === 'stop' ? 'convo.stopping' : 'convo.sendingNow'), busy: true, n, bind })
    const settle = (text: string) => setSaid((cur) => (cur?.n === n ? { what, text, busy: false, n, bind } : cur))
    api('POST', `/api/sessions/${sid}/conversation/${what === 'stop' ? 'interrupt' : 'send-now'}`, { pane: b.pane, pid: b.pid, start: b.start, session: b.session })
      .then(() => settle(t(what === 'stop' ? 'convo.stopped' : 'convo.sentNow')))
      .catch((e) => settle(refusal(e)))
  }
  const canPress = bound && working

  // What was sent, until the log has a user turn of its text after it (each
  // turn answers one); not seen in time, it says so. One the agent queued
  // shows in its queue instead.
  const norm = (s: string) => s.replace(/\s+/g, ' ').trim()
  const queued = useMemo(() => new Set((convo.queued ?? []).map((q) => norm(q.text))), [convo.queued])
  const queuedRef = useRef(queued)
  queuedRef.current = queued
  const sentN = useRef(0)
  useEffect(() => {
    if (convo.status !== 'ready' || !sent.some((p) => p.state !== 'sent' && p.bind === bindKey)) return
    const key = bindKey + ' ' + convo.gen
    let u = used.get(key)
    if (!u) {
      used.clear() // (only this reading's turns can confirm one)
      used.set(key, (u = new Set()))
    }
    const users = convo.entries.filter((e: ConvoEntry) => e.role === 'user')
    let changed = false
    const next = sent.flatMap((p) => {
      if (p.state === 'sent' || p.bind !== bindKey) return [p]
      if (p.gen !== convo.gen) {
        changed = true
        return [{ ...p, state: 'sent' as const }]
      }
      const hit = users.find((e) => e.seq > p.after && !u.has(e.seq) && afterSend(e, p.at) && sameTurn(e, p.text))
      if (!hit) return [p]
      u.add(hit.seq)
      if (u.size > 200) for (const q of [...u].sort((a, b) => a - b).slice(0, 100)) u.delete(q)
      changed = true
      return []
    })
    if (changed) setSent(() => next)
  }, [convo.entries, convo.gen, convo.status, sent, bindKey])
  useEffect(() => {
    const waiting = sent.filter((p) => p.state === 'waiting' && !queued.has(norm(p.text)))
    if (!waiting.length) return
    const due = Math.min(...waiting.map((p) => p.at)) + PENDING_MS - Date.now()
    const timer = setTimeout(() => {
      const late = (p: Sent) => p.state === 'waiting' && Date.now() - p.at >= PENDING_MS && !queuedRef.current.has(norm(p.text))
      setSent((ps) => (ps.some(late) ? ps.map((p) => (late(p) ? { ...p, state: 'unseen' as const } : p)) : ps))
    }, Math.max(0, due) + 50)
    return () => clearTimeout(timer)
  }, [sent, queued])
  const shownSent = sent.filter((p) => p.bind === bindKey && !(p.state !== 'sent' && queued.has(norm(p.text))))

  // The composer: its draft kept per agent (as the terminal's per session).
  const draftKey = 'agents ' + cur.key
  const draft = shownDraft ?? getDraft(draftKey)
  // What is typed or put in (a path, a completion, a long paste) is the
  // draft, kept; an entry of what was sent, shown while going through them,
  // is not: the draft put aside stays kept, and comes back.
  const histPos = useRef<string | null>(null)
  const histStash = useRef('')
  const setDraft = (s: string) => {
    histPos.current = null
    setDraftShown(() => s)
    keepDraft(draftKey, s)
  }
  const showSent = (h: string | null) => {
    if (h === null) return setDraft(histStash.current)
    if (histPos.current === null) histStash.current = draft
    histPos.current = h
    setDraftShown(() => h)
    keepDraft(draftKey, histStash.current)
  }
  useEffect(
    () => () => {
      // (Another agent shown: this one's draft is as it was put aside.)
      if (histPos.current !== null) setDraftShown(() => histStash.current)
      histPos.current = null
    },
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [],
  )
  const field = useRef<HTMLTextAreaElement>(null)
  const completion = useCompletion({ sessionId: sid, field, draft, setDraft, agent: row?.a.agent ?? 'claude' })
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
    for (const f of files) {
      if (!alive.current) break
      const abort = new AbortController()
      upAbort.current = abort
      const name = f.name || 'image'
      setUp({ name, p: 0, abort })
      try {
        const r = await uploadFile(sid, f, (p) => setUp((u) => (u?.abort === abort ? { ...u, p } : u)), abort.signal)
        setUp((u) => (u?.abort === abort ? null : u))
        const quoted = r.prefix === '@' ? geminiPath(r.path) : shellPath(r.path)
        if (!alive.current) break
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
    const text = pos < h.length ? h[pos] : histStash.current
    requestAnimationFrame(() => {
      const el = field.current
      if (el) el.selectionStart = el.selectionEnd = dir < 0 ? 0 : text.length
    })
    return true
  }
  useEffect(() => {
    const el = field.current
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
  }, [draft])
  // Why it cannot send now, if it cannot.
  const blocked = !bound ? t('agents.cannotSend') : agent?.other ? t('convo.otherAgent', { agent: agent.other }) : null
  const send = () => {
    const b = convo.bind
    if (sending || !draft.trim()) return
    if (blocked || !b) return toast(blocked ?? t('agents.cannotSend'), true)
    if (up) return toast(t('term.waitUpload'), true) // (its path is not in the draft yet)
    const text = expandPastes(draft, 100 << 10)
    if (text === null || new TextEncoder().encode(text).length > 100 << 10) return toast(t('agents.tooLong'), true)
    const was = draft // (what is typed meanwhile is a new draft: it stays)
    setSending(true)
    const after = convo.entries.length ? convo.entries[convo.entries.length - 1].seq : 0
    const at = Date.now()
    api('POST', `/api/sessions/${sid}/conversation/send`, { pane: b.pane, pid: b.pid, start: b.start, session: b.session, text })
      .then(() => {
        remember(was)
        if (histPos.current === was) showSent(null) // (an entry sent again: the draft put aside is back)
        else if (getDraft(draftKey) === was) {
          if (histPos.current !== null) histStash.current = '' // (put aside while it went: it went)
          keepDraft(draftKey, '')
          setDraftShown((d) => (d === undefined || d === was ? '' : d))
        }
        setSent((ps) => [...ps, { id: Date.now() * 100 + (++sentN.current % 100), text, state: 'waiting' as const, bind: bindKey, after, gen: convo.gen, at }].slice(-5))
      })
      .catch((e) => toast(e instanceof ApiError && e.code === 'unknown' ? t('agents.sendUnknown') : refusal(e), true)) // (the draft stays)
      .finally(() => {
        setSending(false)
        field.current?.focus() // (null once another agent is shown)
      })
  }
  const enterKey = isMac ? 'Return' : 'Enter'
  const modEnter = isMac ? '⌘Return' : 'Ctrl+Enter'
  const sendKey = composerEnter === 'send' ? enterKey : modEnter
  const to = agent ? [agent.title, agent.where].filter((x) => x && x !== '~').join(' · ') : ''
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
      <div className="composer-box">
        <textarea
          ref={field}
          className="composer-input"
          value={draft}
          aria-label={t('term.composer')}
          autoCapitalize="off"
          autoCorrect="off"
          spellCheck={false}
          placeholder={to ? t('convo.toPlaceholder', { name: to }) : t('term.composerPlaceholder')}
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
          {blocked && <span className="composer-keys agents-chat-why">{blocked}</span>}
          {!blocked && hasKeyboardPointer() && (
            <span className="composer-keys" aria-hidden="true">
              {composerEnter === 'send' ? t('term.composerKeysSend', { send: enterKey, newline: isMac ? 'Shift+Return' : 'Shift+Enter' }) : t('term.composerKeysNewline', { send: modEnter })}
            </span>
          )}
          {canPress && !draft.trim() ? (
            <button type="button" className="composer-send is-stop" onClick={() => press('stop')} aria-label={t('convo.stop')} title={t('convo.stop')}>
              <Square size={14} fill="currentColor" strokeWidth={0} />
            </button>
          ) : (
            <button type="button" className="composer-send" onClick={send} disabled={sending || !!blocked} aria-label={t('term.send')} title={t('term.send') + (isTouch ? '' : ' (' + sendKey + ')')}>
              <ArrowUp size={18} strokeWidth={2.25} />
            </button>
          )}
        </div>
      </div>
    </div>
  )

  const openTerminal = () => navigate(`/t/${sid}` + (cur.pane ? '?pane=' + encodeURIComponent(cur.pane) : ''))
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
        waits && row ? (
          // (One a pane: nothing of another's carries over. Not tied to a terminal: no size to wait for.)
          <InlineReply key={row.a.pane_id} sessionId={sid} agent={row.a} resizedAt={0} active={active} held={() => false} />
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
      onSendNow={canPress ? () => press('now') : undefined}
      sendNow={said?.what === 'now' && said.bind === bindKey ? said.text : undefined}
      lead={lead}
    />
    </div>
  )
}

// A phone's list of the agents: waiting for you first, then working, then the
// rest, the latest to change first in each, as it was when the list showed
// (statuses change in place; rows do not move under the finger; one new comes
// last). Its summary line puts them in order again and goes to the first
// waiting; "only mine" keeps those waiting, and those done since.
function PhoneList({ entries, all, titles, onPick }: { entries: Entry[]; all: SessionInfo[]; titles: Map<string, string>; onPick: (e: Entry) => void }) {
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
  const rank = (e: Entry) => ['blocked', 'working'].indexOf(e.row?.status ?? '') + 1 || 3
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
  const regroup = () => {
    setOrder(sorted(entries))
    requestAnimationFrame(() => box.current?.scrollTo({ top: 0, behavior: matchMedia('(prefers-reduced-motion: reduce)').matches ? 'auto' : 'smooth' }))
  }
  const shown = mine ? list.filter((e) => e.row?.status === 'blocked' || e.row?.status === 'done') : list
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
      {shown.length === 0 && <p className="agents-list-none">{t('agents.noneMine')}</p>}
      <ul className="agents-list-rows">
        {shown.map((e) => {
          const r = e.row
          const status = r?.lost ? 'unknown' : (r?.status ?? 'unknown')
          const title = titles.get(e.key) || (r ? paneTitle(r.a.title) || r.a.agent : `${e.s.host_name} · ${sessionWhich(e.s, all)}`)
          const where = r ? [r.s.host_name, r.ws && r.ws !== '~' ? r.ws : '', isClaude(r.a.agent) ? '' : r.a.agent].filter(Boolean).join(' · ') : e.s.host_name + ' · tmux'
          const since = r && !r.lost ? when({ since: r.a.since, status }, now) : ''
          return (
            <li key={e.key + ' ' + e.s.id}>
              <button type="button" className={'agents-list-row is-' + status} onClick={() => onPick(e)}>
                <span className={'agents-mark is-' + status} aria-hidden="true">
                  {status === 'working' ? <span className="convo-spin is-small" /> : null}
                </span>
                <span className="agents-list-text">
                  <span className="agents-list-title">{title}</span>
                  <span className="agents-list-where">{where}</span>
                </span>
                <span className={'agents-list-side is-' + status}>
                  {status === 'blocked' ? t('convo.status.blocked') : status === 'done' ? t('agents.unseen') : since || (status === 'working' ? t('convo.status.working') : '')}
                </span>
              </button>
            </li>
          )
        })}
      </ul>
    </div>
  )
}
