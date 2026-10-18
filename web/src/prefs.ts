import { useSyncExternalStore } from 'react'
import { api } from './api'
import { resolveScheme, type Scheme } from './terminal/themes'

// Per-user preferences. Cached locally for a fast first paint, synced to the
// server so a phone and a laptop share them.

export interface Prefs {
  theme: 'system' | 'light' | 'dark'
  // Terminal colours are separate from the page theme: most people want a
  // dark terminal even in a light UI. A scheme id, or "ui" (see resolveScheme).
  termTheme: string
  fontSize: number
  keybar: boolean
  optionIsMeta: boolean
  webgl: boolean
}

// Phones get a slightly smaller default so common output fits without wrapping.
const phone = window.matchMedia('(pointer: coarse) and (max-width: 599px)').matches
const defaults: Prefs = { theme: 'system', termTheme: 'corral-dark', fontSize: phone ? 13 : 14, keybar: true, optionIsMeta: true, webgl: false }

// The local copy is kept per account (several people may share a device);
// until the account is known the last one used here paints the first frame.
const LAST = 'corral.prefs.last'
const cacheKey = (uid: number | null) => (uid === null ? null : 'corral.prefs.' + uid)

function readCache(uid: number | null): Prefs {
  try {
    const key = cacheKey(uid ?? Number(localStorage.getItem(LAST) ?? NaN))
    const raw = key && !key.endsWith('NaN') ? localStorage.getItem(key) : null
    if (raw) return { ...defaults, ...sanitize(JSON.parse(raw)) }
  } catch {
    /* ignore */
  }
  return { ...defaults }
}

let owner: number | null = null
let state: Prefs = readCache(null)
const subs = new Set<() => void>()

function emit() {
  try {
    const key = cacheKey(owner)
    if (key) localStorage.setItem(key, JSON.stringify(state))
  } catch {
    /* ignore */
  }
  applyTheme()
  subs.forEach((s) => s())
}

// Saving. Only changed fields are sent. The server takes the later arrival,
// except that this page's own older request never undoes its newer one: each
// page load is a writer with its own request numbers (see apiPatchSettings).
// No clock is involved. Each request names the account it is for, and the
// server refuses it if the browser is signed in as someone else by then.
// Pending changes belong to one sign-in: signing out or in drops them, and
// requests of an earlier sign-in are never sent.
const writer = Array.from(crypto.getRandomValues(new Uint8Array(12)), (b) => b.toString(36).padStart(2, '0')).join('')
let seq = 0
let edits = 0 // counts changes made on this page
const editedAt = new Map<string, number>() // field -> edits count when last changed here
let pending = new Map<string, { value: unknown; edit: number }>()
let generation = 0
let saveTimer: number | undefined
let queue: Promise<void> = Promise.resolve()
let retryTimer: number | undefined

function request(p: typeof pending) {
  const set: Record<string, unknown> = {}
  for (const [k, v] of p) set[k] = v.value
  return { user: owner, writer, seq: ++seq, set }
}

export function setPrefs(patch: Partial<Prefs>) {
  state = { ...state, ...patch }
  for (const [k, v] of Object.entries(patch)) {
    edits++
    editedAt.set(k, edits)
    pending.set(k, { value: v, edit: edits })
  }
  emit()
  clearTimeout(saveTimer)
  saveTimer = window.setTimeout(save, 600)
}

function save() {
  clearTimeout(saveTimer)
  saveTimer = undefined
  const gen = generation
  queue = queue.then(async () => {
    if (gen !== generation || pending.size === 0 || owner === null) return
    const sent = new Map(pending)
    const ctl = new AbortController()
    const timer = window.setTimeout(() => ctl.abort(), 10000)
    try {
      const res = await fetch('/api/me/settings', {
        method: 'PATCH', credentials: 'same-origin', signal: ctl.signal,
        headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(request(sent)),
      })
      if (gen !== generation) return
      if (res.ok) {
        // Accepted or superseded by this page's own newer request: either way
        // done, unless the field changed again meanwhile.
        for (const [k, v] of sent) if (pending.get(k)?.edit === v.edit) pending.delete(k)
      } else if (res.status === 400 || res.status === 401 || res.status === 409) {
        // Refused for good: invalid, signed out, or now another account.
        for (const [k, v] of sent) if (pending.get(k)?.edit === v.edit) pending.delete(k)
      } else scheduleRetry()
    } catch {
      if (gen === generation) scheduleRetry()
    } finally {
      clearTimeout(timer)
    }
  })
}

