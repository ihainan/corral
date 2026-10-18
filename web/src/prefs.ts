import { useSyncExternalStore } from 'react'
import { api } from './api'

// Per-user preferences. Cached locally for a fast first paint, synced to the
// server so a phone and a laptop share them.

export interface Prefs {
  theme: 'system' | 'light' | 'dark'
  fontSize: number
  keybar: boolean
  optionIsMeta: boolean
  webgl: boolean
}

const defaults: Prefs = { theme: 'system', fontSize: 14, keybar: true, optionIsMeta: true, webgl: false }

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
  saveTimer = window.setTimeout(() => {
    api('PUT', '/api/me/settings', state).catch(() => {})
  }, 600)
}

export async function syncPrefsFromServer() {
  try {
    const remote = await api<Partial<Prefs>>('GET', '/api/me/settings')
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
  applyTheme()
  subs.forEach((s) => s())
})

export function isDark(): boolean {
  return state.theme === 'dark' || (state.theme === 'system' && media.matches)
}

export function applyTheme() {
  const root = document.documentElement
  if (state.theme === 'system') root.removeAttribute('data-theme')
  else root.setAttribute('data-theme', state.theme)
}

applyTheme()
