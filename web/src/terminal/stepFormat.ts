import type { ConvoEvent } from './conversation'
import type { Verb } from './convoItems'

export type TaskIconKind = 'cmd' | 'agent' | 'monitor'

export interface TaskEventMeta {
  icon: TaskIconKind
  headKey: string
  isCode: boolean
}

const KNOWN_CLI = /^(?:cd|ls|cat|head|tail|grep|rg|fd|find|sed|awk|echo|printf|env|export|sh|bash|zsh|fish|go|git|gh|npm|pnpm|yarn|npx|node|bun|deno|python|python3|pip|uv|pytest|cargo|rustc|make|cmake|docker|kubectl|helm|ssh|scp|rsync|curl|wget|tar|zip|unzip|chmod|chown|cp|mv|rm|mkdir|touch|ln|kill|ps|ss|lsof|tmux|herdr|playwright|vite|tsc|eslint|prettier)\b/

// Whether a background command's text is a raw shell command (rendered in
// monospace) rather than a human-readable description (rendered in UI font).
export function looksLikeShellCommand(raw: string): boolean {
  const s = raw.trim()
  if (!s) return false
  if (s.includes('\n')) return true
  if (/^(?:\.\.?\/|\/|~\/|\$\s|[A-Z_][A-Z0-9_]*=\S)/.test(s)) return true
  if (/(?:&&|\|\||[;|]\s|>>?|2>&1|\$\(|`)/.test(s)) return true
  const first = s.split(/\s+/)[0]
  if (/^[a-z0-9._-]+$/.test(first)) {
    if (KNOWN_CLI.test(s)) return true
    if (/\s(?:--?[a-zA-Z0-9]|\.\.?\/|\/|~\/)/.test(s)) return true
  }
  return false
}

// Structured presentation metadata for background task events (ConvoEvent
// with kind === 'task'): icon kind, i18n key for the status prefix, and
// whether the task's name should use monospace (only raw shell commands).
export function taskEventMeta(ev: Pick<ConvoEvent, 'task' | 'status' | 'text'>): TaskEventMeta {
  const icon: TaskIconKind = ev.task === 'agent' || ev.task === 'monitor' ? ev.task : 'cmd'
  const status = ev.status === 'failed' || ev.status === 'stopped' || ev.status === 'waiting' ? ev.status : 'done'
  return {
    icon,
    headKey: `convo.task.${icon}.${status}`,
    isCode: icon === 'cmd' && looksLikeShellCommand(ev.text ?? ''),
  }
}

// Whether a tool row's object text should use monospace: strictly limited to
// raw shell commands, file paths, and code search patterns (Grep/Glob).
export function toolObjectIsCode(name: string, verb: Verb | null, hasTitle: boolean, hasStdin: boolean): boolean {
  if (hasStdin || hasTitle) return false
  if (verb === 'cmd' || verb === 'read' || verb === 'edit' || verb === 'write') return true
  return name === 'Grep' || name === 'Glob'
}
