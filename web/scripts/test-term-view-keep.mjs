import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { fileURLToPath } from 'node:url'
import ts from 'typescript'

const srcPath = fileURLToPath(new URL('../src/terminal/termViewKeep.ts', import.meta.url))
const src = readFileSync(srcPath, 'utf8')
const { outputText } = ts.transpileModule(src, {
  compilerOptions: { module: ts.ModuleKind.ESNext, target: ts.ScriptTarget.ES2022 },
})
const {
  resolveTermViewKeep,
  shouldResizePty,
  paneFocusAction,
  onPaneTerminalClaim,
  claimPaneTerminal,
  isTerminalInputKey,
  isTerminalBeforeInput,
} = await import(`data:text/javascript;base64,${Buffer.from(outputText).toString('base64')}`)

const K1 = 's1 p1 100 200 sess-a'
const K2 = 's1 p1 101 201 sess-b'

// 1. Before first open, chat view must NOT eagerly mount or connect the main terminal.
assert.deepEqual(
  resolveTermViewKeep({ keptKey: null, currentKey: K1, canTermView: true, termShown: false, peerNeedsPane: false }),
  { keptKey: null, mounted: false, visible: false },
)

// 2. First Ctrl+` to terminal mounts and records keptKey.
assert.deepEqual(
  resolveTermViewKeep({ keptKey: null, currentKey: K1, canTermView: true, termShown: true, peerNeedsPane: false }),
  { keptKey: K1, mounted: true, visible: true },
)

// 3. Ctrl+` back to chat keeps the already-mounted terminal alive (hidden, not unmounted/reconnected).
assert.deepEqual(
  resolveTermViewKeep({ keptKey: K1, currentKey: K1, canTermView: true, termShown: false, peerNeedsPane: false }),
  { keptKey: K1, mounted: true, visible: false },
)

// 4. Rapid Ctrl+` back to terminal reuses the kept instance with no unmount gap.
assert.deepEqual(
  resolveTermViewKeep({ keptKey: K1, currentKey: K1, canTermView: true, termShown: true, peerNeedsPane: false }),
  { keptKey: K1, mounted: true, visible: true },
)

// 5. While hidden in chat, if a peer panel (dock terminal / composer panel / inline live) needs the pane,
//    the hidden main terminal yields immediately so backend's single pane-ws slot does not 409.
assert.deepEqual(
  resolveTermViewKeep({ keptKey: K1, currentKey: K1, canTermView: true, termShown: false, peerNeedsPane: true }),
  { keptKey: null, mounted: false, visible: false },
)

// 6. While main terminal is visible, it owns the pane even if dockTab was previously 'terminal' (Dock hides its tab via noTerminal).
assert.deepEqual(
  resolveTermViewKeep({ keptKey: K1, currentKey: K1, canTermView: true, termShown: true, peerNeedsPane: true }),
  { keptKey: K1, mounted: true, visible: true },
)

// 7. Switching to another agent or a restarted process/session drops the previous kept terminal
//    and does not eagerly connect the new one while in chat view.
assert.deepEqual(
  resolveTermViewKeep({ keptKey: K1, currentKey: K2, canTermView: true, termShown: false, peerNeedsPane: false }),
  { keptKey: null, mounted: false, visible: false },
)

// 8. Narrowing below TERM_VIEW_MIN or losing bind unmounts cleanly.
assert.deepEqual(
  resolveTermViewKeep({ keptKey: K1, currentKey: K1, canTermView: false, termShown: false, peerNeedsPane: false }),
  { keptKey: null, mounted: false, visible: false },
)
assert.deepEqual(
  resolveTermViewKeep({ keptKey: K1, currentKey: null, canTermView: true, termShown: false, peerNeedsPane: false }),
  { keptKey: null, mounted: false, visible: false },
)

// 9. shouldResizePty deduplicates identical dimensions and rejects zero/invalid dimensions.
assert.equal(shouldResizePty(null, { cols: 120, rows: 40 }), true)
assert.equal(shouldResizePty({ cols: 120, rows: 40 }, { cols: 120, rows: 40 }), false)
assert.equal(shouldResizePty({ cols: 120, rows: 40 }, { cols: 121, rows: 40 }), true)
assert.equal(shouldResizePty({ cols: 120, rows: 40 }, { cols: 120, rows: 39 }), true)
assert.equal(shouldResizePty({ cols: 120, rows: 40 }, { cols: 0, rows: 40 }), false)
assert.equal(shouldResizePty({ cols: 120, rows: 40 }, { cols: 120, rows: 0 }), false)
assert.equal(shouldResizePty(null, null), false)

