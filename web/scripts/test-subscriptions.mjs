import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { fileURLToPath } from 'node:url'
import ts from 'typescript'

const srcPath = fileURLToPath(new URL('../src/terminal/subscriptions.ts', import.meta.url))
const src = readFileSync(srcPath, 'utf8')
const { outputText } = ts.transpileModule(src, {
  compilerOptions: { module: ts.ModuleKind.ESNext, target: ts.ScriptTarget.ES2022 },
})
const {
  SUBSCRIPTION_STALE_SEC,
  isSubscriptionStale,
  isSubscriptionUnfetched,
  validWeeklyScoped,
  scopedAllowancePct,
  formatCreditsAmount,
  subscriptionCreditsState,
  refreshCooldownRemaining,
  formatCooldownShort,
  formatResetCountdown,
  subscriptionWindowState,
  providerDisplayName,
  providerIconProgram,
  subscriptionHostsSummary,
  subscriptionRefreshErrorKey,
  sortSubscriptions,
} = await import(`data:text/javascript;base64,${Buffer.from(outputText).toString('base64')}`)

const now = 1800000000

// Staleness threshold: < 24h fresh, >= 24h stale.
assert.equal(isSubscriptionStale(now - 3600, now), false)
assert.equal(isSubscriptionStale(now - SUBSCRIPTION_STALE_SEC + 1, now), false)
assert.equal(isSubscriptionStale(now - SUBSCRIPTION_STALE_SEC, now), true)
assert.equal(isSubscriptionStale(0, now), true)

// Cooldown remaining and formatting.
assert.equal(refreshCooldownRemaining(undefined, now), 0)
assert.equal(refreshCooldownRemaining(now - 5, now), 0)
assert.equal(refreshCooldownRemaining(now + 125, now), 125)
assert.equal(formatCooldownShort(125, 'zh-CN'), '2 分 5 秒')
assert.equal(formatCooldownShort(125, 'en'), '2m 5s')
assert.equal(formatCooldownShort(300, 'en'), '5m')

// Reset countdown formatting.
assert.equal(formatResetCountdown(now + 37 * 3600, now, 'en'), '1d 13h')
assert.equal(formatResetCountdown(now + 37 * 3600, now, 'zh-CN'), '1 天 13 小时')
assert.equal(formatResetCountdown(now + 45, now, 'en'), '<1m')

// Window state tone: warn >= 80, danger >= 95, stale/expired always muted as 'stale'.
assert.deepEqual(subscriptionWindowState({ used: 50, resets_at: now + 3600 }, now, false), { usedPct: 50, expired: false, tone: '' })
assert.deepEqual(subscriptionWindowState({ used: 82, resets_at: now + 3600 }, now, false), { usedPct: 82, expired: false, tone: 'warn' })
assert.deepEqual(subscriptionWindowState({ used: 97, resets_at: now + 3600 }, now, false), { usedPct: 97, expired: false, tone: 'danger' })
assert.deepEqual(subscriptionWindowState({ used: 97, resets_at: now + 3600 }, now, true), { usedPct: 97, expired: false, tone: 'stale' })
assert.deepEqual(subscriptionWindowState({ used: 90, resets_at: now - 10 }, now, false), { usedPct: 90, expired: true, tone: 'stale' })

// Provider display & icons.
assert.equal(providerDisplayName('claude'), 'Claude Code')
assert.equal(providerDisplayName('codex'), 'Codex')
assert.equal(providerDisplayName('agy'), 'Antigravity')
assert.equal(providerDisplayName('devin'), 'Devin')
assert.equal(providerDisplayName('opencode'), 'OpenCode')
assert.equal(providerIconProgram('agy'), 'antigravity')
assert.equal(providerIconProgram('devin'), 'devin')
assert.equal(providerIconProgram('opencode'), 'opencode')

// Unfetched refresh-only placeholder detection & sorting order (fresh -> unfetched -> stale).
assert.equal(isSubscriptionUnfetched({ updated_at: 0, quota: { known: false } }), true)
assert.equal(isSubscriptionUnfetched({ updated_at: now - 60, quota: { known: true } }), false)
assert.equal(isSubscriptionUnfetched({ updated_at: 0 }), false)

const sortedWithUnfetched = sortSubscriptions(
  [
    { provider: 'claude', account_key: 'stale', updated_at: now - SUBSCRIPTION_STALE_SEC - 10, quota: { known: true } },
    { provider: 'opencode', account_key: 'host:1', updated_at: 0, quota: { known: false } },
    { provider: 'devin', account_key: 'fresh', updated_at: now - 60, quota: { known: true } },
  ],
  now,
)
assert.deepEqual(
  sortedWithUnfetched.map((x) => x.account_key),
  ['fresh', 'host:1', 'stale'],
)

// Weekly scoped validation and allowance formatting.
const scoped = validWeeklyScoped([
  { name: 'Fable', used: 0, resets_at: now + 3600, is_active: false, allowance_percent_of_weekly: 50 },
  { name: '', used: 10, resets_at: now + 3600 },
  { name: 'InvalidPct', used: 150, resets_at: now + 3600 },
])
assert.equal(scoped.length, 1)
assert.equal(scoped[0].name, 'Fable')
assert.equal(scopedAllowancePct(scoped[0]), 50)
assert.equal(scopedAllowancePct({ allowance_percent_of_weekly: 0 }), undefined)
assert.equal(scopedAllowancePct({ allowance_percent_of_weekly: undefined }), undefined)

// Credits amount formatting and state calculation.
assert.equal(formatCreditsAmount({ used: 12.5, limit: 50, unit: 'ACU' }), '12.5 / 50 ACU')
assert.equal(formatCreditsAmount({ used: 8.25, limit: 100, unit: 'USD' }), '$8.25 / $100')
assert.deepEqual(subscriptionCreditsState({ used: 42, limit: 50, unit: 'ACU', resets_at: now + 3600 }, now, false), {
  usedPct: 84,
  expired: false,
  tone: 'warn',
})

// Host deduplication with primary host first.
assert.deepEqual(
  subscriptionHostsSummary({
    host_name: 'box-b',
    hosts: [
      { host_id: 1, host_name: 'box-a', seen_at: 10 },
      { host_id: 2, host_name: 'box-b', seen_at: 20 },
      { host_id: 3, host_name: 'box-a', seen_at: 5 },
    ],
  }),
  ['box-b', 'box-a'],
)

// Sorting: fresh before stale, newest first within each group.
const sorted = sortSubscriptions(
  [
    { provider: 'claude', account_key: 'stale-new', updated_at: now - SUBSCRIPTION_STALE_SEC - 10 },
    { provider: 'codex', account_key: 'fresh-old', updated_at: now - 600 },
    { provider: 'agy', account_key: 'fresh-new', updated_at: now - 60 },
    { provider: 'claude', account_key: 'stale-old', updated_at: now - SUBSCRIPTION_STALE_SEC * 3 },
  ],
  now,
)
assert.deepEqual(
  sorted.map((x) => x.account_key),
  ['fresh-new', 'fresh-old', 'stale-new', 'stale-old'],
)

// Manual refresh error code mapping.
assert.equal(subscriptionRefreshErrorKey('account_changed'), 'subs.accountChanged')
assert.equal(subscriptionRefreshErrorKey('host_offline'), 'subs.hostOfflineHint')
assert.equal(subscriptionRefreshErrorKey('refresh_failed'), null)

console.log('subscriptions: all tests passed')
