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

export function clearDrafts() {
  drafts.clear()
  replies.clear()
  history = []
}
