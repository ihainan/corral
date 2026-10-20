import { ctrlChar, specialKey, noMods, type KeysApart, type Mods, type SpecialKey } from './keys'

// A custom key sends a sequence written the way tmux's send-keys takes it:
// steps separated by spaces, each a key ("Esc", "Enter", "Up", "F5"…), a key
// or character with modifiers ("C-b" Ctrl+b, "M-x" Alt+x, "S-Tab",
// "C-M-Left"), or text, which is typed as it is ("%", "ls"). As in a shell,
// quotes and a backslash make text of what they cover ("git status",
// 'Enter', foo\ bar), and quoted and unquoted parts written together are one
// step. Steps follow each other directly: "ls -la" is typed "ls-la", as in
// tmux. Shift+Enter is what the terminal sends for it (a newline that does
// not submit, for coding agents).
//
// Moshi's way is read too: a comma ending a word separates steps ("C-b, S-t"),
// "text:" makes text of the rest up to such a comma ("text:git status,
// enter"), and "dash", "plus" and "comma" name those characters.

// chord: the key as written with its modifiers, for a text step that came
// from one (C-b is the text \x02); see describeMacro.
export type Step = { key: SpecialKey; mods: Mods } | { text: string; chord?: string }

export const maxMacro = 200 // characters of a sequence
export const maxSteps = 32

const named = new Map<string, SpecialKey>(
  Object.entries({
    esc: 'Esc', escape: 'Esc', enter: 'Enter', return: 'Enter', tab: 'Tab', btab: 'ShiftTab',
    bspace: 'Backspace', backspace: 'Backspace', bs: 'Backspace', up: 'Up', down: 'Down', left: 'Left', right: 'Right',
    home: 'Home', end: 'End', pgup: 'PageUp', pageup: 'PageUp', pgdn: 'PageDown', pagedown: 'PageDown',
    ins: 'Insert', insert: 'Insert', del: 'Delete', delete: 'Delete',
  } as Record<string, SpecialKey>),
)
for (let i = 1; i <= 12; i++) named.set('f' + i, ('F' + i) as SpecialKey)

const charNames = new Map(Object.entries({ space: ' ', dash: '-', plus: '+', comma: ',' }))

// Control characters, C0, DEL and C1: never part of a sequence or a label.
export const controlChars = /[\x00-\x1f\x7f-\x9f]/

export type MacroError = 'macro.tooLong' | 'macro.control' | 'macro.quote' | 'macro.unknown' | 'macro.tooMany' | 'macro.empty'

// A word of the sequence: its text, and whether any of it was quoted or
// escaped (then it is text, whatever it says).
type Word = { text: string; literal: boolean }

