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

const { rowTasks, splitRowTasks, hasBgCli, rowMatchesQuery, visibleCliAgents, buildAgentGroups } = order
const {
  mergeRowTasks,
  conversationRunningItems,
  filterHostCmds,
  keepTmuxBg,
  readTmuxBg,
  subagentRequestUrl,
  shouldContinueSubPoll,
} = side

// Minimal matchQuery equivalent (case-insensitive substring across fields, as in agents.ts).
const matchQuery = (q, ...fields) => {
  const needle = q.trim().toLowerCase()
  if (!needle) return true
  return fields.some((f) => !!f && f.toLowerCase().includes(needle))
}

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
// from convo.running (by task ID) AND clears them naturally when both herdr and
// convo.running stop reporting them after command exit.
{
  const herdrTasks = rowTasks([
    { id: '11111111-2222-4333-8444-555555555501', what: 'Run Neon on WP1', since: 1700000000000, agent: 'agy', task: 'b1', cmd: 'agy -p WP1' },
  ])
  const convoRunning = [
    { id: 'b1', kind: 'cmd', what: 'Run Neon on WP1', cmd: 'agy -p WP1' },
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

  // Command finishes: next herdr scan omits it and convo.running omits it.
  const afterExit = mergeRowTasks(rowTasks([], undefined), [])
  assert.equal(afterExit, undefined)
  const parentAfter = makeRow({ key: 'p1', title: 'Model Hub CYY', status: 'done', since: 1700000000000, tasks: afterExit })
  assert.equal(hasBgCli(parentAfter), false)
  const gDone = buildAgentGroups([parentAfter], { group: 'status', sort: 'recent' })
  assert.deepEqual(
    gDone.map((g) => [g.kind, g.rows.map((r) => r.key)]),
    [['todo', ['p1']]],
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
    items.map((x) => [x.kind, x.agent ?? '', x.id, x.task ?? '']),
    [
      ['agent', 'agy', '11111111-2222-4333-8444-555555555501', 'b7'],
      ['agent', 'codex', '22222222-3333-4444-8555-666666666602', 'b7'],
      ['agent', 'claude', '33333333-4444-4555-8666-777777777703', 'b8'],
      ['cmd', '', 'b2', ''],
    ],
  )
}

// 5. Frontend review #3: filterHostCmds deduplicates stale herdr bg_cmd_list
// entries already promoted in runningItems (both exact task IDs and synthetic
// codex-/agy- host PIDs) and decrements hostCmds.n so header status and
// SessionCard never double-count or double-render them.
{
  const staleHostCmds = {
    n: 2,
    list: [
      { task: 'b1', title: 'agy -p WP1', desc: 'Run Neon on WP1', since: 1700000000000 },
      { task: 'b2', title: 'npm run dev', desc: 'Dev server', since: 1700000010000 },
    ],
  }
  const runningItems = [
    { id: '11111111-2222-4333-8444-555555555501', kind: 'agent', agent: 'agy', task: 'b1', what: 'Run Neon on WP1', cmd: 'agy -p WP1' },
  ]
  const filtered = filterHostCmds(staleHostCmds, runningItems)
  assert.ok(filtered)
  assert.equal(filtered.n, 1)
  assert.deepEqual(filtered.list.map((c) => c.task), ['b2'])

  // When all host cmds were promoted, filterHostCmds returns undefined.
  const allPromoted = filterHostCmds({ n: 1, list: [staleHostCmds.list[0]] }, runningItems)
  assert.equal(allPromoted, undefined)

  // Synthetic codex-<pid> host command matched by command text is also removed.
  const synthHost = {
    n: 1,
    list: [{ task: 'codex-8597', title: 'agy -p WP1 --model gemini-4-neon-high', since: 1700000000000 }],
  }
  const synthFiltered = filterHostCmds(synthHost, [
    { id: '11111111-2222-4333-8444-555555555501', kind: 'agent', agent: 'agy', task: 'call_abc', what: 'agy -p WP1 --model gemini-4-neon-high', cmd: 'agy -p WP1 --model gemini-4-neon-high' },
  ])
  assert.equal(synthFiltered, undefined)
}

// 6. Frontend review #1: tmux background cache retains CLI subagents after
// switching away from a tmux parent so sidebar child rows and Working group
// placement survive until expiry or empty refresh.
{
  const cache = new Map()
  const now = 1700000100000
  const freshMs = 120_000
  const cliAgents = [
    { id: '11111111-2222-4333-8444-555555555501', what: 'Run Neon on WP1', since: now - 10_000, agent: 'agy', task: 'b1', cmd: 'agy -p WP1' },
  ]
  keepTmuxBg(cache, 'tmux:1', undefined, cliAgents, now)

  // Switched to another agent 30s later: CLI subagent still read from cache.
  const cached = readTmuxBg(cache, 'tmux:1', now + 30_000, freshMs)
  assert.equal(cached.cmds, undefined)
  assert.equal(cached.agents?.length, 1)
  const tmuxRow = makeRow({
    key: 'tmux:1',
    title: 'box · main',
    status: 'unknown',
    cmds: cached.cmds,
    tasks: rowTasks(cached.agents, cached.cmds),
  })
  assert.equal(hasBgCli(tmuxRow), true)
  const groups = buildAgentGroups([tmuxRow], { group: 'status', sort: 'recent' })
  assert.deepEqual(groups.map((g) => [g.kind, g.rows.map((r) => r.key)]), [['working', ['tmux:1']]])

  // Expired after freshMs: cache yields empty.
  const expired = readTmuxBg(cache, 'tmux:1', now + freshMs + 1, freshMs)
  assert.equal(expired.agents, undefined)

  // Fresh read with no cmds and no agents clears the cache immediately.
  keepTmuxBg(cache, 'tmux:1', undefined, undefined, now + 5_000)
  assert.equal(readTmuxBg(cache, 'tmux:1', now + 6_000, freshMs).agents, undefined)
}

// 7. Frontend review #4: sidebar search finds parent rows via CLI sub-row
// description, command text, or CLI type, and narrows displayed CLI children
// when only a subset matches.
{
  const tasks = rowTasks([
    { id: '11111111-2222-4333-8444-555555555501', what: 'Run Neon on WP1', since: 1700000000000, agent: 'agy', task: 'b1', cmd: 'agy -p WP1 --model gemini-4-neon-high' },
    { id: '22222222-3333-4444-8555-666666666602', what: 'Audit auth', since: 1700000010000, agent: 'claude', task: 'b2', cmd: 'claude -p audit' },
  ])
  const parent = makeRow({ key: 'p1', title: 'Model Hub CYY', where: 'main', program: 'claude', status: 'working', tasks })

  // Query matching only WP1 (absent from parent title/where/program) still matches parent.
  assert.equal(rowMatchesQuery(parent, 'WP1', matchQuery), true)
  assert.equal(rowMatchesQuery(parent, 'agy', matchQuery), true)
  assert.equal(rowMatchesQuery(parent, 'gemini-4-neon', matchQuery), true)
  assert.equal(rowMatchesQuery(parent, 'nonexistent-xyz', matchQuery), false)

  // Narrowing CLI children when parent itself did not match:
  assert.deepEqual(
    visibleCliAgents(parent, 'WP1', false, matchQuery).map((x) => x.id),
    ['11111111-2222-4333-8444-555555555501'],
  )
  // When parent itself matches ('Model Hub'), all CLI children stay visible:
  assert.equal(visibleCliAgents(parent, 'Model Hub', false, matchQuery).length, 2)
  // When collapsed (no query), CLI children are hidden:
  assert.equal(visibleCliAgents(parent, '', true, matchQuery).length, 0)
}

// 8. Frontend review #2 & #6: subagentRequestUrl carries cli and cli_parent
// query params, and shouldContinueSubPoll stops polling when depth-1 CLI ends.
{
  const nativeUrl = subagentRequestUrl('s1', 'w1:p1', 'a1b2c3d4', 0, 0, false)
  assert.equal(nativeUrl, '/api/sessions/s1/conversation/subagent?pane=w1%3Ap1&agent=a1b2c3d4&gen=0&rev=0&older=0')

  const cliUrl = subagentRequestUrl('s1', 'w1:p1', '11111111-2222-4333-8444-555555555501', 2, 5, false, 'agy')
  assert.ok(cliUrl.includes('cli=agy'))
  assert.ok(!cliUrl.includes('cli_parent='))

  const nestedCliUrl = subagentRequestUrl(
    's1',
    'w1:p1',
    'a987654321',
    0,
    0,
    true,
    'claude',
    '22222222-3333-4444-8555-666666666601',
  )
  assert.ok(nestedCliUrl.includes('cli=claude'))
  assert.ok(nestedCliUrl.includes('cli_parent=22222222-3333-4444-8555-666666666601'))
  assert.ok(nestedCliUrl.includes('older=1'))

  // Polling stops for depth-1 CLI subagent as soon as live flips to false.
  assert.equal(shouldContinueSubPoll(true, false, 0, 1000), true)
  assert.equal(shouldContinueSubPoll(false, false, 999, 1000), false)
  // Nested subagent polls only while its log grew within growingMs.
  assert.equal(shouldContinueSubPoll(false, true, 1000, 10_000, 30_000), true)
  assert.equal(shouldContinueSubPoll(false, true, 1000, 40_000, 30_000), false)
}

// 9. Source-level contract checks so accidental removal of SubPanel read-only
// guards (ForkAtOf/RewindAtOf) or subagentRequestUrl wiring in subagent.ts fails tests.
{
  const subTs = readFileSync(fileURLToPath(new URL('../src/terminal/subagent.ts', import.meta.url)), 'utf8')
  assert.ok(subTs.includes('subagentRequestUrl(sessionId, pane, agent, a.gen, a.rev, older && n === 0, cli, cliParent)'), 'subagent.ts must wire cli & cliParent through subagentRequestUrl')
  assert.ok(subTs.includes('shouldContinueSubPoll('), 'subagent.ts must use shouldContinueSubPoll')

  const convoTsx = readFileSync(fileURLToPath(new URL('../src/terminal/Conversation.tsx', import.meta.url)), 'utf8')
  const subPanelIdx = convoTsx.indexOf('<SubPanel')
  assert.ok(subPanelIdx > 0)
  const surrounding = convoTsx.slice(Math.max(0, subPanelIdx - 300), subPanelIdx)
  assert.ok(surrounding.includes('<ForkAtOf.Provider value={undefined}>'), 'SubPanel must disable ForkAtOf')
  assert.ok(surrounding.includes('<RewindAtOf.Provider value={undefined}>'), 'SubPanel must disable RewindAtOf')
}

console.log('test-bg-cli: ok')
