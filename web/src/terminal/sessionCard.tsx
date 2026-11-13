// The session card: what the conversation's agent works in and on, at one
// look — its folder and git (read when the card opens, never while shut:
// GET …/git, then …/git/pr), what changed, what runs in the background, its
// tasks, its context and model. Over the thread from a button in its bar on
// a wide screen; from the bar's ⋮ menu, as a sheet from the bottom, on a
// narrow one. Groups with nothing to say are not shown; values not known are
// not shown either (no "0%" for an unknown window).
//
// Nothing here changes the repository: committing or opening a pull request
// is asked of the agent, in words put in the composer for the person to send
// (the agent knows where its work stands; a commit from here could take a
// half-made change, or meet the agent's own git).
import { useEffect, useLayoutEffect, useRef, useState, type ReactNode } from 'react'
import { createPortal } from 'react-dom'
import { Bot, FolderOpen, GitBranch, GitCommitHorizontal, GitPullRequest, Gauge, ListTodo, MessageSquareText, Terminal } from 'lucide-react'
import { api, ApiError } from '../api'
import { t } from '../i18n'
import { Dialog, DialogHead } from '../components/ui'
import { duration } from './convoTime'
import type { ConvoTask } from './conversation'

type GitInfo = {
  repo: boolean
  reason?: string
  dir?: string
  root?: string
  branch?: string
  detached?: boolean
  upstream?: string
  ahead?: number
  behind?: number
  status: boolean
  changed: number
  untracked: number
  last?: { hash: string; time: number; subject: string }
  base?: string
  base_ahead?: number
  diff?: { files: number; add: number; del: number }
}
type GitPR = { gh: boolean; pr: { number: number; state: string; title: string; url: string; draft: boolean } | null }

export interface CardFile {
  path: string
  add: number
  del: number
  key: string // (the answer whose changes panel shows it)
}

export interface CardData {
  sid: string
  pane: string
  // What the agent's log says it changed, of what is loaded of it.
  edits: { files: CardFile[]; add: number; del: number } | null
  running: { id: string; kind: string; what: string; ts?: string }[]
  tasks: ConvoTask[] | null
  ctx: { used: number; window?: number } | null
  model?: string
  effort?: string
  mode?: string
}

export interface CardActs {
  onFolder?: (dir: string) => void // (the file panel, there)
  onChanges: (key: string, path?: string) => void
  onRunning: (id: string) => void
  onDraft?: (text: string) => void // (words put in the composer, not sent)
}

const tokens = (n: number) => (n >= 1e6 ? (n / 1e6).toFixed(n >= 1e7 ? 0 : 1) + 'M' : n >= 1e3 ? Math.round(n / 1e3) + 'K' : String(n))
const shortDir = (p: string) => {
  const parts = p.split('/').filter(Boolean)
  return parts.length > 3 ? '…/' + parts.slice(-3).join('/') : p
}
const baseName = (p: string) => p.slice(p.lastIndexOf('/') + 1)
const FILES_SHOWN = 5

function useGit(sid: string, pane: string) {
  const [git, setGit] = useState<GitInfo | null>(null)
  const [pr, setPR] = useState<GitPR | null>(null)
  const [error, setError] = useState('')
  const [again, setAgain] = useState(0)
  useEffect(() => {
    const ctl = new AbortController()
    const q = pane ? '?pane=' + encodeURIComponent(pane) : ''
    setError('')
    api<GitInfo>('GET', `/api/sessions/${sid}/git${q}`, undefined, { signal: ctl.signal, timeoutMs: 20000 }).then(
      (g) => {
        if (ctl.signal.aborted) return
        setGit(g)
        // (Its pull request only of a branch of a repository: gh asked apart, a network call of the host's.)
        if (g.repo && g.branch)
          api<GitPR>('GET', `/api/sessions/${sid}/git/pr${q}`, undefined, { signal: ctl.signal, timeoutMs: 20000 }).then(
            (p) => !ctl.signal.aborted && setPR(p),
            () => {},
          )
      },
      (e) => !ctl.signal.aborted && setError(e instanceof ApiError && e.code === 'busy' ? t('card.gitBusy') : t('card.gitFailed')),
    )
    return () => ctl.abort()
  }, [sid, pane, again])
  return { git, pr, error, retry: () => setAgain((n) => n + 1) }
}

