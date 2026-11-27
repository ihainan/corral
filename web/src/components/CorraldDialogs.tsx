import { useCallback, useEffect, useState } from 'react'
import { Check, Copy } from 'lucide-react'
import { ApiError, errorText, type Host } from '../api'
import {
  deleteHostWithOptions,
  getCorraldReadme,
  getHostCorrald,
  type CorraldLeftItem,
  type CorraldReadme,
  type CorraldStatus,
  type DeleteHostOptions,
} from '../corraldApi'
import {
  buildDiagnostics,
  classifyDeleteHostResult,
  corraldFailureKind,
  hasInstalledCorrald,
  otherUsersCount,
  type CorraldFailureKind,
  type CorraldProgressModel,
  type CorraldRemoveOutcome,
  type DeleteHostOutcome,
} from '../corraldLogic'
import { hasKey, t, type I18nKey } from '../i18n'
import { Alert, copyText, Dialog, DialogHead, LoadError, useToast } from './ui'
import { Markdown } from '../terminal/markdown'

// Localized step label for corrald operations.
export function corraldStepLabel(step: string): string {
  const clean = step.trim().toLowerCase()
  const key = 'corrald.step.' + clean
  if (clean && hasKey(key)) return t(key as I18nKey)
  return step || t('corrald.step.working')
}

// Localized failure message per consent-design.md "失败处理".
export function corraldFailureText(
  kind: CorraldFailureKind,
  opts?: { os?: string; machineId?: string; message?: string; pid?: number; targetVersion?: string; fallbackVersion?: string },
): { body: string; residue?: string } {
  switch (kind) {
    case 'unsupported_os':
      return {
        body: opts?.os ? t('corrald.err.unsupportedOsNamed', { os: opts.os }) : t('corrald.err.unsupportedOs'),
        residue: t('corrald.residue.none'),
      }
    case 'unsupported_arch':
      return {
        body: t('corrald.err.unsupportedArch'),
        residue: t('corrald.residue.none'),
      }
    case 'home_readonly':
      return {
        body: t('corrald.err.homeReadonly'),
        residue: t('corrald.residue.stageRemoved'),
      }
    case 'no_space':
      return {
        body: t('corrald.err.noSpace'),
        residue: t('corrald.residue.noSpace'),
      }
    case 'checksum_mismatch':
      return {
        body: t('corrald.err.checksumMismatch'),
        residue: t('corrald.residue.stageRemoved'),
      }
    case 'start_failed':
      return {
        body: t('corrald.err.startFailed', { mid: opts?.machineId || '<machine-id>' }),
        residue: t('corrald.residue.startFailed'),
      }
    case 'disconnected':
      return {
        body: t('corrald.err.disconnected'),
        residue: t('corrald.residue.disconnected'),
      }
    case 'update_failed':
      return {
        body: t('corrald.err.updateFailed', {
          next: opts?.targetVersion || '?',
          prev: opts?.fallbackVersion || '?',
        }),
      }
    case 'stop_failed':
      return {
        body: opts?.pid ? t('corrald.err.stopFailedPid', { pid: opts.pid }) : t('corrald.err.stopFailed'),
      }
    case 'partial':
      return {
        body: t('corrald.err.partial'),
      }
    case 'removing':
      return {
        body: t('corrald.err.removing'),
      }
    default:
      return {
        body: opts?.message || t('corrald.err.other'),
      }
  }
}

// Human-readable label for a manifest scope item (reads/actions/writes/egress/budget/optional).
export function formatScopeItem(item: string): string {
  const key = 'corrald.scope.' + item
  if (hasKey(key)) return t(key as I18nKey)
  return item
}

export function formatCapName(cap: string): string {
  const key = 'corrald.cap.' + cap
  if (hasKey(key)) return t(key as I18nKey)
  return cap
}

export function CorraldCommandBox({ cmd }: { cmd: string }) {
  const [copied, setCopied] = useState(false)
  const copy = async () => {
    if (await copyText(cmd)) {
      setCopied(true)
      setTimeout(() => setCopied(false), 1500)
    }
  }
  return (
    <div className="alert-cmd">
      <code>{cmd}</code>
      <button
        type="button"
        className="alert-cmd-copy"
        onClick={() => void copy()}
        aria-label={copied ? t('common.copied') : t('common.copy')}
        title={copied ? t('common.copied') : t('common.copy')}
      >
        {copied ? <Check size={14} /> : <Copy size={14} />}
      </button>
    </div>
  )
}

