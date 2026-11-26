import { useState, type FormEvent } from 'react'
import { CornerDownRight, GitBranch, RotateCcw, TextCursorInput } from 'lucide-react'
import { api, ApiError, errorText, type SessionInfo } from '../api'
import { t, type I18nKey } from '../i18n'
import { Dialog } from '../components/ui'
import { expectSession, renamedHerdr } from './agents'

// What the conversation page does to an agent's place rather than to its
// conversation: a copy of the conversation started in a workspace of its
// own (fork), and the names herdr shows (its workspace, tab, pane).

// Forking: of all of it, or of it up to the end of one turn (at: the next
// message the person typed, cut before; Claude Code's only). The copy is
// started as a new agent is (the gateway's apiNewAgent, fork): in the
// folder the conversation began in, its first message waiting in its
// composer (not on the agent's command line, where it could be taken for
// one of its options).
// A Codex's copy up to before the person's message at: how many of their
// messages from it on its transcript goes back over, its words, whether
// another begins alike, whether it can be taken whole to the page's input.
export type CodexRewind = { back: number; text: string; alike: boolean; clear: boolean; at: string }

export function ForkDialog({
  sessionId,
  hostId,
  kind,
  source,
  at,
  rewind,
  rewindNow,
  dir,
  name,
  check,
  onDone,
  onClose,
}: {
  sessionId: string
  hostId: number
  kind: 'claude' | 'codex'
  source: string
  at?: string
  rewind?: CodexRewind // (a Codex's copy, rewound to before the person's message back-th from the last, its words text)
  rewindNow?: () => CodexRewind | undefined // (asked again as it is sent: the conversation may have gone on meanwhile)
  dir: string
  name: string
  check?: () => string // (asked again as it is sent: why not now, or '')
  onDone: (pane: string, first: string, opened?: SessionInfo, rewound?: boolean, named?: boolean, rw?: CodexRewind) => void // (rw: as counted when sent) // (opened: from a tmux session, the terminal opened on the copy's own; rewound: false where a Codex's rewind was not seen to end there)
  onClose: () => void
}) {
  const [label, setLabel] = useState(() => t('fork.defaultName', { name }).slice(0, 40))
  // (A Codex's copy up to a turn: the message after it, put back here to change and send.)
  const [first, setFirst] = useState(() => (rewind?.clear ? rewind.text : ''))
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  // (One key for one thing asked: said again unchanged, the same copy.)
  const keyFor = useState(() => ({ sig: '', key: '' }))[0]
  const keyOf = (sig: string) => {
    if (keyFor.sig !== sig) {
      keyFor.sig = sig
      keyFor.key = crypto.randomUUID()
    }
    return keyFor.key
  }
  const submit = async (e?: FormEvent) => {
    e?.preventDefault()
    if (busy) return
    const no = check?.()
    if (no) return setError(no)
    setBusy(true)
    setError('')
    try {
      // (Counted again now: a turn gone on meanwhile moves where it goes back to — review fork2-8.)
      const rw = rewind ? rewindNow?.() : undefined
      if (rewind && !rw) {
        setError(t('fork.atGone'))
        setBusy(false)
        return
      }
      const body = { dir, kind, label: label.trim(), resume: source, fork: true, at: at ?? '', ...(rw ? { back: rw.back, at_text: rw.text, clear: rw.clear } : {}) }
      const r = await api<{ pane?: string; tmux?: string; rewound?: boolean; named?: boolean }>('POST', `/api/sessions/${sessionId}/agents`, { ...body, key: keyOf(JSON.stringify(body)) }, { timeoutMs: 90000 })
      if (r.tmux) {
        // (From a tmux session: the copy in a tmux session of its own, a terminal opened on it here — as a new agent's.)
        let opened: SessionInfo
        try {
          opened = await api<SessionInfo>('POST', '/api/sessions', { host_id: hostId, cols: 160, rows: 50, open: { kind: 'tmux', name: r.tmux } })
        } catch (e) {
          setError(t('newAgent.tmuxNotOpened', { name: r.tmux, why: errorText(e) }))
          setBusy(false)
          return
        }
        onDone('', first.trim(), opened, r.rewound, r.named, rw)
        return
      }
      onDone(r.pane ?? '', first.trim(), undefined, r.rewound, r.named, rw)
    } catch (err) {
      if (err instanceof ApiError && err.code === 'no_session') setError(t('fork.noSession'))
      else if (err instanceof ApiError && err.code === 'bad_message') setError(t('fork.badMessage'))
      else if (err instanceof ApiError && err.code === 'start_failed') setError(t('newAgent.failed', { why: err.message }) + (err.extra.closed === false ? ' ' + t('newAgent.notClosed') : ''))
      else if (err instanceof ApiError && (err.code === 'busy' || err.code === 'in_progress')) setError(t('newAgent.busy'))
      else if (err instanceof ApiError && err.code === 'unknown') setError(t('newAgent.unknown') + (typeof err.extra.workspace === 'string' && err.extra.workspace ? ' ' + t('newAgent.which', { w: err.extra.workspace }) : ''))
      else setError(errorText(err))
      setBusy(false)
    }
  }
  const close = () => !busy && onClose()
  return (
    <Dialog title={t(at || rewind ? 'fork.titleAt' : 'fork.title')} onClose={close} icon={<GitBranch size={18} />}>
      <form className="form" onSubmit={submit}>
        <p className="lead">{t(rewind ? 'fork.bodyAtCodex' : at ? 'fork.bodyAt' : 'fork.body')}</p>
        <p className="hint">{t('fork.shared')}</p>
        <label className="field">
          <span>{t('newAgent.label')}</span>
          <input className="input" value={label} maxLength={40} autoComplete="off" onChange={(e) => setLabel(e.target.value)} />
        </label>
        <label className="field">
          <span>{t('fork.first')}</span>
          <textarea className="input" rows={3} value={first} placeholder={t('fork.firstHint')} onChange={(e) => setFirst(e.target.value)} />
        </label>
        {error && <p className="err">{error}</p>}
        <div className="dialog-actions">
          <button type="button" className="btn" onClick={close} disabled={busy}>
            {t('common.cancel')}
          </button>
          <button type="submit" className="btn btn-primary" disabled={busy}>
            {t(busy ? 'fork.busy' : 'fork.confirm')}
          </button>
        </div>
      </form>
    </Dialog>
  )
}

