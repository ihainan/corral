import { useCallback, useEffect, useRef, useState } from 'react'
import { ApiError } from '../api'
import {
  getHostCorrald,
  publishHostCorrald,
  readCachedCorrald,
  subscribeHostCorrald,
  triggerCorraldNoticesCheck,
  type CorraldStatus,
} from '../corraldApi'
import { corraldStatusBarModel, detectCorraldTransition, shouldPollCorrald } from '../corraldLogic'
import { t } from '../i18n'
import { navigate } from '../router'
import { StatusItem } from '../workbench'
import { useToast } from './ui'

// Deduplicates one-time lost/error notices across re-renders per session.
const notifiedOnce = new Set<string>()

export function CorraldStatusBarItem({ hostId }: { hostId: number; hostName?: string }) {
  const toast = useToast()
  const [status, setStatus] = useState<CorraldStatus | null>(() => readCachedCorrald(hostId)?.status ?? null)
  const prevRef = useRef<CorraldStatus | null>(status)

  const handleStatus = useCallback(
    (next: CorraldStatus | null) => {
      const prev = prevRef.current
      prevRef.current = next
      setStatus(next)
      if (!next) return
      const tr = detectCorraldTransition(prev, next)
      if (tr?.type === 'updated') {
        triggerCorraldNoticesCheck()
      } else if (tr?.type === 'lost') {
        const key = `${hostId}:lost`
        if (!notifiedOnce.has(key)) {
          notifiedOnce.add(key)
          toast(t('corrald.state.lostDesc'), true)
        }
      } else if (tr?.type === 'error') {
        const key = `${hostId}:error:${tr.code}`
        if (!notifiedOnce.has(key)) {
          notifiedOnce.add(key)
          toast(t('corrald.state.errorDescGeneric'), true)
        }
      }
    },
    [hostId, toast],
  )

  const fetchOnce = useCallback(
    async (signal?: AbortSignal) => {
      try {
        const s = await getHostCorrald(hostId, signal)
        publishHostCorrald(hostId, s, s === null)
        handleStatus(s)
      } catch (e) {
        if (e instanceof ApiError && e.code === 'aborted') return
      }
    },
    [hostId, handleStatus],
  )

  useEffect(() => {
    const cached = readCachedCorrald(hostId)
    prevRef.current = cached?.status ?? null
    setStatus(cached?.status ?? null)
    const unsub = subscribeHostCorrald(hostId, (s) => handleStatus(s))
    if (cached?.unavailable) return unsub
    const ctl = new AbortController()
    if (!cached) void fetchOnce(ctl.signal)
    return () => {
      unsub()
      ctl.abort()
    }
  }, [hostId, fetchOnce, handleStatus])

  // Poll at 1 s only while an active operation (e.g. updating) is running and tab is visible.
  const poll = shouldPollCorrald(status)
  useEffect(() => {
    if (!poll) return
    let stopped = false
    let timer: ReturnType<typeof setTimeout> | undefined
    let ctl: AbortController | null = null

    const schedule = () => {
      if (stopped || document.visibilityState !== 'visible') return
      timer = setTimeout(() => {
        if (stopped || document.visibilityState !== 'visible') return
        ctl = new AbortController()
        void fetchOnce(ctl.signal).finally(() => {
          if (!stopped) schedule()
        })
      }, 1000)
    }

    const onVis = () => {
      if (document.visibilityState === 'visible') {
        clearTimeout(timer)
        ctl?.abort()
        ctl = new AbortController()
        void fetchOnce(ctl.signal).finally(() => {
          if (!stopped) schedule()
        })
      } else {
        clearTimeout(timer)
        ctl?.abort()
      }
    }

    schedule()
    document.addEventListener('visibilitychange', onVis)
    return () => {
      stopped = true
      clearTimeout(timer)
      ctl?.abort()
      document.removeEventListener('visibilitychange', onVis)
    }
  }, [poll, fetchOnce])

  const model = corraldStatusBarModel(status)
  if (!model) return null

  const label =
    model.kind === 'updating'
      ? model.version
        ? t('corrald.statusBarUpdating', { version: model.version })
        : t('corrald.statusBarUpdatingNoVer')
      : model.version
        ? t('corrald.statusBar', { version: model.version })
        : t('corrald.statusBarNoVer')

  return (
    <StatusItem
      onClick={() => navigate(`/hosts/${hostId}/edit`)}
      title={t('corrald.statusBarHint')}
      tone={model.degraded ? 'warn' : undefined}
    >
      {label}
    </StatusItem>
  )
}
