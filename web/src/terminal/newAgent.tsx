import { useEffect, useMemo, useRef, useState, type FormEvent, type KeyboardEvent } from 'react'
import { Bot, Folder, GitFork, History, Search, X } from 'lucide-react'
import { api, ApiError, errorText, type SessionInfo } from '../api'
import { t } from '../i18n'
import { Dialog } from '../components/ui'
import { local } from '../storage'
import { AgentGlyph } from './agentIcons'

// A new agent from the conversation page: from a herdr session, a workspace
// of its own in a folder, the agent started there; from a tmux session, a
// tmux session of its own, a terminal opened on it here (or a conversation
// of before searched across all folders and resumed, or copied); its first
// message, if one is written, waits in its composer. See the gateway's apiNewAgent.

export const NEW_AGENT = 'corral:new-agent' // (asked from the list: the page opens this)
// Asked from elsewhere (the command palette), the agents' page perhaps not
// there yet: kept until it is, and takes it as it comes.
let newAgentAsked = false
export function askNewAgent() {
  newAgentAsked = true
  window.dispatchEvent(new Event(NEW_AGENT))
}
export function takeNewAgentAsk(): boolean {
  const v = newAgentAsked
  newAgentAsked = false
  return v
}

export interface NewSubagentParent {
  sessionId: string
  pane: string
  title: string
  dir: string
  ws?: string
}

type Kind = 'claude' | 'codex' | 'opencode' | 'agy'
type Placement = 'split' | 'workspace'
type TabMode = 'new' | 'resume'
type OldSession = { id: string; kind: Kind; dir: string; modified: number; title?: string; first?: string }

const RECENT = 'corral.newAgentDirs'
const recentDirs = (): string[] => {
  try {
    const v = JSON.parse(local.get(RECENT) ?? '[]')
    return Array.isArray(v) ? v.filter((x): x is string => typeof x === 'string').slice(0, 10) : []
  } catch {
    return []
  }
}
const keepDir = (d: string) => {
  try {
    local.set(RECENT, JSON.stringify([d, ...recentDirs().filter((x) => x !== d)].slice(0, 10)))
  } catch {
    // (not kept: no matter)
  }
}

// The kind last started from here (this device's).
const KIND = 'corral.newAgentKind'
const lastKind = (): Kind => {
  const v = local.get(KIND)
  return v === 'codex' || v === 'opencode' || v === 'agy' ? v : 'claude'
}

const SUB_PLACEMENT = 'corral.subagentPlacement'
const lastPlacement = (): Placement => (local.get(SUB_PLACEMENT) === 'workspace' ? 'workspace' : 'split')

const herdrOf = (s: SessionInfo) => (s.persistence === 'herdr' && s.meta?.t === 'herdr' && !s.meta.lost ? s.meta : null)

function shortDir(p: string): string {
  const s = p.replace(/^\/(?:home|Users)\/[^/]+/, '~').replace(/\/+$/, '') || '/'
  if (s.length <= 38) return s
  const parts = s.split('/').filter(Boolean)
  return parts.length >= 2 ? '…/' + parts.slice(-2).join('/') : s
}

function relativeTime(ms: number): string {
  if (!ms) return ''
  const diff = Math.max(0, Date.now() - ms)
  const mins = Math.floor(diff / 60000)
  if (mins < 1) return t('newAgent.justNow')
  if (mins < 60) return t('newAgent.minsAgo', { n: mins })
  const hrs = Math.floor(mins / 60)
  if (hrs < 24) return t('newAgent.hoursAgo', { n: hrs })
  const days = Math.floor(hrs / 24)
  if (days < 7) return t('newAgent.daysAgo', { n: days })
  const d = new Date(ms)
  return `${d.getMonth() + 1}/${d.getDate()}`
}

function matchSessionTokens(s: OldSession, q: string): boolean {
  const toks = q.trim().toLowerCase().split(/\s+/).filter(Boolean)
  if (!toks.length) return true
  const hay = `${s.kind} ${s.dir} ${s.title ?? ''} ${s.first ?? ''} ${s.id}`.toLowerCase()
  return toks.every((tok) => hay.includes(tok))
}

