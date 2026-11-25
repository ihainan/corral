import { useEffect, useSyncExternalStore } from 'react'
import { ApiError, api, onAuthChange } from '../api'

export type TranslateMode = 'translate' | 'summarize'

export interface TranslateConfig {
  enabled: boolean
  mode: TranslateMode
  target_lang: string
  base_url: string
  model: string
  has_key: boolean
}

export interface TranslateConfigPatch {
  enabled?: boolean
  mode?: TranslateMode
  target_lang?: string
  base_url?: string
  model?: string
  api_key?: string
  clear_key?: boolean
}

export interface TranslateTestResult {
  ok: boolean
  mode?: TranslateMode
  original?: string
  sample: string
  latency_ms: number
}

export interface ThoughtTranslationState {
  enabled: boolean
  mode: TranslateMode
  status: 'idle' | 'loading' | 'done' | 'skipped' | 'error'
  text?: string
  error?: string
}

export const TRANSLATE_LANGS = [
  { code: 'zh-CN', label: '简体中文' },
  { code: 'zh-TW', label: '繁體中文' },
  { code: 'en', label: 'English' },
  { code: 'ja', label: '日本語' },
  { code: 'ko', label: '한국어' },
  { code: 'fr', label: 'Français' },
  { code: 'de', label: 'Deutsch' },
  { code: 'es', label: 'Español' },
] as const

interface CacheEntry {
  status: 'loading' | 'done' | 'skipped' | 'error'
  text?: string
  error?: string
}

const MAX_CACHE = 500
const BATCH_MAX = 8
const MAX_INFLIGHT = 2
const BATCH_DELAY_MS = 80

let config: TranslateConfig | null = null
let configLoaded = false
let configPromise: Promise<TranslateConfig | null> | null = null
let storeVer = 0
// (Which account's the store is: an answer asked for another, before a sign-out, is taken for nothing — review 6.)
let storeGen = 0

const cache = new Map<string, CacheEntry>()
const listeners = new Set<() => void>()
const queue: { key: string; mode: TranslateMode; lang: string; text: string }[] = []
const queuedKeys = new Set<string>()
let flushTimer: ReturnType<typeof setTimeout> | null = null
let inFlight = 0

function notify() {
  storeVer++
  for (const fn of listeners) fn()
}

function subscribe(fn: () => void): () => void {
  listeners.add(fn)
  return () => {
    listeners.delete(fn)
  }
}

function resetStore() {
  storeGen++
  config = null
  configLoaded = false
  configPromise = null
  cache.clear()
  queue.length = 0
  queuedKeys.clear()
  if (flushTimer) {
    clearTimeout(flushTimer)
    flushTimer = null
  }
  notify()
}

onAuthChange(resetStore)

export function ensureTranslateConfig(force = false): Promise<TranslateConfig | null> {
  if (!force && configLoaded) return Promise.resolve(config)
  if (!force && configPromise) return configPromise
  const g = storeGen
  const p = api<TranslateConfig>('GET', '/api/me/translate')
    .then((c) => {
      if (g !== storeGen) return config
      config = c
      configLoaded = true
      notify()
      return c
    })
    .catch(() => {
      if (g !== storeGen) return config
      configLoaded = true
      notify()
      return config
    })
    .finally(() => {
      if (configPromise === p) configPromise = null
    })
  configPromise = p
  return p
}

export async function saveTranslateConfig(patch: TranslateConfigPatch): Promise<TranslateConfig> {
  const prevLang = config?.target_lang
  const prevMode = config?.mode
  const g = storeGen
  const next = await api<TranslateConfig>('PUT', '/api/me/translate', patch)
  if (g !== storeGen) return next
  config = next
  configLoaded = true
  // Clear cached errors (e.g. user just fixed API key or URL) and refetch if target language/mode changed.
  for (const [k, v] of cache) {
    if (v.status === 'error' || (prevLang && prevLang !== next.target_lang) || (prevMode && prevMode !== next.mode)) {
      cache.delete(k)
    }
  }
  notify()
  return next
}

