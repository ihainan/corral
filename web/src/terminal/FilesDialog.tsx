import { useEffect, useLayoutEffect, useRef, useState, type FormEvent } from 'react'
import { ArrowUp, Check, Clock, Copy, Download, Eye, FileQuestion, Folder, FolderPlus, House, Image as ImageIcon, LocateFixed, MoreHorizontal, Pencil, PencilLine, RotateCw, Search, SquareTerminal, Trash2, Upload, X } from 'lucide-react'
import { FileTypeIcon } from './fileIcons'
import { api, ApiError, errorText, postFile, type FileEntry, type FileListing } from '../api'
import { getLang, t } from '../i18n'
import { Confirm, Dialog, Field, LoadError, Menu, copyText, useToast, type MenuItem } from '../components/ui'
import { local } from '../storage'
import { downloadPath, PictureDialog } from './picture'
import { IMAGE_EXT, previewable, TextPreview } from './textPreview'

type Props = {
  sessionId: string
  start: string | null // where the panel was last time on this page
  reveal?: string // a file to show: its folder opened, it marked (over start)
  onPath: (p: string) => void
  onInsert: (path: string) => void // puts a path into the terminal (or the composer)
  onClose: () => void
}

type Upload = { key: number; name: string; progress: number; error?: string; abort: AbortController }

const HIDDEN_KEY = 'corral.files.hidden'

const join = (dir: string, name: string) => (dir === '/' ? '/' + name : dir + '/' + name)
// cleanPath spells a path the way the server answers with it (as Go's
// path.Clean does), so two spellings of one folder compare equal.
export function cleanPath(p: string): string {
  const out: string[] = []
  for (const part of p.split('/')) {
    if (part === '' || part === '.') continue
    if (part === '..') out.pop()
    else out.push(part)
  }
  return '/' + out.join('/')
}
const parent = (p: string) => (p === '/' ? '/' : p.slice(0, p.lastIndexOf('/')) || '/')

export function formatSize(n: number): string {
  if (n < 1024) return n + ' B'
  const units = ['KB', 'MB', 'GB', 'TB']
  let v = n / 1024
  let i = 0
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024
    i++
  }
  return (v < 10 ? v.toFixed(1) : Math.round(v)) + ' ' + units[i]
}

