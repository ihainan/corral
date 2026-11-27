// Pure version comparison and UI decision logic for Claude Code's AskUserQuestion hook.
// Kept free of React/DOM imports so Node test scripts can run it directly.

// First Claude Code version verified (2026-11-27, live models on 2.1.316,
// 2.1.318 and 2.1.330) to write AskUserQuestion to its session log while
// waiting for an answer without needing the PreToolUse hook. Older versions
// (reported since 2.1.281; observed running on 2.1.252, 2.1.284 and
// 2.1.299-2.1.304) still keep the hook.
export const ASK_HOOK_NO_NEED_VERSION = '2.1.316'

export interface AskHookInfo {
  state?: 'on' | 'on-own' | 'off' | 'blocked'
  why?: string
  cannot?: 'link' | 'hard_link' | 'not_file' | 'invalid' | 'no_sum'
  file: string
  seen: string
  snippet: string
  declined: boolean
  // True when every Claude Code version detected on this host is >= ASK_HOOK_NO_NEED_VERSION.
  unnecessary?: boolean
  // The bell hook's (the same file): Claude Code rings the terminal's bell when it stops.
  bell?: { state?: 'on' | 'off' | 'blocked'; why?: string; snippet: string }
}

// parseClaudeVersion parses "X.Y.Z" or "X.Y.Z (Claude Code)" into [major, minor, patch].
// Returns null for empty, unknown, pre-release, or malformed strings.
export function parseClaudeVersion(raw: string | null | undefined): [number, number, number] | null {
  if (typeof raw !== 'string') return null
  const s = raw.trim()
  if (!s || /[\r\n]/.test(s)) return null
  const sp = s.indexOf(' ')
  const ver = sp >= 0 ? s.slice(0, sp) : s
  if (sp >= 0) {
    const rest = s.slice(sp + 1).trim()
    if (!rest.startsWith('(') || !rest.endsWith(')')) return null
  }
  const parts = ver.split('.')
  if (parts.length !== 3) return null
  const nums: number[] = []
  for (const p of parts) {
    if (!/^\d+$/.test(p)) return null
    const n = Number(p)
    if (!Number.isSafeInteger(n) || n < 0) return null
    nums.push(n)
  }
  return [nums[0], nums[1], nums[2]]
}

export function compareClaudeVersion(a: [number, number, number], b: [number, number, number]): -1 | 0 | 1 {
  for (let i = 0; i < 3; i++) {
    if (a[i] < b[i]) return -1
    if (a[i] > b[i]) return 1
  }
  return 0
}

const NO_NEED_TUPLE = parseClaudeVersion(ASK_HOOK_NO_NEED_VERSION)!

// claudeNeedsAskHook returns true when the given Claude Code version is below
// ASK_HOOK_NO_NEED_VERSION, or unknown / unparseable (safe fallback).
export function claudeNeedsAskHook(version: string | null | undefined): boolean {
  const parsed = parseClaudeVersion(version)
  if (!parsed) return true
  return compareClaudeVersion(parsed, NO_NEED_TUPLE) < 0
}

// hintOf: what the card says of the hook, from what the host's file says:
// nothing (on — the question was asked before it — or not read), that hooks
// are turned off there, that the file is one Corral leaves alone, the offer,
// or (declined) only a quiet way back to it.
export function hintOf(info: AskHookInfo | null): 'none' | 'blocked' | 'cannot' | 'offer' | 'declined' {
  if (!info || info.state === 'on' || info.state === 'on-own') return 'none'
  if (info.state === 'blocked') return 'blocked'
  if (info.cannot) return 'cannot'
  if (!info.state) return 'none'
  return info.declined ? 'declined' : 'offer'
}

// asksFromScreen: a question of Claude Code's drawn from its screen, its log
// not having it — where the card may offer the hook: drawn as one (a question
// or a review page read off the screen), or the screen itself shown, with
// Claude Code's question frame's own last line on it.
// Suppressed when the session's Claude Code version is >= ASK_HOOK_NO_NEED_VERSION.
export function asksFromScreen(o: { agent?: string; logAsks: boolean; screen?: string; drawn: boolean; version?: string }): boolean {
  if (o.agent !== 'claude' || o.logAsks || o.screen === undefined) return false
  if (!claudeNeedsAskHook(o.version)) return false
  // (The frame's own last line, as the last line: words in a command a
  // permission prompt shows do not make it one — review535-7.)
  const last = o.screen.split('\n').filter((l) => l.trim()).pop() ?? ''
  return o.drawn || /^\s*Enter to select\b.*\bEsc to cancel\s*$/.test(last)
}

export type AskHookRowHintKey = 'askHook.rowHint' | 'askHook.rowHintUnnecessary' | 'askHook.rowHintUnnecessaryOn'

// askHookRowState decides the copy and controls for the host settings row.
// When info.unnecessary is true, an existing hook is not auto-deleted; the row
// explains it is no longer needed and keeps the turn-off button.
export function askHookRowState(info: AskHookInfo | null): {
  hintKey: AskHookRowHintKey
  canTurnOn: boolean
  canTurnOff: boolean
  showSnippet: boolean
} {
  const unnecessary = !!info?.unnecessary
  const hintKey: AskHookRowHintKey = unnecessary
    ? info?.state === 'on'
      ? 'askHook.rowHintUnnecessaryOn'
      : 'askHook.rowHintUnnecessary'
    : 'askHook.rowHint'
  return {
    hintKey,
    canTurnOn: !!info && !info.cannot && info.state === 'off' && !unnecessary,
    canTurnOff: !!info && !info.cannot && info.state === 'on',
    showSnippet: !!info?.cannot && !unnecessary,
  }
}
