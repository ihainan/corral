import { useEffect, useState, type ReactNode } from 'react'
import { Copy, Download, FolderOpen } from 'lucide-react'
import { ApiError, authGen, errorText, onAuthChange } from '../api'
import { t } from '../i18n'
import { copyText, Dialog, DialogHead, HeadTool, useToast } from '../components/ui'
import { FileTypeIcon } from './fileIcons'

// Pictures of a session's host shown on the page (files/view). Asked for one
// at a time a session — each is a transfer, and a host's file channels are
// few — what is looked at now before what is only shown small. What came is
// kept a little while (a few, half a minute: the file may be written again)
// to be shown small again (looked at, it is asked for afresh), and dropped at once on signing in or out; whoever
// shows one makes its own object URL of it, and lets it go. The browser's
// own cache never has them (no-store): a look after signing out is asked
// again, and refused.

// downloadPath has the browser save a file of the session's host.
export function downloadPath(sessionId: string, p: string) {
  const a = document.createElement('a')
  a.href = `/api/sessions/${sessionId}/files/download?path=` + encodeURIComponent(p)
  a.download = p.slice(p.lastIndexOf('/') + 1)
  document.body.appendChild(a)
  a.click()
  a.remove()
}

type Job = { sid: string; path: string; resolve: (b: Blob) => void; reject: (e: unknown) => void; signal?: AbortSignal; gen: number; fresh?: boolean; n: number }

const KEEP = 24
const FRESH_MS = 30_000
const kept = new Map<string, { blob: Blob; at: number; n: number }>() // (the oldest first; n: in the order they came)
let came = 0
const queues = new Map<string, Job[]>()
const running = new Set<string>()

const keyOf = (sid: string, path: string) => sid + '\0' + path

function keep(key: string, blob: Blob) {
  kept.delete(key)
  kept.set(key, { blob, at: Date.now(), n: ++came })
  while (kept.size > KEEP) kept.delete(kept.keys().next().value!)
}

// (Signed in or out: none of it is shown again, nor what was asked as before.)
onAuthChange(() => {
  kept.clear()
  for (const q of queues.values()) for (const j of q.splice(0)) j.reject(new ApiError(401, 'unauthorized', 'signed out'))
})

// forgetPicture drops what is kept of one (the browser could not show it).
export const forgetPicture = (sid: string, path: string) => void kept.delete(keyOf(sid, path))

// lookAt is the picture; first: put before the others waiting; fresh: not
// what is kept of it.
export function lookAt(sid: string, path: string, opts: { first?: boolean; signal?: AbortSignal; fresh?: boolean } = {}): Promise<Blob> {
  const had = kept.get(keyOf(sid, path))
  if (had && !opts.fresh && Date.now() - had.at < FRESH_MS) return Promise.resolve(had.blob)
  return new Promise((resolve, reject) => {
    const q = queues.get(sid) ?? []
    const job = { sid, path, resolve, reject, signal: opts.signal, gen: authGen(), fresh: opts.fresh, n: came }
    if (opts.first) q.unshift(job)
    else q.push(job)
    queues.set(sid, q)
    void work(sid)
  })
}

async function work(sid: string) {
  if (running.has(sid)) return
  running.add(sid)
  try {
    for (;;) {
      const job = queues.get(sid)?.shift()
      if (!job) break
      if (job.signal?.aborted) {
        job.reject(new ApiError(0, 'aborted', 'aborted'))
        continue
      }
      // (Asked for twice while waiting: the second has it from the first — one asked afresh, only if it came after the asking.)
      const had = kept.get(keyOf(sid, job.path))
      if (had && (job.fresh ? had.n > job.n : Date.now() - had.at < 1000)) {
        job.resolve(had.blob)
        continue
      }
      try {
        job.resolve(await fetchPicture(job))
      } catch (e) {
        job.reject(e)
      }
    }
  } finally {
    running.delete(sid)
    queues.delete(sid)
  }
}

// (Busy — another transfer of the session's under way — is asked again a
// few times, a little later each time.)
async function fetchPicture(job: Job): Promise<Blob> {
  for (let i = 0; ; i++) {
    const res = await fetch(`/api/sessions/${job.sid}/files/view?path=` + encodeURIComponent(job.path), { credentials: 'same-origin', signal: job.signal })
    if (job.gen !== authGen()) throw new ApiError(401, 'unauthorized', 'signed out') // (asked as another sign-in)
    if (res.ok) {
      const blob = await res.blob()
      if (job.gen !== authGen()) throw new ApiError(401, 'unauthorized', 'signed out')
      keep(keyOf(job.sid, job.path), blob)
      return blob
    }
    let code = 'failed'
    let message = res.statusText
    try {
      const j = await res.json()
      code = j.error ?? code
      message = j.message ?? message
    } catch {
      // (not said: as failed)
    }
    if (res.status === 429 && i < 3) {
      await new Promise((r) => setTimeout(r, 800 * 2 ** i))
      continue
    }
    throw new ApiError(res.status, code, message)
  }
}

