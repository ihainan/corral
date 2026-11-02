import { useEffect, useState } from 'react'
import { File as FileIcon, X } from 'lucide-react'
import { t } from '../i18n'
import { attachedIn, keepAttached, type Attached } from './drafts'
export { wholeLabels } from './drafts'
import { lookAt, useObjectURL } from './picture'

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

// A whole label of the kind this makes, whether or not its file is known
// (one whose path was lost is not sent as it is). Labels are kept whole in
// the box (wholeLabels): one cut short is not to be looked for.
export const ATTACH_LABEL = /\[(?:图片|文件|Attached (?:image|file)) #\d+[：:] ?[^\]\n]{1,60}\]/g


// attach keeps a file just put up on the host (path) and is its label,
// for the draft; quoted: the path as the agent is to read it.
// (Its picture is asked of the host, as any: what the browser holds of it
// is not shown as it is, the host's checks of a picture's size not made.)
export function attach(f: File, path: string, quoted: string, host: number): string {
  const image = PICTURE_PATH.test(path)
  const name = labelName(f.name || (image ? 'image' : 'file'))
  const a = keepAttached({ send: quoted, path, name, image, host }, (n) => t(image ? 'attach.image' : 'attach.file', { n, name }))
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
  // (Its picture: asked of the host, after what is looked at; not of another host.)
  const [blob, setBlob] = useState<Blob | null>(null)
  const thumb = useObjectURL(blob)
  useEffect(() => {
    if (blob || !a.image || !here) return
    const ctl = new AbortController()
    lookAt(sid, a.path, { signal: ctl.signal }).then(
      (b) => !ctl.signal.aborted && setBlob(b),
      () => {},
    )
    return () => ctl.abort()
  }, [sid, a.path, a.image, here, blob])
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
