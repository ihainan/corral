// Pure session naming, validation, and key-migration helpers (tested in web/scripts).

// Matches backend tmuxNameRE: tmux rewrites '.' and ':' in session names, and
// Corral validates both tmux and herdr session names against this safe subset.
export const MUX_NAME_RE = /^[A-Za-z0-9_-]{1,64}$/

export type SessionNameError = '' | 'empty' | 'too_long' | 'invalid_chars'

export function validateMuxSessionName(raw: string): SessionNameError {
  if (!raw) return 'empty'
  if (raw.length > 64) return 'too_long'
  if (!MUX_NAME_RE.test(raw)) return 'invalid_chars'
  return ''
}

// Corral display names (used for herdr sessions, which do not support remote
// session renaming, and plain shells): 1..64 visible characters, no control chars.
export function validateDisplaySessionName(raw: string): SessionNameError {
  const s = raw.trim()
  if (!s) return 'empty'
  if ([...s].length > 64) return 'too_long'
  if (/[\x00-\x1f\x7f-\x9f]/.test(s)) return 'invalid_chars'
  return ''
}

export function normalizeMuxName(kind: 'tmux' | 'herdr', name: string): string {
  return kind === 'herdr' && (!name || name === 'default') ? '' : name
}

export function isDuplicateMuxName(kind: 'tmux' | 'herdr', name: string, existing: readonly string[]): boolean {
  const target = normalizeMuxName(kind, name)
  return existing.some((x) => normalizeMuxName(kind, x) === target)
}

// Rewrite tab order place keys when a tmux session is renamed so the tab stays
// in its exact position in TerminalTabs.
export function renameOrderEntries(
  order: readonly [string, number][],
  hostId: number,
  persistence: 'tmux' | 'herdr',
  oldName: string,
  newName: string,
): { order: [string, number][]; changed: boolean } {
  const oldBase = `${hostId}|${persistence}|${normalizeMuxName(persistence, oldName)}`
  const newBase = `${hostId}|${persistence}|${normalizeMuxName(persistence, newName)}`
  if (oldBase === newBase) return { order: order.map(([k, g]) => [k, g]), changed: false }
  let changed = false
  const next = order.map(([k, gone]): [string, number] => {
    if (k === oldBase) {
      changed = true
      return [newBase, gone]
    }
    if (k.startsWith(oldBase + '#')) {
      changed = true
      return [newBase + k.slice(oldBase.length), gone]
    }
    return [k, gone]
  })
  return { order: next, changed }
}

// Compute the canonical tmux entry key (matching chatSend.ts tmuxKey).
export function tmuxEntryKey(hostId: number, muxSession: string): string {
  return `${hostId}|tmux|${muxSession}`
}

// Event dispatched after a successful session rename so open views
// (TerminalTabs, Terminal, AgentsChat) update in sync without waiting for poll.
export const SESSION_RENAMED_EVENT = 'corral:session-renamed'

export type SessionRenamedPayload = {
  id: string
  host_id: number
  persistence?: 'none' | 'tmux' | 'herdr'
  oldMuxSession: string
  newMuxSession: string
  oldDisplayName: string
  newDisplayName: string
}

export function notifySessionRenamed(detail: SessionRenamedPayload) {
  if (typeof window === 'undefined') return
  window.dispatchEvent(new CustomEvent<SessionRenamedPayload>(SESSION_RENAMED_EVENT, { detail }))
}
