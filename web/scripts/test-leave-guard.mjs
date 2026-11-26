import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { fileURLToPath } from 'node:url'
import ts from 'typescript'

const srcPath = fileURLToPath(new URL('../src/leaveGuard.ts', import.meta.url))
const src = readFileSync(srcPath, 'utf8')
const { outputText } = ts.transpileModule(src, {
  compilerOptions: { module: ts.ModuleKind.ESNext, target: ts.ScriptTarget.ES2022 },
})
const {
  isActiveTerminalStatus,
  shouldConfirmLeave,
  setLeaveGuardTerminal,
  registerDraftCheck,
  hasTrackedDrafts,
} = await import(`data:text/javascript;base64,${Buffer.from(outputText).toString('base64')}`)

// Active vs inactive terminal statuses.
for (const s of ['connecting', 'open', 'reconnecting', 'offline']) {
  assert.equal(isActiveTerminalStatus(s), true, `expected ${s} to be active`)
}
for (const s of ['ended', 'gone', 'full', 'revoked', 'closed', '']) {
  assert.equal(isActiveTerminalStatus(s), false, `expected ${s} to be inactive`)
}

// Disabled setting never prompts even with open terminals and unsent drafts.
assert.equal(
  shouldConfirmLeave({ enabled: false, bypassUntil: 0, now: 1000, openTerminals: 2, hasDrafts: true }),
  false,
)

// Enabled with neither open terminals nor unsent drafts does not prompt.
assert.equal(
  shouldConfirmLeave({ enabled: true, bypassUntil: 0, now: 1000, openTerminals: 0, hasDrafts: false }),
  false,
)

// Enabled with an open terminal session prompts.
assert.equal(
  shouldConfirmLeave({ enabled: true, bypassUntil: 0, now: 1000, openTerminals: 1, hasDrafts: false }),
  true,
)

// Enabled with unsent draft content prompts even with no open terminals.
assert.equal(
  shouldConfirmLeave({ enabled: true, bypassUntil: 0, now: 1000, openTerminals: 0, hasDrafts: true }),
  true,
)

// Self-initiated reload / redirect bypass suppresses confirmation within its window.
assert.equal(
  shouldConfirmLeave({ enabled: true, bypassUntil: 6000, now: 1000, openTerminals: 2, hasDrafts: true }),
  false,
)
assert.equal(
  shouldConfirmLeave({ enabled: true, bypassUntil: 6000, now: 5999, openTerminals: 1, hasDrafts: false }),
  false,
)

// Once the bypass window expires, confirmation resumes.
assert.equal(
  shouldConfirmLeave({ enabled: true, bypassUntil: 6000, now: 6000, openTerminals: 1, hasDrafts: false }),
  true,
)
assert.equal(
  shouldConfirmLeave({ enabled: true, bypassUntil: 6000, now: 7000, openTerminals: 0, hasDrafts: true }),
  true,
)

// Draft check registration and unregistering.
assert.equal(hasTrackedDrafts(), false)
let hasA = false
let hasB = false
const offA = registerDraftCheck(() => hasA)
const offB = registerDraftCheck(() => hasB)
assert.equal(hasTrackedDrafts(), false)
hasB = true
assert.equal(hasTrackedDrafts(), true)
hasB = false
hasA = true
assert.equal(hasTrackedDrafts(), true)
offA()
assert.equal(hasTrackedDrafts(), false)
offB()

// Terminal token tracking does not throw on duplicate add/delete.
const t1 = Symbol('t1')
setLeaveGuardTerminal(t1, true)
setLeaveGuardTerminal(t1, true)
setLeaveGuardTerminal(t1, false)
setLeaveGuardTerminal(t1, false)

console.log('leaveGuard: all tests passed')
