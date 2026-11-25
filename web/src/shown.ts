import { createContext, createElement, useContext, useEffect, useLayoutEffect, useRef, type DependencyList, type ReactNode } from 'react'

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
// set by the agents' page on each kept tab's box. Hidden, what was sent goes
// on to its end — an answer, a key, a file going up — and is not stopped as
// for a view closed (plans/agent-tabs.md 4.4: what was sent may finish).
// Elsewhere (no kept tabs) a view is never hidden.
// (kids: the parts of a view that may go while it is behind — the conversation beside it, changed from another
// window —, each with its own box: let go when it goes, not only with the whole.)
export type KeptBox = { id: number; hidden: boolean; later: Map<object, () => void>; kids: Map<string, KeptBox> }
export const KeptView = createContext<KeptBox | null>(null)
let boxes = 0
export const keptBox = (hidden = false): KeptBox => ({ id: ++boxes, hidden, later: new Map(), kids: new Map() })
export function hideBox(box: KeptBox, hidden: boolean) {
  box.hidden = hidden
  for (const k of box.kids.values()) hideBox(k, hidden)
}
// A part of a kept view with a box of its own (named: another part in its place is another box).
export function KeptPart({ name, children }: { name: string; children: ReactNode }) {
  const box = useContext(KeptView)
  if (!box) return children
  let mine = box.kids.get(name)
  if (!mine) box.kids.set(name, (mine = keptBox(box.hidden)))
  // (Keyed by its box: one let go and taken again — closed there, opened again before its tab was shown — is made
  // anew, not the one whose sending was stopped — review 11.)
  return createElement(KeptView.Provider, { value: mine, key: mine.id }, children)
}
// The view of a box let go for good: shown, its effects clean up as for any
// view closed; hidden, they were cleaned up already as it went behind (React
// runs them no more), so what they put off is done now.
export function letGoBox(box: KeptBox) {
  box.hidden = false
  const l = [...box.later.values()]
  box.later.clear()
  for (const f of l) f()
  // (Its parts kept on it: React's development replay of a page's effects lets go and takes it again — review 9.)
  for (const k of box.kids.values()) letGoBox(k)
}

// What stops what a view sent, from an effect's cleanup: done at once for a
// view closed; for one sent to a tab behind, put off till it is let go from
// there (review 6: closed from behind, its keys and files still went) — and
// dropped once it is shown again (back(), from the effect's setup).
export function useWhenGone(): { gone: (fn: () => void) => void; back: () => void } {
  const box = useContext(KeptView)
  const now = useRef(box)
  now.current = box
  const me = useRef({}).current
  // (Done at the end of the task, not in it: React's development replay cleans an effect up and sets it going
  // again at once — set going again, what it put off is dropped — review 10.)
  const pending = useRef<(() => void) | null>(null)
  return useRef({
    gone: (fn: () => void) => {
      const b = now.current
      if (b?.hidden) return void b.later.set(me, fn)
      pending.current = fn
      queueMicrotask(() => {
        if (pending.current !== fn) return
        pending.current = null
        fn()
      })
    },
    back: () => {
      pending.current = null
      now.current?.later.delete(me)
    },
  }).current
}
