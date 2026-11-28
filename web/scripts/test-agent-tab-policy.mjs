import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { fileURLToPath } from 'node:url'
import ts from 'typescript'

const srcPath = fileURLToPath(new URL('../src/terminal/agentTabPolicy.ts', import.meta.url))
const src = readFileSync(srcPath, 'utf8')
const { outputText } = ts.transpileModule(src, {
  compilerOptions: { module: ts.ModuleKind.ESNext, target: ts.ScriptTarget.ES2022 },
})
const { closeAgentTabs, migrateAgentTabsKey } = await import(
  `data:text/javascript;base64,${Buffer.from(outputText).toString('base64')}`
)

const tabs3 = [
  { id: 't1', key: '1||p1', s: 's1', pane: 'p1' },
  { id: 't2', key: '1||p2', s: 's1', pane: 'p2' },
  { id: 't3', key: '1||p3', s: 's1', pane: 'p3' },
]

// 1. Closing the front tab in the middle switches to the next tab on the right.
{
  const r = closeAgentTabs(tabs3, 't2', '1||p2')
  assert.equal(r.changed, true)
  assert.equal(r.closedFront, true)
  assert.equal(r.front, 't3')
  assert.equal(r.nextTab?.id, 't3')
  assert.deepEqual(
    r.tabs.map((x) => x.id),
    ['t1', 't3'],
  )
}

// 2. Closing the rightmost front tab switches to the neighbor on the left.
{
  const r = closeAgentTabs(tabs3, 't3', '1||p3')
  assert.equal(r.changed, true)
  assert.equal(r.closedFront, true)
  assert.equal(r.front, 't2')
  assert.equal(r.nextTab?.id, 't2')
  assert.deepEqual(
    r.tabs.map((x) => x.id),
    ['t1', 't2'],
  )
}

// 3. Closing the only open tab leaves no front tab (normal empty middle area).
{
  const r = closeAgentTabs([tabs3[0]], 't1', '1||p1')
  assert.equal(r.changed, true)
  assert.equal(r.closedFront, true)
  assert.equal(r.front, null)
  assert.equal(r.nextTab, null)
  assert.deepEqual(r.tabs, [])
}

// 4. Closing a background tab removes it without switching the active tab.
{
  const r = closeAgentTabs(tabs3, 't1', '1||p3')
  assert.equal(r.changed, true)
  assert.equal(r.closedFront, false)
  assert.equal(r.front, 't1')
  assert.equal(r.nextTab, null)
  assert.deepEqual(
    r.tabs.map((x) => x.id),
    ['t1', 't2'],
  )
}

// 5. Closing an agent open only as a split pane keeps the tab open and clears its split.
{
  const withSplit = [
    { id: 't1', key: '1||p1', s: 's1', pane: 'p1', split: '1||p2', focus: 'secondary' },
    { id: 't3', key: '1||p3', s: 's1', pane: 'p3' },
  ]
  const r = closeAgentTabs(withSplit, 't1', '1||p2')
  assert.equal(r.changed, true)
  assert.equal(r.closedFront, false)
  assert.equal(r.front, 't1')
  assert.equal(r.tabs.length, 2)
  assert.equal(r.tabs[0].split, undefined)
  assert.equal(r.tabs[0].focus, undefined)
}

// 6. Closing an agent not present in any tab is a no-op.
{
  const r = closeAgentTabs(tabs3, 't2', '1||p99')
  assert.equal(r.changed, false)
  assert.equal(r.closedFront, false)
  assert.equal(r.front, 't2')
  assert.equal(r.tabs.length, 3)
}

// 7. Migrating a renamed tmux session key updates both primary key and split references.
{
  const tmuxTabs = [
    { id: 't1', key: '7|tmux|old', s: 's1' },
    { id: 't2', key: '7||p1', s: 's2', pane: 'p1', split: '7|tmux|old', focus: 'secondary' },
    { id: 't3', key: '8|tmux|old', s: 's3' },
  ]
  const m = migrateAgentTabsKey(tmuxTabs, '7|tmux|old', '7|tmux|new-name')
  assert.equal(m.changed, true)
  assert.equal(m.tabs[0].key, '7|tmux|new-name')
  assert.equal(m.tabs[1].split, '7|tmux|new-name')
  assert.equal(m.tabs[2].key, '8|tmux|old')
}

console.log('agentTabPolicy: all tests passed')
