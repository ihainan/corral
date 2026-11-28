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
    default:
      return 'card.envReasonFailed'
  }
}
