// Pure state-machine, view-model, and diagnostic logic for the host helper
// daemon (corrald). Kept free of React/DOM imports so Node test scripts can
// exercise every state and transition directly.

export type CorraldState =
  | 'off'
  | 'installing'
  | 'running'
  | 'stopped'
  | 'updating'
  | 'paused'
  | 'lost'
  | 'error'
  | 'removing'
  | 'remove_pending'
  | 'rebind'
  | 'incompatible'

export type CorraldOpKind = 'install' | 'update' | 'pause' | 'resume' | 'remove'
export type CorraldOpState = 'pending' | 'running' | 'done' | 'failed' | 'cancelled' | 'unknown'

export interface CorraldLeftItem {
  path: string
  reason?: string
}

export interface CorraldOp {
  id: string
  kind: CorraldOpKind
  state: CorraldOpState
  step: string
  steps?: string[]
  cancellable?: boolean
  error_code?: string
  detail?: string
  result?: 'removed' | 'partial' | 'stop_failed' | 'pending' | string
  left?: CorraldLeftItem[]
  pid?: number
  host_info?: string
}

export interface CorraldRunning {
  pid: number
  uptime_s: number
  rss_kb: number
}

export interface CorraldUsers {
  count: number
  others: string[]
}

export interface CorraldConsent {
  by: string
  at: number | string
}

export interface CorraldScopeChange {
  target_version?: string
  added: string[]
  summary?: string
}

export interface CorraldError {
  code: string
  message?: string
  detail?: string
  target_version?: string
  fallback_version?: string
  machine_id?: string
  os?: string
  arch?: string
  left?: CorraldLeftItem[]
  pid?: number
}

export interface CorraldPerfCompareItem {
  op: 'send' | 'card' | string
  after_ms: number
  before_ms: number
  after_n: number
  before_n: number
}

export interface CorraldUpdateNotice {
  id: number
  host_id: number
  host_name: string
  old_version: string
  new_version: string
  new_release: number
  success: boolean
  created_at: number
}

export interface GroupedCorraldUpdateNotices {
  mergedSuccess: {
    version: string
    hostCount: number
    ids: number[]
  } | null
  failures: CorraldUpdateNotice[]
  allIds: number[]
}

export interface CorraldChangelogEntry {
  version: string
  zh: string[]
  en: string[]
}

export interface CorraldChangelog {
  current_version: string
  current_release: number
  entries: CorraldChangelogEntry[]
}

export interface CorraldAdminPolicy {
  allowed: boolean
  locked_by_env: boolean
}

export interface CorraldStatus {
  state: CorraldState
  version?: string
  release?: number
  gateway_version?: string
  manifest_hash?: string
  estimated_size_mb?: number
  agents_count?: number
  last_ok_at?: number
  running: CorraldRunning | null
  users: CorraldUsers | null
  consent: CorraldConsent | null
  op: CorraldOp | null
  caps: Record<string, string> | null
  scope_change: CorraldScopeChange | null
  error: CorraldError | null
  admin_disabled: boolean
  files: string[]
  can_replace_with_gateway?: boolean
  machine_id?: string
  perf_comparison?: CorraldPerfCompareItem[]
}

export interface DeleteHostOptions {
  remove_corrald: boolean
  stop_vscode: boolean
  remove_vscode: boolean
}

export interface DeleteHostResult {
  deleted?: boolean
  reachable?: boolean
  corrald?: {
    state?: 'removed' | 'partial' | 'stop_failed' | 'unreachable' | 'remove_pending' | 'skipped' | 'error' | string
    left?: CorraldLeftItem[]
    pid?: number
    error?: string
    uninstall_cmd?: string
  }
  vscode?: {
    stopped?: boolean
    removed?: boolean
    error?: string
  }
}

export const DEFAULT_UNINSTALL_CMD = '~/.local/lib/corral/corrald uninstall'

export const DEFAULT_INSTALL_STEPS = ['check', 'upload', 'verify', 'install', 'start'] as const
export const DEFAULT_REMOVE_STEPS = ['mark', 'stop', 'remove', 'verify'] as const
export const DEFAULT_UPDATE_STEPS = ['upload', 'install', 'handoff', 'verify'] as const

