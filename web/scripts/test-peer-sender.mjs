import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { fileURLToPath } from 'node:url'
import ts from 'typescript'

const srcPath = fileURLToPath(new URL('../src/terminal/peerSender.ts', import.meta.url))
const src = readFileSync(srcPath, 'utf8')
const { outputText } = ts.transpileModule(src, {
  compilerOptions: { module: ts.ModuleKind.ESNext, target: ts.ScriptTarget.ES2022 },
})
const {
  formatPeerPlain,
  peerClickHow,
  peerHref,
  peerLocation,
  peerRefKey,
  resolvePeerSender,
  resolvePeerToolTarget,
  sanitizePeerText,
} = await import(`data:text/javascript;base64,${Buffer.from(outputText).toString('base64')}`)

// 1. peerRefKey requires positive finite pid and start.
assert.equal(peerRefKey(530048, 193322812), '530048:193322812')
assert.equal(peerRefKey(0, 193322812), '')
assert.equal(peerRefKey(530048, 0), '')
assert.equal(peerRefKey(undefined, 193322812), '')
assert.equal(peerRefKey(-1, 100), '')

// 2. peerHref encodes special chars and never emits unsafe protocols.
assert.equal(peerHref('sess-1', 'w1:p2'), '/agents?s=sess-1&pane=w1%3Ap2')
assert.equal(peerHref('sess-2'), '/agents?s=sess-2')
assert.equal(peerHref(''), '')
assert.equal(peerHref('sess&evil=1', 'w1:p1&x=2'), '/agents?s=sess%26evil%3D1&pane=w1%3Ap1%26x%3D2')

// 3. sanitizePeerText strips ASCII control chars and BiDi overrides.
assert.equal(sanitizePeerText('  \x1b\u202ehello\x00world  '), 'helloworld')

// 4. Live matched peer uses exact live sidebar title, location, href, and onPick.
{
  let picked = null
  const liveByKey = new Map([
    [
      '1||w1:p2',
      {
        key: '1||w1:p2',
        title: 'corral-dev',
        where: 'fix cross-session',
        sessionGroup: 'devbox · herdr',
        hostName: 'devbox',
        sessionLabel: 'herdr',
        sessionId: 'sess-live',
        pane: 'w1:p2',
        program: 'claude',
        onPick: (how) => {
          picked = how ?? 'plain'
        },
      },
    ],
  ])
  const peers = {
    '530048:193322812': {
      key: '1||w1:p2',
      session_id: 'sess-live',
      host_id: 1,
      host_name: 'devbox',
      persistence: 'herdr',
      mux_session: '',
      pane: 'w1:p2',
      workspace: 'corral-dev',
      title: 'stale-title',
      program: 'claude',
      alive: true,
    },
  }
  const r = resolvePeerSender({
    from: 'ai-68',
    peerPid: 530048,
    peerStart: 193322812,
    peers,
    liveByKey,
  })
  assert.equal(r.kind, 'live')
  assert.equal(r.name, 'corral-dev')
  assert.equal(r.where, 'devbox · herdr')
  assert.equal(r.href, '/agents?s=sess-live&pane=w1%3Ap2')
  r.onPick(peerClickHow({ metaKey: true, ctrlKey: false, altKey: false }))
  assert.equal(picked, 'tab')
  r.onPick(peerClickHow({ metaKey: false, ctrlKey: false, altKey: true }))
  assert.equal(picked, 'split')
  r.onPick(peerClickHow({ metaKey: false, ctrlKey: false, altKey: false }))
  assert.equal(picked, 'plain')
}

// 5. Exited peer (alive=false or absent from liveByKey) keeps remembered Corral
//    title & location with no href/link.
{
  const peers = {
    '530048:193322812': {
      key: '1||w1:p2',
      host_id: 1,
      host_name: 'devbox',
      persistence: 'herdr',
      mux_session: 'work',
      pane: 'w1:p2',
      workspace: 'corral-dev',
      title: 'corral-dev',
      program: 'claude',
      alive: false,
    },
  }
  const r = resolvePeerSender({
    from: 'ai-68',
    peerPid: 530048,
    peerStart: 193322812,
    peers,
    liveByKey: new Map(),
  })
  assert.equal(r.kind, 'exited')
  assert.equal(r.name, 'corral-dev')
  assert.equal(r.where, 'devbox · herdr work')
  assert.equal('href' in r, false)
}

// 6. Unmatched peer (missing from peers or unverified pid/start) falls back to
//    original name as unknown.
{
  const r1 = resolvePeerSender({
    from: 'ai-58',
    peerPid: 999,
    peerStart: 888,
    peers: {},
    liveByKey: new Map(),
  })
  assert.deepEqual(r1, { kind: 'unknown', name: 'ai-58' })

  const r2 = resolvePeerSender({
    from: '<script>alert(1)</script>',
    peers: {},
    liveByKey: new Map(),
  })
  assert.equal(r2.kind, 'unknown')
  assert.equal(r2.name, '<script>alert(1)</script>')
}

// 7. peerLocation deduplicates tmux session name when it equals title.
assert.equal(
  peerLocation({ host_name: 'devbox', persistence: 'tmux', mux_session: 'main' }, 'main'),
  'devbox',
)
assert.equal(
  peerLocation({ host_name: 'devbox', persistence: 'herdr', display_name: '我的会话' }, 'corral'),
  'devbox · 我的会话',
)

// 8. SendMessage tool target resolution + plain formatting.
{
  const peers = {
    '10:20': {
      key: '1||w1:p1',
      host_name: 'devbox',
      persistence: 'herdr',
      title: 'Worker A',
      alive: false,
    },
  }
  const hit = resolvePeerToolTarget({ name: 'SendMessage', title: 'ai-10', peer_pid: 10, peer_start: 20 }, peers)
  assert.equal(hit?.kind, 'exited')
  assert.equal(hit?.name, 'Worker A')
  assert.equal(resolvePeerToolTarget({ name: 'Bash', title: 'ls', peer_pid: 10, peer_start: 20 }, peers), null)

  const say = {
    from: (n) => `来自 ${n}`,
    fromSession: (n) => `来自会话 ${n}`,
    notInCorral: '不在 Corral 中',
    exited: '已退出',
  }
  assert.equal(formatPeerPlain(hit, say), '来自 Worker A (devbox · herdr · 已退出)')
  assert.equal(formatPeerPlain({ kind: 'unknown', name: 'ai-58' }, say), '来自会话 ai-58 (不在 Corral 中)')
}

console.log('test-peer-sender: ok')
