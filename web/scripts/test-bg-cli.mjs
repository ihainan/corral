import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { fileURLToPath } from 'node:url'
import ts from 'typescript'

function loadTs(rel) {
  const srcPath = fileURLToPath(new URL(rel, import.meta.url))
  const src = readFileSync(srcPath, 'utf8')
  const { outputText } = ts.transpileModule(src, {
    compilerOptions: { module: ts.ModuleKind.ESNext, target: ts.ScriptTarget.ES2022 },
  })
  return import(`data:text/javascript;base64,${Buffer.from(outputText).toString('base64')}`)
}

const order = await loadTs('../src/terminal/agentOrder.ts')
const side = await loadTs('../src/terminal/sideState.ts')

const { rowTasks, splitRowTasks, hasBgCli, buildAgentGroups } = order
const { mergeRowTasks, conversationRunningItems } = side

function makeRow(partial) {
  return {
    where: '',
    status: 'idle',
    current: false,
    onPick: () => {},
    ...partial,
  }
}

// 1. All 6 background commands recognized as agy CLI subagents:
// splitRowTasks puts all 6 in cliAgents (with CLI program, description, time)
// and leaves 0 in bgTasks (so "N commands in the background" is NOT shown).
{
  const bgAgentList = Array.from({ length: 6 }, (_, i) => ({
    id: `11111111-2222-4333-8444-55555555550${i + 1}`,
    what: `Run Neon on WP${i + 1}`,
    since: 1700000000000 + i * 1000,
    agent: 'agy',
    task: `b${i + 1}`,
    cmd: `agy -p "WP${i + 1}" --model gemini-4-neon-high`,
  }))
  const tasks = rowTasks(bgAgentList, undefined)
  assert.ok(tasks && tasks.length === 6)
  const { cliAgents, bgTasks } = splitRowTasks(tasks)
  assert.equal(cliAgents.length, 6)
  assert.equal(bgTasks.length, 0)
  assert.equal(cliAgents[0].agent, 'agy')
  assert.equal(cliAgents[0].name, 'Run Neon on WP1')
  assert.equal(cliAgents[0].since, 1700000000000)
  assert.equal(cliAgents[0].task, 'b1')
}

// 2. Mixed background work: CLI subagents (agy, claude) + native Claude Task
// subagent + plain commands (including an unresolved agy fallback).
// CLI subagents go to cliAgents; native subagent and plain commands stay in bgTasks.
{
  const tasks = rowTasks(
    [
      { id: 'native001', what: 'Explore repo', since: 1700000100000 },
      { id: 'aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeee1', what: 'Run Neon on WP1', since: 1700000200000, agent: 'agy', task: 'b1', cmd: 'agy -p WP1' },
      { id: 'aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeee2', what: 'Review auth', since: 1700000300000, agent: 'claude', task: 'b2', cmd: 'claude -p review' },
    ],
    {
      n: 2,
      list: [
        // b1 accidentally still present in cmds must be suppressed by ownedTasks
        { task: 'b1', title: 'agy -p WP1', desc: 'Run Neon on WP1', since: 1700000200000 },
        { task: 'b3', title: 'agy -p no-session', desc: 'Fallback cmd', since: 1700000400000 },
      ],
    },
  )
  const { cliAgents, bgTasks } = splitRowTasks(tasks)
  assert.deepEqual(
    cliAgents.map((x) => [x.agent, x.id, x.name]),
    [
      ['agy', 'aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeee1', 'Run Neon on WP1'],
      ['claude', 'aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeee2', 'Review auth'],
    ],
  )
  assert.deepEqual(
    bgTasks.map((x) => [x.kind, x.id]),
    [
      ['agent', 'native001'],
      ['cmd', 'b3'],
    ],
  )
}