const NON_CANCELLABLE_STEPS = new Set(['start', 'done'])

export type CorraldFailureKind =
  | 'unsupported_os'
  | 'unsupported_arch'
  | 'home_readonly'
  | 'no_space'
  | 'checksum_mismatch'
  | 'start_failed'
  | 'disconnected'
  | 'update_failed'
  | 'handoff_failed'
  | 'handoff_in_progress'
  | 'not_newer'
  | 'proto_mismatch'
  | 'op_unknown'
  | 'stop_failed'
  | 'partial'
  | 'removing'
  | 'other'

export type CorraldSectionMode =
  | 'admin_disabled'
  | 'installing'
  | 'updating'
  | 'removing'
  | 'off'
  | 'running'
  | 'stopped'
  | 'paused'
  | 'lost'
  | 'error'
  | 'remove_pending'
  | 'rebind'
  | 'incompatible'

export type CorraldActionId =
  | 'enable'
  | 'cancel'
  | 'pause'
  | 'resume'
  | 'remove'
  | 'retry'
  | 'rebind'
  | 'replace_gateway'

export interface CorraldProgressModel {
  kind: CorraldOpKind
  step: string
  current: number
  total: number
  percent: number
  cancellable: boolean
}

export interface CorraldFailureModel {
  kind: CorraldFailureKind
  code: string
  message?: string
  detail?: string
  os?: string
  machineId?: string
  left?: CorraldLeftItem[]
  pid?: number
  canCopyDiag: boolean
}

export interface CorraldRemoveOutcome {
  kind: 'partial' | 'stop_failed' | 'remove_pending'
  left?: CorraldLeftItem[]
  pid?: number
  cmd: string
}

export interface CorraldSectionModel {
  mode: CorraldSectionMode
  poll: boolean
  progress: CorraldProgressModel | null
  scopeChange: { targetVersion: string; added: string[]; summary?: string } | null
  degraded: string[]
  updateFailure: { targetVersion: string; fallbackVersion: string } | null
  failure: CorraldFailureModel | null
  removeOutcome: CorraldRemoveOutcome | null
  actions: CorraldActionId[]
  showDetailsRows: boolean
  showVersionRow: boolean
  showUsersRow: boolean
  showReadmeLink: boolean
  showChangelogLink: boolean
  showFiles: boolean
  perfComparison: CorraldPerfCompareItem[]
  rssMb: number | null
  uptimeSec: number | null
  otherUsers: number
}

export interface CorraldStatusBarModel {
  kind: 'running' | 'updating'
  version: string
  degraded: boolean
}

export type CorraldTransition =
  | { type: 'enabled'; version: string; agentsCount?: number }
  | { type: 'removed' }
  | { type: 'remove_partial'; left: CorraldLeftItem[]; cmd: string }
  | { type: 'remove_stop_failed'; pid: number; cmd: string }
  | { type: 'remove_pending'; cmd: string }
  | { type: 'updated'; version: string }
  | { type: 'install_failed'; code: string; detail?: string }
  | { type: 'install_cancelled' }
  | { type: 'lost' }
  | { type: 'error'; code: string }

export type DeleteHostOutcome =
  | { outcome: 'done' }
  | { outcome: 'removed' }
  | { outcome: 'unreachable'; cmd: string }
  | { outcome: 'partial'; left: CorraldLeftItem[]; cmd: string }
  | { outcome: 'stop_failed'; pid: number; cmd: string }
  | { outcome: 'remove_pending'; cmd: string }

// Returns true for 409 {error:"corrald_unknown_result"} (mapped to ApiError.code).
export function isCorraldUnknownResult(e: unknown): boolean {
  if (!e || typeof e !== 'object') return false
  const err = e as { status?: unknown; code?: unknown }
  if (err.code !== 'corrald_unknown_result') return false
  return err.status === undefined || err.status === 409
}

export function isOpActive(op: CorraldOp | null | undefined): boolean {
  return !!op && (op.state === 'pending' || op.state === 'running')
}

