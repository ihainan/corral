import { useCallback, useEffect, useMemo, useState } from 'react'
import { AlertTriangle, ChevronLeft, RefreshCw, Server } from 'lucide-react'
import { api, ApiError, errorText } from '../api'
import { getLang, t } from '../i18n'
import { navigate } from '../router'
import { LoadError, useToast } from '../components/ui'
import { AgentIcon } from '../terminal/agentIcons'
import { quotaPlanLabel } from '../terminal/quotaPlan'
import { whenText } from './pageText'
import {
  formatCooldownShort,
  formatResetCountdown,
  isSubscriptionStale,
  providerDisplayName,
  providerIconProgram,
  refreshCooldownRemaining,
  sortSubscriptions,
  subscriptionHostsSummary,
  subscriptionWindowState,
  type SubQuotaWindow,
  type SubscriptionItem,
  type SubscriptionsListResponse,
} from '../terminal/subscriptions'

function WindowRow({ label, win, nowSec, stale }: { label: string; win: SubQuotaWindow; nowSec: number; stale: boolean }) {
  const lang = getLang()
  const st = subscriptionWindowState(win, nowSec, stale)
  const resetAtText = win.resets_at > 0 ? new Date(win.resets_at * 1000).toLocaleString() : ''
  let detail: string
  if (st.expired) {
    detail = t('subs.windowReset', { n: st.usedPct })
  } else if (win.resets_at > 0) {
    detail = t('subs.windowUsed', { n: st.usedPct, in: formatResetCountdown(win.resets_at, nowSec, lang) })
  } else {
    detail = t('subs.windowUsedNoReset', { n: st.usedPct })
  }
  const toneCls = st.tone ? ` is-${st.tone}` : ''
  return (
    <div className="subs-win">
      <div className="subs-win-head">
        <span className="subs-win-name">{label}</span>
        <span className={'subs-win-val tnum' + toneCls} title={resetAtText || undefined}>
          {detail}
        </span>
      </div>
      <div className={'subs-bar' + toneCls} aria-hidden="true">
        <span style={{ width: `${st.expired ? 0 : st.usedPct}%` }} />
      </div>
    </div>
  )
}

function SubscriptionCard({
  item,
  nowSec,
  refreshing,
  onRefresh,
}: {
  item: SubscriptionItem
  nowSec: number
  refreshing: boolean
  onRefresh: (item: SubscriptionItem) => void
}) {
  const lang = getLang()
  const stale = isSubscriptionStale(item.updated_at, nowSec)
  const plan = quotaPlanLabel(item.plan || item.quota.plan)
  const hosts = subscriptionHostsSummary(item)
  const cooldownLeft = refreshCooldownRemaining(item.next_refresh_at, nowSec)
  const hasGroups = (item.quota.groups?.length ?? 0) > 0
  const hasWindows = !!item.quota.five_hour || !!item.quota.weekly

  const updatedTitle = item.updated_at > 0 ? new Date(item.updated_at * 1000).toLocaleString() : ''
  const updatedWhen = item.updated_at > 0 ? whenText(item.updated_at, nowSec * 1000) : '—'

  return (
    <article className={'subs-card' + (stale ? ' is-stale' : '')}>
      <header className="subs-card-head">
        <div className="subs-id">
          <AgentIcon program={providerIconProgram(item.provider)} size={20} bare />
          <div className="subs-id-text">
            <div className="subs-id-line">
              <span className="subs-provider">{providerDisplayName(item.provider)}</span>
              {plan && <span className="subs-plan">{plan}</span>}
              {stale && (
                <span className="subs-badge is-stale" title={t('subs.staleHint')}>
                  <AlertTriangle size={12} aria-hidden="true" />
                  <span>{t('subs.staleBadge')}</span>
                </span>
              )}
            </div>
            {item.account_label && <span className="subs-account">{item.account_label}</span>}
          </div>
        </div>
        <div className="subs-act">
          {item.refresh_reason === 'passive_only' ? (
            <span className="subs-passive-note">{t('subs.passiveOnly')}</span>
          ) : (
            <button
              type="button"
              className="btn btn-sm subs-refresh-btn"
              disabled={refreshing || !item.can_refresh || cooldownLeft > 0}
              title={
                !item.can_refresh
                  ? t('subs.hostOfflineHint')
                  : cooldownLeft > 0
                    ? t('subs.cooldownWait', { d: formatCooldownShort(cooldownLeft, lang) })
                    : t('subs.refreshTip')
              }
              onClick={() => onRefresh(item)}
            >
              <RefreshCw size={13} className={refreshing ? 'spin' : undefined} aria-hidden="true" />
              <span>
                {refreshing
                  ? t('subs.refreshing')
                  : cooldownLeft > 0
                    ? formatCooldownShort(cooldownLeft, lang)
                    : t('subs.refresh')}
              </span>
            </button>
          )}
        </div>
      </header>

      {stale && <p className="subs-stale-note">{t('subs.staleNote', { at: updatedWhen })}</p>}

      <div className="subs-windows">
        {hasGroups ? (
          item.quota.groups!.map((g) => (
            <div key={g.name} className="subs-group">
              <div className="subs-group-name">{g.name}</div>
              {g.five_hour && <WindowRow label={t('quota.fiveHour')} win={g.five_hour} nowSec={nowSec} stale={stale} />}
              {g.weekly && <WindowRow label={t('quota.weekly')} win={g.weekly} nowSec={nowSec} stale={stale} />}
            </div>
          ))
        ) : hasWindows ? (
          <>
            {item.quota.five_hour && <WindowRow label={t('quota.fiveHour')} win={item.quota.five_hour} nowSec={nowSec} stale={stale} />}
            {item.quota.weekly && <WindowRow label={t('quota.weekly')} win={item.quota.weekly} nowSec={nowSec} stale={stale} />}
          </>
        ) : (
          <p className="hint">{t('subs.noWindows')}</p>
        )}
      </div>

      <footer className="subs-card-foot">
        <span className="subs-meta" title={updatedTitle}>
          {t('subs.updatedAt', { at: updatedWhen })}
        </span>
        {hosts.length > 0 && (
          <span className="subs-meta subs-hosts" title={hosts.join(', ')}>
            <Server size={12} aria-hidden="true" />
            <span>{t('subs.fromHost', { host: hosts.join(' · ') })}</span>
          </span>
        )}
        {item.refresh_reason === 'host_offline' && <span className="subs-meta subs-offline">{t('subs.hostOffline')}</span>}
      </footer>
    </article>
  )
}

