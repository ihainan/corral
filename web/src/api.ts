import { hasKey, t } from './i18n'

export interface Me {
  id: number
  username: string
  display_name: string
  role: 'admin' | 'member'
  totp?: boolean // two-step sign-in is on
  must_change_password?: boolean // an administrator set the password: choose one's own first
}

export interface Bootstrap {
  version: string
  instance?: string // this run of the gateway (a restart changes it)
  setup_needed: boolean
  default_language: string
  forwarding: boolean
  code: boolean // hosts can be opened in VS Code (code-server)
  upload_max_bytes: number
  recording: boolean // sessions are recorded
  recordings?: boolean // there are recordings this user may see (or sessions are recorded)
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
  jump_host_id: number | null // reached through this host of the user's
  persistence: 'none' | 'tmux' | 'herdr'
  tmux_session: string
  herdr_session: string
  created_at: number
  updated_at: number
  last_connected_at: number
  version: number // changes with every saved edit; sent back with an edit
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
  recording?: boolean // its output is being recorded
  watching?: number // windows of others watching it
  meta?: HerdrSummary // last side information published by the session
  persistence?: 'none' | 'tmux' | 'herdr' // what the shell runs in
  mux_session?: string // the tmux or herdr session ('' for herdr's default)
  preview_rev?: number // counts the screens windows sent (see SessionPreview)
}

// What one of the owner's windows last showed of a session's screen, as text.
export interface SessionPreview {
  cols: number
  lines: string[]
  at: number // unix seconds
  rev: number // as preview_rev
}

// What to open on a host (see the picker).
export interface OpenChoice {
  kind: 'shell' | 'tmux' | 'tmux_new' | 'herdr' | 'herdr_new'
  name?: string
}

// What a host has to open, as the gateway found it.
export interface HostChoices {
  choose: {
    tmux: boolean
    tmux_sessions: { name: string; windows: number; attached: boolean }[]
    herdr: boolean
    herdr_sessions: { name: string; default: boolean; running: boolean }[]
    unknown?: boolean
  }
  running: { id: string; kind: 'shell' | 'tmux' | 'herdr'; name: string; title: string; created_at: number }[]
  pending: string
}

// choiceOf is what a running terminal was opened as.
export function choiceOf(s: SessionInfo): OpenChoice | undefined {
  if (s.persistence === 'tmux') return { kind: 'tmux', name: s.mux_session ?? '' }
  if (s.persistence === 'herdr') return { kind: 'herdr', name: s.mux_session ?? '' }
  if (s.persistence === 'none') return { kind: 'shell' }
  return undefined
}

