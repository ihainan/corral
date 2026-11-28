import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { fileURLToPath } from 'node:url'
import ts from 'typescript'

const srcPath = fileURLToPath(new URL('../src/terminal/paneEnv.ts', import.meta.url))
const src = readFileSync(srcPath, 'utf8')
const { outputText } = ts.transpileModule(src, {
  compilerOptions: { module: ts.ModuleKind.ESNext, target: ts.ScriptTarget.ES2022 },
})
const {
  sortPaneEnvVars,
  filterPaneEnvVars,
  isEnvRevealed,
  envDisplayValue,
  envCopyValue,
  envReasonKey,
  createPaneEnvRevealState,
  resetPaneEnvReveal,
  startPaneEnvReveal,
  isPaneEnvRevealCurrent,
  finishPaneEnvReveal,
  failPaneEnvReveal,
  hidePaneEnvVar,
} = await import(`data:text/javascript;base64,${Buffer.from(outputText).toString('base64')}`)

const sample = [
  { name: 'Z_PLAIN', sensitive: false, value: '/usr/local/bin' },
  { name: 'ANTHROPIC_API_KEY', sensitive: true, preview: '••••••••' },
  { name: 'HOME', sensitive: false, value: '/home/dev' },
  { name: 'DB_PASSWORD', sensitive: true, preview: '••••••••' },
]

// Alphabetical sort by variable name.
assert.deepEqual(
  sortPaneEnvVars(sample).map((v) => v.name),
  ['ANTHROPIC_API_KEY', 'DB_PASSWORD', 'HOME', 'Z_PLAIN'],
)

// Search matches name and non-sensitive values, but never hidden previews.
assert.deepEqual(
  filterPaneEnvVars(sample, {}, 'local').map((v) => v.name),
  ['Z_PLAIN'],
)
assert.deepEqual(
  filterPaneEnvVars(sample, {}, '••••').map((v) => v.name),
  [],
  'unrevealed sensitive preview must not match value search',
)
assert.deepEqual(
  filterPaneEnvVars(sample, { ANTHROPIC_API_KEY: 'sk-ant-real-secret' }, 'real-secret').map((v) => v.name),
  ['ANTHROPIC_API_KEY'],
  'revealed sensitive value may match search',
)

// Copy is strictly gated on reveal for sensitive vars, allowed directly for non-sensitive vars.
assert.equal(envCopyValue(sample[0], {}), '/usr/local/bin')
assert.equal(envCopyValue(sample[1], {}), undefined)
assert.equal(envCopyValue(sample[1], { ANTHROPIC_API_KEY: 'sk-ant-full' }), 'sk-ant-full')
assert.equal(isEnvRevealed(sample[1], {}), false)
assert.equal(isEnvRevealed(sample[1], { ANTHROPIC_API_KEY: 'sk-ant-full' }), true)
assert.equal(isEnvRevealed(sample[0], {}), true)

// Display value shows fixed bullet mask while hidden, full value once revealed.
assert.deepEqual(envDisplayValue(sample[1], {}), { text: '••••••••', masked: true, revealed: false })
assert.deepEqual(envDisplayValue({ name: 'DSN', sensitive: true, preview: 'a:b@••••••••' }, {}), { text: '••••••••', masked: true, revealed: false })
assert.deepEqual(envDisplayValue({ name: 'DSN', sensitive: true, preview: '' }, {}), { text: '••••••••', masked: true, revealed: false })
assert.deepEqual(envDisplayValue(sample[1], { ANTHROPIC_API_KEY: 'sk-ant-full' }), { text: 'sk-ant-full', masked: false, revealed: true })
assert.deepEqual(envDisplayValue(sample[2], {}), { text: '/home/dev', masked: false, revealed: true })

// Reason mapping covers every backend unavailability code.
assert.equal(envReasonKey('no_proc'), 'card.envReasonNoProc')
assert.equal(envReasonKey('permission_denied'), 'card.envReasonPerm')
assert.equal(envReasonKey('no_process'), 'card.envReasonNoProcess')
assert.equal(envReasonKey('herdr_unavailable'), 'card.envReasonHerdr')
assert.equal(envReasonKey('unsupported_mode'), 'card.envReasonUnsupported')
assert.equal(envReasonKey('pane_not_in_session'), 'card.envReasonPaneNotInSession')
assert.equal(envReasonKey('session_mismatch'), 'card.envReasonSessionMismatch')
assert.equal(envReasonKey('unknown_code'), 'card.envReasonFailed')

