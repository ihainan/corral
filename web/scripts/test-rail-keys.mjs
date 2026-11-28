import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { fileURLToPath } from 'node:url'
import ts from 'typescript'

const srcPath = fileURLToPath(new URL('../src/terminal/railKeys.ts', import.meta.url))
const src = readFileSync(srcPath, 'utf8')
const { outputText } = ts.transpileModule(src, {
  compilerOptions: { module: ts.ModuleKind.ESNext, target: ts.ScriptTarget.ES2022 },
})
const {
  RAIL_HOLD_MS,
  railDigit,
  railHoldTransition,
  decideRailKey,
} = await import(`data:text/javascript;base64,${Buffer.from(outputText).toString('base64')}`)

const ev = (over = {}) => ({
  key: '1',
  code: 'Digit1',
  altKey: true,
  ctrlKey: false,
  metaKey: false,
  shiftKey: false,
  isComposing: false,
  keyCode: 49,
  repeat: false,
  defaultPrevented: false,
  ...over,
})

const ctx = (over = {}) => ({
  railActive: true,
  modalOpen: false,
  mac: true,
  inTextField: false,
  inTerminal: false,
  keysFirst: 'terminal',
  ...over,
})

assert.ok(RAIL_HOLD_MS > 0)

// 1. railDigit: physical Digit1..Digit9 with Alt only (including macOS Option special chars).
assert.equal(railDigit(ev({ key: '¡', code: 'Digit1' })), 1)
assert.equal(railDigit(ev({ key: '™', code: 'Digit2' })), 2)
assert.equal(railDigit(ev({ key: '•', code: 'Digit8' })), 8)
assert.equal(railDigit(ev({ key: 'ª', code: 'Digit9' })), 9)
assert.equal(railDigit(ev({ key: 'º', code: 'Digit0' })), 0)
assert.equal(railDigit(ev({ key: '1', code: 'Numpad1' })), 0)
assert.equal(railDigit(ev({ key: 'a', code: 'KeyA' })), 0)
assert.equal(railDigit(ev({ altKey: false })), 0)
assert.equal(railDigit(ev({ ctrlKey: true })), 0) // AltGr on Windows/Linux
assert.equal(railDigit(ev({ metaKey: true })), 0)
assert.equal(railDigit(ev({ shiftKey: true })), 0)
assert.equal(railDigit(ev({ isComposing: true })), 0)
assert.equal(railDigit(ev({ keyCode: 229 })), 0)

// 2. railHoldTransition: holding Alt/Option starts timer only when rail is active and eligible.
const altDown = ev({ key: 'Alt', code: 'AltLeft', keyCode: 18 })
const altUp = ev({ key: 'Alt', code: 'AltLeft', altKey: false, keyCode: 18 })

assert.equal(railHoldTransition('keydown', altDown, { pending: false, shown: false }, ctx()), 'start-timer')
assert.equal(
  railHoldTransition('keydown', altDown, { pending: false, shown: false }, ctx({ inTerminal: true, keysFirst: 'terminal' })),
  'start-timer',
)
assert.equal(
  railHoldTransition('keydown', altDown, { pending: false, shown: false }, ctx({ railActive: false })),
  'none',
)
assert.equal(
  railHoldTransition('keydown', altDown, { pending: false, shown: false }, ctx({ modalOpen: true })),
  'none',
)
// On macOS inside a regular text input, Option types special characters (¡, ™, •): do not show rail hints.
assert.equal(
  railHoldTransition('keydown', altDown, { pending: false, shown: false }, ctx({ mac: true, inTextField: true })),
  'none',
)
// On Windows/Linux inside a regular text input, Alt does not type symbols: hints may still show.
assert.equal(
  railHoldTransition('keydown', altDown, { pending: false, shown: false }, ctx({ mac: false, inTextField: true })),
  'start-timer',
)
// Repeat or extra modifiers do not start the timer.
assert.equal(
  railHoldTransition('keydown', { ...altDown, repeat: true }, { pending: false, shown: false }, ctx()),
  'none',
)
assert.equal(
  railHoldTransition('keydown', { ...altDown, metaKey: true }, { pending: false, shown: false }, ctx()),
  'none',
)
assert.equal(
  railHoldTransition('keydown', { ...altDown, ctrlKey: true }, { pending: false, shown: false }, ctx()),
  'none',
)
assert.equal(
  railHoldTransition('keydown', { ...altDown, shiftKey: true }, { pending: false, shown: false }, ctx()),
  'none',
)
assert.equal(
  railHoldTransition('keydown', altDown, { pending: true, shown: false }, ctx()),
  'none',
)

// Quick chord before hold threshold cancels the pending timer so hints do not flash.
assert.equal(
  railHoldTransition('keydown', ev({ key: '¡', code: 'Digit1' }), { pending: true, shown: false }, ctx({ inTerminal: true })),
  'cancel',
)
assert.equal(
  railHoldTransition('keydown', ev({ key: 'ArrowUp', code: 'ArrowUp' }), { pending: true, shown: false }, ctx()),
  'cancel',
)

// While hints are shown, pressing a digit with only Alt keeps hints up for decideRailKey,
// while adding another modifier or releasing Alt hides them.
assert.equal(
  railHoldTransition('keydown', ev({ key: '¡', code: 'Digit1' }), { pending: false, shown: true }, ctx({ inTerminal: true })),
  'none',
)
assert.equal(
  railHoldTransition('keydown', ev({ key: 'i', code: 'KeyI', metaKey: true }), { pending: false, shown: true }, ctx()),
  'hide',
)
assert.equal(
  railHoldTransition('keyup', altUp, { pending: false, shown: true }, ctx()),
  'hide',
)
assert.equal(
  railHoldTransition('keyup', altUp, { pending: true, shown: false }, ctx()),
  'hide',
)
assert.equal(
  railHoldTransition('blur', null, { pending: false, shown: true }, ctx()),
  'hide',
)
assert.equal(
  railHoldTransition('hide-rail', null, { pending: false, shown: true }, ctx()),
  'hide',
)
assert.equal(
  railHoldTransition('focus-text', null, { pending: false, shown: true }, ctx({ mac: true, inTextField: true })),
  'hide',
)
assert.equal(
  railHoldTransition('focus-text', null, { pending: false, shown: true }, ctx({ mac: false, inTextField: true })),
  'none',
)