function LeftList({ left }: { left?: CorraldLeftItem[] }) {
  if (!left || left.length === 0) return null
  return (
    <ul className="alert-list">
      {left.map((x, i) => (
        <li key={i}>
          <code>{x.path}</code>
          {x.reason ? ` — ${x.reason}` : ''}
        </li>
      ))}
    </ul>
  )
}

// Detailed documentation dialog backed by GET /api/corrald/readme.
// Used both directly from HostEdit and as the second screen from the Consent Alert.
export function CorraldReadmeDialog({ onClose, onBack }: { onClose: () => void; onBack?: () => void }) {
  const [readme, setReadme] = useState<CorraldReadme | null>(null)
  const [loading, setLoading] = useState(true)
  const [unavailable, setUnavailable] = useState(false)
  const [failed, setFailed] = useState(false)

  const load = useCallback((signal?: AbortSignal) => {
    setLoading(true)
    setFailed(false)
    setUnavailable(false)
    getCorraldReadme(signal)
      .then((r) => {
        setReadme(r)
        setLoading(false)
      })
      .catch((e) => {
        if (e instanceof ApiError && e.code === 'aborted') return
        if (e instanceof ApiError && e.status === 404) {
          setUnavailable(true)
          setLoading(false)
          return
        }
        setFailed(true)
        setLoading(false)
      })
  }, [])

  useEffect(() => {
    const ctl = new AbortController()
    load(ctl.signal)
    return () => ctl.abort()
  }, [load])

  return (
    <Dialog title={readme?.title || t('corrald.readmeTitle')} onClose={onClose} wide hideTitle className="dlg corrald-readme-dialog">
      <DialogHead title={readme?.title || t('corrald.readmeTitle')} onClose={onClose} />
      <div className="dlg-body">
        {loading ? (
          <p className="loading">{t('common.loading')}</p>
        ) : unavailable ? (
          <p className="hint">{t('corrald.readmeUnavailable')}</p>
        ) : failed ? (
          <LoadError onRetry={() => load()} />
        ) : readme?.format === 'markdown' ? (
          <div className="corrald-readme-md">
            <Markdown text={readme.content} onCopyCode={(c) => void copyText(c)} copyLabel={t('common.copy')} soft />
          </div>
        ) : (
          <pre className="corrald-readme-pre">{readme?.content ?? ''}</pre>
        )}
      </div>
      <div className="dlg-foot">
        {onBack && (
          <button type="button" className="btn" onClick={onBack}>
            {t('common.back')}
          </button>
        )}
        <button type="button" className="btn btn-primary" onClick={onClose}>
          {t('common.close')}
        </button>
      </div>
    </Dialog>
  )
}

// Scope change details alert ("更新内容").
export function CorraldScopeAlert({
  targetVersion,
  added,
  summary,
  busy,
  onAgree,
  onDismiss,
  onClose,
}: {
  targetVersion: string
  added: string[]
  summary?: string
  busy: boolean
  onAgree: () => void
  onDismiss: () => void
  onClose: () => void
}) {
  return (
    <Alert
      title={targetVersion ? t('corrald.scopeTitle', { version: targetVersion }) : t('corrald.scopeTitleGeneric')}
      onClose={onClose}
      actions={[
        { label: t('corrald.scopeLater'), onClick: onDismiss, disabled: busy },
        { label: t('corrald.scopeAgree'), tone: 'primary', onClick: onAgree, disabled: busy },
      ]}
    >
      {summary && <p className="alert-body">{summary}</p>}
      <p className="alert-sec">{t('corrald.scopeAddedLead')}</p>
      <ul className="alert-list">
        {added.map((item) => (
          <li key={item}>• {formatScopeItem(item)}</li>
        ))}
      </ul>
    </Alert>
  )
}