function formatTime(unix: number): string {
  const d = new Date(unix * 1000)
  const sameYear = d.getFullYear() === new Date().getFullYear()
  return new Intl.DateTimeFormat(getLang(), sameYear ? { month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit' } : { year: 'numeric', month: 'short', day: 'numeric' }).format(d)
}

const collator = new Intl.Collator(undefined, { numeric: true, sensitivity: 'base' })
// Folders first; then by name, or the newest first (byTime: "what was just
// made" — pages-1105 2.4).
const sortEntries = (list: FileEntry[], byTime = false) =>
  [...list].sort((a, b) =>
    (a.type === 'dir') !== (b.type === 'dir') ? (a.type === 'dir' ? -1 : 1) : (byTime ? b.mtime - a.mtime : 0) || collator.compare(a.name, b.name))
const ORDER_KEY = 'corral.files.byTime'
const FILTER_FROM = 8 // (entries in a folder: more than this, and the filter shows)

// Browse and change files on the session's host, over the terminal's own
// SSH connection.
export function FilesDialog({ sessionId, start, reveal, onPath, onInsert, onClose }: Props) {
  const toast = useToast()
  const base = `/api/sessions/${sessionId}/files`
  const [path, setPath] = useState<string | null>(null)
  const [listing, setListing] = useState<FileListing | null>(null)
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState('')
  const [hidden, setHidden] = useState(() => local.get(HIDDEN_KEY) === '1')
  // Narrowing the folder shown to the names with these letters (this folder
  // only, as listed: a cut-short listing says so); cleared on leaving it.
  const [filterQ, setFilterQ] = useState('')
  const [byTime, setByTime] = useState(() => local.get(ORDER_KEY) === '1')
  const crumbsRef = useRef<HTMLElement>(null)
  const listedPath = useRef<string | null>(null)
  const [editing, setEditing] = useState<string | null>(null) // the path being typed
  const [uploads, setUploads] = useState<Upload[]>([])
  const [dragOver, setDragOver] = useState(false)
  const [dialog, setDialog] = useState<React.ReactNode>(null)
  // Each dialog closes only itself: an operation that finishes late must not
  // close a dialog opened after its own was dismissed.
  const dialogId = useRef(0)
  const showDialog = (make: (close: () => void) => React.ReactNode) => {
    const id = ++dialogId.current
    setDialog(make(() => dialogId.current === id && setDialog(null)))
  }
  const [viewing, setViewing] = useState<string | null>(null) // a picture of the folder looked at
  const [reading, setReading] = useState<string | null>(null) // a text file of it read
  // (A file looked at: a picture as one, anything else as text — the host says if it is.)
  const look = (full: string) => (IMAGE_EXT.test(full) ? setViewing(full) : setReading(full))
  const [question, setQuestion] = useState<React.ReactNode>(null) // the upload queue's, apart from the dialogs above
  const seq = useRef(0) // numbers navigations; only the latest one's answer is shown
  const settled = useRef(0) // the latest navigation that has finished
  const want = useRef<string | null>(null) // the folder the latest navigation is for (null: not known yet)
  const failedAt = useRef<string | null>(null) // the folder that did not open (what "retry" opens)
  const refreshAfter = useRef(false) // a change landed while a navigation was under way
  const fileInput = useRef<HTMLInputElement>(null)
  const listRef = useRef<HTMLUListElement>(null)
  const uploadKey = useRef(0)
  const alive = useRef(true)
  const pathRef = useRef(path)
  pathRef.current = path
  const uploadsRef = useRef<Upload[]>([])
  uploadsRef.current = uploads
  const pendingAsk = useRef<((ok: boolean) => void) | null>(null)
  // The file asked to be shown: marked while its folder is the one listed.
  const marked = reveal ? { dir: cleanPath(parent(reveal)), name: reveal.slice(reveal.lastIndexOf('/') + 1) } : null
  const markedSaid = useRef(false)
  useEffect(() => {
    alive.current = true
    return () => {
      // Late answers of this panel are ignored, uploads still running stop,
      // and a question left open counts as "no".
      alive.current = false
      seq.current++
      uploadsRef.current.forEach((u) => u.abort.abort())
      queue.current = []
      pendingAsk.current?.(false)
    }
  }, [])

  // load lists a folder ('' is the home folder).
  const load = async (p: string, fallbackHome = false, n = ++seq.current) => {
    want.current = p ? cleanPath(p) : null
    setLoading(true)
    setError('')
    try {
      const l = await api<FileListing>('GET', base + '?path=' + encodeURIComponent(p), undefined, { timeoutMs: 30000 })
      if (n !== seq.current) return
      want.current = l.path
      setListing(l)
      if (l.path !== listedPath.current) setFilterQ('')
      listedPath.current = l.path
      setPath(l.path)
      onPath(l.path)
      listRef.current?.scrollTo(0, 0)
    } catch (e) {
      if (n !== seq.current) return
      if (fallbackHome && e instanceof ApiError && (e.code === 'file_not_found' || e.code === 'not_a_folder' || e.code === 'permission_denied')) return load('', false, n)
      failedAt.current = p
      setError(errorText(e))
    } finally {
      if (n === seq.current) {
        settled.current = n
        setLoading(false)
        // A change made meanwhile may not be in this answer.
        if (refreshAfter.current && alive.current && want.current) {
          refreshAfter.current = false
          void load(want.current)
        }
      }
    }
  }
  const reload = () => path !== null && load(path)

  // refresh shows a change made in dir, if the panel is (still) going there:
  // at once, or after the navigation under way.
  const refresh = (dir: string) => {
    if (!alive.current) return
    // Where the navigation under way goes is not known yet: refresh after it.
    if (want.current === null) refreshAfter.current = true
    if (want.current !== dir) return
    if (settled.current !== seq.current) refreshAfter.current = true
    else void load(dir)
  }

  // goTerminalFolder opens the terminal's folder; it is a navigation of its
  // own, so a later one (home, a typed path…) wins over its late answer.
  const goTerminalFolder = async (quiet: boolean) => {
    const n = ++seq.current
    const from = pathRef.current
    want.current = null
    setLoading(true)
    let p = ''
    try {
      p = (await api<{ path: string }>('GET', base + '/cwd', undefined, { timeoutMs: 15000 })).path
      if (!p && !quiet && n === seq.current) toast(t('files.noCwd'), true)
    } catch (e) {
      if (!quiet && n === seq.current) toast(errorText(e), true)
    }
    if (n !== seq.current) return
    if (p) return load(p, quiet, n)
    if (quiet) return load('', false, n)
    // Not available: the panel stays where it was (before the first listing
    // came, that is the home folder).
    if (from === null) return load('', false, n)
    want.current = from
    settled.current = n
    setLoading(false)
    if (refreshAfter.current) {
      refreshAfter.current = false
      void load(from)
    }
  }

  useEffect(() => {
    if (marked) void load(marked.dir, true)
    else if (start) void load(start, true)
    else void goTerminalFolder(true)
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [sessionId])

  const markHere = !!marked && !!listing && listing.path === marked.dir
  const needle = filterQ.trim().toLowerCase()
  const entries = listing
    ? sortEntries(listing.entries.filter((e) => (hidden || !e.name.startsWith('.') || (markHere && e.name === marked!.name)) && (!needle || e.name.toLowerCase().includes(needle))), byTime)
    : []
  const filterable = !!listing && (listing.entries.length > FILTER_FROM || needle !== '')
  // (A deep path: its end — where one is, and the pencil to edit it — in sight.)
  useLayoutEffect(() => {
    const el = crumbsRef.current
    if (el) el.scrollLeft = el.scrollWidth
  }, [path])
  // (Once listed: scrolled to; not there, said so, once.)
  useEffect(() => {
    if (!marked || !listing || markedSaid.current) return
    markedSaid.current = true
    // (Its folder not opened — gone, not allowed: the home folder instead, said so.)
    if (!markHere) return toast(t('files.revealGone', { dir: marked.dir }), true)
    const el = listRef.current?.querySelector(`[data-name="${CSS.escape(marked.name)}"]`)
    if (el) el.scrollIntoView({ block: 'center' })
    else toast(t(listing.truncated ? 'files.revealCut' : 'files.notThere', { name: marked.name }), true)
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [markHere, listing])
  const hiddenCount = listing ? listing.entries.length - listing.entries.filter((e) => !e.name.startsWith('.')).length : 0

  const toggleHidden = () => {
    setHidden((h) => {
      local.set(HIDDEN_KEY, h ? '0' : '1')
      return !h
    })
  }

  const download = (e: FileEntry) => {
    if (path === null) return
    downloadPath(sessionId, join(path, e.name))
  }

  // Uploads wait in one queue and go one after another, each into the folder
  // shown when it was chosen; only one question is asked at a time.
  type Job = { key: number; file: File; dir: string; abort: AbortController }
  const queue = useRef<Job[]>([])
  const working = useRef(false)
  const upload = (files: File[]) => {
    if (path === null || files.length === 0) return
    const jobs = files.map((file) => ({ key: ++uploadKey.current, file, dir: path, abort: new AbortController() }))
    queue.current.push(...jobs)
    setUploads((u) => [...u, ...jobs.map((j) => ({ key: j.key, name: j.file.name, progress: 0, abort: j.abort }))])
    if (!working.current) void work()
  }
  const work = async () => {
    working.current = true
    try {
      while (alive.current && queue.current.length > 0) await sendOne(queue.current.shift()!)
    } finally {
      working.current = false
    }
  }
  const sendOne = async ({ key, file: f, dir, abort }: Job) => {
    const set = (patch: Partial<Upload>) => setUploads((u) => u.map((x) => (x.key === key ? { ...x, ...patch } : x)))
    const drop = () => setUploads((u) => u.filter((x) => x.key !== key))
    const send = (overwrite: boolean) =>
      postFile(`${base}/upload?dir=${encodeURIComponent(dir)}&name=${encodeURIComponent(f.name)}${overwrite ? '&overwrite=1' : ''}`, f, (p) => set({ progress: p }), abort.signal)
    try {
      try {
        await send(false)
      } catch (e) {
        if (!(e instanceof ApiError && e.code === 'file_exists')) throw e
        if (!(await ask(t('files.replaceBody', { name: f.name }))) || abort.signal.aborted) return drop()
        set({ progress: 0 })
        await send(true)
      }
      drop()
      refresh(dir)
    } catch (e) {
      if (e instanceof ApiError && e.code === 'aborted') drop()
      else set({ error: e instanceof ApiError && e.code === 'bad_name' ? t('files.badName') : errorText(e) })
    }
  }

  const ask = (body: string) =>
    new Promise<boolean>((resolve) => {
      if (!alive.current) return resolve(false)
      let answered = false
      const answer = (ok: boolean) => {
        if (answered) return
        answered = true
        pendingAsk.current = null
        resolve(ok)
      }
      pendingAsk.current = answer
      setQuestion(
        <Confirm
          title={t('files.replaceTitle')}
          body={body}
          confirm={t('files.replace')}
          onConfirm={() => answer(true)}
          onClose={() => {
            setQuestion(null)
            answer(false)
          }}
        />,
      )
    })

  const nameDialog = (title: string, initial: string, action: string, submit: (name: string) => Promise<void>) =>
    showDialog((close) => <NameDialog title={title} initial={initial} action={action} onSubmit={submit} onClose={close} />)

  const mkdir = () => {
    if (path === null) return
    const dir = path
    nameDialog(t('files.newFolder'), '', t('files.create'), async (name) => {
      await api('POST', base + '/mkdir', { dir, name })
      refresh(dir)
    })
  }
  const rename = (e: FileEntry) => {
    if (path === null) return
    const dir = path
    nameDialog(t('files.rename'), e.name, t('files.rename'), async (name) => {
      await api('POST', base + '/rename', { path: join(dir, e.name), name })
      refresh(dir)
    })
  }
  const remove = (e: FileEntry) => {
    if (path === null) return
    const dir = path
    showDialog((close) =>
      <Confirm
        title={t('files.deleteTitle')}
        body={e.type === 'dir' && !e.link ? t('files.deleteFolderBody', { name: e.name }) : t('files.deleteBody', { name: e.name })}
        confirm={t('common.delete')}
        danger
        onClose={close}
        onConfirm={async () => {
          await api('POST', base + '/delete', { path: join(dir, e.name) }, { timeoutMs: 150000 })
          refresh(dir)
        }}
      />,
    )
  }

  const items = (e: FileEntry): MenuItem[] => {
    const full = path === null ? '' : join(path, e.name)
    return [
      e.type === 'file' && previewable(e.name) ? { label: t(IMAGE_EXT.test(e.name) ? 'convo.path.view' : 'preview.item'), icon: IMAGE_EXT.test(e.name) ? <ImageIcon size={16} /> : <Eye size={16} />, onSelect: () => look(full) } : null,
      e.type === 'dir'
        ? { label: t('files.open'), icon: <Folder size={16} />, onSelect: () => void load(full) }
        : e.type === 'file'
          ? { label: t('files.download'), icon: <Download size={16} />, onSelect: () => download(e) }
          : null,
      { label: t('files.insertPath'), icon: <SquareTerminal size={16} />, onSelect: () => leave(() => onInsert(full)) },
      {
        label: t('files.copyPath'),
        icon: <Copy size={16} />,
        onSelect: async () => {
          if (await copyText(full)) toast(t('common.copied'))
        },
      },
      { label: t('files.rename'), icon: <Pencil size={16} />, separator: true, onSelect: () => rename(e) },
      { label: t('common.delete'), icon: <Trash2 size={16} />, danger: true, onSelect: () => remove(e) },
    ].filter(Boolean) as MenuItem[]
  }

  const submitPath = (ev: FormEvent) => {
    ev.preventDefault()
    // Spaces may belong to a name; only a pasted line break is dropped.
    const p = (editing ?? '').replace(/[\r\n]+$/, '')
    if (!p.startsWith('/')) return toast(t('files.absolutePath'), true)
    setEditing(null)
    void load(p)
  }

  const crumbs = path === null ? [] : path.split('/').filter(Boolean)

  // Drag and drop is handled for the whole panel, its backdrop and the
  // layers it opens (React passes their events up through the portals), so
  // a drop never reaches the terminal page's own upload.
  const dragHandlers = {
    onDragOver: (e: React.DragEvent) => {
      e.stopPropagation()
      if (!e.dataTransfer.types.includes('Files')) return
      e.preventDefault()
      if (path !== null) setDragOver(true)
    },
    onDragLeave: (e: React.DragEvent) => {
      e.stopPropagation()
      if (!(e.relatedTarget instanceof Node) || !document.querySelector('.overlay')?.contains(e.relatedTarget)) setDragOver(false)
    },
    onDrop: (e: React.DragEvent) => {
      e.stopPropagation()
      setDragOver(false)
      const files = [...e.dataTransfer.files]
      if (files.length === 0) return
      e.preventDefault()
      upload(files)
    },
  }

  // Leaving the panel stops the uploads still running: asked first.
  const leave = (go: () => void) => {
    const running = uploadsRef.current.filter((u) => !u.error).length
    if (running === 0) return go()
    showDialog((close) => (
      <Confirm
        title={t('files.leaveTitle')}
        body={t('files.leaveBody', { n: running })}
        confirm={t('files.leaveConfirm')}
        danger
        onClose={close}
        onConfirm={go}
      />
    ))
  }
  const closePanel = () => leave(onClose)

  return (
    <div className="files-root" {...dragHandlers}>
    <Dialog
      title={t('files.title')}
      onClose={closePanel}
      wide
      hideTitle
      className="files-dialog"
    >
      <div className="files-head">
        <h2>{t('files.title')}</h2>
        <button className="icon-btn" onClick={closePanel} aria-label={t('common.close')}><X size={18} /></button>
      </div>

      {editing !== null ? (
        <form className="files-path-edit" onSubmit={submitPath}>
          <input
            className="input"
            value={editing}
            autoFocus
            spellCheck={false}
            autoCapitalize="off"
            autoCorrect="off"
            aria-label={t('files.path')}
            onChange={(e) => setEditing(e.target.value)}
          />
          <button className="btn btn-primary" type="submit">{t('files.go')}</button>
          <button className="btn" type="button" onClick={() => setEditing(null)}>{t('common.cancel')}</button>
        </form>
      ) : (
        <nav className="files-crumbs" aria-label={t('files.path')} ref={crumbsRef}>
          <button className="crumb" onClick={() => void load('/')} disabled={path === null}>/</button>
          {crumbs.map((c, i) => {
            const p = '/' + crumbs.slice(0, i + 1).join('/')
            const last = i === crumbs.length - 1
            return (
              <span key={p} className="crumb-wrap">
                {i > 0 && <span className="crumb-sep" aria-hidden="true">/</span>}
                <button className="crumb" aria-current={last ? 'location' : undefined} onClick={() => (last ? setEditing(p) : void load(p))} title={last ? t('files.editPath') : p}>
                  {c}
                </button>
              </span>
            )
          })}
          <button className="icon-btn crumb-edit" onClick={() => setEditing(path ?? '/')} aria-label={t('files.editPath')} title={t('files.editPath')}>
            <Pencil size={14} />
          </button>
        </nav>
      )}

      <div className="files-tools">
        <button className="icon-btn" onClick={() => path !== null && void load(parent(path))} disabled={path === null || path === '/'} aria-label={t('files.up')} title={t('files.up')}>
          <ArrowUp size={18} />
        </button>
        <button className="icon-btn" onClick={() => void load('')} aria-label={t('files.home')} title={t('files.home')}>
          <House size={18} />
        </button>
        {/* (A phone's width: the less used ones go in the "⋯" menu, with words — ux1101 T35.) */}
        <button
          className="icon-btn files-tool-extra"
          onClick={async () => {
            await goTerminalFolder(false)
          }}
          aria-label={t('files.cwd')}
          title={t('files.cwd')}
        >
          <LocateFixed size={18} />
        </button>
        <button className="icon-btn files-tool-extra" onClick={reload} disabled={path === null} aria-label={t('files.refresh')} title={t('files.refresh')}>
          <RotateCw size={18} />
        </button>
        <div className="spacer" />
        {/* (One icon, on or off by its pressed state: ux1101 T36.) */}
        <button className="icon-btn files-tool-extra" aria-pressed={hidden} onClick={toggleHidden} aria-label={t('files.showHidden')} title={t('files.showHidden')}>
          <Eye size={18} />
        </button>
        <button className="icon-btn files-tool-extra" onClick={mkdir} disabled={path === null} aria-label={t('files.newFolder')} title={t('files.newFolder')}>
          <FolderPlus size={18} />
        </button>
        <Menu
          label={t('common.more')}
          items={() => [
            { label: t('files.cwd'), icon: <LocateFixed size={16} />, onSelect: () => void goTerminalFolder(false) },
            { label: t('files.refresh'), icon: <RotateCw size={16} />, disabled: path === null, onSelect: reload },
            { label: t('files.showHidden'), icon: hidden ? <Check size={16} /> : <span className="menu-icon-space" />, checked: hidden, onSelect: toggleHidden },
            { label: t('files.newFolder'), icon: <FolderPlus size={16} />, disabled: path === null, onSelect: mkdir },
          ]}
          button={(open, toggle) => (
            <button className="icon-btn files-tools-more" aria-expanded={open} aria-haspopup="menu" onClick={toggle} aria-label={t('common.more')} title={t('common.more')}>
              <MoreHorizontal size={18} />
            </button>
          )}
        />
        <button className="btn files-upload" onClick={() => fileInput.current?.click()} disabled={path === null}>
          <Upload size={16} />
          <span className="files-upload-label">{t('files.upload')}</span>
        </button>
      </div>

      {filterable && (
        <div className="files-filter">
          <label className="files-filter-box">
            <Search size={15} aria-hidden="true" />
            <input type="search" value={filterQ} onChange={(e) => setFilterQ(e.target.value)} placeholder={t('files.filter')} aria-label={t('files.filter')} />
          </label>
          <button
            type="button"
            className="btn btn-ghost files-order"
            aria-pressed={byTime}
            onClick={() => {
              setByTime(!byTime)
              local.set(ORDER_KEY, byTime ? '0' : '1')
            }}
            title={t('files.byTime')}
          >
            <Clock size={15} /> {t('files.byTimeShort')}
          </button>
        </div>
      )}

      {uploads.length > 0 && (
        <ul className="files-uploads" aria-label={t('files.uploads')}>
          {uploads.map((u) => (
            <li key={u.key} className={u.error ? 'failed' : undefined}>
              <span className="files-upload-name">{u.name}</span>
              {u.error ? (
                <span className="files-upload-err">{u.error}</span>
              ) : (
                <span className="files-progress" role="progressbar" aria-label={t('files.uploadingName', { name: u.name })} aria-valuemin={0} aria-valuemax={100} aria-valuenow={Math.round(u.progress * 100)}>
                  <i style={{ width: Math.round(u.progress * 100) + '%' }} />
                </span>
              )}
              <button
                className="icon-btn"
                aria-label={u.error ? t('common.close') : t('files.cancelUpload')}
                onClick={() => (u.error ? setUploads((l) => l.filter((x) => x.key !== u.key)) : u.abort.abort())}
              >
                <X size={16} />
              </button>
            </li>
          ))}
        </ul>
      )}

      <div className="files-body">
        {error ? (
          <LoadError text={error} onRetry={() => void load(failedAt.current ?? path ?? '')} />
        ) : listing === null ? (
          <p className="hint files-note">{t('common.loading')}</p>
        ) : (
          <>
            <ul className={'files-list' + (loading ? ' loading' : '')} ref={listRef}>
              {entries.map((e) => (
                <li key={e.name} className={'files-row' + (markHere && e.name === marked!.name ? ' is-marked' : '')} data-name={e.name}>
                  <EntryButton entry={e} items={() => items(e)} onOpen={() => void load(join(path!, e.name))} onLook={e.type === 'file' && previewable(e.name) ? () => look(join(path!, e.name)) : undefined} />
                  <Menu
                    label={e.name}
                    heading={e.name}
                    items={() => items(e)}
                    button={(open, toggle) => (
                      <button className="icon-btn files-more" aria-expanded={open} aria-label={t('files.actionsFor', { name: e.name })} onClick={toggle}>
                        <MoreHorizontal size={18} />
                      </button>
                    )}
                  />
                </li>
              ))}
            </ul>
            {entries.length === 0 && (
              <p className="hint files-note">
                {needle ? (
                  <>
                    {t(listing.truncated ? 'files.noMatchCut' : 'files.noMatch')}{' '}
                    <button className="link-btn" onClick={() => setFilterQ('')}>{t('common.clearSearch')}</button>
                  </>
                ) : hiddenCount > 0 && !hidden ? (
                  // (The way to show them, here: not a description of where it is — ux1101 T38.)
                  <>
                    {t('files.onlyHidden', { n: hiddenCount })}{' '}
                    <button className="link-btn" onClick={toggleHidden}>{t('files.showHidden')}</button>
                  </>
                ) : (
                  t('files.empty')
                )}
              </p>
            )}
            {listing.truncated && <p className="hint files-note">{t('files.truncated', { n: listing.entries.length })}</p>}
          </>
        )}
        {dragOver && <div className="files-drop">{t('files.dropHint', { dir: path ?? '' })}</div>}
      </div>
    </Dialog>
      {/* Outside the dialog, so opening it does not try to focus the picker. */}
      <input
        ref={fileInput}
        type="file"
        multiple
        hidden
        onChange={(e) => {
          const files = [...(e.target.files ?? [])]
          e.target.value = ''
          upload(files)
        }}
      />
      {dialog}
      {question}
      {viewing && <PictureDialog sid={sessionId} path={viewing} onClose={() => setViewing(null)} />}
      {reading && <TextPreview sid={sessionId} path={reading} onClose={() => setReading(null)} />}
    </div>
  )
}

// A folder opens; a file that can be looked at is (onLook); any other shows
// what can be done with it (as its ⋯ does for every one).
function EntryButton({ entry: e, items, onOpen, onLook }: { entry: FileEntry; items: () => MenuItem[]; onOpen: () => void; onLook?: () => void }) {
  // (Of its kind, by its name, as VS Code draws it; one of no known kind — a link to nothing — as before.)
  const icon = e.type === 'dir' || e.type === 'file' ? <FileTypeIcon name={e.name} dir={e.type === 'dir'} /> : <FileQuestion size={18} />
  const body = (
    <>
      <span className={'files-icon' + (e.type === 'dir' ? ' dir' : '')} aria-hidden="true">{icon}</span>
      <span className="files-name">
        <span className="files-label">{e.name}</span>
        {e.link && <span className="files-link" aria-label={t('files.link')} title={t('files.link')}>↗</span>}
      </span>
      <span className="files-meta">
        {e.type === 'file' && <span className="tnum">{formatSize(e.size)}</span>}
        <span className="tnum">{formatTime(e.mtime)}</span>
      </span>
    </>
  )
  if (e.type === 'dir') {
    return <button className="files-entry" onClick={onOpen}>{body}</button>
  }
  if (onLook) return <button className="files-entry" onClick={onLook}>{body}</button>
  return (
    <Menu
      label={e.name}
      heading={e.name}
      items={items}
      button={(open, toggle) => (
        <button className="files-entry" aria-expanded={open} aria-haspopup="menu" onClick={toggle}>
          {body}
        </button>
      )}
    />
  )
}

function NameDialog(props: { title: string; initial: string; action: string; onSubmit: (name: string) => Promise<void>; onClose: () => void }) {
  const [name, setName] = useState(props.initial)
  const [error, setError] = useState('')
  const [busy, setBusy] = useState(false)
  const ref = useRef<HTMLInputElement>(null)
  useEffect(() => {
    // Select the name without its extension, as file managers do.
    const el = ref.current
    if (!el) return
    el.focus()
    const dot = props.initial.lastIndexOf('.')
    el.setSelectionRange(0, dot > 0 ? dot : props.initial.length)
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])
  const submit = async (e: FormEvent) => {
    e.preventDefault()
    // Checked as typed: spaces at either end are a valid part of a name.
    const n = name
    if (!n.trim() || n === '.' || n === '..' || n.includes('/')) return setError(t('files.badName'))
    setBusy(true)
    try {
      await props.onSubmit(n)
      props.onClose()
    } catch (err) {
      setError(err instanceof ApiError && err.code === 'bad_name' ? t('files.badName') : errorText(err))
      setBusy(false)
    }
  }
  return (
    <Dialog title={props.title} onClose={props.onClose} icon={<PencilLine size={18} />}>
      <form className="form" onSubmit={submit}>
        <Field label={t('files.name')} htmlFor="files-name" error={error}>
          <input id="files-name" ref={ref} className="input" value={name} spellCheck={false} autoCapitalize="off" autoCorrect="off" onChange={(e) => setName(e.target.value)} />
        </Field>
        <div className="dialog-actions">
          <button type="button" className="btn" onClick={props.onClose}>{t('common.cancel')}</button>
          <button type="submit" className="btn btn-primary" disabled={busy || !name.trim()}>{props.action}</button>
        </div>
      </form>
    </Dialog>
  )
}
