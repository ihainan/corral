import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { fileURLToPath } from 'node:url'
import React from 'react'
import ts from 'typescript'

const readSrc = (rel) => readFileSync(fileURLToPath(new URL(rel, import.meta.url)), 'utf8')
const parseTsx = (rel) => ts.createSourceFile(rel, readSrc(rel), ts.ScriptTarget.Latest, true, ts.ScriptKind.TSX)

const policySrc = readSrc('../src/terminal/agentTabPolicy.ts')
const policyCode = ts.transpileModule(policySrc, {
  compilerOptions: { module: ts.ModuleKind.ESNext, target: ts.ScriptTarget.ES2022 },
}).outputText
const policy = await import(`data:text/javascript;base64,${Buffer.from(policyCode).toString('base64')}`)

const transpileFn = (srcFile, fnName, extra = '') => {
  const node = srcFile.statements.find((s) => ts.isFunctionDeclaration(s) && s.name?.text === fnName)
  assert.ok(node, `expected function ${fnName} in ${srcFile.fileName}`)
  const raw = node.getText(srcFile).replace(/^export\s+/, '')
  return ts.transpileModule(`${raw}\n${extra}`, {
    compilerOptions: {
      module: ts.ModuleKind.ESNext,
      target: ts.ScriptTarget.ES2022,
      jsx: ts.JsxEmit.React,
    },
  }).outputText
}

// Lightweight React-element tree inspector for direct component execution without DOM.
function findAll(node, pred, out = []) {
  if (!node || typeof node !== 'object') return out
  if (Array.isArray(node)) {
    for (const item of node) findAll(item, pred, out)
    return out
  }
  if (React.isValidElement(node)) {
    if (pred(node)) out.push(node)
    if (node.props && 'children' in node.props) findAll(node.props.children, pred, out)
  }
  return out
}

function makeHooks() {
  const states = []
  const refs = []
  let sIdx = 0
  let rIdx = 0
  return {
    reset() {
      sIdx = 0
      rIdx = 0
    },
    useState(init) {
      const i = sIdx++
      if (i >= states.length) states.push(typeof init === 'function' ? init() : init)
      const set = (v) => {
        states[i] = typeof v === 'function' ? v(states[i]) : v
      }
      return [states[i], set]
    },
    useRef(init) {
      const i = rIdx++
      if (i >= refs.length) refs.push({ current: init })
      return refs[i]
    },
    useEffect() {},
    useLayoutEffect() {},
    useContext() {
      return undefined
    },
  }
}

const askCardFile = parseTsx('../src/terminal/askCard.tsx')
const replyFile = parseTsx('../src/terminal/ReplyDialog.tsx')
const agentsChatFile = parseTsx('../src/pages/AgentsChat.tsx')

// Build real transpiled OwnBox, AskPanel, PlanPanel, ReviewPanel, Choices, useDrive from askCard.tsx
function loadAskCardFns(hooks) {
  const code = [
    transpileFn(askCardFile, 'useDrive'),
    transpileFn(askCardFile, 'OwnBox'),
    transpileFn(askCardFile, 'AskPanel'),
    transpileFn(askCardFile, 'ReviewPanel'),
    transpileFn(askCardFile, 'PlanPanel'),
    transpileFn(askCardFile, 'Choices'),
  ].join('\n')
  const cardDrafts = new Map()
  const cardTicks = new Map()
  const env = {
    React,
    ...hooks,
    t: (k) => k,
    cardDrafts,
    cardTicks,
    keepDraft: () => {},
    ownOK: (s) => s.trim().length > 0,
    choiceText: (l) => ({ text: l }),
    lasting: (l) => (/always/i.test(l) ? 'rule' : ''),
    Markdown: ({ text }) => React.createElement('div', null, text),
    ToTerminal: () => null,
    readAskScreen: () => ({ kind: 'unknown' }),
    at: () => ({ kind: 'unknown' }),
    sameQ: () => true,
    step: () => new Promise(() => {}),
    pickWithEnter: () => new Promise(() => {}),
    tickAndGo: () => new Promise(() => {}),
    pickReview: () => new Promise(() => {}),
    ownWords: () => new Promise(() => {}),
    questionOwn: (n) => n,
    planChange: (n) => n,
    withAlive: (t) => t,
    Unsure: class Unsure extends Error {},
    ApiError: class ApiError extends Error {},
    isCorraldUnknownResult: () => false,
    NotAsShown: class NotAsShown extends Error {},
    Filled: class Filled extends Error {},
    MaybeSent: class MaybeSent extends Error {},
    refused: () => false,
  }
  return new Function(
    'env',
    `with (env) {\n${code}\nreturn { useDrive, OwnBox, AskPanel, ReviewPanel, PlanPanel, Choices }\n}`,
  )(env)
}

// ============================================================================
// 1. P1: AskUserQuestion (AskPanel / OwnBox), PlanPanel (OwnBox), and
//    Permission amend (InlineReply -> OwnBox) must call onTyped when typing.
// ============================================================================
{
  // 1a. OwnBox directly: non-empty input calls onTyped; whitespace-only does not.
  const hooks = makeHooks()
  const { OwnBox } = loadAskCardFns(hooks)
  let tabs = [{ id: 't1', key: 'A', s: 's1', preview: true }]
  let typedCount = 0
  const onTyped = () => {
    typedCount++
    tabs = policy.pinAgentTabByKey(tabs, 'A').tabs
  }
  const tree = OwnBox({ label: 'Custom', busy: false, onGo: () => {}, onTyped })
  const [input] = findAll(tree, (el) => el.type === 'input')
  assert.ok(input, 'OwnBox should render an input')
  input.props.onChange({ target: { value: '   ' } })
  assert.equal(typedCount, 0, 'whitespace-only input should not pin')
  assert.equal(tabs[0].preview, true)
  input.props.onChange({ target: { value: 'use postgres' } })
  assert.equal(typedCount, 1, 'non-empty input in OwnBox must call onTyped immediately')
  assert.equal(tabs[0].preview, undefined, 'preview tab A must be pinned after typing in OwnBox')
}

