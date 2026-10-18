import { hasKey, t } from './i18n'

export interface Me {
  id: number
  username: string
  display_name: string
  role: 'admin' | 'member'
}

export interface Bootstrap {
  version: string
  setup_needed: boolean
  default_language: string
  forwarding: boolean
  upload_max_bytes: number
  user: Me | null
}

export interface Host {
  id: number
  name: string
  address: string
  port: number
  username: string
  auth: 'password' | 'key' | 'ask'
  credential_id: number | null
  persistence: 'none' | 'tmux' | 'herdr'
  tmux_session: string
  herdr_session: string
  created_at: number
  updated_at: number
  last_connected_at: number
}

export interface KeyCred {
  id: number
  name: string
  kind: 'private_key'
  public_key: string
  created_at: number
}

export interface SessionInfo {
  id: string
  host_id: number
  host_name: string
  state: 'running' | 'exited' | 'lost' | 'closed'
  created_at: number
  viewers: number
  cols: number
  rows: number
  title: string
}

export interface AdminUser {
  id: number
  username: string
  display_name: string
  role: 'admin' | 'member'
  disabled: boolean
  created_at: number
  open_sessions: number
}

export interface AuditEntry {
  id: number
  ts: number
  user_id: number | null
  username: string
  action: string
  target: string
  ip: string
  detail: Record<string, unknown>
}

export class ApiError extends Error {
  status: number
  code: string
  extra: Record<string, unknown>
  constructor(status: number, code: string, message: string, extra?: Record<string, unknown>) {
    super(message)
    this.status = status
    this.code = code
    this.extra = extra ?? {}
  }
  // A sentence for the user, from the error code when we have a translation.
  get human(): string {
    if (this.code === 'locked') return t('error.locked', { s: Number(this.extra.retry_after ?? 60) })
    if (this.code === 'in_use') return t('error.in_use', { hosts: ((this.extra.hosts as string[]) ?? []).join('、') })
    const key = 'error.' + this.code
    if (hasKey(key)) return t(key)
    if (this.code === 'network') return t('error.network')
    return this.message || t('error.internal')
  }
  get field(): string | undefined {
    return this.extra.field as string | undefined
  }
}

let onUnauthorized: (() => void) | null = null
export function setUnauthorizedHandler(fn: () => void) {
  onUnauthorized = fn
}

export async function api<T>(method: string, path: string, body?: unknown): Promise<T> {
  let res: Response
  try {
    res = await fetch(path, {
      method,
      credentials: 'same-origin',
      headers: body !== undefined ? { 'Content-Type': 'application/json' } : undefined,
      body: body !== undefined ? JSON.stringify(body) : undefined,
    })
  } catch {
    throw new ApiError(0, 'network', t('error.network'))
  }
  const text = await res.text()
  let data: any = null
  try {
    data = text ? JSON.parse(text) : null
  } catch {
    /* not JSON */
  }
  if (!res.ok) {
    if (res.status === 401 && path !== '/api/login' && onUnauthorized) onUnauthorized()
    throw new ApiError(res.status, data?.error ?? 'internal', data?.message ?? '', data?.extra)
  }
  return data as T
}

export async function uploadFile(sessionId: string, file: Blob, onProgress?: (p: number) => void): Promise<{ path: string }> {
  return new Promise((resolve, reject) => {
    const xhr = new XMLHttpRequest()
    xhr.open('POST', `/api/sessions/${sessionId}/upload`)
    xhr.withCredentials = true
    xhr.upload.onprogress = (e) => {
      if (e.lengthComputable && onProgress) onProgress(e.loaded / e.total)
    }
    xhr.onload = () => {
      let data: any = null
      try {
        data = JSON.parse(xhr.responseText)
      } catch {
        /* ignore */
      }
      if (xhr.status >= 200 && xhr.status < 300) resolve(data)
      else reject(new ApiError(xhr.status, data?.error ?? 'internal', data?.message ?? '', data?.extra))
    }
    xhr.onerror = () => reject(new ApiError(0, 'network', t('error.network')))
    const fd = new FormData()
    fd.append('file', file, 'upload')
    xhr.send(fd)
  })
}

export function errorText(e: unknown): string {
  if (e instanceof ApiError) return e.human
  return t('error.internal')
}

export interface HerdrAgent {
  pane_id: string
  agent: string
  status: string
  title: string
  cwd: string
}

export interface HerdrWorkspace {
  id: string
  label: string
  number: number
  status: string
  focused: boolean
  agents: HerdrAgent[]
}

export interface HerdrSummary {
  t: 'herdr'
  session: string
  version: string
  workspaces: HerdrWorkspace[]
  waiting: number
}
