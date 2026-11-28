// The keys the agents page takes for itself, as plain tests of a key
// event (plans/4a-shortcuts.md). None that browsers, systems or Chinese
// input methods commonly use: not ⌘F/Ctrl+F (find in page), not Ctrl+O off
// a Mac (open a file); a few are kept that a browser has a little-used
// meaning for and the web commonly takes (/, ⌥↑↓, ⌘K: see the help).

import { local } from '../storage'
import { railDigit } from './railKeys'

export interface KeyLike {
  key: string
  ctrlKey: boolean
  metaKey: boolean
  altKey: boolean
  shiftKey: boolean
  isComposing: boolean
  keyCode: number
}

export const onMac = () => /Mac|iPhone|iPad/.test(navigator.platform)

// A terminal focused and it first (the setting "keysFirst"): Corral's own
// shortcuts that the terminal turns into input for its program — ⌥↑↓, as
// escape sequences — let it have them. (Those of the window — ⌘ ones, the
// tabs' Ctrl+Tab and Ctrl+Shift ones, Ctrl+, — are taken either way: in a
// terminal they would only scroll it, or send nothing — review of W5.)
export const terminalHasKeys = (e: Event, first: 'terminal' | 'corral' | undefined) =>
  first !== 'corral' && e.target instanceof Element && !!e.target.closest('.xterm')
// Corral first, one of them taken from a focused terminal: said once, how to give it back.
export function takenFromTerminal(e: Event, key: string, say: (m: string) => void, words: (key: string) => string) {
  if (!(e.target instanceof Element) || !e.target.closest('.xterm')) return
  const k = 'corral.keyTaken.' + key
  if (local.get(k)) return
  local.set(k, '1')
  say(words(key))
}

// An input method at work: some browsers say so only by keyCode 229 on the
// first and last keys of a word.
export const composing = (e: KeyLike) => e.isComposing || e.keyCode === 229

// (Ctrl alone or ⌘ held: a shortcut, not a character. Ctrl with Alt is how
// Windows reports AltGr, which types "[" on German or French layouts; a
// Mac's Option does on its own.)
const shortcutHeld = (e: KeyLike) => e.metaKey || (e.ctrlKey && !e.altKey)

// "[" or "]": to my message before or after (-1, 1); 0 for any other key.
export function jumpKey(e: KeyLike): -1 | 0 | 1 {
  if (composing(e) || shortcutHeld(e)) return 0
  return e.key === '[' ? -1 : e.key === ']' ? 1 : 0
}

// "?": the help (Shift as it takes; not ⌘⇧/, the Mac's own Help menu).
export const helpKey = (e: KeyLike) => !composing(e) && !shortcutHeld(e) && e.key === '?'

// Ctrl+O, all of a conversation's steps opened or folded: as Claude Code
// has it, on a Mac only (Ctrl, not ⌘; elsewhere it is the browser's Open).
export const foldAllKey = (e: KeyLike, mac: boolean) => mac && !composing(e) && e.ctrlKey && !e.metaKey && !e.altKey && !e.shiftKey && e.key.toLowerCase() === 'o'

// Alt+Up/Down (⌥ on a Mac): to the agent before or after on the list.
export function switchKey(e: KeyLike): -1 | 0 | 1 {
  if (composing(e) || !e.altKey || e.ctrlKey || e.metaKey || e.shiftKey) return 0
  return e.key === 'ArrowUp' ? -1 : e.key === 'ArrowDown' ? 1 : 0
}

// Alt+1…9 (⌥ on a Mac): the rail's n-th page, as its icons go down (doc UI
// item 15, 11-25); 0 for any other key. By the key's place, not what it types
// (a Mac's ⌥1 types "¡"); Ctrl with Alt is AltGr (see shortcutHeld).
export function railKey(e: KeyLike & { code: string }): number {
  return railDigit(e)
}
export const RAIL_KEYS = onMac() ? '⌥1–9' : 'Alt+1–9'

// Ctrl+` (the key left of 1; a Mac's Control too, as VS Code's terminal): an agent's conversation and its terminal,
// the one for the other in its place (UI 14, 11-25). Taken from its terminal too — the way back from it. (What it
// would send there, NUL, Ctrl+Space sends as well.) By the key's place, not its letter: other layouts put ` elsewhere.
export const termViewKey = (e: KeyLike & { code: string }) => !composing(e) && e.ctrlKey && !e.altKey && !e.metaKey && !e.shiftKey && e.code === 'Backquote'
export const TERM_VIEW_KEY = 'Ctrl+`'
export const railKeyName = (n: number) => (onMac() ? '⌥' : 'Alt+') + n

// ⌘P on a Mac, Ctrl+P elsewhere: to another agent, by its name — the list over
// the conversation, or the search of the one beside it. As VS Code's quick open;
// taken while typing too, which "/" could not be (the person, 11-23: "/" is the
// composer's own). The browser's is Print, which a page may take and the web does.
export const pickKey = (e: KeyLike, mac: boolean) =>
  !composing(e) && !e.altKey && !e.shiftKey && e.key.toLowerCase() === 'p' && (mac ? e.metaKey && !e.ctrlKey : e.ctrlKey && !e.metaKey)

// ⌘B on a Mac, Ctrl+B elsewhere: the list beside folded or opened, as VS Code
// has it. (Firefox's own is its bookmarks sidebar, little used: as "/".)
export const sideKey = (e: KeyLike, mac: boolean) =>
  !composing(e) && !e.altKey && !e.shiftKey && e.key.toLowerCase() === 'b' && (mac ? e.metaKey && !e.ctrlKey : e.ctrlKey && !e.metaKey)
