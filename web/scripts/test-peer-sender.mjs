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
  PEER_SLOT_MAX_AGE_MS,
  formatPeerPlain,
  peerClickHow,
  peerFresh,
  peerHref,
  peerLocation,
  peerRefKey,
  peerTargetKey,
  resolvePeerSender,
  sanitizePeerText,
  sidebarHerdrTitle,
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

// 4. Positive test: online sender returned by backend shows Corral name,
//    location, and /agents link (no sidebar matching on the frontend).
{
  const peers = {
    '530048:193322812': {
      session_id: 'sess-live',
      pane: 'w1:p2',
      title: 'corral-dev',
      host_name: 'devbox',
      persistence: 'herdr',
      mux_session: '',
      workspace: 'corral-dev',
    },
  }
  const r = resolvePeerSender({
    from: 'ai-68',
    peerPid: 530048,
    peerStart: 193322812,
    peers,
  })
  assert.deepEqual(r, {
    kind: 'live',
    name: 'corral-dev',
    where: 'devbox · herdr',
    sessionId: 'sess-live',
    pane: 'w1:p2',
    href: '/agents?s=sess-live&pane=w1%3Ap2',
  })
  assert.equal(peerClickHow({ metaKey: true, ctrlKey: false, altKey: false }), 'tab')
  assert.equal(peerClickHow({ metaKey: false, ctrlKey: false, altKey: true }), 'split')
  assert.equal(peerClickHow({ metaKey: false, ctrlKey: false, altKey: false }), undefined)
}

// 5. Scenario #3 (fix3): missing fields on frontend (missing session_id,
//    missing title, missing pid/start, or absent from backend peers) ALWAYS
//    falls back to original name + "not in Corral" with no link.
{
  const cases = [
    ['missing from peers', { from: 'ai-58', peerPid: 999, peerStart: 888, peers: {} }],
    ['missing pid/start', { from: 'ai-58', peers: { '999:888': { session_id: 's1', title: 'Agent' } } }],
    ['missing session_id', { from: 'ai-58', peerPid: 999, peerStart: 888, peers: { '999:888': { title: 'Agent', pane: 'w1:p1' } } }],
    ['missing title', { from: 'ai-58', peerPid: 999, peerStart: 888, peers: { '999:888': { session_id: 's1', pane: 'w1:p1' } } }],
    ['control-only title', { from: 'ai-58', peerPid: 999, peerStart: 888, peers: { '999:888': { session_id: 's1', title: '\x00\u202e' } } }],
    ['control-only session_id', { from: 'ai-58', peerPid: 999, peerStart: 888, peers: { '999:888': { session_id: '\x1b', title: 'Agent' } } }],
  ]
  for (const [label, input] of cases) {
    const r = resolvePeerSender(input)
    assert.deepEqual(r, { kind: 'unknown', name: 'ai-58' }, `${label}: expected unknown`)
  }

  // Raw HTML-like sender names are preserved as plain data (React escapes them).
  const rawHtml = resolvePeerSender({ from: '<script>alert(1)</script>', peers: {} })
  assert.deepEqual(rawHtml, { kind: 'unknown', name: '<script>alert(1)</script>' })
}

// 6. peerLocation & plain-text formatting for live and unknown peers.
assert.equal(
  peerLocation({ host_name: 'devbox', persistence: 'tmux', mux_session: 'main' }, 'main'),
  'devbox',
)
assert.equal(
  peerLocation({ host_name: 'devbox', persistence: 'herdr', display_name: '我的会话' }, 'corral'),
  'devbox · 我的会话',
)
{
  const say = {
    from: (n) => `来自 ${n}`,
    fromSession: (n) => `来自会话 ${n}`,
    notInCorral: '不在 Corral 中',
  }
  assert.equal(
    formatPeerPlain(
      { kind: 'live', name: 'Worker A', where: 'devbox · herdr', sessionId: 's1', pane: 'w1:p1', href: '/agents?s=s1&pane=w1%3Ap1' },
      say,
    ),
    '来自 Worker A (devbox · herdr)',
  )
  assert.equal(formatPeerPlain({ kind: 'unknown', name: 'ai-58' }, say), '来自会话 ai-58 (不在 Corral 中)')
}

