import { useLayoutEffect, useRef, type RefObject } from 'react'

// Rows that move by themselves (a poll regroups a list, a row comes in above
// them) slide to where they go, so the eye can follow them; motion-1105.
// Each element under the box marked data-flip="<a key of its own>" is
// measured at every commit; one whose place changed since the last is drawn
// from where it was and slid to where it is (0.15s, transform only). One new
// to the box, marked data-flip-in too, fades in; one marked data-flip-fade
// fades in where it now is instead of sliding there (a group's heading: it
// would slide across the rows it passes, its words over theirs).
// Not when: the box is new (nothing to come from); quiet changed (the person
// asked for a new order or set of rows — a search, a sort — and the change
// is theirs); reduced motion is asked for; the page is hidden; the move is
// longer than the box is high (from out of sight: no sliding past it all).

const MOVE_MS = 150
const IN_MS = 150
const EASE = 'cubic-bezier(0.2, 0, 0, 1)'

const still = () => window.matchMedia('(prefers-reduced-motion: reduce)').matches

// Where an element is laid out: by its offsets, so neither a transform (a
// move under way) nor a scroll counts.
function laidAt(el: HTMLElement): number {
  let y = 0
  for (let e: HTMLElement | null = el; e; e = e.offsetParent as HTMLElement | null) y += e.offsetTop
  return y
}

// The vertical part of its transform now (a move under way), else 0.
function drawnOff(el: HTMLElement): number {
  if (!el.getAnimations().length) return 0
  const m = getComputedStyle(el).transform
  if (!m || m === 'none') return 0
  try {
    return new DOMMatrixReadOnly(m).m42
  } catch {
    return 0
  }
}

export function useFlip(box: RefObject<HTMLElement | null>, quiet?: unknown) {
  const was = useRef<Map<string, number> | null>(null)
  const quietWas = useRef(quiet)
  useLayoutEffect(() => {
    const root = box.current
    if (!root) {
      was.current = null
      return
    }
    const els = [...root.querySelectorAll<HTMLElement>('[data-flip]')]
    const base = laidAt(root)
    const now = new Map<string, number>()
    for (const el of els) now.set(el.dataset.flip!, laidAt(el) - base)
    const before = was.current
    const asked = quietWas.current !== quiet
    quietWas.current = quiet
    was.current = now
    if (!before || !before.size || asked || still() || document.hidden) return
    const far = Math.max(root.clientHeight, window.innerHeight)
    for (const el of els) {
      const from = before.get(el.dataset.flip!)
      if (from === undefined) {
        if (el.dataset.flipIn !== undefined) el.animate([{ opacity: 0 }, { opacity: 1 }], { duration: IN_MS, easing: 'ease-out' })
        continue
      }
      const to = now.get(el.dataset.flip!)!
      if (from === to) continue
      // (Mid-way through a move already: on from where it is drawn.)
      const d = from - to + drawnOff(el)
      el.getAnimations().forEach((a) => a.cancel())
      if (Math.abs(d) < 2 || Math.abs(d) > far) continue
      if (el.dataset.flipFade !== undefined) {
        el.animate([{ opacity: 0 }, { opacity: 1 }], { duration: IN_MS, easing: 'ease-out' })
        continue
      }
      el.animate([{ transform: `translateY(${d}px)` }, { transform: 'none' }], { duration: MOVE_MS, easing: EASE })
    }
  })
}
