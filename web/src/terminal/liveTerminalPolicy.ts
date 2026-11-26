// Decides whether an error or state in AgentsChat should automatically pop up
// the inline live terminal (PanelWindow in live mode), refresh for a native
// prompt card, offer a non-intrusive "Open terminal" button, or stay silent.
//
// Only confirmed states that require the user to act in the terminal auto-open
// the live terminal:
//   - A terminal dialog/picker the chat page cannot render (agent_waiting when
//     no native AskCard/PanelWindow is open, picker).
//   - The terminal focus is on another view (subagent_view, other_view).
//   - Unsent words in the terminal's input box (box_draft, draft) or a message
//     verified to be stuck in the input unsent (typed_not_sent,
//     typed_other_view, enter_not_taken).
//
// Ambiguous / transient states where the agent may simply be working normally
// (unknown, no_screen, cannot_verify, no_input) never auto-open; they return
// 'hint' so the page can offer a clickable terminal button without interrupting.
// Normal working / busy states (agent_busy, working, no error / log lag) return
// 'none'.

export type LiveDecision = 'open' | 'refresh' | 'hint' | 'none'

export interface LiveTerminalInput {
  code?: string
  agent?: string
  hasBind?: boolean
  waits?: boolean
  panelOpen?: boolean
}

export const liveFor = (agent?: string): boolean =>
  (agent ?? 'claude') === 'claude' || agent === 'opencode' || agent === 'codex' || agent === 'agy'

export const CONFIRMED_LIVE_CODES: ReadonlySet<string> = new Set([
  'agent_waiting',
  'picker',
  'subagent_view',
  'other_view',
  'box_draft',
  'draft',
  'typed_not_sent',
  'typed_other_view',
  'enter_not_taken',
])

export const HINT_LIVE_CODES: ReadonlySet<string> = new Set([
  'unknown',
  'no_screen',
  'cannot_verify',
  'no_input',
])

export function decideLiveTerminal(input: LiveTerminalInput): LiveDecision {
  const { code, agent, hasBind = true, waits = false, panelOpen = false } = input
  if (!hasBind || !code) return 'none'
  if (code === 'agent_waiting' && agent === 'agy') return 'refresh'
  if (!liveFor(agent)) return 'none'
  if (waits || panelOpen) return 'none'
  if (CONFIRMED_LIVE_CODES.has(code)) return 'open'
  if (HINT_LIVE_CODES.has(code)) return 'hint'
  return 'none'
}

export function shouldOpenLiveTerminal(input: LiveTerminalInput): boolean {
  return decideLiveTerminal(input) === 'open'
}

export function shouldClearLiveTerminal(state: { waits?: boolean; panelOpen?: boolean }): boolean {
  return !!(state.waits || state.panelOpen)
}
