import type { FitAddon } from '@xterm/addon-fit'
import type { Terminal } from '@xterm/xterm'

// The fit addon keeps 14px on the right for a scrollbar whenever there is
// scrollback. xterm's scrollbar floats over the text, and on a touch screen
// it only shows while scrolling: there the room goes to the text instead
// (two columns on a phone, seen as an empty strip on the right).
export function fitToEdge(fit: FitAddon, term: Terminal) {
  const propose = fit.proposeDimensions.bind(fit)
  fit.proposeDimensions = () => {
    const d = propose()
    if (!d || !matchMedia('(pointer: coarse)').matches) return d
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    const cell: number | undefined = (term as any)._core?._renderService?.dimensions?.css?.cell?.width
    const el = term.element
    if (!cell || !el?.parentElement) return d
    const own = getComputedStyle(el)
    const width = parseInt(getComputedStyle(el.parentElement).width) - parseInt(own.paddingLeft) - parseInt(own.paddingRight)
    return { cols: Math.max(2, Math.floor(Math.max(0, width) / cell)), rows: d.rows }
  }
}