export async function testTranslateConfig(draft?: { mode?: TranslateMode; target_lang?: string; base_url?: string; model?: string; api_key?: string }): Promise<TranslateTestResult> {
  return api<TranslateTestResult>('POST', '/api/me/translate/test', draft ?? {})
}

// Fast client-side script check mirroring internal/web/translate.go needsThoughtTranslation.
// Strips code blocks, inline code, URLs, and Unix paths so code-heavy reasoning
// is classified purely by its natural-language prose.
export function clientNeedsTranslation(raw: string, targetLang: string): boolean {
  let s = raw.trim()
  if (!s) return false
  s = s.replace(/```[\s\S]*?```/g, ' ')
  s = s.replace(/`[^`]*`/g, ' ')
  s = s.replace(/\[([^\]]*)\]\([^)]*\)/g, '$1')
  s = s.replace(/https?:\/\/\S+/g, ' ')
  s = s.replace(/(?:~|\.{1,2})?\/(?:[A-Za-z0-9._-]+\/)+[A-Za-z0-9._-]*/g, ' ')

  let latin = 0
  let han = 0
  let kana = 0
  let hangul = 0
  let other = 0
  for (const ch of s) {
    const cp = ch.codePointAt(0)!
    if ((cp >= 0x41 && cp <= 0x5a) || (cp >= 0x61 && cp <= 0x7a) || (cp >= 0xc0 && cp <= 0x24f)) {
      latin++
    } else if ((cp >= 0x4e00 && cp <= 0x9fff) || (cp >= 0x3400 && cp <= 0x4dbf) || (cp >= 0x20000 && cp <= 0x2a6df)) {
      han++
    } else if ((cp >= 0x3040 && cp <= 0x30ff) || (cp >= 0x31f0 && cp <= 0x31ff)) {
      kana++
    } else if ((cp >= 0xac00 && cp <= 0xd7af) || (cp >= 0x1100 && cp <= 0x11ff)) {
      hangul++
    } else if (cp > 0x7f && /\p{L}/u.test(ch)) {
      other++
    }
  }
  const total = latin + han + kana + hangul + other
  if (total < 8) return false

  switch (targetLang) {
    case 'zh-CN':
    case 'zh-TW':
      return han * 4 < total
    case 'ja':
      return (kana + han) * 4 < total || kana === 0
    case 'ko':
      return hangul * 4 < total
    case 'en':
      return latin * 4 < total * 3
    case 'fr':
    case 'de':
    case 'es':
      return true
    default:
      return true
  }
}

function clientNeedsProcessing(raw: string, targetLang: string, mode: TranslateMode): boolean {
  if (clientNeedsTranslation(raw, targetLang)) return true
  if (mode === 'summarize' && raw.trim().length >= 180) return true
  return false
}

function cacheKey(mode: TranslateMode, lang: string, text: string): string {
  return mode + '\0' + lang + '\0' + text
}

function setCache(key: string, entry: CacheEntry) {
  if (cache.size >= MAX_CACHE && !cache.has(key)) {
    const oldest = cache.keys().next().value
    if (oldest !== undefined) cache.delete(oldest)
  }
  cache.set(key, entry)
}

function enqueue(mode: TranslateMode, lang: string, text: string, priority: boolean) {
  const key = cacheKey(mode, lang, text)
  if (cache.has(key) || queuedKeys.has(key)) return
  setCache(key, { status: 'loading' })
  queuedKeys.add(key)
  if (priority) {
    queue.unshift({ key, mode, lang, text })
  } else {
    queue.push({ key, mode, lang, text })
  }
  notify()
  if (priority) {
    if (flushTimer) {
      clearTimeout(flushTimer)
      flushTimer = null
    }
    pump()
  } else if (!flushTimer) {
    flushTimer = setTimeout(() => {
      flushTimer = null
      pump()
    }, BATCH_DELAY_MS)
  }
}

function pump() {
  while (inFlight < MAX_INFLIGHT && queue.length > 0) {
    const firstMode = queue[0].mode
    const firstLang = queue[0].lang
    const batch: { key: string; mode: TranslateMode; lang: string; text: string }[] = []
    for (let i = 0; i < queue.length && batch.length < BATCH_MAX; ) {
      if (queue[i].mode === firstMode && queue[i].lang === firstLang) {
        batch.push(queue.splice(i, 1)[0])
      } else {
        i++
      }
    }
    if (batch.length === 0) break
    inFlight++
    void sendBatch(firstMode, firstLang, batch).finally(() => {
      inFlight--
      if (queue.length > 0) pump()
    })
  }
}

async function sendBatch(mode: TranslateMode, lang: string, batch: { key: string; mode: TranslateMode; lang: string; text: string }[]) {
  const g = storeGen
  try {
    const res = await api<{
      target_lang: string
      results: { hash: string; status: 'ok' | 'skipped' | 'error'; text?: string; cached?: boolean; error?: string }[]
    }>('POST', '/api/translate/thoughts', {
      mode,
      target_lang: lang,
      items: batch.map((b) => ({ text: b.text })),
    })
    if (g !== storeGen) return
    const results = res.results ?? []
    for (let i = 0; i < batch.length; i++) {
      const b = batch[i]
      queuedKeys.delete(b.key)
      const r = results[i]
      if (!r) {
        setCache(b.key, { status: 'error', error: 'missing result' })
      } else if (r.status === 'ok' && r.text) {
        setCache(b.key, { status: 'done', text: r.text })
      } else if (r.status === 'skipped') {
        setCache(b.key, { status: 'skipped' })
      } else {
        setCache(b.key, { status: 'error', error: r.error || 'translation failed' })
      }
    }
  } catch (err) {
    if (g !== storeGen) return
    if (err instanceof ApiError && err.code === 'translate_disabled') {
      if (config) config = { ...config, enabled: false }
      for (const b of batch) {
        queuedKeys.delete(b.key)
        cache.delete(b.key)
      }
    } else {
      const msg = err instanceof Error ? err.message : 'translation failed'
      for (const b of batch) {
        queuedKeys.delete(b.key)
        setCache(b.key, { status: 'error', error: msg })
      }
    }
  }
  notify()
}

export function useTranslateConfig(): { config: TranslateConfig | null; loaded: boolean; refresh: () => Promise<TranslateConfig | null> } {
  useSyncExternalStore(subscribe, () => storeVer)
  useEffect(() => {
    void ensureTranslateConfig()
  }, [])
  return { config, loaded: configLoaded, refresh: () => ensureTranslateConfig(true) }
}

export function useThoughtTranslation(text: string, opts: { active: boolean; priority?: boolean }): ThoughtTranslationState {
  useSyncExternalStore(subscribe, () => storeVer)
  const trimmed = text.trim()
  const enabled = !!config?.enabled
  const mode: TranslateMode = config?.mode === 'summarize' ? 'summarize' : 'translate'
  const lang = config?.target_lang || 'zh-CN'

  useEffect(() => {
    if (opts.active) void ensureTranslateConfig()
  }, [opts.active])

  const needs = enabled && !!trimmed && clientNeedsProcessing(trimmed, lang, mode)
  const key = needs ? cacheKey(mode, lang, trimmed) : ''
  const entry = key ? cache.get(key) : undefined

  useEffect(() => {
    if (!opts.active || !needs || entry) return
    enqueue(mode, lang, trimmed, !!opts.priority)
  }, [opts.active, needs, entry, mode, lang, trimmed, opts.priority])

  if (!enabled || !trimmed) return { enabled, mode, status: 'idle' }
  if (!needs) return { enabled, mode, status: 'skipped' }
  if (!entry) return { enabled, mode, status: opts.active ? 'loading' : 'idle' }
  return { enabled, mode, status: entry.status, text: entry.text, error: entry.error }
}
