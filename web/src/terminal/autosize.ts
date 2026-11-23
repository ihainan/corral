import { useEffect, type RefObject } from 'react'

// A composer's box grows with its text, up to max: its height measured on a
// hidden copy, never by letting the box itself go to 'auto' first. That
// shrank it for a moment, and what is beside it grew: a scrolled thread over
// it was pulled back by as much (its scroll kept within what then fit) and
// not put back when the box grew again, so the thread jumped at each change,
// at each letter an input method sends. Set only when it changes.
let copy: HTMLTextAreaElement | null = null
const COPIED = [
  'boxSizing',
  'width',
  'minHeight',
  'paddingTop',
  'paddingBottom',
  'paddingLeft',
  'paddingRight',
  'borderTopWidth',
  'borderBottomWidth',
  'borderLeftWidth',
  'borderRightWidth',
  'fontFamily',
  'fontSize',
  'fontWeight',
  'fontStyle',
  'fontStretch',
  'fontVariant',
  'fontFeatureSettings',
  'fontKerning',
  'textRendering',
  'direction',
  'letterSpacing',
  'wordSpacing',
  'lineHeight',
  'textTransform',
  'textIndent',
  'whiteSpace',
  'wordBreak',
  'overflowWrap',
  'tabSize',
] as const

export function fitComposer(el: HTMLTextAreaElement, max: number) {
  if (!el.isConnected || el.clientWidth === 0) return // (not shown: measured when it is)
  if (!copy) {
    copy = document.createElement('textarea')
    copy.setAttribute('aria-hidden', 'true')
    copy.tabIndex = -1
    Object.assign(copy.style, { position: 'absolute', top: '0', left: '-9999px', visibility: 'hidden', pointerEvents: 'none', overflow: 'hidden', height: '0', maxHeight: 'none', borderStyle: 'solid' })
  }
  if (!copy.isConnected) document.body.appendChild(copy) // (not among the page's own: React keeps those)
  const cs = getComputedStyle(el)
  for (const k of COPIED) copy.style[k] = cs[k]
  copy.rows = el.rows
  copy.value = el.value
  copy.placeholder = el.placeholder // (shown when empty, and as tall as it wraps)
  const h = Math.min(copy.scrollHeight, max) + 'px'
  copy.value = ''
  if (el.style.height !== h) el.style.height = h
  // Still more than it holds, under max: what the copy could not know, as
  // a scrollbar the box itself took (a paste, before it grew) narrowing it
  // by a line. Only ever taller: nothing beside it is pulled back.
  if (el.scrollHeight > el.clientHeight && el.clientHeight < max) el.style.height = Math.min(el.scrollHeight, max) + 'px'
}

// A composer's box fitted to its text, and to its placeholder (an agent's
// name, a suggestion: it changes with no change of the text), measured
// again when the box's width changes (a phone turned, a side opened: the
// lines wrap anew) and when what is visible changes: up to 40% of what is
// visible, less on very short screens (a phone on its side with the
// keyboard up), so that what is over it keeps some room. on: whether it
// is there.
export function useComposerFit(ref: RefObject<HTMLTextAreaElement | null>, text: string, placeholder: string, on = true) {
  useEffect(() => {
    const el = ref.current
    if (!el || !on) return
    const fit = () => {
      const vh = window.visualViewport?.height ?? window.innerHeight
      fitComposer(el, Math.max(40, Math.min(vh * 0.4, vh - 260)))
    }
    fit()
    let width = el.clientWidth
    const ro = new ResizeObserver(() => {
      if (el.clientWidth === width) return // (its own height set: nothing to measure)
      width = el.clientWidth
      fit()
    })
    ro.observe(el)
    const vv = window.visualViewport
    vv?.addEventListener('resize', fit)
    return () => {
      ro.disconnect()
      vv?.removeEventListener('resize', fit)
    }
  }, [ref, text, placeholder, on])
}
