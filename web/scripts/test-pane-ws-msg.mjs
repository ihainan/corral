import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { fileURLToPath } from 'node:url'
import ts from 'typescript'

const srcPath = fileURLToPath(new URL('../src/terminal/paneWsMsg.ts', import.meta.url))
const src = readFileSync(srcPath, 'utf8')
const { outputText } = ts.transpileModule(src, {
  compilerOptions: { module: ts.ModuleKind.ESNext, target: ts.ScriptTarget.ES2022 },
})
const { classifyPaneWsMessage } = await import(
  `data:text/javascript;base64,${Buffer.from(outputText).toString('base64')}`
)

// 1. ssh_channels_busy reports channels_busy directly (must NOT fall back to polling).
assert.deepEqual(
  classifyPaneWsMessage(JSON.stringify({ t: 'error', code: 'ssh_channels_busy' }), 0),
  { kind: 'channels_busy' },
)

// 2. Non-zero early exit with < 256 bytes triggers fallback.
assert.deepEqual(
  classifyPaneWsMessage(JSON.stringify({ t: 'exit', code: 1, bytes: 0 }), 0),
  { kind: 'fallback' },
)
assert.deepEqual(
  classifyPaneWsMessage(JSON.stringify({ t: 'exit', code: 127, bytes: 100 }), 100),
  { kind: 'fallback' },
)

// 3. Normal exit (code 0) or non-zero exit after >= 256 bytes closes without fallback.
assert.deepEqual(
  classifyPaneWsMessage(JSON.stringify({ t: 'exit', code: 0, bytes: 0 }), 0),
  { kind: 'closed' },
)
assert.deepEqual(
  classifyPaneWsMessage(JSON.stringify({ t: 'exit', code: 1, bytes: 300 }), 0),
  { kind: 'closed' },
)
assert.deepEqual(
  classifyPaneWsMessage(JSON.stringify({ t: 'exit', code: 1, bytes: 0 }), 512),
  { kind: 'closed' },
)

// 4. Unrelated messages (pong, unknown error, malformed JSON) are ignored.
assert.deepEqual(classifyPaneWsMessage(JSON.stringify({ t: 'pong' }), 0), { kind: 'ignore' })
assert.deepEqual(classifyPaneWsMessage(JSON.stringify({ t: 'error', code: 'other' }), 0), { kind: 'ignore' })
assert.deepEqual(classifyPaneWsMessage('not json', 0), { kind: 'ignore' })

console.log('paneWsMsg: all tests passed')
