import type { SpecialKey } from './keys'

// The key bar's layout, the user's: keys pinned on the left, and the row
// that scrolls; the keyboard toggle stays on the right. A key is one of the
// built-in ones (by id) or the user's own (a label and a sequence, see
// macro.ts); any of them but the repeating and modifier ones may have a
// sequence for a long press (long: "" is none, absent is the key's default).
// Kept with the other settings (prefs.keyLayout), checked by the gateway
// too (internal/web/api_auth.go keyLayout: keep the ids in step).

export type LayoutKey = { k: BuiltinId; long?: string } | { label: string; keys: string; long?: string }
export interface KeyLayout {
  pinned: LayoutKey[]
  row: LayoutKey[]
}

export const maxPinned = 6
export const maxRow = 48
export const maxCustom = 24
export const maxLabel = 12
// The gateway's limit for a stored layout, in bytes (it escapes < > & and
// U+2028, U+2029 as \u003c…).
export const maxLayoutBytes = 48 << 10
export const layoutBytes = (l: KeyLayout) => new TextEncoder().encode(JSON.stringify(l).replace(/[<>&\u2028\u2029]/g, '\\u0000')).length

type Builtin =
  | { label: string; aria: string; key: SpecialKey; repeat?: boolean; long?: string }
  | { label: string; aria: string; text: string; long?: string }
  | { label: string; aria: string; mod: 'ctrl' | 'alt' }
  | { label: string; aria: string; act: 'fn' | 'panel' }

// (Labels that are words are set by the key bar, in the user's language.)
export const builtins = {
  esc: { label: 'Esc', aria: 'Escape', key: 'Esc', long: 'Esc Esc' },
  tab: { label: 'Tab', aria: 'Tab', key: 'Tab', long: 'BTab' },
  ctrl: { label: 'Ctrl', aria: 'Ctrl', mod: 'ctrl' },
  alt: { label: 'Alt', aria: 'Alt', mod: 'alt' },
  fn: { label: 'Fn', aria: 'Fn', act: 'fn' },
  panel: { label: '', aria: '', act: 'panel' },
  up: { label: '↑', aria: 'Up', key: 'Up', repeat: true },
  down: { label: '↓', aria: 'Down', key: 'Down', repeat: true },
  left: { label: '←', aria: 'Left', key: 'Left', repeat: true },
  right: { label: '→', aria: 'Right', key: 'Right', repeat: true },
  newline: { label: '', aria: '', key: 'AgentNewline' },
  enter: { label: 'Enter', aria: 'Enter', key: 'Enter' },
  home: { label: 'Home', aria: 'Home', key: 'Home' },
  end: { label: 'End', aria: 'End', key: 'End' },
  pgup: { label: 'PgUp', aria: 'Page Up', key: 'PageUp', repeat: true },
  pgdn: { label: 'PgDn', aria: 'Page Down', key: 'PageDown', repeat: true },
  del: { label: 'Del', aria: 'Delete', key: 'Delete' },
  'sym:|': { label: '|', aria: '|', text: '|' },
  'sym:/': { label: '/', aria: '/', text: '/' },
  'sym:~': { label: '~', aria: '~', text: '~' },
  'sym:-': { label: '-', aria: '-', text: '-' },
  'sym:`': { label: '`', aria: '`', text: '`' },
  'sym:\\': { label: '\\', aria: '\\', text: '\\' },
  'sym:_': { label: '_', aria: '_', text: '_' },
  'sym::': { label: ':', aria: ':', text: ':' },
  'sym:*': { label: '*', aria: '*', text: '*' },
  'sym:&': { label: '&', aria: '&', text: '&' },
  'sym:$': { label: '$', aria: '$', text: '$' },
} satisfies Record<string, Builtin>
export type BuiltinId = keyof typeof builtins
export const builtinIds = Object.keys(builtins) as BuiltinId[]

export const isBuiltin = (k: LayoutKey): k is { k: BuiltinId; long?: string } => 'k' in k
// Whether a key may have a long press: not one that repeats while held, not
// a modifier (Ctrl's opens the shortcut panel), not Fn or the panel key.
export function mayLong(k: LayoutKey): boolean {
  if (!isBuiltin(k)) return true
  const b: Builtin = builtins[k.k]
  return !('mod' in b) && !('act' in b) && !('repeat' in b && b.repeat)
}
// The sequence a long press sends, or '' for none.
export function longOf(k: LayoutKey): string {
  if (!mayLong(k)) return ''
  if (k.long !== undefined) return k.long
  return isBuiltin(k) ? ((builtins[k.k] as { long?: string }).long ?? '') : ''
}

const symbols: BuiltinId[] = ['sym:|', 'sym:/', 'sym:~', 'sym:-', 'sym:`', 'sym:\\', 'sym:_', 'sym::', 'sym:*', 'sym:&', 'sym:$']

// The layout before the user changed it: as the key bar always was, with
// the user's own keys (from before layouts) after Fn.
export function defaultLayout(custom: { label: string; keys: string }[] = []): KeyLayout {
  return {
    pinned: [{ k: 'esc' }, { k: 'tab' }, { k: 'ctrl' }, { k: 'alt' }],
    row: [
      { k: 'fn' },
      ...custom.map((c) => ({ label: c.label, keys: c.keys })),
      { k: 'up' }, { k: 'down' }, { k: 'left' }, { k: 'right' },
      { k: 'newline' },
      ...symbols.map((k) => ({ k })),
    ],
  }
}

const controlChars = /[\x00-\x1f\x7f-\x9f]/
const text = (v: unknown, max: number, empty = false): v is string =>
  typeof v === 'string' && [...v].length <= max && !controlChars.test(v) && (empty ? true : v.trim() !== '')

// A layout as stored, or null when it is not one (then the default applies).
export function sanitizeLayout(v: unknown): KeyLayout | null {
  if (!v || typeof v !== 'object') return null
  const o = v as Record<string, unknown>
  if (Object.keys(o).length !== 2 || !Array.isArray(o.pinned) || !Array.isArray(o.row) || o.pinned.length > maxPinned || o.row.length > maxRow) return null
  const seen = new Set<string>()
  let custom = 0
  const key = (x: unknown): LayoutKey | null => {
    if (!x || typeof x !== 'object') return null
    const e = x as Record<string, unknown>
    if (Object.keys(e).some((f) => f !== 'k' && f !== 'label' && f !== 'keys' && f !== 'long')) return null
    if ('long' in e && !text(e.long, 200, true)) return null
    const long = 'long' in e ? { long: e.long as string } : {}
    if ('k' in e) {
      if (typeof e.k !== 'string') return null
      if (!Object.hasOwn(builtins, e.k) || seen.has(e.k) || 'label' in e || 'keys' in e) return null
      seen.add(e.k)
      return { k: e.k as BuiltinId, ...long }
    }
    if (!text(e.label, maxLabel) || !text(e.keys, 200) || ++custom > maxCustom) return null
    return { label: e.label, keys: e.keys, ...long }
  }
  const pinned = o.pinned.map(key)
  const row = o.row.map(key)
  if (pinned.includes(null) || row.includes(null)) return null
  return { pinned: pinned as LayoutKey[], row: row as LayoutKey[] }
}

// The layout in use: the user's, or the default (with their keys from before).
export const layoutIn = (layout: KeyLayout | null, custom: { label: string; keys: string }[]): KeyLayout => layout ?? defaultLayout(custom)
