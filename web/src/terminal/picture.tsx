import { useEffect, useState } from 'react'
import { Copy, Download, FolderOpen, X } from 'lucide-react'
import { ApiError, authGen, errorText } from '../api'
import { t } from '../i18n'
import { copyText, Dialog, useToast } from '../components/ui'

// Pictures of a session's host shown on the page (files/view). Asked for one
// at a time a session — each is a transfer, and a host's file channels are
// few — what is looked at now before what is only shown small; kept a while
// as the browser's own copy (an object URL) to be shown again, and dropped
// once signed out. Never cached by the browser itself (no-store): a look
// after signing out is asked again, and refused.

// downloadPath has the browser save a file of the session's host.
export function downloadPath(sessionId: string, p: string) {
  const a = document.createElement('a')
  a.href = `/api/sessions/${sessionId}/files/download?path=` + encodeURIComponent(p)
  a.download = p.slice(p.lastIndexOf('/') + 1)
  document.body.appendChild(a)
  a.click()
  a.remove()
}

type Job = { sid: string; path: string; resolve: (url: string) => void; reject: (e: unknown) => void; signal?: AbortSignal }

const KEEP = 24
const kept = new Map<string, { url: string; gen: number }>() // (the oldest first)
const queues = new Map<string, Job[]>()
const running = new Set<string>()

const keyOf = (sid: string, path: string) => sid + '\0' + path

function keep(key: string, url: string) {
  kept.set(key, { url, gen: authGen() })
  while (kept.size > KEEP) {
    const [k, v] = kept.entries().next().value!
    kept.delete(k)
    URL.revokeObjectURL(v.url)
  }
}

// seedPicture keeps the browser's own copy of a picture just put on the
// host: looked at without asking for it again.
export function seedPicture(sid: string, path: string, file: Blob) {
  keep(keyOf(sid, path), URL.createObjectURL(file))
}

// lookAt is the picture's object URL; first: put before the others waiting.
export function lookAt(sid: string, path: string, opts: { first?: boolean; signal?: AbortSignal } = {}): Promise<string> {
  const key = keyOf(sid, path)
  const had = kept.get(key)
  if (had && had.gen === authGen()) {
    kept.delete(key) // (the latest used: last to go)
    kept.set(key, had)
    return Promise.resolve(had.url)
  }
  if (had) {
    kept.delete(key)
    URL.revokeObjectURL(had.url)
  }
  return new Promise((resolve, reject) => {
    const q = queues.get(sid) ?? []
    const job = { sid, path, resolve, reject, signal: opts.signal }
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
      if (had && had.gen === authGen()) {
        job.resolve(had.url)
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
async function fetchPicture(job: Job): Promise<string> {
  const gen = authGen()
  for (let i = 0; ; i++) {
    const res = await fetch(`/api/sessions/${job.sid}/files/view?path=` + encodeURIComponent(job.path), { credentials: 'same-origin', signal: job.signal })
    if (res.ok) {
      const blob = await res.blob()
      const url = URL.createObjectURL(blob)
      if (gen === authGen()) keep(keyOf(job.sid, job.path), url)
      return url
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
  const [url, setUrl] = useState<string | null>(null)
  const [error, setError] = useState('')
  const [again, setAgain] = useState(0)
  useEffect(() => {
    const ctl = new AbortController()
    setUrl(null)
    setError('')
    lookAt(sid, path, { first: true, signal: ctl.signal }).then(
      (u) => !ctl.signal.aborted && setUrl(u),
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
        {url ? (
          <img className="picture-img" src={url} alt={name} onError={() => setError(t('picture.notPicture'))} />
        ) : error ? (
          <div className="picture-note" role="alert">
            <p>{error}</p>
            <button type="button" className="btn" onClick={() => setAgain((n) => n + 1)}>{t('common.retry')}</button>
          </div>
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
