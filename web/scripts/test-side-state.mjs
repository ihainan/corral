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

const side = await loadTs('../src/terminal/sideState.ts')
const order = await loadTs('../src/terminal/agentOrder.ts')

const { reconcileAgentStatus, mergeRowTasks, convoLastUserMs, stampAgeMs, resolveSubPeers } = side
const { asHeld, buildAgentGroups, rowTasks } = order

function makeRow(partial) {
  return {
    where: '',
    status: 'idle',
    current: false,
    onPick: () => {},
    ...partial,
  }
}

// 1. Background subagent in live conversation log appears in sidebar tasks even
// before herdr's periodic cmdProber scan reports bg_agent_list (no-bg-task.png).
{
  const herdrTasks = rowTasks(undefined, undefined) // herdr has not scanned yet
  assert.equal(herdrTasks, undefined)

  const convoRunning = [
    {
      id: 'a9876543210',
      what: 'Survey test K8s cluster capacity',
      kind: 'agent',
      ts: '2026-11-26T12:30:00.000Z',
    },
  ]
  const merged = mergeRowTasks(herdrTasks, convoRunning, { blocked: false, lost: false })
  assert.ok(merged && merged.length === 1)
  assert.equal(merged[0].kind, 'agent')
  assert.equal(merged[0].id, 'a9876543210')
  assert.equal(merged[0].name, 'Survey test K8s cluster capacity')
  assert.equal(merged[0].live, true)
  assert.equal(merged[0].since, Date.parse('2026-11-26T12:30:00.000Z'))
}

// 2. Merging herdr tasks + convoRunning deduplicates identical subagents and
// commands (including synthetic host PIDs like monitor-<pid>), keeping subagents first.
{
  const herdrTasks = rowTasks(
    [{ id: 'sub1', what: '', since: 1700000100000 }],
    {
      n: 2,
      list: [
        { task: 'b1', title: 'go test ./...', desc: 'Run tests', since: 1700000200000, out: '/tmp/b1.output' },
        { task: 'monitor-42', title: 'while kill -0 99; do sleep 20; done', since: 1700000300000 },
      ],
    },
  )
  const convoRunning = [
    { id: 'sub1', what: 'Survey cluster', kind: 'agent', ts: '2026-11-26T12:30:00.000Z' },
    { id: 'sub2', what: 'Check logs', kind: 'agent', ts: '2026-11-26T12:31:00.000Z' },
    { id: 'b1', what: 'Run tests', kind: 'cmd', cmd: 'go test ./...', out: '/tmp/b1.output' },
    { id: 'mon_abc', what: 'Watch job 99', kind: 'cmd', cmd: 'while kill -0 99; do sleep 20; done' },
  ]
  const merged = mergeRowTasks(herdrTasks, convoRunning)
  assert.ok(merged)
  assert.deepEqual(
    merged.map((x) => [x.kind, x.id]),
    [
      ['agent', 'sub1'],
      ['agent', 'sub2'],
      ['cmd', 'b1'],
      ['cmd', 'mon_abc'],
    ],
  )
  assert.equal(merged[0].name, 'Survey cluster')
  assert.equal(merged[0].since, 1700000100000)
  // Blocked or lost suppresses sidebar background tasks.
  assert.equal(mergeRowTasks(herdrTasks, convoRunning, { blocked: true }), undefined)
  assert.equal(mergeRowTasks(herdrTasks, convoRunning, { lost: true }), undefined)
}

// 3. Herdr says idle, conversation process/log says busy (working) -> reconciled
// status is working for both conversation view and sidebar, and sidebar groups
// it into Working (not Idle) with the conversation turn-start timestamp.
{
  const idleSince = 1700000000000
  const turnStart = 1700000060000
  const rec = reconcileAgentStatus({
    herdrStatus: 'idle',
    herdrRawStatus: 'idle',
    herdrSince: idleSince,
    convoLive: true,
    convoAgent: 'claude',
    convoStatus: 'busy',
    convoWorkingSince: turnStart,
  })
  assert.equal(rec.sideStatus, 'working')
  assert.equal(rec.viewStatus, 'working')
  assert.equal(rec.since, turnStart)

  const rows = [
    makeRow({
      key: 'win-ssh',
      title: 'Windows SSH',
      status: rec.sideStatus,
      liveStatus: rec.sideStatus,
      since: rec.since,
      current: true,
      placeAs: { status: 'idle', since: idleSince },
    }),
    makeRow({ key: 'build', title: 'Build Server', status: 'working', since: turnStart - 1000 }),
    makeRow({ key: 'other', title: 'Other Idle', status: 'idle', since: idleSince - 5000 }),
  ]
  const groups = buildAgentGroups(rows.map(asHeld), { group: 'status', sort: 'recent', nestChildren: true })
  const byKind = new Map(groups.map((g) => [g.kind, g]))
  assert.deepEqual(
    byKind.get('working')?.rows.map((r) => r.key),
    ['win-ssh', 'build'],
  )
  assert.deepEqual(
    byKind.get('idle')?.rows.map((r) => r.key),
    ['other'],
  )
}

