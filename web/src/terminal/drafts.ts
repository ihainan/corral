import { session as tabStore } from '../storage'

// The composer's memory: drafts by session, so one survives leaving the
// terminal page and coming back, and what was sent, to bring back; and the
// reply dialog's unsent answers. Any of them may hold secrets: drafts (with
// the pastes they hold) are also kept in this browser tab's own storage, so a
// reload keeps them and closing the tab ends them; the rest is in this page's
// memory only. A change of account ends all of it.
const drafts = new Map<string, string>()

export const getDraft = (session: string) => drafts.get(session) ?? ''
export function keepDraft(session: string, text: string) {
  if (text) drafts.set(session, text)
  else drafts.delete(session)
  saveSoon()
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
  saveSoon()
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

// Whose they are: a change of account (also learning who is signed in, at
// start) ends the memory of the one before, and brings back what this tab
// kept for this one (nothing for anyone else).
const STORE = 'corral.drafts'
const STORE_MAX = 2_000_000 // characters; past it the pastes are not kept
let owner: number | null = null
let saveTimer: number | undefined
export function draftsFor(uid: number | null) {
  if (uid === owner) return
  clearTimeout(saveTimer)
  saveTimer = undefined
  drafts.clear()
  replies.clear()
  pastes.clear()
  shown.clear()
  history = []
  owner = uid
  let saved: { uid?: number; drafts?: Record<string, string>; pastes?: [string, string][]; n?: number } | null = null
  try {
    saved = JSON.parse(tabStore.get(STORE) ?? 'null')
  } catch {
    saved = null
  }
  if (uid === null || !saved || saved.uid !== uid) {
    tabStore.remove(STORE)
    return
  }
  for (const [k, v] of Object.entries(saved.drafts ?? {})) if (typeof v === 'string' && v) drafts.set(k, v)
  for (const p of saved.pastes ?? []) if (Array.isArray(p) && typeof p[0] === 'string' && typeof p[1] === 'string') pastes.set(p[0], p[1])
  if (Number.isInteger(saved.n)) pasteN = Math.max(pasteN, saved.n as number)
}
function save() {
  saveTimer = undefined
  if (owner === null) return
  if (drafts.size === 0) return tabStore.remove(STORE)
  const held = [...pastes].filter(([l]) => [...drafts.values()].some((d) => d.includes(l)))
  let out = JSON.stringify({ uid: owner, drafts: Object.fromEntries(drafts), pastes: held, n: pasteN })
  if (out.length > STORE_MAX) out = JSON.stringify({ uid: owner, drafts: Object.fromEntries(drafts), pastes: [], n: pasteN })
  tabStore.set(STORE, out)
}
// (A moment after typing stops, and at once as the page goes or is hidden.)
function saveSoon() {
  clearTimeout(saveTimer)
  saveTimer = window.setTimeout(save, 400)
}
window.addEventListener('pagehide', () => saveTimer !== undefined && (clearTimeout(saveTimer), save()))
// (A phone may stop a page in the background without a pagehide: kept as it goes there.)
document.addEventListener('visibilitychange', () => document.hidden && saveTimer !== undefined && (clearTimeout(saveTimer), save()))
