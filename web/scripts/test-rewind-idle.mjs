import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { fileURLToPath } from 'node:url'
import ts from 'typescript'

const srcPath = fileURLToPath(new URL('../src/terminal/rewindIdle.ts', import.meta.url))
const src = readFileSync(srcPath, 'utf8')
const { outputText } = ts.transpileModule(src, {
  compilerOptions: { module: ts.ModuleKind.ESNext, target: ts.ScriptTarget.ES2022 },
})
const { isRewindIdle } = await import(`data:text/javascript;base64,${Buffer.from(outputText).toString('base64')}`)

// Waiting for reply (agent.status === 'blocked') must disable rewind even when
// herdr's raw row status still reports 'idle'.
assert.equal(isRewindIdle({ status: 'blocked', dialog: false }, 'idle', 'herdr'), false)
assert.equal(isRewindIdle({ status: 'blocked', dialog: false }, 'done', 'herdr'), false)

// Working or unknown agent status disables rewind even if raw row status is idle.
assert.equal(isRewindIdle({ status: 'working', dialog: false }, 'idle', 'herdr'), false)
assert.equal(isRewindIdle({ status: 'unknown', dialog: false }, 'idle', 'herdr'), false)

// Open Claude Code dialog disables rewind even when status is idle.
assert.equal(isRewindIdle({ status: 'idle', dialog: true }, 'idle', 'herdr'), false)
assert.equal(isRewindIdle({ status: 'idle', dialog: true }, undefined, 'tmux'), false)

// Idle or done agent status with no dialog enables rewind.
assert.equal(isRewindIdle({ status: 'idle', dialog: false }, 'idle', 'herdr'), true)
assert.equal(isRewindIdle({ status: 'done', dialog: false }, 'done', 'herdr'), true)
assert.equal(isRewindIdle({ status: 'idle', dialog: false }, undefined, 'tmux'), true)
assert.equal(isRewindIdle({ status: 'done', dialog: false }, undefined, 'tmux'), true)

// Fallback to cur.row.status when agent status is unavailable.
assert.equal(isRewindIdle(null, 'idle', 'herdr'), true)
assert.equal(isRewindIdle(undefined, 'done', 'herdr'), true)
assert.equal(isRewindIdle(null, 'working', 'herdr'), false)
assert.equal(isRewindIdle(null, 'blocked', 'herdr'), false)
assert.equal(isRewindIdle(null, undefined, 'tmux'), false)

// Non-herdr/non-tmux sessions never enable rewind.
assert.equal(isRewindIdle({ status: 'idle', dialog: false }, 'idle', 'plain'), false)
assert.equal(isRewindIdle({ status: 'idle', dialog: false }, 'idle', undefined), false)

console.log('rewindIdle: all tests passed')
