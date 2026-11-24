import { useState } from 'react'

// An account's picture (doc UI item 6, 11-24): the one it set in its settings,
// or else the first letter of its name. Its address names when it was set, so
// the browser keeps it until it changes.

export const AVATAR_SIDE = 256
const AVATAR_MAX_BYTES = 200 << 10 // (the gateway's limit: store.MaxAvatarBytes)

export function Avatar({ id, at, name, className }: { id?: number; at?: number; name: string; className: string }) {
  const src = id && at ? `/api/users/${id}/avatar?v=${at}` : ''
  const [broken, setBroken] = useState('') // (one that would not load: its initial instead)
  const img = !!src && broken !== src
  return (
    <span className={className + (img ? ' has-img' : '')} aria-hidden="true">
      {img ? <img src={src} alt="" draggable={false} onError={() => setBroken(src)} /> : (name.trim()[0] ?? '?').toUpperCase()}
    </span>
  )
}

// squareAvatar: the picture chosen, its middle square drawn at 256 pixels (or
// fewer, if it is smaller) over white, as a JPEG small enough to send — a phone's
// photo of megabytes becomes some tens of kilobytes. Throws if the browser cannot
// read it.
export async function squareAvatar(file: Blob): Promise<Blob> {
  const url = URL.createObjectURL(file)
  try {
    const img = new Image()
    img.src = url
    await img.decode() // (as it is shown: a photo's own turning applied)
    const w = img.naturalWidth
    const h = img.naturalHeight
    if (!w || !h) throw new Error('empty picture')
    const side = Math.min(w, h)
    const out = Math.max(1, Math.min(AVATAR_SIDE, side))
    const canvas = document.createElement('canvas')
    canvas.width = canvas.height = out
    const g = canvas.getContext('2d')
    if (!g) throw new Error('no canvas')
    g.fillStyle = '#fff' // (a PNG's clear parts: white, not the black a JPEG would make of them)
    g.fillRect(0, 0, out, out)
    g.imageSmoothingQuality = 'high'
    g.drawImage(img, (w - side) / 2, (h - side) / 2, side, side, 0, 0, out, out)
    for (const q of [0.85, 0.7, 0.55, 0.4]) {
      const b = await new Promise<Blob | null>((r) => canvas.toBlob(r, 'image/jpeg', q))
      if (b && b.size <= AVATAR_MAX_BYTES) return b
    }
    throw new Error('picture too large')
  } finally {
    URL.revokeObjectURL(url)
  }
}