// 3. decideRailKey: inactive/hidden rail (e.g. background AgentsChat in TerminalTabs) must NEVER swallow the key!
assert.deepEqual(
  decideRailKey(ev({ key: '¡', code: 'Digit1' }), {
    ...ctx({ railActive: false, inTerminal: true }),
    hintsShown: true,
    itemCount: 7,
    currentIndex: 0,
  }),
  { action: 'ignore' },
)

// 4. decideRailKey in a focused terminal with default keysFirst='terminal':
//    - Quick Option+1 (hints not shown): ignored so xterm sends ESC+1 (or ¡ if optionIsMeta=false) to herdr/tmux/shell.
assert.deepEqual(
  decideRailKey(ev({ key: '¡', code: 'Digit1' }), {
    ...ctx({ inTerminal: true, keysFirst: 'terminal' }),
    hintsShown: false,
    itemCount: 7,
    currentIndex: 1,
  }),
  { action: 'ignore' },
)
//    - Held Option (hints shown) + 1: navigates to Agents (index 0) and hides hints, without terminal-taken toast.
assert.deepEqual(
  decideRailKey(ev({ key: '¡', code: 'Digit1' }), {
    ...ctx({ inTerminal: true, keysFirst: 'terminal' }),
    hintsShown: true,
    itemCount: 7,
    currentIndex: 1,
  }),
  { action: 'navigate', index: 0, notifyTakenFromTerminal: false, hideHints: true },
)
//    - Held Option (hints shown) + 2 while already on Terminals (index 1): consumes without leaking to PTY.
assert.deepEqual(
  decideRailKey(ev({ key: '™', code: 'Digit2' }), {
    ...ctx({ inTerminal: true, keysFirst: 'terminal' }),
    hintsShown: true,
    itemCount: 7,
    currentIndex: 1,
  }),
  { action: 'consume-current', index: 1, hideHints: true },
)

// 5. decideRailKey in a focused terminal with keysFirst='corral':
//    - Quick Option+1 navigates immediately and triggers the one-time takenFromTerminal toast.
assert.deepEqual(
  decideRailKey(ev({ key: '¡', code: 'Digit1' }), {
    ...ctx({ inTerminal: true, keysFirst: 'corral' }),
    hintsShown: false,
    itemCount: 7,
    currentIndex: 1,
  }),
  { action: 'navigate', index: 0, notifyTakenFromTerminal: true, hideHints: false },
)
//    - Held Option (hints shown) + 1 navigates without the takenFromTerminal toast (explicitly requested via rail).
assert.deepEqual(
  decideRailKey(ev({ key: '¡', code: 'Digit1' }), {
    ...ctx({ inTerminal: true, keysFirst: 'corral' }),
    hintsShown: true,
    itemCount: 7,
    currentIndex: 1,
  }),
  { action: 'navigate', index: 0, notifyTakenFromTerminal: false, hideHints: true },
)

// 6. Outside a terminal: quick Option+2 switches immediately; macOS text fields never get hijacked.
assert.deepEqual(
  decideRailKey(ev({ key: '™', code: 'Digit2' }), {
    ...ctx({ inTerminal: false }),
    hintsShown: false,
    itemCount: 7,
    currentIndex: 0,
  }),
  { action: 'navigate', index: 1, notifyTakenFromTerminal: false, hideHints: false },
)
assert.deepEqual(
  decideRailKey(ev({ key: '•', code: 'Digit8' }), {
    ...ctx({ mac: true, inTextField: true }),
    hintsShown: true,
    itemCount: 8,
    currentIndex: 0,
  }),
  { action: 'ignore' },
)
// Non-Mac text input still switches on Alt+digit.
assert.deepEqual(
  decideRailKey(ev({ key: '2', code: 'Digit2' }), {
    ...ctx({ mac: false, inTextField: true }),
    hintsShown: false,
    itemCount: 7,
    currentIndex: 0,
  }),
  { action: 'navigate', index: 1, notifyTakenFromTerminal: false, hideHints: false },
)

// 7. Guards: out-of-range digit, modal open, already defaultPrevented, key repeat.
assert.deepEqual(
  decideRailKey(ev({ key: '8', code: 'Digit8' }), {
    ...ctx(),
    hintsShown: true,
    itemCount: 5,
    currentIndex: 0,
  }),
  { action: 'ignore' },
)
assert.deepEqual(
  decideRailKey(ev({ key: '1', code: 'Digit1' }), {
    ...ctx({ modalOpen: true }),
    hintsShown: true,
    itemCount: 7,
    currentIndex: 1,
  }),
  { action: 'ignore' },
)
assert.deepEqual(
  decideRailKey(ev({ key: '1', code: 'Digit1', defaultPrevented: true }), {
    ...ctx(),
    hintsShown: true,
    itemCount: 7,
    currentIndex: 1,
  }),
  { action: 'ignore' },
)
assert.deepEqual(
  decideRailKey(ev({ key: '1', code: 'Digit1', repeat: true }), {
    ...ctx(),
    hintsShown: true,
    itemCount: 7,
    currentIndex: 1,
  }),
  { action: 'consume-current', index: 0, hideHints: false },
)

console.log('railKeys: all tests passed')
