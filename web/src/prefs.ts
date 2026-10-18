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
    if (raw) return { ...defaults, ...JSON.parse(raw) }
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

// Saving. Only changed fields are sent, each stamped with the time it was
// changed; the server keeps a field only if the stamp is newer than what it
// has (see apiPatchSettings). So requests may arrive in any order, a
// keepalive may race a normal save, and another device's newer change is
// never undone. Pending changes belong to one account and one sign-in:
// signing out or in drops them, and a save already queued for the previous
// account is not sent.
type Pending = Map<string, { value: unknown; ts: number }>
let pending: Pending = new Map()
let generation = 0
let lastTS = 0
let saveTimer: number | undefined
let queue: Promise<void> = Promise.resolve()

const stamp = () => (lastTS = Math.max(lastTS + 1, Date.now()))

function body(p: Pending) {
  const set: Record<string, unknown> = {}
  const ts: Record<string, number> = {}
  for (const [k, v] of p) {
    set[k] = v.value
    ts[k] = v.ts
  }
  return { set, ts }
}

export function setPrefs(patch: Partial<Prefs>) {
  state = { ...state, ...patch }
  for (const [k, v] of Object.entries(patch)) pending.set(k, { value: v, ts: stamp() })
  emit()
  clearTimeout(saveTimer)
  saveTimer = window.setTimeout(save, 600)
}

function save() {
  clearTimeout(saveTimer)
  saveTimer = undefined
  const gen = generation
  queue = queue.then(async () => {
    if (gen !== generation || pending.size === 0) return
    const sent = new Map(pending)
    try {
      await api('PATCH', '/api/me/settings', body(sent))
      // Drop what was confirmed, unless it changed again meanwhile.
      for (const [k, v] of sent) if (pending.get(k)?.ts === v.ts) pending.delete(k)
    } catch {
      /* kept; goes with the next save or the keepalive on leaving */
    }
  })
}

// Sends what is still pending now, e.g. before signing out.
export async function flushPrefs() {
  save()
  await queue
}

// Leaving the page: pending fields go out with keepalive, which survives
// the unload. Their stamps make a duplicate or late arrival harmless.
function flush() {
  if (pending.size === 0 || owner === null) return
  clearTimeout(saveTimer)
  saveTimer = undefined
  void fetch('/api/me/settings', {
    method: 'PATCH', credentials: 'same-origin', keepalive: true,
    headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body(pending)),
  }).catch(() => {})
}
window.addEventListener('pagehide', flush)
document.addEventListener('visibilitychange', () => document.visibilityState === 'hidden' && flush())

// Forget the signed-in account's pending work (sign-out).
export function resetPrefsSession() {
  generation++
  pending = new Map()
  clearTimeout(saveTimer)
  saveTimer = undefined
  owner = null
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
  try {
    const remote = await api<Partial<Prefs>>('GET', '/api/me/settings')
    if (gen !== generation || owner !== userId) return // signed out or switched meanwhile
    // The server's copy, except for fields changed here and not yet confirmed.
    const local: Partial<Prefs> = {}
    for (const [k, v] of pending) (local as Record<string, unknown>)[k] = v.value
    state = { ...defaults, ...remote, ...local }
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
