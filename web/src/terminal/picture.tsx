import { useEffect, useState } from 'react'
import { Copy, Download, FolderOpen, X } from 'lucide-react'
import { ApiError, authGen, errorText, onAuthChange } from '../api'
import { t } from '../i18n'
import { copyText, Dialog, useToast } from '../components/ui'

// Pictures of a session's host shown on the page (files/view). Asked for one
// at a time a session — each is a transfer, and a host's file channels are
// few — what is looked at now before what is only shown small. What came is
// kept a little while (a few, half a minute: the file may be written again)
// to be shown again, and dropped at once on signing in or out; whoever
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

type Job = { sid: string; path: string; resolve: (b: Blob) => void; reject: (e: unknown) => void; signal?: AbortSignal; gen: number }

const KEEP = 24
const FRESH_MS = 30_000
// (seeded: the browser's own copy of a file it just put up — of a name of its own, never written again.)
const kept = new Map<string, { blob: Blob; at: number; seeded?: boolean }>() // (the oldest first)
const queues = new Map<string, Job[]>()
const running = new Set<string>()

const keyOf = (sid: string, path: string) => sid + '\0' + path

function keep(key: string, blob: Blob, seeded = false) {
  kept.delete(key)
  kept.set(key, { blob, at: Date.now(), seeded })
  while (kept.size > KEEP) kept.delete(kept.keys().next().value!)
}

// (Signed in or out: none of it is shown again, nor what was asked as before.)
onAuthChange(() => {
  kept.clear()
  for (const q of queues.values()) for (const j of q.splice(0)) j.reject(new ApiError(401, 'unauthorized', 'signed out'))
})

// seedPicture keeps the browser's own copy of a picture just put on the
// host: looked at without asking for it again.
export function seedPicture(sid: string, path: string, file: Blob) {
  keep(keyOf(sid, path), file, true)
}

// forgetPicture drops what is kept of one (the browser could not show it).
export const forgetPicture = (sid: string, path: string) => void kept.delete(keyOf(sid, path))

// lookAt is the picture; first: put before the others waiting; fresh: not
// what is kept of it.
export function lookAt(sid: string, path: string, opts: { first?: boolean; signal?: AbortSignal; fresh?: boolean } = {}): Promise<Blob> {
  const had = kept.get(keyOf(sid, path))
  if (had && !opts.fresh && (had.seeded || Date.now() - had.at < FRESH_MS)) return Promise.resolve(had.blob)
  return new Promise((resolve, reject) => {
    const q = queues.get(sid) ?? []
    const job = { sid, path, resolve, reject, signal: opts.signal, gen: authGen() }
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
      // (Asked for twice while waiting: the second has it from the first.)
      const had = kept.get(keyOf(sid, job.path))
      if (had && Date.now() - had.at < 1000) {
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

// A picture of the host, looked at: fitted to the window, its path, and
// what else can be done with it.
export function PictureDialog({ sid, path, onClose, onReveal }: { sid: string; path: string; onClose: () => void; onReveal?: () => void }) {
  const toast = useToast()
  const [blob, setBlob] = useState<Blob | null>(null)
  const [error, setError] = useState('')
  const [again, setAgain] = useState(0)
  const url = useObjectURL(blob)
  useEffect(() => {
    const ctl = new AbortController()
    setBlob(null)
    setError('')
    lookAt(sid, path, { first: true, signal: ctl.signal, fresh: again > 0 }).then(
      (b) => !ctl.signal.aborted && setBlob(b),
      (e) => !ctl.signal.aborted && setError(pictureError(e)),
    )
    return () => ctl.abort()
  }, [sid, path, again])
  const name = path.slice(path.lastIndexOf('/') + 1)
  return (
    <Dialog title={name} onClose={onClose} wide hideTitle className="picture-dialog">
      <div className="picture-head">
        <h2 className="picture-name" title={name}>{name}</h2>
        <button type="button" className="icon-btn" onClick={onClose} aria-label={t('common.close')}><X size={18} /></button>
      </div>
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
      <div className="picture-foot">
        <span className="picture-path" title={path}>{path}</span>
        <span className="picture-acts">
          <button
            type="button"
            className="btn btn-ghost"
            onClick={async () => {
              if (await copyText(path)) toast(t('common.copied'))
            }}
          >
            <Copy size={15} /> {t('convo.path.copy')}
          </button>
          {onReveal && (
            <button type="button" className="btn btn-ghost" onClick={onReveal}>
              <FolderOpen size={15} /> {t('convo.path.reveal')}
            </button>
          )}
          <button type="button" className="btn btn-ghost" onClick={() => downloadPath(sid, path)}>
            <Download size={15} /> {t('convo.path.download')}
          </button>
        </span>
      </div>
    </Dialog>
  )
}
