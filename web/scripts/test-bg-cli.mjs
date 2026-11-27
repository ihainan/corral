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
  createSubConvoPoller,
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

// 5. Frontend review #3 + Round 3 #4 & #5: filterHostCmds deduplicates stale
// herdr bg_cmd_list entries strictly by exact task ID when task ID is present,
// falls back to exact normalized text equality (at most 1 consumption per CLI)
// ONLY when task ID is absent, never deletes an independent command via prefix
// match, and decrements overflow cmds.n ONLY when cmds.tasks confirms membership.
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

  // Synthetic codex-<pid> host command matches strictly by its host task ID.
  const synthHost = {
    n: 2,
    list: [
      { task: 'codex-8597', title: 'npm run dev', since: 1700000000000 },
      { task: 'codex-8598', title: 'npm run dev & agy -p audit', since: 1700000001000 },
    ],
  }
  const synthFiltered = filterHostCmds(synthHost, [
    { id: '11111111-2222-4333-8444-555555555501', kind: 'agent', agent: 'agy', task: 'codex-8598', what: 'npm run dev & agy -p audit', cmd: 'npm run dev & agy -p audit' },
  ])
  assert.ok(synthFiltered)
  assert.equal(synthFiltered.n, 1)
  assert.deepEqual(synthFiltered.list.map((c) => c.task), ['codex-8597'])

  // Even when a CLI has a different task ID, a command WITH a task ID is NEVER
  // deleted by prefix/text match (independent `npm run dev` stays intact).
  const prefixGuard = filterHostCmds(
    { n: 1, list: [{ task: 'codex-100', title: 'npm run dev', since: 1700000000000 }] },
    [{ id: '11111111-2222-4333-8444-555555555501', kind: 'agent', agent: 'agy', task: 'codex-200', what: 'npm run dev & agy -p audit', cmd: 'npm run dev & agy -p audit' }],
  )
  assert.ok(prefixGuard)
  assert.equal(prefixGuard.n, 1)
  assert.equal(prefixGuard.list.length, 1)

  // Taskless host commands: prefix MUST NOT match, exact text matches at most ONCE per CLI.
  const tasklessHost = {
    n: 3,
    list: [
      { title: 'npm run dev', since: 1700000000000 },
      { title: 'npm run dev & agy -p audit', since: 1700000001000 },
      { title: 'npm run dev & agy -p audit', since: 1700000002000 },
    ],
  }
  const tasklessFiltered = filterHostCmds(tasklessHost, [
    { id: '11111111-2222-4333-8444-555555555501', kind: 'agent', agent: 'agy', what: 'npm run dev & agy -p audit', cmd: 'npm run dev & agy -p audit' },
  ])
  assert.ok(tasklessFiltered)
  assert.equal(tasklessFiltered.n, 2)
  assert.deepEqual(
    tasklessFiltered.list.map((c) => c.title),
    ['npm run dev', 'npm run dev & agy -p audit'],
  )

  // Round 3 #5: overflow task beyond the first 8 in cmds.list.
  const first8 = Array.from({ length: 8 }, (_, i) => ({ task: `b${i + 1}`, title: `cmd ${i + 1}`, since: 1700000000000 + i }))
  const stale9WithTasks = {
    n: 9,
    list: first8,
    tasks: [...first8.map((c) => c.task), 'b9'],
  }
  // b9 is confirmed inside cmds.tasks -> n decrements from 9 to 8 (procMore = 0).
  const overflowDeduped = filterHostCmds(stale9WithTasks, [
    { id: '11111111-2222-4333-8444-555555555509', kind: 'agent', agent: 'agy', task: 'b9', what: 'Run WP9', cmd: 'agy -p WP9' },
  ])
  assert.ok(overflowDeduped)
  assert.equal(overflowDeduped.n, 8)
  assert.equal(overflowDeduped.list.length, 8)

  // Two CLIs spawned by the SAME overflow task b9 decrement n only ONCE (9 -> 8).
  const overflowMultiCli = filterHostCmds(stale9WithTasks, [
    { id: '11111111-2222-4333-8444-555555555509', kind: 'agent', agent: 'agy', task: 'b9', what: 'Parallel' },
    { id: '22222222-3333-4444-8555-666666666609', kind: 'agent', agent: 'codex', task: 'b9', what: 'Parallel' },
  ])
  assert.equal(overflowMultiCli?.n, 8)

  // Brand-new task b10 NOT in cmds.tasks MUST NOT wrongly decrement n (stays 9).
  const brandNew = filterHostCmds(stale9WithTasks, [
    { id: '11111111-2222-4333-8444-555555555510', kind: 'agent', agent: 'agy', task: 'b10', what: 'New WP10' },
  ])
  assert.equal(brandNew?.n, 9)

  // When cmds.tasks is absent (uncertain whether b9 was counted), do NOT decrement n.
  const uncertain = filterHostCmds(
    { n: 9, list: first8 },
    [{ id: '11111111-2222-4333-8444-555555555509', kind: 'agent', agent: 'agy', task: 'b9', what: 'Run WP9' }],
  )
  assert.equal(uncertain?.n, 9)
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
  assert.ok(!nestedCliUrl.includes('cli_chain='))
  assert.ok(nestedCliUrl.includes('older=1'))

  // Round 3 #6: grandchild subagent carries cli_parent (root) AND cli_chain (direct parent chain).
  const grandchildUrl = subagentRequestUrl(
    's1',
    'w1:p1',
    '01a1dd7f-4856-7042-8ad2-f46c56372e03',
    0,
    0,
    false,
    'codex',
    '01a1dd7f-4856-7042-8ad2-f46c56372e01',
    ['01a1dd7f-4856-7042-8ad2-f46c56372e02'],
  )
  assert.ok(grandchildUrl.includes('cli=codex'))
  assert.ok(grandchildUrl.includes('cli_parent=01a1dd7f-4856-7042-8ad2-f46c56372e01'))
  assert.ok(grandchildUrl.includes('cli_chain=01a1dd7f-4856-7042-8ad2-f46c56372e02'))

  // Polling stops for depth-1 CLI subagent as soon as live flips to false.
  assert.equal(shouldContinueSubPoll(true, false, 0, 1000), true)
  assert.equal(shouldContinueSubPoll(false, false, 999, 1000), false)
  // Nested subagent polls only while its log grew within growingMs.
  assert.equal(shouldContinueSubPoll(false, true, 1000, 10_000, 30_000), true)
  assert.equal(shouldContinueSubPoll(false, true, 1000, 40_000, 30_000), false)
}