// useObjectURL is an object URL of a picture while it is shown.
export function useObjectURL(blob: Blob | null): string | null {
  const [url, setUrl] = useState<string | null>(null)
  useEffect(() => {
    if (!blob) return setUrl(null)
    const u = URL.createObjectURL(blob)
    setUrl(u)
    return () => URL.revokeObjectURL(u)
  }, [blob])
  return url
}

export function pictureError(e: unknown): string {
  const code = e instanceof ApiError ? e.code : ''
  switch (code) {
    case 'file_not_found':
      return t('picture.gone')
    case 'too_large':
      return t('picture.tooLarge')
    case 'not_an_image':
      return t('picture.notPicture')
    case 'busy':
      return t('picture.busy')
    case 'session_ended':
      return t('picture.ended')
  }
  return errorText(e)
}

// Where a file of the host is, under its name in a viewer's head: its folder
// by the last parts of it (the whole is on hover, and copied), with what else
// is said of it after (its size, say).
export function fileWhere(path: string, ...more: (string | false | undefined)[]): string {
  const dir = path.slice(0, path.lastIndexOf('/') + 1) || '/'
  const parts = dir.split('/').filter(Boolean)
  const short = parts.length > 3 ? '…/' + parts.slice(-3).join('/') + '/' : dir
  return [short, ...more.filter(Boolean)].join(' · ')
}

// What can be done with a file looked at, as the viewer's head holds it:
// copy its path, show it in the file panel, download it. (extra: the
// viewer's own, before them.)
export function FileTools({ sid, path, onReveal, extra }: { sid: string; path: string; onReveal?: () => void; extra?: ReactNode }) {
  const toast = useToast()
  return (
    <>
      {extra}
      <HeadTool
        label={t('convo.path.copy')}
        onClick={async () => {
          if (await copyText(path)) toast(t('common.copied'))
        }}
      >
        <Copy size={16} />
      </HeadTool>
      {onReveal && (
        <HeadTool label={t('convo.path.reveal')} onClick={onReveal}>
          <FolderOpen size={16} />
        </HeadTool>
      )}
      <HeadTool label={t('convo.path.download')} onClick={() => downloadPath(sid, path)}>
        <Download size={16} />
      </HeadTool>
    </>
  )
}

// A picture of the host, looked at: fitted to the window, where it is, and
// what else can be done with it.
export function PictureDialog({ sid, path, onClose, onReveal }: { sid: string; path: string; onClose: () => void; onReveal?: () => void }) {
  const [blob, setBlob] = useState<Blob | null>(null)
  const [error, setError] = useState('')
  const [again, setAgain] = useState(0)
  const url = useObjectURL(blob)
  useEffect(() => {
    const ctl = new AbortController()
    setBlob(null)
    setError('')
    lookAt(sid, path, { first: true, signal: ctl.signal, fresh: true }).then(
      (b) => !ctl.signal.aborted && setBlob(b),
      (e) => !ctl.signal.aborted && setError(pictureError(e)),
    )
    return () => ctl.abort()
  }, [sid, path, again])
  const name = path.slice(path.lastIndexOf('/') + 1)
  return (
    <Dialog title={name} onClose={onClose} wide hideTitle className="dlg picture-dialog">
      <DialogHead icon={<FileTypeIcon name={name} />} title={name} titleText={path} oneLine sub={fileWhere(path)} subText={path} tools={<FileTools sid={sid} path={path} onReveal={onReveal} />} onClose={onClose} />
      <div className="picture-body">
        {error ? (
          <div className="picture-note" role="alert">
            <p>{error}</p>
            <button type="button" className="btn" onClick={() => setAgain((n) => n + 1)}>{t('common.retry')}</button>
          </div>
        ) : url ? (
          <img
            className="picture-img"
            src={url}
            alt={name}
            onError={() => {
              // (Not to be shown by the browser after all: said so, and asked again on retry.)
              forgetPicture(sid, path)
              setBlob(null)
              setError(t('picture.notPicture'))
            }}
          />
        ) : (
          <p className="picture-note hint">{t('common.loading')}</p>
        )}
      </div>
    </Dialog>
  )
}
