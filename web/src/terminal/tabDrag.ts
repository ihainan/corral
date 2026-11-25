import { useEffect, useLayoutEffect, useRef, useState } from 'react'

// (Shared by the terminals' tabs and the agents' — 11-24.)
// Dragging a tab to another place. A mouse drags once it moves a few
// pixels (a click stays a click); a finger holds still a moment first, as
// moving at once scrolls the bar. The others make room as it passes, the bar
// scrolls by itself at its ends, and Esc (or the browser taking the touch)
// leaves the tabs as they were.
interface Drag {
  id: string
  from: number
  to: number
  dx: number
  w: number // the dragged tab's width and the gap after it: what the others move by
}
const DRAG_MOUSE_PX = 4
const DRAG_HOLD_MS = 300
const DRAG_SLOP_PX = 8 // (a finger that moves more before then is scrolling)
const DRAG_EDGE_PX = 32

// Tabs taking other places — moved by a key (Ctrl+Shift+PageUp/Down), one
// closed, or the order changed in another window — slide there from where
// they were drawn, as Chrome's do (doc UI item 13, 11-25); not after a drag,
// whose tabs are drawn where they go already, nor with less motion asked for.
// Where each tab was is read after every render (cheap: a bar's few tabs),
// so it is where it was last drawn, the window since resized or not.
const SLIDE_MS = 180
export function useTabSlide(nav: React.RefObject<HTMLElement | null>, order: string, still: () => boolean) {
  const was = useRef<{ order: string; at: Map<string, number> } | null>(null)
  useLayoutEffect(() => {
    const el = nav.current
    if (!el) return
    // (A bar hidden, its page's away: no places to slide from — it shows its tabs where they are, once back.)
    if (!el.getClientRects().length) return void (was.current = null)
    const tabs = [...el.querySelectorAll<HTMLElement>('.term-tab[data-tab]')]
    const at = new Map(tabs.map((x) => [x.dataset.tab ?? '', x.offsetLeft]))
    const before = was.current
    was.current = { order, at }
    if (!before || before.order === order || still() || matchMedia('(prefers-reduced-motion: reduce)').matches) return
    for (const x of tabs) {
      const from = before.at.get(x.dataset.tab ?? '')
      const to = at.get(x.dataset.tab ?? '')
      if (from === undefined || to === undefined || from === to) continue
      x.animate?.([{ transform: `translateX(${from - to}px)` }, { transform: 'none' }], { duration: SLIDE_MS, easing: 'cubic-bezier(0.2, 0.8, 0.2, 1)' })
    }
  })
}

// The tab in front scrolled into the bar's view (where it is nearest), by
// where it is placed: not where it is drawn while it slides (useTabSlide),
// which scrollIntoView would go by.
export function showFront(nav: HTMLElement | null) {
  const x = nav?.querySelector<HTMLElement>('[aria-current="page"]')?.closest<HTMLElement>('.term-tab')
  if (!nav || !x) return
  const r = x.getBoundingClientRect()
  const box = nav.getBoundingClientRect()
  const t = getComputedStyle(x).transform
  const left = r.left - (t && t !== 'none' ? new DOMMatrixReadOnly(t).m41 : 0) - box.left + nav.scrollLeft
  if (left < nav.scrollLeft) nav.scrollLeft = left
  else if (left + r.width > nav.scrollLeft + nav.clientWidth) nav.scrollLeft = left + r.width - nav.clientWidth
}