// Poll every 1 s only while an operation is active (installing, updating, removing, etc.).
export function shouldPollCorrald(status: CorraldStatus | null | undefined): boolean {
  if (!status) return false
  if (isOpActive(status.op)) return true
  return status.state === 'installing' || status.state === 'updating' || status.state === 'removing'
}

// Cancellation is only valid during an active install before the daemon starts.
export function isOpCancellable(op: CorraldOp | null | undefined): boolean {
  if (!op || op.kind !== 'install' || !isOpActive(op)) return false
  if (typeof op.cancellable === 'boolean') return op.cancellable
  const step = (op.step ?? '').trim().toLowerCase()
  return !NON_CANCELLABLE_STEPS.has(step)
}

export function opProgress(op: CorraldOp | null | undefined, fallbackKind: CorraldOpKind = 'install'): CorraldProgressModel {
  const kind = op?.kind ?? fallbackKind
  const step = op?.step ?? ''
  const defaults =
    kind === 'remove'
      ? DEFAULT_REMOVE_STEPS
      : kind === 'update'
        ? DEFAULT_UPDATE_STEPS
        : DEFAULT_INSTALL_STEPS
  const steps = op?.steps && op.steps.length > 0 ? op.steps : [...defaults]
  const total = Math.max(1, steps.length)
  let idx = step ? steps.findIndex((s) => s.toLowerCase() === step.toLowerCase()) : -1
  if (idx < 0) idx = op?.state === 'done' ? total - 1 : 0
  const current = Math.min(total, Math.max(1, idx + 1))
  const percent = op?.state === 'done' ? 100 : Math.max(8, Math.min(96, Math.round((current / total) * 100)))
  return {
    kind,
    step,
    current,
    total,
    percent,
    cancellable: isOpCancellable(op),
  }
}

export function corraldFailureKind(code: string | undefined): CorraldFailureKind {
  switch ((code ?? '').trim().toLowerCase()) {
    case 'unsupported_os':
      return 'unsupported_os'
    case 'unsupported_arch':
      return 'unsupported_arch'
    case 'home_readonly':
      return 'home_readonly'
    case 'no_space':
      return 'no_space'
    case 'checksum_mismatch':
      return 'checksum_mismatch'
    case 'start_failed':
      return 'start_failed'
    case 'disconnected':
      return 'disconnected'
    case 'update_failed':
      return 'update_failed'
    case 'handoff_failed':
      return 'handoff_failed'
    case 'handoff_in_progress':
      return 'handoff_in_progress'
    case 'not_newer':
      return 'not_newer'
    case 'proto_mismatch':
      return 'proto_mismatch'
    case 'op_expired':
    case 'op_unknown':
    case 'corrald_unknown_result':
      return 'op_unknown'
    case 'stop_failed':
      return 'stop_failed'
    case 'partial':
      return 'partial'
    case 'removing':
      return 'removing'
    default:
      return 'other'
  }
}

export function degradedCaps(caps: Record<string, string> | null | undefined): string[] {
  if (!caps) return []
  return Object.entries(caps)
    .filter(([, v]) => v !== 'ok')
    .map(([k]) => k)
    .sort()
}

export function formatRssMb(rssKb: number | null | undefined): number | null {
  if (typeof rssKb !== 'number' || !Number.isFinite(rssKb) || rssKb <= 0) return null
  return Math.max(1, Math.round(rssKb / 1024))
}

export function otherUsersCount(users: CorraldUsers | null | undefined): number {
  if (!users) return 0
  const fromList = Array.isArray(users.others) ? users.others.length : 0
  const fromCount = typeof users.count === 'number' && users.count > 1 ? users.count - 1 : 0
  return Math.max(fromList, fromCount)
}

export function hasInstalledCorrald(status: CorraldStatus | null | undefined): boolean {
  return !!status && status.state !== 'off'
}

