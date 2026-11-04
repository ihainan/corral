import type { MenuItem } from '../components/ui'
import type { I18nKey } from '../i18n'

// The agents page's list: its rows, their groups and their order (plain,
// for the list, the switcher and Alt+Up/Down alike; tested on its own).

export interface ConvoListRow {
  key: string
  title: string
  where: string // its workspace or session (its host too, where there are several)
  unsent?: number // messages sent to it not seen taken in (see unsentOf)
  now?: string // what it is about now, where known: waits to be allowed, the step under way, how its answer begins
  program?: string // the program in it, for its icon
  status: 'working' | 'blocked' | 'done' | 'idle' | 'unknown'
  bg?: 'running' | 'stale' // (its turn over, subagents still out)
  cmds?: RowCmds // (its turn over: commands it left running in the background)
  since?: number // (unix ms) when its status last changed
  current: boolean
  placeAs?: { status: ConvoListRow['status']; since?: number; cmds?: RowCmds } // (the one shown: where it is placed meanwhile, see asHeld)
  pinned?: number // pinned at the top: its place among the pinned (the order they were pinned)
  onPick: () => void
  menu?: () => (MenuItem | null)[] // what else is done with it, from its row's menu (the side list's)
}

// Commands an agent left running in the background: how many, the first of
// them (what each runs, when it began, unix ms).
export interface RowCmds {
  n: number
  list: { title?: string; since?: number }[]
}

// What is for the person to do: one waiting for an answer, one done and not
// looked at since. Above the rest, those waiting first: working ones are
// not, nothing being for the person there yet (and coming and going often).
// (A turn over with commands it left running in the background — a build, a
// server — counts as at work, as the person asked (11-04): even a server
// that runs on keeps it there, and not "done, not looked at".)
const working = (r: ConvoListRow) => r.status === 'working' || (!!r.cmds && r.status !== 'blocked')
const forYou = (r: ConvoListRow) => (r.status === 'blocked' || r.status === 'done') && !working(r)
// (Those pinned first, whatever their status, latest to change first — as
// every group, 11-04; in no other group.)
export const pinned = (r: ConvoListRow) => r.pinned !== undefined
export const GROUPS: { name: I18nKey; of: (r: ConvoListRow) => boolean }[] = [
  { name: 'convo.group.pinned', of: pinned },
  { name: 'convo.group.todo', of: (r) => !pinned(r) && forYou(r) },
  { name: 'convo.group.working', of: (r) => !pinned(r) && working(r) },
  { name: 'convo.group.recent', of: (r) => !pinned(r) && !forYou(r) && !working(r) },
]
// A group's rows in their order: the latest to change first (those waiting
// before those done, in the first).
export const inGroup = (g: (typeof GROUPS)[number], rows: ConvoListRow[]) => {
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
// (Its commands in the background too, as they place it among those at work — review batch4.)
export const asHeld = (r: ConvoListRow): ConvoListRow => (r.placeAs && r.current ? { ...r, status: r.placeAs.status, since: r.placeAs.since, cmds: r.placeAs.cmds } : r)

// The rest, as the list shows them: by the day their status last changed;
// those of no known time (a tmux session, the gateway restarted) last.
// (today, yesterday: the starts of the two calendar days, a day of a clock
// change being 23 or 25 hours long.)
export const DAYS: { name: I18nKey; of: (since: number, today: number, yesterday: number) => boolean }[] = [
  { name: 'convo.group.today', of: (since, today) => since >= today },
  { name: 'convo.group.yesterday', of: (since, today, yesterday) => since >= yesterday && since < today },
  { name: 'convo.group.earlier', of: (since, _, yesterday) => since > 0 && since < yesterday },
  { name: 'convo.group.other', of: (since) => since <= 0 },
]
// The groups of old ones that are folded when long, and how many of such
// a group are shown unfolded. (Today's and yesterday's are not: they are
// what the person is at, and are short.)
export const FOLDED: I18nKey[] = ['convo.group.earlier', 'convo.group.other', 'convo.group.idle']
export const foldAt = 5

export const dayStart = (now: number) => new Date(now).setHours(0, 0, 0, 0)
export const dayBefore = (start: number) => {
  const d = new Date(start)
  d.setDate(d.getDate() - 1)
  return d.setHours(0, 0, 0, 0) // (a midnight skipped by a clock change: its first hour)
}

// Of the old ones, which group folded when long a row is in (at rest,
// unpinned, since before yesterday or since no known time); else none.
const recent = GROUPS.find((g) => g.name === 'convo.group.recent')!
export const foldedDay = (r: ConvoListRow, today: number, yesterday: number) =>
  recent.of(r) ? DAYS.find((d) => FOLDED.includes(d.name) && d.of(r.since ?? 0, today, yesterday))?.name : undefined
