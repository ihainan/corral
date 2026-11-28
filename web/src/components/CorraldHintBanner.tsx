import { createContext, useCallback, useContext, useEffect, useLayoutEffect, useRef, useState, type ReactNode } from 'react'
import { X } from 'lucide-react'
import { ApiError, errorText } from '../api'
import { useSession } from '../app'
import {
  CORRALD_HINTS_EVENT,
  cancelHostCorrald,
  dismissCorraldHint,
  getCorraldHint,
  getHostCorrald,
  installHostCorrald,
  markCorraldHintShown,
  publishHostCorrald,
  readCachedCorrald,
  subscribeAdminCorraldPolicy,
  subscribeHostCorrald,
  triggerCorraldHintsCheck,
  type CorraldHint,
  type CorraldHintSync,
  type CorraldHintSyncSnapshot,
  type CorraldStatus,
} from '../corraldApi'
import {
  corraldHintMessageKey,
  corraldSectionModel,
  createCorraldHintSync,
  detectCorraldTransition,
  isCorraldStatusHintEligible,
  shouldPollCorrald,
  shouldShowCorraldHint,
} from '../corraldLogic'
import { t } from '../i18n'
import { usePrefs } from '../prefs'
import { navigate } from '../router'
import { CorraldConsentAlert } from './CorraldDialogs'
import { useToast } from './ui'

export const CorraldSideBannerContext = createContext<ReactNode>(null)

export function useCorraldSideBanner(): ReactNode {
  return useContext(CorraldSideBannerContext)
}

export function CorraldHintBanner({
  hint,
  active,
  busy,
  onMountShown,
  onUnmount,
  onEnable,
  onDismiss,
  onClose,
}: {
  hint: CorraldHint
  active: boolean
  busy?: boolean
  onMountShown: (hostId: number, generation: number) => void
  onUnmount: (hostId: number) => void
  onEnable: (hint: CorraldHint) => void
  onDismiss: (hint: CorraldHint) => void
  onClose: (hint: CorraldHint) => void
}) {
  const hostId = hint.host_id
  const generation = hint.generation ?? 0
  useEffect(() => {
    if (!active || hostId <= 0) return
    onMountShown(hostId, generation)
    return () => {
      onUnmount(hostId)
    }
  }, [active, hostId, generation, onMountShown, onUnmount])

  const hostName = hint.host_name || `#${hint.host_id}`
  const msg = t(corraldHintMessageKey(hint), { host: hostName, n: hint.count })

  return (
    <div className="convo-corrald-hint" role="region" aria-label={t('corrald.hint.regionLabel')}>
      <div className="convo-corrald-hint-head">
        <p className="convo-corrald-hint-text">{msg}</p>
        <button
          type="button"
          className="convo-corrald-hint-close"
          aria-label={t('corrald.hint.close')}
          title={t('corrald.hint.close')}
          onClick={() => onClose(hint)}
        >
          <X size={14} strokeWidth={1.75} aria-hidden="true" />
        </button>
      </div>
      <div className="convo-corrald-hint-acts">
        <button
          type="button"
          className="convo-corrald-hint-btn is-primary"
          disabled={busy}
          onClick={() => onEnable(hint)}
        >
          {t('corrald.hint.enable')}
        </button>
        <button
          type="button"
          className="convo-corrald-hint-btn"
          disabled={busy}
          onClick={() => onDismiss(hint)}
        >
          {t('corrald.hint.dismiss')}
        </button>
      </div>
    </div>
  )
}

const HINT_POLL_MS = 30_000

const EMPTY_SYNC_SNAPSHOT: CorraldHintSyncSnapshot = {
  generation: 0,
  shownHostIds: new Set<number>(),
  hiddenHostIds: new Set<number>(),
  pending: [],
}

