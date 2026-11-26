// isRewindIdle reports whether the in-place Claude Code rewind action should
// be enabled. Prefer the resolved agent status (which accounts for blocked /
// waiting-for-reply states even when herdr still reports the pane idle), and
// fall back to the raw herdr row status only when agent status is unavailable.
export function isRewindIdle(
  agent: { status?: string; dialog?: boolean } | null | undefined,
  rowStatus: string | undefined,
  persistence: string | undefined,
): boolean {
  if (persistence !== 'herdr' && persistence !== 'tmux') return false
  if (agent?.dialog) return false
  const status = agent?.status ?? rowStatus
  return status === 'idle' || status === 'done'
}
