// The Changes view: what the agent's folder has changed, file by file with
// each file's diff (GET …/git/changes, read on the host as the session
// card's git is: read only, the pane's own folder). Uncommitted — the
// working tree against HEAD, staged or not, with the files git does not
// track listed — or, where the card knows the remote's default branch,
// against it from where the branch left it (the card's "against" counts).
// Opened from those counts on the card. A file's diff folds: one too long
// starts folded, as do those past a first screenful or so of lines; one
// the host's cap left out is read alone on asking.
import { useEffect, useMemo, useRef, useState } from 'react'
import { ChevronRight, Eye, FolderOpen, GitCompareArrows, RotateCw } from 'lucide-react'
import { api, ApiError } from '../api'
import { t, type I18nKey } from '../i18n'
import { Dialog, DialogHead, HeadTool } from '../components/ui'
import { isDark, usePrefs } from '../prefs'
import type { ConvoHunk } from './conversation'
import { Hunks } from './hunks'

export type ChangesAgainst = 'work' | 'base'

type ChangedFile = {
  path: string
  from?: string
  status: string
  add: number
  del: number
  binary?: boolean
  hunks?: ConvoHunk[]
  cut?: boolean
  omitted?: boolean
}
type Changes = {
  repo: boolean
  reason?: string
  root?: string
  base?: string
  files: ChangedFile[]
  more?: boolean
  untracked?: string[]
  untracked_more?: boolean
}

// What can be done with a file of it, on its host: looked at, its folder
// shown (absolute paths, the repository's top before them).
export type ChangesActs = { view?: (path: string) => void; open?: (dir: string) => void }

// (A file of more lines than this starts folded; so do all after the first this many lines shown.)
const FOLD_LINES = 400
const OPEN_LINES = 2000
const STATUS = new Set(['A', 'M', 'D', 'R', 'C', 'T', 'U'])

const lineCount = (f: ChangedFile) => f.hunks?.reduce((n, h) => n + h.lines.length, 0) ?? 0

function reasonText(r: string, base?: string): string {
  switch (r) {
    case 'nodir':
      return t('card.noDir')
    case 'nogit':
      return t('card.noGit')
    case 'norepo':
      return t('card.noRepo')
    case 'oldgit':
      return t('card.oldGit')
    case 'notimeout':
      return t('gch.err.notimeout')
    case 'partial':
      return t('gch.err.partial')
    case 'nobase':
      return t('gch.err.nobase', { base: base ?? '' })
  }
  return t('gch.err.failed')
}
const failText = (e: unknown) => (e instanceof ApiError ? (e.code === 'busy' ? t('card.gitBusy') : e.code === 'timeout' ? t('gch.err.timeout') : t('gch.err.failed')) : t('gch.err.failed'))

function Counts({ add, del }: { add: number; del: number }) {
  return (
    <span className="gch-counts">
      <span className="gch-add">+{add}</span> <span className="gch-del">−{del}</span>
    </span>
  )
}

