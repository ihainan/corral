import { useEffect, useState } from 'react'
import { File as FileIcon, X } from 'lucide-react'
import { t } from '../i18n'
import { attachedIn, keepAttached, type Attached } from './drafts'
import { lookAt, seedPicture } from './picture'

// Files put up from a composer: a label in the draft (sent as the path),
// and over the box a chip each — its picture small, its name, its whole
// path in its tooltip — to look at it (a picture) or find it (another
// file), or take it out of the message (the file stays on the host, as it
// did when its path was deleted from the draft).

export const PICTURE_PATH = /\.(png|jpe?g|gif|webp)$/i

// A file's name as its label says it: no brackets or line breaks (the
// label's own end), not too long (its end, the extension, kept).
export function labelName(name: string): string {
  const n = name.replace(/[[\]\x00-\x1f\x7f]/g, ' ').replace(/\s+/g, ' ').trim() || 'file'
  return n.length > 40 ? n.slice(0, 28) + '…' + n.slice(-11) : n
}

// A label of the kind this makes, whether or not its file is known (one
// whose path was lost is not sent as it is).
export const ATTACH_LABEL = /\[(?:图片|文件|Attached (?:image|file)) #\d+[：:] ?[^\]\n]{1,60}\]/g

// The browser's own copies of files just put up: shown small at once, and
// looked at without asking the host. This page's only, a few.
const thumbs = new Map<string, string>() // label → object URL
function keepThumb(label: string, file: File) {
  thumbs.set(label, URL.createObjectURL(file))
  while (thumbs.size > 40) {
    const [k, v] = thumbs.entries().next().value!
    thumbs.delete(k)
    URL.revokeObjectURL(v)
  }
}

// attach keeps a file just put up on the host (path) and is its label,
// for the draft; quoted: the path as the agent is to read it.
export function attach(f: File, path: string, quoted: string, sid: string, host: number): string {
  const image = PICTURE_PATH.test(path)
  const name = labelName(f.name || (image ? 'image' : 'file'))
  const a = keepAttached({ send: quoted, path, name, image, host }, (n) => t(image ? 'attach.image' : 'attach.file', { n, name }))
  if (image) {
    keepThumb(a.label, f)
    seedPicture(sid, path, f)
  }
  return a.label
}

// takeOut is the draft without a file's label (and the space put after it).
export const takeOut = (draft: string, label: string) => draft.replace(label + ' ', '').replace(label, '')

export function AttachStrip({ draft, sid, host, onRemove, onOpen }: { draft: string; sid: string; host: number; onRemove: (label: string) => void; onOpen: (a: Attached) => void }) {
  const files = attachedIn(draft)
  if (!files.length) return null
  return (
    <div className="attach-strip" aria-label={t('attach.title')}>
      {files.map((a) => (
        <AttachChip key={a.label} a={a} sid={sid} here={a.host === host} onRemove={() => onRemove(a.label)} onOpen={() => onOpen(a)} />
      ))}
    </div>
  )
}

function AttachChip({ a, sid, here, onRemove, onOpen }: { a: Attached; sid: string; here: boolean; onRemove: () => void; onOpen: () => void }) {
  // (Its picture: the browser's copy, else — the page loaded since — asked
  // of the host, after what is looked at; not of another host.)
  const [thumb, setThumb] = useState(() => thumbs.get(a.label) ?? null)
  useEffect(() => {
    if (thumb || !a.image || !here) return
    const ctl = new AbortController()
    lookAt(sid, a.path, { signal: ctl.signal }).then(
      (u) => !ctl.signal.aborted && setThumb(u),
      () => {},
    )
    return () => ctl.abort()
  }, [sid, a.path, a.image, here, thumb])
  return (
    <span className={'attach-chip' + (here ? '' : ' is-other')} title={here ? a.path : t('attach.otherHost', { path: a.path })}>
      <button type="button" className="attach-open" onClick={onOpen} disabled={!here} aria-label={t(a.image ? 'attach.view' : 'attach.reveal', { name: a.name })}>
        {thumb ? <img className="attach-thumb" src={thumb} alt="" /> : <span className="attach-icon" aria-hidden="true"><FileIcon size={15} /></span>}
        <span className="attach-name">{a.name}</span>
      </button>
      <button type="button" className="attach-x" onClick={onRemove} aria-label={t('attach.remove', { name: a.name })} title={t('attach.remove', { name: a.name })}>
        <X size={13} />
      </button>
    </span>
  )
}