export interface AdminUser {
  id: number
  username: string
  display_name: string
  role: 'admin' | 'member'
  disabled: boolean
  totp: boolean
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
    if (this.code === 'jump_in_use') return t('error.jump_in_use', { hosts: ((this.extra.hosts as string[]) ?? []).join('、') })
    if (this.code === 'jump_failed') {
      const why = 'reason.' + String(this.extra.reason ?? '')
      return t('error.jump_failed') + (hasKey(why) ? t('reason.wrap', { reason: t(why as Parameters<typeof t>[0]) }) : '')
    }
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

// Counts sign-ins and sign-outs on this page. A 401 only signs the page out
// if no sign-in or sign-out happened since its request was sent: a late
// answer to a request of an earlier sign-in must not end the current one.
let authGeneration = 0
export const authGen = () => authGeneration
// startup: who is signed in learnt as the page starts, which changes no
// one: what was asked for early was asked as them, and is kept.
export function bumpAuthGen(startup = false) {
  authGeneration++
  if (!startup) early.clear() // (asked for as another sign-in, or none)
  else for (const e of early.values()) e.gen = authGeneration
}

// What every page asks for first, asked for as the page starts (askEarly),
// all at once, rather than each once the app is up and the one before has
// answered. The first ask of each takes that answer, while it is fresh and
// of the same sign-in. (Not a preload link: WebKit asks again for those.)
type Early = { at: number; gen: number; res: Promise<Response>; ctl: AbortController }
const early = new Map<string, Early>()
const EARLY_MS = 5000
export function askEarly(paths: string[]) {
  for (const p of paths) {
    const ctl = new AbortController()
    const res = fetch(p, { credentials: 'same-origin', signal: ctl.signal })
    res.catch(() => {}) // (one never taken may fail unseen; the one taking it asks again)
    early.set(p, { at: Date.now(), gen: authGeneration, res, ctl })
  }
}
function takeEarly(path: string): Early | undefined {
  const e = early.get(path)
  if (!e) return undefined
  early.delete(path)
  return e.gen === authGeneration && Date.now() - e.at <= EARLY_MS ? e : undefined
}
export function unauthorized(sentAt: number) {
  if (sentAt === authGeneration && onUnauthorized) onUnauthorized()
}

export interface ApiOptions {
  signal?: AbortSignal
  timeoutMs?: number
}

export async function api<T>(method: string, path: string, body?: unknown, opts: ApiOptions = {}): Promise<T> {
  let res: Response
  const sentAt = authGeneration
  const ctl = new AbortController()
  const onAbort = () => ctl.abort()
  if (opts.signal?.aborted) ctl.abort() // (cancelled before it was asked: not asked at all)
  opts.signal?.addEventListener('abort', onAbort)
  const timer = opts.timeoutMs ? setTimeout(() => ctl.abort(), opts.timeoutMs) : undefined
  let data: any = null
  try {
    try {
      const asked = method === 'GET' && body === undefined ? takeEarly(path) : undefined
      // (Its time out and its cancelling are this request's, as if asked now.)
      if (asked) ctl.signal.addEventListener('abort', () => asked.ctl.abort())
      const fresh = () =>
        fetch(path, {
          method,
          credentials: 'same-origin',
          signal: ctl.signal,
          headers: body !== undefined ? { 'Content-Type': 'application/json' } : undefined,
          body: body !== undefined ? JSON.stringify(body) : undefined,
        })
      // (One answered with a refusal is asked again: it may be of a moment
      // before — a password changed since, say.)
      res = asked ? await asked.res.then((r) => (r.ok ? r : fresh()), fresh) : await fresh()
      // The timeout and cancellation cover reading the body too.
      const text = await res.text()
      try {
        data = text ? JSON.parse(text) : null
      } catch {
        /* not JSON */
      }
    } catch {
      if (opts.signal?.aborted) throw new ApiError(0, 'aborted', '')
      throw new ApiError(0, 'network', t('error.network'))
    }
  } finally {
    clearTimeout(timer)
    opts.signal?.removeEventListener('abort', onAbort)
  }
  if (!res.ok) {
    if (res.status === 401 && path !== '/api/login') unauthorized(sentAt)
    throw new ApiError(res.status, data?.error ?? 'internal', data?.message ?? '', data?.extra)
  }
  return data as T
}

// prefix: what to type before the path for the program in front ("@" for
// Gemini CLI), as the gateway saw it.
export async function uploadFile(sessionId: string, file: Blob, onProgress?: (p: number) => void, signal?: AbortSignal): Promise<{ path: string; prefix?: string }> {
  return postFile(`/api/sessions/${sessionId}/upload`, file, onProgress, signal)
}

// postFile sends one file as a multipart upload, reporting progress; aborting
// the signal stops it (and rejects with code 'aborted').
export async function postFile<T = { path: string }>(url: string, file: Blob, onProgress?: (p: number) => void, signal?: AbortSignal): Promise<T> {
  const sentAt = authGeneration
  return new Promise((resolve, reject) => {
    const xhr = new XMLHttpRequest()
    xhr.open('POST', url)
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
      if (xhr.status >= 200 && xhr.status < 300) return resolve(data)
      if (xhr.status === 401) unauthorized(sentAt)
      reject(new ApiError(xhr.status, data?.error ?? 'internal', data?.message ?? '', data?.extra))
    }
    xhr.onerror = () => reject(new ApiError(0, 'network', t('error.network')))
    xhr.onabort = () => reject(new ApiError(0, 'aborted', ''))
    if (signal?.aborted) return reject(new ApiError(0, 'aborted', ''))
    signal?.addEventListener('abort', () => xhr.abort(), { once: true })
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
  session?: string // what runs there, as herdr says (Claude Code's session id)
  since?: number // (unix ms) when its status last changed, as the gateway saw it
  round?: number // which round of its status (what marking it seen names)
  // Claude Code done or idle: what it left running (subagents out: "running", bg_n of them, since bg_since).
  bg?: 'checking' | 'none' | 'running' | 'unknown'
  bg_n?: number
  bg_since?: number
  // Commands it left running in the background (done or idle): how many, the first listed.
  bg_cmds?: number
  bg_cmd_list?: { task: string; title?: string; since?: number }[]
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
  focused_agent?: string // the agent in the focused pane ("claude", "codex"…)
  focused_pane?: string
  lost?: boolean // the gateway lost herdr after this: it is what was so then
  keys?: Record<string, string> // bindings the host's herdr config sets (the shortcut panel's)
  keys_unknown?: boolean // not read (yet, or it failed): the panel presses none of herdr's keys
}

export interface Forward {
  id: string
  host_id: number
  port: number
  sharing: 'private' | 'users'
  created_at: number
  url: string
}

export interface ListeningPort {
  port: number
  address: string
}

export interface FileEntry {
  name: string
  type: 'dir' | 'file' | 'other' // for a link, what it points to
  link: boolean
  size: number
  mtime: number
  mode: string
}

export interface FileListing {
  path: string
  entries: FileEntry[]
  truncated: boolean
}

export interface Recording {
  id: string
  owner_id: number
  owner_name: string
  host_id: number
  host_name: string
  started_at: number
  ended_at: number // 0 while recording
  bytes: number
  truncated: boolean
  gaps: boolean
}

// A user the owner lets watch a session.
export interface Share {
  user_id: number
  username: string
  display_name: string
  watching: boolean
}

// A session someone else lets this user watch.
export interface SharedSession {
  id: string
  host_name: string
  owner_name: string
  state: string
  created_at: number
  title: string
}

// The start-up answer, or null when the gateway does not answer (within 5 s).
export async function probeBootstrap(): Promise<{ instance?: string; user?: { id: number } | null } | null> {
  const ctl = new AbortController()
  const tm = setTimeout(() => ctl.abort(), 5000)
  try {
    const r = await fetch('/api/bootstrap', { credentials: 'same-origin', cache: 'no-store', signal: ctl.signal })
    return r.ok ? await r.json() : null
  } catch {
    return null
  } finally {
    clearTimeout(tm)
  }
}
