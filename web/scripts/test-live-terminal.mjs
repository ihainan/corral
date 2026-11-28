import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { fileURLToPath } from 'node:url'
import ts from 'typescript'

const srcPath = fileURLToPath(new URL('../src/terminal/liveTerminalPolicy.ts', import.meta.url))
const src = readFileSync(srcPath, 'utf8')
const { outputText } = ts.transpileModule(src, {
  compilerOptions: { module: ts.ModuleKind.ESNext, target: ts.ScriptTarget.ES2022 },
})
const {
  CONFIRMED_LIVE_CODES,
  canUseDraftCard,
  composerBlockKey,
  decideLiveTerminal,
  isBlockerResolved,
  liveFor,
  liveTerminalNoteKey,
  normalizeLiveReason,
  reduceBlockerOnScreen,
  sameBlockerReason,
  shouldClearLiveTerminal,
  shouldOpenLiveTerminal,
} = await import(`data:text/javascript;base64,${Buffer.from(outputText).toString('base64')}`)

const i18nSrc = readFileSync(fileURLToPath(new URL('../src/i18n.ts', import.meta.url)), 'utf8')
const DIGEST = 'a'.repeat(64)

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

// 3. Recognized unsent drafts on Claude Code and Codex use the native DraftCard
//    instead of popping up a raw terminal.
assert.equal(
  decideLiveTerminal({ code: 'box_draft', agent: 'claude', hasBind: true, draft: '重新登录，我不要系统管理员，我要换一个用户', digest: DIGEST }),
  'draft_card',
)
assert.equal(
  shouldOpenLiveTerminal({ code: 'box_draft', agent: 'claude', hasBind: true, draft: '重新登录', digest: DIGEST }),
  false,
)
assert.equal(
  decideLiveTerminal({ code: 'draft', agent: 'codex', hasBind: true, draft: '请帮我检查这个接口', digest: DIGEST }),
  'draft_card',
)
assert.equal(canUseDraftCard({ code: 'box_draft', agent: 'claude', draft: 'hello', digest: DIGEST }), true)
assert.equal(canUseDraftCard({ code: 'draft', agent: 'codex', draft: 'hello', digest: DIGEST }), true)

// Unrecognized drafts (missing text or digest, or agents without box-draft clear/send)
// still fall back to the live terminal popup.
assert.equal(decideLiveTerminal({ code: 'box_draft', agent: 'claude', hasBind: true }), 'open')
assert.equal(decideLiveTerminal({ code: 'box_draft', agent: 'claude', hasBind: true, draft: '   ', digest: DIGEST }), 'open')
assert.equal(decideLiveTerminal({ code: 'box_draft', agent: 'claude', hasBind: true, draft: 'text' }), 'open')
assert.equal(decideLiveTerminal({ code: 'draft', agent: 'opencode', hasBind: true, draft: 'text', digest: DIGEST }), 'open')
assert.equal(decideLiveTerminal({ code: 'draft', agent: 'agy', hasBind: true, draft: 'text', digest: DIGEST }), 'open')

// 4. Confirmed terminal-only blockers auto-open the live terminal with an explicit
//    one-sentence explanation and composer block message.
const expectedNotes = {
  agent_waiting: ['panel.why.agentWaiting', 'agents.blocked.agentWaiting'],
  picker: ['panel.why.picker', 'agents.blocked.picker'],
  subagent_view: ['panel.why.subagentView', 'agents.blocked.subagentView'],
  other_view: ['panel.why.otherView', 'agents.blocked.otherView'],
  box_draft: ['panel.why.draft', 'agents.blocked.draft'],
  draft: ['panel.why.draft', 'agents.blocked.draft'],
  typed_not_sent: ['panel.why.typedNotSent', 'agents.blocked.unsent'],
  typed_other_view: ['panel.why.typedOtherView', 'agents.blocked.unsent'],
  enter_not_taken: ['panel.why.enterNotTaken', 'agents.blocked.unsent'],
}

for (const code of CONFIRMED_LIVE_CODES) {
  assert.equal(decideLiveTerminal({ code, agent: 'claude', hasBind: true }), 'open', `${code} without draft card should auto-open`)
  assert.equal(shouldOpenLiveTerminal({ code, agent: 'claude', hasBind: true }), true, `${code} should auto-open`)
  const [wantNote, wantBlock] = expectedNotes[code]
  assert.equal(liveTerminalNoteKey(code), wantNote, `note key for ${code}`)
  assert.equal(composerBlockKey(code), wantBlock, `composer block key for ${code}`)
  // Verify both zh and en entries exist in i18n.ts (at least 2 occurrences each).
  const noteMatches = i18nSrc.match(new RegExp(`'${wantNote.replace(/\./g, '\\.')}':`, 'g')) ?? []
  const blockMatches = i18nSrc.match(new RegExp(`'${wantBlock.replace(/\./g, '\\.')}':`, 'g')) ?? []
  assert.ok(noteMatches.length >= 2, `i18n missing zh/en for ${wantNote}`)
  assert.ok(blockMatches.length >= 2, `i18n missing zh/en for ${wantBlock}`)
}

// 5. Manual close suppresses repeated auto-popups for the SAME reason,
//    while allowing a DIFFERENT reason to open normally.
assert.equal(normalizeLiveReason('box_draft'), 'draft')
assert.equal(normalizeLiveReason('draft'), 'draft')
assert.equal(sameBlockerReason('box_draft', 'draft'), true)
assert.equal(sameBlockerReason('picker', 'subagent_view'), false)