// 4. Both sources update:
// 4a. Herdr catches up to working (while placeAs was idle from when row was opened,
//     exact scenario of idle-but-working.png) -> stays in Working group.
// 4b. Both return to idle -> moves to Idle group.
// 4c. Both report done (unseen) -> moves to For you (todo) group.
{
  const idleSince = 1700000000000
  const workSince = 1700000060000
  const doneSince = 1700000120000

  // 4a: both say working
  const bothWorking = reconcileAgentStatus({
    herdrStatus: 'working',
    herdrRawStatus: 'working',
    herdrSince: workSince,
    convoLive: true,
    convoAgent: 'claude',
    convoStatus: 'busy',
    convoWorkingSince: workSince - 1000,
  })
  assert.equal(bothWorking.sideStatus, 'working')
  assert.equal(bothWorking.viewStatus, 'working')
  assert.equal(bothWorking.since, workSince)

  const gWork = buildAgentGroups(
    [
      makeRow({
        key: 'win-ssh',
        title: 'Windows SSH',
        status: bothWorking.sideStatus,
        liveStatus: bothWorking.sideStatus,
        since: bothWorking.since,
        current: true,
        placeAs: { status: 'idle', since: idleSince },
      }),
    ].map(asHeld),
    { group: 'status', sort: 'recent', nestChildren: true },
  )
  assert.deepEqual(gWork.map((g) => [g.kind, g.rows.map((r) => r.key)]), [['working', ['win-ssh']]])

  // 4b: both say idle
  const bothIdle = reconcileAgentStatus({
    herdrStatus: 'idle',
    herdrRawStatus: 'idle',
    herdrSince: doneSince,
    convoLive: true,
    convoAgent: 'claude',
    convoStatus: 'idle',
    convoWorkingSince: workSince,
  })
  assert.equal(bothIdle.sideStatus, 'idle')
  assert.equal(bothIdle.viewStatus, 'idle')
  assert.equal(bothIdle.since, doneSince)

  const gIdle = buildAgentGroups(
    [
      makeRow({
        key: 'win-ssh',
        title: 'Windows SSH',
        status: bothIdle.sideStatus,
        liveStatus: bothIdle.sideStatus,
        since: bothIdle.since,
        current: true,
        placeAs: { status: 'working', since: workSince },
      }),
    ].map(asHeld),
    { group: 'status', sort: 'recent', nestChildren: true },
  )
  assert.deepEqual(gIdle.map((g) => [g.kind, g.rows.map((r) => r.key)]), [['idle', ['win-ssh']]])

  // 4c: herdr says done, log says idle (finished unseen turn)
  const bothDone = reconcileAgentStatus({
    herdrStatus: 'done',
    herdrRawStatus: 'done',
    herdrSince: doneSince,
    convoLive: true,
    convoAgent: 'claude',
    convoStatus: 'idle',
    convoWorkingSince: workSince,
  })
  assert.equal(bothDone.sideStatus, 'done')
  assert.equal(bothDone.viewStatus, 'done')

  const gDone = buildAgentGroups(
    [
      makeRow({
        key: 'win-ssh',
        title: 'Windows SSH',
        status: bothDone.sideStatus,
        liveStatus: bothDone.sideStatus,
        since: bothDone.since,
        current: true,
      }),
    ].map(asHeld),
    { group: 'status', sort: 'recent', nestChildren: true },
  )
  assert.deepEqual(gDone.map((g) => [g.kind, g.rows.map((r) => r.key)]), [['todo', ['win-ssh']]])
}

// 5. asHeld holds ONLY done -> idle (reading an unseen finished agent), never
// trapping idle -> working, blocked -> working, done -> working, or working -> idle.
{
  // done -> idle stays in todo while reading
  const doneToIdle = makeRow({
    key: 'd1',
    title: 'Just Read',
    status: 'idle',
    liveStatus: 'idle',
    since: 200,
    current: true,
    placeAs: { status: 'done', since: 100 },
  })
  assert.deepEqual(
    buildAgentGroups([asHeld(doneToIdle)], { group: 'status' }).map((g) => [g.kind, g.rows.map((r) => r.key)]),
    [['todo', ['d1']]],
  )

  // done -> working (user sent follow-up) moves immediately to Working
  const doneToWork = { ...doneToIdle, status: 'working', liveStatus: 'working' }
  assert.deepEqual(
    buildAgentGroups([asHeld(doneToWork)], { group: 'status' }).map((g) => [g.kind, g.rows.map((r) => r.key)]),
    [['working', ['d1']]],
  )

  // blocked -> working (user answered prompt) moves to Working (not dumped into Idle!)
  const blockedToWork = makeRow({
    key: 'b1',
    title: 'Answered',
    status: 'working',
    liveStatus: 'working',
    since: 300,
    current: true,
    placeAs: { status: 'blocked', since: 100 },
  })
  assert.deepEqual(
    buildAgentGroups([asHeld(blockedToWork)], { group: 'status' }).map((g) => [g.kind, g.rows.map((r) => r.key)]),
    [['working', ['b1']]],
  )
}