// Authorization, installation progress, failure, and second-screen readme alert.
export function CorraldConsentAlert({
  hostName,
  status,
  progress,
  rebind,
  busy,
  cancelling,
  failedError,
  onConfirm,
  onCancelInstall,
  onClose,
}: {
  hostName: string
  status: CorraldStatus
  progress: CorraldProgressModel | null
  rebind?: boolean
  busy: boolean
  cancelling: boolean
  failedError: { code: string; detail?: string; message?: string; os?: string; machineId?: string } | null
  onConfirm: () => void
  onCancelInstall: () => void
  onClose: () => void
}) {
  const [showReadme, setShowReadme] = useState(false)
  const [copiedDiag, setCopiedDiag] = useState(false)

  if (showReadme) {
    return <CorraldReadmeDialog onBack={() => setShowReadme(false)} onClose={onClose} />
  }

  // Installing progress view (03-installing.png)
  if (progress && progress.kind === 'install') {
    const stepText = t('corrald.stepCounter', {
      step: corraldStepLabel(progress.step),
      i: progress.current,
      n: progress.total,
    })
    return (
      <Alert
        title={t('corrald.installingTitle')}
        viewKey="installing"
        onClose={onClose}
        live={stepText}
        actions={
          progress.cancellable
            ? [{ label: t('common.cancel'), onClick: onCancelInstall, disabled: cancelling }]
            : [{ label: t('common.close'), onClick: onClose }]
        }
      >
        <div className="alert-bar" role="progressbar" aria-valuenow={progress.percent} aria-valuemin={0} aria-valuemax={100}>
          <span className="alert-bar-fill" style={{ width: `${progress.percent}%` }} />
        </div>
        <p className="alert-sec">{stepText}</p>
      </Alert>
    )
  }

  // Failure view (consent-design.md "失败处理")
  if (failedError) {
    const kind = corraldFailureKind(failedError.code)
    const msg = corraldFailureText(kind, {
      os: failedError.os,
      machineId: failedError.machineId ?? status.machine_id,
      message: failedError.message,
    })
    const copyDiag = async () => {
      const text = buildDiagnostics(
        { ...status, error: status.error ?? { code: failedError.code, detail: failedError.detail, message: failedError.message } },
        hostName,
      )
      if (await copyText(text)) {
        setCopiedDiag(true)
        setTimeout(() => setCopiedDiag(false), 1500)
      }
    }
    return (
      <Alert
        title={t('corrald.installFailedTitle')}
        viewKey="failed"
        onClose={onClose}
        live={msg.body}
        actions={[
          { label: copiedDiag ? t('common.copied') : t('corrald.copyDiag'), onClick: () => void copyDiag() },
          { label: t('common.close'), tone: 'primary', onClick: onClose },
        ]}
      >
        <p className="alert-body">{msg.body}</p>
        {msg.residue && <p className="alert-sec">{msg.residue}</p>}
      </Alert>
    )
  }

  // Consent view (02-consent.png, 02-consent-dark.png, 14-consent-phone.png)
  const sizeMb = status.estimated_size_mb ?? 10
  return (
    <Alert
      title={t(rebind ? 'corrald.rebindTitle' : 'corrald.consentTitle', { host: hostName })}
      viewKey="consent"
      onClose={onClose}
      actions={[
        { label: t('corrald.consentDecline'), onClick: onClose, disabled: busy },
        { label: t('corrald.consentEnable'), tone: 'primary', onClick: onConfirm, disabled: busy },
      ]}
    >
      <p className="alert-body">{t(rebind ? 'corrald.rebindBody' : 'corrald.consentBody')}</p>
      <p className="alert-sec">
        {t('corrald.consentNote', { size: sizeMb })}{' '}
        <button type="button" className="alert-link" onClick={() => setShowReadme(true)}>
          {t('corrald.readmeLink')}
        </button>
      </p>
    </Alert>
  )
}

