import type { MenuItem } from '../components/ui'
import type { I18nKey } from '../i18n'

// The agents page's list: its rows, their groups and their order (plain,
// for the list, the switcher and Alt+Up/Down alike; tested on its own).

export type SidebarGroup = 'status' | 'project' | 'session'
export type SidebarSort = 'recent' | 'smart' | 'name'

export interface OrderOpts {
  group?: SidebarGroup
  sort?: SidebarSort
  pinFixed?: boolean
}

export interface ConvoListRow {
  key: string
  title: string
  where: string // its workspace or session (its host too, where there are several)
  now?: string // what it is about now, where known: waits to be allowed, the step under way, how its answer begins
  program?: string // the program in it, for its icon
  status: 'working' | 'blocked' | 'done' | 'idle' | 'unknown'
  bg?: 'running' | 'stale' // (its turn over, subagents still out)
  cmds?: RowCmds // (its turn over: commands it left running in the background)
  since?: number // (unix ms) when its status last changed
  current: boolean
  placeAs?: { status: ConvoListRow['status']; since?: number; cmds?: RowCmds } // (the one shown: where it is placed meanwhile, see asHeld)
  pinned?: number // pinned at the top: its place among the pinned (the order they were pinned)
  projectKey?: string // normalized working directory path (for "Group by project")
  sessionGroup?: string // host + session label (for "Group by session")
  onPick: () => void
  menu?: () => (MenuItem | null)[] // what else is done with it, from its row's menu (the side list's)
}

// Commands an agent left running in the background: how many, the first of
// them (what each runs, when it began, unix ms).
export interface RowCmds {
  n: number
  list: { task?: string; title?: string; since?: number; out?: string }[]
}

export interface AgentGroup {
  id: string
  label?: string
  labelKey?: I18nKey
  sub?: string
  kind: 'pinned' | 'todo' | 'working' | 'idle' | 'other' | 'project' | 'session'
  rows: ConvoListRow[]
  collapsible?: boolean
  workingCount?: number
  doneCount?: number
}

// What is for the person to do: one waiting for an answer, one done and not
// looked at since. Above the rest, those waiting first: working ones are
// not, nothing being for the person there yet (and coming and going often).
// (A turn over with commands it left running in the background — a build, a
// server — counts as at work, as the person asked (11-04): even a server
// that runs on keeps it there, and not "done, not looked at".)
const working = (r: ConvoListRow) => r.status === 'working' || (!!r.cmds && r.status !== 'blocked')
const forYou = (r: ConvoListRow) => (r.status === 'blocked' || r.status === 'done') && !working(r)
// (Those pinned first, whatever their status; in no other group.)
export const pinned = (r: ConvoListRow) => r.pinned !== undefined
export const GROUPS: { name: I18nKey; of: (r: ConvoListRow) => boolean }[] = [
  { name: 'convo.group.pinned', of: pinned },
  { name: 'convo.group.todo', of: (r) => !pinned(r) && forYou(r) },
  { name: 'convo.group.working', of: (r) => !pinned(r) && working(r) },
  { name: 'convo.group.recent', of: (r) => !pinned(r) && !forYou(r) && !working(r) },
]

// Priority rank for "Smart" sort: waiting for approval first, then actively
// working / background tasks, then finished (unseen), then idle, then unknown.
function smartRank(r: ConvoListRow): number {
  if (r.status === 'blocked') return 0
  if (working(r)) return 1
  if (r.status === 'done') return 2
  if (r.status === 'idle') return 3
  return 4
}

// The latest to change first; those of no known time last, as they were.
const byRecent = (rows: ConvoListRow[]) => rows.map((r, i) => [r, i] as const).sort(([a, i], [b, j]) => (b.since ?? 0) - (a.since ?? 0) || i - j).map(([r]) => r)

const byName = (rows: ConvoListRow[]) =>
  rows
    .map((r, i) => [r, i] as const)
    .sort(([a, i], [b, j]) => a.title.localeCompare(b.title, undefined, { numeric: true, sensitivity: 'base' }) || a.where.localeCompare(b.where, undefined, { numeric: true, sensitivity: 'base' }) || i - j)
    .map(([r]) => r)

const bySmart = (rows: ConvoListRow[]) =>
  rows
    .map((r, i) => [r, i] as const)
    .sort(([a, i], [b, j]) => smartRank(a) - smartRank(b) || (b.since ?? 0) - (a.since ?? 0) || i - j)
    .map(([r]) => r)