{
  // 1b. AskPanel forwards onTyped to OwnBox and onAct to multi-select option toggles.
  const hooks = makeHooks()
  const { AskPanel, OwnBox } = loadAskCardFns(hooks)
  const target = { key: 'k1', fill: async () => {}, screen: async () => ({ text: '', digest: 'd' }), reply: async () => {} }
  let typedCalls = 0
  let actCalls = 0
  const singleTree = AskPanel({
    call: { id: 'c1', name: 'AskUserQuestion', asks: [{ question: 'DB?', multi: false, options: [{ label: 'SQLite' }] }] },
    index: 0,
    shown: { kind: 'question', question: 'DB?', multi: false, beside: false, tabs: [], options: [{ n: 1, label: 'SQLite' }, { n: 2, label: 'Type something' }], own: 2 },
    target,
    drive: () => {},
    busy: false,
    alive: () => true,
    onTyped: () => typedCalls++,
    onAct: () => actCalls++,
  })
  const ownBoxes = findAll(singleTree, (el) => el.type === OwnBox)
  assert.equal(ownBoxes.length, 1, 'AskPanel should render OwnBox when own + fill are available')
  assert.equal(typeof ownBoxes[0].props.onTyped, 'function', 'AskPanel must pass onTyped to OwnBox')
  const obHooks = makeHooks()
  const { OwnBox: OwnBoxExec } = loadAskCardFns(obHooks)
  const [input] = findAll(OwnBoxExec(ownBoxes[0].props), (el) => el.type === 'input')
  input.props.onChange({ target: { value: 'custom db' } })
  assert.equal(typedCalls, 1, 'typing in AskPanel custom answer box must trigger onTyped')

  // Multi-select checkbox toggle must trigger onAct immediately before submit.
  const multiHooks = makeHooks()
  const { AskPanel: MultiAsk } = loadAskCardFns(multiHooks)
  const multiTree = MultiAsk({
    call: { id: 'c2', name: 'AskUserQuestion', asks: [{ question: 'Features?', multi: true, options: [{ label: 'Auth' }] }] },
    index: 0,
    shown: { kind: 'question', question: 'Features?', multi: true, beside: false, tabs: [], options: [{ n: 1, label: 'Auth' }] },
    target,
    drive: () => {},
    busy: false,
    alive: () => true,
    onTyped: () => typedCalls++,
    onAct: () => actCalls++,
  })
  const [optBtn] = findAll(multiTree, (el) => el.type === 'button' && el.props.className?.includes('convo-ask-opt'))
  assert.ok(optBtn, 'AskPanel multi should render option button')
  optBtn.props.onClick()
  assert.equal(actCalls, 1, 'toggling a multi-select option in AskPanel must pin immediately')
}

{
  // 1c. PlanPanel forwards onTyped to its OwnBox and onAct to Choices (including lasting first tap).
  const hooks = makeHooks()
  const { PlanPanel, OwnBox, Choices } = loadAskCardFns(hooks)
  const target = { key: 'k1', fill: async () => {}, screen: async () => ({ text: '', digest: 'd' }), reply: async () => {} }
  let typedCalls = 0
  let actCalls = 0
  const planTree = PlanPanel({
    call: { id: 'p1', name: 'ExitPlanMode', plan: '# Plan' },
    shown: {
      kind: 'plan',
      options: [
        { n: 1, label: 'Yes, always allow' },
        { n: 2, label: 'Tell Claude what to change' },
      ],
    },
    digest: 'd1',
    target,
    drive: () => {},
    busy: false,
    onScreen: () => {},
    alive: () => true,
    onTyped: () => typedCalls++,
    onAct: () => actCalls++,
  })
  const ownBoxes = findAll(planTree, (el) => el.type === OwnBox)
  assert.equal(ownBoxes.length, 1, 'PlanPanel should render OwnBox for plan changes')
  assert.equal(typeof ownBoxes[0].props.onTyped, 'function', 'PlanPanel must pass onTyped to OwnBox')
  ownBoxes[0].props.onTyped()
  assert.equal(typedCalls, 1)

  const [choicesEl] = findAll(planTree, (el) => el.type === Choices)
  assert.ok(choicesEl, 'PlanPanel should render Choices')
  const cHooks = makeHooks()
  const { Choices: ChoicesExec } = loadAskCardFns(cHooks)
  const [lastingBtn] = findAll(ChoicesExec(choicesEl.props), (el) => el.type === 'button')
  lastingBtn.props.onClick()
  assert.equal(actCalls, 1, 'first click on a lasting plan option must pin immediately')
}

// ============================================================================
// 1d & 2. InlineReply: permission OwnBox receives onTyped; clicking an option,
//    pressing an answer key, or driving a structured card pins IMMEDIATELY
//    (synchronously before the async reply request completes).
// ============================================================================
function loadInlineReplyEnv(hooks, { screen, opts, readState, target, capturedKeys = [] }) {
  const code = [
    transpileFn(replyFile, 'useReply'),
    transpileFn(replyFile, 'useConfirm'),
    transpileFn(replyFile, 'InlineReply'),
  ].join('\n')
  const AskPanelStub = (p) => React.createElement('AskPanelStub', p)
  const ReviewPanelStub = (p) => React.createElement('ReviewPanelStub', p)
  const PlanPanelStub = (p) => React.createElement('PlanPanelStub', p)
  const OwnBoxStub = (p) => React.createElement('OwnBoxStub', p)
  const askHooks = makeHooks()
  const { useDrive } = loadAskCardFns(askHooks)
  const env = {
    React,
    ...hooks,
    t: (k) => k,
    useToast: () => () => {},
    ShowTerminal: React.createContext(undefined),
    useFreshAsk: () => false,
    useWhenGone: () => ({ back() {}, gone() {} }),
    getReplyDraft: () => '',
    keepReplyDraft: () => {},
    choices: () => opts,
    readAskScreen: () => readState,
    askOn: (call, s) => (call?.name === 'AskUserQuestion' && s.kind === 'question' ? 0 : -1),
    planOn: () => false,
    screenCall: () => undefined,
    reviewAsDrawn: () => false,
    askLead: () => undefined,
    asksFromScreen: () => false,
    promptBlock: () => null,
    codexAsk: () => null,
    codexQuestionKey: () => null,
    codexOptions: (l) => l.map((label) => ({ label })),
    question: () => 'Allow bash?',
    questionText: (q) => q,
    choiceText: (l) => ({ text: l }),
    lasting: (l) => (/always/i.test(l) ? 'rule' : ''),
    folds: () => false,
    fromLog: () => null,
    agentName: () => 'Claude',
    shownLines: 40,
    isTouchOnly: false,
    useDrive,
    useAnswerKeys: (_on, _ready, _keys, _held, _ref, cb) => {
      capturedKeys.push(cb)
    },
    AskPanel: AskPanelStub,
    ReviewPanel: ReviewPanelStub,
    PlanPanel: PlanPanelStub,
    OwnBox: OwnBoxStub,
    PanelWindow: (p) => React.createElement('PanelWindowStub', p),
    AskHookHint: () => null,
    LoadError: () => null,
    Asked: () => null,
    ScreenText: () => null,
    RotateCw: () => null,
    ownWords: () => new Promise(() => {}),
    permissionAmend: (n, y) => ({ n, y }),
    codexAnswerWith: () => new Promise(() => {}),
    isCorraldUnknownResult: () => false,
    ApiError: class ApiError extends Error {},
    NotAsShown: class NotAsShown extends Error {},
    Filled: class Filled extends Error {},
    MaybeSent: class MaybeSent extends Error {},
    CodexNotAsShown: class CodexNotAsShown extends Error {},
    MaybeAnswered: class MaybeAnswered extends Error {},
    keepShown: (s) => s,
    document: { visibilityState: 'visible', hidden: false },
    window: { setTimeout, clearTimeout },
  }
  const fns = new Function(
    'env',
    `with (env) {\n${code}\nreturn { useReply, useConfirm, InlineReply }\n}`,
  )(env)
  return { ...fns, AskPanelStub, OwnBoxStub, PlanPanelStub, ReviewPanelStub, target, screen }
}