function Row({ icon, children, sub, onClick, title }: { icon: ReactNode; children: ReactNode; sub?: ReactNode; onClick?: () => void; title?: string }) {
  const body = (
    <>
      <span className="scard-ic" aria-hidden="true">{icon}</span>
      <span className="scard-text">
        <span className="scard-main">{children}</span>
        {sub && <span className="scard-sub">{sub}</span>}
      </span>
    </>
  )
  return onClick ? (
    <button type="button" className="scard-row is-act" onClick={onClick} title={title}>
      {body}
    </button>
  ) : (
    <div className="scard-row" title={title}>
      {body}
    </div>
  )
}

function Counts({ add, del }: { add: number; del: number }) {
  return (
    <span className="scard-counts">
      <span className="scard-add">+{add}</span> <span className="scard-del">−{del}</span>
    </span>
  )
}

function CardBody({ data, acts, close }: { data: CardData; acts: CardActs; close: () => void }) {
  const { git, pr, error, retry } = useGit(data.sid, data.pane)
  const now = Date.now()
  const ago = (ms: number) => t('card.ago', { d: duration(Math.max(now - ms, 0)) })
  const act = (f: () => void) => () => {
    close()
    f()
  }
  const files = data.edits?.files ?? []
  const done = data.tasks?.filter((x) => x.status === 'completed').length ?? 0
  const current = data.tasks?.find((x) => x.status === 'in_progress')
  return (
    <>
      <section className="scard-sec" aria-label={t('card.git')}>
        {!git && !error && <p className="scard-note">{t('card.loading')}</p>}
        {error && (
          <p className="scard-note">
            {error}{' '}
            <button type="button" className="link-btn" onClick={retry}>
              {t('common.retry')}
            </button>
          </p>
        )}
        {git && !git.repo && (
          <>
            {git.dir && (
              <Row icon={<FolderOpen size={16} />} onClick={acts.onFolder ? act(() => acts.onFolder!(git.dir!)) : undefined} title={git.dir}>
                {shortDir(git.dir)}
              </Row>
            )}
            <p className="scard-note">{t(git.reason === 'nogit' ? 'card.noGit' : git.reason === 'oldgit' ? 'card.oldGit' : git.reason === 'nodir' ? 'card.noDir' : 'card.noRepo')}</p>
          </>
        )}
        {git?.repo && (
          <>
            <Row icon={<FolderOpen size={16} />} onClick={acts.onFolder && git.root ? act(() => acts.onFolder!(git.root!)) : undefined} title={git.root}>
              {shortDir(git.root ?? '')}
            </Row>
            <Row
              icon={<GitBranch size={16} />}
              sub={
                !git.status
                  ? t('card.statusSlow')
                  : git.changed || git.untracked
                    ? [git.changed ? t('card.uncommitted', { n: git.changed }) : '', git.untracked ? t('card.untracked', { n: git.untracked }) : ''].filter(Boolean).join(t('convo.sep'))
                    : t('card.clean')
              }
            >
              <span className="scard-strong">{git.detached ? t('card.detached') : git.branch}</span>
              {git.upstream ? (
                git.ahead || git.behind ? (
                  <span className="scard-dim">
                    {' '}
                    {[git.ahead ? t('card.ahead', { n: git.ahead }) : '', git.behind ? t('card.behind', { n: git.behind }) : ''].filter(Boolean).join(' ')} {git.upstream}
                  </span>
                ) : (
                  <span className="scard-dim"> {t('card.synced', { u: git.upstream })}</span>
                )
              ) : (
                !git.detached && <span className="scard-dim"> {t('card.noUpstream')}</span>
              )}
            </Row>
            {git.last && (
              <Row icon={<GitCommitHorizontal size={16} />} sub={<span className="scard-mono">{git.last.hash} · {ago(git.last.time * 1000)}</span>} title={git.last.subject}>
                <span className="scard-one">{git.last.subject}</span>
              </Row>
            )}
            {pr?.pr && (
              <a className="scard-row is-act" href={pr.pr.url} target="_blank" rel="noreferrer" title={pr.pr.url}>
                <span className="scard-ic" aria-hidden="true">
                  <GitPullRequest size={16} />
                </span>
                <span className="scard-text">
                  <span className="scard-main">
                    <span className="scard-strong">#{pr.pr.number}</span> <span className="scard-one">{pr.pr.title}</span>
                  </span>
                  <span className="scard-sub">{t(pr.pr.draft ? 'card.prDraft' : pr.pr.state === 'MERGED' ? 'card.prMerged' : pr.pr.state === 'CLOSED' ? 'card.prClosed' : 'card.prOpen')}</span>
                </span>
              </a>
            )}
          </>
        )}
      </section>

      {(git?.diff || files.length > 0) && (
        <section className="scard-sec">
          <h3 className="scard-head">{t('card.changes')}</h3>
          {git?.diff && git.base && (
            <div className="scard-kv">
              <span>{t('card.vsBase', { base: git.base })}</span>
              <span>
                {t('card.filesN', { n: git.diff.files })} <Counts add={git.diff.add} del={git.diff.del} />
                {git.base_ahead ? <span className="scard-dim"> · {t('card.commitsN', { n: git.base_ahead })}</span> : null}
              </span>
              <span className="scard-hint">{t('card.vsBaseHint')}</span>
            </div>
          )}
          {data.edits && files.length > 0 && (
            <>
              <div className="scard-kv">
                <span>{t('card.byLog')}</span>
                <span>
                  {t('card.filesN', { n: files.length })} <Counts add={data.edits.add} del={data.edits.del} />
                </span>
                <span className="scard-hint">{t('card.byLogHint')}</span>
              </div>
              <ul className="scard-files">
                {files.slice(0, FILES_SHOWN).map((f) => (
                  <li key={f.path}>
                    <button type="button" className="scard-file" onClick={act(() => acts.onChanges(f.key, f.path))} title={f.path}>
                      <span className="scard-file-name">{baseName(f.path)}</span>
                      <Counts add={f.add} del={f.del} />
                    </button>
                  </li>
                ))}
                {files.length > FILES_SHOWN && <li className="scard-dim">{t('card.moreFiles', { n: files.length - FILES_SHOWN })}</li>}
              </ul>
            </>
          )}
        </section>
      )}

      {data.running.length > 0 && (
        <section className="scard-sec">
          <h3 className="scard-head">{t('card.running')}</h3>
          {data.running.map((r) => (
            <Row key={r.id} icon={r.kind === 'agent' ? <Bot size={16} /> : <Terminal size={16} />} onClick={act(() => acts.onRunning(r.id))} sub={r.ts ? t('convo.bgFor', { for: duration(Math.max(now - Date.parse(r.ts), 0)) }) : undefined}>
              <span className="scard-one">{r.what || t(r.kind === 'agent' ? 'convo.bgAgent' : 'convo.bgCmd')}</span>
            </Row>
          ))}
        </section>
      )}

      {data.tasks && data.tasks.length > 0 && (
        <section className="scard-sec">
          <h3 className="scard-head">
            {t('card.tasks')} <span className="scard-dim">{done} / {data.tasks.length}</span>
          </h3>
          {current && (
            <Row icon={<ListTodo size={16} />}>
              <span className="scard-one">{current.subject}</span>
            </Row>
          )}
        </section>
      )}

      {(data.ctx || data.model) && (
        <section className="scard-sec">
          <h3 className="scard-head">{t('card.context')}</h3>
          <Row icon={<Gauge size={16} />} sub={[data.model, data.effort && t('card.effort', { e: data.effort }), data.mode].filter(Boolean).join(t('convo.sep'))}>
            {data.ctx ? (
              data.ctx.window ? (
                <>
                  <span className="scard-strong">{Math.round((data.ctx.used / data.ctx.window) * 100)}%</span> <span className="scard-dim">{tokens(data.ctx.used)} / {tokens(data.ctx.window)}</span>
                </>
              ) : (
                <span className="scard-strong">{tokens(data.ctx.used)} tokens</span>
              )
            ) : (
              <span className="scard-dim">{t('card.ctxUnknown')}</span>
            )}
          </Row>
        </section>
      )}

      {acts.onDraft && git?.repo && (
        <section className="scard-sec scard-asks">
          {(git.changed > 0 || git.untracked > 0) && (
            <button type="button" className="btn" onClick={act(() => acts.onDraft!(t('card.commitText')))} title={t('card.draftHint')}>
              <MessageSquareText size={15} /> {t('card.askCommit')}
            </button>
          )}
          {pr && pr.gh && !pr.pr && git.branch && git.base && git.branch !== git.base.replace(/^origin\//, '') && (
            <button type="button" className="btn" onClick={act(() => acts.onDraft!(t('card.prText')))} title={t('card.draftHint')}>
              <GitPullRequest size={15} /> {t('card.askPR')}
            </button>
          )}
          <p className="scard-hint">{t('card.draftHint')}</p>
        </section>
      )}
    </>
  )
}

