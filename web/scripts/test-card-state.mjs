import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { fileURLToPath } from 'node:url'
import ts from 'typescript'

const srcPath = fileURLToPath(new URL('../src/terminal/cardState.ts', import.meta.url))
const src = readFileSync(srcPath, 'utf8')
const { outputText } = ts.transpileModule(src, {
  compilerOptions: { module: ts.ModuleKind.ESNext, target: ts.ScriptTarget.ES2022 },
})
const {
  activeBusyKey,
  cardSignature,
  expireCardSettle,
  failCardPress,
  finishCardPress,
  initialCardState,
  pollCardFailed,
  pollCardFound,
  pressModeOf,
  resetCardState,
  startCardPress,
  visibleCard,
} = await import(`data:text/javascript;base64,${Buffer.from(outputText).toString('base64')}`)

const offerCard = {
  state: 'offer',
  tag: 'Heads up',
  text: 'Performance tests must be marked exclusive.',
  choices: [
    { key: '1', label: 'Learn more' },
    { key: '2', label: 'Knew this already' },
    { key: '0', label: 'Dismiss' },
  ],
  actionable: true,
  empty: true,
}

const makingCard = {
  state: 'making',
  tag: 'Heads up',
  text: 'One moment…',
  choices: null,
  actionable: false,
  empty: true,
}

const explainedCard = {
  state: 'explained',
  tag: 'Heads up',
  text: 'Detailed explanation of exclusive tests.',
  choices: [
    { key: '1', label: 'Understood' },
    { key: '2', label: 'Chat in main session' },
  ],
  actionable: true,
  empty: true,
}

// 1. Button press mode classification.
assert.equal(pressModeOf('Learn more'), 'keep')
assert.equal(pressModeOf('Unknown future action'), 'keep')
for (const label of [
  'Knew this already',
  'Know this already',
  'Dismiss',
  'Understood',
  'Chat in main session',
  'Disable',
  'Helpful',
  'Not helpful',
  'Didn’t read',
  "Didn't read",
  'OK',
]) {
  assert.equal(pressModeOf(label), 'dismiss', `expected ${label} to dismiss`)
}

// 2. Signature distinguishes state/text/choices changes.
assert.notEqual(cardSignature(offerCard), cardSignature(makingCard))
assert.notEqual(cardSignature(offerCard), cardSignature(explainedCard))
assert.equal(cardSignature(offerCard), cardSignature({ ...offerCard, empty: false }))

// 3. Clicking Dismiss hides the card immediately and blocks duplicate presses.
let s = pollCardFound(initialCardState(), offerCard, 1000)
assert.deepEqual(visibleCard(s, 1000), offerCard)
assert.equal(activeBusyKey(s, 1000), null)

const dismissStart = startCardPress(s, '0', 'Dismiss')
assert.equal(dismissStart.accepted, true)
s = dismissStart.state
assert.equal(visibleCard(s, 1000), null, 'dismissed card must disappear immediately')
assert.equal(activeBusyKey(s, 1000), null)

// Rapid second press before backend responds is rejected (no duplicate key send).
const dup = startCardPress(s, '2', 'Knew this already')
assert.equal(dup.accepted, false)

// Stale poll returning the same card while sending does NOT flash the card back.
s = pollCardFound(s, offerCard, 1200)
assert.equal(visibleCard(s, 1200), null)

// Backend succeeds -> enters settling; stale poll still returning same card stays hidden.
s = finishCardPress(s, dismissStart.pressId, 1500, 16000)
assert.equal(visibleCard(s, 2000), null)
s = pollCardFound(s, offerCard, 2500)
assert.equal(visibleCard(s, 2500), null)

// Poll failure during settling does not flash the dismissed card back.
s = pollCardFailed(s)
assert.equal(visibleCard(s, 3000), null)

// Once poll confirms card is gone (null), pending clears cleanly.
s = pollCardFound(s, null, 3500)
assert.equal(s.pending, null)
assert.equal(visibleCard(s, 3500), null)

// 4. Failure on Dismiss restores the card with error reason and bumps errorSeq.
let sFail = pollCardFound(initialCardState(), offerCard, 1000)
const dFail = startCardPress(sFail, '2', 'Knew this already')
assert.equal(dFail.accepted, true)
sFail = dFail.state
assert.equal(visibleCard(sFail, 1000), null)

