import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { fileURLToPath } from 'node:url'
import ts from 'typescript'

const srcPath = fileURLToPath(new URL('../src/terminal/pushEventsLogic.ts', import.meta.url))
const src = readFileSync(srcPath, 'utf8')
const { outputText } = ts.transpileModule(src, {
  compilerOptions: { module: ts.ModuleKind.ESNext, target: ts.ScriptTarget.ES2022 },
})
const {
  AGENTS_COALESCE_MS,
  AGENTS_COVERED_POLL,
  AGENTS_HIDDEN_DEFAULT,
  AGENTS_VISIBLE_DEFAULT,
  CONVO_POLL_ACTIVE,
  CONVO_POLL_COVERED,
  CONVO_POLL_DEFAULT,
  CONVO_POLL_MORE,
  PUSH_PING_EVERY,
  PUSH_PONG_TIMEOUT,
  PUSH_RECONNECT_BACKOFFS,
  PushEventClient,
  SOCKET_OPEN,
  agentsNextPollDelay,
  allVisibleHostsCovered,
  convoNextPollDelay,
  createAgentsHintCoalescer,
  createConvoScheduler,
  createSerializedRefresher,
  matchesConvoHint,
  nextReconnectDelay,
  parsePushMessage,
} = await import(`data:text/javascript;base64,${Buffer.from(outputText).toString('base64')}`)

// Deterministic fake timer clock.
function makeClock() {
  let now = 0
  let nextId = 1
  const tasks = new Map()
  return {
    now: () => now,
    timers: {
      setTimeout(fn, ms) {
        const id = nextId++
        tasks.set(id, { at: now + ms, fn, ms })
        return id
      },
      clearTimeout(id) {
        tasks.delete(id)
      },
    },
    pendingCount: () => tasks.size,
    advance(ms) {
      const target = now + ms
      while (true) {
        let bestId = null
        let bestAt = Infinity
        for (const [id, t] of tasks) {
          if (t.at <= target && t.at < bestAt) {
            bestAt = t.at
            bestId = id
          }
        }
        if (bestId === null) break
        const t = tasks.get(bestId)
        tasks.delete(bestId)
        now = t.at
        t.fn()
      }
      now = target
    },
  }
}

function makeFakeSocket() {
  const sent = []
  const sock = {
    readyState: 0,
    sent,
    closed: false,
    onopen: null,
    onmessage: null,
    onerror: null,
    onclose: null,
    send(data) {
      sent.push(data)
    },
    close() {
      if (this.closed) return
      this.closed = true
      this.readyState = 3
      this.onclose?.()
    },
    emitOpen() {
      this.readyState = SOCKET_OPEN
      this.onopen?.()
    },
    emitMsg(obj) {
      this.onmessage?.({ data: typeof obj === 'string' ? obj : JSON.stringify(obj) })
    },
    emitDrop() {
      this.readyState = 3
      this.closed = true
      this.onclose?.()
    },
  }
  return sock
}