function words(spec: string): Word[] | null {
  const out: Word[] = []
  let cur: Word | null = null
  let textWord = false // after "text:": spaces are part of it, up to a separating comma
  const end = () => {
    if (cur && (cur.text || cur.literal)) out.push(cur)
    cur = null
    textWord = false
  }
  for (let i = 0; i < spec.length; i++) {
    const c = spec[i]
    // A comma that ends a word (before a space, or last) separates steps
    // after a key or a chord, or text: ("hi, there" stays text, as before;
    // so does M-, which is Alt+comma).
    if (c === ',' && cur && (i + 1 === spec.length || spec[i + 1] === ' ' || spec[i + 1] === '\t') && (textWord || (!cur.literal && isKeyWord(cur.text)))) {
      end()
      continue
    }
    if (textWord) {
      if (cur!.text || (c !== ' ' && c !== '\t')) cur!.text += c // (as it is: quotes and backslashes too)
      continue
    }
    if ((c === ' ' || c === '\t') && !textWord) {
      end()
      continue
    }
    if (!cur && spec.startsWith('text:', i)) {
      cur = { text: '', literal: true }
      textWord = true
      i += 4
      continue
    }
    cur ??= { text: '', literal: false }
    if (c === '"' || c === "'") {
      const end = c === "'" ? spec.indexOf("'", i + 1) : closingDouble(spec, i + 1)
      if (end < 0) return null
      const body = spec.slice(i + 1, end)
      cur.text += c === '"' ? body.replace(/\\(["\\])/g, '$1') : body
      cur.literal = true
      i = end
    } else if (c === '\\' && i + 1 < spec.length) {
      cur.text += spec[++i]
      cur.literal = true
    } else cur.text += c
  }
  if (textWord && cur) cur.text = cur.text.trimEnd()
  end()
  return out
}

// A word that names a key or a chord (C-b, Enter, S-Tab), not text.
function isKeyWord(w: string): boolean {
  const m = /^((?:[CMS]-)*)(.+)$/s.exec(w)
  if (!m) return false
  return !!m[1] || named.has(m[2].toLowerCase()) || charNames.has(m[2].toLowerCase())
}

function closingDouble(s: string, from: number): number {
  for (let i = from; i < s.length; i++) {
    if (s[i] === '\\') i++
    else if (s[i] === '"') return i
  }
  return -1
}

// parseMacro reads a sequence, or says what is wrong with it (an i18n key
// and the step at fault).
export function parseMacro(spec: string): { steps: Step[] } | { error: MacroError; at: string } {
  if ([...spec].length > maxMacro) return { error: 'macro.tooLong', at: '' }
  if (controlChars.test(spec)) return { error: 'macro.control', at: '' }
  const ws = words(spec)
  if (!ws) return { error: 'macro.quote', at: '' }
  const steps: Step[] = []
  for (const w of ws) {
    if (w.literal) {
      if (w.text) steps.push({ text: w.text })
    } else {
      const step = parseToken(w.text)
      if (!step) return { error: 'macro.unknown', at: w.text }
      steps.push(step)
    }
    if (steps.length > maxSteps) return { error: 'macro.tooMany', at: '' }
  }
  if (steps.length === 0) return { error: 'macro.empty', at: '' }
  return { steps }
}

function parseToken(tok: string): Step | null {
  const mods = { ...noMods }
  let rest = tok
  // Modifier prefixes, as long as something follows them.
  for (;;) {
    const p = /^([CMS])-(.+)$/s.exec(rest)
    if (!p) break
    if (p[1] === 'C') mods.ctrl = true
    else if (p[1] === 'M') mods.alt = true
    else mods.shift = true
    rest = p[2]
  }
  const key = named.get(rest.toLowerCase())
  if (key) {
    // Shift+Enter is the newline coding agents take (as the terminal sends it).
    if (key === 'Enter' && mods.shift && !mods.ctrl && !mods.alt) return { key: 'AgentNewline', mods: { ...noMods } }
    return { key, mods }
  }
  const char = charNames.get(rest.toLowerCase())
  if (char) rest = char
  if (!mods.ctrl && !mods.alt && !mods.shift) return { text: rest }
  // A modifier applies to one character. Ctrl and Shift only to the ASCII
  // ones they have a meaning for; Alt (ESC before it) to any.
  if ([...rest].length !== 1) return null
  let ch = rest
  if (mods.shift) {
    if (!/^[a-zA-Z]$/.test(ch)) return null
    ch = ch.toUpperCase()
  }
  if (mods.ctrl) {
    if (!/^[\x20-\x7e]$/.test(ch)) return null
    const c = ctrlChar(ch)
    if (c === null) return null
    ch = c
  }
  return { text: (mods.alt ? '\x1b' : '') + ch, chord: chordName(mods, rest === ' ' ? 'Space' : rest.length === 1 && /[a-z]/i.test(rest) ? rest.toUpperCase() : rest) }
}

const keyNames: Partial<Record<SpecialKey, string>> = {
  Up: '↑', Down: '↓', Left: '←', Right: '→', PageUp: 'PgUp', PageDown: 'PgDn', Delete: 'Del', Insert: 'Ins', Backspace: '⌫', ShiftTab: 'Shift+Tab', AgentNewline: 'Shift+Enter',
}
function chordName(m: Mods, key: string): string {
  return (m.ctrl ? 'Ctrl+' : '') + (m.alt ? 'Alt+' : '') + (m.shift ? 'Shift+' : '') + key
}

// What a sequence sends, in words: "Ctrl+B, then T" (then is given).
export function describeMacro(steps: Step[], then: string, text: (s: string) => string): string {
  return steps
    .map((s) => ('key' in s ? chordName(s.mods, keyNames[s.key] ?? s.key) : s.chord ?? text(s.text)))
    .join(then)
}

// macroBytes is what a parsed sequence sends; appCursor and keysApart as for specialKey.
export function macroBytes(steps: Step[], appCursor: boolean, keysApart: KeysApart = null): string {
  return steps.map((s) => ('key' in s ? specialKey(s.key, s.mods, appCursor, keysApart) : s.text)).join('')
}