function scheduleRetry() {
  clearTimeout(retryTimer)
  retryTimer = window.setTimeout(() => pending.size && save(), 15000)
}
window.addEventListener('online', () => pending.size && save())

// Sends what is still pending now, e.g. before signing out.
export async function flushPrefs() {
  save()
  await queue
}

// Leaving the page: pending fields go out with keepalive, which survives
// the unload; the request number keeps a late arrival from undoing anything.
function flush() {
  if (pending.size === 0 || owner === null) return
  clearTimeout(saveTimer)
  saveTimer = undefined
  void fetch('/api/me/settings', {
    method: 'PATCH', credentials: 'same-origin', keepalive: true,
    headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(request(pending)),
  }).catch(() => {})
}
window.addEventListener('pagehide', flush)
document.addEventListener('visibilitychange', () => {
  if (document.visibilityState === 'hidden') flush()
  else if (pending.size) save()
})

// Another tab signed in as someone else: this page's account is gone.
window.addEventListener('storage', (e) => {
  if (e.key === LAST && owner !== null && e.newValue !== String(owner)) location.reload()
})

// Forget the signed-in account's pending work (sign-out).
export function resetPrefsSession() {
  generation++
  pending = new Map()
  editedAt.clear()
  queue = Promise.resolve() // requests of the old sign-in no longer hold up new ones
  clearTimeout(saveTimer)
  clearTimeout(retryTimer)
  saveTimer = undefined
  owner = null
}

// Values from the server or the local cache are checked, not trusted.
function sanitize(raw: unknown): Partial<Prefs> {
  const o = (raw && typeof raw === 'object' ? raw : {}) as Record<string, unknown>
  const out: Partial<Prefs> = {}
  if (o.theme === 'system' || o.theme === 'light' || o.theme === 'dark') out.theme = o.theme
  if (typeof o.termTheme === 'string' && /^[a-z0-9][a-z0-9-]{0,63}$/.test(o.termTheme)) out.termTheme = o.termTheme
  if (Number.isInteger(o.fontSize) && (o.fontSize as number) >= 9 && (o.fontSize as number) <= 28) out.fontSize = o.fontSize as number
  for (const k of ['keybar', 'optionIsMeta', 'webgl'] as const) if (typeof o[k] === 'boolean') out[k] = o[k] as boolean
  return out
}

export async function syncPrefsFromServer(userId: number) {
  if (owner !== userId) {
    resetPrefsSession()
    owner = userId
    state = readCache(userId)
    try {
      localStorage.setItem(LAST, String(userId))
    } catch {
      /* ignore */
    }
    emit()
  }
  const gen = generation
  const startedAt = edits
  try {
    const remote = sanitize(await api<unknown>('GET', '/api/me/settings'))
    if (gen !== generation || owner !== userId) return // signed out or switched meanwhile
    // Fields changed on this page after the request went out keep their
    // local value, saved or not: the response may predate them.
    for (const [k, at] of editedAt) if (at > startedAt) delete (remote as Record<string, unknown>)[k]
    state = { ...defaults, ...state, ...remote }
    emit()
  } catch {
    /* keep local */
  }
}

export function getPrefs(): Prefs {
  return state
}

export function usePrefs(): Prefs {
  return useSyncExternalStore(
    (cb) => {
      subs.add(cb)
      return () => subs.delete(cb)
    },
    () => state,
  )
}

const media = window.matchMedia('(prefers-color-scheme: dark)')
media.addEventListener('change', () => {
  // A new snapshot, so components reading isDark()/isTermDark() re-render.
  state = { ...state }
  applyTheme()
  subs.forEach((s) => s())
})

export function isDark(): boolean {
  return state.theme === 'dark' || (state.theme === 'system' && media.matches)
}

export function termScheme(): Scheme {
  return resolveScheme(state.termTheme, isDark())
}

export function isTermDark(): boolean {
  return termScheme().dark
}

// The browser chrome colour (status bar, tab strip) follows the page, or the
// terminal while one is open.
let chromeOverride: string | null = null
export function setChromeColor(c: string | null) {
  chromeOverride = c
  paintChrome()
}
function paintChrome() {
  const c = chromeOverride ?? (isDark() ? '#0f1115' : '#ffffff')
  document.querySelectorAll('meta[name="theme-color"]').forEach((m) => m.setAttribute('content', c))
}

export function applyTheme() {
  const root = document.documentElement
  if (state.theme === 'system') root.removeAttribute('data-theme')
  else root.setAttribute('data-theme', state.theme)
  paintChrome()
}

applyTheme()
