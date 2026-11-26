// Pure lifecycle decisions and peer-claim coordination for the conversation's
// main terminal view (UI 14, Ctrl+`), keeping an already-connected terminal
// mounted and hidden across quick Chat <-> Terminal toggles while yielding the
// backend's single direct pane-ws slot whenever another panel on the same pane
// needs it.

export interface TermKeepInput {
  keptKey: string | null
  currentKey: string | null
  canTermView: boolean
  termShown: boolean
  peerNeedsPane: boolean
}

export interface TermKeepDecision {
  keptKey: string | null
  mounted: boolean
  visible: boolean
}

export function resolveTermViewKeep(input: TermKeepInput): TermKeepDecision {
  const { keptKey, currentKey, canTermView, termShown, peerNeedsPane } = input
  if (!canTermView || !currentKey) {
    return { keptKey: null, mounted: false, visible: false }
  }
  if (termShown) {
    return { keptKey: currentKey, mounted: true, visible: true }
  }
  if (peerNeedsPane) {
    return { keptKey: null, mounted: false, visible: false }
  }
  const keep = keptKey === currentKey
  return { keptKey: keep ? currentKey : null, mounted: keep, visible: false }
}

export interface TermDims {
  cols: number
  rows: number
}

export function shouldResizePty(lastSent: TermDims | null, next: TermDims | null): boolean {
  if (!next || next.cols <= 0 || next.rows <= 0) return false
  if (!lastSent) return true
  return lastSent.cols !== next.cols || lastSent.rows !== next.rows
}

export interface PaneFocusInput {
  active: boolean
  wasActive: boolean
  coarse: boolean
  isLive: boolean
  isMain: boolean
  modalOpen: boolean
  hasFocus: boolean
}

export function paneFocusAction(input: PaneFocusInput): 'focus' | 'blur' | 'none' {
  if (!input.active) {
    return input.hasFocus ? 'blur' : 'none'
  }
  if (input.wasActive) return 'none'
  if (input.coarse || input.modalOpen) return 'none'
  if (input.isLive && !input.isMain) return 'none'
  return 'focus'
}

// Coordination for backend's single pane-ws attach slot per (session, pane):
// when any non-main PanelWindow (inline reply, dock terminal, composer panel)
// becomes active for a pane, any hidden kept main terminal for that same pane
// unmounts immediately to release the slot.
const paneClaimListeners = new Map<string, Set<() => void>>()

const paneClaimKey = (sid: string, pane: string) => `${sid}\0${pane}`

export function onPaneTerminalClaim(sid: string, pane: string, onClaim: () => void): () => void {
  const key = paneClaimKey(sid, pane)
  let set = paneClaimListeners.get(key)
  if (!set) {
    set = new Set()
    paneClaimListeners.set(key, set)
  }
  set.add(onClaim)
  return () => {
    const cur = paneClaimListeners.get(key)
    if (!cur) return
    cur.delete(onClaim)
    if (cur.size === 0) paneClaimListeners.delete(key)
  }
}

export function claimPaneTerminal(sid: string, pane: string): void {
  if (!sid || !pane) return
  const set = paneClaimListeners.get(paneClaimKey(sid, pane))
  if (!set) return
  for (const fn of [...set]) fn()
}