// 3. mergeRowTasks deduplicates promoted CLI subagents against raw cmd entries
// from convo.running (by task ID) so a promoted command never appears twice.
{
  const herdrTasks = rowTasks([
    { id: '11111111-2222-4333-8444-555555555501', what: 'Run Neon on WP1', since: 1700000000000, agent: 'agy', task: 'b1', cmd: 'agy -p WP1' },
  ])
  const convoRunning = [
    // Raw un-upgraded cmd from log with same task ID 'b1' must be suppressed.
    { id: 'b1', kind: 'cmd', what: 'Run Neon on WP1', cmd: 'agy -p WP1' },
    // Separate plain command stays.
    { id: 'b9', kind: 'cmd', what: 'Run tests', cmd: 'go test ./...' },
  ]
  const merged = mergeRowTasks(herdrTasks, convoRunning)
  assert.ok(merged)
  const { cliAgents, bgTasks } = splitRowTasks(merged)
  assert.equal(cliAgents.length, 1)
  assert.equal(cliAgents[0].id, '11111111-2222-4333-8444-555555555501')
  assert.deepEqual(
    bgTasks.map((x) => [x.kind, x.id]),
    [['cmd', 'b9']],
  )
}

// 4. conversationRunningItems upgrades convo.running cmds using agentTasks CLI
// info, appends CLI subagents missing from convo.running (e.g. done/idle parent),
// and handles multiple CLI subagents under one task.
{
  const agentTasks = rowTasks([
    { id: '11111111-2222-4333-8444-555555555501', what: 'Parallel review', since: 1700000100000, agent: 'agy', task: 'b7', cmd: 'agy -p A & codex exec B' },
    { id: '22222222-3333-4444-8555-666666666602', what: 'Parallel review', since: 1700000100000, agent: 'codex', task: 'b7', cmd: 'agy -p A & codex exec B' },
    { id: '33333333-4444-4555-8666-777777777703', what: 'Idle-parent CLI', since: 1700000200000, agent: 'claude', task: 'b8', cmd: 'claude -p check' },
  ])
  const convoRunning = [
    { id: 'b7', kind: 'cmd', what: 'Parallel review', cmd: 'agy -p A & codex exec B', ts: '2026-11-27T01:00:00.000Z' },
    { id: 'b2', kind: 'cmd', what: 'Dev server', cmd: 'npm run dev', ts: '2026-11-27T01:01:00.000Z' },
  ]
  const items = conversationRunningItems(convoRunning, agentTasks)
  assert.deepEqual(
    items.map((x) => [x.kind, x.agent ?? '', x.id]),
    [
      ['agent', 'agy', '11111111-2222-4333-8444-555555555501'],
      ['agent', 'codex', '22222222-3333-4444-8555-666666666602'],
      ['agent', 'claude', '33333333-4444-4555-8666-777777777703'],
      ['cmd', '', 'b2'],
    ],
  )
}

// 5. Sidebar grouping: a done/idle parent with background CLI subagents stays
// in the Working group (same as background commands), and returns to done/idle
// once they finish and disappear.
{
  const cliTasks = rowTasks([
    { id: '11111111-2222-4333-8444-555555555501', what: 'Run Neon on WP1', since: 1700000000000, agent: 'agy', task: 'b1' },
  ])
  const activeParent = makeRow({ key: 'p1', title: 'Model Hub CYY', status: 'done', since: 1700000000000, tasks: cliTasks })
  assert.equal(hasBgCli(activeParent), true)
  const gActive = buildAgentGroups([activeParent], { group: 'status', sort: 'recent' })
  assert.deepEqual(
    gActive.map((g) => [g.kind, g.rows.map((r) => r.key)]),
    [['working', ['p1']]],
  )

  // Once the background CLI command finishes, tasks is undefined -> moves back to todo (done).
  const finishedParent = makeRow({ key: 'p1', title: 'Model Hub CYY', status: 'done', since: 1700000000000, tasks: undefined })
  assert.equal(hasBgCli(finishedParent), false)
  const gDone = buildAgentGroups([finishedParent], { group: 'status', sort: 'recent' })
  assert.deepEqual(
    gDone.map((g) => [g.kind, g.rows.map((r) => r.key)]),
    [['todo', ['p1']]],
  )
}

console.log('test-bg-cli: ok')
