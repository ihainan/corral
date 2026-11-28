import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { fileURLToPath } from 'node:url'
import ts from 'typescript'

const srcPath = fileURLToPath(new URL('../src/terminal/agentTabPolicy.ts', import.meta.url))
const src = readFileSync(srcPath, 'utf8')
const { outputText } = ts.transpileModule(src, {
  compilerOptions: { module: ts.ModuleKind.ESNext, target: ts.ScriptTarget.ES2022 },
})
const {
  closeAgentTabs,
  expandSplitAgentTab,
  migrateAgentTabsKey,
  moveAgentTab,
  normalizeAgentTabs,
  openAgentTab,
  pinAgentTab,
  pinAgentTabByKey,
} = await import(`data:text/javascript;base64,${Buffer.from(outputText).toString('base64')}`)

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

// 8. Preview mode: single-click when no tabs open creates a preview tab.
{
  let seq = 0
  const makeId = () => `new-${++seq}`
  const r = openAgentTab([], null, { key: '1||p1', s: 's1', pane: 'p1' }, 'preview', makeId)
  assert.equal(r.changed, true)
  assert.equal(r.existing, null)
  assert.equal(r.front, 'new-1')
  assert.equal(r.tabs.length, 1)
  assert.equal(r.tabs[0].preview, true)
}

// 9. Preview mode: single-click replaces the existing preview tab in place.
{
  let seq = 0
  const makeId = () => `new-${++seq}`
  const start = [
    { id: 't1', key: '1||p1', s: 's1', pane: 'p1' },
    { id: 't2', key: '1||p2', s: 's1', pane: 'p2', title: 'Old', program: 'claude', preview: true },
  ]
  const r = openAgentTab(start, 't1', { key: '1||p3', s: 's1', pane: 'p3' }, 'preview', makeId)
  assert.equal(r.changed, true)
  assert.equal(r.existing, null)
  assert.equal(r.front, 't2')
  assert.equal(r.tabs.length, 2)
  assert.equal(r.tabs[0].key, '1||p1')
  assert.equal(r.tabs[0].preview, undefined)
  assert.equal(r.tabs[1].id, 't2')
  assert.equal(r.tabs[1].key, '1||p3')
  assert.equal(r.tabs[1].title, undefined)
  assert.equal(r.tabs[1].program, undefined)
  assert.equal(r.tabs[1].preview, true)
}

// 10. Preview mode: when all open tabs are pinned, single-click opens a new preview tab right after front.
{
  let seq = 0
  const makeId = () => `new-${++seq}`
  const r = openAgentTab(tabs3, 't1', { key: '1||p4', s: 's1', pane: 'p4' }, 'preview', makeId)
  assert.equal(r.changed, true)
  assert.equal(r.existing, null)
  assert.equal(r.front, 'new-1')
  assert.deepEqual(
    r.tabs.map((x) => x.id),
    ['t1', 'new-1', 't2', 't3'],
  )
  assert.equal(r.tabs[1].preview, true)
  assert.equal(r.tabs[0].preview, undefined)
}

// 11. Preview mode: clicking an already-open normal tab focuses it without downgrading to preview.
{
  const start = [
    { id: 't1', key: '1||p1', s: 's1', pane: 'p1' },
    { id: 't2', key: '1||p2', s: 's1', pane: 'p2', preview: true },
  ]
  const r = openAgentTab(start, 't2', { key: '1||p1', s: 's1', pane: 'p1' }, 'preview', () => 'unused')
  assert.equal(r.changed, false)
  assert.equal(r.front, 't1')
  assert.equal(r.existing?.id, 't1')
  assert.equal(r.tabs[0].preview, undefined)
  assert.equal(r.tabs[1].preview, true)
}

// 12. Pinned mode (double-click / Cmd+click): promotes an already-open preview tab to normal.
{
  const start = [
    { id: 't1', key: '1||p1', s: 's1', pane: 'p1' },
    { id: 't2', key: '1||p2', s: 's1', pane: 'p2', preview: true },
  ]
  const r = openAgentTab(start, 't1', { key: '1||p2', s: 's1', pane: 'p2' }, 'pinned', () => 'unused')
  assert.equal(r.changed, true)
  assert.equal(r.front, 't2')
  assert.equal(r.existing?.id, 't2')
  assert.equal('preview' in r.tabs[1], false)
}

// 13. Pinned mode: opens a new normal tab after front without replacing an existing preview tab.
{
  const start = [
    { id: 't1', key: '1||p1', s: 's1', pane: 'p1' },
    { id: 't2', key: '1||p2', s: 's1', pane: 'p2', preview: true },
  ]
  const r = openAgentTab(start, 't1', { key: '1||p3', s: 's1', pane: 'p3' }, 'pinned', () => 't3')
  assert.equal(r.changed, true)
  assert.equal(r.existing, null)
  assert.equal(r.front, 't3')
  assert.deepEqual(
    r.tabs.map((x) => [x.id, !!x.preview]),
    [
      ['t1', false],
      ['t3', false],
      ['t2', true],
    ],
  )
}

