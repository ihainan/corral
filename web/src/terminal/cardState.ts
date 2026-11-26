// Pure state transitions for Claude Code's "Heads up" / "You should know" card
// (cardNotice.tsx): immediate optimistic dismissal for closing actions,
// processing state for expanding actions ("Learn more"), suppression of stale
// polls showing the just-pressed card while the terminal redraws, and rollback
// with error details if the background request fails.

export interface ClaudeCard {
  state: 'offer' | 'explained' | 'making' | 'folded' | 'ready' | 'failed' | 'other'
  tag?: string
  text: string
  choices: { key: string; label: string }[] | null
  actionable: boolean
  empty: boolean
}

export type PressMode = 'dismiss' | 'keep'

export interface PendingPress {
  id: number
  sig: string
  key: string
  label: string
  mode: PressMode
  phase: 'sending' | 'settling'
  saved: ClaudeCard
  until: number
}

export interface CardViewState {
  card: ClaudeCard | null
  stale: boolean
  pending: PendingPress | null
  error: string
  errorSeq: number
  lastPressId: number
}

// How long to keep suppressing polls that still show the exact same card
// after POST /conversation/card succeeds, matching cardLooker's AFTER_PRESS.
export const SETTLE_MS = 16_000

const DISMISS_LABELS = new Set([
  'Knew this already',
  'Know this already',
  'Dismiss',
  'Understood',
  'Chat in main session',
  'Disable',
  'Helpful',
  'Not helpful',
  'Didn’t read',
  "Didn't read",
  'OK',
])

export function pressModeOf(label: string): PressMode {
  return DISMISS_LABELS.has(label) ? 'dismiss' : 'keep'
}

export function cardSignature(card: ClaudeCard): string {
  const choices = (card.choices ?? []).map((c) => `${c.key}:${c.label}`).join('\x1f')
  return [card.state, card.tag ?? '', card.text, choices].join('\x1e')
}

export function initialCardState(lastPressId = 0): CardViewState {
  return { card: null, stale: false, pending: null, error: '', errorSeq: 0, lastPressId }
}

export function resetCardState(state: CardViewState): CardViewState {
  // Bump lastPressId so any in-flight press from the previous conversation is ignored.
  return initialCardState(state.lastPressId + 1)
}

export function startCardPress(
  state: CardViewState,
  key: string,
  label: string,
): { state: CardViewState; accepted: boolean; pressId: number } {
  const card = state.card
  if (!card || state.pending || state.stale || !card.actionable || !card.empty) {
    return { state, accepted: false, pressId: 0 }
  }
  const choice = (card.choices ?? []).find((c) => c.key === key)
  if (!choice) {
    return { state, accepted: false, pressId: 0 }
  }
  const pressId = state.lastPressId + 1
  const resolvedLabel = label || choice.label
  const pending: PendingPress = {
    id: pressId,
    sig: cardSignature(card),
    key,
    label: resolvedLabel,
    mode: pressModeOf(resolvedLabel),
    phase: 'sending',
    saved: card,
    until: Infinity,
  }
  return {
    state: { ...state, pending, error: '', lastPressId: pressId },
    accepted: true,
    pressId,
  }
}

export function finishCardPress(
  state: CardViewState,
  pressId: number,
  now: number,
  settleMs = SETTLE_MS,
): CardViewState {
  if (!state.pending || state.pending.id !== pressId) return state
  return {
    ...state,
    pending: { ...state.pending, phase: 'settling', until: now + settleMs },
  }
}

export function failCardPress(
  state: CardViewState,
  pressId: number,
  code: string,
  message: string,
): CardViewState {
  if (!state.pending || state.pending.id !== pressId) return state
  if (code === 'card_gone') {
    return { ...state, card: null, stale: false, pending: null, error: '' }
  }
  return {
    ...state,
    card: state.card ?? state.pending.saved,
    pending: null,
    error: message,
    errorSeq: state.errorSeq + 1,
  }
}

export function pollCardFound(
  state: CardViewState,
  nextCard: ClaudeCard | null,
  now: number,
): CardViewState {
  const pending = state.pending
  if (pending) {
    if (nextCard === null) {
      if (pending.phase === 'sending') {
        return { ...state, card: null, stale: false }
      }
      return { ...state, card: null, stale: false, pending: null, error: '' }
    }
    const nextSig = cardSignature(nextCard)
    if (nextSig !== pending.sig) {
      return { ...state, card: nextCard, stale: false, pending: null, error: '' }
    }
    if (pending.phase === 'sending' || now < pending.until) {
      return { ...state, card: nextCard, stale: false }
    }
    return { ...state, card: nextCard, stale: false, pending: null }
  }
  if (nextCard === null) {
    return { ...state, card: null, stale: false, error: '' }
  }
  const changed = !state.card || cardSignature(state.card) !== cardSignature(nextCard)
  return {
    ...state,
    card: nextCard,
    stale: false,
    error: changed ? '' : state.error,
  }
}

export function pollCardFailed(state: CardViewState): CardViewState {
  return { ...state, stale: true }
}

export function expireCardSettle(state: CardViewState, now: number): CardViewState {
  if (!state.pending || state.pending.phase !== 'settling' || now < state.pending.until) return state
  return { ...state, pending: null }
}

export function visibleCard(state: CardViewState, now: number): ClaudeCard | null {
  if (
    state.pending &&
    state.pending.mode === 'dismiss' &&
    (state.pending.phase === 'sending' || now < state.pending.until)
  ) {
    return null
  }
  return state.card
}

export function activeBusyKey(state: CardViewState, now: number): string | null {
  if (
    state.pending &&
    state.pending.mode === 'keep' &&
    (state.pending.phase === 'sending' || now < state.pending.until)
  ) {
    return state.pending.key
  }
  return null
}
