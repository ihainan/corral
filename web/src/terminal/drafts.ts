import { session as tabStore } from '../storage'

// The composer's memory: drafts by session, so one survives leaving the
// terminal page and coming back, and what was sent, to bring back; and the
// reply dialog's unsent answers. Any of them may hold secrets: drafts (with
// the pastes they hold), and messages sent whose fate is not known yet, are
// also kept in this browser tab's own storage, so a reload keeps them and
// closing the tab ends them; the rest is in this page's memory only. A change
// of account ends all of it.
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

// Messages sent from the agents page whose fate is not known yet (not seen
// in the agent's log), by agent: kept with the drafts, so a reload does not
// forget one that may never have gone. What each holds is the page's to say.
let sent: Record<string, unknown[]> = {}
export const keptSent = (): Readonly<Record<string, unknown[]>> => sent
export function keepSent(all: Record<string, unknown[]>) {
  sent = Object.fromEntries(Object.entries(all).filter(([, v]) => v.length))
  saveSoon()
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

// Files put up to the host from a composer: shown in the draft as a label
// ("[图片 #3：shot.png]") with a chip over the box, sent as what the agent
// reads — the path, quoted as the upload put it (send). Kept with the
// drafts, apart from the pastes and never given up for them (each is
// small); past a few the oldest no draft or history entry holds goes.
// host: the host it was put on (a label taken to another host's composer
// is not sent there).
export type Attached = { label: string; send: string; path: string; name: string; image: boolean; host: number }
const ATTACHED_MAX = 200
const attached = new Map<string, Attached>()
let attachN = 0
export function keepAttached(a: Omit<Attached, 'label'>, label: (n: number) => string): Attached {
  const one = { ...a, label: label(++attachN) }
  attached.set(one.label, one)
  for (const old of attached.keys()) {
    if (attached.size <= ATTACHED_MAX) break
    if (old !== one.label && !inUse(old)) attached.delete(old)
  }
  saveSoon()
  return one
}
// The files a text holds, in the order it holds them (each once).
export function attachedIn(s: string): Attached[] {
  const at: [number, Attached][] = []
  for (const a of attached.values()) {
    const i = s.indexOf(a.label)
    if (i >= 0) at.push([i, a])
  }
  return at.sort((x, y) => x[0] - y[0]).map(([, a]) => a)
}

// What of a text cannot be sent as it is: a file put on another host than
// this one, or a label of a file whose path is not known (any more: the
// tab's storage lost it). label: a whole label's pattern. Its words, to be
// said; null where all is well — or where the text is too long to send at
// all (max, as expandPastes is given it: that says so). A long paste's text
// is looked at as it will be sent.
export function unsendable(s: string, host: number, label: RegExp, max: number): { other?: Attached; lost?: string } | null {
  const text = pastesExpanded(s, max)
  if (text === null) return null
  const other = attachedIn(text).find((a) => a.host !== host)
  if (other) return { other }
  for (const m of text.matchAll(label)) if (!attached.has(m[0])) return { lost: m[0] }
  return null
}
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
// (Files put up go in as what the agent reads, their paths — also ones a
// paste holds: the pastes first, then the files, each once.)
export function expandPastes(s: string, max: number): string | null {
  const t = pastesExpanded(s, max)
  return t === null ? null : expandLabels(t, new Map([...attached].map(([l, a]) => [l, a.send])), max)
}
// (Before the files are, a label may be longer than its path: the text in between bounded apart.)
const pastesExpanded = (s: string, max: number) => expandLabels(s, pastes, 2 * max + (64 << 10))
function expandLabels(s: string, labels: Map<string, string>, max: number): string | null {
  let out = ''
  let rest = s
  const of = (l: string) => labels.get(l)!
  for (;;) {
    let at = -1
    let label = ''
    for (const l of labels.keys()) {
      const i = rest.indexOf(l)
      if (i >= 0 && (at < 0 || i < at)) [at, label] = [i, l]
    }
    if (at < 0) break
    out += rest.slice(0, at) + of(label)
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
  attached.clear()
  shown.clear()
  history = []
  sent = {}
  owner = uid
  let saved: { uid?: number; drafts?: Record<string, string>; pastes?: [string, string][]; n?: number; files?: Attached[]; fn?: number; sent?: Record<string, unknown> } | null = null
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
  for (const a of saved.files ?? [])
    if (a && typeof a.label === 'string' && typeof a.send === 'string' && typeof a.path === 'string' && typeof a.name === 'string' && typeof a.host === 'number')
      attached.set(a.label, { label: a.label, send: a.send, path: a.path, name: a.name, image: !!a.image, host: a.host })
  if (Number.isInteger(saved.fn)) attachN = Math.max(attachN, saved.fn as number)
  for (const [k, v] of Object.entries(saved.sent ?? {})) if (Array.isArray(v) && v.length) sent[k] = v
}
function save() {
  saveTimer = undefined
  if (owner === null) return
  if (drafts.size === 0 && Object.keys(sent).length === 0) return tabStore.remove(STORE)
  const held = [...pastes].filter(([l]) => [...drafts.values()].some((d) => d.includes(l)))
  // (Files: the ones a draft or what was sent holds — a message sent comes back from the history with its own.)
  const files = [...attached.values()].filter((a) => inUse(a.label))
  const keep = { uid: owner, drafts: Object.fromEntries(drafts), files, fn: attachN, n: pasteN }
  // (Too much for it: what was sent goes first, then the pastes; never the files.)
  let out = JSON.stringify({ ...keep, pastes: held, sent })
  if (out.length > STORE_MAX) out = JSON.stringify({ ...keep, pastes: held })
  if (out.length > STORE_MAX) out = JSON.stringify({ ...keep, pastes: [] })
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