{
  // Test InlineReply on a permission card with Tab-to-amend OwnBoxes + pending reply promise:
  let resolveReply
  const pendingReply = new Promise((r) => {
    resolveReply = r
  })
  const screen = { text: 'Allow bash?\n1. Yes\n2. Yes, always\n3. No\nTab to amend', digest: 'dig-1' }
  const opts = [
    { key: '1', label: 'Yes' },
    { key: '2', label: 'Yes, always' },
    { key: '3', label: 'No' },
  ]
  const target = {
    key: 't-perm',
    agent: 'claude',
    fill: async () => {},
    screen: async () => screen,
    reply: () => pendingReply,
  }
  const capturedKeys = []

  let tabs = [{ id: 't1', key: 'A', s: 's1', preview: true }]
  let interactCalls = 0
  const onInteract = () => {
    interactCalls++
    tabs = policy.pinAgentTabByKey(tabs, 'A').tabs
  }

  const replyHooks = makeHooks()
  let setScreenRef
  const origUseState = replyHooks.useState.bind(replyHooks)
  let stateCall = 0
  replyHooks.useState = (init) => {
    const idx = stateCall++
    // In InlineReply: 0=sent, 1=shown, then inside useReply: 2=screen!
    if (idx === 2) {
      const pair = origUseState(screen)
      setScreenRef = pair[1]
      return pair
    }
    return origUseState(init)
  }
  const env2 = loadInlineReplyEnv(replyHooks, { screen, opts, readState: { kind: 'unknown' }, target, capturedKeys })
  const tree = env2.InlineReply({
    target,
    onTyped: onInteract,
    onAct: onInteract,
    onAnswered: () => {},
  })
  assert.ok(setScreenRef)

  // 1d. Permission card's Yes/No OwnBoxes must receive onTyped!
  const permOwnBoxes = findAll(tree, (el) => el.type === env2.OwnBoxStub)
  assert.equal(permOwnBoxes.length, 2, 'permission card should render No and Yes OwnBox')
  for (const ob of permOwnBoxes) {
    assert.equal(typeof ob.props.onTyped, 'function', 'permission OwnBox must receive onTyped')
  }

  // 2a. Clicking option "1. Yes" while reply request is still pending must pin A IMMEDIATELY!
  const optBtns = findAll(tree, (el) => el.type === 'button' && el.props.className?.includes('convo-ask-opt'))
  assert.equal(optBtns.length, 3)
  optBtns[0].props.onClick()
  assert.ok(interactCalls >= 1, 'clicking answer option must trigger onInteract synchronously before request settles')
  assert.equal(tabs[0].preview, undefined, 'preview tab A must already be pinned before reply settles')

  // Switching to B while reply is still pending must NOT replace A!
  const switched = policy.openAgentTab(tabs, 't1', { key: 'B', s: 's2' }, 'preview', () => 't2')
  assert.equal(switched.tabs.some((x) => x.key === 'A'), true, 'A must survive switching to B while reply is in flight')
  assert.equal(switched.tabs.some((x) => x.key === 'B' && x.preview), true, 'B opens in a new preview tab')
  resolveReply()

  // 2b. Clicking a lasting choice ("2. Yes, always") on first tap pins immediately too.
  tabs = [{ id: 't1', key: 'A', s: 's1', preview: true }]
  interactCalls = 0
  optBtns[1].props.onClick()
  assert.ok(interactCalls >= 1, 'first tap on lasting choice must pin immediately')
  assert.equal(tabs[0].preview, undefined)

  // 2c. Pressing an answer key ('1') via useAnswerKeys pins immediately before request settles.
  tabs = [{ id: 't1', key: 'A', s: 's1', preview: true }]
  interactCalls = 0
  assert.ok(capturedKeys.length > 0, 'useAnswerKeys callback should be registered')
  capturedKeys[capturedKeys.length - 1]('1')
  assert.ok(interactCalls >= 1, 'pressing answer key must pin immediately')
  assert.equal(tabs[0].preview, undefined)
}

{
  // 2d. Structured AskPanel inside InlineReply: InlineReply passes onTyped + onAct to AskPanel,
  //     and drive() calls onStart synchronously before async reply finishes.
  const screen = { text: 'Pick one?\n1. Foo\n2. Type something', digest: 'dig-2' }
  const readState = {
    kind: 'question',
    question: 'Pick one?',
    multi: false,
    beside: false,
    tabs: [],
    options: [
      { n: 1, label: 'Foo', cursor: true },
      { n: 2, label: 'Type something' },
    ],
    own: 2,
  }
  const target = {
    key: 't-ask',
    agent: 'claude',
    fill: async () => {},
    screen: async () => screen,
    reply: () => new Promise(() => {}),
  }
  const replyHooks = makeHooks()
  const origUseState = replyHooks.useState.bind(replyHooks)
  let stateCall = 0
  replyHooks.useState = (init) => {
    const idx = stateCall++
    return idx === 2 ? origUseState(screen) : origUseState(init)
  }
  const env = loadInlineReplyEnv(replyHooks, { screen, opts: [], readState, target })
  let tabs = [{ id: 't1', key: 'A', s: 's1', preview: true }]
  const onInteract = () => {
    tabs = policy.pinAgentTabByKey(tabs, 'A').tabs
  }
  const tree = env.InlineReply({
    target,
    call: { id: 'ask-1', name: 'AskUserQuestion', asks: [{ question: 'Pick one?', multi: false, options: [{ label: 'Foo' }] }] },
    onTyped: onInteract,
    onAct: onInteract,
    onAnswered: () => {},
  })
  const [askEl] = findAll(tree, (el) => el.type === env.AskPanelStub)
  assert.ok(askEl, 'InlineReply should render AskPanel for structured question')
  assert.equal(typeof askEl.props.onTyped, 'function', 'InlineReply must pass onTyped to AskPanel')
  assert.equal(typeof askEl.props.onAct, 'function', 'InlineReply must pass onAct to AskPanel')
  // Triggering drive() (as clicking an option in AskPanel does) must pin A synchronously:
  askEl.props.drive(() => new Promise(() => {}))
  assert.equal(tabs[0].preview, undefined, 'drive() must pin preview tab synchronously before promise settles')
}

