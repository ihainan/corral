import { useSyncExternalStore } from 'react'
import type { RowTask } from './agentOrder'

// A task a side-list row hangs under an agent (UI item 8, 11-25), tapped: the
// agent's conversation is gone to, and that conversation, once shown, opens
// the task — a command's output, a subagent's own conversation — as one of its
// own lists would. One asked at a time: the last tap's.

export interface TaskAsked {
  key: string // the agent's (agentKey)
  task: RowTask
  n: number
  at: number // (Date.now() when asked: one not taken soon — its conversation could not be read — is let go, not opened later)
}

export const TASK_ASK_FOR = 30_000

let asked: TaskAsked | null = null
let seq = 0
const subs = new Set<() => void>()
const tell = () => subs.forEach((f) => f())

export function askTask(key: string, task: RowTask) {
  asked = { key, task, n: ++seq, at: Date.now() }
  tell()
}

// taskTaken: the conversation opened it (n: which asking), or it is let go.
export function taskTaken(n: number) {
  if (asked?.n !== n) return
  asked = null
  tell()
}

const subscribe = (f: () => void) => {
  subs.add(f)
  return () => subs.delete(f)
}

// useTaskAsked: the task asked of key's conversation, if one is.
export function useTaskAsked(key: string): TaskAsked | null {
  const a = useSyncExternalStore(subscribe, () => asked)
  return a && a.key === key ? a : null
}