// 1. Pure helpers: backoff schedule, message parsing, matching, poll delays.
{
  assert.deepEqual([...PUSH_RECONNECT_BACKOFFS], [1000, 2000, 5000, 10000, 30000])
  assert.equal(nextReconnectDelay(0), 1000)
  assert.equal(nextReconnectDelay(1), 2000)
  assert.equal(nextReconnectDelay(2), 5000)
  assert.equal(nextReconnectDelay(3), 10000)
  assert.equal(nextReconnectDelay(4), 30000)
  assert.equal(nextReconnectDelay(10), 30000)

  assert.deepEqual(parsePushMessage('{"t":"convo","session":"s1","pane":"w1:p1"}'), {
    t: 'convo',
    session: 's1',
    pane: 'w1:p1',
  })
  assert.deepEqual(parsePushMessage('{"t":"agents","host":7}'), { t: 'agents', host: 7 })
  assert.deepEqual(parsePushMessage('{"t":"cover","hosts":[5,2]}'), { t: 'cover', hosts: [2, 5] })
  assert.equal(parsePushMessage('{"t":"bad"}'), null)
  assert.equal(parsePushMessage('not-json'), null)

  assert.equal(matchesConvoHint('s1', 'w1:p1', 's1', 'w1:p1'), true)
  assert.equal(matchesConvoHint('s1', 'w1:p1', 's1', 'w1:p2'), false)
  assert.equal(matchesConvoHint('s1', 'w1:p1', 's2', 'w1:p1'), false)
  assert.equal(matchesConvoHint('s1', undefined, 's1', '%3'), true)
  assert.equal(matchesConvoHint('s1', undefined, 's2', '%3'), false)

  assert.equal(convoNextPollDelay({ covered: false, active: false, more: false }), CONVO_POLL_DEFAULT)
  assert.equal(convoNextPollDelay({ covered: false, active: true, more: false }), CONVO_POLL_ACTIVE)
  assert.equal(convoNextPollDelay({ covered: true, active: false, more: false }), CONVO_POLL_COVERED)
  assert.equal(convoNextPollDelay({ covered: true, active: true, more: false }), CONVO_POLL_COVERED)
  assert.equal(convoNextPollDelay({ covered: true, active: false, more: true }), CONVO_POLL_MORE)

  assert.equal(
    agentsNextPollDelay({ defaultEvery: AGENTS_VISIBLE_DEFAULT, hidden: false, allCovered: false }),
    AGENTS_VISIBLE_DEFAULT,
  )
  assert.equal(
    agentsNextPollDelay({ defaultEvery: AGENTS_VISIBLE_DEFAULT, hidden: false, allCovered: true }),
    AGENTS_COVERED_POLL,
  )
  assert.equal(
    agentsNextPollDelay({ defaultEvery: 3000, hidden: false, allCovered: true }),
    AGENTS_COVERED_POLL,
  )
  assert.equal(
    agentsNextPollDelay({ defaultEvery: AGENTS_VISIBLE_DEFAULT, hidden: true, allCovered: true }),
    AGENTS_HIDDEN_DEFAULT,
  )
}

// 2. PushEventClient: watch union, cover updates, hidden suppression, disconnect + backoff reconnect.
{
  const clock = makeClock()
  let hidden = false
  const sockets = []
  const client = new PushEventClient({
    createSocket: () => {
      const s = makeFakeSocket()
      sockets.push(s)
      return s
    },
    isHidden: () => hidden,
    timers: clock.timers,
    pingEveryMs: 0,
  })

  const convoHits = []
  const agentsHits = []
  let stateChanges = 0
  client.onConvo((s, p) => convoHits.push(`${s}:${p}`))
  client.onAgents((h) => agentsHits.push(h))
  client.onStateChange(() => stateChanges++)

  const srcA = Symbol('a')
  const srcB = Symbol('b')
  const unwatchA = client.watchHosts(srcA, [2, 1])
  client.setActive(true)
  assert.equal(sockets.length, 1)

  sockets[0].emitOpen()
  assert.equal(client.isConnected(), true)
  assert.deepEqual(sockets[0].sent, ['{"t":"watch","hosts":[1,2]}'])

  // Second source adds host 3 -> sends updated union [1,2,3].
  const unwatchB = client.watchHosts(srcB, [3, 2])
  assert.deepEqual(sockets[0].sent.at(-1), '{"t":"watch","hosts":[1,2,3]}')

  // Cover arrives -> host 1 & 2 covered, host 3 not covered.
  sockets[0].emitMsg({ t: 'cover', hosts: [1, 2] })
  assert.equal(client.isHostCovered(1), true)
  assert.equal(client.isHostCovered(2), true)
  assert.equal(client.isHostCovered(3), false)
  assert.equal(client.areAllHostsCovered([1, 2]), true)
  assert.equal(client.areAllHostsCovered([1, 2, 3]), false)

  // Visible page receives convo & agents hints.
  sockets[0].emitMsg({ t: 'convo', session: 'sess-1', pane: 'w1:p1' })
  sockets[0].emitMsg({ t: 'agents', host: 1 })
  assert.deepEqual(convoHits, ['sess-1:w1:p1'])
  assert.deepEqual(agentsHits, [1])

  // Hidden page suppresses convo & agents hints (no extra requests while hidden),
  // while still processing cover state updates.
  hidden = true
  sockets[0].emitMsg({ t: 'convo', session: 'sess-1', pane: 'w1:p1' })
  sockets[0].emitMsg({ t: 'agents', host: 1 })
  sockets[0].emitMsg({ t: 'cover', hosts: [1, 2, 3] })
  assert.deepEqual(convoHits, ['sess-1:w1:p1'])
  assert.deepEqual(agentsHits, [1])
  assert.equal(client.isHostCovered(3), true)
  hidden = false

  // Disconnect immediately clears coverage and triggers state notification,
  // then reconnects with exact backoff sequence: 1s, 2s, 5s, 10s, 30s, 30s.
  const beforeDropChanges = stateChanges
  sockets[0].emitDrop()
  assert.equal(client.isConnected(), false)
  assert.equal(client.isHostCovered(1), false)
  assert.equal(stateChanges > beforeDropChanges, true)

  const expectedBackoffs = [1000, 2000, 5000, 10000, 30000, 30000]
  for (let i = 0; i < expectedBackoffs.length; i++) {
    const delay = expectedBackoffs[i]
    const prevCount = sockets.length
    clock.advance(delay - 1)
    assert.equal(sockets.length, prevCount, `reconnected too early on attempt ${i}`)
    clock.advance(1)
    assert.equal(sockets.length, prevCount + 1, `did not reconnect at ${delay}ms on attempt ${i}`)
    if (i < expectedBackoffs.length - 1) {
      sockets.at(-1).emitDrop()
    }
  }

  // Successful reconnect re-sends current watch list and resets backoff to 1000ms.
  sockets.at(-1).emitOpen()
  assert.deepEqual(sockets.at(-1).sent, ['{"t":"watch","hosts":[1,2,3]}'])
  sockets.at(-1).emitDrop()
  const countBeforeResetCheck = sockets.length
  clock.advance(999)
  assert.equal(sockets.length, countBeforeResetCheck)
  clock.advance(1)
  assert.equal(sockets.length, countBeforeResetCheck + 1)

  unwatchA()
  unwatchB()
  client.setActive(false)
}