// ============================================================================
// 3. P2: Phone layout (stack = true) must still pass onInteract so interacting
//    in phone layout pins the persisted preview tab instead of losing it.
// ============================================================================
{
  const agentsChatSrc = readSrc('../src/pages/AgentsChat.tsx')
  // Verify the actual JSX attributes passed to <AgentChat> in AgentsChat.tsx:
  const jsxAttrs = []
  function visit(node) {
    if (ts.isJsxOpeningElement(node) || ts.isJsxSelfClosingElement(node)) {
      if (node.tagName.getText(agentsChatFile) === 'AgentChat') {
        const onInteractAttr = node.attributes.properties.find(
          (p) => ts.isJsxAttribute(p) && p.name.getText(agentsChatFile) === 'onInteract',
        )
        assert.ok(onInteractAttr && onInteractAttr.initializer && ts.isJsxExpression(onInteractAttr.initializer), 'AgentChat must pass onInteract')
        jsxAttrs.push(onInteractAttr.initializer.expression.getText(agentsChatFile))
      }
    }
    ts.forEachChild(node, visit)
  }
  visit(agentsChatFile)
  assert.equal(jsxAttrs.length, 2, 'expected primary and secondary AgentChat elements')

  // Execute the ACTUAL onInteract expressions from AgentsChat.tsx with stack = true!
  let tabs = [{ id: 't1', key: 'A', s: 's1', preview: true }]
  const pinTabByKey = (key) => {
    const res = policy.pinAgentTabByKey(tabs, key)
    if (res.changed) tabs = res.tabs
  }
  for (const [idx, expr] of jsxAttrs.entries()) {
    const code = ts.transpileModule(`return (${expr})`, {
      compilerOptions: { module: ts.ModuleKind.ESNext, target: ts.ScriptTarget.ES2022 },
    }).outputText
    const fn = new Function('stack', 'k', 'sk', 'pinTabByKey', code)(true, 'A', 'A', pinTabByKey)
    assert.equal(typeof fn, 'function', `AgentChat #${idx + 1} onInteract must be a function even when stack=true`)
  }

  // Full phone-resize reproduction: desktop opens preview A -> narrow to phone (stack=true) ->
  // interact in A -> open B via sync effect -> widen back to desktop -> A must still be kept!
  const primaryExpr = jsxAttrs[1]
  const primaryOnInteract = new Function(
    'stack',
    'k',
    'pinTabByKey',
    ts.transpileModule(`return (${primaryExpr})`, {
      compilerOptions: { module: ts.ModuleKind.ESNext, target: ts.ScriptTarget.ES2022 },
    }).outputText,
  )(true, 'A', pinTabByKey)
  primaryOnInteract()
  assert.equal(tabs[0].preview, undefined, 'interacting in phone layout must pin A')
  const afterB = policy.openAgentTab(tabs, 't1', { key: 'B', s: 's2' }, 'preview', () => 't2')
  assert.deepEqual(
    afterB.tabs.map((x) => [x.key, !!x.preview]),
    [
      ['A', false],
      ['B', true],
    ],
    'after interacting in phone layout and opening B, A must stay pinned when returning to desktop',
  )

  // Also confirm <InlineReply> in AgentsChat.tsx does not wait for onAnswered to pin.
  let inlineReplyChecked = false
  function visitInline(node) {
    if (ts.isJsxOpeningElement(node) || ts.isJsxSelfClosingElement(node)) {
      if (node.tagName.getText(agentsChatFile) === 'InlineReply') {
        const names = new Set(
          node.attributes.properties
            .filter(ts.isJsxAttribute)
            .map((p) => p.name.getText(agentsChatFile)),
        )
        assert.equal(names.has('onTyped'), true, 'InlineReply must receive onTyped')
        assert.equal(names.has('onAct'), true, 'InlineReply must receive onAct')
        inlineReplyChecked = true
      }
    }
    ts.forEachChild(node, visitInline)
  }
  visitInline(agentsChatFile)
  assert.equal(inlineReplyChecked, true)
  assert.ok(agentsChatSrc.includes('expandSplitAgentTab'))
}