// 6. Additional reconciliation edge cases:
// - Herdr working + log idle stays working (never falsely downgraded).
// - Background running + answerable waiting prompt surfaces as blocked in BOTH views.
// - Claude dialog open does not surface as blocked.
// - Herdr lost stays unknown.
{
  const herdrAhead = reconcileAgentStatus({
    herdrStatus: 'working',
    herdrRawStatus: 'working',
    herdrSince: 500,
    convoLive: true,
    convoAgent: 'claude',
    convoStatus: 'idle',
  })
  assert.equal(herdrAhead.sideStatus, 'working')
  assert.equal(herdrAhead.viewStatus, 'working')

  const bgBlocked = reconcileAgentStatus({
    herdrStatus: 'working',
    herdrRawStatus: 'done',
    herdrSince: 400,
    bg: 'running',
    bgSince: 400,
    convoLive: true,
    convoAgent: 'claude',
    convoStatus: 'waiting',
    convoWaitingFor: 'permission prompt',
    convoWorkingSince: 600,
  })
  assert.equal(bgBlocked.sideStatus, 'blocked')
  assert.equal(bgBlocked.viewStatus, 'blocked')

  const dialog = reconcileAgentStatus({
    herdrStatus: 'idle',
    herdrRawStatus: 'idle',
    herdrSince: 400,
    convoLive: true,
    convoAgent: 'claude',
    convoStatus: 'waiting',
    convoWaitingFor: 'dialog open',
  })
  assert.equal(dialog.sideStatus, 'idle')
  assert.equal(dialog.viewStatus, 'idle')
  assert.equal(dialog.dialog, true)

  const lost = reconcileAgentStatus({
    herdrStatus: 'unknown',
    herdrRawStatus: 'idle',
    herdrSince: 400,
    herdrLost: true,
    convoLive: true,
    convoAgent: 'claude',
    convoStatus: 'busy',
  })
  assert.equal(lost.sideStatus, 'unknown')
  assert.equal(lost.viewStatus, 'unknown')
}

// 7. convoLastUserMs & stampAgeMs (fixes blank timestamp for recently started turns).
{
  assert.equal(
    convoLastUserMs([
      { role: 'user', ts: '2026-11-26T12:00:00.000Z' },
      { role: 'assistant', ts: '2026-11-26T12:01:00.000Z' },
      { role: 'user', ts: '2026-11-26T12:02:00.000Z', mid: true },
      { role: 'user', ts: '2026-11-26T12:03:00.000Z', gone: true },
    ]),
    Date.parse('2026-11-26T12:00:00.000Z'),
  )
  // 7 seconds ahead of a stale 30s tick clamps to 0 ("now"), not null (blank).
  assert.equal(stampAgeMs(1000000 + 7000, 1000000), 0)
  assert.equal(stampAgeMs(1000000 - 120000, 1000000), 120000)
  // Bogus far-future timestamp (>60s ahead) still returns null.
  assert.equal(stampAgeMs(1000000 + 120000, 1000000), null)
  assert.equal(stampAgeMs(undefined, 1000000), null)
}

// 8. resolveSubPeers (fix5 #5): completed subagent panels must NOT keep using
//    their frozen historical subPeers snapshot once polling stops; they follow
//    the parent conversation's live peers refresh (or drop links when offline).
{
  const subOld = { '10:20': { session_id: 's1', pane: 'w1:p2', title: 'Stale Sub Peer' } }
  const parentLive = { '10:20': { session_id: 's1', pane: 'w1:p2', title: 'Live Parent Peer' } }

  // While subagent is still polling, merges live subPeers with parentLive (parent wins on shared keys).
  assert.deepEqual(
    resolveSubPeers({
      going: true,
      subFailed: false,
      subPeers: { ...subOld, '30:40': { session_id: 's1', pane: 'w1:p3', title: 'Sub-Only Live' } },
      parentPeers: parentLive,
      parentOkAt: 1000,
      parentFailing: false,
    }),
    {
      '10:20': { session_id: 's1', pane: 'w1:p2', title: 'Live Parent Peer' },
      '30:40': { session_id: 's1', pane: 'w1:p3', title: 'Sub-Only Live' },
    },
  )

  // Once subagent stops polling (!going), frozen subPeers is discarded; follows parentLive only.
  assert.deepEqual(
    resolveSubPeers({
      going: false,
      subFailed: false,
      subPeers: { '99:88': { session_id: 's1', pane: 'w1:p9', title: 'Exited Frozen Peer' } },
      parentPeers: parentLive,
      parentOkAt: 2000,
      parentFailing: false,
    }),
    parentLive,
  )

  // When subagent stopped polling AND parent no longer has the peer (or parent is failing), no stale link remains.
  assert.equal(
    resolveSubPeers({
      going: false,
      subFailed: false,
      subPeers: subOld,
      parentPeers: undefined,
      parentOkAt: 2000,
      parentFailing: false,
    }),
    undefined,
  )
  assert.equal(
    resolveSubPeers({
      going: false,
      subFailed: false,
      subPeers: subOld,
      parentPeers: parentLive,
      parentOkAt: 2000,
      parentFailing: true,
    }),
    undefined,
  )
}

console.log('sideState: all tests passed')
