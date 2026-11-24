import { useState } from 'react'

// An account's picture (doc UI item 6, 11-24): the one it set in its settings,
// or else the first letter of its name. Its address names when it was set, so
// the browser keeps it until it changes.

export const AVATAR_SIDE = 256
const AVATAR_MAX_BYTES = 200 << 10 // (the gateway's limit: store.MaxAvatarBytes)
// (What is read whole before it is made small: a phone's photo of 48 megapixels is
// taken, one of more is refused before it is opened — opened, a picture takes
// four bytes a pixel, and 12000 square was half a gigabyte. Review of 11-24.)
const AVATAR_MAX_PIXELS = 64_000_000
const AVATAR_MAX_FILE = 40 << 20

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

export class AvatarTooLarge extends Error {}

// pictureSize: a PNG's, JPEG's, GIF's or WebP's width and height from its first
// bytes, without opening it; null for any other kind (or one not understood).
export async function pictureSize(file: Blob): Promise<[number, number] | null> {
  const b = new Uint8Array(await file.slice(0, 512 << 10).arrayBuffer())
  const u16 = (i: number) => (b[i] << 8) | b[i + 1]
  const u32 = (i: number) => ((b[i] << 24) | (b[i + 1] << 16) | (b[i + 2] << 8) | b[i + 3]) >>> 0
  const le16 = (i: number) => b[i] | (b[i + 1] << 8)
  const le24 = (i: number) => b[i] | (b[i + 1] << 8) | (b[i + 2] << 16)
  const ascii = (i: number, n: number) => String.fromCharCode(...b.slice(i, i + n))
  if (b.length >= 24 && b[0] === 0x89 && ascii(1, 3) === 'PNG') return [u32(16), u32(20)]
  if (b.length >= 10 && ascii(0, 3) === 'GIF') return [le16(6), le16(8)]
  if (b.length >= 30 && ascii(0, 4) === 'RIFF' && ascii(8, 4) === 'WEBP') {
    const kind = ascii(12, 4)
    if (kind === 'VP8X') return [le24(24) + 1, le24(27) + 1]
    if (kind === 'VP8 ') return [le16(26) & 0x3fff, le16(28) & 0x3fff]
    if (kind === 'VP8L') {
      const v = (b[21] | (b[22] << 8) | (b[23] << 16) | (b[24] << 24)) >>> 0
      return [(v & 0x3fff) + 1, ((v >>> 14) & 0x3fff) + 1]
    }
    return null
  }
  if (b.length >= 4 && b[0] === 0xff && b[1] === 0xd8) {
    // (Its segments in turn till a frame's start: its height and width there.)
    let i = 2
    while (i + 9 < b.length) {
      if (b[i] !== 0xff) return null
      const m = b[i + 1]
      if (m === 0xff) {
        i++
        continue
      }
      if (m >= 0xc0 && m <= 0xcf && m !== 0xc4 && m !== 0xc8 && m !== 0xcc) return [u16(i + 7), u16(i + 5)]
      i += 2 + u16(i + 2)
    }
  }
  return null
}

// squareAvatar: the picture chosen, its middle square drawn at 256 pixels (or
// fewer, if it is smaller) over white, as a JPEG small enough to send — a phone's
// photo of megabytes becomes some tens of kilobytes. Throws AvatarTooLarge for
// one too big to open, another error if the browser cannot read it.
export async function squareAvatar(file: Blob): Promise<Blob> {
  if (file.size > AVATAR_MAX_FILE) throw new AvatarTooLarge()
  const size = await pictureSize(file)
  if (size && size[0] * size[1] > AVATAR_MAX_PIXELS) throw new AvatarTooLarge()
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
