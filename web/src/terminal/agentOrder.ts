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
  nestChildren?: boolean
  // Optional collapsed parent keys: used by listOrder so Alt+Up/Down follows
  // the exact visible projection (keeping current and live-blocked rows visible).
  collapsedParents?: Set<string> | string[]
}

export interface ConvoListRow {
  key: string
  title: string
  where: string // its workspace or session (its host too, where there are several)
  now?: string // what it is about now, where known: waits to be allowed, the step under way, how its answer begins
  program?: string // the program in it, for its icon
  status: 'working' | 'blocked' | 'done' | 'idle' | 'unknown'
  // Real-time status distinct from frozen placeAs.status so approval alerts
  // ("blocked") always surface immediately in the uncollapsible attention group.
  liveStatus?: 'working' | 'blocked' | 'done' | 'idle' | 'unknown'
  bg?: 'running' | 'stale' // (its turn over, subagents still out)
  cmds?: RowCmds // (its turn over: commands it left running in the background)
  since?: number // (unix ms) when its status last changed
  current: boolean
  placeAs?: { status: ConvoListRow['status']; since?: number; cmds?: RowCmds } // (the one shown: where it is placed meanwhile, see asHeld)
  pinned?: number // pinned at the top: its place among the pinned (the order they were pinned)
  projectKey?: string // normalized working directory path (for "Group by project")
  sessionGroup?: string // host + session label (for "Group by session")
  parentKey?: string // agentKey of confirmed herdr parent within the same server
  lineageSource?: string
  onPick: () => void
  menu?: () => (MenuItem | null)[] // what else is done with it, from its row's menu (the side list's)

  // Projection metadata populated by buildAgentGroups on shallow row copies:
  nestDepth?: 0 | 1
  nestedParentKey?: string
  viaParentTitle?: string
  crossParentTitle?: string
  childKeys?: string[]
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
  tree?: boolean
  blockedCount?: number
  workingCount?: number
  doneCount?: number
}

// Live blocked check: always evaluates real-time status (never frozen placeAs).
export const isLiveBlocked = (r: ConvoListRow) => (r.liveStatus ?? r.status) === 'blocked'

function groupCounts(rows: ConvoListRow[]) {
  let blockedCount = 0
  let workingCount = 0
  let doneCount = 0
  for (const r of rows) {
    if (isLiveBlocked(r)) blockedCount++
    else if (working(r)) workingCount++
    else if (r.status === 'done') doneCount++
  }
  return { blockedCount, workingCount, doneCount }
}

// What is for the person to do: one waiting for an answer, one done and not
// looked at since. Above the rest, those waiting first: working ones are
// not, nothing being for the person there yet (and coming and going often).
// (A turn over with commands it left running in the background — a build, a
// server — counts as at work, as the person asked (11-04): even a server
// that runs on keeps it there, and not "done, not looked at".)
const working = (r: ConvoListRow) => !isLiveBlocked(r) && (r.status === 'working' || !!r.cmds)
const forYou = (r: ConvoListRow) => isLiveBlocked(r) || (r.status === 'done' && !working(r))
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
  if (isLiveBlocked(r)) return 0
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
  return g.name === 'convo.group.todo' ? [...of.filter(isLiveBlocked), ...of.filter((r) => !isLiveBlocked(r))] : of
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

// Flatten a bucket's eligible parent-child edges to visual depth 1.
// Non-nestable edges (missing parent in this bucket, self-loops, cycles) are
// already cut because only edges whose parent exists inside `bucketRows` are
// kept before tree flattening (e.g. A(proj X) -> B(proj Y) -> C(proj Y) cuts
// A->B so C nests cleanly under B inside proj Y, while B carries a subtle
// `crossParentTitle: A.title` hint).
interface NestedForest {
  rows: ConvoListRow[]
  trees: { root: ConvoListRow; children: ConvoListRow[]; maxSince: number; bestRank: number }[]
}