export function useTabDrag(nav: React.RefObject<HTMLElement | null>, ids: string[], current: string, onMove: (id: string, to: number) => void) {
  const [drag, setDrag] = useState<Drag | null>(null)
  const cur = useRef<{
    id: string
    from: number
    pointer: number
    x0: number // where the pointer went down (client)
    x: number // where it is
    on: boolean
    at?: number // where it went down in the bar's content, once dragging
    tabs?: { left: number; width: number }[] // the draggable tabs, in the bar's content
    gap?: number
    to?: number
    hold?: number
    frame?: number
    dead?: boolean // let go of before the release (see drop)
    stop: () => void
    drop: () => void
  } | null>(null)
  const endedAt = useRef(0)
  const moved = useRef(false) // a drop changed the order (and the bar is not to scroll for it)

  const update = () => {
    const d = cur.current
    const el = nav.current
    if (!d?.on || !el || !d.tabs || d.at === undefined) return
    const box = el.getBoundingClientRect()
    const me = d.tabs[d.from]
    const first = d.tabs[0]
    const last = d.tabs[d.tabs.length - 1]
    // Drawn within the row; placed by where the pointer is: past another
    // tab's middle, it goes beyond that one (whatever the widths: a wide tab
    // held at the start goes before a narrow first one). Within its own place
    // it is past none it was not past before.
    const px = d.x - box.left + el.scrollLeft
    const dx = Math.min(Math.max(px - d.at, first.left - me.left), last.left + last.width - me.left - me.width)
    let to = 0
    d.tabs.forEach((r, j) => j !== d.from && r.left + r.width / 2 < px && to++)
    d.to = to
    setDrag({ id: d.id, from: d.from, to, dx, w: me.width + (d.gap ?? 0) })
  }

  // The bar scrolls while the pointer is near either end of it.
  const scrollAtEnds = () => {
    const d = cur.current
    const el = nav.current
    if (!d?.on || !el) return
    const box = el.getBoundingClientRect()
    const by = d.x < box.left + DRAG_EDGE_PX ? -8 : d.x > box.right - DRAG_EDGE_PX ? 8 : 0
    if (by && el.scrollWidth > el.clientWidth) {
      el.scrollLeft += by
      update()
    }
    d.frame = requestAnimationFrame(scrollAtEnds)
  }

  const begin = () => {
    const d = cur.current
    const el = nav.current
    if (!d || !el) return
    const box = el.getBoundingClientRect()
    const tabs = [...el.querySelectorAll<HTMLElement>('.term-tab[data-place]')].map((t) => {
      const r = t.getBoundingClientRect()
      return { left: r.left - box.left + el.scrollLeft, width: r.width }
    })
    if (tabs.length < 2 || !tabs[d.from]) return d.stop()
    d.on = true
    d.tabs = tabs
    d.gap = parseFloat(getComputedStyle(el).columnGap) || 0
    d.at = d.x0 - box.left + el.scrollLeft
    update()
    d.frame = requestAnimationFrame(scrollAtEnds)
  }

  const onPointerDown = (e: React.PointerEvent, id: string, from: number) => {
    if (e.button !== 0 || cur.current || ids.length < 2) return
    const mouse = e.pointerType === 'mouse'
    const pointer = e.pointerId
    const move = (ev: PointerEvent) => {
      const d = cur.current
      if (!d || d.dead || ev.pointerId !== pointer) return
      d.x = ev.clientX
      if (d.on) update()
      else if (mouse && Math.abs(d.x - d.x0) > DRAG_MOUSE_PX) begin()
      else if (!mouse && Math.abs(d.x - d.x0) > DRAG_SLOP_PX) d.stop()
    }
    const up = (ev: PointerEvent) => {
      const d = cur.current
      if (!d || ev.pointerId !== pointer) return
      if (d.on || d.dead) endedAt.current = performance.now()
      if (d.on && d.to !== undefined && d.to !== d.from) {
        moved.current = true
        onMove(d.id, d.to)
      }
      d.stop()
    }
    const cancel = (ev: PointerEvent) => ev.pointerId === pointer && cur.current?.stop()
    const key = (ev: KeyboardEvent) => {
      if (ev.key !== 'Escape' || !cur.current?.on) return
      ev.preventDefault()
      ev.stopPropagation()
      cur.current.drop()
    }
    // (A finger held still is not asking for the page's menu.)
    const menu = (ev: Event) => ev.preventDefault()
    const stop = () => {
      const d = cur.current
      if (d?.hold) clearTimeout(d.hold)
      if (d?.frame) cancelAnimationFrame(d.frame)
      cur.current = null
      window.removeEventListener('pointermove', move)
      window.removeEventListener('pointerup', up)
      window.removeEventListener('pointercancel', cancel)
      window.removeEventListener('keydown', key, true)
      window.removeEventListener('contextmenu', menu, true)
      setDrag(null)
    }
    // Let go of with no move (Esc, the tabs changed, another one in front):
    // the rest of the press is still the drag's, and its release no click.
    const drop = () => {
      const d = cur.current
      if (d?.dead) return // (let go of already: the release is still awaited)
      if (!d?.on) return stop()
      d.on = false
      d.dead = true
      if (d.frame) cancelAnimationFrame(d.frame)
      setDrag(null)
    }
    cur.current = { id, from, pointer, x0: e.clientX, x: e.clientX, on: false, stop, drop }
    window.addEventListener('pointermove', move)
    window.addEventListener('pointerup', up)
    window.addEventListener('pointercancel', cancel)
    window.addEventListener('keydown', key, true)
    if (!mouse) {
      window.addEventListener('contextmenu', menu, true)
      cur.current.hold = window.setTimeout(begin, DRAG_HOLD_MS)
    }
  }

  // A finger holding a tab does not scroll the bar. (The bar's own listener,
  // there from the start: whether a touch may be held back from scrolling is
  // settled when it begins, so one added once the drag starts comes too late.)
  useEffect(() => {
    const el = nav.current
    if (!el) return
    const touchMove = (ev: TouchEvent) => {
      if (cur.current?.on && ev.cancelable) ev.preventDefault()
    }
    el.addEventListener('touchmove', touchMove, { passive: false })
    return () => el.removeEventListener('touchmove', touchMove)
  }, [nav])

  // The tabs changed under a drag (one opened or ended, or moved by another
  // window), or another tab came to the front (this bar is hidden then): the
  // drag is let go of.
  const idsKey = ids.join()
  useEffect(() => () => cur.current?.drop(), [idsKey, current])
  useEffect(() => () => cur.current?.stop(), [])

  // Where tab j of the draggable ones is drawn meanwhile.
  const offset = (j: number): number => {
    if (!drag) return 0
    if (j === drag.from) return drag.dx
    if (drag.from < drag.to && j > drag.from && j <= drag.to) return -drag.w
    if (drag.to < drag.from && j >= drag.to && j < drag.from) return drag.w
    return 0
  }
  // A click that ends a drag is not one.
  const dragged = () => performance.now() - endedAt.current < 400
  return { drag, onPointerDown, offset, dragged, moved }
}