// 3. ConvoScheduler: immediate push fetch, strict non-concurrency with queued
//    follow-up, coverage interval switching, and immediate revert on disconnect.
{
  const clock = makeClock()
  let covered = true
  let hidden = false
  let activeInFlight = 0
  let maxConcurrent = 0
  const calls = []
  let deferred = null

  const sched = createConvoScheduler({
    fetchOnce(src) {
      calls.push({ src, at: clock.now() })
      activeInFlight++
      maxConcurrent = Math.max(maxConcurrent, activeInFlight)
      return new Promise((resolve) => {
        deferred = (res = { active: false, more: false }) => {
          activeInFlight--
          deferred = null
          resolve(res)
        }
      })
    },
    isCovered: () => covered,
    isHidden: () => hidden,
    timers: clock.timers,
  })

  // Start -> initial request ('other').
  sched.start()
  assert.equal(calls.length, 1)
  assert.equal(calls[0].src, 'other')

  // Two push hints arrive while request #1 is in flight -> MUST NOT run concurrently!
  sched.onPushHint()
  sched.onPushHint()
  assert.equal(calls.length, 1)
  assert.equal(maxConcurrent, 1)

  // Resolve request #1 -> immediately triggers exactly ONE follow-up with src='push'.
  deferred({ active: false, more: false })
  await Promise.resolve()
  await Promise.resolve()
  assert.equal(calls.length, 2)
  assert.equal(calls[1].src, 'push')
  assert.equal(maxConcurrent, 1)

  // Resolve request #2 while covered -> next poll scheduled at 10000ms.
  deferred({ active: true, more: false })
  await Promise.resolve()
  await Promise.resolve()
  clock.advance(9999)
  assert.equal(calls.length, 2)
  clock.advance(1)
  assert.equal(calls.length, 3)
  assert.equal(calls[2].src, 'poll')

  // Resolve request #3 while covered -> idle, 10000ms timer pending.
  deferred({ active: false, more: false })
  await Promise.resolve()
  await Promise.resolve()

  // Push hint arrives while idle -> IMMEDIATELY triggers src='push' without waiting 10s!
  clock.advance(500)
  sched.onPushHint()
  assert.equal(calls.length, 4)
  assert.equal(calls[3].src, 'push')

  // Resolve request #4 while covered -> 10000ms timer pending.
  deferred({ active: false, more: false })
  await Promise.resolve()
  await Promise.resolve()

  // Push channel disconnects -> covered becomes false -> immediately shortens timer to 2000ms!
  covered = false
  sched.onCoverageChange()
  clock.advance(1999)
  assert.equal(calls.length, 4)
  clock.advance(1)
  assert.equal(calls.length, 5)
  assert.equal(calls[4].src, 'poll')

  // Hidden page ignores push hints.
  deferred({ active: false, more: false })
  await Promise.resolve()
  await Promise.resolve()
  hidden = true
  sched.onPushHint()
  assert.equal(calls.length, 5)

  sched.stop()
}