// Delete helper confirmation, progress, and outcome alert (06-delete-helper.png).
export function CorraldRemoveAlert({
  hostName,
  status,
  progress,
  outcome,
  busy,
  onConfirm,
  onClose,
}: {
  hostName: string
  status: CorraldStatus
  progress: CorraldProgressModel | null
  outcome: CorraldRemoveOutcome | null
  busy: boolean
  onConfirm: () => void
  onClose: () => void
}) {
  const [copiedCmd, setCopiedCmd] = useState(false)

  if (outcome) {
    const copyCmd = async () => {
      if (await copyText(outcome.cmd)) {
        setCopiedCmd(true)
        setTimeout(() => setCopiedCmd(false), 1500)
      }
    }
    const title =
      outcome.kind === 'partial'
        ? t('corrald.removePartialTitle')
        : outcome.kind === 'stop_failed'
          ? t('corrald.removeStopFailedTitle')
          : t('corrald.removePendingTitle')
    const body =
      outcome.kind === 'partial'
        ? t('corrald.err.partial')
        : outcome.kind === 'stop_failed'
          ? outcome.pid
            ? t('corrald.err.stopFailedPid', { pid: outcome.pid })
            : t('corrald.err.stopFailed')
          : t('corrald.state.removePending')
    return (
      <Alert
        title={title}
        viewKey={`remove-${outcome.kind}`}
        onClose={onClose}
        live={body}
        actions={[
          { label: copiedCmd ? t('common.copied') : t('corrald.copyCmd'), onClick: () => void copyCmd() },
          { label: t('common.ok'), tone: 'primary', onClick: onClose },
        ]}
      >
        <p className="alert-body">{body}</p>
        {outcome.kind === 'partial' && <LeftList left={outcome.left} />}
        <p className="alert-sec">{t('corrald.runOnHost')}</p>
        <CorraldCommandBox cmd={outcome.cmd} />
      </Alert>
    )
  }

  if (progress && progress.kind === 'remove') {
    const stepText = t('corrald.stepCounter', {
      step: corraldStepLabel(progress.step),
      i: progress.current,
      n: progress.total,
    })
    return (
      <Alert
        title={t('corrald.removingTitle')}
        viewKey="removing"
        onClose={onClose}
        live={stepText}
        actions={[{ label: t('common.close'), onClick: onClose }]}
      >
        <div className="alert-bar" role="progressbar" aria-valuenow={progress.percent} aria-valuemin={0} aria-valuemax={100}>
          <span className="alert-bar-fill" style={{ width: `${progress.percent}%` }} />
        </div>
        <p className="alert-sec">{stepText}</p>
      </Alert>
    )
  }

  const others = otherUsersCount(status.users)
  const sizeMb = status.estimated_size_mb ?? 20
  return (
    <Alert
      title={t('corrald.removeTitle', { host: hostName })}
      viewKey="remove-confirm"
      onClose={onClose}
      actions={[
        { label: t('common.cancel'), onClick: onClose, disabled: busy },
        { label: t('common.delete'), tone: 'danger', onClick: onConfirm, disabled: busy },
      ]}
    >
      <p className="alert-body">{t('corrald.removeBody', { size: sizeMb })}</p>
      {others > 0 && <p className="alert-sec">{t('corrald.removeOthers', { n: others })}</p>}
    </Alert>
  )
}

