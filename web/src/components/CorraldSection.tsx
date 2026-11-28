import { useCallback, useEffect, useRef, useState } from 'react'
import { TriangleAlert } from 'lucide-react'
import { ApiError, errorText } from '../api'
import {
  cancelHostCorrald,
  getHostCorrald,
  installHostCorrald,
  pauseHostCorrald,
  publishHostCorrald,
  removeHostCorrald,
  resumeHostCorrald,
  retryHostCorrald,
  triggerCorraldHintsCheck,
  triggerCorraldNoticesCheck,
  updateHostCorrald,
  type CorraldStatus,
} from '../corraldApi'
import {
  buildDiagnostics,
  corraldSectionModel,
  detectCorraldTransition,
  formatPerfSeconds,
  shouldPollCorrald,
  type CorraldRemoveOutcome,
} from '../corraldLogic'
import { hasKey, relTime, t, type I18nKey } from '../i18n'
import { navigate } from '../router'
import { copyText, useToast } from './ui'
import {
  CorraldChangelogDialog,
  CorraldCommandBox,
  CorraldConsentAlert,
  CorraldReadmeDialog,
  CorraldRemoveAlert,
  CorraldScopeAlert,
  corraldFailureText,
  corraldStepLabel,
  formatCapName,
  formatScopeItem,
} from './CorraldDialogs'

function formatUptime(sec: number): string {
  if (sec < 60) return t('corrald.uptimeSeconds', { n: Math.max(1, Math.floor(sec)) })
  const mins = Math.floor(sec / 60)
  if (mins < 60) return t('corrald.uptimeMinutes', { n: mins })
  const hours = Math.floor(mins / 60)
  if (hours < 48) {
    const rem = mins % 60
    return rem > 0 ? t('corrald.uptimeHoursMin', { h: hours, m: rem }) : t('corrald.uptimeHours', { h: hours })
  }
  const days = Math.floor(hours / 24)
  return t('corrald.uptimeDays', { d: days })
}

