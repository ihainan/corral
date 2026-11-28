// Pure helpers for the Subscriptions overview: staleness checks, reset
// countdown math, refresh cooldown formatting, window state classification,
// host deduplication, and deterministic sorting.

export interface SubQuotaWindow {
  used: number
  resets_at: number
}

export interface SubQuotaGroup {
  name: string
  five_hour?: SubQuotaWindow
  weekly?: SubQuotaWindow
}

export interface SubQuota {
  known: boolean
  updated_at?: number
  plan?: string
  five_hour?: SubQuotaWindow
  weekly?: SubQuotaWindow
  groups?: SubQuotaGroup[]
}

export interface SubHost {
  host_id: number
  host_name: string
  seen_at: number
}

export interface SubscriptionItem {
  provider: string
  account_key: string
  account_label?: string
  plan?: string
  host_id: number
  host_name: string
  hosts: SubHost[]
  quota: SubQuota
  updated_at: number
  seen_at: number
  can_refresh: boolean
  refresh_reason?: string
  next_refresh_at?: number
}

export interface SubscriptionsListResponse {
  items: SubscriptionItem[]
  server_time: number
  cooldown_seconds: number
}

// Snapshots older than 24 hours are flagged as stale so historical figures are
// never mistaken for current usage.
export const SUBSCRIPTION_STALE_SEC = 24 * 60 * 60

export function isSubscriptionStale(updatedAt: number, nowSec: number, thresholdSec = SUBSCRIPTION_STALE_SEC): boolean {
  if (!updatedAt || updatedAt <= 0) return true
  return nowSec - updatedAt >= thresholdSec
}

export function refreshCooldownRemaining(nextRefreshAt: number | undefined, nowSec: number): number {
  if (!nextRefreshAt || nextRefreshAt <= nowSec) return 0
  return Math.ceil(nextRefreshAt - nowSec)
}

export function formatCooldownShort(sec: number, lang: string): string {
  const s = Math.max(0, Math.ceil(sec))
  const m = Math.floor(s / 60)
  const r = s % 60
  if (lang === 'zh-CN') {
    if (m > 0 && r > 0) return `${m} 分 ${r} 秒`
    if (m > 0) return `${m} 分钟`
    return `${r} 秒`
  }
  if (m > 0 && r > 0) return `${m}m ${r}s`
  if (m > 0) return `${m}m`
  return `${r}s`
}

export function formatResetCountdown(resetsAt: number, nowSec: number, lang: string): string {
  const diff = Math.max(0, Math.floor(resetsAt - nowSec))
  if (diff < 60) return lang === 'zh-CN' ? '不到 1 分钟' : '<1m'
  const days = Math.floor(diff / 86400)
  const hours = Math.floor((diff % 86400) / 3600)
  const mins = Math.floor((diff % 3600) / 60)
  if (lang === 'zh-CN') {
    if (days > 0) return hours > 0 ? `${days} 天 ${hours} 小时` : `${days} 天`
    if (hours > 0) return mins > 0 ? `${hours} 小时 ${mins} 分` : `${hours} 小时`
    return `${mins} 分钟`
  }
  if (days > 0) return hours > 0 ? `${days}d ${hours}h` : `${days}d`
  if (hours > 0) return mins > 0 ? `${hours}h ${mins}m` : `${hours}h`
  return `${mins}m`
}

export type WindowTone = '' | 'warn' | 'danger' | 'stale'

export function subscriptionWindowState(
  w: SubQuotaWindow,
  nowSec: number,
  stale: boolean,
): { usedPct: number; expired: boolean; tone: WindowTone } {
  const usedPct = Math.max(0, Math.min(100, Math.round(w.used)))
  const expired = w.resets_at > 0 && w.resets_at <= nowSec
  if (stale || expired) {
    return { usedPct, expired, tone: 'stale' }
  }
  const tone: WindowTone = usedPct >= 95 ? 'danger' : usedPct >= 80 ? 'warn' : ''
  return { usedPct, expired: false, tone }
}

export function providerDisplayName(provider: string): string {
  switch (provider) {
    case 'claude':
      return 'Claude Code'
    case 'codex':
      return 'Codex'
    case 'agy':
      return 'Antigravity'
    default:
      return provider
  }
}

export function providerIconProgram(provider: string): string {
  switch (provider) {
    case 'claude':
      return 'claude'
    case 'codex':
      return 'codex'
    case 'agy':
      return 'antigravity'
    default:
      return provider
  }
}

// Returns distinct host names for an account with the primary reporting host first.
export function subscriptionHostsSummary(item: Pick<SubscriptionItem, 'host_name' | 'hosts'>): string[] {
  const out: string[] = []
  const seen = new Set<string>()
  const push = (name: string) => {
    const clean = name.trim()
    if (!clean || seen.has(clean)) return
    seen.add(clean)
    out.push(clean)
  }
  if (item.host_name) push(item.host_name)
  for (const h of item.hosts ?? []) {
    push(h.host_name)
  }
  return out
}

// Fresh accounts appear before stale ones; within each group newest updated_at first.
export function sortSubscriptions(items: readonly SubscriptionItem[], nowSec: number): SubscriptionItem[] {
  return [...items].sort((a, b) => {
    const aStale = isSubscriptionStale(a.updated_at, nowSec)
    const bStale = isSubscriptionStale(b.updated_at, nowSec)
    if (aStale !== bStale) return aStale ? 1 : -1
    if (a.updated_at !== b.updated_at) return b.updated_at - a.updated_at
    if (a.provider !== b.provider) return a.provider < b.provider ? -1 : 1
    return a.account_key < b.account_key ? -1 : a.account_key > b.account_key ? 1 : 0
  })
}
