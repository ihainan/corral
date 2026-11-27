import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { fileURLToPath } from 'node:url'
import ts from 'typescript'

const srcPath = fileURLToPath(new URL('../src/terminal/askHookLogic.ts', import.meta.url))
const src = readFileSync(srcPath, 'utf8')
const { outputText } = ts.transpileModule(src, {
  compilerOptions: { module: ts.ModuleKind.ESNext, target: ts.ScriptTarget.ES2022 },
})
const {
  ASK_HOOK_NO_NEED_VERSION,
  askHookRowState,
  asksFromScreen,
  claudeNeedsAskHook,
  compareClaudeVersion,
  hintOf,
  parseClaudeVersion,
} = await import(`data:text/javascript;base64,${Buffer.from(outputText).toString('base64')}`)

assert.equal(ASK_HOOK_NO_NEED_VERSION, '2.1.316')

// 1. parseClaudeVersion: valid formats and rejection of malformed inputs.
assert.deepEqual(parseClaudeVersion('2.1.316'), [2, 1, 316])
assert.deepEqual(parseClaudeVersion('2.1.330 (Claude Code)'), [2, 1, 330])
assert.deepEqual(parseClaudeVersion('  2.1.318 (Claude Code)\n'), [2, 1, 318])
for (const bad of [
  '',
  '   ',
  null,
  undefined,
  'unknown',
  '2.1',
  '2',
  '2.1.316.0',
  'v2.1.316',
  '2.1.x',
  '2.1.316-rc.1',
  '2.1.316+build',
  '-2.1.316',
  '2.1.316 extra',
  'error 2.1.316',
  '2.1.316\n2.1.318',
]) {
  assert.equal(parseClaudeVersion(bad), null, `expected ${JSON.stringify(bad)} to be invalid`)
}

// 2. compareClaudeVersion ordering.
assert.equal(compareClaudeVersion([2, 1, 315], [2, 1, 316]), -1)
assert.equal(compareClaudeVersion([2, 1, 316], [2, 1, 316]), 0)
assert.equal(compareClaudeVersion([2, 1, 317], [2, 1, 316]), 1)
assert.equal(compareClaudeVersion([2, 0, 999], [2, 1, 0]), -1)
assert.equal(compareClaudeVersion([2, 2, 0], [2, 1, 999]), 1)
assert.equal(compareClaudeVersion([3, 0, 0], [2, 99, 99]), 1)

// 3. claudeNeedsAskHook: below, equal, above, unknown, malformed.
for (const v of ['2.1.252', '2.1.281', '2.1.284', '2.1.299', '2.1.304', '2.1.315', '2.1.315 (Claude Code)', '2.0.999']) {
  assert.equal(claudeNeedsAskHook(v), true, `below threshold ${v} should need hook`)
}
for (const v of ['2.1.316', '2.1.316 (Claude Code)', '2.1.318', '2.1.330', '2.1.330 (Claude Code)', '2.2.0', '3.0.0']) {
  assert.equal(claudeNeedsAskHook(v), false, `at/above threshold ${v} should not need hook`)
}
for (const v of [undefined, null, '', '   ', '2.1', '2.1.x', 'v2.1.316', '2.1.316-beta.1', '2.1.316.1', 'not a version']) {
  assert.equal(claudeNeedsAskHook(v), true, `unknown/malformed ${JSON.stringify(v)} should keep hook`)
}

// 4. asksFromScreen: respects session Claude Code version.
const qScreen = 'Pick one?\n> 1. A\nEnter to select · Esc to cancel'
assert.equal(asksFromScreen({ agent: 'claude', logAsks: false, screen: qScreen, drawn: true }), true)
assert.equal(asksFromScreen({ agent: 'claude', logAsks: false, screen: qScreen, drawn: true, version: '2.1.315' }), true)
assert.equal(asksFromScreen({ agent: 'claude', logAsks: false, screen: qScreen, drawn: true, version: 'bad' }), true)
assert.equal(asksFromScreen({ agent: 'claude', logAsks: false, screen: qScreen, drawn: true, version: '2.1.316' }), false)
assert.equal(asksFromScreen({ agent: 'claude', logAsks: false, screen: qScreen, drawn: true, version: '2.1.330' }), false)
assert.equal(asksFromScreen({ agent: 'claude', logAsks: true, screen: qScreen, drawn: true, version: '2.1.304' }), false)
assert.equal(asksFromScreen({ agent: 'codex', logAsks: false, screen: qScreen, drawn: true, version: '2.1.304' }), false)

// 5. askHookRowState: normal host vs host where all detected versions >= 2.1.316.
const baseInfo = { file: '~/.claude/settings.json', seen: 'abc', snippet: '{}', declined: false }
assert.deepEqual(askHookRowState({ ...baseInfo, state: 'off' }), {
  hintKey: 'askHook.rowHint',
  canTurnOn: true,
  canTurnOff: false,
  showSnippet: false,
})
assert.deepEqual(askHookRowState({ ...baseInfo, state: 'on' }), {
  hintKey: 'askHook.rowHint',
  canTurnOn: false,
  canTurnOff: true,
  showSnippet: false,
})
assert.deepEqual(askHookRowState({ ...baseInfo, state: 'on', unnecessary: true }), {
  hintKey: 'askHook.rowHintUnnecessaryOn',
  canTurnOn: false,
  canTurnOff: true,
  showSnippet: false,
})
assert.deepEqual(askHookRowState({ ...baseInfo, state: 'off', unnecessary: true }), {
  hintKey: 'askHook.rowHintUnnecessary',
  canTurnOn: false,
  canTurnOff: false,
  showSnippet: false,
})
assert.deepEqual(askHookRowState({ ...baseInfo, state: 'on', cannot: 'link', unnecessary: true }), {
  hintKey: 'askHook.rowHintUnnecessaryOn',
  canTurnOn: false,
  canTurnOff: false,
  showSnippet: false,
})
assert.deepEqual(askHookRowState({ ...baseInfo, state: 'off', cannot: 'link', unnecessary: false }), {
  hintKey: 'askHook.rowHint',
  canTurnOn: false,
  canTurnOff: false,
  showSnippet: true,
})

// 6. hintOf remains intact for old/unknown sessions.
assert.equal(hintOf({ ...baseInfo, state: 'off' }), 'offer')
assert.equal(hintOf({ ...baseInfo, state: 'off', declined: true }), 'declined')
assert.equal(hintOf({ ...baseInfo, state: 'on' }), 'none')
assert.equal(hintOf({ ...baseInfo, state: 'blocked', why: 'disableAllHooks' }), 'blocked')
assert.equal(hintOf({ ...baseInfo, state: 'off', cannot: 'link' }), 'cannot')

console.log('test-ask-hook.mjs: all assertions passed')
