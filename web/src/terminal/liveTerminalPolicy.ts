// Decides whether an error or state in AgentsChat should automatically show a
// native DraftCard, pop up the inline live terminal (PanelWindow in live mode),
// refresh for a native prompt card, offer a non-intrusive "Show the terminal"
// button, or stay silent.
//
// Recognized unsent drafts in the terminal's input box (box_draft / draft on
// Claude Code or Codex, with extracted text and verified digest) return
// 'draft_card' so the page handles them natively (Send / Clear / Handle in
// terminal) without throwing a raw terminal at the user.
//
// Confirmed states the chat page cannot render natively auto-open the live
// terminal with a one-sentence explanation and block chat sending until the
// trigger reason disappears:
//   - A terminal dialog/picker the chat page cannot render (agent_waiting when
//     no native AskCard/PanelWindow is open, picker).
//   - The terminal focus is on another view (subagent_view, other_view).
//   - Unrecognized/unparseable terminal drafts (draft / box_draft without
//     extractable text or digest) or a message verified stuck in the input
//     unsent (typed_not_sent, typed_other_view, enter_not_taken).
//
// Ambiguous / transient states where the agent may simply be working normally
// (unknown, no_screen, cannot_verify, no_input) never auto-open; they return
// 'hint' so the page can offer a clickable terminal button without interrupting.
// Normal working / busy states (agent_busy, working, no error / log lag) return
// 'none'.

export type LiveDecision = 'open' | 'draft_card' | 'refresh' | 'hint' | 'none'

export type LiveReason =
  | 'agent_waiting'
  | 'picker'
  | 'subagent_view'
  | 'other_view'
  | 'box_draft'
  | 'draft'
  | 'typed_not_sent'
  | 'typed_other_view'
  | 'enter_not_taken'

export type LiveNoteKey =
  | 'panel.why.agentWaiting'
  | 'panel.why.picker'
  | 'panel.why.subagentView'
  | 'panel.why.otherView'
  | 'panel.why.draft'
  | 'panel.why.typedNotSent'
  | 'panel.why.typedOtherView'
  | 'panel.why.enterNotTaken'

export type ComposerBlockKey =
  | 'agents.blocked.draft'
  | 'agents.blocked.agentWaiting'
  | 'agents.blocked.picker'
  | 'agents.blocked.subagentView'
  | 'agents.blocked.otherView'
  | 'agents.blocked.unsent'

export interface ScreenBlocker {
  code: string
  draft?: string
  digest?: string
}

export interface TerminalBlockerState {
  bind: string
  reason: LiveReason
  mode: 'draft_card' | 'terminal' | 'dismissed'
  draft?: string
  digest?: string
  openedEndSeq: number
}

export interface LiveTerminalInput {
  code?: string
  agent?: string
  hasBind?: boolean
  waits?: boolean
  panelOpen?: boolean
  draft?: string
  digest?: string
  dismissedReason?: string | null
}

export interface BlockerObservation {
  waits?: boolean
  panelOpen?: boolean
  agentStatus?: string
  logAdvanced?: boolean
  screenChecked?: boolean
  screenBlocker?: ScreenBlocker | null
}

export const liveFor = (agent?: string): boolean =>
  (agent ?? 'claude') === 'claude' || agent === 'opencode' || agent === 'codex' || agent === 'agy'

