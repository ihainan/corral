import type { ITheme } from '@xterm/xterm'

// Terminal palettes. The page chrome around the terminal (top bar, key bar)
// takes its colours from the same choice, see .term-root.dark/.light.

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