// 9. Round 3 #7: Real behavioral lifecycle tests for createSubConvoPoller with
// fake timers and stubbed async fetchPage — verifying polling while live, no
// further timers after CLI ends, timer cancellation & in-flight abort on panel
// close/unmount, and nested cli_chain polling expiry.
{
  const makeFakeTimers = () => {
    let nextId = 1
    const active = new Map()
    const cleared = []
    return {
      active,
      cleared,
      setTimer: (fn, ms) => {
        const id = nextId++
        active.set(id, { fn, ms })
        return id
      },
      clearTimer: (id) => {
        cleared.push(id)
        active.delete(id)
      },
      fireNext: () => {
        const first = active.entries().next()
        if (first.done) return false
        const [id, { fn }] = first.value
        active.delete(id)
        fn()
        return true
      },
    }
  }

  // 9a. Live CLI polls periodically, then stops scheduling once live flips false.
  {
    const timers = makeFakeTimers()
    let live = true
    let clock = 1000
    const urls = []
    let state = { entries: [], running: [], older: false, olderCut: false, loading: true, failed: false, grewAt: 0 }
    const poller = createSubConvoPoller({
      sessionId: 's1',
      pane: 'w1:p1',
      agent: '11111111-2222-4333-8444-555555555501',
      cli: 'agy',
      nested: false,
      live: () => live,
      active: () => true,
      hidden: () => false,
      now: () => clock,
      everyMs: 3000,
      fetchPage: async (url) => {
        urls.push(url)
        return { gen: 1, rev: urls.length, entries: [{ seq: urls.length }] }
      },
      onState: (up) => {
        state = up(state)
      },
      setTimer: timers.setTimer,
      clearTimer: timers.clearTimer,
    })

    await poller.run(false)
    assert.equal(urls.length, 1)
    assert.ok(urls[0].includes('cli=agy'))
    assert.equal(timers.active.size, 1)
    assert.equal([...timers.active.values()][0].ms, 3000)

    // Firing the timer polls again while still live.
    clock += 3000
    assert.equal(timers.fireNext(), true)
    await new Promise((r) => setImmediate(r))
    assert.equal(urls.length, 2)
    assert.ok(urls[1].includes('gen=1&rev=1'))
    assert.equal(timers.active.size, 1)

    // CLI ends: live flips false; one final read catches trailing output and
    // MUST NOT schedule another timer.
    live = false
    clock += 1000
    await poller.run(false)
    assert.equal(urls.length, 3)
    assert.equal(timers.active.size, 0, 'must not schedule next poll after CLI ends')
    assert.equal(state.entries.length, 3)
  }

  // 9b. Panel close/unmount (stop()) cancels pending timer AND aborts in-flight
  // request without updating state or scheduling another poll after resolution.
  {
    const timers = makeFakeTimers()
    let resolveInFlight
    let capturedSignal
    let stateUpdatesAfterStop = 0
    let stopped = false
    let state = { entries: [], running: [], older: false, olderCut: false, loading: true, failed: false, grewAt: 0 }
    const poller = createSubConvoPoller({
      sessionId: 's1',
      pane: 'w1:p1',
      agent: '11111111-2222-4333-8444-555555555501',
      cli: 'agy',
      nested: false,
      live: () => true,
      active: () => true,
      hidden: () => false,
      everyMs: 3000,
      fetchPage: (_url, signal) => {
        capturedSignal = signal
        return new Promise((resolve) => {
          resolveInFlight = resolve
        })
      },
      onState: (up) => {
        if (stopped) stateUpdatesAfterStop++
        state = up(state)
      },
      setTimer: timers.setTimer,
      clearTimer: timers.clearTimer,
    })

    const inflight = poller.run(false)
    assert.equal(capturedSignal.aborted, false)
    stopped = true
    poller.stop()
    assert.equal(capturedSignal.aborted, true, 'stop() must abort in-flight request')
    // Even if the stubbed promise resolves late, no state update or timer may occur.
    resolveInFlight({ gen: 1, rev: 1, entries: [{ seq: 1 }] })
    await inflight
    assert.equal(stateUpdatesAfterStop, 0, 'aborted fetch must not update state')
    assert.equal(timers.active.size, 0, 'stop() must not schedule next poll')
  }

  // 9c. Closing panel while idle between polls clears the pending timer.
  {
    const timers = makeFakeTimers()
    let state = { entries: [], running: [], older: false, olderCut: false, loading: true, failed: false, grewAt: 0 }
    const poller = createSubConvoPoller({
      sessionId: 's1',
      pane: 'w1:p1',
      agent: '11111111-2222-4333-8444-555555555501',
      cli: 'claude',
      nested: false,
      live: () => true,
      active: () => true,
      hidden: () => false,
      everyMs: 3000,
      fetchPage: async () => ({ gen: 1, rev: 1, entries: [{ seq: 1 }] }),
      onState: (up) => {
        state = up(state)
      },
      setTimer: timers.setTimer,
      clearTimer: timers.clearTimer,
    })
    await poller.run(false)
    assert.equal(timers.active.size, 1)
    poller.stop()
    assert.equal(timers.active.size, 0, 'stop() must clear pending timer')
    assert.ok(timers.cleared.length >= 1)
  }

  // 9d. Nested Codex grandchild carries cli_chain and stops polling once quiet > growingMs.
  {
    const timers = makeFakeTimers()
    let clock = 10_000
    const urls = []
    let state = { entries: [], running: [], older: false, olderCut: false, loading: true, failed: false, grewAt: 0 }
    const poller = createSubConvoPoller({
      sessionId: 's1',
      pane: 'w1:p1',
      agent: '01a1dd7f-4856-7042-8ad2-f46c56372e03',
      cli: 'codex',
      cliParent: '01a1dd7f-4856-7042-8ad2-f46c56372e01',
      cliChain: ['01a1dd7f-4856-7042-8ad2-f46c56372e02'],
      nested: true,
      live: () => false,
      active: () => true,
      hidden: () => false,
      now: () => clock,
      everyMs: 3000,
      growingMs: 30_000,
      fetchPage: async (url) => {
        urls.push(url)
        return urls.length === 1
          ? { gen: 1, rev: 1, entries: [{ seq: 1 }] }
          : { gen: 1, rev: 1, entries: [] }
      },
      onState: (up) => {
        state = up(state)
      },
      setTimer: timers.setTimer,
      clearTimer: timers.clearTimer,
    })
    await poller.run(false)
    assert.ok(urls[0].includes('cli_chain=01a1dd7f-4856-7042-8ad2-f46c56372e02'))
    assert.equal(timers.active.size, 1, 'nested subagent polls while recently grown')
    clock += 31_000
    timers.fireNext()
    await new Promise((r) => setImmediate(r))
    assert.equal(urls.length, 2)
    assert.equal(timers.active.size, 0, 'nested subagent stops polling after growingMs without new entries')
  }
}

// 10. Read-only SubPanel wiring check (ForkAtOf/RewindAtOf disabled around SubPanel).
{
  const subTs = readFileSync(fileURLToPath(new URL('../src/terminal/subagent.ts', import.meta.url)), 'utf8')
  assert.ok(subTs.includes('createSubConvoPoller<'), 'subagent.ts must delegate polling lifecycle to createSubConvoPoller')

  const convoTsx = readFileSync(fileURLToPath(new URL('../src/terminal/Conversation.tsx', import.meta.url)), 'utf8')
  const subPanelIdx = convoTsx.indexOf('<SubPanel')
  assert.ok(subPanelIdx > 0)
  const surrounding = convoTsx.slice(Math.max(0, subPanelIdx - 300), subPanelIdx)
  assert.ok(surrounding.includes('<ForkAtOf.Provider value={undefined}>'), 'SubPanel must disable ForkAtOf')
  assert.ok(surrounding.includes('<RewindAtOf.Provider value={undefined}>'), 'SubPanel must disable RewindAtOf')
}

console.log('test-bg-cli: ok')