assert.equal(
  decideLiveTerminal({ code: 'picker', agent: 'claude', hasBind: true, dismissedReason: 'picker' }),
  'hint',
  'dismissed picker must not repeatedly auto-open',
)
assert.equal(
  decideLiveTerminal({ code: 'box_draft', agent: 'claude', hasBind: true, draft: 'hello', digest: DIGEST, dismissedReason: 'draft' }),
  'hint',
  'dismissed draft must not repeatedly re-show DraftCard',
)
assert.equal(
  decideLiveTerminal({ code: 'subagent_view', agent: 'claude', hasBind: true, dismissedReason: 'picker' }),
  'open',
  'different reason should still open',
)

// 6. Trigger reason disappears -> auto-close & restore composer.
for (const reason of CONFIRMED_LIVE_CODES) {
  assert.equal(
    isBlockerResolved(reason, { screenChecked: true, screenBlocker: { code: '' } }),
    true,
    `${reason} must resolve when screen shows clean empty input box`,
  )
  assert.equal(
    isBlockerResolved(reason, { screenChecked: true, screenBlocker: { code: 'no_input' } }),
    false,
    `${reason} must not falsely resolve when screen is unreadable/no_input`,
  )
  assert.equal(
    isBlockerResolved(reason, { screenChecked: true, screenBlocker: { code: reason } }),
    false,
    `${reason} must stay open while still present on screen`,
  )

  const openState = { bind: 's1 p1', reason, mode: 'terminal', openedEndSeq: 10 }
  const cleared = reduceBlockerOnScreen(openState, 'claude', { screenChecked: true, screenBlocker: { code: '' } })
  assert.deepEqual(cleared, { next: null, resolved: true }, `${reason} reduces to null when cleared`)
}

// Working status resolves prompt/picker/typed_not_sent, but does NOT falsely resolve box_draft.
assert.equal(isBlockerResolved('agent_waiting', { agentStatus: 'working' }), true)
assert.equal(isBlockerResolved('picker', { agentStatus: 'working' }), true)
assert.equal(isBlockerResolved('typed_not_sent', { agentStatus: 'working' }), true)
assert.equal(isBlockerResolved('box_draft', { agentStatus: 'working' }), false)
assert.equal(isBlockerResolved('subagent_view', { agentStatus: 'working' }), false)

// Log advancing resolves stuck-send and prompt states.
for (const reason of ['agent_waiting', 'picker', 'typed_not_sent', 'typed_other_view', 'enter_not_taken']) {
  assert.equal(isBlockerResolved(reason, { logAdvanced: true }), true, `${reason} resolves when log advances`)
}

// DraftCard state transitions: updated draft text refreshes in place; new terminal-only blocker escalates to terminal.
const draftCardState = { bind: 's1 p1', reason: 'box_draft', mode: 'draft_card', draft: 'old', digest: DIGEST, openedEndSeq: 5 }
const digest2 = 'b'.repeat(64)
const updated = reduceBlockerOnScreen(draftCardState, 'claude', {
  screenChecked: true,
  screenBlocker: { code: 'box_draft', draft: 'new text', digest: digest2 },
})
assert.equal(updated.resolved, false)
assert.equal(updated.next?.mode, 'draft_card')
assert.equal(updated.next?.draft, 'new text')
assert.equal(updated.next?.digest, digest2)

const escalated = reduceBlockerOnScreen(draftCardState, 'claude', {
  screenChecked: true,
  screenBlocker: { code: 'subagent_view' },
})
assert.equal(escalated.resolved, false)
assert.equal(escalated.next?.mode, 'terminal')
assert.equal(escalated.next?.reason, 'subagent_view')

// 7. Native UI already handling the prompt (AskCard waits or dialog PanelWindow open)
//    must NOT stash a live popup behind it, and should clear any existing live popup.
for (const code of ['agent_waiting', 'picker', 'typed_not_sent', 'box_draft', 'unknown']) {
  assert.equal(decideLiveTerminal({ code, agent: 'claude', hasBind: true, waits: true }), 'none')
  assert.equal(decideLiveTerminal({ code, agent: 'claude', hasBind: true, panelOpen: true }), 'none')
}
assert.equal(shouldClearLiveTerminal({ waits: true, panelOpen: false }), true)
assert.equal(shouldClearLiveTerminal({ waits: false, panelOpen: true }), true)
assert.equal(shouldClearLiveTerminal({ waits: false, panelOpen: false }), false)

// 8. Antigravity CLI waiting refreshes for native AskCard instead of raw terminal.
assert.equal(decideLiveTerminal({ code: 'agent_waiting', agent: 'agy', hasBind: true }), 'refresh')
assert.equal(shouldOpenLiveTerminal({ code: 'agent_waiting', agent: 'agy', hasBind: true }), false)

// 9. Unbound or non-live agents (e.g. devin) never open live terminal.
assert.equal(liveFor('claude'), true)
assert.equal(liveFor('codex'), true)
assert.equal(liveFor('opencode'), true)
assert.equal(liveFor('agy'), true)
assert.equal(liveFor('devin'), false)
assert.equal(decideLiveTerminal({ code: 'agent_waiting', agent: 'devin', hasBind: true }), 'none')
assert.equal(decideLiveTerminal({ code: 'agent_waiting', agent: 'claude', hasBind: false }), 'none')

console.log('liveTerminalPolicy: all tests passed')
