// Pure helpers for session environment variable filtering, masking state,
// copy gating, and unavailability reason mapping. Kept free of React/i18n
// runtime imports so web/scripts/test-pane-env.mjs can test it directly.

export interface PaneEnvVar {
  name: string
  sensitive: boolean
  value?: string
  preview?: string
}

export interface PaneEnvResponse {
  mode: string
  pane?: string
  pid?: number
  process?: string
  reason?: string
  truncated?: boolean
  vars: PaneEnvVar[]
}

export function sortPaneEnvVars(vars: readonly PaneEnvVar[]): PaneEnvVar[] {
  return [...vars].sort((a, b) => (a.name < b.name ? -1 : a.name > b.name ? 1 : 0))
}

// Filters variables by name (always) and visible value (non-sensitive or
// explicitly revealed). Hidden sensitive previews are never searched.
export function filterPaneEnvVars(
  vars: readonly PaneEnvVar[],
  revealed: Readonly<Record<string, string>>,
  query: string,
): PaneEnvVar[] {
  const sorted = sortPaneEnvVars(vars)
  const q = query.trim().toLowerCase()
  if (!q) return sorted
  return sorted.filter((v) => {
    if (v.name.toLowerCase().includes(q)) return true
    if (!v.sensitive) return (v.value ?? '').toLowerCase().includes(q)
    if (Object.prototype.hasOwnProperty.call(revealed, v.name)) {
      return revealed[v.name].toLowerCase().includes(q)
    }
    return false
  })
}

export function isEnvRevealed(v: PaneEnvVar, revealed: Readonly<Record<string, string>>): boolean {
  return !v.sensitive || Object.prototype.hasOwnProperty.call(revealed, v.name)
}

export function envDisplayValue(
  v: PaneEnvVar,
  revealed: Readonly<Record<string, string>>,
): { text: string; masked: boolean; revealed: boolean } {
  if (!v.sensitive) {
    return { text: v.value ?? '', masked: false, revealed: true }
  }
  if (Object.prototype.hasOwnProperty.call(revealed, v.name)) {
    return { text: revealed[v.name], masked: false, revealed: true }
  }
  return { text: v.preview || '••••••••', masked: true, revealed: false }
}

// Returns the full value eligible to copy, or undefined when a sensitive
// variable has not been revealed yet (copy requires explicit reveal first).
export function envCopyValue(v: PaneEnvVar, revealed: Readonly<Record<string, string>>): string | undefined {
  if (!v.sensitive) return v.value ?? ''
  if (Object.prototype.hasOwnProperty.call(revealed, v.name)) return revealed[v.name]
  return undefined
}

export type PaneEnvReasonKey =
  | 'card.envReasonNoProc'
  | 'card.envReasonPerm'
  | 'card.envReasonNoProcess'
  | 'card.envReasonHerdr'
  | 'card.envReasonUnsupported'
  | 'card.envReasonPaneNotInSession'
  | 'card.envReasonFailed'

export function envReasonKey(reason?: string): PaneEnvReasonKey {
  switch (reason) {
    case 'no_proc':
      return 'card.envReasonNoProc'
    case 'permission_denied':
      return 'card.envReasonPerm'
    case 'no_process':
      return 'card.envReasonNoProcess'
    case 'herdr_unavailable':
      return 'card.envReasonHerdr'
    case 'unsupported_mode':
      return 'card.envReasonUnsupported'
    case 'pane_not_in_session':
      return 'card.envReasonPaneNotInSession'
    default:
      return 'card.envReasonFailed'
  }
}

// Epoch-guarded reveal state machine so late POST /pane-env/reveal responses
// from before a collapse, session/pane switch, reload, or hide are always
// discarded and never re-expose masked secrets.

export interface PaneEnvRevealToken {
  epoch: number
  seq: number
}

export interface PaneEnvRevealState {
  epoch: number
  revealed: Record<string, string>
  revealing: Record<string, number>
  hiddenSeq: Record<string, number>
  nextSeq: number
  error: boolean
}

export function createPaneEnvRevealState(): PaneEnvRevealState {
  return {
    epoch: 0,
    revealed: {},
    revealing: {},
    hiddenSeq: {},
    nextSeq: 1,
    error: false,
  }
}

export function resetPaneEnvReveal(state: PaneEnvRevealState): PaneEnvRevealState {
  return {
    epoch: state.epoch + 1,
    revealed: {},
    revealing: {},
    hiddenSeq: {},
    nextSeq: state.nextSeq,
    error: false,
  }
}

export function startPaneEnvReveal(
  state: PaneEnvRevealState,
  name: string,
): { state: PaneEnvRevealState; token: PaneEnvRevealToken | null } {
  if (Object.prototype.hasOwnProperty.call(state.revealing, name)) {
    return { state, token: null }
  }
  const seq = state.nextSeq
  return {
    state: {
      ...state,
      revealing: { ...state.revealing, [name]: seq },
      nextSeq: seq + 1,
      error: false,
    },
    token: { epoch: state.epoch, seq },
  }
}

export function isPaneEnvRevealCurrent(
  state: PaneEnvRevealState,
  name: string,
  token: PaneEnvRevealToken,
): boolean {
  if (state.epoch !== token.epoch) return false
  if (state.revealing[name] !== token.seq) return false
  if ((state.hiddenSeq[name] ?? 0) >= token.seq) return false
  return true
}

export function finishPaneEnvReveal(
  state: PaneEnvRevealState,
  name: string,
  token: PaneEnvRevealToken,
  value: string,
): PaneEnvRevealState {
  if (!isPaneEnvRevealCurrent(state, name, token)) return state
  const nextRevealing = { ...state.revealing }
  delete nextRevealing[name]
  return {
    ...state,
    revealed: { ...state.revealed, [name]: value },
    revealing: nextRevealing,
    error: false,
  }
}

export function failPaneEnvReveal(
  state: PaneEnvRevealState,
  name: string,
  token: PaneEnvRevealToken,
): PaneEnvRevealState {
  if (!isPaneEnvRevealCurrent(state, name, token)) return state
  const nextRevealing = { ...state.revealing }
  delete nextRevealing[name]
  return {
    ...state,
    revealing: nextRevealing,
    error: true,
  }
}

export function hidePaneEnvVar(state: PaneEnvRevealState, name: string): PaneEnvRevealState {
  const hasRevealed = Object.prototype.hasOwnProperty.call(state.revealed, name)
  const hasRevealing = Object.prototype.hasOwnProperty.call(state.revealing, name)
  if (!hasRevealed && !hasRevealing) return state
  const nextRevealed = hasRevealed ? { ...state.revealed } : state.revealed
  if (hasRevealed) delete nextRevealed[name]
  const nextRevealing = hasRevealing ? { ...state.revealing } : state.revealing
  if (hasRevealing) delete nextRevealing[name]
  const seq = state.nextSeq
  return {
    ...state,
    revealed: nextRevealed,
    revealing: nextRevealing,
    hiddenSeq: { ...state.hiddenSeq, [name]: seq },
    nextSeq: seq + 1,
  }
}
