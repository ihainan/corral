import { useEffect, useMemo, useRef, useState, type ReactNode } from 'react'
import { ArrowUp, ChevronLeft, Square } from 'lucide-react'
import { api, ApiError, errorText, type SessionInfo } from '../api'
import { t } from '../i18n'
import { navigate } from '../router'
import { setChromeColor, termScheme, usePrefs } from '../prefs'
import { local } from '../storage'
import { useToast } from '../components/ui'
import { chromeVars } from '../terminal/themes'
import { expandPastes, getDraft, keepDraft, keepPaste, remember } from '../terminal/drafts'
import { useCompletion } from '../terminal/Completion'
import { InlineReply } from '../terminal/ReplyDialog'
import { ConversationView, type ConvoAgent, type ConvoListRow, type ConvoPending } from '../terminal/Conversation'
import { asking, useConversation, type ConvoEntry } from '../terminal/conversation'
import { isClaude, listAgents, paneTitle, useRunningSessions, type AgentRow } from '../terminal/agents'
import { afterSend, holdTermMode, PENDING_MS, sameTurn, tmuxKey } from '../terminal/chatSend'
import { sessionWhich } from '../terminal/sessionName'

// The agents page: every agent of the sessions the gateway has open, and the
// conversation of the one picked, with a composer that sends to it. It
// talks to each agent's pane directly (POST …/conversation/send, interrupt,
// send-now): what herdr has in front, or a terminal tab shows, is not
// changed. With tabs it is a tab of its own (the terminals stay open behind
// it); on a phone a page of its own.

const LAST = 'corral.agentsPick'
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
  const picked = pick
    ? entries.find((e) => e.s.id === pick.s && (e.pane ?? '') === (pick.pane ?? '')) ??
      (pick.pane === undefined ? entries.find((e) => e.s.id === pick.s && e.s.meta?.t === 'herdr' && e.pane === e.s.meta.focused_pane) ?? entries.find((e) => e.s.id === pick.s) : undefined)
    : undefined
  // One picked that is not in the list for a moment, as herdr's summary of
  // its session is not there or not current, stays shown as last seen; one
  // a current summary no longer has (its pane closed) does not.
  const lastCur = useRef<Entry | undefined>(undefined)
  const pickedSession = pick ? sessions?.find((x) => x.id === pick.s) : undefined
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
    const q = new URLSearchParams({ s: e.s.id })
    if (e.pane) q.set('pane', e.pane)
    navigate('/agents?' + q, true)
  }
  // Shown without being picked (none was, its session ended, an address
  // named one gone): it is the pick from now on, so the list's order
  // changing does not move the view. One shown is remembered, however picked.
  useEffect(() => {
    if (!cur) return
    if (!(pick && pick.s === cur.s.id && (pick.pane ?? '') === (cur.pane ?? ''))) return choose(cur)
    try {
      local.set(LAST, JSON.stringify({ s: cur.s.id, pane: cur.pane }))
    } catch {
      /* not kept */
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [cur?.key, cur?.s.id, pick?.s, pick?.pane])
  // Back and forward between agents: the address says which.
  useEffect(() => {
    const onPop = () => location.pathname === '/agents' && setPick(readPick())
    window.addEventListener('popstate', onPop)
    return () => window.removeEventListener('popstate', onPop)
  }, [])
  // What was sent to each agent and is not in its log yet, and whether a
  // message to it is on its way: kept here, so going to another agent and
  // back loses neither (a second send of the same could follow).
  // So are its draft as shown, and the log's turns that confirmed one.
  const [sentBy, setSentBy] = useState<Record<string, Sent[]>>({})
  const [sendingBy, setSendingBy] = useState<Record<string, boolean>>({})
  const [draftBy, setDraftBy] = useState<Record<string, string>>({})
  const usedBy = useRef(new Map<string, Map<string, Set<number>>>())
  // (Of sessions that ended: forgotten. A draft stays in drafts.ts's keeping.)
  useEffect(() => {
    if (!sessions) return
    const live = (k: string) => sessions.some((x) => k.endsWith(' ' + x.id))
    const prune = <T,>(all: Record<string, T>) => (Object.keys(all).every(live) ? all : Object.fromEntries(Object.entries(all).filter(([k]) => live(k))))
    setSentBy(prune)
    setSendingBy(prune)
    setDraftBy(prune)
    for (const k of usedBy.current.keys()) if (!live(k)) usedBy.current.delete(k)
  }, [sessions])

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
  if (!cur)
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
    const k = cur.key + ' ' + cur.s.id
    body = (
      <AgentChat
        key={k}
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
      />
    )
  }

  return (
    <div className={'term-root agents-chat ' + (scheme.dark ? 'dark' : 'light') + (active ? '' : ' inactive')} inert={!active} aria-hidden={active ? undefined : true} style={chromeVars(scheme) as React.CSSProperties} ref={rootRef}>
      {bar ?? (
        <header className="term-bar">
          <button type="button" className="icon-btn" onClick={() => navigate('/')} aria-label={t('common.back')} title={t('common.back')}>
            <ChevronLeft size={20} />
          </button>
          <span className="agents-chat-title">{t('nav.agents')}</span>
        </header>
      )}
      <div className="term-body">{body}</div>
    </div>
  )
}

type Sent = ConvoPending & { bind: string; after: number; gen: number; at: number }

// The conversation of one agent, and what is sent to it. Keyed by the
// agent: nothing of another's (its draft, what was sent, a stop) carries over.
function AgentChat({ cur, entries, all, titles, onPick, active, toast, composerEnter, sent, setSent, sending, setSending, draft: shownDraft, setDraftShown, used }: {
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
  const setDraft = (s: string) => {
    setDraftShown(() => s)
    keepDraft(draftKey, s)
  }
  const field = useRef<HTMLTextAreaElement>(null)
  const completion = useCompletion({ sessionId: sid, field, draft, setDraft, agent: row?.a.agent ?? 'claude' })
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
    const text = expandPastes(draft, 100 << 10)
    if (text === null || new TextEncoder().encode(text).length > 100 << 10) return toast(t('agents.tooLong'), true)
    const was = draft // (what is typed meanwhile is a new draft: it stays)
    setSending(true)
    const after = convo.entries.length ? convo.entries[convo.entries.length - 1].seq : 0
    const at = Date.now()
    api('POST', `/api/sessions/${sid}/conversation/send`, { pane: b.pane, pid: b.pid, start: b.start, session: b.session, text })
      .then(() => {
        remember(was)
        if (getDraft(draftKey) === was) {
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
          onChange={(e) => setDraft(e.target.value)}
          onSelect={() => completion.update()}
          {...completion.field}
          onBlur={() => setTimeout(completion.update)}
          onPaste={(e) => {
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
            if (e.key === 'Enter' && (e.metaKey || e.ctrlKey)) {
              e.preventDefault()
              send()
            } else if (e.key === 'Enter' && hasKeyboardPointer() && composerEnter === 'send' && !e.shiftKey && !e.altKey) {
              e.preventDefault()
              send()
            }
          }}
        />
        <div className="composer-tools">
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
  return (
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
      onStop={canPress ? () => press('stop') : undefined}
      stop={said?.what === 'stop' && said.bind === bindKey ? said.text : undefined}
      side={{ rows }}
      active={active}
      onDismiss={(id) => setSent((ps) => ps.filter((p) => p.id !== id))}
      onShowTerminal={openTerminal}
      onCopied={() => toast(t('common.copied'))}
      composer={composer}
      onSendNow={canPress ? () => press('now') : undefined}
      sendNow={said?.what === 'now' && said.bind === bindKey ? said.text : undefined}
    />
  )
}
