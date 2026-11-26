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
const { buildAgentGroups, isLiveBlocked, listOrder } = mod

function row(partial) {
  return {
    where: '',
    status: 'idle',
    current: false,
    onPick: () => {},
    ...partial,
  }
}

// 1. Lost rows group by lastStatus (working / idle / todo), and only lost rows
// without lastStatus fall into Other; non-lost rows behave identically to before.
{
  const rows = [
    row({ key: 'liveBlocked', title: 'Live Blocked', status: 'blocked', since: 700 }),
    row({ key: 'liveDone', title: 'Live Done', status: 'done', since: 650 }),
    row({ key: 'liveWork', title: 'Live Work', status: 'working', since: 500 }),
    row({ key: 'liveIdle', title: 'Live Idle', status: 'idle', since: 400 }),
    row({ key: 'liveNoSince', title: 'Live No Since', status: 'idle' }),
    row({ key: 'liveUnknown', title: 'Live Unknown', status: 'unknown' }),
    row({ key: 'lostWork', title: 'Lost Work', status: 'unknown', lost: true, lastStatus: 'working', since: 450 }),
    row({ key: 'lostIdle', title: 'Lost Idle', status: 'unknown', lost: true, lastStatus: 'idle', since: 350 }),
    row({ key: 'lostIdleNoSince', title: 'Lost Idle No Since', status: 'unknown', lost: true, lastStatus: 'idle' }),
    row({ key: 'lostBlocked', title: 'Lost Blocked', status: 'unknown', lost: true, lastStatus: 'blocked', since: 300 }),
    row({ key: 'lostDone', title: 'Lost Done', status: 'unknown', lost: true, lastStatus: 'done', since: 250 }),
    row({ key: 'lostNone', title: 'Lost No Last', status: 'unknown', lost: true, since: 600 }),
  ]

  for (const nestChildren of [true, false]) {
    const groups = buildAgentGroups(rows, { group: 'status', sort: 'recent', nestChildren })
    const byKind = new Map(groups.map((g) => [g.kind, g]))

    // Live blocked stays ahead of done / stale-todo; lost rows do not inflate live counts.
    assert.deepEqual(
      byKind.get('todo').rows.map((r) => r.key),
      ['liveBlocked', 'liveDone', 'lostBlocked', 'lostDone'],
      `nest=${nestChildren} todo`,
    )
    assert.equal(byKind.get('todo').blockedCount, 1, `nest=${nestChildren} todo blockedCount`)
    assert.equal(byKind.get('todo').doneCount, 1, `nest=${nestChildren} todo doneCount`)

    // lost + last=working goes into Working alongside live working, sorted by since.
    assert.deepEqual(
      byKind.get('working').rows.map((r) => r.key),
      ['liveWork', 'lostWork'],
      `nest=${nestChildren} working`,
    )
    assert.equal(byKind.get('working').workingCount, 1, `nest=${nestChildren} workingCount`)

    // lost + last=idle goes into Idle (with or without since).
    assert.deepEqual(
      byKind.get('idle').rows.map((r) => r.key),
      ['liveIdle', 'lostIdle', 'lostIdleNoSince'],
      `nest=${nestChildren} idle`,
    )

    // Only lost without lastStatus (plus non-lost rows with no since) go into Other.
    assert.deepEqual(
      byKind.get('other').rows.map((r) => r.key),
      ['lostNone', 'liveNoSince', 'liveUnknown'],
      `nest=${nestChildren} other`,
    )
  }

  // Visible order follows the same grouping.
  assert.deepEqual(
    listOrder(rows, { group: 'status', sort: 'recent', nestChildren: true }).map((r) => r.key),
    ['liveBlocked', 'liveDone', 'lostBlocked', 'lostDone', 'liveWork', 'lostWork', 'liveIdle', 'lostIdle', 'lostIdleNoSince', 'lostNone', 'liveNoSince', 'liveUnknown'],
  )
}

// 2. Lost rows never trigger live-blocked promotion in Project/Session modes.
{
  const rows = [
    row({ key: 'lostBlocked', title: 'Lost Blocked', status: 'unknown', lost: true, lastStatus: 'blocked', since: 300, projectKey: '/p/a', sessionGroup: 'h · default' }),
    row({ key: 'liveIdle', title: 'Live Idle', status: 'idle', since: 200, projectKey: '/p/a', sessionGroup: 'h · default' }),
  ]
  assert.equal(isLiveBlocked(rows[0]), false)

  for (const group of ['project', 'session']) {
    const groups = buildAgentGroups(rows, { group, sort: 'recent', nestChildren: true })
    assert.equal(groups.some((g) => g.kind === 'todo'), false, `mode=${group}`)
    assert.equal(groups[0].blockedCount, 0, `mode=${group}`)
    assert.deepEqual(groups[0].rows.map((r) => r.key), ['lostBlocked', 'liveIdle'], `mode=${group}`)
  }
}

// 3. Non-lost behavior remains completely unchanged.
{
  const rows = [
    row({ key: 'b', title: 'Blocked', status: 'blocked', since: 100 }),
    row({ key: 'd', title: 'Done', status: 'done', since: 200 }),
    row({ key: 'w', title: 'Working', status: 'working', since: 300 }),
    row({ key: 'i', title: 'Idle', status: 'idle', since: 400 }),
    row({ key: 'o', title: 'Idle No Time', status: 'idle' }),
  ]
  const groups = buildAgentGroups(rows, { group: 'status', sort: 'recent', nestChildren: true })
  assert.deepEqual(
    groups.map((g) => [g.kind, g.rows.map((r) => r.key)]),
    [
      ['todo', ['b', 'd']],
      ['working', ['w']],
      ['idle', ['i']],
      ['other', ['o']],
    ],
  )
  assert.equal(isLiveBlocked(rows[0]), true)
}

console.log('agentLost: all tests passed')
