// Composer drafts by session, so one survives leaving the terminal page and
// coming back. Kept in this page's memory only (drafts may hold secrets):
// a reload or signing out ends them.
const drafts = new Map<string, string>()

export const getDraft = (session: string) => drafts.get(session) ?? ''
export function keepDraft(session: string, text: string) {
  if (text) drafts.set(session, text)
  else drafts.delete(session)
}
export const clearDrafts = () => drafts.clear()
