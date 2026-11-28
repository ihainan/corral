// Pure state transitions for agent tabs (tested directly in web/scripts).

export type AgentTabLike = {
  id: string
  key: string
  s: string
  pane?: string
  split?: string
  focus?: 'primary' | 'secondary'
  title?: string
  program?: string
  // True for the single preview (unpinned) tab; omitted for normal/pinned tabs.
  preview?: boolean
}

export type OpenAgentTabTarget = {
  key: string
  s: string
  pane?: string
}

export type OpenAgentTabResult<T extends AgentTabLike> = {
  tabs: T[]
  front: string
  focus?: 'primary' | 'secondary'
  // Set when the target was already open in an existing tab (primary or split).
  existing: T | null
  changed: boolean
}

function withoutPreview<T extends AgentTabLike>(tab: T): T {
  if (!('preview' in tab)) return tab
  const next = { ...tab }
  delete next.preview
  return next
}

// Sanitize persisted tabs so only explicit `preview: true` is kept and at most
// one preview tab exists (if multiple exist from older/raced writes, only the
// last one remains a preview tab and earlier ones are promoted to normal).
export function normalizeAgentTabs<T extends AgentTabLike>(tabs: readonly T[]): T[] {
  let lastPreview = -1
  for (let i = 0; i < tabs.length; i++) {
    if (tabs[i].preview === true) lastPreview = i
  }
  return tabs.map((tab, i) => {
    if (i === lastPreview) return tab.preview === true ? tab : { ...tab, preview: true }
    if (tab.preview !== undefined) return withoutPreview(tab)
    return tab
  })
}

// Open or focus a conversation in the tab bar following VSCode's preview-tab
// semantics:
// - If already open as a tab's primary agent (or live split pane), focus that
//   tab; if mode === 'pinned' and that tab is preview, promote it to normal.
// - Otherwise, in 'preview' mode, reuse the single existing preview tab in
//   place (if any) or insert a new preview tab right after the front tab.
// - In 'pinned' mode, insert a new normal (non-preview) tab right after the
//   front tab.
export function openAgentTab<T extends AgentTabLike>(
  tabs: readonly T[],
  front: string | null,
  target: OpenAgentTabTarget,
  mode: 'preview' | 'pinned',
  makeId: () => string,
  liveKeys?: ReadonlySet<string>,
): OpenAgentTabResult<T> {
  const openIdx = tabs.findIndex((x) => x.key === target.key)
  if (openIdx >= 0) {
    const open = tabs[openIdx]
    const needSync = open.s !== target.s || (open.pane ?? '') !== (target.pane ?? '')
    const needPin = mode === 'pinned' && !!open.preview
    if (!needSync && !needPin) {
      return { tabs: [...tabs], front: open.id, existing: open, changed: false }
    }
    const updatedBase: T = needSync ? { ...open, s: target.s, pane: target.pane } : { ...open }
    const updated = needPin ? withoutPreview(updatedBase) : updatedBase
    const next = tabs.map((x, i) => (i === openIdx ? updated : x))
    return { tabs: next, front: updated.id, existing: updated, changed: true }
  }

  const besideIdx = tabs.findIndex((x) => x.split === target.key && (!liveKeys || liveKeys.has(x.key)))
  if (besideIdx >= 0) {
    const beside = tabs[besideIdx]
    if (mode === 'pinned' && beside.preview) {
      const updated = withoutPreview(beside)
      const next = tabs.map((x, i) => (i === besideIdx ? updated : x))
      return { tabs: next, front: updated.id, focus: 'secondary', existing: updated, changed: true }
    }
    return { tabs: [...tabs], front: beside.id, focus: 'secondary', existing: beside, changed: false }
  }

  if (mode === 'preview') {
    const previewIdx = tabs.findIndex((x) => x.preview === true)
    if (previewIdx >= 0) {
      const prev = tabs[previewIdx]
      const replaced: T = {
        ...prev,
        key: target.key,
        s: target.s,
        pane: target.pane,
        split: undefined,
        focus: undefined,
        title: undefined,
        program: undefined,
        preview: true,
      }
      const next = tabs.map((x, i) => (i === previewIdx ? replaced : x))
      return { tabs: next, front: replaced.id, existing: null, changed: true }
    }
  }

  const created = {
    id: makeId(),
    key: target.key,
    s: target.s,
    ...(target.pane !== undefined ? { pane: target.pane } : {}),
    ...(mode === 'preview' ? { preview: true } : {}),
  } as T
  const next = [...tabs]
  const frontIdx = front ? next.findIndex((x) => x.id === front) : -1
  const insertAt = frontIdx >= 0 ? frontIdx + 1 : next.length
  next.splice(insertAt, 0, created)
  return { tabs: next, front: created.id, existing: null, changed: true }
}