// The names herdr shows: its workspace's and its tab's (a name each must
// have), the pane's own (none: herdr's own again). Only what was changed is
// sent, each by the id it had when this was opened.
export type RenameTarget = {
  sessionId: string
  ws: { id: string; label: string }
  tab?: { id: string; label: string }
  pane: { id: string; label: string }
}

export function RenameDialog({ target, onClose }: { target: RenameTarget; onClose: () => void }) {
  const [ws, setWs] = useState(target.ws.label === '~' ? '' : target.ws.label)
  const [tab, setTab] = useState(target.tab?.label ?? '')
  const [pane, setPane] = useState(target.pane.label)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  const clean = (s: string) => s.replace(/\s+/g, ' ').trim()
  const changes: { action: string; target: string; label: string }[] = []
  if (clean(ws) !== clean(target.ws.label === '~' ? '' : target.ws.label)) changes.push({ action: 'rename_workspace', target: target.ws.id, label: clean(ws) })
  if (target.tab && clean(tab) !== clean(target.tab.label)) changes.push({ action: 'rename_tab', target: target.tab.id, label: clean(tab) })
  if (clean(pane) !== clean(target.pane.label)) changes.push({ action: 'rename_pane', target: target.pane.id, label: clean(pane) })
  // (A name required only of what is changed: a pane named alone, its
  // workspace's unnamed as it was — review585-12.)
  const ready = changes.length > 0 && changes.every((c) => c.action === 'rename_pane' || !!c.label)
  const submit = async (e?: FormEvent) => {
    e?.preventDefault()
    if (!ready || busy) return
    setBusy(true)
    setError('')
    // (One after another: herdr answers one request per connection, and a
    // name taken is not undone if a later one fails — what failed is said.)
    const show = (done: typeof changes) => {
      const got = (action: string) => done.find((c) => c.action === action)?.label
      const w = got('rename_workspace'), tb = got('rename_tab'), p = got('rename_pane')
      if (w === undefined && tb === undefined && p === undefined) return
      // (The names given, shown at once in the list and the title: herdr's own list says them a few seconds later.)
      expectSession(
        target.sessionId,
        (s) => renamedHerdr(s, w !== undefined ? { id: target.ws.id, label: w } : undefined, tb !== undefined && target.tab ? { id: target.tab.id, label: tb } : undefined, p !== undefined ? { id: target.pane.id, label: p } : undefined),
        (s) =>
          s.meta?.t === 'herdr' &&
          s.meta.workspaces.some((x) => x.id === target.ws.id && (w === undefined || x.label === w) && x.agents.some((a) => a.pane_id === target.pane.id && (p === undefined || (a.label ?? '') === p) && (tb === undefined || a.tab_label === tb))),
      )
    }
    const done: typeof changes = []
    for (const c of changes) {
      try {
        await api('POST', `/api/sessions/${target.sessionId}/herdr`, c)
        done.push(c)
      } catch (err) {
        show(done) // (what was named before it failed: so named)
        setError(t('rename.failed', { what: t(`rename.${c.action}` as 'rename.rename_workspace'), why: err instanceof ApiError && err.code === 'herdr_busy' ? t('herdr.busy') : errorText(err) }))
        setBusy(false)
        return
      }
    }
    show(done)
    onClose()
  }
  const close = () => !busy && onClose()
  return (
    <Dialog title={t('rename.title')} onClose={close} icon={<TextCursorInput size={18} />}>
      <form className="form" onSubmit={submit}>
        <label className="field">
          <span>{t('rename.workspace')}</span>
          <input className="input" value={ws} maxLength={40} autoFocus autoComplete="off" onChange={(e) => setWs(e.target.value)} />
        </label>
        {target.tab && (
          <label className="field">
            <span>{t('rename.tab')}</span>
            <input className="input" value={tab} maxLength={40} autoComplete="off" onChange={(e) => setTab(e.target.value)} />
          </label>
        )}
        <label className="field">
          <span>{t('rename.pane')}</span>
          <input className="input" value={pane} maxLength={40} autoComplete="off" placeholder={t('rename.paneHint')} onChange={(e) => setPane(e.target.value)} />
        </label>
        <p className="hint">{t('rename.hint')}</p>
        {error && <p className="err">{error}</p>}
        <div className="dialog-actions">
          <button type="button" className="btn" onClick={close} disabled={busy}>
            {t('common.cancel')}
          </button>
          <button type="submit" className="btn btn-primary" disabled={busy || !ready}>
            {t('rename.confirm')}
          </button>
        </div>
      </form>
    </Dialog>
  )
}

