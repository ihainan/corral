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
  buildPeerTitles,
  formatPeerPlain,
  nextConvoPeers,
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
  })
  assert.deepEqual(r, {
    kind: 'live',
    name: 'Security Reviewer',
    where: 'devbox · herdr',
    sessionId: 'sess-live',
    pane: 'w1:p2',
    href: '/agents?s=sess-live&pane=w1%3Ap2',
  })

  // Guard: AgentsChat.tsx must call sidebarHerdrTitle, buildPeerTitles, and pass peerTitles to ConversationView
  const agentsChatSrc = readFileSync(fileURLToPath(new URL('../src/pages/AgentsChat.tsx', import.meta.url)), 'utf8')
  assert.match(agentsChatSrc, /sidebarHerdrTitle\(/, 'AgentsChat.tsx must reuse sidebarHerdrTitle')
  assert.match(agentsChatSrc, /buildPeerTitles\(/, 'AgentsChat.tsx must build peerTitles via buildPeerTitles')
  assert.match(agentsChatSrc, /peerTitles=\{peerTitles\}/, 'AgentsChat.tsx must pass peerTitles to ConversationView')
}

// 8. Fix7 #1 [P1]: healthy idle conversation keeps its link past 10 minutes of
//    unchanged polls (no frontend okAt age cutoff); failing drops it; a poll
//    whose only change is `peers` produces a new reference and updates the sender.
{
  const initialPeers = {
    '530048:193322812': {
      session_id: 'sess-live',
      pane: 'w1:p2',
      title: 'corral-dev',
      host_name: 'devbox',
      persistence: 'herdr',
    },
  }
  const t0 = 1_700_000_000_000
  const elevenMinLater = t0 + 11 * 60 * 1000

  // 11 minutes of successful unchanged polls: nextConvoPeers reuses the same
  // reference (so conversation.ts keeps prior state + old okAt), yet the link stays live.
  let statePeers = nextConvoPeers(undefined, initialPeers)
  assert.equal(statePeers, initialPeers)
  const unchangedClone = JSON.parse(JSON.stringify(initialPeers))
  const afterUnchangedPoll = nextConvoPeers(statePeers, unchangedClone)
  assert.equal(afterUnchangedPoll, statePeers, 'unchanged poll must keep same peers reference')
  assert.equal(peerFresh({ failing: false, okAt: t0, now: elevenMinLater }), true)
  const after11m = resolvePeerSender({
    from: 'ai-68',
    peerPid: 530048,
    peerStart: 193322812,
    peers: afterUnchangedPoll,
    failing: false,
    okAt: t0,
    now: elevenMinLater,
  })
  assert.equal(after11m.kind, 'live')
  assert.equal(after11m.name, 'corral-dev')

  // Immediately when failing === true -> drop link, show raw name + "not in Corral"
  assert.equal(peerFresh({ failing: true }), false)
  const failingRes = resolvePeerSender({
    from: 'ai-68',
    peerPid: 530048,
    peerStart: 193322812,
    peers: statePeers,
    failing: true,
  })
  assert.deepEqual(failingRes, { kind: 'unknown', name: 'ai-68' })

  // Poll where ONLY `peers` changed (sender renamed): must NOT be treated as unchanged
  const renamedPeers = {
    '530048:193322812': {
      ...initialPeers['530048:193322812'],
      title: 'corral-renamed',
    },
  }
  const updatedPeers = nextConvoPeers(statePeers, renamedPeers)
  assert.notEqual(updatedPeers, statePeers, 'peers-only change must return a new reference')
  const updatedSender = resolvePeerSender({
    from: 'ai-68',
    peerPid: 530048,
    peerStart: 193322812,
    peers: updatedPeers,
    failing: false,
  })
  assert.equal(updatedSender.kind, 'live')
  assert.equal(updatedSender.name, 'corral-renamed')

  // Poll where server drops the expired/exited peer (empty peers map): must update to undefined
  const clearedPeers = nextConvoPeers(updatedPeers, {})
  assert.equal(clearedPeers, undefined)
  assert.deepEqual(
    resolvePeerSender({
      from: 'ai-68',
      peerPid: 530048,
      peerStart: 193322812,
      peers: clearedPeers,
      failing: false,
    }),
    { kind: 'unknown', name: 'ai-68' },
  )

  // Guard: conversation.ts uses nextConvoPeers, checks c.peers === peers in unchanged guard,
  // and clears peers on failing refresh; Conversation.tsx does not pass okAt to resolvePeerSender.
  const convoSrc = readFileSync(fileURLToPath(new URL('../src/terminal/conversation.ts', import.meta.url)), 'utf8')
  assert.match(convoSrc, /nextConvoPeers\(c\.peers,\s*a\.peers\)/, 'conversation.ts must use nextConvoPeers')
  assert.match(convoSrc, /c\.peers === peers/, 'conversation.ts must compare c.peers === peers in unchanged guard')
  assert.match(convoSrc, /failing:\s*true,\s*peers:\s*undefined/, 'conversation.ts must drop peers on failing refresh')
  const convoTsxSrc = readFileSync(fileURLToPath(new URL('../src/terminal/Conversation.tsx', import.meta.url)), 'utf8')
  const peerHeadBlock = convoTsxSrc.slice(convoTsxSrc.indexOf('function PeerSenderHead'), convoTsxSrc.indexOf('function PeerSenderHead') + 400)
  assert.doesNotMatch(peerHeadBlock, /\bokAt\b/, 'PeerSenderHead must not gate links on okAt age')
}

// 9. Fix7 #2 & #3 [P2]: buildPeerTitles includes tmux entries (with sidebar's own
//    name) AND registers every deduped connection's (session_id, pane) key for
//    both herdr and tmux rows.
{
  const herdrKey = (s, pane) => `${s.host_id}|${!s.mux_session || s.mux_session === 'default' ? '' : s.mux_session}|${pane}`
  const tmuxKey = (s) => `${s.host_id}|tmux|${s.mux_session ?? ''}`

  const allSessions = [
    // Two connections to the same herdr server on host 10 ("" and "default" normalize to the same server)
    { id: 'h-conn-1', host_id: 10, persistence: 'herdr', mux_session: '' },
    { id: 'h-conn-2', host_id: 10, persistence: 'herdr', mux_session: 'default' },
    // Different herdr server on same host (must NOT get h-conn-1's title)
    { id: 'h-other-mux', host_id: 10, persistence: 'herdr', mux_session: 'work' },
    // Different host (must NOT get h-conn-1's title)
    { id: 'h-other-host', host_id: 20, persistence: 'herdr', mux_session: '' },
    // Two connections to the same tmux session on host 30
    { id: 't-conn-1', host_id: 30, persistence: 'tmux', mux_session: 'corral-admin' },
    { id: 't-conn-2', host_id: 30, persistence: 'tmux', mux_session: 'corral-admin' },
    // Separate unvisited tmux session on host 30
    { id: 't-unvisited', host_id: 30, persistence: 'tmux', mux_session: 'ops' },
  ]

  const peerTitles = buildPeerTitles(
    [
      // Deduped herdr lineage child (sidebar selected h-conn-1)
      { key: '10||w1:p2', sessionId: 'h-conn-1', pane: 'w1:p2', isHerdr: true, lost: false, title: 'Security Reviewer' },
      // Lost herdr row must NOT be registered
      { key: '10||w1:p9', sessionId: 'h-conn-1', pane: 'w1:p9', isHerdr: true, lost: true, title: 'Stale Lost' },
      // Deduped tmux entry with cached conversation title in sidebar
      { key: '30|tmux|corral-admin', sessionId: 't-conn-1', isHerdr: false, title: 'Fix login bug' },
      // Unvisited tmux entry whose sidebar title is "tmux-box · ops"
      { key: '30|tmux|ops', sessionId: 't-unvisited', isHerdr: false, title: 'tmux-box · ops' },
    ],
    allSessions,
    { herdr: herdrKey, tmux: tmuxKey },
  )

  // Fix7 #3: BOTH deduped herdr connections resolve to the sidebar title
  assert.equal(peerTitles.get(peerTargetKey('h-conn-1', 'w1:p2')), 'Security Reviewer')
  assert.equal(peerTitles.get(peerTargetKey('h-conn-2', 'w1:p2')), 'Security Reviewer')
  assert.equal(peerTitles.has(peerTargetKey('h-other-mux', 'w1:p2')), false)
  assert.equal(peerTitles.has(peerTargetKey('h-other-host', 'w1:p2')), false)
  assert.equal(peerTitles.has(peerTargetKey('h-conn-1', 'w1:p9')), false)
  assert.equal(peerTitles.has(peerTargetKey('h-conn-2', 'w1:p9')), false)

  // Peer matched by backend on the OTHER herdr connection (h-conn-2) still gets sidebar title
  const dedupedHerdr = resolvePeerSender({
    from: 'ai-sub',
    peerPid: 111,
    peerStart: 222,
    peers: {
      '111:222': {
        session_id: 'h-conn-2',
        pane: 'w1:p2',
        title: 'Main Agent',
        host_name: 'devbox',
        persistence: 'herdr',
        workspace: 'Main Agent',
      },
    },
    peerTitles,
    failing: false,
  })
  assert.deepEqual(dedupedHerdr, {
    kind: 'live',
    name: 'Security Reviewer',
    where: 'devbox · herdr',
    sessionId: 'h-conn-2',
    pane: 'w1:p2',
    href: '/agents?s=h-conn-2&pane=w1%3Ap2',
  })

  // Fix7 #2: tmux entry uses sidebar's cached conversation title (NOT terminal title),
  // across BOTH deduped tmux connections.
  assert.equal(peerTitles.get(peerTargetKey('t-conn-1')), 'Fix login bug')
  assert.equal(peerTitles.get(peerTargetKey('t-conn-2')), 'Fix login bug')
  const tmuxCached = resolvePeerSender({
    from: 'ai-tmux',
    peerPid: 333,
    peerStart: 444,
    peers: {
      '333:444': {
        session_id: 't-conn-2',
        title: 'zsh',
        host_name: 'tmux-box',
        persistence: 'tmux',
        mux_session: 'corral-admin',
      },
    },
    peerTitles,
    failing: false,
  })
  assert.deepEqual(tmuxCached, {
    kind: 'live',
    name: 'Fix login bug',
    where: 'tmux-box · corral-admin',
    sessionId: 't-conn-2',
    pane: undefined,
    href: '/agents?s=t-conn-2',
  })

  // Unvisited tmux entry uses sidebar's "host · session" title without duplicating where
  const tmuxUnvisited = resolvePeerSender({
    from: 'ai-ops',
    peerPid: 555,
    peerStart: 666,
    peers: {
      '555:666': {
        session_id: 't-unvisited',
        title: 'bash',
        host_name: 'tmux-box',
        persistence: 'tmux',
        mux_session: 'ops',
      },
    },
    peerTitles,
    failing: false,
  })
  assert.deepEqual(tmuxUnvisited, {
    kind: 'live',
    name: 'tmux-box · ops',
    where: '',
    sessionId: 't-unvisited',
    pane: undefined,
    href: '/agents?s=t-unvisited',
  })
}

console.log('test-peer-sender: ok')