// Reveal epoch / stale response guard tests:
// 1. Normal reveal succeeds and clears revealing state.
{
  let st = createPaneEnvRevealState()
  const r1 = startPaneEnvReveal(st, 'API_KEY')
  assert.ok(r1.token)
  st = r1.state
  assert.equal(isPaneEnvRevealCurrent(st, 'API_KEY', r1.token), true)
  // Duplicate click while in flight is ignored.
  const dup = startPaneEnvReveal(st, 'API_KEY')
  assert.equal(dup.token, null)
  st = finishPaneEnvReveal(st, 'API_KEY', r1.token, 'secret-1')
  assert.equal(st.revealed.API_KEY, 'secret-1')
  assert.equal(st.revealing.API_KEY, undefined)
  assert.equal(st.error, false)
}

// 2. Collapse & re-expand (reset) before reveal returns -> late response & late error are discarded.
{
  let st = createPaneEnvRevealState()
  const r1 = startPaneEnvReveal(st, 'API_KEY')
  st = r1.state
  // Collapse then expand calls resetPaneEnvReveal.
  st = resetPaneEnvReveal(st)
  st = resetPaneEnvReveal(st)
  assert.equal(isPaneEnvRevealCurrent(st, 'API_KEY', r1.token), false)
  st = finishPaneEnvReveal(st, 'API_KEY', r1.token, 'leaked-secret')
  assert.equal(st.revealed.API_KEY, undefined, 'late reveal after collapse/expand must not re-expose secret')
  st = failPaneEnvReveal(st, 'API_KEY', r1.token)
  assert.equal(st.error, false, 'late error from stale epoch must not set error')
}

// 3. Re-click reveal in new epoch -> stale epoch response must not clobber or clear new in-flight reveal.
{
  let st = createPaneEnvRevealState()
  const oldReq = startPaneEnvReveal(st, 'API_KEY')
  st = oldReq.state
  // Session/pane switch resets epoch.
  st = resetPaneEnvReveal(st)
  const newReq = startPaneEnvReveal(st, 'API_KEY')
  st = newReq.state
  assert.ok(newReq.token)
  // Old response arrives first -> ignored; new request remains in flight.
  st = finishPaneEnvReveal(st, 'API_KEY', oldReq.token, 'old-secret')
  assert.equal(st.revealed.API_KEY, undefined)
  assert.equal(st.revealing.API_KEY, newReq.token.seq)
  // Old error also ignored.
  st = failPaneEnvReveal(st, 'API_KEY', oldReq.token)
  assert.equal(st.error, false)
  assert.equal(st.revealing.API_KEY, newReq.token.seq)
  // New response arrives -> accepted.
  st = finishPaneEnvReveal(st, 'API_KEY', newReq.token, 'new-secret')
  assert.equal(st.revealed.API_KEY, 'new-secret')
  assert.equal(st.revealing.API_KEY, undefined)
}

// 4. Hide while reveal is in flight -> late response is discarded, while sibling var reveal succeeds.
{
  let st = createPaneEnvRevealState()
  const rA = startPaneEnvReveal(st, 'VAR_A')
  st = rA.state
  const rB = startPaneEnvReveal(st, 'VAR_B')
  st = rB.state
  st = hidePaneEnvVar(st, 'VAR_A')
  st = finishPaneEnvReveal(st, 'VAR_A', rA.token, 'secret-A')
  assert.equal(st.revealed.VAR_A, undefined, 'hidden variable must stay hidden when late response returns')
  st = finishPaneEnvReveal(st, 'VAR_B', rB.token, 'secret-B')
  assert.equal(st.revealed.VAR_B, 'secret-B', 'sibling variable reveal must still succeed')
  // Hiding revealed VAR_B removes it cleanly.
  st = hidePaneEnvVar(st, 'VAR_B')
  assert.equal(st.revealed.VAR_B, undefined)
}

console.log('paneEnv: all tests passed')