export function ChangesView({ sid, pane, base, start, acts, onClose }: { sid: string; pane: string; base?: string; start: ChangesAgainst; acts?: ChangesActs; onClose: () => void }) {
  usePrefs() // (its colours follow the theme)
  const [against, setAgainst] = useState<ChangesAgainst>(base ? start : 'work')
  const [read, setRead] = useState<Changes | null>(null)
  const [error, setError] = useState('')
  const [again, setAgain] = useState(0)
  // (Each file's fold, by path, once the person has opened or folded it; files read alone.
  // Maps, not objects: a file may be named "__proto__" — review, 11-15.)
  const [folded, setFolded] = useState<Map<string, boolean>>(() => new Map())
  const [alone, setAlone] = useState<Map<string, ChangedFile | 'loading' | string>>(() => new Map())
  const put = <V,>(set: (f: (m: Map<string, V>) => Map<string, V>) => void, k: string, v: V) => set((m) => new Map(m).set(k, v))
  // (A file read alone answers into the reading it was asked in, not a later one — another tab's.)
  const gen = useRef(0)
  useEffect(() => {
    gen.current++
    const ctl = new AbortController()
    setError('')
    setRead(null)
    setFolded(new Map())
    setAlone(new Map())
    api<Changes>('GET', `/api/sessions/${sid}/git/changes?` + new URLSearchParams({ pane, against }).toString(), undefined, { signal: ctl.signal, timeoutMs: 25000 }).then(
      (c) => !ctl.signal.aborted && setRead(c),
      (e) => !ctl.signal.aborted && setError(failText(e)),
    )
    return () => ctl.abort()
  }, [sid, pane, against, again])
  const files = useMemo(
    () =>
      (read?.files ?? []).map((f) => {
        const a = alone.get(f.path)
        return typeof a === 'object' ? a : f
      }),
    [read, alone],
  )
  // Which start folded: the long, and those past the first lines shown.
  const foldAtFirst = useMemo(() => {
    const out = new Map<string, boolean>()
    let shown = 0
    for (const f of read?.files ?? []) {
      const n = lineCount(f)
      const fold = n > FOLD_LINES || (n > 0 && shown + n > OPEN_LINES)
      out.set(f.path, fold)
      if (!fold) shown += n
    }
    return out
  }, [read])
  // (A renamed file asked for with the name it had, so git reads it as the
  // rename it is; only its diff taken from the answer — what it is and its
  // counts stay the list's: review, 11-15.)
  const loadAlone = (f0: ChangedFile) => {
    const n = gen.current
    const path = f0.path
    put(setAlone, path, 'loading')
    const q = new URLSearchParams({ pane, against, path })
    if (f0.from) q.set('from', f0.from)
    api<Changes>('GET', `/api/sessions/${sid}/git/changes?` + q.toString(), undefined, { timeoutMs: 25000 }).then(
      (c) => {
        if (n !== gen.current) return
        const f = c.files.find((x) => x.path === path)
        put(setAlone, path, f ? { ...f0, hunks: f.hunks, cut: f.cut, binary: f.binary, omitted: f.omitted } : t('gch.goneNow'))
        put(setFolded, path, false)
      },
      (e) => n === gen.current && put(setAlone, path, failText(e)),
    )
  }
  const add = files.reduce((n, f) => n + f.add, 0)
  const del = files.reduce((n, f) => n + f.del, 0)
  const root = read?.root ?? ''
  const full = (p: string) => root.replace(/\/+$/, '') + '/' + p
  const sub = read?.repo ? [root, t('card.filesN', { n: files.length }) + (read.more ? '+' : '')].join(' · ') : undefined
  return (
    <Dialog title={t('gch.title')} onClose={onClose} wide hideTitle className="dlg gch-dialog">
      <DialogHead
        icon={<GitCompareArrows size={18} />}
        title={t('gch.title')}
        oneLine
        sub={sub}
        subText={root}
        tools={
          <HeadTool label={t('preview.reload')} onClick={() => setAgain((n) => n + 1)}>
            <RotateCw size={16} />
          </HeadTool>
        }
        onClose={onClose}
      />
      <div className={'gch term-vars ' + (isDark() ? 'dark' : 'light')}>
        <div className="gch-bar">
          {base ? (
            <div className="gch-tabs" role="tablist" aria-label={t('gch.title')}>
              {(['work', 'base'] as const).map((a) => (
                <button key={a} type="button" role="tab" className="gch-tab" aria-selected={against === a} onClick={() => setAgainst(a)}>
                  {a === 'work' ? t('gch.work') : t('card.vsBase', { base })}
                </button>
              ))}
            </div>
          ) : (
            <span className="gch-what">{t('gch.work')}</span>
          )}
          {read?.repo && files.length > 0 && <Counts add={add} del={del} />}
        </div>
        <p className="gch-hint">{t(against === 'work' ? 'gch.workHint' : 'card.vsBaseHint')}</p>
        {error ? (
          <div className="gch-note" role="alert">
            <p>{error}</p>
            <button type="button" className="btn" onClick={() => setAgain((n) => n + 1)}>{t('common.retry')}</button>
          </div>
        ) : !read ? (
          <p className="gch-note">{t('common.loading')}</p>
        ) : !read.repo ? (
          <p className="gch-note">{reasonText(read.reason ?? '', base)}</p>
        ) : (
          <>
            {files.length === 0 && !read.untracked?.length && read.untracked_more && <p className="gch-note">{t('gch.untrackedUnread')}</p>}
            {files.length === 0 && !read.untracked?.length && !read.untracked_more && <p className="gch-note">{against === 'work' ? t('gch.none') : t('gch.noneBase', { base: read.base ?? base ?? '' })}</p>}
            {files.map((f) => {
              const st = alone.get(f.path)
              const open = !(folded.get(f.path) ?? foldAtFirst.get(f.path))
              const n = lineCount(f)
              const slash = f.path.lastIndexOf('/')
              const stKey = ('gch.st.' + (STATUS.has(f.status) ? f.status : 'M')) as I18nKey
              return (
                <section key={f.path} className="gch-file" data-status={f.status}>
                  <div className="gch-file-head">
                    <button type="button" className="gch-file-btn" aria-expanded={open} onClick={() => put(setFolded, f.path, open)} title={f.from ? f.from + ' → ' + f.path : f.path}>
                      <ChevronRight size={14} className="gch-chev" aria-hidden="true" />
                      <span className={'gch-st is-' + f.status} title={t(stKey)}>{STATUS.has(f.status) ? f.status : 'M'}</span>
                      <span className="gch-path">
                        {slash >= 0 && <span className="gch-dir">{f.path.slice(0, slash + 1)}</span>}
                        <span className="gch-name">{f.path.slice(slash + 1)}</span>
                        {f.from && <span className="gch-from">{t('gch.renamed', { from: f.from })}</span>}
                      </span>
                      {f.binary ? <span className="gch-bin">{t('gch.binaryTag')}</span> : <Counts add={f.add} del={f.del} />}
                    </button>
                    {acts?.view && f.status !== 'D' && (
                      <button type="button" className="icon-btn gch-file-tool" onClick={() => acts.view!(full(f.path))} aria-label={t('gch.open')} title={t('gch.open')}>
                        <Eye size={16} />
                      </button>
                    )}
                  </div>
                  {open &&
                    (f.binary ? (
                      <p className="gch-file-note">{t('gch.binary')}</p>
                    ) : (
                      <>
                        {!!f.hunks?.length && <Hunks hunks={f.hunks} className="is-whole" />}
                        {/* (Left out, or cut with others — lines of it or none, a change of type's second
                            half, say: said so whatever was read, and read alone on asking; asked again
                            after a failure — review, 11-15. Read alone and still cut: said, no more to ask.) */}
                        {f.omitted || f.cut ? (
                          <div className="gch-file-note">
                            <span>{typeof st === 'string' && st !== 'loading' ? st : t(f.omitted ? 'gch.omitted' : 'gch.cut')}</span>
                            {typeof st !== 'object' && (
                              <button type="button" className="btn" disabled={st === 'loading'} onClick={() => loadAlone(f)}>
                                {st === 'loading' ? t('common.loading') : t(st ? 'common.retry' : 'gch.load')}
                              </button>
                            )}
                          </div>
                        ) : (
                          !f.hunks?.length && <p className="gch-file-note">{t(f.from ? 'gch.movedOnly' : 'gch.noLines')}</p>
                        )}
                      </>
                    ))}
                  {!open && n > 0 && (
                    <button type="button" className="gch-file-note gch-unfold" onClick={() => put(setFolded, f.path, false)}>
                      {t('gch.folded', { n })}
                    </button>
                  )}
                </section>
              )
            })}
            {read.more && <p className="gch-note">{t('gch.more')}</p>}
            {(!!read.untracked?.length || (read.untracked_more && files.length > 0)) && (
              <section className="gch-untracked">
                <h3 className="gch-head">{t('gch.untracked', { n: (read.untracked?.length ?? 0) + (read.untracked_more ? '+' : '') })}</h3>
                <p className="gch-hint">{t('gch.untrackedHint')}</p>
                <ul>
                  {(read.untracked ?? []).map((p) => {
                    const dir = p.endsWith('/')
                    const go = dir ? acts?.open && (() => acts.open!(full(p).replace(/\/+$/, ''))) : acts?.view && (() => acts.view!(full(p)))
                    return (
                      <li key={p}>
                        {go ? (
                          <button type="button" className="gch-ut" onClick={go} title={full(p)}>
                            {dir ? <FolderOpen size={14} /> : <Eye size={14} />} <span className="gch-path">{p}</span>
                          </button>
                        ) : (
                          <span className="gch-ut">
                            <span className="gch-path">{p}</span>
                          </span>
                        )}
                      </li>
                    )
                  })}
                </ul>
                {read.untracked_more && <p className="gch-hint">{t('gch.untrackedMore')}</p>}
              </section>
            )}
          </>
        )}
      </div>
    </Dialog>
  )
}
