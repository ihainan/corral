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
