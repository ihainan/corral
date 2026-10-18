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

let saveTimer: number | undefined
export function setPrefs(patch: Partial<Prefs>) {
  state = { ...state, ...patch }
  emit()
  clearTimeout(saveTimer)
  saveTimer = window.setTimeout(save, 600)
}

function save() {
  saveTimer = undefined
  api('PUT', '/api/me/settings', state).catch(() => {})
}

// A change made just before a reload or closing the tab must still reach
// the server, or the next page load would read the old value back.
function flush() {
  if (saveTimer === undefined) return
  clearTimeout(saveTimer)
  saveTimer = undefined
  void fetch('/api/me/settings', {
    method: 'PUT', credentials: 'same-origin', keepalive: true,
    headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(state),
  }).catch(() => {})
}
window.addEventListener('pagehide', flush)
document.addEventListener('visibilitychange', () => document.visibilityState === 'hidden' && flush())

export async function syncPrefsFromServer() {
  try {
    const remote = await api<Partial<Prefs>>('GET', '/api/me/settings')
    if (saveTimer !== undefined) return // a change made meanwhile wins; it is on its way
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
