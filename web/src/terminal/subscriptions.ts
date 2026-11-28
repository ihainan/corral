// Pure helpers for the Subscriptions overview: staleness checks, reset
// countdown math, refresh cooldown formatting, window state classification,
// host deduplication, and deterministic sorting.

export interface SubQuotaWindow {
  used: number
  resets_at: number
  limited?: boolean
}

export interface SubQuotaScopedLimit {
  name: string
  used: number
  resets_at: number
  is_active?: boolean
  allowance_percent_of_weekly?: number
}

export interface SubQuotaCredits {
  used: number
  limit: number
  unit: string
  resets_at?: number
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
  daily?: SubQuotaWindow
  weekly?: SubQuotaWindow
  monthly?: SubQuotaWindow
  weekly_scoped?: SubQuotaScopedLimit[]
  groups?: SubQuotaGroup[]
  credits?: SubQuotaCredits
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

export function isSubscriptionUnfetched(item: Pick<SubscriptionItem, 'updated_at'> & { quota?: Pick<SubQuota, 'known'> }): boolean {
  return item.quota !== undefined && !item.quota.known && (!item.updated_at || item.updated_at <= 0)
}

export function validWeeklyScoped(list?: SubQuotaScopedLimit[]): SubQuotaScopedLimit[] {
  if (!Array.isArray(list)) return []
  return list.filter(
    (x) =>
      !!x &&
      typeof x.name === 'string' &&
      x.name.trim().length > 0 &&
      typeof x.used === 'number' &&
      Number.isFinite(x.used) &&
      x.used >= 0 &&
      x.used <= 100,
  )
}

export function scopedAllowancePct(entry: Pick<SubQuotaScopedLimit, 'allowance_percent_of_weekly'>): number | undefined {
  const v = entry.allowance_percent_of_weekly
  if (typeof v !== 'number' || !Number.isFinite(v) || v <= 0 || v > 100) return undefined
  return Math.round(v)
}

function formatCreditNum(n: number): string {
  const rounded = Math.round(n * 100) / 100
  return Number.isInteger(rounded) ? String(rounded) : rounded.toFixed(2).replace(/0+$/, '').replace(/\.$/, '')
}

export function formatCreditsAmount(c: Pick<SubQuotaCredits, 'used' | 'limit' | 'unit'>): string {
  const u = formatCreditNum(Math.max(0, c.used))
  const lim = formatCreditNum(Math.max(0, c.limit))
  if (c.unit === 'USD') return `$${u} / $${lim}`
  return `${u} / ${lim} ${c.unit}`
}

export function subscriptionCreditsState(
  c: SubQuotaCredits,
  nowSec: number,
  stale: boolean,
): { usedPct: number; expired: boolean; tone: WindowTone } {
  const pct = c.limit > 0 && Number.isFinite(c.used) ? (c.used / c.limit) * 100 : 0
  return subscriptionWindowState({ used: pct, resets_at: c.resets_at ?? 0 }, nowSec, stale)
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
  const tone: WindowTone = w.limited || usedPct >= 95 ? 'danger' : usedPct >= 80 ? 'warn' : ''
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
    case 'devin':
      return 'Devin'
    case 'opencode':
      return 'OpenCode'
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
    case 'devin':
      return 'devin'
    case 'opencode':
      return 'opencode'
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

// Fresh accounts appear first, then unfetched refresh-only cards, then stale
// ones; within each group newest updated_at first.
export function sortSubscriptions(items: readonly SubscriptionItem[], nowSec: number): SubscriptionItem[] {
  const rank = (x: SubscriptionItem) => {
    if (isSubscriptionUnfetched(x)) return 1
    return isSubscriptionStale(x.updated_at, nowSec) ? 2 : 0
  }
  return [...items].sort((a, b) => {
    const ra = rank(a)
    const rb = rank(b)
    if (ra !== rb) return ra - rb
    if (a.updated_at !== b.updated_at) return b.updated_at - a.updated_at
    if (a.provider !== b.provider) return a.provider < b.provider ? -1 : 1
    return a.account_key < b.account_key ? -1 : a.account_key > b.account_key ? 1 : 0
  })
}

export type SubscriptionRefreshErrorKey = 'subs.hostOfflineHint' | 'subs.accountChanged' | 'subs.opencodeNotRunning'

// Maps subscription-specific manual refresh error codes to dedicated i18n keys.
export function subscriptionRefreshErrorKey(code?: string): SubscriptionRefreshErrorKey | null {
  switch (code) {
    case 'host_offline':
      return 'subs.hostOfflineHint'
    case 'account_changed':
      return 'subs.accountChanged'
    case 'opencode_not_running':
      return 'subs.opencodeNotRunning'
    default:
      return null
  }
}
