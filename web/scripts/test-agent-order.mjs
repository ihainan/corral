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

console.log('agentOrder: all tests passed')