export function CorraldSection({ hostId, hostName }: { hostId: number; hostName: string }) {
  const toast = useToast()
  const [status, setStatus] = useState<CorraldStatus | null>(null)
  const [available, setAvailable] = useState<boolean | null>(null)
  const [busy, setBusy] = useState(false)
  const [cancelling, setCancelling] = useState(false)
  const [consentOpen, setConsentOpen] = useState<'enable' | 'rebind' | null>(null)
  const [installFailed, setInstallFailed] = useState<{ code: string; detail?: string; message?: string; os?: string; machineId?: string } | null>(null)
  const [removeOpen, setRemoveOpen] = useState(false)
  const [removeOutcome, setRemoveOutcome] = useState<CorraldRemoveOutcome | null>(null)
  const [readmeOpen, setReadmeOpen] = useState(false)
  const [changelogOpen, setChangelogOpen] = useState(false)
  const [scopeOpen, setScopeOpen] = useState(false)
  const [dismissedScopeVer, setDismissedScopeVer] = useState('')
  const [copiedDiag, setCopiedDiag] = useState(false)

  const prevRef = useRef<CorraldStatus | null>(null)
  const consentOpenRef = useRef(consentOpen)
  consentOpenRef.current = consentOpen
  const removeOpenRef = useRef(removeOpen)
  removeOpenRef.current = removeOpen

  const applyStatus = useCallback(
    (next: CorraldStatus | null) => {
      if (next === null) {
        setAvailable(false)
        setStatus(null)
        publishHostCorrald(hostId, null, true)
        return
      }
      const prev = prevRef.current
      prevRef.current = next
      setAvailable(true)
      setStatus(next)
      publishHostCorrald(hostId, next, false)

      const tr = detectCorraldTransition(prev, next)
      if (!tr) return
      if (tr.type === 'enabled') {
        setConsentOpen(null)
        setInstallFailed(null)
        toast.notice({
          title: t('corrald.note.enabledTitle'),
          body:
            typeof tr.agentsCount === 'number'
              ? t('corrald.note.enabledBody', { host: hostName, n: tr.agentsCount })
              : t('corrald.note.enabledBodySimple', { host: hostName }),
          action: { label: t('corrald.note.view'), run: () => navigate('/agents') },
        })
      } else if (tr.type === 'removed') {
        setRemoveOpen(false)
        setRemoveOutcome(null)
        toast.notice({
          title: t('corrald.note.removedTitle'),
          body: t('corrald.note.removedBody', { host: hostName }),
        })
      } else if (tr.type === 'remove_partial') {
        setRemoveOutcome({ kind: 'partial', left: tr.left, cmd: tr.cmd })
        setRemoveOpen(true)
      } else if (tr.type === 'remove_stop_failed') {
        setRemoveOutcome({ kind: 'stop_failed', pid: tr.pid, cmd: tr.cmd })
        setRemoveOpen(true)
      } else if (tr.type === 'remove_pending') {
        if (removeOpenRef.current) setRemoveOutcome({ kind: 'remove_pending', cmd: tr.cmd })
      } else if (tr.type === 'updated') {
        triggerCorraldNoticesCheck()
      } else if (tr.type === 'install_failed') {
        if (consentOpenRef.current) {
          setInstallFailed({
            code: tr.code,
            detail: tr.detail,
            message: next.error?.message,
            os: next.error?.os,
            machineId: next.machine_id ?? next.error?.machine_id,
          })
        }
      } else if (tr.type === 'install_cancelled') {
        setConsentOpen(null)
        setInstallFailed(null)
        toast(t('corrald.cancelled'))
      }
    },
    [hostId, hostName, toast],
  )

  const refresh = useCallback(
    async (signal?: AbortSignal) => {
      try {
        const s = await getHostCorrald(hostId, signal)
        applyStatus(s)
      } catch (e) {
        if (e instanceof ApiError && e.code === 'aborted') return
        // Non-404 transient error on initial load: do not break the host settings page.
      }
    },
    [hostId, applyStatus],
  )

  useEffect(() => {
    prevRef.current = null
    setStatus(null)
    setAvailable(null)
    const ctl = new AbortController()
    void refresh(ctl.signal)
    return () => ctl.abort()
  }, [hostId, refresh])

  const shouldPoll = shouldPollCorrald(status)
  useEffect(() => {
    if (!shouldPoll) return
    let stopped = false
    let timer: ReturnType<typeof setTimeout> | undefined
    let ctl: AbortController | null = null

    const schedule = () => {
      if (stopped || document.visibilityState !== 'visible') return
      timer = setTimeout(() => {
        if (stopped || document.visibilityState !== 'visible') return
        ctl = new AbortController()
        void refresh(ctl.signal).finally(() => {
          if (!stopped) schedule()
        })
      }, 1000)
    }

    const onVis = () => {
      if (document.visibilityState === 'visible') {
        clearTimeout(timer)
        ctl?.abort()
        ctl = new AbortController()
        void refresh(ctl.signal).finally(() => {
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
  }, [shouldPoll, refresh])

  if (available !== true || !status) return null

  const model = corraldSectionModel(status, { dismissedScopeVersion: dismissedScopeVer })
  const displayName = hostName || `#${hostId}`

  const runAction = async (fn: () => Promise<unknown>) => {
    if (busy) return
    setBusy(true)
    try {
      await fn()
      await refresh()
    } catch (e) {
      toast(errorText(e), true)
    } finally {
      setBusy(false)
    }
  }

  const doInstall = async (rebind: boolean) => {
    if (busy) return
    setBusy(true)
    setInstallFailed(null)
    try {
      const op = await installHostCorrald(hostId, {
        ...(status.manifest_hash ? { manifest_hash: status.manifest_hash } : {}),
        ...(rebind ? { rebind: true } : {}),
      })
      if (op) {
        applyStatus({ ...status, state: 'installing', op, error: null })
      }
      await refresh()
    } catch (e) {
      const code = e instanceof ApiError ? e.code : 'other'
      setInstallFailed({ code, message: errorText(e) })
    } finally {
      setBusy(false)
    }
  }

  const doCancel = async () => {
    if (cancelling) return
    setCancelling(true)
    try {
      await cancelHostCorrald(hostId)
      await refresh()
    } catch (e) {
      toast(errorText(e), true)
    } finally {
      setCancelling(false)
    }
  }

  const doRemove = async () => {
    if (busy) return
    setBusy(true)
    setRemoveOutcome(null)
    try {
      const op = await removeHostCorrald(hostId)
      if (op) {
        applyStatus({ ...status, state: 'removing', op, error: null })
      }
      await refresh()
    } catch (e) {
      toast(errorText(e), true)
    } finally {
      setBusy(false)
    }
  }

  const doAgreeUpdate = () => {
    setScopeOpen(false)
    void runAction(async () => {
      const op = await updateHostCorrald(hostId, status.manifest_hash ? { manifest_hash: status.manifest_hash } : {})
      if (op) applyStatus({ ...status, state: 'updating', op, scope_change: null })
    })
  }

  const doReplaceGateway = () => {
    void runAction(async () => {
      const op = await updateHostCorrald(hostId, {
        ...(status.manifest_hash ? { manifest_hash: status.manifest_hash } : {}),
        replace: true,
      })
      if (op) applyStatus({ ...status, state: 'updating', op })
    })
  }

  const dismissScope = () => {
    if (!model.scopeChange) return
    setDismissedScopeVer(model.scopeChange.targetVersion || '__pending__')
    setScopeOpen(false)
  }

  const copyDiag = async () => {
    if (await copyText(buildDiagnostics(status, displayName))) {
      setCopiedDiag(true)
      setTimeout(() => setCopiedDiag(false), 1500)
    }
  }

  const renderActions = () => {
    if (model.actions.length === 0) return null
    return (
      <div className="corrald-acts">
        {model.actions.map((act) => {
          switch (act) {
            case 'enable':
              return (
                <button
                  key={act}
                  type="button"
                  className="corrald-btn is-primary"
                  disabled={busy}
                  onClick={() => {
                    setInstallFailed(null)
                    setConsentOpen('enable')
                    triggerCorraldHintsCheck()
                  }}
                >
                  {t('corrald.enableBtn')}
                </button>
              )
            case 'cancel':
              return (
                <button key={act} type="button" className="corrald-btn" disabled={cancelling} onClick={() => void doCancel()}>
                  {t('common.cancel')}
                </button>
              )
            case 'pause':
              return (
                <button key={act} type="button" className="corrald-btn" disabled={busy} onClick={() => void runAction(() => pauseHostCorrald(hostId))}>
                  {t('corrald.pause')}
                </button>
              )
            case 'resume':
              return (
                <button key={act} type="button" className="corrald-btn is-primary" disabled={busy} onClick={() => void runAction(() => resumeHostCorrald(hostId))}>
                  {t('corrald.resume')}
                </button>
              )
            case 'retry':
              return (
                <button key={act} type="button" className="corrald-btn is-primary" disabled={busy} onClick={() => void runAction(() => retryHostCorrald(hostId))}>
                  {t('common.retry')}
                </button>
              )
            case 'rebind':
              return (
                <button
                  key={act}
                  type="button"
                  className="corrald-btn is-primary"
                  disabled={busy}
                  onClick={() => {
                    setInstallFailed(null)
                    setConsentOpen('rebind')
                  }}
                >
                  {t('corrald.rebindBtn')}
                </button>
              )
            case 'replace_gateway':
              return (
                <button key={act} type="button" className="corrald-btn is-primary" disabled={busy} onClick={doReplaceGateway}>
                  {t('corrald.replaceGateway')}
                </button>
              )
            case 'remove':
              return (
                <button
                  key={act}
                  type="button"
                  className="corrald-btn is-danger"
                  disabled={busy}
                  onClick={() => {
                    setRemoveOutcome(null)
                    setRemoveOpen(true)
                  }}
                >
                  {t('corrald.removeBtn')}
                </button>
              )
          }
        })}
      </div>
    )
  }

  const renderProgressRow = (heading: string) => {
    const p = model.progress!
    const stepText = t('corrald.stepCounter', { step: corraldStepLabel(p.step), i: p.current, n: p.total })
    return (
      <div className="corrald-row is-stack">
        <div className="corrald-k">
          {heading}
          <small aria-live="polite">{stepText}</small>
        </div>
        <div className="alert-bar" role="progressbar" aria-valuenow={p.percent} aria-valuemin={0} aria-valuemax={100}>
          <span className="alert-bar-fill" style={{ width: `${p.percent}%` }} />
        </div>
        {model.actions.length > 0 && renderActions()}
      </div>
    )
  }

  return (
    <section className="corrald-sec" aria-labelledby="corrald-sec-title">
      <h3 id="corrald-sec-title">{t('corrald.title')}</h3>
      <div className="corrald-group">
        {model.scopeChange && (
          <>
            <div className="corrald-row">
              <span className="corrald-warn-ic" aria-hidden="true">
                <TriangleAlert size={15} />
              </span>
              <div className="corrald-k">
                {model.scopeChange.targetVersion
                  ? t('corrald.scopeTitle', { version: model.scopeChange.targetVersion })
                  : t('corrald.scopeTitleGeneric')}
                <small>
                  {model.scopeChange.summary ||
                    t('corrald.scopeInlineSummary', {
                      items: model.scopeChange.added.map(formatScopeItem).join('、'),
                    })}
                </small>
              </div>
            </div>
            <div className="corrald-row">
              <div className="corrald-acts">
                <button type="button" className="corrald-btn" disabled={busy} onClick={() => setScopeOpen(true)}>
                  {t('corrald.scopeDetails')}
                </button>
                <button type="button" className="corrald-btn" disabled={busy} onClick={dismissScope}>
                  {t('corrald.scopeLater')}
                </button>
                <button type="button" className="corrald-btn is-primary" disabled={busy} onClick={doAgreeUpdate}>
                  {t('corrald.scopeAgree')}
                </button>
              </div>
            </div>
          </>
        )}

        {model.updateFailure && (
          <div className="corrald-row">
            <span className="corrald-warn-ic" aria-hidden="true">
              <TriangleAlert size={15} />
            </span>
            <div className="corrald-k">
              {t('corrald.err.updateFailed', {
                next: model.updateFailure.targetVersion || '?',
                prev: model.updateFailure.fallbackVersion || status.version || '?',
              })}
            </div>
          </div>
        )}

        {model.mode === 'admin_disabled' && (
          <div className="corrald-row">
            <div className="corrald-k">{t('corrald.adminDisabled')}</div>
          </div>
        )}

        {model.mode === 'off' && (
          <div className="corrald-row">
            <div className="corrald-k">
              {t('corrald.state.off')}
              <small>{t('corrald.offDesc')}</small>
            </div>
            {renderActions()}
          </div>
        )}

        {model.mode === 'installing' && renderProgressRow(t('corrald.installingTitle'))}
        {model.mode === 'updating' &&
          renderProgressRow(
            status.gateway_version
              ? t('corrald.state.updatingTo', { version: status.gateway_version })
              : t('corrald.state.updating'),
          )}
        {model.mode === 'removing' && renderProgressRow(t('corrald.removingTitle'))}

        {model.mode === 'running' && (
          <>
            <div className="corrald-row">
              <div className="corrald-k">{t('corrald.row.status')}</div>
              <div className="corrald-v">
                <span className="corrald-status">
                  <span className={'corrald-dot ' + (model.degraded.length > 0 ? 'is-warn' : 'is-ok')} aria-hidden="true" />
                  {model.degraded.length > 0 ? t('corrald.state.runningDegraded') : t('corrald.state.running')}
                </span>
              </div>
            </div>
            {model.degraded.length > 0 && (
              <div className="corrald-row">
                <div className="corrald-k">
                  <small>
                    {t('corrald.degradedDetail', {
                      caps: model.degraded.map(formatCapName).join('、'),
                    })}
                  </small>
                </div>
              </div>
            )}
          </>
        )}

        {model.mode === 'stopped' && (
          <div className="corrald-row">
            <div className="corrald-k">
              {t('corrald.row.status')}
              <small>{t('corrald.state.stoppedDesc')}</small>
            </div>
            <div className="corrald-v">{t('corrald.state.stopped')}</div>
          </div>
        )}

        {model.mode === 'paused' && (
          <div className="corrald-row">
            <div className="corrald-k">
              {t('corrald.state.paused')}
              <small>{t('corrald.state.pausedDesc')}</small>
            </div>
          </div>
        )}

        {model.mode === 'lost' && (
          <div className="corrald-row">
            <div className="corrald-k">
              <span className="corrald-status">
                <span className="corrald-dot is-warn" aria-hidden="true" />
                {t('corrald.state.lost')}
              </span>
              <small>
                {t('corrald.state.lostDesc')}
                {status.last_ok_at ? ' ' + t('corrald.lastOk', { when: relTime(status.last_ok_at) }) : ''}
              </small>
            </div>
          </div>
        )}

        {model.mode === 'error' && (
          <div className="corrald-row">
            <div className="corrald-k">
              <span className="corrald-status">
                <span className="corrald-dot is-danger" aria-hidden="true" />
                {t('corrald.state.error')}
              </span>
              <small>
                {model.failure
                  ? t('corrald.state.errorDesc', {
                      reason: corraldFailureText(model.failure.kind, {
                        os: model.failure.os,
                        machineId: model.failure.machineId,
                        message: model.failure.message,
                        pid: model.failure.pid,
                      }).body,
                    })
                  : t('corrald.state.errorDescGeneric')}
              </small>
            </div>
          </div>
        )}

        {model.mode === 'rebind' && (
          <div className="corrald-row">
            <span className="corrald-warn-ic" aria-hidden="true">
              <TriangleAlert size={15} />
            </span>
            <div className="corrald-k">
              {t('corrald.state.rebind')}
              <small>{t('corrald.state.rebindDesc')}</small>
            </div>
          </div>
        )}

        {model.mode === 'incompatible' && (
          <div className="corrald-row">
            <span className="corrald-warn-ic" aria-hidden="true">
              <TriangleAlert size={15} />
            </span>
            <div className="corrald-k">
              {t('corrald.state.incompatible', { version: status.version ?? '?' })}
            </div>
          </div>
        )}

        {model.showVersionRow && status.version && (
          <div className="corrald-row">
            <div className="corrald-k">{t('corrald.row.version')}</div>
            <div className="corrald-v">{status.version}</div>
          </div>
        )}

        {model.showDetailsRows && model.uptimeSec !== null && (
          <div className="corrald-row">
            <div className="corrald-k">{t('corrald.row.uptime')}</div>
            <div className="corrald-v">{formatUptime(model.uptimeSec)}</div>
          </div>
        )}

        {model.showDetailsRows && model.rssMb !== null && (
          <div className="corrald-row">
            <div className="corrald-k">{t('corrald.row.memory')}</div>
            <div className="corrald-v">{model.rssMb} MB</div>
          </div>
        )}

        {model.showUsersRow && (
          <div className="corrald-row">
            <div className="corrald-k">{t('corrald.row.users')}</div>
            <div className="corrald-v">
              {model.otherUsers > 0 ? t('corrald.usersOthers', { n: model.otherUsers }) : t('corrald.usersOnlyYou')}
            </div>
          </div>
        )}

        {model.perfComparison.length > 0 && (
          <div className="corrald-row is-stack">
            <div className="corrald-k">{t('corrald.row.perf')}</div>
            <ul className="corrald-perf-list">
              {model.perfComparison.map((item) => {
                const key = `corrald.perfLine.${item.op}`
                if (!hasKey(key)) return null
                return (
                  <li key={item.op}>
                    {t(key as I18nKey, {
                      after: formatPerfSeconds(item.after_ms),
                      before: formatPerfSeconds(item.before_ms),
                    })}
                  </li>
                )
              })}
            </ul>
          </div>
        )}

        {model.showFiles && (
          <div className="corrald-row">
            <details className="corrald-files">
              <summary>
                <span>{t('corrald.row.files')}</span>
                <span className="corrald-v">{t('corrald.filesCount', { n: status.files.length })}</span>
              </summary>
              <ul className="corrald-files-list">
                {status.files.map((f) => (
                  <li key={f}>{f}</li>
                ))}
              </ul>
            </details>
          </div>
        )}

        {model.failure && model.mode === 'off' && (
          <div className="corrald-row is-stack">
            <div className="corrald-k">
              <small>
                {
                  corraldFailureText(model.failure.kind, {
                    os: model.failure.os,
                    machineId: model.failure.machineId,
                    message: model.failure.message,
                  }).body
                }
              </small>
            </div>
            <div className="corrald-acts">
              <button type="button" className="corrald-btn" onClick={() => void copyDiag()}>
                {copiedDiag ? t('common.copied') : t('corrald.copyDiag')}
              </button>
            </div>
          </div>
        )}

        {model.removeOutcome && (
          <div className="corrald-row is-stack">
            <div className="corrald-k">
              {model.removeOutcome.kind === 'remove_pending'
                ? t('corrald.state.removePending')
                : model.removeOutcome.kind === 'partial'
                  ? t('corrald.err.partial')
                  : model.removeOutcome.pid
                    ? t('corrald.err.stopFailedPid', { pid: model.removeOutcome.pid })
                    : t('corrald.err.stopFailed')}
              {model.removeOutcome.left && model.removeOutcome.left.length > 0 && (
                <ul className="alert-list">
                  {model.removeOutcome.left.map((x, idx) => (
                    <li key={idx}>
                      <code>{x.path}</code>
                      {x.reason ? ` — ${x.reason}` : ''}
                    </li>
                  ))}
                </ul>
              )}
            </div>
            <CorraldCommandBox cmd={model.removeOutcome.cmd} />
          </div>
        )}

        {model.mode !== 'off' &&
          model.mode !== 'installing' &&
          model.mode !== 'updating' &&
          model.mode !== 'removing' &&
          (model.actions.length > 0 || model.failure?.canCopyDiag) && (
            <div className="corrald-row">
              {model.failure?.canCopyDiag && (
                <button type="button" className="corrald-btn" onClick={() => void copyDiag()}>
                  {copiedDiag ? t('common.copied') : t('corrald.copyDiag')}
                </button>
              )}
              {renderActions()}
            </div>
          )}
      </div>

      {(model.showReadmeLink || model.showChangelogLink) && (
        <p className="corrald-foot">
          {model.showReadmeLink && (
            <button type="button" className="link-btn" onClick={() => setReadmeOpen(true)}>
              {t('corrald.readmeLink')}
            </button>
          )}
          {model.showReadmeLink && model.showChangelogLink && <span className="corrald-foot-sep" aria-hidden="true">·</span>}
          {model.showChangelogLink && (
            <button type="button" className="link-btn" onClick={() => setChangelogOpen(true)}>
              {t('corrald.changelogLink')}
            </button>
          )}
        </p>
      )}

      {consentOpen && (
        <CorraldConsentAlert
          hostName={displayName}
          status={status}
          progress={model.mode === 'installing' ? model.progress : null}
          rebind={consentOpen === 'rebind'}
          busy={busy}
          cancelling={cancelling}
          failedError={installFailed}
          onConfirm={() => void doInstall(consentOpen === 'rebind')}
          onCancelInstall={() => void doCancel()}
          onClose={() => {
            setConsentOpen(null)
            setInstallFailed(null)
          }}
        />
      )}

      {removeOpen && (
        <CorraldRemoveAlert
          hostName={displayName}
          status={status}
          progress={model.mode === 'removing' ? model.progress : null}
          outcome={removeOutcome}
          busy={busy}
          onConfirm={() => void doRemove()}
          onClose={() => {
            setRemoveOpen(false)
            setRemoveOutcome(null)
          }}
        />
      )}

      {scopeOpen && model.scopeChange && (
        <CorraldScopeAlert
          targetVersion={model.scopeChange.targetVersion}
          added={model.scopeChange.added}
          summary={model.scopeChange.summary}
          busy={busy}
          onAgree={doAgreeUpdate}
          onDismiss={dismissScope}
          onClose={() => setScopeOpen(false)}
        />
      )}

      {readmeOpen && <CorraldReadmeDialog onClose={() => setReadmeOpen(false)} />}
      {changelogOpen && (
        <CorraldChangelogDialog
          currentVersion={status.version || status.gateway_version}
          onClose={() => setChangelogOpen(false)}
        />
      )}
    </section>
  )
}