const byPinOrder = (rows: ConvoListRow[]) =>
  rows
    .map((r, i) => [r, i] as const)
    .sort(([a, i], [b, j]) => (a.pinned ?? Number.MAX_SAFE_INTEGER) - (b.pinned ?? Number.MAX_SAFE_INTEGER) || i - j)
    .map(([r]) => r)

export function sortRows(rows: ConvoListRow[], sort: SidebarSort = 'recent'): ConvoListRow[] {
  if (sort === 'smart') return bySmart(rows)
  if (sort === 'name') return byName(rows)
  return byRecent(rows)
}

// A group's rows in their order: the latest to change first (those waiting
// before those done, in the first). Kept for status-mode callers and tests.
export const inGroup = (g: (typeof GROUPS)[number], rows: ConvoListRow[], opts?: OrderOpts) => {
  const sort = opts?.sort ?? 'recent'
  const filtered = rows.filter(g.of)
  if (g.name === 'convo.group.pinned' && opts?.pinFixed) return byPinOrder(filtered)
  const of = sortRows(filtered, sort)
  return g.name === 'convo.group.todo' ? [...of.filter((r) => r.status === 'blocked'), ...of.filter((r) => r.status !== 'blocked')] : of
}

// Normalize a working directory path for grouping (trim trailing slashes; collapse home dirs to "~").
export function normalizeProjectPath(cwd?: string): string {
  const s = (cwd ?? '').trim().replace(/\/+$/, '')
  if (!s) return ''
  if (/^(?:\/home\/[^/]+|\/Users\/[^/]+|\/root|~)$/.test(s)) return '~'
  return s
}

// Build short, disambiguated display labels for a set of normalized project paths.
// e.g. ["/work/a/web", "/work/b/web", "/work/corral"] -> {"/work/a/web": "a/web", "/work/b/web": "b/web", "/work/corral": "corral"}.
export function projectLabels(paths: string[]): Map<string, string> {
  const uniq = [...new Set(paths.filter(Boolean))]
  const baseOf = (p: string) => (p === '~' ? '~' : p.slice(p.lastIndexOf('/') + 1) || p)
  const counts = new Map<string, number>()
  for (const p of uniq) {
    const b = baseOf(p)
    counts.set(b, (counts.get(b) ?? 0) + 1)
  }
  const out = new Map<string, string>()
  for (const p of uniq) {
    const b = baseOf(p)
    if ((counts.get(b) ?? 0) <= 1 || p === '~') {
      out.set(p, b)
      continue
    }
    const parts = p.split('/').filter(Boolean)
    out.set(p, parts.length >= 2 ? parts.slice(-2).join('/') : b)
  }
  return out
}

