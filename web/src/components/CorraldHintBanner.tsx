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
  type CorraldHint,
  type CorraldStatus,
} from '../corraldApi'
import {
  corraldHintMessageKey,
  corraldSectionModel,
  detectCorraldTransition,
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
  onShown,
  onEnable,
  onDismiss,
  onClose,
}: {
  hint: CorraldHint
  active: boolean
  busy?: boolean
  onShown: (hostId: number) => void
  onEnable: (hint: CorraldHint) => void
  onDismiss: (hint: CorraldHint) => void
  onClose: (hint: CorraldHint) => void
}) {
  const hostId = hint.host_id
  useEffect(() => {
    if (!active || hostId <= 0) return
    onShown(hostId)
  }, [active, hostId, onShown])

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

// Owns fetching the single contextual corrald hint, reporting shown/dismissed,
// and running the shared CorraldConsentAlert enablement flow right from the
// Agents page without duplicating consent/install UI.
export function useCorraldHintController(active: boolean): {
  banner: ReactNode
  dialog: ReactNode
} {
  const { me } = useSession()
  const userId = me?.id
  const { corraldHints: masterEnabled } = usePrefs()
  const toast = useToast()

  const [hint, setHint] = useState<CorraldHint | null>(null)
  const [sessionHidden, setSessionHidden] = useState<Set<number>>(() => new Set())
  const reportedShownRef = useRef<Set<number>>(new Set())
  const prevMasterRef = useRef<boolean>(masterEnabled)

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
    setHint(null)
    setSessionHidden(new Set())
    reportedShownRef.current.clear()
    setTarget(null)
    setStatus(null)
    setInstallFailed(null)
    prevStatusRef.current = null
  }, [userId])

  // When the user turns the account-level switch back ON, clear session-local
  // hide/shown tracking so newly restored hosts can be shown immediately.
  useEffect(() => {
    const prev = prevMasterRef.current
    prevMasterRef.current = masterEnabled
    if (!masterEnabled) {
      setHint(null)
      return
    }
    if (!prev && masterEnabled) {
      setSessionHidden(new Set())
      reportedShownRef.current.clear()
    }
  }, [masterEnabled])

  const fetchHint = useCallback(
    async (signal?: AbortSignal) => {
      const reqUser = userIdRef.current
      if (!reqUser || !masterEnabled) return
      try {
        const next = await getCorraldHint(signal)
        if (userIdRef.current !== reqUser || signal?.aborted) return
        setHint(next)
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
    const onRefresh = (ev: Event) => {
      const detail = (ev as CustomEvent<{ resetSession?: boolean } | undefined>).detail
      if (detail?.resetSession) {
        setSessionHidden(new Set())
        reportedShownRef.current.clear()
      }
      run()
    }
    const onVis = () => {
      if (document.visibilityState === 'visible') run()
    }
    window.addEventListener(CORRALD_HINTS_EVENT, onRefresh)
    document.addEventListener('visibilitychange', onVis)
    return () => {
      stopped = true
      ctl?.abort()
      window.clearInterval(timer)
      window.removeEventListener(CORRALD_HINTS_EVENT, onRefresh)
      document.removeEventListener('visibilitychange', onVis)
    }
  }, [active, userId, masterEnabled, fetchHint])

  const hideForSession = useCallback((hostId: number) => {
    setSessionHidden((prev) => {
      if (prev.has(hostId)) return prev
      const next = new Set(prev)
      next.add(hostId)
      return next
    })
    setHint((cur) => (cur && cur.host_id === hostId ? null : cur))
  }, [])

  const handleShown = useCallback((hostId: number) => {
    if (reportedShownRef.current.has(hostId)) return
    reportedShownRef.current.add(hostId)
    void markCorraldHintShown(hostId).catch(() => {})
  }, [])

  const handleClose = useCallback(
    (h: CorraldHint) => {
      // Closing hides only for this session (does not mark dismissed), while
      // shown_at was already recorded when the banner appeared.
      hideForSession(h.host_id)
    },
    [hideForSession],
  )

  const handleDismiss = useCallback(
    (h: CorraldHint) => {
      hideForSession(h.host_id)
      void dismissCorraldHint(h.host_id).catch(() => {})
    },
    [hideForSession],
  )

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
      try {
        const cached = readCachedCorrald(hostId)
        const s = cached && !cached.unavailable && cached.status ? cached.status : await getHostCorrald(hostId)
        if (!s || s.admin_disabled || s.state !== 'off') {
          hideForSession(hostId)
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
    [loadingStatus, hideForSession, toast],
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
      hideForSession(hostId)
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

  const visibleHint = shouldShowCorraldHint(hint, { masterEnabled, sessionHiddenHostIds: sessionHidden }) ? hint : null

  const banner = visibleHint ? (
    <CorraldHintBanner
      hint={visibleHint}
      active={active}
      busy={loadingStatus}
      onShown={handleShown}
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