sFail = failCardPress(sFail, dFail.pressId, 'card_box_typed', 'Input box is not empty.')
assert.deepEqual(visibleCard(sFail, 1500), offerCard, 'failed dismiss must restore the card')
assert.equal(sFail.error, 'Input box is not empty.')
assert.equal(sFail.errorSeq, 1)
assert.equal(sFail.pending, null)

// 5. Failure with card_gone does NOT restore a card that is already gone.
let sGone = pollCardFound(initialCardState(), offerCard, 1000)
const dGone = startCardPress(sGone, '0', 'Dismiss')
sGone = failCardPress(dGone.state, dGone.pressId, 'card_gone', 'The card is gone.')
assert.equal(visibleCard(sGone, 1200), null)
assert.equal(sGone.card, null)
assert.equal(sGone.error, '')

// 6. Clicking "Learn more" keeps card visible with busyKey until card updates.
let sLearn = pollCardFound(initialCardState(), offerCard, 1000)
const learnStart = startCardPress(sLearn, '1', 'Learn more')
assert.equal(learnStart.accepted, true)
sLearn = learnStart.state
assert.deepEqual(visibleCard(sLearn, 1000), offerCard)
assert.equal(activeBusyKey(sLearn, 1000), '1')

// Duplicate press while sending or settling is rejected.
assert.equal(startCardPress(sLearn, '1', 'Learn more').accepted, false)
sLearn = finishCardPress(sLearn, learnStart.pressId, 1800, 16000)
assert.equal(activeBusyKey(sLearn, 2000), '1', 'stays busy while waiting for card transition')
assert.equal(startCardPress(sLearn, '0', 'Dismiss').accepted, false)

// Stale poll showing the same offerCard keeps busyKey active.
sLearn = pollCardFound(sLearn, offerCard, 2500)
assert.equal(activeBusyKey(sLearn, 2500), '1')

// Poll showing the new makingCard clears pending and busyKey.
sLearn = pollCardFound(sLearn, makingCard, 3000)
assert.equal(activeBusyKey(sLearn, 3000), null)
assert.deepEqual(visibleCard(sLearn, 3000), makingCard)

// Poll showing explainedCard updates cleanly.
sLearn = pollCardFound(sLearn, explainedCard, 5000)
assert.deepEqual(visibleCard(sLearn, 5000), explainedCard)

// 7. Settle timeout releases pending if terminal ignored the key.
let sTimeout = pollCardFound(initialCardState(), offerCard, 1000)
const tStart = startCardPress(sTimeout, '0', 'Dismiss')
sTimeout = finishCardPress(tStart.state, tStart.pressId, 2000, 5000)
assert.equal(visibleCard(sTimeout, 6999), null)
sTimeout = expireCardSettle(sTimeout, 7000)
assert.equal(sTimeout.pending, null)
assert.deepEqual(visibleCard(sTimeout, 7000), offerCard)

// 8. Switching conversations ignores late completion/failure from the old conversation.
let sConv = pollCardFound(initialCardState(), offerCard, 1000)
const oldPress = startCardPress(sConv, '0', 'Dismiss')
sConv = resetCardState(oldPress.state)
sConv = pollCardFound(sConv, explainedCard, 2000)
sConv = failCardPress(sConv, oldPress.pressId, 'no_answer', 'network error')
assert.deepEqual(visibleCard(sConv, 2100), explainedCard)
assert.equal(sConv.error, '')

// 9. Guard rejects invalid card states or unknown keys.
const staleState = pollCardFailed(pollCardFound(initialCardState(), offerCard, 1000))
assert.equal(startCardPress(staleState, '0', 'Dismiss').accepted, false)

const typedState = pollCardFound(initialCardState(), { ...offerCard, empty: false }, 1000)
assert.equal(startCardPress(typedState, '0', 'Dismiss').accepted, false)

const unreadableState = pollCardFound(initialCardState(), { ...offerCard, actionable: false }, 1000)
assert.equal(startCardPress(unreadableState, '0', 'Dismiss').accepted, false)

const validState = pollCardFound(initialCardState(), offerCard, 1000)
assert.equal(startCardPress(validState, '9', 'Dismiss').accepted, false)

console.log('cardState: all tests passed')