export function SubscriptionsSection() {
  const toast = useToast()
  const [items, setItems] = useState<SubscriptionItem[] | null>(null)
  const [failed, setFailed] = useState(false)
  const [refreshingKey, setRefreshingKey] = useState('')
  const [nowSec, setNowSec] = useState(() => Math.floor(Date.now() / 1000))

  // Local clock tick only: redraws countdowns and relative timestamps with zero
  // network or provider calls.
  useEffect(() => {
    const id = window.setInterval(() => setNowSec(Math.floor(Date.now() / 1000)), 15000)
    return () => window.clearInterval(id)
  }, [])

  // Read cached snapshots once on mount. Never triggers provider API calls.
  const load = useCallback(async () => {
    setFailed(false)
    try {
      const res = await api<SubscriptionsListResponse>('GET', '/api/subscriptions')
      setItems(res.items ?? [])
      setNowSec(Math.floor(Date.now() / 1000))
    } catch {
      setFailed(true)
    }
  }, [])

  useEffect(() => {
    void load()
  }, [load])

  const sorted = useMemo(() => (items ? sortSubscriptions(items, nowSec) : []), [items, nowSec])

  const onRefresh = async (item: SubscriptionItem) => {
    const key = `${item.provider}\0${item.account_key}`
    if (refreshingKey) return
    setRefreshingKey(key)
    try {
      const res = await api<{ item: SubscriptionItem; server_time: number }>('POST', '/api/subscriptions/refresh', {
        provider: item.provider,
        account_key: item.account_key,
      }, { timeoutMs: 40000 })
      setItems((prev) =>
        (prev ?? []).map((cur) =>
          cur.provider === item.provider && cur.account_key === item.account_key ? res.item : cur,
        ),
      )
      setNowSec(Math.floor(Date.now() / 1000))
      toast(t('subs.refreshed'))
    } catch (err) {
      if (err instanceof ApiError && err.code === 'rate_limited') {
        const extra = err.extra as { retry_after?: number; next_refresh_at?: number } | undefined
        if (extra?.next_refresh_at) {
          setItems((prev) =>
            (prev ?? []).map((cur) =>
              cur.provider === item.provider && cur.account_key === item.account_key
                ? { ...cur, next_refresh_at: extra.next_refresh_at }
                : cur,
            ),
          )
        }
        const wait = formatCooldownShort(extra?.retry_after ?? 60, getLang())
        toast(t('subs.cooldownWait', { d: wait }), true)
      } else if (err instanceof ApiError && err.code === 'host_offline') {
        toast(t('subs.hostOfflineHint'), true)
      } else {
        toast(errorText(err), true)
      }
    } finally {
      setRefreshingKey('')
    }
  }

  return (
    <section className="section subs-section">
      <div className="subs-head">
        <h2>{t('subs.title')}</h2>
        <p className="hint">{t('subs.intro')}</p>
      </div>
      {failed ? (
        <LoadError text={t('common.loadFailed')} onRetry={() => void load()} />
      ) : items === null ? (
        <p className="loading">{t('common.loading')}</p>
      ) : sorted.length === 0 ? (
        <div className="subs-empty">
          <p className="subs-empty-title">{t('subs.emptyTitle')}</p>
          <p className="hint">{t('subs.emptyHint')}</p>
        </div>
      ) : (
        <div className="subs-list">
          {sorted.map((item) => (
            <SubscriptionCard
              key={`${item.provider}\0${item.account_key}`}
              item={item}
              nowSec={nowSec}
              refreshing={refreshingKey === `${item.provider}\0${item.account_key}`}
              onRefresh={onRefresh}
            />
          ))}
        </div>
      )}
    </section>
  )
}

export function SubscriptionsPage() {
  return (
    <main className="page page-form page-subscriptions">
      <div className="page-head">
        <button type="button" className="icon-btn phone-only" onClick={() => navigate('/settings')} aria-label={t('common.back')}>
          <ChevronLeft size={18} aria-hidden="true" />
        </button>
        <h1>{t('subs.title')}</h1>
      </div>
      <SubscriptionsSection />
    </main>
  )
}
