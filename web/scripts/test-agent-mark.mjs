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

const { agentStatusMark } = await loadTs('../src/terminal/sideState.ts')

// 1. Plain states match the sidebar and tab marks directly.
assert.equal(agentStatusMark({ status: 'blocked' }), 'blocked')
assert.equal(agentStatusMark({ status: 'working' }), 'working')
assert.equal(agentStatusMark({ status: 'done' }), 'done')
assert.equal(agentStatusMark({ status: 'idle' }), '')
assert.equal(agentStatusMark({ status: 'unknown' }), '')
assert.equal(agentStatusMark({}), '')

// 2. Background running: subagents (bg='running', with or without status='working' from withBg)
//    and background commands (cmds truthy on idle/done/unknown) produce 'bg' (hollow ring).
assert.equal(agentStatusMark({ status: 'working', bg: 'running' }), 'bg')
assert.equal(agentStatusMark({ status: 'idle', bg: 'running' }), 'bg')
assert.equal(agentStatusMark({ status: 'done', bg: 'running' }), 'bg')
assert.equal(agentStatusMark({ status: 'idle', cmds: { n: 1 } }), 'bg')
assert.equal(agentStatusMark({ status: 'done', cmds: { n: 2 } }), 'bg')
assert.equal(agentStatusMark({ status: 'unknown', cmds: true }), 'bg')
assert.equal(agentStatusMark({ status: 'idle', cmds: { n: 0 } }), '')
// An agent CLI run as a background command (UI item 50) counts as background work too;
// a plain command task without an agent CLI does not, and working still wins over it.
assert.equal(agentStatusMark({ status: 'done', tasks: [{ kind: 'agent', agent: 'agy' }] }), 'bg')
assert.equal(agentStatusMark({ status: 'unknown', tasks: [{ kind: 'agent', agent: 'claude' }] }), 'bg')
assert.equal(agentStatusMark({ status: 'done', tasks: [{ kind: 'agent' }] }), 'done')
assert.equal(agentStatusMark({ status: 'working', tasks: [{ kind: 'agent', agent: 'codex' }] }), 'working')
assert.equal(agentStatusMark({ status: 'idle', cmds: false }), '')

// 3. Stale background work produces 'stale' instead of 'done' or empty.
assert.equal(agentStatusMark({ status: 'done', bg: 'stale' }), 'stale')
assert.equal(agentStatusMark({ status: 'idle', bg: 'stale' }), 'stale')

// 4. Failed unseen turn ('done' + !bg + failed) produces 'failed', taking precedence over cmds.
assert.equal(agentStatusMark({ status: 'done', failed: 'rate_limit' }), 'failed')
assert.equal(agentStatusMark({ status: 'done', failed: 'server_error', cmds: { n: 1 } }), 'failed')
// Once seen ('idle') or while bg subagents are still tracked, failed does not trigger 'failed'.
assert.equal(agentStatusMark({ status: 'idle', failed: 'rate_limit' }), '')
assert.equal(agentStatusMark({ status: 'done', bg: 'running', failed: 'rate_limit' }), 'bg')
assert.equal(agentStatusMark({ status: 'done', bg: 'stale', failed: 'rate_limit' }), 'stale')

// 5. Blocked (waiting for the user) always takes precedence over bg / stale / cmds.
assert.equal(agentStatusMark({ status: 'blocked', bg: 'running' }), 'blocked')
assert.equal(agentStatusMark({ status: 'blocked', bg: 'stale' }), 'blocked')
assert.equal(agentStatusMark({ status: 'blocked', cmds: { n: 2 } }), 'blocked')

// 6. Active foreground turn ('working' without bg='running') shows 'working' even if stale cmds remain.
assert.equal(agentStatusMark({ status: 'working', cmds: { n: 1 } }), 'working')
assert.equal(agentStatusMark({ status: 'working', bg: 'checking' }), 'working')

// 7. Lost herdr stream clears the live mark.
assert.equal(agentStatusMark({ status: 'blocked', lost: true }), '')
assert.equal(agentStatusMark({ status: 'working', bg: 'running', lost: true }), '')
assert.equal(agentStatusMark({ status: 'idle', cmds: { n: 1 }, lost: true }), '')
assert.equal(agentStatusMark({ status: 'done', failed: 'err', lost: true }), '')

console.log('agentMark: all tests passed')
