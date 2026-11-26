import type { ConvoEntry } from './conversation'

// Claude Code's built-in file editing tools whose changes are tracked by
// Claude Code's own Rewind checkpoints (Bash changes are NOT undone by Rewind).
export function isEditTool(name: string): boolean {
  return name === 'Edit' || name === 'MultiEdit' || name === 'Write' || name === 'NotebookEdit'
}

export function rewindFileBase(path: string): string {
  const first = path.split('\n')[0].trim().replace(/[\\/]+$/, '')
  if (!first) return ''
  return first.split(/[\\/]/).pop()?.trim() ?? ''
}

export interface RewindEditedFile {
  path: string
  name: string
}

// rewindEditedFiles collects the distinct files modified by Claude's edit
// tools (Write/Edit/MultiEdit/NotebookEdit) in active entries strictly after
// the target user message, preserving first-seen order. Failed tool calls,
// Bash edits, and already-gone entries are ignored.
export function rewindEditedFiles(entries: readonly ConvoEntry[], targetId: string): RewindEditedFile[] {
  if (!targetId) return []
  let targetIdx = -1
  for (let i = 0; i < entries.length; i++) {
    const e = entries[i]
    if (e.role === 'user' && !e.gone && !e.mid && e.id === targetId) {
      targetIdx = i
      break
    }
  }
  if (targetIdx < 0) return []

  const seen = new Set<string>()
  const out: RewindEditedFile[] = []
  for (let i = targetIdx + 1; i < entries.length; i++) {
    const e = entries[i]
    if (e.gone) continue
    for (const p of e.parts) {
      if (p.kind !== 'tool' || !p.tool || p.tool.error) continue
      if (!isEditTool(p.tool.name)) continue
      const rawPath = (p.tool.detail ?? '').split('\n')[0].trim().replace(/[\\/]+$/, '')
      if (!rawPath || seen.has(rawPath)) continue
      const name = rewindFileBase(rawPath)
      if (!name) continue
      seen.add(rawPath)
      out.push({ path: rawPath, name })
    }
  }
  return out
}

export const REWIND_FILES_MAX_SHOWN = 5

export interface FormattedRewindFiles {
  shown: { label: string; path: string }[]
  more: number
  total: number
}

// formatRewindFiles prepares edited files for display in the rewind confirm
// dialog: shows basenames (disambiguating duplicate basenames with their parent
// directory), capped at maxShown with the remainder reported in `more`.
export function formatRewindFiles(
  files: readonly RewindEditedFile[],
  maxShown = REWIND_FILES_MAX_SHOWN,
): FormattedRewindFiles {
  const nameCounts = new Map<string, number>()
  for (const f of files) {
    nameCounts.set(f.name, (nameCounts.get(f.name) ?? 0) + 1)
  }
  const limit = Math.max(0, maxShown)
  const shown = files.slice(0, limit).map((f) => {
    let label = f.name
    if ((nameCounts.get(f.name) ?? 0) > 1) {
      const segs = f.path.replace(/[\\/]+$/, '').split(/[\\/]/).filter(Boolean)
      if (segs.length >= 2) label = segs.slice(-2).join('/')
    }
    return { label, path: f.path }
  })
  return {
    shown,
    more: Math.max(0, files.length - shown.length),
    total: files.length,
  }
}

export type RewindMode = 'conversation' | 'code'

export interface RewindDialogState {
  hasCodeOption: boolean
  mode: RewindMode
  danger: boolean
  bodyKey: 'rewind.confirmBody' | 'rewind.confirmCodeBody'
  confirmKey: 'rewind.confirmTitle' | 'rewind.confirmCode'
}

export function rewindDialogState(mode: RewindMode, filesCount: number): RewindDialogState {
  const hasCodeOption = filesCount > 0
  const effective: RewindMode = hasCodeOption && mode === 'code' ? 'code' : 'conversation'
  const danger = effective === 'code'
  return {
    hasCodeOption,
    mode: effective,
    danger,
    bodyKey: danger ? 'rewind.confirmCodeBody' : 'rewind.confirmBody',
    confirmKey: danger ? 'rewind.confirmCode' : 'rewind.confirmTitle',
  }
}