function projectNestedBucket(
  bucketRows: ConvoListRow[],
  allByKey: Map<string, ConvoListRow>,
  sort: SidebarSort,
  nestChildren: boolean,
): NestedForest {
  if (!nestChildren || !bucketRows.length) {
    const sorted = sortRows(bucketRows, sort).map((r) => stripNestMeta(r, allByKey, false))
    return {
      rows: sorted,
      trees: sorted.map((r) => ({ root: r, children: [], maxSince: r.since ?? 0, bestRank: smartRank(r) })),
    }
  }

  const inBucket = new Map<string, ConvoListRow>()
  for (const r of bucketRows) inBucket.set(r.key, r)

  // Step 1: Build clean parent pointers ONLY for edges strictly inside this bucket.
  const localParent = new Map<string, string>()
  for (const r of bucketRows) {
    if (r.parentKey && r.parentKey !== r.key && inBucket.has(r.parentKey)) {
      localParent.set(r.key, r.parentKey)
    }
  }

  // Step 2: Resolve each node to its cycle-free root inside this bucket.
  const rootOf = new Map<string, string>()
  const directParentOf = new Map<string, string>()
  for (const r of bucketRows) {
    let cur = r.key
    const seen = new Set<string>([cur])
    let root = cur
    let steps = 0
    while (localParent.has(cur) && steps < 16) {
      const next = localParent.get(cur)!
      if (seen.has(next)) {
        // Cycle: break by treating r as a root.
        root = r.key
        break
      }
      seen.add(next)
      root = next
      cur = next
      steps++
    }
    if (root !== r.key) {
      rootOf.set(r.key, root)
      directParentOf.set(r.key, localParent.get(r.key)!)
    }
  }

  const childrenByRoot = new Map<string, ConvoListRow[]>()
  const roots: ConvoListRow[] = []
  for (const r of bucketRows) {
    const root = rootOf.get(r.key)
    if (root) {
      const list = childrenByRoot.get(root) ?? []
      list.push(r)
      childrenByRoot.set(root, list)
    } else {
      roots.push(r)
    }
  }

  // Build tree entries with aggregated sort keys (so an idle parent with an active
  // child stays ordered near recent/smart activity).
  const trees = roots.map((root, idx) => {
    const rawKids = sortRows(childrenByRoot.get(root.key) ?? [], sort)
    const kids = rawKids.map((kid): ConvoListRow => {
      const directKey = directParentOf.get(kid.key)
      const viaParentTitle = directKey && directKey !== root.key ? inBucket.get(directKey)?.title : undefined
      return {
        ...kid,
        nestDepth: 1,
        nestedParentKey: root.key,
        viaParentTitle,
        crossParentTitle: undefined,
        childKeys: undefined,
      }
    })
    const crossParent = root.parentKey && !localParent.has(root.key) ? allByKey.get(root.parentKey)?.title : undefined
    const rootProj: ConvoListRow = {
      ...root,
      nestDepth: 0,
      nestedParentKey: undefined,
      viaParentTitle: undefined,
      crossParentTitle: crossParent,
      childKeys: kids.length ? kids.map((k) => k.key) : undefined,
    }
    let maxSince = rootProj.since ?? 0
    let bestRank = smartRank(rootProj)
    for (const k of kids) {
      if ((k.since ?? 0) > maxSince) maxSince = k.since ?? 0
      const rank = smartRank(k)
      if (rank < bestRank) bestRank = rank
    }
    return { root: rootProj, children: kids, maxSince, bestRank, idx }
  })

  trees.sort((a, b) => {
    if (sort === 'name') {
      return (
        a.root.title.localeCompare(b.root.title, undefined, { numeric: true, sensitivity: 'base' }) ||
        a.root.where.localeCompare(b.root.where, undefined, { numeric: true, sensitivity: 'base' }) ||
        a.idx - b.idx
      )
    }
    if (sort === 'smart') {
      return a.bestRank - b.bestRank || b.maxSince - a.maxSince || a.idx - b.idx
    }
    return b.maxSince - a.maxSince || a.idx - b.idx
  })

  const flat: ConvoListRow[] = []
  for (const t of trees) {
    flat.push(t.root, ...t.children)
  }
  return { rows: flat, trees }
}

function stripNestMeta(r: ConvoListRow, allByKey: Map<string, ConvoListRow>, showCross: boolean): ConvoListRow {
  const crossParentTitle = showCross && r.parentKey ? allByKey.get(r.parentKey)?.title : undefined
  if (
    r.nestDepth === undefined &&
    r.nestedParentKey === undefined &&
    r.viaParentTitle === undefined &&
    r.crossParentTitle === crossParentTitle &&
    r.childKeys === undefined
  ) {
    return r
  }
  return {
    ...r,
    nestDepth: undefined,
    nestedParentKey: undefined,
    viaParentTitle: undefined,
    crossParentTitle,
    childKeys: undefined,
  }
}