// Build the ordered list of groups for the sidebar / phone list / keyboard switcher.
// In "project" and "session" modes, unpinned blocked agents always surface in a top
// "Waiting for you" group so approval prompts are never hidden inside a collapsed folder.
export function buildAgentGroups(rows: ConvoListRow[], opts: OrderOpts = {}): AgentGroup[] {
  const groupMode = opts.group ?? 'status'
  const sort = opts.sort ?? 'recent'
  const pinFixed = !!opts.pinFixed

  if (groupMode === 'status') {
    const out: AgentGroup[] = []
    for (const g of GROUPS) {
      const of = inGroup(g, rows, opts)
      if (!of.length) continue
      if (g.name === 'convo.group.recent') {
        const idle = of.filter((p) => (p.since ?? 0) > 0)
        const other = of.filter((p) => (p.since ?? 0) <= 0)
        if (idle.length) out.push({ id: 'convo.group.idle', labelKey: 'convo.group.idle', kind: 'idle', rows: idle })
        if (other.length) out.push({ id: 'convo.group.other', labelKey: 'convo.group.other', kind: 'other', rows: other })
      } else {
        const kind = g.name === 'convo.group.pinned' ? 'pinned' : g.name === 'convo.group.todo' ? 'todo' : 'working'
        out.push({ id: g.name, labelKey: g.name, kind, rows: of })
      }
    }
    return out
  }

  const out: AgentGroup[] = []
  const pinnedRows = rows.filter(pinned)
  if (pinnedRows.length) {
    out.push({
      id: 'convo.group.pinned',
      labelKey: 'convo.group.pinned',
      kind: 'pinned',
      rows: pinFixed ? byPinOrder(pinnedRows) : sortRows(pinnedRows, sort),
    })
  }

  const unpinned = rows.filter((r) => !pinned(r))
  // Keep unpinned blocked agents visible at the top regardless of project/session folding.
  const blockedRows = unpinned.filter((r) => r.status === 'blocked')
  if (blockedRows.length) {
    out.push({
      id: 'convo.group.todo',
      labelKey: 'convo.group.todo',
      kind: 'todo',
      rows: sortRows(blockedRows, sort),
    })
  }

  const rest = unpinned.filter((r) => r.status !== 'blocked')
  if (!rest.length) return out

  const isProject = groupMode === 'project'
  const labels = isProject ? projectLabels(rest.map((r) => r.projectKey ?? '')) : null
  const buckets = new Map<string, { label?: string; labelKey?: I18nKey; sub?: string; rows: ConvoListRow[]; isFallback: boolean }>()

  for (const r of rest) {
    const rawKey = isProject ? (r.projectKey ?? '') : (r.sessionGroup ?? '')
    const isFallback = !rawKey
    const mapKey = isFallback ? '__other__' : rawKey
    let b = buckets.get(mapKey)
    if (!b) {
      b = isFallback
        ? { labelKey: 'convo.group.other', rows: [], isFallback: true }
        : isProject
          ? { label: labels?.get(rawKey) ?? rawKey, sub: rawKey === '~' ? undefined : rawKey, rows: [], isFallback: false }
          : { label: rawKey, rows: [], isFallback: false }
      buckets.set(mapKey, b)
    }
    b.rows.push(r)
  }

  const entries = [...buckets.entries()].map(([k, b], idx) => {
    const sorted = sortRows(b.rows, sort)
    let maxSince = 0
    let bestRank = 99
    let workingCount = 0
    let doneCount = 0
    for (const r of sorted) {
      if ((r.since ?? 0) > maxSince) maxSince = r.since ?? 0
      const rank = smartRank(r)
      if (rank < bestRank) bestRank = rank
      if (working(r)) workingCount++
      else if (r.status === 'done') doneCount++
    }
    return {
      k,
      idx,
      maxSince,
      bestRank,
      group: {
        id: (isProject ? 'proj:' : 'sess:') + k,
        label: b.label,
        labelKey: b.labelKey,
        sub: b.sub,
        kind: (b.isFallback ? 'other' : isProject ? 'project' : 'session') as AgentGroup['kind'],
        rows: sorted,
        collapsible: true,
        workingCount,
        doneCount,
      },
      isFallback: b.isFallback,
    }
  })

  entries.sort((a, b) => {
    if (a.isFallback !== b.isFallback) return a.isFallback ? 1 : -1
    if (sort === 'name') {
      const la = a.group.label ?? ''
      const lb = b.group.label ?? ''
      return la.localeCompare(lb, undefined, { numeric: true, sensitivity: 'base' }) || a.idx - b.idx
    }
    if (sort === 'smart') {
      return a.bestRank - b.bestRank || b.maxSince - a.maxSince || a.idx - b.idx
    }
    return b.maxSince - a.maxSince || a.idx - b.idx
  })

  for (const e of entries) out.push(e.group)
  return out
}

// The agents in their order on the list (for the list, the switcher and Alt+Up/Down alike).
export function listOrder(rows: ConvoListRow[], opts?: OrderOpts): ConvoListRow[] {
  return buildAgentGroups(rows.map(asHeld), opts).flatMap((g) => g.rows)
}

// Reorder a pinned agent among the currently visible pinned keys while preserving
// any offline/away pinned keys in place.
export function reorderPinned(pins: string[], key: string, visibleKeys: Set<string>, dir: 'top' | 'up' | 'down'): string[] | null {
  const visible = pins.filter((k) => visibleKeys.has(k))
  const pos = visible.indexOf(key)
  if (pos < 0 || visible.length < 2) return null
  let targetPos = pos
  if (dir === 'top') targetPos = 0
  else if (dir === 'up') targetPos = pos - 1
  else if (dir === 'down') targetPos = pos + 1
  if (targetPos < 0 || targetPos >= visible.length || targetPos === pos) return null

  const nextVisible = [...visible]
  nextVisible.splice(pos, 1)
  nextVisible.splice(targetPos, 0, key)

  // Splice the reordered visible keys back into the slots they occupied in pins.
  let vi = 0
  return pins.map((k) => (visibleKeys.has(k) ? nextVisible[vi++] : k))
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