// ============================================================================
// 4. P2: Expanding the secondary split pane in AgentsChat.tsx must clear the
//    old split and open B as an independent pinned tab (not restore A|B split).
// ============================================================================
{
  const agentsFn = agentsChatFile.statements.find((s) => ts.isFunctionDeclaration(s) && s.name?.text === 'AgentsChat')
  assert.ok(agentsFn && agentsFn.body)
  const decls = new Map()
  for (const st of agentsFn.body.statements) {
    if (!ts.isVariableStatement(st)) continue
    for (const d of st.declarationList.declarations) {
      if (ts.isIdentifier(d.name) && d.initializer) decls.set(d.name.text, d.initializer.getText(agentsChatFile))
    }
  }
  for (const name of ['pinTab', 'pinTabByKey', 'choose', 'goTab', 'expandSplit']) {
    assert.ok(decls.has(name), `expected ${name} in AgentsChat`)
  }

  const bundle = ['pinTab', 'pinTabByKey', 'choose', 'goTab', 'expandSplit']
    .map((n) => `const ${n} = ${decls.get(n)}`)
    .join('\n')
  const js = ts.transpileModule(bundle, {
    compilerOptions: { module: ts.ModuleKind.ESNext, target: ts.ScriptTarget.ES2022 },
  }).outputText

  const entryA = { key: 'A', s: { id: 's1' }, pane: 'p1' }
  const entryB = { key: 'B', s: { id: 's1' }, pane: 'p2' }
  const tabsRef = { current: [{ id: 't1', key: 'A', s: 's1', pane: 'p1', split: 'B', focus: 'secondary' }] }
  const frontRef = { current: 't1' }
  const splitSeen = { current: { cur: 'A', split: 'B' } }
  const pickKey = { current: { s: 's1', pane: 'p1', key: 'A' } }
  const activeRef = { current: true }
  const named = { current: false }
  let splitKey = 'B'
  let focusedPane = 'secondary'
  let pick = { s: 's1', pane: 'p1', key: 'A' }
  let navTo = ''
  let idSeq = 0

  const env = {
    ...policy,
    tabsRef,
    frontRef,
    splitSeen,
    pickKey,
    activeRef,
    named,
    entries: [entryA, entryB],
    splitEntry: entryB,
    stack: false,
    screen: 'chat',
    pick,
    focusedPane,
    LAST: 'corral.last',
    local: { set() {} },
    history: { state: null },
    FROM_LIST: {},
    newTabId: () => `t-new-${++idSeq}`,
    tabPick: (x) => ({ s: x.s, pane: x.pane, key: x.key }),
    chatAddress: (x) => `/agents?s=${'s' in x && typeof x.s === 'object' ? x.s.id : x.s}&pane=${x.pane ?? ''}`,
    putTabs: (l) => {
      tabsRef.current = l
    },
    putFront: (id) => {
      frontRef.current = id
    },
    setSplitKey: (v) => {
      splitKey = v
    },
    setFocusedPane: (v) => {
      focusedPane = v
    },
    setPick: (v) => {
      pick = v
    },
    setGaveUp: () => {},
    setScreen: () => {},
    navigate: (url) => {
      navTo = url
    },
    openSplit: () => {},
  }
  const { expandSplit } = new Function('env', `with (env) {\n${js}\nreturn { expandSplit }\n}`)(env)

  expandSplit()

  assert.equal(splitKey, null, 'expandSplit must clear splitKey instead of restoring the old split')
  assert.equal(focusedPane, 'primary', 'expandSplit must focus primary pane')
  assert.equal(tabsRef.current.length, 2, 'expandSplit must leave A open and give B its own tab')
  assert.equal(tabsRef.current[0].key, 'A')
  assert.equal(tabsRef.current[0].split, undefined, 'A tab must have split cleared')
  assert.equal(tabsRef.current[1].key, 'B')
  assert.equal(tabsRef.current[1].split, undefined, 'expanded B tab must not have split')
  assert.equal(tabsRef.current[1].preview, undefined, 'expanded B tab must be pinned (not preview)')
  assert.equal(frontRef.current, tabsRef.current[1].id, 'front tab must switch to independent B tab')
  assert.equal(pick.key, 'B', 'pick must switch to B')
  assert.equal(navTo, '/agents?s=s1&pane=p2')
}