// Build the ordered list of groups for the sidebar / phone list / keyboard switcher.
// Real-time blocked agents (liveStatus ?? status === 'blocked') always surface in
// an uncollapsible top "Needs you" group (or stay unhidden inside Pinned), never
// buried inside collapsed parents or folders.
export function buildAgentGroups(rows: ConvoListRow[], opts: OrderOpts = {}): AgentGroup[] {
  const groupMode = opts.group ?? 'status'
  const sort = opts.sort ?? 'recent'
  const pinFixed = !!opts.pinFixed
  const nestChildren = opts.nestChildren ?? true

  const allByKey = new Map<string, ConvoListRow>()
  for (const r of rows) allByKey.set(r.key, r)

  const out: AgentGroup[] = []

  // 1. Pinned group (independent manual order; blocked pinned rows are protected in UI).
  // When nestChildren is enabled, non-blocked unpinned descendants of a pinned parent
  // nest directly underneath their nearest pinned ancestor inside Pinned so orchestrators
  // pinned at the top actually display their subagents underneath them.
  const pinnedRows = rows.filter(pinned)
  const pinnedKeys = new Set(pinnedRows.map((r) => r.key))
  const unpinned = rows.filter((r) => !pinned(r))

  // 2. Unpinned live-blocked agents ALWAYS surface at top of uncollapsible "Needs you"
  //    (never nested under a parent, even if the parent is pinned or collapsed).
  const blockedRows = sortRows(unpinned.filter(isLiveBlocked), sort).map((r) => stripNestMeta(r, allByKey, nestChildren))
  const allNonBlocked = unpinned.filter((r) => !isLiveBlocked(r))

  const pinnedChildKeys = new Set<string>()
  const pinnedChildrenByRoot = new Map<string, ConvoListRow[]>()
  if (nestChildren && pinnedRows.length && allNonBlocked.length) {
    const nonBlockedByKey = new Map<string, ConvoListRow>()
    for (const r of allNonBlocked) nonBlockedByKey.set(r.key, r)

    for (const r of allNonBlocked) {
      let curKey = r.parentKey
      const seen = new Set<string>([r.key])
      let steps = 0
      let pinnedAnc: string | undefined
      while (curKey && steps < 16) {
        if (seen.has(curKey)) break
        seen.add(curKey)
        if (pinnedKeys.has(curKey)) {
          pinnedAnc = curKey
          break
        }
        const up = nonBlockedByKey.get(curKey)
        if (!up) break
        curKey = up.parentKey
        steps++
      }
      if (pinnedAnc) {
        pinnedChildKeys.add(r.key)
        const list = pinnedChildrenByRoot.get(pinnedAnc) ?? []
        list.push(r)
        pinnedChildrenByRoot.set(pinnedAnc, list)
      }
    }
  }

  if (pinnedRows.length) {
    const pBase = pinFixed ? byPinOrder(pinnedRows) : sortRows(pinnedRows, sort)
    const pProjected: ConvoListRow[] = []
    for (const p of pBase) {
      const rawKids = nestChildren ? sortRows(pinnedChildrenByRoot.get(p.key) ?? [], sort) : []
      const kids = rawKids.map((kid): ConvoListRow => ({
        ...kid,
        nestDepth: 1,
        nestedParentKey: p.key,
        viaParentTitle: kid.parentKey && kid.parentKey !== p.key ? allByKey.get(kid.parentKey)?.title : undefined,
        crossParentTitle: undefined,
        childKeys: undefined,
      }))
      const crossParent = nestChildren && p.parentKey ? allByKey.get(p.parentKey)?.title : undefined
      pProjected.push({
        ...p,
        nestDepth: kids.length ? 0 : undefined,
        nestedParentKey: undefined,
        viaParentTitle: undefined,
        crossParentTitle: crossParent,
        childKeys: kids.length ? kids.map((k) => k.key) : undefined,
      })
      for (const k of kids) pProjected.push(k)
    }
    out.push({
      id: 'convo.group.pinned',
      labelKey: 'convo.group.pinned',
      kind: 'pinned',
      rows: pProjected,
      collapsible: true,
      ...groupCounts(pProjected),
    })
  }

  const nonBlocked = pinnedChildKeys.size ? allNonBlocked.filter((r) => !pinnedChildKeys.has(r.key)) : allNonBlocked

  if (groupMode === 'status') {
    if (!nestChildren) {
      // Exact flat Status-mode behavior.
      const doneRows = sortRows(nonBlocked.filter((r) => r.status === 'done' && !working(r)), sort).map((r) => stripNestMeta(r, allByKey, false))
      const todoRows = [...blockedRows, ...doneRows]
      if (todoRows.length) {
        out.push({ id: 'convo.group.todo', labelKey: 'convo.group.todo', kind: 'todo', rows: todoRows, collapsible: false, ...groupCounts(todoRows) })
      }
      const workRows = sortRows(nonBlocked.filter(working), sort).map((r) => stripNestMeta(r, allByKey, false))
      if (workRows.length) {
        out.push({ id: 'convo.group.working', labelKey: 'convo.group.working', kind: 'working', rows: workRows, collapsible: true, ...groupCounts(workRows) })
      }
      const recentRows = sortRows(nonBlocked.filter((r) => !forYou(r) && !working(r)), sort).map((r) => stripNestMeta(r, allByKey, false))
      const idle = recentRows.filter((p) => (p.since ?? 0) > 0)
      const other = recentRows.filter((p) => (p.since ?? 0) <= 0)
      if (idle.length) out.push({ id: 'convo.group.idle', labelKey: 'convo.group.idle', kind: 'idle', rows: idle, collapsible: true, ...groupCounts(idle) })
      if (other.length) out.push({ id: 'convo.group.other', labelKey: 'convo.group.other', kind: 'other', rows: other, collapsible: true, ...groupCounts(other) })
      return out
    }

    // Nested Status mode: build parent-child trees among unpinned non-blocked rows,
    // placing each tree into the highest active status group of its members so an
    // idle parent with a working/done child stays prominently visible alongside its child.
    const forest = projectNestedBucket(nonBlocked, allByKey, sort, true)
    const doneTrees: ConvoListRow[] = []
    const workTrees: ConvoListRow[] = []
    const idleTrees: ConvoListRow[] = []
    const otherTrees: ConvoListRow[] = []

    for (const t of forest.trees) {
      const members = [t.root, ...t.children]
      const hasDone = members.some((m) => m.status === 'done' && !working(m))
      const hasWorking = members.some(working)
      if (hasDone) doneTrees.push(...members)
      else if (hasWorking) workTrees.push(...members)
      else if (t.maxSince > 0) idleTrees.push(...members)
      else otherTrees.push(...members)
    }

    const todoRows = [...blockedRows, ...doneTrees]
    if (todoRows.length) {
      out.push({ id: 'convo.group.todo', labelKey: 'convo.group.todo', kind: 'todo', rows: todoRows, collapsible: false, ...groupCounts(todoRows) })
    }
    if (workTrees.length) {
      out.push({ id: 'convo.group.working', labelKey: 'convo.group.working', kind: 'working', rows: workTrees, collapsible: true, ...groupCounts(workTrees) })
    }
    if (idleTrees.length) {
      out.push({ id: 'convo.group.idle', labelKey: 'convo.group.idle', kind: 'idle', rows: idleTrees, collapsible: true, ...groupCounts(idleTrees) })
    }
    if (otherTrees.length) {
      out.push({ id: 'convo.group.other', labelKey: 'convo.group.other', kind: 'other', rows: otherTrees, collapsible: true, ...groupCounts(otherTrees) })
    }
    return out
  }

  // Project / Session modes:
  if (blockedRows.length) {
    out.push({
      id: 'convo.group.todo',
      labelKey: 'convo.group.todo',
      kind: 'todo',
      rows: blockedRows,
      collapsible: false,
      ...groupCounts(blockedRows),
    })
  }

  if (!nonBlocked.length) return out

  const isProject = groupMode === 'project'
  const labels = isProject ? projectLabels(nonBlocked.map((r) => r.projectKey ?? '')) : null
  const buckets = new Map<string, { label?: string; labelKey?: I18nKey; sub?: string; rows: ConvoListRow[]; isFallback: boolean }>()

  for (const r of nonBlocked) {
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

  // Cut cross-bucket edges BEFORE flattening each bucket's forest!
  const entries = [...buckets.entries()].map(([k, b], idx) => {
    const forest = projectNestedBucket(b.rows, allByKey, sort, nestChildren)
    const sorted = forest.rows
    let maxSince = 0
    let bestRank = 99
    for (const r of sorted) {
      if ((r.since ?? 0) > maxSince) maxSince = r.since ?? 0
      const rank = smartRank(r)
      if (rank < bestRank) bestRank = rank
    }
    const counts = groupCounts(sorted)
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
        tree: !b.isFallback,
        ...counts,
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

// The agents in their visible order on the list (for the list, the switcher and
// Alt+Up/Down alike), deduplicated by key and respecting collapsedParents while
// always keeping current and live-blocked rows reachable.
export function listOrder(rows: ConvoListRow[], opts?: OrderOpts): ConvoListRow[] {
  const collapsed = opts?.collapsedParents
    ? opts.collapsedParents instanceof Set
      ? opts.collapsedParents
      : new Set(opts.collapsedParents)
    : null
  const seen = new Set<string>()
  const out: ConvoListRow[] = []
  for (const g of buildAgentGroups(rows.map(asHeld), opts)) {
    for (const r of g.rows) {
      if (seen.has(r.key)) continue
      if (collapsed && r.nestDepth === 1 && r.nestedParentKey && collapsed.has(r.nestedParentKey) && !r.current && !isLiveBlocked(r)) {
        continue
      }
      seen.add(r.key)
      out.push(r)
    }
  }
  return out
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
// liveStatus preserves the real-time status so a live "blocked" transition
// is never suppressed by a frozen placeAs.
export const asHeld = (r: ConvoListRow): ConvoListRow =>
  r.placeAs && r.current
    ? { ...r, liveStatus: r.liveStatus ?? r.status, status: r.placeAs.status, since: r.placeAs.since, cmds: r.placeAs.cmds }
    : r

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
