import { useEffect, useRef, useState } from 'react'
import { File as FileIcon, Image as ImageIcon, RotateCw, X } from 'lucide-react'
import { t } from '../i18n'
import { attachedIn, keepAttached, type Attached } from './drafts'
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
// (one whose path was lost is not sent as it is). A label is words in the
// box, as a long paste's is: one edited is no longer its file's — its chip
// goes at once — and is sent as the words it is.
// (A picture pasted, named alike by the browser, is labelled by its number
// alone, "[图片 #1]" — 11-05 — which is not taken as a label here: words
// typed so are no file lost; such a label goes as its path while its file is
// known, as words if not — review 3374eba.)
export const ATTACH_LABEL = /\[(?:图片|文件|Attached (?:image|file)) #\d+[：:] ?[^\]\n]{1,60}\]/g


// (A picture pasted is named "image.png" by the browser, every one alike:
// told by its number in the message instead, as its label has it — 11-05.)
const GENERIC = /^(?:image|img|pasted image|screenshot)(?:\.(?:png|jpe?g|gif|webp))?$/i
// attach keeps a file just put up on the host (path) and is its label,
// for the draft; quoted: the path as the agent is to read it.
// (Its picture is asked of the host, as any: what the browser holds of it
// is not shown as it is, the host's checks of a picture's size not made.)
export function attach(f: File, path: string, quoted: string, host: number): string {
  const image = PICTURE_PATH.test(path)
  const name = labelName(f.name || (image ? 'image' : 'file'))
  const a = keepAttached({ send: quoted, path, name, image, host, size: f.size }, (n) => (image && GENERIC.test(name) ? t('attach.imageN', { n }) : t(image ? 'attach.image' : 'attach.file', { n, name })))
  return a.label
}

// takeOut is the draft without a file's label (and the space put after it).
export const takeOut = (draft: string, label: string) => draft.replace(label + ' ', '').replace(label, '')

// A file on its way up (or that did not get there): its place in the strip
// already, how far it has gone drawn on it; one that failed, to be tried again
// by a tap, or taken out.
export type Going = { id: number; name: string; image: boolean; size: number; p: number; state: 'wait' | 'going' | 'failed'; error?: string }

// A file's size as it is said beside its name.
export function sizeText(n: number): string {
  if (n < 1024) return n + ' B'
  if (n < 1024 * 1024) return (n / 1024).toFixed(n < 10 * 1024 ? 1 : 0) + ' KB'
  return (n / 1024 / 1024).toFixed(1) + ' MB'
}

export function AttachStrip({ draft, sid, host, onRemove, onOpen, going = [], onRetry, onCancel }: { draft: string; sid: string; host: number; onRemove: (label: string) => void; onOpen: (a: Attached) => void; going?: Going[]; onRetry?: (id: number) => void; onCancel?: (id: number) => void }) {
  const files = attachedIn(draft)
  const ref = useRef<HTMLDivElement>(null)
  // (More of it than its width shows, on a phone above all: its end fades, so it is seen to go on.)
  const [more, setMore] = useState(false)
  useEffect(() => {
    const el = ref.current
    if (!el) return
    const look = () => setMore(el.scrollWidth - el.scrollLeft - el.clientWidth > 2)
    look()
    el.addEventListener('scroll', look, { passive: true })
    const ro = new ResizeObserver(look)
    ro.observe(el)
    for (const k of el.children) ro.observe(k)
    return () => {
      el.removeEventListener('scroll', look)
      ro.disconnect()
    }
  }, [files.length, going.length])
  if (!files.length && !going.length) return null
  return (
    <div ref={ref} className={'attach-strip' + (more ? ' is-more' : '')} aria-label={t('attach.title')}>
      {files.map((a) => (
        <AttachChip key={a.label} a={a} sid={sid} here={a.host === host} onRemove={() => onRemove(a.label)} onOpen={() => onOpen(a)} />
      ))}
      {going.map((g) => (
        <GoingChip key={'up' + g.id} g={g} onRetry={() => onRetry?.(g.id)} onCancel={() => onCancel?.(g.id)} />
      ))}
    </div>
  )
}

function GoingChip({ g, onRetry, onCancel }: { g: Going; onRetry: () => void; onCancel: () => void }) {
  const failed = g.state === 'failed'
  const said = failed ? t('attach.failed') : t('attach.going', { p: Math.round(g.p * 100) })
  return (
    <span className={'attach-chip is-file is-going' + (failed ? ' is-failed' : '')} role="status" title={failed ? g.error : g.name}>
      <button type="button" className="attach-open" onClick={failed ? onRetry : undefined} disabled={!failed} aria-label={failed ? t('attach.retry', { name: g.name }) : g.name + ' · ' + said}>
        <span className="attach-icon" aria-hidden="true">{failed ? <RotateCw size={16} /> : g.image ? <ImageIcon size={16} /> : <FileIcon size={16} />}</span>
        <span className="attach-text">
          <span className="attach-name">{g.name}</span>
          <span className="attach-sub">{said}</span>
        </span>
      </button>
      {!failed && <span className="attach-bar" style={{ width: Math.round(g.p * 100) + '%' }} aria-hidden="true" />}
      <button type="button" className="attach-x" onClick={onCancel} aria-label={t('attach.cancel', { name: g.name })} title={t('attach.cancel', { name: g.name })}>
        <X size={13} />
      </button>
    </span>
  )
}

function shownName(a: Attached): string {
  const n = /#(\d+)/.exec(a.label)?.[1]
  return a.image && n && GENERIC.test(a.name) ? t('convo.picN', { n }) : a.name
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
  // (A picture: a tile of it, its name in its title; any other file: a card, its name and size.)
  return (
    <span className={'attach-chip' + (a.image ? ' is-pic' : ' is-file') + (here ? '' : ' is-other')} title={here ? a.path : t('attach.otherHost', { path: a.path })}>
      <button type="button" className="attach-open" onClick={onOpen} disabled={!here} aria-label={t(a.image ? 'attach.view' : 'attach.reveal', { name: shownName(a) })}>
        {a.image ? (
          thumb ? <img className="attach-thumb" src={thumb} alt="" /> : <span className="attach-icon" aria-hidden="true"><ImageIcon size={18} /></span>
        ) : (
          <>
            <span className="attach-icon" aria-hidden="true"><FileIcon size={16} /></span>
            <span className="attach-text">
              <span className="attach-name">{shownName(a)}</span>
              {a.size !== undefined && <span className="attach-sub">{sizeText(a.size)}</span>}
            </span>
          </>
        )}
      </button>
      <button type="button" className="attach-x" onClick={onRemove} aria-label={t('attach.remove', { name: a.name })} title={t('attach.remove', { name: a.name })}>
        <X size={13} />
      </button>
    </span>
  )
}