// Owns fetching the single contextual corrald hint, persisting & retrying
// shown/dismissed writes across reloads, reacting immediately to host/admin
// corrald status updates, and running the shared CorraldConsentAlert flow.
export function useCorraldHintController(active: boolean): {
  banner: ReactNode
  dialog: ReactNode
} {
  const { me } = useSession()
  const userId = me?.id
  const { corraldHints: masterEnabled } = usePrefs()
  const toast = useToast()

  const [hint, setHint] = useState<CorraldHint | null>(null)
  const [syncSnap, setSyncSnap] = useState<CorraldHintSyncSnapshot>(EMPTY_SYNC_SNAPSHOT)
  const [currentlyDisplayedHostId, setCurrentlyDisplayedHostId] = useState<number | null>(null)
  const [knownHostStatus, setKnownHostStatus] = useState<{ status: CorraldStatus | null; unavailable?: boolean } | null>(null)
  const [adminDisabledLocally, setAdminDisabledLocally] = useState(false)

  const mountCountRef = useRef<Map<number, number>>(new Map())
  const syncRef = useRef<CorraldHintSync | null>(null)
  if (!syncRef.current) {
    syncRef.current = createCorraldHintSync({
      markShown: (hostId, gen) => markCorraldHintShown(hostId, gen),
      dismiss: (hostId, gen) => dismissCorraldHint(hostId, gen),
      isPermanentError: (err) => err instanceof ApiError && (err.status === 400 || err.status === 404),
      onChange: (snap) => setSyncSnap(snap),
    })
  }

  // Shared consent/install dialog state (reusing CorraldConsentAlert).
  const [target, setTarget] = useState<{ hostId: number; hostName: string } | null>(null)
  const [status, setStatus] = useState<CorraldStatus | null>(null)
  const [loadingStatus, setLoadingStatus] = useState(false)
  const [busy, setBusy] = useState(false)
  const [cancelling, setCancelling] = useState(false)
  const [installFailed, setInstallFailed] = useState<{
    code: string
    detail?: string
    message?: string
    os?: string
    machineId?: string
  } | null>(null)
  const prevStatusRef = useRef<CorraldStatus | null>(null)
  const targetRef = useRef(target)
  targetRef.current = target
  const userIdRef = useRef(userId)
  userIdRef.current = userId

  // Reset per-account state cleanly when switching or signing out of an account.
  useLayoutEffect(() => {
    mountCountRef.current.clear()
    setHint(null)
    setCurrentlyDisplayedHostId(null)
    setKnownHostStatus(null)
    setAdminDisabledLocally(false)
    setTarget(null)
    setStatus(null)
    setInstallFailed(null)
    prevStatusRef.current = null
    syncRef.current?.setUser(userId)
  }, [userId])

  useEffect(() => {
    const sync = syncRef.current
    return () => {
      sync?.dispose()
    }
  }, [])

  // Clear visible hint immediately when account-level master switch is turned off.
  useEffect(() => {
    if (!masterEnabled) {
      setHint(null)
      setCurrentlyDisplayedHostId(null)
    }
  }, [masterEnabled])

  // Track admin policy changes immediately without waiting for the next poll.
  useEffect(() => {
    return subscribeAdminCorraldPolicy((disabled) => {
      setAdminDisabledLocally(disabled)
      if (disabled) {
        setHint(null)
        setCurrentlyDisplayedHostId(null)
      }
    })
  }, [])

  // Subscribe to live CorraldStatus updates for the hinted host so enabling,
  // installing, or admin-disabling it from any entry hides the bar immediately.
  const hintHostId = hint?.host_id ?? 0
  useEffect(() => {
    if (hintHostId <= 0) {
      setKnownHostStatus(null)
      return
    }
    const cached = readCachedCorrald(hintHostId)
    if (cached) {
      setKnownHostStatus(cached)
      if (cached.unavailable || (cached.status && !isCorraldStatusHintEligible(cached.status))) {
        setHint((cur) => (cur && cur.host_id === hintHostId ? null : cur))
        setCurrentlyDisplayedHostId((cur) => (cur === hintHostId ? null : cur))
      }
    } else {
      setKnownHostStatus(null)
    }
    return subscribeHostCorrald(hintHostId, (st, unavailable) => {
      setKnownHostStatus({ status: st, unavailable })
      if (unavailable || (st && !isCorraldStatusHintEligible(st))) {
        setHint((cur) => (cur && cur.host_id === hintHostId ? null : cur))
        setCurrentlyDisplayedHostId((cur) => (cur === hintHostId ? null : cur))
      }
    })
  }, [hintHostId])

  const fetchHint = useCallback(
    async (signal?: AbortSignal) => {
      const reqUser = userIdRef.current
      if (!reqUser || !masterEnabled) return
      try {
        const res = await getCorraldHint(signal)
        if (userIdRef.current !== reqUser || signal?.aborted) return
        const sync = syncRef.current!
        sync.syncServerGeneration(res.generation)
        if (res.hint) {
          setAdminDisabledLocally(false)
          setHint({ ...res.hint, generation: res.hint.generation ?? res.generation })
        } else {
          setHint(null)
        }
      } catch (e) {
        if (e instanceof ApiError && (e.code === 'aborted' || e.status === 404)) return
      }
    },
    [masterEnabled],
  )

  useEffect(() => {
    if (!active || !userId || !masterEnabled) return
    let stopped = false
    let ctl: AbortController | null = null

    const run = () => {
      if (stopped || document.visibilityState !== 'visible') return
      ctl?.abort()
      ctl = new AbortController()
      void fetchHint(ctl.signal)
    }

    run()
    const timer = window.setInterval(run, HINT_POLL_MS)
    const onRefresh = () => {
      run()
    }
    const onOnline = () => {
      void syncRef.current?.flushPending()
      run()
    }
    const onVis = () => {
      if (document.visibilityState === 'visible') run()
    }
    window.addEventListener(CORRALD_HINTS_EVENT, onRefresh)
    window.addEventListener('online', onOnline)
    document.addEventListener('visibilitychange', onVis)
    return () => {
      stopped = true
      ctl?.abort()
      window.clearInterval(timer)
      window.removeEventListener(CORRALD_HINTS_EVENT, onRefresh)
      window.removeEventListener('online', onOnline)
      document.removeEventListener('visibilitychange', onVis)
    }
  }, [active, userId, masterEnabled, fetchHint])

  const hideLocally = useCallback((hostId: number) => {
    syncRef.current?.hideLocally(hostId)
    setCurrentlyDisplayedHostId((cur) => (cur === hostId ? null : cur))
    setHint((cur) => (cur && cur.host_id === hostId ? null : cur))
  }, [])

  const handleMountShown = useCallback((hostId: number, gen: number) => {
    const counts = mountCountRef.current
    counts.set(hostId, (counts.get(hostId) ?? 0) + 1)
    setCurrentlyDisplayedHostId(hostId)
    syncRef.current?.recordShown(hostId, gen)
  }, [])

  const handleUnmount = useCallback((hostId: number) => {
    const counts = mountCountRef.current
    const next = Math.max(0, (counts.get(hostId) ?? 1) - 1)
    if (next === 0) {
      counts.delete(hostId)
      // Defer via microtask so React StrictMode's synchronous dev remount keeps
      // currentlyDisplayedHostId intact while real list unmounts (mobile list ->
      // conversation -> back, or picker close -> reopen) clear it cleanly.
      queueMicrotask(() => {
        if ((mountCountRef.current.get(hostId) ?? 0) === 0) {
          setCurrentlyDisplayedHostId((cur) => (cur === hostId ? null : cur))
        }
      })
    } else {
      counts.set(hostId, next)
    }
  }, [])

  const handleClose = useCallback(
    (h: CorraldHint) => {
      // Closing hides locally (does not send dismiss); shown_at is already
      // recorded & persisted by handleMountShown.
      hideLocally(h.host_id)
    },
    [hideLocally],
  )

  const handleDismiss = useCallback((h: CorraldHint) => {
    const gen = h.generation ?? syncRef.current?.getSnapshot().generation ?? 0
    setCurrentlyDisplayedHostId((cur) => (cur === h.host_id ? null : cur))
    setHint((cur) => (cur && cur.host_id === h.host_id ? null : cur))
    syncRef.current?.recordDismiss(h.host_id, gen)
  }, [])

  const applyStatus = useCallback(
    (hostId: number, hostName: string, next: CorraldStatus | null) => {
      if (next === null) {
        publishHostCorrald(hostId, null, true)
        setTarget(null)
        setStatus(null)
        return
      }
      const prev = prevStatusRef.current
      prevStatusRef.current = next
      setStatus(next)
      publishHostCorrald(hostId, next, false)

      const tr = detectCorraldTransition(prev, next)
      if (!tr) return
      if (tr.type === 'enabled') {
        setTarget(null)
        setInstallFailed(null)
        toast.notice({
          title: t('corrald.note.enabledTitle'),
          body:
            typeof tr.agentsCount === 'number'
              ? t('corrald.note.enabledBody', { host: hostName, n: tr.agentsCount })
              : t('corrald.note.enabledBodySimple', { host: hostName }),
          action: { label: t('corrald.note.view'), run: () => navigate('/agents') },
        })
      } else if (tr.type === 'install_failed') {
        if (targetRef.current?.hostId === hostId) {
          setInstallFailed({
            code: tr.code,
            detail: tr.detail,
            message: next.error?.message,
            os: next.error?.os,
            machineId: next.machine_id ?? next.error?.machine_id,
          })
        }
      } else if (tr.type === 'install_cancelled') {
        setTarget(null)
        setInstallFailed(null)
        toast(t('corrald.cancelled'))
      }
    },
    [toast],
  )

  const refreshStatus = useCallback(
    async (hostId: number, hostName: string, signal?: AbortSignal) => {
      try {
        const s = await getHostCorrald(hostId, signal)
        if (targetRef.current?.hostId !== hostId) return
        applyStatus(hostId, hostName, s)
      } catch (e) {
        if (e instanceof ApiError && e.code === 'aborted') return
      }
    },
    [applyStatus],
  )

  const handleEnable = useCallback(
    async (h: CorraldHint) => {
      if (loadingStatus) return
      const hostId = h.host_id
      const hostName = h.host_name || `#${hostId}`
      setInstallFailed(null)
      setLoadingStatus(true)
      triggerCorraldHintsCheck()
      try {
        const cached = readCachedCorrald(hostId)
        const s = cached && !cached.unavailable && cached.status ? cached.status : await getHostCorrald(hostId)
        if (!s || !isCorraldStatusHintEligible(s)) {
          publishHostCorrald(hostId, s, s === null)
          hideLocally(hostId)
          return
        }
        prevStatusRef.current = s
        setStatus(s)
        setTarget({ hostId, hostName })
      } catch (e) {
        toast(errorText(e), true)
      } finally {
        setLoadingStatus(false)
      }
    },
    [loadingStatus, hideLocally, toast],
  )

  const shouldPoll = target !== null && shouldPollCorrald(status)
  useEffect(() => {
    if (!shouldPoll || !target) return
    const { hostId, hostName } = target
    let stopped = false
    let timer: ReturnType<typeof setTimeout> | undefined
    let ctl: AbortController | null = null

    const schedule = () => {
      if (stopped || document.visibilityState !== 'visible') return
      timer = setTimeout(() => {
        if (stopped || document.visibilityState !== 'visible') return
        ctl = new AbortController()
        void refreshStatus(hostId, hostName, ctl.signal).finally(() => {
          if (!stopped) schedule()
        })
      }, 1000)
    }

    const onVis = () => {
      if (document.visibilityState === 'visible') {
        clearTimeout(timer)
        ctl?.abort()
        ctl = new AbortController()
        void refreshStatus(hostId, hostName, ctl.signal).finally(() => {
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
  }, [shouldPoll, target, refreshStatus])

  const doInstall = async () => {
    if (!target || !status || busy) return
    const { hostId, hostName } = target
    setBusy(true)
    setInstallFailed(null)
    try {
      const op = await installHostCorrald(hostId, {
        ...(status.manifest_hash ? { manifest_hash: status.manifest_hash } : {}),
      })
      hideLocally(hostId)
      if (op) {
        applyStatus(hostId, hostName, { ...status, state: 'installing', op, error: null })
      }
      await refreshStatus(hostId, hostName)
    } catch (e) {
      const code = e instanceof ApiError ? e.code : 'other'
      setInstallFailed({ code, message: errorText(e) })
    } finally {
      setBusy(false)
    }
  }

  const doCancel = async () => {
    if (!target || cancelling) return
    const { hostId, hostName } = target
    setCancelling(true)
    try {
      await cancelHostCorrald(hostId)
      await refreshStatus(hostId, hostName)
    } catch (e) {
      toast(errorText(e), true)
    } finally {
      setCancelling(false)
    }
  }

  const visibleHint =
    !adminDisabledLocally &&
    shouldShowCorraldHint(hint, {
      masterEnabled,
      sessionHiddenHostIds: syncSnap.hiddenHostIds,
      shownHostIds: syncSnap.shownHostIds,
      currentlyDisplayedHostId,
      hostStatus: knownHostStatus?.status,
      hostUnavailable: knownHostStatus?.unavailable,
    })
      ? hint
      : null

  const banner = visibleHint ? (
    <CorraldHintBanner
      hint={visibleHint}
      active={active}
      busy={loadingStatus}
      onMountShown={handleMountShown}
      onUnmount={handleUnmount}
      onEnable={(h) => void handleEnable(h)}
      onDismiss={handleDismiss}
      onClose={handleClose}
    />
  ) : null

  const model = status ? corraldSectionModel(status) : null
  const dialog =
    target && status ? (
      <CorraldConsentAlert
        hostName={target.hostName}
        status={status}
        progress={model?.mode === 'installing' ? model.progress : null}
        busy={busy}
        cancelling={cancelling}
        failedError={installFailed}
        onConfirm={() => void doInstall()}
        onCancelInstall={() => void doCancel()}
        onClose={() => {
          setTarget(null)
          setInstallFailed(null)
        }}
      />
    ) : null

  return { banner, dialog }
}
