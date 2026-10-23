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

function luminance(h: string): number {
  const c = [1, 3, 5].map((i) => parseInt(h.slice(i, i + 2), 16) / 255).map((v) => (v <= 0.03928 ? v / 12.92 : ((v + 0.055) / 1.055) ** 2.4))
  return 0.2126 * c[0] + 0.7152 * c[1] + 0.0722 * c[2]
}

export function contrast(a: string, b: string): number {
  const [x, y] = [luminance(a), luminance(b)].sort((p, q) => q - p)
  return (x + 0.05) / (y + 0.05)
}

// Moves `fg` towards white or black until it reads against every background
// given at the ratio asked for (WCAG: 4.5 for text).
function readable(fg: string, bgs: string[], min: number, dark: boolean): string {
  const target = dark ? '#ffffff' : '#000000'
  for (let t = 0; t <= 1.0001; t += 0.05) {
    const c = mix(fg, target, t)
    if (bgs.every((bg) => contrast(c, bg) >= min)) return c
  }
  return target
}

// CSS variables for the chrome, blended from the palette's background and
// foreground so any scheme gets a matching top bar and key bar; text colours
// are then pushed until they reach 4.5:1 on every surface they sit on. The
// terminal itself keeps the scheme's colours untouched. Corral's own pair
// keeps its hand-tuned values in terminal.css.
export function chromeVars(s: Scheme): Record<string, string> {
  if (s === corralDark || s === corralLight) return {}
  const bg = s.theme.background ?? '#000000'
  const fg = s.theme.foreground ?? '#ffffff'
  const d = s.dark
  const chrome = mix(bg, fg, d ? 0.04 : 0.03)
  const key = mix(bg, fg, d ? 0.1 : 0)
  const keyActive = mix(bg, fg, d ? 0.18 : 0.1)
  const hover = mix(bg, fg, d ? 0.08 : 0.06)
  const accentSoft = mix(bg, s.theme.blue ?? '#4a78ec', d ? 0.22 : 0.14)
  // Every surface chrome text sits on, including the highlighted row
  // (current herdr workspace, latched modifier keys).
  const surfaces = [bg, chrome, key, hover, accentSoft]
  const code = [bg, chrome] // (a code block's, a step's line)
  return {
    '--term-bg': bg,
    '--term-chrome': chrome,
    '--term-line': mix(bg, fg, d ? 0.1 : 0.12),
    '--term-key': key,
    '--term-key-active': keyActive,
    '--term-hover': hover,
    '--term-fg': readable(fg, [...surfaces, keyActive], 4.5, d),
    '--term-fg-2': readable(mix(fg, bg, 0.25), surfaces, 4.5, d),
    '--term-fg-3': readable(mix(fg, bg, 0.45), surfaces, 4.5, d),
    '--term-accent-soft': accentSoft,
    '--term-accent-text': readable(s.theme.blue ?? '#4a78ec', surfaces, 4.5, d),
    '--term-danger-text': readable(s.theme.red ?? '#d0453e', surfaces, 4.5, d),
    '--term-warn-text': readable(s.theme.yellow ?? '#e0a24a', surfaces, 4.5, d),
    '--term-ok-text': readable(s.theme.green ?? '#3fa45b', surfaces, 4.5, d),
    // Code coloured by its language, on the code's own surface (the chrome colour).
    '--term-syn-keyword': readable(s.theme.magenta ?? '#c678dd', code, 4.5, d),
    '--term-syn-string': readable(s.theme.green ?? '#3fa45b', code, 4.5, d),
    '--term-syn-number': readable(s.theme.cyan ?? '#2aa1b3', code, 4.5, d),
    '--term-syn-title': readable(s.theme.blue ?? '#4a78ec', code, 4.5, d),
    '--term-syn-type': readable(s.theme.yellow ?? '#e0a24a', code, 4.5, d),
    '--term-syn-attr': readable(s.theme.brightCyan ?? s.theme.cyan ?? '#2aa1b3', code, 4.5, d),
    '--term-syn-comment': readable(s.theme.brightBlack ?? mix(fg, bg, 0.45), code, 4.5, d),
  }
}