function shellQuote(arg: string): string {
  if (/^[a-zA-Z0-9_./~:-]+$/.test(arg)) return arg
  return `'${arg.replace(/'/g, `'\\''`)}'`
}

export function buildRemoveFilesCommand(left: CorraldLeftItem[]): string {
  const paths = left.map((x) => x.path.trim()).filter(Boolean)
  if (paths.length === 0) return DEFAULT_UNINSTALL_CMD
  return 'rm -rf -- ' + paths.map(shellQuote).join(' ')
}

export function buildStopCommand(pid?: number): string {
  if (typeof pid === 'number' && Number.isInteger(pid) && pid > 0) {
    return `kill ${pid}`
  }
  return DEFAULT_UNINSTALL_CMD
}

export function buildDiagnostics(status: CorraldStatus, hostName: string): string {
  const lines: string[] = []
  if (hostName) lines.push(`host: ${hostName}`)
  lines.push(`state: ${status.state}`)
  if (status.version) lines.push(`version: ${status.version}`)
  if (status.release) lines.push(`release: ${status.release}`)
  if (status.op) {
    const parts = [status.op.kind, status.op.id, status.op.state]
    if (status.op.step) parts.push(`step=${status.op.step}`)
    lines.push(`op: ${parts.filter(Boolean).join(' ')}`)
    if (status.op.error_code) lines.push(`op_error: ${status.op.error_code}`)
    if (status.op.detail) lines.push(`op_detail: ${status.op.detail}`)
    if (status.op.host_info) lines.push(`host_info: ${status.op.host_info}`)
  }
  if (status.error) {
    if (status.error.code) lines.push(`error: ${status.error.code}`)
    if (status.error.detail) lines.push(`detail: ${status.error.detail}`)
    if (status.error.os || status.error.arch) {
      lines.push(`system: ${[status.error.os, status.error.arch].filter(Boolean).join('/')}`)
    }
  }
  if (status.machine_id || status.error?.machine_id) {
    lines.push(`machine_id: ${status.machine_id ?? status.error?.machine_id}`)
  }
  return lines.join('\n')
}

// Pure state + op -> HostEdit section view model.
export function corraldSectionModel(
  status: CorraldStatus,
  opts?: { dismissedScopeVersion?: string },
): CorraldSectionModel {
  const poll = shouldPollCorrald(status)
  const op = status.op
  const activeOp = isOpActive(op) ? op : null
  const degraded = degradedCaps(status.caps)
  const rssMb = formatRssMb(status.running?.rss_kb)
  const uptimeSec =
    typeof status.running?.uptime_s === 'number' && status.running.uptime_s >= 0
      ? status.running.uptime_s
      : null
  const otherUsers = otherUsersCount(status.users)

  // Determine effective display mode.
  let mode: CorraldSectionMode
  if (status.admin_disabled) {
    mode = 'admin_disabled'
  } else if (activeOp?.kind === 'remove' || status.state === 'removing') {
    mode = 'removing'
  } else if (activeOp?.kind === 'install' || status.state === 'installing') {
    mode = 'installing'
  } else if (activeOp?.kind === 'update' || status.state === 'updating') {
    mode = 'updating'
  } else {
    mode = status.state
  }

  // Progress for active operations.
  let progress: CorraldProgressModel | null = null
  if (mode === 'installing') progress = opProgress(op, 'install')
  else if (mode === 'updating') progress = opProgress(op, 'update')
  else if (mode === 'removing') progress = opProgress(op, 'remove')

  // Scope change banner (shown when running/stopped/paused and not dismissed for this version).
  const sc = status.scope_change
  const targetVer = sc?.target_version ?? status.gateway_version ?? ''
  const scopeChange =
    !status.admin_disabled &&
    mode !== 'installing' &&
    mode !== 'updating' &&
    mode !== 'removing' &&
    mode !== 'off' &&
    sc &&
    Array.isArray(sc.added) &&
    sc.added.length > 0 &&
    !(opts?.dismissedScopeVersion && opts.dismissedScopeVersion === (targetVer || '__pending__'))
      ? { targetVersion: targetVer, added: sc.added, summary: sc.summary }
      : null

  // Remove outcomes (partial / stop_failed / remove_pending).
  let removeOutcome: CorraldRemoveOutcome | null = null
  const removeErrCode = status.error?.code ?? (op?.kind === 'remove' ? op.error_code : undefined)
  const removeResult = op?.kind === 'remove' ? op.result : undefined
  if (mode === 'remove_pending') {
    removeOutcome = { kind: 'remove_pending', cmd: DEFAULT_UNINSTALL_CMD }
  } else if (removeResult === 'partial' || corraldFailureKind(removeErrCode) === 'partial') {
    const left = op?.left ?? status.error?.left ?? []
    removeOutcome = { kind: 'partial', left, cmd: buildRemoveFilesCommand(left) }
  } else if (removeResult === 'stop_failed' || corraldFailureKind(removeErrCode) === 'stop_failed') {
    const pid = op?.pid ?? status.error?.pid ?? status.running?.pid
    removeOutcome = { kind: 'stop_failed', pid, cmd: buildStopCommand(pid) }
  }

  // Update failure notice (when rolled back to previous version and still running/stopped).
  let updateFailure: { targetVersion: string; fallbackVersion: string } | null = null
  if (
    (mode === 'running' || mode === 'stopped') &&
    (corraldFailureKind(status.error?.code) === 'update_failed' ||
      (op?.kind === 'update' && op.state === 'failed'))
  ) {
    updateFailure = {
      targetVersion: status.error?.target_version ?? status.gateway_version ?? '',
      fallbackVersion: status.error?.fallback_version ?? status.version ?? '',
    }
  }

  // General failure model (for error state, or failed install op while off).
  let failure: CorraldFailureModel | null = null
  if (!status.admin_disabled && !removeOutcome) {
    if (mode === 'error') {
      const code = status.error?.code || op?.error_code || 'other'
      failure = {
        kind: corraldFailureKind(code),
        code,
        message: status.error?.message,
        detail: status.error?.detail ?? op?.detail,
        os: status.error?.os,
        machineId: status.machine_id ?? status.error?.machine_id,
        left: status.error?.left ?? op?.left,
        pid: status.error?.pid ?? op?.pid,
        canCopyDiag: true,
      }
    } else if (mode === 'off' && ((op?.kind === 'install' && op.state === 'failed') || status.error)) {
      const code = status.error?.code || op?.error_code || 'other'
      failure = {
        kind: corraldFailureKind(code),
        code,
        message: status.error?.message,
        detail: status.error?.detail ?? op?.detail,
        os: status.error?.os,
        machineId: status.machine_id ?? status.error?.machine_id,
        left: status.error?.left ?? op?.left,
        pid: status.error?.pid ?? op?.pid,
        canCopyDiag: true,
      }
    }
  }

  // Allowed actions per mode.
  const actions: CorraldActionId[] = []
  switch (mode) {
    case 'admin_disabled':
      break
    case 'off':
      actions.push('enable')
      break
    case 'installing':
      if (progress?.cancellable) actions.push('cancel')
      break
    case 'running':
      actions.push('pause', 'remove')
      break
    case 'stopped':
      actions.push('remove')
      break
    case 'updating':
      break
    case 'paused':
      actions.push('resume', 'remove')
      break
    case 'lost':
      actions.push('remove')
      break
    case 'error':
      actions.push('retry', 'remove')
      break
    case 'removing':
      break
    case 'remove_pending':
      break
    case 'rebind':
      actions.push('rebind', 'remove')
      break
    case 'incompatible':
      actions.push('replace_gateway', 'remove')
      break
  }

  const showDetailsRows = mode === 'running'
  const showVersionRow =
    !!status.version &&
    (mode === 'running' ||
      mode === 'stopped' ||
      mode === 'updating' ||
      mode === 'paused' ||
      mode === 'lost' ||
      mode === 'incompatible')
  const showUsersRow = !!status.users && (mode === 'running' || mode === 'stopped')
  const showFiles =
    Array.isArray(status.files) &&
    status.files.length > 0 &&
    mode !== 'off' &&
    mode !== 'admin_disabled'
  const perfComparison =
    !status.admin_disabled &&
    (mode === 'running' || mode === 'stopped') &&
    Array.isArray(status.perf_comparison)
      ? status.perf_comparison.filter(
          (item) =>
            item &&
            typeof item.op === 'string' &&
            Number.isFinite(item.after_ms) &&
            Number.isFinite(item.before_ms) &&
            item.after_n >= 20 &&
            item.before_n >= 20,
        )
      : []

  return {
    mode,
    poll,
    progress,
    scopeChange,
    degraded,
    updateFailure,
    failure,
    removeOutcome,
    actions,
    showDetailsRows,
    showVersionRow,
    showUsersRow,
    showReadmeLink: true,
    showChangelogLink: !status.admin_disabled,
    showFiles,
    perfComparison,
    rssMb,
    uptimeSec,
    otherUsers,
  }
}

// Workbench status bar view model.
export function corraldStatusBarModel(status: CorraldStatus | null | undefined): CorraldStatusBarModel | null {
  if (!status || status.admin_disabled) return null
  if (status.state === 'updating' || (status.op?.kind === 'update' && isOpActive(status.op))) {
    return { kind: 'updating', version: status.version ?? '', degraded: false }
  }
  if (status.state === 'running') {
    return {
      kind: 'running',
      version: status.version ?? '',
      degraded: degradedCaps(status.caps).length > 0,
    }
  }
  return null
}

// Detect user-visible completion/error transitions between two polled snapshots.
export function detectCorraldTransition(
  prev: CorraldStatus | null,
  next: CorraldStatus,
): CorraldTransition | null {
  if (!prev) return null

  const wasInstalling = prev.state === 'installing' || (prev.op?.kind === 'install' && isOpActive(prev.op))
  const wasRemoving = prev.state === 'removing' || (prev.op?.kind === 'remove' && isOpActive(prev.op))
  const wasUpdating = prev.state === 'updating' || (prev.op?.kind === 'update' && isOpActive(prev.op))

  if (wasInstalling && !isOpActive(next.op) && next.state !== 'installing') {
    if (next.op?.kind === 'install' && next.op.state === 'cancelled') {
      return { type: 'install_cancelled' }
    }
    if (next.state === 'running' || (next.op?.kind === 'install' && next.op.state === 'done')) {
      return { type: 'enabled', version: next.version ?? '', agentsCount: next.agents_count }
    }
    if (next.state === 'error' || (next.op?.kind === 'install' && next.op.state === 'failed') || next.error) {
      return {
        type: 'install_failed',
        code: next.error?.code ?? next.op?.error_code ?? 'other',
        detail: next.error?.detail ?? next.op?.detail,
      }
    }
  }

  if (wasRemoving && !isOpActive(next.op) && next.state !== 'removing') {
    if (next.state === 'remove_pending' || next.op?.result === 'pending') {
      return { type: 'remove_pending', cmd: DEFAULT_UNINSTALL_CMD }
    }
    const removeCode = next.error?.code ?? next.op?.error_code
    if (next.op?.result === 'partial' || corraldFailureKind(removeCode) === 'partial') {
      const left = next.op?.left ?? next.error?.left ?? []
      return { type: 'remove_partial', left, cmd: buildRemoveFilesCommand(left) }
    }
    if (next.op?.result === 'stop_failed' || corraldFailureKind(removeCode) === 'stop_failed') {
      const pid = next.op?.pid ?? next.error?.pid ?? next.running?.pid ?? 0
      return { type: 'remove_stop_failed', pid, cmd: buildStopCommand(pid) }
    }
    if (
      next.state === 'off' &&
      !next.error &&
      next.op?.state !== 'failed' &&
      (!next.op?.result || next.op.result === 'removed')
    ) {
      return { type: 'removed' }
    }
  }

  if (wasUpdating && !isOpActive(next.op) && next.state !== 'updating') {
    const failed =
      corraldFailureKind(next.error?.code) === 'update_failed' ||
      (next.op?.kind === 'update' && next.op.state === 'failed') ||
      next.state === 'error'
    if (!failed && (next.state === 'running' || (next.op?.kind === 'update' && next.op.state === 'done'))) {
      return { type: 'updated', version: next.version ?? '' }
    }
  }

  if (prev.state !== 'lost' && next.state === 'lost') {
    return { type: 'lost' }
  }
  if (prev.state !== 'error' && next.state === 'error' && !wasInstalling && !wasRemoving) {
    return { type: 'error', code: next.error?.code ?? next.op?.error_code ?? 'other' }
  }

  return null
}

// Classify the response of DELETE /api/hosts/{id} with optional corrald cleanup.
export function classifyDeleteHostResult(
  res: DeleteHostResult | null | undefined,
  requestedRemoveCorrald: boolean,
): DeleteHostOutcome {
  if (!requestedRemoveCorrald || !res) return { outcome: 'done' }
  const c = res.corrald
  if (res.reachable === false || c?.state === 'unreachable') {
    return { outcome: 'unreachable', cmd: c?.uninstall_cmd || DEFAULT_UNINSTALL_CMD }
  }
  if (c?.state === 'partial') {
    const left = c.left ?? []
    return { outcome: 'partial', left, cmd: buildRemoveFilesCommand(left) }
  }
  if (c?.state === 'stop_failed') {
    const pid = c.pid ?? 0
    return { outcome: 'stop_failed', pid, cmd: buildStopCommand(pid) }
  }
  if (c?.state === 'remove_pending') {
    return { outcome: 'remove_pending', cmd: c.uninstall_cmd || DEFAULT_UNINSTALL_CMD }
  }
  if (c?.state === 'removed') {
    return { outcome: 'removed' }
  }
  return { outcome: 'done' }
}

// Format measured latency in milliseconds as seconds for comparison rows
// (e.g. 300 ms -> "0.3", 1900 ms -> "1.9"; sub-0.1 s values keep two decimals).
export function formatPerfSeconds(ms: number): string {
  if (!Number.isFinite(ms) || ms <= 0) return '0.0'
  const s = ms / 1000
  if (s < 0.095) return s.toFixed(2)
  return s.toFixed(1)
}

function compareVersionStrings(a: string, b: string): number {
  const norm = (v: string) =>
    v
      .trim()
      .replace(/^v/, '')
      .split('-')[0]
      .split('.')
      .map((p) => Number.parseInt(p, 10))
  const pa = norm(a)
  const pb = norm(b)
  const n = Math.max(pa.length, pb.length)
  for (let i = 0; i < n; i++) {
    const va = Number.isFinite(pa[i]) ? pa[i] : 0
    const vb = Number.isFinite(pb[i]) ? pb[i] : 0
    if (va !== vb) return va - vb
  }
  return a.localeCompare(b)
}

function isNewerUpdateNotice(a: CorraldUpdateNotice, b: CorraldUpdateNotice): boolean {
  if (a.new_release !== b.new_release) return a.new_release > b.new_release
  if (a.new_version !== b.new_version) {
    const cmp = compareVersionStrings(a.new_version, b.new_version)
    if (cmp !== 0) return cmp > 0
  }
  if (a.created_at !== b.created_at) return a.created_at > b.created_at
  return a.id > b.id
}

// Merge all unseen successful update notices into one summary notice (showing
// the newest target version and distinct host count), while keeping failed
// notices individual per host.
export function groupCorraldUpdateNotices(notices: CorraldUpdateNotice[]): GroupedCorraldUpdateNotices {
  const allIds: number[] = []
  const successIds: number[] = []
  const successHosts = new Set<number>()
  let latestSuccess: CorraldUpdateNotice | null = null
  const failByHost = new Map<string, CorraldUpdateNotice>()

  for (const n of notices) {
    if (!n || typeof n.id !== 'number') continue
    allIds.push(n.id)
    if (n.success) {
      successIds.push(n.id)
      successHosts.add(n.host_id > 0 ? n.host_id : -n.id)
      if (!latestSuccess || isNewerUpdateNotice(n, latestSuccess)) {
        latestSuccess = n
      }
    } else {
      const key = `${n.host_id}:${n.old_version}:${n.new_version}`
      failByHost.set(key, n)
    }
  }

  return {
    mergedSuccess: latestSuccess
      ? {
          version: latestSuccess.new_version || '',
          hostCount: successHosts.size,
          ids: successIds,
        }
      : null,
    failures: [...failByHost.values()],
    allIds,
  }
}
