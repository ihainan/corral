import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { fileURLToPath } from 'node:url'
import ts from 'typescript'

const srcPath = fileURLToPath(new URL('../src/terminal/stepFormat.ts', import.meta.url))
const src = readFileSync(srcPath, 'utf8')
const { outputText } = ts.transpileModule(src, {
  compilerOptions: { module: ts.ModuleKind.ESNext, target: ts.ScriptTarget.ES2022 },
})
const { looksLikeShellCommand, taskEventMeta, toolObjectIsCode } = await import(
  `data:text/javascript;base64,${Buffer.from(outputText).toString('base64')}`
)

// 1. looksLikeShellCommand: prose descriptions must stay UI font
for (const desc of [
  '',
  '   ',
  'Add Devin CLI support to Corral',
  'Run SWE-2.1 on batch C rewind task',
  'Wait for P1.4j gate results',
  'Show Devin CLI branch commits and diff stat',
  '构建前端产物并校验样式',
  'check build output',
]) {
  assert.equal(looksLikeShellCommand(desc), false, `expected prose, not shell command: ${JSON.stringify(desc)}`)
}

// 2. looksLikeShellCommand: raw shell commands use monospace
for (const cmd of [
  'go test -race ./internal/...',
  'cd /tmp/work && pnpm run build',
  './scripts/check.sh',
  '/usr/local/bin/corral --version',
  '~/bin/run',
  'GOTMPDIR=/tmp/go go vet ./internal/...',
  'git status',
  'echo ok | grep ok',
  'line1\nline2',
  'custom-cli --flag value',
]) {
  assert.equal(looksLikeShellCommand(cmd), true, `expected shell command: ${JSON.stringify(cmd)}`)
}

// 3. taskEventMeta: subagent & monitor tasks never use monospace; cmd only when raw command
assert.deepEqual(taskEventMeta({ task: 'agent', status: 'done', text: 'Add Devin CLI support to Corral' }), {
  icon: 'agent',
  headKey: 'convo.task.agent.done',
  isCode: false,
})
assert.deepEqual(taskEventMeta({ task: 'agent', status: 'failed', text: 'go test ./...' }), {
  icon: 'agent',
  headKey: 'convo.task.agent.failed',
  isCode: false,
})
assert.deepEqual(taskEventMeta({ task: 'monitor', status: 'stopped', text: 'tail -f /tmp/out.log' }), {
  icon: 'monitor',
  headKey: 'convo.task.monitor.stopped',
  isCode: false,
})
assert.deepEqual(taskEventMeta({ task: 'cmd', status: 'done', text: 'Run SWE-2.1 on batch C rewind task' }), {
  icon: 'cmd',
  headKey: 'convo.task.cmd.done',
  isCode: false,
})
assert.deepEqual(taskEventMeta({ task: 'cmd', status: 'failed', text: 'go test -race ./internal/...' }), {
  icon: 'cmd',
  headKey: 'convo.task.cmd.failed',
  isCode: true,
})
assert.deepEqual(taskEventMeta({}), {
  icon: 'cmd',
  headKey: 'convo.task.cmd.done',
  isCode: false,
})

// 4. toolObjectIsCode: monospace only for raw commands, file paths, and Grep/Glob patterns
assert.equal(toolObjectIsCode('Bash', 'cmd', false, false), true)
assert.equal(toolObjectIsCode('Bash', 'cmd', true, false), false)
assert.equal(toolObjectIsCode('Bash', 'cmd', false, true), false)
assert.equal(toolObjectIsCode('Read', 'read', false, false), true)
assert.equal(toolObjectIsCode('Edit', 'edit', false, false), true)
assert.equal(toolObjectIsCode('Write', 'write', false, false), true)
assert.equal(toolObjectIsCode('Read', 'read', true, false), false)
assert.equal(toolObjectIsCode('Grep', 'search', false, false), true)
assert.equal(toolObjectIsCode('Glob', 'search', false, false), true)
assert.equal(toolObjectIsCode('WebSearch', 'search', false, false), false)
assert.equal(toolObjectIsCode('ToolSearch', 'search', false, false), false)
assert.equal(toolObjectIsCode('WebFetch', 'web', false, false), false)
assert.equal(toolObjectIsCode('Agent', 'agent', false, false), false)
assert.equal(toolObjectIsCode('TaskStop', 'stop', false, false), false)
assert.equal(toolObjectIsCode('Artifact', 'page', false, false), false)
assert.equal(toolObjectIsCode('mcp__docs__get_page', 'doc', false, false), false)
assert.equal(toolObjectIsCode('mcp__pw__browser_click', 'browser', false, false), false)
assert.equal(toolObjectIsCode('mcp__srv__tool', 'mcp', false, false), false)

// 5. Verify i18n keys for every task kind x status + sep exist in both zh and en
const i18nSrc = readFileSync(fileURLToPath(new URL('../src/i18n.ts', import.meta.url)), 'utf8')
const kinds = ['cmd', 'agent', 'monitor']
const statuses = ['done', 'failed', 'stopped', 'waiting']
const requiredKeys = ['convo.task.sep', 'convo.task.code', 'convo.task.cut']
for (const k of kinds) for (const s of statuses) requiredKeys.push(`convo.task.${k}.${s}`)
for (const key of requiredKeys) {
  const matches = i18nSrc.match(new RegExp(`'${key.replace(/\./g, '\\.')}':`, 'g')) ?? []
  assert.equal(matches.length, 2, `expected 2 i18n entries (zh + en) for ${key}, found ${matches.length}`)
}

console.log('stepFormat: all tests passed')
