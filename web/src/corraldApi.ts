// REST client for the host helper daemon (corrald), matching spec section 8.
import { api, ApiError } from './api'
import type {
  CorraldOp,
  CorraldStatus,
  DeleteHostOptions,
  DeleteHostResult,
} from './corraldLogic'

export type {
  CorraldActionId,
  CorraldConsent,
  CorraldError,
  CorraldFailureKind,
  CorraldFailureModel,
  CorraldLeftItem,
  CorraldOp,
  CorraldOpKind,
  CorraldOpState,
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
  CorraldUsers,
  DeleteHostOptions,
  DeleteHostOutcome,
  DeleteHostResult,
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
  return api<CorraldOp>('POST', `/api/hosts/${hostId}/corrald/install`, body)
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

// Lightweight shared pub/sub cache per host so HostEdit and the workbench
// status bar stay in sync without duplicate fetches.
type Listener = (status: CorraldStatus | null) => void
const cache = new Map<number, { at: number; status: CorraldStatus | null; unavailable?: boolean }>()
const listeners = new Map<number, Set<Listener>>()
const CACHE_TTL_MS = 30_000

export function readCachedCorrald(hostId: number): { status: CorraldStatus | null; unavailable?: boolean } | undefined {
  const hit = cache.get(hostId)
  if (!hit) return undefined
  if (Date.now() - hit.at > CACHE_TTL_MS && !hit.unavailable) return undefined
  return { status: hit.status, unavailable: hit.unavailable }
}

export function publishHostCorrald(hostId: number, status: CorraldStatus | null, unavailable = false) {
  cache.set(hostId, { at: Date.now(), status, unavailable })
  for (const fn of listeners.get(hostId) ?? []) fn(status)
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

export function forgetCorraldCache() {
  cache.clear()
}
