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
} = await import(`data:text/javascript;base64,${Buffer.from(outputText).toString('base64')}`)

const sample = [
  { name: 'Z_PLAIN', sensitive: false, value: '/usr/local/bin' },
  { name: 'ANTHROPIC_API_KEY', sensitive: true, preview: 'sk-a••••••••' },
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
  filterPaneEnvVars(sample, {}, 'sk-a').map((v) => v.name),
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

// Display value shows preview while hidden, full value once revealed.
assert.deepEqual(envDisplayValue(sample[1], {}), { text: 'sk-a••••••••', masked: true, revealed: false })
assert.deepEqual(envDisplayValue(sample[1], { ANTHROPIC_API_KEY: 'sk-ant-full' }), { text: 'sk-ant-full', masked: false, revealed: true })
assert.deepEqual(envDisplayValue(sample[2], {}), { text: '/home/dev', masked: false, revealed: true })

// Reason mapping covers every backend unavailability code.
assert.equal(envReasonKey('no_proc'), 'card.envReasonNoProc')
assert.equal(envReasonKey('permission_denied'), 'card.envReasonPerm')
assert.equal(envReasonKey('no_process'), 'card.envReasonNoProcess')
assert.equal(envReasonKey('herdr_unavailable'), 'card.envReasonHerdr')
assert.equal(envReasonKey('unsupported_mode'), 'card.envReasonUnsupported')
assert.equal(envReasonKey('unknown_code'), 'card.envReasonFailed')

console.log('paneEnv: all tests passed')