// 4. Agents hint coalescer: merges multiple hints within 300ms into 1 reload;
//    suppresses reloads while hidden.
{
  const clock = makeClock()
  let hidden = false
  let flushes = 0
  const coalescer = createAgentsHintCoalescer({
    delayMs: AGENTS_COALESCE_MS,
    isHidden: () => hidden,
    onFlush: () => flushes++,
    timers: clock.timers,
  })

  coalescer.trigger()
  clock.advance(50)
  coalescer.trigger()
  clock.advance(100)
  coalescer.trigger()
  clock.advance(149)
  assert.equal(flushes, 0)
  clock.advance(1)
  assert.equal(flushes, 1)

  // Further advance causes no spurious second flush.
  clock.advance(1000)
  assert.equal(flushes, 1)

  // Hidden page ignores triggers.
  hidden = true
  coalescer.trigger()
  clock.advance(500)
  assert.equal(flushes, 1)

  // If page hides before pending 300ms timer fires, flush is suppressed.
  hidden = false
  coalescer.trigger()
  clock.advance(150)
  hidden = true
  clock.advance(200)
  assert.equal(flushes, 1)

  coalescer.cancel()
}

// 5. Regression A.4: Heartbeat pong timeout detects half-open sockets, drops
//    coverage immediately, and triggers reconnect backoff.
{
  const clock = makeClock()
  const sockets = []
  const client = new PushEventClient({
    createSocket: () => {
      const s = makeFakeSocket()
      sockets.push(s)
      return s
    },
    isHidden: () => false,
    timers: clock.timers,
    pingEveryMs: PUSH_PING_EVERY,
    pongTimeoutMs: PUSH_PONG_TIMEOUT,
  })
  let stateNotifies = 0
  client.onStateChange(() => stateNotifies++)
  client.watchHosts(Symbol('hb'), [1])
  client.setActive(true)
  sockets[0].emitOpen()
  sockets[0].emitMsg({ t: 'cover', hosts: [1] })
  assert.equal(client.isConnected(), true)
  assert.equal(client.isHostCovered(1), true)

  // First ping answered with pong -> stays connected & covered.
  clock.advance(PUSH_PING_EVERY)
  assert.equal(sockets[0].sent.at(-1), '{"t":"ping"}')
  clock.advance(2000)
  sockets[0].emitMsg({ t: 'pong' })
  clock.advance(PUSH_PONG_TIMEOUT)
  assert.equal(client.isConnected(), true)
  assert.equal(client.isHostCovered(1), true)

  // Second ping answered by any non-pong message -> also clears timeout.
  clock.advance(PUSH_PING_EVERY - 2000 - PUSH_PONG_TIMEOUT)
  assert.equal(sockets[0].sent.filter((s) => s === '{"t":"ping"}').length, 2)
  clock.advance(3000)
  sockets[0].emitMsg({ t: 'agents', host: 1 })
  clock.advance(PUSH_PONG_TIMEOUT)
  assert.equal(client.isConnected(), true)

  // Third ping gets NO pong / NO message -> times out at PUSH_PONG_TIMEOUT,
  // drops coverage immediately, closes stale socket, and schedules reconnect.
  clock.advance(PUSH_PING_EVERY - 3000 - PUSH_PONG_TIMEOUT)
  assert.equal(sockets[0].sent.filter((s) => s === '{"t":"ping"}').length, 3)
  const beforeNotifies = stateNotifies
  clock.advance(PUSH_PONG_TIMEOUT - 1)
  assert.equal(client.isConnected(), true)
  assert.equal(client.isHostCovered(1), true)
  clock.advance(1)
  assert.equal(client.isConnected(), false)
  assert.equal(client.isHostCovered(1), false)
  assert.equal(sockets[0].closed, true)
  assert.equal(stateNotifies > beforeNotifies, true)

  // Reconnect triggers after 1s initial backoff.
  clock.advance(999)
  assert.equal(sockets.length, 1)
  clock.advance(1)
  assert.equal(sockets.length, 2)

  client.setActive(false)
}