// 14. Split pane target: focuses existing tab on secondary pane and pins if requested.
{
  const start = [{ id: 't1', key: '1||p1', s: 's1', pane: 'p1', split: '1||p2', preview: true }]
  const live = new Set(['1||p1', '1||p2'])
  const r1 = openAgentTab(start, 't1', { key: '1||p2', s: 's1', pane: 'p2' }, 'preview', () => 'unused', live)
  assert.equal(r1.focus, 'secondary')
  assert.equal(r1.tabs[0].preview, true)
  const r2 = openAgentTab(start, 't1', { key: '1||p2', s: 's1', pane: 'p2' }, 'pinned', () => 'unused', live)
  assert.equal(r2.changed, true)
  assert.equal(r2.focus, 'secondary')
  assert.equal('preview' in r2.tabs[0], false)
}

// 15. pinAgentTab and pinAgentTabByKey promote preview tabs (and no-op on normal tabs).
{
  const start = [
    { id: 't1', key: '1||p1', s: 's1', pane: 'p1' },
    { id: 't2', key: '1||p2', s: 's1', pane: 'p2', split: '1||p3', preview: true },
  ]
  assert.equal(pinAgentTab(start, 't1').changed, false)
  const p1 = pinAgentTab(start, 't2')
  assert.equal(p1.changed, true)
  assert.equal('preview' in p1.tabs[1], false)

  assert.equal(pinAgentTabByKey(start, '1||p1').changed, false)
  const p2 = pinAgentTabByKey(start, '1||p3')
  assert.equal(p2.changed, true)
  assert.equal('preview' in p2.tabs[1], false)
}

// 16. moveAgentTab reorders tabs and pins a dragged preview tab.
{
  const start = [
    { id: 't1', key: '1||p1', s: 's1', pane: 'p1' },
    { id: 't2', key: '1||p2', s: 's1', pane: 'p2', preview: true },
    { id: 't3', key: '1||p3', s: 's1', pane: 'p3' },
  ]
  const m = moveAgentTab(start, 't2', 0)
  assert.equal(m.changed, true)
  assert.deepEqual(
    m.tabs.map((x) => [x.id, !!x.preview]),
    [
      ['t2', false],
      ['t1', false],
      ['t3', false],
    ],
  )
  assert.equal('preview' in m.tabs[0], false)
}

// 17. normalizeAgentTabs sanitizes invalid preview flags and enforces at most one preview tab.
{
  const dirty = [
    { id: 't1', key: '1||p1', s: 's1', preview: true },
    { id: 't2', key: '1||p2', s: 's1', preview: false },
    { id: 't3', key: '1||p3', s: 's1', preview: true },
  ]
  const clean = normalizeAgentTabs(dirty)
  assert.deepEqual(
    clean.map((x) => [x.id, x.preview]),
    [
      ['t1', undefined],
      ['t2', undefined],
      ['t3', true],
    ],
  )
  assert.equal('preview' in clean[0], false)
  assert.equal('preview' in clean[1], false)
}

// 18. expandSplitAgentTab clears split on front tab and opens secondary as its own independent pinned tab.
{
  const start = [
    { id: 't1', key: '1||p1', s: 's1', pane: 'p1', split: '1||p2', focus: 'secondary' },
    { id: 't2', key: '1||p3', s: 's1', pane: 'p3', preview: true },
  ]
  const live = new Set(['1||p1', '1||p2', '1||p3'])
  const r = expandSplitAgentTab(start, 't1', { key: '1||p2', s: 's1', pane: 'p2' }, () => 't-expanded', live)
  assert.equal(r.changed, true)
  assert.equal(r.existing, null)
  assert.equal(r.front, 't-expanded')
  assert.deepEqual(
    r.tabs.map((x) => ({ id: x.id, key: x.key, split: x.split, focus: x.focus, preview: x.preview })),
    [
      { id: 't1', key: '1||p1', split: undefined, focus: undefined, preview: undefined },
      { id: 't-expanded', key: '1||p2', split: undefined, focus: undefined, preview: undefined },
      { id: 't2', key: '1||p3', split: undefined, focus: undefined, preview: true },
    ],
  )
  assert.equal('split' in r.tabs[0], false)
  assert.equal('focus' in r.tabs[0], false)
  assert.equal('preview' in r.tabs[1], false)
}

console.log('agentTabPolicy: all tests passed')
