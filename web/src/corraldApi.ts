// REST client for the host helper daemon (corrald), matching spec section 8.
import { api, ApiError } from './api'
import type {
  CorraldAdminPolicy,
  CorraldChangelog,
  CorraldHint,
  CorraldHintResponse,
  CorraldOp,
  CorraldStatus,
  CorraldUpdateNotice,
  DeleteHostOptions,
  DeleteHostResult,
} from './corraldLogic'

export type {
  CorraldActionId,
  CorraldAdminPolicy,
  CorraldChangelog,
  CorraldChangelogEntry,
  CorraldConsent,
  CorraldError,
  CorraldFailureKind,
  CorraldFailureModel,
  CorraldHint,
  CorraldHintCachedStatus,
  CorraldHintController,
  CorraldHintControllerDeps,
  CorraldHintControllerState,
  CorraldHintMessageKey,
  CorraldHintOp,
  CorraldHintReason,
  CorraldHintResponse,
  CorraldLeftItem,
  CorraldOp,
  CorraldOpKind,
  CorraldOpState,
  CorraldPerfCompareItem,
  CorraldProgressModel,
  CorraldRemoveOutcome,
  CorraldRunning,
  CorraldScopeChange,
  CorraldSectionMode,
  CorraldSectionModel,
  CorraldState,
  CorraldStatus,
  CorraldStatusBarModel,
  CorraldTransition,
  CorraldUpdateNotice,
  CorraldUsers,
  DeleteHostOptions,
  DeleteHostOutcome,
  DeleteHostResult,
  GroupedCorraldUpdateNotices,
} from './corraldLogic'

export interface CorraldReadme {
  content: string
  title?: string
  manifest_hash?: string
  format: 'markdown' | 'text'
}

// Returns null when the gateway does not expose corrald (404).
export async function getHostCorrald(hostId: number, signal?: AbortSignal): Promise<CorraldStatus | null> {
  try {
    return await api<CorraldStatus>('GET', `/api/hosts/${hostId}/corrald`, undefined, { signal })
  } catch (e) {
    if (e instanceof ApiError && e.status === 404) return null
    throw e
  }
}

export async function installHostCorrald(
  hostId: number,
  body: { manifest_hash?: string; rebind?: boolean } = {},
): Promise<CorraldOp> {
  const op = await api<CorraldOp>('POST', `/api/hosts/${hostId}/corrald/install`, body)
  triggerCorraldHintsCheck()
  return op
}

export async function cancelHostCorrald(hostId: number): Promise<CorraldOp> {
  return api<CorraldOp>('POST', `/api/hosts/${hostId}/corrald/cancel`)
}

export async function pauseHostCorrald(hostId: number): Promise<void> {
  await api('POST', `/api/hosts/${hostId}/corrald/pause`)
}

export async function resumeHostCorrald(hostId: number): Promise<void> {
  await api('POST', `/api/hosts/${hostId}/corrald/resume`)
}

export async function removeHostCorrald(hostId: number): Promise<CorraldOp> {
  return api<CorraldOp>('POST', `/api/hosts/${hostId}/corrald/remove`)
}

export async function updateHostCorrald(
  hostId: number,
  body: { manifest_hash?: string; replace?: boolean } = {},
): Promise<CorraldOp> {
  return api<CorraldOp>('POST', `/api/hosts/${hostId}/corrald/update`, body)
}

export async function retryHostCorrald(hostId: number): Promise<CorraldOp> {
  return api<CorraldOp>('POST', `/api/hosts/${hostId}/corrald/retry`)
}

export async function deleteHostWithOptions(
  hostId: number,
  opts?: DeleteHostOptions,
): Promise<DeleteHostResult | null> {
  return api<DeleteHostResult | null>('DELETE', `/api/hosts/${hostId}`, opts)
}

export async function getCorraldReadme(signal?: AbortSignal): Promise<CorraldReadme> {
  return api<CorraldReadme>('GET', '/api/corrald/readme', undefined, { signal })
}

export async function getCorraldChangelog(version?: string, signal?: AbortSignal): Promise<CorraldChangelog> {
  const qs = version ? `?version=${encodeURIComponent(version)}` : ''
  return api<CorraldChangelog>('GET', '/api/corrald/changelog' + qs, undefined, { signal })
}

export async function markCorraldChangelogSeen(version: string, release?: number): Promise<void> {
  await api('POST', '/api/corrald/changelog/seen', { version, ...(release ? { release } : {}) })
}