// The card itself: anchored under its button on a wide screen (anchor: the
// button), a sheet from the bottom otherwise.
export function SessionCard({ data, acts, anchor, sheet, onClose }: { data: CardData; acts: CardActs; anchor: HTMLElement | null; sheet: boolean; onClose: () => void }) {
  const box = useRef<HTMLDivElement>(null)
  const [at, setAt] = useState<{ top: number; right: number; maxHeight: number } | null>(null)
  useLayoutEffect(() => {
    if (sheet || !anchor) return
    const place = () => {
      const r = anchor.getBoundingClientRect()
      const vv = window.visualViewport
      const h = vv ? vv.height : window.innerHeight
      setAt({ top: r.bottom + 8, right: Math.max(8, window.innerWidth - r.right), maxHeight: Math.max(160, h - r.bottom - 24) })
    }
    place()
    window.addEventListener('resize', place)
    return () => window.removeEventListener('resize', place)
  }, [anchor, sheet])
  // Shut by Escape or a press outside (not on its own button: that toggles it); the focus back to the button.
  useEffect(() => {
    if (sheet) return
    const away = (e: PointerEvent) => {
      const n = e.target as Node
      if (!box.current?.contains(n) && !anchor?.contains(n)) onClose()
    }
    const onKey = (e: KeyboardEvent) => {
      if (e.key !== 'Escape') return
      e.preventDefault()
      e.stopPropagation()
      onClose()
      anchor?.focus()
    }
    document.addEventListener('pointerdown', away, true)
    document.addEventListener('keydown', onKey, true)
    box.current?.focus({ preventScroll: true })
    return () => {
      document.removeEventListener('pointerdown', away, true)
      document.removeEventListener('keydown', onKey, true)
    }
  }, [sheet, anchor, onClose])
  if (sheet)
    return (
      <Dialog title={t('card.title')} onClose={onClose} hideTitle className="dlg scard-dialog">
        <DialogHead title={t('card.title')} onClose={onClose} />
        <div className="dlg-body scard">
          <CardBody data={data} acts={acts} close={onClose} />
        </div>
      </Dialog>
    )
  if (!at) return null
  return createPortal(
    <div ref={box} className="scard scard-pop" role="dialog" aria-label={t('card.title')} tabIndex={-1} style={{ top: at.top, right: at.right, maxHeight: at.maxHeight }}>
      <CardBody data={data} acts={acts} close={onClose} />
    </div>,
    document.body,
  )
}
