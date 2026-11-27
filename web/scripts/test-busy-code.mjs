import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { fileURLToPath } from 'node:url'
import ts from 'typescript'

const srcPath = fileURLToPath(new URL('../src/terminal/busyCode.ts', import.meta.url))
const src = readFileSync(srcPath, 'utf8')
const { outputText } = ts.transpileModule(src, {
  compilerOptions: { module: ts.ModuleKind.ESNext, target: ts.ScriptTarget.ES2022 },
})
const { NOTHING_PRESSED, isBusyErrorCode, isNothingPressedCode, shouldIgnorePanelPollError } = await import(
  `data:text/javascript;base64,${Buffer.from(outputText).toString('base64')}`
)

// 1. NOTHING_PRESSED includes both busy and ssh_channels_busy so mode switches
// refused before pressing any key do not mark the mode as unknown.
assert.equal(NOTHING_PRESSED.includes('busy'), true)
assert.equal(NOTHING_PRESSED.includes('ssh_channels_busy'), true)
assert.equal(isNothingPressedCode('ssh_channels_busy'), true)
assert.equal(isNothingPressedCode('busy'), true)
assert.equal(isNothingPressedCode('unknown'), false)
assert.equal(isNothingPressedCode(undefined), false)

// 2. PanelWindow polling ignores ssh_channels_busy and busy (as well as abort)
// so transient channel capacity refusals do not accumulate into a lost state.
assert.equal(isBusyErrorCode('busy'), true)
assert.equal(isBusyErrorCode('ssh_channels_busy'), true)
assert.equal(isBusyErrorCode('no_screen'), false)

assert.equal(shouldIgnorePanelPollError(false, 'ssh_channels_busy'), true)
assert.equal(shouldIgnorePanelPollError(false, 'busy'), true)
assert.equal(shouldIgnorePanelPollError(true, 'no_screen'), true)
assert.equal(shouldIgnorePanelPollError(false, 'no_screen'), false)
assert.equal(shouldIgnorePanelPollError(false, 'host_error'), false)
assert.equal(shouldIgnorePanelPollError(false, undefined), false)

console.log('busyCode: all tests passed')
