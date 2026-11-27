// Pure classification helpers for backend busy/refusal API error codes,
// shared by AgentsChat and PanelWindow so capacity refusals (ssh_channels_busy)
// are handled consistently with legacy busy codes.

// Refusals of a switch made before any key was pressed (see convoPress, convoModeTo).
export const NOTHING_PRESSED: readonly string[] = [
  'bad_request',
  'unauthorized',
  'unsupported',
  'busy',
  'ssh_channels_busy',
  'host_error',
  'agent_changed',
  'not_in_front',
  'refused',
  'no_screen',
  'not_found',
  'forbidden',
  'subagent_view',
  'other_view',
  'no_input',
  'box_draft',
  'screen_changed',
]

// Transient SSH channel / pane busy codes that should be treated as busy rather
// than a real read/transport failure.
export function isBusyErrorCode(code: string | undefined): boolean {
  return code === 'busy' || code === 'ssh_channels_busy'
}

// Whether a failed mode/press refusal guarantees no key was pressed in the pane.
export function isNothingPressedCode(code: string | undefined): boolean {
  return !!code && NOTHING_PRESSED.includes(code)
}

// Whether a PanelWindow poll error should be ignored without incrementing the
// failure counter toward the "lost" state.
export function shouldIgnorePanelPollError(aborted: boolean, code: string | undefined): boolean {
  return aborted || isBusyErrorCode(code)
}
