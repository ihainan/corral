// Pure parser for JSON control frames sent by GET /api/sessions/{id}/pane-ws.
// Kept free of DOM/React imports so Node unit tests can exercise it directly.

export type PaneWsEvent =
  | { kind: 'ignore' }
  | { kind: 'fallback' }
  | { kind: 'closed' }
  | { kind: 'channels_busy' }

export function classifyPaneWsMessage(raw: string, receivedBytes: number): PaneWsEvent {
  let msg: { t?: string; code?: number | string; bytes?: number }
  try {
    msg = JSON.parse(raw) as { t?: string; code?: number | string; bytes?: number }
  } catch {
    return { kind: 'ignore' }
  }
  if (msg.t === 'error' && msg.code === 'ssh_channels_busy') {
    return { kind: 'channels_busy' }
  }
  if (msg.t === 'exit') {
    const exitCode = typeof msg.code === 'number' ? msg.code : 0
    const serverBytes = typeof msg.bytes === 'number' ? msg.bytes : 0
    if (exitCode !== 0 && Math.max(receivedBytes, serverBytes) < 256) {
      return { kind: 'fallback' }
    }
    return { kind: 'closed' }
  }
  return { kind: 'ignore' }
}

// Terminal-generated protocol replies (focus in/out, cursor position, device
// attributes, mode reports, OSC/DCS query replies, and SGR mouse wheel scroll)
// that xterm.js may emit via onData when the user is only viewing or scrolling.
const NON_USER_TERM_INPUT = [
  /^\x1b\[[IO]$/,
  /^\x1b\[\??\d+(?:;\d+)*[Rn]$/,
  /^\x1b\[[?>=][\d;]*c$/,
  /^\x1b\[\??[\d;]*\$y$/,
  /^\x1b\]\d+;[^\x07\x1b]*(?:\x07|\x1b\\)$/,
  /^\x1bP[\s\S]*\x1b\\$/,
  /^\x1b\[<6[4-7];\d+;\d+[Mm]$/,
]

// Whether an xterm.js onData / button byte payload represents real user input
// (typing, pressing Enter/Esc/arrows, pasting) rather than automatic terminal
// query responses or wheel scrolling.
export function isUserTerminalInput(data: string): boolean {
  if (!data) return false
  return !NON_USER_TERM_INPUT.some((re) => re.test(data))
}