export async function getCorraldNotices(signal?: AbortSignal): Promise<CorraldUpdateNotice[]> {
  const res = await api<{ notices?: CorraldUpdateNotice[] }>('GET', '/api/corrald/notices', undefined, { signal })
  return Array.isArray(res?.notices) ? res.notices : []
}

export async function markCorraldNoticesSeen(ids: number[]): Promise<void> {
  if (ids.length === 0) return
  await api('POST', '/api/corrald/notices/seen', { ids })
}

export async function getAdminCorraldPolicy(signal?: AbortSignal): Promise<CorraldAdminPolicy> {
  return api<CorraldAdminPolicy>('GET', '/api/admin/corrald', undefined, { signal })
}

export async function setAdminCorraldPolicy(allowed: boolean): Promise<CorraldAdminPolicy> {
  const res = await api<CorraldAdminPolicy>('PUT', '/api/admin/corrald', { allowed })
  if (!res.allowed) {
    for (const [hostId, entry] of cache) {
      if (entry.status) {
        publishHostCorrald(hostId, { ...entry.status, admin_disabled: true }, entry.unavailable)
      }
    }
    for (const fn of adminPolicyListeners) fn(true)
  } else {
    for (const fn of adminPolicyListeners) fn(false)
  }
  triggerCorraldHintsCheck()
  return res
}

export const CORRALD_NOTICES_EVENT = 'corral.corraldNoticesRefresh'
export const CORRALD_HINTS_EVENT = 'corral.corraldHintsRefresh'

export function triggerCorraldNoticesCheck() {
  if (typeof window !== 'undefined') {
    window.dispatchEvent(new CustomEvent(CORRALD_NOTICES_EVENT))
  }
}

export function triggerCorraldHintsCheck(detail?: { resetSession?: boolean }) {
  if (typeof window !== 'undefined') {
    window.dispatchEvent(new CustomEvent(CORRALD_HINTS_EVENT, { detail }))
  }
}

// Fetch the single most recent eligible contextual corrald hint and current
// per-user reset generation.
export async function getCorraldHint(signal?: AbortSignal): Promise<CorraldHintResponse> {
  try {
    const res = await api<{ hint?: CorraldHint | null; generation?: number }>('GET', '/api/corrald/hints', undefined, { signal })
    const generation = typeof res?.generation === 'number' && Number.isFinite(res.generation) ? res.generation : 0
    return { hint: res?.hint ?? null, generation }
  } catch (e) {
    if (e instanceof ApiError && e.status === 404) return { hint: null, generation: 0 }
    throw e
  }
}

export async function claimCorraldHintShown(hostId: number, generation = 0): Promise<boolean> {
  const res = await api<{ claimed?: boolean }>('POST', `/api/hosts/${hostId}/corrald/hint/shown`, { generation })
  return res?.claimed === true
}

export async function dismissCorraldHint(hostId: number, generation = 0): Promise<void> {
  await api('POST', `/api/hosts/${hostId}/corrald/hint/dismiss`, { generation })
}

// Lightweight shared pub/sub cache per host so HostEdit, the workbench
// status bar, and the contextual hint bar stay in sync without duplicate fetches.
type Listener = (status: CorraldStatus | null, unavailable?: boolean) => void
type AdminPolicyListener = (adminDisabled: boolean) => void
const cache = new Map<number, { at: number; status: CorraldStatus | null; unavailable?: boolean }>()
const listeners = new Map<number, Set<Listener>>()
const adminPolicyListeners = new Set<AdminPolicyListener>()
const CACHE_TTL_MS = 30_000

export function readCachedCorrald(hostId: number): { status: CorraldStatus | null; unavailable?: boolean } | undefined {
  const hit = cache.get(hostId)
  if (!hit) return undefined
  if (Date.now() - hit.at > CACHE_TTL_MS && !hit.unavailable) return undefined
  return { status: hit.status, unavailable: hit.unavailable }
}

export function publishHostCorrald(hostId: number, status: CorraldStatus | null, unavailable = false) {
  cache.set(hostId, { at: Date.now(), status, unavailable })
  for (const fn of listeners.get(hostId) ?? []) fn(status, unavailable)
}

export function subscribeHostCorrald(hostId: number, fn: Listener): () => void {
  let set = listeners.get(hostId)
  if (!set) {
    set = new Set()
    listeners.set(hostId, set)
  }
  set.add(fn)
  return () => {
    set!.delete(fn)
    if (set!.size === 0) listeners.delete(hostId)
  }
}

export function subscribeAdminCorraldPolicy(fn: AdminPolicyListener): () => void {
  adminPolicyListeners.add(fn)
  return () => {
    adminPolicyListeners.delete(fn)
  }
}

export function forgetCorraldCache() {
  cache.clear()
}
