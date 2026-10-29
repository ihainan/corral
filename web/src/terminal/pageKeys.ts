// The keys the agents page takes for itself, as plain tests of a key
// event (plans/4a-shortcuts.md). None that browsers, systems or Chinese
// input methods commonly use: not ⌘F/Ctrl+F (find in page), not Ctrl+O off
// a Mac (open a file); a few are kept that a browser has a little-used
// meaning for and the web commonly takes (/, ⌥↑↓, ⌘K: see the help).

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
