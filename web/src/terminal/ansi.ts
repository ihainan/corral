// A screen as the host read it, with its colours, as runs of text and style:
// the colour sequences (SGR) kept, every other sequence dropped. Nothing of it
// becomes markup: each run is text with a style of a known form.

export interface AnsiRun {
  text: string
  color?: string
  background?: string
  bold?: boolean
  dim?: boolean
  italic?: boolean
  underline?: boolean
  inverse?: boolean
}

const BASE = ['#1b1e24', '#e5666d', '#6fc27a', '#d9b45b', '#6ea8ef', '#c586d8', '#56bcc4', '#c9ced6']
const BRIGHT = ['#767e8c', '#f08a90', '#8fd79a', '#ecca7a', '#92bff5', '#d8a4e6', '#7fd3da', '#f2f4f7']

function color256(n: number): string | undefined {
  if (!Number.isInteger(n) || n < 0 || n > 255) return undefined
  if (n < 8) return BASE[n]
  if (n < 16) return BRIGHT[n - 8]
  if (n < 232) {
    const c = n - 16
    const v = (x: number) => (x === 0 ? 0 : 55 + x * 40)
    return `rgb(${v(Math.floor(c / 36))},${v(Math.floor(c / 6) % 6)},${v(c % 6)})`
  }
  const g = 8 + (n - 232) * 10
  return `rgb(${g},${g},${g})`
}

function extended(codes: number[], i: number): { color?: string; used: number } {
  if (codes[i + 1] === 5) return { color: color256(codes[i + 2]), used: 2 }
  if (codes[i + 1] === 2) {
    const [r, g, b] = codes.slice(i + 2, i + 5)
    const ok = [r, g, b].every((x) => Number.isInteger(x) && x >= 0 && x <= 255)
    return { color: ok ? `rgb(${r},${g},${b})` : undefined, used: 4 }
  }
  return { used: 0 }
}

type Style = Omit<AnsiRun, 'text'>

function apply(style: Style, codes: number[]): Style {
  let s = { ...style }
  for (let i = 0; i < codes.length; i++) {
    const c = codes[i]
    if (c === 0) s = {}
    else if (c === 1) s.bold = true
    else if (c === 2) s.dim = true
    else if (c === 3) s.italic = true
    else if (c === 4) s.underline = true
    else if (c === 7) s.inverse = true
    else if (c === 22) s.bold = s.dim = undefined
    else if (c === 23) s.italic = undefined
    else if (c === 24) s.underline = undefined
    else if (c === 27) s.inverse = undefined
    else if (c >= 30 && c <= 37) s.color = BASE[c - 30]
    else if (c === 39) s.color = undefined
    else if (c >= 40 && c <= 47) s.background = BASE[c - 40]
    else if (c === 49) s.background = undefined
    else if (c >= 90 && c <= 97) s.color = BRIGHT[c - 90]
    else if (c >= 100 && c <= 107) s.background = BRIGHT[c - 100]
    else if (c === 38 || c === 48) {
      const e = extended(codes, i)
      if (c === 38) s.color = e.color
      else s.background = e.color
      i += e.used
    }
  }
  return s
}

// The sequences a screen read holds: CSI (m: colours, kept), OSC, and the two-byte kind.
// eslint-disable-next-line no-control-regex
const SEQ = /\x1b\[([0-9;:?]*)([\x20-\x2f]*)([\x40-\x7e])|\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)?|\x1b[\x20-\x2f]*[\x30-\x7e]?/g

export function ansiLines(screen: string): AnsiRun[][] {
  let style: Style = {}
  return screen.split('\n').map((line) => {
    const runs: AnsiRun[] = []
    const put = (text: string) => {
      // eslint-disable-next-line no-control-regex
      const t = text.replace(/[\x00-\x08\x0b-\x1f\x7f]/g, '')
      if (t) runs.push({ text: t, ...style })
    }
    let at = 0
    SEQ.lastIndex = 0
    for (let m = SEQ.exec(line); m; m = SEQ.exec(line)) {
      put(line.slice(at, m.index))
      at = m.index + m[0].length
      if (m[3] === 'm' && !m[2]) style = apply(style, (m[1] || '0').split(/[;:]/).map(Number))
    }
    put(line.slice(at))
    return runs
  })
}