// 7. Fix6 #1 [P1]: unified sidebar naming rule (sidebarHerdrTitle + peerTitles).
//    - Lineage child without custom pane label shows its own name (NOT workspace name).
//    - Custom pane label wins over workspace name and conversation/agent name.
//    - Plain workspace agent keeps the workspace name.
//    - resolvePeerSender reuses the sidebar title via peerTitles for the backend-matched pane.
{
  // Lineage child without custom label in workspace "Main Agent" -> own name "Security Reviewer"
  assert.equal(
    sidebarHerdrTitle({
      ws: 'Main Agent',
      label: '',
      agentName: 'claude',
      agent: 'claude',
      said: 'Security Reviewer',
      named: true,
      sameWsParent: true,
    }),
    'Security Reviewer',
  )
  // Lineage child without custom label and without named title -> agentName (NOT workspace "Main Agent")
  assert.equal(
    sidebarHerdrTitle({
      ws: 'Main Agent',
      label: '',
      agentName: 'Security Reviewer',
      agent: 'claude',
      said: 'repo-dir',
      named: false,
      sameWsParent: true,
    }),
    'Security Reviewer',
  )
  // Custom pane label wins for lineage child
  assert.equal(
    sidebarHerdrTitle({
      ws: 'Main Agent',
      label: 'Custom Reviewer',
      agentName: 'claude',
      agent: 'claude',
      said: 'Security Reviewer',
      named: true,
      sameWsParent: true,
    }),
    'Custom Reviewer',
  )
  // Custom pane label wins for plain workspace agent
  assert.equal(
    sidebarHerdrTitle({
      ws: 'Main Agent',
      label: 'Primary Worker',
      agentName: 'claude',
      agent: 'claude',
      said: 'Some Chat',
      named: true,
      sameWsParent: false,
    }),
    'Primary Worker',
  )
  // Plain workspace agent without custom label keeps workspace name
  assert.equal(
    sidebarHerdrTitle({
      ws: 'Main Agent',
      label: '',
      agentName: 'claude',
      agent: 'claude',
      said: 'Some Chat',
      named: true,
      sameWsParent: false,
    }),
    'Main Agent',
  )
  // Home workspace "~" falls back to said
  assert.equal(
    sidebarHerdrTitle({
      ws: '~',
      label: '',
      agentName: 'claude',
      agent: 'claude',
      said: 'Standalone Chat',
      named: true,
      sameWsParent: false,
    }),
    'Standalone Chat',
  )

  // resolvePeerSender prefers sidebar-computed peerTitles for backend-matched (session_id, pane)
  const peers = {
    '530048:193322812': {
      session_id: 'sess-live',
      pane: 'w1:p2',
      title: 'Main Agent',
      host_name: 'devbox',
      persistence: 'herdr',
      workspace: 'Main Agent',
    },
  }
  const peerTitles = new Map([[peerTargetKey('sess-live', 'w1:p2'), 'Security Reviewer']])
  const r = resolvePeerSender({
    from: 'ai-68',
    peerPid: 530048,
    peerStart: 193322812,
    peers,
    peerTitles,
    failing: false,
    okAt: 1_000_000,
    now: 1_000_000 + 5_000,
  })
  assert.deepEqual(r, {
    kind: 'live',
    name: 'Security Reviewer',
    where: 'devbox · herdr',
    sessionId: 'sess-live',
    pane: 'w1:p2',
    href: '/agents?s=sess-live&pane=w1%3Ap2',
  })

  // Guard: AgentsChat.tsx must call sidebarHerdrTitle and pass peerTitles to ConversationView
  const agentsChatSrc = readFileSync(fileURLToPath(new URL('../src/pages/AgentsChat.tsx', import.meta.url)), 'utf8')
  assert.match(agentsChatSrc, /sidebarHerdrTitle\(/, 'AgentsChat.tsx must reuse sidebarHerdrTitle')
  assert.match(agentsChatSrc, /peerTitles=\{peerTitles\}/, 'AgentsChat.tsx must pass peerTitles to ConversationView')
}

// 8. Fix6 #2 [P2]: failing refreshes or stale peer data (>10m) must stop linking
//    and fall back to the message's own name + "not in Corral".
{
  const peers = {
    '530048:193322812': {
      session_id: 'sess-live',
      pane: 'w1:p2',
      title: 'corral-dev',
      host_name: 'devbox',
      persistence: 'herdr',
    },
  }
  const t0 = 1_700_000_000_000
  assert.equal(PEER_SLOT_MAX_AGE_MS, 10 * 60 * 1000)

  // Healthy & within 10-minute slot limit -> live link
  assert.equal(peerFresh({ failing: false, okAt: t0, now: t0 + PEER_SLOT_MAX_AGE_MS }), true)
  const fresh = resolvePeerSender({
    from: 'ai-68',
    peerPid: 530048,
    peerStart: 193322812,
    peers,
    failing: false,
    okAt: t0,
    now: t0 + PEER_SLOT_MAX_AGE_MS,
  })
  assert.equal(fresh.kind, 'live')

  // Immediately when failing === true -> drop link, show raw name + "not in Corral"
  assert.equal(peerFresh({ failing: true, okAt: t0, now: t0 + 1000 }), false)
  const failingRes = resolvePeerSender({
    from: 'ai-68',
    peerPid: 530048,
    peerStart: 193322812,
    peers,
    failing: true,
    okAt: t0,
    now: t0 + 1000,
  })
  assert.deepEqual(failingRes, { kind: 'unknown', name: 'ai-68' })

  // Older than 10-minute slot limit even if failing wasn't flipped -> drop link
  assert.equal(peerFresh({ failing: false, okAt: t0, now: t0 + PEER_SLOT_MAX_AGE_MS + 1 }), false)
  const staleRes = resolvePeerSender({
    from: 'ai-68',
    peerPid: 530048,
    peerStart: 193322812,
    peers,
    failing: false,
    okAt: t0,
    now: t0 + PEER_SLOT_MAX_AGE_MS + 1,
  })
  assert.deepEqual(staleRes, { kind: 'unknown', name: 'ai-68' })

  // Zero okAt (never had a successful read) -> drop link
  assert.deepEqual(
    resolvePeerSender({
      from: 'ai-68',
      peerPid: 530048,
      peerStart: 193322812,
      peers,
      failing: false,
      okAt: 0,
      now: t0,
    }),
    { kind: 'unknown', name: 'ai-68' },
  )

  // Guard: conversation.ts must clear peers when entering failing state
  const convoSrc = readFileSync(fileURLToPath(new URL('../src/terminal/conversation.ts', import.meta.url)), 'utf8')
  assert.match(convoSrc, /failing:\s*true,\s*peers:\s*undefined/, 'conversation.ts must drop peers on failing refresh')
}

console.log('test-peer-sender: ok')
