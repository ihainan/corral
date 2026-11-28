// Pure key-handling logic for the workbench rail's Alt/Option + 1..9 shortcuts.
// Kept free of DOM/browser globals so unit tests in web/scripts/ can run it directly.

export interface RailKeyEvent {
  key: string
  code: string
  altKey: boolean
  ctrlKey: boolean
  metaKey: boolean
  shiftKey: boolean
  isComposing: boolean
  keyCode: number
  repeat?: boolean
  defaultPrevented?: boolean
}

export interface RailKeyContext {
  railActive: boolean
  modalOpen: boolean
  mac: boolean
  inTextField: boolean
  inTerminal: boolean
  keysFirst: 'terminal' | 'corral' | undefined
}

export interface RailHoldState {
  pending: boolean
  shown: boolean
}

export type RailHoldAction = 'none' | 'start-timer' | 'cancel' | 'hide'

export type RailKeyDecision =
  | { action: 'ignore' }
  | { action: 'consume-current'; index: number; hideHints: boolean }
  | { action: 'navigate'; index: number; notifyTakenFromTerminal: boolean; hideHints: boolean }

// How long Alt/Option is held alone before the rail shows its number hints.
export const RAIL_HOLD_MS = 450

// An input method at work: some browsers report this only via keyCode 229.
export const isComposingKey = (e: Pick<RailKeyEvent, 'isComposing' | 'keyCode'>): boolean =>
  e.isComposing || e.keyCode === 229

// Alt+1..9 (Option on macOS): by physical key code, not e.key (on macOS Option+1
// types "¡"). Ctrl+Alt is AltGr on Windows/Linux and is excluded.
export function railDigit(e: Pick<RailKeyEvent, 'code' | 'altKey' | 'ctrlKey' | 'metaKey' | 'shiftKey' | 'isComposing' | 'keyCode'>): number {
  if (isComposingKey(e) || !e.altKey || e.ctrlKey || e.metaKey || e.shiftKey) return 0
  const m = /^Digit([1-9])$/.exec(e.code)
  return m ? Number(m[1]) : 0
}

// State-machine transition for showing/hiding the rail's number hints when Alt/Option is held.
//
// Consistency rule with terminals:
// - Holding Alt/Option alone for >= RAIL_HOLD_MS always shows the hints (unless a modal is open,
//   the rail is inactive/hidden, or on macOS focus is inside a regular text input where Option
//   types characters like • or ™).
// - Once hints are shown, pressing 1..9 ALWAYS activates the rail shortcut, even when focus is in
//   a terminal with keysFirst === 'terminal'.
// - Pressing Alt+digit (or any other Alt chord) quickly (< RAIL_HOLD_MS) cancels the timer so no
//   hints appear, and in a terminal with keysFirst === 'terminal' the chord passes straight through
//   to the terminal program (herdr, tmux, readline, or macOS special characters).
export function railHoldTransition(
  kind: 'keydown' | 'keyup' | 'blur' | 'hide-rail' | 'focus-text',
  e: Partial<RailKeyEvent> | null,
  state: RailHoldState,
  ctx: RailKeyContext,
): RailHoldAction {
  if (kind === 'blur' || kind === 'hide-rail') {
    return state.pending || state.shown ? 'hide' : 'none'
  }
  if (kind === 'focus-text') {
    return ctx.mac && ctx.inTextField && (state.pending || state.shown) ? 'hide' : 'none'
  }
  if (kind === 'keyup') {
    return e?.key === 'Alt' && (state.pending || state.shown) ? 'hide' : 'none'
  }
  // keydown
  if (!e) return 'none'
  if (e.key !== 'Alt') {
    // Another key pressed before the hold threshold: it is a quick chord (e.g. Alt+1, Alt+↑, Alt+b),
    // not a hold-to-peek gesture — cancel the timer so hints do not flash.
    if (state.pending) return 'cancel'
    // While hints are shown, adding another modifier or losing Alt drops the hints.
    if (state.shown && (e.ctrlKey || e.metaKey || e.shiftKey || !e.altKey)) return 'hide'
    return 'none'
  }
  if (e.repeat || state.pending || state.shown) return 'none'
  if (e.ctrlKey || e.metaKey || e.shiftKey || isComposingKey({ isComposing: !!e.isComposing, keyCode: e.keyCode ?? 0 })) return 'none'
  if (!ctx.railActive || ctx.modalOpen) return 'none'
  if (ctx.mac && ctx.inTextField) return 'none'
  return 'start-timer'
}

// Decides what the rail should do on a keydown event.
export function decideRailKey(
  e: RailKeyEvent,
  ctx: RailKeyContext & { hintsShown: boolean; itemCount: number; currentIndex: number },
): RailKeyDecision {
  if (!ctx.railActive || ctx.modalOpen || e.defaultPrevented) return { action: 'ignore' }
  const n = railDigit(e)
  if (n === 0 || n > ctx.itemCount) return { action: 'ignore' }
  // On macOS inside a regular text input, Option+digit types symbols (¡, ™, £, •…): never hijack.
  if (ctx.mac && ctx.inTextField) return { action: 'ignore' }
  // In a focused terminal with terminal-first priority, quick Alt/Option+digit goes to the terminal
  // program; holding Alt/Option until hints appear switches pages instead.
  if (ctx.inTerminal && ctx.keysFirst !== 'corral' && !ctx.hintsShown) return { action: 'ignore' }

  const index = n - 1
  if (e.repeat || index === ctx.currentIndex) {
    return { action: 'consume-current', index, hideHints: !e.repeat && ctx.hintsShown }
  }
  return {
    action: 'navigate',
    index,
    notifyTakenFromTerminal: ctx.inTerminal && !ctx.hintsShown,
    hideHints: ctx.hintsShown,
  }
}
