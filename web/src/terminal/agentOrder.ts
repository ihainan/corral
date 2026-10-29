import type { I18nKey } from '../i18n'

// The agents page's list: its rows, their groups and their order (plain,
// for the list, the switcher and Alt+Up/Down alike; tested on its own).

export interface ConvoListRow {
  key: string
  title: string
  where: string // its workspace or session (its host too, where there are several)
  program?: string // the program in it, for its icon
  status: 'working' | 'blocked' | 'done' | 'idle' | 'unknown'
  bg?: 'running' | 'stale' // (its turn over, subagents still out)
  since?: number // (unix ms) when its status last changed
  current: boolean
  placeAs?: { status: ConvoListRow['status']; since?: number } // (the one shown: where it is placed meanwhile, see asHeld)
  pinned?: number // pinned at the top: its place among the pinned (the order they were pinned)
  onPick: () => void
}

// What is for the person to do: one waiting for an answer, one done and not
// looked at since. Above the rest, those waiting first: working ones are
// not, nothing being for the person there yet (and coming and going often).
const forYou = (r: ConvoListRow) => r.status === 'blocked' || r.status === 'done'
// (Those pinned first, whatever their status, in the order they were pinned;
// in no other group.)
export const pinned = (r: ConvoListRow) => r.pinned !== undefined
export const GROUPS: { name: I18nKey; of: (r: ConvoListRow) => boolean }[] = [
  { name: 'convo.group.pinned', of: pinned },
  { name: 'convo.group.todo', of: (r) => !pinned(r) && forYou(r) },
  { name: 'convo.group.working', of: (r) => !pinned(r) && r.status === 'working' },
  { name: 'convo.group.recent', of: (r) => !pinned(r) && !forYou(r) && r.status !== 'working' },
]
// A group's rows in their order: the latest to change first (those waiting
// before those done, in the first).
export const inGroup = (g: (typeof GROUPS)[number], rows: ConvoListRow[]) => {
  if (g.name === 'convo.group.pinned') return rows.filter(g.of).sort((a, b) => a.pinned! - b.pinned!)
  const of = byRecent(rows.filter(g.of))
  return g.name === 'convo.group.todo' ? [...of.filter((r) => r.status === 'blocked'), ...of.filter((r) => r.status !== 'blocked')] : of
}

// The latest to change first; those of no known time last, as they were.
const byRecent = (rows: ConvoListRow[]) => rows.map((r, i) => [r, i] as const).sort(([a, i], [b, j]) => (b.since ?? 0) - (a.since ?? 0) || i - j).map(([r]) => r)

// The agents in their order on the list: those for the person (waiting,
// then done and not looked at), then working, then the rest, the latest to
// change first in each (those of no known time, as after the gateway
// restarted, last).
export function listOrder(rows: ConvoListRow[]): ConvoListRow[] {
  return GROUPS.flatMap((g) => inGroup(g, rows.map(asHeld)))
}

// The one being read stays where it was when it was gone to, whatever
// becomes of it meanwhile (looked at, a done one is no longer for the
// person; answered, a waiting one works): it goes to its place once another
// is (else it would move from under the pointer as it is read). Placed so
// by the lists and by Alt+Up/Down alike (placeAs, which its page sets);
// what its row says is as it is.
export const asHeld = (r: ConvoListRow): ConvoListRow => (r.placeAs && r.current ? { ...r, status: r.placeAs.status, since: r.placeAs.since } : r)
