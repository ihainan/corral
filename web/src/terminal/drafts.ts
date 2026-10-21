// The composer's memory: drafts by session, so one survives leaving the
// terminal page and coming back, and what was sent, to bring back; and the
// reply dialog's unsent answers. Kept in this page's memory only (any of
// them may hold secrets): a reload or a change of account ends them.
const drafts = new Map<string, string>()

export const getDraft = (session: string) => drafts.get(session) ?? ''
export function keepDraft(session: string, text: string) {
  if (text) drafts.set(session, text)
  else drafts.delete(session)
}

// Sent messages, oldest first, across this account's sessions.
const HISTORY_MAX = 50
let history: string[] = []
export const sentHistory = (): readonly string[] => history
export function remember(text: string) {
  if (!text.trim()) return
  history = [...history.filter((h) => h !== text), text].slice(-HISTORY_MAX)
}

// Answers being typed to agents (the reply dialog), by session and pane.
const replies = new Map<string, string>()
export const getReplyDraft = (key: string) => replies.get(key) ?? ''
export function keepReplyDraft(key: string, text: string) {
  if (text) replies.set(key, text)
  else replies.delete(key)
}

// Long pastes into the composer: shown there as a label ("[Pasted text #1,
// 120 lines]"), sent as the text itself. Kept with the drafts (sent ones too,
// so an entry brought back from the history still expands); past a few the
// oldest that no draft (one put aside while going through the history is
// kept as the draft) or history entry holds goes.
const PASTES_MAX = 50
const pastes = new Map<string, string>()
let pasteN = 0
// What each composer shows now (an entry brought back from the history is
// shown while the draft kept is the one put aside).
const shown = new Map<string, string>()
export function showDraft(session: string, text: string) {
  if (text) shown.set(session, text)
  else shown.delete(session)
}
const inUse = (l: string) =>
  history.some((h) => h.includes(l)) || [...drafts.values()].some((d) => d.includes(l)) || [...shown.values()].some((d) => d.includes(l))
export function keepPaste(text: string, label: (n: number) => string): string {
  const l = label(++pasteN)
  pastes.set(l, text)
  for (const old of pastes.keys()) {
    if (pastes.size <= PASTES_MAX) break
    if (old !== l && !inUse(old)) pastes.delete(old)
  }
  return l
}
// The text with its labels expanded, or null past max characters (counted as
// it goes, so a label repeated many times is not expanded first).
export function expandPastes(s: string, max: number): string | null {
  let out = ''
  let rest = s
  for (;;) {
    let at = -1
    let label = ''
    for (const l of pastes.keys()) {
      const i = rest.indexOf(l)
      if (i >= 0 && (at < 0 || i < at)) [at, label] = [i, l]
    }
    if (at < 0) break
    out += rest.slice(0, at) + pastes.get(label)!
    rest = rest.slice(at + label.length)
    if (out.length > max) return null
  }
  out += rest
  return out.length > max ? null : out
}

export function clearDrafts() {
  drafts.clear()
  replies.clear()
  pastes.clear()
  shown.clear()
  history = []
}
