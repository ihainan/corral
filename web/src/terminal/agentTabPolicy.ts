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
