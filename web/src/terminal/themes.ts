import type { ITheme } from '@xterm/xterm'
import { schemes, type Scheme } from './schemes'

// Terminal palettes. Corral's own pair comes first; the rest are the bundled
// schemes. The page chrome around the terminal (top bar, key bar, composer)
// is derived from the chosen palette, see chromeVars.

export const lightTheme: ITheme = {
  background: '#ffffff',
  foreground: '#1f2329',
  cursor: '#2c5fcc',
  cursorAccent: '#ffffff',
  selectionBackground: '#c9d8f5',
  black: '#1f2329', red: '#c4302b', green: '#2f7d3b', yellow: '#946200',
  blue: '#2c5fcc', magenta: '#9b3fb0', cyan: '#127a87', white: '#a4abb6',
  brightBlack: '#6b7280', brightRed: '#e0443e', brightGreen: '#3c9a4a', brightYellow: '#b07a00',
  brightBlue: '#3f74e6', brightMagenta: '#b456c9', brightCyan: '#1593a3', brightWhite: '#c9ced6',
}

export const darkTheme: ITheme = {
  background: '#0f1115',
  foreground: '#d7dae0',
  cursor: '#e6e8ec',
  cursorAccent: '#0f1115',
  selectionBackground: '#4a78ec59',
  black: '#1c1f25', red: '#ef6f6c', green: '#7dc58f', yellow: '#e5b567',
  blue: '#7aa2f7', magenta: '#c99be6', cyan: '#6cc4cf', white: '#c5c9d1',
  brightBlack: '#5c6370', brightRed: '#ff8f8b', brightGreen: '#9adba8', brightYellow: '#f2cc85',
  brightBlue: '#9bbbfa', brightMagenta: '#dcb4f2', brightCyan: '#8fd8e1', brightWhite: '#f2f4f7',
}

export const corralDark: Scheme = { id: 'corral-dark', name: 'Corral Dark', dark: true, theme: darkTheme }
export const corralLight: Scheme = { id: 'corral-light', name: 'Corral Light', dark: false, theme: lightTheme }
export const allSchemes: Scheme[] = [corralDark, corralLight, ...schemes]
export type { Scheme }

// The stored preference: a scheme id, or "ui" to follow the page theme with
// Corral's own pair. "dark"/"light" are the values used before schemes.
export function resolveScheme(pref: string, uiDark: boolean): Scheme {
  if (pref === 'ui') return uiDark ? corralDark : corralLight
  if (pref === 'dark') return corralDark
  if (pref === 'light') return corralLight
  return allSchemes.find((s) => s.id === pref) ?? corralDark
}

function mix(a: string, b: string, t: number): string {
  const p = (h: string) => [1, 3, 5].map((i) => parseInt(h.slice(i, i + 2), 16))
  const [x, y] = [p(a), p(b)]
  return '#' + x.map((v, i) => Math.round(v + (y[i] - v) * t).toString(16).padStart(2, '0')).join('')
}

// CSS variables for the chrome, blended from the palette's background and
// foreground so any scheme gets a matching top bar and key bar. Corral's own
// pair keeps its hand-tuned values in terminal.css.
export function chromeVars(s: Scheme): Record<string, string> {
  if (s === corralDark || s === corralLight) return {}
  const bg = s.theme.background ?? '#000000'
  const fg = s.theme.foreground ?? '#ffffff'
  const accent = s.theme.blue ?? '#4a78ec'
  return {
    '--term-bg': bg,
    '--term-chrome': mix(bg, fg, s.dark ? 0.04 : 0.03),
    '--term-line': mix(bg, fg, s.dark ? 0.1 : 0.12),
    '--term-key': mix(bg, fg, s.dark ? 0.1 : 0),
    '--term-key-active': mix(bg, fg, s.dark ? 0.18 : 0.1),
    '--term-hover': mix(bg, fg, s.dark ? 0.08 : 0.06),
    '--term-fg': fg,
    '--term-fg-2': mix(fg, bg, 0.3),
    '--term-fg-3': mix(fg, bg, 0.5),
    '--term-accent-soft': mix(bg, accent, s.dark ? 0.22 : 0.14),
    '--term-accent-text': accent,
  }
}
