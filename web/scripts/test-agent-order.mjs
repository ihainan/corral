import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { fileURLToPath } from 'node:url'
import ts from 'typescript'

const srcPath = fileURLToPath(new URL('../src/terminal/agentOrder.ts', import.meta.url))
const src = readFileSync(srcPath, 'utf8')
const { outputText } = ts.transpileModule(src, {
  compilerOptions: { module: ts.ModuleKind.ESNext, target: ts.ScriptTarget.ES2022 },
})
const mod = await import(`data:text/javascript;base64,${Buffer.from(outputText).toString('base64')}`)
const { buildAgentGroups, listOrder, normalizeProjectPath, projectLabels, reorderPinned } = mod

function row(partial) {
  return {
    where: '',
    status: 'idle',
    current: false,
    onPick: () => {},
    ...partial,
  }
}

// 1. Path normalization & disambiguation
assert.equal(normalizeProjectPath('/home/ihainan/'), '~')
assert.equal(normalizeProjectPath('/Users/alice'), '~')
assert.equal(normalizeProjectPath('/work/corral/'), '/work/corral')
assert.equal(normalizeProjectPath(''), '')

const labels = projectLabels(['/work/a/web', '/work/b/web', '/work/corral', '~'])
assert.equal(labels.get('/work/a/web'), 'a/web')
assert.equal(labels.get('/work/b/web'), 'b/web')
assert.equal(labels.get('/work/corral'), 'corral')
assert.equal(labels.get('~'), '~')

// 2. Pinned order: fixed vs recent/smart/name
{
  const rows = [
    row({ key: 'p1', title: 'Beta', pinned: 0, status: 'idle', since: 100 }),
    row({ key: 'p2', title: 'Alpha', pinned: 1, status: 'blocked', since: 50 }),
    row({ key: 'p3', title: 'Gamma', pinned: 2, status: 'idle', since: 300 }),
  ]
  assert.deepEqual(listOrder(rows, { group: 'status', sort: 'recent', pinFixed: false }).map((r) => r.key), ['p3', 'p1', 'p2'])
  assert.deepEqual(listOrder(rows, { group: 'status', sort: 'smart', pinFixed: false }).map((r) => r.key), ['p2', 'p3', 'p1'])
  assert.deepEqual(listOrder(rows, { group: 'status', sort: 'name', pinFixed: false }).map((r) => r.key), ['p2', 'p1', 'p3'])
  assert.deepEqual(listOrder(rows, { group: 'status', sort: 'recent', pinFixed: true }).map((r) => r.key), ['p1', 'p2', 'p3'])
  assert.deepEqual(listOrder(rows, { group: 'status', sort: 'smart', pinFixed: true }).map((r) => r.key), ['p1', 'p2', 'p3'])
}

// 3. Project grouping + group-level sorting + top blocked safety
{
  const rows = [
    row({ key: 'pin', title: 'Pinned', pinned: 0, status: 'idle', since: 10, projectKey: '/p/alpha' }),
    row({ key: 'wait', title: 'Needs Approval', status: 'blocked', since: 20, projectKey: '/p/zebra' }),
    row({ key: 'w1', title: 'Z-Work', status: 'working', since: 100, projectKey: '/p/alpha' }),
    row({ key: 'i1', title: 'A-Idle', status: 'idle', since: 500, projectKey: '/p/beta' }),
    row({ key: 'i2', title: 'B-Idle', status: 'idle', since: 200, projectKey: '/p/alpha' }),
    row({ key: 'none', title: 'No CWD', status: 'idle', since: 900 }),
  ]

  const byRecent = buildAgentGroups(rows, { group: 'project', sort: 'recent' })
  assert.deepEqual(byRecent.map((g) => g.label ?? g.labelKey), ['convo.group.pinned', 'convo.group.todo', 'beta', 'alpha', 'convo.group.other'])
  assert.equal(byRecent[0].collapsible, true)
  assert.equal(byRecent[0].tree, undefined)
  assert.equal(byRecent[1].collapsible, false)
  assert.equal(byRecent[2].collapsible, true)
  assert.equal(byRecent[2].tree, true)
  assert.equal(byRecent.find((g) => g.label === 'alpha').workingCount, 1)
  assert.deepEqual(byRecent.find((g) => g.label === 'alpha').rows.map((r) => r.key), ['i2', 'w1'])

  const bySmart = buildAgentGroups(rows, { group: 'project', sort: 'smart' })
  assert.deepEqual(bySmart.map((g) => g.label ?? g.labelKey), ['convo.group.pinned', 'convo.group.todo', 'alpha', 'beta', 'convo.group.other'])
  assert.deepEqual(bySmart.find((g) => g.label === 'alpha').rows.map((r) => r.key), ['w1', 'i2'])

  const byName = buildAgentGroups(rows, { group: 'project', sort: 'name' })
  assert.deepEqual(byName.map((g) => g.label ?? g.labelKey), ['convo.group.pinned', 'convo.group.todo', 'alpha', 'beta', 'convo.group.other'])
  assert.deepEqual(byName.find((g) => g.label === 'alpha').rows.map((r) => r.key), ['i2', 'w1'])
}