// ============================================================================
// 5. P2 (fix3a): InlineReply -> PanelWindow notice card "Continue" pins
//    SYNCHRONOUSLY on click, before the panel POST request resolves.
// ============================================================================
{
  const panelFile = parseTsx('../src/terminal/PanelWindow.tsx')
  const noticeSrc = readSrc('../src/terminal/noticeDialog.ts')
  const noticeMod = await import(
    `data:text/javascript;base64,${Buffer.from(
      ts.transpileModule(noticeSrc, { compilerOptions: { module: ts.ModuleKind.ESNext, target: ts.ScriptTarget.ES2022 } }).outputText,
    ).toString('base64')}`
  )

  // Step A: InlineReply in shownLive fallback mode must pass onInput={act} to PanelWindow.
  const screen = { text: '──────────────\nAn unrecognized confirmation notice\n\nEnter to continue · Esc to cancel', digest: 'dig-notice' }
  const target = {
    key: 't-live',
    agent: 'claude',
    live: { sid: 's1', press: () => ({ pane: '%1' }) },
    screen: async () => screen,
    reply: async () => {},
  }
  const replyHooks = makeHooks()
  const origUseState = replyHooks.useState.bind(replyHooks)
  let stateCall = 0
  replyHooks.useState = (init) => {
    const idx = stateCall++
    return idx === 2 ? origUseState(screen) : origUseState(init)
  }
  let tabs = [{ id: 't1', key: 'A', s: 's1', preview: true }]
  const onInteract = () => {
    tabs = policy.pinAgentTabByKey(tabs, 'A').tabs
  }
  const replyEnv = loadInlineReplyEnv(replyHooks, { screen, opts: [], readState: { kind: 'unknown' }, target })
  const inlineTree = replyEnv.InlineReply({
    target,
    onTyped: onInteract,
    onAct: onInteract,
    onAnswered: () => {},
  })
  const [panelEl] = findAll(inlineTree, (el) => el.type?.name === 'PanelWindow' || el.type === 'PanelWindowStub')
  assert.ok(panelEl, 'InlineReply must render PanelWindow when falling back to live view')
  assert.equal(typeof panelEl.props.onInput, 'function', 'InlineReply must pass onInput to PanelWindow so clicks pin synchronously')

  // Step B: Execute actual PanelWindow with the props from InlineReply on a notice card,
  // with `api` hanging (never resolved yet), click "Continue", and assert A is pinned immediately!
  const panelHooks = makeHooks()
  let pStateIdx = 0
  const pOrigState = panelHooks.useState.bind(panelHooks)
  panelHooks.useState = (init) => {
    const idx = pStateIdx++
    // In PanelWindow: 0=screen, 1=loaded
    if (idx === 0) return pOrigState(screen)
    if (idx === 1) return pOrigState(true)
    return pOrigState(init)
  }
  let atRef
  const pOrigRef = panelHooks.useRef.bind(panelHooks)
  panelHooks.useRef = (init) => {
    const r = pOrigRef(init)
    if (init && typeof init === 'object' && 'queue' in init && 'shown' in init) {
      r.current.shown = screen.digest
      atRef = r
    }
    return r
  }

  let apiCalled = 0
  let resolveApi
  const pendingApi = new Promise((r) => {
    resolveApi = r
  })
  const PaneTerminalStub = (p) => React.createElement('PaneTerminalStub', p)
  const panelCode = transpileFn(panelFile, 'PanelWindow')
  const panelEnv = {
    React,
    Fragment: React.Fragment,
    ...panelHooks,
    useCallback: (fn) => fn,
    useMemo: (fn) => fn(),
    useId: () => 'id-1',
    useWhenGone: () => ({ back() {}, gone() {} }),
    t: (k) => k,
    isCoarse: () => false,
    claimPaneTerminal: () => {},
    ansiLines: (txt) => txt.split('\n').map((line) => [{ text: line }]),
    readNoticeDialog: noticeMod.readNoticeDialog,
    renderLinked: (txt) => txt,
    noticeLabel: (a) => a.hint,
    keyWord: (k) => k,
    settles: (k) => k === 'enter' || k === 'space' || k === 'shift+tab' || /^[0-9]$/.test(k),
    runStyle: () => ({}),
    shouldIgnorePanelPollError: () => false,
    PaneTerminal: PaneTerminalStub,
    ApiError: class ApiError extends Error {},
    errorText: (e) => String(e),
    api: () => {
      apiCalled++
      return pendingApi
    },
    EVERY: 800,
    EVERY_LIVE: 350,
    ESC_AGAIN: 1000,
    QUEUE: 16,
    FAILS: 4,
    NOTICE_LOOK_MS: 4000,
    SAID_FOR: 2000,
    STALE: 3000,
    PTY_TRIES: 2,
    TOUCH_KEYS: [],
    document: { hidden: false },
    setTimeout,
    clearTimeout,
  }
  const { PanelWindow } = new Function('env', `with (env) {\n${panelCode}\nreturn { PanelWindow }\n}`)(panelEnv)

  const noticeTree = PanelWindow(panelEl.props)
  const noticeBtns = findAll(noticeTree, (el) => el.type === 'button' && el.props.className?.includes('convo-notice-act'))
  assert.ok(noticeBtns.length >= 1, 'PanelWindow should render notice action button')
  assert.ok(atRef)

  // Click "Continue" while api() is still pending (unresolved):
  noticeBtns[0].props.onClick()
  assert.equal(apiCalled, 1, 'clicking Continue should trigger panel POST request')
  assert.equal(tabs[0].preview, undefined, 'clicking Continue on notice card must pin preview tab A synchronously before request resolves')

  // Switching to B right after clicking (before request resolves) must preserve A!
  const switched = policy.openAgentTab(tabs, 't1', { key: 'B', s: 's2' }, 'preview', () => 't2')
  assert.equal(switched.tabs.some((x) => x.key === 'A'), true, 'A must not be replaced by B while notice request is in flight')
  assert.equal(switched.tabs.some((x) => x.key === 'B' && x.preview), true)
  resolveApi({ text: '', digest: 'next' })

  // Step C: When PanelWindow renders PaneTerminal (usePty = true, no notice on screen),
  // it must forward onInput down to PaneTerminal.
  const ptyHooks = makeHooks()
  let ptyIdx = 0
  const ptyOrigState = ptyHooks.useState.bind(ptyHooks)
  ptyHooks.useState = (init) => {
    const idx = ptyIdx++
    if (idx === 0) return ptyOrigState({ text: 'plain terminal', digest: 'dig-pty' })
    if (idx === 1) return ptyOrigState(true)
    return ptyOrigState(init)
  }
  const { PanelWindow: PanelWindowPty } = new Function('env', `with (env) {\n${panelCode}\nreturn { PanelWindow }\n}`)({
    ...panelEnv,
    ...ptyHooks,
  })
  const ptyTree = PanelWindowPty(panelEl.props)
  const [paneTermEl] = findAll(ptyTree, (el) => el.type === PaneTerminalStub)
  assert.ok(paneTermEl, 'PanelWindow should render PaneTerminal when no notice card is shown')
  assert.equal(typeof paneTermEl.props.onInput, 'function', 'PanelWindow must forward onInput to PaneTerminal')
}

