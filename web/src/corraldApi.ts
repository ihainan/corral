// REST client for the host helper daemon (corrald), matching spec section 8.
import { api, ApiError, authGen, unauthorized } from './api'
import { t } from './i18n'
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

function unwrapOp(res: CorraldOp | { op: CorraldOp } | null | undefined): CorraldOp | null {
  if (!res) return null
  if ('op' in res && res.op) return res.op
  return res as CorraldOp
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
): Promise<CorraldOp | null> {
  const res = await api<CorraldOp | { op: CorraldOp }>('POST', `/api/hosts/${hostId}/corrald/install`, body)
  return unwrapOp(res)
}

export async function cancelHostCorrald(hostId: number): Promise<CorraldOp | null> {
  const res = await api<CorraldOp | { op: CorraldOp } | null>('POST', `/api/hosts/${hostId}/corrald/cancel`)
  return unwrapOp(res)
}

export async function pauseHostCorrald(hostId: number): Promise<void> {
  await api('POST', `/api/hosts/${hostId}/corrald/pause`)
}

export async function resumeHostCorrald(hostId: number): Promise<void> {
  await api('POST', `/api/hosts/${hostId}/corrald/resume`)
}

export async function removeHostCorrald(hostId: number): Promise<CorraldOp | null> {
  const res = await api<CorraldOp | { op: CorraldOp }>('POST', `/api/hosts/${hostId}/corrald/remove`)
  return unwrapOp(res)
}

export async function updateHostCorrald(
  hostId: number,
  body: { manifest_hash?: string; replace?: boolean } = {},
): Promise<CorraldOp | null> {
  const res = await api<CorraldOp | { op: CorraldOp }>('POST', `/api/hosts/${hostId}/corrald/update`, body)
  return unwrapOp(res)
}

export async function retryHostCorrald(hostId: number): Promise<CorraldOp | null> {
  const res = await api<CorraldOp | { op: CorraldOp }>('POST', `/api/hosts/${hostId}/corrald/retry`)
  return unwrapOp(res)
}

export async function deleteHostWithOptions(
  hostId: number,
  opts?: DeleteHostOptions,
): Promise<DeleteHostResult | null> {
  return api<DeleteHostResult | null>('DELETE', `/api/hosts/${hostId}`, opts)
}

// Fetches GET /api/corrald/readme. Accepts either plain text/Markdown
// (matching README.txt directly) or JSON ({content|text|markdown, title?, manifest_hash?}).
export async function getCorraldReadme(signal?: AbortSignal): Promise<CorraldReadme> {
  const sentAt = authGen()
  let res: Response
  let raw: string
  try {
    res = await fetch('/api/corrald/readme', { credentials: 'same-origin', signal })
    raw = await res.text()
  } catch {
    if (signal?.aborted) throw new ApiError(0, 'aborted', '')
    throw new ApiError(0, 'network', t('error.network'))
  }
  const ct = (res.headers.get('content-type') ?? '').toLowerCase()
  let parsed: Record<string, unknown> | null = null
  if (ct.includes('application/json') || raw.trimStart().startsWith('{')) {
    try {
      const v = JSON.parse(raw)
      if (v && typeof v === 'object') parsed = v as Record<string, unknown>
    } catch {
      /* treat as plain text */
    }
  }
  if (!res.ok) {
    if (res.status === 401) unauthorized(sentAt)
    const code = String(parsed?.error ?? parsed?.code ?? 'internal')
    const msg = String(parsed?.message ?? '')
    throw new ApiError(res.status, code, msg, parsed?.extra as Record<string, unknown> | undefined)
  }
  if (parsed) {
    const content = String(parsed.content ?? parsed.markdown ?? parsed.text ?? '')
    const isMd = ct.includes('markdown') || typeof parsed.markdown === 'string' || looksLikeMarkdown(content)
    return {
      content,
      title: typeof parsed.title === 'string' ? parsed.title : undefined,
      manifest_hash: typeof parsed.manifest_hash === 'string' ? parsed.manifest_hash : undefined,
      format: isMd ? 'markdown' : 'text',
    }
  }
  const isMd = ct.includes('markdown') || looksLikeMarkdown(raw)
  return {
    content: raw,
    format: isMd ? 'markdown' : 'text',
  }
}

function looksLikeMarkdown(s: string): boolean {
  return /(^|\n)#{1,6}\s+/.test(s) || /(^|\n)[-*]\s+/.test(s) || /```/.test(s)
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
