import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { fileURLToPath } from 'node:url'
import ts from 'typescript'

const srcPath = fileURLToPath(new URL('../src/terminal/liveTerminalPolicy.ts', import.meta.url))
const src = readFileSync(srcPath, 'utf8')
const { outputText } = ts.transpileModule(src, {
  compilerOptions: { module: ts.ModuleKind.ESNext, target: ts.ScriptTarget.ES2022 },
})
const { decideLiveTerminal, shouldOpenLiveTerminal, shouldClearLiveTerminal, liveFor } = await import(
  `data:text/javascript;base64,${Buffer.from(outputText).toString('base64')}`
)

// 1. Normal working states (spinner, background shell) must NOT pop up the live terminal.
assert.equal(decideLiveTerminal({ agent: 'claude', hasBind: true }), 'none')
assert.equal(shouldOpenLiveTerminal({ agent: 'claude', hasBind: true }), false)
assert.equal(decideLiveTerminal({ code: 'agent_busy', agent: 'claude', hasBind: true }), 'none')
assert.equal(shouldOpenLiveTerminal({ code: 'agent_busy', agent: 'claude', hasBind: true }), false)
assert.equal(decideLiveTerminal({ code: 'working', agent: 'claude', hasBind: true }), 'none')
assert.equal(decideLiveTerminal({ code: 'busy', agent: 'claude', hasBind: true }), 'none')

// 2. Log lag / uncertain delivery / transient unreadable screen must NOT pop up,
//    and ambiguous screen/send codes offer a clickable terminal hint instead.
for (const code of ['unknown', 'no_screen', 'cannot_verify', 'no_input']) {
  assert.equal(decideLiveTerminal({ code, agent: 'claude', hasBind: true }), 'hint', `${code} should offer hint, not auto-open`)
  assert.equal(shouldOpenLiveTerminal({ code, agent: 'claude', hasBind: true }), false, `${code} must not auto-open`)
}
assert.equal(decideLiveTerminal({ code: 'network', agent: 'claude', hasBind: true }), 'none')
assert.equal(decideLiveTerminal({ code: 'host_error', agent: 'claude', hasBind: true }), 'none')
assert.equal(decideLiveTerminal({ code: 'send_failed', agent: 'claude', hasBind: true }), 'none')

// 3. Confirmed terminal dialog/panel or view mismatch DOES pop up the live terminal.
for (const code of ['agent_waiting', 'picker', 'subagent_view', 'other_view']) {
  assert.equal(decideLiveTerminal({ code, agent: 'claude', hasBind: true }), 'open', `${code} should auto-open`)
  assert.equal(shouldOpenLiveTerminal({ code, agent: 'claude', hasBind: true }), true, `${code} should auto-open`)
}

// 4. Confirmed unsent message (stuck in input box or blocked by terminal draft) DOES pop up.
for (const code of ['typed_not_sent', 'typed_other_view', 'enter_not_taken', 'box_draft', 'draft']) {
  assert.equal(decideLiveTerminal({ code, agent: 'claude', hasBind: true }), 'open', `${code} should auto-open`)
  assert.equal(shouldOpenLiveTerminal({ code, agent: 'claude', hasBind: true }), true, `${code} should auto-open`)
}

// 5. Native UI already handling the prompt (AskCard waits or dialog PanelWindow open)
//    must NOT stash a live popup behind it, and should clear any existing live popup.
for (const code of ['agent_waiting', 'picker', 'typed_not_sent', 'unknown']) {
  assert.equal(decideLiveTerminal({ code, agent: 'claude', hasBind: true, waits: true }), 'none')
  assert.equal(decideLiveTerminal({ code, agent: 'claude', hasBind: true, panelOpen: true }), 'none')
}
assert.equal(shouldClearLiveTerminal({ waits: true, panelOpen: false }), true)
assert.equal(shouldClearLiveTerminal({ waits: false, panelOpen: true }), true)
assert.equal(shouldClearLiveTerminal({ waits: false, panelOpen: false }), false)

// 6. Antigravity CLI waiting refreshes for native AskCard instead of raw terminal.
assert.equal(decideLiveTerminal({ code: 'agent_waiting', agent: 'agy', hasBind: true }), 'refresh')
assert.equal(shouldOpenLiveTerminal({ code: 'agent_waiting', agent: 'agy', hasBind: true }), false)

// 7. Unbound or non-live agents (e.g. devin) never open live terminal.
assert.equal(liveFor('claude'), true)
assert.equal(liveFor('codex'), true)
assert.equal(liveFor('opencode'), true)
assert.equal(liveFor('agy'), true)
assert.equal(liveFor('devin'), false)
assert.equal(decideLiveTerminal({ code: 'agent_waiting', agent: 'devin', hasBind: true }), 'none')
assert.equal(decideLiveTerminal({ code: 'agent_waiting', agent: 'claude', hasBind: false }), 'none')

console.log('liveTerminalPolicy: all tests passed')