// Linking an agent under another by hand, where Corral did not see the one start the other (doc item 47, 11-25: a
// subagent started by a shell script Corral will not read as surely starting it). The gateway checks both panes are
// still who they were and refuses a cycle; a link made so is kept as the person's (source "manual").
export interface LinkPane {
  key: string
  title: string
  where: string
  pane: string
  term: string
  session: string
}
export function LinkParentDialog({ sessionId, child, parents, onClose, onLinked }: { sessionId: string; child: LinkPane; parents: LinkPane[]; onClose: () => void; onLinked: (parent: LinkPane) => void }) {
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  const pick = async (p: LinkPane) => {
    if (busy) return
    setBusy(true)
    setError('')
    try {
      await api('POST', `/api/sessions/${encodeURIComponent(sessionId)}/herdr/lineage`, {
        action: 'link',
        child_pane: child.pane,
        child_term_id: child.term,
        child_session: child.session,
        parent_pane: p.pane,
        parent_term_id: p.term,
        parent_session: p.session,
      })
      onLinked(p)
      onClose()
    } catch (err) {
      setError(errorText(err))
      setBusy(false)
    }
  }
  return (
    <Dialog title={t('convo.lineage.linkTitle', { name: child.title })} onClose={onClose} busy={busy} sub={t('convo.lineage.linkSub')}>
      {parents.length === 0 ? (
        <p className="hint">{t('convo.lineage.linkNone')}</p>
      ) : (
        <ul className="pick-list">
          {parents.map((p) => (
            <li key={p.key}>
              <button type="button" className="pick-item" disabled={busy} onClick={() => void pick(p)}>
                <span className="pick-icon">
                  <CornerDownRight size={16} />
                </span>
                <span className="pick-main">
                  <span className="pick-title">{p.title}</span>
                  {p.where && p.where !== p.title && <span className="pick-sub">{p.where}</span>}
                </span>
              </button>
            </li>
          ))}
        </ul>
      )}
      {error && (
        <p className="err" role="alert">
          {error}
        </p>
      )}
    </Dialog>
  )
}