// Delete host confirmation + unreachable/partial cleanup alert (10-delete-host.png, 11-host-unreachable.png).
export function DeleteHostAlert({
  host,
  codeEnabled,
  onDeleted,
  onClose,
}: {
  host: Host
  codeEnabled: boolean
  onDeleted: () => void
  onClose: () => void
}) {
  const toast = useToast()
  const [corrald, setCorrald] = useState<CorraldStatus | null>(null)
  const [removeCorrald, setRemoveCorrald] = useState(true)
  const [stopVscode, setStopVscode] = useState(false)
  const [removeVscode, setRemoveVscode] = useState(false)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  const [outcome, setOutcome] = useState<DeleteHostOutcome | null>(null)
  const [copiedCmd, setCopiedCmd] = useState(false)

  useEffect(() => {
    const ctl = new AbortController()
    getHostCorrald(host.id, ctl.signal)
      .then((s) => setCorrald(s))
      .catch(() => {})
    return () => ctl.abort()
  }, [host.id])

  const hasCorrald = hasInstalledCorrald(corrald)
  const others = otherUsersCount(corrald?.users)
  const sizeMb = corrald?.estimated_size_mb ?? 20

  const submit = async () => {
    if (busy) return
    setBusy(true)
    setError('')
    const opts: DeleteHostOptions = {
      remove_corrald: hasCorrald && removeCorrald,
      stop_vscode: codeEnabled && stopVscode,
      remove_vscode: codeEnabled && removeVscode,
    }
    try {
      const res = await deleteHostWithOptions(host.id, opts)
      onDeleted()
      const classified = classifyDeleteHostResult(res, opts.remove_corrald)
      if (classified.outcome === 'done') {
        onClose()
        return
      }
      if (classified.outcome === 'removed') {
        toast.notice({
          title: t('corrald.note.removedTitle'),
          body: t('corrald.note.removedBody', { host: host.name }),
        })
        onClose()
        return
      }
      setOutcome(classified)
      setBusy(false)
    } catch (e) {
      setError(errorText(e))
      setBusy(false)
    }
  }

  if (outcome && outcome.outcome !== 'done' && outcome.outcome !== 'removed') {
    const cmd = outcome.cmd
    const copyCmd = async () => {
      if (await copyText(cmd)) {
        setCopiedCmd(true)
        setTimeout(() => setCopiedCmd(false), 1500)
      }
    }
    if (outcome.outcome === 'unreachable') {
      return (
        <Alert
          title={t('hosts.unreachableTitle', { name: host.name })}
          viewKey="unreachable"
          onClose={onClose}
          live={t('hosts.unreachableBody')}
          actions={[
            { label: copiedCmd ? t('common.copied') : t('corrald.copyCmd'), onClick: () => void copyCmd() },
            { label: t('common.ok'), tone: 'primary', onClick: onClose },
          ]}
        >
          <p className="alert-body">{t('hosts.unreachableBody')}</p>
          <p className="alert-sec">{t('hosts.unreachableRun')}</p>
          <CorraldCommandBox cmd={cmd} />
        </Alert>
      )
    }
    const title =
      outcome.outcome === 'partial'
        ? t('corrald.removePartialTitle')
        : outcome.outcome === 'stop_failed'
          ? t('corrald.removeStopFailedTitle')
          : t('corrald.removePendingTitle')
    const body =
      outcome.outcome === 'partial'
        ? t('corrald.err.partial')
        : outcome.outcome === 'stop_failed'
          ? outcome.pid
            ? t('corrald.err.stopFailedPid', { pid: outcome.pid })
            : t('corrald.err.stopFailed')
          : t('corrald.state.removePending')
    return (
      <Alert
        title={title}
        viewKey={`delete-${outcome.outcome}`}
        onClose={onClose}
        live={body}
        actions={[
          { label: copiedCmd ? t('common.copied') : t('corrald.copyCmd'), onClick: () => void copyCmd() },
          { label: t('common.ok'), tone: 'primary', onClick: onClose },
        ]}
      >
        <p className="alert-body">{body}</p>
        {outcome.outcome === 'partial' && <LeftList left={outcome.left} />}
        <p className="alert-sec">{t('corrald.runOnHost')}</p>
        <CorraldCommandBox cmd={cmd} />
      </Alert>
    )
  }

  return (
    <Alert
      title={t('hosts.deleteTitle', { name: host.name })}
      viewKey="delete-host"
      onClose={onClose}
      canClose={!busy}
      actions={[
        { label: t('common.cancel'), onClick: onClose, disabled: busy },
        { label: t('common.delete'), tone: 'danger', onClick: () => void submit(), disabled: busy },
      ]}
    >
      <p className="alert-body">{t('hosts.deleteBody')}</p>
      {(hasCorrald || codeEnabled) && (
        <div className="alert-checks">
          {hasCorrald && (
            <label>
              <input
                type="checkbox"
                checked={removeCorrald}
                disabled={busy}
                onChange={(e) => setRemoveCorrald(e.target.checked)}
              />
              <span>
                {t('hosts.deleteCorrald')}
                <small>
                  {others > 0
                    ? t('hosts.deleteCorraldSubOthers', { size: sizeMb, n: others })
                    : t('hosts.deleteCorraldSub', { size: sizeMb })}
                </small>
              </span>
            </label>
          )}
          {codeEnabled && (
            <>
              <label>
                <input
                  type="checkbox"
                  checked={stopVscode}
                  disabled={busy}
                  onChange={(e) => setStopVscode(e.target.checked)}
                />
                <span>{t('hosts.deleteStopVscode')}</span>
              </label>
              <label>
                <input
                  type="checkbox"
                  checked={removeVscode}
                  disabled={busy}
                  onChange={(e) => setRemoveVscode(e.target.checked)}
                />
                <span>
                  {t('hosts.deleteRemoveVscode')}
                  <small>{t('hosts.deleteVscodeSize')}</small>
                </span>
              </label>
            </>
          )}
        </div>
      )}
      {error && (
        <p className="err" role="alert">
          {error}
        </p>
      )}
    </Alert>
  )
}
