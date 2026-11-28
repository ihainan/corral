import { createContext, useCallback, useContext, useEffect, useLayoutEffect, useRef, useState, type ReactNode } from 'react'
import { X } from 'lucide-react'
import { ApiError, errorText } from '../api'
import { useSession } from '../app'
import {
  CORRALD_HINTS_EVENT,
  cancelHostCorrald,
  claimCorraldHintShown,
  dismissCorraldHint,
  getCorraldHint,
  getHostCorrald,
  installHostCorrald,
  publishHostCorrald,
  readCachedCorrald,
  subscribeAdminCorraldPolicy,
  subscribeHostCorrald,
  triggerCorraldHintsCheck,
  type CorraldHint,
  type CorraldHintController,
  type CorraldHintControllerState,
  type CorraldStatus,
} from '../corraldApi'
import {
  corraldHintMessageKey,
  corraldSectionModel,
  createCorraldHintController,
  detectCorraldTransition,
  isCorraldStatusHintEligible,
  shouldPollCorrald,
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
  busy,
  onEnable,
  onDismiss,
  onClose,
}: {
  hint: CorraldHint
  busy?: boolean
  onEnable: (hint: CorraldHint) => void
  onDismiss: (hint: CorraldHint) => void
  onClose: (hint: CorraldHint) => void
}) {
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

// Mounted inside AgentList only when the list surface is visible. Triggers the
// server-side atomic claim when mounted and renders nothing until claimed:true.
function CorraldHintSlot({
  active,
  displayedHint,
  busy,
  onSlotMounted,
  onSlotUnmounted,
  onEnable,
  onDismiss,
  onClose,
}: {
  active: boolean
  displayedHint: CorraldHint | null
  busy?: boolean
  onSlotMounted: () => void
  onSlotUnmounted: () => void
  onEnable: (hint: CorraldHint) => void
  onDismiss: (hint: CorraldHint) => void
  onClose: (hint: CorraldHint) => void
}) {
  useEffect(() => {
    if (!active) return
    onSlotMounted()
    return () => {
      onSlotUnmounted()
    }
  }, [active, onSlotMounted, onSlotUnmounted])

  if (!displayedHint) return null
  return (
    <CorraldHintBanner
      hint={displayedHint}
      busy={busy}
      onEnable={onEnable}
      onDismiss={onDismiss}
      onClose={onClose}
    />
  )
}

const HINT_POLL_MS = 30_000

const EMPTY_CONTROLLER_STATE: CorraldHintControllerState = {
  displayedHint: null,
  candidateHint: null,
  pendingDismissCount: 0,
}

// Owns fetching the single contextual corrald hint, claiming it atomically on
// the server before rendering, retrying dismiss in memory with backoff,
// reacting immediately to host/admin corrald status updates, and running the
// shared CorraldConsentAlert flow.
export function useCorraldHintController(active: boolean): {
  banner: ReactNode
  dialog: ReactNode
} {
  const { me } = useSession()
  const userId = me?.id
  const { corraldHints: masterEnabled } = usePrefs()
  const toast = useToast()

  const [ctlState, setCtlState] = useState<CorraldHintControllerState>(EMPTY_CONTROLLER_STATE)

  const ctlRef = useRef<CorraldHintController | null>(null)
  if (!ctlRef.current) {
    ctlRef.current = createCorraldHintController({
      claimShown: (hostId, gen) => claimCorraldHintShown(hostId, gen),
      dismiss: (hostId, gen) => dismissCorraldHint(hostId, gen),
      readCachedStatus: (hostId) => readCachedCorrald(hostId),
      isPermanentError: (err) => err instanceof ApiError && (err.status === 400 || err.status === 404),
      onChange: (next) => setCtlState(next),
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
  const policySeqRef = useRef(0)

  // Reset per-account state cleanly when switching or signing out of an account.
  useLayoutEffect(() => {
    setTarget(null)
    setStatus(null)
    setInstallFailed(null)
    prevStatusRef.current = null
    ctlRef.current?.setUser(userId)
  }, [userId])

  // Sync account-level master switch immediately.
  useLayoutEffect(() => {
    ctlRef.current?.setMasterEnabled(masterEnabled)
  }, [masterEnabled])

  useEffect(() => {
    const ctl = ctlRef.current
    return () => {
      ctl?.dispose()
    }
  }, [])

  // Track admin policy changes immediately without waiting for the next poll.
  useEffect(() => {
    return subscribeAdminCorraldPolicy((disabled) => {
      policySeqRef.current++
      ctlRef.current?.setAdminDisabled(disabled)
    })
  }, [])

  // Subscribe to live CorraldStatus updates for the candidate or displayed host
  // so enabling, installing, or admin-disabling it from any entry cancels/hides
  // the hint immediately.
  const watchedHostId = ctlState.displayedHint?.host_id ?? ctlState.candidateHint?.host_id ?? 0
  useEffect(() => {
    if (watchedHostId <= 0) return
    const cached = readCachedCorrald(watchedHostId)
    if (cached) {
      ctlRef.current?.onHostStatus(watchedHostId, cached.status, cached.unavailable)
    }
    return subscribeHostCorrald(watchedHostId, (st, unavailable) => {
      ctlRef.current?.onHostStatus(watchedHostId, st, unavailable)
    })
  }, [watchedHostId])

  const fetchHint = useCallback(
    async (signal?: AbortSignal) => {
      const reqUser = userIdRef.current
      if (!reqUser || !masterEnabled) return
      const startPolicySeq = policySeqRef.current
      try {
        const res = await getCorraldHint(signal)
        if (userIdRef.current !== reqUser || signal?.aborted) return
        if (policySeqRef.current !== startPolicySeq) return
        if (res.hint) {
          ctlRef.current?.setAdminDisabled(false)
        }
        ctlRef.current?.receiveServerHint(res.hint, res.generation)
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
      void ctlRef.current?.flushPendingDismisses()
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

  const handleSlotMounted = useCallback(() => {
    ctlRef.current?.onSlotMounted()
  }, [])

  const handleSlotUnmounted = useCallback(() => {
    ctlRef.current?.onSlotUnmounted()
  }, [])

  const handleClose = useCallback((_h: CorraldHint) => {
    // Closing with X needs no extra write: shown_at is already recorded on the
    // server from the atomic claim that allowed this banner to render.
    ctlRef.current?.closeDisplayed()
  }, [])

  const handleDismiss = useCallback((h: CorraldHint) => {
    ctlRef.current?.dismiss(h)
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
          ctlRef.current?.hideHost(hostId)
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
    [loadingStatus, toast],
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
      ctlRef.current?.hideHost(hostId)
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

  const banner =
    ctlState.displayedHint || ctlState.candidateHint ? (
      <CorraldHintSlot
        key={userId ?? 0}
        active={active}
        displayedHint={ctlState.displayedHint}
        busy={loadingStatus}
        onSlotMounted={handleSlotMounted}
        onSlotUnmounted={handleSlotUnmounted}
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