// 4. Host & session grouping
{
  const rows = [
    row({ key: 'a', title: 'A', status: 'idle', since: 100, sessionGroup: '101 · herdr · 1' }),
    row({ key: 'b', title: 'B', status: 'idle', since: 300, sessionGroup: '101 · herdr · 3' }),
    row({ key: 'c', title: 'C', status: 'idle', since: 200, sessionGroup: '101 · herdr · 1' }),
  ]
  const recent = buildAgentGroups(rows, { group: 'session', sort: 'recent' })
  assert.deepEqual(recent.map((g) => g.label), ['101 · herdr · 3', '101 · herdr · 1'])

  const byName = buildAgentGroups(rows, { group: 'session', sort: 'name' })
  assert.deepEqual(byName.map((g) => g.label), ['101 · herdr · 1', '101 · herdr · 3'])
  assert.deepEqual(listOrder(rows, { group: 'session', sort: 'name' }).map((r) => r.key), ['a', 'c', 'b'])
}

// 5. Pinned manual reorder preserving offline pins
{
  const pins = ['offline-1', 'a', 'offline-2', 'b', 'c']
  const visible = new Set(['a', 'b', 'c'])
  assert.deepEqual(reorderPinned(pins, 'c', visible, 'top'), ['offline-1', 'c', 'offline-2', 'a', 'b'])
  assert.deepEqual(reorderPinned(pins, 'b', visible, 'up'), ['offline-1', 'b', 'offline-2', 'a', 'c'])
  assert.deepEqual(reorderPinned(pins, 'a', visible, 'down'), ['offline-1', 'b', 'offline-2', 'a', 'c'])
  assert.equal(reorderPinned(pins, 'a', visible, 'up'), null)
  assert.equal(reorderPinned(pins, 'c', visible, 'down'), null)
}

// 6. Parent-child lineage nesting in Status mode + depth-1 cap + blocked safety + opt-out
{
  const rows = [
    row({ key: 'p', title: 'Orchestrator', status: 'idle', since: 100 }),
    row({ key: 'c1', title: 'Worker 1', status: 'working', since: 300, parentKey: 'p' }),
    row({ key: 'c2', title: 'Grandchild', status: 'idle', since: 250, parentKey: 'c1' }),
    row({ key: 'c3', title: 'Needs Approval Child', status: 'blocked', since: 400, parentKey: 'p' }),
    row({ key: 'solo', title: 'Solo Idle', status: 'idle', since: 200 }),
  ]

  const nested = buildAgentGroups(rows, { group: 'status', sort: 'recent', nestChildren: true })
  // c3 (blocked) MUST rise to uncollapsible todo with crossParentTitle; p + c1 + c2 move together into working!
  assert.deepEqual(nested.map((g) => g.kind), ['todo', 'working', 'idle'])
  assert.equal(nested[0].collapsible, false)
  assert.deepEqual(nested[0].rows.map((r) => r.key), ['c3'])
  assert.equal(nested[0].rows[0].crossParentTitle, 'Orchestrator')
  assert.deepEqual(nested[1].rows.map((r) => r.key), ['p', 'c1', 'c2'])
  assert.equal(nested[1].rows[0].nestDepth, 0)
  assert.deepEqual(nested[1].rows[0].childKeys, ['c1', 'c2'])
  assert.equal(nested[1].rows[1].nestDepth, 1)
  assert.equal(nested[1].rows[1].nestedParentKey, 'p')
  assert.equal(nested[1].rows[2].nestDepth, 1)
  assert.equal(nested[1].rows[2].viaParentTitle, 'Worker 1')

  // Collapsed parent hides non-current children in listOrder, but keeps current child visible.
  const withCurrentKid = rows.map((r) => (r.key === 'c2' ? { ...r, current: true } : r))
  assert.deepEqual(
    listOrder(withCurrentKid, { group: 'status', sort: 'recent', nestChildren: true, collapsedParents: ['p'] }).map((r) => r.key),
    ['c3', 'p', 'c2', 'solo'],
  )

  // Opt-out (nestChildren: false) restores completely flat grouping.
  const flat = buildAgentGroups(rows, { group: 'status', sort: 'recent', nestChildren: false })
  assert.deepEqual(flat.map((g) => g.kind), ['todo', 'working', 'idle'])
  assert.deepEqual(flat[1].rows.map((r) => r.key), ['c1'])
  assert.deepEqual(flat[2].rows.map((r) => r.key), ['c2', 'solo', 'p'])
}

