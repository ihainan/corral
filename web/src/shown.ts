// The agents' page keeps the tabs not in front drawn but hidden (React's
// <Activity>: display: none on them). What is looked for over the whole page
// is the one shown, not its copy in a tab behind.
export const isShown = (e: Element): boolean =>
  typeof e.checkVisibility === 'function' ? e.checkVisibility() : (e as HTMLElement).offsetParent !== null || getComputedStyle(e).position === 'fixed'

export function firstShown<T extends Element = HTMLElement>(sel: string, root: ParentNode = document): T | null {
  for (const e of root.querySelectorAll<T>(sel)) if (isShown(e)) return e
  return null
}
