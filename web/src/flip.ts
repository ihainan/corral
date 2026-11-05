import { useLayoutEffect, useRef, type RefObject } from 'react'

// Rows that move by themselves (a poll regroups a list, a row comes in above
// them) slide to where they go, so the eye can follow them; motion-1105.
// Each element under the box marked data-flip="<a key of its own>" is
// measured at every commit; one whose place changed since the last is drawn
// from where it was and slid to where it is (0.15s, transform only). One new
// to the box, marked data-flip-in too, fades in; one marked data-flip-fade
// fades in where it now is instead of sliding there (a group's heading: it
// would slide across the rows it passes, its words over theirs).
// Not when: the box is new (nothing to come from), or the page still coming
// in (its first second: one list read in after another is the page being
// drawn, not a change to follow); quiet changed (the person
// asked for a new order or set of rows — a search, a sort — and the change
// is theirs); reduced motion is asked for; the page is hidden; the move is
// longer than the box is seen high (from out of sight: no sliding past it all).

const SETTLE_MS = 1000
const MOVE_MS = 150
const IN_MS = 150
const EASE = 'cubic-bezier(0.2, 0, 0, 1)'

const still = () => window.matchMedia('(prefers-reduced-motion: reduce)').matches

// (Its own animations, by their id: not the CSS transitions an element has
// of its own, a hover's colour.)
const FLIP = 'flip'
const ours = (el: HTMLElement) => el.getAnimations().filter((a) => a.id === FLIP)

// How far a move may slide: as far as the box is seen — its scroller's
// height, not its own (a long list's box is all of it, mostly out of sight).
function seenHeight(root: HTMLElement): number {
  for (let e: HTMLElement | null = root; e; e = e.parentElement) {
    // (A scroller, whether it has more than it shows now or not: a list grown short is still seen through it — review2.)
    const o = getComputedStyle(e).overflowY
    if (o === 'auto' || o === 'scroll') return Math.min(e.clientHeight, window.innerHeight)
  }
  return window.innerHeight
}

// Where an element is laid out: by its offsets, so neither a transform (a
// move under way) nor a scroll counts.
function laidAt(el: HTMLElement): number {
  let y = 0
  for (let e: HTMLElement | null = el; e; e = e.offsetParent as HTMLElement | null) y += e.offsetTop
  return y
}

// The vertical part of its transform now (a move under way), else 0.
function drawnOff(el: HTMLElement): number {
  if (!ours(el).length) return 0
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
  const born = useRef(performance.now())
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
    if (!before || !before.size || asked || still() || document.hidden || performance.now() - born.current < SETTLE_MS) {
      // (A slide under way, from before this: ended, its rows where they are now — not left to
      // run on from a place that is no longer theirs, as when a search is typed mid-way.)
      for (const el of els) ours(el).forEach((a) => a.cancel())
      return
    }
    let far = -1
    for (const el of els) {
      const from = before.get(el.dataset.flip!)
      if (from === undefined) {
        if (el.dataset.flipIn !== undefined) el.animate([{ opacity: 0 }, { opacity: 1 }], { duration: IN_MS, easing: 'ease-out', id: FLIP })
        continue
      }
      const to = now.get(el.dataset.flip!)!
      if (from === to) continue
      // (Mid-way through a move already: on from where it is drawn.)
      const d = from - to + drawnOff(el)
      ours(el).forEach((a) => a.cancel())
      if (far < 0) far = seenHeight(root) // (read once, and only where something moved)
      if (Math.abs(d) < 2 || Math.abs(d) > far) continue
      if (el.dataset.flipFade !== undefined) {
        el.animate([{ opacity: 0 }, { opacity: 1 }], { duration: IN_MS, easing: 'ease-out', id: FLIP })
        continue
      }
      el.animate([{ transform: `translateY(${d}px)` }, { transform: 'none' }], { duration: MOVE_MS, easing: EASE, id: FLIP })
    }
  })
}
