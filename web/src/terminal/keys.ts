// Byte sequences for keys the on-screen keyboard lacks. Follows xterm's
// conventions (as implemented in xterm.js Keyboard.ts) so programs see exactly
// what a desktop terminal would send.

export interface Mods {
  ctrl: boolean
  alt: boolean
  shift: boolean
}

export const noMods: Mods = { ctrl: false, alt: false, shift: false }

// xterm modifier parameter: 1 + shift + 2·alt + 4·ctrl
function modParam(m: Mods): number {
  return 1 + (m.shift ? 1 : 0) + (m.alt ? 2 : 0) + (m.ctrl ? 4 : 0)
}

function hasMods(m: Mods) {
  return m.ctrl || m.alt || m.shift
}

export type SpecialKey =
  | 'Esc' | 'Tab' | 'ShiftTab' | 'Enter' | 'Backspace'
  | 'Up' | 'Down' | 'Left' | 'Right'
  | 'Home' | 'End' | 'PageUp' | 'PageDown' | 'Insert' | 'Delete'
  | 'F1' | 'F2' | 'F3' | 'F4' | 'F5' | 'F6' | 'F7' | 'F8' | 'F9' | 'F10' | 'F11' | 'F12'
  | 'AgentNewline'

const tilde: Partial<Record<SpecialKey, number>> = {
  Insert: 2, Delete: 3, PageUp: 5, PageDown: 6,
  F5: 15, F6: 17, F7: 18, F8: 19, F9: 20, F10: 21, F11: 23, F12: 24,
}

// appCursor: the program enabled application cursor keys (DECCKM), e.g. vim, less.
export function specialKey(key: SpecialKey, m: Mods, appCursor: boolean): string {
  const p = modParam(m)
  const alt = m.alt ? '\x1b' : ''
  switch (key) {
    case 'Esc': return alt + '\x1b'
    case 'Tab': return m.shift ? '\x1b[Z' : alt + '\t'
    case 'ShiftTab': return '\x1b[Z'
    case 'Enter': return alt + '\r'
    case 'Backspace': return alt + (m.ctrl ? '\x08' : '\x7f')
    // Esc+CR is what Claude Code's /terminal-setup binds Shift+Enter to; Codex
    // treats it (Alt+Enter) as a newline too.
    case 'AgentNewline': return '\x1b\r'
    case 'Up': case 'Down': case 'Right': case 'Left': {
      const c = { Up: 'A', Down: 'B', Right: 'C', Left: 'D' }[key]
      if (hasMods(m)) return `\x1b[1;${p}${c}`
      return (appCursor ? '\x1bO' : '\x1b[') + c
    }
    case 'Home': case 'End': {
      const c = key === 'Home' ? 'H' : 'F'
      if (hasMods(m)) return `\x1b[1;${p}${c}`
      return (appCursor ? '\x1bO' : '\x1b[') + c
    }
    case 'F1': case 'F2': case 'F3': case 'F4': {
      const c = { F1: 'P', F2: 'Q', F3: 'R', F4: 'S' }[key]
      return hasMods(m) ? `\x1b[1;${p}${c}` : '\x1bO' + c
    }
    default: {
      const n = tilde[key]
      if (n === undefined) return ''
      return hasMods(m) ? `\x1b[${n};${p}~` : `\x1b[${n}~`
    }
  }
}

// Apply latched Ctrl/Alt from the key bar to text typed on the soft keyboard.
export function applyMods(data: string, m: Mods): string {
  if (!m.ctrl && !m.alt) return data
  // Only single typed characters combine with modifiers; pastes pass through.
  if ([...data].length !== 1) return data
  let out = data
  if (m.ctrl) out = ctrlChar(data) ?? data
  if (m.alt) out = '\x1b' + out
  return out
}

export function ctrlChar(ch: string): string | null {
  const c = ch.toLowerCase()
  if (c >= 'a' && c <= 'z') return String.fromCharCode(c.charCodeAt(0) - 96)
  switch (ch) {
    case '@': case ' ': case '2': return '\x00'
    case '[': case '3': return '\x1b'
    case '\\': case '4': return '\x1c'
    case ']': case '5': return '\x1d'
    case '^': case '6': return '\x1e'
    case '_': case '-': case '7': return '\x1f'
    case '?': case '8': return '\x7f'
  }
  return null
}
