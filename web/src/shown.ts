import { createContext, useContext, useEffect, useLayoutEffect, useRef, type DependencyList } from 'react'

// The agents' page keeps the tabs not in front drawn but hidden (React's
// <Activity>: display: none on them, their portals' too). What is looked for
// over the whole page is the one shown, not its copy in a tab behind.
export const isShown = (e: Element): boolean => (typeof e.checkVisibility === 'function' ? e.checkVisibility() : e.getClientRects().length > 0)

export function firstShown<T extends Element = HTMLElement>(sel: string, root: ParentNode = document): T | null {
  for (const e of root.querySelectorAll<T>(sel)) if (isShown(e)) return e
  return null
}
// (A dialog shown over the page: not one in a tab behind.)
export const modalShown = (sel = '[aria-modal="true"]') => !!firstShown(sel)

// A hidden <Activity> keeps a view's state but runs its effects again when it
// is shown: that is not a view made anew. These tell the two apart.

// When the view was last shown (made, or shown again from a tab behind): read
// while drawing, so the drawing that shows it already goes by it.
export function useShownAt(): number {
  const hidden = useRef(true)
  const at = useRef(0)
  if (hidden.current) at.current = Date.now()
  useLayoutEffect(() => {
    hidden.current = false
    return () => void (hidden.current = true)
  }, [])
  return at.current
}

// Done as the view goes to the back (or goes): what is open over it closed, say.
export function useOnHide(fn: () => void) {
  const f = useRef(fn)
  f.current = fn
  useLayoutEffect(() => () => f.current(), [])
}

// An effect for a change of its deps only — not for the view shown again with
// the same ones (a reset there would undo what the kept view holds).
export function useEffectOnChange(fn: () => void, deps: DependencyList) {
  const last = useRef<DependencyList | null>(null)
  useEffect(() => {
    if (last.current && last.current.length === deps.length && last.current.every((v, i) => Object.is(v, deps[i]))) return
    last.current = deps
    fn()
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, deps)
}

// Whether the view is being sent to a tab behind (kept) rather than let go:
// set by the agents' page on each kept tab's box, read as an effect is cleaned
// up. Hidden, what was sent goes on to its end — an answer, a key, a file
// going up — and is not stopped as for a view closed (plans/agent-tabs.md 4.4:
// what was sent may finish). Elsewhere (no kept tabs) always false.
export const KeptView = createContext<{ hidden: boolean } | null>(null)
export function useHiddenNow(): () => boolean {
  const box = useContext(KeptView)
  return () => !!box?.hidden
}