// Promote a preview tab to a normal (pinned) tab by tab id.
export function pinAgentTab<T extends AgentTabLike>(
  tabs: readonly T[],
  id: string | null,
): { tabs: T[]; changed: boolean } {
  if (!id) return { tabs: [...tabs], changed: false }
  const idx = tabs.findIndex((x) => x.id === id)
  if (idx < 0 || !tabs[idx].preview) return { tabs: [...tabs], changed: false }
  const next = tabs.map((x, i) => (i === idx ? withoutPreview(x) : x))
  return { tabs: next, changed: true }
}

// Promote any preview tab hosting `key` (as primary or split pane) to a normal tab.
export function pinAgentTabByKey<T extends AgentTabLike>(
  tabs: readonly T[],
  key: string,
): { tabs: T[]; changed: boolean } {
  if (!key) return { tabs: [...tabs], changed: false }
  let changed = false
  const next = tabs.map((x) => {
    if (!x.preview || (x.key !== key && x.split !== key)) return x
    changed = true
    return withoutPreview(x)
  })
  return { tabs: next, changed }
}

// Reorder a tab in the tab bar; moving a preview tab also pins it (matches VSCode).
export function moveAgentTab<T extends AgentTabLike>(
  tabs: readonly T[],
  id: string,
  to: number,
): { tabs: T[]; changed: boolean } {
  const from = tabs.findIndex((x) => x.id === id)
  if (from < 0 || to < 0 || to >= tabs.length || from === to) {
    return { tabs: [...tabs], changed: false }
  }
  const next = [...tabs]
  const [moved] = next.splice(from, 1)
  next.splice(to, 0, withoutPreview(moved))
  return { tabs: next, changed: true }
}

export type CloseAgentTabsResult<T extends AgentTabLike> = {
  tabs: T[]
  front: string | null
  nextTab: T | null
  closedFront: boolean
  changed: boolean
}

// Close all tabs whose primary agent matches closedKey (user actively closed
// that agent in Corral) and clear split references to closedKey in any other
// tab. If the front tab is closed, pick the adjacent tab (next to the right,
// else closest to the left, else null) — identical to clicking × on the tab.
export function closeAgentTabs<T extends AgentTabLike>(
  tabs: readonly T[],
  front: string | null,
  closedKey: string,
): CloseAgentTabsResult<T> {
  const closedIds = new Set(tabs.filter((x) => x.key === closedKey).map((x) => x.id))
  const at = front ? tabs.findIndex((x) => x.id === front) : -1
  let changed = closedIds.size > 0
  const remaining: T[] = []
  for (const tab of tabs) {
    if (closedIds.has(tab.id)) continue
    if (tab.split === closedKey) {
      changed = true
      remaining.push({ ...tab, split: undefined, focus: undefined })
    } else {
      remaining.push(tab)
    }
  }
  const closedFront = !!front && closedIds.has(front)
  if (!closedFront) {
    return { tabs: remaining, front, nextTab: null, closedFront: false, changed }
  }
  const nextTab =
    (at >= 0 ? tabs.slice(at + 1).find((x) => !closedIds.has(x.id)) : undefined) ??
    (at > 0 ? [...tabs.slice(0, at)].reverse().find((x) => !closedIds.has(x.id)) : undefined) ??
    null
  const resolvedNext = nextTab ? (remaining.find((x) => x.id === nextTab.id) ?? null) : null
  return {
    tabs: remaining,
    front: resolvedNext?.id ?? null,
    nextTab: resolvedNext,
    closedFront: true,
    changed: true,
  }
}

// Migrate agent tab keys when a tmux session is renamed so open tabs keep
// pointing at the renamed tmux session instead of appearing ended.
export function migrateAgentTabsKey<T extends AgentTabLike>(
  tabs: readonly T[],
  oldKey: string,
  newKey: string,
): { tabs: T[]; changed: boolean } {
  if (!oldKey || !newKey || oldKey === newKey) return { tabs: [...tabs], changed: false }
  let changed = false
  const next = tabs.map((tab) => {
    const keyHit = tab.key === oldKey
    const splitHit = tab.split === oldKey
    if (!keyHit && !splitHit) return tab
    changed = true
    return {
      ...tab,
      ...(keyHit ? { key: newKey } : {}),
      ...(splitHit ? { split: newKey } : {}),
    }
  })
  return { tabs: next, changed }
}

// Event dispatched when the user actively closes an agent pane from another
// surface (e.g. Terminal's close_pane shortcut) so AgentsChat closes its tab
// instead of treating it as an unexpected exit.
export const AGENT_CLOSED_EVENT = 'corral:agent-closed'

export function notifyAgentClosed(key: string) {
  if (typeof window === 'undefined' || !key) return
  window.dispatchEvent(new CustomEvent<{ key: string }>(AGENT_CLOSED_EVENT, { detail: { key } }))
}