// ============================================================================
// 6. P2 (fix4 + fix5): PaneTerminal pins only when a user key actually emits
//    xterm onData (no key blacklist/whitelist), plus explicit input sources:
//    - Shift+PageUp, Shift+PageDown, Ctrl+Insert emit no onData -> do NOT pin
//    - Mac Option+N (Dead + optionIsMeta -> ESC+n) and Command+Backspace (-> DEL)
//      emit onData -> DO pin
//    - wheel/mouse-move/focus/CPR bytes via term.onData have no armed key -> do NOT pin
//    - Shift+F3 (ESC[1;2R) and uppercase A (via keypress) -> DO pin
//    - keyboard onData, Shift+Enter, IME commit, paste, mobile beforeinput, and
//      touch key bar pin even while WebSocket is connecting or closed
// ============================================================================
{
  const paneTermFile = parseTsx('../src/terminal/PaneTerminal.tsx')
  const paneWsSrc = readSrc('../src/terminal/paneWsMsg.ts')
  const termKeepSrc = readSrc('../src/terminal/termViewKeep.ts')
  const paneWsMod = await import(
    `data:text/javascript;base64,${Buffer.from(
      ts.transpileModule(paneWsSrc, { compilerOptions: { module: ts.ModuleKind.ESNext, target: ts.ScriptTarget.ES2022 } }).outputText,
    ).toString('base64')}`
  )
  const termKeepMod = await import(
    `data:text/javascript;base64,${Buffer.from(
      ts.transpileModule(termKeepSrc, { compilerOptions: { module: ts.ModuleKind.ESNext, target: ts.ScriptTarget.ES2022 } }).outputText,
    ).toString('base64')}`
  )
  assert.equal('isUserTerminalInput' in paneWsMod, false, 'paneWsMsg.ts must not classify PTY bytes')
  assert.equal('isTerminalInputKey' in termKeepMod, false, 'termViewKeep.ts must not maintain a key blacklist')

  const paneTermCode = transpileFn(paneTermFile, 'PaneTerminal')

  const sentFrames = []
  const fakeWs = {
    readyState: 1, // starts OPEN
    send(payload) {
      sentFrames.push(payload)
    },
    close() {},
  }
  const taListeners = new Map()
  const fakeTextarea = {
    addEventListener(type, fn) {
      taListeners.set(type, fn)
    },
    removeEventListener(type) {
      taListeners.delete(type)
    },
  }
  let customKeyHandler = null
  let onDataCb = null
  class FakeTerminal {
    constructor() {
      this.textarea = fakeTextarea
      this.unicode = {}
      this.options = {}
      this.cols = 80
      this.rows = 24
    }
    loadAddon() {}
    open() {}
    focus() {}
    blur() {}
    dispose() {}
    attachCustomKeyEventHandler(fn) {
      customKeyHandler = fn
    }
    onData(cb) {
      onDataCb = cb
      return { dispose() {} }
    }
  }

  const hooks = makeHooks()
  let refIdx = 0
  const origRef = hooks.useRef.bind(hooks)
  hooks.useRef = (init) => {
    const r = origRef(init)
    const idx = refIdx++
    // 0=hostRef, 1=wsRef
    if (idx === 0) r.current = { clientWidth: 400, clientHeight: 200, closest: () => null }
    if (idx === 1) r.current = fakeWs
    return r
  }

  const effects = []
  const env = {
    React,
    ...hooks,
    useCallback: (fn) => fn,
    useId: () => 'pty-1',
    useEffect(fn, deps) {
      // Run the mount-xterm effect (deps = [sid, pane]) so real key/textarea/onData handlers bind.
      if (Array.isArray(deps) && deps.length === 2 && deps[0] === 's1' && deps[1] === '%1') {
        effects.push(fn)
      }
    },
    useLayoutEffect() {},
    usePrefs: () => ({ fontSize: 13, optionIsMeta: true }),
    termScheme: () => ({ id: 'dark', theme: { background: '#000' } }),
    isCoarse: () => true,
    t: (k) => k,
    TOUCH_KEYS: [
      ['enter', 'Enter', '\r'],
      ['esc', 'Esc', '\x1b'],
    ],
    MAX_INPUT_CHUNK: 256 * 1024,
    PING_EVERY_MS: 25000,
    PONG_TIMEOUT_MS: 12000,
    ACK_BATCH_BYTES: 64 * 1024,
    FONT_STACK: 'mono',
    PANEL_THEME: {},
    WebSocket: { OPEN: 1, CONNECTING: 0, CLOSED: 3 },
    TextEncoder,
    ResizeObserver: class {
      observe() {}
      disconnect() {}
    },
    document: { activeElement: null },
    isActiveTerminalStatus: () => true,
    setLeaveGuardTerminal: () => {},
    classifyPaneWsMessage: paneWsMod.classifyPaneWsMessage,
    createTerminalKeyGate: termKeepMod.createTerminalKeyGate,
    isTerminalBeforeInput: termKeepMod.isTerminalBeforeInput,
    paneFocusAction: () => 'none',
    shouldResizePty: () => false,
    modalShown: () => false,
    openTerminalLink: () => {},
    Terminal: FakeTerminal,
    FitAddon: class {
      proposeDimensions() {
        return { cols: 80, rows: 24 }
      }
    },
    Unicode11Addon: class {},
    WebLinksAddon: class {},
    setTimeout,
    clearTimeout,
  }
  const { PaneTerminal } = new Function('env', `with (env) {\n${paneTermCode}\nreturn { PaneTerminal }\n}`)(env)

  let tabs = [{ id: 't1', key: 'A', s: 's1', preview: true }]
  let pinCalls = 0
  const onInput = () => {
    pinCalls++
    tabs = policy.pinAgentTabByKey(tabs, 'A').tabs
  }
  const resetPreview = () => {
    tabs = [{ id: 't1', key: 'A', s: 's1', preview: true }]
    pinCalls = 0
  }

  const tree = PaneTerminal({
    sid: 's1',
    pane: '%1',
    active: true,
    live: { note: 'waiting' },
    onSettled: () => {},
    onFallback: () => {},
    onInput,
  })
  assert.equal(effects.length, 1, 'mount xterm effect should be captured')
  effects[0]()
  assert.equal(typeof customKeyHandler, 'function', 'PaneTerminal must attach custom key handler')
  assert.equal(typeof onDataCb, 'function', 'PaneTerminal must subscribe to term.onData')

  // Emulates xterm CoreBrowserTerminal._keyDown/_keyPress: calls customKeyEventHandler,
  // and if allowed and xterm produces terminal bytes, synchronously fires term.onData(data).
  const dispatchKey = async (ev, xtermData = null) => {
    const allowed = customKeyHandler(ev)
    if (allowed !== false && xtermData) onDataCb(xtermData)
    await Promise.resolve()
  }

  // 6a. Wheel scrolling (alt-screen ESC[A/B, SGR Alt+wheel ESC[<73;10;5M), SGR mouse movement
  //     (ESC[<35;10;5M), and focus/CPR replies via term.onData must NOT pin preview tab A!
  for (const seq of ['\x1b[A', '\x1b[B', '\x1b[<73;10;5M', '\x1b[<35;10;5M', '\x1b[I', '\x1b[O', '\x1b[24;1R']) {
    onDataCb(seq)
  }
  assert.equal(pinCalls, 0, 'wheel scrolling, mouse movement, and terminal replies must NOT pin preview tab')
  assert.equal(tabs[0].preview, true)
  assert.equal(sentFrames.length, 7, 'terminal data still forwards over WebSocket')

  // 6b. fix5 non-input keys handled locally by xterm (no onData emitted) must NOT pin:
  //     1) Shift+PageUp (scrolls viewport only)
  //     2) Shift+PageDown (scrolls viewport only)
  //     3) Ctrl+Insert (copy only)
  await dispatchKey({ type: 'keydown', key: 'PageUp', keyCode: 33, shiftKey: true }, null)
  assert.equal(pinCalls, 0, 'Shift+PageUp (local scroll, no onData) must NOT pin preview tab')
  assert.equal(tabs[0].preview, true)

  await dispatchKey({ type: 'keydown', key: 'PageDown', keyCode: 34, shiftKey: true }, null)
  assert.equal(pinCalls, 0, 'Shift+PageDown (local scroll, no onData) must NOT pin preview tab')
  assert.equal(tabs[0].preview, true)

  await dispatchKey({ type: 'keydown', key: 'Insert', keyCode: 45, ctrlKey: true }, null)
  assert.equal(pinCalls, 0, 'Ctrl+Insert (copy, no onData) must NOT pin preview tab')
  assert.equal(tabs[0].preview, true)

  // Subsequent non-keyboard onData after a local-only key (e.g. holding Alt then scrolling wheel) must NOT pin:
  await dispatchKey({ type: 'keydown', key: 'Alt', altKey: true }, null)
  onDataCb('\x1b[<73;10;5M')
  assert.equal(pinCalls, 0, 'Alt+wheel after Alt keydown must NOT pin preview tab')

  // Pure modifiers, Meta copy shortcut, keyup, focus/blur, and F6 leave-focus (even if blur emits ESC[O]) must NOT pin:
  await dispatchKey({ type: 'keydown', key: 'Shift', shiftKey: true }, null)
  await dispatchKey({ type: 'keydown', key: 'Control', ctrlKey: true }, null)
  await dispatchKey({ type: 'keydown', key: 'c', metaKey: true }, null)
  await dispatchKey({ type: 'keyup', key: 'a' }, null)
  customKeyHandler({ type: 'keydown', key: 'F6', preventDefault() {}, stopPropagation() {} })
  onDataCb('\x1b[O') // focus-out report if moving focus out of terminal
  taListeners.get('focus')?.()
  taListeners.get('blur')?.()
  assert.equal(pinCalls, 0, 'modifiers, Meta shortcuts, keyup, F6 leave, and focus/blur must NOT pin')
  assert.equal(tabs[0].preview, true)

  // 6c. fix5 keys that xterm DOES turn into PTY input MUST pin:
  //     4) Mac Option+N under macOptionIsMeta (key === 'Dead', code === 'KeyN', keyCode === 229 -> ESC+n)
  resetPreview()
  await dispatchKey({ type: 'keydown', key: 'Dead', code: 'KeyN', keyCode: 229, altKey: true }, '\x1bn')
  assert.equal(pinCalls, 1, 'Mac Option+N (Dead under optionIsMeta -> ESC+n) must pin preview tab')
  assert.equal(tabs[0].preview, undefined)

  //     5) Mac Command+Backspace (metaKey === true, key === 'Backspace', keyCode === 8 -> DEL)
  resetPreview()
  await dispatchKey({ type: 'keydown', key: 'Backspace', keyCode: 8, metaKey: true }, '\x7f')
  assert.equal(pinCalls, 1, 'Mac Command+Backspace (metaKey -> DEL) must pin preview tab')
  assert.equal(tabs[0].preview, undefined)

  // 6d. Shift+F3 (which emits ESC[1;2R) and uppercase A (emitted in keypress) MUST pin:
  resetPreview()
  await dispatchKey({ type: 'keydown', key: 'F3', keyCode: 114, shiftKey: true }, '\x1b[1;2R')
  assert.equal(pinCalls, 1, 'Shift+F3 keypress must pin preview tab immediately')
  assert.equal(tabs[0].preview, undefined, 'preview tab A must be pinned after Shift+F3')

  resetPreview()
  await dispatchKey({ type: 'keydown', key: 'A', keyCode: 65, shiftKey: true }, null)
  assert.equal(pinCalls, 0, 'A-Z keydown without onData waits for keypress')
  await dispatchKey({ type: 'keypress', key: 'A', charCode: 65, shiftKey: true }, 'A')
  assert.equal(pinCalls, 1, 'uppercase A via keypress onData must pin preview tab')
  assert.equal(tabs[0].preview, undefined)

  // 6e. Typing / Option+N / Command+Backspace / Shift+Enter / Paste / IME commit / Touch key bar
  //     while WebSocket is CONNECTING or CLOSED (readyState !== OPEN) must STILL pin immediately!
  fakeWs.readyState = 0 // CONNECTING
  const beforeSent = sentFrames.length

  resetPreview()
  await dispatchKey({ type: 'keydown', key: 'y', keyCode: 89 }, 'y')
  assert.equal(pinCalls, 1, 'typing while WebSocket is connecting must still pin immediately')
  assert.equal(tabs[0].preview, undefined)

  fakeWs.readyState = 3 // CLOSED
  resetPreview()
  await dispatchKey({ type: 'keydown', key: 'Dead', code: 'KeyN', keyCode: 229, altKey: true }, '\x1bn')
  assert.equal(pinCalls, 1, 'Option+N while WebSocket is closed must still pin immediately')
  assert.equal(tabs[0].preview, undefined)

  resetPreview()
  customKeyHandler({ type: 'keydown', key: 'Enter', shiftKey: true, preventDefault() {}, stopPropagation() {} })
  assert.equal(pinCalls, 1, 'Shift+Enter while WebSocket is closed must still pin immediately')
  assert.equal(tabs[0].preview, undefined)

  resetPreview()
  const [enterBtn] = findAll(tree, (el) => el.type === 'button' && el.props.className === 'convo-panelwin-key')
  assert.ok(enterBtn, 'PaneTerminal should render touch key button when isCoarse()')
  enterBtn.props.onClick()
  assert.equal(pinCalls, 1, 'touch key bar click while WebSocket is closed must still pin immediately')
  assert.equal(tabs[0].preview, undefined)
  assert.equal(sentFrames.length, beforeSent, 'no WS frames sent while disconnected')

  // Paste and IME compositionend + mobile beforeinput must also pin immediately (even disconnected):
  resetPreview()
  taListeners.get('paste')?.()
  assert.equal(pinCalls, 1, 'paste in PTY terminal must pin immediately')
  assert.equal(tabs[0].preview, undefined)

  resetPreview()
  taListeners.get('beforeinput')?.({ inputType: 'insertCompositionText', data: 'ni' })
  assert.equal(pinCalls, 0, 'in-progress IME composition preview should wait for commit')
  taListeners.get('compositionend')?.({ data: '你' })
  assert.equal(pinCalls, 1, 'IME compositionend with committed text must pin immediately')
  assert.equal(tabs[0].preview, undefined)

  resetPreview()
  taListeners.get('beforeinput')?.({ inputType: 'insertText', data: 'a' })
  assert.equal(pinCalls, 1, 'mobile soft keyboard beforeinput insertText must pin immediately')
  assert.equal(tabs[0].preview, undefined)

  // Verify every PanelWindow in AgentsChat.tsx, Conversation.tsx, and dock.tsx passes onInput.
  for (const rel of ['../src/pages/AgentsChat.tsx', '../src/terminal/Conversation.tsx', '../src/terminal/dock.tsx']) {
    const sf = parseTsx(rel)
    let count = 0
    function checkPanel(node) {
      if (ts.isJsxOpeningElement(node) || ts.isJsxSelfClosingElement(node)) {
        if (node.tagName.getText(sf) === 'PanelWindow') {
          count++
          const hasOnInput = node.attributes.properties.some(
            (p) => ts.isJsxAttribute(p) && p.name.getText(sf) === 'onInput',
          )
          assert.equal(hasOnInput, true, `PanelWindow in ${rel} must pass onInput`)
        }
      }
      ts.forEachChild(node, checkPanel)
    }
    checkPanel(sf)
    assert.ok(count > 0, `expected PanelWindow in ${rel}`)
  }
}

console.log('agentTabPreviewUI: all tests passed')