export function NewAgentDialog({
  sessions,
  parent,
  onDone,
  onOpen,
  onClose,
}: {
  sessions: SessionInfo[]
  parent?: NewSubagentParent
  onDone: (s: SessionInfo, pane: string, first: string, parent?: NewSubagentParent) => void
  onOpen: (s: SessionInfo, pane: string) => void
  onClose: () => void
}) {
  // (Where one may be started: herdr's sessions, and tmux's — locked to parent's herdr session for subagents.)
  const herdrs = parent ? sessions.filter((s) => s.id === parent.sessionId && herdrOf(s)) : sessions.filter((s) => herdrOf(s) || s.persistence === 'tmux')
  const [sid, setSid] = useState(parent?.sessionId ?? herdrs[0]?.id ?? '')
  const sess = herdrs.find((s) => s.id === sid)
  // (Folders the agents there work in, then the ones started from here lately.)
  const dirs = useMemo(() => {
    const m = sess ? herdrOf(sess) : null
    const seen = (m?.workspaces ?? []).flatMap((w) => w.agents.map((a) => a.cwd)).filter(Boolean)
    return [...new Set([...(parent?.dir ? [parent.dir] : []), ...seen, ...recentDirs()])].slice(0, 12)
  }, [sess, parent?.dir])

  const [tab, setTab] = useState<TabMode>('new')
  const [dir, setDir] = useState(() => parent?.dir || dirs[0] || '~/')
  const [kind, setKind] = useState<Kind>(lastKind)
  const [placement, setPlacement] = useState<Placement>(lastPlacement)
  // In a git worktree of its own: Herdr sessions support native worktree.create
  // for all 4 agent kinds (with optional custom branch), while tmux supports
  // Claude Code (-w) and Codex (--worktree).
  const [worktree, setWorktree] = useState(false)
  const [worktreeBranch, setWorktreeBranch] = useState('')
  const [label, setLabel] = useState('')
  const [first, setFirst] = useState('')

  // Search & resume closed conversations state (all folders & all 4 agent kinds).
  const [searchQ, setSearchQ] = useState('')
  const [kindFilter, setKindFilter] = useState<'all' | Kind>('all')
  const [scope, setScope] = useState<'all' | 'folder'>('all')
  const [closedOnly, setClosedOnly] = useState(true)
  const [pool, setPool] = useState<OldSession[] | null>(null)
  const [poolBusy, setPoolBusy] = useState(false)
  const [picked, setPicked] = useState<OldSession | null>(null)
  const [fork, setFork] = useState(false)
  const asked = useRef(0)
  const searchInput = useRef<HTMLInputElement>(null)

  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')

  const keyFor = useState(() => ({ sig: '', key: '' }))[0]
  const keyOf = (sig: string) => {
    if (keyFor.sig !== sig) {
      keyFor.sig = sig
      keyFor.key = crypto.randomUUID()
    }
    return keyFor.key
  }

  // All currently running agent session IDs across all open Herdr sessions
  // (so "closed only" accurately hides sessions already open anywhere, and
  // clicking a running session jumps straight to its pane).
  const running = useMemo(() => {
    const out = new Map<string, { s: SessionInfo; pane: string }>()
    for (const s of sessions) {
      const m = herdrOf(s)
      if (!m) continue
      for (const w of m.workspaces ?? []) {
        for (const a of w.agents) {
          if (a.session && !out.has(a.session)) out.set(a.session, { s, pane: a.pane_id })
        }
      }
    }
    return out
  }, [sessions])

  // Load / search closed sessions whenever the Resume tab is open or filters change.
  useEffect(() => {
    if (tab !== 'resume' || !sess) return
    const n = ++asked.current
    const delay = searchQ.trim() ? 240 : 0
    const timer = window.setTimeout(async () => {
      setPoolBusy(true)
      setError('')
      try {
        const params = new URLSearchParams()
        if (searchQ.trim()) params.set('q', searchQ.trim())
        if (kindFilter !== 'all') params.set('kind', kindFilter)
        if (scope === 'folder' && dir.trim()) params.set('dir', dir.trim())
        const qs = params.toString()
        const r = await api<{ sessions: OldSession[] }>('GET', `/api/sessions/${sess.id}/agent-sessions${qs ? '?' + qs : ''}`)
        if (n === asked.current) setPool(r.sessions ?? [])
      } catch (e) {
        if (n === asked.current) setError(e instanceof ApiError && e.code === 'bad_folder' ? t('newAgent.badFolder') : errorText(e))
      } finally {
        if (n === asked.current) setPoolBusy(false)
      }
    }, delay)
    return () => window.clearTimeout(timer)
  }, [tab, sess, searchQ, kindFilter, scope, dir])

  useEffect(() => {
    if (tab === 'resume') {
      requestAnimationFrame(() => searchInput.current?.focus())
    }
  }, [tab])

  const visibleOlds = useMemo(() => {
    if (!pool) return []
    return pool.filter((o) => {
      if (kindFilter !== 'all' && o.kind !== kindFilter) return false
      if (closedOnly && running.has(o.id)) return false
      return matchSessionTokens(o, searchQ)
    })
  }, [pool, kindFilter, closedOnly, running, searchQ])

  // Keep `picked` pointing at a visible row or clear it if filtered out.
  useEffect(() => {
    if (tab !== 'resume') return
    if (picked && !visibleOlds.some((o) => o.id === picked.id && o.kind === picked.kind)) {
      setPicked(visibleOlds[0] ?? null)
    } else if (!picked && visibleOlds.length > 0) {
      setPicked(visibleOlds[0])
    }
  }, [tab, visibleOlds, picked])

  const activeKind: Kind = tab === 'resume' && picked ? picked.kind : kind
  const activeDir = tab === 'resume' && picked ? picked.dir : dir.trim()
  const activeResume = tab === 'resume' && picked ? picked.id : ''
  const canFork = activeKind === 'claude' || activeKind === 'codex'
  const copying = fork && canFork && !!activeResume
  const isHerdrSess = !!sess && !!herdrOf(sess)
  const canWorktree = tab === 'new' && !(parent && placement === 'split') && (isHerdrSess || (kind !== 'opencode' && kind !== 'agy'))
  const inWorktree = worktree && canWorktree

  if (!herdrs.length) {
    return (
      <Dialog title={t(parent ? 'newAgent.subagentTitle' : 'newAgent.title')} onClose={onClose} icon={parent ? <GitFork size={18} /> : <Bot size={18} />}>
        <p className="lead">{t('newAgent.noHerdr')}</p>
        <div className="dialog-actions">
          <button className="btn" onClick={onClose}>
            {t('common.close')}
          </button>
        </div>
      </Dialog>
    )
  }

  const startSession = async (override?: OldSession) => {
    if (!sess || busy) return
    const target = override ?? (tab === 'resume' ? picked : null)
    if (tab === 'resume' && !target) return
    if (target) {
      const open = running.get(target.id)
      if (open && !(fork && (target.kind === 'claude' || target.kind === 'codex'))) {
        onOpen(open.s, open.pane)
        return
      }
    }
    const useDir = target ? target.dir : dir.trim()
    const useKind: Kind = target ? target.kind : kind
    const useResume = target ? target.id : ''
    const useFork = !!(target && fork && (useKind === 'claude' || useKind === 'codex'))
    if (!useDir) return

    setBusy(true)
    setError('')
    try {
      const body = {
        dir: useDir,
        kind: useKind,
        label: label.trim(),
        resume: useResume,
        fork: useFork,
        worktree: !useResume && inWorktree,
        ...(!useResume && inWorktree && isHerdrSess && worktreeBranch.trim() ? { worktree_branch: worktreeBranch.trim() } : {}),
        ...(parent ? { parent_pane: parent.pane, placement } : {}),
      }
      const r = await api<{ pane?: string; tmux?: string }>('POST', `/api/sessions/${sess.id}/agents`, { ...body, key: keyOf(sess.id + '\n' + JSON.stringify(body)) }, { timeoutMs: 90000 })
      keepDir(useDir)
      local.set(KIND, useKind)
      if (parent) local.set(SUB_PLACEMENT, placement)
      const said = !useResume || useFork ? first.trim() : ''
      if (r.tmux) {
        let opened: SessionInfo
        try {
          opened = await api<SessionInfo>('POST', '/api/sessions', { host_id: sess.host_id, cols: 160, rows: 50, open: { kind: 'tmux', name: r.tmux } })
        } catch (e) {
          setError(t('newAgent.tmuxNotOpened', { name: r.tmux, why: errorText(e) }))
          setBusy(false)
          return
        }
        onDone(opened, '', said, parent)
        return
      }
      onDone(sess, r.pane ?? '', said, parent)
    } catch (err) {
      if (err instanceof ApiError && err.code === 'bad_folder') setError(t('newAgent.badFolder'))
      else if (err instanceof ApiError && err.code === 'no_session') setError(t('fork.noSession'))
      else if (err instanceof ApiError && err.code === 'start_failed') {
        const notClosedKey = parent && placement === 'split' ? 'newAgent.notClosedPane' : 'newAgent.notClosed'
        setError(t('newAgent.failed', { why: err.message }) + (err.extra.closed === false ? ' ' + t(notClosedKey) : ''))
      } else if (err instanceof ApiError && (err.code === 'busy' || err.code === 'in_progress')) setError(t('newAgent.busy'))
      else setError(t('newAgent.unknown') + (err instanceof ApiError && typeof err.extra.workspace === 'string' && err.extra.workspace ? ' ' + t('newAgent.which', { w: err.extra.workspace }) : ''))
      setBusy(false)
    }
  }

  const submit = (e?: FormEvent) => {
    e?.preventDefault()
    void startSession()
  }

  const onSearchKey = (e: KeyboardEvent<HTMLInputElement>) => {
    if (!visibleOlds.length) return
    if (e.key === 'ArrowDown' || e.key === 'ArrowUp') {
      e.preventDefault()
      const idx = picked ? visibleOlds.findIndex((o) => o.id === picked.id && o.kind === picked.kind) : -1
      const next = e.key === 'ArrowDown' ? (idx + 1) % visibleOlds.length : (idx <= 0 ? visibleOlds.length - 1 : idx - 1)
      setPicked(visibleOlds[next])
    } else if (e.key === 'Enter' && picked) {
      e.preventDefault()
      void startSession(picked)
    }
  }

  const close = () => !busy && onClose()
  const pickedRunning = picked ? running.get(picked.id) : undefined

  return (
    <Dialog title={t(parent ? 'newAgent.subagentTitle' : 'newAgent.title')} onClose={close} className="new-agent-dialog" icon={parent ? <GitFork size={18} /> : tab === 'resume' ? <History size={18} /> : <Bot size={18} />}>
      <form className="new-agent" onSubmit={submit}>
        {parent && (
          <p className="new-agent-parent">
            <GitFork size={14} aria-hidden="true" />
            <span>{t('newAgent.subagentUnder', { name: parent.title })}</span>
          </p>
        )}
        {!parent && (
          <div className="seg" role="tablist" aria-label={t('newAgent.title')}>
            <button type="button" role="tab" aria-selected={tab === 'new'} aria-pressed={tab === 'new'} onClick={() => setTab('new')}>
              {t('newAgent.tab.new')}
            </button>
            <button type="button" role="tab" aria-selected={tab === 'resume'} aria-pressed={tab === 'resume'} onClick={() => setTab('resume')}>
              {t('newAgent.tab.resume')}
            </button>
          </div>
        )}
        {!parent && herdrs.length > 1 && (
          <label className="field">
            <span>{t('newAgent.where')}</span>
            <select
              className="input"
              value={sid}
              onChange={(e) => {
                asked.current++
                setSid(e.target.value)
                setPool(null)
                setPicked(null)
              }}
            >
              {herdrs.map((s) => (
                <option key={s.id} value={s.id}>
                  {s.host_name + (s.mux_session ? ' · ' + s.mux_session : '') + (s.persistence === 'tmux' ? ' · tmux' : '')}
                </option>
              ))}
            </select>
          </label>
        )}
        {parent && (
          <div className="field">
            <span>{t('newAgent.placement')}</span>
            <div className="seg" role="radiogroup" aria-label={t('newAgent.placement')}>
              {(['split', 'workspace'] as const).map((p) => (
                <button key={p} type="button" role="radio" aria-checked={placement === p} aria-pressed={placement === p} onClick={() => setPlacement(p)}>
                  {t(`newAgent.placement.${p}`)}
                </button>
              ))}
            </div>
            <p className="hint">{t(`newAgent.placementHint.${placement}`)}</p>
          </div>
        )}

        {tab === 'new' ? (
          <>
            <label className="field">
              <span>{t('newAgent.dir')}</span>
              <input
                className="input"
                value={dir}
                list="new-agent-dirs"
                autoCapitalize="none"
                autoCorrect="off"
                spellCheck={false}
                onChange={(e) => setDir(e.target.value)}
              />
              <datalist id="new-agent-dirs">
                {dirs.map((d) => (
                  <option key={d} value={d} />
                ))}
              </datalist>
            </label>
            <div className="field">
              <span>{t('newAgent.kind')}</span>
              <div className="seg seg-marks" role="radiogroup">
                {(['claude', 'codex', 'opencode', 'agy'] as const).map((k) => (
                  <button key={k} type="button" role="radio" aria-checked={kind === k} aria-pressed={kind === k} onClick={() => setKind(k)}>
                    <AgentGlyph program={k} />
                    {t(`newAgent.kind.${k}`)}
                  </button>
                ))}
              </div>
            </div>
            {canWorktree && (
              <div className="field">
                <label className="check">
                  <input type="checkbox" checked={worktree} onChange={(e) => setWorktree(e.target.checked)} />
                  <span>{t('newAgent.worktree')}</span>
                </label>
                <p className="hint">{t(isHerdrSess && (kind === 'agy' || kind === 'opencode' || !!worktreeBranch.trim()) ? 'newAgent.worktreeHintHerdr' : 'newAgent.worktreeHint', { agent: t(`newAgent.kind.${kind}`) })}</p>
                {inWorktree && isHerdrSess && (
                  <label className="field new-agent-wt-branch">
                    <span>{t('newAgent.worktreeBranch')}</span>
                    <input
                      className="input"
                      value={worktreeBranch}
                      maxLength={120}
                      autoCapitalize="none"
                      autoCorrect="off"
                      spellCheck={false}
                      placeholder={t('newAgent.worktreeBranchHint')}
                      onChange={(e) => setWorktreeBranch(e.target.value)}
                    />
                  </label>
                )}
              </div>
            )}
            <label className="field">
              <span>{t('newAgent.label')}</span>
              <input className="input" value={label} maxLength={40} placeholder={t(parent && placement === 'split' ? 'newAgent.labelHintSplit' : 'newAgent.labelHint')} onChange={(e) => setLabel(e.target.value)} />
            </label>
            {!parent && (
              <div className="new-agent-old">
                <button
                  type="button"
                  className="link-btn"
                  onClick={() => {
                    setKindFilter('all')
                    setScope('all')
                    setTab('resume')
                  }}
                >
                  {t('newAgent.resumeShortcut')}
                </button>
              </div>
            )}
            <label className="field">
              <span>{t('newAgent.first')}</span>
              <textarea className="input" rows={3} value={first} placeholder={t('newAgent.firstHint')} onChange={(e) => setFirst(e.target.value)} />
            </label>
          </>
        ) : (
          <div className="new-agent-resume">
            <div className="new-agent-search">
              <Search size={15} className="new-agent-search-ic" aria-hidden="true" />
              <input
                ref={searchInput}
                type="text"
                value={searchQ}
                placeholder={t('newAgent.searchPlaceholder')}
                autoCapitalize="none"
                autoCorrect="off"
                spellCheck={false}
                onChange={(e) => setSearchQ(e.target.value)}
                onKeyDown={onSearchKey}
              />
              {searchQ && (
                <button type="button" className="icon-btn new-agent-search-clear" onClick={() => setSearchQ('')} aria-label={t('common.close')}>
                  <X size={14} />
                </button>
              )}
            </div>
            <div className="new-agent-filters">
              <div className="new-agent-chips" role="group" aria-label={t('newAgent.kind')}>
                {(['all', 'claude', 'codex', 'opencode', 'agy'] as const).map((k) => (
                  <button key={k} type="button" className={'new-agent-chip' + (kindFilter === k ? ' is-on' : '')} onClick={() => setKindFilter(k)}>
                    {k !== 'all' && <AgentGlyph program={k} />}
                    <span>{k === 'all' ? t('newAgent.filterAll') : t(`newAgent.kind.${k}`)}</span>
                  </button>
                ))}
              </div>
              <div className="new-agent-subfilters">
                <button type="button" className={'new-agent-chip' + (scope === 'all' ? ' is-on' : '')} onClick={() => setScope(scope === 'all' ? 'folder' : 'all')} title={scope === 'folder' ? dir : undefined}>
                  <Folder size={12} aria-hidden="true" />
                  <span>{t(scope === 'all' ? 'newAgent.scopeAll' : 'newAgent.scopeFolder')}</span>
                </button>
                <label className="new-agent-closed-check">
                  <input type="checkbox" checked={closedOnly} onChange={(e) => setClosedOnly(e.target.checked)} />
                  <span>{t('newAgent.closedOnly')}</span>
                </label>
              </div>
            </div>
            <div className="new-agent-olds is-search" role="listbox" aria-label={t('newAgent.tab.resume')}>
              {poolBusy && pool === null ? (
                <p className="hint new-agent-empty">{t('common.loading')}</p>
              ) : visibleOlds.length === 0 ? (
                <p className="hint new-agent-empty">{t(searchQ.trim() ? 'newAgent.noMatch' : 'newAgent.noClosed')}</p>
              ) : (
                visibleOlds.map((o) => {
                  const open = running.get(o.id)
                  const selected = picked?.id === o.id && picked?.kind === o.kind
                  const title = o.title || o.first || o.id.slice(0, 8)
                  const snippet = o.title && o.first && o.first !== o.title ? o.first : ''
                  return (
                    <button
                      key={o.kind + ':' + o.id}
                      type="button"
                      role="option"
                      aria-selected={selected}
                      className={'new-agent-resrow' + (selected ? ' on' : '')}
                      onClick={() => setPicked(o)}
                      onDoubleClick={() => void startSession(o)}
                    >
                      <span className="new-agent-res-ic">
                        <AgentGlyph program={o.kind} />
                      </span>
                      <span className="new-agent-res-body">
                        <span className="new-agent-res-top">
                          <span className="new-agent-oldtitle">{title}</span>
                          <span className="new-agent-oldwhen">
                            {open ? t(fork && (o.kind === 'claude' || o.kind === 'codex') ? 'newAgent.running' : 'newAgent.open') : relativeTime(o.modified)}
                          </span>
                        </span>
                        {/* (Its second line: how it began, its folder at the end — doc UI item 17, 11-26.) */}
                        <span className="new-agent-res-sub">
                          <span className="new-agent-res-snip">{snippet}</span>
                          <span className="new-agent-res-dir" title={o.dir}>
                            {shortDir(o.dir)}
                          </span>
                        </span>
                      </span>
                    </button>
                  )
                })
              )}
            </div>
            {picked && canFork && (
              <div className="field">
                <label className="check">
                  <input type="checkbox" checked={fork} onChange={(e) => setFork(e.target.checked)} />
                  <span>{t('newAgent.fork')}</span>
                </label>
                {fork && <p className="hint">{t('fork.shared')}</p>}
              </div>
            )}
            {picked && copying && (
              <label className="field">
                <span>{t('newAgent.first')}</span>
                <textarea className="input" rows={2} value={first} placeholder={t('newAgent.firstHint')} onChange={(e) => setFirst(e.target.value)} />
              </label>
            )}
          </div>
        )}

        {error && <p className="err">{error}</p>}
        <div className="dialog-actions">
          <button type="button" className="btn" onClick={close} disabled={busy}>
            {t('common.cancel')}
          </button>
          <button type="submit" className="btn btn-primary" disabled={busy || !activeDir || (tab === 'resume' && !picked)}>
            {t(
              busy
                ? 'newAgent.starting'
                : tab === 'resume' && pickedRunning && !copying
                  ? 'newAgent.openRunning'
                  : copying
                    ? 'newAgent.forkGo'
                    : activeResume
                      ? 'newAgent.resume'
                      : 'newAgent.start',
            )}
          </button>
        </div>
      </form>
    </Dialog>
  )
}