export interface RewindTarget {
  sessionId: string
  pane: string
  terminalId?: string
  pid: number
  start: number
  session: string
  messageId: string
  text: string
}

const REWIND_ERR_KEYS: Record<string, I18nKey> = {
  terminal_changed: 'rewind.err.terminal_changed',
  process_changed: 'rewind.err.process_changed',
  session_changed: 'rewind.err.session_changed',
  not_in_front: 'rewind.err.not_in_front',
  agent_busy: 'rewind.err.agent_busy',
  input_not_empty: 'rewind.err.input_not_empty',
  ambiguous_message: 'rewind.err.ambiguous_message',
  message_not_found: 'rewind.err.message_not_found',
  message_too_long: 'rewind.err.message_too_long',
  message_text_unavailable: 'rewind.err.message_text_unavailable',
  not_ready: 'rewind.notReady',
  picker_not_opened: 'rewind.err.picker_not_opened',
  target_mismatch: 'rewind.err.target_mismatch',
  confirm_mismatch: 'rewind.err.confirm_mismatch',
  option_not_found: 'rewind.err.option_not_found',
  panel_not_closed: 'rewind.err.panel_not_closed',
  rewind_uncertain: 'rewind.err.rewind_uncertain',
  busy: 'rewind.err.busy',
  unsupported: 'rewind.err.unsupported',
}

export function RewindDialog({
  target,
  onDone,
  onRefresh,
  onClose,
}: {
  target: RewindTarget
  onDone: (text: string, warning?: string) => void
  onRefresh: () => void
  onClose: () => void
}) {
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  const submit = async (e?: FormEvent) => {
    e?.preventDefault()
    if (busy) return
    setBusy(true)
    setError('')
    try {
      const r = await api<{ ok: boolean; text?: string; warning?: string }>(
        'POST',
        `/api/sessions/${encodeURIComponent(target.sessionId)}/conversation/rewind`,
        {
          pane: target.pane,
          terminal_id: target.terminalId ?? '',
          pid: target.pid,
          start: target.start,
          session: target.session,
          message_id: target.messageId,
        },
        { timeoutMs: 60000 },
      )
      onDone(r.text ?? target.text, r.warning)
    } catch (err) {
      if (err instanceof ApiError) {
        if (err.code === 'terminal_changed' || err.code === 'process_changed' || err.code === 'session_changed') {
          onRefresh()
        }
        const key = REWIND_ERR_KEYS[err.code]
        setError(key ? t(key) : errorText(err))
      } else {
        setError(errorText(err))
      }
      setBusy(false)
    }
  }
  const close = () => !busy && onClose()
  return (
    <Dialog title={t('rewind.confirmTitle')} onClose={close} busy={busy} icon={<RotateCcw size={18} />}>
      <form className="form" onSubmit={submit}>
        <p className="lead">{t('rewind.confirmBody')}</p>
        {error && (
          <p className="err" role="alert">
            {error}
          </p>
        )}
        <div className="dialog-actions">
          <button type="button" className="btn btn-ghost" disabled={busy} onClick={close}>
            {t('common.cancel')}
          </button>
          <button type="submit" className="btn btn-primary" disabled={busy}>
            {t(busy ? 'rewind.busy' : 'rewind.confirmTitle')}
          </button>
        </div>
      </form>
    </Dialog>
  )
}