// 10. paneFocusAction focuses main terminal on false->true transition, blurs on true->false,
//     never steals focus when staying active or for non-main live panels / coarse / modal.
assert.equal(
  paneFocusAction({ active: true, wasActive: false, coarse: false, isLive: true, isMain: true, modalOpen: false, hasFocus: false }),
  'focus',
)
assert.equal(
  paneFocusAction({ active: true, wasActive: true, coarse: false, isLive: true, isMain: true, modalOpen: false, hasFocus: false }),
  'none',
)
assert.equal(
  paneFocusAction({ active: false, wasActive: true, coarse: false, isLive: true, isMain: true, modalOpen: false, hasFocus: true }),
  'blur',
)
assert.equal(
  paneFocusAction({ active: false, wasActive: true, coarse: false, isLive: true, isMain: true, modalOpen: false, hasFocus: false }),
  'none',
)
assert.equal(
  paneFocusAction({ active: true, wasActive: false, coarse: false, isLive: true, isMain: false, modalOpen: false, hasFocus: false }),
  'none',
)
assert.equal(
  paneFocusAction({ active: true, wasActive: false, coarse: false, isLive: false, isMain: false, modalOpen: false, hasFocus: false }),
  'focus',
)
assert.equal(
  paneFocusAction({ active: true, wasActive: false, coarse: true, isLive: true, isMain: true, modalOpen: false, hasFocus: false }),
  'none',
)
assert.equal(
  paneFocusAction({ active: true, wasActive: false, coarse: false, isLive: true, isMain: true, modalOpen: true, hasFocus: false }),
  'none',
)

// 11. Peer pane claim notifies matching (sid, pane) listener only and unsubscribes cleanly.
let hitsA = 0
let hitsB = 0
const offA = onPaneTerminalClaim('s1', 'pane-1', () => hitsA++)
const offB = onPaneTerminalClaim('s1', 'pane-2', () => hitsB++)
claimPaneTerminal('s1', 'pane-1')
assert.equal(hitsA, 1)
assert.equal(hitsB, 0)
offA()
claimPaneTerminal('s1', 'pane-1')
assert.equal(hitsA, 1)
claimPaneTerminal('s1', 'pane-2')
assert.equal(hitsB, 1)
offB()
claimPaneTerminal('s1', 'pane-2')
assert.equal(hitsB, 1)

// 12. isTerminalInputKey accepts real input keys (including Shift+F3) and ignores modifiers/meta/keyup/IME.
for (const ev of [
  { type: 'keydown', key: 'y' },
  { type: 'keydown', key: 'Enter' },
  { type: 'keydown', key: 'Escape' },
  { type: 'keydown', key: 'F3', shiftKey: true },
  { type: 'keydown', key: 'ArrowUp' },
  { type: 'keydown', key: 'c', ctrlKey: true },
]) {
  assert.equal(isTerminalInputKey(ev), true, `expected input key: ${JSON.stringify(ev)}`)
}
for (const ev of [
  { type: 'keyup', key: 'y' },
  { type: 'keydown', key: 'Shift' },
  { type: 'keydown', key: 'Control' },
  { type: 'keydown', key: 'Alt' },
  { type: 'keydown', key: 'Meta' },
  { type: 'keydown', key: 'c', metaKey: true },
  { type: 'keydown', key: 'a', isComposing: true },
  { type: 'keydown', key: 'Process', keyCode: 229 },
  { type: 'keydown', key: 'Unidentified' },
]) {
  assert.equal(isTerminalInputKey(ev), false, `expected non-input key: ${JSON.stringify(ev)}`)
}

// 13. isTerminalBeforeInput accepts mobile/paste edits and ignores mid-composition previews.
assert.equal(isTerminalBeforeInput({ inputType: 'insertText', data: 'a' }), true)
assert.equal(isTerminalBeforeInput({ inputType: 'deleteContentBackward', data: null }), true)
assert.equal(isTerminalBeforeInput({ inputType: 'insertFromPaste', data: 'hi' }), true)
assert.equal(isTerminalBeforeInput({ inputType: 'insertCompositionText', data: 'ni' }), false)
assert.equal(isTerminalBeforeInput({ inputType: 'deleteCompositionText', data: null }), false)

console.log('termViewKeep: all tests passed')