// 6. Regression A.5: createSerializedRefresher (used by Hosts.tsx) keeps at
//    most 1 request in flight and coalesces burst triggers into 1 follow-up.
{
  let inFlight = 0
  let maxConcurrent = 0
  let started = 0
  let release = null
  const refresher = createSerializedRefresher(() => {
    started++
    inFlight++
    maxConcurrent = Math.max(maxConcurrent, inFlight)
    return new Promise((resolve) => {
      release = () => {
        inFlight--
        release = null
        resolve()
      }
    })
  })

  const p1 = refresher.trigger()
  assert.equal(started, 1)
  assert.equal(refresher.inFlight(), true)
  // 20 triggers while #1 is in flight -> MUST NOT overlap!
  for (let i = 0; i < 20; i++) void refresher.trigger()
  assert.equal(started, 1)
  assert.equal(maxConcurrent, 1)

  // Finish #1 -> runs exactly ONE follow-up (#2).
  release()
  await Promise.resolve()
  await Promise.resolve()
  assert.equal(started, 2)
  assert.equal(maxConcurrent, 1)
  assert.equal(refresher.inFlight(), true)

  // Finish #2 -> settles cleanly with no extra runs.
  release()
  await p1
  assert.equal(started, 2)
  assert.equal(refresher.inFlight(), false)
  refresher.stop()
}

// 7. Regression A.6: List polling slows to 60s ONLY when ALL visible hosts
//    (and session hosts) are covered; any single uncovered host keeps original interval.
{
  const coveredSet = new Set([1])
  const isCov = (id) => coveredSet.has(id)

  // Before /api/hosts loads: never slow down even if current session host is covered.
  assert.equal(
    allVisibleHostsCovered({
      connected: true,
      visibleHostsLoaded: false,
      visibleHosts: [],
      sessionHosts: [1],
      isHostCovered: isCov,
    }),
    false,
  )

  // Mixed environment: host 1 (has session) is covered, host 2 (no session yet) is NOT covered -> MUST NOT slow!
  assert.equal(
    allVisibleHostsCovered({
      connected: true,
      visibleHostsLoaded: true,
      visibleHosts: [1, 2],
      sessionHosts: [1],
      isHostCovered: isCov,
    }),
    false,
  )

  // Both visible hosts covered -> slows to 60s!
  coveredSet.add(2)
  assert.equal(
    allVisibleHostsCovered({
      connected: true,
      visibleHostsLoaded: true,
      visibleHosts: [1, 2],
      sessionHosts: [1],
      isHostCovered: isCov,
    }),
    true,
  )

  // Session on an additional uncovered host 3 -> MUST NOT slow!
  assert.equal(
    allVisibleHostsCovered({
      connected: true,
      visibleHostsLoaded: true,
      visibleHosts: [1, 2],
      sessionHosts: [1, 3],
      isHostCovered: isCov,
    }),
    false,
  )

  // Disconnected push channel -> MUST NOT slow!
  assert.equal(
    allVisibleHostsCovered({
      connected: false,
      visibleHostsLoaded: true,
      visibleHosts: [1, 2],
      sessionHosts: [1],
      isHostCovered: isCov,
    }),
    false,
  )
}

console.log('test-push-events.mjs: all assertions passed')