export const CONFIRMED_LIVE_CODES: ReadonlySet<string> = new Set<LiveReason>([
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

export function isLiveReason(code?: string): code is LiveReason {
  return !!code && CONFIRMED_LIVE_CODES.has(code)
}

export function normalizeLiveReason(code?: string | null): string | null {
  if (!code || !CONFIRMED_LIVE_CODES.has(code)) return null
  if (code === 'box_draft' || code === 'draft') return 'draft'
  return code
}

export function sameBlockerReason(a?: string | null, b?: string | null): boolean {
  const na = normalizeLiveReason(a)
  return !!na && na === normalizeLiveReason(b)
}

export function canUseDraftCard(input: { code?: string; agent?: string; draft?: string; digest?: string }): boolean {
  if (input.code !== 'box_draft' && input.code !== 'draft') return false
  const kind = input.agent ?? 'claude'
  if (kind !== 'claude' && kind !== 'codex') return false
  return !!input.draft?.trim() && !!input.digest
}

export function decideLiveTerminal(input: LiveTerminalInput): LiveDecision {
  const { code, agent, hasBind = true, waits = false, panelOpen = false, draft, digest, dismissedReason } = input
  if (!hasBind || !code) return 'none'
  if (code === 'agent_waiting' && agent === 'agy') return 'refresh'
  if (!liveFor(agent)) return 'none'
  if (waits || panelOpen) return 'none'
  if (CONFIRMED_LIVE_CODES.has(code)) {
    if (sameBlockerReason(dismissedReason, code)) return 'hint'
    if (canUseDraftCard({ code, agent, draft, digest })) return 'draft_card'
    return 'open'
  }
  if (HINT_LIVE_CODES.has(code)) return 'hint'
  return 'none'
}

export function shouldOpenLiveTerminal(input: LiveTerminalInput): boolean {
  return decideLiveTerminal(input) === 'open'
}

export function shouldClearLiveTerminal(state: { waits?: boolean; panelOpen?: boolean }): boolean {
  return !!(state.waits || state.panelOpen)
}

export function liveTerminalNoteKey(reason: LiveReason): LiveNoteKey {
  switch (reason) {
    case 'agent_waiting':
      return 'panel.why.agentWaiting'
    case 'picker':
      return 'panel.why.picker'
    case 'subagent_view':
      return 'panel.why.subagentView'
    case 'other_view':
      return 'panel.why.otherView'
    case 'box_draft':
    case 'draft':
      return 'panel.why.draft'
    case 'typed_not_sent':
      return 'panel.why.typedNotSent'
    case 'typed_other_view':
      return 'panel.why.typedOtherView'
    case 'enter_not_taken':
      return 'panel.why.enterNotTaken'
  }
}

export function composerBlockKey(reason: LiveReason): ComposerBlockKey {
  switch (reason) {
    case 'box_draft':
    case 'draft':
      return 'agents.blocked.draft'
    case 'agent_waiting':
      return 'agents.blocked.agentWaiting'
    case 'picker':
      return 'agents.blocked.picker'
    case 'subagent_view':
      return 'agents.blocked.subagentView'
    case 'other_view':
      return 'agents.blocked.otherView'
    case 'typed_not_sent':
    case 'typed_other_view':
    case 'enter_not_taken':
      return 'agents.blocked.unsent'
  }
}

export function isBlockerResolved(reason: LiveReason, obs: BlockerObservation): boolean {
  if (obs.waits || obs.panelOpen) return true
  if (obs.screenChecked) {
    const code = obs.screenBlocker?.code ?? ''
    if (code === '') return true
  }
  if (obs.logAdvanced) {
    if (
      reason === 'agent_waiting' ||
      reason === 'picker' ||
      reason === 'typed_not_sent' ||
      reason === 'typed_other_view' ||
      reason === 'enter_not_taken'
    ) {
      return true
    }
  }
  if (obs.agentStatus === 'working') {
    if (reason === 'agent_waiting' || reason === 'picker' || reason === 'typed_not_sent') {
      return true
    }
  }
  return false
}

export function reduceBlockerOnScreen(
  cur: TerminalBlockerState,
  agent: string | undefined,
  obs: BlockerObservation,
): { next: TerminalBlockerState | null; resolved: boolean } {
  if (isBlockerResolved(cur.reason, obs)) {
    return { next: null, resolved: true }
  }
  const sb = obs.screenChecked ? obs.screenBlocker : null
  if (!sb || !sb.code || sb.code === 'no_input') {
    return { next: cur, resolved: false }
  }
  if (cur.mode === 'draft_card') {
    if (canUseDraftCard({ code: sb.code, agent, draft: sb.draft, digest: sb.digest })) {
      if (sb.draft !== cur.draft || sb.digest !== cur.digest) {
        return {
          next: { ...cur, reason: sb.code as LiveReason, draft: sb.draft, digest: sb.digest },
          resolved: false,
        }
      }
      return { next: cur, resolved: false }
    }
    if (isLiveReason(sb.code)) {
      return {
        next: { ...cur, reason: sb.code, mode: 'terminal', draft: undefined, digest: undefined },
        resolved: false,
      }
    }
  } else if (cur.mode === 'terminal') {
    if (isLiveReason(sb.code) && !sameBlockerReason(cur.reason, sb.code)) {
      return {
        next: { ...cur, reason: sb.code, draft: sb.draft, digest: sb.digest },
        resolved: false,
      }
    }
  }
  return { next: cur, resolved: false }
}
