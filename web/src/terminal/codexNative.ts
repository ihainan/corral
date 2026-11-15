// Native items are keyed by engine IDs. Replayed completions replace the same
// item; they never create a second copy of a message or append its text twice.
export interface NativeThread {
  id: string
  name?: string
  preview?: string
  cwd: string
  model?: string
  reasoningEffort?: string
  canAcceptDirectInput?: boolean
  status: { type: string; activeFlags?: string[] }
}
export interface NativeItem {
  id: string
  type: string
  text?: string
  summary?: string[]
  content?: { type: string; text?: string; path?: string; url?: string }[]
  command?: string | string[]
  aggregatedOutput?: string
  status?: string
  changes?: { path: string; diff?: string }[]
  server?: string
  tool?: string
  name?: string
  result?: unknown
  _history?: boolean
}
export interface NativeRow { item: NativeItem; turnId: string; completedAtMs?: number | null }
export interface NativeEvent {
  seq: number
  method: string
  thread?: string
  request_id?: string | number
  params?: Record<string, unknown>
  requests?: NativeEvent[]
}
export interface NativeSnapshot {
  thread: NativeThread
  page: { data: NativeRow[]; nextCursor?: string | null }
  seq: number
  connected: boolean
  turn: string
  requests: NativeEvent[] | null
}

export function applyNativeEvent(rows: NativeRow[], event: NativeEvent): NativeRow[] {
  const p = event.params ?? {}
  const item = p.item as NativeItem | undefined
  const id = item?.id ?? (p.itemId as string | undefined)
  if (!id) return rows
  const at = rows.findIndex(r => r.item.id === id)
  const was = at < 0 ? undefined : rows[at].item
  let next: NativeItem
  if (event.method === 'item/completed' && item) next = { ...item, _history: true }
  else if (event.method === 'item/started' && item) {
    if (was?._history) return rows
    next = item
  } else if (event.method === 'item/agentMessage/delta') {
    if (was?._history) return rows
    next = { ...was, id, type: 'agentMessage', text: ((was?.text ?? '') + String(p.delta ?? '')).slice(-131072) }
  } else if (event.method === 'item/reasoning/summaryTextDelta') {
    if (was?._history) return rows
    const summary = [...(was?.summary ?? [])]
    const index = Number(p.summaryIndex ?? 0)
    if (!Number.isInteger(index) || index < 0 || index > 128) return rows
    summary[index] = ((summary[index] ?? '') + String(p.delta ?? '')).slice(-65536)
    next = { ...was, id, type: 'reasoning', summary }
  } else if (event.method === 'item/commandExecution/outputDelta') {
    if (was?._history) return rows
    next = { ...was, id, type: 'commandExecution', aggregatedOutput: ((was?.aggregatedOutput ?? '') + String(p.delta ?? '')).slice(-65536) }
  } else return rows
  const row = { item: next, turnId: String(p.turnId ?? (at < 0 ? '' : rows[at].turnId)) }
  return (at < 0 ? [...rows, row] : rows.map((r, i) => i === at ? row : r)).slice(-1000)
}

export function nativeUserText(text: string): string {
  const match = /^\s*<send_user_message_question_reply>([\s\S]*)<\/send_user_message_question_reply>\s*$/.exec(text)
  if (match) {
    try {
      const rows: unknown = JSON.parse(match[1])
      if (Array.isArray(rows) && rows.length && rows.every(r => typeof r?.answer === 'string')) return rows.map(r => [r.question, r.answer].filter(Boolean).join('\n')).join('\n\n')
    } catch { /* Preserve malformed content instead of dropping a user message. */ }
  }
  const notice = /^\s*<task-notification>[\s\S]*?<summary>([\s\S]*?)<\/summary>[\s\S]*?<\/task-notification>\s*$/.exec(text)
  return notice?.[1] ?? text
}

export function importedTool(text: string): { name: string; body: string; title: string } | null {
  const match = /^\[external agent tool (?:call|result): ([\w.:+-]{1,128})\]\s*\n([\s\S]*)$/.exec(text)
  if (!match) return null
  const description = /^description: ([^\n]*)\n/.exec(match[2])
  return { name: match[1], title: description?.[1] ?? match[1], body: match[2].replace(/^description: [^\n]*\n/, '').replace(/^command: /, '') }
}
