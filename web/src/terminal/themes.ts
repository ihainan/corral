import type { ITheme } from '@xterm/xterm'

// Terminal palettes tuned to sit inside the app's light and dark surfaces.

export const lightTheme: ITheme = {
  background: '#fbfbfc',
  foreground: '#1f2329',
  cursor: '#2c5fcc',
  cursorAccent: '#fbfbfc',
  selectionBackground: '#c9d8f5',
  black: '#1f2329', red: '#c4302b', green: '#2f7d3b', yellow: '#946200',
  blue: '#2c5fcc', magenta: '#9b3fb0', cyan: '#127a87', white: '#a4abb6',
  brightBlack: '#6b7280', brightRed: '#e0443e', brightGreen: '#3c9a4a', brightYellow: '#b07a00',
  brightBlue: '#3f74e6', brightMagenta: '#b456c9', brightCyan: '#1593a3', brightWhite: '#d9dde3',
}

export const darkTheme: ITheme = {
  background: '#101317',
  foreground: '#d9dee6',
  cursor: '#8fb0f6',
  cursorAccent: '#101317',
  selectionBackground: '#33476e',
  black: '#2a3038', red: '#ef6b6b', green: '#7cc68a', yellow: '#e2b65c',
  blue: '#7fa4f4', magenta: '#cf8ee0', cyan: '#6fc4d0', white: '#c9ced6',
  brightBlack: '#5c6573', brightRed: '#ff8a85', brightGreen: '#97dba3', brightYellow: '#f2cb79',
  brightBlue: '#9dbbf8', brightMagenta: '#dfa7ec', brightCyan: '#8ad6e0', brightWhite: '#f1f3f6',
}
