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

let state: Prefs = load()
const subs = new Set<() => void>()

function load(): Prefs {
  try {
    const raw = localStorage.getItem('corral.prefs')
    if (raw) return { ...defaults, ...JSON.parse(raw) }
  } catch {
    /* ignore */
  }
  return { ...defaults }
}

function emit() {
  try {
    localStorage.setItem('corral.prefs', JSON.stringify(state))
  } catch {
    /* ignore */
  }
  applyTheme()
  subs.forEach((s) => s())
}

// Saving. Every change bumps `version`; `confirmed` is the newest version
// the server has acknowledged. PUTs go out one at a time, so an older
// snapshot can never land after a newer one. Until the latest change is
// confirmed, a marker in localStorage names the account it belongs to: the
// next load on this device then pushes the local settings instead of letting
// the server's older copy overwrite them.
const DIRTY = 'corral.prefsDirty'
let version = 0
let confirmed = 0
let owner: number | null = null
let saveTimer: number | undefined
let queue: Promise<void> = Promise.resolve()

function markDirty() {
  try {
    localStorage.setItem(DIRTY, owner === null ? '' : String(owner))
  } catch {
    /* ignore */
  }
}
function clearDirty() {
  try {
    localStorage.removeItem(DIRTY)
  } catch {
    /* ignore */
  }
}

export function setPrefs(patch: Partial<Prefs>) {
  state = { ...state, ...patch }
  version++
  markDirty()
  emit()
  clearTimeout(saveTimer)
  saveTimer = window.setTimeout(save, 600)
}

function save() {
  clearTimeout(saveTimer)
  saveTimer = undefined
  queue = queue.then(async () => {
    const v = version
    const snapshot = state
    try {
      await api('PUT', '/api/me/settings', snapshot)
      if (v > confirmed) confirmed = v
      if (confirmed === version) clearDirty()
    } catch {
      /* stays dirty; retried on the next change or the next load */
    }
  })
}

// Leaving the page: whatever is not confirmed yet goes out with keepalive,
// which survives the unload. The marker stays until a later load confirms it.
function flush() {
  if (confirmed === version) return
  clearTimeout(saveTimer)
  saveTimer = undefined
  void fetch('/api/me/settings', {
    method: 'PUT', credentials: 'same-origin', keepalive: true,
    headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(state),
  }).catch(() => {})
}
window.addEventListener('pagehide', flush)
document.addEventListener('visibilitychange', () => document.visibilityState === 'hidden' && flush())

export async function syncPrefsFromServer(userId: number) {
  owner = userId
  let dirtyFor: string | null = null
  try {
    dirtyFor = localStorage.getItem(DIRTY)
  } catch {
    /* ignore */
  }
  if (dirtyFor === String(userId)) {
    // This account changed something here that the server may not have.
    version++
    save()
    return
  }
  if (dirtyFor !== null) clearDirty() // another account's leftovers
  const started = version
  try {
    const remote = await api<Partial<Prefs>>('GET', '/api/me/settings')
    if (version !== started) return // changed meanwhile; that change wins
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