// 7. Cross-bucket edge cutting BEFORE forest flattening in Project mode
{
  // A(proj X) -> B(proj Y) -> C(proj Y): edge A->B is cut first so C nests cleanly under B in proj Y!
  const rows = [
    row({ key: 'a', title: 'Root X', status: 'idle', since: 100, projectKey: '/repo/x' }),
    row({ key: 'b', title: 'Child Y', status: 'idle', since: 200, projectKey: '/repo/y', parentKey: 'a' }),
    row({ key: 'c', title: 'Grandchild Y', status: 'idle', since: 300, projectKey: '/repo/y', parentKey: 'b' }),
  ]
  const groups = buildAgentGroups(rows, { group: 'project', sort: 'recent', nestChildren: true })
  const grpY = groups.find((g) => g.label === 'y')
  assert.ok(grpY)
  assert.deepEqual(grpY.rows.map((r) => r.key), ['b', 'c'])
  assert.equal(grpY.rows[0].nestDepth, 0)
  assert.equal(grpY.rows[0].crossParentTitle, 'Root X')
  assert.deepEqual(grpY.rows[0].childKeys, ['c'])
  assert.equal(grpY.rows[1].nestDepth, 1)
  assert.equal(grpY.rows[1].nestedParentKey, 'b')
  assert.equal(grpY.rows[1].viaParentTitle, undefined)
}

// 8. Real-time liveStatus overrides frozen placeAs for blocked approval safety
{
  const rows = [
    row({
      key: 'cur',
      title: 'Current Now Blocked',
      status: 'blocked',
      current: true,
      placeAs: { status: 'idle', since: 100 },
      since: 500,
    }),
    row({ key: 'other', title: 'Other Idle', status: 'idle', since: 200 }),
  ]
  const ordered = listOrder(rows, { group: 'project', sort: 'recent', nestChildren: true })
  assert.equal(ordered[0].key, 'cur')
  const groups = buildAgentGroups(rows.map(mod.asHeld), { group: 'project', sort: 'recent', nestChildren: true })
  assert.equal(groups[0].kind, 'todo')
  assert.equal(groups[0].collapsible, false)
  assert.equal(groups[0].rows[0].key, 'cur')
}

// 9. Missing parent promotes child cleanly; cycles break cleanly without infinite loops
{
  const rows = [
    row({ key: 'orphan', title: 'Orphan', status: 'idle', since: 100, parentKey: 'missing' }),
    row({ key: 'cyc1', title: 'Cyc 1', status: 'idle', since: 200, parentKey: 'cyc2' }),
    row({ key: 'cyc2', title: 'Cyc 2', status: 'idle', since: 150, parentKey: 'cyc1' }),
  ]
  const groups = buildAgentGroups(rows, { group: 'status', sort: 'recent', nestChildren: true })
  const keys = groups.flatMap((g) => g.rows.map((r) => r.key))
  assert.deepEqual(keys.sort(), ['cyc1', 'cyc2', 'orphan'])
}

console.log('agentOrder: all tests passed')
